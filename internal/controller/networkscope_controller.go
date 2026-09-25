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

package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kevents "k8s.io/client-go/tools/events"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
	"hypersurgery.dev/subnet-operator/internal/audit"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/metrics"
	"hypersurgery.dev/subnet-operator/internal/tenancy"
)

const (
	// ConditionReady is True when every account/region of the scope synced successfully.
	ConditionReady = "Ready"

	defaultResyncInterval = 10 * time.Minute
	minResyncInterval     = time.Minute
	defaultConcurrency    = 4
)

// NetworkScopeReconciler discovers the accounts and regions of a NetworkScope and mirrors
// their VPCs and subnets as VPC and Subnet objects. It never writes to AWS.
type NetworkScopeReconciler struct {
	client.Client
	Scheme     *runtime.Scheme
	Discoverer inventory.Discoverer
	// APIReader reads VPC and Subnet lists directly from the API server, because the
	// informer cache may not yet contain objects created earlier in the same sync.
	// When nil, the cached client is used.
	APIReader client.Reader
	// Concurrency is the number of account/region pairs discovered in parallel, across all
	// scopes: the cap is on what this instance asks of AWS at once, so it must not multiply
	// with the number of scopes being reconciled.
	Concurrency int
	// Creators remembers who created a resource, from CloudTrail events. The auto-import
	// policy asks it before falling back to inheritance or account defaults.
	Creators *CreatorCache
	// Recorder puts policy decisions and unreachable targets on the scope, where
	// `kubectl describe networkscope` shows them.
	Recorder kevents.EventRecorder
	// Audit receives one line per policy decision. Nil keeps the audit trail off.
	Audit audit.Sink
	// Clock tells the time for resync and backoff decisions. Nil is the real clock.
	Clock clock.PassiveClock
	// Identity is the Kubernetes user the operator authenticates as. The auto-import policy
	// writes it into the created-by annotation of the imports it creates — the same value the
	// admission webhook would write — and into the created_by of its audit lines. Empty when
	// it could not be found out; the webhook still records the real one on the import.
	Identity string

	pending     pendingTargets
	backoff     throttleBackoff
	slots       chan struct{}
	slotsOnce   sync.Once
	changes     chan event.GenericEvent
	changesOnce sync.Once
}

// +kubebuilder:rbac:groups=aws.hypersurgery,resources=networkscopes,verbs=get;list;watch
// +kubebuilder:rbac:groups=aws.hypersurgery,resources=networkscopes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=aws.hypersurgery,resources=networkscopes/finalizers,verbs=update
// +kubebuilder:rbac:groups=aws.hypersurgery,resources=vpcs;subnets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=aws.hypersurgery,resources=vpcs/status;subnets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=aws.hypersurgery,resources=resourceimports,verbs=get;list;watch;create
// Events are written on the objects people look at, including cluster-scoped ones, so this
// permission cannot be namespaced the way the credential Secret is.
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// Reconcile syncs the scope. A full sync (every account/region) runs when the spec changed or
// the resync interval elapsed; in between, only the targets reported changed by EC2 events
// (see NotifyChanged) are synced.
func (r *NetworkScopeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	log := logf.FromContext(ctx)

	scope := &awsv1alpha1.NetworkScope{}
	if err := r.Get(ctx, req.NamespacedName, scope); err != nil {
		if apierrors.IsNotFound(err) {
			// VPC and Subnet objects are garbage-collected through their owner references.
			metrics.Forget(req.Name)
			r.pending.take(req.Name)
			r.backoff.forget(req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !scope.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Once per change of the spec, not on every sync: the warning is about how the scope is
	// written, and a spec change is when somebody is looking at it.
	if scope.Spec.NamespaceSelector == nil && scope.Status.ObservedGeneration != scope.Generation {
		log.Info("NetworkScope has no namespaceSelector; every namespace may use it", "scope", scope.Name)
		eventf(r.Recorder, scope, corev1.EventTypeWarning, EventNamespacesUnrestricted, ActionCheckNamespaces,
			"%s", tenancy.UnrestrictedWarning(scope))
	}

	now := r.now()
	all := expandTargets(scope)
	full := needsFullSync(scope, now)
	// Taken before discovery: events arriving during the sync stay pending and trigger another one.
	changed := r.pending.take(scope.Name)
	defer func() {
		if reterr != nil {
			r.pending.add(scope.Name, changed)
		}
	}()

	// A target backed off after throttling is left out until its delay runs out — even from a
	// full sync, even when an event names it: another call now is exactly what the account
	// cannot take. Once the delay has run out it is tried on its own, between full syncs,
	// rather than waiting for the next one and joining its burst.
	var targets []inventory.Target
	for _, t := range all {
		key := t.Key()
		if r.backoff.waiting(scope.Name, key, now) {
			continue
		}
		if full || changed[key] || r.backoff.due(scope.Name, key, now) {
			targets = append(targets, t)
		}
	}
	if !full {
		if len(targets) == 0 {
			return ctrl.Result{RequeueAfter: r.requeueAfter(scope, all, untilFullSync(scope, now))}, nil
		}
		log.V(1).Info("syncing changed targets", "targets", len(targets))
	}
	results := r.discoverAll(ctx, targets)

	tagKeys := resolveTagKeys(scope.Spec.TagKeys)
	for i := range results {
		res := &results[i]
		key := res.target.Key()
		if errors.Is(res.err, inventory.ErrThrottled) {
			// Reachable but busy: the last known inventory stays, and the target is retried on
			// its own schedule instead of at the scope's cadence.
			until := r.backoff.throttled(scope.Name, key, r.now())
			log.Info("discovery throttled, backing off", "account", res.target.Account,
				"region", res.target.Region, "retryAt", until, "error", res.err.Error())
			eventf(r.Recorder, scope, corev1.EventTypeWarning, EventTargetThrottled, ActionDiscover,
				"Account %s in %s is throttled by the API, next attempt at %s: %v", res.target.Account,
				res.target.Region, until.UTC().Format(time.RFC3339), res.err)
			continue
		}
		r.backoff.reset(scope.Name, key)
		if res.err != nil {
			log.Error(res.err, "discovery failed", "account", res.target.Account, "region", res.target.Region)
			// The status keeps the last known inventory of this target, so without an Event
			// nothing on the object says the numbers stopped moving.
			eventf(r.Recorder, scope, corev1.EventTypeWarning, EventTargetUnreachable, ActionDiscover,
				"Could not read account %s in %s: %v", res.target.Account, res.target.Region, res.err)
			continue
		}
		if err := r.syncTarget(ctx, scope, res.target, res.snapshot, tagKeys); err != nil {
			// Kubernetes API errors are retried with backoff; AWS errors wait for the next resync.
			return ctrl.Result{}, fmt.Errorf("sync %s: %w", res.target.Key(), err)
		}
	}

	if full {
		if err := r.deleteRemovedTargets(ctx, scope.Name, all); err != nil {
			return ctrl.Result{}, err
		}
	}
	vpcs, err := r.updateOverlaps(ctx, scope.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	subnets := &awsv1alpha1.SubnetList{}
	if err := r.reader().List(ctx, subnets, client.MatchingLabels{awsv1alpha1.LabelScope: scope.Name}); err != nil {
		return ctrl.Result{}, err
	}

	// What the selector left out: counted, alerted on, and offered to the policy.
	r.reportUnmanaged(ctx, scope, results)

	syncTime := metav1.NewTime(now)
	statusTargets, throttled, err := r.updateStatus(ctx, scope, all, results, full, len(vpcs), len(subnets.Items), syncTime)
	if err != nil {
		return ctrl.Result{}, err
	}
	metrics.SetScope(scope.Name, vpcs, subnets.Items, metricTargets(statusTargets, results, throttled), float64(now.Unix()))

	if full {
		return ctrl.Result{RequeueAfter: r.requeueAfter(scope, all, resyncInterval(scope))}, nil
	}
	return ctrl.Result{RequeueAfter: r.requeueAfter(scope, all, untilFullSync(scope, now))}, nil
}

func (r *NetworkScopeReconciler) now() time.Time {
	if r.Clock == nil {
		return time.Now()
	}
	return r.Clock.Now()
}

// requeueAfter brings the next reconcile forward from untilNext when a backed-off target of
// the scope is due before it. The backoff was set after discovery, so it is measured from the
// clock now rather than from the start of the reconcile.
func (r *NetworkScopeReconciler) requeueAfter(scope *awsv1alpha1.NetworkScope, all []inventory.Target,
	untilNext time.Duration) time.Duration {
	if until, ok := r.backoff.next(scope.Name, all); ok {
		return min(untilNext, max(until.Sub(r.now()), time.Second))
	}
	return untilNext
}

// fullSyncSlack absorbs timer jitter, so a requeue scheduled for the interval is not
// mistaken for an early one.
const fullSyncSlack = 5 * time.Second

func needsFullSync(scope *awsv1alpha1.NetworkScope, now time.Time) bool {
	if scope.Status.LastSyncTime == nil || scope.Status.ObservedGeneration != scope.Generation {
		return true
	}
	return !now.Before(scope.Status.LastSyncTime.Add(resyncInterval(scope) - fullSyncSlack))
}

func untilFullSync(scope *awsv1alpha1.NetworkScope, now time.Time) time.Duration {
	if scope.Status.LastSyncTime == nil {
		return time.Second
	}
	return max(scope.Status.LastSyncTime.Add(resyncInterval(scope)).Sub(now), time.Second)
}

func (r *NetworkScopeReconciler) reader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

type targetResult struct {
	target   inventory.Target
	snapshot *inventory.Snapshot
	err      error
}

func expandTargets(scope *awsv1alpha1.NetworkScope) []inventory.Target {
	var targets []inventory.Target
	for _, a := range scope.Spec.Accounts {
		regions := a.Regions
		if len(regions) == 0 {
			regions = scope.Spec.Regions
		}
		for _, region := range regions {
			targets = append(targets, inventory.Target{
				Scope:             scope.Name,
				Account:           a.ID,
				Region:            region,
				RoleARN:           a.RoleARN,
				ExternalID:        a.ExternalID,
				VPCTagSelector:    scope.Spec.VPCTagSelector,
				DiscoverUnmanaged: discoverUnmanaged(scope),
			})
		}
	}
	return targets
}

// discoverySlots is the instance-wide semaphore behind Concurrency. It is shared by every
// reconcile, so the cap holds however many scopes are reconciled at once.
func (r *NetworkScopeReconciler) discoverySlots() chan struct{} {
	r.slotsOnce.Do(func() {
		r.slots = make(chan struct{}, r.concurrency())
	})
	return r.slots
}

func (r *NetworkScopeReconciler) concurrency() int {
	if r.Concurrency <= 0 {
		return defaultConcurrency
	}
	return r.Concurrency
}

func (r *NetworkScopeReconciler) discoverAll(ctx context.Context, targets []inventory.Target) []targetResult {
	results := make([]targetResult, len(targets))
	slots := r.discoverySlots()
	g, gctx := errgroup.WithContext(ctx)
	// The per-call limit only bounds the goroutines of this sync; the slots are the real cap.
	g.SetLimit(r.concurrency())
	for i, t := range targets {
		g.Go(func() error {
			select {
			case slots <- struct{}{}:
			case <-gctx.Done():
				results[i] = targetResult{target: t, err: gctx.Err()}
				return nil
			}
			defer func() { <-slots }()
			snap, err := r.Discoverer.Discover(gctx, t)
			results[i] = targetResult{target: t, snapshot: snap, err: err}
			return nil // one failing account must not stop the others
		})
	}
	_ = g.Wait()
	return results
}

func resolveTagKeys(k awsv1alpha1.TagKeys) awsv1alpha1.TagKeys {
	if k.Owner == "" {
		k.Owner = awsv1alpha1.DefaultOwnerTagKey
	}
	if k.Env == "" {
		k.Env = awsv1alpha1.DefaultEnvTagKey
	}
	if k.Tier == "" {
		k.Tier = awsv1alpha1.DefaultTierTagKey
	}
	return k
}

func resyncInterval(scope *awsv1alpha1.NetworkScope) time.Duration {
	if scope.Spec.ResyncInterval == nil {
		return defaultResyncInterval
	}
	return max(scope.Spec.ResyncInterval.Duration, minResyncInterval)
}

// syncTarget creates, updates and deletes the VPC and Subnet objects of one account/region
// so that they match the snapshot.
func (r *NetworkScopeReconciler) syncTarget(ctx context.Context, scope *awsv1alpha1.NetworkScope,
	target inventory.Target, snap *inventory.Snapshot, tagKeys awsv1alpha1.TagKeys) error {
	type vpcTotals struct {
		subnets          int32
		total, available int64
	}
	totals := map[string]*vpcTotals{}
	for _, v := range snap.VPCs {
		totals[v.ID] = &vpcTotals{}
	}

	seenSubnets := map[string]bool{}
	for _, s := range snap.Subnets {
		status := subnetStatus(s, tagKeys, scope.Spec.RequiredSubnetTags)
		if t := totals[s.VPCID]; t != nil {
			t.subnets++
			t.total += status.TotalIPs
			t.available += status.AvailableIPs
		}
		obj := &awsv1alpha1.Subnet{ObjectMeta: metav1.ObjectMeta{Name: s.ID}}
		spec := awsv1alpha1.SubnetSpec{SubnetID: s.ID, VPCID: s.VPCID, Account: s.Account, Region: s.Region}
		ok, err := r.upsert(ctx, scope, obj, labelsFor(scope.Name, target, s.VPCID), func() {
			obj.Spec = spec
		}, func() bool {
			if equality.Semantic.DeepEqual(obj.Status, status) {
				return false
			}
			obj.Status = status
			return true
		})
		if err != nil {
			return err
		}
		if ok {
			seenSubnets[s.ID] = true
		}
	}

	seenVPCs := map[string]bool{}
	for _, v := range snap.VPCs {
		t := totals[v.ID]
		obj := &awsv1alpha1.VPC{ObjectMeta: metav1.ObjectMeta{Name: v.ID}}
		spec := awsv1alpha1.VPCSpec{VPCID: v.ID, Account: v.Account, Region: v.Region}
		ok, err := r.upsert(ctx, scope, obj, labelsFor(scope.Name, target, v.ID), func() {
			obj.Spec = spec
		}, func() bool {
			status := awsv1alpha1.VPCStatus{
				Name:           v.Tags["Name"],
				State:          v.State,
				IsDefault:      v.IsDefault,
				CIDRBlocks:     v.CIDRBlocks,
				IPv6CIDRBlocks: v.IPv6CIDRBlocks,
				Owner:          v.Tags[tagKeys.Owner],
				Env:            v.Tags[tagKeys.Env],
				Tags:           v.Tags,
				Subnets:        t.subnets,
				TotalIPs:       t.total,
				AvailableIPs:   t.available,
				// Overlaps are computed across the whole scope in updateOverlaps.
				OverlapsWith: obj.Status.OverlapsWith,
			}
			if equality.Semantic.DeepEqual(obj.Status, status) {
				return false
			}
			obj.Status = status
			return true
		})
		if err != nil {
			return err
		}
		if ok {
			seenVPCs[v.ID] = true
		}
	}

	return r.deleteGone(ctx, scope.Name, target, seenVPCs, seenSubnets)
}

func subnetStatus(s inventory.Subnet, tagKeys awsv1alpha1.TagKeys, required []string) awsv1alpha1.SubnetStatus {
	total := inventory.UsableIPv4(s.CIDRBlock)
	status := awsv1alpha1.SubnetStatus{
		Name:               s.Tags["Name"],
		State:              s.State,
		CIDRBlock:          s.CIDRBlock,
		IPv6CIDRBlocks:     s.IPv6CIDRBlocks,
		AvailabilityZone:   s.AvailabilityZone,
		AvailabilityZoneID: s.AvailabilityZoneID,
		Public:             s.Public,
		RouteTableID:       s.RouteTableID,
		TotalIPs:           total,
		AvailableIPs:       s.AvailableIPs,
		UtilizationPercent: inventory.UtilizationPercent(total, s.AvailableIPs),
		Owner:              s.Tags[tagKeys.Owner],
		Env:                s.Tags[tagKeys.Env],
		Tier:               s.Tags[tagKeys.Tier],
		Tags:               s.Tags,
	}
	for _, k := range required {
		if strings.TrimSpace(s.Tags[k]) == "" {
			status.MissingTags = append(status.MissingTags, k)
		}
	}
	return status
}

func labelsFor(scope string, target inventory.Target, vpcID string) map[string]string {
	return map[string]string{
		awsv1alpha1.LabelScope:   scope,
		awsv1alpha1.LabelAccount: target.Account,
		awsv1alpha1.LabelRegion:  target.Region,
		awsv1alpha1.LabelVPC:     vpcID,
	}
}

// upsert creates or updates obj (labels, owner reference, spec via mutateSpec), then
// writes its status when mutateStatus reports a change. It returns false without error when
// the object already belongs to another NetworkScope: two scopes covering the same account
// and region must not fight over the same objects.
func (r *NetworkScopeReconciler) upsert(ctx context.Context, scope *awsv1alpha1.NetworkScope, obj client.Object,
	labels map[string]string, mutateSpec func(), mutateStatus func() bool) (bool, error) {
	log := logf.FromContext(ctx)
	conflict := false
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, obj, func() error {
		if owner := obj.GetLabels()[awsv1alpha1.LabelScope]; owner != "" && owner != scope.Name {
			conflict = true
			return nil
		}
		obj.SetLabels(labels)
		mutateSpec()
		return controllerutil.SetControllerReference(scope, obj, r.Scheme)
	})
	if err != nil {
		return false, err
	}
	if conflict {
		log.Info("object belongs to another NetworkScope, skipping", "name", obj.GetName(),
			"owner", obj.GetLabels()[awsv1alpha1.LabelScope])
		return false, nil
	}
	if mutateStatus() {
		if err := r.Status().Update(ctx, obj); err != nil {
			return false, err
		}
	}
	return true, nil
}

// deleteGone deletes objects of the target that were not in the latest snapshot.
// It only touches Kubernetes objects; AWS is never modified.
func (r *NetworkScopeReconciler) deleteGone(ctx context.Context, scope string, target inventory.Target,
	seenVPCs, seenSubnets map[string]bool) error {
	sel := client.MatchingLabels{
		awsv1alpha1.LabelScope:   scope,
		awsv1alpha1.LabelAccount: target.Account,
		awsv1alpha1.LabelRegion:  target.Region,
	}
	return r.deleteObjects(ctx, sel,
		func(s *awsv1alpha1.Subnet) bool { return !seenSubnets[s.Name] },
		func(v *awsv1alpha1.VPC) bool { return !seenVPCs[v.Name] })
}

// deleteRemovedTargets deletes objects of account/region pairs that are no longer in the scope spec.
func (r *NetworkScopeReconciler) deleteRemovedTargets(ctx context.Context, scope string, targets []inventory.Target) error {
	current := map[string]bool{}
	for _, t := range targets {
		current[t.Account+"/"+t.Region] = true
	}
	removed := func(o client.Object) bool {
		l := o.GetLabels()
		return !current[l[awsv1alpha1.LabelAccount]+"/"+l[awsv1alpha1.LabelRegion]]
	}
	return r.deleteObjects(ctx, client.MatchingLabels{awsv1alpha1.LabelScope: scope},
		func(s *awsv1alpha1.Subnet) bool { return removed(s) },
		func(v *awsv1alpha1.VPC) bool { return removed(v) })
}

// deleteObjects deletes the Subnet and VPC objects matching sel for which the predicates return true.
func (r *NetworkScopeReconciler) deleteObjects(ctx context.Context, sel client.MatchingLabels,
	deleteSubnet func(*awsv1alpha1.Subnet) bool, deleteVPC func(*awsv1alpha1.VPC) bool) error {
	subnets := &awsv1alpha1.SubnetList{}
	if err := r.reader().List(ctx, subnets, sel); err != nil {
		return err
	}
	for i := range subnets.Items {
		if deleteSubnet(&subnets.Items[i]) {
			if err := client.IgnoreNotFound(r.Delete(ctx, &subnets.Items[i])); err != nil {
				return err
			}
		}
	}
	vpcs := &awsv1alpha1.VPCList{}
	if err := r.reader().List(ctx, vpcs, sel); err != nil {
		return err
	}
	for i := range vpcs.Items {
		if deleteVPC(&vpcs.Items[i]) {
			if err := client.IgnoreNotFound(r.Delete(ctx, &vpcs.Items[i])); err != nil {
				return err
			}
		}
	}
	return nil
}

// updateOverlaps recomputes CIDR overlaps between all VPCs of the scope, including VPCs of
// targets whose last discovery failed, and returns the up-to-date VPC objects.
func (r *NetworkScopeReconciler) updateOverlaps(ctx context.Context, scope string) ([]awsv1alpha1.VPC, error) {
	list := &awsv1alpha1.VPCList{}
	if err := r.reader().List(ctx, list, client.MatchingLabels{awsv1alpha1.LabelScope: scope}); err != nil {
		return nil, err
	}
	vpcs := list.Items
	for i := range vpcs {
		var overlaps []string
		for j := range vpcs {
			if i != j && inventory.Overlaps(vpcs[i].Status.CIDRBlocks, vpcs[j].Status.CIDRBlocks) {
				overlaps = append(overlaps, vpcRef(&vpcs[j]))
			}
		}
		slices.Sort(overlaps)
		if equality.Semantic.DeepEqual(vpcs[i].Status.OverlapsWith, overlaps) {
			continue
		}
		vpcs[i].Status.OverlapsWith = overlaps
		if err := r.Status().Update(ctx, &vpcs[i]); err != nil {
			return nil, err
		}
	}
	return vpcs, nil
}

func vpcRef(v *awsv1alpha1.VPC) string {
	return v.Spec.Account + "/" + v.Spec.Region + "/" + v.Spec.VPCID
}

// metricTargets turns the status of each target into its metrics. A throttled target counts
// as up: it answered, only not fast enough, and hs_aws_target_throttled says so on its own.
func metricTargets(targets []awsv1alpha1.TargetStatus, results []targetResult,
	throttled map[inventory.TargetKey]bool) []metrics.TargetResult {
	synced := map[inventory.TargetKey]bool{}
	for _, r := range results {
		synced[r.target.Key()] = true
	}
	out := make([]metrics.TargetResult, 0, len(targets))
	for _, t := range targets {
		key := inventory.TargetKey{Account: t.Account, Region: t.Region}
		out = append(out, metrics.TargetResult{
			Account: t.Account, Region: t.Region, OK: t.Error == "" || throttled[key],
			Synced: synced[key], Throttled: throttled[key],
		})
	}
	return out
}

// updateStatus records the sync. Targets that were not synced this time keep their previous
// status; lastSyncTime of the scope only moves on full syncs, because it schedules the next one.
// It also returns which targets are throttled rather than failed.
func (r *NetworkScopeReconciler) updateStatus(ctx context.Context, scope *awsv1alpha1.NetworkScope,
	all []inventory.Target, results []targetResult, full bool, vpcs, subnets int, now metav1.Time,
) ([]awsv1alpha1.TargetStatus, map[inventory.TargetKey]bool, error) {
	byKey := map[inventory.TargetKey]targetResult{}
	for _, res := range results {
		byKey[res.target.Key()] = res
	}

	var targets []awsv1alpha1.TargetStatus
	var throttled map[inventory.TargetKey]bool
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &awsv1alpha1.NetworkScope{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(scope), latest); err != nil {
			return err
		}
		previous := map[inventory.TargetKey]awsv1alpha1.TargetStatus{}
		for _, t := range latest.Status.Targets {
			previous[inventory.TargetKey{Account: t.Account, Region: t.Region}] = t
		}

		var failed, busy []string
		throttled = map[inventory.TargetKey]bool{}
		targets = make([]awsv1alpha1.TargetStatus, 0, len(all))
		for _, t := range all {
			key := t.Key()
			ts := awsv1alpha1.TargetStatus{Account: t.Account, Region: t.Region}
			prev := previous[key]
			res, synced := byKey[key]
			switch {
			case !synced:
				ts = prev
				ts.Account, ts.Region = t.Account, t.Region
			case res.err != nil:
				// Keep the last good numbers and time so a flapping account does not look empty.
				ts.VPCs, ts.Subnets, ts.LastSyncTime = prev.VPCs, prev.Subnets, prev.LastSyncTime
				// And the last known unmanaged resources: they are what a later recovery is
				// compared against, and losing them here would report all of them as new.
				ts.UnmanagedIDs = prev.UnmanagedIDs
				ts.Error = res.err.Error()
			default:
				ts.VPCs, ts.Subnets = count32(len(res.snapshot.VPCs)), count32(len(res.snapshot.Subnets))
				ts.UnmanagedVPCs = count32(len(res.snapshot.UnmanagedVPCs))
				ts.UnmanagedSubnets = count32(len(res.snapshot.UnmanagedSubnets))
				ts.UnmanagedIDs = unmanagedIDs(res.snapshot)
				ts.LastSyncTime = &now
			}
			switch {
			case ts.Error == "":
			case isThrottled(synced, res, ts, r.backoff.backedOff(scope.Name, key)):
				throttled[key] = true
				busy = append(busy, key.String())
			default:
				failed = append(failed, key.String())
			}
			targets = append(targets, ts)
		}

		cond := metav1.Condition{Type: ConditionReady, Status: metav1.ConditionTrue, Reason: "Synced",
			Message: fmt.Sprintf("%d account/region targets synced", len(all)), ObservedGeneration: latest.Generation}
		switch {
		case len(failed) > 0:
			// An unreachable target is the problem worth naming first; throttled ones recover
			// on their own.
			cond.Status, cond.Reason = metav1.ConditionFalse, "SyncFailed"
			cond.Message = fmt.Sprintf("%d of %d targets failed: %s", len(failed), len(all), strings.Join(failed, ", "))
			if len(busy) > 0 {
				cond.Message += fmt.Sprintf("; %d throttled: %s", len(busy), strings.Join(busy, ", "))
			}
		case len(busy) > 0:
			cond.Status, cond.Reason = metav1.ConditionFalse, "Throttled"
			cond.Message = fmt.Sprintf("%d of %d targets throttled, retried with backoff: %s",
				len(busy), len(all), strings.Join(busy, ", "))
		}
		meta.SetStatusCondition(&latest.Status.Conditions, cond)
		if full {
			latest.Status.ObservedGeneration = latest.Generation
			latest.Status.LastSyncTime = &now
		}
		latest.Status.VPCs, latest.Status.Subnets = count32(vpcs), count32(subnets)
		unmanaged := int32(0)
		for _, t := range targets {
			unmanaged += t.UnmanagedVPCs + t.UnmanagedSubnets
		}
		latest.Status.Unmanaged = unmanaged
		latest.Status.Targets = targets
		return r.Status().Update(ctx, latest)
	})
	return targets, throttled, err
}

// isThrottled tells a throttled target from a failed one. A target discovered in this sync
// says so through its error; one carried over from an earlier sync through the backoff, or,
// after a restart has emptied the backoff, through the error text its last attempt recorded.
func isThrottled(synced bool, res targetResult, ts awsv1alpha1.TargetStatus, backedOff bool) bool {
	if synced {
		return errors.Is(res.err, inventory.ErrThrottled)
	}
	return backedOff || strings.HasPrefix(ts.Error, inventory.ErrThrottled.Error())
}

// SetupWithManager sets up the controller with the Manager. VPC and Subnet objects are not
// watched: they are outputs. Besides spec changes, scopes are synced on their resync interval
// and when NotifyChanged reports EC2 changes.
func (r *NetworkScopeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&awsv1alpha1.NetworkScope{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WatchesRawSource(source.Channel(r.changesChannel(), &handler.EnqueueRequestForObject{})).
		Named("networkscope").
		Complete(r)
}
