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
	"context"
	"sync"
	"time"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"hypersurgery.dev/subnet-operator/internal/inventory"
)

const (
	// creatorCacheSize bounds the cache: attribution is a nicety, not a ledger, and the
	// operator must not grow a map for every resource an account ever created.
	creatorCacheSize = 512
	// creatorCacheTTL is how long a creation stays interesting. A resource nobody imported
	// within a day is a decision for a human, not for the policy.
	creatorCacheTTL = 24 * time.Hour
)

// CreatorCache remembers who created a resource, from the change events of the providers
// (on AWS, CloudTrail through EventBridge and SQS).
// It is best-effort by design: a miss means the policy falls back to inheritance or account
// defaults, which is exactly what should happen for a resource created before the operator.
//
// The zero value is ready to use.
type CreatorCache struct {
	mu      sync.Mutex
	entries map[string]creatorEntry
	now     func() time.Time // replaced in tests
}

type creatorEntry struct {
	principal string
	seen      time.Time
}

func (c *CreatorCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// Record stores the principals of newly created resources. It is the Created side of every
// provider's provider.EventSink.
func (c *CreatorCache) Record(ctx context.Context, created []inventory.Creation) {
	log := logf.FromContext(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]creatorEntry, creatorCacheSize)
	}
	now := c.clock()
	for _, e := range created {
		if e.ResourceID == "" || e.Principal == "" {
			continue
		}
		c.entries[e.ResourceID] = creatorEntry{principal: e.Principal, seen: now}
		log.V(1).Info("recorded creator", "resource", e.ResourceID, "principal", e.Principal, "event", e.EventName)
	}
	c.evictLocked(now)
}

// Lookup returns the principal that created the resource, or "" when it is unknown.
func (c *CreatorCache) Lookup(resourceID string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[resourceID]
	if !ok || c.clock().Sub(e.seen) > creatorCacheTTL {
		return ""
	}
	return e.principal
}

// evictLocked drops expired entries, and then the oldest ones until the cache fits.
func (c *CreatorCache) evictLocked(now time.Time) {
	for id, e := range c.entries {
		if now.Sub(e.seen) > creatorCacheTTL {
			delete(c.entries, id)
		}
	}
	for len(c.entries) > creatorCacheSize {
		oldestID, oldest := "", time.Time{}
		for id, e := range c.entries {
			if oldestID == "" || e.seen.Before(oldest) {
				oldestID, oldest = id, e.seen
			}
		}
		delete(c.entries, oldestID)
	}
}
