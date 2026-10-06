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
	gcpcloud "hypersurgery.dev/subnet-operator/internal/cloud/gcp"
	"hypersurgery.dev/subnet-operator/internal/cloud/gcp/gcpfake"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

// GCP change events end to end, like the AWS ones: an audit log entry published to the
// subscription (pstest) is read by the provider's event source, which tells the scope
// controller; the next reconcile discovers only the target the entry was about.
var _ = Describe("NetworkScope Controller with GCP change events", func() {
	const (
		topic        = "projects/ops-central/topics/subnet-operator-events"
		subscription = "projects/ops-central/subscriptions/subnet-operator-events"
		compute      = "compute.googleapis.com"
	)
	var (
		scopeName  string
		cloud      *gcpfake.Cloud
		pubsub     *gcpfake.PubSub
		reconciler *NetworkScopeReconciler
	)

	reconcileScope := func() {
		GinkgoHelper()
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: scopeName}})
		Expect(err).NotTo(HaveOccurred())
	}
	// subnetworkListsOf returns the regions whose subnetworks were listed since the last call.
	subnetworkListsOf := func() []string {
		var regions []string
		for _, r := range cloud.Requests() {
			if _, rest, ok := strings.Cut(r, "/regions/"); ok && strings.Contains(rest, "/subnetworks") {
				region, _, _ := strings.Cut(rest, "/")
				regions = append(regions, region)
			}
		}
		return regions
	}
	// awaitNotification waits until the event source has told the controller about a change.
	awaitNotification := func() {
		GinkgoHelper()
		Eventually(reconciler.changesChannel()).WithTimeout(15 * time.Second).Should(Receive())
	}

	BeforeEach(func() {
		scopeCounter++
		scopeName = fmt.Sprintf("gcp-events-%d", scopeCounter)
		cloud = gcpfake.New()
		DeferCleanup(cloud.Close)
		cloud.AddProject(gcpProject, "987654321098")
		cloud.AddNetwork(gcpProject, "shared")
		cloud.AddSubnetwork(gcpProject, gcpEurope, "shared", "apps", "10.10.0.0/24")
		cloud.AddSubnetwork(gcpProject, gcpUS, "shared", "batch", "10.20.0.0/24")

		var err error
		pubsub, err = gcpfake.NewPubSub()
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(pubsub.Close)
		Expect(pubsub.CreateTopic(ctx, topic)).To(Succeed())
		Expect(pubsub.CreateSubscription(ctx, subscription, topic)).To(Succeed())

		p := gcpcloud.NewProvider(gcpcloud.Options{ClientOptions: cloud.ClientOptions(),
			ComputeEndpoint: cloud.ComputeEndpoint(), ResourceManagerEndpoint: cloud.ResourceManagerEndpoint,
			IAMCredentialsEndpoint: cloud.IAMCredentialsEndpoint(),
			EventsSubscription:     subscription, EventsDebounce: 100 * time.Millisecond,
			PubSubClientOptions: pubsub.ClientOptions(), EventsLog: logr.Discard()})
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
				Provider: networkv1.ProviderGCP,
				Accounts: []networkv1.Account{{ID: gcpProject}},
				Regions:  []string{gcpEurope, gcpUS},
				GCP:      &networkv1.GCPScope{TagParent: "organizations/" + gcpOrg},
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
		Expect(subnetworkListsOf()).To(ConsistOf(gcpEurope, gcpUS), "the first sync is a full one")
		s := &networkv1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, s)).To(Succeed())
		Expect(s.Status.Capabilities).To(ContainElement(networkv1.CapabilityChangeEvents))
	})

	It("syncs the region of a created subnetwork within seconds, and only that region", func() {
		payments := cloud.AddSubnetwork(gcpProject, gcpUS, "shared", "payments", "10.20.4.0/24")
		id := "projects/" + gcpProject + "/regions/" + gcpUS + "/subnetworks/" + payments.Name
		msg := pubsub.Publish(topic, gcpfake.AuditLogEntry(gcpProject, compute, "v1.compute.subnetworks.insert", id,
			"maria.k@example.com"))
		awaitNotification()
		reconcileScope()

		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: networkv1.ObjectName(id)}, &networkv1.Subnet{})).To(Succeed())
		Expect(subnetworkListsOf()).To(ConsistOf(gcpUS), "the other region was not reported changed")
		Eventually(func() bool { return pubsub.Acked(msg) }).Should(BeTrue())
	})

	It("syncs every region of the project when its global network changes", func() {
		pubsub.Publish(topic, gcpfake.AuditLogEntry(gcpProject, compute, "v1.compute.networks.patch",
			"projects/"+gcpProject+"/global/networks/shared", "ops@example.com"))
		awaitNotification()
		reconcileScope()
		Expect(subnetworkListsOf()).To(ConsistOf(gcpEurope, gcpUS))
	})

	It("ignores entries about other projects and other resources, and survives garbage", func() {
		pubsub.Publish(topic, []byte("not a log entry"))
		pubsub.Publish(topic, gcpfake.AuditLogEntry(gcpProject, compute, "v1.compute.instances.insert",
			"projects/"+gcpProject+"/zones/"+gcpUS+"-a/instances/vm-1", "ops@example.com"))
		pubsub.Publish(topic, gcpfake.AuditLogEntry("shop-other-9", compute, "v1.compute.subnetworks.insert",
			"projects/shop-other-9/regions/"+gcpUS+"/subnetworks/x", "ops@example.com"))
		Consistently(reconciler.changesChannel()).WithTimeout(time.Second).ShouldNot(Receive())

		// The source still works after all that.
		pubsub.Publish(topic, gcpfake.AuditLogEntry(gcpProject, compute, "v1.compute.subnetworks.delete",
			"projects/"+gcpProject+"/regions/"+gcpEurope+"/subnetworks/apps", "ops@example.com"))
		awaitNotification()
	})
})
