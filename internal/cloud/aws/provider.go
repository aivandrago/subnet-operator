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
	"time"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
	"hypersurgery.dev/subnet-operator/internal/cloud/aws/events"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

// Identity is how the operator reaches an AWS account: a role to assume, with the external ID
// its trust policy may require. The zero value is the operator's own credentials (EKS Pod
// Identity, IRSA), which only reach the account the operator runs in.
type Identity struct {
	RoleARN    string
	ExternalID string
}

// Own implements inventory.Identity.
func (i Identity) Own() bool { return i.RoleARN == "" }

// String implements inventory.Identity. The external ID is not a secret, but it is not shown
// either: the role names the identity.
func (i Identity) String() string {
	if i.Own() {
		return "the operator's own credentials"
	}
	return i.RoleARN
}

// identityOf reads the AWS identity of a target. A target without one uses the operator's own
// credentials; a target carrying another provider's identity is a wiring mistake.
func identityOf(target inventory.Target) (Identity, error) {
	switch id := target.Identity.(type) {
	case nil:
		return Identity{}, nil
	case Identity:
		return id, nil
	case *Identity:
		if id == nil {
			return Identity{}, nil
		}
		return *id, nil
	default:
		return Identity{}, errors.New("the target's identity is not an AWS identity: " + id.String())
	}
}

// Clients are the EC2 clients of the provider. The real Discoverer implements all three; tests
// put fakes here. A nil client makes its calls fail.
type Clients struct {
	Discoverer      inventory.Discoverer
	SubnetWriter    inventory.SubnetWriter
	OwnershipWriter inventory.OwnershipWriter
}

// Events configures the provider's change events: EventBridge rules deliver EC2 API calls
// from CloudTrail to an SQS queue, which the operator long-polls (deploy/events).
type Events struct {
	SQS      events.SQSAPI
	QueueURL string
	// Debounce is how long events are collected before the changed targets are resynced.
	Debounce time.Duration
	Log      logr.Logger
}

// Provider is the AWS provider: accounts reached by sts:AssumeRole, VPCs and subnets read and
// created with EC2, ownership in resource tags, change events from EventBridge through SQS.
type Provider struct {
	clients Clients
	events  *Events
}

var _ provider.Provider = (*Provider)(nil)

// NewProvider returns the AWS provider with the given clients. changeEvents may be nil: without
// change events the inventory follows the resync interval.
func NewProvider(clients Clients, changeEvents *Events) *Provider {
	return &Provider{clients: clients, events: changeEvents}
}

// Options configure the provider NewFromEnvironment builds.
type Options struct {
	// EventsQueueURL is the SQS queue with EC2 change events; empty for none.
	EventsQueueURL string
	EventsDebounce time.Duration
	// OnThrottle is told about every EC2 attempt the API throttled.
	OnThrottle func(target inventory.Target, operation string)
	Log        logr.Logger
}

// NewFromEnvironment builds the provider with the operator's own AWS configuration (the
// default credential chain: EKS Pod Identity, IRSA, environment).
func NewFromEnvironment(ctx context.Context, opts Options) (*Provider, error) {
	d, err := NewDiscoverer(ctx)
	if err != nil {
		return nil, err
	}
	d.OnThrottle = opts.OnThrottle
	var ev *Events
	if opts.EventsQueueURL != "" {
		client, err := NewSQSClient(ctx, opts.EventsQueueURL)
		if err != nil {
			return nil, err
		}
		ev = &Events{SQS: client, QueueURL: opts.EventsQueueURL, Debounce: opts.EventsDebounce, Log: opts.Log}
	}
	return NewProvider(Clients{Discoverer: d, SubnetWriter: d, OwnershipWriter: d}, ev), nil
}

// Name implements provider.Provider.
func (p *Provider) Name() networkv1beta1.Provider { return networkv1beta1.ProviderAWS }

// Capabilities implements provider.Provider. EC2 reports free addresses for every subnet, and
// creates subnets; change events need the queue.
func (p *Provider) Capabilities() []networkv1beta1.Capability {
	caps := []networkv1beta1.Capability{networkv1beta1.CapabilityCreateSubnet, networkv1beta1.CapabilityIPUsage}
	if p.events != nil {
		caps = append(caps, networkv1beta1.CapabilityChangeEvents)
	}
	return caps
}

// Ownership implements provider.Provider: VPCs and subnets both carry their own tags.
func (p *Provider) Ownership() networkv1beta1.Ownership {
	return networkv1beta1.Ownership{
		Networks: networkv1beta1.OwnershipResourceTags,
		Subnets:  networkv1beta1.OwnershipResourceTags,
	}
}

// Identity implements provider.Provider. Writes use writeRoleARN, never roleARN, so the read
// role can stay read-only. The external ID goes with either role.
func (p *Provider) Identity(account networkv1beta1.Account, access provider.Access) inventory.Identity {
	a := account.AWSAccount()
	if access == provider.Write {
		return Identity{RoleARN: a.WriteRoleARN, ExternalID: a.ExternalID}
	}
	return Identity{RoleARN: a.RoleARN, ExternalID: a.ExternalID}
}

// WriteIdentityField implements provider.Provider.
func (p *Provider) WriteIdentityField() string { return "aws.writeRoleARN" }

// OwnershipPermission implements provider.Provider.
func (p *Provider) OwnershipPermission() string { return "ec2:CreateTags" }

var errNoClient = errors.New("the AWS provider has no EC2 client for this call")

// Discover implements inventory.Discoverer.
func (p *Provider) Discover(ctx context.Context, target inventory.Target) (*inventory.Snapshot, error) {
	if p.clients.Discoverer == nil {
		return nil, errNoClient
	}
	return p.clients.Discoverer.Discover(ctx, target)
}

// CreateSubnet implements inventory.SubnetWriter.
func (p *Provider) CreateSubnet(ctx context.Context, target inventory.Target, req inventory.CreateSubnetRequest) (string, error) {
	if p.clients.SubnetWriter == nil {
		return "", errNoClient
	}
	return p.clients.SubnetWriter.CreateSubnet(ctx, target, req)
}

// WriteOwnership implements inventory.OwnershipWriter.
func (p *Provider) WriteOwnership(ctx context.Context, target inventory.Target, resourceID string, tags map[string]string) error {
	if p.clients.OwnershipWriter == nil {
		return errNoClient
	}
	return p.clients.OwnershipWriter.WriteOwnership(ctx, target, resourceID, tags)
}

// Events implements provider.Provider: the SQS poller, when a queue is configured.
func (p *Provider) Events(sink provider.EventSink) manager.Runnable {
	if p.events == nil {
		return nil
	}
	return &events.Poller{
		SQS:      p.events.SQS,
		QueueURL: p.events.QueueURL,
		Sink:     sink.Changed,
		OnCreate: sink.Created,
		Debounce: p.events.Debounce,
		Log:      p.events.Log,
	}
}

// Ensure the SQS client satisfies what the poller needs.
var _ events.SQSAPI = (*sqs.Client)(nil)
