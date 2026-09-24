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
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kevents "k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
	"hypersurgery.dev/subnet-operator/internal/allocator"
	"hypersurgery.dev/subnet-operator/internal/audit"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/metrics"
)

const (
	// ConditionAllocated is True when every requested AZ has a reserved CIDR.
	ConditionAllocated = "Allocated"

	claimRetryInterval = time.Minute
)

// SubnetClaimReconciler reserves CIDRs for SubnetClaims and, when writes are enabled and the
// claim asks for it, creates the subnets. It never deletes a subnet.
type SubnetClaimReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader reads inventory lists straight from the API server, so a subnet created a
	// moment ago by another claim is not missed because the cache is behind.
	APIReader client.Reader
	// Writer creates subnets. Nil means writes are impossible.
	Writer inventory.SubnetWriter
	// WritesEnabled gates every call to Writer. Off by default: an operator installed for
	// inventory must not start creating things because someone applied a claim.
	WritesEnabled bool
	// Notify asks the scope controller to resync a target after a subnet was created.
	Notify func(ctx context.Context, changed []inventory.TargetKey) error
	// Recorder puts allocations and refusals on the claim, where `kubectl describe` shows them.
	Recorder kevents.EventRecorder
	// Audit receives one line per reserved CIDR and per created subnet. Nil keeps the audit
	// trail off.
	Audit audit.Sink
}

// +kubebuilder:rbac:groups=aws.hypersurgery,resources=subnetclaims,verbs=get;list;watch
// +kubebuilder:rbac:groups=aws.hypersurgery,resources=subnetclaims/status,verbs=get;update;patch

// Reconcile brings one claim as far as its mode and the write switch allow.
func (r *SubnetClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	claim := &awsv1alpha1.SubnetClaim{}
	if err := r.Get(ctx, req.NamespacedName, claim); err != nil {
		if apierrors.IsNotFound(err) {
			metrics.ForgetClaim(req.Namespace, req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !claim.DeletionTimestamp.IsZero() {
		metrics.ForgetClaim(req.Namespace, req.Name)
		// Subnets stay: deleting them is a human decision, made in AWS.
		return ctrl.Result{}, nil
	}

	status, requeue, err := r.reconcile(ctx, claim)
	if err != nil {
		log.Error(err, "claim failed")
	}
	if statusErr := r.writeStatus(ctx, claim, status); statusErr != nil {
		return ctrl.Result{}, statusErr
	}
	// From the status just written, so the metric never says something the object does not.
	metrics.ClaimReady(req.Namespace, req.Name, status.Conditions)
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// reconcile computes the new status. The returned error is for Kubernetes API problems only;
// everything about AWS and the claim itself lands in the status conditions.
func (r *SubnetClaimReconciler) reconcile(ctx context.Context, claim *awsv1alpha1.SubnetClaim) (awsv1alpha1.SubnetClaimStatus, time.Duration, error) {
	status := *claim.Status.DeepCopy()
	status.ObservedGeneration = claim.Generation
	fail := func(reason, msg string) (awsv1alpha1.SubnetClaimStatus, time.Duration, error) {
		setCondition(&status, ConditionReady, metav1.ConditionFalse, reason, msg, claim.Generation)
		eventf(r.Recorder, claim, corev1.EventTypeWarning, reason, ActionReserveCIDR, "%s", msg)
		return status, claimRetryInterval, nil
	}

	// The scope supplies the credentials and the inventory the claim is checked against.
	scope := &awsv1alpha1.NetworkScope{}
	if err := r.Get(ctx, types.NamespacedName{Name: claim.Spec.ScopeRef}, scope); err != nil {
		if apierrors.IsNotFound(err) {
			return fail("ScopeNotFound", fmt.Sprintf("NetworkScope %q not found", claim.Spec.ScopeRef))
		}
		return status, 0, err
	}
	target, ok := targetFor(scope, claim.Spec.Account, claim.Spec.Region)
	if !ok {
		return fail("AccountNotInScope", fmt.Sprintf("account %s in %s is not covered by NetworkScope %q",
			claim.Spec.Account, claim.Spec.Region, scope.Name))
	}

	vpc := &awsv1alpha1.VPC{}
	if err := r.reader().Get(ctx, types.NamespacedName{Name: claim.Spec.VPCID}, vpc); err != nil {
		if apierrors.IsNotFound(err) {
			return fail("VPCNotFound", fmt.Sprintf("VPC %s has not been discovered by NetworkScope %q; check the account, region and tag selector",
				claim.Spec.VPCID, scope.Name))
		}
		return status, 0, err
	}
	if vpc.Spec.Account != claim.Spec.Account || vpc.Spec.Region != claim.Spec.Region {
		return fail("VPCNotFound", fmt.Sprintf("VPC %s belongs to %s/%s, not %s/%s",
			claim.Spec.VPCID, vpc.Spec.Account, vpc.Spec.Region, claim.Spec.Account, claim.Spec.Region))
	}

	subnets := &awsv1alpha1.SubnetList{}
	if err := r.reader().List(ctx, subnets, client.MatchingLabels{awsv1alpha1.LabelVPC: claim.Spec.VPCID}); err != nil {
		return status, 0, err
	}
	claims := &awsv1alpha1.SubnetClaimList{}
	if err := r.reader().List(ctx, claims); err != nil {
		return status, 0, err
	}

	// Keep what is already reserved, adopt subnets created earlier for this claim, and drop
	// AZs that left the spec.
	status.Allocations = reconcileAllocations(claim, status.Allocations, subnets.Items)

	// Everything in use in this VPC: real subnets plus other claims' reservations.
	pool := allocator.Pool{CIDRs: vpc.Status.CIDRBlocks}
	for _, s := range subnets.Items {
		pool.Used = append(pool.Used, s.Status.CIDRBlock)
	}
	for _, other := range claims.Items {
		if other.UID == claim.UID || other.Spec.VPCID != claim.Spec.VPCID {
			continue
		}
		for _, a := range other.Status.Allocations {
			pool.Used = append(pool.Used, a.CIDRBlock)
		}
	}
	for _, a := range status.Allocations {
		pool.Used = append(pool.Used, a.CIDRBlock)
	}

	var missing []string
	for _, az := range claim.Spec.AvailabilityZones {
		if findAllocation(status.Allocations, az) == nil {
			missing = append(missing, az)
		}
	}
	if len(missing) > 0 {
		cidrs, err := allocator.Allocate(pool, int(claim.Spec.PrefixLength), len(missing))
		if err != nil {
			setCondition(&status, ConditionAllocated, metav1.ConditionFalse, "NoSpace", err.Error(), claim.Generation)
			return fail("NoSpace", err.Error())
		}
		for i, az := range missing {
			status.Allocations = append(status.Allocations, awsv1alpha1.SubnetAllocation{
				AvailabilityZone: az, CIDRBlock: cidrs[i], State: awsv1alpha1.AllocationPending,
			})
			eventf(r.Recorder, claim, corev1.EventTypeNormal, EventAllocated, ActionReserveCIDR,
				"Reserved %s in %s for %s", cidrs[i], claim.Spec.VPCID, az)
			r.record(ctx, claim, audit.Record{
				Result: audit.ResultReserved, CIDR: cidrs[i],
				Reason: fmt.Sprintf("reserved in %s for %s", claim.Spec.VPCID, az),
			})
		}
	}
	sortAllocations(status.Allocations)
	setCondition(&status, ConditionAllocated, metav1.ConditionTrue, "Allocated",
		fmt.Sprintf("%d CIDRs reserved in %s", len(status.Allocations), claim.Spec.VPCID), claim.Generation)

	if claim.Spec.Mode == awsv1alpha1.ClaimModeAllocate {
		setCondition(&status, ConditionReady, metav1.ConditionTrue, "Allocated",
			"CIDRs are reserved; create the subnets from status.allocations", claim.Generation)
		return status, 0, nil
	}

	if !r.WritesEnabled || r.Writer == nil {
		return fail("WritesDisabled", "the operator runs read-only; start it with --enable-writes to create subnets")
	}
	if target.RoleARN == "" && scope.AccountHasReadRole(claim.Spec.Account) {
		return fail("NoWriteRole", fmt.Sprintf("account %s has no writeRoleARN in NetworkScope %q", claim.Spec.Account, scope.Name))
	}

	created, needsRetry := r.createPending(ctx, claim, target, &status)
	if created {
		if r.Notify != nil {
			if err := r.Notify(ctx, []inventory.TargetKey{target.Key()}); err != nil {
				logf.FromContext(ctx).Error(err, "failed to request a resync after creating subnets")
			}
		}
	}
	if needsRetry {
		return fail("CreateFailed", "one or more subnets could not be created; see status.allocations")
	}
	setCondition(&status, ConditionReady, metav1.ConditionTrue, "Created",
		fmt.Sprintf("%d subnets exist in %s", len(status.Allocations), claim.Spec.VPCID), claim.Generation)
	return status, 0, nil
}

// createPending creates every allocation that has no subnet yet. It reports whether anything
// was created and whether anything still needs a retry.
func (r *SubnetClaimReconciler) createPending(ctx context.Context, claim *awsv1alpha1.SubnetClaim,
	target inventory.Target, status *awsv1alpha1.SubnetClaimStatus) (created, needsRetry bool) {
	log := logf.FromContext(ctx)
	for i := range status.Allocations {
		a := &status.Allocations[i]
		if a.SubnetID != "" {
			a.State, a.Error = awsv1alpha1.AllocationCreated, ""
			continue
		}
		tags := subnetTags(claim, a.AvailabilityZone)
		id, err := r.Writer.CreateSubnet(ctx, target, inventory.CreateSubnetRequest{
			VPCID:               claim.Spec.VPCID,
			CIDRBlock:           a.CIDRBlock,
			AvailabilityZone:    a.AvailabilityZone,
			RouteTableID:        claim.Spec.RouteTableID,
			MapPublicIPOnLaunch: claim.Spec.MapPublicIPOnLaunch,
			Tags:                tags,
		})
		switch {
		case errors.Is(err, inventory.ErrCIDRConflict):
			// Someone took the block between our inventory and the call: forget it, the next
			// pass allocates a fresh one from an inventory that now includes the winner.
			log.Info("cidr taken, reallocating", "cidr", a.CIDRBlock, "az", a.AvailabilityZone)
			r.record(ctx, claim, audit.Record{Result: audit.ResultFailed, CIDR: a.CIDRBlock,
				Reason: "the CIDR was taken between discovery and the call", Error: err.Error()})
			a.CIDRBlock, a.State, a.Error = "", awsv1alpha1.AllocationFailed, err.Error()
			needsRetry = true
		case err != nil && id != "":
			// The subnet exists but a follow-up step failed; keep the ID so we never create twice.
			// It exists in AWS, so the audit trail says so even though the claim is not Ready.
			r.record(ctx, claim, audit.Record{Result: audit.ResultFailed, CIDR: a.CIDRBlock,
				ResourceID: id, TagsAfter: tags, Reason: "the subnet exists but a follow-up step failed",
				Error: err.Error()})
			a.SubnetID, a.State, a.Error = id, awsv1alpha1.AllocationFailed, err.Error()
			created, needsRetry = true, true
		case err != nil:
			// The claim's own CreateFailed Event comes from fail() once, with the rest in
			// status.allocations; one Event per failed AZ would say the same thing louder.
			r.record(ctx, claim, audit.Record{Result: audit.ResultFailed, CIDR: a.CIDRBlock, Error: err.Error()})
			a.State, a.Error = awsv1alpha1.AllocationFailed, err.Error()
			needsRetry = true
		default:
			eventf(r.Recorder, claim, corev1.EventTypeNormal, EventSubnetCreated, ActionCreateSubnet,
				"Created %s (%s) in %s for %s", id, a.CIDRBlock, a.AvailabilityZone, claim.Spec.Owner)
			r.record(ctx, claim, audit.Record{Result: audit.ResultApplied, CIDR: a.CIDRBlock,
				ResourceID: id, TagsAfter: tags,
				Reason: fmt.Sprintf("created in %s for %s", claim.Spec.VPCID, a.AvailabilityZone)})
			a.SubnetID, a.State, a.Error = id, awsv1alpha1.AllocationCreated, ""
			created = true
		}
	}
	// Allocations that lost their CIDR are re-done on the next pass.
	status.Allocations = slices.DeleteFunc(status.Allocations, func(a awsv1alpha1.SubnetAllocation) bool {
		return a.CIDRBlock == ""
	})
	return created, needsRetry
}

// record writes the audit line for one allocation. What the caller supplies is what differs
// between a reservation and a created subnet; everything that locates the claim is filled in
// here, so no call site can name the account or the owner differently.
func (r *SubnetClaimReconciler) record(ctx context.Context, claim *awsv1alpha1.SubnetClaim, rec audit.Record) {
	if r.Audit == nil {
		return
	}
	rec.Action = audit.ActionAllocate
	rec.Scope = claim.Spec.ScopeRef
	rec.Account = claim.Spec.Account
	rec.Region = claim.Spec.Region
	rec.Object = objectRef("SubnetClaim", claim)
	// The owner tag is who the subnet is attributed to, and it is the same value the subnet
	// itself carries in AWS.
	rec.Principal = claim.Spec.Owner
	audit.Emit(ctx, r.Audit, rec)
}

// reconcileAllocations keeps allocations for AZs still in the spec, and adopts subnets that
// carry this claim's tag (created before a status update was lost, or by a previous claim
// with the same name).
func reconcileAllocations(claim *awsv1alpha1.SubnetClaim, current []awsv1alpha1.SubnetAllocation,
	subnets []awsv1alpha1.Subnet) []awsv1alpha1.SubnetAllocation {
	var out []awsv1alpha1.SubnetAllocation
	for _, a := range current {
		if slices.Contains(claim.Spec.AvailabilityZones, a.AvailabilityZone) && a.CIDRBlock != "" {
			out = append(out, a)
		}
	}
	tag := claimTag(claim)
	for _, s := range subnets {
		if s.Status.Tags[awsv1alpha1.TagClaim] != tag {
			continue
		}
		if !slices.Contains(claim.Spec.AvailabilityZones, s.Status.AvailabilityZone) {
			continue
		}
		if existing := findAllocation(out, s.Status.AvailabilityZone); existing != nil {
			if existing.SubnetID == "" {
				existing.SubnetID, existing.CIDRBlock, existing.State, existing.Error = s.Spec.SubnetID, s.Status.CIDRBlock, awsv1alpha1.AllocationCreated, ""
			}
			continue
		}
		out = append(out, awsv1alpha1.SubnetAllocation{
			AvailabilityZone: s.Status.AvailabilityZone, CIDRBlock: s.Status.CIDRBlock,
			SubnetID: s.Spec.SubnetID, State: awsv1alpha1.AllocationCreated,
		})
	}
	return out
}

func findAllocation(list []awsv1alpha1.SubnetAllocation, az string) *awsv1alpha1.SubnetAllocation {
	for i := range list {
		if list[i].AvailabilityZone == az {
			return &list[i]
		}
	}
	return nil
}

func sortAllocations(list []awsv1alpha1.SubnetAllocation) {
	slices.SortFunc(list, func(a, b awsv1alpha1.SubnetAllocation) int {
		return strings.Compare(a.AvailabilityZone, b.AvailabilityZone)
	})
}

func claimTag(claim *awsv1alpha1.SubnetClaim) string {
	return claim.Namespace + "/" + claim.Name
}

// subnetTags builds the tags of a subnet: the claim's own tags first, the organization's
// hs/* tags on top so they cannot be overridden.
func subnetTags(claim *awsv1alpha1.SubnetClaim, az string) map[string]string {
	tags := map[string]string{}
	maps.Copy(tags, claim.Spec.Tags)
	prefix := claim.Spec.NamePrefix
	if prefix == "" {
		prefix = claim.Name
	}
	tags["Name"] = prefix + "-" + strings.TrimPrefix(az, claim.Spec.Region)
	tags[awsv1alpha1.DefaultOwnerTagKey] = claim.Spec.Owner
	if claim.Spec.Env != "" {
		tags[awsv1alpha1.DefaultEnvTagKey] = claim.Spec.Env
	}
	if claim.Spec.Tier != "" {
		tags[awsv1alpha1.DefaultTierTagKey] = claim.Spec.Tier
	}
	tags[awsv1alpha1.TagManagedBy] = awsv1alpha1.TagManagedByValue
	tags[awsv1alpha1.TagClaim] = claimTag(claim)
	return tags
}

// targetFor returns the write target for the account/region, if the scope covers it.
// RoleARN is the account's write role, which may be empty for the operator's own account.
func targetFor(scope *awsv1alpha1.NetworkScope, account, region string) (inventory.Target, bool) {
	a, ok := scope.Account(account)
	if !ok || !scope.Covers(account, region) {
		return inventory.Target{}, false
	}
	return inventory.Target{Account: account, Region: region, RoleARN: a.WriteRoleARN, ExternalID: a.ExternalID}, true
}

func setCondition(status *awsv1alpha1.SubnetClaimStatus, typ string, st metav1.ConditionStatus, reason, msg string, gen int64) {
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: typ, Status: st, Reason: reason, Message: msg, ObservedGeneration: gen,
	})
}

func (r *SubnetClaimReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *SubnetClaimReconciler) writeStatus(ctx context.Context, claim *awsv1alpha1.SubnetClaim, status awsv1alpha1.SubnetClaimStatus) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &awsv1alpha1.SubnetClaim{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(claim), latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		latest.Status = status
		return r.Status().Update(ctx, latest)
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *SubnetClaimReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&awsv1alpha1.SubnetClaim{}).
		Named("subnetclaim").
		Complete(r)
}
