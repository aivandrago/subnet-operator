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
	"maps"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
)

func azureScope() *networkv1.NetworkScope {
	return &networkv1.NetworkScope{Spec: networkv1.NetworkScopeSpec{
		Provider: networkv1.ProviderAzure,
		Accounts: []networkv1.Account{{ID: contractSubscription}},
		Regions:  []string{"westeurope", "germanywestcentral"},
	}}
}

func TestValidateScope(t *testing.T) {
	p := NewProvider(Options{})
	if _, errs := p.ValidateScope(azureScope()); len(errs) > 0 {
		t.Errorf("a valid scope is refused: %v", errs)
	}
	for what, tc := range map[string]struct {
		mutate func(*networkv1.NetworkScope)
		want   string
	}{
		"an AWS region":  {func(s *networkv1.NetworkScope) { s.Spec.Regions = []string{"eu-central-1"} }, "not an Azure location"},
		"a GCP region":   {func(s *networkv1.NetworkScope) { s.Spec.Regions = []string{"europe-west1"} }, "not an Azure location"},
		"a display name": {func(s *networkv1.NetworkScope) { s.Spec.Accounts[0].Regions = []string{"West Europe"} }, "not an Azure location"},
		"an AWS account": {func(s *networkv1.NetworkScope) { s.Spec.Accounts[0].ID = "111111111111" }, "subscription ID"},
		"a subscription twice in two cases": {func(s *networkv1.NetworkScope) {
			s.Spec.Accounts = append(s.Spec.Accounts, networkv1.Account{ID: strings.ToUpper(contractSubscription)})
		}, "without regard to case"},
		"account defaults twice in two cases": {func(s *networkv1.NetworkScope) {
			s.Spec.AutoImport = &networkv1.AutoImportPolicy{AccountDefaults: []networkv1.AccountDefault{
				{Account: contractSubscription}, {Account: strings.ToUpper(contractSubscription)}}}
		}, "accountDefaults[1].account"},
		"a resource group twice in two cases": {func(s *networkv1.NetworkScope) {
			s.Spec.Azure = &networkv1.AzureScope{ResourceGroups: []string{"rg-net", "RG-Net"}}
		}, "resourceGroups[1]"},
		"a tag name in two cases": {func(s *networkv1.NetworkScope) {
			s.Spec.RequiredSubnetTags = []string{"HS-Tier"}
		}, "same Azure tag name"},
		"a selector key in another case than a tag key": {func(s *networkv1.NetworkScope) {
			s.Spec.TagKeys.Owner = "Owner"
			s.Spec.NetworkSelector = &networkv1.NetworkSelector{MatchTags: map[string]string{"owner": "x"}}
		}, "same Azure tag name"},
	} {
		s := azureScope()
		tc.mutate(s)
		_, errs := p.ValidateScope(s)
		if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), tc.want) {
			t.Errorf("%s: %v, want %q", what, errs, tc.want)
		}
	}
	upper := azureScope()
	upper.Spec.Accounts[0].ID = strings.ToUpper(contractSubscription)
	upper.Spec.Azure = &networkv1.AzureScope{ResourceGroups: []string{"RG-Net", "rg-apps"}}
	upper.Spec.TagKeys.Owner = "Team"
	upper.Spec.RequiredSubnetTags = []string{"Team", "hs-tier"}
	if _, errs := p.ValidateScope(upper); len(errs) > 0 {
		t.Errorf("a subscription ID in capitals, resource groups and tag names of its own are refused: %v", errs)
	}
	for _, mode := range []networkv1.AutoImportMode{networkv1.AutoImportOff, networkv1.AutoImportDryRun,
		networkv1.AutoImportApply} {
		s := azureScope()
		s.Spec.AutoImport = &networkv1.AutoImportPolicy{Mode: mode}
		if _, errs := p.ValidateScope(s); len(errs) > 0 {
			t.Errorf("an auto-import policy in mode %s is refused: %v", mode, errs)
		}
	}
}

func azureClaim() *networkv1.SubnetClaim {
	return &networkv1.SubnetClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: "team-a"},
		Spec: networkv1.SubnetClaimSpec{ScopeRef: "azure", Account: strings.ToUpper(contractSubscription),
			Region: contractLocation, NetworkID: vnetID("hub"), PrefixLength: 24, Owner: "team-a", Env: "prod"},
	}
}

// The auto-import rules that go by a resource's creator can never match on Azure, where no
// creator is known: the scope is accepted, and says so.
func TestValidateScopeWarnsAboutCreatorRules(t *testing.T) {
	p := NewProvider(Options{})
	scope := &networkv1.NetworkScope{Spec: networkv1.NetworkScopeSpec{
		Provider: networkv1.ProviderAzure,
		Accounts: []networkv1.Account{{ID: "5b3c2a10-0000-4000-8000-00000000a2e1"}},
		Regions:  []string{"westeurope"},
		AutoImport: &networkv1.AutoImportPolicy{
			InheritFromNetwork: []string{"hs-owner"},
			Skip:               []networkv1.SkipRule{{TagKey: "managed-by", TagValue: "terraform"}},
		},
	}}
	if warnings, errs := p.ValidateScope(scope); len(warnings) > 0 || len(errs) > 0 {
		t.Fatalf("a policy without creator rules: warnings %v, errors %v; want neither", warnings, errs)
	}

	scope.Spec.AutoImport.FromCreator = []networkv1.CreatorRule{
		{PrincipalPrefix: "payments-deploy@", Tags: map[string]string{"hs-owner": "team-payments"}}}
	scope.Spec.AutoImport.Skip = append(scope.Spec.AutoImport.Skip, networkv1.SkipRule{PrincipalPrefix: "terraform-"})
	warnings, errs := p.ValidateScope(scope)
	if len(errs) > 0 {
		t.Fatalf("creator rules are refused on Azure: %v; want a warning only", errs)
	}
	if len(warnings) != 2 || !strings.Contains(warnings[0], "spec.autoImport.fromCreator") ||
		!strings.Contains(warnings[1], "spec.autoImport.skip") {
		t.Fatalf("warnings = %q, want one for fromCreator and one for the skip rule by principal", warnings)
	}
	for _, w := range warnings {
		if !strings.Contains(w, "never match on Azure") {
			t.Errorf("warning %q does not say the rule never matches", w)
		}
	}
}

func TestValidateClaim(t *testing.T) {
	p := NewProvider(Options{})
	if errs := p.ValidateClaim(azureClaim()); len(errs) > 0 {
		t.Errorf("a valid claim is refused: %v", errs)
	}
	if reason, msg := p.ClaimRefusal(azureClaim()); reason != "" {
		t.Errorf("a valid claim is refused by the controller: %s %s", reason, msg)
	}
	for what, tc := range map[string]struct {
		mutate       func(*networkv1.SubnetClaim)
		want, reason string
	}{
		"zones": {func(c *networkv1.SubnetClaim) { c.Spec.Zones = []string{"1"} }, "regional", "ZonesNotSupported"},
		"an AWS account": {func(c *networkv1.SubnetClaim) { c.Spec.Account = "111111111111" }, "subscription ID",
			"InvalidClaim"},
		"a GCP region": {func(c *networkv1.SubnetClaim) { c.Spec.Region = "europe-west1" }, "not an Azure location",
			"InvalidClaim"},
		"an AWS network": {func(c *networkv1.SubnetClaim) { c.Spec.NetworkID = "vpc-0123" }, "virtual network ID",
			"InvalidClaim"},
		"a subnet as network": {func(c *networkv1.SubnetClaim) { c.Spec.NetworkID = vnetID("hub") + "/subnets/a" },
			"virtual network ID", "InvalidClaim"},
		"a network of another subscription": {func(c *networkv1.SubnetClaim) {
			c.Spec.NetworkID = strings.Replace(vnetID("hub"), contractSubscription, "00000000-0000-4000-8000-000000000001", 1)
		}, "not in", "InvalidClaim"},
		"a /30": {func(c *networkv1.SubnetClaim) { c.Spec.PrefixLength = 30 }, "/29", "InvalidClaim"},
		"GCP options": {func(c *networkv1.SubnetClaim) { c.Spec.GCP = &networkv1.GCPClaimOptions{} },
			"only valid for provider GCP", "InvalidClaim"},
		"AWS options": {func(c *networkv1.SubnetClaim) { c.Spec.AWS = &networkv1.AWSClaimOptions{} },
			"only valid for provider AWS", "InvalidClaim"},
		"a name Azure refuses": {func(c *networkv1.SubnetClaim) { c.Spec.NamePrefix = "apps-" }, "subnet name",
			"InvalidClaim"},
		"a reserved name": {func(c *networkv1.SubnetClaim) { c.Spec.NamePrefix = "azurebastionsubnet" }, "reserves",
			"InvalidClaim"},
		"an entry that cannot fit": {func(c *networkv1.SubnetClaim) {
			c.Spec.Tags = map[string]string{"cost-center": strings.Repeat("c", 200)}
		}, "256 characters", "OwnershipEntryTooLong"},
	} {
		c := azureClaim()
		tc.mutate(c)
		errs := p.ValidateClaim(c)
		if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), tc.want) {
			t.Errorf("%s: %v, want %q", what, errs, tc.want)
		}
		if reason, _ := p.ClaimRefusal(c); reason != tc.reason {
			t.Errorf("%s: refusal reason %q, want %q", what, reason, tc.reason)
		}
	}
}

// portalID is a virtual network's ID as the Azure portal shows it: the subscription as it was
// typed, resourceGroups and Microsoft.Network/virtualNetworks in camel case, the names in the
// case they were created with.
func portalID(name string) string {
	return "/subscriptions/" + strings.ToUpper(contractSubscription) + "/resourceGroups/" +
		strings.ToUpper(contractGroup) + "/providers/Microsoft.Network/virtualNetworks/" + name
}

// The case of an Azure ID is never what a claim or an import is refused for: ARM compares IDs
// without regard to case, and the webhooks and controllers bring them into the operator's
// spelling rather than asking somebody to retype what they pasted from the portal.
func TestValidateAcceptsResourceIDsInAnyCase(t *testing.T) {
	p := NewProvider(Options{})
	for _, id := range []string{portalID("Hub"), strings.ToUpper(vnetID("hub")), vnetID("hub")} {
		c := azureClaim()
		c.Spec.NetworkID = id
		if errs := p.ValidateClaim(c); len(errs) > 0 {
			t.Errorf("a claim on %s is refused: %v", id, errs)
		}
		if reason, msg := p.ClaimRefusal(c); reason != "" {
			t.Errorf("a claim on %s is refused by the controller: %s %s", id, reason, msg)
		}
		for _, resource := range []string{id, id + "/subnets/Apps"} {
			imp := &networkv1.ResourceImport{Spec: networkv1.ResourceImportSpec{ScopeRef: "azure",
				Account: contractSubscription, Region: contractLocation, ResourceID: resource,
				Tags: map[string]string{"hs-owner": "team-a"}}}
			if errs := p.ValidateImport(imp); len(errs) > 0 {
				t.Errorf("an import of %s is refused: %v", resource, errs)
			}
			if reason, msg := p.ImportRefusal(imp); reason != "" {
				t.Errorf("an import of %s is refused by the controller: %s %s", resource, reason, msg)
			}
		}
	}
	// One spelling only: the provider's own is the one the API package gives the webhooks.
	if got, want := canonicalID(portalID("Hub")), vnetID("hub"); got != want {
		t.Errorf("canonicalID = %s, want %s", got, want)
	}
	if got := networkv1.CanonicalResourceID(networkv1.ProviderAzure, portalID("Hub")); got != vnetID("hub") {
		t.Errorf("CanonicalResourceID = %s, want %s", got, vnetID("hub"))
	}
}

func TestClaimTags(t *testing.T) {
	c := azureClaim()
	c.Spec.Tags = map[string]string{"cost-center": "42", "hs-owner": "somebody else"}
	got := ClaimTags(c)
	want := map[string]string{"cost-center": "42", "hs-owner": "team-a", "hs-env": "prod",
		"hs-managed-by": "subnet-operator", "hs-claim": "team-a/orders"}
	if !maps.Equal(got, want) {
		t.Errorf("ClaimTags = %v, want %v", got, want)
	}
}

func TestValidateImport(t *testing.T) {
	p := NewProvider(Options{})
	imp := func(id string, tags map[string]string) *networkv1.ResourceImport {
		return &networkv1.ResourceImport{Spec: networkv1.ResourceImportSpec{ScopeRef: "azure",
			Account: contractSubscription, Region: contractLocation, ResourceID: id, Tags: tags}}
	}
	for _, ok := range []*networkv1.ResourceImport{
		imp(vnetID("hub"), map[string]string{"hs-owner": "team-a"}),
		imp(vnetID("hub")+"/subnets/apps", map[string]string{"hs-owner": "team-a"}),
		imp(vnetID("hub"), map[string]string{"hs-owner": strings.Repeat("o", 250), "hs-env": strings.Repeat("e", 250)}),
	} {
		if errs := p.ValidateImport(ok); len(errs) > 0 {
			t.Errorf("a valid import of %s is refused: %v", ok.Spec.ResourceID, errs)
		}
	}
	for what, tc := range map[string]struct {
		imp          *networkv1.ResourceImport
		want, reason string
	}{
		"an AWS subnet": {imp("subnet-0123", nil), "virtual network or subnet ID", "InvalidResourceID"},
		"another subscription": {imp(strings.Replace(vnetID("hub"), contractSubscription,
			"00000000-0000-4000-8000-000000000001", 1), nil), "not in", "InvalidResourceID"},
		"a subnet entry that cannot fit": {imp(vnetID("hub")+"/subnets/apps", map[string]string{
			"hs-owner": strings.Repeat("o", 200), "hs-env": strings.Repeat("e", 100)}), "256", "OwnershipEntryTooLong"},
	} {
		errs := p.ValidateImport(tc.imp)
		if len(errs) == 0 || !strings.Contains(errs.ToAggregate().Error(), tc.want) {
			t.Errorf("%s: %v, want %q", what, errs, tc.want)
		}
		if reason, _ := p.ImportRefusal(tc.imp); reason != tc.reason {
			t.Errorf("%s: refusal reason %q, want %q", what, reason, tc.reason)
		}
	}

	// Without a location (#117): the webhook requires it, after the mutating webhook had its
	// chance to fill it in; the controller does not refuse for it, since it reads the virtual
	// network and takes the location from there.
	nowhere := imp(vnetID("hub")+"/subnets/apps", map[string]string{"hs-owner": "team-a"})
	nowhere.Spec.Region = ""
	errs := p.ValidateImport(nowhere)
	if len(errs) != 1 || errs[0].Type != field.ErrorTypeRequired || errs[0].Field != "spec.region" {
		t.Errorf("an import without a location: %v, want spec.region: Required", errs)
	}
	if reason, msg := p.ImportRefusal(nowhere); reason != "" {
		t.Errorf("the controller refuses an import without a location: %s: %s", reason, msg)
	}
	nowhere.Spec.Region = "West Europe"
	if reason, _ := p.ImportRefusal(nowhere); reason != "InvalidResourceID" {
		t.Errorf("a location that is not one: refusal reason %q", reason)
	}
}

func TestValidateTags(t *testing.T) {
	p := NewProvider(Options{})
	ok := map[string]string{"hs-owner": "team/a", "Cost Center": "", "hs-subnet": "not an entry",
		strings.Repeat("k", maxTagNameLength): strings.Repeat("v", maxTagValueLength)}
	if errs := p.ValidateTags(field.NewPath("tags"), ok); len(errs) > 0 {
		t.Errorf("valid Azure tags are refused: %v", errs)
	}
	for _, bad := range []map[string]string{
		{"a/b": "x"}, {"a?b": "x"}, {`a\b`: "x"}, {"a&b": "x"}, {"a>b": "x"},
		{"HS-SUBNET-apps": "x"}, {"Owner": "a", "owner": "b"},
	} {
		if errs := p.ValidateTags(field.NewPath("tags"), bad); len(errs) == 0 {
			t.Errorf("%v is accepted", bad)
		}
	}
}
