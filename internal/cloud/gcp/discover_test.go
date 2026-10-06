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
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/googleapis/gax-go/v2/apierror"
	"google.golang.org/api/googleapi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/cloud/gcp/gcpfake"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

func contractTarget() inventory.Target {
	return inventory.Target{Provider: networkv1.ProviderGCP, Account: contractProject, Region: contractRegion,
		GCP: &networkv1.GCPScope{TagParent: "organizations/" + contractOrg}}
}

func discoverOK(t *testing.T, p *Provider, target inventory.Target) *inventory.Snapshot {
	t.Helper()
	snap, err := p.Discover(context.Background(), target)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	return snap
}

func TestDiscoverReportsRangesUsageAndIPv6(t *testing.T) {
	p, cloud := newFakeProvider(t, Options{})
	cloud.AddNetwork(contractProject, "shared", orgTag("hs-owner", "platform"))
	gke := cloud.AddSubnetwork(contractProject, contractRegion, "shared", "gke", "10.0.0.0/24", orgTag("hs-owner", "payments"))
	cloud.AddSubnetwork(contractProject, "us-central1", "shared", "elsewhere", "10.1.0.0/24")
	cloud.Update(func() {
		gke.Used = 10
		gke.Secondary = []gcpfake.Range{{Name: "pods", CIDR: "10.4.0.0/20", Used: 96}, {Name: "services", CIDR: "10.8.0.0/24"}}
		gke.StackType, gke.IPv6AccessType, gke.InternalIPv6Prefix = "IPV4_IPV6", "INTERNAL", "fd20:a:b:c::/64"
	})

	snap := discoverOK(t, p, contractTarget())
	if len(snap.Networks) != 1 || len(snap.Subnets) != 1 {
		t.Fatalf("got %d networks and %d subnets, want the network and the one subnet of %s", len(snap.Networks),
			len(snap.Subnets), contractRegion)
	}
	n := snap.Networks[0]
	if n.ID != "projects/contract-project/global/networks/shared" || n.Region != "" || n.Name != "shared" ||
		len(n.CIDRBlocks) != 0 || n.Tags["hs-owner"] != "platform" || n.GCP == nil || n.GCP.RoutingMode != "REGIONAL" {
		t.Errorf("network = %+v", n)
	}
	s := snap.Subnets[0]
	if s.ID != "projects/contract-project/regions/europe-west1/subnetworks/gke" || s.NetworkID != n.ID ||
		s.Region != contractRegion || s.Zone != "" || s.Name != "gke" || s.CIDRBlock != "10.0.0.0/24" {
		t.Errorf("subnet identity = %+v", s)
	}
	checkGKESubnet(t, s)
}

// checkGKESubnet checks the ranges, usage and GCP details of the subnet the test above made.
func checkGKESubnet(t *testing.T, s inventory.Subnet) {
	t.Helper()
	if s.TotalIPs == nil || *s.TotalIPs != 252 || s.AvailableIPs == nil || *s.AvailableIPs != 242 {
		t.Errorf("subnet usage = total %v, free %v, want 252 and 242 (a /24 less 4 reserved, 10 used)", s.TotalIPs, s.AvailableIPs)
	}
	if !slices.Equal(s.SecondaryCIDRBlocks, []string{"10.4.0.0/20", "10.8.0.0/24"}) {
		t.Errorf("secondary CIDR blocks = %v", s.SecondaryCIDRBlocks)
	}
	if !slices.Equal(s.IPv6CIDRBlocks, []string{"fd20:a:b:c::/64"}) {
		t.Errorf("IPv6 CIDR blocks = %v", s.IPv6CIDRBlocks)
	}
	g := s.GCP
	if g == nil || g.Purpose != "PRIVATE" || g.StackType != "IPV4_IPV6" || g.IPv6AccessType != "INTERNAL" || len(g.SecondaryRanges) != 2 {
		t.Fatalf("GCP status = %+v", g)
	}
	pods := g.SecondaryRanges[0]
	if pods.Name != "pods" || *pods.TotalIPs != 4096 || *pods.AvailableIPs != 4000 {
		t.Errorf("pods range = %s %s total %d free %d, want 4096 and 4000 (no reserved addresses)", pods.Name,
			pods.CIDRBlock, *pods.TotalIPs, *pods.AvailableIPs)
	}
	if s.Tags["hs-owner"] != "payments" || s.OwnershipSource != networkv1.OwnershipSourceSubnet {
		t.Errorf("subnet ownership = %v from %q", s.Tags, s.OwnershipSource)
	}
}

// A network's peerings come with networks.list, by the peer's resource name: active once both
// sides peered, inactive while only one has, or once the peer network is gone.
func TestDiscoverReportsPeerings(t *testing.T) {
	p, cloud := newFakeProvider(t, Options{})
	cloud.AddProject("other-project", "111111111111")
	cloud.AddNetwork(contractProject, "shared", orgTag("hs-owner", "platform"))
	cloud.AddNetwork(contractProject, "ml", orgTag("hs-owner", "ml"))
	cloud.AddNetwork("other-project", "apps")
	cloud.AddPeering(contractProject, "shared", "to-apps", "other-project", "apps")
	cloud.AddPeering("other-project", "apps", "to-shared", contractProject, "shared")
	cloud.AddPeering(contractProject, "ml", "to-apps", "other-project", "apps")

	snap := discoverOK(t, p, contractTarget())
	byName := map[string]inventory.Network{}
	for _, n := range snap.Networks {
		byName[n.Name] = n
	}
	want := []networkv1.GCPNetworkPeering{{Name: "to-apps", Network: "projects/other-project/global/networks/apps",
		State: "ACTIVE", StateDetails: "[2026-09-29T00:00:00.000-07:00]: Connected."}}
	if got := byName["shared"].GCP.Peerings; !slices.Equal(got, want) {
		t.Errorf("shared peerings = %+v, want %+v", got, want)
	}
	if got := byName["ml"].GCP.Peerings; len(got) != 1 || got[0].State != "INACTIVE" ||
		got[0].Network != "projects/other-project/global/networks/apps" {
		t.Errorf("ml peerings = %+v, want one INACTIVE peering with apps", got)
	}

	cloud.RemoveNetwork("other-project", "apps")
	snap = discoverOK(t, p, contractTarget())
	for _, n := range snap.Networks {
		if len(n.GCP.Peerings) != 1 || n.GCP.Peerings[0].State != "INACTIVE" {
			t.Errorf("%s peerings = %+v once the peer is gone, want one INACTIVE", n.Name, n.GCP.Peerings)
		}
	}
}

// Resource Manager only counts what is bound to the resource itself, names the scope's own
// keys by their short name, and keeps every other parent's keys namespaced.
func TestDiscoverDecodesDirectTagBindingsOnly(t *testing.T) {
	p, cloud := newFakeProvider(t, Options{})
	cloud.AddNetwork(contractProject, "vpc",
		orgTag("hs-owner", "team-a"),
		gcpfake.Binding{Parent: "organizations/" + contractOrg, Namespace: contractOrg, Key: "hs-env", Value: "prod",
			Inherited: true},
		gcpfake.Binding{Parent: "projects/777", Namespace: "other-project", Key: "hs-owner", Value: "somebody-else"},
	)
	n := discoverOK(t, p, contractTarget()).Networks[0]
	want := map[string]string{"hs-owner": "team-a", "other-project/hs-owner": "somebody-else"}
	if len(n.Tags) != len(want) {
		t.Errorf("tags = %v, want %v", n.Tags, want)
	}
	for k, v := range want {
		if n.Tags[k] != v {
			t.Errorf("tags = %v, want %v", n.Tags, want)
		}
	}
}

// A project as tag parent is named by ID in the scope, and by number on the tags.
func TestDiscoverResolvesAProjectTagParent(t *testing.T) {
	p, cloud := newFakeProvider(t, Options{})
	cloud.AddProject("tag-home", "555")
	cloud.AddNetwork(contractProject, "vpc", gcpfake.Binding{Parent: "projects/555", Namespace: "tag-home", Key: "hs-owner",
		Value: "team-b"})
	target := contractTarget()
	target.GCP = &networkv1.GCPScope{TagParent: "projects/tag-home"}
	if got := discoverOK(t, p, target).Networks[0].Tags; got["hs-owner"] != "team-b" {
		t.Errorf("tags = %v, want hs-owner=team-b from the project tag parent", got)
	}
}

// What the API is asked: utilization with the subnetworks, the project by number in
// Resource Manager names, the tags of a subnetwork from its region's endpoint, and paging
// followed to the end.
func TestDiscoverRequestShape(t *testing.T) {
	p, cloud := newFakeProvider(t, Options{})
	cloud.AddNetwork(contractProject, "vpc")
	for i, name := range []string{"a", "b", "c", "d", "e"} {
		cloud.AddSubnetwork(contractProject, contractRegion, "vpc", name, "10.0."+string(rune('0'+i))+".0/24")
	}
	if got := len(discoverOK(t, p, contractTarget()).Subnets); got != 5 {
		t.Errorf("%d subnets, want all 5 across the pages", got)
	}
	var sawUsage, sawRegionalTags bool
	for _, r := range cloud.Requests() {
		if strings.Contains(r, "/subnetworks?") && strings.Contains(r, "views=WITH_UTILIZATION") {
			sawUsage = true
		}
		if strings.HasPrefix(r, "GET /rm/"+contractRegion+"/v3/effectiveTags?") &&
			strings.Contains(r, "projects%2F"+contractNumber+"%2Fregions%2F"+contractRegion) {
			sawRegionalTags = true
		}
	}
	if !sawUsage || !sawRegionalTags {
		t.Errorf("utilization asked for: %v; subnetwork tags read at the regional endpoint by project number: %v",
			sawUsage, sawRegionalTags)
	}
}

func TestDiscoverRetriesThrottledCallsAndSlowsDown(t *testing.T) {
	var throttled []string
	p, cloud := newFakeProvider(t, Options{OnThrottle: func(_ inventory.Target, op string) {
		throttled = append(throttled, op)
	}})
	var waits []time.Duration
	p.discoverer.sleep = func(ctx context.Context, d time.Duration) error {
		if d > 0 {
			waits = append(waits, d)
		}
		return nil
	}
	cloud.AddNetwork(contractProject, "vpc")
	cloud.ThrottleNext(2)
	if _, err := p.Discover(context.Background(), contractTarget()); err != nil {
		t.Fatalf("a throttling burst shorter than the retries failed discovery: %v", err)
	}
	if len(throttled) != 2 {
		t.Errorf("throttled attempts reported: %v, want 2", throttled)
	}
	// Doubling while throttled, halving again once calls get through.
	if len(waits) < 2 || !slices.Equal(waits[:2], []time.Duration{time.Second, 2 * time.Second}) ||
		slices.ContainsFunc(waits[2:], func(d time.Duration) bool { return d >= 2*time.Second }) {
		t.Errorf("waits = %v, want 1s and 2s before the retries, then shorter ones", waits)
	}
}

func TestDiscoverReportsPersistentThrottlingAndDenial(t *testing.T) {
	p, cloud := newFakeProvider(t, Options{})
	cloud.Fail(gcpfake.Throttle)
	if _, err := p.Discover(context.Background(), contractTarget()); !errors.Is(err, inventory.ErrThrottled) {
		t.Errorf("persistent throttling: err = %v, want ErrThrottled", err)
	}
	cloud.Fail(gcpfake.Deny)
	_, err := p.Discover(context.Background(), contractTarget())
	if err == nil || errors.Is(err, inventory.ErrThrottled) {
		t.Errorf("a missing permission: err = %v, want an error that is not ErrThrottled", err)
	}
}

func TestDiscoverRefusesAnotherProvidersIdentity(t *testing.T) {
	p, _ := newFakeProvider(t, Options{})
	target := contractTarget()
	target.Identity = otherIdentity{}
	if _, err := p.Discover(context.Background(), target); err == nil {
		t.Error("a target with another provider's identity was discovered")
	}
}

type otherIdentity struct{}

func (otherIdentity) Own() bool      { return false }
func (otherIdentity) String() string { return "arn:aws:iam::111111111111:role/x" }

func TestIsThrottle(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"403 rateLimitExceeded", &googleapi.Error{Code: http.StatusForbidden,
			Errors: []googleapi.ErrorItem{{Reason: "rateLimitExceeded"}}}, true},
		{"403 userRateLimitExceeded", &googleapi.Error{Code: http.StatusForbidden,
			Errors: []googleapi.ErrorItem{{Reason: "userRateLimitExceeded"}}}, true},
		{"429", &googleapi.Error{Code: http.StatusTooManyRequests}, true},
		{"403 forbidden", &googleapi.Error{Code: http.StatusForbidden, Errors: []googleapi.ErrorItem{{Reason: "forbidden"}}}, false},
		{"404", &googleapi.Error{Code: http.StatusNotFound}, false},
		{"gRPC RESOURCE_EXHAUSTED", status.Error(codes.ResourceExhausted, "quota"), true},
		{"gRPC PERMISSION_DENIED", status.Error(codes.PermissionDenied, "no"), false},
		{"nil", nil, false},
	} {
		if got := isThrottle(tc.err); got != tc.want {
			t.Errorf("%s: isThrottle = %v, want %v", tc.name, got, tc.want)
		}
		if tc.err == nil {
			continue
		}
		if ae, ok := apierror.FromError(tc.err); ok {
			if got := isThrottle(ae); got != tc.want {
				t.Errorf("%s wrapped in apierror: isThrottle = %v, want %v", tc.name, got, tc.want)
			}
		}
	}
}
