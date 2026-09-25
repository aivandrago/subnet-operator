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

package aws

import (
	"context"
	"fmt"
	"maps"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/provider"
	"hypersurgery.dev/subnet-operator/internal/provider/providertest"
)

// The AWS provider runs the provider contract (#45) against memoryEC2: the whole provider, from
// the Discoverer's credential checks through DiscoverTarget, CreateSubnet and ApplyTags, with
// only the EC2 client replaced. test/e2e runs the same contract against Moto.
func TestProviderContract(t *testing.T) {
	providertest.Run(t, func(t *testing.T) providertest.Fixture {
		return newMemoryFixture()
	})
}

const contractAccount = "111111111111"

type memoryFixture struct {
	ec2      *memoryEC2
	provider *Provider
}

func newMemoryFixture() *memoryFixture {
	cloud := &memoryEC2{vpcs: map[string]*memoryVPC{}, subnets: map[string]*memorySubnet{}}
	d := newDiscoverer(aws.Config{Region: "eu-central-1"})
	d.ownAccount = contractAccount
	d.testEC2 = func(inventory.Target) ec2Client { return cloud }
	return &memoryFixture{ec2: cloud, provider: NewProvider(Clients{Discoverer: d, SubnetWriter: d, OwnershipWriter: d}, nil)}
}

func (f *memoryFixture) Provider() provider.Provider { return f.provider }

func (f *memoryFixture) Target() inventory.Target {
	return inventory.Target{Provider: networkv1beta1.ProviderAWS, Scope: "contract", Account: contractAccount,
		Region: "eu-central-1"}
}

func (f *memoryFixture) Zone() string { return "eu-central-1a" }

func (f *memoryFixture) CreateNetwork(t providertest.T, cidr string, tags map[string]string) string {
	f.ec2.mu.Lock()
	defer f.ec2.mu.Unlock()
	f.ec2.next++
	id := fmt.Sprintf("vpc-%08x", f.ec2.next)
	f.ec2.vpcs[id] = &memoryVPC{cidr: cidr, tags: clone(tags)}
	return id
}

func (f *memoryFixture) CreateSubnet(t providertest.T, networkID, cidr string, tags map[string]string) string {
	t.Helper()
	out, err := f.ec2.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId: aws.String(networkID), CidrBlock: aws.String(cidr), AvailabilityZone: aws.String(f.Zone()),
		TagSpecifications: tagSpecs(ec2types.ResourceTypeSubnet, tags),
	})
	if err != nil {
		t.Fatalf("create subnet: %v", err)
	}
	return aws.ToString(out.Subnet.SubnetId)
}

func (f *memoryFixture) MakeIPUsageUnknown(_ providertest.T, subnetID string) bool {
	f.ec2.mu.Lock()
	defer f.ec2.mu.Unlock()
	f.ec2.subnets[subnetID].usageUnknown = true
	return true
}

func (f *memoryFixture) Fail(_ providertest.T, failure providertest.Failure) (func(), bool) {
	code := map[providertest.Failure]string{
		providertest.Throttled:    "RequestLimitExceeded",
		providertest.AccessDenied: "UnauthorizedOperation",
	}[failure]
	f.ec2.mu.Lock()
	defer f.ec2.mu.Unlock()
	f.ec2.failWith = code
	return func() {
		f.ec2.mu.Lock()
		defer f.ec2.mu.Unlock()
		f.ec2.failWith = ""
	}, true
}

func (f *memoryFixture) InvalidTags() []map[string]string {
	return []map[string]string{
		{"aws:cloudformation:stack-name": "mine"},
		{"": "empty key"},
		{strings.Repeat("k", maxTagKeyLength+1): "long key"},
		{"hs/owner": strings.Repeat("v", maxTagValueLength+1)},
	}
}

// memoryEC2 is EC2 in memory: VPCs and subnets with tags, the tag and vpc-id filters, the
// conflict checks of CreateSubnet, and failures on demand.
type memoryEC2 struct {
	mu       sync.Mutex
	next     int
	vpcs     map[string]*memoryVPC
	subnets  map[string]*memorySubnet
	failWith string
}

type memoryVPC struct {
	cidr string
	tags map[string]string
}

type memorySubnet struct {
	vpc, cidr, zone string
	tags            map[string]string
	usageUnknown    bool
}

func clone(m map[string]string) map[string]string {
	out := map[string]string{}
	maps.Copy(out, m)
	return out
}

func (e *memoryEC2) fail() error {
	if e.failWith == "" {
		return nil
	}
	return &smithy.GenericAPIError{Code: e.failWith, Message: "simulated"}
}

func ec2Tags(m map[string]string) []ec2types.Tag {
	out := make([]ec2types.Tag, 0, len(m))
	for _, k := range sortedKeys(m) {
		out = append(out, ec2types.Tag{Key: aws.String(k), Value: aws.String(m[k])})
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// matches applies EC2's filters: tag:<key> with values, tag-key, vpc-id.
func matches(filters []ec2types.Filter, vpcID string, tags map[string]string) bool {
	for _, f := range filters {
		name := aws.ToString(f.Name)
		switch {
		case name == "vpc-id":
			if !slices.Contains(f.Values, vpcID) {
				return false
			}
		case name == "tag-key":
			if !slices.ContainsFunc(f.Values, func(k string) bool { _, ok := tags[k]; return ok }) {
				return false
			}
		case strings.HasPrefix(name, "tag:"):
			v, ok := tags[strings.TrimPrefix(name, "tag:")]
			if !ok || !slices.Contains(f.Values, v) {
				return false
			}
		default:
			panic("memoryEC2 does not know filter " + name)
		}
	}
	return true
}

func (e *memoryEC2) DescribeVpcs(_ context.Context, in *ec2.DescribeVpcsInput, _ ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return nil, err
	}
	out := &ec2.DescribeVpcsOutput{}
	for _, id := range sortedIDs(e.vpcs) {
		v := e.vpcs[id]
		if !matches(in.Filters, id, v.tags) {
			continue
		}
		out.Vpcs = append(out.Vpcs, ec2types.Vpc{VpcId: aws.String(id), CidrBlock: aws.String(v.cidr),
			State: ec2types.VpcStateAvailable, IsDefault: aws.Bool(false), Tags: ec2Tags(v.tags)})
	}
	return out, nil
}

func (e *memoryEC2) DescribeSubnets(_ context.Context, in *ec2.DescribeSubnetsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return nil, err
	}
	out := &ec2.DescribeSubnetsOutput{}
	for _, id := range sortedIDs(e.subnets) {
		s := e.subnets[id]
		if !matches(in.Filters, s.vpc, s.tags) {
			continue
		}
		sn := ec2types.Subnet{SubnetId: aws.String(id), VpcId: aws.String(s.vpc), CidrBlock: aws.String(s.cidr),
			AvailabilityZone: aws.String(s.zone), AvailabilityZoneId: aws.String("euc1-az2"),
			State: ec2types.SubnetStateAvailable, Tags: ec2Tags(s.tags)}
		if !s.usageUnknown {
			sn.AvailableIpAddressCount = aws.Int32(int32(inventory.UsableIPv4(s.cidr, ReservedIPs)))
		}
		out.Subnets = append(out.Subnets, sn)
	}
	return out, nil
}

func (e *memoryEC2) DescribeRouteTables(context.Context, *ec2.DescribeRouteTablesInput, ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return nil, err
	}
	return &ec2.DescribeRouteTablesOutput{}, nil
}

func (e *memoryEC2) CreateSubnet(_ context.Context, in *ec2.CreateSubnetInput, _ ...func(*ec2.Options)) (*ec2.CreateSubnetOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return nil, err
	}
	vpcID, cidr := aws.ToString(in.VpcId), aws.ToString(in.CidrBlock)
	vpc, ok := e.vpcs[vpcID]
	if !ok {
		return nil, &smithy.GenericAPIError{Code: "InvalidVpcID.NotFound", Message: vpcID}
	}
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return nil, &smithy.GenericAPIError{Code: "InvalidParameterValue", Message: cidr}
	}
	if outer := netip.MustParsePrefix(vpc.cidr); !outer.Contains(prefix.Addr()) || prefix.Bits() < outer.Bits() {
		return nil, &smithy.GenericAPIError{Code: "InvalidSubnet.Range", Message: cidr + " is not within " + vpc.cidr}
	}
	for _, s := range e.subnets {
		if s.vpc == vpcID && netip.MustParsePrefix(s.cidr).Overlaps(prefix) {
			return nil, &smithy.GenericAPIError{Code: "InvalidSubnet.Conflict", Message: cidr + " conflicts with " + s.cidr}
		}
	}
	e.next++
	id := fmt.Sprintf("subnet-%08x", e.next)
	tags := map[string]string{}
	for _, spec := range in.TagSpecifications {
		for _, tag := range spec.Tags {
			tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
		}
	}
	e.subnets[id] = &memorySubnet{vpc: vpcID, cidr: cidr, zone: aws.ToString(in.AvailabilityZone), tags: tags}
	return &ec2.CreateSubnetOutput{Subnet: &ec2types.Subnet{SubnetId: aws.String(id)}}, nil
}

func (e *memoryEC2) ModifySubnetAttribute(context.Context, *ec2.ModifySubnetAttributeInput, ...func(*ec2.Options)) (*ec2.ModifySubnetAttributeOutput, error) {
	return &ec2.ModifySubnetAttributeOutput{}, nil
}

func (e *memoryEC2) AssociateRouteTable(context.Context, *ec2.AssociateRouteTableInput, ...func(*ec2.Options)) (*ec2.AssociateRouteTableOutput, error) {
	return &ec2.AssociateRouteTableOutput{}, nil
}

// CreateTags adds and overwrites the tags it names and leaves every other tag alone.
func (e *memoryEC2) CreateTags(_ context.Context, in *ec2.CreateTagsInput, _ ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := e.fail(); err != nil {
		return nil, err
	}
	for _, id := range in.Resources {
		var tags map[string]string
		switch {
		case e.vpcs[id] != nil:
			tags = e.vpcs[id].tags
		case e.subnets[id] != nil:
			tags = e.subnets[id].tags
		default:
			return nil, &smithy.GenericAPIError{Code: "InvalidID", Message: id}
		}
		for _, tag := range in.Tags {
			tags[aws.ToString(tag.Key)] = aws.ToString(tag.Value)
		}
	}
	return &ec2.CreateTagsOutput{}, nil
}

func sortedIDs[V any](m map[string]V) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
