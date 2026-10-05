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

package v1

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"
)

// The keys every release before 2.0 wrote on AWS. They must never change: resources tagged
// with them would silently drop out of the inventory.
func TestAWSOperatorTagKeysAreTheOnesBefore20(t *testing.T) {
	for _, p := range []Provider{ProviderAWS, "", "SomeFutureCloud"} {
		k := OperatorTagKeysFor(p)
		want := OperatorTagKeys{Owner: "hs/owner", Env: "hs/env", Tier: "hs/tier", Managed: "hs/managed",
			ManagedBy: "hs/managed-by", Claim: "hs/claim", claimSeparator: "/"}
		if k != want {
			t.Errorf("OperatorTagKeysFor(%q) = %+v, want %+v", p, k, want)
		}
		if got := k.ClaimValue("team-a", "db"); got != "team-a/db" {
			t.Errorf("claim value on %q = %q, want team-a/db", p, got)
		}
	}
	if got := DefaultTagKeys(ProviderAWS); got != (TagKeys{Owner: "hs/owner", Env: "hs/env", Tier: "hs/tier"}) {
		t.Errorf("DefaultTagKeys(AWS) = %+v", got)
	}
}

func TestGCPOperatorTagKeysAreValidTagKeyShortNames(t *testing.T) {
	k := OperatorTagKeysFor(ProviderGCP)
	for _, key := range []string{k.Owner, k.Env, k.Tier, k.Managed, k.ManagedBy, k.Claim} {
		if !strings.HasPrefix(key, "hs-") || strings.ContainsAny(key, "/") {
			t.Errorf("GCP key %q is not an hs-* short name", key)
		}
	}
	if got := k.ClaimValue("team-a", "db.v2"); got != "team-a_db.v2" {
		t.Errorf("GCP claim value = %q, want team-a_db.v2", got)
	}
	if got := (resolvedTagKeysOf(ProviderGCP)); got.Owner != "hs-owner" || got.Env != "hs-env" || got.Tier != "hs-tier" {
		t.Errorf("a GCP scope without tagKeys resolves to %+v", got)
	}
}

// A GCP tag value is at most 63 characters: a longer namespace_name keeps a prefix and ends in
// ten hex digits of its hash, so it stays unique, valid and the same on every call.
func TestGCPClaimValueStaysAValidUniqueTagValue(t *testing.T) {
	k := OperatorTagKeysFor(ProviderGCP)
	ns := "team-payments-platform"
	long := strings.Repeat("a", 30) + ".claim-" + strings.Repeat("b", 20)
	v := k.ClaimValue(ns, long)
	if len(v) > 63 || len(v) < 50 {
		t.Fatalf("claim value %q has %d characters, want at most 63 and most of them used", v, len(v))
	}
	if !strings.HasPrefix(v, ns+"_") {
		t.Errorf("claim value %q does not start with the namespace", v)
	}
	if v != k.ClaimValue(ns, long) {
		t.Error("the claim value is not stable")
	}
	if other := k.ClaimValue(ns, long+"c"); other == v {
		t.Errorf("two claims share the value %q", v)
	}
	// A cut that ends in a "." or "-" must not leave it before the hash.
	for i := range 20 {
		name := strings.Repeat("x", 33+i) + strings.Repeat(".-", 20)
		got := k.ClaimValue("ns", name)
		cut := got[:len(got)-11]
		if last := cut[len(cut)-1]; last == '.' || last == '-' || last == '_' {
			t.Errorf("claim value %q has %q before its hash", got, last)
		}
	}
	// AWS values have no such limit.
	aws := OperatorTagKeysFor(ProviderAWS)
	if got := aws.ClaimValue(ns, long); got != ns+"/"+long {
		t.Errorf("AWS claim value = %q, want the whole namespace/name", got)
	}
}

// resolvedTagKeysOf is what a scope of the provider without spec.tagKeys resolves to.
func resolvedTagKeysOf(p Provider) TagKeys {
	s := &NetworkScope{Spec: NetworkScopeSpec{Provider: p}}
	return s.ResolvedTagKeys()
}

func TestObjectNameKeepsAWSIDsAndHashesTheRest(t *testing.T) {
	for _, id := range []string{"vpc-0abc123", "subnet-0123456789abcdef0"} {
		if got := ObjectName(id); got != id {
			t.Errorf("ObjectName(%q) = %q, want the ID itself", id, got)
		}
	}
	a := ObjectName("projects/alpha-1/regions/europe-west1/subnetworks/Apps_Subnet")
	b := ObjectName("projects/beta-22/regions/europe-west1/subnetworks/Apps_Subnet")
	if a == b {
		t.Errorf("two subnetworks of the same name in different projects share the object name %q", a)
	}
	if !strings.HasPrefix(a, "apps-subnet-") || len(a) != len("apps-subnet-")+objectNameHashLength {
		t.Errorf("ObjectName = %q, want apps-subnet-<10 hex digits>", a)
	}
	if again := ObjectName("projects/alpha-1/regions/europe-west1/subnetworks/Apps_Subnet"); again != a {
		t.Errorf("ObjectName is not stable: %q, then %q", a, again)
	}
	long := "projects/p/global/networks/" + strings.Repeat("n", 63)
	for _, id := range []string{long, "projects/p/global/networks/---", a + "/"} {
		name := ObjectName(id)
		if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
			t.Errorf("ObjectName(%q) = %q is not a valid name and label value: %v", id, name, errs)
		}
		if errs := validation.IsValidLabelValue(name); len(errs) > 0 {
			t.Errorf("ObjectName(%q) = %q is not a valid label value: %v", id, name, errs)
		}
	}
}
