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

package v1alpha1

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

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
	"hypersurgery.dev/subnet-operator/internal/allocator"
)

var subnetclaimlog = logf.Log.WithName("subnetclaim-resource")

// SetupSubnetClaimWebhookWithManager registers the webhook for SubnetClaim in the manager.
// writesEnabled mirrors the manager's --enable-writes switch: the webhook only warns about
// it, so that a claim applied to a read-only operator says so at apply time.
func SetupSubnetClaimWebhookWithManager(mgr ctrl.Manager, writesEnabled bool) error {
	return ctrl.NewWebhookManagedBy(mgr, &awsv1alpha1.SubnetClaim{}).
		WithValidator(&SubnetClaimValidator{Client: mgr.GetClient(), WritesEnabled: writesEnabled}).
		WithDefaulter(&SubnetClaimDefaulter{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-aws-hypersurgery-v1alpha1-subnetclaim,mutating=true,failurePolicy=ignore,sideEffects=None,groups=aws.hypersurgery,resources=subnetclaims,verbs=create;update,versions=v1alpha1,name=msubnetclaim-v1alpha1.kb.io,admissionReviewVersions=v1

// SubnetClaimDefaulter fills in the fields a claim can work out for itself.
type SubnetClaimDefaulter struct{}

// Default records who created the claim and writes the name prefix into the spec. The
// controller already falls back to the claim's name; doing it here as well means the Name tag
// the subnets will carry is visible in the object instead of only in the operator's head.
func (d *SubnetClaimDefaulter) Default(ctx context.Context, obj *awsv1alpha1.SubnetClaim) error {
	stampCreatedBy(ctx, obj)
	if obj.Spec.NamePrefix == "" && obj.Name != "" {
		obj.Spec.NamePrefix = obj.Name
	}
	if obj.Spec.Mode == "" {
		obj.Spec.Mode = awsv1alpha1.ClaimModeCreate
	}
	return nil
}

// +kubebuilder:webhook:path=/validate-aws-hypersurgery-v1alpha1-subnetclaim,mutating=false,failurePolicy=fail,sideEffects=None,groups=aws.hypersurgery,resources=subnetclaims,verbs=create;update,versions=v1alpha1,name=vsubnetclaim-v1alpha1.kb.io,admissionReviewVersions=v1

// SubnetClaimValidator refuses claims that can never be satisfied, and warns about the ones
// that will simply wait.
type SubnetClaimValidator struct {
	// Client reads the scope and the inventory the claim is checked against.
	Client client.Reader
	// WritesEnabled is the manager's --enable-writes switch.
	WritesEnabled bool
}

// ValidateCreate checks a new claim.
func (v *SubnetClaimValidator) ValidateCreate(ctx context.Context, obj *awsv1alpha1.SubnetClaim) (
	admission.Warnings, error) {
	subnetclaimlog.V(1).Info("Validating SubnetClaim on create", "name", obj.GetName())
	if e := validateCreatedByOnCreate(ctx, obj); e != nil {
		return nil, invalidError("SubnetClaim", obj.Name, field.ErrorList{e})
	}
	return v.validate(ctx, obj)
}

// ValidateUpdate checks a changed claim, and first of all that it still points at the same
// VPC: a claim that moves has already created subnets somewhere else, and the operator never
// deletes them.
func (v *SubnetClaimValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *awsv1alpha1.SubnetClaim) (
	admission.Warnings, error) {
	subnetclaimlog.V(1).Info("Validating SubnetClaim on update", "name", newObj.GetName())

	spec := field.NewPath("spec")
	var errs field.ErrorList
	for _, e := range []*field.Error{
		immutableField(spec.Child("scopeRef"), oldObj.Spec.ScopeRef, newObj.Spec.ScopeRef),
		immutableField(spec.Child("account"), oldObj.Spec.Account, newObj.Spec.Account),
		immutableField(spec.Child("region"), oldObj.Spec.Region, newObj.Spec.Region),
		immutableField(spec.Child("vpcID"), oldObj.Spec.VPCID, newObj.Spec.VPCID),
		validateCreatedByOnUpdate(oldObj, newObj),
	} {
		if e != nil {
			errs = append(errs, e)
		}
	}
	// The prefix length only matters while nothing is reserved yet. Afterwards the existing
	// allocations keep their size, so a change would quietly apply to new AZs only.
	if len(oldObj.Status.Allocations) > 0 && oldObj.Spec.PrefixLength != newObj.Spec.PrefixLength {
		errs = append(errs, field.Invalid(spec.Child("prefixLength"), newObj.Spec.PrefixLength,
			fmt.Sprintf("prefixLength cannot change once CIDRs are reserved (was %d): delete the claim and write a new one",
				oldObj.Spec.PrefixLength)))
	}
	if len(errs) > 0 {
		return nil, invalidError("SubnetClaim", newObj.Name, errs)
	}

	warnings, err := v.validate(ctx, newObj)
	return append(warnings, droppedZoneWarnings(oldObj, newObj)...), err
}

// ValidateDelete accepts every deletion: the subnets stay in AWS, which is the point.
func (v *SubnetClaimValidator) ValidateDelete(_ context.Context, _ *awsv1alpha1.SubnetClaim) (
	admission.Warnings, error) {
	return nil, nil
}

// validate is everything that is checked on both create and update.
func (v *SubnetClaimValidator) validate(ctx context.Context, claim *awsv1alpha1.SubnetClaim) (
	admission.Warnings, error) {
	spec := field.NewPath("spec")
	var errs field.ErrorList
	var warnings admission.Warnings

	errs = append(errs, validateTags(spec.Child("tags"), claim.Spec.Tags)...)
	if _, ok := claim.Spec.Tags["Name"]; ok {
		warnings = append(warnings, "the Name tag is overwritten with namePrefix plus the availability zone")
	}
	if e := validateRegion(spec.Child("region"), claim.Spec.Region); e != nil {
		errs = append(errs, e)
	}
	for i, az := range claim.Spec.AvailabilityZones {
		if !strings.HasPrefix(az, claim.Spec.Region) {
			errs = append(errs, field.Invalid(spec.Child("availabilityZones").Index(i), az,
				fmt.Sprintf("not an availability zone of region %s", claim.Spec.Region)))
		}
	}

	scope, e := scopeFor(ctx, v.Client, spec.Child("scopeRef"), claim.Spec.ScopeRef)
	if e != nil {
		return warnings, invalidError("SubnetClaim", claim.Name, append(errs, e))
	}
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
	if claim.Spec.Mode == awsv1alpha1.ClaimModeCreate {
		account, _ := scope.Account(claim.Spec.Account)
		if account.WriteRoleARN == "" && scope.AccountHasReadRole(claim.Spec.Account) {
			errs = append(errs, field.Invalid(spec.Child("mode"), claim.Spec.Mode,
				fmt.Sprintf("account %s has no writeRoleARN in NetworkScope %q, so subnets cannot be created there; "+
					"set one, or use mode Allocate to only reserve CIDRs", claim.Spec.Account, scope.Name)))
		} else if !v.WritesEnabled {
			warnings = append(warnings,
				writesDisabledWarning("the CIDRs are reserved but no subnet is created")...)
		}
	}

	vpcWarnings, vpcErrs := v.validateAgainstVPC(ctx, claim, scope)
	warnings = append(warnings, vpcWarnings...)
	errs = append(errs, vpcErrs...)

	return warnings, invalidError("SubnetClaim", claim.Name, errs)
}

// validateAgainstVPC checks the claim against the discovered VPC: that the requested prefix
// fits inside it at all, and that there is still room for the subnets that are missing.
// A VPC that has not been discovered yet is a warning, not an error: the next resync may
// well bring it, and a claim written next to its scope must not depend on the apply order.
func (v *SubnetClaimValidator) validateAgainstVPC(ctx context.Context, claim *awsv1alpha1.SubnetClaim,
	scope *awsv1alpha1.NetworkScope) (admission.Warnings, field.ErrorList) {
	spec := field.NewPath("spec")

	vpc := &awsv1alpha1.VPC{}
	if err := v.Client.Get(ctx, client.ObjectKey{Name: claim.Spec.VPCID}, vpc); err != nil {
		if apierrors.IsNotFound(err) {
			return admission.Warnings{fmt.Sprintf(
				"VPC %s is not in the inventory of NetworkScope %q (yet); the claim stays pending until it is discovered",
				claim.Spec.VPCID, scope.Name)}, nil
		}
		return admission.Warnings{"the VPC could not be read, so the claim was not checked against it: " + err.Error()}, nil
	}
	if vpc.Spec.Account != claim.Spec.Account || vpc.Spec.Region != claim.Spec.Region {
		return nil, field.ErrorList{field.Invalid(spec.Child("vpcID"), claim.Spec.VPCID,
			fmt.Sprintf("VPC %s is in %s/%s, not in %s/%s", claim.Spec.VPCID,
				vpc.Spec.Account, vpc.Spec.Region, claim.Spec.Account, claim.Spec.Region))}
	}

	if len(vpc.Status.CIDRBlocks) == 0 {
		return admission.Warnings{fmt.Sprintf("VPC %s has no CIDR blocks in the inventory yet", claim.Spec.VPCID)}, nil
	}
	if largest, ok := largestPrefix(vpc.Status.CIDRBlocks); ok && int(claim.Spec.PrefixLength) < largest {
		return nil, field.ErrorList{field.Invalid(spec.Child("prefixLength"), claim.Spec.PrefixLength,
			fmt.Sprintf("a /%d does not fit in VPC %s, whose largest block is a /%d",
				claim.Spec.PrefixLength, claim.Spec.VPCID, largest))}
	}

	needed := 0
	for _, az := range claim.Spec.AvailabilityZones {
		if !slices.ContainsFunc(claim.Status.Allocations, func(a awsv1alpha1.SubnetAllocation) bool {
			return a.AvailabilityZone == az && a.CIDRBlock != ""
		}) {
			needed++
		}
	}
	if needed == 0 {
		return nil, nil
	}

	pool, err := v.poolFor(ctx, claim, vpc)
	if err != nil {
		// The inventory could not be read. The controller checks the same thing again, so
		// refusing the claim over our own failure would help nobody.
		return admission.Warnings{"the free space in the VPC could not be checked: " + err.Error()}, nil
	}
	if _, err := allocator.Allocate(pool, int(claim.Spec.PrefixLength), needed); err != nil {
		if errors.Is(err, allocator.ErrNoSpace) {
			return nil, field.ErrorList{field.Invalid(spec.Child("prefixLength"), claim.Spec.PrefixLength,
				fmt.Sprintf("VPC %s has no room for %d more /%d: %v",
					claim.Spec.VPCID, needed, claim.Spec.PrefixLength, err))}
		}
		return admission.Warnings{"the free space in the VPC could not be checked: " + err.Error()}, nil
	}
	return nil, nil
}

// poolFor is what is taken in the VPC already: the discovered subnets plus the reservations
// of every other claim. It is the same pool the controller allocates from, read through the
// manager's cache, so a claim that is refused here would have failed there too.
func (v *SubnetClaimValidator) poolFor(ctx context.Context, claim *awsv1alpha1.SubnetClaim,
	vpc *awsv1alpha1.VPC) (allocator.Pool, error) {
	pool := allocator.Pool{CIDRs: vpc.Status.CIDRBlocks}

	subnets := &awsv1alpha1.SubnetList{}
	if err := v.Client.List(ctx, subnets, client.MatchingLabels{awsv1alpha1.LabelVPC: claim.Spec.VPCID}); err != nil {
		return allocator.Pool{}, err
	}
	for i := range subnets.Items {
		pool.Used = append(pool.Used, subnets.Items[i].Status.CIDRBlock)
	}

	claims := &awsv1alpha1.SubnetClaimList{}
	if err := v.Client.List(ctx, claims); err != nil {
		return allocator.Pool{}, err
	}
	for i := range claims.Items {
		other := &claims.Items[i]
		if other.Spec.VPCID != claim.Spec.VPCID {
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

// droppedZoneWarnings says out loud what dropping an availability zone means: the subnet
// that was created for it stays in AWS, because the operator never deletes one.
func droppedZoneWarnings(oldObj, newObj *awsv1alpha1.SubnetClaim) admission.Warnings {
	var warnings admission.Warnings
	for _, a := range oldObj.Status.Allocations {
		if a.SubnetID == "" || slices.Contains(newObj.Spec.AvailabilityZones, a.AvailabilityZone) {
			continue
		}
		warnings = append(warnings, fmt.Sprintf(
			"availability zone %s was dropped: subnet %s (%s) stays in AWS, the operator never deletes one",
			a.AvailabilityZone, a.SubnetID, a.CIDRBlock))
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
