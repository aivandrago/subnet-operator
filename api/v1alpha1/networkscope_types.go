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
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
	// +kubebuilder:validation:Pattern=`^[0-9]{12}$`
	// +required
	ID string `json:"id"`

	// roleARN is assumed (sts:AssumeRole) to reach the account.
	// When empty, the operator's own credentials (EKS Pod Identity / IRSA) are used,
	// which only works for the account the operator runs in.
	// +kubebuilder:validation:Pattern=`^arn:aws[a-z-]*:iam::[0-9]{12}:role/.+$`
	// +optional
	RoleARN string `json:"roleARN,omitempty"`

	// externalID is passed to sts:AssumeRole when the role trust policy requires it.
	// +optional
	ExternalID string `json:"externalID,omitempty"`

	// writeRoleARN is assumed for SubnetClaims in Create mode. It is separate from roleARN
	// so that discovery never carries write permissions. When empty, claims for this
	// account can only be allocated, not created.
	// +kubebuilder:validation:Pattern=`^arn:aws[a-z-]*:iam::[0-9]{12}:role/.+$`
	// +optional
	WriteRoleARN string `json:"writeRoleARN,omitempty"`

	// regions overrides spec.regions for this account.
	// +optional
	Regions []string `json:"regions,omitempty"`
}

// TagKeys names the tags that carry inventory metadata.
type TagKeys struct {
	// owner is the tag key holding the owning team.
	// +kubebuilder:default="hs/owner"
	// +optional
	Owner string `json:"owner,omitempty"`

	// env is the tag key holding the environment.
	// +kubebuilder:default="hs/env"
	// +optional
	Env string `json:"env,omitempty"`

	// tier is the tag key holding the subnet tier (public, private, db, ...).
	// +kubebuilder:default="hs/tier"
	// +optional
	Tier string `json:"tier,omitempty"`
}

// NetworkScopeSpec defines which AWS accounts and regions are discovered.
type NetworkScopeSpec struct {
	// accounts to discover.
	// +kubebuilder:validation:MinItems=1
	// +listType=map
	// +listMapKey=id
	// +required
	Accounts []AccountSpec `json:"accounts"`

	// regions to discover in every account (unless the account overrides them).
	// +kubebuilder:validation:MinItems=1
	// +required
	Regions []string `json:"regions"`

	// vpcTagSelector limits discovery to VPCs that carry all of these tags.
	// An empty value matches any value of the key. All subnets of a selected VPC
	// are discovered, including untagged ones, so they show up in compliance reports.
	// When the selector is empty, every VPC is discovered.
	// +optional
	VPCTagSelector map[string]string `json:"vpcTagSelector,omitempty"`

	// requiredSubnetTags are tag keys every subnet must have. Missing keys are
	// reported in the Subnet status and as metrics.
	// +optional
	RequiredSubnetTags []string `json:"requiredSubnetTags,omitempty"`

	// tagKeys maps inventory fields (owner, env, tier) to tag keys.
	// +optional
	TagKeys TagKeys `json:"tagKeys,omitzero"`

	// resyncInterval is how often the full inventory is refreshed.
	// +kubebuilder:default="10m"
	// +optional
	ResyncInterval *metav1.Duration `json:"resyncInterval,omitempty"`

	// discoverUnmanaged also lists the VPCs the selector does not match, and their subnets,
	// so resources nobody has claimed are counted and can be imported instead of staying
	// invisible. They are reported and alerted on, never mirrored as objects.
	// +kubebuilder:default=true
	// +optional
	DiscoverUnmanaged *bool `json:"discoverUnmanaged,omitempty"`

	// autoImport tags unmanaged resources on its own, deriving the tags from who created the
	// resource, from its VPC, or from the account. Off unless configured.
	// +optional
	AutoImport *AutoImportPolicy `json:"autoImport,omitempty"`
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
	// +optional
	UnmanagedVPCs int32 `json:"unmanagedVPCs,omitempty"`
	// unmanagedSubnets is the number of subnets inside those VPCs.
	// +optional
	UnmanagedSubnets int32 `json:"unmanagedSubnets,omitempty"`
	// unmanagedIDs are the VPC and subnet IDs that were outside the selector at the last
	// successful sync. The operator keeps them here rather than in memory so that a restart or
	// a change of leader can tell a resource that appeared since then from one that was
	// already known — otherwise every known unmanaged resource would be reported as new again.
	// +optional
	// +listType=set
	UnmanagedIDs []string `json:"unmanagedIDs,omitempty"`
	// lastSyncTime is the time of the last successful sync.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`
	// error is the last sync error, empty when the last sync succeeded.
	// +optional
	Error string `json:"error,omitempty"`
}

// NetworkScopeStatus defines the observed state of NetworkScope.
type NetworkScopeStatus struct {
	// observedGeneration is the generation of the spec that was last synced.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// lastSyncTime is when the last full sync finished.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// vpcs is the number of VPCs discovered across all targets.
	// +optional
	VPCs int32 `json:"vpcs,omitempty"`

	// subnets is the number of subnets discovered across all targets.
	// +optional
	Subnets int32 `json:"subnets,omitempty"`

	// unmanaged is the number of discovered resources without the managed tag, across all
	// targets. Anything above zero is something nobody has taken responsibility for.
	// +optional
	Unmanaged int32 `json:"unmanaged,omitempty"`

	// targets reports each account/region pair.
	// +listType=atomic
	// +optional
	Targets []TargetStatus `json:"targets,omitempty"`

	// conditions: Ready is True when every target synced successfully.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=nscope
// +kubebuilder:printcolumn:name="VPCs",type=integer,JSONPath=`.status.vpcs`
// +kubebuilder:printcolumn:name="Subnets",type=integer,JSONPath=`.status.subnets`
// +kubebuilder:printcolumn:name="Unmanaged",type=integer,JSONPath=`.status.unmanaged`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Last sync",type=date,JSONPath=`.status.lastSyncTime`

// NetworkScope selects AWS accounts and regions whose VPCs and subnets are discovered.
type NetworkScope struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of NetworkScope
	// +required
	Spec NetworkScopeSpec `json:"spec"`

	// status defines the observed state of NetworkScope
	// +optional
	Status NetworkScopeStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// NetworkScopeList contains a list of NetworkScope
type NetworkScopeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []NetworkScope `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &NetworkScope{}, &NetworkScopeList{})
		return nil
	})
}

// AccountHasReadRole reports whether the account is reached through sts:AssumeRole, which
// means it is not the operator's own account and needs a write role of its own for claims.
func (s *NetworkScope) AccountHasReadRole(account string) bool {
	for _, a := range s.Spec.Accounts {
		if a.ID == account {
			return a.RoleARN != ""
		}
	}
	return false
}

// Account returns the account entry, if the scope lists it.
func (s *NetworkScope) Account(account string) (AccountSpec, bool) {
	for _, a := range s.Spec.Accounts {
		if a.ID == account {
			return a, true
		}
	}
	return AccountSpec{}, false
}

// RegionsFor returns the regions discovered in an account: its own list when it has one, the
// scope's otherwise. It returns nil when the scope does not list the account at all.
func (s *NetworkScope) RegionsFor(account string) []string {
	a, ok := s.Account(account)
	if !ok {
		return nil
	}
	if len(a.Regions) > 0 {
		return a.Regions
	}
	return s.Spec.Regions
}

// Covers reports whether the scope discovers this account/region pair.
func (s *NetworkScope) Covers(account, region string) bool {
	return slices.Contains(s.RegionsFor(account), region)
}
