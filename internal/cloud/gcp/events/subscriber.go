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
	"fmt"
	"regexp"
	"sync/atomic"
	"time"

	"cloud.google.com/go/pubsub/v2"
	"github.com/go-logr/logr"
	"google.golang.org/api/option"

	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

// Receiver is the part of a Pub/Sub subscriber the source uses: *pubsub.Subscriber.
type Receiver interface {
	Receive(ctx context.Context, f func(context.Context, *pubsub.Message)) error
}

// Connect returns the receiver of the subscription and what closes it.
type Connect func(ctx context.Context) (Receiver, func() error, error)

// maxOutstanding bounds the messages being handled at once. Handling one is a JSON decode and
// a channel send, so a small number keeps up with any audit log volume.
const maxOutstanding = 100

// SubscriptionPattern is the shape of a subscription's full name: the project it lives in is
// usually a central one, not a project of the scope, so it is always named.
var SubscriptionPattern = regexp.MustCompile(`^projects/([a-z][a-z0-9.:-]*[a-z0-9])/subscriptions/[A-Za-z][A-Za-z0-9._~+%-]{2,254}$`)

// PubSub connects to a subscription with the Pub/Sub client library, as the operator's own
// Google identity (Application Default Credentials: GKE Workload Identity or Workload Identity
// Federation) unless opts say otherwise. The client also honours PUBSUB_EMULATOR_HOST.
func PubSub(subscription string, opts ...option.ClientOption) Connect {
	return func(ctx context.Context) (Receiver, func() error, error) {
		m := SubscriptionPattern.FindStringSubmatch(subscription)
		if m == nil {
			return nil, nil, fmt.Errorf("%q is not a subscription name: projects/<project>/subscriptions/<name>", subscription)
		}
		client, err := pubsub.NewClient(ctx, m[1], opts...)
		if err != nil {
			return nil, nil, fmt.Errorf("pub/sub client: %w", err)
		}
		s := client.Subscriber(subscription)
		s.ReceiveSettings.MaxOutstandingMessages = maxOutstanding
		return s, client.Close, nil
	}
}

// Subscriber pulls audit log entries from a Pub/Sub subscription fed by a log sink and flushes
// the changed targets to the sink every Debounce interval, so a burst of API calls causes one
// resync per target. It mirrors the AWS SQS poller: messages are acknowledged once read,
// including the ones that cannot be parsed, and the periodic resync covers anything lost.
type Subscriber struct {
	Connect      Connect
	Subscription string
	Sink         provider.EventSink
	// Projects looks up the project ID of a project number.
	Projects ProjectResolver
	Debounce time.Duration
	Log      logr.Logger

	// backoff is the first wait after a failed receive; tests shorten it.
	backoff time.Duration
}

// NeedLeaderElection makes only the leader consume the subscription.
func (s *Subscriber) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable. It returns when ctx is cancelled.
func (s *Subscriber) Start(ctx context.Context) error {
	debounce := s.Debounce
	if debounce <= 0 {
		debounce = 10 * time.Second
	}
	received := make(chan []inventory.TargetKey)
	go s.receiveLoop(ctx, received)

	pending := map[inventory.TargetKey]bool{}
	ticker := time.NewTicker(debounce)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case keys := <-received:
			for _, k := range keys {
				pending[k] = true
			}
		case <-ticker.C:
			if len(pending) == 0 {
				continue
			}
			changed := make([]inventory.TargetKey, 0, len(pending))
			for k := range pending {
				changed = append(changed, k)
			}
			if err := s.Sink.Changed(ctx, changed); err != nil {
				s.Log.Error(err, "failed to enqueue changed targets, retrying on the next tick")
				continue
			}
			pending = map[inventory.TargetKey]bool{}
		}
	}
}

// maxBackoff caps the wait between attempts to receive, as on AWS.
const maxBackoff = time.Minute

// receiveLoop keeps a streaming pull open. Receive retries what Pub/Sub calls retryable on its
// own and returns the rest (a deleted subscription, a missing permission); those are logged,
// counted, and retried with backoff, for a subscription or a grant can be fixed without
// restarting the operator.
func (s *Subscriber) receiveLoop(ctx context.Context, out chan<- []inventory.TargetKey) {
	first := s.backoff
	if first <= 0 {
		first = time.Second
	}
	backoff := first
	var receiver Receiver
	for ctx.Err() == nil {
		if receiver == nil {
			r, closeFn, err := s.Connect(ctx)
			if err != nil {
				s.failed(ctx, err, backoff)
				backoff = s.wait(ctx, backoff)
				continue
			}
			defer func() { _ = closeFn() }() // once: the receiver is kept from here on
			receiver = r
		}
		started := time.Now()
		var delivered atomic.Bool
		err := receiver.Receive(ctx, func(ctx context.Context, m *pubsub.Message) {
			if s.handle(ctx, m, out) {
				delivered.Store(true)
			}
		})
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			err = errors.New("the subscription stopped delivering")
		}
		if delivered.Load() || time.Since(started) > maxBackoff {
			backoff = first // it worked for a while: this is a new failure, not the same one
		}
		s.failed(ctx, err, backoff)
		backoff = s.wait(ctx, backoff)
	}
}

func (s *Subscriber) failed(ctx context.Context, err error, backoff time.Duration) {
	if errors.Is(err, context.Canceled) && ctx.Err() != nil {
		return
	}
	s.Log.Error(err, "failed to receive events", "subscription", s.Subscription, "retryIn", backoff.String())
	if s.Sink.ReceiveFailed != nil {
		s.Sink.ReceiveFailed()
	}
}

// wait sleeps for backoff, or until ctx is done, and returns the next backoff.
func (s *Subscriber) wait(ctx context.Context, backoff time.Duration) time.Duration {
	select {
	case <-ctx.Done():
	case <-time.After(backoff):
	}
	return min(backoff*2, maxBackoff)
}

// handle parses one message, passes on what it reports and acknowledges it. Unparsable
// messages are acknowledged too: redelivering them would never succeed. It reports whether
// the message was delivered, which a message dropped on shutdown is not.
func (s *Subscriber) handle(ctx context.Context, m *pubsub.Message, out chan<- []inventory.TargetKey) bool {
	key, creation, ok, err := ParseWithCreation(m.Data, s.Projects)
	result := provider.EventIgnored
	switch {
	case err != nil:
		result = provider.EventMalformed
		s.Log.Error(err, "dropping unparsable event", "messageId", m.ID)
	case ok:
		result = provider.EventResync
		if creation != nil && s.Sink.Created != nil {
			s.Sink.Created(ctx, []inventory.Creation{*creation})
		}
		select {
		case out <- []inventory.TargetKey{key}:
		case <-ctx.Done():
			m.Nack() // shutting down: the next leader gets it
			return false
		}
	}
	m.Ack()
	if s.Sink.Received != nil {
		s.Sink.Received(result)
	}
	return true
}
