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

package controller

import (
	"maps"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// The claim tag is always written on AWS, and on GCP only when the scope binds it.
func TestBindsClaimTag(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target inventory.Target
		want   bool
	}{
		{"AWS", inventory.Target{Provider: networkv1.ProviderAWS}, true},
		{"GCP without settings", inventory.Target{Provider: networkv1.ProviderGCP}, false},
		{"GCP unset", inventory.Target{Provider: networkv1.ProviderGCP, GCP: &networkv1.GCPScope{}}, false},
		{"GCP Skip", inventory.Target{Provider: networkv1.ProviderGCP,
			GCP: &networkv1.GCPScope{ClaimTag: networkv1.GCPClaimTagSkip}}, false},
		{"GCP Bind", inventory.Target{Provider: networkv1.ProviderGCP,
			GCP: &networkv1.GCPScope{ClaimTag: networkv1.GCPClaimTagBind}}, true},
	} {
		if got := bindsClaimTag(tc.target); got != tc.want {
			t.Errorf("%s: bindsClaimTag = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// Without the claim tag, a claim cannot put one on its subnetwork through spec.tags either:
// the key is the operator's.
func TestSubnetTagsWithoutTheClaimTag(t *testing.T) {
	claim := &networkv1.SubnetClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout", Namespace: "payments"},
		Spec: networkv1.SubnetClaimSpec{Owner: "team-payments", Env: "prod",
			Tags: map[string]string{"hs-claim": "other_claim", "cost-center": "42"}},
	}
	got := subnetTags(claim, networkv1.ProviderGCP, false, "")
	want := map[string]string{"hs-owner": "team-payments", "hs-env": "prod", "hs-managed-by": "subnet-operator",
		"cost-center": "42"}
	if !maps.Equal(got, want) {
		t.Errorf("tags = %v, want %v", got, want)
	}
	got = subnetTags(claim, networkv1.ProviderGCP, true, "")
	if got["hs-claim"] != "payments_checkout" {
		t.Errorf("with the claim tag: hs-claim = %q, want payments_checkout", got["hs-claim"])
	}
}
