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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Import states.
const (
	// ImportPending means the tags have not been applied yet.
	ImportPending = "Pending"
	// ImportApplied means the tags are on the resource in AWS.
	ImportApplied = "Applied"
	// ImportSkipped means nothing was applied on purpose: a dry run.
	ImportSkipped = "Skipped"
	// ImportFailed means the last attempt failed; see status.error.
	ImportFailed = "Failed"
)

// RequestedByPolicy is the requestedBy value of imports the auto-import policy creates.
const RequestedByPolicy = "auto-import policy"

// LabelResource carries the AWS resource ID an object refers to, so the imports for one
// resource can be found without listing every import.
const LabelResource = "aws.hypersurgery/resource"

// ResourceImportSpec asks for tags to be applied to one existing AWS resource.
type ResourceImportSpec struct {
	// scopeRef is the NetworkScope that supplies the credentials; the account and region
	// below must be part of it.
	ScopeRef string `json:"scopeRef"`

	// account is the 12-digit AWS account ID that owns the resource.
	Account string `json:"account"`

	// region is the AWS region of the resource.
	Region string `json:"region"`

	// resourceID is the VPC or subnet to tag.
	ResourceID string `json:"resourceID"`

	// tags are applied to the resource. Tags with other keys are left alone: the operator
	// only ever adds tags, it never removes one.
	Tags map[string]string `json:"tags"`

	// requestedBy records who asked for the import: a person, a team, or "auto-import policy".
	RequestedBy string `json:"requestedBy,omitempty"`

	// dryRun records what would be applied without touching AWS. The auto-import policy sets
	// it in DryRun mode, so a day of imports can be reviewed before anything is tagged.
	DryRun bool `json:"dryRun,omitempty"`
}

// ResourceImportStatus is the observed state of the import.
type ResourceImportStatus struct {
	// observedGeneration is the spec generation the status refers to.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// state is Pending, Applied, Skipped (dry run) or Failed.
	State string `json:"state,omitempty"`

	// appliedTags are the tags that reached AWS.
	AppliedTags map[string]string `json:"appliedTags,omitempty"`

	// appliedTime is when they were applied.
	AppliedTime *metav1.Time `json:"appliedTime,omitempty"`

	// error is the last failure, empty otherwise.
	Error string `json:"error,omitempty"`

	// conditions: Ready is True once the import is settled, whether it applied the tags or
	// deliberately did nothing in a dry run.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ResourceImport takes an existing VPC or subnet under management by tagging it in AWS.
// The resource itself, and every tag the import does not name, is left exactly as it was.
type ResourceImport struct {
	TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the tags to apply
	Spec ResourceImportSpec `json:"spec"`

	// status defines the observed state of ResourceImport
	Status ResourceImportStatus `json:"status,omitzero"`
}
