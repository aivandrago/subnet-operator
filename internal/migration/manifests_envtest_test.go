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

package migration

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"regexp"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
)

// The examples of the last release, converted, must be accepted by the new CRDs: their schema
// and their CEL rules. Server-side dry run runs both without keeping anything.
var _ = Describe("Converted manifests", func() {
	It("are accepted by the network.hypersurgery.dev CRDs", func() {
		fixtures, err := filepath.Glob(filepath.Join("testdata", "v1alpha1", "*.yaml"))
		Expect(err).NotTo(HaveOccurred())
		converted := 0
		for _, fixture := range fixtures {
			in, err := os.ReadFile(fixture)
			Expect(err).NotTo(HaveOccurred())
			var out, notes bytes.Buffer
			Expect(Manifests(bytes.NewReader(in), &out, &notes)).To(Succeed())

			reader := utilyaml.NewYAMLReader(bufioReader(out.Bytes()))
			for {
				doc, err := reader.Read()
				if err != nil {
					break
				}
				obj := &unstructured.Unstructured{}
				if err := utilyaml.Unmarshal(doc, &obj.Object); err != nil || len(obj.Object) == 0 {
					continue
				}
				if obj.GroupVersionKind().Group != networkv1.GroupVersion.Group {
					continue
				}
				if obj.GetNamespace() == "" && obj.GetKind() != "NetworkScope" && obj.GetKind() != "SheetExport" {
					obj.SetNamespace("default")
				}
				Expect(k8sClient.Create(ctx, obj, client.DryRunAll)).To(Succeed(), "%s from %s", obj.GetName(), fixture)
				converted++
			}
		}
		Expect(converted).To(Equal(7), "every converted object of the fixtures was sent")
	})

	It("match the examples and samples, which the CRDs accept as they are", func() {
		// The examples are what people copy; one the API server refuses is a bug in the docs.
		files, err := filepath.Glob(filepath.Join("..", "..", "examples", "0*.yaml"))
		Expect(err).NotTo(HaveOccurred())
		samples, err := filepath.Glob(filepath.Join("..", "..", "config", "samples", "network_*.yaml"))
		Expect(err).NotTo(HaveOccurred())
		sent := 0
		for _, file := range append(files, samples...) {
			in, err := os.ReadFile(file)
			Expect(err).NotTo(HaveOccurred())
			reader := utilyaml.NewYAMLReader(bufioReader(in))
			for {
				doc, err := reader.Read()
				if err != nil {
					break
				}
				obj := &unstructured.Unstructured{}
				if err := utilyaml.Unmarshal(doc, &obj.Object); err != nil || len(obj.Object) == 0 {
					continue
				}
				Expect(obj.GetAPIVersion()).NotTo(HavePrefix("aws.hypersurgery"), "%s still uses the old group", file)
				if obj.GroupVersionKind().Group != networkv1.GroupVersion.Group {
					continue
				}
				Expect(obj.GetAPIVersion()).To(Equal(networkv1.GroupVersion.String()),
					"%s uses a deprecated version", file)
				Expect(k8sClient.Create(ctx, obj, client.DryRunAll)).To(Succeed(), "%s from %s", obj.GetName(), file)
				sent++
			}
		}
		Expect(sent).To(BeNumerically(">=", 10))
	})

	It("match the complete manifests in the docs, which the CRDs accept as they are", func() {
		// A YAML block in the docs with an apiVersion, a kind and a name is a manifest someone
		// will apply; at v1 it must be accepted. Blocks at older versions are the upgrade
		// guide's before-and-after and are left alone; fragments (a spec: alone) are skipped.
		docs := []string{filepath.Join("..", "..", "README.md"),
			filepath.Join("..", "..", "charts", "subnet-operator", "README.md")}
		err := filepath.WalkDir(filepath.Join("..", "..", "docs"), func(p string, d os.DirEntry, err error) error {
			if err == nil && filepath.Ext(p) == ".md" {
				docs = append(docs, p)
			}
			return err
		})
		Expect(err).NotTo(HaveOccurred())
		block := regexp.MustCompile("(?ms)^\\s*```ya?ml\\s*\\n(.*?)^\\s*```")
		sent := 0
		for _, file := range docs {
			in, err := os.ReadFile(file)
			Expect(err).NotTo(HaveOccurred())
			for _, m := range block.FindAllSubmatch(in, -1) {
				reader := utilyaml.NewYAMLReader(bufioReader(m[1]))
				for {
					doc, err := reader.Read()
					if err != nil {
						break
					}
					obj := &unstructured.Unstructured{}
					if err := utilyaml.Unmarshal(doc, &obj.Object); err != nil || len(obj.Object) == 0 {
						continue
					}
					if obj.GetAPIVersion() != networkv1.GroupVersion.String() || obj.GetName() == "" {
						continue
					}
					if obj.GetNamespace() == "" && obj.GetKind() != "NetworkScope" && obj.GetKind() != "SheetExport" &&
						obj.GetKind() != "Network" && obj.GetKind() != "Subnet" {
						obj.SetNamespace("default")
					}
					Expect(k8sClient.Create(ctx, obj, client.DryRunAll)).To(Succeed(), "%s from %s", obj.GetName(), file)
					sent++
				}
			}
		}
		Expect(sent).To(BeNumerically(">=", 2), "the README's SubnetClaim and ResourceImport at least")
	})

	It("are refused when an AWS account ID is not 12 digits, by the CRD itself", func() {
		scope := &networkv1.NetworkScope{
			ObjectMeta: metav1.ObjectMeta{Name: "bad-account"},
			Spec: networkv1.NetworkScopeSpec{Provider: networkv1.ProviderAWS,
				Accounts: []networkv1.Account{{ID: "12345"}}, Regions: []string{"eu-central-1"}},
		}
		err := k8sClient.Create(ctx, scope, client.DryRunAll)
		Expect(err).To(MatchError(ContainSubstring("an AWS account id is 12 digits")))
	})
})

func bufioReader(b []byte) *bufio.Reader { return bufio.NewReader(bytes.NewReader(b)) }
