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

package controller

import (
	"fmt"
	"reflect"
	"slices"
	"testing"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
)

func vpc(id, account string, cidrs ...string) networkv1.Network {
	return networkv1.Network{
		Spec:   networkv1.NetworkSpec{Provider: networkv1.ProviderAWS, ID: id, Account: account, Region: "eu-central-1"},
		Status: networkv1.NetworkStatus{CIDRBlocks: cidrs},
	}
}

func gcpNetwork(project, name string, peerings ...networkv1.GCPNetworkPeering) networkv1.Network {
	return networkv1.Network{
		Spec:   networkv1.NetworkSpec{Provider: networkv1.ProviderGCP, ID: "projects/" + project + "/global/networks/" + name, Account: project},
		Status: networkv1.NetworkStatus{Name: name, GCP: &networkv1.GCPNetworkStatus{Peerings: peerings}},
	}
}

func gcpSubnetwork(project, region, network, name, cidr string, secondary ...string) networkv1.Subnet {
	return networkv1.Subnet{
		Spec: networkv1.SubnetSpec{Provider: networkv1.ProviderGCP, Account: project, Region: region,
			ID:        "projects/" + project + "/regions/" + region + "/subnetworks/" + name,
			NetworkID: "projects/" + project + "/global/networks/" + network},
		Status: networkv1.SubnetStatus{CIDRBlock: cidr, SecondaryCIDRBlocks: secondary},
	}
}

func peering(project, network, state string) networkv1.GCPNetworkPeering {
	return networkv1.GCPNetworkPeering{Name: "to-" + network, Network: "projects/" + project + "/global/networks/" + network,
		State: state}
}

func overlapsOf(networks []networkv1.Network, subnets []networkv1.Subnet) []networkv1.Network {
	applyOverlaps(networks, scopeOverlaps(networks, subnets))
	return networks
}

// On AWS the VPCs' own blocks are compared, every pair of the scope, as they always were.
func TestOverlapsOfVPCsCompareTheirBlocks(t *testing.T) {
	got := overlapsOf([]networkv1.Network{
		vpc("vpc-hub", "111111111111", "10.0.0.0/16", "100.64.0.0/20"),
		vpc("vpc-spoke", "222222222222", "10.0.128.0/20"),
		vpc("vpc-far", "222222222222", "10.1.0.0/16", "100.64.8.0/24"),
		vpc("vpc-bad", "222222222222", "not a cidr"),
	}, nil)
	want := [][]string{
		{"222222222222/eu-central-1/vpc-far", "222222222222/eu-central-1/vpc-spoke"},
		{"111111111111/eu-central-1/vpc-hub"},
		{"111111111111/eu-central-1/vpc-hub"},
		nil,
	}
	for i := range got {
		if !slices.Equal(got[i].Status.OverlapsWith, want[i]) {
			t.Errorf("%s overlaps %v, want %v", got[i].Spec.ID, got[i].Status.OverlapsWith, want[i])
		}
		if got[i].Status.GCP != nil {
			t.Errorf("%s has GCP details", got[i].Spec.ID)
		}
	}
}

// A GCP network has no range of its own: its subnetworks' primary, secondary and IPv6 ranges
// are compared, across projects and regions, and a peering between the two is reported.
func TestOverlapsOfGCPNetworksCompareTheirSubnetworks(t *testing.T) {
	networks := []networkv1.Network{
		gcpNetwork("host", "shared", peering("apps", "apps", "ACTIVE"), peering("ml", "ml", "INACTIVE")),
		gcpNetwork("apps", "apps", peering("host", "shared", "ACTIVE")),
		gcpNetwork("ml", "ml"),
		gcpNetwork("sandbox", "scratch"),
		gcpNetwork("sandbox", "clean"),
	}
	v6 := gcpSubnetwork("sandbox", "us-central1", "clean", "v6", "10.99.0.0/24")
	v6.Status.IPv6CIDRBlocks = []string{"fd20:1::/64"}
	v6b := gcpSubnetwork("ml", "us-central1", "ml", "v6", "10.98.0.0/24")
	v6b.Status.IPv6CIDRBlocks = []string{"fd20:1::/64"}
	subnets := []networkv1.Subnet{
		gcpSubnetwork("host", "europe-west1", "shared", "gke", "10.0.0.0/24", "10.4.0.0/14"),
		gcpSubnetwork("host", "us-central1", "shared", "batch", "10.1.0.0/24"),
		// Primary against a secondary range, in another region and project.
		gcpSubnetwork("apps", "europe-west4", "apps", "web", "10.5.0.0/24"),
		// A one-sided peering: ml never peered back.
		gcpSubnetwork("ml", "europe-west1", "ml", "train", "10.1.0.0/20"),
		// Not peered with anyone: still reported, like two VPCs.
		gcpSubnetwork("sandbox", "europe-west1", "scratch", "s", "10.0.0.128/25"),
		v6, v6b,
	}
	got := overlapsOf(networks, subnets)

	shared := got[0].Status
	wantRefs := []string{"apps//projects/apps/global/networks/apps", "ml//projects/ml/global/networks/ml",
		"sandbox//projects/sandbox/global/networks/scratch"}
	if !slices.Equal(shared.OverlapsWith, wantRefs) {
		t.Errorf("shared overlaps %v, want %v", shared.OverlapsWith, wantRefs)
	}
	want := []networkv1.GCPNetworkOverlap{
		{Network: wantRefs[0], Peered: true, PeeringState: "ACTIVE", RangeCount: 1, Ranges: []networkv1.GCPOverlappingRange{
			{CIDRBlock: "10.4.0.0/14", Subnet: "projects/host/regions/europe-west1/subnetworks/gke",
				OtherCIDRBlock: "10.5.0.0/24", OtherSubnet: "projects/apps/regions/europe-west4/subnetworks/web"}}},
		{Network: wantRefs[1], Peered: true, PeeringState: "INACTIVE", RangeCount: 1, Ranges: []networkv1.GCPOverlappingRange{
			{CIDRBlock: "10.1.0.0/24", Subnet: "projects/host/regions/us-central1/subnetworks/batch",
				OtherCIDRBlock: "10.1.0.0/20", OtherSubnet: "projects/ml/regions/europe-west1/subnetworks/train"}}},
		{Network: wantRefs[2], RangeCount: 1, Ranges: []networkv1.GCPOverlappingRange{
			{CIDRBlock: "10.0.0.0/24", Subnet: "projects/host/regions/europe-west1/subnetworks/gke",
				OtherCIDRBlock: "10.0.0.128/25", OtherSubnet: "projects/sandbox/regions/europe-west1/subnetworks/s"}}},
	}
	if !reflect.DeepEqual(shared.GCP.Overlaps, want) {
		t.Errorf("shared overlaps:\n got %+v\nwant %+v", shared.GCP.Overlaps, want)
	}

	// The other side sees the same pair, from its end, with the peering it does not have.
	ml := got[2].Status.GCP.Overlaps
	if len(ml) != 2 || ml[0].Network != "host//projects/host/global/networks/shared" || !ml[0].Peered ||
		ml[0].PeeringState != "INACTIVE" || ml[0].Ranges[0].CIDRBlock != "10.1.0.0/20" {
		t.Errorf("ml overlaps = %+v", ml)
	}
	// IPv6 ranges count too.
	if ml[1].Network != "sandbox//projects/sandbox/global/networks/clean" || ml[1].Peered ||
		ml[1].Ranges[0].CIDRBlock != "fd20:1::/64" {
		t.Errorf("ml overlaps = %+v", ml)
	}
	if !slices.Equal(got[3].Status.OverlapsWith, []string{"host//projects/host/global/networks/shared"}) {
		t.Errorf("scratch overlaps %v", got[3].Status.OverlapsWith)
	}
}

// Two auto mode networks overlap in every region: every pair is counted, the first few listed.
func TestOverlapRangesAreCapped(t *testing.T) {
	networks := []networkv1.Network{gcpNetwork("a", "default"), gcpNetwork("b", "default")}
	subnets := make([]networkv1.Subnet, 0, 24)
	for i := range 12 {
		region := fmt.Sprintf("region-%02d", i)
		cidr := fmt.Sprintf("10.%d.0.0/20", 128+i)
		subnets = append(subnets, gcpSubnetwork("a", region, "default", "default", cidr),
			gcpSubnetwork("b", region, "default", "default", cidr))
	}
	got := overlapsOf(networks, subnets)[0].Status.GCP.Overlaps
	if len(got) != 1 || got[0].RangeCount != 12 || len(got[0].Ranges) != maxOverlapRanges ||
		got[0].Ranges[0].CIDRBlock != "10.128.0.0/20" || got[0].Ranges[4].CIDRBlock != "10.132.0.0/20" {
		t.Errorf("overlaps = %+v", got)
	}
}

// Default networks of many projects all overlap each other: overlapsWith lists every one,
// the details every peered one and the first others, so the object stays small.
func TestUnpeeredOverlapDetailsAreCapped(t *testing.T) {
	networks := []networkv1.Network{gcpNetwork("p000", "default", peering("p070", "default", "ACTIVE"))}
	subnets := []networkv1.Subnet{gcpSubnetwork("p000", "europe-west1", "default", "default", "10.132.0.0/20")}
	for i := 1; i <= 80; i++ {
		project := fmt.Sprintf("p%03d", i)
		networks = append(networks, gcpNetwork(project, "default"))
		subnets = append(subnets, gcpSubnetwork(project, "europe-west1", "default", "default", "10.132.0.0/20"))
	}
	got := overlapsOf(networks, subnets)[0].Status
	if len(got.OverlapsWith) != 80 {
		t.Errorf("overlapsWith has %d networks, want 80", len(got.OverlapsWith))
	}
	details := got.GCP.Overlaps
	if len(details) != maxUnpeeredOverlaps+1 {
		t.Fatalf("%d details, want %d unpeered and the peered one", len(details), maxUnpeeredOverlaps)
	}
	if !details[len(details)-1].Peered || details[len(details)-1].Network != "p070//projects/p070/global/networks/default" {
		t.Errorf("the peered network is missing: last is %+v", details[len(details)-1])
	}
	if details[0].Network != "p001//projects/p001/global/networks/default" {
		t.Errorf("first is %s", details[0].Network)
	}
}

// A network's own ranges never overlap each other in a report, and nothing is reported twice.
func TestOverlapsWithinOneNetworkAreNotReported(t *testing.T) {
	networks := []networkv1.Network{gcpNetwork("a", "one"), gcpNetwork("a", "two")}
	subnets := []networkv1.Subnet{
		gcpSubnetwork("a", "europe-west1", "one", "x", "10.0.0.0/16"),
		gcpSubnetwork("a", "europe-west1", "one", "y", "10.0.1.0/24"), // not what Compute allows, but harmless
		gcpSubnetwork("a", "europe-west1", "two", "z", "192.168.0.0/24"),
		gcpSubnetwork("a", "europe-west1", "gone", "orphan", "192.168.0.0/24"), // its network is not in the scope
	}
	for _, n := range overlapsOf(networks, subnets) {
		if len(n.Status.OverlapsWith) != 0 || len(n.Status.GCP.Overlaps) != 0 {
			t.Errorf("%s overlaps %v", n.Spec.ID, n.Status.OverlapsWith)
		}
	}
}

// A scope of thousands of subnets is sorted once, not compared pair by pair.
func BenchmarkScopeOverlaps(b *testing.B) {
	networks := make([]networkv1.Network, 0, 200)
	subnets := make([]networkv1.Subnet, 0, 200*20)
	for n := range 200 {
		name := fmt.Sprintf("n%d", n)
		networks = append(networks, gcpNetwork("p", name))
		for s := range 20 {
			subnets = append(subnets, gcpSubnetwork("p", "europe-west1", name, fmt.Sprintf("%s-%d", name, s),
				fmt.Sprintf("10.%d.%d.0/24", n%250, s), fmt.Sprintf("172.16.%d.0/24", s)))
		}
	}
	for b.Loop() {
		scopeOverlaps(networks, subnets)
	}
}
