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

package v1

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

// The compatibility promise (docs/api-compatibility.md) checked against the schema: v1 as the
// CRDs in testdata/crds-1.0 define it is the baseline, and the CRDs generated now may only add
// to it. That catches what a review can miss in a long CRD diff: a field removed or renamed, a
// type changed, an optional field made required, a list type or key changed, an enum value
// dropped, validation tightened.
//
// The baseline is the schema 1.0 ships. Refresh it only in the release commit of 1.0; after
// that, never.

// allowedNewRules are CEL rules added after the baseline that are not a tightening: each one
// constrains only fields the baseline did not have. Key: the schema path and the rule.
var allowedNewRules = map[string]bool{}

func TestV1OnlyGrowsFromTheBaseline(t *testing.T) {
	baseline := v1Schemas(t, filepath.Join("testdata", "crds-1.0"))
	current := v1Schemas(t, filepath.Join("..", "..", "config", "crd", "bases"))
	if len(baseline) == 0 {
		t.Fatal("no baseline CRDs")
	}
	for name, old := range baseline {
		now, ok := current[name]
		if !ok {
			t.Errorf("%s: the CRD, or its v1 version, is gone", name)
			continue
		}
		for _, problem := range compareSchemas(name, old, now) {
			t.Error(problem)
		}
	}
}

// The check itself has to catch what it claims to.
func TestCompareSchemasFindsBreakingChanges(t *testing.T) {
	base := func() *apiextensionsv1.JSONSchemaProps {
		return &apiextensionsv1.JSONSchemaProps{Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"name":   {Type: "string", MaxLength: new(int64(10))},
			"mode":   {Type: "string", Enum: []apiextensionsv1.JSON{{Raw: []byte(`"A"`)}, {Raw: []byte(`"B"`)}}},
			"zones":  {Type: "array", XListType: new("set"), Items: &apiextensionsv1.JSONSchemaPropsOrArray{Schema: &apiextensionsv1.JSONSchemaProps{Type: "string"}}},
			"counts": {Type: "integer", Minimum: new(1.0)},
		}}
	}
	for change, mutate := range map[string]func(s *apiextensionsv1.JSONSchemaProps){
		"a removed field": func(s *apiextensionsv1.JSONSchemaProps) { delete(s.Properties, "name") },
		"a changed type": func(s *apiextensionsv1.JSONSchemaProps) {
			p := s.Properties["counts"]
			p.Type = "string"
			s.Properties["counts"] = p
		},
		"a new required field": func(s *apiextensionsv1.JSONSchemaProps) { s.Required = []string{"name"} },
		"a dropped enum value": func(s *apiextensionsv1.JSONSchemaProps) {
			p := s.Properties["mode"]
			p.Enum = p.Enum[:1]
			s.Properties["mode"] = p
		},
		"a changed list type": func(s *apiextensionsv1.JSONSchemaProps) {
			p := s.Properties["zones"]
			p.XListType = new("atomic")
			s.Properties["zones"] = p
		},
		"a shorter maximum": func(s *apiextensionsv1.JSONSchemaProps) {
			p := s.Properties["name"]
			p.MaxLength = new(int64(5))
			s.Properties["name"] = p
		},
		"a higher minimum": func(s *apiextensionsv1.JSONSchemaProps) {
			p := s.Properties["counts"]
			p.Minimum = new(2.0)
			s.Properties["counts"] = p
		},
		"a new pattern": func(s *apiextensionsv1.JSONSchemaProps) {
			p := s.Properties["name"]
			p.Pattern = "^a"
			s.Properties["name"] = p
		},
		"a new validation rule": func(s *apiextensionsv1.JSONSchemaProps) {
			s.XValidations = apiextensionsv1.ValidationRules{{Rule: "false"}}
		},
	} {
		now := base()
		mutate(now)
		if problems := compareSchemas("test", base(), now); len(problems) == 0 {
			t.Errorf("%s went unnoticed", change)
		}
	}

	grown := base()
	grown.Properties["added"] = apiextensionsv1.JSONSchemaProps{Type: "string"}
	p := grown.Properties["mode"]
	p.Enum = append(p.Enum, apiextensionsv1.JSON{Raw: []byte(`"C"`)})
	grown.Properties["mode"] = p
	if problems := compareSchemas("test", base(), grown); len(problems) != 0 {
		t.Errorf("additions were refused: %v", problems)
	}
}

// v1Schemas reads the v1 schema of every CRD in a directory, by CRD name.
func v1Schemas(t *testing.T, dir string) map[string]*apiextensionsv1.JSONSchemaProps {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]*apiextensionsv1.JSONSchemaProps{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := yaml.UnmarshalStrict(raw, crd); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for _, v := range crd.Spec.Versions {
			if v.Name == GroupVersion.Version && v.Schema != nil {
				out[crd.Name] = v.Schema.OpenAPIV3Schema
			}
		}
	}
	return out
}

// compareSchemas lists what now takes away from old, recursively.
func compareSchemas(path string, old, now *apiextensionsv1.JSONSchemaProps) []string {
	var problems []string
	add := func(format string, args ...any) {
		problems = append(problems, path+": "+fmt.Sprintf(format, args...))
	}
	compareShape(add, old, now)
	compareLimits(add, old, now)
	compareRules(add, path, old, now)

	for name, o := range old.Properties {
		n, ok := now.Properties[name]
		if !ok {
			add("field %s was removed or renamed", name)
			continue
		}
		problems = append(problems, compareSchemas(path+"."+name, &o, &n)...)
	}
	if old.Items != nil && old.Items.Schema != nil {
		if now.Items == nil || now.Items.Schema == nil {
			add("the item schema was removed")
		} else {
			problems = append(problems, compareSchemas(path+"[]", old.Items.Schema, now.Items.Schema)...)
		}
	}
	if old.AdditionalProperties != nil && old.AdditionalProperties.Schema != nil {
		if now.AdditionalProperties == nil || now.AdditionalProperties.Schema == nil {
			add("the value schema of the map was removed")
		} else {
			problems = append(problems, compareSchemas(path+"{}", old.AdditionalProperties.Schema,
				now.AdditionalProperties.Schema)...)
		}
	}
	return problems
}

type report func(format string, args ...any)

// compareShape checks what a value is: its type, whether it is required, its enum, how lists
// and maps merge, its pattern and format, and its default.
func compareShape(add report, old, now *apiextensionsv1.JSONSchemaProps) {
	if old.Type != now.Type {
		add("type %q became %q", old.Type, now.Type)
	}
	for _, r := range now.Required {
		if !slices.Contains(old.Required, r) {
			add("%s became required", r)
		}
	}
	for _, e := range old.Enum {
		if !slices.ContainsFunc(now.Enum, func(n apiextensionsv1.JSON) bool { return string(n.Raw) == string(e.Raw) }) {
			add("enum value %s was dropped", e.Raw)
		}
	}
	if len(old.Enum) == 0 && len(now.Enum) > 0 {
		add("an enum was added to an existing field")
	}
	for what, pair := range map[string][2]string{
		"list type":                            {deref(old.XListType), deref(now.XListType)},
		"list map keys":                        {strings.Join(old.XListMapKeys, ","), strings.Join(now.XListMapKeys, ",")},
		"map type":                             {deref(old.XMapType), deref(now.XMapType)},
		"pattern":                              {old.Pattern, now.Pattern},
		"format":                               {old.Format, now.Format},
		"default":                              {rawOf(old.Default), rawOf(now.Default)},
		"x-kubernetes-preserve-unknown-fields": {fmt.Sprint(deref(old.XPreserveUnknownFields)), fmt.Sprint(deref(now.XPreserveUnknownFields))},
	} {
		if pair[0] != pair[1] {
			add("%s %q became %q", what, pair[0], pair[1])
		}
	}
}

// compareLimits refuses a lower maximum or a higher minimum than the baseline had, or one
// where it had none.
func compareLimits(add report, old, now *apiextensionsv1.JSONSchemaProps) {
	for what, pair := range map[string][2]*int64{
		"maxLength":     {old.MaxLength, now.MaxLength},
		"maxItems":      {old.MaxItems, now.MaxItems},
		"maxProperties": {old.MaxProperties, now.MaxProperties},
	} {
		if o, n := pair[0], pair[1]; n != nil && (o == nil || *n < *o) {
			add("%s lowered to %d", what, *n)
		}
	}
	for what, pair := range map[string][2]*int64{
		"minLength":     {old.MinLength, now.MinLength},
		"minItems":      {old.MinItems, now.MinItems},
		"minProperties": {old.MinProperties, now.MinProperties},
	} {
		if o, n := pair[0], pair[1]; n != nil && (o == nil || *n > *o) {
			add("%s raised to %d", what, *n)
		}
	}
	if now.Minimum != nil && (old.Minimum == nil || *now.Minimum > *old.Minimum) {
		add("minimum raised to %v", *now.Minimum)
	}
	if now.Maximum != nil && (old.Maximum == nil || *now.Maximum < *old.Maximum) {
		add("maximum lowered to %v", *now.Maximum)
	}
}

// compareRules refuses a CEL rule the baseline did not have, unless it is listed as allowed.
func compareRules(add report, path string, old, now *apiextensionsv1.JSONSchemaProps) {
	for _, r := range now.XValidations {
		known := slices.ContainsFunc(old.XValidations, func(o apiextensionsv1.ValidationRule) bool { return o.Rule == r.Rule })
		if !known && !allowedNewRules[path+" "+r.Rule] {
			add("new validation rule %q; if it constrains only fields 1.0 did not have, add it to allowedNewRules", r.Rule)
		}
	}
}

func rawOf(j *apiextensionsv1.JSON) string {
	if j == nil {
		return ""
	}
	return string(j.Raw)
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
