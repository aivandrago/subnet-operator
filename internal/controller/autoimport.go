/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"maps"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
	"hypersurgery.dev/subnet-operator/internal/audit"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/metrics"
	"hypersurgery.dev/subnet-operator/internal/policy"
	"hypersurgery.dev/subnet-operator/internal/tenancy"
)

// discoverUnmanaged says whether the scope wants to see past its own tag selector. It
// defaults to true: a resource nobody tagged is exactly the one worth knowing about.
func discoverUnmanaged(scope *awsv1alpha1.NetworkScope) bool {
	if scope.Spec.DiscoverUnmanaged == nil {
		return true
	}
	return *scope.Spec.DiscoverUnmanaged
}

// reportUnmanaged publishes the unmanaged counts as metrics and lets the auto-import policy
// act on them. Failures here never fail the sync: the inventory is the job, this is extra.
func (r *NetworkScopeReconciler) reportUnmanaged(ctx context.Context, scope *awsv1alpha1.NetworkScope,
	results []targetResult) {
	log := logf.FromContext(ctx)
	// Rebuild from this sync: an account whose last untagged subnet was imported should report
	// nothing rather than its old number. The counter beside the gauge keeps its memory, so it
	// still rises once per resource rather than once per resync.
	//
	// A target whose discovery failed is skipped below, so its series disappears for as long as
	// the outage lasts. That is deliberate: a gap says "we do not know right now", which is
	// true, while a held-over number would read as current. The Ready condition and
	// hs_aws_target_up say why the gap is there.
	metrics.ClearUnmanaged(scope.Name)

	// scope was read before this sync writes its status, so these are the unmanaged resources
	// the previous sync saw — possibly in another process, before a restart or a change of
	// leader. Starting from them means only what appeared since counts as new.
	var known []string
	for _, t := range scope.Status.Targets {
		known = append(known, t.UnmanagedIDs...)
	}
	metrics.SeedUnmanaged(scope.Name, known)

	policyAllowed := r.autoImportAllowed(ctx, scope)
	for i := range results {
		res := &results[i]
		if res.err != nil || res.snapshot == nil {
			continue
		}
		vpcIDs := make([]string, 0, len(res.snapshot.UnmanagedVPCs))
		for _, v := range res.snapshot.UnmanagedVPCs {
			vpcIDs = append(vpcIDs, v.ID)
		}
		subnetIDs := make([]string, 0, len(res.snapshot.UnmanagedSubnets))
		for _, s := range res.snapshot.UnmanagedSubnets {
			subnetIDs = append(subnetIDs, s.ID)
		}
		metrics.Unmanaged(scope.Name, res.target.Account, res.target.Region, "vpc", vpcIDs)
		metrics.Unmanaged(scope.Name, res.target.Account, res.target.Region, "subnet", subnetIDs)

		if !policyAllowed {
			continue
		}
		if err := r.runAutoImport(ctx, scope, res.snapshot); err != nil {
			log.Error(err, "auto-import failed", "target", res.target.Key())
		}
	}
}

// autoImportAllowed checks that the namespace the policy writes its imports to may use the
// scope. The webhook refuses a scope that gets this wrong; this is for a scope written while
// it was off, or a namespace relabelled since. A refused policy decides nothing: its imports
// would be refused by the import controller anyway, and the Event says why on the scope.
func (r *NetworkScopeReconciler) autoImportAllowed(ctx context.Context, scope *awsv1alpha1.NetworkScope) bool {
	p := scope.Spec.AutoImport
	if p == nil || p.Mode == "" || p.Mode == awsv1alpha1.AutoImportOff {
		return true
	}
	namespace := p.ImportNamespace()
	refusal, err := namespaceRefusal(ctx, r.Client, scope, namespace)
	if err != nil {
		// Unlike discovery, the policy is optional; skipping it for one sync is cheaper than
		// failing the sync over a namespace lookup.
		logf.FromContext(ctx).Error(err, "Could not check the auto-import namespace, skipping the policy",
			"namespace", namespace)
		return false
	}
	if refusal != "" {
		eventf(r.Recorder, scope, corev1.EventTypeWarning, tenancy.ReasonNamespaceNotAllowed, ActionAutoImport,
			"The auto-import policy is not run: %s", refusal)
		return false
	}
	return true
}

// runAutoImport asks the policy about every unmanaged resource and writes a ResourceImport
// for the ones it can attribute. It never applies tags itself: the import controller does
// that, so a policy decision and a hand-written import go through exactly the same path.
func (r *NetworkScopeReconciler) runAutoImport(ctx context.Context, scope *awsv1alpha1.NetworkScope,
	snap *inventory.Snapshot) error {
	p := scope.Spec.AutoImport
	if p == nil || p.Mode == "" || p.Mode == awsv1alpha1.AutoImportOff {
		return nil
	}
	log := logf.FromContext(ctx)

	// A subnet usually belongs to whoever owns its VPC, so the parent's tags are needed even
	// when the VPC itself is managed.
	vpcTags := map[string]map[string]string{}
	for _, v := range snap.VPCs {
		vpcTags[v.ID] = v.Tags
	}
	for _, v := range snap.UnmanagedVPCs {
		vpcTags[v.ID] = v.Tags
	}

	resources := make([]policy.Resource, 0, len(snap.UnmanagedVPCs)+len(snap.UnmanagedSubnets))
	for _, v := range snap.UnmanagedVPCs {
		resources = append(resources, policy.Resource{
			ID: v.ID, Account: v.Account, Region: v.Region, Tags: v.Tags,
		})
	}
	for _, s := range snap.UnmanagedSubnets {
		resources = append(resources, policy.Resource{
			ID: s.ID, Account: s.Account, Region: s.Region, Tags: s.Tags,
			IsSubnet: true, ParentVPCTags: vpcTags[s.VPCID],
		})
	}

	for _, res := range resources {
		creator := ""
		if r.Creators != nil {
			creator = r.Creators.Lookup(res.ID)
		}
		decision := policy.Decide(res, creator, p)
		switch decision.Verdict {
		case policy.VerdictImport:
			created, err := r.ensureImport(ctx, scope, res, decision, creator)
			if err != nil {
				return err
			}
			if created {
				result := audit.ResultApplied
				if p.Mode == awsv1alpha1.AutoImportDryRun {
					result = audit.ResultDryRun
				}
				metrics.AutoImport(scope.Name, res.Account, res.Region, result)
				eventf(r.Recorder, scope, corev1.EventTypeNormal, EventAutoImportRequested, ActionAutoImport,
					"Requested the import of %s in %s/%s: %s", res.ID, res.Account, res.Region, decision.Reason)
				r.recordDecision(ctx, scope, res, decision, creator, result)
				log.Info("auto-import created", "resource", res.ID, "mode", p.Mode,
					"reason", decision.Reason, "creator", creator)
			}
		case policy.VerdictSkip:
			metrics.AutoImport(scope.Name, res.Account, res.Region, audit.ResultSkipped)
			// No Event: a skip repeats on every sync and says nothing changed. The counter and
			// the audit line are the record.
			r.recordDecision(ctx, scope, res, decision, creator, audit.ResultSkipped)
		case policy.VerdictNoOwner:
			// Left unmanaged on purpose: the Event and the alert ask a human to pick an owner.
			metrics.AutoImport(scope.Name, res.Account, res.Region, audit.ResultNoOwner)
			eventf(r.Recorder, scope, corev1.EventTypeWarning, EventNoOwner, ActionAutoImport,
				"No rule could attribute %s in %s/%s: %s", res.ID, res.Account, res.Region, decision.Reason)
			r.recordDecision(ctx, scope, res, decision, creator, audit.ResultNoOwner)
		}
	}
	return nil
}

// recordDecision writes the audit line for one policy verdict. The tags are the ones the
// resource carries now and the ones it would carry, so a SIEM can see the change the policy
// asked for even when the import itself is applied minutes later, or not at all.
func (r *NetworkScopeReconciler) recordDecision(ctx context.Context, scope *awsv1alpha1.NetworkScope,
	res policy.Resource, decision policy.Decision, creator, result string) {
	if r.Audit == nil {
		return
	}
	var after map[string]string
	if len(decision.Tags) > 0 {
		after = maps.Clone(res.Tags)
		if after == nil {
			after = map[string]string{}
		}
		maps.Copy(after, decision.Tags)
	}
	audit.Emit(ctx, r.Audit, audit.Record{
		Action:     audit.ActionPolicyDecision,
		Result:     result,
		Scope:      scope.Name,
		Account:    res.Account,
		Region:     res.Region,
		ResourceID: res.ID,
		Object:     objectRef("NetworkScope", scope),
		Principal:  requestedBy(creator),
		CreatedBy:  r.createdBy(),
		Reason:     decision.Reason,
		TagsBefore: res.Tags,
		TagsAfter:  after,
	})
}

// ensureImport creates the ResourceImport for a resource unless one already exists. It
// reports whether it created anything, so the metrics count decisions and not resyncs.
func (r *NetworkScopeReconciler) ensureImport(ctx context.Context, scope *awsv1alpha1.NetworkScope,
	res policy.Resource, decision policy.Decision, creator string) (bool, error) {
	ns := scope.Spec.AutoImport.ImportNamespace()

	existing := &awsv1alpha1.ResourceImportList{}
	if err := r.reader().List(ctx, existing, client.InNamespace(ns),
		client.MatchingLabels{awsv1alpha1.LabelResource: res.ID}); err != nil {
		return false, err
	}
	if len(existing.Items) > 0 {
		return false, nil
	}

	imp := &awsv1alpha1.ResourceImport{
		ObjectMeta: metav1.ObjectMeta{
			Name:      importName(res.ID),
			Namespace: ns,
			Labels: map[string]string{
				awsv1alpha1.LabelResource: res.ID,
				awsv1alpha1.LabelScope:    scope.Name,
				awsv1alpha1.LabelAccount:  res.Account,
				awsv1alpha1.LabelRegion:   res.Region,
			},
			Annotations: r.importAnnotations(decision),
		},
		Spec: awsv1alpha1.ResourceImportSpec{
			ScopeRef:    scope.Name,
			Account:     res.Account,
			Region:      res.Region,
			ResourceID:  res.ID,
			Tags:        decision.Tags,
			RequestedBy: requestedBy(creator),
			DryRun:      scope.Spec.AutoImport.Mode == awsv1alpha1.AutoImportDryRun,
		},
	}
	if err := r.Create(ctx, imp); err != nil {
		// Another worker got there first; that is a success, not a conflict worth reporting.
		if apierrors.IsAlreadyExists(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// importAnnotations carries the policy's reason and, when the operator knows who it is, its
// own identity as the creator. The admission webhook overwrites created-by with the same user
// on the way in; setting it here as well means an import the policy writes while the webhooks
// are off still says where it came from.
func (r *NetworkScopeReconciler) importAnnotations(decision policy.Decision) map[string]string {
	annotations := map[string]string{annotationReason: decision.Reason}
	if r.Identity != "" {
		annotations[awsv1alpha1.AnnotationCreatedBy] = r.Identity
	}
	return annotations
}

// requestedBy says who a generated import is attributed to: the policy, and the principal
// that created the resource when CloudTrail told us one. It is the single source the import's
// status, its Event and its audit line all read, so they never disagree about the person.
func requestedBy(creator string) string {
	if creator == "" {
		return awsv1alpha1.RequestedByPolicy
	}
	return fmt.Sprintf("%s (created by %s)", awsv1alpha1.RequestedByPolicy, creator)
}

// annotationReason carries the policy's sentence to the ResourceImport it writes, so the
// import can say why the resource was taken over without deciding that again.
const annotationReason = "aws.hypersurgery/reason"

// importName keeps the object name predictable and inside the 253-character limit.
func importName(resourceID string) string {
	name := resourceID + "-import"
	if len(name) > 253 {
		name = name[:253]
	}
	return name
}

// unmanagedIDs lists the VPCs and subnets of a snapshot that are outside the selector, sorted so
// the status does not change when nothing else did.
func unmanagedIDs(s *inventory.Snapshot) []string {
	ids := make([]string, 0, len(s.UnmanagedVPCs)+len(s.UnmanagedSubnets))
	for _, v := range s.UnmanagedVPCs {
		ids = append(ids, v.ID)
	}
	for _, sn := range s.UnmanagedSubnets {
		ids = append(ids, sn.ID)
	}
	slices.Sort(ids)
	return ids
}

// createdBy is the created_by of a policy decision: the policy runs as the operator, so the
// decision is taken by the operator's own identity.
func (r *NetworkScopeReconciler) createdBy() string {
	if r.Identity == "" {
		return audit.CreatedByUnknown
	}
	return r.Identity
}
