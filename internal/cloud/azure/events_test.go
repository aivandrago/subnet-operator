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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/cloud/azure/azurefake"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

const (
	eventsQueue  = "subnet-operator-events"
	writeSuccess = "Microsoft.Resources.ResourceWriteSuccess"
)

// ChangeEvents is only claimed when there is a queue to read them from.
func TestChangeEventsNeedAQueue(t *testing.T) {
	without := NewProvider(Options{})
	if slices.Contains(without.Capabilities(), networkv1.CapabilityChangeEvents) {
		t.Errorf("capabilities without a queue: %v", without.Capabilities())
	}
	if without.Events(provider.EventSink{}) != nil {
		t.Error("an event source without a queue")
	}
	with := NewProvider(Options{EventsQueueURL: "https://netops.queue.core.windows.net/" + eventsQueue})
	if !slices.Contains(with.Capabilities(), networkv1.CapabilityChangeEvents) {
		t.Errorf("capabilities with a queue: %v", with.Capabilities())
	}
	if with.Events(provider.EventSink{}) == nil {
		t.Error("no event source with a queue")
	}
}

// A queue URL that cannot work, or that carries a storage secret, stops the start.
func TestAMalformedQueueURLStopsTheStart(t *testing.T) {
	for _, bad := range []string{eventsQueue, "https://netops.queue.core.windows.net/q?sv=2024-11-04&sig=secret"} {
		if _, err := NewFromEnvironment(t.Context(), Options{EventsQueueURL: bad}); err == nil ||
			!strings.Contains(err.Error(), "change events queue") {
			t.Errorf("%s: err = %v", bad, err)
		}
	}
	if _, err := NewFromEnvironment(t.Context(), Options{
		EventsQueueURL: "https://netops.queue.core.windows.net/" + eventsQueue}); err != nil {
		t.Errorf("a well-formed queue URL: %v", err)
	}
}

// The provider's event source end to end, with the real SDK clients against the fake: an event
// names a resource and no location, and the last discovery says where the network is. A network
// discovery has not seen resyncs its whole subscription; a subscription no scope names, nothing.
func TestEventsAreMappedToTargetsByTheLastDiscovery(t *testing.T) {
	p, cloud := newFakeProvider(t, Options{EventsDebounce: 100 * time.Millisecond,
		EventsPollInterval: 20 * time.Millisecond, EventsLog: logr.Discard()})
	cloud.AddQueue(eventsQueue)
	// Only the operator's own identity may read the queue: that is the one the poller must use.
	cloud.RestrictQueue(eventsQueue, azurefake.Operator)
	p.discoverer.opts.EventsQueueURL = cloud.QueueURL(eventsQueue)
	cloud.AddVirtualNetwork(contractSubscription, "rg-net", "Hub", "West Europe", []string{"10.0.0.0/16"}, nil)
	cloud.AddVirtualNetwork(contractSubscription, "rg-net", "north", "northeurope", []string{"10.1.0.0/16"}, nil)

	changed := make(chan []inventory.TargetKey, 4)
	results := make(chan string, 16)
	source := p.Events(provider.EventSink{
		Changed: func(_ context.Context, keys []inventory.TargetKey) error {
			changed <- keys
			return nil
		},
		Received: func(result string) { results <- result },
	})
	go func() { _ = source.Start(t.Context()) }()
	next := func(what string) []inventory.TargetKey {
		t.Helper()
		select {
		case got := <-changed:
			return got
		case <-time.After(10 * time.Second):
			t.Fatalf("no change reported for %s", what)
			return nil
		}
	}
	result := func(what string) string {
		t.Helper()
		select {
		case got := <-results:
			return got
		case <-time.After(10 * time.Second):
			t.Fatalf("no result for %s", what)
			return ""
		}
	}
	prefix := "/subscriptions/" + contractSubscription + "/resourceGroups/rg-net/providers/Microsoft.Network/"

	// Before any discovery the subscription is nobody's, and its events are ignored.
	cloud.Publish(eventsQueue, azurefake.ResourceEvent(writeSuccess, prefix+"virtualNetworks/Hub"))
	if got := result("an event before the first discovery"); got != provider.EventIgnored {
		t.Errorf("before the first discovery: %s, want ignored", got)
	}

	// The scope spells the subscription in upper case; the target carries it in lowercase, as
	// the controllers build it.
	target := inventory.Target{Provider: networkv1.ProviderAzure, Scope: "events",
		Account: networkv1.CanonicalAccountID(networkv1.ProviderAzure, strings.ToUpper(contractSubscription)),
		Region:  "westeurope"}
	if _, err := p.Discover(t.Context(), target); err != nil {
		t.Fatal(err)
	}

	// A subnet of the network, with the ID in lowercase: the network's own location.
	cloud.Publish(eventsQueue, azurefake.ResourceEvent(writeSuccess,
		strings.ToLower(prefix+"virtualNetworks/Hub/subnets/payments")))
	want := []inventory.TargetKey{{Account: target.Account, Region: "westeurope"}}
	if got := next("a subnet of a known network"); !slices.Equal(got, want) {
		t.Errorf("changed %v, want %v", got, want)
	}
	// A network in another location of the subscription, which the listing saw too.
	cloud.Publish(eventsQueue, azurefake.ResourceEvent(writeSuccess, strings.ToUpper(prefix+"virtualNetworks/north")))
	want = []inventory.TargetKey{{Account: target.Account, Region: "northeurope"}}
	if got := next("a network in another location"); !slices.Equal(got, want) {
		t.Errorf("changed %v, want %v", got, want)
	}
	// A network created since: every location of the subscription.
	cloud.Publish(eventsQueue, azurefake.ResourceEvent(writeSuccess, prefix+"virtualNetworks/new"))
	want = []inventory.TargetKey{{Account: target.Account}}
	if got := next("a network discovery has not seen"); !slices.Equal(got, want) {
		t.Errorf("changed %v, want %v", got, want)
	}
	// A deleted network is resynced where it was, and forgotten: one of its name may come back
	// anywhere.
	const deleteSuccess = "Microsoft.Resources.ResourceDeleteSuccess"
	cloud.Publish(eventsQueue, azurefake.ResourceEvent(deleteSuccess, prefix+"virtualNetworks/north"))
	want = []inventory.TargetKey{{Account: target.Account, Region: "northeurope"}}
	if got := next("a deleted network"); !slices.Equal(got, want) {
		t.Errorf("changed %v, want %v", got, want)
	}
	cloud.Publish(eventsQueue, azurefake.ResourceEvent(writeSuccess, prefix+"virtualNetworks/north"))
	want = []inventory.TargetKey{{Account: target.Account}}
	if got := next("a network recreated after its delete"); !slices.Equal(got, want) {
		t.Errorf("changed %v, want %v", got, want)
	}

	// Another subscription's network is none of this operator's.
	for range 5 {
		<-results
	}
	cloud.Publish(eventsQueue, azurefake.ResourceEvent(writeSuccess, strings.ReplaceAll(prefix+"virtualNetworks/Hub",
		contractSubscription, "99999999-0000-4000-8000-000000000000")))
	if got := result("an event of another subscription"); got != provider.EventIgnored {
		t.Errorf("another subscription: %s, want ignored", got)
	}
	select {
	case got := <-changed:
		t.Errorf("another subscription's event changed %v", got)
	case <-time.After(300 * time.Millisecond):
	}
	eventually := time.Now().Add(10 * time.Second)
	for cloud.QueueLength(eventsQueue) != 0 {
		if time.Now().After(eventually) {
			t.Fatalf("%d messages left in the queue", cloud.QueueLength(eventsQueue))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A discovery limited to resource groups only adds to what is known of the subscription, and a
// listing of the whole subscription forgets what is gone.
func TestTheNetworkMemoryFollowsTheListings(t *testing.T) {
	p, cloud := newFakeProvider(t, Options{})
	cloud.AddVirtualNetwork(contractSubscription, "rg-a", "a", "westeurope", []string{"10.0.0.0/16"}, nil)
	cloud.AddVirtualNetwork(contractSubscription, "rg-b", "b", "westeurope", []string{"10.1.0.0/16"}, nil)
	id := func(group, name string) string {
		return strings.ToLower("/subscriptions/" + contractSubscription + "/resourceGroups/" + group +
			"/providers/Microsoft.Network/virtualNetworks/" + name)
	}
	target := inventory.Target{Provider: networkv1.ProviderAzure, Scope: "events", Account: contractSubscription,
		Region: "westeurope", Azure: &networkv1.AzureScope{ResourceGroups: []string{"rg-a"}}}
	if _, err := p.Discover(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	known := func() []string {
		p.discoverer.mu.Lock()
		defer p.discoverer.mu.Unlock()
		out := make([]string, 0, len(p.discoverer.networks[contractSubscription]))
		for k := range p.discoverer.networks[contractSubscription] {
			out = append(out, k)
		}
		slices.Sort(out)
		return out
	}
	if got := known(); !slices.Equal(got, []string{id("rg-a", "a")}) {
		t.Errorf("after a listing of rg-a: %v", got)
	}
	target.Azure.ResourceGroups = []string{"rg-b"}
	if _, err := p.Discover(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	if got := known(); !slices.Equal(got, []string{id("rg-a", "a"), id("rg-b", "b")}) {
		t.Errorf("after a listing of rg-b: %v", got)
	}
	cloud.RemoveVirtualNetwork(contractSubscription, "rg-a", "a")
	target.Azure = nil
	if _, err := p.Discover(t.Context(), target); err != nil {
		t.Fatal(err)
	}
	if got := known(); !slices.Equal(got, []string{id("rg-b", "b")}) {
		t.Errorf("after a listing of the subscription: %v", got)
	}
}
