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
	"fmt"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kevents "k8s.io/client-go/tools/events"
	clocktesting "k8s.io/utils/clock/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	azurecloud "hypersurgery.dev/subnet-operator/internal/cloud/azure"
	"hypersurgery.dev/subnet-operator/internal/cloud/azure/azurefake"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

// azureTargetGauge reads one per-target gauge of an Azure scope, or -1 when the series does
// not exist.
func azureTargetGauge(metric, scope, location string) float64 {
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
			if labels["provider"] == "azure" && labels["scope"] == scope && labels["region"] == location {
				return m.GetGauge().GetValue()
			}
		}
	}
	return -1
}

// A subscription in several locations is listed once per sync (#118), and a location that does
// not exist is told from one that is empty (#119): the real Azure provider against the in-repo
// fake, driven by the NetworkScope controller, whose targets start side by side.
var _ = Describe("NetworkScope Controller with Azure locations", func() {
	const (
		resync        = 5 * time.Minute
		swedenCentral = "swedencentral"
		ukSouth       = "uksouth"
	)
	var (
		scopeName  string
		cloud      *azurefake.Cloud
		reconciler *NetworkScopeReconciler
		recorder   *kevents.FakeRecorder
		clk        *clocktesting.FakeClock
		// jitter is what the throttle backoff draws next, one value per throttled target.
		jitter []float64
	)
	locations := []string{azureWestEurope, azureNorthEurope, swedenCentral, ukSouth}
	networkID := func(name string) string {
		return strings.ToLower("/subscriptions/" + azureSubscription + "/resourceGroups/" + azureGroup +
			"/providers/Microsoft.Network/virtualNetworks/" + name)
	}

	// Time moves with the fake clock, so each reconcile below discovers exactly what the
	// schedule says: a full sync once the interval has passed, the targets an event names or
	// whose backoff has run out before that, and nothing at all otherwise.
	reconcileAt := func(d time.Duration) {
		GinkgoHelper()
		clk.Step(d)
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: scopeName}})
		Expect(err).NotTo(HaveOccurred())
	}
	getScope := func() *networkv1.NetworkScope {
		GinkgoHelper()
		s := &networkv1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, s)).To(Succeed())
		return s
	}
	targetOf := func(location string) networkv1.TargetStatus {
		GinkgoHelper()
		for _, t := range getScope().Status.Targets {
			if t.Region == location {
				return t
			}
		}
		Fail("no target for " + location)
		return networkv1.TargetStatus{}
	}
	// targetEvents are the Events about targets since the last call.
	targetEvents := func(reason string) []string {
		var out []string
		for _, e := range emittedEvents(recorder) {
			if strings.HasPrefix(e, "Warning "+reason) {
				out = append(out, e)
			}
		}
		return out
	}
	exists := func(obj client.Object, id string) bool {
		GinkgoHelper()
		err := k8sClient.Get(ctx, types.NamespacedName{Name: networkv1.ObjectName(id)}, obj)
		if err != nil {
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%v", err)
		}
		return err == nil
	}

	BeforeEach(func() {
		scopeCounter++
		scopeName = fmt.Sprintf("azure-locations-%d", scopeCounter)
		cloud = azurefake.New()
		DeferCleanup(cloud.Close)
		cloud.AddSubscription(azureSubscription)
		cloud.AddVirtualNetwork(azureSubscription, azureGroup, "hub", azureWestEurope, []string{"10.10.0.0/16"}, nil)
		cloud.AddSubnet(azureSubscription, azureGroup, "hub", "apps", "10.10.1.0/24")
		cloud.AddVirtualNetwork(azureSubscription, azureGroup, "north", azureNorthEurope, []string{"10.20.0.0/16"}, nil)
		cloud.AddSubnet(azureSubscription, azureGroup, "north", "batch", "10.20.1.0/24")

		p := azurecloud.NewProvider(azurecloud.Options{Credential: cloud.Credential(),
			ResourceManagerEndpoint: cloud.Endpoint(), Transport: cloud.Transport(),
			// A throttled call is retried at once: the pacing has tests of its own.
			Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() }})
		recorder = kevents.NewFakeRecorder(64)
		clk = clocktesting.NewFakeClock(time.Now().Truncate(time.Second))
		reconciler = &NetworkScopeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			Providers: provider.MustRegistry(p), Recorder: recorder, Clock: clk}
		jitter = nil
		reconciler.backoff.jitter = func() float64 {
			if len(jitter) == 0 {
				return 0.999999
			}
			j := jitter[0]
			jitter = jitter[1:]
			return j
		}

		scope := &networkv1.NetworkScope{
			ObjectMeta: metav1.ObjectMeta{Name: scopeName},
			Spec: networkv1.NetworkScopeSpec{
				Provider:          networkv1.ProviderAzure,
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "net"}},
				Accounts:          []networkv1.Account{{ID: azureSubscription}},
				Regions:           locations,
				ResyncInterval:    &metav1.Duration{Duration: resync},
			},
		}
		Expect(k8sClient.Create(ctx, scope)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, scope))).To(Succeed())
			Expect(k8sClient.DeleteAllOf(ctx, &networkv1.Subnet{}, client.MatchingLabels{networkv1.LabelScope: scopeName})).To(Succeed())
			Expect(k8sClient.DeleteAllOf(ctx, &networkv1.Network{}, client.MatchingLabels{networkv1.LabelScope: scopeName})).To(Succeed())
		})
	})

	It("lists the subscription once for its four locations, and again for every later sync", func() {
		reconcileAt(0)
		calls := cloud.Calls()
		Expect(calls[azurefake.ListAll]).To(Equal(1), "one listing for four locations")
		Expect(calls[azurefake.ListUsage]).To(Equal(2), "usages of the two virtual networks, each by its own location")
		Expect(calls[azurefake.ListLocations]).To(Equal(1), "the two empty locations are looked up with one call")

		scope := getScope()
		Expect(meta.IsStatusConditionTrue(scope.Status.Conditions, ConditionReady)).To(BeTrue(), "%v", scope.Status.Conditions)
		Expect(scope.Status.Targets).To(HaveLen(4))
		Expect(exists(&networkv1.Network{}, networkID("hub"))).To(BeTrue())
		Expect(exists(&networkv1.Network{}, networkID("north"))).To(BeTrue())

		By("an empty location that exists: synced, with nothing in it and nothing said")
		empty := targetOf(swedenCentral)
		Expect(empty.Error).To(BeEmpty())
		Expect(empty.Networks).To(Equal(int32(0)))
		Expect(empty.LastSyncTime).NotTo(BeNil(), "status.targets shows it synced, with networks: 0")
		Expect(azureTargetGauge("hs_target_up", scopeName, swedenCentral)).To(Equal(1.0))
		Expect(targetEvents("Target")).To(BeEmpty(), "no Event about a location that is only empty")

		By("a reconcile inside the interval with no event pending: no discovery, no call")
		reconcileAt(10 * time.Second)
		Expect(cloud.Calls()).To(BeEmpty())

		By("the resync of the location an event names: a listing of its own, which sees the change")
		cloud.AddSubnet(azureSubscription, azureGroup, "north", "payments", "10.20.2.0/24")
		Expect(reconciler.NotifyChanged(ctx, []inventory.TargetKey{{Account: azureSubscription, Region: azureNorthEurope}})).
			To(Succeed())
		reconcileAt(10 * time.Second)
		calls = cloud.Calls()
		Expect(calls[azurefake.ListAll]).To(Equal(1), "a targeted resync is not answered from the last sync")
		Expect(calls[azurefake.ListUsage]).To(Equal(1), "only the location the event names")
		Expect(exists(&networkv1.Subnet{}, networkID("north")+"/subnets/payments")).To(BeTrue())

		By("the next full sync: one listing again, with what changed since")
		cloud.AddVirtualNetwork(azureSubscription, azureGroup, "sweden", swedenCentral, []string{"10.30.0.0/16"}, nil)
		cloud.RemoveVirtualNetwork(azureSubscription, azureGroup, "hub")
		reconcileAt(resync)
		calls = cloud.Calls()
		Expect(calls[azurefake.ListAll]).To(Equal(1), "one listing for the four locations of the next sync")
		Expect(exists(&networkv1.Network{}, networkID("sweden"))).To(BeTrue())
		Expect(exists(&networkv1.Network{}, networkID("hub"))).To(BeFalse(), "the next sync is not answered from the last one")
		Expect(targetOf(swedenCentral).Networks).To(Equal(int32(1)))
	})

	It("reports a throttled listing on every location, and lists again for each retry", func() {
		reconcileAt(0)
		cloud.Calls()
		emittedEvents(recorder)

		By("the listing throttled through every attempt of the next full sync")
		cloud.FailEndpoint(azurefake.ListAll, azurefake.Throttle)
		// westeurope is due first and alone; the others together, later.
		jitter = []float64{0, 0.9, 0.9, 0.9}
		reconcileAt(resync)
		Expect(cloud.Calls()[azurefake.ListAll]).To(Equal(5), "the five attempts of one call, not of one per location")
		scope := getScope()
		cond := meta.FindStatusCondition(scope.Status.Conditions, ConditionReady)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("Throttled"))
		for _, location := range locations {
			t := targetOf(location)
			Expect(t.Error).To(HavePrefix(inventory.ErrThrottled.Error()), location)
			Expect(t.Error).To(ContainSubstring("SubscriptionRequestsThrottled"), location)
			Expect(cond.Message).To(ContainSubstring(azureSubscription + "/" + location))
			Expect(azureTargetGauge("hs_target_throttled", scopeName, location)).To(Equal(1.0), location)
			Expect(azureTargetGauge("hs_target_up", scopeName, location)).To(Equal(1.0),
				"%s is throttled, not down: SubnetInventoryTargetDown must not fire", location)
		}
		Expect(targetEvents(EventTargetThrottled)).To(HaveLen(4), "one TargetThrottled Event per location")
		Expect(exists(&networkv1.Network{}, networkID("hub"))).To(BeTrue(), "a throttled target keeps its inventory")
		Expect(targetOf(azureWestEurope).Networks).To(Equal(int32(1)))

		By("one location retried alone after its backoff: its own listing, not the failed one")
		cloud.FailEndpoint(azurefake.ListAll, azurefake.None)
		cloud.AddSubnet(azureSubscription, azureGroup, "hub", "data", "10.10.2.0/24")
		reconcileAt(throttleBackoffBase/2 + time.Second)
		Expect(cloud.Calls()[azurefake.ListAll]).To(Equal(1))
		Expect(targetOf(azureWestEurope).Error).To(BeEmpty())
		Expect(targetOf(azureWestEurope).Subnets).To(Equal(int32(2)))
		Expect(azureTargetGauge("hs_target_throttled", scopeName, azureWestEurope)).To(Equal(0.0))
		Expect(targetOf(azureNorthEurope).Error).To(HavePrefix(inventory.ErrThrottled.Error()), "still waiting")
		Expect(azureTargetGauge("hs_target_throttled", scopeName, azureNorthEurope)).To(Equal(1.0))

		By("the other three retried together: one listing for the three")
		reconcileAt(throttleBackoffBase / 2)
		Expect(cloud.Calls()[azurefake.ListAll]).To(Equal(1))
		scope = getScope()
		Expect(meta.IsStatusConditionTrue(scope.Status.Conditions, ConditionReady)).To(BeTrue(), "%v", scope.Status.Conditions)
		for _, location := range locations {
			Expect(targetOf(location).Error).To(BeEmpty(), location)
			Expect(azureTargetGauge("hs_target_throttled", scopeName, location)).To(Equal(0.0), location)
		}
	})

	It("reports a listing that fails on every location, and recovers with the next sync", func() {
		reconcileAt(0)
		cloud.Calls()
		emittedEvents(recorder)

		cloud.FailEndpoint(azurefake.ListAll, azurefake.Deny)
		reconcileAt(resync)
		Expect(cloud.Calls()[azurefake.ListAll]).To(Equal(1), "one refused call, not one per location")
		scope := getScope()
		cond := meta.FindStatusCondition(scope.Status.Conditions, ConditionReady)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("SyncFailed"))
		for _, location := range locations {
			Expect(targetOf(location).Error).To(ContainSubstring("AuthorizationFailed"), location)
			Expect(azureTargetGauge("hs_target_up", scopeName, location)).To(Equal(0.0),
				"%s is down: SubnetInventoryTargetDown fires", location)
			Expect(azureTargetGauge("hs_target_throttled", scopeName, location)).To(Equal(0.0), location)
		}
		Expect(targetEvents(EventTargetUnreachable)).To(HaveLen(4), "one TargetUnreachable Event per location")
		Expect(exists(&networkv1.Network{}, networkID("hub"))).To(BeTrue(), "an unreadable subscription is not an empty one")

		cloud.FailEndpoint(azurefake.ListAll, azurefake.None)
		reconcileAt(resync)
		Expect(cloud.Calls()[azurefake.ListAll]).To(Equal(1))
		Expect(meta.IsStatusConditionTrue(getScope().Status.Conditions, ConditionReady)).To(BeTrue())
		for _, location := range locations {
			Expect(azureTargetGauge("hs_target_up", scopeName, location)).To(Equal(1.0), location)
		}
	})

	It("fails the target of a location that does not exist, naming the one it could be", func() {
		scope := getScope()
		scope.Spec.Regions = []string{azureWestEurope, "westeuropa", swedenCentral}
		Expect(k8sClient.Update(ctx, scope)).To(Succeed())
		reconcileAt(0)

		want := `location "westeuropa" does not exist in subscription ` + azureSubscription + `: did you mean westeurope?`
		Expect(targetOf("westeuropa").Error).To(Equal(want))
		Expect(targetOf("westeuropa").LastSyncTime).To(BeNil(), "it never synced")
		scope = getScope()
		cond := meta.FindStatusCondition(scope.Status.Conditions, ConditionReady)
		Expect(cond.Status).To(Equal(metav1.ConditionFalse))
		Expect(cond.Reason).To(Equal("SyncFailed"))
		Expect(cond.Message).To(Equal("1 of 3 targets failed: " + azureSubscription + "/westeuropa"))
		Expect(azureTargetGauge("hs_target_up", scopeName, "westeuropa")).To(Equal(0.0), "SubnetInventoryTargetDown fires")
		unreachable := targetEvents(EventTargetUnreachable)
		Expect(unreachable).To(HaveLen(1))
		Expect(unreachable[0]).To(ContainSubstring("in westeuropa: " + want))

		By("the locations that exist are synced next to it, the empty one quietly")
		Expect(targetOf(azureWestEurope).Error).To(BeEmpty())
		Expect(targetOf(azureWestEurope).Networks).To(Equal(int32(1)))
		Expect(targetOf(swedenCentral).Error).To(BeEmpty())
		Expect(azureTargetGauge("hs_target_up", scopeName, azureWestEurope)).To(Equal(1.0))
		Expect(azureTargetGauge("hs_target_up", scopeName, swedenCentral)).To(Equal(1.0))
		Expect(exists(&networkv1.Network{}, networkID("hub"))).To(BeTrue())

		By("the name corrected: the scope is Ready")
		scope.Spec.Regions = []string{azureWestEurope, swedenCentral}
		Expect(k8sClient.Update(ctx, scope)).To(Succeed())
		reconcileAt(time.Minute)
		Expect(meta.IsStatusConditionTrue(getScope().Status.Conditions, ConditionReady)).To(BeTrue())
	})

	It("only warns when the identity may not read the subscription's locations", func() {
		cloud.FailEndpoint(azurefake.ListLocations, azurefake.Deny)
		scope := getScope()
		scope.Spec.Regions = []string{azureWestEurope, "westeuropa", swedenCentral}
		Expect(k8sClient.Update(ctx, scope)).To(Succeed())
		reconcileAt(0)

		scope = getScope()
		Expect(meta.IsStatusConditionTrue(scope.Status.Conditions, ConditionReady)).To(BeTrue(), "%v", scope.Status.Conditions)
		for _, t := range scope.Status.Targets {
			Expect(t.Error).To(BeEmpty(), t.Region)
			Expect(t.LastSyncTime).NotTo(BeNil(), t.Region)
			Expect(azureTargetGauge("hs_target_up", scopeName, t.Region)).To(Equal(1.0), t.Region)
		}
		events := emittedEvents(recorder)
		var warnings []string
		for _, e := range events {
			Expect(e).NotTo(HavePrefix("Warning "+EventTargetUnreachable), "a refused locations call fails no target")
			if strings.HasPrefix(e, "Warning "+EventTargetWarning+" ") {
				warnings = append(warnings, e)
			}
		}
		Expect(warnings).To(HaveLen(1), "one Event for the subscription, not one per empty location: %v", events)
		Expect(warnings[0]).To(ContainSubstring("Account " + azureSubscription + ": location names are not checked"))
		Expect(warnings[0]).To(ContainSubstring("HTTP 403 AuthorizationFailed"))
		Expect(warnings[0]).To(ContainSubstring("Microsoft.Resources/subscriptions/locations/read"))
		Expect(cloud.Calls()[azurefake.ListLocations]).To(Equal(1))
	})
})
