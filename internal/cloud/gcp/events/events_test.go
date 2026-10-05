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
	"fmt"
	"testing"

	"hypersurgery.dev/subnet-operator/internal/inventory"
)

const (
	project = "shop-host-1"
	number  = "987654321098"
	region  = "europe-west1"
)

// insertSubnetwork is shaped like the entry a log sink publishes when a person creates a
// subnetwork in the console: the completion entry of the operation, which names the resource
// and who asked for it.
const insertSubnetwork = `{
  "protoPayload": {
    "@type": "type.googleapis.com/google.cloud.audit.AuditLog",
    "status": {},
    "authenticationInfo": {"principalEmail": "maria.k@example.com"},
    "requestMetadata": {"callerIp": "203.0.113.7"},
    "serviceName": "compute.googleapis.com",
    "methodName": "v1.compute.subnetworks.insert",
    "resourceName": "projects/shop-host-1/regions/europe-west1/subnetworks/payments",
    "request": {"@type": "type.googleapis.com/compute.subnetworks.insert"}
  },
  "insertId": "-x1y2z3e4",
  "resource": {
    "type": "gce_subnetwork",
    "labels": {"project_id": "shop-host-1", "location": "europe-west1", "subnetwork_id": "4471", "subnetwork_name": "payments"}
  },
  "timestamp": "2026-09-28T09:12:44.113Z",
  "severity": "NOTICE",
  "logName": "projects/shop-host-1/logs/cloudaudit.googleapis.com%2Factivity",
  "operation": {"id": "operation-1727514764113-abc", "producer": "compute.googleapis.com", "last": true},
  "receiveTimestamp": "2026-09-28T09:12:45.001Z"
}`

// entry builds an audit log entry with the given payload fields.
func entry(service, method, resourceName, extra string) string {
	return fmt.Sprintf(`{"protoPayload":{"@type":"type.googleapis.com/google.cloud.audit.AuditLog",`+
		`"serviceName":%q,"methodName":%q,"resourceName":%q%s},`+
		`"logName":"projects/shop-host-1/logs/cloudaudit.googleapis.com%%2Factivity"}`,
		service, method, resourceName, extra)
}

func compute(method, resourceName string) string {
	return entry("compute.googleapis.com", method, resourceName, "")
}

// tagBinding is a Resource Manager tag binding change on the resource with the full name
// parent, as the request names it.
func tagBinding(method, parent string) string {
	return entry("cloudresourcemanager.googleapis.com", method, "tagValues/281484271805522",
		fmt.Sprintf(`,"request":{"tagBinding":{"parent":%q,"tagValue":"tagValues/281484271805522"}}`, parent))
}

func knownProjects(n string) (string, bool) {
	if n == number {
		return project, true
	}
	return "", false
}

func TestParse(t *testing.T) {
	subnet := "projects/shop-host-1/regions/europe-west1/subnetworks/payments"
	network := "projects/shop-host-1/global/networks/shared"
	inRegion := inventory.TargetKey{Account: project, Region: region}
	everyRegion := inventory.TargetKey{Account: project}
	cases := []struct {
		name string
		body string
		want inventory.TargetKey
		ok   bool
		err  bool
	}{
		{"subnetwork created", insertSubnetwork, inRegion, true, false},
		{"subnetwork expanded", compute("v1.compute.subnetworks.expandIpCidrRange", subnet), inRegion, true, false},
		{"subnetwork deleted, beta API", compute("beta.compute.subnetworks.delete", subnet), inRegion, true, false},
		{"network created: every region", compute("v1.compute.networks.insert", network), everyRegion, true, false},
		{"network peered", compute("v1.compute.networks.addPeering", network), everyRegion, true, false},
		{"network deleted by its URL", compute("v1.compute.networks.delete",
			"https://www.googleapis.com/compute/v1/"+network), everyRegion, true, false},
		{"failed call", entry("compute.googleapis.com", "v1.compute.subnetworks.insert", subnet,
			`,"status":{"code":7,"message":"PERMISSION_DENIED"}`), inventory.TargetKey{}, false, false},
		{"a read, with Data Access logs on", compute("v1.compute.subnetworks.list", "projects/shop-host-1/regions/europe-west1"),
			inventory.TargetKey{}, false, false},
		{"an IAM policy", compute("v1.compute.subnetworks.setIamPolicy", subnet), inventory.TargetKey{}, false, false},
		{"an instance", compute("v1.compute.instances.insert", "projects/shop-host-1/zones/europe-west1-b/instances/vm-1"),
			inventory.TargetKey{}, false, false},
		{"a firewall rule", compute("v1.compute.firewalls.insert", "projects/shop-host-1/global/firewalls/allow-ssh"),
			inventory.TargetKey{}, false, false},
		{"another service", entry("storage.googleapis.com", "storage.buckets.create", "projects/_/buckets/b", ""),
			inventory.TargetKey{}, false, false},
		{"subnetwork tagged, by project number", tagBinding("TagBindings.CreateTagBinding",
			"//compute.googleapis.com/projects/987654321098/regions/europe-west1/subnetworks/4471"), inRegion, true, false},
		{"network untagged", tagBinding("TagBindings.DeleteTagBinding",
			"//compute.googleapis.com/projects/987654321098/global/networks/9921"), everyRegion, true, false},
		{"instance tagged", tagBinding("TagBindings.CreateTagBinding",
			"//compute.googleapis.com/projects/987654321098/zones/europe-west1-b/instances/77"), inventory.TargetKey{}, false, false},
		{"project tagged", tagBinding("TagBindings.CreateTagBinding",
			"//cloudresourcemanager.googleapis.com/projects/987654321098"), inventory.TargetKey{}, false, false},
		{"tag values listed", entry("cloudresourcemanager.googleapis.com", "TagValues.ListTagValues", "tagKeys/1", ""),
			inventory.TargetKey{}, false, false},
		{"unknown project number: the log's project", tagBinding("TagBindings.CreateTagBinding",
			"//compute.googleapis.com/projects/111111111111/regions/europe-west1/subnetworks/4471"), inRegion, true, false},
		{"not an audit log", `{"textPayload":"hello","logName":"projects/shop-host-1/logs/app"}`,
			inventory.TargetKey{}, false, false},
		{"garbage", `not json`, inventory.TargetKey{}, false, true},
		{"a subnetwork change naming no subnetwork", compute("v1.compute.subnetworks.insert", "projects/shop-host-1"),
			inventory.TargetKey{}, false, true},
		{"no project to be found", `{"protoPayload":{"@type":"type.googleapis.com/google.cloud.audit.AuditLog",` +
			`"serviceName":"cloudresourcemanager.googleapis.com","methodName":"TagBindings.CreateTagBinding",` +
			`"request":{"tagBinding":{"parent":"//compute.googleapis.com/projects/111111111111/global/networks/1"}}},` +
			`"logName":"organizations/123/logs/cloudaudit.googleapis.com%2Factivity"}`, inventory.TargetKey{}, false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok, err := Parse([]byte(c.body), knownProjects)
			if (err != nil) != c.err || ok != c.ok || got != c.want {
				t.Errorf("Parse = %+v, %v, %v; want %+v, %v, err=%v", got, ok, err, c.want, c.ok, c.err)
			}
		})
	}
}

func TestParseWithCreationReportsTheCreator(t *testing.T) {
	key, creation, ok, err := ParseWithCreation([]byte(insertSubnetwork), nil)
	if err != nil || !ok {
		t.Fatalf("ParseWithCreation = %v, %v", ok, err)
	}
	if key != (inventory.TargetKey{Account: project, Region: region}) {
		t.Errorf("target = %v", key)
	}
	want := inventory.Creation{Target: key, ResourceID: "projects/shop-host-1/regions/europe-west1/subnetworks/payments",
		Principal: "maria.k@example.com", EventName: "compute.subnetworks.insert"}
	if creation == nil || *creation != want {
		t.Errorf("creation = %+v, want %+v (the ID the inventory reports the subnetwork by)", creation, want)
	}
}

// A person acting as a service account shows up as the service account; when the entry does
// not say who that was, the first principal of the chain is the next best.
func TestParseWithCreationPrincipals(t *testing.T) {
	network := "projects/shop-host-1/global/networks/shared"
	for _, c := range []struct {
		auth, want string
	}{
		{`{"principalEmail":"deploy@shop-host-1.iam.gserviceaccount.com","serviceAccountDelegationInfo":` +
			`[{"firstPartyPrincipal":{"principalEmail":"maria.k@example.com"}}]}`, "deploy@shop-host-1.iam.gserviceaccount.com"},
		{`{"serviceAccountDelegationInfo":[{"firstPartyPrincipal":{"principalEmail":"maria.k@example.com"}}]}`,
			"maria.k@example.com"},
		{`{}`, ""},
	} {
		body := entry("compute.googleapis.com", "v1.compute.networks.insert", network, `,"authenticationInfo":`+c.auth)
		_, creation, ok, err := ParseWithCreation([]byte(body), nil)
		if err != nil || !ok || creation == nil {
			t.Fatalf("ParseWithCreation = %v, %v, %v", creation, ok, err)
		}
		if creation.Principal != c.want || creation.ResourceID != network {
			t.Errorf("%s: creation = %+v, want principal %q", c.auth, creation, c.want)
		}
	}
}

func TestParseWithCreationOnlyForInserts(t *testing.T) {
	for _, body := range []string{
		compute("v1.compute.subnetworks.patch", "projects/shop-host-1/regions/europe-west1/subnetworks/payments"),
		tagBinding("TagBindings.CreateTagBinding", "//compute.googleapis.com/projects/987654321098/global/networks/9921"),
	} {
		_, creation, ok, err := ParseWithCreation([]byte(body), knownProjects)
		if err != nil || !ok {
			t.Fatalf("ParseWithCreation = %v, %v", ok, err)
		}
		if creation != nil {
			t.Errorf("creation = %+v, want none", creation)
		}
	}
}

func TestSubscriptionPattern(t *testing.T) {
	for s, want := range map[string]bool{
		"projects/ops-central/subscriptions/subnet-operator-events": true,
		"projects/example.com:ops/subscriptions/events":             true,
		"subnet-operator-events":                                    false,
		"projects/ops-central/topics/subnet-operator-events":        false,
		"projects/ops-central/subscriptions/":                       false,
		"projects//subscriptions/events":                            false,
	} {
		if got := SubscriptionPattern.MatchString(s); got != want {
			t.Errorf("%q: %v, want %v", s, got, want)
		}
	}
}
