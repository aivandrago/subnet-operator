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
	"bytes"
	"fmt"
	"io/fs"
	"net/http"
	"regexp"
	"time"
)

// ContentSecurityPolicy lets the app load its own script, style and images and talk to its
// own origin, and nothing else. The names and tags the app renders come from AWS, where
// anybody who can create a subnet can write them; if one of them ever slipped past the app's
// escaping, there is no inline script it could run and no other origin it could send the
// viewer's token to.
const ContentSecurityPolicy = "default-src 'none'; script-src 'self'; style-src 'self'; " +
	"img-src 'self' data:; connect-src 'self'; manifest-src 'self'; font-src 'self'; " +
	"base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// The app decides between demo data and a live cluster from this meta tag. The public site
// ships it as "demo"; the dashboard serves the same page with "cluster".
var (
	sourceMeta = regexp.MustCompile(`<meta name="subnet-dashboard-source" content="[a-z]*">`)
	// Web fonts come from Google on the public site. In the cluster the policy above blocks
	// them anyway, and an internal tool has no business telling a third party who opened it.
	externalFont = regexp.MustCompile(`(?m)^[ \t]*<link[^>]*fonts\.(googleapis|gstatic)\.com[^>]*>\r?\n`)
)

// Handler serves the app under /dashboard/ and /assets/, and the API calls under /apis/.
// The API paths are the API server's own, so the app builds the same URLs whether it talks to
// kubectl proxy or to this server.
func Handler(files fs.FS, proxy http.Handler) (http.Handler, error) {
	index, err := fs.ReadFile(files, "dashboard/index.html")
	if err != nil {
		return nil, fmt.Errorf("reading the app: %w", err)
	}
	if !sourceMeta.Match(index) {
		return nil, fmt.Errorf("the app's index.html has no subnet-dashboard-source meta tag")
	}
	index = sourceMeta.ReplaceAll(index, []byte(`<meta name="subnet-dashboard-source" content="cluster">`))
	index = externalFont.ReplaceAll(index, nil)

	static := http.FileServerFS(files)
	mux := http.NewServeMux()
	mux.Handle("GET /healthz", HealthHandler())
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard/", http.StatusFound)
	})
	serveIndex := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(index))
	}
	mux.HandleFunc("GET /dashboard/{$}", serveIndex)
	mux.HandleFunc("GET /dashboard/index.html", serveIndex)
	mux.Handle("GET /dashboard/", static)
	mux.Handle("GET /assets/", static)
	// Everything that looks like an API call goes to the proxy, which refuses what is not on
	// its list. /api/ is the core group: it is routed here only to be refused with a Status the
	// app understands, rather than a 404 page.
	mux.Handle("/apis/", proxy)
	mux.Handle("/api/", proxy)
	return securityHeaders(mux), nil
}

// HealthHandler answers the kubelet's probes. The dashboard keeps no state and needs nothing
// to serve its page, so being able to answer is all there is to check.
func HealthHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok"))
	})
	return mux
}

// securityHeaders applies to every response, the app's and the proxied API's alike. No
// response carries CORS headers and no preflight is ever answered, so no other origin can
// read from the dashboard or send it the custom header a write requires.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", ContentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), clipboard-read=()")
		next.ServeHTTP(w, r)
	})
}
