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

// AutoImportMode says how far the policy goes on its own.
type AutoImportMode string

const (
	// AutoImportOff leaves unmanaged resources alone. They are still counted and alerted on.
	AutoImportOff AutoImportMode = "Off"
	// AutoImportDryRun creates the ResourceImport objects with dryRun set, so a day of them
	// can be reviewed before anything in AWS is touched.
	AutoImportDryRun AutoImportMode = "DryRun"
	// AutoImportApply creates real imports: the resources get tagged.
	AutoImportApply AutoImportMode = "Apply"
)

// DefaultManagedTag marks a resource as being in scope for the operator.
const (
	DefaultManagedTag   = "hs/managed"
	DefaultManagedValue = "true"
)

// CreatorRule maps the principal that created a resource to the tags it should carry.
type CreatorRule struct {
	// principalPrefix is matched against the CloudTrail principal, e.g.
	// "arn:aws:sts::222222222222:assumed-role/payments-deploy/". A prefix, not a pattern:
	// the operator never guesses an owner from a partial match in the middle of an ARN.
	PrincipalPrefix string `json:"principalPrefix"`

	// tags are applied when the prefix matches.
	Tags map[string]string `json:"tags"`
}

// AccountDefault gives an account a set of fallback tags.
type AccountDefault struct {
	// account is the 12-digit AWS account ID.
	Account string `json:"account"`

	// tags are applied to resources in that account when nothing more specific matched.
	Tags map[string]string `json:"tags"`
}

// SkipRule keeps the policy away from resources somebody else owns. Terraform is the usual
// reason: tagging a resource it manages makes the next plan want to remove the tags again.
type SkipRule struct {
	// tagKey, when present on the resource, skips it. With tagValue set, only that value skips.
	TagKey string `json:"tagKey,omitempty"`

	// tagValue narrows tagKey to one value.
	TagValue string `json:"tagValue,omitempty"`

	// principalPrefix skips resources created by a matching principal, e.g. a CI role.
	PrincipalPrefix string `json:"principalPrefix,omitempty"`
}

// AutoImportPolicy decides which unmanaged resources get tagged, and with what.
type AutoImportPolicy struct {
	// mode is Off (report only), DryRun (record what would be applied) or Apply.
	Mode AutoImportMode `json:"mode,omitempty"`

	// namespace holds the generated ResourceImport objects.
	Namespace string `json:"namespace,omitempty"`

	// fromCreator derives tags from the principal that created the resource, which CloudTrail
	// reports. Rules are tried in order and the first match wins.
	FromCreator []CreatorRule `json:"fromCreator,omitempty"`

	// inheritFromVPC copies these tag keys from the parent VPC onto an unmanaged subnet.
	// Tried after fromCreator, for the common case of a subnet added by hand to a VPC that
	// already has an owner.
	InheritFromVPC []string `json:"inheritFromVPC,omitempty"`

	// accountDefaults are the last resort, per account.
	AccountDefaults []AccountDefault `json:"accountDefaults,omitempty"`

	// skip keeps the policy away from matching resources entirely.
	Skip []SkipRule `json:"skip,omitempty"`

	// requiredTags must all be resolved before anything is imported. Defaults to the owner
	// tag: a resource nobody owns stays unmanaged, and the alert asks a human to decide.
	RequiredTags []string `json:"requiredTags,omitempty"`

	// managedTag and managedValue are added to every generated import, because that is what
	// makes the resource part of the inventory.
	ManagedTag   string `json:"managedTag,omitempty"`
	ManagedValue string `json:"managedValue,omitempty"`
}
