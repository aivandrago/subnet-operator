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

// Command dashboard serves the subnet dashboard inside the cluster and reads the inventory
// from the API server with the viewer's own token. It ships in the operator's image, as a
// second entrypoint, so the image's signature and SBOM cover it as well.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"hypersurgery.dev/subnet-operator/internal/dashboard"
	"hypersurgery.dev/subnet-operator/site"
)

// defaultCAFile is where the chart mounts the kube-root-ca.crt ConfigMap. A pod normally
// finds the CA next to its service account token, in a volume this one does not have: it runs
// without a token on purpose.
const defaultCAFile = "/var/run/kube-root-ca/ca.crt"

func main() {
	if err := run(); err != nil {
		slog.Error("Dashboard stopped", "err", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		listen       string
		healthListen string
		apiServer    string
		caFile       string
		tlsCertFile  string
		tlsKeyFile   string
		maxBodyBytes int64
	)
	flag.StringVar(&listen, "listen", ":8080", "Address to serve the dashboard on.")
	flag.StringVar(&healthListen, "health-listen", ":8081",
		"Address of the plain-HTTP /healthz endpoint for the kubelet probes; empty to serve it on --listen only.")
	flag.StringVar(&apiServer, "api-server", inClusterAPIServer(),
		"Kubernetes API server URL. Defaults to the in-cluster address.")
	flag.StringVar(&caFile, "ca-file", defaultCAFile, "CA bundle the API server's certificate is checked against.")
	flag.StringVar(&tlsCertFile, "tls-cert-file", "", "Serve HTTPS with this certificate instead of HTTP.")
	flag.StringVar(&tlsKeyFile, "tls-key-file", "", "Key of --tls-cert-file.")
	flag.Int64Var(&maxBodyBytes, "max-body-bytes", dashboard.DefaultMaxBodyBytes,
		"Largest request body forwarded to the API server.")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(log)

	if apiServer == "" {
		return errors.New("--api-server is required outside a cluster")
	}
	upstream, err := url.Parse(apiServer)
	if err != nil || upstream.Scheme != "https" || upstream.Host == "" {
		// The viewer's token travels on this connection; it never goes out in the clear.
		return fmt.Errorf("--api-server must be an https URL, got %q", apiServer)
	}
	if (tlsCertFile == "") != (tlsKeyFile == "") {
		return errors.New("--tls-cert-file and --tls-key-file go together")
	}
	transport, err := apiTransport(caFile)
	if err != nil {
		return err
	}

	proxy := dashboard.NewProxy(dashboard.ProxyOptions{
		Upstream:     upstream,
		Transport:    transport,
		MaxBodyBytes: maxBodyBytes,
		Logf:         func(format string, args ...any) { log.Info(fmt.Sprintf(format, args...)) },
	})
	handler, err := dashboard.Handler(site.Dashboard, proxy)
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
		// No WriteTimeout: a watch is a response that stays open on purpose.
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	errc := make(chan error, 2)
	// The probes get a port of their own, so a NetworkPolicy can open it to the kubelet while
	// the dashboard itself is only reachable from the ingress controller.
	var health *http.Server
	if healthListen != "" {
		health = &http.Server{Addr: healthListen, Handler: dashboard.HealthHandler(), ReadHeaderTimeout: 5 * time.Second}
		go func() { errc <- health.ListenAndServe() }()
	}
	go func() {
		log.Info("Serving the dashboard", "address", listen, "apiServer", upstream.String(), "tls", tlsCertFile != "")
		if tlsCertFile != "" {
			errc <- srv.ListenAndServeTLS(tlsCertFile, tlsKeyFile)
		} else {
			errc <- srv.ListenAndServe()
		}
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if health != nil {
		_ = health.Shutdown(shutdown)
	}
	return srv.Shutdown(shutdown)
}

// inClusterAPIServer is the address every pod is given for the API server.
func inClusterAPIServer() string {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return ""
	}
	return "https://" + net.JoinHostPort(host, port)
}

// apiTransport trusts the cluster CA and nothing else, and carries no credentials of its own:
// every request it sends is authenticated by the header the viewer's browser sent.
func apiTransport(caFile string) (*http.Transport, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("reading the API server CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificate in %s", caFile)
	}
	return &http.Transport{
		// No HTTP(S)_PROXY from the environment: the tokens go to the API server directly.
		Proxy:                 nil,
		TLSClientConfig:       &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	}, nil
}
