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

// Package dashboard serves the dashboard app inside the cluster and passes its API calls on
// to the Kubernetes API server as the person using it.
//
// The dashboard has no identity of its own. Its pod runs without a service account token and
// without RBAC, and every call it forwards carries the bearer token of the incoming request,
// untouched. What a viewer sees is what their own RBAC lets them read; what they import is
// recorded under their own name. A dashboard that read with a service account of its own
// would show everyone everything its account can see, and create imports that anybody who can
// reach it can ask for: the confused deputy this design exists to rule out.
package dashboard

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"regexp"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Group is the only API group the dashboard reads.
const Group = "aws.hypersurgery"

// SelfSubjectReviewPath is the one call outside Group the dashboard forwards: it asks the API
// server who the token belongs to, so the app can show the viewer who they are signed in as.
// A SelfSubjectReview is answered from the request's own credentials and stored nowhere.
const SelfSubjectReviewPath = "/apis/authentication.k8s.io/v1/selfsubjectreviews"

// DefaultMaxBodyBytes caps a forwarded request body. A ResourceImport is a few hundred bytes;
// anything near this size is not one.
const DefaultMaxBodyBytes = 64 << 10

// CSRFHeader must be present on every write. A browser only sends a custom header
// cross-origin after a CORS preflight, which the dashboard never answers, so another site
// cannot make a signed-in viewer's browser create an import. The header matters because an
// SSO proxy in front of the dashboard typically turns a session cookie into the bearer token:
// without this check, the cookie alone would authorise the write.
const CSRFHeader = "X-Subnet-Dashboard"

var (
	versionPattern = regexp.MustCompile(`^v[1-9][0-9]*((alpha|beta)[1-9][0-9]*)?$`)
	// resourcePattern is a plural resource name: a DNS label.
	resourcePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	// namePattern is an object or namespace name: a DNS subdomain.
	namePattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
)

// forwardedHeaders are the only request headers that reach the API server. An allowlist
// rather than a list of what to drop: Impersonate-*, the front-proxy X-Remote-* headers,
// cookies and whatever a future Kubernetes version gives meaning to are all left behind
// without anyone having to remember them.
var forwardedHeaders = []string{"Authorization", "Accept", "Content-Type"}

// Proxy forwards the allowed API calls to the API server with the caller's own token.
type Proxy struct {
	upstream     *url.URL
	maxBodyBytes int64
	reverse      *httputil.ReverseProxy
	logf         func(format string, args ...any)
}

// ProxyOptions configures a Proxy.
type ProxyOptions struct {
	// Upstream is the API server, e.g. https://10.96.0.1:443.
	Upstream *url.URL
	// Transport reaches the upstream. It must trust the API server's CA and must not add
	// credentials of its own.
	Transport http.RoundTripper
	// MaxBodyBytes caps a forwarded request body; DefaultMaxBodyBytes when zero.
	MaxBodyBytes int64
	// Logf reports denied requests and upstream failures. Tokens are never passed to it.
	Logf func(format string, args ...any)
}

// NewProxy returns a Proxy for the given API server.
func NewProxy(opts ProxyOptions) *Proxy {
	p := &Proxy{
		upstream:     opts.Upstream,
		maxBodyBytes: opts.MaxBodyBytes,
		logf:         opts.Logf,
	}
	if p.maxBodyBytes <= 0 {
		p.maxBodyBytes = DefaultMaxBodyBytes
	}
	if p.logf == nil {
		p.logf = func(string, ...any) {}
	}
	p.reverse = &httputil.ReverseProxy{
		Transport: opts.Transport,
		// A watch streams events as they happen; buffering would hold them back.
		FlushInterval: -1,
		Rewrite:       p.rewrite,
		ModifyResponse: func(res *http.Response) error {
			// Inventory data is not for shared caches, and the API server never needs to set a
			// cookie on the dashboard's origin.
			res.Header.Set("Cache-Control", "no-store")
			res.Header.Del("Set-Cookie")
			// The dashboard sets its own; the API server's copy would only duplicate it.
			res.Header.Del("X-Content-Type-Options")
			if isWatch(res.Request.URL) {
				// ingress-nginx and other nginx-based proxies in front buffer responses by
				// default, which would hold a watch's events back until the stream ends.
				res.Header.Set("X-Accel-Buffering", "no")
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			p.logf("Could not reach the API server: %s %s: %v", r.Method, r.URL.Path, err)
			writeStatus(w, http.StatusBadGateway, metav1.StatusReasonServiceUnavailable,
				"the dashboard could not reach the Kubernetes API server")
		},
	}
	return p
}

// rewrite builds the upstream request from scratch: the path the allowlist approved, the
// query, and the few headers in forwardedHeaders. ReverseProxy has already removed the
// hop-by-hop headers, and Rewrite (unlike Director) adds no X-Forwarded-* of its own.
func (p *Proxy) rewrite(pr *httputil.ProxyRequest) {
	pr.SetURL(p.upstream)
	pr.Out.URL.Path = singleJoin(p.upstream.Path, pr.In.URL.Path)
	pr.Out.URL.RawPath = ""
	pr.Out.URL.RawQuery = pr.In.URL.RawQuery
	pr.Out.Host = p.upstream.Host

	out := http.Header{}
	for _, h := range forwardedHeaders {
		if v := pr.In.Header.Values(h); len(v) > 0 {
			out[h] = append([]string(nil), v...)
		}
	}
	out.Set("User-Agent", "subnet-operator-dashboard")
	pr.Out.Header = out
}

func singleJoin(base, p string) string {
	return strings.TrimSuffix(base, "/") + p
}

// ServeHTTP checks the request and forwards it, or answers it with a Kubernetes Status.
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !hasBearerToken(r) {
		// No token means no identity, and the dashboard has none of its own to fall back on.
		w.Header().Set("WWW-Authenticate", `Bearer realm="kubernetes"`)
		writeStatus(w, http.StatusUnauthorized, metav1.StatusReasonUnauthorized,
			"the dashboard forwards your own Kubernetes token and none was sent: sign in, or paste a token")
		return
	}

	if reason := Allowed(r.Method, r.URL); reason != "" {
		p.logf("Denied request: %s %s: %s", r.Method, r.URL.EscapedPath(), reason)
		writeStatus(w, http.StatusForbidden, metav1.StatusReasonForbidden,
			"the dashboard does not forward this request: "+reason)
		return
	}

	if r.Method == http.MethodPost {
		// Every POST gets the same checks, the SelfSubjectReview too. It changes nothing, but a
		// single rule for everything that is not a GET leaves nothing to reason about case by
		// case, and the app sends the header anyway.
		if reason := checkWrite(r); reason != "" {
			p.logf("Denied write: %s %s: %s", r.Method, r.URL.EscapedPath(), reason)
			writeStatus(w, http.StatusForbidden, metav1.StatusReasonForbidden, reason)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, p.maxBodyBytes))
		if err != nil {
			if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
				writeStatus(w, http.StatusRequestEntityTooLarge, metav1.StatusReasonRequestEntityTooLarge,
					fmt.Sprintf("the request body is larger than %d bytes", p.maxBodyBytes))
				return
			}
			writeStatus(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, "could not read the request body")
			return
		}
		if r.URL.Path == SelfSubjectReviewPath {
			if reason := checkSelfSubjectReview(body); reason != "" {
				p.logf("Denied write: %s %s: %s", r.Method, r.URL.EscapedPath(), reason)
				writeStatus(w, http.StatusForbidden, metav1.StatusReasonForbidden, reason)
				return
			}
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	} else {
		// A GET carries no body worth forwarding.
		r.Body = http.NoBody
		r.ContentLength = 0
	}
	p.reverse.ServeHTTP(w, r)
}

// hasBearerToken reports whether the request carries exactly one "Authorization: Bearer"
// header. Any other scheme counts as no token: Basic credentials are not something the
// dashboard should be relaying. The token itself is forwarded as it came, never inspected.
func hasBearerToken(r *http.Request) bool {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return false
	}
	scheme, token, found := strings.Cut(values[0], " ")
	return found && strings.EqualFold(scheme, "Bearer") && token != "" && !strings.ContainsAny(token, " \t\r\n")
}

// Allowed reports why a request may not be forwarded, or "" when it may. Reads of any
// resource in the aws.hypersurgery group are allowed — get, list and watch are all GETs —
// and so are creating a ResourceImport and creating a SelfSubjectReview. Nothing else is: not
// Secrets, not other groups, not the rest of authentication.k8s.io, not deletes, updates or
// patches, not discovery.
func Allowed(method string, u *url.URL) string {
	// An escaped path is one the API server might read differently from the allowlist:
	// %2F is a slash to one and part of a name to the other.
	if u.RawPath != "" && u.RawPath != u.Path {
		return "escaped characters in the path"
	}
	p := u.Path
	if p != path.Clean(p) || strings.Contains(p, "//") {
		return "the path is not in canonical form"
	}
	if p == SelfSubjectReviewPath {
		if method != http.MethodPost {
			return "the only request to authentication.k8s.io the dashboard forwards is creating a SelfSubjectReview"
		}
		// It takes no parameters the app needs; dryRun, fieldManager and the like stay behind.
		if u.RawQuery != "" {
			return "a SelfSubjectReview is forwarded without query parameters"
		}
		return ""
	}
	parts := strings.Split(strings.TrimPrefix(p, "/"), "/")
	if len(parts) < 4 || parts[0] != "apis" {
		return "only the " + Group + " API is forwarded"
	}
	if parts[1] != Group {
		return "only the " + Group + " API is forwarded"
	}
	if !versionPattern.MatchString(parts[2]) {
		return "not an API version"
	}
	rest := parts[3:]

	namespaced := false
	if rest[0] == "namespaces" {
		if len(rest) < 3 {
			// /namespaces and /namespaces/<ns> are the Namespace resource, not ours.
			return "namespaces are not forwarded"
		}
		if !validName(rest[1]) {
			return "not a namespace name"
		}
		namespaced = true
		rest = rest[2:]
	}
	resource := rest[0]
	if !resourcePattern.MatchString(resource) {
		return "not a resource name"
	}
	switch len(rest) {
	case 1:
	case 2:
		if !validName(rest[1]) {
			return "not an object name"
		}
	case 3:
		if !validName(rest[1]) || rest[2] != "status" {
			return "only the status subresource is forwarded"
		}
	default:
		return "unknown path"
	}

	switch method {
	case http.MethodGet:
		return ""
	case http.MethodPost:
		if namespaced && resource == "resourceimports" && len(rest) == 1 {
			return ""
		}
		return "the only write the dashboard forwards is creating a ResourceImport in a namespace"
	default:
		return method + " is not forwarded"
	}
}

func validName(s string) bool {
	return len(s) <= 253 && namePattern.MatchString(s)
}

// isWatch reports whether a request asks for a watch stream rather than a single answer.
func isWatch(u *url.URL) bool {
	if u == nil {
		return false
	}
	v := u.Query().Get("watch")
	return v == "1" || v == "true"
}

// checkSelfSubjectReview accepts exactly the body the app sends: an empty SelfSubjectReview,
// with no field beyond apiVersion and kind. The API server would ignore anything else, but a
// body that is not what it claims to be has no business passing through.
func checkSelfSubjectReview(body []byte) string {
	const reason = "the body must be an empty authentication.k8s.io/v1 SelfSubjectReview"
	dec := json.NewDecoder(bytes.NewReader(body))
	var fields map[string]json.RawMessage
	if err := dec.Decode(&fields); err != nil || dec.More() {
		return reason
	}
	if _, err := dec.Token(); err != io.EOF {
		return reason
	}
	var apiVersion, kind string
	for k, v := range fields {
		var err error
		switch k {
		case "apiVersion":
			err = json.Unmarshal(v, &apiVersion)
		case "kind":
			err = json.Unmarshal(v, &kind)
		default:
			return reason
		}
		if err != nil {
			return reason
		}
	}
	if apiVersion != "authentication.k8s.io/v1" || kind != "SelfSubjectReview" {
		return reason
	}
	return ""
}

// checkWrite keeps writes to the dashboard's own page: a JSON body, the custom header a
// cross-origin page cannot send without a preflight, and no browser signal that the request
// came from somewhere else.
func checkWrite(r *http.Request) string {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return "a write must be sent as application/json"
	}
	if r.Header.Get(CSRFHeader) == "" {
		return "a write must carry the " + CSRFHeader + " header"
	}
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return "a write must come from the dashboard's own page"
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		// An SSO proxy in front may rewrite Host to the Service's name; the host the browser
		// used then arrives as X-Forwarded-Host. Trusting it is safe here: a page on another
		// origin could only set it together with the custom header above, which it cannot.
		o, err := url.Parse(origin)
		if err != nil || (o.Host != r.Host && o.Host != r.Header.Get("X-Forwarded-Host")) {
			return "a write must come from the dashboard's own page"
		}
	}
	return ""
}

// writeStatus answers like the API server does, so the app handles the dashboard's own
// refusals and the API server's with the same code.
func writeStatus(w http.ResponseWriter, code int, reason metav1.StatusReason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(&metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   metav1.StatusFailure,
		Message:  message,
		Reason:   reason,
		Code:     int32(code),
	})
}
