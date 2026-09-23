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
	"fmt"
	"testing"
	"time"

	"hypersurgery.dev/subnet-operator/internal/events"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

func creation(id, principal string) events.Creation {
	return events.Creation{
		Target:     inventory.TargetKey{Account: "111111111111", Region: "eu-central-1"},
		ResourceID: id, Principal: principal, EventName: "CreateSubnet",
	}
}

func TestCreatorCacheRecordsAndLooksUp(t *testing.T) {
	c := &CreatorCache{}
	c.Record(context.Background(), []events.Creation{creation("subnet-1", "assumed-role/payments/maria.k")})

	if got := c.Lookup("subnet-1"); got != "assumed-role/payments/maria.k" {
		t.Errorf("lookup = %q", got)
	}
	if got := c.Lookup("subnet-unknown"); got != "" {
		t.Errorf("an unknown resource must return nothing, got %q", got)
	}
}

func TestCreatorCacheIgnoresIncompleteEvents(t *testing.T) {
	c := &CreatorCache{}
	c.Record(context.Background(), []events.Creation{
		creation("", "assumed-role/x"),
		creation("subnet-2", ""),
	})
	if len(c.entries) != 0 {
		t.Errorf("entries = %v, want none: neither event names both a resource and a creator", c.entries)
	}
}

func TestCreatorCacheExpires(t *testing.T) {
	now := time.Now()
	c := &CreatorCache{now: func() time.Time { return now }}
	c.Record(context.Background(), []events.Creation{creation("subnet-3", "assumed-role/ops/anton")})

	now = now.Add(creatorCacheTTL + time.Minute)
	if got := c.Lookup("subnet-3"); got != "" {
		t.Errorf("lookup = %q, want nothing: a day-old creation is a decision for a human", got)
	}

	// The next write sweeps the expired entry out rather than letting the map grow.
	c.Record(context.Background(), []events.Creation{creation("subnet-4", "assumed-role/ops/anton")})
	if _, ok := c.entries["subnet-3"]; ok {
		t.Error("the expired entry is still in the map")
	}
}

func TestCreatorCacheStaysBounded(t *testing.T) {
	now := time.Now()
	c := &CreatorCache{now: func() time.Time { return now }}

	// Fill past the limit, each entry a second newer than the last.
	for i := range creatorCacheSize + 50 {
		now = now.Add(time.Second)
		c.Record(context.Background(), []events.Creation{creation(fmt.Sprintf("subnet-%d", i), "assumed-role/ops/anton")})
	}

	if len(c.entries) > creatorCacheSize {
		t.Fatalf("entries = %d, want at most %d", len(c.entries), creatorCacheSize)
	}
	// The oldest went first, the newest is still there.
	if got := c.Lookup("subnet-0"); got != "" {
		t.Errorf("the oldest entry survived eviction: %q", got)
	}
	if got := c.Lookup(fmt.Sprintf("subnet-%d", creatorCacheSize+49)); got == "" {
		t.Error("the newest entry was evicted")
	}
}
