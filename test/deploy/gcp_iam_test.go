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
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// The GCP custom roles in deploy/gcp, checked like the AWS roles: discovery reads and nothing
// else, the write role creates and binds but never deletes, and the operator's own identity
// may only get access tokens of the service accounts it impersonates.

type gcpRole struct {
	Title               string   `yaml:"title"`
	Description         string   `yaml:"description"`
	Stage               string   `yaml:"stage"`
	IncludedPermissions []string `yaml:"includedPermissions"`
}

func loadGCPRole(t *testing.T, name string) gcpRole {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "gcp", name))
	if err != nil {
		t.Fatal(err)
	}
	var r gcpRole
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return r
}

// permissionPattern is the shape of an IAM permission: service.resource.verb.
var permissionPattern = regexp.MustCompile(`^[a-z]+\.[a-zA-Z]+\.[a-zA-Z]+$`)

// Every file is a role definition `gcloud iam roles create --file` takes.
func TestGCPRolesAreRoleDefinitions(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "deploy", "gcp", "*.yaml"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no roles in deploy/gcp: %v", err)
	}
	for _, f := range files {
		r := loadGCPRole(t, filepath.Base(f))
		if r.Title == "" || r.Description == "" || r.Stage != "GA" || len(r.IncludedPermissions) == 0 {
			t.Errorf("%s: %+v", f, r)
		}
		if !slices.IsSorted(r.IncludedPermissions) || len(slices.Compact(slices.Clone(r.IncludedPermissions))) !=
			len(r.IncludedPermissions) {
			t.Errorf("%s: permissions are not sorted and distinct: %v", f, r.IncludedPermissions)
		}
		for _, p := range r.IncludedPermissions {
			if !permissionPattern.MatchString(p) {
				t.Errorf("%s: %q is not a permission name", f, p)
			}
			if strings.Contains(p, "*") || strings.HasSuffix(p, ".setIamPolicy") || strings.HasSuffix(p, ".delete") ||
				strings.Contains(p, "deleteTagBinding") {
				t.Errorf("%s grants %s", f, p)
			}
		}
	}
}

func TestGCPReaderRoleOnlyReads(t *testing.T) {
	read := regexp.MustCompile(`\.(get|list|listEffectiveTags|listTagBindings)$`)
	for _, p := range loadGCPRole(t, "reader-role.yaml").IncludedPermissions {
		if !read.MatchString(p) {
			t.Errorf("the reader role grants %s; it must only read", p)
		}
	}
}

// The writer role's writes are the ones the operator makes; anything else it grants reads.
func TestGCPWriterRoleWritesOnlyWhatTheOperatorWrites(t *testing.T) {
	writes := []string{"compute.subnetworks.create", "compute.networks.updatePolicy",
		"compute.subnetworks.createTagBinding", "compute.networks.createTagBinding"}
	read := regexp.MustCompile(`\.(get|list|listEffectiveTags|listTagBindings)$`)
	for _, p := range loadGCPRole(t, "writer-role.yaml").IncludedPermissions {
		if !slices.Contains(writes, p) && !read.MatchString(p) {
			t.Errorf("the writer role grants %s", p)
		}
	}
}

// The write identity holds the writer role alone (with tag user), so the role carries every
// call the write path makes: the writes, reading back a subnetwork, its operation and its
// bindings, and the project number that tag bindings name resources by.
func TestGCPWriterRoleCoversTheWritePath(t *testing.T) {
	got := loadGCPRole(t, "writer-role.yaml").IncludedPermissions
	for _, p := range []string{"compute.subnetworks.create", "compute.networks.updatePolicy",
		"compute.subnetworks.createTagBinding", "compute.networks.createTagBinding", "compute.subnetworks.get",
		"compute.networks.get", "compute.regionOperations.get", "compute.subnetworks.listEffectiveTags",
		"compute.networks.listEffectiveTags", "resourcemanager.projects.get"} {
		if !slices.Contains(got, p) {
			t.Errorf("the writer role lacks %s", p)
		}
	}
}

func TestGCPTagRolesNeverUnbindOrDelete(t *testing.T) {
	for file, writes := range map[string][]string{
		"tag-user-role.yaml":          {"resourcemanager.tagValueBindings.create"},
		"tag-value-creator-role.yaml": {"resourcemanager.tagValues.create"},
	} {
		for _, p := range loadGCPRole(t, file).IncludedPermissions {
			if !slices.Contains(writes, p) && !strings.HasSuffix(p, ".get") && !strings.HasSuffix(p, ".list") {
				t.Errorf("%s grants %s", file, p)
			}
		}
	}
}

func TestGCPImpersonatorRoleOnlyGetsAccessTokens(t *testing.T) {
	got := loadGCPRole(t, "impersonator-role.yaml").IncludedPermissions
	if !slices.Equal(got, []string{"iam.serviceAccounts.getAccessToken"}) {
		t.Errorf("the impersonator role grants %v, want iam.serviceAccounts.getAccessToken only", got)
	}
}

// The change event subscriber only reads the subscription: no publishing, which would let the
// operator forge its own events, and nothing on topics or other subscriptions.
func TestGCPEventsSubscriberRoleOnlyConsumes(t *testing.T) {
	got := loadGCPRole(t, "events-subscriber-role.yaml").IncludedPermissions
	if !slices.Equal(got, []string{"pubsub.subscriptions.consume"}) {
		t.Errorf("the events subscriber role grants %v, want pubsub.subscriptions.consume only", got)
	}
}
