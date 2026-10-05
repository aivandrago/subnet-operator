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
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/cloud/azure/azurefake"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// writeFixture is a provider and a fake with the virtual network hub (10.10.0.0/16), owned by
// platform in prod, with the subnet apps and its entry.
func writeFixture(t *testing.T) (*Provider, *azurefake.Cloud, inventory.Target) {
	t.Helper()
	p, cloud := newFakeProvider(t, Options{})
	cloud.AddVirtualNetwork(contractSubscription, contractGroup, "hub", contractLocation, []string{"10.10.0.0/16"},
		map[string]string{"hs-owner": "platform", "hs-env": "prod",
			SubnetEntryName("apps"): EncodeSubnetEntry(map[string]string{"hs-owner": "payments"})})
	cloud.AddSubnet(contractSubscription, contractGroup, "hub", "apps", "10.10.1.0/24")
	cloud.AddSubnet(contractSubscription, contractGroup, "hub", "data", "10.10.2.0/24")
	target := inventory.Target{Provider: networkv1.ProviderAzure, Account: contractSubscription, Region: contractLocation}
	return p, cloud, target
}

func claimRequest(name, cidr string) inventory.CreateSubnetRequest {
	return inventory.CreateSubnetRequest{NetworkID: vnetID("hub"), CIDRBlock: cidr, Name: name,
		Tags: map[string]string{"hs-owner": "team-a", "hs-env": "prod", "hs-managed-by": "subnet-operator",
			"hs-claim": "default/" + name}}
}

// methods lists the requests of one kind as "METHOD <what>", in order.
func methods(requests []string) []string {
	var out []string
	for _, r := range requests {
		method, uri, _ := strings.Cut(r, " ")
		path, _, _ := strings.Cut(uri, "?")
		switch {
		case strings.HasSuffix(path, "/providers/Microsoft.Resources/tags/default"):
			out = append(out, method+" tags")
		case strings.Contains(path, "/operations/"):
			out = append(out, method+" operation")
		case strings.Contains(path, "/subnets/"):
			out = append(out, method+" subnet")
		default:
			out = append(out, method+" "+path)
		}
	}
	return out
}

func TestCreateSubnet(t *testing.T) {
	p, cloud, target := writeFixture(t)
	ctx := context.Background()
	_ = cloud.Requests()

	id, err := p.CreateSubnet(ctx, target, claimRequest("orders", "10.10.3.0/24"))
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}
	if want := vnetID("hub") + "/subnets/orders"; id != want {
		t.Errorf("id = %s, want %s", id, want)
	}
	got := methods(cloud.Requests())
	want := []string{"GET subnet", "GET tags", "PATCH tags", "PUT subnet", "GET operation", "GET operation",
		"GET operation", "GET subnet"}
	if !slices.Equal(got, want) {
		t.Errorf("requests = %v, want %v: the entry first, then the subnet, polled to its end", got, want)
	}
	if s := cloud.Subnet(contractSubscription, contractGroup, "hub", "orders"); s == nil ||
		!slices.Equal(s.AddressPrefixes, []string{"10.10.3.0/24"}) {
		t.Fatalf("subnet orders = %+v", s)
	}
	entry := DecodeSubnetEntry(cloud.Tags(contractSubscription, contractGroup, "hub")["hs-subnet-orders"])
	wantEntry := map[string]string{"hs-owner": "team-a", "hs-managed-by": "subnet-operator", "hs-claim": "default/orders"}
	if !maps.Equal(entry, wantEntry) {
		t.Errorf("entry = %v, want %v: hs-env=prod is the network's and is inherited", entry, wantEntry)
	}

	// The same create again, as after a lost status update: the subnet is the claim's.
	again, err := p.CreateSubnet(ctx, target, claimRequest("orders", "10.10.3.0/24"))
	if err != nil || again != id {
		t.Errorf("CreateSubnet again = %q, %v; want %s", again, err, id)
	}
	if slices.Contains(methods(cloud.Requests()), "PUT subnet") {
		t.Errorf("an existing subnet was PUT again")
	}

	// Another claim naming an existing subnet gets an error and changes nothing.
	for _, req := range []inventory.CreateSubnetRequest{
		claimRequest("apps", "10.10.1.0/24"), claimRequest("orders", "10.10.9.0/24"),
		func() inventory.CreateSubnetRequest {
			r := claimRequest("orders", "10.10.3.0/24")
			r.Tags["hs-claim"] = "default/other"
			return r
		}(),
	} {
		_, err := p.CreateSubnet(ctx, target, req)
		if err == nil || !strings.Contains(err.Error(), "exists already") {
			t.Errorf("CreateSubnet(%s, %s, %s) = %v, want exists already", req.Name, req.CIDRBlock, req.Tags["hs-claim"], err)
		}
	}
	if got := methods(cloud.Requests()); slices.Contains(got, "PUT subnet") || slices.Contains(got, "PATCH tags") {
		t.Errorf("a refused create wrote: %v", got)
	}
	if n := cloud.UnconditionalPuts(); n > 0 {
		t.Errorf("%d subnet PUTs without If-None-Match: *", n)
	}

	// A subnet that appears between the read and the create is not changed.
	cloud.Update(func() {
		cloud.VirtualNetwork(contractSubscription, contractGroup, "hub").Subnets = append(
			cloud.VirtualNetwork(contractSubscription, contractGroup, "hub").Subnets, &azurefake.Subnet{Name: "racer",
				AddressPrefixes: []string{"10.10.12.0/24"}})
	})
	c, err := p.discoverer.writeClientsFor(target)
	if err != nil {
		t.Fatal(err)
	}
	network, _ := parseResourceID(vnetID("hub"))
	err = p.discoverer.createError(c, "racer", claimRequest("racer", "10.10.13.0/24"), func() error {
		_, err := c.subnets.BeginCreateOrUpdate(policy.WithHTTPHeader(ctx, http.Header{"If-None-Match": []string{"*"}}),
			network.group, network.vnet, "racer", armnetwork.Subnet{Properties: &armnetwork.SubnetPropertiesFormat{
				AddressPrefix: new("10.10.13.0/24")}}, nil)
		return err
	}())
	if err == nil || !strings.Contains(err.Error(), "exists already") {
		t.Errorf("a PUT on a subnet that appeared meanwhile = %v, want exists already", err)
	}
	if s := cloud.Subnet(contractSubscription, contractGroup, "hub", "racer"); !slices.Equal(s.AddressPrefixes,
		[]string{"10.10.12.0/24"}) {
		t.Errorf("the subnet that appeared meanwhile was changed: %v", s.AddressPrefixes)
	}
}

func TestCreateSubnetRefusals(t *testing.T) {
	p, cloud, target := writeFixture(t)
	ctx := context.Background()

	for what, tc := range map[string]struct {
		req  inventory.CreateSubnetRequest
		want string
	}{
		"a zone": {func() inventory.CreateSubnetRequest {
			r := claimRequest("x", "10.10.5.0/24")
			r.Zone = "1"
			return r
		}(), "regional"},
		"a reserved name": {claimRequest("GatewaySubnet", "10.10.5.0/24"), "reserves"},
		"an invalid name": {claimRequest("x-", "10.10.5.0/24"), "subnet name"},
		"a subnet as network": {func() inventory.CreateSubnetRequest {
			r := claimRequest("x", "10.10.5.0/24")
			r.NetworkID += "/subnets/a"
			return r
		}(), "not an Azure virtual network ID"},
		"another subscription": {func() inventory.CreateSubnetRequest {
			r := claimRequest("x", "10.10.5.0/24")
			r.NetworkID = strings.Replace(r.NetworkID, contractSubscription, "00000000-0000-4000-8000-000000000001", 1)
			return r
		}(), "not in subscription"},
	} {
		if _, err := p.CreateSubnet(ctx, target, tc.req); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", what, err, tc.want)
		}
	}

	for _, cidr := range []string{"10.10.1.0/24", "10.10.1.128/25", "172.16.0.0/24"} {
		_, err := p.CreateSubnet(ctx, target, claimRequest("clash", cidr))
		if !errors.Is(err, inventory.ErrCIDRConflict) {
			t.Errorf("CreateSubnet(%s) = %v, want ErrCIDRConflict", cidr, err)
		}
	}
	if cloud.Subnet(contractSubscription, contractGroup, "hub", "clash") != nil {
		t.Errorf("a refused subnet exists")
	}

	// An operation that fails at its end is reported with ARM's error.
	cloud.FailOperations("NetcfgSubnetRangesOverlap", "Subnet 'late' overlaps a subnet created meanwhile.")
	if _, err := p.CreateSubnet(ctx, target, claimRequest("late", "10.10.6.0/24")); !errors.Is(err, inventory.ErrCIDRConflict) {
		t.Errorf("a failed operation = %v, want ErrCIDRConflict", err)
	}
	cloud.FailOperations("InternalServerError", "Something went wrong.")
	if _, err := p.CreateSubnet(ctx, target, claimRequest("late", "10.10.6.0/24")); err == nil ||
		!strings.Contains(err.Error(), "InternalServerError") {
		t.Errorf("a failed operation = %v, want ARM's error", err)
	}
	cloud.FailOperations("", "")
	if _, err := p.CreateSubnet(ctx, target, claimRequest("late", "10.10.6.0/24")); err != nil {
		t.Errorf("the next attempt, with the entry of the failed ones in place: %v", err)
	}
}

func TestWritesRetryBusyAndThrottled(t *testing.T) {
	p, cloud, target := writeFixture(t)
	ctx := context.Background()

	cloud.BusyNext(2)
	if _, err := p.CreateSubnet(ctx, target, claimRequest("busy", "10.10.7.0/24")); err != nil {
		t.Errorf("a create while another operation briefly held the network: %v", err)
	}
	cloud.BusyNext(retryMaxAttempts)
	_, err := p.CreateSubnet(ctx, target, claimRequest("busier", "10.10.8.0/24"))
	if err == nil || !strings.Contains(err.Error(), "another operation") || errors.Is(err, inventory.ErrThrottled) {
		t.Errorf("a network held by another operation for longer = %v", err)
	}
	cloud.BusyNext(0)

	cloud.ThrottleNext(3)
	if err := p.WriteOwnership(ctx, target, vnetID("hub"), map[string]string{"hs-tier": "shared"}); err != nil {
		t.Errorf("a write throttled three times: %v", err)
	}
	cloud.Fail(azurefake.Throttle)
	if err := p.WriteOwnership(ctx, target, vnetID("hub"), map[string]string{"cost-center": "1"}); !errors.Is(err, inventory.ErrThrottled) {
		t.Errorf("a write throttled for good = %v, want ErrThrottled", err)
	}
	cloud.Fail(azurefake.None)
}

func TestWritesWithoutPermission(t *testing.T) {
	p, cloud, target := writeFixture(t)
	ctx := context.Background()
	before := cloud.Tags(contractSubscription, contractGroup, "hub")
	cloud.Fail(azurefake.DenyWrites)
	defer cloud.Fail(azurefake.None)

	_, err := p.CreateSubnet(ctx, target, claimRequest("denied", "10.10.4.0/24"))
	if err == nil || !strings.Contains(err.Error(), "writer role") || !strings.Contains(err.Error(), "AuthorizationFailed") {
		t.Errorf("CreateSubnet without the role = %v", err)
	}
	err = p.WriteOwnership(ctx, target, vnetID("hub"), map[string]string{"hs-tier": "shared"})
	if err == nil || !strings.Contains(err.Error(), "Microsoft.Resources/tags/write") || errors.Is(err, inventory.ErrThrottled) {
		t.Errorf("WriteOwnership without the role = %v", err)
	}
	if after := cloud.Tags(contractSubscription, contractGroup, "hub"); !maps.Equal(before, after) {
		t.Errorf("tags changed without permission: %v", after)
	}
}

func TestWriteOwnership(t *testing.T) {
	p, cloud, target := writeFixture(t)
	ctx := context.Background()
	apps, data := vnetID("hub")+"/subnets/apps", vnetID("hub")+"/subnets/data"
	tags := func() map[string]string { return cloud.Tags(contractSubscription, contractGroup, "hub") }

	// A subnet's values include what it inherits: the network's owner is not taken over.
	err := p.WriteOwnership(ctx, target, data, map[string]string{"hs-owner": "team-a", "hs-tier": "db"})
	if !errors.Is(err, inventory.ErrOwnershipConflict) || !strings.Contains(err.Error(), "inherited") {
		t.Errorf("an import over an inherited owner = %v, want ErrOwnershipConflict", err)
	}
	if _, ok := tags()["hs-subnet-data"]; ok {
		t.Errorf("a refused write left an entry")
	}
	// What it inherits with the same value is not written; what is new is.
	_ = cloud.Requests()
	if err := p.WriteOwnership(ctx, target, data, map[string]string{"hs-owner": "platform", "hs-env": "prod"}); err != nil {
		t.Errorf("an import of the inherited values: %v", err)
	}
	if slices.Contains(methods(cloud.Requests()), "PATCH tags") {
		t.Errorf("an import of values the subnet already has wrote something")
	}
	if err := p.WriteOwnership(ctx, target, data, map[string]string{"hs-owner": "platform", "hs-tier": "db"}); err != nil {
		t.Errorf("an import that adds a tier: %v", err)
	}
	if got := DecodeSubnetEntry(tags()["hs-subnet-data"]); !maps.Equal(got, map[string]string{"hs-tier": "db"}) {
		t.Errorf("entry of data = %v, want only hs-tier", got)
	}

	// The entry's own values are not replaced; the network's may differ from them.
	err = p.WriteOwnership(ctx, target, apps, map[string]string{"hs-owner": "billing"})
	if !errors.Is(err, inventory.ErrOwnershipConflict) || !strings.Contains(err.Error(), `hs-owner is "payments"`) {
		t.Errorf("an import over the entry's owner = %v", err)
	}
	if err := p.WriteOwnership(ctx, target, apps, map[string]string{"hs-owner": "payments", "hs-env": "dev"}); err == nil {
		t.Errorf("an import over the inherited env was accepted")
	}

	// A network's own tags, compared without regard to case.
	if err := p.WriteOwnership(ctx, target, vnetID("hub"), map[string]string{"HS-Owner": "platform", "cost-center": "7"}); err != nil {
		t.Errorf("a network import: %v", err)
	}
	if err := p.WriteOwnership(ctx, target, vnetID("hub"), map[string]string{"HS-OWNER": "other"}); !errors.Is(err, inventory.ErrOwnershipConflict) {
		t.Errorf("a network import over its owner = %v, want ErrOwnershipConflict", err)
	}
	got := tags()
	if got["hs-owner"] != "platform" || got["cost-center"] != "7" || got["HS-Owner"] != "" {
		t.Errorf("network tags = %v", got)
	}
	if err := p.WriteOwnership(ctx, target, vnetID("hub"), nil); err != nil {
		t.Errorf("writing nothing: %v", err)
	}
}

func TestWriteOwnershipCaseInsensitiveEntry(t *testing.T) {
	p, cloud, target := writeFixture(t)
	cloud.SetTag(contractSubscription, contractGroup, "hub", "HS-SUBNET-Data", "HS-Tier=db")
	err := p.WriteOwnership(context.Background(), target, vnetID("hub")+"/subnets/data",
		map[string]string{"hs-tier": "db", "cost-center": "9"})
	if err != nil {
		t.Fatalf("WriteOwnership: %v", err)
	}
	tags := cloud.Tags(contractSubscription, contractGroup, "hub")
	if _, dup := tags["hs-subnet-data"]; dup {
		t.Errorf("a second entry was written for data: %v", tags)
	}
	if got := DecodeSubnetEntry(tags["HS-SUBNET-Data"]); !maps.Equal(got, map[string]string{"HS-Tier": "db", "cost-center": "9"}) {
		t.Errorf("entry = %v", got)
	}
}

func TestWriteLimits(t *testing.T) {
	p, cloud, target := writeFixture(t)
	ctx := context.Background()

	long := map[string]string{"hs-tier": strings.Repeat("t", 120), "cost-center": strings.Repeat("c", 120)}
	err := p.WriteOwnership(ctx, target, vnetID("hub")+"/subnets/data", long)
	if !errors.Is(err, inventory.ErrOwnershipEntryTooLong) {
		t.Errorf("an entry over 256 characters = %v, want ErrOwnershipEntryTooLong", err)
	}

	// hub carries 3 tags; fill it to the limit.
	for i := range azurefake.MaxTags - 3 {
		cloud.SetTag(contractSubscription, contractGroup, "hub", fmt.Sprintf("filler-%02d", i), "x")
	}
	for what, write := range map[string]func() error{
		"a new entry": func() error {
			return p.WriteOwnership(ctx, target, vnetID("hub")+"/subnets/data", map[string]string{"hs-tier": "db"})
		},
		"a network tag": func() error {
			return p.WriteOwnership(ctx, target, vnetID("hub"), map[string]string{"hs-tier": "db"})
		},
		"a created subnet": func() error {
			_, err := p.CreateSubnet(ctx, target, claimRequest("full", "10.10.9.0/24"))
			return err
		},
	} {
		if err := write(); !errors.Is(err, inventory.ErrTagBudgetExceeded) {
			t.Errorf("%s on a network with 50 tags = %v, want ErrTagBudgetExceeded", what, err)
		}
	}
	if cloud.Subnet(contractSubscription, contractGroup, "hub", "full") != nil {
		t.Errorf("a subnet was created without room for its entry")
	}
	// An entry that exists takes no more room.
	if err := p.WriteOwnership(ctx, target, vnetID("hub")+"/subnets/apps", map[string]string{"hs-tier": "web"}); err != nil {
		t.Errorf("adding to an existing entry on a full network: %v", err)
	}
}
