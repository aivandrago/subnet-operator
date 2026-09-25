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
	"k8s.io/apimachinery/pkg/runtime"
)

// SecretKeyRef points at one key of a Secret.
type SecretKeyRef struct {
	// name of the Secret.
	// +required
	Name string `json:"name"`

	// namespace of the Secret.
	// +required
	Namespace string `json:"namespace"`

	// key holding the Google service account JSON.
	// +kubebuilder:default="credentials.json"
	// +optional
	Key string `json:"key,omitempty"`
}

// SheetExportSpec describes a read-only mirror of a NetworkScope in a Google Sheet.
type SheetExportSpec struct {
	// scopeRef is the NetworkScope whose subnets are exported.
	// +required
	ScopeRef string `json:"scopeRef"`

	// spreadsheetID is the ID from the spreadsheet URL
	// (docs.google.com/spreadsheets/d/<spreadsheetID>/edit). The spreadsheet must be shared
	// with the service account as an editor.
	// +required
	SpreadsheetID string `json:"spreadsheetID"`

	// sheetName is the tab that holds the inventory. It is created if missing and fully
	// rewritten on every export, so people should not keep their own data in it.
	// +kubebuilder:default="Subnets"
	// +optional
	SheetName string `json:"sheetName,omitempty"`

	// credentialsSecretRef holds the Google service account JSON.
	// +required
	CredentialsSecretRef SecretKeyRef `json:"credentialsSecretRef"`

	// extraTagColumns adds one column per AWS tag key, after the standard columns.
	// +listType=atomic
	// +optional
	ExtraTagColumns []string `json:"extraTagColumns,omitempty"`

	// refreshInterval is how often the sheet is rewritten.
	// +kubebuilder:default="5m"
	// +optional
	RefreshInterval *metav1.Duration `json:"refreshInterval,omitempty"`
}

// SheetExportStatus is the observed state of the export.
type SheetExportStatus struct {
	// observedGeneration is the generation of the spec that was last exported.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// lastExportTime is when the sheet was last written.
	// +optional
	LastExportTime *metav1.Time `json:"lastExportTime,omitempty"`

	// rows is the number of subnets in the sheet, excluding the header.
	// +optional
	Rows int32 `json:"rows,omitempty"`

	// url links to the exported sheet.
	// +optional
	URL string `json:"url,omitempty"`

	// conditions: Ready is True when the last export succeeded.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:deprecatedversion:warning="aws.hypersurgery/v1alpha1 SheetExport is deprecated and removed in 0.9; the operator migrates it to network.hypersurgery.dev/v1beta1 SheetExport, use that instead"
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Scope",type=string,JSONPath=`.spec.scopeRef`
// +kubebuilder:printcolumn:name="Rows",type=integer,JSONPath=`.status.rows`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Last export",type=date,JSONPath=`.status.lastExportTime`
// +kubebuilder:printcolumn:name="URL",type=string,JSONPath=`.status.url`,priority=1

// SheetExport mirrors the subnets of a NetworkScope into a Google Sheet, for people who
// still want to look at a table. The sheet is written by the operator and never read back.
type SheetExport struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SheetExport
	// +required
	Spec SheetExportSpec `json:"spec"`

	// status defines the observed state of SheetExport
	// +optional
	Status SheetExportStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SheetExportList contains a list of SheetExport
type SheetExportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SheetExport `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &SheetExport{}, &SheetExportList{})
		return nil
	})
}
