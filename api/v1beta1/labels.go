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

// Labels set on discovered Network and Subnet objects, and on the ResourceImports the
// auto-import policy writes.
const (
	LabelScope    = "network.hypersurgery.dev/scope"
	LabelProvider = "network.hypersurgery.dev/provider"
	LabelAccount  = "network.hypersurgery.dev/account"
	LabelRegion   = "network.hypersurgery.dev/region"
	LabelNetwork  = "network.hypersurgery.dev/network"
	// LabelResource carries the provider ID of the resource an object refers to, so the
	// imports for one resource can be found without listing every import.
	LabelResource = "network.hypersurgery.dev/resource"
)

// AnnotationCreatedBy carries the Kubernetes user that created a SubnetClaim or a
// ResourceImport, as the API server authenticated it. The operator's admission webhooks write
// it on creation, whatever the object carried, and refuse any later change to it, so unlike
// spec.requestedBy or spec.owner it cannot name somebody else.
//
// There is one exception, for the migration from aws.hypersurgery/v1alpha1: an object the
// operator itself creates with AnnotationMigratedFrom keeps the created-by value it copied from
// the old object, because the old group's webhooks guarded that value the same way.
const AnnotationCreatedBy = "network.hypersurgery.dev/created-by"

// AnnotationReason says why the auto-import policy created a ResourceImport.
const AnnotationReason = "network.hypersurgery.dev/reason"

// AnnotationMigratedFrom is set on an object the operator created by migrating an
// aws.hypersurgery/v1alpha1 object. Its value is the old object's apiVersion.
const AnnotationMigratedFrom = "network.hypersurgery.dev/migrated-from"

// AnnotationMigratedTo is set on an aws.hypersurgery/v1alpha1 object once it has been migrated.
// Its value is the name of the network.hypersurgery.dev object that replaces it, which from then
// on is the only one reconciled.
const AnnotationMigratedTo = "network.hypersurgery.dev/migrated-to"
