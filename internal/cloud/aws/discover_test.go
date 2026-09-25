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
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// fakeEC2 serves fixed pages and records the filters it was called with.
type fakeEC2 struct {
	vpcPages    [][]ec2types.Vpc
	subnets     []ec2types.Subnet
	routeTables []ec2types.RouteTable
	// subnetsByVPC, when set, answers DescribeSubnets per vpc-id filter instead of
	// returning the same list to everyone.
	subnetsByVPC map[string][]ec2types.Subnet

	vpcFilters    []ec2types.Filter
	subnetFilters [][]ec2types.Filter
	vpcCalls      int
}

func (f *fakeEC2) DescribeVpcs(_ context.Context, in *ec2.DescribeVpcsInput, _ ...func(*ec2.Options)) (*ec2.DescribeVpcsOutput, error) {
	f.vpcFilters = in.Filters
	f.vpcCalls++
	page := 0
	if in.NextToken != nil {
		page = 1
	}
	out := &ec2.DescribeVpcsOutput{Vpcs: f.vpcPages[page]}
	if page+1 < len(f.vpcPages) {
		out.NextToken = aws.String("next")
	}
	return out, nil
}

func (f *fakeEC2) DescribeSubnets(_ context.Context, in *ec2.DescribeSubnetsInput, _ ...func(*ec2.Options)) (*ec2.DescribeSubnetsOutput, error) {
	f.subnetFilters = append(f.subnetFilters, in.Filters)
	if f.subnetsByVPC == nil {
		return &ec2.DescribeSubnetsOutput{Subnets: f.subnets}, nil
	}
	var out []ec2types.Subnet
	for _, filter := range in.Filters {
		if aws.ToString(filter.Name) != "vpc-id" {
			continue
		}
		for _, id := range filter.Values {
			out = append(out, f.subnetsByVPC[id]...)
		}
	}
	return &ec2.DescribeSubnetsOutput{Subnets: out}, nil
}

func (f *fakeEC2) DescribeRouteTables(context.Context, *ec2.DescribeRouteTablesInput, ...func(*ec2.Options)) (*ec2.DescribeRouteTablesOutput, error) {
	return &ec2.DescribeRouteTablesOutput{RouteTables: f.routeTables}, nil
}

func associated() *ec2types.VpcCidrBlockState {
	return &ec2types.VpcCidrBlockState{State: ec2types.VpcCidrBlockStateCodeAssociated}
}

func TestDiscoverTarget(t *testing.T) {
	api := &fakeEC2{
		vpcPages: [][]ec2types.Vpc{
			{{
				VpcId:     aws.String("vpc-a"),
				CidrBlock: aws.String("10.0.0.0/16"),
				State:     ec2types.VpcStateAvailable,
				CidrBlockAssociationSet: []ec2types.VpcCidrBlockAssociation{
					{CidrBlock: aws.String("10.0.0.0/16"), CidrBlockState: associated()},
					{CidrBlock: aws.String("100.64.0.0/16"), CidrBlockState: associated()},
					{CidrBlock: aws.String("100.65.0.0/16"), CidrBlockState: &ec2types.VpcCidrBlockState{State: ec2types.VpcCidrBlockStateCodeDisassociated}},
				},
				Tags: []ec2types.Tag{{Key: aws.String("hs/managed"), Value: aws.String("true")}},
			}},
			{{VpcId: aws.String("vpc-b"), CidrBlock: aws.String("10.1.0.0/16"), IsDefault: aws.Bool(true)}},
		},
		subnets: []ec2types.Subnet{
			{SubnetId: aws.String("subnet-public"), VpcId: aws.String("vpc-a"), CidrBlock: aws.String("10.0.0.0/24"),
				AvailabilityZone: aws.String("eu-central-1a"), AvailabilityZoneId: aws.String("euc1-az2"),
				AvailableIpAddressCount: aws.Int32(200),
				Tags:                    []ec2types.Tag{{Key: aws.String("hs/owner"), Value: aws.String("team-a")}}},
			{SubnetId: aws.String("subnet-private"), VpcId: aws.String("vpc-a"), CidrBlock: aws.String("10.0.1.0/24"),
				AvailableIpAddressCount: aws.Int32(10)},
		},
		routeTables: []ec2types.RouteTable{
			{
				RouteTableId: aws.String("rtb-main"), VpcId: aws.String("vpc-a"),
				Associations: []ec2types.RouteTableAssociation{{Main: aws.Bool(true)}},
				Routes: []ec2types.Route{
					{DestinationCidrBlock: aws.String("0.0.0.0/0"), NatGatewayId: aws.String("nat-1")},
				},
			},
			{
				RouteTableId: aws.String("rtb-public"), VpcId: aws.String("vpc-a"),
				Associations: []ec2types.RouteTableAssociation{{SubnetId: aws.String("subnet-public")}},
				Routes: []ec2types.Route{
					{DestinationCidrBlock: aws.String("0.0.0.0/0"), GatewayId: aws.String("igw-1")},
				},
			},
		},
	}

	target := inventory.Target{Account: "111111111111", Region: "eu-central-1",
		NetworkSelector: map[string]string{"hs/managed": "true", "team": ""}}
	snap, err := DiscoverTarget(context.Background(), api, target)
	if err != nil {
		t.Fatal(err)
	}

	if len(api.vpcFilters) != 2 || aws.ToString(api.vpcFilters[0].Name) != "tag:hs/managed" ||
		aws.ToString(api.vpcFilters[1].Name) != "tag-key" || api.vpcFilters[1].Values[0] != "team" {
		t.Errorf("unexpected VPC filters: %+v", api.vpcFilters)
	}
	if len(api.subnetFilters) != 1 || len(api.subnetFilters[0]) != 1 ||
		aws.ToString(api.subnetFilters[0][0].Name) != "vpc-id" || len(api.subnetFilters[0][0].Values) != 2 {
		t.Errorf("subnets must be filtered by the selected VPC IDs, got %+v", api.subnetFilters)
	}

	if len(snap.Networks) != 2 {
		t.Fatalf("want 2 VPCs across pages, got %d", len(snap.Networks))
	}
	a := snap.Networks[0]
	if want := []string{"10.0.0.0/16", "100.64.0.0/16"}; len(a.CIDRBlocks) != 2 || a.CIDRBlocks[0] != want[0] || a.CIDRBlocks[1] != want[1] {
		t.Errorf("CIDR blocks = %v, want %v", a.CIDRBlocks, want)
	}
	if a.Account != target.Account || a.Region != target.Region || a.Tags["hs/managed"] != "true" {
		t.Errorf("unexpected VPC %+v", a)
	}
	if !snap.Networks[1].AWS.IsDefault {
		t.Error("vpc-b must be the default VPC")
	}

	byID := map[string]inventory.Subnet{}
	for _, s := range snap.Subnets {
		byID[s.ID] = s
	}
	pub, priv := byID["subnet-public"], byID["subnet-private"]
	if !pub.AWS.Public || pub.AWS.RouteTableID != "rtb-public" || pub.AvailableIPs == nil || *pub.AvailableIPs != 200 ||
		pub.AWS.AvailabilityZoneID != "euc1-az2" || pub.Tags["hs/owner"] != "team-a" {
		t.Errorf("unexpected public subnet %+v", pub)
	}
	if priv.AWS.Public || priv.AWS.RouteTableID != "rtb-main" {
		t.Errorf("subnet without association must use the main route table and be private: %+v", priv)
	}
}

func TestDiscoverTargetWithoutSelectorListsEverything(t *testing.T) {
	api := &fakeEC2{vpcPages: [][]ec2types.Vpc{{{VpcId: aws.String("vpc-a")}}}}
	if _, err := DiscoverTarget(context.Background(), api, inventory.Target{}); err != nil {
		t.Fatal(err)
	}
	if len(api.vpcFilters) != 0 || len(api.subnetFilters) != 1 || len(api.subnetFilters[0]) != 0 {
		t.Errorf("no filters expected without a selector: vpc=%v subnet=%v", api.vpcFilters, api.subnetFilters)
	}
}
