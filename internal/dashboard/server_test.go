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
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"hypersurgery.dev/subnet-operator/site"
)

func newTestHandler(t *testing.T, api *fakeAPIServer) http.Handler {
	t.Helper()
	h, err := Handler(site.Dashboard, newTestProxy(t, api))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func get(h http.Handler, target string) *httptest.ResponseRecorder {
	return do(h, httptest.NewRequest(http.MethodGet, target, nil))
}

// The public site must keep showing demo data: it is the same file the dashboard embeds.
func TestThePublicPageDefaultsToDemoData(t *testing.T) {
	index, err := fs.ReadFile(site.Dashboard, "dashboard/index.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), `<meta name="subnet-dashboard-source" content="demo">`) {
		t.Error("site/dashboard/index.html does not ship with the demo source")
	}
}

func TestTheServedPageReadsTheClusterAndLoadsNothingFromElsewhere(t *testing.T) {
	h := newTestHandler(t, newFakeAPIServer(t))
	for _, target := range []string{"/dashboard/", "/dashboard/index.html"} {
		rec := get(h, target)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d", target, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, `<meta name="subnet-dashboard-source" content="cluster">`) {
			t.Errorf("%s: the page is not switched to the cluster source", target)
		}
		if strings.Contains(body, "fonts.googleapis.com") || strings.Contains(body, "fonts.gstatic.com") {
			t.Errorf("%s: the page still loads fonts from Google", target)
		}
		// With a policy that forbids inline script, an inline script would be dead code and
		// the app would not start.
		if strings.Contains(body, "<script>") || strings.Contains(body, "<style>") {
			t.Errorf("%s: inline script or style, which the CSP blocks", target)
		}
	}
}

func TestEveryResponseCarriesTheSecurityHeaders(t *testing.T) {
	h := newTestHandler(t, newFakeAPIServer(t))
	api := signed(http.MethodGet, "/apis/network.hypersurgery.dev/v1beta1/subnets", nil)
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"page":        get(h, "/dashboard/"),
		"script":      get(h, "/assets/dashboard.js"),
		"api":         do(h, api),
		"refused api": get(h, "/api/v1/secrets"),
	} {
		csp := rec.Header().Get("Content-Security-Policy")
		for _, directive := range []string{"default-src 'none'", "script-src 'self'", "connect-src 'self'", "frame-ancestors 'none'"} {
			if !strings.Contains(csp, directive) {
				t.Errorf("%s: CSP %q lacks %q", name, csp, directive)
			}
		}
		if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
			t.Errorf("%s: CSP %q allows inline code", name, csp)
		}
		for k, want := range map[string]string{
			"X-Content-Type-Options": "nosniff",
			"X-Frame-Options":        "DENY",
			"Referrer-Policy":        "no-referrer",
		} {
			if got := rec.Header().Values(k); len(got) != 1 || got[0] != want {
				t.Errorf("%s: %s is %q, want %q", name, k, got, want)
			}
		}
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Errorf("%s: a CORS header lets other origins read the response", name)
		}
	}
}

func TestTheAppAndItsAssetsAreServed(t *testing.T) {
	h := newTestHandler(t, newFakeAPIServer(t))
	for _, target := range []string{
		"/assets/dashboard.js", "/assets/dashboard.css", "/dashboard/app.js", "/dashboard/app.css",
		"/dashboard/sw.js", "/dashboard/manifest.webmanifest", "/dashboard/icon.svg", "/healthz",
	} {
		if rec := get(h, target); rec.Code != http.StatusOK {
			t.Errorf("%s: got %d", target, rec.Code)
		}
	}
	if rec := get(h, "/"); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/dashboard/" {
		t.Errorf("/: got %d to %q, want a redirect to /dashboard/", rec.Code, rec.Header().Get("Location"))
	}
	// Nothing else from the site is carried into the binary.
	if rec := get(h, "/index.html"); rec.Code != http.StatusNotFound {
		t.Errorf("/index.html: got %d, want 404", rec.Code)
	}
	if rec := do(h, httptest.NewRequest(http.MethodPost, "/dashboard/", nil)); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /dashboard/: got %d, want 405", rec.Code)
	}
}

func TestAPageWithoutTheSourceSwitchIsRefused(t *testing.T) {
	files := fstest.MapFS{"dashboard/index.html": {Data: []byte("<html></html>")}}
	if _, err := Handler(files, http.NotFoundHandler()); err == nil {
		t.Error("the dashboard started with a page it cannot switch to the cluster, which would show demo data")
	}
}
