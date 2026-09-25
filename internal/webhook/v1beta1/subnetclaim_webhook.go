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

package v1beta1

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
	"hypersurgery.dev/subnet-operator/internal/allocator"
)

var subnetclaimlog = logf.Log.WithName("subnetclaim-resource")

// SetupSubnetClaimWebhookWithManager registers the webhook for SubnetClaim in the manager.
// writesEnabled mirrors the manager's --enable-writes switch: the webhook only warns about
// it, so that a claim applied to a read-only operator says so at apply time. operator is the
// user the operator authenticates as, which may carry a migrated claim's creator over.
func SetupSubnetClaimWebhookWithManager(mgr ctrl.Manager, writesEnabled bool, operator string) error {
	return ctrl.NewWebhookManagedBy(mgr, &networkv1beta1.SubnetClaim{}).
		WithValidator(&SubnetClaimValidator{Client: mgr.GetClient(), WritesEnabled: writesEnabled, Operator: operator}).
		WithDefaulter(&SubnetClaimDefaulter{Operator: operator}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-network-hypersurgery-dev-v1beta1-subnetclaim,mutating=true,failurePolicy=ignore,sideEffects=None,groups=network.hypersurgery.dev,resources=subnetclaims,verbs=create;update,versions=v1beta1,name=msubnetclaim-v1beta1.kb.io,admissionReviewVersions=v1

// SubnetClaimDefaulter fills in the fields a claim can work out for itself.
type SubnetClaimDefaulter struct {
	// Operator is the user the operator authenticates as; see keepsCopiedCreator.
	Operator string
}

// Default records who created the claim and writes the name prefix into the spec. The
// controller already falls back to the claim's name; doing it here as well means the Name tag
// the subnets will carry is visible in the object instead of only in the operator's head.
func (d *SubnetClaimDefaulter) Default(ctx context.Context, obj *networkv1beta1.SubnetClaim) error {
	stampCreatedBy(ctx, d.Operator, obj)
	if obj.Spec.NamePrefix == "" && obj.Name != "" {
		obj.Spec.NamePrefix = obj.Name
	}
	if obj.Spec.Mode == "" {
		obj.Spec.Mode = networkv1beta1.ClaimModeCreate
	}
	return nil
}

// +kubebuilder:webhook:path=/validate-network-hypersurgery-dev-v1beta1-subnetclaim,mutating=false,failurePolicy=fail,sideEffects=None,groups=network.hypersurgery.dev,resources=subnetclaims,verbs=create;update,versions=v1beta1,name=vsubnetclaim-v1beta1.kb.io,admissionReviewVersions=v1

// SubnetClaimValidator refuses claims that can never be satisfied, and warns about the ones
// that will simply wait.
type SubnetClaimValidator struct {
	// Client reads the scope and the inventory the claim is checked against.
	Client client.Reader
	// WritesEnabled is the manager's --enable-writes switch.
	WritesEnabled bool
	// Operator is the user the operator authenticates as; see keepsCopiedCreator.
	Operator string
}

// ValidateCreate checks a new claim.
func (v *SubnetClaimValidator) ValidateCreate(ctx context.Context, obj *networkv1beta1.SubnetClaim) (
	admission.Warnings, error) {
	subnetclaimlog.V(1).Info("Validating SubnetClaim on create", "name", obj.GetName())
	if e := validateCreatedByOnCreate(ctx, v.Operator, obj); e != nil {
		return nil, invalidError("SubnetClaim", obj.Name, field.ErrorList{e})
	}
	if createdByMigration(ctx, v.Operator, obj) {
		return nil, nil
	}
	return v.Validate(ctx, obj)
}

// ValidateUpdate checks a changed claim, and first of all that it still points at the same
// network: a claim that moves has already created subnets somewhere else, and the operator
// never deletes them.
func (v *SubnetClaimValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *networkv1beta1.SubnetClaim) (
	admission.Warnings, error) {
	subnetclaimlog.V(1).Info("Validating SubnetClaim on update", "name", newObj.GetName())

	spec := field.NewPath("spec")
	var errs field.ErrorList
	for _, e := range []*field.Error{
		immutableField(spec.Child("scopeRef"), oldObj.Spec.ScopeRef, newObj.Spec.ScopeRef),
		immutableField(spec.Child("account"), oldObj.Spec.Account, newObj.Spec.Account),
		immutableField(spec.Child("region"), oldObj.Spec.Region, newObj.Spec.Region),
		immutableField(spec.Child("networkID"), oldObj.Spec.NetworkID, newObj.Spec.NetworkID),
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

	warnings, err := v.Validate(ctx, newObj)
	return append(warnings, droppedZoneWarnings(oldObj, newObj)...), err
}

// ValidateDelete accepts every deletion: the subnets stay in the cloud, which is the point.
func (v *SubnetClaimValidator) ValidateDelete(_ context.Context, _ *networkv1beta1.SubnetClaim) (
	admission.Warnings, error) {
	return nil, nil
}

// Validate is everything that is checked on both create and update. The webhook for
// aws.hypersurgery/v1alpha1 runs it on the converted form of an old object.
func (v *SubnetClaimValidator) Validate(ctx context.Context, claim *networkv1beta1.SubnetClaim) (
	admission.Warnings, error) {
	spec := field.NewPath("spec")
	var errs field.ErrorList
	var warnings admission.Warnings

	errs = append(errs, validateTags(spec.Child("tags"), claim.Spec.Tags)...)
	if _, ok := claim.Spec.Tags["Name"]; ok {
		warnings = append(warnings, "the Name tag is overwritten with namePrefix plus the zone")
	}

	scope, e := scopeFor(ctx, v.Client, spec.Child("scopeRef"), claim.Spec.ScopeRef)
	if e != nil {
		return warnings, invalidError("SubnetClaim", claim.Name, append(errs, e))
	}
	// What the fields must look like depends on the provider, which is the scope's.
	if scope.Spec.Provider != networkv1beta1.ProviderAWS {
		return warnings, invalidError("SubnetClaim", claim.Name, append(errs, field.Invalid(spec.Child("scopeRef"),
			claim.Spec.ScopeRef, fmt.Sprintf("NetworkScope %q has provider %q, which this operator does not support",
				scope.Name, scope.Spec.Provider))))
	}
	errs = append(errs, validateAWSClaim(claim)...)
	if e := validateNamespace(ctx, v.Client, scope, claim.Namespace); e != nil {
		return warnings, invalidError("SubnetClaim", claim.Name, append(errs, e))
	}
	if !scope.Covers(claim.Spec.Account, claim.Spec.Region) {
		errs = append(errs, field.Invalid(spec.Child("account"), claim.Spec.Account,
			fmt.Sprintf("account %s in %s is not discovered by NetworkScope %q; add it to spec.accounts or spec.regions there",
				claim.Spec.Account, claim.Spec.Region, scope.Name)))
		return warnings, invalidError("SubnetClaim", claim.Name, errs)
	}

	// Create mode needs a write role of its own in every account the operator reaches through
	// sts:AssumeRole. Without one the claim would allocate and then stop.
	if claim.Spec.Mode == networkv1beta1.ClaimModeCreate {
		account, _ := scope.Account(claim.Spec.Account)
		if account.AWSAccount().WriteRoleARN == "" && scope.AccountHasReadRole(claim.Spec.Account) {
			errs = append(errs, field.Invalid(spec.Child("mode"), claim.Spec.Mode,
				fmt.Sprintf("account %s has no aws.writeRoleARN in NetworkScope %q, so subnets cannot be created there; "+
					"set one, or use mode Allocate to only reserve CIDRs", claim.Spec.Account, scope.Name)))
		} else if !v.WritesEnabled {
			warnings = append(warnings,
				writesDisabledWarning("the CIDRs are reserved but no subnet is created")...)
		}
	}

	networkWarnings, networkErrs := v.validateAgainstNetwork(ctx, claim, scope)
	warnings = append(warnings, networkWarnings...)
	errs = append(errs, networkErrs...)

	return warnings, invalidError("SubnetClaim", claim.Name, errs)
}

// validateAgainstNetwork checks the claim against the discovered network: that the requested
// prefix fits inside it at all, and that there is still room for the subnets that are missing.
// A network that has not been discovered yet is a warning, not an error: the next resync may
// well bring it, and a claim written next to its scope must not depend on the apply order.
func (v *SubnetClaimValidator) validateAgainstNetwork(ctx context.Context, claim *networkv1beta1.SubnetClaim,
	scope *networkv1beta1.NetworkScope) (admission.Warnings, field.ErrorList) {
	spec := field.NewPath("spec")

	network := &networkv1beta1.Network{}
	if err := v.Client.Get(ctx, client.ObjectKey{Name: claim.Spec.NetworkID}, network); err != nil {
		if apierrors.IsNotFound(err) {
			return admission.Warnings{fmt.Sprintf(
				"network %s is not in the inventory of NetworkScope %q (yet); the claim stays pending until it is discovered",
				claim.Spec.NetworkID, scope.Name)}, nil
		}
		return admission.Warnings{"the network could not be read, so the claim was not checked against it: " + err.Error()}, nil
	}
	if network.Spec.Account != claim.Spec.Account || network.Spec.Region != claim.Spec.Region {
		return nil, field.ErrorList{field.Invalid(spec.Child("networkID"), claim.Spec.NetworkID,
			fmt.Sprintf("network %s is in %s/%s, not in %s/%s", claim.Spec.NetworkID,
				network.Spec.Account, network.Spec.Region, claim.Spec.Account, claim.Spec.Region))}
	}

	if len(network.Status.CIDRBlocks) == 0 {
		return admission.Warnings{fmt.Sprintf("network %s has no CIDR blocks in the inventory yet", claim.Spec.NetworkID)}, nil
	}
	if largest, ok := largestPrefix(network.Status.CIDRBlocks); ok && int(claim.Spec.PrefixLength) < largest {
		return nil, field.ErrorList{field.Invalid(spec.Child("prefixLength"), claim.Spec.PrefixLength,
			fmt.Sprintf("a /%d does not fit in network %s, whose largest block is a /%d",
				claim.Spec.PrefixLength, claim.Spec.NetworkID, largest))}
	}

	needed := 0
	for _, zone := range claim.Spec.Zones {
		if !slices.ContainsFunc(claim.Status.Allocations, func(a networkv1beta1.SubnetAllocation) bool {
			return a.Zone == zone && a.CIDRBlock != ""
		}) {
			needed++
		}
	}
	if needed == 0 {
		return nil, nil
	}

	pool, err := v.poolFor(ctx, claim, network)
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
func (v *SubnetClaimValidator) poolFor(ctx context.Context, claim *networkv1beta1.SubnetClaim,
	network *networkv1beta1.Network) (allocator.Pool, error) {
	pool := allocator.Pool{CIDRs: network.Status.CIDRBlocks}

	subnets := &networkv1beta1.SubnetList{}
	if err := v.Client.List(ctx, subnets, client.MatchingLabels{networkv1beta1.LabelNetwork: claim.Spec.NetworkID}); err != nil {
		return allocator.Pool{}, err
	}
	for i := range subnets.Items {
		pool.Used = append(pool.Used, subnets.Items[i].Status.CIDRBlock)
	}

	claims := &networkv1beta1.SubnetClaimList{}
	if err := v.Client.List(ctx, claims); err != nil {
		return allocator.Pool{}, err
	}
	for i := range claims.Items {
		other := &claims.Items[i]
		if other.Spec.NetworkID != claim.Spec.NetworkID {
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
func droppedZoneWarnings(oldObj, newObj *networkv1beta1.SubnetClaim) admission.Warnings {
	var warnings admission.Warnings
	for _, a := range oldObj.Status.Allocations {
		if a.SubnetID == "" || slices.Contains(newObj.Spec.Zones, a.Zone) {
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

// awsNetworkID is the shape of a VPC ID, which is what networkID is on AWS.
var awsNetworkID = regexp.MustCompile(`^vpc-[0-9a-f]+$`)

// AWS subnet sizes and the number of zones a claim may span.
const (
	awsMinPrefixLength = 16
	awsMaxPrefixLength = 28
	awsMaxZones        = 6
)

// validateAWSClaim checks what the schema cannot, because it depends on the scope's provider
// being AWS: the account and ID formats, the region, one to six availability zones of that
// region, and a prefix length AWS accepts.
func validateAWSClaim(claim *networkv1beta1.SubnetClaim) field.ErrorList {
	spec := field.NewPath("spec")
	var errs field.ErrorList
	if !awsAccountPattern.MatchString(claim.Spec.Account) {
		errs = append(errs, field.Invalid(spec.Child("account"), claim.Spec.Account, "an AWS account ID is 12 digits"))
	}
	if e := validateRegion(spec.Child("region"), claim.Spec.Region); e != nil {
		errs = append(errs, e)
	}
	if !awsNetworkID.MatchString(claim.Spec.NetworkID) {
		errs = append(errs, field.Invalid(spec.Child("networkID"), claim.Spec.NetworkID, "not an AWS VPC ID, e.g. vpc-0abc"))
	}
	if claim.Spec.PrefixLength < awsMinPrefixLength || claim.Spec.PrefixLength > awsMaxPrefixLength {
		errs = append(errs, field.Invalid(spec.Child("prefixLength"), claim.Spec.PrefixLength,
			fmt.Sprintf("AWS subnets are between a /%d and a /%d", awsMinPrefixLength, awsMaxPrefixLength)))
	}
	switch {
	case len(claim.Spec.Zones) == 0:
		errs = append(errs, field.Required(spec.Child("zones"),
			"AWS subnets are zonal: list one to six availability zones, e.g. eu-central-1a"))
	case len(claim.Spec.Zones) > awsMaxZones:
		errs = append(errs, field.TooMany(spec.Child("zones"), len(claim.Spec.Zones), awsMaxZones))
	}
	for i, zone := range claim.Spec.Zones {
		if !strings.HasPrefix(zone, claim.Spec.Region) {
			errs = append(errs, field.Invalid(spec.Child("zones").Index(i), zone,
				fmt.Sprintf("not an availability zone of region %s", claim.Spec.Region)))
		}
	}
	return errs
}
