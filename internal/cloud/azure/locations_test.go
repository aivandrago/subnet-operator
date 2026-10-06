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
	"slices"
	"strings"
	"testing"
	"time"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	"hypersurgery.dev/subnet-operator/internal/cloud/azure/azurefake"
	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// A location that does not exist, and one that is only empty (#119).

// locationsFake is a subscription with one virtual network, in westeurope, and a clock the test
// moves.
func locationsFake(t *testing.T) (*Provider, *azurefake.Cloud, *time.Time) {
	t.Helper()
	p, cloud := newFakeProvider(t, Options{})
	cloud.AddVirtualNetwork(contractSubscription, testGroup, "hub", "westeurope", []string{"10.0.0.0/16"}, nil)
	cloud.AddSubnet(contractSubscription, testGroup, "hub", "apps", "10.0.1.0/24")
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	p.discoverer.now = func() time.Time { return now }
	cloud.Calls()
	return p, cloud, &now
}

func in(location string) inventory.Target {
	return targetsIn(location)[0]
}

// A misspelt location fails its target and names the locations it could have been; the other
// locations of the subscription are read as ever.
func TestAnUnknownLocationFailsItsTargetAndNamesTheClosest(t *testing.T) {
	p, cloud, _ := locationsFake(t)
	results := syncTargets(p, targetsIn("westeurope", "westeuropa"), nil)
	if results[0].err != nil || len(results[0].snapshot.Networks) != 1 {
		t.Errorf("westeurope next to a misspelt location: %+v, %v", results[0].snapshot, results[0].err)
	}
	err := results[1].err
	unknown, ok := errors.AsType[*UnknownLocationError](err)
	if !ok {
		t.Fatalf("westeuropa: %+v, %v; want an UnknownLocationError", results[1].snapshot, err)
	}
	if unknown.Location != "westeuropa" || unknown.Subscription != contractSubscription ||
		!slices.Equal(unknown.Candidates, []string{"westeurope"}) {
		t.Errorf("unknown location = %+v, want westeuropa with the candidate westeurope", unknown)
	}
	want := `location "westeuropa" does not exist in subscription ` + contractSubscription + `: did you mean westeurope?`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err, want)
	}
	if errors.Is(err, inventory.ErrThrottled) {
		t.Error("an unknown location is reported as throttling")
	}
	if calls := cloud.Calls(); calls[azurefake.ListLocations] != 1 {
		t.Errorf("%d locations requests, want 1", calls[azurefake.ListLocations])
	}

	// Several candidates, the closest first, and none at all.
	_, err = p.Discover(context.Background(), in("westus4"))
	if unknown, _ := errors.AsType[*UnknownLocationError](err); unknown == nil ||
		!slices.Equal(unknown.Candidates, []string{"westus", "westus2", "westus3"}) ||
		!strings.HasSuffix(err.Error(), ": did you mean westus, westus2 or westus3?") {
		t.Errorf("westus4: %v", err)
	}
	_, err = p.Discover(context.Background(), in("atlantis"))
	if unknown, _ := errors.AsType[*UnknownLocationError](err); unknown == nil || len(unknown.Candidates) != 0 ||
		!strings.Contains(err.Error(), "no location has a name like it") {
		t.Errorf("atlantis: %v", err)
	}
	// The list that said so is asked again without a call, while it is fresh.
	if calls := cloud.Calls(); calls[azurefake.ListLocations] != 0 {
		t.Errorf("%d more locations requests within the hour, want none", calls[azurefake.ListLocations])
	}

	// As an identity of the subscription's own, the error is the scope's all the same, and
	// does not start with the identity.
	const reader = "44444444-dddd-4000-8000-0000000000ad"
	cloud.AddIdentity(azurefake.DefaultTenant, reader, true)
	asReader := in("westeuropa")
	asReader.Identity = Identity{TenantID: azurefake.DefaultTenant, ClientID: reader}
	if _, err := p.Discover(context.Background(), asReader); err == nil || err.Error() != want {
		t.Errorf("as an identity: %v, want %q", err, want)
	}
}

// A location the subscription has, with no virtual network in it, syncs with nothing in it and
// nothing to say; the list that said it exists is kept for a day.
func TestAnEmptyLocationThatExistsIsQuiet(t *testing.T) {
	p, cloud, now := locationsFake(t)
	for _, location := range []string{"northeurope", "North Europe", "swedencentral"} {
		snap, err := p.Discover(context.Background(), in(location))
		if err != nil || len(snap.Networks)+len(snap.Subnets)+len(snap.Warnings) != 0 {
			t.Errorf("%s: %+v, %v; want an empty snapshot without warnings", location, snap, err)
		}
	}
	if calls := cloud.Calls(); calls[azurefake.ListLocations] != 1 {
		t.Errorf("%d locations requests for three empty locations, want 1", calls[azurefake.ListLocations])
	}
	*now = now.Add(locationsValid - time.Minute)
	discoverAll(t, p, in("northeurope"))
	if calls := cloud.Calls(); calls[azurefake.ListLocations] != 0 {
		t.Errorf("%d locations requests within the day, want the list kept", calls[azurefake.ListLocations])
	}
	*now = now.Add(2 * time.Minute)
	discoverAll(t, p, in("northeurope"))
	if calls := cloud.Calls(); calls[azurefake.ListLocations] != 1 {
		t.Errorf("%d locations requests after a day, want the list read again", calls[azurefake.ListLocations])
	}

	// With resource groups in the scope, an empty location is as quiet, and a misspelt one is
	// as wrong.
	grouped := in("northeurope")
	grouped.Azure = &networkv1.AzureScope{ResourceGroups: []string{testGroup}}
	if snap, err := p.Discover(context.Background(), grouped); err != nil || len(snap.Warnings) != 0 {
		t.Errorf("an empty location of a scope with resource groups: %+v, %v", snap, err)
	}
	grouped.Region = "northeuropa"
	if _, err := p.Discover(context.Background(), grouped); !isUnknownLocation(err) {
		t.Errorf("a misspelt location of a scope with resource groups: %v", err)
	}
}

// A location with a virtual network in it is never looked up: it is real whatever the list
// says, and costs no call.
func TestALocationWithNetworksIsNotLookedUp(t *testing.T) {
	p, cloud, _ := locationsFake(t)
	cloud.SetLocations(contractSubscription, "eastus")
	cloud.FailEndpoint(azurefake.ListLocations, azurefake.Deny)
	snap := discoverAll(t, p, in("westeurope"))
	if len(snap.Networks) != 1 || len(snap.Warnings) != 0 {
		t.Errorf("westeurope = %+v, want its network and no warning", snap)
	}
	if calls := cloud.Calls(); calls[azurefake.ListLocations] != 0 {
		t.Errorf("%d locations requests for a location with a network", calls[azurefake.ListLocations])
	}
}

// An identity that may not read the locations (the reader role of 3.1) still syncs its targets:
// the names are not checked, which the snapshot says, and the refusal is left alone for an hour.
func TestARefusedLocationsCallIsAWarningNotAFailure(t *testing.T) {
	p, cloud, now := locationsFake(t)
	cloud.FailEndpoint(azurefake.ListLocations, azurefake.Deny)
	warnings := make([]string, 0, 2)
	for _, location := range []string{"northeurope", "westeuropa"} {
		snap, err := p.Discover(context.Background(), in(location))
		if err != nil || len(snap.Networks) != 0 || len(snap.Warnings) != 1 {
			t.Fatalf("%s: %+v, %v; want an empty snapshot with one warning", location, snap, err)
		}
		warnings = append(warnings, snap.Warnings[0])
	}
	want := "location names are not checked in subscription " + contractSubscription + ": its locations could not be " +
		"read as the operator's own credentials (HTTP 403 AuthorizationFailed); the read identity needs " +
		"Microsoft.Resources/subscriptions/locations/read on the subscription, which the reader role of deploy/azure " +
		"holds from 3.2 on"
	if warnings[0] != want || warnings[1] != want {
		t.Errorf("warnings = %q, want %q for both", warnings, want)
	}
	if calls := cloud.Calls(); calls[azurefake.ListLocations] != 1 {
		t.Errorf("%d locations requests, want the one that was refused", calls[azurefake.ListLocations])
	}

	// The role is updated: within the hour nothing is asked again, after it the names are checked.
	cloud.FailEndpoint(azurefake.ListLocations, azurefake.None)
	*now = now.Add(locationsRecheck - time.Minute)
	if snap := discoverAll(t, p, in("westeuropa")); len(snap.Warnings) != 1 {
		t.Errorf("within the hour: %+v, want the warning still", snap)
	}
	*now = now.Add(2 * time.Minute)
	if _, err := p.Discover(context.Background(), in("westeuropa")); !isUnknownLocation(err) {
		t.Errorf("after the hour: %v, want the misspelt location reported", err)
	}
	if snap := discoverAll(t, p, in("northeurope")); len(snap.Warnings) != 0 {
		t.Errorf("after the hour: %+v, want no warning", snap)
	}
	if calls := cloud.Calls(); calls[azurefake.ListLocations] != 1 {
		t.Errorf("%d locations requests after the hour, want 1", calls[azurefake.ListLocations])
	}
}

// A name the list lacks is believed missing only while the list is fresh: a location Azure
// added since is found within the hour.
func TestAnUnknownLocationIsLookedUpAgainAfterAnHour(t *testing.T) {
	p, cloud, now := locationsFake(t)
	if _, err := p.Discover(context.Background(), in("austriaeast")); !isUnknownLocation(err) {
		t.Fatalf("austriaeast: %v", err)
	}
	cloud.SetLocations(contractSubscription, append(slices.Clone(azurefake.DefaultLocations), "austriaeast")...)
	*now = now.Add(locationsRecheck - time.Minute)
	if _, err := p.Discover(context.Background(), in("austriaeast")); !isUnknownLocation(err) {
		t.Errorf("within the hour: %v, want the list still believed", err)
	}
	*now = now.Add(2 * time.Minute)
	if snap, err := p.Discover(context.Background(), in("austriaeast")); err != nil || len(snap.Warnings) != 0 {
		t.Errorf("after the hour: %+v, %v; want the new location found", snap, err)
	}
	if calls := cloud.Calls(); calls[azurefake.ListLocations] != 2 {
		t.Errorf("%d locations requests, want one an hour (2)", calls[azurefake.ListLocations])
	}
}

// A throttled locations call is throttling of the target, like any other call, and is not kept.
func TestAThrottledLocationsCallIsThrottling(t *testing.T) {
	p, cloud, _ := locationsFake(t)
	cloud.FailEndpoint(azurefake.ListLocations, azurefake.Throttle)
	if _, err := p.Discover(context.Background(), in("northeurope")); !errors.Is(err, inventory.ErrThrottled) {
		t.Errorf("a throttled locations call: %v, want ErrThrottled", err)
	}
	if calls := cloud.Calls(); calls[azurefake.ListLocations] != retryMaxAttempts {
		t.Errorf("%d locations requests, want %d attempts", calls[azurefake.ListLocations], retryMaxAttempts)
	}
	cloud.FailEndpoint(azurefake.ListLocations, azurefake.None)
	if snap, err := p.Discover(context.Background(), in("northeurope")); err != nil || len(snap.Warnings) != 0 {
		t.Errorf("after the throttling: %+v, %v", snap, err)
	}
}

// Empty locations of a subscription that start at the same moment are checked with one call.
func TestTargetsShareOneLocationsCall(t *testing.T) {
	p, cloud, _ := locationsFake(t)
	release := cloud.Hold(azurefake.ListLocations)
	results := syncTargets(p, targetsIn("northeurope", "swedencentral", "uksouth", "ukzouth"), func() {
		eventually(t, "the locations call", func() bool { return cloud.CallsSoFar()[azurefake.ListLocations] > 0 })
		time.Sleep(100 * time.Millisecond)
		release()
	})
	for i, r := range results[:3] {
		if r.err != nil {
			t.Errorf("target %d: %v", i, r.err)
		}
	}
	if !isUnknownLocation(results[3].err) {
		t.Errorf("ukzouth: %v", results[3].err)
	}
	if calls := cloud.Calls(); calls[azurefake.ListLocations] != 1 || calls[azurefake.ListAll] != 1 {
		t.Errorf("%d locations and %d List All requests for four targets, want 1 and 1",
			calls[azurefake.ListLocations], calls[azurefake.ListAll])
	}
}

func TestClosestLocations(t *testing.T) {
	names := map[string]bool{}
	for _, n := range append(slices.Clone(azurefake.DefaultLocations), "francecentral", "francesouth", "eu") {
		names[n] = true
	}
	for location, want := range map[string][]string{
		"westeuropa":    {"westeurope"},
		"west-europe":   {"westeurope"},
		"europewest":    nil,
		"eastus1":       {"eastus", "eastus2"},
		"france":        {"francesouth", "francecentral"},
		"swedencentral": {"swedencentral"},
		"x":             {"eu"},
		"germanywest":   {"germanywestcentral"},
	} {
		if got := closestLocations(location, names); !slices.Equal(got, want) {
			t.Errorf("closest to %s = %v, want %v", location, got, want)
		}
	}
}
