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
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
	"hypersurgery.dev/subnet-operator/test/utils"
)

// namespace where the project is deployed in
const namespace = "subnet-operator-system"

// serviceAccountName created for the project
const serviceAccountName = "subnet-operator-controller-manager"

// metricsServiceName is the name of the metrics service of the project
const metricsServiceName = "subnet-operator-controller-manager-metrics-service"

// deploymentName is the controller-manager Deployment
const deploymentName = "subnet-operator-controller-manager"

// The resources of the new group, named in full: while aws.hypersurgery is still installed,
// `kubectl get networkscopes` means its NetworkScopes, because kubectl picks the group that
// sorts first.
const (
	resScopes    = "networkscopes.network.hypersurgery.dev"
	resNetworks  = "networks.network.hypersurgery.dev"
	resSubnets   = "subnets.network.hypersurgery.dev"
	resClaims    = "subnetclaims.network.hypersurgery.dev"
	resImports   = "resourceimports.network.hypersurgery.dev"
	resExports   = "sheetexports.network.hypersurgery.dev"
	resOldScopes = "networkscopes.aws.hypersurgery"
	resOldClaims = "subnetclaims.aws.hypersurgery"
	resOldImport = "resourceimports.aws.hypersurgery"
)

// scopeName is the NetworkScope created by the discovery specs
const scopeName = "e2e"

// metricsRoleBindingName is the name of the RBAC that will be created to allow get the metrics data
const metricsRoleBindingName = "subnet-operator-metrics-binding"

var _ = Describe("Manager", Ordered, func() {
	var (
		controllerPodName string
		fix               fixture
	)

	// Before running the tests, set up the environment by creating the namespace,
	// enforce the restricted security policy to the namespace, installing CRDs,
	// and deploying the controller.
	BeforeAll(func() {
		By("creating manager namespace")
		cmd := exec.Command("kubectl", "create", "ns", namespace)
		_, err := utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")

		By("labeling the namespace to enforce the restricted security policy")
		cmd = exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
			"pod-security.kubernetes.io/enforce=restricted")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")

		By("installing CRDs")
		cmd = exec.Command("make", "install")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to install CRDs")

		By("deploying the controller-manager")
		cmd = exec.Command("make", "deploy", fmt.Sprintf("IMG=%s", managerImage))
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to deploy the controller-manager")

		By("creating the events queue in Moto")
		queueURL := createEventsQueue(context.Background())

		By("pointing the controller-manager at Moto")
		cmd = exec.Command("kubectl", "set", "env", "deployment/"+deploymentName, "-n", namespace,
			"AWS_ENDPOINT_URL="+motoClusterEndpoint, "AWS_REGION="+awsRegion, "EVENTS_QUEUE_URL="+queueURL,
			"AWS_ACCESS_KEY_ID=test", "AWS_SECRET_ACCESS_KEY=test", "AWS_EC2_METADATA_DISABLED=true")
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to configure the controller-manager")

		By("enabling writes, so SubnetClaims can create subnets")
		cmd = exec.Command("kubectl", "patch", "deployment/"+deploymentName, "-n", namespace, "--type=json", "-p",
			`[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--enable-writes"}]`)
		_, err = utils.Run(cmd)
		Expect(err).NotTo(HaveOccurred(), "Failed to enable writes")
		cmd = exec.Command("kubectl", "rollout", "status", "deployment/"+deploymentName, "-n", namespace, "--timeout=3m")
		if _, err = utils.Run(cmd); err != nil {
			dumpManagerState()
			Expect(err).NotTo(HaveOccurred(), "controller-manager rollout did not finish")
		}
	})

	// After all tests have been executed, clean up by undeploying the controller, uninstalling CRDs,
	// and deleting the namespace.
	AfterAll(func() {
		By("deleting the NetworkScope")
		cmd := exec.Command("kubectl", "delete", resScopes, scopeName, "--ignore-not-found", "--wait=false")
		_, _ = utils.Run(cmd)

		By("cleaning up the curl pod for metrics")
		cmd = exec.Command("kubectl", "delete", "pod", "curl-metrics", "-n", namespace)
		_, _ = utils.Run(cmd)

		By("undeploying the controller-manager")
		cmd = exec.Command("make", "undeploy")
		_, _ = utils.Run(cmd)

		By("uninstalling CRDs")
		cmd = exec.Command("make", "uninstall")
		_, _ = utils.Run(cmd)

		By("removing manager namespace")
		cmd = exec.Command("kubectl", "delete", "ns", namespace)
		_, _ = utils.Run(cmd)
	})

	// After each test, check for failures and collect logs, events,
	// and pod descriptions for debugging.
	AfterEach(func() {
		specReport := CurrentSpecReport()
		if specReport.Failed() {
			By("Fetching controller manager pod logs")
			cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
			controllerLogs, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Controller logs:\n %s", controllerLogs)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Controller logs: %s", err)
			}

			By("Fetching Kubernetes events")
			cmd = exec.Command("kubectl", "get", "events", "-n", namespace, "--sort-by=.lastTimestamp")
			eventsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Kubernetes events:\n%s", eventsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get Kubernetes events: %s", err)
			}

			By("Fetching curl-metrics logs")
			cmd = exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
			metricsOutput, err := utils.Run(cmd)
			if err == nil {
				_, _ = fmt.Fprintf(GinkgoWriter, "Metrics logs:\n %s", metricsOutput)
			} else {
				_, _ = fmt.Fprintf(GinkgoWriter, "Failed to get curl-metrics logs: %s", err)
			}

			By("Fetching controller manager pod description")
			cmd = exec.Command("kubectl", "describe", "pod", controllerPodName, "-n", namespace)
			podDescription, err := utils.Run(cmd)
			if err == nil {
				fmt.Println("Pod description:\n", podDescription)
			} else {
				fmt.Println("Failed to describe controller pod")
			}
		}
	})

	SetDefaultEventuallyTimeout(2 * time.Minute)
	SetDefaultEventuallyPollingInterval(time.Second)

	Context("Manager", func() {
		It("should run successfully", func() {
			By("validating that the controller-manager pod is running as expected")
			verifyControllerUp := func(g Gomega) {
				By("getting the name of the controller-manager pod")
				cmd := exec.Command("kubectl", "get",
					"pods", "-l", "control-plane=controller-manager",
					"-o", "go-template={{ range .items }}"+
						"{{ if not .metadata.deletionTimestamp }}"+
						"{{ .metadata.name }}"+
						"{{ \"\\n\" }}{{ end }}{{ end }}",
					"-n", namespace,
				)

				podOutput, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve controller-manager pod information")
				podNames := utils.GetNonEmptyLines(podOutput)
				g.Expect(podNames).To(HaveLen(1), "expected 1 controller pod running")
				controllerPodName = podNames[0]
				g.Expect(controllerPodName).To(ContainSubstring("controller-manager"))

				By("validating the pod's status")
				cmd = exec.Command("kubectl", "get",
					"pods", controllerPodName, "-o", "jsonpath={.status.phase}",
					"-n", namespace,
				)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Running"), "Incorrect controller-manager pod status")
			}
			Eventually(verifyControllerUp).Should(Succeed())
		})

		It("mirrors VPCs and subnets of the hub and a spoke account", func() {
			By("seeding VPCs and subnets into Moto")
			fix = seedAWS(context.Background())

			By("creating the NetworkScope")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`
apiVersion: network.hypersurgery.dev/v1beta1
kind: NetworkScope
metadata:
  name: %s
spec:
  provider: AWS
  accounts:
    - id: "%s"
    - id: "%s"
      aws:
        roleARN: %s
  regions: [%s]
  networkSelector:
    matchTags:
      hs/managed: "true"
  requiredSubnetTags: [hs/owner, hs/env, hs/tier]
  namespaceSelector:
    matchExpressions:
      - key: kubernetes.io/metadata.name
        operator: In
        values: [default]
  resyncInterval: 1m
`, scopeName, hubAccount, spokeAccount, spokeRoleARN, awsRegion))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create the NetworkScope")

			By("waiting for the first sync")
			Eventually(func(g Gomega) {
				scope := &networkv1beta1.NetworkScope{}
				g.Expect(getObject(resScopes, scopeName, scope)).To(Succeed())
				g.Expect(readyCondition(scope)).NotTo(BeNil())
				g.Expect(readyCondition(scope).Status).To(Equal(metav1.ConditionTrue), readyCondition(scope).Message)
				g.Expect(scope.Status.Networks).To(Equal(int32(2)))
				g.Expect(scope.Status.Subnets).To(Equal(int32(3)))
			}, 3*time.Minute, 2*time.Second).Should(Succeed())

			By("checking the public subnet")
			sn := &networkv1beta1.Subnet{}
			Expect(getObject(resSubnets, fix.publicSubnet, sn)).To(Succeed())
			Expect(sn.Labels).To(HaveKeyWithValue(networkv1beta1.LabelAccount, hubAccount))
			Expect(sn.Spec.Provider).To(Equal(networkv1beta1.ProviderAWS))
			Expect(sn.Spec.NetworkID).To(Equal(fix.hubVPC))
			Expect(sn.Labels).To(HaveKeyWithValue(networkv1beta1.LabelProvider, "aws"))
			Expect(sn.Status.Name).To(Equal("prod-public-a"))
			Expect(sn.Status.CIDRBlock).To(Equal("10.0.1.0/24"))
			Expect(sn.Status.Zone).To(Equal(awsRegion + "a"))
			Expect(sn.Status.AWS).NotTo(BeNil())
			Expect(sn.Status.AWS.Public).To(BeTrue())
			Expect(sn.Status.AWS.RouteTableID).To(HavePrefix("rtb-"))
			Expect(sn.Status.TotalIPs).To(HaveValue(Equal(int64(251))))
			Expect(sn.Status.AvailableIPs).To(HaveValue(Equal(int64(251))))
			Expect(sn.Status.Owner).To(Equal("team-web"))
			Expect(sn.Status.Tier).To(Equal("public"))
			Expect(sn.Status.MissingTags).To(BeEmpty())

			By("checking the untagged private subnet")
			sn = &networkv1beta1.Subnet{}
			Expect(getObject(resSubnets, fix.privateSubnet, sn)).To(Succeed())
			Expect(sn.Status.AWS.Public).To(BeFalse())
			Expect(sn.Status.MissingTags).To(Equal([]string{"hs/owner", "hs/env", "hs/tier"}))

			By("checking the spoke account subnet reached through AssumeRole")
			sn = &networkv1beta1.Subnet{}
			Expect(getObject(resSubnets, fix.spokeSubnet, sn)).To(Succeed())
			Expect(sn.Spec.Account).To(Equal(spokeAccount))
			Expect(sn.Status.TotalIPs).To(HaveValue(Equal(int64(11))))
			Expect(sn.Status.Owner).To(Equal("team-partner"))

			By("checking VPC aggregates and the cross-account CIDR overlap")
			vpc := &networkv1beta1.Network{}
			Expect(getObject(resNetworks, fix.hubVPC, vpc)).To(Succeed())
			Expect(vpc.Status.Name).To(Equal("prod"))
			Expect(vpc.Status.Owner).To(Equal("platform"))
			Expect(vpc.Status.Subnets).To(Equal(int32(2)))
			Expect(vpc.Status.TotalIPs).To(HaveValue(Equal(int64(502))))
			Expect(vpc.Status.OverlapsWith).To(Equal([]string{spokeAccount + "/" + awsRegion + "/" + fix.spokeVPC}))

			By("checking that the VPC without hs/managed=true is ignored")
			Expect(getObject(resNetworks, fix.unmanagedVPC, &networkv1beta1.Network{})).NotTo(Succeed())
		})

		It("counts the resources nobody tagged without mirroring them", func() {
			By("waiting for the unmanaged sandbox VPC and its subnet to be counted")
			Eventually(func(g Gomega) {
				scope := &networkv1beta1.NetworkScope{}
				g.Expect(getObject(resScopes, scopeName, scope)).To(Succeed())
				hub := targetStatus(scope, hubAccount)
				g.Expect(hub).NotTo(BeNil(), "the hub account has no target status yet")
				// Moto keeps a default VPC of its own in every account, so the sandbox VPC and
				// its subnet raise counts that are not zero to begin with: assert what this
				// fixture added, not a total that belongs to Moto.
				g.Expect(hub.UnmanagedNetworks).To(BeNumerically(">=", 1))
				g.Expect(hub.UnmanagedSubnets).To(BeNumerically(">=", 1))
				g.Expect(scope.Status.Unmanaged).To(Equal(sumUnmanaged(scope)))
			}, 3*time.Minute, 2*time.Second).Should(Succeed())

			By("checking that counting them did not put them in the inventory")
			Expect(getObject(resNetworks, fix.unmanagedVPC, &networkv1beta1.Network{})).NotTo(Succeed())
			Expect(getObject(resSubnets, fix.unmanagedSubnet, &networkv1beta1.Subnet{})).NotTo(Succeed())
		})

		It("removes a subnet deleted in AWS on the next resync", func() {
			deleteSubnet(context.Background(), fix.privateSubnet)
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", resSubnets, fix.privateSubnet, "--ignore-not-found", "-o", "name"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(BeEmpty())
			}, 3*time.Minute, 5*time.Second).Should(Succeed())
		})

		It("reports an unreachable account without dropping the others", func() {
			By("adding an account whose roleARN points to a different account")
			cmd := exec.Command("kubectl", "patch", resScopes, scopeName, "--type=json", "-p",
				`[{"op":"add","path":"/spec/accounts/-","value":{"id":"333333333333","aws":{"roleARN":"arn:aws:iam::444444444444:role/wrong"}}}]`)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())

			Eventually(func(g Gomega) {
				scope := &networkv1beta1.NetworkScope{}
				g.Expect(getObject(resScopes, scopeName, scope)).To(Succeed())
				cond := readyCondition(scope)
				g.Expect(cond).NotTo(BeNil())
				g.Expect(cond.Status).To(Equal(metav1.ConditionFalse))
				g.Expect(cond.Message).To(ContainSubstring("333333333333/" + awsRegion))
				g.Expect(scope.Status.Subnets).To(Equal(int32(2)))
			}, 2*time.Minute, 2*time.Second).Should(Succeed())
			Expect(getObject(resSubnets, fix.publicSubnet, &networkv1beta1.Subnet{})).To(Succeed())
		})

		It("syncs a changed account within seconds of an EC2 change event", func() {
			By("making full resyncs rare, so only the event can explain a quick update")
			cmd := exec.Command("kubectl", "patch", resScopes, scopeName, "--type=merge", "-p",
				`{"spec":{"resyncInterval":"1h"}}`)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				scope := &networkv1beta1.NetworkScope{}
				g.Expect(getObject(resScopes, scopeName, scope)).To(Succeed())
				g.Expect(scope.Status.ObservedGeneration).To(Equal(scope.Generation))
			}, time.Minute, time.Second).Should(Succeed())

			By("creating a subnet in the spoke account and reporting it through SQS")
			ctx := context.Background()
			subnet := createSpokeSubnet(ctx, fix.spokeVPC, "10.0.129.0/24")
			sendChangeEvent(ctx, spokeAccount, "CreateSubnet")

			Eventually(func(g Gomega) {
				sn := &networkv1beta1.Subnet{}
				g.Expect(getObject(resSubnets, subnet, sn)).To(Succeed())
				g.Expect(sn.Spec.Account).To(Equal(spokeAccount))
				g.Expect(sn.Status.TotalIPs).To(HaveValue(Equal(int64(251))))
			}, time.Minute, 2*time.Second).Should(Succeed())

			By("checking that the event was consumed")
			out, err := utils.Run(exec.Command("kubectl", "logs", "deployment/"+deploymentName, "-n", namespace))
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(ContainSubstring("Consuming EC2 change events"))
		})

		It("creates the subnets a SubnetClaim asks for", func() {
			By("claiming a /24 in two AZs of the hub VPC")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`
apiVersion: network.hypersurgery.dev/v1beta1
kind: SubnetClaim
metadata:
  name: payments
  namespace: default
spec:
  scopeRef: %s
  account: "%s"
  region: %s
  networkID: %s
  prefixLength: 24
  zones: [%sa, %sb]
  owner: team-payments
  env: prod
  tier: private
  tags:
    cost-center: cc-42
`, scopeName, hubAccount, awsRegion, fix.hubVPC, awsRegion, awsRegion))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create the SubnetClaim")

			var claim networkv1beta1.SubnetClaim
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", resClaims, "payments", "-n", "default", "-o", "json"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(json.Unmarshal([]byte(out), &claim)).To(Succeed())
				g.Expect(claim.Status.Allocations).To(HaveLen(2))
				for _, a := range claim.Status.Allocations {
					g.Expect(a.State).To(Equal(networkv1beta1.AllocationCreated), a.Error)
					g.Expect(a.SubnetID).To(HavePrefix("subnet-"))
				}
				g.Expect(claimReady(&claim)).NotTo(BeNil())
				g.Expect(claimReady(&claim).Status).To(Equal(metav1.ConditionTrue), claimReady(&claim).Message)
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("checking the subnets in AWS")
			// 10.0.1.0/24 and 10.0.2.0/24 exist; the claim gets the next free blocks.
			created := describeSubnetsByTag(context.Background(), "hs/claim", "default/payments")
			Expect(created).To(HaveLen(2))
			cidrs := []string{}
			for _, sn := range created {
				cidrs = append(cidrs, sn.cidr)
				Expect(sn.tags).To(HaveKeyWithValue("hs/owner", "team-payments"))
				Expect(sn.tags).To(HaveKeyWithValue("hs/tier", "private"))
				Expect(sn.tags).To(HaveKeyWithValue("cost-center", "cc-42"))
				Expect(sn.tags).To(HaveKeyWithValue("hs/managed-by", "subnet-operator"))
				Expect(sn.tags["Name"]).To(HavePrefix("payments-"))
			}
			// First fit over 10.0.0.0/16: 10.0.0.0/24 was never used, and 10.0.2.0/24 became free
			// when an earlier spec deleted the private subnet in AWS. 10.0.1.0/24 stays taken.
			Expect(cidrs).To(ConsistOf("10.0.0.0/24", "10.0.2.0/24"))

			By("checking that the inventory picked them up")
			Eventually(func(g Gomega) {
				for _, a := range claim.Status.Allocations {
					sn := &networkv1beta1.Subnet{}
					g.Expect(getObject(resSubnets, a.SubnetID, sn)).To(Succeed())
					g.Expect(sn.Status.CIDRBlock).To(Equal(a.CIDRBlock))
					g.Expect(sn.Status.Owner).To(Equal("team-payments"))
				}
			}, 2*time.Minute, 2*time.Second).Should(Succeed())
		})

		It("should ensure the metrics endpoint is serving metrics", func() {
			By("creating a ClusterRoleBinding for the service account to allow access to metrics")
			cmd := exec.Command("kubectl", "create", "clusterrolebinding", metricsRoleBindingName,
				"--clusterrole=subnet-operator-metrics-reader",
				fmt.Sprintf("--serviceaccount=%s:%s", namespace, serviceAccountName),
			)
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create ClusterRoleBinding")

			By("validating that the metrics service is available")
			cmd = exec.Command("kubectl", "get", "service", metricsServiceName, "-n", namespace)
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Metrics service should exist")

			By("getting the service account token")
			token, err := serviceAccountToken()
			Expect(err).NotTo(HaveOccurred())
			Expect(token).NotTo(BeEmpty())

			By("ensuring the controller pod is ready")
			verifyControllerPodReady := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pod", controllerPodName, "-n", namespace,
					"-o", "jsonpath={.status.conditions[?(@.type=='Ready')].status}")
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("True"), "Controller pod not ready")
			}
			Eventually(verifyControllerPodReady, 3*time.Minute, time.Second).Should(Succeed())

			By("verifying that the controller manager is serving the metrics server")
			verifyMetricsServerStarted := func(g Gomega) {
				cmd := exec.Command("kubectl", "logs", controllerPodName, "-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(ContainSubstring("Serving metrics server"),
					"Metrics server not yet started")
			}
			Eventually(verifyMetricsServerStarted, 3*time.Minute, time.Second).Should(Succeed())

			// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness

			By("creating the curl-metrics pod to access the metrics endpoint")
			cmd = exec.Command("kubectl", "run", "curl-metrics", "--restart=Never",
				"--namespace", namespace,
				"--image=curlimages/curl:latest",
				"--overrides",
				fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": [
								"for i in $(seq 1 30); do curl -v -k -H 'Authorization: Bearer %s' https://%s.%s.svc.cluster.local:8443/metrics && exit 0 || sleep 2; done; exit 1"
							],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {
									"drop": ["ALL"]
								},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {
									"type": "RuntimeDefault"
								}
							}
						}],
						"serviceAccountName": "%s"
					}
				}`, token, metricsServiceName, namespace, serviceAccountName))
			_, err = utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred(), "Failed to create curl-metrics pod")

			By("waiting for the curl-metrics pod to complete.")
			verifyCurlUp := func(g Gomega) {
				cmd := exec.Command("kubectl", "get", "pods", "curl-metrics",
					"-o", "jsonpath={.status.phase}",
					"-n", namespace)
				output, err := utils.Run(cmd)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(output).To(Equal("Succeeded"), "curl pod in wrong status")
			}
			Eventually(verifyCurlUp, 5*time.Minute).Should(Succeed())

			By("getting the metrics by checking curl-metrics logs")
			verifyMetricsAvailable := func(g Gomega) {
				metricsOutput, err := getMetricsOutput()
				g.Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
				g.Expect(metricsOutput).NotTo(BeEmpty())
				g.Expect(metricsOutput).To(ContainSubstring("< HTTP/1.1 200 OK"))
			}
			Eventually(verifyMetricsAvailable, 2*time.Minute).Should(Succeed())

			By("checking the inventory metrics")
			metricsOutput, err := getMetricsOutput()
			Expect(err).NotTo(HaveOccurred())
			Expect(metricsOutput).To(MatchRegexp(
				`hs_aws_subnet_available_ips\{[^}]*subnet_id="%s"[^}]*\} 251`, fix.publicSubnet))
			Expect(metricsOutput).To(MatchRegexp(
				`hs_aws_vpc_cidr_overlaps\{[^}]*vpc_id="%s"[^}]*\} 1`, fix.hubVPC))
			Expect(metricsOutput).To(ContainSubstring(
				`hs_aws_target_up{account="333333333333",region="%s",scope="%s"} 0`, awsRegion, scopeName))
			Expect(metricsOutput).To(ContainSubstring(
				`hs_aws_target_up{account="%s",region="%s",scope="%s"} 1`, spokeAccount, awsRegion, scopeName))

			By("checking the unmanaged resource metrics the onboarding alert reads")
			// This spec is declared before the import specs, so the sandbox VPC and its subnet
			// are still untagged here: both the gauge and the counter must see them.
			Expect(metricsOutput).To(MatchRegexp(
				`hs_aws_unmanaged_resources\{account="%s",[^}]*kind="vpc"[^}]*\} [1-9]`, hubAccount))
			Expect(metricsOutput).To(MatchRegexp(
				`hs_aws_unmanaged_resources\{account="%s",[^}]*kind="subnet"[^}]*\} [1-9]`, hubAccount))
			Expect(metricsOutput).To(MatchRegexp(
				`hs_aws_unmanaged_resources_total\{account="%s",[^}]*kind="vpc"[^}]*\} [1-9]`, hubAccount))
			Expect(metricsOutput).To(MatchRegexp(
				`hs_aws_unmanaged_resources_total\{account="%s",[^}]*kind="subnet"[^}]*\} [1-9]`, hubAccount))
		})

		It("takes an unmanaged VPC and its subnet over with ResourceImports", func() {
			By("importing the sandbox VPC and the untagged subnet inside it")
			applyImports(fmt.Sprintf(`
apiVersion: network.hypersurgery.dev/v1beta1
kind: ResourceImport
metadata:
  name: sandbox-vpc-import
  namespace: default
spec:
  scopeRef: %[1]s
  account: "%[2]s"
  region: %[3]s
  resourceID: %[4]s
  requestedBy: e2e
  tags:
    hs/managed: "true"
    hs/owner: team-sandbox
---
apiVersion: network.hypersurgery.dev/v1beta1
kind: ResourceImport
metadata:
  name: sandbox-subnet-import
  namespace: default
spec:
  scopeRef: %[1]s
  account: "%[2]s"
  region: %[3]s
  resourceID: %[5]s
  requestedBy: e2e
  tags:
    hs/owner: team-sandbox
    hs/env: dev
    hs/tier: private
`, scopeName, hubAccount, awsRegion, fix.unmanagedVPC, fix.unmanagedSubnet))

			for _, name := range []string{"sandbox-vpc-import", "sandbox-subnet-import"} {
				Eventually(func(g Gomega) {
					imp := &networkv1beta1.ResourceImport{}
					g.Expect(getNamespaced(resImports, "default", name, imp)).To(Succeed())
					g.Expect(imp.Status.State).To(Equal(networkv1beta1.ImportApplied), imp.Status.Error)
					g.Expect(importReady(imp)).NotTo(BeNil())
					g.Expect(importReady(imp).Status).To(Equal(metav1.ConditionTrue), importReady(imp).Message)
					g.Expect(imp.Status.AppliedTags).To(Equal(imp.Spec.Tags))
					g.Expect(imp.Status.AppliedTime).NotTo(BeNil())
				}, 2*time.Minute, 2*time.Second).Should(Succeed())
			}

			By("checking the tags in AWS, and that the ones already there survived")
			Expect(tagsOf(context.Background(), fix.unmanagedVPC)).To(SatisfyAll(
				HaveKeyWithValue("hs/managed", "true"),
				HaveKeyWithValue("hs/owner", "team-sandbox"),
				HaveKeyWithValue("Name", "sandbox"),
			))
			Expect(tagsOf(context.Background(), fix.unmanagedSubnet)).To(SatisfyAll(
				HaveKeyWithValue("hs/owner", "team-sandbox"),
				HaveKeyWithValue("hs/tier", "private"),
			))

			By("waiting for the inventory to pick both up")
			Eventually(func(g Gomega) {
				vpc := &networkv1beta1.Network{}
				g.Expect(getObject(resNetworks, fix.unmanagedVPC, vpc)).To(Succeed())
				g.Expect(vpc.Status.Owner).To(Equal("team-sandbox"))

				sn := &networkv1beta1.Subnet{}
				g.Expect(getObject(resSubnets, fix.unmanagedSubnet, sn)).To(Succeed())
				g.Expect(sn.Spec.NetworkID).To(Equal(fix.unmanagedVPC))
				g.Expect(sn.Status.Owner).To(Equal("team-sandbox"))
				g.Expect(sn.Status.MissingTags).To(BeEmpty())
			}, 3*time.Minute, 2*time.Second).Should(Succeed())
		})

		It("changes nothing for a dry-run import", func() {
			By("creating one more untagged subnet in AWS")
			subnet := createSubnet(context.Background(), hubEC2(context.Background()),
				fix.unmanagedVPC, "10.50.2.0/24", awsRegion+"b")

			applyImports(fmt.Sprintf(`
apiVersion: network.hypersurgery.dev/v1beta1
kind: ResourceImport
metadata:
  name: dry-run-import
  namespace: default
spec:
  scopeRef: %s
  account: "%s"
  region: %s
  resourceID: %s
  requestedBy: e2e
  dryRun: true
  tags:
    hs/owner: team-sandbox
`, scopeName, hubAccount, awsRegion, subnet))

			Eventually(func(g Gomega) {
				imp := &networkv1beta1.ResourceImport{}
				g.Expect(getNamespaced(resImports, "default", "dry-run-import", imp)).To(Succeed())
				g.Expect(imp.Status.State).To(Equal(networkv1beta1.ImportSkipped))
				g.Expect(importReady(imp)).NotTo(BeNil())
				g.Expect(importReady(imp).Reason).To(Equal("DryRun"))
				g.Expect(importReady(imp).Message).To(ContainSubstring("hs/owner=team-sandbox"))
				g.Expect(imp.Status.AppliedTags).To(BeEmpty())
				g.Expect(imp.Status.AppliedTime).To(BeNil())
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("checking that AWS never heard about it")
			// Give a wrong reconcile time to show itself: a dry run that tags anyway would have
			// done so long before the import reported Skipped.
			Consistently(func(g Gomega) {
				g.Expect(tagsOf(context.Background(), subnet)).To(BeEmpty())
			}, 15*time.Second, 3*time.Second).Should(Succeed())
		})

		It("migrates an aws.hypersurgery/v1alpha1 claim with its reservations", func() {
			By("stopping the operator, which would migrate the claim before its status is written")
			// A 0.7 claim has its reservations before 0.8 ever sees it. Written while the operator
			// runs, the copy would be taken between the create and the status write.
			scaleManager := func(replicas string) {
				GinkgoHelper()
				_, err := utils.Run(exec.Command("kubectl", "scale", "deployment/"+deploymentName, "-n", namespace,
					"--replicas="+replicas))
				Expect(err).NotTo(HaveOccurred())
			}
			scaleManager("0")
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
					"-n", namespace, "-o", "name"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(strings.TrimSpace(out)).To(BeEmpty())
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("writing a claim the way 0.7 left it: in the old group, with a reserved CIDR in its status")
			cmd := exec.Command("kubectl", "apply", "-f", "-")
			cmd.Stdin = strings.NewReader(fmt.Sprintf(`
apiVersion: aws.hypersurgery/v1alpha1
kind: SubnetClaim
metadata:
  name: legacy
  namespace: default
  annotations:
    aws.hypersurgery/created-by: jane@example.com
spec:
  scopeRef: %s
  account: "%s"
  region: %s
  vpcID: %s
  prefixLength: 24
  availabilityZones: [%sa]
  mode: Allocate
  owner: team-legacy
  namePrefix: legacy
`, scopeName, hubAccount, awsRegion, fix.hubVPC, awsRegion))
			_, err := utils.Run(cmd)
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.Run(exec.Command("kubectl", "patch", resOldClaims, "legacy", "-n", "default",
				"--subresource=status", "--type=merge", "-p",
				fmt.Sprintf(`{"status":{"allocations":[{"availabilityZone":"%sa","cidrBlock":"10.0.200.0/24","state":"Pending"}]}}`,
					awsRegion)))
			Expect(err).NotTo(HaveOccurred())

			By("starting the operator again")
			scaleManager("1")
			_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/"+deploymentName,
				"-n", namespace, "--timeout=3m"))
			Expect(err).NotTo(HaveOccurred())
			out, err := utils.Run(exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
				"-n", namespace, "-o", "jsonpath={.items[0].metadata.name}"))
			Expect(err).NotTo(HaveOccurred())
			controllerPodName = strings.TrimSpace(out)

			By("waiting for its copy in network.hypersurgery.dev, which keeps the reservation")
			Eventually(func(g Gomega) {
				claim := &networkv1beta1.SubnetClaim{}
				g.Expect(getNamespaced(resClaims, "default", "legacy", claim)).To(Succeed())
				g.Expect(claim.Annotations).To(HaveKeyWithValue(networkv1beta1.AnnotationCreatedBy, "jane@example.com"))
				g.Expect(claim.Spec.NetworkID).To(Equal(fix.hubVPC))
				g.Expect(claim.Status.Allocations).To(HaveLen(1))
				g.Expect(claim.Status.Allocations[0].Name).To(Equal("legacy-a"))
				g.Expect(claim.Status.Allocations[0].CIDRBlock).To(Equal("10.0.200.0/24"),
					"the reservation is kept, not allocated again")
				g.Expect(claimReady(claim)).NotTo(BeNil())
				g.Expect(claimReady(claim).Status).To(Equal(metav1.ConditionTrue), claimReady(claim).Message)
			}, 2*time.Minute, 2*time.Second).Should(Succeed())

			By("finding the old claim marked as migrated")
			out, err = kubectlOut("get", resOldClaims, "legacy", "-n", "default",
				"-o", "jsonpath={.metadata.annotations.network\\.hypersurgery\\.dev/migrated-to}")
			Expect(err).NotTo(HaveOccurred())
			Expect(out).To(Equal("legacy"))
		})

		It("converts manifests with the image's migrate-manifests command", func() {
			dir, err := utils.GetProjectDir()
			Expect(err).NotTo(HaveOccurred())
			in, err := os.ReadFile(filepath.Join(dir, "internal", "migration", "testdata", "v1alpha1", "02-organization.yaml"))
			Expect(err).NotTo(HaveOccurred())
			cmd := exec.Command("docker", "run", "--rm", "-i", managerImage, "migrate-manifests")
			cmd.Stdin = strings.NewReader(string(in))
			var stdout, stderr strings.Builder
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			Expect(cmd.Run()).To(Succeed(), stderr.String())
			Expect(stdout.String()).To(ContainSubstring("apiVersion: network.hypersurgery.dev/v1beta1"))
			Expect(stdout.String()).To(ContainSubstring("provider: AWS"))
			Expect(stderr.String()).To(ContainSubstring("namespaceSelector"), "the note about the selector")

			By("applying the result, which the new CRDs accept")
			apply := exec.Command("kubectl", "apply", "--dry-run=server", "-f", "-")
			apply.Stdin = strings.NewReader(stdout.String())
			_, err = utils.Run(apply)
			Expect(err).NotTo(HaveOccurred())
		})

		It("serves the dashboard, which reads the cluster as the viewer and never as itself", func() {
			const dashboard = "subnet-operator-dashboard"

			By("installing the dashboard from the Helm chart, from the image under test")
			// Only the dashboard's own template: the operator itself is deployed with kustomize.
			// The namespace enforces the restricted Pod Security profile, so this also checks
			// that the dashboard's pod is admitted under it.
			image := strings.SplitN(managerImage, ":", 2)
			dir, err := utils.GetProjectDir()
			Expect(err).NotTo(HaveOccurred())
			helm := exec.Command(envOr("HELM", "helm"), "template", "subnet-operator", "charts/subnet-operator",
				"-n", namespace, "--show-only", "templates/dashboard.yaml",
				"--set", "dashboard.enabled=true", "--set", "image.repository="+image[0], "--set", "image.tag="+image[1])
			helm.Dir = dir
			manifest, err := helm.Output()
			Expect(err).NotTo(HaveOccurred(), "helm template failed")
			apply := exec.Command("kubectl", "apply", "-n", namespace, "-f", "-")
			apply.Stdin = strings.NewReader(string(manifest))
			_, err = utils.Run(apply)
			Expect(err).NotTo(HaveOccurred())
			_, err = utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/"+dashboard,
				"-n", namespace, "--timeout=3m"))
			Expect(err).NotTo(HaveOccurred(), "the dashboard did not become ready")

			By("creating one viewer who may read subnets and one who may read nothing")
			for _, args := range [][]string{
				{"create", "serviceaccount", "dashboard-reader", "-n", namespace},
				{"create", "serviceaccount", "dashboard-nobody", "-n", namespace},
				{"create", "clusterrole", "dashboard-e2e-reader", "--verb=get,list,watch", "--resource=" + resSubnets},
				{"create", "clusterrolebinding", "dashboard-e2e-reader", "--clusterrole=dashboard-e2e-reader",
					"--serviceaccount=" + namespace + ":dashboard-reader"},
			} {
				_, err := utils.Run(exec.Command("kubectl", args...))
				Expect(err).NotTo(HaveOccurred())
			}
			DeferCleanup(func() {
				_, _ = utils.Run(exec.Command("kubectl", "delete", "clusterrolebinding,clusterrole", "dashboard-e2e-reader", "--ignore-not-found"))
				_, _ = utils.Run(exec.Command("kubectl", "delete", "pod", "curl-dashboard", "-n", namespace, "--ignore-not-found"))
			})
			token := func(sa string) string {
				out, err := utils.Run(exec.Command("kubectl", "create", "token", sa, "-n", namespace))
				Expect(err).NotTo(HaveOccurred())
				return strings.TrimSpace(out)
			}
			reader, nobody := token("dashboard-reader"), token("dashboard-nobody")

			By("calling the dashboard as each of them, and as nobody at all")
			base := fmt.Sprintf("http://%s.%s.svc.cluster.local", dashboard, namespace)
			subnets := base + "/apis/network.hypersurgery.dev/v1beta1/subnets"
			script := strings.Join([]string{
				// The Service may take a moment to route to the new pod.
				"for i in $(seq 1 30); do curl -sf --max-time 5 -o /dev/null " + base + "/dashboard/ && break; sleep 2; done",
				"call() { echo \"== $1\"; shift; curl -s --max-time 20 -w '\\ncode=%{http_code}\\n' \"$@\"; }",
				"call page " + base + "/dashboard/ -o /dev/null",
				"call reader -H 'Authorization: Bearer " + reader + "' " + subnets,
				"call nobody -H 'Authorization: Bearer " + nobody + "' " + subnets,
				"call anonymous " + subnets,
				"call secrets -H 'Authorization: Bearer " + reader + "' " + base + "/api/v1/namespaces/" + namespace + "/secrets",
				"call impersonating -H 'Authorization: Bearer " + reader + "' -H 'Impersonate-User: system:admin' " + subnets,
			}, "\n")
			_, err = utils.Run(exec.Command("kubectl", "run", "curl-dashboard", "--restart=Never",
				"--namespace", namespace, "--image=curlimages/curl:latest", "--overrides", fmt.Sprintf(`{
					"spec": {
						"containers": [{
							"name": "curl",
							"image": "curlimages/curl:latest",
							"command": ["/bin/sh", "-c"],
							"args": [%q],
							"securityContext": {
								"readOnlyRootFilesystem": true,
								"allowPrivilegeEscalation": false,
								"capabilities": {"drop": ["ALL"]},
								"runAsNonRoot": true,
								"runAsUser": 1000,
								"seccompProfile": {"type": "RuntimeDefault"}
							}
						}]
					}
				}`, script)))
			Expect(err).NotTo(HaveOccurred())
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "pod", "curl-dashboard", "-n", namespace,
					"-o", "jsonpath={.status.phase}"))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(out).To(Equal("Succeeded"))
			}, 3*time.Minute, 2*time.Second).Should(Succeed(), func() string {
				// Say which call was stuck, and what the dashboard made of it.
				calls, _ := utils.Run(exec.Command("kubectl", "logs", "curl-dashboard", "-n", namespace))
				dash, _ := utils.Run(exec.Command("kubectl", "logs", "deployment/"+dashboard, "-n", namespace, "--tail=50"))
				return "curl output:\n" + calls + "\ndashboard logs:\n" + dash
			})
			logs, err := utils.Run(exec.Command("kubectl", "logs", "curl-dashboard", "-n", namespace))
			Expect(err).NotTo(HaveOccurred())

			calls := map[string]string{}
			for _, part := range strings.Split(logs, "== ")[1:] {
				name, body, _ := strings.Cut(part, "\n")
				calls[name] = body
			}
			Expect(calls["page"]).To(ContainSubstring("code=200"), "the app is served")

			// The reader's own RBAC lets them list subnets, so the list comes back, with the
			// subnets discovered earlier in the suite.
			Expect(calls["reader"]).To(ContainSubstring("code=200"))
			Expect(calls["reader"]).To(ContainSubstring(fix.publicSubnet))

			// The same request with a token that may read nothing is refused by the API server,
			// in that viewer's name: the dashboard lent it no permissions of its own.
			Expect(calls["nobody"]).To(ContainSubstring("code=403"))
			Expect(calls["nobody"]).To(ContainSubstring("system:serviceaccount:" + namespace + ":dashboard-nobody"))
			Expect(calls["nobody"]).To(ContainSubstring(`cannot list resource \"subnets\"`))

			Expect(calls["anonymous"]).To(ContainSubstring("code=401"))

			// Secrets are refused by the dashboard itself, before the API server is asked, even
			// for a viewer the API server might have let through.
			Expect(calls["secrets"]).To(ContainSubstring("code=403"))
			Expect(calls["secrets"]).To(ContainSubstring("the dashboard does not forward this request"))

			// Impersonation headers are dropped, so the API server sees the viewer as themselves.
			// Had the header reached it, it would have refused the reader, who may not impersonate.
			Expect(calls["impersonating"]).To(ContainSubstring("code=200"))
			Expect(calls["impersonating"]).To(ContainSubstring(fix.publicSubnet))
		})

		// Last on purpose: it leaves two replicas running, and every spec before it expects
		// exactly one. AfterAll undeploys everything straight after, so nothing scales back —
		// a DeferCleanup here would run after AfterAll, against a deployment already gone.
		It("hands the lease over within seconds when the leader is stopped", func() {
			const leaseName = "1095b947.hypersurgery"
			holder := func(g Gomega) string {
				out, err := utils.Run(exec.Command("kubectl", "get", "lease", leaseName, "-n", namespace,
					"-o", "jsonpath={.spec.holderIdentity}"))
				g.Expect(err).NotTo(HaveOccurred())
				return strings.SplitN(out, "_", 2)[0] // "<pod>_<uuid>"
			}

			By("running a standby next to the leader")
			_, err := utils.Run(exec.Command("kubectl", "scale", "deployment", deploymentName,
				"-n", namespace, "--replicas=2"))
			Expect(err).NotTo(HaveOccurred())

			var pods []string
			Eventually(func(g Gomega) {
				out, err := utils.Run(exec.Command("kubectl", "get", "pods", "-l", "control-plane=controller-manager",
					"-n", namespace, "-o", "jsonpath={range .items[*]}{.metadata.name} {.status.containerStatuses[0].ready}{\"\\n\"}{end}"))
				g.Expect(err).NotTo(HaveOccurred())
				pods = nil
				for _, line := range utils.GetNonEmptyLines(out) {
					if f := strings.Fields(line); len(f) == 2 && f[1] == "true" {
						pods = append(pods, f[0])
					}
				}
				g.Expect(pods).To(HaveLen(2), "both replicas ready")
			}, 3*time.Minute, 2*time.Second).Should(Succeed())

			var leader string
			Eventually(func(g Gomega) {
				leader = holder(g)
				g.Expect(pods).To(ContainElement(leader), "the lease is held by one of the two")
			}, time.Minute, time.Second).Should(Succeed())
			standby := pods[0]
			if standby == leader {
				standby = pods[1]
			}

			By("stopping the leader the way a drain or a rollout does")
			stopped := time.Now()
			_, err = utils.Run(exec.Command("kubectl", "delete", "pod", leader, "-n", namespace, "--wait=false"))
			Expect(err).NotTo(HaveOccurred())

			// Without a hand-over nobody may take the lease until it runs out — 15s after the
			// leader's last renewal. With one, the leader clears it on the way out and a live
			// replica takes it within a retry period. Ten seconds tells the two apart.
			//
			// Any live replica counts, not only the standby: the Deployment starts a replacement
			// for the stopped pod straight away, and it may win the lease fairly. Every change of
			// holder is logged with its time, so a failure says which of the two things happened.
			var newLeader string
			last := leader
			Eventually(func(g Gomega) {
				h := holder(g)
				if h != last {
					_, _ = fmt.Fprintf(GinkgoWriter, "%6s after stop: lease holder %q\n",
						time.Since(stopped).Round(10*time.Millisecond), h)
					last = h
				}
				g.Expect(h).NotTo(BeEmpty(), "released, not yet taken")
				g.Expect(h).NotTo(Equal(leader), "still held by the stopped leader")
				newLeader = h
			}, 30*time.Second, 250*time.Millisecond).Should(Succeed())
			took := time.Since(stopped)
			_, _ = fmt.Fprintf(GinkgoWriter, "%s took the lease %s after the leader was stopped (standby was %s)\n",
				newLeader, took, standby)
			Expect(took).To(BeNumerically("<", 10*time.Second),
				"the lease was waited out instead of handed over")

			// The old leader is gone; AfterAll dumps the logs of controllerPodName on a failure,
			// so point it at the pod that now does the work.
			controllerPodName = newLeader
		})

		// +kubebuilder:scaffold:e2e-webhooks-checks

		// TODO: Customize the e2e test suite with scenarios specific to your project.
		// Consider applying sample/CR(s) and check their status and/or verifying
		// the reconciliation by using the metrics, i.e.:
		// metricsOutput, err := getMetricsOutput()
		// Expect(err).NotTo(HaveOccurred(), "Failed to retrieve logs from curl pod")
		// Expect(metricsOutput).To(ContainSubstring(
		//    fmt.Sprintf(`controller_runtime_reconcile_total{controller="%s",result="success"} 1`,
		//    strings.ToLower(<Kind>),
		// ))
	})
})

// serviceAccountToken returns a token for the specified service account in the given namespace.
// It uses the Kubernetes TokenRequest API to generate a token by directly sending a request
// and parsing the resulting token from the API response.
func serviceAccountToken() (string, error) {
	const tokenRequestRawString = `{
		"apiVersion": "authentication.k8s.io/v1",
		"kind": "TokenRequest"
	}`

	By("creating temporary file to store the token request")
	secretName := fmt.Sprintf("%s-token-request", serviceAccountName)
	tokenRequestFile := filepath.Join("/tmp", secretName)
	err := os.WriteFile(tokenRequestFile, []byte(tokenRequestRawString), os.FileMode(0o644))
	if err != nil {
		return "", err
	}

	var out string
	verifyTokenCreation := func(g Gomega) {
		By("executing kubectl command to create the token")
		cmd := exec.Command("kubectl", "create", "--raw", fmt.Sprintf(
			"/api/v1/namespaces/%s/serviceaccounts/%s/token",
			namespace,
			serviceAccountName,
		), "-f", tokenRequestFile)

		output, err := cmd.CombinedOutput()
		g.Expect(err).NotTo(HaveOccurred())

		By("parsing the JSON output to extract the token")
		var token tokenRequest
		err = json.Unmarshal(output, &token)
		g.Expect(err).NotTo(HaveOccurred())

		out = token.Status.Token
	}
	Eventually(verifyTokenCreation).Should(Succeed())

	return out, err
}

// dumpManagerState prints what the manager pods are doing, so a failed rollout says why.
func dumpManagerState() {
	for _, args := range [][]string{
		{"get", "pods", "-n", namespace, "-o", "wide"},
		{"describe", "deployment/" + deploymentName, "-n", namespace},
		{"logs", "-n", namespace, "-l", "control-plane=controller-manager", "--all-containers", "--tail=200"},
		{"logs", "-n", namespace, "-l", "control-plane=controller-manager", "--all-containers", "--tail=200", "--previous"},
	} {
		out, err := utils.Run(exec.Command("kubectl", args...))
		_, _ = fmt.Fprintf(GinkgoWriter, "\n--- kubectl %s ---\n%s\nerr: %v\n", strings.Join(args, " "), out, err)
	}
}

// kubectlOut runs kubectl and returns what it wrote to stdout. Its stderr goes to the test log:
// reading a deprecated kind prints a warning there, which must not end up in the JSON.
func kubectlOut(args ...string) (string, error) {
	cmd := exec.Command("kubectl", args...)
	dir, err := utils.GetProjectDir()
	if err != nil {
		return "", err
	}
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	_, _ = fmt.Fprintf(GinkgoWriter, "running: kubectl %s\n", strings.Join(args, " "))
	out, err := cmd.Output()
	if stderr.Len() > 0 {
		_, _ = fmt.Fprintf(GinkgoWriter, "%s", stderr.String())
	}
	if err != nil {
		return string(out), fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out), nil
}

// getObject reads a cluster-scoped object with kubectl into obj.
func getObject(kind, name string, obj any) error {
	out, err := kubectlOut("get", kind, name, "-o", "json")
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(out), obj)
}

// getNamespaced reads a namespaced object with kubectl into obj.
func getNamespaced(kind, ns, name string, obj any) error {
	out, err := kubectlOut("get", kind, name, "-n", ns, "-o", "json")
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(out), obj)
}

// applyImports applies a manifest given as text.
func applyImports(manifest string) {
	cmd := exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	_, err := utils.Run(cmd)
	Expect(err).NotTo(HaveOccurred(), "Failed to apply the ResourceImports")
}

// targetStatus returns the status of one account's target in the scope's region, or nil.
func targetStatus(scope *networkv1beta1.NetworkScope, account string) *networkv1beta1.TargetStatus {
	for i := range scope.Status.Targets {
		if scope.Status.Targets[i].Account == account && scope.Status.Targets[i].Region == awsRegion {
			return &scope.Status.Targets[i]
		}
	}
	return nil
}

// sumUnmanaged adds up the per-target counts, which is what the scope total should be.
func sumUnmanaged(scope *networkv1beta1.NetworkScope) int32 {
	var total int32
	for _, t := range scope.Status.Targets {
		total += t.UnmanagedNetworks + t.UnmanagedSubnets
	}
	return total
}

func importReady(imp *networkv1beta1.ResourceImport) *metav1.Condition {
	for i := range imp.Status.Conditions {
		if imp.Status.Conditions[i].Type == "Ready" {
			return &imp.Status.Conditions[i]
		}
	}
	return nil
}

func claimReady(claim *networkv1beta1.SubnetClaim) *metav1.Condition {
	for i := range claim.Status.Conditions {
		if claim.Status.Conditions[i].Type == "Ready" {
			return &claim.Status.Conditions[i]
		}
	}
	return nil
}

func readyCondition(scope *networkv1beta1.NetworkScope) *metav1.Condition {
	for i := range scope.Status.Conditions {
		if scope.Status.Conditions[i].Type == "Ready" {
			return &scope.Status.Conditions[i]
		}
	}
	return nil
}

// getMetricsOutput retrieves and returns the logs from the curl pod used to access the metrics endpoint.
func getMetricsOutput() (string, error) {
	By("getting the curl-metrics logs")
	cmd := exec.Command("kubectl", "logs", "curl-metrics", "-n", namespace)
	return utils.Run(cmd)
}

// tokenRequest is a simplified representation of the Kubernetes TokenRequest API response,
// containing only the token field that we need to extract.
type tokenRequest struct {
	Status struct {
		Token string `json:"token"`
	} `json:"status"`
}
