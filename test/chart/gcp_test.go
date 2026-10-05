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
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// The operator's own Google identity comes in three shapes: whatever the environment offers
// (nothing rendered), GKE Workload Identity (a service account annotation) and Workload
// Identity Federation (a credential configuration and a projected token).

const (
	wifProvider = "projects/123456789012/locations/global/workloadIdentityPools/clusters/providers/prod-eks"
	gcpOperator = "subnet-operator@ops-project.iam.gserviceaccount.com"
)

func operatorServiceAccount(t *testing.T, r rendered) corev1.ServiceAccount {
	t.Helper()
	for _, sa := range r.serviceAccounts {
		if strings.HasSuffix(sa.Name, "subnet-operator") {
			return sa
		}
	}
	t.Fatal("no operator service account")
	return corev1.ServiceAccount{}
}

func operatorPod(t *testing.T, r rendered) corev1.PodTemplateSpec {
	t.Helper()
	return r.deployment(t, "subnet-operator").Spec.Template
}

func envOf(c corev1.Container) map[string]string {
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	return env
}

func refuses(t *testing.T, want string, set ...string) {
	t.Helper()
	args := make([]string, 0, 3+2*len(set))
	args = append(args, "template", "release", chartDir)
	for _, s := range set {
		args = append(args, "--set", s)
	}
	out, err := exec.Command(helmBinary(t), args...).CombinedOutput()
	if err == nil {
		t.Errorf("%v rendered; want it refused", set)
		return
	}
	if !strings.Contains(string(out), want) {
		t.Errorf("%v: %s; want a message containing %q", set, out, want)
	}
}

// Without settings, GCP renders nothing credential-related: Application Default Credentials
// find what the environment has.
func TestGCPWithoutCredentialSettings(t *testing.T) {
	r := render(t, "providers.gcp.enabled=true")
	if a := operatorServiceAccount(t, r).Annotations; len(a) != 0 {
		t.Errorf("service account annotations = %v, want none", a)
	}
	pod := operatorPod(t, r)
	if _, ok := envOf(pod.Spec.Containers[0])["GOOGLE_APPLICATION_CREDENTIALS"]; ok {
		t.Error("GOOGLE_APPLICATION_CREDENTIALS is set without Workload Identity Federation")
	}
	for _, v := range pod.Spec.Volumes {
		if strings.HasPrefix(v.Name, "gcp-") {
			t.Errorf("volume %s without Workload Identity Federation", v.Name)
		}
	}
	if len(r.configMaps) != 0 {
		t.Errorf("config maps %v without Workload Identity Federation", r.names)
	}
}

// GKE Workload Identity is the Kubernetes service account's annotation; a hand-set one wins.
func TestGKEWorkloadIdentity(t *testing.T) {
	const key = "iam.gke.io/gcp-service-account"
	r := render(t, "providers.gcp.enabled=true", "providers.gcp.workloadIdentity.serviceAccount="+gcpOperator)
	if got := operatorServiceAccount(t, r).Annotations[key]; got != gcpOperator {
		t.Errorf("%s = %q, want %s", key, got, gcpOperator)
	}
	r = render(t, "providers.gcp.workloadIdentity.serviceAccount="+gcpOperator)
	if _, ok := operatorServiceAccount(t, r).Annotations[key]; ok {
		t.Error("the GKE annotation is rendered while GCP is off")
	}
	r = render(t, "providers.gcp.enabled=true", "providers.gcp.workloadIdentity.serviceAccount="+gcpOperator,
		`serviceAccount.annotations.iam\.gke\.io/gcp-service-account=explicit@p-1.iam.gserviceaccount.com`)
	if got := operatorServiceAccount(t, r).Annotations[key]; got != "explicit@p-1.iam.gserviceaccount.com" {
		t.Errorf("%s = %q, want the explicit one", key, got)
	}
	refuses(t, "not a service account email", "providers.gcp.enabled=true",
		"providers.gcp.workloadIdentity.serviceAccount=subnet-operator")
}

// wifMounts checks the pod reads the credential configuration from the volume and the
// projected token, and returns the configuration volume and the token projection.
func wifMounts(t *testing.T, pod corev1.PodTemplateSpec) (corev1.Volume, *corev1.ServiceAccountTokenProjection) {
	t.Helper()
	c := pod.Spec.Containers[0]
	if got := envOf(c)["GOOGLE_APPLICATION_CREDENTIALS"]; got != "/etc/gcp-wif/credential-configuration.json" {
		t.Errorf("GOOGLE_APPLICATION_CREDENTIALS = %q", got)
	}
	mounts := map[string]string{}
	for _, m := range c.VolumeMounts {
		mounts[m.Name] = m.MountPath
		if strings.HasPrefix(m.Name, "gcp-") && !m.ReadOnly {
			t.Errorf("volume %s is mounted writable", m.Name)
		}
	}
	if mounts["gcp-credential-config"] != "/etc/gcp-wif" || mounts["gcp-wif-token"] != "/var/run/secrets/gcp-wif" {
		t.Errorf("mounts = %v", mounts)
	}
	var config corev1.Volume
	var token *corev1.ServiceAccountTokenProjection
	for _, v := range pod.Spec.Volumes {
		switch v.Name {
		case "gcp-credential-config":
			config = v
		case "gcp-wif-token":
			if v.Projected == nil || len(v.Projected.Sources) != 1 || v.Projected.Sources[0].ServiceAccountToken == nil {
				t.Fatalf("token volume = %+v, want one projected service account token", v)
			}
			token = v.Projected.Sources[0].ServiceAccountToken
		}
	}
	if token == nil || config.Name == "" {
		t.Fatalf("volumes = %+v, want the credential configuration and the token", pod.Spec.Volumes)
	}
	if token.Path != "token" {
		t.Errorf("token path = %q, want token", token.Path)
	}
	return config, token
}

// Workload Identity Federation from a pool provider: the chart renders the credential
// configuration, mounts it and the token, and rolls the pods when the configuration changes.
func TestWorkloadIdentityFederationRendered(t *testing.T) {
	r := render(t, "providers.gcp.enabled=true", "providers.gcp.wif.provider="+wifProvider,
		"providers.gcp.wif.serviceAccount="+gcpOperator)
	pod := operatorPod(t, r)
	config, token := wifMounts(t, pod)
	if config.ConfigMap == nil || config.ConfigMap.Name != "release-subnet-operator-gcp-credentials" {
		t.Errorf("configuration volume = %+v, want the chart's ConfigMap", config)
	}
	if token.Audience != "https://iam.googleapis.com/"+wifProvider {
		t.Errorf("token audience = %q, want the provider's default audience", token.Audience)
	}
	if token.ExpirationSeconds == nil || *token.ExpirationSeconds != 3600 {
		t.Errorf("token lifetime = %v, want 3600", token.ExpirationSeconds)
	}
	if pod.Annotations["checksum/gcp-credentials"] == "" {
		t.Error("no checksum of the credential configuration on the pod")
	}
	if a := operatorServiceAccount(t, r).Annotations; len(a) != 0 {
		t.Errorf("service account annotations = %v, want none", a)
	}

	if len(r.configMaps) != 1 {
		t.Fatalf("config maps: %v", r.names)
	}
	var cfg struct {
		Type             string `json:"type"`
		Audience         string `json:"audience"`
		SubjectTokenType string `json:"subject_token_type"`
		TokenURL         string `json:"token_url"`
		Impersonation    string `json:"service_account_impersonation_url"`
		CredentialSource struct {
			File   string `json:"file"`
			Format struct {
				Type string `json:"type"`
			} `json:"format"`
		} `json:"credential_source"`
	}
	if err := json.Unmarshal([]byte(r.configMaps[0].Data["credential-configuration.json"]), &cfg); err != nil {
		t.Fatalf("credential configuration: %v", err)
	}
	if cfg.Type != "external_account" || cfg.Audience != "//iam.googleapis.com/"+wifProvider ||
		cfg.SubjectTokenType != "urn:ietf:params:oauth:token-type:jwt" ||
		cfg.TokenURL != "https://sts.googleapis.com/v1/token" ||
		cfg.CredentialSource.File != "/var/run/secrets/gcp-wif/token" || cfg.CredentialSource.Format.Type != "text" {
		t.Errorf("credential configuration = %+v", cfg)
	}
	if cfg.Impersonation != "https://iamcredentials.googleapis.com/v1/projects/-/serviceAccounts/"+gcpOperator+
		":generateAccessToken" {
		t.Errorf("impersonation URL = %q", cfg.Impersonation)
	}

	// Without a service account, the federated principal itself holds the roles.
	r = render(t, "providers.gcp.enabled=true", "providers.gcp.wif.provider="+wifProvider)
	data := r.configMaps[0].Data["credential-configuration.json"]
	if strings.Contains(data, "service_account_impersonation_url") {
		t.Errorf("an impersonation URL without a service account: %s", data)
	}
}

// A credential configuration of the user's own, from a ConfigMap or a Secret, with the audience
// and token path they chose.
func TestWorkloadIdentityFederationFromOwnConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  []string
		want func(corev1.Volume) bool
	}{
		{"configMap", []string{"providers.gcp.wif.credentialConfig.configMap=gcp-wif"},
			func(v corev1.Volume) bool { return v.ConfigMap != nil && v.ConfigMap.Name == "gcp-wif" }},
		{"secret", []string{"providers.gcp.wif.credentialConfig.secret=gcp-wif"},
			func(v corev1.Volume) bool { return v.Secret != nil && v.Secret.SecretName == "gcp-wif" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := render(t, append([]string{"providers.gcp.enabled=true", "providers.gcp.wif.audience=my-audience",
				"providers.gcp.wif.tokenExpirationSeconds=7200"}, tc.set...)...)
			config, token := wifMounts(t, operatorPod(t, r))
			if !tc.want(config) {
				t.Errorf("configuration volume = %+v", config)
			}
			if token.Audience != "my-audience" || *token.ExpirationSeconds != 7200 {
				t.Errorf("token = %+v", token)
			}
			if len(r.configMaps) != 0 {
				t.Errorf("the chart rendered a configuration next to the user's own: %v", r.names)
			}
		})
	}
	// Another token path moves the mount.
	r := render(t, "providers.gcp.enabled=true", "providers.gcp.wif.credentialConfig.configMap=gcp-wif",
		"providers.gcp.wif.audience=a", "providers.gcp.wif.tokenPath=/var/run/service-account/gcp-token")
	for _, v := range operatorPod(t, r).Spec.Volumes {
		if v.Name == "gcp-wif-token" && v.Projected.Sources[0].ServiceAccountToken.Path != "gcp-token" {
			t.Errorf("token file = %q, want gcp-token", v.Projected.Sources[0].ServiceAccountToken.Path)
		}
	}
	for _, m := range operatorPod(t, r).Spec.Containers[0].VolumeMounts {
		if m.Name == "gcp-wif-token" && m.MountPath != "/var/run/service-account" {
			t.Errorf("token mounted at %q, want /var/run/service-account", m.MountPath)
		}
	}
}

func TestWorkloadIdentityFederationRefusesWhatCannotWork(t *testing.T) {
	on := "providers.gcp.enabled=true"
	refuses(t, "workloadIdentityPools/<pool>", on, "providers.gcp.wif.provider=pool/eks")
	refuses(t, "needs providers.gcp.wif.audience", on, "providers.gcp.wif.credentialConfig.configMap=gcp-wif")
	refuses(t, "not both", on, "providers.gcp.wif.credentialConfig.configMap=a",
		"providers.gcp.wif.credentialConfig.secret=b", "providers.gcp.wif.audience=x")
	refuses(t, "names its service account itself", on, "providers.gcp.wif.credentialConfig.configMap=a",
		"providers.gcp.wif.audience=x", "providers.gcp.wif.serviceAccount="+gcpOperator)
	refuses(t, "not a service account email", on, "providers.gcp.wif.provider="+wifProvider,
		"providers.gcp.wif.serviceAccount=someone")
	refuses(t, "not both", on, "providers.gcp.wif.provider="+wifProvider,
		"providers.gcp.workloadIdentity.serviceAccount="+gcpOperator)
	refuses(t, "absolute file path", on, "providers.gcp.wif.provider="+wifProvider, "providers.gcp.wif.tokenPath=token")

	// All of it is ignored while GCP is off.
	r := render(t, "providers.gcp.wif.provider="+wifProvider)
	if len(r.configMaps) != 0 || slices.ContainsFunc(operatorPod(t, r).Spec.Volumes,
		func(v corev1.Volume) bool { return strings.HasPrefix(v.Name, "gcp-") }) {
		t.Errorf("Workload Identity Federation rendered while GCP is off: %v", r.names)
	}
}

// gcpEgress returns the NetworkPolicy's egress rules to link-local addresses and to the
// Google API ranges, as "cidr:port".
func gcpEgress(t *testing.T, set ...string) []string {
	t.Helper()
	r := render(t, append([]string{"networkPolicy.enabled=true", "providers.aws.enabled=false"}, set...)...)
	var out []string
	for _, np := range r.netpols {
		if strings.HasSuffix(np.Name, "-dashboard") {
			continue
		}
		for _, rule := range np.Spec.Egress {
			for _, to := range rule.To {
				if to.IPBlock == nil || to.IPBlock.CIDR == "0.0.0.0/0" {
					continue
				}
				for _, p := range rule.Ports {
					out = append(out, fmt.Sprintf("%s:%s", to.IPBlock.CIDR, p.Port.String()))
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

// The GKE metadata server hands out Workload Identity's credentials; it is allowed where they
// may come from it, and not with Workload Identity Federation, where the address would be
// another cloud's instance metadata service.
func TestGCPEgress(t *testing.T) {
	gke := []string{"169.254.169.252/32:988", "169.254.169.254/32:80"}
	for _, tc := range []struct {
		name string
		set  []string
		want []string
	}{
		{"GCP off", []string{"providers.aws.enabled=true"}, []string{"169.254.170.23/32:80"}},
		{"ADC", []string{"providers.gcp.enabled=true"}, gke},
		{"GKE Workload Identity", []string{"providers.gcp.enabled=true",
			"providers.gcp.workloadIdentity.serviceAccount=" + gcpOperator}, gke},
		{"Workload Identity Federation", []string{"providers.gcp.enabled=true",
			"providers.gcp.wif.provider=" + wifProvider}, nil},
		{"turned off", []string{"providers.gcp.enabled=true", "providers.gcp.metadataServer.enabled=false"}, nil},
		{"turned on with WIF", []string{"providers.gcp.enabled=true", "providers.gcp.wif.provider=" + wifProvider,
			"providers.gcp.metadataServer.enabled=true"}, gke},
		{"Dataplane V2 only", []string{"providers.gcp.enabled=true",
			"providers.gcp.metadataServer.endpoints[0].cidr=169.254.169.254/32",
			"providers.gcp.metadataServer.endpoints[0].port=80"}, []string{"169.254.169.254/32:80"}},
		{"private Google access", []string{"providers.gcp.enabled=true", "providers.gcp.metadataServer.enabled=false",
			"providers.gcp.apiCIDRs[0]=199.36.153.8/30"}, []string{"199.36.153.8/30:443"}},
		{"API ranges while GCP is off", []string{"providers.aws.enabled=true", "providers.gcp.apiCIDRs[0]=199.36.153.8/30"},
			[]string{"169.254.170.23/32:80"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := gcpEgress(t, tc.set...); !slices.Equal(got, tc.want) {
				t.Errorf("egress = %v, want %v", got, tc.want)
			}
		})
	}
}

// providers.gcp.events.* configures the Pub/Sub subscription of the change events. Off by
// default, and only with the GCP provider.
func TestGCPChangeEvents(t *testing.T) {
	hasEventsFlag := func(args []string) bool {
		return slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--gcp-events") })
	}
	if args, _ := managerSettings(t, "providers.gcp.enabled=true"); hasEventsFlag(args) {
		t.Errorf("args = %v, want no events flag without a subscription", args)
	}
	const sub = "projects/ops-central/subscriptions/subnet-operator-events"
	args, _ := managerSettings(t, "providers.gcp.enabled=true", "providers.gcp.events.subscription="+sub)
	for _, want := range []string{"--gcp-events-subscription=" + sub, "--gcp-events-debounce=10s"} {
		if !slices.Contains(args, want) {
			t.Errorf("args = %v, missing %s", args, want)
		}
	}
	args, _ = managerSettings(t, "providers.gcp.enabled=true", "providers.gcp.events.subscription="+sub,
		"providers.gcp.events.debounce=30s")
	if !slices.Contains(args, "--gcp-events-debounce=30s") {
		t.Errorf("args = %v, want the debounce set", args)
	}
	if args, _ := managerSettings(t, "providers.gcp.events.subscription="+sub); hasEventsFlag(args) {
		t.Errorf("args = %v, want no GCP events flag while GCP is not enabled", args)
	}
	refuses(t, "is not projects/<project>/subscriptions/<name>", "providers.gcp.enabled=true",
		"providers.gcp.events.subscription=subnet-operator-events")
}
