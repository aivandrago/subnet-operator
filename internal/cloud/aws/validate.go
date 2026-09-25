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

package aws

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation/field"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
)

// What an AWS scope, claim and import must look like beyond the schema. The webhooks refuse
// what fails here at apply time; the controllers refuse the same claims and imports in their
// status, for objects created while the webhooks were not running.

// regionPattern is the shape of a region name: eu-central-1, us-gov-east-1, cn-north-1.
// The list of real regions changes faster than a release of this operator, so the shape is
// all that is checked: a typo like "eu-central1" is caught, a brand new region is not.
var regionPattern = regexp.MustCompile(`^[a-z]{2}(-[a-z]+)+-[0-9]+$`)

// accountPattern is the shape of an AWS account ID.
var accountPattern = regexp.MustCompile(`^[0-9]{12}$`)

// arnAccountPattern pulls the account out of an IAM role ARN. The CRD already checks the
// overall shape, so the match is only used to compare the account.
var arnAccountPattern = regexp.MustCompile(`^arn:aws[a-z-]*:iam::([0-9]{12}):role/`)

// networkIDPattern is the shape of a VPC ID, which is what networkID is on AWS.
var networkIDPattern = regexp.MustCompile(`^vpc-[0-9a-f]+$`)

// resourceIDPattern is the shape of what an import can tag on AWS: a VPC or a subnet.
var resourceIDPattern = regexp.MustCompile(`^(vpc|subnet)-[0-9a-f]+$`)

// Tag limits. CreateTags refuses anything past them, and "aws:" is reserved for AWS.
const (
	maxTagKeyLength   = 128
	maxTagValueLength = 256
	reservedTagPrefix = "aws:"
)

// Subnet sizes and the number of zones a claim may span.
const (
	minPrefixLength = 16
	maxPrefixLength = 28
	maxZones        = 6
)

// validateRegion checks the shape of a region name.
func validateRegion(path *field.Path, region string) *field.Error {
	if regionPattern.MatchString(region) {
		return nil
	}
	return field.Invalid(path, region, "not an AWS region name, e.g. eu-central-1")
}

// ValidateScope implements provider.Provider: region names, roles that live in the account
// they reach, and at most one account without a role (the operator has one identity of its
// own; every further account is reached through sts:AssumeRole).
func (p *Provider) ValidateScope(scope *networkv1.NetworkScope) ([]string, field.ErrorList) {
	spec := field.NewPath("spec")
	var errs field.ErrorList
	var warnings []string
	for i, region := range scope.Spec.Regions {
		if e := validateRegion(spec.Child("regions").Index(i), region); e != nil {
			errs = append(errs, e)
		}
	}
	var ownAccounts []string
	for i, account := range scope.Spec.Accounts {
		path := spec.Child("accounts").Index(i)
		errs = append(errs, validateAccount(path, account)...)
		aws := account.AWSAccount()
		if aws.RoleARN == "" {
			ownAccounts = append(ownAccounts, account.ID)
		}
		if aws.ExternalID != "" && aws.RoleARN == "" {
			warnings = append(warnings, fmt.Sprintf(
				"account %s sets aws.externalID but no aws.roleARN, so the external ID is never used", account.ID))
		}
	}
	if len(ownAccounts) > 1 {
		errs = append(errs, field.Invalid(spec.Child("accounts"), ownAccounts,
			"only one account may omit aws.roleARN (the account the operator itself runs in); "+
				"the others need a role to assume"))
	}
	return warnings, errs
}

// validateAccount checks one account entry: its regions, and that its roles live in the
// account they are meant to reach. sts:AssumeRole can only assume a role of that account, so
// a role ARN from a different one is always a copy-paste mistake.
func validateAccount(path *field.Path, account networkv1.Account) field.ErrorList {
	var errs field.ErrorList
	for i, region := range account.Regions {
		if e := validateRegion(path.Child("regions").Index(i), region); e != nil {
			errs = append(errs, e)
		}
	}
	aws := account.AWSAccount()
	roles := []struct{ name, arn string }{
		{"roleARN", aws.RoleARN},
		{"writeRoleARN", aws.WriteRoleARN},
	}
	for _, role := range roles {
		if role.arn == "" {
			continue
		}
		match := arnAccountPattern.FindStringSubmatch(role.arn)
		if match == nil {
			continue // the CRD pattern already refuses anything that is not a role ARN
		}
		if match[1] != account.ID {
			errs = append(errs, field.Invalid(path.Child("aws").Child(role.name), role.arn,
				fmt.Sprintf("the role lives in account %s, but this entry is account %s: sts:AssumeRole can only "+
					"assume a role of the account it reaches", match[1], account.ID)))
		}
	}
	return errs
}

// ValidateClaim implements provider.Provider: the account and ID formats, the region, one to
// six availability zones of that region, and a prefix length AWS accepts.
func (p *Provider) ValidateClaim(claim *networkv1.SubnetClaim) field.ErrorList {
	spec := field.NewPath("spec")
	var errs field.ErrorList
	if !accountPattern.MatchString(claim.Spec.Account) {
		errs = append(errs, field.Invalid(spec.Child("account"), claim.Spec.Account, "an AWS account ID is 12 digits"))
	}
	if e := validateRegion(spec.Child("region"), claim.Spec.Region); e != nil {
		errs = append(errs, e)
	}
	if !networkIDPattern.MatchString(claim.Spec.NetworkID) {
		errs = append(errs, field.Invalid(spec.Child("networkID"), claim.Spec.NetworkID, "not an AWS VPC ID, e.g. vpc-0abc"))
	}
	if claim.Spec.PrefixLength < minPrefixLength || claim.Spec.PrefixLength > maxPrefixLength {
		errs = append(errs, field.Invalid(spec.Child("prefixLength"), claim.Spec.PrefixLength,
			fmt.Sprintf("AWS subnets are between a /%d and a /%d", minPrefixLength, maxPrefixLength)))
	}
	switch {
	case len(claim.Spec.Zones) == 0:
		errs = append(errs, field.Required(spec.Child("zones"),
			"AWS subnets are zonal: list one to six availability zones, e.g. eu-central-1a"))
	case len(claim.Spec.Zones) > maxZones:
		errs = append(errs, field.TooMany(spec.Child("zones"), len(claim.Spec.Zones), maxZones))
	}
	for i, zone := range claim.Spec.Zones {
		if !strings.HasPrefix(zone, claim.Spec.Region) {
			errs = append(errs, field.Invalid(spec.Child("zones").Index(i), zone,
				fmt.Sprintf("not an availability zone of region %s", claim.Spec.Region)))
		}
	}
	return errs
}

// ClaimRefusal implements provider.Provider.
func (p *Provider) ClaimRefusal(claim *networkv1.SubnetClaim) (string, string) {
	if len(claim.Spec.Zones) == 0 {
		return "ZonesRequired", "AWS subnets are zonal: list one to six availability zones in spec.zones"
	}
	if claim.Spec.PrefixLength < minPrefixLength || claim.Spec.PrefixLength > maxPrefixLength {
		return "InvalidPrefixLength", fmt.Sprintf("AWS subnets are between a /%d and a /%d, not a /%d",
			minPrefixLength, maxPrefixLength, claim.Spec.PrefixLength)
	}
	return "", ""
}

// ValidateImport implements provider.Provider: the account and resource ID formats, and a
// region, which every AWS network and subnet has.
func (p *Provider) ValidateImport(imp *networkv1.ResourceImport) field.ErrorList {
	spec := field.NewPath("spec")
	var errs field.ErrorList
	if !accountPattern.MatchString(imp.Spec.Account) {
		errs = append(errs, field.Invalid(spec.Child("account"), imp.Spec.Account, "an AWS account ID is 12 digits"))
	}
	if imp.Spec.Region == "" {
		errs = append(errs, field.Required(spec.Child("region"), "every AWS network and subnet is regional"))
	} else if e := validateRegion(spec.Child("region"), imp.Spec.Region); e != nil {
		errs = append(errs, e)
	}
	if !resourceIDPattern.MatchString(imp.Spec.ResourceID) {
		errs = append(errs, field.Invalid(spec.Child("resourceID"), imp.Spec.ResourceID,
			"not an AWS VPC or subnet ID, e.g. vpc-0abc or subnet-0abc"))
	}
	return errs
}

// ImportRefusal implements provider.Provider.
func (p *Provider) ImportRefusal(imp *networkv1.ResourceImport) (string, string) {
	if imp.Spec.Region == "" {
		return "RegionRequired", "every AWS network and subnet is regional: set spec.region"
	}
	if !resourceIDPattern.MatchString(imp.Spec.ResourceID) {
		return "InvalidResourceID", fmt.Sprintf("%q is not an AWS VPC or subnet ID (vpc-… or subnet-…)", imp.Spec.ResourceID)
	}
	return "", ""
}

// ValidateTags implements provider.Provider with the EC2 tag rules, so a mistake shows up at
// apply time instead of as a failed CreateTags call minutes later. "/" is a valid key
// character on AWS, which is why the default keys are hs/owner, hs/env and hs/tier.
func (p *Provider) ValidateTags(path *field.Path, tags map[string]string) field.ErrorList {
	var errs field.ErrorList
	for _, key := range slices.Sorted(maps.Keys(tags)) {
		keyPath := path.Key(key)
		switch {
		case key == "":
			errs = append(errs, field.Invalid(keyPath, key, "tag key must not be empty"))
		case strings.HasPrefix(strings.ToLower(key), reservedTagPrefix):
			errs = append(errs, field.Invalid(keyPath, key, `tag keys starting with "aws:" are reserved by AWS`))
		case len(key) > maxTagKeyLength:
			errs = append(errs, field.Invalid(keyPath, key,
				fmt.Sprintf("tag key is longer than the %d characters AWS allows", maxTagKeyLength)))
		}
		if len(tags[key]) > maxTagValueLength {
			errs = append(errs, field.Invalid(keyPath, tags[key],
				fmt.Sprintf("tag value is longer than the %d characters AWS allows", maxTagValueLength)))
		}
	}
	return errs
}
