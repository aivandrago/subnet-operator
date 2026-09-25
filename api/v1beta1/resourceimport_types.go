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

package v1beta1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// Import states.
const (
	// ImportPending means the tags have not been applied yet.
	ImportPending = "Pending"
	// ImportApplied means the tags are on the resource.
	ImportApplied = "Applied"
	// ImportSkipped means nothing was applied on purpose: a dry run.
	ImportSkipped = "Skipped"
	// ImportFailed means the last attempt failed; see status.error.
	ImportFailed = "Failed"
)

// RequestedByPolicy is the requestedBy value of imports the auto-import policy creates.
const RequestedByPolicy = "auto-import policy"

// ResourceImportSpec asks for tags to be applied to one existing network or subnet.
type ResourceImportSpec struct {
	// scopeRef is the NetworkScope that supplies the credentials; the account and region
	// below must be part of it.
	// +required
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="NetworkScope",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	ScopeRef string `json:"scopeRef"`

	// account is the account that owns the resource, in the provider's format (for AWS the
	// 12-digit account ID).
	// +kubebuilder:validation:MinLength=1
	// +required
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Account",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	Account string `json:"account"`

	// region of the resource. Required for regional resources, which on AWS is every one.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Region",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	Region string `json:"region,omitempty"`

	// resourceID is the provider ID of the network or subnet to tag: for AWS a VPC or subnet
	// ID.
	// +kubebuilder:validation:MinLength=1
	// +required
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Resource ID",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	ResourceID string `json:"resourceID"`

	// tags are applied to the resource. Tags with other keys are left alone: the operator
	// only ever adds tags, it never removes one.
	// +kubebuilder:validation:MinProperties=1
	// +required
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Tags"
	Tags map[string]string `json:"tags"`

	// requestedBy records who asked for the import: a person, a team, or "auto-import policy".
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Requested By",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	RequestedBy string `json:"requestedBy,omitempty"`

	// dryRun records what would be applied without touching the cloud. The auto-import policy sets
	// it in DryRun mode, so a day of imports can be reviewed before anything is tagged.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Dry Run",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:booleanSwitch"}
	DryRun bool `json:"dryRun,omitempty"`
}

// ResourceImportStatus is the observed state of the import.
type ResourceImportStatus struct {
	// observedGeneration is the spec generation the status refers to.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// state is Pending, Applied, Skipped (dry run) or Failed.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="State",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	State string `json:"state,omitempty"`

	// appliedTags are the tags that reached the provider.
	// +optional
	AppliedTags map[string]string `json:"appliedTags,omitempty"`

	// appliedTime is when they were applied.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Applied",xDescriptors={"urn:alm:descriptor:timestamp"}
	AppliedTime *metav1.Time `json:"appliedTime,omitempty"`

	// error is the last failure, empty otherwise.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Error",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	Error string `json:"error,omitempty"`

	// conditions: Ready is True once the import is settled, whether it applied the tags or
	// deliberately did nothing in a dry run.
	// +listType=map
	// +listMapKey=type
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Conditions",xDescriptors={"urn:alm:descriptor:io.kubernetes.conditions"}
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:deprecatedversion:warning="network.hypersurgery.dev/v1beta1 is deprecated: use network.hypersurgery.dev/v1, which has the same fields. v1beta1 is served until at least 1.2 and six months after 1.0"
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=hypersurgery
// +kubebuilder:printcolumn:name="Resource",type=string,JSONPath=`.spec.resourceID`
// +kubebuilder:printcolumn:name="Account",type=string,JSONPath=`.spec.account`
// +kubebuilder:printcolumn:name="State",type=string,JSONPath=`.status.state`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +kubebuilder:printcolumn:name="Requested by",type=string,JSONPath=`.spec.requestedBy`,priority=1
// +operator-sdk:csv:customresourcedefinitions:displayName="Resource Import",resources={{Network,v1,""},{Subnet,v1,""},{Event,v1,""}}

// ResourceImport takes an existing network or subnet under management by tagging it.
// The resource itself, and every tag the import does not name, is left exactly as it was.
type ResourceImport struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the tags to apply
	// +required
	Spec ResourceImportSpec `json:"spec"`

	// status defines the observed state of ResourceImport
	// +optional
	Status ResourceImportStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ResourceImportList contains a list of ResourceImport
type ResourceImportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ResourceImport `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ResourceImport{}, &ResourceImportList{})
		return nil
	})
}
