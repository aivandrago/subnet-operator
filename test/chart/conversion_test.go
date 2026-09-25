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

package chart

import (
	"os"
	"slices"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	"sigs.k8s.io/yaml"

	"hypersurgery.dev/subnet-operator/internal/crdversions"
)

// The operator configures its CRDs' conversion itself (Helm does not upgrade crds/): the
// webhook when it serves one, None otherwise, so that a CRD never points at a webhook that is
// not there.
func TestCRDConversionFollowsTheWebhook(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  []string
		want []string
	}{
		{"by default", nil,
			[]string{"--crd-conversion=webhook", "--conversion-webhook-service=subnets/release-subnet-operator-webhook"}},
		{"with cert-manager", []string{"webhook.certificate.certManager=true"},
			[]string{"--crd-conversion=webhook", "--conversion-webhook-service=subnets/release-subnet-operator-webhook"}},
		{"without the webhook", []string{"webhook.enabled=false"}, []string{"--crd-conversion=none"}},
		{"without the conversion", []string{"webhook.conversion.enabled=false"}, []string{"--crd-conversion=none"}},
		// helm upgrade --reuse-values keeps the values of the release it upgrades, which have no
		// webhook.conversion.
		{"with values from before 1.0", []string{"webhook.conversion=null"},
			[]string{"--crd-conversion=webhook", "--conversion-webhook-service=subnets/release-subnet-operator-webhook"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, _ := managerSettings(t, tc.set...)
			var got []string
			for _, a := range args {
				if strings.HasPrefix(a, "--crd-conversion") || strings.HasPrefix(a, "--conversion-webhook-service") {
					got = append(got, a)
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// The Service the conversion points at is the webhook's, on the port the operator assumes.
func TestTheConversionServiceIsTheWebhookService(t *testing.T) {
	r := render(t)
	for _, s := range r.services {
		if s.Name == "release-subnet-operator-webhook" {
			if len(s.Spec.Ports) != 1 || s.Spec.Ports[0].Port != 443 {
				t.Errorf("ports = %v, want 443", s.Spec.Ports)
			}
			return
		}
	}
	t.Errorf("no webhook Service among %v", r.names)
}

// The operator may read and change its own CRDs, named one by one, and no other.
func TestTheOperatorMayChangeOnlyItsOwnCRDs(t *testing.T) {
	r := render(t)
	var rules []rbacv1.PolicyRule
	for _, role := range r.clusterRoles {
		if role.Name == "release-subnet-operator" {
			rules = role.Rules
		}
	}
	if rules == nil {
		t.Fatalf("no operator ClusterRole among %v", r.names)
	}
	want := map[string][]string{
		"customresourcedefinitions":        {"get", "patch"},
		"customresourcedefinitions/status": {"get", "update"},
	}
	seen := map[string]bool{}
	for _, rule := range rules {
		if !slices.Contains(rule.APIGroups, "apiextensions.k8s.io") {
			continue
		}
		for _, res := range rule.Resources {
			verbs, ok := want[res]
			if !ok {
				t.Errorf("unexpected apiextensions resource %s", res)
				continue
			}
			seen[res] = true
			if !slices.Equal(rule.Verbs, verbs) {
				t.Errorf("%s: verbs %v, want %v", res, rule.Verbs, verbs)
			}
			got, names := slices.Sorted(slices.Values(rule.ResourceNames)), slices.Sorted(slices.Values(crdversions.CRDNames()))
			if !slices.Equal(got, names) {
				t.Errorf("%s: resourceNames %v, want %v", res, got, names)
			}
		}
	}
	for res := range want {
		if !seen[res] {
			t.Errorf("no rule for %s", res)
		}
	}

	// The kustomize install's role is generated from the operator's markers; it says the same.
	raw := readFile(t, "../../config/rbac/role.yaml")
	var role rbacv1.ClusterRole
	if err := yaml.Unmarshal(raw, &role); err != nil {
		t.Fatal(err)
	}
	for _, rule := range role.Rules {
		if slices.Contains(rule.APIGroups, "apiextensions.k8s.io") {
			got, names := slices.Sorted(slices.Values(rule.ResourceNames)), slices.Sorted(slices.Values(crdversions.CRDNames()))
			if !slices.Equal(got, names) {
				t.Errorf("config/rbac/role.yaml %v: resourceNames %v, want %v", rule.Resources, got, names)
			}
		}
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
