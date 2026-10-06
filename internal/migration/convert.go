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

// Package migration is what is left of the move from aws.hypersurgery/v1alpha1 to
// network.hypersurgery.dev/v1 (ADR 0002 §9) once the old group is gone (0.9).
//
// 0.8 served both groups and copied every object, status included, into the new one with a
// migration controller. 0.9 serves only the new group. Two things remain:
//
//   - the mapping, as pure functions in this file, used by `manager migrate-manifests` to
//     rewrite manifests kept in git. Manifests carry no state, so only metadata and spec are
//     converted;
//   - the guard (guard.go), which keeps the operator from running next to old objects that
//     0.8 never migrated, whose state would otherwise be ignored.
//
// Only what people write is converted: NetworkScope, SubnetClaim, ResourceImport and
// SheetExport. VPC and Subnet objects were a cache that the new NetworkScope rebuilds as
// Network and Subnet objects.
package migration

import (
	"maps"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	awsv1alpha1 "hypersurgery.dev/subnet-operator/internal/migration/v1alpha1"
)

// OldAPIVersion is the apiVersion manifests are converted from.
const OldAPIVersion = awsv1alpha1.APIVersion

// oldPrefix and newPrefix are the label and annotation key prefixes of the two groups.
const (
	oldPrefix = "aws.hypersurgery/"
	newPrefix = "network.hypersurgery.dev/"
)

// renamedKeys are the label and annotation keys whose name changes beyond the prefix.
var renamedKeys = map[string]string{
	awsv1alpha1.LabelVPC: networkv1.LabelNetwork,
}

// The kinds the migration moves.
const (
	kindNetworkScope   = "NetworkScope"
	kindSubnetClaim    = "SubnetClaim"
	kindResourceImport = "ResourceImport"
	kindSheetExport    = "SheetExport"
)

// Notes are what a conversion wants a human to know: a changed default made explicit, a field
// that had to move. migrate-manifests writes them to stderr.
type Notes []string

func (n *Notes) add(format string) { *n = append(*n, format) }

// objectMeta converts the metadata of an old object: what the author wrote stays, apart from
// the keys of the old group, which are renamed.
func objectMeta(old metav1.ObjectMeta) metav1.ObjectMeta {
	return metav1.ObjectMeta{
		Name:        old.Name,
		Namespace:   old.Namespace,
		Labels:      convertKeys(old.Labels),
		Annotations: convertKeys(old.Annotations),
	}
}

// convertKeys renames the old group's keys. The migration's own markers never carry over:
// migrated-to belonged to the old object, migrated-from to a copy 0.8 made in the cluster.
func convertKeys(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if k == networkv1.AnnotationMigratedTo || k == networkv1.AnnotationMigratedFrom {
			continue
		}
		if k == "kubectl.kubernetes.io/last-applied-configuration" {
			// It describes the old object: a kubectl apply of the new manifest would compute
			// its three-way diff against an object of another kind.
			continue
		}
		out[convertKey(k)] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
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
func NetworkScope(old *awsv1alpha1.NetworkScope) (*networkv1.NetworkScope, Notes) {
	var notes Notes
	o := old.Spec
	spec := networkv1.NetworkScopeSpec{
		Provider:           networkv1.ProviderAWS,
		Regions:            cloneSlice(o.Regions),
		RequiredSubnetTags: cloneSlice(o.RequiredSubnetTags),
		TagKeys:            tagKeys(o.TagKeys),
		ResyncInterval:     cloneDuration(o.ResyncInterval),
		DiscoverUnmanaged:  cloneBool(o.DiscoverUnmanaged),
		AutoImport:         autoImport(o.AutoImport),
		NamespaceSelector:  o.NamespaceSelector.DeepCopy(),
	}
	for _, a := range o.Accounts {
		account := networkv1.Account{ID: a.ID, Regions: cloneSlice(a.Regions)}
		if a.RoleARN != "" || a.ExternalID != "" || a.WriteRoleARN != "" {
			account.AWS = &networkv1.AWSAccount{RoleARN: a.RoleARN, ExternalID: a.ExternalID, WriteRoleARN: a.WriteRoleARN}
		}
		spec.Accounts = append(spec.Accounts, account)
	}
	if len(o.VPCTagSelector) > 0 {
		spec.NetworkSelector = &networkv1.NetworkSelector{MatchTags: maps.Clone(o.VPCTagSelector)}
	}
	if spec.NamespaceSelector == nil {
		spec.NamespaceSelector = &metav1.LabelSelector{}
		notes.add("spec.namespaceSelector was unset, which allowed every namespace in aws.hypersurgery/v1alpha1 and " +
			"allows none in network.hypersurgery.dev; it is now {} (every namespace) so nothing is locked out. " +
			"Replace it with a selector of the namespaces that should use the scope")
	}

	scope := &networkv1.NetworkScope{
		TypeMeta:   metav1.TypeMeta{APIVersion: networkv1.GroupVersion.String(), Kind: kindNetworkScope},
		ObjectMeta: objectMeta(old.ObjectMeta),
		Spec:       spec,
	}
	return scope, notes
}

// tagKeys writes out the keys the old scope used, defaults included.
func tagKeys(k awsv1alpha1.TagKeys) networkv1.TagKeys {
	out := networkv1.TagKeys{Owner: k.Owner, Env: k.Env, Tier: k.Tier}
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

func autoImport(p *awsv1alpha1.AutoImportPolicy) *networkv1.AutoImportPolicy {
	if p == nil {
		return nil
	}
	out := &networkv1.AutoImportPolicy{
		Mode:               networkv1.AutoImportMode(p.Mode),
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
		out.FromCreator = append(out.FromCreator, networkv1.CreatorRule{
			PrincipalPrefix: r.PrincipalPrefix, Tags: maps.Clone(r.Tags)})
	}
	for _, d := range p.AccountDefaults {
		out.AccountDefaults = append(out.AccountDefaults, networkv1.AccountDefault{
			Account: d.Account, Tags: maps.Clone(d.Tags)})
	}
	for _, r := range p.Skip {
		out.Skip = append(out.Skip, networkv1.SkipRule{
			TagKey: r.TagKey, TagValue: r.TagValue, PrincipalPrefix: r.PrincipalPrefix})
	}
	return out
}

// SubnetClaim converts a claim: vpcID becomes networkID,
// availabilityZones becomes zones, and the AWS-only options move into aws.
func SubnetClaim(old *awsv1alpha1.SubnetClaim) (*networkv1.SubnetClaim, Notes) {
	o := old.Spec
	claim := &networkv1.SubnetClaim{
		TypeMeta:   metav1.TypeMeta{APIVersion: networkv1.GroupVersion.String(), Kind: kindSubnetClaim},
		ObjectMeta: objectMeta(old.ObjectMeta),
		Spec: networkv1.SubnetClaimSpec{
			ScopeRef:     o.ScopeRef,
			Account:      o.Account,
			Region:       o.Region,
			NetworkID:    o.VPCID,
			PrefixLength: o.PrefixLength,
			Zones:        cloneSlice(o.AvailabilityZones),
			Mode:         networkv1.ClaimMode(o.Mode),
			Owner:        o.Owner,
			Env:          o.Env,
			Tier:         o.Tier,
			NamePrefix:   o.NamePrefix,
			Tags:         maps.Clone(o.Tags),
		},
	}
	if o.RouteTableID != "" || o.MapPublicIPOnLaunch {
		claim.Spec.AWS = &networkv1.AWSClaimOptions{
			RouteTableID: o.RouteTableID, MapPublicIPOnLaunch: o.MapPublicIPOnLaunch}
	}

	return claim, nil
}

// ResourceImport converts an import. Nothing in it changes shape: resourceID
// already was the provider's ID.
func ResourceImport(old *awsv1alpha1.ResourceImport) (*networkv1.ResourceImport, Notes) {
	o := old.Spec
	imp := &networkv1.ResourceImport{
		TypeMeta:   metav1.TypeMeta{APIVersion: networkv1.GroupVersion.String(), Kind: kindResourceImport},
		ObjectMeta: objectMeta(old.ObjectMeta),
		Spec: networkv1.ResourceImportSpec{
			ScopeRef:    o.ScopeRef,
			Account:     o.Account,
			Region:      o.Region,
			ResourceID:  o.ResourceID,
			Tags:        maps.Clone(o.Tags),
			RequestedBy: o.RequestedBy,
			DryRun:      o.DryRun,
		},
	}
	return imp, nil
}

// SheetExport converts an export. It was cloud-neutral already.
func SheetExport(old *awsv1alpha1.SheetExport) (*networkv1.SheetExport, Notes) {
	o := old.Spec
	exp := &networkv1.SheetExport{
		TypeMeta:   metav1.TypeMeta{APIVersion: networkv1.GroupVersion.String(), Kind: kindSheetExport},
		ObjectMeta: objectMeta(old.ObjectMeta),
		Spec: networkv1.SheetExportSpec{
			ScopeRef:      o.ScopeRef,
			SpreadsheetID: o.SpreadsheetID,
			SheetName:     o.SheetName,
			CredentialsSecretRef: networkv1.SecretKeyRef{
				Name: o.CredentialsSecretRef.Name, Namespace: o.CredentialsSecretRef.Namespace, Key: o.CredentialsSecretRef.Key},
			ExtraTagColumns: cloneSlice(o.ExtraTagColumns),
			RefreshInterval: cloneDuration(o.RefreshInterval),
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
