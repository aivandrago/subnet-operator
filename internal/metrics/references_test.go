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
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// The alerts, the dashboards and the docs name metrics, labels and alerts as plain text, so
// nothing but these tests notices when one of them names something the operator does not
// export. They read the files from the repository root.

const root = "../.."

// otherMetrics are exported by other packages of the operator.
var otherMetrics = map[string][]string{
	"hs_migration_pending_objects":                 {"kind"},
	"hs_crd_stored_versions":                       {"kind", "version"},
	"hs_storage_migration_rewritten_objects_total": {"kind"},
}

// removedAllowed are the files that may still name the hs_aws_* metrics 0.9 removed: the ones
// that explain the rename.
var removedAllowed = map[string]bool{
	"docs/policy.md":                     true,
	"docs/operations/upgrades.md":        true,
	"docs/adr/0002-multi-cloud-model.md": true,
}

var metricName = regexp.MustCompile(`\bhs_[a-z0-9_]*[a-z0-9_]`)

// neutralLabels maps every current metric name to its labels.
func neutralLabels() map[string][]string {
	out := map[string][]string{}
	for name, fam := range families() {
		out[name] = fam.f.labels
	}
	maps.Copy(out, otherMetrics)
	return out
}

func removedNames() map[string]bool {
	out := map[string]bool{}
	for _, r := range renames {
		out[r.removed] = true
	}
	return out
}

func read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Grafana panels suffix counters in PromQL, and Prometheus has histogram suffixes; neither is
// used here, so a name is either exported or a mistake.
func TestAlertsAndDashboardsUseTheNeutralMetrics(t *testing.T) {
	known := neutralLabels()
	dashboards, _ := filepath.Glob(filepath.Join(root, "charts/subnet-operator/dashboards/*.json"))
	web, _ := filepath.Glob(filepath.Join(root, "site/dashboard/*.js"))
	files := make([]string, 0, 2+len(dashboards)+len(web))
	files = append(files, "charts/subnet-operator/templates/prometheusrule.yaml", "hack/alerts/alerts_test.yaml")
	for _, f := range append(dashboards, web...) {
		rel, _ := filepath.Rel(root, f)
		files = append(files, rel)
	}
	for _, f := range files {
		for _, name := range metricName.FindAllString(read(t, f), -1) {
			if _, ok := known[name]; !ok {
				t.Errorf("%s uses %s, which the operator does not export under that name", f, name)
			}
		}
	}
}

// Docs may name the removed hs_aws_* metrics where they explain the rename; everywhere else
// they name what the operator exports. A name ending in _ (hs_aws_, hs_subnet_) is a prefix and
// must prefix something real.
func TestDocsNameRealMetrics(t *testing.T) {
	known := neutralLabels()
	removed := removedNames()
	var files []string
	for _, dir := range []string{"docs", "site", "charts/subnet-operator"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			switch filepath.Ext(p) {
			case ".md", ".html", ".svg", ".yaml", ".txt":
				rel, _ := filepath.Rel(root, p)
				files = append(files, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	files = append(files, "README.md")
	for _, f := range files {
		for _, name := range metricName.FindAllString(read(t, f), -1) {
			_, current := known[name]
			old := removed[name]
			switch {
			case strings.HasSuffix(name, "_"):
				if !prefixesAny(name, known) && !prefixesAny(name, removed) {
					t.Errorf("%s: no metric starts with %s", f, name)
				}
			case current:
			case old:
				if !removedAllowed[filepath.ToSlash(f)] {
					t.Errorf("%s names %s, removed in 0.9; name its replacement", f, name)
				}
			default:
				t.Errorf("%s names %s, which the operator does not export", f, name)
			}
		}
	}
}

func prefixesAny[V any](prefix string, names map[string]V) bool {
	for n := range names {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

// A rule template, split into its alerts.
type alertRule struct {
	name, body string
}

func alertRules(t *testing.T) []alertRule {
	t.Helper()
	src := read(t, "charts/subnet-operator/templates/prometheusrule.yaml")
	parts := regexp.MustCompile(`(?m)^\s*- alert: `).Split(src, -1)
	out := make([]alertRule, 0, len(parts)-1)
	for _, p := range parts[1:] {
		name, body, _ := strings.Cut(p, "\n")
		out = append(out, alertRule{name: strings.TrimSpace(name), body: body})
	}
	if len(out) == 0 {
		t.Fatal("no alerts found in the PrometheusRule template")
	}
	return out
}

// An alert's text is filled from the labels of the series that fired it. A label the metric
// does not have renders as an empty string, which promtool only notices if a test gives the
// series that label, and the tests below are written by hand.
func TestAlertTemplatesUseLabelsTheMetricHas(t *testing.T) {
	known := neutralLabels()
	labelRef := regexp.MustCompile(`\$labels\.([a-z_]+)`)
	for _, rule := range alertRules(t) {
		names := metricName.FindAllString(rule.body, -1)
		if len(names) == 0 {
			continue // SubnetOperatorDown reads up, not the operator's metrics
		}
		for _, ref := range labelRef.FindAllStringSubmatch(rule.body, -1) {
			for _, name := range names {
				if _, ok := known[name]; !ok {
					continue // TestAlertsAndDashboardsUseTheNeutralMetrics says so
				}
				if !slices.Contains(known[name], ref[1]) {
					t.Errorf("alert %s uses $labels.%s, which %s does not have", rule.name, ref[1], name)
				}
			}
		}
	}
}

// promtool tests, the parts these checks need.
type promtoolTests struct {
	Tests []struct {
		Name        string `json:"name"`
		InputSeries []struct {
			Series string `json:"series"`
		} `json:"input_series"`
		AlertRuleTest []struct {
			Alertname string `json:"alertname"`
			ExpAlerts []struct {
				ExpLabels map[string]string `json:"exp_labels"`
			} `json:"exp_alerts"`
		} `json:"alert_rule_test"`
	} `json:"tests"`
}

// Every alert the chart ships has a test in which it fires and one in which it stays quiet, so
// `make test-alerts` fails when a rule can no longer fire, or fires on the wrong thing.
func TestEveryAlertFiresAndStaysQuietInATest(t *testing.T) {
	var tests promtoolTests
	if err := yaml.Unmarshal([]byte(read(t, "hack/alerts/alerts_test.yaml")), &tests); err != nil {
		t.Fatal(err)
	}
	fires, quiet := map[string]bool{}, map[string]bool{}
	for _, tc := range tests.Tests {
		for _, a := range tc.AlertRuleTest {
			if len(a.ExpAlerts) > 0 {
				fires[a.Alertname] = true
			} else {
				quiet[a.Alertname] = true
			}
		}
	}
	for _, rule := range alertRules(t) {
		if !fires[rule.name] {
			t.Errorf("alert %s has no promtool test in which it fires", rule.name)
		}
		if !quiet[rule.name] {
			t.Errorf("alert %s has no promtool test in which it stays quiet", rule.name)
		}
	}
}

// The input series of the promtool tests stand for what the operator exports, so they may only
// carry labels the metric has, and must carry provider: the alerts group and route by it.
func TestAlertTestSeriesLookLikeTheOperatorsOwn(t *testing.T) {
	var tests promtoolTests
	if err := yaml.Unmarshal([]byte(read(t, "hack/alerts/alerts_test.yaml")), &tests); err != nil {
		t.Fatal(err)
	}
	known := neutralLabels()
	series := regexp.MustCompile(`^(hs_[a-z0-9_]+)\{(.*)\}$`)
	label := regexp.MustCompile(`([a-z_]+)\s*=`)
	for _, tc := range tests.Tests {
		for _, in := range tc.InputSeries {
			m := series.FindStringSubmatch(strings.TrimSpace(in.Series))
			if m == nil {
				continue
			}
			if _, ok := known[m[1]]; !ok {
				continue // TestAlertsAndDashboardsUseTheNeutralMetrics says so
			}
			var got []string
			for _, l := range label.FindAllStringSubmatch(m[2], -1) {
				got = append(got, l[1])
				if !slices.Contains(known[m[1]], l[1]) {
					t.Errorf("test %q: %s has no label %s", tc.Name, m[1], l[1])
				}
			}
			if slices.Contains(known[m[1]], labelProvider) && !slices.Contains(got, labelProvider) {
				t.Errorf("test %q: %s without a provider label", tc.Name, m[1])
			}
		}
	}
}

// Each alert has a section in the runbook, and every runbook link points at a section that
// exists.
func TestEveryAlertHasARunbookSection(t *testing.T) {
	runbook := read(t, "docs/operations/runbook.md")
	anchors := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^#+ (.+)$`).FindAllStringSubmatch(runbook, -1) {
		anchors[strings.ToLower(strings.ReplaceAll(strings.TrimSpace(m[1]), " ", "-"))] = true
	}
	link := regexp.MustCompile(`docs/operations/runbook\.md#([a-z0-9-]+)`)
	var missing []string
	for _, rule := range alertRules(t) {
		if !anchors[strings.ToLower(rule.name)] {
			missing = append(missing, rule.name)
		}
		// Alertmanager shows runbook_url next to the alert: each one links its own section.
		own := "runbook_url: \"https://github.com/aivandrago/subnet-operator/blob/main/docs/operations/runbook.md#" +
			strings.ToLower(rule.name) + "\""
		if !strings.Contains(rule.body, own) {
			t.Errorf("alert %s has no runbook_url annotation linking its runbook section", rule.name)
		}
		for _, m := range link.FindAllStringSubmatch(rule.body, -1) {
			if !anchors[m[1]] {
				t.Errorf("alert %s links to runbook.md#%s, which is not a section", rule.name, m[1])
			}
		}
	}
	slices.Sort(missing)
	if len(missing) > 0 {
		t.Errorf("alerts without a section in docs/operations/runbook.md: %v", missing)
	}
}

// The troubleshooting index in docs/operations/README.md links every alert the chart ships to
// its runbook entry, so someone paged by an alert finds it from the index as well as from the
// alert's runbook_url.
func TestTroubleshootingIndexListsEveryAlert(t *testing.T) {
	index := read(t, "docs/operations/README.md")
	_, section, found := strings.Cut(index, "## Troubleshooting index")
	if !found {
		t.Fatal("docs/operations/README.md has no \"## Troubleshooting index\" section")
	}
	for _, rule := range alertRules(t) {
		link := "(runbook.md#" + strings.ToLower(rule.name) + ")"
		if !strings.Contains(section, "`"+rule.name+"`") || !strings.Contains(section, link) {
			t.Errorf("the troubleshooting index in docs/operations/README.md does not list alert %s with a link %s",
				rule.name, link)
		}
	}
}
