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

	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
	"hypersurgery.dev/subnet-operator/test/utils"
)

const (
	// upgradeRelease is the Helm release name the install docs use, so the objects are named
	// the way they are in a user's cluster.
	upgradeRelease = "subnet-operator"
	// upgradeDeployment is the chart's fullname for that release: the release name contains the
	// chart name, so it is the release name alone, before and after the upgrade.
	upgradeDeployment = upgradeRelease
	// upgradeScope is the NetworkScope the upgrade spec creates.
	upgradeScope = "upgrade"
	// publishedChart is where the previous release is published, signed. UPGRADE_CHART
	// overrides it, e.g. with a ChartMuseum repository added under another name.
	publishedChart = "oci://ghcr.io/aivandrago/charts/subnet-operator"
	// localChart is the chart of the build under test.
	localChart = "charts/subnet-operator"
	// leaseName is the leader election lease; only its holder reconciles and counts.
	leaseName = "1095b947.hypersurgery"
	// oldCRDs are the CRDs of aws.hypersurgery/v1alpha1, which 0.8 installed and served, and
	// which nothing deletes on an upgrade.
	oldCRDs = "networkscopes.aws.hypersurgery vpcs.aws.hypersurgery subnets.aws.hypersurgery " +
		"subnetclaims.aws.hypersurgery resourceimports.aws.hypersurgery sheetexports.aws.hypersurgery"
)

// The upgrade spec installs the latest published release with Helm, creates one of every kind
// of object, upgrades to the build under test the way the release notes tell users to (the new
// CRDs with kubectl first, then helm upgrade), and checks that nothing was lost and nothing was
// reported twice. It runs on its own, with `make test-upgrade`: it installs the operator with
// Helm into the namespace the Manager specs deploy to with kustomize, so the two cannot share a
// cluster.
//
// The previous release is 0.8, which served both aws.hypersurgery/v1alpha1 and
// network.hypersurgery.dev/v1beta1 and migrated the old objects into the new group. This one
// serves only the new group. So the objects are created the way a user coming from 0.7 has
// them, in the old group, and 0.8 migrates them before the upgrade: what must survive is what
// 0.8 left in the new group, status included. An upgrade from 0.7 straight to this release is
// not supported, and the guard that enforces it is checked at the end.
var _ = Describe("Upgrade", Label("upgrade"), Ordered, func() {
	var (
		previous string // the published chart version installed first
		chartRef string
		fix      fixture
		// dryRunVPC is untagged and imported in dry-run mode, so it stays unmanaged: the known
		// resource that must not be counted again after the upgrade.
		dryRunVPC string
		before    newSnapshot
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
		// without a token of its own: the transport is not what an upgrade changes. The AWS
		// settings use their 0.8 names, which this release still accepts (deprecated).
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
		waitForRollout(upgradeDeployment)

		image, err := utils.Run(exec.Command("kubectl", "get", "deployment", upgradeDeployment, "-n", namespace,
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
			{"delete", resClaims + "," + resImports, "--all", "-n", "default", "--timeout=1m"},
			{"delete", resScopes + "," + resExports, "--all", "--timeout=1m"},
		} {
			_, _ = utils.Run(exec.Command("kubectl", args...))
		}
		_, _ = utils.Run(exec.Command(envOr("HELM", "helm"), "uninstall", upgradeRelease, "-n", namespace))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "-f", localChart+"/crds/", "--ignore-not-found", "--wait=false"))
		_, _ = utils.Run(exec.Command("kubectl", append([]string{"delete", "crd", "--ignore-not-found", "--wait=false"},
			strings.Fields(oldCRDs)...)...))
		_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", namespace, "--wait=false"))
	})

	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			dumpReleaseState()
		}
	})

	SetDefaultEventuallyTimeout(3 * time.Minute)
	SetDefaultEventuallyPollingInterval(2 * time.Second)

	It("runs the previous release, which migrates old-group objects of every kind into the new group", func() {
		ctx := context.Background()

		By("seeding VPCs and subnets into Moto")
		fix = seedAWS(ctx)
		dryRunVPC = createVPC(ctx, hubEC2(ctx), "10.70.0.0/16", "Name", "dry-run")

		By("creating a NetworkScope, a SubnetClaim, ResourceImports and a SheetExport in aws.hypersurgery/v1alpha1")
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

		By("waiting until the previous release has migrated every one of them")
		Eventually(func(g Gomega) {
			samples := scrape(g, leaderRunning(g, ""))
			for _, kind := range []string{"NetworkScope", "SubnetClaim", "ResourceImport", "SheetExport"} {
				s := findMetric(samples, "hs_migration_pending_objects", map[string]string{"kind": kind})
				g.Expect(s).NotTo(BeNil(), "no hs_migration_pending_objects for %s yet", kind)
				g.Expect(s.value).To(BeZero(), "%s objects still waiting", kind)
			}
			for _, args := range [][]string{
				{resOldScopes, upgradeScope},
				{"sheetexports.aws.hypersurgery", upgradeScope},
				{resOldClaims, "payments", "-n", "default"},
				{resOldImport, "sandbox-vpc-import", "-n", "default"},
				{resOldImport, "sandbox-subnet-import", "-n", "default"},
				{resOldImport, "dry-run-import", "-n", "default"},
			} {
				g.Expect(migratedTo(args...)).To(Equal(args[1]), "%v", args)
			}
		}).Should(Succeed())

		By("waiting until every migrated object has settled in the new group")
		Eventually(func(g Gomega) {
			s := takeNewSnapshot(g)

			g.Expect(readyCondition(&s.scope)).NotTo(BeNil())
			g.Expect(readyCondition(&s.scope).Status).To(Equal(metav1.ConditionTrue), readyCondition(&s.scope).Message)
			hub := targetStatus(&s.scope, hubAccount)
			g.Expect(hub).NotTo(BeNil())
			// The dry-run VPC stays unmanaged, so the known set is never empty.
			g.Expect(hub.UnmanagedIDs).To(ContainElement(dryRunVPC))

			claim := s.claims["payments"]
			g.Expect(claimReady(&claim)).NotTo(BeNil())
			g.Expect(claimReady(&claim).Status).To(Equal(metav1.ConditionTrue), claimReady(&claim).Message)
			g.Expect(claim.Status.Allocations).To(HaveLen(2))
			for _, a := range claim.Status.Allocations {
				g.Expect(s.subnets).To(HaveKey(a.SubnetID), "the inventory has the claimed subnet")
			}
			g.Expect(claim.Annotations).To(HaveKey(networkv1beta1.AnnotationCreatedBy), "the creator travelled with the claim")

			for _, name := range []string{"sandbox-vpc-import", "sandbox-subnet-import"} {
				imp := s.imports[name]
				g.Expect(imp.Status.State).To(Equal(networkv1beta1.ImportApplied), name+": "+imp.Status.Error)
				g.Expect(imp.Status.AppliedTime).NotTo(BeNil())
			}
			g.Expect(s.imports["dry-run-import"].Status.State).To(Equal(networkv1beta1.ImportSkipped))

			g.Expect(s.networks).To(HaveKey(fix.unmanagedVPC), "the imported VPC is in the inventory")
			g.Expect(s.networks[fix.unmanagedVPC].Status.Owner).To(Equal("team-sandbox"))
			g.Expect(s.subnets).To(HaveKey(fix.unmanagedSubnet), "the imported subnet is in the inventory")
			g.Expect(s.subnets[fix.unmanagedSubnet].Status.Owner).To(Equal("team-sandbox"))

			exp := s.exports[upgradeScope]
			g.Expect(sheetReady(exp.Status.Conditions)).NotTo(BeNil())
			g.Expect(sheetReady(exp.Status.Conditions).Reason).To(Equal("ExportFailed"))

			before = s
		}).Should(Succeed())

		By("waiting for a full resync that changes nothing, so the snapshot is the steady state")
		// Imports, claims and the syncs they trigger land in any order; a snapshot taken between
		// two of them would be compared against a state the operator was never going to keep.
		Eventually(func(g Gomega) {
			s := takeNewSnapshot(g)
			g.Expect(s.scope.Status.LastSyncTime).NotTo(BeNil())
			g.Expect(s.scope.Status.LastSyncTime.After(before.scope.Status.LastSyncTime.Time)).To(BeTrue(),
				"no full resync since the snapshot yet")
			was := before.steadyState()
			before = s
			g.Expect(s.steadyState()).To(Equal(was), "the inventory was still changing")
		}, 4*time.Minute, 5*time.Second).Should(Succeed())
		_, _ = fmt.Fprintf(GinkgoWriter, "steady state before the upgrade:\n%s\n", before.steadyState())
	})

	It("upgrades to the build under test: the new CRDs first, then helm upgrade", func() {
		By("applying the CRDs of the build under test")
		_, err := utils.Run(exec.Command("kubectl", "apply", "-f", localChart+"/crds/"))
		Expect(err).NotTo(HaveOccurred())

		By("upgrading the release to the local chart and the image under test")
		image := strings.SplitN(managerImage, ":", 2)
		_, err = utils.Run(exec.Command(envOr("HELM", "helm"), "upgrade", upgradeRelease, localChart,
			"-n", namespace, "-f", values, "--set", "image.repository="+image[0], "--set", "image.tag="+image[1]))
		Expect(err).NotTo(HaveOccurred(), "helm upgrade to the build under test failed")
		waitForRollout(upgradeDeployment)

		By("finding the old group's CRDs still there, with the old objects: nothing deletes them but a person")
		for _, crd := range strings.Fields(oldCRDs) {
			_, err := utils.Run(exec.Command("kubectl", "get", "crd", crd))
			Expect(err).NotTo(HaveOccurred(), "%s is gone", crd)
		}
		Expect(migratedTo(resOldClaims, "payments", "-n", "default")).To(Equal("payments"))

		By("finding no webhook for the old group any more")
		for _, kind := range []string{"validatingwebhookconfiguration", "mutatingwebhookconfiguration"} {
			out, err := kubectlOut("get", kind, upgradeDeployment, "-o", "jsonpath={.webhooks[*].rules[*].apiGroups}")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("network.hypersurgery.dev"))
			Expect(out).NotTo(ContainSubstring("aws.hypersurgery"))
		}
	})

	It("keeps every object and its status, and reports no known unmanaged resource as new", func() {
		var after newSnapshot
		leader, acquired := newLeader()

		By("waiting for a sync the new release started")
		// lastSyncTime is when the sync started, in whole seconds. One that started in a later
		// second than the lease was taken cannot be the previous release's. Nothing changed in
		// AWS, so the new release's full sync must arrive at the steady state the previous one
		// left: the same counts, the same known unmanaged resources, the same networks and
		// subnets.
		Eventually(func(g Gomega) {
			after = takeNewSnapshot(g)
			g.Expect(after.scope.Status.LastSyncTime).NotTo(BeNil())
			g.Expect(after.scope.Status.LastSyncTime.Time).To(BeTemporally(">", acquired.Truncate(time.Second)),
				"the scope has not synced since the new leader took over")
			g.Expect(readyCondition(&after.scope)).NotTo(BeNil())
			g.Expect(readyCondition(&after.scope).Status).To(Equal(metav1.ConditionTrue), readyCondition(&after.scope).Message)
			g.Expect(after.steadyState()).To(Equal(before.steadyState()))
		}).Should(Succeed())

		By("comparing the NetworkScope")
		// What the new release adds (a default of a new field) is allowed; what it loses or
		// changes is not.
		Expect(subsetDiff("spec", before.scope.Spec, after.scope.Spec)).To(BeEmpty())
		Expect(subsetDiff("annotations", before.scope.Annotations, after.scope.Annotations)).To(BeEmpty())
		for _, t := range before.scope.Status.Targets {
			now := targetStatus(&after.scope, t.Account)
			Expect(now).NotTo(BeNil(), "target %s is gone", t.Account)
			Expect(now.Error).To(BeEmpty())
			Expect(now.UnmanagedIDs).To(ConsistOf(t.UnmanagedIDs), "the known unmanaged resources of %s", t.Account)
		}

		By("comparing the networks and subnets")
		Expect(mapKeys(after.networks)).To(ConsistOf(mapKeys(before.networks)))
		for id, n := range before.networks {
			Expect(subsetDiff(id, n.Spec, after.networks[id].Spec)).To(BeEmpty())
			Expect(after.networks[id].Status.CIDRBlocks).To(Equal(n.Status.CIDRBlocks), id)
			Expect(after.networks[id].Status.Owner).To(Equal(n.Status.Owner), id)
			Expect(after.networks[id].Status.Subnets).To(Equal(n.Status.Subnets), id)
		}
		Expect(mapKeys(after.subnets)).To(ConsistOf(mapKeys(before.subnets)))
		for id, s := range before.subnets {
			Expect(subsetDiff(id, s.Spec, after.subnets[id].Spec)).To(BeEmpty())
			Expect(after.subnets[id].Status.CIDRBlock).To(Equal(s.Status.CIDRBlock), id)
			Expect(after.subnets[id].Status.Owner).To(Equal(s.Status.Owner), id)
			Expect(after.subnets[id].Status.Tags).To(Equal(s.Status.Tags), id)
		}

		By("comparing the SubnetClaim: still Ready, with the same reservations and no second set in AWS")
		oldClaim := before.claims["payments"]
		Eventually(func(g Gomega) {
			claim := takeNewSnapshot(g).claims["payments"]
			g.Expect(claimReady(&claim)).NotTo(BeNil())
			g.Expect(claimReady(&claim).Status).To(Equal(metav1.ConditionTrue), claimReady(&claim).Message)
			g.Expect(claim.Status.Allocations).To(HaveLen(len(oldClaim.Status.Allocations)))
			for _, a := range oldClaim.Status.Allocations {
				g.Expect(claim.Status.Allocations).To(ContainElement(SatisfyAll(
					HaveField("Name", a.Name), HaveField("Zone", a.Zone), HaveField("CIDRBlock", a.CIDRBlock),
					HaveField("SubnetID", a.SubnetID))))
			}
			g.Expect(claim.Annotations[networkv1beta1.AnnotationCreatedBy]).
				To(Equal(oldClaim.Annotations[networkv1beta1.AnnotationCreatedBy]), "the creator stays")
		}).Should(Succeed())
		Expect(describeSubnetsByTag(context.Background(), "hs/claim", "default/payments")).To(HaveLen(2))

		By("comparing the ResourceImports: applied once, not again")
		for name, imp := range before.imports {
			now := after.imports[name]
			Expect(now.Status.State).To(Equal(imp.Status.State), name)
			Expect(now.Status.AppliedTags).To(Equal(imp.Status.AppliedTags), name)
			// An import applied again would carry a new time: the CreateTags call nobody asked for.
			Expect(now.Status.AppliedTime.Equal(imp.Status.AppliedTime)).To(BeTrue(),
				"%s was applied again: %v, before %v", name, now.Status.AppliedTime, imp.Status.AppliedTime)
			Expect(now.Annotations[networkv1beta1.AnnotationCreatedBy]).
				To(Equal(imp.Annotations[networkv1beta1.AnnotationCreatedBy]), name)
		}
		Expect(tagsOf(context.Background(), dryRunVPC)).NotTo(HaveKey("hs/owner"), "the dry run stayed dry")

		By("comparing the SheetExport")
		Expect(subsetDiff("spec", before.exports[upgradeScope].Spec, after.exports[upgradeScope].Spec)).To(BeEmpty())

		By("counting nothing as pending: every old object is migrated")
		var samples []metricSample
		Eventually(func(g Gomega) {
			samples = scrape(g, leader)
			for _, kind := range []string{"NetworkScope", "SubnetClaim", "ResourceImport", "SheetExport"} {
				s := findMetric(samples, "hs_migration_pending_objects", map[string]string{"kind": kind})
				g.Expect(s).NotTo(BeNil(), "no hs_migration_pending_objects for %s", kind)
				g.Expect(s.value).To(BeZero(), kind)
			}
		}).Should(Succeed())

		By("waiting for the new leader's first sync to report every target")
		// The gauges are set for each target in the same pass that counts newly seen resources,
		// subnets last, so once all four exist that pass is over and the counter has its answer.
		Eventually(func(g Gomega) {
			samples = scrape(g, leader)
			for _, account := range []string{hubAccount, spokeAccount} {
				for _, kind := range []string{networkKind, "subnet"} {
					g.Expect(findSample(samples, unmanagedGauge, account, kind)).NotTo(BeNil(),
						"no %s for %s/%s yet", unmanagedGauge, account, kind)
				}
			}
		}).Should(Succeed())
		for _, s := range samples {
			if s.name == unmanagedCounter && s.labels["scope"] == upgradeScope {
				Expect(s.value).To(BeZero(), "counted again after the upgrade: %s %v", s.name, s.labels)
			}
		}
		Expect(findSample(samples, unmanagedGauge, hubAccount, networkKind).value).To(BeNumerically(">=", 1),
			"the dry-run VPC is still unmanaged, so the check above had something to not count")
	})

	It("lets the webhooks of the new release accept an update of every object", func() {
		// A label change is an UPDATE of the whole object, which the validating and mutating
		// webhooks see like any other edit. An object the new release refuses would be stuck:
		// nobody could change it without deleting it first.
		for _, args := range [][]string{
			{resScopes, upgradeScope},
			{resExports, upgradeScope},
			{resClaims, "payments", "-n", "default"},
			{resImports, "sandbox-vpc-import", "-n", "default"},
			{resImports, "sandbox-subnet-import", "-n", "default"},
			{resImports, "dry-run-import", "-n", "default"},
		} {
			_, err := utils.Run(exec.Command("kubectl", append([]string{"label", "--overwrite"},
				append(args, "e2e.hypersurgery.dev/upgraded=true")...)...))
			Expect(err).NotTo(HaveOccurred(), "the new release refused an update of %v", args)
		}
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
			vpcs := findSample(samples, unmanagedCounter, hubAccount, networkKind)
			g.Expect(vpcs).NotTo(BeNil(), "no %s", unmanagedCounter)
			g.Expect(vpcs.value).To(Equal(1.0), unmanagedCounter)
			for _, s := range samples {
				if s.name != unmanagedCounter || s.labels["scope"] != upgradeScope ||
					(s.labels["account"] == hubAccount && s.labels["kind"] == networkKind) {
					continue
				}
				g.Expect(s.value).To(BeZero(), "counted: %s %v", s.name, s.labels)
			}
		}).Should(Succeed())
	})

	It("holds a starting operator back while an old object was never migrated, and lets go once it is gone", func() {
		// What an upgrade that skipped 0.8, or did not wait for its migration, leaves behind.
		// Nothing serves webhooks for the old group any more, so the object is accepted as it is.
		By("creating a claim in aws.hypersurgery/v1alpha1 that nothing migrates")
		Expect(applyManifest(fmt.Sprintf(`
apiVersion: aws.hypersurgery/v1alpha1
kind: SubnetClaim
metadata:
  name: forgotten
  namespace: default
spec:
  scopeRef: %s
  account: "%s"
  region: %s
  vpcID: %s
  prefixLength: 24
  availabilityZones: [%sa]
  mode: Allocate
  owner: team-forgotten
`, upgradeScope, hubAccount, awsRegion, fix.hubVPC, awsRegion))).To(Succeed())

		By("finding it counted and warned about by the running release, which carries on")
		leader, _ := newLeader()
		Eventually(func(g Gomega) {
			s := findMetric(scrape(g, leader), "hs_migration_pending_objects", map[string]string{"kind": "SubnetClaim"})
			g.Expect(s).NotTo(BeNil())
			g.Expect(s.value).To(Equal(1.0))
		}).Should(Succeed())
		Eventually(func(g Gomega) {
			out, err := kubectlOut("get", "events", "-n", "default", "--field-selector",
				"involvedObject.name=forgotten,reason=MigrationPending", "-o", "jsonpath={.items[*].message}")
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(out).To(ContainSubstring("Never migrated to network.hypersurgery.dev"))
		}).Should(Succeed())
		oldPods := operatorPods()

		By("restarting the operator, whose new pod must not become ready")
		_, err := utils.Run(exec.Command("kubectl", "rollout", "restart", "deployment/"+upgradeDeployment, "-n", namespace))
		Expect(err).NotTo(HaveOccurred())
		var blocked string
		Eventually(func(g Gomega) {
			for _, pod := range operatorPods() {
				if !slices.Contains(oldPods, pod) {
					blocked = pod
				}
			}
			g.Expect(blocked).NotTo(BeEmpty(), "no new pod yet")
			logs, err := kubectlOut("logs", blocked, "-n", namespace)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(logs).To(ContainSubstring("were never migrated to network.hypersurgery.dev (SubnetClaim default/forgotten)"))
			g.Expect(logs).To(ContainSubstring("Roll back to 0.8.x"))
		}).Should(Succeed())
		Consistently(func(g Gomega) {
			ready, err := kubectlOut("get", "pod", blocked, "-n", namespace,
				"-o", `jsonpath={.status.conditions[?(@.type=="Ready")].status}`)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(ready).NotTo(Equal("True"), "the blocked pod became ready")
			g.Expect(operatorPods()).To(ContainElements(oldPods), "the running pods were replaced regardless")
		}, 45*time.Second, 5*time.Second).Should(Succeed())
		Eventually(func(g Gomega) {
			s := findMetric(scrape(g, blocked), "hs_migration_pending_objects", map[string]string{"kind": "SubnetClaim"})
			g.Expect(s).NotTo(BeNil())
			g.Expect(s.value).To(Equal(1.0))
		}).Should(Succeed())
		holder, err := kubectlOut("get", "lease", leaseName, "-n", namespace, "-o", "jsonpath={.spec.holderIdentity}")
		Expect(err).NotTo(HaveOccurred())
		Expect(holder).NotTo(HavePrefix(blocked+"_"), "a blocked pod never takes the lease")

		By("deleting the object, after which the rollout completes on its own")
		_, err = utils.Run(exec.Command("kubectl", "delete", resOldClaims, "forgotten", "-n", "default"))
		Expect(err).NotTo(HaveOccurred())
		waitForRollout(upgradeDeployment)
		newLeader()
	})

	It("keeps running after the old CRDs are deleted, as the upgrade guide says to", func() {
		_, err := utils.Run(exec.Command("kubectl", append([]string{"delete", "crd"}, strings.Fields(oldCRDs)...)...))
		Expect(err).NotTo(HaveOccurred())
		_, err = utils.Run(exec.Command("kubectl", "rollout", "restart", "deployment/"+upgradeDeployment, "-n", namespace))
		Expect(err).NotTo(HaveOccurred())
		waitForRollout(upgradeDeployment)
		leader, acquired := newLeader()

		Eventually(func(g Gomega) {
			scope := &networkv1beta1.NetworkScope{}
			g.Expect(getObject(resScopes, upgradeScope, scope)).To(Succeed())
			g.Expect(scope.Status.LastSyncTime).NotTo(BeNil())
			g.Expect(scope.Status.LastSyncTime.Time).To(BeTemporally(">", acquired.Truncate(time.Second)))
			g.Expect(readyCondition(scope)).NotTo(BeNil())
			g.Expect(readyCondition(scope).Status).To(Equal(metav1.ConditionTrue))
			samples := scrape(g, leader)
			for _, kind := range []string{"NetworkScope", "SubnetClaim", "ResourceImport", "SheetExport"} {
				s := findMetric(samples, "hs_migration_pending_objects", map[string]string{"kind": kind})
				g.Expect(s).NotTo(BeNil(), kind)
				g.Expect(s.value).To(BeZero(), kind)
			}
		}).Should(Succeed())
		// kubectl resolves the plain names to the one group left, once its discovery cache has
		// caught up; a fresh cache stands in for that.
		out, err := kubectlOut("get", "subnetclaims", "-n", "default", "--cache-dir", GinkgoT().TempDir(),
			"-o", "jsonpath={.items[*].apiVersion}")
		Expect(err).NotTo(HaveOccurred())
		Expect(out).To(ContainSubstring("network.hypersurgery.dev/v1beta1"))
	})
})

// migratedTo is the migrated-to annotation of an old-group object, which 0.8 set once the
// object's copy existed, or the error kubectl gave.
func migratedTo(args ...string) string {
	out, err := kubectlOut(append(append([]string{"get"}, args...),
		"-o", `jsonpath={.metadata.annotations.network\.hypersurgery\.dev/migrated-to}`)...)
	if err != nil {
		return "error: " + err.Error()
	}
	return out
}

// operatorPods lists the release's operator pods that are not being deleted.
func operatorPods() []string {
	GinkgoHelper()
	out, err := kubectlOut("get", "pods", "-n", namespace, "-l",
		"app.kubernetes.io/instance="+upgradeRelease+",app.kubernetes.io/name="+upgradeDeployment,
		"-o", `go-template={{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}} {{end}}{{end}}`)
	Expect(err).NotTo(HaveOccurred())
	return strings.Fields(out)
}

// leaderRunning returns the pod holding the lease, when it runs image ("" for any).
func leaderRunning(g Gomega, image string) string {
	out, err := utils.Run(exec.Command("kubectl", "get", "lease", leaseName, "-n", namespace,
		"-o", "jsonpath={.spec.holderIdentity}"))
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(strings.TrimSpace(out)).NotTo(BeEmpty(), "the lease is not held")
	leader := strings.SplitN(strings.TrimSpace(out), "_", 2)[0] // "<pod>_<uuid>"
	if image != "" {
		running, err := utils.Run(exec.Command("kubectl", "get", "pod", leader, "-n", namespace,
			"-o", "jsonpath={.spec.containers[0].image}"))
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(strings.TrimSpace(running)).To(Equal(image))
	}
	return leader
}

func sheetReady(conds []metav1.Condition) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == "Ready" {
			return &conds[i]
		}
	}
	return nil
}

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
// The unmanaged-resource metrics of the release under test, and the kind label of a network.
const (
	unmanagedGauge   = "hs_unmanaged_resources"
	unmanagedCounter = "hs_unmanaged_resources_total"
	networkKind      = "network"
)

func findSample(samples []metricSample, name, account, kind string) *metricSample {
	for i, s := range samples {
		if s.name == name && s.labels["scope"] == upgradeScope && s.labels["account"] == account &&
			s.labels["region"] == awsRegion && s.labels["kind"] == kind {
			return &samples[i]
		}
	}
	return nil
}

func mapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
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
