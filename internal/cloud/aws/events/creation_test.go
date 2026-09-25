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

import "testing"

// createSubnetEvent is shaped like what EventBridge delivers for a console-made subnet:
// userIdentity names the person, responseElements names what they made.
const createSubnetEvent = `{
  "version": "0",
  "source": "aws.ec2",
  "detail-type": "AWS API Call via CloudTrail",
  "account": "111111111111",
  "region": "eu-central-1",
  "detail": {
    "eventSource": "ec2.amazonaws.com",
    "eventName": "CreateSubnet",
    "awsRegion": "eu-central-1",
    "recipientAccountId": "222222222222",
    "userIdentity": {
      "type": "AssumedRole",
      "arn": "arn:aws:sts::222222222222:assumed-role/payments-deploy/maria.k",
      "sessionContext": {
        "sessionIssuer": {
          "type": "Role",
          "arn": "arn:aws:iam::222222222222:role/payments-deploy"
        }
      }
    },
    "requestParameters": { "cidrBlock": "10.20.40.0/22", "vpcId": "vpc-0aa11bb2" },
    "responseElements": {
      "subnet": { "subnetId": "subnet-04d1c2b3a4e5f607", "state": "pending" }
    }
  }
}`

func TestParseWithCreationReportsTheCreator(t *testing.T) {
	key, creation, ok, err := ParseWithCreation(createSubnetEvent)
	if err != nil || !ok {
		t.Fatalf("ParseWithCreation = %v, %v", ok, err)
	}
	if key.Account != "222222222222" || key.Region != "eu-central-1" {
		t.Errorf("target = %v, want the CloudTrail account and region", key)
	}
	if creation == nil {
		t.Fatal("a CreateSubnet event must report the creation")
	}
	if creation.ResourceID != "subnet-04d1c2b3a4e5f607" {
		t.Errorf("resource = %q", creation.ResourceID)
	}
	if want := "arn:aws:sts::222222222222:assumed-role/payments-deploy/maria.k"; creation.Principal != want {
		t.Errorf("principal = %q, want the session ARN, which names the person", creation.Principal)
	}
	if creation.EventName != "CreateSubnet" {
		t.Errorf("event = %q", creation.EventName)
	}
}

func TestParseWithCreationFallsBackToTheSessionIssuer(t *testing.T) {
	body := `{"source":"aws.ec2","account":"111111111111","region":"eu-west-1","detail":{` +
		`"eventName":"CreateVpc","awsRegion":"eu-west-1","recipientAccountId":"111111111111",` +
		`"userIdentity":{"sessionContext":{"sessionIssuer":{"arn":"arn:aws:iam::111111111111:role/ops-admin"}}},` +
		`"responseElements":{"vpc":{"vpcId":"vpc-09e8d7c6"}}}}`
	_, creation, ok, err := ParseWithCreation(body)
	if err != nil || !ok || creation == nil {
		t.Fatalf("ParseWithCreation = %v, %v, %v", creation, ok, err)
	}
	if want := "arn:aws:iam::111111111111:role/ops-admin"; creation.Principal != want {
		t.Errorf("principal = %q, want %q", creation.Principal, want)
	}
	if creation.ResourceID != "vpc-09e8d7c6" {
		t.Errorf("resource = %q, want the created VPC", creation.ResourceID)
	}
}

func TestParseWithCreationIgnoresOtherEvents(t *testing.T) {
	// A tag change is worth a resync but creates nothing, so there is nobody to attribute.
	body := event("CreateTags", "111111111111",
		`,"requestParameters":{"resourcesSet":{"items":[{"resourceId":"subnet-1"}]}}`)
	_, creation, ok, err := ParseWithCreation(body)
	if err != nil || !ok {
		t.Fatalf("ParseWithCreation = %v, %v", ok, err)
	}
	if creation != nil {
		t.Errorf("creation = %+v, want none", creation)
	}
}

func TestParseStillWorks(t *testing.T) {
	// The narrow entry point keeps behaving as the poller's other callers expect.
	key, ok, err := Parse(createSubnetEvent)
	if err != nil || !ok || key.Account != "222222222222" {
		t.Fatalf("Parse = %v, %v, %v", key, ok, err)
	}
}
