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
	"strings"
	"sync"
	"testing"
	"time"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/cloud/azure/azurefake"
	"hypersurgery.dev/subnet-operator/internal/cloud/azure/events"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// One listing per subscription and sync (#118).

var sharingLocations = []string{"westeurope", "northeurope", "swedencentral", "uksouth"}

// sharingFake is a subscription with two virtual networks of two subnets in each of four
// locations: eight networks, which the fake lists in four pages.
func sharingFake(t *testing.T, opts Options) (*Provider, *azurefake.Cloud) {
	t.Helper()
	p, cloud := newFakeProvider(t, opts)
	for i, location := range sharingLocations {
		for j := range 2 {
			name := fmt.Sprintf("%s-%d", location, j)
			cloud.AddVirtualNetwork(contractSubscription, testGroup, name, location,
				[]string{fmt.Sprintf("10.%d.0.0/16", 10*i+j)}, nil)
			cloud.AddSubnet(contractSubscription, testGroup, name, "apps", fmt.Sprintf("10.%d.1.0/24", 10*i+j))
			cloud.AddSubnet(contractSubscription, testGroup, name, "data", fmt.Sprintf("10.%d.2.0/24", 10*i+j))
		}
	}
	cloud.Calls()
	return p, cloud
}

func targetsIn(locations ...string) []inventory.Target {
	out := make([]inventory.Target, 0, len(locations))
	for _, l := range locations {
		t := contractTarget()
		t.Region = l
		out = append(out, t)
	}
	return out
}

type discovery struct {
	snapshot *inventory.Snapshot
	err      error
}

// syncTargets discovers the targets side by side as one sync, the way the NetworkScope
// controller does, and returns each one's result. started, when set, is called once all of
// them run.
func syncTargets(p *Provider, targets []inventory.Target, started func()) []discovery {
	ctx, end := inventory.WithSync(context.Background())
	defer end()
	out := make([]discovery, len(targets))
	var wg sync.WaitGroup
	for i, target := range targets {
		wg.Go(func() {
			snap, err := p.Discover(ctx, target)
			out[i] = discovery{snap, err}
		})
	}
	if started != nil {
		started()
	}
	wg.Wait()
	return out
}

// eventually waits for the condition, which the test cannot be told about.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !cond(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// listingPages is how many requests one listing of the sharing fake's eight networks is.
const listingPages = 4

// Four locations of a subscription that start at the same moment are answered by one listing:
// one lists, the others wait for it, and each gets the networks of its own location.
func TestTargetsOfASyncShareOneListing(t *testing.T) {
	p, cloud := sharingFake(t, Options{})
	release := cloud.Hold(azurefake.ListAll)
	results := syncTargets(p, targetsIn(sharingLocations...), func() {
		// The first request of the listing has arrived and is held: every other target has
		// time to start, and to list for itself if it would.
		eventually(t, "the listing", func() bool { return cloud.CallsSoFar()[azurefake.ListAll] > 0 })
		time.Sleep(100 * time.Millisecond)
		release()
	})
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("%s: %v", sharingLocations[i], r.err)
		}
		if len(r.snapshot.Networks) != 2 || len(r.snapshot.Subnets) != 4 {
			t.Errorf("%s: %d networks and %d subnets, want its own 2 and 4", sharingLocations[i],
				len(r.snapshot.Networks), len(r.snapshot.Subnets))
		}
		for _, n := range r.snapshot.Networks {
			if n.Region != sharingLocations[i] || !strings.HasPrefix(n.Name, sharingLocations[i]) {
				t.Errorf("%s reports network %s of %s", sharingLocations[i], n.Name, n.Region)
			}
		}
		for _, s := range r.snapshot.Subnets {
			if s.AvailableIPs == nil || *s.AvailableIPs != 251 {
				t.Errorf("%s: subnet %s has %v free addresses, want its usage read", sharingLocations[i], s.ID, s.AvailableIPs)
			}
		}
	}
	calls := cloud.Calls()
	if calls[azurefake.ListAll] != listingPages {
		t.Errorf("%d List All requests for four locations, want the %d pages of one listing",
			calls[azurefake.ListAll], listingPages)
	}
	if calls[azurefake.ListUsage] != 8 {
		t.Errorf("%d usage requests, want one per virtual network (8)", calls[azurefake.ListUsage])
	}
}

// The Resource Manager requests of one full sync of a subscription in 1, 2 and 4 locations, as
// docs/operations/limits.md has them: listed once per location before 3.2 (which is what a
// discovery outside a sync still does), once per sync now.
func TestRequestsPerSyncByLocations(t *testing.T) {
	for _, tc := range []struct {
		locations                 int
		before, after, usageReads int
	}{
		{locations: 1, before: 4, after: 4, usageReads: 2},
		{locations: 2, before: 8, after: 4, usageReads: 4},
		{locations: 4, before: 16, after: 4, usageReads: 8},
	} {
		p, cloud := sharingFake(t, Options{})
		targets := targetsIn(sharingLocations[:tc.locations]...)
		for _, target := range targets {
			if _, err := p.Discover(context.Background(), target); err != nil {
				t.Fatal(err)
			}
		}
		if calls := cloud.Calls(); calls[azurefake.ListAll] != tc.before || calls[azurefake.ListUsage] != tc.usageReads {
			t.Errorf("%d locations, each listing for itself: %d list and %d usage requests, want %d and %d",
				tc.locations, calls[azurefake.ListAll], calls[azurefake.ListUsage], tc.before, tc.usageReads)
		}
		for _, r := range syncTargets(p, targets, nil) {
			if r.err != nil {
				t.Fatal(r.err)
			}
		}
		calls := cloud.Calls()
		if calls[azurefake.ListAll] != tc.after || calls[azurefake.ListUsage] != tc.usageReads {
			t.Errorf("%d locations in one sync: %d list and %d usage requests, want %d and %d",
				tc.locations, calls[azurefake.ListAll], calls[azurefake.ListUsage], tc.after, tc.usageReads)
		}
		if calls[azurefake.ListLocations] != 0 {
			t.Errorf("%d locations: %d locations requests for locations that have networks", tc.locations,
				calls[azurefake.ListLocations])
		}
	}
}

// A listing is one sync's: the next sync lists again and sees what changed, and so does a sync
// of one target only, which is what a change event's resync and a retry after a backoff are.
func TestEverySyncListsAfresh(t *testing.T) {
	p, cloud := sharingFake(t, Options{})
	targets := targetsIn(sharingLocations...)
	syncTargets(p, targets, nil)
	cloud.Calls()

	cloud.AddVirtualNetwork(contractSubscription, testGroup, "new", "uksouth", []string{"10.200.0.0/16"}, nil)
	cloud.RemoveVirtualNetwork(contractSubscription, testGroup, "westeurope-0")
	results := syncTargets(p, targets, nil)
	if n := len(results[3].snapshot.Networks); n != 3 {
		t.Errorf("the next sync reports %d networks in uksouth, want the new one too (3)", n)
	}
	if n := len(results[0].snapshot.Networks); n != 1 {
		t.Errorf("the next sync reports %d networks in westeurope, want the deleted one gone (1)", n)
	}
	if calls := cloud.Calls(); calls[azurefake.ListAll] != listingPages {
		t.Errorf("the next sync made %d List All requests, want a listing of its own (%d)", calls[azurefake.ListAll],
			listingPages)
	}

	cloud.AddSubnet(contractSubscription, testGroup, "new", "apps", "10.200.1.0/24")
	results = syncTargets(p, targets[3:], nil)
	if n := len(results[0].snapshot.Subnets); n != 5 {
		t.Errorf("a resync of uksouth alone reports %d subnets, want the new one too (5)", n)
	}
	if calls := cloud.Calls(); calls[azurefake.ListAll] != listingPages {
		t.Errorf("a resync of one target made %d List All requests, want a listing of its own (%d)",
			calls[azurefake.ListAll], listingPages)
	}
}

// A throttled listing throttles every location of the subscription, after the retries of one
// call and not of one per location; each reports it as its own, and a later sync, of all of
// them or of one alone after its backoff, lists again and is not answered from the failure.
func TestAThrottledListingThrottlesEveryTargetOfTheSync(t *testing.T) {
	var mu sync.Mutex
	throttled := map[string]int{}
	p, cloud := sharingFake(t, Options{OnThrottle: func(target inventory.Target, op string) {
		mu.Lock()
		defer mu.Unlock()
		throttled[op]++
	}})
	targets := targetsIn(sharingLocations...)
	cloud.FailEndpoint(azurefake.ListAll, azurefake.Throttle)
	for i, r := range syncTargets(p, targets, nil) {
		if !errors.Is(r.err, inventory.ErrThrottled) || !strings.Contains(r.err.Error(), "SubscriptionRequestsThrottled") {
			t.Errorf("%s: %v, want ErrThrottled with ARM's code", sharingLocations[i], r.err)
		}
	}
	if calls := cloud.Calls(); calls[azurefake.ListAll] != retryMaxAttempts || calls[azurefake.ListUsage] != 0 {
		t.Errorf("%d List All and %d usage requests, want the %d attempts of one call and no usages",
			calls[azurefake.ListAll], calls[azurefake.ListUsage], retryMaxAttempts)
	}
	if throttled["virtualNetworks.listAll"] != retryMaxAttempts {
		t.Errorf("OnThrottle = %v, want every throttled attempt once", throttled)
	}

	// One target comes back alone after its backoff, while ARM still throttles: its own call.
	if r := syncTargets(p, targets[1:2], nil)[0]; !errors.Is(r.err, inventory.ErrThrottled) {
		t.Errorf("the retry of one target: %v, want ErrThrottled", r.err)
	}
	if calls := cloud.Calls(); calls[azurefake.ListAll] != retryMaxAttempts {
		t.Errorf("the retry made %d List All requests, want its own %d attempts", calls[azurefake.ListAll], retryMaxAttempts)
	}

	// And once ARM answers again, the retry of one target is answered by ARM.
	cloud.FailEndpoint(azurefake.ListAll, azurefake.None)
	if r := syncTargets(p, targets[1:2], nil)[0]; r.err != nil || len(r.snapshot.Networks) != 2 {
		t.Errorf("the retry of one target after the throttling: %+v, %v", r.snapshot, r.err)
	}
	for i, r := range syncTargets(p, targets, nil) {
		if r.err != nil || len(r.snapshot.Networks) != 2 {
			t.Errorf("%s after the throttling: %+v, %v", sharingLocations[i], r.snapshot, r.err)
		}
	}
}

// A listing that fails fails every location of the subscription with the error ARM gave, each
// as it would have alone, and the next sync recovers.
func TestAFailedListingFailsEveryTargetOfTheSync(t *testing.T) {
	p, cloud := sharingFake(t, Options{})
	targets := targetsIn(sharingLocations...)
	cloud.FailEndpoint(azurefake.ListAll, azurefake.Deny)
	for i, r := range syncTargets(p, targets, nil) {
		if r.err == nil || errors.Is(r.err, inventory.ErrThrottled) ||
			!strings.Contains(r.err.Error(), "list virtual networks: ") || !strings.Contains(r.err.Error(), "AuthorizationFailed") {
			t.Errorf("%s: %v, want the listing's AuthorizationFailed", sharingLocations[i], r.err)
		}
	}
	if calls := cloud.Calls(); calls[azurefake.ListAll] != 1 {
		t.Errorf("%d List All requests, want the one that was refused", calls[azurefake.ListAll])
	}
	cloud.FailEndpoint(azurefake.ListAll, azurefake.None)
	for i, r := range syncTargets(p, targets, nil) {
		if r.err != nil || len(r.snapshot.Networks) != 2 {
			t.Errorf("%s after the role was assigned: %+v, %v", sharingLocations[i], r.snapshot, r.err)
		}
	}
}

// A listing is shared by the locations of one subscription as one scope reads it, and by
// nobody else: not by another scope, another identity, or other resource groups.
func TestAListingIsNotSharedAcrossScopesIdentitiesOrResourceGroups(t *testing.T) {
	const reader = "33333333-cccc-4000-8000-0000000000ad"
	p, cloud := sharingFake(t, Options{})
	cloud.AddIdentity(azurefake.DefaultTenant, reader, true)
	cloud.AddVirtualNetwork(contractSubscription, "rg-other", "spoke", "westeurope", []string{"10.250.0.0/16"}, nil)
	cloud.Calls()

	west, north := targetsIn("westeurope")[0], targetsIn("northeurope")[0]
	otherScope := north
	otherScope.Scope = "another-scope"
	asReader := north
	asReader.Identity = Identity{TenantID: azurefake.DefaultTenant, ClientID: reader}
	group, sameGroup, otherGroup := west, north, north
	group.Azure = &networkv1.AzureScope{ResourceGroups: []string{testGroup}}
	sameGroup.Azure = &networkv1.AzureScope{ResourceGroups: []string{strings.ToLower(testGroup)}}
	otherGroup.Azure = &networkv1.AzureScope{ResourceGroups: []string{"rg-other"}}

	results := syncTargets(p, []inventory.Target{west, north, otherScope, asReader, group, sameGroup, otherGroup}, nil)
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("target %d: %v", i, r.err)
		}
	}
	if n := len(results[0].snapshot.Networks); n != 3 {
		t.Errorf("westeurope of the whole subscription reports %d networks, want 3", n)
	}
	if n := len(results[4].snapshot.Networks); n != 2 {
		t.Errorf("westeurope of %s reports %d networks, want its 2 and not the whole subscription's", testGroup, n)
	}
	if n := len(results[6].snapshot.Networks); n != 0 {
		t.Errorf("northeurope of rg-other reports %d networks, want none", n)
	}
	calls := cloud.Calls()
	// The whole subscription: once for the scope's two locations, once for the other scope and
	// once as the other identity, nine networks in five pages each.
	if calls[azurefake.ListAll] != 3*5 {
		t.Errorf("%d List All requests, want three listings of five pages", calls[azurefake.ListAll])
	}
	// Resource groups: one listing for the two targets of the same group (in any case), of four
	// pages, and one for the other group.
	if calls[azurefake.ListGroup] != 4+1 {
		t.Errorf("%d List requests, want one listing of %s and one of rg-other (5)", calls[azurefake.ListGroup], testGroup)
	}
	if n := len(cloud.RequestsBy(reader)); n == 0 {
		t.Error("the other identity made no request of its own")
	}
}

// What a change event is looked up in (changes.go) is what the shared listing saw, for every
// location of the sync; a listing that failed leaves it alone, and the next one that succeeds
// forgets a network that is gone.
func TestTheSharedListingIsWhatChangeEventsAreLookedUpIn(t *testing.T) {
	p, cloud := sharingFake(t, Options{})
	d := p.discoverer
	targets := targetsIn(sharingLocations...)
	locate := func(name string) string {
		t.Helper()
		key, ok := d.locate(events.Change{Subscription: contractSubscription, NetworkID: "/subscriptions/" +
			contractSubscription + "/resourceGroups/" + testGroup + "/providers/Microsoft.Network/virtualNetworks/" + name})
		if !ok {
			t.Fatalf("%s: the subscription is not watched", name)
		}
		return key.Region
	}
	syncTargets(p, targets, nil)
	for _, location := range sharingLocations {
		if got := locate(location + "-1"); got != location {
			t.Errorf("a change to %s-1 resyncs %q, want %s", location, got, location)
		}
	}

	cloud.RemoveVirtualNetwork(contractSubscription, testGroup, "uksouth-0")
	cloud.FailEndpoint(azurefake.ListAll, azurefake.Deny)
	syncTargets(p, targets, nil)
	if got := locate("uksouth-0"); got != "uksouth" {
		t.Errorf("after a failed listing a change to uksouth-0 resyncs %q, want what was known (uksouth)", got)
	}
	cloud.FailEndpoint(azurefake.ListAll, azurefake.None)
	cloud.AddVirtualNetwork(contractSubscription, testGroup, "moved", "northeurope", []string{"10.210.0.0/16"}, nil)
	syncTargets(p, targets[:1], nil)
	if got := locate("uksouth-0"); got != "" {
		t.Errorf("a deleted network is still known in %q", got)
	}
	if got := locate("moved"); got != "northeurope" {
		t.Errorf("a network a sync of another location listed resyncs %q, want northeurope", got)
	}
}

// The listing of `manager azure-orphaned-entries` (orphans.go) is a read of its own even inside
// a sync: it is never answered with what a sync's targets share, nor shared with them.
func TestOwnershipEntriesAreNeverReadFromASyncsListing(t *testing.T) {
	p, cloud := sharingFake(t, Options{})
	ctx, end := inventory.WithSync(context.Background())
	defer end()
	target := contractTarget()
	if _, err := p.Discover(ctx, target); err != nil {
		t.Fatal(err)
	}
	cloud.Calls()
	cloud.AddVirtualNetwork(contractSubscription, testGroup, "late", "westeurope", []string{"10.220.0.0/16"}, nil)
	for range 2 {
		networks, err := p.OwnershipEntries(ctx, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(networks) != 9 {
			t.Errorf("%d networks reported, want the 9 there are now", len(networks))
		}
	}
	if calls := cloud.Calls(); calls[azurefake.ListAll] != 2*5 {
		t.Errorf("%d List All requests for two reads, want two listings of five pages", calls[azurefake.ListAll])
	}
}
