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

package crdversions

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// upgradeGuide is the page whose rollback commands the spec below runs as they are written.
var upgradeGuide = filepath.Join("..", "..", "docs", "operations", "upgrades.md")

// guideCommands returns the first shell block of a section of the upgrade guide, without the
// helm lines: there is no release in envtest, and what the spec checks is what the kubectl
// lines leave behind for 0.9.
func guideCommands(heading string) string {
	GinkgoHelper()
	f, err := os.Open(upgradeGuide)
	Expect(err).NotTo(HaveOccurred())
	defer func() { _ = f.Close() }()
	var (
		inSection, inBlock bool
		lines              []string
	)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "#") && !inBlock:
			inSection = line == heading
		case inSection && !inBlock && line == "```sh":
			inBlock = true
		case inBlock && line == "```":
			Expect(lines).NotTo(BeEmpty(), "an empty shell block under %s", heading)
			return strings.Join(lines, "\n")
		case inBlock:
			if !strings.HasPrefix(strings.TrimSpace(line), "helm ") {
				lines = append(lines, line)
			}
		}
	}
	Expect(scanner.Err()).NotTo(HaveOccurred())
	Fail("no shell block under " + heading + " in " + upgradeGuide)
	return ""
}

// The upgrade guide's way back from 1.0 to 0.9: the CRDs' conversion set back to None with
// kubectl, then helm rollback, keeping the 1.0 CRDs. 0.9 reads and writes v1beta1 only and does
// not serve the conversion webhook, so this runs the guide's own commands against a cluster in
// the state 1.0 leaves it in, and checks that what 0.9 does then works.
var _ = Describe("Rolling back to 0.9 as the upgrade guide says", Ordered, func() {
	var kubectlDir, kubeconfig string

	BeforeAll(func() {
		installCRDs0_9()
		upgradeCRDs()
		_, err := (&Upgrader{Client: k8sClient}).MigrateStorage(ctx)
		Expect(err).NotTo(HaveOccurred())
		// The conversion as 1.0 leaves it, pointing at a webhook that is gone once 0.9 runs.
		caFile := filepath.Join(GinkgoT().TempDir(), "ca.crt")
		Expect(os.WriteFile(caFile, testCA("rollback"), 0o600)).To(Succeed())
		Expect((&Upgrader{Client: k8sClient, Conversion: ConversionWebhook, CAFile: caFile,
			Service: types.NamespacedName{Namespace: "subnets", Name: "subnet-operator-webhook"}}).
			ConfigureConversion(ctx)).To(Succeed())
		Eventually(func() error { return listEvery("v1beta1") }).Should(HaveOccurred(),
			"v1beta1 is served without the webhook; the rollback would not need the guide's step")

		user, err := testEnv.AddUser(envtest.User{Name: "rollback", Groups: []string{"system:masters"}}, nil)
		Expect(err).NotTo(HaveOccurred())
		kubectl, err := user.Kubectl()
		Expect(err).NotTo(HaveOccurred())
		Expect(kubectl.Path).NotTo(BeEmpty(), "no kubectl among the envtest binaries")
		kubectlDir = filepath.Dir(kubectl.Path)
		for _, o := range kubectl.Opts {
			if v, ok := strings.CutPrefix(o, "--kubeconfig="); ok {
				kubeconfig = v
			}
		}
		Expect(kubeconfig).NotTo(BeEmpty())
	})
	AfterAll(removeCRDs)

	It("sets every CRD's conversion back to None with the guide's commands", func() {
		script := guideCommands("### Rolling back to 0.9")
		Expect(script).To(ContainSubstring("kubectl patch crd"))
		cmd := exec.CommandContext(ctx, "bash", "-euo", "pipefail", "-c", script)
		cmd.Env = append(os.Environ(), "PATH="+kubectlDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			"KUBECONFIG="+kubeconfig)
		out, err := cmd.CombinedOutput()
		Expect(err).NotTo(HaveOccurred(), "the guide's commands failed:\n%s", out)

		for _, k := range Kinds {
			crd := &apiextensionsv1.CustomResourceDefinition{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: k.CRDName()}, crd)).To(Succeed())
			Expect(crd.Spec.Conversion.Strategy).To(Equal(apiextensionsv1.NoneConverter), k.Kind)
			Expect(crd.Spec.Conversion.Webhook).To(BeNil(), k.Kind)
			Expect(crd.Status.StoredVersions).To(Equal([]string{"v1"}), k.Kind)
		}
	})

	It("lets 0.9 read and write v1beta1 with the 1.0 CRDs", func() {
		Eventually(func() error { return listEvery("v1beta1") }).Should(Succeed())
		for _, f := range fixtures {
			obj := &unstructured.Unstructured{}
			Expect(get("v1beta1", f.kind, f.namespace, f.name, obj)).To(Succeed())
			v1 := &unstructured.Unstructured{}
			Expect(get("v1", f.kind, f.namespace, f.name, v1)).To(Succeed())
			Expect(obj.Object["spec"]).To(Equal(v1.Object["spec"]), f.kind)
			Expect(obj.Object["status"]).To(Equal(v1.Object["status"]), f.kind)
			Expect(obj.Object["status"]).To(Equal(f.status), f.kind)
			obj.SetLabels(map[string]string{"written-by": "0.9"})
			Expect(k8sClient.Update(ctx, obj)).To(Succeed(), f.kind)
			Expect(k8sClient.Status().Update(ctx, obj)).To(Succeed(), f.kind)
		}
		created := &unstructured.Unstructured{}
		created.SetGroupVersionKind(gvk("v1beta1", "SheetExport"))
		created.SetName("after-the-rollback")
		Expect(unstructured.SetNestedField(created.Object, map[string]any{
			"scopeRef": "payments", "spreadsheetID": "other",
			"credentialsSecretRef": map[string]any{"name": "google", "namespace": "default"},
		}, "spec")).To(Succeed())
		Expect(k8sClient.Create(ctx, created)).To(Succeed())
		for _, k := range Kinds {
			Expect(storedVersionsOf(k)).To(Equal([]string{"v1"}), "what 0.9 writes is stored at v1")
		}
	})

	It("refuses the 0.9 CRDs, which the guide says to keep away", func() {
		for _, crd := range readCRDs(filepath.Join("testdata", "crds-0.9")) {
			current := &apiextensionsv1.CustomResourceDefinition{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: crd.Name}, current)).To(Succeed())
			current.Spec = crd.Spec
			err := k8sClient.Update(ctx, current)
			Expect(err).To(HaveOccurred(), crd.Name)
			Expect(err.Error()).To(ContainSubstring("storedVersions"), crd.Name)
		}
	})

	It("finds nothing to rewrite when upgraded to 1.0 again", func() {
		results, err := (&Upgrader{Client: k8sClient}).MigrateStorage(ctx)
		Expect(err).NotTo(HaveOccurred())
		for _, r := range results {
			Expect(r.Rewritten).To(BeZero(), r.Kind.Kind)
			Expect(r.Trimmed).To(BeFalse(), r.Kind.Kind)
		}
		Consistently(func() error { return listEvery("v1") }, 500*time.Millisecond).Should(Succeed())
	})
})
