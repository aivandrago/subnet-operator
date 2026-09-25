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

// Package providertest is the contract every provider must pass (#45, ADR 0002 §6): what
// discovery reports and in what shape, what a network selector selects, how unmanaged
// resources are reported, that unknown IP usage stays unknown, that throttling is reported as
// inventory.ErrThrottled and a taken CIDR as inventory.ErrCIDRConflict, that ownership writes
// only add, that the ownership model and ownershipSource agree, and that the tag keys the
// operator writes are valid for the provider.
//
// A provider runs it against a Fixture: the provider wired to a cloud the suite can arrange,
// usually an in-memory fake of the cloud's API, and where one exists an emulator too. The suite
// never assumes the account is empty: every case works with networks it created and tags it
// made up, so a shared emulator or a real sandbox account can run it as well.
package providertest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/validation/field"

	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

// T is what the suite needs of a test. *testing.T and Ginkgo's GinkgoT() both have it.
type T interface {
	Helper()
	Logf(format string, args ...any)
	Errorf(format string, args ...any)
	Fatalf(format string, args ...any)
	Skipf(format string, args ...any)
}

// Failure is a way the cloud's API can fail that the suite asks a fixture to simulate.
type Failure int

const (
	// Throttled is the API rate-limiting the caller for longer than the provider retries.
	Throttled Failure = iota
	// AccessDenied is the identity lacking a permission.
	AccessDenied
)

func (f Failure) String() string {
	switch f {
	case Throttled:
		return "throttling"
	case AccessDenied:
		return "access denied"
	}
	return fmt.Sprintf("failure %d", int(f))
}

// Fixture is a provider wired to a cloud the suite can arrange. The arranging methods act on
// the cloud directly, not through the provider, the way somebody with the console would.
type Fixture interface {
	// Provider is the provider under test.
	Provider() provider.Provider
	// Target is the account and region the suite works in, reached with the operator's own
	// identity, for reading and writing alike.
	Target() inventory.Target
	// Zone is the zone subnets are created in on a provider with zonal subnets, or "".
	Zone() string
	// CreateNetwork creates a network with the CIDR and tags and returns its ID.
	CreateNetwork(t T, cidr string, tags map[string]string) string
	// CreateSubnet creates a subnet with the CIDR and tags in the network (in Zone) and
	// returns its ID.
	CreateSubnet(t T, networkID, cidr string, tags map[string]string) string
	// MakeIPUsageUnknown makes the cloud stop reporting the free addresses of the subnet. It
	// returns false when the fixture cannot, and the case is skipped.
	MakeIPUsageUnknown(t T, subnetID string) bool
	// Fail makes every call to the cloud fail that way until restore is called. It returns
	// false when the fixture cannot, and the case is skipped.
	Fail(t T, f Failure) (restore func(), ok bool)
	// InvalidTags are tag sets the provider must refuse, each for its own reason.
	InvalidTags() []map[string]string
}

// Case is one clause of the contract.
type Case struct {
	Name string
	Run  func(t T, f Fixture)
}

// Cases returns the contract, in the order it is best read.
func Cases() []Case {
	return []Case{
		{"declares its name, capabilities and ownership model", declaresItself},
		{"reaches an account without its member with the operator's own identity", ownIdentityByDefault},
		{"reports networks and subnets in the neutral shape", discoveryShape},
		{"selects networks by all of their tags, an empty value matching any value", selectorSemantics},
		{"reports what the selector leaves out as unmanaged, and only then", unmanagedReporting},
		{"reports unknown IP usage as unknown, not as zero", unknownIPUsage},
		{"reports persistent throttling as ErrThrottled, and nothing else", throttling},
		{"creates subnets with their tags, and reports a taken CIDR as ErrCIDRConflict", createSubnet},
		{"writes ownership by adding, never removing", ownershipWritesOnlyAdd},
		{"accepts the tag keys the operator writes and refuses invalid tags", tagRules},
	}
}

// Run runs the contract with a fresh fixture per case.
func Run(t *testing.T, newFixture func(t *testing.T) Fixture) {
	for _, c := range Cases() {
		t.Run(c.Name, func(t *testing.T) {
			c.Run(t, newFixture(t))
		})
	}
}

// Values the cases write and then look for.
const (
	nameTag  = "Name"
	teamA    = "team-a"
	newOwner = "new"
	yes      = "yes"
	prod     = "prod"
)

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Minute)
}

// unique returns a short random string, so that tags and CIDRs of one run do not meet
// those of another in a shared account.
func unique(t T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	return hex.EncodeToString(b)
}

// netBlock returns a random 10.x.0.0/16 prefix as "10.x", for a network of the case's own.
func netBlock(t T) string {
	t.Helper()
	b := make([]byte, 1)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("random: %v", err)
	}
	return fmt.Sprintf("10.%d", 1+int(b[0])%250)
}

func discover(t T, f Fixture, selector map[string]string, unmanaged bool) *inventory.Snapshot {
	t.Helper()
	c, cancel := ctx()
	defer cancel()
	target := f.Target()
	target.NetworkSelector = selector
	target.DiscoverUnmanaged = unmanaged
	snap, err := f.Provider().Discover(c, target)
	if err != nil {
		t.Fatalf("Discover(%v, unmanaged=%v) failed: %v", selector, unmanaged, err)
	}
	if snap == nil {
		t.Fatalf("Discover returned no snapshot and no error")
	}
	return snap
}

func networkIDs(ns []inventory.Network) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.ID)
	}
	return out
}

func subnetIDs(ss []inventory.Subnet) []string {
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		out = append(out, s.ID)
	}
	return out
}

func findNetwork(ns []inventory.Network, id string) (inventory.Network, bool) {
	i := slices.IndexFunc(ns, func(n inventory.Network) bool { return n.ID == id })
	if i < 0 {
		return inventory.Network{}, false
	}
	return ns[i], true
}

func findSubnet(ss []inventory.Subnet, id string) (inventory.Subnet, bool) {
	i := slices.IndexFunc(ss, func(s inventory.Subnet) bool { return s.ID == id })
	if i < 0 {
		return inventory.Subnet{}, false
	}
	return ss[i], true
}

// keys are the ownership tag keys the operator uses by default on the provider.
func keys(f Fixture) networkv1beta1.TagKeys {
	return networkv1beta1.DefaultTagKeys(f.Provider().Name())
}

func declaresItself(t T, f Fixture) {
	p := f.Provider()
	if p.Name() == "" {
		t.Errorf("the provider has no name")
	}
	known := []networkv1beta1.Capability{networkv1beta1.CapabilityCreateSubnet,
		networkv1beta1.CapabilityChangeEvents, networkv1beta1.CapabilityIPUsage}
	seen := map[networkv1beta1.Capability]bool{}
	for _, c := range p.Capabilities() {
		if !slices.Contains(known, c) {
			t.Errorf("capability %q is not one the API defines", c)
		}
		if seen[c] {
			t.Errorf("capability %q is listed twice", c)
		}
		seen[c] = true
	}
	models := []networkv1beta1.OwnershipModel{networkv1beta1.OwnershipResourceTags,
		networkv1beta1.OwnershipParentNetworkTags}
	o := p.Ownership()
	if !slices.Contains(models, o.Networks) {
		t.Errorf("ownership model of networks %q is not one the API defines", o.Networks)
	}
	if !slices.Contains(models, o.Subnets) {
		t.Errorf("ownership model of subnets %q is not one the API defines", o.Subnets)
	}
	if o.Networks == networkv1beta1.OwnershipParentNetworkTags {
		t.Errorf("a network has no parent network to keep its metadata on")
	}
	if p.WriteIdentityField() == "" {
		t.Errorf("the provider does not name the field of its write identity")
	}
	if p.OwnershipPermission() == "" {
		t.Errorf("the provider does not name the permission ownership writes need")
	}
	if target := f.Target(); target.Provider != p.Name() {
		t.Errorf("the fixture's target is for provider %q, not %q", target.Provider, p.Name())
	}
}

func ownIdentityByDefault(t T, f Fixture) {
	p := f.Provider()
	account := networkv1beta1.Account{ID: f.Target().Account}
	for _, access := range []provider.Access{provider.Read, provider.Write} {
		id := p.Identity(account, access)
		if id != nil && !id.Own() {
			t.Errorf("an account without identity settings is reached as %s (access %d), not with the operator's own",
				id, access)
		}
	}
}

func discoveryShape(t T, f Fixture) {
	p := f.Provider()
	target := f.Target()
	k := keys(f)
	sel := "contract-" + unique(t)
	block := netBlock(t)
	netID := f.CreateNetwork(t, block+".0.0/16", map[string]string{sel: yes, nameTag: sel})
	tagged := f.CreateSubnet(t, netID, block+".1.0/24", map[string]string{k.Owner: teamA, nameTag: sel + "-a"})
	bare := f.CreateSubnet(t, netID, block+".2.0/24", nil)

	snap := discover(t, f, map[string]string{sel: yes}, false)
	if len(snap.UnmanagedNetworks) != 0 || len(snap.UnmanagedSubnets) != 0 {
		t.Errorf("unmanaged resources reported without DiscoverUnmanaged: %v %v",
			networkIDs(snap.UnmanagedNetworks), subnetIDs(snap.UnmanagedSubnets))
	}
	n, ok := findNetwork(snap.Networks, netID)
	if !ok {
		t.Fatalf("network %s is not in %v", netID, networkIDs(snap.Networks))
	}
	if n.Account != target.Account || n.Region != target.Region {
		t.Errorf("network %s is reported in %s/%s, not %s/%s", netID, n.Account, n.Region, target.Account, target.Region)
	}
	if !slices.Contains(n.CIDRBlocks, block+".0.0/16") {
		t.Errorf("network %s has CIDR blocks %v, missing %s.0.0/16", netID, n.CIDRBlocks, block)
	}
	if n.Tags[sel] != yes {
		t.Errorf("network %s has tags %v, missing %s=yes", netID, n.Tags, sel)
	}
	for _, id := range []string{tagged, bare} {
		s, ok := findSubnet(snap.Subnets, id)
		if !ok {
			t.Fatalf("subnet %s is not in %v", id, subnetIDs(snap.Subnets))
		}
		if s.NetworkID != netID || s.Account != target.Account || s.Region != target.Region {
			t.Errorf("subnet %s is reported in %s/%s/%s, not %s/%s/%s", id, s.Account, s.Region, s.NetworkID,
				target.Account, target.Region, netID)
		}
		if s.Zone != f.Zone() {
			t.Errorf("subnet %s is in zone %q, want %q", id, s.Zone, f.Zone())
		}
	}
	a, _ := findSubnet(snap.Subnets, tagged)
	if a.CIDRBlock != block+".1.0/24" {
		t.Errorf("subnet %s has CIDR %q, want %s.1.0/24", tagged, a.CIDRBlock, block)
	}
	if a.Tags[k.Owner] != teamA {
		t.Errorf("subnet %s has tags %v, missing %s=team-a", tagged, a.Tags, k.Owner)
	}

	ipUsage := provider.HasCapability(p, networkv1beta1.CapabilityIPUsage)
	seen := map[string]bool{}
	for _, s := range snap.Subnets {
		if seen[s.ID] {
			t.Errorf("subnet %s is reported twice", s.ID)
		}
		seen[s.ID] = true
		checkOwnershipSource(t, p, s)
		if s.TotalIPs != nil && *s.TotalIPs < 0 {
			t.Errorf("subnet %s has %d addresses", s.ID, *s.TotalIPs)
		}
		if s.AvailableIPs != nil && s.TotalIPs != nil && *s.AvailableIPs > *s.TotalIPs {
			t.Errorf("subnet %s has %d free of %d addresses", s.ID, *s.AvailableIPs, *s.TotalIPs)
		}
		if !ipUsage && s.AvailableIPs != nil {
			t.Errorf("subnet %s reports free addresses, but the provider does not declare IPUsage", s.ID)
		}
	}
	// A fresh, empty subnet on a provider that reports usage has a known number of free
	// addresses; that is what IPUsage promises.
	if ipUsage {
		for _, id := range []string{tagged, bare} {
			s, _ := findSubnet(snap.Subnets, id)
			if s.AvailableIPs == nil || s.TotalIPs == nil {
				t.Errorf("subnet %s does not report its addresses (total %v, free %v) although the provider declares "+
					"IPUsage", id, s.TotalIPs, s.AvailableIPs)
			}
		}
	}
}

// checkOwnershipSource: every subnet says where its ownership values came from, and the
// answer fits the model the provider declares. Under ResourceTags a subnet's metadata is its
// own; only under ParentNetworkTags can it be inherited from the network.
func checkOwnershipSource(t T, p provider.Provider, s inventory.Subnet) {
	t.Helper()
	switch s.OwnershipSource {
	case networkv1beta1.OwnershipSourceSubnet:
	case networkv1beta1.OwnershipSourceNetwork:
		if p.Ownership().Subnets != networkv1beta1.OwnershipParentNetworkTags {
			t.Errorf("subnet %s inherits its ownership from the network, but subnets use the %s model",
				s.ID, p.Ownership().Subnets)
		}
	default:
		t.Errorf("subnet %s has ownershipSource %q, want Subnet or Network", s.ID, s.OwnershipSource)
	}
}

func selectorSemantics(t T, f Fixture) {
	k1, k2 := "contract-"+unique(t), "contract-"+unique(t)
	a := f.CreateNetwork(t, netBlock(t)+".0.0/16", map[string]string{k1: "v"})
	b := f.CreateNetwork(t, netBlock(t)+".0.0/16", map[string]string{k1: "w"})
	c := f.CreateNetwork(t, netBlock(t)+".0.0/16", map[string]string{k2: "v"})
	d := f.CreateNetwork(t, netBlock(t)+".0.0/16", nil)
	all := map[string]string{a: "A {k1: v}", b: "B {k1: w}", c: "C {k2: v}", d: "D (no tags)"}

	for _, tc := range []struct {
		name     string
		selector map[string]string
		want     []string
	}{
		{"a value selects that value only", map[string]string{k1: "v"}, []string{a}},
		{"an empty value selects any value of the key", map[string]string{k1: ""}, []string{a, b}},
		{"every entry must match", map[string]string{k1: "v", k2: "v"}, nil},
		{"no selector selects every network", nil, []string{a, b, c, d}},
	} {
		got := networkIDs(discover(t, f, tc.selector, false).Networks)
		for id, label := range all {
			if want := slices.Contains(tc.want, id); slices.Contains(got, id) != want {
				t.Errorf("%s: network %s selected = %v, want %v", tc.name, label, !want, want)
			}
		}
	}
}

func unmanagedReporting(t T, f Fixture) {
	sel := "contract-" + unique(t)
	blockA, blockD := netBlock(t), netBlock(t)
	a := f.CreateNetwork(t, blockA+".0.0/16", map[string]string{sel: yes})
	sa := f.CreateSubnet(t, a, blockA+".1.0/24", nil)
	d := f.CreateNetwork(t, blockD+".0.0/16", nil)
	sd := f.CreateSubnet(t, d, blockD+".1.0/24", nil)

	snap := discover(t, f, map[string]string{sel: yes}, true)
	check := func(what string, ids []string, id string, want bool) {
		t.Helper()
		if slices.Contains(ids, id) != want {
			t.Errorf("%s contains %s = %v, want %v (%v)", what, id, !want, want, ids)
		}
	}
	check("networks", networkIDs(snap.Networks), a, true)
	check("networks", networkIDs(snap.Networks), d, false)
	check("unmanaged networks", networkIDs(snap.UnmanagedNetworks), d, true)
	check("unmanaged networks", networkIDs(snap.UnmanagedNetworks), a, false)
	check("subnets", subnetIDs(snap.Subnets), sa, true)
	check("subnets", subnetIDs(snap.Subnets), sd, false)
	check("unmanaged subnets", subnetIDs(snap.UnmanagedSubnets), sd, true)
	check("unmanaged subnets", subnetIDs(snap.UnmanagedSubnets), sa, false)
	for _, s := range snap.UnmanagedSubnets {
		if s.NetworkID == a {
			t.Errorf("unmanaged subnet %s belongs to the selected network %s", s.ID, a)
		}
	}

	off := discover(t, f, map[string]string{sel: yes}, false)
	if len(off.UnmanagedNetworks) != 0 || len(off.UnmanagedSubnets) != 0 {
		t.Errorf("unmanaged resources reported without DiscoverUnmanaged: %v %v",
			networkIDs(off.UnmanagedNetworks), subnetIDs(off.UnmanagedSubnets))
	}
	// Without a selector every network is in scope, so nothing is left over to report.
	everything := discover(t, f, nil, true)
	if len(everything.UnmanagedNetworks) != 0 || len(everything.UnmanagedSubnets) != 0 {
		t.Errorf("unmanaged resources reported without a selector: %v %v",
			networkIDs(everything.UnmanagedNetworks), subnetIDs(everything.UnmanagedSubnets))
	}
}

func unknownIPUsage(t T, f Fixture) {
	block := netBlock(t)
	n := f.CreateNetwork(t, block+".0.0/16", nil)
	s := f.CreateSubnet(t, n, block+".1.0/24", nil)
	if !f.MakeIPUsageUnknown(t, s) {
		t.Skipf("the fixture cannot make IP usage unknown")
	}
	got, ok := findSubnet(discover(t, f, nil, false).Subnets, s)
	if !ok {
		t.Fatalf("subnet %s is not discovered", s)
	}
	if got.AvailableIPs != nil {
		t.Errorf("subnet %s reports %d free addresses although the cloud did not say; unknown must stay nil",
			s, *got.AvailableIPs)
	}
}

func throttling(t T, f Fixture) {
	for _, tc := range []struct {
		failure   Failure
		throttled bool
	}{{Throttled, true}, {AccessDenied, false}} {
		restore, ok := f.Fail(t, tc.failure)
		if !ok {
			t.Logf("the fixture cannot simulate %s", tc.failure)
			continue
		}
		c, cancel := ctx()
		_, err := f.Provider().Discover(c, f.Target())
		cancel()
		restore()
		switch {
		case err == nil:
			t.Errorf("%s: discovery succeeded", tc.failure)
		case errors.Is(err, inventory.ErrThrottled) != tc.throttled:
			t.Errorf("%s: errors.Is(%v, ErrThrottled) = %v, want %v", tc.failure, err, !tc.throttled, tc.throttled)
		}
	}
}

func createSubnet(t T, f Fixture) {
	p := f.Provider()
	k := keys(f)
	block := netBlock(t)
	n := f.CreateNetwork(t, block+".0.0/16", nil)
	req := inventory.CreateSubnetRequest{NetworkID: n, CIDRBlock: block + ".8.0/24", Zone: f.Zone(),
		Tags: map[string]string{k.Owner: "team-c", nameTag: "contract-" + unique(t)}}
	c, cancel := ctx()
	defer cancel()

	if !provider.HasCapability(p, networkv1beta1.CapabilityCreateSubnet) {
		if _, err := p.CreateSubnet(c, f.Target(), req); !errors.Is(err, inventory.ErrNotSupported) {
			t.Errorf("the provider does not declare CreateSubnet, but CreateSubnet returned %v, not ErrNotSupported", err)
		}
		return
	}
	id, err := p.CreateSubnet(c, f.Target(), req)
	if err != nil || id == "" {
		t.Fatalf("CreateSubnet = %q, %v", id, err)
	}
	s, ok := findSubnet(discover(t, f, nil, false).Subnets, id)
	if !ok {
		t.Fatalf("the created subnet %s is not discovered", id)
	}
	if s.NetworkID != n || s.CIDRBlock != req.CIDRBlock || s.Zone != f.Zone() {
		t.Errorf("created subnet is %s/%s in zone %q, want %s/%s in %q", s.NetworkID, s.CIDRBlock, s.Zone,
			n, req.CIDRBlock, f.Zone())
	}
	if s.Tags[k.Owner] != "team-c" {
		t.Errorf("created subnet has tags %v, missing %s=team-c", s.Tags, k.Owner)
	}
	if s.OwnershipSource != networkv1beta1.OwnershipSourceSubnet {
		t.Errorf("a subnet created with its own tags reports ownershipSource %q, want Subnet", s.OwnershipSource)
	}

	for _, cidr := range []string{req.CIDRBlock, block + ".8.0/25", "172.31.254.0/24"} {
		clash := req
		clash.CIDRBlock = cidr
		clash.Tags = nil
		if _, err := p.CreateSubnet(c, f.Target(), clash); !errors.Is(err, inventory.ErrCIDRConflict) {
			t.Errorf("CreateSubnet(%s) in a network with %s taken, inside %s.0.0/16: err = %v, want ErrCIDRConflict",
				cidr, req.CIDRBlock, block, err)
		}
	}
}

func ownershipWritesOnlyAdd(t T, f Fixture) {
	p := f.Provider()
	k := keys(f)
	mark := "contract-" + unique(t)
	block := netBlock(t)
	n := f.CreateNetwork(t, block+".0.0/16", map[string]string{mark: "network"})
	s := f.CreateSubnet(t, n, block+".1.0/24", map[string]string{nameTag: mark, mark: "keep", k.Owner: "old"})
	c, cancel := ctx()
	defer cancel()

	write := func(id string, tags map[string]string) {
		t.Helper()
		if err := p.WriteOwnership(c, f.Target(), id, tags); err != nil {
			t.Fatalf("WriteOwnership(%s, %v) failed: %v", id, tags, err)
		}
	}
	write(s, map[string]string{k.Owner: newOwner, k.Env: prod})
	write(s, map[string]string{k.Owner: newOwner, k.Env: prod}) // the same again changes nothing
	write(s, nil)                                               // and nothing at all is not an error
	write(n, map[string]string{k.Owner: "network-team"})

	snap := discover(t, f, map[string]string{mark: ""}, false)
	got, ok := findSubnet(snap.Subnets, s)
	if !ok {
		t.Fatalf("subnet %s is not discovered", s)
	}
	for key, want := range map[string]string{nameTag: mark, mark: "keep", k.Owner: newOwner, k.Env: prod} {
		if got.Tags[key] != want {
			t.Errorf("subnet %s: %s = %q after the write, want %q (tags %v)", s, key, got.Tags[key], want, got.Tags)
		}
	}
	if got.OwnershipSource != networkv1beta1.OwnershipSourceSubnet {
		t.Errorf("a subnet with ownership of its own reports ownershipSource %q, want Subnet", got.OwnershipSource)
	}
	net, ok := findNetwork(snap.Networks, n)
	if !ok {
		t.Fatalf("network %s is not discovered", n)
	}
	if net.Tags[mark] != "network" || net.Tags[k.Owner] != "network-team" {
		t.Errorf("network %s has tags %v after the write, want %s=network and %s=network-team", n, net.Tags, mark, k.Owner)
	}
}

func tagRules(t T, f Fixture) {
	p := f.Provider()
	k := keys(f)
	// What the operator itself writes, on every provider: the scope's default keys, the
	// auto-import policy's managed marker, and what a created subnet is tagged with. A provider
	// that cannot carry one of these needs provider-dependent keys before it can ship.
	written := map[string]string{
		k.Owner: teamA, k.Env: prod, k.Tier: "private",
		networkv1beta1.DefaultManagedTag: networkv1beta1.DefaultManagedValue,
		networkv1beta1.TagManagedBy:      networkv1beta1.TagManagedByValue,
		networkv1beta1.TagClaim:          "team-a/claim",
		nameTag:                          "claim-a",
	}
	if errs := p.ValidateTags(field.NewPath("tags"), written); len(errs) > 0 {
		t.Errorf("the provider refuses tags the operator writes: %v", errs.ToAggregate())
	}
	invalid := f.InvalidTags()
	if len(invalid) == 0 {
		t.Errorf("the fixture names no invalid tags; every provider has rules")
	}
	for _, tags := range invalid {
		if errs := p.ValidateTags(field.NewPath("tags"), tags); len(errs) == 0 {
			t.Errorf("the provider accepts tags it must refuse: %s", formatTags(tags))
		}
	}
}

func formatTags(tags map[string]string) string {
	parts := make([]string, 0, len(tags))
	for k, v := range tags {
		if len(v) > 20 {
			v = v[:20] + "…"
		}
		if len(k) > 20 {
			k = k[:20] + "…"
		}
		parts = append(parts, k+"="+v)
	}
	slices.Sort(parts)
	return strings.Join(parts, ", ")
}
