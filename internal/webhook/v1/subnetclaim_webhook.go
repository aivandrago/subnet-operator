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

package v1

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/allocator"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

var subnetclaimlog = logf.Log.WithName("subnetclaim-resource")

// SetupSubnetClaimWebhookWithManager registers the webhook for SubnetClaim in the manager.
// writesEnabled mirrors the manager's --enable-writes switch: the webhook only warns about
// it, so that a claim applied to a read-only operator says so at apply time.
func SetupSubnetClaimWebhookWithManager(mgr ctrl.Manager, providers *provider.Registry, writesEnabled bool) error {
	return ctrl.NewWebhookManagedBy(mgr, &networkv1.SubnetClaim{}).
		WithValidator(&SubnetClaimValidator{Client: mgr.GetClient(), Providers: providers, WritesEnabled: writesEnabled}).
		WithDefaulter(&SubnetClaimDefaulter{Client: mgr.GetClient()}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-network-hypersurgery-dev-v1-subnetclaim,mutating=true,failurePolicy=ignore,sideEffects=None,groups=network.hypersurgery.dev,resources=subnetclaims,verbs=create;update,versions=v1,name=msubnetclaim-v1.kb.io,admissionReviewVersions=v1

// SubnetClaimDefaulter fills in the fields a claim can work out for itself.
type SubnetClaimDefaulter struct {
	// Client reads the claim's scope, whose provider decides how IDs are spelt.
	Client client.Reader
}

// Default records who created the claim and writes the name prefix into the spec. The
// controller already falls back to the claim's name; doing it here as well means the Name tag
// the subnets will carry is visible in the object instead of only in the operator's head.
//
// On a provider whose IDs are case-insensitive (Azure) it also rewrites spec.account and
// spec.networkID into the operator's spelling, lowercase: an ID pasted from the portal
// (/subscriptions/.../resourceGroups/My-RG/providers/Microsoft.Network/virtualNetworks/hub) is
// accepted, and the object then shows the ID the inventory, the events and the audit trail use.
// AWS and GCP IDs are case-sensitive and stay as written.
func (d *SubnetClaimDefaulter) Default(ctx context.Context, obj *networkv1.SubnetClaim) error {
	stampCreatedBy(ctx, obj)
	obj.CanonicalizeIDs(canonicalProvider(ctx, d.Client, obj.Spec.ScopeRef))
	if obj.Spec.NamePrefix == "" && obj.Name != "" {
		obj.Spec.NamePrefix = obj.Name
	}
	if obj.Spec.Mode == "" {
		obj.Spec.Mode = networkv1.ClaimModeCreate
	}
	return nil
}

// +kubebuilder:webhook:path=/validate-network-hypersurgery-dev-v1-subnetclaim,mutating=false,failurePolicy=fail,sideEffects=None,groups=network.hypersurgery.dev,resources=subnetclaims,verbs=create;update,versions=v1,name=vsubnetclaim-v1.kb.io,admissionReviewVersions=v1

// SubnetClaimValidator refuses claims that can never be satisfied, and warns about the ones
// that will simply wait.
type SubnetClaimValidator struct {
	// Client reads the scope and the inventory the claim is checked against.
	Client client.Reader
	// Providers are the clouds the operator runs with; the claim's scope picks the one whose
	// rules the claim is checked against.
	Providers *provider.Registry
	// WritesEnabled is the manager's --enable-writes switch.
	WritesEnabled bool
}

// ValidateCreate checks a new claim.
func (v *SubnetClaimValidator) ValidateCreate(ctx context.Context, obj *networkv1.SubnetClaim) (
	admission.Warnings, error) {
	subnetclaimlog.V(1).Info("Validating SubnetClaim on create", "name", obj.GetName())
	if e := validateCreatedByOnCreate(ctx, obj); e != nil {
		return nil, invalidError("SubnetClaim", obj.Name, field.ErrorList{e})
	}
	return v.validate(ctx, obj, nil)
}

// ValidateUpdate checks a changed claim, and first of all that it still points at the same
// network: a claim that moves has already created subnets somewhere else, and the operator
// never deletes them.
func (v *SubnetClaimValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *networkv1.SubnetClaim) (
	admission.Warnings, error) {
	subnetclaimlog.V(1).Info("Validating SubnetClaim on update", "name", newObj.GetName())

	spec := field.NewPath("spec")
	var errs field.ErrorList
	// The scope cannot change, so the old object's names the provider for both.
	cloud := canonicalProvider(ctx, v.Client, oldObj.Spec.ScopeRef)
	for _, e := range []*field.Error{
		immutableField(spec.Child("scopeRef"), oldObj.Spec.ScopeRef, newObj.Spec.ScopeRef),
		immutableID(spec.Child("account"), oldObj.Spec.Account, newObj.Spec.Account,
			func(id string) string { return networkv1.CanonicalAccountID(cloud, id) }),
		immutableField(spec.Child("region"), oldObj.Spec.Region, newObj.Spec.Region),
		immutableID(spec.Child("networkID"), oldObj.Spec.NetworkID, newObj.Spec.NetworkID,
			func(id string) string { return networkv1.CanonicalResourceID(cloud, id) }),
		validateCreatedByOnUpdate(oldObj, newObj),
	} {
		if e != nil {
			errs = append(errs, e)
		}
	}
	// The prefix length only matters while nothing is reserved yet. Afterwards the existing
	// allocations keep their size, so a change would quietly apply to new zones only.
	if len(oldObj.Status.Allocations) > 0 && oldObj.Spec.PrefixLength != newObj.Spec.PrefixLength {
		errs = append(errs, field.Invalid(spec.Child("prefixLength"), newObj.Spec.PrefixLength,
			fmt.Sprintf("prefixLength cannot change once CIDRs are reserved (was %d): delete the claim and write a new one",
				oldObj.Spec.PrefixLength)))
	}
	if len(errs) > 0 {
		return nil, invalidError("SubnetClaim", newObj.Name, errs)
	}

	warnings, err := v.validate(ctx, newObj, oldObj)
	return append(warnings, droppedZoneWarnings(oldObj, newObj)...), err
}

// ValidateDelete accepts every deletion: the subnets stay in the cloud, which is the point.
func (v *SubnetClaimValidator) ValidateDelete(_ context.Context, _ *networkv1.SubnetClaim) (
	admission.Warnings, error) {
	return nil, nil
}

// Validate is everything that is checked on both create and update.
func (v *SubnetClaimValidator) Validate(ctx context.Context, claim *networkv1.SubnetClaim) (
	admission.Warnings, error) {
	return v.validate(ctx, claim, nil)
}

// validate is Validate, with the claim as it was before an update (nil on create).
func (v *SubnetClaimValidator) validate(ctx context.Context, claim, oldClaim *networkv1.SubnetClaim) (
	admission.Warnings, error) {
	spec := field.NewPath("spec")
	var errs field.ErrorList
	var warnings admission.Warnings

	scope, e := scopeFor(ctx, v.Client, spec.Child("scopeRef"), claim.Spec.ScopeRef)
	if e != nil {
		return warnings, invalidError("SubnetClaim", claim.Name, append(errs, e))
	}
	// The mutating webhook stores the IDs in the operator's spelling, but it is failurePolicy
	// Ignore: when it did not run, the claim is still checked as the controller will read it,
	// against the network its ID names in any case.
	claim = claim.DeepCopy()
	claim.CanonicalizeIDs(scope.Spec.Provider)
	// What the fields and tags must look like depends on the provider, which is the scope's.
	p, e := providerFor(v.Providers, scope, claim.Spec.ScopeRef)
	if e != nil {
		return warnings, invalidError("SubnetClaim", claim.Name, append(errs, e))
	}
	if _, ok := claim.Spec.Tags["Name"]; ok && len(claim.Spec.Zones) > 0 {
		warnings = append(warnings, "the Name tag is overwritten with namePrefix plus the zone")
	}
	errs = append(errs, p.ValidateTags(spec.Child("tags"), claim.Spec.Tags)...)
	errs = append(errs, p.ValidateClaim(claim)...)
	if e := validateNamespace(ctx, v.Client, scope, claim.Namespace); e != nil {
		return warnings, invalidError("SubnetClaim", claim.Name, append(errs, e))
	}
	if !scope.Covers(claim.Spec.Account, claim.Spec.Region) {
		errs = append(errs, field.Invalid(spec.Child("account"), claim.Spec.Account,
			fmt.Sprintf("account %s in %s is not discovered by NetworkScope %q; add it to spec.accounts or spec.regions there",
				claim.Spec.Account, claim.Spec.Region, scope.Name)))
		return warnings, invalidError("SubnetClaim", claim.Name, errs)
	}

	// Create mode needs a provider that can create subnets, and a write identity of its own in
	// every account the operator reaches with a read identity of its own (on AWS, through
	// sts:AssumeRole). Without one the claim would allocate and then stop.
	if claim.Spec.Mode == networkv1.ClaimModeCreate {
		switch {
		case !provider.HasCapability(p, networkv1.CapabilityCreateSubnet):
			errs = append(errs, field.Invalid(spec.Child("mode"), claim.Spec.Mode,
				fmt.Sprintf("provider %s cannot create subnets in this release; use mode Allocate to only reserve CIDRs",
					p.Name())))
		case provider.MissingWriteIdentity(p, scope, claim.Spec.Account):
			errs = append(errs, field.Invalid(spec.Child("mode"), claim.Spec.Mode,
				fmt.Sprintf("account %s has no %s in NetworkScope %q, so subnets cannot be created there; "+
					"set one, or use mode Allocate to only reserve CIDRs", claim.Spec.Account, p.WriteIdentityField(),
					scope.Name)))
		case !v.WritesEnabled:
			warnings = append(warnings,
				writesDisabledWarning("the CIDRs are reserved but no subnet is created")...)
		}
	}

	if scope.Spec.Provider == networkv1.ProviderGCP {
		idWarnings, idErrs := v.validateUniqueSubnetworkID(ctx, claim, oldClaim)
		warnings = append(warnings, idWarnings...)
		errs = append(errs, idErrs...)
	}

	networkWarnings, networkErrs := v.validateAgainstNetwork(ctx, claim, scope)
	warnings = append(warnings, networkWarnings...)
	errs = append(errs, networkErrs...)

	return warnings, invalidError("SubnetClaim", claim.Name, errs)
}

// validateUniqueSubnetworkID refuses a GCP claim that leads to the same subnetwork ID
// (projects/<account>/regions/<region>/subnetworks/<namePrefix>) as another claim of its scope,
// in any namespace, or to one another claim already holds in its status. A subnetwork on GCP is
// named by namePrefix alone, and with spec.gcp.claimTag Skip a claim that lost its status finds
// its subnetwork again by that ID; with two claims leading to it, neither would, and the second
// to create would fail with "exists already". Two claims in mode Allocate create nothing, so they
// may share a namePrefix.
//
// On an update, a clash the claim already had before (both written before this check existed,
// or while the webhooks were off) is a warning, not an error, so that the claim can still be
// changed and deleted; only a clash the update itself brings in (a new namePrefix, or mode
// Allocate turned into Create) is refused.
//
// The claims are read from the manager's cache, so two claims created at the same moment can
// both be admitted. The controller then still adopts neither subnetwork by name (adoptsByName),
// which is safe: the second create fails with "exists already", as before this check.
func (v *SubnetClaimValidator) validateUniqueSubnetworkID(ctx context.Context,
	claim, oldClaim *networkv1.SubnetClaim) (admission.Warnings, field.ErrorList) {
	if oldClaim != nil && !claim.DeletionTimestamp.IsZero() {
		return nil, nil // the controller removing its finalizer
	}
	claims := &networkv1.SubnetClaimList{}
	if err := v.Client.List(ctx, claims); err != nil {
		// The controller checks the same thing before it adopts anything by name.
		return admission.Warnings{"the other claims could not be read, so the subnetwork name was not checked " +
			"for clashes: " + err.Error()}, nil
	}
	id := claim.GCPSubnetworkID()
	var clashes []string
	var warnings admission.Warnings
	for i := range claims.Items {
		other := &claims.Items[i]
		if other.Spec.ScopeRef != claim.Spec.ScopeRef ||
			(other.Namespace == claim.Namespace && other.Name == claim.Name) {
			continue
		}
		why := subnetworkClash(claim, other)
		if why == "" {
			continue
		}
		name := other.Namespace + "/" + other.Name
		if oldClaim != nil && subnetworkClash(oldClaim, other) != "" {
			warnings = append(warnings, fmt.Sprintf("SubnetClaim %s %s %s too; neither claim can find its "+
				"subnetwork again by name if its status is lost: give one of them another namePrefix", name, why, id))
			continue
		}
		clashes = append(clashes, fmt.Sprintf("SubnetClaim %s %s", name, why))
	}
	if len(clashes) == 0 {
		return warnings, nil
	}
	return warnings, field.ErrorList{field.Invalid(field.NewPath("spec", "namePrefix"), claim.NamePrefixOrName(),
		fmt.Sprintf("the claim leads to subnetwork %s, and %s it already; on GCP namePrefix is the subnetwork's "+
			"name, so choose another namePrefix", id, strings.Join(clashes, " and ")))}
}

// subnetworkClash says how other stands in the way of the subnetwork claim creates or finds
// again, or returns "" when it does not.
func subnetworkClash(claim, other *networkv1.SubnetClaim) string {
	id := claim.GCPSubnetworkID()
	if claim.CreatesSubnets() && slices.ContainsFunc(other.Status.Allocations,
		func(a networkv1.SubnetAllocation) bool { return a.SubnetID == id }) {
		return "holds"
	}
	if other.GCPSubnetworkID() == id && (claim.CreatesSubnets() || other.CreatesSubnets()) {
		return "leads to"
	}
	return ""
}

// validateAgainstNetwork checks the claim against the discovered network: that the requested
// prefix fits inside it at all, and that there is still room for the subnets that are missing.
// A network that has not been discovered yet is a warning, not an error: the next resync may
// well bring it, and a claim written next to its scope must not depend on the apply order.
func (v *SubnetClaimValidator) validateAgainstNetwork(ctx context.Context, claim *networkv1.SubnetClaim,
	scope *networkv1.NetworkScope) (admission.Warnings, field.ErrorList) {
	spec := field.NewPath("spec")

	network := &networkv1.Network{}
	if err := v.Client.Get(ctx, client.ObjectKey{Name: networkv1.ObjectName(claim.Spec.NetworkID)}, network); err != nil {
		if apierrors.IsNotFound(err) {
			return admission.Warnings{fmt.Sprintf(
				"network %s is not in the inventory of NetworkScope %q (yet); the claim stays pending until it is discovered",
				claim.Spec.NetworkID, scope.Name)}, nil
		}
		return admission.Warnings{"the network could not be read, so the claim was not checked against it: " + err.Error()}, nil
	}
	// A global network (GCP, no region) serves every region of its project.
	if !networkv1.SameAccount(network.Spec.Provider, network.Spec.Account, claim.Spec.Account) || (network.Spec.Region != "" && network.Spec.Region != claim.Spec.Region) {
		return nil, field.ErrorList{field.Invalid(spec.Child("networkID"), claim.Spec.NetworkID,
			fmt.Sprintf("network %s is in %s/%s, not in %s/%s", claim.Spec.NetworkID,
				network.Spec.Account, network.Spec.Region, claim.Spec.Account, claim.Spec.Region))}
	}

	poolCIDRs := claim.PoolCIDRs(network)
	if len(poolCIDRs) == 0 {
		return admission.Warnings{fmt.Sprintf("network %s has no CIDR blocks in the inventory yet", claim.Spec.NetworkID)}, nil
	}
	if largest, ok := largestPrefix(poolCIDRs); ok && int(claim.Spec.PrefixLength) < largest {
		return nil, field.ErrorList{field.Invalid(spec.Child("prefixLength"), claim.Spec.PrefixLength,
			fmt.Sprintf("a /%d does not fit in network %s, whose largest block is a /%d",
				claim.Spec.PrefixLength, claim.Spec.NetworkID, largest))}
	}

	needed := 0
	for _, zone := range claim.Slots() {
		if !slices.ContainsFunc(claim.Status.Allocations, func(a networkv1.SubnetAllocation) bool {
			return a.Zone == zone && a.CIDRBlock != ""
		}) {
			needed++
		}
	}
	if needed == 0 {
		return nil, nil
	}

	pool, err := v.poolFor(ctx, claim, network, scope.Spec.Provider)
	if err != nil {
		// The inventory could not be read. The controller checks the same thing again, so
		// refusing the claim over our own failure would help nobody.
		return admission.Warnings{"the free space in the network could not be checked: " + err.Error()}, nil
	}
	if _, err := allocator.Allocate(pool, int(claim.Spec.PrefixLength), needed); err != nil {
		if errors.Is(err, allocator.ErrNoSpace) {
			return nil, field.ErrorList{field.Invalid(spec.Child("prefixLength"), claim.Spec.PrefixLength,
				fmt.Sprintf("network %s has no room for %d more /%d: %v",
					claim.Spec.NetworkID, needed, claim.Spec.PrefixLength, err))}
		}
		return admission.Warnings{"the free space in the network could not be checked: " + err.Error()}, nil
	}
	return nil, nil
}

// poolFor is what is taken in the network already: the discovered subnets plus the reservations
// of every other claim. It is the same pool the controller allocates from, read through the
// manager's cache, so a claim that is refused here would have failed there too.
//
// Another claim is in the same network when its ID names it as cloud compares IDs: without
// regard to case on Azure, where a claim written while the webhooks were off may still spell
// the ID as the portal does, exactly on AWS and GCP.
func (v *SubnetClaimValidator) poolFor(ctx context.Context, claim *networkv1.SubnetClaim,
	network *networkv1.Network, cloud networkv1.Provider) (allocator.Pool, error) {
	pool := allocator.Pool{CIDRs: claim.PoolCIDRs(network)}

	subnets := &networkv1.SubnetList{}
	if err := v.Client.List(ctx, subnets, client.MatchingLabels{networkv1.LabelNetwork: networkv1.ObjectName(claim.Spec.NetworkID)}); err != nil {
		return allocator.Pool{}, err
	}
	for i := range subnets.Items {
		pool.Used = append(pool.Used, subnets.Items[i].Status.CIDRBlock)
		pool.Used = append(pool.Used, subnets.Items[i].Status.SecondaryCIDRBlocks...)
	}

	claims := &networkv1.SubnetClaimList{}
	if err := v.Client.List(ctx, claims); err != nil {
		return allocator.Pool{}, err
	}
	for i := range claims.Items {
		other := &claims.Items[i]
		if !networkv1.SameResourceID(cloud, other.Spec.NetworkID, claim.Spec.NetworkID) {
			continue
		}
		if other.Namespace == claim.Namespace && other.Name == claim.Name {
			continue // our own reservations are counted through status.allocations below
		}
		for _, a := range other.Status.Allocations {
			pool.Used = append(pool.Used, a.CIDRBlock)
		}
	}
	for _, a := range claim.Status.Allocations {
		pool.Used = append(pool.Used, a.CIDRBlock)
	}
	return pool, nil
}

// droppedZoneWarnings says out loud what dropping a zone means: the subnet that was created
// for it stays in the cloud, because the operator never deletes one.
func droppedZoneWarnings(oldObj, newObj *networkv1.SubnetClaim) admission.Warnings {
	var warnings admission.Warnings
	for _, a := range oldObj.Status.Allocations {
		if a.SubnetID == "" || slices.Contains(newObj.Slots(), a.Zone) {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"zone %s was dropped: subnet %s (%s) stays in the cloud, the operator never deletes one",
			a.Zone, a.SubnetID, a.CIDRBlock))
	}
	return warnings
}

// largestPrefix returns the prefix length of the biggest IPv4 block in the list, which is
// the smallest number of bits. It reports false when none of them parses as IPv4.
func largestPrefix(cidrs []string) (int, bool) {
	largest, found := 33, false
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil || !p.Addr().Is4() {
			continue
		}
		if p.Bits() < largest {
			largest, found = p.Bits(), true
		}
	}
	return largest, found
}
