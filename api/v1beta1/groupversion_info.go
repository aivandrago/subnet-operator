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

// Package v1beta1 contains API Schema definitions for the network.hypersurgery.dev v1beta1 API
// group: the cloud-neutral API that replaced aws.hypersurgery/v1alpha1 in 0.8 (ADR 0002).
//
// Deprecated since 1.0: use package v1, which has the same fields. The API server still serves
// v1beta1, converting through the operator's conversion webhook, until at least 1.2 and six
// months after 1.0 (docs/api-compatibility.md). The operator itself reads and writes v1 only.
//
// Every kind that talks about the cloud names its provider, directly (NetworkScope, Network,
// Subnet) or through the scope it refers to (SubnetClaim, ResourceImport). Provider is an open
// enum: clients must treat a value they do not know as "a provider this client does not know"
// and skip the object instead of failing.
// +kubebuilder:object:generate=true
// +groupName=network.hypersurgery.dev
package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// SchemeGroupVersion is group version used to register these objects.
	// This name is used by applyconfiguration generators (e.g. controller-gen).
	SchemeGroupVersion = schema.GroupVersion{Group: "network.hypersurgery.dev", Version: "v1beta1"}

	// GroupVersion is an alias for SchemeGroupVersion, for backward compatibility.
	GroupVersion = SchemeGroupVersion

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = runtime.NewSchemeBuilder(func(scheme *runtime.Scheme) error {
		metav1.AddToGroupVersion(scheme, SchemeGroupVersion)
		return nil
	})

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)
