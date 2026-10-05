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

// Package azure is the Azure provider: subscriptions, virtual networks and subnets, read with
// the Azure Resource Manager (ARM) REST API through the official SDK (armnetwork). Azure
// subnets cannot carry tags, so a subnet's ownership is an entry on its virtual network, the
// tag hs-subnet-<subnet name> (ADR 0002 §6, tags.go). It creates subnets and writes ownership
// through the Tags API's Merge operation, only ever adding (writer.go, #53). Each subscription
// is reached with the operator's own identity or with read and write identities of its own
// (accounts[].azure, credentials.go, #54). Change events come from Event Grid through a Storage
// queue (package events, changes.go, #55) when a queue is configured.
package azure

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/cloud/azure/events"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

// Identity is how the operator reaches a subscription. The zero value is the operator's own
// identity: the SDK's DefaultAzureCredential, which on Kubernetes is Microsoft Entra Workload
// ID (a federated credential for the operator's service account). A client ID, and the tenant
// it lives in, is the Azure counterpart of an AWS role to assume (accounts[].azure): the
// operator exchanges the same service account token for a token of that identity.
type Identity struct {
	// TenantID is the Microsoft Entra tenant of the application, empty for the operator's own.
	TenantID string
	// ClientID is the application (client) ID to authenticate as, empty for the operator's
	// own identity.
	ClientID string
}

// Own implements inventory.Identity.
func (i Identity) Own() bool { return i.ClientID == "" && i.TenantID == "" }

// String implements inventory.Identity.
func (i Identity) String() string {
	switch {
	case i.Own():
		return "the operator's own credentials"
	case i.TenantID == "":
		return "client " + i.ClientID
	default:
		return "client " + i.ClientID + " in tenant " + i.TenantID
	}
}

// identityOf reads the Azure identity of a target. A target without one uses the operator's
// own credentials; a target carrying another provider's identity is a wiring mistake.
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
		return Identity{}, errors.New("the target's identity is not an Azure identity: " + id.String())
	}
}

// Options configure the provider.
type Options struct {
	// Credential is the operator's own credential. Nil builds the SDK's DefaultAzureCredential
	// at the first discovery: environment variables, Workload Identity
	// (AZURE_FEDERATED_TOKEN_FILE, set by the AKS webhook or by the chart), a managed
	// identity. Tests put a static token here.
	Credential azcore.TokenCredential
	// Cloud is the Azure cloud: its Resource Manager endpoint and audience and its Microsoft
	// Entra authority host (Clouds, --azure-cloud). Empty is the public cloud.
	Cloud cloud.Configuration
	// AuthorityHost replaces the cloud's Microsoft Entra authority host
	// (https://login.microsoftonline.com/ in the public cloud), for every identity.
	AuthorityHost string
	// TenantID is the tenant of accounts' identities that name none. Empty is AZURE_TENANT_ID,
	// the operator's own tenant.
	TenantID string
	// FederatedTokenFile is the service account token the operator exchanges for tokens of
	// accounts' identities. Empty is AZURE_FEDERATED_TOKEN_FILE.
	FederatedTokenFile string
	// DisableInstanceDiscovery skips Microsoft Entra instance discovery before a token
	// request, which only a private cloud such as Azure Stack, or a fake, needs.
	DisableInstanceDiscovery bool
	// ResourceManagerEndpoint replaces the cloud's ARM endpoint (https://management.azure.com
	// in the public cloud): tests point it at the in-repo fake (azurefake).
	ResourceManagerEndpoint string
	// Transport replaces the SDK's HTTP client, for tests.
	Transport policy.Transporter
	// PollInterval replaces the wait between the polls of a long-running operation (creating a
	// subnet), which is otherwise what ARM's Retry-After asks for, or 5 s: tests shorten it.
	PollInterval time.Duration
	// OnThrottle is told about every ARM attempt that was rate-limited, including the ones a
	// retry then got through.
	OnThrottle func(target inventory.Target, operation string)
	Log        logr.Logger
	// EventsQueueURL is the Storage queue (https://<account>.queue.core.windows.net/<queue>) an
	// Event Grid subscription fills with the resource events of virtual networks; empty for
	// none. The operator reads it as its own identity.
	EventsQueueURL string
	// EventsDebounce is how long events are collected before the changed targets resync.
	EventsDebounce time.Duration
	// EventsPollInterval replaces the wait after the queue was found empty: tests shorten it.
	EventsPollInterval time.Duration
	// EventsLog is the change event source's logger.
	EventsLog logr.Logger
}

// Provider is the Azure provider.
type Provider struct {
	discoverer *Discoverer
}

var _ provider.Provider = (*Provider)(nil)

// NewProvider returns the Azure provider. Nothing is called until the first discovery, so it
// starts without credentials; a discovery without them fails and says so in the scope.
func NewProvider(opts Options) *Provider {
	return &Provider{discoverer: newDiscoverer(opts)}
}

// NewFromEnvironment builds the provider with the operator's own Azure credentials, which are
// looked up at the first discovery. A change event queue that cannot be one stops the start,
// like a malformed Pub/Sub subscription does on GCP: left alone it would only ever fail.
func NewFromEnvironment(_ context.Context, opts Options) (*Provider, error) {
	if opts.EventsQueueURL != "" {
		if err := events.CheckQueueURL(opts.EventsQueueURL); err != nil {
			return nil, fmt.Errorf("change events queue: %w", err)
		}
	}
	return NewProvider(opts), nil
}

// Name implements provider.Provider.
func (p *Provider) Name() networkv1.Provider { return networkv1.ProviderAzure }

// Capabilities implements provider.Provider. ARM reports the addresses used per subnet (the
// virtual network's usages), and subnets are created with their ownership entry; change events
// only with a queue to read them from.
func (p *Provider) Capabilities() []networkv1.Capability {
	caps := []networkv1.Capability{networkv1.CapabilityCreateSubnet, networkv1.CapabilityIPUsage}
	if p.discoverer.opts.EventsQueueURL != "" {
		caps = append(caps, networkv1.CapabilityChangeEvents)
	}
	return caps
}

// Ownership implements provider.Provider: a virtual network carries its own tags; a subnet
// cannot, and its ownership is an entry on its virtual network (ADR 0002 §6).
func (p *Provider) Ownership() networkv1.Ownership {
	return networkv1.Ownership{
		Networks: networkv1.OwnershipResourceTags,
		Subnets:  networkv1.OwnershipParentNetworkTags,
	}
}

// Identity implements provider.Provider. Reads go through the account's azure.clientID and
// writes through its azure.writeClientID, both in azure.tenantID; either is the operator's own
// identity when empty. IDs are used in lowercase, so that one identity is one credential and
// one quota bucket however the scope spells it.
func (p *Provider) Identity(account networkv1.Account, access provider.Access) inventory.Identity {
	a := account.Azure
	if a == nil {
		return Identity{}
	}
	client := a.ClientID
	if access == provider.Write {
		client = a.WriteClientID
	}
	if client == "" {
		return Identity{}
	}
	return Identity{TenantID: strings.ToLower(a.TenantID), ClientID: strings.ToLower(client)}
}

// WriteIdentityField implements provider.Provider: the write identity (ADR 0002 §3).
func (p *Provider) WriteIdentityField() string { return "azure.writeClientID" }

// OwnershipPermission implements provider.Provider: writing tags through the Tags API, which
// the Tag Contributor role grants without write access to the virtual network itself.
func (p *Provider) OwnershipPermission() string { return "Microsoft.Resources/tags/write" }

// ReplacesOwnershipValues implements provider.Provider. The Tags API's Merge operation would
// replace the value of a name the virtual network carries, but the operator never overwrites
// an ownership value: a write reads the tags first and refuses the whole write with
// inventory.ErrOwnershipConflict, as on GCP (writer.go).
func (p *Provider) ReplacesOwnershipValues() bool { return false }

// Discover implements inventory.Discoverer.
func (p *Provider) Discover(ctx context.Context, target inventory.Target) (*inventory.Snapshot, error) {
	return p.discoverer.Discover(ctx, target)
}

// CreateSubnet implements inventory.SubnetWriter: a subnet of the virtual network, after its
// ownership entry, as the target's write identity.
func (p *Provider) CreateSubnet(ctx context.Context, target inventory.Target, req inventory.CreateSubnetRequest) (string, error) {
	return p.discoverer.CreateSubnet(ctx, target, req)
}

// WriteOwnership implements inventory.OwnershipWriter: the virtual network's own tags, or a
// subnet's entry on its virtual network, only ever added, as the target's write identity.
func (p *Provider) WriteOwnership(ctx context.Context, target inventory.Target, resourceID string, tags map[string]string) error {
	return p.discoverer.WriteOwnership(ctx, target, resourceID, tags)
}

// Events implements provider.Provider: the Storage queue poller, when a queue is configured.
// Without one the inventory follows the resync interval.
func (p *Provider) Events(sink provider.EventSink) manager.Runnable {
	d := p.discoverer
	if d.opts.EventsQueueURL == "" {
		return nil
	}
	return &events.Poller{
		Connect: events.StorageQueue(d.opts.EventsQueueURL, func() (azcore.TokenCredential, error) {
			return d.credentialFor(Identity{})
		}, d.azcoreOptions()),
		QueueURL:     d.opts.EventsQueueURL,
		Sink:         sink,
		Locate:       d.locate,
		Debounce:     d.opts.EventsDebounce,
		PollInterval: d.opts.EventsPollInterval,
		Log:          d.opts.EventsLog,
	}
}
