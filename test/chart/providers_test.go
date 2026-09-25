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
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// managerSettings returns the operator container's arguments and environment.
func managerSettings(t *testing.T, set ...string) ([]string, map[string]string) {
	t.Helper()
	d := render(t, set...).deployment(t, "subnet-operator")
	c := d.Spec.Template.Spec.Containers[0]
	env := map[string]string{}
	for _, e := range c.Env {
		env[e.Name] = e.Value
	}
	return c.Args, env
}

func TestTheAWSProviderIsEnabledByDefault(t *testing.T) {
	args, env := managerSettings(t)
	if !slices.Contains(args, "--providers=aws") {
		t.Errorf("args = %v, want --providers=aws", args)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "--aws-events") || strings.HasPrefix(a, "--events-") {
			t.Errorf("an events flag without a queue: %s", a)
		}
	}
	if len(env) != 0 {
		t.Errorf("environment without any setting: %v", env)
	}
}

// providers.aws.* is where 0.9 configures AWS; the 0.8 names (events.*, aws.*) keep working
// until 0.10, and the new name wins when both are set.
func TestAWSSettingsUnderProvidersAndTheirDeprecatedNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  []string
	}{
		{"providers.aws", []string{"providers.aws.events.queueUrl=https://sqs.eu-central-1.amazonaws.com/1/q",
			"providers.aws.events.debounce=30s", "providers.aws.region=eu-central-1",
			"providers.aws.endpointURL=http://moto:5000"}},
		{"the 0.7 names", []string{"events.queueUrl=https://sqs.eu-central-1.amazonaws.com/1/q",
			"events.debounce=30s", "aws.region=eu-central-1", "aws.endpointURL=http://moto:5000"}},
		{"both, the new ones win", []string{"providers.aws.events.queueUrl=https://sqs.eu-central-1.amazonaws.com/1/q",
			"events.queueUrl=https://sqs.eu-west-1.amazonaws.com/1/old", "providers.aws.events.debounce=30s",
			"events.debounce=5s", "providers.aws.region=eu-central-1", "aws.region=eu-west-1",
			"providers.aws.endpointURL=http://moto:5000"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, env := managerSettings(t, tc.set...)
			for _, want := range []string{"--aws-events-queue-url=https://sqs.eu-central-1.amazonaws.com/1/q",
				"--aws-events-debounce=30s"} {
				if !slices.Contains(args, want) {
					t.Errorf("args = %v, missing %s", args, want)
				}
			}
			if env["AWS_REGION"] != "eu-central-1" || env["AWS_ENDPOINT_URL"] != "http://moto:5000" {
				t.Errorf("env = %v", env)
			}
		})
	}
}

// The operator's own identity is per provider: for IRSA the role becomes the service account
// annotation EKS reads. An annotation set on the service account by hand wins.
func TestTheAWSIdentityOfTheOperator(t *testing.T) {
	annotations := func(set ...string) map[string]string {
		t.Helper()
		for _, sa := range render(t, set...).serviceAccounts {
			if strings.HasSuffix(sa.Name, "subnet-operator") {
				return sa.Annotations
			}
		}
		t.Fatal("no operator service account")
		return nil
	}
	const role = "arn:aws:iam::111111111111:role/subnet-operator"
	if got := annotations("providers.aws.irsaRoleARN=" + role); got["eks.amazonaws.com/role-arn"] != role {
		t.Errorf("annotations = %v, want the IRSA role", got)
	}
	if got := annotations(); len(got) != 0 {
		t.Errorf("annotations without IRSA = %v, want none (EKS Pod Identity needs none)", got)
	}
	got := annotations("providers.aws.irsaRoleARN="+role,
		`serviceAccount.annotations.eks\.amazonaws\.com/role-arn=explicit`)
	if got["eks.amazonaws.com/role-arn"] != "explicit" {
		t.Errorf("annotations = %v, want the explicit one", got)
	}
}

// podIdentityEgress returns the NetworkPolicy's egress rule to the EKS Pod Identity agent as
// "cidr:port", or "" when there is none.
func podIdentityEgress(t *testing.T, set ...string) string {
	t.Helper()
	r := render(t, append([]string{"networkPolicy.enabled=true"}, set...)...)
	for _, np := range r.netpols {
		if strings.HasSuffix(np.Name, "-dashboard") {
			continue
		}
		for _, rule := range np.Spec.Egress {
			for _, to := range rule.To {
				if to.IPBlock == nil || !strings.HasPrefix(to.IPBlock.CIDR, "169.254.") && to.IPBlock.CIDR != "10.0.0.1/32" {
					continue
				}
				return fmt.Sprintf("%s:%s", to.IPBlock.CIDR, rule.Ports[0].Port.String())
			}
		}
	}
	return ""
}

// The Pod Identity agent is AWS's, so its egress rule is configured under providers.aws since
// 0.9 (owner decision, 2026-09-25). The 0.8 name, networkPolicy.egress.podIdentity, keeps
// working until 0.10, and a key set under the new name wins.
func TestPodIdentityEgressUnderProvidersAndItsDeprecatedName(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  []string
		want string
	}{
		{"defaults", nil, "169.254.170.23/32:80"},
		{"providers.aws, off (IRSA)", []string{"providers.aws.podIdentity.enabled=false"}, ""},
		{"providers.aws, elsewhere", []string{"providers.aws.podIdentity.cidr=10.0.0.1/32",
			"providers.aws.podIdentity.port=81"}, "10.0.0.1/32:81"},
		{"the 0.8 name, off", []string{"networkPolicy.egress.podIdentity.enabled=false"}, ""},
		{"the 0.8 name, elsewhere", []string{"networkPolicy.egress.podIdentity.cidr=10.0.0.1/32",
			"networkPolicy.egress.podIdentity.port=81"}, "10.0.0.1/32:81"},
		{"both, the new one wins", []string{"networkPolicy.egress.podIdentity.enabled=false",
			"providers.aws.podIdentity.enabled=true", "networkPolicy.egress.podIdentity.port=81",
			"providers.aws.podIdentity.port=82"}, "169.254.170.23/32:82"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := podIdentityEgress(t, tc.set...); got != tc.want {
				t.Errorf("egress to the Pod Identity agent = %q, want %q", got, tc.want)
			}
		})
	}
}

// The 0.7 forms of the chart's NetworkScope values were deprecated in 0.8 and removed in 0.9.
// Rendered as they are, the API server would prune the unknown fields silently, and an account
// would lose its roles; the chart refuses them with the new name instead.
func TestTheRemovedNetworkScopeValuesAreRefused(t *testing.T) {
	for _, tc := range []struct {
		set  []string
		want string
	}{
		{[]string{"networkScope.vpcTagSelector.hs/managed=true"}, "networkScope.networkSelector.matchTags"},
		{[]string{"networkScope.accounts[0].id=222222222222",
			"networkScope.accounts[0].roleARN=arn:aws:iam::222222222222:role/read"}, "aws member"},
		{[]string{"networkScope.accounts[0].id=222222222222",
			"networkScope.accounts[0].writeRoleARN=arn:aws:iam::222222222222:role/write"}, "aws member"},
	} {
		args := make([]string, 0, 7+2*len(tc.set))
		args = append(args, "template", "release", chartDir, "--set", "networkScope.create=true",
			"--set", "networkScope.regions[0]=eu-central-1")
		for _, s := range tc.set {
			args = append(args, "--set", s)
		}
		out, err := exec.Command(helmBinary(t), args...).CombinedOutput()
		if err == nil {
			t.Errorf("%v rendered; want it refused", tc.set)
			continue
		}
		if !strings.Contains(string(out), tc.want) || !strings.Contains(string(out), "removed in 0.9") {
			t.Errorf("%v: %s; want a message naming %q", tc.set, out, tc.want)
		}
	}

	// The current form renders.
	r := render(t, "networkScope.create=true", "networkScope.regions[0]=eu-central-1",
		"networkScope.accounts[0].id=222222222222",
		"networkScope.accounts[0].aws.roleARN=arn:aws:iam::222222222222:role/read")
	if !slices.Contains(r.names, "NetworkScope/organization") {
		t.Errorf("no NetworkScope among %v", r.names)
	}
}
