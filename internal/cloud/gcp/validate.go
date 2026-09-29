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

package gcp

import (
	"fmt"
	"maps"
	"net/netip"
	"regexp"
	"slices"

	"k8s.io/apimachinery/pkg/util/validation/field"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
)

// What a GCP scope, claim and import must look like beyond the schema. The webhooks refuse
// what fails here at apply time; the controllers refuse the same claims and imports in their
// status, for objects created while the webhooks were not running.

// regionPattern is the shape of a GCP region name: europe-west1, us-central1,
// northamerica-northeast2. As on AWS only the shape is checked, so a new region needs no
// release; an AWS name such as eu-central-1 is caught.
var regionPattern = regexp.MustCompile(`^[a-z]+(-[a-z]+)+[0-9]+$`)

// projectPattern is the shape of a project ID; the CRD checks the same.
var projectPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)

// shortNamePattern is what the operator accepts as the short name of a Resource Manager tag
// key or value: letters and digits at both ends, "-", "_" and "." in between. Google has at
// times allowed more in values; this is the rule both keys and values have always met.
var shortNamePattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9._-]*[A-Za-z0-9])?$`)

// maxShortNameLength is the longest short name the operator accepts for a tag key or value.
const maxShortNameLength = 63

// Subnetwork sizes: Compute's smallest primary range is a /29; the operator allocates up to a
// /8, which is already more than any pool a claim is likely to name.
const (
	minPrefixLength = 8
	maxPrefixLength = 29
)

func validateRegion(path *field.Path, region string) *field.Error {
	if regionPattern.MatchString(region) {
		return nil
	}
	return field.Invalid(path, region, "not a GCP region name, e.g. europe-west1")
}

// ValidateScope implements provider.Provider: region and project names, service accounts, and
// a tag parent.
func (p *Provider) ValidateScope(scope *networkv1.NetworkScope) ([]string, field.ErrorList) {
	spec := field.NewPath("spec")
	var warnings []string
	var errs field.ErrorList
	for i, region := range scope.Spec.Regions {
		if e := validateRegion(spec.Child("regions").Index(i), region); e != nil {
			errs = append(errs, e)
		}
	}
	for i, account := range scope.Spec.Accounts {
		path := spec.Child("accounts").Index(i)
		if !projectPattern.MatchString(account.ID) {
			errs = append(errs, field.Invalid(path.Child("id"), account.ID,
				"a GCP account id is a project ID: 6 to 30 lowercase letters, digits and hyphens, starting with a letter"))
		}
		for j, region := range account.Regions {
			if e := validateRegion(path.Child("regions").Index(j), region); e != nil {
				errs = append(errs, e)
			}
		}
		w, e := checkAccountIdentity(path, account)
		warnings, errs = append(warnings, w...), append(errs, e...)
	}
	if scope.Spec.GCP == nil {
		errs = append(errs, field.Required(spec.Child("gcp").Child("tagParent"),
			"a GCP scope names the organization or project that owns its tag keys"))
	}
	warnings = append(warnings, claimTagWarnings(scope.Spec.GCP)...)
	return warnings, errs
}

// claimTagWarnings says what gcp.claimTag Bind costs: a tag value per claim, which either has
// to be created before each claim, or is created by the operator and never deleted.
func claimTagWarnings(gcp *networkv1.GCPScope) []string {
	switch {
	case !gcp.BindsClaimTag():
		return nil
	case gcp.CreateTagValues:
		return []string{"spec.gcp.claimTag is Bind with createTagValues: every Create claim creates its own " +
			"hs-claim value, which the operator never deletes, and a tag key holds at most 1,000 values " +
			"(TagValueLimitReached); prefer claimTag Skip unless something outside Kubernetes needs the claim tag"}
	default:
		return []string{"spec.gcp.claimTag is Bind without createTagValues: every Create claim fails with " +
			"TagValueMissing until its value <tagParent>/hs-claim/<namespace>_<name> has been created"}
	}
}

// ValidateClaim implements provider.Provider: the project, region and network ID formats, no
// zones (a subnetwork is regional), a prefix length Compute accepts, the pool the subnetwork
// is carved from, a subnetwork name Compute accepts, and owner, env and tier values that can
// be tag values.
func (p *Provider) ValidateClaim(claim *networkv1.SubnetClaim) field.ErrorList {
	spec := field.NewPath("spec")
	var errs field.ErrorList
	if !projectPattern.MatchString(claim.Spec.Account) {
		errs = append(errs, field.Invalid(spec.Child("account"), claim.Spec.Account,
			"a GCP account is a project ID: 6 to 30 lowercase letters, digits and hyphens, starting with a letter"))
	}
	if e := validateRegion(spec.Child("region"), claim.Spec.Region); e != nil {
		errs = append(errs, e)
	}
	if ref, ok := parseResourceID(claim.Spec.NetworkID); !ok || ref.subnet() {
		errs = append(errs, field.Invalid(spec.Child("networkID"), claim.Spec.NetworkID,
			"not a GCP network ID, e.g. projects/<project>/global/networks/<name>"))
	} else if ref.project != claim.Spec.Account {
		errs = append(errs, field.Invalid(spec.Child("networkID"), claim.Spec.NetworkID,
			fmt.Sprintf("the network is in project %s, not in the claim's account %s", ref.project, claim.Spec.Account)))
	}
	if len(claim.Spec.Zones) > 0 {
		errs = append(errs, field.Forbidden(spec.Child("zones"),
			"GCP subnetworks are regional: a claim on GCP lists no zones and is for one subnetwork"))
	}
	if claim.Spec.PrefixLength < minPrefixLength || claim.Spec.PrefixLength > maxPrefixLength {
		errs = append(errs, field.Invalid(spec.Child("prefixLength"), claim.Spec.PrefixLength,
			fmt.Sprintf("GCP subnetworks are between a /%d and a /%d here", minPrefixLength, maxPrefixLength)))
	}
	if claim.Spec.AWS != nil {
		errs = append(errs, field.Forbidden(spec.Child("aws"), "aws is only valid for provider AWS"))
	}
	errs = append(errs, validatePools(spec.Child("gcp").Child("poolCIDRs"), claim)...)
	if name := claim.NamePrefixOrName(); !resourceNamePattern.MatchString(name) {
		errs = append(errs, field.Invalid(spec.Child("namePrefix"), name,
			"on GCP namePrefix is the subnetwork's name: 1 to 63 lowercase letters, digits and hyphens, starting "+
				"with a letter and not ending with a hyphen"))
	}
	for _, v := range []struct {
		path  *field.Path
		value string
	}{{spec.Child("owner"), claim.Spec.Owner}, {spec.Child("env"), claim.Spec.Env}, {spec.Child("tier"), claim.Spec.Tier}} {
		if v.value != "" {
			errs = append(errs, checkShortName(v.path, "value", v.value)...)
		}
	}
	return errs
}

// validatePools checks the ranges a claim is carved from: at least one, each an IPv4 network
// address with its prefix, and one at least as large as the subnetwork.
func validatePools(path *field.Path, claim *networkv1.SubnetClaim) field.ErrorList {
	if claim.Spec.GCP == nil || len(claim.Spec.GCP.PoolCIDRs) == 0 {
		return field.ErrorList{field.Required(path,
			"a GCP VPC network has no address space of its own: name the IPv4 ranges the subnetwork is carved from")}
	}
	var errs field.ErrorList
	fits := false
	for i, cidr := range claim.Spec.GCP.PoolCIDRs {
		prefix, err := netip.ParsePrefix(cidr)
		switch {
		case err != nil || !prefix.Addr().Is4():
			errs = append(errs, field.Invalid(path.Index(i), cidr, "not an IPv4 CIDR, e.g. 10.20.0.0/16"))
		case prefix.Masked() != prefix:
			errs = append(errs, field.Invalid(path.Index(i), cidr,
				fmt.Sprintf("not a network address; did you mean %s?", prefix.Masked())))
		case prefix.Bits() <= int(claim.Spec.PrefixLength):
			fits = true
		}
	}
	if len(errs) == 0 && !fits {
		errs = append(errs, field.Invalid(path, claim.Spec.GCP.PoolCIDRs,
			fmt.Sprintf("no pool is large enough for a /%d", claim.Spec.PrefixLength)))
	}
	return errs
}

// ClaimRefusal implements provider.Provider: what ValidateClaim refuses, for a claim that got
// past a webhook that was not running.
func (p *Provider) ClaimRefusal(claim *networkv1.SubnetClaim) (string, string) {
	switch {
	case len(claim.Spec.Zones) > 0:
		return "ZonesNotSupported", "GCP subnetworks are regional: remove spec.zones"
	case claim.Spec.GCP == nil || len(claim.Spec.GCP.PoolCIDRs) == 0:
		return "PoolRequired", "a GCP VPC network has no address space of its own: set spec.gcp.poolCIDRs"
	}
	if errs := p.ValidateClaim(claim); len(errs) > 0 {
		return "InvalidClaim", errs.ToAggregate().Error()
	}
	return "", ""
}

// ValidateImport implements provider.Provider: the project and resource ID formats. A
// subnetwork is regional and names its region, which must be the import's; a network is
// global, and its import names no region or any region the scope covers.
func (p *Provider) ValidateImport(imp *networkv1.ResourceImport) field.ErrorList {
	spec := field.NewPath("spec")
	var errs field.ErrorList
	if !projectPattern.MatchString(imp.Spec.Account) {
		errs = append(errs, field.Invalid(spec.Child("account"), imp.Spec.Account,
			"a GCP account is a project ID: 6 to 30 lowercase letters, digits and hyphens, starting with a letter"))
	}
	if imp.Spec.Region != "" {
		if e := validateRegion(spec.Child("region"), imp.Spec.Region); e != nil {
			errs = append(errs, e)
		}
	}
	ref, ok := parseResourceID(imp.Spec.ResourceID)
	switch {
	case !ok:
		errs = append(errs, field.Invalid(spec.Child("resourceID"), imp.Spec.ResourceID,
			"not a GCP network or subnetwork ID, e.g. projects/<project>/global/networks/<name> or "+
				"projects/<project>/regions/<region>/subnetworks/<name>"))
	case ref.project != imp.Spec.Account:
		errs = append(errs, field.Invalid(spec.Child("resourceID"), imp.Spec.ResourceID,
			fmt.Sprintf("the resource is in project %s, not in the import's account %s", ref.project, imp.Spec.Account)))
	case ref.subnet() && ref.region != imp.Spec.Region:
		errs = append(errs, field.Invalid(spec.Child("region"), imp.Spec.Region,
			fmt.Sprintf("the subnetwork is in region %s", ref.region)))
	}
	return errs
}

// ImportRefusal implements provider.Provider.
func (p *Provider) ImportRefusal(imp *networkv1.ResourceImport) (string, string) {
	if errs := p.ValidateImport(imp); len(errs) > 0 {
		return "InvalidResourceID", errs.ToAggregate().Error()
	}
	return "", ""
}

// ValidateTags implements provider.Provider with the rules of Resource Manager tag short
// names: a key is a TagKey of the scope's tag parent and a value one of its TagValues, and
// neither can contain "/". That is why the GCP default keys are hs-owner, hs-env and hs-tier.
func (p *Provider) ValidateTags(path *field.Path, tags map[string]string) field.ErrorList {
	errs := make(field.ErrorList, 0, 2*len(tags))
	for _, key := range slices.Sorted(maps.Keys(tags)) {
		errs = append(errs, checkShortName(path.Key(key), "key", key)...)
		errs = append(errs, checkShortName(path.Key(key), "value", tags[key])...)
	}
	return errs
}

// checkShortName checks one tag key or value short name.
func checkShortName(p *field.Path, what, s string) field.ErrorList {
	switch {
	case s == "":
		return field.ErrorList{field.Invalid(p, s, fmt.Sprintf("a tag %s must not be empty on GCP", what))}
	case len(s) > maxShortNameLength:
		return field.ErrorList{field.Invalid(p, s, fmt.Sprintf("a GCP tag %s is at most %d characters",
			what, maxShortNameLength))}
	case !shortNamePattern.MatchString(s):
		return field.ErrorList{field.Invalid(p, s, fmt.Sprintf(`a GCP tag %s is letters and digits, with "-", "_" `+
			`and "." between them; "/" is not allowed`, what))}
	}
	return nil
}
