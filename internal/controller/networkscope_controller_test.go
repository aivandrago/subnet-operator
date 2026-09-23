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
	"errors"
	"fmt"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// fakeDiscoverer returns a fixed snapshot or error per "account/region".
type fakeDiscoverer struct {
	mu        sync.Mutex
	snapshots map[string]*inventory.Snapshot
	errs      map[string]error
	calls     map[string]int
}

func (f *fakeDiscoverer) Discover(_ context.Context, t inventory.Target) (*inventory.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := t.Account + "/" + t.Region
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[key]++
	if err := f.errs[key]; err != nil {
		return nil, err
	}
	if s := f.snapshots[key]; s != nil {
		return s, nil
	}
	return &inventory.Snapshot{}, nil
}

const (
	accountA = "111111111111"
	accountB = "222222222222"
	region   = "eu-central-1"
)

func snapshotA() *inventory.Snapshot {
	return &inventory.Snapshot{
		VPCs: []inventory.VPC{{ID: "vpc-aaa", Account: accountA, Region: region, State: "available",
			CIDRBlocks: []string{"10.0.0.0/16"}, Tags: map[string]string{"Name": "prod", "hs/owner": "platform"}}},
		Subnets: []inventory.Subnet{
			{ID: "subnet-a1", VPCID: "vpc-aaa", Account: accountA, Region: region, State: "available",
				CIDRBlock: "10.0.1.0/24", AvailabilityZone: "eu-central-1a", AvailableIPs: 51, Public: true,
				Tags: map[string]string{"hs/owner": "team-a", "hs/env": "prod", "hs/tier": "public"}},
			{ID: "subnet-a2", VPCID: "vpc-aaa", Account: accountA, Region: region, State: "available",
				CIDRBlock: "10.0.2.0/24", AvailabilityZone: "eu-central-1b", AvailableIPs: 251},
		},
	}
}

func snapshotB() *inventory.Snapshot {
	return &inventory.Snapshot{
		VPCs: []inventory.VPC{{ID: "vpc-bbb", Account: accountB, Region: region, State: "available",
			CIDRBlocks: []string{"10.0.128.0/20"}}},
	}
}

var scopeCounter int

var _ = Describe("NetworkScope Controller", func() {
	var (
		scopeName  string
		discoverer *fakeDiscoverer
		reconciler *NetworkScopeReconciler
	)

	reconcileScope := func() time.Duration {
		GinkgoHelper()
		res, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: scopeName}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		Expect(res.RequeueAfter).To(BeNumerically("<=", 5*time.Minute))
		return res.RequeueAfter
	}

	notify := func(accounts ...string) {
		GinkgoHelper()
		keys := make([]inventory.TargetKey, 0, len(accounts))
		for _, a := range accounts {
			keys = append(keys, inventory.TargetKey{Account: a, Region: region})
		}
		Expect(reconciler.NotifyChanged(ctx, keys)).To(Succeed())
	}

	getScope := func() *awsv1alpha1.NetworkScope {
		GinkgoHelper()
		s := &awsv1alpha1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, s)).To(Succeed())
		return s
	}

	BeforeEach(func() {
		scopeCounter++
		scopeName = fmt.Sprintf("scope-%d", scopeCounter)
		discoverer = &fakeDiscoverer{
			snapshots: map[string]*inventory.Snapshot{accountA + "/" + region: snapshotA(), accountB + "/" + region: snapshotB()},
			errs:      map[string]error{},
		}
		reconciler = &NetworkScopeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), Discoverer: discoverer}

		scope := &awsv1alpha1.NetworkScope{
			ObjectMeta: metav1.ObjectMeta{Name: scopeName},
			Spec: awsv1alpha1.NetworkScopeSpec{
				Accounts: []awsv1alpha1.AccountSpec{
					{ID: accountA},
					{ID: accountB, RoleARN: "arn:aws:iam::" + accountB + ":role/aws-subnet-operator-readonly"},
				},
				Regions:            []string{region},
				RequiredSubnetTags: []string{"hs/owner", "hs/env"},
				ResyncInterval:     &metav1.Duration{Duration: 5 * time.Minute},
			},
		}
		Expect(k8sClient.Create(ctx, scope)).To(Succeed())
	})

	AfterEach(func() {
		// envtest runs no garbage collector, so children are removed explicitly.
		Expect(k8sClient.DeleteAllOf(ctx, &awsv1alpha1.Subnet{}, client.MatchingLabels{awsv1alpha1.LabelScope: scopeName})).To(Succeed())
		Expect(k8sClient.DeleteAllOf(ctx, &awsv1alpha1.VPC{}, client.MatchingLabels{awsv1alpha1.LabelScope: scopeName})).To(Succeed())
		Expect(k8sClient.Delete(ctx, getScope())).To(Succeed())
	})

	It("mirrors VPCs and subnets with inventory data and compliance findings", func() {
		reconcileScope()

		s := &awsv1alpha1.Subnet{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "subnet-a1"}, s)).To(Succeed())
		Expect(s.Spec).To(Equal(awsv1alpha1.SubnetSpec{SubnetID: "subnet-a1", VPCID: "vpc-aaa", Account: accountA, Region: region}))
		Expect(s.Labels).To(HaveKeyWithValue(awsv1alpha1.LabelScope, scopeName))
		Expect(s.Labels).To(HaveKeyWithValue(awsv1alpha1.LabelAccount, accountA))
		Expect(s.OwnerReferences).To(HaveLen(1))
		Expect(s.OwnerReferences[0].Name).To(Equal(scopeName))
		Expect(s.Status.CIDRBlock).To(Equal("10.0.1.0/24"))
		Expect(s.Status.TotalIPs).To(Equal(int64(251)))
		Expect(s.Status.AvailableIPs).To(Equal(int64(51)))
		Expect(s.Status.UtilizationPercent).To(Equal(int32(79)))
		Expect(s.Status.Public).To(BeTrue())
		Expect(s.Status.Owner).To(Equal("team-a"))
		Expect(s.Status.Tier).To(Equal("public"))
		Expect(s.Status.MissingTags).To(BeEmpty())

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "subnet-a2"}, s)).To(Succeed())
		Expect(s.Status.MissingTags).To(Equal([]string{"hs/owner", "hs/env"}))

		v := &awsv1alpha1.VPC{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "vpc-aaa"}, v)).To(Succeed())
		Expect(v.Status.Name).To(Equal("prod"))
		Expect(v.Status.Owner).To(Equal("platform"))
		Expect(v.Status.Subnets).To(Equal(int32(2)))
		Expect(v.Status.TotalIPs).To(Equal(int64(502)))
		Expect(v.Status.AvailableIPs).To(Equal(int64(302)))
		Expect(v.Status.OverlapsWith).To(Equal([]string{accountB + "/" + region + "/vpc-bbb"}))

		scope := getScope()
		Expect(scope.Status.VPCs).To(Equal(int32(2)))
		Expect(scope.Status.Subnets).To(Equal(int32(2)))
		Expect(scope.Status.Targets).To(HaveLen(2))
		Expect(meta.IsStatusConditionTrue(scope.Status.Conditions, ConditionReady)).To(BeTrue())
	})

	It("deletes objects that disappeared from AWS but keeps objects of failed targets", func() {
		reconcileScope()
		firstSync := getScope().Status.Targets[1].LastSyncTime

		discoverer.mu.Lock()
		snap := snapshotA()
		snap.Subnets = snap.Subnets[:1]
		discoverer.snapshots[accountA+"/"+region] = snap
		discoverer.errs[accountB+"/"+region] = errors.New("AccessDenied")
		discoverer.mu.Unlock()
		notify(accountA, accountB)
		reconcileScope()

		err := k8sClient.Get(ctx, types.NamespacedName{Name: "subnet-a2"}, &awsv1alpha1.Subnet{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "subnet-a2 is gone from AWS")
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "vpc-bbb"}, &awsv1alpha1.VPC{})).To(Succeed(),
			"a failed discovery must not delete the account's objects")

		scope := getScope()
		cond := meta.FindStatusCondition(scope.Status.Conditions, ConditionReady)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Message).To(ContainSubstring(accountB))
		b := scope.Status.Targets[1]
		Expect(b.Error).To(Equal("AccessDenied"))
		Expect(b.VPCs).To(Equal(int32(1)))
		Expect(b.LastSyncTime.Equal(firstSync)).To(BeTrue())
	})

	It("syncs only the targets reported changed until the next full sync", func() {
		Expect(reconcileScope()).To(Equal(5 * time.Minute))
		fullSync := getScope().Status.LastSyncTime

		By("reconciling without changes: nothing is discovered")
		Expect(reconcileScope()).To(BeNumerically("<", 5*time.Minute))
		Expect(discoverer.calls[accountA+"/"+region]).To(Equal(1))

		By("changing both accounts in AWS but reporting only account A")
		discoverer.mu.Lock()
		snap := snapshotA()
		snap.Subnets = append(snap.Subnets, inventory.Subnet{ID: "subnet-a3", VPCID: "vpc-aaa", Account: accountA,
			Region: region, CIDRBlock: "10.0.3.0/24", AvailableIPs: 251})
		discoverer.snapshots[accountA+"/"+region] = snap
		discoverer.snapshots[accountB+"/"+region] = &inventory.Snapshot{}
		discoverer.mu.Unlock()
		notify(accountA, "999999999999")
		reconcileScope()

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "subnet-a3"}, &awsv1alpha1.Subnet{})).To(Succeed())
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "vpc-bbb"}, &awsv1alpha1.VPC{})).To(Succeed(),
			"account B was not reported changed and must not be synced before the next full sync")
		Expect(discoverer.calls[accountB+"/"+region]).To(Equal(1))

		scope := getScope()
		Expect(scope.Status.LastSyncTime.Equal(fullSync)).To(BeTrue(), "a partial sync does not move the full-sync time")
		Expect(scope.Status.Subnets).To(Equal(int32(3)))
		Expect(scope.Status.Targets).To(HaveLen(2))
		Expect(scope.Status.Targets[0].Subnets).To(Equal(int32(3)))
		Expect(scope.Status.Targets[1].VPCs).To(Equal(int32(1)))
	})

	It("deletes objects of accounts removed from the scope", func() {
		reconcileScope()
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "vpc-bbb"}, &awsv1alpha1.VPC{})).To(Succeed())

		scope := getScope()
		scope.Spec.Accounts = scope.Spec.Accounts[:1]
		Expect(k8sClient.Update(ctx, scope)).To(Succeed())
		reconcileScope()

		err := k8sClient.Get(ctx, types.NamespacedName{Name: "vpc-bbb"}, &awsv1alpha1.VPC{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "vpc-bbb belongs to the removed account")
		v := &awsv1alpha1.VPC{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "vpc-aaa"}, v)).To(Succeed())
		Expect(v.Status.OverlapsWith).To(BeEmpty(), "the overlapping VPC is gone")
		Expect(getScope().Status.Targets).To(HaveLen(1))
	})

	It("does not take over objects owned by another scope", func() {
		foreign := &awsv1alpha1.VPC{
			ObjectMeta: metav1.ObjectMeta{Name: "vpc-bbb", Labels: map[string]string{awsv1alpha1.LabelScope: "other"}},
			Spec:       awsv1alpha1.VPCSpec{VPCID: "vpc-bbb", Account: accountB, Region: region},
		}
		Expect(k8sClient.Create(ctx, foreign)).To(Succeed())
		DeferCleanup(func() { Expect(k8sClient.Delete(ctx, foreign)).To(Succeed()) })

		reconcileScope()

		v := &awsv1alpha1.VPC{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "vpc-bbb"}, v)).To(Succeed())
		Expect(v.Labels).To(HaveKeyWithValue(awsv1alpha1.LabelScope, "other"))
		Expect(v.OwnerReferences).To(BeEmpty())
	})
})
