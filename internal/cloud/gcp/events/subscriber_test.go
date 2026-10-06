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

package events

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"

	"hypersurgery.dev/subnet-operator/internal/cloud/gcp/gcpfake"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

const (
	topic        = "projects/ops-central/topics/subnet-operator-events"
	subscription = "projects/ops-central/subscriptions/subnet-operator-events"
)

// recorder is an EventSink that remembers what it was told.
type recorder struct {
	mu       sync.Mutex
	flushed  chan []inventory.TargetKey
	created  []inventory.Creation
	results  map[string]int
	failures int
}

func newRecorder() *recorder {
	return &recorder{flushed: make(chan []inventory.TargetKey, 10), results: map[string]int{}}
}

func (r *recorder) sink() provider.EventSink {
	return provider.EventSink{
		Changed: func(_ context.Context, changed []inventory.TargetKey) error {
			r.flushed <- changed
			return nil
		},
		Created: func(_ context.Context, created []inventory.Creation) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.created = append(r.created, created...)
		},
		Received: func(result string) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.results[result]++
		},
		ReceiveFailed: func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.failures++
		},
	}
}

func (r *recorder) counts() (map[string]int, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.results), r.failures
}

func newPubSub(t *testing.T) *gcpfake.PubSub {
	t.Helper()
	ps, err := gcpfake.NewPubSub()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ps.Close)
	if err := ps.CreateTopic(t.Context(), topic); err != nil {
		t.Fatal(err)
	}
	return ps
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The subscriber, with the real Pub/Sub client against pstest: a burst of entries becomes one
// flush per target, every message is acknowledged (the unreadable one too) and counted.
func TestSubscriberDebouncesAcknowledgesAndCounts(t *testing.T) {
	ps := newPubSub(t)
	if err := ps.CreateSubscription(t.Context(), subscription, topic); err != nil {
		t.Fatal(err)
	}
	rec := newRecorder()
	s := &Subscriber{Connect: PubSub(subscription, ps.ClientOptions()...), Subscription: subscription,
		Sink: rec.sink(), Projects: knownProjects, Debounce: 300 * time.Millisecond, Log: logr.Discard()}
	go func() { _ = s.Start(t.Context()) }()

	subnet := "projects/shop-host-1/regions/europe-west1/subnetworks/payments"
	ids := []string{
		ps.Publish(topic, gcpfake.AuditLogEntry(project, "compute.googleapis.com", "v1.compute.subnetworks.insert",
			subnet, "maria.k@example.com")),
		ps.Publish(topic, gcpfake.AuditLogEntry(project, "compute.googleapis.com", "v1.compute.subnetworks.patch",
			subnet, "maria.k@example.com")),
		ps.Publish(topic, gcpfake.AuditLogEntry("shop-dev-2", "compute.googleapis.com", "v1.compute.networks.delete",
			"projects/shop-dev-2/global/networks/scratch", "ops@example.com")),
		ps.Publish(topic, gcpfake.AuditLogEntry(project, "compute.googleapis.com", "v1.compute.instances.insert",
			"projects/shop-host-1/zones/europe-west1-b/instances/vm-1", "maria.k@example.com")),
		ps.Publish(topic, []byte("garbage")),
	}

	var got []inventory.TargetKey
	select {
	case got = <-rec.flushed:
	case <-time.After(10 * time.Second):
		t.Fatal("no flush")
	}
	slices.SortFunc(got, func(x, y inventory.TargetKey) int { return strings.Compare(x.String(), y.String()) })
	want := []inventory.TargetKey{{Account: "shop-dev-2"}, {Account: project, Region: region}}
	if !slices.Equal(got, want) {
		t.Errorf("flushed %v, want %v (one entry per target)", got, want)
	}
	select {
	case extra := <-rec.flushed:
		t.Errorf("unexpected second flush %v", extra)
	case <-time.After(600 * time.Millisecond):
	}
	for _, id := range ids {
		eventually(t, "message "+id+" acknowledged", func() bool { return ps.Acked(id) })
	}
	results, failures := rec.counts()
	if want := map[string]int{provider.EventResync: 3, provider.EventIgnored: 1, provider.EventMalformed: 1}; !maps.Equal(results, want) {
		t.Errorf("results = %v, want %v", results, want)
	}
	if failures != 0 {
		t.Errorf("%d failed receives, want none", failures)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.created) != 1 || rec.created[0].ResourceID != subnet || rec.created[0].Principal != "maria.k@example.com" {
		t.Errorf("created = %+v, want the inserted subnetwork and who inserted it", rec.created)
	}
}

// A subscription that does not exist (yet, or any more) is a failure that is counted and
// retried, not a crash: once it is there, events flow.
func TestSubscriberRetriesAMissingSubscription(t *testing.T) {
	ps := newPubSub(t)
	rec := newRecorder()
	s := &Subscriber{Connect: PubSub(subscription, ps.ClientOptions()...), Subscription: subscription,
		Sink: rec.sink(), Debounce: 100 * time.Millisecond, Log: logr.Discard(), backoff: 50 * time.Millisecond}
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	go func() { _ = s.Start(ctx); close(done) }()

	eventually(t, "two failed receives", func() bool { _, f := rec.counts(); return f >= 2 })

	if err := ps.CreateSubscription(t.Context(), subscription, topic); err != nil {
		t.Fatal(err)
	}
	id := ps.Publish(topic, gcpfake.AuditLogEntry(project, "compute.googleapis.com", "v1.compute.networks.insert",
		"projects/shop-host-1/global/networks/shared", "ops@example.com"))
	select {
	case got := <-rec.flushed:
		if !slices.Equal(got, []inventory.TargetKey{{Account: project}}) {
			t.Errorf("flushed %v", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("no flush once the subscription exists")
	}
	eventually(t, "the message acknowledged", func() bool { return ps.Acked(id) })

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the subscriber did not stop with its context")
	}
}

// A name that cannot be a subscription fails when connecting, and counts like any failure.
func TestPubSubRefusesABadName(t *testing.T) {
	_, _, err := PubSub("subnet-operator-events")(t.Context())
	if err == nil || !strings.Contains(err.Error(), "projects/<project>/subscriptions/<name>") {
		t.Errorf("err = %v", err)
	}
}

func TestSubscriberNeedsLeaderElection(t *testing.T) {
	if !(&Subscriber{}).NeedLeaderElection() {
		t.Error("only the leader may consume the subscription")
	}
}
