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
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"hypersurgery.dev/subnet-operator/test/utils"
)

var (
	// managerImage is the manager image to be built and loaded for testing.
	managerImage = "example.com/aws-subnet-operator:v0.0.1"

	// motoContainer runs Moto on the Kind docker network.
	motoContainer = envOr("KIND_CLUSTER", "kind") + "-moto"
	// motoHostEndpoint is Moto as seen from the test process (published port on localhost).
	motoHostEndpoint string
	// motoClusterEndpoint is Moto as seen from pods in the Kind cluster (container IP).
	motoClusterEndpoint string
)

// TestE2E runs the e2e test suite in Kind, with Moto standing in for the AWS APIs.
//
// To enable kubectl kuberc (use custom kubectl configurations), set: KUBECTL_KUBERC=true
// By default, kuberc is disabled to ensure consistent test behavior across different environments.
func TestE2E(t *testing.T) {
	RegisterFailHandler(Fail)
	_, _ = fmt.Fprintf(GinkgoWriter, "Starting aws-subnet-operator e2e test suite\n")
	RunSpecs(t, "e2e suite")
}

var _ = BeforeSuite(func() {
	By("building the manager image")
	cmd := exec.Command("make", "docker-build", fmt.Sprintf("IMG=%s", managerImage))
	_, err := utils.Run(cmd)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to build the manager image")

	By("loading the manager image on Kind")
	err = utils.LoadImageToKindClusterWithName(managerImage)
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to load the manager image into Kind")

	configureKubectlKubeRC()
	startMoto()
})

var _ = AfterSuite(func() {
	By("removing the Moto container")
	_, _ = utils.Run(exec.Command("docker", "rm", "-f", motoContainer))
})

// startMoto runs Moto on the "kind" docker network, so that pods reach it by container IP
// and the test process reaches it through a random localhost port.
func startMoto() {
	By("starting Moto")
	_, _ = utils.Run(exec.Command("docker", "rm", "-f", motoContainer))
	_, err := utils.Run(exec.Command("docker", "run", "-d", "--name", motoContainer,
		"--network", "kind", "-p", "127.0.0.1::5000", envOr("MOTO_IMAGE", "motoserver/moto:latest")))
	ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to start Moto")

	out, err := utils.Run(exec.Command("docker", "port", motoContainer, "5000/tcp"))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	motoHostEndpoint = "http://" + strings.TrimSpace(strings.Split(out, "\n")[0])

	out, err = utils.Run(exec.Command("docker", "inspect", "-f",
		`{{(index .NetworkSettings.Networks "kind").IPAddress}}`, motoContainer))
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
	motoClusterEndpoint = "http://" + strings.TrimSpace(out) + ":5000"

	By("waiting for Moto to answer at " + motoHostEndpoint)
	EventuallyWithOffset(1, func() error {
		_, err := utils.Run(exec.Command("curl", "-fsS", "-o", "/dev/null", motoHostEndpoint+"/moto-api/"))
		return err
	}, time.Minute, time.Second).Should(Succeed())
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Disable kubectl kuberc by default for test isolation.
// This prevents local kubectl configurations from affecting test behavior.
// To enable kuberc, set: KUBECTL_KUBERC=true
func configureKubectlKubeRC() {
	if os.Getenv("KUBECTL_KUBERC") != "true" {
		By("disabling kubectl kuberc for test isolation")
		err := os.Setenv("KUBECTL_KUBERC", "false")
		ExpectWithOffset(1, err).NotTo(HaveOccurred(), "Failed to disable kubectl kuberc")
		_, _ = fmt.Fprintf(GinkgoWriter,
			"kubectl kuberc disabled for consistent test behavior (override with KUBECTL_KUBERC=true)\n")
	} else {
		_, _ = fmt.Fprintf(GinkgoWriter, "kubectl kuberc enabled (KUBECTL_KUBERC=true)\n")
	}
}
