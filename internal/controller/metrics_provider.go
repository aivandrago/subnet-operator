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

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
)

// scopeProvider is the provider of the named scope, for the provider label of a claim's or an
// import's readiness metric. It is empty while the scope cannot be read: the metric still says
// whether the object is ready, and the next reconcile fills the label in.
func scopeProvider(ctx context.Context, c client.Reader, name string) networkv1beta1.Provider {
	scope := &networkv1beta1.NetworkScope{}
	if err := c.Get(ctx, types.NamespacedName{Name: name}, scope); err != nil {
		return ""
	}
	return scope.Spec.Provider
}
