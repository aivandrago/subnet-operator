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

// Package provider is the seam between the cloud-neutral core and the clouds. A Provider
// bundles everything the controllers and webhooks need from one cloud: how an account is
// reached, what the cloud's IDs, zones and tags must look like, discovery, subnet creation,
// ownership writes and an optional change-event source. The operator builds a Registry of the
// providers it runs with, and a NetworkScope selects one by spec.provider (ADR 0002).
//
// Every implementation must pass the contract suite in providertest.
package provider

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// Access says what an identity is used for. Reading and writing are separate identities on
// every provider, so the read identity never carries write permissions (ADR 0002 §3).
type Access int

const (
	// Read is discovery.
	Read Access = iota
	// Write is creating subnets and writing ownership metadata.
	Write
)

// Provider is one cloud.
type Provider interface {
	// Name is the value of NetworkScope.spec.provider the provider serves.
	Name() networkv1.Provider
	// Capabilities lists what the provider can do as the operator runs it; it is reported on
	// NetworkScope.status.capabilities. CreateSubnet must be listed exactly when CreateSubnet
	// does not return ErrNotSupported.
	Capabilities() []networkv1.Capability
	// Ownership says where the provider keeps ownership metadata (ADR 0002 §6).
	Ownership() networkv1.Ownership

	// Identity returns what the provider reaches an account with, for reading or for writing.
	// An account without the provider's member, or with an empty one, is reached with the
	// operator's own identity.
	Identity(account networkv1.Account, access Access) inventory.Identity
	// WriteIdentityField names the field of an account entry that holds the write identity,
	// such as aws.writeRoleARN, for messages that tell somebody what to set.
	WriteIdentityField() string
	// OwnershipPermission names the one permission a write identity needs to write ownership
	// (on AWS ec2:CreateTags), for messages: an identity for imports needs nothing else.
	OwnershipPermission() string
	// ReplacesOwnershipValues reports whether an ownership write replaces the value of a key
	// the resource already carries (AWS CreateTags does), or refuses the whole write with
	// inventory.ErrOwnershipConflict (a GCP tag binding would have to be deleted first, and the
	// operator never removes anything).
	ReplacesOwnershipValues() bool

	// ValidateScope checks what a scope of this provider must look like beyond its schema:
	// region names, identities that belong to their account. Warnings do not refuse the scope.
	ValidateScope(scope *networkv1.NetworkScope) (warnings []string, errs field.ErrorList)
	// ValidateClaim checks the provider-specific shape of a claim: account, network ID, zones,
	// prefix length.
	ValidateClaim(claim *networkv1.SubnetClaim) field.ErrorList
	// ValidateImport checks the provider-specific shape of an import: account, region,
	// resource ID.
	ValidateImport(imp *networkv1.ResourceImport) field.ErrorList
	// ValidateTags reports tag keys and values the provider would refuse to write.
	ValidateTags(path *field.Path, tags map[string]string) field.ErrorList
	// ClaimRefusal is what the claim controller reports when ValidateClaim would have refused
	// a claim that got past a webhook that was not running: a condition reason and a message,
	// or two empty strings.
	ClaimRefusal(claim *networkv1.SubnetClaim) (reason, message string)
	// ImportRefusal is ClaimRefusal for imports.
	ImportRefusal(imp *networkv1.ResourceImport) (reason, message string)

	inventory.Discoverer
	inventory.SubnetWriter
	inventory.OwnershipWriter

	// Events returns the runnable that delivers the provider's change events to sink, or nil
	// when no event source is configured. Only the leader should run it.
	Events(sink EventSink) manager.Runnable
}

// ImportLocator is implemented by a provider on which an import cannot be trusted to name
// something that exists, and whose resources are in a region only the cloud knows. That is
// Azure: a subnet's ownership is an entry on its virtual network, which the Tags API writes
// whether or not the subnet exists, and a resource ID names no location. On such a provider an
// import's region is its network's: the webhooks fill spec.region in and check it from the
// discovered network, and the import controller reads the resource before anything is written.
//
// AWS and GCP do not implement it. Their tag writes name the resource itself, so the cloud
// refuses a resource that does not exist, and the region is required (AWS) or part of the
// resource ID (GCP).
type ImportLocator interface {
	// ImportNetwork splits the resource ID of an import into the ID of the network it names or
	// belongs to, in the spelling the inventory uses, and the subnet's name, empty for a
	// network. ok is false for an ID that is not one of the provider's.
	ImportNetwork(resourceID string) (networkID, subnet string, ok bool)
	// LocateImport reads the network or subnet from the cloud and returns the region it is in.
	// The target's Identity is the read identity: it is a read, and a dry run, which reports
	// the same, may have no write identity at all. A network, or a subnet of it, that does not
	// exist is inventory.ErrResourceNotFound; a call still rate-limited after the retries wraps
	// inventory.ErrThrottled.
	LocateImport(ctx context.Context, target inventory.Target, resourceID string) (region string, err error)
}

// EventSink receives what a provider's change events report.
type EventSink struct {
	// Changed is told which account/region pairs changed; they are resynced. A pair without a
	// region stands for every region of the account: a change to a global resource, such as a
	// GCP network.
	Changed func(ctx context.Context, changed []inventory.TargetKey) error
	// Created is told which resources were just created, and by whom.
	Created func(ctx context.Context, created []inventory.Creation)
	// Received, when set, is told what became of every event read from the source: one of
	// EventResync, EventIgnored and EventMalformed.
	Received func(result string)
	// ReceiveFailed, when set, is told about every failed attempt to read from the source. The
	// source retries with backoff on its own.
	ReceiveFailed func()
}

// What became of a change event, as EventSink.Received is told.
const (
	// EventResync is an event that reported a change: its target is resynced.
	EventResync = "resync"
	// EventIgnored is an event about something the inventory does not show, or a failed call.
	EventIgnored = "ignored"
	// EventMalformed is an event that could not be read, or not mapped to a target. It is
	// dropped (deleted from the queue, acknowledged), since it would never read better.
	EventMalformed = "malformed"
)

// EventResults are the values of EventSink.Received's result.
var EventResults = []string{EventResync, EventIgnored, EventMalformed}

// HasCapability reports whether the provider lists the capability.
func HasCapability(p Provider, c networkv1.Capability) bool {
	return slices.Contains(p.Capabilities(), c)
}

// MissingWriteIdentity reports whether an account of the scope is reached with an identity
// of its own for reading but has no write identity. Such an account is not the one the
// operator runs in, so the operator's own identity cannot write there either.
func MissingWriteIdentity(p Provider, scope *networkv1.NetworkScope, accountID string) bool {
	account, ok := scope.Account(accountID)
	if !ok {
		return false
	}
	return !ownIdentity(p.Identity(account, Read)) && ownIdentity(p.Identity(account, Write))
}

func ownIdentity(id inventory.Identity) bool {
	return id == nil || id.Own()
}

// Registry holds the providers the operator runs with, by name.
type Registry struct {
	byName map[networkv1.Provider]Provider
	names  []networkv1.Provider
}

// NewRegistry registers the providers. Two providers of the same name are a programming error.
func NewRegistry(providers ...Provider) (*Registry, error) {
	r := &Registry{byName: map[networkv1.Provider]Provider{}}
	for _, p := range providers {
		name := p.Name()
		if name == "" {
			return nil, fmt.Errorf("provider %T has no name", p)
		}
		if _, dup := r.byName[name]; dup {
			return nil, fmt.Errorf("provider %q is registered twice", name)
		}
		r.byName[name] = p
		r.names = append(r.names, name)
	}
	slices.Sort(r.names)
	return r, nil
}

// MustRegistry is NewRegistry for tests and fixed wiring; it panics on a duplicate.
func MustRegistry(providers ...Provider) *Registry {
	r, err := NewRegistry(providers...)
	if err != nil {
		panic(err)
	}
	return r
}

// Get returns the provider of that name. A nil registry has none.
func (r *Registry) Get(name networkv1.Provider) (Provider, bool) {
	if r == nil {
		return nil, false
	}
	p, ok := r.byName[name]
	return p, ok
}

// All returns the registered providers, sorted by name.
func (r *Registry) All() []Provider {
	if r == nil {
		return nil
	}
	out := make([]Provider, 0, len(r.names))
	for _, n := range r.names {
		out = append(out, r.byName[n])
	}
	return out
}

// NotEnabled is the message for a scope whose provider is not registered.
func (r *Registry) NotEnabled(name networkv1.Provider) string {
	var enabled []string
	if r != nil {
		for _, n := range r.names {
			enabled = append(enabled, string(n))
		}
	}
	list := "none"
	if len(enabled) > 0 {
		list = strings.Join(enabled, ", ")
	}
	return fmt.Sprintf("provider %q is not enabled in this operator (enabled: %s)", name, list)
}
