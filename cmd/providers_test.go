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
	"strings"
	"testing"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
)

func TestNewProvidersBuildsTheNamedOnes(t *testing.T) {
	t.Setenv("AWS_REGION", "eu-central-1")
	r, err := newProviders(context.Background(), " AWS, aws ,", providerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.All()) != 1 {
		t.Fatalf("providers = %d, want AWS once", len(r.All()))
	}
	if _, ok := r.Get(networkv1.ProviderAWS); !ok {
		t.Error("AWS is not registered")
	}
}

func TestNewProvidersBuildsGCPOnlyWhenNamed(t *testing.T) {
	t.Setenv("AWS_REGION", "eu-central-1")
	r, err := newProviders(context.Background(), "aws,GCP", providerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get(networkv1.ProviderGCP); !ok || len(r.All()) != 2 {
		t.Errorf("providers = %d, want AWS and GCP", len(r.All()))
	}
	// The default stays AWS alone: GCP runs only where the operator is told to.
	r, err = newProviders(context.Background(), "aws", providerConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Get(networkv1.ProviderGCP); ok {
		t.Error("GCP runs although only aws was named")
	}
}

func TestNewProvidersRefusesUnknownAndEmptyLists(t *testing.T) {
	if _, err := newProviders(context.Background(), "aws,azure", providerConfig{}); err == nil ||
		!strings.Contains(err.Error(), `unknown provider "azure"`) {
		t.Errorf("err = %v, want azure refused as unknown", err)
	}
	if _, err := newProviders(context.Background(), " , ", providerConfig{}); err == nil {
		t.Error("an empty provider list was accepted")
	}
}
