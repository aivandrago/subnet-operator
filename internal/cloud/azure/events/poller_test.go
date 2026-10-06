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
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/go-logr/logr"

	"hypersurgery.dev/subnet-operator/internal/cloud/azure/azurefake"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

const (
	queueName = "subnet-operator-events"
	location  = "westeurope"
	// otherSubscription is one no scope names.
	otherSubscription = "99999999-0000-4000-8000-000000000000"
)

// recorder is an EventSink that remembers what it was told.
type recorder struct {
	mu       sync.Mutex
	flushed  chan []inventory.TargetKey
	results  map[string]int
	failures int
	// failFlushes makes that many further flushes fail.
	failFlushes int
	attempts    int
}

func newRecorder() *recorder {
	return &recorder{flushed: make(chan []inventory.TargetKey, 10), results: map[string]int{}}
}

func (r *recorder) sink() provider.EventSink {
	return provider.EventSink{
		Changed: func(_ context.Context, changed []inventory.TargetKey) error {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.attempts++
			if r.failFlushes > 0 {
				r.failFlushes--
				return errors.New("the API server is not answering")
			}
			r.flushed <- changed
			return nil
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

func (r *recorder) flushAttempts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attempts
}

// locate knows one subscription and where its network "hub" is, as the provider does after a
// discovery.
func locate(c Change) (inventory.TargetKey, bool) {
	if c.Subscription != subscription {
		return inventory.TargetKey{}, false
	}
	key := inventory.TargetKey{Account: subscription}
	if c.NetworkID == canonical {
		key.Region = location
	}
	return key, true
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

// start runs a poller against a fake's queue with the real Storage SDK client, as the fake's
// operator identity.
func start(t *testing.T, cloud *azurefake.Cloud, rec *recorder) {
	t.Helper()
	p := &Poller{
		Connect: StorageQueue(cloud.QueueURL(queueName), func() (azcore.TokenCredential, error) {
			return cloud.Credential(), nil
		}, azcore.ClientOptions{Transport: cloud.Transport()}),
		QueueURL: cloud.QueueURL(queueName), Sink: rec.sink(), Locate: locate, Log: logr.Discard(),
		Debounce: 200 * time.Millisecond, PollInterval: 20 * time.Millisecond, Backoff: 20 * time.Millisecond,
	}
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	go func() { _ = p.Start(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("the poller did not stop with its context")
		}
	})
}

func newCloud(t *testing.T) *azurefake.Cloud {
	t.Helper()
	cloud := azurefake.New()
	t.Cleanup(cloud.Close)
	cloud.AddQueue(queueName)
	return cloud
}

func waitFlush(t *testing.T, rec *recorder) []inventory.TargetKey {
	t.Helper()
	select {
	case got := <-rec.flushed:
		slices.SortFunc(got, func(x, y inventory.TargetKey) int { return strings.Compare(x.String(), y.String()) })
		return got
	case <-time.After(10 * time.Second):
		t.Fatal("no flush")
		return nil
	}
}

func noFlush(t *testing.T, rec *recorder) {
	t.Helper()
	select {
	case extra := <-rec.flushed:
		t.Errorf("unexpected flush %v", extra)
	case <-time.After(500 * time.Millisecond):
	}
}

// A burst of events about one watched virtual network (the network, a subnet of it, its tags,
// however the IDs are spelled) is exactly one resync of its target, and the messages are gone
// from the queue once it is enqueued.
func TestEventsOfAWatchedNetworkResyncItsTargetOnce(t *testing.T) {
	cloud, rec := newCloud(t), newRecorder()
	ids := []string{
		cloud.Publish(queueName, azurefake.ResourceEvent(writeSuccess, network)),
		cloud.Publish(queueName, azurefake.ResourceEvent(writeSuccess, strings.ToLower(network)+"/subnets/payments")),
		cloud.Publish(queueName, azurefake.CloudEvent(writeSuccess,
			strings.ToUpper(network)+"/providers/Microsoft.Resources/tags/default")),
		cloud.Publish(queueName, azurefake.ResourceEvent(deleteSuccess, network+"/subnets/old")),
	}
	start(t, cloud, rec)

	want := []inventory.TargetKey{{Account: subscription, Region: location}}
	if got := waitFlush(t, rec); !slices.Equal(got, want) {
		t.Errorf("flushed %v, want %v", got, want)
	}
	noFlush(t, rec)
	for _, id := range ids {
		eventually(t, "message "+id+" deleted", func() bool { queued, _ := cloud.Queued(queueName, id); return !queued })
	}
	results, failures := rec.counts()
	if want := map[string]int{provider.EventResync: 4}; !maps.Equal(results, want) {
		t.Errorf("results = %v, want %v", results, want)
	}
	if failures != 0 {
		t.Errorf("%d failures, want none", failures)
	}
}

// A network the provider has not seen resyncs every location of its subscription: a key
// without a region.
func TestAnUnknownNetworkResyncsItsSubscription(t *testing.T) {
	cloud, rec := newCloud(t), newRecorder()
	cloud.Publish(queueName, azurefake.ResourceEvent(writeSuccess, strings.Replace(network, "hub", "new", 1)))
	start(t, cloud, rec)
	if got, want := waitFlush(t, rec), []inventory.TargetKey{{Account: subscription}}; !slices.Equal(got, want) {
		t.Errorf("flushed %v, want %v", got, want)
	}
}

// Events about other resource types and about subscriptions the operator does not discover are
// dropped: deleted from the queue, counted as ignored, and nothing is resynced.
func TestUnrelatedEventsAreDropped(t *testing.T) {
	cloud, rec := newCloud(t), newRecorder()
	rg := "/subscriptions/" + subscription + "/resourceGroups/rg-net"
	cloud.Publish(queueName, azurefake.ResourceEvent(writeSuccess, rg+"/providers/Microsoft.Compute/virtualMachines/vm-1"))
	cloud.Publish(queueName, azurefake.ResourceEvent(writeSuccess, rg+"/providers/Microsoft.Network/routeTables/rt"))
	cloud.Publish(queueName, azurefake.ResourceEvent("Microsoft.Resources.ResourceWriteFailure", network))
	cloud.Publish(queueName, azurefake.ResourceEvent(writeSuccess, strings.ReplaceAll(network, subscription,
		otherSubscription)))
	start(t, cloud, rec)

	eventually(t, "the queue emptied", func() bool { return cloud.QueueLength(queueName) == 0 })
	noFlush(t, rec)
	results, failures := rec.counts()
	if want := map[string]int{provider.EventIgnored: 4}; !maps.Equal(results, want) || failures != 0 {
		t.Errorf("results = %v, failures = %d, want %v and none", results, failures, want)
	}
}

// Whatever is put in the queue, the poller survives it: unreadable and oversized messages are
// dropped and counted as malformed, and the event behind them is still handled.
func TestMalformedAndOversizedMessagesAreDroppedAndCounted(t *testing.T) {
	cloud, rec := newCloud(t), newRecorder()
	valid := string(azurefake.ResourceEvent(writeSuccess, network))
	cloud.Enqueue(queueName, "garbage!")
	cloud.Enqueue(queueName, valid[:len(valid)/2])
	cloud.Enqueue(queueName, "")
	cloud.Enqueue(queueName, `{"eventType":"`+writeSuccess+`","subject":["`+network+`"]}`)
	cloud.Enqueue(queueName, valid+strings.Repeat(" ", MaxMessageBytes))
	cloud.Enqueue(queueName, strings.Repeat("[", 30000))
	cloud.Publish(queueName, []byte(valid))
	start(t, cloud, rec)

	want := []inventory.TargetKey{{Account: subscription, Region: location}}
	if got := waitFlush(t, rec); !slices.Equal(got, want) {
		t.Errorf("flushed %v, want %v", got, want)
	}
	eventually(t, "the queue emptied", func() bool { return cloud.QueueLength(queueName) == 0 })
	results, failures := rec.counts()
	if want := map[string]int{provider.EventMalformed: 6, provider.EventResync: 1}; !maps.Equal(results, want) ||
		failures != 0 {
		t.Errorf("results = %v, failures = %d, want %v and none", results, failures, want)
	}
}

// While the resync cannot be enqueued, the event stays in the queue: an operator that stops
// now loses nothing. It is deleted once the flush went through.
func TestAnEventIsNotDeletedUntilItsResyncIsEnqueued(t *testing.T) {
	cloud, rec := newCloud(t), newRecorder()
	rec.failFlushes = 3
	id := cloud.Publish(queueName, azurefake.ResourceEvent(writeSuccess, network))
	start(t, cloud, rec)

	eventually(t, "two failed flushes", func() bool { return rec.flushAttempts() >= 2 })
	if queued, _ := cloud.Queued(queueName, id); !queued {
		t.Fatal("the message was deleted before its resync was enqueued")
	}
	want := []inventory.TargetKey{{Account: subscription, Region: location}}
	if got := waitFlush(t, rec); !slices.Equal(got, want) {
		t.Errorf("flushed %v, want %v", got, want)
	}
	eventually(t, "the message deleted", func() bool { queued, _ := cloud.Queued(queueName, id); return !queued })
}

// An operator that stops between the receive and the flush leaves the event to the next
// leader.
func TestAnEventSurvivesAPollerThatStops(t *testing.T) {
	cloud, rec := newCloud(t), newRecorder()
	id := cloud.Publish(queueName, azurefake.ResourceEvent(writeSuccess, network))
	p := &Poller{
		Connect: StorageQueue(cloud.QueueURL(queueName), func() (azcore.TokenCredential, error) {
			return cloud.Credential(), nil
		}, azcore.ClientOptions{Transport: cloud.Transport()}),
		Sink: rec.sink(), Locate: locate, Log: logr.Discard(), Debounce: time.Hour, PollInterval: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { _ = p.Start(ctx); close(done) }()
	eventually(t, "the message received", func() bool { _, n := cloud.Queued(queueName, id); return n == 1 })
	cancel()
	<-done
	if queued, _ := cloud.Queued(queueName, id); !queued {
		t.Error("the message is gone, and nobody was told about its change")
	}
}

// A queue that does not exist (yet, or any more) is a failure that is counted and retried with
// backoff, not a crash: once it is there, events flow.
func TestAnUnreachableQueueIsCountedAndRetried(t *testing.T) {
	cloud, rec := newCloud(t), newRecorder()
	cloud.RemoveQueue(queueName)
	start(t, cloud, rec)
	eventually(t, "two failed receives", func() bool { _, f := rec.counts(); return f >= 2 })

	cloud.AddQueue(queueName)
	cloud.Publish(queueName, azurefake.ResourceEvent(writeSuccess, network))
	want := []inventory.TargetKey{{Account: subscription, Region: location}}
	if got := waitFlush(t, rec); !slices.Equal(got, want) {
		t.Errorf("flushed %v, want %v", got, want)
	}
}

// An identity without a data role on the queue is refused, and that is a counted failure.
func TestAQueueTheOperatorMayNotReadIsCounted(t *testing.T) {
	cloud, rec := newCloud(t), newRecorder()
	cloud.RestrictQueue(queueName, "somebody-else")
	cloud.Publish(queueName, azurefake.ResourceEvent(writeSuccess, network))
	start(t, cloud, rec)
	eventually(t, "two failed receives", func() bool { _, f := rec.counts(); return f >= 2 })
	noFlush(t, rec)

	cloud.RestrictQueue(queueName, azurefake.Operator)
	waitFlush(t, rec)
}

// An identity that may read the queue but not delete from it (Storage Queue Data Reader
// instead of Data Message Processor) still gets its resync, and every failed delete is counted
// so the alert fires.
func TestAFailedDeleteIsCounted(t *testing.T) {
	cloud, rec := newCloud(t), newRecorder()
	cloud.DenyQueueDeletes(queueName, true)
	id := cloud.Publish(queueName, azurefake.ResourceEvent(writeSuccess, network))
	cloud.Enqueue(queueName, "garbage!")
	start(t, cloud, rec)

	waitFlush(t, rec)
	eventually(t, "two failed deletes", func() bool { _, f := rec.counts(); return f >= 2 })
	if queued, _ := cloud.Queued(queueName, id); !queued {
		t.Error("the message is gone although its delete was refused")
	}
}

// Deleting a message that is gone already is no failure, so it is not counted as one; a queue
// that is gone is.
func TestDeletingAMessageThatIsGoneIsNotAFailure(t *testing.T) {
	cloud := newCloud(t)
	cloud.Publish(queueName, azurefake.ResourceEvent(writeSuccess, network))
	queue, err := StorageQueue(cloud.QueueURL(queueName), func() (azcore.TokenCredential, error) {
		return cloud.Credential(), nil
	}, azcore.ClientOptions{Transport: cloud.Transport()})(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	messages, err := queue.Receive(t.Context(), time.Minute)
	if err != nil || len(messages) != 1 || messages[0].DequeueCount != 1 {
		t.Fatalf("received %+v, %v", messages, err)
	}
	for _, attempt := range []string{"first", "second"} {
		if err := queue.Delete(t.Context(), messages[0]); err != nil {
			t.Errorf("%s delete: %v", attempt, err)
		}
	}
	cloud.RemoveQueue(queueName)
	if err := queue.Delete(t.Context(), messages[0]); err == nil {
		t.Error("deleting from a queue that does not exist succeeded")
	}
}

// stubQueue hands out fixed messages once and remembers what was deleted.
type stubQueue struct {
	mu       sync.Mutex
	messages []Message
	deleted  []string
}

func (q *stubQueue) Receive(context.Context, time.Duration) ([]Message, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.messages
	q.messages = nil
	return out, nil
}

func (q *stubQueue) Delete(_ context.Context, m Message) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.deleted = append(q.deleted, m.ID)
	return nil
}

// A message that keeps coming back is not handled for ever: past MaxDeliveries it is dropped
// without a resync, and counted. At MaxDeliveries it is still handled.
func TestAMessageDeliveredTooOftenIsDropped(t *testing.T) {
	text := string(azurefake.ResourceEvent(writeSuccess, network))
	q := &stubQueue{messages: []Message{
		{ID: "poison", PopReceipt: "r1", Text: text, DequeueCount: MaxDeliveries + 1},
	}}
	rec := newRecorder()
	p := &Poller{Connect: func(context.Context) (Queue, error) { return q, nil }, Sink: rec.sink(), Locate: locate,
		Log: logr.Discard(), Debounce: 100 * time.Millisecond, PollInterval: 20 * time.Millisecond}
	go func() { _ = p.Start(t.Context()) }()

	eventually(t, "the poison message deleted", func() bool {
		q.mu.Lock()
		defer q.mu.Unlock()
		return slices.Contains(q.deleted, "poison")
	})
	noFlush(t, rec)
	if results, _ := rec.counts(); !maps.Equal(results, map[string]int{provider.EventMalformed: 1}) {
		t.Errorf("results = %v, want one malformed", results)
	}

	q.mu.Lock()
	q.messages = []Message{{ID: "last-try", PopReceipt: "r2", Text: text, DequeueCount: MaxDeliveries}}
	q.mu.Unlock()
	want := []inventory.TargetKey{{Account: subscription, Region: location}}
	if got := waitFlush(t, rec); !slices.Equal(got, want) {
		t.Errorf("flushed %v, want %v", got, want)
	}
}

// Without a locator every event of a virtual network resyncs its subscription.
func TestWithoutALocatorTheSubscriptionIsResynced(t *testing.T) {
	p := &Poller{Log: logr.Discard()}
	keys, result := p.handle(Message{Text: string(azurefake.ResourceEvent(writeSuccess, network))})
	if result != provider.EventResync || !slices.Equal(keys, []inventory.TargetKey{{Account: subscription}}) {
		t.Errorf("keys = %v, result = %s", keys, result)
	}
}

// The queue is read as the operator's own identity: a URL that carries a shared access
// signature, is not https, or names no queue is refused.
func TestCheckQueueURL(t *testing.T) {
	for _, good := range []string{
		"https://netops.queue.core.windows.net/subnet-operator-events",
		"https://netops.queue.core.usgovcloudapi.net/subnet-operator-events/",
		"https://127.0.0.1:10001/devstoreaccount1/subnet-operator-events",
	} {
		if err := CheckQueueURL(good); err != nil {
			t.Errorf("%s: %v", good, err)
		}
	}
	for bad, want := range map[string]string{
		"subnet-operator-events": "not a queue URL",
		"http://netops.queue.core.windows.net/subnet-operator-events": "not a queue URL",
		"https://netops.queue.core.windows.net":                       "no queue",
		"https://netops.queue.core.windows.net/":                      "no queue",
		"https://netops.queue.core.windows.net/q?sv=2024-11-04&sig=x": "shared access signature",
		"https://user:key@netops.queue.core.windows.net/q":            "shared access signature",
		"https://netops.queue.core.windows.net/q\x7f":                 "not a URL",
	} {
		if err := CheckQueueURL(bad); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", bad, err, want)
		}
	}
	if _, err := StorageQueue("https://netops.queue.core.windows.net/q?sig=x", nil, azcore.ClientOptions{})(t.Context()); err == nil {
		t.Error("a queue URL with a signature connected")
	}
}

func TestPollerNeedsLeaderElection(t *testing.T) {
	if !(&Poller{}).NeedLeaderElection() {
		t.Error("only the leader may consume the queue")
	}
}
