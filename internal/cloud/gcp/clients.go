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
	"fmt"
	"slices"
	"strings"
	"sync"

	compute "cloud.google.com/go/compute/apiv1"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
)

// clients are the Google API clients of one identity. They all use the REST transport: the
// Compute client has no other, and one transport keeps the errors uniform.
type clients struct {
	networks    *compute.NetworksClient
	subnetworks *compute.SubnetworksClient
	projects    *resourcemanager.ProjectsClient
	tagKeys     *resourcemanager.TagKeysClient
	tagValues   *resourcemanager.TagValuesClient

	opts       []option.ClientOption
	rmEndpoint func(location string) string
	// token is the impersonated token source the clients authenticate with, nil for the
	// operator's own identity.
	token oauth2.TokenSource

	mu sync.Mutex
	// tagBindings is one client per location: Resource Manager serves the tags of a regional
	// resource only from that region's endpoint.
	tagBindings map[string]*resourcemanager.TagBindingsClient
}

// globalLocation is the location of global resources and of Resource Manager's own endpoint.
const globalLocation = "global"

// defaultResourceManagerEndpoint is Google's Resource Manager endpoint of a location.
func defaultResourceManagerEndpoint(location string) string {
	if location == "" || location == globalLocation {
		return "https://cloudresourcemanager.googleapis.com"
	}
	return "https://" + location + "-cloudresourcemanager.googleapis.com"
}

func newClients(ctx context.Context, opts []option.ClientOption, cfg Options) (*clients, error) {
	computeOpts := opts
	if cfg.ComputeEndpoint != "" {
		computeOpts = append(slices.Clip(computeOpts), option.WithEndpoint(cfg.ComputeEndpoint))
	}
	networks, err := compute.NewNetworksRESTClient(ctx, computeOpts...)
	if err != nil {
		return nil, fmt.Errorf("compute networks client: %w", err)
	}
	subnetworks, err := compute.NewSubnetworksRESTClient(ctx, computeOpts...)
	if err != nil {
		return nil, fmt.Errorf("compute subnetworks client: %w", err)
	}
	endpoint := cfg.ResourceManagerEndpoint
	if endpoint == nil {
		endpoint = defaultResourceManagerEndpoint
	}
	global := append(slices.Clip(opts), option.WithEndpoint(endpoint(globalLocation)))
	projects, err := resourcemanager.NewProjectsRESTClient(ctx, global...)
	if err != nil {
		return nil, fmt.Errorf("resource manager projects client: %w", err)
	}
	// Tag keys and values are global resources, served by the global endpoint only.
	tagKeys, err := resourcemanager.NewTagKeysRESTClient(ctx, global...)
	if err != nil {
		return nil, fmt.Errorf("resource manager tag keys client: %w", err)
	}
	tagValues, err := resourcemanager.NewTagValuesRESTClient(ctx, global...)
	if err != nil {
		return nil, fmt.Errorf("resource manager tag values client: %w", err)
	}
	return &clients{networks: networks, subnetworks: subnetworks, projects: projects, tagKeys: tagKeys,
		tagValues: tagValues, opts: opts, rmEndpoint: endpoint,
		tagBindings: map[string]*resourcemanager.TagBindingsClient{}}, nil
}

// tagBindingsAt returns the tag bindings client of a location ("global" or a region).
func (c *clients) tagBindingsAt(ctx context.Context, location string) (*resourcemanager.TagBindingsClient, error) {
	location = strings.ToLower(location)
	c.mu.Lock()
	defer c.mu.Unlock()
	if tb, ok := c.tagBindings[location]; ok {
		return tb, nil
	}
	// Like the other clients it outlives the call that builds it (see Discoverer.buildClients).
	tb, err := resourcemanager.NewTagBindingsRESTClient(context.WithoutCancel(ctx),
		append(slices.Clip(c.opts), option.WithEndpoint(c.rmEndpoint(location)))...)
	if err != nil {
		return nil, fmt.Errorf("resource manager tag bindings client for %s: %w", location, err)
	}
	c.tagBindings[location] = tb
	return tb, nil
}
