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
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"

	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

// Discovery assumes roleARN, writes assume writeRoleARN, never the other way round: the read
// role must stay read-only.
func TestIdentitySeparatesReadAndWrite(t *testing.T) {
	p := NewProvider(Clients{}, nil)
	account := networkv1beta1.Account{ID: "222222222222", AWS: &networkv1beta1.AWSAccount{
		RoleARN:      "arn:aws:iam::222222222222:role/read",
		WriteRoleARN: "arn:aws:iam::222222222222:role/write",
		ExternalID:   "ext",
	}}
	if got := p.Identity(account, provider.Read); got != (Identity{RoleARN: account.AWS.RoleARN, ExternalID: "ext"}) {
		t.Errorf("read identity = %#v", got)
	}
	if got := p.Identity(account, provider.Write); got != (Identity{RoleARN: account.AWS.WriteRoleARN, ExternalID: "ext"}) {
		t.Errorf("write identity = %#v", got)
	}
	if !p.Identity(networkv1beta1.Account{ID: "111111111111"}, provider.Write).Own() {
		t.Error("an account without an aws member is not reached with the operator's own credentials")
	}
}

type foreignIdentity struct{}

func (foreignIdentity) Own() bool      { return false }
func (foreignIdentity) String() string { return "projects/p/serviceAccounts/reader" }

// A target carrying another provider's identity is a wiring mistake; it must fail rather than
// fall back to the operator's own credentials.
func TestForeignIdentityIsRefused(t *testing.T) {
	d := newDiscoverer(awsConfigForTest())
	_, err := d.Discover(context.Background(), inventory.Target{Account: "111111111111", Region: "eu-central-1",
		Identity: foreignIdentity{}})
	if err == nil || !strings.Contains(err.Error(), "not an AWS identity") {
		t.Fatalf("err = %v, want a refusal of the foreign identity", err)
	}
}

// ChangeEvents is only claimed when there is a queue to read them from.
func TestCapabilitiesFollowTheConfiguration(t *testing.T) {
	without := NewProvider(Clients{}, nil).Capabilities()
	if slices.Contains(without, networkv1beta1.CapabilityChangeEvents) {
		t.Errorf("capabilities without a queue = %v", without)
	}
	for _, c := range []networkv1beta1.Capability{networkv1beta1.CapabilityCreateSubnet, networkv1beta1.CapabilityIPUsage} {
		if !slices.Contains(without, c) {
			t.Errorf("capabilities = %v, missing %s", without, c)
		}
	}
	with := NewProvider(Clients{}, &Events{QueueURL: "https://sqs.eu-central-1.amazonaws.com/1/q"})
	if !slices.Contains(with.Capabilities(), networkv1beta1.CapabilityChangeEvents) {
		t.Errorf("capabilities with a queue = %v", with.Capabilities())
	}
	if NewProvider(Clients{}, nil).Events(provider.EventSink{}) != nil {
		t.Error("an event source without a queue")
	}
	if with.Events(provider.EventSink{}) == nil {
		t.Error("no event source although a queue is configured")
	}
}

func awsConfigForTest() aws.Config {
	return aws.Config{Region: "eu-central-1"}
}
