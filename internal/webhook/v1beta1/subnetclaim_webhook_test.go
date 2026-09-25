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

package v1beta1

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

const (
	claimAccount = "200000000001"
	claimRegion  = "eu-central-1"
	claimScope   = "claims"
	claimVPC     = "vpc-0aaa1"
)

// claimIn builds a claim for the fixture VPC, which every spec then breaks in one way.
func claimIn(name string, prefixLength int32, zones ...string) *networkv1beta1.SubnetClaim {
	if len(zones) == 0 {
		zones = []string{claimRegion + "a"}
	}
	return &networkv1beta1.SubnetClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: networkv1beta1.SubnetClaimSpec{
			ScopeRef:     claimScope,
			Account:      claimAccount,
			Region:       claimRegion,
			NetworkID:    claimVPC,
			PrefixLength: prefixLength,
			Zones:        zones,
			Mode:         networkv1beta1.ClaimModeAllocate,
			Owner:        "payments",
		},
	}
}

// ensureClaimNetwork makes sure the fixture network, a 10.0.0.0/22, is in the inventory.
func ensureClaimNetwork() {
	GinkgoHelper()
	network := &networkv1beta1.Network{
		ObjectMeta: metav1.ObjectMeta{Name: claimVPC},
		Spec:       networkv1beta1.NetworkSpec{Provider: networkv1beta1.ProviderAWS, ID: claimVPC, Account: claimAccount, Region: claimRegion},
	}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: claimVPC}, &networkv1beta1.Network{}); err != nil {
		mustCreate(network)
		network.Status.CIDRBlocks = []string{"10.0.0.0/22"}
		Expect(k8sClient.Status().Update(ctx, network)).To(Succeed())
	}
}

var _ = Describe("SubnetClaim webhook", func() {
	var created []client.Object

	BeforeEach(func() {
		By("a scope and a discovered VPC with a 10.0.0.0/22 block")
		s := scope(claimScope, claimAccount, claimRegion)
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: claimScope}, &networkv1beta1.NetworkScope{}); err != nil {
			mustCreate(s)
		}
		ensureClaimNetwork()
	})

	AfterEach(func() {
		for _, obj := range created {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
		created = nil
	})

	It("names the subnets after the claim when no prefix was given", func() {
		obj := claimIn("payments-db", 24)
		mustCreate(obj)
		created = append(created, obj)

		Expect(obj.Spec.NamePrefix).To(Equal("payments-db"))
	})

	It("refuses a claim whose NetworkScope does not exist", func() {
		obj := claimIn("no-scope", 24)
		obj.Spec.ScopeRef = "nowhere"
		expectDenied(obj, "spec.scopeRef")
	})

	It("refuses an account the scope does not discover", func() {
		obj := claimIn("wrong-account", 24)
		obj.Spec.Account = "200000000099"
		expectDenied(obj, "is not discovered by NetworkScope")
	})

	It("refuses an availability zone from another region", func() {
		obj := claimIn("wrong-zone", 24, "eu-west-1a")
		expectDenied(obj, "not an availability zone of region eu-central-1")
	})

	It("refuses a subnet bigger than the VPC it would live in", func() {
		obj := claimIn("too-big", 20)
		expectDenied(obj, "does not fit in network")
	})

	It("refuses a claim the VPC has no room for", func() {
		obj := claimIn("too-many", 22, claimRegion+"a", claimRegion+"b")
		expectDenied(obj, "has no room for")
	})

	It("refuses a tag key AWS reserves for itself", func() {
		obj := claimIn("reserved-tag", 24)
		obj.Spec.Tags = map[string]string{"aws:cloudformation:stack-name": "payments"}
		expectDenied(obj, "reserved by AWS")
	})

	It("refuses Create mode for an account without a write role", func() {
		By("a scope whose account is reached through a read role")
		s := scope("claims-read-only", "200000000002", claimRegion)
		awsOf(&s.Spec.Accounts[0]).RoleARN = "arn:aws:iam::200000000002:role/reader"
		mustCreate(s)
		created = append(created, s)

		obj := claimIn("no-write-role", 24)
		obj.Spec.ScopeRef = s.Name
		obj.Spec.Account = "200000000002"
		obj.Spec.NetworkID = "vpc-0bbb2"
		obj.Spec.Mode = networkv1beta1.ClaimModeCreate
		expectDenied(obj, "has no aws.writeRoleARN")
	})

	It("keeps the VPC of a claim that already exists", func() {
		obj := claimIn("stays-put", 24)
		mustCreate(obj)
		created = append(created, obj)

		obj.Spec.NetworkID = "vpc-0ccc3"
		expectUpdateDenied(obj, "field is immutable")
	})

	It("keeps the prefix length once CIDRs are reserved", func() {
		obj := claimIn("reserved-already", 24)
		mustCreate(obj)
		created = append(created, obj)

		obj.Status.Allocations = []networkv1beta1.SubnetAllocation{
			{Name: "alloc-" + claimRegion + "a", Zone: claimRegion + "a", CIDRBlock: "10.0.0.0/24", State: networkv1beta1.AllocationPending},
		}
		Expect(k8sClient.Status().Update(ctx, obj)).To(Succeed())

		obj.Spec.PrefixLength = 26
		expectUpdateDenied(obj, "prefixLength cannot change once CIDRs are reserved")
	})

	It("accepts a claim for a VPC nobody has discovered yet, and says so", func() {
		obj := claimIn("not-discovered-yet", 24)
		obj.Spec.NetworkID = "vpc-0ddd4"

		validator := &SubnetClaimValidator{Client: k8sClient, Providers: testProviders, WritesEnabled: true}
		warnings, err := validator.ValidateCreate(ctx, obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).To(ContainElement(ContainSubstring("not in the inventory")))
	})

	It("refuses a claim whose scope's provider the operator does not run", func() {
		obj := claimIn("provider-off", 24)
		validator := &SubnetClaimValidator{Client: k8sClient, Providers: provider.MustRegistry(), WritesEnabled: true}
		_, err := validator.ValidateCreate(ctx, obj)
		Expect(err).To(MatchError(ContainSubstring(`provider "AWS" is not enabled in this operator`)))
	})

	It("warns that a read-only operator will not create the subnets", func() {
		obj := claimIn("read-only-operator", 24)
		obj.Spec.Mode = networkv1beta1.ClaimModeCreate

		validator := &SubnetClaimValidator{Client: k8sClient, Providers: testProviders, WritesEnabled: false}
		warnings, err := validator.ValidateCreate(ctx, obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).To(ContainElement(ContainSubstring("--enable-writes")))
	})

	It("warns that a subnet stays in AWS when its zone is dropped", func() {
		oldObj := claimIn("dropped-zone", 24, claimRegion+"a", claimRegion+"b")
		oldObj.Status.Allocations = []networkv1beta1.SubnetAllocation{
			{Name: "alloc-" + claimRegion + "b", Zone: claimRegion + "b", CIDRBlock: "10.0.1.0/24", SubnetID: "subnet-0eee5"},
		}
		newObj := claimIn("dropped-zone", 24, claimRegion+"a")

		validator := &SubnetClaimValidator{Client: k8sClient, Providers: testProviders, WritesEnabled: true}
		warnings, err := validator.ValidateUpdate(ctx, oldObj, newObj)
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).To(ContainElement(ContainSubstring("subnet-0eee5")))
	})
})
