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
	"cmp"
	"net/netip"
	"slices"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
)

// The details of status.gcp.overlaps are capped so that a Network object stays small however
// many networks overlap it: every auto mode network, such as each project's default network,
// has the same ranges in every region, and a scope of many projects makes every one of them
// overlap every other. overlapsWith and hs_network_cidr_overlaps still count them all.
const (
	// maxOverlapRanges caps the pairs of ranges listed per pair of networks; rangeCount says
	// how many there are.
	maxOverlapRanges = 5
	// maxUnpeeredOverlaps caps the networks listed that are not peered with this one. Peered
	// ones are always listed: a network has few peerings, and they are what matters most.
	maxUnpeeredOverlaps = 50
)

// networkRange is one address range of a network: a CIDR block of its own, or a range of one
// of its subnets.
type networkRange struct {
	prefix  netip.Prefix
	cidr    string // as the object spells it
	network int    // index into the networks
	subnet  string // the subnet's ID, empty for the network's own block
}

// networkOverlap is another network that overlaps one: which, how many pairs of ranges
// overlap, and the first of them in address order. Networks that reuse one range everywhere,
// such as the same GKE pod range in every network, overlap in many pairs; only a few are kept.
type networkOverlap struct {
	other  int
	count  int
	pairs  []rangePair // while the ranges are compared
	ranges []networkv1.GCPOverlappingRange
}

// rangePair is a range of the network and a range of the other network that overlap, as
// indexes into the sorted ranges: comparing the indexes compares the ranges.
type rangePair struct{ own, other int }

// add counts a pair of ranges and keeps it if it is among the first maxOverlapRanges.
func (o *networkOverlap) add(p rangePair) {
	o.count++
	i, _ := slices.BinarySearchFunc(o.pairs, p, compareRangePairs)
	if i >= maxOverlapRanges {
		return
	}
	if o.pairs == nil {
		o.pairs = make([]rangePair, 0, maxOverlapRanges)
	}
	if len(o.pairs) == maxOverlapRanges {
		o.pairs = o.pairs[:maxOverlapRanges-1] // make room without growing
	}
	o.pairs = slices.Insert(o.pairs, i, p)
}

func compareRanges(a, b networkRange) int {
	return cmp.Or(a.prefix.Addr().Compare(b.prefix.Addr()), cmp.Compare(a.prefix.Bits(), b.prefix.Bits()),
		cmp.Compare(a.network, b.network), cmp.Compare(a.subnet, b.subnet))
}

func compareRangePairs(a, b rangePair) int {
	return cmp.Or(cmp.Compare(a.own, b.own), cmp.Compare(a.other, b.other))
}

// scopeOverlaps finds, for every network of a scope, the other networks whose ranges overlap
// its own: out[i] lists those of networks[i], by index, in no particular order.
//
// The ranges of a network are where its addresses live. On AWS they are the VPC's IPv4 CIDR
// blocks, as they have always been compared. A GCP VPC network has no range of its own, only
// its subnetworks have: their primary, secondary and IPv6 ranges are its ranges, and so is
// the range of a legacy network. Compute already refuses an overlap inside one network, so
// two ranges of the same network are never reported.
//
// The ranges are sorted by address; since two CIDR prefixes overlap only when one contains
// the other, each range is compared with the ranges that start inside it and no others, which
// keeps a scope of many networks and subnets far from comparing every pair.
func scopeOverlaps(networks []networkv1.Network, subnets []networkv1.Subnet) [][]networkOverlap {
	index := make(map[string]int, len(networks))
	for i := range networks {
		index[networks[i].Spec.ID] = i
	}
	var ranges []networkRange
	add := func(network int, subnet string, cidrs ...string) {
		for _, c := range cidrs {
			if p, err := netip.ParsePrefix(c); err == nil {
				ranges = append(ranges, networkRange{prefix: p.Masked(), cidr: c, network: network, subnet: subnet})
			}
		}
	}
	for i := range networks {
		add(i, "", networks[i].Status.CIDRBlocks...)
	}
	for k := range subnets {
		s := &subnets[k]
		i, ok := index[s.Spec.NetworkID]
		if !ok || networks[i].Spec.Provider != networkv1.ProviderGCP {
			continue
		}
		add(i, s.Spec.ID, s.Status.CIDRBlock)
		add(i, s.Spec.ID, s.Status.SecondaryCIDRBlocks...)
		add(i, s.Spec.ID, s.Status.IPv6CIDRBlocks...)
	}
	slices.SortFunc(ranges, compareRanges)

	byPair := make([]map[int]*networkOverlap, len(networks))
	record := func(own, other int) {
		a, b := ranges[own].network, ranges[other].network
		if byPair[a] == nil {
			byPair[a] = map[int]*networkOverlap{}
		}
		o := byPair[a][b]
		if o == nil {
			o = &networkOverlap{other: b}
			byPair[a][b] = o
		}
		o.add(rangePair{own: own, other: other})
	}
	for i, a := range ranges {
		for j := i + 1; j < len(ranges) && a.prefix.Contains(ranges[j].prefix.Addr()); j++ {
			if a.network != ranges[j].network {
				record(i, j)
				record(j, i)
			}
		}
	}

	out := make([][]networkOverlap, len(networks))
	for i, pairs := range byPair {
		for _, o := range pairs {
			for _, p := range o.pairs {
				own, other := ranges[p.own], ranges[p.other]
				o.ranges = append(o.ranges, networkv1.GCPOverlappingRange{CIDRBlock: own.cidr, Subnet: own.subnet,
					OtherCIDRBlock: other.cidr, OtherSubnet: other.subnet})
			}
			o.pairs = nil
			out[i] = append(out[i], *o)
		}
	}
	return out
}

// peeringState returns, for two networks, the state of the first one's peering with the
// second, or of the second's with the first when only that exists, and whether there is any.
func peeringState(a, b *networkv1.Network) (string, bool) {
	for _, pair := range [][2]*networkv1.Network{{a, b}, {b, a}} {
		if pair[0].Status.GCP == nil {
			continue
		}
		for _, p := range pair[0].Status.GCP.Peerings {
			if p.Network == pair[1].Spec.ID {
				return p.State, true
			}
		}
	}
	return "", false
}

// applyOverlaps writes what scopeOverlaps found into the networks' status: overlapsWith for
// every provider, and on GCP the details, with the peering between the two networks.
func applyOverlaps(networks []networkv1.Network, overlaps [][]networkOverlap) {
	for i := range networks {
		n := &networks[i]
		var refs []string
		var details []networkv1.GCPNetworkOverlap
		for _, o := range overlaps[i] {
			other := &networks[o.other]
			refs = append(refs, networkRef(other))
			if n.Spec.Provider != networkv1.ProviderGCP {
				continue
			}
			d := networkv1.GCPNetworkOverlap{Network: networkRef(other), RangeCount: int32(min(o.count, 1<<30)),
				Ranges: o.ranges}
			d.PeeringState, d.Peered = peeringState(n, other)
			details = append(details, d)
		}
		slices.Sort(refs)
		n.Status.OverlapsWith = refs
		slices.SortFunc(details, func(a, b networkv1.GCPNetworkOverlap) int { return cmp.Compare(a.Network, b.Network) })
		unpeered := 0
		details = slices.DeleteFunc(details, func(d networkv1.GCPNetworkOverlap) bool {
			if d.Peered {
				return false
			}
			unpeered++
			return unpeered > maxUnpeeredOverlaps
		})
		if n.Status.GCP != nil {
			n.Status.GCP.Overlaps = details
		}
	}
}
