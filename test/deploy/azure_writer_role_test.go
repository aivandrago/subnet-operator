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

package deploy

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The Azure writer role (deploy/azure/writer-role.json, #53), checked like the AWS and GCP
// write roles: it creates subnets and writes tags, and never deletes, never changes a virtual
// network, and grants nothing. Replace AssignableScopes with the subscriptions it is created
// for, then
//
//	az role definition create --role-definition deploy/azure/writer-role.json
//	az role assignment create --assignee <write identity> --role "Subnet operator writer" \
//	  --scope /subscriptions/<id>/resourceGroups/<network resource group>
//
// Imports alone need only Microsoft.Resources/tags/read and /write, which the built-in Tag
// Contributor role grants.

type azureRole struct {
	Name             string   `json:"Name"`
	IsCustom         bool     `json:"IsCustom"`
	Description      string   `json:"Description"`
	Actions          []string `json:"Actions"`
	NotActions       []string `json:"NotActions"`
	DataActions      []string `json:"DataActions"`
	NotDataActions   []string `json:"NotDataActions"`
	AssignableScopes []string `json:"AssignableScopes"`
}

func loadAzureWriterRole(t *testing.T) azureRole {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "azure", "writer-role.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r azureRole
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("writer-role.json: %v", err)
	}
	return r
}

func TestAzureWriterRole(t *testing.T) {
	r := loadAzureWriterRole(t)
	if r.Name == "" || !r.IsCustom || r.Description == "" || len(r.AssignableScopes) == 0 {
		t.Errorf("not a custom role definition: %+v", r)
	}
	if len(r.DataActions) > 0 || len(r.NotActions) > 0 || len(r.NotDataActions) > 0 {
		t.Errorf("the role grants data actions or relies on NotActions: %+v", r)
	}
	if !slices.IsSorted(r.Actions) || len(slices.Compact(slices.Clone(r.Actions))) != len(r.Actions) {
		t.Errorf("actions are not sorted and distinct: %v", r.Actions)
	}
	// What the provider calls (internal/cloud/azure/writer.go): read and create a subnet, poll
	// the operation, read and merge tags.
	for _, needed := range []string{
		"Microsoft.Network/virtualNetworks/subnets/read",
		"Microsoft.Network/virtualNetworks/subnets/write",
		"Microsoft.Network/locations/operations/read",
		"Microsoft.Resources/tags/read",
		"Microsoft.Resources/tags/write",
	} {
		if !slices.Contains(r.Actions, needed) {
			t.Errorf("the writer role lacks %s", needed)
		}
	}
	for _, a := range r.Actions {
		lower := strings.ToLower(a)
		switch {
		case strings.Contains(a, "*"):
			t.Errorf("the writer role grants a wildcard: %s", a)
		case strings.HasSuffix(lower, "/delete"), strings.HasSuffix(lower, "/action"):
			t.Errorf("the writer role grants %s", a)
		case strings.HasPrefix(lower, "microsoft.authorization/"):
			t.Errorf("the writer role touches role assignments: %s", a)
		case lower == "microsoft.network/virtualnetworks/write":
			t.Errorf("the writer role may change virtual networks (address space, peerings): %s", a)
		}
	}
}
