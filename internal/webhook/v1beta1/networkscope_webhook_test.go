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

// scope builds a NetworkScope over one account and region that every namespace may use.
func scope(name, account, region string) *networkv1beta1.NetworkScope {
	return &networkv1beta1.NetworkScope{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: networkv1beta1.NetworkScopeSpec{
			Provider:          networkv1beta1.ProviderAWS,
			Accounts:          []networkv1beta1.Account{{ID: account}},
			Regions:           []string{region},
			NamespaceSelector: &metav1.LabelSelector{},
		},
	}
}

var _ = Describe("NetworkScope webhook", func() {
	var created []client.Object

	AfterEach(func() {
		for _, obj := range created {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
		created = nil
	})

	It("fills in the tag keys somebody blanked out", func() {
		obj := scope("scope-defaults", "100000000001", "eu-central-1")
		obj.Spec.TagKeys = networkv1beta1.TagKeys{Owner: "", Env: "team/env", Tier: ""}
		mustCreate(obj)
		created = append(created, obj)

		Expect(obj.Spec.TagKeys.Owner).To(Equal(networkv1beta1.DefaultOwnerTagKey))
		Expect(obj.Spec.TagKeys.Env).To(Equal("team/env"), "a key somebody chose is left alone")
		Expect(obj.Spec.TagKeys.Tier).To(Equal(networkv1beta1.DefaultTierTagKey))
		Expect(obj.Spec.ResyncInterval).NotTo(BeNil())
	})

	It("refuses a region name that is not a region", func() {
		obj := scope("scope-bad-region", "100000000002", "eu-central1")
		expectDenied(obj, "not an AWS region name")
	})

	It("refuses a region name that is not a region on an account", func() {
		obj := scope("scope-bad-account-region", "100000000003", "eu-central-1")
		obj.Spec.Accounts[0].Regions = []string{"europe-west"}
		expectDenied(obj, "not an AWS region name")
	})

	It("refuses a role that lives in a different account", func() {
		obj := scope("scope-foreign-role", "100000000004", "eu-central-1")
		awsOf(&obj.Spec.Accounts[0]).RoleARN = "arn:aws:iam::999999999999:role/reader"
		expectDenied(obj, "sts:AssumeRole can only assume a role of the account it reaches")
	})

	It("refuses a write role that lives in a different account", func() {
		obj := scope("scope-foreign-write-role", "100000000005", "eu-central-1")
		awsOf(&obj.Spec.Accounts[0]).RoleARN = "arn:aws:iam::100000000005:role/reader"
		awsOf(&obj.Spec.Accounts[0]).WriteRoleARN = "arn:aws:iam::999999999999:role/writer"
		expectDenied(obj, "sts:AssumeRole can only assume a role of the account it reaches")
	})

	It("refuses a second account without a role, because the operator has one identity", func() {
		obj := scope("scope-two-own-accounts", "100000000006", "eu-central-1")
		obj.Spec.Accounts = append(obj.Spec.Accounts, networkv1beta1.Account{ID: "100000000007"})
		expectDenied(obj, "only one account may omit aws.roleARN")
	})

	It("refuses an account and region another scope already discovers", func() {
		first := scope("scope-first", "100000000008", "eu-central-1")
		mustCreate(first)
		created = append(created, first)

		second := scope("scope-overlap", "100000000008", "eu-central-1")
		awsOf(&second.Spec.Accounts[0]).RoleARN = "arn:aws:iam::100000000008:role/reader"
		expectDenied(second, "already discovered by NetworkScope")
	})

	It("accepts the same account in a region nobody else discovers", func() {
		first := scope("scope-frankfurt", "100000000009", "eu-central-1")
		mustCreate(first)
		created = append(created, first)

		second := scope("scope-ireland", "100000000009", "eu-west-1")
		awsOf(&second.Spec.Accounts[0]).RoleARN = "arn:aws:iam::100000000009:role/reader"
		mustCreate(second)
		created = append(created, second)
	})

	It("warns that an external ID without a role is never used", func() {
		obj := scope("scope-external-id", "100000000010", "eu-central-1")
		awsOf(&obj.Spec.Accounts[0]).ExternalID = "shared-secret"

		validator := &NetworkScopeValidator{Client: k8sClient, Providers: testProviders}
		warnings, err := validator.ValidateCreate(ctx, obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).To(ContainElement(ContainSubstring("aws.externalID but no aws.roleARN")))
	})

	It("refuses a scope whose provider the operator does not run", func() {
		obj := scope("scope-provider-off", "100000000012", "eu-central-1")
		validator := &NetworkScopeValidator{Client: k8sClient, Providers: provider.MustRegistry()}
		_, err := validator.ValidateCreate(ctx, obj)
		Expect(err).To(MatchError(ContainSubstring(`spec.provider: Unsupported value: "AWS"`)))
	})

	It("warns that auto-import in Apply mode tags resources in AWS", func() {
		obj := scope("scope-auto-import", "100000000011", "eu-central-1")
		obj.Spec.AutoImport = &networkv1beta1.AutoImportPolicy{Mode: networkv1beta1.AutoImportApply}

		validator := &NetworkScopeValidator{Client: k8sClient, Providers: testProviders}
		warnings, err := validator.ValidateCreate(ctx, obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).To(ContainElement(ContainSubstring("auto-import is in Apply mode")))
	})
})
