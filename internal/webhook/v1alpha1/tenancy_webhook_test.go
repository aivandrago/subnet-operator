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

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
)

const (
	tenantAccount = "400000000001"
	tenantRegion  = "eu-central-1"
	tenantScope   = "tenants"
	// tenantNamespace is selected by the scope's namespaceSelector, otherNamespace is not.
	tenantNamespace = "team-payments"
	otherNamespace  = "team-data"
)

// ensureNamespace creates a namespace once; envtest has no namespace controller, so a
// namespace created by one spec is still there for the next.
func ensureNamespace(name string, labels map[string]string) {
	GinkgoHelper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: name}, &corev1.Namespace{}); err != nil {
		Expect(k8sClient.Create(ctx, ns)).To(Succeed())
	}
}

// clientAs is a client whose requests the API server attributes to user. The test's own
// credentials may impersonate anybody; system:masters keeps the RBAC out of the way, so the
// only thing that differs from k8sClient is the name the webhooks see.
func clientAs(user string) client.Client {
	GinkgoHelper()
	impersonating := rest.CopyConfig(cfg)
	impersonating.Impersonate = rest.ImpersonationConfig{UserName: user, Groups: []string{"system:masters"}}
	c, err := client.New(impersonating, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	return c
}

// asUser is the context of an admission request made by user, for calling a validator
// directly the way the webhook server would.
func asUser(user string, op admissionv1.Operation) admission.Request {
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: op, UserInfo: authenticationv1.UserInfo{Username: user},
	}}
}

var _ = Describe("Namespaces allowed to use a NetworkScope", func() {
	var created []client.Object

	claimInNamespace := func(name, namespace string) *awsv1alpha1.SubnetClaim {
		return &awsv1alpha1.SubnetClaim{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: awsv1alpha1.SubnetClaimSpec{
				ScopeRef: tenantScope, Account: tenantAccount, Region: tenantRegion, VPCID: "vpc-0bbb1",
				PrefixLength: 24, AvailabilityZones: []string{tenantRegion + "a"},
				Mode: awsv1alpha1.ClaimModeCreate, Owner: "payments",
			},
		}
	}
	importInNamespace := func(name, namespace string) *awsv1alpha1.ResourceImport {
		return &awsv1alpha1.ResourceImport{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: awsv1alpha1.ResourceImportSpec{
				ScopeRef: tenantScope, Account: tenantAccount, Region: tenantRegion,
				ResourceID: "subnet-0bbb2", Tags: map[string]string{awsv1alpha1.DefaultOwnerTagKey: "payments"},
			},
		}
	}

	BeforeEach(func() {
		By("two team namespaces, and a scope only the payments team may use")
		ensureNamespace(tenantNamespace, map[string]string{"example.com/team": "payments"})
		ensureNamespace(otherNamespace, map[string]string{"example.com/team": "data"})
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: tenantScope}, &awsv1alpha1.NetworkScope{}); err != nil {
			s := scope(tenantScope, tenantAccount, tenantRegion)
			s.Spec.NamespaceSelector = &metav1.LabelSelector{
				MatchLabels: map[string]string{"example.com/team": "payments"}}
			mustCreate(s)
		}
	})

	AfterEach(func() {
		for _, obj := range created {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
		created = nil
	})

	It("refuses a claim in a namespace the scope does not select", func() {
		expectDenied(claimInNamespace("elsewhere", otherNamespace),
			`NetworkScope "tenants" does not allow namespace "team-data"`)
	})

	It("accepts a claim in a namespace the scope selects", func() {
		obj := claimInNamespace("at-home", tenantNamespace)
		mustCreate(obj)
		created = append(created, obj)
	})

	It("refuses an import in a namespace the scope does not select", func() {
		expectDenied(importInNamespace("elsewhere", otherNamespace),
			`NetworkScope "tenants" does not allow namespace "team-data"`)
	})

	It("accepts an import in a namespace the scope selects", func() {
		obj := importInNamespace("at-home", tenantNamespace)
		mustCreate(obj)
		created = append(created, obj)
	})

	It("selects namespaces by name through the label the API server sets", func() {
		s := scope("tenants-by-name", "400000000002", tenantRegion)
		s.Spec.NamespaceSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key: corev1.LabelMetadataName, Operator: metav1.LabelSelectorOpIn, Values: []string{otherNamespace},
		}}}
		mustCreate(s)
		created = append(created, s)

		allowed := importInNamespace("by-name", otherNamespace)
		allowed.Spec.ScopeRef, allowed.Spec.Account = s.Name, "400000000002"
		mustCreate(allowed)
		created = append(created, allowed)

		refused := importInNamespace("by-name", tenantNamespace)
		refused.Spec.ScopeRef, refused.Spec.Account = s.Name, "400000000002"
		expectDenied(refused, `does not allow namespace "team-payments"`)
	})

	It("refuses a scope whose auto-import policy writes to a namespace it does not select", func() {
		s := scope("tenants-policy-elsewhere", "400000000003", tenantRegion)
		s.Spec.NamespaceSelector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"example.com/team": "payments"}}
		s.Spec.AutoImport = &awsv1alpha1.AutoImportPolicy{Mode: awsv1alpha1.AutoImportDryRun, Namespace: otherNamespace}
		expectDenied(s, "spec.autoImport.namespace")
	})

	It("accepts a scope whose auto-import namespace does not exist yet, with a warning", func() {
		s := scope("tenants-policy-later", "400000000004", tenantRegion)
		s.Spec.NamespaceSelector = &metav1.LabelSelector{
			MatchLabels: map[string]string{"example.com/team": "payments"}}
		s.Spec.AutoImport = &awsv1alpha1.AutoImportPolicy{Mode: awsv1alpha1.AutoImportDryRun, Namespace: "not-yet"}

		warnings, err := (&NetworkScopeValidator{Client: k8sClient}).ValidateCreate(ctx, s)
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).To(ContainElement(ContainSubstring(`namespace "not-yet"`)))
	})

	It("warns that a scope without a namespaceSelector may be used from any namespace", func() {
		s := scope("tenants-open", "400000000005", tenantRegion)

		warnings, err := (&NetworkScopeValidator{Client: k8sClient}).ValidateCreate(ctx, s)
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).To(ContainElement(ContainSubstring("has no spec.namespaceSelector")))
	})

	It("refuses a namespaceSelector that does not parse", func() {
		s := scope("tenants-broken-selector", "400000000006", tenantRegion)
		s.Spec.NamespaceSelector = &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
			Key: "example.com/team", Operator: metav1.LabelSelectorOpIn,
		}}}
		expectDenied(s, "spec.namespaceSelector")
	})
})

var _ = Describe("The created-by annotation", func() {
	var created []client.Object

	BeforeEach(func() {
		s := scope(importScope, importAccount, importRegion)
		s.Spec.AutoImport = &awsv1alpha1.AutoImportPolicy{
			Mode: awsv1alpha1.AutoImportDryRun, RequiredTags: []string{awsv1alpha1.DefaultOwnerTagKey}}
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: importScope}, &awsv1alpha1.NetworkScope{}); err != nil {
			mustCreate(s)
		}
		c := scope(claimScope, claimAccount, claimRegion)
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: claimScope}, &awsv1alpha1.NetworkScope{}); err != nil {
			mustCreate(c)
		}
	})

	AfterEach(func() {
		for _, obj := range created {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
		created = nil
	})

	It("names the authenticated user on an import, whatever the object said", func() {
		obj := importOf("forged-creator", map[string]string{awsv1alpha1.DefaultOwnerTagKey: "payments"})
		obj.Annotations = map[string]string{awsv1alpha1.AnnotationCreatedBy: "somebody-else"}
		obj.Spec.RequestedBy = "somebody-else"

		jane := clientAs("jane@example.com")
		Eventually(func() error { return jane.Create(ctx, obj) }).Should(Succeed())
		created = append(created, obj)

		Expect(obj.Annotations).To(HaveKeyWithValue(awsv1alpha1.AnnotationCreatedBy, "jane@example.com"))
		Expect(obj.Spec.RequestedBy).To(Equal("somebody-else"),
			"requestedBy stays free text; it is the annotation that cannot lie")
	})

	It("names the authenticated user on a claim, whatever the object said", func() {
		obj := claimIn("forged-creator", 26)
		obj.Annotations = map[string]string{awsv1alpha1.AnnotationCreatedBy: "somebody-else"}

		jane := clientAs("jane@example.com")
		Eventually(func() error { return jane.Create(ctx, obj) }).Should(Succeed())
		created = append(created, obj)

		Expect(obj.Annotations).To(HaveKeyWithValue(awsv1alpha1.AnnotationCreatedBy, "jane@example.com"))
	})

	It("refuses an update that changes who created an import", func() {
		obj := importOf("rewritten-creator", map[string]string{awsv1alpha1.DefaultOwnerTagKey: "payments"})
		mustCreate(obj)
		created = append(created, obj)

		obj.Annotations[awsv1alpha1.AnnotationCreatedBy] = "somebody-else"
		expectUpdateDenied(obj, "cannot be changed")
	})

	It("refuses an update that removes who created a claim", func() {
		obj := claimIn("removed-creator", 26)
		mustCreate(obj)
		created = append(created, obj)

		delete(obj.Annotations, awsv1alpha1.AnnotationCreatedBy)
		expectUpdateDenied(obj, "cannot be changed")
	})

	It("refuses an update that adds a creator to an object created without one", func() {
		oldObj := importOf("no-creator", map[string]string{awsv1alpha1.DefaultOwnerTagKey: "payments"})
		newObj := oldObj.DeepCopy()
		newObj.Annotations = map[string]string{awsv1alpha1.AnnotationCreatedBy: "somebody-else"}

		_, err := (&ResourceImportValidator{Client: k8sClient, WritesEnabled: true}).ValidateUpdate(ctx, oldObj, newObj)
		Expect(err).To(MatchError(ContainSubstring("was not set")))
	})

	It("refuses a create the mutating webhook did not stamp", func() {
		// The mutating webhook is failurePolicy Ignore: when its call fails, the object reaches
		// the validating webhook with whatever annotation it was sent with.
		obj := importOf("unstamped", map[string]string{awsv1alpha1.DefaultOwnerTagKey: "payments"})
		obj.Annotations = map[string]string{awsv1alpha1.AnnotationCreatedBy: "somebody-else"}
		reqCtx := admission.NewContextWithRequest(ctx, asUser("jane@example.com", admissionv1.Create))

		_, err := (&ResourceImportValidator{Client: k8sClient, WritesEnabled: true}).ValidateCreate(reqCtx, obj)
		Expect(err).To(MatchError(ContainSubstring("must name the user that creates the object (jane@example.com)")))

		claim := claimIn("unstamped", 26)
		_, err = (&SubnetClaimValidator{Client: k8sClient, WritesEnabled: true}).ValidateCreate(reqCtx, claim)
		Expect(err).To(MatchError(ContainSubstring("must name the user that creates the object")),
			"a missing annotation is as unstamped as a forged one")
	})
})
