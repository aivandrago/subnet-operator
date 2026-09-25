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

// Package migration moves objects from aws.hypersurgery/v1alpha1 to
// network.hypersurgery.dev/v1beta1 (ADR 0002 §9).
//
// A conversion webhook cannot do it: it converts between versions of one CRD, and the two
// groups are different CRDs. So the objects are copied. The mapping is one set of pure
// functions in this file, used both by the migration controller inside the operator and by
// `manager migrate-manifests`, which rewrites manifests kept in git.
//
// Only what people write is migrated: NetworkScope, SubnetClaim, ResourceImport and
// SheetExport. VPC and Subnet objects are a cache that the new NetworkScope rebuilds as
// Network and Subnet objects.
package migration

import (
	"maps"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	awsv1alpha1 "hypersurgery.dev/subnet-operator/api/v1alpha1"
	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
)

// OldAPIVersion is the apiVersion objects are migrated from, and the value of the
// migrated-from annotation on the objects the migration creates.
const OldAPIVersion = "aws.hypersurgery/v1alpha1"

// oldPrefix and newPrefix are the label and annotation key prefixes of the two groups.
const (
	oldPrefix = "aws.hypersurgery/"
	newPrefix = "network.hypersurgery.dev/"
)

// renamedKeys are the label and annotation keys whose name changes beyond the prefix.
var renamedKeys = map[string]string{
	awsv1alpha1.LabelVPC: networkv1beta1.LabelNetwork,
}

// The kinds the migration moves.
const (
	kindNetworkScope   = "NetworkScope"
	kindSubnetClaim    = "SubnetClaim"
	kindResourceImport = "ResourceImport"
	kindSheetExport    = "SheetExport"
)

// Target says where a converted object is going, which decides the metadata it keeps.
type Target int

const (
	// ForCluster is an object the migration controller creates next to the old one. It drops
	// the metadata of tools that track the old object (kubectl apply, Argo CD, Flux, Helm): a
	// copy carrying it would look like theirs, and a tool pruning what is not in git would
	// delete it.
	ForCluster Target = iota
	// ForManifest is a manifest rewritten for a git repository. Its metadata is what the
	// author wrote and stays, apart from the keys of the old group, which are renamed.
	ForManifest
)

// droppedAnnotationPrefixes and droppedLabels are the tool-tracking metadata ForCluster drops.
var (
	droppedAnnotationPrefixes = []string{
		"argocd.argoproj.io/",
		"meta.helm.sh/",
		"kustomize.toolkit.fluxcd.io/",
		"helm.toolkit.fluxcd.io/",
	}
	droppedLabels = []string{
		"app.kubernetes.io/instance",
		"app.kubernetes.io/managed-by",
		"helm.sh/chart",
	}
	droppedLabelPrefixes = []string{
		"kustomize.toolkit.fluxcd.io/",
		"helm.toolkit.fluxcd.io/",
	}
)

// Notes are what a conversion wants a human to know: a changed default made explicit, a field
// that had to move. The controller puts them in an Event, migrate-manifests on stderr.
type Notes []string

func (n *Notes) add(format string) { *n = append(*n, format) }

// objectMeta converts the metadata of an old object.
func objectMeta(old metav1.ObjectMeta, target Target) metav1.ObjectMeta {
	meta := metav1.ObjectMeta{
		Name:        old.Name,
		Namespace:   old.Namespace,
		Labels:      convertKeys(old.Labels, target, false),
		Annotations: convertKeys(old.Annotations, target, true),
	}
	// The value is the old object's group and version: which object it came from is the name
	// and namespace, the same on both sides.
	if target == ForCluster {
		if meta.Annotations == nil {
			meta.Annotations = map[string]string{}
		}
		meta.Annotations[networkv1beta1.AnnotationMigratedFrom] = OldAPIVersion
	}
	return meta
}

// convertKeys renames the old group's keys and, for the cluster, drops tool-tracking ones.
// The migration's own markers never carry over: migrated-to belongs to the old object.
func convertKeys(in map[string]string, target Target, annotations bool) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if k == networkv1beta1.AnnotationMigratedTo || k == networkv1beta1.AnnotationMigratedFrom {
			continue
		}
		if k == "kubectl.kubernetes.io/last-applied-configuration" {
			// It describes the old object: a kubectl apply of the new manifest would compute
			// its three-way diff against an object of another kind. Dropped either way.
			continue
		}
		if target == ForCluster && dropped(k, annotations) {
			continue
		}
		out[convertKey(k)] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func dropped(key string, annotation bool) bool {
	if annotation {
		for _, p := range droppedAnnotationPrefixes {
			if strings.HasPrefix(key, p) {
				return true
			}
		}
		return false
	}
	if slices.Contains(droppedLabels, key) {
		return true
	}
	for _, p := range droppedLabelPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

func convertKey(k string) string {
	if renamed, ok := renamedKeys[k]; ok {
		return renamed
	}
	if rest, ok := strings.CutPrefix(k, oldPrefix); ok {
		return newPrefix + rest
	}
	return k
}

// NetworkScope converts a scope. provider becomes AWS and the AWS settings move into the aws
// members. tagKeys is written out in full, so that the new group's provider-dependent defaults
// can never change which tags a migrated scope reads. An unset namespaceSelector becomes {}:
// unset allowed every namespace in the old group and allows none in the new one, and a
// migration must not lock teams out of their scope — the note says so, because {} is exactly
// the permissive setting the new default exists to avoid.
func NetworkScope(old *awsv1alpha1.NetworkScope, target Target) (*networkv1beta1.NetworkScope, Notes) {
	var notes Notes
	o := old.Spec
	spec := networkv1beta1.NetworkScopeSpec{
		Provider:           networkv1beta1.ProviderAWS,
		Regions:            cloneSlice(o.Regions),
		RequiredSubnetTags: cloneSlice(o.RequiredSubnetTags),
		TagKeys:            tagKeys(o.TagKeys),
		ResyncInterval:     cloneDuration(o.ResyncInterval),
		DiscoverUnmanaged:  cloneBool(o.DiscoverUnmanaged),
		AutoImport:         autoImport(o.AutoImport),
		NamespaceSelector:  o.NamespaceSelector.DeepCopy(),
	}
	for _, a := range o.Accounts {
		account := networkv1beta1.Account{ID: a.ID, Regions: cloneSlice(a.Regions)}
		if a.RoleARN != "" || a.ExternalID != "" || a.WriteRoleARN != "" {
			account.AWS = &networkv1beta1.AWSAccount{RoleARN: a.RoleARN, ExternalID: a.ExternalID, WriteRoleARN: a.WriteRoleARN}
		}
		spec.Accounts = append(spec.Accounts, account)
	}
	if len(o.VPCTagSelector) > 0 {
		spec.NetworkSelector = &networkv1beta1.NetworkSelector{MatchTags: maps.Clone(o.VPCTagSelector)}
	}
	if spec.NamespaceSelector == nil {
		spec.NamespaceSelector = &metav1.LabelSelector{}
		notes.add("spec.namespaceSelector was unset, which allowed every namespace in aws.hypersurgery/v1alpha1 and " +
			"allows none in network.hypersurgery.dev; it is now {} (every namespace) so nothing is locked out. " +
			"Replace it with a selector of the namespaces that should use the scope")
	}

	scope := &networkv1beta1.NetworkScope{
		TypeMeta:   metav1.TypeMeta{APIVersion: networkv1beta1.GroupVersion.String(), Kind: kindNetworkScope},
		ObjectMeta: objectMeta(old.ObjectMeta, target),
		Spec:       spec,
	}
	s := old.Status
	scope.Status = networkv1beta1.NetworkScopeStatus{
		// The new object has a generation of its own; zero makes its first reconcile a full
		// sync, which is what creates its Network and Subnet objects.
		ObservedGeneration: 0,
		LastSyncTime:       s.LastSyncTime.DeepCopy(),
		Networks:           s.VPCs,
		Subnets:            s.Subnets,
		Unmanaged:          s.Unmanaged,
		Conditions:         cloneConditions(s.Conditions),
	}
	for _, t := range s.Targets {
		scope.Status.Targets = append(scope.Status.Targets, networkv1beta1.TargetStatus{
			Account: t.Account, Region: t.Region, Networks: t.VPCs, Subnets: t.Subnets,
			UnmanagedNetworks: t.UnmanagedVPCs, UnmanagedSubnets: t.UnmanagedSubnets,
			UnmanagedIDs: cloneSlice(t.UnmanagedIDs), LastSyncTime: t.LastSyncTime.DeepCopy(), Error: t.Error,
		})
	}
	return scope, notes
}

// tagKeys writes out the keys the old scope used, defaults included.
func tagKeys(k awsv1alpha1.TagKeys) networkv1beta1.TagKeys {
	out := networkv1beta1.TagKeys{Owner: k.Owner, Env: k.Env, Tier: k.Tier}
	if out.Owner == "" {
		out.Owner = awsv1alpha1.DefaultOwnerTagKey
	}
	if out.Env == "" {
		out.Env = awsv1alpha1.DefaultEnvTagKey
	}
	if out.Tier == "" {
		out.Tier = awsv1alpha1.DefaultTierTagKey
	}
	return out
}

func autoImport(p *awsv1alpha1.AutoImportPolicy) *networkv1beta1.AutoImportPolicy {
	if p == nil {
		return nil
	}
	out := &networkv1beta1.AutoImportPolicy{
		Mode:               networkv1beta1.AutoImportMode(p.Mode),
		Namespace:          p.Namespace,
		InheritFromNetwork: cloneSlice(p.InheritFromVPC),
		RequiredTags:       cloneSlice(p.RequiredTags),
		ManagedTag:         p.ManagedTag,
		ManagedValue:       p.ManagedValue,
	}
	// The old CRD defaulted the managed tag; the new one leaves it to the provider's default,
	// which on AWS is the same key. Written out, it cannot change under the scope.
	if out.ManagedTag == "" {
		out.ManagedTag = awsv1alpha1.DefaultManagedTag
	}
	for _, r := range p.FromCreator {
		out.FromCreator = append(out.FromCreator, networkv1beta1.CreatorRule{
			PrincipalPrefix: r.PrincipalPrefix, Tags: maps.Clone(r.Tags)})
	}
	for _, d := range p.AccountDefaults {
		out.AccountDefaults = append(out.AccountDefaults, networkv1beta1.AccountDefault{
			Account: d.Account, Tags: maps.Clone(d.Tags)})
	}
	for _, r := range p.Skip {
		out.Skip = append(out.Skip, networkv1beta1.SkipRule{
			TagKey: r.TagKey, TagValue: r.TagValue, PrincipalPrefix: r.PrincipalPrefix})
	}
	return out
}

// SubnetClaim converts a claim, reservations included: vpcID becomes networkID,
// availabilityZones becomes zones, and the AWS-only options move into aws. Each allocation is
// keyed by the name the claim's subnet in that zone has (namePrefix and the zone suffix), which
// is the Name tag the operator already gave the subnets it created.
func SubnetClaim(old *awsv1alpha1.SubnetClaim, target Target) (*networkv1beta1.SubnetClaim, Notes) {
	o := old.Spec
	claim := &networkv1beta1.SubnetClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: networkv1beta1.GroupVersion.String(), Kind: kindSubnetClaim},
		ObjectMeta: objectMeta(old.ObjectMeta, target),
		Spec: networkv1beta1.SubnetClaimSpec{
			ScopeRef:     o.ScopeRef,
			Account:      o.Account,
			Region:       o.Region,
			NetworkID:    o.VPCID,
			PrefixLength: o.PrefixLength,
			Zones:        cloneSlice(o.AvailabilityZones),
			Mode:         networkv1beta1.ClaimMode(o.Mode),
			Owner:        o.Owner,
			Env:          o.Env,
			Tier:         o.Tier,
			NamePrefix:   o.NamePrefix,
			Tags:         maps.Clone(o.Tags),
		},
	}
	if o.RouteTableID != "" || o.MapPublicIPOnLaunch {
		claim.Spec.AWS = &networkv1beta1.AWSClaimOptions{
			RouteTableID: o.RouteTableID, MapPublicIPOnLaunch: o.MapPublicIPOnLaunch}
	}

	prefix := claim.NamePrefixOrName()
	claim.Status = networkv1beta1.SubnetClaimStatus{
		ObservedGeneration: 0,
		Conditions:         cloneConditions(old.Status.Conditions),
	}
	for _, a := range old.Status.Allocations {
		claim.Status.Allocations = append(claim.Status.Allocations, networkv1beta1.SubnetAllocation{
			Name:      networkv1beta1.SubnetName(prefix, o.Region, a.AvailabilityZone),
			Zone:      a.AvailabilityZone,
			CIDRBlock: a.CIDRBlock,
			SubnetID:  a.SubnetID,
			State:     a.State,
			Error:     a.Error,
		})
	}
	return claim, nil
}

// ResourceImport converts an import and its history. Nothing in it changes shape: resourceID
// already was the provider's ID.
func ResourceImport(old *awsv1alpha1.ResourceImport, target Target) (*networkv1beta1.ResourceImport, Notes) {
	o := old.Spec
	imp := &networkv1beta1.ResourceImport{
		TypeMeta:   metav1.TypeMeta{APIVersion: networkv1beta1.GroupVersion.String(), Kind: kindResourceImport},
		ObjectMeta: objectMeta(old.ObjectMeta, target),
		Spec: networkv1beta1.ResourceImportSpec{
			ScopeRef:    o.ScopeRef,
			Account:     o.Account,
			Region:      o.Region,
			ResourceID:  o.ResourceID,
			Tags:        maps.Clone(o.Tags),
			RequestedBy: o.RequestedBy,
			DryRun:      o.DryRun,
		},
		Status: networkv1beta1.ResourceImportStatus{
			ObservedGeneration: 0,
			State:              old.Status.State,
			AppliedTags:        maps.Clone(old.Status.AppliedTags),
			AppliedTime:        old.Status.AppliedTime.DeepCopy(),
			Error:              old.Status.Error,
			Conditions:         cloneConditions(old.Status.Conditions),
		},
	}
	return imp, nil
}

// SheetExport converts an export. It was cloud-neutral already.
func SheetExport(old *awsv1alpha1.SheetExport, target Target) (*networkv1beta1.SheetExport, Notes) {
	o := old.Spec
	exp := &networkv1beta1.SheetExport{
		TypeMeta:   metav1.TypeMeta{APIVersion: networkv1beta1.GroupVersion.String(), Kind: kindSheetExport},
		ObjectMeta: objectMeta(old.ObjectMeta, target),
		Spec: networkv1beta1.SheetExportSpec{
			ScopeRef:      o.ScopeRef,
			SpreadsheetID: o.SpreadsheetID,
			SheetName:     o.SheetName,
			CredentialsSecretRef: networkv1beta1.SecretKeyRef{
				Name: o.CredentialsSecretRef.Name, Namespace: o.CredentialsSecretRef.Namespace, Key: o.CredentialsSecretRef.Key},
			ExtraTagColumns: cloneSlice(o.ExtraTagColumns),
			RefreshInterval: cloneDuration(o.RefreshInterval),
		},
		Status: networkv1beta1.SheetExportStatus{
			ObservedGeneration: 0,
			LastExportTime:     old.Status.LastExportTime.DeepCopy(),
			Rows:               old.Status.Rows,
			URL:                old.Status.URL,
			Conditions:         cloneConditions(old.Status.Conditions),
		},
	}
	return exp, nil
}

func cloneSlice[T any](s []T) []T {
	if s == nil {
		return nil
	}
	return append([]T(nil), s...)
}

func cloneDuration(d *metav1.Duration) *metav1.Duration {
	if d == nil {
		return nil
	}
	c := *d
	return &c
}

func cloneBool(b *bool) *bool {
	if b == nil {
		return nil
	}
	c := *b
	return &c
}

func cloneConditions(c []metav1.Condition) []metav1.Condition {
	if c == nil {
		return nil
	}
	out := make([]metav1.Condition, len(c))
	for i := range c {
		c[i].DeepCopyInto(&out[i])
	}
	return out
}
