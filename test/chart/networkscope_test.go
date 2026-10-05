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

package chart

import (
	"maps"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	azurecloud "hypersurgery.dev/subnet-operator/internal/cloud/azure"
	gcpcloud "hypersurgery.dev/subnet-operator/internal/cloud/gcp"
)

// renderScope renders the chart's NetworkScope with helm arguments (-f, --set) and decodes it.
func renderScope(t *testing.T, args ...string) networkv1.NetworkScope {
	t.Helper()
	all := append([]string{"template", "release", chartDir, "--show-only", "templates/networkscope.yaml"}, args...)
	out, err := exec.Command(helmBinary(t), all...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template %v: %v: %s", args, err, out)
	}
	var scope networkv1.NetworkScope
	if err := yaml.UnmarshalStrict(out, &scope); err != nil {
		t.Fatalf("decoding the NetworkScope: %v\n%s", err, out)
	}
	return scope
}

// gcpScopeValues are the least a GCP scope from the chart needs.
var gcpScopeValues = []string{"--set", "providers.gcp.enabled=true", "--set", "networkScope.create=true",
	"--set", "networkScope.provider=GCP", "--set", "networkScope.gcp.tagParent=organizations/123456789012",
	"--set", "networkScope.accounts[0].id=net-host-prod", "--set", "networkScope.regions[0]=europe-west1"}

// examples/values-gcp.yaml renders a GCP scope the GCP provider accepts, with its service
// accounts, its settings and the GCP tag keys.
func TestTheGCPExampleRendersAGCPScope(t *testing.T) {
	scope := renderScope(t, "-f", "../../examples/values-gcp.yaml")
	if scope.Spec.Provider != networkv1.ProviderGCP {
		t.Fatalf("provider = %s, want GCP", scope.Spec.Provider)
	}
	if scope.Spec.GCP == nil || scope.Spec.GCP.TagParent != "organizations/123456789012" ||
		scope.Spec.GCP.ClaimTag != networkv1.GCPClaimTagSkip || scope.Spec.GCP.CreateTagValues {
		t.Errorf("gcp = %+v", scope.Spec.GCP)
	}
	if len(scope.Spec.Accounts) != 1 || scope.Spec.Accounts[0].GCP == nil ||
		scope.Spec.Accounts[0].GCP.WriteServiceAccount != "subnet-writer@net-host-prod.iam.gserviceaccount.com" {
		t.Errorf("accounts = %+v", scope.Spec.Accounts)
	}
	if scope.Spec.NetworkSelector == nil || !maps.Equal(scope.Spec.NetworkSelector.MatchTags,
		map[string]string{"hs-managed": "true"}) {
		t.Errorf("networkSelector = %+v, want hs-managed only", scope.Spec.NetworkSelector)
	}
	warnings, errs := gcpcloud.NewProvider(gcpcloud.Options{}).ValidateScope(&scope)
	if len(errs) > 0 || len(warnings) > 0 {
		t.Errorf("the GCP provider: warnings %v, errors %v", warnings, errs)
	}
}

// On GCP the chart's AWS default keys become the GCP ones, and the gcp settings are rendered.
func TestAGCPScopeGetsTheGCPKeysAndSettings(t *testing.T) {
	scope := renderScope(t, gcpScopeValues...)
	if scope.Spec.GCP.ClaimTag != "" || scope.Spec.GCP.CreateTagValues {
		t.Errorf("gcp = %+v, want the server's defaults", scope.Spec.GCP)
	}
	if !maps.Equal(scope.Spec.NetworkSelector.MatchTags, map[string]string{"hs-managed": "true"}) {
		t.Errorf("matchTags = %v, want hs-managed", scope.Spec.NetworkSelector.MatchTags)
	}
	if !slices.Equal(scope.Spec.RequiredSubnetTags, []string{"hs-owner", "hs-env", "hs-tier"}) {
		t.Errorf("requiredSubnetTags = %v, want the GCP keys", scope.Spec.RequiredSubnetTags)
	}

	scope = renderScope(t, append(slices.Clone(gcpScopeValues), "--set", "networkScope.gcp.claimTag=Bind",
		"--set", "networkScope.gcp.createTagValues=true", "--set", "networkScope.networkSelector.matchTags.team=",
		"--set", "networkScope.requiredSubnetTags={hs-owner}")...)
	if scope.Spec.GCP.ClaimTag != networkv1.GCPClaimTagBind || !scope.Spec.GCP.CreateTagValues {
		t.Errorf("gcp = %+v, want Bind and createTagValues", scope.Spec.GCP)
	}
	if !maps.Equal(scope.Spec.NetworkSelector.MatchTags, map[string]string{"team": ""}) {
		t.Errorf("matchTags = %v, want the user's key without the AWS default", scope.Spec.NetworkSelector.MatchTags)
	}
	if !slices.Equal(scope.Spec.RequiredSubnetTags, []string{"hs-owner"}) {
		t.Errorf("requiredSubnetTags = %v", scope.Spec.RequiredSubnetTags)
	}
}

// The AWS scope stays what it was: provider AWS and no gcp or azure member.
func TestAnAWSScopeIsUnchanged(t *testing.T) {
	scope := renderScope(t, "-f", "../../examples/values-organization.yaml")
	if scope.Spec.Provider != networkv1.ProviderAWS || scope.Spec.GCP != nil || scope.Spec.Azure != nil {
		t.Errorf("provider %s, gcp %+v, azure %+v", scope.Spec.Provider, scope.Spec.GCP, scope.Spec.Azure)
	}
	if !maps.Equal(scope.Spec.NetworkSelector.MatchTags, map[string]string{"hs/managed": "true"}) ||
		!slices.Equal(scope.Spec.RequiredSubnetTags, []string{"hs/owner", "hs/env", "hs/tier"}) {
		t.Errorf("tags = %v %v", scope.Spec.NetworkSelector.MatchTags, scope.Spec.RequiredSubnetTags)
	}
}

func TestNetworkScopeValuesThatCannotWorkAreRefused(t *testing.T) {
	// without is the least GCP scope as --set values, without the one starting with drop.
	without := func(drop string) []string {
		var out []string
		for i := 1; i < len(gcpScopeValues); i += 2 {
			if drop == "" || !strings.HasPrefix(gcpScopeValues[i], drop) {
				out = append(out, gcpScopeValues[i])
			}
		}
		return out
	}
	with := func(extra ...string) []string { return append(without(""), extra...) }
	refuses(t, "needs networkScope.gcp.tagParent", without("networkScope.gcp.tagParent")...)
	refuses(t, "needs providers.gcp.enabled", without("providers.gcp.enabled")...)
	refuses(t, "neither organizations/<organization number> nor projects/<project ID>",
		with("networkScope.gcp.tagParent=folders/1")...)
	refuses(t, "neither Skip nor Bind", with("networkScope.gcp.claimTag=Always")...)
	refuses(t, "has an aws member", with("networkScope.accounts[0].aws.roleARN=arn:aws:iam::1:role/r")...)
	refuses(t, "cannot be a GCP tag key", with("networkScope.requiredSubnetTags={hs/owner}")...)
	refuses(t, "cannot be a GCP tag key", with("networkScope.networkSelector.matchTags.hs/env=prod")...)
	refuses(t, "is not supported; use AWS, GCP or Azure", with("networkScope.provider=OCI")...)
	refuses(t, "has an azure member",
		with("networkScope.accounts[0].azure.clientID=3f0c6a1e-0000-4000-8000-0000000000b1")...)
	refuses(t, "networkScope.azure is only for networkScope.provider Azure",
		with("networkScope.azure.resourceGroups={rg-network}")...)

	aws := []string{"networkScope.create=true", "networkScope.regions[0]=eu-central-1",
		"networkScope.accounts[0].id=111111111111"}
	refuses(t, "networkScope.gcp is only for networkScope.provider GCP",
		append(slices.Clone(aws), "networkScope.gcp.claimTag=Bind")...)
	refuses(t, "has a gcp member",
		append(slices.Clone(aws), "networkScope.accounts[0].gcp.serviceAccount=a@b-cdef.iam.gserviceaccount.com")...)
	refuses(t, "networkScope.azure is only for networkScope.provider Azure",
		append(slices.Clone(aws), "networkScope.azure.resourceGroups={rg-network}")...)
	refuses(t, "has an azure member",
		append(slices.Clone(aws), "networkScope.accounts[0].azure.clientID=3f0c6a1e-0000-4000-8000-0000000000b1")...)
}

// azureScopeValues are the least an Azure scope from the chart needs.
var azureScopeValues = []string{"--set", "providers.azure.enabled=true", "--set", "networkScope.create=true",
	"--set", "networkScope.provider=Azure",
	"--set", "networkScope.accounts[0].id=5b3c2a10-0000-4000-8000-00000000a2e1",
	"--set", "networkScope.regions[0]=westeurope"}

// examples/values-azure.yaml renders an Azure scope the Azure provider accepts, with its
// identities, its resource groups and the keys Azure tag names allow.
func TestTheAzureExampleRendersAnAzureScope(t *testing.T) {
	scope := renderScope(t, "-f", "../../examples/values-azure.yaml")
	if scope.Spec.Provider != networkv1.ProviderAzure {
		t.Fatalf("provider = %s, want Azure", scope.Spec.Provider)
	}
	if scope.Spec.Azure == nil || !slices.Equal(scope.Spec.Azure.ResourceGroups, []string{"rg-network-prod"}) {
		t.Errorf("azure = %+v, want the example's resource group", scope.Spec.Azure)
	}
	if scope.Spec.GCP != nil {
		t.Errorf("gcp = %+v in an Azure scope", scope.Spec.GCP)
	}
	if len(scope.Spec.Accounts) != 1 || scope.Spec.Accounts[0].Azure == nil ||
		scope.Spec.Accounts[0].Azure.ClientID != "3f0c6a1e-0000-4000-8000-0000000000b1" ||
		scope.Spec.Accounts[0].Azure.WriteClientID != "3f0c6a1e-0000-4000-8000-0000000000c1" {
		t.Errorf("accounts = %+v", scope.Spec.Accounts)
	}
	if scope.Spec.NetworkSelector == nil || !maps.Equal(scope.Spec.NetworkSelector.MatchTags,
		map[string]string{"hs-managed": "true"}) {
		t.Errorf("networkSelector = %+v, want hs-managed only", scope.Spec.NetworkSelector)
	}
	p := azurecloud.NewProvider(azurecloud.Options{})
	warnings, errs := p.ValidateScope(&scope)
	if len(errs) > 0 || len(warnings) > 0 {
		t.Errorf("the Azure provider: warnings %v, errors %v", warnings, errs)
	}
	if errs := p.ValidateTags(nil, scope.Spec.NetworkSelector.MatchTags); len(errs) > 0 {
		t.Errorf("the Azure provider refuses the selector's tag names: %v", errs)
	}
}

// On Azure the chart's AWS default keys become the ones an Azure tag name allows, and a scope
// without resource groups has no azure member: it discovers the whole subscription.
func TestAnAzureScopeGetsTheAzureKeysAndSettings(t *testing.T) {
	scope := renderScope(t, azureScopeValues...)
	if scope.Spec.Provider != networkv1.ProviderAzure || scope.Spec.Azure != nil || scope.Spec.GCP != nil {
		t.Errorf("provider %s, azure %+v, gcp %+v; want Azure and no settings", scope.Spec.Provider,
			scope.Spec.Azure, scope.Spec.GCP)
	}
	if !maps.Equal(scope.Spec.NetworkSelector.MatchTags, map[string]string{"hs-managed": "true"}) {
		t.Errorf("matchTags = %v, want hs-managed", scope.Spec.NetworkSelector.MatchTags)
	}
	if !slices.Equal(scope.Spec.RequiredSubnetTags, []string{"hs-owner", "hs-env", "hs-tier"}) {
		t.Errorf("requiredSubnetTags = %v, want the Azure keys", scope.Spec.RequiredSubnetTags)
	}

	scope = renderScope(t, append(slices.Clone(azureScopeValues),
		"--set", "networkScope.azure.resourceGroups={rg-network,rg-hub}",
		"--set", "networkScope.accounts[0].azure.tenantID=0f8fad5b-0000-4000-8000-00000000d0c5",
		"--set", "networkScope.accounts[0].azure.clientID=3f0c6a1e-0000-4000-8000-0000000000b1",
		"--set", "networkScope.networkSelector.matchTags.team=", "--set", "networkScope.requiredSubnetTags={hs-owner}")...)
	if scope.Spec.Azure == nil || !slices.Equal(scope.Spec.Azure.ResourceGroups, []string{"rg-network", "rg-hub"}) {
		t.Errorf("azure = %+v, want both resource groups", scope.Spec.Azure)
	}
	if a := scope.Spec.Accounts[0].Azure; a == nil || a.TenantID != "0f8fad5b-0000-4000-8000-00000000d0c5" ||
		a.ClientID != "3f0c6a1e-0000-4000-8000-0000000000b1" {
		t.Errorf("accounts[0].azure = %+v", a)
	}
	if !maps.Equal(scope.Spec.NetworkSelector.MatchTags, map[string]string{"team": ""}) {
		t.Errorf("matchTags = %v, want the user's key without the AWS default", scope.Spec.NetworkSelector.MatchTags)
	}
	if !slices.Equal(scope.Spec.RequiredSubnetTags, []string{"hs-owner"}) {
		t.Errorf("requiredSubnetTags = %v", scope.Spec.RequiredSubnetTags)
	}
}

func TestAzureNetworkScopeValuesThatCannotWorkAreRefused(t *testing.T) {
	// without is the least Azure scope as --set values, without the one starting with drop.
	without := func(drop string) []string {
		var out []string
		for i := 1; i < len(azureScopeValues); i += 2 {
			if drop == "" || !strings.HasPrefix(azureScopeValues[i], drop) {
				out = append(out, azureScopeValues[i])
			}
		}
		return out
	}
	with := func(extra ...string) []string { return append(without(""), extra...) }
	refuses(t, "needs providers.azure.enabled", without("providers.azure.enabled")...)
	refuses(t, "are the same resource group on Azure",
		with("networkScope.azure.resourceGroups={rg-network,RG-Network}")...)
	refuses(t, "has an aws member", with("networkScope.accounts[0].aws.roleARN=arn:aws:iam::1:role/r")...)
	refuses(t, "has a gcp member",
		with("networkScope.accounts[0].gcp.serviceAccount=a@b-cdef.iam.gserviceaccount.com")...)
	refuses(t, "networkScope.gcp is only for networkScope.provider GCP",
		with("networkScope.gcp.tagParent=organizations/123456789012")...)
	refuses(t, "cannot be an Azure tag name", with("networkScope.requiredSubnetTags={hs/owner}")...)
	refuses(t, "cannot be an Azure tag name", with("networkScope.networkSelector.matchTags.hs/env=prod")...)
}
