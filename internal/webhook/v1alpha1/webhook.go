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

// Package v1alpha1 holds the admission webhooks for the deprecated aws.hypersurgery API group,
// served for one release (0.8) while the operator migrates its objects to
// network.hypersurgery.dev. It is removed in 0.9 with the group.
//
// It keeps three promises of the old group, and adds one:
//
//   - The created-by annotation is written from the authenticated user on CREATE and cannot be
//     changed afterwards. The migration copies it into the new group, so it has to stay as
//     trustworthy as it was.
//   - An old object is checked the way its new-group copy will be: it is converted with the
//     migration's own mapping and run through the new group's validator. A scope that is not
//     migrated yet is read from the old group, so apply order does not matter.
//   - The old defaults (tag keys, name prefix, mode, the import's labels) are still filled in.
//   - New: a migrated object's spec can no longer change. Only its copy is reconciled, so a
//     change here would be silently ignored; the refusal names the object to edit instead.
package v1alpha1

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
	"hypersurgery.dev/subnet-operator/internal/migration"
	webhookv1beta1 "hypersurgery.dev/subnet-operator/internal/webhook/v1beta1"
)

// SetupWebhooksWithManager registers the aws.hypersurgery webhooks. The paths are the ones the
// old group always had, so the webhook configurations of earlier releases keep working.
func SetupWebhooksWithManager(mgr ctrl.Manager, writesEnabled bool) error {
	reader := &legacyScopes{Reader: mgr.GetClient()}
	if err := ctrl.NewWebhookManagedBy(mgr, &awsv1alpha1.NetworkScope{}).
		WithDefaulter(&networkScopeDefaulter{}).
		WithValidator(&networkScopeValidator{next: &webhookv1beta1.NetworkScopeValidator{Client: reader}}).
		Complete(); err != nil {
		return err
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &awsv1alpha1.SubnetClaim{}).
		WithDefaulter(&subnetClaimDefaulter{}).
		WithValidator(&subnetClaimValidator{next: &webhookv1beta1.SubnetClaimValidator{
			Client: reader, WritesEnabled: writesEnabled}}).
		Complete(); err != nil {
		return err
	}
	return ctrl.NewWebhookManagedBy(mgr, &awsv1alpha1.ResourceImport{}).
		WithDefaulter(&resourceImportDefaulter{}).
		WithValidator(&resourceImportValidator{next: &webhookv1beta1.ResourceImportValidator{
			Client: reader, WritesEnabled: writesEnabled}}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-aws-hypersurgery-v1alpha1-networkscope,mutating=true,failurePolicy=ignore,sideEffects=None,groups=aws.hypersurgery,resources=networkscopes,verbs=create;update,versions=v1alpha1,name=mnetworkscope-v1alpha1.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-aws-hypersurgery-v1alpha1-networkscope,mutating=false,failurePolicy=fail,sideEffects=None,groups=aws.hypersurgery,resources=networkscopes,verbs=create;update,versions=v1alpha1,name=vnetworkscope-v1alpha1.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/mutate-aws-hypersurgery-v1alpha1-subnetclaim,mutating=true,failurePolicy=ignore,sideEffects=None,groups=aws.hypersurgery,resources=subnetclaims,verbs=create;update,versions=v1alpha1,name=msubnetclaim-v1alpha1.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-aws-hypersurgery-v1alpha1-subnetclaim,mutating=false,failurePolicy=fail,sideEffects=None,groups=aws.hypersurgery,resources=subnetclaims,verbs=create;update,versions=v1alpha1,name=vsubnetclaim-v1alpha1.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/mutate-aws-hypersurgery-v1alpha1-resourceimport,mutating=true,failurePolicy=ignore,sideEffects=None,groups=aws.hypersurgery,resources=resourceimports,verbs=create;update,versions=v1alpha1,name=mresourceimport-v1alpha1.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-aws-hypersurgery-v1alpha1-resourceimport,mutating=false,failurePolicy=fail,sideEffects=None,groups=aws.hypersurgery,resources=resourceimports,verbs=create;update,versions=v1alpha1,name=vresourceimport-v1alpha1.kb.io,admissionReviewVersions=v1

// defaultResyncInterval is the old CRD's default, for a scope that set the field to null.
var defaultResyncInterval = metav1.Duration{Duration: 10 * time.Minute}

type networkScopeDefaulter struct{}

// Default fills in the tag keys and the resync interval, as the old group always did.
func (d *networkScopeDefaulter) Default(_ context.Context, obj *awsv1alpha1.NetworkScope) error {
	keys := &obj.Spec.TagKeys
	if keys.Owner == "" {
		keys.Owner = awsv1alpha1.DefaultOwnerTagKey
	}
	if keys.Env == "" {
		keys.Env = awsv1alpha1.DefaultEnvTagKey
	}
	if keys.Tier == "" {
		keys.Tier = awsv1alpha1.DefaultTierTagKey
	}
	if obj.Spec.ResyncInterval == nil {
		interval := defaultResyncInterval
		obj.Spec.ResyncInterval = &interval
	}
	return nil
}

type subnetClaimDefaulter struct{}

// Default records the creator and fills in the name prefix and the mode.
func (d *subnetClaimDefaulter) Default(ctx context.Context, obj *awsv1alpha1.SubnetClaim) error {
	stampCreatedBy(ctx, obj)
	if obj.Spec.NamePrefix == "" && obj.Name != "" {
		obj.Spec.NamePrefix = obj.Name
	}
	if obj.Spec.Mode == "" {
		obj.Spec.Mode = awsv1alpha1.ClaimModeCreate
	}
	return nil
}

type resourceImportDefaulter struct{}

// Default records the creator, labels the import with what it refers to, and fills in who
// requested it.
func (d *resourceImportDefaulter) Default(ctx context.Context, obj *awsv1alpha1.ResourceImport) error {
	stampCreatedBy(ctx, obj)
	if obj.Spec.ResourceID != "" {
		if obj.Labels == nil {
			obj.Labels = map[string]string{}
		}
		for key, value := range map[string]string{
			awsv1alpha1.LabelResource: obj.Spec.ResourceID,
			awsv1alpha1.LabelScope:    obj.Spec.ScopeRef,
			awsv1alpha1.LabelAccount:  obj.Spec.Account,
			awsv1alpha1.LabelRegion:   obj.Spec.Region,
		} {
			if _, ok := obj.Labels[key]; !ok && value != "" {
				obj.Labels[key] = value
			}
		}
	}
	if obj.Spec.RequestedBy == "" {
		if req, err := admission.RequestFromContext(ctx); err == nil && req.UserInfo.Username != "" {
			obj.Spec.RequestedBy = req.UserInfo.Username
		}
	}
	return nil
}

type networkScopeValidator struct {
	next *webhookv1beta1.NetworkScopeValidator
}

func (v *networkScopeValidator) ValidateCreate(ctx context.Context, obj *awsv1alpha1.NetworkScope) (
	admission.Warnings, error) {
	scope, _ := migration.NetworkScope(obj, migration.ForCluster)
	w, err := v.next.Validate(ctx, scope)
	return w, inOldGroup(err)
}

func (v *networkScopeValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *awsv1alpha1.NetworkScope) (
	admission.Warnings, error) {
	if migrated, err := refuseSpecChange("NetworkScope", oldObj, newObj, oldObj.Spec, newObj.Spec); migrated {
		return nil, err
	}
	scope, _ := migration.NetworkScope(newObj, migration.ForCluster)
	w, err := v.next.Validate(ctx, scope)
	return w, inOldGroup(err)
}

func (v *networkScopeValidator) ValidateDelete(context.Context, *awsv1alpha1.NetworkScope) (admission.Warnings, error) {
	return nil, nil
}

type subnetClaimValidator struct {
	next *webhookv1beta1.SubnetClaimValidator
}

func (v *subnetClaimValidator) ValidateCreate(ctx context.Context, obj *awsv1alpha1.SubnetClaim) (
	admission.Warnings, error) {
	if e := validateCreatedByOnCreate(ctx, obj); e != nil {
		return nil, invalid("SubnetClaim", obj.Name, e)
	}
	claim, _ := migration.SubnetClaim(obj, migration.ForCluster)
	w, err := v.next.Validate(ctx, claim)
	return w, inOldGroup(err)
}

func (v *subnetClaimValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *awsv1alpha1.SubnetClaim) (
	admission.Warnings, error) {
	if e := validateCreatedByOnUpdate(oldObj, newObj); e != nil {
		return nil, invalid("SubnetClaim", newObj.Name, e)
	}
	if migrated, err := refuseSpecChange("SubnetClaim", oldObj, newObj, oldObj.Spec, newObj.Spec); migrated {
		return nil, err
	}
	oldClaim, _ := migration.SubnetClaim(oldObj, migration.ForCluster)
	newClaim, _ := migration.SubnetClaim(newObj, migration.ForCluster)
	w, err := v.next.ValidateUpdate(ctx, oldClaim, newClaim)
	return w, inOldGroup(err)
}

func (v *subnetClaimValidator) ValidateDelete(context.Context, *awsv1alpha1.SubnetClaim) (admission.Warnings, error) {
	return nil, nil
}

type resourceImportValidator struct {
	next *webhookv1beta1.ResourceImportValidator
}

func (v *resourceImportValidator) ValidateCreate(ctx context.Context, obj *awsv1alpha1.ResourceImport) (
	admission.Warnings, error) {
	if e := validateCreatedByOnCreate(ctx, obj); e != nil {
		return nil, invalid("ResourceImport", obj.Name, e)
	}
	imp, _ := migration.ResourceImport(obj, migration.ForCluster)
	w, err := v.next.Validate(ctx, imp)
	return w, inOldGroup(err)
}

func (v *resourceImportValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *awsv1alpha1.ResourceImport) (
	admission.Warnings, error) {
	if e := validateCreatedByOnUpdate(oldObj, newObj); e != nil {
		return nil, invalid("ResourceImport", newObj.Name, e)
	}
	if migrated, err := refuseSpecChange("ResourceImport", oldObj, newObj, oldObj.Spec, newObj.Spec); migrated {
		return nil, err
	}
	oldImp, _ := migration.ResourceImport(oldObj, migration.ForCluster)
	newImp, _ := migration.ResourceImport(newObj, migration.ForCluster)
	w, err := v.next.ValidateUpdate(ctx, oldImp, newImp)
	return w, inOldGroup(err)
}

func (v *resourceImportValidator) ValidateDelete(context.Context, *awsv1alpha1.ResourceImport) (admission.Warnings, error) {
	return nil, nil
}

// refuseSpecChange handles an update of a migrated object. It reports migrated as true when
// the object is migrated, and then an error if the spec changed. Anything else — labels,
// annotations, a re-apply of the same manifest — is let through unchecked: the object is only
// a record now, and checking it against a newer inventory could refuse an edit that changes
// nothing.
func refuseSpecChange(kind string, oldObj, newObj client.Object, oldSpec, newSpec any) (migrated bool, err error) {
	to, ok := oldObj.GetAnnotations()[networkv1beta1.AnnotationMigratedTo]
	if !ok {
		return false, nil
	}
	if equality.Semantic.DeepEqual(oldSpec, newSpec) {
		return true, nil
	}
	name := to
	if ns := newObj.GetNamespace(); ns != "" {
		name = ns + "/" + to
	}
	return true, invalid(kind, newObj.GetName(), field.Forbidden(field.NewPath("spec"), fmt.Sprintf(
		"this object was migrated to network.hypersurgery.dev/v1beta1 %s %s, which is the one the operator "+
			"reconciles; change that one (manager migrate-manifests converts a manifest)", kind, name)))
}

// validateCreatedByOnCreate refuses a new object whose created-by annotation is not the user
// the API server authenticated; see the new group's webhook for why.
func validateCreatedByOnCreate(ctx context.Context, obj client.Object) *field.Error {
	req, err := admission.RequestFromContext(ctx)
	if err != nil || req.UserInfo.Username == "" {
		return nil
	}
	if obj.GetAnnotations()[awsv1alpha1.AnnotationCreatedBy] == req.UserInfo.Username {
		return nil
	}
	return field.Forbidden(createdByPath(),
		fmt.Sprintf("must name the user that creates the object (%s); it is set by the operator's mutating webhook, "+
			"which did not run or was overridden", req.UserInfo.Username))
}

// validateCreatedByOnUpdate refuses any change to the created-by annotation.
func validateCreatedByOnUpdate(oldObj, newObj client.Object) *field.Error {
	oldValue, hadIt := oldObj.GetAnnotations()[awsv1alpha1.AnnotationCreatedBy]
	newValue, hasIt := newObj.GetAnnotations()[awsv1alpha1.AnnotationCreatedBy]
	if oldValue == newValue && hadIt == hasIt {
		return nil
	}
	was := "not set"
	if hadIt {
		was = fmt.Sprintf("%q", oldValue)
	}
	return field.Forbidden(createdByPath(),
		fmt.Sprintf("records who created the object and cannot be changed (was %s)", was))
}

func createdByPath() *field.Path {
	return field.NewPath("metadata").Child("annotations").Key(awsv1alpha1.AnnotationCreatedBy)
}

// stampCreatedBy writes the authenticated user into the created-by annotation on CREATE.
func stampCreatedBy(ctx context.Context, obj client.Object) {
	req, err := admission.RequestFromContext(ctx)
	if err != nil || req.Operation != admissionv1.Create || req.UserInfo.Username == "" {
		return
	}
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[awsv1alpha1.AnnotationCreatedBy] = req.UserInfo.Username
	obj.SetAnnotations(annotations)
}

func invalid(kind, name string, e *field.Error) error {
	return apierrors.NewInvalid(schema.GroupKind{Group: awsv1alpha1.GroupVersion.Group, Kind: kind}, name, field.ErrorList{e})
}

// inOldGroup rewrites the group of an Invalid error from the new group's validator, so kubectl
// names the kind the person applied.
func inOldGroup(err error) error {
	var status *apierrors.StatusError
	if errors.As(err, &status) && status.ErrStatus.Details != nil &&
		status.ErrStatus.Details.Group == networkv1beta1.GroupVersion.Group {
		status.ErrStatus.Details.Group = awsv1alpha1.GroupVersion.Group
		status.ErrStatus.Message = replaceGroup(status.ErrStatus.Message)
	}
	return err
}

func replaceGroup(msg string) string {
	return strings.Replace(msg, "."+networkv1beta1.GroupVersion.Group, "."+awsv1alpha1.GroupVersion.Group, 1)
}
