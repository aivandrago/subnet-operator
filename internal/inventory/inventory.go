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

// Package inventory holds the cloud-neutral model of discovered networks. Providers (see
// internal/provider) fill it; controllers turn it into Kubernetes objects. Nothing here names a
// cloud: what only one provider has travels in that provider's member (AWS), typed with the
// API's own provider structs so the controllers can copy it into the status unread.
package inventory

import (
	"context"
	"errors"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
)

// Target is one account/region pair to discover or write to.
type Target struct {
	// Provider is the cloud of the target: the provider of the scope it was expanded from.
	Provider networkv1.Provider
	// Scope is the NetworkScope the target was expanded from. Discovery does not depend on
	// it; it only labels what a provider reports about the target, such as throttled calls.
	Scope   string
	Account string
	Region  string
	// Identity is how the provider reaches the account: a value of the provider's own type
	// (on AWS the role to assume). Nil, like an identity whose Own reports true, means the
	// operator's own identity.
	Identity Identity
	// NetworkSelector limits discovery to networks carrying all of these tags; an empty
	// value matches any value of the key.
	NetworkSelector map[string]string
	// DiscoverUnmanaged also reports the networks the selector does not match, and their
	// subnets, so nothing in the account stays invisible just because nobody tagged it.
	DiscoverUnmanaged bool
}

// Identity is a provider's reference to the credentials it uses for one account: an AWS role
// to assume, a GCP service account to impersonate, an Azure client ID. It never holds a
// secret.
type Identity interface {
	// Own reports whether this is the operator's own identity (EKS Pod Identity or IRSA on
	// AWS), which needs nothing to be assumed.
	Own() bool
	// String names the identity for messages and logs.
	String() string
}

// OwnIdentity reports whether the target is reached with the operator's own identity.
func (t Target) OwnIdentity() bool {
	return t.Identity == nil || t.Identity.Own()
}

// Network is a discovered network: an AWS VPC, a GCP VPC network, an Azure VNet.
type Network struct {
	ID             string
	Account        string
	Region         string
	State          string
	CIDRBlocks     []string
	IPv6CIDRBlocks []string
	// Tags are the network's decoded ownership and other metadata.
	Tags map[string]string

	AWS *networkv1.AWSNetworkStatus
}

// Subnet is a discovered subnet.
type Subnet struct {
	ID             string
	NetworkID      string
	Account        string
	Region         string
	State          string
	CIDRBlock      string
	IPv6CIDRBlocks []string
	// Zone is set on providers whose subnets are zonal.
	Zone string
	// TotalIPs and AvailableIPs are the usable and the free IPv4 addresses. Nil means the
	// provider does not know, which is not the same as none.
	TotalIPs     *int64
	AvailableIPs *int64
	// OwnershipSource says whether Tags carry the subnet's own ownership metadata or its
	// network's.
	OwnershipSource networkv1.OwnershipSource
	// Tags are the subnet's decoded ownership and other metadata.
	Tags map[string]string

	AWS *networkv1.AWSSubnetStatus
}

// Snapshot is the result of discovering one target.
type Snapshot struct {
	Networks []Network
	Subnets  []Subnet
	// UnmanagedNetworks and UnmanagedSubnets are what the selector left out. They are
	// reported and counted, never mirrored as objects: the operator does not manage them.
	UnmanagedNetworks []Network
	UnmanagedSubnets  []Subnet
}

// Discoverer reads the networks of one target. Implementations must be read-only.
type Discoverer interface {
	// Discover returns an error wrapping ErrThrottled when the provider kept rate-limiting
	// the target after its own retries, so callers can back off instead of reporting the
	// target unreachable.
	Discover(ctx context.Context, target Target) (*Snapshot, error)
}

// ErrThrottled means the cloud API rate-limited the target: it is reachable, but busy.
var ErrThrottled = errors.New("throttled by the cloud API")

// TargetKey identifies an account/region pair. The provider is not part of it: account IDs of
// different providers cannot be confused (12 digits on AWS, a project ID starting with a
// letter on GCP, a UUID on Azure), and the key is what change events carry.
type TargetKey struct {
	Account string
	Region  string
}

// Key returns the target's account/region pair.
func (t Target) Key() TargetKey {
	return TargetKey{Account: t.Account, Region: t.Region}
}

// String formats the pair as "account/region".
func (k TargetKey) String() string {
	return k.Account + "/" + k.Region
}

// CreateSubnetRequest describes one subnet to create.
type CreateSubnetRequest struct {
	NetworkID string
	CIDRBlock string
	// Zone is set on providers whose subnets are zonal.
	Zone string
	// Tags are the ownership and other metadata the subnet is created with.
	Tags map[string]string

	AWS *networkv1.AWSClaimOptions
}

// SubnetWriter creates subnets. Implementations must never delete anything.
type SubnetWriter interface {
	// CreateSubnet creates the subnet in the target and returns its ID. The target's Identity
	// is the write identity. ErrCIDRConflict is returned when the CIDR is already taken or
	// outside the network; ErrNotSupported when the provider cannot create subnets.
	CreateSubnet(ctx context.Context, target Target, req CreateSubnetRequest) (string, error)
}

// ErrCIDRConflict means the CIDR overlaps an existing subnet: allocate another one.
var ErrCIDRConflict = errors.New("cidr conflicts with an existing subnet")

// ErrNotSupported means the provider cannot do what was asked.
var ErrNotSupported = errors.New("not supported by the provider")

// OwnershipWriter records ownership metadata for an existing network or subnet. Where it goes
// is the provider's business (ADR 0002 §6): on the resource itself, or on its parent network.
// Implementations must only ever add or overwrite the keys they are given: removing metadata
// somebody else set is not the operator's call.
type OwnershipWriter interface {
	// WriteOwnership records the tags for the network or subnet in the target. The target's
	// Identity is the write identity.
	WriteOwnership(ctx context.Context, target Target, resourceID string, tags map[string]string) error
}

// Creation is a resource somebody just made, and the principal who made it, as a provider's
// change events report it. The provider's audit trail is the only place that knows the
// latter, which is why the operator reads it from the events and not by asking the API later.
type Creation struct {
	Target     TargetKey
	ResourceID string
	Principal  string
	EventName  string
}
