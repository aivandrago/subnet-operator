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

package gcpfake

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// Identities in the fake. Every call carries a bearer token, and the fake knows whose it is:
// the operator's own (the token of ClientOptions), or a service account's, issued by the fake
// IAM Service Account Credentials API
//
//	POST <iam>/v1/projects/-/serviceAccounts/{email}:generateAccessToken
//
// to a caller the service account lets impersonate it. A project that is restricted answers
// only the principals it names, with 403 as Google does for a missing permission.

// Operator is the principal of the operator's own identity (Application Default Credentials).
const Operator = "operator"

// operatorToken is the bearer token of the operator's own identity.
const operatorToken = "gcpfake-operator"

// DefaultTokenLifetime is how long an issued token is valid unless SetTokenLifetime says
// otherwise: an hour, Google's default.
const DefaultTokenLifetime = time.Hour

// IAMCredentialsEndpoint is the fake's IAM Service Account Credentials endpoint.
func (c *Cloud) IAMCredentialsEndpoint() string { return c.server.URL + "/iam/" }

// AddServiceAccount creates a service account that the principals may impersonate
// (roles/iam.serviceAccountTokenCreator on it). Principals are Operator or other service
// accounts' emails.
func (c *Cloud) AddServiceAccount(email string, impersonators ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.serviceAccounts[email] = slices.Clone(impersonators)
}

// RestrictProject lets only the principals call the project's Compute and Resource Manager
// resources; everyone else gets 403. An unrestricted project answers every principal.
func (c *Cloud) RestrictProject(projectID string, principals ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.project(projectID).principals = slices.Clone(principals)
}

// RestrictProjectWrites lets only the principals write to the project: create subnetworks,
// bind tags to its resources and create values of its tag keys, as a project where the others
// hold read roles only. The principals must also be let in by RestrictProject, if it is
// restricted. A project without write restrictions takes writes from every principal it lets in.
func (c *Cloud) RestrictProjectWrites(projectID string, principals ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.project(projectID).writers = slices.Clone(principals)
}

// SetTokenLifetime sets how long the tokens issued from now on are valid.
func (c *Cloud) SetTokenLifetime(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tokenLifetime = d
}

// TokensIssued returns how many tokens the fake issued for the service account.
func (c *Cloud) TokensIssued(email string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.issued[email]
}

// RequestsBy returns the requests the principal made so far, as Requests formats them. Unlike
// Requests it forgets nothing.
func (c *Cloud) RequestsBy(principal string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.requestsBy[principal])
}

// principalOf returns whose token the request carries, or "" for none the fake issued.
func (c *Cloud) principalOf(r *http.Request) string {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return ""
	}
	if token == operatorToken {
		return Operator
	}
	t, ok := c.tokens[token]
	if !ok || time.Now().After(t.expiry) {
		return ""
	}
	return t.principal
}

// authorized answers 401 for a request without a valid token, and 403 for a principal the
// project does not let in or, for a write, does not let write. It returns false when it answered.
func (c *Cloud) authorized(w http.ResponseWriter, r *http.Request, p *project) bool {
	principal := c.principalOf(r)
	if principal == "" {
		writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "",
			"Request had invalid authentication credentials. Expected OAuth 2 access token.")
		return false
	}
	if p != nil && p.principals != nil && !slices.Contains(p.principals, principal) {
		writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "forbidden",
			fmt.Sprintf("Required permission is missing for %s on project %s", principal, p.id))
		return false
	}
	// Every write the fake serves is a POST, and every POST but generateAccessToken (which
	// does not come here) is a write.
	if p != nil && p.writers != nil && r.Method == http.MethodPost && !slices.Contains(p.writers, principal) {
		writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "forbidden",
			fmt.Sprintf("Required permission is missing for %s to write in project %s", principal, p.id))
		return false
	}
	return true
}

type issuedToken struct {
	principal string
	expiry    time.Time
}

// generateAccessToken is iamcredentials projects.serviceAccounts.generateAccessToken. Google
// answers 403 both for a service account the caller may not impersonate and for one that does
// not exist, so the fake does too.
func (c *Cloud) generateAccessToken(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.begin(w, r, true) {
		return
	}
	email, ok := strings.CutSuffix(r.PathValue("name"), ":generateAccessToken")
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "notFound", "unknown method")
		return
	}
	caller := c.principalOf(r)
	if caller == "" {
		writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "",
			"Request had invalid authentication credentials. Expected OAuth 2 access token.")
		return
	}
	impersonators, exists := c.serviceAccounts[email]
	if !exists || !slices.Contains(impersonators, caller) {
		writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "",
			"Permission 'iam.serviceAccounts.getAccessToken' denied on resource (or it may not exist).")
		return
	}
	var req struct {
		Scope    []string `json:"scope"`
		Lifetime string   `json:"lifetime"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Scope) == 0 {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "badRequest", "scope is required")
		return
	}
	c.issued[email]++
	token := fmt.Sprintf("gcpfake-token-%d", c.nextID)
	c.nextID++
	expiry := time.Now().Add(c.tokenLifetime).UTC()
	c.tokens[token] = issuedToken{principal: email, expiry: expiry}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": token, "expireTime": expiry.Format(time.RFC3339)})
}
