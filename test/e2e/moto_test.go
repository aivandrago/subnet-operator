//go:build e2e
// +build e2e

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

package e2e

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	. "github.com/onsi/gomega"
)

const (
	// hubAccount is Moto's default account: the operator's own credentials land here.
	hubAccount = "123456789012"
	// spokeAccount is reached with sts:AssumeRole; Moto isolates its resources.
	spokeAccount = "222222222222"
	spokeRoleARN = "arn:aws:iam::" + spokeAccount + ":role/aws-subnet-operator-readonly"
	awsRegion    = "eu-central-1"
	// eventsQueue receives EC2 change events in the hub account.
	eventsQueue = "aws-subnet-operator-events"
)

// fixture holds the IDs of the resources seeded into Moto.
type fixture struct {
	hubVPC, unmanagedVPC, spokeVPC string
	publicSubnet, privateSubnet    string
	spokeSubnet                    string
	// unmanagedSubnet lives in unmanagedVPC and carries no tags at all: the resource somebody
	// created by hand that the operator should count but not manage.
	unmanagedSubnet string
}

func hubConfig(ctx context.Context) aws.Config {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(awsRegion),
		config.WithBaseEndpoint(motoHostEndpoint),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	Expect(err).NotTo(HaveOccurred())
	return cfg
}

func hubEC2(ctx context.Context) *ec2.Client {
	return ec2.NewFromConfig(hubConfig(ctx))
}

func spokeEC2(ctx context.Context) *ec2.Client {
	cfg := hubConfig(ctx)
	cfg.Credentials = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), spokeRoleARN))
	return ec2.NewFromConfig(cfg)
}

// createEventsQueue creates the hub events queue and returns its URL as pods see it.
func createEventsQueue(ctx context.Context) string {
	_, err := sqs.NewFromConfig(hubConfig(ctx)).CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: aws.String(eventsQueue)})
	Expect(err).NotTo(HaveOccurred())
	return motoClusterEndpoint + "/" + hubAccount + "/" + eventsQueue
}

// sendChangeEvent puts an EventBridge "AWS API Call via CloudTrail" event on the queue, as the
// hub rule would after CloudTrail recorded the call. Moto does not emit CloudTrail events.
func sendChangeEvent(ctx context.Context, account, eventName string) {
	body := fmt.Sprintf(`{"version":"0","source":"aws.ec2","detail-type":"AWS API Call via CloudTrail",`+
		`"account":%q,"region":%q,"detail":{"eventSource":"ec2.amazonaws.com","eventName":%q,`+
		`"awsRegion":%q,"recipientAccountId":%q}}`, account, awsRegion, eventName, awsRegion, account)
	_, err := sqs.NewFromConfig(hubConfig(ctx)).SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:    aws.String(motoHostEndpoint + "/" + hubAccount + "/" + eventsQueue),
		MessageBody: aws.String(body),
	})
	Expect(err).NotTo(HaveOccurred())
}

// tagSpecs builds tag specifications from key/value pairs. EC2 rejects a specification
// without tags, so none is returned for an untagged resource.
func tagSpecs(resource ec2types.ResourceType, kv ...string) []ec2types.TagSpecification {
	if len(kv) == 0 {
		return nil
	}
	var tags []ec2types.Tag
	for i := 0; i+1 < len(kv); i += 2 {
		tags = append(tags, ec2types.Tag{Key: aws.String(kv[i]), Value: aws.String(kv[i+1])})
	}
	return []ec2types.TagSpecification{{ResourceType: resource, Tags: tags}}
}

func createVPC(ctx context.Context, api *ec2.Client, cidr string, kv ...string) string {
	out, err := api.CreateVpc(ctx, &ec2.CreateVpcInput{
		CidrBlock:         aws.String(cidr),
		TagSpecifications: tagSpecs(ec2types.ResourceTypeVpc, kv...),
	})
	Expect(err).NotTo(HaveOccurred())
	return aws.ToString(out.Vpc.VpcId)
}

func createSubnet(ctx context.Context, api *ec2.Client, vpc, cidr, az string, kv ...string) string {
	out, err := api.CreateSubnet(ctx, &ec2.CreateSubnetInput{
		VpcId: aws.String(vpc), CidrBlock: aws.String(cidr), AvailabilityZone: aws.String(az),
		TagSpecifications: tagSpecs(ec2types.ResourceTypeSubnet, kv...),
	})
	Expect(err).NotTo(HaveOccurred())
	return aws.ToString(out.Subnet.SubnetId)
}

// makePublic routes the subnet's default route to a new internet gateway.
func makePublic(ctx context.Context, api *ec2.Client, vpc, subnet string) {
	igw, err := api.CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{})
	Expect(err).NotTo(HaveOccurred())
	igwID := igw.InternetGateway.InternetGatewayId
	_, err = api.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{InternetGatewayId: igwID, VpcId: aws.String(vpc)})
	Expect(err).NotTo(HaveOccurred())
	rt, err := api.CreateRouteTable(ctx, &ec2.CreateRouteTableInput{VpcId: aws.String(vpc)})
	Expect(err).NotTo(HaveOccurred())
	_, err = api.CreateRoute(ctx, &ec2.CreateRouteInput{
		RouteTableId: rt.RouteTable.RouteTableId, DestinationCidrBlock: aws.String("0.0.0.0/0"), GatewayId: igwID,
	})
	Expect(err).NotTo(HaveOccurred())
	_, err = api.AssociateRouteTable(ctx, &ec2.AssociateRouteTableInput{
		RouteTableId: rt.RouteTable.RouteTableId, SubnetId: aws.String(subnet),
	})
	Expect(err).NotTo(HaveOccurred())
}

// seedAWS creates a managed VPC with a public and an untagged private subnet and an unmanaged
// VPC in the hub account, and a managed VPC overlapping the hub VPC in the spoke account.
func seedAWS(ctx context.Context) fixture {
	hub, spoke := hubEC2(ctx), spokeEC2(ctx)
	var f fixture

	f.hubVPC = createVPC(ctx, hub, "10.0.0.0/16", "hs/managed", "true", "Name", "prod", "hs/owner", "platform")
	f.publicSubnet = createSubnet(ctx, hub, f.hubVPC, "10.0.1.0/24", awsRegion+"a",
		"Name", "prod-public-a", "hs/owner", "team-web", "hs/env", "prod", "hs/tier", "public")
	makePublic(ctx, hub, f.hubVPC, f.publicSubnet)
	f.privateSubnet = createSubnet(ctx, hub, f.hubVPC, "10.0.2.0/24", awsRegion+"b")
	f.unmanagedVPC = createVPC(ctx, hub, "10.50.0.0/16", "Name", "sandbox")
	f.unmanagedSubnet = createSubnet(ctx, hub, f.unmanagedVPC, "10.50.1.0/24", awsRegion+"a")

	f.spokeVPC = createVPC(ctx, spoke, "10.0.128.0/20", "hs/managed", "true", "Name", "partner")
	f.spokeSubnet = createSubnet(ctx, spoke, f.spokeVPC, "10.0.128.0/28", awsRegion+"a",
		"hs/owner", "team-partner", "hs/env", "prod", "hs/tier", "private")
	return f
}

func createSpokeSubnet(ctx context.Context, vpc, cidr string) string {
	return createSubnet(ctx, spokeEC2(ctx), vpc, cidr, awsRegion+"b",
		"hs/owner", "team-partner", "hs/env", "prod", "hs/tier", "private")
}

// motoSubnet is what the e2e checks about a subnet the operator created.
type motoSubnet struct {
	id, cidr string
	tags     map[string]string
}

// describeSubnetsByTag lists hub-account subnets carrying the tag.
func describeSubnetsByTag(ctx context.Context, key, value string) []motoSubnet {
	out, err := hubEC2(ctx).DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{{Name: aws.String("tag:" + key), Values: []string{value}}},
	})
	Expect(err).NotTo(HaveOccurred())
	var result []motoSubnet
	for _, s := range out.Subnets {
		tags := map[string]string{}
		for _, t := range s.Tags {
			tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
		}
		result = append(result, motoSubnet{id: aws.ToString(s.SubnetId), cidr: aws.ToString(s.CidrBlock), tags: tags})
	}
	return result
}

// tagsOf reads the tags a hub-account resource carries, which is how the e2e sees what an
// import actually did to AWS rather than what its status claims.
func tagsOf(ctx context.Context, resourceID string) map[string]string {
	out, err := hubEC2(ctx).DescribeTags(ctx, &ec2.DescribeTagsInput{
		Filters: []ec2types.Filter{{Name: aws.String("resource-id"), Values: []string{resourceID}}},
	})
	Expect(err).NotTo(HaveOccurred())
	tags := map[string]string{}
	for _, t := range out.Tags {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return tags
}

func deleteSubnet(ctx context.Context, id string) {
	_, err := hubEC2(ctx).DeleteSubnet(ctx, &ec2.DeleteSubnetInput{SubnetId: aws.String(id)})
	Expect(err).NotTo(HaveOccurred())
}
