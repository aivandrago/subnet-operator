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

// Package events is the GCP provider's change-event source, the counterpart of the AWS
// EventBridge → SQS path: Admin Activity audit log entries of Compute Engine networks and
// subnetworks and of Resource Manager tag bindings, routed by a log sink to a Pub/Sub topic
// and pulled from a subscription, are turned into project/region pairs that need a resync.
package events

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"hypersurgery.dev/subnet-operator/internal/inventory"
)

// auditLogType is the protoPayload type of a Cloud Audit Logs entry.
const auditLogType = "type.googleapis.com/google.cloud.audit.AuditLog"

const (
	computeService         = "compute.googleapis.com"
	resourceManagerService = "cloudresourcemanager.googleapis.com"
)

// collections are the Compute Engine collections whose changes the operator reports.
// Everything else Compute logs (instances, firewalls, routers, ...) is left alone, as the
// AWS path leaves RunInstances alone; free IP counts follow the periodic resync.
var collections = map[string]bool{"networks": true, "subnetworks": true}

// unchangingVerbs are the methods of those collections that change nothing the operator
// reports. Reads only appear when Data Access logs are on and the sink lets them through;
// IAM policies are not part of the inventory. Every other method (insert, delete, patch,
// expandIpCidrRange, setPrivateIpGoogleAccess, addPeering, switchToCustomMode, and whatever
// Google adds) is a change: a missed change costs a stale inventory until the next resync, an
// extra one a single discovery.
var unchangingVerbs = map[string]bool{
	"get": true, "list": true, "aggregatedList": true, "listUsable": true,
	"getIamPolicy": true, "setIamPolicy": true, "testIamPermissions": true, "getEffectiveFirewalls": true,
}

// createVerb is the Compute method that creates a network or subnetwork.
const createVerb = "insert"

// logEntry is the part of a LogEntry, as a log sink publishes it to Pub/Sub (JSON), that the
// operator reads.
type logEntry struct {
	LogName  string `json:"logName"`
	Resource struct {
		Labels map[string]string `json:"labels"`
	} `json:"resource"`
	ProtoPayload *struct {
		Type         string `json:"@type"`
		ServiceName  string `json:"serviceName"`
		MethodName   string `json:"methodName"`
		ResourceName string `json:"resourceName"`
		Status       *struct {
			Code int `json:"code"`
		} `json:"status"`
		AuthenticationInfo struct {
			PrincipalEmail               string `json:"principalEmail"`
			ServiceAccountDelegationInfo []struct {
				FirstPartyPrincipal struct {
					PrincipalEmail string `json:"principalEmail"`
				} `json:"firstPartyPrincipal"`
			} `json:"serviceAccountDelegationInfo"`
		} `json:"authenticationInfo"`
		Request  json.RawMessage `json:"request"`
		Response json.RawMessage `json:"response"`
	} `json:"protoPayload"`
}

// ProjectResolver returns the ID of a project given its number, and whether it knows it.
// Resource Manager names resources by project number; the operator knows the numbers of the
// projects it has discovered.
type ProjectResolver func(number string) (string, bool)

// Parse reads one log entry. It returns ok=false for entries that do not affect the inventory:
// other log types, failed calls, other services, collections and methods, and tag bindings on
// other resources. A key without a region stands for every region of the project: networks
// are global in GCP.
func Parse(body []byte, projects ProjectResolver) (key inventory.TargetKey, ok bool, err error) {
	key, _, ok, err = ParseWithCreation(body, projects)
	return key, ok, err
}

// ParseWithCreation also reports the network or subnetwork an insert produced and who called
// it, so an unmanaged resource can be attributed to a person later.
func ParseWithCreation(body []byte, projects ProjectResolver) (key inventory.TargetKey, creation *inventory.Creation,
	ok bool, err error) {
	var e logEntry
	if err := json.Unmarshal(body, &e); err != nil {
		return key, nil, false, fmt.Errorf("decode log entry: %w", err)
	}
	p := e.ProtoPayload
	if p == nil || p.Type != auditLogType || (p.Status != nil && p.Status.Code != 0) {
		return key, nil, false, nil
	}
	method := trimVersion(p.MethodName)
	switch p.ServiceName {
	case computeService:
		collection, verb, found := strings.Cut(strings.TrimPrefix(method, "compute."), ".")
		if !found || !strings.HasPrefix(method, "compute.") || !collections[collection] || unchangingVerbs[verb] {
			return key, nil, false, nil
		}
		r, found := parseComputeName(p.ResourceName)
		if !found || r.collection != collection {
			return key, nil, false, fmt.Errorf("%s names no %s: %q", p.MethodName, collection, p.ResourceName)
		}
		if key, err = targetOf(e, r, projects); err != nil {
			return key, nil, false, fmt.Errorf("%s: %w", p.MethodName, err)
		}
		if verb == createVerb {
			creation = &inventory.Creation{Target: key, ResourceID: r.id(key.Account), Principal: principalOf(e),
				EventName: method}
		}
		return key, creation, true, nil
	case resourceManagerService:
		if !isTagBindingChange(method) {
			return key, nil, false, nil
		}
		// The bound resource is the tag binding's parent, in the request or in the response of
		// the operation, depending on the call and on which of its entries this is.
		r, found := firstComputeName(p.ResourceName, string(p.Request), string(p.Response))
		if !found {
			return key, nil, false, nil // a tag on an instance, a bucket, a project, ...
		}
		if key, err = targetOf(e, r, projects); err != nil {
			return key, nil, false, fmt.Errorf("%s: %w", p.MethodName, err)
		}
		return key, nil, true, nil
	default:
		return key, nil, false, nil
	}
}

// trimVersion drops the API version Compute prefixes its method names with
// (v1.compute.subnetworks.insert, beta.compute.networks.patch).
func trimVersion(method string) string {
	for _, v := range []string{"v1.", "beta.", "alpha."} {
		if rest, ok := strings.CutPrefix(method, v); ok {
			return rest
		}
	}
	return method
}

// isTagBindingChange reports whether a Resource Manager method binds or unbinds a tag:
// TagBindings.CreateTagBinding, TagBindings.DeleteTagBinding, and the same names with the
// full service prefix.
func isTagBindingChange(method string) bool {
	return strings.HasSuffix(method, "CreateTagBinding") || strings.HasSuffix(method, "DeleteTagBinding")
}

// computeResource is one network or subnetwork as a log entry names it.
type computeResource struct {
	project    string // a project ID, or a number in Resource Manager's full resource names
	region     string // empty for a (global) network
	collection string
	name       string // a name, or a numeric ID in Resource Manager's full resource names
}

// id is the resource's ID as the inventory reports it, in the given project.
func (r computeResource) id(project string) string {
	if r.region == "" {
		return fmt.Sprintf("projects/%s/global/%s/%s", project, r.collection, r.name)
	}
	return fmt.Sprintf("projects/%s/regions/%s/%s/%s", project, r.region, r.collection, r.name)
}

// computeName matches a network or subnetwork in a relative resource name
// (projects/p/regions/r/subnetworks/s), a full one (//compute.googleapis.com/projects/...)
// or a URL (https://www.googleapis.com/compute/v1/projects/...).
var computeName = regexp.MustCompile(`projects/([a-z0-9][a-z0-9.:-]*)/(?:global|regions/([a-z0-9-]+))/` +
	`(networks|subnetworks)/([A-Za-z0-9_-]+)`)

func parseComputeName(s string) (computeResource, bool) {
	m := computeName.FindStringSubmatch(s)
	if m == nil {
		return computeResource{}, false
	}
	r := computeResource{project: m[1], region: m[2], collection: m[3], name: m[4]}
	if (r.collection == "subnetworks") != (r.region != "") {
		return computeResource{}, false // a regional network or a global subnetwork is no resource
	}
	return r, true
}

// fullComputeName matches the full resource name of a network or subnetwork, which is how a
// tag binding names its parent.
var fullComputeName = regexp.MustCompile(`//compute\.googleapis\.com/projects/[^"\s]+`)

func firstComputeName(texts ...string) (computeResource, bool) {
	for _, t := range texts {
		for _, full := range fullComputeName.FindAllString(t, -1) {
			if r, ok := parseComputeName(full); ok {
				return r, true
			}
		}
	}
	return computeResource{}, false
}

// targetOf is the project and region a resource is discovered in. A project named by number
// is looked up; the entry's own project (the resource labels, then the log name) is the
// fallback, since audit logs are written to the project the resource is in.
func targetOf(e logEntry, r computeResource, projects ProjectResolver) (inventory.TargetKey, error) {
	project := r.project
	if isNumber(project) {
		id, ok := "", false
		if projects != nil {
			id, ok = projects(project)
		}
		if !ok {
			id = firstNonEmpty(e.Resource.Labels["project_id"], logProject(e.LogName))
		}
		project = id
	}
	if project == "" || isNumber(project) {
		return inventory.TargetKey{}, fmt.Errorf("cannot tell the project ID of %s", r.id(r.project))
	}
	return inventory.TargetKey{Account: project, Region: r.region}, nil
}

// logProject is the project of a log name (projects/<id>/logs/...), or "" for a folder's or
// an organization's log.
func logProject(logName string) string {
	rest, ok := strings.CutPrefix(logName, "projects/")
	if !ok {
		return ""
	}
	project, _, _ := strings.Cut(rest, "/")
	return project
}

// principalOf is who made the call: the principal the call was authenticated as, and when
// that is empty, the first principal of an impersonation chain.
func principalOf(e logEntry) string {
	a := e.ProtoPayload.AuthenticationInfo
	if a.PrincipalEmail != "" || len(a.ServiceAccountDelegationInfo) == 0 {
		return a.PrincipalEmail
	}
	return a.ServiceAccountDelegationInfo[0].FirstPartyPrincipal.PrincipalEmail
}

func isNumber(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
