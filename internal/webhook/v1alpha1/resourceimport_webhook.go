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

package v1alpha1

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
)

var resourceimportlog = logf.Log.WithName("resourceimport-resource")

// SetupResourceImportWebhookWithManager registers the webhook for ResourceImport in the
// manager. writesEnabled mirrors the manager's --enable-writes switch.
func SetupResourceImportWebhookWithManager(mgr ctrl.Manager, writesEnabled bool) error {
	return ctrl.NewWebhookManagedBy(mgr, &awsv1alpha1.ResourceImport{}).
		WithValidator(&ResourceImportValidator{Client: mgr.GetClient(), WritesEnabled: writesEnabled}).
		WithDefaulter(&ResourceImportDefaulter{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-aws-hypersurgery-v1alpha1-resourceimport,mutating=true,failurePolicy=ignore,sideEffects=None,groups=aws.hypersurgery,resources=resourceimports,verbs=create;update,versions=v1alpha1,name=mresourceimport-v1alpha1.kb.io,admissionReviewVersions=v1

// ResourceImportDefaulter records who asked for an import and labels it like the auto-import
// policy does.
type ResourceImportDefaulter struct{}

// Default labels the import with the resource it refers to, records who created it and fills
// in requestedBy.
//
// The label is what the auto-import policy lists to find out whether a resource already has
// an import. Without it, an import somebody wrote by hand is invisible to the policy, which
// then creates a second one for the same resource.
//
// The created-by annotation is the authenticated user, always, and is what the audit trail
// trusts. requestedBy stays free text — a ticket, a team, somebody the import is done for —
// and defaults to the same user, because "who asked for this" is the question a reviewer asks
// of an import months later, and the person applying it rarely thinks to write their own name.
func (d *ResourceImportDefaulter) Default(ctx context.Context, obj *awsv1alpha1.ResourceImport) error {
	stampCreatedBy(ctx, obj)
	if obj.Spec.ResourceID != "" {
		if obj.Labels == nil {
			obj.Labels = map[string]string{}
		}
		setIfEmpty(obj.Labels, awsv1alpha1.LabelResource, obj.Spec.ResourceID)
		setIfEmpty(obj.Labels, awsv1alpha1.LabelScope, obj.Spec.ScopeRef)
		setIfEmpty(obj.Labels, awsv1alpha1.LabelAccount, obj.Spec.Account)
		setIfEmpty(obj.Labels, awsv1alpha1.LabelRegion, obj.Spec.Region)
	}
	if obj.Spec.RequestedBy == "" {
		if req, err := admission.RequestFromContext(ctx); err == nil && req.UserInfo.Username != "" {
			obj.Spec.RequestedBy = req.UserInfo.Username
		}
	}
	return nil
}

// setIfEmpty fills a label without overwriting one somebody set on purpose. A label value
// that is not a valid label value is left out rather than making the object unwritable.
func setIfEmpty(labels map[string]string, key, value string) {
	if value == "" {
		return
	}
	if _, ok := labels[key]; !ok {
		labels[key] = value
	}
}

// +kubebuilder:webhook:path=/validate-aws-hypersurgery-v1alpha1-resourceimport,mutating=false,failurePolicy=fail,sideEffects=None,groups=aws.hypersurgery,resources=resourceimports,verbs=create;update,versions=v1alpha1,name=vresourceimport-v1alpha1.kb.io,admissionReviewVersions=v1

// ResourceImportValidator refuses imports that would fail in AWS, or that point at a
// resource no scope can reach.
type ResourceImportValidator struct {
	// Client reads the scopes and the inventory.
	Client client.Reader
	// WritesEnabled is the manager's --enable-writes switch.
	WritesEnabled bool
}

// ValidateCreate checks a new import.
func (v *ResourceImportValidator) ValidateCreate(ctx context.Context, obj *awsv1alpha1.ResourceImport) (
	admission.Warnings, error) {
	resourceimportlog.V(1).Info("Validating ResourceImport on create", "name", obj.GetName())
	if e := validateCreatedByOnCreate(ctx, obj); e != nil {
		return nil, invalidError("ResourceImport", obj.Name, field.ErrorList{e})
	}
	return v.validate(ctx, obj)
}

// ValidateUpdate checks a changed import. What it points at is frozen: an import that moves
// to another resource has already tagged the first one, and tags are never taken back.
func (v *ResourceImportValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *awsv1alpha1.ResourceImport) (
	admission.Warnings, error) {
	resourceimportlog.V(1).Info("Validating ResourceImport on update", "name", newObj.GetName())

	spec := field.NewPath("spec")
	var errs field.ErrorList
	for _, e := range []*field.Error{
		immutableField(spec.Child("scopeRef"), oldObj.Spec.ScopeRef, newObj.Spec.ScopeRef),
		immutableField(spec.Child("account"), oldObj.Spec.Account, newObj.Spec.Account),
		immutableField(spec.Child("region"), oldObj.Spec.Region, newObj.Spec.Region),
		immutableField(spec.Child("resourceID"), oldObj.Spec.ResourceID, newObj.Spec.ResourceID),
		validateCreatedByOnUpdate(oldObj, newObj),
	} {
		if e != nil {
			errs = append(errs, e)
		}
	}
	if len(errs) > 0 {
		return nil, invalidError("ResourceImport", newObj.Name, errs)
	}

	warnings, err := v.validate(ctx, newObj)
	if oldObj.Status.State == awsv1alpha1.ImportApplied && !newObj.Spec.DryRun {
		for key := range oldObj.Status.AppliedTags {
			if _, still := newObj.Spec.Tags[key]; !still {
				warnings = append(warnings, fmt.Sprintf(
					"tag %q was already applied to %s and stays on it: the operator only ever adds tags",
					key, newObj.Spec.ResourceID))
			}
		}
	}
	return warnings, err
}

// ValidateDelete accepts every deletion: the tags stay on the resource.
func (v *ResourceImportValidator) ValidateDelete(_ context.Context, _ *awsv1alpha1.ResourceImport) (
	admission.Warnings, error) {
	return nil, nil
}

func (v *ResourceImportValidator) validate(ctx context.Context, imp *awsv1alpha1.ResourceImport) (
	admission.Warnings, error) {
	spec := field.NewPath("spec")
	var errs field.ErrorList
	var warnings admission.Warnings

	errs = append(errs, validateTags(spec.Child("tags"), imp.Spec.Tags)...)
	if e := validateRegion(spec.Child("region"), imp.Spec.Region); e != nil {
		errs = append(errs, e)
	}

	scope, e := scopeFor(ctx, v.Client, spec.Child("scopeRef"), imp.Spec.ScopeRef)
	if e != nil {
		return warnings, invalidError("ResourceImport", imp.Name, append(errs, e))
	}
	if e := validateNamespace(ctx, v.Client, scope, imp.Namespace); e != nil {
		return warnings, invalidError("ResourceImport", imp.Name, append(errs, e))
	}

	// The resource must belong to exactly one scope: the one that supplies the credentials.
	// More than one is ambiguous, and none means nothing can reach the resource.
	covering, err := scopesCovering(ctx, v.Client, imp.Spec.Account, imp.Spec.Region)
	switch {
	case err != nil:
		warnings = append(warnings, "the NetworkScopes could not be read, so the account/region pair was not checked: "+
			err.Error())
	case len(covering) == 0:
		errs = append(errs, field.Invalid(spec.Child("account"), imp.Spec.Account,
			fmt.Sprintf("no NetworkScope discovers account %s in %s, so nothing can reach %s",
				imp.Spec.Account, imp.Spec.Region, imp.Spec.ResourceID)))
	case len(covering) > 1:
		errs = append(errs, field.Invalid(spec.Child("account"), imp.Spec.Account,
			fmt.Sprintf("account %s in %s is discovered by more than one NetworkScope (%s); the import must be "+
				"unambiguous about which credentials tag the resource",
				imp.Spec.Account, imp.Spec.Region, strings.Join(covering, ", "))))
	case !scope.Covers(imp.Spec.Account, imp.Spec.Region):
		errs = append(errs, field.Invalid(spec.Child("scopeRef"), imp.Spec.ScopeRef,
			fmt.Sprintf("NetworkScope %q does not discover account %s in %s; %q does",
				scope.Name, imp.Spec.Account, imp.Spec.Region, covering[0])))
	}
	if len(errs) > 0 {
		return warnings, invalidError("ResourceImport", imp.Name, errs)
	}

	resourceWarnings, resourceErrs := v.validateAgainstInventory(ctx, imp, scope)
	warnings = append(warnings, resourceWarnings...)
	errs = append(errs, resourceErrs...)

	if !imp.Spec.DryRun && !v.WritesEnabled {
		warnings = append(warnings, writesDisabledWarning("the import waits instead of tagging "+imp.Spec.ResourceID)...)
	}

	return warnings, invalidError("ResourceImport", imp.Name, errs)
}

// validateAgainstInventory checks the import against the discovered resource: that it is
// where the import says it is, and that it ends up with the tags the scope's auto-import
// policy requires. A resource that is not in the inventory is a warning, not an error: it
// may be discovered on the next resync, or be one of the unmanaged resources that an import
// exists precisely to take over.
func (v *ResourceImportValidator) validateAgainstInventory(ctx context.Context, imp *awsv1alpha1.ResourceImport,
	scope *awsv1alpha1.NetworkScope) (admission.Warnings, field.ErrorList) {
	spec := field.NewPath("spec")

	account, region, tags, found, err := v.resource(ctx, imp.Spec.ResourceID)
	if err != nil {
		return admission.Warnings{"the resource could not be read from the inventory: " + err.Error()}, nil
	}

	var warnings admission.Warnings
	if !found {
		warnings = append(warnings, fmt.Sprintf(
			"%s is not in the inventory of NetworkScope %q; it is either unmanaged or not discovered yet",
			imp.Spec.ResourceID, scope.Name))
	} else if account != imp.Spec.Account || region != imp.Spec.Region {
		return warnings, field.ErrorList{field.Invalid(spec.Child("resourceID"), imp.Spec.ResourceID,
			fmt.Sprintf("%s is in %s/%s, not in %s/%s", imp.Spec.ResourceID, account, region,
				imp.Spec.Account, imp.Spec.Region))}
	}

	// CreateTags replaces the value of every key it names. That is sometimes the point — an
	// owner being corrected — but it is a change to a tag somebody set, not an addition, and
	// the person applying the import should see which values it replaces.
	for _, key := range slices.Sorted(maps.Keys(imp.Spec.Tags)) {
		if old := tags[key]; old != "" && old != imp.Spec.Tags[key] {
			warnings = append(warnings, fmt.Sprintf("%s already carries %s=%q; the import replaces it with %q",
				imp.Spec.ResourceID, key, old, imp.Spec.Tags[key]))
		}
	}

	// The auto-import policy refuses to import a resource whose required tags nobody could
	// resolve. An import written by hand is held to the same bar, so that the two paths
	// cannot disagree about what "managed" means.
	missing := missingRequiredTags(scope, imp.Spec.Tags, tags)
	switch {
	case len(missing) == 0:
	case found:
		return warnings, field.ErrorList{field.Required(spec.Child("tags"),
			fmt.Sprintf("NetworkScope %q requires %s on an imported resource, and %s has neither",
				scope.Name, strings.Join(missing, ", "), imp.Spec.ResourceID))}
	default:
		warnings = append(warnings, fmt.Sprintf(
			"NetworkScope %q requires %s on an imported resource; the import does not set them, so %s only counts as "+
				"managed if it already carries them", scope.Name, strings.Join(missing, ", "), imp.Spec.ResourceID))
	}
	return warnings, nil
}

// resource looks the VPC or subnet up in the inventory and reports where it is and what it
// already carries.
func (v *ResourceImportValidator) resource(ctx context.Context, id string) (
	account, region string, tags map[string]string, found bool, err error) {
	var obj client.Object
	switch {
	case strings.HasPrefix(id, "vpc-"):
		obj = &awsv1alpha1.VPC{}
	case strings.HasPrefix(id, "subnet-"):
		obj = &awsv1alpha1.Subnet{}
	default:
		return "", "", nil, false, nil // the CRD pattern already refuses anything else
	}
	if err := v.Client.Get(ctx, client.ObjectKey{Name: id}, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", nil, false, nil
		}
		return "", "", nil, false, err
	}
	switch o := obj.(type) {
	case *awsv1alpha1.VPC:
		return o.Spec.Account, o.Spec.Region, o.Status.Tags, true, nil
	case *awsv1alpha1.Subnet:
		return o.Spec.Account, o.Spec.Region, o.Status.Tags, true, nil
	}
	return "", "", nil, false, nil
}

// missingRequiredTags returns the tag keys the scope's auto-import policy requires that
// neither the import nor the resource itself supplies. It is the same rule the policy
// applies, including its default of requiring an owner.
func missingRequiredTags(scope *awsv1alpha1.NetworkScope, wanted, existing map[string]string) []string {
	policy := scope.Spec.AutoImport
	if policy == nil || policy.Mode == "" || policy.Mode == awsv1alpha1.AutoImportOff {
		return nil
	}
	required := policy.RequiredTags
	if len(required) == 0 {
		required = []string{awsv1alpha1.DefaultOwnerTagKey}
	}
	var missing []string
	for _, key := range required {
		if wanted[key] != "" || existing[key] != "" {
			continue
		}
		missing = append(missing, key)
	}
	return missing
}
