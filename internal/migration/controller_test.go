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
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
)

func reconcilerFor(kind string) *Reconciler {
	for _, k := range kinds {
		if k.name == kind {
			return &Reconciler{Client: k8sClient, APIReader: k8sClient, kind: k}
		}
	}
	Fail("no kind " + kind)
	return nil
}

func migrate(kind string, obj client.Object) ctrl.Result {
	GinkgoHelper()
	res, err := reconcilerFor(kind).Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(obj)})
	Expect(err).NotTo(HaveOccurred())
	return res
}

var counter int

var _ = Describe("The migration controller", func() {
	var (
		scopeName, ns string
		synced        metav1.Time
	)

	BeforeEach(func() {
		counter++
		scopeName = fmt.Sprintf("org-%d", counter)
		ns = fmt.Sprintf("team-%d", counter)
		synced = metav1.NewTime(time.Now().Add(-time.Minute).Truncate(time.Second))
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
	})

	oldScope := func() *awsv1alpha1.NetworkScope {
		GinkgoHelper()
		s := &awsv1alpha1.NetworkScope{
			ObjectMeta: metav1.ObjectMeta{Name: scopeName},
			Spec: awsv1alpha1.NetworkScopeSpec{
				Accounts: []awsv1alpha1.AccountSpec{{ID: "111111111111"},
					{ID: "222222222222", RoleARN: "arn:aws:iam::222222222222:role/read"}},
				Regions:        []string{"eu-central-1"},
				VPCTagSelector: map[string]string{"hs/managed": "true"},
			},
		}
		Expect(k8sClient.Create(ctx, s)).To(Succeed())
		s.Status = awsv1alpha1.NetworkScopeStatus{ObservedGeneration: s.Generation, LastSyncTime: &synced, VPCs: 1,
			Subnets: 1, Unmanaged: 1, Targets: []awsv1alpha1.TargetStatus{{Account: "111111111111",
				Region: "eu-central-1", VPCs: 1, Subnets: 1, UnmanagedVPCs: 1, UnmanagedIDs: []string{"vpc-0dead"},
				LastSyncTime: &synced}}}
		Expect(k8sClient.Status().Update(ctx, s)).To(Succeed())
		return s
	}
	oldClaim := func() *awsv1alpha1.SubnetClaim {
		GinkgoHelper()
		c := &awsv1alpha1.SubnetClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "payments", Namespace: ns,
				Annotations: map[string]string{awsv1alpha1.AnnotationCreatedBy: "jane@example.com"}},
			Spec: awsv1alpha1.SubnetClaimSpec{ScopeRef: scopeName, Account: "111111111111", Region: "eu-central-1",
				VPCID: "vpc-0abc", PrefixLength: 24, AvailabilityZones: []string{"eu-central-1a", "eu-central-1b"},
				Mode: awsv1alpha1.ClaimModeAllocate, Owner: "team-payments", NamePrefix: "payments"},
		}
		Expect(k8sClient.Create(ctx, c)).To(Succeed())
		c.Status.Allocations = []awsv1alpha1.SubnetAllocation{
			{AvailabilityZone: "eu-central-1a", CIDRBlock: "10.0.0.0/24", State: awsv1alpha1.AllocationPending},
			{AvailabilityZone: "eu-central-1b", CIDRBlock: "10.0.1.0/24", State: awsv1alpha1.AllocationPending},
		}
		Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())
		return c
	}

	It("copies a scope, a claim, an import and an export with their status, then marks the old ones", func() {
		scope := oldScope()
		claim := oldClaim()

		applied := metav1.NewTime(synced.Add(-time.Hour))
		imp := &awsv1alpha1.ResourceImport{
			ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: ns,
				Annotations: map[string]string{awsv1alpha1.AnnotationCreatedBy: "joe@example.com"}},
			Spec: awsv1alpha1.ResourceImportSpec{ScopeRef: scopeName, Account: "111111111111", Region: "eu-central-1",
				ResourceID: "vpc-0abc", Tags: map[string]string{"hs/owner": "team-sandbox"}, RequestedBy: "joe"},
		}
		Expect(k8sClient.Create(ctx, imp)).To(Succeed())
		imp.Status = awsv1alpha1.ResourceImportStatus{State: awsv1alpha1.ImportApplied,
			AppliedTags: map[string]string{"hs/owner": "team-sandbox"}, AppliedTime: &applied}
		Expect(k8sClient.Status().Update(ctx, imp)).To(Succeed())

		exp := &awsv1alpha1.SheetExport{
			ObjectMeta: metav1.ObjectMeta{Name: scopeName},
			Spec: awsv1alpha1.SheetExportSpec{ScopeRef: scopeName, SpreadsheetID: "sheet",
				CredentialsSecretRef: awsv1alpha1.SecretKeyRef{Name: "google", Namespace: ns}},
		}
		Expect(k8sClient.Create(ctx, exp)).To(Succeed())

		By("the old scope's discovered objects, which the new scope rediscovers")
		labels := map[string]string{awsv1alpha1.LabelScope: scopeName}
		vpc := &awsv1alpha1.VPC{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("vpc-0%04d", counter), Labels: labels},
			Spec: awsv1alpha1.VPCSpec{VPCID: "vpc-0abc", Account: "111111111111", Region: "eu-central-1"}}
		Expect(k8sClient.Create(ctx, vpc)).To(Succeed())
		subnet := &awsv1alpha1.Subnet{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("subnet-0%04d", counter), Labels: labels},
			Spec: awsv1alpha1.SubnetSpec{SubnetID: "subnet-0abc", VPCID: "vpc-0abc", Account: "111111111111", Region: "eu-central-1"}}
		Expect(k8sClient.Create(ctx, subnet)).To(Succeed())

		By("a claim that comes before its scope waits for it")
		Expect(migrate("SubnetClaim", claim).RequeueAfter).To(Equal(scopeWaitInterval))
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), &networkv1beta1.SubnetClaim{}))).
			To(BeTrue())

		migrate("NetworkScope", scope)
		migrate("SubnetClaim", claim)
		migrate("ResourceImport", imp)
		migrate("SheetExport", exp)

		By("the scope: AWS, its roles in the aws member, every namespace, and what it knew")
		newScope := &networkv1beta1.NetworkScope{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Name: scopeName}, newScope)).To(Succeed())
		Expect(newScope.Spec.Provider).To(Equal(networkv1beta1.ProviderAWS))
		Expect(newScope.Spec.Accounts[1].AWS.RoleARN).To(Equal("arn:aws:iam::222222222222:role/read"))
		Expect(newScope.Spec.NamespaceSelector).NotTo(BeNil())
		Expect(newScope.Spec.NamespaceSelector.MatchLabels).To(BeEmpty())
		Expect(newScope.Spec.TagKeys.Owner).To(Equal("hs/owner"))
		Expect(newScope.Annotations).To(HaveKeyWithValue(networkv1beta1.AnnotationMigratedFrom, OldAPIVersion))
		Expect(newScope.Status.Targets).To(HaveLen(1))
		Expect(newScope.Status.Targets[0].UnmanagedIDs).To(ConsistOf("vpc-0dead"))
		Expect(newScope.Status.ObservedGeneration).To(BeZero(), "the first reconcile of the copy is a full sync")

		By("the claim: its reservations, keyed by subnet name, and its creator")
		newClaim := &networkv1beta1.SubnetClaim{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), newClaim)).To(Succeed())
		Expect(newClaim.Spec.NetworkID).To(Equal("vpc-0abc"))
		Expect(newClaim.Spec.Zones).To(ConsistOf("eu-central-1a", "eu-central-1b"))
		Expect(newClaim.Annotations).To(HaveKeyWithValue(networkv1beta1.AnnotationCreatedBy, "jane@example.com"))
		Expect(newClaim.Status.Allocations).To(ConsistOf(
			networkv1beta1.SubnetAllocation{Name: "payments-a", Zone: "eu-central-1a", CIDRBlock: "10.0.0.0/24",
				State: networkv1beta1.AllocationPending},
			networkv1beta1.SubnetAllocation{Name: "payments-b", Zone: "eu-central-1b", CIDRBlock: "10.0.1.0/24",
				State: networkv1beta1.AllocationPending},
		))

		By("the import: applied once, when it was")
		newImp := &networkv1beta1.ResourceImport{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(imp), newImp)).To(Succeed())
		Expect(newImp.Status.State).To(Equal(networkv1beta1.ImportApplied))
		Expect(newImp.Status.AppliedTime.Equal(&applied)).To(BeTrue())
		Expect(newImp.Annotations).To(HaveKeyWithValue(networkv1beta1.AnnotationCreatedBy, "joe@example.com"))

		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(exp), &networkv1beta1.SheetExport{})).To(Succeed())

		By("every old object pointing at its copy")
		for _, obj := range []client.Object{&awsv1alpha1.NetworkScope{}, &awsv1alpha1.SubnetClaim{},
			&awsv1alpha1.ResourceImport{}, &awsv1alpha1.SheetExport{}} {
			key := map[string]client.ObjectKey{
				"*v1alpha1.NetworkScope":   {Name: scopeName},
				"*v1alpha1.SubnetClaim":    client.ObjectKeyFromObject(claim),
				"*v1alpha1.ResourceImport": client.ObjectKeyFromObject(imp),
				"*v1alpha1.SheetExport":    {Name: scopeName},
			}[fmt.Sprintf("%T", obj)]
			Expect(k8sClient.Get(ctx, key, obj)).To(Succeed())
			Expect(obj.GetAnnotations()).To(HaveKeyWithValue(networkv1beta1.AnnotationMigratedTo, key.Name))
		}

		By("the old scope's VPC and Subnet objects gone, as a cache nobody updates any more")
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(vpc), &awsv1alpha1.VPC{}))).To(BeTrue())
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKeyFromObject(subnet), &awsv1alpha1.Subnet{}))).To(BeTrue())
	})

	It("does not copy an object again after its copy was deleted", func() {
		scope := oldScope()
		migrate("NetworkScope", scope)
		copied := &networkv1beta1.NetworkScope{ObjectMeta: metav1.ObjectMeta{Name: scopeName}}
		Expect(k8sClient.Delete(ctx, copied)).To(Succeed())

		migrate("NetworkScope", scope)
		Expect(apierrors.IsNotFound(k8sClient.Get(ctx, client.ObjectKey{Name: scopeName}, copied))).To(BeTrue(),
			"a migrated object is done; its copy was deleted on purpose")
	})

	It("keeps a copy applied from git before the migration, and gives it the old reservations", func() {
		oldScope()
		claim := oldClaim()
		migrate("NetworkScope", &awsv1alpha1.NetworkScope{ObjectMeta: metav1.ObjectMeta{Name: scopeName}})

		By("the same claim, converted with migrate-manifests and applied, with a spec change of its own")
		applied, _ := SubnetClaim(claim, ForManifest)
		applied.Status = networkv1beta1.SubnetClaimStatus{}
		applied.Spec.Tags = map[string]string{"cost-center": "42"}
		applied.ResourceVersion = ""
		Expect(k8sClient.Create(ctx, applied)).To(Succeed())

		migrate("SubnetClaim", claim)
		got := &networkv1beta1.SubnetClaim{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), got)).To(Succeed())
		Expect(got.Spec.Tags).To(HaveKeyWithValue("cost-center", "42"), "the applied spec wins")
		Expect(got.Status.Allocations).To(HaveLen(2), "and it gets the reservations it did not have")
	})

	It("leaves a copy that already has reservations of its own alone", func() {
		oldScope()
		claim := oldClaim()
		migrate("NetworkScope", &awsv1alpha1.NetworkScope{ObjectMeta: metav1.ObjectMeta{Name: scopeName}})

		applied, _ := SubnetClaim(claim, ForManifest)
		applied.ResourceVersion = ""
		applied.Status = networkv1beta1.SubnetClaimStatus{}
		Expect(k8sClient.Create(ctx, applied)).To(Succeed())
		applied.Status.Allocations = []networkv1beta1.SubnetAllocation{
			{Name: "payments-a", Zone: "eu-central-1a", CIDRBlock: "10.9.0.0/24"}}
		Expect(k8sClient.Status().Update(ctx, applied)).To(Succeed())

		migrate("SubnetClaim", claim)
		got := &networkv1beta1.SubnetClaim{}
		Expect(k8sClient.Get(ctx, client.ObjectKeyFromObject(claim), got)).To(Succeed())
		Expect(got.Status.Allocations).To(HaveLen(1))
		Expect(got.Status.Allocations[0].CIDRBlock).To(Equal("10.9.0.0/24"))
	})
})

var _ = Describe("Legacy", func() {
	It("says a new object waits while its old counterpart is not migrated, and not afterwards", func() {
		counter++
		name := fmt.Sprintf("legacy-%d", counter)
		old := &awsv1alpha1.NetworkScope{ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: awsv1alpha1.NetworkScopeSpec{Accounts: []awsv1alpha1.AccountSpec{{ID: "111111111111"}},
				Regions: []string{"eu-central-1"}}}
		Expect(k8sClient.Create(ctx, old)).To(Succeed())

		legacy := &Legacy{Client: k8sClient}
		pending, err := legacy.Pending(ctx, &networkv1beta1.NetworkScope{ObjectMeta: metav1.ObjectMeta{Name: name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(pending).To(BeTrue())

		migrate("NetworkScope", old)
		pending, err = legacy.Pending(ctx, &networkv1beta1.NetworkScope{ObjectMeta: metav1.ObjectMeta{Name: name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(pending).To(BeFalse())

		pending, err = legacy.Pending(ctx, &networkv1beta1.NetworkScope{ObjectMeta: metav1.ObjectMeta{Name: "never-old"}})
		Expect(err).NotTo(HaveOccurred())
		Expect(pending).To(BeFalse(), "an object that never had an old counterpart does not wait")
	})

	It("reports the reservations of unmigrated claims in one network", func() {
		counter++
		ns := fmt.Sprintf("legacy-%d", counter)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}})).To(Succeed())
		for i, vpc := range []string{"vpc-0aaa", "vpc-0bbb"} {
			c := &awsv1alpha1.SubnetClaim{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("c%d", i), Namespace: ns},
				Spec: awsv1alpha1.SubnetClaimSpec{ScopeRef: "x", Account: "111111111111", Region: "eu-central-1",
					VPCID: vpc, PrefixLength: 24, AvailabilityZones: []string{"eu-central-1a"}, Owner: "o"},
			}
			Expect(k8sClient.Create(ctx, c)).To(Succeed())
			c.Status.Allocations = []awsv1alpha1.SubnetAllocation{
				{AvailabilityZone: "eu-central-1a", CIDRBlock: fmt.Sprintf("10.%d.0.0/24", i)}}
			Expect(k8sClient.Status().Update(ctx, c)).To(Succeed())
		}
		used, err := (&Legacy{Client: k8sClient}).Reservations(ctx, "vpc-0aaa")
		Expect(err).NotTo(HaveOccurred())
		Expect(used).To(ContainElement("10.0.0.0/24"))
		Expect(used).NotTo(ContainElement("10.1.0.0/24"))
	})
})

// The examples of the last release, converted, must be accepted by the new CRDs: their schema
// and their CEL rules. Server-side dry run runs both without keeping anything.
var _ = Describe("Converted manifests", func() {
	It("are accepted by the network.hypersurgery.dev CRDs", func() {
		fixtures, err := filepath.Glob(filepath.Join("testdata", "v1alpha1", "*.yaml"))
		Expect(err).NotTo(HaveOccurred())
		converted := 0
		for _, fixture := range fixtures {
			in, err := os.ReadFile(fixture)
			Expect(err).NotTo(HaveOccurred())
			var out, notes bytes.Buffer
			Expect(Manifests(bytes.NewReader(in), &out, &notes)).To(Succeed())

			reader := utilyaml.NewYAMLReader(bufioReader(out.Bytes()))
			for {
				doc, err := reader.Read()
				if err != nil {
					break
				}
				obj := &unstructured.Unstructured{}
				if err := utilyaml.Unmarshal(doc, &obj.Object); err != nil || len(obj.Object) == 0 {
					continue
				}
				if obj.GroupVersionKind().Group != networkv1beta1.GroupVersion.Group {
					continue
				}
				if obj.GetNamespace() == "" && obj.GetKind() != "NetworkScope" && obj.GetKind() != "SheetExport" {
					obj.SetNamespace("default")
				}
				Expect(k8sClient.Create(ctx, obj, client.DryRunAll)).To(Succeed(), "%s from %s", obj.GetName(), fixture)
				converted++
			}
		}
		Expect(converted).To(Equal(7), "every converted object of the fixtures was sent")
	})

	It("match the examples and samples, which the CRDs accept as they are", func() {
		// The examples are what people copy; one the API server refuses is a bug in the docs.
		files, err := filepath.Glob(filepath.Join("..", "..", "examples", "0*.yaml"))
		Expect(err).NotTo(HaveOccurred())
		samples, err := filepath.Glob(filepath.Join("..", "..", "config", "samples", "network_*.yaml"))
		Expect(err).NotTo(HaveOccurred())
		sent := 0
		for _, file := range append(files, samples...) {
			in, err := os.ReadFile(file)
			Expect(err).NotTo(HaveOccurred())
			reader := utilyaml.NewYAMLReader(bufioReader(in))
			for {
				doc, err := reader.Read()
				if err != nil {
					break
				}
				obj := &unstructured.Unstructured{}
				if err := utilyaml.Unmarshal(doc, &obj.Object); err != nil || len(obj.Object) == 0 {
					continue
				}
				Expect(obj.GetAPIVersion()).NotTo(HavePrefix("aws.hypersurgery"), "%s still uses the old group", file)
				if obj.GroupVersionKind().Group != networkv1beta1.GroupVersion.Group {
					continue
				}
				Expect(k8sClient.Create(ctx, obj, client.DryRunAll)).To(Succeed(), "%s from %s", obj.GetName(), file)
				sent++
			}
		}
		Expect(sent).To(BeNumerically(">=", 10))
	})

	It("are refused when an AWS account ID is not 12 digits, by the CRD itself", func() {
		scope := &networkv1beta1.NetworkScope{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-account"},
			Spec: networkv1beta1.NetworkScopeSpec{Provider: networkv1beta1.ProviderAWS,
				Accounts: []networkv1beta1.Account{{ID: "12345"}}, Regions: []string{"eu-central-1"}},
		}
		err := k8sClient.Create(ctx, scope, client.DryRunAll)
		Expect(err).To(MatchError(ContainSubstring("an AWS account id is 12 digits")))
	})
})

func bufioReader(b []byte) *bufio.Reader { return bufio.NewReader(bytes.NewReader(b)) }
