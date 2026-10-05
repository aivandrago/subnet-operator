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

package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// NetworkSpec identifies the observed network. It is written by the operator, not by users.
type NetworkSpec struct {
	// provider is the provider of the scope that discovered the network.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Provider",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	Provider Provider `json:"provider"`
	// id is the provider's ID of the network: the VPC ID on AWS, the network's resource name
	// (projects/<project>/global/networks/<name>) on GCP.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="ID",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	ID string `json:"id"`
	// account is the account that owns the network.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Account",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	Account string `json:"account"`
	// region of the network. Empty for providers whose networks are global.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Region",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	Region string `json:"region,omitempty"`
}

// AWSNetworkStatus is what only an AWS VPC has.
type AWSNetworkStatus struct {
	// isDefault is true for the default VPC of the region.
	// +optional
	IsDefault bool `json:"isDefault,omitempty"`
}

// GCPNetworkStatus is what only a GCP VPC network has.
type GCPNetworkStatus struct {
	// routingMode is the network's dynamic routing mode, REGIONAL or GLOBAL.
	// +optional
	RoutingMode string `json:"routingMode,omitempty"`
	// autoCreateSubnetworks is true for an auto mode network, which has a subnet in every
	// region from 10.128.0.0/9.
	// +optional
	AutoCreateSubnetworks bool `json:"autoCreateSubnetworks,omitempty"`
	// peerings are the network's VPC Network Peering connections, as Compute lists them.
	// +listType=atomic
	// +optional
	Peerings []GCPNetworkPeering `json:"peerings,omitempty"`
	// overlaps details overlapsWith: for other networks of the scope whose ranges overlap
	// this network's, whether the two are peered and which ranges overlap. A VPC network has
	// no range of its own, so the ranges compared are those of its subnetworks: primary,
	// secondary and IPv6. Every peered network is listed, and the first 50 others by name;
	// overlapsWith lists them all.
	// +listType=atomic
	// +optional
	Overlaps []GCPNetworkOverlap `json:"overlaps,omitempty"`
}

// GCPNetworkPeering is a VPC Network Peering connection of a GCP network.
type GCPNetworkPeering struct {
	// name is the peering's name.
	Name string `json:"name"`
	// network is the resource name of the peer network,
	// projects/<project>/global/networks/<name>.
	Network string `json:"network"`
	// state is ACTIVE once both networks have a peering with each other, INACTIVE while only
	// this one has.
	// +optional
	State string `json:"state,omitempty"`
	// stateDetails is Compute's explanation of the state.
	// +optional
	StateDetails string `json:"stateDetails,omitempty"`
}

// GCPNetworkOverlap is another network of the scope whose ranges overlap this network's.
type GCPNetworkOverlap struct {
	// network is the other network as overlapsWith names it, "<account>/<region>/<id>".
	Network string `json:"network"`
	// peered is true when either network has a peering with the other, active or not. A
	// peering between overlapping networks cannot become active, or stops exchanging the
	// overlapping routes.
	// +optional
	Peered bool `json:"peered,omitempty"`
	// peeringState is the state of this network's peering with the other one, or of the other
	// network's peering with this one when only that exists: ACTIVE or INACTIVE. Empty when the
	// networks are not peered.
	// +optional
	PeeringState string `json:"peeringState,omitempty"`
	// rangeCount is the number of pairs of overlapping ranges.
	// +optional
	RangeCount int32 `json:"rangeCount,omitempty"`
	// ranges are the first pairs of overlapping ranges, in address order, at most five.
	// +kubebuilder:validation:MaxItems=5
	// +listType=atomic
	// +optional
	Ranges []GCPOverlappingRange `json:"ranges,omitempty"`
}

// GCPOverlappingRange is a range of this network that overlaps a range of another network.
type GCPOverlappingRange struct {
	// cidrBlock is this network's range.
	CIDRBlock string `json:"cidrBlock"`
	// subnet is the resource name of this network's subnetwork the range belongs to. Empty for
	// the range of a legacy network, which has one of its own.
	// +optional
	Subnet string `json:"subnet,omitempty"`
	// otherCIDRBlock is the other network's range.
	OtherCIDRBlock string `json:"otherCIDRBlock"`
	// otherSubnet is the resource name of the other network's subnetwork the range belongs to.
	// +optional
	OtherSubnet string `json:"otherSubnet,omitempty"`
}

// NetworkStatus is the observed state of the network.
type NetworkStatus struct {
	// name is the network's name: the value of the Name tag on AWS, the resource's name on
	// GCP.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Name",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	Name string `json:"name,omitempty"`
	// state is the provider's state of the network.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="State",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	State string `json:"state,omitempty"`
	// cidrBlocks are the associated IPv4 CIDR blocks, primary first. Empty on GCP, where the
	// address ranges belong to the subnets.
	// +listType=atomic
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="CIDR Blocks"
	CIDRBlocks []string `json:"cidrBlocks,omitempty"`
	// ipv6CIDRBlocks are the associated IPv6 CIDR blocks.
	// +listType=atomic
	// +optional
	IPv6CIDRBlocks []string `json:"ipv6CIDRBlocks,omitempty"`
	// owner is read from the owner tag configured in the NetworkScope.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Owner",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	Owner string `json:"owner,omitempty"`
	// env is read from the env tag configured in the NetworkScope.
	// +optional
	Env string `json:"env,omitempty"`
	// tags are the network's tags at the provider.
	// +optional
	Tags map[string]string `json:"tags,omitempty"`
	// subnets is the number of discovered subnets.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Subnets",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:number"}
	Subnets int32 `json:"subnets,omitempty"`
	// totalIPs is the sum of usable IPv4 addresses in all subnets. Unset when unknown.
	// +optional
	TotalIPs *int64 `json:"totalIPs,omitempty"`
	// availableIPs is the sum of free IPv4 addresses in all subnets. Unset when the free
	// addresses of any subnet are unknown.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Free IPs",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:number"}
	AvailableIPs *int64 `json:"availableIPs,omitempty"`
	// overlapsWith lists networks in the same NetworkScope whose CIDRs overlap this one, as
	// "<account>/<region>/<id>". Overlaps break peering and transit routing. On GCP the ranges
	// compared are the subnetworks' (see gcp.overlaps).
	// +listType=atomic
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Overlaps With"
	OverlapsWith []string `json:"overlapsWith,omitempty"`
	// aws holds what only an AWS VPC has.
	// +optional
	AWS *AWSNetworkStatus `json:"aws,omitempty"`
	// gcp holds what only a GCP VPC network has.
	// +optional
	GCP *GCPNetworkStatus `json:"gcp,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=hsnet,categories=hypersurgery
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`
// +kubebuilder:printcolumn:name="Account",type=string,JSONPath=`.spec.account`
// +kubebuilder:printcolumn:name="Region",type=string,JSONPath=`.spec.region`
// +kubebuilder:printcolumn:name="Name",type=string,JSONPath=`.status.name`
// +kubebuilder:printcolumn:name="CIDR",type=string,JSONPath=`.status.cidrBlocks[0]`
// +kubebuilder:printcolumn:name="Owner",type=string,JSONPath=`.status.owner`
// +kubebuilder:printcolumn:name="Subnets",type=integer,JSONPath=`.status.subnets`
// +kubebuilder:printcolumn:name="Free IPs",type=integer,JSONPath=`.status.availableIPs`
// +kubebuilder:printcolumn:name="Env",type=string,JSONPath=`.status.env`,priority=1
// +kubebuilder:printcolumn:name="Overlaps",type=string,JSONPath=`.status.overlapsWith`,priority=1
// +operator-sdk:csv:customresourcedefinitions:displayName="Network"

// Network is a network discovered by a NetworkScope: an AWS VPC or a GCP VPC network. It is
// read-only for users. On AWS the object is named after the VPC ID; on GCP, whose network names
// repeat across projects, after the network's name and a hash of its resource name.
type Network struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec identifies the network
	// +required
	Spec NetworkSpec `json:"spec"`

	// status is the observed state of the network
	// +optional
	Status NetworkStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// NetworkList contains a list of Network
type NetworkList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Network `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Network{}, &NetworkList{})
		return nil
	})
}
