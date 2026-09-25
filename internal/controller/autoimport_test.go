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
	"context"
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/metrics"
)

var autoCounter int

var _ = Describe("Auto-import policy", func() {
	const (
		autoAccount  = "111111111111"
		autoRegion   = "eu-central-1"
		paymentsRole = "arn:aws:sts::111111111111:assumed-role/payments-deploy/maria.k"
		terraformCI  = "arn:aws:sts::111111111111:assumed-role/terraform-apply/runner"
	)
	var (
		scopeName  string
		discoverer *fakeDiscoverer
		creators   *CreatorCache
		reconciler *NetworkScopeReconciler
	)

	// An account holding one managed VPC, one unmanaged VPC and two unmanaged subnets:
	// one inside the managed VPC (so inheritance can apply) and one inside the unmanaged one.
	snapshot := func() *inventory.Snapshot {
		return &inventory.Snapshot{
			Networks: []inventory.Network{{ID: "vpc-0aa11bb2", Account: autoAccount, Region: autoRegion,
				CIDRBlocks: []string{"10.0.0.0/16"},
				Tags:       map[string]string{"hs/managed": "true", "hs/owner": "team-platform", "hs/env": "prod"}}},
			UnmanagedNetworks: []inventory.Network{{ID: "vpc-0fee1dead", Account: autoAccount, Region: autoRegion,
				CIDRBlocks: []string{"10.90.0.0/16"}, Tags: map[string]string{"Name": "legacy"}}},
			UnmanagedSubnets: []inventory.Subnet{
				{ID: "subnet-0abc1111", NetworkID: "vpc-0aa11bb2", Account: autoAccount, Region: autoRegion, CIDRBlock: "10.0.9.0/24"},
				{ID: "subnet-0def2222", NetworkID: "vpc-0fee1dead", Account: autoAccount, Region: autoRegion, CIDRBlock: "10.90.1.0/24"},
			},
		}
	}

	reconcileScope := func() {
		GinkgoHelper()
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: scopeName}})
		Expect(err).NotTo(HaveOccurred())
	}
	imports := func() []networkv1beta1.ResourceImport {
		GinkgoHelper()
		list := &networkv1beta1.ResourceImportList{}
		Expect(k8sClient.List(ctx, list, client.InNamespace("default"),
			client.MatchingLabels{networkv1beta1.LabelScope: scopeName})).To(Succeed())
		return list.Items
	}
	importFor := func(resourceID string) *networkv1beta1.ResourceImport {
		GinkgoHelper()
		for _, imp := range imports() {
			if imp.Spec.ResourceID == resourceID {
				return &imp
			}
		}
		return nil
	}
	createScope := func(policy *networkv1beta1.AutoImportPolicy) {
		GinkgoHelper()
		Expect(k8sClient.Create(ctx, &networkv1beta1.NetworkScope{
			ObjectMeta: metav1.ObjectMeta{Name: scopeName},
			Spec: networkv1beta1.NetworkScopeSpec{
				Provider:          networkv1beta1.ProviderAWS,
				NamespaceSelector: &metav1.LabelSelector{},
				Accounts:          []networkv1beta1.Account{{ID: autoAccount}},
				Regions:           []string{autoRegion},
				NetworkSelector:   &networkv1beta1.NetworkSelector{MatchTags: map[string]string{"hs/managed": "true"}},
				AutoImport:        policy,
			},
		})).To(Succeed())
	}
	policy := func(mode networkv1beta1.AutoImportMode) *networkv1beta1.AutoImportPolicy {
		return &networkv1beta1.AutoImportPolicy{
			Mode: mode,
			FromCreator: []networkv1beta1.CreatorRule{
				{PrincipalPrefix: "arn:aws:sts::111111111111:assumed-role/payments-",
					Tags: map[string]string{"hs/owner": "team-payments"}},
			},
			InheritFromNetwork: []string{"hs/owner", "hs/env"},
			Skip:               []networkv1beta1.SkipRule{{PrincipalPrefix: "arn:aws:sts::111111111111:assumed-role/terraform-"}},
		}
	}

	BeforeEach(func() {
		autoCounter++
		scopeName = fmt.Sprintf("auto-scope-%d", autoCounter)
		creators = &CreatorCache{}
		discoverer = &fakeDiscoverer{
			snapshots: map[string]*inventory.Snapshot{autoAccount + "/" + autoRegion: snapshot()},
			errs:      map[string]error{},
		}
		reconciler = &NetworkScopeReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Providers: awsProviders(discoverer, nil, nil), Creators: creators,
		}
	})

	AfterEach(func() {
		for _, imp := range imports() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &imp))).To(Succeed())
		}
		Expect(k8sClient.DeleteAllOf(ctx, &networkv1beta1.Subnet{}, client.MatchingLabels{networkv1beta1.LabelScope: scopeName})).To(Succeed())
		Expect(k8sClient.DeleteAllOf(ctx, &networkv1beta1.Network{}, client.MatchingLabels{networkv1beta1.LabelScope: scopeName})).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &networkv1beta1.NetworkScope{
			ObjectMeta: metav1.ObjectMeta{Name: scopeName}}))).To(Succeed())
	})

	// dueForResync moves the scope's last full sync back past its resync interval. A reconcile
	// inside the interval with no pending events returns before discovery, so without this a
	// second reconcile checks nothing — and an assertion that "nothing changed" passes for the
	// wrong reason.
	dueForResync := func() {
		GinkgoHelper()
		scope := &networkv1beta1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, scope)).To(Succeed())
		past := metav1.NewTime(time.Now().Add(-2 * time.Hour))
		scope.Status.LastSyncTime = &past
		Expect(k8sClient.Status().Update(ctx, scope)).To(Succeed())
	}

	// newlyUnmanaged reads hs_unmanaged_resources_total for this scope — the counter behind
	// the UnmanagedNetworkResource alert.
	newlyUnmanaged := func() float64 {
		GinkgoHelper()
		families, err := ctrlmetrics.Registry.Gather()
		Expect(err).NotTo(HaveOccurred())
		total := 0.0
		for _, f := range families {
			if f.GetName() != "hs_unmanaged_resources_total" {
				continue
			}
			for _, m := range f.GetMetric() {
				for _, l := range m.GetLabel() {
					if l.GetName() == "scope" && l.GetValue() == scopeName {
						total += m.GetCounter().GetValue()
					}
				}
			}
		}
		return total
	}

	// The alert fires on an increase of that counter. When the set of resources already counted
	// lived only in process memory, a restart or a change of leader emptied it and every known
	// unmanaged resource was counted — and alerted on — again.
	It("does not report known unmanaged resources as new after a restart", func() {
		createScope(nil)
		reconcileScope()
		Expect(newlyUnmanaged()).To(Equal(3.0), "a first discovery: all three are new")

		scope := &networkv1beta1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, scope)).To(Succeed())
		Expect(scope.Status.Targets[0].UnmanagedIDs).To(
			ConsistOf("vpc-0fee1dead", "subnet-0abc1111", "subnet-0def2222"))

		// The alert asks whether the counter rose, so that is what is checked. Forget drops what
		// the process remembers about the scope and keeps the counter, as counters should be kept,
		// so it stands in for a restart or a change of leader without disturbing the baseline.
		By("losing the process's memory, as a restart or a change of leader does")
		metrics.Forget(scopeName)
		dueForResync()
		reconcileScope()
		Expect(newlyUnmanaged()).To(Equal(3.0), "nothing appeared, so the counter must not rise")

		By("but a resource created while the operator was away still counts")
		metrics.Forget(scopeName)
		snap := snapshot()
		snap.UnmanagedSubnets = append(snap.UnmanagedSubnets, inventory.Subnet{
			ID: "subnet-0new3333", NetworkID: "vpc-0fee1dead", Account: autoAccount, Region: autoRegion,
			CIDRBlock: "10.90.2.0/24"})
		discoverer.snapshots[autoAccount+"/"+autoRegion] = snap
		dueForResync()
		reconcileScope()
		Expect(newlyUnmanaged()).To(Equal(4.0), "one more: only the one created during the outage")
	})

	It("counts unmanaged resources without importing anything when the policy is off", func() {
		createScope(nil)
		reconcileScope()

		scope := &networkv1beta1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, scope)).To(Succeed())
		Expect(scope.Status.Unmanaged).To(Equal(int32(3)), "one VPC and two subnets")
		Expect(scope.Status.Targets[0].UnmanagedNetworks).To(Equal(int32(1)))
		Expect(scope.Status.Targets[0].UnmanagedSubnets).To(Equal(int32(2)))
		Expect(imports()).To(BeEmpty())

		By("and the unmanaged resources are not mirrored as objects")
		subnets := &networkv1beta1.SubnetList{}
		Expect(k8sClient.List(ctx, subnets, client.MatchingLabels{networkv1beta1.LabelScope: scopeName})).To(Succeed())
		for _, s := range subnets.Items {
			Expect(s.Spec.ID).NotTo(Equal("subnet-0def2222"))
		}
	})

	It("imports what it can attribute and leaves the rest alone", func() {
		creators.Record(context.Background(), []inventory.Creation{{
			Target:     inventory.TargetKey{Account: autoAccount, Region: autoRegion},
			ResourceID: "vpc-0fee1dead", Principal: paymentsRole, EventName: "CreateVpc",
		}})
		createScope(policy(networkv1beta1.AutoImportApply))
		reconcileScope()

		By("the VPC gets the creator's owner")
		vpcImport := importFor("vpc-0fee1dead")
		Expect(vpcImport).NotTo(BeNil())
		Expect(vpcImport.Spec.Tags).To(Equal(map[string]string{"hs/owner": "team-payments", "hs/managed": "true"}))
		Expect(vpcImport.Spec.DryRun).To(BeFalse())
		Expect(vpcImport.Spec.RequestedBy).To(ContainSubstring(networkv1beta1.RequestedByPolicy))
		Expect(vpcImport.Spec.RequestedBy).To(ContainSubstring("maria.k"), "the person is worth recording")
		Expect(vpcImport.Labels).To(HaveKeyWithValue(networkv1beta1.LabelResource, "vpc-0fee1dead"))

		By("the subnet in the managed VPC inherits from it")
		inherited := importFor("subnet-0abc1111")
		Expect(inherited).NotTo(BeNil())
		Expect(inherited.Spec.Tags).To(Equal(map[string]string{
			"hs/owner": "team-platform", "hs/env": "prod", "hs/managed": "true"}))

		By("the subnet nobody can be found for stays unmanaged")
		Expect(importFor("subnet-0def2222")).To(BeNil())
		Expect(imports()).To(HaveLen(2))

		By("a second sync creates no duplicates")
		reconcileScope()
		Expect(imports()).To(HaveLen(2))
	})

	It("marks its imports as dry runs in DryRun mode", func() {
		creators.Record(context.Background(), []inventory.Creation{{
			ResourceID: "vpc-0fee1dead", Principal: paymentsRole, EventName: "CreateVpc",
		}})
		createScope(policy(networkv1beta1.AutoImportDryRun))
		reconcileScope()

		imp := importFor("vpc-0fee1dead")
		Expect(imp).NotTo(BeNil())
		Expect(imp.Spec.DryRun).To(BeTrue())
	})

	It("skips what a pipeline created", func() {
		creators.Record(context.Background(), []inventory.Creation{{
			ResourceID: "vpc-0fee1dead", Principal: terraformCI, EventName: "CreateVpc",
		}})
		createScope(policy(networkv1beta1.AutoImportApply))
		reconcileScope()

		Expect(importFor("vpc-0fee1dead")).To(BeNil(), "Terraform owns it; tagging it would fight the next plan")
		Expect(importFor("subnet-0abc1111")).NotTo(BeNil(), "the rest is unaffected")
	})

	// autoImportSeries reads hs_auto_imports_total for this scope, by result — the counter behind
	// the AutoImportedResources digest.
	autoImportSeries := func() map[string]float64 {
		GinkgoHelper()
		families, err := ctrlmetrics.Registry.Gather()
		Expect(err).NotTo(HaveOccurred())
		out := map[string]float64{}
		for _, f := range families {
			if f.GetName() != "hs_auto_imports_total" {
				continue
			}
			for _, m := range f.GetMetric() {
				labels := map[string]string{}
				for _, l := range m.GetLabel() {
					labels[l.GetName()] = l.GetValue()
				}
				if labels["scope"] == scopeName {
					Expect(labels).To(HaveKeyWithValue("account", autoAccount))
					out[labels["result"]] = m.GetCounter().GetValue()
				}
			}
		}
		return out
	}

	// The digest fires on an increase. A series that first appears at 1 has not increased as far
	// as Prometheus can tell, so before this the first import after a restart never made it.
	It("exports every auto-import result at zero before the policy decides anything", func() {
		discoverer.snapshots[autoAccount+"/"+autoRegion] = &inventory.Snapshot{}
		createScope(policy(networkv1beta1.AutoImportApply))
		reconcileScope()
		Expect(autoImportSeries()).To(Equal(map[string]float64{
			"applied": 0, "dryrun": 0, "skipped": 0, "no_owner": 0}))
	})

	It("exports no auto-import series for a scope without a policy", func() {
		createScope(nil)
		reconcileScope()
		Expect(autoImportSeries()).To(BeEmpty())
	})
})
