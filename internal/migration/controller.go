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
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kevents "k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
)

// Event reasons on old-group objects.
const (
	// EventMigrated is on an old object once its copy in the new group exists.
	EventMigrated = "Migrated"
	// EventMigrationFailed is on an old object whose copy could not be created; the message
	// says why, and the migration tries again with backoff.
	EventMigrationFailed = "MigrationFailed"
)

// scopeWaitInterval is how soon an object whose scope is not migrated yet is looked at again.
const scopeWaitInterval = 2 * time.Second

// +kubebuilder:rbac:groups=aws.hypersurgery,resources=networkscopes;subnetclaims;resourceimports;sheetexports,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=aws.hypersurgery,resources=vpcs;subnets,verbs=get;list;watch;delete;deletecollection
// +kubebuilder:rbac:groups=network.hypersurgery.dev,resources=networkscopes;subnetclaims;resourceimports;sheetexports,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=network.hypersurgery.dev,resources=networkscopes/status;subnetclaims/status;resourceimports/status;sheetexports/status,verbs=get;update;patch

// kind is what the migration needs to know about one old kind.
type kind struct {
	name string
	// old and fresh return empty objects of the old and the new kind.
	old, fresh func() client.Object
	// convert returns the new object, status included, for an old one.
	convert func(old client.Object) (client.Object, Notes)
	// copyStatus copies the converted status into obj, whose metadata and spec stay.
	copyStatus func(obj, converted client.Object)
	// hasStatus reports whether a new object already carries state worth keeping.
	hasStatus func(obj client.Object) bool
	// scopeRef is the scope an object of the kind refers to, "" for a scope.
	scopeRef func(old client.Object) string
}

var kinds = []kind{
	{
		name:  kindNetworkScope,
		old:   func() client.Object { return &awsv1alpha1.NetworkScope{} },
		fresh: func() client.Object { return &networkv1beta1.NetworkScope{} },
		convert: func(o client.Object) (client.Object, Notes) {
			return NetworkScope(o.(*awsv1alpha1.NetworkScope), ForCluster)
		},
		copyStatus: func(obj, c client.Object) {
			obj.(*networkv1beta1.NetworkScope).Status = c.(*networkv1beta1.NetworkScope).Status
		},
		hasStatus: func(obj client.Object) bool {
			s := obj.(*networkv1beta1.NetworkScope).Status
			return s.LastSyncTime != nil || len(s.Targets) > 0
		},
		scopeRef: func(client.Object) string { return "" },
	},
	{
		name:  kindSubnetClaim,
		old:   func() client.Object { return &awsv1alpha1.SubnetClaim{} },
		fresh: func() client.Object { return &networkv1beta1.SubnetClaim{} },
		convert: func(o client.Object) (client.Object, Notes) {
			return SubnetClaim(o.(*awsv1alpha1.SubnetClaim), ForCluster)
		},
		copyStatus: func(obj, c client.Object) {
			obj.(*networkv1beta1.SubnetClaim).Status = c.(*networkv1beta1.SubnetClaim).Status
		},
		hasStatus: func(obj client.Object) bool {
			return len(obj.(*networkv1beta1.SubnetClaim).Status.Allocations) > 0
		},
		scopeRef: func(o client.Object) string { return o.(*awsv1alpha1.SubnetClaim).Spec.ScopeRef },
	},
	{
		name:  kindResourceImport,
		old:   func() client.Object { return &awsv1alpha1.ResourceImport{} },
		fresh: func() client.Object { return &networkv1beta1.ResourceImport{} },
		convert: func(o client.Object) (client.Object, Notes) {
			return ResourceImport(o.(*awsv1alpha1.ResourceImport), ForCluster)
		},
		copyStatus: func(obj, c client.Object) {
			obj.(*networkv1beta1.ResourceImport).Status = c.(*networkv1beta1.ResourceImport).Status
		},
		hasStatus: func(obj client.Object) bool {
			return obj.(*networkv1beta1.ResourceImport).Status.State != ""
		},
		scopeRef: func(o client.Object) string { return o.(*awsv1alpha1.ResourceImport).Spec.ScopeRef },
	},
	{
		name:  kindSheetExport,
		old:   func() client.Object { return &awsv1alpha1.SheetExport{} },
		fresh: func() client.Object { return &networkv1beta1.SheetExport{} },
		convert: func(o client.Object) (client.Object, Notes) {
			return SheetExport(o.(*awsv1alpha1.SheetExport), ForCluster)
		},
		copyStatus: func(obj, c client.Object) {
			obj.(*networkv1beta1.SheetExport).Status = c.(*networkv1beta1.SheetExport).Status
		},
		hasStatus: func(obj client.Object) bool {
			return obj.(*networkv1beta1.SheetExport).Status.LastExportTime != nil
		},
		scopeRef: func(o client.Object) string { return o.(*awsv1alpha1.SheetExport).Spec.ScopeRef },
	},
}

// Reconciler migrates the objects of one old kind. For each old object without a
// network.hypersurgery.dev counterpart it creates one with the same name and namespace, copies
// the status through the status subresource (a claim's reservations, an import's history, a
// scope's known unmanaged resources), and then marks the old object migrated-to. From then on
// only the new object is reconciled: the old group's controllers are gone, and the old group's
// webhook refuses spec changes to a migrated object.
//
// A counterpart that already exists — applied from a repository converted with
// migrate-manifests before the operator got to it — is kept as it is; only when it has no state
// of its own yet does it get the old object's status.
type Reconciler struct {
	// Client reads old objects through the cache and writes both groups.
	Client client.Client
	// APIReader reads new objects past the cache, so a copy created a moment ago is not
	// created again.
	APIReader client.Reader
	// Recorder puts the outcome on the old object, where `kubectl describe` shows it.
	Recorder kevents.EventRecorder

	kind kind
}

// Reconcile migrates one old object.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	old := r.kind.old()
	if err := r.Client.Get(ctx, req.NamespacedName, old); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !old.GetDeletionTimestamp().IsZero() {
		return ctrl.Result{}, nil
	}
	if _, done := old.GetAnnotations()[networkv1beta1.AnnotationMigratedTo]; done {
		// Migrated before. A counterpart deleted since was deleted on purpose.
		return ctrl.Result{}, r.cleanUp(ctx, old)
	}

	// Scopes first: the copy of a claim, an import or an export works without its scope, but
	// only reports ScopeNotFound until the scope is there.
	if ref := r.kind.scopeRef(old); ref != "" {
		waiting, err := r.scopeWaiting(ctx, ref)
		if err != nil {
			return ctrl.Result{}, err
		}
		if waiting {
			return ctrl.Result{RequeueAfter: scopeWaitInterval}, nil
		}
	}

	converted, notes := r.kind.convert(old)
	existing := r.kind.fresh()
	err := r.APIReader.Get(ctx, req.NamespacedName, existing)
	switch {
	case apierrors.IsNotFound(err):
		obj := converted.DeepCopyObject().(client.Object)
		if err := r.Client.Create(ctx, obj); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return ctrl.Result{RequeueAfter: scopeWaitInterval}, nil
			}
			r.eventf(old, corev1.EventTypeWarning, EventMigrationFailed,
				"Could not create network.hypersurgery.dev/v1beta1 %s %s: %v", r.kind.name, describe(old), err)
			return ctrl.Result{}, err
		}
		r.kind.copyStatus(obj, converted)
		if err := r.Client.Status().Update(ctx, obj); err != nil {
			// The copy exists without its status; the next attempt finds it empty and fills it.
			return ctrl.Result{}, fmt.Errorf("copy the status of %s %s: %w", r.kind.name, describe(old), err)
		}
	case err != nil:
		return ctrl.Result{}, err
	case !r.kind.hasStatus(existing) && r.kind.hasStatus(converted):
		r.kind.copyStatus(existing, converted)
		if err := r.Client.Status().Update(ctx, existing); err != nil {
			return ctrl.Result{}, fmt.Errorf("copy the status of %s %s: %w", r.kind.name, describe(old), err)
		}
		notes = append(notes, "a network.hypersurgery.dev object of that name already existed; it kept its spec "+
			"and got this object's status")
	default:
		notes = append(notes, "a network.hypersurgery.dev object of that name already existed with a state of its "+
			"own; it was left as it is")
	}

	patch := client.MergeFrom(old.DeepCopyObject().(client.Object))
	annotations := old.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[networkv1beta1.AnnotationMigratedTo] = old.GetName()
	old.SetAnnotations(annotations)
	if err := r.Client.Patch(ctx, old, patch); err != nil {
		return ctrl.Result{}, err
	}
	msg := fmt.Sprintf("Migrated to network.hypersurgery.dev/v1beta1 %s %s; change that one from now on",
		r.kind.name, describe(old))
	if len(notes) > 0 {
		msg += ". " + strings.Join(notes, ". ")
	}
	r.eventf(old, corev1.EventTypeNormal, EventMigrated, "%s", msg)
	log.Info("Migrated to network.hypersurgery.dev/v1beta1", "kind", r.kind.name, "object", describe(old),
		"notes", notes)
	return ctrl.Result{}, r.cleanUp(ctx, old)
}

// scopeWaiting reports whether the named scope still waits for its own migration: the old
// one exists, unmigrated, and the new one does not.
func (r *Reconciler) scopeWaiting(ctx context.Context, name string) (bool, error) {
	scope := &networkv1beta1.NetworkScope{}
	err := r.APIReader.Get(ctx, client.ObjectKey{Name: name}, scope)
	if err == nil || !apierrors.IsNotFound(err) {
		return false, err
	}
	old := &awsv1alpha1.NetworkScope{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: name}, old); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	_, done := old.GetAnnotations()[networkv1beta1.AnnotationMigratedTo]
	return !done, nil
}

// cleanUp removes what a migrated scope leaves behind: its old VPC and Subnet objects. They
// are a cache nobody updates any more, and left in place they would report free addresses
// and owners that are no longer true.
func (r *Reconciler) cleanUp(ctx context.Context, old client.Object) error {
	if _, ok := old.(*awsv1alpha1.NetworkScope); !ok {
		return nil
	}
	sel := client.MatchingLabels{awsv1alpha1.LabelScope: old.GetName()}
	if err := r.Client.DeleteAllOf(ctx, &awsv1alpha1.Subnet{}, sel); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err := r.Client.DeleteAllOf(ctx, &awsv1alpha1.VPC{}, sel); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *Reconciler) eventf(obj client.Object, eventType, reason, note string, args ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(obj, nil, eventType, reason, "Migrate", note, args...)
}

func describe(obj client.Object) string {
	if obj.GetNamespace() == "" {
		return obj.GetName()
	}
	return obj.GetNamespace() + "/" + obj.GetName()
}

// SetupWithManager registers one migration controller per old kind.
func SetupWithManager(mgr ctrl.Manager, recorder kevents.EventRecorder) error {
	for _, k := range kinds {
		r := &Reconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Recorder: recorder, kind: k}
		if err := ctrl.NewControllerManagedBy(mgr).
			For(k.old()).
			Named("migrate-" + strings.ToLower(k.name)).
			Complete(r); err != nil {
			return err
		}
	}
	return nil
}

// Served reports whether the API server serves aws.hypersurgery/v1alpha1, which is what the
// migration needs to run at all: after the old CRDs are deleted there is nothing to migrate,
// and a controller watching a kind that does not exist would keep the manager from starting.
func Served(mapper meta.RESTMapper) (bool, error) {
	_, err := mapper.RESTMapping(schema.GroupKind{Group: awsv1alpha1.GroupVersion.Group, Kind: "NetworkScope"},
		awsv1alpha1.GroupVersion.Version)
	switch {
	case err == nil:
		return true, nil
	case meta.IsNoMatchError(err):
		return false, nil
	default:
		return false, err
	}
}
