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

	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
	"hypersurgery.dev/subnet-operator/internal/allocator"
	"hypersurgery.dev/subnet-operator/internal/audit"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/metrics"
	"hypersurgery.dev/subnet-operator/internal/provider"
	"hypersurgery.dev/subnet-operator/internal/tenancy"
)

const (
	// ConditionAllocated is True when every requested zone has a reserved CIDR.
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
	// Providers are the clouds the operator runs with; the claim's scope picks one, which
	// checks the claim and creates its subnets.
	Providers *provider.Registry
	// WritesEnabled gates every subnet creation. Off by default: an operator installed for
	// inventory must not start creating things because someone applied a claim.
	WritesEnabled bool
	// Notify asks the scope controller to resync a target after a subnet was created.
	Notify func(ctx context.Context, changed []inventory.TargetKey) error
	// Recorder puts allocations and refusals on the claim, where `kubectl describe` shows them.
	Recorder kevents.EventRecorder
	// Audit receives one line per reserved CIDR and per created subnet. Nil keeps the audit
	// trail off.
	Audit audit.Sink
	// WebhooksEnabled says the manager serves the admission webhooks, which write the
	// created-by annotation and refuse changes to it. Only then does the audit trail repeat
	// the annotation; otherwise anybody could have written it, and the line says "unknown".
	WebhooksEnabled bool
}

// +kubebuilder:rbac:groups=network.hypersurgery.dev,resources=subnetclaims,verbs=get;list;watch
// Namespaces are read for their labels, which a scope's namespaceSelector matches against.
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=network.hypersurgery.dev,resources=subnetclaims/status,verbs=get;update;patch

// Reconcile brings one claim as far as its mode and the write switch allow.
func (r *SubnetClaimReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	claim := &networkv1beta1.SubnetClaim{}
	if err := r.Get(ctx, req.NamespacedName, claim); err != nil {
		if apierrors.IsNotFound(err) {
			metrics.ForgetClaim(req.Namespace, req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !claim.DeletionTimestamp.IsZero() {
		metrics.ForgetClaim(req.Namespace, req.Name)
		// Subnets stay: deleting them is a human decision, made in the cloud.
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
	metrics.ClaimReady(req.Namespace, req.Name, scopeProvider(ctx, r, claim.Spec.ScopeRef), status.Conditions)
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

// reconcile computes the new status. The returned error is for Kubernetes API problems only;
// everything about AWS and the claim itself lands in the status conditions.
func (r *SubnetClaimReconciler) reconcile(ctx context.Context, claim *networkv1beta1.SubnetClaim) (networkv1beta1.SubnetClaimStatus, time.Duration, error) {
	status := *claim.Status.DeepCopy()
	status.ObservedGeneration = claim.Generation
	fail := func(reason, msg string) (networkv1beta1.SubnetClaimStatus, time.Duration, error) {
		setCondition(&status, ConditionReady, metav1.ConditionFalse, reason, msg, claim.Generation)
		eventf(r.Recorder, claim, corev1.EventTypeWarning, reason, ActionReserveCIDR, "%s", msg)
		return status, claimRetryInterval, nil
	}

	// The scope supplies the credentials and the inventory the claim is checked against.
	scope, p, target, reason, msg, err := r.scopeFor(ctx, claim)
	if err != nil {
		return status, 0, err
	}
	if reason != "" {
		return fail(reason, msg)
	}

	// The webhook refuses the same claims at apply time; this is for the ones created while
	// it was not running.
	if reason, msg := p.ClaimRefusal(claim); reason != "" {
		return fail(reason, msg)
	}

	network := &networkv1beta1.Network{}
	if err := r.reader().Get(ctx, types.NamespacedName{Name: claim.Spec.NetworkID}, network); err != nil {
		if apierrors.IsNotFound(err) {
			return fail("NetworkNotFound", fmt.Sprintf("network %s has not been discovered by NetworkScope %q; check the account, region and network selector",
				claim.Spec.NetworkID, scope.Name))
		}
		return status, 0, err
	}
	if network.Spec.Account != claim.Spec.Account || network.Spec.Region != claim.Spec.Region {
		return fail("NetworkNotFound", fmt.Sprintf("network %s belongs to %s/%s, not %s/%s",
			claim.Spec.NetworkID, network.Spec.Account, network.Spec.Region, claim.Spec.Account, claim.Spec.Region))
	}

	subnets := &networkv1beta1.SubnetList{}
	if err := r.reader().List(ctx, subnets, client.MatchingLabels{networkv1beta1.LabelNetwork: claim.Spec.NetworkID}); err != nil {
		return status, 0, err
	}
	claims := &networkv1beta1.SubnetClaimList{}
	if err := r.reader().List(ctx, claims); err != nil {
		return status, 0, err
	}

	// Keep what is already reserved, adopt subnets created earlier for this claim, and drop
	// zones that left the spec.
	status.Allocations = reconcileAllocations(claim, status.Allocations, subnets.Items)

	noSpace := r.allocateMissing(ctx, claim, network, subnets.Items, claims.Items, &status)
	if noSpace != nil {
		setCondition(&status, ConditionAllocated, metav1.ConditionFalse, "NoSpace", noSpace.Error(), claim.Generation)
		return fail("NoSpace", noSpace.Error())
	}
	sortAllocations(status.Allocations)
	setCondition(&status, ConditionAllocated, metav1.ConditionTrue, "Allocated",
		fmt.Sprintf("%d CIDRs reserved in %s", len(status.Allocations), claim.Spec.NetworkID), claim.Generation)

	if claim.Spec.Mode == networkv1beta1.ClaimModeAllocate {
		setCondition(&status, ConditionReady, metav1.ConditionTrue, "Allocated",
			"CIDRs are reserved; create the subnets from status.allocations", claim.Generation)
		return status, 0, nil
	}

	if !provider.HasCapability(p, networkv1beta1.CapabilityCreateSubnet) {
		return fail("CreateNotSupported", fmt.Sprintf("provider %s cannot create subnets in this release; "+
			"use mode Allocate to only reserve CIDRs", p.Name()))
	}
	if !r.WritesEnabled {
		return fail("WritesDisabled", "the operator runs read-only; start it with --enable-writes to create subnets")
	}
	if provider.MissingWriteIdentity(p, scope, claim.Spec.Account) {
		return fail("NoWriteRole", fmt.Sprintf("account %s has no %s in NetworkScope %q",
			claim.Spec.Account, p.WriteIdentityField(), scope.Name))
	}

	created, needsRetry := r.createPending(ctx, claim, p, target, &status)
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
		fmt.Sprintf("%d subnets exist in %s", len(status.Allocations), claim.Spec.NetworkID), claim.Generation)
	return status, 0, nil
}

// allocateMissing reserves a CIDR for every zone of the claim that has none yet, from what is
// free in the network: its blocks minus the discovered subnets and every other claim's
// reservations. noSpace says the network is full.
func (r *SubnetClaimReconciler) allocateMissing(ctx context.Context, claim *networkv1beta1.SubnetClaim,
	network *networkv1beta1.Network, subnets []networkv1beta1.Subnet, claims []networkv1beta1.SubnetClaim,
	status *networkv1beta1.SubnetClaimStatus) (noSpace error) {
	var missing []string
	for _, zone := range claim.Spec.Zones {
		if findAllocation(status.Allocations, zone) == nil {
			missing = append(missing, zone)
		}
	}
	if len(missing) == 0 {
		return nil
	}

	pool := allocator.Pool{CIDRs: network.Status.CIDRBlocks}
	for _, s := range subnets {
		pool.Used = append(pool.Used, s.Status.CIDRBlock)
	}
	for _, other := range claims {
		if other.UID == claim.UID || other.Spec.NetworkID != claim.Spec.NetworkID {
			continue
		}
		for _, a := range other.Status.Allocations {
			pool.Used = append(pool.Used, a.CIDRBlock)
		}
	}
	for _, a := range status.Allocations {
		pool.Used = append(pool.Used, a.CIDRBlock)
	}

	cidrs, err := allocator.Allocate(pool, int(claim.Spec.PrefixLength), len(missing))
	if err != nil {
		return err // a full network is an answer for the status, not a failure to ask
	}
	for i, zone := range missing {
		status.Allocations = append(status.Allocations, networkv1beta1.SubnetAllocation{
			Name: subnetName(claim, zone), Zone: zone, CIDRBlock: cidrs[i], State: networkv1beta1.AllocationPending,
		})
		eventf(r.Recorder, claim, corev1.EventTypeNormal, EventAllocated, ActionReserveCIDR,
			"Reserved %s in %s for %s", cidrs[i], claim.Spec.NetworkID, zone)
		r.record(ctx, claim, audit.Record{
			Result: audit.ResultReserved, CIDR: cidrs[i],
			Reason: fmt.Sprintf("reserved in %s for %s", claim.Spec.NetworkID, zone),
		})
	}
	return nil
}

// scopeFor reads the claim's scope and checks that the claim may use it at all: the scope
// exists, allows the claim's namespace and covers its account and region. A reason is a
// refusal for the status; the error is for Kubernetes API problems only.
//
// The namespace is checked before anything that uses the scope: a namespace the scope does not
// allow gets no reservation and no subnet. What the claim already has stays — the operator
// never deletes a subnet, and dropping the reservations would hand their CIDRs to the next
// claim while the subnets may still exist.
func (r *SubnetClaimReconciler) scopeFor(ctx context.Context, claim *networkv1beta1.SubnetClaim) (
	scope *networkv1beta1.NetworkScope, p provider.Provider, target inventory.Target, reason, msg string, err error) {
	scope = &networkv1beta1.NetworkScope{}
	if err := r.Get(ctx, types.NamespacedName{Name: claim.Spec.ScopeRef}, scope); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil, target, "ScopeNotFound", fmt.Sprintf("NetworkScope %q not found", claim.Spec.ScopeRef), nil
		}
		return nil, nil, target, "", "", err
	}
	refusal, err := namespaceRefusal(ctx, r.Client, scope, claim.Namespace)
	if err != nil {
		return nil, nil, target, "", "", err
	}
	if refusal != "" {
		return nil, nil, target, tenancy.ReasonNamespaceNotAllowed, refusal, nil
	}
	p, ok := r.Providers.Get(scope.Spec.Provider)
	if !ok {
		return nil, nil, target, ReasonProviderNotEnabled, r.Providers.NotEnabled(scope.Spec.Provider), nil
	}
	target, ok = writeTarget(scope, p, claim.Spec.Account, claim.Spec.Region)
	if !ok {
		return nil, nil, target, "AccountNotInScope", fmt.Sprintf("account %s in %s is not covered by NetworkScope %q",
			claim.Spec.Account, claim.Spec.Region, scope.Name), nil
	}
	return scope, p, target, "", "", nil
}

// createPending creates every allocation that has no subnet yet. It reports whether anything
// was created and whether anything still needs a retry.
func (r *SubnetClaimReconciler) createPending(ctx context.Context, claim *networkv1beta1.SubnetClaim,
	w inventory.SubnetWriter, target inventory.Target, status *networkv1beta1.SubnetClaimStatus) (created, needsRetry bool) {
	log := logf.FromContext(ctx)
	for i := range status.Allocations {
		a := &status.Allocations[i]
		if a.SubnetID != "" {
			a.State, a.Error = networkv1beta1.AllocationCreated, ""
			continue
		}
		tags := subnetTags(claim, a.Zone)
		id, err := w.CreateSubnet(ctx, target, inventory.CreateSubnetRequest{
			NetworkID: claim.Spec.NetworkID,
			CIDRBlock: a.CIDRBlock,
			Zone:      a.Zone,
			Tags:      tags,
			// The provider's own options travel as the claim states them.
			AWS: claim.Spec.AWS,
		})
		switch {
		case errors.Is(err, inventory.ErrCIDRConflict):
			// Someone took the block between our inventory and the call: forget it, the next
			// pass allocates a fresh one from an inventory that now includes the winner.
			log.Info("cidr taken, reallocating", "cidr", a.CIDRBlock, "zone", a.Zone)
			r.record(ctx, claim, audit.Record{Result: audit.ResultFailed, CIDR: a.CIDRBlock,
				Reason: "the CIDR was taken between discovery and the call", Error: err.Error()})
			a.CIDRBlock, a.State, a.Error = "", networkv1beta1.AllocationFailed, err.Error()
			needsRetry = true
		case err != nil && id != "":
			// The subnet exists but a follow-up step failed; keep the ID so we never create twice.
			// It exists in AWS, so the audit trail says so even though the claim is not Ready.
			r.record(ctx, claim, audit.Record{Result: audit.ResultFailed, CIDR: a.CIDRBlock,
				ResourceID: id, TagsAfter: tags, Reason: "the subnet exists but a follow-up step failed",
				Error: err.Error()})
			a.SubnetID, a.State, a.Error = id, networkv1beta1.AllocationFailed, err.Error()
			created, needsRetry = true, true
		case err != nil:
			// The claim's own CreateFailed Event comes from fail() once, with the rest in
			// status.allocations; one Event per failed AZ would say the same thing louder.
			r.record(ctx, claim, audit.Record{Result: audit.ResultFailed, CIDR: a.CIDRBlock, Error: err.Error()})
			a.State, a.Error = networkv1beta1.AllocationFailed, err.Error()
			needsRetry = true
		default:
			eventf(r.Recorder, claim, corev1.EventTypeNormal, EventSubnetCreated, ActionCreateSubnet,
				"Created %s (%s) in %s for %s", id, a.CIDRBlock, a.Zone, claim.Spec.Owner)
			r.record(ctx, claim, audit.Record{Result: audit.ResultApplied, CIDR: a.CIDRBlock,
				ResourceID: id, TagsAfter: tags,
				Reason: fmt.Sprintf("created in %s for %s", claim.Spec.NetworkID, a.Zone)})
			a.SubnetID, a.State, a.Error = id, networkv1beta1.AllocationCreated, ""
			created = true
		}
	}
	// Allocations that lost their CIDR are re-done on the next pass.
	status.Allocations = slices.DeleteFunc(status.Allocations, func(a networkv1beta1.SubnetAllocation) bool {
		return a.CIDRBlock == ""
	})
	return created, needsRetry
}

// record writes the audit line for one allocation. What the caller supplies is what differs
// between a reservation and a created subnet; everything that locates the claim is filled in
// here, so no call site can name the account or the owner differently.
func (r *SubnetClaimReconciler) record(ctx context.Context, claim *networkv1beta1.SubnetClaim, rec audit.Record) {
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
	rec.CreatedBy = createdBy(claim, r.WebhooksEnabled)
	audit.Emit(ctx, r.Audit, rec)
}

// reconcileAllocations keeps allocations for zones still in the spec, and adopts subnets that
// carry this claim's tag (created before a status update was lost, or by a previous claim
// with the same name).
func reconcileAllocations(claim *networkv1beta1.SubnetClaim, current []networkv1beta1.SubnetAllocation,
	subnets []networkv1beta1.Subnet) []networkv1beta1.SubnetAllocation {
	var out []networkv1beta1.SubnetAllocation
	for _, a := range current {
		if slices.Contains(claim.Spec.Zones, a.Zone) && a.CIDRBlock != "" {
			out = append(out, a)
		}
	}
	tag := claimTag(claim)
	for _, s := range subnets {
		if s.Status.Tags[networkv1beta1.TagClaim] != tag {
			continue
		}
		if !slices.Contains(claim.Spec.Zones, s.Status.Zone) {
			continue
		}
		if existing := findAllocation(out, s.Status.Zone); existing != nil {
			if existing.SubnetID == "" {
				existing.SubnetID, existing.CIDRBlock, existing.State, existing.Error = s.Spec.ID, s.Status.CIDRBlock, networkv1beta1.AllocationCreated, ""
			}
			continue
		}
		out = append(out, networkv1beta1.SubnetAllocation{
			Name: subnetName(claim, s.Status.Zone), Zone: s.Status.Zone, CIDRBlock: s.Status.CIDRBlock,
			SubnetID: s.Spec.ID, State: networkv1beta1.AllocationCreated,
		})
	}
	return out
}

// findAllocation returns the allocation for a zone. Allocations are keyed by name in the API,
// but the name is derived from the zone, and the zone is what the spec lists.
func findAllocation(list []networkv1beta1.SubnetAllocation, zone string) *networkv1beta1.SubnetAllocation {
	for i := range list {
		if list[i].Zone == zone {
			return &list[i]
		}
	}
	return nil
}

func sortAllocations(list []networkv1beta1.SubnetAllocation) {
	slices.SortFunc(list, func(a, b networkv1beta1.SubnetAllocation) int {
		return strings.Compare(a.Zone, b.Zone)
	})
}

// subnetName is the name of the claim's subnet in a zone, which is also its allocation's key.
func subnetName(claim *networkv1beta1.SubnetClaim, zone string) string {
	return networkv1beta1.SubnetName(claim.NamePrefixOrName(), claim.Spec.Region, zone)
}

func claimTag(claim *networkv1beta1.SubnetClaim) string {
	return claim.Namespace + "/" + claim.Name
}

// subnetTags builds the tags of a subnet: the claim's own tags first, the organization's
// hs/* tags on top so they cannot be overridden.
func subnetTags(claim *networkv1beta1.SubnetClaim, zone string) map[string]string {
	tags := map[string]string{}
	maps.Copy(tags, claim.Spec.Tags)
	tags["Name"] = subnetName(claim, zone)
	tags[networkv1beta1.DefaultOwnerTagKey] = claim.Spec.Owner
	if claim.Spec.Env != "" {
		tags[networkv1beta1.DefaultEnvTagKey] = claim.Spec.Env
	}
	if claim.Spec.Tier != "" {
		tags[networkv1beta1.DefaultTierTagKey] = claim.Spec.Tier
	}
	tags[networkv1beta1.TagManagedBy] = networkv1beta1.TagManagedByValue
	tags[networkv1beta1.TagClaim] = claimTag(claim)
	return tags
}

// namespaceRefusal says why objects in the namespace may not use the scope, or "" when they
// may. The error is for a failure to ask; a namespace that is gone, or a selector that does
// not parse, is a refusal like any other.
func namespaceRefusal(ctx context.Context, reader client.Reader, scope *networkv1beta1.NetworkScope,
	namespace string) (string, error) {
	allowed, err := tenancy.Allowed(ctx, reader, scope, namespace)
	switch {
	case tenancy.Refused(err):
		return err.Error(), nil
	case err != nil:
		return "", err
	case !allowed:
		return tenancy.NotAllowedMessage(scope, namespace), nil
	}
	return "", nil
}

// writeTarget returns the write target for the account/region, if the scope covers it. Its
// identity is the account's write identity, which is the operator's own for the account the
// operator runs in.
func writeTarget(scope *networkv1beta1.NetworkScope, p provider.Provider, account, region string) (inventory.Target, bool) {
	a, ok := scope.Account(account)
	if !ok || !scope.Covers(account, region) {
		return inventory.Target{}, false
	}
	return inventory.Target{Provider: p.Name(), Scope: scope.Name, Account: account, Region: region,
		Identity: p.Identity(a, provider.Write)}, true
}

func setCondition(status *networkv1beta1.SubnetClaimStatus, typ string, st metav1.ConditionStatus, reason, msg string, gen int64) {
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

func (r *SubnetClaimReconciler) writeStatus(ctx context.Context, claim *networkv1beta1.SubnetClaim, status networkv1beta1.SubnetClaimStatus) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &networkv1beta1.SubnetClaim{}
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
		For(&networkv1beta1.SubnetClaim{}).
		Named("subnetclaim").
		Complete(r)
}
