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

package chart

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

// The chart-signed webhook certificate (issue #71): it lives webhook.certificate.duration, an
// upgrade reads it back while it has more than renewBefore left, and signs a new one when it
// has not, when it outlives the configured duration, or when the Secret has no record of it
// (every certificate a release before 2.0.1 signed). An upgrade is `helm template
// --dry-run=server` against a kube-apiserver (envtest) that holds the Secret, which is what
// makes `lookup` return it.

const (
	certSecret  = "release-subnet-operator-webhook-cert"
	annotNB     = "network.hypersurgery.dev/webhook-not-before"
	annotNA     = "network.hypersurgery.dev/webhook-not-after"
	annotPrevCA = "network.hypersurgery.dev/webhook-previous-ca-until"
	day         = 24 * time.Hour
)

var cluster struct {
	once       sync.Once
	env        *envtest.Environment
	kubeconfig string
	client     *kubernetes.Clientset
	err        error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if cluster.env != nil {
		_ = cluster.env.Stop()
	}
	os.Exit(code)
}

// apiServer starts envtest once for the package, or skips when its binaries are not there
// (make test provides them).
func apiServer(t *testing.T) (string, *kubernetes.Clientset) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS is not set; run make test")
	}
	cluster.once.Do(func() {
		env := &envtest.Environment{}
		cfg, err := env.Start()
		if err != nil {
			cluster.err = err
			return
		}
		cluster.env = env
		user, err := env.AddUser(envtest.User{Name: "chart-test", Groups: []string{"system:masters"}}, cfg)
		if err != nil {
			cluster.err = err
			return
		}
		kc, err := user.KubeConfig()
		if err != nil {
			cluster.err = err
			return
		}
		dir, err := os.MkdirTemp("", "chart-kubeconfig")
		if err != nil {
			cluster.err = err
			return
		}
		cluster.kubeconfig = filepath.Join(dir, "kubeconfig")
		if cluster.err = os.WriteFile(cluster.kubeconfig, kc, 0o600); cluster.err != nil {
			return
		}
		cluster.client, cluster.err = kubernetes.NewForConfig(cfg)
	})
	if cluster.err != nil {
		t.Fatalf("starting envtest: %v", cluster.err)
	}
	return cluster.kubeconfig, cluster.client
}

// webhookRender is what the chart renders around the webhook certificate.
type webhookRender struct {
	secret     *corev1.Secret
	validating admissionv1.ValidatingWebhookConfiguration
	mutating   admissionv1.MutatingWebhookConfiguration
	deployment appsv1.Deployment
	names      []string
}

// renderWebhook runs helm template, against the API server when kubeconfig is set (an
// upgrade: lookup sees what is there), plainly otherwise (an install, or GitOps).
func renderWebhook(t *testing.T, namespace, kubeconfig string, set ...string) webhookRender {
	t.Helper()
	out, err := helmTemplate(t, namespace, kubeconfig, set...)
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	var r webhookRender
	for doc := range strings.SplitSeq(out, "\n---") {
		var meta struct {
			Kind     string            `json:"kind"`
			Metadata metav1.ObjectMeta `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil || meta.Kind == "" {
			continue
		}
		r.names = append(r.names, meta.Kind+"/"+meta.Metadata.Name)
		var into any
		switch {
		case meta.Kind == "Secret" && meta.Metadata.Name == certSecret:
			r.secret = &corev1.Secret{}
			into = r.secret
		case meta.Kind == "ValidatingWebhookConfiguration":
			into = &r.validating
		case meta.Kind == "MutatingWebhookConfiguration":
			into = &r.mutating
		case meta.Kind == "Deployment" && strings.HasSuffix(meta.Metadata.Name, "subnet-operator"):
			into = &r.deployment
		default:
			continue
		}
		if err := yaml.Unmarshal([]byte(doc), into); err != nil {
			t.Fatalf("decoding %s: %v", meta.Kind, err)
		}
	}
	return r
}

func helmTemplate(t *testing.T, namespace, kubeconfig string, set ...string) (string, error) {
	t.Helper()
	args := []string{"template", "release", chartDir, "--namespace", namespace}
	if kubeconfig != "" {
		args = append(args, "--dry-run=server", "--kubeconfig", kubeconfig)
	}
	for _, s := range set {
		args = append(args, "--set", s)
	}
	cmd := exec.Command(helmBinary(t), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", &helmError{err: err, stderr: stderr.String()}
	}
	return string(out), nil
}

type helmError struct {
	err    error
	stderr string
}

func (e *helmError) Error() string { return e.err.Error() + ": " + e.stderr }

// certificates decodes every certificate of a PEM bundle.
func certificates(t *testing.T, bundle []byte) []*x509.Certificate {
	t.Helper()
	var certs []*x509.Certificate
	for {
		var block *pem.Block
		block, bundle = pem.Decode(bundle)
		if block == nil {
			break
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			t.Fatalf("parsing a certificate: %v", err)
		}
		certs = append(certs, c)
	}
	if len(certs) == 0 {
		t.Fatalf("no certificate in %q", bundle)
	}
	return certs
}

// check verifies what holds for every chart-signed certificate: the serving certificate
// chains to the first CA of ca.crt, both live at most validity, the Secret's record matches
// the certificate, and the webhook configurations trust exactly ca.crt.
func (r webhookRender) check(t *testing.T, validity time.Duration) (
	serving *x509.Certificate, cas []*x509.Certificate,
) {
	t.Helper()
	if r.secret == nil {
		t.Fatalf("no Secret %s among %v", certSecret, r.names)
	}
	d := r.secret.Data
	serving = certificates(t, d["tls.crt"])[0]
	cas = certificates(t, d["ca.crt"])
	if err := serving.CheckSignatureFrom(cas[0]); err != nil {
		t.Errorf("the serving certificate is not signed by the first CA of ca.crt: %v", err)
	}
	for _, c := range []*x509.Certificate{serving, cas[0]} {
		if got := c.NotAfter.Sub(c.NotBefore); got > validity+time.Minute {
			t.Errorf("%s lives %v, want at most %v", c.Subject.CommonName, got, validity)
		}
	}
	notAfter, err := time.Parse(time.RFC3339, r.secret.Annotations[annotNA])
	if err != nil {
		t.Fatalf("%s: %v", annotNA, err)
	}
	if diff := notAfter.Sub(serving.NotAfter).Abs(); diff > time.Minute {
		t.Errorf("%s = %v, the certificate expires %v", annotNA, notAfter, serving.NotAfter)
	}
	if _, err := time.Parse(time.RFC3339, r.secret.Annotations[annotNB]); err != nil {
		t.Errorf("%s: %v", annotNB, err)
	}
	for _, w := range r.validating.Webhooks {
		if !bytes.Equal(w.ClientConfig.CABundle, d["ca.crt"]) {
			t.Errorf("validating %s trusts a different CA bundle than ca.crt", w.Name)
		}
	}
	for _, w := range r.mutating.Webhooks {
		if !bytes.Equal(w.ClientConfig.CABundle, d["ca.crt"]) {
			t.Errorf("mutating %s trusts a different CA bundle than ca.crt", w.Name)
		}
	}
	return serving, cas
}

func TestTheSelfSignedCertificateLivesTheConfiguredDuration(t *testing.T) {
	for _, tc := range []struct {
		name     string
		set      []string
		validity time.Duration
	}{
		{"by default, a year", nil, 365 * day},
		{"90 days", []string{"webhook.certificate.duration=2160h", "webhook.certificate.renewBefore=240h"}, 90 * day},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := renderWebhook(t, "subnets", "", tc.set...)
			serving, cas := r.check(t, tc.validity)
			if len(cas) != 1 {
				t.Errorf("a new install trusts %d CAs, want 1", len(cas))
			}
			if left := time.Until(serving.NotAfter); left < tc.validity-time.Hour {
				t.Errorf("the certificate expires in %v, want %v", left, tc.validity)
			}
			if _, ok := r.secret.Annotations[annotPrevCA]; ok {
				t.Errorf("a new install records a previous CA")
			}
		})
	}
}

func TestRenewBeforeMustBeShorterThanTheDuration(t *testing.T) {
	_, err := helmTemplate(t, "subnets", "", "webhook.certificate.duration=720h", "webhook.certificate.renewBefore=720h")
	if err == nil || !strings.Contains(err.Error(), "must be shorter than webhook.certificate.duration") {
		t.Errorf("got %v, want renewBefore refused", err)
	}
}

// installed creates the Secret a first install renders in its own namespace, with the
// annotations changed by edit, and returns what that install rendered.
func installed(t *testing.T, namespace string, edit func(*corev1.Secret)) webhookRender {
	t.Helper()
	_, client := apiServer(t)
	ctx := context.Background()
	if _, err := client.CoreV1().Namespaces().Create(ctx,
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating namespace %s: %v", namespace, err)
	}
	first := renderWebhook(t, namespace, "")
	s := first.secret.DeepCopy()
	s.Namespace = namespace
	if edit != nil {
		edit(s)
	}
	owned(s, namespace)
	if _, err := client.CoreV1().Secrets(namespace).Create(ctx, s, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the Secret: %v", err)
	}
	return first
}

// owned puts on a rendered Secret what Helm puts on what it installs: a server-side dry run
// refuses to adopt anything else.
func owned(s *corev1.Secret, namespace string) {
	s.Namespace = namespace
	if s.Annotations == nil {
		s.Annotations = map[string]string{}
	}
	s.Annotations["meta.helm.sh/release-name"] = "release"
	s.Annotations["meta.helm.sh/release-namespace"] = namespace
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func TestAnUpgradeFarFromExpiryReusesTheCertificate(t *testing.T) {
	kubeconfig, _ := apiServer(t)
	first := installed(t, "cert-reuse", nil)
	r := renderWebhook(t, "cert-reuse", kubeconfig)
	r.check(t, 365*day)
	for _, k := range []string{"ca.crt", "tls.crt", "tls.key"} {
		if !bytes.Equal(r.secret.Data[k], first.secret.Data[k]) {
			t.Errorf("%s changed on an upgrade far from expiry", k)
		}
	}
	for _, k := range []string{annotNB, annotNA} {
		if r.secret.Annotations[k] != first.secret.Annotations[k] {
			t.Errorf("%s: %q, was %q", k, r.secret.Annotations[k], first.secret.Annotations[k])
		}
	}
}

// renewed checks an upgrade that signed a new certificate: the new CA first in ca.crt, the
// previous one after it for the pods that still serve the old certificate, and a date to drop
// it.
func renewed(t *testing.T, first, r webhookRender) {
	t.Helper()
	serving, cas := r.check(t, 365*day)
	if bytes.Equal(r.secret.Data["tls.key"], first.secret.Data["tls.key"]) {
		t.Fatalf("the key was not renewed")
	}
	if left := time.Until(serving.NotAfter); left < 364*day {
		t.Errorf("the new certificate expires in %v, want a year", left)
	}
	previous := certificates(t, first.secret.Data["ca.crt"])[0]
	if len(cas) != 2 || !cas[1].Equal(previous) {
		t.Fatalf("ca.crt holds %d CAs, want the new one and the previous one", len(cas))
	}
	oldServing := certificates(t, first.secret.Data["tls.crt"])[0]
	if err := oldServing.CheckSignatureFrom(cas[1]); err != nil {
		t.Errorf("the certificate the pods still serve does not chain to ca.crt: %v", err)
	}
	until, err := time.Parse(time.RFC3339, r.secret.Annotations[annotPrevCA])
	if err != nil {
		t.Fatalf("%s: %v", annotPrevCA, err)
	}
	if d := time.Until(until); d < 50*time.Minute || d > 61*time.Minute {
		t.Errorf("the previous CA is kept for %v, want an hour", d)
	}
}

func TestAnUpgradeNearExpiryRenewsTheCertificate(t *testing.T) {
	kubeconfig, _ := apiServer(t)
	first := installed(t, "cert-near-expiry", func(s *corev1.Secret) {
		s.Annotations[annotNB] = stamp(time.Now().Add(-345 * day))
		s.Annotations[annotNA] = stamp(time.Now().Add(20 * day))
	})
	renewed(t, first, renderWebhook(t, "cert-near-expiry", kubeconfig))
}

// Every release before 2.0.1 signed a certificate for 10 years and recorded nothing: the first
// upgrade renews it.
func TestAnUpgradeFromBefore201RenewsTheCertificate(t *testing.T) {
	kubeconfig, _ := apiServer(t)
	first := installed(t, "cert-unrecorded", func(s *corev1.Secret) { s.Annotations = nil })
	renewed(t, first, renderWebhook(t, "cert-unrecorded", kubeconfig))
}

// A certificate signed for longer than the duration now configured is renewed.
func TestAnUpgradeRenewsACertificateThatOutlivesTheDuration(t *testing.T) {
	kubeconfig, _ := apiServer(t)
	first := installed(t, "cert-too-long", func(s *corev1.Secret) {
		s.Annotations[annotNB] = stamp(time.Now().Add(-day))
		s.Annotations[annotNA] = stamp(time.Now().Add(3649 * day))
	})
	renewed(t, first, renderWebhook(t, "cert-too-long", kubeconfig))
}

// The previous CA stays in ca.crt until previous-ca-until, and the first upgrade after that
// drops it, leaving the certificate as it is.
func TestThePreviousCAIsDroppedAfterItsGracePeriod(t *testing.T) {
	kubeconfig, client := apiServer(t)
	first := installed(t, "cert-previous-ca", func(s *corev1.Secret) { s.Annotations = nil })
	rotated := renderWebhook(t, "cert-previous-ca", kubeconfig)
	renewed(t, first, rotated)

	ctx := context.Background()
	s := rotated.secret.DeepCopy()
	owned(s, "cert-previous-ca")
	if _, err := client.CoreV1().Secrets(s.Namespace).Update(ctx, s, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("updating the Secret: %v", err)
	}
	kept := renderWebhook(t, s.Namespace, kubeconfig)
	kept.check(t, 365*day)
	if !bytes.Equal(kept.secret.Data["ca.crt"], rotated.secret.Data["ca.crt"]) ||
		kept.secret.Annotations[annotPrevCA] != rotated.secret.Annotations[annotPrevCA] {
		t.Errorf("the previous CA was dropped before previous-ca-until")
	}

	s.Annotations[annotPrevCA] = stamp(time.Now().Add(-time.Minute))
	if _, err := client.CoreV1().Secrets(s.Namespace).Update(ctx, s, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("updating the Secret: %v", err)
	}
	dropped := renderWebhook(t, s.Namespace, kubeconfig)
	_, cas := dropped.check(t, 365*day)
	if len(cas) != 1 || !cas[0].Equal(certificates(t, rotated.secret.Data["ca.crt"])[0]) {
		t.Errorf("ca.crt holds %d CAs after previous-ca-until, want the current one alone", len(cas))
	}
	if _, ok := dropped.secret.Annotations[annotPrevCA]; ok {
		t.Errorf("%s is still set", annotPrevCA)
	}
	for _, k := range []string{"tls.crt", "tls.key"} {
		if !bytes.Equal(dropped.secret.Data[k], rotated.secret.Data[k]) {
			t.Errorf("%s changed when only the previous CA was due to go", k)
		}
	}
}

// With existingSecret the chart signs nothing and asks cert-manager for nothing: the pods
// mount that Secret, and the webhook configurations trust caBundle, or ca.crt of the Secret.
func TestAnExistingSecret(t *testing.T) {
	ca := renderWebhook(t, "subnets", "").secret.Data["ca.crt"]
	expect := func(t *testing.T, r webhookRender, bundle []byte) {
		t.Helper()
		for _, n := range r.names {
			if n == "Secret/"+certSecret || strings.HasPrefix(n, "Certificate/") || strings.HasPrefix(n, "Issuer/") {
				t.Errorf("rendered %s with an existing Secret", n)
			}
		}
		var mounted string
		for _, v := range r.deployment.Spec.Template.Spec.Volumes {
			if v.Name == "webhook-cert" && v.Secret != nil {
				mounted = v.Secret.SecretName
			}
		}
		if mounted != "my-webhook-tls" {
			t.Errorf("the operator mounts %q, want my-webhook-tls", mounted)
		}
		for _, w := range r.validating.Webhooks {
			if !bytes.Equal(w.ClientConfig.CABundle, bundle) {
				t.Errorf("%s: caBundle is not the given CA", w.Name)
			}
		}
		if len(r.validating.Webhooks) == 0 {
			t.Errorf("no validating webhooks")
		}
		if ann := r.validating.Annotations["cert-manager.io/inject-ca-from"]; ann != "" {
			t.Errorf("cert-manager injects %s into an existing Secret's webhooks", ann)
		}
	}

	t.Run("with caBundle", func(t *testing.T) {
		r := renderWebhook(t, "subnets", "", "webhook.certificate.existingSecret=my-webhook-tls",
			"webhook.certificate.certManager=true", "webhook.certificate.caBundle="+string(ca))
		expect(t, r, ca)
	})
	t.Run("without caBundle, under helm template", func(t *testing.T) {
		_, err := helmTemplate(t, "subnets", "", "webhook.certificate.existingSecret=my-webhook-tls")
		if err == nil || !strings.Contains(err.Error(), "set webhook.certificate.caBundle") {
			t.Errorf("got %v, want a missing CA refused", err)
		}
	})
	t.Run("ca.crt from the Secret", func(t *testing.T) {
		kubeconfig, client := apiServer(t)
		ctx := context.Background()
		ns := "cert-existing"
		if _, err := client.CoreV1().Namespaces().Create(ctx,
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("creating namespace: %v", err)
		}
		if _, err := client.CoreV1().Secrets(ns).Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "my-webhook-tls"},
			Data:       map[string][]byte{"ca.crt": ca, "tls.crt": []byte("x"), "tls.key": []byte("x")},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatalf("creating the Secret: %v", err)
		}
		expect(t, renderWebhook(t, ns, kubeconfig, "webhook.certificate.existingSecret=my-webhook-tls"), ca)
	})
}
