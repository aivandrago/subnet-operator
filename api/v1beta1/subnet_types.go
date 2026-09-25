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

// SubnetSpec identifies the observed subnet. It is written by the operator, not by users.
type SubnetSpec struct {
	// provider is the provider of the scope that discovered the subnet.
	Provider Provider `json:"provider"`
	// id is the provider's ID of the subnet; for AWS the subnet ID.
	ID string `json:"id"`
	// networkID is the ID of the network the subnet belongs to; for AWS the VPC ID.
	NetworkID string `json:"networkID"`
	// account is the account that owns the subnet.
	Account string `json:"account"`
	// region of the subnet.
	Region string `json:"region"`
}

// AWSSubnetStatus is what only an AWS subnet has.
type AWSSubnetStatus struct {
	// availabilityZoneID is the account-independent AZ ID, e.g. euc1-az2.
	// +optional
	AvailabilityZoneID string `json:"availabilityZoneID,omitempty"`
	// public is true when the subnet's route table has a default route to an internet gateway.
	// +optional
	Public bool `json:"public,omitempty"`
	// routeTableID is the explicitly associated route table, or the VPC main route table.
	// +optional
	RouteTableID string `json:"routeTableID,omitempty"`
}

// SubnetStatus is the observed state of the subnet.
type SubnetStatus struct {
	// name is the value of the Name tag.
	// +optional
	Name string `json:"name,omitempty"`
	// state is the provider's state of the subnet.
	// +optional
	State string `json:"state,omitempty"`
	// cidrBlock is the IPv4 CIDR block.
	// +optional
	CIDRBlock string `json:"cidrBlock,omitempty"`
	// ipv6CIDRBlocks are the associated IPv6 CIDR blocks.
	// +listType=atomic
	// +optional
	IPv6CIDRBlocks []string `json:"ipv6CIDRBlocks,omitempty"`
	// zone is the zone of a zonal subnet, e.g. the AWS availability zone eu-central-1a. Empty
	// for providers whose subnets are regional.
	// +optional
	Zone string `json:"zone,omitempty"`
	// totalIPs is the number of usable IPv4 addresses (AWS reserves 5 per subnet). Unset when
	// unknown.
	// +optional
	TotalIPs *int64 `json:"totalIPs,omitempty"`
	// availableIPs is the number of free IPv4 addresses. Unset when the provider could not
	// report it, which is not the same as zero.
	// +optional
	AvailableIPs *int64 `json:"availableIPs,omitempty"`
	// utilizationPercent is the share of used IPv4 addresses, 0-100. Unset when unknown.
	// +optional
	UtilizationPercent *int32 `json:"utilizationPercent,omitempty"`
	// owner is read from the owner tag configured in the NetworkScope.
	// +optional
	Owner string `json:"owner,omitempty"`
	// env is read from the env tag configured in the NetworkScope.
	// +optional
	Env string `json:"env,omitempty"`
	// tier is read from the tier tag configured in the NetworkScope.
	// +optional
	Tier string `json:"tier,omitempty"`
	// tags are the subnet's tags at the provider.
	// +optional
	Tags map[string]string `json:"tags,omitempty"`
	// missingTags lists required tag keys that are absent or empty.
	// +listType=atomic
	// +optional
	MissingTags []string `json:"missingTags,omitempty"`
	// aws holds what only an AWS subnet has.
	// +optional
	AWS *AWSSubnetStatus `json:"aws,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=hssubnet,categories=hypersurgery
// +kubebuilder:printcolumn:name="Network",type=string,JSONPath=`.spec.networkID`
// +kubebuilder:printcolumn:name="CIDR",type=string,JSONPath=`.status.cidrBlock`
// +kubebuilder:printcolumn:name="Zone",type=string,JSONPath=`.status.zone`
// +kubebuilder:printcolumn:name="Free IPs",type=integer,JSONPath=`.status.availableIPs`
// +kubebuilder:printcolumn:name="Used %",type=integer,JSONPath=`.status.utilizationPercent`
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.status.owner`
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`,priority=1
// +kubebuilder:printcolumn:name="Public",type=boolean,JSONPath=`.status.aws.public`,priority=1
// +kubebuilder:printcolumn:name="Account",type=string,JSONPath=`.spec.account`,priority=1
// +kubebuilder:printcolumn:name="Region",type=string,JSONPath=`.spec.region`,priority=1
// +kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.status.name`,priority=1
// +kubebuilder:printcolumn:name="Env",type=string,JSONPath=`.status.env`,priority=1
// +kubebuilder:printcolumn:name="Tier",type=string,JSONPath=`.status.tier`,priority=1
// +kubebuilder:printcolumn:name="Missing tags",type=string,JSONPath=`.status.missingTags`,priority=1

// Subnet is a subnet discovered by a NetworkScope. It is read-only for users. On AWS the
// object is named after the subnet ID.
type Subnet struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec identifies the subnet
	// +required
	Spec SubnetSpec `json:"spec"`

	// status is the observed state of the subnet
	// +optional
	Status SubnetStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// SubnetList contains a list of Subnet
type SubnetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Subnet `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Subnet{}, &SubnetList{})
		return nil
	})
}

// AWSStatus returns the AWS part of the status, empty when there is none.
func (s *SubnetStatus) AWSStatus() AWSSubnetStatus {
	if s.AWS == nil {
		return AWSSubnetStatus{}
	}
	return *s.AWS
}
