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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/api/googleapi"
	iamcredentials "google.golang.org/api/iamcredentials/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"hypersurgery.dev/subnet-operator/internal/inventory"
)

const (
	// retryMaxAttempts is how often one Google API call is tried before discovery sees its
	// error, as on AWS.
	retryMaxAttempts = 5
	// maxPace caps the delay the adaptive pacing puts before a call. Compute and Resource
	// Manager quotas are per-minute buckets, so a minute is the longest a wait can usefully
	// be; anything longer is the controller's per-target backoff, which frees the slot.
	maxPace = time.Minute
	// minPace is the first delay after a throttled call.
	minPace = time.Second
)

// Discoverer discovers GCP targets. It keeps the API clients per identity and the adaptive
// pacing per quota bucket across discoveries.
type Discoverer struct {
	opts Options

	mu      sync.Mutex
	clients map[Identity]*clients
	// iam is the IAM Service Account Credentials client of the operator's own identity,
	// which impersonates the accounts' service accounts.
	iam *iamcredentials.Service
	// pace is the delay before the next call per quota bucket (project and region for
	// Compute, the endpoint's location for Resource Manager): doubled on every throttled
	// attempt up to maxPace, halved on every success.
	pace map[string]time.Duration
	// projectNumbers caches project ID to number. Resource Manager names resources by
	// number, and a project's number never changes.
	projectNumbers map[string]string

	// sleep waits between attempts; tests replace it.
	sleep func(ctx context.Context, d time.Duration) error
}

func newDiscoverer(opts Options) *Discoverer {
	return &Discoverer{
		opts:           opts,
		clients:        map[Identity]*clients{},
		pace:           map[string]time.Duration{},
		projectNumbers: map[string]string{},
		sleep:          sleepCtx,
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

// clientsFor returns the API clients of an identity, building them once, and for a service
// account makes sure the operator can act as it: a token that cannot be had fails here, with
// an ImpersonationError that says what to grant, rather than as the first API call's
// transport error. The token is cached, so this costs a call only when it is renewed.
func (d *Discoverer) clientsFor(ctx context.Context, id Identity) (*clients, error) {
	c, err := d.buildClients(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.token != nil {
		if _, err := c.token.Token(); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (d *Discoverer) buildClients(ctx context.Context, id Identity) (*clients, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if c, ok := d.clients[id]; ok {
		return c, nil
	}
	// The clients outlive the call that builds them. Credentials that keep the context they
	// were made with (a Workload Identity Federation token exchange does) would fail every
	// renewal after that call ended.
	ctx = context.WithoutCancel(ctx)
	opts, token, err := d.credentials(ctx, id)
	if err != nil {
		return nil, err
	}
	c, err := newClients(ctx, opts, d.opts)
	if err != nil {
		return nil, err
	}
	c.token = token
	d.clients[id] = c
	return c, nil
}

// call runs one Google API call with the target's pacing, retrying it while it is throttled.
// bucket names the quota the call counts against. A call still throttled after the last
// attempt returns its error, which Discover wraps in inventory.ErrThrottled.
func (d *Discoverer) call(ctx context.Context, target inventory.Target, bucket, operation string,
	fn func(ctx context.Context) error) error {
	var err error
	for attempt := 1; ; attempt++ {
		if werr := d.sleep(ctx, d.currentPace(bucket)); werr != nil {
			return werr
		}
		err = fn(ctx)
		if !isThrottle(err) {
			d.adjustPace(bucket, false)
			return err
		}
		d.adjustPace(bucket, true)
		if d.opts.OnThrottle != nil {
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

func (d *Discoverer) adjustPace(bucket string, throttled bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := d.pace[bucket]
	switch {
	case throttled:
		p = min(max(2*p, minPace), maxPace)
	case p < minPace:
		p = 0
	default:
		p /= 2
	}
	if p == 0 {
		delete(d.pace, bucket)
		return
	}
	d.pace[bucket] = p
}

// throttleReasons are the error reasons Google APIs rate-limit with. A 403 with any other
// reason is a missing permission and must not be retried.
var throttleReasons = map[string]bool{
	"rateLimitExceeded":     true,
	"userRateLimitExceeded": true,
	"RATE_LIMIT_EXCEEDED":   true,
}

// isThrottle reports whether the error is Google rate-limiting the caller: HTTP 429, a 403
// with reason rateLimitExceeded or userRateLimitExceeded, or gRPC RESOURCE_EXHAUSTED.
func isThrottle(err error) bool {
	if err == nil {
		return false
	}
	if gerr, ok := errors.AsType[*googleapi.Error](err); ok {
		if gerr.Code == http.StatusTooManyRequests {
			return true
		}
		if gerr.Code == http.StatusForbidden {
			for _, item := range gerr.Errors {
				if throttleReasons[item.Reason] {
					return true
				}
			}
		}
	}
	if aerr, ok := errors.AsType[*apierror.APIError](err); ok {
		if aerr.HTTPCode() == http.StatusTooManyRequests || throttleReasons[aerr.Reason()] {
			return true
		}
		if s := aerr.GRPCStatus(); s != nil && s.Code() == codes.ResourceExhausted {
			return true
		}
	}
	if s, ok := status.FromError(err); ok && s.Code() == codes.ResourceExhausted {
		return true
	}
	return false
}

// Discover implements inventory.Discoverer. An error that is still a throttling error after
// the retries wraps inventory.ErrThrottled.
func (d *Discoverer) Discover(ctx context.Context, target inventory.Target) (*inventory.Snapshot, error) {
	id, err := identityOf(target)
	if err != nil {
		return nil, err
	}
	c, err := d.clientsFor(ctx, id)
	if err != nil {
		if isThrottle(err) {
			return nil, fmt.Errorf("%w: %w", inventory.ErrThrottled, err)
		}
		return nil, err
	}
	snap, err := d.discoverTarget(ctx, c, target)
	if err != nil && isThrottle(err) {
		return nil, fmt.Errorf("%w: %w", inventory.ErrThrottled, err)
	}
	return snap, err
}

// computeBucket is the Compute quota a call counts against: reads per project and region, or
// the project's global reads.
func computeBucket(project, region string) string {
	if region == "" {
		region = globalLocation
	}
	return "compute/" + project + "/" + region
}

func resourceManagerBucket(location string) string {
	return "resourcemanager/" + strings.ToLower(location)
}
