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

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	azurecloud "hypersurgery.dev/subnet-operator/internal/cloud/azure"
	"hypersurgery.dev/subnet-operator/internal/cloud/azure/azurefake"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

// Azure change events end to end, like the AWS and GCP ones: a resource event put in the
// Storage queue (azurefake) the way Event Grid delivers it is read by the provider's event
// source, which tells the scope controller; the next reconcile discovers only the target the
// event was about, although the event names no location.
var _ = Describe("NetworkScope Controller with Azure change events", func() {
	const (
		queue         = "subnet-operator-events"
		writeSuccess  = "Microsoft.Resources.ResourceWriteSuccess"
		deleteSuccess = "Microsoft.Resources.ResourceDeleteSuccess"
	)
	var (
		scopeName  string
		cloud      *azurefake.Cloud
		reconciler *NetworkScopeReconciler
	)
	// The IDs as ARM spells them; the inventory has them in lowercase.
	networks := "/subscriptions/" + azureSubscription + "/resourceGroups/" + azureGroup +
		"/providers/Microsoft.Network/virtualNetworks/"

	reconcileScope := func() {
		GinkgoHelper()
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: scopeName}})
		Expect(err).NotTo(HaveOccurred())
	}
	// usagesReadOf returns the virtual networks whose usages were read since the last call: a
	// sync reads them for the networks of its own location only.
	usagesReadOf := func() []string {
		var names []string
		for _, r := range cloud.Requests() {
			if before, _, ok := strings.Cut(r, "/usages"); ok && strings.HasPrefix(r, "GET ") {
				names = append(names, before[strings.LastIndex(before, "/")+1:])
			}
		}
		return names
	}
	awaitNotification := func() {
		GinkgoHelper()
		Eventually(reconciler.changesChannel()).WithTimeout(15 * time.Second).Should(Receive())
	}

	BeforeEach(func() {
		scopeCounter++
		scopeName = fmt.Sprintf("azure-events-%d", scopeCounter)
		cloud = azurefake.New()
		DeferCleanup(cloud.Close)
		cloud.AddSubscription(azureSubscription)
		cloud.AddVirtualNetwork(azureSubscription, azureGroup, "hub", azureWestEurope, []string{"10.0.0.0/16"}, nil)
		cloud.AddSubnet(azureSubscription, azureGroup, "hub", "apps", "10.0.0.0/24")
		cloud.AddVirtualNetwork(azureSubscription, azureGroup, "north", azureNorthEurope, []string{"10.1.0.0/16"}, nil)
		cloud.AddSubnet(azureSubscription, azureGroup, "north", "batch", "10.1.0.0/24")
		cloud.AddQueue(queue)

		p := azurecloud.NewProvider(azurecloud.Options{Credential: cloud.Credential(),
			ResourceManagerEndpoint: cloud.Endpoint(), Transport: cloud.Transport(),
			EventsQueueURL: cloud.QueueURL(queue), EventsDebounce: 100 * time.Millisecond,
			EventsPollInterval: 20 * time.Millisecond, EventsLog: logr.Discard()})
		reconciler = &NetworkScopeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			Providers: provider.MustRegistry(p)}
		source := p.Events(provider.EventSink{Changed: reconciler.NotifyChanged})
		Expect(source).NotTo(BeNil())
		sourceCtx, stop := context.WithCancel(ctx)
		DeferCleanup(stop)
		go func() { _ = source.Start(sourceCtx) }()

		scope := &networkv1.NetworkScope{
			ObjectMeta: metav1.ObjectMeta{Name: scopeName},
			Spec: networkv1.NetworkScopeSpec{
				Provider: networkv1.ProviderAzure,
				// In upper case, as a scope may spell it: events carry it in lowercase.
				Accounts: []networkv1.Account{{ID: strings.ToUpper(azureSubscription)}},
				Regions:  []string{azureWestEurope, azureNorthEurope},
				// Full syncs are rare, so only an event can explain a quick update.
				ResyncInterval: &metav1.Duration{Duration: time.Hour},
			},
		}
		Expect(k8sClient.Create(ctx, scope)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, scope))).To(Succeed())
			Expect(k8sClient.DeleteAllOf(ctx, &networkv1.Subnet{}, client.MatchingLabels{networkv1.LabelScope: scopeName})).To(Succeed())
			Expect(k8sClient.DeleteAllOf(ctx, &networkv1.Network{}, client.MatchingLabels{networkv1.LabelScope: scopeName})).To(Succeed())
		})

		reconcileScope()
		Expect(usagesReadOf()).To(ConsistOf("hub", "north"), "the first sync is a full one")
		s := &networkv1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, s)).To(Succeed())
		Expect(s.Status.Capabilities).To(ContainElement(networkv1.CapabilityChangeEvents))
	})

	It("syncs the location of a created subnet within seconds, and only that location", func() {
		cloud.AddSubnet(azureSubscription, azureGroup, "north", "payments", "10.1.4.0/24")
		id := networks + "north/subnets/payments"
		msg := cloud.Publish(queue, azurefake.ResourceEvent(writeSuccess, id))
		awaitNotification()
		reconcileScope()

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: networkv1.ObjectName(strings.ToLower(id))},
			&networkv1.Subnet{})).To(Succeed())
		Expect(usagesReadOf()).To(ConsistOf("north"), "the other location was not reported changed")
		Eventually(func() bool { queued, _ := cloud.Queued(queue, msg); return queued }).Should(BeFalse())
	})

	It("syncs every location of the subscription for a virtual network it has not seen", func() {
		cloud.AddVirtualNetwork(azureSubscription, azureGroup, "new", azureNorthEurope, []string{"10.2.0.0/16"}, nil)
		cloud.AddSubnet(azureSubscription, azureGroup, "new", "a", "10.2.0.0/24")
		cloud.Publish(queue, azurefake.ResourceEvent(writeSuccess, networks+"new"))
		awaitNotification()
		reconcileScope()
		Expect(usagesReadOf()).To(ConsistOf("hub", "north", "new"))
	})

	It("ignores events about other subscriptions and other resources, and survives garbage", func() {
		group := "/subscriptions/" + azureSubscription + "/resourceGroups/" + azureGroup
		cloud.Enqueue(queue, "not an event")
		cloud.Publish(queue, azurefake.ResourceEvent(writeSuccess, group+"/providers/Microsoft.Compute/virtualMachines/vm-1"))
		cloud.Publish(queue, azurefake.ResourceEvent(writeSuccess,
			strings.ReplaceAll(networks+"hub", azureSubscription, "99999999-0000-4000-8000-000000000000")))
		Consistently(reconciler.changesChannel()).WithTimeout(time.Second).ShouldNot(Receive())
		Eventually(func() int { return cloud.QueueLength(queue) }).Should(BeZero())

		// The source still works after all that.
		cloud.Publish(queue, azurefake.ResourceEvent(deleteSuccess, networks+"hub/subnets/apps"))
		awaitNotification()
	})
})
