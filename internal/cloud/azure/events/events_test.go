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

package events

import (
	"encoding/base64"
	"strings"
	"testing"

	"hypersurgery.dev/subnet-operator/internal/cloud/azure/azurefake"
)

const (
	subscription = "5b3c2a10-0000-4000-8000-00000000a2e1"
	network      = "/subscriptions/" + subscription + "/resourceGroups/rg-net/providers/Microsoft.Network/virtualNetworks/hub"
	// canonical is how the inventory spells the network: in lowercase.
	canonical = "/subscriptions/" + subscription + "/resourcegroups/rg-net/providers/microsoft.network/virtualnetworks/hub"
)

func parseOne(t *testing.T, text string) Change {
	t.Helper()
	changes, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 {
		t.Fatalf("changes = %+v, want one", changes)
	}
	return changes[0]
}

// A write to a virtual network, to a subnet of it, to a peering or to its tags (where the
// ownership of its subnets lives) all name the virtual network; only its own delete says it is
// gone.
func TestEventsNameTheirVirtualNetwork(t *testing.T) {
	for name, tc := range map[string]struct {
		eventType, resource string
		deleted             bool
	}{
		"network written": {writeSuccess, network, false},
		"network deleted": {deleteSuccess, network, true},
		"subnet written":  {writeSuccess, network + "/subnets/payments", false},
		"subnet deleted":  {deleteSuccess, network + "/subnets/payments", false},
		"tags written":    {writeSuccess, network + "/providers/Microsoft.Resources/tags/default", false},
		"peering written": {writeSuccess, network + "/virtualNetworkPeerings/to-spoke", false},
		"event type case": {strings.ToUpper(writeSuccess), network, false},
		"trailing slash":  {deleteSuccess, network + "/", true},
	} {
		t.Run(name, func(t *testing.T) {
			got := parseOne(t, string(azurefake.ResourceEvent(tc.eventType, tc.resource)))
			want := Change{Subscription: subscription, NetworkID: canonical, NetworkDeleted: tc.deleted}
			if got != want {
				t.Errorf("got %+v, want %+v", got, want)
			}
		})
	}
}

// ARM IDs are case-insensitive and Event Grid spells them both ways (resourceGroups in one
// event, resourcegroups in the next): every spelling is the same network.
func TestResourceIDsMatchInAnyCase(t *testing.T) {
	for _, id := range []string{
		network,
		strings.ToLower(network),
		strings.ToUpper(network),
		"/subscriptions/" + strings.ToUpper(subscription) + "/resourcegroups/RG-Net/providers/microsoft.network/" +
			"VirtualNetworks/Hub/Subnets/Payments",
	} {
		if got := parseOne(t, string(azurefake.ResourceEvent(writeSuccess, id))); got.NetworkID != canonical ||
			got.Subscription != subscription {
			t.Errorf("%s: got %+v", id, got)
		}
	}
}

// What Event Grid delivers, in each form: its own schema or CloudEvents, one event or an array,
// as JSON or as the Base64 a Storage queue message carries.
func TestEverySchemaAndEncodingIsRead(t *testing.T) {
	grid := string(azurefake.ResourceEvent(writeSuccess, network))
	cloud := string(azurefake.CloudEvent(writeSuccess, network))
	for name, text := range map[string]string{
		"event grid schema": grid,
		"cloudevents":       cloud,
		"base64":            base64.StdEncoding.EncodeToString([]byte(grid)),
		"array":             "[" + grid + "]",
		"padded":            "  " + grid + "\n",
		"without a subject, from data.resourceUri": strings.Replace(grid, `"subject":"`+network+`"`, `"subject":""`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if got := parseOne(t, text); got.NetworkID != canonical {
				t.Errorf("got %+v", got)
			}
		})
	}
	changes, err := Parse("[" + grid + "," + cloud + "]")
	if err != nil || len(changes) != 2 {
		t.Errorf("an array of two: %+v, %v", changes, err)
	}
}

// Events that change nothing the inventory shows are read and reported as nothing: failed and
// cancelled writes, actions (a NIC joining a subnet), and every other resource type.
func TestEventsThatDoNotAffectTheInventory(t *testing.T) {
	rg := "/subscriptions/" + subscription + "/resourceGroups/rg-net"
	for name, event := range map[string][]byte{
		"write failed":    azurefake.ResourceEvent("Microsoft.Resources.ResourceWriteFailure", network),
		"write cancelled": azurefake.ResourceEvent("Microsoft.Resources.ResourceWriteCancel", network),
		"delete failed":   azurefake.ResourceEvent("Microsoft.Resources.ResourceDeleteFailure", network),
		"action":          azurefake.ResourceEvent("Microsoft.Resources.ResourceActionSuccess", network+"/subnets/a"),
		"virtual machine": azurefake.ResourceEvent(writeSuccess, rg+"/providers/Microsoft.Compute/virtualMachines/vm-1"),
		"network interface": azurefake.ResourceEvent(writeSuccess,
			rg+"/providers/Microsoft.Network/networkInterfaces/nic-1"),
		"resource group": azurefake.ResourceEvent(writeSuccess, rg),
		"a name that only looks like it": azurefake.ResourceEvent(writeSuccess,
			rg+"/providers/Microsoft.Storage/storageAccounts/Microsoft.Network/virtualNetworks/hub"),
		"another event source": []byte(`{"eventType":"Microsoft.Storage.BlobCreated","subject":"/blobServices/default"}`),
	} {
		t.Run(name, func(t *testing.T) {
			changes, err := Parse(string(event))
			if err != nil || len(changes) != 0 {
				t.Errorf("changes = %+v, err = %v, want neither", changes, err)
			}
		})
	}
}

// A message is untrusted: what is no resource event, or contradicts itself, is an error (and
// dropped by the poller), never a panic and never a resync.
func TestMalformedMessagesAreErrors(t *testing.T) {
	other := "/subscriptions/99999999-0000-4000-8000-000000000000"
	grid := string(azurefake.ResourceEvent(writeSuccess, network))
	for name, text := range map[string]string{
		"empty":                       "",
		"blank":                       "  \n",
		"garbage":                     "garbage!",
		"truncated json":              grid[:len(grid)/2],
		"base64 of garbage":           base64.StdEncoding.EncodeToString([]byte("garbage")),
		"a json string":               `"` + network + `"`,
		"an empty array":              "[]",
		"an array of numbers":         "[1, 2]",
		"no event type":               `{"subject":"` + network + `"}`,
		"no resource":                 `{"eventType":"` + writeSuccess + `"}`,
		"a subject of the wrong type": `{"eventType":"` + writeSuccess + `","subject":{"a":1}}`,
		"deeply nested":               strings.Repeat("[", 20000) + strings.Repeat("]", 20000),
		"the topic of another subscription": strings.Replace(grid, `"topic":"/subscriptions/`+subscription+`"`,
			`"topic":"`+other+`"`, 1),
		"the data of another subscription": strings.Replace(grid, `"subscriptionId":"`+subscription+`"`,
			`"subscriptionId":"99999999-0000-4000-8000-000000000000"`, 1),
		"oversized": grid + strings.Repeat(" ", MaxMessageBytes),
	} {
		t.Run(name, func(t *testing.T) {
			changes, err := Parse(text)
			if err == nil {
				t.Errorf("changes = %+v, want an error", changes)
			}
		})
	}
	// A resource group's topic is a scope below the subscription, and as good as it.
	scoped := strings.Replace(grid, `"topic":"/subscriptions/`+subscription+`"`,
		`"topic":"/subscriptions/`+subscription+`/resourceGroups/rg-net"`, 1)
	if _, err := Parse(scoped); err != nil {
		t.Errorf("a resource group's topic: %v", err)
	}
}
