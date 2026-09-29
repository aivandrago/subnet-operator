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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
)

func gcpScope(mutate func(*networkv1.NetworkScope)) *networkv1.NetworkScope {
	s := &networkv1.NetworkScope{Spec: networkv1.NetworkScopeSpec{
		Provider: networkv1.ProviderGCP,
		Accounts: []networkv1.Account{{ID: "shared-vpc-host"}},
		Regions:  []string{"europe-west1", "northamerica-northeast2"},
		GCP:      &networkv1.GCPScope{TagParent: "organizations/123"},
	}}
	if mutate != nil {
		mutate(s)
	}
	return s
}

func TestValidateScope(t *testing.T) {
	p := NewProvider(Options{})
	if _, errs := p.ValidateScope(gcpScope(nil)); len(errs) > 0 {
		t.Errorf("a valid scope was refused: %v", errs)
	}
	for name, mutate := range map[string]func(*networkv1.NetworkScope){
		"an AWS region":           func(s *networkv1.NetworkScope) { s.Spec.Regions = []string{"eu-central-1"} },
		"an AWS account":          func(s *networkv1.NetworkScope) { s.Spec.Accounts[0].ID = "111111111111" },
		"an account's AWS region": func(s *networkv1.NetworkScope) { s.Spec.Accounts[0].Regions = []string{"us-east-1"} },
		"no tag parent":           func(s *networkv1.NetworkScope) { s.Spec.GCP = nil },
	} {
		if _, errs := p.ValidateScope(gcpScope(mutate)); len(errs) == 0 {
			t.Errorf("%s was accepted", name)
		}
	}
	for _, mode := range []networkv1.AutoImportMode{networkv1.AutoImportOff, networkv1.AutoImportDryRun,
		networkv1.AutoImportApply} {
		s := gcpScope(func(s *networkv1.NetworkScope) { s.Spec.AutoImport = &networkv1.AutoImportPolicy{Mode: mode} })
		if _, errs := p.ValidateScope(s); len(errs) > 0 {
			t.Errorf("an auto-import policy in mode %s was refused: %v", mode, errs)
		}
	}
}

// claimTag Bind is valid either way, with a warning that says what it costs.
func TestValidateScopeWarnsAboutTheClaimTag(t *testing.T) {
	p := NewProvider(Options{})
	for _, tc := range []struct {
		name  string
		gcp   networkv1.GCPScope
		wants string
	}{
		{"unset", networkv1.GCPScope{}, ""},
		{"Skip", networkv1.GCPScope{ClaimTag: networkv1.GCPClaimTagSkip, CreateTagValues: true}, ""},
		{"Bind", networkv1.GCPScope{ClaimTag: networkv1.GCPClaimTagBind}, "TagValueMissing"},
		{"Bind creating values", networkv1.GCPScope{ClaimTag: networkv1.GCPClaimTagBind, CreateTagValues: true},
			"1,000"},
	} {
		s := gcpScope(func(s *networkv1.NetworkScope) {
			g := tc.gcp
			g.TagParent = "organizations/123"
			s.Spec.GCP = &g
		})
		warnings, errs := p.ValidateScope(s)
		if len(errs) > 0 {
			t.Errorf("%s: refused: %v", tc.name, errs)
		}
		switch {
		case tc.wants == "" && len(warnings) > 0:
			t.Errorf("%s: warnings %v, want none", tc.name, warnings)
		case tc.wants != "" && (len(warnings) != 1 || !strings.Contains(warnings[0], tc.wants)):
			t.Errorf("%s: warnings %v, want one about %s", tc.name, warnings, tc.wants)
		}
	}
}

func gcpClaim(mutate func(*networkv1.SubnetClaim)) *networkv1.SubnetClaim {
	c := &networkv1.SubnetClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "payments", Namespace: "team-a"},
		Spec: networkv1.SubnetClaimSpec{
			ScopeRef: "gcp", Account: "shared-vpc-host", Region: "europe-west1",
			NetworkID: "projects/shared-vpc-host/global/networks/shared", PrefixLength: 24,
			Mode: networkv1.ClaimModeCreate, Owner: "team-a", Env: "prod",
			GCP: &networkv1.GCPClaimOptions{PoolCIDRs: []string{"10.20.0.0/16"}},
		},
	}
	if mutate != nil {
		mutate(c)
	}
	return c
}

func TestValidateClaim(t *testing.T) {
	p := NewProvider(Options{})
	if errs := p.ValidateClaim(gcpClaim(nil)); len(errs) > 0 {
		t.Errorf("a valid claim was refused: %v", errs)
	}
	if reason, msg := p.ClaimRefusal(gcpClaim(nil)); reason != "" {
		t.Errorf("the claim controller would refuse a valid claim: %s: %s", reason, msg)
	}
	for name, tc := range map[string]struct {
		mutate func(*networkv1.SubnetClaim)
		reason string
	}{
		"zones":                   {func(c *networkv1.SubnetClaim) { c.Spec.Zones = []string{"europe-west1-b"} }, "ZonesNotSupported"},
		"no pool":                 {func(c *networkv1.SubnetClaim) { c.Spec.GCP = nil }, "PoolRequired"},
		"a pool smaller than it":  {func(c *networkv1.SubnetClaim) { c.Spec.GCP.PoolCIDRs = []string{"10.20.0.0/26"} }, "InvalidClaim"},
		"a pool that is no range": {func(c *networkv1.SubnetClaim) { c.Spec.GCP.PoolCIDRs = []string{"10.20.0.1/16"} }, "InvalidClaim"},
		"an IPv6 pool":            {func(c *networkv1.SubnetClaim) { c.Spec.GCP.PoolCIDRs = []string{"fd00::/48"} }, "InvalidClaim"},
		"an AWS VPC":              {func(c *networkv1.SubnetClaim) { c.Spec.NetworkID = "vpc-0abc" }, "InvalidClaim"},
		"another project's network": {func(c *networkv1.SubnetClaim) {
			c.Spec.NetworkID = "projects/other-project/global/networks/shared"
		}, "InvalidClaim"},
		"an AWS account":        {func(c *networkv1.SubnetClaim) { c.Spec.Account = "111111111111" }, "InvalidClaim"},
		"a /30":                 {func(c *networkv1.SubnetClaim) { c.Spec.PrefixLength = 30 }, "InvalidClaim"},
		"aws options":           {func(c *networkv1.SubnetClaim) { c.Spec.AWS = &networkv1.AWSClaimOptions{} }, "InvalidClaim"},
		"a name with a dot":     {func(c *networkv1.SubnetClaim) { c.Spec.NamePrefix = "payments.v2" }, "InvalidClaim"},
		"an owner with a slash": {func(c *networkv1.SubnetClaim) { c.Spec.Owner = "team/a" }, "InvalidClaim"},
	} {
		claim := gcpClaim(tc.mutate)
		if errs := p.ValidateClaim(claim); len(errs) == 0 {
			t.Errorf("a claim with %s was accepted", name)
		}
		if reason, _ := p.ClaimRefusal(claim); reason != tc.reason {
			t.Errorf("a claim with %s: the controller's reason is %q, want %q", name, reason, tc.reason)
		}
	}
}

func TestValidateImport(t *testing.T) {
	p := NewProvider(Options{})
	imp := func(region, id string) *networkv1.ResourceImport {
		return &networkv1.ResourceImport{Spec: networkv1.ResourceImportSpec{ScopeRef: "gcp", Account: "shared-vpc-host",
			Region: region, ResourceID: id, Tags: map[string]string{"hs-owner": "team-a"}}}
	}
	for _, ok := range []*networkv1.ResourceImport{
		imp("europe-west1", "projects/shared-vpc-host/regions/europe-west1/subnetworks/apps"),
		imp("", "projects/shared-vpc-host/global/networks/shared"),
		imp("europe-west1", "projects/shared-vpc-host/global/networks/shared"),
	} {
		if errs := p.ValidateImport(ok); len(errs) > 0 {
			t.Errorf("import of %s in %q was refused: %v", ok.Spec.ResourceID, ok.Spec.Region, errs)
		}
		if reason, _ := p.ImportRefusal(ok); reason != "" {
			t.Errorf("the controller would refuse the import of %s: %s", ok.Spec.ResourceID, reason)
		}
	}
	for name, bad := range map[string]*networkv1.ResourceImport{
		"an AWS subnet":                  imp("europe-west1", "subnet-0abc"),
		"a subnetwork of another region": imp("us-central1", "projects/shared-vpc-host/regions/europe-west1/subnetworks/apps"),
		"a subnetwork without a region":  imp("", "projects/shared-vpc-host/regions/europe-west1/subnetworks/apps"),
		"another project's network":      imp("", "projects/other-project/global/networks/shared"),
	} {
		if errs := p.ValidateImport(bad); len(errs) == 0 {
			t.Errorf("an import of %s was accepted", name)
		}
		if reason, _ := p.ImportRefusal(bad); reason == "" {
			t.Errorf("the controller would act on an import of %s", name)
		}
	}
}

func TestValidateTags(t *testing.T) {
	p := NewProvider(Options{})
	ok := map[string]string{"hs-owner": "team-a", "cost_center": "cc.42", "Name": "db-1"}
	if errs := p.ValidateTags(field.NewPath("tags"), ok); len(errs) > 0 {
		t.Errorf("valid tags were refused: %v", errs)
	}
	errs := p.ValidateTags(field.NewPath("tags"), map[string]string{"hs/owner": "team-a"})
	if len(errs) != 1 || !strings.Contains(errs[0].Detail, `"/"`) {
		t.Errorf("an AWS key: %v, want one error that names the slash", errs)
	}
}
