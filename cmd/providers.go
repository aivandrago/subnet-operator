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

package main

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	awscloud "hypersurgery.dev/subnet-operator/internal/cloud/aws"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

// providerConfig is the per-provider configuration from the command line. Each provider has
// its own flags (--aws-*), because what configures a cloud's credentials and change events is
// different on every cloud.
type providerConfig struct {
	aws awscloud.Options
}

// providerFactories are the providers this build can run, by their --providers name. A new
// cloud adds one entry here, its flags, and its provider package (ADR 0002, "Consequences").
var providerFactories = map[string]func(ctx context.Context, cfg providerConfig) (provider.Provider, error){
	"aws": func(ctx context.Context, cfg providerConfig) (provider.Provider, error) {
		return awscloud.NewFromEnvironment(ctx, cfg.aws)
	},
}

// knownProviders lists the --providers names this build knows, sorted.
func knownProviders() []string {
	return slices.Sorted(maps.Keys(providerFactories))
}

// newProviders builds the registry of the providers named in a comma-separated list.
func newProviders(ctx context.Context, names string, cfg providerConfig) (*provider.Registry, error) {
	var providers []provider.Provider
	seen := map[string]bool{}
	for name := range strings.SplitSeq(names, ",") {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		factory, ok := providerFactories[name]
		if !ok {
			return nil, fmt.Errorf("unknown provider %q; this build knows %s", name, strings.Join(knownProviders(), ", "))
		}
		p, err := factory(ctx, cfg)
		if err != nil {
			return nil, fmt.Errorf("provider %s: %w", name, err)
		}
		providers = append(providers, p)
	}
	if len(providers) == 0 {
		return nil, fmt.Errorf("no provider enabled; set --providers to some of %s", strings.Join(knownProviders(), ", "))
	}
	return provider.NewRegistry(providers...)
}
