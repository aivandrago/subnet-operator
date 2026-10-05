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
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	azurecloud "hypersurgery.dev/subnet-operator/internal/cloud/azure"
	"hypersurgery.dev/subnet-operator/internal/cloud/azure/azurefake"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// The Azure custom roles in deploy/azure, checked like the AWS and GCP roles: discovery reads
// and nothing else, the writer never deletes (azure_writer_role_test.go), and each holds the
// action of every Resource Manager call its identity makes.

// loadAzureRole reads a role definition of deploy/azure (azureRole is in
// azure_writer_role_test.go).
func loadAzureRole(t *testing.T, name string) azureRole {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "azure", name))
	if err != nil {
		t.Fatal(err)
	}
	var r azureRole
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return r
}

// azureActionPattern is the shape of an Azure RBAC action: a resource provider, resource types
// and an operation.
var azureActionPattern = regexp.MustCompile(`^Microsoft\.[A-Za-z]+(/[A-Za-z]+)+/(read|write|action)$`)

// Every file is a role definition `az role definition create --role-definition @file` takes.
func TestAzureRolesAreRoleDefinitions(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("..", "..", "deploy", "azure", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no roles in deploy/azure: %v", err)
	}
	for _, f := range files {
		r := loadAzureRole(t, filepath.Base(f))
		if r.Name == "" || !r.IsCustom || r.Description == "" || len(r.Actions) == 0 || len(r.AssignableScopes) == 0 {
			t.Errorf("%s: %+v", f, r)
		}
		if len(r.NotActions)+len(r.DataActions)+len(r.NotDataActions) != 0 {
			t.Errorf("%s: NotActions, DataActions or NotDataActions: %+v", f, r)
		}
		if !slices.IsSorted(r.Actions) || len(slices.Compact(slices.Clone(r.Actions))) != len(r.Actions) {
			t.Errorf("%s: actions are not sorted and distinct: %v", f, r.Actions)
		}
		for _, a := range r.Actions {
			if !azureActionPattern.MatchString(a) {
				t.Errorf("%s: %q is not a single action (no wildcards)", f, a)
			}
			if strings.HasPrefix(a, "Microsoft.Authorization/") || strings.HasSuffix(a, "/delete") {
				t.Errorf("%s grants %s", f, a)
			}
		}
	}
}

func TestAzureReaderRoleOnlyReads(t *testing.T) {
	for _, a := range loadAzureRole(t, "reader-role.json").Actions {
		if !strings.HasSuffix(a, "/read") {
			t.Errorf("the reader role grants %s; it must only read", a)
		}
	}
}

// armActions maps the Resource Manager requests the provider makes to the actions RBAC checks
// for them. A request no entry matches fails the test below: a new call needs its action here
// and, if it is a read of discovery, in the reader role.
var armActions = []struct {
	method string
	path   *regexp.Regexp
	action string
}{
	{"GET", regexp.MustCompile(`^/subscriptions/[^/]+/providers/Microsoft\.Network/virtualNetworks$`),
		"Microsoft.Network/virtualNetworks/read"},
	{"GET", regexp.MustCompile(`^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/Microsoft\.Network/virtualNetworks$`),
		"Microsoft.Network/virtualNetworks/read"},
	{"GET", regexp.MustCompile(`^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/Microsoft\.Network/` +
		`virtualNetworks/[^/]+$`), "Microsoft.Network/virtualNetworks/read"},
	{"GET", regexp.MustCompile(`^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/Microsoft\.Network/` +
		`virtualNetworks/[^/]+/usages$`), "Microsoft.Network/virtualNetworks/usages/read"},
	{"GET", regexp.MustCompile(`^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/Microsoft\.Network/` +
		`virtualNetworks/[^/]+/subnets/[^/]+$`), "Microsoft.Network/virtualNetworks/subnets/read"},
	{"PUT", regexp.MustCompile(`^/subscriptions/[^/]+/resourceGroups/[^/]+/providers/Microsoft\.Network/` +
		`virtualNetworks/[^/]+/subnets/[^/]+$`), "Microsoft.Network/virtualNetworks/subnets/write"},
	{"GET", regexp.MustCompile(`^/subscriptions/[^/]+/providers/Microsoft\.Network/locations/[^/]+/operations/[^/]+$`),
		"Microsoft.Network/locations/operations/read"},
	{"GET", regexp.MustCompile(`/providers/Microsoft\.Network/virtualNetworks/[^/]+/providers/Microsoft\.Resources/` +
		`tags/default$`), "Microsoft.Resources/tags/read"},
	{"PATCH", regexp.MustCompile(`/providers/Microsoft\.Network/virtualNetworks/[^/]+/providers/Microsoft\.Resources/` +
		`tags/default$`), "Microsoft.Resources/tags/write"},
}

// checkCovered fails every request that armActions does not map, or whose action the role
// lacks.
func checkCovered(t *testing.T, what, role string, requests []string) {
	t.Helper()
	if len(requests) == 0 {
		t.Fatalf("%s: no requests recorded", what)
	}
	granted := loadAzureRole(t, role).Actions
	for _, req := range requests {
		method, uri, _ := strings.Cut(req, " ")
		path, _, _ := strings.Cut(uri, "?")
		action := ""
		for _, m := range armActions {
			if m.method == method && m.path.MatchString(path) {
				action = m.action
				break
			}
		}
		switch {
		case action == "":
			t.Errorf("%s calls %s %s, which armActions does not map to an action", what, method, path)
		case !slices.Contains(granted, action):
			t.Errorf("%s calls %s %s, which needs %s; %s lacks it", what, method, path, action, role)
		}
	}
}

const (
	coverSubscription = "00000000-0000-4000-8000-0000000000d1"
	coverGroup        = "rg-net"
)

func coverFake(t *testing.T) (*azurecloud.Provider, *azurefake.Cloud, inventory.Target) {
	t.Helper()
	cloud := azurefake.New()
	t.Cleanup(cloud.Close)
	cloud.AddSubscription(coverSubscription)
	cloud.AddVirtualNetwork(coverSubscription, coverGroup, "hub", "westeurope", []string{"10.0.0.0/16"}, nil)
	cloud.AddSubnet(coverSubscription, coverGroup, "hub", "apps", "10.0.1.0/24")
	p := azurecloud.NewProvider(azurecloud.Options{Credential: cloud.Credential(),
		ResourceManagerEndpoint: cloud.Endpoint(), Transport: cloud.Transport(), PollInterval: time.Millisecond})
	return p, cloud, inventory.Target{Provider: networkv1.ProviderAzure, Scope: "roles", Account: coverSubscription,
		Region: "westeurope"}
}

// Discovery, over the whole subscription and over resource groups, makes only calls whose
// action the reader role grants.
func TestAzureReaderRoleCoversDiscovery(t *testing.T) {
	p, cloud, target := coverFake(t)
	for _, groups := range [][]string{nil, {coverGroup}} {
		target.Azure = &networkv1.AzureScope{ResourceGroups: groups}
		snap, err := p.Discover(context.Background(), target)
		if err != nil || len(snap.Subnets) != 1 {
			t.Fatalf("discovery with resource groups %v: %v, %v", groups, snap, err)
		}
	}
	checkCovered(t, "discovery", "reader-role.json", cloud.Requests())
}

// The read before an import, of a network and of a subnet, is one call whose action the reader
// role grants: it is made as the read identity, so that Tag Contributor stays enough to write.
func TestAzureReaderRoleCoversTheReadBeforeAnImport(t *testing.T) {
	p, cloud, target := coverFake(t)
	vnet := strings.ToLower("/subscriptions/" + coverSubscription + "/resourceGroups/" + coverGroup +
		"/providers/Microsoft.Network/virtualNetworks/hub")
	for _, id := range []string{vnet, vnet + "/subnets/apps"} {
		if region, err := p.LocateImport(context.Background(), target, id); err != nil || region != "westeurope" {
			t.Fatalf("LocateImport(%s) = %q, %v", id, region, err)
		}
	}
	requests := cloud.Requests()
	if len(requests) != 2 {
		t.Errorf("two imports located with %d calls, want one each: %v", len(requests), requests)
	}
	checkCovered(t, "the read before an import", "reader-role.json", requests)
}

// Creating a subnet and writing ownership, of a network and of a subnet, make only calls whose
// action the writer role grants.
func TestAzureWriterRoleCoversTheWrites(t *testing.T) {
	p, cloud, target := coverFake(t)
	vnet := strings.ToLower("/subscriptions/" + coverSubscription + "/resourceGroups/" + coverGroup +
		"/providers/Microsoft.Network/virtualNetworks/hub")
	ctx := context.Background()
	if _, err := p.CreateSubnet(ctx, target, inventory.CreateSubnetRequest{NetworkID: vnet, CIDRBlock: "10.0.8.0/24",
		Name: "claimed", Tags: map[string]string{"hs-owner": "team-a"}}); err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}
	if err := p.WriteOwnership(ctx, target, vnet, map[string]string{"hs-env": "prod"}); err != nil {
		t.Fatalf("WriteOwnership of the network: %v", err)
	}
	if err := p.WriteOwnership(ctx, target, vnet+"/subnets/apps", map[string]string{"hs-owner": "team-b"}); err != nil {
		t.Fatalf("WriteOwnership of a subnet: %v", err)
	}
	checkCovered(t, "the writes", "writer-role.json", cloud.Requests())
}
