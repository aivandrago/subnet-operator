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
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/cloud/azure/azurefake"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

const (
	testReader        = "3f0c6a1e-0000-4000-8000-0000000000a1"
	testWriter        = "3f0c6a1e-0000-4000-8000-0000000000b2"
	otherTenant       = "9a8b7c6d-0000-4000-8000-0000000000c3"
	otherSubscription = "00000000-0000-4000-8000-0000000c0de2"
)

// newIdentityProvider wires a provider to a fake with the contract subscription, a network in
// it, and a federated reader, and restricts the subscription to the reader.
func newIdentityProvider(t *testing.T) (*Provider, *azurefake.Cloud) {
	t.Helper()
	p, fake := newFakeProvider(t, Options{})
	fake.AddVirtualNetwork(contractSubscription, testGroup, "hub", contractLocation, []string{"10.0.0.0/16"}, nil)
	fake.AddSubnet(contractSubscription, testGroup, "hub", "apps", "10.0.1.0/24")
	fake.AddIdentity(azurefake.DefaultTenant, testReader, true)
	fake.RestrictSubscription(contractSubscription, testReader)
	return p, fake
}

func targetAs(id inventory.Identity) inventory.Target {
	t := contractTarget()
	t.Identity = id
	return t
}

func TestIdentityPerAccess(t *testing.T) {
	p := NewProvider(Options{})
	for _, tc := range []struct {
		name        string
		azure       *networkv1.AzureAccount
		read, write Identity
	}{
		{"no member", nil, Identity{}, Identity{}},
		{"an empty member", &networkv1.AzureAccount{}, Identity{}, Identity{}},
		{"a read identity only", &networkv1.AzureAccount{ClientID: testReader},
			Identity{ClientID: testReader}, Identity{}},
		{"both, in capitals, in another tenant", &networkv1.AzureAccount{ClientID: strings.ToUpper(testReader),
			WriteClientID: testWriter, TenantID: strings.ToUpper(otherTenant)},
			Identity{TenantID: otherTenant, ClientID: testReader}, Identity{TenantID: otherTenant, ClientID: testWriter}},
		{"a write identity only", &networkv1.AzureAccount{WriteClientID: testWriter},
			Identity{}, Identity{ClientID: testWriter}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			account := networkv1.Account{ID: contractSubscription, Azure: tc.azure}
			if got := p.Identity(account, provider.Read); got != tc.read {
				t.Errorf("read identity = %#v, want %#v", got, tc.read)
			}
			if got := p.Identity(account, provider.Write); got != tc.write {
				t.Errorf("write identity = %#v, want %#v", got, tc.write)
			}
		})
	}
	scope := &networkv1.NetworkScope{Spec: networkv1.NetworkScopeSpec{Provider: networkv1.ProviderAzure,
		Accounts: []networkv1.Account{{ID: contractSubscription, Azure: &networkv1.AzureAccount{ClientID: testReader}}}}}
	if !provider.MissingWriteIdentity(p, scope, contractSubscription) {
		t.Error("a subscription read through an identity of its own, without a write identity, is not reported")
	}
}

// Discovery exchanges the operator's service account token for a token of the read identity,
// and nothing reaches Resource Manager as the operator's own identity.
func TestDiscoverAuthenticatesAsTheReadIdentity(t *testing.T) {
	p, fake := newIdentityProvider(t)
	snap := discoverAll(t, p, targetAs(Identity{ClientID: testReader}))
	if len(snap.Subnets) != 1 {
		t.Errorf("subnets = %v, want apps", snap.Subnets)
	}
	if len(fake.RequestsBy(testReader)) == 0 {
		t.Error("no request was made as the read identity")
	}
	if got := fake.RequestsBy(azurefake.Operator); len(got) != 0 {
		t.Errorf("requests as the operator's own identity: %v", got)
	}

	// The operator's own identity has no role in the subscription any more.
	_, err := p.Discover(context.Background(), contractTarget())
	if err == nil || !strings.Contains(err.Error(), "AuthorizationFailed") {
		t.Errorf("discovery as the operator's own identity = %v, want AuthorizationFailed", err)
	}
}

// One credential per identity: its token is reused across discoveries, subscriptions and
// spellings of the client ID, and each identity has its own.
func TestCredentialsAreKeptPerTenantAndClient(t *testing.T) {
	p, fake := newIdentityProvider(t)
	fake.AddSubscription(otherSubscription)
	fake.AddIdentity(azurefake.DefaultTenant, testWriter, true)
	upper := Identity{ClientID: strings.ToUpper(testReader)}
	discoverAll(t, p, targetAs(Identity{ClientID: testReader}))
	discoverAll(t, p, targetAs(Identity{ClientID: testReader}))
	discoverAll(t, p, targetAs(upper))
	discoverAll(t, p, targetAs(Identity{ClientID: testReader, TenantID: azurefake.DefaultTenant}))
	other := targetAs(Identity{ClientID: testReader})
	other.Account = otherSubscription
	discoverAll(t, p, other)
	if n := fake.TokensIssued(testReader); n != 1 {
		t.Errorf("%d tokens issued for the reader, want 1", n)
	}
	other.Identity = Identity{ClientID: testWriter}
	discoverAll(t, p, other)
	if n := fake.TokensIssued(testWriter); n != 1 {
		t.Errorf("%d tokens issued for the writer, want 1", n)
	}
}

// A token Microsoft Entra ID refuses fails the target, naming the identity, what the
// federated identity credential must say, and Entra's reason; it is not throttling, and the
// operator's own identity is not tried instead.
func TestARefusedTokenSaysWhatToSetUp(t *testing.T) {
	for _, tc := range refusedTokens {
		t.Run(tc.name, func(t *testing.T) {
			p, fake := newIdentityProvider(t)
			tc.arrange(fake, testReader)
			_, err := p.Discover(context.Background(), targetAs(Identity{ClientID: testReader}))
			if _, ok := errors.AsType[*CredentialError](err); !ok {
				t.Fatalf("err = %v, want a CredentialError", err)
			}
			if errors.Is(err, inventory.ErrThrottled) {
				t.Errorf("a refused token is reported as throttling: %v", err)
			}
			for _, want := range append([]string{"cannot authenticate as client " + testReader + ": "}, tc.says...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not say %q: %v", want, err)
				}
			}
			if tc.settings && !strings.Contains(err.Error(), "azure.clientID or writeClientID of the account") {
				t.Errorf("the error does not say where the client ID is set: %v", err)
			}
			if strings.Contains(err.Error(), "\n") {
				t.Errorf("the error spans lines, which a status condition shows badly: %q", err)
			}
			if got := fake.RequestsBy(azurefake.Operator); len(got) != 0 {
				t.Errorf("the operator's own identity was used instead: %v", got)
			}
		})
	}
}

// refusedTokens are the ways the fake Microsoft Entra ID refuses the service account token for
// an identity, and what the error says in each whether the identity is an account's or the
// operator's own: what a federated identity credential must name, and Entra's code.
var refusedTokens = []struct {
	name string
	// arrange makes Entra refuse the client ID in the default tenant.
	arrange func(fake *azurefake.Cloud, clientID string)
	says    []string
	// settings is set where the error names the settings the client and tenant ID come from.
	settings bool
}{
	{"no federated identity credential", func(fake *azurefake.Cloud, clientID string) {
		fake.AddIdentity(azurefake.DefaultTenant, clientID, false)
	}, []string{"Microsoft Entra ID gave no token for the operator's service account",
		"the identity needs a federated identity credential with issuer " + azurefake.ServiceAccountIssuer +
			", subject " + azurefake.ServiceAccountSubject + " and audience api://AzureADTokenExchange " +
			"(deploy/azure/README.md)", "invalid_client: AADSTS70021: No matching federated identity record"}, false},
	{"no such identity in the tenant", func(fake *azurefake.Cloud, clientID string) {
		fake.RemoveIdentity(azurefake.DefaultTenant, clientID)
	}, []string{"Microsoft Entra ID knows no application with this client ID in the tenant",
		"a federated identity credential with issuer " + azurefake.ServiceAccountIssuer +
			", subject " + azurefake.ServiceAccountSubject + " and audience api://AzureADTokenExchange " +
			"(deploy/azure/README.md)", "unauthorized_client: AADSTS700016: Application with identifier"}, true},
}

// workloadIdentityEnvironment gives the process the environment the AKS webhook, or the chart,
// gives the operator's pod, for the fake's tenant and service account token, and makes the
// provider build the SDK's DefaultAzureCredential from it as the manager does.
func workloadIdentityEnvironment(t *testing.T, p *Provider, clientID string) (tokenFile string) {
	t.Helper()
	tokenFile, err := azurefake.WriteServiceAccountToken(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AZURE_CLIENT_ID", strings.ToUpper(clientID))
	t.Setenv("AZURE_TENANT_ID", azurefake.DefaultTenant)
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", tokenFile)
	// Only Workload ID of the SDK's chain: a test must not ask the machine's metadata service
	// or its az for a token.
	t.Setenv("AZURE_TOKEN_CREDENTIALS", "WorkloadIdentityCredential")
	p.discoverer.opts.Credential = nil
	p.discoverer.opts.FederatedTokenFile = ""
	p.discoverer.opts.TenantID = ""
	return tokenFile
}

// The operator's own identity, used for an account without an azure member, is the SDK's
// DefaultAzureCredential, which on Kubernetes exchanges the same service account token. It
// reads a subscription like an account's identity does.
func TestTheOperatorsOwnIdentityIsWorkloadIdentity(t *testing.T) {
	p, fake := newIdentityProvider(t)
	workloadIdentityEnvironment(t, p, testWriter)
	fake.AddIdentity(azurefake.DefaultTenant, testWriter, true)
	fake.RestrictSubscription(contractSubscription, testWriter)
	if snap := discoverAll(t, p, contractTarget()); len(snap.Subnets) != 1 {
		t.Errorf("subnets = %v, want apps", snap.Subnets)
	}
	if n := fake.TokensIssued(testWriter); n != 1 {
		t.Errorf("%d tokens issued for the operator's own identity, want 1", n)
	}
}

// A token Microsoft Entra ID refuses for the operator's own identity reads like one refused for
// an account's (#120): the same CredentialError, with the same hint in the same words, naming
// what the operator knows of an identity no scope names: the client and tenant ID and the token
// file from its environment, and its service account token's issuer and subject.
func TestARefusedTokenOfTheOperatorsOwnIdentitySaysTheSame(t *testing.T) {
	for _, tc := range refusedTokens {
		t.Run(tc.name, func(t *testing.T) {
			p, fake := newIdentityProvider(t)
			workloadIdentityEnvironment(t, p, testWriter)
			tc.arrange(fake, testWriter)
			_, err := p.Discover(context.Background(), contractTarget())
			credErr, ok := errors.AsType[*CredentialError](err)
			if !ok {
				t.Fatalf("err = %v, want a CredentialError", err)
			}
			if !credErr.Identity.Own() || credErr.ClientID != testWriter || credErr.TenantID != azurefake.DefaultTenant {
				t.Errorf("the error is about %+v, want the operator's own identity with its client and tenant", credErr)
			}
			if errors.Is(err, inventory.ErrThrottled) {
				t.Errorf("a refused token is reported as throttling: %v", err)
			}
			who := "cannot authenticate as the operator's own identity (client " + testWriter + " in tenant " +
				azurefake.DefaultTenant + ", from AZURE_CLIENT_ID and AZURE_TENANT_ID): "
			for _, want := range append([]string{who}, tc.says...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not say %q: %v", want, err)
				}
			}
			if tc.settings && !strings.Contains(err.Error(), "check AZURE_CLIENT_ID and AZURE_TENANT_ID "+
				"(providers.azure.workloadIdentity.clientId and providers.azure.tenantId in the chart)") {
				t.Errorf("the error does not say where the client ID is set: %v", err)
			}
			if strings.Contains(err.Error(), "\n") {
				t.Errorf("the error spans lines, which a status condition shows badly: %q", err)
			}
			// The token is the pod's secret: only its issuer and subject may be quoted.
			token := azurefake.ServiceAccountToken()
			for part := range strings.SplitSeq(token, ".") {
				if strings.Contains(err.Error(), part) {
					t.Errorf("the error quotes a part of the service account token: %v", err)
				}
			}

			// Apart from who the identity is and where it is set, the two errors are one text.
			account, own := &CredentialError{Identity: Identity{ClientID: testWriter}, Issuer: credErr.Issuer,
				Subject: credErr.Subject, Err: credErr.Err}, credErr.Error()
			normal := strings.NewReplacer(who, "cannot authenticate as X: ", credErr.settings(), "SETTINGS",
				"cannot authenticate as client "+testWriter+": ", "cannot authenticate as X: ", account.settings(), "SETTINGS")
			if a, o := normal.Replace(account.Error()), normal.Replace(own); a != o {
				t.Errorf("the two errors differ in more than the identity:\naccount: %s\nown:     %s", a, o)
			}
		})
	}
}

// What the error of the operator's own identity says when Microsoft Entra ID was not what
// refused it: no Workload ID at all, or a token request that got no answer. The SDK's report
// of the credentials it tried, which spans lines, is put on one.
func TestTheOperatorsOwnIdentityWithoutAnAnswerFromEntra(t *testing.T) {
	sdk := errors.New("DefaultAzureCredential: failed to acquire a token.\nAttempted credentials:\n\t" +
		"EnvironmentCredential: missing environment variable AZURE_CLIENT_ID")
	for _, tc := range []struct {
		name string
		err  *CredentialError
		says []string
	}{
		{"without Workload ID", &CredentialError{Err: sdk},
			[]string{"cannot authenticate as the operator's own identity (AZURE_CLIENT_ID and AZURE_TENANT_ID are not set): ",
				"the operator runs without Microsoft Entra Workload ID (AZURE_FEDERATED_TOKEN_FILE is not set)",
				"az login", "providers.azure.workloadIdentity", "Attempted credentials: EnvironmentCredential: missing"}},
		{"with a token file and no client ID", &CredentialError{TenantID: otherTenant, TokenFile: "/var/run/token",
			Issuer: "https://issuer.example", Subject: "system:serviceaccount:ns:sa", Err: sdk},
			[]string{"the operator's own identity (AZURE_CLIENT_ID is not set; tenant " + otherTenant + ", from AZURE_TENANT_ID)",
				"no token for the operator's service account (token file /var/run/token)",
				"issuer https://issuer.example, subject system:serviceaccount:ns:sa and audience api://AzureADTokenExchange",
				"(deploy/azure/README.md)"}},
		{"with a client ID and no tenant", &CredentialError{ClientID: testReader, TokenFile: "/var/run/token", Err: sdk},
			[]string{"the operator's own identity (client " + testReader + ", from AZURE_CLIENT_ID; AZURE_TENANT_ID is not set)",
				"the cluster's service account issuer, subject system:serviceaccount:<namespace>:<service account>"}},
		{"a code the operator has no words of its own for", &CredentialError{Identity: Identity{ClientID: testReader},
			Err: &azidentity.AuthenticationFailedError{RawResponse: &http.Response{StatusCode: http.StatusBadRequest,
				Body: io.NopCloser(strings.NewReader(`{"error":"invalid_request","error_codes":[90002],` +
					`"error_description":"AADSTS90002: Tenant 'x' not found.\r\nTrace ID: 1"}`))}}},
			[]string{"Microsoft Entra ID gave no token for the operator's service account; the identity needs a federated",
				"invalid_request: AADSTS90002: Tenant 'x' not found."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.err.Error()
			for _, want := range tc.says {
				if !strings.Contains(got, want) {
					t.Errorf("the error does not say %q: %s", want, got)
				}
			}
			if strings.ContainsAny(got, "\n\t\r") {
				t.Errorf("the error spans lines: %q", got)
			}
		})
	}
}

// Without a service account token an account's identity cannot be reached, and the operator
// says so rather than read as itself.
func TestAnAccountIdentityNeedsWorkloadIdentity(t *testing.T) {
	t.Setenv("AZURE_FEDERATED_TOKEN_FILE", "")
	t.Setenv("AZURE_TENANT_ID", "")
	p, fake := newIdentityProvider(t)
	p.discoverer.opts.FederatedTokenFile = ""
	_, err := p.Discover(context.Background(), targetAs(Identity{ClientID: testReader}))
	if _, ok := errors.AsType[*NoTokenFileError](err); !ok || !strings.Contains(err.Error(), "AZURE_FEDERATED_TOKEN_FILE") ||
		!strings.Contains(err.Error(), "client "+testReader) {
		t.Errorf("err = %v, want a NoTokenFileError naming the client and the variable", err)
	}

	p, _ = newFakeProvider(t, Options{})
	p.discoverer.opts.TenantID = ""
	_, err = p.Discover(context.Background(), targetAs(Identity{ClientID: testReader}))
	if _, ok := errors.AsType[*NoTokenFileError](err); !ok || !strings.Contains(err.Error(), "tenant") {
		t.Errorf("err = %v, want a NoTokenFileError about the tenant", err)
	}
	if got := fake.RequestsBy(azurefake.Operator); len(got) != 0 {
		t.Errorf("the operator's own identity was used instead: %v", got)
	}
}

// A subscription of another tenant is reached through an identity of that tenant (a
// multitenant app registration provisioned there), with the account's tenantID; tokens of the
// operator's tenant are refused there.
func TestAnotherTenantsSubscription(t *testing.T) {
	p, fake := newIdentityProvider(t)
	fake.SetTenant(contractSubscription, otherTenant)
	fake.AddIdentity(otherTenant, testReader, true)

	_, err := p.Discover(context.Background(), contractTarget())
	if err == nil || !strings.Contains(err.Error(), "InvalidAuthenticationTokenTenant") {
		t.Errorf("the operator's own identity on another tenant's subscription: %v", err)
	}
	account := networkv1.Account{ID: contractSubscription, Azure: &networkv1.AzureAccount{ClientID: testReader,
		TenantID: otherTenant}}
	snap := discoverAll(t, p, targetAs(p.Identity(account, provider.Read)))
	if len(snap.Subnets) != 1 {
		t.Errorf("subnets = %v, want apps", snap.Subnets)
	}
}

// Resource Manager refusing an account's identity (no role assignment) names the identity by
// its client ID, which is what the scope says; ARM names it by its object ID.
func TestAForbiddenIdentityIsNamed(t *testing.T) {
	p, fake := newIdentityProvider(t)
	fake.AddIdentity(azurefake.DefaultTenant, testWriter, true)
	_, err := p.Discover(context.Background(), targetAs(Identity{ClientID: testWriter}))
	if err == nil || errors.Is(err, inventory.ErrThrottled) || !strings.Contains(err.Error(), "as client "+testWriter) ||
		!strings.Contains(err.Error(), "AuthorizationFailed") {
		t.Errorf("err = %v, want AuthorizationFailed as client %s", err, testWriter)
	}
}

func TestAThrottledTokenRequestIsThrottling(t *testing.T) {
	err := &CredentialError{Identity: Identity{ClientID: testReader},
		Err: &azidentity.AuthenticationFailedError{RawResponse: &http.Response{StatusCode: http.StatusTooManyRequests}}}
	if !isThrottle(err) {
		t.Error("a 429 from Microsoft Entra ID is not throttling")
	}
	err.Err = &azidentity.AuthenticationFailedError{RawResponse: &http.Response{StatusCode: http.StatusBadRequest}}
	if isThrottle(err) {
		t.Error("a 400 from Microsoft Entra ID is throttling")
	}
}

func TestCloudConfiguration(t *testing.T) {
	d := newDiscoverer(Options{})
	if got := d.cloudConfig(); got.ActiveDirectoryAuthorityHost != cloud.AzurePublic.ActiveDirectoryAuthorityHost ||
		got.Services[cloud.ResourceManager] != cloud.AzurePublic.Services[cloud.ResourceManager] {
		t.Errorf("the default fake is %+v, want the public fake", got)
	}
	d = newDiscoverer(Options{Cloud: Clouds["AzureUSGovernment"], AuthorityHost: "https://login.example/"})
	got := d.cloudConfig()
	if got.ActiveDirectoryAuthorityHost != "https://login.example/" {
		t.Errorf("authority host = %s", got.ActiveDirectoryAuthorityHost)
	}
	if got.Services[cloud.ResourceManager].Endpoint != "https://management.usgovcloudapi.net" {
		t.Errorf("ARM endpoint = %s", got.Services[cloud.ResourceManager].Endpoint)
	}
	if cloud.AzureGovernment.ActiveDirectoryAuthorityHost == "https://login.example/" {
		t.Error("the override changed the SDK's own configuration")
	}
	for _, name := range []string{"AzurePublic", "AzureUSGovernment", "AzureChina"} {
		if _, ok := Clouds[name]; !ok {
			t.Errorf("no fake %s", name)
		}
	}
}

func TestCheckAccountIdentity(t *testing.T) {
	scope := func(a *networkv1.AzureAccount) *networkv1.NetworkScope {
		return &networkv1.NetworkScope{Spec: networkv1.NetworkScopeSpec{Provider: networkv1.ProviderAzure,
			Regions: []string{"westeurope"}, Accounts: []networkv1.Account{{ID: contractSubscription, Azure: a}}}}
	}
	p := NewProvider(Options{})
	for _, tc := range []struct {
		name     string
		azure    *networkv1.AzureAccount
		errs     []string
		warnings int
	}{
		{"none", nil, nil, 0},
		{"read and write in another tenant", &networkv1.AzureAccount{ClientID: testReader,
			WriteClientID: strings.ToUpper(testWriter), TenantID: otherTenant}, nil, 0},
		{"no UUIDs", &networkv1.AzureAccount{ClientID: "reader", WriteClientID: "writer@example.com",
			TenantID: "contoso.onmicrosoft.com"},
			[]string{"spec.accounts[0].azure.clientID", "spec.accounts[0].azure.writeClientID",
				"spec.accounts[0].azure.tenantID"}, 0},
		{"a tenant alone", &networkv1.AzureAccount{TenantID: otherTenant},
			[]string{"spec.accounts[0].azure.clientID"}, 0},
		{"one identity for both", &networkv1.AzureAccount{ClientID: testReader,
			WriteClientID: strings.ToUpper(testReader)}, nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warnings, errs := p.ValidateScope(scope(tc.azure))
			fields := make([]string, 0, len(errs))
			for _, e := range errs {
				fields = append(fields, e.Field)
			}
			if !slices.Equal(fields, tc.errs) {
				t.Errorf("errors on %v, want %v: %v", fields, tc.errs, errs)
			}
			if len(warnings) != tc.warnings {
				t.Errorf("warnings = %v, want %d", warnings, tc.warnings)
			}
		})
	}
}
