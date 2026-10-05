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

package azurefake

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Microsoft Entra ID in memory: the part the SDK's WorkloadIdentityCredential calls (through
// MSAL) to exchange a Kubernetes service account token for an access token of a managed
// identity or app registration, served over HTTPS on a host of its own (AuthorityHost):
//
//	GET  /{tenant}/v2.0/.well-known/openid-configuration
//	POST /{tenant}/oauth2/v2.0/token   (client_credentials with a jwt-bearer client_assertion)
//
// An identity (AddIdentity) is a client ID in a tenant; it gives a token for the fake's
// service account token (ServiceAccountToken) while it is federated, that is, while it has a
// federated identity credential that trusts the operator's service account. Otherwise the
// fake answers as Entra does: AADSTS700016 for a client ID the tenant does not have,
// AADSTS70021 for one without a matching federated identity credential. Every token names its
// principal (the client ID) and tenant, which Resource Manager checks: a subscription answers
// tokens of its own tenant only (InvalidAuthenticationTokenTenant), and only the principals it
// is restricted to (RestrictSubscription, RestrictSubscriptionWrites).

// Operator is the principal of Credential's token: the operator's own identity.
const Operator = "operator"

// DefaultTenant is the tenant of Credential's token and of every subscription AddSubscription
// creates.
const DefaultTenant = "72f988bf-0000-4000-8000-00000000f00d"

// ServiceAccountIssuer and ServiceAccountSubject are the claims of ServiceAccountToken, as a
// cluster's service account issuer would sign them for the operator.
const (
	ServiceAccountIssuer  = "https://oidc.cluster.example/fake"
	ServiceAccountSubject = "system:serviceaccount:subnet-operator-system:subnet-operator"
)

// principal is who a bearer token was issued to.
type principal struct {
	tenant, client string
}

type identity struct {
	federated bool
}

// AuthorityHost is the fake Microsoft Entra authority host, for the SDK's cloud configuration.
// Its certificate is the one Transport trusts. MSAL cannot run instance discovery against it:
// the credentials need DisableInstanceDiscovery.
func (c *Cloud) AuthorityHost() string { return c.login.URL + "/" }

// ServiceAccountToken is the Kubernetes service account token the fake's identities trust: a
// JWT with ServiceAccountIssuer, ServiceAccountSubject and the audience api://AzureADTokenExchange,
// unsigned, since the fake compares it rather than verifies it.
func ServiceAccountToken() string {
	enc := func(v any) string {
		raw, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	return enc(map[string]string{"alg": "RS256", "kid": "fake"}) + "." +
		enc(map[string]any{"iss": ServiceAccountIssuer, "sub": ServiceAccountSubject,
			"aud": []string{"api://AzureADTokenExchange"}}) + ".fake-signature"
}

// WriteServiceAccountToken writes ServiceAccountToken to a file in dir, where the kubelet would
// project it, and returns the file's path.
func WriteServiceAccountToken(dir string) (string, error) {
	path := filepath.Join(dir, "azure-identity-token")
	return path, os.WriteFile(path, []byte(ServiceAccountToken()), 0o600)
}

// AddIdentity creates an identity, a managed identity or app registration with the client ID,
// in the tenant, or changes whether it is federated: whether it trusts ServiceAccountToken.
func (c *Cloud) AddIdentity(tenant, clientID string, federated bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.identities[principal{strings.ToLower(tenant), strings.ToLower(clientID)}] = &identity{federated: federated}
}

// RemoveIdentity deletes an identity.
func (c *Cloud) RemoveIdentity(tenant, clientID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.identities, principal{strings.ToLower(tenant), strings.ToLower(clientID)})
}

// SetTenant moves a subscription to another tenant: from then on it answers only tokens of
// that tenant.
func (c *Cloud) SetTenant(sub, tenant string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subscription(sub).tenant = strings.ToLower(tenant)
}

// RestrictSubscription lets only the principals (client IDs, or Operator) call the
// subscription; everyone else gets 403 AuthorizationFailed, as a principal without a role
// assignment would. An unrestricted subscription answers every principal of its tenant.
func (c *Cloud) RestrictSubscription(sub string, principals ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subscription(sub).principals = lower(principals)
}

// RestrictSubscriptionWrites lets only the principals write to the subscription (any call but
// a GET), as a subscription where the others hold reader roles only. They must also be let in
// by RestrictSubscription, if it is restricted. A subscription without write restrictions
// takes writes from every principal it lets in.
func (c *Cloud) RestrictSubscriptionWrites(sub string, principals ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subscription(sub).writers = lower(principals)
}

func lower(values []string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, strings.ToLower(v))
	}
	return out
}

// TokensIssued returns how many tokens the fake issued for the client ID.
func (c *Cloud) TokensIssued(clientID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.issued[strings.ToLower(clientID)]
}

// RequestsBy returns the Resource Manager requests the principal (a client ID, or Operator)
// made so far, as Requests formats them. Unlike Requests it forgets nothing.
func (c *Cloud) RequestsBy(principal string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.requestsBy[strings.ToLower(principal)])
}

func newLogin(c *Cloud) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{tenant}/v2.0/.well-known/openid-configuration", c.openIDConfiguration)
	mux.HandleFunc("POST /{tenant}/oauth2/v2.0/token", c.token)
	s := httptest.NewUnstartedServer(mux)
	// Clients the SDK keeps alive are cut off when the fake closes; that is not worth a line.
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.StartTLS()
	return s
}

func (c *Cloud) openIDConfiguration(w http.ResponseWriter, r *http.Request) {
	base := c.login.URL + "/" + r.PathValue("tenant")
	writeJSON(w, map[string]any{
		"issuer":                 base + "/v2.0",
		"authorization_endpoint": base + "/oauth2/v2.0/authorize",
		"token_endpoint":         base + "/oauth2/v2.0/token",
		"token_endpoint_auth_methods_supported": []string{"client_secret_post", "private_key_jwt",
			"client_secret_basic"},
	})
}

// token answers a client credentials request with a federated client assertion, as Entra
// does.
func (c *Cloud) token(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := r.ParseForm(); err != nil {
		writeOAuthError(w, http.StatusBadRequest, "invalid_request", 900144, err.Error())
		return
	}
	tenant := strings.ToLower(r.PathValue("tenant"))
	client := strings.ToLower(r.PostForm.Get("client_id"))
	switch {
	case r.PostForm.Get("grant_type") != "client_credentials":
		writeOAuthError(w, http.StatusBadRequest, "unsupported_grant_type", 70003,
			"The app requested an unsupported grant type.")
		return
	case r.PostForm.Get("client_assertion_type") != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer":
		writeOAuthError(w, http.StatusUnauthorized, "invalid_client", 7000218,
			"The request body must contain the following parameter: 'client_assertion' or 'client_secret'.")
		return
	}
	id := c.identities[principal{tenant, client}]
	switch {
	case id == nil:
		writeOAuthError(w, http.StatusBadRequest, "unauthorized_client", 700016, fmt.Sprintf(
			"Application with identifier '%s' was not found in the directory '%s'.", client, tenant))
		return
	case !id.federated || r.PostForm.Get("client_assertion") != ServiceAccountToken():
		writeOAuthError(w, http.StatusBadRequest, "invalid_client", 70021, fmt.Sprintf(
			"No matching federated identity record found for presented assertion. Assertion Issuer: '%s'. "+
				"Assertion Subject: '%s'. Assertion Audience: 'api://AzureADTokenExchange'.",
			ServiceAccountIssuer, ServiceAccountSubject))
		return
	}
	c.issued[client]++
	token := fmt.Sprintf("%s.%s.%s.%d", Token, tenant, client, c.issued[client])
	c.tokens[token] = principal{tenant, client}
	writeJSON(w, map[string]any{"token_type": "Bearer", "expires_in": 3599, "ext_expires_in": 3599,
		"access_token": token})
}

// writeOAuthError answers the way Entra does: {"error", "error_description", "error_codes"}.
func writeOAuthError(w http.ResponseWriter, status int, code string, aadsts int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		errorKey:            code,
		"error_description": fmt.Sprintf("AADSTS%d: %s Trace ID: 00000000-0000-0000-0000-000000000000", aadsts, message),
		"error_codes":       []int{aadsts},
	})
}

// authorize checks the bearer token of a Resource Manager request against the subscription:
// its tenant, and the principals the subscription answers. It answers and returns false when
// the request may not go on.
func (c *Cloud) authorize(w http.ResponseWriter, r *http.Request, s *subscription, who principal) bool {
	if who.tenant != s.tenant {
		writeError(w, http.StatusUnauthorized, "InvalidAuthenticationTokenTenant", fmt.Sprintf("The access token is "+
			"from the wrong issuer 'https://sts.windows.net/%s/'. It must match the tenant "+
			"'https://sts.windows.net/%s/' associated with this subscription.", who.tenant, s.tenant))
		return false
	}
	allowed := s.principals == nil || slices.Contains(s.principals, who.client)
	if r.Method != http.MethodGet {
		allowed = allowed && (s.writers == nil || slices.Contains(s.writers, who.client))
	}
	if !allowed {
		writeError(w, http.StatusForbidden, "AuthorizationFailed", fmt.Sprintf("The client '%s' with object id "+
			"'%s' does not have authorization to perform action '%s' over scope '/subscriptions/%s' or the scope is "+
			"invalid. If access was recently granted, please refresh your credentials.", who.client, who.client,
			action(r), s.id))
		return false
	}
	return true
}

// errorKey is the member an ARM or Entra error answer holds its error in.
const errorKey = "error"
