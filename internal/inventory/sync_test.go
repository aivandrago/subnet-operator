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
	"sync/atomic"
	"testing"
)

// Discoveries of one sync that ask for the same key at the same moment are answered by one
// read, and so are the ones that ask later in the sync.
func TestSharedReadsOncePerSyncAndKey(t *testing.T) {
	ctx, end := WithSync(context.Background())
	defer end()
	var reads atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	read := func(ctx context.Context) (int, error) {
		if reads.Add(1) == 1 {
			close(started)
		}
		<-release
		return 42, ctx.Err()
	}
	const callers = 8
	var wg sync.WaitGroup
	got := make([]int, callers)
	for i := range callers {
		wg.Go(func() {
			v, err := Shared(ctx, "listing", read)
			if err != nil {
				t.Errorf("caller %d: %v", i, err)
			}
			got[i] = v
		})
	}
	<-started
	close(release)
	wg.Wait()
	if v, err := Shared(ctx, "listing", read); v != 42 || err != nil {
		t.Errorf("a later caller of the sync got %d, %v", v, err)
	}
	if n := reads.Load(); n != 1 {
		t.Errorf("%d reads for %d callers of one sync, want 1", n, callers+1)
	}
	for i, v := range got {
		if v != 42 {
			t.Errorf("caller %d got %d", i, v)
		}
	}
	if v, err := Shared(ctx, "another", func(context.Context) (int, error) { return 7, nil }); v != 7 || err != nil {
		t.Errorf("another key got %d, %v, want a read of its own", v, err)
	}
}

// A read that failed fails every caller of the sync, and is not what the next sync is answered
// with; nor is anything, once the sync has ended or where there is none.
func TestSharedKeepsNothingBeyondTheSync(t *testing.T) {
	boom := errors.New("boom")
	var reads int
	read := func(context.Context) (string, error) {
		reads++
		if reads == 1 {
			return "", boom
		}
		return "fresh", nil
	}
	first, end := WithSync(context.Background())
	for range 3 {
		if _, err := Shared(first, "k", read); !errors.Is(err, boom) {
			t.Fatalf("a caller of the failed sync got %v, want the failure", err)
		}
	}
	if reads != 1 {
		t.Fatalf("%d reads in the failed sync, want 1", reads)
	}
	end()

	second, end2 := WithSync(context.Background())
	if v, err := Shared(second, "k", read); v != "fresh" || err != nil {
		t.Errorf("the next sync got %q, %v, want a read of its own", v, err)
	}
	end2()
	if _, _ = Shared(second, "k", read); reads != 3 {
		t.Errorf("%d reads, want one more after the sync ended", reads)
	}
	if _, _ = Shared(first, "k", read); reads != 4 {
		t.Errorf("%d reads, want one more in the ended first sync", reads)
	}
	for range 2 {
		_, _ = Shared(context.Background(), "k", read)
	}
	if reads != 6 {
		t.Errorf("%d reads, want every call outside a sync to read", reads)
	}
}

// A read whose own caller gave up says nothing about the cloud: the callers still waiting read
// for themselves.
func TestSharedDoesNotShareAnAbandonedRead(t *testing.T) {
	ctx, end := WithSync(context.Background())
	defer end()
	leader, cancel := context.WithCancel(ctx)
	reading := make(chan struct{})
	var reads atomic.Int32
	read := func(ctx context.Context) (string, error) {
		if reads.Add(1) == 1 {
			close(reading)
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "mine", nil
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		if _, err := Shared(leader, "k", read); !errors.Is(err, context.Canceled) {
			t.Errorf("the caller that gave up got %v", err)
		}
	})
	<-reading
	wg.Go(func() {
		if v, err := Shared(ctx, "k", read); v != "mine" || err != nil {
			t.Errorf("the caller still waiting got %q, %v, want a read of its own", v, err)
		}
	})
	cancel()
	wg.Wait()

	// And a caller that gives up while waiting does not wait for the read.
	blocked, unblock := make(chan struct{}), make(chan struct{})
	go func() {
		_, _ = Shared(ctx, "slow", func(context.Context) (string, error) {
			close(blocked)
			<-unblock
			return "", nil
		})
	}()
	<-blocked
	waiter, stop := context.WithCancel(ctx)
	stop()
	if _, err := Shared(waiter, "slow", read); !errors.Is(err, context.Canceled) {
		t.Errorf("a waiter whose context ended got %v", err)
	}
	close(unblock)
}
