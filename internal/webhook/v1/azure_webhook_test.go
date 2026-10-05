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
	"context"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
)

// withStoredClaim reads like the API server would if it also stored the claim: one written
// while the webhooks were off, which no spec can create through an API server that serves them.
type withStoredClaim struct {
	client.Reader
	claim networkv1.SubnetClaim
}

func (r withStoredClaim) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := r.Reader.List(ctx, list, opts...); err != nil {
		return err
	}
	if claims, ok := list.(*networkv1.SubnetClaimList); ok {
		claims.Items = append(claims.Items, r.claim)
	}
	return nil
}

func azureScope(name, subscription, location string) *networkv1.NetworkScope {
	return &networkv1.NetworkScope{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: networkv1.NetworkScopeSpec{
			Provider:          networkv1.ProviderAzure,
			Accounts:          []networkv1.Account{{ID: subscription}},
			Regions:           []string{location},
			NamespaceSelector: &metav1.LabelSelector{},
		},
	}
}

var _ = Describe("Webhooks for an Azure scope", func() {
	var created []client.Object

	AfterEach(func() {
		for _, obj := range created {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
		created = nil
	})

	It("defaults the tag keys to the Azure ones", func() {
		obj := azureScope("azure-defaults", "00000000-0000-4000-8000-000000000001", "westeurope")
		obj.Spec.AutoImport = &networkv1.AutoImportPolicy{Mode: networkv1.AutoImportOff}
		mustCreate(obj)
		created = append(created, obj)

		Expect(obj.Spec.TagKeys).To(Equal(networkv1.TagKeys{Owner: "hs-owner", Env: "hs-env", Tier: "hs-tier"}))
		Expect(obj.Spec.AutoImport.ManagedTag).To(Equal("hs-managed"))
	})

	It("refuses a location name of another cloud", func() {
		expectDenied(azureScope("azure-aws-region", "00000000-0000-4000-8000-000000000002", "eu-central-1"),
			"not an Azure location name")
	})

	It("accepts an auto-import policy, since the provider writes ownership", func() {
		obj := azureScope("azure-auto-import", "00000000-0000-4000-8000-000000000003", "westeurope")
		obj.Spec.AutoImport = &networkv1.AutoImportPolicy{Mode: networkv1.AutoImportDryRun}
		mustCreate(obj)
		created = append(created, obj)
	})

	It("accepts a subscription ID in capitals and refuses what equals another entry without regard to case", func() {
		sub := "0000000A-0000-4000-8000-00000000000B"
		obj := azureScope("azure-capitals", sub, "westeurope")
		obj.Spec.Azure = &networkv1.AzureScope{ResourceGroups: []string{"RG-Net"}}
		mustCreate(obj)
		created = append(created, obj)
		Expect(obj.Spec.Accounts[0].ID).To(Equal(sub), "the spec is not rewritten; the operator lowercases the ID where it uses it")

		expectDenied(azureScope("azure-same-subscription", strings.ToLower(sub), "westeurope"),
			"is already discovered by NetworkScope")

		twice := azureScope("azure-twice", "00000000-0000-4000-8000-00000000000c", "westeurope")
		twice.Spec.Accounts = append(twice.Spec.Accounts, networkv1.Account{ID: "00000000-0000-4000-8000-00000000000C"})
		expectDenied(twice, "without regard to case")

		groups := azureScope("azure-groups", "00000000-0000-4000-8000-00000000000d", "westeurope")
		groups.Spec.Azure = &networkv1.AzureScope{ResourceGroups: []string{"rg-net", "RG-NET"}}
		expectDenied(groups, "without regard to case")

		names := azureScope("azure-names", "00000000-0000-4000-8000-00000000000e", "westeurope")
		names.Spec.RequiredSubnetTags = []string{"HS-Owner"}
		expectDenied(names, "same Azure tag name")
	})

	It("warns when an account reads and writes as the same identity", func() {
		obj := azureScope("azure-one-identity", "00000000-0000-4000-8000-000000000005", "westeurope")
		obj.Spec.Accounts[0].Azure = &networkv1.AzureAccount{ClientID: "7d1f00e2-0000-4000-8000-0000000000aa",
			WriteClientID: "7D1F00E2-0000-4000-8000-0000000000AA"}
		warnings, err := (&NetworkScopeValidator{Client: k8sClient, Providers: testProviders}).ValidateCreate(ctx, obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).To(ContainElement(ContainSubstring("reads and writes as the same identity")))

		obj.Spec.Accounts[0].Azure.WriteClientID = "7d1f00e2-0000-4000-8000-0000000000bb"
		warnings, err = (&NetworkScopeValidator{Client: k8sClient, Providers: testProviders}).ValidateCreate(ctx, obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).NotTo(ContainElement(ContainSubstring("same identity")))
	})

	It("checks claims and imports against the Azure rules", func() {
		sub := "00000000-0000-4000-8000-000000000004"
		obj := azureScope("azure-writes", sub, "westeurope")
		mustCreate(obj)
		created = append(created, obj)
		vnet := "/subscriptions/" + sub + "/resourcegroups/rg/providers/microsoft.network/virtualnetworks/hub"

		claim := func(name string, mutate func(*networkv1.SubnetClaim)) *networkv1.SubnetClaim {
			c := &networkv1.SubnetClaim{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: networkv1.SubnetClaimSpec{ScopeRef: obj.Name, Account: strings.ToUpper(sub), Region: "westeurope",
					NetworkID: vnet, PrefixLength: 24, Owner: "team-a", Mode: networkv1.ClaimModeCreate},
			}
			if mutate != nil {
				mutate(c)
			}
			return c
		}
		ok := claim("azure-claim", nil)
		mustCreate(ok)
		created = append(created, ok)

		expectDenied(claim("azure-claim-zones", func(c *networkv1.SubnetClaim) { c.Spec.Zones = []string{"1"} }),
			"Azure subnets are regional")
		expectDenied(claim("azure-claim-gateway", func(c *networkv1.SubnetClaim) { c.Spec.NamePrefix = "GatewaySubnet" }),
			"reserves for its own service")
		expectDenied(claim("azure-claim-pool", func(c *networkv1.SubnetClaim) {
			c.Spec.GCP = &networkv1.GCPClaimOptions{PoolCIDRs: []string{"10.0.0.0/16"}}
		}), "gcp is only valid for provider GCP")
		expectDenied(claim("azure-claim-long", func(c *networkv1.SubnetClaim) {
			c.Spec.Tags = map[string]string{"cost-center": strings.Repeat("x", 200)}
		}), "one tag value of at most 256 characters")

		imp := func(name, id string, tags map[string]string) *networkv1.ResourceImport {
			return &networkv1.ResourceImport{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: networkv1.ResourceImportSpec{ScopeRef: obj.Name, Account: sub, Region: "westeurope",
					ResourceID: id, Tags: tags},
			}
		}
		good := imp("azure-import", vnet+"/subnets/apps", map[string]string{"hs-owner": "team-a"})
		mustCreate(good)
		created = append(created, good)
		expectDenied(imp("azure-import-long", vnet+"/subnets/apps", map[string]string{
			"hs-owner": strings.Repeat("o", 200), "hs-env": strings.Repeat("e", 100)}),
			"one tag value of at most 256 characters")
		expectDenied(imp("azure-import-entry", vnet, map[string]string{"hs-subnet-apps": "hs-owner=x"}),
			"written by the operator only")
	})

	// The owner's decision for Azure identifiers: accept any case. ARM compares subscription and
	// resource IDs without regard to case and the portal shows them in camel case, so an ID
	// pasted from there is brought into the operator's spelling instead of being refused.
	Context("with IDs as the Azure portal spells them", func() {
		const sub = "0000000a-0000-4000-8000-0000000000f1"
		var (
			scopeName string
			vnet      string // the inventory's spelling
			portal    string // the portal's spelling of the same virtual network
		)

		BeforeEach(func() {
			scopeName = "azure-portal-ids"
			vnet = "/subscriptions/" + sub + "/resourcegroups/my-rg/providers/microsoft.network/virtualnetworks/hub"
			portal = "/subscriptions/" + strings.ToUpper(sub) +
				"/resourceGroups/My-RG/providers/Microsoft.Network/virtualNetworks/Hub"

			if err := k8sClient.Get(ctx, client.ObjectKey{Name: scopeName}, &networkv1.NetworkScope{}); err != nil {
				mustCreate(azureScope(scopeName, sub, "westeurope"))
			}
			// The virtual network as discovery mirrors it: named after its ID in lowercase.
			name := networkv1.ObjectName(vnet)
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, &networkv1.Network{}); err != nil {
				network := &networkv1.Network{
					ObjectMeta: metav1.ObjectMeta{Name: name},
					Spec: networkv1.NetworkSpec{Provider: networkv1.ProviderAzure, ID: vnet, Account: sub,
						Region: "westeurope"},
				}
				mustCreate(network)
				network.Status.CIDRBlocks = []string{"10.20.0.0/24"}
				Expect(k8sClient.Status().Update(ctx, network)).To(Succeed())
			}
			// Admitted only once the webhooks' cache holds the scope, so the specs below do not
			// race it: the mutating webhook leaves an object alone while it cannot read its scope.
			primer := &networkv1.SubnetClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "azure-portal-primer", Namespace: "default"},
				Spec: networkv1.SubnetClaimSpec{ScopeRef: scopeName, Account: sub, Region: "westeurope",
					NetworkID: vnet, PrefixLength: 28, Owner: "team-a", Mode: networkv1.ClaimModeAllocate},
			}
			mustCreate(primer)
			created = append(created, primer)
		})

		portalClaim := func(name string, prefixLength int32) *networkv1.SubnetClaim {
			return &networkv1.SubnetClaim{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: networkv1.SubnetClaimSpec{ScopeRef: scopeName, Account: strings.ToUpper(sub),
					Region: "westeurope", NetworkID: portal, PrefixLength: prefixLength, Owner: "team-a",
					Mode: networkv1.ClaimModeAllocate},
			}
		}
		portalImport := func(name, id string) *networkv1.ResourceImport {
			return &networkv1.ResourceImport{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: networkv1.ResourceImportSpec{ScopeRef: scopeName, Account: strings.ToUpper(sub),
					Region: "westeurope", ResourceID: id, Tags: map[string]string{"hs-owner": "team-a"}},
			}
		}

		It("accepts a claim's network ID and stores it, and the subscription, in lowercase", func() {
			obj := portalClaim("azure-portal-claim", 28)
			mustCreate(obj)
			created = append(created, obj)
			Expect(obj.Spec.NetworkID).To(Equal(vnet), "kubectl get shows the ID the operator uses")
			Expect(obj.Spec.Account).To(Equal(sub))

			By("finding the discovered network by it: a /20 does not fit in the hub's /24")
			expectDenied(portalClaim("azure-portal-claim-big", 20), "does not fit in network "+vnet)

			By("taking the portal's spelling again on an update for the same network, not a change of it")
			obj.Spec.NetworkID, obj.Spec.Account = portal, strings.ToUpper(sub)
			obj.Spec.Env = "prod"
			Expect(k8sClient.Update(ctx, obj)).To(Succeed())
			Expect(obj.Spec.NetworkID).To(Equal(vnet))
			Expect(obj.Spec.Account).To(Equal(sub))

			By("still refusing another network")
			obj.Spec.NetworkID = strings.Replace(portal, "Hub", "Spoke", 1)
			expectUpdateDenied(obj, "field is immutable")
		})

		It("accepts an import's resource ID, stores it in lowercase and labels the import by it", func() {
			obj := portalImport("azure-portal-import", portal+"/subnets/App")
			mustCreate(obj)
			created = append(created, obj)
			Expect(obj.Spec.ResourceID).To(Equal(vnet + "/subnets/app"))
			Expect(obj.Spec.Account).To(Equal(sub))
			Expect(obj.Labels).To(HaveKeyWithValue(networkv1.LabelAccount, sub),
				"a selector on the account label matches however the subscription ID was typed")
			Expect(obj.Labels).To(HaveKeyWithValue(networkv1.LabelResource, networkv1.ObjectName(vnet+"/subnets/app")),
				"the label the auto-import policy looks an existing import up by")

			By("taking the portal's spelling again on an update for the same resource")
			obj.Spec.ResourceID = portal + "/subnets/App"
			obj.Spec.Tags["hs-tier"] = "web"
			Expect(k8sClient.Update(ctx, obj)).To(Succeed())
			Expect(obj.Spec.ResourceID).To(Equal(vnet + "/subnets/app"))

			By("lowercasing an account label that names the subscription in capitals, and no other label")
			labelled := portalImport("azure-portal-import-labelled", portal)
			labelled.Labels = map[string]string{networkv1.LabelAccount: strings.ToUpper(sub),
				networkv1.LabelRegion: "somewhere-else"}
			mustCreate(labelled)
			created = append(created, labelled)
			Expect(labelled.Labels).To(HaveKeyWithValue(networkv1.LabelAccount, sub))
			Expect(labelled.Labels).To(HaveKeyWithValue(networkv1.LabelRegion, "somewhere-else"))
		})

		// The mutating webhook is failurePolicy Ignore: the validating one must not depend on it.
		It("checks an object the mutating webhook did not rewrite against the same network", func() {
			claims := &SubnetClaimValidator{Client: k8sClient, Providers: testProviders, WritesEnabled: true}
			big := portalClaim("azure-portal-unmutated", 20)
			_, err := claims.ValidateCreate(ctx, big)
			Expect(err).To(MatchError(ContainSubstring("does not fit in network " + vnet)))
			Expect(big.Spec.NetworkID).To(Equal(portal), "a validating webhook changes nothing")

			fits := portalClaim("azure-portal-unmutated", 28)
			warnings, err := claims.ValidateCreate(ctx, fits)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).NotTo(ContainElement(ContainSubstring("is not in the inventory")))

			stored := portalClaim("azure-portal-unmutated", 28)
			stored.Spec.NetworkID, stored.Spec.Account = vnet, sub
			_, err = claims.ValidateUpdate(ctx, fits, stored)
			Expect(err).NotTo(HaveOccurred(), "the object written while the webhook was off can be updated")

			By("counting the reservations of a stored claim that spells the network as the portal does")
			holder := portalClaim("azure-portal-holder", 24)
			holder.Status.Allocations = []networkv1.SubnetAllocation{{Name: holder.Name, CIDRBlock: "10.20.0.0/24",
				State: networkv1.AllocationPending}}
			claims.Client = withStoredClaim{Reader: k8sClient, claim: *holder}
			stored.Name = "azure-portal-late"
			_, err = claims.ValidateCreate(ctx, stored)
			Expect(err).To(MatchError(ContainSubstring("has no room")), "the whole /24 is reserved by the other claim")

			imports := &ResourceImportValidator{Client: k8sClient, Providers: testProviders, WritesEnabled: true}
			imp := portalImport("azure-portal-unmutated", portal)
			warnings, err = imports.ValidateCreate(ctx, imp)
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).NotTo(ContainElement(ContainSubstring("is not in the inventory")))
			Expect(imp.Spec.ResourceID).To(Equal(portal))

			storedImp := portalImport("azure-portal-unmutated", vnet)
			storedImp.Spec.Account = sub
			_, err = imports.ValidateUpdate(ctx, imp, storedImp)
			Expect(err).NotTo(HaveOccurred())
		})
	})

	// #116 and #117. An Azure resource ID names no location, and every Azure resource has one: an
	// import ends up with its virtual network's, filled in from the inventory where the operator
	// knows it. And the ID of a subnet names its virtual network, so the webhook can tell a subnet
	// the network does not have from a network nobody has discovered.
	Context("with a discovered virtual network", func() {
		const sub = "0000000b-0000-4000-8000-0000000000f2"
		const scopeName = "azure-import-region"
		vnet := "/subscriptions/" + sub + "/resourcegroups/rg-net/providers/microsoft.network/virtualnetworks/hub"
		unknown := strings.Replace(vnet, "/hub", "/undiscovered", 1)

		imp := func(name, id, region string) *networkv1.ResourceImport {
			return &networkv1.ResourceImport{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: networkv1.ResourceImportSpec{ScopeRef: scopeName, Account: sub, Region: region, ResourceID: id,
					Tags: map[string]string{"hs-owner": "team-a"}},
			}
		}

		BeforeEach(func() {
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: scopeName}, &networkv1.NetworkScope{}); err != nil {
				scope := azureScope(scopeName, sub, "westeurope")
				scope.Spec.Regions = []string{"westeurope", "northeurope"}
				mustCreate(scope)
			}
			// The inventory as discovery mirrors it: the virtual network and its one subnet.
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: networkv1.ObjectName(vnet)}, &networkv1.Network{}); err != nil {
				mustCreate(&networkv1.Network{
					ObjectMeta: metav1.ObjectMeta{Name: networkv1.ObjectName(vnet)},
					Spec: networkv1.NetworkSpec{Provider: networkv1.ProviderAzure, ID: vnet, Account: sub,
						Region: "westeurope"},
				})
				mustCreate(&networkv1.Subnet{
					ObjectMeta: metav1.ObjectMeta{Name: networkv1.ObjectName(vnet + "/subnets/apps")},
					Spec: networkv1.SubnetSpec{Provider: networkv1.ProviderAzure, ID: vnet + "/subnets/apps",
						NetworkID: vnet, Account: sub, Region: "westeurope"},
				})
			}
			// Admitted only once the webhooks' cache holds the scope and the network: until then
			// nothing fills the region in, and an import without one is refused.
			primer := imp("azure-region-primer", vnet, "")
			mustCreate(primer)
			created = append(created, primer)
		})

		It("fills an import's region in with the virtual network's location, and labels it", func() {
			for name, id := range map[string]string{
				"azure-region-vnet":   strings.ToUpper(vnet),
				"azure-region-subnet": vnet + "/subnets/apps",
				// A subnet that is not in the inventory yet still names its virtual network.
				"azure-region-new-subnet": vnet + "/subnets/created-a-moment-ago",
			} {
				obj := imp(name, id, "")
				mustCreate(obj)
				created = append(created, obj)
				Expect(obj.Spec.Region).To(Equal("westeurope"), name)
				Expect(obj.Labels).To(HaveKeyWithValue(networkv1.LabelRegion, "westeurope"), name)
			}

			By("leaving a region somebody wrote alone, for the validating webhook to judge")
			wrong := imp("azure-region-wrong", vnet, "northeurope")
			Expect((&ResourceImportDefaulter{Client: k8sClient, Providers: testProviders}).Default(ctx, wrong)).To(Succeed())
			Expect(wrong.Spec.Region).To(Equal("northeurope"))
		})

		It("refuses a region that is not the virtual network's", func() {
			expectDenied(imp("azure-region-contradicts", vnet, "northeurope"), "westeurope, not in "+sub+"/northeurope")
			expectDenied(imp("azure-region-contradicts-subnet", vnet+"/subnets/apps", "northeurope"),
				"westeurope, not in "+sub+"/northeurope")
			expectDenied(imp("azure-region-contradicts-new", vnet+"/subnets/created-a-moment-ago", "northeurope"),
				"spec.region: Invalid value")
		})

		It("requires the region of a virtual network it has not discovered", func() {
			expectDenied(imp("azure-region-unknown", unknown, ""), "spec.region: Required value")

			By("accepting it with one: the controller checks it against Azure")
			obj := imp("azure-region-unknown", unknown+"/subnets/apps", "northeurope")
			mustCreate(obj)
			created = append(created, obj)
			Expect(obj.Labels).To(HaveKeyWithValue(networkv1.LabelRegion, "northeurope"))
		})

		It("warns that a discovered virtual network has no such subnet", func() {
			imports := &ResourceImportValidator{Client: k8sClient, Providers: testProviders, WritesEnabled: true}
			specific := ContainElement(And(ContainSubstring(vnet+" is in the inventory"),
				ContainSubstring(`had no subnet "app"`), ContainSubstring("ResourceNotFound")))
			generic := ContainElement(ContainSubstring("is not in the inventory"))

			warnings, err := imports.ValidateCreate(ctx, imp("azure-subnet-misspelt", vnet+"/subnets/app", "westeurope"))
			Expect(err).NotTo(HaveOccurred(), "it may have been created since the last sync")
			Expect(warnings).To(specific)
			Expect(warnings).NotTo(generic)

			warnings, err = imports.ValidateCreate(ctx, imp("azure-subnet-known", vnet+"/subnets/apps", "westeurope"))
			Expect(err).NotTo(HaveOccurred())
			Expect(warnings).To(BeEmpty())

			By("keeping the general warning for a virtual network nobody has discovered")
			for _, id := range []string{unknown, unknown + "/subnets/app"} {
				warnings, err = imports.ValidateCreate(ctx, imp("azure-subnet-unknown", id, "westeurope"))
				Expect(err).NotTo(HaveOccurred())
				Expect(warnings).To(generic)
				Expect(warnings).NotTo(specific)
			}
		})

		It("lets the region of an import written while the webhooks were off be filled in, once", func() {
			imports := &ResourceImportValidator{Client: k8sClient, Providers: testProviders, WritesEnabled: true}
			stored := imp("azure-region-stored", vnet+"/subnets/apps", "")
			filled := imp("azure-region-stored", vnet+"/subnets/apps", "westeurope")
			_, err := imports.ValidateUpdate(ctx, stored, filled)
			Expect(err).NotTo(HaveOccurred())

			_, err = imports.ValidateUpdate(ctx, stored, imp("azure-region-stored", vnet+"/subnets/apps", "northeurope"))
			Expect(err).To(MatchError(ContainSubstring("westeurope, not in")), "filled in, but not with anything")
			_, err = imports.ValidateUpdate(ctx, stored, stored.DeepCopy())
			Expect(err).To(MatchError(ContainSubstring("spec.region: Required value")))
			_, err = imports.ValidateUpdate(ctx, filled, imp("azure-region-stored", vnet+"/subnets/apps", "northeurope"))
			Expect(err).To(MatchError(ContainSubstring("field is immutable")), "a region that is set does not change")
		})
	})

	// Only Azure IDs are case-insensitive. On AWS and GCP another case is another resource (or
	// none), so nothing is rewritten and a change of case is a change of the resource.
	It("leaves AWS and GCP IDs exactly as written, and treats another case as another resource", func() {
		aws := scope("case-aws", "200000000071", "eu-central-1")
		mustCreate(aws)
		created = append(created, aws)
		gcp := gcpScope("case-gcp", "case-project-1", "europe-west1")
		mustCreate(gcp)
		created = append(created, gcp)

		for _, tc := range []struct {
			scope, account, network, resource string
		}{
			{aws.Name, "200000000071", "vpc-0ABCdef", "subnet-0ABCdef"},
			{gcp.Name, "Case-Project-1", "projects/case-project-1/global/networks/Shared-VPC",
				"projects/case-project-1/regions/europe-west1/subnetworks/Apps"},
		} {
			claim := &networkv1.SubnetClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "case", Namespace: "default"},
				Spec: networkv1.SubnetClaimSpec{ScopeRef: tc.scope, Account: tc.account, Region: "r",
					NetworkID: tc.network, PrefixLength: 24, Owner: "team-a"},
			}
			Expect((&SubnetClaimDefaulter{Client: k8sClient}).Default(ctx, claim)).To(Succeed())
			Expect(claim.Spec.NetworkID).To(Equal(tc.network))
			Expect(claim.Spec.Account).To(Equal(tc.account))

			lower := claim.DeepCopy()
			lower.Spec.NetworkID = strings.ToLower(tc.network)
			_, err := (&SubnetClaimValidator{Client: k8sClient, Providers: testProviders}).ValidateUpdate(ctx, claim, lower)
			Expect(err).To(MatchError(ContainSubstring("spec.networkID: Invalid value")), tc.scope)
			Expect(err).To(MatchError(ContainSubstring("field is immutable")), tc.scope)

			imp := &networkv1.ResourceImport{
				ObjectMeta: metav1.ObjectMeta{Name: "case", Namespace: "default"},
				Spec: networkv1.ResourceImportSpec{ScopeRef: tc.scope, Account: tc.account, Region: "r",
					ResourceID: tc.resource},
			}
			Expect((&ResourceImportDefaulter{Client: k8sClient}).Default(ctx, imp)).To(Succeed())
			Expect(imp.Spec.ResourceID).To(Equal(tc.resource))
			Expect(imp.Spec.Account).To(Equal(tc.account))
			Expect(imp.Labels).To(HaveKeyWithValue(networkv1.LabelAccount, tc.account), "the account label is not lowercased")
			Expect(imp.Labels).To(HaveKeyWithValue(networkv1.LabelResource, networkv1.ObjectName(tc.resource)))

			lowerImp := imp.DeepCopy()
			lowerImp.Spec.ResourceID = strings.ToLower(tc.resource)
			_, err = (&ResourceImportValidator{Client: k8sClient, Providers: testProviders}).ValidateUpdate(ctx, imp, lowerImp)
			Expect(err).To(MatchError(ContainSubstring("spec.resourceID: Invalid value")), tc.scope)
		}

		By("not filling in a region on AWS or GCP, nor letting one be added later")
		for _, tc := range []struct{ scope, account, resource string }{
			{aws.Name, "200000000071", "subnet-0abc"},
			{gcp.Name, "case-project-1", "projects/case-project-1/global/networks/shared-vpc"},
		} {
			stored := &networkv1.ResourceImport{
				ObjectMeta: metav1.ObjectMeta{Name: "case", Namespace: "default"},
				Spec: networkv1.ResourceImportSpec{ScopeRef: tc.scope, Account: tc.account, ResourceID: tc.resource,
					Tags: map[string]string{"owner": "team-a"}},
			}
			Expect((&ResourceImportDefaulter{Client: k8sClient, Providers: testProviders}).Default(ctx, stored)).To(Succeed())
			Expect(stored.Spec.Region).To(BeEmpty(), tc.scope)
			Expect(stored.Labels).NotTo(HaveKey(networkv1.LabelRegion), tc.scope)
			later := stored.DeepCopy()
			later.Spec.Region = "r"
			_, err := (&ResourceImportValidator{Client: k8sClient, Providers: testProviders}).ValidateUpdate(ctx, stored, later)
			Expect(err).To(MatchError(ContainSubstring("spec.region: Invalid value")), tc.scope)
			Expect(err).To(MatchError(ContainSubstring("field is immutable")), tc.scope)
		}

		By("an AWS account label in capitals that only differs in case is left alone too")
		imp := &networkv1.ResourceImport{
			ObjectMeta: metav1.ObjectMeta{Name: "case", Namespace: "default",
				Labels: map[string]string{networkv1.LabelAccount: "ACCOUNT-A"}},
			Spec: networkv1.ResourceImportSpec{ScopeRef: aws.Name, Account: "account-a", ResourceID: "subnet-0abc"},
		}
		Expect((&ResourceImportDefaulter{Client: k8sClient}).Default(ctx, imp)).To(Succeed())
		Expect(imp.Labels).To(HaveKeyWithValue(networkv1.LabelAccount, "ACCOUNT-A"))
	})
})
