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
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// fakeEC2Writer records the write calls and can fail each of them.
type fakeEC2Writer struct {
	created    *ec2.CreateSubnetInput
	modified   *ec2.ModifySubnetAttributeInput
	associated *ec2.AssociateRouteTableInput

	createErr    error
	associateErr error
}

func (f *fakeEC2Writer) CreateSubnet(_ context.Context, in *ec2.CreateSubnetInput, _ ...func(*ec2.Options)) (*ec2.CreateSubnetOutput, error) {
	f.created = in
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &ec2.CreateSubnetOutput{Subnet: &ec2types.Subnet{SubnetId: aws.String("subnet-0new")}}, nil
}

func (f *fakeEC2Writer) ModifySubnetAttribute(_ context.Context, in *ec2.ModifySubnetAttributeInput, _ ...func(*ec2.Options)) (*ec2.ModifySubnetAttributeOutput, error) {
	f.modified = in
	return &ec2.ModifySubnetAttributeOutput{}, nil
}

func (f *fakeEC2Writer) AssociateRouteTable(_ context.Context, in *ec2.AssociateRouteTableInput, _ ...func(*ec2.Options)) (*ec2.AssociateRouteTableOutput, error) {
	f.associated = in
	if f.associateErr != nil {
		return nil, f.associateErr
	}
	return &ec2.AssociateRouteTableOutput{AssociationId: aws.String("rtbassoc-1")}, nil
}

func request() inventory.CreateSubnetRequest {
	return inventory.CreateSubnetRequest{
		VPCID: "vpc-1", CIDRBlock: "10.0.5.0/24", AvailabilityZone: "eu-central-1a",
		RouteTableID: "rtb-private", MapPublicIPOnLaunch: true,
		Tags: map[string]string{"hs/owner": "team-a", "Name": "claim-a", "cost-center": "cc-42"},
	}
}

func TestCreateSubnetAppliesEverything(t *testing.T) {
	api := &fakeEC2Writer{}
	id, err := CreateSubnet(context.Background(), api, request())
	if err != nil || id != "subnet-0new" {
		t.Fatalf("got %q, %v", id, err)
	}
	if aws.ToString(api.created.VpcId) != "vpc-1" || aws.ToString(api.created.CidrBlock) != "10.0.5.0/24" ||
		aws.ToString(api.created.AvailabilityZone) != "eu-central-1a" {
		t.Errorf("unexpected create input %+v", api.created)
	}
	tags := api.created.TagSpecifications[0].Tags
	if api.created.TagSpecifications[0].ResourceType != ec2types.ResourceTypeSubnet || len(tags) != 3 {
		t.Fatalf("unexpected tag specification %+v", api.created.TagSpecifications)
	}
	// Sorted by key: Name, cost-center, hs/owner.
	if aws.ToString(tags[0].Key) != "Name" || aws.ToString(tags[1].Key) != "cost-center" || aws.ToString(tags[2].Key) != "hs/owner" {
		t.Errorf("tags are not sorted: %v %v %v", aws.ToString(tags[0].Key), aws.ToString(tags[1].Key), aws.ToString(tags[2].Key))
	}
	if api.modified == nil || !aws.ToBool(api.modified.MapPublicIpOnLaunch.Value) || aws.ToString(api.modified.SubnetId) != "subnet-0new" {
		t.Errorf("MapPublicIpOnLaunch not applied: %+v", api.modified)
	}
	if api.associated == nil || aws.ToString(api.associated.RouteTableId) != "rtb-private" || aws.ToString(api.associated.SubnetId) != "subnet-0new" {
		t.Errorf("route table not associated: %+v", api.associated)
	}
}

func TestCreateSubnetSkipsOptionalSteps(t *testing.T) {
	api := &fakeEC2Writer{}
	req := request()
	req.RouteTableID, req.MapPublicIPOnLaunch, req.Tags = "", false, nil
	if _, err := CreateSubnet(context.Background(), api, req); err != nil {
		t.Fatal(err)
	}
	if api.modified != nil || api.associated != nil || api.created.TagSpecifications != nil {
		t.Errorf("optional steps must be skipped: modified=%v associated=%v tags=%v", api.modified, api.associated, api.created.TagSpecifications)
	}
}

func TestCreateSubnetMapsConflicts(t *testing.T) {
	for _, code := range []string{"InvalidSubnet.Conflict", "InvalidSubnet.Range"} {
		api := &fakeEC2Writer{createErr: &smithy.GenericAPIError{Code: code, Message: "taken"}}
		id, err := CreateSubnet(context.Background(), api, request())
		if !errors.Is(err, inventory.ErrCIDRConflict) || id != "" {
			t.Errorf("%s: want ErrCIDRConflict, got %q, %v", code, id, err)
		}
	}
	api := &fakeEC2Writer{createErr: &smithy.GenericAPIError{Code: "UnauthorizedOperation", Message: "no"}}
	if _, err := CreateSubnet(context.Background(), api, request()); errors.Is(err, inventory.ErrCIDRConflict) || err == nil {
		t.Errorf("other errors must not be conflicts: %v", err)
	}
}

func TestCreateSubnetReturnsIDWhenFollowUpFails(t *testing.T) {
	api := &fakeEC2Writer{associateErr: errors.New("boom")}
	id, err := CreateSubnet(context.Background(), api, request())
	if err == nil || id != "subnet-0new" {
		t.Fatalf("the subnet exists, its ID must be returned with the error: %q, %v", id, err)
	}
}
