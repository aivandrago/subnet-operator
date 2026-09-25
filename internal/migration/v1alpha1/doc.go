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

// Package v1alpha1 holds the Go types of aws.hypersurgery/v1alpha1, the API group the operator
// used up to 0.8, for one purpose only: `manager migrate-manifests` reads manifests written for
// it and converts them to network.hypersurgery.dev/v1. The group is not served any more
// (it was removed in 0.9), so nothing here is registered in a scheme or generates a CRD, and
// the validation markers the old CRDs were generated from are gone with them.
package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// APIVersion is the group and version these types were served as.
const APIVersion = "aws.hypersurgery/v1alpha1"

// TypeMeta is what the kinds here embed in place of metav1.TypeMeta. controller-gen takes every
// type that embeds both metav1.TypeMeta and metav1.ObjectMeta for an API kind and writes a CRD
// for it, and these are not an API any more. The JSON is the same.
type TypeMeta struct {
	metav1.TypeMeta `json:",inline"`
}
