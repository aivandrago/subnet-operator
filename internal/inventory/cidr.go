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
	"net/netip"
)

// UsableIPv4 returns the number of usable addresses in an IPv4 subnet CIDR of which the
// provider reserves `reserved` for itself (5 on AWS and Azure, 4 on GCP).
func UsableIPv4(cidr string, reserved int64) int64 {
	p, err := netip.ParsePrefix(cidr)
	if err != nil || !p.Addr().Is4() {
		return 0
	}
	n := int64(1) << (32 - p.Bits())
	if n <= reserved {
		return 0
	}
	return n - reserved
}

// UtilizationPercent returns the share of used addresses, rounded down, 0-100.
func UtilizationPercent(total, available int64) int32 {
	if total <= 0 || available >= total {
		return 0
	}
	if available < 0 {
		available = 0
	}
	// The guards above leave the result between 0 and 100, which int32 holds comfortably;
	// spelling the bounds out keeps both the reader and the scanner from having to prove it.
	switch used := (total - available) * 100 / total; {
	case used >= 100:
		return 100
	case used <= 0:
		return 0
	default:
		return int32(used)
	}
}

// Overlaps reports whether any prefix in a overlaps any prefix in b.
// Unparsable entries are ignored.
func Overlaps(a, b []string) bool {
	for _, x := range a {
		px, err := netip.ParsePrefix(x)
		if err != nil {
			continue
		}
		for _, y := range b {
			py, err := netip.ParsePrefix(y)
			if err != nil {
				continue
			}
			if px.Overlaps(py) {
				return true
			}
		}
	}
	return false
}
