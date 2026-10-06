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
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"

	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// A location that does not exist (#119). Resource Manager has no location filter: discovery
// lists a subscription's virtual networks and keeps those in the target's location, so a
// location nobody has, westeuropa, used to sync cleanly with nothing in it. Admission cannot
// tell: it checks the shape of a name and calls no cloud.
//
// A target whose listing has no virtual network in its location, and only such a target, has
// its location looked up in the subscription's own list of locations (the Subscriptions API's
// List Locations, one call that answers for every location). A name the list does not have
// fails the target and names the closest ones it has; a name it has is a location that is
// empty, which is nothing to report. A location with a virtual network in the listing is real
// whatever any list says, and costs no call.
//
// The call needs Microsoft.Resources/subscriptions/locations/read, which the reader role holds
// from 3.2 on. An identity without it (a role made from the 3.1 file, a role assigned on
// resource groups only) gets a 403: that is not a failure of the target, whose virtual networks
// were just read, but a warning that its location could not be checked.

const (
	// locationsAPIVersion is the version of the Subscriptions API the list is read with.
	locationsAPIVersion = "2022-12-01"
	// locationsOperation names the call for the throttling metrics.
	locationsOperation = "subscriptions.listLocations"
	// locationsValid is how long a list answers for the names it has. Locations are added a few
	// times a year and never renamed.
	locationsValid = 24 * time.Hour
	// locationsRecheck is how old a list may be when it is taken to say a name does not exist,
	// and how long a list that could not be read is left alone: a location added since, or a
	// role fixed since, is seen within the hour, and a misspelt location costs one call an hour.
	locationsRecheck = time.Hour
	// maxLocationCandidates is how many similar names an unknown location is answered with.
	maxLocationCandidates = 3
)

// UnknownLocationError is a target whose location the subscription does not have.
type UnknownLocationError struct {
	Subscription string
	Location     string
	// Candidates are the subscription's locations spelt most like it, the closest first.
	Candidates []string
}

func (e *UnknownLocationError) Error() string {
	msg := fmt.Sprintf("location %q does not exist in subscription %s", e.Location, e.Subscription)
	if len(e.Candidates) == 0 {
		return msg + ", and no location has a name like it (az account list-locations -o table has the names)"
	}
	return msg + ": did you mean " + oneOf(e.Candidates) + "?"
}

// oneOf lists names as "a", "a or b", "a, b or c".
func oneOf(names []string) string {
	if len(names) == 1 {
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
}

// locationList is a subscription's locations as one identity read them, or why it could not.
type locationList struct {
	// done is closed once the read has ended; until then the fields below are not set.
	done  chan struct{}
	names map[string]bool
	err   error
	read  time.Time
	// dropped is a read that says nothing to anybody else: it was throttled, or its caller gave
	// up.
	dropped bool
}

// answers reports whether the list can be asked about the location now: a list that could not
// be read for locationsRecheck, one that has the name for locationsValid, and one that lacks it
// only while it is fresh.
func (l *locationList) answers(location string, now time.Time) bool {
	age := now.Sub(l.read)
	if l.err == nil && l.names[location] {
		return age < locationsValid
	}
	return age < locationsRecheck
}

// checkLocation looks the target's location up in the subscription's locations. It returns an
// UnknownLocationError for a name the subscription does not have, a throttling error when the
// list could not be read for that, and otherwise no error: with a warning when the list could
// not be read, which leaves the name unchecked.
func (d *Discoverer) checkLocation(ctx context.Context, id Identity, target inventory.Target) (string, error) {
	location := normalLocation(target.Region)
	list, err := d.locations(ctx, id, target, location)
	switch {
	case err != nil:
		return "", err
	case list.err != nil:
		return uncheckedWarning(id, target, list.err), nil
	case list.names[location]:
		return "", nil
	}
	return "", &UnknownLocationError{Subscription: target.Account, Location: target.Region,
		Candidates: closestLocations(location, list.names)}
}

// uncheckedWarning says that, and why, the target's location was not checked.
func uncheckedWarning(id Identity, target inventory.Target, err error) string {
	why := err.Error()
	fix := ""
	if respErr, ok := errors.AsType[*azcore.ResponseError](err); ok {
		// ARM's own message is long and names the principal by its object ID.
		why = fmt.Sprintf("HTTP %d %s", respErr.StatusCode, respErr.ErrorCode)
		if respErr.StatusCode == http.StatusForbidden {
			fix = "; the read identity needs Microsoft.Resources/subscriptions/locations/read on the subscription, " +
				"which the reader role of deploy/azure holds from 3.2 on"
		}
	}
	return fmt.Sprintf("location names are not checked in subscription %s: its locations could not be read as %s (%s)%s",
		target.Account, id, why, fix)
}

// locations returns the subscription's locations as the identity reads them, from the list kept
// for it when that still answers for the location, and otherwise from ARM: one call, however
// many targets ask at the same moment. The error returned is throttling or the caller's own
// context; any other failure of the read is the list's.
func (d *Discoverer) locations(ctx context.Context, id Identity, target inventory.Target, location string) (
	*locationList, error) {
	key := clientKey{identity: id, subscription: strings.ToLower(target.Account)}
	for {
		d.mu.Lock()
		list := d.locationLists[key]
		if list != nil {
			select {
			case <-list.done:
				if !list.dropped && list.answers(location, d.now()) {
					d.mu.Unlock()
					return list, nil
				}
				list = nil
			default:
			}
		}
		if list == nil {
			list = &locationList{done: make(chan struct{})}
			d.locationLists[key] = list
			d.mu.Unlock()
			names, err := d.listLocations(ctx, id, target)
			d.mu.Lock()
			list.names, list.err, list.read = names, err, d.now()
			if err != nil && (isThrottle(err) || ctx.Err() != nil) {
				// Neither says anything about the subscription's locations.
				list.dropped = true
				if d.locationLists[key] == list {
					delete(d.locationLists, key)
				}
			}
			d.mu.Unlock()
			close(list.done)
			if list.dropped {
				return nil, err
			}
			return list, nil
		}
		d.mu.Unlock()
		select {
		case <-list.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if list.dropped && list.err != nil && isThrottle(list.err) {
			// Throttled a moment ago: asking again at once is what the subscription cannot take.
			return nil, list.err
		}
	}
}

// listLocations reads the names of the subscription's locations, in lowercase.
func (d *Discoverer) listLocations(ctx context.Context, id Identity, target inventory.Target) (map[string]bool, error) {
	cred, err := d.credentialFor(id)
	if err != nil {
		return nil, err
	}
	client, err := arm.NewClient("subnet-operator/locations", "v1.0.0", cred, d.clientOptions())
	if err != nil {
		return nil, err
	}
	var names map[string]bool
	err = d.call(ctx, target, armBucket(id, target.Account), locationsOperation,
		func(ctx context.Context) (*http.Response, error) {
			names = map[string]bool{}
			next := runtime.JoinPaths(client.Endpoint(), "subscriptions", url.PathEscape(target.Account), "locations") +
				"?api-version=" + locationsAPIVersion
			var resp *http.Response
			for next != "" {
				req, err := runtime.NewRequest(ctx, http.MethodGet, next)
				if err != nil {
					return resp, err
				}
				req.Raw().Header.Set("Accept", "application/json")
				resp, err = client.Pipeline().Do(req)
				if err != nil {
					return resp, err
				}
				if !runtime.HasStatusCode(resp, http.StatusOK) {
					return resp, runtime.NewResponseError(resp)
				}
				var page struct {
					Value []struct {
						Name string `json:"name"`
					} `json:"value"`
					NextLink string `json:"nextLink"`
				}
				if err := runtime.UnmarshalAsJSON(resp, &page); err != nil {
					return resp, err
				}
				for _, l := range page.Value {
					if l.Name != "" {
						names[normalLocation(l.Name)] = true
					}
				}
				next = page.NextLink
			}
			return resp, nil
		})
	if err != nil {
		return nil, fmt.Errorf("list locations: %w", err)
	}
	return names, nil
}

// closestLocations are the names most like the location: those within a few edits of it, and
// those it is the start of, the closest first and at most maxLocationCandidates.
func closestLocations(location string, names map[string]bool) []string {
	type candidate struct {
		name     string
		distance int
	}
	// A third of the name may be wrong, at least two letters: westeuropa is westeurope, and
	// nothing is anything like a name of two letters.
	limit := max(2, len(location)/3)
	var found []candidate
	for name := range names {
		distance := editDistance(location, name)
		if distance > limit && (len(location) < 4 || !strings.HasPrefix(name, location)) {
			continue
		}
		found = append(found, candidate{name, distance})
	}
	slices.SortFunc(found, func(a, b candidate) int {
		return cmp.Or(cmp.Compare(a.distance, b.distance), cmp.Compare(a.name, b.name))
	})
	out := make([]string, 0, maxLocationCandidates)
	for _, c := range found[:min(len(found), maxLocationCandidates)] {
		out = append(out, c.name)
	}
	return out
}

// editDistance is the Levenshtein distance between two names: the letters to add, drop or
// change to make one the other.
func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			change := prev[j-1]
			if a[i-1] != b[j-1] {
				change++
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, change)
		}
		prev = cur
	}
	return prev[len(b)]
}
