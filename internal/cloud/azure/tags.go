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
	"maps"
	"slices"
	"strings"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// Ownership of Azure subnets (ADR 0002 §6). A subnet cannot carry tags, so its virtual network
// carries one tag per subnet that has ownership of its own:
//
//	hs-subnet-<subnet name> = hs-owner=payments;hs-env=prod;hs-tier=db
//
// The value holds the subnet's tags as key=value pairs, sorted by key and separated by ";",
// with "%", ";" and "=" percent-encoded in keys and values, so every key round-trips: the
// scope's tagKeys, requiredSubnetTags and an import's tags are arbitrary. A subnet name (1-80
// letters, digits, "_", "." and "-") is always valid in a tag name, and both are
// case-insensitive, so the entry is found whatever the case of either, and so are the names
// inside it (tagNamer). The value must fit the 256 characters of an Azure tag value, which #53
// checks before it writes one; #53 writes the names in the operator's own spelling, lowercase
// for its default keys and the hs-subnet- prefix.
//
// A subnet without an entry inherits its virtual network's tags and reports ownershipSource
// Network. A subnet with one reports Subnet: its tags are the network's with the entry's laid
// over them key by key, so an entry only needs what differs from the network. The entries are
// left out of the network's own tags.

// subnetEntryPrefix starts the name of a subnet's entry on its virtual network.
var subnetEntryPrefix = networkv1.OperatorTagKeysFor(networkv1.ProviderAzure).SubnetEntryPrefix

// SubnetEntryName is the name of the tag that holds a subnet's ownership on its virtual
// network.
func SubnetEntryName(subnet string) string {
	return subnetEntryPrefix + subnet
}

// entryEscaper percent-encodes what separates the pairs of an entry, and "%" itself.
var entryEscaper = strings.NewReplacer("%", "%25", ";", "%3B", "=", "%3D")

// entryUnescaper undoes entryEscaper. "%25" goes last, so an escaped "%3B" stays "%3B".
var entryUnescaper = strings.NewReplacer("%3B", ";", "%3b", ";", "%3D", "=", "%3d", "=", "%25", "%")

// EncodeSubnetEntry is the value of a subnet's entry for its tags.
func EncodeSubnetEntry(tags map[string]string) string {
	pairs := make([]string, 0, len(tags))
	for _, k := range slices.Sorted(maps.Keys(tags)) {
		pairs = append(pairs, entryEscaper.Replace(k)+"="+entryEscaper.Replace(tags[k]))
	}
	return strings.Join(pairs, ";")
}

// DecodeSubnetEntry reads the tags of a subnet from its entry. A pair without "=" or with an
// empty key is skipped: an entry somebody edited by hand must not hide the rest of it.
func DecodeSubnetEntry(value string) map[string]string {
	out := map[string]string{}
	for pair := range strings.SplitSeq(value, ";") {
		k, v, ok := strings.Cut(pair, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			continue
		}
		out[entryUnescaper.Replace(k)] = entryUnescaper.Replace(v)
	}
	return out
}

// tagNamer spells discovered tag names the way the scope does. Azure compares tag names
// without regard to case ("Tag names are case-insensitive for operations"; values are
// case-sensitive), and a tag somebody set as HS-Owner in the portal is the operator's hs-owner.
// The controllers look tags up by name, so discovery reports a tag whose name equals one the
// scope reads (its tag keys, selector, required tags and auto-import keys, and the operator's
// own keys) under the scope's spelling. Other names keep Azure's spelling.
type tagNamer map[string]string

// newTagNamer knows the names of the target: the scope's (TagNames and the selector's keys)
// before the operator's defaults, so a scope that spells a default key its own way keeps its
// spelling.
func newTagNamer(target inventory.Target) tagNamer {
	n := tagNamer{}
	add := func(name string) {
		if name == "" {
			return
		}
		if _, ok := n[strings.ToLower(name)]; !ok {
			n[strings.ToLower(name)] = name
		}
	}
	for _, name := range target.TagNames {
		add(name)
	}
	for _, name := range slices.Sorted(maps.Keys(target.NetworkSelector)) {
		add(name)
	}
	k := networkv1.OperatorTagKeysFor(networkv1.ProviderAzure)
	for _, name := range []string{k.Owner, k.Env, k.Tier, k.Managed, k.ManagedBy, k.Claim} {
		add(name)
	}
	return n
}

// name is the spelling a discovered tag name is reported under.
func (n tagNamer) name(name string) string {
	if known, ok := n[strings.ToLower(name)]; ok {
		return known
	}
	return name
}

// networkTags splits the tags of a virtual network into its own and its subnets' entries, by
// lowercase subnet name, with the names of both in the scope's spelling.
func (n tagNamer) networkTags(raw map[string]*string) (own map[string]string, entries map[string]map[string]string) {
	own, entries = map[string]string{}, map[string]map[string]string{}
	for k, v := range raw {
		value := ""
		if v != nil {
			value = *v
		}
		if len(k) > len(subnetEntryPrefix) && strings.EqualFold(k[:len(subnetEntryPrefix)], subnetEntryPrefix) {
			entry := map[string]string{}
			for ek, ev := range DecodeSubnetEntry(value) {
				entry[n.name(ek)] = ev
			}
			entries[strings.ToLower(k[len(subnetEntryPrefix):])] = entry
			continue
		}
		own[n.name(k)] = value
	}
	return own, entries
}

// subnetTags are the tags a subnet reports, and where they came from: the network's, with its
// entry's laid over them name by name. A name of the entry replaces the network's tag of the
// same name in any case.
func subnetTags(name string, network map[string]string, entries map[string]map[string]string) (
	map[string]string, networkv1.OwnershipSource) {
	tags := maps.Clone(network)
	entry, ok := entries[strings.ToLower(name)]
	if !ok {
		return tags, networkv1.OwnershipSourceNetwork
	}
	for k, v := range entry {
		maps.DeleteFunc(tags, func(existing, _ string) bool { return strings.EqualFold(existing, k) })
		tags[k] = v
	}
	return tags, networkv1.OwnershipSourceSubnet
}
