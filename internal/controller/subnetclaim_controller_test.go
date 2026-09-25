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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

// fakeWriter records subnet creations and can fail on demand.
type fakeWriter struct {
	mu       sync.Mutex
	requests []inventory.CreateSubnetRequest
	targets  []inventory.Target
	// conflictOnce makes the first creation of this CIDR report a conflict.
	conflictOnce map[string]bool
	failWith     error
	next         int
}

func (f *fakeWriter) CreateSubnet(_ context.Context, t inventory.Target, req inventory.CreateSubnetRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return "", f.failWith
	}
	if f.conflictOnce[req.CIDRBlock] {
		delete(f.conflictOnce, req.CIDRBlock)
		return "", fmt.Errorf("%w: InvalidSubnet.Conflict", inventory.ErrCIDRConflict)
	}
	f.requests = append(f.requests, req)
	f.targets = append(f.targets, t)
	f.next++
	return fmt.Sprintf("subnet-new%d", f.next), nil
}

var claimCounter int

var _ = Describe("SubnetClaim Controller", func() {
	const (
		claimAccount = "111111111111"
		spokeAccount = "222222222222"
		claimRegion  = "eu-central-1"
	)
	var (
		scopeName  string
		claimName  string
		claimVPC   string
		labels     map[string]string
		writer     *fakeWriter
		notified   []inventory.TargetKey
		reconciler *SubnetClaimReconciler
	)

	reconcileClaim := func() error {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: claimName, Namespace: "default"}})
		return err
	}
	getClaim := func() *networkv1.SubnetClaim {
		GinkgoHelper()
		c := &networkv1.SubnetClaim{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: claimName, Namespace: "default"}, c)).To(Succeed())
		return c
	}
	createClaim := func(mutate func(*networkv1.SubnetClaim)) {
		GinkgoHelper()
		c := &networkv1.SubnetClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: "default"},
			Spec: networkv1.SubnetClaimSpec{
				ScopeRef: scopeName, Account: claimAccount, Region: claimRegion, NetworkID: claimVPC,
				PrefixLength: 24, Zones: []string{claimRegion + "a", claimRegion + "b"},
				Mode: networkv1.ClaimModeCreate, Owner: "team-payments", Env: "prod", Tier: "private",
				Tags: map[string]string{"cost-center": "cc-42", "hs/owner": "someone-else"},
			},
		}
		if mutate != nil {
			mutate(c)
		}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
	}
	createSubnet := func(id, cidr, az string, tags map[string]string) {
		GinkgoHelper()
		sn := &networkv1.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("%s-%d", id, claimCounter), Labels: labels},
			Spec:       networkv1.SubnetSpec{Provider: networkv1.ProviderAWS, ID: id, NetworkID: claimVPC, Account: claimAccount, Region: claimRegion},
		}
		Expect(k8sClient.Create(ctx, sn)).To(Succeed())
		sn.Status = networkv1.SubnetStatus{CIDRBlock: cidr, Zone: az, Tags: tags}
		Expect(k8sClient.Status().Update(ctx, sn)).To(Succeed())
	}
	cond := func(c *networkv1.SubnetClaim, typ string) *metav1.Condition {
		return meta.FindStatusCondition(c.Status.Conditions, typ)
	}

	BeforeEach(func() {
		claimCounter++
		scopeName = fmt.Sprintf("claim-scope-%d", claimCounter)
		claimName = fmt.Sprintf("claim-%d", claimCounter)
		claimVPC = fmt.Sprintf("vpc-c1a1%04d", claimCounter)
		labels = map[string]string{networkv1.LabelScope: scopeName, networkv1.LabelAccount: claimAccount,
			networkv1.LabelRegion: claimRegion, networkv1.LabelNetwork: claimVPC}
		writer = &fakeWriter{conflictOnce: map[string]bool{}}
		notified = nil
		reconciler = &SubnetClaimReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Providers: awsProviders(nil, writer, nil), WritesEnabled: true,
			Notify: func(_ context.Context, keys []inventory.TargetKey) error {
				notified = append(notified, keys...)
				return nil
			},
		}

		Expect(k8sClient.Create(ctx, &networkv1.NetworkScope{
			ObjectMeta: metav1.ObjectMeta{Name: scopeName},
			Spec: networkv1.NetworkScopeSpec{
				Provider:          networkv1.ProviderAWS,
				NamespaceSelector: &metav1.LabelSelector{},
				Accounts: []networkv1.Account{
					{ID: claimAccount},
					{ID: spokeAccount, AWS: &networkv1.AWSAccount{RoleARN: "arn:aws:iam::" + spokeAccount + ":role/aws-subnet-operator-readonly"}},
				},
				Regions: []string{claimRegion},
			},
		})).To(Succeed())

		vpc := &networkv1.Network{
			ObjectMeta: metav1.ObjectMeta{Name: claimVPC, Labels: labels},
			Spec:       networkv1.NetworkSpec{Provider: networkv1.ProviderAWS, ID: claimVPC, Account: claimAccount, Region: claimRegion},
		}
		Expect(k8sClient.Create(ctx, vpc)).To(Succeed())
		vpc.Status.CIDRBlocks = []string{"10.50.0.0/16"}
		Expect(k8sClient.Status().Update(ctx, vpc)).To(Succeed())

		// Two subnets already exist in the VPC.
		createSubnet("subnet-exist0", "10.50.0.0/24", claimRegion+"a", nil)
		createSubnet("subnet-exist1", "10.50.1.0/24", claimRegion+"b", nil)
	})

	AfterEach(func() {
		Expect(k8sClient.DeleteAllOf(ctx, &networkv1.Subnet{}, client.MatchingLabels{networkv1.LabelScope: scopeName})).To(Succeed())
		Expect(k8sClient.DeleteAllOf(ctx, &networkv1.Network{}, client.MatchingLabels{networkv1.LabelScope: scopeName})).To(Succeed())
		Expect(k8sClient.DeleteAllOf(ctx, &networkv1.SubnetClaim{}, client.InNamespace("default"))).To(Succeed())
		Expect(k8sClient.Delete(ctx, &networkv1.NetworkScope{ObjectMeta: metav1.ObjectMeta{Name: scopeName}})).To(Succeed())
	})

	It("reserves free CIDRs and creates tagged subnets", func() {
		createClaim(nil)
		Expect(reconcileClaim()).To(Succeed())

		c := getClaim()
		Expect(cond(c, ConditionAllocated).Status).To(Equal(metav1.ConditionTrue))
		Expect(cond(c, ConditionReady).Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Status.Allocations).To(HaveLen(2))
		Expect(c.Status.Allocations[0].Zone).To(Equal(claimRegion + "a"))
		Expect(c.Status.Allocations[0].CIDRBlock).To(Equal("10.50.2.0/24"), "the first free block after the existing subnets")
		Expect(c.Status.Allocations[1].CIDRBlock).To(Equal("10.50.3.0/24"))
		Expect(c.Status.Allocations[0].SubnetID).To(Equal("subnet-new1"))
		Expect(c.Status.Allocations[0].State).To(Equal(networkv1.AllocationCreated))

		Expect(writer.requests).To(HaveLen(2))
		req := writer.requests[0]
		Expect(req.NetworkID).To(Equal(claimVPC))
		Expect(req.Tags).To(HaveKeyWithValue("Name", claimName+"-a"))
		Expect(req.Tags).To(HaveKeyWithValue("hs/owner", "team-payments"), "the claim's owner wins over spec.tags")
		Expect(req.Tags).To(HaveKeyWithValue("hs/env", "prod"))
		Expect(req.Tags).To(HaveKeyWithValue("hs/tier", "private"))
		Expect(req.Tags).To(HaveKeyWithValue("cost-center", "cc-42"))
		Expect(req.Tags).To(HaveKeyWithValue(networkv1.TagManagedBy, networkv1.TagManagedByValue))
		Expect(req.Tags).To(HaveKeyWithValue(networkv1.TagClaim, "default/"+claimName))
		Expect(writer.targets[0].OwnIdentity()).To(BeTrue(), "the operator's own account uses its own credentials")
		Expect(notified).To(Equal([]inventory.TargetKey{{Account: claimAccount, Region: claimRegion}}))

		By("a second pass creates nothing more")
		Expect(reconcileClaim()).To(Succeed())
		Expect(writer.requests).To(HaveLen(2))
	})

	It("only reserves CIDRs in Allocate mode", func() {
		createClaim(func(c *networkv1.SubnetClaim) { c.Spec.Mode = networkv1.ClaimModeAllocate })
		Expect(reconcileClaim()).To(Succeed())
		c := getClaim()
		Expect(cond(c, ConditionReady).Status).To(Equal(metav1.ConditionTrue))
		Expect(cond(c, ConditionReady).Reason).To(Equal("Allocated"))
		Expect(c.Status.Allocations[0].State).To(Equal(networkv1.AllocationPending))
		Expect(writer.requests).To(BeEmpty())
	})

	It("refuses to create while writes are disabled", func() {
		reconciler.WritesEnabled = false
		createClaim(nil)
		Expect(reconcileClaim()).To(Succeed())
		c := getClaim()
		Expect(cond(c, ConditionAllocated).Status).To(Equal(metav1.ConditionTrue), "allocation still happens")
		Expect(cond(c, ConditionReady).Reason).To(Equal("WritesDisabled"))
		Expect(writer.requests).To(BeEmpty())

		By("and a claim that cannot be fulfilled is visible to Prometheus, not only in its status")
		Expect(readySeries("hs_subnet_claim_ready", claimName)).To(
			Equal(map[string]float64{"WritesDisabled": 0}))
	})

	It("takes reservations of other claims into account", func() {
		other := &networkv1.SubnetClaim{
			ObjectMeta: metav1.ObjectMeta{Name: claimName + "-other", Namespace: "default"},
			Spec: networkv1.SubnetClaimSpec{ScopeRef: scopeName, Account: claimAccount, Region: claimRegion,
				NetworkID: claimVPC, PrefixLength: 24, Zones: []string{claimRegion + "a"}, Owner: "x"},
		}
		Expect(k8sClient.Create(ctx, other)).To(Succeed())
		other.Status.Allocations = []networkv1.SubnetAllocation{{Name: "alloc-" + claimRegion + "a", Zone: claimRegion + "a", CIDRBlock: "10.50.2.0/24", State: networkv1.AllocationPending}}
		Expect(k8sClient.Status().Update(ctx, other)).To(Succeed())

		createClaim(nil)
		Expect(reconcileClaim()).To(Succeed())
		c := getClaim()
		Expect(c.Status.Allocations[0].CIDRBlock).To(Equal("10.50.3.0/24"), "10.50.2.0/24 is reserved by the other claim")
	})

	It("reallocates after a CIDR conflict", func() {
		writer.conflictOnce["10.50.2.0/24"] = true
		createClaim(nil)

		Expect(reconcileClaim()).To(Succeed())
		c := getClaim()
		Expect(cond(c, ConditionReady).Reason).To(Equal("CreateFailed"))
		Expect(c.Status.Allocations).To(HaveLen(1), "the conflicting reservation was dropped")
		Expect(readySeries("hs_subnet_claim_ready", claimName)).To(
			Equal(map[string]float64{"CreateFailed": 0}))

		// The winner shows up in the inventory before the next pass.
		createSubnet("subnet-winner", "10.50.2.0/24", claimRegion+"c", nil)

		Expect(reconcileClaim()).To(Succeed())
		c = getClaim()
		Expect(cond(c, ConditionReady).Status).To(Equal(metav1.ConditionTrue))
		cidrs := []string{c.Status.Allocations[0].CIDRBlock, c.Status.Allocations[1].CIDRBlock}
		Expect(cidrs).To(ConsistOf("10.50.3.0/24", "10.50.4.0/24"))

		By("the recovered claim reports ready, and the old failure is not left behind as a series")
		Expect(readySeries("hs_subnet_claim_ready", claimName)).To(
			Equal(map[string]float64{cond(c, ConditionReady).Reason: 1}))
	})

	It("stops reporting a claim that has been deleted", func() {
		reconciler.WritesEnabled = false
		createClaim(nil)
		Expect(reconcileClaim()).To(Succeed())
		Expect(readySeries("hs_subnet_claim_ready", claimName)).NotTo(BeEmpty())

		Expect(k8sClient.Delete(ctx, getClaim())).To(Succeed())
		Expect(reconcileClaim()).To(Succeed())
		Expect(readySeries("hs_subnet_claim_ready", claimName)).To(BeEmpty(),
			"a deleted claim must not keep an alert firing")
	})

	It("keeps the reservation when creation fails for another reason", func() {
		writer.failWith = errors.New("UnauthorizedOperation: not allowed to CreateSubnet")
		createClaim(nil)
		Expect(reconcileClaim()).To(Succeed())
		c := getClaim()
		Expect(cond(c, ConditionReady).Reason).To(Equal("CreateFailed"))
		Expect(c.Status.Allocations[0].State).To(Equal(networkv1.AllocationFailed))
		Expect(c.Status.Allocations[0].Error).To(ContainSubstring("UnauthorizedOperation"))
		Expect(c.Status.Allocations[0].CIDRBlock).To(Equal("10.50.2.0/24"), "the reservation is kept for the retry")
	})

	It("adopts subnets that already carry the claim tag", func() {
		createSubnet("subnet-adopt", "10.50.9.0/24", claimRegion+"a",
			map[string]string{networkv1.TagClaim: "default/" + claimName})

		createClaim(nil)
		Expect(reconcileClaim()).To(Succeed())
		c := getClaim()
		a := c.Status.Allocations[0]
		Expect(a.SubnetID).To(Equal("subnet-adopt"))
		Expect(a.CIDRBlock).To(Equal("10.50.9.0/24"))
		Expect(a.State).To(Equal(networkv1.AllocationCreated))
		Expect(writer.requests).To(HaveLen(1), "only the other AZ is created")
		Expect(writer.requests[0].Zone).To(Equal(claimRegion + "b"))
	})

	It("reports a VPC that was not discovered", func() {
		createClaim(func(c *networkv1.SubnetClaim) { c.Spec.NetworkID = "vpc-0deadbeef0" })
		Expect(reconcileClaim()).To(Succeed())
		Expect(cond(getClaim(), ConditionReady).Reason).To(Equal("NetworkNotFound"))
		Expect(writer.requests).To(BeEmpty())
	})

	It("allocates but does not create for a spoke without a write role", func() {
		spokeVPC := fmt.Sprintf("vpc-5b0ce%04d", claimCounter)
		spokeLabels := map[string]string{networkv1.LabelScope: scopeName, networkv1.LabelAccount: spokeAccount,
			networkv1.LabelRegion: claimRegion, networkv1.LabelNetwork: spokeVPC}
		vpc := &networkv1.Network{
			ObjectMeta: metav1.ObjectMeta{Name: spokeVPC, Labels: spokeLabels},
			Spec:       networkv1.NetworkSpec{Provider: networkv1.ProviderAWS, ID: spokeVPC, Account: spokeAccount, Region: claimRegion},
		}
		Expect(k8sClient.Create(ctx, vpc)).To(Succeed())
		vpc.Status.CIDRBlocks = []string{"10.60.0.0/16"}
		Expect(k8sClient.Status().Update(ctx, vpc)).To(Succeed())

		createClaim(func(c *networkv1.SubnetClaim) { c.Spec.Account = spokeAccount; c.Spec.NetworkID = spokeVPC })
		Expect(reconcileClaim()).To(Succeed())
		c := getClaim()
		Expect(cond(c, ConditionAllocated).Status).To(Equal(metav1.ConditionTrue))
		Expect(c.Status.Allocations[0].CIDRBlock).To(Equal("10.60.0.0/24"))
		Expect(cond(c, ConditionReady).Reason).To(Equal("NoWriteRole"))
		Expect(writer.requests).To(BeEmpty())
	})

	It("refuses a claim without zones, which every AWS subnet needs", func() {
		createClaim(func(c *networkv1.SubnetClaim) { c.Spec.Zones = nil })
		Expect(reconcileClaim()).To(Succeed())
		Expect(cond(getClaim(), ConditionReady).Reason).To(Equal("ZonesRequired"))
		Expect(getClaim().Status.Allocations).To(BeEmpty())
	})

	It("refuses a claim whose scope's provider the operator does not run", func() {
		reconciler.Providers = provider.MustRegistry()
		createClaim(nil)
		Expect(reconcileClaim()).To(Succeed())
		Expect(cond(getClaim(), ConditionReady).Reason).To(Equal(ReasonProviderNotEnabled))
		Expect(getClaim().Status.Allocations).To(BeEmpty())
		Expect(writer.requests).To(BeEmpty())
	})

	It("refuses a prefix length AWS does not accept", func() {
		createClaim(func(c *networkv1.SubnetClaim) { c.Spec.PrefixLength = 30 })
		Expect(reconcileClaim()).To(Succeed())
		Expect(cond(getClaim(), ConditionReady).Reason).To(Equal("InvalidPrefixLength"))
		Expect(writer.requests).To(BeEmpty())
	})
})
