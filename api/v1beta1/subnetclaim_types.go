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
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
// +kubebuilder:validation:Enum=Allocate;Create
type ClaimMode string

const (
	// ClaimModeAllocate only reserves CIDRs and writes them to the status. Nothing is
	// created in the cloud: Terraform-first teams create the subnets from the status themselves.
	ClaimModeAllocate ClaimMode = "Allocate"
	// ClaimModeCreate reserves CIDRs and creates the subnets.
	ClaimModeCreate ClaimMode = "Create"
)

// Allocation states.
const (
	// AllocationPending means a CIDR is reserved but the subnet is not created yet.
	AllocationPending = "Pending"
	// AllocationCreated means the subnet exists.
	AllocationCreated = "Created"
	// AllocationFailed means the last attempt to create the subnet failed; see error.
	AllocationFailed = "Failed"
)

// AWSClaimOptions are the settings of subnets created on AWS.
type AWSClaimOptions struct {
	// routeTableID is associated with every created subnet. Leave empty for the VPC main table.
	// +kubebuilder:validation:Pattern=`^rtb-[0-9a-f]+$`
	// +optional
	RouteTableID string `json:"routeTableID,omitempty"`

	// mapPublicIPOnLaunch sets the subnet attribute of the same name.
	// +optional
	MapPublicIPOnLaunch bool `json:"mapPublicIPOnLaunch,omitempty"`
}

// SubnetClaimSpec asks for subnets in a network.
type SubnetClaimSpec struct {
	// scopeRef is the NetworkScope whose inventory and credentials are used. The account and
	// region below must be part of that scope, and the claim's provider is the scope's.
	// +required
	ScopeRef string `json:"scopeRef"`

	// account is the account that owns the network, in the provider's format (for AWS the
	// 12-digit account ID).
	// +kubebuilder:validation:MinLength=1
	// +required
	Account string `json:"account"`

	// region of the network.
	// +required
	Region string `json:"region"`

	// networkID is the network the subnets are carved out of, as its provider ID (for AWS the
	// VPC ID). It must be discovered by the scope.
	// +kubebuilder:validation:MinLength=1
	// +required
	NetworkID string `json:"networkID"`

	// prefixLength is the size of every subnet, e.g. 24 for a /24. The provider decides the
	// range; AWS allows 16 to 28.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=32
	// +required
	PrefixLength int32 `json:"prefixLength"`

	// zones lists the zones to create one subnet in each. Required for providers whose
	// subnets are zonal: on AWS one to six availability zones, e.g. eu-central-1a.
	// +kubebuilder:validation:MaxItems=6
	// +listType=set
	// +optional
	Zones []string `json:"zones,omitempty"`

	// mode is Allocate (reserve CIDRs only) or Create (also create the subnets).
	// +kubebuilder:default=Create
	// +optional
	Mode ClaimMode `json:"mode,omitempty"`

	// owner, env and tier become the owner, env and tier tags (hs/owner, hs/env and hs/tier on
	// AWS).
	// +required
	Owner string `json:"owner"`
	// +optional
	Env string `json:"env,omitempty"`
	// +optional
	Tier string `json:"tier,omitempty"`

	// namePrefix is the Name tag; the zone suffix is appended, e.g. payments-a. Defaults to the
	// claim name.
	// +optional
	NamePrefix string `json:"namePrefix,omitempty"`

	// tags are added to every subnet. The owner, env and tier tags above win on conflict.
	// +optional
	Tags map[string]string `json:"tags,omitempty"`

	// aws holds the settings of subnets created on AWS. Only valid when the scope's provider is
	// AWS.
	// +optional
	AWS *AWSClaimOptions `json:"aws,omitempty"`
}

// SubnetAllocation is one reserved CIDR and, once created, its subnet.
type SubnetAllocation struct {
	// name of the subnet the allocation is for: namePrefix and the zone suffix. It is the Name
	// tag the created subnet carries, and the key of the list.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// zone the subnet is for, on providers with zonal subnets.
	// +optional
	Zone string `json:"zone,omitempty"`
	// cidrBlock reserved for it.
	CIDRBlock string `json:"cidrBlock"`
	// subnetID is the provider ID of the subnet once it exists.
	// +optional
	SubnetID string `json:"subnetID,omitempty"`
	// state is Pending, Created or Failed.
	// +optional
	State string `json:"state,omitempty"`
	// error is the last creation error, empty otherwise.
	// +optional
	Error string `json:"error,omitempty"`
}

// SubnetClaimStatus is the observed state of the claim.
type SubnetClaimStatus struct {
	// observedGeneration is the spec generation the status refers to.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// allocations, one per subnet. CIDRs stay stable once reserved.
	// +listType=map
	// +listMapKey=name
	// +optional
	Allocations []SubnetAllocation `json:"allocations,omitempty"`

	// conditions: Allocated is True when every subnet has a CIDR; Ready is True when the claim
	// is fulfilled (subnets created, or allocated in Allocate mode).
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:categories=hypersurgery
// +kubebuilder:printcolumn:name="Network",type=string,JSONPath=`.spec.networkID`
// +kubebuilder:printcolumn:name="Prefix",type=integer,JSONPath=`.spec.prefixLength`
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.mode`
// +kubebuilder:printcolumn:name="Allocated",type=string,JSONPath=`.status.conditions[?(@.type=="Allocated")].status`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Account",type=string,JSONPath=`.spec.account`,priority=1
// +kubebuilder:printcolumn:name="Region",type=string,JSONPath=`.spec.region`,priority=1

// SubnetClaim requests subnets in a network: the operator reserves free CIDRs and, in Create
// mode, creates the subnets with the organization's tags. Subnets are never deleted by the
// operator: deleting a claim leaves them in place.
type SubnetClaim struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired subnets
	// +required
	Spec SubnetClaimSpec `json:"spec"`

	// status defines the observed state of SubnetClaim
	// +optional
	Status SubnetClaimStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SubnetClaimList contains a list of SubnetClaim
type SubnetClaimList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []SubnetClaim `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &SubnetClaim{}, &SubnetClaimList{})
		return nil
	})
}

// AWSOptions returns the claim's AWS options, empty when it has none.
func (c *SubnetClaim) AWSOptions() AWSClaimOptions {
	if c.Spec.AWS == nil {
		return AWSClaimOptions{}
	}
	return *c.Spec.AWS
}

// NamePrefixOrName is the prefix of the Name tag of the claim's subnets.
func (c *SubnetClaim) NamePrefixOrName() string {
	if c.Spec.NamePrefix != "" {
		return c.Spec.NamePrefix
	}
	return c.Name
}

// SubnetName is the name of the claim's subnet in a zone: the name prefix and the zone's
// suffix after the region, e.g. payments-a for eu-central-1a. It is both the Name tag of a
// created subnet and the key of its allocation.
func SubnetName(prefix, region, zone string) string {
	return prefix + "-" + strings.TrimPrefix(zone, region)
}
