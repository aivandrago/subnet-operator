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
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kevents "k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/audit"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/metrics"
	"hypersurgery.dev/subnet-operator/internal/provider"
	"hypersurgery.dev/subnet-operator/internal/tenancy"
)

const importRetryInterval = time.Minute

// ResourceImportReconciler takes existing resources under management by tagging them. It only
// ever adds tags, and only with writes enabled: an operator installed to take inventory must
// not start changing the account because somebody applied an object.
type ResourceImportReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader reads the scope straight from the API server when the cache may lag.
	APIReader client.Reader
	// Providers are the clouds the operator runs with; the import's scope picks one, which
	// checks the import and writes the tags where it keeps ownership.
	Providers *provider.Registry
	// WritesEnabled gates every ownership write.
	WritesEnabled bool
	// Notify asks the scope controller to resync the target, so the freshly tagged resource
	// shows up in the inventory within seconds rather than at the next full sync.
	Notify func(ctx context.Context, changed []inventory.TargetKey) error
	// Recorder puts the outcome on the object, where `kubectl describe` shows it.
	Recorder kevents.EventRecorder
	// Audit receives one line per import. Nil keeps the audit trail off.
	Audit audit.Sink
	// WebhooksEnabled says the manager serves the admission webhooks, which write the
	// created-by annotation and refuse changes to it. Only then does the audit trail repeat
	// the annotation; otherwise anybody could have written it, and the line says "unknown".
	WebhooksEnabled bool
}

// +kubebuilder:rbac:groups=network.hypersurgery.dev,resources=resourceimports,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=network.hypersurgery.dev,resources=resourceimports/status,verbs=get;update;patch

// Reconcile applies one import, or explains in the status why it did not.
func (r *ResourceImportReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	imp := &networkv1.ResourceImport{}
	if err := r.Get(ctx, req.NamespacedName, imp); err != nil {
		if apierrors.IsNotFound(err) {
			metrics.ForgetImport(req.Namespace, req.Name)
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !imp.DeletionTimestamp.IsZero() {
		metrics.ForgetImport(req.Namespace, req.Name)
		// Tags stay on the resource: deleting the object is not a request to untag anything.
		return ctrl.Result{}, nil
	}

	status, requeue, err := r.reconcile(ctx, imp)
	if err != nil {
		log.Error(err, "import failed", "resource", imp.Spec.ResourceID)
	}
	if statusErr := r.writeStatus(ctx, imp, status); statusErr != nil {
		return ctrl.Result{}, statusErr
	}
	// From the status just written, so the metric never says something the object does not.
	metrics.ImportReady(req.Namespace, req.Name, scopeProvider(ctx, r, imp.Spec.ScopeRef), status.State, status.Conditions)
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *ResourceImportReconciler) reconcile(ctx context.Context, imp *networkv1.ResourceImport) (
	networkv1.ResourceImportStatus, time.Duration, error) {
	status := *imp.Status.DeepCopy()
	status.ObservedGeneration = imp.Generation

	fail := func(state, reason, msg string) (networkv1.ResourceImportStatus, time.Duration, error) {
		status.State = state
		setImportCondition(&status, ConditionReady, metav1.ConditionFalse, reason, msg, imp.Generation)
		eventf(r.Recorder, imp, corev1.EventTypeWarning, reason, ActionApplyTags, "%s", msg)
		return status, importRetryInterval, nil
	}

	// Already done: CreateTags is idempotent, but calling AWS on every resync would be noise
	// in somebody's CloudTrail for no reason.
	if status.State == networkv1.ImportApplied && maps.Equal(status.AppliedTags, imp.Spec.Tags) &&
		meta.IsStatusConditionTrue(status.Conditions, ConditionReady) {
		return status, 0, nil
	}

	scope := &networkv1.NetworkScope{}
	if err := r.Get(ctx, types.NamespacedName{Name: imp.Spec.ScopeRef}, scope); err != nil {
		if apierrors.IsNotFound(err) {
			return fail(networkv1.ImportFailed, "ScopeNotFound",
				fmt.Sprintf("NetworkScope %q not found", imp.Spec.ScopeRef))
		}
		return status, 0, err
	}
	// Before anything that uses the scope, dry runs included: a dry run is how somebody finds
	// out what the write role would do, which is the scope's business too. An import that has
	// already applied its tags returned above and stays Applied — nothing is taken back.
	if refusal, err := namespaceRefusal(ctx, r.Client, scope, imp.Namespace); err != nil {
		return status, 0, err
	} else if refusal != "" {
		return fail(networkv1.ImportFailed, tenancy.ReasonNamespaceNotAllowed, refusal)
	}
	p, ok := r.Providers.Get(scope.Spec.Provider)
	if !ok {
		return fail(networkv1.ImportFailed, ReasonProviderNotEnabled, r.Providers.NotEnabled(scope.Spec.Provider))
	}
	// The webhook refuses the same imports at apply time.
	if reason, msg := p.ImportRefusal(imp); reason != "" {
		return fail(networkv1.ImportFailed, reason, msg)
	}
	target, ok := writeTarget(scope, p, imp.Spec.Account, imp.Spec.Region)
	if !ok {
		return fail(networkv1.ImportFailed, "AccountNotInScope",
			fmt.Sprintf("account %s in %s is not covered by NetworkScope %q",
				imp.Spec.Account, imp.Spec.Region, scope.Name))
	}

	if imp.Spec.DryRun {
		status.State = networkv1.ImportSkipped
		status.Error = ""
		setImportCondition(&status, ConditionReady, metav1.ConditionTrue, "DryRun",
			fmt.Sprintf("dry run: %s would get %s", imp.Spec.ResourceID, formatTags(imp.Spec.Tags)), imp.Generation)
		eventf(r.Recorder, imp, corev1.EventTypeNormal, EventImportDryRun, ActionApplyTags,
			"Dry run: %s would get %s", imp.Spec.ResourceID, formatTags(imp.Spec.Tags))
		r.record(ctx, imp, audit.ResultDryRun, "")
		return status, 0, nil
	}
	if !r.WritesEnabled {
		return fail(networkv1.ImportPending, "WritesDisabled",
			"the operator runs read-only; start it with --enable-writes to apply tags")
	}
	if provider.MissingWriteIdentity(p, scope, imp.Spec.Account) {
		return fail(networkv1.ImportPending, "NoWriteRole",
			fmt.Sprintf("account %s has no %s in NetworkScope %q; one with %s alone is enough",
				imp.Spec.Account, p.WriteIdentityField(), scope.Name, p.OwnershipPermission()))
	}

	if err := p.WriteOwnership(ctx, target, imp.Spec.ResourceID, imp.Spec.Tags); err != nil {
		status.Error = err.Error()
		r.record(ctx, imp, audit.ResultFailed, err.Error())
		return fail(networkv1.ImportFailed, "TagsNotApplied", err.Error())
	}

	now := metav1.Now()
	status.State = networkv1.ImportApplied
	status.AppliedTags = maps.Clone(imp.Spec.Tags)
	status.AppliedTime = &now
	status.Error = ""
	setImportCondition(&status, ConditionReady, metav1.ConditionTrue, "Applied",
		fmt.Sprintf("%s now carries %s", imp.Spec.ResourceID, formatTags(imp.Spec.Tags)), imp.Generation)
	eventf(r.Recorder, imp, corev1.EventTypeNormal, EventImported, ActionApplyTags,
		"Imported %s with %s, requested by %s", imp.Spec.ResourceID, formatTags(imp.Spec.Tags), imp.Spec.RequestedBy)
	r.record(ctx, imp, audit.ResultApplied, "")

	if r.Notify != nil {
		if err := r.Notify(ctx, []inventory.TargetKey{target.Key()}); err != nil {
			logf.FromContext(ctx).Error(err, "failed to request a resync after tagging")
		}
	}
	return status, 0, nil
}

// record writes the audit line for one import. The principal is the import's requestedBy,
// which is what the auto-import policy filled with the CloudTrail creator, so a line written
// here and a line written by the policy name the same person. created_by is the Kubernetes
// user that created the import, which is what to believe when the two disagree.
func (r *ResourceImportReconciler) record(ctx context.Context, imp *networkv1.ResourceImport,
	result, failure string) {
	if r.Audit == nil {
		return
	}
	before := r.knownTags(ctx, imp.Spec.ResourceID)
	var after map[string]string
	if result != audit.ResultFailed {
		after = maps.Clone(before)
		if after == nil {
			after = map[string]string{}
		}
		maps.Copy(after, imp.Spec.Tags)
	}
	audit.Emit(ctx, r.Audit, audit.Record{
		Action:     audit.ActionImport,
		Result:     result,
		Scope:      imp.Spec.ScopeRef,
		Account:    imp.Spec.Account,
		Region:     imp.Spec.Region,
		ResourceID: imp.Spec.ResourceID,
		Object:     objectRef("ResourceImport", imp),
		// requestedBy says who a change is attributed to: a person or ticket for a
		// hand-written import, the policy and the CloudTrail creator for a generated one. It is
		// free text, so the authenticated creator goes next to it rather than in its place.
		Principal:  imp.Spec.RequestedBy,
		CreatedBy:  createdBy(imp, r.WebhooksEnabled),
		Reason:     imp.Annotations[annotationReason],
		TagsBefore: before,
		TagsAfter:  after,
		Error:      failure,
	})
}

// knownTags returns the tags the operator last saw on the resource, from the inventory it
// mirrors. A resource nobody has tagged is not mirrored, so nil means "not known to us",
// which is what the audit line should say rather than claiming the resource had no tags.
func (r *ResourceImportReconciler) knownTags(ctx context.Context, resourceID string) map[string]string {
	key := types.NamespacedName{Name: resourceID}
	subnet := &networkv1.Subnet{}
	if err := r.reader().Get(ctx, key, subnet); err == nil {
		return subnet.Status.Tags
	}
	network := &networkv1.Network{}
	if err := r.reader().Get(ctx, key, network); err == nil {
		return network.Status.Tags
	}
	return nil
}

func (r *ResourceImportReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

// formatTags renders the tags in a stable order, for a status message somebody has to read.
func formatTags(tags map[string]string) string {
	keys := slices.Sorted(maps.Keys(tags))
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+tags[k])
	}
	return strings.Join(parts, ", ")
}

func setImportCondition(status *networkv1.ResourceImportStatus, typ string, st metav1.ConditionStatus,
	reason, msg string, gen int64) {
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: typ, Status: st, Reason: reason, Message: msg, ObservedGeneration: gen,
	})
}

func (r *ResourceImportReconciler) writeStatus(ctx context.Context, imp *networkv1.ResourceImport,
	status networkv1.ResourceImportStatus) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &networkv1.ResourceImport{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(imp), latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		latest.Status = status
		return r.Status().Update(ctx, latest)
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *ResourceImportReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkv1.ResourceImport{}).
		Named("resourceimport").
		Complete(r)
}
