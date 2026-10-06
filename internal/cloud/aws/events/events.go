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

// Package events is the AWS provider's change-event source: it turns EC2 change events
// (CloudTrail API calls delivered by EventBridge to SQS) into account/region pairs that need a
// resync.
package events

import (
	"encoding/json"
	"fmt"
	"strings"

	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// relevantEvents are the EC2 API calls that change what the operator reports.
// Free IP counts change with every ENI and are left to the periodic resync.
var relevantEvents = map[string]bool{
	"CreateVpc": true, "DeleteVpc": true,
	"AssociateVpcCidrBlock": true, "DisassociateVpcCidrBlock": true,
	"CreateSubnet": true, "DeleteSubnet": true, "ModifySubnetAttribute": true,
	"AssociateSubnetCidrBlock": true, "DisassociateSubnetCidrBlock": true,
	"CreateTags": true, "DeleteTags": true,
	"CreateRouteTable": true, "DeleteRouteTable": true,
	"AssociateRouteTable": true, "DisassociateRouteTable": true, "ReplaceRouteTableAssociation": true,
	"CreateRoute": true, "DeleteRoute": true, "ReplaceRoute": true,
	"AttachInternetGateway": true, "DetachInternetGateway": true,
}

// networkResourcePrefixes are the resource IDs whose tags matter. Tagging an instance or a
// volume produces CreateTags events too, and those are ignored.
var networkResourcePrefixes = []string{"vpc-", "subnet-", "rtb-", "igw-"}

type envelope struct {
	Source     string `json:"source"`
	DetailType string `json:"detail-type"`
	Account    string `json:"account"`
	Region     string `json:"region"`
	Detail     struct {
		EventName          string `json:"eventName"`
		AWSRegion          string `json:"awsRegion"`
		RecipientAccountID string `json:"recipientAccountId"`
		ErrorCode          string `json:"errorCode"`
		RequestParameters  struct {
			ResourcesSet *struct {
				Items []struct {
					ResourceID string `json:"resourceId"`
				} `json:"items"`
			} `json:"resourcesSet"`
		} `json:"requestParameters"`
		UserIdentity struct {
			ARN            string `json:"arn"`
			SessionContext struct {
				SessionIssuer struct {
					ARN string `json:"arn"`
				} `json:"sessionIssuer"`
			} `json:"sessionContext"`
		} `json:"userIdentity"`
		ResponseElements struct {
			Subnet *struct {
				SubnetID string `json:"subnetId"`
			} `json:"subnet"`
			VPC *struct {
				VPCID string `json:"vpcId"`
			} `json:"vpc"`
		} `json:"responseElements"`
	} `json:"detail"`
}

// Parse reads one EventBridge event. It returns ok=false for events that do not affect the
// inventory: other sources, failed API calls, irrelevant API calls and tags on other resources.
func Parse(body string) (key inventory.TargetKey, ok bool, err error) {
	key, _, ok, err = ParseWithCreation(body)
	return key, ok, err
}

// ParseWithCreation also reports the resource a CreateVpc or CreateSubnet call produced and
// who called it, so an unmanaged resource can be attributed to a person later.
func ParseWithCreation(body string) (key inventory.TargetKey, creation *inventory.Creation, ok bool, err error) {
	var e envelope
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		return key, nil, false, fmt.Errorf("decode event: %w", err)
	}
	if e.Source != "aws.ec2" || e.Detail.ErrorCode != "" || !relevantEvents[e.Detail.EventName] {
		return key, nil, false, nil
	}
	if rs := e.Detail.RequestParameters.ResourcesSet; rs != nil && isTagEvent(e.Detail.EventName) {
		network := false
		for _, it := range rs.Items {
			for _, p := range networkResourcePrefixes {
				if strings.HasPrefix(it.ResourceID, p) {
					network = true
				}
			}
		}
		if !network {
			return key, nil, false, nil
		}
	}

	// Events forwarded from another account's bus keep the originating account in "account";
	// the CloudTrail fields are preferred because they name the account that owns the resource.
	key.Account = firstNonEmpty(e.Detail.RecipientAccountID, e.Account)
	key.Region = firstNonEmpty(e.Detail.AWSRegion, e.Region)
	if key.Account == "" || key.Region == "" {
		return key, nil, false, fmt.Errorf("event %s has no account or region", e.Detail.EventName)
	}

	if id := createdResourceID(e); id != "" {
		creation = &inventory.Creation{Target: key, ResourceID: id, Principal: principalOf(e), EventName: e.Detail.EventName}
	}
	return key, creation, true, nil
}

// createdResourceID is the VPC or subnet a create call produced, empty for everything else.
func createdResourceID(e envelope) string {
	switch e.Detail.EventName {
	case "CreateSubnet":
		if s := e.Detail.ResponseElements.Subnet; s != nil {
			return s.SubnetID
		}
	case "CreateVpc":
		if v := e.Detail.ResponseElements.VPC; v != nil {
			return v.VPCID
		}
	}
	return ""
}

// principalOf prefers the full session ARN, which names the person, and falls back to the
// role the session was issued from.
func principalOf(e envelope) string {
	return firstNonEmpty(e.Detail.UserIdentity.ARN, e.Detail.UserIdentity.SessionContext.SessionIssuer.ARN)
}

func isTagEvent(name string) bool {
	return name == "CreateTags" || name == "DeleteTags"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
