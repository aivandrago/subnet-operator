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

package gcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"cloud.google.com/go/compute/apiv1/computepb"
	"cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"golang.org/x/sync/errgroup"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// ReservedIPs is the number of addresses GCP reserves in the primary IPv4 range of every
// subnetwork: network, gateway, second-to-last and broadcast. Secondary ranges reserve none.
const ReservedIPs = 4

// tagReadConcurrency caps the tag reads of one discovery: Resource Manager answers one
// resource per call.
const tagReadConcurrency = 8

// computeResource is the prefix of the full resource names Resource Manager uses for Compute
// resources, which name the project by number and the resource by numeric ID.
const computeResource = "//compute.googleapis.com/"

// discovered is one network or subnetwork with what its tags are read from.
type discovered struct {
	location string // Resource Manager location: "global" or the region
	resource string // full resource name
	tags     map[string]string
	gone     bool
}

// discoverTarget reads the project's VPC networks (global, so every region's target reports
// them), the subnetworks of the target's region with their IP usage, and the direct tag
// bindings of both. Like on AWS, the networks the selector leaves out are reported
// separately with their subnets when DiscoverUnmanaged is set.
func (d *Discoverer) discoverTarget(ctx context.Context, c *clients, target inventory.Target) (*inventory.Snapshot, error) {
	project, region := target.Account, target.Region
	number, err := d.projectNumber(ctx, c, target, project)
	if err != nil {
		return nil, fmt.Errorf("look up project %s: %w", project, err)
	}
	tagParent, err := d.tagParent(ctx, c, target)
	if err != nil {
		return nil, err
	}

	rawNetworks, err := d.listNetworks(ctx, c, target)
	if err != nil {
		return nil, fmt.Errorf("list networks: %w", err)
	}
	netTags := make([]discovered, len(rawNetworks))
	for i, n := range rawNetworks {
		netTags[i] = discovered{location: globalLocation,
			resource: fmt.Sprintf("%sprojects/%s/global/networks/%d", computeResource, number, n.GetId())}
	}
	if err := d.readTags(ctx, c, target, tagParent, netTags); err != nil {
		return nil, fmt.Errorf("read network tags: %w", err)
	}

	unmanagedWanted := target.DiscoverUnmanaged && len(target.NetworkSelector) > 0
	snap := &inventory.Snapshot{}
	managed, unmanaged := map[string]bool{}, map[string]bool{}
	for i, n := range rawNetworks {
		if netTags[i].gone {
			continue
		}
		network := networkOf(project, n, netTags[i].tags)
		switch {
		case matchesSelector(network.Tags, target.NetworkSelector):
			snap.Networks = append(snap.Networks, network)
			managed[network.ID] = true
		case unmanagedWanted:
			snap.UnmanagedNetworks = append(snap.UnmanagedNetworks, network)
			unmanaged[network.ID] = true
		}
	}
	if len(managed) == 0 && len(unmanaged) == 0 {
		return snap, nil
	}

	rawSubnets, err := d.listSubnetworks(ctx, c, target)
	if err != nil {
		return nil, fmt.Errorf("list subnetworks: %w", err)
	}
	var wanted []*computepb.Subnetwork
	for _, s := range rawSubnets {
		if network := relativeName(s.GetNetwork()); managed[network] || unmanaged[network] {
			wanted = append(wanted, s)
		}
	}
	subTags := make([]discovered, len(wanted))
	for i, s := range wanted {
		subTags[i] = discovered{location: region,
			resource: fmt.Sprintf("%sprojects/%s/regions/%s/subnetworks/%d", computeResource, number, region, s.GetId())}
	}
	if err := d.readTags(ctx, c, target, tagParent, subTags); err != nil {
		return nil, fmt.Errorf("read subnetwork tags: %w", err)
	}
	for i, s := range wanted {
		if subTags[i].gone {
			continue
		}
		subnet := subnetOf(project, region, s, subTags[i].tags)
		if managed[subnet.NetworkID] {
			snap.Subnets = append(snap.Subnets, subnet)
		} else {
			snap.UnmanagedSubnets = append(snap.UnmanagedSubnets, subnet)
		}
	}
	return snap, nil
}

// matchesSelector reports whether the tags satisfy every entry of the selector. An empty
// value matches any value of the key. Compute cannot filter by tags, so it is always applied
// here.
func matchesSelector(tags, selector map[string]string) bool {
	for k, want := range selector {
		got, ok := tags[k]
		if !ok || (want != "" && got != want) {
			return false
		}
	}
	return true
}

// projectID returns the ID of a project given its number, if a discovery has looked the
// project up. Change events name some resources by project number; the projects they are
// about are the scopes' projects, which discovery has seen.
func (d *Discoverer) projectID(number string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, n := range d.projectNumbers {
		if n == number {
			return id, true
		}
	}
	return "", false
}

// projectNumber returns the number of a project, which Resource Manager names resources by.
func (d *Discoverer) projectNumber(ctx context.Context, c *clients, target inventory.Target, project string) (string, error) {
	d.mu.Lock()
	number, ok := d.projectNumbers[project]
	d.mu.Unlock()
	if ok {
		return number, nil
	}
	var p *resourcemanagerpb.Project
	err := d.call(ctx, target, resourceManagerBucket(globalLocation), "projects.get", func(ctx context.Context) error {
		var err error
		p, err = c.projects.GetProject(ctx, &resourcemanagerpb.GetProjectRequest{Name: "projects/" + project})
		return err
	})
	if err != nil {
		return "", err
	}
	number, ok = strings.CutPrefix(p.GetName(), "projects/")
	if !ok || number == "" {
		return "", fmt.Errorf("project %s has the unexpected name %q", project, p.GetName())
	}
	d.mu.Lock()
	d.projectNumbers[project] = number
	d.mu.Unlock()
	return number, nil
}

// tagParent returns the parent of the scope's tag keys as Resource Manager reports it on a
// tag (organizations/<number> or projects/<number>), or "" when the target has no GCP
// settings, in which case every tag keeps its namespaced key.
func (d *Discoverer) tagParent(ctx context.Context, c *clients, target inventory.Target) (string, error) {
	if target.GCP == nil || target.GCP.TagParent == "" {
		return "", nil
	}
	parent := target.GCP.TagParent
	project, ok := strings.CutPrefix(parent, "projects/")
	if !ok {
		return parent, nil
	}
	if _, err := strconv.ParseUint(project, 10, 64); err == nil {
		return parent, nil
	}
	number, err := d.projectNumber(ctx, c, target, project)
	if err != nil {
		return "", fmt.Errorf("look up tagParent %s: %w", parent, err)
	}
	return "projects/" + number, nil
}

func (d *Discoverer) listNetworks(ctx context.Context, c *clients, target inventory.Target) ([]*computepb.Network, error) {
	var out []*computepb.Network
	err := d.call(ctx, target, computeBucket(target.Account, ""), "networks.list", func(ctx context.Context) error {
		out = nil
		it := c.networks.List(ctx, &computepb.ListNetworksRequest{Project: target.Account})
		for {
			n, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return nil
			}
			if err != nil {
				return err
			}
			out = append(out, n)
		}
	})
	return out, err
}

// listSubnetworks lists the subnetworks of the target's region with their IP usage.
func (d *Discoverer) listSubnetworks(ctx context.Context, c *clients, target inventory.Target) ([]*computepb.Subnetwork, error) {
	var out []*computepb.Subnetwork
	err := d.call(ctx, target, computeBucket(target.Account, target.Region), "subnetworks.list", func(ctx context.Context) error {
		out = nil
		it := c.subnetworks.List(ctx, &computepb.ListSubnetworksRequest{
			Project: target.Account,
			Region:  target.Region,
			Views:   new(computepb.ListSubnetworksRequest_WITH_UTILIZATION.String()),
		})
		for {
			s, err := it.Next()
			if errors.Is(err, iterator.Done) {
				return nil
			}
			if err != nil {
				return err
			}
			out = append(out, s)
		}
	})
	return out, err
}

// readTags reads the direct tag bindings of each resource, a few at a time. A resource
// Resource Manager no longer knows was deleted after it was listed, and is marked gone.
func (d *Discoverer) readTags(ctx context.Context, c *clients, target inventory.Target, tagParent string,
	resources []discovered) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(tagReadConcurrency)
	for i := range resources {
		r := &resources[i]
		g.Go(func() error {
			tb, err := c.tagBindingsAt(gctx, r.location)
			if err != nil {
				return err
			}
			var tags []*resourcemanagerpb.EffectiveTag
			err = d.call(gctx, target, resourceManagerBucket(r.location), "effectiveTags.list", func(ctx context.Context) error {
				tags = nil
				it := tb.ListEffectiveTags(ctx, &resourcemanagerpb.ListEffectiveTagsRequest{Parent: r.resource})
				for {
					t, err := it.Next()
					if errors.Is(err, iterator.Done) {
						return nil
					}
					if err != nil {
						return err
					}
					tags = append(tags, t)
				}
			})
			if isNotFound(err) {
				r.gone = true
				return nil
			}
			if err != nil {
				return err
			}
			r.tags = decodeTags(tags, tagParent)
			return nil
		})
	}
	return g.Wait()
}

func isNotFound(err error) bool {
	var gerr *googleapi.Error
	return errors.As(err, &gerr) && gerr.Code == http.StatusNotFound
}

// decodeTags turns the effective tags of a resource into its tag map (ADR 0002 §6). Only
// direct bindings count: a tag inherited from the project, a folder or the organization says
// nothing about who owns this network or subnet. A key of the scope's tag parent is reported
// by its short name (hs-owner), so tagKeys, requiredSubnetTags and the selector name it the
// way the scope does; any other key keeps its namespaced name (<parent>/<key>), so two
// parents' keys of the same short name cannot be confused.
func decodeTags(tags []*resourcemanagerpb.EffectiveTag, tagParent string) map[string]string {
	out := map[string]string{}
	for _, t := range tags {
		if t.GetInherited() {
			continue
		}
		key := t.GetNamespacedTagKey()
		if tagParent != "" && t.GetTagKeyParentName() == tagParent {
			key = lastSegment(key)
		}
		out[key] = lastSegment(t.GetNamespacedTagValue())
	}
	return out
}

func lastSegment(s string) string {
	return s[strings.LastIndex(s, "/")+1:]
}

// relativeName turns a Compute URL (https://www.googleapis.com/compute/v1/projects/p/...)
// into the relative resource name (projects/p/...), the canonical ID of ADR 0002 §5.
func relativeName(url string) string {
	if i := strings.Index(url, "projects/"); i >= 0 {
		return url[i:]
	}
	return url
}

func networkOf(project string, n *computepb.Network, tags map[string]string) inventory.Network {
	network := inventory.Network{
		ID:      "projects/" + project + "/global/networks/" + n.GetName(),
		Account: project,
		Name:    n.GetName(),
		Tags:    tags,
		GCP: &networkv1.GCPNetworkStatus{
			RoutingMode:           n.GetRoutingConfig().GetRoutingMode(),
			AutoCreateSubnetworks: n.GetAutoCreateSubnetworks(),
		},
	}
	// The peerings come with the network: networks.list needs no further call or permission.
	for _, p := range n.GetPeerings() {
		network.GCP.Peerings = append(network.GCP.Peerings, networkv1.GCPNetworkPeering{
			Name:         p.GetName(),
			Network:      relativeName(p.GetNetwork()),
			State:        p.GetState(),
			StateDetails: p.GetStateDetails(),
		})
	}
	// Only a legacy network has a range of its own; a VPC network's ranges are its subnets'.
	if r := n.GetIPv4Range(); r != "" {
		network.CIDRBlocks = []string{r}
	}
	if r := n.GetInternalIpv6Range(); r != "" {
		network.IPv6CIDRBlocks = []string{r}
	}
	return network
}

func subnetOf(project, region string, s *computepb.Subnetwork, tags map[string]string) inventory.Subnet {
	subnet := inventory.Subnet{
		ID:              "projects/" + project + "/regions/" + region + "/subnetworks/" + s.GetName(),
		NetworkID:       relativeName(s.GetNetwork()),
		Account:         project,
		Region:          region,
		Name:            s.GetName(),
		State:           s.GetState(),
		CIDRBlock:       s.GetIpCidrRange(),
		OwnershipSource: networkv1.OwnershipSourceSubnet,
		Tags:            tags,
		GCP: &networkv1.GCPSubnetStatus{
			Purpose:               s.GetPurpose(),
			Role:                  s.GetRole(),
			StackType:             s.GetStackType(),
			IPv6AccessType:        s.GetIpv6AccessType(),
			PrivateIPGoogleAccess: s.GetPrivateIpGoogleAccess(),
		},
	}
	for _, p := range []string{s.GetInternalIpv6Prefix(), s.GetExternalIpv6Prefix()} {
		if p != "" {
			subnet.IPv6CIDRBlocks = append(subnet.IPv6CIDRBlocks, p)
		}
	}

	// Free addresses per range, as Compute reports them with views=WITH_UTILIZATION; the
	// primary range has no name. A range Compute says nothing about stays unknown.
	free := map[string]int64{}
	for _, u := range s.GetUtilizationDetails().GetIpv4Utilizations() {
		if u.TotalFreeIp != nil {
			free[u.GetRangeName()] = u.GetTotalFreeIp()
		}
	}
	if subnet.CIDRBlock == "" {
		// IPv6-only: no IPv4 addresses at all, which is known.
		subnet.TotalIPs, subnet.AvailableIPs = new(int64(0)), new(int64(0))
	} else {
		total := inventory.UsableIPv4(subnet.CIDRBlock, ReservedIPs)
		subnet.TotalIPs = &total
		if f, ok := free[""]; ok {
			subnet.AvailableIPs = new(min(max(f, 0), total))
		}
	}
	for _, r := range s.GetSecondaryIpRanges() {
		subnet.SecondaryCIDRBlocks = append(subnet.SecondaryCIDRBlocks, r.GetIpCidrRange())
		sr := networkv1.GCPSecondaryRange{Name: r.GetRangeName(), CIDRBlock: r.GetIpCidrRange()}
		total := inventory.UsableIPv4(sr.CIDRBlock, 0)
		sr.TotalIPs = &total
		if f, ok := free[sr.Name]; ok {
			sr.AvailableIPs = new(min(max(f, 0), total))
		}
		subnet.GCP.SecondaryRanges = append(subnet.GCP.SecondaryRanges, sr)
	}
	return subnet
}
