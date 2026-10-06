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
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/audit"
	azurecloud "hypersurgery.dev/subnet-operator/internal/cloud/azure"
	"hypersurgery.dev/subnet-operator/internal/cloud/azure/azurefake"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

// Writes on an Azure scope (#53), end to end: the claim, import and scope controllers with the
// real Azure provider against the in-repo fake of Azure Resource Manager.
var _ = Describe("Azure writes", func() {
	var (
		scopeName string
		cloud     *azurefake.Cloud
		scopes    *NetworkScopeReconciler
		claims    *SubnetClaimReconciler
		imports   *ResourceImportReconciler
	)
	vnetID := strings.ToLower("/subscriptions/" + azureSubscription + "/resourceGroups/" + azureGroup +
		"/providers/Microsoft.Network/virtualNetworks/hub")
	appsID, dataID := vnetID+"/subnets/apps", vnetID+"/subnets/data"
	hubTags := func() map[string]string { return cloud.Tags(azureSubscription, azureGroup, "hub") }
	entryOf := func(subnet string) map[string]string {
		GinkgoHelper()
		v, ok := hubTags()[azurecloud.SubnetEntryName(subnet)]
		Expect(ok).To(BeTrue(), "hub has no entry for %s: %v", subnet, hubTags())
		return azurecloud.DecodeSubnetEntry(v)
	}

	reconcileScope := func() {
		GinkgoHelper()
		_, err := scopes.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: scopeName}})
		Expect(err).NotTo(HaveOccurred())
	}
	updateScope := func(mutate func(*networkv1.NetworkScope)) {
		GinkgoHelper()
		scope := &networkv1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, scope)).To(Succeed())
		mutate(scope)
		Expect(k8sClient.Update(ctx, scope)).To(Succeed())
	}
	// rediscover changes the scope's spec, so its next reconcile is a full sync.
	rediscover := func() {
		GinkgoHelper()
		updateScope(func(s *networkv1.NetworkScope) {
			interval := time.Minute
			if s.Spec.ResyncInterval != nil {
				interval = s.Spec.ResyncInterval.Duration + time.Minute
			}
			s.Spec.ResyncInterval = &metav1.Duration{Duration: interval}
		})
		reconcileScope()
	}
	reconcileClaim := func(claim *networkv1.SubnetClaim) *networkv1.SubnetClaim {
		GinkgoHelper()
		_, err := claims.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(claim)})
		Expect(err).NotTo(HaveOccurred())
		got := &networkv1.SubnetClaim{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), got)).To(Succeed())
		return got
	}
	reconcileImport := func(imp *networkv1.ResourceImport) *networkv1.ResourceImport {
		GinkgoHelper()
		_, err := imports.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(imp)})
		Expect(err).NotTo(HaveOccurred())
		got := &networkv1.ResourceImport{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(imp), got)).To(Succeed())
		return got
	}
	create := func(obj client.Object) {
		GinkgoHelper()
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed()) })
	}
	readyReason := func(conds []metav1.Condition) string {
		GinkgoHelper()
		ready := meta.FindStatusCondition(conds, ConditionReady)
		Expect(ready).NotTo(BeNil())
		return ready.Reason
	}

	BeforeEach(func() {
		scopeCounter++
		scopeName = fmt.Sprintf("azure-writes-%d", scopeCounter)
		cloud = azurefake.New()
		DeferCleanup(cloud.Close)
		cloud.AddSubscription(azureSubscription)
		cloud.AddVirtualNetwork(azureSubscription, azureGroup, "hub", azureWestEurope, []string{"10.10.0.0/16"},
			map[string]string{"hs-owner": "platform", "hs-env": "prod",
				azurecloud.SubnetEntryName("apps"): azurecloud.EncodeSubnetEntry(map[string]string{"hs-owner": "payments"})})
		cloud.AddSubnet(azureSubscription, azureGroup, "hub", "apps", "10.10.0.0/24")
		cloud.AddSubnet(azureSubscription, azureGroup, "hub", "data", "10.10.1.0/24")

		tokenFile, err := azurefake.WriteServiceAccountToken(GinkgoT().TempDir())
		Expect(err).NotTo(HaveOccurred())
		p := azurecloud.NewProvider(azurecloud.Options{Credential: cloud.Credential(),
			ResourceManagerEndpoint: cloud.Endpoint(), Transport: cloud.Transport(), PollInterval: time.Millisecond,
			AuthorityHost: cloud.AuthorityHost(), DisableInstanceDiscovery: true,
			TenantID: azurefake.DefaultTenant, FederatedTokenFile: tokenFile})
		providers := provider.MustRegistry(p)
		scopes = &NetworkScopeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			Providers: providers}
		claims = &SubnetClaimReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			Providers: providers, WritesEnabled: true}
		imports = &ResourceImportReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			Providers: providers, WritesEnabled: true}

		scope := &networkv1.NetworkScope{
			ObjectMeta: metav1.ObjectMeta{Name: scopeName},
			Spec: networkv1.NetworkScopeSpec{
				Provider:          networkv1.ProviderAzure,
				Accounts:          []networkv1.Account{{ID: strings.ToUpper(azureSubscription)}},
				Regions:           []string{azureWestEurope},
				NamespaceSelector: &metav1.LabelSelector{},
			},
		}
		Expect(k8sClient.Create(ctx, scope)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, scope))).To(Succeed())
			Expect(k8sClient.DeleteAllOf(ctx, &networkv1.Subnet{}, client.MatchingLabels{networkv1.LabelScope: scopeName})).To(Succeed())
			Expect(k8sClient.DeleteAllOf(ctx, &networkv1.Network{}, client.MatchingLabels{networkv1.LabelScope: scopeName})).To(Succeed())
			Expect(k8sClient.DeleteAllOf(ctx, &networkv1.ResourceImport{}, client.InNamespace("default"),
				client.MatchingLabels{networkv1.LabelScope: scopeName})).To(Succeed())
		})
		reconcileScope()
	})

	newClaim := func(name string) *networkv1.SubnetClaim {
		return &networkv1.SubnetClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			// The subscription in capitals, as somebody may copy it from the portal.
			Spec: networkv1.SubnetClaimSpec{ScopeRef: scopeName, Account: strings.ToUpper(azureSubscription),
				Region: azureWestEurope, NetworkID: vnetID, PrefixLength: 24, Mode: networkv1.ClaimModeCreate,
				Owner: "team-a", Env: "prod", Tags: map[string]string{"cost-center": "42"}},
		}
	}
	newImport := func(name, id string, tags map[string]string) *networkv1.ResourceImport {
		return &networkv1.ResourceImport{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
			Spec: networkv1.ResourceImportSpec{ScopeRef: scopeName, Account: azureSubscription, Region: azureWestEurope,
				ResourceID: id, Tags: tags},
		}
	}

	It("creates the subnet of a Create-mode claim after its entry, and adopts it again by the claim tag", func() {
		claim := newClaim(fmt.Sprintf("orders-%d", scopeCounter))
		create(claim)

		got := reconcileClaim(claim)
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, ConditionReady)).To(BeTrue(), "%v", got.Status.Conditions)
		Expect(got.Status.Allocations).To(HaveLen(1))
		a := got.Status.Allocations[0]
		Expect(a.Name).To(Equal(claim.Name), "a zone-less allocation is keyed by the subnet's name")
		Expect(a.Zone).To(BeEmpty())
		Expect(a.CIDRBlock).To(Equal("10.10.2.0/24"), "10.10.0.0/24 and 10.10.1.0/24 are taken")
		Expect(a.SubnetID).To(Equal(vnetID + "/subnets/" + claim.Name))
		Expect(a.State).To(Equal(networkv1.AllocationCreated))

		s := cloud.Subnet(azureSubscription, azureGroup, "hub", claim.Name)
		Expect(s).NotTo(BeNil())
		Expect(s.AddressPrefixes).To(Equal([]string{"10.10.2.0/24"}))
		Expect(entryOf(claim.Name)).To(Equal(map[string]string{"hs-owner": "team-a", "cost-center": "42",
			"hs-managed-by": "subnet-operator", "hs-claim": "default/" + claim.Name}),
			"hs-env=prod is the network's, and inherited")
		Expect(cloud.UnconditionalPuts()).To(BeZero(), "every PUT says If-None-Match: *")

		rediscover()
		sub := &networkv1.Subnet{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: networkv1.ObjectName(a.SubnetID)}, sub)).To(Succeed())
		Expect(sub.Status.Owner).To(Equal("team-a"))
		Expect(sub.Status.Env).To(Equal("prod"))
		Expect(sub.Status.OwnershipSource).To(Equal(networkv1.OwnershipSourceSubnet))

		// Lost status: the claim finds its subnet by the claim tag in the entry.
		lost := &networkv1.SubnetClaim{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), lost)).To(Succeed())
		lost.Status.Allocations = nil
		Expect(k8sClient.Status().Update(ctx, lost)).To(Succeed())
		again := reconcileClaim(claim)
		Expect(meta.IsStatusConditionTrue(again.Status.Conditions, ConditionReady)).To(BeTrue(), "%v", again.Status.Conditions)
		Expect(again.Status.Allocations).To(HaveLen(1))
		Expect(again.Status.Allocations[0].SubnetID).To(Equal(a.SubnetID))
		Expect(again.Status.Allocations[0].CIDRBlock).To(Equal("10.10.2.0/24"))

		// Another claim for the same name gets its own range and an error, never the subnet.
		second := newClaim(fmt.Sprintf("orders-copy-%d", scopeCounter))
		second.Spec.NamePrefix = claim.Name
		create(second)
		refused := reconcileClaim(second)
		Expect(meta.IsStatusConditionTrue(refused.Status.Conditions, ConditionReady)).To(BeFalse())
		Expect(refused.Status.Allocations).To(HaveLen(1))
		Expect(refused.Status.Allocations[0].SubnetID).To(BeEmpty())
		Expect(refused.Status.Allocations[0].Error).To(ContainSubstring("exists already"))
		Expect(entryOf(claim.Name)).To(HaveKeyWithValue("hs-claim", "default/"+claim.Name))
	})

	// The mutating webhook stores Azure IDs in lowercase; these objects were written while it was
	// not running (no webhook serves this suite), in the spelling the portal shows.
	Context("with IDs the webhook did not rewrite", func() {
		portalID := "/subscriptions/" + strings.ToUpper(azureSubscription) + "/resourceGroups/" +
			strings.ToUpper(azureGroup) + "/providers/Microsoft.Network/virtualNetworks/Hub"

		It("finds the network of a claim by its ID in any case, and creates the subnet there", func() {
			claim := newClaim(fmt.Sprintf("portal-%d", scopeCounter))
			claim.Spec.NetworkID = portalID
			create(claim)

			got := reconcileClaim(claim)
			Expect(meta.IsStatusConditionTrue(got.Status.Conditions, ConditionReady)).To(BeTrue(), "%v", got.Status.Conditions)
			Expect(got.Spec.NetworkID).To(Equal(portalID), "the controller does not rewrite a spec")
			Expect(got.Status.Allocations).To(HaveLen(1))
			Expect(got.Status.Allocations[0].CIDRBlock).To(Equal("10.10.2.0/24"))
			Expect(got.Status.Allocations[0].SubnetID).To(Equal(vnetID+"/subnets/"+claim.Name),
				"the subnet's ID is the inventory's, in lowercase")
			Expect(cloud.Subnet(azureSubscription, azureGroup, "hub", claim.Name)).NotTo(BeNil())
		})

		It("reserves from one pool for claims that spell the network differently", func() {
			first := newClaim(fmt.Sprintf("portal-first-%d", scopeCounter))
			first.Spec.NetworkID, first.Spec.Mode = portalID, networkv1.ClaimModeAllocate
			create(first)
			Expect(reconcileClaim(first).Status.Allocations[0].CIDRBlock).To(Equal("10.10.2.0/24"))

			second := newClaim(fmt.Sprintf("portal-second-%d", scopeCounter))
			second.Spec.Mode = networkv1.ClaimModeAllocate
			create(second)
			Expect(reconcileClaim(second).Status.Allocations[0].CIDRBlock).To(Equal("10.10.3.0/24"),
				"the first claim's reservation is in the same virtual network")

			third := newClaim(fmt.Sprintf("portal-third-%d", scopeCounter))
			third.Spec.NetworkID, third.Spec.Mode = strings.ToUpper(vnetID), networkv1.ClaimModeAllocate
			create(third)
			Expect(reconcileClaim(third).Status.Allocations[0].CIDRBlock).To(Equal("10.10.4.0/24"))
		})

		It("applies an import whose resource ID is in any case", func() {
			imp := newImport(fmt.Sprintf("portal-data-%d", scopeCounter), portalID+"/subnets/Data",
				map[string]string{"hs-tier": "db"})
			imp.Spec.Account = strings.ToUpper(azureSubscription)
			create(imp)
			got := reconcileImport(imp)
			Expect(got.Status.State).To(Equal(networkv1.ImportApplied), "%v", got.Status.Conditions)
			Expect(got.Spec.ResourceID).To(Equal(portalID+"/subnets/Data"), "the controller does not rewrite a spec")
			Expect(entryOf("data")).To(Equal(map[string]string{"hs-tier": "db"}))
			ready := meta.FindStatusCondition(got.Status.Conditions, ConditionReady)
			Expect(ready.Message).To(ContainSubstring(dataID), "the status names the resource as the inventory does")
		})
	})

	It("imports by adding to a subnet's entry, and refuses to replace a value, inherited ones included", func() {
		imp := newImport(fmt.Sprintf("data-%d", scopeCounter), dataID, map[string]string{"hs-owner": "platform",
			"hs-tier": "db"})
		create(imp)
		got := reconcileImport(imp)
		Expect(got.Status.State).To(Equal(networkv1.ImportApplied), "%v", got.Status.Conditions)
		Expect(entryOf("data")).To(Equal(map[string]string{"hs-tier": "db"}), "hs-owner=platform is inherited")

		takeover := newImport(fmt.Sprintf("data-owner-%d", scopeCounter), dataID, map[string]string{"hs-owner": "billing"})
		create(takeover)
		got = reconcileImport(takeover)
		Expect(got.Status.State).To(Equal(networkv1.ImportFailed))
		Expect(readyReason(got.Status.Conditions)).To(Equal("TagValueConflict"))
		Expect(got.Status.Error).To(ContainSubstring("inherited from the virtual network"))

		apps := newImport(fmt.Sprintf("apps-%d", scopeCounter), appsID, map[string]string{"hs-owner": "billing",
			"hs-tier": "web"})
		create(apps)
		got = reconcileImport(apps)
		Expect(readyReason(got.Status.Conditions)).To(Equal("TagValueConflict"))
		Expect(entryOf("apps")).To(Equal(map[string]string{"hs-owner": "payments"}), "nothing of a refused import")

		network := newImport(fmt.Sprintf("hub-%d", scopeCounter), vnetID, map[string]string{"hs-tier": "shared"})
		create(network)
		got = reconcileImport(network)
		Expect(got.Status.State).To(Equal(networkv1.ImportApplied), "%v", got.Status.Conditions)
		Expect(hubTags()).To(HaveKeyWithValue("hs-tier", "shared"))
	})

	It("counts the ownership entries whose subnet does not exist, in the status and as a metric", func() {
		orphaned := func() (int32, float64) {
			GinkgoHelper()
			rediscover()
			n := &networkv1.Network{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: networkv1.ObjectName(vnetID)}, n)).To(Succeed())
			value, ok := azureNetworkMetric("hs_network_orphaned_ownership_entries", scopeName, vnetID)
			Expect(ok).To(BeTrue(), "the metric is exported for an Azure network")
			Expect(n.Status.Azure.SubnetOwnershipEntries-n.Status.Azure.OrphanedSubnetOwnershipEntries).
				To(BeNumerically(">=", 1), "the entry of apps names a subnet that exists")
			return n.Status.Azure.OrphanedSubnetOwnershipEntries, value
		}
		status, metric := orphaned()
		Expect(status).To(BeZero(), "every entry names a subnet")
		Expect(metric).To(BeZero())

		// A subnet deleted since, an entry in capitals of a subnet that exists, and a tag that
		// is no entry.
		cloud.SetTag(azureSubscription, azureGroup, "hub", "hs-subnet-deleted", "hs-owner=team-gone")
		cloud.SetTag(azureSubscription, azureGroup, "hub", "HS-SUBNET-DATA", "hs-tier=db")
		cloud.SetTag(azureSubscription, azureGroup, "hub", "hs-subnets", "apps,data")
		status, metric = orphaned()
		Expect(status).To(Equal(int32(1)))
		Expect(metric).To(Equal(float64(1)))

		// The subnet comes to exist: its entry is orphaned no more.
		cloud.AddSubnet(azureSubscription, azureGroup, "hub", "Deleted", "10.10.200.0/24")
		status, metric = orphaned()
		Expect(status).To(BeZero())
		Expect(metric).To(BeZero())
	})

	It("refuses claims and imports once the virtual network has no tag left, and says so", func() {
		cloud.Update(func() {
			hub := cloud.VirtualNetwork(azureSubscription, azureGroup, "hub")
			for i := len(hub.Tags); i < azurefake.MaxTags; i++ {
				hub.Tags[fmt.Sprintf("filler-%02d", i)] = "x"
			}
		})
		rediscover()
		n := &networkv1.Network{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: networkv1.ObjectName(vnetID)}, n)).To(Succeed())
		Expect(n.Status.Azure.TagCount).To(Equal(int32(azurefake.MaxTags)))
		tags, ok := azureNetworkMetric("hs_network_tags", scopeName, vnetID)
		Expect(ok).To(BeTrue())
		Expect(tags).To(Equal(float64(azurefake.MaxTags)))

		claim := newClaim(fmt.Sprintf("full-%d", scopeCounter))
		create(claim)
		got := reconcileClaim(claim)
		Expect(readyReason(got.Status.Conditions)).To(Equal("TagBudgetExceeded"))
		Expect(got.Status.Allocations[0].Error).To(ContainSubstring("limit of 50"))
		Expect(cloud.Subnet(azureSubscription, azureGroup, "hub", claim.Name)).To(BeNil(), "nothing is created half-way")

		imp := newImport(fmt.Sprintf("data-full-%d", scopeCounter), dataID, map[string]string{"hs-tier": "db"})
		create(imp)
		gotImp := reconcileImport(imp)
		Expect(gotImp.Status.State).To(Equal(networkv1.ImportFailed))
		Expect(readyReason(gotImp.Status.Conditions)).To(Equal("TagBudgetExceeded"))

		// The subnet with an entry already still takes more, in the same tag.
		apps := newImport(fmt.Sprintf("apps-full-%d", scopeCounter), appsID, map[string]string{"hs-tier": "web"})
		create(apps)
		Expect(reconcileImport(apps).Status.State).To(Equal(networkv1.ImportApplied))
	})

	It("writes the tags the provider checks a claim against", func() {
		claim := newClaim("any")
		claim.Spec.Tier = "db"
		claim.Spec.Tags["hs-claim"] = "somebody else's"
		target := inventory.Target{Provider: networkv1.ProviderAzure}
		Expect(subnetTags(claim, networkv1.ProviderAzure, bindsClaimTag(target), "")).To(
			Equal(azurecloud.ClaimTags(claim)))
	})

	It("refuses a claim whose entry would not fit in one tag value", func() {
		claim := newClaim(fmt.Sprintf("long-%d", scopeCounter))
		claim.Spec.Tags = map[string]string{"cost-center": strings.Repeat("c", 200)}
		create(claim)
		got := reconcileClaim(claim)
		Expect(readyReason(got.Status.Conditions)).To(Equal("OwnershipEntryTooLong"))
		Expect(got.Status.Allocations).To(BeEmpty(), "refused before anything is reserved")
	})

	// #116: the Tags API writes hs-subnet-<name> on the virtual network whether or not the
	// subnet exists, so the import reads the virtual network first.
	Context("an import of something that does not exist", func() {
		var sink *bytes.Buffer
		lateID := vnetID + "/subnets/late"
		lateTags := map[string]string{"hs-tier": "db", "cost-center": "7"}

		BeforeEach(func() {
			sink = &bytes.Buffer{}
			imports.Audit = audit.NewWriter(sink)
		})

		It("refuses a subnet the virtual network does not have, writes nothing, and applies once it exists", func() {
			before := hubTags()
			imp := newImport(fmt.Sprintf("late-%d", scopeCounter), lateID, lateTags)
			create(imp)

			res, err := imports.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(imp)})
			Expect(err).NotTo(HaveOccurred())
			Expect(res.RequeueAfter).To(Equal(importRetryInterval), "retried, so a subnet created later is imported")
			got := &networkv1.ResourceImport{}
			Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(imp), got)).To(Succeed())
			Expect(got.Status.State).To(Equal(networkv1.ImportFailed))
			Expect(readyReason(got.Status.Conditions)).To(Equal(ReasonResourceNotFound))
			Expect(got.Status.Error).To(And(ContainSubstring(`has no subnet "late"`), ContainSubstring("it has apps, data")))
			Expect(got.Status.AppliedTags).To(BeEmpty())
			Expect(hubTags()).To(Equal(before), "no entry for a subnet that does not exist")
			lines := auditLines(sink)
			Expect(lines).To(HaveLen(1))
			Expect(lines[0].Result).To(Equal(audit.ResultFailed))
			Expect(lines[0].Error).To(ContainSubstring(`has no subnet "late"`))

			By("creating the subnet: the same import applies at its next attempt")
			cloud.AddSubnet(azureSubscription, azureGroup, "hub", "late", "10.10.5.0/24")
			got = reconcileImport(imp)
			Expect(got.Status.State).To(Equal(networkv1.ImportApplied), "%v", got.Status.Conditions)
			Expect(got.Status.Error).To(BeEmpty())
			Expect(entryOf("late")).To(Equal(lateTags))

			// A full sync: the scope's reconcile inside its resync interval does not discover.
			rediscover()
			sub := &networkv1.Subnet{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: networkv1.ObjectName(lateID)}, sub)).To(Succeed())
			Expect(sub.Status.Tier).To(Equal("db"))
			Expect(sub.Status.OwnershipSource).To(Equal(networkv1.OwnershipSourceSubnet))
		})

		It("refuses a virtual network that does not exist", func() {
			gone := strings.Replace(vnetID, "/hub", "/hubb", 1)
			for _, id := range []string{gone, gone + "/subnets/apps"} {
				imp := newImport(fmt.Sprintf("gone-%d-%d", scopeCounter, len(id)), id, map[string]string{"hs-tier": "db"})
				create(imp)
				got := reconcileImport(imp)
				Expect(got.Status.State).To(Equal(networkv1.ImportFailed))
				Expect(readyReason(got.Status.Conditions)).To(Equal(ReasonResourceNotFound))
				Expect(got.Status.Error).To(ContainSubstring("virtualnetworks/hubb was not found in Azure"))
			}
		})

		It("says so in a dry run, which writes nothing either way", func() {
			before := hubTags()
			missing := newImport(fmt.Sprintf("dry-late-%d", scopeCounter), lateID, lateTags)
			missing.Spec.DryRun = true
			create(missing)
			got := reconcileImport(missing)
			Expect(got.Status.State).To(Equal(networkv1.ImportFailed), "a dry run reports what the import would do")
			Expect(readyReason(got.Status.Conditions)).To(Equal(ReasonResourceNotFound))
			ready := meta.FindStatusCondition(got.Status.Conditions, ConditionReady)
			Expect(ready.Message).To(And(HavePrefix("dry run: "), ContainSubstring(`has no subnet "late"`)))

			existing := newImport(fmt.Sprintf("dry-data-%d", scopeCounter), dataID, lateTags)
			existing.Spec.DryRun = true
			create(existing)
			got = reconcileImport(existing)
			Expect(got.Status.State).To(Equal(networkv1.ImportSkipped), "%v", got.Status.Conditions)
			Expect(readyReason(got.Status.Conditions)).To(Equal("DryRun"))
			Expect(hubTags()).To(Equal(before))

			By("a dry run needs no write identity and no --enable-writes to find out")
			imports.WritesEnabled = false
			readOnly := newImport(fmt.Sprintf("dry-ro-%d", scopeCounter), lateID, lateTags)
			readOnly.Spec.DryRun = true
			create(readOnly)
			Expect(readyReason(reconcileImport(readOnly).Status.Conditions)).To(Equal(ReasonResourceNotFound))
		})

		It("reads the virtual network once per attempt, and not at all once applied or while it waits", func() {
			reads := func() int {
				n := 0
				for _, r := range cloud.Requests() {
					if strings.HasPrefix(r, "GET ") && strings.Contains(r, "/virtualNetworks/hub?") {
						n++
					}
				}
				return n
			}
			imp := newImport(fmt.Sprintf("once-%d", scopeCounter), dataID, map[string]string{"hs-tier": "db"})
			create(imp)

			imports.WritesEnabled = false
			_ = cloud.Requests()
			Expect(readyReason(reconcileImport(imp).Status.Conditions)).To(Equal("WritesDisabled"))
			Expect(cloud.Requests()).To(BeEmpty(), "an import that waits for --enable-writes costs no ARM call")

			imports.WritesEnabled = true
			Expect(reconcileImport(imp).Status.State).To(Equal(networkv1.ImportApplied))
			Expect(reads()).To(Equal(1))

			Expect(reconcileImport(imp).Status.State).To(Equal(networkv1.ImportApplied))
			Expect(cloud.Requests()).To(BeEmpty(), "an applied import calls ARM no more")
		})
	})

	// The read before an import is the read identity's: Tag Contributor, which is all a write
	// identity needs for imports, cannot read a virtual network.
	It("reads an import's virtual network as the read identity and writes as the write identity", func() {
		const reader, writer = "7d1f00e2-0000-4000-8000-0000000000aa", "7d1f00e2-0000-4000-8000-0000000000bb"
		cloud.AddIdentity(azurefake.DefaultTenant, reader, true)
		cloud.AddIdentity(azurefake.DefaultTenant, writer, true)
		cloud.RestrictSubscription(azureSubscription, reader, writer)
		cloud.RestrictSubscriptionWrites(azureSubscription, writer)
		updateScope(func(s *networkv1.NetworkScope) {
			s.Spec.Accounts[0].Azure = &networkv1.AzureAccount{ClientID: reader, WriteClientID: writer}
		})
		own := len(cloud.RequestsBy(azurefake.Operator))

		imp := newImport(fmt.Sprintf("identities-%d", scopeCounter), dataID, map[string]string{"hs-tier": "db"})
		create(imp)
		got := reconcileImport(imp)
		Expect(got.Status.State).To(Equal(networkv1.ImportApplied), "%v", got.Status.Conditions)

		network := ContainSubstring("/virtualNetworks/hub?")
		Expect(cloud.RequestsBy(reader)).To(ConsistOf(And(HavePrefix("GET "), network)), "one read, of the virtual network")
		Expect(cloud.RequestsBy(writer)).To(And(Not(ContainElement(network)), ContainElement(HavePrefix("PATCH "))),
			"the write identity uses the Tags API alone")
		Expect(cloud.RequestsBy(azurefake.Operator)).To(HaveLen(own), "the operator's own identity is not used instead")
	})

	// #117: a resource ID names no location, and every Azure resource has one.
	Context("an import and its region", func() {
		var (
			sink     *bytes.Buffer
			resynced []inventory.TargetKey
		)

		BeforeEach(func() {
			sink = &bytes.Buffer{}
			resynced = nil
			imports.Audit = audit.NewWriter(sink)
			imports.Notify = func(_ context.Context, changed []inventory.TargetKey) error {
				resynced = append(resynced, changed...)
				return nil
			}
		})

		It("takes the virtual network's location for an import written without one", func() {
			// Only an import created while the webhooks were off has no region.
			imp := newImport(fmt.Sprintf("nowhere-%d", scopeCounter), dataID, map[string]string{"hs-tier": "db"})
			imp.Spec.Region = ""
			create(imp)
			got := reconcileImport(imp)
			Expect(got.Status.State).To(Equal(networkv1.ImportApplied), "%v", got.Status.Conditions)
			Expect(got.Spec.Region).To(BeEmpty(), "the controller does not rewrite a spec")
			Expect(entryOf("data")).To(Equal(map[string]string{"hs-tier": "db"}))

			lines := auditLines(sink)
			Expect(lines).To(HaveLen(1))
			Expect(lines[0].Result).To(Equal(audit.ResultApplied))
			Expect(lines[0].Region).To(Equal(azureWestEurope), "the audit line says where the resource is")
			Expect(resynced).To(ConsistOf(inventory.TargetKey{Account: azureSubscription, Region: azureWestEurope}),
				"the target that is resynced is the virtual network's")

			By("a dry run without a region reports it too")
			dry := newImport(fmt.Sprintf("nowhere-dry-%d", scopeCounter), appsID, map[string]string{"hs-tier": "web"})
			dry.Spec.Region, dry.Spec.DryRun = "", true
			create(dry)
			Expect(reconcileImport(dry).Status.State).To(Equal(networkv1.ImportSkipped))
			lines = auditLines(sink)
			Expect(lines).To(HaveLen(2))
			Expect(lines[1].Region).To(Equal(azureWestEurope))
		})

		It("refuses a region that is not the virtual network's, and writes nothing", func() {
			updateScope(func(s *networkv1.NetworkScope) { s.Spec.Regions = []string{azureWestEurope, "northeurope"} })
			before := hubTags()
			imp := newImport(fmt.Sprintf("elsewhere-%d", scopeCounter), dataID, map[string]string{"hs-tier": "db"})
			imp.Spec.Region = "northeurope"
			create(imp)
			got := reconcileImport(imp)
			Expect(got.Status.State).To(Equal(networkv1.ImportFailed))
			Expect(readyReason(got.Status.Conditions)).To(Equal(ReasonRegionMismatch))
			Expect(got.Status.Error).To(ContainSubstring("is in westeurope, not in northeurope"))
			Expect(hubTags()).To(Equal(before))
			Expect(resynced).To(BeEmpty())
		})

		It("refuses an import without a region whose virtual network is where the scope does not look", func() {
			cloud.AddVirtualNetwork(azureSubscription, azureGroup, "far", "northeurope", []string{"10.77.0.0/16"}, nil)
			far := strings.Replace(vnetID, "/hub", "/far", 1)
			imp := newImport(fmt.Sprintf("far-%d", scopeCounter), far, map[string]string{"hs-owner": "team-a"})
			imp.Spec.Region = ""
			create(imp)
			got := reconcileImport(imp)
			Expect(got.Status.State).To(Equal(networkv1.ImportFailed))
			Expect(readyReason(got.Status.Conditions)).To(Equal("AccountNotInScope"))
			Expect(meta.FindStatusCondition(got.Status.Conditions, ConditionReady).Message).To(ContainSubstring("is in northeurope"))
			Expect(cloud.Tags(azureSubscription, azureGroup, "far")).To(BeEmpty())
		})
	})

	It("takes unmanaged resources over with an auto-import policy in Apply mode", func() {
		cloud.AddVirtualNetwork(azureSubscription, azureGroup, "legacy", azureWestEurope, []string{"10.99.0.0/16"}, nil)
		cloud.AddSubnet(azureSubscription, azureGroup, "legacy", "old", "10.99.0.0/24")
		updateScope(func(s *networkv1.NetworkScope) {
			s.Spec.NetworkSelector = &networkv1.NetworkSelector{MatchTags: map[string]string{"hs-owner": ""}}
			s.Spec.AutoImport = &networkv1.AutoImportPolicy{Mode: networkv1.AutoImportApply,
				AccountDefaults: []networkv1.AccountDefault{{Account: strings.ToUpper(azureSubscription),
					Tags: map[string]string{"hs-owner": "platform"}}}}
		})
		reconcileScope()

		list := &networkv1.ResourceImportList{}
		Expect(k8sClient.List(ctx, list, client.InNamespace("default"),
			client.MatchingLabels{networkv1.LabelScope: scopeName})).To(Succeed())
		Expect(list.Items).NotTo(BeEmpty())
		for i := range list.Items {
			got := reconcileImport(&list.Items[i])
			Expect(got.Status.State).To(Equal(networkv1.ImportApplied), "%s: %v", got.Spec.ResourceID, got.Status.Conditions)
		}
		legacy := cloud.Tags(azureSubscription, azureGroup, "legacy")
		Expect(legacy).To(HaveKeyWithValue("hs-owner", "platform"))
		Expect(legacy).To(HaveKeyWithValue("hs-managed", "true"))
		if entry, ok := legacy[azurecloud.SubnetEntryName("old")]; ok {
			// The subnet inherits what the network got; its entry holds only what differs.
			Expect(azurecloud.DecodeSubnetEntry(entry)).NotTo(HaveKey("hs-owner"))
		}
	})
})

// azureNetworkMetric returns the value of a gauge's series for one Azure network.
func azureNetworkMetric(metric, scope, networkID string) (float64, bool) {
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
			if labels["scope"] == scope && labels["network_id"] == networkID && labels["provider"] == "azure" {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}
