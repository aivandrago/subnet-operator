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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/metrics"
	"hypersurgery.dev/subnet-operator/internal/policy"
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

		if err := r.runAutoImport(ctx, scope, res.snapshot); err != nil {
			log.Error(err, "auto-import failed", "target", res.target.Key())
		}
	}
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
				result := "applied"
				if p.Mode == awsv1alpha1.AutoImportDryRun {
					result = "dryrun"
				}
				metrics.AutoImport(scope.Name, res.Account, res.Region, result)
				log.Info("auto-import created", "resource", res.ID, "mode", p.Mode,
					"reason", decision.Reason, "creator", creator)
			}
		case policy.VerdictSkip:
			metrics.AutoImport(scope.Name, res.Account, res.Region, "skipped")
		case policy.VerdictNoOwner:
			// Left unmanaged on purpose: the alert asks a human to pick an owner.
			metrics.AutoImport(scope.Name, res.Account, res.Region, "no_owner")
		}
	}
	return nil
}

// ensureImport creates the ResourceImport for a resource unless one already exists. It
// reports whether it created anything, so the metrics count decisions and not resyncs.
func (r *NetworkScopeReconciler) ensureImport(ctx context.Context, scope *awsv1alpha1.NetworkScope,
	res policy.Resource, decision policy.Decision, creator string) (bool, error) {
	ns := scope.Spec.AutoImport.Namespace
	if ns == "" {
		ns = "default"
	}

	existing := &awsv1alpha1.ResourceImportList{}
	if err := r.reader().List(ctx, existing, client.InNamespace(ns),
		client.MatchingLabels{awsv1alpha1.LabelResource: res.ID}); err != nil {
		return false, err
	}
	if len(existing.Items) > 0 {
		return false, nil
	}

	requestedBy := awsv1alpha1.RequestedByPolicy
	if creator != "" {
		requestedBy = fmt.Sprintf("%s (created by %s)", awsv1alpha1.RequestedByPolicy, creator)
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
			Annotations: map[string]string{
				"aws.hypersurgery/reason": decision.Reason,
			},
		},
		Spec: awsv1alpha1.ResourceImportSpec{
			ScopeRef:    scope.Name,
			Account:     res.Account,
			Region:      res.Region,
			ResourceID:  res.ID,
			Tags:        decision.Tags,
			RequestedBy: requestedBy,
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

// importName keeps the object name predictable and inside the 253-character limit.
func importName(resourceID string) string {
	name := resourceID + "-import"
	if len(name) > 253 {
		name = name[:253]
	}
	return name
}
