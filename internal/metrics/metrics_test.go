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

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
)

func subnet(id, owner string, available int64) awsv1alpha1.Subnet {
	return awsv1alpha1.Subnet{
		Spec: awsv1alpha1.SubnetSpec{SubnetID: id, VPCID: "vpc-1", Account: "111111111111", Region: "eu-central-1"},
		Status: awsv1alpha1.SubnetStatus{Owner: owner, CIDRBlock: "10.0.0.0/24", TotalIPs: 251,
			AvailableIPs: available, MissingTags: []string{"hs/env"}},
	}
}

func TestSetScopeReplacesSeries(t *testing.T) {
	targets := []TargetResult{{Account: "111111111111", Region: "eu-central-1", OK: true}}
	SetScope("s1", nil, []awsv1alpha1.Subnet{subnet("subnet-1", "team-a", 10), subnet("subnet-2", "", 200)}, targets, 1)
	SetScope("s2", nil, []awsv1alpha1.Subnet{subnet("subnet-9", "", 5)}, targets, 1)
	if got := testutil.CollectAndCount(subnetAvailableIPs); got != 3 {
		t.Fatalf("want 3 series, got %d", got)
	}

	// subnet-2 disappeared and subnet-1 changed owner: no stale series may remain.
	SetScope("s1", nil, []awsv1alpha1.Subnet{subnet("subnet-1", "team-b", 10)},
		[]TargetResult{{Account: "111111111111", Region: "eu-central-1", OK: false, Synced: true}}, 2)
	if got := testutil.CollectAndCount(subnetAvailableIPs); got != 2 {
		t.Fatalf("want 2 series after resync, got %d", got)
	}
	if got := testutil.ToFloat64(subnetMissingTags.WithLabelValues("s1", "111111111111", "eu-central-1", "vpc-1",
		"subnet-1", "", "10.0.0.0/24", "", "team-b", "", "", "false")); got != 1 {
		t.Errorf("missing tags = %v, want 1", got)
	}
	if got := testutil.ToFloat64(targetUp.WithLabelValues("s1", "111111111111", "eu-central-1")); got != 0 {
		t.Errorf("target_up = %v, want 0", got)
	}
	if got := testutil.ToFloat64(targetSyncErrors.WithLabelValues("s1", "111111111111", "eu-central-1")); got != 1 {
		t.Errorf("sync errors = %v, want 1", got)
	}

	Forget("s1")
	if got := testutil.CollectAndCount(subnetAvailableIPs); got != 1 {
		t.Fatalf("want only s2 left, got %d", got)
	}
}

// An IPv6-only subnet has no IPv4 capacity to report. Exporting zero usable and zero free IPv4
// addresses for it made SubnetFull fire forever on a subnet working exactly as designed.
func TestIPv6OnlySubnetReportsNoIPv4Capacity(t *testing.T) {
	const scope = "ipv6-test"
	t.Cleanup(func() { Forget(scope) })

	v6only := awsv1alpha1.Subnet{
		Spec: awsv1alpha1.SubnetSpec{SubnetID: "subnet-v6", VPCID: "vpc-1", Account: "111111111111", Region: "eu-central-1"},
		Status: awsv1alpha1.SubnetStatus{IPv6CIDRBlocks: []string{"2600:1f18:abcd:1200::/64"},
			TotalIPs: 0, AvailableIPs: 0, MissingTags: []string{"hs/owner"}},
	}
	dual := subnet("subnet-dual", "team-a", 40)
	dual.Status.IPv6CIDRBlocks = []string{"2600:1f18:abcd:1201::/64"}
	SetScope(scope, nil, []awsv1alpha1.Subnet{v6only, dual}, nil, 1)

	// Only the dual-stack subnet has IPv4 capacity, so it alone has the two capacity series.
	for name, g := range map[string]interface {
		Collect(chan<- prometheus.Metric)
	}{
		"available": subnetAvailableIPs, "total": subnetTotalIPs,
	} {
		if got := seriesFor(g, "subnet_id", "subnet-v6"); got != 0 {
			t.Errorf("%s: IPv6-only subnet has %d series, want none", name, got)
		}
		if got := seriesFor(g, "subnet_id", "subnet-dual"); got != 1 {
			t.Errorf("%s: dual-stack subnet has %d series, want 1", name, got)
		}
	}
	// Tag compliance is about the subnet, not its address family, so it is reported for both.
	if got := seriesFor(subnetMissingTags, "subnet_id", "subnet-v6"); got != 1 {
		t.Errorf("missing tags: IPv6-only subnet has %d series, want 1", got)
	}
}

// seriesFor counts the series a collector exposes with the given label value.
func seriesFor(c interface {
	Collect(chan<- prometheus.Metric)
}, label, value string) int {
	ch := make(chan prometheus.Metric, 64)
	go func() { c.Collect(ch); close(ch) }()
	n := 0
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			continue
		}
		for _, lp := range pb.GetLabel() {
			if lp.GetName() == label && lp.GetValue() == value {
				n++
			}
		}
	}
	return n
}

// A throttled target is reachable and must not read as down, nor count as a failed discovery:
// the alerts for the two are different, and so is what an operator does about them.
func TestThrottledTargetIsUpAndThrottled(t *testing.T) {
	const scope = "throttle-test"
	t.Cleanup(func() { Forget(scope) })
	busy := []TargetResult{{Account: "111111111111", Region: "eu-central-1", OK: true, Synced: true, Throttled: true}}

	SetScope(scope, nil, nil, busy, 1)
	up := targetUp.WithLabelValues(scope, "111111111111", "eu-central-1")
	throttled := targetThrottled.WithLabelValues(scope, "111111111111", "eu-central-1")
	if testutil.ToFloat64(up) != 1 || testutil.ToFloat64(throttled) != 1 {
		t.Fatalf("target_up = %v, target_throttled = %v, want 1 and 1", testutil.ToFloat64(up), testutil.ToFloat64(throttled))
	}
	if got := testutil.ToFloat64(targetSyncErrors.WithLabelValues(scope, "111111111111", "eu-central-1")); got != 0 {
		t.Errorf("sync errors = %v, want 0 for a throttled target", got)
	}

	SetScope(scope, nil, nil, []TargetResult{{Account: "111111111111", Region: "eu-central-1", OK: true, Synced: true}}, 2)
	if got := testutil.ToFloat64(targetThrottled.WithLabelValues(scope, "111111111111", "eu-central-1")); got != 0 {
		t.Errorf("target_throttled = %v after recovery, want 0", got)
	}

	APIThrottled(scope, "111111111111", "eu-central-1", "DescribeSubnets")
	APIThrottled(scope, "111111111111", "eu-central-1", "DescribeSubnets")
	if got := testutil.ToFloat64(apiThrottled.WithLabelValues(scope, "111111111111", "eu-central-1", "DescribeSubnets")); got != 2 {
		t.Errorf("api_throttled_total = %v, want 2", got)
	}
}
