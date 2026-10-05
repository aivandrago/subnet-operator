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
	"path/filepath"
	"slices"
	"testing"

	"sigs.k8s.io/yaml"
)

// Every generated CRD must reach both ways of installing the operator: the chart's crds/
// directory and the kustomize install, which lists its CRDs by name. controller-gen writes
// new files without touching either list, and a kind missing from them only shows as a
// NoKindMatch once the operator runs.
func TestEveryCRDIsInstalled(t *testing.T) {
	generated, err := filepath.Glob("../../config/crd/bases/*.yaml")
	if err != nil || len(generated) == 0 {
		t.Fatalf("no generated CRDs: %v", err)
	}

	raw, err := os.ReadFile("../../config/crd/kustomization.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var kustomization struct {
		Resources []string `json:"resources"`
	}
	if err := yaml.Unmarshal(raw, &kustomization); err != nil {
		t.Fatal(err)
	}

	for _, crd := range generated {
		name := filepath.Base(crd)
		if !slices.Contains(kustomization.Resources, "bases/"+name) {
			t.Errorf("config/crd/kustomization.yaml does not list bases/%s", name)
		}
		if _, err := os.Stat(filepath.Join(chartDir, "crds", name)); err != nil {
			t.Errorf("the chart does not ship %s (make helm-crds)", name)
		}
	}
}
