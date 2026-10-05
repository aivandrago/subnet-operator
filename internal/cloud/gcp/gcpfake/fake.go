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

// Package gcpfake is Google Cloud in memory, for tests: the parts of the Compute Engine and
// Resource Manager REST APIs the GCP provider calls, served over HTTP so the provider runs
// with the real Google client libraries, their wire format, paging and error parsing. There
// is no Compute emulator (research note #40), so this is what `make test` runs the provider
// contract against.
//
// It keeps projects, VPC networks, subnetworks with their secondary ranges, IP usage, tag
// keys, tag values and tag bindings, answers
//
//	GET  /compute/v1/projects/{project}/global/networks[/{network}]
//	GET  /compute/v1/projects/{project}/regions/{region}/subnetworks[?views=WITH_UTILIZATION]
//	GET  /compute/v1/projects/{project}/regions/{region}/subnetworks/{subnetwork}
//	POST /compute/v1/projects/{project}/regions/{region}/subnetworks (with params.resourceManagerTags)
//	GET  /compute/v1/projects/{project}/regions/{region}/operations/{operation}
//	GET  <resource manager>/v3/projects/{project}
//	GET  <resource manager>/v3/effectiveTags?parent=//compute.googleapis.com/...
//	GET  <resource manager>/v3/tagKeys/namespaced?name=... and tagValues/namespaced?name=...
//	POST <resource manager>/v3/tagValues and /v3/tagBindings
//	POST <iam credentials>/v1/projects/-/serviceAccounts/{email}:generateAccessToken
//
// and knows who calls: the operator's own identity or a service account it impersonates
// (iam.go), so a test sees which identity read or wrote what, and a project can refuse
// everyone but the principals it names. Tag keys and values under a project belong to that
// project for this; those under an organization answer every principal.
//
// Compute and Resource Manager refuse what Google refuses: a subnetwork range that overlaps
// another range of the network (400), a name taken in the region (409), a tag value that does
// not exist, a second value of a bound key (400), a key past its value limit (400), a tag
// binding at the wrong location. Long-running operations are done when they are returned.
//
// with the Compute pages cut short so paging is exercised, and fails on demand the way Google
// does: 403 rateLimitExceeded from Compute, 429 from Resource Manager, 403 for a missing
// permission. Resource Manager is served per location, as Google serves it: the tags of a
// subnetwork only from its region's endpoint.
package gcpfake

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/compute/apiv1/computepb"
	"cloud.google.com/go/longrunning/autogen/longrunningpb"
	"cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	"golang.org/x/oauth2"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// Failure is a way every call can be made to fail.
type Failure int

const (
	// None lets calls succeed.
	None Failure = iota
	// Throttle rate-limits every call: Compute with 403 rateLimitExceeded, Resource Manager
	// with 429.
	Throttle
	// Deny refuses every call with 403 for a missing permission.
	Deny
)

// globalLocation is the Resource Manager location of global resources and projects.
const globalLocation = "global"

// PageSize is how many items a Compute or Resource Manager list page holds.
const PageSize = 2

// Cloud is the fake. Its methods arrange the cloud directly, the way somebody with the console
// would; the provider only reads it over HTTP.
type Cloud struct {
	server *httptest.Server

	mu       sync.Mutex
	nextID   uint64
	projects map[string]*project
	// tagKeys and tagValues are by namespaced name: <namespace>/<key>[/<value>].
	tagKeys   map[string]*TagKey
	tagValues map[string]*TagValue
	// MaxValuesPerKey is how many values a tag key may hold; Resource Manager allows 1,000.
	MaxValuesPerKey int
	failure         Failure
	// throttleNext throttles that many further calls, then lets them through.
	throttleNext int
	requests     []string
	// requestsBy are the requests by principal.
	requestsBy map[string][]string

	// serviceAccounts are the principals that may impersonate each service account.
	serviceAccounts map[string][]string
	// tokens are the tokens issued by the fake IAM, by token.
	tokens        map[string]issuedToken
	issued        map[string]int
	tokenLifetime time.Duration
}

type project struct {
	id, number string
	networks   map[string]*Network    // by name
	subnets    map[string]*Subnetwork // by region/name
	// principals are the only principals that may call the project, nil for everyone.
	principals []string
	// writers are the only principals that may write to the project, nil for everyone.
	writers []string
}

// Binding is a tag bound to a network or subnetwork.
type Binding struct {
	// Parent is the tag key's parent: organizations/<number> or projects/<number>.
	Parent string
	// Namespace is how the parent appears in namespaced names: the organization number, or
	// the project ID.
	Namespace string
	Key       string
	Value     string
	// Inherited marks a tag the resource inherits from an ancestor rather than has bound.
	Inherited bool
}

// Network is a VPC network of the fake.
type Network struct {
	ID                    uint64
	Name                  string
	AutoCreateSubnetworks bool
	RoutingMode           string
	InternalIPv6Range     string
	Bindings              []Binding
	// Peerings are the network's VPC Network Peering connections; AddPeering makes them.
	Peerings []Peering
}

// Peering is a VPC Network Peering connection of a network of the fake.
type Peering struct {
	Name string
	// Project and Network name the peer network.
	Project, Network string
	// State is ACTIVE once the peer network has a peering back, INACTIVE until then.
	State        string
	StateDetails string
}

// Range is a secondary range of a subnetwork.
type Range struct {
	Name string
	CIDR string
	// Used is how many of its addresses are allocated.
	Used int64
}

// Subnetwork is a subnetwork of the fake.
type Subnetwork struct {
	ID                 uint64
	Name               string
	Region             string
	Network            string // the network's name
	CIDR               string
	Secondary          []Range
	Purpose            string
	StackType          string
	IPv6AccessType     string
	InternalIPv6Prefix string
	// Used is how many addresses of the primary range are allocated, besides the four GCP
	// reserves.
	Used int64
	// UsageUnknown leaves the subnetwork's utilization out of every answer.
	UsageUnknown bool
	// PrivateIPGoogleAccess is the subnetwork setting of the same name.
	PrivateIPGoogleAccess bool
	Bindings              []Binding
}

// TagKey is a Resource Manager tag key.
type TagKey struct {
	ID        uint64
	Parent    string // organizations/<number> or projects/<number>
	Namespace string // the organization number or the project ID
	ShortName string
}

// Name is the key's resource name, tagKeys/<id>.
func (k *TagKey) Name() string { return "tagKeys/" + strconv.FormatUint(k.ID, 10) }

// TagValue is a Resource Manager tag value.
type TagValue struct {
	ID        uint64
	Key       *TagKey
	ShortName string
}

// Name is the value's resource name, tagValues/<id>.
func (v *TagValue) Name() string { return "tagValues/" + strconv.FormatUint(v.ID, 10) }

func (v *TagValue) namespaced() string {
	return v.Key.Namespace + "/" + v.Key.ShortName + "/" + v.ShortName
}

// New starts the fake. Close it when done.
func New() *Cloud {
	c := &Cloud{projects: map[string]*project{}, nextID: 1000, tagKeys: map[string]*TagKey{},
		tagValues: map[string]*TagValue{}, MaxValuesPerKey: 1000, requestsBy: map[string][]string{},
		serviceAccounts: map[string][]string{}, tokens: map[string]issuedToken{}, issued: map[string]int{},
		tokenLifetime: DefaultTokenLifetime}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /compute/v1/projects/{project}/global/networks", c.listNetworks)
	mux.HandleFunc("GET /compute/v1/projects/{project}/global/networks/{network}", c.getNetwork)
	mux.HandleFunc("GET /compute/v1/projects/{project}/regions/{region}/subnetworks", c.listSubnetworks)
	mux.HandleFunc("GET /compute/v1/projects/{project}/regions/{region}/subnetworks/{subnetwork}", c.getSubnetwork)
	mux.HandleFunc("POST /compute/v1/projects/{project}/regions/{region}/subnetworks", c.insertSubnetwork)
	mux.HandleFunc("GET /compute/v1/projects/{project}/regions/{region}/operations/{operation}", c.getOperation)
	mux.HandleFunc("GET /rm/{location}/v3/projects/{project}", c.getProject)
	mux.HandleFunc("GET /rm/{location}/v3/effectiveTags", c.listEffectiveTags)
	mux.HandleFunc("GET /rm/{location}/v3/tagKeys/namespaced", c.getNamespacedTagKey)
	mux.HandleFunc("GET /rm/{location}/v3/tagValues/namespaced", c.getNamespacedTagValue)
	mux.HandleFunc("POST /rm/{location}/v3/tagValues", c.createTagValue)
	mux.HandleFunc("POST /rm/{location}/v3/tagBindings", c.createTagBinding)
	mux.HandleFunc("POST /iam/v1/projects/-/serviceAccounts/{name}", c.generateAccessToken)
	c.server = httptest.NewServer(mux)
	return c
}

// Close stops the fake.
func (c *Cloud) Close() { c.server.Close() }

// ClientOptions make a Google API client talk to the fake as the operator's own identity. Add
// option.WithTokenSource after them to act as another principal.
func (c *Cloud) ClientOptions() []option.ClientOption {
	return []option.ClientOption{option.WithTokenSource(oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: operatorToken, TokenType: "Bearer"}))}
}

// ComputeEndpoint is the fake's Compute endpoint.
func (c *Cloud) ComputeEndpoint() string { return c.server.URL }

// ResourceManagerEndpoint is the fake's Resource Manager endpoint of a location.
func (c *Cloud) ResourceManagerEndpoint(location string) string {
	return c.server.URL + "/rm/" + location
}

// AddProject creates a project with the number Resource Manager knows it by.
func (c *Cloud) AddProject(id, number string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.projects[id] = &project{id: id, number: number, networks: map[string]*Network{}, subnets: map[string]*Subnetwork{}}
}

func (c *Cloud) project(id string) *project {
	p := c.projects[id]
	if p == nil {
		panic("gcpfake: no project " + id)
	}
	return p
}

// AddNetwork creates a custom mode VPC network with the tags bound.
func (c *Cloud) AddNetwork(projectID, name string, bindings ...Binding) *Network {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	n := &Network{ID: c.nextID, Name: name, RoutingMode: "REGIONAL", Bindings: bindings}
	c.registerTags(bindings)
	c.project(projectID).networks[name] = n
	return n
}

// AddSubnetwork creates a subnetwork of a network with the tags bound. It panics on a range
// that overlaps another subnetwork of the network, which Compute refuses.
func (c *Cloud) AddSubnetwork(projectID, region, network, name, cidr string, bindings ...Binding) *Subnetwork {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.project(projectID)
	if p.networks[network] == nil {
		panic("gcpfake: no network " + network)
	}
	prefix := netip.MustParsePrefix(cidr)
	for _, s := range p.subnets {
		if s.Network == network && netip.MustParsePrefix(s.CIDR).Overlaps(prefix) {
			panic(fmt.Sprintf("gcpfake: %s overlaps %s of subnetwork %s", cidr, s.CIDR, s.Name))
		}
	}
	c.nextID++
	s := &Subnetwork{ID: c.nextID, Name: name, Region: region, Network: network, CIDR: cidr, Purpose: "PRIVATE",
		StackType: "IPV4_ONLY", Bindings: bindings}
	c.registerTags(bindings)
	p.subnets[region+"/"+name] = s
	return s
}

// AddPeering adds a peering from one network to another, possibly in another project. Like
// in Compute, it stays INACTIVE until the peer network has a peering back, and both turn
// ACTIVE then. Unlike Compute, it does not refuse networks whose ranges overlap, so tests can
// build what Compute leaves behind when one side peered before the ranges were changed.
func (c *Cloud) AddPeering(projectID, network, name, peerProject, peerNetwork string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.project(projectID).networks[network]
	if n == nil {
		panic("gcpfake: no network " + network)
	}
	pr := Peering{Name: name, Project: peerProject, Network: peerNetwork, State: "INACTIVE",
		StateDetails: "[2026-09-29T00:00:00.000-07:00]: Waiting for peer network to connect."}
	if peer := c.projects[peerProject]; peer != nil && peer.networks[peerNetwork] != nil {
		back := peer.networks[peerNetwork]
		for i := range back.Peerings {
			if back.Peerings[i].Project == projectID && back.Peerings[i].Network == network {
				pr.State, pr.StateDetails = "ACTIVE", "[2026-09-29T00:00:00.000-07:00]: Connected."
				back.Peerings[i].State, back.Peerings[i].StateDetails = pr.State, pr.StateDetails
			}
		}
	}
	n.Peerings = append(n.Peerings, pr)
}

// RemoveNetwork deletes a network and its subnetworks, as deleting them in the console does.
func (c *Cloud) RemoveNetwork(projectID, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.project(projectID)
	delete(p.networks, name)
	for key, s := range p.subnets {
		if s.Network == name {
			delete(p.subnets, key)
		}
	}
	// A peering with a network that is gone stays, inactive.
	for _, other := range c.projects {
		for _, n := range other.networks {
			for i := range n.Peerings {
				if n.Peerings[i].Project == projectID && n.Peerings[i].Network == name {
					n.Peerings[i].State = "INACTIVE"
					n.Peerings[i].StateDetails = "[2026-09-29T00:00:00.000-07:00]: Peer network was deleted."
				}
			}
		}
	}
}

// Networks returns the project's networks, for Update to change.
func (c *Cloud) Networks(projectID string) []*Network {
	p := c.project(projectID)
	out := make([]*Network, 0, len(p.networks))
	for _, name := range sortedKeys(p.networks) {
		out = append(out, p.networks[name])
	}
	return out
}

// Subnetworks returns the project's subnetworks in every region, for Update to change.
func (c *Cloud) Subnetworks(projectID string) []*Subnetwork {
	p := c.project(projectID)
	out := make([]*Subnetwork, 0, len(p.subnets))
	for _, key := range sortedKeys(p.subnets) {
		out = append(out, p.subnets[key])
	}
	return out
}

// Update changes networks and subnetworks of the fake under its lock: call Networks and
// Subnetworks inside f.
func (c *Cloud) Update(f func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f()
}

// Fail makes every call fail that way until Fail(None).
func (c *Cloud) Fail(f Failure) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failure = f
}

// ThrottleNext rate-limits the next n calls, then lets calls through again.
func (c *Cloud) ThrottleNext(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.throttleNext = n
}

// Requests returns the requests served so far, as "METHOD path?query", and forgets them.
func (c *Cloud) Requests() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.requests
	c.requests = nil
	return out
}

// begin records the request and answers it with the failure in force, if any. It returns
// false when it answered.
func (c *Cloud) begin(w http.ResponseWriter, r *http.Request, resourceManager bool) bool {
	line := r.Method + " " + r.URL.RequestURI()
	c.requests = append(c.requests, line)
	if principal := c.principalOf(r); principal != "" {
		c.requestsBy[principal] = append(c.requestsBy[principal], line)
	}
	failure := c.failure
	if c.throttleNext > 0 {
		c.throttleNext--
		failure = Throttle
	}
	switch failure {
	case Throttle:
		if resourceManager {
			writeError(w, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", "", "Quota exceeded for quota metric 'Read requests'")
		} else {
			writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "rateLimitExceeded",
				"Quota exceeded for quota metric 'Read requests' and limit 'Read requests per minute per region'")
		}
		return false
	case Deny:
		writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "forbidden", "Required permission is missing")
		return false
	}
	return true
}

// writeError answers the way Google APIs do: {"error": {code, message, status, errors[]}}.
func writeError(w http.ResponseWriter, code int, status, reason, message string) {
	body := map[string]any{"code": code, "message": message, "status": status}
	if reason != "" {
		body["errors"] = []map[string]string{{"message": message, "domain": "usageLimits", "reason": reason}}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": body})
}

func writeProto(w http.ResponseWriter, m proto.Message) {
	raw, err := protojson.Marshal(m)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// page cuts a sorted list at the page token, an offset.
func page[T any](items []T, token string) ([]T, string) {
	start, _ := strconv.Atoi(token)
	start = min(max(start, 0), len(items))
	end := min(start+PageSize, len(items))
	next := ""
	if end < len(items) {
		next = strconv.Itoa(end)
	}
	return items[start:end], next
}

func selfLink(parts ...string) string {
	return "https://www.googleapis.com/compute/v1/" + strings.Join(parts, "/")
}

func (c *Cloud) listNetworks(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.begin(w, r, false) {
		return
	}
	p := c.projects[r.PathValue("project")]
	if !c.authorized(w, r, p) {
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "notFound", "project not found")
		return
	}
	var items []*computepb.Network
	for _, name := range sortedKeys(p.networks) {
		items = append(items, networkProto(p, p.networks[name]))
	}
	items, next := page(items, r.URL.Query().Get("pageToken"))
	out := &computepb.NetworkList{Items: items}
	if next != "" {
		out.NextPageToken = new(next)
	}
	writeProto(w, out)
}

func networkProto(p *project, n *Network) *computepb.Network {
	item := &computepb.Network{
		Id:                    new(n.ID),
		Name:                  new(n.Name),
		SelfLink:              new(selfLink("projects", p.id, "global", "networks", n.Name)),
		AutoCreateSubnetworks: new(n.AutoCreateSubnetworks),
		RoutingConfig:         &computepb.NetworkRoutingConfig{RoutingMode: new(n.RoutingMode)},
	}
	if n.InternalIPv6Range != "" {
		item.InternalIpv6Range = new(n.InternalIPv6Range)
	}
	for _, pr := range n.Peerings {
		item.Peerings = append(item.Peerings, &computepb.NetworkPeering{
			Name:                 new(pr.Name),
			Network:              new(selfLink("projects", pr.Project, "global", "networks", pr.Network)),
			State:                new(pr.State),
			StateDetails:         new(pr.StateDetails),
			ExchangeSubnetRoutes: new(true),
		})
	}
	return item
}

// usable is the number of addresses in a range less those reserved.
func usable(cidr string, reserved int64) int64 {
	p := netip.MustParsePrefix(cidr)
	return int64(1)<<(32-p.Bits()) - reserved
}

func subnetworkProto(p *project, s *Subnetwork, withUsage bool) *computepb.Subnetwork {
	item := &computepb.Subnetwork{
		Id:                    new(s.ID),
		Name:                  new(s.Name),
		Region:                new(selfLink("projects", p.id, "regions", s.Region)),
		Network:               new(selfLink("projects", p.id, "global", "networks", s.Network)),
		SelfLink:              new(selfLink("projects", p.id, "regions", s.Region, "subnetworks", s.Name)),
		IpCidrRange:           new(s.CIDR),
		Purpose:               new(s.Purpose),
		StackType:             new(s.StackType),
		PrivateIpGoogleAccess: new(s.PrivateIPGoogleAccess),
	}
	if s.IPv6AccessType != "" {
		item.Ipv6AccessType = new(s.IPv6AccessType)
	}
	if s.InternalIPv6Prefix != "" {
		item.InternalIpv6Prefix = new(s.InternalIPv6Prefix)
	}
	usage := &computepb.SubnetworkUtilizationDetails{Ipv4Utilizations: []*computepb.SubnetworkUtilizationDetailsIPV4Utilization{{
		// Google counts the four reserved addresses as allocated (research note #40).
		TotalAllocatedIp: new(4 + s.Used),
		TotalFreeIp:      new(usable(s.CIDR, 4) - s.Used),
	}}}
	for _, sr := range s.Secondary {
		item.SecondaryIpRanges = append(item.SecondaryIpRanges,
			&computepb.SubnetworkSecondaryRange{RangeName: new(sr.Name), IpCidrRange: new(sr.CIDR)})
		usage.Ipv4Utilizations = append(usage.Ipv4Utilizations, &computepb.SubnetworkUtilizationDetailsIPV4Utilization{
			RangeName: new(sr.Name), TotalAllocatedIp: new(sr.Used),
			TotalFreeIp: new(usable(sr.CIDR, 0) - sr.Used),
		})
	}
	if withUsage && !s.UsageUnknown {
		item.UtilizationDetails = usage
	}
	return item
}

func (c *Cloud) listSubnetworks(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.begin(w, r, false) {
		return
	}
	p := c.projects[r.PathValue("project")]
	if !c.authorized(w, r, p) {
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "notFound", "project not found")
		return
	}
	region := r.PathValue("region")
	withUsage := r.URL.Query().Get("views") == "WITH_UTILIZATION"
	var items []*computepb.Subnetwork
	for _, key := range sortedKeys(p.subnets) {
		s := p.subnets[key]
		if s.Region != region {
			continue
		}
		item := subnetworkProto(p, s, withUsage)
		items = append(items, item)
	}
	items, next := page(items, r.URL.Query().Get("pageToken"))
	out := &computepb.SubnetworkList{Items: items}
	if next != "" {
		out.NextPageToken = new(next)
	}
	writeProto(w, out)
}

func (c *Cloud) getProject(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.begin(w, r, true) {
		return
	}
	if r.PathValue("location") != globalLocation {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "badRequest", "projects are served globally")
		return
	}
	p := c.projects[r.PathValue("project")]
	if !c.authorized(w, r, p) {
		return
	}
	if p == nil {
		writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "forbidden", "project not found or permission denied")
		return
	}
	writeProto(w, &resourcemanagerpb.Project{Name: "projects/" + p.number, ProjectId: p.id,
		State: resourcemanagerpb.Project_ACTIVE})
}

// resolve finds the network or subnetwork a Resource Manager parent names, its project, and
// the location its tags are served from.
func (c *Cloud) resolve(parent string) (p *project, bindings *[]Binding, location string, ok bool) {
	rest, found := strings.CutPrefix(parent, "//compute.googleapis.com/projects/")
	if !found {
		return nil, nil, "", false
	}
	parts := strings.Split(rest, "/")
	for _, candidate := range c.projects {
		if candidate.number == parts[0] {
			p = candidate
		}
	}
	if p == nil {
		return nil, nil, "", false // resources are named by project number, never by ID
	}
	switch {
	case len(parts) == 4 && parts[1] == "global" && parts[2] == "networks":
		for _, n := range p.networks {
			if strconv.FormatUint(n.ID, 10) == parts[3] {
				return p, &n.Bindings, globalLocation, true
			}
		}
	case len(parts) == 5 && parts[1] == "regions" && parts[3] == "subnetworks":
		for _, s := range p.subnets {
			if s.Region == parts[2] && strconv.FormatUint(s.ID, 10) == parts[4] {
				return p, &s.Bindings, s.Region, true
			}
		}
	}
	return p, nil, "", false
}

func (c *Cloud) listEffectiveTags(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.begin(w, r, true) {
		return
	}
	p, bindings, location, ok := c.resolve(r.URL.Query().Get("parent"))
	if !c.authorized(w, r, p) {
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "notFound", "resource not found")
		return
	}
	if r.PathValue("location") != location {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "badRequest",
			"the resource's tags are served by the "+location+" endpoint")
		return
	}
	var tags []*resourcemanagerpb.EffectiveTag
	for _, b := range *bindings {
		tags = append(tags, &resourcemanagerpb.EffectiveTag{
			TagValue:           "tagValues/" + strconv.Itoa(len(b.Key)+len(b.Value)),
			NamespacedTagValue: b.Namespace + "/" + b.Key + "/" + b.Value,
			TagKey:             "tagKeys/" + strconv.Itoa(len(b.Key)),
			NamespacedTagKey:   b.Namespace + "/" + b.Key,
			TagKeyParentName:   b.Parent,
			Inherited:          b.Inherited,
		})
	}
	tags, next := page(tags, r.URL.Query().Get("pageToken"))
	writeProto(w, &resourcemanagerpb.ListEffectiveTagsResponse{EffectiveTags: tags, NextPageToken: next})
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// registerTags makes sure the keys and values of arranged bindings exist, as they must in
// Resource Manager before anything is bound. Call with the lock held.
func (c *Cloud) registerTags(bindings []Binding) {
	for _, b := range bindings {
		if b.Inherited {
			continue
		}
		c.ensureTagValue(b.Parent, b.Namespace, b.Key, b.Value)
	}
}

func (c *Cloud) ensureTagKey(parent, namespace, key string) *TagKey {
	name := namespace + "/" + key
	if k := c.tagKeys[name]; k != nil {
		return k
	}
	c.nextID++
	k := &TagKey{ID: c.nextID, Parent: parent, Namespace: namespace, ShortName: key}
	c.tagKeys[name] = k
	return k
}

func (c *Cloud) ensureTagValue(parent, namespace, key, value string) *TagValue {
	k := c.ensureTagKey(parent, namespace, key)
	name := namespace + "/" + key + "/" + value
	if v := c.tagValues[name]; v != nil {
		return v
	}
	c.nextID++
	v := &TagValue{ID: c.nextID, Key: k, ShortName: value}
	c.tagValues[name] = v
	return v
}

// AddTagKey creates a tag key under a parent (organizations/<number> with the number as its
// namespace, or projects/<number> with the project ID).
func (c *Cloud) AddTagKey(parent, namespace, key string) *TagKey {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ensureTagKey(parent, namespace, key)
}

// AddTagValue creates a tag value, and its key if needed.
func (c *Cloud) AddTagValue(parent, namespace, key, value string) *TagValue {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ensureTagValue(parent, namespace, key, value)
}

// TagValueExists reports whether <namespace>/<key>/<value> exists.
func (c *Cloud) TagValueExists(namespace, key, value string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tagValues[namespace+"/"+key+"/"+value] != nil
}

func (c *Cloud) valuesOf(k *TagKey) int {
	n := 0
	for _, v := range c.tagValues {
		if v.Key == k {
			n++
		}
	}
	return n
}

func (c *Cloud) valueByName(name string) *TagValue {
	for _, v := range c.tagValues {
		if v.Name() == name {
			return v
		}
	}
	return nil
}

// Subnetwork returns a subnetwork by region and name, or nil.
func (c *Cloud) Subnetwork(projectID, region, name string) *Subnetwork {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.project(projectID).subnets[region+"/"+name]
}

func (c *Cloud) getNetwork(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.begin(w, r, false) {
		return
	}
	p := c.projects[r.PathValue("project")]
	if !c.authorized(w, r, p) {
		return
	}
	if p == nil || p.networks[r.PathValue("network")] == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "notFound", "The resource was not found")
		return
	}
	writeProto(w, networkProto(p, p.networks[r.PathValue("network")]))
}

func (c *Cloud) getSubnetwork(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.begin(w, r, false) {
		return
	}
	p := c.projects[r.PathValue("project")]
	if !c.authorized(w, r, p) {
		return
	}
	var s *Subnetwork
	if p != nil {
		s = p.subnets[r.PathValue("region")+"/"+r.PathValue("subnetwork")]
	}
	if s == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "notFound", "The resource was not found")
		return
	}
	writeProto(w, subnetworkProto(p, s, false))
}

// doneOperation is a Compute operation that has finished.
func doneOperation(p *project, region, name, target string) *computepb.Operation {
	return &computepb.Operation{
		Name:          new(name),
		Status:        computepb.Operation_DONE.Enum(),
		OperationType: new("insert"),
		TargetLink:    new(target),
		SelfLink:      new(selfLink("projects", p.id, "regions", region, "operations", name)),
	}
}

func (c *Cloud) insertSubnetwork(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.begin(w, r, false) {
		return
	}
	p := c.projects[r.PathValue("project")]
	if !c.authorized(w, r, p) {
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "notFound", "project not found")
		return
	}
	region := r.PathValue("region")
	in := &computepb.Subnetwork{}
	raw, err := io.ReadAll(r.Body)
	if err == nil {
		err = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(raw, in)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "parseError", err.Error())
		return
	}
	network := in.GetNetwork()[strings.LastIndex(in.GetNetwork(), "/")+1:]
	if p.networks[network] == nil || !strings.Contains(in.GetNetwork(), "projects/"+p.id+"/global/networks/") {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid",
			fmt.Sprintf("Invalid value for field 'resource.network': '%s'. The referenced network resource cannot be found.",
				in.GetNetwork()))
		return
	}
	name := in.GetName()
	if p.subnets[region+"/"+name] != nil {
		writeError(w, http.StatusConflict, "ALREADY_EXISTS", "alreadyExists",
			fmt.Sprintf("The resource 'projects/%s/regions/%s/subnetworks/%s' already exists", p.id, region, name))
		return
	}
	prefix, err := netip.ParsePrefix(in.GetIpCidrRange())
	if err != nil || prefix.Masked() != prefix || prefix.Bits() > 29 {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid",
			fmt.Sprintf("Invalid IPCidrRange: %s is not a valid subnetwork range.", in.GetIpCidrRange()))
		return
	}
	for _, key := range sortedKeys(p.subnets) {
		s := p.subnets[key]
		if s.Network != network {
			continue
		}
		ranges := []string{s.CIDR}
		for _, sr := range s.Secondary {
			ranges = append(ranges, sr.CIDR)
		}
		for _, cidr := range ranges {
			if netip.MustParsePrefix(cidr).Overlaps(prefix) {
				writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid",
					fmt.Sprintf("Invalid IPCidrRange: %s conflicts with existing subnetwork '%s' in region '%s'.",
						prefix, s.Name, s.Region))
				return
			}
		}
	}
	var bindings []Binding
	for keyName, valueName := range in.GetParams().GetResourceManagerTags() {
		v := c.valueByName(valueName)
		if v == nil || v.Key.Name() != keyName {
			writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "invalid",
				fmt.Sprintf("Invalid resource manager tag %s=%s.", keyName, valueName))
			return
		}
		bindings = append(bindings, Binding{Parent: v.Key.Parent, Namespace: v.Key.Namespace, Key: v.Key.ShortName,
			Value: v.ShortName})
	}
	c.nextID++
	s := &Subnetwork{ID: c.nextID, Name: name, Region: region, Network: network, CIDR: prefix.String(),
		Purpose: "PRIVATE", StackType: "IPV4_ONLY", PrivateIPGoogleAccess: in.GetPrivateIpGoogleAccess(),
		Bindings: bindings}
	p.subnets[region+"/"+name] = s
	c.nextID++
	writeProto(w, doneOperation(p, region, "operation-"+strconv.FormatUint(c.nextID, 10),
		selfLink("projects", p.id, "regions", region, "subnetworks", name)))
}

func (c *Cloud) getOperation(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.begin(w, r, false) {
		return
	}
	p := c.projects[r.PathValue("project")]
	if !c.authorized(w, r, p) {
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "notFound", "project not found")
		return
	}
	writeProto(w, doneOperation(p, r.PathValue("region"), r.PathValue("operation"), ""))
}

// globalOnly answers a Resource Manager call that only the global endpoint serves. It returns
// false when it answered.
func globalOnly(w http.ResponseWriter, r *http.Request) bool {
	if r.PathValue("location") != globalLocation {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "badRequest", "tag keys and values are served globally")
		return false
	}
	return true
}

func tagKeyProto(k *TagKey) *resourcemanagerpb.TagKey {
	return &resourcemanagerpb.TagKey{Name: k.Name(), Parent: k.Parent, ShortName: k.ShortName,
		NamespacedName: k.Namespace + "/" + k.ShortName}
}

func tagValueProto(v *TagValue) *resourcemanagerpb.TagValue {
	return &resourcemanagerpb.TagValue{Name: v.Name(), Parent: v.Key.Name(), ShortName: v.ShortName,
		NamespacedName: v.namespaced()}
}

func (c *Cloud) getNamespacedTagKey(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.begin(w, r, true) || !globalOnly(w, r) {
		return
	}
	k := c.tagKeys[r.URL.Query().Get("name")]
	if !c.authorized(w, r, c.namespaceProject(r.URL.Query().Get("name"))) {
		return
	}
	if k == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "", "TagKey not found: "+r.URL.Query().Get("name"))
		return
	}
	writeProto(w, tagKeyProto(k))
}

func (c *Cloud) getNamespacedTagValue(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.begin(w, r, true) || !globalOnly(w, r) {
		return
	}
	v := c.tagValues[r.URL.Query().Get("name")]
	if !c.authorized(w, r, c.namespaceProject(r.URL.Query().Get("name"))) {
		return
	}
	if v == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "", "TagValue not found: "+r.URL.Query().Get("name"))
		return
	}
	writeProto(w, tagValueProto(v))
}

// parentProject is the project a tag key's parent names (projects/<number>), or nil for an
// organization. Call with the lock held.
func (c *Cloud) parentProject(parent string) *project {
	number, ok := strings.CutPrefix(parent, "projects/")
	if !ok {
		return nil
	}
	for _, p := range c.projects {
		if p.number == number {
			return p
		}
	}
	return nil
}

// namespaceProject is the project whose ID starts a namespaced tag key or value name, or nil
// for an organization number. Call with the lock held.
func (c *Cloud) namespaceProject(name string) *project {
	namespace, _, _ := strings.Cut(name, "/")
	return c.projects[namespace]
}

// doneLRO is a finished Resource Manager long-running operation with its result.
func doneLRO(w http.ResponseWriter, name string, result proto.Message) {
	response, err := anypb.New(result)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL", "", err.Error())
		return
	}
	writeProto(w, &longrunningpb.Operation{Name: name, Done: true,
		Result: &longrunningpb.Operation_Response{Response: response}})
}

func (c *Cloud) createTagValue(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.begin(w, r, true) || !globalOnly(w, r) {
		return
	}
	in := &resourcemanagerpb.TagValue{}
	raw, err := io.ReadAll(r.Body)
	if err == nil {
		err = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(raw, in)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "", err.Error())
		return
	}
	var key *TagKey
	for _, k := range c.tagKeys {
		if k.Name() == in.GetParent() {
			key = k
		}
	}
	var owner *project
	if key != nil {
		owner = c.parentProject(key.Parent)
	}
	if !c.authorized(w, r, owner) {
		return
	}
	switch {
	case key == nil:
		writeError(w, http.StatusNotFound, "NOT_FOUND", "", "TagKey not found: "+in.GetParent())
		return
	case c.tagValues[key.Namespace+"/"+key.ShortName+"/"+in.GetShortName()] != nil:
		writeError(w, http.StatusConflict, "ALREADY_EXISTS", "", "A TagValue with this short name already exists")
		return
	case c.valuesOf(key) >= c.MaxValuesPerKey:
		writeError(w, http.StatusBadRequest, "FAILED_PRECONDITION", "",
			fmt.Sprintf("The maximum number of TagValues per TagKey (limit %d) has been reached", c.MaxValuesPerKey))
		return
	}
	v := c.ensureTagValue(key.Parent, key.Namespace, key.ShortName, in.GetShortName())
	doneLRO(w, "operations/rdv."+strconv.FormatUint(v.ID, 10), tagValueProto(v))
}

func (c *Cloud) createTagBinding(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.begin(w, r, true) {
		return
	}
	in := &resourcemanagerpb.TagBinding{}
	raw, err := io.ReadAll(r.Body)
	if err == nil {
		err = protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(raw, in)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "", err.Error())
		return
	}
	p, bindings, location, ok := c.resolve(in.GetParent())
	if !c.authorized(w, r, p) {
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "", "resource not found")
		return
	}
	if r.PathValue("location") != location {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "badRequest",
			"the resource's tags are served by the "+location+" endpoint")
		return
	}
	v := c.valueByName(in.GetTagValue())
	if v == nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "", "TagValue not found: "+in.GetTagValue())
		return
	}
	for _, b := range *bindings {
		if b.Inherited || b.Namespace != v.Key.Namespace || b.Key != v.Key.ShortName {
			continue
		}
		if b.Value == v.ShortName {
			writeError(w, http.StatusConflict, "ALREADY_EXISTS", "", "The TagBinding already exists")
		} else {
			writeError(w, http.StatusBadRequest, "FAILED_PRECONDITION", "",
				"A TagValue of the same TagKey is already bound to the resource")
		}
		return
	}
	*bindings = append(*bindings, Binding{Parent: v.Key.Parent, Namespace: v.Key.Namespace, Key: v.Key.ShortName,
		Value: v.ShortName})
	c.nextID++
	doneLRO(w, "operations/rctb."+strconv.FormatUint(c.nextID, 10), &resourcemanagerpb.TagBinding{
		Name:     "tagBindings/" + url.PathEscape(in.GetParent()) + "/" + v.Name(),
		Parent:   in.GetParent(),
		TagValue: v.Name(), TagValueNamespacedName: v.namespaced(),
	})
}
