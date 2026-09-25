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
	"fmt"
	"slices"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/smithy-go"

	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// EC2WriteAPI is the subset of the EC2 client used to create subnets.
type EC2WriteAPI interface {
	CreateSubnet(ctx context.Context, in *ec2.CreateSubnetInput, opts ...func(*ec2.Options)) (*ec2.CreateSubnetOutput, error)
	ModifySubnetAttribute(ctx context.Context, in *ec2.ModifySubnetAttributeInput, opts ...func(*ec2.Options)) (*ec2.ModifySubnetAttributeOutput, error)
	AssociateRouteTable(ctx context.Context, in *ec2.AssociateRouteTableInput, opts ...func(*ec2.Options)) (*ec2.AssociateRouteTableOutput, error)
}

// EC2TagAPI is the one call needed to take a resource under management. A role holding only
// this is enough for imports, which is why it is a separate interface.
type EC2TagAPI interface {
	CreateTags(ctx context.Context, in *ec2.CreateTagsInput, opts ...func(*ec2.Options)) (*ec2.CreateTagsOutput, error)
}

var (
	_ inventory.SubnetWriter    = (*Discoverer)(nil)
	_ inventory.OwnershipWriter = (*Discoverer)(nil)
)

// WriteOwnership implements inventory.OwnershipWriter with the target's write role. On AWS
// every VPC and subnet carries its own tags, so the tags go onto the resource itself.
func (d *Discoverer) WriteOwnership(ctx context.Context, target inventory.Target, resourceID string, tags map[string]string) error {
	api, err := d.writeClient(ctx, target)
	if err != nil {
		return err
	}
	return ApplyTags(ctx, api, resourceID, tags)
}

// writeClient is the EC2 client for a write to the target, with its write role.
func (d *Discoverer) writeClient(ctx context.Context, target inventory.Target) (ec2Client, error) {
	creds, err := d.credentialsFor(ctx, target)
	if err != nil {
		return nil, err
	}
	if d.testEC2 != nil {
		return d.testEC2(target), nil
	}
	cfg := d.base.Copy()
	cfg.Region = target.Region
	cfg.Credentials = creds
	return ec2.NewFromConfig(cfg), nil
}

// ApplyTags adds tags to one resource. CreateTags overwrites the keys it names and leaves
// every other tag alone, so running the same import twice changes nothing.
func ApplyTags(ctx context.Context, api EC2TagAPI, resourceID string, tags map[string]string) error {
	if len(tags) == 0 {
		return nil
	}
	if _, err := api.CreateTags(ctx, &ec2.CreateTagsInput{
		Resources: []string{resourceID},
		Tags:      sortedTags(tags),
	}); err != nil {
		return fmt.Errorf("tag %s: %w", resourceID, err)
	}
	return nil
}

// CreateSubnet implements inventory.SubnetWriter with the target's write role.
func (d *Discoverer) CreateSubnet(ctx context.Context, target inventory.Target, req inventory.CreateSubnetRequest) (string, error) {
	api, err := d.writeClient(ctx, target)
	if err != nil {
		return "", err
	}
	return CreateSubnet(ctx, api, req)
}

// CreateSubnet creates one tagged subnet and applies the optional attribute and route table.
// A CIDR that AWS rejects as taken is reported as inventory.ErrCIDRConflict.
func CreateSubnet(ctx context.Context, api EC2WriteAPI, req inventory.CreateSubnetRequest) (string, error) {
	out, err := api.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId:             aws.String(req.NetworkID),
		CidrBlock:         aws.String(req.CIDRBlock),
		AvailabilityZone:  aws.String(req.Zone),
		TagSpecifications: tagSpecs(ec2types.ResourceTypeSubnet, req.Tags),
	})
	if err != nil {
		if isConflict(err) {
			return "", fmt.Errorf("%w: %s", inventory.ErrCIDRConflict, err.Error())
		}
		return "", fmt.Errorf("create subnet: %w", err)
	}
	id := aws.ToString(out.Subnet.SubnetId)

	// The subnet exists from here on; later failures are reported with its ID so the
	// controller records it instead of creating a second one.
	opts := req.AWS
	if opts == nil {
		opts = &networkv1beta1.AWSClaimOptions{}
	}
	if opts.MapPublicIPOnLaunch {
		if _, err := api.ModifySubnetAttribute(ctx, &ec2.ModifySubnetAttributeInput{
			SubnetId:            aws.String(id),
			MapPublicIpOnLaunch: &ec2types.AttributeBooleanValue{Value: aws.Bool(true)},
		}); err != nil {
			return id, fmt.Errorf("set MapPublicIpOnLaunch on %s: %w", id, err)
		}
	}
	if opts.RouteTableID != "" {
		if _, err := api.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{
			SubnetId: aws.String(id), RouteTableId: aws.String(opts.RouteTableID),
		}); err != nil {
			return id, fmt.Errorf("associate %s with %s: %w", id, opts.RouteTableID, err)
		}
	}
	return id, nil
}

// isConflict recognises the EC2 errors for a CIDR that is already in use or outside the VPC.
func isConflict(err error) bool {
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return slices.Contains([]string{"InvalidSubnet.Conflict", "InvalidSubnet.Range"}, apiErr.ErrorCode())
}

func tagSpecs(resource ec2types.ResourceType, tags map[string]string) []ec2types.TagSpecification {
	if len(tags) == 0 {
		return nil
	}
	return []ec2types.TagSpecification{{ResourceType: resource, Tags: sortedTags(tags)}}
}

// sortedTags keeps request bodies stable, which makes tests and logs readable.
func sortedTags(tags map[string]string) []ec2types.Tag {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]ec2types.Tag, 0, len(keys))
	for _, k := range keys {
		out = append(out, ec2types.Tag{Key: aws.String(k), Value: aws.String(tags[k])})
	}
	return out
}
