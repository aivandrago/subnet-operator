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

// Package chart checks what the Helm chart renders. The properties tested here are the ones a
// template change could quietly break and no cluster would complain about: a dashboard that
// can read with an identity of its own, or pods that land behind the wrong Service.
package chart

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"
)

const chartDir = "../../charts/subnet-operator"

// helmBinary is the helm `make helm` downloads, or one on PATH. CI runs `make helm-lint`
// before the tests, so there it is always the pinned one.
func helmBinary(t *testing.T) string {
	t.Helper()
	if p, err := filepath.Abs("../../bin/helm"); err == nil {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if p, err := exec.LookPath("helm"); err == nil {
		return p
	}
	t.Skip("helm not found; run make helm")
	return ""
}

type rendered struct {
	deployments     []appsv1.Deployment
	services        []corev1.Service
	serviceAccounts []corev1.ServiceAccount
	pdbs            []policyv1.PodDisruptionBudget
	netpols         []networkingv1.NetworkPolicy
	roleBindings    []rbacv1.RoleBinding
	clusterBindings []rbacv1.ClusterRoleBinding
	clusterRoles    []rbacv1.ClusterRole
	names           []string
}

func render(t *testing.T, set ...string) rendered {
	t.Helper()
	args := make([]string, 0, 5+2*len(set))
	args = append(args, "template", "release", chartDir, "--namespace", "subnets")
	for _, s := range set {
		args = append(args, "--set", s)
	}
	cmd := exec.Command(helmBinary(t), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helm template: %v: %s", err, stderr.String())
	}

	var r rendered
	for doc := range strings.SplitSeq(string(out), "\n---") {
		var meta struct {
			Kind     string            `json:"kind"`
			Metadata metav1.ObjectMeta `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(doc), &meta); err != nil || meta.Kind == "" {
			continue
		}
		r.names = append(r.names, meta.Kind+"/"+meta.Metadata.Name)
		switch meta.Kind {
		case "Deployment":
			decode(t, doc, &r.deployments)
		case "Service":
			decode(t, doc, &r.services)
		case "ServiceAccount":
			decode(t, doc, &r.serviceAccounts)
		case "PodDisruptionBudget":
			decode(t, doc, &r.pdbs)
		case "NetworkPolicy":
			decode(t, doc, &r.netpols)
		case "RoleBinding":
			decode(t, doc, &r.roleBindings)
		case "ClusterRoleBinding":
			decode(t, doc, &r.clusterBindings)
		case "ClusterRole":
			decode(t, doc, &r.clusterRoles)
		}
	}
	return r
}

func decode[T any](t *testing.T, doc string, into *[]T) {
	t.Helper()
	var v T
	if err := yaml.Unmarshal([]byte(doc), &v); err != nil {
		t.Fatalf("decoding %T: %v", v, err)
	}
	*into = append(*into, v)
}

func (r rendered) deployment(t *testing.T, suffix string) appsv1.Deployment {
	t.Helper()
	for _, d := range r.deployments {
		if strings.HasSuffix(d.Name, suffix) {
			return d
		}
	}
	t.Fatalf("no Deployment ending in %q among %v", suffix, r.names)
	return appsv1.Deployment{}
}

func TestTheDashboardIsOffByDefault(t *testing.T) {
	for _, n := range render(t).names {
		if strings.Contains(n, "dashboard") {
			t.Errorf("%s is rendered although dashboard.enabled is false", n)
		}
	}
}

func TestTheDashboardHasNoIdentityOfItsOwn(t *testing.T) {
	r := render(t, "dashboard.enabled=true")
	d := r.deployment(t, "-dashboard")
	pod := d.Spec.Template.Spec

	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("the dashboard pod mounts a service account token")
	}
	var sa *corev1.ServiceAccount
	for i := range r.serviceAccounts {
		if r.serviceAccounts[i].Name == pod.ServiceAccountName {
			sa = &r.serviceAccounts[i]
		}
	}
	if sa == nil {
		t.Fatalf("the dashboard runs as %q, which the chart does not create: it would inherit whatever that account can do",
			pod.ServiceAccountName)
	}
	if sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Error("the dashboard's service account mounts its token by default")
	}
	var operator appsv1.Deployment
	for _, dep := range r.deployments {
		if dep.Name != d.Name {
			operator = dep
		}
	}
	if pod.ServiceAccountName == operator.Spec.Template.Spec.ServiceAccountName {
		t.Error("the dashboard runs as the operator's service account")
	}
	for _, b := range r.roleBindings {
		for _, s := range b.Subjects {
			if s.Name == sa.Name {
				t.Errorf("RoleBinding %s grants the dashboard's service account a role", b.Name)
			}
		}
	}
	for _, b := range r.clusterBindings {
		for _, s := range b.Subjects {
			if s.Name == sa.Name {
				t.Errorf("ClusterRoleBinding %s grants the dashboard's service account a role", b.Name)
			}
		}
	}
}

func TestTheDashboardRunsLockedDown(t *testing.T) {
	d := render(t, "dashboard.enabled=true").deployment(t, "-dashboard")
	pod := d.Spec.Template.Spec
	if len(pod.Containers) != 1 {
		t.Fatalf("%d containers, want 1", len(pod.Containers))
	}
	c := pod.Containers[0]
	if len(c.Command) != 1 || c.Command[0] != "/dashboard" {
		t.Errorf("command %v, want the image's /dashboard entrypoint", c.Command)
	}
	psc, sc := pod.SecurityContext, c.SecurityContext
	switch {
	case psc == nil || psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot:
		t.Error("the pod may run as root")
	case psc.SeccompProfile == nil || psc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault:
		t.Error("no RuntimeDefault seccomp profile")
	case sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem:
		t.Error("the root filesystem is writable")
	case sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation:
		t.Error("privilege escalation is allowed")
	case sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL":
		t.Error("capabilities are not all dropped")
	}
	if c.Resources.Limits.Cpu().IsZero() || c.Resources.Limits.Memory().IsZero() {
		t.Error("no resource limits")
	}
	for _, env := range c.Env {
		if strings.Contains(strings.ToUpper(env.Name), "TOKEN") {
			t.Errorf("the dashboard is handed a token in %s", env.Name)
		}
	}
}

// The dashboard and the operator must not select each other's pods: a Service or budget that
// matched both would send webhook calls or metrics scrapes to the dashboard, or count it as an
// operator replica.
func TestTheDashboardAndTheOperatorDoNotShareSelectors(t *testing.T) {
	r := render(t, "dashboard.enabled=true", "networkPolicy.enabled=true")
	dash := r.deployment(t, "-dashboard")
	dashLabels := labels.Set(dash.Spec.Template.Labels)
	var op appsv1.Deployment
	for _, d := range r.deployments {
		if d.Name != dash.Name {
			op = d
		}
	}
	opLabels := labels.Set(op.Spec.Template.Labels)

	check := func(kind, name string, sel map[string]string) {
		if len(sel) == 0 {
			return
		}
		s := labels.SelectorFromSet(sel)
		mine := strings.HasSuffix(name, "-dashboard")
		if mine && s.Matches(opLabels) {
			t.Errorf("%s %s selects the operator's pods", kind, name)
		}
		if !mine && s.Matches(dashLabels) {
			t.Errorf("%s %s selects the dashboard's pods", kind, name)
		}
		if mine && !s.Matches(dashLabels) {
			t.Errorf("%s %s does not select the dashboard's pods", kind, name)
		}
	}
	for _, d := range r.deployments {
		check("Deployment", d.Name, d.Spec.Selector.MatchLabels)
	}
	for _, s := range r.services {
		check("Service", s.Name, s.Spec.Selector)
	}
	for _, p := range r.pdbs {
		check("PodDisruptionBudget", p.Name, p.Spec.Selector.MatchLabels)
	}
	found := false
	for _, np := range r.netpols {
		check("NetworkPolicy", np.Name, np.Spec.PodSelector.MatchLabels)
		found = found || strings.HasSuffix(np.Name, "-dashboard")
	}
	if !found {
		t.Error("networkPolicy.enabled renders no NetworkPolicy for the dashboard")
	}
}
