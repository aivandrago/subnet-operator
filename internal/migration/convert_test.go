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

package migration

import (
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	awsv1alpha1 "hypersurgery.dev/subnet-operator/internal/migration/v1alpha1"
)

func oldScope() *awsv1alpha1.NetworkScope {
	synced := metav1.NewTime(time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC))
	return &awsv1alpha1.NetworkScope{
		ObjectMeta: metav1.ObjectMeta{Name: "organization", Generation: 7},
		Spec: awsv1alpha1.NetworkScopeSpec{
			Accounts: []awsv1alpha1.AccountSpec{
				{ID: "111111111111"},
				{ID: "222222222222", RoleARN: "arn:aws:iam::222222222222:role/read", ExternalID: "x",
					WriteRoleARN: "arn:aws:iam::222222222222:role/write", Regions: []string{"us-east-1"}},
			},
			Regions:            []string{"eu-central-1"},
			VPCTagSelector:     map[string]string{"hs/managed": "true"},
			RequiredSubnetTags: []string{"hs/owner"},
			TagKeys:            awsv1alpha1.TagKeys{Owner: "team"},
			ResyncInterval:     &metav1.Duration{Duration: 5 * time.Minute},
			DiscoverUnmanaged:  new(false),
			AutoImport: &awsv1alpha1.AutoImportPolicy{
				Mode: awsv1alpha1.AutoImportDryRun, Namespace: "platform", InheritFromVPC: []string{"hs/owner"},
				AccountDefaults: []awsv1alpha1.AccountDefault{{Account: "111111111111", Tags: map[string]string{"hs/owner": "p"}}},
				FromCreator:     []awsv1alpha1.CreatorRule{{PrincipalPrefix: "arn:aws:sts::1:assumed-role/x/", Tags: map[string]string{"a": "b"}}},
				Skip:            []awsv1alpha1.SkipRule{{TagKey: "managed-by", TagValue: "terraform"}},
			},
		},
		Status: awsv1alpha1.NetworkScopeStatus{
			ObservedGeneration: 7, LastSyncTime: &synced, VPCs: 3, Subnets: 9, Unmanaged: 2,
			Targets: []awsv1alpha1.TargetStatus{{Account: "111111111111", Region: "eu-central-1", VPCs: 3, Subnets: 9,
				UnmanagedVPCs: 1, UnmanagedSubnets: 1, UnmanagedIDs: []string{"subnet-2", "vpc-1"}, LastSyncTime: &synced}},
		},
	}
}

func TestNetworkScope(t *testing.T) {
	scope, notes := NetworkScope(oldScope())
	s := scope.Spec

	if s.Provider != networkv1.ProviderAWS {
		t.Errorf("provider = %q, want AWS", s.Provider)
	}
	if s.Accounts[0].AWS != nil {
		t.Errorf("an account reached with the operator's own identity gets no aws member, got %+v", s.Accounts[0].AWS)
	}
	wantAWS := networkv1.AWSAccount{RoleARN: "arn:aws:iam::222222222222:role/read", ExternalID: "x",
		WriteRoleARN: "arn:aws:iam::222222222222:role/write"}
	if got := s.Accounts[1].AWS; got == nil || *got != wantAWS {
		t.Errorf("aws = %+v, want %+v", got, wantAWS)
	}
	if !reflect.DeepEqual(s.Accounts[1].Regions, []string{"us-east-1"}) {
		t.Errorf("account regions = %v", s.Accounts[1].Regions)
	}
	if s.NetworkSelector == nil || s.NetworkSelector.MatchTags["hs/managed"] != "true" {
		t.Errorf("vpcTagSelector did not become networkSelector.matchTags: %+v", s.NetworkSelector)
	}
	// Written out in full: the defaults of the old group, whatever the new one defaults to.
	if want := (networkv1.TagKeys{Owner: "team", Env: "hs/env", Tier: "hs/tier"}); s.TagKeys != want {
		t.Errorf("tagKeys = %+v, want %+v", s.TagKeys, want)
	}
	if s.ResyncInterval.Duration != 5*time.Minute || s.DiscoverUnmanaged == nil || *s.DiscoverUnmanaged {
		t.Errorf("resyncInterval %v, discoverUnmanaged %v", s.ResyncInterval, s.DiscoverUnmanaged)
	}
	p := s.AutoImport
	if p.Mode != networkv1.AutoImportDryRun || p.Namespace != "platform" ||
		!reflect.DeepEqual(p.InheritFromNetwork, []string{"hs/owner"}) || p.ManagedTag != "hs/managed" ||
		len(p.AccountDefaults) != 1 || len(p.FromCreator) != 1 || len(p.Skip) != 1 {
		t.Errorf("autoImport = %+v", p)
	}

	// The one change of meaning: unset allowed every namespace and now allows none.
	if s.NamespaceSelector == nil || len(s.NamespaceSelector.MatchLabels) != 0 || len(s.NamespaceSelector.MatchExpressions) != 0 {
		t.Errorf("an unset namespaceSelector must become {}, got %+v", s.NamespaceSelector)
	}
	if len(notes) != 1 || !strings.Contains(notes[0], "namespaceSelector") {
		t.Errorf("the conversion must say that it opened the scope to every namespace, notes: %q", notes)
	}

}

// A manifest carries no state: whatever status the old document had (a `kubectl get -o yaml`
// export has one) is not converted, since apply ignores it and 0.9 has nothing to copy it into.
func TestNetworkScopeLeavesStatusOut(t *testing.T) {
	scope, _ := NetworkScope(oldScope())
	if !reflect.DeepEqual(scope.Status, networkv1.NetworkScopeStatus{}) {
		t.Errorf("status = %+v, want none", scope.Status)
	}
}

func TestNetworkScopeKeepsASelector(t *testing.T) {
	old := oldScope()
	old.Spec.NamespaceSelector = &metav1.LabelSelector{MatchLabels: map[string]string{"team": "payments"}}
	scope, notes := NetworkScope(old)
	if !reflect.DeepEqual(scope.Spec.NamespaceSelector, old.Spec.NamespaceSelector) {
		t.Errorf("namespaceSelector = %+v", scope.Spec.NamespaceSelector)
	}
	if len(notes) != 0 {
		t.Errorf("nothing to say about a scope that had a selector, got %q", notes)
	}
	old.Spec.NamespaceSelector = &metav1.LabelSelector{}
	if scope, _ := NetworkScope(old); scope.Spec.NamespaceSelector == nil {
		t.Error("an explicit {} stays {}")
	}
}

func TestSubnetClaim(t *testing.T) {
	old := &awsv1alpha1.SubnetClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "payments", Namespace: "team-payments"},
		Spec: awsv1alpha1.SubnetClaimSpec{
			ScopeRef: "organization", Account: "222222222222", Region: "eu-central-1", VPCID: "vpc-0abc",
			PrefixLength: 24, AvailabilityZones: []string{"eu-central-1a", "eu-central-1b"},
			Mode: awsv1alpha1.ClaimModeCreate, Owner: "team-payments", Env: "prod", Tier: "db",
			NamePrefix: "payments-db", Tags: map[string]string{"cost-center": "42"},
			RouteTableID: "rtb-0abc", MapPublicIPOnLaunch: true,
		},
		Status: awsv1alpha1.SubnetClaimStatus{
			ObservedGeneration: 4,
			Allocations: []awsv1alpha1.SubnetAllocation{
				{AvailabilityZone: "eu-central-1a", CIDRBlock: "10.0.0.0/24", SubnetID: "subnet-a", State: "Created"},
				{AvailabilityZone: "eu-central-1b", CIDRBlock: "10.0.1.0/24", State: "Failed", Error: "boom"},
			},
			Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "CreateFailed"}},
		},
	}
	claim, _ := SubnetClaim(old)
	s := claim.Spec
	if s.NetworkID != "vpc-0abc" || !reflect.DeepEqual(s.Zones, old.Spec.AvailabilityZones) ||
		s.Mode != networkv1.ClaimModeCreate || s.NamePrefix != "payments-db" || s.Tags["cost-center"] != "42" {
		t.Errorf("spec = %+v", s)
	}
	if s.AWS == nil || s.AWS.RouteTableID != "rtb-0abc" || !s.AWS.MapPublicIPOnLaunch {
		t.Errorf("aws = %+v", s.AWS)
	}
	if len(claim.Status.Allocations) != 0 || len(claim.Status.Conditions) != 0 {
		t.Errorf("status = %+v; a manifest carries no reservations", claim.Status)
	}

	old.Spec.RouteTableID, old.Spec.MapPublicIPOnLaunch = "", false
	claim, _ = SubnetClaim(old)
	if claim.Spec.AWS != nil {
		t.Errorf("no AWS options, no aws member; got %+v", claim.Spec.AWS)
	}
}

func TestResourceImportAndSheetExport(t *testing.T) {
	applied := metav1.NewTime(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
	imp, _ := ResourceImport(&awsv1alpha1.ResourceImport{
		ObjectMeta: metav1.ObjectMeta{Name: "i", Namespace: "n"},
		Spec: awsv1alpha1.ResourceImportSpec{ScopeRef: "s", Account: "111111111111", Region: "eu-central-1",
			ResourceID: "vpc-1", Tags: map[string]string{"hs/owner": "a"}, RequestedBy: "jane", DryRun: true},
		Status: awsv1alpha1.ResourceImportStatus{State: "Applied", AppliedTags: map[string]string{"hs/owner": "a"},
			AppliedTime: &applied},
	})
	if imp.Spec.ResourceID != "vpc-1" || imp.Spec.RequestedBy != "jane" || !imp.Spec.DryRun ||
		imp.Status.State != "" || imp.Status.AppliedTime != nil {
		t.Errorf("import = %+v", imp)
	}

	exp, _ := SheetExport(&awsv1alpha1.SheetExport{
		ObjectMeta: metav1.ObjectMeta{Name: "e"},
		Spec: awsv1alpha1.SheetExportSpec{ScopeRef: "s", SpreadsheetID: "id", SheetName: "Subnets",
			CredentialsSecretRef: awsv1alpha1.SecretKeyRef{Name: "g", Namespace: "ns", Key: "k"},
			ExtraTagColumns:      []string{"cc"}},
		Status: awsv1alpha1.SheetExportStatus{Rows: 12, URL: "https://example.com", LastExportTime: &applied},
	})
	if exp.Spec.CredentialsSecretRef.Key != "k" || exp.Spec.ExtraTagColumns[0] != "cc" || exp.Status.Rows != 0 {
		t.Errorf("export = %+v", exp)
	}
}

func TestMetadata(t *testing.T) {
	old := metav1.ObjectMeta{
		Name: "payments", Namespace: "team",
		Labels: map[string]string{
			"aws.hypersurgery/scope":     "org",
			"aws.hypersurgery/vpc":       "vpc-1",
			"app.kubernetes.io/instance": "payments-app",
			"team":                       "payments",
		},
		Annotations: map[string]string{
			awsv1alpha1.AnnotationCreatedBy:                    "jane@example.com",
			"aws.hypersurgery/reason":                          "tags from creator rule",
			"kubectl.kubernetes.io/last-applied-configuration": "{}",
			"argocd.argoproj.io/tracking-id":                   "app:group/kind:ns/name",
			networkv1.AnnotationMigratedTo:                     "payments",
			"note":                                             "kept",
		},
	}

	// A manifest keeps what its author wrote, tool metadata included, with the old group's keys
	// renamed. It is not marked as a copy the operator made: it is applied by a person or a
	// GitOps tool, whose own name is the creator.
	manifest := objectMeta(old)
	wantLabels := map[string]string{
		networkv1.LabelScope:         "org",
		networkv1.LabelNetwork:       "vpc-1",
		"app.kubernetes.io/instance": "payments-app",
		"team":                       "payments",
	}
	if !reflect.DeepEqual(manifest.Labels, wantLabels) {
		t.Errorf("labels = %v, want %v", manifest.Labels, wantLabels)
	}
	wantAnnotations := map[string]string{
		networkv1.AnnotationCreatedBy:    "jane@example.com",
		networkv1.AnnotationReason:       "tags from creator rule",
		"argocd.argoproj.io/tracking-id": "app:group/kind:ns/name",
		"note":                           "kept",
	}
	if !reflect.DeepEqual(manifest.Annotations, wantAnnotations) {
		t.Errorf("annotations = %v, want %v", manifest.Annotations, wantAnnotations)
	}
	if manifest.Labels["app.kubernetes.io/instance"] != "payments-app" {
		t.Errorf("a manifest keeps its labels, got %v", manifest.Labels)
	}
	if _, ok := manifest.Annotations[networkv1.AnnotationMigratedFrom]; ok {
		t.Error("a manifest must not carry the migration marker")
	}
	if _, ok := manifest.Annotations["kubectl.kubernetes.io/last-applied-configuration"]; ok {
		t.Error("the last applied configuration describes the old object and must go")
	}
}
