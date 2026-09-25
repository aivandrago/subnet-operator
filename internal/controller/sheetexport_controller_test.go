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
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
	"hypersurgery.dev/subnet-operator/internal/sheets"
)

// fakeSyncer records the last table written instead of calling Google.
type fakeSyncer struct {
	mu            sync.Mutex
	spreadsheetID string
	sheetName     string
	table         sheets.Table
	err           error
}

func (f *fakeSyncer) Sync(_ context.Context, spreadsheetID, sheetName string, t sheets.Table) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spreadsheetID, f.sheetName, f.table = spreadsheetID, sheetName, t
	return f.err
}

var exportCounter int

var _ = Describe("SheetExport Controller", func() {
	const (
		exportAccount = "111111111111"
		exportRegion  = "eu-central-1"
	)
	var (
		exportName string
		scopeName  string
		secretName string
		syncer     *fakeSyncer
		reconciler *SheetExportReconciler
		credJSON   []byte
	)

	reconcileExport := func() (reconcile.Result, error) {
		return reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: exportName}})
	}

	getExport := func() *networkv1beta1.SheetExport {
		GinkgoHelper()
		e := &networkv1beta1.SheetExport{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: exportName}, e)).To(Succeed())
		return e
	}

	BeforeEach(func() {
		exportCounter++
		exportName = fmt.Sprintf("export-%d", exportCounter)
		scopeName = fmt.Sprintf("export-scope-%d", exportCounter)
		secretName = fmt.Sprintf("google-%d", exportCounter)
		syncer = &fakeSyncer{}
		// The test only checks that these bytes reach the syncer unchanged; the object
		// carries no key material, which is the whole point of using it here.
		// nosemgrep
		credJSON = []byte(`{"type":"service_account"}`)
		reconciler = &SheetExportReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
			NewSyncer: func(_ context.Context, credentials []byte) (sheets.Syncer, error) {
				Expect(string(credentials)).To(Equal(string(credJSON)))
				return syncer, nil
			}}

		scope := &networkv1beta1.NetworkScope{
			ObjectMeta: metav1.ObjectMeta{Name: scopeName},
			Spec: networkv1beta1.NetworkScopeSpec{
				Provider:          networkv1beta1.ProviderAWS,
				NamespaceSelector: &metav1.LabelSelector{},
				Accounts:          []networkv1beta1.Account{{ID: exportAccount}},
				Regions:           []string{exportRegion},
			},
		}
		Expect(k8sClient.Create(ctx, scope)).To(Succeed())
		synced := metav1.NewTime(time.Date(2026, 9, 22, 8, 5, 0, 0, time.UTC))
		scope.Status.Targets = []networkv1beta1.TargetStatus{{Account: exportAccount, Region: exportRegion, LastSyncTime: &synced}}
		Expect(k8sClient.Status().Update(ctx, scope)).To(Succeed())

		labels := map[string]string{
			networkv1beta1.LabelScope: scopeName, networkv1beta1.LabelAccount: exportAccount,
			networkv1beta1.LabelRegion: exportRegion, networkv1beta1.LabelNetwork: "vpc-e1",
		}
		vpc := &networkv1beta1.Network{
			ObjectMeta: metav1.ObjectMeta{Name: "vpc-e1-" + exportName, Labels: labels},
			Spec:       networkv1beta1.NetworkSpec{Provider: networkv1beta1.ProviderAWS, ID: "vpc-e1", Account: exportAccount, Region: exportRegion},
		}
		Expect(k8sClient.Create(ctx, vpc)).To(Succeed())
		vpc.Status.Name = "prod"
		Expect(k8sClient.Status().Update(ctx, vpc)).To(Succeed())

		for i, cidr := range []string{"10.0.2.0/24", "10.0.1.0/24"} {
			sn := &networkv1beta1.Subnet{
				ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("subnet-e%d-%s", i, exportName), Labels: labels},
				Spec: networkv1beta1.SubnetSpec{Provider: networkv1beta1.ProviderAWS, ID: fmt.Sprintf("subnet-e%d", i), NetworkID: "vpc-e1",
					Account: exportAccount, Region: exportRegion},
			}
			Expect(k8sClient.Create(ctx, sn)).To(Succeed())
			sn.Status = networkv1beta1.SubnetStatus{CIDRBlock: cidr, TotalIPs: ptr.To[int64](251), AvailableIPs: ptr.To[int64](51),
				UtilizationPercent: ptr.To[int32](79), Owner: "team-a", Tags: map[string]string{"cost-center": "cc-42"}}
			Expect(k8sClient.Status().Update(ctx, sn)).To(Succeed())
		}

		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "default"},
			Data:       map[string][]byte{"credentials.json": credJSON},
		})).To(Succeed())

		Expect(k8sClient.Create(ctx, &networkv1beta1.SheetExport{
			ObjectMeta: metav1.ObjectMeta{Name: exportName},
			Spec: networkv1beta1.SheetExportSpec{
				ScopeRef: scopeName, SpreadsheetID: "sheet-1", SheetName: "Subnets",
				CredentialsSecretRef: networkv1beta1.SecretKeyRef{Name: secretName, Namespace: "default", Key: "credentials.json"},
				ExtraTagColumns:      []string{"cost-center"},
				RefreshInterval:      &metav1.Duration{Duration: 5 * time.Minute},
			},
		})).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.DeleteAllOf(ctx, &networkv1beta1.Subnet{}, client.MatchingLabels{networkv1beta1.LabelScope: scopeName})).To(Succeed())
		Expect(k8sClient.DeleteAllOf(ctx, &networkv1beta1.Network{}, client.MatchingLabels{networkv1beta1.LabelScope: scopeName})).To(Succeed())
		Expect(k8sClient.Delete(ctx, &networkv1beta1.SheetExport{ObjectMeta: metav1.ObjectMeta{Name: exportName}})).To(Succeed())
		Expect(k8sClient.Delete(ctx, &networkv1beta1.NetworkScope{ObjectMeta: metav1.ObjectMeta{Name: scopeName}})).To(Succeed())
		Expect(k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: "default"}})).To(Succeed())
	})

	It("writes the scope's subnets into the sheet", func() {
		res, err := reconcileExport()
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(Equal(5 * time.Minute))

		Expect(syncer.spreadsheetID).To(Equal("sheet-1"))
		Expect(syncer.sheetName).To(Equal("Subnets"))
		Expect(syncer.table.Header).To(ContainElement("cost-center"))
		Expect(syncer.table.Rows).To(HaveLen(2))
		Expect(syncer.table.Rows[0][7]).To(Equal("10.0.1.0/24"), "rows are sorted by CIDR")
		Expect(syncer.table.Rows[0][4]).To(Equal("prod"), "the network name is resolved")
		Expect(syncer.table.Rows[0][18]).To(Equal("2026-09-22 08:05"), "the target's last sync time")

		export := getExport()
		Expect(export.Status.Rows).To(Equal(int32(2)))
		Expect(export.Status.URL).To(Equal("https://docs.google.com/spreadsheets/d/sheet-1/edit"))
		Expect(export.Status.LastExportTime).NotTo(BeNil())
		Expect(meta.IsStatusConditionTrue(export.Status.Conditions, ConditionReady)).To(BeTrue())
	})

	It("reports a failing export without losing the previous one", func() {
		_, err := reconcileExport()
		Expect(err).NotTo(HaveOccurred())
		firstExport := getExport().Status.LastExportTime

		syncer.err = errors.New("googleapi: Error 403: The caller does not have permission")
		_, err = reconcileExport()
		Expect(err).To(HaveOccurred())

		export := getExport()
		cond := meta.FindStatusCondition(export.Status.Conditions, ConditionReady)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Message).To(ContainSubstring("403"))
		Expect(export.Status.Rows).To(Equal(int32(2)), "the previous row count is kept")
		Expect(export.Status.LastExportTime.Equal(firstExport)).To(BeTrue())
	})

	It("reports a missing credentials key", func() {
		secret := &corev1.Secret{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: secretName, Namespace: "default"}, secret)).To(Succeed())
		secret.Data = map[string][]byte{"other.json": credJSON}
		Expect(k8sClient.Update(ctx, secret)).To(Succeed())

		_, err := reconcileExport()
		Expect(err).To(HaveOccurred())
		cond := meta.FindStatusCondition(getExport().Status.Conditions, ConditionReady)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Message).To(ContainSubstring("credentials.json"))
	})

	It("reports a missing NetworkScope", func() {
		export := getExport()
		export.Spec.ScopeRef = "does-not-exist"
		Expect(k8sClient.Update(ctx, export)).To(Succeed())

		_, err := reconcileExport()
		Expect(err).To(HaveOccurred())
		cond := meta.FindStatusCondition(getExport().Status.Conditions, ConditionReady)
		Expect(cond.Message).To(ContainSubstring("does-not-exist"))
	})
})
