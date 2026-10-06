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
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation/field"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/cloud/gcp/gcpfake"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

// Reads use the account's serviceAccount, writes its writeServiceAccount, and an account
// without them the operator's own identity, for both.
func TestIdentityPerAccess(t *testing.T) {
	p := NewProvider(Options{})
	for _, tc := range []struct {
		name        string
		gcp         *networkv1.GCPAccount
		read, write Identity
	}{
		{"no member", nil, Identity{}, Identity{}},
		{"empty member", &networkv1.GCPAccount{}, Identity{}, Identity{}},
		{"read only", &networkv1.GCPAccount{ServiceAccount: contractReader}, Identity{ServiceAccount: contractReader},
			Identity{}},
		{"both", &networkv1.GCPAccount{ServiceAccount: contractReader, WriteServiceAccount: contractWriter},
			Identity{ServiceAccount: contractReader}, Identity{ServiceAccount: contractWriter}},
		{"write only", &networkv1.GCPAccount{WriteServiceAccount: contractWriter}, Identity{},
			Identity{ServiceAccount: contractWriter}},
	} {
		account := networkv1.Account{ID: contractProject, GCP: tc.gcp}
		if got := p.Identity(account, provider.Read); got != tc.read {
			t.Errorf("%s: read identity = %v, want %v", tc.name, got, tc.read)
		}
		if got := p.Identity(account, provider.Write); got != tc.write {
			t.Errorf("%s: write identity = %v, want %v", tc.name, got, tc.write)
		}
	}
	// A project read as a service account is not reached with the operator's own identity,
	// so without a write service account it cannot be written to.
	scope := &networkv1.NetworkScope{Spec: networkv1.NetworkScopeSpec{Accounts: []networkv1.Account{
		{ID: contractProject, GCP: &networkv1.GCPAccount{ServiceAccount: contractReader}}}}}
	if !provider.MissingWriteIdentity(p, scope, contractProject) {
		t.Error("a project read as a service account, without a write service account, is not reported")
	}
}

// newImpersonationProvider is a provider on a fake whose project answers only the reader and
// the writer, which the operator's own identity may impersonate.
func newImpersonationProvider(t *testing.T) (*Provider, *gcpfake.Cloud) {
	t.Helper()
	p, cloud := newFakeProvider(t, Options{})
	cloud.AddServiceAccount(contractReader, gcpfake.Operator)
	cloud.AddServiceAccount(contractWriter, gcpfake.Operator)
	cloud.RestrictProject(contractProject, contractReader, contractWriter)
	cloud.AddNetwork(contractProject, "vpc")
	cloud.AddSubnetwork(contractProject, contractRegion, "vpc", "apps", "10.0.0.0/24")
	return p, cloud
}

func readerTarget() inventory.Target {
	target := contractTarget()
	target.Identity = Identity{ServiceAccount: contractReader}
	return target
}

// The chain: the operator's own identity asks IAM for the reader's token, and every Compute
// and Resource Manager call is made as the reader.
func TestDiscoverImpersonatesTheReadServiceAccount(t *testing.T) {
	p, cloud := newImpersonationProvider(t)
	if got := len(discoverOK(t, p, readerTarget()).Subnets); got != 1 {
		t.Errorf("%d subnets, want the project's one", got)
	}
	own := cloud.RequestsBy(gcpfake.Operator)
	if len(own) != 1 || !strings.HasPrefix(own[0], "POST /iam/v1/projects/-/serviceAccounts/"+contractReader+
		":generateAccessToken") {
		t.Errorf("the operator's own identity made %v, want only the token request for the reader", own)
	}
	var compute, tags bool
	for _, r := range cloud.RequestsBy(contractReader) {
		compute = compute || strings.Contains(r, "/compute/v1/projects/"+contractProject+"/")
		tags = tags || strings.Contains(r, "/v3/effectiveTags?")
	}
	if !compute || !tags {
		t.Errorf("the reader made %v, want the Compute and the Resource Manager calls", cloud.RequestsBy(contractReader))
	}
	if w := cloud.RequestsBy(contractWriter); len(w) != 0 {
		t.Errorf("discovery acted as the write service account: %v", w)
	}
	if n := cloud.TokensIssued(contractWriter); n != 0 {
		t.Errorf("discovery asked for %d tokens of the write service account", n)
	}
}

// Without a service account the operator reads as itself, and a project that lets only the
// service accounts in refuses it.
func TestDiscoverUsesTheOperatorsOwnIdentityWithoutAServiceAccount(t *testing.T) {
	p, cloud := newImpersonationProvider(t)
	if _, err := p.Discover(context.Background(), contractTarget()); err == nil {
		t.Fatal("the operator's own identity read a project that only lets its service accounts in")
	}
	if cloud.TokensIssued(contractReader) != 0 {
		t.Error("a target without a service account impersonated one")
	}
	cloud.RestrictProject(contractProject, gcpfake.Operator)
	discoverOK(t, p, contractTarget())
	if len(cloud.RequestsBy(gcpfake.Operator)) == 0 {
		t.Error("no request was made as the operator's own identity")
	}
}

// One token per service account, reused until shortly before it expires.
func TestImpersonatedTokensAreCachedPerServiceAccount(t *testing.T) {
	p, cloud := newImpersonationProvider(t)
	ctx := context.Background()
	for range 3 {
		discoverOK(t, p, readerTarget())
	}
	if n := cloud.TokensIssued(contractReader); n != 1 {
		t.Errorf("three discoveries asked for %d reader tokens, want 1", n)
	}
	// The write identity has clients and a token of its own; the reader's is not reused.
	if _, err := p.discoverer.clientsFor(ctx, Identity{ServiceAccount: contractWriter}); err != nil {
		t.Fatalf("clients of the write service account: %v", err)
	}
	if r, w := cloud.TokensIssued(contractReader), cloud.TokensIssued(contractWriter); r != 1 || w != 1 {
		t.Errorf("tokens issued: reader %d, writer %d, want one each", r, w)
	}

	// A token that expires within the early-expiry window is renewed before the next call
	// rather than reused.
	p2, cloud2 := newImpersonationProvider(t)
	cloud2.SetTokenLifetime(tokenEarlyExpiry / 2)
	discoverOK(t, p2, readerTarget())
	discoverOK(t, p2, readerTarget())
	if n := cloud2.TokensIssued(contractReader); n < 2 {
		t.Errorf("two discoveries with tokens about to expire asked for %d tokens, want them renewed", n)
	}
}

// Impersonation that is refused is a failure of the target that says what to grant, is not
// throttling, and makes no call to the project.
func TestARefusedImpersonationSaysWhatToGrant(t *testing.T) {
	p, cloud := newImpersonationProvider(t)
	cloud.AddServiceAccount(contractReader) // nobody may impersonate it
	_, err := p.Discover(context.Background(), readerTarget())
	if err == nil {
		t.Fatal("discovery through a service account the operator may not impersonate succeeded")
	}
	if errors.Is(err, inventory.ErrThrottled) {
		t.Errorf("a refused impersonation is reported as throttling: %v", err)
	}
	var ierr *ImpersonationError
	if !errors.As(err, &ierr) || ierr.ServiceAccount != contractReader {
		t.Errorf("err = %v, want an ImpersonationError for %s", err, contractReader)
	}
	for _, want := range []string{contractReader, "roles/iam.serviceAccountTokenCreator", "permission denied"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q: %v", want, err)
		}
	}
	if r := cloud.RequestsBy(contractReader); len(r) != 0 {
		t.Errorf("calls were made as the reader without a token: %v", r)
	}

	// A service account that does not exist is refused the same way.
	target := contractTarget()
	target.Identity = Identity{ServiceAccount: "nobody@contract-project.iam.gserviceaccount.com"}
	if _, err := p.Discover(context.Background(), target); !errors.As(err, &ierr) {
		t.Errorf("an unknown service account: err = %v, want an ImpersonationError", err)
	}

	// Once granted, the next discovery gets through: a refusal is not cached.
	cloud.AddServiceAccount(contractReader, gcpfake.Operator)
	discoverOK(t, p, readerTarget())
}

// IAM rate-limiting the token request is throttling, which the controller backs off from.
func TestAThrottledTokenRequestIsThrottling(t *testing.T) {
	p, cloud := newImpersonationProvider(t)
	cloud.ThrottleNext(1)
	if _, err := p.Discover(context.Background(), readerTarget()); !errors.Is(err, inventory.ErrThrottled) {
		t.Errorf("a throttled token request: err = %v, want ErrThrottled", err)
	}
	discoverOK(t, p, readerTarget())
}

// The clients outlive the discovery that built them: a cancelled first discovery does not
// break the next.
func TestClientsOutliveTheDiscoveryThatBuiltThem(t *testing.T) {
	p, _ := newImpersonationProvider(t)
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := p.discoverer.clientsFor(ctx, Identity{ServiceAccount: contractReader}); err != nil {
		t.Fatal(err)
	}
	cancel()
	discoverOK(t, p, readerTarget())
}

func TestCheckAccountIdentity(t *testing.T) {
	path := field.NewPath("spec", "accounts").Index(0)
	for _, tc := range []struct {
		name     string
		gcp      *networkv1.GCPAccount
		errs     int
		warnings int
	}{
		{"none", nil, 0, 0},
		{"read and write", &networkv1.GCPAccount{ServiceAccount: contractReader, WriteServiceAccount: contractWriter}, 0, 0},
		{"a default compute service account", &networkv1.GCPAccount{
			ServiceAccount: "123456789012-compute@developer.gserviceaccount.com"}, 0, 0},
		{"not an email", &networkv1.GCPAccount{ServiceAccount: "subnet-reader"}, 1, 0},
		{"a user", &networkv1.GCPAccount{WriteServiceAccount: "someone@example.com"}, 1, 0},
		{"the same for both", &networkv1.GCPAccount{ServiceAccount: contractReader, WriteServiceAccount: contractReader}, 0, 1},
	} {
		warnings, errs := checkAccountIdentity(path, networkv1.Account{ID: contractProject, GCP: tc.gcp})
		if len(errs) != tc.errs || len(warnings) != tc.warnings {
			t.Errorf("%s: %d errors (%v) and %d warnings (%v), want %d and %d", tc.name, len(errs), errs.ToAggregate(),
				len(warnings), warnings, tc.errs, tc.warnings)
		}
	}
}
