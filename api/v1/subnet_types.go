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

// SubnetSpec identifies the observed subnet. It is written by the operator, not by users.
type SubnetSpec struct {
	// provider is the provider of the scope that discovered the subnet.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Provider",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	Provider Provider `json:"provider"`
	// id is the provider's ID of the subnet: the subnet ID on AWS, the subnetwork's resource
	// name (projects/<project>/regions/<region>/subnetworks/<name>) on GCP, the subnet's
	// resource ID (<virtual network ID>/subnets/<name>) in lowercase on Azure.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="ID",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	ID string `json:"id"`
	// networkID is the ID of the network the subnet belongs to, as the network's spec.id.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Network ID",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	NetworkID string `json:"networkID"`
	// account is the account that owns the subnet.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Account",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	Account string `json:"account"`
	// region of the subnet.
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Region",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
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

// GCPSecondaryRange is a secondary IPv4 range of a GCP subnetwork, such as the Pod and
// Service ranges of a GKE cluster.
type GCPSecondaryRange struct {
	// name is the range's name within the subnetwork.
	Name string `json:"name"`
	// cidrBlock is the range.
	CIDRBlock string `json:"cidrBlock"`
	// totalIPs is the number of addresses in the range (a secondary range reserves none).
	// +optional
	TotalIPs *int64 `json:"totalIPs,omitempty"`
	// availableIPs is the number of free addresses in the range. Unset when unknown.
	// +optional
	AvailableIPs *int64 `json:"availableIPs,omitempty"`
}

// GCPSubnetStatus is what only a GCP subnetwork has.
type GCPSubnetStatus struct {
	// purpose is what the subnetwork is for: PRIVATE for ordinary subnets, or a special
	// purpose such as REGIONAL_MANAGED_PROXY or PRIVATE_SERVICE_CONNECT, whose addresses are
	// not for VMs.
	// +optional
	Purpose string `json:"purpose,omitempty"`
	// role is ACTIVE or BACKUP for a proxy-only subnetwork, empty otherwise.
	// +optional
	Role string `json:"role,omitempty"`
	// stackType is IPV4_ONLY, IPV4_IPV6 or IPV6_ONLY.
	// +optional
	StackType string `json:"stackType,omitempty"`
	// ipv6AccessType is INTERNAL or EXTERNAL for a subnetwork with IPv6.
	// +optional
	IPv6AccessType string `json:"ipv6AccessType,omitempty"`
	// privateIPGoogleAccess is true when VMs without external addresses can reach Google APIs.
	// +optional
	PrivateIPGoogleAccess bool `json:"privateIPGoogleAccess,omitempty"`
	// secondaryRanges are the subnetwork's secondary IPv4 ranges with their usage. Their
	// addresses are not counted in the subnet's totalIPs and availableIPs.
	// +listType=atomic
	// +optional
	SecondaryRanges []GCPSecondaryRange `json:"secondaryRanges,omitempty"`
}

// AzureSubnetStatus is what only an Azure subnet has.
type AzureSubnetStatus struct {
	// resourceGroup is the resource group of the subnet's virtual network, spelt as Azure
	// returned it.
	// +optional
	ResourceGroup string `json:"resourceGroup,omitempty"`
	// delegations are the services the subnet is delegated to, such as
	// Microsoft.Web/serverFarms.
	// +listType=atomic
	// +optional
	Delegations []string `json:"delegations,omitempty"`
	// serviceEndpoints are the services the subnet has service endpoints for, such as
	// Microsoft.Storage.
	// +listType=atomic
	// +optional
	ServiceEndpoints []string `json:"serviceEndpoints,omitempty"`
	// routeTableID is the resource ID of the route table associated with the subnet.
	// +optional
	RouteTableID string `json:"routeTableID,omitempty"`
	// networkSecurityGroupID is the resource ID of the network security group associated with
	// the subnet.
	// +optional
	NetworkSecurityGroupID string `json:"networkSecurityGroupID,omitempty"`
	// natGatewayID is the resource ID of the NAT gateway associated with the subnet.
	// +optional
	NATGatewayID string `json:"natGatewayID,omitempty"`
	// ipConfigurations is the number of IP configurations in the subnet: network interfaces,
	// private endpoints, internal load balancer frontends.
	// +optional
	IPConfigurations int32 `json:"ipConfigurations,omitempty"`
	// serviceManaged is true when a service manages addresses in the subnet: it is delegated,
	// or has service association or resource navigation links (App Service, SQL Managed
	// Instance and the like). Such services may use addresses that are not IP configurations,
	// so availableIPs is less certain.
	// +optional
	ServiceManaged bool `json:"serviceManaged,omitempty"`
	// ipUsageSource says where availableIPs came from: VirtualNetworkUsage (Azure's usage of the
	// virtual network, per subnet) or IPConfigurations (the subnet's usable addresses less its IP
	// configurations, when that usage did not cover the subnet). Empty while availableIPs is
	// unknown.
	// +optional
	IPUsageSource string `json:"ipUsageSource,omitempty"`
}

// SubnetStatus is the observed state of the subnet.
type SubnetStatus struct {
	// name is the subnet's name: the value of the Name tag on AWS, the resource's name on GCP
	// and Azure.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Name",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	Name string `json:"name,omitempty"`
	// state is the provider's state of the subnet.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="State",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	State string `json:"state,omitempty"`
	// cidrBlock is the IPv4 CIDR block (on GCP the primary range).
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="CIDR Block",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	CIDRBlock string `json:"cidrBlock,omitempty"`
	// secondaryCIDRBlocks are further IPv4 ranges of the subnet that are not its primary
	// block, such as the secondary ranges of a GCP subnetwork. They take addresses of the
	// network like cidrBlock does, but are not counted in totalIPs and availableIPs. On Azure
	// they are the subnet's further IPv4 address prefixes, which are: Azure places addresses in
	// any prefix of a subnet and reports one usage for all of them.
	// +listType=atomic
	// +optional
	SecondaryCIDRBlocks []string `json:"secondaryCIDRBlocks,omitempty"`
	// ipv6CIDRBlocks are the associated IPv6 CIDR blocks.
	// +listType=atomic
	// +optional
	IPv6CIDRBlocks []string `json:"ipv6CIDRBlocks,omitempty"`
	// zone is the zone of a zonal subnet, e.g. the AWS availability zone eu-central-1a. Empty
	// for providers whose subnets are regional.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Zone",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	Zone string `json:"zone,omitempty"`
	// totalIPs is the number of usable IPv4 addresses, as the provider counts them (AWS
	// reserves 5 per subnet, GCP 4 in the primary range, Azure 5 in each address prefix). Unset
	// when unknown.
	// +optional
	TotalIPs *int64 `json:"totalIPs,omitempty"`
	// availableIPs is the number of free IPv4 addresses. Unset when the provider could not
	// report it, which is not the same as zero.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Free IPs",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:number"}
	AvailableIPs *int64 `json:"availableIPs,omitempty"`
	// utilizationPercent is the share of used IPv4 addresses, 0-100. Unset when unknown.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Utilization (%)",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:number"}
	UtilizationPercent *int32 `json:"utilizationPercent,omitempty"`
	// owner is read from the owner tag configured in the NetworkScope.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Owner",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text"}
	Owner string `json:"owner,omitempty"`
	// env is read from the env tag configured in the NetworkScope.
	// +optional
	Env string `json:"env,omitempty"`
	// tier is read from the tier tag configured in the NetworkScope.
	// +optional
	Tier string `json:"tier,omitempty"`
	// ownershipSource says where owner, env and tier were read from: Subnet (the subnet's own
	// metadata) or Network (inherited from its network, on providers whose subnets cannot
	// carry metadata of their own).
	// +optional
	OwnershipSource OwnershipSource `json:"ownershipSource,omitempty"`
	// tags are the subnet's tags at the provider. Azure subnets cannot carry tags: there they
	// are the tags of the subnet's virtual network, with the subnet's own entry on it
	// (hs-subnet-<subnet name>) laid over them key by key.
	// +optional
	Tags map[string]string `json:"tags,omitempty"`
	// missingTags lists required tag keys that are absent or empty.
	// +listType=atomic
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Missing Tags"
	MissingTags []string `json:"missingTags,omitempty"`
	// aws holds what only an AWS subnet has.
	// +optional
	AWS *AWSSubnetStatus `json:"aws,omitempty"`
	// gcp holds what only a GCP subnetwork has.
	// +optional
	GCP *GCPSubnetStatus `json:"gcp,omitempty"`
	// azure holds what only an Azure subnet has.
	// +optional
	Azure *AzureSubnetStatus `json:"azure,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:storageversion
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
// +operator-sdk:csv:customresourcedefinitions:displayName="Subnet"

// Subnet is a subnet discovered by a NetworkScope. It is read-only for users. On AWS the
// object is named after the subnet ID; on GCP and Azure, whose subnet names repeat across
// networks, after the subnet's name and a hash of its resource name.
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
