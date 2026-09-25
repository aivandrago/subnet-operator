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

package v1

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
	"hypersurgery.dev/subnet-operator/internal/crdversions"
)

// These specs go through the API server, whose CRDs envtest pointed at this package's webhook
// server: every read or write at v1beta1 of an object stored at v1 is a call to /convert.
var _ = Describe("Conversion webhook", func() {
	var created []client.Object

	AfterEach(func() {
		for _, obj := range created {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
		created = nil
	})

	It("is what the CRDs use", func() {
		for _, name := range []string{"networkscopes", "networks", "subnets", "subnetclaims", "resourceimports",
			"sheetexports"} {
			crd := &apiextensionsv1.CustomResourceDefinition{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: name + ".network.hypersurgery.dev"}, crd)).To(Succeed())
			Expect(crd.Spec.Conversion.Strategy).To(Equal(apiextensionsv1.WebhookConverter), name)
		}
	})

	It("serves a v1 object at v1beta1 and back, spec and status", func() {
		obj := scope("conversion-scope", "100000000101", "eu-central-1")
		mustCreate(obj)
		created = append(created, obj)
		obj.Status.Networks = 3
		obj.Status.Capabilities = []networkv1.Capability{networkv1.CapabilityCreateSubnet}
		obj.Status.Conditions = []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Synced",
			LastTransitionTime: metav1.Now()}}
		Expect(k8sClient.Status().Update(ctx, obj)).To(Succeed())

		beta := &networkv1beta1.NetworkScope{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), beta)).To(Succeed())
		Expect(beta.Spec.Accounts[0].ID).To(Equal("100000000101"))
		Expect(beta.Spec.TagKeys.Owner).To(Equal(networkv1beta1.DefaultOwnerTagKey))
		Expect(beta.Status.Networks).To(Equal(int32(3)))
		Expect(beta.Status.Capabilities).To(ConsistOf(networkv1beta1.CapabilityCreateSubnet))
		Expect(beta.Status.Conditions).To(HaveLen(1))

		// Written back at v1beta1 and read at v1, nothing is lost.
		beta.Spec.RequiredSubnetTags = []string{"hs/owner"}
		Expect(k8sClient.Update(ctx, beta)).To(Succeed())
		again := &networkv1.NetworkScope{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(obj), again)).To(Succeed())
		Expect(again.Spec.RequiredSubnetTags).To(Equal([]string{"hs/owner"}))
		Expect(again.Status.Networks).To(Equal(int32(3)))
		Expect(again.Spec.NamespaceSelector).NotTo(BeNil())
	})

	It("accepts an object created at v1beta1, with the v1 webhooks' defaults and checks", func() {
		ns := &unstructured.Unstructured{}
		ns.SetAPIVersion("network.hypersurgery.dev/v1beta1")
		ns.SetKind("NetworkScope")
		ns.SetName("conversion-beta")
		ns.Object["spec"] = map[string]any{
			"provider": "AWS", "regions": []any{"eu-central-1"},
			"accounts":          []any{map[string]any{"id": "100000000102"}},
			"namespaceSelector": map[string]any{},
		}
		mustCreate(ns)
		created = append(created, ns)

		obj := &networkv1.NetworkScope{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "conversion-beta"}, obj)).To(Succeed())
		Expect(obj.Spec.TagKeys.Owner).To(Equal(networkv1.DefaultOwnerTagKey), "the defaulting webhook ran")
		Expect(obj.Spec.DiscoverUnmanaged).To(Equal(new(true)))
	})

	// v1 declares regions a set; a request at v1beta1 is checked against the v1beta1 schema,
	// which declared them atomic, so the webhook is what keeps a duplicate out of a v1 object.
	It("refuses at v1beta1 a duplicate that v1's schema refuses", func() {
		ns := &unstructured.Unstructured{}
		ns.SetAPIVersion("network.hypersurgery.dev/v1beta1")
		ns.SetKind("NetworkScope")
		ns.SetName("conversion-duplicate")
		ns.Object["spec"] = map[string]any{
			"provider": "AWS", "regions": []any{"eu-central-1", "eu-central-1"},
			"accounts":          []any{map[string]any{"id": "100000000103"}},
			"namespaceSelector": map[string]any{},
		}
		expectDenied(ns, "spec.regions[1]: Duplicate value")
	})
})

var _ = Describe("Conversion webhook: objects as they were written", func() {
	It("serves a v1 object at v1beta1 with its values spelled as they were stored", func() {
		// SheetExport has no admission webhook, so what is stored is what was sent: "5m", as
		// the CRD default writes it, where the Go types would say "5m0s".
		export := &unstructured.Unstructured{}
		export.SetAPIVersion("network.hypersurgery.dev/v1")
		export.SetKind("SheetExport")
		export.SetName("conversion-exact")
		export.Object["spec"] = map[string]any{
			"scopeRef": "somewhere", "spreadsheetID": "sheet", "refreshInterval": "5m",
			"extraTagColumns":      []any{},
			"credentialsSecretRef": map[string]any{"name": "google", "namespace": "default"},
		}
		mustCreate(export)
		DeferCleanup(func() { Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, export))).To(Succeed()) })

		beta := &unstructured.Unstructured{}
		beta.SetAPIVersion("network.hypersurgery.dev/v1beta1")
		beta.SetKind("SheetExport")
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: "conversion-exact"}, beta)).To(Succeed())
		Expect(beta.GetAPIVersion()).To(Equal("network.hypersurgery.dev/v1beta1"))
		Expect(beta.Object["spec"]).To(HaveKeyWithValue("refreshInterval", "5m"))
		Expect(beta.Object["spec"]).To(HaveKeyWithValue("extraTagColumns", []any{}))
	})

	It("is where the operator points the CRDs", func() {
		Expect(ConvertPath).To(Equal(crdversions.ConvertPath))
	})
})

// ExactConversion only swaps in the original where the typed conversion agrees with it.
var _ = Describe("ExactConversion", func() {
	scheme := runtime.NewScheme()
	Expect(networkv1.AddToScheme(scheme)).To(Succeed())
	Expect(networkv1beta1.AddToScheme(scheme)).To(Succeed())

	review := func(desired string, objects ...string) []byte {
		GinkgoHelper()
		r := apiextensionsv1.ConversionReview{Request: &apiextensionsv1.ConversionRequest{UID: "1",
			DesiredAPIVersion: desired}}
		for _, o := range objects {
			r.Request.Objects = append(r.Request.Objects, runtime.RawExtension{Raw: []byte(o)})
		}
		raw, err := json.Marshal(r)
		Expect(err).NotTo(HaveOccurred())
		return raw
	}
	// typed answers the way the typed conversion does, with the objects it is given.
	typed := func(converted ...string) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			r := apiextensionsv1.ConversionReview{Response: &apiextensionsv1.ConversionResponse{UID: "1",
				Result: metav1.Status{Status: metav1.StatusSuccess}}}
			for _, o := range converted {
				r.Response.ConvertedObjects = append(r.Response.ConvertedObjects, runtime.RawExtension{Raw: []byte(o)})
			}
			_ = json.NewEncoder(w).Encode(r)
		})
	}
	convert := func(h http.Handler, body []byte) []string {
		GinkgoHelper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, ConvertPath, bytes.NewReader(body)))
		Expect(rec.Code).To(Equal(http.StatusOK))
		var out apiextensionsv1.ConversionReview
		Expect(json.Unmarshal(rec.Body.Bytes(), &out)).To(Succeed())
		objs := make([]string, 0, len(out.Response.ConvertedObjects))
		for _, o := range out.Response.ConvertedObjects {
			objs = append(objs, string(o.Raw))
		}
		return objs
	}
	const original = `{"apiVersion":"network.hypersurgery.dev/v1","kind":"SheetExport","metadata":{"name":"x"},` +
		`"spec":{"scopeRef":"s","spreadsheetID":"id","refreshInterval":"5m","credentialsSecretRef":{"name":"n","namespace":"ns"}}}`

	It("returns the original, moved, when it means the same", func() {
		sameMeaning := `{"apiVersion":"network.hypersurgery.dev/v1beta1","kind":"SheetExport","metadata":{"name":"x"},` +
			`"spec":{"scopeRef":"s","spreadsheetID":"id","refreshInterval":"5m0s","credentialsSecretRef":{"name":"n","namespace":"ns"}}}`
		out := convert(ExactConversion(scheme, typed(sameMeaning)), review("network.hypersurgery.dev/v1beta1", original))
		Expect(out).To(HaveLen(1))
		Expect(out[0]).To(ContainSubstring(`"refreshInterval":"5m"`))
		Expect(out[0]).To(ContainSubstring(`"apiVersion":"network.hypersurgery.dev/v1beta1"`))
	})

	It("returns the typed result when the conversion changed something", func() {
		changed := `{"apiVersion":"network.hypersurgery.dev/v1beta1","kind":"SheetExport","metadata":{"name":"x"},` +
			`"spec":{"scopeRef":"other","spreadsheetID":"id","refreshInterval":"5m0s","credentialsSecretRef":{"name":"n","namespace":"ns"}}}`
		out := convert(ExactConversion(scheme, typed(changed)), review("network.hypersurgery.dev/v1beta1", original))
		Expect(out).To(Equal([]string{changed}))
	})

	It("passes a failed review through", func() {
		failed := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(apiextensionsv1.ConversionReview{Response: &apiextensionsv1.ConversionResponse{
				UID: "1", Result: metav1.Status{Status: metav1.StatusFailure, Message: "no"}}})
		})
		rec := httptest.NewRecorder()
		ExactConversion(scheme, failed).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, ConvertPath,
			bytes.NewReader(review("network.hypersurgery.dev/v1beta1", original))))
		Expect(rec.Body.String()).To(ContainSubstring(`"message":"no"`))
	})
})
