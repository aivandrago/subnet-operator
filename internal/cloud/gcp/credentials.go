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
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"time"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	iamcredentials "google.golang.org/api/iamcredentials/v1"
	"google.golang.org/api/option"
	"k8s.io/apimachinery/pkg/util/validation/field"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
)

// How the operator acts as a project's service account (#48). Its own identity is Application
// Default Credentials: on GKE the Workload Identity of its Kubernetes service account, anywhere
// else a Workload Identity Federation credential configuration (external_account) that the
// chart mounts with a projected service account token. Nothing in the code differs between
// the two; the client libraries read either.
//
// An account's service account is reached by impersonation, the AssumeRole of GCP: the own
// identity asks the IAM Service Account Credentials API for an access token of the service
// account (generateAccessToken), which needs roles/iam.serviceAccountTokenCreator on it. The
// token is cached until shortly before it expires, per service account, and the API clients
// of that identity carry it.

const (
	// tokenScope is the OAuth scope of an impersonated token. What the token may do is limited
	// by the IAM roles of the service account (deploy/gcp), which is how Google recommends
	// limiting it; narrower scopes would only add a second list to keep right.
	tokenScope = "https://www.googleapis.com/auth/cloud-platform"
	// tokenLifetime is how long an impersonated token is valid: an hour, the most Google
	// grants without an organization policy exception.
	tokenLifetime = time.Hour
	// tokenEarlyExpiry is how long before it expires a cached token is replaced, so that no
	// call starts with a token that runs out on the way.
	tokenEarlyExpiry = 5 * time.Minute
	// tokenTimeout bounds one generateAccessToken call. Token sources have no context, and a
	// refresh must not hang a discovery.
	tokenTimeout = 30 * time.Second
)

// ImpersonationError is the operator's own identity failing to get a token of a service
// account. It names the service account and what to grant, since that is what somebody
// reading the scope's status needs; the error from Google is kept, so throttling is still
// recognised as such.
type ImpersonationError struct {
	ServiceAccount string
	Err            error
}

func (e *ImpersonationError) Error() string {
	if code := httpCode(e.Err); code == http.StatusForbidden || code == http.StatusNotFound {
		return fmt.Sprintf("cannot impersonate service account %s: permission denied or no such service account; the "+
			"operator's own identity needs iam.serviceAccounts.getAccessToken on it (roles/iam.serviceAccountTokenCreator "+
			"or deploy/gcp/impersonator-role.yaml), and its project the IAM Service Account Credentials API: %v",
			e.ServiceAccount, e.Err)
	}
	return fmt.Sprintf("cannot impersonate service account %s: %v", e.ServiceAccount, e.Err)
}

func (e *ImpersonationError) Unwrap() error { return e.Err }

func httpCode(err error) int {
	if gerr, ok := errors.AsType[*googleapi.Error](err); ok {
		return gerr.Code
	}
	return 0
}

// impersonation is a token source of one service account, through the IAM Service Account
// Credentials API as the operator's own identity.
type impersonation struct {
	iam            *iamcredentials.Service
	serviceAccount string
}

// Token implements oauth2.TokenSource.
func (i *impersonation) Token() (*oauth2.Token, error) {
	ctx, cancel := context.WithTimeout(context.Background(), tokenTimeout)
	defer cancel()
	resp, err := i.iam.Projects.ServiceAccounts.GenerateAccessToken("projects/-/serviceAccounts/"+i.serviceAccount,
		&iamcredentials.GenerateAccessTokenRequest{
			Scope:    []string{tokenScope},
			Lifetime: fmt.Sprintf("%ds", int(tokenLifetime.Seconds())),
		}).Context(ctx).Do()
	if err != nil {
		return nil, &ImpersonationError{ServiceAccount: i.serviceAccount, Err: err}
	}
	expiry, err := time.Parse(time.RFC3339, resp.ExpireTime)
	if err != nil {
		return nil, &ImpersonationError{ServiceAccount: i.serviceAccount,
			Err: fmt.Errorf("unreadable expiry %q: %w", resp.ExpireTime, err)}
	}
	return &oauth2.Token{AccessToken: resp.AccessToken, TokenType: "Bearer", Expiry: expiry}, nil
}

// iamService returns the IAM Service Account Credentials client of the operator's own
// identity, building it once. The caller holds d.mu.
func (d *Discoverer) iamService(ctx context.Context) (*iamcredentials.Service, error) {
	if d.iam != nil {
		return d.iam, nil
	}
	opts := slices.Clip(d.opts.ClientOptions)
	if d.opts.IAMCredentialsEndpoint != "" {
		opts = append(opts, option.WithEndpoint(d.opts.IAMCredentialsEndpoint))
	}
	svc, err := iamcredentials.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("iam credentials client: %w", err)
	}
	d.iam = svc
	return svc, nil
}

// credentials returns the options of the API clients that act as the identity, and the token
// source they authenticate with when it is not the operator's own. The operator's own identity
// is the configured options as they are: Application Default Credentials, or what a test puts
// there. A service account adds a cached impersonated token source to them. The caller holds
// d.mu.
func (d *Discoverer) credentials(ctx context.Context, id Identity) ([]option.ClientOption, oauth2.TokenSource, error) {
	if id.Own() {
		return d.opts.ClientOptions, nil, nil
	}
	svc, err := d.iamService(ctx)
	if err != nil {
		return nil, nil, err
	}
	ts := oauth2.ReuseTokenSourceWithExpiry(nil, &impersonation{iam: svc, serviceAccount: id.ServiceAccount},
		tokenEarlyExpiry)
	return append(slices.Clip(d.opts.ClientOptions), option.WithTokenSource(ts)), ts, nil
}

// serviceAccountPattern is the shape of a service account email; the CRD checks the same.
var serviceAccountPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{4,61}[a-z0-9]@[a-z0-9][a-z0-9.-]*\.gserviceaccount\.com$`)

// checkAccountIdentity checks the service accounts of one account of a scope. A service
// account may live in any project (a central one per team is common), so unlike an AWS role
// it is not tied to the account's ID. Reading and writing as the same one is allowed, with a
// warning: it gives discovery the write permissions.
func checkAccountIdentity(path *field.Path, account networkv1.Account) ([]string, field.ErrorList) {
	a := account.GCP
	if a == nil {
		return nil, nil
	}
	var errs field.ErrorList
	for _, sa := range []struct{ name, email string }{
		{"serviceAccount", a.ServiceAccount},
		{"writeServiceAccount", a.WriteServiceAccount},
	} {
		if sa.email != "" && !serviceAccountPattern.MatchString(sa.email) {
			errs = append(errs, field.Invalid(path.Child("gcp", sa.name), sa.email,
				"not a service account email, e.g. subnet-reader@my-project.iam.gserviceaccount.com"))
		}
	}
	var warnings []string
	if a.ServiceAccount != "" && a.ServiceAccount == a.WriteServiceAccount {
		warnings = append(warnings, fmt.Sprintf("account %s reads and writes as the same service account %s, so "+
			"discovery carries its write permissions; give writes a service account of their own", account.ID,
			a.ServiceAccount))
	}
	return warnings, errs
}
