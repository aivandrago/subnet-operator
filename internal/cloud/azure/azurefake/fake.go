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

// Package azurefake is Azure in memory, for tests: the parts of the Azure Resource Manager
// REST API the Azure provider calls, served over HTTPS so the provider runs with the real Azure
// SDK clients (armnetwork), their wire format, paging, bearer token policy and error parsing.
// There is no networking emulator (research note #41), and the SDK's own fake package has no
// state, so this is what `make test` runs the provider contract against.
//
// It keeps subscriptions, virtual networks with their tags, and subnets with their address
// prefixes, delegations, IP configurations and usage, and answers
//
//	GET /subscriptions/{subscription}/providers/Microsoft.Network/virtualNetworks
//	GET /subscriptions/{subscription}/resourceGroups/{group}/providers/Microsoft.Network/virtualNetworks
//	GET /subscriptions/{subscription}/resourceGroups/{group}/providers/Microsoft.Network/virtualNetworks/{name}/usages
//
// with pages cut short so the nextLink paging is exercised, the locations of a subscription,
// which is what a location name is checked against (locations.go),
//
//	GET /subscriptions/{subscription}/locations
//
// one virtual network with its
// subnets inline, which is what the operator reads before an import,
//
//	GET /subscriptions/{subscription}/resourceGroups/{group}/providers/Microsoft.Network/virtualNetworks/{name}
//
// and the writes of #53:
//
//	GET /subscriptions/{subscription}/resourceGroups/{group}/providers/Microsoft.Network/virtualNetworks/{name}/subnets/{subnet}
//	PUT /subscriptions/{subscription}/resourceGroups/{group}/providers/Microsoft.Network/virtualNetworks/{name}/subnets/{subnet}
//	GET /subscriptions/{subscription}/providers/Microsoft.Network/locations/{location}/operations/{id}
//	GET and PATCH {virtual network ID}/providers/Microsoft.Resources/tags/default
//
// A subnet PUT is a long-running operation, as on ARM: 201 with the subnet Updating and an
// Azure-AsyncOperation URL that reports InProgress a few times (SetOperationPolls) before
// Succeeded, or Failed with the error FailOperations set. It honours If-None-Match: *, and
// refuses a prefix outside the address space (NetcfgSubnetRangeOutsideVnet) or overlapping
// another subnet (NetcfgSubnetRangesOverlap). The Tags API serves Merge only, the one
// operation the operator may use: Replace and Delete are refused, so a test fails if the
// provider ever sends one. Tag names are matched without regard to case, as Azure does, and a
// merged name keeps the spelling it had; a resource takes at most 50 tags.
//
// It reports the reads left in the x-ms-ratelimit-remaining-subscription-reads header, and
// fails on demand the way ARM does: 429 SubscriptionRequestsThrottled with Retry-After, 403
// AuthorizationFailed for a missing role assignment (every call, or only writes), 409
// AnotherOperationInProgress while another operation holds the virtual network, 404
// SubscriptionNotFound and ResourceGroupNotFound, 401 without a bearer token. Resource group
// names are compared without regard to case, as ARM does. One endpoint can be made to fail
// alone (FailEndpoint), its requests are counted (Calls), and its answers can be held back until
// a test lets them go (Hold), which is how the tests of the listing a sync shares make several
// discoveries wait on one call.
//
// Azure Queue Storage is served too, for the change events (queue.go).
//
// Microsoft Entra ID is faked too (entra.go): the token exchange of Workload Identity, and the
// tenant and principal each token stands for, which the subscriptions check, for reads and
// writes alike (RestrictSubscription, RestrictSubscriptionWrites).
package azurefake

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12"
)

// Failure is a way every call can be made to fail.
type Failure int

const (
	// None lets calls succeed.
	None Failure = iota
	// Throttle rate-limits every call with 429 and a Retry-After of a second.
	Throttle
	// Deny refuses every call with 403 AuthorizationFailed.
	Deny
	// DenyWrites refuses every write (PUT, PATCH) with 403 AuthorizationFailed and lets reads
	// through: a write identity without its role assignment.
	DenyWrites
)

// MaxTags is how many tags Azure allows on one resource.
const MaxTags = 50

// PageSize is how many items a list page holds.
const PageSize = 2

// Token is the bearer token Credential hands out and the fake expects.
const Token = "azurefake-token"

// reserved is the number of addresses Azure reserves in each address prefix of a subnet.
const reserved = 5

// Cloud is the fake. Its methods arrange the cloud directly, the way somebody with the portal
// would; the provider only reads it over HTTP.
type Cloud struct {
	server *httptest.Server
	// login is Microsoft Entra ID (entra.go).
	login *httptest.Server

	mu            sync.Mutex
	subscriptions map[string]*subscription
	failure       Failure
	// throttleNext throttles that many further calls, then lets them through.
	throttleNext int
	// retryAfter is the Retry-After a throttled call answers with.
	retryAfter string
	// remainingReads is reported in x-ms-ratelimit-remaining-subscription-reads; -1 leaves
	// the header out.
	remainingReads int
	requests       []string
	// calls counts the Resource Manager requests per endpoint (Endpoint), endpointFailures are
	// the failures in force for one endpoint each, and held the endpoints whose requests wait
	// until the channel is closed.
	calls            map[string]int
	endpointFailures map[string]Failure
	held             map[string]chan struct{}

	// identities are the managed identities and app registrations, by tenant and client ID.
	identities map[principal]*identity
	// tokens are the bearer tokens the fake issued, and whom to.
	tokens map[string]principal
	// issued counts the tokens issued per client ID.
	issued map[string]int
	// requestsBy are the Resource Manager requests per principal.
	requestsBy map[string][]string

	// busyNext answers that many further writes with 409 AnotherOperationInProgress.
	busyNext int
	// operationPolls is how often a new operation reports InProgress before it ends.
	operationPolls int
	// failOperations makes every new operation end Failed with this error, when code is set.
	failOperations struct{ code, message string }
	operations     map[string]*operation
	nextOperation  int
	// unconditionalPuts counts subnet PUTs without If-None-Match: *, which would update a
	// subnet that exists.
	unconditionalPuts int

	// queues are the Storage queues, by name (queue.go).
	queues      map[string]*queue
	nextReceipt int
}

// operation is a long-running operation of the fake.
type operation struct {
	polls         int
	code, message string
}

type subscription struct {
	id     string
	tenant string
	// principals are the only ones it answers, nil for everyone of its tenant; writers the
	// only ones it takes writes from, nil for everyone it answers.
	principals, writers []string
	groups              map[string]bool            // lowercase names of its resource groups
	vnets               map[string]*VirtualNetwork // by lowercase resource group/name
	// locations are the locations it may deploy to, as the locations call names them.
	locations []string
}

// VirtualNetwork is a virtual network of the fake.
type VirtualNetwork struct {
	ResourceGroup   string
	Name            string
	Location        string
	AddressPrefixes []string
	Tags            map[string]string
	Subnets         []*Subnet
	// State is its provisioning state, Succeeded unless set.
	State string
}

// Subnet is a subnet of the fake.
type Subnet struct {
	Name             string
	AddressPrefixes  []string
	Delegations      []string
	ServiceEndpoints []string
	// IPConfigurations is how many IP configurations (NICs and the like) the subnet has.
	IPConfigurations int
	// ServiceAssociationLinks is how many service association links it has.
	ServiceAssociationLinks int
	// Used is the usages' currentValue: the addresses in use. It starts as the number of IP
	// configurations it was created with, none.
	Used int64
	// UsageUnknown makes the usages report -1 for it, as ARM does for gateway subnets.
	UsageUnknown bool
	// NotInUsages leaves it out of the usages.
	NotInUsages  bool
	RouteTableID string
}

// New starts the fake. Close it when done.
func New() *Cloud {
	c := &Cloud{subscriptions: map[string]*subscription{}, retryAfter: "1", remainingReads: 11999,
		identities: map[principal]*identity{}, tokens: map[string]principal{Token: {DefaultTenant, Operator}},
		issued: map[string]int{}, requestsBy: map[string][]string{},
		operationPolls: 2, operations: map[string]*operation{}, queues: map[string]*queue{},
		calls: map[string]int{}, endpointFailures: map[string]Failure{}, held: map[string]chan struct{}{}}
	mux := http.NewServeMux()
	const vnetPath = "/subscriptions/{subscription}/resourceGroups/{group}/providers/Microsoft.Network/" +
		"virtualNetworks/{name}"
	mux.HandleFunc("GET "+vnetPath, c.getVirtualNetwork)
	mux.HandleFunc("GET "+vnetPath+"/subnets/{subnet}", c.getSubnet)
	mux.HandleFunc("PUT "+vnetPath+"/subnets/{subnet}", c.putSubnet)
	mux.HandleFunc("GET /subscriptions/{subscription}/providers/Microsoft.Network/locations/{location}/"+
		"operations/{id}", c.getOperation)
	mux.HandleFunc("GET "+vnetPath+"/providers/Microsoft.Resources/tags/default", c.getTags)
	mux.HandleFunc("PATCH "+vnetPath+"/providers/Microsoft.Resources/tags/default", c.patchTags)
	mux.HandleFunc("GET /subscriptions/{subscription}/providers/Microsoft.Network/virtualNetworks", c.listAll)
	mux.HandleFunc("GET /subscriptions/{subscription}/resourceGroups/{group}/providers/Microsoft.Network/"+
		"virtualNetworks", c.listGroup)
	mux.HandleFunc("GET /subscriptions/{subscription}/resourceGroups/{group}/providers/Microsoft.Network/"+
		"virtualNetworks/{name}/usages", c.listUsage)
	mux.HandleFunc("GET /subscriptions/{subscription}/locations", c.listLocations)
	mux.HandleFunc("GET /"+QueueAccount+"/{queue}/messages", c.getMessages)
	mux.HandleFunc("DELETE /"+QueueAccount+"/{queue}/messages/{id}", c.deleteMessage)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		c.requests = append(c.requests, r.Method+" "+r.URL.RequestURI())
		c.mu.Unlock()
		writeError(w, http.StatusNotFound, "InvalidResourceType", "azurefake does not serve "+r.URL.Path)
	})
	c.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A held endpoint answers once the test lets it: the request has arrived and is counted
		// (Calls), so a test can wait for it and then for whatever else it wants in flight.
		endpoint := Endpoint(r.Method, r.URL.Path)
		c.mu.Lock()
		c.calls[endpoint]++
		hold := c.held[endpoint]
		c.mu.Unlock()
		if hold != nil {
			select {
			case <-hold:
			case <-r.Context().Done():
				return
			}
		}
		mux.ServeHTTP(w, r)
	}))
	// Connections the SDK opened side by side and never used are cut off when the fake closes;
	// that is not worth a line each.
	c.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	c.server.StartTLS()
	c.login = newLogin(c)
	return c
}

// Close stops the fake.
func (c *Cloud) Close() {
	c.server.Close()
	c.login.Close()
}

// Endpoint is the fake's Resource Manager endpoint.
func (c *Cloud) Endpoint() string { return c.server.URL }

// Transport is an HTTP client that trusts the fake's certificate.
func (c *Cloud) Transport() policy.Transporter { return c.server.Client() }

// Credential hands out Token, which the fake accepts as the operator's own identity (Operator,
// in DefaultTenant).
func (c *Cloud) Credential() azcore.TokenCredential { return staticToken{} }

type staticToken struct{}

func (staticToken) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: Token, ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// AddSubscription creates an empty subscription in DefaultTenant.
func (c *Cloud) AddSubscription(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subscriptions[strings.ToLower(id)] = &subscription{id: id, tenant: DefaultTenant, groups: map[string]bool{},
		vnets: map[string]*VirtualNetwork{}, locations: slices.Clone(DefaultLocations)}
}

// DefaultLocations are the locations a new subscription has: a few of Azure's, as the
// locations call names them.
var DefaultLocations = []string{"centralus", "eastus", "eastus2", "germanywestcentral", "northeurope",
	"southeastasia", "swedencentral", "uksouth", "westeurope", "westus", "westus2", "westus3"}

// SetLocations replaces the locations the subscription has.
func (c *Cloud) SetLocations(sub string, locations ...string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subscription(sub).locations = slices.Clone(locations)
}

// The endpoints Endpoint tells apart, named like the operations the provider reports throttling
// under.
const (
	ListAll       = "virtualNetworks.listAll"
	ListGroup     = "virtualNetworks.list"
	ListUsage     = "virtualNetworks.listUsage"
	ListLocations = "subscriptions.listLocations"
	// Other is every other request.
	Other = "other"
)

// Endpoint names the discovery endpoint a Resource Manager request is for, or Other.
func Endpoint(method, path string) string {
	if method != http.MethodGet {
		return Other
	}
	parts := strings.Split(strings.Trim(path, "/"), "/")
	last := strings.ToLower(parts[len(parts)-1])
	switch {
	case len(parts) == 3 && last == "locations":
		return ListLocations
	case len(parts) == 5 && last == "virtualnetworks":
		return ListAll
	case len(parts) == 7 && last == "virtualnetworks":
		return ListGroup
	case len(parts) == 9 && last == "usages":
		return ListUsage
	}
	return Other
}

// Calls returns how many requests each endpoint got so far, every page of a listing and every
// attempt counted, and starts counting again.
func (c *Cloud) Calls() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.calls
	c.calls = map[string]int{}
	return out
}

// CallsSoFar is Calls without starting over.
func (c *Cloud) CallsSoFar() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.calls)
}

// FailEndpoint makes every call of one endpoint fail that way until FailEndpoint(endpoint,
// None), whatever Fail says for the others.
func (c *Cloud) FailEndpoint(endpoint string, f Failure) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f == None {
		delete(c.endpointFailures, endpoint)
		return
	}
	c.endpointFailures[endpoint] = f
}

// Hold makes the requests of an endpoint wait, counted but unanswered, until release is called.
func (c *Cloud) Hold(endpoint string) (release func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	hold := make(chan struct{})
	c.held[endpoint] = hold
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.held[endpoint] == hold {
			delete(c.held, endpoint)
			close(hold)
		}
	}
}

// AddResourceGroup creates an empty resource group. AddVirtualNetwork creates the group of the
// network itself.
func (c *Cloud) AddResourceGroup(sub, group string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.subscription(sub).groups[strings.ToLower(group)] = true
}

func (c *Cloud) subscription(id string) *subscription {
	s := c.subscriptions[strings.ToLower(id)]
	if s == nil {
		panic("azurefake: no subscription " + id)
	}
	return s
}

func vnetKey(group, name string) string {
	return strings.ToLower(group + "/" + name)
}

// AddVirtualNetwork creates a virtual network with the address space and tags.
func (c *Cloud) AddVirtualNetwork(sub, group, name, location string, prefixes []string,
	tags map[string]string) *VirtualNetwork {
	c.mu.Lock()
	defer c.mu.Unlock()
	if tags == nil {
		tags = map[string]string{}
	}
	v := &VirtualNetwork{ResourceGroup: group, Name: name, Location: location,
		AddressPrefixes: slices.Clone(prefixes), Tags: tags}
	s := c.subscription(sub)
	s.groups[strings.ToLower(group)] = true
	s.vnets[vnetKey(group, name)] = v
	return v
}

// VirtualNetwork returns a virtual network, for Update to change.
func (c *Cloud) VirtualNetwork(sub, group, name string) *VirtualNetwork {
	v := c.subscription(sub).vnets[vnetKey(group, name)]
	if v == nil {
		panic("azurefake: no virtual network " + group + "/" + name)
	}
	return v
}

// AddSubnet creates a subnet of a virtual network. It panics on a prefix outside the virtual
// network's address space, or one that overlaps another subnet's, both of which ARM refuses.
func (c *Cloud) AddSubnet(sub, group, vnet, name string, prefixes ...string) *Subnet {
	c.mu.Lock()
	defer c.mu.Unlock()
	v := c.VirtualNetwork(sub, group, vnet)
	for _, p := range prefixes {
		prefix := netip.MustParsePrefix(p)
		inside := false
		for _, space := range v.AddressPrefixes {
			sp := netip.MustParsePrefix(space)
			inside = inside || (sp.Bits() <= prefix.Bits() && sp.Contains(prefix.Addr()))
		}
		if !inside {
			panic(fmt.Sprintf("azurefake: %s is outside the address space %v of %s", p, v.AddressPrefixes, vnet))
		}
		for _, other := range v.Subnets {
			for _, op := range other.AddressPrefixes {
				if netip.MustParsePrefix(op).Overlaps(prefix) {
					panic(fmt.Sprintf("azurefake: %s overlaps %s of subnet %s", p, op, other.Name))
				}
			}
		}
	}
	s := &Subnet{Name: name, AddressPrefixes: slices.Clone(prefixes)}
	v.Subnets = append(v.Subnets, s)
	return s
}

// SetTag sets a tag of a virtual network, as the portal's tag editor would.
func (c *Cloud) SetTag(sub, group, vnet, key, value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.VirtualNetwork(sub, group, vnet).Tags[key] = value
}

// RemoveVirtualNetwork deletes a virtual network and its subnets.
func (c *Cloud) RemoveVirtualNetwork(sub, group, name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.subscription(sub).vnets, vnetKey(group, name))
}

// Update changes the fake under its lock: call VirtualNetwork inside f.
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

// SetRetryAfter sets the Retry-After header of throttled calls, "" for none.
func (c *Cloud) SetRetryAfter(v string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.retryAfter = v
}

// SetRemainingReads sets what x-ms-ratelimit-remaining-subscription-reads reports; -1 leaves it
// out.
func (c *Cloud) SetRemainingReads(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.remainingReads = n
}

// Requests returns the requests served so far, as "METHOD path?query", and forgets them.
func (c *Cloud) Requests() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.requests
	c.requests = nil
	return out
}

// begin records the request and answers it with the failure in force, if any. It returns the
// subscription, or nil when it answered.
func (c *Cloud) begin(w http.ResponseWriter, r *http.Request) *subscription {
	c.requests = append(c.requests, r.Method+" "+r.URL.RequestURI())
	write := r.Method != http.MethodGet
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	who, issued := c.tokens[bearer]
	if !ok || !issued {
		writeError(w, http.StatusUnauthorized, "AuthenticationFailed", "Authentication failed. The 'Authorization' "+
			"header is missing or not a bearer token the fake issued.")
		return nil
	}
	c.requestsBy[who.client] = append(c.requestsBy[who.client], r.Method+" "+r.URL.RequestURI())
	if r.URL.Query().Get("api-version") == "" {
		writeError(w, http.StatusBadRequest, "MissingApiVersionParameter", "The api-version query parameter "+
			"(?api-version=) is required for all requests.")
		return nil
	}
	sub := r.PathValue("subscription")
	failure := c.failure
	if f, ok := c.endpointFailures[Endpoint(r.Method, r.URL.Path)]; ok {
		failure = f
	}
	if c.throttleNext > 0 {
		c.throttleNext--
		failure = Throttle
	}
	if failure == DenyWrites && write {
		failure = Deny
	}
	switch failure {
	case Throttle:
		if c.retryAfter != "" {
			w.Header().Set("Retry-After", c.retryAfter)
		}
		writeError(w, http.StatusTooManyRequests, "SubscriptionRequestsThrottled", fmt.Sprintf(
			"Number of 'read' requests for subscription '%s' actor 'azurefake' exceeded. Please try again after "+
				"'%s' seconds after additional tokens are available.", sub, c.retryAfter))
		return nil
	case Deny:
		writeError(w, http.StatusForbidden, "AuthorizationFailed", fmt.Sprintf("The client 'azurefake' does not "+
			"have authorization to perform action '%s' over scope '/subscriptions/%s' or the scope is invalid. If "+
			"access was recently granted, please refresh your credentials.", action(r), sub))
		return nil
	}
	if write && c.busyNext > 0 {
		c.busyNext--
		writeError(w, http.StatusConflict, "AnotherOperationInProgress", fmt.Sprintf("Another operation on this or "+
			"dependent resource is in progress. To retry later, please follow the Retry-After header. Resource "+
			"'%s'.", r.PathValue("name")))
		return nil
	}
	s := c.subscriptions[strings.ToLower(sub)]
	if s == nil {
		writeError(w, http.StatusNotFound, "SubscriptionNotFound", fmt.Sprintf("The subscription '%s' could not "+
			"be found.", sub))
		return nil
	}
	if !c.authorize(w, r, s, who) {
		return nil
	}
	if c.remainingReads >= 0 {
		w.Header().Set("x-ms-ratelimit-remaining-subscription-reads", strconv.Itoa(c.remainingReads))
	}
	return s
}

// writeError answers the way ARM does: {"error": {"code", "message"}}.
func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("x-ms-failure-cause", "gateway")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{errorKey: map[string]string{"code": code, "message": message}})
}

func writeJSON(w http.ResponseWriter, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "InternalServerError", err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(raw)
}

// page cuts a sorted list at the skip token, an offset, and returns the nextLink of the rest.
func page[T any](r *http.Request, base string, items []T) ([]T, *string) {
	start, _ := strconv.Atoi(r.URL.Query().Get("$skiptoken"))
	start = min(max(start, 0), len(items))
	end := min(start+PageSize, len(items))
	if end >= len(items) {
		return items[start:end], nil
	}
	q := r.URL.Query()
	q.Set("$skiptoken", strconv.Itoa(end))
	next := base + r.URL.Path + "?" + q.Encode()
	return items[start:end], &next
}

func vnetID(sub string, v *VirtualNetwork) string {
	return "/subscriptions/" + sub + "/resourceGroups/" + v.ResourceGroup +
		"/providers/Microsoft.Network/virtualNetworks/" + v.Name
}

func ptrs(values []string) []*string {
	out := make([]*string, 0, len(values))
	for _, v := range values {
		out = append(out, new(v))
	}
	return out
}

// listLocations answers with the subscription's locations, as ARM does: every one in one
// answer, each with its name and display name.
func (c *Cloud) listLocations(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.begin(w, r)
	if s == nil {
		return
	}
	type location struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		DisplayName string `json:"displayName"`
	}
	items := make([]location, 0, len(s.locations))
	for _, name := range s.locations {
		items = append(items, location{ID: "/subscriptions/" + s.id + "/locations/" + name, Name: name,
			DisplayName: name})
	}
	writeJSON(w, map[string]any{"value": items})
}

func (c *Cloud) listAll(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.begin(w, r)
	if s == nil {
		return
	}
	c.listVirtualNetworks(w, r, s, "")
}

func (c *Cloud) listGroup(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.begin(w, r)
	if s == nil {
		return
	}
	group := r.PathValue("group")
	if !s.groups[strings.ToLower(group)] {
		writeError(w, http.StatusNotFound, "ResourceGroupNotFound", fmt.Sprintf("Resource group '%s' could "+
			"not be found.", group))
		return
	}
	c.listVirtualNetworks(w, r, s, group)
}

// listVirtualNetworks answers with the subscription's virtual networks, those of one resource
// group when group is set.
func (c *Cloud) listVirtualNetworks(w http.ResponseWriter, r *http.Request, s *subscription, group string) {
	var items []*armnetwork.VirtualNetwork
	for _, key := range sortedKeys(s.vnets) {
		v := s.vnets[key]
		if group != "" && !strings.EqualFold(v.ResourceGroup, group) {
			continue
		}
		item := virtualNetworkOf(s.id, v)
		items = append(items, item)
	}
	items, next := page(r, c.server.URL, items)
	writeJSON(w, armnetwork.VirtualNetworkListResult{Value: items, NextLink: next})
}

// virtualNetworkOf is a virtual network as ARM answers with it: its location, tags and address
// space, and its subnets inline.
func virtualNetworkOf(sub string, v *VirtualNetwork) *armnetwork.VirtualNetwork {
	id := vnetID(sub, v)
	state := armnetwork.ProvisioningState(v.State)
	if state == "" {
		state = armnetwork.ProvisioningStateSucceeded
	}
	tags := map[string]*string{}
	for k, val := range v.Tags {
		tags[k] = new(val)
	}
	item := &armnetwork.VirtualNetwork{
		ID:       new(id),
		Name:     new(v.Name),
		Type:     new("Microsoft.Network/virtualNetworks"),
		Location: new(v.Location),
		Tags:     tags,
		Properties: &armnetwork.VirtualNetworkPropertiesFormat{
			AddressSpace:      &armnetwork.AddressSpace{AddressPrefixes: ptrs(v.AddressPrefixes)},
			ProvisioningState: new(state),
		},
	}
	for _, sn := range v.Subnets {
		item.Properties.Subnets = append(item.Properties.Subnets, subnetOf(id, sn))
	}
	return item
}

// getVirtualNetwork answers with one virtual network, 404 ResourceNotFound when the resource
// group has none of that name.
func (c *Cloud) getVirtualNetwork(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.begin(w, r)
	if s == nil {
		return
	}
	if v := c.vnetOf(w, r, s); v != nil {
		writeJSON(w, virtualNetworkOf(s.id, v))
	}
}

func subnetOf(vnet string, s *Subnet) *armnetwork.Subnet {
	id := vnet + "/subnets/" + s.Name
	props := &armnetwork.SubnetPropertiesFormat{ProvisioningState: new(armnetwork.ProvisioningStateSucceeded)}
	if len(s.AddressPrefixes) == 1 {
		props.AddressPrefix = new(s.AddressPrefixes[0])
	} else {
		props.AddressPrefixes = ptrs(s.AddressPrefixes)
	}
	for i, d := range s.Delegations {
		props.Delegations = append(props.Delegations, &armnetwork.Delegation{
			Name:       new(fmt.Sprintf("delegation-%d", i)),
			Properties: &armnetwork.ServiceDelegationPropertiesFormat{ServiceName: new(d)},
		})
	}
	for _, e := range s.ServiceEndpoints {
		props.ServiceEndpoints = append(props.ServiceEndpoints, &armnetwork.ServiceEndpointPropertiesFormat{Service: new(e)})
	}
	for i := range s.IPConfigurations {
		props.IPConfigurations = append(props.IPConfigurations, &armnetwork.IPConfiguration{
			ID: new(fmt.Sprintf("%s/networkInterfaces/nic-%d/ipConfigurations/ipconfig1", vnet, i)),
		})
	}
	for i := range s.ServiceAssociationLinks {
		props.ServiceAssociationLinks = append(props.ServiceAssociationLinks, &armnetwork.ServiceAssociationLink{
			ID: new(fmt.Sprintf("%s/serviceAssociationLinks/link-%d", id, i)),
		})
	}
	if s.RouteTableID != "" {
		props.RouteTable = &armnetwork.RouteTable{ID: new(s.RouteTableID)}
	}
	return &armnetwork.Subnet{ID: new(id), Name: new(s.Name), Type: new("Microsoft.Network/virtualNetworks/subnets"),
		Properties: props}
}

// usable is the number of IPv4 addresses of a subnet less the five Azure reserves in each of
// its prefixes. The fake takes the usages' limit to be that, which the research note (#41)
// found the documented example suggests; the conformance run (#56) confirms it.
func usable(prefixes []string) int64 {
	var n int64
	for _, p := range prefixes {
		prefix := netip.MustParsePrefix(p)
		if prefix.Addr().Is4() {
			n += int64(1)<<(32-prefix.Bits()) - reserved
		}
	}
	return n
}

func (c *Cloud) listUsage(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.begin(w, r)
	if s == nil {
		return
	}
	v := s.vnets[vnetKey(r.PathValue("group"), r.PathValue("name"))]
	if v == nil {
		writeError(w, http.StatusNotFound, "ResourceNotFound", fmt.Sprintf("The Resource "+
			"'Microsoft.Network/virtualNetworks/%s' under resource group '%s' was not found.",
			r.PathValue("name"), r.PathValue("group")))
		return
	}
	var items []*armnetwork.VirtualNetworkUsage
	for _, sn := range v.Subnets {
		if sn.NotInUsages {
			continue
		}
		current, limit := float64(sn.Used), float64(usable(sn.AddressPrefixes))
		if sn.UsageUnknown {
			current, limit = -1, -1
		}
		items = append(items, &armnetwork.VirtualNetworkUsage{
			ID:           new(vnetID(s.id, v) + "/subnets/" + sn.Name),
			CurrentValue: new(current),
			Limit:        new(limit),
			Name:         &armnetwork.VirtualNetworkUsageName{Value: new("SubnetSpace"), LocalizedValue: new("Subnet size and usage")},
			Unit:         new("Count"),
		})
	}
	items, next := page(r, c.server.URL, items)
	writeJSON(w, armnetwork.VirtualNetworkListUsageResult{Value: items, NextLink: next})
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// BusyNext answers the next n writes with 409 AnotherOperationInProgress, then lets writes
// through again.
func (c *Cloud) BusyNext(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.busyNext = n
}

// UnconditionalPuts is how many subnet PUTs came without If-None-Match: *, which on a subnet
// that exists would replace its settings.
func (c *Cloud) UnconditionalPuts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.unconditionalPuts
}

// SetOperationPolls sets how often a new long-running operation reports InProgress before it
// ends.
func (c *Cloud) SetOperationPolls(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.operationPolls = n
}

// FailOperations makes every new long-running operation end Failed with the error, and the
// subnet it was for is not created; an empty code lets them succeed again.
func (c *Cloud) FailOperations(code, message string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.failOperations.code, c.failOperations.message = code, message
}

// Tags returns a copy of a virtual network's tags.
func (c *Cloud) Tags(sub, group, vnet string) map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.VirtualNetwork(sub, group, vnet).Tags)
}

// Subnet returns a subnet of a virtual network by name, in any case, or nil.
func (c *Cloud) Subnet(sub, group, vnet, name string) *Subnet {
	c.mu.Lock()
	defer c.mu.Unlock()
	return findSubnet(c.VirtualNetwork(sub, group, vnet), name)
}

func findSubnet(v *VirtualNetwork, name string) *Subnet {
	for _, s := range v.Subnets {
		if strings.EqualFold(s.Name, name) {
			return s
		}
	}
	return nil
}

// action is the Azure RBAC action a request needs, for AuthorizationFailed messages.
func action(r *http.Request) string {
	switch {
	case Endpoint(r.Method, r.URL.Path) == ListLocations:
		return "Microsoft.Resources/subscriptions/locations/read"
	case strings.Contains(r.URL.Path, "/providers/Microsoft.Resources/tags/"):
		if r.Method == http.MethodGet {
			return "Microsoft.Resources/tags/read"
		}
		return "Microsoft.Resources/tags/write"
	case strings.Contains(r.URL.Path, "/subnets/"):
		if r.Method == http.MethodGet {
			return "Microsoft.Network/virtualNetworks/subnets/read"
		}
		return "Microsoft.Network/virtualNetworks/subnets/write"
	}
	return "Microsoft.Network/virtualNetworks/read"
}

// vnetOf returns the virtual network a request names, or answers 404 and returns nil.
func (c *Cloud) vnetOf(w http.ResponseWriter, r *http.Request, s *subscription) *VirtualNetwork {
	group := r.PathValue("group")
	if !s.groups[strings.ToLower(group)] {
		writeError(w, http.StatusNotFound, "ResourceGroupNotFound", fmt.Sprintf("Resource group '%s' could "+
			"not be found.", group))
		return nil
	}
	v := s.vnets[vnetKey(group, r.PathValue("name"))]
	if v == nil {
		writeError(w, http.StatusNotFound, "ResourceNotFound", fmt.Sprintf("The Resource "+
			"'Microsoft.Network/virtualNetworks/%s' under resource group '%s' was not found.",
			r.PathValue("name"), group))
	}
	return v
}

func (c *Cloud) getSubnet(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.begin(w, r)
	if s == nil {
		return
	}
	v := c.vnetOf(w, r, s)
	if v == nil {
		return
	}
	sn := findSubnet(v, r.PathValue("subnet"))
	if sn == nil {
		writeError(w, http.StatusNotFound, "NotFound", fmt.Sprintf("Resource %s/subnets/%s not found.",
			vnetID(s.id, v), r.PathValue("subnet")))
		return
	}
	writeJSON(w, subnetOf(vnetID(s.id, v), sn))
}

// putSubnet creates a subnet as ARM does: checked against the address space and the other
// subnets first, then a long-running operation. A subnet that exists already is updated,
// unless the request says If-None-Match: *; the operator always says it.
func (c *Cloud) putSubnet(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.begin(w, r)
	if s == nil {
		return
	}
	v := c.vnetOf(w, r, s)
	if v == nil {
		return
	}
	name := r.PathValue("subnet")
	var body armnetwork.Subnet
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequestFormat", "Cannot parse the request: "+err.Error())
		return
	}
	existing := findSubnet(v, name)
	if r.Header.Get("If-None-Match") != "*" {
		c.unconditionalPuts++
	}
	if existing != nil && r.Header.Get("If-None-Match") == "*" {
		writeError(w, http.StatusPreconditionFailed, "PreconditionFailed", fmt.Sprintf("The subnet '%s' exists "+
			"already, and the request asked for it not to.", name))
		return
	}
	var prefixes []string
	if body.Properties != nil {
		if body.Properties.AddressPrefix != nil {
			prefixes = append(prefixes, *body.Properties.AddressPrefix)
		}
		for _, p := range body.Properties.AddressPrefixes {
			if p != nil {
				prefixes = append(prefixes, *p)
			}
		}
	}
	if len(prefixes) == 0 {
		writeError(w, http.StatusBadRequest, "NetcfgInvalidSubnet", fmt.Sprintf("Subnet '%s' is not valid: it "+
			"has no address prefix.", name))
		return
	}
	for _, p := range prefixes {
		prefix, err := netip.ParsePrefix(p)
		if err != nil || prefix.Masked() != prefix {
			writeError(w, http.StatusBadRequest, "NetcfgInvalidSubnet", fmt.Sprintf("Subnet '%s' is not valid "+
				"because its IP address range %s is not a valid CIDR network address.", name, p))
			return
		}
		inside := false
		for _, space := range v.AddressPrefixes {
			sp := netip.MustParsePrefix(space)
			inside = inside || (sp.Bits() <= prefix.Bits() && sp.Contains(prefix.Addr()))
		}
		if !inside {
			writeError(w, http.StatusBadRequest, "NetcfgSubnetRangeOutsideVnet", fmt.Sprintf("Subnet '%s' is not "+
				"valid because its IP address range is outside the IP address range of virtual network '%s'.",
				name, v.Name))
			return
		}
		for _, other := range v.Subnets {
			if other == existing {
				continue
			}
			for _, op := range other.AddressPrefixes {
				if netip.MustParsePrefix(op).Overlaps(prefix) {
					writeError(w, http.StatusBadRequest, "NetcfgSubnetRangesOverlap", fmt.Sprintf("Subnet '%s' is "+
						"not valid because its IP address range overlaps with that of an existing subnet in virtual "+
						"network '%s'.", name, v.Name))
					return
				}
			}
		}
	}

	op := &operation{polls: c.operationPolls, code: c.failOperations.code, message: c.failOperations.message}
	c.nextOperation++
	opID := fmt.Sprintf("op-%d", c.nextOperation)
	c.operations[opID] = op
	sn := existing
	if op.code == "" {
		if sn == nil {
			sn = &Subnet{Name: name}
			v.Subnets = append(v.Subnets, sn)
		}
		sn.AddressPrefixes = prefixes
	}
	out := subnetOf(vnetID(s.id, v), &Subnet{Name: name, AddressPrefixes: prefixes})
	out.Properties.ProvisioningState = new(armnetwork.ProvisioningStateUpdating)
	w.Header().Set("Azure-AsyncOperation", c.server.URL+"/subscriptions/"+s.id+
		"/providers/Microsoft.Network/locations/"+normalLocation(v.Location)+"/operations/"+opID+
		"?api-version="+r.URL.Query().Get("api-version"))
	w.Header().Set("Retry-After", "10")
	status := http.StatusCreated
	if existing != nil {
		status = http.StatusOK
	}
	raw, _ := json.Marshal(out)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

func normalLocation(location string) string {
	return strings.ToLower(strings.ReplaceAll(location, " ", ""))
}

// operationStatus is the field of an operation's status.
const operationStatus = "status"

func (c *Cloud) getOperation(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.begin(w, r) == nil {
		return
	}
	op := c.operations[r.PathValue("id")]
	switch {
	case op == nil:
		writeError(w, http.StatusNotFound, "OperationNotFound", "The operation was not found.")
	case op.polls > 0:
		op.polls--
		w.Header().Set("Retry-After", "10")
		writeJSON(w, map[string]string{operationStatus: "InProgress"})
	case op.code != "":
		writeJSON(w, map[string]any{operationStatus: "Failed", errorKey: map[string]string{"code": op.code,
			"message": op.message}})
	default:
		writeJSON(w, map[string]string{operationStatus: "Succeeded"})
	}
}

// tagsResource is the Tags API's answer: the tags of the resource at the scope.
func tagsResource(scope string, tags map[string]string) map[string]any {
	return map[string]any{"id": scope + "/providers/Microsoft.Resources/tags/default", "name": "default",
		"type": "Microsoft.Resources/tags", "properties": map[string]any{"tags": tags}}
}

func (c *Cloud) getTags(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.begin(w, r)
	if s == nil {
		return
	}
	if v := c.vnetOf(w, r, s); v != nil {
		writeJSON(w, tagsResource(vnetID(s.id, v), v.Tags))
	}
}

// patchTags merges tags into a virtual network's: a name it carries in any case gets the new
// value and keeps its spelling, a new name is added. Replace and Delete are refused.
func (c *Cloud) patchTags(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.begin(w, r)
	if s == nil {
		return
	}
	v := c.vnetOf(w, r, s)
	if v == nil {
		return
	}
	var body struct {
		Operation  string `json:"operation"`
		Properties struct {
			Tags map[string]string `json:"tags"`
		} `json:"properties"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "InvalidRequestContent", "Cannot parse the request: "+err.Error())
		return
	}
	if body.Operation != "Merge" {
		writeError(w, http.StatusBadRequest, "InvalidRequestContent", fmt.Sprintf("azurefake serves the Merge "+
			"operation only, the one the operator may use; got %q.", body.Operation))
		return
	}
	merged := maps.Clone(v.Tags)
	for name, value := range body.Properties.Tags {
		if len(value) > 256 {
			writeError(w, http.StatusBadRequest, "InvalidTagValueLength", fmt.Sprintf("The tag value '%s' "+
				"exceeds the maximum length of 256 characters.", value))
			return
		}
		spelled := name
		for existing := range merged {
			if strings.EqualFold(existing, name) {
				spelled = existing
			}
		}
		merged[spelled] = value
	}
	if len(merged) > MaxTags {
		writeError(w, http.StatusBadRequest, "InvalidTagCount", fmt.Sprintf("The resource would have %d tags; "+
			"a resource can have a maximum of 50 tags.", len(merged)))
		return
	}
	v.Tags = merged
	writeJSON(w, tagsResource(vnetID(s.id, v), v.Tags))
}
