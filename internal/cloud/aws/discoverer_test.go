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

package aws

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/aws/retry"
	"github.com/aws/aws-sdk-go-v2/credentials"

	"hypersurgery.dev/subnet-operator/internal/inventory"
)

const throttledAccount = "111111111111"

// throttlingEC2 is an EC2 endpoint that answers the first `throttle` requests with
// RequestLimitExceeded, the way EC2 does, and every later one with an empty DescribeVpcs.
// An empty VPC list ends discovery after one call, so each request is one attempt of it.
type throttlingEC2 struct {
	throttle int64
	code     string
	requests atomic.Int64
}

func (e *throttlingEC2) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/xml")
	if e.requests.Add(1) <= e.throttle {
		code := e.code
		if code == "" {
			code = "RequestLimitExceeded"
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, `<Response><Errors><Error><Code>%s</Code><Message>Request limit exceeded.</Message></Error></Errors><RequestID>r-1</RequestID></Response>`, code)
		return
	}
	_, _ = fmt.Fprint(w, `<DescribeVpcsResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/"><requestId>r-2</requestId><vpcSet/></DescribeVpcsResponse>`)
}

type throttleLog struct {
	mu    sync.Mutex
	calls []string
}

func (l *throttleLog) record(t inventory.Target, op string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, t.Scope+"|"+t.Account+"/"+t.Region+"|"+op)
}

func (l *throttleLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.calls)
}

// discovererFor points a Discoverer at the fake endpoint, as the operator's own account, with
// no delay between retries so the test does not sleep through the SDK's backoff.
func discovererFor(t *testing.T, endpoint http.Handler) (*Discoverer, *throttleLog) {
	t.Helper()
	srv := httptest.NewServer(endpoint)
	t.Cleanup(srv.Close)
	d := newDiscoverer(aws.Config{
		Region:       "eu-central-1",
		Credentials:  credentials.NewStaticCredentialsProvider("AKID", "SECRET", ""),
		BaseEndpoint: aws.String(srv.URL),
	})
	d.ownAccount = throttledAccount
	d.retryBackoff = retry.BackoffDelayerFunc(func(int, error) (time.Duration, error) { return 0, nil })
	log := &throttleLog{}
	d.OnThrottle = log.record
	return d, log
}

func throttledTarget() inventory.Target {
	return inventory.Target{Scope: "org", Account: throttledAccount, Region: "eu-central-1"}
}

// A burst of RequestLimitExceeded that the SDK rides out is not a failed discovery, but it is
// counted: that count is what shows an account getting busier before anything goes stale. The
// SDK's default of three attempts would have failed this discovery.
//
// The retry backoff is zero here, so any time the retries take is the adaptive mode's client
// side rate limiter slowing down after the throttles — which is what it is there for.
func TestThrottledCallsAreRetriedAndCounted(t *testing.T) {
	t.Parallel()
	endpoint := &throttlingEC2{throttle: 4}
	d, log := discovererFor(t, endpoint)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := d.Discover(ctx, throttledTarget()); err != nil {
		t.Fatalf("discovery failed after %d requests: %v", endpoint.requests.Load(), err)
	}
	if took := time.Since(start); took < time.Second {
		t.Errorf("retries took %v: the client did not slow down after being throttled", took)
	}
	if got := endpoint.requests.Load(); got != 5 {
		t.Errorf("requests = %d, want 5 (four throttled attempts and one that went through)", got)
	}
	if got := log.count(); got != 4 {
		t.Fatalf("throttled attempts reported = %d, want 4: %v", got, log.calls)
	}
	if want := "org|" + throttledAccount + "/eu-central-1|DescribeVpcs"; log.calls[0] != want {
		t.Errorf("reported %q, want %q", log.calls[0], want)
	}
}

// Throttling that outlasts the retries is reported as throttling, so the controller can back
// the target off instead of calling it unreachable.
func TestPersistentThrottlingIsReportedAsThrottled(t *testing.T) {
	t.Parallel()
	d, log := discovererFor(t, &throttlingEC2{throttle: 1 << 30})

	_, err := d.Discover(context.Background(), throttledTarget())
	if !errors.Is(err, inventory.ErrThrottled) {
		t.Fatalf("err = %v, want it to wrap inventory.ErrThrottled", err)
	}
	if got := log.count(); got != retryMaxAttempts {
		t.Errorf("throttled attempts reported = %d, want %d", got, retryMaxAttempts)
	}
}

// An access problem is not throttling, however it is retried, and must still read as unreachable.
func TestAccessErrorsAreNotThrottling(t *testing.T) {
	d, log := discovererFor(t, &throttlingEC2{throttle: 1 << 30, code: "UnauthorizedOperation"})

	_, err := d.Discover(context.Background(), throttledTarget())
	if err == nil || errors.Is(err, inventory.ErrThrottled) {
		t.Fatalf("err = %v, want a plain error", err)
	}
	if got := log.count(); got != 0 {
		t.Errorf("throttled attempts reported = %d, want 0", got)
	}
}

// The adaptive mode only slows down if it remembers: one retryer per account and region, kept
// across discoveries and shared by every scope that reads the pair.
func TestAdaptiveRetryerIsKeptPerAccountAndRegion(t *testing.T) {
	d := newDiscoverer(aws.Config{})
	creds := credentials.NewStaticCredentialsProvider("AKID", "SECRET", "")
	a := inventory.Target{Scope: "one", Account: throttledAccount, Region: "eu-central-1"}
	sameInOtherScope := inventory.Target{Scope: "two", Account: throttledAccount, Region: "eu-central-1"}
	otherRegion := inventory.Target{Scope: "one", Account: throttledAccount, Region: "us-east-1"}

	first := d.ec2Client(a, creds).Options().Retryer
	if _, ok := first.(*retry.AdaptiveMode); !ok {
		t.Fatalf("retryer is %T, want *retry.AdaptiveMode", first)
	}
	if first.MaxAttempts() != retryMaxAttempts {
		t.Errorf("max attempts = %d, want %d", first.MaxAttempts(), retryMaxAttempts)
	}
	if again := d.ec2Client(sameInOtherScope, creds).Options().Retryer; again != first {
		t.Error("a second discovery of the same account/region got a fresh retryer")
	}
	if other := d.ec2Client(otherRegion, creds).Options().Retryer; other == first {
		t.Error("another region shares the retryer, and with it the rate limit")
	}
}
