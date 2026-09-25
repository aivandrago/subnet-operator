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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
	"hypersurgery.dev/subnet-operator/internal/migration"
)

// legacyScopes is the reader the new group's validators get when they check an old object:
// NetworkScopes that are still waiting for their migration are read from the old group and
// converted, so an old claim applied together with its old scope is checked against that scope
// rather than refused because its copy does not exist yet. Everything else is read as it is.
type legacyScopes struct {
	client.Reader
}

// Get reads a scope from the new group, and from the old one when it is not migrated yet.
func (r *legacyScopes) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := r.Reader.Get(ctx, key, obj, opts...)
	scope, isScope := obj.(*networkv1beta1.NetworkScope)
	if !isScope || !apierrors.IsNotFound(err) {
		return err
	}
	old := &awsv1alpha1.NetworkScope{}
	if oldErr := r.Reader.Get(ctx, key, old); oldErr != nil {
		return err
	}
	if _, done := old.Annotations[networkv1beta1.AnnotationMigratedTo]; done {
		return err
	}
	converted, _ := migration.NetworkScope(old, migration.ForCluster)
	converted.DeepCopyInto(scope)
	return nil
}

// List lists scopes from the new group plus the old ones not migrated yet.
func (r *legacyScopes) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := r.Reader.List(ctx, list, opts...); err != nil {
		return err
	}
	scopes, ok := list.(*networkv1beta1.NetworkScopeList)
	if !ok {
		return nil
	}
	old := &awsv1alpha1.NetworkScopeList{}
	if err := r.Reader.List(ctx, old, opts...); err != nil {
		return nil //nolint:nilerr // the new group's answer stands on its own
	}
	have := map[string]bool{}
	for i := range scopes.Items {
		have[scopes.Items[i].Name] = true
	}
	for i := range old.Items {
		o := &old.Items[i]
		if _, done := o.Annotations[networkv1beta1.AnnotationMigratedTo]; done || have[o.Name] {
			continue
		}
		converted, _ := migration.NetworkScope(o, migration.ForCluster)
		scopes.Items = append(scopes.Items, *converted)
	}
	return nil
}
