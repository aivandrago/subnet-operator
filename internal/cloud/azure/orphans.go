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
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// Orphaned ownership entries (#121). A subnet's ownership is the tag hs-subnet-<subnet name> on
// its virtual network (tags.go), and the operator never removes one: it never deletes. So an
// entry outlives its subnet when the subnet is deleted, when a subnet is created again under
// another name (Azure cannot rename one), and when a create wrote the entry, which goes first
// (writer.go), and the subnet then never came to exist and its claim was given up. Such an
// entry names no subnet and still counts against the 50 tags of the virtual network.
//
// This file finds them, and that is all it does: it reads the virtual networks with their
// subnets and tags as the read identity, with the one listing discovery makes, and reports the
// entries no subnet of the network has the name of. Nothing here writes. Removing an entry is
// left to a person with az (RemoveCommand prints the command), so that "the operator never
// removes a tag" stays true of every line of this package.

// OrphanedEntry is an ownership entry of a virtual network whose subnet does not exist.
type OrphanedEntry struct {
	// Tag is the entry's tag name as Azure spells it, and Subnet the subnet name in it.
	Tag    string `json:"tag"`
	Subnet string `json:"subnet"`
	// Value is the entry as it is written, and Tags the ownership it holds, decoded.
	Value string            `json:"value"`
	Tags  map[string]string `json:"tags,omitempty"`
	// Claim is the SubnetClaim (namespace/name) the entry was written for, from its hs-claim;
	// empty for an entry an import wrote. A create that failed and is still retried looks
	// exactly like this from Azure, which is why the claim is named.
	Claim string `json:"claim,omitempty"`
	// OperatorFormat reports whether the entry reads exactly as the operator writes one
	// (IsOperatorEntry). An entry that does not was written or edited by something else.
	OperatorFormat bool `json:"operatorFormat"`
}

// NetworkOwnershipEntries is a virtual network with what its tags hold: how many of the 50 it
// may carry are used, how many of them are ownership entries, and the entries whose subnet does
// not exist.
type NetworkOwnershipEntries struct {
	// ID is the virtual network's resource ID in lowercase, as Network objects carry it.
	ID            string `json:"networkID"`
	Subscription  string `json:"subscription"`
	ResourceGroup string `json:"resourceGroup"`
	Name          string `json:"name"`
	Location      string `json:"location"`
	// TagCount is the number of tags the virtual network carries, of TagLimit.
	TagCount int `json:"tagCount"`
	TagLimit int `json:"tagLimit"`
	// OwnershipEntries is the number of those tags that are ownership entries.
	OwnershipEntries int `json:"ownershipEntries"`
	// Orphaned are the entries whose subnet does not exist, by tag name.
	Orphaned []OrphanedEntry `json:"orphaned"`
}

// OwnershipEntries reads the virtual networks of the target's subscription (of the resource
// groups its scope names, if it names any) as the target's identity, and reports each with its
// orphaned ownership entries. Only the virtual networks in one of the locations are reported,
// all of them without locations; the target's region is not used. It makes the listing call
// discovery makes and nothing else, so the reader role is all it needs.
func (p *Provider) OwnershipEntries(ctx context.Context, target inventory.Target, locations []string) (
	[]NetworkOwnershipEntries, error) {
	d := p.discoverer
	id, err := identityOf(target)
	if err != nil {
		return nil, err
	}
	c, err := d.clientFor(id, target.Account)
	if err != nil {
		return nil, err
	}
	// A read of its own, always: never the listing a sync shares (listVirtualNetworks).
	vnets, err := d.listSubscription(ctx, c, id, target)
	switch {
	case err == nil:
	case isThrottle(err):
		return nil, fmt.Errorf("%w: list virtual networks: %w", inventory.ErrThrottled, err)
	case !id.Own() && !isCredentialError(err):
		return nil, fmt.Errorf("as %s: list virtual networks: %w", id, err)
	default:
		return nil, fmt.Errorf("list virtual networks: %w", err)
	}
	wanted := map[string]bool{}
	for _, l := range locations {
		wanted[normalLocation(l)] = true
	}
	var out []NetworkOwnershipEntries
	for _, v := range vnets {
		if len(wanted) > 0 && !wanted[normalLocation(deref(v.Location))] {
			continue
		}
		ref, ok := parseResourceID(deref(v.ID))
		if !ok {
			return nil, fmt.Errorf("%q is not a virtual network ID", deref(v.ID))
		}
		tags := map[string]string{}
		for k, value := range v.Tags {
			tags[k] = deref(value)
		}
		n := NetworkOwnershipEntries{
			ID:               canonicalID(deref(v.ID)),
			Subscription:     strings.ToLower(ref.subscription),
			ResourceGroup:    ref.group,
			Name:             deref(v.Name),
			Location:         normalLocation(deref(v.Location)),
			TagCount:         len(tags),
			TagLimit:         maxTagsPerResource,
			OwnershipEntries: len(tags) - len(ownTags(tags)),
			Orphaned:         orphanedEntries(tags, subnetNames(v)),
		}
		out = append(out, n)
	}
	slices.SortFunc(out, func(a, b NetworkOwnershipEntries) int { return cmp.Compare(a.ID, b.ID) })
	return out, nil
}

func subnetNames(v *armnetwork.VirtualNetwork) []string {
	subnets := subnetsOf(v)
	names := make([]string, 0, len(subnets))
	for _, s := range subnets {
		names = append(names, deref(s.Name))
	}
	return names
}

// orphanedEntries are the ownership entries among a virtual network's tags that none of its
// subnets has the name of, by tag name. Azure compares both tag names and subnet names without
// regard to case, so hs-subnet-Apps is the entry of a subnet apps. A tag that is not an entry
// (anything not starting with hs-subnet-, and the bare prefix) is never one of them.
func orphanedEntries(tags map[string]string, subnets []string) []OrphanedEntry {
	exists := map[string]bool{}
	for _, s := range subnets {
		exists[strings.ToLower(s)] = true
	}
	claimKey := networkv1.OperatorTagKeysFor(networkv1.ProviderAzure).Claim
	out := []OrphanedEntry{}
	for name, value := range tags {
		subnet, ok := entrySubnet(name)
		if !ok || exists[strings.ToLower(subnet)] {
			continue
		}
		e := OrphanedEntry{Tag: name, Subnet: subnet, Value: value, OperatorFormat: IsOperatorEntry(name, value)}
		if decoded := DecodeSubnetEntry(value); len(decoded) > 0 {
			e.Tags = decoded
			e.Claim, _ = lookupFold(decoded, claimKey)
		}
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b OrphanedEntry) int { return cmp.Compare(strings.ToLower(a.Tag), strings.ToLower(b.Tag)) })
	return out
}

// entrySubnet is the subnet name in the name of an ownership entry, and whether the tag is one.
func entrySubnet(tag string) (string, bool) {
	if len(tag) <= len(subnetEntryPrefix) || !strings.EqualFold(tag[:len(subnetEntryPrefix)], subnetEntryPrefix) {
		return "", false
	}
	return tag[len(subnetEntryPrefix):], true
}

// IsOperatorEntry reports whether a tag reads exactly as the operator writes a subnet's
// ownership entry: hs-subnet- and a subnet name ARM accepts, with a value that is the encoding
// of the pairs it decodes to (key=value pairs sorted by key, separated by ";", percent-encoded;
// EncodeSubnetEntry), which is true of every value the operator writes, whole or merged into
// an entry that was there. Anything else under the prefix was written or edited by somebody
// else, and is theirs to judge: the operator's tooling lists it and proposes nothing for it.
func IsOperatorEntry(tag, value string) bool {
	subnet, ok := entrySubnet(tag)
	if !ok || !subnetNamePattern.MatchString(subnet) || value == "" || len(value) > maxTagValueLength {
		return false
	}
	return EncodeSubnetEntry(DecodeSubnetEntry(value)) == value
}

// RemoveCommand is the Azure CLI command that removes one ownership entry from its virtual
// network, for a POSIX shell: the Tags API's Delete operation with the entry's name and the
// value it had when it was read. It is printed, never run: the operator removes nothing. It is
// empty for a tag that is not an entry in the operator's own format, which nothing the operator
// ships proposes to remove.
func RemoveCommand(networkID string, e OrphanedEntry) string {
	if !IsOperatorEntry(e.Tag, e.Value) {
		return ""
	}
	return "az tag update --resource-id " + shellQuote(networkID) + " --operation Delete --tags " +
		shellQuote(e.Tag+"="+e.Value)
}

// shellQuote quotes a word for a POSIX shell: in single quotes, where nothing is special but
// the quote itself.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
