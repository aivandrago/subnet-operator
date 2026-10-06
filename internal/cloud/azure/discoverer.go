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
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"

	"hypersurgery.dev/subnet-operator/internal/inventory"
)

const (
	// retryMaxAttempts is how often one ARM call is tried before discovery sees its error, as
	// on AWS and GCP.
	retryMaxAttempts = 5
	// maxPace caps the delay the adaptive pacing puts before a call. ARM refills its read
	// buckets every second and Microsoft.Network counts reads over five minutes; a minute is
	// the longest a wait usefully is, anything longer is the controller's per-target backoff.
	maxPace = time.Minute
	// minPace is the first delay after a throttled call, and the delay while ARM reports few
	// reads left.
	minPace = time.Second
	// lowRemainingReads is where the pacing starts before ARM throttles: a tenth of the 250
	// reads a subscription's bucket holds, which it refills at 25 a second.
	lowRemainingReads = 25
)

// remainingReadsHeader is where ARM reports the reads left in the caller's bucket for the
// subscription.
const remainingReadsHeader = "x-ms-ratelimit-remaining-subscription-reads"

// Discoverer discovers Azure targets. It keeps the ARM clients per identity and subscription
// and the adaptive pacing per quota bucket across discoveries.
type Discoverer struct {
	opts Options

	mu sync.Mutex
	// credential is the operator's own credential, built at the first discovery.
	credential azcore.TokenCredential
	// credentials are the credentials of accounts' identities, by tenant and client ID
	// (resolve), each built at its first use (credentials.go).
	credentials map[Identity]azcore.TokenCredential
	clients     map[clientKey]*armnetwork.VirtualNetworksClient
	// writers are the write clients per identity and subscription (writer.go).
	writers map[clientKey]*writeClients
	// pace is the delay before the next call per quota bucket (identity and subscription):
	// raised on every throttled attempt to at least the Retry-After ARM asked for, up to
	// maxPace, halved on every success, and kept at minPace while ARM reports few reads left.
	pace map[string]time.Duration
	// watched are the subscriptions discovery was asked for, and networks where their virtual
	// networks are, by lowercase subscription and resource ID, as the last listing saw them:
	// what a change event, which names a resource and no location, is mapped to a target with
	// (changes.go).
	watched  map[string]bool
	networks map[string]map[string]string

	// sleep waits between attempts; tests replace it.
	sleep func(ctx context.Context, d time.Duration) error
}

type clientKey struct {
	identity     Identity
	subscription string
}

func newDiscoverer(opts Options) *Discoverer {
	return &Discoverer{
		opts:        opts,
		credentials: map[Identity]azcore.TokenCredential{},
		clients:     map[clientKey]*armnetwork.VirtualNetworksClient{},
		writers:     map[clientKey]*writeClients{},
		pace:        map[string]time.Duration{},
		watched:     map[string]bool{},
		networks:    map[string]map[string]string{},
		sleep:       sleepCtx,
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// clientFor returns the virtual networks client of an identity for a subscription.
func (d *Discoverer) clientFor(id Identity, subscription string) (*armnetwork.VirtualNetworksClient, error) {
	cred, err := d.credentialFor(id)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	key := clientKey{identity: id, subscription: subscription}
	if c, ok := d.clients[key]; ok {
		return c, nil
	}
	c, err := armnetwork.NewVirtualNetworksClient(subscription, cred, d.clientOptions())
	if err != nil {
		return nil, fmt.Errorf("virtual networks client for subscription %s: %w", subscription, err)
	}
	d.clients[key] = c
	return c, nil
}

// clientOptions are the ARM client options: the SDK does not retry, so that throttling reaches
// the adaptive pacing below (the SDK would wait out each Retry-After inside one call), and does
// not register resource providers, which is a write.
func (d *Discoverer) clientOptions() *arm.ClientOptions {
	opts := &arm.ClientOptions{ClientOptions: d.azcoreOptions(), DisableRPRegistration: true}
	opts.Retry = policy.RetryOptions{MaxRetries: -1}
	return opts
}

// call runs one ARM call with the bucket's pacing, retrying it while it is throttled, or, for
// a write, while another operation holds the virtual network (isBusy). fn returns the last HTTP
// response it got, whose rate limit headers steer the pacing. A call still throttled after the
// last attempt returns its error, which Discover wraps in inventory.ErrThrottled.
func (d *Discoverer) call(ctx context.Context, target inventory.Target, bucket, operation string,
	fn func(ctx context.Context) (*http.Response, error)) error {
	var err error
	for attempt := 1; ; attempt++ {
		if werr := d.sleep(ctx, d.currentPace(bucket)); werr != nil {
			return werr
		}
		var resp *http.Response
		resp, err = fn(ctx)
		throttled, retryAfter := throttling(err)
		busy := !throttled && isBusy(err)
		if !throttled && !busy {
			d.adjustPace(bucket, false, 0, remainingReads(resp))
			return err
		}
		if busy {
			retryAfter = retryAfterOf(err)
		}
		d.adjustPace(bucket, true, retryAfter, -1)
		if throttled && d.opts.OnThrottle != nil {
			d.opts.OnThrottle(target, operation)
		}
		if attempt >= retryMaxAttempts {
			return err
		}
	}
}

func (d *Discoverer) currentPace(bucket string) time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pace[bucket]
}

// adjustPace doubles the bucket's delay on a throttled attempt, to at least what Retry-After
// asked for, and halves it on a success; while ARM reports fewer than lowRemainingReads reads
// left (remaining is -1 when it did not say), it stays at least minPace.
func (d *Discoverer) adjustPace(bucket string, throttled bool, retryAfter time.Duration, remaining int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.pace[bucket]
	switch {
	case throttled:
		p = min(max(2*p, minPace, retryAfter), maxPace)
	case p < minPace:
		p = 0
	default:
		p /= 2
	}
	if !throttled && remaining >= 0 && remaining < lowRemainingReads {
		p = max(p, minPace)
	}
	if p == 0 {
		delete(d.pace, bucket)
		return
	}
	d.pace[bucket] = p
}

// throttling reports whether the error is ARM rate-limiting the caller (HTTP 429), and how long
// it asked the caller to wait. Microsoft.Network also answers 429 with the code
// RetryableErrorDueToAnotherOperation while another operation holds the resource; for reads
// that is as good a reason to wait and retry, and it is treated the same.
func throttling(err error) (bool, time.Duration) {
	respErr, ok := errors.AsType[*azcore.ResponseError](err)
	if !ok || respErr.StatusCode != http.StatusTooManyRequests {
		return false, 0
	}
	return true, retryAfter(respErr.RawResponse)
}

// retryAfterOf is the Retry-After of a failed call.
func retryAfterOf(err error) time.Duration {
	if respErr, ok := errors.AsType[*azcore.ResponseError](err); ok {
		return retryAfter(respErr.RawResponse)
	}
	return 0
}

func isThrottle(err error) bool {
	throttled, _ := throttling(err)
	return throttled || throttledToken(err)
}

// retryAfter reads how long ARM asked the caller to wait: retry-after-ms, x-ms-retry-after-ms,
// or Retry-After in seconds or as a date.
func retryAfter(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	for _, h := range []string{"retry-after-ms", "x-ms-retry-after-ms"} {
		if ms, err := strconv.ParseInt(resp.Header.Get(h), 10, 64); err == nil && ms > 0 {
			return time.Duration(ms) * time.Millisecond
		}
	}
	v := resp.Header.Get("Retry-After")
	if s, err := strconv.ParseInt(v, 10, 64); err == nil && s > 0 {
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0)
	}
	return 0
}

// remainingReads is what ARM reported as the reads left in the bucket, -1 when it did not.
func remainingReads(resp *http.Response) int {
	if resp == nil {
		return -1
	}
	n, err := strconv.Atoi(resp.Header.Get(remainingReadsHeader))
	if err != nil {
		return -1
	}
	return n
}

// Discover implements inventory.Discoverer. An error that is still a throttling error after
// the retries wraps inventory.ErrThrottled.
func (d *Discoverer) Discover(ctx context.Context, target inventory.Target) (*inventory.Snapshot, error) {
	id, err := identityOf(target)
	if err != nil {
		return nil, err
	}
	d.watch(target.Account)
	c, err := d.clientFor(id, target.Account)
	if err != nil {
		return nil, err
	}
	snap, err := d.discoverTarget(ctx, c, id, target)
	switch {
	case err == nil:
		return snap, nil
	case isThrottle(err):
		return nil, fmt.Errorf("%w: %w", inventory.ErrThrottled, err)
	case !id.Own() && !isCredentialError(err):
		// ARM names the principal by its object ID; the scope names it by its client ID.
		return nil, fmt.Errorf("as %s: %w", id, err)
	}
	return nil, err
}

func isCredentialError(err error) bool {
	_, ok := errors.AsType[*CredentialError](err)
	return ok
}

// armBucket is the ARM read quota a call counts against: per subscription and per principal.
func armBucket(id Identity, subscription string) string {
	return "arm/" + id.String() + "/" + subscription
}
