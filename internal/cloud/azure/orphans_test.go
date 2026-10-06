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
	"slices"
	"strings"
	"testing"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/cloud/azure/azurefake"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

func orphanedTags(n NetworkOwnershipEntries) []string {
	out := make([]string, 0, len(n.Orphaned))
	for _, e := range n.Orphaned {
		out = append(out, e.Tag)
	}
	return out
}

func ownershipEntries(t *testing.T, p *Provider, target inventory.Target, locations ...string) []NetworkOwnershipEntries {
	t.Helper()
	got, err := p.OwnershipEntries(context.Background(), target, locations)
	if err != nil {
		t.Fatalf("OwnershipEntries: %v", err)
	}
	return got
}

func removeSubnet(fake *azurefake.Cloud, vnet, name string) {
	fake.Update(func() {
		v := fake.VirtualNetwork(contractSubscription, contractGroup, vnet)
		v.Subnets = slices.DeleteFunc(v.Subnets, func(s *azurefake.Subnet) bool { return s.Name == name })
	})
}

// The three ways an entry comes to name no subnet (#121): its subnet was deleted, its subnet
// was created again under another name, and a create wrote it and the subnet never came to
// exist. Entries of subnets that exist are not listed, in whatever case either is spelt, and
// neither is any tag that is not an entry.
func TestOrphanedEntriesAreTheOnesWithoutASubnet(t *testing.T) {
	p, fake, target := writeFixture(t)
	ctx := context.Background()
	entry := func(owner string) string { return EncodeSubnetEntry(map[string]string{"hs-owner": owner}) }
	set := func(tag, value string) { fake.SetTag(contractSubscription, contractGroup, "hub", tag, value) }

	// A subnet with an entry that is deleted: the entry of data stays.
	set(SubnetEntryName("data"), entry("team-data"))
	if got := ownershipEntries(t, p, target); len(got) != 1 || len(got[0].Orphaned) != 0 {
		t.Fatalf("with every entry's subnet in place: %+v, want one network and nothing orphaned", got)
	}
	removeSubnet(fake, "hub", "data")

	// A subnet created again under another name: Azure cannot rename one.
	set(SubnetEntryName("web-old"), entry("team-web"))
	fake.AddSubnet(contractSubscription, contractGroup, "hub", "web-new", "10.10.3.0/24")
	set(SubnetEntryName("web-new"), entry("team-web"))

	// A create that wrote its entry and then failed, and was given up.
	fake.FailOperations("InternalServerError", "Something went wrong.")
	if _, err := p.CreateSubnet(ctx, target, claimRequest("abandoned", "10.10.6.0/24")); err == nil {
		t.Fatal("the create succeeded")
	}
	fake.FailOperations("", "")

	// Names in any case: Azure compares tag names and subnet names without regard to it.
	fake.AddSubnet(contractSubscription, contractGroup, "hub", "MixedCase", "10.10.4.0/24")
	set("HS-Subnet-MIXEDCASE", entry("team-case"))
	set("Hs-Subnet-Gone", entry("team-gone"))

	// Not entries: other tags, the network's own ownership, the bare prefix, a near miss.
	set("cost-center", "42")
	set("hs-subnet-", "nothing")
	set("hs-subnets", "apps,data")
	set("hs-subnetwork-apps", entry("x"))

	got := ownershipEntries(t, p, target)
	if len(got) != 1 {
		t.Fatalf("networks = %+v, want hub", got)
	}
	hub := got[0]
	want := []string{"hs-subnet-abandoned", "hs-subnet-data", "Hs-Subnet-Gone", "hs-subnet-web-old"}
	if !slices.Equal(orphanedTags(hub), want) {
		t.Errorf("orphaned = %v, want %v", orphanedTags(hub), want)
	}
	if hub.ID != vnetID("hub") || hub.ResourceGroup != contractGroup || hub.Name != "hub" ||
		hub.Subscription != contractSubscription || hub.Location != contractLocation {
		t.Errorf("the network is %+v", hub)
	}
	// hs-owner, hs-env, cost-center, hs-subnet-, hs-subnets, hs-subnetwork-apps of its own,
	// and the entries of apps, data, web-old, web-new, abandoned, MixedCase and Gone.
	if hub.TagCount != 13 || hub.TagLimit != 50 || hub.OwnershipEntries != 7 {
		t.Errorf("tags = %d of %d, %d of them entries; want 13 of 50, 7", hub.TagCount, hub.TagLimit, hub.OwnershipEntries)
	}
	if n := len(fake.Tags(contractSubscription, contractGroup, "hub")); n != hub.TagCount {
		t.Errorf("the network carries %d tags and %d are reported", n, hub.TagCount)
	}
	abandoned, gone := hub.Orphaned[0], hub.Orphaned[2]
	if abandoned.Claim != "default/abandoned" || abandoned.Subnet != "abandoned" || !abandoned.OperatorFormat ||
		abandoned.Tags["hs-owner"] != "team-a" || abandoned.Value != fake.Tags(contractSubscription, contractGroup,
		"hub")["hs-subnet-abandoned"] {
		t.Errorf("the entry of the abandoned create is %+v", abandoned)
	}
	if gone.Subnet != "Gone" || gone.Claim != "" || gone.Value != "hs-owner=team-gone" {
		t.Errorf("the entry in capitals is %+v", gone)
	}
}

// Listing is a read, as the read identity, with the one call discovery lists with: a reader
// that may write nothing can run it, and nothing is written.
func TestOwnershipEntriesNeedOnlyTheReader(t *testing.T) {
	p, fake, target := writeFixture(t)
	fake.SetTag(contractSubscription, contractGroup, "hub", SubnetEntryName("gone"), "hs-owner=x")
	fake.AddIdentity(azurefake.DefaultTenant, testReader, true)
	fake.RestrictSubscription(contractSubscription, testReader)
	fake.RestrictSubscriptionWrites(contractSubscription)
	target.Identity = Identity{ClientID: testReader}
	before := fake.Tags(contractSubscription, contractGroup, "hub")
	fake.Requests()

	got := ownershipEntries(t, p, target)
	if len(got) != 1 || !slices.Equal(orphanedTags(got[0]), []string{"hs-subnet-gone"}) {
		t.Errorf("entries = %+v, want hs-subnet-gone orphaned", got)
	}
	requests := fake.Requests()
	if len(requests) == 0 {
		t.Fatal("nothing was asked")
	}
	for _, r := range requests {
		if !strings.HasPrefix(r, "GET /subscriptions/"+contractSubscription+"/providers/Microsoft.Network/virtualNetworks?") {
			t.Errorf("request %q, want only the listing of virtual networks", r)
		}
	}
	if len(fake.RequestsBy(azurefake.Operator)) != 0 {
		t.Errorf("requests as the operator's own identity: %v", fake.RequestsBy(azurefake.Operator))
	}
	if after := fake.Tags(contractSubscription, contractGroup, "hub"); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("the tags changed: %v, were %v", after, before)
	}

	// An identity without the role is refused, and named.
	fake.AddIdentity(azurefake.DefaultTenant, testWriter, true)
	target.Identity = Identity{ClientID: testWriter}
	if _, err := p.OwnershipEntries(context.Background(), target, nil); err == nil ||
		!strings.Contains(err.Error(), "as client "+testWriter) || !strings.Contains(err.Error(), "AuthorizationFailed") {
		t.Errorf("err = %v, want AuthorizationFailed as client %s", err, testWriter)
	}
	fake.Fail(azurefake.Throttle)
	target.Identity = Identity{ClientID: testReader}
	if _, err := p.OwnershipEntries(context.Background(), target, nil); !errors.Is(err, inventory.ErrThrottled) {
		t.Errorf("err = %v, want ErrThrottled", err)
	}
}

// A network at Azure's limit is reported as 50 of 50, with what of it is orphaned; every
// virtual network of the subscription is read, in every location unless some are named, and
// only the scope's resource groups when it names some.
func TestOwnershipEntriesCountTagsPerNetwork(t *testing.T) {
	p, fake, target := writeFixture(t)
	full := map[string]string{"hs-owner": "platform"}
	for i := range 40 {
		full[fmt.Sprintf("cost-%02d", i)] = "x"
	}
	for i := range 9 {
		full[SubnetEntryName(fmt.Sprintf("s%d", i))] = "hs-owner=team"
	}
	fake.AddVirtualNetwork(contractSubscription, testGroup, "Full", "North Europe", []string{"10.20.0.0/16"}, full)
	for i := range 3 {
		fake.AddSubnet(contractSubscription, testGroup, "Full", fmt.Sprintf("S%d", i), fmt.Sprintf("10.20.%d.0/24", i))
	}
	fake.AddVirtualNetwork(contractSubscription, testGroup, "empty", "eastus", []string{"10.30.0.0/16"}, nil)

	got := ownershipEntries(t, p, target)
	if len(got) != 3 {
		t.Fatalf("networks = %+v, want hub, Full and empty", got)
	}
	var fullNet NetworkOwnershipEntries
	for _, n := range got {
		if n.Name == "Full" {
			fullNet = n
		}
		if n.Name == "empty" && (n.TagCount != 0 || n.Orphaned == nil || len(n.Orphaned) != 0) {
			t.Errorf("a network without tags is %+v, want 0 tags and an empty list", n)
		}
	}
	if fullNet.TagCount != 50 || fullNet.TagLimit != 50 || fullNet.OwnershipEntries != 9 || fullNet.Location != "northeurope" ||
		!slices.Equal(orphanedTags(fullNet), []string{"hs-subnet-s3", "hs-subnet-s4", "hs-subnet-s5", "hs-subnet-s6",
			"hs-subnet-s7", "hs-subnet-s8"}) {
		t.Errorf("the full network is %+v, want 50 of 50 tags, 9 entries, s3 to s8 orphaned", fullNet)
	}

	if got := ownershipEntries(t, p, target, "NorthEurope", "westus"); len(got) != 1 || got[0].Name != "Full" {
		t.Errorf("in northeurope and westus: %+v, want Full", got)
	}
	target.Azure = &networkv1.AzureScope{ResourceGroups: []string{strings.ToUpper(contractGroup)}}
	if got := ownershipEntries(t, p, target); len(got) != 1 || got[0].Name != "hub" {
		t.Errorf("in the scope's resource group: %+v, want hub", got)
	}
}

func TestIsOperatorEntry(t *testing.T) {
	for _, tc := range []struct {
		tag, value string
		want       bool
	}{
		{"hs-subnet-apps", "hs-owner=payments", true},
		{"HS-SUBNET-Apps_1.x", "hs-claim=default/checkout;hs-env=prod;hs-owner=team-payments", true},
		{"hs-subnet-apps", EncodeSubnetEntry(map[string]string{"a;b": "c=d", "e%": "it's 100%"}), true},
		{"hs-subnet-apps", "hs-owner=", true},
		// Not what the operator writes: unsorted, not pairs, a pair twice, an escape in
		// lowercase, nothing at all, more than a tag value holds.
		{"hs-subnet-apps", "hs-owner=payments;hs-env=prod", false},
		{"hs-subnet-apps", "owned by payments", false},
		{"hs-subnet-apps", "hs-owner=a;hs-owner=b", false},
		{"hs-subnet-apps", "a%3bb=c", false},
		{"hs-subnet-apps", "hs-owner=payments;", false},
		{"hs-subnet-apps", "", false},
		{"hs-subnet-apps", "k=" + strings.Repeat("v", 255), false},
		// Not an entry's name: no subnet name, not a subnet name, another tag.
		{"hs-subnet-", "hs-owner=payments", false},
		{"hs-subnet-my subnet", "hs-owner=payments", false},
		{"hs-subnet--apps", "hs-owner=payments", false},
		{"hs-owner", "hs-owner=payments", false},
		{"cost-center", "a=b", false},
	} {
		if got := IsOperatorEntry(tc.tag, tc.value); got != tc.want {
			t.Errorf("IsOperatorEntry(%q, %q) = %v, want %v", tc.tag, tc.value, got, tc.want)
		}
	}
}

// The command names one tag with the value it was read with, quoted for a shell, and there is
// none for a tag the operator would not have written.
func TestRemoveCommand(t *testing.T) {
	id := vnetID("hub")
	got := RemoveCommand(id, OrphanedEntry{Tag: "hs-subnet-old", Value: "hs-env=prod;hs-owner=team-a"})
	want := "az tag update --resource-id '" + id + "' --operation Delete --tags 'hs-subnet-old=hs-env=prod;hs-owner=team-a'"
	if got != want {
		t.Errorf("command = %s\nwant      %s", got, want)
	}
	quoted := RemoveCommand(id, OrphanedEntry{Tag: "hs-subnet-old", Value: "hs-owner=it's $(mine) `x`"})
	if !strings.HasSuffix(quoted, ` --tags 'hs-subnet-old=hs-owner=it'\''s $(mine) `+"`x`'") {
		t.Errorf("a value with a quote is not quoted for a shell: %s", quoted)
	}
	for _, e := range []OrphanedEntry{
		{Tag: "hs-subnet-old", Value: "owned by payments"},
		{Tag: "hs-owner", Value: "hs-owner=payments"},
		{Tag: "cost-center", Value: "a=b"},
		{Tag: "hs-subnet-", Value: "a=b"},
	} {
		if got := RemoveCommand(id, e); got != "" {
			t.Errorf("a command for %+v: %s", e, got)
		}
	}
}
