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
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
	"hypersurgery.dev/subnet-operator/internal/audit"
)

// Every metric is named hs_<name> and carries a provider label; it names what it measures in
// words every cloud shares: network_id and zone (ADR 0002 §7). The hs_aws_* names 0.8 exported
// were removed in 0.9 (docs/operations/upgrades.md has the mapping).
const (
	prefix = "hs_"

	labelProvider = "provider"
	labelScope    = "scope"
	labelAccount  = "account"
	labelRegion   = "region"
	labelNetwork  = "network_id"
	labelSubnet   = "subnet_id"
	labelZone     = "zone"
	labelName     = "name"
	labelOwner    = "owner"
	labelEnv      = "env"
	labelKind     = "kind"
	labelReason   = "reason"
	labelResult   = "result"
	// labelNamespace is the Kubernetes namespace of a claim or an import, not the metric prefix.
	labelNamespace = "namespace"
)

// Kinds of unmanaged resource, the values of the kind label.
const (
	KindNetwork = "network"
	KindSubnet  = "subnet"
)

// autoImportResults are the values of hs_auto_imports_total's result label: the audit log's
// results for a policy verdict, so a metric and the audit lines behind it use one word.
var autoImportResults = []string{audit.ResultApplied, audit.ResultDryRun, audit.ResultSkipped, audit.ResultNoOwner}

// family describes one metric: its name without the prefix, its help text and its labels.
type family struct {
	name, help string
	labels     []string
}

// gauge is a gauge vector with the family it was built from.
type gauge struct {
	family
	vec *prometheus.GaugeVec
}

func newGauge(f family) *gauge {
	return &gauge{family: f,
		vec: prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: prefix + f.name, Help: f.help}, f.labels)}
}

func (g *gauge) set(l prometheus.Labels, v float64) { g.vec.With(l).Set(v) }

func (g *gauge) deletePartialMatch(l prometheus.Labels) { g.vec.DeletePartialMatch(l) }

// counter is a counter vector with the family it was built from.
type counter struct {
	family
	vec *prometheus.CounterVec
}

func newCounter(f family) *counter {
	return &counter{family: f,
		vec: prometheus.NewCounterVec(prometheus.CounterOpts{Name: prefix + f.name, Help: f.help}, f.labels)}
}

func (c *counter) inc(l prometheus.Labels) { c.vec.With(l).Inc() }

// touch makes sure the series exists, at 0 if nothing was counted yet. increase() needs a
// sample before the first rise to see it: a series that first appears at 1 has not increased
// as far as Prometheus can tell, and an alert on the increase stays silent.
func (c *counter) touch(l prometheus.Labels) { c.vec.With(l) }

var (
	subnetLabels = []string{labelProvider, labelScope, labelAccount, labelRegion, labelNetwork, labelSubnet,
		labelName, "cidr", labelZone, labelOwner, labelEnv, "tier", "public"}
	networkLabels = []string{labelProvider, labelScope, labelAccount, labelRegion, labelNetwork, labelName,
		labelOwner, labelEnv}
	targetLabels = []string{labelProvider, labelScope, labelAccount, labelRegion}

	subnetAvailableIPs = newGauge(family{name: "subnet_available_ips",
		help: "Free IPv4 addresses in the subnet.", labels: subnetLabels})
	subnetTotalIPs = newGauge(family{name: "subnet_total_ips",
		help:   "Usable IPv4 addresses in the subnet: the CIDR size minus the addresses the provider reserves (5 on AWS).",
		labels: subnetLabels})
	subnetMissingTags = newGauge(family{name: "subnet_missing_required_tags",
		help: "Number of required tags that are absent or empty on the subnet.", labels: subnetLabels})
	// The name follows the kind, Network, rather than 0.8's VPC.
	networkOverlaps = newGauge(family{name: "network_cidr_overlaps",
		help: "Number of other networks in the scope whose CIDRs overlap this network.", labels: networkLabels})
	targetUp = newGauge(family{name: "target_up",
		help:   "1 if the account/region is reachable: its last discovery succeeded or was only throttled. 0 otherwise.",
		labels: targetLabels})
	targetSyncErrors = newCounter(family{name: "target_sync_errors_total",
		help: "Failed discoveries of the account/region, not counting throttled ones.", labels: targetLabels})
	// A throttled target is reachable but stale. It gets its own gauge rather than target_up
	// == 0, because the cause and the fix differ: an unreachable account needs its credentials
	// fixed, a throttled one needs less load, and it recovers on its own.
	targetThrottled = newGauge(family{name: "target_throttled",
		help:   "1 while the account/region is backed off because its last discovery was throttled, 0 otherwise.",
		labels: targetLabels})
	apiThrottled = newCounter(family{name: "api_throttled_total",
		help:   "Cloud API calls throttled for the account/region, counted per attempt, including attempts that were retried successfully.",
		labels: []string{labelProvider, labelScope, labelAccount, labelRegion, "operation"}})
	scopeLastSync = newGauge(family{name: "scope_last_sync_timestamp_seconds",
		help: "Unix time of the last finished sync of the scope.", labels: []string{labelProvider, labelScope}})

	unmanagedLabels  = []string{labelProvider, labelScope, labelAccount, labelRegion, labelKind}
	unmanagedCurrent = newGauge(family{name: "unmanaged_resources",
		help:   "Discovered resources without the managed tag: nobody has taken responsibility for them.",
		labels: unmanagedLabels})
	unmanagedSeen = newCounter(family{name: "unmanaged_resources_total",
		help:   "Unmanaged resources seen for the first time. Alert on an increase: something appeared that nobody owns.",
		labels: unmanagedLabels})
	autoImports = newCounter(family{name: "auto_imports_total",
		help:   "Decisions the auto-import policy took, by result: applied, dryrun, skipped or no_owner.",
		labels: []string{labelProvider, labelScope, labelAccount, labelRegion, labelResult}})

	// Claims and imports are requests someone made and is waiting on. Without these a claim
	// stuck on NoSpace or an import the cloud refused was visible only in the object's status,
	// so nothing could page on it. provider is their scope's, empty while the scope is missing.
	claimReady = newGauge(family{name: "subnet_claim_ready",
		help:   "1 when the SubnetClaim is fulfilled, 0 while it is not; reason is the Ready condition's.",
		labels: []string{labelProvider, labelNamespace, labelName, labelReason}})
	importReady = newGauge(family{name: "resource_import_ready",
		help:   "1 when the ResourceImport is settled (applied, or recorded as a dry run), 0 while it is not.",
		labels: []string{labelProvider, labelNamespace, labelName, "state", labelReason}})

	gauges = []*gauge{subnetAvailableIPs, subnetTotalIPs, subnetMissingTags, networkOverlaps, targetUp,
		targetThrottled, scopeLastSync, unmanagedCurrent, claimReady, importReady}
	counters = []*counter{targetSyncErrors, apiThrottled, unmanagedSeen, autoImports}
)

// seenUnmanaged remembers which resources have already been counted, per scope, so the
// counter rises once per resource instead of once per resync.
var seenUnmanaged = struct {
	mu sync.Mutex
	m  map[string]map[string]bool
}{m: map[string]map[string]bool{}}

func init() {
	for _, g := range gauges {
		ctrlmetrics.Registry.MustRegister(g.vec)
	}
	for _, c := range counters {
		ctrlmetrics.Registry.MustRegister(c.vec)
	}
}

// ProviderLabel is the value of the provider label for a provider: lowercase, like other label
// values ("aws", later "gcp" and "azure").
func ProviderLabel(p networkv1beta1.Provider) string {
	return strings.ToLower(string(p))
}

// TargetResult is the state of one account/region after a sync.
type TargetResult struct {
	Account, Region string
	// OK is true when the last discovery of the target succeeded.
	OK bool
	// Synced is true when the target was discovered in this sync (not only carried over).
	Synced bool
	// Throttled is true while the target is backed off after throttling. Such a target is
	// reachable, so OK should be true as well.
	Throttled bool
}

// SetScope replaces every inventory series of the scope with the given objects,
// so renamed, retagged or removed subnets do not leave stale series behind.
func SetScope(scope string, provider networkv1beta1.Provider, networks []networkv1beta1.Network,
	subnets []networkv1beta1.Subnet, targets []TargetResult, lastSync float64) {
	forgetSeries(scope)
	p := ProviderLabel(provider)
	for _, s := range subnets {
		l := prometheus.Labels{
			labelProvider: p, labelScope: scope, labelAccount: s.Spec.Account, labelRegion: s.Spec.Region,
			labelNetwork: s.Spec.NetworkID, labelSubnet: s.Spec.ID, labelName: s.Status.Name,
			"cidr": s.Status.CIDRBlock, labelZone: s.Status.Zone, labelOwner: s.Status.Owner,
			labelEnv: s.Status.Env, "tier": s.Status.Tier, "public": strconv.FormatBool(s.Status.AWSStatus().Public),
		}
		// IPv4 capacity only exists for a subnet that has an IPv4 CIDR. An IPv6-only subnet
		// reports zero usable and zero free IPv4 addresses, which reads as "full" to anything
		// that compares the two, and SubnetFull fired forever on subnets working exactly as
		// designed. Leaving the series out says what is true: there is no IPv4 capacity here to
		// measure. Tag compliance applies to every subnet, so that gauge stays.
		// The same goes for a provider that could not say how many addresses are free: no
		// series is the truth, zero would be a full subnet.
		if s.Status.CIDRBlock != "" && s.Status.AvailableIPs != nil && s.Status.TotalIPs != nil {
			subnetAvailableIPs.set(l, float64(*s.Status.AvailableIPs))
			subnetTotalIPs.set(l, float64(*s.Status.TotalIPs))
		}
		subnetMissingTags.set(l, float64(len(s.Status.MissingTags)))
	}
	for _, n := range networks {
		networkOverlaps.set(prometheus.Labels{
			labelProvider: p, labelScope: scope, labelAccount: n.Spec.Account, labelRegion: n.Spec.Region,
			labelNetwork: n.Spec.ID, labelName: n.Status.Name, labelOwner: n.Status.Owner, labelEnv: n.Status.Env,
		}, float64(len(n.Status.OverlapsWith)))
	}
	for _, t := range targets {
		l := prometheus.Labels{labelProvider: p, labelScope: scope, labelAccount: t.Account, labelRegion: t.Region}
		up, throttled := 0.0, 0.0
		if t.OK {
			up = 1
		}
		if t.Throttled {
			throttled = 1
		}
		targetUp.set(l, up)
		targetThrottled.set(l, throttled)
		targetSyncErrors.touch(l)
		if t.Synced && !t.OK {
			targetSyncErrors.inc(l)
		}
	}
	scopeLastSync.set(prometheus.Labels{labelProvider: p, labelScope: scope}, lastSync)
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
	unmanagedCurrent.deletePartialMatch(prometheus.Labels{labelScope: scope})
}

// forgetSeries drops the gauges so a sync can rebuild them. It deliberately keeps the set of
// resources already seen: that set is what stops the "newly unmanaged" counter from counting
// the same resource again on every resync.
func forgetSeries(scope string) {
	l := prometheus.Labels{labelScope: scope}
	for _, g := range []*gauge{subnetAvailableIPs, subnetTotalIPs, subnetMissingTags,
		networkOverlaps, targetUp, targetThrottled, scopeLastSync} {
		g.deletePartialMatch(l)
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

// Unmanaged records what one account/region holds outside the selector. kind is KindNetwork or
// KindSubnet; ids are the resource identifiers, so a resource that was already there is not
// counted as newly seen again.
func Unmanaged(scope string, provider networkv1beta1.Provider, account, region, kind string, ids []string) {
	l := prometheus.Labels{labelProvider: ProviderLabel(provider), labelScope: scope, labelAccount: account,
		labelRegion: region, labelKind: kind}
	unmanagedCurrent.set(l, float64(len(ids)))
	unmanagedSeen.touch(l)

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
		unmanagedSeen.inc(l)
	}
}

// APIThrottled counts one throttled cloud API attempt for the account/region.
func APIThrottled(scope string, provider networkv1beta1.Provider, account, region, operation string) {
	apiThrottled.inc(prometheus.Labels{
		labelProvider: ProviderLabel(provider), labelScope: scope, labelAccount: account, labelRegion: region,
		"operation": operation,
	})
}

// AutoImportTarget prepares the auto-import counter of an account/region whose scope runs the
// policy: every result starts at 0, so the first decision after a start is a rise that
// increase() can see. Without it the first applied import after a restart appeared at 1 and
// the AutoImportedResources digest left it out.
func AutoImportTarget(scope string, provider networkv1beta1.Provider, account, region string) {
	for _, result := range autoImportResults {
		autoImports.touch(prometheus.Labels{
			labelProvider: ProviderLabel(provider), labelScope: scope, labelAccount: account, labelRegion: region,
			labelResult: result,
		})
	}
}

// AutoImport counts one decision of the auto-import policy.
func AutoImport(scope string, provider networkv1beta1.Provider, account, region, result string) {
	autoImports.inc(prometheus.Labels{
		labelProvider: ProviderLabel(provider), labelScope: scope, labelAccount: account, labelRegion: region,
		labelResult: result,
	})
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

// ClaimReady records whether a SubnetClaim is fulfilled. provider is its scope's, empty when
// the scope does not exist. The object's previous series is dropped first, so a claim whose
// reason or provider changed does not leave the old one behind.
func ClaimReady(ns, name string, provider networkv1beta1.Provider, conds []metav1.Condition) {
	ForgetClaim(ns, name)
	v, reason := readiness(conds)
	claimReady.set(prometheus.Labels{labelProvider: ProviderLabel(provider), labelNamespace: ns,
		labelName: name, labelReason: reason}, v)
}

// ForgetClaim drops a SubnetClaim that no longer exists.
func ForgetClaim(ns, name string) {
	claimReady.deletePartialMatch(prometheus.Labels{labelNamespace: ns, labelName: name})
}

// ImportReady records whether a ResourceImport has settled, with its state (Pending, Applied,
// Skipped, Failed) beside the reason. provider is its scope's, empty when the scope does not
// exist.
func ImportReady(ns, name string, provider networkv1beta1.Provider, state string, conds []metav1.Condition) {
	ForgetImport(ns, name)
	v, reason := readiness(conds)
	importReady.set(prometheus.Labels{labelProvider: ProviderLabel(provider), labelNamespace: ns,
		labelName: name, "state": state, labelReason: reason}, v)
}

// ForgetImport drops a ResourceImport that no longer exists.
func ForgetImport(ns, name string) {
	importReady.deletePartialMatch(prometheus.Labels{labelNamespace: ns, labelName: name})
}
