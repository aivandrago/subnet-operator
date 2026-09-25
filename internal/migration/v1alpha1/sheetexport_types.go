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

// SecretKeyRef points at one key of a Secret.
type SecretKeyRef struct {
	// name of the Secret.
	Name string `json:"name"`

	// namespace of the Secret.
	Namespace string `json:"namespace"`

	// key holding the Google service account JSON.
	Key string `json:"key,omitempty"`
}

// SheetExportSpec describes a read-only mirror of a NetworkScope in a Google Sheet.
type SheetExportSpec struct {
	// scopeRef is the NetworkScope whose subnets are exported.
	ScopeRef string `json:"scopeRef"`

	// spreadsheetID is the ID from the spreadsheet URL
	// (docs.google.com/spreadsheets/d/<spreadsheetID>/edit). The spreadsheet must be shared
	// with the service account as an editor.
	SpreadsheetID string `json:"spreadsheetID"`

	// sheetName is the tab that holds the inventory. It is created if missing and fully
	// rewritten on every export, so people should not keep their own data in it.
	SheetName string `json:"sheetName,omitempty"`

	// credentialsSecretRef holds the Google service account JSON.
	CredentialsSecretRef SecretKeyRef `json:"credentialsSecretRef"`

	// extraTagColumns adds one column per AWS tag key, after the standard columns.
	ExtraTagColumns []string `json:"extraTagColumns,omitempty"`

	// refreshInterval is how often the sheet is rewritten.
	RefreshInterval *metav1.Duration `json:"refreshInterval,omitempty"`
}

// SheetExportStatus is the observed state of the export.
type SheetExportStatus struct {
	// observedGeneration is the generation of the spec that was last exported.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// lastExportTime is when the sheet was last written.
	LastExportTime *metav1.Time `json:"lastExportTime,omitempty"`

	// rows is the number of subnets in the sheet, excluding the header.
	Rows int32 `json:"rows,omitempty"`

	// url links to the exported sheet.
	URL string `json:"url,omitempty"`

	// conditions: Ready is True when the last export succeeded.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SheetExport mirrors the subnets of a NetworkScope into a Google Sheet, for people who
// still want to look at a table. The sheet is written by the operator and never read back.
type SheetExport struct {
	TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of SheetExport
	Spec SheetExportSpec `json:"spec"`

	// status defines the observed state of SheetExport
	Status SheetExportStatus `json:"status,omitzero"`
}
