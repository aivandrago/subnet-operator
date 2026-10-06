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

package allocator

import (
	"errors"
	"slices"
	"testing"
)

func TestAllocateFirstFit(t *testing.T) {
	pool := Pool{
		CIDRs: []string{"10.0.0.0/16"},
		Used:  []string{"10.0.0.0/24", "10.0.1.0/24", "10.0.3.0/25"},
	}
	got, err := Allocate(pool, 24, 3)
	if err != nil {
		t.Fatal(err)
	}
	// 10.0.2.0/24 is free; 10.0.3.0/24 overlaps the /25 in use; then 10.0.4.0 and 10.0.5.0.
	want := []string{"10.0.2.0/24", "10.0.4.0/24", "10.0.5.0/24"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAllocateSpansSecondaryBlocks(t *testing.T) {
	pool := Pool{
		CIDRs: []string{"100.64.0.0/22", "10.0.0.0/23"},
		Used:  []string{"10.0.0.0/23"}, // the primary block is full
	}
	got, err := Allocate(pool, 24, 2)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"100.64.0.0/24", "100.64.1.0/24"}; !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAllocateNoSpace(t *testing.T) {
	pool := Pool{CIDRs: []string{"10.0.0.0/24"}, Used: []string{"10.0.0.0/25"}}
	_, err := Allocate(pool, 25, 2)
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("want ErrNoSpace, got %v", err)
	}
}

func TestAllocateSkipsBlocksLargerThanTheVPC(t *testing.T) {
	pool := Pool{CIDRs: []string{"10.0.0.0/26"}}
	if _, err := Allocate(pool, 24, 1); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("a /24 cannot come out of a /26, got %v", err)
	}
}

func TestAllocateIgnoresIPv6AndRejectsGarbage(t *testing.T) {
	got, err := Allocate(Pool{CIDRs: []string{"2001:db8::/56", "10.9.0.0/24"}}, 26, 1)
	if err != nil || !slices.Equal(got, []string{"10.9.0.0/26"}) {
		t.Fatalf("got %v, %v", got, err)
	}
	if _, err := Allocate(Pool{CIDRs: []string{"nope"}}, 24, 1); err == nil {
		t.Fatal("garbage must be rejected")
	}
}

func TestAllocateIsDeterministic(t *testing.T) {
	pool := Pool{CIDRs: []string{"10.0.0.0/16"}, Used: []string{"10.0.128.0/17"}}
	a, _ := Allocate(pool, 20, 4)
	b, _ := Allocate(pool, 20, 4)
	if !slices.Equal(a, b) || a[0] != "10.0.0.0/20" || a[3] != "10.0.48.0/20" {
		t.Fatalf("unexpected %v / %v", a, b)
	}
}
