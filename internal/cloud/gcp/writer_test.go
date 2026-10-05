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

package gcp

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/cloud/gcp/gcpfake"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

const writeNetwork = "projects/" + contractProject + "/global/networks/shared"

// newWriteFixture is a provider and a fake with one network, the operator's tag keys under
// the organization and one owner value.
func newWriteFixture(t *testing.T, createValues bool) (*Provider, *gcpfake.Cloud, inventory.Target) {
	t.Helper()
	p, cloud := newFakeProvider(t, Options{})
	addOperatorTagKeys(cloud)
	cloud.AddTagValue("organizations/"+contractOrg, contractOrg, "hs-owner", "team-a")
	cloud.AddNetwork(contractProject, "shared")
	target := inventory.Target{Provider: networkv1.ProviderGCP, Scope: "gcp", Account: contractProject,
		Region: contractRegion, GCP: &networkv1.GCPScope{TagParent: "organizations/" + contractOrg,
			CreateTagValues: createValues}}
	return p, cloud, target
}

func bindingsOf(s *gcpfake.Subnetwork) []string {
	out := make([]string, 0, len(s.Bindings))
	for _, b := range s.Bindings {
		out = append(out, b.Key+"="+b.Value)
	}
	slices.Sort(out)
	return out
}

func TestCreateSubnetBindsTagsAtCreation(t *testing.T) {
	p, cloud, target := newWriteFixture(t, false)
	id, err := p.CreateSubnet(context.Background(), target, inventory.CreateSubnetRequest{
		NetworkID: writeNetwork, CIDRBlock: "10.20.0.0/24", Name: "payments",
		Tags: map[string]string{"hs-owner": "team-a"},
		GCP:  &networkv1.GCPClaimOptions{PrivateIPGoogleAccess: true},
	})
	if err != nil {
		t.Fatalf("CreateSubnet: %v", err)
	}
	if want := "projects/" + contractProject + "/regions/" + contractRegion + "/subnetworks/payments"; id != want {
		t.Errorf("id = %q, want %q", id, want)
	}
	s := cloud.Subnetwork(contractProject, contractRegion, "payments")
	if s == nil || s.CIDR != "10.20.0.0/24" || !s.PrivateIPGoogleAccess {
		t.Fatalf("created subnetwork = %+v", s)
	}
	if got := bindingsOf(s); !slices.Equal(got, []string{"hs-owner=team-a"}) {
		t.Errorf("bindings = %v, want hs-owner=team-a bound at creation", got)
	}
	// No tagBindings.create: the tags came with the insert.
	for _, r := range cloud.Requests() {
		if strings.Contains(r, "tagBindings") {
			t.Errorf("a separate binding call was made: %s", r)
		}
	}

	// The same name, network and range again is the subnetwork an earlier attempt created.
	again, err := p.CreateSubnet(context.Background(), target, inventory.CreateSubnetRequest{
		NetworkID: writeNetwork, CIDRBlock: "10.20.0.0/24", Name: "payments"})
	if err != nil || again != id {
		t.Errorf("creating it again = %q, %v; want %q", again, err, id)
	}
	// The same name with another range is somebody else's.
	if _, err := p.CreateSubnet(context.Background(), target, inventory.CreateSubnetRequest{
		NetworkID: writeNetwork, CIDRBlock: "10.21.0.0/24", Name: "payments"}); err == nil ||
		errors.Is(err, inventory.ErrCIDRConflict) || !strings.Contains(err.Error(), "exists already") {
		t.Errorf("a taken name with another range: %v", err)
	}
	// An overlapping range, in any region of the network, is a CIDR conflict.
	cloud.AddSubnetwork(contractProject, "us-central1", "shared", "far-away", "10.30.0.0/24")
	if _, err := p.CreateSubnet(context.Background(), target, inventory.CreateSubnetRequest{
		NetworkID: writeNetwork, CIDRBlock: "10.30.0.0/25", Name: "clash"}); !errors.Is(err, inventory.ErrCIDRConflict) {
		t.Errorf("an overlapping range: %v, want ErrCIDRConflict", err)
	}
}

func TestCreateSubnetRefusesWhatGCPCannotDo(t *testing.T) {
	p, _, target := newWriteFixture(t, false)
	for name, req := range map[string]inventory.CreateSubnetRequest{
		"a zone":                    {NetworkID: writeNetwork, CIDRBlock: "10.20.0.0/24", Name: "a", Zone: "europe-west1-b"},
		"an invalid name":           {NetworkID: writeNetwork, CIDRBlock: "10.20.0.0/24", Name: "Payments_1"},
		"another project's network": {NetworkID: "projects/other-project/global/networks/shared", CIDRBlock: "10.20.0.0/24", Name: "a"},
		"an AWS VPC":                {NetworkID: "vpc-0abc", CIDRBlock: "10.20.0.0/24", Name: "a"},
	} {
		if _, err := p.CreateSubnet(context.Background(), target, req); err == nil {
			t.Errorf("CreateSubnet with %s succeeded", name)
		}
	}
}

func TestMissingTagValuesAreRefusedUnlessTheScopeAllowsCreatingThem(t *testing.T) {
	p, cloud, target := newWriteFixture(t, false)
	req := inventory.CreateSubnetRequest{NetworkID: writeNetwork, CIDRBlock: "10.20.0.0/24", Name: "payments",
		Tags: map[string]string{"hs-owner": "team-b"}}
	_, err := p.CreateSubnet(context.Background(), target, req)
	if !errors.Is(err, inventory.ErrTagValueMissing) || !strings.Contains(err.Error(), contractOrg+"/hs-owner/team-b") {
		t.Fatalf("a missing value: %v, want ErrTagValueMissing naming it", err)
	}
	if cloud.Subnetwork(contractProject, contractRegion, "payments") != nil {
		t.Error("the subnetwork was created although its tags could not be bound")
	}

	_, err = p.CreateSubnet(context.Background(), target, inventory.CreateSubnetRequest{NetworkID: writeNetwork,
		CIDRBlock: "10.20.0.0/24", Name: "payments", Tags: map[string]string{"cost-center": "cc-1"}})
	if !errors.Is(err, inventory.ErrTagKeyMissing) || !strings.Contains(err.Error(), "cost-center") {
		t.Errorf("a missing key: %v, want ErrTagKeyMissing naming it", err)
	}

	target.GCP.CreateTagValues = true
	if _, err := p.CreateSubnet(context.Background(), target, req); err != nil {
		t.Fatalf("with createTagValues: %v", err)
	}
	if !cloud.TagValueExists(contractOrg, "hs-owner", "team-b") {
		t.Error("the value was not created")
	}
	// Keys are never created, not even with createTagValues.
	_, err = p.CreateSubnet(context.Background(), target, inventory.CreateSubnetRequest{NetworkID: writeNetwork,
		CIDRBlock: "10.21.0.0/24", Name: "other", Tags: map[string]string{"cost-center": "cc-1"}})
	if !errors.Is(err, inventory.ErrTagKeyMissing) {
		t.Errorf("a missing key with createTagValues: %v, want ErrTagKeyMissing", err)
	}
}

func TestAKeyAtItsValueLimitIsReported(t *testing.T) {
	p, cloud, target := newWriteFixture(t, true)
	cloud.MaxValuesPerKey = 1 // hs-owner holds team-a already
	cloud.AddSubnetwork(contractProject, contractRegion, "shared", "apps", "10.10.0.0/24")
	err := p.WriteOwnership(context.Background(), target,
		"projects/"+contractProject+"/regions/"+contractRegion+"/subnetworks/apps", map[string]string{"hs-owner": "team-b"})
	if !errors.Is(err, inventory.ErrTagValueLimit) {
		t.Errorf("a key at its limit: %v, want ErrTagValueLimit", err)
	}
}

func TestWriteOwnershipBindsOnlyWhatIsMissing(t *testing.T) {
	p, cloud, target := newWriteFixture(t, true)
	s := cloud.AddSubnetwork(contractProject, contractRegion, "shared", "apps", "10.10.0.0/24",
		orgTag("hs-owner", "team-a"))
	id := "projects/" + contractProject + "/regions/" + contractRegion + "/subnetworks/apps"
	cloud.Requests()
	if err := p.WriteOwnership(context.Background(), target, id,
		map[string]string{"hs-owner": "team-a", "hs-env": "prod"}); err != nil {
		t.Fatalf("WriteOwnership: %v", err)
	}
	var binds []string
	for _, r := range cloud.Requests() {
		if strings.HasPrefix(r, "POST") && strings.Contains(r, "tagBindings") {
			binds = append(binds, r)
		}
	}
	if len(binds) != 1 || !strings.Contains(binds[0], "/rm/"+contractRegion+"/") {
		t.Errorf("binding calls = %v, want one, at the region's endpoint", binds)
	}
	if got := bindingsOf(s); !slices.Equal(got, []string{"hs-env=prod", "hs-owner=team-a"}) {
		t.Errorf("bindings = %v", got)
	}

	err := p.WriteOwnership(context.Background(), target, id, map[string]string{"hs-owner": "team-b", "hs-tier": "db"})
	if !errors.Is(err, inventory.ErrOwnershipConflict) || !strings.Contains(err.Error(), `hs-owner is "team-a"`) {
		t.Errorf("a changed owner: %v, want ErrOwnershipConflict naming the bound value", err)
	}
	if got := bindingsOf(s); !slices.Equal(got, []string{"hs-env=prod", "hs-owner=team-a"}) {
		t.Errorf("a refused write bound something: %v", got)
	}
}
