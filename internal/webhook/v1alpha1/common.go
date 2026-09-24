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

// Package v1alpha1 holds the admission webhooks for the aws.hypersurgery API group.
//
// The webhooks are the fast feedback path, not the only check: everything they refuse, the
// controllers also refuse, in the status, for objects that were created while the webhook
// was not running. What the webhook adds is that the mistake is reported by the tool the
// person is already looking at, before the object exists.
//
// A rule is an error only when it cannot become true later. Anything that depends on
// discovery having run — whether a VPC is in the inventory yet — is a warning, because the
// answer changes on its own within a resync and refusing it would make the apply order of a
// GitOps repository matter.
package v1alpha1

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
	"hypersurgery.dev/subnet-operator/internal/tenancy"
)

// awsRegionPattern is the shape of a region name: eu-central-1, us-gov-east-1, cn-north-1.
// The list of real regions changes faster than a release of this operator, so the shape is
// all that is checked: a typo like "eu-central1" is caught, a brand new region is not.
var awsRegionPattern = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)

// AWS tag limits. CreateTags refuses anything past them, and "aws:" is reserved for AWS.
const (
	maxTagKeyLength   = 128
	maxTagValueLength = 256
	reservedTagPrefix = "aws:"
)

// arnAccountPattern pulls the account out of an IAM role ARN. The CRD already checks the
// overall shape, so the match is only used to compare the account.
var arnAccountPattern = regexp.MustCompile(`^arn:aws[a-z-]*:iam::([0-9]{12}):role/`)

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
	gk := schema.GroupKind{Group: awsv1alpha1.SchemeGroupVersion.Group, Kind: kind}
	return apierrors.NewInvalid(gk, name, errs)
}

// immutableField reports a change to a field that must not change after creation.
func immutableField[T comparable](path *field.Path, old, updated T) *field.Error {
	if old == updated {
		return nil
	}
	return field.Invalid(path, updated,
		fmt.Sprintf("field is immutable: it identifies resources in AWS that the operator would otherwise orphan (was %v)", old))
}

// validateRegion checks the shape of a region name.
func validateRegion(path *field.Path, region string) *field.Error {
	if awsRegionPattern.MatchString(region) {
		return nil
	}
	return field.Invalid(path, region, "not an AWS region name, e.g. eu-central-1")
}

// validateTags reports the tags AWS would refuse, so the mistake shows up at apply time
// instead of as a failed CreateTags call minutes later.
func validateTags(path *field.Path, tags map[string]string) field.ErrorList {
	var errs field.ErrorList
	for _, key := range slices.Sorted(maps.Keys(tags)) {
		keyPath := path.Key(key)
		switch {
		case key == "":
			errs = append(errs, field.Invalid(keyPath, key, "tag key must not be empty"))
		case strings.HasPrefix(strings.ToLower(key), reservedTagPrefix):
			errs = append(errs, field.Invalid(keyPath, key, `tag keys starting with "aws:" are reserved by AWS`))
		case len(key) > maxTagKeyLength:
			errs = append(errs, field.Invalid(keyPath, key,
				fmt.Sprintf("tag key is longer than the %d characters AWS allows", maxTagKeyLength)))
		}
		if len(tags[key]) > maxTagValueLength {
			errs = append(errs, field.Invalid(keyPath, tags[key],
				fmt.Sprintf("tag value is longer than the %d characters AWS allows", maxTagValueLength)))
		}
	}
	return errs
}

// scopeFor reads the NetworkScope an object refers to. A missing scope is a field error:
// unlike discovery, it does not appear on its own.
func scopeFor(ctx context.Context, reader client.Reader, path *field.Path, name string) (
	*awsv1alpha1.NetworkScope, *field.Error) {
	scope := &awsv1alpha1.NetworkScope{}
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
	scopes := &awsv1alpha1.NetworkScopeList{}
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
func validateNamespace(ctx context.Context, reader client.Reader, scope *awsv1alpha1.NetworkScope,
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
func validateCreatedByOnCreate(ctx context.Context, obj client.Object) *field.Error {
	req, err := admission.RequestFromContext(ctx)
	if err != nil || req.UserInfo.Username == "" {
		return nil
	}
	got := obj.GetAnnotations()[awsv1alpha1.AnnotationCreatedBy]
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
	annotations[awsv1alpha1.AnnotationCreatedBy] = req.UserInfo.Username
	obj.SetAnnotations(annotations)
}
