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

// Package metrics exposes the discovered inventory as Prometheus metrics on the
// controller-runtime metrics endpoint.
package metrics

import (
	"strconv"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
)

const (
	namespace = "hs_aws"

	labelScope   = "scope"
	labelAccount = "account"
	labelRegion  = "region"
	labelVPC     = "vpc_id"
	labelName    = "name"
	labelOwner   = "owner"
	labelEnv     = "env"
	labelKind    = "kind"
)

var (
	subnetLabels = []string{labelScope, labelAccount, labelRegion, labelVPC, "subnet_id", labelName,
		"cidr", "az", labelOwner, labelEnv, "tier", "public"}
	vpcLabels    = []string{labelScope, labelAccount, labelRegion, labelVPC, labelName, labelOwner, labelEnv}
	targetLabels = []string{labelScope, labelAccount, labelRegion}

	subnetAvailableIPs = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "subnet_available_ips",
		Help: "Free IPv4 addresses in the subnet.",
	}, subnetLabels)
	subnetTotalIPs = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "subnet_total_ips",
		Help: "Usable IPv4 addresses in the subnet (CIDR size minus the 5 addresses AWS reserves).",
	}, subnetLabels)
	subnetMissingTags = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "subnet_missing_required_tags",
		Help: "Number of required tags that are absent or empty on the subnet.",
	}, subnetLabels)
	vpcOverlaps = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "vpc_cidr_overlaps",
		Help: "Number of other VPCs in the scope whose CIDRs overlap this VPC.",
	}, vpcLabels)
	targetUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "target_up",
		Help: "1 if the last discovery of the account/region succeeded, 0 otherwise.",
	}, targetLabels)
	targetSyncErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "target_sync_errors_total",
		Help: "Failed discoveries of the account/region.",
	}, targetLabels)
	scopeLastSync = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "scope_last_sync_timestamp_seconds",
		Help: "Unix time of the last finished sync of the scope.",
	}, []string{labelScope})

	unmanagedLabels  = []string{labelScope, labelAccount, labelRegion, labelKind}
	unmanagedCurrent = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "unmanaged_resources",
		Help: "Discovered resources without the managed tag: nobody has taken responsibility for them.",
	}, unmanagedLabels)
	unmanagedSeen = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "unmanaged_resources_total",
		Help: "Unmanaged resources seen for the first time. Alert on an increase: something appeared that nobody owns.",
	}, unmanagedLabels)
	autoImports = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace, Name: "auto_imports_total",
		Help: "Decisions the auto-import policy took, by result: applied, dryrun, skipped or no_owner.",
	}, []string{labelScope, labelAccount, labelRegion, "result"})
)

// seenUnmanaged remembers which resources have already been counted, per scope, so the
// counter rises once per resource instead of once per resync.
var seenUnmanaged = struct {
	mu sync.Mutex
	m  map[string]map[string]bool
}{m: map[string]map[string]bool{}}

func init() {
	ctrlmetrics.Registry.MustRegister(subnetAvailableIPs, subnetTotalIPs, subnetMissingTags,
		vpcOverlaps, targetUp, targetSyncErrors, scopeLastSync,
		unmanagedCurrent, unmanagedSeen, autoImports)
}

// TargetResult is the state of one account/region after a sync.
type TargetResult struct {
	Account, Region string
	// OK is true when the last discovery of the target succeeded.
	OK bool
	// Synced is true when the target was discovered in this sync (not only carried over).
	Synced bool
}

// SetScope replaces every inventory series of the scope with the given objects,
// so renamed, retagged or removed subnets do not leave stale series behind.
func SetScope(scope string, vpcs []awsv1alpha1.VPC, subnets []awsv1alpha1.Subnet, targets []TargetResult, lastSync float64) {
	forgetSeries(scope)
	for _, s := range subnets {
		l := prometheus.Labels{
			labelScope: scope, labelAccount: s.Spec.Account, labelRegion: s.Spec.Region, labelVPC: s.Spec.VPCID,
			"subnet_id": s.Spec.SubnetID, labelName: s.Status.Name, "cidr": s.Status.CIDRBlock,
			"az": s.Status.AvailabilityZone, labelOwner: s.Status.Owner, labelEnv: s.Status.Env,
			"tier": s.Status.Tier, "public": strconv.FormatBool(s.Status.Public),
		}
		subnetAvailableIPs.With(l).Set(float64(s.Status.AvailableIPs))
		subnetTotalIPs.With(l).Set(float64(s.Status.TotalIPs))
		subnetMissingTags.With(l).Set(float64(len(s.Status.MissingTags)))
	}
	for _, v := range vpcs {
		vpcOverlaps.With(prometheus.Labels{
			labelScope: scope, labelAccount: v.Spec.Account, labelRegion: v.Spec.Region, labelVPC: v.Spec.VPCID,
			labelName: v.Status.Name, labelOwner: v.Status.Owner, labelEnv: v.Status.Env,
		}).Set(float64(len(v.Status.OverlapsWith)))
	}
	for _, t := range targets {
		l := prometheus.Labels{labelScope: scope, labelAccount: t.Account, labelRegion: t.Region}
		if t.OK {
			targetUp.With(l).Set(1)
		} else {
			targetUp.With(l).Set(0)
		}
		if t.Synced && !t.OK {
			targetSyncErrors.With(l).Inc()
		}
	}
	scopeLastSync.With(prometheus.Labels{labelScope: scope}).Set(lastSync)
}

// Forget removes every series of the scope and what it remembered, for a scope that is gone.
// Error counters are kept, as counters should be.
func Forget(scope string) {
	forgetSeries(scope)
	ClearUnmanaged(scope)
	seenUnmanaged.mu.Lock()
	delete(seenUnmanaged.m, scope)
	seenUnmanaged.mu.Unlock()
}

// ClearUnmanaged drops the scope's unmanaged gauges so a sync can rebuild them: an account
// whose last untagged subnet was imported should report nothing, not its old number. The set
// of resources already seen is left alone, because the counter beside the gauge must rise once
// per resource rather than once per resync.
func ClearUnmanaged(scope string) {
	unmanagedCurrent.DeletePartialMatch(prometheus.Labels{labelScope: scope})
}

// forgetSeries drops the gauges so a sync can rebuild them. It deliberately keeps the set of
// resources already seen: that set is what stops the "newly unmanaged" counter from counting
// the same resource again on every resync.
func forgetSeries(scope string) {
	l := prometheus.Labels{labelScope: scope}
	for _, g := range []*prometheus.GaugeVec{subnetAvailableIPs, subnetTotalIPs, subnetMissingTags,
		vpcOverlaps, targetUp, scopeLastSync} {
		g.DeletePartialMatch(l)
	}
}

// Unmanaged records what one account/region holds outside the selector. ids are the resource
// identifiers, so a resource that was already there is not counted as newly seen again.
func Unmanaged(scope, account, region, kind string, ids []string) {
	l := prometheus.Labels{labelScope: scope, labelAccount: account, labelRegion: region, labelKind: kind}
	unmanagedCurrent.With(l).Set(float64(len(ids)))

	seenUnmanaged.mu.Lock()
	defer seenUnmanaged.mu.Unlock()
	seen := seenUnmanaged.m[scope]
	if seen == nil {
		seen = map[string]bool{}
		seenUnmanaged.m[scope] = seen
	}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		unmanagedSeen.With(l).Inc()
	}
}

// AutoImport counts one decision of the auto-import policy.
func AutoImport(scope, account, region, result string) {
	autoImports.With(prometheus.Labels{
		labelScope: scope, labelAccount: account, labelRegion: region, "result": result,
	}).Inc()
}
