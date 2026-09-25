//go:build e2e
// +build e2e

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

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
	"hypersurgery.dev/subnet-operator/internal/migration"
	"hypersurgery.dev/subnet-operator/test/utils"
)

const (
	// upgradeRelease is the Helm release name the install docs use, so the objects are named
	// the way they are in a user's cluster.
	upgradeRelease = "subnet-operator"
	// oldDeployment is the previous chart's fullname for that release: the chart was called
	// aws-subnet-operator, and the release name does not contain it.
	oldDeployment = upgradeRelease + "-aws-subnet-operator"
	// upgradeDeployment is the chart's fullname for that release since the rename: the release
	// name contains the chart name, so it is the release name alone.
	upgradeDeployment = upgradeRelease
	// upgradeScope is the NetworkScope the upgrade spec creates.
	upgradeScope = "upgrade"
	// publishedChart is where the last release with the old chart name and the old API group
	// is published, signed: the upgrade 0.8 has to survive. UPGRADE_CHART overrides it, e.g.
	// with a ChartMuseum repository added under another name. 0.9 moves this to the new name.
	publishedChart = "oci://ghcr.io/aivandrago/charts/aws-subnet-operator"
	// localChart is the chart of the build under test.
	localChart = "charts/subnet-operator"
	// leaseName is the leader election lease; only its holder reconciles and counts. It is the
	// same before and after the rename, which is what keeps an old and a new pod from both
	// leading during the rollout.
	leaseName = "1095b947.hypersurgery"
)

// The upgrade spec installs the latest published release with Helm, creates one of every kind
// of object, upgrades to the build under test the way the release notes tell users to (the new
// CRDs with kubectl first, then helm upgrade), and checks that nothing was lost and nothing was
// reported twice. It runs on its own, with `make test-upgrade`: it installs the operator with
// Helm into the namespace the Manager specs deploy to with kustomize, so the two cannot share a
// cluster.
//
// This upgrade crosses the rename of the chart (aws-subnet-operator to subnet-operator) and of
// the API group (aws.hypersurgery/v1alpha1 to network.hypersurgery.dev/v1beta1): the release is
// upgraded in place to the renamed chart, its objects get new names, and the operator migrates
// every object of the old group, status included.
var _ = Describe("Upgrade", Label("upgrade"), Ordered, func() {
	var (
		previous string // the published chart version installed first
		chartRef string
		fix      fixture
		// dryRunVPC is untagged and imported in dry-run mode, so it stays unmanaged: the known
		// resource that must not be counted again after the upgrade.
		dryRunVPC string
		before    oldSnapshot
		values    string
	)

	BeforeAll(func() {
		if os.Getenv("E2E_UPGRADE") != "true" {
			Skip("the upgrade spec runs with `make test-upgrade` (E2E_UPGRADE=true)")
		}

		chartRef = envOr("UPGRADE_CHART", publishedChart)
		previous = os.Getenv("UPGRADE_FROM")
		if previous == "" {
			By("finding the latest published chart")
			out, err := utils.Run(exec.Command(envOr("HELM", "helm"), "show", "chart", chartRef))
			Expect(err).NotTo(HaveOccurred(), "could not read the published chart; set UPGRADE_FROM to pick a version")
			m := regexp.MustCompile(`(?m)^version:\s*"?([^"\s]+)"?\s*$`).FindStringSubmatch(out)
			Expect(m).NotTo(BeNil(), "no version in the published chart:\n%s", out)
			previous = m[1]
		}
		_, _ = fmt.Fprintf(GinkgoWriter, "Upgrading from chart %s %s to the build under test\n", chartRef, previous)

		// Upgrading a release to itself proves nothing. That happens only when the checkout is
		// the release commit itself; a shallow CI checkout carries no tags and never skips.
		if tags, err := utils.Run(exec.Command("git", "tag", "--points-at", "HEAD")); err == nil &&
			slices.Contains(strings.Fields(tags), "v"+previous) {
			Skip(fmt.Sprintf("the previous release, %s, is the build under test (HEAD is tagged v%s); "+
				"set UPGRADE_FROM to an older version to test that upgrade instead", previous, previous))
		}

		By("creating the namespace with the restricted Pod Security profile")
		_, err := utils.Run(exec.Command("kubectl", "create", "ns", namespace))
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.Run(exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted"))
		Expect(err).NotTo(HaveOccurred())

		By("writing the values both releases are installed with")
		// Plain HTTP metrics, so the spec can read them through the API server's pod proxy
		// without a token of its own: the transport is not what an upgrade changes.
		queueURL := createEventsQueue(context.Background())
		values = filepath.Join(GinkgoT().TempDir(), "values.yaml")
		Expect(os.WriteFile(values, []byte(fmt.Sprintf(`
aws:
  region: %s
  endpointURL: %s
events:
  queueUrl: %s
writes:
  enabled: true
metrics:
  secure: false
extraEnv:
  - name: AWS_ACCESS_KEY_ID
    value: test
  - name: AWS_SECRET_ACCESS_KEY
    value: test
  - name: AWS_EC2_METADATA_DISABLED
    value: "true"
`, awsRegion, motoClusterEndpoint, queueURL)), 0o600)).To(Succeed())

		By(fmt.Sprintf("installing the published chart %s", previous))
		_, err = utils.Run(exec.Command(envOr("HELM", "helm"), "install", upgradeRelease, chartRef,
			"--version", previous, "-n", namespace, "-f", values))
		Expect(err).NotTo(HaveOccurred(), "helm install of the previous release failed")
		waitForRollout(oldDeployment)

		image, err := utils.Run(exec.Command("kubectl", "get", "deployment", oldDeployment, "-n", namespace,
			"-o", "jsonpath={.spec.template.spec.containers[0].image}"))
		Expect(err).NotTo(HaveOccurred())
		Expect(image).To(HaveSuffix(":"+previous), "the previous release runs its own published image")
	})

	AfterAll(func() {
		if os.Getenv("E2E_UPGRADE") != "true" {
			return
		}
		// Objects with finalizers go first, while the operator is still there to release them.
		for _, args := range [][]string{
			{"delete", resClaims + "," + resImports + "," + resOldClaims + "," + resOldImport,
				"--all", "-n", "default", "--timeout=1m"},
			{"delete", resScopes + "," + resExports + "," + resOldScopes, "--all", "--timeout=1m"},
		} {
			_, _ = utils.Run(exec.Command("kubectl", args...))
		}
		_, _ = utils.Run(exec.Command(envOr("HELM", "helm"), "uninstall", upgradeRelease, "-n", namespace))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "-f", localChart+"/crds/", "--ignore-not-found", "--wait=false"))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", namespace, "--wait=false"))
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpReleaseState()
		}
	})

	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("runs the previous release with one of every kind of object", func() {
		ctx := context.Background()

		By("seeding VPCs and subnets into Moto")
		fix = seedAWS(ctx)
		dryRunVPC = createVPC(ctx, hubEC2(ctx), "10.70.0.0/16", "Name", "dry-run")

		By("creating a NetworkScope, a SubnetClaim, ResourceImports and a SheetExport")
		// The webhook is served by the operator's pods, which may still be starting it.
		Eventually(func() error {
			return applyManifest(fmt.Sprintf(`
apiVersion: aws.hypersurgery/v1alpha1
kind: NetworkScope
metadata:
  name: %[1]s
spec:
  accounts:
    - id: "%[2]s"
    - id: "%[3]s"
      roleARN: %[4]s
  regions: [%[5]s]
  vpcTagSelector:
    hs/managed: "true"
  requiredSubnetTags: [hs/owner, hs/env, hs/tier]
  resyncInterval: 1m
---
apiVersion: aws.hypersurgery/v1alpha1
kind: SubnetClaim
metadata:
  name: payments
  namespace: default
spec:
  scopeRef: %[1]s
  account: "%[2]s"
  region: %[5]s
  vpcID: %[6]s
  prefixLength: 24
  availabilityZones: [%[5]sa, %[5]sb]
  owner: team-payments
  env: prod
  tier: private
---
apiVersion: aws.hypersurgery/v1alpha1
kind: ResourceImport
metadata:
  name: sandbox-vpc-import
  namespace: default
spec:
  scopeRef: %[1]s
  account: "%[2]s"
  region: %[5]s
  resourceID: %[7]s
  requestedBy: e2e
  tags:
    hs/managed: "true"
    hs/owner: team-sandbox
---
apiVersion: aws.hypersurgery/v1alpha1
kind: ResourceImport
metadata:
  name: sandbox-subnet-import
  namespace: default
spec:
  scopeRef: %[1]s
  account: "%[2]s"
  region: %[5]s
  resourceID: %[8]s
  requestedBy: e2e
  tags:
    hs/owner: team-sandbox
    hs/env: dev
    hs/tier: private
---
apiVersion: aws.hypersurgery/v1alpha1
kind: ResourceImport
metadata:
  name: dry-run-import
  namespace: default
spec:
  scopeRef: %[1]s
  account: "%[2]s"
  region: %[5]s
  resourceID: %[9]s
  requestedBy: e2e
  dryRun: true
  tags:
    hs/owner: team-sandbox
---
apiVersion: aws.hypersurgery/v1alpha1
kind: SheetExport
metadata:
  name: %[1]s
spec:
  scopeRef: %[1]s
  spreadsheetID: e2e-upgrade
  # The secret does not exist: there is no Google API here, so the export fails the same
  # way before and after the upgrade, and that is what is compared.
  credentialsSecretRef:
    name: google-sheets
    namespace: %[10]s
`, upgradeScope, hubAccount, spokeAccount, spokeRoleARN, awsRegion, fix.hubVPC,
				fix.unmanagedVPC, fix.unmanagedSubnet, dryRunVPC, namespace))
		}, time.Minute, 5*time.Second).Should(Succeed())

		By("waiting until every object has settled")
		Eventually(func(g Gomega) {
			s := takeOldSnapshot(g)

			g.Expect(oldReady(s.scope.Status.Conditions)).NotTo(BeNil())
			g.Expect(oldReady(s.scope.Status.Conditions).Status).To(Equal(metav1.ConditionTrue),
				oldReady(s.scope.Status.Conditions).Message)
			hub := oldTarget(&s.scope, hubAccount)
			g.Expect(hub).NotTo(BeNil())
			// The dry-run VPC stays unmanaged, so the known set is never empty.
			g.Expect(hub.UnmanagedIDs).To(ContainElement(dryRunVPC))

			claim := s.claims["payments"]
			g.Expect(oldReady(claim.Status.Conditions)).NotTo(BeNil())
			g.Expect(oldReady(claim.Status.Conditions).Status).To(Equal(metav1.ConditionTrue),
				oldReady(claim.Status.Conditions).Message)
			g.Expect(claim.Status.Allocations).To(HaveLen(2))
			for _, a := range claim.Status.Allocations {
				g.Expect(s.subnets).To(HaveKey(a.SubnetID), "the inventory has the claimed subnet")
			}

			for _, name := range []string{"sandbox-vpc-import", "sandbox-subnet-import"} {
				imp := s.imports[name]
				g.Expect(imp.Status.State).To(Equal(awsv1alpha1.ImportApplied), name+": "+imp.Status.Error)
				g.Expect(imp.Status.AppliedTime).NotTo(BeNil())
			}
			g.Expect(s.imports["dry-run-import"].Status.State).To(Equal(awsv1alpha1.ImportSkipped))

			g.Expect(s.vpcs).To(HaveKey(fix.unmanagedVPC), "the imported VPC is in the inventory")
			g.Expect(s.vpcs[fix.unmanagedVPC].Status.Owner).To(Equal("team-sandbox"))
			g.Expect(s.subnets).To(HaveKey(fix.unmanagedSubnet), "the imported subnet is in the inventory")
			g.Expect(s.subnets[fix.unmanagedSubnet].Status.Owner).To(Equal("team-sandbox"))

			exp := s.exports[upgradeScope]
			g.Expect(oldReady(exp.Status.Conditions)).NotTo(BeNil())
			g.Expect(oldReady(exp.Status.Conditions).Reason).To(Equal("ExportFailed"))

			before = s
		}).Should(Succeed())

		By("waiting for a full resync that changes nothing, so the snapshot is the steady state")
		// Imports, claims and the syncs they trigger land in any order; a snapshot taken between
		// two of them would be compared against a state the operator was never going to keep.
		Eventually(func(g Gomega) {
			s := takeOldSnapshot(g)
			g.Expect(s.scope.Status.LastSyncTime).NotTo(BeNil())
			g.Expect(s.scope.Status.LastSyncTime.After(before.scope.Status.LastSyncTime.Time)).To(BeTrue(),
				"no full resync since the snapshot yet")
			was := before.steadyState()
			before = s
			g.Expect(s.steadyState()).To(Equal(was), "the inventory was still changing")
		}, 4*time.Minute, 5*time.Second).Should(Succeed())
		_, _ = fmt.Fprintf(GinkgoWriter, "steady state before the upgrade:\n%s\n", before.steadyState())
	})

	It("upgrades to the renamed chart and the build under test: the new CRDs first, then helm upgrade", func() {
		By("applying the CRDs of the build under test, which adds the new group and deprecates the old one")
		_, err := utils.Run(exec.Command("kubectl", "apply", "-f", localChart+"/crds/"))
		Expect(err).NotTo(HaveOccurred())
		By("giving the nscope short name to the new group, as the upgrade guide says")
		_, err = utils.Run(exec.Command("kubectl", "replace", "-f", localChart+"/crds/aws.hypersurgery_networkscopes.yaml"))
		Expect(err).NotTo(HaveOccurred())

		By("upgrading the release to the renamed local chart and the image under test")
		image := strings.SplitN(managerImage, ":", 2)
		_, err = utils.Run(exec.Command(envOr("HELM", "helm"), "upgrade", upgradeRelease, localChart,
			"-n", namespace, "-f", values, "--set", "image.repository="+image[0], "--set", "image.tag="+image[1]))
		Expect(err).NotTo(HaveOccurred(), "helm upgrade to the build under test failed")
		waitForRollout(upgradeDeployment)

		By("finding the objects of the old chart gone: the release renamed them")
		Eventually(func(g Gomega) {
			out, err := utils.Run(exec.Command("kubectl", "get", "deployment,service,serviceaccount", "-n", namespace,
				"-l", "app.kubernetes.io/instance="+upgradeRelease, "-o", "name"))
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).NotTo(ContainSubstring(oldDeployment))
		}).Should(Succeed())
	})

	It("migrates every old object, and reports no known unmanaged resource as new", func() {
		leader, _ := newLeader()

		By("waiting until nothing is left to migrate")
		Eventually(func(g Gomega) {
			samples := scrape(g, leader)
			for _, kind := range []string{"NetworkScope", "SubnetClaim", "ResourceImport", "SheetExport"} {
				s := findMetric(samples, "hs_migration_pending_objects", map[string]string{"kind": kind})
				g.Expect(s).NotTo(BeNil(), "no hs_migration_pending_objects for %s yet", kind)
				g.Expect(s.value).To(BeZero(), "%s objects still waiting", kind)
			}
		}).Should(Succeed())

		By("waiting for the new leader's first sync to report every target")
		// The gauges are set for each target in the same pass that counts newly seen resources,
		// subnets last, so once all four exist that pass is over and the counter has its answer.
		var samples []metricSample
		Eventually(func(g Gomega) {
			samples = scrape(g, leader)
			for _, account := range []string{hubAccount, spokeAccount} {
				for _, kind := range []string{"vpc", "subnet"} {
					g.Expect(findSample(samples, "hs_aws_unmanaged_resources", account, kind)).NotTo(BeNil(),
						"no unmanaged gauge for %s/%s yet", account, kind)
				}
			}
		}).Should(Succeed())

		By("checking that the counter did not rise for a resource the previous release already knew")
		// The known unmanaged resources travel in the scope's status, which the migration copies
		// before the new scope's first sync.
		for _, s := range samples {
			if s.name == "hs_aws_unmanaged_resources_total" && s.labels["scope"] == upgradeScope {
				Expect(s.value).To(BeZero(), "counted again after the upgrade: %v", s.labels)
			}
		}
		Expect(findSample(samples, "hs_aws_unmanaged_resources", hubAccount, "vpc").value).To(BeNumerically(">=", 1),
			"the dry-run VPC is still unmanaged, so the check above had something to not count")
	})

	It("keeps every object and its status in the new group", func() {
		var after newSnapshot
		_, acquired := newLeader()
		By("waiting for a sync the new release started")
		// lastSyncTime is when the sync started, in whole seconds. One that started in a later
		// second than the lease was taken cannot be the previous release's.
		// Nothing changed in AWS, so the new release's full sync must arrive at the steady state
		// the previous one left: the same counts, the same known unmanaged resources, the same
		// networks and subnets.
		Eventually(func(g Gomega) {
			after = takeNewSnapshot(g)
			g.Expect(after.scope.Status.LastSyncTime).NotTo(BeNil())
			g.Expect(after.scope.Status.LastSyncTime.Time).To(BeTemporally(">", acquired.Truncate(time.Second)),
				"the scope has not synced since the new leader took over")
			g.Expect(readyCondition(&after.scope)).NotTo(BeNil())
			g.Expect(readyCondition(&after.scope).Status).To(Equal(metav1.ConditionTrue), readyCondition(&after.scope).Message)
			g.Expect(after.steadyState()).To(Equal(before.steadyState()))
		}).Should(Succeed())

		By("comparing the NetworkScope with the migration's own conversion of the old one")
		converted, _ := migration.NetworkScope(&before.scope, migration.ForCluster)
		Expect(subsetDiff("spec", converted.Spec, after.scope.Spec)).To(BeEmpty())
		Expect(after.scope.Spec.NamespaceSelector).NotTo(BeNil(), "unset in the old group, {} in the new one")
		Expect(after.scope.Annotations).To(HaveKeyWithValue(networkv1beta1.AnnotationMigratedFrom, migration.OldAPIVersion))
		for _, t := range before.scope.Status.Targets {
			now := targetStatus(&after.scope, t.Account)
			Expect(now).NotTo(BeNil(), "target %s is gone", t.Account)
			Expect(now.Error).To(BeEmpty())
			Expect(now.UnmanagedIDs).To(ConsistOf(t.UnmanagedIDs), "the known unmanaged resources of %s", t.Account)
		}

		By("comparing the networks and subnets with the VPCs and subnets the old release reported")
		Expect(mapKeys(after.networks)).To(ConsistOf(mapKeys(before.vpcs)))
		for id, v := range before.vpcs {
			n := after.networks[id]
			Expect(n.Spec.Provider).To(Equal(networkv1beta1.ProviderAWS))
			Expect(n.Spec.Account).To(Equal(v.Spec.Account))
			Expect(n.Status.CIDRBlocks).To(Equal(v.Status.CIDRBlocks), id)
			Expect(n.Status.Owner).To(Equal(v.Status.Owner), id)
			Expect(n.Status.Tags).To(Equal(v.Status.Tags), id)
			Expect(n.Status.Subnets).To(Equal(v.Status.Subnets), id)
			Expect(n.Status.OverlapsWith).To(Equal(v.Status.OverlapsWith), id)
		}
		Expect(mapKeys(after.subnets)).To(ConsistOf(mapKeys(before.subnets)))
		for id, s := range before.subnets {
			n := after.subnets[id]
			Expect(n.Spec.NetworkID).To(Equal(s.Spec.VPCID), id)
			Expect(n.Status.CIDRBlock).To(Equal(s.Status.CIDRBlock), id)
			Expect(n.Status.Zone).To(Equal(s.Status.AvailabilityZone), id)
			Expect(n.Status.AvailableIPs).To(HaveValue(Equal(s.Status.AvailableIPs)), id)
			Expect(n.Status.Owner).To(Equal(s.Status.Owner), id)
			Expect(n.Status.Tags).To(Equal(s.Status.Tags), id)
			Expect(n.Status.MissingTags).To(Equal(s.Status.MissingTags), id)
			Expect(n.Status.AWS).NotTo(BeNil())
			Expect(n.Status.AWS.Public).To(Equal(s.Status.Public), id)
		}

		By("finding the old VPC and Subnet objects gone with their migrated scope")
		Eventually(func(g Gomega) {
			var vpcs awsv1alpha1.VPCList
			g.Expect(getList(&vpcs, "vpcs.aws.hypersurgery")).To(Succeed())
			g.Expect(vpcs.Items).To(BeEmpty())
			var subnets awsv1alpha1.SubnetList
			g.Expect(getList(&subnets, "subnets.aws.hypersurgery")).To(Succeed())
			g.Expect(subnets.Items).To(BeEmpty())
		}).Should(Succeed())

		By("comparing the SubnetClaim: still Ready, with the same reservations and no second set in AWS")
		oldClaim := before.claims["payments"]
		Eventually(func(g Gomega) {
			claim := takeNewSnapshot(g).claims["payments"]
			g.Expect(claimReady(&claim)).NotTo(BeNil())
			g.Expect(claimReady(&claim).Status).To(Equal(metav1.ConditionTrue), claimReady(&claim).Message)
			g.Expect(claim.Status.Allocations).To(HaveLen(len(oldClaim.Status.Allocations)))
			for _, a := range oldClaim.Status.Allocations {
				g.Expect(claim.Status.Allocations).To(ContainElement(SatisfyAll(
					HaveField("Zone", a.AvailabilityZone), HaveField("CIDRBlock", a.CIDRBlock),
					HaveField("SubnetID", a.SubnetID))))
			}
			g.Expect(claim.Annotations[networkv1beta1.AnnotationCreatedBy]).
				To(Equal(oldClaim.Annotations[awsv1alpha1.AnnotationCreatedBy]), "the creator travels with the claim")
		}).Should(Succeed())
		Expect(describeSubnetsByTag(context.Background(), "hs/claim", "default/payments")).To(HaveLen(2))

		By("comparing the ResourceImports: applied once, not again")
		for name, imp := range before.imports {
			now := after.imports[name]
			Expect(now.Status.State).To(Equal(string(imp.Status.State)), name)
			Expect(now.Status.AppliedTags).To(Equal(imp.Status.AppliedTags), name)
			// An import applied again would carry a new time: the CreateTags call nobody asked for.
			Expect(now.Status.AppliedTime.Equal(imp.Status.AppliedTime)).To(BeTrue(),
				"%s was applied again: %v, before %v", name, now.Status.AppliedTime, imp.Status.AppliedTime)
			Expect(now.Annotations[networkv1beta1.AnnotationCreatedBy]).
				To(Equal(imp.Annotations[awsv1alpha1.AnnotationCreatedBy]), name)
			if imp.Status.State == awsv1alpha1.ImportApplied {
				Expect(importReady(&now)).NotTo(BeNil())
				Expect(importReady(&now).Status).To(Equal(metav1.ConditionTrue), name)
			}
		}
		Expect(tagsOf(context.Background(), dryRunVPC)).To(HaveKeyWithValue("Name", "dry-run"))
		Expect(tagsOf(context.Background(), dryRunVPC)).NotTo(HaveKey("hs/owner"), "the dry run stayed dry")

		By("comparing the SheetExport")
		exp := after.exports[upgradeScope]
		Expect(exp.Spec.SpreadsheetID).To(Equal(before.exports[upgradeScope].Spec.SpreadsheetID))
		Eventually(func(g Gomega) {
			exp := takeNewSnapshot(g).exports[upgradeScope]
			g.Expect(sheetReady(exp.Status.Conditions)).NotTo(BeNil())
			g.Expect(sheetReady(exp.Status.Conditions).Reason).To(Equal("ExportFailed"))
		}).Should(Succeed())

		By("finding every old object marked with the name of its copy")
		for _, args := range [][]string{
			{resOldScopes, upgradeScope},
			{"sheetexports.aws.hypersurgery", upgradeScope},
			{resOldClaims, "payments", "-n", "default"},
			{resOldImport, "sandbox-vpc-import", "-n", "default"},
			{resOldImport, "sandbox-subnet-import", "-n", "default"},
			{resOldImport, "dry-run-import", "-n", "default"},
		} {
			out, err := kubectlOut(append([]string{"get"},
				append(args, "-o", "jsonpath={.metadata.annotations.network\\.hypersurgery\\.dev/migrated-to}")...)...)
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal(args[1]), "%v", args)
		}
	})

	It("lets the webhooks of the new release accept every object, and refuse edits of migrated ones", func() {
		// A label change is an UPDATE of the whole object, which the validating and mutating
		// webhooks see like any other edit. An object the new release refuses would be stuck:
		// nobody could change it without deleting it first. That holds for the copies, and for
		// the old objects too, which GitOps tools keep applying until their manifests change.
		for _, args := range [][]string{
			{resScopes, upgradeScope},
			{resExports, upgradeScope},
			{resClaims, "payments", "-n", "default"},
			{resImports, "sandbox-vpc-import", "-n", "default"},
			{resImports, "sandbox-subnet-import", "-n", "default"},
			{resImports, "dry-run-import", "-n", "default"},
			{resOldScopes, upgradeScope},
			{resOldClaims, "payments", "-n", "default"},
			{resOldImport, "sandbox-vpc-import", "-n", "default"},
		} {
			_, err := utils.Run(exec.Command("kubectl", append([]string{"label", "--overwrite"},
				append(args, "e2e.hypersurgery.dev/upgraded=true")...)...))
			Expect(err).NotTo(HaveOccurred(), "the new release refused an update of %v", args)
		}

		By("refusing a spec change of a migrated object, naming the one to change")
		_, err := utils.Run(exec.Command("kubectl", "patch", resOldClaims, "payments", "-n", "default",
			"--type=merge", "-p", `{"spec":{"tier":"public"}}`))
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("network.hypersurgery.dev/v1beta1 SubnetClaim default/payments"))
	})

	It("keeps syncing, and still counts an unmanaged resource that is really new", func() {
		ctx := context.Background()
		leader, _ := newLeader()

		By("creating an untagged VPC and reporting it through SQS")
		created := createVPC(ctx, hubEC2(ctx), "10.60.0.0/16", "Name", "after-upgrade")
		sendChangeEvent(ctx, hubAccount, "CreateVpc")

		Eventually(func(g Gomega) {
			scope := &networkv1beta1.NetworkScope{}
			g.Expect(getObject(resScopes, upgradeScope, scope)).To(Succeed())
			hub := targetStatus(scope, hubAccount)
			g.Expect(hub).NotTo(BeNil())
			g.Expect(hub.UnmanagedIDs).To(ContainElement(created))
			g.Expect(scope.Status.Unmanaged).To(Equal(before.scope.Status.Unmanaged + 1))
		}).Should(Succeed())

		By("checking that it, and only it, was counted as new")
		Eventually(func(g Gomega) {
			samples := scrape(g, leader)
			vpcs := findSample(samples, "hs_aws_unmanaged_resources_total", hubAccount, "vpc")
			g.Expect(vpcs).NotTo(BeNil())
			g.Expect(vpcs.value).To(Equal(1.0))
			for _, s := range samples {
				if s.name != "hs_aws_unmanaged_resources_total" || s.labels["scope"] != upgradeScope ||
					(s.labels["account"] == hubAccount && s.labels["kind"] == "vpc") {
					continue
				}
				g.Expect(s.value).To(BeZero(), "counted: %v", s.labels)
			}
		}).Should(Succeed())
	})
})

// steadyTarget is what a full sync of unchanged AWS resources must reproduce for one target,
// the same in both groups.
type steadyTarget struct {
	Account, Region                     string
	Networks, Subnets                   int32
	UnmanagedNetworks, UnmanagedSubnets int32
	UnmanagedIDs                        []string
}

// steady is the scope's counts and known unmanaged resources per target, and which network
// and subnet objects exist, as text so that a failure shows both sides.
func steady(networks, subnets, unmanaged int32, targets []steadyTarget, networkObjects, subnetObjects []string) string {
	for i := range targets {
		slices.Sort(targets[i].UnmanagedIDs)
	}
	slices.SortFunc(targets, func(a, b steadyTarget) int {
		return strings.Compare(a.Account+a.Region, b.Account+b.Region)
	})
	slices.Sort(networkObjects)
	slices.Sort(subnetObjects)
	out, err := json.MarshalIndent(struct {
		Networks, Subnets, Unmanaged  int32
		Targets                       []steadyTarget
		NetworkObjects, SubnetObjects []string
	}{networks, subnets, unmanaged, targets, networkObjects, subnetObjects}, "", "  ")
	Expect(err).NotTo(HaveOccurred())
	return string(out)
}

// oldSnapshot is every aws.hypersurgery/v1alpha1 object the upgrade spec compares, by name.
type oldSnapshot struct {
	scope   awsv1alpha1.NetworkScope
	vpcs    map[string]awsv1alpha1.VPC
	subnets map[string]awsv1alpha1.Subnet
	claims  map[string]awsv1alpha1.SubnetClaim
	imports map[string]awsv1alpha1.ResourceImport
	exports map[string]awsv1alpha1.SheetExport
}

func takeOldSnapshot(g Gomega) oldSnapshot {
	s := oldSnapshot{
		vpcs: map[string]awsv1alpha1.VPC{}, subnets: map[string]awsv1alpha1.Subnet{},
		claims: map[string]awsv1alpha1.SubnetClaim{}, imports: map[string]awsv1alpha1.ResourceImport{},
		exports: map[string]awsv1alpha1.SheetExport{},
	}
	g.Expect(getObject(resOldScopes, upgradeScope, &s.scope)).To(Succeed())
	var vpcs awsv1alpha1.VPCList
	g.Expect(getList(&vpcs, "vpcs.aws.hypersurgery")).To(Succeed())
	for _, v := range vpcs.Items {
		s.vpcs[v.Name] = v
	}
	var subnets awsv1alpha1.SubnetList
	g.Expect(getList(&subnets, "subnets.aws.hypersurgery")).To(Succeed())
	for _, sn := range subnets.Items {
		s.subnets[sn.Name] = sn
	}
	var claims awsv1alpha1.SubnetClaimList
	g.Expect(getList(&claims, resOldClaims, "-n", "default")).To(Succeed())
	for _, c := range claims.Items {
		s.claims[c.Name] = c
	}
	var imports awsv1alpha1.ResourceImportList
	g.Expect(getList(&imports, resOldImport, "-n", "default")).To(Succeed())
	for _, i := range imports.Items {
		s.imports[i.Name] = i
	}
	var exports awsv1alpha1.SheetExportList
	g.Expect(getList(&exports, "sheetexports.aws.hypersurgery")).To(Succeed())
	for _, e := range exports.Items {
		s.exports[e.Name] = e
	}
	return s
}

func (s oldSnapshot) steadyState() string {
	var targets []steadyTarget
	for _, t := range s.scope.Status.Targets {
		targets = append(targets, steadyTarget{t.Account, t.Region, t.VPCs, t.Subnets,
			t.UnmanagedVPCs, t.UnmanagedSubnets, slices.Clone(t.UnmanagedIDs)})
	}
	return steady(s.scope.Status.VPCs, s.scope.Status.Subnets, s.scope.Status.Unmanaged, targets,
		mapKeys(s.vpcs), mapKeys(s.subnets))
}

// newSnapshot is every network.hypersurgery.dev object the upgrade spec compares, by name.
type newSnapshot struct {
	scope    networkv1beta1.NetworkScope
	networks map[string]networkv1beta1.Network
	subnets  map[string]networkv1beta1.Subnet
	claims   map[string]networkv1beta1.SubnetClaim
	imports  map[string]networkv1beta1.ResourceImport
	exports  map[string]networkv1beta1.SheetExport
}

func takeNewSnapshot(g Gomega) newSnapshot {
	s := newSnapshot{
		networks: map[string]networkv1beta1.Network{}, subnets: map[string]networkv1beta1.Subnet{},
		claims: map[string]networkv1beta1.SubnetClaim{}, imports: map[string]networkv1beta1.ResourceImport{},
		exports: map[string]networkv1beta1.SheetExport{},
	}
	g.Expect(getObject(resScopes, upgradeScope, &s.scope)).To(Succeed())
	var networks networkv1beta1.NetworkList
	g.Expect(getList(&networks, resNetworks)).To(Succeed())
	for _, n := range networks.Items {
		s.networks[n.Name] = n
	}
	var subnets networkv1beta1.SubnetList
	g.Expect(getList(&subnets, resSubnets)).To(Succeed())
	for _, sn := range subnets.Items {
		s.subnets[sn.Name] = sn
	}
	var claims networkv1beta1.SubnetClaimList
	g.Expect(getList(&claims, resClaims, "-n", "default")).To(Succeed())
	for _, c := range claims.Items {
		s.claims[c.Name] = c
	}
	var imports networkv1beta1.ResourceImportList
	g.Expect(getList(&imports, resImports, "-n", "default")).To(Succeed())
	for _, i := range imports.Items {
		s.imports[i.Name] = i
	}
	var exports networkv1beta1.SheetExportList
	g.Expect(getList(&exports, resExports)).To(Succeed())
	for _, e := range exports.Items {
		s.exports[e.Name] = e
	}
	return s
}

func (s newSnapshot) steadyState() string {
	var targets []steadyTarget
	for _, t := range s.scope.Status.Targets {
		targets = append(targets, steadyTarget{t.Account, t.Region, t.Networks, t.Subnets,
			t.UnmanagedNetworks, t.UnmanagedSubnets, slices.Clone(t.UnmanagedIDs)})
	}
	return steady(s.scope.Status.Networks, s.scope.Status.Subnets, s.scope.Status.Unmanaged, targets,
		mapKeys(s.networks), mapKeys(s.subnets))
}

func oldReady(conds []metav1.Condition) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == "Ready" {
			return &conds[i]
		}
	}
	return nil
}

func oldTarget(scope *awsv1alpha1.NetworkScope, account string) *awsv1alpha1.TargetStatus {
	for i := range scope.Status.Targets {
		if scope.Status.Targets[i].Account == account && scope.Status.Targets[i].Region == awsRegion {
			return &scope.Status.Targets[i]
		}
	}
	return nil
}

// findMetric returns the first sample of the metric whose labels include these, or nil.
func findMetric(samples []metricSample, name string, labels map[string]string) *metricSample {
	for i, s := range samples {
		if s.name != name {
			continue
		}
		match := true
		for k, v := range labels {
			match = match && s.labels[k] == v
		}
		if match {
			return &samples[i]
		}
	}
	return nil
}

func getList(list any, args ...string) error {
	out, err := kubectlOut(append(append([]string{"get"}, args...), "-o", "json")...)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(out), list)
}

func applyManifest(manifest string) error {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)
	return err
}

func waitForRollout(deployment string) {
	_, err := utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/"+deployment,
		"-n", namespace, "--timeout=4m"))
	if err != nil {
		dumpReleaseState()
	}
	Expect(err).NotTo(HaveOccurred(), "the operator did not roll out")
}

// newLeader waits for the lease to be held by a running pod of the image under test and returns
// that pod and when it took the lease: only the leader reconciles, so only its metrics say what
// the new release counted.
func newLeader() (string, time.Time) {
	var (
		leader   string
		acquired time.Time
	)
	Eventually(func(g Gomega) {
		out, err := utils.Run(exec.Command("kubectl", "get", "lease", leaseName, "-n", namespace,
			"-o", "jsonpath={.spec.holderIdentity} {.spec.acquireTime}"))
		g.Expect(err).NotTo(HaveOccurred())
		fields := strings.Fields(out)
		g.Expect(fields).To(HaveLen(2), "the lease is not held: %q", out)
		leader = strings.SplitN(fields[0], "_", 2)[0] // "<pod>_<uuid>"
		acquired, err = time.Parse(time.RFC3339Nano, fields[1])
		g.Expect(err).NotTo(HaveOccurred())
		out, err = utils.Run(exec.Command("kubectl", "get", "pod", leader, "-n", namespace,
			"-o", "jsonpath={.spec.containers[0].image} {.status.phase} {.metadata.deletionTimestamp}"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(out)).To(Equal(managerImage+" Running"), "the lease holder %s", leader)
	}).Should(Succeed())
	_, _ = fmt.Fprintf(GinkgoWriter, "the new release's leader is %s, since %s\n", leader, acquired)
	return leader, acquired
}

// metricSample is one line of the Prometheus text format.
type metricSample struct {
	name   string
	labels map[string]string
	value  float64
}

var (
	sampleLine = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)\{([^}]*)\} (\S+)`)
	labelPair  = regexp.MustCompile(`([a-zA-Z_][a-zA-Z0-9_]*)="((?:[^"\\]|\\.)*)"`)
)

// scrape reads the pod's metrics through the API server's pod proxy.
func scrape(g Gomega, pod string) []metricSample {
	out, err := utils.Run(exec.Command("kubectl", "get", "--raw",
		fmt.Sprintf("/api/v1/namespaces/%s/pods/%s:8443/proxy/metrics", namespace, pod)))
	g.Expect(err).NotTo(HaveOccurred())
	var samples []metricSample
	for line := range strings.SplitSeq(out, "\n") {
		m := sampleLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		v, err := strconv.ParseFloat(m[3], 64)
		g.Expect(err).NotTo(HaveOccurred(), line)
		s := metricSample{name: m[1], labels: map[string]string{}, value: v}
		for _, l := range labelPair.FindAllStringSubmatch(m[2], -1) {
			s.labels[l[1]] = l[2]
		}
		samples = append(samples, s)
	}
	return samples
}

// findSample returns the scope's sample of an unmanaged metric for the account and kind in the
// test region, or nil when the process never set it.
func findSample(samples []metricSample, name, account, kind string) *metricSample {
	for i, s := range samples {
		if s.name == name && s.labels["scope"] == upgradeScope && s.labels["account"] == account &&
			s.labels["region"] == awsRegion && s.labels["kind"] == kind {
			return &samples[i]
		}
	}
	return nil
}

// subsetDiff compares two values through their JSON form and describes the first thing that
// before had and after lost or changed. What after adds is allowed: a newer release may report
// more than an older one did, but never less or differently.
func subsetDiff(path string, before, after any) string {
	var b, a any
	for _, p := range []struct {
		in  any
		out *any
	}{{before, &b}, {after, &a}} {
		raw, err := json.Marshal(p.in)
		Expect(err).NotTo(HaveOccurred())
		Expect(json.Unmarshal(raw, p.out)).To(Succeed())
	}
	return jsonSubsetDiff(path, b, a)
}

func jsonSubsetDiff(path string, before, after any) string {
	switch b := before.(type) {
	case map[string]any:
		a, ok := after.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: was an object, now %v", path, after)
		}
		for k, v := range b {
			if d := jsonSubsetDiff(path+"."+k, v, a[k]); d != "" {
				return d
			}
		}
	case []any:
		a, ok := after.([]any)
		if !ok || len(a) != len(b) {
			return fmt.Sprintf("%s: was %v, now %v", path, before, after)
		}
		for i := range b {
			if d := jsonSubsetDiff(fmt.Sprintf("%s[%d]", path, i), b[i], a[i]); d != "" {
				return d
			}
		}
	default:
		if before != after {
			return fmt.Sprintf("%s: was %v, now %v", path, before, after)
		}
	}
	return ""
}

func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func sheetReady(conds []metav1.Condition) *metav1.Condition {
	return oldReady(conds)
}

// dumpReleaseState prints what the release's pods are doing, so a failure says why.
func dumpReleaseState() {
	selector := "app.kubernetes.io/instance=" + upgradeRelease
	for _, args := range [][]string{
		{"get", "pods", "-n", namespace, "-o", "wide"},
		{"describe", "deployment/" + upgradeDeployment, "-n", namespace},
		{"get", "events", "-n", namespace, "--sort-by=.lastTimestamp"},
		{"logs", "-n", namespace, "-l", selector, "--all-containers", "--tail=300", "--prefix"},
	} {
		out, err := utils.Run(exec.Command("kubectl", args...))
		_, _ = fmt.Fprintf(GinkgoWriter, "\n--- kubectl %s ---\n%s\nerr: %v\n", strings.Join(args, " "), out, err)
	}
}
