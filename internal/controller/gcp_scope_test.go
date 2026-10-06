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
	"fmt"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	gcpcloud "hypersurgery.dev/subnet-operator/internal/cloud/gcp"
	"hypersurgery.dev/subnet-operator/internal/cloud/gcp/gcpfake"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

const (
	gcpProject = "shop-host-1"
	gcpOrg     = "123456789012"
	gcpEurope  = "europe-west1"
	gcpUS      = "us-central1"
)

func gcpOrgTag(key, value string) gcpfake.Binding {
	return gcpfake.Binding{Parent: "organizations/" + gcpOrg, Namespace: gcpOrg, Key: key, Value: value}
}

// gcpMetric returns the value of a gauge's series for one GCP subnet, and whether it exists.
func gcpMetric(metric, scope, subnetID string) (float64, bool) {
	GinkgoHelper()
	return gcpSeries(metric, scope, "subnet_id", subnetID)
}

// gcpSeries returns the value of a gauge's series for one GCP resource, named by a label.
func gcpSeries(metric, scope, label, value string) (float64, bool) {
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
			if labels["scope"] == scope && labels[label] == value && labels["provider"] == "gcp" {
				return m.GetGauge().GetValue(), true
			}
		}
	}
	return 0, false
}

// A GCP scope reconciles end to end with discovery only: the real GCP provider against the
// in-repo fake of Compute and Resource Manager, into Network and Subnet objects.
var _ = Describe("NetworkScope Controller with GCP", func() {
	var (
		scopeName  string
		cloud      *gcpfake.Cloud
		reconciler *NetworkScopeReconciler
	)

	networkID := "projects/" + gcpProject + "/global/networks/shared"
	appsID := "projects/" + gcpProject + "/regions/" + gcpEurope + "/subnetworks/apps"
	batchID := "projects/" + gcpProject + "/regions/" + gcpUS + "/subnetworks/batch"

	reconcileScope := func() {
		GinkgoHelper()
		_, err := reconciler.Reconcile(ctx, reconcile.Request{NamespacedName: types.NamespacedName{Name: scopeName}})
		Expect(err).NotTo(HaveOccurred())
	}

	// resync changes the spec, so the next reconcile is a full sync.
	resync := func(mutate func(*networkv1.NetworkScope)) {
		GinkgoHelper()
		scope := &networkv1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, scope)).To(Succeed())
		mutate(scope)
		Expect(k8sClient.Update(ctx, scope)).To(Succeed())
		reconcileScope()
	}

	getNetwork := func(id string) (*networkv1.Network, error) {
		n := &networkv1.Network{}
		return n, k8sClient.Get(ctx, types.NamespacedName{Name: networkv1.ObjectName(id)}, n)
	}
	getSubnet := func(id string) (*networkv1.Subnet, error) {
		s := &networkv1.Subnet{}
		return s, k8sClient.Get(ctx, types.NamespacedName{Name: networkv1.ObjectName(id)}, s)
	}

	BeforeEach(func() {
		scopeCounter++
		scopeName = fmt.Sprintf("gcp-scope-%d", scopeCounter)
		cloud = gcpfake.New()
		DeferCleanup(cloud.Close)
		cloud.AddProject(gcpProject, "987654321098")
		cloud.AddNetwork(gcpProject, "shared", gcpOrgTag("hs-owner", "platform"), gcpOrgTag("hs-env", "prod"))
		apps := cloud.AddSubnetwork(gcpProject, gcpEurope, "shared", "apps", "10.10.0.0/24",
			gcpOrgTag("hs-owner", "payments"), gcpOrgTag("hs-tier", "private"))
		cloud.AddSubnetwork(gcpProject, gcpUS, "shared", "batch", "10.20.0.0/24")
		cloud.Update(func() {
			apps.Used = 2
			apps.Secondary = []gcpfake.Range{{Name: "pods", CIDR: "10.64.0.0/20", Used: 16}}
		})

		p := gcpcloud.NewProvider(gcpcloud.Options{ClientOptions: cloud.ClientOptions(),
			ComputeEndpoint: cloud.ComputeEndpoint(), ResourceManagerEndpoint: cloud.ResourceManagerEndpoint,
			IAMCredentialsEndpoint: cloud.IAMCredentialsEndpoint()})
		reconciler = &NetworkScopeReconciler{Client: k8sClient, Scheme: k8sClient.Scheme(), APIReader: k8sClient,
			Providers: provider.MustRegistry(p)}

		scope := &networkv1.NetworkScope{
			ObjectMeta: metav1.ObjectMeta{Name: scopeName},
			Spec: networkv1.NetworkScopeSpec{
				Provider:           networkv1.ProviderGCP,
				Accounts:           []networkv1.Account{{ID: gcpProject}},
				Regions:            []string{gcpEurope, gcpUS},
				GCP:                &networkv1.GCPScope{TagParent: "organizations/" + gcpOrg},
				RequiredSubnetTags: []string{"hs-owner"},
			},
		}
		Expect(k8sClient.Create(ctx, scope)).To(Succeed())
		DeferCleanup(func() {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, scope))).To(Succeed())
			// No garbage collector in envtest: remove what the scope created.
			Expect(k8sClient.DeleteAllOf(ctx, &networkv1.Subnet{}, client.MatchingLabels{networkv1.LabelScope: scopeName})).To(Succeed())
			Expect(k8sClient.DeleteAllOf(ctx, &networkv1.Network{}, client.MatchingLabels{networkv1.LabelScope: scopeName})).To(Succeed())
		})
	})

	It("mirrors the project's global network once and the subnetworks of every region", func() {
		reconcileScope()

		n, err := getNetwork(networkID)
		Expect(err).NotTo(HaveOccurred())
		Expect(n.Name).To(MatchRegexp(`^shared-[0-9a-f]{10}$`))
		Expect(n.Spec).To(Equal(networkv1.NetworkSpec{Provider: networkv1.ProviderGCP, ID: networkID, Account: gcpProject}))
		Expect(n.Labels).To(HaveKeyWithValue(networkv1.LabelRegion, ""))
		Expect(n.Labels).To(HaveKeyWithValue(networkv1.LabelProvider, "gcp"))
		Expect(n.Status.Name).To(Equal("shared"))
		Expect(n.Status.Owner).To(Equal("platform"), "the default GCP owner key is hs-owner")
		Expect(n.Status.Env).To(Equal("prod"))
		Expect(n.Status.CIDRBlocks).To(BeEmpty())
		Expect(n.Status.GCP).NotTo(BeNil())
		Expect(n.Status.Subnets).To(Equal(int32(2)), "both regions' subnetworks count for the global network")
		Expect(n.Status.TotalIPs).To(HaveValue(Equal(int64(2 * 252))))
		Expect(n.Status.AvailableIPs).To(HaveValue(Equal(int64(250 + 252))))

		apps, err := getSubnet(appsID)
		Expect(err).NotTo(HaveOccurred())
		Expect(apps.Spec).To(Equal(networkv1.SubnetSpec{Provider: networkv1.ProviderGCP, ID: appsID, NetworkID: networkID,
			Account: gcpProject, Region: gcpEurope}))
		Expect(apps.Labels).To(HaveKeyWithValue(networkv1.LabelNetwork, n.Name))
		Expect(apps.Labels).To(HaveKeyWithValue(networkv1.LabelRegion, gcpEurope))
		Expect(apps.Status.Name).To(Equal("apps"))
		Expect(apps.Status.Zone).To(BeEmpty())
		Expect(apps.Status.Owner).To(Equal("payments"))
		Expect(apps.Status.Tier).To(Equal("private"))
		Expect(apps.Status.OwnershipSource).To(Equal(networkv1.OwnershipSourceSubnet))
		Expect(apps.Status.AvailableIPs).To(HaveValue(Equal(int64(250))))
		Expect(apps.Status.SecondaryCIDRBlocks).To(Equal([]string{"10.64.0.0/20"}))
		Expect(apps.Status.GCP.SecondaryRanges).To(HaveLen(1))
		Expect(apps.Status.GCP.SecondaryRanges[0].AvailableIPs).To(HaveValue(Equal(int64(4096 - 16))))
		Expect(apps.Status.MissingTags).To(BeEmpty())

		batch, err := getSubnet(batchID)
		Expect(err).NotTo(HaveOccurred())
		Expect(batch.Labels).To(HaveKeyWithValue(networkv1.LabelRegion, gcpUS))
		Expect(batch.Status.MissingTags).To(Equal([]string{"hs-owner"}))

		scope := &networkv1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, scope)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(scope.Status.Conditions, ConditionReady)).To(BeTrue(),
			"%v", scope.Status.Conditions)
		Expect(scope.Status.Networks).To(Equal(int32(1)))
		Expect(scope.Status.Subnets).To(Equal(int32(2)))
		Expect(scope.Status.Capabilities).To(Equal([]networkv1.Capability{networkv1.CapabilityCreateSubnet,
			networkv1.CapabilityIPUsage}))
		Expect(scope.Status.Ownership).To(Equal(&networkv1.Ownership{Networks: networkv1.OwnershipResourceTags,
			Subnets: networkv1.OwnershipResourceTags}))
		Expect(scope.Status.Targets).To(HaveLen(2))

		free, ok := gcpMetric("hs_subnet_available_ips", scopeName, appsID)
		Expect(ok).To(BeTrue(), "the subnet's series carries provider=gcp")
		Expect(free).To(Equal(250.0))
	})

	It("keeps the global network while its project is in the scope, and drops what the cloud lost", func() {
		reconcileScope()

		// A region leaves the scope: its subnetworks go, the network stays.
		resync(func(s *networkv1.NetworkScope) { s.Spec.Regions = []string{gcpEurope} })
		_, err := getSubnet(batchID)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the subnet of the removed region is gone: %v", err)
		n, err := getNetwork(networkID)
		Expect(err).NotTo(HaveOccurred())
		Expect(n.Status.Subnets).To(Equal(int32(1)))

		// The network is deleted in the cloud: its object and its subnets' go on the next sync.
		cloud.RemoveNetwork(gcpProject, "shared")
		resync(func(s *networkv1.NetworkScope) { s.Spec.ResyncInterval = &metav1.Duration{Duration: 2 * time.Minute} })
		_, err = getNetwork(networkID)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the deleted network is gone: %v", err)
		_, err = getSubnet(appsID)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "the deleted network's subnet is gone: %v", err)
	})

	It("reports networks whose subnetworks overlap, across projects, and whether they are peered", func() {
		const appsProject = "shop-apps-1"
		appsNetworkID := "projects/" + appsProject + "/global/networks/apps"
		sandboxID := "projects/" + gcpProject + "/global/networks/sandbox"
		cloud.AddProject(appsProject, "555555555555")
		cloud.AddNetwork(appsProject, "apps")
		// The pod range of shared's apps subnetwork holds web's primary range.
		cloud.AddSubnetwork(appsProject, gcpEurope, "apps", "web", "10.64.8.0/24")
		// apps peered with shared, which never peered back: the peering waits, and cannot
		// become active while the ranges overlap.
		cloud.AddPeering(appsProject, "apps", "to-shared", gcpProject, "shared")
		// A network nobody peered with, in the same project, overlapping batch in another region.
		cloud.AddNetwork(gcpProject, "sandbox")
		cloud.AddSubnetwork(gcpProject, gcpUS, "sandbox", "scratch", "10.20.0.0/25")
		resync(func(s *networkv1.NetworkScope) {
			s.Spec.Accounts = append(s.Spec.Accounts, networkv1.Account{ID: appsProject})
		})

		shared, err := getNetwork(networkID)
		Expect(err).NotTo(HaveOccurred())
		appsRef, sandboxRef := appsProject+"//"+appsNetworkID, gcpProject+"//"+sandboxID
		Expect(shared.Status.OverlapsWith).To(Equal([]string{appsRef, sandboxRef}))
		Expect(shared.Status.GCP.Peerings).To(BeEmpty())
		Expect(shared.Status.GCP.Overlaps).To(Equal([]networkv1.GCPNetworkOverlap{
			{Network: appsRef, Peered: true, PeeringState: "INACTIVE", RangeCount: 1,
				Ranges: []networkv1.GCPOverlappingRange{{CIDRBlock: "10.64.0.0/20", Subnet: appsID,
					OtherCIDRBlock: "10.64.8.0/24", OtherSubnet: "projects/" + appsProject + "/regions/" + gcpEurope + "/subnetworks/web"}}},
			{Network: sandboxRef, RangeCount: 1,
				Ranges: []networkv1.GCPOverlappingRange{{CIDRBlock: "10.20.0.0/24", Subnet: batchID,
					OtherCIDRBlock: "10.20.0.0/25", OtherSubnet: "projects/" + gcpProject + "/regions/" + gcpUS + "/subnetworks/scratch"}}},
		}))

		apps, err := getNetwork(appsNetworkID)
		Expect(err).NotTo(HaveOccurred())
		Expect(apps.Status.GCP.Peerings).To(Equal([]networkv1.GCPNetworkPeering{{Name: "to-shared",
			Network: networkID, State: "INACTIVE", StateDetails: "[2026-09-29T00:00:00.000-07:00]: Waiting for peer network to connect."}}))
		Expect(apps.Status.OverlapsWith).To(Equal([]string{gcpProject + "//" + networkID}))

		overlaps, ok := gcpSeries("hs_network_cidr_overlaps", scopeName, "network_id", networkID)
		Expect(ok).To(BeTrue())
		Expect(overlaps).To(Equal(2.0))
		peered, ok := gcpSeries("hs_network_peered_cidr_overlaps", scopeName, "network_id", networkID)
		Expect(ok).To(BeTrue())
		Expect(peered).To(Equal(1.0))
		peered, ok = gcpSeries("hs_network_peered_cidr_overlaps", scopeName, "network_id", sandboxID)
		Expect(ok).To(BeTrue())
		Expect(peered).To(Equal(0.0), "an overlap with a network it is not peered with")

		// The sandbox goes, and with it the overlap it had; the peered one stays.
		cloud.RemoveNetwork(gcpProject, "sandbox")
		resync(func(s *networkv1.NetworkScope) { s.Spec.ResyncInterval = &metav1.Duration{Duration: 2 * time.Minute} })
		shared, err = getNetwork(networkID)
		Expect(err).NotTo(HaveOccurred())
		Expect(shared.Status.OverlapsWith).To(Equal([]string{appsRef}))
		Expect(shared.Status.GCP.Overlaps).To(HaveLen(1))
		overlaps, _ = gcpSeries("hs_network_cidr_overlaps", scopeName, "network_id", networkID)
		Expect(overlaps).To(Equal(1.0))

		// A sync that changes nothing keeps the details: the discovery does not know them.
		resync(func(s *networkv1.NetworkScope) { s.Spec.ResyncInterval = &metav1.Duration{Duration: 3 * time.Minute} })
		again, err := getNetwork(networkID)
		Expect(err).NotTo(HaveOccurred())
		Expect(again.ResourceVersion).To(Equal(shared.ResourceVersion), "nothing changed, nothing is written")
	})

	It("keeps a global network while a target of its project could not be read", func() {
		reconcileScope()
		cloud.Fail(gcpfake.Deny)
		resync(func(s *networkv1.NetworkScope) { s.Spec.ResyncInterval = &metav1.Duration{Duration: 2 * time.Minute} })
		_, err := getNetwork(networkID)
		Expect(err).NotTo(HaveOccurred(), "an unreadable project is not an empty one")
		scope := &networkv1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, scope)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(scope.Status.Conditions, ConditionReady)).To(BeFalse())
	})

	It("reads a project as its service account, and names what to grant when it cannot", func() {
		const reader = "subnet-reader@shop-host-1.iam.gserviceaccount.com"
		cloud.AddServiceAccount(reader) // nobody may impersonate it yet
		cloud.RestrictProject(gcpProject, reader)
		resync(func(s *networkv1.NetworkScope) {
			s.Spec.Accounts[0].GCP = &networkv1.GCPAccount{ServiceAccount: reader}
		})

		scope := &networkv1.NetworkScope{}
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, scope)).To(Succeed())
		ready := meta.FindStatusCondition(scope.Status.Conditions, ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("SyncFailed"), "a refused impersonation is a failed target, not throttling")
		Expect(scope.Status.Targets).To(HaveLen(2))
		for _, t := range scope.Status.Targets {
			Expect(t.Error).To(ContainSubstring(reader))
			Expect(t.Error).To(ContainSubstring("roles/iam.serviceAccountTokenCreator"))
		}
		_, err := getSubnet(appsID)
		Expect(apierrors.IsNotFound(err)).To(BeTrue(), "nothing is read without the service account: %v", err)

		// Granted, the next sync reads the project as the service account.
		cloud.AddServiceAccount(reader, gcpfake.Operator)
		resync(func(s *networkv1.NetworkScope) { s.Spec.ResyncInterval = &metav1.Duration{Duration: 2 * time.Minute} })
		Expect(k8sClient.Get(ctx, types.NamespacedName{Name: scopeName}, scope)).To(Succeed())
		Expect(meta.IsStatusConditionTrue(scope.Status.Conditions, ConditionReady)).To(BeTrue(), "%v", scope.Status.Conditions)
		_, err = getSubnet(appsID)
		Expect(err).NotTo(HaveOccurred())
		Expect(cloud.RequestsBy(reader)).NotTo(BeEmpty())
	})

	It("refuses GCP scopes the schema can tell are wrong", func() {
		base := func() *networkv1.NetworkScope {
			return &networkv1.NetworkScope{ObjectMeta: metav1.ObjectMeta{Name: scopeName + "-bad"},
				Spec: networkv1.NetworkScopeSpec{Provider: networkv1.ProviderGCP, Accounts: []networkv1.Account{{ID: gcpProject}},
					Regions: []string{gcpEurope}, GCP: &networkv1.GCPScope{TagParent: "organizations/" + gcpOrg}}}
		}
		for what, mutate := range map[string]func(*networkv1.NetworkScope){
			"no gcp member":           func(s *networkv1.NetworkScope) { s.Spec.GCP = nil },
			"an AWS account ID":       func(s *networkv1.NetworkScope) { s.Spec.Accounts[0].ID = "111111111111" },
			"an aws member":           func(s *networkv1.NetworkScope) { s.Spec.AWS = &networkv1.AWSScope{} },
			"an account's aws member": func(s *networkv1.NetworkScope) { s.Spec.Accounts[0].AWS = &networkv1.AWSAccount{} },
			"a folder as tag parent":  func(s *networkv1.NetworkScope) { s.Spec.GCP.TagParent = "folders/1" },
			"a service account that is no email": func(s *networkv1.NetworkScope) {
				s.Spec.Accounts[0].GCP = &networkv1.GCPAccount{ServiceAccount: "subnet-reader"}
			},
			"a user as write service account": func(s *networkv1.NetworkScope) {
				s.Spec.Accounts[0].GCP = &networkv1.GCPAccount{WriteServiceAccount: "someone@example.com"}
			},
			"a claimTag that is neither Skip nor Bind": func(s *networkv1.NetworkScope) { s.Spec.GCP.ClaimTag = "Always" },
		} {
			s := base()
			mutate(s)
			err := k8sClient.Create(ctx, s)
			Expect(apierrors.IsInvalid(err)).To(BeTrue(), "%s: %v", what, err)
		}
		aws := base()
		aws.Spec.Provider, aws.Spec.Accounts[0].ID = networkv1.ProviderAWS, "111111111111"
		err := k8sClient.Create(ctx, aws)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "a gcp member on an AWS scope: %v", err)
		awsAccount := base()
		awsAccount.Spec.Provider, awsAccount.Spec.GCP = networkv1.ProviderAWS, nil
		awsAccount.Spec.Accounts[0] = networkv1.Account{ID: "111111111111",
			GCP: &networkv1.GCPAccount{ServiceAccount: "subnet-reader@shop-host-1.iam.gserviceaccount.com"}}
		err = k8sClient.Create(ctx, awsAccount)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "an account's gcp member on an AWS scope: %v", err)
		Expect(err.Error()).To(ContainSubstring("accounts[].gcp is only valid for provider GCP"))
		// claimTag lives in the gcp member, so it is refused on AWS with it.
		awsClaimTag := base()
		awsClaimTag.Spec.Provider, awsClaimTag.Spec.Accounts[0].ID = networkv1.ProviderAWS, "111111111111"
		awsClaimTag.Spec.GCP = &networkv1.GCPScope{TagParent: "organizations/" + gcpOrg, ClaimTag: networkv1.GCPClaimTagBind}
		err = k8sClient.Create(ctx, awsClaimTag)
		Expect(apierrors.IsInvalid(err)).To(BeTrue(), "claimTag on an AWS scope: %v", err)
		Expect(err.Error()).To(ContainSubstring("gcp is only valid for provider GCP"))

		good := base()
		good.Spec.Accounts[0].GCP = &networkv1.GCPAccount{
			ServiceAccount:      "subnet-reader@shop-host-1.iam.gserviceaccount.com",
			WriteServiceAccount: "subnet-writer@shop-host-1.iam.gserviceaccount.com"}
		good.Spec.GCP.ClaimTag = networkv1.GCPClaimTagBind
		Expect(k8sClient.Create(ctx, good)).To(Succeed(), "service accounts and claimTag Bind on a GCP scope are accepted")
		Expect(k8sClient.Delete(ctx, good)).To(Succeed())
	})
})
