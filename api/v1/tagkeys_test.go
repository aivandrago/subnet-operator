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
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

// The keys every release before 2.0 wrote on AWS. They must never change: resources tagged
// with them would silently drop out of the inventory.
func TestAWSOperatorTagKeysAreTheOnesBefore20(t *testing.T) {
	for _, p := range []Provider{ProviderAWS, "", "SomeFutureCloud"} {
		k := OperatorTagKeysFor(p)
		want := OperatorTagKeys{Owner: "hs/owner", Env: "hs/env", Tier: "hs/tier", Managed: "hs/managed",
			ManagedBy: "hs/managed-by", Claim: "hs/claim", claimSeparator: "/"}
		if k != want {
			t.Errorf("OperatorTagKeysFor(%q) = %+v, want %+v", p, k, want)
		}
		if got := k.ClaimValue("team-a", "db"); got != "team-a/db" {
			t.Errorf("claim value on %q = %q, want team-a/db", p, got)
		}
	}
	if got := DefaultTagKeys(ProviderAWS); got != (TagKeys{Owner: "hs/owner", Env: "hs/env", Tier: "hs/tier"}) {
		t.Errorf("DefaultTagKeys(AWS) = %+v", got)
	}
}

func TestGCPOperatorTagKeysAreValidTagKeyShortNames(t *testing.T) {
	k := OperatorTagKeysFor(ProviderGCP)
	for _, key := range []string{k.Owner, k.Env, k.Tier, k.Managed, k.ManagedBy, k.Claim} {
		if !strings.HasPrefix(key, "hs-") || strings.ContainsAny(key, "/") {
			t.Errorf("GCP key %q is not an hs-* short name", key)
		}
	}
	if got := k.ClaimValue("team-a", "db.v2"); got != "team-a_db.v2" {
		t.Errorf("GCP claim value = %q, want team-a_db.v2", got)
	}
	if got := (resolvedTagKeysOf(ProviderGCP)); got.Owner != "hs-owner" || got.Env != "hs-env" || got.Tier != "hs-tier" {
		t.Errorf("a GCP scope without tagKeys resolves to %+v", got)
	}
}

// Azure tag names cannot contain "/", so the keys are the GCP ones; a tag value can, so the claim
// value is namespace/name as on AWS. Subnets carry no tags: their ownership is an entry on the
// virtual network, named with the subnet entry prefix.
func TestAzureOperatorTagKeys(t *testing.T) {
	k := OperatorTagKeysFor(ProviderAzure)
	gcp := OperatorTagKeysFor(ProviderGCP)
	if k.TagKeys() != gcp.TagKeys() || k.Managed != gcp.Managed || k.ManagedBy != gcp.ManagedBy || k.Claim != gcp.Claim {
		t.Errorf("Azure keys %+v are not the hs-* keys of GCP %+v", k, gcp)
	}
	for _, key := range []string{k.Owner, k.Env, k.Tier, k.Managed, k.ManagedBy, k.Claim, k.SubnetEntryPrefix} {
		if strings.ContainsAny(key, `<>%&\?/`) {
			t.Errorf("Azure key %q has a character Azure tag names cannot", key)
		}
	}
	if k.SubnetEntryPrefix != "hs-subnet-" || gcp.SubnetEntryPrefix != "" || OperatorTagKeysFor(ProviderAWS).SubnetEntryPrefix != "" {
		t.Errorf("subnet entry prefixes: Azure %q, GCP %q; only Azure keeps subnet ownership on the network",
			k.SubnetEntryPrefix, gcp.SubnetEntryPrefix)
	}
	if got := k.ClaimValue("team-a", "db.v2"); got != "team-a/db.v2" {
		t.Errorf("Azure claim value = %q, want team-a/db.v2", got)
	}
	long := k.ClaimValue(strings.Repeat("n", 63), strings.Repeat("c", 253))
	if len(long) > 256 || long != k.ClaimValue(strings.Repeat("n", 63), strings.Repeat("c", 253)) {
		t.Errorf("a long Azure claim value has %d characters, want at most 256 and stable", len(long))
	}
	if got := resolvedTagKeysOf(ProviderAzure); got != (TagKeys{Owner: "hs-owner", Env: "hs-env", Tier: "hs-tier"}) {
		t.Errorf("an Azure scope without tagKeys resolves to %+v", got)
	}
}

// A GCP tag value is at most 63 characters: a longer namespace_name keeps a prefix and ends in
// ten hex digits of its hash, so it stays unique, valid and the same on every call.
func TestGCPClaimValueStaysAValidUniqueTagValue(t *testing.T) {
	k := OperatorTagKeysFor(ProviderGCP)
	ns := "team-payments-platform"
	long := strings.Repeat("a", 30) + ".claim-" + strings.Repeat("b", 20)
	v := k.ClaimValue(ns, long)
	if len(v) > 63 || len(v) < 50 {
		t.Fatalf("claim value %q has %d characters, want at most 63 and most of them used", v, len(v))
	}
	if !strings.HasPrefix(v, ns+"_") {
		t.Errorf("claim value %q does not start with the namespace", v)
	}
	if v != k.ClaimValue(ns, long) {
		t.Error("the claim value is not stable")
	}
	if other := k.ClaimValue(ns, long+"c"); other == v {
		t.Errorf("two claims share the value %q", v)
	}
	// A cut that ends in a "." or "-" must not leave it before the hash.
	for i := range 20 {
		name := strings.Repeat("x", 33+i) + strings.Repeat(".-", 20)
		got := k.ClaimValue("ns", name)
		cut := got[:len(got)-11]
		if last := cut[len(cut)-1]; last == '.' || last == '-' || last == '_' {
			t.Errorf("claim value %q has %q before its hash", got, last)
		}
	}
	// AWS values have no such limit.
	aws := OperatorTagKeysFor(ProviderAWS)
	if got := aws.ClaimValue(ns, long); got != ns+"/"+long {
		t.Errorf("AWS claim value = %q, want the whole namespace/name", got)
	}
}

// resolvedTagKeysOf is what a scope of the provider without spec.tagKeys resolves to.
func resolvedTagKeysOf(p Provider) TagKeys {
	s := &NetworkScope{Spec: NetworkScopeSpec{Provider: p}}
	return s.ResolvedTagKeys()
}

func TestObjectNameKeepsAWSIDsAndHashesTheRest(t *testing.T) {
	for _, id := range []string{"vpc-0abc123", "subnet-0123456789abcdef0"} {
		if got := ObjectName(id); got != id {
			t.Errorf("ObjectName(%q) = %q, want the ID itself", id, got)
		}
	}
	a := ObjectName("projects/alpha-1/regions/europe-west1/subnetworks/Apps_Subnet")
	b := ObjectName("projects/beta-22/regions/europe-west1/subnetworks/Apps_Subnet")
	if a == b {
		t.Errorf("two subnetworks of the same name in different projects share the object name %q", a)
	}
	if !strings.HasPrefix(a, "apps-subnet-") || len(a) != len("apps-subnet-")+objectNameHashLength {
		t.Errorf("ObjectName = %q, want apps-subnet-<10 hex digits>", a)
	}
	if again := ObjectName("projects/alpha-1/regions/europe-west1/subnetworks/Apps_Subnet"); again != a {
		t.Errorf("ObjectName is not stable: %q, then %q", a, again)
	}
	long := "projects/p/global/networks/" + strings.Repeat("n", 63)
	for _, id := range []string{long, "projects/p/global/networks/---", a + "/"} {
		name := ObjectName(id)
		if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
			t.Errorf("ObjectName(%q) = %q is not a valid name and label value: %v", id, name, errs)
		}
		if errs := validation.IsValidLabelValue(name); len(errs) > 0 {
			t.Errorf("ObjectName(%q) = %q is not a valid label value: %v", id, name, errs)
		}
	}
}

// Azure compares subscription IDs and tag names without regard to case; the operator keys an
// Azure account by its lowercase ID, and every other provider's IDs and names as they are.
func TestAzureAccountsAndTagNamesIgnoreCase(t *testing.T) {
	const sub = "5B3C2A10-0000-4000-8000-00000000A2E1"
	if got := CanonicalAccountID(ProviderAzure, sub); got != strings.ToLower(sub) {
		t.Errorf("CanonicalAccountID(Azure) = %q", got)
	}
	if got := CanonicalAccountID(ProviderGCP, "My-Project"); got != "My-Project" {
		t.Errorf("CanonicalAccountID(GCP) = %q, want it unchanged", got)
	}
	scope := &NetworkScope{Spec: NetworkScopeSpec{Provider: ProviderAzure, Accounts: []Account{{ID: sub}},
		Regions: []string{"westeurope"}}}
	if !scope.Covers(strings.ToLower(sub), "westeurope") {
		t.Error("an Azure scope does not cover its subscription spelled in lowercase")
	}
	aws := &NetworkScope{Spec: NetworkScopeSpec{Provider: ProviderAWS, Accounts: []Account{{ID: "abc"}},
		Regions: []string{"eu-central-1"}}}
	if aws.Covers("ABC", "eu-central-1") {
		t.Error("an AWS account ID is compared without regard to case")
	}
	if !CaseInsensitiveTagNames(ProviderAzure) || CaseInsensitiveTagNames(ProviderAWS) || CaseInsensitiveTagNames(ProviderGCP) {
		t.Error("only Azure tag names are case-insensitive")
	}
}

func TestTagNamesAreWhatTheScopeReads(t *testing.T) {
	scope := &NetworkScope{Spec: NetworkScopeSpec{Provider: ProviderAzure,
		TagKeys:            TagKeys{Owner: "Team"},
		NetworkSelector:    &NetworkSelector{MatchTags: map[string]string{"Landing-Zone": ""}},
		RequiredSubnetTags: []string{"cost-center", "Team"},
		AutoImport: &AutoImportPolicy{ManagedTag: "Managed", InheritFromNetwork: []string{"app"},
			RequiredTags: []string{"Team"}, FromCreator: []CreatorRule{{Tags: map[string]string{"creator-team": "x"}}},
			AccountDefaults: []AccountDefault{{Tags: map[string]string{"default-team": "y"}}},
			Skip:            []SkipRule{{TagKey: "skip-me"}}},
	}}
	want := []string{"Landing-Zone", "Managed", "Team", "app", "cost-center", "creator-team", "default-team",
		"hs-claim", "hs-env", "hs-managed-by", "hs-tier", "skip-me"}
	if got := scope.TagNames(); !slices.Equal(got, want) {
		t.Errorf("TagNames = %v, want %v", got, want)
	}
	plain := &NetworkScope{Spec: NetworkScopeSpec{Provider: ProviderAzure}}
	if got := plain.TagNames(); !slices.Equal(got, []string{"hs-claim", "hs-env", "hs-managed", "hs-managed-by",
		"hs-owner", "hs-tier"}) {
		t.Errorf("TagNames of a scope with defaults only = %v", got)
	}
}

// Only Azure resource IDs are case-insensitive: the operator lowercases those and leaves an AWS
// or GCP ID exactly as it is, where another case is another (or no) resource.
func TestCanonicalResourceIDLowercasesAzureOnly(t *testing.T) {
	portal := "/subscriptions/0000000A-0000-4000-8000-00000000000B/resourceGroups/My-RG/providers/" +
		"Microsoft.Network/virtualNetworks/Hub/subnets/App"
	if got := CanonicalResourceID(ProviderAzure, portal); got != strings.ToLower(portal) {
		t.Errorf("CanonicalResourceID(Azure) = %q, want it in lowercase", got)
	}
	if !SameResourceID(ProviderAzure, portal, strings.ToLower(portal)) {
		t.Error("two spellings of one Azure resource ID are not the same resource")
	}
	for p, id := range map[Provider]string{
		ProviderAWS: "vpc-0ABCdef",
		ProviderGCP: "projects/My-Project/global/networks/Shared-VPC",
		"":          portal, // no provider known (the scope could not be read): nothing is assumed
	} {
		if got := CanonicalResourceID(p, id); got != id {
			t.Errorf("CanonicalResourceID(%q) = %q, want %q unchanged", p, got, id)
		}
		if SameResourceID(p, id, strings.ToLower(id)) {
			t.Errorf("SameResourceID(%q) takes %q and its lowercase for one resource", p, id)
		}
	}
}

func TestCanonicalizeIDsRewritesAzureClaimsAndImportsOnly(t *testing.T) {
	sub := "0000000A-0000-4000-8000-00000000000B"
	vnet := "/subscriptions/" + sub + "/resourceGroups/My-RG/providers/Microsoft.Network/virtualNetworks/Hub"
	claim := &SubnetClaim{Spec: SubnetClaimSpec{Account: sub, NetworkID: vnet}}
	claim.CanonicalizeIDs(ProviderAzure)
	if claim.Spec.Account != strings.ToLower(sub) || claim.Spec.NetworkID != strings.ToLower(vnet) {
		t.Errorf("Azure claim: account %q, networkID %q, want both in lowercase", claim.Spec.Account, claim.Spec.NetworkID)
	}
	imp := &ResourceImport{Spec: ResourceImportSpec{Account: sub, ResourceID: vnet + "/subnets/App"}}
	imp.CanonicalizeIDs(ProviderAzure)
	if imp.Spec.Account != strings.ToLower(sub) || imp.Spec.ResourceID != strings.ToLower(vnet+"/subnets/App") {
		t.Errorf("Azure import: account %q, resourceID %q, want both in lowercase", imp.Spec.Account, imp.Spec.ResourceID)
	}
	for _, p := range []Provider{ProviderAWS, ProviderGCP} {
		claim := &SubnetClaim{Spec: SubnetClaimSpec{Account: "My-Project", NetworkID: "projects/My-Project/global/networks/VPC"}}
		claim.CanonicalizeIDs(p)
		if claim.Spec.Account != "My-Project" || claim.Spec.NetworkID != "projects/My-Project/global/networks/VPC" {
			t.Errorf("%s claim was rewritten: %+v", p, claim.Spec)
		}
		imp := &ResourceImport{Spec: ResourceImportSpec{Account: "My-Project", ResourceID: "Subnet-0ABC"}}
		imp.CanonicalizeIDs(p)
		if imp.Spec.Account != "My-Project" || imp.Spec.ResourceID != "Subnet-0ABC" {
			t.Errorf("%s import was rewritten: %+v", p, imp.Spec)
		}
	}
}
