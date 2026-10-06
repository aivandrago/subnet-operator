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

package deploy

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"hypersurgery.dev/subnet-operator/internal/cloud/azure/azurefake"
	azureevents "hypersurgery.dev/subnet-operator/internal/cloud/azure/events"
)

// The Bicep templates of the Azure change events in deploy/azure/events, checked like the
// roles: the operator may only receive and delete messages, the queue takes no keys, and the
// event subscription delivers what the operator's parser acts on and nothing it would ignore.

func loadBicep(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "deploy", "azure", "events", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// The built-in roles by their IDs, which never change.
const (
	queueDataContributor      = "974c5e8b-45b9-4653-ba55-5f855dd0fb88"
	queueDataMessageProcessor = "8a0f0c08-91a1-4084-bc3d-661d67233fed"
	queueDataMessageSender    = "c6a89b2d-59bc-44d0-9896-0f6e12d7b80a"
)

var roleID = regexp.MustCompile(`'([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})'`)

func rolesIn(bicep string) []string {
	matches := roleID.FindAllStringSubmatch(bicep, -1)
	ids := make([]string, 0, len(matches))
	for _, m := range matches {
		ids = append(ids, m[1])
	}
	return ids
}

func TestAzureEventsQueueGrantsTheOperatorOnlyToProcessMessages(t *testing.T) {
	queue := loadBicep(t, "queue.bicep")
	if got := rolesIn(queue); !slices.Equal(got, []string{queueDataMessageProcessor}) {
		t.Errorf("roles assigned by queue.bicep = %v, want only Storage Queue Data Message Processor (%s), not "+
			"Contributor (%s)", got, queueDataMessageProcessor, queueDataContributor)
	}
	if !strings.Contains(queue, "scope: queue\n") {
		t.Error("the operator's role is not assigned on the queue itself")
	}
	if !strings.Contains(queue, "allowSharedKeyAccess: false") {
		t.Error("the storage account allows account keys and SAS tokens")
	}
	sender := loadBicep(t, "sender-role.bicep")
	if got := rolesIn(sender); !slices.Equal(got, []string{queueDataMessageSender}) {
		t.Errorf("roles assigned by sender-role.bicep = %v, want only Storage Queue Data Message Sender", got)
	}
	if !strings.Contains(sender, "scope: queue\n") {
		t.Error("the topic's role is not assigned on the queue itself")
	}
}

// What the event subscription lets through is what the operator resyncs for: every included
// event type about a virtual network is a change to the parser, and the subject filter is the
// resource type the parser matches.
func TestAzureEventSubscriptionDeliversWhatTheOperatorReads(t *testing.T) {
	topic := loadBicep(t, "topic.bicep")
	types := regexp.MustCompile(`'(Microsoft\.Resources\.Resource[A-Za-z]+)'`).FindAllStringSubmatch(topic, -1)
	if len(types) != 2 {
		t.Fatalf("included event types = %v, want the write and the delete success", types)
	}
	const network = "/subscriptions/5b3c2a10-0000-4000-8000-00000000a2e1/resourceGroups/rg/providers/" +
		"Microsoft.Network/virtualNetworks/hub"
	for _, m := range types {
		changes, err := azureevents.Parse(string(azurefake.ResourceEvent(m[1], network+"/subnets/a")))
		if err != nil || len(changes) != 1 {
			t.Errorf("%s is delivered but not a change to the operator: %v, %v", m[1], changes, err)
		}
	}
	const filter = "/providers/Microsoft.Network/virtualNetworks/"
	if !strings.Contains(topic, "'"+filter+"'") || !strings.Contains(topic, "key: 'subject'") ||
		!strings.Contains(topic, "operatorType: 'StringContains'") {
		t.Errorf("topic.bicep does not filter subjects containing %s", filter)
	}
	if !strings.Contains(network+"/subnets/a", filter) {
		t.Error("the filter would not pass a subnet's subject")
	}
	for _, want := range []string{"deliveryWithResourceIdentity", "endpointType: 'StorageQueue'",
		"topicType: 'Microsoft.Resources.Subscriptions'", "eventDeliverySchema: 'EventGridSchema'"} {
		if !strings.Contains(topic, want) {
			t.Errorf("topic.bicep has no %s", want)
		}
	}
}

// With resourceGroupNames the event subscription also filters on the resource group, for a scope
// that discovers some resource groups only (spec.azure.resourceGroups). The prefix it filters on
// is the one a virtual network's subject begins with, it is cut at the group's name so that one
// group does not let through another whose name starts the same, and without the parameter
// every group is delivered, as before. Event Grid matches without regard to case; the parser
// reads the subjects that lets through, however Azure spells them.
func TestAzureEventSubscriptionCanBeLimitedToResourceGroups(t *testing.T) {
	topic := loadBicep(t, "topic.bicep")
	for _, want := range []string{"param resourceGroupNames array = []", "operatorType: 'StringBeginsWith'",
		"empty(resourceGroupNames)", "values: resourceGroupPrefixes"} {
		if !strings.Contains(topic, want) {
			t.Fatalf("topic.bicep has no %s", want)
		}
	}
	format := regexp.MustCompile(`for name in resourceGroupNames: '\$\{subscription\(\)\.id\}([^$']*)\$\{name\}([^']*)'`).
		FindStringSubmatch(topic)
	if format == nil {
		t.Fatal("topic.bicep does not build the resource group prefixes from the subscription's ID and the names")
	}
	const subscription = "/subscriptions/5b3c2a10-0000-4000-8000-00000000a2e1"
	prefix := func(group string) string { return subscription + format[1] + group + format[2] }
	// passes is Event Grid's StringBeginsWith: without regard to case.
	passes := func(subject, group string) bool {
		return strings.HasPrefix(strings.ToLower(subject), strings.ToLower(prefix(group)))
	}
	for _, subject := range []string{
		subscription + "/resourceGroups/rg-network/providers/Microsoft.Network/virtualNetworks/hub",
		subscription + "/resourceGroups/rg-network/providers/Microsoft.Network/virtualNetworks/hub/subnets/a",
		// ARM does not spell the same ID the same way everywhere.
		subscription + "/resourcegroups/RG-Network/providers/microsoft.network/virtualnetworks/hub/subnets/a",
	} {
		if !passes(subject, "rg-network") {
			t.Errorf("the filter for rg-network (%s) would not pass %s", prefix("rg-network"), subject)
		}
		if passes(subject, "rg-net") {
			t.Errorf("the filter for rg-net (%s) passes %s, which is in rg-network", prefix("rg-net"), subject)
		}
		changes, err := azureevents.Parse(string(azurefake.ResourceEvent(
			"Microsoft.Resources.ResourceWriteSuccess", subject)))
		if err != nil || len(changes) != 1 {
			t.Errorf("%s passes the filter but is not a change to the operator: %v, %v", subject, changes, err)
		}
	}
}
