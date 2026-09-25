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

package v1

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/provider"
	"hypersurgery.dev/subnet-operator/internal/tenancy"
)

var networkscopelog = logf.Log.WithName("networkscope-resource")

// defaultResyncInterval matches the CRD default; the webhook only fills it in when the field
// was set to null explicitly, which the schema default does not cover.
var defaultResyncInterval = metav1.Duration{Duration: 10 * time.Minute}

// SetupNetworkScopeWebhookWithManager registers the webhook for NetworkScope in the manager.
func SetupNetworkScopeWebhookWithManager(mgr ctrl.Manager, providers *provider.Registry) error {
	return ctrl.NewWebhookManagedBy(mgr, &networkv1.NetworkScope{}).
		WithValidator(&NetworkScopeValidator{Client: mgr.GetClient(), Providers: providers}).
		WithDefaulter(&NetworkScopeDefaulter{}).
		Complete()
}

// +kubebuilder:webhook:path=/mutate-network-hypersurgery-dev-v1-networkscope,mutating=true,failurePolicy=ignore,sideEffects=None,groups=network.hypersurgery.dev,resources=networkscopes,verbs=create;update,versions=v1,name=mnetworkscope-v1.kb.io,admissionReviewVersions=v1

// NetworkScopeDefaulter fills in the tag keys and the resync interval.
type NetworkScopeDefaulter struct{}

// Default fills in what depends on the provider, which a static schema default cannot: the tag
// keys (hs/owner on AWS) and the auto-import policy's managed tag. It also covers what the
// schema default does not: an empty string is a value, so `tagKeys: {owner: ""}` keeps its
// empty string and would make the operator look for a tag with no name. Here it becomes the
// default key again.
func (d *NetworkScopeDefaulter) Default(_ context.Context, obj *networkv1.NetworkScope) error {
	obj.Spec.TagKeys = obj.ResolvedTagKeys()
	if p := obj.Spec.AutoImport; p != nil && p.ManagedTag == "" {
		p.ManagedTag = networkv1.DefaultManagedTag
	}
	if obj.Spec.ResyncInterval == nil {
		interval := defaultResyncInterval
		obj.Spec.ResyncInterval = &interval
	}
	return nil
}

// +kubebuilder:webhook:path=/validate-network-hypersurgery-dev-v1-networkscope,mutating=false,failurePolicy=fail,sideEffects=None,groups=network.hypersurgery.dev,resources=networkscopes,verbs=create;update,versions=v1,name=vnetworkscope-v1.kb.io,admissionReviewVersions=v1

// NetworkScopeValidator checks a scope against the shape its provider expects and against the
// other scopes: two scopes discovering the same account and region would mirror the same
// networks and subnets into the same objects and overwrite each other forever.
type NetworkScopeValidator struct {
	// Client lists the other scopes.
	Client client.Reader
	// Providers are the clouds the operator runs with; the scope's provider must be one.
	Providers *provider.Registry
}

// ValidateCreate checks a new scope.
func (v *NetworkScopeValidator) ValidateCreate(ctx context.Context, obj *networkv1.NetworkScope) (
	admission.Warnings, error) {
	networkscopelog.V(1).Info("Validating NetworkScope on create", "name", obj.GetName())
	return v.validate(ctx, obj, nil)
}

// ValidateUpdate checks a changed scope. Nothing in a scope is immutable: it owns no cloud
// resource, and removing an account is how discovery is stopped.
func (v *NetworkScopeValidator) ValidateUpdate(ctx context.Context, oldObj, newObj *networkv1.NetworkScope) (
	admission.Warnings, error) {
	networkscopelog.V(1).Info("Validating NetworkScope on update", "name", newObj.GetName())
	return v.validate(ctx, newObj, oldObj)
}

// ValidateDelete accepts every deletion: nothing in AWS is touched by it.
func (v *NetworkScopeValidator) ValidateDelete(_ context.Context, _ *networkv1.NetworkScope) (
	admission.Warnings, error) {
	return nil, nil
}

// Validate is everything that is checked on both create and update.
func (v *NetworkScopeValidator) Validate(ctx context.Context, scope *networkv1.NetworkScope) (
	admission.Warnings, error) {
	return v.validate(ctx, scope, nil)
}

// validate checks a scope; old is the scope before an update, nil on create.
func (v *NetworkScopeValidator) validate(ctx context.Context, scope, old *networkv1.NetworkScope) (
	admission.Warnings, error) {
	spec := field.NewPath("spec")
	errs := duplicateErrs(scope, old)
	var warnings admission.Warnings

	// The CRD's enum only lists providers some release implements; one this operator does not
	// run (not enabled, or a newer CRD in front of an older operator) cannot be synced.
	p, ok := v.Providers.Get(scope.Spec.Provider)
	if !ok {
		all := v.Providers.All()
		enabled := make([]string, 0, len(all))
		for _, q := range all {
			enabled = append(enabled, string(q.Name()))
		}
		errs = append(errs, field.NotSupported(spec.Child("provider"), scope.Spec.Provider, enabled))
		return warnings, invalidError("NetworkScope", scope.Name, errs)
	}
	providerWarnings, providerErrs := p.ValidateScope(scope)
	warnings = append(warnings, providerWarnings...)
	errs = append(errs, providerErrs...)

	overlapErrs, err := v.validateNoOverlap(ctx, scope)
	if err != nil {
		// The other scopes could not be listed. The account/region pair is not claimed by
		// anything we can see, and the controllers report a conflict in the status anyway.
		warnings = append(warnings, "the other NetworkScopes could not be read, so the account/region pairs were not "+
			"checked for overlap: "+err.Error())
	}
	errs = append(errs, overlapErrs...)

	if policy := scope.Spec.AutoImport; policy != nil && policy.Mode == networkv1.AutoImportApply {
		warnings = append(warnings, "auto-import is in Apply mode: unmanaged resources are tagged in the cloud "+
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
func (v *NetworkScopeValidator) validateNamespaces(ctx context.Context, scope *networkv1.NetworkScope) (
	admission.Warnings, field.ErrorList) {
	selectorPath := field.NewPath("spec").Child("namespaceSelector")
	if scope.Spec.NamespaceSelector == nil {
		warnings := admission.Warnings{tenancy.NoNamespaceWarning(scope)}
		if p := scope.Spec.AutoImport; p != nil && p.Mode != "" && p.Mode != networkv1.AutoImportOff {
			// The policy writes its imports into a namespace like anybody else, so it is
			// refused too; that is a scope that cannot do what it asks for.
			return nil, field.ErrorList{field.Required(selectorPath,
				"the auto-import policy writes its imports into spec.autoImport.namespace, which the selector must "+
					"allow; without a selector no namespace is allowed")}
		}
		return warnings, nil
	}
	if _, err := metav1.LabelSelectorAsSelector(scope.Spec.NamespaceSelector); err != nil {
		return nil, field.ErrorList{field.Invalid(selectorPath, scope.Spec.NamespaceSelector, err.Error())}
	}
	var warnings admission.Warnings
	if tenancy.Unrestricted(scope) {
		warnings = append(warnings, tenancy.UnrestrictedWarning(scope))
	}

	policy := scope.Spec.AutoImport
	if policy == nil || policy.Mode == "" || policy.Mode == networkv1.AutoImportOff {
		return warnings, nil
	}
	namespace := policy.ImportNamespace()
	allowed, err := tenancy.Allowed(ctx, v.Client, scope, namespace)
	switch {
	case errors.Is(err, tenancy.ErrNamespaceNotFound):
		return append(warnings, fmt.Sprintf(
			"namespace %q, where the auto-import policy writes its imports, does not exist yet; the policy creates "+
				"nothing until it exists and spec.namespaceSelector selects it", namespace)), nil
	case err != nil:
		return append(warnings, "the auto-import namespace could not be checked against spec.namespaceSelector: "+
			err.Error()), nil
	case !allowed:
		return warnings, field.ErrorList{field.Invalid(field.NewPath("spec").Child("autoImport").Child("namespace"),
			namespace, "spec.namespaceSelector does not select this namespace, so the imports the policy writes "+
				"there would be refused; select it, or point the policy at a namespace the selector allows")}
	}
	return warnings, nil
}

// validateNoOverlap refuses an account/region pair another scope already discovers. Two
// scopes over the same pair produce the same Network and Subnet objects from two reconcilers,
// and the inventory then flips between them.
func (v *NetworkScopeValidator) validateNoOverlap(ctx context.Context, scope *networkv1.NetworkScope) (
	field.ErrorList, error) {
	others := &networkv1.NetworkScopeList{}
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

// duplicateErrs refuses the same entry twice in the lists v1 declares as sets or maps: regions,
// required tags and the auto-import account defaults. The v1 schema refuses them itself, but a
// request made at v1beta1 is checked against the v1beta1 schema, which declared these lists
// atomic; this webhook sees both, as v1. A list an update leaves as it was is not checked
// again, so an object written with a duplicate before 1.0 can still be changed elsewhere.
func duplicateErrs(scope, old *networkv1.NetworkScope) field.ErrorList {
	spec := field.NewPath("spec")
	var errs field.ErrorList
	check := func(path *field.Path, values, oldValues []string, hadOld bool) {
		if hadOld && slices.Equal(values, oldValues) {
			return
		}
		seen := make(map[string]bool, len(values))
		for i, v := range values {
			if seen[v] {
				errs = append(errs, field.Duplicate(path.Index(i), v))
			}
			seen[v] = true
		}
	}
	var oldSpec networkv1.NetworkScopeSpec
	if old != nil {
		oldSpec = old.Spec
	}
	check(spec.Child("regions"), scope.Spec.Regions, oldSpec.Regions, old != nil)
	check(spec.Child("requiredSubnetTags"), scope.Spec.RequiredSubnetTags, oldSpec.RequiredSubnetTags, old != nil)
	for i, a := range scope.Spec.Accounts {
		oldAccount, found := networkv1.Account{}, false
		if old != nil {
			oldAccount, found = old.Account(a.ID)
		}
		check(spec.Child("accounts").Index(i).Child("regions"), a.Regions, oldAccount.Regions, found)
	}
	if p := scope.Spec.AutoImport; p != nil {
		accounts := make([]string, 0, len(p.AccountDefaults))
		for _, d := range p.AccountDefaults {
			accounts = append(accounts, d.Account)
		}
		var oldAccounts []string
		if old != nil && old.Spec.AutoImport != nil {
			for _, d := range old.Spec.AutoImport.AccountDefaults {
				oldAccounts = append(oldAccounts, d.Account)
			}
		}
		check(spec.Child("autoImport").Child("accountDefaults"), accounts, oldAccounts, old != nil)
	}
	return errs
}
