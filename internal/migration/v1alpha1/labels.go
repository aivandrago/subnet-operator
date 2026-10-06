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

// Labels set on discovered VPC and Subnet objects.
const (
	LabelScope   = "aws.hypersurgery/scope"
	LabelAccount = "aws.hypersurgery/account"
	LabelRegion  = "aws.hypersurgery/region"
	LabelVPC     = "aws.hypersurgery/vpc"
)

// AnnotationCreatedBy carries the Kubernetes user that created a SubnetClaim or a
// ResourceImport, as the API server authenticated it. The operator's admission webhooks write
// it on creation, whatever the object carried, and refuse any later change to it, so unlike
// spec.requestedBy or spec.owner it cannot name somebody else.
const AnnotationCreatedBy = "aws.hypersurgery/created-by"
