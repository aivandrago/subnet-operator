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
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// failureModes is the page whose commands the second spec below runs as they are written.
var failureModes = filepath.Join("..", "..", "docs", "operations", "failure-modes.md")

// What that page says about deleting the CRDs after the operator is
// uninstalled, when their conversion still names a webhook that nothing serves any more.
var _ = Describe("Deleting the CRDs once the conversion webhook is gone", func() {
	// The conversion as the operator sets it, to a Service that does not exist here.
	webhookWithoutService := func() {
		GinkgoHelper()
		caFile := filepath.Join(GinkgoT().TempDir(), "ca.crt")
		Expect(os.WriteFile(caFile, testCA("uninstall"), 0o600)).To(Succeed())
		Expect((&Upgrader{Client: k8sClient, Conversion: ConversionWebhook, CAFile: caFile,
			Service: types.NamespacedName{Namespace: "subnets", Name: "subnet-operator-webhook"}}).
			ConfigureConversion(ctx)).To(Succeed())
	}
	deleteCRDs := func() {
		GinkgoHelper()
		for _, k := range Kinds {
			crd := &apiextensionsv1.CustomResourceDefinition{}
			crd.Name = k.CRDName()
			Expect(k8sClient.Delete(ctx, crd)).To(Succeed())
		}
	}
	// The specs delete the CRDs themselves; this removes what a failed one leaves behind.
	AfterEach(removeCRDs)

	It("removes them, objects and all, when every object is stored at v1", func() {
		installCRDs0_9()
		upgradeCRDs()
		_, err := (&Upgrader{Client: k8sClient}).MigrateStorage(ctx)
		Expect(err).NotTo(HaveOccurred())
		webhookWithoutService()
		for _, k := range Kinds {
			crd := &apiextensionsv1.CustomResourceDefinition{}
			Expect(k8sClient.Get(ctx, client.ObjectKey{Name: k.CRDName()}, crd)).To(Succeed())
			Expect(crd.Spec.Conversion.Strategy).To(Equal(apiextensionsv1.WebhookConverter), k.Kind)
			Expect(crd.Status.StoredVersions).To(Equal([]string{"v1"}), k.Kind)
		}

		// The state 1.0 and later leave behind: the API server reads the objects at the version
		// they are stored at, and asks the webhook nothing. The conversion stays as it is.
		deleteCRDs()
		waitForCRDsGone()
	})

	It("keeps them Terminating while objects are stored at v1beta1, until the conversion is None", func() {
		installCRDs0_9()
		upgradeCRDs()
		// Not what the operator does: it turns the webhook on only after the migration. It is
		// what a conversion patched by hand, or applied from git, before that gives.
		webhookWithoutService()

		deleteCRDs()
		for _, k := range Kinds {
			Eventually(askingAgain(func() (string, error) { return terminating(k) }), crdRemovalTimeout).Should(And(
				ContainSubstring("InstanceDeletionFailed"), ContainSubstring("conversion webhook for"),
				ContainSubstring(`service "subnet-operator-webhook" not found`)), k.Kind)
		}
		Consistently(remainingCRDs, "2s").Should(HaveLen(len(Kinds)))

		env := kubectlEnv()
		heading := "## A CRD that stays `Terminating` after the operator is uninstalled"
		// The condition changes with every attempt of the API server, and names the webhook in
		// most of them; the API server is asked to try again because it may have stopped trying.
		diagnosis := guideCommands(failureModes, heading, 1)
		Eventually(askingAgain(func() (string, error) { return runGuideCommands(diagnosis, env), nil }),
			crdRemovalTimeout, time.Second).Should(And(HavePrefix("InstanceDeletionFailed: could not list instances: "),
			ContainSubstring(`service "subnet-operator-webhook" not found`)))

		// The guide's remedy: the conversion back to None, and then any other change to the CRD,
		// because the API server retries a removal that failed later and later (the delay
		// doubles, up to 1000 seconds) and only a change that is not its own brings that forward.
		remedy := guideCommands(failureModes, heading, 2)
		Expect(remedy).To(And(ContainSubstring("kubectl patch crd"), ContainSubstring("kubectl annotate crd")))
		runGuideCommands(remedy, env)
		for _, k := range Kinds {
			crd := &apiextensionsv1.CustomResourceDefinition{}
			if err := k8sClient.Get(ctx, client.ObjectKey{Name: k.CRDName()}, crd); err == nil {
				Expect(crd.Spec.Conversion.Strategy).To(Equal(apiextensionsv1.NoneConverter), k.Kind)
			}
		}
		// The guide says to repeat the change for a CRD that is still there, which is what the
		// wait does.
		waitForCRDsGone()
	})
})

// terminating returns the reason and message of a CRD's Terminating condition.
func terminating(k Kind) (string, error) {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: k.CRDName()}, crd); err != nil {
		return "", err
	}
	for _, c := range crd.Status.Conditions {
		if c.Type == apiextensionsv1.Terminating {
			return c.Reason + ": " + c.Message, nil
		}
	}
	return "", nil
}
