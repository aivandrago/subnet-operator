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

// Package tenancy decides which namespaces may use a NetworkScope. The admission webhooks
// and the controllers both ask it, so the rule cannot differ between refusing an object at
// apply time and refusing it in its status.
package tenancy

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
)

// ReasonNamespaceNotAllowed is the status condition and Event reason of an object whose
// namespace the scope's namespaceSelector does not select.
const ReasonNamespaceNotAllowed = "NamespaceNotAllowed"

// ErrNamespaceNotFound is returned by Allowed when the namespace does not exist, so a caller
// that can wait for it (a scope naming the namespace its auto-import policy writes to) can
// tell that apart from a namespace that exists and is not selected.
var ErrNamespaceNotFound = errors.New("namespace not found")

// ErrInvalidSelector is returned by Allowed when the scope's namespaceSelector does not parse.
// Such a scope allows no namespace.
var ErrInvalidSelector = errors.New("invalid spec.namespaceSelector")

// Refused reports whether an error from Allowed is a refusal in its own right — the namespace
// is gone or the selector is broken — rather than a failure to ask, which is worth a retry.
func Refused(err error) bool {
	return errors.Is(err, ErrNamespaceNotFound) || errors.Is(err, ErrInvalidSelector)
}

// Allowed reports whether objects in the namespace may refer to the scope. The namespace's
// labels are read through reader; the scope's selector decides. A scope without a selector
// allows every namespace without reading anything.
func Allowed(ctx context.Context, reader client.Reader, scope *awsv1alpha1.NetworkScope, namespace string) (bool, error) {
	if scope.Spec.NamespaceSelector == nil {
		return true, nil
	}
	ns := &corev1.Namespace{}
	if err := reader.Get(ctx, client.ObjectKey{Name: namespace}, ns); err != nil {
		if apierrors.IsNotFound(err) {
			return false, fmt.Errorf("%w: %s", ErrNamespaceNotFound, namespace)
		}
		return false, err
	}
	allowed, err := scope.AllowsNamespace(ns.Labels)
	if err != nil {
		return false, fmt.Errorf("%w of NetworkScope %q: %w", ErrInvalidSelector, scope.Name, err)
	}
	return allowed, nil
}

// NotAllowedMessage is the sentence the webhooks and the controllers use for a refusal, so
// that `kubectl apply` and `kubectl describe` say the same thing.
func NotAllowedMessage(scope *awsv1alpha1.NetworkScope, namespace string) string {
	return fmt.Sprintf("NetworkScope %q does not allow namespace %q to use it (spec.namespaceSelector: %s); "+
		"a cluster administrator decides which namespaces may use a scope", scope.Name, namespace,
		metav1.FormatLabelSelector(scope.Spec.NamespaceSelector))
}

// UnrestrictedWarning is the sentence for a scope without a namespaceSelector.
func UnrestrictedWarning(scope *awsv1alpha1.NetworkScope) string {
	return fmt.Sprintf("NetworkScope %q has no spec.namespaceSelector, so a SubnetClaim or ResourceImport in any "+
		"namespace can make the operator use its roles; set one (the v1 API will allow no namespace without it)",
		scope.Name)
}
