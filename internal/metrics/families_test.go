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
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
)

// The metrics, as ADR 0002 §7 decides them, with the hs_aws_* name each had up to 0.8. The
// old names were removed in 0.9; they are listed so the docs can be checked against them.
var renames = []struct {
	name, labels, removed string
}{
	{"hs_subnet_available_ips",
		"provider scope account region network_id subnet_id name cidr zone owner env tier public",
		"hs_aws_subnet_available_ips"},
	{"hs_subnet_total_ips",
		"provider scope account region network_id subnet_id name cidr zone owner env tier public",
		"hs_aws_subnet_total_ips"},
	{"hs_subnet_missing_required_tags",
		"provider scope account region network_id subnet_id name cidr zone owner env tier public",
		"hs_aws_subnet_missing_required_tags"},
	{"hs_network_cidr_overlaps", "provider scope account region network_id name owner env", "hs_aws_vpc_cidr_overlaps"},
	{"hs_target_up", "provider scope account region", "hs_aws_target_up"},
	{"hs_target_sync_errors_total", "provider scope account region", "hs_aws_target_sync_errors_total"},
	{"hs_target_throttled", "provider scope account region", "hs_aws_target_throttled"},
	{"hs_api_throttled_total", "provider scope account region operation", "hs_aws_api_throttled_total"},
	{"hs_scope_last_sync_timestamp_seconds", "provider scope", "hs_aws_scope_last_sync_timestamp_seconds"},
	{"hs_unmanaged_resources", "provider scope account region kind", "hs_aws_unmanaged_resources"},
	{"hs_unmanaged_resources_total", "provider scope account region kind", "hs_aws_unmanaged_resources_total"},
	{"hs_auto_imports_total", "provider scope account region result", "hs_aws_auto_imports_total"},
	{"hs_subnet_claim_ready", "provider namespace name reason", "hs_aws_subnet_claim_ready"},
	{"hs_resource_import_ready", "provider namespace name state reason", "hs_aws_resource_import_ready"},
}

// exported is one metric of this package: its family and its collector.
type exported struct {
	f family
	c prometheus.Collector
}

// families is every metric of this package, by name.
func families() map[string]exported {
	out := map[string]exported{}
	for _, g := range gauges {
		out[prefix+g.name] = exported{g.family, g.vec}
	}
	for _, c := range counters {
		out[prefix+c.name] = exported{c.family, c.vec}
	}
	return out
}

// describe returns a collector's metric name and variable labels, from its descriptor.
func describe(t *testing.T, c prometheus.Collector) (string, string) {
	t.Helper()
	ch := make(chan *prometheus.Desc, 1)
	c.Describe(ch)
	d := (<-ch).String()
	// Desc{fqName: "x", help: "…", constLabels: {}, variableLabels: {a,b}}
	name := d[strings.Index(d, `fqName: "`)+len(`fqName: "`):]
	name = name[:strings.Index(name, `"`)]
	vars := d[strings.Index(d, "variableLabels: {")+len("variableLabels: {"):]
	vars = vars[:strings.Index(vars, "}")]
	return name, strings.ReplaceAll(vars, ",", " ")
}

func TestMetricsMatchTheADR(t *testing.T) {
	fams := families()
	if len(fams) != len(renames) {
		t.Errorf("%d metrics exported, %d in the table: a metric was added without deciding its name", len(fams), len(renames))
	}
	for _, r := range renames {
		fam, ok := fams[r.name]
		if !ok {
			t.Errorf("%s is not exported", r.name)
			continue
		}
		if name, labels := describe(t, fam.c); name != r.name || labels != r.labels {
			t.Errorf("metric is %s{%s}, want %s{%s}", name, labels, r.name, r.labels)
		}
	}
}

// 0.9 removed the hs_aws_* family outright: nothing registers a metric under the old prefix,
// and nothing still calls itself deprecated.
func TestTheRemovedNamesAreNotExported(t *testing.T) {
	const scope = "removed-names"
	t.Cleanup(func() { Forget(scope) })
	exerciseEverything(t, scope)
	fams, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fams {
		if strings.HasPrefix(f.GetName(), "hs_aws_") {
			t.Errorf("%s is still exported; the hs_aws_* family was removed in 0.9", f.GetName())
		}
		if strings.HasPrefix(f.GetName(), prefix) && strings.Contains(f.GetHelp(), "Deprecated") {
			t.Errorf("help of %s says it is deprecated: %q", f.GetName(), f.GetHelp())
		}
	}
}

// exerciseEverything calls every recording function once for the scope.
func exerciseEverything(t *testing.T, scope string) {
	t.Helper()
	t.Cleanup(func() {
		ForgetClaim("team", "claim")
		ForgetImport("team", "import")
	})
	sub := subnet("subnet-1", "team-a", 10)
	sub.Status.Zone = "eu-central-1a"
	net := networkv1.Network{
		Spec:   networkv1.NetworkSpec{ID: "vpc-1", Account: "111111111111", Region: "eu-central-1"},
		Status: networkv1.NetworkStatus{Name: "hub", OverlapsWith: []string{"vpc-2"}},
	}
	SetScope(scope, aws, []networkv1.Network{net}, []networkv1.Subnet{sub},
		[]TargetResult{{Account: "111111111111", Region: "eu-central-1", Synced: true}}, 42)
	Unmanaged(scope, aws, "111111111111", "eu-central-1", KindNetwork, []string{"vpc-9"})
	Unmanaged(scope, aws, "111111111111", "eu-central-1", KindSubnet, []string{"subnet-9"})
	AutoImportTarget(scope, aws, "111111111111", "eu-central-1")
	AutoImport(scope, aws, "111111111111", "eu-central-1", "applied")
	APIThrottled(scope, aws, "111111111111", "eu-central-1", "DescribeVpcs")
	ClaimReady("team", "claim", aws, []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "NoSpace"}})
	ImportReady("team", "import", aws, "Applied", []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Applied"}})
}

// Every recording function writes the metric it is for, with the provider lowercase like every
// other label value, and networks called networks.
func TestEveryMetricIsRecorded(t *testing.T) {
	const scope = "every-metric"
	t.Cleanup(func() { Forget(scope) })
	exerciseEverything(t, scope)

	for name, fam := range families() {
		if len(series(t, fam.c)) == 0 {
			t.Errorf("%s: no series recorded, the test does not exercise it", name)
		}
	}
	got := series(t, unmanagedCurrent.vec)
	want := fmt.Sprintf("account=111111111111 kind=network provider=aws region=eu-central-1 scope=%s = 1", scope)
	if !slices.Contains(got, want) {
		t.Errorf("hs_unmanaged_resources = %v, want a series %q", got, want)
	}
}

// series renders a collector's series as sorted "label=value … = value" lines.
func series(t *testing.T, c prometheus.Collector) []string {
	t.Helper()
	ch := make(chan prometheus.Metric, 256)
	go func() { c.Collect(ch); close(ch) }()
	var out []string
	for m := range ch {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			t.Fatal(err)
		}
		l := prometheus.Labels{}
		for _, lp := range pb.GetLabel() {
			l[lp.GetName()] = lp.GetValue()
		}
		keys := make([]string, 0, len(l))
		for k := range l {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+l[k])
		}
		v := pb.GetGauge().GetValue() + pb.GetCounter().GetValue()
		out = append(out, fmt.Sprintf("%s = %g", strings.Join(parts, " "), v))
	}
	slices.Sort(out)
	return out
}

// A claim whose scope turns up later gets its provider then, without leaving the series
// without one behind.
func TestClaimReadyFollowsItsProvider(t *testing.T) {
	t.Cleanup(func() { ForgetClaim("team", "late") })
	notFound := []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "ScopeNotFound"}}
	ClaimReady("team", "late", "", notFound)
	ClaimReady("team", "late", aws, []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Created"}})

	got := series(t, claimReady.vec)
	var mine []string
	for _, s := range got {
		if strings.Contains(s, "name=late ") {
			mine = append(mine, s)
		}
	}
	want := []string{"name=late namespace=team provider=aws reason=Created = 1"}
	if strings.Join(mine, "\n") != strings.Join(want, "\n") {
		t.Errorf("hs_subnet_claim_ready for team/late = %v, want %v", mine, want)
	}
}
