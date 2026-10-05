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

package migration

import (
	"context"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/prometheus/client_golang/prometheus/testutil"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	kevents "k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
)

// oldObject is an aws.hypersurgery/v1alpha1 object as 0.8 left it: migrated (marked
// migrated-to) or not.
func oldObject(kind, namespace, name string, migrated bool, spec map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	obj.SetAPIVersion(OldAPIVersion)
	obj.SetKind(kind)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	if migrated {
		obj.SetAnnotations(map[string]string{networkv1.AnnotationMigratedTo: name})
	}
	return obj
}

func oldClaim(name string, migrated bool) *unstructured.Unstructured {
	return oldObject("SubnetClaim", "default", name, migrated, map[string]any{
		"scopeRef": "organization", "account": "111111111111", "region": "eu-central-1", "vpcID": "vpc-0abc",
		"prefixLength": int64(24), "availabilityZones": []any{"eu-central-1a"}, "owner": "team-payments",
	})
}

func oldScopeObject(name string, migrated bool) *unstructured.Unstructured {
	return oldObject("NetworkScope", "", name, migrated, map[string]any{
		"accounts": []any{map[string]any{"id": "111111111111"}}, "regions": []any{"eu-central-1"},
	})
}

func pending(kind string) float64 {
	return testutil.ToFloat64(pendingObjects.WithLabelValues(kind))
}

// The guard is what keeps 0.9 from running next to state it would ignore. The specs build the
// situations an upgrade from 0.8 can leave behind and check what the guard says about them.
var _ = Describe("The guard against unmigrated aws.hypersurgery/v1alpha1 objects", Ordered, func() {
	var created []client.Object

	create := func(obj client.Object) {
		GinkgoHelper()
		Expect(k8sClient.Create(ctx, obj)).To(Succeed())
		created = append(created, obj)
	}

	AfterEach(func() {
		for _, obj := range created {
			if u, ok := obj.(*unstructured.Unstructured); ok && len(u.GetFinalizers()) > 0 {
				Expect(client.IgnoreNotFound(k8sClient.Patch(ctx, u,
					client.RawPatch(types.MergePatchType, []byte(`{"metadata":{"finalizers":null}}`))))).To(Succeed())
			}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
		created = nil
	})

	It("lets the operator run when every old object was migrated", func() {
		create(oldScopeObject("migrated", true))
		create(oldClaim("migrated", true))

		scan, err := FindUnmigrated(ctx, k8sClient)
		Expect(err).NotTo(HaveOccurred())
		Expect(scan.Served).To(ConsistOf("NetworkScope", "SubnetClaim", "ResourceImport", "SheetExport"))
		Expect(scan.Objects).To(BeEmpty())

		guard := &Guard{Client: k8sClient}
		Expect(guard.Check(ctx)).To(BeTrue())
		Expect(guard.Ready(nil)).To(Succeed())
		for _, kind := range oldKinds {
			Expect(pending(kind)).To(BeZero(), kind)
		}
	})

	It("holds the operator back while an object was never migrated, and says which and what to do", func() {
		create(oldScopeObject("migrated", true))
		create(oldClaim("payments", false))
		create(oldScopeObject("forgotten", false))
		// Being deleted, so on its way out whatever happens: it holds nothing back.
		leaving := oldClaim("leaving", false)
		leaving.SetFinalizers([]string{"example.com/hold"})
		create(leaving)
		Expect(k8sClient.Delete(ctx, leaving)).To(Succeed())

		recorder := kevents.NewFakeRecorder(10)
		guard := &Guard{Client: k8sClient, Recorder: recorder}
		Expect(guard.Check(ctx)).To(BeFalse())

		Expect(pending("SubnetClaim")).To(Equal(1.0))
		Expect(pending("NetworkScope")).To(Equal(1.0))
		Expect(pending("ResourceImport")).To(BeZero())

		err := guard.Ready(nil)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("2 aws.hypersurgery/v1alpha1 object(s) were never migrated"))
		Expect(err.Error()).To(ContainSubstring("NetworkScope forgotten, SubnetClaim default/payments"))
		Expect(err.Error()).To(ContainSubstring("Roll back to 0.8.x"))
		Expect(err.Error()).To(ContainSubstring(networkv1.AnnotationMigratedTo))

		By("recording a Warning on each of them, once")
		Expect(recorder.Events).To(HaveLen(2))
		for range 2 {
			Expect(<-recorder.Events).To(SatisfyAll(HavePrefix("Warning MigrationPending "),
				ContainSubstring("migrate-manifests")))
		}
		Expect(guard.Check(ctx)).To(BeFalse())
		Expect(recorder.Events).To(BeEmpty(), "the same objects are not warned about again")
	})

	It("records the Event on the old object itself", func() {
		claim := oldClaim("described", false)
		create(claim)

		recorder := &capturingRecorder{}
		guard := &Guard{Client: k8sClient, Recorder: recorder}
		Expect(guard.Check(ctx)).To(BeFalse())
		Expect(recorder.objects).To(HaveLen(1))
		ref := recorder.objects[0]
		Expect(ref.GetObjectKind().GroupVersionKind().GroupVersion().String()).To(Equal(OldAPIVersion))
		Expect(ref.GetObjectKind().GroupVersionKind().Kind).To(Equal("SubnetClaim"))
		Expect(ref.(client.Object).GetUID()).To(Equal(claim.GetUID()))
	})

	It("lets go once the last one is migrated or deleted", func() {
		claim := oldClaim("late", false)
		create(claim)

		guard := &Guard{Client: k8sClient, Interval: 100 * time.Millisecond}
		Expect(guard.Check(ctx)).To(BeFalse())

		done := make(chan error, 1)
		go func() { done <- guard.Start(ctx) }()
		Consistently(done, 500*time.Millisecond).ShouldNot(Receive(), "the object is still there")

		By("marking it the way 0.8 does once its copy exists")
		claim.SetAnnotations(map[string]string{networkv1.AnnotationMigratedTo: "late"})
		Expect(k8sClient.Update(ctx, claim)).To(Succeed())
		Eventually(done, 5*time.Second).Should(Receive(MatchError(ErrCleared)))
		Expect(guard.Ready(nil)).To(Succeed())
		Expect(pending("SubnetClaim")).To(BeZero())
	})

	It("stops when the manager does, without claiming anything was cleared", func() {
		create(oldClaim("stuck", false))
		guard := &Guard{Client: k8sClient, Interval: 50 * time.Millisecond}
		stop, cancelGuard := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- guard.Start(stop) }()
		cancelGuard()
		Eventually(done).Should(Receive(BeNil()))
	})

	It("does not let an operator that may not look run blind", func() {
		create(oldClaim("hidden", false))
		// A user with no RBAC at all, like an operator whose ClusterRole was written by hand
		// and lacks the old group.
		impersonating := rest.CopyConfig(cfg)
		impersonating.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:x:no-rbac"}
		blind, err := client.New(impersonating, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())

		_, err = FindUnmigrated(ctx, blind)
		Expect(apierrors.IsForbidden(err)).To(BeTrue(), "%v", err)
		guard := &Guard{Client: blind}
		Expect(guard.Check(ctx)).To(BeFalse())
		Expect(guard.Ready(nil)).To(MatchError(ContainSubstring("could not check")))
	})

	It("keeps counting while the operator runs, and warns about an old object applied after the start", func() {
		recorder := kevents.NewFakeRecorder(10)
		watcher := &Watcher{Client: k8sClient, Recorder: recorder, Interval: 100 * time.Millisecond}
		stop, cancelWatcher := context.WithCancel(ctx)
		defer cancelWatcher()
		done := make(chan error, 1)
		go func() { done <- watcher.Start(stop) }()

		Eventually(func() float64 { return pending("SubnetClaim") }).Should(BeZero())
		create(oldClaim("reapplied", false))
		Eventually(func() float64 { return pending("SubnetClaim") }, 5*time.Second).Should(Equal(1.0))
		Eventually(recorder.Events, 5*time.Second).Should(Receive(HavePrefix("Warning MigrationPending")))
		Consistently(done, 300*time.Millisecond).ShouldNot(Receive(), "an ignored object does not stop the operator")
	})

	It("finds nothing, and stops looking, once the old CRDs are deleted", func() {
		watcher := &Watcher{Client: k8sClient, Interval: 100 * time.Millisecond}
		stop, cancelWatcher := context.WithCancel(ctx)
		defer cancelWatcher()
		done := make(chan error, 1)
		go func() { done <- watcher.Start(stop) }()
		Consistently(done, 300*time.Millisecond).ShouldNot(Receive())

		create(oldClaim("goes-with-its-crd", false))
		for _, crd := range oldCRDs() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, crd))).To(Succeed())
		}
		created = nil
		Eventually(func(g Gomega) {
			scan, err := FindUnmigrated(ctx, k8sClient)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(scan.Served).To(BeEmpty())
			g.Expect(scan.Objects).To(BeEmpty())
		}, 30*time.Second).Should(Succeed())
		Eventually(done, 30*time.Second).Should(Receive(BeNil()))
		for _, kind := range oldKinds {
			Expect(pending(kind)).To(BeZero(), kind)
		}

		By("answering the same to a client that never saw the old group")
		fresh, err := client.New(cfg, client.Options{Scheme: scheme.Scheme})
		Expect(err).NotTo(HaveOccurred())
		scan, err := FindUnmigrated(ctx, fresh)
		Expect(err).NotTo(HaveOccurred())
		Expect(scan.Served).To(BeEmpty())
		Expect((&Guard{Client: fresh}).Check(ctx)).To(BeTrue())
	})
})

// oldCRDs are the CRDs of the old group that 0.8 shipped, as the suite installed them.
func oldCRDs() []client.Object {
	GinkgoHelper()
	files, err := filepath.Glob(filepath.Join("testdata", "crds-0.8", "*.yaml"))
	Expect(err).NotTo(HaveOccurred())
	crds := make([]client.Object, 0, len(files))
	for _, f := range files {
		raw, err := os.ReadFile(f)
		Expect(err).NotTo(HaveOccurred())
		crd := &apiextensionsv1.CustomResourceDefinition{}
		Expect(yaml.Unmarshal(raw, crd)).To(Succeed())
		crds = append(crds, crd)
	}
	Expect(crds).To(HaveLen(4))
	return crds
}

// capturingRecorder keeps the objects Events were recorded on.
type capturingRecorder struct {
	objects []runtime.Object
}

func (r *capturingRecorder) Eventf(regarding, _ runtime.Object, _, _, _, _ string, _ ...any) {
	r.objects = append(r.objects, regarding)
}
