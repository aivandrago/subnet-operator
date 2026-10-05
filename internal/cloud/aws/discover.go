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

// Package aws discovers VPCs and subnets with the EC2 API. It only calls Describe* APIs.
package aws

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// ReservedIPs is the number of addresses AWS reserves in every IPv4 subnet.
const ReservedIPs = 5

// EC2API is the subset of the EC2 client used for discovery.
type EC2API interface {
	ec2.DescribeVpcsAPIClient
	ec2.DescribeSubnetsAPIClient
	ec2.DescribeRouteTablesAPIClient
}

// maxFilterValues keeps vpc-id filters well under the EC2 per-filter value limit.
const maxFilterValues = 100

// DiscoverTarget reads the VPCs matching the target's network selector, all their subnets,
// and their route tables (to tell public subnets from private ones).
//
// With DiscoverUnmanaged the VPCs the selector leaves out are read in the same call and
// reported separately, together with their subnets: one DescribeVpcs either way, because
// the selector is cheap to apply in Go and a second round trip is not.
func DiscoverTarget(ctx context.Context, api EC2API, target inventory.Target) (*inventory.Snapshot, error) {
	unmanagedWanted := target.DiscoverUnmanaged && len(target.NetworkSelector) > 0

	listFilters := tagFilters(target.NetworkSelector)
	if unmanagedWanted {
		listFilters = nil // ask for everything, then split
	}
	all, err := describeVPCs(ctx, api, target, listFilters)
	if err != nil {
		return nil, fmt.Errorf("describe VPCs: %w", err)
	}

	vpcs := all
	var unmanagedVPCs []inventory.Network
	if unmanagedWanted {
		vpcs = nil
		for _, v := range all {
			if matchesSelector(v.Tags, target.NetworkSelector) {
				vpcs = append(vpcs, v)
			} else {
				unmanagedVPCs = append(unmanagedVPCs, v)
			}
		}
	}

	snap := &inventory.Snapshot{Networks: vpcs, UnmanagedNetworks: unmanagedVPCs}
	if len(unmanagedVPCs) > 0 {
		subnets, err := describeSubnetsOfVPCs(ctx, api, target, unmanagedVPCs)
		if err != nil {
			return nil, fmt.Errorf("describe unmanaged subnets: %w", err)
		}
		snap.UnmanagedSubnets = subnets
	}
	if len(vpcs) == 0 {
		return snap, nil
	}

	// Without a selector every VPC is in scope, so no vpc-id filter is needed.
	var vpcIDChunks [][]string
	if len(target.NetworkSelector) == 0 {
		vpcIDChunks = [][]string{nil}
	} else {
		ids := make([]string, 0, len(vpcs))
		for _, v := range vpcs {
			ids = append(ids, v.ID)
		}
		vpcIDChunks = slices.Collect(slices.Chunk(ids, maxFilterValues))
	}

	routes := map[string]routeInfo{}
	mainRoutes := map[string]routeInfo{}
	for _, ids := range vpcIDChunks {
		if err := describeRouteTables(ctx, api, ids, routes, mainRoutes); err != nil {
			return nil, fmt.Errorf("describe route tables: %w", err)
		}
	}
	for _, ids := range vpcIDChunks {
		subnets, err := describeSubnets(ctx, api, target, ids, routes, mainRoutes)
		if err != nil {
			return nil, fmt.Errorf("describe subnets: %w", err)
		}
		snap.Subnets = append(snap.Subnets, subnets...)
	}
	return snap, nil
}

func tagFilters(selector map[string]string) []ec2types.Filter {
	keys := make([]string, 0, len(selector))
	for k := range selector {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	filters := make([]ec2types.Filter, 0, len(keys))
	for _, k := range keys {
		if v := selector[k]; v != "" {
			filters = append(filters, ec2types.Filter{Name: aws.String("tag:" + k), Values: []string{v}})
		} else {
			filters = append(filters, ec2types.Filter{Name: aws.String("tag-key"), Values: []string{k}})
		}
	}
	return filters
}

func vpcIDFilter(ids []string) []ec2types.Filter {
	if len(ids) == 0 {
		return nil
	}
	return []ec2types.Filter{{Name: aws.String("vpc-id"), Values: ids}}
}

// matchesSelector reports whether the tags satisfy every entry of the selector. An empty
// value matches any value of the key, mirroring what the EC2 tag filters do server side.
func matchesSelector(tags, selector map[string]string) bool {
	for k, want := range selector {
		got, ok := tags[k]
		if !ok || (want != "" && got != want) {
			return false
		}
	}
	return true
}

// describeSubnetsOfVPCs lists the subnets of the given VPCs. Route tables are not read: an
// unmanaged subnet is only counted and offered for import, so public or private does not
// matter until somebody takes it under management.
func describeSubnetsOfVPCs(ctx context.Context, api EC2API, target inventory.Target, vpcs []inventory.Network) ([]inventory.Subnet, error) {
	ids := make([]string, 0, len(vpcs))
	for _, v := range vpcs {
		ids = append(ids, v.ID)
	}
	var out []inventory.Subnet
	for chunk := range slices.Chunk(ids, maxFilterValues) {
		subnets, err := describeSubnets(ctx, api, target, chunk, nil, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, subnets...)
	}
	return out, nil
}

func describeVPCs(ctx context.Context, api EC2API, target inventory.Target, filters []ec2types.Filter) ([]inventory.Network, error) {
	var out []inventory.Network
	p := ec2.NewDescribeVpcsPaginator(api, &ec2.DescribeVpcsInput{Filters: filters})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, v := range page.Vpcs {
			vpc := inventory.Network{
				ID:      aws.ToString(v.VpcId),
				Account: target.Account,
				Region:  target.Region,
				State:   string(v.State),
				Tags:    tagMap(v.Tags),
				AWS:     &networkv1.AWSNetworkStatus{IsDefault: aws.ToBool(v.IsDefault)},
			}
			// The primary CIDR first, then additional associated blocks.
			if c := aws.ToString(v.CidrBlock); c != "" {
				vpc.CIDRBlocks = append(vpc.CIDRBlocks, c)
			}
			for _, a := range v.CidrBlockAssociationSet {
				c := aws.ToString(a.CidrBlock)
				if a.CidrBlockState == nil || !isAssociated(string(a.CidrBlockState.State)) || slices.Contains(vpc.CIDRBlocks, c) {
					continue
				}
				vpc.CIDRBlocks = append(vpc.CIDRBlocks, c)
			}
			for _, a := range v.Ipv6CidrBlockAssociationSet {
				if a.Ipv6CidrBlockState == nil || !isAssociated(string(a.Ipv6CidrBlockState.State)) {
					continue
				}
				vpc.IPv6CIDRBlocks = append(vpc.IPv6CIDRBlocks, aws.ToString(a.Ipv6CidrBlock))
			}
			out = append(out, vpc)
		}
	}
	return out, nil
}

func isAssociated(state string) bool {
	return state == "associated"
}

type routeInfo struct {
	tableID string
	public  bool
}

func describeRouteTables(ctx context.Context, api EC2API, vpcIDs []string, bySubnet, byVPCMain map[string]routeInfo) error {
	p := ec2.NewDescribeRouteTablesPaginator(api, &ec2.DescribeRouteTablesInput{Filters: vpcIDFilter(vpcIDs)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, rt := range page.RouteTables {
			info := routeInfo{tableID: aws.ToString(rt.RouteTableId), public: hasInternetGatewayDefaultRoute(rt.Routes)}
			for _, a := range rt.Associations {
				if aws.ToBool(a.Main) {
					byVPCMain[aws.ToString(rt.VpcId)] = info
				}
				if id := aws.ToString(a.SubnetId); id != "" {
					bySubnet[id] = info
				}
			}
		}
	}
	return nil
}

func hasInternetGatewayDefaultRoute(routes []ec2types.Route) bool {
	for _, r := range routes {
		if !strings.HasPrefix(aws.ToString(r.GatewayId), "igw-") || r.State == ec2types.RouteStateBlackhole {
			continue
		}
		if aws.ToString(r.DestinationCidrBlock) == "0.0.0.0/0" || aws.ToString(r.DestinationIpv6CidrBlock) == "::/0" {
			return true
		}
	}
	return false
}

func describeSubnets(ctx context.Context, api EC2API, target inventory.Target, vpcIDs []string,
	routes, mainRoutes map[string]routeInfo) ([]inventory.Subnet, error) {
	var out []inventory.Subnet
	p := ec2.NewDescribeSubnetsPaginator(api, &ec2.DescribeSubnetsInput{Filters: vpcIDFilter(vpcIDs)})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, s := range page.Subnets {
			sn := inventory.Subnet{
				ID:        aws.ToString(s.SubnetId),
				NetworkID: aws.ToString(s.VpcId),
				Account:   target.Account,
				Region:    target.Region,
				State:     string(s.State),
				CIDRBlock: aws.ToString(s.CidrBlock),
				Zone:      aws.ToString(s.AvailabilityZone),
				TotalIPs:  new(inventory.UsableIPv4(aws.ToString(s.CidrBlock), ReservedIPs)),
				// A subnet's tags are its own on AWS: nothing is inherited from the VPC.
				OwnershipSource: networkv1.OwnershipSourceSubnet,
				Tags:            tagMap(s.Tags),
				AWS:             &networkv1.AWSSubnetStatus{AvailabilityZoneID: aws.ToString(s.AvailabilityZoneId)},
			}
			// A count EC2 left out is unknown, not zero: zero would read as a full subnet.
			if s.AvailableIpAddressCount != nil {
				sn.AvailableIPs = new(int64(*s.AvailableIpAddressCount))
			}
			for _, a := range s.Ipv6CidrBlockAssociationSet {
				if a.Ipv6CidrBlockState == nil || !isAssociated(string(a.Ipv6CidrBlockState.State)) {
					continue
				}
				sn.IPv6CIDRBlocks = append(sn.IPv6CIDRBlocks, aws.ToString(a.Ipv6CidrBlock))
			}
			// A subnet without an explicit association uses the VPC main route table.
			info, ok := routes[sn.ID]
			if !ok {
				info = mainRoutes[sn.NetworkID]
			}
			sn.AWS.RouteTableID, sn.AWS.Public = info.tableID, info.public
			out = append(out, sn)
		}
	}
	return out, nil
}

func tagMap(tags []ec2types.Tag) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	m := make(map[string]string, len(tags))
	for _, t := range tags {
		m[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return m
}
