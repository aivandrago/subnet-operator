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
	"context"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/cloud/azure/azurefake"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/provider"
	"hypersurgery.dev/subnet-operator/internal/provider/providertest"
)

// The Azure provider runs the provider contract (#45) against azurefake: the whole provider,
// with the real Azure SDK clients talking HTTPS to the fake instead of Azure.
func TestProviderContract(t *testing.T) {
	providertest.Run(t, func(t *testing.T) providertest.Fixture {
		return newFakeFixture(t)
	})
}

const (
	contractSubscription = "00000000-0000-4000-8000-00000000c0de"
	contractLocation     = "westeurope"
	contractGroup        = "rg-contract"
)

type fakeFixture struct {
	cloud    *azurefake.Cloud
	provider *Provider
	next     int
}

// newFakeProvider wires a provider to a fresh fake with one subscription, without waits
// between retries.
func newFakeProvider(t *testing.T, opts Options) (*Provider, *azurefake.Cloud) {
	t.Helper()
	cloud := azurefake.New()
	t.Cleanup(cloud.Close)
	cloud.AddSubscription(contractSubscription)
	opts.Credential = cloud.Credential()
	opts.ResourceManagerEndpoint = cloud.Endpoint()
	opts.Transport = cloud.Transport()
	opts.AuthorityHost = cloud.AuthorityHost()
	opts.DisableInstanceDiscovery = true
	if opts.TenantID == "" {
		opts.TenantID = azurefake.DefaultTenant
	}
	if opts.FederatedTokenFile == "" {
		file, err := azurefake.WriteServiceAccountToken(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		opts.FederatedTokenFile = file
	}
	p := NewProvider(opts)
	p.discoverer.sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	return p, cloud
}

func newFakeFixture(t *testing.T) *fakeFixture {
	p, cloud := newFakeProvider(t, Options{})
	return &fakeFixture{cloud: cloud, provider: p}
}

func (f *fakeFixture) Provider() provider.Provider { return f.provider }

func (f *fakeFixture) Target() inventory.Target {
	return inventory.Target{Provider: networkv1.ProviderAzure, Scope: "contract", Account: contractSubscription,
		Region: contractLocation}
}

func (f *fakeFixture) Zone() string          { return "" }
func (f *fakeFixture) NetworkRegion() string { return contractLocation }
func (f *fakeFixture) NetworkCIDRs() bool    { return true }

func vnetID(name string) string {
	return canonicalID("/subscriptions/" + contractSubscription + "/resourceGroups/" + contractGroup +
		"/providers/Microsoft.Network/virtualNetworks/" + name)
}

func (f *fakeFixture) CreateNetwork(_ providertest.T, cidr string, tags map[string]string) string {
	f.next++
	name := fmt.Sprintf("vnet-%d", f.next)
	f.cloud.AddVirtualNetwork(contractSubscription, contractGroup, name, contractLocation, []string{cidr},
		maps.Clone(tags))
	return vnetID(name)
}

// CreateSubnet creates the subnet and, since an Azure subnet cannot carry tags, writes its tags
// as its entry on the virtual network, as somebody following the operator's convention would.
func (f *fakeFixture) CreateSubnet(_ providertest.T, networkID, cidr string, tags map[string]string) string {
	f.next++
	name := fmt.Sprintf("Subnet-%d", f.next)
	vnet := networkID[strings.LastIndex(networkID, "/")+1:]
	f.cloud.AddSubnet(contractSubscription, contractGroup, vnet, name, cidr)
	if len(tags) > 0 {
		f.cloud.SetTag(contractSubscription, contractGroup, vnet, SubnetEntryName(name), EncodeSubnetEntry(tags))
	}
	return networkID + "/subnets/" + strings.ToLower(name)
}

func (f *fakeFixture) MakeIPUsageUnknown(_ providertest.T, subnetID string) bool {
	found := false
	f.cloud.Update(func() {
		parts := strings.Split(subnetID, "/")
		v := f.cloud.VirtualNetwork(contractSubscription, contractGroup, parts[len(parts)-3])
		for _, s := range v.Subnets {
			if strings.EqualFold(s.Name, parts[len(parts)-1]) {
				s.UsageUnknown, found = true, true
			}
		}
	})
	return found
}

func (f *fakeFixture) Fail(_ providertest.T, failure providertest.Failure) (func(), bool) {
	switch failure {
	case providertest.Throttled:
		f.cloud.Fail(azurefake.Throttle)
	case providertest.AccessDenied:
		f.cloud.Fail(azurefake.Deny)
	default:
		return nil, false
	}
	return func() { f.cloud.Fail(azurefake.None) }, true
}

func (f *fakeFixture) InvalidTags() []map[string]string {
	return []map[string]string{
		{"hs/owner": "team-a"},
		{"": "empty name"},
		{"cost<center": "x"},
		{"a%b": "x"},
		{"hs-subnet-apps": "hs-owner=team-a"},
		{"hs-owner": "team-a", "HS-Owner": "team-b"},
		{strings.Repeat("k", maxTagNameLength+1): "long name"},
		{"hs-owner": strings.Repeat("v", maxTagValueLength+1)},
	}
}

// Identities of the contract subscription: user-assigned managed identities with a federated
// identity credential for the operator's service account, the reader holding a reader role and
// the writer a writer role.
const (
	contractReader = "11111111-aaaa-4000-8000-0000000000ad"
	contractWriter = "22222222-bbbb-4000-8000-0000000000e1"
)

var _ providertest.Identities = (*fakeFixture)(nil)

func (f *fakeFixture) AccountWithIdentities(_ providertest.T) networkv1.Account {
	f.cloud.AddIdentity(azurefake.DefaultTenant, contractReader, true)
	f.cloud.AddIdentity(azurefake.DefaultTenant, contractWriter, true)
	f.cloud.RestrictSubscription(contractSubscription, contractReader, contractWriter)
	f.cloud.RestrictSubscriptionWrites(contractSubscription, contractWriter)
	return networkv1.Account{ID: contractSubscription, Azure: &networkv1.AzureAccount{
		ClientID: contractReader, WriteClientID: contractWriter}}
}

func (f *fakeFixture) RevokeIdentities(_ providertest.T) func() {
	f.cloud.AddIdentity(azurefake.DefaultTenant, contractReader, false)
	f.cloud.AddIdentity(azurefake.DefaultTenant, contractWriter, false)
	return func() {
		f.cloud.AddIdentity(azurefake.DefaultTenant, contractReader, true)
		f.cloud.AddIdentity(azurefake.DefaultTenant, contractWriter, true)
	}
}
