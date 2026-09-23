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
	"maps"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// fakeTagWriter records what would be applied, and can fail.
type fakeTagWriter struct {
	mu      sync.Mutex
	calls   []taggedResource
	failErr error
}

type taggedResource struct {
	target     inventory.Target
	resourceID string
	tags       map[string]string
}

func (f *fakeTagWriter) ApplyTags(_ context.Context, target inventory.Target, resourceID string, tags map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failErr != nil {
		return f.failErr
	}
	f.calls = append(f.calls, taggedResource{target: target, resourceID: resourceID, tags: maps.Clone(tags)})
	return nil
}

var importCounter int

var _ = Describe("ResourceImport Controller", func() {
	const (
		hubAccount   = "111111111111"
		spokeAccount = "222222222222"
		importRegion = "eu-central-1"
		resourceID   = "subnet-04d1c2b3a4e5f607"
	)
	var (
		scopeName  string
		importName string
		writer     *fakeTagWriter
		notified   []inventory.TargetKey
		reconciler *ResourceImportReconciler
	)

	reconcileImport := func() error {
		_, err := reconciler.Reconcile(ctx, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: importName, Namespace: "default"}})
		return err
	}
	getImport := func() *awsv1alpha1.ResourceImport {
		GinkgoHelper()
		imp := &awsv1alpha1.ResourceImport{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: importName, Namespace: "default"}, imp)).To(Succeed())
		return imp
	}
	createImport := func(mutate func(*awsv1alpha1.ResourceImport)) {
		GinkgoHelper()
		imp := &awsv1alpha1.ResourceImport{
			ObjectMeta: metav1.ObjectMeta{Name: importName, Namespace: "default",
				Labels: map[string]string{awsv1alpha1.LabelResource: resourceID}},
			Spec: awsv1alpha1.ResourceImportSpec{
				ScopeRef: scopeName, Account: hubAccount, Region: importRegion, ResourceID: resourceID,
				Tags:        map[string]string{"hs/managed": "true", "hs/owner": "team-data"},
				RequestedBy: "anton",
			},
		}
		if mutate != nil {
			mutate(imp)
		}
		Expect(k8sClient.Create(ctx, imp)).To(Succeed())
	}
	readyCond := func(imp *awsv1alpha1.ResourceImport) *metav1.Condition {
		return meta.FindStatusCondition(imp.Status.Conditions, ConditionReady)
	}

	BeforeEach(func() {
		importCounter++
		scopeName = fmt.Sprintf("import-scope-%d", importCounter)
		importName = fmt.Sprintf("import-%d", importCounter)
		writer = &fakeTagWriter{}
		notified = nil
		reconciler = &ResourceImportReconciler{
			Client: k8sClient, Scheme: k8sClient.Scheme(), Writer: writer, WritesEnabled: true,
			Notify: func(_ context.Context, keys []inventory.TargetKey) error {
				notified = append(notified, keys...)
				return nil
			},
		}

		Expect(k8sClient.Create(ctx, &awsv1alpha1.NetworkScope{
			ObjectMeta: metav1.ObjectMeta{Name: scopeName},
			Spec: awsv1alpha1.NetworkScopeSpec{
				Accounts: []awsv1alpha1.AccountSpec{
					{ID: hubAccount},
					{ID: spokeAccount, RoleARN: "arn:aws:iam::" + spokeAccount + ":role/aws-subnet-operator-readonly"},
				},
				Regions: []string{importRegion},
			},
		})).To(Succeed())
	})

	AfterEach(func() {
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &awsv1alpha1.ResourceImport{
			ObjectMeta: metav1.ObjectMeta{Name: importName, Namespace: "default"}}))).To(Succeed())
		Expect(k8sClient.Delete(ctx, &awsv1alpha1.NetworkScope{ObjectMeta: metav1.ObjectMeta{Name: scopeName}})).To(Succeed())
	})

	It("applies the tags and asks for a resync", func() {
		createImport(nil)
		Expect(reconcileImport()).To(Succeed())

		Expect(writer.calls).To(HaveLen(1))
		Expect(writer.calls[0].resourceID).To(Equal(resourceID))
		Expect(writer.calls[0].tags).To(Equal(map[string]string{"hs/managed": "true", "hs/owner": "team-data"}))
		Expect(writer.calls[0].target.RoleARN).To(BeEmpty(), "the operator's own account uses its own credentials")
		Expect(notified).To(Equal([]inventory.TargetKey{{Account: hubAccount, Region: importRegion}}))

		imp := getImport()
		Expect(imp.Status.State).To(Equal(awsv1alpha1.ImportApplied))
		Expect(imp.Status.AppliedTags).To(Equal(imp.Spec.Tags))
		Expect(imp.Status.AppliedTime).NotTo(BeNil())
		Expect(readyCond(imp).Status).To(Equal(metav1.ConditionTrue))
		Expect(readyCond(imp).Message).To(ContainSubstring("hs/owner=team-data"))

		By("a second pass calls AWS again for nothing")
		Expect(reconcileImport()).To(Succeed())
		Expect(writer.calls).To(HaveLen(1))
	})

	It("re-applies when the tags change", func() {
		createImport(nil)
		Expect(reconcileImport()).To(Succeed())

		imp := getImport()
		imp.Spec.Tags["hs/env"] = "prod"
		Expect(k8sClient.Update(ctx, imp)).To(Succeed())
		Expect(reconcileImport()).To(Succeed())

		Expect(writer.calls).To(HaveLen(2))
		Expect(writer.calls[1].tags).To(HaveKeyWithValue("hs/env", "prod"))
	})

	It("changes nothing in a dry run", func() {
		createImport(func(i *awsv1alpha1.ResourceImport) { i.Spec.DryRun = true })
		Expect(reconcileImport()).To(Succeed())

		Expect(writer.calls).To(BeEmpty())
		imp := getImport()
		Expect(imp.Status.State).To(Equal(awsv1alpha1.ImportSkipped))
		Expect(readyCond(imp).Status).To(Equal(metav1.ConditionTrue))
		Expect(readyCond(imp).Reason).To(Equal("DryRun"))
		Expect(readyCond(imp).Message).To(ContainSubstring("hs/owner=team-data"))
		Expect(notified).To(BeEmpty())
	})

	It("refuses while writes are disabled", func() {
		reconciler.WritesEnabled = false
		createImport(nil)
		Expect(reconcileImport()).To(Succeed())

		Expect(writer.calls).To(BeEmpty())
		imp := getImport()
		Expect(imp.Status.State).To(Equal(awsv1alpha1.ImportPending))
		Expect(readyCond(imp).Reason).To(Equal("WritesDisabled"))
	})

	It("reports a spoke account without a write role", func() {
		createImport(func(i *awsv1alpha1.ResourceImport) { i.Spec.Account = spokeAccount })
		Expect(reconcileImport()).To(Succeed())

		Expect(writer.calls).To(BeEmpty())
		imp := getImport()
		Expect(readyCond(imp).Reason).To(Equal("NoWriteRole"))
		Expect(readyCond(imp).Message).To(ContainSubstring("ec2:CreateTags"))
	})

	It("reports a scope that does not cover the account", func() {
		createImport(func(i *awsv1alpha1.ResourceImport) { i.Spec.Account = "999999999999" })
		Expect(reconcileImport()).To(Succeed())
		Expect(readyCond(getImport()).Reason).To(Equal("AccountNotInScope"))

		By("and a scope that is not there at all")
		imp := getImport()
		imp.Spec.ScopeRef = "does-not-exist"
		Expect(k8sClient.Update(ctx, imp)).To(Succeed())
		Expect(reconcileImport()).To(Succeed())
		Expect(readyCond(getImport()).Reason).To(Equal("ScopeNotFound"))
	})

	It("keeps the failure in the status and retries", func() {
		writer.failErr = errors.New("UnauthorizedOperation: not allowed to CreateTags")
		createImport(nil)
		Expect(reconcileImport()).To(Succeed())

		imp := getImport()
		Expect(imp.Status.State).To(Equal(awsv1alpha1.ImportFailed))
		Expect(imp.Status.Error).To(ContainSubstring("UnauthorizedOperation"))
		Expect(readyCond(imp).Status).To(Equal(metav1.ConditionFalse))

		By("succeeding once the permission is there")
		writer.failErr = nil
		Expect(reconcileImport()).To(Succeed())
		Expect(getImport().Status.State).To(Equal(awsv1alpha1.ImportApplied))
		Expect(writer.calls).To(HaveLen(1))
	})
})
