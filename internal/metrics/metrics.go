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
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	// labelNamespace is the Kubernetes namespace of a claim or an import, not the metric prefix.
	labelNamespace = "namespace"
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

	// Claims and imports are requests someone made and is waiting on. Without these a claim
	// stuck on NoSpace or an import that AWS refused was visible only in the object's status,
	// so nothing could page on it.
	claimReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "subnet_claim_ready",
		Help: "1 when the SubnetClaim is fulfilled, 0 while it is not; reason is the Ready condition's.",
	}, []string{labelNamespace, labelName, "reason"})
	importReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "resource_import_ready",
		Help: "1 when the ResourceImport is settled (applied, or recorded as a dry run), 0 while it is not.",
	}, []string{labelNamespace, labelName, "state", "reason"})
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
		unmanagedCurrent, unmanagedSeen, autoImports, claimReady, importReady)
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
		// IPv4 capacity only exists for a subnet that has an IPv4 CIDR. An IPv6-only subnet
		// reports zero usable and zero free IPv4 addresses, which reads as "full" to anything
		// that compares the two, and SubnetFull fired forever on subnets working exactly as
		// designed. Leaving the series out says what is true: there is no IPv4 capacity here to
		// measure. Tag compliance applies to every subnet, so that gauge stays.
		if s.Status.CIDRBlock != "" {
			subnetAvailableIPs.With(l).Set(float64(s.Status.AvailableIPs))
			subnetTotalIPs.With(l).Set(float64(s.Status.TotalIPs))
		}
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

// SeedUnmanaged tells a process that has not yet counted anything for the scope which
// unmanaged resources were already known before it started — the IDs the previous sync wrote to
// the scope's status. Without it, the first sync after a restart or a change of leader would
// count every known unmanaged resource as newly seen and the alert would fire for all of them.
// A resource created while the operator was down is not in the list, so it still counts.
//
// Once the process has its own record for the scope, that record is newer than the status it
// is about to overwrite, so seeding again does nothing.
func SeedUnmanaged(scope string, known []string) {
	seenUnmanaged.mu.Lock()
	defer seenUnmanaged.mu.Unlock()
	if _, ok := seenUnmanaged.m[scope]; ok {
		return
	}
	seen := make(map[string]bool, len(known))
	for _, id := range known {
		seen[id] = true
	}
	seenUnmanaged.m[scope] = seen
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

// readiness turns an object's conditions into the gauge value and the reason label: the Ready
// condition, or "Unknown" before the controller has written one.
func readiness(conds []metav1.Condition) (float64, string) {
	c := meta.FindStatusCondition(conds, "Ready")
	if c == nil {
		return 0, "Unknown"
	}
	if c.Status == metav1.ConditionTrue {
		return 1, c.Reason
	}
	return 0, c.Reason
}

// ClaimReady records whether a SubnetClaim is fulfilled. The object's previous series is
// dropped first, so a claim whose reason changed does not leave the old one behind.
func ClaimReady(ns, name string, conds []metav1.Condition) {
	claimReady.DeletePartialMatch(prometheus.Labels{labelNamespace: ns, labelName: name})
	v, reason := readiness(conds)
	claimReady.WithLabelValues(ns, name, reason).Set(v)
}

// ForgetClaim drops a SubnetClaim that no longer exists.
func ForgetClaim(ns, name string) {
	claimReady.DeletePartialMatch(prometheus.Labels{labelNamespace: ns, labelName: name})
}

// ImportReady records whether a ResourceImport has settled, with its state (Pending, Applied,
// Skipped, Failed) beside the reason.
func ImportReady(ns, name, state string, conds []metav1.Condition) {
	importReady.DeletePartialMatch(prometheus.Labels{labelNamespace: ns, labelName: name})
	v, reason := readiness(conds)
	importReady.WithLabelValues(ns, name, state, reason).Set(v)
}

// ForgetImport drops a ResourceImport that no longer exists.
func ForgetImport(ns, name string) {
	importReady.DeletePartialMatch(prometheus.Labels{labelNamespace: ns, labelName: name})
}
