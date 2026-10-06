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

package gcp

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"cloud.google.com/go/compute/apiv1/computepb"
	"cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/codes"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// Writes (#47): a subnetwork is created with its tags bound in the same call
// (params.resourceManagerTags), and an import binds tags with tagBindings.create. Both only
// add. A resource can carry one value per tag key, and replacing it would mean deleting the
// binding first, which the operator never does: a write that would change a bound value is
// refused as a whole with inventory.ErrOwnershipConflict, before anything is bound.
//
// Tag keys are never created: they are the platform team's, under the scope's tagParent. Tag
// values are created only when the scope sets gcp.createTagValues; otherwise a missing one is
// inventory.ErrTagValueMissing, named in the error. Which tags a claim binds is the claim
// controller's decision: the claim tag hs-claim only with gcp.claimTag Bind.

var (
	// networkIDPattern is the canonical ID of a VPC network: projects/<project>/global/networks/<name>.
	networkIDPattern = regexp.MustCompile(`^projects/([a-z][a-z0-9-]{4,28}[a-z0-9])/global/networks/([a-z]([-a-z0-9]{0,61}[a-z0-9])?)$`)
	// subnetIDPattern is the canonical ID of a subnetwork:
	// projects/<project>/regions/<region>/subnetworks/<name>.
	subnetIDPattern = regexp.MustCompile(`^projects/([a-z][a-z0-9-]{4,28}[a-z0-9])/regions/([a-z]+(-[a-z]+)+[0-9]+)/subnetworks/([a-z]([-a-z0-9]{0,61}[a-z0-9])?)$`)
	// resourceNamePattern is what Compute accepts as the name of a network or subnetwork.
	resourceNamePattern = regexp.MustCompile(`^[a-z]([-a-z0-9]{0,61}[a-z0-9])?$`)
)

// resourceRef is a network or subnetwork named by its canonical ID.
type resourceRef struct {
	project, region, name string // region is empty for a network
}

func (r resourceRef) subnet() bool { return r.region != "" }

func parseResourceID(id string) (resourceRef, bool) {
	if m := networkIDPattern.FindStringSubmatch(id); m != nil {
		return resourceRef{project: m[1], name: m[2]}, true
	}
	if m := subnetIDPattern.FindStringSubmatch(id); m != nil {
		return resourceRef{project: m[1], region: m[2], name: m[4]}, true
	}
	return resourceRef{}, false
}

// writeClients returns the API clients of the target's identity, which for a write is the
// account's write identity.
func (d *Discoverer) writeClients(ctx context.Context, target inventory.Target) (*clients, error) {
	id, err := identityOf(target)
	if err != nil {
		return nil, err
	}
	return d.clientsFor(ctx, id)
}

// CreateSubnet creates a regional subnetwork with its tags bound at creation. A range Compute
// refuses because it overlaps another one is inventory.ErrCIDRConflict. A subnetwork of the
// same name that already exists with the same network and range is the one an earlier attempt
// created (its status update was lost), and is returned as created.
func (d *Discoverer) CreateSubnet(ctx context.Context, target inventory.Target, req inventory.CreateSubnetRequest) (string, error) {
	network, ok := parseResourceID(req.NetworkID)
	if !ok || network.subnet() {
		return "", fmt.Errorf("%q is not a GCP network ID (projects/<project>/global/networks/<name>)", req.NetworkID)
	}
	if network.project != target.Account {
		return "", fmt.Errorf("network %s is not in project %s", req.NetworkID, target.Account)
	}
	if req.Zone != "" {
		return "", fmt.Errorf("GCP subnetworks are regional; zone %q cannot be used", req.Zone)
	}
	if !resourceNamePattern.MatchString(req.Name) {
		return "", fmt.Errorf("%q is not a valid subnetwork name: 1 to 63 lowercase letters, digits and hyphens, "+
			"starting with a letter", req.Name)
	}
	c, err := d.writeClients(ctx, target)
	if err != nil {
		return "", err
	}
	values, err := d.tagValuesFor(ctx, c, target, req.Tags)
	if err != nil {
		return "", err
	}
	id := "projects/" + target.Account + "/regions/" + target.Region + "/subnetworks/" + req.Name

	subnet := &computepb.Subnetwork{
		Name:        new(req.Name),
		Network:     new(req.NetworkID),
		IpCidrRange: new(req.CIDRBlock),
	}
	if req.GCP != nil && req.GCP.PrivateIPGoogleAccess {
		subnet.PrivateIpGoogleAccess = new(true)
	}
	if len(values) > 0 {
		subnet.Params = &computepb.SubnetworkParams{ResourceManagerTags: values}
	}
	err = d.call(ctx, target, computeBucket(target.Account, target.Region), "subnetworks.insert", func(ctx context.Context) error {
		op, err := c.subnetworks.Insert(ctx, &computepb.InsertSubnetworkRequest{
			Project: target.Account, Region: target.Region, SubnetworkResource: subnet,
		})
		if err != nil {
			return err
		}
		return op.Wait(ctx)
	})
	switch {
	case err == nil:
		return id, nil
	case isAlreadyExists(err):
		return d.existingSubnet(ctx, c, target, id, req)
	case isRangeConflict(err):
		return "", fmt.Errorf("%w: %s", inventory.ErrCIDRConflict, err.Error())
	case isThrottle(err):
		return "", fmt.Errorf("%w: create subnetwork %s: %w", inventory.ErrThrottled, id, err)
	default:
		return "", fmt.Errorf("create subnetwork %s: %w", id, err)
	}
}

// existingSubnet decides about a subnetwork that already has the name a create asked for.
func (d *Discoverer) existingSubnet(ctx context.Context, c *clients, target inventory.Target, id string,
	req inventory.CreateSubnetRequest) (string, error) {
	var s *computepb.Subnetwork
	err := d.call(ctx, target, computeBucket(target.Account, target.Region), "subnetworks.get", func(ctx context.Context) error {
		var err error
		s, err = c.subnetworks.Get(ctx, &computepb.GetSubnetworkRequest{
			Project: target.Account, Region: target.Region, Subnetwork: req.Name,
		})
		return err
	})
	if err != nil {
		return "", fmt.Errorf("subnetwork %s exists already and could not be read: %w", id, err)
	}
	if relativeName(s.GetNetwork()) == req.NetworkID && s.GetIpCidrRange() == req.CIDRBlock {
		return id, nil
	}
	return "", fmt.Errorf("subnetwork %s exists already, in %s with range %s; choose another namePrefix",
		id, relativeName(s.GetNetwork()), s.GetIpCidrRange())
}

// WriteOwnership binds the tags to the network or subnetwork: every key not bound yet gets its
// value, a key bound to the same value is left alone, and a key bound to another value refuses
// the whole write (inventory.ErrOwnershipConflict) before anything is bound.
func (d *Discoverer) WriteOwnership(ctx context.Context, target inventory.Target, resourceID string, tags map[string]string) error {
	if len(tags) == 0 {
		return nil
	}
	ref, ok := parseResourceID(resourceID)
	if !ok {
		return fmt.Errorf("%q is not a GCP network or subnetwork ID", resourceID)
	}
	if ref.project != target.Account {
		return fmt.Errorf("%s is not in project %s", resourceID, target.Account)
	}
	c, err := d.writeClients(ctx, target)
	if err != nil {
		return err
	}
	location, resource, err := d.resourceName(ctx, c, target, ref)
	if err != nil {
		return fmt.Errorf("look up %s: %w", resourceID, err)
	}
	tagParent, err := d.tagParent(ctx, c, target)
	if err != nil {
		return err
	}
	bound, err := d.directTags(ctx, c, target, tagParent, location, resource)
	if err != nil {
		return fmt.Errorf("read the tags of %s: %w", resourceID, err)
	}

	var conflicts []string
	missing := map[string]string{}
	for _, key := range slices.Sorted(maps.Keys(tags)) {
		have, isBound := bound[key]
		switch {
		case !isBound:
			missing[key] = tags[key]
		case have != tags[key]:
			conflicts = append(conflicts, fmt.Sprintf("%s is %q, not %q", key, have, tags[key]))
		}
	}
	if len(conflicts) > 0 {
		return fmt.Errorf("%w: %s already carries %s; a GCP resource carries one value per tag key and the "+
			"operator never removes a binding, so nothing was bound", inventory.ErrOwnershipConflict, resourceID,
			strings.Join(conflicts, ", "))
	}
	values, err := d.tagValuesFor(ctx, c, target, missing)
	if err != nil {
		return err
	}
	tb, err := c.tagBindingsAt(ctx, location)
	if err != nil {
		return err
	}
	for _, keyName := range slices.Sorted(maps.Keys(values)) {
		value := values[keyName]
		err := d.call(ctx, target, resourceManagerBucket(location), "tagBindings.create", func(ctx context.Context) error {
			op, err := tb.CreateTagBinding(ctx, &resourcemanagerpb.CreateTagBindingRequest{
				TagBinding: &resourcemanagerpb.TagBinding{Parent: resource, TagValue: value},
			})
			if err != nil {
				return err
			}
			_, err = op.Wait(ctx)
			return err
		})
		if err != nil && !isAlreadyExists(err) {
			if isThrottle(err) {
				return fmt.Errorf("%w: bind %s to %s: %w", inventory.ErrThrottled, value, resourceID, err)
			}
			return fmt.Errorf("bind %s to %s: %w", value, resourceID, err)
		}
	}
	return nil
}

// resourceName returns the Resource Manager location and full resource name of a network or
// subnetwork, which name the project by number and the resource by numeric ID.
func (d *Discoverer) resourceName(ctx context.Context, c *clients, target inventory.Target, ref resourceRef) (
	location, resource string, err error) {
	number, err := d.projectNumber(ctx, c, target, ref.project)
	if err != nil {
		return "", "", err
	}
	if !ref.subnet() {
		var n *computepb.Network
		err = d.call(ctx, target, computeBucket(ref.project, ""), "networks.get", func(ctx context.Context) error {
			n, err = c.networks.Get(ctx, &computepb.GetNetworkRequest{Project: ref.project, Network: ref.name})
			return err
		})
		if err != nil {
			return "", "", err
		}
		return globalLocation, fmt.Sprintf("%sprojects/%s/global/networks/%d", computeResource, number, n.GetId()), nil
	}
	var s *computepb.Subnetwork
	err = d.call(ctx, target, computeBucket(ref.project, ref.region), "subnetworks.get", func(ctx context.Context) error {
		s, err = c.subnetworks.Get(ctx, &computepb.GetSubnetworkRequest{Project: ref.project, Region: ref.region,
			Subnetwork: ref.name})
		return err
	})
	if err != nil {
		return "", "", err
	}
	return ref.region, fmt.Sprintf("%sprojects/%s/regions/%s/subnetworks/%d", computeResource, number, ref.region,
		s.GetId()), nil
}

// directTags reads the tags bound to one resource directly, decoded like discovery does.
func (d *Discoverer) directTags(ctx context.Context, c *clients, target inventory.Target, tagParent, location,
	resource string) (map[string]string, error) {
	r := []discovered{{location: location, resource: resource}}
	if err := d.readTags(ctx, c, target, tagParent, r); err != nil {
		return nil, err
	}
	if r[0].gone {
		return nil, fmt.Errorf("%s was not found", resource)
	}
	return r[0].tags, nil
}

// tagNamespace is how the scope's tag parent appears in namespaced tag names: the
// organization number, or the project ID.
func tagNamespace(target inventory.Target) (string, error) {
	if target.GCP == nil || target.GCP.TagParent == "" {
		return "", errors.New("the scope has no gcp.tagParent, so the operator does not know which tag keys to bind")
	}
	parent := target.GCP.TagParent
	if ns, ok := strings.CutPrefix(parent, "organizations/"); ok {
		return ns, nil
	}
	if ns, ok := strings.CutPrefix(parent, "projects/"); ok {
		return ns, nil
	}
	return "", fmt.Errorf("gcp.tagParent %q is neither organizations/<number> nor projects/<project ID>", parent)
}

// tagValuesFor resolves each key=value to the tag key and value resources Resource Manager
// binds by (tagKeys/<id> → tagValues/<id>), creating a missing value when the scope allows it.
// Nothing is bound here, so a missing key or value refuses the write before it starts.
func (d *Discoverer) tagValuesFor(ctx context.Context, c *clients, target inventory.Target,
	tags map[string]string) (map[string]string, error) {
	if len(tags) == 0 {
		return nil, nil
	}
	ns, err := tagNamespace(target)
	if err != nil {
		return nil, err
	}
	parent := target.GCP.TagParent
	out := make(map[string]string, len(tags))
	for _, key := range slices.Sorted(maps.Keys(tags)) {
		value := tags[key]
		var tk *resourcemanagerpb.TagKey
		err := d.call(ctx, target, resourceManagerBucket(globalLocation), "tagKeys.getNamespaced", func(ctx context.Context) error {
			var err error
			tk, err = c.tagKeys.GetNamespacedTagKey(ctx, &resourcemanagerpb.GetNamespacedTagKeyRequest{Name: ns + "/" + key})
			return err
		})
		switch {
		case isMissing(err):
			return nil, fmt.Errorf("%w: %s/%s; create the tag key %s under %s (the operator never creates keys)",
				inventory.ErrTagKeyMissing, ns, key, key, parent)
		case err != nil:
			return nil, fmt.Errorf("look up tag key %s/%s: %w", ns, key, err)
		}

		name := ns + "/" + key + "/" + value
		var tv *resourcemanagerpb.TagValue
		err = d.call(ctx, target, resourceManagerBucket(globalLocation), "tagValues.getNamespaced", func(ctx context.Context) error {
			var err error
			tv, err = c.tagValues.GetNamespacedTagValue(ctx, &resourcemanagerpb.GetNamespacedTagValueRequest{Name: name})
			return err
		})
		switch {
		case isMissing(err) && target.GCP.CreateTagValues:
			tv, err = d.createTagValue(ctx, c, target, tk, value, name)
			if err != nil {
				return nil, err
			}
		case isMissing(err) && key == networkv1.OperatorTagKeysFor(networkv1.ProviderGCP).Claim:
			return nil, fmt.Errorf("%w: %s; create it under the tag key %s, set gcp.createTagValues on the "+
				"scope to let the operator create it, or set gcp.claimTag to Skip to bind no claim tag",
				inventory.ErrTagValueMissing, name, tk.GetName())
		case isMissing(err):
			return nil, fmt.Errorf("%w: %s; create it under the tag key %s, or set gcp.createTagValues on the "+
				"scope to let the operator create it", inventory.ErrTagValueMissing, name, tk.GetName())
		case err != nil:
			return nil, fmt.Errorf("look up tag value %s: %w", name, err)
		}
		out[tk.GetName()] = tv.GetName()
	}
	return out, nil
}

// createTagValue creates a value under a tag key. A value somebody created in the meantime is
// read back; a key that holds as many values as Resource Manager allows (1,000) is
// inventory.ErrTagValueLimit.
func (d *Discoverer) createTagValue(ctx context.Context, c *clients, target inventory.Target, key *resourcemanagerpb.TagKey,
	value, name string) (*resourcemanagerpb.TagValue, error) {
	var tv *resourcemanagerpb.TagValue
	err := d.call(ctx, target, resourceManagerBucket(globalLocation), "tagValues.create", func(ctx context.Context) error {
		op, err := c.tagValues.CreateTagValue(ctx, &resourcemanagerpb.CreateTagValueRequest{
			TagValue: &resourcemanagerpb.TagValue{Parent: key.GetName(), ShortName: value,
				Description: "Created by subnet-operator"},
		})
		if err != nil {
			return err
		}
		tv, err = op.Wait(ctx)
		return err
	})
	switch {
	case err == nil:
		return tv, nil
	case isAlreadyExists(err):
		err = d.call(ctx, target, resourceManagerBucket(globalLocation), "tagValues.getNamespaced", func(ctx context.Context) error {
			var err error
			tv, err = c.tagValues.GetNamespacedTagValue(ctx, &resourcemanagerpb.GetNamespacedTagValueRequest{Name: name})
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("read tag value %s: %w", name, err)
		}
		return tv, nil
	case isValueLimit(err):
		return nil, fmt.Errorf("%w: cannot create %s: tag key %s holds as many values as Resource Manager allows "+
			"(1,000); delete unused values of the key or use another one: %w", inventory.ErrTagValueLimit, name,
			key.GetName(), err)
	default:
		return nil, fmt.Errorf("create tag value %s: %w", name, err)
	}
}

// errorCode returns the HTTP status and the gRPC code of a Google API error, as far as the
// error carries them.
func errorCode(err error) (int, codes.Code) {
	httpCode, grpcCode := 0, codes.Unknown
	if gerr, ok := errors.AsType[*googleapi.Error](err); ok {
		httpCode = gerr.Code
	}
	if aerr, ok := errors.AsType[*apierror.APIError](err); ok {
		if httpCode == 0 {
			httpCode = aerr.HTTPCode()
		}
		if s := aerr.GRPCStatus(); s != nil {
			grpcCode = s.Code()
		}
	}
	return httpCode, grpcCode
}

// isMissing reports a resource that does not exist. Resource Manager answers a namespaced
// name that does not exist with 404, and with 403 when the caller may not know either way;
// only the first is taken as missing.
func isMissing(err error) bool {
	if err == nil {
		return false
	}
	httpCode, grpcCode := errorCode(err)
	return httpCode == http.StatusNotFound || grpcCode == codes.NotFound
}

func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	httpCode, grpcCode := errorCode(err)
	return httpCode == http.StatusConflict || grpcCode == codes.AlreadyExists
}

// isRangeConflict recognises Compute refusing a subnetwork range that overlaps another range
// of the network or of a peered network. The exact wording is not documented (research note
// #40); Compute answers 400 with a message that says the range conflicts or overlaps.
func isRangeConflict(err error) bool {
	if err == nil {
		return false
	}
	httpCode, _ := errorCode(err)
	if httpCode != 0 && httpCode != http.StatusBadRequest {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "conflicts with") || strings.Contains(msg, "overlaps with")
}

// isValueLimit recognises Resource Manager refusing a tag value because its key holds the
// maximum number of values. It answers FAILED_PRECONDITION (or RESOURCE_EXHAUSTED without a
// rate-limit reason) with a message about the limit.
func isValueLimit(err error) bool {
	if err == nil || isThrottle(err) {
		return false
	}
	httpCode, grpcCode := errorCode(err)
	if grpcCode != codes.FailedPrecondition && grpcCode != codes.ResourceExhausted &&
		httpCode != http.StatusBadRequest && httpCode != http.StatusPreconditionFailed {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "limit") || strings.Contains(msg, "maximum")
}
