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
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/go-logr/logr"

	"hypersurgery.dev/subnet-operator/internal/inventory"
)

const region = "eu-central-1"

func event(name, account, extraDetail string) string {
	return fmt.Sprintf(`{"source":"aws.ec2","detail-type":"AWS API Call via CloudTrail","account":"999999999999",`+
		`"region":"us-east-1","detail":{"eventName":%q,"awsRegion":%q,"recipientAccountId":%q%s}}`,
		name, region, account, extraDetail)
}

func TestParse(t *testing.T) {
	cases := []struct {
		name string
		body string
		want inventory.TargetKey
		ok   bool
		err  bool
	}{
		{"subnet created", event("CreateSubnet", "111111111111", ""),
			inventory.TargetKey{Account: "111111111111", Region: "eu-central-1"}, true, false},
		{"failed call", event("CreateSubnet", "111111111111", `,"errorCode":"UnauthorizedOperation"`),
			inventory.TargetKey{}, false, false},
		{"irrelevant call", event("RunInstances", "111111111111", ""), inventory.TargetKey{}, false, false},
		{"subnet tagged", event("CreateTags", "111111111111",
			`,"requestParameters":{"resourcesSet":{"items":[{"resourceId":"i-1"},{"resourceId":"subnet-1"}]}}`),
			inventory.TargetKey{Account: "111111111111", Region: "eu-central-1"}, true, false},
		{"instance tagged", event("CreateTags", "111111111111",
			`,"requestParameters":{"resourcesSet":{"items":[{"resourceId":"i-1"}]}}`), inventory.TargetKey{}, false, false},
		{"other source", `{"source":"aws.s3","detail":{"eventName":"CreateSubnet"}}`, inventory.TargetKey{}, false, false},
		{"envelope fallback", `{"source":"aws.ec2","account":"222222222222","region":"eu-west-1","detail":{"eventName":"DeleteVpc"}}`,
			inventory.TargetKey{Account: "222222222222", Region: "eu-west-1"}, true, false},
		{"garbage", `not json`, inventory.TargetKey{}, false, true},
		{"no account", `{"source":"aws.ec2","detail":{"eventName":"DeleteVpc"}}`, inventory.TargetKey{}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok, err := Parse(c.body)
			if (err != nil) != c.err || ok != c.ok || got != c.want {
				t.Errorf("Parse = %+v, %v, %v; want %+v, %v, err=%v", got, ok, err, c.want, c.ok, c.err)
			}
		})
	}
}

// fakeSQS returns the queued batches once, then waits like a long poll with no messages.
type fakeSQS struct {
	mu      sync.Mutex
	batches [][]string
	deleted []string
}

func (f *fakeSQS) ReceiveMessage(ctx context.Context, _ *sqs.ReceiveMessageInput, _ ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error) {
	f.mu.Lock()
	if len(f.batches) > 0 {
		batch := f.batches[0]
		f.batches = f.batches[1:]
		f.mu.Unlock()
		out := &sqs.ReceiveMessageOutput{}
		for i, body := range batch {
			out.Messages = append(out.Messages, sqstypes.Message{Body: aws.String(body), ReceiptHandle: aws.String(fmt.Sprintf("rh-%d", i))})
		}
		return out, nil
	}
	f.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(50 * time.Millisecond):
		return &sqs.ReceiveMessageOutput{}, nil
	}
}

func (f *fakeSQS) DeleteMessageBatch(_ context.Context, in *sqs.DeleteMessageBatchInput, _ ...func(*sqs.Options)) (*sqs.DeleteMessageBatchOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range in.Entries {
		f.deleted = append(f.deleted, aws.ToString(e.ReceiptHandle))
	}
	return &sqs.DeleteMessageBatchOutput{}, nil
}

func TestPollerDebouncesAndDeletes(t *testing.T) {
	a := event("CreateSubnet", "111111111111", "")
	b := event("DeleteSubnet", "222222222222", "")
	api := &fakeSQS{batches: [][]string{{a, a, "garbage"}, {b, a}}}

	flushed := make(chan []inventory.TargetKey, 10)
	p := &Poller{SQS: api, QueueURL: "q", Debounce: 200 * time.Millisecond, Log: logr.Discard(),
		Sink: func(_ context.Context, changed []inventory.TargetKey) error {
			flushed <- changed
			return nil
		}}
	go func() { _ = p.Start(t.Context()) }()

	var got []inventory.TargetKey
	select {
	case got = <-flushed:
	case <-time.After(5 * time.Second):
		t.Fatal("no flush")
	}
	slices.SortFunc(got, func(x, y inventory.TargetKey) int { return strings.Compare(x.String(), y.String()) })
	want := []inventory.TargetKey{{Account: "111111111111", Region: "eu-central-1"}, {Account: "222222222222", Region: "eu-central-1"}}
	if !slices.Equal(got, want) {
		t.Errorf("flushed %v, want %v (one entry per target)", got, want)
	}
	select {
	case extra := <-flushed:
		t.Errorf("unexpected second flush %v", extra)
	case <-time.After(500 * time.Millisecond):
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.deleted) != 5 {
		t.Errorf("deleted %d messages, want all 5 including the unparsable one", len(api.deleted))
	}
}
