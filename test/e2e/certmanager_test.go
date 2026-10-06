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
	"bufio"
	"context"
	"crypto"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/crdversions"
	"hypersurgery.dev/subnet-operator/test/utils"
)

const (
	// certManagerNamespace is where cert-manager's own manifest installs it.
	certManagerNamespace = "cert-manager"
	// injectCAFrom is the annotation cert-manager's CA injector acts on: namespace/name of the
	// Certificate whose CA it copies into the annotated object.
	injectCAFrom = "cert-manager.io/inject-ca-from"
	// certificateName is the annotation cert-manager puts on a Secret it issued, naming the
	// Certificate it belongs to.
	certificateName = "cert-manager.io/certificate-name"
	// webhookPort is the port the manager serves its webhooks on, in both installs.
	webhookPort = 9443
	// probeLabel marks the scopes these specs create, so they can be removed together.
	probeLabel = "e2e.hypersurgery.dev/certmanager-probe"
	// tagKeyOwner is what the defaulting webhook writes into an AWS scope's spec.tagKeys.owner.
	// The schema has no default for it, so finding it there means the mutating webhook ran.
	tagKeyOwner = "hs/owner"
)

// The cert-manager specs deploy the operator twice with its webhooks served by a certificate
// cert-manager issues: the kustomize install with its [WEBHOOK] and [CERTMANAGER] sections
// uncommented, and the chart with webhook.certificate.certManager=true. For each they check
// that admission and the conversion between v1beta1 and v1 work with the CA cert-manager
// provides, and that both keep working when the certificate is issued again under a new CA,
// with the API server trusting, at every moment, the certificate each pod serves.
// Who puts the CA into the CRDs differs, and is the point of having both: in the kustomize
// install cert-manager's CA injector does, in the chart the operator copies ca.crt itself
// (internal/crdversions), because Helm does not manage the CRDs.
//
// They run on their own, with `make test-e2e-certmanager`: they install cert-manager into the
// cluster and deploy the operator into the namespace the Manager and upgrade specs use, so
// they cannot share a cluster with either.
var _ = Describe("Webhooks with cert-manager, the kustomize install",
	Label("certmanager", "certmanager-kustomize"), Ordered, func() {
		k := &kustomizeInstall{}
		certManagerSpecs(&certManagerInstall{
			certificate: "subnet-operator-serving-cert",
			commonName:  "subnet-operator-webhook",
			secret:      "webhook-server-cert",
			service:     "subnet-operator-webhook-service",
			validating:  "subnet-operator-validating-webhook-configuration",
			mutating:    "subnet-operator-mutating-webhook-configuration",
			deployment:  deploymentName,
			podSelector: "control-plane=controller-manager",
			replicas:    1,
			deploy:      k.deploy,
			remove:      k.remove,
		})
	})

var _ = Describe("Webhooks with cert-manager, the chart", Label("certmanager", "certmanager-chart"), Ordered, func() {
	certManagerSpecs(&certManagerInstall{
		certificate:            upgradeWebhookService,
		commonName:             upgradeWebhookService,
		secret:                 upgradeWebhookService + "-cert",
		service:                upgradeWebhookService,
		validating:             upgradeDeployment,
		mutating:               upgradeDeployment,
		deployment:             upgradeDeployment,
		podSelector:            "app.kubernetes.io/instance=" + upgradeRelease,
		replicas:               2,
		operatorSetsConversion: true,
		deploy:                 deployChartWithCertManager,
		remove:                 removeChart,
	})
})

// certManagerInstall is one way of installing the operator with cert-manager, and what its
// objects are called there.
type certManagerInstall struct {
	// certificate is the cert-manager Certificate of the webhooks, and secret the Secret
	// cert-manager writes it to, which the manager mounts.
	certificate, secret string
	// commonName is the subject the Certificate asks for.
	commonName string
	// service is the Service in front of the webhook server.
	service string
	// validating and mutating are the webhook configurations.
	validating, mutating string
	deployment           string
	podSelector          string
	replicas             int
	// operatorSetsConversion is true where the operator writes the CA into the CRDs'
	// conversion (--crd-conversion=webhook, the chart), false where cert-manager's CA injector
	// does (the kustomize install).
	operatorSetsConversion bool

	deploy, remove func()
	started        bool
}

// certManagerInstalled is true once this run has installed cert-manager into the cluster.
var certManagerInstalled bool

// probeAccount hands every scope the specs create an AWS account of its own: the validating
// webhook refuses a second scope that discovers the same account and region.
var probeAccount = 500000000000

func certManagerSpecs(c *certManagerInstall) {
	BeforeAll(func() {
		if os.Getenv("E2E_CERTMANAGER") != "true" {
			Skip("the cert-manager specs run with `make test-e2e-certmanager` (E2E_CERTMANAGER=true)")
		}
		ensureCertManager()
		// The other install of these specs used the same namespace and CRDs.
		expectGone()
		c.started = true
		c.deploy()
	})
	AfterAll(func() {
		if !c.started {
			return
		}
		c.remove()
	})
	AfterEach(func() {
		if CurrentSpecReport().Failed() {
			c.dump()
		}
		// Every scope is an account the operator syncs; none is needed by the next spec.
		_, _ = utils.Run(exec.Command("kubectl", "delete", resScopes, "-l", probeLabel, "--timeout=2m"))
	})

	It("serves the webhooks with a certificate cert-manager issued", c.expectIssued)
	It("is trusted by the API server through the CA of that certificate, injected by cert-manager", c.expectInjected)
	It("defaults and admits a valid object, refuses an invalid one with the webhook's message, "+
		"and converts between v1beta1 and v1 both ways", func() {
		Eventually(func(g Gomega) { c.expectWebhooksAnswer(g) }, 2*time.Minute, 2*time.Second).Should(Succeed())
	})
	It("keeps admitting and converting after the certificate is issued again under a new CA, with no restart",
		c.expectRotation)
	It("is issued under a subject, without a warning from cert-manager, and the Certificate gaining it "+
		"is one more renewal nobody notices", c.expectSubject)
}

// ensureCertManager installs the pinned cert-manager release once per run and waits until its
// API accepts objects.
func ensureCertManager() {
	GinkgoHelper()
	if certManagerInstalled {
		return
	}
	manifest := os.Getenv("CERT_MANAGER_MANIFEST")
	Expect(manifest).NotTo(BeEmpty(), "CERT_MANAGER_MANIFEST is the cert-manager release manifest; "+
		"`make test-e2e-certmanager` downloads it and checks its checksum")

	By("installing cert-manager from " + filepath.Base(manifest))
	_, err := utils.Run(exec.Command("kubectl", "apply", "-f", manifest))
	Expect(err).NotTo(HaveOccurred(), "Failed to install cert-manager")
	for _, deployment := range []string{"cert-manager", "cert-manager-cainjector", "cert-manager-webhook"} {
		_, err := utils.Run(exec.Command("kubectl", "rollout", "status", "deployment/"+deployment,
			"-n", certManagerNamespace, "--timeout=5m"))
		if err != nil {
			dumpCertManager()
		}
		Expect(err).NotTo(HaveOccurred(), "cert-manager did not roll out")
	}

	// cert-manager's own webhook answers a moment after its pod is ready, once its CA has been
	// injected. A server-side dry run goes through it and creates nothing.
	By("waiting for cert-manager's API to accept an Issuer")
	Eventually(func() error {
		cmd := exec.Command("kubectl", "apply", "--dry-run=server", "-f", "-")
		cmd.Stdin = strings.NewReader(`
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: e2e-api-check
  namespace: default
spec:
  selfSigned: {}
`)
		_, err := utils.Run(cmd)
		return err
	}, 3*time.Minute, 2*time.Second).Should(Succeed())
	certManagerInstalled = true
}

// expectGone waits until neither the operator's namespace nor its CRDs exist, so an install
// starts from an empty cluster and not from what the previous one is still deleting.
func expectGone() {
	GinkgoHelper()
	Eventually(func(g Gomega) {
		out, err := kubectlOut("get", "namespace", namespace, "--ignore-not-found", "-o", "name")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).To(BeEmpty(), "the namespace of a previous install is still there")
		out, err = kubectlOut(append([]string{"get", "crd", "--ignore-not-found", "-o", "name"}, crdsOfTheGroup...)...)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(out).To(BeEmpty(), "CRDs of a previous install are still there")
	}, 4*time.Minute, 2*time.Second).Should(Succeed())
}

// createNamespace creates the operator's namespace with the restricted Pod Security profile,
// as the other specs do.
func createNamespace() {
	GinkgoHelper()
	_, err := utils.Run(exec.Command("kubectl", "create", "ns", namespace))
	Expect(err).NotTo(HaveOccurred(), "Failed to create namespace")
	_, err = utils.Run(exec.Command("kubectl", "label", "--overwrite", "ns", namespace,
		"pod-security.kubernetes.io/enforce=restricted"))
	Expect(err).NotTo(HaveOccurred(), "Failed to label namespace with restricted policy")
}

// kustomizeInstall deploys config/default with the webhook sections uncommented.
type kustomizeInstall struct {
	// manifest is what was applied, kept to delete the same objects again.
	manifest string
}

func (k *kustomizeInstall) deploy() {
	GinkgoHelper()
	project, err := utils.GetProjectDir()
	Expect(err).NotTo(HaveOccurred())

	By("uncommenting the [WEBHOOK] and [CERTMANAGER] sections in a copy of config/")
	// Not GinkgoT().TempDir(): a copy, so the checkout's own config/ stays as committed.
	dir, err := os.MkdirTemp("", "subnet-operator-config-")
	Expect(err).NotTo(HaveOccurred())
	DeferCleanup(os.RemoveAll, dir)
	Expect(os.CopyFS(dir, os.DirFS(filepath.Join(project, "config")))).To(Succeed())
	Expect(utils.EnableKustomizeWebhooks(dir)).To(Succeed())

	By("rendering it with the image under test")
	_, err = kustomize(filepath.Join(dir, "manager"), "edit", "set", "image", "controller="+managerImage)
	Expect(err).NotTo(HaveOccurred())
	k.manifest, err = kustomize(dir, "build", "default")
	Expect(err).NotTo(HaveOccurred())

	By("deploying the controller-manager, its webhooks and their Certificate")
	createNamespace()
	Expect(applyManifest(k.manifest)).To(Succeed(), "Failed to deploy the kustomize install with webhooks")

	By("pointing the controller-manager at Moto")
	_, err = utils.Run(exec.Command("kubectl", "set", "env", "deployment/"+deploymentName, "-n", namespace,
		"AWS_ENDPOINT_URL="+motoClusterEndpoint, "AWS_REGION="+awsRegion,
		"AWS_ACCESS_KEY_ID=test", "AWS_SECRET_ACCESS_KEY=test", "AWS_EC2_METADATA_DISABLED=true"))
	Expect(err).NotTo(HaveOccurred(), "Failed to configure the controller-manager")
	waitForRollout(deploymentName)
}

func (k *kustomizeInstall) remove() {
	if k.manifest == "" {
		return
	}
	cmd := exec.Command("kubectl", "delete", "--ignore-not-found", "--timeout=4m", "-f", "-")
	cmd.Stdin = strings.NewReader(k.manifest)
	_, _ = utils.Run(cmd)
}

// kustomize runs kustomize in dir and returns what it wrote to stdout: a warning on stderr
// must not end up in the manifest.
func kustomize(dir string, args ...string) (string, error) {
	cmd := exec.Command(envOr("KUSTOMIZE", "kustomize"), args...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	_, _ = fmt.Fprintf(GinkgoWriter, "running: kustomize %s (in %s)\n", strings.Join(args, " "), dir)
	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("kustomize %s: %w: %s", strings.Join(args, " "), err, stderr.String())
	}
	return string(out), nil
}

// deployChartWithCertManager installs the chart of the build under test with its cert-manager
// option set outright, not left to `auto`, so the spec fails if the option stops working
// instead of quietly testing the chart-signed certificate.
func deployChartWithCertManager() {
	GinkgoHelper()
	By("writing the chart's values")
	image := strings.SplitN(managerImage, ":", 2)
	values := filepath.Join(GinkgoT().TempDir(), "values.yaml")
	Expect(os.WriteFile(values, []byte(fmt.Sprintf(`
image:
  repository: %s
  tag: %s
providers:
  aws:
    region: %s
    endpointURL: %s
webhook:
  certificate:
    certManager: true
extraEnv:
  - name: AWS_ACCESS_KEY_ID
    value: test
  - name: AWS_SECRET_ACCESS_KEY
    value: test
  - name: AWS_EC2_METADATA_DISABLED
    value: "true"
`, image[0], image[1], awsRegion, motoClusterEndpoint)), 0o600)).To(Succeed())

	By("installing the chart with webhook.certificate.certManager=true")
	createNamespace()
	_, err := utils.Run(exec.Command(envOr("HELM", "helm"), "install", upgradeRelease, localChart,
		"-n", namespace, "-f", values))
	Expect(err).NotTo(HaveOccurred(), "helm install with cert-manager failed")
	waitForRollout(upgradeDeployment)
}

func removeChart() {
	_, _ = utils.Run(exec.Command(envOr("HELM", "helm"), "uninstall", upgradeRelease, "-n", namespace))
	_, _ = utils.Run(exec.Command("kubectl", "delete", "-f", localChart+"/crds/", "--ignore-not-found", "--timeout=4m"))
	_, _ = utils.Run(exec.Command("kubectl", "delete", "ns", namespace, "--timeout=4m"))
}

// expectIssued checks that the certificate the manager serves comes from cert-manager.
func (c *certManagerInstall) expectIssued() {
	By("finding every Certificate of the install ready")
	Eventually(func(g Gomega) {
		out, err := kubectlOut("get", "certificates.cert-manager.io", "-n", namespace, "-o",
			`jsonpath={range .items[*]}{.metadata.name}={.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}`)
		g.Expect(err).NotTo(HaveOccurred())
		lines := utils.GetNonEmptyLines(out)
		g.Expect(lines).To(ContainElement(c.certificate+"=True"), "the webhook Certificate is not ready")
		for _, line := range lines {
			g.Expect(line).To(HaveSuffix("=True"), "a Certificate cert-manager could not issue")
		}
	}, 3*time.Minute, 2*time.Second).Should(Succeed())

	By("finding the mounted Secret written by cert-manager for that Certificate")
	secret := &corev1.Secret{}
	Expect(getNamespaced("secret", namespace, c.secret, secret)).To(Succeed())
	Expect(secret.Annotations).To(HaveKeyWithValue(certificateName, c.certificate))
	// The chart's own certificate records when it was signed; cert-manager's does not.
	Expect(secret.Annotations).NotTo(HaveKey("network.hypersurgery.dev/webhook-not-after"))
	_, leaf := c.webhookCertificate(Default)
	Expect(leaf.DNSNames).To(ContainElement(c.serverName()))

	By("finding that certificate served by every manager pod")
	Eventually(func(g Gomega) { c.expectServed(g, leaf) }, 2*time.Minute, 2*time.Second).Should(Succeed())
}

// expectInjected checks who gave the API server the CA: cert-manager's injector for the
// admission webhooks in both installs; for the CRDs' conversion the injector in the kustomize
// install and the operator in the chart.
func (c *certManagerInstall) expectInjected() {
	ca, _ := c.webhookCertificate(Default)
	want := namespace + "/" + c.certificate

	By("finding the CA of the Certificate in every webhook and in every CRD's conversion")
	// On a first issuance there is no earlier CA, so the bundles are ca.crt and nothing else.
	exactly := func(g Gomega, bundle []byte, where string) {
		g.Expect(string(bundle)).To(Equal(string(ca)), where)
	}
	Eventually(func(g Gomega) { c.expectTrusted(g, exactly) }, 3*time.Minute, 2*time.Second).Should(Succeed())

	By("finding the webhook configurations annotated for, and written by, cert-manager's CA injector")
	for kind, name := range map[string]string{
		"validatingwebhookconfiguration": c.validating,
		"mutatingwebhookconfiguration":   c.mutating,
	} {
		Expect(annotation(kind, name, injectCAFrom)).To(Equal(want), kind)
		Expect(fieldManagers(kind, name)).To(ContainSubstring("cainjector"), kind)
	}

	args, err := kubectlOut("get", "deployment", c.deployment, "-n", namespace,
		"-o", "jsonpath={.spec.template.spec.containers[0].args}")
	Expect(err).NotTo(HaveOccurred())
	// The Events of the CRDs as they are now, by UID: the other install of these specs had its
	// own CRDs under the same names, and its Events outlive them.
	configuredByOperator := func(g Gomega, crd string) bool {
		uid, err := kubectlOut("get", "crd", crd, "-o", "jsonpath={.metadata.uid}")
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(uid).NotTo(BeEmpty())
		events, err := kubectlOut("get", "events", "-A", "--field-selector",
			"reason="+crdversions.EventConversionConfigured, "-o", "jsonpath={.items[*].involvedObject.uid}")
		g.Expect(err).NotTo(HaveOccurred())
		return strings.Contains(events, uid)
	}
	if c.operatorSetsConversion {
		By("finding the CRDs' conversion written by the operator, not by the CA injector")
		Expect(args).To(ContainSubstring("--crd-conversion=" + crdversions.ConversionWebhook))
		for _, crd := range crdsOfTheGroup {
			Expect(annotation("crd", crd, injectCAFrom)).To(BeEmpty(), crd)
			Expect(fieldManagers("crd", crd)).NotTo(ContainSubstring("cainjector"), crd)
			// The Event is recorded after the patch, and not in the same breath.
			Eventually(func(g Gomega) {
				g.Expect(configuredByOperator(g, crd)).To(BeTrue(), "no ConversionConfigured Event on %s", crd)
			}, time.Minute, 2*time.Second).Should(Succeed())
		}
		return
	}
	By("finding the CRDs' conversion written by the CA injector, and left alone by the operator")
	Expect(args).NotTo(ContainSubstring("--crd-conversion"))
	for _, crd := range crdsOfTheGroup {
		Expect(annotation("crd", crd, injectCAFrom)).To(Equal(want), crd)
		Expect(fieldManagers("crd", crd)).To(ContainSubstring("cainjector"), crd)
		Expect(configuredByOperator(Default, crd)).To(BeFalse(), "the operator configured the conversion of %s", crd)
	}
}

// expectRotation has cert-manager issue the certificate again by deleting its Secret. The key
// is gone with the Secret, and both installs use a self-signed Issuer, so the new certificate
// is its own, new CA: nothing the API server trusted before verifies it.
func (c *certManagerInstall) expectRotation() {
	c.expectIssuedAgain("deleting the Secret of the webhook certificate", func() {
		_, err := utils.Run(exec.Command("kubectl", "delete", "secret", c.secret, "-n", namespace))
		Expect(err).NotTo(HaveOccurred())
	})
}

// expectSubject walks the Certificate through what the upgrade from 3.0 does to it: 3.0 asked
// for no subject, this release asks for a commonName, and a Certificate whose request changed
// is issued again (#124). So the spec first takes the commonName away, which gives the
// Certificate 3.0 had and the warning cert-manager attached to it, and then puts it back,
// which is the upgrade. Both are a new certificate under a new CA, with the operator running:
// neither may be noticed by a client.
//
// What it cannot show is the upgrade's own first minute, in which the replica that holds the
// lease is still a 3.0 one (docs/operations/upgrades.md).
func (c *certManagerInstall) expectSubject() {
	badConfig := func(g Gomega, request string) string {
		out, err := kubectlOut("get", "events", "-n", namespace, "--field-selector",
			"reason=BadConfig,involvedObject.kind=CertificateRequest,involvedObject.name="+request,
			"-o", "jsonpath={.items[*].message}")
		g.Expect(err).NotTo(HaveOccurred())
		return out
	}

	By("finding the certificate issued with the subject the Certificate asks for, and no warning about it")
	_, leaf := c.webhookCertificate(Default)
	Expect(leaf.Subject.CommonName).To(Equal(c.commonName))
	Expect(leaf.Issuer.CommonName).To(Equal(c.commonName), "the certificate is not its own issuer")
	Expect(badConfig(Default, c.currentRequest(Default))).To(BeEmpty())

	old := c.expectIssuedAgain("taking the commonName away, as in the Certificate of 3.0", func() {
		_, err := utils.Run(exec.Command("kubectl", "patch", "certificates.cert-manager.io", c.certificate,
			"-n", namespace, "--type=json", "-p", `[{"op":"remove","path":"/spec/commonName"}]`))
		Expect(err).NotTo(HaveOccurred())
	})
	Expect(old.Subject.String()).To(BeEmpty(), "the Certificate of 3.0 had no subject")
	// The warning this spec exists for: if cert-manager stops giving it, the check below it
	// proves nothing any more.
	Eventually(func(g Gomega) {
		g.Expect(badConfig(g, c.currentRequest(g))).To(ContainSubstring("RFC 5280"))
	}, time.Minute, 2*time.Second).Should(Succeed())

	renewed := c.expectIssuedAgain("giving the Certificate its commonName, as the upgrade to this release does", func() {
		_, err := utils.Run(exec.Command("kubectl", "patch", "certificates.cert-manager.io", c.certificate,
			"-n", namespace, "--type=merge", "-p", fmt.Sprintf(`{"spec":{"commonName":%q}}`, c.commonName)))
		Expect(err).NotTo(HaveOccurred())
	})
	Expect(renewed.Subject.CommonName).To(Equal(c.commonName))
	Expect(renewed.Issuer.CommonName).To(Equal(c.commonName))
	// Events are recorded after the fact; one that is coming would be there within seconds.
	Consistently(func(g Gomega) {
		g.Expect(badConfig(g, c.currentRequest(g))).To(BeEmpty())
	}, 10*time.Second, 2*time.Second).Should(Succeed())
	events, err := kubectlOut("get", "events", "-n", namespace, "--field-selector", "reason=BadConfig",
		"-o", `jsonpath={range .items[*]}{.involvedObject.name}: {.message}{"\n"}{end}`)
	Expect(err).NotTo(HaveOccurred())
	_, _ = fmt.Fprintf(GinkgoWriter, "BadConfig events in the namespace, all of the Certificate without a subject:\n%s\n",
		events)
}

// currentRequest is the CertificateRequest of the Certificate's current revision, which
// cert-manager names after both.
func (c *certManagerInstall) currentRequest(g Gomega) string {
	GinkgoHelper()
	revision, err := kubectlOut("get", "certificates.cert-manager.io", c.certificate, "-n", namespace,
		"-o", "jsonpath={.status.revision}")
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(revision).NotTo(BeEmpty())
	return c.certificate + "-" + revision
}

// expectIssuedAgain has cert-manager issue the webhook certificate again, by whatever trigger
// does, and returns the new certificate. Everything must find its way to the new CA without
// anyone restarting a pod: the CA injector adds it to the bundles of the webhook configurations
// (and, in the kustomize install, of the CRDs), in the chart the operator adds it to the CRDs
// when the Secret changes, the kubelet refreshes the mounted Secret a minute or so later, and
// the manager reloads the certificate it serves.
//
// Those steps do not happen at once, and the order is what matters: the API server has to hold
// the new CA before the first pod serves under it, and the previous one until the last pod has
// stopped. The spec samples exactly that while the renewal goes on (watchTrust) and fails on a
// single sample in which a pod serves a certificate a CRD's bundle does not verify; it also
// lists at v1beta1 every two seconds, through the conversion webhook, and fails if a list did.
// Where it ends: every bundle verifies the new certificate, every pod serves it, admission and
// conversion answer, and the pods are the ones it started with.
func (c *certManagerInstall) expectIssuedAgain(by string, trigger func()) *x509.Certificate {
	GinkgoHelper()
	oldCA, oldLeaf := c.webhookCertificate(Default)
	pods := c.pods(Default)
	revision := func(g Gomega) string {
		out, err := kubectlOut("get", "certificates.cert-manager.io", c.certificate, "-n", namespace, "-o",
			`jsonpath={.status.revision} {.status.conditions[?(@.type=="Ready")].status}`)
		g.Expect(err).NotTo(HaveOccurred())
		return out
	}
	before := revision(Default)
	Expect(before).To(HaveSuffix(" True"))

	// A scope to read at v1beta1 while the renewal goes on: a list of nothing converts nothing.
	Expect(applyManifest(probeScope("v1", probeName("watched"), awsRegion))).To(Succeed())

	start := time.Now()
	since := func() time.Duration { return time.Since(start).Round(time.Second) }
	// Running before the trigger, so the first samples are of the state the renewal starts from.
	trust := c.watchTrust(start)
	defer trust.stop()
	// Not an assertion: admission is trusted through cert-manager's CA injector in both
	// installs, and what it does in the middle of a renewal is cert-manager's to promise. What
	// the calls did goes to the log.
	admission := watchCalls(start, func() error {
		cmd := exec.Command("kubectl", "apply", "--dry-run=server", "-f", "-")
		cmd.Stdin = strings.NewReader(probeScope("v1", "certs-dry-run-599999999999", awsRegion))
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		return nil
	})
	conversion := watchCalls(start, func() error {
		out, err := exec.Command("kubectl", "get", betaResource(resScopes), "-l", probeLabel, "-o", "name").
			CombinedOutput()
		if err != nil {
			return fmt.Errorf("%w: %s", err, out)
		}
		return nil
	})
	defer admission.stop()
	defer conversion.stop()

	By(by)
	trigger()

	By("finding a new certificate from cert-manager, under a new CA")
	var newCA []byte
	var newLeaf *x509.Certificate
	Eventually(func(g Gomega) {
		// A new revision, issued: not a Secret cert-manager is still in the middle of writing.
		now := revision(g)
		g.Expect(now).To(HaveSuffix(" True"))
		g.Expect(now).NotTo(Equal(before))
		newCA, newLeaf = c.webhookCertificate(g)
		g.Expect(string(newCA)).NotTo(Equal(string(oldCA)))
	}, 3*time.Minute, time.Second).Should(Succeed())
	issued := since()
	equal, ok := oldLeaf.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	Expect(ok).To(BeTrue(), "an unexpected key type: %T", oldLeaf.PublicKey)
	Expect(equal.Equal(newLeaf.PublicKey)).To(BeFalse(), "the certificate was issued again for the same key")
	Expect(verify(newLeaf, oldCA, c.serverName())).NotTo(Succeed(), "the previous CA still verifies the new certificate")
	Expect(verify(newLeaf, newCA, c.serverName())).To(Succeed())

	verifiesNew := func(g Gomega, bundle []byte, where string) {
		g.Expect(verify(newLeaf, bundle, c.serverName())).To(Succeed(), where)
	}
	// The pods serve the previous certificate until the kubelet refreshes their mounts, so a
	// bundle has to keep verifying it for now.
	verifiesBoth := func(g Gomega, bundle []byte, where string) {
		verifiesNew(g, bundle, where)
		g.Expect(verify(oldLeaf, bundle, c.serverName())).To(Succeed(), "%s dropped the previous CA", where)
	}
	describe := func(bundle []byte) string {
		keeps := "does not trust"
		if verify(oldLeaf, bundle, c.serverName()) == nil {
			keeps = "still trusts"
		}
		return fmt.Sprintf("%d certificate(s), %s the previous one",
			strings.Count(string(bundle), "BEGIN CERTIFICATE"), keeps)
	}

	By("finding the new CA next to the previous one in the webhook configurations and in every CRD's conversion")
	Eventually(func(g Gomega) { c.expectTrusted(g, verifiesBoth) }, 2*time.Minute, time.Second).Should(Succeed())
	trusted := since()

	By("finding the new certificate served by every pod")
	Eventually(func(g Gomega) { c.expectServed(g, newLeaf) }, 4*time.Minute, 2*time.Second).Should(Succeed())
	served := since()

	By("finding both CAs still there, for a pod the kubelet would have reached later")
	c.expectTrusted(Default, verifiesBoth)

	By("admitting, refusing and converting again")
	Eventually(func(g Gomega) { c.expectWebhooksAnswer(g) }, 2*time.Minute, 2*time.Second).Should(Succeed())
	_, _ = fmt.Fprintf(GinkgoWriter, "Renewal timeline, from the trigger: new certificate issued after %s, new CA in "+
		"the webhook configurations and in every CRD after %s, new certificate served by every pod after %s, "+
		"admission and conversion answering after %s\n", issued, trusted, served, since())
	validating := &admissionregistrationv1.ValidatingWebhookConfiguration{}
	Expect(getObject("validatingwebhookconfiguration", c.validating, validating)).To(Succeed())
	crd := &apiextensionsv1.CustomResourceDefinition{}
	Expect(getObject("crd", resScopes, crd)).To(Succeed())
	_, _ = fmt.Fprintf(GinkgoWriter, "CA bundles after the renewal: the validating webhooks hold %s; the CRDs' "+
		"conversion holds %s\n", describe(validating.Webhooks[0].ClientConfig.CABundle),
		describe(crd.Spec.Conversion.Webhook.ClientConfig.CABundle))

	By("finding that no pod ever served a certificate the CRDs' conversion did not trust")
	_, _ = fmt.Fprintf(GinkgoWriter, "Meanwhile, %s\n", trust.stop())
	Expect(trust.samples).To(BeNumerically(">", 10), "too few samples to say anything")
	Expect(trust.untrusted).To(BeEmpty())

	By("finding that no list at v1beta1 failed")
	_, _ = fmt.Fprintf(GinkgoWriter, "Meanwhile, creates at v1 (server-side dry run, through the admission "+
		"webhooks): %s\nMeanwhile, lists at v1beta1 (through the conversion webhook): %s\n",
		admission.stop(), conversion.stop())
	Expect(conversion.failures).To(BeZero(), conversion.summary)

	By("finding the same pods, never restarted")
	Expect(c.pods(Default)).To(Equal(pods), "the manager pods were replaced or restarted")
	return newLeaf
}

// trustWatch samples, in the background, whether the API server could verify the certificate
// each pod serves with the CA bundle of every CRD's conversion.
type trustWatch struct {
	cancel context.CancelFunc
	done   chan struct{}
	// samples counts the samples that read the bundles and every ready pod's certificate, and
	// unread those that could not read all of it (a port forward that failed, say).
	samples, unread int
	// untrusted is one line for every sample in which a pod served a certificate that a CRD's
	// bundle did not verify.
	untrusted []string
	// serials is the certificates the pods were seen serving, and when each was first seen.
	serials []string
}

// watchTrust samples until stop. One sample reads the CRDs' bundles, then the certificate of
// every pod, then the bundles again, and counts a certificate as untrusted only if neither
// reading of a CRD's bundle verifies it: the bundle changes during a renewal, and a
// certificate read between the two readings may be newer than the first.
func (c *certManagerInstall) watchTrust(start time.Time) *trustWatch {
	ctx, cancel := context.WithCancel(context.Background())
	w := &trustWatch{cancel: cancel, done: make(chan struct{})}
	bundles := func() (map[string][]byte, error) {
		list := &apiextensionsv1.CustomResourceDefinitionList{}
		if err := getList(list, append([]string{"crd"}, crdsOfTheGroup...)...); err != nil {
			return nil, err
		}
		out := map[string][]byte{}
		for _, crd := range list.Items {
			conversion := crd.Spec.Conversion
			if conversion == nil || conversion.Webhook == nil || conversion.Webhook.ClientConfig == nil {
				return nil, fmt.Errorf("%s does not convert through a webhook", crd.Name)
			}
			out[crd.Name] = conversion.Webhook.ClientConfig.CABundle
		}
		if len(out) != len(crdsOfTheGroup) {
			return nil, fmt.Errorf("%d CRDs, want %d", len(out), len(crdsOfTheGroup))
		}
		return out, nil
	}
	seen := map[string]bool{}
	sample := func() error {
		at := time.Since(start).Round(time.Second)
		before, err := bundles()
		if err != nil {
			return err
		}
		pods := &corev1.PodList{}
		if err := getList(pods, "pods", "-n", namespace, "-l", c.podSelector); err != nil {
			return err
		}
		// A pod that cannot be asked does not hide what the others serve.
		var unread error
		served := map[string]*x509.Certificate{}
		for _, pod := range pods.Items {
			// The Service sends the API server to ready pods only.
			if pod.DeletionTimestamp != nil || !podReady(&pod) {
				continue
			}
			leaf, err := servedCertificate(pod.Name)
			if err != nil {
				unread = err
				continue
			}
			served[pod.Name] = leaf
		}
		after, err := bundles()
		if err != nil {
			return err
		}
		for pod, leaf := range served {
			if serial := leaf.SerialNumber.String(); !seen[pod+serial] {
				seen[pod+serial] = true
				w.serials = append(w.serials, fmt.Sprintf("%s serving serial %s from %s on", pod, serial, at))
			}
			for crd := range before {
				first, second := verify(leaf, before[crd], c.serverName()), verify(leaf, after[crd], c.serverName())
				if first != nil && second != nil {
					w.untrusted = append(w.untrusted, fmt.Sprintf("%s after the trigger, %s served the certificate with "+
						"serial %s, which the CA bundle of %s did not verify: %v", at, pod, leaf.SerialNumber, crd, second))
				}
			}
		}
		return unread
	}
	go func() {
		defer close(w.done)
		for ctx.Err() == nil {
			if err := sample(); err != nil {
				w.unread++
			} else {
				w.samples++
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
	}()
	return w
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// stop ends the sampling and says what it saw. It may be called more than once.
func (w *trustWatch) stop() string {
	w.cancel()
	<-w.done
	return fmt.Sprintf("%d sample(s) of every pod's certificate against every CRD's CA bundle (%d more could not be "+
		"read), %d of them untrusted; %s", w.samples, w.unread, len(w.untrusted), strings.Join(w.serials, "; "))
}

// callWatch repeats one call in the background and remembers how it went.
type callWatch struct {
	cancel context.CancelFunc
	done   chan struct{}
	// failures and summary are for after stop.
	failures int
	summary  string
}

// watchCalls makes call every two seconds until stop, and counts its failures and when, from
// start, the first and the last of them happened.
func watchCalls(start time.Time, call func() error) *callWatch {
	ctx, cancel := context.WithCancel(context.Background())
	w := &callWatch{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		var calls int
		var first, last time.Duration
		var example string
		for ctx.Err() == nil {
			at := time.Since(start).Round(time.Second)
			calls++
			if err := call(); err != nil && ctx.Err() == nil {
				w.failures++
				if w.failures == 1 {
					first, example = at, strings.TrimSpace(err.Error())
				}
				last = at
			}
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
			}
		}
		w.summary = fmt.Sprintf("%d call(s), none failed", calls)
		if w.failures > 0 {
			w.summary = fmt.Sprintf("%d call(s), %d failed, the first %s and the last %s after the start, with: %s",
				calls, w.failures, first, last, example)
		}
	}()
	return w
}

// stop ends the calls and says how they went. It may be called more than once.
func (w *callWatch) stop() string {
	w.cancel()
	<-w.done
	return w.summary
}

// expectWebhooksAnswer creates scopes through the webhooks and reads them back at the other
// version. It creates new objects on every call, so it can be retried while the webhooks are
// not reachable yet: an object the defaulting webhook missed (it is failurePolicy Ignore) on
// one attempt does not decide the next.
func (c *certManagerInstall) expectWebhooksAnswer(g Gomega) {
	betaName, v1Name, refusedName := probeName("beta"), probeName("v1"), probeName("refused")

	// Written at v1beta1: the API server converts the request to v1 for the admission webhooks,
	// which are registered for v1 only, and stores it at v1.
	g.Expect(applyManifest(probeScope("v1beta1", betaName, awsRegion))).To(Succeed())
	atV1 := map[string]any{}
	g.Expect(getObject(resScopes, betaName, &atV1)).To(Succeed())
	g.Expect(atV1["apiVersion"]).To(Equal(networkv1.GroupVersion.String()))
	scope := &networkv1.NetworkScope{}
	g.Expect(getObject(resScopes, betaName, scope)).To(Succeed())
	g.Expect(scope.Spec.Regions).To(Equal([]string{awsRegion}))
	g.Expect(scope.Spec.TagKeys.Owner).To(Equal(tagKeyOwner), "the defaulting webhook did not run")

	// Written at v1, read at v1beta1: stored at v1, so the read is a conversion.
	g.Expect(applyManifest(probeScope("v1", v1Name, awsRegion))).To(Succeed())
	stored, atBeta := map[string]any{}, map[string]any{}
	g.Expect(getObject(resScopes, v1Name, &stored)).To(Succeed())
	cmd := exec.Command("kubectl", "get", betaResource(resScopes), v1Name, "-o", "json")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	g.Expect(err).NotTo(HaveOccurred(), stderr.String())
	g.Expect(json.Unmarshal(raw, &atBeta)).To(Succeed())
	g.Expect(atBeta["apiVersion"]).To(Equal(betaGroupVersion))
	g.Expect(atBeta["spec"]).To(Equal(stored["spec"]), "the scope reads differently at v1beta1")
	g.Expect(stderr.String()).To(ContainSubstring("network.hypersurgery.dev/v1beta1 is deprecated"))

	// The v1beta1 schema lists regions as atomic, so only the validating webhook, reached
	// after the conversion, can refuse the duplicate; and it is the only thing wrong.
	cmd = exec.Command("kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(probeScope("v1beta1", refusedName, awsRegion+", "+awsRegion))
	out, err := utils.Run(cmd)
	g.Expect(err).To(HaveOccurred(), "a scope with a duplicate region was admitted:\n%s", out)
	// The validator answers with the same kind of error as the API server's own validation
	// (Invalid, with the field), so kubectl prints it without naming a webhook. utils.Run
	// quotes the output, hence no names or values in the substring.
	g.Expect(err.Error()).To(ContainSubstring("is invalid: spec.regions[1]: Duplicate value: "))
	_, err = kubectlOut("get", resScopes, refusedName)
	g.Expect(err).To(MatchError(ContainSubstring("NotFound")))
}

// probeName names a scope no earlier attempt has used.
func probeName(what string) string {
	probeAccount++
	return fmt.Sprintf("certs-%s-%d", what, probeAccount)
}

// probeScope is the smallest scope the webhooks admit, in the account its name ends with.
func probeScope(version, name, regions string) string {
	return fmt.Sprintf(`
apiVersion: network.hypersurgery.dev/%s
kind: NetworkScope
metadata:
  name: %s
  labels:
    %s: "true"
spec:
  provider: AWS
  accounts:
    - id: "%s"
  regions: [%s]
`, version, name, probeLabel, name[strings.LastIndex(name, "-")+1:], regions)
}

// serverName is the name the API server calls the webhooks by, which the certificate is for.
func (c *certManagerInstall) serverName() string {
	return c.service + "." + namespace + ".svc"
}

// webhookCertificate reads the Secret cert-manager wrote: ca.crt as it is, which is what ends
// up in the CA bundles, and the serving certificate, parsed.
func (c *certManagerInstall) webhookCertificate(g Gomega) ([]byte, *x509.Certificate) {
	GinkgoHelper()
	secret := &corev1.Secret{}
	g.Expect(getNamespaced("secret", namespace, c.secret, secret)).To(Succeed())
	g.Expect(secret.Data["ca.crt"]).NotTo(BeEmpty(), "cert-manager wrote no ca.crt")
	block, _ := pem.Decode(secret.Data["tls.crt"])
	g.Expect(block).NotTo(BeNil(), "tls.crt is not PEM")
	leaf, err := x509.ParseCertificate(block.Bytes)
	g.Expect(err).NotTo(HaveOccurred())
	return secret.Data["ca.crt"], leaf
}

// verify checks a serving certificate the way the API server does: against the CA bundle it
// was given and the name of the Service.
func verify(leaf *x509.Certificate, caBundle []byte, serverName string) error {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caBundle) {
		return fmt.Errorf("no certificate in the CA bundle")
	}
	_, err := leaf.Verify(x509.VerifyOptions{Roots: roots, DNSName: serverName})
	return err
}

// trusts checks one CA bundle the API server holds; where says whose it is.
type trusts func(g Gomega, bundle []byte, where string)

// expectAdmissionTrusted checks the CA bundle of every webhook of both configurations.
func (c *certManagerInstall) expectAdmissionTrusted(g Gomega, check trusts) {
	GinkgoHelper()
	validating := &admissionregistrationv1.ValidatingWebhookConfiguration{}
	g.Expect(getObject("validatingwebhookconfiguration", c.validating, validating)).To(Succeed())
	g.Expect(validating.Webhooks).To(HaveLen(3))
	for _, w := range validating.Webhooks {
		check(g, w.ClientConfig.CABundle, w.Name)
	}
	mutating := &admissionregistrationv1.MutatingWebhookConfiguration{}
	g.Expect(getObject("mutatingwebhookconfiguration", c.mutating, mutating)).To(Succeed())
	g.Expect(mutating.Webhooks).To(HaveLen(3))
	for _, w := range mutating.Webhooks {
		check(g, w.ClientConfig.CABundle, w.Name)
	}
}

// expectTrusted checks the CA bundle of the admission webhooks and of every CRD's conversion,
// and that the conversion goes to the install's webhook Service.
func (c *certManagerInstall) expectTrusted(g Gomega, check trusts) {
	GinkgoHelper()
	c.expectAdmissionTrusted(g, check)
	for _, name := range crdsOfTheGroup {
		crd := &apiextensionsv1.CustomResourceDefinition{}
		g.Expect(getObject("crd", name, crd)).To(Succeed())
		conversion := crd.Spec.Conversion
		g.Expect(conversion).NotTo(BeNil(), name)
		g.Expect(conversion.Strategy).To(Equal(apiextensionsv1.WebhookConverter), name)
		g.Expect(conversion.Webhook).NotTo(BeNil(), name)
		g.Expect(conversion.Webhook.ClientConfig).NotTo(BeNil(), name)
		service := conversion.Webhook.ClientConfig.Service
		g.Expect(service).NotTo(BeNil(), name)
		g.Expect(service.Namespace+"/"+service.Name).To(Equal(namespace+"/"+c.service), name)
		g.Expect(service.Path).To(HaveValue(Equal(crdversions.ConvertPath)), name)
		check(g, conversion.Webhook.ClientConfig.CABundle, name)
	}
}

// pods is the manager's pods, as name to UID and restart count: what must not change for a
// rotation to have needed no restart.
func (c *certManagerInstall) pods(g Gomega) map[string]string {
	GinkgoHelper()
	list := &corev1.PodList{}
	g.Expect(getList(list, "pods", "-n", namespace, "-l", c.podSelector)).To(Succeed())
	out := map[string]string{}
	for _, pod := range list.Items {
		if pod.DeletionTimestamp != nil {
			continue
		}
		restarts := int32(0)
		for _, status := range pod.Status.ContainerStatuses {
			restarts += status.RestartCount
		}
		out[pod.Name] = fmt.Sprintf("%s, %d restart(s)", pod.UID, restarts)
	}
	g.Expect(out).To(HaveLen(c.replicas), "the manager's pods: %v", out)
	return out
}

// expectServed checks that every manager pod serves leaf on its webhook port.
func (c *certManagerInstall) expectServed(g Gomega, leaf *x509.Certificate) {
	GinkgoHelper()
	for pod := range c.pods(g) {
		served, err := servedCertificate(pod)
		g.Expect(err).NotTo(HaveOccurred(), pod)
		g.Expect(served.Equal(leaf)).To(BeTrue(), "%s serves the certificate with serial %s, not %s",
			pod, served.SerialNumber, leaf.SerialNumber)
	}
}

// servedCertificate asks one pod's webhook server for its certificate, through a port
// forward: the Service would answer with whichever pod it picked.
func servedCertificate(pod string) (*x509.Certificate, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "kubectl", "port-forward", "-n", namespace, "pod/"+pod,
		fmt.Sprintf(":%d", webhookPort))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	defer func() {
		cancel()
		_ = cmd.Wait()
	}()

	// kubectl prints "Forwarding from 127.0.0.1:<port> -> 9443" once the local port is open.
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("kubectl port-forward to %s: %w: %s", pod, err, stderr.String())
	}
	m := regexp.MustCompile(`127\.0\.0\.1:(\d+)`).FindStringSubmatch(line)
	if m == nil {
		return nil, fmt.Errorf("no local port in %q", line)
	}
	// The certificate is what is being read, so there is nothing to verify it against yet;
	// the caller compares it with the one cert-manager issued.
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 10 * time.Second}, "tcp", "127.0.0.1:"+m[1],
		&tls.Config{InsecureSkipVerify: true}) // #nosec G402 -- reads the certificate; the caller compares it.
	if err != nil {
		return nil, fmt.Errorf("TLS to %s: %w", pod, err)
	}
	defer func() { _ = conn.Close() }()
	return conn.ConnectionState().PeerCertificates[0], nil
}

// annotation reads one annotation of a cluster-scoped object; empty when it has none.
func annotation(kind, name, key string) string {
	GinkgoHelper()
	out, err := kubectlOut("get", kind, name, "-o", "go-template={{ index .metadata.annotations \""+key+"\" }}")
	Expect(err).NotTo(HaveOccurred())
	if out == "<no value>" {
		return ""
	}
	return out
}

// fieldManagers lists who wrote a cluster-scoped object, as the API server recorded it.
func fieldManagers(kind, name string) string {
	GinkgoHelper()
	out, err := kubectlOut("get", kind, name, "--show-managed-fields",
		"-o", "jsonpath={.metadata.managedFields[*].manager}")
	Expect(err).NotTo(HaveOccurred())
	return out
}

// dump prints what the install and cert-manager are doing, so a failure says why.
func (c *certManagerInstall) dump() {
	for _, args := range [][]string{
		{"get", "pods,certificates.cert-manager.io,certificaterequests.cert-manager.io,issuers.cert-manager.io,secrets",
			"-n", namespace, "-o", "wide"},
		{"describe", "certificates.cert-manager.io", "-n", namespace},
		{"describe", "deployment/" + c.deployment, "-n", namespace},
		{"get", "events", "-n", namespace, "--sort-by=.lastTimestamp"},
		{"logs", "-n", namespace, "-l", c.podSelector, "--all-containers", "--tail=200", "--prefix"},
		{"get", "validatingwebhookconfiguration/" + c.validating, "mutatingwebhookconfiguration/" + c.mutating,
			"--show-managed-fields", "-o", "yaml"},
		{"get", "crd", resScopes, "--show-managed-fields",
			"-o", "jsonpath={.metadata.annotations}{\"\\n\"}{.spec.conversion}{\"\\n\"}{.metadata.managedFields[*].manager}"},
	} {
		out, err := utils.Run(exec.Command("kubectl", args...))
		_, _ = fmt.Fprintf(GinkgoWriter, "\n--- kubectl %s ---\n%s\nerr: %v\n", strings.Join(args, " "), out, err)
	}
	dumpCertManager()
}

// dumpCertManager prints cert-manager's own state.
func dumpCertManager() {
	for _, args := range [][]string{
		{"get", "pods", "-n", certManagerNamespace, "-o", "wide"},
		{"get", "events", "-n", certManagerNamespace, "--sort-by=.lastTimestamp"},
		{"logs", "-n", certManagerNamespace, "deployment/cert-manager", "--tail=100"},
		{"logs", "-n", certManagerNamespace, "deployment/cert-manager-cainjector", "--tail=100"},
	} {
		out, err := utils.Run(exec.Command("kubectl", args...))
		_, _ = fmt.Fprintf(GinkgoWriter, "\n--- kubectl %s ---\n%s\nerr: %v\n", strings.Join(args, " "), out, err)
	}
}
