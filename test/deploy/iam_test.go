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

// Package deploy checks the IAM roles in deploy/iam. They are what an account actually grants
// the operator, whatever the code promises, so the promises of docs/security/threat-model.md
// are tested against them: discovery reads and nothing else, and the write role writes only
// to the kinds of resource the operator creates or tags.
package deploy

import (
	"os"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

type statement struct {
	actions   []string
	resources []string
}

// allowStatements returns the Allow statements of the inline policies of one role in a
// CloudFormation template. The template is walked as YAML nodes, because the intrinsic
// functions (!Sub, !Ref) are tags a plain decode would not know.
func allowStatements(t *testing.T, file, role string) []statement {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	policies := lookup(doc.Content[0], "Resources", role, "Properties", "Policies")
	if policies == nil {
		t.Fatalf("%s: no Resources.%s.Properties.Policies", file, role)
	}
	var out []statement
	for _, p := range policies.Content {
		for _, s := range lookup(p, "PolicyDocument", "Statement").Content {
			if effect := lookup(s, "Effect"); effect == nil || effect.Value != "Allow" {
				continue
			}
			out = append(out, statement{actions: scalars(lookup(s, "Action")), resources: scalars(lookup(s, "Resource"))})
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s: %s allows nothing", file, role)
	}
	return out
}

func lookup(n *yaml.Node, path ...string) *yaml.Node {
	for _, key := range path {
		if n == nil || n.Kind != yaml.MappingNode {
			return nil
		}
		var next *yaml.Node
		for i := 0; i+1 < len(n.Content); i += 2 {
			if n.Content[i].Value == key {
				next = n.Content[i+1]
			}
		}
		n = next
	}
	return n
}

// scalars flattens a scalar or a list of scalars; a !Sub string counts as its template.
func scalars(n *yaml.Node) []string {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.ScalarNode {
		return []string{n.Value}
	}
	var out []string
	for _, c := range n.Content {
		out = append(out, scalars(c)...)
	}
	return out
}

func TestReadOnlyRoleOnlyDescribes(t *testing.T) {
	for _, s := range allowStatements(t, "../../deploy/iam/spoke-readonly-role.cfn.yaml", "ReadOnlyRole") {
		for _, a := range s.actions {
			if !strings.HasPrefix(a, "ec2:Describe") {
				t.Errorf("the discovery role allows %s; it must only read", a)
			}
		}
	}
}

// The write role may call its writes only on the resource types the operator touches. A
// write on "*" — ec2:CreateTags above all — would reach every resource in the account.
func TestWriteRoleWritesOnlyNetworks(t *testing.T) {
	allowed := map[string][]string{
		"ec2:CreateSubnet":          {":vpc/", ":subnet/"},
		"ec2:CreateTags":            {":vpc/", ":subnet/"},
		"ec2:ModifySubnetAttribute": {":subnet/"},
		"ec2:AssociateRouteTable":   {":subnet/", ":route-table/"},
	}
	seen := map[string]bool{}
	for _, s := range allowStatements(t, "../../deploy/iam/spoke-write-role.cfn.yaml", "WriteRole") {
		for _, a := range s.actions {
			if strings.HasPrefix(a, "ec2:Describe") {
				continue
			}
			types, known := allowed[a]
			if !known {
				t.Errorf("the write role allows %s, which the operator never calls", a)
				continue
			}
			seen[a] = true
			for _, r := range s.resources {
				ok := false
				for _, typ := range types {
					ok = ok || strings.Contains(r, typ)
				}
				if !ok {
					t.Errorf("%s is allowed on %q; it must be limited to %v", a, r, types)
				}
			}
		}
	}
	for a := range allowed {
		if !seen[a] {
			t.Errorf("the write role does not allow %s, which subnet creation needs", a)
		}
	}
}
