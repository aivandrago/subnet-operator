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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
	"hypersurgery.dev/subnet-operator/internal/events"
	"hypersurgery.dev/subnet-operator/internal/inventory"
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
			VPCs: []inventory.VPC{{ID: "vpc-0aa11bb2", Account: autoAccount, Region: autoRegion,
				CIDRBlocks: []string{"10.0.0.0/16"},
				Tags:       map[string]string{"hs/managed": "true", "hs/owner": "team-platform", "hs/env": "prod"}}},
			UnmanagedVPCs: []inventory.VPC{{ID: "vpc-0fee1dead", Account: autoAccount, Region: autoRegion,
				CIDRBlocks: []string{"10.90.0.0/16"}, Tags: map[string]string{"Name": "legacy"}}},
			UnmanagedSubnets: []inventory.Subnet{
				{ID: "subnet-0abc1111", VPCID: "vpc-0aa11bb2", Account: autoAccount, Region: autoRegion, CIDRBlock: "10.0.9.0/24"},
				{ID: "subnet-0def2222", VPCID: "vpc-0fee1dead", Account: autoAccount, Region: autoRegion, CIDRBlock: "10.90.1.0/24"},
			},
		}
	}

	reconcileScope := func() {
		GinkgoHelper()
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: scopeName}})
		Expect(err).NotTo(HaveOccurred())
	}
	imports := func() []awsv1alpha1.ResourceImport {
		GinkgoHelper()
		list := &awsv1alpha1.ResourceImportList{}
		Expect(k8sClient.List(ctx, list, client.InNamespace("default"),
			client.MatchingLabels{awsv1alpha1.LabelScope: scopeName})).To(Succeed())
		return list.Items
	}
	importFor := func(resourceID string) *awsv1alpha1.ResourceImport {
		GinkgoHelper()
		for _, imp := range imports() {
			if imp.Spec.ResourceID == resourceID {
				return &imp
			}
		}
		return nil
	}
	createScope := func(policy *awsv1alpha1.AutoImportPolicy) {
		GinkgoHelper()
		Expect(k8sClient.Create(ctx, &awsv1alpha1.NetworkScope{
			ObjectMeta: metav1.ObjectMeta{Name: scopeName},
			Spec: awsv1alpha1.NetworkScopeSpec{
				Accounts:       []awsv1alpha1.AccountSpec{{ID: autoAccount}},
				Regions:        []string{autoRegion},
				VPCTagSelector: map[string]string{"hs/managed": "true"},
				AutoImport:     policy,
			},
		})).To(Succeed())
	}
	policy := func(mode awsv1alpha1.AutoImportMode) *awsv1alpha1.AutoImportPolicy {
		return &awsv1alpha1.AutoImportPolicy{
			Mode: mode,
			FromCreator: []awsv1alpha1.CreatorRule{
				{PrincipalPrefix: "arn:aws:sts::111111111111:assumed-role/payments-",
					Tags: map[string]string{"hs/owner": "team-payments"}},
			},
			InheritFromVPC: []string{"hs/owner", "hs/env"},
			Skip:           []awsv1alpha1.SkipRule{{PrincipalPrefix: "arn:aws:sts::111111111111:assumed-role/terraform-"}},
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
			Client: k8sClient, Scheme: k8sClient.Scheme(), Discoverer: discoverer, Creators: creators,
		}
	})

	AfterEach(func() {
		for _, imp := range imports() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &imp))).To(Succeed())
		}
		Expect(k8sClient.DeleteAllOf(ctx, &awsv1alpha1.Subnet{}, client.MatchingLabels{awsv1alpha1.LabelScope: scopeName})).To(Succeed())
		Expect(k8sClient.DeleteAllOf(ctx, &awsv1alpha1.VPC{}, client.MatchingLabels{awsv1alpha1.LabelScope: scopeName})).To(Succeed())
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &awsv1alpha1.NetworkScope{
			ObjectMeta: metav1.ObjectMeta{Name: scopeName}}))).To(Succeed())
	})

	It("counts unmanaged resources without importing anything when the policy is off", func() {
		createScope(nil)
		reconcileScope()

		scope := &awsv1alpha1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, scope)).To(Succeed())
		Expect(scope.Status.Unmanaged).To(Equal(int32(3)), "one VPC and two subnets")
		Expect(scope.Status.Targets[0].UnmanagedVPCs).To(Equal(int32(1)))
		Expect(scope.Status.Targets[0].UnmanagedSubnets).To(Equal(int32(2)))
		Expect(imports()).To(BeEmpty())

		By("and the unmanaged resources are not mirrored as objects")
		subnets := &awsv1alpha1.SubnetList{}
		Expect(k8sClient.List(ctx, subnets, client.MatchingLabels{awsv1alpha1.LabelScope: scopeName})).To(Succeed())
		for _, s := range subnets.Items {
			Expect(s.Spec.SubnetID).NotTo(Equal("subnet-0def2222"))
		}
	})

	It("imports what it can attribute and leaves the rest alone", func() {
		creators.Record(context.Background(), []events.Creation{{
			Target:     inventory.TargetKey{Account: autoAccount, Region: autoRegion},
			ResourceID: "vpc-0fee1dead", Principal: paymentsRole, EventName: "CreateVpc",
		}})
		createScope(policy(awsv1alpha1.AutoImportApply))
		reconcileScope()

		By("the VPC gets the creator's owner")
		vpcImport := importFor("vpc-0fee1dead")
		Expect(vpcImport).NotTo(BeNil())
		Expect(vpcImport.Spec.Tags).To(Equal(map[string]string{"hs/owner": "team-payments", "hs/managed": "true"}))
		Expect(vpcImport.Spec.DryRun).To(BeFalse())
		Expect(vpcImport.Spec.RequestedBy).To(ContainSubstring(awsv1alpha1.RequestedByPolicy))
		Expect(vpcImport.Spec.RequestedBy).To(ContainSubstring("maria.k"), "the person is worth recording")
		Expect(vpcImport.Labels).To(HaveKeyWithValue(awsv1alpha1.LabelResource, "vpc-0fee1dead"))

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
		creators.Record(context.Background(), []events.Creation{{
			ResourceID: "vpc-0fee1dead", Principal: paymentsRole, EventName: "CreateVpc",
		}})
		createScope(policy(awsv1alpha1.AutoImportDryRun))
		reconcileScope()

		imp := importFor("vpc-0fee1dead")
		Expect(imp).NotTo(BeNil())
		Expect(imp.Spec.DryRun).To(BeTrue())
	})

	It("skips what a pipeline created", func() {
		creators.Record(context.Background(), []events.Creation{{
			ResourceID: "vpc-0fee1dead", Principal: terraformCI, EventName: "CreateVpc",
		}})
		createScope(policy(awsv1alpha1.AutoImportApply))
		reconcileScope()

		Expect(importFor("vpc-0fee1dead")).To(BeNil(), "Terraform owns it; tagging it would fight the next plan")
		Expect(importFor("subnet-0abc1111")).NotTo(BeNil(), "the rest is unaffected")
	})
})
