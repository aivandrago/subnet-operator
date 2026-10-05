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

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"

	awscloud "hypersurgery.dev/subnet-operator/internal/cloud/aws"
	azurecloud "hypersurgery.dev/subnet-operator/internal/cloud/azure"
	gcpcloud "hypersurgery.dev/subnet-operator/internal/cloud/gcp"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

// providerConfig is the per-provider configuration from the command line. Each provider has
// its own flags (--aws-*), because what configures a cloud's credentials and change events is
// different on every cloud.
type providerConfig struct {
	aws   awscloud.Options
	gcp   gcpcloud.Options
	azure azurecloud.Options
}

// providerFactories are the providers this build can run, by their --providers name. A new
// cloud adds one entry here, its flags, and its provider package (ADR 0002, "Consequences").
var providerFactories = map[string]func(ctx context.Context, cfg providerConfig) (provider.Provider, error){
	"aws": func(ctx context.Context, cfg providerConfig) (provider.Provider, error) {
		return awscloud.NewFromEnvironment(ctx, cfg.aws)
	},
	"gcp": func(ctx context.Context, cfg providerConfig) (provider.Provider, error) {
		return gcpcloud.NewFromEnvironment(ctx, cfg.gcp)
	},
	"azure": func(ctx context.Context, cfg providerConfig) (provider.Provider, error) {
		return azurecloud.NewFromEnvironment(ctx, cfg.azure)
	},
}

// azureEndpoints checks --azure-cloud and --azure-authority-host, whether they came from the
// command line or from the environment, and returns the cloud's endpoints.
func azureEndpoints(cloudName, authorityHost string) (cloud.Configuration, error) {
	cfg, ok := azurecloud.Clouds[cloudName]
	if !ok {
		return cloud.Configuration{}, fmt.Errorf("unknown Azure cloud %q (--azure-cloud, %s); use %s", cloudName,
			envName("azure-cloud"), strings.Join(slices.Sorted(maps.Keys(azurecloud.Clouds)), ", "))
	}
	if authorityHost != "" && !strings.HasPrefix(authorityHost, "https://") {
		return cloud.Configuration{}, fmt.Errorf("the Azure authority host %q (--azure-authority-host, %s) must be an "+
			"https:// URL", authorityHost, envName("azure-authority-host"))
	}
	return cfg, nil
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
