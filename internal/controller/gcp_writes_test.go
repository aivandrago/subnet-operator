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
	"slices"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	gcpcloud "hypersurgery.dev/subnet-operator/internal/cloud/gcp"
	"hypersurgery.dev/subnet-operator/internal/cloud/gcp/gcpfake"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

// bindingsOf lists what is bound to a subnetwork or network of the fake as key=value.
func bindingsOf(bindings []gcpfake.Binding) []string {
	out := make([]string, 0, len(bindings))
	for _, b := range bindings {
		out = append(out, b.Key+"="+b.Value)
	}
	slices.Sort(out)
	return out
}

// Writes on a GCP scope (#47), end to end: the claim, import and scope controllers with the
// real GCP provider against the in-repo fake of Compute and Resource Manager.
var _ = Describe("GCP writes", func() {
	var (
		scopeName string
		cloud     *gcpfake.Cloud
		providers *provider.Registry
		scopes    *NetworkScopeReconciler
		claims    *SubnetClaimReconciler
		imports   *ResourceImportReconciler
	)
	orgParent := "organizations/" + gcpOrg
	networkID := "projects/" + gcpProject + "/global/networks/shared"
	appsID := "projects/" + gcpProject + "/regions/" + gcpEurope + "/subnetworks/apps"

	reconcileScope := func() {
		GinkgoHelper()
		_, err := scopes.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: scopeName}})
		Expect(err).NotTo(HaveOccurred())
	}
	// rediscover changes the scope's spec, so its next reconcile is a full sync that finds what
	// the claims created.
	rediscover := func() {
		GinkgoHelper()
		scope := &networkv1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, scope)).To(Succeed())
		interval := time.Minute
		if scope.Spec.ResyncInterval != nil {
			interval = scope.Spec.ResyncInterval.Duration + time.Minute
		}
		scope.Spec.ResyncInterval = &metav1.Duration{Duration: interval}
		Expect(k8sClient.Update(ctx, scope)).To(Succeed())
		reconcileScope()
	}
	updateScope := func(mutate func(*networkv1.NetworkScope)) {
		GinkgoHelper()
		scope := &networkv1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, scope)).To(Succeed())
		mutate(scope)
		Expect(k8sClient.Update(ctx, scope)).To(Succeed())
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

	BeforeEach(func() {
		scopeCounter++
		scopeName = fmt.Sprintf("gcp-writes-%d", scopeCounter)
		cloud = gcpfake.New()
		DeferCleanup(cloud.Close)
		cloud.AddProject(gcpProject, "987654321098")
		// The platform team's keys, and the values it has created so far.
		k := networkv1.OperatorTagKeysFor(networkv1.ProviderGCP)
		for _, key := range []string{k.Owner, k.Env, k.Tier, k.Managed, k.ManagedBy, k.Claim} {
			cloud.AddTagKey(orgParent, gcpOrg, key)
		}
		cloud.AddNetwork(gcpProject, "shared", gcpOrgTag("hs-owner", "platform"))
		cloud.AddSubnetwork(gcpProject, gcpEurope, "shared", "apps", "10.10.0.0/24",
			gcpOrgTag("hs-owner", "payments"))

		p := gcpcloud.NewProvider(gcpcloud.Options{ClientOptions: cloud.ClientOptions(),
			ComputeEndpoint: cloud.ComputeEndpoint(), ResourceManagerEndpoint: cloud.ResourceManagerEndpoint})
		providers = provider.MustRegistry(p)
		scopes = &NetworkScopeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			Providers: providers}
		claims = &SubnetClaimReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			Providers: providers, WritesEnabled: true}
		imports = &ResourceImportReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			Providers: providers, WritesEnabled: true}

		scope := &networkv1.NetworkScope{
			ObjectMeta: metav1.ObjectMeta{Name: scopeName},
			Spec: networkv1.NetworkScopeSpec{
				Provider:          networkv1.ProviderGCP,
				Accounts:          []networkv1.Account{{ID: gcpProject}},
				Regions:           []string{gcpEurope},
				GCP:               &networkv1.GCPScope{TagParent: orgParent, CreateTagValues: true},
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
			Spec: networkv1.SubnetClaimSpec{ScopeRef: scopeName, Account: gcpProject, Region: gcpEurope,
				NetworkID: networkID, PrefixLength: 24, Mode: networkv1.ClaimModeCreate, Owner: "team-a", Env: "prod",
				GCP: &networkv1.GCPClaimOptions{PoolCIDRs: []string{"10.10.0.0/16"}, PrivateIPGoogleAccess: true}},
		}
	}

	// loseStatus drops a claim's allocations, as a status update lost after the create would.
	loseStatus := func(claim *networkv1.SubnetClaim) {
		GinkgoHelper()
		got := &networkv1.SubnetClaim{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), got)).To(Succeed())
		got.Status.Allocations = nil
		Expect(k8sClient.Status().Update(ctx, got)).To(Succeed())
	}

	It("creates the subnetwork of a Create-mode claim with its ownership tags and no claim tag by default", func() {
		scope := &networkv1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, scope)).To(Succeed())
		Expect(scope.Spec.GCP.ClaimTag).To(Equal(networkv1.GCPClaimTagSkip), "the API server defaults claimTag")

		claim := newClaim(fmt.Sprintf("orders-%d", scopeCounter))
		create(claim)

		got := reconcileClaim(claim)
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, ConditionReady)).To(BeTrue(), "%v", got.Status.Conditions)
		Expect(got.Status.Allocations).To(HaveLen(1))
		a := got.Status.Allocations[0]
		Expect(a.Name).To(Equal(claim.Name), "a zone-less allocation is keyed by the subnetwork's name")
		Expect(a.Zone).To(BeEmpty())
		Expect(a.CIDRBlock).To(Equal("10.10.1.0/24"), "10.10.0.0/24 is taken by apps")
		Expect(a.SubnetID).To(Equal("projects/" + gcpProject + "/regions/" + gcpEurope + "/subnetworks/" + claim.Name))
		Expect(a.State).To(Equal(networkv1.AllocationCreated))

		s := cloud.Subnetwork(gcpProject, gcpEurope, claim.Name)
		Expect(s).NotTo(BeNil())
		Expect(s.CIDR).To(Equal("10.10.1.0/24"))
		Expect(s.PrivateIPGoogleAccess).To(BeTrue())
		Expect(bindingsOf(s.Bindings)).To(ConsistOf("hs-owner=team-a", "hs-env=prod",
			"hs-managed-by=subnet-operator"))
		Expect(cloud.TagValueExists(gcpOrg, "hs-claim", "default_"+claim.Name)).To(BeFalse(),
			"no per-claim value is created")

		// Discovered, the subnetwork is adopted again by its name even if the status is lost.
		rediscover()
		loseStatus(claim)
		again := reconcileClaim(claim)
		Expect(meta.IsStatusConditionTrue(again.Status.Conditions, ConditionReady)).To(BeTrue(), "%v", again.Status.Conditions)
		Expect(again.Status.Allocations).To(HaveLen(1))
		Expect(again.Status.Allocations[0].SubnetID).To(Equal(a.SubnetID))
		Expect(again.Status.Allocations[0].CIDRBlock).To(Equal("10.10.1.0/24"))
	})

	It("does not adopt by name what another claim could have created", func() {
		first := newClaim(fmt.Sprintf("orders-%d", scopeCounter))
		create(first)
		Expect(meta.IsStatusConditionTrue(reconcileClaim(first).Status.Conditions, ConditionReady)).To(BeTrue())
		rediscover()

		// A second claim for the same subnetwork name reserves its own range and is refused by
		// Compute; it does not take the first claim's subnetwork over.
		second := newClaim(fmt.Sprintf("orders-copy-%d", scopeCounter))
		second.Spec.NamePrefix = first.Name
		create(second)
		got := reconcileClaim(second)
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, ConditionReady)).To(BeFalse())
		Expect(got.Status.Allocations).To(HaveLen(1))
		Expect(got.Status.Allocations[0].SubnetID).To(BeEmpty())
		Expect(got.Status.Allocations[0].Error).To(ContainSubstring("exists already"))

		// With two claims leading to the same name, neither adopts it by name.
		loseStatus(first)
		again := reconcileClaim(first)
		Expect(again.Status.Allocations).To(HaveLen(1))
		Expect(again.Status.Allocations[0].SubnetID).To(BeEmpty(), "ambiguous: adopted by neither claim")

		// A subnetwork the operator did not create is never adopted by name.
		cloud.AddSubnetwork(gcpProject, gcpEurope, "shared", "handmade", "10.10.9.0/24",
			gcpOrgTag("hs-owner", "team-a"))
		rediscover()
		handmade := newClaim(fmt.Sprintf("handmade-%d", scopeCounter))
		handmade.Spec.NamePrefix = "handmade"
		create(handmade)
		got = reconcileClaim(handmade)
		Expect(got.Status.Allocations).To(HaveLen(1))
		Expect(got.Status.Allocations[0].CIDRBlock).NotTo(Equal("10.10.9.0/24"))
		Expect(got.Status.Allocations[0].SubnetID).To(BeEmpty())
	})

	It("binds the claim tag when the scope sets claimTag Bind, and adopts by it", func() {
		updateScope(func(s *networkv1.NetworkScope) { s.Spec.GCP.ClaimTag = networkv1.GCPClaimTagBind })
		claim := newClaim(fmt.Sprintf("ledger-%d", scopeCounter))
		create(claim)

		got := reconcileClaim(claim)
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, ConditionReady)).To(BeTrue(), "%v", got.Status.Conditions)
		s := cloud.Subnetwork(gcpProject, gcpEurope, claim.Name)
		Expect(s).NotTo(BeNil())
		Expect(bindingsOf(s.Bindings)).To(ConsistOf("hs-owner=team-a", "hs-env=prod",
			"hs-managed-by=subnet-operator", "hs-claim=default_"+claim.Name))

		rediscover()
		loseStatus(claim)
		again := reconcileClaim(claim)
		Expect(again.Status.Allocations).To(HaveLen(1))
		Expect(again.Status.Allocations[0].SubnetID).To(Equal(got.Status.Allocations[0].SubnetID))
	})

	It("needs no hs-claim value without createTagValues when the claim tag is skipped", func() {
		updateScope(func(s *networkv1.NetworkScope) { s.Spec.GCP.CreateTagValues = false })
		for key, value := range map[string]string{"hs-owner": "team-a", "hs-env": "prod",
			"hs-managed-by": networkv1.TagManagedByValue} {
			cloud.AddTagValue("organizations/"+gcpOrg, gcpOrg, key, value)
		}
		claim := newClaim(fmt.Sprintf("invoices-%d", scopeCounter))
		create(claim)

		got := reconcileClaim(claim)
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, ConditionReady)).To(BeTrue(), "%v", got.Status.Conditions)
		Expect(bindingsOf(cloud.Subnetwork(gcpProject, gcpEurope, claim.Name).Bindings)).To(ConsistOf(
			"hs-owner=team-a", "hs-env=prod", "hs-managed-by=subnet-operator"))
	})

	It("refuses a claim whose tag value is missing when the scope may not create values", func() {
		updateScope(func(s *networkv1.NetworkScope) {
			s.Spec.GCP.CreateTagValues = false
			s.Spec.GCP.ClaimTag = networkv1.GCPClaimTagBind
		})
		claim := newClaim(fmt.Sprintf("billing-%d", scopeCounter))
		create(claim)

		got := reconcileClaim(claim)
		ready := meta.FindStatusCondition(got.Status.Conditions, ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("TagValueMissing"))
		Expect(got.Status.Allocations).To(HaveLen(1))
		Expect(got.Status.Allocations[0].Error).To(ContainSubstring(gcpOrg + "/hs-claim/default_" + claim.Name))
		Expect(cloud.Subnetwork(gcpProject, gcpEurope, claim.Name)).To(BeNil(), "nothing is created half-way")
	})

	It("imports by adding bindings and refuses to replace a bound value", func() {
		imp := &networkv1.ResourceImport{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("apps-%d", scopeCounter), Namespace: "default"},
			Spec: networkv1.ResourceImportSpec{ScopeRef: scopeName, Account: gcpProject, Region: gcpEurope,
				ResourceID: appsID, Tags: map[string]string{"hs-owner": "payments", "hs-env": "prod"}},
		}
		create(imp)
		got := reconcileImport(imp)
		Expect(got.Status.State).To(Equal(networkv1.ImportApplied), "%v", got.Status.Conditions)
		apps := cloud.Subnetwork(gcpProject, gcpEurope, "apps")
		Expect(bindingsOf(apps.Bindings)).To(Equal([]string{"hs-env=prod", "hs-owner=payments"}))

		other := &networkv1.ResourceImport{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("apps-owner-%d", scopeCounter), Namespace: "default"},
			Spec: networkv1.ResourceImportSpec{ScopeRef: scopeName, Account: gcpProject, Region: gcpEurope,
				ResourceID: appsID, Tags: map[string]string{"hs-owner": "billing", "hs-tier": "db"}},
		}
		create(other)
		got = reconcileImport(other)
		Expect(got.Status.State).To(Equal(networkv1.ImportFailed))
		ready := meta.FindStatusCondition(got.Status.Conditions, ConditionReady)
		Expect(ready.Reason).To(Equal("TagValueConflict"))
		Expect(ready.Message).To(ContainSubstring(`hs-owner is "payments"`))
		Expect(bindingsOf(apps.Bindings)).To(Equal([]string{"hs-env=prod", "hs-owner=payments"}),
			"nothing is bound by a refused import, hs-tier included")
	})

	It("imports a global network without a region", func() {
		imp := &networkv1.ResourceImport{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("shared-%d", scopeCounter), Namespace: "default"},
			Spec: networkv1.ResourceImportSpec{ScopeRef: scopeName, Account: gcpProject, ResourceID: networkID,
				Tags: map[string]string{"hs-env": "prod"}},
		}
		create(imp)
		got := reconcileImport(imp)
		Expect(got.Status.State).To(Equal(networkv1.ImportApplied), "%v", got.Status.Conditions)
		shared := cloud.Networks(gcpProject)[0]
		Expect(bindingsOf(shared.Bindings)).To(Equal([]string{"hs-env=prod", "hs-owner=platform"}))
	})

	// Only Azure IDs are compared without regard to case: a GCP resource name in another case is
	// not the discovered network, and is neither rewritten nor matched to it.
	It("compares GCP network IDs exactly: another case is not the discovered network", func() {
		i := strings.LastIndex(networkID, "/")
		capitals := networkID[:i+1] + strings.ToUpper(networkID[i+1:])
		Expect(capitals).NotTo(Equal(networkID))

		claim := newClaim(fmt.Sprintf("capitals-%d", scopeCounter))
		claim.Spec.NetworkID = capitals
		create(claim)
		got := reconcileClaim(claim)
		Expect(meta.IsStatusConditionTrue(got.Status.Conditions, ConditionReady)).To(BeFalse(), "%v", got.Status.Conditions)
		Expect(got.Status.Allocations).To(BeEmpty())
		Expect(got.Spec.NetworkID).To(Equal(capitals))
		Expect(cloud.Subnetwork(gcpProject, gcpEurope, claim.Name)).To(BeNil())

		imp := &networkv1.ResourceImport{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("capitals-%d", scopeCounter), Namespace: "default"},
			Spec: networkv1.ResourceImportSpec{ScopeRef: scopeName, Account: gcpProject, ResourceID: capitals,
				Tags: map[string]string{"hs-env": "prod"}},
		}
		create(imp)
		Expect(reconcileImport(imp).Status.State).To(Equal(networkv1.ImportFailed))
	})

	It("takes unmanaged resources over with an auto-import policy in Apply mode", func() {
		cloud.AddNetwork(gcpProject, "legacy")
		cloud.AddSubnetwork(gcpProject, gcpEurope, "legacy", "old", "10.99.0.0/24")
		updateScope(func(s *networkv1.NetworkScope) {
			s.Spec.NetworkSelector = &networkv1.NetworkSelector{MatchTags: map[string]string{"hs-managed": ""}}
			s.Spec.AutoImport = &networkv1.AutoImportPolicy{Mode: networkv1.AutoImportApply,
				AccountDefaults: []networkv1.AccountDefault{{Account: gcpProject,
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
		old := cloud.Subnetwork(gcpProject, gcpEurope, "old")
		Expect(bindingsOf(old.Bindings)).To(ContainElements("hs-owner=platform", "hs-managed=true"))
		Expect(cloud.TagValueExists(gcpOrg, "hs-managed", "true")).To(BeTrue(),
			"the scope lets the operator create the values it needs")
	})
})
