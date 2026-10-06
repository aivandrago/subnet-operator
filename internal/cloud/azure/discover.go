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
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"golang.org/x/sync/errgroup"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// ReservedIPs is the number of addresses Azure reserves in every IPv4 address prefix of a
// subnet: the network address, the default gateway, two for Azure DNS and the broadcast
// address.
const ReservedIPs = 5

// usageReadConcurrency caps the usage reads of one discovery: ARM answers one virtual network
// per call.
const usageReadConcurrency = 8

// Where availableIPs of a subnet came from (AzureSubnetStatus.ipUsageSource).
const (
	// UsageFromVirtualNetworkUsage is the virtual network's usages: ARM's own count per subnet.
	UsageFromVirtualNetworkUsage = "VirtualNetworkUsage"
	// UsageFromIPConfigurations is the subnet's usable addresses less its IP configurations,
	// for a subnet the usages leave out.
	UsageFromIPConfigurations = "IPConfigurations"
)

// usage is ARM's count of a subnet's addresses: currentValue used of limit. Either is -1 when
// ARM does not know (it says so for gateway subnets).
type usage struct {
	current, limit float64
}

// discoverTarget reads the subscription's virtual networks, with their subnets and tags (one
// paged List All call per subscription, or one paged List call per resource group when the
// scope names resource groups; once per sync, whatever the number of its locations in the
// scope: listVirtualNetworks), keeps those in the target's location, and reads the usages of
// each virtual network discovery reports (one paged call each). Tag names are matched without
// regard to case (tagNamer). Like on AWS and GCP, the networks the selector leaves out are
// reported separately with their subnets when DiscoverUnmanaged is set.
//
// A target with no virtual network in its location at all has its location checked
// (locations.go): one that does not exist fails the target, one that is merely empty does not.
//
// The listing is shared with the other targets of the sync: nothing here may change it.
func (d *Discoverer) discoverTarget(ctx context.Context, c *armnetwork.VirtualNetworksClient, id Identity,
	target inventory.Target) (*inventory.Snapshot, error) {
	vnets, err := d.listVirtualNetworks(ctx, c, id, target)
	if err != nil {
		return nil, fmt.Errorf("list virtual networks: %w", err)
	}
	located := slices.ContainsFunc(vnets, func(v *armnetwork.VirtualNetwork) bool { return inLocation(v, target) })
	snap := &inventory.Snapshot{}
	if !located {
		warning, err := d.checkLocation(ctx, id, target)
		if err != nil {
			return nil, err
		}
		if warning != "" {
			snap.Warnings = append(snap.Warnings, warning)
		}
		return snap, nil
	}

	unmanagedWanted := target.DiscoverUnmanaged && len(target.NetworkSelector) > 0
	namer := newTagNamer(target)
	type found struct {
		vnet    *armnetwork.VirtualNetwork
		ref     *arm.ResourceID
		network inventory.Network
		entries map[string]map[string]string
		managed bool
		usages  map[string]usage
	}
	var wanted []*found
	for _, v := range vnets {
		if !inLocation(v, target) {
			continue
		}
		ref, err := arm.ParseResourceID(deref(v.ID))
		if err != nil {
			return nil, fmt.Errorf("virtual network %q: %w", deref(v.ID), err)
		}
		own, entries := namer.networkTags(v.Tags)
		f := &found{vnet: v, ref: ref, entries: entries,
			network: networkOf(target, v, ref, own, len(v.Tags), len(entries))}
		switch {
		case namer.matchesSelector(own, target.NetworkSelector):
			f.managed = true
		case !unmanagedWanted:
			continue
		}
		wanted = append(wanted, f)
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(usageReadConcurrency)
	for _, f := range wanted {
		if len(subnetsOf(f.vnet)) == 0 {
			continue
		}
		g.Go(func() error {
			u, err := d.listUsages(gctx, c, id, target, f.ref)
			if err != nil {
				return fmt.Errorf("list usages of virtual network %s: %w", f.ref.Name, err)
			}
			f.usages = u
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	for _, f := range wanted {
		var subnets []inventory.Subnet
		for _, s := range subnetsOf(f.vnet) {
			subnets = append(subnets, subnetOf(f.network, f.ref, s, f.entries, f.usages))
		}
		if f.managed {
			snap.Networks = append(snap.Networks, f.network)
			snap.Subnets = append(snap.Subnets, subnets...)
		} else {
			snap.UnmanagedNetworks = append(snap.UnmanagedNetworks, f.network)
			snap.UnmanagedSubnets = append(snap.UnmanagedSubnets, subnets...)
		}
	}
	return snap, nil
}

// inLocation reports whether the virtual network is in the target's location.
func inLocation(v *armnetwork.VirtualNetwork, target inventory.Target) bool {
	return strings.EqualFold(normalLocation(deref(v.Location)), normalLocation(target.Region))
}

// listingKey tells the listings of a sync apart (inventory.Shared): a listing is the same for
// every location of a subscription, and for nobody else. Not for another scope, whose sync is
// its own in any case; not for another identity, which may see other resource groups and has a
// read quota of its own; not for other resource groups.
type listingKey struct {
	scope, subscription string
	identity            Identity
	// groups are the scope's resource groups in lowercase, each followed by a slash, which no
	// group name contains.
	groups string
}

func listingKeyOf(id Identity, target inventory.Target) listingKey {
	key := listingKey{scope: target.Scope, identity: id,
		subscription: networkv1.CanonicalAccountID(networkv1.ProviderAzure, target.Account)}
	for _, g := range resourceGroups(target) {
		key.groups += strings.ToLower(g) + "/"
	}
	return key
}

// listVirtualNetworks returns the virtual networks of the target's subscription, in every
// location: ARM has no location filter, so what the target wants is what every other location
// of the subscription wants too (#118). The targets of one sync therefore share one listing:
// the first to get here lists, the others wait for it and get the same networks, or the same
// error, throttling included, which each then reports as its own. The listing lives as long as
// the sync and no longer (inventory.Shared), so the next sync, the resync a change event asks
// for and the retry of a throttled target each list again. A discovery that is not part of a
// sync lists for itself.
func (d *Discoverer) listVirtualNetworks(ctx context.Context, c *armnetwork.VirtualNetworksClient, id Identity,
	target inventory.Target) ([]*armnetwork.VirtualNetwork, error) {
	return inventory.Shared(ctx, listingKeyOf(id, target),
		func(ctx context.Context) ([]*armnetwork.VirtualNetwork, error) {
			return d.listSubscription(ctx, c, id, target)
		})
}

// listSubscription lists the virtual networks of the target's subscription. Without resource
// groups in the scope it is one List All call per subscription, which is cheaper than one per
// resource group; with them, one List call per resource group, which needs read access to those
// groups only. A throttled page starts that listing over. A resource group that does not exist
// fails the target, like a subscription that does not: it is a typo or a group deleted, and the
// scope should say so.
func (d *Discoverer) listSubscription(ctx context.Context, c *armnetwork.VirtualNetworksClient, id Identity,
	target inventory.Target) ([]*armnetwork.VirtualNetwork, error) {
	groups := resourceGroups(target)
	if len(groups) == 0 {
		vnets, err := listPaged(ctx, d, target, id, "virtualNetworks.listAll", func() *runtime.Pager[armnetwork.VirtualNetworksClientListAllResponse] {
			return c.NewListAllPager(nil)
		}, func(r armnetwork.VirtualNetworksClientListAllResponse) []*armnetwork.VirtualNetwork { return r.Value })
		if err == nil {
			d.remember(target.Account, vnets, true)
		}
		return vnets, err
	}
	var out []*armnetwork.VirtualNetwork
	for _, group := range groups {
		vnets, err := listPaged(ctx, d, target, id, "virtualNetworks.list", func() *runtime.Pager[armnetwork.VirtualNetworksClientListResponse] {
			return c.NewListPager(group, nil)
		}, func(r armnetwork.VirtualNetworksClientListResponse) []*armnetwork.VirtualNetwork { return r.Value })
		if err != nil {
			return nil, fmt.Errorf("resource group %s: %w", group, err)
		}
		out = append(out, vnets...)
	}
	d.remember(target.Account, out, false)
	return out, nil
}

// listPaged runs one paged listing as one ARM call of the pacing: a pager cannot resume after
// an error, so a throttled page starts it over.
func listPaged[R any](ctx context.Context, d *Discoverer, target inventory.Target, id Identity, operation string,
	newPager func() *runtime.Pager[R], values func(R) []*armnetwork.VirtualNetwork) ([]*armnetwork.VirtualNetwork, error) {
	var out []*armnetwork.VirtualNetwork
	err := d.call(ctx, target, armBucket(id, target.Account), operation,
		func(ctx context.Context) (*http.Response, error) {
			out = nil
			var resp *http.Response
			pager := newPager()
			for pager.More() {
				page, err := pager.NextPage(policy.WithCaptureResponse(ctx, &resp))
				if err != nil {
					return resp, err
				}
				out = append(out, values(page)...)
			}
			return resp, nil
		})
	return out, err
}

// resourceGroups are the resource groups the target's scope limits discovery to, each once
// whatever its case (ARM compares them without regard to case), in the order the scope lists
// them; none for the whole subscription.
func resourceGroups(target inventory.Target) []string {
	if target.Azure == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, g := range target.Azure.ResourceGroups {
		if g == "" || seen[strings.ToLower(g)] {
			continue
		}
		seen[strings.ToLower(g)] = true
		out = append(out, g)
	}
	return out
}

// listUsages reads ARM's address counts for every subnet of a virtual network, by lowercase
// subnet ID.
func (d *Discoverer) listUsages(ctx context.Context, c *armnetwork.VirtualNetworksClient, id Identity,
	target inventory.Target, vnet *arm.ResourceID) (map[string]usage, error) {
	var out map[string]usage
	err := d.call(ctx, target, armBucket(id, target.Account), "virtualNetworks.listUsage",
		func(ctx context.Context) (*http.Response, error) {
			out = map[string]usage{}
			var resp *http.Response
			pager := c.NewListUsagePager(vnet.ResourceGroupName, vnet.Name, nil)
			for pager.More() {
				page, err := pager.NextPage(policy.WithCaptureResponse(ctx, &resp))
				if err != nil {
					return resp, err
				}
				for _, u := range page.Value {
					if u == nil || u.ID == nil || u.CurrentValue == nil || u.Limit == nil {
						continue
					}
					out[strings.ToLower(*u.ID)] = usage{current: *u.CurrentValue, limit: *u.Limit}
				}
			}
			return resp, nil
		})
	return out, err
}

// matchesSelector reports whether the tags, named by the namer, satisfy every entry of the
// selector: a name is matched without regard to case, a value exactly. An empty value matches
// any value of the key. The selector is applied to the network's own tags, not to its subnets'
// entries.
func (n tagNamer) matchesSelector(tags, selector map[string]string) bool {
	for k, want := range selector {
		got, ok := tags[n.name(k)]
		if !ok || (want != "" && got != want) {
			return false
		}
	}
	return true
}

// normalLocation is a location as ARM spells it in resource IDs and lists: lowercase, without
// spaces ("West Europe" is westeurope).
func normalLocation(location string) string {
	return strings.ToLower(strings.ReplaceAll(location, " ", ""))
}

// canonicalID is the ID the inventory uses for an ARM resource: the ID in lowercase. ARM IDs
// are case-insensitive, and not every API spells them alike (resourceGroups or resourcegroups,
// the group's own case), so object names, labels and the IDs of claims and imports compare
// them in one spelling. It is networkv1.CanonicalResourceID, which the webhooks and the
// controllers bring a claim's and an import's IDs into, so there is one such spelling.
func canonicalID(id string) string {
	return networkv1.CanonicalResourceID(networkv1.ProviderAzure, id)
}

func networkOf(target inventory.Target, v *armnetwork.VirtualNetwork, ref *arm.ResourceID, tags map[string]string,
	tagCount, entries int) inventory.Network {
	n := inventory.Network{
		ID:      canonicalID(deref(v.ID)),
		Account: target.Account,
		Region:  target.Region,
		Name:    deref(v.Name),
		Tags:    tags,
		Azure: &networkv1.AzureNetworkStatus{
			ResourceGroup:          ref.ResourceGroupName,
			TagCount:               int32(tagCount),
			SubnetOwnershipEntries: int32(entries),
		},
	}
	if p := v.Properties; p != nil {
		if p.ProvisioningState != nil {
			n.State = string(*p.ProvisioningState)
		}
		if p.AddressSpace != nil {
			n.CIDRBlocks, n.IPv6CIDRBlocks = splitFamilies(p.AddressSpace.AddressPrefixes)
		}
	}
	return n
}

func subnetsOf(v *armnetwork.VirtualNetwork) []*armnetwork.Subnet {
	if v.Properties == nil {
		return nil
	}
	var out []*armnetwork.Subnet
	for _, s := range v.Properties.Subnets {
		if s != nil && s.ID != nil {
			out = append(out, s)
		}
	}
	return out
}

// subnetOf builds a subnet of the network. Its addresses: every IPv4 address prefix less the
// five Azure reserves in each; free, ARM's usage limit less its current value (both on the
// same footing, whether or not they count the reserved addresses), or, for a subnet the usages
// leave out and no service manages, the usable addresses less its IP configurations.
func subnetOf(network inventory.Network, ref *arm.ResourceID, s *armnetwork.Subnet,
	entries map[string]map[string]string, usages map[string]usage) inventory.Subnet {
	tags, source := subnetTags(deref(s.Name), network.Tags, entries)
	subnet := inventory.Subnet{
		ID:              canonicalID(deref(s.ID)),
		NetworkID:       network.ID,
		Account:         network.Account,
		Region:          network.Region,
		Name:            deref(s.Name),
		OwnershipSource: source,
		Tags:            tags,
		Azure:           &networkv1.AzureSubnetStatus{ResourceGroup: ref.ResourceGroupName},
	}
	p := s.Properties
	if p == nil {
		p = &armnetwork.SubnetPropertiesFormat{}
	}
	if p.ProvisioningState != nil {
		subnet.State = string(*p.ProvisioningState)
	}
	prefixes := p.AddressPrefixes
	if len(prefixes) == 0 && p.AddressPrefix != nil {
		prefixes = []*string{p.AddressPrefix}
	}
	v4, v6 := splitFamilies(prefixes)
	subnet.IPv6CIDRBlocks = v6
	if len(v4) > 0 {
		subnet.CIDRBlock = v4[0]
		subnet.SecondaryCIDRBlocks = v4[1:]
	}
	if len(subnet.SecondaryCIDRBlocks) == 0 {
		subnet.SecondaryCIDRBlocks = nil
	}

	az := subnet.Azure
	for _, dl := range p.Delegations {
		if dl != nil && dl.Properties != nil && dl.Properties.ServiceName != nil {
			az.Delegations = append(az.Delegations, *dl.Properties.ServiceName)
		}
	}
	for _, se := range p.ServiceEndpoints {
		if se != nil && se.Service != nil {
			az.ServiceEndpoints = append(az.ServiceEndpoints, *se.Service)
		}
	}
	if p.RouteTable != nil {
		az.RouteTableID = deref(p.RouteTable.ID)
	}
	if p.NetworkSecurityGroup != nil {
		az.NetworkSecurityGroupID = deref(p.NetworkSecurityGroup.ID)
	}
	if p.NatGateway != nil {
		az.NATGatewayID = deref(p.NatGateway.ID)
	}
	az.IPConfigurations = int32(len(p.IPConfigurations))
	az.ServiceManaged = len(p.Delegations) > 0 || len(p.ServiceAssociationLinks) > 0 ||
		len(p.ResourceNavigationLinks) > 0

	if len(v4) == 0 {
		// IPv6 only: no IPv4 addresses at all, which is known.
		subnet.TotalIPs, subnet.AvailableIPs = new(int64(0)), new(int64(0))
		return subnet
	}
	var total int64
	for _, prefix := range v4 {
		total += inventory.UsableIPv4(prefix, ReservedIPs)
	}
	subnet.TotalIPs = &total
	u, reported := usages[strings.ToLower(deref(s.ID))]
	switch {
	case reported && u.current >= 0 && u.limit >= 0:
		subnet.AvailableIPs = new(min(max(int64(u.limit-u.current), 0), total))
		az.IPUsageSource = UsageFromVirtualNetworkUsage
	case reported:
		// ARM says it does not know (-1), as it does for gateway subnets: unknown it stays.
	case !az.ServiceManaged:
		subnet.AvailableIPs = new(min(max(total-int64(len(p.IPConfigurations)), 0), total))
		az.IPUsageSource = UsageFromIPConfigurations
	}
	return subnet
}

// splitFamilies sorts address prefixes into IPv4 and IPv6, in their order. A prefix that does
// not parse is left out.
func splitFamilies(prefixes []*string) (v4, v6 []string) {
	for _, p := range prefixes {
		if p == nil {
			continue
		}
		prefix, err := netip.ParsePrefix(*p)
		switch {
		case err != nil:
		case prefix.Addr().Is4():
			v4 = append(v4, *p)
		default:
			v6 = append(v6, *p)
		}
	}
	return v4, v6
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}
