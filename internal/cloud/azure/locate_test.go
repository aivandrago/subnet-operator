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
	"context"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	"hypersurgery.dev/subnet-operator/internal/cloud/azure/azurefake"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// The read before an import (#116, #117): where the resource is, and whether it exists at all.
func TestLocateImport(t *testing.T) {
	p, cloud, target := writeFixture(t)
	ctx := context.Background()
	before := cloud.Tags(contractSubscription, contractGroup, "hub")
	// ARM spells a location either way; the operator's is the one of resource IDs.
	cloud.AddVirtualNetwork(contractSubscription, contractGroup, "spoke", "North Europe", []string{"10.20.0.0/16"}, nil)
	_ = cloud.Requests()

	for id, want := range map[string]string{
		vnetID("hub"):                                    contractLocation,
		vnetID("hub") + "/subnets/apps":                  contractLocation,
		strings.ToUpper(vnetID("hub")) + "/subnets/DATA": contractLocation,
		vnetID("spoke"):                                  "northeurope",
	} {
		got, err := p.LocateImport(ctx, target, id)
		if err != nil || got != want {
			t.Errorf("LocateImport(%s) = %q, %v, want %q", id, got, err, want)
		}
	}
	if got := methods(cloud.Requests()); len(got) != 4 {
		t.Errorf("four imports located with %v, want one read of the virtual network each", got)
	}

	for what, tc := range map[string]struct{ id, want string }{
		"a misspelt subnet":             {vnetID("hub") + "/subnets/app", `has no subnet "app" (it has apps, data)`},
		"a subnet of no subnets":        {vnetID("spoke") + "/subnets/apps", "it has no subnets"},
		"a missing network":             {vnetID("hubb"), "was not found in Azure"},
		"a subnet of a missing network": {vnetID("hubb") + "/subnets/apps", "virtualnetworks/hubb was not found"},
		"a missing resource group": {strings.Replace(vnetID("hub"), strings.ToLower(contractGroup), "rg-gone", 1),
			"ResourceGroupNotFound"},
	} {
		_, err := p.LocateImport(ctx, target, tc.id)
		if !errors.Is(err, inventory.ErrResourceNotFound) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want ErrResourceNotFound with %q", what, err, tc.want)
		}
	}

	if _, err := p.LocateImport(ctx, target, "subnet-0123"); err == nil || errors.Is(err, inventory.ErrResourceNotFound) {
		t.Errorf("an ID of another cloud = %v, want an error that is not ErrResourceNotFound", err)
	}
	if after := cloud.Tags(contractSubscription, contractGroup, "hub"); !maps.Equal(before, after) {
		t.Errorf("locating wrote tags: %v", after)
	}
}

func TestLocateImportNamesTheSubnetsUpToALimit(t *testing.T) {
	names := make([]string, 0, maxSubnetsNamed+3)
	for i := range maxSubnetsNamed + 3 {
		names = append(names, string(rune('a'+i)))
	}
	if got := subnetsNamed(names); !strings.HasSuffix(got, "j and 3 more") {
		t.Errorf("subnetsNamed of %d = %q", len(names), got)
	}
}

func TestLocateImportFailures(t *testing.T) {
	p, cloud, target := writeFixture(t)
	p.discoverer.sleep = func(context.Context, time.Duration) error { return nil }
	ctx := context.Background()

	cloud.ThrottleNext(3)
	if _, err := p.LocateImport(ctx, target, vnetID("hub")); err != nil {
		t.Errorf("a read throttled three times: %v", err)
	}
	cloud.Fail(azurefake.Throttle)
	if _, err := p.LocateImport(ctx, target, vnetID("hub")); !errors.Is(err, inventory.ErrThrottled) {
		t.Errorf("a read throttled for good = %v, want ErrThrottled", err)
	}
	// A reader role on other resource groups, or none: not "the subnet does not exist".
	cloud.Fail(azurefake.Deny)
	_, err := p.LocateImport(ctx, target, vnetID("hub")+"/subnets/apps")
	if err == nil || errors.Is(err, inventory.ErrResourceNotFound) || !strings.Contains(err.Error(), "the read identity") ||
		!strings.Contains(err.Error(), "Microsoft.Network/virtualNetworks/read") {
		t.Errorf("a read without the role = %v", err)
	}
	cloud.Fail(azurefake.None)
}

func TestImportNetwork(t *testing.T) {
	p := NewProvider(Options{})
	portal := "/subscriptions/" + strings.ToUpper(contractSubscription) + "/resourceGroups/RG/providers/" +
		"Microsoft.Network/virtualNetworks/Hub"
	lower := strings.ToLower(portal)
	for id, want := range map[string][2]string{
		portal:                   {lower, ""},
		portal + "/subnets/Apps": {lower, "Apps"},
	} {
		network, subnet, ok := p.ImportNetwork(id)
		if !ok || network != want[0] || subnet != want[1] {
			t.Errorf("ImportNetwork(%s) = %q, %q, %v", id, network, subnet, ok)
		}
	}
	if _, _, ok := p.ImportNetwork("vpc-0123"); ok {
		t.Error("an AWS ID is taken for an Azure one")
	}
}
