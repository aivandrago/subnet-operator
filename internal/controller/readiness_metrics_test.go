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

package controller

import (
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// readySeries returns the series one object in the default namespace — where every claim
// and import in these tests lives — has in a readiness gauge, keyed by the labels that
// describe its state — "reason" for a claim, "state/reason" for an import — so a test can check
// both the value and that an old reason did not linger after the object moved on.
func readySeries(metric, name string) map[string]float64 {
	GinkgoHelper()
	families, err := ctrlmetrics.Registry.Gather()
	Expect(err).NotTo(HaveOccurred())
	out := map[string]float64{}
	for _, f := range families {
		if f.GetName() != metric {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["namespace"] != "default" || labels["name"] != name {
				continue
			}
			key := labels["reason"]
			if state, ok := labels["state"]; ok {
				key = strings.Join([]string{state, labels["reason"]}, "/")
			}
			out[key] = m.GetGauge().GetValue()
		}
	}
	return out
}
