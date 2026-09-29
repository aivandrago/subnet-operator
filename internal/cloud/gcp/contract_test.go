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
	"fmt"
	"strings"
	"testing"
	"time"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/cloud/gcp/gcpfake"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/provider"
	"hypersurgery.dev/subnet-operator/internal/provider/providertest"
)

// The GCP provider runs the provider contract (#45) against gcpfake: the whole provider, with
// the real Google client libraries talking HTTP to the fake instead of Google.
func TestProviderContract(t *testing.T) {
	providertest.Run(t, func(t *testing.T) providertest.Fixture {
		return newFakeFixture(t)
	})
}

const (
	contractProject = "contract-project"
	contractNumber  = "424242424242"
	contractRegion  = "europe-west1"
	contractOrg     = "123456789012"
)

// orgTag is a binding of a key the scope's tag parent (the organization) owns.
func orgTag(key, value string) gcpfake.Binding {
	return gcpfake.Binding{Parent: "organizations/" + contractOrg, Namespace: contractOrg, Key: key, Value: value}
}

type fakeFixture struct {
	cloud    *gcpfake.Cloud
	provider *Provider
	next     int
}

// newFakeProvider wires a provider to a fresh fake with one project, without waits between
// retries.
func newFakeProvider(t *testing.T, opts Options) (*Provider, *gcpfake.Cloud) {
	t.Helper()
	cloud := gcpfake.New()
	t.Cleanup(cloud.Close)
	cloud.AddProject(contractProject, contractNumber)
	opts.ClientOptions = cloud.ClientOptions()
	opts.IAMCredentialsEndpoint = cloud.IAMCredentialsEndpoint()
	opts.ComputeEndpoint = cloud.ComputeEndpoint()
	opts.ResourceManagerEndpoint = cloud.ResourceManagerEndpoint
	p := NewProvider(opts)
	p.discoverer.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return p, cloud
}

func newFakeFixture(t *testing.T) *fakeFixture {
	p, cloud := newFakeProvider(t, Options{})
	addOperatorTagKeys(cloud)
	return &fakeFixture{cloud: cloud, provider: p}
}

// addOperatorTagKeys creates the tag keys the operator writes under the contract's
// organization, as the platform team would before installing it.
func addOperatorTagKeys(cloud *gcpfake.Cloud) {
	k := networkv1.OperatorTagKeysFor(networkv1.ProviderGCP)
	for _, key := range []string{k.Owner, k.Env, k.Tier, k.Managed, k.ManagedBy, k.Claim} {
		cloud.AddTagKey("organizations/"+contractOrg, contractOrg, key)
	}
}

func (f *fakeFixture) Provider() provider.Provider { return f.provider }

func (f *fakeFixture) Target() inventory.Target {
	return inventory.Target{Provider: networkv1.ProviderGCP, Scope: "contract", Account: contractProject,
		Region: contractRegion, GCP: &networkv1.GCPScope{TagParent: "organizations/" + contractOrg, CreateTagValues: true}}
}

func (f *fakeFixture) Zone() string          { return "" }
func (f *fakeFixture) NetworkRegion() string { return "" }
func (f *fakeFixture) NetworkCIDRs() bool    { return false }

func bindings(tags map[string]string) []gcpfake.Binding {
	out := make([]gcpfake.Binding, 0, len(tags))
	for k, v := range tags {
		out = append(out, orgTag(k, v))
	}
	return out
}

func (f *fakeFixture) CreateNetwork(_ providertest.T, _ string, tags map[string]string) string {
	f.next++
	name := fmt.Sprintf("net-%d", f.next)
	f.cloud.AddNetwork(contractProject, name, bindings(tags)...)
	return "projects/" + contractProject + "/global/networks/" + name
}

func (f *fakeFixture) CreateSubnet(_ providertest.T, networkID, cidr string, tags map[string]string) string {
	f.next++
	name := fmt.Sprintf("subnet-%d", f.next)
	network := networkID[strings.LastIndex(networkID, "/")+1:]
	f.cloud.AddSubnetwork(contractProject, contractRegion, network, name, cidr, bindings(tags)...)
	return "projects/" + contractProject + "/regions/" + contractRegion + "/subnetworks/" + name
}

func (f *fakeFixture) MakeIPUsageUnknown(_ providertest.T, subnetID string) bool {
	name := subnetID[strings.LastIndex(subnetID, "/")+1:]
	found := false
	f.cloud.Update(func() {
		for _, s := range f.cloud.Subnetworks(contractProject) {
			if s.Name == name {
				s.UsageUnknown, found = true, true
			}
		}
	})
	return found
}

func (f *fakeFixture) Fail(_ providertest.T, failure providertest.Failure) (func(), bool) {
	switch failure {
	case providertest.Throttled:
		f.cloud.Fail(gcpfake.Throttle)
	case providertest.AccessDenied:
		f.cloud.Fail(gcpfake.Deny)
	default:
		return nil, false
	}
	return func() { f.cloud.Fail(gcpfake.None) }, true
}

func (f *fakeFixture) InvalidTags() []map[string]string {
	return []map[string]string{
		{"hs/owner": "team-a"},
		{"": "empty key"},
		{"hs-owner": ""},
		{"hs-owner": "team/a"},
		{"-hs-owner": "team-a"},
		{strings.Repeat("k", maxShortNameLength+1): "long key"},
		{"hs-owner": strings.Repeat("v", maxShortNameLength+1)},
	}
}

// Service accounts of the contract project, which the operator's own identity may
// impersonate.
const (
	contractReader = "subnet-reader@contract-project.iam.gserviceaccount.com"
	contractWriter = "subnet-writer@contract-project.iam.gserviceaccount.com"
)

var _ providertest.Identities = (*fakeFixture)(nil)

func (f *fakeFixture) AccountWithIdentities(_ providertest.T) networkv1.Account {
	f.cloud.AddServiceAccount(contractReader, gcpfake.Operator)
	f.cloud.AddServiceAccount(contractWriter, gcpfake.Operator)
	f.cloud.RestrictProject(contractProject, contractReader, contractWriter)
	f.cloud.RestrictProjectWrites(contractProject, contractWriter)
	return networkv1.Account{ID: contractProject, GCP: &networkv1.GCPAccount{
		ServiceAccount: contractReader, WriteServiceAccount: contractWriter}}
}

func (f *fakeFixture) RevokeIdentities(_ providertest.T) func() {
	f.cloud.AddServiceAccount(contractReader)
	f.cloud.AddServiceAccount(contractWriter)
	return func() {
		f.cloud.AddServiceAccount(contractReader, gcpfake.Operator)
		f.cloud.AddServiceAccount(contractWriter, gcpfake.Operator)
	}
}
