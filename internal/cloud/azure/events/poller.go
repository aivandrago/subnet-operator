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
	"net/url"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azqueue"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azqueue/queueerror"
	"github.com/go-logr/logr"

	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

// Message is one message of the queue, as a receive returned it.
type Message struct {
	ID string
	// PopReceipt is what deletes this delivery of the message; a later delivery has another.
	PopReceipt string
	Text       string
	// DequeueCount is how often the message has been received, this time included.
	DequeueCount int64
}

// Queue is the part of a Storage queue the source uses.
type Queue interface {
	// Receive returns the messages that are visible, at most 32, without waiting for one, and
	// hides them from other receivers for the visibility timeout.
	Receive(ctx context.Context, visibility time.Duration) ([]Message, error)
	// Delete removes a message for good.
	Delete(ctx context.Context, m Message) error
}

// Connect returns the queue. It is called until it succeeds, so a credential that is not
// there yet is retried like an unreachable queue.
type Connect func(ctx context.Context) (Queue, error)

// CheckQueueURL reports why a URL cannot be the queue: it is
// https://<account>.queue.core.windows.net/<queue> (or the queue endpoint of another cloud),
// without a query. A shared access signature in the URL is refused on purpose: the operator
// reads the queue as its own identity and holds no storage secret.
func CheckQueueURL(queueURL string) error {
	u, err := url.Parse(queueURL)
	switch {
	case err != nil:
		return fmt.Errorf("%q is not a URL: %w", queueURL, err)
	case u.Scheme != "https" || u.Host == "":
		return fmt.Errorf("%q is not a queue URL: https://<account>.queue.core.windows.net/<queue>", queueURL)
	case u.RawQuery != "" || u.Fragment != "" || u.User != nil:
		return errors.New("the queue URL must not carry a query, such as a shared access signature, or credentials: " +
			"the operator reads the queue as its own identity")
	case strings.Trim(u.Path, "/") == "":
		return fmt.Errorf("%q names a storage account but no queue: https://<account>.queue.core.windows.net/<queue>",
			queueURL)
	}
	return nil
}

// StorageQueue connects to a queue with the Azure SDK, as the credential it is given: the
// operator's own identity, which needs the Storage Queue Data Message Processor role on the
// queue and nothing else.
func StorageQueue(queueURL string, credential func() (azcore.TokenCredential, error),
	options azcore.ClientOptions) Connect {
	return func(context.Context) (Queue, error) {
		if err := CheckQueueURL(queueURL); err != nil {
			return nil, err
		}
		cred, err := credential()
		if err != nil {
			return nil, err
		}
		client, err := azqueue.NewQueueClient(queueURL, cred, &azqueue.ClientOptions{ClientOptions: options})
		if err != nil {
			return nil, fmt.Errorf("storage queue client: %w", err)
		}
		return storageQueue{client}, nil
	}
}

type storageQueue struct{ client *azqueue.QueueClient }

// receiveBatch is the most messages one Get Messages call returns.
const receiveBatch = 32

func (q storageQueue) Receive(ctx context.Context, visibility time.Duration) ([]Message, error) {
	n, seconds := int32(receiveBatch), int32(max(visibility/time.Second, 1))
	resp, err := q.client.DequeueMessages(ctx, &azqueue.DequeueMessagesOptions{NumberOfMessages: &n,
		VisibilityTimeout: &seconds})
	if err != nil {
		return nil, err
	}
	out := make([]Message, 0, len(resp.Messages))
	for _, m := range resp.Messages {
		if m == nil || m.MessageID == nil || m.PopReceipt == nil {
			continue
		}
		msg := Message{ID: *m.MessageID, PopReceipt: *m.PopReceipt}
		if m.MessageText != nil {
			msg.Text = *m.MessageText
		}
		if m.DequeueCount != nil {
			msg.DequeueCount = *m.DequeueCount
		}
		out = append(out, msg)
	}
	return out, nil
}

// Delete removes the message. One that is gone already (it expired, or a later delivery of it
// was dropped) is what the caller wanted, not a failure.
func (q storageQueue) Delete(ctx context.Context, m Message) error {
	_, err := q.client.DeleteMessage(ctx, m.ID, m.PopReceipt, nil)
	if queueerror.HasCode(err, queueerror.MessageNotFound) {
		return nil
	}
	return err
}

// Locate maps a changed virtual network to the target to resync, and reports whether there is
// one: the provider knows which subscriptions it discovers and where their networks are, which
// an event does not say.
type Locate func(c Change) (inventory.TargetKey, bool)

const (
	// MaxDeliveries is how often a message is handled. Each delivery of an event means a
	// resync, so a message that keeps coming back (its delete keeps failing, or the resync could
	// not be enqueued for minutes) is dropped rather than looped on until the queue expires it;
	// the periodic resync covers what it reported.
	MaxDeliveries = 5
	// maxUnsettled bounds the messages kept for deletion while their resync is not enqueued
	// yet. Beyond it a message is left to be redelivered, which costs a resync and no memory.
	maxUnsettled = 4096
	// maxBackoff caps the wait between attempts to receive, as on AWS and GCP.
	maxBackoff = time.Minute
	// defaultPollInterval is the wait after a receive that found the queue empty. A Storage
	// queue has no long polling, and every receive is a billed transaction.
	defaultPollInterval = 5 * time.Second
)

// Poller polls a Storage queue an Event Grid subscription fills and flushes the changed
// targets to the sink every Debounce interval, so a burst of writes causes one resync per
// target. Unlike a message of the SQS and Pub/Sub sources, an event that reports a change is
// deleted only once its resync is enqueued: until then it stays in the queue, hidden, and
// comes back if the operator stops or the sink keeps failing. Messages that report nothing,
// cannot be read, or were delivered MaxDeliveries times are deleted at once.
type Poller struct {
	Connect  Connect
	QueueURL string
	Sink     provider.EventSink
	// Locate maps a changed network to its target. Nil resyncs every location of the
	// network's subscription.
	Locate   Locate
	Debounce time.Duration
	Log      logr.Logger

	// PollInterval replaces the wait after an empty receive, and Backoff the first wait after
	// a failed one; tests shorten them.
	PollInterval, Backoff time.Duration
}

// NeedLeaderElection makes only the leader consume the queue.
func (p *Poller) NeedLeaderElection() bool { return true }

// batch is what one receive found to resync: the targets, and the messages that said so,
// which are deleted once the targets are enqueued.
type batch struct {
	keys     []inventory.TargetKey
	messages []Message
	queue    Queue
}

// Start implements manager.Runnable. It returns when ctx is cancelled.
func (p *Poller) Start(ctx context.Context) error {
	debounce := p.Debounce
	if debounce <= 0 {
		debounce = 10 * time.Second
	}
	received := make(chan batch)
	// A message stays hidden long enough for two flushes, then comes back.
	go p.receiveLoop(ctx, received, 2*debounce+30*time.Second)

	pending := map[inventory.TargetKey]bool{}
	// unsettled are the messages behind pending, by ID: a redelivery replaces the delivery it
	// follows, whose pop receipt no longer deletes anything.
	unsettled := map[string]Message{}
	var queue Queue
	ticker := time.NewTicker(debounce)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case b := <-received:
			queue = b.queue
			for _, k := range b.keys {
				pending[k] = true
			}
			for _, m := range b.messages {
				if _, known := unsettled[m.ID]; known || len(unsettled) < maxUnsettled {
					unsettled[m.ID] = m
				}
			}
		case <-ticker.C:
			if len(pending) == 0 {
				continue
			}
			changed := make([]inventory.TargetKey, 0, len(pending))
			for k := range pending {
				changed = append(changed, k)
			}
			if err := p.Sink.Changed(ctx, changed); err != nil {
				p.Log.Error(err, "failed to enqueue changed targets, retrying on the next tick")
				continue
			}
			for _, m := range unsettled {
				p.delete(ctx, queue, m)
			}
			pending, unsettled = map[inventory.TargetKey]bool{}, map[string]Message{}
		}
	}
}

// delete removes a message that is done with. A failure is counted like a failed receive: the
// message will be delivered again, and an identity that may read but not delete is a setup
// mistake somebody has to see.
func (p *Poller) delete(ctx context.Context, queue Queue, m Message) {
	err := queue.Delete(ctx, m)
	if err == nil || ctx.Err() != nil {
		return
	}
	p.Log.Error(err, "failed to delete an event, it will be delivered again", "messageId", m.ID,
		"queue", p.QueueURL)
	if p.Sink.ReceiveFailed != nil {
		p.Sink.ReceiveFailed()
	}
}

// receiveLoop polls the queue. A failed receive (no such queue, a missing role assignment, no
// token) is logged, counted and retried with backoff, for a queue or a grant can be fixed
// without restarting the operator.
func (p *Poller) receiveLoop(ctx context.Context, out chan<- batch, visibility time.Duration) {
	first, idle := p.Backoff, p.PollInterval
	if first <= 0 {
		first = time.Second
	}
	if idle <= 0 {
		idle = defaultPollInterval
	}
	backoff := first
	var queue Queue
	for ctx.Err() == nil {
		var messages []Message
		var err error
		if queue == nil {
			queue, err = p.Connect(ctx)
		}
		if err == nil {
			messages, err = queue.Receive(ctx, visibility)
		}
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			p.Log.Error(err, "failed to receive events", "queue", p.QueueURL, "retryIn", backoff.String())
			if p.Sink.ReceiveFailed != nil {
				p.Sink.ReceiveFailed()
			}
			if !sleep(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		backoff = first
		if len(messages) == 0 {
			if !sleep(ctx, idle) {
				return
			}
			continue
		}
		b := batch{queue: queue}
		for _, m := range messages {
			keys, result := p.handle(m)
			if p.Sink.Received != nil {
				p.Sink.Received(result)
			}
			if result != provider.EventResync {
				p.delete(ctx, queue, m)
				continue
			}
			b.keys = append(b.keys, keys...)
			b.messages = append(b.messages, m)
		}
		if len(b.messages) == 0 {
			continue
		}
		select {
		case out <- b:
		case <-ctx.Done():
			return // not deleted: the next leader gets them
		}
	}
}

// handle decides what becomes of one message: the targets to resync, or why there are none.
func (p *Poller) handle(m Message) ([]inventory.TargetKey, string) {
	if m.DequeueCount > MaxDeliveries {
		p.Log.Error(nil, "dropping an event that keeps being delivered", "messageId", m.ID,
			"deliveries", m.DequeueCount)
		return nil, provider.EventMalformed
	}
	changes, err := Parse(m.Text)
	if err != nil {
		p.Log.Error(err, "dropping unparsable event", "messageId", m.ID)
		return nil, provider.EventMalformed
	}
	var keys []inventory.TargetKey
	for _, c := range changes {
		key, ok := inventory.TargetKey{Account: c.Subscription}, true
		if p.Locate != nil {
			key, ok = p.Locate(c)
		}
		if ok {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil, provider.EventIgnored
	}
	return keys, provider.EventResync
}

// sleep waits for d and reports whether ctx is still live.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
