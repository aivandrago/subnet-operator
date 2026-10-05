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

package gcp

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/cloud/gcp/gcpfake"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

const (
	eventsTopic        = "projects/ops-central/topics/subnet-operator-events"
	eventsSubscription = "projects/ops-central/subscriptions/subnet-operator-events"
)

// ChangeEvents is only claimed when there is a subscription to read them from.
func TestChangeEventsNeedASubscription(t *testing.T) {
	without := NewProvider(Options{})
	if slices.Contains(without.Capabilities(), networkv1.CapabilityChangeEvents) {
		t.Errorf("capabilities without a subscription: %v", without.Capabilities())
	}
	if without.Events(provider.EventSink{}) != nil {
		t.Error("an event source without a subscription")
	}
	with := NewProvider(Options{EventsSubscription: eventsSubscription})
	if !slices.Contains(with.Capabilities(), networkv1.CapabilityChangeEvents) {
		t.Errorf("capabilities with a subscription: %v", with.Capabilities())
	}
	if with.Events(provider.EventSink{}) == nil {
		t.Error("no event source with a subscription")
	}
}

func TestAMalformedSubscriptionStopsTheStart(t *testing.T) {
	_, err := NewFromEnvironment(t.Context(), Options{EventsSubscription: "subnet-operator-events"})
	if err == nil || !strings.Contains(err.Error(), "projects/<project>/subscriptions/<name>") {
		t.Errorf("err = %v", err)
	}
	if _, err := NewFromEnvironment(t.Context(), Options{EventsSubscription: eventsSubscription}); err != nil {
		t.Errorf("a well-formed subscription: %v", err)
	}
}

// The provider's event source end to end: a tag binding names its subnetwork by project
// number, and the project a discovery looked up turns it back into the project ID the scope
// knows.
func TestTagBindingEventsNameTheProjectByID(t *testing.T) {
	ps, err := gcpfake.NewPubSub()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ps.Close)
	if err := ps.CreateTopic(t.Context(), eventsTopic); err != nil {
		t.Fatal(err)
	}
	if err := ps.CreateSubscription(t.Context(), eventsSubscription, eventsTopic); err != nil {
		t.Fatal(err)
	}
	p, cloud := newFakeProvider(t, Options{EventsSubscription: eventsSubscription,
		PubSubClientOptions: ps.ClientOptions(), EventsDebounce: 100 * time.Millisecond, EventsLog: logr.Discard()})
	net := cloud.AddNetwork(contractProject, "shared")
	target := inventory.Target{Provider: networkv1.ProviderGCP, Scope: "events", Account: contractProject,
		Region: contractRegion, GCP: &networkv1.GCPScope{TagParent: "organizations/" + contractOrg}}
	if _, err := p.Discover(t.Context(), target); err != nil {
		t.Fatal(err)
	}

	changed := make(chan []inventory.TargetKey, 1)
	source := p.Events(provider.EventSink{Changed: func(_ context.Context, keys []inventory.TargetKey) error {
		changed <- keys
		return nil
	}})
	go func() { _ = source.Start(t.Context()) }()

	entry := fmt.Sprintf(`{"protoPayload":{"@type":"type.googleapis.com/google.cloud.audit.AuditLog",`+
		`"serviceName":"cloudresourcemanager.googleapis.com","methodName":"TagBindings.CreateTagBinding",`+
		`"request":{"tagBinding":{"parent":"//compute.googleapis.com/projects/%s/global/networks/%d"}}},`+
		`"logName":"organizations/%s/logs/cloudaudit.googleapis.com%%2Factivity"}`, contractNumber, net.ID, contractOrg)
	ps.Publish(eventsTopic, []byte(entry))
	select {
	case got := <-changed:
		if !slices.Equal(got, []inventory.TargetKey{{Account: contractProject}}) {
			t.Errorf("changed %v, want every region of %s", got, contractProject)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no change reported")
	}
}
