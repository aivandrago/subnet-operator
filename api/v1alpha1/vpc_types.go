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

// VPCSpec identifies the observed VPC. It is written by the operator, not by users.
type VPCSpec struct {
	// vpcID is the AWS VPC ID.
	VPCID string `json:"vpcID"`
	// account is the AWS account ID that owns the VPC.
	Account string `json:"account"`
	// region is the AWS region of the VPC.
	Region string `json:"region"`
}

// VPCStatus is the observed state of the VPC.
type VPCStatus struct {
	// name is the value of the Name tag.
	// +optional
	Name string `json:"name,omitempty"`
	// state is the AWS VPC state.
	// +optional
	State string `json:"state,omitempty"`
	// isDefault is true for the default VPC of the region.
	// +optional
	IsDefault bool `json:"isDefault,omitempty"`
	// cidrBlocks are the associated IPv4 CIDR blocks, primary first.
	// +listType=atomic
	// +optional
	CIDRBlocks []string `json:"cidrBlocks,omitempty"`
	// ipv6CIDRBlocks are the associated IPv6 CIDR blocks.
	// +listType=atomic
	// +optional
	IPv6CIDRBlocks []string `json:"ipv6CIDRBlocks,omitempty"`
	// owner is read from the owner tag configured in the NetworkScope.
	// +optional
	Owner string `json:"owner,omitempty"`
	// env is read from the env tag configured in the NetworkScope.
	// +optional
	Env string `json:"env,omitempty"`
	// tags are the AWS tags of the VPC.
	// +optional
	Tags map[string]string `json:"tags,omitempty"`
	// subnets is the number of discovered subnets.
	// +optional
	Subnets int32 `json:"subnets,omitempty"`
	// totalIPs is the sum of usable IPv4 addresses in all subnets.
	// +optional
	TotalIPs int64 `json:"totalIPs,omitempty"`
	// availableIPs is the sum of free IPv4 addresses in all subnets.
	// +optional
	AvailableIPs int64 `json:"availableIPs,omitempty"`
	// overlapsWith lists VPCs in the same NetworkScope whose CIDRs overlap this VPC,
	// as "<account>/<region>/<vpc-id>". Overlaps break peering and Transit Gateway routing.
	// +listType=atomic
	// +optional
	OverlapsWith []string `json:"overlapsWith,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Account",type=string,JSONPath=`.spec.account`
// +kubebuilder:printcolumn:name="Region",type=string,JSONPath=`.spec.region`
// +kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.status.name`
// +kubebuilder:printcolumn:name="CIDR",type=string,JSONPath=`.status.cidrBlocks[0]`
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.status.owner`
// +kubebuilder:printcolumn:name="Subnets",type=integer,JSONPath=`.status.subnets`
// +kubebuilder:printcolumn:name="Free IPs",type=integer,JSONPath=`.status.availableIPs`
// +kubebuilder:printcolumn:name="Env",type=string,JSONPath=`.status.env`,priority=1
// +kubebuilder:printcolumn:name="Overlaps",type=string,JSONPath=`.status.overlapsWith`,priority=1

// VPC is an AWS VPC discovered by a NetworkScope. It is read-only for users.
type VPC struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec identifies the VPC
	// +required
	Spec VPCSpec `json:"spec"`

	// status is the observed state of the VPC
	// +optional
	Status VPCStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// VPCList contains a list of VPC
type VPCList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []VPC `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &VPC{}, &VPCList{})
		return nil
	})
}
