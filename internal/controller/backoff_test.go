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

package controller

import (
	"testing"
	"time"

	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// Each throttled discovery doubles the delay until the cap, and jitter only ever takes up to
// half of it away: a throttled target never comes back sooner than half its delay.
func TestThrottleBackoffGrowsExponentiallyUpToTheCap(t *testing.T) {
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute,
		16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, full := range want {
		failures := i + 1
		if got := backoffDelay(failures, 0.999999); got > full || got < full-time.Second {
			t.Errorf("failures=%d, highest jitter: delay %v, want just under %v", failures, got, full)
		}
		if got := backoffDelay(failures, 0); got != full/2 {
			t.Errorf("failures=%d, no jitter: delay %v, want %v", failures, got, full/2)
		}
	}
	if got := backoffDelay(10_000, 0.5); got > throttleBackoffMax {
		t.Errorf("a target throttled for weeks waits %v, over the %v cap", got, throttleBackoffMax)
	}
}

// Targets throttled in the same burst must not all come back in the same second, or they are
// throttled together again.
func TestThrottleBackoffSpreadsTargetsThrottledTogether(t *testing.T) {
	b := &throttleBackoff{}
	now := time.Now()
	seen := map[time.Time]bool{}
	for i := range 20 {
		key := inventory.TargetKey{Account: "111111111111", Region: "region-" + string(rune('a'+i))}
		until := b.throttled("org", key, now)
		if d := until.Sub(now); d < throttleBackoffBase/2 || d > throttleBackoffBase {
			t.Fatalf("first delay %v, want between %v and %v", d, throttleBackoffBase/2, throttleBackoffBase)
		}
		seen[until] = true
	}
	if len(seen) < 10 {
		t.Errorf("20 targets throttled at once come back at only %d distinct times", len(seen))
	}
}

// The default jitter stays in [0, 1) and covers the interval, so the random half of a delay
// can take any value in it.
func TestRandomJitterIsInUnitInterval(t *testing.T) {
	low, high := false, false
	for range 1000 {
		j := randomJitter()
		if j < 0 || j >= 1 {
			t.Fatalf("randomJitter() = %v, want in [0, 1)", j)
		}
		low = low || j < 0.25
		high = high || j >= 0.75
	}
	if !low || !high {
		t.Errorf("1000 draws never fell below 0.25 (%v) or above 0.75 (%v)", low, high)
	}
}

// A success, or a failure that is not throttling, starts the target over: the next throttle
// waits the base delay again, not the doubled one.
func TestThrottleBackoffResetsAfterSuccess(t *testing.T) {
	b := &throttleBackoff{jitter: func() float64 { return 0.999999 }}
	key := inventory.TargetKey{Account: "111111111111", Region: "eu-central-1"}
	now := time.Now()

	b.throttled("org", key, now)
	b.throttled("org", key, now)
	if until := b.throttled("org", key, now); until.Sub(now) < 3*time.Minute {
		t.Fatalf("third throttle waits %v, want close to 4m", until.Sub(now))
	}
	if !b.waiting("org", key, now) || b.due("org", key, now) {
		t.Fatal("a freshly throttled target must be waiting, not due")
	}
	if !b.due("org", key, now.Add(5*time.Minute)) {
		t.Fatal("the target must be due once its delay has run out")
	}
	if b.backedOff("other", key) {
		t.Fatal("backoff leaked into another scope")
	}

	b.reset("org", key)
	if b.backedOff("org", key) {
		t.Fatal("reset target is still backed off")
	}
	if until := b.throttled("org", key, now); until.Sub(now) > throttleBackoffBase {
		t.Errorf("after a reset the first throttle waits %v, want at most %v", until.Sub(now), throttleBackoffBase)
	}
}
