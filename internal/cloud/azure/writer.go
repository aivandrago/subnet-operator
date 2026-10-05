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

package azure

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/resources/armresources/v3"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// Writes (#53). A subnet is created with the Subnets API (PUT, a long-running operation that
// is polled to its end) and its ownership is the entry hs-subnet-<name> on its virtual network
// (tags.go), written with the Tags API's Merge operation on the virtual network's scope. Merge
// sends only the names it writes, so the rest of the virtual network (its other tags, its
// subnets, its address space) is never read-modified-written, and the write identity needs no
// write access to the virtual network for an import.
//
// Only adding. Merge replaces the value of a name the resource carries, so every write reads
// the virtual network's tags first and refuses the whole write with
// inventory.ErrOwnershipConflict when a name it would write already has another value, as on
// GCP: the operator never overwrites an ownership value. For an existing subnet the values it
// already has are its effective tags, the network's with its entry laid over them, which is what
// discovery reports and the import webhook compares against: an import cannot take over an
// owner a subnet inherits either. A subnet the operator creates has no ownership yet, so only an
// entry left under its name counts there. Pairs the network already carries with the same value
// are left out of the entry: an entry holds what differs from its network (ADR 0002 §6).
//
// The read and the Merge are two calls, and the Tags API has no precondition on a single tag,
// so a value written by somebody else between them is replaced; for a subnet entry that is a
// write to the same subnet's entry at the same moment, which the operator itself never does
// twice at once (one claim or import per resource). The window is one ARM round trip.
//
// Azure allows 50 tags per resource and 256 characters per value. A write that would take the
// virtual network past 50 is inventory.ErrTagBudgetExceeded, and an entry longer than 256
// characters inventory.ErrOwnershipEntryTooLong; both before anything is written.

// maxTagsPerResource is how many tags Azure allows on one resource.
const maxTagsPerResource = 50

// defaultPollInterval is the wait between polls of a long-running operation when ARM does not
// say how long to wait (it usually does, with Retry-After).
const defaultPollInterval = 5 * time.Second

var (
	// resourceIDPattern is the ID of a virtual network or one of its subnets, in any case:
	// /subscriptions/<id>/resourceGroups/<group>/providers/Microsoft.Network/virtualNetworks/<name>[/subnets/<name>].
	resourceIDPattern = regexp.MustCompile(`(?i)^/subscriptions/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})` +
		`/resourcegroups/([^/]+)/providers/microsoft\.network/virtualnetworks/([^/]+)(?:/subnets/([^/]+))?$`)
	// subnetNamePattern is what ARM accepts as a subnet's name: 1 to 80 letters, digits, "_",
	// "." and "-", starting with a letter or digit and ending with a letter, digit or "_".
	subnetNamePattern = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9_.-]{0,78}[a-zA-Z0-9_])?$`)
)

// reservedSubnetNames are the names Azure gives a meaning of their own: a subnet of that name
// is where the service expects to be deployed. A claim does not create one.
var reservedSubnetNames = []string{"GatewaySubnet", "AzureFirewallSubnet", "AzureFirewallManagementSubnet",
	"AzureBastionSubnet", "RouteServerSubnet"}

// resourceRef is a virtual network, or a subnet when subnet is set, as its ID names it.
type resourceRef struct {
	subscription, group, vnet, subnet string
}

func parseResourceID(id string) (resourceRef, bool) {
	m := resourceIDPattern.FindStringSubmatch(id)
	if m == nil {
		return resourceRef{}, false
	}
	return resourceRef{subscription: m[1], group: m[2], vnet: m[3], subnet: m[4]}, true
}

// vnetScope is the virtual network's ID as the Tags API takes it for a scope, spelt the way
// ARM does; ARM compares IDs without regard to case, so the names keep the ID's case.
func (r resourceRef) vnetScope() string {
	return "/subscriptions/" + r.subscription + "/resourceGroups/" + r.group +
		"/providers/Microsoft.Network/virtualNetworks/" + r.vnet
}

// writeClients are the ARM clients a write identity uses in one subscription.
type writeClients struct {
	id      Identity
	subnets *armnetwork.SubnetsClient
	tags    *armresources.TagsClient
}

// writeClientsFor returns the write clients of the target's identity, which for a write is the
// account's write identity (Provider.Identity(account, Write)); an identity the provider cannot
// use is refused by credentialFor, never replaced by the operator's own.
func (d *Discoverer) writeClientsFor(target inventory.Target) (*writeClients, error) {
	id, err := identityOf(target)
	if err != nil {
		return nil, err
	}
	cred, err := d.credentialFor(id)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	key := clientKey{identity: id, subscription: target.Account}
	if c, ok := d.writers[key]; ok {
		return c, nil
	}
	subnets, err := armnetwork.NewSubnetsClient(target.Account, cred, d.clientOptions())
	if err != nil {
		return nil, fmt.Errorf("subnets client for subscription %s: %w", target.Account, err)
	}
	tags, err := armresources.NewTagsClient(target.Account, cred, d.clientOptions())
	if err != nil {
		return nil, fmt.Errorf("tags client for subscription %s: %w", target.Account, err)
	}
	c := &writeClients{id: id, subnets: subnets, tags: tags}
	d.writers[key] = c
	return c, nil
}

// armWriteBucket is the ARM write quota a call counts against, apart from the reads.
func armWriteBucket(id Identity, subscription string) string {
	return "arm-write/" + id.String() + "/" + subscription
}

// CreateSubnet creates a subnet in the virtual network with the request's CIDR, after its
// ownership entry. The entry goes first so that the subnet never exists without its owner; an
// entry left by a create that then failed (a CIDR taken in the meantime) names no subnet and
// is used again by the next attempt, which has the same name. A subnet of that name that
// exists already, with the same prefix and an entry naming the same claim, is the one an
// earlier attempt created (its status update was lost) and is returned as created; any other is
// an error, and is never changed: the PUT says If-None-Match: *, since a PUT on an existing
// subnet would replace its settings.
func (d *Discoverer) CreateSubnet(ctx context.Context, target inventory.Target, req inventory.CreateSubnetRequest) (string, error) {
	network, ok := parseResourceID(req.NetworkID)
	if !ok || network.subnet != "" {
		return "", fmt.Errorf("%q is not an Azure virtual network ID (/subscriptions/<id>/resourceGroups/<group>/"+
			"providers/Microsoft.Network/virtualNetworks/<name>)", req.NetworkID)
	}
	if !strings.EqualFold(network.subscription, target.Account) {
		return "", fmt.Errorf("virtual network %s is not in subscription %s", req.NetworkID, target.Account)
	}
	if req.Zone != "" {
		return "", fmt.Errorf("zone %q cannot be used: Azure subnets are regional", req.Zone)
	}
	if e := validateSubnetName(req.Name); e != "" {
		return "", fmt.Errorf("%q is not a subnet name the operator creates: %s", req.Name, e)
	}
	c, err := d.writeClientsFor(target)
	if err != nil {
		return "", err
	}
	id := canonicalID(network.vnetScope() + "/subnets/" + req.Name)

	existing, err := d.getSubnet(ctx, c, target, network, req.Name)
	if err != nil {
		return "", fmt.Errorf("read subnet %s: %w", id, d.explain(c, err))
	}
	if existing != nil {
		return d.existingSubnet(ctx, c, target, network, id, existing, req)
	}
	if err := d.writeSubnetEntry(ctx, c, target, network, req.Name, req.Tags, false); err != nil {
		return "", err
	}

	subnet := armnetwork.Subnet{Properties: &armnetwork.SubnetPropertiesFormat{AddressPrefix: new(req.CIDRBlock)}}
	var poller *runtime.Poller[armnetwork.SubnetsClientCreateOrUpdateResponse]
	createOnly := policy.WithHTTPHeader(ctx, http.Header{"If-None-Match": []string{"*"}})
	err = d.call(createOnly, target, armWriteBucket(c.id, target.Account), "subnets.createOrUpdate",
		func(ctx context.Context) (*http.Response, error) {
			var resp *http.Response
			var err error
			poller, err = c.subnets.BeginCreateOrUpdate(policy.WithCaptureResponse(ctx, &resp), network.group,
				network.vnet, req.Name, subnet, nil)
			return resp, err
		})
	if err == nil {
		_, err = pollDone(ctx, d, target, c, "subnets.createOrUpdate", poller)
	}
	if err != nil {
		return "", d.createError(c, id, req, err)
	}
	return id, nil
}

// createError says why a subnet was not created: a CIDR ARM refuses because it overlaps
// another subnet or lies outside the address space is inventory.ErrCIDRConflict, so the claim
// allocates another one.
func (d *Discoverer) createError(c *writeClients, id string, req inventory.CreateSubnetRequest, err error) error {
	switch {
	case isRangeConflict(err):
		return fmt.Errorf("%w: %s: %s", inventory.ErrCIDRConflict, req.CIDRBlock, err.Error())
	case isExistsAlready(err):
		return fmt.Errorf("subnet %s exists already (created by somebody else a moment ago); the operator never "+
			"changes an existing subnet, so choose another namePrefix: %w", id, err)
	case isBusy(err):
		return fmt.Errorf("create subnet %s: another operation on the virtual network is still in progress after "+
			"%d attempts; the claim retries: %w", id, retryMaxAttempts, err)
	case isThrottle(err):
		return fmt.Errorf("%w: create subnet %s: %w", inventory.ErrThrottled, id, err)
	}
	return fmt.Errorf("create subnet %s: %w", id, d.explain(c, err))
}

// existingSubnet decides about a subnet that already has the name a create asked for.
func (d *Discoverer) existingSubnet(ctx context.Context, c *writeClients, target inventory.Target, network resourceRef,
	id string, s *armnetwork.Subnet, req inventory.CreateSubnetRequest) (string, error) {
	var prefixes []string
	if p := s.Properties; p != nil {
		if p.AddressPrefix != nil {
			prefixes = append(prefixes, *p.AddressPrefix)
		}
		for _, prefix := range p.AddressPrefixes {
			if prefix != nil {
				prefixes = append(prefixes, *prefix)
			}
		}
	}
	claimKey := networkv1.OperatorTagKeysFor(networkv1.ProviderAzure).Claim
	if slices.Equal(prefixes, []string{req.CIDRBlock}) && req.Tags[claimKey] != "" {
		tags, err := d.readTags(ctx, c, target, network)
		if err != nil {
			return "", err
		}
		_, entry, _ := findEntry(tags, deref(s.Name))
		if v, ok := lookupFold(entry, claimKey); ok && v == req.Tags[claimKey] {
			// Ours: whatever the entry still lacks is added, as the first attempt would have.
			if err := d.writeSubnetEntry(ctx, c, target, network, deref(s.Name), req.Tags, false); err != nil {
				return "", err
			}
			return id, nil
		}
	}
	return "", fmt.Errorf("subnet %s exists already, with %s, and was not created for this claim; the operator "+
		"never changes an existing subnet, so choose another namePrefix", id, strings.Join(prefixes, ", "))
}

// getSubnet reads a subnet, nil when it does not exist.
func (d *Discoverer) getSubnet(ctx context.Context, c *writeClients, target inventory.Target, network resourceRef,
	name string) (*armnetwork.Subnet, error) {
	var got *armnetwork.Subnet
	err := d.call(ctx, target, armBucket(c.id, target.Account), "subnets.get",
		func(ctx context.Context) (*http.Response, error) {
			var resp *http.Response
			r, err := c.subnets.Get(policy.WithCaptureResponse(ctx, &resp), network.group, network.vnet, name, nil)
			if err == nil {
				got = &r.Subnet
			}
			return resp, err
		})
	if isNotFound(err) {
		return nil, nil
	}
	return got, err
}

// WriteOwnership adds the tags to a virtual network, or to a subnet's entry on its virtual
// network: a name not carried yet gets its value, a name carried with the same value is left
// alone, and a name carried with another value refuses the whole write
// (inventory.ErrOwnershipConflict) before anything is written.
func (d *Discoverer) WriteOwnership(ctx context.Context, target inventory.Target, resourceID string, tags map[string]string) error {
	if len(tags) == 0 {
		return nil
	}
	ref, ok := parseResourceID(resourceID)
	if !ok {
		return fmt.Errorf("%q is not an Azure virtual network or subnet ID", resourceID)
	}
	if !strings.EqualFold(ref.subscription, target.Account) {
		return fmt.Errorf("%s is not in subscription %s", resourceID, target.Account)
	}
	c, err := d.writeClientsFor(target)
	if err != nil {
		return err
	}
	if ref.subnet != "" {
		return d.writeSubnetEntry(ctx, c, target, ref, ref.subnet, tags, true)
	}

	current, err := d.readTags(ctx, c, target, ref)
	if err != nil {
		return err
	}
	own := ownTags(current)
	var conflicts []string
	missing := map[string]string{}
	for _, key := range slices.Sorted(maps.Keys(tags)) {
		have, carried := lookupFold(own, key)
		switch {
		case !carried:
			missing[key] = tags[key]
		case have != tags[key]:
			conflicts = append(conflicts, fmt.Sprintf("%s is %q, not %q", key, have, tags[key]))
		}
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("%w: virtual network %s already carries %s; the operator never replaces an ownership "+
			"value, so nothing was written", inventory.ErrOwnershipConflict, resourceID, strings.Join(conflicts, ", "))
	}
	if len(missing) == 0 {
		return nil
	}
	if n := len(current) + len(missing); n > maxTagsPerResource {
		return budgetError(ref, len(current), len(missing))
	}
	return d.mergeTags(ctx, c, target, ref, missing)
}

// writeSubnetEntry adds the tags to the entry of a subnet on its virtual network. With
// inherited, the subnet exists and the values it already has are its effective tags (the
// network's with its entry laid over them); without, the subnet is being created and only an
// entry left under its name counts. Either way a pair the network carries with the same value
// is not written: the subnet inherits it.
func (d *Discoverer) writeSubnetEntry(ctx context.Context, c *writeClients, target inventory.Target, network resourceRef,
	subnet string, tags map[string]string, inherited bool) error {
	if len(tags) == 0 {
		return nil
	}
	current, err := d.readTags(ctx, c, target, network)
	if err != nil {
		return err
	}
	own := ownTags(current)
	entryName, entry, hasEntry := findEntry(current, subnet)
	if !hasEntry {
		entryName = SubnetEntryName(subnet)
		entry = map[string]string{}
	}
	var conflicts []string
	missing := map[string]string{}
	for _, key := range slices.Sorted(maps.Keys(tags)) {
		want := tags[key]
		if have, ok := lookupFold(entry, key); ok {
			if have != want {
				conflicts = append(conflicts, fmt.Sprintf("%s is %q, not %q", key, have, want))
			}
			continue
		}
		if have, ok := lookupFold(own, key); ok {
			switch {
			case have == want:
				continue
			case inherited:
				conflicts = append(conflicts, fmt.Sprintf("%s is %q (inherited from the virtual network), not %q",
					key, have, want))
				continue
			}
		}
		missing[key] = want
	}
	subnetID := canonicalID(network.vnetScope() + "/subnets/" + subnet)
	if len(conflicts) > 0 {
		return fmt.Errorf("%w: subnet %s already carries %s; the operator never replaces an ownership value, so "+
			"nothing was written", inventory.ErrOwnershipConflict, subnetID, strings.Join(conflicts, ", "))
	}
	if len(missing) == 0 {
		return nil
	}
	merged := maps.Clone(entry)
	maps.Copy(merged, missing)
	value := EncodeSubnetEntry(merged)
	if len(value) > maxTagValueLength {
		return fmt.Errorf("%w: the ownership of subnet %s would be %d characters in its entry %s on the virtual "+
			"network, and an Azure tag value holds %d; use shorter tag names and values, or fewer tags",
			inventory.ErrOwnershipEntryTooLong, subnetID, len(value), entryName, maxTagValueLength)
	}
	if !hasEntry && len(current)+1 > maxTagsPerResource {
		return budgetError(network, len(current), 1)
	}
	return d.mergeTags(ctx, c, target, network, map[string]string{entryName: value})
}

func budgetError(network resourceRef, carried, adding int) error {
	return fmt.Errorf("%w: virtual network %s carries %d tags, and adding %d would pass Azure's limit of %d per "+
		"resource; its subnets' ownership entries (hs-subnet-*) share the limit with its own tags, so remove tags "+
		"it no longer needs", inventory.ErrTagBudgetExceeded, canonicalID(network.vnetScope()), carried, adding,
		maxTagsPerResource)
}

// readTags reads the tags of a virtual network through the Tags API, which the write identity
// may read with the Tag Contributor role alone.
func (d *Discoverer) readTags(ctx context.Context, c *writeClients, target inventory.Target,
	network resourceRef) (map[string]string, error) {
	out := map[string]string{}
	err := d.call(ctx, target, armBucket(c.id, target.Account), "tags.getAtScope",
		func(ctx context.Context) (*http.Response, error) {
			var resp *http.Response
			r, err := c.tags.GetAtScope(policy.WithCaptureResponse(ctx, &resp), strings.TrimPrefix(network.vnetScope(), "/"), nil)
			if err == nil && r.Properties != nil {
				for k, v := range r.Properties.Tags {
					out[k] = deref(v)
				}
			}
			return resp, err
		})
	if err != nil {
		if isThrottle(err) {
			return nil, fmt.Errorf("%w: read the tags of %s: %w", inventory.ErrThrottled, canonicalID(network.vnetScope()), err)
		}
		return nil, fmt.Errorf("read the tags of %s: %w", canonicalID(network.vnetScope()), d.explain(c, err))
	}
	return out, nil
}

// mergeTags adds tags to a virtual network with the Tags API's Merge operation, which sends
// only these names.
func (d *Discoverer) mergeTags(ctx context.Context, c *writeClients, target inventory.Target, network resourceRef,
	tags map[string]string) error {
	patch := armresources.TagsPatchResource{Operation: new(armresources.TagsPatchOperationMerge),
		Properties: &armresources.Tags{Tags: map[string]*string{}}}
	for k, v := range tags {
		patch.Properties.Tags[k] = new(v)
	}
	var poller *runtime.Poller[armresources.TagsClientUpdateAtScopeResponse]
	err := d.call(ctx, target, armWriteBucket(c.id, target.Account), "tags.updateAtScope",
		func(ctx context.Context) (*http.Response, error) {
			var resp *http.Response
			var err error
			poller, err = c.tags.BeginUpdateAtScope(policy.WithCaptureResponse(ctx, &resp),
				strings.TrimPrefix(network.vnetScope(), "/"), patch, nil)
			return resp, err
		})
	if err == nil {
		_, err = pollDone(ctx, d, target, c, "tags.updateAtScope", poller)
	}
	id := canonicalID(network.vnetScope())
	switch {
	case err == nil:
		return nil
	case isTagLimit(err):
		return fmt.Errorf("%w: virtual network %s: %w", inventory.ErrTagBudgetExceeded, id, err)
	case isThrottle(err):
		return fmt.Errorf("%w: tag %s: %w", inventory.ErrThrottled, id, err)
	case isBusy(err):
		return fmt.Errorf("tag %s: another operation on the virtual network is still in progress after %d "+
			"attempts: %w", id, retryMaxAttempts, err)
	}
	return fmt.Errorf("tag %s: %w", id, d.explain(c, err))
}

// pollDone polls a long-running operation to its end and returns its result. Each poll is one
// call of the pacing, so a throttled poll waits and polls again; between polls it waits what
// ARM's Retry-After asks for, or Options.PollInterval.
func pollDone[T any](ctx context.Context, d *Discoverer, target inventory.Target, c *writeClients, operation string,
	poller *runtime.Poller[T]) (T, error) {
	var zero T
	var last *http.Response
	for !poller.Done() {
		if err := d.sleep(ctx, d.pollWait(last)); err != nil {
			return zero, err
		}
		err := d.call(ctx, target, armBucket(c.id, target.Account), operation+".poll",
			func(ctx context.Context) (*http.Response, error) {
				resp, err := poller.Poll(ctx)
				if err == nil {
					last = resp
				}
				return resp, err
			})
		if err != nil {
			return zero, err
		}
	}
	var result T
	err := d.call(ctx, target, armBucket(c.id, target.Account), operation+".result",
		func(ctx context.Context) (*http.Response, error) {
			var resp *http.Response
			var err error
			result, err = poller.Result(policy.WithCaptureResponse(ctx, &resp))
			return resp, err
		})
	return result, err
}

// pollWait is how long to wait before the next poll.
func (d *Discoverer) pollWait(last *http.Response) time.Duration {
	if d.opts.PollInterval > 0 {
		return d.opts.PollInterval
	}
	if wait := retryAfter(last); wait > 0 {
		return min(wait, maxPace)
	}
	return defaultPollInterval
}

// explain adds to an authorization failure which identity lacks what.
func (d *Discoverer) explain(c *writeClients, err error) error {
	if respErr, ok := errors.AsType[*azcore.ResponseError](err); ok && respErr.StatusCode == http.StatusForbidden {
		return fmt.Errorf("the write identity (%s) lacks a permission; grant it the writer role "+
			"(deploy/azure/writer-role.json), or Tag Contributor for imports alone: %w", c.id, err)
	}
	return err
}

// ownTags are a virtual network's tags without its subnets' entries.
func ownTags(tags map[string]string) map[string]string {
	own := map[string]string{}
	for k, v := range tags {
		if len(k) > len(subnetEntryPrefix) && strings.EqualFold(k[:len(subnetEntryPrefix)], subnetEntryPrefix) {
			continue
		}
		own[k] = v
	}
	return own
}

// findEntry returns the entry of a subnet among a virtual network's tags, whatever the case of
// either name, with the name it is spelt with.
func findEntry(tags map[string]string, subnet string) (string, map[string]string, bool) {
	want := SubnetEntryName(subnet)
	for k, v := range tags {
		if strings.EqualFold(k, want) {
			return k, DecodeSubnetEntry(v), true
		}
	}
	return "", nil, false
}

// lookupFold finds a tag by name without regard to case, as Azure does.
func lookupFold(tags map[string]string, name string) (string, bool) {
	if v, ok := tags[name]; ok {
		return v, true
	}
	for k, v := range tags {
		if strings.EqualFold(k, name) {
			return v, true
		}
	}
	return "", false
}

// validateSubnetName says what is wrong with a subnet name the operator would create, or "".
func validateSubnetName(name string) string {
	if !subnetNamePattern.MatchString(name) {
		return "an Azure subnet name is 1 to 80 letters, digits, \"_\", \".\" and \"-\", starting with a letter or " +
			"digit and ending with a letter, digit or \"_\""
	}
	for _, reserved := range reservedSubnetNames {
		if strings.EqualFold(name, reserved) {
			return reserved + " is a subnet name Azure reserves for its own service"
		}
	}
	return ""
}

func responseError(err error) (*azcore.ResponseError, bool) {
	return errors.AsType[*azcore.ResponseError](err)
}

func isNotFound(err error) bool {
	respErr, ok := responseError(err)
	return ok && respErr.StatusCode == http.StatusNotFound
}

// isRangeConflict recognises ARM refusing a subnet's prefix because it overlaps another
// subnet of the virtual network (NetcfgSubnetRangesOverlap) or lies outside its address space
// (NetcfgSubnetRangeOutsideVnet). The codes are those the Azure documentation and the research
// note (#41) name; the run against real subscriptions (#56) confirms them.
func isRangeConflict(err error) bool {
	respErr, ok := responseError(err)
	if !ok {
		return false
	}
	switch respErr.ErrorCode {
	case "NetcfgSubnetRangesOverlap", "NetcfgSubnetRangeOutsideVnet":
		return true
	}
	return false
}

// isExistsAlready recognises a subnet that appeared between the read and the create: the PUT's
// If-None-Match: * refused it, or ARM refused to change a subnet in use.
func isExistsAlready(err error) bool {
	respErr, ok := responseError(err)
	return ok && (respErr.StatusCode == http.StatusPreconditionFailed ||
		respErr.ErrorCode == "InUseSubnetCannotBeUpdated" || respErr.ErrorCode == "InUseSubnetCannotBeDeleted")
}

// isBusy recognises Microsoft.Network refusing a write because another operation holds the
// virtual network (409 AnotherOperationInProgress); its 429 RetryableErrorDueToAnotherOperation
// is throttling. Both are retried.
func isBusy(err error) bool {
	respErr, ok := responseError(err)
	return ok && respErr.StatusCode == http.StatusConflict && respErr.ErrorCode == "AnotherOperationInProgress"
}

// isTagLimit recognises ARM refusing a tag write that would take the resource past 50 tags,
// which the write checks before it asks; this is for a tag somebody added in between.
func isTagLimit(err error) bool {
	respErr, ok := responseError(err)
	if !ok || respErr.StatusCode != http.StatusBadRequest {
		return false
	}
	return respErr.ErrorCode == "InvalidTagCount" || strings.Contains(strings.ToLower(respErr.Error()), "maximum of 50 tags")
}
