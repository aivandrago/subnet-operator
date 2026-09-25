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
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/go-logr/logr"

	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// SQSAPI is the subset of the SQS client used by the poller.
type SQSAPI interface {
	ReceiveMessage(ctx context.Context, in *sqs.ReceiveMessageInput, opts ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessageBatch(ctx context.Context, in *sqs.DeleteMessageBatchInput, opts ...func(*sqs.Options)) (*sqs.DeleteMessageBatchOutput, error)
}

// Sink receives the account/region pairs that changed since the last flush.
type Sink func(ctx context.Context, changed []inventory.TargetKey) error

// CreationSink receives newly created resources and who created them. It is called as the
// events arrive rather than on the debounce tick, because the attribution is what makes the
// resync that follows useful.
type CreationSink func(ctx context.Context, created []inventory.Creation)

// Poller long-polls an SQS queue fed by EventBridge and flushes changed targets to the sink
// every Debounce interval, so a burst of API calls causes one resync per target.
type Poller struct {
	SQS      SQSAPI
	QueueURL string
	Sink     Sink
	// OnCreate, when set, is told about every VPC and subnet a CloudTrail event says was
	// just created, together with the principal that created it.
	OnCreate CreationSink
	Debounce time.Duration
	Log      logr.Logger
}

// NeedLeaderElection makes only the leader consume the queue.
func (p *Poller) NeedLeaderElection() bool { return true }

// Start implements manager.Runnable. It returns when ctx is cancelled.
func (p *Poller) Start(ctx context.Context) error {
	debounce := p.Debounce
	if debounce <= 0 {
		debounce = 10 * time.Second
	}
	received := make(chan []inventory.TargetKey)
	go p.receiveLoop(ctx, received)

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
			if err := p.Sink(ctx, changed); err != nil {
				p.Log.Error(err, "failed to enqueue changed targets, retrying on the next tick")
				continue
			}
			pending = map[inventory.TargetKey]bool{}
		}
	}
}

func (p *Poller) receiveLoop(ctx context.Context, out chan<- []inventory.TargetKey) {
	backoff := time.Second
	for ctx.Err() == nil {
		keys, err := p.receiveOnce(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				p.Log.Error(err, "failed to receive events", "queue", p.QueueURL, "retryIn", backoff.String())
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff = time.Second
		if len(keys) == 0 {
			continue
		}
		select {
		case out <- keys:
		case <-ctx.Done():
			return
		}
	}
}

// receiveOnce reads up to 10 messages, parses them and deletes them. Unparsable messages are
// deleted too: redelivering them would never succeed, and the periodic resync covers any loss.
func (p *Poller) receiveOnce(ctx context.Context) ([]inventory.TargetKey, error) {
	out, err := p.SQS.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
		QueueUrl:            aws.String(p.QueueURL),
		MaxNumberOfMessages: 10,
		WaitTimeSeconds:     20,
	})
	if err != nil {
		return nil, err
	}
	var keys []inventory.TargetKey
	var created []inventory.Creation
	var entries []sqstypes.DeleteMessageBatchRequestEntry
	for i, m := range out.Messages {
		key, creation, ok, err := ParseWithCreation(aws.ToString(m.Body))
		switch {
		case err != nil:
			p.Log.Error(err, "dropping unparsable event", "messageId", aws.ToString(m.MessageId))
		case ok:
			keys = append(keys, key)
			if creation != nil {
				created = append(created, *creation)
			}
		}
		entries = append(entries, sqstypes.DeleteMessageBatchRequestEntry{
			Id: aws.String(strconv.Itoa(i)), ReceiptHandle: m.ReceiptHandle,
		})
	}
	if len(entries) > 0 {
		// A failed delete means redelivery and one extra resync, which is harmless.
		if _, err := p.SQS.DeleteMessageBatch(ctx, &sqs.DeleteMessageBatchInput{
			QueueUrl: aws.String(p.QueueURL), Entries: entries,
		}); err != nil {
			p.Log.Error(err, "failed to delete events")
		}
	}
	if len(created) > 0 && p.OnCreate != nil {
		p.OnCreate(ctx, created)
	}
	return keys, nil
}
