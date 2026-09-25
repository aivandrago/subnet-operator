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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kevents "k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
	"hypersurgery.dev/subnet-operator/internal/audit"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/tenancy"
)

var tenancyCounter int

// These specs run without admission webhooks, which is the point: they are what stands
// between a claim or an import applied while the webhooks were off and the write roles.
var _ = Describe("Namespaces allowed to use a NetworkScope", func() {
	const (
		tenancyAccount = "111111111111"
		tenancyRegion  = "eu-central-1"
		teamLabel      = "example.com/team"
	)
	var (
		scopeName string
		allowedNS string
		otherNS   string
		vpcID     string
		recorder  *kevents.FakeRecorder
	)

	createNamespace := func(name, team string) {
		GinkgoHelper()
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name: name, Labels: map[string]string{teamLabel: team}}})).To(Succeed())
	}
	paymentsOnly := func() *metav1.LabelSelector {
		return &metav1.LabelSelector{MatchLabels: map[string]string{teamLabel: "payments"}}
	}
	createScope := func(selector *metav1.LabelSelector, policy *networkv1beta1.AutoImportPolicy) {
		GinkgoHelper()
		Expect(k8sClient.Create(ctx, &networkv1beta1.NetworkScope{
			ObjectMeta: metav1.ObjectMeta{Name: scopeName},
			Spec: networkv1beta1.NetworkScopeSpec{
				Provider:          networkv1beta1.ProviderAWS,
				Accounts:          []networkv1beta1.Account{{ID: tenancyAccount}},
				Regions:           []string{tenancyRegion},
				NetworkSelector:   &networkv1beta1.NetworkSelector{MatchTags: map[string]string{"hs/managed": "true"}},
				NamespaceSelector: selector,
				AutoImport:        policy,
			},
		})).To(Succeed())
	}
	setSelector := func(selector *metav1.LabelSelector) {
		GinkgoHelper()
		scope := &networkv1beta1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, scope)).To(Succeed())
		scope.Spec.NamespaceSelector = selector
		Expect(k8sClient.Update(ctx, scope)).To(Succeed())
	}

	BeforeEach(func() {
		tenancyCounter++
		scopeName = fmt.Sprintf("tenancy-scope-%d", tenancyCounter)
		allowedNS = fmt.Sprintf("tenancy-payments-%d", tenancyCounter)
		otherNS = fmt.Sprintf("tenancy-data-%d", tenancyCounter)
		vpcID = fmt.Sprintf("vpc-7e4a%04d", tenancyCounter)
		recorder = kevents.NewFakeRecorder(64)
		createNamespace(allowedNS, "payments")
		createNamespace(otherNS, "data")
	})

	AfterEach(func() {
		for _, ns := range []string{allowedNS, otherNS} {
			Expect(k8sClient.DeleteAllOf(ctx, &networkv1beta1.SubnetClaim{}, client.InNamespace(ns))).To(Succeed())
			Expect(k8sClient.DeleteAllOf(ctx, &networkv1beta1.ResourceImport{}, client.InNamespace(ns))).To(Succeed())
		}
		Expect(k8sClient.DeleteAllOf(ctx, &networkv1beta1.Network{},
			client.MatchingLabels{networkv1beta1.LabelScope: scopeName})).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &networkv1beta1.NetworkScope{
			ObjectMeta: metav1.ObjectMeta{Name: scopeName}}))).To(Succeed())
	})

	Describe("a SubnetClaim", func() {
		var (
			writer     *fakeWriter
			reconciler *SubnetClaimReconciler
		)

		createVPC := func() {
			GinkgoHelper()
			vpc := &networkv1beta1.Network{
				ObjectMeta: metav1.ObjectMeta{Name: vpcID,
					Labels: map[string]string{networkv1beta1.LabelScope: scopeName, networkv1beta1.LabelNetwork: vpcID}},
				Spec: networkv1beta1.NetworkSpec{Provider: networkv1beta1.ProviderAWS, ID: vpcID, Account: tenancyAccount, Region: tenancyRegion},
			}
			Expect(k8sClient.Create(ctx, vpc)).To(Succeed())
			vpc.Status.CIDRBlocks = []string{"10.70.0.0/16"}
			Expect(k8sClient.Status().Update(ctx, vpc)).To(Succeed())
		}
		createClaim := func(namespace string, zones ...string) {
			GinkgoHelper()
			Expect(k8sClient.Create(ctx, &networkv1beta1.SubnetClaim{
				ObjectMeta: metav1.ObjectMeta{Name: "capacity", Namespace: namespace},
				Spec: networkv1beta1.SubnetClaimSpec{
					ScopeRef: scopeName, Account: tenancyAccount, Region: tenancyRegion, NetworkID: vpcID,
					PrefixLength: 24, Zones: zones,
					Mode: networkv1beta1.ClaimModeCreate, Owner: "team-payments",
				},
			})).To(Succeed())
		}
		reconcileClaim := func(namespace string) *networkv1beta1.SubnetClaim {
			GinkgoHelper()
			key := types.NamespacedName{Name: "capacity", Namespace: namespace}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			claim := &networkv1beta1.SubnetClaim{}
			Expect(k8sClient.Get(ctx, key, claim)).To(Succeed())
			return claim
		}

		BeforeEach(func() {
			writer = &fakeWriter{conflictOnce: map[string]bool{}}
			reconciler = &SubnetClaimReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(), Writer: writer, WritesEnabled: true,
				Recorder: recorder,
			}
			createVPC()
		})

		It("never reaches the writer from a namespace the scope does not select", func() {
			createScope(paymentsOnly(), nil)
			createClaim(otherNS, tenancyRegion+"a")

			claim := reconcileClaim(otherNS)

			ready := meta.FindStatusCondition(claim.Status.Conditions, ConditionReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Status).To(Equal(metav1.ConditionFalse))
			Expect(ready.Reason).To(Equal(tenancy.ReasonNamespaceNotAllowed))
			Expect(ready.Message).To(ContainSubstring(otherNS))
			Expect(claim.Status.Allocations).To(BeEmpty(), "not even a reservation")
			Expect(writer.requests).To(BeEmpty())
			haveEvent(recorder, "Warning "+tenancy.ReasonNamespaceNotAllowed)
		})

		It("creates subnets from a namespace the scope selects", func() {
			createScope(paymentsOnly(), nil)
			createClaim(allowedNS, tenancyRegion+"a")

			claim := reconcileClaim(allowedNS)

			Expect(meta.IsStatusConditionTrue(claim.Status.Conditions, ConditionReady)).To(BeTrue())
			Expect(writer.requests).To(HaveLen(1))
		})

		It("keeps what a claim has but writes nothing more once its namespace is no longer allowed", func() {
			createScope(paymentsOnly(), nil)
			createClaim(allowedNS, tenancyRegion+"a")
			Expect(reconcileClaim(allowedNS).Status.Allocations).To(HaveLen(1))
			Expect(writer.requests).To(HaveLen(1))

			By("the scope narrowing to another team, and the claim asking for a second zone")
			setSelector(&metav1.LabelSelector{MatchLabels: map[string]string{teamLabel: "data"}})
			claim := &networkv1beta1.SubnetClaim{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "capacity", Namespace: allowedNS}, claim)).To(Succeed())
			claim.Spec.Zones = append(claim.Spec.Zones, tenancyRegion+"b")
			Expect(k8sClient.Update(ctx, claim)).To(Succeed())

			claim = reconcileClaim(allowedNS)

			Expect(meta.FindStatusCondition(claim.Status.Conditions, ConditionReady).Reason).
				To(Equal(tenancy.ReasonNamespaceNotAllowed))
			Expect(writer.requests).To(HaveLen(1), "no subnet for the new zone")
			Expect(claim.Status.Allocations).To(HaveLen(1), "the existing subnet stays reserved")
			Expect(claim.Status.Allocations[0].SubnetID).To(Equal("subnet-new1"))
		})
	})

	Describe("a ResourceImport", func() {
		var (
			writer     *fakeTagWriter
			reconciler *ResourceImportReconciler
		)

		createImport := func(namespace string) {
			GinkgoHelper()
			Expect(k8sClient.Create(ctx, &networkv1beta1.ResourceImport{
				ObjectMeta: metav1.ObjectMeta{Name: "take-over", Namespace: namespace},
				Spec: networkv1beta1.ResourceImportSpec{
					ScopeRef: scopeName, Account: tenancyAccount, Region: tenancyRegion,
					ResourceID: "subnet-0e4a0001", Tags: map[string]string{"hs/owner": "team-payments"},
				},
			})).To(Succeed())
		}
		reconcileImport := func(namespace string) *networkv1beta1.ResourceImport {
			GinkgoHelper()
			key := types.NamespacedName{Name: "take-over", Namespace: namespace}
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: key})
			Expect(err).NotTo(HaveOccurred())
			imp := &networkv1beta1.ResourceImport{}
			Expect(k8sClient.Get(ctx, key, imp)).To(Succeed())
			return imp
		}

		BeforeEach(func() {
			writer = &fakeTagWriter{}
			reconciler = &ResourceImportReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(), Writer: writer, WritesEnabled: true,
				Recorder: recorder,
			}
		})

		It("never reaches the writer from a namespace the scope does not select", func() {
			createScope(paymentsOnly(), nil)
			createImport(otherNS)

			imp := reconcileImport(otherNS)

			Expect(imp.Status.State).To(Equal(networkv1beta1.ImportFailed))
			ready := meta.FindStatusCondition(imp.Status.Conditions, ConditionReady)
			Expect(ready).NotTo(BeNil())
			Expect(ready.Reason).To(Equal(tenancy.ReasonNamespaceNotAllowed))
			Expect(writer.calls).To(BeEmpty())
		})

		It("tags from a namespace the scope selects", func() {
			createScope(paymentsOnly(), nil)
			createImport(allowedNS)

			imp := reconcileImport(allowedNS)

			Expect(imp.Status.State).To(Equal(networkv1beta1.ImportApplied))
			Expect(writer.calls).To(HaveLen(1))
		})

		It("refuses every namespace when the selector does not parse", func() {
			createScope(paymentsOnly(), nil)
			createImport(allowedNS)
			// The CRD cannot tell an In without values from a good one; only the webhook, which
			// is off here, refuses it.
			setSelector(&metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: teamLabel, Operator: metav1.LabelSelectorOpIn}}})

			imp := reconcileImport(allowedNS)

			Expect(meta.FindStatusCondition(imp.Status.Conditions, ConditionReady).Reason).
				To(Equal(tenancy.ReasonNamespaceNotAllowed))
			Expect(writer.calls).To(BeEmpty())
		})

		It("works for every namespace when the scope's selector is empty", func() {
			createScope(&metav1.LabelSelector{}, nil)
			createImport(otherNS)

			imp := reconcileImport(otherNS)

			Expect(imp.Status.State).To(Equal(networkv1beta1.ImportApplied))
			Expect(writer.calls).To(HaveLen(1))
		})

		It("is refused in every namespace when the scope has no selector", func() {
			// In aws.hypersurgery/v1alpha1 an unset selector allowed every namespace; in this
			// group it allows none.
			createScope(nil, nil)
			createImport(allowedNS)

			imp := reconcileImport(allowedNS)

			Expect(imp.Status.State).To(Equal(networkv1beta1.ImportFailed))
			Expect(meta.FindStatusCondition(imp.Status.Conditions, ConditionReady).Reason).
				To(Equal(tenancy.ReasonNamespaceNotAllowed))
			Expect(meta.FindStatusCondition(imp.Status.Conditions, ConditionReady).Message).
				To(ContainSubstring("which allows no namespace"))
			Expect(writer.calls).To(BeEmpty())
		})
	})

	Describe("a NetworkScope", func() {
		var (
			discoverer *fakeDiscoverer
			reconciler *NetworkScopeReconciler
			sink       *bytes.Buffer
		)

		reconcileScope := func() {
			GinkgoHelper()
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: scopeName}})
			Expect(err).NotTo(HaveOccurred())
		}
		importsIn := func(namespace string) []networkv1beta1.ResourceImport {
			GinkgoHelper()
			list := &networkv1beta1.ResourceImportList{}
			Expect(k8sClient.List(ctx, list, client.InNamespace(namespace),
				client.MatchingLabels{networkv1beta1.LabelScope: scopeName})).To(Succeed())
			return list.Items
		}
		policyIn := func(namespace string) *networkv1beta1.AutoImportPolicy {
			return &networkv1beta1.AutoImportPolicy{
				Mode: networkv1beta1.AutoImportApply, Namespace: namespace,
				AccountDefaults: []networkv1beta1.AccountDefault{
					{Account: tenancyAccount, Tags: map[string]string{"hs/owner": "team-platform"}}},
			}
		}

		BeforeEach(func() {
			sink = &bytes.Buffer{}
			discoverer = &fakeDiscoverer{
				snapshots: map[string]*inventory.Snapshot{tenancyAccount + "/" + tenancyRegion: {
					UnmanagedVPCs: []inventory.VPC{{ID: "vpc-0e4a1dea", Account: tenancyAccount, Region: tenancyRegion,
						CIDRBlocks: []string{"10.91.0.0/16"}}},
				}},
				errs: map[string]error{},
			}
			reconciler = &NetworkScopeReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(), Discoverer: discoverer, Creators: &CreatorCache{},
				Recorder: recorder, Audit: audit.NewWriter(sink),
				Identity: "system:serviceaccount:subnet-operator-system:subnet-operator",
			}
		})

		It("warns once per spec change that its namespaceSelector allows every namespace", func() {
			createScope(&metav1.LabelSelector{}, nil)

			reconcileScope()
			haveEvent(recorder, "Warning "+EventNamespacesUnrestricted, "has an empty spec.namespaceSelector")

			// A second reconcile inside the resync interval with nothing pending returns before
			// discovery; the warning must not depend on that, and must not repeat either.
			reconcileScope()
			Expect(emittedEvents(recorder)).NotTo(ContainElement(ContainSubstring(EventNamespacesUnrestricted)))
		})

		It("does not warn about a scope that has a selector, or none", func() {
			createScope(paymentsOnly(), nil)
			reconcileScope()
			setSelector(nil)

			reconcileScope()
			Expect(emittedEvents(recorder)).NotTo(ContainElement(ContainSubstring(EventNamespacesUnrestricted)))
		})

		It("runs no auto-import policy into a namespace the scope does not select", func() {
			createScope(paymentsOnly(), policyIn(otherNS))

			reconcileScope()

			Expect(importsIn(otherNS)).To(BeEmpty())
			haveEvent(recorder, "Warning "+tenancy.ReasonNamespaceNotAllowed, "auto-import policy is not run")
		})

		It("writes imports into an allowed namespace as the operator", func() {
			createScope(paymentsOnly(), policyIn(allowedNS))

			reconcileScope()

			imports := importsIn(allowedNS)
			Expect(imports).To(HaveLen(1))
			Expect(imports[0].Annotations).To(HaveKeyWithValue(networkv1beta1.AnnotationCreatedBy, reconciler.Identity))

			decisions := auditLines(sink)
			Expect(decisions).NotTo(BeEmpty())
			Expect(decisions[0].CreatedBy).To(Equal(reconciler.Identity), "the policy decides as the operator")
		})
	})
})
