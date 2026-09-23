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

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestUnmanagedCountsCurrentAndNewlySeen(t *testing.T) {
	const scope, account, region = "unmanaged-test", "333333333333", "eu-central-1"
	t.Cleanup(func() { Forget(scope) })

	Unmanaged(scope, account, region, "subnet", []string{"subnet-a", "subnet-b"})
	if got := testutil.ToFloat64(unmanagedCurrent.WithLabelValues(scope, account, region, "subnet")); got != 2 {
		t.Errorf("current = %v, want 2", got)
	}
	if got := testutil.ToFloat64(unmanagedSeen.WithLabelValues(scope, account, region, "subnet")); got != 2 {
		t.Errorf("seen = %v, want 2", got)
	}

	// The same two on the next resync: the gauge stays, the counter must not move, otherwise
	// the alert would fire every sync for resources nobody touched.
	Unmanaged(scope, account, region, "subnet", []string{"subnet-a", "subnet-b"})
	if got := testutil.ToFloat64(unmanagedSeen.WithLabelValues(scope, account, region, "subnet")); got != 2 {
		t.Errorf("seen = %v, want it unchanged at 2", got)
	}

	// One goes away, one appears: the gauge follows, the counter counts only the new one.
	Unmanaged(scope, account, region, "subnet", []string{"subnet-b", "subnet-c"})
	if got := testutil.ToFloat64(unmanagedCurrent.WithLabelValues(scope, account, region, "subnet")); got != 2 {
		t.Errorf("current = %v, want 2", got)
	}
	if got := testutil.ToFloat64(unmanagedSeen.WithLabelValues(scope, account, region, "subnet")); got != 3 {
		t.Errorf("seen = %v, want 3", got)
	}

	// Nothing unmanaged left.
	Unmanaged(scope, account, region, "subnet", nil)
	if got := testutil.ToFloat64(unmanagedCurrent.WithLabelValues(scope, account, region, "subnet")); got != 0 {
		t.Errorf("current = %v, want 0", got)
	}
}

func TestForgetDropsUnmanagedSeries(t *testing.T) {
	const scope = "unmanaged-forget"
	Unmanaged(scope, "111111111111", "eu-west-1", "vpc", []string{"vpc-a"})
	before := testutil.CollectAndCount(unmanagedCurrent)

	Forget(scope)
	if got := testutil.CollectAndCount(unmanagedCurrent); got != before-1 {
		t.Errorf("series after Forget = %d, want %d", got, before-1)
	}

	// The seen set went with it, so a scope that comes back counts its resources afresh.
	Unmanaged(scope, "111111111111", "eu-west-1", "vpc", []string{"vpc-a"})
	if got := testutil.ToFloat64(unmanagedSeen.WithLabelValues(scope, "111111111111", "eu-west-1", "vpc")); got != 2 {
		t.Errorf("seen = %v, want 2 (once before Forget, once after)", got)
	}
	Forget(scope)
}

func TestAutoImportCountsByResult(t *testing.T) {
	const scope, account, region = "autoimport-test", "222222222222", "eu-west-1"
	AutoImport(scope, account, region, "applied")
	AutoImport(scope, account, region, "applied")
	AutoImport(scope, account, region, "no_owner")

	if got := testutil.ToFloat64(autoImports.WithLabelValues(scope, account, region, "applied")); got != 2 {
		t.Errorf("applied = %v, want 2", got)
	}
	if got := testutil.ToFloat64(autoImports.WithLabelValues(scope, account, region, "no_owner")); got != 1 {
		t.Errorf("no_owner = %v, want 1", got)
	}
}

// A resync rebuilds the scope's gauges. It must not take the unmanaged gauge with it, and it
// must not make the operator forget which resources it had already seen — otherwise the
// "something new appeared" counter grows on every sync and the alert fires forever.
func TestUnmanagedSurvivesAResync(t *testing.T) {
	const scope = "resync"
	t.Cleanup(func() { Forget(scope) })

	Unmanaged(scope, "111111111111", "eu-central-1", "vpc", []string{"vpc-1", "vpc-2"})
	SetScope(scope, nil, nil, []TargetResult{{Account: "111111111111", Region: "eu-central-1", OK: true, Synced: true}}, 1)

	gauge := testutil.ToFloat64(unmanagedCurrent.WithLabelValues(scope, "111111111111", "eu-central-1", "vpc"))
	if gauge != 2 {
		t.Fatalf("unmanaged gauge = %v after a resync, want 2: rebuilding the scope wiped it", gauge)
	}

	// Same two resources reported again: nothing is new.
	Unmanaged(scope, "111111111111", "eu-central-1", "vpc", []string{"vpc-1", "vpc-2"})
	SetScope(scope, nil, nil, []TargetResult{{Account: "111111111111", Region: "eu-central-1", OK: true, Synced: true}}, 2)
	Unmanaged(scope, "111111111111", "eu-central-1", "vpc", []string{"vpc-1", "vpc-2", "vpc-3"})

	seen := testutil.ToFloat64(unmanagedSeen.WithLabelValues(scope, "111111111111", "eu-central-1", "vpc"))
	if seen != 3 {
		t.Errorf("newly seen counter = %v, want 3 (two at first, one added later)", seen)
	}
	if g := testutil.ToFloat64(unmanagedCurrent.WithLabelValues(scope, "111111111111", "eu-central-1", "vpc")); g != 3 {
		t.Errorf("unmanaged gauge = %v, want 3", g)
	}

	// A scope that goes away takes both with it.
	Forget(scope)
	if c := testutil.CollectAndCount(unmanagedCurrent); c != 0 {
		t.Errorf("gauge series left after Forget: %d", c)
	}
}
