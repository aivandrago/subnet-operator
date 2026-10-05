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

// Capability is something a provider can do, as the operator runs it (ADR 0002 §8). The
// dashboard and the webhooks read it from NetworkScope.status.capabilities instead of guessing
// from the provider's name. More values may be added; a client must ignore values it does not
// know.
type Capability string

const (
	// CapabilityCreateSubnet: SubnetClaims in Create mode can create subnets. Whether writes are
	// switched on (--enable-writes) is a separate, operator-wide setting.
	CapabilityCreateSubnet Capability = "CreateSubnet"
	// CapabilityChangeEvents: the operator receives the provider's change events, so changes
	// show up within seconds instead of at the next resync.
	CapabilityChangeEvents Capability = "ChangeEvents"
	// CapabilityIPUsage: the provider reports free addresses per subnet. Individual subnets may
	// still report them as unknown.
	CapabilityIPUsage Capability = "IPUsage"
)

// OwnershipModel says where a provider keeps the ownership metadata (owner, env, tier, the
// managed marker) of a kind of resource (ADR 0002 §6). More values may be added.
type OwnershipModel string

const (
	// OwnershipResourceTags keeps the metadata on the resource itself: AWS tags, Azure tags on
	// a VNet, GCP Resource Manager tag bindings.
	OwnershipResourceTags OwnershipModel = "ResourceTags"
	// OwnershipParentNetworkTags keeps the metadata of a subnet on its network, one entry per
	// subnet: Azure subnets, which cannot carry tags.
	OwnershipParentNetworkTags OwnershipModel = "ParentNetworkTags"
)

// Ownership is the ownership model of a provider, per kind of resource.
type Ownership struct {
	// networks is where the metadata of a network is kept.
	// +optional
	Networks OwnershipModel `json:"networks,omitempty"`
	// subnets is where the metadata of a subnet is kept.
	// +optional
	Subnets OwnershipModel `json:"subnets,omitempty"`
}

// OwnershipSource says where the ownership values of one subnet were read from.
type OwnershipSource string

const (
	// OwnershipSourceSubnet: the subnet's own metadata (on the subnet, or its own entry on the
	// parent network).
	OwnershipSourceSubnet OwnershipSource = "Subnet"
	// OwnershipSourceNetwork: the subnet has no metadata of its own and inherits its network's.
	OwnershipSourceNetwork OwnershipSource = "Network"
)
