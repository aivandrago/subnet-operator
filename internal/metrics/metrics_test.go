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

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
)

func subnet(id, owner string, available int64) awsv1alpha1.Subnet {
	return awsv1alpha1.Subnet{
		Spec:   awsv1alpha1.SubnetSpec{SubnetID: id, VPCID: "vpc-1", Account: "111111111111", Region: "eu-central-1"},
		Status: awsv1alpha1.SubnetStatus{Owner: owner, TotalIPs: 251, AvailableIPs: available, MissingTags: []string{"hs/env"}},
	}
}

func TestSetScopeReplacesSeries(t *testing.T) {
	targets := []TargetResult{{Account: "111111111111", Region: "eu-central-1", OK: true}}
	SetScope("s1", nil, []awsv1alpha1.Subnet{subnet("subnet-1", "team-a", 10), subnet("subnet-2", "", 200)}, targets, 1)
	SetScope("s2", nil, []awsv1alpha1.Subnet{subnet("subnet-9", "", 5)}, targets, 1)
	if got := testutil.CollectAndCount(subnetAvailableIPs); got != 3 {
		t.Fatalf("want 3 series, got %d", got)
	}

	// subnet-2 disappeared and subnet-1 changed owner: no stale series may remain.
	SetScope("s1", nil, []awsv1alpha1.Subnet{subnet("subnet-1", "team-b", 10)},
		[]TargetResult{{Account: "111111111111", Region: "eu-central-1", OK: false, Synced: true}}, 2)
	if got := testutil.CollectAndCount(subnetAvailableIPs); got != 2 {
		t.Fatalf("want 2 series after resync, got %d", got)
	}
	if got := testutil.ToFloat64(subnetMissingTags.WithLabelValues("s1", "111111111111", "eu-central-1", "vpc-1",
		"subnet-1", "", "", "", "team-b", "", "", "false")); got != 1 {
		t.Errorf("missing tags = %v, want 1", got)
	}
	if got := testutil.ToFloat64(targetUp.WithLabelValues("s1", "111111111111", "eu-central-1")); got != 0 {
		t.Errorf("target_up = %v, want 0", got)
	}
	if got := testutil.ToFloat64(targetSyncErrors.WithLabelValues("s1", "111111111111", "eu-central-1")); got != 1 {
		t.Errorf("sync errors = %v, want 1", got)
	}

	Forget("s1")
	if got := testutil.CollectAndCount(subnetAvailableIPs); got != 1 {
		t.Fatalf("want only s2 left, got %d", got)
	}
}
