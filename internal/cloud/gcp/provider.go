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

// Package gcp is the Google Cloud provider: projects, VPC networks and subnetworks, read with
// the Compute Engine API, and ownership in Resource Manager tags (ADR 0002 §6). It creates
// subnetworks and binds tags (#47). Projects are reached with the operator's own identity or by
// impersonating a service account per project and per direction, read or write
// (credentials.go). Change events come from Cloud Audit Logs through a log sink and Pub/Sub
// (package events, #49) when a subscription is configured.
package gcp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	"google.golang.org/api/option"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/cloud/gcp/events"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

// Identity is how the operator reaches a project. The zero value is the operator's own
// identity: Application Default Credentials, which on Kubernetes come from GKE Workload
// Identity or Workload Identity Federation. A service account to impersonate
// (accounts[].gcp.serviceAccount, or writeServiceAccount for writes) is the GCP counterpart of
// an AWS role to assume.
type Identity struct {
	// ServiceAccount is the email of a service account to impersonate, empty for the
	// operator's own identity.
	ServiceAccount string
}

// Own implements inventory.Identity.
func (i Identity) Own() bool { return i.ServiceAccount == "" }

// String implements inventory.Identity.
func (i Identity) String() string {
	if i.Own() {
		return "the operator's own credentials"
	}
	return i.ServiceAccount
}

// identityOf reads the GCP identity of a target. A target without one uses the operator's own
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
		return Identity{}, errors.New("the target's identity is not a GCP identity: " + id.String())
	}
}

// Options configure the provider.
type Options struct {
	// ClientOptions are added to every Google API client the provider builds, after the
	// credentials: tests point them at the in-repo fake (gcpfake) with
	// option.WithoutAuthentication and option.WithHTTPClient.
	ClientOptions []option.ClientOption
	// ComputeEndpoint replaces https://compute.googleapis.com.
	ComputeEndpoint string
	// ResourceManagerEndpoint returns the Resource Manager endpoint of a location: "global",
	// or a region for the tags of regional resources, which Resource Manager only serves
	// from the region's own endpoint. Nil is Google's endpoints.
	ResourceManagerEndpoint func(location string) string
	// IAMCredentialsEndpoint replaces https://iamcredentials.googleapis.com, where the
	// operator's own identity gets the tokens of the service accounts it impersonates.
	IAMCredentialsEndpoint string
	// OnThrottle is told about every Google API attempt that was rate-limited, including the
	// ones a retry then got through.
	OnThrottle func(target inventory.Target, operation string)
	Log        logr.Logger

	// EventsSubscription is the Pub/Sub subscription (projects/<project>/subscriptions/<name>)
	// a log sink fills with the audit log entries of network changes; empty for none.
	EventsSubscription string
	// EventsDebounce is how long events are collected before the changed targets resync.
	EventsDebounce time.Duration
	// PubSubClientOptions are added to the Pub/Sub client, which uses gRPC rather than the
	// REST transport ClientOptions are for: tests point it at pstest with
	// option.WithGRPCConn. Without them it runs as the operator's own identity.
	PubSubClientOptions []option.ClientOption
	// EventsLog is the change event source's logger.
	EventsLog logr.Logger
}

// Provider is the GCP provider.
type Provider struct {
	discoverer *Discoverer
	events     *eventsConfig
}

// eventsConfig is the provider's change event source, when one is configured.
type eventsConfig struct {
	subscription string
	connect      events.Connect
	debounce     time.Duration
	log          logr.Logger
}

var _ provider.Provider = (*Provider)(nil)

// NewProvider returns the GCP provider. Nothing is called until the first discovery, so it
// starts without credentials; a discovery without them fails and says so in the scope. The
// change event source, too, connects only once it runs, and retries until it can.
func NewProvider(opts Options) *Provider {
	p := &Provider{discoverer: newDiscoverer(opts)}
	if opts.EventsSubscription != "" {
		p.events = &eventsConfig{subscription: opts.EventsSubscription, debounce: opts.EventsDebounce,
			connect: events.PubSub(opts.EventsSubscription, opts.PubSubClientOptions...), log: opts.EventsLog}
	}
	return p
}

// NewFromEnvironment builds the provider with the operator's own Google credentials
// (Application Default Credentials). A subscription name that cannot be right stops the
// operator from starting, rather than failing every minute from then on.
func NewFromEnvironment(_ context.Context, opts Options) (*Provider, error) {
	if s := opts.EventsSubscription; s != "" && !events.SubscriptionPattern.MatchString(s) {
		return nil, fmt.Errorf("events subscription %q is not projects/<project>/subscriptions/<name>", s)
	}
	return NewProvider(opts), nil
}

// Name implements provider.Provider.
func (p *Provider) Name() networkv1.Provider { return networkv1.ProviderGCP }

// Capabilities implements provider.Provider. Compute reports free addresses per range
// (views=WITH_UTILIZATION), and subnetworks are created with their tags bound; change events
// need the subscription.
func (p *Provider) Capabilities() []networkv1.Capability {
	caps := []networkv1.Capability{networkv1.CapabilityCreateSubnet, networkv1.CapabilityIPUsage}
	if p.events != nil {
		caps = append(caps, networkv1.CapabilityChangeEvents)
	}
	return caps
}

// Ownership implements provider.Provider: networks and subnetworks both carry their own
// Resource Manager tag bindings.
func (p *Provider) Ownership() networkv1.Ownership {
	return networkv1.Ownership{
		Networks: networkv1.OwnershipResourceTags,
		Subnets:  networkv1.OwnershipResourceTags,
	}
}

// Identity implements provider.Provider. Reads impersonate the account's gcp.serviceAccount
// and writes its gcp.writeServiceAccount, never the read one; an empty one is the operator's
// own identity.
func (p *Provider) Identity(account networkv1.Account, access provider.Access) inventory.Identity {
	a := account.GCP
	switch {
	case a == nil:
		return Identity{}
	case access == provider.Write:
		return Identity{ServiceAccount: a.WriteServiceAccount}
	default:
		return Identity{ServiceAccount: a.ServiceAccount}
	}
}

// WriteIdentityField implements provider.Provider.
func (p *Provider) WriteIdentityField() string { return "gcp.writeServiceAccount" }

// OwnershipPermission implements provider.Provider: binding a tag value to a network or
// subnetwork. The binding also needs createTagBinding on the resource itself
// (compute.subnetworks.createTagBinding, compute.networks.createTagBinding).
func (p *Provider) OwnershipPermission() string { return "resourcemanager.tagValueBindings.create" }

// ReplacesOwnershipValues implements provider.Provider: a resource carries one value per tag
// key, and changing it would mean deleting the binding, which the operator never does.
func (p *Provider) ReplacesOwnershipValues() bool { return false }

// Discover implements inventory.Discoverer.
func (p *Provider) Discover(ctx context.Context, target inventory.Target) (*inventory.Snapshot, error) {
	return p.discoverer.Discover(ctx, target)
}

// CreateSubnet implements inventory.SubnetWriter: a regional subnetwork, with its tags bound
// at creation, as the target's write identity.
func (p *Provider) CreateSubnet(ctx context.Context, target inventory.Target, req inventory.CreateSubnetRequest) (string, error) {
	return p.discoverer.CreateSubnet(ctx, target, req)
}

// WriteOwnership implements inventory.OwnershipWriter: tag bindings on the network or
// subnetwork, only ever added, as the target's write identity.
func (p *Provider) WriteOwnership(ctx context.Context, target inventory.Target, resourceID string, tags map[string]string) error {
	return p.discoverer.WriteOwnership(ctx, target, resourceID, tags)
}

// Events implements provider.Provider: the Pub/Sub subscriber, when a subscription is
// configured. Without one the inventory follows the resync interval.
func (p *Provider) Events(sink provider.EventSink) manager.Runnable {
	if p.events == nil {
		return nil
	}
	return &events.Subscriber{
		Connect:      p.events.connect,
		Subscription: p.events.subscription,
		Sink:         sink,
		Projects:     p.discoverer.projectID,
		Debounce:     p.events.debounce,
		Log:          p.events.log,
	}
}
