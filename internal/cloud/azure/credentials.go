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
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/cloud"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"k8s.io/apimachinery/pkg/util/validation/field"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
)

// How the operator acts as a subscription's identity (#54). Its own identity is the SDK's
// DefaultAzureCredential: on Kubernetes, Microsoft Entra Workload ID, a service account token
// (AZURE_FEDERATED_TOKEN_FILE, projected by the AKS webhook or by the chart) exchanged for a
// token of the operator's managed identity or app registration (AZURE_CLIENT_ID).
//
// Azure has no AssumeRole and no impersonation API. An account's identity (accounts[].azure,
// a user-assigned managed identity or an app registration) is reached the same way as the
// operator's own: the same service account token is exchanged for a token of that client ID,
// which works when the identity has a federated identity credential that trusts the operator's
// service account (issuer, subject system:serviceaccount:<namespace>:<name>). So one token file
// serves every identity, and each keeps its own role assignments: a read and a write identity
// per subscription, or per landing zone, and identities in other tenants (multitenant app
// registrations provisioned there), without the operator's own identity holding any role.
//
// There is one credential per identity (tenant and client ID), built at its first use and kept;
// the SDK caches its tokens until shortly before they expire, and rereads the token file as the
// kubelet rotates it.

const (
	// envTenantID and envFederatedTokenFile are where the AKS Workload Identity webhook, or
	// the chart, puts the operator's own tenant and service account token file.
	envTenantID           = "AZURE_TENANT_ID"
	envFederatedTokenFile = "AZURE_FEDERATED_TOKEN_FILE"
)

// uuidPattern is the shape of a client, tenant or subscription ID, in either case; the CRD
// checks the same.
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Clouds are the Azure clouds --azure-cloud names: the Resource Manager endpoint and audience,
// and the Microsoft Entra authority host, of each.
var Clouds = map[string]cloud.Configuration{
	"AzurePublic":       cloud.AzurePublic,
	"AzureUSGovernment": cloud.AzureGovernment,
	"AzureChina":        cloud.AzureChina,
}

// CredentialError is a token of an identity that cannot be had: Microsoft Entra ID refused the
// operator's service account token for it (no federated identity credential that trusts it,
// the wrong tenant, an identity that does not exist), or the operator has no token to offer.
// It names the identity and what to set up, since that is what somebody reading the scope's
// status needs; the SDK's error is kept, so a throttled token request is still recognised as
// throttling.
type CredentialError struct {
	Identity Identity
	// Subject and Issuer are the operator's service account token's, which a federated
	// identity credential must name; empty when the token could not be read.
	Subject, Issuer string
	Err             error
}

func (e *CredentialError) Error() string {
	var trust string
	switch {
	case e.Subject != "" && e.Issuer != "":
		trust = fmt.Sprintf("issuer %s, subject %s", e.Issuer, e.Subject)
	default:
		trust = "the cluster's service account issuer, subject system:serviceaccount:<namespace>:<service account>"
	}
	return fmt.Sprintf("cannot authenticate as %s: Microsoft Entra ID gave no token for the operator's service account; "+
		"the identity needs a federated identity credential with %s and audience api://AzureADTokenExchange "+
		"(deploy/azure/README.md): %s", e.Identity, trust, summary(e.Err))
}

func (e *CredentialError) Unwrap() error { return e.Err }

// summary is the gist of an SDK authentication error: the AADSTS line of Microsoft Entra ID's
// answer rather than the SDK's whole report of the exchange.
func summary(err error) string {
	authErr, ok := errors.AsType[*azidentity.AuthenticationFailedError](err)
	if !ok || authErr.RawResponse == nil || authErr.RawResponse.Body == nil {
		return err.Error()
	}
	body, rerr := io.ReadAll(authErr.RawResponse.Body)
	authErr.RawResponse.Body = io.NopCloser(bytes.NewReader(body))
	if rerr != nil {
		return err.Error()
	}
	var answer struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if json.Unmarshal(body, &answer) != nil || answer.Description == "" {
		return fmt.Sprintf("HTTP %d from Microsoft Entra ID", authErr.RawResponse.StatusCode)
	}
	desc, _, _ := strings.Cut(answer.Description, "\n")
	desc, _, _ = strings.Cut(strings.TrimSpace(desc), " Trace ID:")
	return fmt.Sprintf("%s: %s", answer.Error, strings.TrimSpace(desc))
}

// NoTokenFileError is an account identity without the service account token to exchange: the
// operator runs without Workload Identity.
type NoTokenFileError struct {
	Identity Identity
	Reason   string
}

func (e *NoTokenFileError) Error() string {
	return fmt.Sprintf("cannot authenticate as %s: %s; an account's azure.clientID and writeClientID need Microsoft "+
		"Entra Workload ID for the operator (providers.azure.workloadIdentity in the chart, or %s and %s)",
		e.Identity, e.Reason, envFederatedTokenFile, envTenantID)
}

// identityCredential is the credential of one identity: it turns the SDK's errors into a
// CredentialError that names the identity.
type identityCredential struct {
	id        Identity
	tokenFile string
	cred      azcore.TokenCredential
}

// GetToken implements azcore.TokenCredential.
func (c *identityCredential) GetToken(ctx context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	tok, err := c.cred.GetToken(ctx, opts)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return tok, err
		}
		issuer, subject := tokenClaims(c.tokenFile)
		return tok, &CredentialError{Identity: c.id, Issuer: issuer, Subject: subject, Err: err}
	}
	return tok, nil
}

// tokenClaims reads the issuer and subject of the service account token, for the error that
// says what a federated identity credential must name. The token is not verified: Microsoft
// Entra ID does that; this is only a hint.
func tokenClaims(file string) (issuer, subject string) {
	if file == "" {
		return "", ""
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return "", ""
	}
	parts := strings.Split(strings.TrimSpace(string(raw)), ".")
	if len(parts) != 3 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var claims struct {
		Issuer  string `json:"iss"`
		Subject string `json:"sub"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "", ""
	}
	return claims.Issuer, claims.Subject
}

// cloudConfig is the cloud the operator talks to: --azure-cloud (the public cloud by default),
// with the authority host and the Resource Manager endpoint replaced where the options say.
func (d *Discoverer) cloudConfig() cloud.Configuration {
	base := d.opts.Cloud
	if base.ActiveDirectoryAuthorityHost == "" && len(base.Services) == 0 {
		base = cloud.AzurePublic
	}
	cfg := cloud.Configuration{
		ActiveDirectoryAuthorityHost: base.ActiveDirectoryAuthorityHost,
		Services:                     map[cloud.ServiceName]cloud.ServiceConfiguration{},
	}
	maps.Copy(cfg.Services, base.Services)
	if d.opts.AuthorityHost != "" {
		cfg.ActiveDirectoryAuthorityHost = d.opts.AuthorityHost
	}
	if d.opts.ResourceManagerEndpoint != "" {
		rm := cfg.Services[cloud.ResourceManager]
		rm.Endpoint = d.opts.ResourceManagerEndpoint
		cfg.Services[cloud.ResourceManager] = rm
	}
	return cfg
}

func (d *Discoverer) azcoreOptions() azcore.ClientOptions {
	return azcore.ClientOptions{Cloud: d.cloudConfig(), Transport: d.opts.Transport}
}

// credentialFor returns the credential of an identity, building it at its first use. The
// operator's own is Options.Credential or DefaultAzureCredential; an account's is a Workload
// Identity credential for its client ID and tenant, from the operator's own token file. An
// account's identity that cannot be built fails, rather than falls back to the operator's own
// identity, which would act with permissions nobody meant to use.
func (d *Discoverer) credentialFor(id Identity) (azcore.TokenCredential, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if id.Own() {
		return d.ownCredential()
	}
	key := d.resolve(id)
	if c, ok := d.credentials[key]; ok {
		return c, nil
	}
	tokenFile := d.opts.FederatedTokenFile
	if tokenFile == "" {
		tokenFile = os.Getenv(envFederatedTokenFile)
	}
	if tokenFile == "" {
		return nil, &NoTokenFileError{Identity: id, Reason: "the operator has no service account token to exchange (" +
			envFederatedTokenFile + " is not set)"}
	}
	if key.TenantID == "" {
		return nil, &NoTokenFileError{Identity: id, Reason: "neither the account (azure.tenantID) nor the operator (" +
			envTenantID + ") names a tenant"}
	}
	cred, err := azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
		ClientOptions:            d.azcoreOptions(),
		ClientID:                 key.ClientID,
		TenantID:                 key.TenantID,
		TokenFilePath:            tokenFile,
		DisableInstanceDiscovery: d.opts.DisableInstanceDiscovery,
	})
	if err != nil {
		return nil, fmt.Errorf("cannot authenticate as %s: %w", id, err)
	}
	c := &identityCredential{id: id, tokenFile: tokenFile, cred: cred}
	d.credentials[key] = c
	return c, nil
}

// ownCredential returns the operator's own credential. The caller holds d.mu.
func (d *Discoverer) ownCredential() (azcore.TokenCredential, error) {
	if d.credential != nil {
		return d.credential, nil
	}
	if d.opts.Credential != nil {
		d.credential = d.opts.Credential
		return d.credential, nil
	}
	cred, err := azidentity.NewDefaultAzureCredential(&azidentity.DefaultAzureCredentialOptions{
		ClientOptions: d.azcoreOptions()})
	if err != nil {
		return nil, fmt.Errorf("no Azure credentials for the operator (Workload Identity, managed identity "+
			"or environment): %w", err)
	}
	d.credential = cred
	return cred, nil
}

// resolve is the identity a credential is kept under: in lowercase, and in the operator's own
// tenant when the account names none, so that the same identity is one credential however the
// scopes spell it.
func (d *Discoverer) resolve(id Identity) Identity {
	key := Identity{TenantID: strings.ToLower(id.TenantID), ClientID: strings.ToLower(id.ClientID)}
	if key.TenantID == "" {
		key.TenantID = d.opts.TenantID
		if key.TenantID == "" {
			key.TenantID = os.Getenv(envTenantID)
		}
		key.TenantID = strings.ToLower(key.TenantID)
	}
	return key
}

// throttledToken reports whether the error is Microsoft Entra ID rate-limiting a token request.
func throttledToken(err error) bool {
	authErr, ok := errors.AsType[*azidentity.AuthenticationFailedError](err)
	return ok && authErr.RawResponse != nil && authErr.RawResponse.StatusCode == http.StatusTooManyRequests
}

// checkAccountIdentity checks the identities of one account of a scope: client and tenant IDs
// are UUIDs, a tenant only comes with a client ID, and reading and writing as the same
// identity is allowed with a warning, since it gives discovery the write permissions. Unlike
// an AWS role an identity is not tied to the subscription: one may read many.
func checkAccountIdentity(path *field.Path, account networkv1.Account) ([]string, field.ErrorList) {
	a := account.Azure
	if a == nil {
		return nil, nil
	}
	var errs field.ErrorList
	for _, id := range []struct{ name, value string }{
		{"clientID", a.ClientID},
		{"writeClientID", a.WriteClientID},
		{"tenantID", a.TenantID},
	} {
		if id.value != "" && !uuidPattern.MatchString(id.value) {
			errs = append(errs, field.Invalid(path.Child("azure", id.name), id.value,
				"not a Microsoft Entra ID: a UUID, as the portal shows it"))
		}
	}
	if a.TenantID != "" && a.ClientID == "" && a.WriteClientID == "" {
		errs = append(errs, field.Required(path.Child("azure", "clientID"),
			"azure.tenantID needs azure.clientID or azure.writeClientID: it is the tenant they are in"))
	}
	var warnings []string
	if a.ClientID != "" && strings.EqualFold(a.ClientID, a.WriteClientID) {
		warnings = append(warnings, fmt.Sprintf("account %s reads and writes as the same identity %s, so discovery "+
			"carries its write permissions; give writes an identity of their own", account.ID, a.ClientID))
	}
	return warnings, errs
}
