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

// Package kustomize checks the kustomize install. Its webhooks are off by default and turned
// on by uncommenting the [WEBHOOK] and [CERTMANAGER] sections, as kubebuilder lays them out;
// nothing else would notice if one of those sections stopped working.
package kustomize

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	"hypersurgery.dev/subnet-operator/internal/crdversions"
)

// servingCert is the cert-manager Certificate of the webhooks, as namespace/name.
const servingCert = "subnet-operator-system/subnet-operator-serving-cert"

func kustomizeBinary(t *testing.T) string {
	t.Helper()
	if p, err := filepath.Abs("../../bin/kustomize"); err == nil {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("kustomize"); err == nil {
		return p
	}
	t.Skip("kustomize not found; run make kustomize")
	return ""
}

func build(t *testing.T, dir string) []string {
	t.Helper()
	cmd := exec.Command(kustomizeBinary(t), "build", dir)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("kustomize build %s: %v: %s", dir, err, stderr.String())
	}
	return strings.Split(string(out), "\n---\n")
}

func crdsOf(t *testing.T, docs []string) map[string]apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	out := map[string]apiextensionsv1.CustomResourceDefinition{}
	for _, doc := range docs {
		if !strings.Contains(doc, "kind: CustomResourceDefinition") {
			continue
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal([]byte(doc), &crd); err != nil {
			t.Fatal(err)
		}
		out[crd.Name] = crd
	}
	return out
}

// By default the kustomize install has no webhooks, so the CRDs keep the API server's own
// conversion, which is exact for v1beta1 and v1.
func TestTheDefaultInstallConvertsWithoutAWebhook(t *testing.T) {
	crds := crdsOf(t, build(t, "../../config/default"))
	for _, name := range crdversions.CRDNames() {
		crd, ok := crds[name]
		if !ok {
			t.Errorf("%s is not installed", name)
			continue
		}
		if crd.Spec.Conversion != nil && crd.Spec.Conversion.Strategy != apiextensionsv1.NoneConverter {
			t.Errorf("%s: conversion %s without a webhook to serve it", name, crd.Spec.Conversion.Strategy)
		}
	}
}

// With the [WEBHOOK] and [CERTMANAGER] sections uncommented, every CRD converts through the
// operator's webhook Service, with the CA cert-manager injects from the webhook's certificate.
func TestTheWebhookSectionsWireTheConversionOfEveryCRD(t *testing.T) {
	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../../config")); err != nil {
		t.Fatal(err)
	}
	uncomment(t, filepath.Join(dir, "crd", "kustomization.yaml"), func(line string) bool {
		return strings.HasPrefix(line, "#- path: patches/webhook_in_") ||
			line == "#configurations:" || line == "#- kustomizeconfig.yaml"
	})
	uncomment(t, filepath.Join(dir, "default", "kustomization.yaml"), webhookSections())

	docs := build(t, filepath.Join(dir, "default"))
	crds := crdsOf(t, docs)
	for _, name := range crdversions.CRDNames() {
		crd := crds[name]
		c := crd.Spec.Conversion
		if c == nil || c.Strategy != apiextensionsv1.WebhookConverter || c.Webhook == nil ||
			c.Webhook.ClientConfig == nil || c.Webhook.ClientConfig.Service == nil {
			t.Errorf("%s: conversion %+v", name, c)
			continue
		}
		svc := c.Webhook.ClientConfig.Service
		if svc.Namespace != "subnet-operator-system" || svc.Name != "subnet-operator-webhook-service" ||
			svc.Path == nil || *svc.Path != crdversions.ConvertPath {
			t.Errorf("%s: conversion goes to %s/%s %v", name, svc.Namespace, svc.Name, svc.Path)
		}
		if got := crd.Annotations["cert-manager.io/inject-ca-from"]; got != servingCert {
			t.Errorf("%s: inject-ca-from = %q", name, got)
		}
	}
	for _, kind := range []string{"ValidatingWebhookConfiguration", "MutatingWebhookConfiguration"} {
		found := false
		for _, doc := range docs {
			if strings.Contains(doc, "kind: "+kind) {
				found = true
				if !strings.Contains(doc, "cert-manager.io/inject-ca-from: "+servingCert) {
					t.Errorf("%s without the CA injection", kind)
				}
			}
		}
		if !found {
			t.Errorf("no %s", kind)
		}
	}
}

// webhookSections decides, line by line, what enabling the webhooks with cert-manager
// uncomments in config/default: the webhook and certmanager resources, the manager's webhook
// patch, and every replacement except the ones for the metrics certificate.
func webhookSections() func(string) bool {
	inReplacements, metrics, patch := false, false, false
	return func(line string) bool {
		switch {
		case line == "#- ../webhook" || line == "#- ../certmanager":
			return true
		case line == "#- path: manager_webhook_patch.yaml":
			patch = true
			return true
		case patch && strings.HasPrefix(line, "#  "):
			return true
		case line == "#replacements:":
			patch, inReplacements = false, true
			return true
		case !inReplacements:
			patch = false
			return false
		case strings.HasPrefix(line, "# - source:"):
			// The two metrics blocks come first; the first webhook block ends them.
			if strings.Contains(line, "for metrics") {
				metrics = true
			} else if strings.Contains(line, "webhook") || strings.Contains(line, "Webhook") {
				metrics = false
			}
			return !metrics
		case strings.HasPrefix(line, "# +kubebuilder"):
			return false
		}
		if metrics {
			return false
		}
		return strings.HasPrefix(line, "#   ") || strings.HasPrefix(line, "#  targets")
	}
}

// uncomment removes the leading "#" of every line keep says to.
func uncomment(t *testing.T, path string, keep func(string) bool) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	for i, line := range lines {
		if keep(line) {
			lines[i] = strings.TrimPrefix(line, "#")
		}
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Fatal(err)
	}
}
