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

// Package v1beta1 holds the admission webhooks for the network.hypersurgery.dev API group.
//
// The webhooks are the fast feedback path, not the only check: everything they refuse, the
// controllers also refuse, in the status, for objects that were created while the webhook
// was not running. What the webhook adds is that the mistake is reported by the tool the
// person is already looking at, before the object exists.
//
// A rule is an error only when it cannot become true later. Anything that depends on
// discovery having run — whether a network is in the inventory yet — is a warning, because the
// answer changes on its own within a resync and refusing it would make the apply order of a
// GitOps repository matter.
package v1beta1

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
	"hypersurgery.dev/subnet-operator/internal/provider"
	"hypersurgery.dev/subnet-operator/internal/tenancy"
)

// DefaultCertDir is where controller-runtime looks for the webhook serving certificate when
// the manager is not told otherwise.
const DefaultCertDir = "/tmp/k8s-webhook-server/serving-certs"

// CertificateAvailable reports whether a serving certificate is in place. The webhook server
// cannot start without one, so a manager that was installed without webhooks — the plain
// kustomize install, or "make run" on a laptop — leaves them off and says so, rather than
// crashing on a file it was never given.
func CertificateAvailable(dir, name string) bool {
	if dir == "" {
		dir = DefaultCertDir
	}
	info, err := os.Stat(filepath.Join(dir, name))
	return err == nil && !info.IsDir() && info.Size() > 0
}

// invalidError turns field errors into the message kubectl prints, or nil when the list is
// empty. Field errors are used throughout so the message names the field that is wrong.
func invalidError(kind, name string, errs field.ErrorList) error {
	if len(errs) == 0 {
		return nil
	}
	gk := schema.GroupKind{Group: networkv1beta1.SchemeGroupVersion.Group, Kind: kind}
	return apierrors.NewInvalid(gk, name, errs)
}

// immutableField reports a change to a field that must not change after creation.
func immutableField[T comparable](path *field.Path, old, updated T) *field.Error {
	if old == updated {
		return nil
	}
	return field.Invalid(path, updated,
		fmt.Sprintf("field is immutable: it identifies resources in the cloud that the operator would otherwise orphan (was %v)", old))
}

// providerFor returns the provider of the scope an object refers to. A provider the operator
// does not run is refused on the scope reference: nothing about the object can be checked or
// done without it.
func providerFor(providers *provider.Registry, scope *networkv1beta1.NetworkScope, scopeRef string) (
	provider.Provider, *field.Error) {
	p, ok := providers.Get(scope.Spec.Provider)
	if !ok {
		return nil, field.Invalid(field.NewPath("spec").Child("scopeRef"), scopeRef,
			fmt.Sprintf("NetworkScope %q: %s", scope.Name, providers.NotEnabled(scope.Spec.Provider)))
	}
	return p, nil
}

// scopeFor reads the NetworkScope an object refers to. A missing scope is a field error:
// unlike discovery, it does not appear on its own.
func scopeFor(ctx context.Context, reader client.Reader, path *field.Path, name string) (
	*networkv1beta1.NetworkScope, *field.Error) {
	scope := &networkv1beta1.NetworkScope{}
	if err := reader.Get(ctx, client.ObjectKey{Name: name}, scope); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, field.NotFound(path, name)
		}
		return nil, field.InternalError(path, err)
	}
	return scope, nil
}

// scopesCovering lists every NetworkScope that discovers the account/region pair, sorted by
// name so the message is the same on every call.
func scopesCovering(ctx context.Context, reader client.Reader, account, region string) ([]string, error) {
	scopes := &networkv1beta1.NetworkScopeList{}
	if err := reader.List(ctx, scopes); err != nil {
		return nil, err
	}
	var names []string
	for i := range scopes.Items {
		if scopes.Items[i].Covers(account, region) {
			names = append(names, scopes.Items[i].Name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// writesDisabledWarning tells the person that the object will be accepted and then sit
// there, because the operator was started without --enable-writes.
func writesDisabledWarning(what string) admission.Warnings {
	return admission.Warnings{
		fmt.Sprintf("the operator runs read-only: %s until it is started with --enable-writes", what),
	}
}

// validateNamespace refuses an object whose namespace the scope does not allow. The error is
// on spec.scopeRef, because that is the field that asks for the scope's roles; the object's
// namespace is not something the person can change in the manifest they are applying.
func validateNamespace(ctx context.Context, reader client.Reader, scope *networkv1beta1.NetworkScope,
	namespace string) *field.Error {
	path := field.NewPath("spec").Child("scopeRef")
	allowed, err := tenancy.Allowed(ctx, reader, scope, namespace)
	switch {
	case err != nil:
		// A namespace that cannot be read is refused too: letting the object in would hand it
		// the write roles on the strength of a check that did not run.
		return field.Forbidden(path, fmt.Sprintf("the namespace could not be checked against NetworkScope %q: %v",
			scope.Name, err))
	case !allowed:
		return field.Forbidden(path, tenancy.NotAllowedMessage(scope, namespace))
	}
	return nil
}

// validateCreatedByOnCreate refuses a new object whose created-by annotation is not the user
// the API server authenticated. The mutating webhook writes it, and it is failurePolicy
// Ignore, so this is what stops an object from being created without it — or with a forged
// one — when that call failed; the validating webhook is served by the same process and fails
// closed. Outside an admission request (a unit test calling the validator) there is no user to
// compare with and nothing is checked.
//
// There is no exception, the operator included. (0.8 had one for its migration from
// aws.hypersurgery/v1alpha1, which copied the old object's creator; 0.9 does not migrate.)
func validateCreatedByOnCreate(ctx context.Context, obj client.Object) *field.Error {
	req, err := admission.RequestFromContext(ctx)
	if err != nil || req.UserInfo.Username == "" {
		return nil
	}
	got := obj.GetAnnotations()[networkv1beta1.AnnotationCreatedBy]
	if got == req.UserInfo.Username {
		return nil
	}
	return field.Forbidden(createdByPath(),
		fmt.Sprintf("must name the user that creates the object (%s); it is set by the operator's mutating webhook, "+
			"which did not run or was overridden", req.UserInfo.Username))
}

// validateCreatedByOnUpdate refuses any change to the created-by annotation, including adding
// it to an object that was created without it and removing it.
func validateCreatedByOnUpdate(oldObj, newObj client.Object) *field.Error {
	oldValue, hadIt := oldObj.GetAnnotations()[networkv1beta1.AnnotationCreatedBy]
	newValue, hasIt := newObj.GetAnnotations()[networkv1beta1.AnnotationCreatedBy]
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
	return field.NewPath("metadata").Child("annotations").Key(networkv1beta1.AnnotationCreatedBy)
}

// stampCreatedBy writes the authenticated user into the created-by annotation on CREATE,
// replacing whatever the object carried: the annotation exists precisely so that the audit
// trail does not depend on what the person applying the object chose to write. Other
// operations leave it alone; the validating webhook refuses a change.
func stampCreatedBy(ctx context.Context, obj client.Object) {
	req, err := admission.RequestFromContext(ctx)
	if err != nil || req.Operation != admissionv1.Create || req.UserInfo.Username == "" {
		return
	}
	annotations := obj.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations[networkv1beta1.AnnotationCreatedBy] = req.UserInfo.Username
	obj.SetAnnotations(annotations)
}
