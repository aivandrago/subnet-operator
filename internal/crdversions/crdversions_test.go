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

package crdversions

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const group = "network.hypersurgery.dev"

// fixtures are one object of every kind, as a 0.9 user has them, statuses included.
var fixtures = []struct {
	kind, namespace, name string
	spec, status          map[string]any
}{
	{kind: "NetworkScope", name: "payments", spec: map[string]any{
		"provider": "AWS", "regions": []any{"eu-central-1"},
		"accounts": []any{map[string]any{"id": "111111111111"}},
	}, status: map[string]any{"networks": int64(2), "subnets": int64(5)}},
	{kind: "Network", name: "vpc-0a", spec: map[string]any{
		"provider": "AWS", "id": "vpc-0a", "account": "111111111111", "region": "eu-central-1",
	}, status: map[string]any{"cidrBlocks": []any{"10.0.0.0/16"}, "subnets": int64(5)}},
	{kind: "Subnet", name: "subnet-0a", spec: map[string]any{
		"provider": "AWS", "id": "subnet-0a", "networkID": "vpc-0a", "account": "111111111111",
		"region": "eu-central-1",
	}, status: map[string]any{"cidrBlock": "10.0.1.0/24", "availableIPs": int64(250),
		"aws": map[string]any{"public": true}}},
	{kind: "SubnetClaim", namespace: "default", name: "payments-db", spec: map[string]any{
		"scopeRef": "payments", "account": "111111111111", "region": "eu-central-1", "networkID": "vpc-0a",
		"prefixLength": int64(24), "owner": "payments", "zones": []any{"eu-central-1a"},
	}, status: map[string]any{"allocations": []any{map[string]any{
		"name": "payments-db-a", "zone": "eu-central-1a", "cidrBlock": "10.0.2.0/24", "state": "Pending",
	}}}},
	{kind: "ResourceImport", namespace: "default", name: "import-vpc", spec: map[string]any{
		"scopeRef": "payments", "account": "111111111111", "region": "eu-central-1", "resourceID": "vpc-0a",
		"tags": map[string]any{"hs/owner": "payments"},
	}, status: map[string]any{"state": "Applied", "appliedTags": map[string]any{"hs/owner": "payments"}}},
	{kind: "SheetExport", name: "inventory", spec: map[string]any{
		"scopeRef": "payments", "spreadsheetID": "sheet",
		"credentialsSecretRef": map[string]any{"name": "google", "namespace": "default"},
	}, status: map[string]any{"rows": int64(5), "url": "https://docs.google.com/spreadsheets/d/sheet"}},
}

func gvk(version, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: group, Version: version, Kind: kind}
}

func readCRDs(dir string) []*apiextensionsv1.CustomResourceDefinition {
	GinkgoHelper()
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	Expect(err).NotTo(HaveOccurred())
	Expect(files).To(HaveLen(len(Kinds)))
	crds := make([]*apiextensionsv1.CustomResourceDefinition, 0, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		Expect(err).NotTo(HaveOccurred())
		crd := &apiextensionsv1.CustomResourceDefinition{}
		Expect(yaml.UnmarshalStrict(raw, crd)).To(Succeed())
		crds = append(crds, crd)
	}
	return crds
}

// installCRDs0_9 installs the CRDs 0.9 shipped and creates one object of every kind at v1beta1,
// status included.
func installCRDs0_9() {
	GinkgoHelper()
	for _, crd := range readCRDs(filepath.Join("testdata", "crds-0.9")) {
		// A CRD that an earlier teardown failed to remove can still be terminating; its name is
		// free again once the API server has removed it.
		Eventually(func() error { return k8sClient.Create(ctx, crd.DeepCopy()) }, crdRemovalTimeout).
			Should(Succeed())
	}
	for _, f := range fixtures {
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(gvk("v1beta1", f.kind))
		obj.SetName(f.name)
		obj.SetNamespace(f.namespace)
		Expect(unstructured.SetNestedField(obj.Object, f.spec, "spec")).To(Succeed())
		Eventually(func() error { return k8sClient.Create(ctx, obj) }).Should(Succeed())
		Expect(unstructured.SetNestedField(obj.Object, f.status, "status")).To(Succeed())
		Expect(k8sClient.Status().Update(ctx, obj)).To(Succeed())
	}
}

// upgradeCRDs applies the CRDs of this release over the installed ones, as the upgrade guide
// does before helm upgrade, and waits until v1 is served.
func upgradeCRDs() {
	GinkgoHelper()
	for _, crd := range readCRDs(filepath.Join("..", "..", "config", "crd", "bases")) {
		Eventually(func() error {
			current := &apiextensionsv1.CustomResourceDefinition{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: crd.Name}, current); err != nil {
				return err
			}
			current.Spec = crd.Spec
			return k8sClient.Update(ctx, current)
		}).Should(Succeed())
	}
	for _, f := range fixtures {
		Eventually(func() error { return get("v1", f.kind, f.namespace, f.name, &unstructured.Unstructured{}) }).
			Should(Succeed())
	}
}

// removeCRDs deletes the objects, then the CRDs, and waits until the CRDs are gone.
//
// Deleting the CRDs alone leaves the objects to the API server's CRD finalizer, which lists and
// deletes them through the CRD's storage. That cannot work while the conversion is a webhook
// nobody serves and objects are stored at v1beta1, the state a spec that fails half way may
// leave. So the conversion goes back to None first, which reads every object whatever version
// it is stored at, and the objects are deleted through the API: the finalizer finds nothing to
// do, and a storage that has just served requests.
func removeCRDs() {
	GinkgoHelper()
	start := time.Now()
	none := &apiextensionsv1.CustomResourceConversion{Strategy: apiextensionsv1.NoneConverter}
	for _, k := range Kinds {
		// A CRD that is not there is fine: a BeforeAll that failed half way is cleaned up too.
		Eventually(func() error { return client.IgnoreNotFound(setConversionOf(k, none)) }, crdRemovalTimeout).
			Should(Succeed())
	}
	for _, k := range Kinds {
		Eventually(func() ([]string, error) { return deleteObjects(k) }, crdRemovalTimeout).
			Should(BeEmpty(), "%s objects are still there after being deleted", k.Kind)
	}
	emptied := time.Now()
	for _, k := range Kinds {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		crd.Name = k.CRDName()
		Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, crd))).To(Succeed())
	}
	waitForCRDsGone()
	GinkgoWriter.Printf("removed the objects in %s and the CRDs in %s\n",
		emptied.Sub(start).Round(time.Millisecond), time.Since(emptied).Round(time.Millisecond))
}

// waitForCRDsGone waits until the API server has removed the CRDs that were deleted.
func waitForCRDsGone() {
	GinkgoHelper()
	Eventually(askingAgain(func() ([]string, error) { return remainingCRDs(), nil }), crdRemovalTimeout,
		50*time.Millisecond).Should(BeEmpty(), "CRDs are still there after being deleted")
}

// askingAgain is f for an Eventually that waits for the API server to remove deleted CRDs, or
// to say why it cannot: once a second it also asks the API server to try again.
//
// It has to ask because the API server can lose its own retry (#127, seen with 1.37). The CRD
// finalizer's first attempt fails with "storage is (re)initializing" when it is the first to
// use the CRD's storage after a change of the CRD, before the storage's watch cache is filled.
// It retries milliseconds later, and when that retry reads the CRD before the finalizer's own
// update of the Terminating condition has reached its informer, the write is a conflict, which
// the finalizer takes for a change somebody else made and will tell it about. Nobody does: it
// ignores updates that differ in that condition only. The CRD then stays as it is, with nothing
// wrong with it, until the informer's next resync, which comes every five minutes: that is how
// the teardown came to wait three minutes in vain on a busy runner. Any other change to the
// CRD makes the finalizer try again at once, and an annotation is one.
func askingAgain[T any](f func() (T, error)) func() (T, error) {
	asked := time.Now()
	return func() (T, error) {
		if time.Since(asked) >= time.Second {
			asked = time.Now()
			if err := retryRemoval(); err != nil {
				var zero T
				return zero, err
			}
		}
		return f()
	}
}

// retryRemoval changes an annotation of every CRD that is deleted and still there.
func retryRemoval() error {
	for _, k := range Kinds {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		if err := k8sClient.Get(ctx, client.ObjectKey{Name: k.CRDName()}, crd); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		if crd.DeletionTimestamp == nil {
			continue
		}
		GinkgoWriter.Printf("%s is still there: asking the API server to try again\n", crd.Name)
		crd.SetAnnotations(map[string]string{"retry-removal": time.Now().Format(time.RFC3339Nano)})
		// Gone, or changed by the finalizer meanwhile: the next round sees which.
		if err := k8sClient.Update(ctx, crd); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return err
		}
	}
	return nil
}

// deleteObjects deletes every object of a kind, at the version the CRD stores, and returns
// the names it found: none once they are gone, or when the CRD is not installed.
func deleteObjects(k Kind) ([]string, error) {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: k.CRDName()}, crd); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	version := ""
	for _, v := range crd.Spec.Versions {
		if v.Storage {
			version = v.Name
		}
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(gvk(version, k.Kind+"List"))
	if err := k8sClient.List(ctx, list); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, client.ObjectKeyFromObject(&list.Items[i]).String())
		if err := client.IgnoreNotFound(k8sClient.Delete(ctx, &list.Items[i])); err != nil {
			return nil, err
		}
	}
	return names, nil
}

// remainingCRDs describes every CRD of the group that still exists, with its conditions: a CRD
// that does not go away says why in Terminating, and "Expected false to be true" does not.
func remainingCRDs() []string {
	var remaining []string
	for _, k := range Kinds {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		err := k8sClient.Get(ctx, client.ObjectKey{Name: k.CRDName()}, crd)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			remaining = append(remaining, fmt.Sprintf("%s: %v", k.CRDName(), err))
			continue
		}
		desc := fmt.Sprintf("%s: conversion %s, storedVersions %v", crd.Name, crd.Spec.Conversion.Strategy,
			crd.Status.StoredVersions)
		if crd.DeletionTimestamp != nil {
			desc += ", deleted at " + crd.DeletionTimestamp.Format(time.RFC3339)
		}
		for _, c := range crd.Status.Conditions {
			desc += fmt.Sprintf("; %s=%s since %s, %s: %s", c.Type, c.Status,
				c.LastTransitionTime.Format(time.RFC3339), c.Reason, c.Message)
		}
		remaining = append(remaining, desc)
	}
	return remaining
}

// crdRemovalTimeout is how long each step of removing the CRDs may take: the conversion back to
// None, a kind's objects deleted, the CRDs gone. On the CI runner (4 vCPU), in 24 teardowns
// alone and next to other envtest suites, the objects took 10 ms to 2.3 s (the seconds are the
// client waiting out a "storage is (re)initializing" after a spec changed the conversion) and
// the CRDs 11 ms to 0.35 s; on two cores shared with fourteen busy loops, 6.4 s and 2.3 s at
// most. Thirty seconds is ten times the slowest step seen on the runner. A removal that takes
// longer is stuck, not slow: the three minutes this used to be were never enough for the lost
// retry described at askingAgain, which lasts until the API server's next five-minute resync.
const crdRemovalTimeout = 30 * time.Second

func get(version, kind, namespace, name string, obj *unstructured.Unstructured) error {
	obj.SetGroupVersionKind(gvk(version, kind))
	return k8sClient.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj)
}

// listEvery lists every kind at a version and returns the first error.
func listEvery(version string) error {
	for _, k := range Kinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gvk(version, k.Kind+"List"))
		if err := k8sClient.List(ctx, list); err != nil {
			return err
		}
	}
	return nil
}

func storedVersionsOf(k Kind) []string {
	GinkgoHelper()
	crd := &apiextensionsv1.CustomResourceDefinition{}
	Expect(k8sClient.Get(ctx, client.ObjectKey{Name: k.CRDName()}, crd)).To(Succeed())
	return crd.Status.StoredVersions
}

// setConversion sets every CRD's conversion. With a URL, the API server has to call a webhook
// for every object it serves at a version other than the one the object is stored at; the
// URL here answers nothing, so such a read fails. That is how the specs see, through the API
// alone, at which version an object is stored.
func setConversion(url string) {
	GinkgoHelper()
	conversion := &apiextensionsv1.CustomResourceConversion{Strategy: apiextensionsv1.NoneConverter}
	if url != "" {
		conversion = &apiextensionsv1.CustomResourceConversion{
			Strategy: apiextensionsv1.WebhookConverter,
			Webhook: &apiextensionsv1.WebhookConversion{
				ClientConfig:             &apiextensionsv1.WebhookClientConfig{URL: new(url)},
				ConversionReviewVersions: []string{"v1"},
			},
		}
	}
	for _, k := range Kinds {
		Eventually(func() error { return setConversionOf(k, conversion) }).Should(Succeed())
	}
}

func setConversionOf(k Kind, conversion *apiextensionsv1.CustomResourceConversion) error {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: k.CRDName()}, crd); err != nil {
		return err
	}
	crd.Spec.Conversion = conversion
	return k8sClient.Update(ctx, crd)
}

const deadWebhook = "https://127.0.0.1:1/convert"

var testCAs = map[string][]byte{}

// testCA is a self-signed CA certificate in PEM, the same one for the same name: the API server
// refuses a caBundle it cannot parse.
func testCA(name string) []byte {
	GinkgoHelper()
	return testCAFor(name, time.Now().Add(30*24*time.Hour))
}

// testCAFor is testCA with the moment the certificate expires, for the first use of a name.
func testCAFor(name string, notAfter time.Time) []byte {
	GinkgoHelper()
	if ca, ok := testCAs[name]; ok {
		return ca
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	Expect(err).NotTo(HaveOccurred())
	testCAs[name] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return testCAs[name]
}

var _ = Describe("MigrateStorage", Ordered, func() {
	var recorder *events.FakeRecorder

	BeforeAll(func() {
		installCRDs0_9()
		recorder = events.NewFakeRecorder(20)
	})
	AfterAll(removeCRDs)

	It("waits for the CRDs of this release and changes nothing before", func() {
		u := &Upgrader{Client: k8sClient, Recorder: recorder}
		_, err := u.MigrateStorage(ctx)
		Expect(err).To(MatchError(ContainSubstring("apply the CRDs of this release first")))
		for _, k := range Kinds {
			Expect(storedVersionsOf(k)).To(Equal([]string{"v1beta1"}))
		}
		Expect(recorder.Events).To(BeEmpty())
	})

	It("finds the objects stored as v1beta1 once the CRDs are upgraded", func() {
		upgradeCRDs()
		for _, k := range Kinds {
			Expect(storedVersionsOf(k)).To(ConsistOf("v1beta1", "v1"), k.Kind)
		}
		// The control for the check below: while the objects are stored as v1beta1, a read at
		// v1 needs a conversion, and a dead webhook fails it.
		setConversion(deadWebhook)
		Eventually(func() error { return listEvery("v1") }).Should(HaveOccurred())
		setConversion("")
		Eventually(func() error { return listEvery("v1") }).Should(Succeed())
	})

	It("rewrites every object at v1, unchanged, and trims storedVersions to [v1]", func() {
		before := map[string]*unstructured.Unstructured{}
		for _, f := range fixtures {
			obj := &unstructured.Unstructured{}
			Expect(get("v1", f.kind, f.namespace, f.name, obj)).To(Succeed())
			before[f.kind] = obj
		}

		counted := map[string]float64{}
		for _, k := range Kinds {
			counted[k.Kind] = testutil.ToFloat64(rewritten.WithLabelValues(k.Kind))
		}
		u := &Upgrader{Client: k8sClient, Recorder: recorder}
		results, err := u.MigrateStorage(ctx)
		Expect(err).NotTo(HaveOccurred())
		Expect(results).To(HaveLen(len(Kinds)))
		for _, r := range results {
			Expect(r.Rewritten).To(Equal(1), r.Kind.Kind)
			Expect(r.Trimmed).To(BeTrue(), r.Kind.Kind)
			Expect(r.StoredVersions).To(Equal([]string{"v1"}))
			Expect(storedVersionsOf(r.Kind)).To(Equal([]string{"v1"}))
			Expect(testutil.ToFloat64(storedVersions.WithLabelValues(r.Kind.Kind, "v1"))).To(Equal(1.0))
			Expect(storedVersions.DeleteLabelValues(r.Kind.Kind, "v1beta1")).To(BeFalse(), "v1beta1 is still exported")
		}
		for _, k := range Kinds {
			Expect(testutil.ToFloat64(rewritten.WithLabelValues(k.Kind)) - counted[k.Kind]).To(Equal(1.0))
		}
		for range Kinds {
			var e string
			Eventually(recorder.Events).Should(Receive(&e))
			Expect(e).To(ContainSubstring(EventStorageMigrated))
			Expect(e).To(ContainSubstring("from [v1beta1 v1] to [v1]"))
		}

		for _, f := range fixtures {
			after := &unstructured.Unstructured{}
			Expect(get("v1", f.kind, f.namespace, f.name, after)).To(Succeed())
			old := before[f.kind]
			Expect(after.GetResourceVersion()).NotTo(Equal(old.GetResourceVersion()), "%s was not written", f.kind)
			Expect(after.GetGeneration()).To(Equal(old.GetGeneration()), f.kind)
			Expect(after.Object["spec"]).To(Equal(old.Object["spec"]), f.kind)
			Expect(after.Object["status"]).To(Equal(old.Object["status"]), f.kind)
		}
	})

	It("leaves nothing stored as v1beta1", func() {
		setConversion(deadWebhook)
		// Once the dead webhook is in effect, reads at v1beta1 fail: the objects are stored at
		// v1 and need a conversion to be served at v1beta1...
		Eventually(func() error { return listEvery("v1beta1") }).Should(HaveOccurred())
		// ...and reads at v1 need none, so every object is stored at v1.
		Expect(listEvery("v1")).To(Succeed())
		setConversion("")
		Eventually(func() error { return listEvery("v1beta1") }).Should(Succeed())
	})

	It("does nothing when run again", func() {
		rvs := map[string]string{}
		for _, f := range fixtures {
			obj := &unstructured.Unstructured{}
			Expect(get("v1", f.kind, f.namespace, f.name, obj)).To(Succeed())
			rvs[f.kind] = obj.GetResourceVersion()
		}
		u := &Upgrader{Client: k8sClient, Recorder: recorder}
		results, err := u.MigrateStorage(ctx)
		Expect(err).NotTo(HaveOccurred())
		for _, r := range results {
			Expect(r.Rewritten).To(BeZero())
			Expect(r.Trimmed).To(BeFalse())
		}
		for _, f := range fixtures {
			obj := &unstructured.Unstructured{}
			Expect(get("v1", f.kind, f.namespace, f.name, obj)).To(Succeed())
			Expect(obj.GetResourceVersion()).To(Equal(rvs[f.kind]), f.kind)
		}
		Consistently(recorder.Events, "200ms").ShouldNot(Receive())
	})

	It("keeps v1beta1 readable, with the same content", func() {
		for _, f := range fixtures {
			obj := &unstructured.Unstructured{}
			Expect(get("v1beta1", f.kind, f.namespace, f.name, obj)).To(Succeed())
			Expect(obj.GetAPIVersion()).To(Equal(group + "/v1beta1"))
			v1 := &unstructured.Unstructured{}
			Expect(get("v1", f.kind, f.namespace, f.name, v1)).To(Succeed())
			Expect(obj.Object["spec"]).To(Equal(v1.Object["spec"]), f.kind)
			Expect(obj.Object["status"]).To(Equal(v1.Object["status"]), f.kind)
		}
	})
})

var _ = Describe("ConfigureConversion", Ordered, func() {
	var (
		caFile   string
		recorder *events.FakeRecorder
		service  = types.NamespacedName{Namespace: "subnets", Name: "subnet-operator-webhook"}
	)

	BeforeAll(func() {
		installCRDs0_9()
		upgradeCRDs()
		caFile = filepath.Join(GinkgoT().TempDir(), "ca.crt")
		Expect(os.WriteFile(caFile, testCA("first"), 0o600)).To(Succeed())
		recorder = events.NewFakeRecorder(50)
	})
	AfterAll(removeCRDs)

	conversionOf := func(k Kind) *apiextensionsv1.CustomResourceConversion {
		GinkgoHelper()
		crd := &apiextensionsv1.CustomResourceDefinition{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: k.CRDName()}, crd)).To(Succeed())
		return crd.Spec.Conversion
	}

	It("points every CRD at the webhook, with the CA", func() {
		u := &Upgrader{Client: k8sClient, Recorder: recorder, Conversion: ConversionWebhook, Service: service,
			CAFile: caFile}
		Expect(u.ConfigureConversion(ctx)).To(Succeed())
		for _, k := range Kinds {
			c := conversionOf(k)
			Expect(c.Strategy).To(Equal(apiextensionsv1.WebhookConverter))
			ref := c.Webhook.ClientConfig.Service
			Expect(ref.Namespace).To(Equal("subnets"))
			Expect(ref.Name).To(Equal("subnet-operator-webhook"))
			Expect(*ref.Path).To(Equal("/convert"))
			Expect(*ref.Port).To(Equal(int32(443)))
			Expect(c.Webhook.ClientConfig.CABundle).To(Equal(testCA("first")))
			Expect(c.Webhook.ConversionReviewVersions).To(Equal([]string{"v1"}))
		}
		for range Kinds {
			Eventually(recorder.Events).Should(Receive(ContainSubstring(EventConversionConfigured)))
		}
	})

	It("does not write a CRD that is already right", func() {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: Kinds[0].CRDName()}, crd)).To(Succeed())
		u := &Upgrader{Client: k8sClient, Recorder: recorder, Conversion: ConversionWebhook, Service: service,
			CAFile: caFile}
		Expect(u.ConfigureConversion(ctx)).To(Succeed())
		again := &apiextensionsv1.CustomResourceDefinition{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: Kinds[0].CRDName()}, again)).To(Succeed())
		Expect(again.ResourceVersion).To(Equal(crd.ResourceVersion))
		Consistently(recorder.Events, "200ms").ShouldNot(Receive())
	})

	It("follows a renewed CA", func() {
		Expect(os.WriteFile(caFile, testCA("second"), 0o600)).To(Succeed())
		u := &Upgrader{Client: k8sClient, Conversion: ConversionWebhook, Service: service, CAFile: caFile}
		Expect(u.ConfigureConversion(ctx)).To(Succeed())
		for _, k := range Kinds {
			Expect(conversionOf(k).Webhook.ClientConfig.CABundle).To(Equal(testCA("second")))
		}
	})

	It("falls back to None without a CA, and says so", func() {
		u := &Upgrader{Client: k8sClient, Conversion: ConversionWebhook, Service: service,
			CAFile: filepath.Join(GinkgoT().TempDir(), "missing")}
		Expect(u.ConfigureConversion(ctx)).To(MatchError(ContainSubstring("no CA for the conversion webhook")))
		for _, k := range Kinds {
			Expect(conversionOf(k).Strategy).To(Equal(apiextensionsv1.NoneConverter))
		}
	})

	It("sets None when asked to, and leaves the CRDs alone when not managing them", func() {
		u := &Upgrader{Client: k8sClient, Conversion: ConversionWebhook, Service: service, CAFile: caFile}
		Expect(u.ConfigureConversion(ctx)).To(Succeed())
		u = &Upgrader{Client: k8sClient, Conversion: ConversionUnmanaged}
		Expect(u.ConfigureConversion(ctx)).To(Succeed())
		Expect(conversionOf(Kinds[0]).Strategy).To(Equal(apiextensionsv1.WebhookConverter))
		u = &Upgrader{Client: k8sClient, Conversion: ConversionNone}
		Expect(u.ConfigureConversion(ctx)).To(Succeed())
		for _, k := range Kinds {
			Expect(conversionOf(k).Strategy).To(Equal(apiextensionsv1.NoneConverter))
			Expect(conversionOf(k).Webhook).To(BeNil())
		}
	})
})

var _ = Describe("Start", Ordered, func() {
	BeforeAll(installCRDs0_9)
	AfterAll(removeCRDs)

	It("migrates once the CRDs are upgraded, and only then turns the webhook on", func() {
		caFile := filepath.Join(GinkgoT().TempDir(), "ca.crt")
		Expect(os.WriteFile(caFile, testCA("start"), 0o600)).To(Succeed())
		u := &Upgrader{Client: k8sClient, Conversion: ConversionWebhook, CAFile: caFile, Interval: 200 * time.Millisecond,
			Service: types.NamespacedName{Namespace: "subnets", Name: "webhook"}}
		runCtx, stop := contextWithCancel()
		done := make(chan error)
		go func() { done <- u.Start(runCtx) }()

		// The operator is upgraded before its CRDs: it waits, and leaves the conversion alone,
		// because objects stored as v1beta1 may still need it.
		Consistently(func() apiextensionsv1.ConversionStrategyType {
			crd := &apiextensionsv1.CustomResourceDefinition{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: Kinds[0].CRDName()}, crd)).To(Succeed())
			return crd.Spec.Conversion.Strategy
		}, "1s").Should(Equal(apiextensionsv1.NoneConverter))

		upgradeCRDs()
		Eventually(func() []string { return storedVersionsOf(Kinds[len(Kinds)-1]) }, 10*time.Second).
			Should(Equal([]string{"v1"}))
		Eventually(func() apiextensionsv1.ConversionStrategyType {
			crd := &apiextensionsv1.CustomResourceDefinition{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: Kinds[0].CRDName()}, crd)).To(Succeed())
			return crd.Spec.Conversion.Strategy
		}, 10*time.Second).Should(Equal(apiextensionsv1.WebhookConverter))

		stop()
		Eventually(done).Should(Receive(BeNil()))
		setConversion("")
	})

	It("refuses a webhook without a Service", func() {
		u := &Upgrader{Client: k8sClient, Conversion: ConversionWebhook}
		Expect(u.Start(ctx)).To(MatchError(ContainSubstring("Service")))
		u = &Upgrader{Client: k8sClient, Conversion: "sometimes"}
		Expect(u.Start(ctx)).To(MatchError(ContainSubstring("unknown")))
	})
})

// The kinds here and the CRDs the operator ships must be the same set: a kind left out would
// keep v1beta1 in its storedVersions for ever, and the RBAC names the CRDs one by one.
var _ = Describe("Kinds", func() {
	It("covers every CRD of the group, and the RBAC names each", func() {
		names := make([]string, 0, len(Kinds))
		for _, crd := range readCRDs(filepath.Join("..", "..", "config", "crd", "bases")) {
			names = append(names, crd.Name)
		}
		Expect(CRDNames()).To(ConsistOf(names))

		role, err := os.ReadFile(filepath.Join("..", "..", "config", "rbac", "role.yaml"))
		Expect(err).NotTo(HaveOccurred())
		for _, name := range names {
			Expect(strings.Count(string(role), "- "+name)).To(Equal(2), name)
		}
	})
})
