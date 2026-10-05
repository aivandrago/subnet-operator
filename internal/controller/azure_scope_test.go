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

package controller

import (
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	azurecloud "hypersurgery.dev/subnet-operator/internal/cloud/azure"
	"hypersurgery.dev/subnet-operator/internal/cloud/azure/azurefake"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

const (
	azureSubscription = "5b3c2a10-0000-4000-8000-00000000a2e1"
	azureGroup        = "RG-Shared-Network"
	azureWestEurope   = "westeurope"
	azureNorthEurope  = "northeurope"
)

// azureMetric returns the value of a gauge's series for one Azure subnet, and whether it exists.
func azureMetric(metric, scope, subnetID string) (float64, bool) {
	GinkgoHelper()
	families, err := ctrlmetrics.Registry.Gather()
	Expect(err).NotTo(HaveOccurred())
	for _, f := range families {
		if f.GetName() != metric {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["scope"] == scope && labels["subnet_id"] == subnetID && labels["provider"] == "azure" {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

// An Azure scope reconciles end to end with discovery only: the real Azure provider, with the
// Azure SDK, against the in-repo fake of Azure Resource Manager, into Network and Subnet objects.
var _ = Describe("NetworkScope Controller with Azure", func() {
	var (
		scopeName  string
		cloud      *azurefake.Cloud
		reconciler *NetworkScopeReconciler
	)

	vnetID := strings.ToLower("/subscriptions/" + azureSubscription + "/resourceGroups/" + azureGroup +
		"/providers/Microsoft.Network/virtualNetworks/hub")
	appsID := vnetID + "/subnets/apps"
	dataID := vnetID + "/subnets/data"
	otherID := strings.ToLower("/subscriptions/" + azureSubscription + "/resourceGroups/" + azureGroup +
		"/providers/Microsoft.Network/virtualNetworks/north")

	reconcileScope := func() {
		GinkgoHelper()
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: scopeName}})
		Expect(err).NotTo(HaveOccurred())
	}

	// resync changes the spec, so the next reconcile is a full sync.
	resync := func(mutate func(*networkv1.NetworkScope)) {
		GinkgoHelper()
		scope := &networkv1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, scope)).To(Succeed())
		mutate(scope)
		Expect(k8sClient.Update(ctx, scope)).To(Succeed())
		reconcileScope()
	}

	getNetwork := func(id string) (*networkv1.Network, error) {
		n := &networkv1.Network{}
		return n, k8sClient.Get(ctx, types.NamespacedName{Name: networkv1.ObjectName(id)}, n)
	}
	getSubnet := func(id string) (*networkv1.Subnet, error) {
		s := &networkv1.Subnet{}
		return s, k8sClient.Get(ctx, types.NamespacedName{Name: networkv1.ObjectName(id)}, s)
	}

	BeforeEach(func() {
		scopeCounter++
		scopeName = fmt.Sprintf("azure-scope-%d", scopeCounter)
		cloud = azurefake.New()
		DeferCleanup(cloud.Close)
		cloud.AddSubscription(azureSubscription)
		cloud.AddVirtualNetwork(azureSubscription, azureGroup, "hub", azureWestEurope, []string{"10.10.0.0/16"},
			map[string]string{"hs-owner": "platform", "hs-env": "prod",
				azurecloud.SubnetEntryName("apps"): azurecloud.EncodeSubnetEntry(map[string]string{
					"hs-owner": "payments", "hs-tier": "private"})})
		apps := cloud.AddSubnet(azureSubscription, azureGroup, "hub", "apps", "10.10.1.0/24")
		cloud.AddSubnet(azureSubscription, azureGroup, "hub", "data", "10.10.2.0/24")
		cloud.AddVirtualNetwork(azureSubscription, azureGroup, "north", azureNorthEurope, []string{"10.20.0.0/16"}, nil)
		cloud.AddSubnet(azureSubscription, azureGroup, "north", "batch", "10.20.1.0/24")
		cloud.Update(func() { apps.Used = 6 })

		tokenFile, err := azurefake.WriteServiceAccountToken(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		p := azurecloud.NewProvider(azurecloud.Options{Credential: cloud.Credential(),
			ResourceManagerEndpoint: cloud.Endpoint(), Transport: cloud.Transport(),
			AuthorityHost: cloud.AuthorityHost(), DisableInstanceDiscovery: true,
			TenantID: azurefake.DefaultTenant, FederatedTokenFile: tokenFile})
		reconciler = &NetworkScopeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			Providers: provider.MustRegistry(p)}

		scope := &networkv1.NetworkScope{
			ObjectMeta: metav1.ObjectMeta{Name: scopeName},
			Spec: networkv1.NetworkScopeSpec{
				Provider:           networkv1.ProviderAzure,
				Accounts:           []networkv1.Account{{ID: azureSubscription}},
				Regions:            []string{azureWestEurope},
				RequiredSubnetTags: []string{"hs-tier"},
			},
		}
		Expect(k8sClient.Create(ctx, scope)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, scope))).To(Succeed())
			// No garbage collector in envtest: remove what the scope created.
			Expect(k8sClient.DeleteAllOf(ctx, &networkv1.Subnet{}, client.MatchingLabels{networkv1.LabelScope: scopeName})).To(Succeed())
			Expect(k8sClient.DeleteAllOf(ctx, &networkv1.Network{}, client.MatchingLabels{networkv1.LabelScope: scopeName})).To(Succeed())
		})
	})

	It("mirrors the location's virtual networks and their subnets, with subnet ownership from the network", func() {
		reconcileScope()

		n, err := getNetwork(vnetID)
		Expect(err).NotTo(HaveOccurred())
		Expect(n.Name).To(MatchRegexp(`^hub-[0-9a-f]{10}$`))
		Expect(n.Spec).To(Equal(networkv1.NetworkSpec{Provider: networkv1.ProviderAzure, ID: vnetID,
			Account: azureSubscription, Region: azureWestEurope}))
		Expect(n.Labels).To(HaveKeyWithValue(networkv1.LabelProvider, "azure"))
		Expect(n.Labels).To(HaveKeyWithValue(networkv1.LabelAccount, azureSubscription))
		Expect(n.Status.Name).To(Equal("hub"))
		Expect(n.Status.Owner).To(Equal("platform"), "the default Azure owner key is hs-owner")
		Expect(n.Status.CIDRBlocks).To(Equal([]string{"10.10.0.0/16"}))
		Expect(n.Status.Tags).NotTo(HaveKey(azurecloud.SubnetEntryName("apps")), "subnet entries are the subnets'")
		Expect(n.Status.Azure).To(Equal(&networkv1.AzureNetworkStatus{ResourceGroup: azureGroup, TagCount: 3,
			SubnetOwnershipEntries: 1}))
		Expect(n.Status.Subnets).To(Equal(int32(2)))
		Expect(n.Status.TotalIPs).To(HaveValue(Equal(int64(2 * 251))))
		Expect(n.Status.AvailableIPs).To(HaveValue(Equal(int64(245 + 251))))

		apps, err := getSubnet(appsID)
		Expect(err).NotTo(HaveOccurred())
		Expect(apps.Spec).To(Equal(networkv1.SubnetSpec{Provider: networkv1.ProviderAzure, ID: appsID, NetworkID: vnetID,
			Account: azureSubscription, Region: azureWestEurope}))
		Expect(apps.Labels).To(HaveKeyWithValue(networkv1.LabelNetwork, n.Name))
		Expect(apps.Status.Name).To(Equal("apps"))
		Expect(apps.Status.Zone).To(BeEmpty())
		Expect(apps.Status.Owner).To(Equal("payments"))
		Expect(apps.Status.Env).To(Equal("prod"), "what the entry does not say comes from the network")
		Expect(apps.Status.Tier).To(Equal("private"))
		Expect(apps.Status.OwnershipSource).To(Equal(networkv1.OwnershipSourceSubnet))
		Expect(apps.Status.AvailableIPs).To(HaveValue(Equal(int64(245))))
		Expect(apps.Status.Azure.IPUsageSource).To(Equal(azurecloud.UsageFromVirtualNetworkUsage))
		Expect(apps.Status.Azure.ResourceGroup).To(Equal(azureGroup))
		Expect(apps.Status.MissingTags).To(BeEmpty())

		data, err := getSubnet(dataID)
		Expect(err).NotTo(HaveOccurred())
		Expect(data.Status.Owner).To(Equal("platform"))
		Expect(data.Status.OwnershipSource).To(Equal(networkv1.OwnershipSourceNetwork))
		Expect(data.Status.MissingTags).To(Equal([]string{"hs-tier"}))

		_, err = getNetwork(otherID)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a virtual network of another location is not the scope's: %v", err)

		scope := &networkv1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, scope)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(scope.Status.Conditions, ConditionReady)).To(BeTrue(),
			"%v", scope.Status.Conditions)
		Expect(scope.Status.Networks).To(Equal(int32(1)))
		Expect(scope.Status.Subnets).To(Equal(int32(2)))
		Expect(scope.Status.Capabilities).To(Equal([]networkv1.Capability{networkv1.CapabilityCreateSubnet,
			networkv1.CapabilityIPUsage}))
		Expect(scope.Status.Ownership).To(Equal(&networkv1.Ownership{Networks: networkv1.OwnershipResourceTags,
			Subnets: networkv1.OwnershipParentNetworkTags}))
		Expect(scope.Status.Targets).To(HaveLen(1))

		free, ok := azureMetric("hs_subnet_available_ips", scopeName, appsID)
		Expect(ok).To(BeTrue(), "the subnet's series carries provider=azure")
		Expect(free).To(Equal(245.0))
	})

	It("follows a new location, a changed entry and a deleted virtual network", func() {
		reconcileScope()

		resync(func(s *networkv1.NetworkScope) { s.Spec.Regions = []string{azureWestEurope, azureNorthEurope} })
		_, err := getNetwork(otherID)
		Expect(err).NotTo(HaveOccurred())

		cloud.SetTag(azureSubscription, azureGroup, "hub", azurecloud.SubnetEntryName("data"),
			azurecloud.EncodeSubnetEntry(map[string]string{"hs-owner": "analytics", "hs-tier": "db"}))
		cloud.RemoveVirtualNetwork(azureSubscription, azureGroup, "north")
		resync(func(s *networkv1.NetworkScope) { s.Spec.ResyncInterval = &metav1.Duration{Duration: 2 * time.Minute} })

		data, err := getSubnet(dataID)
		Expect(err).NotTo(HaveOccurred())
		Expect(data.Status.Owner).To(Equal("analytics"))
		Expect(data.Status.OwnershipSource).To(Equal(networkv1.OwnershipSourceSubnet))
		_, err = getNetwork(otherID)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the deleted virtual network is gone: %v", err)
	})

	It("reads a subscription ID in capitals, tag names in any case, and only the scope's resource groups", func() {
		cloud.Update(func() {
			hub := cloud.VirtualNetwork(azureSubscription, azureGroup, "hub")
			delete(hub.Tags, "hs-owner")
			delete(hub.Tags, azurecloud.SubnetEntryName("apps"))
			hub.Tags["HS-Owner"] = "platform"
			hub.Tags["HS-SUBNET-Apps"] = "Hs-Owner=payments;HS-TIER=private"
		})
		cloud.AddVirtualNetwork(azureSubscription, "rg-other", "spoke", azureWestEurope, []string{"10.30.0.0/16"},
			map[string]string{"hs-owner": "other"})
		cloud.AddSubnet(azureSubscription, "rg-other", "spoke", "web", "10.30.1.0/24")
		spokeID := strings.ToLower("/subscriptions/" + azureSubscription +
			"/resourceGroups/rg-other/providers/Microsoft.Network/virtualNetworks/spoke")
		upper := strings.ToUpper(azureSubscription)
		resync(func(s *networkv1.NetworkScope) { s.Spec.Accounts[0].ID = upper })

		n, err := getNetwork(vnetID)
		Expect(err).NotTo(HaveOccurred())
		Expect(n.Spec.Account).To(Equal(azureSubscription), "the operator uses the subscription ID in lowercase")
		Expect(n.Labels).To(HaveKeyWithValue(networkv1.LabelAccount, azureSubscription))
		Expect(n.Status.Owner).To(Equal("platform"), "HS-Owner is hs-owner")
		Expect(n.Status.Tags).To(HaveKeyWithValue("hs-owner", "platform"))
		apps, err := getSubnet(appsID)
		Expect(err).NotTo(HaveOccurred())
		Expect(apps.Spec.Account).To(Equal(azureSubscription))
		Expect(apps.Status.OwnershipSource).To(Equal(networkv1.OwnershipSourceSubnet), "HS-SUBNET-Apps is the entry of apps")
		Expect(apps.Status.Owner).To(Equal("payments"))
		Expect(apps.Status.Tier).To(Equal("private"))
		Expect(apps.Status.MissingTags).To(BeEmpty(), "HS-TIER is the required hs-tier")
		_, err = getNetwork(spokeID)
		Expect(err).NotTo(HaveOccurred(), "the whole subscription is discovered without resource groups")

		scope := &networkv1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, scope)).To(Succeed())
		Expect(scope.Status.Targets).To(HaveLen(1))
		Expect(scope.Status.Targets[0].Account).To(Equal(azureSubscription))

		resync(func(s *networkv1.NetworkScope) {
			s.Spec.Azure = &networkv1.AzureScope{ResourceGroups: []string{strings.ToUpper(azureGroup)}}
		})
		_, err = getNetwork(vnetID)
		Expect(err).NotTo(HaveOccurred(), "the resource group is matched without regard to case")
		_, err = getNetwork(spokeID)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "a virtual network outside the scope's resource groups: %v", err)
	})

	It("keeps the inventory while the subscription cannot be read, and says why", func() {
		reconcileScope()
		cloud.Fail(azurefake.Deny)
		resync(func(s *networkv1.NetworkScope) { s.Spec.ResyncInterval = &metav1.Duration{Duration: 2 * time.Minute} })
		_, err := getNetwork(vnetID)
		Expect(err).NotTo(HaveOccurred(), "an unreadable subscription is not an empty one")
		scope := &networkv1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, scope)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(scope.Status.Conditions, ConditionReady)).To(BeFalse())
		Expect(scope.Status.Targets).To(HaveLen(1))
		Expect(scope.Status.Targets[0].Error).To(ContainSubstring("AuthorizationFailed"))
	})

	It("reads a subscription as its identity, and says what to set up when it cannot", func() {
		const reader = "7d1f00e2-0000-4000-8000-0000000000aa"
		cloud.AddIdentity(azurefake.DefaultTenant, reader, false) // no federated credential yet
		cloud.RestrictSubscription(azureSubscription, reader)
		resync(func(s *networkv1.NetworkScope) {
			s.Spec.Accounts[0].Azure = &networkv1.AzureAccount{ClientID: strings.ToUpper(reader)}
		})

		scope := &networkv1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, scope)).To(Succeed())
		ready := meta.FindStatusCondition(scope.Status.Conditions, ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("SyncFailed"), "a refused token is a failed target, not throttling")
		Expect(scope.Status.Targets).To(HaveLen(1))
		Expect(scope.Status.Targets[0].Error).To(ContainSubstring("client " + reader))
		Expect(scope.Status.Targets[0].Error).To(ContainSubstring("federated identity credential"))
		Expect(scope.Status.Targets[0].Error).To(ContainSubstring(azurefake.ServiceAccountSubject))
		_, err := getSubnet(appsID)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "nothing is read without the identity: %v", err)
		Expect(cloud.RequestsBy(azurefake.Operator)).To(BeEmpty(), "no fallback to the operator's own identity")

		// Federated, the next sync reads the subscription as the identity.
		cloud.AddIdentity(azurefake.DefaultTenant, reader, true)
		resync(func(s *networkv1.NetworkScope) { s.Spec.ResyncInterval = &metav1.Duration{Duration: 2 * time.Minute} })
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, scope)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(scope.Status.Conditions, ConditionReady)).To(BeTrue(), "%v", scope.Status.Conditions)
		_, err = getSubnet(appsID)
		Expect(err).NotTo(HaveOccurred())
		Expect(cloud.RequestsBy(reader)).NotTo(BeEmpty())
		Expect(cloud.RequestsBy(azurefake.Operator)).To(BeEmpty())
	})

	It("refuses Azure scopes the schema can tell are wrong", func() {
		base := func() *networkv1.NetworkScope {
			return &networkv1.NetworkScope{ObjectMeta: metav1.ObjectMeta{Name: scopeName + "-bad"},
				Spec: networkv1.NetworkScopeSpec{Provider: networkv1.ProviderAzure,
					Accounts: []networkv1.Account{{ID: azureSubscription}}, Regions: []string{azureWestEurope}}}
		}
		for what, mutate := range map[string]func(*networkv1.NetworkScope){
			"an AWS account ID": func(s *networkv1.NetworkScope) { s.Spec.Accounts[0].ID = "111111111111" },
			"a resource group name with a slash": func(s *networkv1.NetworkScope) {
				s.Spec.Azure = &networkv1.AzureScope{ResourceGroups: []string{"rg/net"}}
			},
			"a resource group name ending in a period": func(s *networkv1.NetworkScope) {
				s.Spec.Azure = &networkv1.AzureScope{ResourceGroups: []string{"rg-net."}}
			},
			"a resource group listed twice": func(s *networkv1.NetworkScope) {
				s.Spec.Azure = &networkv1.AzureScope{ResourceGroups: []string{"rg", "rg"}}
			},
			"an aws member":           func(s *networkv1.NetworkScope) { s.Spec.AWS = &networkv1.AWSScope{} },
			"a gcp member":            func(s *networkv1.NetworkScope) { s.Spec.GCP = &networkv1.GCPScope{TagParent: "organizations/1"} },
			"an account's gcp member": func(s *networkv1.NetworkScope) { s.Spec.Accounts[0].GCP = &networkv1.GCPAccount{} },
			"a client ID that is no UUID": func(s *networkv1.NetworkScope) {
				s.Spec.Accounts[0].Azure = &networkv1.AzureAccount{ClientID: "subnet-reader"}
			},
			"a tenant domain name": func(s *networkv1.NetworkScope) {
				s.Spec.Accounts[0].Azure = &networkv1.AzureAccount{ClientID: "7D1F00E2-0000-4000-8000-0000000000AA",
					TenantID: "contoso.onmicrosoft.com"}
			},
			"a tenant without a client ID": func(s *networkv1.NetworkScope) {
				s.Spec.Accounts[0].Azure = &networkv1.AzureAccount{TenantID: "9a8b7c6d-0000-4000-8000-0000000000c3"}
			},
		} {
			s := base()
			mutate(s)
			err := k8sClient.Create(ctx, s)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%s: %v", what, err)
		}
		upper := base()
		upper.Spec.Accounts[0].ID = strings.ToUpper(azureSubscription)
		upper.Spec.Azure = &networkv1.AzureScope{ResourceGroups: []string{"RG-Net_(eu).1", "Netzwerk-Größe"}}
		Expect(k8sClient.Create(ctx, upper)).To(Succeed(), "a subscription ID in capitals and resource group names")
		Expect(k8sClient.Delete(ctx, upper)).To(Succeed())
		aws := base()
		aws.Spec.Provider, aws.Spec.Accounts[0].ID = networkv1.ProviderAWS, "111111111111"
		aws.Spec.Azure = &networkv1.AzureScope{}
		err := k8sClient.Create(ctx, aws)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "an azure member on an AWS scope: %v", err)
		awsAccount := base()
		awsAccount.Spec.Provider = networkv1.ProviderAWS
		awsAccount.Spec.Accounts[0] = networkv1.Account{ID: "111111111111",
			Azure: &networkv1.AzureAccount{ClientID: "7d1f00e2-0000-4000-8000-0000000000aa"}}
		err = k8sClient.Create(ctx, awsAccount)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "an account's azure member on an AWS scope: %v", err)
		Expect(err.Error()).To(ContainSubstring("accounts[].azure is only valid for provider Azure"))

		good := base()
		good.Spec.Accounts[0].Azure = &networkv1.AzureAccount{ClientID: "7D1F00E2-0000-4000-8000-0000000000AA",
			WriteClientID: "7d1f00e2-0000-4000-8000-0000000000bb", TenantID: "9A8B7C6D-0000-4000-8000-0000000000C3"}
		Expect(k8sClient.Create(ctx, good)).To(Succeed(), "identities on an Azure scope are accepted, in either case")
		Expect(k8sClient.Delete(ctx, good)).To(Succeed())
	})
})
