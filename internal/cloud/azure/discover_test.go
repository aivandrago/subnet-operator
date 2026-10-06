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
	"sync"
	"testing"
	"time"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/cloud/azure/azurefake"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

const testGroup = "RG-Network"

func discoverAll(t *testing.T, p *Provider, target inventory.Target) *inventory.Snapshot {
	t.Helper()
	snap, err := p.Discover(context.Background(), target)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return snap
}

func contractTarget() inventory.Target {
	return inventory.Target{Provider: networkv1.ProviderAzure, Scope: "test", Account: contractSubscription,
		Region: contractLocation}
}

func subnetByName(t *testing.T, subnets []inventory.Subnet, name string) inventory.Subnet {
	t.Helper()
	for _, s := range subnets {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no subnet %s in %v", name, subnets)
	return inventory.Subnet{}
}

// discoverHub discovers one virtual network with subnets of every kind discovery tells apart,
// next to one in another location.
func discoverHub(t *testing.T) *inventory.Snapshot {
	t.Helper()
	p, cloud := newFakeProvider(t, Options{})
	cloud.AddVirtualNetwork(contractSubscription, testGroup, "Hub", "West Europe",
		[]string{"10.0.0.0/16", "10.1.0.0/16", "fd00:db8::/48"},
		map[string]string{"hs-owner": "platform", "hs-env": "prod", "hs-managed": "true",
			SubnetEntryName("APPS"): EncodeSubnetEntry(map[string]string{"hs-owner": "payments", "hs-tier": "private"})})
	apps := cloud.AddSubnet(contractSubscription, testGroup, "Hub", "apps", "10.0.1.0/24", "10.1.1.0/24", "fd00:db8:0:1::/64")
	web := cloud.AddSubnet(contractSubscription, testGroup, "Hub", "web", "10.0.2.0/24")
	sql := cloud.AddSubnet(contractSubscription, testGroup, "Hub", "sql", "10.0.3.0/27")
	gw := cloud.AddSubnet(contractSubscription, testGroup, "Hub", "GatewaySubnet", "10.0.255.0/27")
	cloud.AddSubnet(contractSubscription, testGroup, "Hub", "v6", "fd00:db8:0:2::/64")
	cloud.Update(func() {
		apps.Used, apps.IPConfigurations, apps.RouteTableID = 10, 3, "/subscriptions/x/rt"
		apps.ServiceEndpoints = []string{"Microsoft.Storage"}
		web.NotInUsages, web.IPConfigurations = true, 4
		sql.NotInUsages, sql.Delegations = true, []string{"Microsoft.Sql/managedInstances"}
		gw.UsageUnknown = true
	})
	// Elsewhere: another location, and nothing in it is reported.
	cloud.AddVirtualNetwork(contractSubscription, testGroup, "us", "eastus", []string{"10.9.0.0/16"}, nil)

	return discoverAll(t, p, contractTarget())
}

var hubID = canonicalID("/subscriptions/" + contractSubscription + "/resourceGroups/" + testGroup +
	"/providers/Microsoft.Network/virtualNetworks/Hub")

func TestDiscoverReadsVirtualNetworks(t *testing.T) {
	snap := discoverHub(t)
	if len(snap.Networks) != 1 {
		t.Fatalf("networks = %v, want the westeurope one only", snap.Networks)
	}
	n := snap.Networks[0]
	wantID := hubID
	if n.ID != wantID || n.Name != "Hub" || n.Region != contractLocation || n.Account != contractSubscription {
		t.Errorf("network = %s %q in %s/%s", n.ID, n.Name, n.Account, n.Region)
	}
	if !slices.Equal(n.CIDRBlocks, []string{"10.0.0.0/16", "10.1.0.0/16"}) ||
		!slices.Equal(n.IPv6CIDRBlocks, []string{"fd00:db8::/48"}) {
		t.Errorf("network blocks = %v / %v", n.CIDRBlocks, n.IPv6CIDRBlocks)
	}
	if want := map[string]string{"hs-owner": "platform", "hs-env": "prod", "hs-managed": "true"}; !maps.Equal(n.Tags, want) {
		t.Errorf("network tags = %v, want %v without the subnet entry", n.Tags, want)
	}
	if n.Azure == nil || n.Azure.ResourceGroup != testGroup || n.Azure.TagCount != 4 || n.Azure.SubnetOwnershipEntries != 1 {
		t.Errorf("network azure status = %+v", n.Azure)
	}
	if n.State != "Succeeded" {
		t.Errorf("network state = %q", n.State)
	}
}

// A subnet with several prefixes, its own ownership entry and a usage.
func TestDiscoverReadsASubnetWithItsEntryAndUsage(t *testing.T) {
	snap := discoverHub(t)
	wantID := hubID
	a := subnetByName(t, snap.Subnets, "apps")
	if a.ID != wantID+"/subnets/apps" || a.NetworkID != wantID || a.Region != contractLocation || a.Zone != "" {
		t.Errorf("apps = %s in %s/%s zone %q", a.ID, a.NetworkID, a.Region, a.Zone)
	}
	if a.CIDRBlock != "10.0.1.0/24" || !slices.Equal(a.SecondaryCIDRBlocks, []string{"10.1.1.0/24"}) ||
		!slices.Equal(a.IPv6CIDRBlocks, []string{"fd00:db8:0:1::/64"}) {
		t.Errorf("apps prefixes = %s %v %v", a.CIDRBlock, a.SecondaryCIDRBlocks, a.IPv6CIDRBlocks)
	}
	// Both IPv4 prefixes count, five reserved in each; the usages' limit less its current value.
	if *a.TotalIPs != 2*251 || *a.AvailableIPs != 2*251-10 {
		t.Errorf("apps addresses = %d free of %d, want %d of %d", *a.AvailableIPs, *a.TotalIPs, 2*251-10, 2*251)
	}
	if a.OwnershipSource != networkv1.OwnershipSourceSubnet ||
		!maps.Equal(a.Tags, map[string]string{"hs-owner": "payments", "hs-env": "prod", "hs-tier": "private", "hs-managed": "true"}) {
		t.Errorf("apps ownership = %s %v, want its entry (found whatever its case) over the network's tags",
			a.OwnershipSource, a.Tags)
	}
	if az := a.Azure; az.ResourceGroup != testGroup || az.IPConfigurations != 3 || az.RouteTableID != "/subscriptions/x/rt" ||
		!slices.Equal(az.ServiceEndpoints, []string{"Microsoft.Storage"}) || az.ServiceManaged ||
		az.IPUsageSource != UsageFromVirtualNetworkUsage {
		t.Errorf("apps azure status = %+v", az)
	}
}

// Subnets whose free addresses come from elsewhere, or stay unknown.
func TestDiscoverFallsBackOrLeavesUsageUnknown(t *testing.T) {
	snap := discoverHub(t)
	w := subnetByName(t, snap.Subnets, "web")
	if w.OwnershipSource != networkv1.OwnershipSourceNetwork || w.Tags["hs-owner"] != "platform" {
		t.Errorf("web inherits %s %v, want the network's", w.OwnershipSource, w.Tags)
	}
	if w.AvailableIPs == nil || *w.AvailableIPs != 251-4 || w.Azure.IPUsageSource != UsageFromIPConfigurations {
		t.Errorf("web, left out of the usages, has %v free (%s), want 247 from its IP configurations",
			w.AvailableIPs, w.Azure.IPUsageSource)
	}
	s := subnetByName(t, snap.Subnets, "sql")
	if !s.Azure.ServiceManaged || s.AvailableIPs != nil || s.Azure.IPUsageSource != "" ||
		!slices.Equal(s.Azure.Delegations, []string{"Microsoft.Sql/managedInstances"}) {
		t.Errorf("a delegated subnet left out of the usages = %+v, free %v; its free addresses are unknown", s.Azure, s.AvailableIPs)
	}
	if *s.TotalIPs != 32-5 {
		t.Errorf("sql total = %d", *s.TotalIPs)
	}
	g := subnetByName(t, snap.Subnets, "GatewaySubnet")
	if g.AvailableIPs != nil || g.TotalIPs == nil {
		t.Errorf("a subnet whose usage is -1 reports %v free of %v; free must stay unknown", g.AvailableIPs, g.TotalIPs)
	}
	v6 := subnetByName(t, snap.Subnets, "v6")
	if *v6.TotalIPs != 0 || *v6.AvailableIPs != 0 || v6.CIDRBlock != "" {
		t.Errorf("an IPv6-only subnet has %d/%d IPv4 addresses and CIDR %q", *v6.AvailableIPs, *v6.TotalIPs, v6.CIDRBlock)
	}
}

// Lists are paged; the provider follows every nextLink, and reads usages only for the virtual
// networks it reports.
func TestDiscoverFollowsPagesAndReadsUsagesOfReportedNetworksOnly(t *testing.T) {
	p, cloud := newFakeProvider(t, Options{})
	for i := range 5 {
		name := fmt.Sprintf("vnet-%d", i)
		tags := map[string]string{}
		if i%2 == 0 {
			tags["hs-managed"] = "true"
		}
		cloud.AddVirtualNetwork(contractSubscription, testGroup, name, contractLocation,
			[]string{fmt.Sprintf("10.%d.0.0/16", i)}, tags)
		for j := range 3 {
			cloud.AddSubnet(contractSubscription, testGroup, name, fmt.Sprintf("s%d", j), fmt.Sprintf("10.%d.%d.0/24", i, j))
		}
	}
	cloud.Requests()
	target := contractTarget()
	target.NetworkSelector = map[string]string{"hs-managed": "true"}
	snap := discoverAll(t, p, target)
	if len(snap.Networks) != 3 || len(snap.Subnets) != 9 || len(snap.UnmanagedNetworks) != 0 {
		t.Fatalf("got %d networks, %d subnets, %d unmanaged networks; want 3, 9, 0",
			len(snap.Networks), len(snap.Subnets), len(snap.UnmanagedNetworks))
	}
	for _, s := range snap.Subnets {
		if s.AvailableIPs == nil || *s.AvailableIPs != 251 {
			t.Errorf("subnet %s free = %v, want 251 read over two pages of usages", s.ID, s.AvailableIPs)
		}
	}
	var lists, usages int
	for _, r := range cloud.Requests() {
		switch {
		case strings.Contains(r, "/usages"):
			usages++
		case strings.Contains(r, "/virtualNetworks?"):
			lists++
		}
	}
	// Five networks at two a page are three pages; three reported networks' usages, two pages each.
	if lists != 3 || usages != 6 {
		t.Errorf("requests: %d list pages, %d usage pages; want 3 and 6", lists, usages)
	}
}

// Throttling is retried with the pacing Retry-After asks for, reported to OnThrottle each time,
// and only what is left after the last attempt is ErrThrottled.
func TestDiscoverRetriesThrottlingAndThenReportsIt(t *testing.T) {
	var mu sync.Mutex
	var throttled []string
	p, cloud := newFakeProvider(t, Options{OnThrottle: func(_ inventory.Target, op string) {
		mu.Lock()
		defer mu.Unlock()
		throttled = append(throttled, op)
	}})
	var slept []time.Duration
	p.discoverer.sleep = func(ctx context.Context, d time.Duration) error {
		if d > 0 {
			slept = append(slept, d)
		}
		return ctx.Err()
	}
	cloud.AddVirtualNetwork(contractSubscription, testGroup, "hub", contractLocation, []string{"10.0.0.0/16"}, nil)
	cloud.AddSubnet(contractSubscription, testGroup, "hub", "apps", "10.0.1.0/24")
	cloud.SetRetryAfter("7")
	cloud.ThrottleNext(2)
	snap := discoverAll(t, p, contractTarget())
	if len(snap.Subnets) != 1 {
		t.Fatalf("subnets = %v", snap.Subnets)
	}
	if !slices.Equal(throttled, []string{"virtualNetworks.listAll", "virtualNetworks.listAll"}) {
		t.Errorf("OnThrottle = %v, want the two throttled list calls", throttled)
	}
	if len(slept) < 2 || slept[0] != 7*time.Second || slept[1] != 14*time.Second {
		t.Errorf("waits = %v, want Retry-After's 7s, then doubled", slept)
	}

	cloud.Fail(azurefake.Throttle)
	_, err := p.Discover(context.Background(), contractTarget())
	if !errors.Is(err, inventory.ErrThrottled) || !strings.Contains(err.Error(), "SubscriptionRequestsThrottled") {
		t.Errorf("persistent throttling: %v, want ErrThrottled with ARM's code", err)
	}
	cloud.Fail(azurefake.Deny)
	_, err = p.Discover(context.Background(), contractTarget())
	if err == nil || errors.Is(err, inventory.ErrThrottled) || !strings.Contains(err.Error(), "AuthorizationFailed") {
		t.Errorf("a missing role assignment: %v, want AuthorizationFailed and not throttling", err)
	}
}

// Before ARM throttles, it reports the reads left; with few left the pacing slows down, and it
// speeds up again once there are enough.
func TestPacingFollowsTheRemainingReads(t *testing.T) {
	d := newDiscoverer(Options{})
	d.adjustPace("b", false, 0, lowRemainingReads-1)
	if got := d.currentPace("b"); got != minPace {
		t.Errorf("pace with few reads left = %v, want %v", got, minPace)
	}
	d.adjustPace("b", false, 0, 1000)
	d.adjustPace("b", false, 0, 1000)
	if got := d.currentPace("b"); got != 0 {
		t.Errorf("pace with reads to spare = %v, want none", got)
	}
	d.adjustPace("b", true, 5*time.Minute, -1)
	if got := d.currentPace("b"); got != maxPace {
		t.Errorf("pace after a Retry-After of 5m = %v, want the cap %v", got, maxPace)
	}
}

func TestRetryAfterHeaders(t *testing.T) {
	for _, tc := range []struct {
		header, value string
		want          time.Duration
	}{
		{"Retry-After", "3", 3 * time.Second},
		{"retry-after-ms", "1500", 1500 * time.Millisecond},
		{"x-ms-retry-after-ms", "250", 250 * time.Millisecond},
		{"Retry-After", "soon", 0},
	} {
		resp := &http.Response{Header: http.Header{}}
		resp.Header.Set(tc.header, tc.value)
		if got := retryAfter(resp); got != tc.want {
			t.Errorf("%s: %s = %v, want %v", tc.header, tc.value, got, tc.want)
		}
	}
	if retryAfter(nil) != 0 {
		t.Error("no response, but a wait")
	}
}

func TestSubnetEntriesRoundTrip(t *testing.T) {
	tags := map[string]string{"hs-owner": "payments", "hs-env": "prod", "odd;key=": "a=b;c%d", "empty": ""}
	entry := EncodeSubnetEntry(tags)
	if strings.Count(entry, ";") != len(tags)-1 {
		t.Errorf("entry %q does not separate its %d pairs by ';' alone", entry, len(tags))
	}
	if got := DecodeSubnetEntry(entry); !maps.Equal(got, tags) {
		t.Errorf("round trip of %v = %v (entry %q)", tags, got, entry)
	}
	if got := EncodeSubnetEntry(map[string]string{"hs-tier": "db", "hs-owner": "payments"}); got != "hs-owner=payments;hs-tier=db" {
		t.Errorf("entry = %q, want the pairs sorted by key", got)
	}
	// Edited by hand: what can be read is.
	if got := DecodeSubnetEntry(" hs-owner=a; ;broken;=x;hs-env=b "); !maps.Equal(got, map[string]string{"hs-owner": "a", "hs-env": "b "}) {
		t.Errorf("a hand-edited entry reads as %v", got)
	}
	own, entries := newTagNamer(contractTarget()).networkTags(map[string]*string{"HS-Subnet-Apps": new("hs-owner=a"), "hs-subnet-": new("x"),
		"hs-owner": new("b"), "nil": nil})
	if !maps.Equal(own, map[string]string{"hs-subnet-": "x", "hs-owner": "b", "nil": ""}) || len(entries) != 1 ||
		entries["apps"]["hs-owner"] != "a" {
		t.Errorf("networkTags = %v, %v", own, entries)
	}
}

// Azure compares tag names without regard to case: tags set as HS-Owner, Hs-Subnet-Web or
// HS-MANAGED are the operator's hs-owner, entry and marker, and a name the scope reads is
// reported in the scope's spelling. Values are case-sensitive and compared exactly.
func TestDiscoverMatchesTagNamesWithoutRegardToCase(t *testing.T) {
	p, cloud := newFakeProvider(t, Options{})
	cloud.AddVirtualNetwork(contractSubscription, testGroup, "hub", contractLocation, []string{"10.0.0.0/16"},
		map[string]string{"HS-Owner": "Platform", "HS-MANAGED": "true", "COST-CENTER": "1", "Other": "kept",
			"Hs-Subnet-WEB": "HS-OWNER=payments;Hs-Tier=db;cost-center=2"})
	cloud.AddSubnet(contractSubscription, testGroup, "hub", "web", "10.0.1.0/24")
	cloud.AddSubnet(contractSubscription, testGroup, "hub", "data", "10.0.2.0/24")

	target := contractTarget()
	target.NetworkSelector = map[string]string{"hs-managed": "true"}
	target.TagNames = []string{"hs-owner", "hs-env", "hs-tier", "Cost-Center"}
	snap := discoverAll(t, p, target)
	if len(snap.Networks) != 1 {
		t.Fatalf("networks = %v, want the hub, selected by HS-MANAGED", snap.Networks)
	}
	want := map[string]string{"hs-owner": "Platform", "hs-managed": "true", "Cost-Center": "1", "Other": "kept"}
	if n := snap.Networks[0]; !maps.Equal(n.Tags, want) || n.Azure.SubnetOwnershipEntries != 1 {
		t.Errorf("network tags = %v (%d entries), want %v and one entry", n.Tags, n.Azure.SubnetOwnershipEntries, want)
	}
	web := subnetByName(t, snap.Subnets, "web")
	wantWeb := map[string]string{"hs-owner": "payments", "hs-tier": "db", "hs-managed": "true", "Cost-Center": "2",
		"Other": "kept"}
	if web.OwnershipSource != networkv1.OwnershipSourceSubnet || !maps.Equal(web.Tags, wantWeb) {
		t.Errorf("web = %s %v, want its entry over the network's tags, by name in any case: %v",
			web.OwnershipSource, web.Tags, wantWeb)
	}
	if data := subnetByName(t, snap.Subnets, "data"); data.OwnershipSource != networkv1.OwnershipSourceNetwork ||
		data.Tags["hs-owner"] != "Platform" {
		t.Errorf("data = %s %v, want the network's tags", data.OwnershipSource, data.Tags)
	}

	// A name the scope does not read keeps Azure's spelling, and an entry's name replaces the
	// network's tag of the same name in any case rather than sitting next to it.
	target.TagNames = nil
	web = subnetByName(t, discoverAll(t, p, target).Subnets, "web")
	if web.Tags["cost-center"] != "2" || len(web.Tags) != 5 {
		t.Errorf("web tags = %v, want cost-center=2 in place of COST-CENTER", web.Tags)
	}

	// Values stay case-sensitive.
	target.NetworkSelector = map[string]string{"HS-Managed": "TRUE"}
	if snap := discoverAll(t, p, target); len(snap.Networks) != 0 {
		t.Errorf("a selector value TRUE matches true: %v", snap.Networks)
	}
	target.NetworkSelector = map[string]string{"Hs-Managed": "true"}
	if snap := discoverAll(t, p, target); len(snap.Networks) != 1 || snap.Networks[0].Tags["Hs-Managed"] != "true" {
		t.Errorf("a selector key in another case: %v", snap.Networks)
	}
}

// With resource groups in the scope, discovery lists the virtual networks of those groups
// only, one List call per group (each once, whatever its case), and never List All.
func TestDiscoverListsOnlyTheScopesResourceGroups(t *testing.T) {
	p, cloud := newFakeProvider(t, Options{})
	for i, group := range []string{"RG-A", "rg-b", "rg-c"} {
		cloud.AddVirtualNetwork(contractSubscription, group, "vnet", contractLocation,
			[]string{fmt.Sprintf("10.%d.0.0/16", i)}, nil)
		cloud.AddSubnet(contractSubscription, group, "vnet", "s", fmt.Sprintf("10.%d.1.0/24", i))
	}
	cloud.Requests()
	target := contractTarget()
	target.Azure = &networkv1.AzureScope{ResourceGroups: []string{"rg-a", "RG-B", "Rg-A"}}
	snap := discoverAll(t, p, target)
	groups := make([]string, 0, len(snap.Networks))
	for _, n := range snap.Networks {
		groups = append(groups, n.Azure.ResourceGroup)
	}
	slices.Sort(groups)
	if !slices.Equal(groups, []string{"RG-A", "rg-b"}) || len(snap.Subnets) != 2 {
		t.Errorf("networks in %v with %d subnets, want those of RG-A and rg-b", groups, len(snap.Subnets))
	}
	var lists []string
	for _, r := range cloud.Requests() {
		if !strings.Contains(r, "/usages") {
			lists = append(lists, r[:strings.Index(r, "?")])
		}
	}
	base := "GET /subscriptions/" + contractSubscription
	if want := []string{
		base + "/resourceGroups/rg-a/providers/Microsoft.Network/virtualNetworks",
		base + "/resourceGroups/RG-B/providers/Microsoft.Network/virtualNetworks",
	}; !slices.Equal(lists, want) {
		t.Errorf("list requests = %v, want %v", lists, want)
	}

	// A group that does not exist fails the target, naming it.
	target.Azure.ResourceGroups = []string{"rg-a", "rg-gone"}
	_, err := p.Discover(context.Background(), target)
	if err == nil || !strings.Contains(err.Error(), "rg-gone") || !strings.Contains(err.Error(), "ResourceGroupNotFound") {
		t.Errorf("a missing resource group: %v", err)
	}
	// Throttling of the per-group call is reported under its own operation.
	var ops []string
	p2, cloud2 := newFakeProvider(t, Options{OnThrottle: func(_ inventory.Target, op string) { ops = append(ops, op) }})
	cloud2.AddResourceGroup(contractSubscription, "rg-empty")
	cloud2.ThrottleNext(1)
	target.Azure.ResourceGroups = []string{"rg-empty"}
	if snap := discoverAll(t, p2, target); len(snap.Networks) != 0 {
		t.Errorf("an empty group has networks: %v", snap.Networks)
	}
	if !slices.Equal(ops, []string{"virtualNetworks.list"}) {
		t.Errorf("OnThrottle = %v", ops)
	}
}
