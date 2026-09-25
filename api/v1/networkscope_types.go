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
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
)

// Provider names the cloud a NetworkScope discovers. It is an open enum: releases that
// implement another cloud add its value, so a client must treat a value it does not know as a
// provider it does not know and skip the object, not fail on it.
// +kubebuilder:validation:Enum=AWS
type Provider string

const (
	// ProviderAWS is Amazon Web Services: accounts, VPCs and subnets.
	ProviderAWS Provider = "AWS"
)

// Default tag keys on AWS, where "/" is a valid tag key character. They are the keys the
// aws.hypersurgery/v1alpha1 API used, so nothing changes for resources tagged before.
const (
	DefaultOwnerTagKey = "hs/owner"
	DefaultEnvTagKey   = "hs/env"
	DefaultTierTagKey  = "hs/tier"
)

// DefaultTagKeys returns the tag keys a scope of the provider uses when it does not name its
// own. They depend on the provider because the key rules do (ADR 0002 §5); the defaulting
// webhook writes them into the spec and the controllers fall back to them.
// Only AWS exists so far; GCP and Azure will use hs-owner, hs-env and hs-tier.
func DefaultTagKeys(_ Provider) TagKeys {
	return TagKeys{Owner: DefaultOwnerTagKey, Env: DefaultEnvTagKey, Tier: DefaultTierTagKey}
}

// Account is one account of the provider: an AWS account ID today, a GCP project or an Azure
// subscription in the releases that add them. Its identity settings are in the member named
// after the scope's provider.
type Account struct {
	// id is the account's ID at the provider. For AWS it is the 12-digit account ID; the
	// format is checked per provider on the scope.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	// +required
	ID string `json:"id"`

	// regions overrides spec.regions for this account. Each region is listed once.
	// +listType=set
	// +optional
	Regions []string `json:"regions,omitempty"`

	// aws holds how the operator reaches the account on AWS. Only valid in a scope with
	// provider AWS.
	// +optional
	AWS *AWSAccount `json:"aws,omitempty"`
}

// AWSAccount is how the operator reaches an AWS account.
type AWSAccount struct {
	// roleARN is assumed (sts:AssumeRole) to read the account. When empty, the operator's own
	// credentials (EKS Pod Identity / IRSA) are used, which only works for the account the
	// operator runs in.
	// +kubebuilder:validation:Pattern=`^arn:aws[a-z-]*:iam::[0-9]{12}:role/.+$`
	// +optional
	RoleARN string `json:"roleARN,omitempty"`

	// externalID is passed to sts:AssumeRole when the role trust policy requires it.
	// +optional
	ExternalID string `json:"externalID,omitempty"`

	// writeRoleARN is assumed for SubnetClaims in Create mode and for ResourceImports. It is
	// separate from roleARN so that discovery never carries write permissions. When empty,
	// claims for an account reached through roleARN can only be allocated, not created.
	// +kubebuilder:validation:Pattern=`^arn:aws[a-z-]*:iam::[0-9]{12}:role/.+$`
	// +optional
	WriteRoleARN string `json:"writeRoleARN,omitempty"`
}

// AWSScope holds scope-wide settings for AWS. It has none yet; it exists so that a scope can
// say `aws: {}` and so that AWS-only settings have a place that is not a neutral field.
type AWSScope struct{}

// TagKeys names the tags that carry inventory metadata. Unset keys default per provider: on
// AWS hs/owner, hs/env and hs/tier.
type TagKeys struct {
	// owner is the tag key holding the owning team.
	// +optional
	Owner string `json:"owner,omitempty"`

	// env is the tag key holding the environment.
	// +optional
	Env string `json:"env,omitempty"`

	// tier is the tag key holding the subnet tier (public, private, db, ...).
	// +optional
	Tier string `json:"tier,omitempty"`
}

// NetworkSelector limits discovery to some of the provider's networks.
type NetworkSelector struct {
	// matchTags selects the networks that carry all of these tags. An empty value matches any
	// value of the key. All subnets of a selected network are discovered, including untagged
	// ones, so they show up in compliance reports.
	// +optional
	MatchTags map[string]string `json:"matchTags,omitempty"`
}

// NetworkScopeSpec defines which accounts and regions of one provider are discovered.
// +kubebuilder:validation:XValidation:rule="!has(self.aws) || self.provider == 'AWS'",message="aws is only valid for provider AWS"
// +kubebuilder:validation:XValidation:rule="self.accounts.all(a, !has(a.aws) || self.provider == 'AWS')",message="accounts[].aws is only valid for provider AWS"
// +kubebuilder:validation:XValidation:rule="self.provider != 'AWS' || self.accounts.all(a, a.id.matches('^[0-9]{12}$'))",message="an AWS account id is 12 digits"
// +kubebuilder:validation:XValidation:rule="self.provider != 'AWS' || !has(self.autoImport) || !has(self.autoImport.accountDefaults) || self.autoImport.accountDefaults.all(d, d.account.matches('^[0-9]{12}$'))",message="an AWS account in autoImport.accountDefaults is 12 digits"
type NetworkScopeSpec struct {
	// provider is the cloud the scope discovers. It cannot change: a scope does not move
	// clouds, a new scope does. More values are added by the releases that implement them.
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="provider is immutable"
	// +required
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Provider",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:select:AWS"}
	Provider Provider `json:"provider"`

	// accounts to discover.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=1000
	// +listType=map
	// +listMapKey=id
	// +required
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Accounts"
	Accounts []Account `json:"accounts"`

	// regions to discover in every account (unless the account overrides them): AWS regions
	// such as eu-central-1. Each region is listed once.
	// +kubebuilder:validation:MinItems=1
	// +listType=set
	// +required
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Regions"
	Regions []string `json:"regions"`

	// networkSelector limits discovery to the networks (AWS VPCs) it selects. When unset or
	// empty, every network is discovered.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Network Selector"
	NetworkSelector *NetworkSelector `json:"networkSelector,omitempty"`

	// requiredSubnetTags are tag keys every subnet must have. Missing keys are reported in the
	// Subnet status and as metrics.
	// +listType=set
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Required Subnet Tags"
	RequiredSubnetTags []string `json:"requiredSubnetTags,omitempty"`

	// tagKeys maps inventory fields (owner, env, tier) to tag keys. Unset keys default per
	// provider; on AWS to hs/owner, hs/env and hs/tier.
	// +optional
	TagKeys TagKeys `json:"tagKeys,omitzero"`

	// resyncInterval is how often the full inventory is refreshed.
	// +kubebuilder:default="10m"
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Resync Interval",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:text","urn:alm:descriptor:com.tectonic.ui:advanced"}
	ResyncInterval *metav1.Duration `json:"resyncInterval,omitempty"`

	// discoverUnmanaged also lists the networks the selector does not match, and their
	// subnets, so resources nobody has claimed are counted and can be imported instead of
	// staying invisible. They are reported and alerted on, never mirrored as objects.
	// +kubebuilder:default=true
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Discover Unmanaged",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:booleanSwitch","urn:alm:descriptor:com.tectonic.ui:advanced"}
	DiscoverUnmanaged *bool `json:"discoverUnmanaged,omitempty"`

	// autoImport tags unmanaged resources on its own, deriving the tags from who created the
	// resource, from its network, or from the account. Off unless configured.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Auto Import",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:advanced"}
	AutoImport *AutoImportPolicy `json:"autoImport,omitempty"`

	// namespaceSelector selects the namespaces whose SubnetClaims and ResourceImports may refer
	// to this scope. Referring to a scope is what makes the operator use its identities,
	// including the write identities, on the object's behalf, so this is what keeps one team's
	// namespace from creating subnets in, or tagging, another team's accounts.
	//
	// Select namespaces by name with the kubernetes.io/metadata.name label, which the API
	// server sets on every namespace and nobody can change, or by a label only cluster
	// administrators can set. An empty selector ({}) allows every namespace on purpose.
	//
	// When unset, no namespace may use the scope. (In aws.hypersurgery/v1alpha1 unset allowed
	// every namespace; migrated scopes get an explicit {} instead.)
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=spec,displayName="Namespace Selector"
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// aws holds scope-wide AWS settings. Only valid with provider AWS.
	// +optional
	AWS *AWSScope `json:"aws,omitempty"`
}

// TargetStatus reports the last sync of one account/region pair.
type TargetStatus struct {
	// account is the account ID.
	Account string `json:"account"`
	// region is the region.
	Region string `json:"region"`
	// networks is the number of networks discovered.
	Networks int32 `json:"networks"`
	// subnets is the number of subnets discovered.
	Subnets int32 `json:"subnets"`
	// unmanagedNetworks is the number of networks the selector does not match.
	// +optional
	UnmanagedNetworks int32 `json:"unmanagedNetworks,omitempty"`
	// unmanagedSubnets is the number of subnets inside those networks.
	// +optional
	UnmanagedSubnets int32 `json:"unmanagedSubnets,omitempty"`
	// unmanagedIDs are the network and subnet IDs that were outside the selector at the last
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
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Last Sync",xDescriptors={"urn:alm:descriptor:timestamp"}
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// networks is the number of networks discovered across all targets.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Networks",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:number"}
	Networks int32 `json:"networks,omitempty"`

	// subnets is the number of subnets discovered across all targets.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Subnets",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:number"}
	Subnets int32 `json:"subnets,omitempty"`

	// unmanaged is the number of discovered resources without the managed tag, across all
	// targets. Anything above zero is something nobody has taken responsibility for.
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Unmanaged",xDescriptors={"urn:alm:descriptor:com.tectonic.ui:number"}
	Unmanaged int32 `json:"unmanaged,omitempty"`

	// capabilities lists what the scope's provider can do as the operator runs it:
	// CreateSubnet, ChangeEvents, IPUsage. More values may be added; ignore unknown ones.
	// +listType=set
	// +optional
	Capabilities []Capability `json:"capabilities,omitempty"`

	// ownership says where the provider keeps ownership metadata of networks and subnets.
	// +optional
	Ownership *Ownership `json:"ownership,omitempty"`

	// targets reports each account/region pair.
	// +listType=atomic
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Targets"
	Targets []TargetStatus `json:"targets,omitempty"`

	// conditions: Ready is True when every target synced successfully.
	// +listType=map
	// +listMapKey=type
	// +optional
	// +operator-sdk:csv:customresourcedefinitions:type=status,displayName="Conditions",xDescriptors={"urn:alm:descriptor:io.kubernetes.conditions"}
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:storageversion
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=nscope,categories=hypersurgery
// +kubebuilder:printcolumn:name="Provider",type=string,JSONPath=`.spec.provider`
// +kubebuilder:printcolumn:name="Networks",type=integer,JSONPath=`.status.networks`
// +kubebuilder:printcolumn:name="Subnets",type=integer,JSONPath=`.status.subnets`
// +kubebuilder:printcolumn:name="Unmanaged",type=integer,JSONPath=`.status.unmanaged`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Last sync",type=date,JSONPath=`.status.lastSyncTime`
// +operator-sdk:csv:customresourcedefinitions:displayName="Network Scope",resources={{Network,v1,""},{Subnet,v1,""},{ResourceImport,v1,""},{Event,v1,""}}

// NetworkScope selects the accounts and regions of one provider whose networks and subnets
// are discovered.
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

// Account returns the account entry, if the scope lists it.
func (s *NetworkScope) Account(account string) (Account, bool) {
	for _, a := range s.Spec.Accounts {
		if a.ID == account {
			return a, true
		}
	}
	return Account{}, false
}

// AWSAccount returns the AWS settings of an account, empty when it has none.
func (a Account) AWSAccount() AWSAccount {
	if a.AWS == nil {
		return AWSAccount{}
	}
	return *a.AWS
}

// AccountHasReadRole reports whether the account is reached through an identity of its own
// (on AWS, sts:AssumeRole), which means it is not the operator's own account and needs a
// write identity of its own for claims and imports.
func (s *NetworkScope) AccountHasReadRole(account string) bool {
	a, ok := s.Account(account)
	return ok && a.AWSAccount().RoleARN != ""
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

// MatchTags returns the tags a network must carry to be discovered, nil for every network.
func (s *NetworkScope) MatchTags() map[string]string {
	if s.Spec.NetworkSelector == nil {
		return nil
	}
	return s.Spec.NetworkSelector.MatchTags
}

// ResolvedTagKeys returns the scope's tag keys with the provider's defaults filled in.
func (s *NetworkScope) ResolvedTagKeys() TagKeys {
	k := s.Spec.TagKeys
	d := DefaultTagKeys(s.Spec.Provider)
	if k.Owner == "" {
		k.Owner = d.Owner
	}
	if k.Env == "" {
		k.Env = d.Env
	}
	if k.Tier == "" {
		k.Tier = d.Tier
	}
	return k
}

// AllowsNamespace reports whether objects in a namespace carrying these labels may refer to
// the scope. An unset selector allows no namespace; an empty one ({}) allows every namespace; a
// selector that does not parse allows none, because a typo in the one field that limits who
// may use the write identities must not open them to everybody.
func (s *NetworkScope) AllowsNamespace(namespaceLabels map[string]string) (bool, error) {
	if s.Spec.NamespaceSelector == nil {
		return false, nil
	}
	selector, err := metav1.LabelSelectorAsSelector(s.Spec.NamespaceSelector)
	if err != nil {
		return false, err
	}
	return selector.Matches(labels.Set(namespaceLabels)), nil
}
