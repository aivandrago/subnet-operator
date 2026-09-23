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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
	"hypersurgery.dev/subnet-operator/internal/sheets"
)

const (
	defaultRefreshInterval = 5 * time.Minute
	minRefreshInterval     = time.Minute
	defaultCredentialsKey  = "credentials.json"
)

// SheetExportReconciler mirrors the subnets of a NetworkScope into a Google Sheet.
type SheetExportReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader reads Secrets without caching every Secret in the cluster.
	APIReader client.Reader
	// NewSyncer builds a sheet writer from Google service account JSON; replaced in tests.
	NewSyncer func(ctx context.Context, credentialsJSON []byte) (sheets.Syncer, error)
}

// +kubebuilder:rbac:groups=aws.hypersurgery,resources=sheetexports,verbs=get;list;watch
// +kubebuilder:rbac:groups=aws.hypersurgery,resources=sheetexports/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile writes the current inventory into the sheet and schedules the next refresh.
func (r *SheetExportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	export := &awsv1alpha1.SheetExport{}
	if err := r.Get(ctx, req.NamespacedName, export); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !export.DeletionTimestamp.IsZero() {
		// The sheet is left as it is: it belongs to the people reading it.
		return ctrl.Result{}, nil
	}

	rows, err := r.export(ctx, export)
	if err != nil {
		if statusErr := r.updateStatus(ctx, export, 0, err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, err
	}
	if err := r.updateStatus(ctx, export, rows, nil); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: refreshInterval(export)}, nil
}

func (r *SheetExportReconciler) export(ctx context.Context, export *awsv1alpha1.SheetExport) (int, error) {
	scope := &awsv1alpha1.NetworkScope{}
	if err := r.Get(ctx, types.NamespacedName{Name: export.Spec.ScopeRef}, scope); err != nil {
		if apierrors.IsNotFound(err) {
			return 0, fmt.Errorf("NetworkScope %q not found", export.Spec.ScopeRef)
		}
		return 0, err
	}

	credentials, err := r.credentials(ctx, export.Spec.CredentialsSecretRef)
	if err != nil {
		return 0, err
	}

	subnets := &awsv1alpha1.SubnetList{}
	if err := r.List(ctx, subnets, client.MatchingLabels{awsv1alpha1.LabelScope: scope.Name}); err != nil {
		return 0, err
	}
	vpcs := &awsv1alpha1.VPCList{}
	if err := r.List(ctx, vpcs, client.MatchingLabels{awsv1alpha1.LabelScope: scope.Name}); err != nil {
		return 0, err
	}
	vpcNames := make(map[string]string, len(vpcs.Items))
	for _, v := range vpcs.Items {
		vpcNames[v.Spec.VPCID] = v.Status.Name
	}
	syncedAt := make(map[string]*metav1.Time, len(scope.Status.Targets))
	for _, t := range scope.Status.Targets {
		syncedAt[t.Account+"/"+t.Region] = t.LastSyncTime
	}

	table := sheets.BuildTable(subnets.Items, vpcNames, syncedAt, export.Spec.ExtraTagColumns)
	syncer, err := r.NewSyncer(ctx, credentials)
	if err != nil {
		return 0, err
	}
	if err := syncer.Sync(ctx, export.Spec.SpreadsheetID, sheetName(export), table); err != nil {
		return 0, err
	}
	return len(table.Rows), nil
}

func (r *SheetExportReconciler) credentials(ctx context.Context, ref awsv1alpha1.SecretKeyRef) ([]byte, error) {
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: ref.Namespace}, secret); err != nil {
		return nil, fmt.Errorf("read credentials secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	key := ref.Key
	if key == "" {
		key = defaultCredentialsKey
	}
	credentials, ok := secret.Data[key]
	if !ok || len(credentials) == 0 {
		return nil, fmt.Errorf("secret %s/%s has no key %q", ref.Namespace, ref.Name, key)
	}
	return credentials, nil
}

func sheetName(export *awsv1alpha1.SheetExport) string {
	if export.Spec.SheetName == "" {
		return "Subnets"
	}
	return export.Spec.SheetName
}

func refreshInterval(export *awsv1alpha1.SheetExport) time.Duration {
	if export.Spec.RefreshInterval == nil {
		return defaultRefreshInterval
	}
	return max(export.Spec.RefreshInterval.Duration, minRefreshInterval)
}

func (r *SheetExportReconciler) updateStatus(ctx context.Context, export *awsv1alpha1.SheetExport,
	rows int, exportErr error) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &awsv1alpha1.SheetExport{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(export), latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		cond := metav1.Condition{Type: ConditionReady, Status: metav1.ConditionTrue, Reason: "Exported",
			Message: fmt.Sprintf("%d subnets written to the sheet", rows), ObservedGeneration: latest.Generation}
		if exportErr != nil {
			cond.Status, cond.Reason, cond.Message = metav1.ConditionFalse, "ExportFailed", exportErr.Error()
		} else {
			now := metav1.Now()
			latest.Status.LastExportTime = &now
			latest.Status.Rows = count32(rows)
			latest.Status.ObservedGeneration = latest.Generation
		}
		latest.Status.URL = sheets.URL(latest.Spec.SpreadsheetID)
		meta.SetStatusCondition(&latest.Status.Conditions, cond)
		return r.Status().Update(ctx, latest)
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *SheetExportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.NewSyncer == nil {
		r.NewSyncer = func(ctx context.Context, credentialsJSON []byte) (sheets.Syncer, error) {
			return sheets.New(ctx, credentialsJSON)
		}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&awsv1alpha1.SheetExport{}).
		Named("sheetexport").
		Complete(r)
}
