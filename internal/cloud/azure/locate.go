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
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"

	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

// The read before an import (#116, #117). The Tags API writes a name on a virtual network
// without asking what the name stands for, so an import of a subnet that does not exist (a
// misspelt name) would leave an hs-subnet-<name> entry that names nothing, counts against the
// network's 50 tags and is never removed. And a resource ID names no location, while every
// Azure resource has one. Both are answered by one read of the virtual network, which returns
// its location and its subnets inline: virtualNetworks.get, as the read identity.
//
// The read identity, because it is a read: the reader role holds
// Microsoft.Network/virtualNetworks/read and the built-in Tag Contributor, which is all a write
// identity needs for imports, does not; and a dry run reports the same without any write
// identity. It counts against the same read quota as discovery (armBucket) and goes through the
// same pacing.
//
// The subnet may still be deleted between this read and the write, one ARM round trip later:
// the Tags API has no precondition to close that window with.

var _ provider.ImportLocator = (*Provider)(nil)

// maxSubnetsNamed is how many of a virtual network's subnets a "no such subnet" message lists:
// enough to spot a misspelt name, short enough for a condition message.
const maxSubnetsNamed = 10

// ImportNetwork implements provider.ImportLocator: the virtual network an import's resource ID
// names or belongs to, in lowercase, and the subnet's name as the ID spells it.
func (p *Provider) ImportNetwork(resourceID string) (networkID, subnet string, ok bool) {
	ref, ok := parseResourceID(resourceID)
	if !ok {
		return "", "", false
	}
	return canonicalID(ref.vnetScope()), ref.subnet, true
}

// LocateImport implements provider.ImportLocator with one virtualNetworks.get as the target's
// identity, the read identity.
func (p *Provider) LocateImport(ctx context.Context, target inventory.Target, resourceID string) (string, error) {
	return p.discoverer.locateImport(ctx, target, resourceID)
}

// locateImport reads the virtual network the ID names, or the one its subnet belongs to, and
// returns its location as ARM spells it in resource IDs (normalLocation). A virtual network
// that does not exist, or one without that subnet, is inventory.ErrResourceNotFound.
func (d *Discoverer) locateImport(ctx context.Context, target inventory.Target, resourceID string) (string, error) {
	ref, ok := parseResourceID(resourceID)
	if !ok {
		return "", fmt.Errorf("%q is not an Azure virtual network or subnet ID", resourceID)
	}
	if !strings.EqualFold(ref.subscription, target.Account) {
		return "", fmt.Errorf("%s is not in subscription %s", resourceID, target.Account)
	}
	id, err := identityOf(target)
	if err != nil {
		return "", err
	}
	c, err := d.clientFor(id, target.Account)
	if err != nil {
		return "", err
	}
	networkID := canonicalID(ref.vnetScope())
	var vnet *armnetwork.VirtualNetwork
	err = d.call(ctx, target, armBucket(id, target.Account), "virtualNetworks.get",
		func(ctx context.Context) (*http.Response, error) {
			var resp *http.Response
			r, err := c.Get(policy.WithCaptureResponse(ctx, &resp), ref.group, ref.vnet, nil)
			if err == nil {
				vnet = &r.VirtualNetwork
			}
			return resp, err
		})
	switch {
	case isNotFound(err):
		// ARM says which part is missing (ResourceNotFound, ResourceGroupNotFound,
		// SubscriptionNotFound), so its answer stays in the message.
		return "", fmt.Errorf("%w: virtual network %s was not found in Azure; check the resource group and the "+
			"name: %w", inventory.ErrResourceNotFound, networkID, err)
	case isThrottle(err):
		return "", fmt.Errorf("%w: read virtual network %s: %w", inventory.ErrThrottled, networkID, err)
	case err != nil:
		return "", fmt.Errorf("read virtual network %s: %w", networkID, explainRead(id, err))
	}
	location := normalLocation(deref(vnet.Location))
	if location == "" {
		return "", fmt.Errorf("read virtual network %s: Azure reported no location for it", networkID)
	}
	if ref.subnet == "" {
		return location, nil
	}
	var names []string
	for _, s := range subnetsOf(vnet) {
		if strings.EqualFold(deref(s.Name), ref.subnet) {
			return location, nil
		}
		names = append(names, deref(s.Name))
	}
	return "", fmt.Errorf("%w: virtual network %s has no subnet %q (%s); nothing was written, since the "+
		"ownership entry of a subnet that does not exist would stay on the virtual network and count against its "+
		"%d tags", inventory.ErrResourceNotFound, networkID, ref.subnet, subnetsNamed(names), maxTagsPerResource)
}

// subnetsNamed lists the subnets a virtual network has, for the message about one it has not.
func subnetsNamed(names []string) string {
	if len(names) == 0 {
		return "it has no subnets"
	}
	slices.Sort(names)
	if len(names) > maxSubnetsNamed {
		return fmt.Sprintf("it has %s and %d more", strings.Join(names[:maxSubnetsNamed], ", "),
			len(names)-maxSubnetsNamed)
	}
	return "it has " + strings.Join(names, ", ")
}

// explainRead adds to an authorization failure of the read before an import which identity
// lacks what. A scope limited to resource groups usually has its reader role on those groups
// alone, so a virtual network in another group ends here.
func explainRead(id Identity, err error) error {
	if respErr, ok := responseError(err); ok && respErr.StatusCode == http.StatusForbidden {
		return fmt.Errorf("the read identity (%s) may not read it; an import reads its virtual network before "+
			"it writes, which needs Microsoft.Network/virtualNetworks/read there (deploy/azure/reader-role.json): %w",
			id, err)
	}
	return err
}
