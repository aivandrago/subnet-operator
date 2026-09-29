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

// Package allocator picks free CIDR blocks out of a VPC.
//
// It is deliberately dumb: first fit, lowest address first, over the VPC's IPv4 blocks,
// skipping anything that overlaps a block already in use. Being deterministic matters more
// than being clever, because two people looking at the same inventory should predict the
// same answer.
package allocator

import (
	"errors"
	"fmt"
	"math/big"
	"net/netip"
	"slices"
)

// ErrNoSpace is returned when no free block of the requested size is left.
var ErrNoSpace = errors.New("no free block of the requested size")

// Pool is the space to allocate from and what already occupies it.
type Pool struct {
	// CIDRs are the VPC's IPv4 blocks.
	CIDRs []string
	// Used are blocks that must not be touched: existing subnets and other reservations.
	Used []string
}

// Allocate returns count free blocks of the given prefix length, ascending. The returned
// blocks do not overlap each other or anything in the pool's used list.
func Allocate(pool Pool, prefixLength, count int) ([]string, error) {
	if count <= 0 {
		return nil, nil
	}
	if prefixLength < 0 || prefixLength > 32 {
		return nil, fmt.Errorf("prefix length %d out of range", prefixLength)
	}

	parents, err := parsePrefixes(pool.CIDRs)
	if err != nil {
		return nil, fmt.Errorf("vpc cidr: %w", err)
	}
	used, err := parsePrefixes(pool.Used)
	if err != nil {
		return nil, fmt.Errorf("used cidr: %w", err)
	}
	slices.SortFunc(parents, comparePrefix)

	var out []string
	for _, parent := range parents {
		if parent.Bits() > prefixLength {
			continue // the requested block is bigger than this VPC block
		}
		for candidate := range blocks(parent, prefixLength) {
			if overlapsAny(candidate, used) {
				continue
			}
			out = append(out, candidate.String())
			used = append(used, candidate)
			if len(out) == count {
				return out, nil
			}
		}
	}
	return nil, fmt.Errorf("%w: wanted %d x /%d, found %d", ErrNoSpace, count, prefixLength, len(out))
}

// blocks yields every /prefixLength block inside parent, ascending.
func blocks(parent netip.Prefix, prefixLength int) func(func(netip.Prefix) bool) {
	return func(yield func(netip.Prefix) bool) {
		start := addrToInt(parent.Masked().Addr())
		size := new(big.Int).Lsh(big.NewInt(1), uint(32-prefixLength))
		total := new(big.Int).Lsh(big.NewInt(1), uint(32-parent.Bits()))
		end := new(big.Int).Add(start, total)
		for cur := new(big.Int).Set(start); cur.Cmp(end) < 0; cur.Add(cur, size) {
			if !yield(netip.PrefixFrom(intToAddr(cur), prefixLength)) {
				return
			}
		}
	}
}

func overlapsAny(p netip.Prefix, used []netip.Prefix) bool {
	return slices.ContainsFunc(used, p.Overlaps)
}

func parsePrefixes(in []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(in))
	for _, s := range in {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return nil, err
		}
		if !p.Addr().Is4() {
			continue // IPv6 is not allocated here
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

func comparePrefix(a, b netip.Prefix) int {
	if c := a.Addr().Compare(b.Addr()); c != 0 {
		return c
	}
	return a.Bits() - b.Bits()
}

func addrToInt(a netip.Addr) *big.Int {
	b := a.As4()
	return new(big.Int).SetBytes(b[:])
}

func intToAddr(i *big.Int) netip.Addr {
	var b [4]byte
	i.FillBytes(b[:])
	return netip.AddrFrom4(b)
}
