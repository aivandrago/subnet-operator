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

package v1

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
)

func gcpScope(name, project, region string) *networkv1.NetworkScope {
	return &networkv1.NetworkScope{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: networkv1.NetworkScopeSpec{
			Provider:          networkv1.ProviderGCP,
			Accounts:          []networkv1.Account{{ID: project}},
			Regions:           []string{region},
			GCP:               &networkv1.GCPScope{TagParent: "organizations/123456789012"},
			NamespaceSelector: &metav1.LabelSelector{},
		},
	}
}

var _ = Describe("Webhooks for a GCP scope", func() {
	var created []client.Object

	AfterEach(func() {
		for _, obj := range created {
			Expect(client.IgnoreNotFound(k8sClient.Delete(ctx, obj))).To(Succeed())
		}
		created = nil
	})

	It("defaults the tag keys to the GCP ones", func() {
		obj := gcpScope("gcp-defaults", "gcp-defaults-1", "europe-west1")
		obj.Spec.AutoImport = &networkv1.AutoImportPolicy{Mode: networkv1.AutoImportOff}
		mustCreate(obj)
		created = append(created, obj)

		Expect(obj.Spec.TagKeys).To(Equal(networkv1.TagKeys{Owner: "hs-owner", Env: "hs-env", Tier: "hs-tier"}))
		Expect(obj.Spec.AutoImport.ManagedTag).To(Equal("hs-managed"))
	})

	It("keeps the AWS defaults for an AWS scope", func() {
		obj := scope("aws-defaults", "100000000099", "eu-central-1")
		obj.Spec.AutoImport = &networkv1.AutoImportPolicy{Mode: networkv1.AutoImportOff}
		mustCreate(obj)
		created = append(created, obj)

		Expect(obj.Spec.TagKeys).To(Equal(networkv1.TagKeys{Owner: "hs/owner", Env: "hs/env", Tier: "hs/tier"}))
		Expect(obj.Spec.AutoImport.ManagedTag).To(Equal("hs/managed"))
	})

	It("refuses an AWS region name", func() {
		expectDenied(gcpScope("gcp-aws-region", "gcp-aws-region-1", "eu-central-1"), "not a GCP region name")
	})

	It("accepts an auto-import policy that binds tags", func() {
		obj := gcpScope("gcp-auto-import", "gcp-auto-import-1", "europe-west1")
		obj.Spec.AutoImport = &networkv1.AutoImportPolicy{Mode: networkv1.AutoImportDryRun}
		mustCreate(obj)
		created = append(created, obj)
	})

	It("warns about a second scope over another region of the same project", func() {
		first := gcpScope("gcp-shared-a", "gcp-shared-1", "europe-west1")
		mustCreate(first)
		created = append(created, first)
		second := gcpScope("gcp-shared-b", "gcp-shared-1", "us-central1")
		warnings, err := (&NetworkScopeValidator{Client: k8sClient, Providers: testProviders}).ValidateCreate(ctx, second)
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).To(ContainElement(And(ContainSubstring("project gcp-shared-1"),
			ContainSubstring(`NetworkScope "gcp-shared-a"`))))
		// The same region stays refused, and says so once.
		same := gcpScope("gcp-shared-c", "gcp-shared-1", "europe-west1")
		warnings, err = (&NetworkScopeValidator{Client: k8sClient, Providers: testProviders}).ValidateCreate(ctx, same)
		Expect(err).To(HaveOccurred())
		Expect(warnings).NotTo(ContainElement(ContainSubstring("GCP networks are global")))
	})

	It("checks claims and imports against the GCP rules", func() {
		obj := gcpScope("gcp-writes", "gcp-writes-1", "europe-west1")
		mustCreate(obj)
		created = append(created, obj)

		networkID := "projects/gcp-writes-1/global/networks/vpc"
		network := &networkv1.Network{
			ObjectMeta: metav1.ObjectMeta{Name: networkv1.ObjectName(networkID)},
			Spec:       networkv1.NetworkSpec{Provider: networkv1.ProviderGCP, ID: networkID, Account: "gcp-writes-1"},
		}
		mustCreate(network)
		created = append(created, network)
		subnetID := "projects/gcp-writes-1/regions/europe-west1/subnetworks/apps"
		subnet := &networkv1.Subnet{
			ObjectMeta: metav1.ObjectMeta{Name: networkv1.ObjectName(subnetID),
				Labels: map[string]string{networkv1.LabelNetwork: network.Name}},
			Spec: networkv1.SubnetSpec{Provider: networkv1.ProviderGCP, ID: subnetID, NetworkID: networkID,
				Account: "gcp-writes-1", Region: "europe-west1"},
		}
		mustCreate(subnet)
		created = append(created, subnet)
		subnet.Status.CIDRBlock = "10.20.0.0/24"
		subnet.Status.SecondaryCIDRBlocks = []string{"10.20.1.0/24"}
		subnet.Status.Tags = map[string]string{"hs-owner": "payments"}
		Expect(k8sClient.Status().Update(ctx, subnet)).To(Succeed())

		claim := func(name string, mutate func(*networkv1.SubnetClaim)) *networkv1.SubnetClaim {
			c := &networkv1.SubnetClaim{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: networkv1.SubnetClaimSpec{ScopeRef: obj.Name, Account: "gcp-writes-1", Region: "europe-west1",
					NetworkID: networkID, PrefixLength: 24, Owner: "team-a",
					GCP: &networkv1.GCPClaimOptions{PoolCIDRs: []string{"10.20.0.0/23"}}},
			}
			if mutate != nil {
				mutate(c)
			}
			return c
		}
		// The pool's two /24s are the subnetwork's primary and secondary range: no room.
		expectDenied(claim("gcp-claim-full", nil), "has no room")
		expectDenied(claim("gcp-claim-zones", func(c *networkv1.SubnetClaim) {
			c.Spec.Zones = []string{"europe-west1-b"}
			c.Spec.GCP.PoolCIDRs = []string{"10.30.0.0/16"}
		}), "GCP subnetworks are regional")
		expectDenied(claim("gcp-claim-no-pool", func(c *networkv1.SubnetClaim) { c.Spec.GCP = nil }), "poolCIDRs")
		ok := claim("gcp-claim-ok", func(c *networkv1.SubnetClaim) { c.Spec.GCP.PoolCIDRs = []string{"10.30.0.0/16"} })
		mustCreate(ok)
		created = append(created, ok)
		// A created subnetwork has no zone to be dropped from the spec.
		withSubnet := ok.DeepCopy()
		withSubnet.Status.Allocations = []networkv1.SubnetAllocation{{Name: ok.Name, CIDRBlock: "10.30.0.0/24",
			SubnetID: "projects/gcp-writes-1/regions/europe-west1/subnetworks/" + ok.Name}}
		changed := withSubnet.DeepCopy()
		changed.Spec.Env = "prod"
		warnings, err := (&SubnetClaimValidator{Client: k8sClient, Providers: testProviders, WritesEnabled: true}).
			ValidateUpdate(ctx, withSubnet, changed)
		Expect(err).NotTo(HaveOccurred())
		Expect(warnings).NotTo(ContainElement(ContainSubstring("was dropped")))

		imp := func(name, id string, tags map[string]string) *networkv1.ResourceImport {
			return &networkv1.ResourceImport{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
				Spec: networkv1.ResourceImportSpec{ScopeRef: obj.Name, Account: "gcp-writes-1", Region: "europe-west1",
					ResourceID: id, Tags: tags},
			}
		}
		expectDenied(imp("gcp-import-replace", subnetID, map[string]string{"hs-owner": "billing"}),
			"never replaces a value")
		add := imp("gcp-import-add", subnetID, map[string]string{"hs-owner": "payments", "hs-env": "prod"})
		mustCreate(add)
		created = append(created, add)
		global := imp("gcp-import-network", networkID, map[string]string{"hs-owner": "platform"})
		global.Spec.Region = ""
		mustCreate(global)
		created = append(created, global)
	})
})
