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

package v1alpha1

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
)

const (
	oldScopeName = "old-org"
	oldAccount   = "500000000001"
	oldRegion    = "eu-central-1"
)

func oldClaim(name string) *awsv1alpha1.SubnetClaim {
	return &awsv1alpha1.SubnetClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: awsv1alpha1.SubnetClaimSpec{
			ScopeRef: oldScopeName, Account: oldAccount, Region: oldRegion, VPCID: "vpc-0aaa1",
			PrefixLength: 24, AvailabilityZones: []string{oldRegion + "a"},
			Mode: awsv1alpha1.ClaimModeAllocate, Owner: "payments",
		},
	}
}

var _ = Describe("The aws.hypersurgery/v1alpha1 webhooks", func() {
	var created []client.Object

	BeforeEach(func() {
		By("an old scope without a namespaceSelector, which nothing has migrated yet")
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: oldScopeName}, &awsv1alpha1.NetworkScope{}); err != nil {
			mustCreate(&awsv1alpha1.NetworkScope{
				ObjectMeta: metav1.ObjectMeta{Name: oldScopeName},
				Spec: awsv1alpha1.NetworkScopeSpec{
					Accounts: []awsv1alpha1.AccountSpec{{ID: oldAccount}}, Regions: []string{oldRegion}},
			})
		}
	})

	AfterEach(func() {
		for _, obj := range created {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
		created = nil
	})

	It("fills in the old defaults", func() {
		scope := &awsv1alpha1.NetworkScope{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: oldScopeName}, scope)).To(Succeed())
		Expect(scope.Spec.TagKeys.Owner).To(Equal(awsv1alpha1.DefaultOwnerTagKey))

		claim := oldClaim("defaults")
		mustCreate(claim)
		created = append(created, claim)
		Expect(claim.Spec.NamePrefix).To(Equal("defaults"))
	})

	It("accepts a claim against an old scope that is not migrated yet, from any namespace", func() {
		// The scope has no namespaceSelector, which in the old group allows every namespace;
		// the claim is checked against the scope as the migration will write it ({}).
		claim := oldClaim("before-migration")
		mustCreate(claim)
		created = append(created, claim)
	})

	It("records the authenticated creator in the old annotation, which the migration carries over", func() {
		claim := oldClaim("created-by")
		claim.Annotations = map[string]string{awsv1alpha1.AnnotationCreatedBy: "somebody-else"}
		jane := clientAs("jane@example.com")
		Eventually(func() error { return jane.Create(ctx, claim) }).Should(Succeed())
		created = append(created, claim)
		Expect(claim.Annotations).To(HaveKeyWithValue(awsv1alpha1.AnnotationCreatedBy, "jane@example.com"))

		claim.Annotations[awsv1alpha1.AnnotationCreatedBy] = "somebody-else"
		expectUpdateDenied(claim, "cannot be changed")
	})

	It("checks an old claim with the new group's rules, and names the old group", func() {
		claim := oldClaim("wrong-zone")
		claim.Spec.AvailabilityZones = []string{"eu-west-1a"}
		expectDenied(claim, "not an availability zone of region eu-central-1")
		expectDenied(claim, `SubnetClaim.aws.hypersurgery "wrong-zone" is invalid`)
	})

	It("refuses to change the spec of a migrated object, and names the one to change", func() {
		claim := oldClaim("migrated")
		mustCreate(claim)
		created = append(created, claim)

		claim.Annotations[networkv1beta1.AnnotationMigratedTo] = claim.Name
		Expect(k8sClient.Update(ctx, claim)).To(Succeed(), "marking it is not a change of the spec")

		claim.Labels = map[string]string{"team": "payments"}
		Expect(k8sClient.Update(ctx, claim)).To(Succeed(), "nor is a label")

		claim.Spec.PrefixLength = 26
		expectUpdateDenied(claim, "was migrated to network.hypersurgery.dev/v1beta1 SubnetClaim default/migrated")
	})

	It("refuses to change the spec of a migrated scope", func() {
		scope := &awsv1alpha1.NetworkScope{
			ObjectMeta: metav1.ObjectMeta{Name: "old-migrated",
				Annotations: map[string]string{networkv1beta1.AnnotationMigratedTo: "old-migrated"}},
			Spec: awsv1alpha1.NetworkScopeSpec{
				Accounts: []awsv1alpha1.AccountSpec{{ID: "500000000002"}}, Regions: []string{oldRegion}},
		}
		mustCreate(scope)
		created = append(created, scope)

		scope.Spec.Regions = append(scope.Spec.Regions, "eu-west-1")
		expectUpdateDenied(scope, "was migrated to network.hypersurgery.dev/v1beta1 NetworkScope old-migrated")
	})
})
