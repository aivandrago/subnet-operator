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
	"errors"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
	"hypersurgery.dev/subnet-operator/internal/tenancy"
)

var networkscopelog = logf.Log.WithName("networkscope-resource")

// defaultResyncInterval matches the CRD default; the webhook only fills it in when the field
// was set to null explicitly, which the schema default does not cover.
var defaultResyncInterval = metav1.Duration{Duration: 10 * time.Minute}

// SetupNetworkScopeWebhookWithManager registers the webhook for NetworkScope in the manager.
func SetupNetworkScopeWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &awsv1alpha1.NetworkScope{}).
		WithValidator(&NetworkScopeValidator{Client: mgr.GetClient()}).
		WithDefaulter(&NetworkScopeDefaulter{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-aws-hypersurgery-v1alpha1-networkscope,mutating=true,failurePolicy=ignore,sideEffects=None,groups=aws.hypersurgery,resources=networkscopes,verbs=create;update,versions=v1alpha1,name=mnetworkscope-v1alpha1.kb.io,admissionReviewVersions=v1

// NetworkScopeDefaulter fills in the tag keys and the resync interval.
type NetworkScopeDefaulter struct{}

// Default fills the fields the schema default cannot: an empty string is a value, so
// `tagKeys: {owner: ""}` keeps its empty string and would make the operator look for a tag
// with no name. Here it becomes the default key again.
func (d *NetworkScopeDefaulter) Default(_ context.Context, obj *awsv1alpha1.NetworkScope) error {
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

// +kubebuilder:webhook:path=/validate-aws-hypersurgery-v1alpha1-networkscope,mutating=false,failurePolicy=fail,sideEffects=None,groups=aws.hypersurgery,resources=networkscopes,verbs=create;update,versions=v1alpha1,name=vnetworkscope-v1alpha1.kb.io,admissionReviewVersions=v1

// NetworkScopeValidator checks a scope against the shape of AWS and against the other
// scopes: two scopes discovering the same account and region would mirror the same VPCs and
// subnets into the same objects and overwrite each other forever.
type NetworkScopeValidator struct {
	// Client lists the other scopes.
	Client client.Reader
}

// ValidateCreate checks a new scope.
func (v *NetworkScopeValidator) ValidateCreate(ctx context.Context, obj *awsv1alpha1.NetworkScope) (
	admission.Warnings, error) {
	networkscopelog.V(1).Info("Validating NetworkScope on create", "name", obj.GetName())
	return v.validate(ctx, obj)
}

// ValidateUpdate checks a changed scope. Nothing in a scope is immutable: it owns no cloud
// resource, and removing an account is how discovery is stopped.
func (v *NetworkScopeValidator) ValidateUpdate(ctx context.Context, _, newObj *awsv1alpha1.NetworkScope) (
	admission.Warnings, error) {
	networkscopelog.V(1).Info("Validating NetworkScope on update", "name", newObj.GetName())
	return v.validate(ctx, newObj)
}

// ValidateDelete accepts every deletion: nothing in AWS is touched by it.
func (v *NetworkScopeValidator) ValidateDelete(_ context.Context, _ *awsv1alpha1.NetworkScope) (
	admission.Warnings, error) {
	return nil, nil
}

func (v *NetworkScopeValidator) validate(ctx context.Context, scope *awsv1alpha1.NetworkScope) (
	admission.Warnings, error) {
	spec := field.NewPath("spec")
	var errs field.ErrorList
	var warnings admission.Warnings

	for i, region := range scope.Spec.Regions {
		if e := validateRegion(spec.Child("regions").Index(i), region); e != nil {
			errs = append(errs, e)
		}
	}

	// The operator has one identity of its own. Every further account is reached through
	// sts:AssumeRole, so exactly one account may go without a role.
	var ownAccounts []string
	for i, account := range scope.Spec.Accounts {
		path := spec.Child("accounts").Index(i)
		errs = append(errs, validateAccount(path, account)...)
		if account.RoleARN == "" {
			ownAccounts = append(ownAccounts, account.ID)
		}
		if account.ExternalID != "" && account.RoleARN == "" {
			warnings = append(warnings, fmt.Sprintf(
				"account %s sets externalID but no roleARN, so the external ID is never used", account.ID))
		}
	}
	if len(ownAccounts) > 1 {
		errs = append(errs, field.Invalid(spec.Child("accounts"), ownAccounts,
			"only one account may omit roleARN (the account the operator itself runs in); "+
				"the others need a role to assume"))
	}

	overlapErrs, err := v.validateNoOverlap(ctx, scope)
	if err != nil {
		// The other scopes could not be listed. The account/region pair is not claimed by
		// anything we can see, and the controllers report a conflict in the status anyway.
		warnings = append(warnings, "the other NetworkScopes could not be read, so the account/region pairs were not "+
			"checked for overlap: "+err.Error())
	}
	errs = append(errs, overlapErrs...)

	if policy := scope.Spec.AutoImport; policy != nil && policy.Mode == awsv1alpha1.AutoImportApply {
		warnings = append(warnings, "auto-import is in Apply mode: unmanaged resources are tagged in AWS "+
			"as soon as the policy resolves an owner")
	}

	nsWarnings, nsErrs := v.validateNamespaces(ctx, scope)
	warnings = append(warnings, nsWarnings...)
	errs = append(errs, nsErrs...)

	return warnings, invalidError("NetworkScope", scope.Name, errs)
}

// validateNamespaces checks the namespace selector, and that the namespace the auto-import
// policy writes its imports to is one the scope allows: otherwise the operator itself would be
// the one using the scope from a namespace nobody granted it to. A namespace that does not
// exist yet is a warning — it may be applied after the scope, with the right labels.
func (v *NetworkScopeValidator) validateNamespaces(ctx context.Context, scope *awsv1alpha1.NetworkScope) (
	admission.Warnings, field.ErrorList) {
	selectorPath := field.NewPath("spec").Child("namespaceSelector")
	if scope.Spec.NamespaceSelector == nil {
		return admission.Warnings{tenancy.UnrestrictedWarning(scope)}, nil
	}
	if _, err := metav1.LabelSelectorAsSelector(scope.Spec.NamespaceSelector); err != nil {
		return nil, field.ErrorList{field.Invalid(selectorPath, scope.Spec.NamespaceSelector, err.Error())}
	}

	policy := scope.Spec.AutoImport
	if policy == nil || policy.Mode == "" || policy.Mode == awsv1alpha1.AutoImportOff {
		return nil, nil
	}
	namespace := policy.ImportNamespace()
	allowed, err := tenancy.Allowed(ctx, v.Client, scope, namespace)
	switch {
	case errors.Is(err, tenancy.ErrNamespaceNotFound):
		return admission.Warnings{fmt.Sprintf(
			"namespace %q, where the auto-import policy writes its imports, does not exist yet; the policy creates "+
				"nothing until it exists and spec.namespaceSelector selects it", namespace)}, nil
	case err != nil:
		return admission.Warnings{"the auto-import namespace could not be checked against spec.namespaceSelector: " +
			err.Error()}, nil
	case !allowed:
		return nil, field.ErrorList{field.Invalid(field.NewPath("spec").Child("autoImport").Child("namespace"),
			namespace, "spec.namespaceSelector does not select this namespace, so the imports the policy writes "+
				"there would be refused; select it, or point the policy at a namespace the selector allows")}
	}
	return nil, nil
}

// validateAccount checks one account entry: its regions, and that its roles live in the
// account they are meant to reach. sts:AssumeRole can only assume a role of that account, so
// a role ARN from a different one is always a copy-paste mistake.
func validateAccount(path *field.Path, account awsv1alpha1.AccountSpec) field.ErrorList {
	var errs field.ErrorList
	for i, region := range account.Regions {
		if e := validateRegion(path.Child("regions").Index(i), region); e != nil {
			errs = append(errs, e)
		}
	}
	roles := []struct{ name, arn string }{
		{"roleARN", account.RoleARN},
		{"writeRoleARN", account.WriteRoleARN},
	}
	for _, role := range roles {
		if role.arn == "" {
			continue
		}
		match := arnAccountPattern.FindStringSubmatch(role.arn)
		if match == nil {
			continue // the CRD pattern already refuses anything that is not a role ARN
		}
		if match[1] != account.ID {
			errs = append(errs, field.Invalid(path.Child(role.name), role.arn,
				fmt.Sprintf("the role lives in account %s, but this entry is account %s: sts:AssumeRole can only "+
					"assume a role of the account it reaches", match[1], account.ID)))
		}
	}
	return errs
}

// validateNoOverlap refuses an account/region pair another scope already discovers. Two
// scopes over the same pair produce the same VPC and Subnet objects from two reconcilers,
// and the inventory then flips between them.
func (v *NetworkScopeValidator) validateNoOverlap(ctx context.Context, scope *awsv1alpha1.NetworkScope) (
	field.ErrorList, error) {
	others := &awsv1alpha1.NetworkScopeList{}
	if err := v.Client.List(ctx, others); err != nil {
		return nil, err
	}

	accounts := field.NewPath("spec").Child("accounts")
	var errs field.ErrorList
	for i, account := range scope.Spec.Accounts {
		for _, region := range scope.RegionsFor(account.ID) {
			for j := range others.Items {
				other := &others.Items[j]
				if other.Name == scope.Name || !other.Covers(account.ID, region) {
					continue
				}
				errs = append(errs, field.Duplicate(accounts.Index(i).Child("id"),
					fmt.Sprintf("account %s in %s is already discovered by NetworkScope %q",
						account.ID, region, other.Name)))
			}
		}
	}
	return errs, nil
}
