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

package dashboard

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// token looks like a service account JWT, with the characters a careless proxy might mangle.
const token = "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ2aWV3ZXIifQ.c2ln-_+/="

// fakeAPIServer records what reached it and answers 200, like an API server that allows
// everything. Whatever the dashboard refuses must therefore be refused by the dashboard.
type fakeAPIServer struct {
	*httptest.Server
	mu   sync.Mutex
	seen []*http.Request
	body []string
}

func newFakeAPIServer(t *testing.T) *fakeAPIServer {
	t.Helper()
	f := &fakeAPIServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.seen = append(f.seen, r.Clone(r.Context()))
		f.body = append(f.body, string(b))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=from-upstream")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
		}
		_, _ = w.Write([]byte(`{"kind":"SubnetList","items":[]}`))
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeAPIServer) requests() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*http.Request(nil), f.seen...)
}

func newTestProxy(t *testing.T, api *fakeAPIServer) *Proxy {
	t.Helper()
	u, err := url.Parse(api.URL)
	if err != nil {
		t.Fatal(err)
	}
	return NewProxy(ProxyOptions{Upstream: u, Transport: api.Client().Transport, MaxBodyBytes: 1024})
}

func do(p http.Handler, r *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, r)
	return rec
}

func signed(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

// write is a request the way the app sends one.
func write(target, body string) *http.Request {
	r := signed(http.MethodPost, target, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(CSRFHeader, "1")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	r.Header.Set("Origin", "http://"+r.Host)
	return r
}

func statusOf(t *testing.T, rec *httptest.ResponseRecorder) metav1.Status {
	t.Helper()
	var s metav1.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatalf("response is not a Status: %v: %s", err, rec.Body.String())
	}
	return s
}

func TestReadsOfTheGroupAreForwardedWithTheViewersTokenVerbatim(t *testing.T) {
	for _, target := range []string{
		"/apis/aws.hypersurgery/v1alpha1/subnets",
		"/apis/aws.hypersurgery/v1alpha1/subnets?labelSelector=aws.hypersurgery%2Fscope%3Dorg&limit=500",
		"/apis/aws.hypersurgery/v1alpha1/subnets?watch=true&resourceVersion=42",
		"/apis/aws.hypersurgery/v1alpha1/vpcs/vpc-0abc",
		"/apis/aws.hypersurgery/v1alpha1/networkscopes/org/status",
		"/apis/aws.hypersurgery/v1alpha1/subnetclaims",
		"/apis/aws.hypersurgery/v1alpha1/namespaces/payments/subnetclaims",
		"/apis/aws.hypersurgery/v1alpha1/namespaces/payments/resourceimports/checkout",
	} {
		t.Run(target, func(t *testing.T) {
			api := newFakeAPIServer(t)
			rec := do(newTestProxy(t, api), signed(http.MethodGet, target, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
			}
			seen := api.requests()
			if len(seen) != 1 {
				t.Fatalf("the API server saw %d requests, want 1", len(seen))
			}
			want, _ := url.Parse(target)
			if seen[0].URL.Path != want.Path || seen[0].URL.RawQuery != want.RawQuery {
				t.Errorf("forwarded as %s, want %s", seen[0].URL.String(), target)
			}
			if got := seen[0].Header.Values("Authorization"); len(got) != 1 || got[0] != "Bearer "+token {
				t.Errorf("Authorization reached the API server as %q, want the viewer's own", got)
			}
		})
	}
}

func TestCreatingAResourceImportIsForwarded(t *testing.T) {
	api := newFakeAPIServer(t)
	body := `{"apiVersion":"aws.hypersurgery/v1alpha1","kind":"ResourceImport"}`
	rec := do(newTestProxy(t, api), write("/apis/aws.hypersurgery/v1alpha1/namespaces/default/resourceimports", body))
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201: %s", rec.Code, rec.Body.String())
	}
	seen := api.requests()
	if len(seen) != 1 || seen[0].Method != http.MethodPost {
		t.Fatalf("the API server saw %v, want one POST", seen)
	}
	if api.body[0] != body {
		t.Errorf("body reached the API server as %q, want %q", api.body[0], body)
	}
	if got := seen[0].Header.Get("Authorization"); got != "Bearer "+token {
		t.Errorf("Authorization reached the API server as %q", got)
	}
	if got := seen[0].Header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type reached the API server as %q", got)
	}
}

func TestEverythingOffTheListIsRefusedBeforeTheAPIServer(t *testing.T) {
	cases := []struct{ method, target string }{
		{http.MethodDelete, "/apis/aws.hypersurgery/v1alpha1/subnets/subnet-0abc"},
		{http.MethodDelete, "/apis/aws.hypersurgery/v1alpha1/namespaces/default/resourceimports/x"},
		{http.MethodPut, "/apis/aws.hypersurgery/v1alpha1/networkscopes/org"},
		{http.MethodPatch, "/apis/aws.hypersurgery/v1alpha1/networkscopes/org"},
		{http.MethodPost, "/apis/aws.hypersurgery/v1alpha1/networkscopes"},
		{http.MethodPost, "/apis/aws.hypersurgery/v1alpha1/namespaces/default/subnetclaims"},
		{http.MethodPost, "/apis/aws.hypersurgery/v1alpha1/resourceimports"},
		{http.MethodPost, "/apis/aws.hypersurgery/v1alpha1/namespaces/default/resourceimports/x"},
		{http.MethodPost, "/apis/aws.hypersurgery/v1alpha1/namespaces/default/resourceimports/x/status"},
		{http.MethodGet, "/api/v1/secrets"},
		{http.MethodGet, "/api/v1/namespaces/kube-system/secrets/admin"},
		{http.MethodGet, "/apis/apps/v1/deployments"},
		{http.MethodGet, "/apis/aws.hypersurgery.evil/v1alpha1/subnets"},
		{http.MethodGet, "/apis/aws.hypersurgery"},
		{http.MethodGet, "/apis/aws.hypersurgery/v1alpha1"},
		{http.MethodGet, "/apis/aws.hypersurgery/v1alpha1/namespaces/default"},
		{http.MethodGet, "/apis/aws.hypersurgery/v1alpha1/subnets/subnet-0abc/proxy"},
		{http.MethodGet, "/apis/aws.hypersurgery/v1alpha1/subnets/../../../api/v1/secrets"},
		{http.MethodGet, "/apis/aws.hypersurgery/v1alpha1/subnets%2F..%2F..%2F..%2Fapi%2Fv1%2Fsecrets"},
		{http.MethodGet, "/apis/aws.hypersurgery/v1alpha1//subnets"},
		{http.MethodGet, "/apis/aws.hypersurgery/latest/subnets"},
		{http.MethodOptions, "/apis/aws.hypersurgery/v1alpha1/subnets"},
		{http.MethodHead, "/apis/aws.hypersurgery/v1alpha1/subnets"},
	}
	for _, c := range cases {
		t.Run(c.method+" "+c.target, func(t *testing.T) {
			api := newFakeAPIServer(t)
			var r *http.Request
			if c.method == http.MethodPost {
				r = write(c.target, `{}`)
			} else {
				r = signed(c.method, c.target, nil)
			}
			rec := do(newTestProxy(t, api), r)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("got %d, want 403: %s", rec.Code, rec.Body.String())
			}
			if s := statusOf(t, rec); s.Reason != metav1.StatusReasonForbidden {
				t.Errorf("reason %q, want Forbidden", s.Reason)
			}
			if n := len(api.requests()); n != 0 {
				t.Errorf("the API server saw %d requests, want none", n)
			}
		})
	}
}

func TestWritesMustComeFromTheDashboardsOwnPage(t *testing.T) {
	const target = "/apis/aws.hypersurgery/v1alpha1/namespaces/default/resourceimports"
	cases := map[string]func(r *http.Request){
		"without the custom header": func(r *http.Request) { r.Header.Del(CSRFHeader) },
		"as a form post":            func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") },
		"as text/plain":             func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		"from another site":         func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"from another origin":       func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example") },
	}
	for name, spoil := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPIServer(t)
			r := write(target, `{}`)
			spoil(r)
			rec := do(newTestProxy(t, api), r)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("got %d, want 403: %s", rec.Code, rec.Body.String())
			}
			if n := len(api.requests()); n != 0 {
				t.Errorf("the API server saw %d requests, want none", n)
			}
		})
	}
}

func TestAWriteThroughAProxyThatRewritesHostIsForwarded(t *testing.T) {
	api := newFakeAPIServer(t)
	r := write("/apis/aws.hypersurgery/v1alpha1/namespaces/default/resourceimports", `{}`)
	r.Host = "subnet-operator-dashboard.subnets.svc"
	r.Header.Set("Origin", "https://subnets.example.com")
	r.Header.Set("X-Forwarded-Host", "subnets.example.com")
	if rec := do(newTestProxy(t, api), r); rec.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201: %s", rec.Code, rec.Body.String())
	}
}

func TestWithoutABearerTokenNothingIsForwarded(t *testing.T) {
	cases := map[string]func(r *http.Request){
		"no Authorization header": func(r *http.Request) { r.Header.Del("Authorization") },
		"basic credentials":       func(r *http.Request) { r.Header.Set("Authorization", "Basic dXNlcjpwYXNz") },
		"an empty bearer token":   func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") },
		"two Authorization headers": func(r *http.Request) {
			r.Header.Add("Authorization", "Bearer other")
		},
		"only a cookie": func(r *http.Request) {
			r.Header.Del("Authorization")
			r.Header.Set("Cookie", "_oauth2_proxy=session")
		},
	}
	for name, spoil := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPIServer(t)
			r := signed(http.MethodGet, "/apis/aws.hypersurgery/v1alpha1/subnets", nil)
			spoil(r)
			rec := do(newTestProxy(t, api), r)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("got %d, want 401: %s", rec.Code, rec.Body.String())
			}
			if s := statusOf(t, rec); s.Reason != metav1.StatusReasonUnauthorized {
				t.Errorf("reason %q, want Unauthorized", s.Reason)
			}
			if rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("a 401 without WWW-Authenticate")
			}
			if n := len(api.requests()); n != 0 {
				t.Errorf("the API server saw %d requests, want none", n)
			}
		})
	}
}

func TestImpersonationAndOtherHeadersStayBehind(t *testing.T) {
	api := newFakeAPIServer(t)
	r := signed(http.MethodGet, "/apis/aws.hypersurgery/v1alpha1/subnets", nil)
	r.Header.Set("Accept", "application/json")
	dropped := map[string]string{
		"Impersonate-User":         "system:admin",
		"Impersonate-Group":        "system:masters",
		"Impersonate-Uid":          "0",
		"Impersonate-Extra-Scopes": "all",
		"X-Remote-User":            "system:admin",
		"X-Remote-Group":           "system:masters",
		"Cookie":                   "_oauth2_proxy=session",
		"X-Forwarded-For":          "10.0.0.1",
		"X-Forwarded-User":         "someone-else",
		"Proxy-Authorization":      "Basic Zm9vOmJhcg==",
		"Te":                       "trailers",
		"X-Anything":               "else",
	}
	for k, v := range dropped {
		r.Header.Set(k, v)
	}
	r.Header.Set("Connection", "keep-alive, X-Anything")

	if rec := do(newTestProxy(t, api), r); rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	seen := api.requests()
	if len(seen) != 1 {
		t.Fatalf("the API server saw %d requests, want 1", len(seen))
	}
	for k := range dropped {
		if v := seen[0].Header.Values(k); len(v) > 0 {
			t.Errorf("%s reached the API server: %q", k, v)
		}
	}
	for k := range seen[0].Header {
		if strings.HasPrefix(strings.ToLower(k), "impersonate-") {
			t.Errorf("%s reached the API server", k)
		}
	}
	if got := seen[0].Header.Get("Authorization"); got != "Bearer "+token {
		t.Errorf("Authorization reached the API server as %q", got)
	}
	if got := seen[0].Header.Get("Accept"); got != "application/json" {
		t.Errorf("Accept reached the API server as %q", got)
	}
}

func TestAnOversizedBodyIsRefused(t *testing.T) {
	api := newFakeAPIServer(t)
	rec := do(newTestProxy(t, api), write("/apis/aws.hypersurgery/v1alpha1/namespaces/default/resourceimports",
		`{"pad":"`+strings.Repeat("x", 2048)+`"}`))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413: %s", rec.Code, rec.Body.String())
	}
	if n := len(api.requests()); n != 0 {
		t.Errorf("the API server saw %d requests, want none", n)
	}
}

func TestAPIResponsesAreNotCachedAndSetNoCookies(t *testing.T) {
	api := newFakeAPIServer(t)
	rec := do(newTestProxy(t, api), signed(http.MethodGet, "/apis/aws.hypersurgery/v1alpha1/subnets", nil))
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q, want no-store", got)
	}
	if got := rec.Header().Values("Set-Cookie"); len(got) > 0 {
		t.Errorf("Set-Cookie passed through: %q", got)
	}
}

// selfSubjectReview is the body the app sends to learn who it is signed in as.
const selfSubjectReview = `{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview"}`

func TestAWatchIsForwardedWithItsQueryAndNotBuffered(t *testing.T) {
	for _, target := range []string{
		"/apis/aws.hypersurgery/v1alpha1/subnets?watch=1&resourceVersion=42&allowWatchBookmarks=true&timeoutSeconds=300",
		"/apis/aws.hypersurgery/v1alpha1/namespaces/payments/resourceimports?watch=true&resourceVersion=7&allowWatchBookmarks=true",
	} {
		t.Run(target, func(t *testing.T) {
			api := newFakeAPIServer(t)
			rec := do(newTestProxy(t, api), signed(http.MethodGet, target, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
			}
			seen := api.requests()
			if len(seen) != 1 {
				t.Fatalf("the API server saw %d requests, want 1", len(seen))
			}
			want, _ := url.Parse(target)
			if seen[0].URL.Path != want.Path || seen[0].URL.RawQuery != want.RawQuery {
				t.Errorf("forwarded as %s, want %s", seen[0].URL.String(), target)
			}
			if got := rec.Header().Get("X-Accel-Buffering"); got != "no" {
				t.Errorf("X-Accel-Buffering %q, want no: a proxy in front would hold the events back", got)
			}
		})
	}

	api := newFakeAPIServer(t)
	rec := do(newTestProxy(t, api), signed(http.MethodGet, "/apis/aws.hypersurgery/v1alpha1/subnets", nil))
	if got := rec.Header().Get("X-Accel-Buffering"); got != "" {
		t.Errorf("a plain list carries X-Accel-Buffering %q", got)
	}
}

// A watch event has to reach the browser while the stream is still open, not when it ends.
func TestAWatchStreamsEventsAsTheyHappen(t *testing.T) {
	release := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"type":"ADDED","object":{"kind":"Subnet"}}` + "\n"))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer api.Close()
	defer close(release)
	u, _ := url.Parse(api.URL)
	front := httptest.NewServer(NewProxy(ProxyOptions{Upstream: u, Transport: api.Client().Transport}))
	defer front.Close()

	req, _ := http.NewRequest(http.MethodGet, front.URL+"/apis/aws.hypersurgery/v1alpha1/subnets?watch=1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res, err := front.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	done := make(chan string, 1)
	go func() {
		buf := make([]byte, 256)
		n, _ := res.Body.Read(buf)
		done <- string(buf[:n])
	}()
	select {
	case got := <-done:
		if !strings.Contains(got, `"ADDED"`) {
			t.Errorf("first read %q, want the ADDED event", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the event did not arrive while the watch was open")
	}
}

func TestASelfSubjectReviewIsForwarded(t *testing.T) {
	api := newFakeAPIServer(t)
	rec := do(newTestProxy(t, api), write(SelfSubjectReviewPath, selfSubjectReview))
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201: %s", rec.Code, rec.Body.String())
	}
	seen := api.requests()
	if len(seen) != 1 || seen[0].Method != http.MethodPost || seen[0].URL.Path != SelfSubjectReviewPath {
		t.Fatalf("the API server saw %v, want one POST to %s", seen, SelfSubjectReviewPath)
	}
	if api.body[0] != selfSubjectReview {
		t.Errorf("body reached the API server as %q", api.body[0])
	}
	if got := seen[0].Header.Get("Authorization"); got != "Bearer "+token {
		t.Errorf("Authorization reached the API server as %q", got)
	}
}

func TestNothingElseInAuthenticationIsForwarded(t *testing.T) {
	cases := []struct{ name, method, target, body string }{
		{"a GET of the review", http.MethodGet, SelfSubjectReviewPath, ""},
		{"a list of reviews with watch", http.MethodGet, SelfSubjectReviewPath + "?watch=1", ""},
		{"a delete", http.MethodDelete, SelfSubjectReviewPath, ""},
		{"a beta review", http.MethodPost, "/apis/authentication.k8s.io/v1beta1/selfsubjectreviews", selfSubjectReview},
		{"a named review", http.MethodPost, SelfSubjectReviewPath + "/me", selfSubjectReview},
		{"a review with parameters", http.MethodPost, SelfSubjectReviewPath + "?dryRun=All", selfSubjectReview},
		{"a TokenReview", http.MethodPost, "/apis/authentication.k8s.io/v1/tokenreviews",
			`{"apiVersion":"authentication.k8s.io/v1","kind":"TokenReview","spec":{"token":"x"}}`},
		{"discovery", http.MethodGet, "/apis/authentication.k8s.io/v1", ""},
		{"an access review", http.MethodPost, "/apis/authorization.k8s.io/v1/selfsubjectaccessreviews",
			`{"apiVersion":"authorization.k8s.io/v1","kind":"SelfSubjectAccessReview"}`},
		{"a TokenReview body", http.MethodPost, SelfSubjectReviewPath,
			`{"apiVersion":"authentication.k8s.io/v1","kind":"TokenReview","spec":{"token":"x"}}`},
		{"another version in the body", http.MethodPost, SelfSubjectReviewPath,
			`{"apiVersion":"authentication.k8s.io/v1beta1","kind":"SelfSubjectReview"}`},
		{"an extra field", http.MethodPost, SelfSubjectReviewPath,
			`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview","spec":{}}`},
		{"a key in another case", http.MethodPost, SelfSubjectReviewPath,
			`{"apiVersion":"authentication.k8s.io/v1","kind":"SelfSubjectReview","Kind":"TokenReview"}`},
		{"two objects", http.MethodPost, SelfSubjectReviewPath, selfSubjectReview + selfSubjectReview},
		{"trailing bytes", http.MethodPost, SelfSubjectReviewPath, selfSubjectReview + " x"},
		{"an array", http.MethodPost, SelfSubjectReviewPath, `[` + selfSubjectReview + `]`},
		{"an empty body", http.MethodPost, SelfSubjectReviewPath, ``},
		{"a kind that is not a string", http.MethodPost, SelfSubjectReviewPath,
			`{"apiVersion":"authentication.k8s.io/v1","kind":{"x":1}}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			api := newFakeAPIServer(t)
			var r *http.Request
			if c.method == http.MethodPost {
				r = write(c.target, c.body)
			} else {
				r = signed(c.method, c.target, nil)
			}
			rec := do(newTestProxy(t, api), r)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("got %d, want 403: %s", rec.Code, rec.Body.String())
			}
			if n := len(api.requests()); n != 0 {
				t.Errorf("the API server saw %d requests, want none", n)
			}
		})
	}
}

// The review changes nothing, but it is a POST, and every POST is held to the same checks.
func TestASelfSubjectReviewMustComeFromTheDashboardsOwnPage(t *testing.T) {
	cases := map[string]func(r *http.Request){
		"without the custom header": func(r *http.Request) { r.Header.Del(CSRFHeader) },
		"as text/plain":             func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		"from another site":         func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
		"from another origin":       func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example") },
	}
	for name, spoil := range cases {
		t.Run(name, func(t *testing.T) {
			api := newFakeAPIServer(t)
			r := write(SelfSubjectReviewPath, selfSubjectReview)
			spoil(r)
			if rec := do(newTestProxy(t, api), r); rec.Code != http.StatusForbidden {
				t.Fatalf("got %d, want 403: %s", rec.Code, rec.Body.String())
			}
			if n := len(api.requests()); n != 0 {
				t.Errorf("the API server saw %d requests, want none", n)
			}
		})
	}
}
