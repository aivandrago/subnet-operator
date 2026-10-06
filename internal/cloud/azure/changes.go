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

package azure

import (
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/cloud/azure/events"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// How a change event becomes a target (#55). An Event Grid resource event names the resource
// that was written and nothing else: no location, and a target is a subscription and a
// location. Asking ARM where the network is would spend the read quota discovery lives on,
// once per event, and could not answer for a network that was just deleted. Discovery already
// lists every virtual network of a subscription with its location, on every sync, so the last
// listing is what an event is looked up in:
//
//   - a network the last listing saw resyncs its own location;
//   - a network it did not see (created since, or outside the scope's resource groups) resyncs
//     every location of the subscription the scopes name, which is what a GCP network event
//     does: the locations of a scope are resynced together and share one listing (#118);
//   - a subscription no discovery was asked for is none of the operator's, and its events are
//     ignored.
//
// The memory is written once per listing, not once per target: the targets of a sync that share
// a listing (listVirtualNetworks) are told about the same networks, and the one that listed has
// remembered them before any of them returns. A listing that failed remembers nothing, so what
// was known stays.
//
// The memory is the leader's own and is lost with it; a new leader resyncs whole subscriptions
// until its first discoveries have run, which they do at once.

// watch remembers that the subscription is one the operator discovers.
func (d *Discoverer) watch(subscription string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.watched[networkv1.CanonicalAccountID(networkv1.ProviderAzure, subscription)] = true
}

// remember records where the listed virtual networks are. A listing of the whole subscription
// replaces what was known of it, so a deleted network is forgotten; a listing of resource
// groups only adds to it.
func (d *Discoverer) remember(subscription string, vnets []*armnetwork.VirtualNetwork, all bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	subscription = networkv1.CanonicalAccountID(networkv1.ProviderAzure, subscription)
	known := d.networks[subscription]
	if known == nil || all {
		known = make(map[string]string, len(vnets))
		d.networks[subscription] = known
	}
	for _, v := range vnets {
		if id, location := canonicalID(deref(v.ID)), normalLocation(deref(v.Location)); id != "" && location != "" {
			known[id] = location
		}
	}
}

// locate implements events.Locate.
func (d *Discoverer) locate(c events.Change) (inventory.TargetKey, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	subscription := networkv1.CanonicalAccountID(networkv1.ProviderAzure, c.Subscription)
	if !d.watched[subscription] {
		return inventory.TargetKey{}, false
	}
	id := canonicalID(c.NetworkID)
	key := inventory.TargetKey{Account: subscription, Region: d.networks[subscription][id]}
	if c.NetworkDeleted {
		// A network of the same name may come back somewhere else; where this one was says
		// nothing about that one.
		delete(d.networks[subscription], id)
	}
	return key, true
}
