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

package v1beta1

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/conversion"
	"sigs.k8s.io/randfill"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
)

// rounds is how many random objects of each kind go through each round trip.
const rounds = 500

// filler fills every field, status included, with random values: nil or empty now and then,
// several list and map entries otherwise. Times have whole seconds, as the API server stores
// them.
func filler(seed int64) *randfill.Filler {
	return randfill.NewWithSeed(seed).NilChance(0.2).NumElements(1, 3).Funcs(
		func(t *metav1.Time, c randfill.Continue) {
			*t = metav1.Unix(c.Int63n(4_000_000_000), 0)
		},
		// The version and kind are the target's own after a conversion, not copied.
		func(tm *metav1.TypeMeta, _ randfill.Continue) {},
		func(f *metav1.FieldsV1, _ randfill.Continue) {
			f.SetRawBytes([]byte(`{"f:spec":{}}`))
		},
		func(d *metav1.Duration, c randfill.Continue) {
			d.Duration = time.Duration(c.Int63n(int64(1000 * time.Hour)))
		},
	)
}

// pair is one kind at both versions, empty.
type pair struct {
	spoke func() conversion.Convertible
	hub   func() conversion.Hub
}

var kinds = map[string]pair{
	"NetworkScope": {
		func() conversion.Convertible { return &NetworkScope{} },
		func() conversion.Hub { return &networkv1.NetworkScope{} }},
	"Network": {
		func() conversion.Convertible { return &Network{} },
		func() conversion.Hub { return &networkv1.Network{} }},
	"Subnet": {
		func() conversion.Convertible { return &Subnet{} },
		func() conversion.Hub { return &networkv1.Subnet{} }},
	"SubnetClaim": {
		func() conversion.Convertible { return &SubnetClaim{} },
		func() conversion.Hub { return &networkv1.SubnetClaim{} }},
	"ResourceImport": {
		func() conversion.Convertible { return &ResourceImport{} },
		func() conversion.Hub { return &networkv1.ResourceImport{} }},
	"SheetExport": {
		func() conversion.Convertible { return &SheetExport{} },
		func() conversion.Hub { return &networkv1.SheetExport{} }},
}

// Every kind the scheme knows has a round trip here, so a kind added later is not left out.
func TestEveryKindIsCovered(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	own := reflect.TypeFor[Subnet]().PkgPath()
	for kind, typ := range s.KnownTypes(GroupVersion) {
		if typ.PkgPath() != own || strings.HasSuffix(kind, "List") {
			continue
		}
		if _, ok := kinds[kind]; !ok {
			t.Errorf("%s has no round-trip test", kind)
		}
	}
}

// v1beta1 → v1 → v1beta1 gives back what went in, for random objects with every field set.
func TestRoundTripFromV1beta1(t *testing.T) {
	for name, k := range kinds {
		t.Run(name, func(t *testing.T) {
			f := filler(1)
			for range rounds {
				in := k.spoke()
				f.Fill(in)
				hub := k.hub()
				if err := in.ConvertTo(hub); err != nil {
					t.Fatalf("to v1: %v", err)
				}
				out := k.spoke()
				if err := out.ConvertFrom(hub); err != nil {
					t.Fatalf("from v1: %v", err)
				}
				if !apiequality.Semantic.DeepEqual(in, out) {
					t.Fatalf("the round trip changed the object:\n%s", diff(in, out))
				}
			}
		})
	}
}

// v1 → v1beta1 → v1 gives back what went in: what the API server stores (v1) survives being
// served at v1beta1 and written back.
func TestRoundTripFromV1(t *testing.T) {
	for name, k := range kinds {
		t.Run(name, func(t *testing.T) {
			f := filler(2)
			for range rounds {
				in := k.hub()
				f.Fill(in)
				spoke := k.spoke()
				if err := spoke.ConvertFrom(in); err != nil {
					t.Fatalf("to v1beta1: %v", err)
				}
				out := k.hub()
				if err := spoke.ConvertTo(out); err != nil {
					t.Fatalf("to v1: %v", err)
				}
				if !apiequality.Semantic.DeepEqual(in, out) {
					t.Fatalf("the round trip changed the object:\n%s", diff(in, out))
				}
			}
		})
	}
}

// A converted object has the same JSON as the original, apart from its apiVersion: the two
// versions have the same fields, which is also what lets the CRDs fall back to the API
// server's own conversion (strategy None) when the webhook is off.
func TestConvertedJSONIsTheSame(t *testing.T) {
	for name, k := range kinds {
		t.Run(name, func(t *testing.T) {
			f := filler(3)
			for range rounds / 5 {
				in := k.spoke()
				f.Fill(in)
				hub := k.hub()
				if err := in.ConvertTo(hub); err != nil {
					t.Fatal(err)
				}
				if a, b := jsonWithoutTypeMeta(t, in), jsonWithoutTypeMeta(t, hub); a != b {
					t.Fatalf("v1beta1 and v1 differ:\n%s\n%s", a, b)
				}
			}
		})
	}
}

// The metadata is copied, not shared: changing the converted object leaves the original alone.
func TestConversionCopiesMetadata(t *testing.T) {
	in := &SubnetClaim{ObjectMeta: metav1.ObjectMeta{Name: "c", Labels: map[string]string{"a": "b"}}}
	hub := &networkv1.SubnetClaim{}
	if err := in.ConvertTo(hub); err != nil {
		t.Fatal(err)
	}
	hub.Labels["a"] = "changed"
	if in.Labels["a"] != "b" {
		t.Errorf("the v1beta1 object's labels changed with the v1 object's")
	}
}

// A field one side does not have fails the conversion instead of disappearing in it.
func TestConvertFieldsRefusesAFieldTheTargetLacks(t *testing.T) {
	type wider struct {
		A string `json:"a,omitempty"`
		B string `json:"b,omitempty"`
	}
	type narrower struct {
		A string `json:"a,omitempty"`
	}
	if err := convertFields(&wider{A: "x", B: "y"}, &narrower{}); err == nil {
		t.Error("b was dropped without an error")
	}
	var out narrower
	if err := convertFields(&wider{A: "x"}, &out); err != nil || out.A != "x" {
		t.Errorf("got %+v, %v", out, err)
	}
}

func TestConversionRefusesAnotherHub(t *testing.T) {
	if err := (&Subnet{}).ConvertTo(&networkv1.Network{}); err == nil {
		t.Error("a Subnet converted to a Network")
	}
	if err := (&Subnet{}).ConvertFrom(&networkv1.Network{}); err == nil {
		t.Error("a Subnet converted from a Network")
	}
}

func jsonWithoutTypeMeta(t *testing.T, obj any) string {
	t.Helper()
	raw, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "apiVersion")
	delete(m, "kind")
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

func diff(a, b any) string {
	in, _ := json.MarshalIndent(a, "", "  ")
	out, _ := json.MarshalIndent(b, "", "  ")
	return "--- in\n" + string(in) + "\n--- out\n" + string(out)
}
