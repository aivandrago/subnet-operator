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

import "testing"

// With the five addresses AWS and Azure reserve.
func TestUsableIPv4(t *testing.T) {
	cases := map[string]int64{
		"10.0.0.0/24":   251,
		"10.0.0.0/28":   11,
		"10.0.0.0/16":   65531,
		"10.0.0.0/30":   0,
		"2001:db8::/64": 0,
		"garbage":       0,
	}
	for cidr, want := range cases {
		if got := UsableIPv4(cidr, 5); got != want {
			t.Errorf("UsableIPv4(%q, 5) = %d, want %d", cidr, got, want)
		}
	}
	// GCP reserves four.
	if got := UsableIPv4("10.0.0.0/24", 4); got != 252 {
		t.Errorf("UsableIPv4(10.0.0.0/24, 4) = %d, want 252", got)
	}
}

func TestUtilizationPercent(t *testing.T) {
	cases := []struct {
		total, available int64
		want             int32
	}{
		{251, 251, 0},
		{251, 0, 100},
		{100, 19, 81},
		{0, 0, 0},
		{10, 20, 0},
	}
	for _, c := range cases {
		if got := UtilizationPercent(c.total, c.available); got != c.want {
			t.Errorf("UtilizationPercent(%d, %d) = %d, want %d", c.total, c.available, got, c.want)
		}
	}
}

func TestOverlaps(t *testing.T) {
	if !Overlaps([]string{"10.0.0.0/16"}, []string{"10.0.128.0/20"}) {
		t.Error("nested prefixes must overlap")
	}
	if Overlaps([]string{"10.0.0.0/16", "10.1.0.0/16"}, []string{"10.2.0.0/16"}) {
		t.Error("disjoint prefixes must not overlap")
	}
	if Overlaps([]string{"bad"}, []string{"10.0.0.0/8"}) {
		t.Error("unparsable prefixes are ignored")
	}
}
