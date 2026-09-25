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

package migration

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files in testdata/v1")

// The fixtures in testdata/v1alpha1 are the examples of the last release with the old group,
// plus what a dump of a running cluster looks like. Their conversion is compared with golden
// files; run `go test ./internal/migration -update` after an intended change, and read the diff.
func TestManifestsGolden(t *testing.T) {
	fixtures, err := filepath.Glob(filepath.Join("testdata", "v1alpha1", "*.yaml"))
	if err != nil || len(fixtures) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, fixture := range fixtures {
		t.Run(filepath.Base(fixture), func(t *testing.T) {
			in, err := os.ReadFile(fixture)
			if err != nil {
				t.Fatal(err)
			}
			var out, notes bytes.Buffer
			if err := Manifests(bytes.NewReader(in), &out, &notes); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			if notes.Len() > 0 {
				got += "# notes:\n# " + strings.ReplaceAll(strings.TrimSpace(notes.String()), "\n", "\n# ") + "\n"
			}
			golden := filepath.Join("testdata", "v1", filepath.Base(fixture))
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if got != string(want) {
				t.Errorf("conversion of %s changed; run with -update if that is intended.\n--- got\n%s\n--- want\n%s",
					fixture, got, want)
			}
		})
	}
}

func TestManifestsCopiesOtherDocumentsVerbatim(t *testing.T) {
	in := "# a comment\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x   # kept as written\n"
	var out, notes bytes.Buffer
	if err := Manifests(strings.NewReader(in), &out, &notes); err != nil {
		t.Fatal(err)
	}
	if out.String() != in {
		t.Errorf("got %q, want the document unchanged", out.String())
	}
	if notes.Len() != 0 {
		t.Errorf("notes = %q", notes.String())
	}
}

func TestManifestsPointsAtLeftoverReferences(t *testing.T) {
	in := "apiVersion: rbac.authorization.k8s.io/v1\nkind: ClusterRole\nmetadata:\n  name: r\n" +
		"rules:\n  - apiGroups: [aws.hypersurgery]\n    resources: [vpcs]\n    verbs: [get]\n"
	var out, notes bytes.Buffer
	if err := Manifests(strings.NewReader(in), &out, &notes); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(notes.String(), "document 1: still mentions aws.hypersurgery") {
		t.Errorf("notes = %q", notes.String())
	}
}

func TestManifestsRefusesWhatItCannotConvert(t *testing.T) {
	for name, in := range map[string]string{
		"an unknown field":          "apiVersion: aws.hypersurgery/v1alpha1\nkind: SubnetClaim\nmetadata:\n  name: x\nspec:\n  subnetCount: 3\n",
		"a kind the group lacks":    "apiVersion: aws.hypersurgery/v1alpha1\nkind: Gateway\nmetadata:\n  name: x\n",
		"a document that is broken": "apiVersion: aws.hypersurgery/v1alpha1\nkind: SubnetClaim\nspec: [\n",
	} {
		t.Run(name, func(t *testing.T) {
			var out, notes bytes.Buffer
			if err := Manifests(strings.NewReader(in), &out, &notes); err == nil {
				t.Errorf("converted without complaint:\n%s", out.String())
			}
		})
	}
}

// A v1beta1 manifest only needs its apiVersion changed; everything else, comments included,
// stays as it was written.
func TestManifestsMovesV1beta1DocumentsToV1(t *testing.T) {
	in := "# the payments scope\napiVersion: network.hypersurgery.dev/v1beta1  # old\nkind: NetworkScope\n" +
		"metadata:\n  name: payments # kept as written\nspec:\n  provider: AWS\n" +
		"---\napiVersion: \"network.hypersurgery.dev/v1beta1\"\nkind: SubnetClaim\nmetadata:\n  name: c\n"
	want := "# the payments scope\napiVersion: network.hypersurgery.dev/v1  # old\nkind: NetworkScope\n" +
		"metadata:\n  name: payments # kept as written\nspec:\n  provider: AWS\n" +
		"---\napiVersion: network.hypersurgery.dev/v1\nkind: SubnetClaim\nmetadata:\n  name: c\n"
	var out, notes bytes.Buffer
	if err := Manifests(strings.NewReader(in), &out, &notes); err != nil {
		t.Fatal(err)
	}
	if out.String() != want {
		t.Errorf("got\n%s\nwant\n%s", out.String(), want)
	}
	if notes.Len() != 0 {
		t.Errorf("notes = %q", notes.String())
	}

	for name, in := range map[string]string{
		"a kind the group lacks": "apiVersion: network.hypersurgery.dev/v1beta1\nkind: Gateway\nmetadata:\n  name: x\n",
		"a flow-style document":  "{apiVersion: network.hypersurgery.dev/v1beta1, kind: SubnetClaim, metadata: {name: x}}\n",
	} {
		t.Run(name, func(t *testing.T) {
			out.Reset()
			if err := Manifests(strings.NewReader(in), &out, &notes); err == nil {
				t.Errorf("converted without complaint:\n%s", out.String())
			}
		})
	}
}
