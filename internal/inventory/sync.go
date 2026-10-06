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

package inventory

import (
	"context"
	"errors"
	"sync"
)

// A sync is one pass of a scope over some of its targets: the discoveries one reconcile runs
// side by side. A provider whose API answers for more than one target at a time (Azure lists a
// subscription's virtual networks whatever their location) reads such an answer once per sync
// and hands it to every target that needs it (Shared).
//
// What a sync shares dies with it. It is not a cache: the next sync, the resync of the targets
// a change event names and the retry of a target after its backoff are syncs of their own and
// read the cloud again.

type syncKey struct{}

// syncState is what the discoveries of one sync share, by the key each provider gives it.
type syncState struct {
	mu    sync.Mutex
	ended bool
	calls map[any]*sharedCall
}

// sharedCall is one read of a sync: in flight until done is closed, then its result.
type sharedCall struct {
	done  chan struct{}
	value any
	err   error
	// abandoned is set when the caller that made the read gave up on it (its context ended),
	// which says nothing about the cloud: the next caller reads for itself.
	abandoned bool
}

// WithSync returns a context for the discoveries of one sync, and the function that ends the
// sync. Discoveries given this context share what they read through Shared; after end, and in
// any other context, Shared reads every time.
func WithSync(ctx context.Context) (_ context.Context, end func()) {
	s := &syncState{calls: map[any]*sharedCall{}}
	return context.WithValue(ctx, syncKey{}, s), func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.ended, s.calls = true, nil
	}
}

// Shared runs read once per sync and key: the first discovery to ask for the key reads, the
// ones that ask while it reads wait for it, and the ones that ask later in the same sync get
// what it read. All of them get the same value, which they must not change, or the same error:
// a read that failed or was throttled fails every discovery of the sync that needs it, and is
// read again only by the next sync. The key must tell apart everything the read depends on.
//
// Outside a sync (a context WithSync did not make, or one whose sync has ended) read runs every
// time. A read its own caller abandoned, because that caller's context ended, is not shared.
func Shared[T any](ctx context.Context, key any, read func(ctx context.Context) (T, error)) (T, error) {
	s, _ := ctx.Value(syncKey{}).(*syncState)
	if s == nil {
		return read(ctx)
	}
	for {
		s.mu.Lock()
		if s.ended {
			s.mu.Unlock()
			return read(ctx)
		}
		call, running := s.calls[key]
		if !running {
			call = &sharedCall{done: make(chan struct{})}
			s.calls[key] = call
			s.mu.Unlock()
			v, err := read(ctx)
			s.mu.Lock()
			call.value, call.err = v, err
			if err != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
				call.abandoned = true
				if !s.ended && s.calls[key] == call {
					delete(s.calls, key)
				}
			}
			s.mu.Unlock()
			close(call.done)
			return v, err
		}
		s.mu.Unlock()
		select {
		case <-call.done:
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		}
		if call.abandoned {
			continue
		}
		v, _ := call.value.(T)
		return v, call.err
	}
}
