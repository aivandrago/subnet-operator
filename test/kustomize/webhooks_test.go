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
	"hypersurgery.dev/subnet-operator/test/utils"
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
	if err := utils.EnableKustomizeWebhooks(dir); err != nil {
		t.Fatal(err)
	}

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
	// Every Certificate config/certmanager brings is asked for under a real Service name: a
	// replacement left commented would leave its placeholder for cert-manager to sign.
	for _, doc := range docs {
		if strings.Contains(doc, "kind: Certificate") && strings.Contains(doc, "SERVICE_NAME") {
			t.Errorf("a Certificate keeps the placeholder of its Service:\n%s", doc)
		}
	}
	// And under a name: the Issuer is self-signed, so each certificate is its own CA, which
	// RFC 5280 does not allow to be nameless (#124). At most 64 characters, or cert-manager
	// refuses the Certificate.
	certificates := 0
	for _, doc := range docs {
		var object struct {
			Kind string `json:"kind"`
			Spec struct {
				CommonName string `json:"commonName"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &object); err != nil || object.Kind != "Certificate" {
			continue
		}
		certificates++
		if cn := object.Spec.CommonName; cn == "" || len(cn) > 64 {
			t.Errorf("a Certificate asks for the commonName %q, want 1 to 64 characters:\n%s", cn, doc)
		}
	}
	if certificates != 2 {
		t.Errorf("%d Certificates, want the webhooks' and the metrics'", certificates)
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
