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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kevents "k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
	"hypersurgery.dev/subnet-operator/internal/audit"
	"hypersurgery.dev/subnet-operator/internal/events"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// auditLines reads back what the sink under test collected.
func auditLines(buf *bytes.Buffer) []audit.Record {
	GinkgoHelper()
	var out []audit.Record
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec audit.Record
		Expect(json.Unmarshal([]byte(line), &rec)).To(Succeed(), line)
		out = append(out, rec)
	}
	return out
}

func linesFor(recs []audit.Record, resourceID string) []audit.Record {
	var out []audit.Record
	for _, rec := range recs {
		if rec.ResourceID == resourceID {
			out = append(out, rec)
		}
	}
	return out
}

// emittedEvents drains the fake recorder.
func emittedEvents(recorder *kevents.FakeRecorder) []string {
	var out []string
	for {
		select {
		case e := <-recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

func haveEvent(recorder *kevents.FakeRecorder, substrings ...string) {
	GinkgoHelper()
	got := emittedEvents(recorder)
	for _, want := range substrings {
		found := false
		for _, e := range got {
			if strings.Contains(e, want) {
				found = true
				break
			}
		}
		Expect(found).To(BeTrue(), "no Event contains %q; got %v", want, got)
	}
}

var auditCounter int

var _ = Describe("Audit trail", func() {
	const (
		auditAccount = "111111111111"
		auditRegion  = "eu-central-1"
		auditCreator = "arn:aws:sts::111111111111:assumed-role/payments-deploy/maria.k"
	)
	var (
		scopeName string
		sink      *bytes.Buffer
		recorder  *kevents.FakeRecorder
	)

	BeforeEach(func() {
		auditCounter++
		scopeName = fmt.Sprintf("audit-scope-%d", auditCounter)
		sink = &bytes.Buffer{}
		recorder = kevents.NewFakeRecorder(64)
	})

	Describe("an import", func() {
		var (
			importName string
			resourceID string
			writer     *fakeTagWriter
			reconciler *ResourceImportReconciler
		)

		reconcileImport := func() error {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: importName, Namespace: "default"}})
			return err
		}

		BeforeEach(func() {
			importName = fmt.Sprintf("audit-import-%d", auditCounter)
			resourceID = fmt.Sprintf("subnet-0a11d1%04d", auditCounter)
			writer = &fakeTagWriter{}
			reconciler = &ResourceImportReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(), Writer: writer, WritesEnabled: true,
				Recorder: recorder, Audit: audit.NewWriter(sink),
			}

			Expect(k8sClient.Create(ctx, &awsv1alpha1.NetworkScope{
				ObjectMeta: metav1.ObjectMeta{Name: scopeName},
				Spec: awsv1alpha1.NetworkScopeSpec{
					Accounts: []awsv1alpha1.AccountSpec{{ID: auditAccount}},
					Regions:  []string{auditRegion},
				},
			})).To(Succeed())
			Expect(k8sClient.Create(ctx, &awsv1alpha1.ResourceImport{
				ObjectMeta: metav1.ObjectMeta{Name: importName, Namespace: "default",
					Labels:      map[string]string{awsv1alpha1.LabelScope: scopeName},
					Annotations: map[string]string{annotationReason: "tags from creator rule"}},
				Spec: awsv1alpha1.ResourceImportSpec{
					ScopeRef: scopeName, Account: auditAccount, Region: auditRegion, ResourceID: resourceID,
					Tags:        map[string]string{"hs/managed": "true", "hs/owner": "team-payments"},
					RequestedBy: requestedBy(auditCreator),
				},
			})).To(Succeed())
		})

		AfterEach(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &awsv1alpha1.ResourceImport{
				ObjectMeta: metav1.ObjectMeta{Name: importName, Namespace: "default"}}))).To(Succeed())
			Expect(k8sClient.DeleteAllOf(ctx, &awsv1alpha1.Subnet{},
				client.MatchingLabels{awsv1alpha1.LabelScope: scopeName})).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &awsv1alpha1.NetworkScope{
				ObjectMeta: metav1.ObjectMeta{Name: scopeName}}))).To(Succeed())
		})

		It("leaves an Event on the object and an audit line with the tags before and after", func() {
			// The resource is already in the inventory with tags of its own, so the line can say
			// what changed rather than only what was applied.
			subnet := &awsv1alpha1.Subnet{
				ObjectMeta: metav1.ObjectMeta{Name: resourceID,
					Labels: map[string]string{awsv1alpha1.LabelScope: scopeName}},
				Spec: awsv1alpha1.SubnetSpec{SubnetID: resourceID, Account: auditAccount, Region: auditRegion},
			}
			Expect(k8sClient.Create(ctx, subnet)).To(Succeed())
			subnet.Status = awsv1alpha1.SubnetStatus{Tags: map[string]string{"Name": "legacy"}}
			Expect(k8sClient.Status().Update(ctx, subnet)).To(Succeed())

			Expect(reconcileImport()).To(Succeed())

			haveEvent(recorder, "Normal "+EventImported, resourceID, awsv1alpha1.RequestedByPolicy, "maria.k")

			lines := auditLines(sink)
			Expect(lines).To(HaveLen(1))
			rec := lines[0]
			Expect(rec.Action).To(Equal(audit.ActionImport))
			Expect(rec.Result).To(Equal(audit.ResultApplied))
			Expect(rec.Time).NotTo(BeZero())
			Expect(rec.Scope).To(Equal(scopeName))
			Expect(rec.Account).To(Equal(auditAccount))
			Expect(rec.Region).To(Equal(auditRegion))
			Expect(rec.ResourceID).To(Equal(resourceID))
			Expect(rec.Object).To(Equal("ResourceImport/default/" + importName))
			Expect(rec.Principal).To(ContainSubstring("maria.k"), "the creator is the principal, not the operator")
			Expect(rec.Reason).To(Equal("tags from creator rule"))
			Expect(rec.TagsBefore).To(Equal(map[string]string{"Name": "legacy"}))
			Expect(rec.TagsAfter).To(Equal(map[string]string{
				"Name": "legacy", "hs/managed": "true", "hs/owner": "team-payments"}))
			Expect(rec.Error).To(BeEmpty())
		})

		It("records a dry run as a dry run, with nothing applied", func() {
			imp := &awsv1alpha1.ResourceImport{}
			Expect(k8sClient.Get(ctx, types.NamespacedName{Name: importName, Namespace: "default"}, imp)).To(Succeed())
			imp.Spec.DryRun = true
			Expect(k8sClient.Update(ctx, imp)).To(Succeed())

			Expect(reconcileImport()).To(Succeed())

			haveEvent(recorder, "Normal "+EventImportDryRun, resourceID)
			lines := auditLines(sink)
			Expect(lines).To(HaveLen(1))
			Expect(lines[0].Result).To(Equal(audit.ResultDryRun))
			Expect(writer.calls).To(BeEmpty())
		})

		It("records a refusal from the cloud with the error and no tags after", func() {
			writer.failErr = errors.New("UnauthorizedOperation: ec2:CreateTags")

			Expect(reconcileImport()).To(Succeed())

			haveEvent(recorder, "Warning TagsNotApplied", "UnauthorizedOperation")
			lines := auditLines(sink)
			Expect(lines).To(HaveLen(1))
			Expect(lines[0].Result).To(Equal(audit.ResultFailed))
			Expect(lines[0].Error).To(ContainSubstring("UnauthorizedOperation"))
			Expect(lines[0].TagsAfter).To(BeEmpty(), "nothing was applied, so nothing may be claimed")
		})
	})

	Describe("an auto-import policy decision", func() {
		var (
			discoverer *fakeDiscoverer
			creators   *CreatorCache
			reconciler *NetworkScopeReconciler
		)

		snapshot := func() *inventory.Snapshot {
			return &inventory.Snapshot{
				UnmanagedVPCs: []inventory.VPC{{ID: "vpc-0a11d17ed", Account: auditAccount, Region: auditRegion,
					CIDRBlocks: []string{"10.90.0.0/16"}, Tags: map[string]string{"Name": "legacy"}}},
				UnmanagedSubnets: []inventory.Subnet{
					{ID: "subnet-0a11d17ed", VPCID: "vpc-0a11d17ed", Account: auditAccount, Region: auditRegion,
						CIDRBlock: "10.90.1.0/24"},
				},
			}
		}
		reconcileScope := func() {
			GinkgoHelper()
			_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: scopeName}})
			Expect(err).NotTo(HaveOccurred())
		}
		createScope := func(p *awsv1alpha1.AutoImportPolicy) {
			GinkgoHelper()
			Expect(k8sClient.Create(ctx, &awsv1alpha1.NetworkScope{
				ObjectMeta: metav1.ObjectMeta{Name: scopeName},
				Spec: awsv1alpha1.NetworkScopeSpec{
					Accounts:       []awsv1alpha1.AccountSpec{{ID: auditAccount}},
					Regions:        []string{auditRegion},
					VPCTagSelector: map[string]string{"hs/managed": "true"},
					AutoImport:     p,
				},
			})).To(Succeed())
		}

		BeforeEach(func() {
			creators = &CreatorCache{}
			discoverer = &fakeDiscoverer{
				snapshots: map[string]*inventory.Snapshot{auditAccount + "/" + auditRegion: snapshot()},
				errs:      map[string]error{},
			}
			reconciler = &NetworkScopeReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(), Discoverer: discoverer, Creators: creators,
				Recorder: recorder, Audit: audit.NewWriter(sink),
			}
		})

		AfterEach(func() {
			list := &awsv1alpha1.ResourceImportList{}
			Expect(k8sClient.List(ctx, list, client.InNamespace("default"),
				client.MatchingLabels{awsv1alpha1.LabelScope: scopeName})).To(Succeed())
			for i := range list.Items {
				Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &list.Items[i]))).To(Succeed())
			}
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &awsv1alpha1.NetworkScope{
				ObjectMeta: metav1.ObjectMeta{Name: scopeName}}))).To(Succeed())
		})

		It("records the resource nobody could be found for as no_owner", func() {
			createScope(&awsv1alpha1.AutoImportPolicy{
				Mode: awsv1alpha1.AutoImportApply,
				FromCreator: []awsv1alpha1.CreatorRule{
					{PrincipalPrefix: "arn:aws:sts::111111111111:assumed-role/payments-",
						Tags: map[string]string{"hs/owner": "team-payments"}},
				},
			})
			// Only the VPC has a known creator; the subnet inside it has none and no rule can
			// inherit anything, because the VPC is unmanaged too.
			creators.Record(context.Background(), []events.Creation{{
				ResourceID: "vpc-0a11d17ed", Principal: auditCreator, EventName: "CreateVpc"}})

			reconcileScope()

			haveEvent(recorder,
				"Normal "+EventAutoImportRequested+" Requested the import of vpc-0a11d17ed",
				"Warning "+EventNoOwner+" No rule could attribute subnet-0a11d17ed")

			decisions := auditLines(sink)
			imported := linesFor(decisions, "vpc-0a11d17ed")
			Expect(imported).To(HaveLen(1))
			Expect(imported[0].Action).To(Equal(audit.ActionPolicyDecision))
			Expect(imported[0].Result).To(Equal(audit.ResultApplied))
			Expect(imported[0].Object).To(Equal("NetworkScope/" + scopeName))
			Expect(imported[0].Principal).To(ContainSubstring("maria.k"))
			Expect(imported[0].TagsBefore).To(Equal(map[string]string{"Name": "legacy"}))
			Expect(imported[0].TagsAfter).To(HaveKeyWithValue("hs/owner", "team-payments"))

			orphan := linesFor(decisions, "subnet-0a11d17ed")
			Expect(orphan).To(HaveLen(1))
			Expect(orphan[0].Result).To(Equal(audit.ResultNoOwner))
			Expect(orphan[0].Reason).To(ContainSubstring("no rule resolved"))
			Expect(orphan[0].Principal).To(Equal(awsv1alpha1.RequestedByPolicy),
				"nobody could be named, so the policy owns the decision")
			Expect(orphan[0].TagsAfter).To(BeEmpty())
		})

		It("reports an account it could not read as an Event on the scope", func() {
			createScope(nil)
			discoverer.errs[auditAccount+"/"+auditRegion] = errors.New("AccessDenied: sts:AssumeRole")

			reconcileScope()

			haveEvent(recorder, "Warning "+EventTargetUnreachable, auditAccount, "AccessDenied")
		})
	})

	Describe("a claim", func() {
		var (
			claimName  string
			claimVPC   string
			writer     *fakeWriter
			reconciler *SubnetClaimReconciler
		)

		BeforeEach(func() {
			claimName = fmt.Sprintf("audit-claim-%d", auditCounter)
			claimVPC = fmt.Sprintf("vpc-0a11d2%04d", auditCounter)
			writer = &fakeWriter{conflictOnce: map[string]bool{}}
			reconciler = &SubnetClaimReconciler{
				Client: k8sClient, Scheme: k8sClient.Scheme(), Writer: writer, WritesEnabled: true,
				Recorder: recorder, Audit: audit.NewWriter(sink),
			}

			Expect(k8sClient.Create(ctx, &awsv1alpha1.NetworkScope{
				ObjectMeta: metav1.ObjectMeta{Name: scopeName},
				Spec: awsv1alpha1.NetworkScopeSpec{
					Accounts: []awsv1alpha1.AccountSpec{{ID: auditAccount}},
					Regions:  []string{auditRegion},
				},
			})).To(Succeed())
			vpc := &awsv1alpha1.VPC{
				ObjectMeta: metav1.ObjectMeta{Name: claimVPC,
					Labels: map[string]string{awsv1alpha1.LabelScope: scopeName, awsv1alpha1.LabelVPC: claimVPC}},
				Spec: awsv1alpha1.VPCSpec{VPCID: claimVPC, Account: auditAccount, Region: auditRegion},
			}
			Expect(k8sClient.Create(ctx, vpc)).To(Succeed())
			vpc.Status.CIDRBlocks = []string{"10.60.0.0/16"}
			Expect(k8sClient.Status().Update(ctx, vpc)).To(Succeed())

			Expect(k8sClient.Create(ctx, &awsv1alpha1.SubnetClaim{
				ObjectMeta: metav1.ObjectMeta{Name: claimName, Namespace: "default"},
				Spec: awsv1alpha1.SubnetClaimSpec{
					ScopeRef: scopeName, Account: auditAccount, Region: auditRegion, VPCID: claimVPC,
					PrefixLength: 24, AvailabilityZones: []string{auditRegion + "a"},
					Mode: awsv1alpha1.ClaimModeCreate, Owner: "team-payments", Env: "prod",
				},
			})).To(Succeed())
		})

		AfterEach(func() {
			Expect(k8sClient.DeleteAllOf(ctx, &awsv1alpha1.SubnetClaim{}, client.InNamespace("default"))).To(Succeed())
			Expect(k8sClient.DeleteAllOf(ctx, &awsv1alpha1.VPC{},
				client.MatchingLabels{awsv1alpha1.LabelScope: scopeName})).To(Succeed())
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, &awsv1alpha1.NetworkScope{
				ObjectMeta: metav1.ObjectMeta{Name: scopeName}}))).To(Succeed())
		})

		It("records the reserved CIDR and the created subnet, attributed to the claim's owner", func() {
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{Name: claimName, Namespace: "default"}})
			Expect(err).NotTo(HaveOccurred())

			haveEvent(recorder, "Normal "+EventAllocated+" Reserved 10.60.0.0/24",
				"Normal "+EventSubnetCreated+" Created subnet-new1")

			lines := auditLines(sink)
			Expect(lines).To(HaveLen(2))
			for _, rec := range lines {
				Expect(rec.Action).To(Equal(audit.ActionAllocate))
				Expect(rec.Scope).To(Equal(scopeName))
				Expect(rec.Object).To(Equal("SubnetClaim/default/" + claimName))
				Expect(rec.Principal).To(Equal("team-payments"))
				Expect(rec.CIDR).To(Equal("10.60.0.0/24"))
			}
			Expect(lines[0].Result).To(Equal(audit.ResultReserved))
			Expect(lines[0].ResourceID).To(BeEmpty(), "nothing exists in AWS yet")
			Expect(lines[1].Result).To(Equal(audit.ResultApplied))
			Expect(lines[1].ResourceID).To(Equal("subnet-new1"))
			Expect(lines[1].TagsAfter).To(HaveKeyWithValue("hs/owner", "team-payments"))
		})
	})
})
