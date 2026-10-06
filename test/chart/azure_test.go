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
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// The operator's own Azure identity comes in three shapes: whatever the environment offers
// (nothing rendered), Microsoft Entra Workload ID through the AKS webhook (a pod label and
// service account annotations), and Workload ID without the webhook (a projected token and the
// AZURE_* variables the webhook would set).

const (
	azureClient = "3f0c6a1e-0000-4000-8000-0000000000a1"
	azureTenant = "72f988bf-0000-4000-8000-00000000f00d"
)

func azureTokenVolume(pod corev1.PodTemplateSpec) *corev1.Volume {
	for i, v := range pod.Spec.Volumes {
		if v.Name == "azure-identity-token" {
			return &pod.Spec.Volumes[i]
		}
	}
	return nil
}

func TestAzureWithoutCredentialSettings(t *testing.T) {
	r := render(t, "providers.azure.enabled=true")
	if a := operatorServiceAccount(t, r).Annotations; len(a) != 0 {
		t.Errorf("service account annotations = %v, want none", a)
	}
	pod := operatorPod(t, r)
	if _, ok := pod.Labels["azure.workload.identity/use"]; ok {
		t.Error("the workload identity label is set without providers.azure.workloadIdentity")
	}
	for name := range envOf(pod.Spec.Containers[0]) {
		if strings.HasPrefix(name, "AZURE_") {
			t.Errorf("%s is set without Workload Identity", name)
		}
	}
	if azureTokenVolume(pod) != nil {
		t.Error("a token is projected without Workload Identity")
	}
	if !slices.Contains(pod.Spec.Containers[0].Args, "--azure-cloud=AzurePublic") {
		t.Errorf("args = %v, want --azure-cloud=AzurePublic", pod.Spec.Containers[0].Args)
	}
	for _, a := range operatorPod(t, render(t)).Spec.Containers[0].Args {
		if strings.HasPrefix(a, "--azure-") {
			t.Errorf("an Azure flag while Azure is off: %s", a)
		}
	}
}

// On AKS the workload identity webhook does the work: the pod label asks for it, the service
// account's annotations say which identity and tenant; a hand-set annotation wins.
func TestAzureWorkloadIdentityThroughTheAKSWebhook(t *testing.T) {
	r := render(t, "providers.azure.enabled=true", "providers.azure.workloadIdentity.enabled=true",
		"providers.azure.workloadIdentity.clientId="+azureClient, "providers.azure.tenantId="+azureTenant)
	a := operatorServiceAccount(t, r).Annotations
	if a["azure.workload.identity/client-id"] != azureClient || a["azure.workload.identity/tenant-id"] != azureTenant {
		t.Errorf("service account annotations = %v", a)
	}
	pod := operatorPod(t, r)
	if pod.Labels["azure.workload.identity/use"] != "true" {
		t.Errorf("pod labels = %v, want azure.workload.identity/use: \"true\"", pod.Labels)
	}
	if azureTokenVolume(pod) != nil || envOf(pod.Spec.Containers[0])["AZURE_FEDERATED_TOKEN_FILE"] != "" {
		t.Error("the chart projects a token the webhook projects")
	}

	r = render(t, "providers.azure.enabled=true", "providers.azure.workloadIdentity.enabled=true")
	if a := operatorServiceAccount(t, r).Annotations; len(a) != 0 {
		t.Errorf("annotations without a client or tenant ID: %v", a)
	}
	r = render(t, "providers.azure.enabled=true", "providers.azure.workloadIdentity.enabled=true",
		"providers.azure.workloadIdentity.clientId="+azureClient,
		`serviceAccount.annotations.azure\.workload\.identity/client-id=00000000-0000-4000-8000-0000000000ff`)
	if got := operatorServiceAccount(t, r).Annotations["azure.workload.identity/client-id"]; got !=
		"00000000-0000-4000-8000-0000000000ff" {
		t.Errorf("client-id = %q, want the explicit one", got)
	}
	r = render(t, "providers.azure.workloadIdentity.enabled=true")
	if _, ok := operatorPod(t, r).Labels["azure.workload.identity/use"]; ok {
		t.Error("the workload identity label is set while Azure is off")
	}
}

// Anywhere else the chart projects the token Microsoft Entra ID accepts and sets what the
// webhook would.
func TestAzureWorkloadIdentityWithoutTheWebhook(t *testing.T) {
	r := render(t, "providers.azure.enabled=true", "providers.azure.workloadIdentity.enabled=true",
		"providers.azure.workloadIdentity.webhook=false", "providers.azure.workloadIdentity.clientId="+azureClient,
		"providers.azure.tenantId="+azureTenant, "providers.azure.workloadIdentity.tokenPath=/var/run/azure/token",
		"providers.azure.workloadIdentity.tokenExpirationSeconds=7200")
	pod := operatorPod(t, r)
	if _, ok := pod.Labels["azure.workload.identity/use"]; ok {
		t.Error("the webhook's label is set without the webhook")
	}
	if a := operatorServiceAccount(t, r).Annotations; len(a) != 0 {
		t.Errorf("service account annotations without the webhook: %v", a)
	}
	env := envOf(pod.Spec.Containers[0])
	for name, want := range map[string]string{"AZURE_FEDERATED_TOKEN_FILE": "/var/run/azure/token",
		"AZURE_TENANT_ID": azureTenant, "AZURE_CLIENT_ID": azureClient} {
		if env[name] != want {
			t.Errorf("%s = %q, want %q", name, env[name], want)
		}
	}
	v := azureTokenVolume(pod)
	if v == nil || v.Projected == nil || len(v.Projected.Sources) != 1 ||
		v.Projected.Sources[0].ServiceAccountToken == nil {
		t.Fatalf("token volume = %+v", v)
	}
	tok := v.Projected.Sources[0].ServiceAccountToken
	if tok.Audience != "api://AzureADTokenExchange" || tok.Path != "token" || tok.ExpirationSeconds == nil ||
		*tok.ExpirationSeconds != 7200 {
		t.Errorf("projected token = %+v", tok)
	}
	mounted := false
	for _, m := range pod.Spec.Containers[0].VolumeMounts {
		mounted = mounted || (m.Name == v.Name && m.MountPath == "/var/run/azure" && m.ReadOnly)
	}
	if !mounted {
		t.Errorf("the token is not mounted read-only at /var/run/azure: %v", pod.Spec.Containers[0].VolumeMounts)
	}

	// Without a client ID of its own the operator only reaches accounts' identities.
	env = envOf(operatorPod(t, render(t, "providers.azure.enabled=true",
		"providers.azure.workloadIdentity.enabled=true", "providers.azure.workloadIdentity.webhook=false",
		"providers.azure.tenantId="+azureTenant)).Spec.Containers[0])
	if _, ok := env["AZURE_CLIENT_ID"]; ok {
		t.Error("AZURE_CLIENT_ID without a client ID")
	}
}

func TestAzureCloud(t *testing.T) {
	args, _ := managerSettings(t, "providers.azure.enabled=true", "providers.azure.cloud=AzureUSGovernment",
		"providers.azure.authorityHost=https://login.microsoftonline.us/")
	for _, want := range []string{"--azure-cloud=AzureUSGovernment",
		"--azure-authority-host=https://login.microsoftonline.us/"} {
		if !slices.Contains(args, want) {
			t.Errorf("args = %v, want %s", args, want)
		}
	}
}

func TestAzureRefusesWhatCannotWork(t *testing.T) {
	on := []string{"providers.azure.enabled=true"}
	wi := append(slices.Clone(on), "providers.azure.workloadIdentity.enabled=true")
	refuses(t, "is not AzurePublic, AzureUSGovernment or AzureChina", append(on, "providers.azure.cloud=AzureGermany")...)
	refuses(t, "is not an https:// URL", append(on, "providers.azure.authorityHost=login.example")...)
	refuses(t, "is not a tenant ID", append(on, "providers.azure.tenantId=contoso.onmicrosoft.com")...)
	refuses(t, "is not a client ID", append(wi, "providers.azure.workloadIdentity.clientId=subnet-operator")...)
	refuses(t, "needs providers.azure.workloadIdentity.enabled", append(on,
		"providers.azure.workloadIdentity.clientId="+azureClient)...)
	refuses(t, "needs providers.azure.tenantId", append(wi, "providers.azure.workloadIdentity.webhook=false")...)
	refuses(t, "must be an absolute file path", append(wi, "providers.azure.workloadIdentity.webhook=false",
		"providers.azure.tenantId="+azureTenant, "providers.azure.workloadIdentity.tokenPath=/token")...)
}

// With the egress narrowed, the Entra ID and Resource Manager ranges are allowed on 443, and
// only while Azure is on.
func TestAzureEgress(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  []string
		want []string
	}{
		{"no ranges", []string{"providers.azure.enabled=true"}, nil},
		{"ranges", []string{"providers.azure.enabled=true", "providers.azure.apiCIDRs[0]=20.190.128.0/18",
			"providers.azure.apiCIDRs[1]=4.150.0.0/18"}, []string{"20.190.128.0/18:443", "4.150.0.0/18:443"}},
		{"ranges while Azure is off", []string{"providers.gcp.enabled=true", "providers.gcp.metadataServer.enabled=false",
			"providers.azure.apiCIDRs[0]=20.190.128.0/18"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := gcpEgress(t, tc.set...); !slices.Equal(got, tc.want) {
				t.Errorf("egress = %v, want %v", got, tc.want)
			}
		})
	}
}

// providers.azure.events.* configures the Storage queue of the change events. Off by default,
// only with the Azure provider, and never with a shared access signature in the values.
func TestAzureChangeEvents(t *testing.T) {
	hasEventsFlag := func(args []string) bool {
		return slices.ContainsFunc(args, func(a string) bool { return strings.HasPrefix(a, "--azure-events") })
	}
	if args, _ := managerSettings(t, "providers.azure.enabled=true"); hasEventsFlag(args) {
		t.Errorf("args = %v, want no events flag without a queue", args)
	}
	const queue = "https://netops.queue.core.windows.net/subnet-operator-events"
	args, _ := managerSettings(t, "providers.azure.enabled=true", "providers.azure.events.queueUrl="+queue)
	for _, want := range []string{"--azure-events-queue-url=" + queue, "--azure-events-debounce=10s"} {
		if !slices.Contains(args, want) {
			t.Errorf("args = %v, missing %s", args, want)
		}
	}
	args, _ = managerSettings(t, "providers.azure.enabled=true", "providers.azure.events.queueUrl="+queue,
		"providers.azure.events.debounce=30s")
	if !slices.Contains(args, "--azure-events-debounce=30s") {
		t.Errorf("args = %v, want the debounce set", args)
	}
	if args, _ := managerSettings(t, "providers.azure.events.queueUrl="+queue); hasEventsFlag(args) {
		t.Errorf("args = %v, want no Azure events flag while Azure is not enabled", args)
	}
	for _, bad := range []string{"subnet-operator-events", "http://netops.queue.core.windows.net/q",
		"https://netops.queue.core.windows.net", queue + "?sv=2024-11-04&sig=secret"} {
		refuses(t, "is not https://<storage account>.queue.core.windows.net/<queue>", "providers.azure.enabled=true",
			"providers.azure.events.queueUrl="+bad)
	}
}
