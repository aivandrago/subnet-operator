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

// Package crdversions moves the operator's CRDs from network.hypersurgery.dev/v1beta1 to v1
// after an upgrade to 1.0, and keeps their conversion pointed at the operator.
//
// Applying the 1.0 CRDs makes v1 the storage version, but the objects already in etcd stay
// encoded as v1beta1 until something writes them, and status.storedVersions keeps listing
// v1beta1. A version listed there cannot be removed from the CRD, so the release that stops
// serving v1beta1 needs every object rewritten and the list trimmed first (see the Kubernetes
// documentation on CRD versioning). The operator does that itself, once, on start
// (MigrateStorage), instead of leaving it as a manual step in the upgrade guide.
//
// Once nothing is stored at v1beta1 any more, it points each CRD's conversion at its own
// webhook, with the CA from its serving certificate, and keeps it there
// (ConfigureConversion). Until then the CRDs keep the None strategy they are installed with:
// the two versions have the same fields, so the API server's own conversion is exact, and the
// operator's reads of objects still stored as v1beta1 do not depend on its webhook being
// reachable while it starts.
package crdversions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	kevents "k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
)

// StorageVersion is the version every object is rewritten at, and the only one left in
// status.storedVersions afterwards.
const StorageVersion = "v1"

// Kind is one of the operator's CRDs.
type Kind struct {
	// Kind is the object kind, e.g. SubnetClaim.
	Kind string
	// Plural is the resource name, e.g. subnetclaims.
	Plural string
}

// CRDName is the CRD's name, e.g. subnetclaims.network.hypersurgery.dev.
func (k Kind) CRDName() string {
	return k.Plural + "." + networkv1.GroupVersion.Group
}

// Kinds are all the operator's CRDs.
var Kinds = []Kind{
	{Kind: "NetworkScope", Plural: "networkscopes"},
	{Kind: "Network", Plural: "networks"},
	{Kind: "Subnet", Plural: "subnets"},
	{Kind: "SubnetClaim", Plural: "subnetclaims"},
	{Kind: "ResourceImport", Plural: "resourceimports"},
	{Kind: "SheetExport", Plural: "sheetexports"},
}

// CRDNames lists the names of the operator's CRDs, the only ones it may read or change.
func CRDNames() []string {
	names := make([]string, 0, len(Kinds))
	for _, k := range Kinds {
		names = append(names, k.CRDName())
	}
	return names
}

// The operator reads and changes its own CRDs, and nothing else of that API.
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;patch,resourceNames=networkscopes.network.hypersurgery.dev;networks.network.hypersurgery.dev;subnets.network.hypersurgery.dev;subnetclaims.network.hypersurgery.dev;resourceimports.network.hypersurgery.dev;sheetexports.network.hypersurgery.dev
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions/status,verbs=get;update,resourceNames=networkscopes.network.hypersurgery.dev;networks.network.hypersurgery.dev;subnets.network.hypersurgery.dev;subnetclaims.network.hypersurgery.dev;resourceimports.network.hypersurgery.dev;sheetexports.network.hypersurgery.dev

// labelKind is the metrics' label for the kind of object.
const labelKind = "kind"

var (
	storedVersions = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "hs_crd_stored_versions",
		Help: "1 for each version the CRD's status.storedVersions lists, by kind and version. After an upgrade to " +
			"1.0 it lists v1beta1 until the operator has rewritten every object at v1; a version still listed " +
			"cannot be removed from the CRD.",
	}, []string{labelKind, "version"})
	rewritten = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "hs_storage_migration_rewritten_objects_total",
		Help: "Objects the operator rewrote so that they are stored at the CRD's storage version, by kind.",
	}, []string{labelKind})
)

func init() {
	ctrlmetrics.Registry.MustRegister(storedVersions, rewritten)
}

// Event reasons.
const (
	// EventStorageMigrated is recorded on a CRD whose objects were rewritten at v1.
	EventStorageMigrated = "StorageVersionMigrated"
	// EventConversionConfigured is recorded on a CRD whose conversion the operator changed.
	EventConversionConfigured = "ConversionConfigured"
)

// Conversion modes.
const (
	// ConversionUnmanaged leaves each CRD's conversion as it was installed: the kustomize
	// install sets it with a patch and cert-manager's CA injector.
	ConversionUnmanaged = ""
	// ConversionNone sets the None strategy: the API server converts on its own, which is
	// exact as long as the versions have the same fields.
	ConversionNone = "none"
	// ConversionWebhook points the conversion at the operator's webhook.
	ConversionWebhook = "webhook"
)

// ConvertPath is where controller-runtime serves the conversion webhook.
const ConvertPath = "/convert"

// Upgrader is a leader-only runnable: it migrates the storage version once, and then, if asked
// to, keeps the CRDs' conversion configured.
type Upgrader struct {
	// Client reads the CRDs and writes objects and CRDs. Reads must not come from a cache: the
	// operator watches neither the CRDs nor, at every version, the objects.
	Client client.Client
	// Recorder records an Event on a CRD whose storage was migrated or whose conversion changed.
	// Nil records none.
	Recorder kevents.EventRecorder

	// Conversion is ConversionUnmanaged, ConversionNone or ConversionWebhook.
	Conversion string
	// Service is the Service in front of the webhook server, for ConversionWebhook.
	Service types.NamespacedName
	// Port is the Service port. Zero means 443.
	Port int32
	// CAFile holds the CA the API server trusts the webhook's certificate with, for
	// ConversionWebhook: ca.crt next to the serving certificate. It is read again on every
	// check, so a renewed CA reaches the CRDs.
	CAFile string

	// Interval is how often a failed migration is retried and the conversion is checked. Zero
	// means a minute.
	Interval time.Duration
}

// NeedLeaderElection makes only the leader rewrite objects and change CRDs.
func (u *Upgrader) NeedLeaderElection() bool {
	return true
}

// Start migrates the storage version, retrying until it succeeds, and then keeps the
// conversion configured until the context ends. It only returns an error for a configuration
// that can never work.
func (u *Upgrader) Start(ctx context.Context) error {
	log := logf.FromContext(ctx).WithName("crd-versions")
	switch u.Conversion {
	case ConversionUnmanaged, ConversionNone:
	case ConversionWebhook:
		if u.Service.Name == "" || u.Service.Namespace == "" {
			return errors.New("the conversion webhook needs the namespace and name of its Service")
		}
	default:
		return fmt.Errorf("unknown CRD conversion mode %q", u.Conversion)
	}
	interval := u.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	migrated := false
	for {
		if !migrated {
			results, err := u.MigrateStorage(ctx)
			for _, r := range results {
				if r.Rewritten > 0 || r.Trimmed {
					log.Info("Migrated the storage version", "crd", r.Kind.CRDName(), "rewritten", r.Rewritten,
						"storedVersions", r.StoredVersions, "was", r.Before)
				}
			}
			if err != nil {
				log.Error(err, "Could not migrate the stored objects to v1 yet; trying again", "in", interval)
			} else {
				migrated = true
			}
		}
		// Only once nothing is stored at v1beta1: until then the operator's own reads may need
		// a conversion, and the API server's is exact and needs no webhook to be up.
		if migrated {
			if err := u.ConfigureConversion(ctx); err != nil {
				log.Error(err, "Could not configure the CRDs' conversion; trying again", "in", interval)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Result is what MigrateStorage did with one CRD.
type Result struct {
	Kind Kind
	// Before is status.storedVersions as it was found.
	Before []string
	// StoredVersions is status.storedVersions afterwards.
	StoredVersions []string
	// Rewritten is the number of objects written again.
	Rewritten int
	// Trimmed is true when storedVersions was changed.
	Trimmed bool
}

// MigrateStorage rewrites every object of every CRD whose status.storedVersions lists anything
// but v1, and then trims the list to [v1]. It is idempotent: a CRD whose list is already [v1]
// is not touched, so every start after the first costs one read per CRD.
//
// Each object is written back unchanged through its status subresource. The API server stores
// it encoded at the storage version, and nothing about it changes but its resourceVersion (and
// its managedFields): the spec, and with it the generation, stays as it was, and admission
// webhooks, which are registered for the main resource, are not called.
func (u *Upgrader) MigrateStorage(ctx context.Context) ([]Result, error) {
	var results []Result
	var errs []error
	for _, k := range Kinds {
		r, err := u.migrateKind(ctx, k)
		if r != nil {
			results = append(results, *r)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", k.CRDName(), err))
		}
	}
	return results, errors.Join(errs...)
}

func (u *Upgrader) migrateKind(ctx context.Context, k Kind) (*Result, error) {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := u.Client.Get(ctx, client.ObjectKey{Name: k.CRDName()}, crd); err != nil {
		return nil, err
	}
	record(k, crd.Status.StoredVersions)
	r := &Result{Kind: k, Before: slices.Clone(crd.Status.StoredVersions), StoredVersions: crd.Status.StoredVersions}
	if slices.Equal(crd.Status.StoredVersions, []string{StorageVersion}) {
		return r, nil
	}
	if storage := storageVersion(crd); storage != StorageVersion {
		// The operator was upgraded, but not its CRDs: objects written now are still stored at
		// the old version, so rewriting them would change nothing.
		return r, fmt.Errorf("the CRD stores %s, not %s: apply the CRDs of this release first "+
			"(kubectl apply -f the chart's crds/ directory, as the upgrade guide says)", storage, StorageVersion)
	}

	n, err := u.rewriteAll(ctx, k)
	r.Rewritten = n
	rewritten.WithLabelValues(k.Kind).Add(float64(n))
	if err != nil {
		return r, err
	}

	// Every object that existed when the list was taken is stored at v1 now, and every object
	// written since was stored at v1 anyway, because v1 was already the storage version.
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if err := u.Client.Get(ctx, client.ObjectKey{Name: k.CRDName()}, crd); err != nil {
			return err
		}
		if storage := storageVersion(crd); storage != StorageVersion {
			return fmt.Errorf("the CRD's storage version changed to %s while its objects were rewritten", storage)
		}
		crd.Status.StoredVersions = []string{StorageVersion}
		return u.Client.Status().Update(ctx, crd)
	})
	if err != nil {
		return r, fmt.Errorf("trimming status.storedVersions: %w", err)
	}
	r.StoredVersions, r.Trimmed = crd.Status.StoredVersions, true
	record(k, r.StoredVersions)
	if u.Recorder != nil {
		u.Recorder.Eventf(crd, nil, corev1.EventTypeNormal, EventStorageMigrated, "MigrateStorage",
			"Rewrote %d object(s) at %s and trimmed status.storedVersions from %v to %v, so %v can be removed "+
				"from the CRD in a later release", n, StorageVersion, r.Before, r.StoredVersions,
			without(r.Before, StorageVersion))
	}
	return r, nil
}

// rewriteAll writes every object of a kind back unchanged, a page at a time, and returns how
// many it wrote.
func (u *Upgrader) rewriteAll(ctx context.Context, k Kind) (int, error) {
	gv := networkv1.GroupVersion
	n := 0
	cont := ""
	for {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(gv.WithKind(k.Kind + "List"))
		if err := u.Client.List(ctx, list, client.Limit(500), client.Continue(cont)); err != nil {
			return n, fmt.Errorf("listing: %w", err)
		}
		for i := range list.Items {
			obj := &list.Items[i]
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				err := u.Client.Status().Update(ctx, obj)
				if apierrors.IsConflict(err) {
					// Written by somebody else since the list: at v1, like anything written
					// now, but writing it again costs nothing and keeps the count honest.
					if getErr := u.Client.Get(ctx, client.ObjectKeyFromObject(obj), obj); getErr != nil {
						return getErr
					}
				}
				return err
			})
			switch {
			case apierrors.IsNotFound(err):
				// Deleted since the list: nothing left to rewrite.
			case err != nil:
				return n, fmt.Errorf("rewriting %s: %w", client.ObjectKeyFromObject(obj), err)
			default:
				n++
			}
		}
		cont = list.GetContinue()
		if cont == "" {
			return n, nil
		}
	}
}

// ConfigureConversion sets each CRD's conversion to what Conversion asks for, and leaves a CRD
// that already has it alone. With ConversionWebhook and no CA to give the API server, it sets
// None instead: a webhook the API server cannot trust would fail every conversion, while None
// is exact for versions with the same fields.
func (u *Upgrader) ConfigureConversion(ctx context.Context) error {
	if u.Conversion == ConversionUnmanaged {
		return nil
	}
	want, caErr := u.desiredConversion()
	var errs []error
	if caErr != nil {
		errs = append(errs, caErr)
	}
	for _, k := range Kinds {
		if err := u.configureKind(ctx, k, want); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", k.CRDName(), err))
		}
	}
	return errors.Join(errs...)
}

// desiredConversion is the conversion the CRDs should have. An error means the webhook was
// asked for and could not be configured; the result is then None.
func (u *Upgrader) desiredConversion() (*apiextensionsv1.CustomResourceConversion, error) {
	none := &apiextensionsv1.CustomResourceConversion{Strategy: apiextensionsv1.NoneConverter}
	if u.Conversion != ConversionWebhook {
		return none, nil
	}
	ca, err := os.ReadFile(u.CAFile)
	if err == nil && len(ca) == 0 {
		err = errors.New("the file is empty")
	}
	if err != nil {
		return none, fmt.Errorf("no CA for the conversion webhook in %s (%w); the CRDs keep the None strategy, which "+
			"converts exactly while v1beta1 and v1 have the same fields", u.CAFile, err)
	}
	port := u.Port
	if port == 0 {
		port = 443
	}
	return &apiextensionsv1.CustomResourceConversion{
		Strategy: apiextensionsv1.WebhookConverter,
		Webhook: &apiextensionsv1.WebhookConversion{
			ClientConfig: &apiextensionsv1.WebhookClientConfig{
				Service: &apiextensionsv1.ServiceReference{
					Namespace: u.Service.Namespace,
					Name:      u.Service.Name,
					Path:      new(ConvertPath),
					Port:      new(port),
				},
				CABundle: ca,
			},
			ConversionReviewVersions: []string{"v1"},
		},
	}, nil
}

func (u *Upgrader) configureKind(ctx context.Context, k Kind, want *apiextensionsv1.CustomResourceConversion) error {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := u.Client.Get(ctx, client.ObjectKey{Name: k.CRDName()}, crd); err != nil {
		return err
	}
	if sameConversion(crd.Spec.Conversion, want) {
		return nil
	}
	before := client.MergeFrom(crd.DeepCopy())
	crd.Spec.Conversion = want.DeepCopy()
	if err := u.Client.Patch(ctx, crd, before); err != nil {
		return err
	}
	logf.FromContext(ctx).WithName("crd-versions").Info("Configured the CRD's conversion",
		"crd", k.CRDName(), "strategy", want.Strategy)
	if u.Recorder != nil {
		note := "Conversion between versions is done by the API server (strategy None)"
		if want.Strategy == apiextensionsv1.WebhookConverter {
			note = fmt.Sprintf("Conversion between versions goes to the operator's webhook, Service %s, path %s",
				u.Service, ConvertPath)
		}
		u.Recorder.Eventf(crd, nil, corev1.EventTypeNormal, EventConversionConfigured, "ConfigureConversion", note)
	}
	return nil
}

// sameConversion compares what the operator sets. The API server defaults nothing the
// operator leaves out, so a plain comparison is enough.
func sameConversion(got, want *apiextensionsv1.CustomResourceConversion) bool {
	if got == nil {
		got = &apiextensionsv1.CustomResourceConversion{Strategy: apiextensionsv1.NoneConverter}
	}
	if got.Strategy != want.Strategy {
		return false
	}
	if want.Strategy != apiextensionsv1.WebhookConverter {
		return true
	}
	if got.Webhook == nil || got.Webhook.ClientConfig == nil || got.Webhook.ClientConfig.Service == nil {
		return false
	}
	g, w := got.Webhook.ClientConfig, want.Webhook.ClientConfig
	return g.URL == nil &&
		g.Service.Namespace == w.Service.Namespace && g.Service.Name == w.Service.Name &&
		ptr.Deref(g.Service.Path, "") == ptr.Deref(w.Service.Path, "") &&
		ptr.Deref(g.Service.Port, 443) == ptr.Deref(w.Service.Port, 443) &&
		string(g.CABundle) == string(w.CABundle) &&
		slices.Equal(got.Webhook.ConversionReviewVersions, want.Webhook.ConversionReviewVersions)
}

func storageVersion(crd *apiextensionsv1.CustomResourceDefinition) string {
	for _, v := range crd.Spec.Versions {
		if v.Storage {
			return v.Name
		}
	}
	return ""
}

func record(k Kind, versions []string) {
	storedVersions.DeletePartialMatch(prometheus.Labels{labelKind: k.Kind})
	for _, v := range versions {
		storedVersions.WithLabelValues(k.Kind, v).Set(1)
	}
}

func without(list []string, drop string) []string {
	return slices.DeleteFunc(slices.Clone(list), func(s string) bool { return s == drop })
}
