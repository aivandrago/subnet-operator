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
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/smithy-go"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// throttledError is what the AWS discoverer returns once the SDK has given up retrying
// RequestLimitExceeded.
func throttledError() error {
	return fmt.Errorf("%w: describe Networks: %w", inventory.ErrThrottled,
		&smithy.GenericAPIError{Code: "RequestLimitExceeded", Message: "Request limit exceeded."})
}

// throttlingDiscoverer throttles the next n discoveries of an account/region, then lets
// them through to the fake, the way a busy account eventually quietens down.
type throttlingDiscoverer struct {
	*fakeDiscoverer
	mu        sync.Mutex
	remaining map[string]int
}

func (d *throttlingDiscoverer) Discover(ctx context.Context, t inventory.Target) (*inventory.Snapshot, error) {
	key := t.Account + "/" + t.Region
	d.mu.Lock()
	throttle := d.remaining[key] > 0
	if throttle {
		d.remaining[key]--
	}
	d.mu.Unlock()
	if throttle {
		d.fakeDiscoverer.mu.Lock()
		d.calls[key]++
		d.fakeDiscoverer.mu.Unlock()
		return nil, throttledError()
	}
	return d.fakeDiscoverer.Discover(ctx, t)
}

// targetGauge reads one per-target gauge of the scope, or -1 when the series does not exist.
func targetGauge(metric, scope, account string) float64 {
	GinkgoHelper()
	families, err := ctrlmetrics.Registry.Gather()
	Expect(err).NotTo(HaveOccurred())
	for _, f := range families {
		if f.GetName() != metric {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["provider"] == "aws" && labels["scope"] == scope && labels["account"] == account && labels["region"] == region {
				return m.GetGauge().GetValue()
			}
		}
	}
	return -1
}

var _ = Describe("NetworkScope Controller under API throttling", func() {
	var (
		scopeName  string
		discoverer *fakeDiscoverer
		throttling *throttlingDiscoverer
		reconciler *NetworkScopeReconciler
		clk        *clocktesting.FakeClock
	)

	const resync = 5 * time.Minute

	// Time is moved with the fake clock rather than by moving status.lastSyncTime back: the
	// scope's lastSyncTime and the backoff both follow it, so every reconcile below discovers
	// exactly what the schedule says it should, and one that returns early does so for the
	// reason under test.
	reconcileAt := func(d time.Duration) time.Duration {
		GinkgoHelper()
		clk.Step(d)
		res, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: scopeName}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))
		return res.RequeueAfter
	}

	getScope := func() *networkv1.NetworkScope {
		GinkgoHelper()
		s := &networkv1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, s)).To(Succeed())
		return s
	}

	calls := func(account string) int {
		discoverer.mu.Lock()
		defer discoverer.mu.Unlock()
		return discoverer.calls[account+"/"+region]
	}

	BeforeEach(func() {
		scopeCounter++
		scopeName = fmt.Sprintf("throttle-%d", scopeCounter)
		discoverer = &fakeDiscoverer{
			snapshots: map[string]*inventory.Snapshot{accountA + "/" + region: snapshotA(), accountB + "/" + region: snapshotB()},
			errs:      map[string]error{},
			calls:     map[string]int{},
		}
		throttling = &throttlingDiscoverer{fakeDiscoverer: discoverer, remaining: map[string]int{}}
		clk = clocktesting.NewFakeClock(time.Now().Truncate(time.Second))
		reconciler = &NetworkScopeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
			Providers: awsProviders(throttling, nil, nil), Clock: clk}
		// The top of each delay, so the schedule below is exact.
		reconciler.backoff.jitter = func() float64 { return 0.999999 }

		Expect(k8sClient.Create(ctx, &networkv1.NetworkScope{
			ObjectMeta: metav1.ObjectMeta{Name: scopeName},
			Spec: networkv1.NetworkScopeSpec{
				Provider:          networkv1.ProviderAWS,
				NamespaceSelector: &metav1.LabelSelector{},
				Accounts:          []networkv1.Account{{ID: accountA}, {ID: accountB}},
				Regions:           []string{region},
				ResyncInterval:    &metav1.Duration{Duration: resync},
			},
		})).To(Succeed())
	})

	AfterEach(func() {
		Expect(k8sClient.DeleteAllOf(ctx, &networkv1.Subnet{}, client.MatchingLabels{networkv1.LabelScope: scopeName})).To(Succeed())
		Expect(k8sClient.DeleteAllOf(ctx, &networkv1.Network{}, client.MatchingLabels{networkv1.LabelScope: scopeName})).To(Succeed())
		Expect(k8sClient.Delete(ctx, getScope())).To(Succeed())
	})

	It("keeps a throttled target's inventory, backs it off on its own schedule and recovers on its own", func() {
		Expect(reconcileAt(0)).To(Equal(resync))
		firstSync := getScope().Status.Targets[1].LastSyncTime

		By("account B throttling through three attempts at the next full sync")
		throttling.mu.Lock()
		throttling.remaining[accountB+"/"+region] = 3
		throttling.mu.Unlock()
		requeue := reconcileAt(resync) // t = 5m, full sync

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: "vpc-bbb"}, &networkv1.Network{})).To(Succeed(),
			"a throttled target keeps its objects")
		scope := getScope()
		b := scope.Status.Targets[1]
		Expect(b.Error).To(HavePrefix(inventory.ErrThrottled.Error()))
		Expect(b.Networks).To(Equal(int32(1)), "the last known numbers stay")
		Expect(b.LastSyncTime.Equal(firstSync)).To(BeTrue(), "and so does the time they were last true")
		cond := meta.FindStatusCondition(scope.Status.Conditions, ConditionReady)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("Throttled"))
		Expect(cond.Message).To(ContainSubstring(accountB))

		Expect(targetGauge("hs_target_up", scopeName, accountB)).To(Equal(1.0),
			"throttled is reachable: SubnetInventoryTargetDown must not fire")
		Expect(targetGauge("hs_target_throttled", scopeName, accountB)).To(Equal(1.0))
		Expect(targetGauge("hs_target_throttled", scopeName, accountA)).To(Equal(0.0))
		Expect(targetGauge("hs_target_up", scopeName, accountA)).To(Equal(1.0))

		By("scheduling the retry of B for the end of its first delay, not the next full sync")
		Expect(requeue).To(BeNumerically("~", throttleBackoffBase, time.Second))

		By("leaving B alone while it waits, even when an event names it")
		Expect(reconciler.NotifyChanged(ctx, []inventory.TargetKey{{Account: accountB, Region: region}})).To(Succeed())
		Expect(reconcileAt(10 * time.Second)).To(BeNumerically("~", throttleBackoffBase-10*time.Second, time.Second))
		Expect(calls(accountB)).To(Equal(2))

		By("retrying B alone once the delay has run out, and doubling the delay when it is throttled again")
		Expect(calls(accountA)).To(Equal(2))
		requeue = reconcileAt(throttleBackoffBase - 10*time.Second) // t = 6m
		Expect(calls(accountB)).To(Equal(3))
		Expect(calls(accountA)).To(Equal(2), "the healthy target keeps the scope's cadence")
		Expect(requeue).To(BeNumerically("~", 2*throttleBackoffBase, time.Second))

		By("throttled a third time: now waiting four minutes, past the next full sync")
		requeue = reconcileAt(2 * throttleBackoffBase) // t = 8m
		Expect(calls(accountB)).To(Equal(4))
		Expect(requeue).To(Equal(2*time.Minute), "the full sync at 10m comes first")

		By("the full sync leaves B out: it is still waiting")
		requeue = reconcileAt(2 * time.Minute) // t = 10m
		Expect(calls(accountA)).To(Equal(3))
		Expect(calls(accountB)).To(Equal(4))
		Expect(requeue).To(BeNumerically("~", 2*time.Minute, time.Second), "B is due at 12m")
		Expect(targetGauge("hs_target_throttled", scopeName, accountB)).To(Equal(1.0))

		By("B getting through at 12m: current again, and back on the normal cadence")
		requeue = reconcileAt(2 * time.Minute) // t = 12m
		Expect(calls(accountB)).To(Equal(5))
		scope = getScope()
		b = scope.Status.Targets[1]
		Expect(b.Error).To(BeEmpty())
		Expect(b.LastSyncTime.Time).To(BeTemporally("~", clk.Now(), time.Second))
		Expect(meta.IsStatusConditionTrue(scope.Status.Conditions, ConditionReady)).To(BeTrue())
		Expect(targetGauge("hs_target_throttled", scopeName, accountB)).To(Equal(0.0))
		Expect(targetGauge("hs_target_up", scopeName, accountB)).To(Equal(1.0))
		Expect(requeue).To(Equal(3*time.Minute), "no backoff left: the next reconcile is the full sync at 15m")

		By("the backoff starting over: throttled again, B waits the first delay, not the doubled one")
		throttling.mu.Lock()
		throttling.remaining[accountB+"/"+region] = 1
		throttling.mu.Unlock()
		requeue = reconcileAt(3 * time.Minute) // t = 15m, full sync
		Expect(calls(accountB)).To(Equal(6))
		Expect(requeue).To(BeNumerically("~", throttleBackoffBase, time.Second))
	})

	It("reports an unreachable target as down, not throttled, and does not back it off", func() {
		reconcileAt(0)
		discoverer.mu.Lock()
		discoverer.errs[accountB+"/"+region] = errors.New("AccessDenied")
		discoverer.mu.Unlock()

		Expect(reconcileAt(resync)).To(Equal(resync), "nothing to retry early")
		cond := meta.FindStatusCondition(getScope().Status.Conditions, ConditionReady)
		Expect(cond.Reason).To(Equal("SyncFailed"))
		Expect(targetGauge("hs_target_up", scopeName, accountB)).To(Equal(0.0))
		Expect(targetGauge("hs_target_throttled", scopeName, accountB)).To(Equal(0.0))

		reconcileAt(resync)
		Expect(calls(accountB)).To(Equal(3), "an unreachable target is tried at every full sync")
	})
})

// countingDiscoverer holds each discovery for a moment and records how many ran at once.
type countingDiscoverer struct {
	inFlight, peak atomic.Int32
}

func (d *countingDiscoverer) Discover(context.Context, inventory.Target) (*inventory.Snapshot, error) {
	n := d.inFlight.Add(1)
	defer d.inFlight.Add(-1)
	for {
		p := d.peak.Load()
		if n <= p || d.peak.CompareAndSwap(p, n) {
			break
		}
	}
	time.Sleep(50 * time.Millisecond)
	return &inventory.Snapshot{}, nil
}

var _ = Describe("NetworkScope discovery concurrency", func() {
	It("caps discoveries across scopes, not per scope", func() {
		counting := &countingDiscoverer{}
		reconciler := &NetworkScopeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(),
			Providers: awsProviders(counting, nil, nil), Concurrency: 2}

		names := make([]string, 0, 3)
		for i := range 3 {
			scopeCounter++
			name := fmt.Sprintf("concurrency-%d", scopeCounter)
			names = append(names, name)
			accounts := make([]networkv1.Account, 0, 4)
			for j := range 4 {
				accounts = append(accounts, networkv1.Account{ID: fmt.Sprintf("3%02d%09d", i, j)})
			}
			scope := &networkv1.NetworkScope{
				ObjectMeta: metav1.ObjectMeta{Name: name},
				Spec: networkv1.NetworkScopeSpec{
					Provider: networkv1.ProviderAWS, Accounts: accounts, Regions: []string{region}},
			}
			Expect(k8sClient.Create(ctx, scope)).To(Succeed())
			DeferCleanup(func() { Expect(k8sClient.Delete(ctx, scope)).To(Succeed()) })
		}

		// Reconciled at once, as they would be with MaxConcurrentReconciles above 1.
		var wg sync.WaitGroup
		for _, name := range names {
			wg.Go(func() {
				defer GinkgoRecover()
				_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: name}})
				Expect(err).NotTo(HaveOccurred())
			})
		}
		wg.Wait()

		Expect(counting.peak.Load()).To(Equal(int32(2)),
			"three scopes of four targets each, with a cap of 2 for the whole instance")
	})
})
