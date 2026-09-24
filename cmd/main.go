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

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"os"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
	"hypersurgery.dev/subnet-operator/internal/audit"
	awscloud "hypersurgery.dev/subnet-operator/internal/cloud/aws"
	"hypersurgery.dev/subnet-operator/internal/controller"
	"hypersurgery.dev/subnet-operator/internal/events"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/metrics"
	webhookv1alpha1 "hypersurgery.dev/subnet-operator/internal/webhook/v1alpha1"
	// +kubebuilder:scaffold:imports
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(awsv1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var webhookPort int
	var discoveryConcurrency int
	var eventsQueueURL string
	var eventsDebounce time.Duration
	var auditSink string
	var enableWrites bool
	var enableWebhooks bool
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	// The defaults are client-go's. They are flags because they trade two things against each
	// other: a short lease means a crashed leader is replaced quickly, and it also means more
	// writes to the API server and a leader that loses its lease on a slow API call.
	var leaseDuration, renewDeadline, retryPeriod time.Duration
	flag.DurationVar(&leaseDuration, "leader-elect-lease-duration", 15*time.Second,
		"How long a standby waits before taking over from a leader that has stopped renewing, "+
			"for example because it crashed. A leader that stops cleanly hands the lease over at once.")
	flag.DurationVar(&renewDeadline, "leader-elect-renew-deadline", 10*time.Second,
		"How long the leader keeps retrying a renewal before it gives up leadership.")
	flag.DurationVar(&retryPeriod, "leader-elect-retry-period", 2*time.Second,
		"How often candidates and the leader act on the lease.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "Port the webhook server listens on. "+
		"Defaults to 9443. Set -1 to disable the webhook server.")
	flag.IntVar(&discoveryConcurrency, "discovery-concurrency", 4,
		"Number of AWS account/region pairs discovered in parallel, across all NetworkScopes.")
	flag.StringVar(&eventsQueueURL, "events-queue-url", os.Getenv("EVENTS_QUEUE_URL"),
		"SQS queue fed by EventBridge with EC2 change events. When set, changed accounts/regions are "+
			"resynced within seconds instead of waiting for the resync interval. Defaults to $EVENTS_QUEUE_URL.")
	flag.DurationVar(&eventsDebounce, "events-debounce", 10*time.Second,
		"How long EC2 change events are collected before the affected accounts/regions are resynced.")
	flag.StringVar(&auditSink, "audit-sink", audit.ModeStdout,
		"Where the audit trail goes: 'stdout' writes one JSON line per import, allocation and policy "+
			"decision to stdout, apart from the operational log on stderr; 'off' writes none. "+
			"The fields are documented in docs/audit.md.")
	flag.BoolVar(&enableWrites, "enable-writes", false,
		"Allow SubnetClaims in Create mode to create subnets in AWS. Off by default: the operator is read-only.")
	flag.BoolVar(&enableWebhooks, "enable-webhooks", true,
		"Serve the admission webhooks. They need a serving certificate and stay off without one.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
		Port:    webhookPort,
	}

	if len(webhookCertPath) > 0 && enableWebhooks {
		if !webhookv1alpha1.CertificateAvailable(webhookCertPath, webhookCertName) {
			setupLog.Error(errors.New("no certificate at the configured path"),
				"The webhook certificate is missing; start with --enable-webhooks=false to run without it",
				"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName)
			os.Exit(1)
		}
	}
	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.25.0/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// If the certificate is not specified, controller-runtime will automatically
	// generate self-signed certificates for the metrics server. While convenient for development and testing,
	// this setup is not recommended for production.
	//
	// TODO(user): If you enable certManager, uncomment the following lines:
	// - [METRICS-WITH-CERTS] at config/default/kustomization.yaml to generate and use certificates
	// managed by cert-manager for the metrics server.
	// - [PROMETHEUS-WITH-CERTS] at config/prometheus/kustomization.yaml for TLS certification.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	if err := validLeaseTimings(leaseDuration, renewDeadline, retryPeriod); err != nil {
		setupLog.Error(err, "Invalid leader election timings")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "1095b947.hypersurgery",
		LeaseDuration:          &leaseDuration,
		RenewDeadline:          &renewDeadline,
		RetryPeriod:            &retryPeriod,
		// A leader that is stopped — a drain, a rollout, a scale-down — gives the lease up on
		// the way out, so the standby takes over within a retry period instead of waiting out
		// the whole lease. That is only safe if nothing keeps working after the manager stops,
		// and nothing does: every piece of work, the SQS poller included, is a runnable of the
		// manager, and main returns as soon as Start does. The manager also waits for in-flight
		// reconciles before it releases, so two leaders never reconcile at once.
		LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	// An unusable audit sink stops the operator rather than letting it run believing it keeps
	// a record: the whole point of the trail is that it is there when somebody asks.
	auditor, err := audit.New(auditSink)
	if err != nil {
		setupLog.Error(err, "Failed to configure the audit sink")
		os.Exit(1)
	}
	if auditSink != audit.ModeOff {
		setupLog.Info("Writing the audit trail", "sink", auditSink)
	}

	discoverer, err := awscloud.NewDiscoverer(context.Background())
	if err != nil {
		setupLog.Error(err, "Failed to load AWS configuration")
		os.Exit(1)
	}
	discoverer.OnThrottle = func(t inventory.Target, operation string) {
		metrics.APIThrottled(t.Scope, t.Account, t.Region, operation)
	}
	// Shared between the event poller, which fills it, and the scope controller, whose
	// auto-import policy asks it who created a resource nobody has tagged.
	creators := &controller.CreatorCache{}
	scopeReconciler := &controller.NetworkScopeReconciler{
		Client:      mgr.GetClient(),
		Scheme:      mgr.GetScheme(),
		Discoverer:  discoverer,
		APIReader:   mgr.GetAPIReader(),
		Concurrency: discoveryConcurrency,
		Creators:    creators,
		Recorder:    mgr.GetEventRecorder("aws.hypersurgery/networkscope"),
		Audit:       auditor,
	}
	if err := scopeReconciler.SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "networkscope")
		os.Exit(1)
	}
	if eventsQueueURL != "" {
		sqsClient, err := awscloud.NewSQSClient(context.Background(), eventsQueueURL)
		if err != nil {
			setupLog.Error(err, "Failed to create the SQS client")
			os.Exit(1)
		}
		if err := mgr.Add(&events.Poller{
			SQS:      sqsClient,
			QueueURL: eventsQueueURL,
			Sink:     scopeReconciler.NotifyChanged,
			OnCreate: creators.Record,
			Debounce: eventsDebounce,
			Log:      ctrl.Log.WithName("events"),
		}); err != nil {
			setupLog.Error(err, "Failed to add the events poller")
			os.Exit(1)
		}
		setupLog.Info("Consuming EC2 change events", "queue", eventsQueueURL)
	}
	if err := (&controller.SheetExportReconciler{
		Client:    mgr.GetClient(),
		Scheme:    mgr.GetScheme(),
		APIReader: mgr.GetAPIReader(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "sheetexport")
		os.Exit(1)
	}
	if err := (&controller.SubnetClaimReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		APIReader:     mgr.GetAPIReader(),
		Writer:        discoverer,
		WritesEnabled: enableWrites,
		Notify:        scopeReconciler.NotifyChanged,
		Recorder:      mgr.GetEventRecorder("aws.hypersurgery/subnetclaim"),
		Audit:         auditor,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "subnetclaim")
		os.Exit(1)
	}
	if enableWrites {
		setupLog.Info("Writes are enabled: SubnetClaims can create subnets and ResourceImports can tag resources")
	}
	if err := (&controller.ResourceImportReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		APIReader:     mgr.GetAPIReader(),
		Writer:        discoverer,
		WritesEnabled: enableWrites,
		Notify:        scopeReconciler.NotifyChanged,
		Recorder:      mgr.GetEventRecorder("aws.hypersurgery/resourceimport"),
		Audit:         auditor,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "resourceimport")
		os.Exit(1)
	}
	// The webhooks refuse objects the controllers would only be able to complain about in a
	// status later. The webhook server needs a serving certificate, so a manager that was
	// never given one — the plain kustomize install, or "make run" on a laptop — reports
	// that admission checks are off and carries on taking inventory. Being told where the
	// certificate is and not finding it there is a different matter, and fatal.
	if enableWebhooks && webhookCertPath == "" &&
		!webhookv1alpha1.CertificateAvailable("", webhookCertName) {
		setupLog.Info("No webhook certificate found, so the admission webhooks stay off; "+
			"the controllers still check everything they always did",
			"dir", webhookv1alpha1.DefaultCertDir, "cert", webhookCertName)
		enableWebhooks = false
	}
	if enableWebhooks {
		if err := webhookv1alpha1.SetupNetworkScopeWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "Failed to create webhook", "webhook", "NetworkScope")
			os.Exit(1)
		}
		if err := webhookv1alpha1.SetupSubnetClaimWebhookWithManager(mgr, enableWrites); err != nil {
			setupLog.Error(err, "Failed to create webhook", "webhook", "SubnetClaim")
			os.Exit(1)
		}
		if err := webhookv1alpha1.SetupResourceImportWebhookWithManager(mgr, enableWrites); err != nil {
			setupLog.Error(err, "Failed to create webhook", "webhook", "ResourceImport")
			os.Exit(1)
		}
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}
