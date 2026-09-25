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
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	. "github.com/onsi/ginkgo/v2"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	awscloud "hypersurgery.dev/subnet-operator/internal/cloud/aws"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/provider"
	"hypersurgery.dev/subnet-operator/internal/provider/providertest"
)

// contractRegion keeps what the contract creates out of the region the operator's scopes in
// the other specs discover, whose counts they check.
const contractRegion = "eu-west-3"

// The AWS provider passes the provider contract (#45) against Moto too, not only against the
// in-memory EC2 of its unit test: the real SDK, the real wire format, Moto's own filter and
// conflict rules. Moto cannot throttle or hide IP usage on demand, so those clauses are
// skipped here and covered by the unit test.
var _ = Describe("AWS provider contract against Moto", Label("contract"), func() {
	for _, c := range providertest.Cases() {
		It(c.Name, func() {
			c.Run(GinkgoT(), newMotoFixture())
		})
	}
})

type motoFixture struct {
	ec2      *ec2.Client
	provider provider.Provider
}

func newMotoFixture() *motoFixture {
	cfg := hubConfig(context.Background())
	cfg.Region = contractRegion
	d := awscloud.NewDiscovererFromConfig(cfg)
	return &motoFixture{
		ec2:      ec2.NewFromConfig(cfg),
		provider: awscloud.NewProvider(awscloud.Clients{Discoverer: d, SubnetWriter: d, OwnershipWriter: d}, nil),
	}
}

func (f *motoFixture) Provider() provider.Provider { return f.provider }

func (f *motoFixture) Target() inventory.Target {
	return inventory.Target{Provider: networkv1.ProviderAWS, Scope: "contract", Account: hubAccount,
		Region: contractRegion}
}

func (f *motoFixture) Zone() string { return contractRegion + "a" }

func (f *motoFixture) CreateNetwork(t providertest.T, cidr string, tags map[string]string) string {
	t.Helper()
	out, err := f.ec2.CreateVpc(context.Background(), &ec2.CreateVpcInput{
		CidrBlock: aws.String(cidr), TagSpecifications: tagSpecs(ec2types.ResourceTypeVpc, flatten(tags)...),
	})
	if err != nil {
		t.Fatalf("create VPC: %v", err)
	}
	return aws.ToString(out.Vpc.VpcId)
}

func (f *motoFixture) CreateSubnet(t providertest.T, networkID, cidr string, tags map[string]string) string {
	t.Helper()
	out, err := f.ec2.CreateSubnet(context.Background(), &ec2.CreateSubnetInput{
		VpcId: aws.String(networkID), CidrBlock: aws.String(cidr), AvailabilityZone: aws.String(f.Zone()),
		TagSpecifications: tagSpecs(ec2types.ResourceTypeSubnet, flatten(tags)...),
	})
	if err != nil {
		t.Fatalf("create subnet: %v", err)
	}
	return aws.ToString(out.Subnet.SubnetId)
}

func (f *motoFixture) MakeIPUsageUnknown(providertest.T, string) bool { return false }

func (f *motoFixture) Fail(providertest.T, providertest.Failure) (func(), bool) { return nil, false }

func (f *motoFixture) InvalidTags() []map[string]string {
	return []map[string]string{
		{"aws:reserved": "x"},
		{strings.Repeat("k", 129): "long key"},
		{"hs/owner": strings.Repeat("v", 257)},
	}
}

// flatten turns tags into the key/value list tagSpecs takes.
func flatten(tags map[string]string) []string {
	var kv []string
	for k, v := range tags {
		kv = append(kv, k, v)
	}
	return kv
}
