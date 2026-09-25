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
	"slices"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"hypersurgery.dev/subnet-operator/internal/inventory"
)

func unmanagedFake() *fakeEC2 {
	return &fakeEC2{
		vpcPages: [][]ec2types.Vpc{{
			{
				VpcId: aws.String("vpc-managed"), CidrBlock: aws.String("10.0.0.0/16"),
				Tags: []ec2types.Tag{{Key: aws.String("hs/managed"), Value: aws.String("true")}},
			},
			{
				VpcId: aws.String("vpc-legacy"), CidrBlock: aws.String("10.90.0.0/16"),
				Tags: []ec2types.Tag{{Key: aws.String("Name"), Value: aws.String("legacy-shared")}},
			},
			{
				VpcId: aws.String("vpc-wrongvalue"), CidrBlock: aws.String("10.91.0.0/16"),
				Tags: []ec2types.Tag{{Key: aws.String("hs/managed"), Value: aws.String("false")}},
			},
		}},
		subnetsByVPC: map[string][]ec2types.Subnet{
			"vpc-managed": {{SubnetId: aws.String("subnet-managed"), VpcId: aws.String("vpc-managed"),
				CidrBlock: aws.String("10.0.1.0/24"), AvailableIpAddressCount: aws.Int32(251)}},
			"vpc-legacy": {{SubnetId: aws.String("subnet-legacy"), VpcId: aws.String("vpc-legacy"),
				CidrBlock: aws.String("10.90.1.0/24"), AvailableIpAddressCount: aws.Int32(251)}},
			"vpc-wrongvalue": {{SubnetId: aws.String("subnet-wrongvalue"), VpcId: aws.String("vpc-wrongvalue"),
				CidrBlock: aws.String("10.91.1.0/24"), AvailableIpAddressCount: aws.Int32(251)}},
		},
	}
}

func TestDiscoverTargetReportsUnmanaged(t *testing.T) {
	api := unmanagedFake()
	target := inventory.Target{
		Account: "111111111111", Region: "eu-central-1",
		NetworkSelector: map[string]string{"hs/managed": "true"}, DiscoverUnmanaged: true,
	}

	snap, err := DiscoverTarget(context.Background(), api, target)
	if err != nil {
		t.Fatal(err)
	}

	// One listing, classified in Go: asking EC2 twice for the same VPCs would be wasteful.
	if api.vpcCalls != 1 {
		t.Errorf("DescribeVpcs called %d times, want once", api.vpcCalls)
	}
	if len(api.vpcFilters) != 0 {
		t.Errorf("the listing must be unfiltered to see everything, got %v", api.vpcFilters)
	}

	if len(snap.Networks) != 1 || snap.Networks[0].ID != "vpc-managed" {
		t.Fatalf("managed VPCs = %+v, want only vpc-managed", snap.Networks)
	}
	unmanaged := make([]string, 0, len(snap.UnmanagedNetworks))
	for _, v := range snap.UnmanagedNetworks {
		unmanaged = append(unmanaged, v.ID)
	}
	// A tag with the wrong value is as unmanaged as no tag at all.
	if want := []string{"vpc-legacy", "vpc-wrongvalue"}; !slices.Equal(unmanaged, want) {
		t.Errorf("unmanaged VPCs = %v, want %v", unmanaged, want)
	}

	if len(snap.Subnets) != 1 || snap.Subnets[0].ID != "subnet-managed" {
		t.Errorf("managed subnets = %+v, want only subnet-managed", snap.Subnets)
	}
	unmanagedSubnets := make([]string, 0, len(snap.UnmanagedSubnets))
	for _, s := range snap.UnmanagedSubnets {
		unmanagedSubnets = append(unmanagedSubnets, s.ID)
	}
	if want := []string{"subnet-legacy", "subnet-wrongvalue"}; !slices.Equal(unmanagedSubnets, want) {
		t.Errorf("unmanaged subnets = %v, want %v", unmanagedSubnets, want)
	}
}

func TestDiscoverTargetWithoutUnmanagedFiltersServerSide(t *testing.T) {
	api := unmanagedFake()
	target := inventory.Target{
		Account: "111111111111", Region: "eu-central-1",
		NetworkSelector: map[string]string{"hs/managed": "true"},
	}

	snap, err := DiscoverTarget(context.Background(), api, target)
	if err != nil {
		t.Fatal(err)
	}
	// The selector goes to EC2, so the fake's filter is what proves nothing extra was read.
	if len(api.vpcFilters) != 1 || aws.ToString(api.vpcFilters[0].Name) != "tag:hs/managed" {
		t.Errorf("filters = %v, want the selector applied server side", api.vpcFilters)
	}
	if len(snap.UnmanagedNetworks) != 0 || len(snap.UnmanagedSubnets) != 0 {
		t.Errorf("nothing unmanaged should be reported, got %d VPCs and %d subnets",
			len(snap.UnmanagedNetworks), len(snap.UnmanagedSubnets))
	}
}

func TestMatchesSelector(t *testing.T) {
	cases := []struct {
		name     string
		tags     map[string]string
		selector map[string]string
		want     bool
	}{
		{"empty selector matches", map[string]string{"a": "b"}, nil, true},
		{"exact value", map[string]string{"hs/managed": "true"}, map[string]string{"hs/managed": "true"}, true},
		{"wrong value", map[string]string{"hs/managed": "false"}, map[string]string{"hs/managed": "true"}, false},
		{"missing key", nil, map[string]string{"hs/managed": "true"}, false},
		{"any value of the key", map[string]string{"team": "x"}, map[string]string{"team": ""}, true},
		{"every entry must match", map[string]string{"a": "1"}, map[string]string{"a": "1", "b": "2"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := matchesSelector(c.tags, c.selector); got != c.want {
				t.Errorf("matchesSelector(%v, %v) = %v, want %v", c.tags, c.selector, got, c.want)
			}
		})
	}
}
