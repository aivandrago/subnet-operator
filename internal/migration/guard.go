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

package migration

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kevents "k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
)

// UpgradeGuide is where a person blocked by the guard finds what to do.
const UpgradeGuide = "https://hypersurgery.dev/docs/#upgrade (docs/operations/upgrades.md, \"Upgrading from 0.8 to 0.9\")"

// EventMigrationPending is the reason of the Warning Event on an aws.hypersurgery/v1alpha1
// object that was never migrated.
const EventMigrationPending = "MigrationPending"

// The old group is only listed, never read in full or written: the guard needs to know which
// objects exist and whether 0.8 marked them.
// +kubebuilder:rbac:groups=aws.hypersurgery,resources=networkscopes;subnetclaims;resourceimports;sheetexports,verbs=list

// oldGroupVersion is the group the operator served up to 0.8.
var oldGroupVersion = schema.GroupVersion{Group: "aws.hypersurgery", Version: "v1alpha1"}

// oldKinds are the aws.hypersurgery/v1alpha1 kinds people wrote and 0.8 migrated. VPC and
// Subnet objects were a cache that the new group rebuilds, so nothing is lost with them.
var oldKinds = []string{kindNetworkScope, kindSubnetClaim, kindResourceImport, kindSheetExport}

// Unmigrated is an aws.hypersurgery/v1alpha1 object without the migrated-to marker 0.8 sets
// once the object's copy exists in network.hypersurgery.dev.
type Unmigrated struct {
	Kind      string
	Namespace string
	Name      string
	UID       types.UID
}

func (u Unmigrated) String() string {
	if u.Namespace == "" {
		return u.Kind + " " + u.Name
	}
	return u.Kind + " " + u.Namespace + "/" + u.Name
}

// Scan is what FindUnmigrated found.
type Scan struct {
	// Served lists the old kinds the API server still serves, that is whose CRDs exist.
	Served []string
	// Objects are the unmigrated objects, sorted by kind, namespace and name.
	Objects []Unmigrated
}

// Count returns the number of unmigrated objects of a kind.
func (r Scan) Count(kind string) int {
	n := 0
	for _, o := range r.Objects {
		if o.Kind == kind {
			n++
		}
	}
	return n
}

// Summary names the first few unmigrated objects, for a log line or a probe answer.
func (r Scan) Summary() string {
	const shown = 5
	names := make([]string, 0, shown)
	for i, o := range r.Objects {
		if i == shown {
			names = append(names, fmt.Sprintf("and %d more", len(r.Objects)-shown))
			break
		}
		names = append(names, o.String())
	}
	return strings.Join(names, ", ")
}

// FindUnmigrated lists the aws.hypersurgery/v1alpha1 objects that 0.8 did not migrate. It
// reads metadata only, so it works without the old group's Go types and schema. A kind whose
// CRD is gone is simply not served; any other error is returned, because an answer that could
// not be checked must not be taken for "nothing to migrate".
//
// An object being deleted does not count: it is on its way out either way.
func FindUnmigrated(ctx context.Context, c client.Reader) (Scan, error) {
	var report Scan
	for _, kind := range oldKinds {
		list := &metav1.PartialObjectMetadataList{}
		list.SetGroupVersionKind(oldGroupVersion.WithKind(kind + "List"))
		if err := c.List(ctx, list); err != nil {
			if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
				continue
			}
			return Scan{}, fmt.Errorf("listing %s.%s: %w", kind, oldGroupVersion.Group, err)
		}
		report.Served = append(report.Served, kind)
		for _, o := range list.Items {
			if !o.DeletionTimestamp.IsZero() {
				continue
			}
			if _, done := o.Annotations[networkv1beta1.AnnotationMigratedTo]; done {
				continue
			}
			report.Objects = append(report.Objects, Unmigrated{Kind: kind, Namespace: o.Namespace, Name: o.Name, UID: o.UID})
		}
	}
	slices.SortFunc(report.Objects, func(a, b Unmigrated) int {
		return strings.Compare(a.Kind+"\x00"+a.Namespace+"\x00"+a.Name, b.Kind+"\x00"+b.Namespace+"\x00"+b.Name)
	})
	return report, nil
}

// pendingObjects counts the old-group objects that were never migrated. While any exist the
// operator does not start its controllers (see Guard).
var pendingObjects = prometheus.NewGaugeVec(prometheus.GaugeOpts{
	Name: "hs_migration_pending_objects",
	Help: "aws.hypersurgery/v1alpha1 objects that were never migrated to network.hypersurgery.dev, by kind; " +
		"0 when the old group is gone. The operator does not start its controllers while any exist " +
		"at startup: upgrade to 0.8.x first and let it migrate them.",
}, []string{"kind"})

func init() {
	ctrlmetrics.Registry.MustRegister(pendingObjects)
}

// record sets hs_migration_pending_objects from a report: every old kind has a sample, 0 when
// nothing of it is pending or its CRD is gone.
func record(r Scan) {
	for _, kind := range oldKinds {
		pendingObjects.WithLabelValues(kind).Set(float64(r.Count(kind)))
	}
}

// Message is what the operator says when it will not start: which objects are in the way,
// and what to do about them.
func Message(r Scan) string {
	return fmt.Sprintf("%d aws.hypersurgery/v1alpha1 object(s) were never migrated to network.hypersurgery.dev (%s). "+
		"This release no longer serves or migrates that group, and will not start its controllers while they exist: "+
		"their reservations and history would be ignored. Roll back to 0.8.x (helm rollback), wait until "+
		"hs_migration_pending_objects is 0 or every old object carries %s, then upgrade again; "+
		"or delete the objects you do not need. See %s",
		len(r.Objects), r.Summary(), networkv1beta1.AnnotationMigratedTo, UpgradeGuide)
}

// eventNote is the Event on each unmigrated object.
const eventNote = "Never migrated to network.hypersurgery.dev: this release no longer migrates aws.hypersurgery/v1alpha1 " +
	"objects, and ignores this one. Let 0.8.x migrate it (roll back and wait until it carries " +
	networkv1beta1.AnnotationMigratedTo + "), or convert its manifest with `manager migrate-manifests`, apply the " +
	"result and delete this object. See " + UpgradeGuide

// maxEvents bounds the Events one check records, so a cluster full of old objects does not
// get a storm of them; the log and the metric have the full count.
const maxEvents = 20

// Guard keeps the operator from running its controllers next to aws.hypersurgery/v1alpha1
// objects that 0.8 never migrated. They would be ignored — a claim's reservations handed out
// again, an import's history lost — so instead the operator starts without controllers and
// webhooks, answers its readiness probe with the reason, sets hs_migration_pending_objects,
// records an Event on each such object, and checks again every Interval. Once nothing is left
// (the objects were migrated by a 0.8 replica, or deleted, or their CRDs removed), Start
// returns ErrCleared, and the process exits to be restarted into normal operation.
//
// During a rolling upgrade from 0.8 the new pod therefore never becomes ready, and the 0.8
// pods keep running.
type Guard struct {
	// Client lists the old objects; it must not be a cache, which would need a watch on a
	// group the operator no longer serves.
	Client client.Reader
	// Recorder puts a Warning Event on unmigrated objects. Nil records none.
	Recorder kevents.EventRecorder
	// Interval is how often the guard looks again. Zero means 30 seconds.
	Interval time.Duration

	mu     sync.Mutex
	reason string
	warned map[types.UID]bool
}

// ErrCleared is returned by Guard.Start once nothing blocks the operator any more.
var ErrCleared = errors.New("no unmigrated aws.hypersurgery/v1alpha1 objects are left")

// NeedLeaderElection lets the guard run without the lease: a blocked replica must not take it
// from a 0.8 replica that is still migrating.
func (g *Guard) NeedLeaderElection() bool { return false }

// Check looks once, and reports whether the operator may run.
func (g *Guard) Check(ctx context.Context) bool {
	log := logf.FromContext(ctx)
	report, err := FindUnmigrated(ctx, g.Client)
	if err != nil {
		g.setReason(fmt.Sprintf("could not check for unmigrated aws.hypersurgery/v1alpha1 objects: %v. "+
			"The operator needs list on them while their CRDs exist; delete the CRDs once nothing is left to migrate. "+
			"See %s", err, UpgradeGuide))
		log.Error(err, "Could not check for unmigrated aws.hypersurgery/v1alpha1 objects; not starting the controllers",
			"guide", UpgradeGuide)
		return false
	}
	record(report)
	if len(report.Objects) == 0 {
		g.setReason("")
		return true
	}
	g.setReason(Message(report))
	log.Error(errors.New("unmigrated objects"), Message(report), "count", len(report.Objects))
	g.warn(report)
	return false
}

// Start checks at once, which records the Events, and then every Interval until nothing
// blocks the operator any more.
func (g *Guard) Start(ctx context.Context) error {
	interval := g.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if g.Check(ctx) {
			logf.FromContext(ctx).Info("Nothing is left to migrate from aws.hypersurgery/v1alpha1; restarting to start the controllers")
			return ErrCleared
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

// Ready is the readiness check of a blocked operator: it fails with the reason.
func (g *Guard) Ready(_ *http.Request) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.reason != "" {
		return errors.New(g.reason)
	}
	return nil
}

func (g *Guard) setReason(reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.reason = reason
}

// warn records the Event on objects not warned about yet by this process.
func (g *Guard) warn(report Scan) {
	if g.Recorder == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.warned == nil {
		g.warned = map[types.UID]bool{}
	}
	recorded := 0
	for _, o := range report.Objects {
		if recorded == maxEvents {
			return
		}
		if g.warned[o.UID] {
			continue
		}
		g.warned[o.UID] = true
		recorded++
		g.Recorder.Eventf(reference(o), nil, corev1.EventTypeWarning, EventMigrationPending, "Blocked", eventNote)
	}
}

// reference is an object an Event can be recorded on without the old group's Go types.
func reference(o Unmigrated) *metav1.PartialObjectMetadata {
	obj := &metav1.PartialObjectMetadata{
		ObjectMeta: metav1.ObjectMeta{Namespace: o.Namespace, Name: o.Name, UID: o.UID},
	}
	obj.SetGroupVersionKind(oldGroupVersion.WithKind(o.Kind))
	return obj
}

// Watcher keeps hs_migration_pending_objects current while the operator runs, and warns with
// an Event about an old object that appears after the start, for example one a GitOps tool
// applies again from a manifest nobody converted. Such an object is ignored; it does not stop
// an operator that is already running.
type Watcher struct {
	// Client lists the old objects; it must not be a cache (see Guard).
	Client client.Reader
	// Recorder puts a Warning Event on unmigrated objects. Nil records none.
	Recorder kevents.EventRecorder
	// Interval is how often it looks. Zero means a minute.
	Interval time.Duration

	guard Guard
	// logged is the count last logged, so the log says it once rather than every minute.
	logged int
}

// NeedLeaderElection lets every replica count, so a standby's metrics say the same.
func (w *Watcher) NeedLeaderElection() bool { return false }

// Start looks every Interval until the manager stops. The old group only ever shrinks in
// normal operation, so once none of its CRDs exist it stops looking.
func (w *Watcher) Start(ctx context.Context) error {
	interval := w.Interval
	if interval <= 0 {
		interval = time.Minute
	}
	w.guard.Recorder = w.Recorder
	log := logf.FromContext(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		report, err := FindUnmigrated(ctx, w.Client)
		switch {
		case err != nil:
			log.Error(err, "Could not count unmigrated aws.hypersurgery/v1alpha1 objects")
		default:
			record(report)
			if len(report.Objects) > 0 && len(report.Objects) != w.logged {
				log.Info("Ignoring aws.hypersurgery/v1alpha1 objects that were never migrated; "+
					"convert their manifests with `manager migrate-manifests`", "objects", report.Summary(),
					"guide", UpgradeGuide)
			}
			w.logged = len(report.Objects)
			w.guard.warn(report)
			if len(report.Served) == 0 {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
