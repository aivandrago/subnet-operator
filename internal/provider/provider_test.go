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

package provider

import (
	"strings"
	"testing"

	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// stub is a provider with a name and identities; every other method panics, which is what
// these tests want: the registry must not call them.
type stub struct {
	Provider
	name  networkv1beta1.Provider
	read  inventory.Identity
	write inventory.Identity
}

func (s stub) Name() networkv1beta1.Provider { return s.name }

func (s stub) Identity(_ networkv1beta1.Account, access Access) inventory.Identity {
	if access == Write {
		return s.write
	}
	return s.read
}

type role string

func (r role) Own() bool      { return r == "" }
func (r role) String() string { return string(r) }

func TestRegistryLooksUpByName(t *testing.T) {
	r, err := NewRegistry(stub{name: "GCP"}, stub{name: "AWS"})
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := r.Get("AWS"); !ok || p.Name() != "AWS" {
		t.Errorf("Get(AWS) = %v, %v", p, ok)
	}
	if _, ok := r.Get("Azure"); ok {
		t.Error("Get(Azure) found a provider nobody registered")
	}
	names := make([]string, 0, len(r.All()))
	for _, p := range r.All() {
		names = append(names, string(p.Name()))
	}
	if strings.Join(names, ",") != "AWS,GCP" {
		t.Errorf("All() = %v, want sorted AWS,GCP", names)
	}
	if msg := r.NotEnabled("Azure"); !strings.Contains(msg, `"Azure"`) || !strings.Contains(msg, "AWS, GCP") {
		t.Errorf("NotEnabled(Azure) = %q, want the provider and the enabled ones", msg)
	}
}

func TestRegistryRefusesDuplicatesAndNamelessProviders(t *testing.T) {
	if _, err := NewRegistry(stub{name: "AWS"}, stub{name: "AWS"}); err == nil {
		t.Error("two AWS providers were registered")
	}
	if _, err := NewRegistry(stub{}); err == nil {
		t.Error("a provider without a name was registered")
	}
}

// Controllers and webhooks built without providers (a nil registry) find none, rather than
// crashing, and say so.
func TestNilRegistryHasNoProviders(t *testing.T) {
	var r *Registry
	if _, ok := r.Get("AWS"); ok {
		t.Error("a nil registry found a provider")
	}
	if len(r.All()) != 0 {
		t.Error("a nil registry lists providers")
	}
	if msg := r.NotEnabled("AWS"); !strings.Contains(msg, "none") {
		t.Errorf("NotEnabled on a nil registry = %q", msg)
	}
}

// An account reached through a read identity of its own is not the operator's account, so it
// needs a write identity of its own too; the operator's own account does not.
func TestMissingWriteIdentity(t *testing.T) {
	scope := &networkv1beta1.NetworkScope{Spec: networkv1beta1.NetworkScopeSpec{
		Accounts: []networkv1beta1.Account{{ID: "a"}}}}
	for _, tc := range []struct {
		name        string
		read, write inventory.Identity
		want        bool
	}{
		{"own account", role(""), role(""), false},
		{"own account, nil identities", nil, nil, false},
		{"spoke without a write identity", role("reader"), role(""), true},
		{"spoke with a write identity", role("reader"), role("writer"), false},
	} {
		if got := MissingWriteIdentity(stub{name: "AWS", read: tc.read, write: tc.write}, scope, "a"); got != tc.want {
			t.Errorf("%s: MissingWriteIdentity = %v, want %v", tc.name, got, tc.want)
		}
	}
	if MissingWriteIdentity(stub{name: "AWS", read: role("reader")}, scope, "not-in-scope") {
		t.Error("an account the scope does not list is reported")
	}
}
