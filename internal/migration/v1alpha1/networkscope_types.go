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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Default tag keys used when a NetworkScope does not override them.
const (
	DefaultOwnerTagKey = "hs/owner"
	DefaultEnvTagKey   = "hs/env"
	DefaultTierTagKey  = "hs/tier"
)

// AccountSpec describes one AWS account the operator discovers.
type AccountSpec struct {
	// id is the 12-digit AWS account ID.
	ID string `json:"id"`

	// roleARN is assumed (sts:AssumeRole) to reach the account.
	// When empty, the operator's own credentials (EKS Pod Identity / IRSA) are used,
	// which only works for the account the operator runs in.
	RoleARN string `json:"roleARN,omitempty"`

	// externalID is passed to sts:AssumeRole when the role trust policy requires it.
	ExternalID string `json:"externalID,omitempty"`

	// writeRoleARN is assumed for SubnetClaims in Create mode. It is separate from roleARN
	// so that discovery never carries write permissions. When empty, claims for this
	// account can only be allocated, not created.
	WriteRoleARN string `json:"writeRoleARN,omitempty"`

	// regions overrides spec.regions for this account.
	Regions []string `json:"regions,omitempty"`
}

// TagKeys names the tags that carry inventory metadata.
type TagKeys struct {
	// owner is the tag key holding the owning team.
	Owner string `json:"owner,omitempty"`

	// env is the tag key holding the environment.
	Env string `json:"env,omitempty"`

	// tier is the tag key holding the subnet tier (public, private, db, ...).
	Tier string `json:"tier,omitempty"`
}

// NetworkScopeSpec defines which AWS accounts and regions are discovered.
type NetworkScopeSpec struct {
	// accounts to discover.
	Accounts []AccountSpec `json:"accounts"`

	// regions to discover in every account (unless the account overrides them).
	Regions []string `json:"regions"`

	// vpcTagSelector limits discovery to VPCs that carry all of these tags.
	// An empty value matches any value of the key. All subnets of a selected VPC
	// are discovered, including untagged ones, so they show up in compliance reports.
	// When the selector is empty, every VPC is discovered.
	VPCTagSelector map[string]string `json:"vpcTagSelector,omitempty"`

	// requiredSubnetTags are tag keys every subnet must have. Missing keys are
	// reported in the Subnet status and as metrics.
	RequiredSubnetTags []string `json:"requiredSubnetTags,omitempty"`

	// tagKeys maps inventory fields (owner, env, tier) to tag keys.
	TagKeys TagKeys `json:"tagKeys,omitzero"`

	// resyncInterval is how often the full inventory is refreshed.
	ResyncInterval *metav1.Duration `json:"resyncInterval,omitempty"`

	// discoverUnmanaged also lists the VPCs the selector does not match, and their subnets,
	// so resources nobody has claimed are counted and can be imported instead of staying
	// invisible. They are reported and alerted on, never mirrored as objects.
	DiscoverUnmanaged *bool `json:"discoverUnmanaged,omitempty"`

	// autoImport tags unmanaged resources on its own, deriving the tags from who created the
	// resource, from its VPC, or from the account. Off unless configured.
	AutoImport *AutoImportPolicy `json:"autoImport,omitempty"`

	// namespaceSelector selects the namespaces whose SubnetClaims and ResourceImports may refer
	// to this scope. Referring to a scope is what makes the operator use its roles, including
	// the write roles, on the object's behalf, so this is what keeps one team's namespace from
	// creating subnets in, or tagging, another team's accounts.
	//
	// Select namespaces by name with the kubernetes.io/metadata.name label, which the API
	// server sets on every namespace and nobody can change, or by a label only cluster
	// administrators can set. An empty selector ({}) allows every namespace on purpose.
	//
	// When unset, every namespace may use the scope, as in earlier releases, and the operator
	// warns about it. The v1 API will allow no namespace when the field is unset.
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
}

// TargetStatus reports the last sync of one account/region pair.
type TargetStatus struct {
	// account is the AWS account ID.
	Account string `json:"account"`
	// region is the AWS region.
	Region string `json:"region"`
	// vpcs is the number of VPCs discovered.
	VPCs int32 `json:"vpcs"`
	// subnets is the number of subnets discovered.
	Subnets int32 `json:"subnets"`
	// unmanagedVPCs is the number of VPCs the tag selector does not match.
	UnmanagedVPCs int32 `json:"unmanagedVPCs,omitempty"`
	// unmanagedSubnets is the number of subnets inside those VPCs.
	UnmanagedSubnets int32 `json:"unmanagedSubnets,omitempty"`
	// unmanagedIDs are the VPC and subnet IDs that were outside the selector at the last
	// successful sync. The operator keeps them here rather than in memory so that a restart or
	// a change of leader can tell a resource that appeared since then from one that was
	// already known — otherwise every known unmanaged resource would be reported as new again.
	UnmanagedIDs []string `json:"unmanagedIDs,omitempty"`
	// lastSyncTime is the time of the last successful sync.
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`
	// error is the last sync error, empty when the last sync succeeded.
	Error string `json:"error,omitempty"`
}

// NetworkScopeStatus defines the observed state of NetworkScope.
type NetworkScopeStatus struct {
	// observedGeneration is the generation of the spec that was last synced.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// lastSyncTime is when the last full sync finished.
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// vpcs is the number of VPCs discovered across all targets.
	VPCs int32 `json:"vpcs,omitempty"`

	// subnets is the number of subnets discovered across all targets.
	Subnets int32 `json:"subnets,omitempty"`

	// unmanaged is the number of discovered resources without the managed tag, across all
	// targets. Anything above zero is something nobody has taken responsibility for.
	Unmanaged int32 `json:"unmanaged,omitempty"`

	// targets reports each account/region pair.
	Targets []TargetStatus `json:"targets,omitempty"`

	// conditions: Ready is True when every target synced successfully.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// NetworkScope selects AWS accounts and regions whose VPCs and subnets are discovered.
type NetworkScope struct {
	TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of NetworkScope
	Spec NetworkScopeSpec `json:"spec"`

	// status defines the observed state of NetworkScope
	Status NetworkScopeStatus `json:"status,omitzero"`
}
