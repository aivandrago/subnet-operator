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

package azure

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/yaml"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
)

// The Azure examples and the manifests in the Azure guide are what people copy. The CRDs check
// their schema (internal/migration sends every example and doc manifest to the API server);
// this checks what the webhooks add for Azure, so a manifest the webhook would refuse cannot
// reach the docs.

const repoRoot = "../../.."

var yamlBlock = regexp.MustCompile("(?ms)^\\s*```ya?ml\\s*\\n(.*?)^\\s*```")

// azureManifests returns the YAML documents of the Azure examples and of the Azure guide's YAML
// blocks, by where they came from.
func azureManifests(t *testing.T) map[string][][]byte {
	t.Helper()
	out := map[string][][]byte{}
	files, err := filepath.Glob(filepath.Join(repoRoot, "examples", "[0-9]*-azure-*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 2 {
		t.Fatalf("found %d Azure examples, want the scope and the claim at least", len(files))
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[f] = append(out[f], bytes.Split(b, []byte("\n---\n"))...)
	}
	guide := filepath.Join(repoRoot, "docs", "azure.md")
	b, err := os.ReadFile(guide)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range yamlBlock.FindAllSubmatch(b, -1) {
		out[guide] = append(out[guide], bytes.Split(m[1], []byte("\n---\n"))...)
	}
	return out
}

func TestAzureExamplesPassTheProvidersValidation(t *testing.T) {
	p := NewProvider(Options{})
	seen := map[string]int{}
	for file, docs := range azureManifests(t) {
		for _, doc := range docs {
			var head struct {
				APIVersion string `json:"apiVersion"`
				Kind       string `json:"kind"`
			}
			if err := yaml.Unmarshal(doc, &head); err != nil || head.APIVersion != networkv1.GroupVersion.String() {
				continue // a values file or a fragment
			}
			var errs field.ErrorList
			switch head.Kind {
			case "NetworkScope":
				s := &networkv1.NetworkScope{}
				if err := yaml.UnmarshalStrict(doc, s); err != nil {
					t.Fatalf("%s: %v", file, err)
				}
				if s.Spec.Provider != networkv1.ProviderAzure {
					t.Errorf("%s: scope %s is not an Azure scope", file, s.Name)
				}
				_, errs = p.ValidateScope(s)
				if s.Spec.NetworkSelector != nil {
					errs = append(errs, p.ValidateTags(field.NewPath("spec", "networkSelector", "matchTags"),
						s.Spec.NetworkSelector.MatchTags)...)
				}
			case "SubnetClaim":
				c := &networkv1.SubnetClaim{}
				if err := yaml.UnmarshalStrict(doc, c); err != nil {
					t.Fatalf("%s: %v", file, err)
				}
				errs = p.ValidateClaim(c)
				errs = append(errs, p.ValidateTags(field.NewPath("spec", "tags"), c.Spec.Tags)...)
			case "ResourceImport":
				i := &networkv1.ResourceImport{}
				if err := yaml.UnmarshalStrict(doc, i); err != nil {
					t.Fatalf("%s: %v", file, err)
				}
				errs = p.ValidateImport(i)
				errs = append(errs, p.ValidateTags(field.NewPath("spec", "tags"), i.Spec.Tags)...)
			default:
				continue
			}
			seen[head.Kind]++
			if len(errs) > 0 {
				t.Errorf("%s: the Azure provider refuses a %s: %v", file, head.Kind, errs.ToAggregate())
			}
		}
	}
	for _, kind := range []string{"NetworkScope", "SubnetClaim", "ResourceImport"} {
		if seen[kind] == 0 {
			t.Errorf("no Azure %s among the examples and the guide; is the pattern right?", kind)
		}
	}
}
