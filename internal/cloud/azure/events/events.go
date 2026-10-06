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

// Package events is the Azure provider's change-event source, the counterpart of the AWS
// EventBridge → SQS path and of the GCP audit logs → Pub/Sub path: the resource events of a
// subscription's Event Grid system topic (Microsoft.Resources.ResourceWriteSuccess and
// ResourceDeleteSuccess), filtered to virtual networks and delivered to a Storage queue the
// operator polls, are turned into subscription/location pairs that need a resync.
package events

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
)

// MaxMessageBytes is the largest message text the source reads. A Storage queue holds
// messages of up to 64 KiB, so nothing Event Grid delivered is larger; anything that is did
// not come from there, and is dropped unread rather than decoded.
const MaxMessageBytes = 64 * 1024

// The event types that report a change: a create or update, or a delete, that succeeded. The
// failures and cancellations changed nothing, and the action events (ResourceAction*) are
// calls such as a NIC joining a subnet, which the inventory does not show: free IP counts
// follow the periodic resync, as on AWS and GCP.
const (
	writeSuccess  = "Microsoft.Resources.ResourceWriteSuccess"
	deleteSuccess = "Microsoft.Resources.ResourceDeleteSuccess"
)

// event is the part of a resource event the operator reads, in either schema Event Grid
// delivers: its own (eventType, topic) or CloudEvents 1.0 (type, source). encoding/json
// matches the names without regard to case.
type event struct {
	Subject   string `json:"subject"`
	EventType string `json:"eventType"`
	Type      string `json:"type"`
	Topic     string `json:"topic"`
	Source    string `json:"source"`
	Data      *struct {
		ResourceURI    string `json:"resourceUri"`
		SubscriptionID string `json:"subscriptionId"`
	} `json:"data"`
}

// Change is a virtual network an event says was written or deleted, itself or something
// below it: a subnet, a peering, its tags (where the ownership of its subnets lives).
type Change struct {
	// Subscription is the subscription ID as the operator spells it (CanonicalAccountID).
	Subscription string
	// NetworkID is the virtual network's resource ID as the inventory spells it
	// (CanonicalResourceID), whatever the event's own case.
	NetworkID string
	// NetworkDeleted is set when the virtual network itself was deleted.
	NetworkDeleted bool
}

// networkID matches the resource ID of a virtual network, or of anything below one. ARM IDs
// are case-insensitive and Event Grid does not spell them alike in every event (resourceGroups
// or resourcegroups, the provider namespace in either case), so the match is too.
var networkID = regexp.MustCompile(`(?i)^(/subscriptions/([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})` +
	`/resourceGroups/[^/]+/providers/Microsoft\.Network/virtualNetworks/[^/]+)(/.*)?$`)

// Parse reads the text of one queue message: one event, or an array of them, as JSON or as
// the Base64 of it, which is how Event Grid writes to a Storage queue. It returns the virtual
// networks the events say changed, none for events that do not affect the inventory (other
// event types, other resource types), and an error for a message that is no resource event or
// contradicts itself. A message is untrusted input: nothing of it is used but the resource ID,
// and that only after it matched.
func Parse(text string) ([]Change, error) {
	if len(text) > MaxMessageBytes {
		return nil, fmt.Errorf("message of %d bytes is larger than the %d a Storage queue holds", len(text),
			MaxMessageBytes)
	}
	body := bytes.TrimSpace([]byte(text))
	if len(body) == 0 {
		return nil, errors.New("empty message")
	}
	if body[0] != '{' && body[0] != '[' {
		decoded, err := base64.StdEncoding.DecodeString(string(body))
		if err != nil {
			return nil, errors.New("message is neither JSON nor Base64")
		}
		body = bytes.TrimSpace(decoded)
	}
	var events []event
	if len(body) > 0 && body[0] == '[' {
		if err := json.Unmarshal(body, &events); err != nil {
			return nil, fmt.Errorf("decode events: %w", err)
		}
	} else {
		var e event
		if err := json.Unmarshal(body, &e); err != nil {
			return nil, fmt.Errorf("decode event: %w", err)
		}
		events = []event{e}
	}
	if len(events) == 0 {
		return nil, errors.New("message holds no event")
	}
	var changes []Change
	for _, e := range events {
		c, ok, err := changeOf(e)
		if err != nil {
			return nil, err
		}
		if ok {
			changes = append(changes, c)
		}
	}
	return changes, nil
}

// changeOf maps one event to the virtual network it is about.
func changeOf(e event) (Change, bool, error) {
	eventType := firstNonEmpty(e.EventType, e.Type)
	if eventType == "" {
		return Change{}, false, errors.New("not an Event Grid event: it has no event type")
	}
	deleted := strings.EqualFold(eventType, deleteSuccess)
	if !deleted && !strings.EqualFold(eventType, writeSuccess) {
		return Change{}, false, nil
	}
	// The subject is the resource ID of the operation's target, and data.resourceUri repeats it.
	id := e.Subject
	if id == "" && e.Data != nil {
		id = e.Data.ResourceURI
	}
	if id == "" {
		return Change{}, false, fmt.Errorf("%s names no resource", eventType)
	}
	m := networkID.FindStringSubmatch(id)
	if m == nil {
		return Change{}, false, nil // a virtual machine, a storage account, ...
	}
	subscription := networkv1.CanonicalAccountID(networkv1.ProviderAzure, m[2])
	// The topic of a subscription's system topic is the subscription. An event that claims a
	// resource of another one was not written by Event Grid, or was routed by a subscription
	// nobody meant to have; either way it is not believed.
	scope := "/subscriptions/" + subscription
	if source := strings.ToLower(firstNonEmpty(e.Topic, e.Source)); source != "" && source != scope &&
		!strings.HasPrefix(source, scope+"/") {
		return Change{}, false, fmt.Errorf("event of %q names a resource of subscription %s", source, subscription)
	}
	if e.Data != nil && e.Data.SubscriptionID != "" && !strings.EqualFold(e.Data.SubscriptionID, subscription) {
		return Change{}, false, fmt.Errorf("event of subscription %q names a resource of subscription %s",
			e.Data.SubscriptionID, subscription)
	}
	return Change{Subscription: subscription, NetworkID: networkv1.CanonicalResourceID(networkv1.ProviderAzure, m[1]),
		NetworkDeleted: deleted && strings.Trim(m[3], "/") == ""}, true, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
