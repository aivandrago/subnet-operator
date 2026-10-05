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
	"crypto/rand"
	"encoding/binary"
	"sync"
	"time"

	"hypersurgery.dev/subnet-operator/internal/inventory"
)

const (
	// throttleBackoffBase is the delay after the first throttled discovery of a target. It is
	// shorter than the default resync interval on purpose: the target is retried alone, outside
	// the burst of a full sync, and the sooner it gets through the less stale it is.
	throttleBackoffBase = time.Minute
	// throttleBackoffMax caps the delay, so a target throttled for hours is still tried twice
	// an hour and recovers within that once the account quietens down.
	throttleBackoffMax = 30 * time.Minute
)

// throttleBackoff remembers, per scope, the targets whose last discovery was throttled and
// when each may be tried again. Nothing else waits on it: a target that is not throttled is
// never in here, and one that succeeds or fails for any other reason leaves at once.
//
// It lives in memory only. After a restart every target is tried at the next full sync, which
// costs at most one throttled discovery per target before the backoff is rebuilt.
type throttleBackoff struct {
	mu sync.Mutex
	m  map[string]map[inventory.TargetKey]*backoffState
	// jitter returns a number in [0, 1); nil uses randomJitter.
	jitter func() float64
}

type backoffState struct {
	failures int
	until    time.Time
}

// backoffDelay is the delay after the given number of consecutive throttled discoveries:
// exponential from throttleBackoffBase up to throttleBackoffMax, with "equal jitter" — half
// of it fixed, half random. The fixed half keeps a throttled target from coming back early;
// the random half keeps targets throttled in the same burst from coming back together and
// being throttled together again.
func backoffDelay(failures int, jitter float64) time.Duration {
	d := throttleBackoffMax
	// Past 2^5 the base is over the cap anyway; stopping there also keeps the shift from
	// overflowing for a target that has been throttled for weeks.
	if failures <= 6 {
		d = min(throttleBackoffBase<<max(failures-1, 0), throttleBackoffMax)
	}
	return d/2 + time.Duration(jitter*float64(d/2))
}

// randomJitter returns a uniformly distributed number in [0, 1) from crypto/rand. The jitter
// is not a secret, but crypto/rand costs nothing at one call per throttled discovery and keeps
// the code free of math/rand, which security scanners flag wherever it appears.
func randomJitter() float64 {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never returns an error since Go 1.24
	return float64(binary.BigEndian.Uint64(b[:])>>11) / (1 << 53)
}

// throttled records a throttled discovery and returns when the target may be tried again.
func (b *throttleBackoff) throttled(scope string, key inventory.TargetKey, now time.Time) time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.m == nil {
		b.m = map[string]map[inventory.TargetKey]*backoffState{}
	}
	if b.m[scope] == nil {
		b.m[scope] = map[inventory.TargetKey]*backoffState{}
	}
	s := b.m[scope][key]
	if s == nil {
		s = &backoffState{}
		b.m[scope][key] = s
	}
	s.failures++
	jitter := randomJitter
	if b.jitter != nil {
		jitter = b.jitter
	}
	s.until = now.Add(backoffDelay(s.failures, jitter()))
	return s.until
}

// reset forgets the target: its discovery went through, or failed for a reason other than
// throttling, which backing off would not help.
func (b *throttleBackoff) reset(scope string, key inventory.TargetKey) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.m[scope], key)
}

// forget drops everything about a scope that no longer exists.
func (b *throttleBackoff) forget(scope string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.m, scope)
}

// state reports whether the target is backed off after throttling, and until when.
func (b *throttleBackoff) state(scope string, key inventory.TargetKey) (throttled bool, until time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.m[scope][key]
	if s == nil {
		return false, time.Time{}
	}
	return true, s.until
}

// backedOff reports whether the target's last discovery was throttled, due or not.
func (b *throttleBackoff) backedOff(scope string, key inventory.TargetKey) bool {
	throttled, _ := b.state(scope, key)
	return throttled
}

// waiting reports whether the target is backed off and may not be tried yet.
func (b *throttleBackoff) waiting(scope string, key inventory.TargetKey, now time.Time) bool {
	throttled, until := b.state(scope, key)
	return throttled && now.Before(until)
}

// due reports whether the target is backed off and its delay has run out.
func (b *throttleBackoff) due(scope string, key inventory.TargetKey, now time.Time) bool {
	throttled, until := b.state(scope, key)
	return throttled && !now.Before(until)
}

// next returns the earliest time one of the targets may be tried again, if any is backed off.
func (b *throttleBackoff) next(scope string, targets []inventory.Target) (time.Time, bool) {
	var earliest time.Time
	found := false
	for _, t := range targets {
		if throttled, until := b.state(scope, t.Key()); throttled && (!found || until.Before(earliest)) {
			earliest, found = until, true
		}
	}
	return earliest, found
}
