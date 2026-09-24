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

// Package inventory holds the cloud-neutral model of discovered networks.
// Cloud providers fill it; controllers turn it into Kubernetes objects.
package inventory

import (
	"context"
	"errors"
)

// Target is one account/region pair to discover.
type Target struct {
	// Scope is the NetworkScope the target was expanded from. Discovery does not depend on
	// it; it only labels what a provider reports about the target, such as throttled calls.
	Scope      string
	Account    string
	Region     string
	RoleARN    string
	ExternalID string
	// VPCTagSelector limits discovery to VPCs carrying all of these tags;
	// an empty value matches any value of the key.
	VPCTagSelector map[string]string
	// DiscoverUnmanaged also reports the VPCs the selector does not match, and their
	// subnets, so nothing in the account stays invisible just because nobody tagged it.
	DiscoverUnmanaged bool
}

// VPC is a discovered virtual network.
type VPC struct {
	ID             string
	Account        string
	Region         string
	State          string
	IsDefault      bool
	CIDRBlocks     []string
	IPv6CIDRBlocks []string
	Tags           map[string]string
}

// Subnet is a discovered subnet.
type Subnet struct {
	ID                 string
	VPCID              string
	Account            string
	Region             string
	State              string
	CIDRBlock          string
	IPv6CIDRBlocks     []string
	AvailabilityZone   string
	AvailabilityZoneID string
	AvailableIPs       int64
	Public             bool
	RouteTableID       string
	Tags               map[string]string
}

// Snapshot is the result of discovering one target.
type Snapshot struct {
	VPCs    []VPC
	Subnets []Subnet
	// UnmanagedVPCs and UnmanagedSubnets are what the tag selector left out. They are
	// reported and counted, never mirrored as objects: the operator does not manage them.
	UnmanagedVPCs    []VPC
	UnmanagedSubnets []Subnet
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

// TargetKey identifies an account/region pair.
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
	VPCID               string
	CIDRBlock           string
	AvailabilityZone    string
	RouteTableID        string
	MapPublicIPOnLaunch bool
	Tags                map[string]string
}

// SubnetWriter creates subnets. Implementations must never delete anything.
type SubnetWriter interface {
	// CreateSubnet creates the subnet in the target and returns its ID. The target's RoleARN
	// is the write role. ErrCIDRConflict is returned when the CIDR is already taken.
	CreateSubnet(ctx context.Context, target Target, req CreateSubnetRequest) (string, error)
}

// ErrCIDRConflict means the CIDR overlaps an existing subnet: allocate another one.
var ErrCIDRConflict = errors.New("cidr conflicts with an existing subnet")

// TagWriter adds tags to an existing resource. Implementations must only ever add: removing
// a tag somebody else set is not the operator's call.
type TagWriter interface {
	// ApplyTags tags the VPC or subnet in the target. The target's RoleARN is the write role.
	ApplyTags(ctx context.Context, target Target, resourceID string, tags map[string]string) error
}
