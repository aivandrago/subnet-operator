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

package azure

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
)

// What an Azure scope, claim and import must look like beyond the schema. The webhooks refuse
// what fails here at apply time; the controllers refuse the same claims and imports in their
// status, for objects created while the webhooks were not running.
//
// The case of a subscription ID or a resource ID is never a reason to refuse: ARM compares both
// without regard to case and the portal shows resource IDs with capitals, so the webhooks and
// the controllers bring them into the operator's spelling (SubnetClaim.CanonicalizeIDs,
// ResourceImport.CanonicalizeIDs) and what is checked here is the shape and the subscription.

// locationPattern is the shape of an Azure location name as ARM spells it: westeurope,
// eastus2, germanywestcentral. As on AWS and GCP only the shape is checked, so a new region
// needs no release; an AWS or GCP name such as eu-central-1 or europe-west1 is caught.
var locationPattern = regexp.MustCompile(`^[a-z][a-z0-9]*$`)

// subscriptionPattern is the shape of a subscription ID; the CRD checks the same. Either case:
// ARM compares subscription IDs without regard to case, and the operator uses them in
// lowercase (networkv1.CanonicalAccountID) wherever the account is a key (TargetKey, object
// specs, labels, metrics, status).
var subscriptionPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Azure tag limits: a name is at most 512 characters and cannot contain any of
// invalidNameChars, a value is at most 256 characters.
const (
	maxTagNameLength  = 512
	maxTagValueLength = 256
	invalidNameChars  = `<>%&\?/`
)

// The prefix lengths Azure accepts for an IPv4 subnet.
const (
	minPrefixLength = 2
	maxPrefixLength = 29
)

func validateLocation(path *field.Path, location string) *field.Error {
	if locationPattern.MatchString(location) {
		return nil
	}
	return field.Invalid(path, location, "not an Azure location name, e.g. westeurope")
}

// ValidateScope implements provider.Provider: location names, subscription IDs and the
// accounts' identities (checkAccountIdentity). Subscription IDs,
// resource groups and tag names are compared without regard to case on Azure, so the lists v1
// declares as sets (accounts, autoImport.accountDefaults, azure.resourceGroups) may not hold
// two entries that differ only in case, and the tag names the scope reads may not either: both
// would name the same thing.
func (p *Provider) ValidateScope(scope *networkv1.NetworkScope) ([]string, field.ErrorList) {
	spec := field.NewPath("spec")
	var warnings []string
	var errs field.ErrorList
	for i, region := range scope.Spec.Regions {
		if e := validateLocation(spec.Child("regions").Index(i), region); e != nil {
			errs = append(errs, e)
		}
	}
	for i, account := range scope.Spec.Accounts {
		path := spec.Child("accounts").Index(i)
		if !subscriptionPattern.MatchString(account.ID) {
			errs = append(errs, field.Invalid(path.Child("id"), account.ID,
				"an Azure account id is a subscription ID: a UUID"))
		}
		for j, region := range account.Regions {
			if e := validateLocation(path.Child("regions").Index(j), region); e != nil {
				errs = append(errs, e)
			}
		}
		w, e := checkAccountIdentity(path, account)
		warnings, errs = append(warnings, w...), append(errs, e...)
	}
	errs = append(errs, foldDuplicates(spec.Child("accounts"), len(scope.Spec.Accounts),
		func(i int) (string, *field.Path) { return scope.Spec.Accounts[i].ID, field.NewPath("id") })...)
	if a := scope.Spec.AutoImport; a != nil {
		errs = append(errs, foldDuplicates(spec.Child("autoImport", "accountDefaults"), len(a.AccountDefaults),
			func(i int) (string, *field.Path) { return a.AccountDefaults[i].Account, field.NewPath("account") })...)
	}
	if az := scope.Spec.Azure; az != nil {
		errs = append(errs, foldDuplicates(spec.Child("azure", "resourceGroups"), len(az.ResourceGroups),
			func(i int) (string, *field.Path) { return az.ResourceGroups[i], nil })...)
	}
	errs = append(errs, tagNameClashes(spec, scope)...)
	warnings = append(warnings, creatorRuleWarnings(scope)...)
	return warnings, errs
}

// creatorRuleWarnings says which auto-import rules of the scope can never match on Azure: the
// ones that go by who created a resource. An Event Grid resource event does not say whether a
// write created the resource or updated it (package events), so the operator records no
// creator on Azure, and such a rule is dead weight that reads as if it worked. It is a
// warning, not a refusal: the rest of the policy (accountDefaults, inheritFromNetwork, skip
// rules by tag) still applies, and a policy shared between scopes of several providers must
// still be accepted.
func creatorRuleWarnings(scope *networkv1.NetworkScope) []string {
	policy := scope.Spec.AutoImport
	if policy == nil {
		return nil
	}
	const why = "never match on Azure: its change events do not say who created a resource, so the operator " +
		"records no creator there; use accountDefaults, inheritFromNetwork or skip rules by tag"
	var warnings []string
	if n := len(policy.FromCreator); n > 0 {
		warnings = append(warnings, fmt.Sprintf("spec.autoImport.fromCreator: the %d creator rule(s) %s", n, why))
	}
	byPrincipal := 0
	for _, rule := range policy.Skip {
		if rule.PrincipalPrefix != "" {
			byPrincipal++
		}
	}
	if byPrincipal > 0 {
		warnings = append(warnings, fmt.Sprintf("spec.autoImport.skip: the %d rule(s) with a principalPrefix %s",
			byPrincipal, why))
	}
	return warnings
}

// foldDuplicates refuses an entry of a list that equals an earlier one without regard to case.
// value returns entry i and the path of the value inside it (nil for the entry itself).
func foldDuplicates(list *field.Path, n int, value func(i int) (string, *field.Path)) field.ErrorList {
	var errs field.ErrorList
	seen := map[string]string{}
	for i := range n {
		v, sub := value(i)
		path := list.Index(i)
		if sub != nil {
			path = path.Child(sub.String())
		}
		if first, ok := seen[strings.ToLower(v)]; ok {
			errs = append(errs, field.Duplicate(path, fmt.Sprintf(
				"%s and %s are the same on Azure, which compares them without regard to case", first, v)))
			continue
		}
		seen[strings.ToLower(v)] = v
	}
	return errs
}

// tagNameClashes refuses two tag names the scope reads that differ only in case: Azure holds
// one tag for both, and discovery could report it under only one of them.
func tagNameClashes(spec *field.Path, scope *networkv1.NetworkScope) field.ErrorList {
	var errs field.ErrorList
	seen := map[string]string{}
	for _, name := range scope.TagNames() {
		lower := strings.ToLower(name)
		if first, ok := seen[lower]; ok {
			errs = append(errs, field.Invalid(spec, name, fmt.Sprintf(
				"the scope names the tag %s and %s, which are the same Azure tag name (Azure compares tag names "+
					"without regard to case); spell it one way in tagKeys, networkSelector, requiredSubnetTags and "+
					"autoImport", first, name)))
			continue
		}
		seen[lower] = name
	}
	return errs
}

// ValidateClaim implements provider.Provider: the subscription, location and virtual network
// ID (in any case, and in the claim's subscription), no zones
// (an Azure subnet is regional), a prefix length Azure accepts, no other provider's options, a
// subnet name Azure accepts, and an ownership entry that fits one tag value. The entry is
// checked with every tag the claim writes; at the write, pairs its virtual network carries with
// the same value are left out, so the one written may be shorter, never longer.
func (p *Provider) ValidateClaim(claim *networkv1.SubnetClaim) field.ErrorList {
	spec := field.NewPath("spec")
	var errs field.ErrorList
	if !subscriptionPattern.MatchString(claim.Spec.Account) {
		errs = append(errs, field.Invalid(spec.Child("account"), claim.Spec.Account,
			"an Azure account is a subscription ID: a UUID"))
	}
	if e := validateLocation(spec.Child("region"), claim.Spec.Region); e != nil {
		errs = append(errs, e)
	}
	errs = append(errs, validateResourceID(spec.Child("networkID"), claim.Spec.NetworkID, claim.Spec.Account, false)...)
	if len(claim.Spec.Zones) > 0 {
		errs = append(errs, field.Forbidden(spec.Child("zones"),
			"Azure subnets are regional: a claim on Azure lists no zones and is for one subnet"))
	}
	if claim.Spec.PrefixLength < minPrefixLength || claim.Spec.PrefixLength > maxPrefixLength {
		errs = append(errs, field.Invalid(spec.Child("prefixLength"), claim.Spec.PrefixLength,
			fmt.Sprintf("Azure subnets are between a /%d and a /%d", minPrefixLength, maxPrefixLength)))
	}
	if claim.Spec.AWS != nil {
		errs = append(errs, field.Forbidden(spec.Child("aws"), "aws is only valid for provider AWS"))
	}
	if claim.Spec.GCP != nil {
		errs = append(errs, field.Forbidden(spec.Child("gcp"), "gcp is only valid for provider GCP; an Azure "+
			"subnet is carved from its virtual network's address space"))
	}
	if name := claim.NamePrefixOrName(); validateSubnetName(name) != "" {
		errs = append(errs, field.Invalid(spec.Child("namePrefix"), name,
			"on Azure namePrefix is the subnet's name: "+validateSubnetName(name)))
	}
	for _, v := range []struct {
		path  *field.Path
		value string
	}{{spec.Child("owner"), claim.Spec.Owner}, {spec.Child("env"), claim.Spec.Env}, {spec.Child("tier"), claim.Spec.Tier}} {
		if len(v.value) > maxTagValueLength {
			errs = append(errs, field.Invalid(v.path, v.value,
				fmt.Sprintf("an Azure tag value is at most %d characters", maxTagValueLength)))
		}
	}
	if n := len(EncodeSubnetEntry(ClaimTags(claim))); n > maxTagValueLength {
		errs = append(errs, field.Invalid(spec.Child("tags"), claim.Spec.Tags, fmt.Sprintf(
			"an Azure subnet's ownership is one tag value of at most %d characters on its virtual network, and "+
				"this claim's tags (owner, env, tier, hs-managed-by, hs-claim and spec.tags) take %d; use shorter "+
				"names and values or fewer tags", maxTagValueLength, n)))
	}
	return errs
}

// ClaimTags are the tags a claim's subnet is created with on Azure: the claim's own, with the
// operator's keys on top, as the claim controller builds them for a subnet without a zone. The
// claim tag is always written on Azure, as on AWS: a tag value costs nothing to create there.
func ClaimTags(claim *networkv1.SubnetClaim) map[string]string {
	k := networkv1.OperatorTagKeysFor(networkv1.ProviderAzure)
	tags := maps.Clone(claim.Spec.Tags)
	if tags == nil {
		tags = map[string]string{}
	}
	tags[k.Owner] = claim.Spec.Owner
	if claim.Spec.Env != "" {
		tags[k.Env] = claim.Spec.Env
	}
	if claim.Spec.Tier != "" {
		tags[k.Tier] = claim.Spec.Tier
	}
	tags[k.ManagedBy] = networkv1.TagManagedByValue
	tags[k.Claim] = k.ClaimValue(claim.Namespace, claim.Name)
	return tags
}

// validateResourceID checks a virtual network ID, or with subnets allowed a subnet ID too: its
// shape and its subscription, both without regard to case. The examples in the message are in
// the portal's spelling, which is what somebody is most likely to paste.
func validateResourceID(path *field.Path, id, account string, subnets bool) field.ErrorList {
	ref, ok := parseResourceID(id)
	switch {
	case !ok || (ref.subnet != "" && !subnets):
		what := "an Azure virtual network ID, e.g. /subscriptions/<subscription>/resourceGroups/<group>/providers/" +
			"Microsoft.Network/virtualNetworks/<name>"
		if subnets {
			what = "an Azure virtual network or subnet ID, e.g. /subscriptions/<subscription>/resourceGroups/<group>/" +
				"providers/Microsoft.Network/virtualNetworks/<name>[/subnets/<name>]"
		}
		return field.ErrorList{field.Invalid(path, id, "not "+what+" (in any case)")}
	case !strings.EqualFold(ref.subscription, account):
		return field.ErrorList{field.Invalid(path, id, fmt.Sprintf("the resource is in subscription %s, not in %s",
			ref.subscription, account))}
	}
	return nil
}

// ClaimRefusal implements provider.Provider: what ValidateClaim refuses, for a claim that got
// past a webhook that was not running.
func (p *Provider) ClaimRefusal(claim *networkv1.SubnetClaim) (string, string) {
	if len(claim.Spec.Zones) > 0 {
		return "ZonesNotSupported", "Azure subnets are regional: remove spec.zones"
	}
	if errs := p.ValidateClaim(claim); len(errs) > 0 {
		return refusalReason(errs, "InvalidClaim"), errs.ToAggregate().Error()
	}
	return "", ""
}

// ValidateImport implements provider.Provider: the subscription, the location, the resource ID
// (a virtual network or a subnet, in any case, in the import's subscription) and, for a
// subnet, an ownership entry that can hold the import's tags. The import's tags are added to
// what the subnet's entry already holds, which only the write can see.
//
// The location is required, as the region is on AWS: every virtual network is in one, and an
// import without it would be labelled and audited as if it were nowhere. Nobody has to type
// it for a virtual network the operator has discovered: the mutating webhook fills it in from
// the inventory before this runs (provider.ImportLocator).
func (p *Provider) ValidateImport(imp *networkv1.ResourceImport) field.ErrorList {
	return validateImport(imp, true)
}

// validateImport is ValidateImport, with or without requiring the location: the controller
// reads the virtual network before it writes and takes the location from there, so an import
// that got past the webhooks without one is not refused for it (ImportRefusal).
func validateImport(imp *networkv1.ResourceImport, locationRequired bool) field.ErrorList {
	spec := field.NewPath("spec")
	var errs field.ErrorList
	if !subscriptionPattern.MatchString(imp.Spec.Account) {
		errs = append(errs, field.Invalid(spec.Child("account"), imp.Spec.Account,
			"an Azure account is a subscription ID: a UUID"))
	}
	switch {
	case imp.Spec.Region != "":
		if e := validateLocation(spec.Child("region"), imp.Spec.Region); e != nil {
			errs = append(errs, e)
		}
	case locationRequired:
		errs = append(errs, field.Required(spec.Child("region"), "every Azure virtual network and subnet is in a "+
			"location: set it to the virtual network's, e.g. westeurope (the mutating webhook fills it in for a "+
			"virtual network that is in the inventory)"))
	}
	errs = append(errs, validateResourceID(spec.Child("resourceID"), imp.Spec.ResourceID, imp.Spec.Account, true)...)
	if ref, ok := parseResourceID(imp.Spec.ResourceID); ok && ref.subnet != "" {
		if n := len(EncodeSubnetEntry(imp.Spec.Tags)); n > maxTagValueLength {
			errs = append(errs, field.Invalid(spec.Child("tags"), imp.Spec.Tags, fmt.Sprintf(
				"an Azure subnet's ownership is one tag value of at most %d characters on its virtual network, "+
					"and these tags take %d", maxTagValueLength, n)))
		}
	}
	return errs
}

// ImportRefusal implements provider.Provider: what ValidateImport refuses, less a missing
// location, which the controller takes from the virtual network it reads (LocateImport).
func (p *Provider) ImportRefusal(imp *networkv1.ResourceImport) (string, string) {
	if errs := validateImport(imp, false); len(errs) > 0 {
		return refusalReason(errs, "InvalidResourceID"), errs.ToAggregate().Error()
	}
	return "", ""
}

// refusalReason is OwnershipEntryTooLong when the only thing wrong is that the tags do not fit
// in a subnet's entry, the fallback otherwise.
func refusalReason(errs field.ErrorList, fallback string) string {
	for _, e := range errs {
		if e.Field != "spec.tags" {
			return fallback
		}
	}
	return "OwnershipEntryTooLong"
}

// ValidateTags implements provider.Provider with Azure's tag rules: a name of 1 to 512
// characters without <, >, %, &, \, ? or / (which is why the Azure default keys are hs-owner,
// hs-env and hs-tier), a value of at most 256 characters, and no two names that differ only in
// case, since Azure treats names case-insensitively. A name starting with the subnet entry
// prefix (hs-subnet-) is the operator's record of a subnet's ownership and cannot be set as a
// tag of its own.
func (p *Provider) ValidateTags(path *field.Path, tags map[string]string) field.ErrorList {
	var errs field.ErrorList
	entryPrefix := networkv1.OperatorTagKeysFor(networkv1.ProviderAzure).SubnetEntryPrefix
	seen := map[string]string{}
	for _, key := range slices.Sorted(maps.Keys(tags)) {
		keyPath := path.Key(key)
		lower := strings.ToLower(key)
		switch {
		case key == "":
			errs = append(errs, field.Invalid(keyPath, key, "tag name must not be empty"))
		case len(key) > maxTagNameLength:
			errs = append(errs, field.Invalid(keyPath, key,
				fmt.Sprintf("an Azure tag name is at most %d characters", maxTagNameLength)))
		case strings.ContainsAny(key, invalidNameChars):
			errs = append(errs, field.Invalid(keyPath, key,
				`an Azure tag name cannot contain <, >, %, &, \, ? or /`))
		case strings.HasPrefix(lower, entryPrefix):
			errs = append(errs, field.Invalid(keyPath, key, fmt.Sprintf(
				"tag names starting with %q hold the ownership of subnets on Azure and are written by the operator only",
				entryPrefix)))
		case seen[lower] != "":
			errs = append(errs, field.Duplicate(keyPath, fmt.Sprintf(
				"%s and %s are the same Azure tag name, which is case-insensitive", seen[lower], key)))
		}
		seen[lower] = key
		if len(tags[key]) > maxTagValueLength {
			errs = append(errs, field.Invalid(keyPath, tags[key],
				fmt.Sprintf("an Azure tag value is at most %d characters", maxTagValueLength)))
		}
	}
	return errs
}
