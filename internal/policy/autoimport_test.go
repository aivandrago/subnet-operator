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

package policy

import (
	"maps"
	"slices"
	"testing"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
)

const (
	payments = "arn:aws:sts::222222222222:assumed-role/payments-deploy/maria.k"
	pipeline = "arn:aws:sts::222222222222:assumed-role/terraform-apply/gitea-runner"
	console  = "arn:aws:sts::111111111111:assumed-role/ops-admin/anton"
)

func fullPolicy() *awsv1alpha1.AutoImportPolicy {
	return &awsv1alpha1.AutoImportPolicy{
		Mode: awsv1alpha1.AutoImportApply,
		FromCreator: []awsv1alpha1.CreatorRule{
			{PrincipalPrefix: "arn:aws:sts::222222222222:assumed-role/payments-", Tags: map[string]string{"hs/owner": "team-payments"}},
		},
		InheritFromVPC:  []string{"hs/owner", "hs/env"},
		AccountDefaults: []awsv1alpha1.AccountDefault{{Account: "111111111111", Tags: map[string]string{"hs/owner": "team-platform"}}},
		Skip: []awsv1alpha1.SkipRule{
			{TagKey: "managed-by", TagValue: "terraform"},
			{PrincipalPrefix: "arn:aws:sts::222222222222:assumed-role/terraform-"},
		},
	}
}

func subnet(tags, parent map[string]string) Resource {
	return Resource{ID: "subnet-0a1b2c3d", Account: "222222222222", Region: "eu-central-1",
		Tags: tags, IsSubnet: true, ParentVPCTags: parent}
}

func TestDecide(t *testing.T) {
	cases := []struct {
		name     string
		resource Resource
		creator  string
		policy   *awsv1alpha1.AutoImportPolicy
		want     Verdict
		tags     map[string]string
	}{
		{
			name: "off by default", resource: subnet(nil, nil), creator: payments,
			policy: &awsv1alpha1.AutoImportPolicy{}, want: VerdictNone,
		},
		{
			name: "nil policy", resource: subnet(nil, nil), creator: payments, policy: nil, want: VerdictNone,
		},
		{
			name: "creator rule wins over inheritance", resource: subnet(nil, map[string]string{"hs/owner": "team-platform"}),
			creator: payments, policy: fullPolicy(), want: VerdictImport,
			tags: map[string]string{"hs/owner": "team-payments", "hs/managed": "true"},
		},
		{
			name:     "inherited from the VPC when no creator rule matches",
			resource: subnet(nil, map[string]string{"hs/owner": "team-web", "hs/env": "prod", "other": "x"}),
			creator:  console, policy: fullPolicy(), want: VerdictImport,
			tags: map[string]string{"hs/owner": "team-web", "hs/env": "prod", "hs/managed": "true"},
		},
		{
			name:     "account defaults are the last resort",
			resource: Resource{ID: "vpc-0abc", Account: "111111111111", Region: "eu-west-1"},
			creator:  console, policy: fullPolicy(), want: VerdictImport,
			tags: map[string]string{"hs/owner": "team-platform", "hs/managed": "true"},
		},
		{
			name: "skipped by tag", resource: subnet(map[string]string{"managed-by": "terraform"}, nil),
			creator: payments, policy: fullPolicy(), want: VerdictSkip,
		},
		{
			name:     "a different value of the same tag key does not skip",
			resource: subnet(map[string]string{"managed-by": "cdk"}, map[string]string{"hs/owner": "team-web"}),
			creator:  console, policy: fullPolicy(), want: VerdictImport,
			tags: map[string]string{"hs/owner": "team-web", "hs/managed": "true"},
		},
		{
			name: "skipped by creating principal", resource: subnet(nil, map[string]string{"hs/owner": "team-web"}),
			creator: pipeline, policy: fullPolicy(), want: VerdictSkip,
		},
		{
			name: "no owner anywhere", resource: subnet(nil, nil), creator: console, policy: fullPolicy(), want: VerdictNoOwner,
		},
		{
			name: "unknown creator and no inheritance", resource: subnet(nil, nil), creator: "", policy: fullPolicy(), want: VerdictNoOwner,
		},
		{
			name:     "an owner the resource already carries counts",
			resource: subnet(map[string]string{"hs/owner": "team-data"}, nil), creator: console,
			policy: fullPolicy(), want: VerdictImport,
			tags: map[string]string{"hs/managed": "true"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Decide(c.resource, c.creator, c.policy)
			if got.Verdict != c.want {
				t.Fatalf("verdict = %q (%s), want %q", got.Verdict, got.Reason, c.want)
			}
			if c.tags != nil && !maps.Equal(got.Tags, c.tags) {
				t.Errorf("tags = %v, want %v", got.Tags, c.tags)
			}
			if got.Verdict == VerdictImport && got.Reason == "" && len(c.tags) > 1 {
				t.Error("an import decision must say where the tags came from")
			}
		})
	}
}

func TestDecideRequiredTags(t *testing.T) {
	p := fullPolicy()
	p.RequiredTags = []string{"hs/owner", "hs/env"}

	// The creator rule only yields the owner, so the environment is still missing.
	got := Decide(subnet(nil, nil), payments, p)
	if got.Verdict != VerdictNoOwner || !slices.Equal(got.Missing, []string{"hs/env"}) {
		t.Fatalf("verdict = %q, missing = %v; want no_owner missing [hs/env]", got.Verdict, got.Missing)
	}

	// With the environment inherited from the VPC it resolves... except that inheritance only
	// runs when nothing else matched, so the owner rule alone still leaves it missing.
	p.FromCreator[0].Tags = map[string]string{"hs/owner": "team-payments", "hs/env": "prod"}
	if got := Decide(subnet(nil, nil), payments, p); got.Verdict != VerdictImport {
		t.Fatalf("verdict = %q (%s), want import", got.Verdict, got.Reason)
	}
}

func TestDecideCustomManagedTag(t *testing.T) {
	p := fullPolicy()
	p.ManagedTag, p.ManagedValue = "netops/managed", "yes"
	got := Decide(subnet(nil, nil), payments, p)
	if got.Tags["netops/managed"] != "yes" {
		t.Fatalf("tags = %v, want the custom managed tag", got.Tags)
	}
	if _, ok := got.Tags[awsv1alpha1.DefaultManagedTag]; ok {
		t.Error("the default managed tag must not be added as well")
	}
}

func TestDecideDryRunStillDecides(t *testing.T) {
	p := fullPolicy()
	p.Mode = awsv1alpha1.AutoImportDryRun
	// Dry run is about what the controller does with the decision, not about the decision.
	if got := Decide(subnet(nil, nil), payments, p); got.Verdict != VerdictImport {
		t.Fatalf("verdict = %q, want import", got.Verdict)
	}
}
