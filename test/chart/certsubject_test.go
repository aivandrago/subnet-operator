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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// The names in the webhook certificates (issue #124). Under the chart's self-signed Issuer the
// certificate cert-manager issues is its own CA, and a CA without a name is what RFC 5280
// forbids, so the Certificate asks for one. A commonName holds at most 64 characters:
// cert-manager refuses a Certificate with a longer one, and the chart-signed certificate, which
// nothing refuses, would simply break the same rule.

// maxCommonName is ub-common-name of RFC 5280.
const maxCommonName = 64

// longName is the longest fullname that still gives a valid Service name with "-webhook".
var longName = strings.Repeat("n", 55)

// renderedCertificate is the cert-manager Certificate the chart renders: its spec, and whether
// there is one.
func renderedCertificate(t *testing.T, namespace string, set ...string) (map[string]any, bool) {
	t.Helper()
	out, err := helmTemplate(t, namespace, "", set...)
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	for doc := range strings.SplitSeq(out, "\n---") {
		var object struct {
			Kind string         `json:"kind"`
			Spec map[string]any `json:"spec"`
		}
		if err := yaml.Unmarshal([]byte(doc), &object); err == nil && object.Kind == "Certificate" {
			return object.Spec, true
		}
	}
	return nil, false
}

func TestTheCertManagerCertificateHasASubjectUnderTheChartsIssuer(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  []string
		want string
	}{
		{"by default, the Service's name", nil, "release-subnet-operator-webhook"},
		{"with the longest name a Service can have", []string{"fullnameOverride=" + longName}, longName + "-webhook"},
		// Such a release cannot be installed, its Service name being too long, but the reason
		// should be that and not a Certificate cert-manager refuses.
		{"cut where a name is longer still", []string{"fullnameOverride=" + strings.Repeat("n", 63)},
			strings.Repeat("n", 63) + "-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spec, ok := renderedCertificate(t, strings.Repeat("s", 63),
				append([]string{"webhook.certificate.certManager=true"}, tc.set...)...)
			if !ok {
				t.Fatal("no Certificate rendered")
			}
			cn, _ := spec["commonName"].(string)
			if cn != tc.want {
				t.Errorf("commonName = %q, want %q", cn, tc.want)
			}
			if cn == "" || len(cn) > maxCommonName {
				t.Errorf("commonName %q is %d characters, want 1 to %d", cn, len(cn), maxCommonName)
			}
		})
	}
}

// An issuer of the user's own signs a leaf, whose issuer has a name already. Its Certificate
// stays as 3.0 rendered it: a changed request would have cert-manager issue it again, and what
// a subject may be is that issuer's policy to decide.
func TestTheCertManagerCertificateIsUnchangedUnderAnIssuerOfYourOwn(t *testing.T) {
	spec, ok := renderedCertificate(t, "subnets", "webhook.certificate.certManager=true",
		"webhook.certificate.issuerRef.name=corporate-ca", "webhook.certificate.issuerRef.kind=ClusterIssuer")
	if !ok {
		t.Fatal("no Certificate rendered")
	}
	want := map[string]any{
		"secretName":  certSecret,
		"duration":    "8760h",
		"renewBefore": "720h",
		"dnsNames": []any{
			"release-subnet-operator-webhook.subnets.svc",
			"release-subnet-operator-webhook.subnets.svc.cluster.local",
		},
		"issuerRef": map[string]any{"name": "corporate-ca", "kind": "ClusterIssuer"},
	}
	got, _ := yaml.Marshal(spec)
	wanted, _ := yaml.Marshal(want)
	if string(got) != string(wanted) {
		t.Errorf("the Certificate's spec is\n%s\nwant what 3.0 rendered:\n%s", got, wanted)
	}
}

// The chart-signed certificates always had names, the Service's with a suffix and its DNS
// name, which a long release name or namespace took past 64 characters.
func TestTheChartSignedCertificatesKeepTheirNamesWithin64Characters(t *testing.T) {
	out, err := helmTemplate(t, strings.Repeat("s", 63), "", "fullnameOverride="+longName)
	if err != nil {
		t.Fatalf("helm template: %v", err)
	}
	for doc := range strings.SplitSeq(out, "\n---") {
		secret := &corev1.Secret{}
		if err := yaml.Unmarshal([]byte(doc), secret); err != nil || secret.Kind != "Secret" ||
			secret.Name != longName+"-webhook-cert" {
			continue
		}
		serving := certificates(t, secret.Data["tls.crt"])[0]
		ca := certificates(t, secret.Data["ca.crt"])[0]
		if got, want := serving.Subject.CommonName, longName+"-webhook"; got != want {
			t.Errorf("the serving certificate is named %q, want %q", got, want)
		}
		if got, want := ca.Subject.CommonName, strings.Repeat("n", 55)+"-webho-ca"; got != want {
			t.Errorf("the CA is named %q, want %q", got, want)
		}
		for _, c := range []string{serving.Subject.CommonName, ca.Subject.CommonName} {
			if c == "" || len(c) > maxCommonName {
				t.Errorf("commonName %q is %d characters, want 1 to %d", c, len(c), maxCommonName)
			}
		}
		// What the API server checks is the DNS name, which is as long as it has to be.
		if err := serving.VerifyHostname(longName + "-webhook." + strings.Repeat("s", 63) + ".svc"); err != nil {
			t.Error(err)
		}
		if err := serving.CheckSignatureFrom(ca); err != nil {
			t.Error(err)
		}
		return
	}
	t.Fatal("no chart-signed Secret rendered")
}
