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

// Package policy decides what the operator should do with a resource nobody has tagged.
//
// The decision is a pure function of the resource, who created it and the policy, so it can
// be read, tested and argued about without an AWS account. Deliberately conservative: the
// operator would rather leave something unmanaged and ask a human than guess an owner.
package policy

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	networkv1beta1 "hypersurgery.dev/subnet-operator/api/v1beta1"
)

// Verdict is what the policy decided.
type Verdict string

const (
	// VerdictNone means the policy is off, so nothing happens.
	VerdictNone Verdict = "none"
	// VerdictImport means the tags were resolved and the resource can be imported.
	VerdictImport Verdict = "import"
	// VerdictSkip means a skip rule matched: somebody else owns this resource.
	VerdictSkip Verdict = "skip"
	// VerdictNoOwner means no rule produced the required tags. The resource stays unmanaged
	// and the alert asks a human to pick an owner.
	VerdictNoOwner Verdict = "no_owner"
)

// Resource is the unmanaged network or subnet under consideration.
type Resource struct {
	// ID is the provider ID, e.g. vpc-… or subnet-… on AWS.
	ID string
	// Account and Region locate it.
	Account, Region string
	// Tags are the tags it already carries.
	Tags map[string]string
	// IsSubnet says whether inheritFromNetwork applies.
	IsSubnet bool
	// ParentNetworkTags are the tags of the network a subnet belongs to; empty for a network.
	ParentNetworkTags map[string]string
}

// Decision is the outcome, with the reason spelled out for the object's status and the log.
type Decision struct {
	Verdict Verdict
	// Tags to apply, set when the verdict is import.
	Tags map[string]string
	// Reason is one human sentence, e.g. "owner from creator arn:…/payments-deploy/".
	Reason string
	// Missing lists the required tags that stayed unresolved, for a no_owner verdict.
	Missing []string
}

// Decide works out what should happen to one unmanaged resource. The creator is the
// CloudTrail principal that created it, empty when it is not known (the event is gone, or the
// resource predates the operator).
func Decide(r Resource, creator string, p *networkv1beta1.AutoImportPolicy) Decision {
	if p == nil || p.Mode == "" || p.Mode == networkv1beta1.AutoImportOff {
		return Decision{Verdict: VerdictNone, Reason: "auto-import is off"}
	}
	if rule, ok := matchSkip(r, creator, p.Skip); ok {
		return Decision{Verdict: VerdictSkip, Reason: skipReason(rule, creator)}
	}

	tags := map[string]string{}
	var reason string

	// The creator is the strongest signal: whoever made the thing is the best guess at who
	// owns it, and the rules are written by the platform team, not inferred.
	if creator != "" {
		for _, rule := range p.FromCreator {
			if strings.HasPrefix(creator, rule.PrincipalPrefix) {
				maps.Copy(tags, rule.Tags)
				reason = fmt.Sprintf("tags from creator rule %q", rule.PrincipalPrefix)
				break
			}
		}
	}

	// Then the network: a subnet added by hand usually belongs to whoever owns the network.
	if len(tags) == 0 && r.IsSubnet && len(p.InheritFromNetwork) > 0 {
		for _, key := range p.InheritFromNetwork {
			if v, ok := r.ParentNetworkTags[key]; ok && v != "" {
				tags[key] = v
			}
		}
		if len(tags) > 0 {
			reason = "tags inherited from the parent network"
		}
	}

	// Finally the account's own defaults.
	if len(tags) == 0 {
		for _, d := range p.AccountDefaults {
			if d.Account == r.Account {
				maps.Copy(tags, d.Tags)
				reason = fmt.Sprintf("tags from the defaults of account %s", d.Account)
				break
			}
		}
	}

	if kept := keepExisting(tags, r.Tags); len(kept) > 0 {
		note := "kept the resource's own " + strings.Join(kept, ", ")
		if reason == "" {
			reason = note
		} else {
			reason += "; " + note
		}
	}

	required := p.RequiredTags
	if len(required) == 0 {
		required = []string{networkv1beta1.DefaultOwnerTagKey}
	}
	var missing []string
	for _, key := range required {
		if v, ok := tags[key]; !ok || v == "" {
			// A tag the resource already carries counts: it was there before the policy ran.
			if v, ok := r.Tags[key]; ok && v != "" {
				continue
			}
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return Decision{
			Verdict: VerdictNoOwner,
			Missing: missing,
			Reason:  fmt.Sprintf("no rule resolved %s", strings.Join(missing, ", ")),
		}
	}

	// The managed tag is what actually puts the resource in the inventory.
	managedTag, managedValue := p.ManagedTag, p.ManagedValue
	if managedTag == "" {
		managedTag = networkv1beta1.DefaultManagedTag
	}
	if managedValue == "" {
		managedValue = networkv1beta1.DefaultManagedValue
	}
	tags[managedTag] = managedValue

	return Decision{Verdict: VerdictImport, Tags: tags, Reason: reason}
}

// keepExisting drops from tags every key the resource already carries with a value, and
// returns those keys sorted. A value somebody set on purpose wins over one a rule inferred:
// CreateTags replaces the value of every key it names, so importing with it would change a tag
// rather than add one. The policy only fills in what is missing.
func keepExisting(tags, existing map[string]string) []string {
	var kept []string
	for key := range tags {
		if existing[key] != "" {
			delete(tags, key)
			kept = append(kept, key)
		}
	}
	slices.Sort(kept)
	return kept
}

func matchSkip(r Resource, creator string, rules []networkv1beta1.SkipRule) (networkv1beta1.SkipRule, bool) {
	for _, rule := range rules {
		if rule.PrincipalPrefix != "" && creator != "" && strings.HasPrefix(creator, rule.PrincipalPrefix) {
			return rule, true
		}
		if rule.TagKey == "" {
			continue
		}
		v, ok := r.Tags[rule.TagKey]
		if ok && (rule.TagValue == "" || v == rule.TagValue) {
			return rule, true
		}
	}
	return networkv1beta1.SkipRule{}, false
}

func skipReason(rule networkv1beta1.SkipRule, creator string) string {
	switch {
	case rule.PrincipalPrefix != "" && creator != "" && strings.HasPrefix(creator, rule.PrincipalPrefix):
		return fmt.Sprintf("created by %q, which the policy skips", rule.PrincipalPrefix)
	case rule.TagValue != "":
		return fmt.Sprintf("carries %s=%s, which the policy skips", rule.TagKey, rule.TagValue)
	default:
		return fmt.Sprintf("carries the %s tag, which the policy skips", rule.TagKey)
	}
}
