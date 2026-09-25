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

	Unmanaged(scope, aws, account, region, KindSubnet, []string{"subnet-a", "subnet-b"})
	if got := testutil.ToFloat64(unmanagedCurrent.vec.WithLabelValues("aws", scope, account, region, "subnet")); got != 2 {
		t.Errorf("current = %v, want 2", got)
	}
	if got := testutil.ToFloat64(unmanagedSeen.vec.WithLabelValues("aws", scope, account, region, "subnet")); got != 2 {
		t.Errorf("seen = %v, want 2", got)
	}

	// The same two on the next resync: the gauge stays, the counter must not move, otherwise
	// the alert would fire every sync for resources nobody touched.
	Unmanaged(scope, aws, account, region, KindSubnet, []string{"subnet-a", "subnet-b"})
	if got := testutil.ToFloat64(unmanagedSeen.vec.WithLabelValues("aws", scope, account, region, "subnet")); got != 2 {
		t.Errorf("seen = %v, want it unchanged at 2", got)
	}

	// One goes away, one appears: the gauge follows, the counter counts only the new one.
	Unmanaged(scope, aws, account, region, KindSubnet, []string{"subnet-b", "subnet-c"})
	if got := testutil.ToFloat64(unmanagedCurrent.vec.WithLabelValues("aws", scope, account, region, "subnet")); got != 2 {
		t.Errorf("current = %v, want 2", got)
	}
	if got := testutil.ToFloat64(unmanagedSeen.vec.WithLabelValues("aws", scope, account, region, "subnet")); got != 3 {
		t.Errorf("seen = %v, want 3", got)
	}

	// Nothing unmanaged left.
	Unmanaged(scope, aws, account, region, KindSubnet, nil)
	if got := testutil.ToFloat64(unmanagedCurrent.vec.WithLabelValues("aws", scope, account, region, "subnet")); got != 0 {
		t.Errorf("current = %v, want 0", got)
	}
}

func TestForgetDropsUnmanagedSeries(t *testing.T) {
	const scope = "unmanaged-forget"
	Unmanaged(scope, aws, "111111111111", "eu-west-1", KindNetwork, []string{"vpc-a"})
	before := testutil.CollectAndCount(unmanagedCurrent.vec)

	Forget(scope)
	if got := testutil.CollectAndCount(unmanagedCurrent.vec); got != before-1 {
		t.Errorf("series after Forget = %d, want %d", got, before-1)
	}

	// The seen set went with it, so a scope that comes back counts its resources afresh.
	Unmanaged(scope, aws, "111111111111", "eu-west-1", KindNetwork, []string{"vpc-a"})
	if got := testutil.ToFloat64(unmanagedSeen.vec.WithLabelValues("aws", scope, "111111111111", "eu-west-1", "network")); got != 2 {
		t.Errorf("seen = %v, want 2 (once before Forget, once after)", got)
	}
	Forget(scope)
}

func TestAutoImportCountsByResult(t *testing.T) {
	const scope, account, region = "autoimport-test", "222222222222", "eu-west-1"
	AutoImport(scope, aws, account, region, "applied")
	AutoImport(scope, aws, account, region, "applied")
	AutoImport(scope, aws, account, region, "no_owner")

	if got := testutil.ToFloat64(autoImports.vec.WithLabelValues("aws", scope, account, region, "applied")); got != 2 {
		t.Errorf("applied = %v, want 2", got)
	}
	if got := testutil.ToFloat64(autoImports.vec.WithLabelValues("aws", scope, account, region, "no_owner")); got != 1 {
		t.Errorf("no_owner = %v, want 1", got)
	}
}

// A resync rebuilds the scope's gauges. It must not take the unmanaged gauge with it, and it
// must not make the operator forget which resources it had already seen — otherwise the
// "something new appeared" counter grows on every sync and the alert fires forever.
func TestUnmanagedSurvivesAResync(t *testing.T) {
	const scope = "resync"
	t.Cleanup(func() { Forget(scope) })

	Unmanaged(scope, aws, "111111111111", "eu-central-1", KindNetwork, []string{"vpc-1", "vpc-2"})
	SetScope(scope, aws, nil, nil, []TargetResult{{Account: "111111111111", Region: "eu-central-1", OK: true, Synced: true}}, 1)

	gauge := testutil.ToFloat64(unmanagedCurrent.vec.WithLabelValues("aws", scope, "111111111111", "eu-central-1", "network"))
	if gauge != 2 {
		t.Fatalf("unmanaged gauge = %v after a resync, want 2: rebuilding the scope wiped it", gauge)
	}

	// Same two resources reported again: nothing is new.
	Unmanaged(scope, aws, "111111111111", "eu-central-1", KindNetwork, []string{"vpc-1", "vpc-2"})
	SetScope(scope, aws, nil, nil, []TargetResult{{Account: "111111111111", Region: "eu-central-1", OK: true, Synced: true}}, 2)
	Unmanaged(scope, aws, "111111111111", "eu-central-1", KindNetwork, []string{"vpc-1", "vpc-2", "vpc-3"})

	seen := testutil.ToFloat64(unmanagedSeen.vec.WithLabelValues("aws", scope, "111111111111", "eu-central-1", "network"))
	if seen != 3 {
		t.Errorf("newly seen counter = %v, want 3 (two at first, one added later)", seen)
	}
	if g := testutil.ToFloat64(unmanagedCurrent.vec.WithLabelValues("aws", scope, "111111111111", "eu-central-1", "network")); g != 3 {
		t.Errorf("unmanaged gauge = %v, want 3", g)
	}

	// A scope that goes away takes both with it.
	Forget(scope)
	if c := testutil.CollectAndCount(unmanagedCurrent.vec); c != 0 {
		t.Errorf("gauge series left after Forget: %d", c)
	}
}

// A process that starts with the resources the previous one already knew about counts only
// what appeared since. Seeding is for a fresh start: once the process has its own record, that
// record is newer than the status it came from, and seeding again must not overwrite it.
func TestSeedingStartsFromWhatWasAlreadyKnown(t *testing.T) {
	const scope, account, region = "seed-test", "333333333333", "eu-central-1"
	t.Cleanup(func() { Forget(scope) })
	seen := func() float64 {
		return testutil.ToFloat64(unmanagedSeen.vec.WithLabelValues("aws", scope, account, region, "subnet"))
	}

	SeedUnmanaged(scope, []string{"subnet-a", "subnet-b"})
	Unmanaged(scope, aws, account, region, KindSubnet, []string{"subnet-a", "subnet-b"})
	if got := seen(); got != 0 {
		t.Fatalf("seen = %v after a seeded start with nothing new, want 0", got)
	}

	Unmanaged(scope, aws, account, region, KindSubnet, []string{"subnet-a", "subnet-b", "subnet-c"})
	if got := seen(); got != 1 {
		t.Fatalf("seen = %v, want 1 for the one new resource", got)
	}

	// A late seed without subnet-c must not make the process forget it has already counted it.
	SeedUnmanaged(scope, []string{"subnet-a", "subnet-b"})
	Unmanaged(scope, aws, account, region, KindSubnet, []string{"subnet-a", "subnet-b", "subnet-c"})
	if got := seen(); got != 1 {
		t.Fatalf("seen = %v after a second seed, want it unchanged at 1", got)
	}
}

// The counter the UnmanagedNetworkResource alert reads exists from the first report of a target,
// at zero, so that the first resource that turns up afterwards is a rise Prometheus can see. A
// series that first appears at 1 has not increased as far as increase() is concerned.
func TestUnmanagedCounterStartsAtZero(t *testing.T) {
	const scope, account, region = "zero-start", "444444444444", "eu-central-1"
	t.Cleanup(func() { Forget(scope) })
	SeedUnmanaged(scope, []string{"subnet-known"})
	Unmanaged(scope, aws, account, region, KindSubnet, []string{"subnet-known"})

	if got := seriesFor(unmanagedSeen.vec, "account", account); got != 1 {
		t.Fatalf("%d counter series for a target with nothing new, want 1 at zero", got)
	}
	if got := testutil.ToFloat64(unmanagedSeen.vec.WithLabelValues("aws", scope, account, region, KindSubnet)); got != 0 {
		t.Errorf("seen = %v, want 0", got)
	}
}

// The same for the counter the AutoImportedResources digest reads: once a target's scope runs
// the policy, every result exists at zero, so the first import after a restart is a rise.
func TestAutoImportCounterStartsAtZero(t *testing.T) {
	const scope, account, region = "autoimport-zero", "555555555555", "eu-central-1"
	AutoImportTarget(scope, aws, account, region)

	if got := seriesFor(autoImports.vec, "account", account); got != len(autoImportResults) {
		t.Fatalf("%d counter series before any decision, want %d, one per result", got, len(autoImportResults))
	}
	for _, result := range []string{"applied", "dryrun", "skipped", "no_owner"} {
		if got := testutil.ToFloat64(autoImports.vec.WithLabelValues("aws", scope, account, region, result)); got != 0 {
			t.Errorf("%s = %v, want 0", result, got)
		}
	}

	// Preparing the target again, as every sync does, leaves what was counted alone.
	AutoImport(scope, aws, account, region, "applied")
	AutoImportTarget(scope, aws, account, region)
	if got := testutil.ToFloat64(autoImports.vec.WithLabelValues("aws", scope, account, region, "applied")); got != 1 {
		t.Errorf("applied = %v after a second sync, want 1", got)
	}
}
