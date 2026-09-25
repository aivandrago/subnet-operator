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

// Tag keys the operator writes on subnets it creates.
const (
	// TagManagedBy marks a subnet as created by the operator.
	TagManagedBy = "hs/managed-by"
	// TagManagedByValue is the value of TagManagedBy on operator-created subnets.
	TagManagedByValue = "subnet-operator"
	// TagClaim names the SubnetClaim (namespace/name) a subnet was created for.
	TagClaim = "hs/claim"
)

// ClaimMode says how far the operator goes with a claim.
type ClaimMode string

const (
	// ClaimModeAllocate only reserves CIDRs and writes them to the status. Nothing is
	// created in AWS: Terraform-first teams create the subnets from the status themselves.
	ClaimModeAllocate ClaimMode = "Allocate"
	// ClaimModeCreate reserves CIDRs and creates the subnets in AWS.
	ClaimModeCreate ClaimMode = "Create"
)

// Allocation states.
const (
	// AllocationPending means a CIDR is reserved but the subnet is not created yet.
	AllocationPending = "Pending"
	// AllocationCreated means the subnet exists in AWS.
	AllocationCreated = "Created"
	// AllocationFailed means the last attempt to create the subnet failed; see error.
	AllocationFailed = "Failed"
)

// SubnetClaimSpec asks for subnets in a VPC.
type SubnetClaimSpec struct {
	// scopeRef is the NetworkScope whose inventory and credentials are used. The account and
	// region below must be part of that scope.
	ScopeRef string `json:"scopeRef"`

	// account is the 12-digit AWS account ID that owns the VPC.
	Account string `json:"account"`

	// region is the AWS region of the VPC.
	Region string `json:"region"`

	// vpcID is the VPC the subnets are carved out of. It must be discovered by the scope.
	VPCID string `json:"vpcID"`

	// prefixLength is the size of every subnet, e.g. 24 for a /24.
	PrefixLength int32 `json:"prefixLength"`

	// availabilityZones lists the AZs to create one subnet in each, e.g. eu-central-1a.
	AvailabilityZones []string `json:"availabilityZones"`

	// mode is Allocate (reserve CIDRs only) or Create (also create the subnets).
	Mode ClaimMode `json:"mode,omitempty"`

	// owner, env and tier become the hs/owner, hs/env and hs/tier tags.
	Owner string `json:"owner"`
	Env   string `json:"env,omitempty"`
	Tier  string `json:"tier,omitempty"`

	// namePrefix is the Name tag; the AZ suffix is appended, e.g. payments-a. Defaults to the claim name.
	NamePrefix string `json:"namePrefix,omitempty"`

	// tags are added to every subnet. The hs/* tags above win on conflict.
	Tags map[string]string `json:"tags,omitempty"`

	// routeTableID is associated with every created subnet. Leave empty for the VPC main table.
	RouteTableID string `json:"routeTableID,omitempty"`

	// mapPublicIPOnLaunch sets the subnet attribute of the same name.
	MapPublicIPOnLaunch bool `json:"mapPublicIPOnLaunch,omitempty"`
}

// SubnetAllocation is one reserved CIDR and, once created, its subnet.
type SubnetAllocation struct {
	// availabilityZone the subnet is for.
	AvailabilityZone string `json:"availabilityZone"`
	// cidrBlock reserved for it.
	CIDRBlock string `json:"cidrBlock"`
	// subnetID once the subnet exists in AWS.
	SubnetID string `json:"subnetID,omitempty"`
	// state is Pending, Created or Failed.
	State string `json:"state,omitempty"`
	// error is the last creation error, empty otherwise.
	Error string `json:"error,omitempty"`
}

// SubnetClaimStatus is the observed state of the claim.
type SubnetClaimStatus struct {
	// observedGeneration is the spec generation the status refers to.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// allocations, one per availability zone. CIDRs stay stable once reserved.
	Allocations []SubnetAllocation `json:"allocations,omitempty"`

	// conditions: Allocated is True when every AZ has a CIDR; Ready is True when the claim
	// is fulfilled (subnets created, or allocated in Allocate mode).
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// SubnetClaim requests subnets in a VPC: the operator reserves free CIDRs and, in Create
// mode, creates the subnets with the organization's tags. Subnets are never deleted by the
// operator: deleting a claim leaves them in place.
type SubnetClaim struct {
	TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired subnets
	Spec SubnetClaimSpec `json:"spec"`

	// status defines the observed state of SubnetClaim
	Status SubnetClaimStatus `json:"status,omitzero"`
}
