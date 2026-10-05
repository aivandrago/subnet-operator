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

package v1beta1

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// Values the operator writes under the keys below, the same on every provider.
const (
	// TagManagedByValue is the value of the managed-by tag on subnets the operator created.
	TagManagedByValue = "subnet-operator"
	// DefaultManagedValue is the value of the managed marker when the auto-import policy does
	// not name one.
	DefaultManagedValue = "true"
)

// OperatorTagKeys are the tag keys the operator itself uses on one provider: the defaults of
// spec.tagKeys, the auto-import policy's managed marker, and what it tags a created subnet
// with. They depend on the provider because the key rules do (ADR 0002 §5 and §6): "/" is a
// valid key character on AWS, so AWS keeps hs/owner, hs/managed, hs/managed-by and hs/claim, the
// keys of every release before 2.0; Google Cloud tag keys cannot contain it, so GCP uses
// hs-owner, hs-managed, hs-managed-by and hs-claim.
// +kubebuilder:object:generate=false
type OperatorTagKeys struct {
	// Owner, Env and Tier are the defaults of NetworkScope.spec.tagKeys.
	Owner string
	Env   string
	Tier  string
	// Managed is the default of NetworkScope.spec.autoImport.managedTag, the marker that puts
	// a resource in the inventory.
	Managed string
	// ManagedBy marks a subnet the operator created; its value is TagManagedByValue.
	ManagedBy string
	// Claim names the SubnetClaim a subnet was created for; its value is ClaimValue.
	Claim string

	claimSeparator string
	// claimMaxLength is the longest claim value the provider accepts, 0 for no limit.
	claimMaxLength int
}

// OperatorTagKeysFor returns the tag keys the operator uses on the provider. A provider this
// release does not know gets the AWS keys, which every release before 2.0 used everywhere.
func OperatorTagKeysFor(p Provider) OperatorTagKeys {
	switch p {
	case ProviderGCP:
		// Resource Manager tag keys: short names of letters, digits, "-", "_" and ".".
		return OperatorTagKeys{Owner: "hs-owner", Env: "hs-env", Tier: "hs-tier", Managed: "hs-managed",
			ManagedBy: "hs-managed-by", Claim: "hs-claim", claimSeparator: "_",
			claimMaxLength: gcpTagValueMaxLength}
	default:
		return OperatorTagKeys{Owner: "hs/owner", Env: "hs/env", Tier: "hs/tier", Managed: "hs/managed",
			ManagedBy: "hs/managed-by", Claim: "hs/claim", claimSeparator: "/"}
	}
}

// TagKeys returns the owner, env and tier keys as the defaults of spec.tagKeys.
func (k OperatorTagKeys) TagKeys() TagKeys {
	return TagKeys{Owner: k.Owner, Env: k.Env, Tier: k.Tier}
}

// gcpTagValueMaxLength is the longest short name the operator writes as a GCP tag value.
const gcpTagValueMaxLength = 63

// claimHashLength is how many hex digits of the SHA-256 a shortened claim value carries.
const claimHashLength = 10

// ClaimValue is the value of the claim tag for a SubnetClaim: namespace/name on AWS. A GCP tag
// value cannot contain "/", so there it is namespace_name; neither a namespace nor a name can
// contain "_", so the value still names exactly one claim. A GCP tag value is at most 63
// characters, so a longer namespace_name keeps its first characters and ends in "-" and the
// first ten hex digits of the SHA-256 of the whole namespace_name, which keeps it unique and
// valid: team-a_a-very-long-claim-name-…-0123456789.
func (k OperatorTagKeys) ClaimValue(namespace, name string) string {
	sep := k.claimSeparator
	if sep == "" {
		sep = "/"
	}
	value := namespace + sep + name
	if k.claimMaxLength == 0 || len(value) <= k.claimMaxLength {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	suffix := hex.EncodeToString(sum[:])[:claimHashLength]
	// A tag value starts and ends with a letter or a digit; the cut may leave "-", "." or "_".
	prefix := strings.TrimRight(value[:k.claimMaxLength-claimHashLength-1], "-._")
	return prefix + "-" + suffix
}

// objectNameHashLength is how many hex digits of the ID's SHA-256 an object name carries.
const objectNameHashLength = 10

// ObjectName is the name of the Network or Subnet object for a resource's canonical ID
// (spec.id), also the value of the network label on its subnets (ADR 0002 §5). An ID that is
// already a valid name and label value, such as an AWS vpc-… or subnet-… ID, is the name
// itself. Any other ID, such as the GCP resource name projects/p/regions/r/subnetworks/n, is
// not unique in its last segment across projects and regions and may be too long, so the name
// is that last segment, made valid and cut short, followed by the first ten hex digits of the
// SHA-256 of the whole ID: n-0123456789.
func ObjectName(id string) string {
	if len(validation.IsDNS1123Label(id)) == 0 {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	suffix := hex.EncodeToString(sum[:])[:objectNameHashLength]
	readable := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r - 'A' + 'a'
		default:
			return '-'
		}
	}, id[strings.LastIndex(id, "/")+1:])
	readable = strings.Trim(readable, "-")
	if limit := validation.DNS1123LabelMaxLength - objectNameHashLength - 1; len(readable) > limit {
		readable = strings.TrimRight(readable[:limit], "-")
	}
	if readable == "" {
		return suffix
	}
	return readable + "-" + suffix
}
