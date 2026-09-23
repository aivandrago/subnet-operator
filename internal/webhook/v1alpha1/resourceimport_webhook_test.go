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
)

const (
	importAccount = "300000000001"
	importRegion  = "eu-central-1"
	importScope   = "imports"
	importSubnet  = "subnet-0fff6"
)

// importOf builds an import of the fixture subnet.
func importOf(name string, tags map[string]string) *awsv1alpha1.ResourceImport {
	return &awsv1alpha1.ResourceImport{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: awsv1alpha1.ResourceImportSpec{
			ScopeRef:   importScope,
			Account:    importAccount,
			Region:     importRegion,
			ResourceID: importSubnet,
			Tags:       tags,
		},
	}
}

var _ = Describe("ResourceImport webhook", func() {
	var created []client.Object

	BeforeEach(func() {
		By("a scope whose auto-import policy insists on an owner, and one unmanaged subnet")
		s := scope(importScope, importAccount, importRegion)
		s.Spec.AutoImport = &awsv1alpha1.AutoImportPolicy{
			Mode:         awsv1alpha1.AutoImportDryRun,
			RequiredTags: []string{awsv1alpha1.DefaultOwnerTagKey},
		}
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: importScope}, &awsv1alpha1.NetworkScope{}); err != nil {
			mustCreate(s)
		}
		subnet := &awsv1alpha1.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: importSubnet},
			Spec: awsv1alpha1.SubnetSpec{
				SubnetID: importSubnet, VPCID: "vpc-0abc7",
				Account: importAccount, Region: importRegion,
			},
		}
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: importSubnet}, &awsv1alpha1.Subnet{}); err != nil {
			mustCreate(subnet)
		}
	})

	AfterEach(func() {
		for _, obj := range created {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
		created = nil
	})

	It("labels the import with the resource, so the policy does not import it twice", func() {
		obj := importOf("labelled", map[string]string{awsv1alpha1.DefaultOwnerTagKey: "payments"})
		mustCreate(obj)
		created = append(created, obj)

		Expect(obj.Labels).To(HaveKeyWithValue(awsv1alpha1.LabelResource, importSubnet))
		Expect(obj.Labels).To(HaveKeyWithValue(awsv1alpha1.LabelScope, importScope))
		Expect(obj.Labels).To(HaveKeyWithValue(awsv1alpha1.LabelAccount, importAccount))
		Expect(obj.Labels).To(HaveKeyWithValue(awsv1alpha1.LabelRegion, importRegion))
	})

	It("records who asked for the import", func() {
		obj := importOf("who-asked", map[string]string{awsv1alpha1.DefaultOwnerTagKey: "payments"})
		mustCreate(obj)
		created = append(created, obj)

		Expect(obj.Spec.RequestedBy).NotTo(BeEmpty(), "the authenticated user should be recorded")
	})

	It("leaves a requestedBy somebody wrote alone", func() {
		obj := importOf("named-requester", map[string]string{awsv1alpha1.DefaultOwnerTagKey: "payments"})
		obj.Spec.RequestedBy = "network team ticket NET-42"
		mustCreate(obj)
		created = append(created, obj)

		Expect(obj.Spec.RequestedBy).To(Equal("network team ticket NET-42"))
	})

	It("refuses an import whose NetworkScope does not exist", func() {
		obj := importOf("no-scope", map[string]string{awsv1alpha1.DefaultOwnerTagKey: "payments"})
		obj.Spec.ScopeRef = "nowhere"
		expectDenied(obj, "spec.scopeRef")
	})

	It("refuses an account no NetworkScope discovers", func() {
		obj := importOf("unreachable", map[string]string{awsv1alpha1.DefaultOwnerTagKey: "payments"})
		obj.Spec.Account = "300000000099"
		expectDenied(obj, "no NetworkScope discovers account")
	})

	It("refuses a resource that lives in another account", func() {
		By("a second scope and a subnet in it")
		other := scope("imports-elsewhere", "300000000002", importRegion)
		mustCreate(other)
		created = append(created, other)

		elsewhere := &awsv1alpha1.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: "subnet-0bcd8"},
			Spec: awsv1alpha1.SubnetSpec{
				SubnetID: "subnet-0bcd8", VPCID: "vpc-0cde9",
				Account: "300000000002", Region: importRegion,
			},
		}
		mustCreate(elsewhere)
		created = append(created, elsewhere)

		obj := importOf("wrong-account", map[string]string{awsv1alpha1.DefaultOwnerTagKey: "payments"})
		obj.Spec.ResourceID = "subnet-0bcd8"
		expectDenied(obj, "not in "+importAccount+"/"+importRegion)
	})

	It("refuses a tag key AWS reserves for itself", func() {
		obj := importOf("reserved-tag", map[string]string{"aws:autoscaling:groupName": "payments"})
		expectDenied(obj, "reserved by AWS")
	})

	It("refuses an import that leaves the resource without the tags the policy requires", func() {
		obj := importOf("no-owner", map[string]string{"hs/env": "prod"})
		expectDenied(obj, "requires hs/owner")
	})

	It("accepts an import when the resource already carries the required tag", func() {
		tagged := &awsv1alpha1.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: "subnet-0def0"},
			Spec: awsv1alpha1.SubnetSpec{
				SubnetID: "subnet-0def0", VPCID: "vpc-0abc7",
				Account: importAccount, Region: importRegion,
			},
		}
		mustCreate(tagged)
		created = append(created, tagged)
		tagged.Status.Tags = map[string]string{awsv1alpha1.DefaultOwnerTagKey: "payments"}
		Expect(k8sClient.Status().Update(ctx, tagged)).To(Succeed())

		obj := importOf("already-owned", map[string]string{"hs/env": "prod"})
		obj.Spec.ResourceID = "subnet-0def0"
		mustCreate(obj)
		created = append(created, obj)
	})

	It("keeps the resource an import points at", func() {
		obj := importOf("stays-put", map[string]string{awsv1alpha1.DefaultOwnerTagKey: "payments"})
		mustCreate(obj)
		created = append(created, obj)

		obj.Spec.ResourceID = "subnet-0ea1b"
		expectUpdateDenied(obj, "field is immutable")
	})

	It("warns that a read-only operator will not apply the tags", func() {
		obj := importOf("read-only-operator", map[string]string{awsv1alpha1.DefaultOwnerTagKey: "payments"})

		validator := &ResourceImportValidator{Client: k8sClient, WritesEnabled: false}
		warnings, err := validator.ValidateCreate(ctx, obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).To(ContainElement(ContainSubstring("--enable-writes")))
	})

	It("warns that a resource nobody has discovered may not exist", func() {
		obj := importOf("not-discovered", map[string]string{awsv1alpha1.DefaultOwnerTagKey: "payments"})
		obj.Spec.ResourceID = "subnet-0fb2c"

		validator := &ResourceImportValidator{Client: k8sClient, WritesEnabled: true}
		warnings, err := validator.ValidateCreate(ctx, obj)
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).To(ContainElement(ContainSubstring("not in the inventory")))
	})
})
