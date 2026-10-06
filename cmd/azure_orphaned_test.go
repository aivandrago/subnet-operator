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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	azurecloud "hypersurgery.dev/subnet-operator/internal/cloud/azure"
	"hypersurgery.dev/subnet-operator/internal/cloud/azure/azurefake"
)

const (
	orphanSubscription = "00000000-0000-4000-8000-00000000c0de"
	otherSubscription  = "00000000-0000-4000-8000-0000000c0de2"
	orphanReader       = "3f0c6a1e-0000-4000-8000-0000000000a1"
	orphanGroup        = "rg-network"
	hubID              = "/subscriptions/" + orphanSubscription + "/resourcegroups/" + orphanGroup +
		"/providers/microsoft.network/virtualnetworks/hub"
)

// orphanFixture is the subcommand against azurefake: a virtual network hub in westeurope with
// two subnets, one with an entry, a network of its own tag, and whatever the test adds.
type orphanFixture struct {
	t       *testing.T
	cloud   *azurefake.Cloud
	objects []client.Object
	env     map[string]string
	// clusterErr fails the connection to the cluster, claimErr every read of a SubnetClaim.
	clusterErr, claimErr error
	clusterUse           int
}

func newOrphanFixture(t *testing.T) *orphanFixture {
	t.Helper()
	cloud := azurefake.New()
	t.Cleanup(cloud.Close)
	cloud.AddSubscription(orphanSubscription)
	cloud.AddVirtualNetwork(orphanSubscription, orphanGroup, "hub", "westeurope", []string{"10.10.0.0/16"},
		map[string]string{"hs-owner": "platform", "cost-center": "42",
			"hs-subnet-apps": "hs-owner=payments"})
	cloud.AddSubnet(orphanSubscription, orphanGroup, "hub", "apps", "10.10.1.0/24")
	cloud.AddSubnet(orphanSubscription, orphanGroup, "hub", "Data", "10.10.2.0/24")
	return &orphanFixture{t: t, cloud: cloud, env: map[string]string{}}
}

func (f *orphanFixture) tag(name, value string) {
	f.cloud.SetTag(orphanSubscription, orphanGroup, "hub", name, value)
}

// run runs the subcommand and returns its exit code and what it printed.
func (f *orphanFixture) run(args ...string) (code int, stdout, stderr string) {
	f.t.Helper()
	tokenFile, err := azurefake.WriteServiceAccountToken(f.t.TempDir())
	if err != nil {
		f.t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	c := &orphanedEntriesCommand{
		stdout: &out, stderr: &errOut,
		getenv: func(k string) string { return f.env[k] },
		now:    func() time.Time { return time.Date(2026, 10, 6, 12, 30, 0, 5, time.UTC) },
		options: func(o azurecloud.Options) azurecloud.Options {
			o.Credential = f.cloud.Credential()
			o.ResourceManagerEndpoint = f.cloud.Endpoint()
			o.Transport = f.cloud.Transport()
			o.AuthorityHost = f.cloud.AuthorityHost()
			o.DisableInstanceDiscovery = true
			o.TenantID = azurefake.DefaultTenant
			o.FederatedTokenFile = tokenFile
			return o
		},
		cluster: func() (client.Reader, error) {
			f.clusterUse++
			if f.clusterErr != nil {
				return nil, f.clusterErr
			}
			return fake.NewClientBuilder().WithScheme(scheme).WithObjects(f.objects...).WithInterceptorFuncs(
				interceptor.Funcs{Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object,
					opts ...client.GetOption) error {
					if _, isClaim := obj.(*networkv1.SubnetClaim); isClaim && f.claimErr != nil {
						return f.claimErr
					}
					return c.Get(ctx, key, obj, opts...)
				}}).Build(), nil
		},
	}
	code = c.run(context.Background(), args)
	return code, out.String(), errOut.String()
}

// onlyReads fails the test if anything but a GET reached Resource Manager, or the tags of hub
// are not what they were.
func (f *orphanFixture) onlyReads(before map[string]string) {
	f.t.Helper()
	requests := f.cloud.Requests()
	if len(requests) == 0 {
		f.t.Error("nothing was read")
	}
	for _, r := range requests {
		if !strings.HasPrefix(r, "GET ") {
			f.t.Errorf("a request that is not a read: %s", r)
		}
	}
	if after := f.cloud.Tags(orphanSubscription, orphanGroup, "hub"); !maps.Equal(after, before) {
		f.t.Errorf("the tags of hub changed: %v, were %v", after, before)
	}
}

func (f *orphanFixture) hubTags() map[string]string {
	return maps.Clone(f.cloud.Tags(orphanSubscription, orphanGroup, "hub"))
}

// With a subscription and nothing else, the entries whose subnet does not exist are listed with
// their network, what they say and the network's tags of 50; the entries of subnets that exist
// and every other tag are not, and nothing is written.
func TestAzureOrphanedEntriesListsThem(t *testing.T) {
	f := newOrphanFixture(t)
	f.tag("hs-subnet-deleted", "hs-env=prod;hs-owner=team-gone")
	f.tag("HS-Subnet-DATA", "hs-tier=db")
	f.tag("hs-subnet-abandoned", "hs-claim=default/abandoned;hs-owner=team-a")
	f.tag("hs-subnet-odd", "somebody's note")
	f.tag("hs-subnets", "apps,data")
	before := f.hubTags()

	code, out, errOut := f.run("--subscription", strings.ToUpper(orphanSubscription))
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q\n%s", code, errOut, out)
	}
	f.onlyReads(before)
	for _, want := range []string{
		hubID + " (westeurope): 8 of 50 tags, 5 ownership entries, 3 orphaned\n",
		"  ENTRY                SAYS                                        NOTE\n",
		"  hs-subnet-abandoned  hs-claim=default/abandoned;hs-owner=team-a  written for SubnetClaim default/abandoned: " +
			"remove it only if that claim is gone or will never create this subnet\n",
		"  hs-subnet-deleted    hs-env=prod;hs-owner=team-gone              \n",
		"  hs-subnet-odd        somebody's note                             not in the operator's format",
		"3 orphaned ownership entries on 1 of the 1 virtual networks read. Nothing was changed;",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the output lacks %q:\n%s", want, out)
		}
	}
	for _, not := range []string{"hs-subnet-apps", "DATA", "cost-center", "hs-subnets", "hs-owner  ", "az tag"} {
		if strings.Contains(out, not) {
			t.Errorf("the output has %q:\n%s", not, out)
		}
	}
	if f.clusterUse != 0 {
		t.Error("the cluster was asked without --scope")
	}

	// Nothing orphaned.
	clean := newOrphanFixture(t)
	code, out, _ = clean.run("--subscription", orphanSubscription)
	if code != 0 || out != "No orphaned ownership entries on the 1 virtual networks read.\n" {
		t.Errorf("exit %d for a subscription with nothing orphaned:\n%s", code, out)
	}
}

func TestAzureOrphanedEntriesAsJSON(t *testing.T) {
	f := newOrphanFixture(t)
	f.tag("hs-subnet-deleted", "hs-env=prod;hs-owner=team-gone")
	f.tag("hs-subnet-odd", "somebody's <note>")
	f.cloud.AddVirtualNetwork(orphanSubscription, orphanGroup, "clean", "northeurope", []string{"10.20.0.0/16"}, nil)
	before := f.hubTags()

	code, out, errOut := f.run("--subscription", orphanSubscription, "-o", "json")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	f.onlyReads(before)
	var report struct {
		ReadAt          string
		NetworksRead    int
		OrphanedEntries int
		Networks        []struct {
			NetworkID, Subscription, ResourceGroup, Name, Location string
			TagCount, TagLimit, OwnershipEntries                   int
			Orphaned                                               []json.RawMessage
		}
		Errors []string
	}
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&report); err != nil {
		t.Fatalf("the output is not the JSON documented: %v\n%s", err, out)
	}
	if report.ReadAt != "2026-10-06T12:30:00Z" || report.NetworksRead != 2 || report.OrphanedEntries != 2 ||
		len(report.Networks) != 1 || len(report.Errors) != 0 {
		t.Fatalf("report = %+v", report)
	}
	n := report.Networks[0]
	if n.NetworkID != hubID || n.Subscription != orphanSubscription || n.ResourceGroup != orphanGroup || n.Name != "hub" ||
		n.Location != "westeurope" || n.TagCount != 5 || n.TagLimit != 50 || n.OwnershipEntries != 3 || len(n.Orphaned) != 2 {
		t.Fatalf("network = %+v", n)
	}
	deleted, odd := n.Orphaned[0], n.Orphaned[1]
	wantDeleted := `{"tag":"hs-subnet-deleted","subnet":"deleted","value":"hs-env=prod;hs-owner=team-gone",` +
		`"tags":{"hs-env":"prod","hs-owner":"team-gone"},"operatorFormat":true,"removeCommand":"az tag update ` +
		`--resource-id '` + hubID + `' --operation Delete --tags 'hs-subnet-deleted=hs-env=prod;hs-owner=team-gone'"}`
	if got := compactJSON(t, deleted); got != wantDeleted {
		t.Errorf("the deleted subnet's entry = %s\nwant %s", got, wantDeleted)
	}
	wantOdd := `{"tag":"hs-subnet-odd","subnet":"odd","value":"somebody's <note>","operatorFormat":false,` +
		`"note":"not in the operator's format (written or edited by something else): no command proposed"}`
	if got := compactJSON(t, odd); got != wantOdd {
		t.Errorf("the entry somebody else wrote = %s\nwant %s", got, wantOdd)
	}
	if !strings.Contains(out, `"orphaned": [`) || strings.Count(out, `"orphaned"`) != 1 {
		t.Errorf("a network's entries are not once under \"orphaned\":\n%s", out)
	}
}

func compactJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

func azureScope(accounts ...networkv1.Account) *networkv1.NetworkScope {
	return &networkv1.NetworkScope{ObjectMeta: metav1.ObjectMeta{Name: "landing-zone"},
		Spec: networkv1.NetworkScopeSpec{Provider: networkv1.ProviderAzure, Regions: []string{"westeurope"},
			Azure: &networkv1.AzureScope{ResourceGroups: []string{orphanGroup}}, Accounts: accounts}}
}

// With a scope, its subscriptions are read as the read identities it names, in its resource
// groups and locations, and an entry written for a SubnetClaim that still exists is listed
// without a command: its create may be tried again, and uses the entry.
func TestAzureOrphanedEntriesOfAScope(t *testing.T) {
	f := newOrphanFixture(t)
	f.cloud.AddIdentity(azurefake.DefaultTenant, orphanReader, true)
	f.cloud.RestrictSubscription(orphanSubscription, orphanReader)
	f.cloud.RestrictSubscriptionWrites(orphanSubscription)
	f.cloud.AddVirtualNetwork(orphanSubscription, orphanGroup, "north", "northeurope", []string{"10.20.0.0/16"},
		map[string]string{"hs-subnet-elsewhere": "hs-owner=x"})
	f.cloud.AddVirtualNetwork(orphanSubscription, "rg-other", "other", "westeurope", []string{"10.30.0.0/16"},
		map[string]string{"hs-subnet-other-group": "hs-owner=x"})
	f.tag("hs-subnet-retrying", "hs-claim=team-a/retrying;hs-owner=team-a")
	f.tag("hs-subnet-given-up", "hs-claim=team-a/given-up;hs-owner=team-a")
	f.tag("hs-subnet-imported", "hs-owner=team-b")
	f.objects = []client.Object{
		azureScope(networkv1.Account{ID: strings.ToUpper(orphanSubscription),
			Azure: &networkv1.AzureAccount{ClientID: orphanReader}}),
		&networkv1.SubnetClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "retrying"}},
	}
	before := f.hubTags()

	code, out, errOut := f.run("--scope", "landing-zone", "--output=json")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	f.onlyReads(before)
	if got := f.cloud.RequestsBy(azurefake.Operator); len(got) != 0 {
		t.Errorf("requests as the operator's own identity, not the scope's reader: %v", got)
	}
	var report orphanReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	if report.NetworksRead != 1 || len(report.Networks) != 1 || report.Networks[0].ID != hubID {
		t.Fatalf("networks = %+v, want hub alone: the scope's resource group and location", report)
	}
	byTag := map[string]reportedEntry{}
	for _, e := range report.Networks[0].Orphaned {
		byTag[e.Tag] = e
	}
	if len(byTag) != 3 {
		t.Fatalf("orphaned = %+v", byTag)
	}
	if e := byTag["hs-subnet-retrying"]; e.ClaimState != claimExists || e.RemoveCommand != "" ||
		!strings.Contains(e.Note, "SubnetClaim team-a/retrying exists and may still create this subnet") {
		t.Errorf("the entry of a claim that exists = %+v", e)
	}
	if e := byTag["hs-subnet-given-up"]; e.ClaimState != claimNotFound || !strings.Contains(e.RemoveCommand,
		"--tags 'hs-subnet-given-up=hs-claim=team-a/given-up;hs-owner=team-a'") {
		t.Errorf("the entry of a claim that is gone = %+v", e)
	}
	if e := byTag["hs-subnet-imported"]; e.ClaimState != "" || e.Note != "" || e.RemoveCommand == "" {
		t.Errorf("the entry an import wrote = %+v", e)
	}

	// As the caller: the reader's role is not theirs here.
	code, _, errOut = f.run("--scope", "landing-zone", "--own-identity")
	if code != 1 || !strings.Contains(errOut, "subscription "+orphanSubscription+": list virtual networks:") ||
		!strings.Contains(errOut, "AuthorizationFailed") {
		t.Errorf("exit %d with --own-identity, stderr %q; want AuthorizationFailed for the subscription", code, errOut)
	}
	if got := f.cloud.RequestsBy(azurefake.Operator); len(got) == 0 {
		t.Error("--own-identity did not read as the caller")
	}
	// Other locations than the scope's, when asked for.
	_, out, _ = f.run("--scope", "landing-zone", "--location", "North Europe")
	if !strings.Contains(out, "virtualnetworks/north (northeurope)") || strings.Contains(out, "virtualnetworks/hub") {
		t.Errorf("with --location northeurope:\n%s", out)
	}
}

// The commands are printed and nothing is run. There is one for an orphaned entry in the
// operator's own format, and none for an entry whose subnet exists, for a tag that is no entry,
// for a tag under the prefix that somebody else wrote, or for the entry of a claim that exists.
func TestAzureOrphanedEntriesPrintsRemoveCommandsAndRunsNone(t *testing.T) {
	f := newOrphanFixture(t)
	f.tag("hs-subnet-deleted", "hs-owner=it's gone")
	f.tag("hs-subnet-odd", "somebody's note")
	f.tag("hs-subnet-retrying", "hs-claim=team-a/retrying;hs-owner=team-a")
	f.tag("hs-subnet-", "hs-owner=x")
	f.objects = []client.Object{
		azureScope(networkv1.Account{ID: orphanSubscription}),
		&networkv1.SubnetClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "retrying"}},
	}
	before := f.hubTags()

	code, out, errOut := f.run("--scope", "landing-zone", "--remove-commands")
	if code != 0 || errOut != "" {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	f.onlyReads(before)
	var commands []string
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line != "" && !strings.HasPrefix(line, "#") {
			commands = append(commands, line)
		}
	}
	want := `az tag update --resource-id '` + hubID + `' --operation Delete --tags ` +
		`'hs-subnet-deleted=hs-owner=it'\''s gone'`
	if len(commands) != 1 || commands[0] != want {
		t.Errorf("commands = %q\nwant one: %s", commands, want)
	}
	for _, want := range []string{
		"# Ownership entries (hs-subnet-<subnet name>) whose subnet did not exist when the virtual\n" +
			"# networks were read, at 2026-10-06T12:30:00Z. Nothing has been removed",
		"# " + hubID + " (westeurope): 7 of 50 tags, 3 orphaned\n",
		"# hs-subnet-odd: not in the operator's format",
		"# hs-subnet-retrying: SubnetClaim team-a/retrying exists and may still create this subnet: no command proposed\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the script lacks %q:\n%s", want, out)
		}
	}

	// Without the cluster to ask, the claim's entry gets its command, and a warning before it.
	code, out, _ = f.run("--subscription", orphanSubscription, "--remove-commands")
	if code != 0 || !strings.Contains(out, "# hs-subnet-retrying: written for SubnetClaim team-a/retrying: remove it "+
		"only if that claim is gone or will never create this subnet\naz tag update --resource-id '"+hubID+"' --operation "+
		"Delete --tags 'hs-subnet-retrying=hs-claim=team-a/retrying;hs-owner=team-a'\n") {
		t.Errorf("exit %d without a cluster:\n%s", code, out)
	}
	f.onlyReads(before)

	// A claim that cannot be read is not taken for one that is gone.
	f.claimErr = apierrors.NewForbidden(schema.GroupResource{Group: "network.hypersurgery.dev", Resource: "subnetclaims"},
		"retrying", errors.New("no access"))
	code, out, _ = f.run("--scope", "landing-zone", "--remove-commands")
	if code != 0 || strings.Contains(out, "az tag update --resource-id '"+hubID+"' --operation Delete --tags "+
		"'hs-subnet-retrying") || !strings.Contains(out, "# hs-subnet-retrying: SubnetClaim team-a/retrying could not be "+
		"read (") || !strings.Contains(out, "no command proposed") {
		t.Errorf("exit %d with a claim that cannot be read:\n%s", code, out)
	}

	clean := newOrphanFixture(t)
	if _, out, _ = clean.run("--subscription", orphanSubscription, "--remove-commands"); strings.Contains(out, "az tag") ||
		!strings.Contains(out, "# No orphaned ownership entries on the 1 virtual networks read.") {
		t.Errorf("a script for nothing orphaned:\n%s", out)
	}
}

// A subscription that cannot be read is reported and fails the command; the others are listed.
func TestAzureOrphanedEntriesReportsWhatItCouldNotRead(t *testing.T) {
	f := newOrphanFixture(t)
	f.tag("hs-subnet-deleted", "hs-owner=team-gone")
	f.objects = []client.Object{azureScope(networkv1.Account{ID: otherSubscription},
		networkv1.Account{ID: orphanSubscription})}
	code, out, errOut := f.run("--scope", "landing-zone")
	if code != 1 || !strings.Contains(errOut, "subscription "+otherSubscription+": list virtual networks:") {
		t.Errorf("exit %d, stderr %q; want the subscription that does not exist named", code, errOut)
	}
	if !strings.Contains(out, "hs-subnet-deleted") {
		t.Errorf("the subscription that could be read is not listed:\n%s", out)
	}

	code, _, errOut = f.run("--scope", "no-such-scope")
	if code != 1 || !strings.Contains(errOut, "read NetworkScope no-such-scope") {
		t.Errorf("exit %d for a scope that does not exist, stderr %q", code, errOut)
	}
	f.clusterErr = errors.New("no kubeconfig")
	code, _, errOut = f.run("--scope", "landing-zone")
	if code != 1 ||
		!strings.Contains(errOut, "cannot reach the cluster to read NetworkScope landing-zone: no kubeconfig") {
		t.Errorf("exit %d without a cluster, stderr %q", code, errOut)
	}
}

func TestAzureOrphanedEntriesFilters(t *testing.T) {
	f := newOrphanFixture(t)
	f.tag("hs-subnet-deleted", "hs-owner=team-gone")
	f.cloud.AddVirtualNetwork(orphanSubscription, "rg-other", "Spoke", "eastus", []string{"10.30.0.0/16"},
		map[string]string{"hs-subnet-spoke-gone": "hs-owner=x"})
	for _, tc := range []struct {
		args       []string
		hub, spoke bool
	}{
		{nil, true, true},
		{[]string{"--network", "SPOKE"}, false, true},
		{[]string{"--network", strings.ToUpper(hubID)}, true, false},
		{[]string{"--resource-group", "rg-other,RG-Other"}, false, true},
		{[]string{"--resource-group", orphanGroup, "--resource-group", "rg-other"}, true, true},
		{[]string{"--location", "eastus"}, false, true},
		{[]string{"--location", "westeurope", "--location", "eastus"}, true, true},
		{[]string{"--location", "westus"}, false, false},
	} {
		code, out, errOut := f.run(append([]string{"--subscription", orphanSubscription}, tc.args...)...)
		if code != 0 {
			t.Errorf("%v: exit %d, stderr %q", tc.args, code, errOut)
		}
		if hub, spoke := strings.Contains(out, "hs-subnet-deleted"), strings.Contains(out, "hs-subnet-spoke-gone"); hub !=
			tc.hub || spoke != tc.spoke {
			t.Errorf("%v: hub listed %v, spoke listed %v; want %v, %v\n%s", tc.args, hub, spoke, tc.hub, tc.spoke, out)
		}
	}
}

func TestAzureOrphanedEntriesUsage(t *testing.T) {
	f := newOrphanFixture(t)
	f.objects = []client.Object{&networkv1.NetworkScope{ObjectMeta: metav1.ObjectMeta{Name: "aws"},
		Spec: networkv1.NetworkScopeSpec{Provider: networkv1.ProviderAWS}}}
	for _, tc := range []struct {
		args []string
		says string
	}{
		{nil, "give either --scope or --subscription"},
		{[]string{"--scope", "a", "--subscription", orphanSubscription}, "give either --scope or --subscription"},
		{[]string{"--subscription", "prod"}, "is not a subscription ID"},
		{[]string{"--subscription", orphanSubscription, "extra"}, `unexpected argument "extra"`},
		{[]string{"--subscription", orphanSubscription, "--client-id", "reader"}, "are UUIDs"},
		{[]string{"--subscription", orphanSubscription, "--tenant-id", orphanReader},
			"--tenant-id is the tenant of --client-id"},
		{[]string{"--subscription", orphanSubscription, "--own-identity"}, "--own-identity goes with --scope"},
		{[]string{"--scope", "a", "--client-id", orphanReader}, "go with --subscription"},
		{[]string{"--scope", "a", "--resource-group", "rg"}, "go with --subscription"},
		{[]string{"--subscription", orphanSubscription, "-o", "yaml"}, "is not table or json"},
		{[]string{"--subscription", orphanSubscription, "-o", "json", "--remove-commands"},
			"--remove-commands prints a shell script"},
		{[]string{"--subscription", orphanSubscription, "--azure-cloud", "AzureMars"}, `unknown Azure cloud "AzureMars"`},
		{[]string{"--subscription", orphanSubscription, "--remove"}, "flag provided but not defined: -remove"},
		{[]string{"--subscription", orphanSubscription, "--yes"}, "flag provided but not defined: -yes"},
		{[]string{"--scope", "aws"}, "NetworkScope aws is of provider AWS"},
	} {
		code, out, errOut := f.run(tc.args...)
		if code != 2 || !strings.Contains(errOut, tc.says) || out != "" {
			t.Errorf("%v: exit %d, stdout %q, stderr %q; want 2 and %q", tc.args, code, out, errOut, tc.says)
		}
	}
	if requests := f.cloud.Requests(); len(requests) != 0 {
		t.Errorf("Azure was asked for a wrong command line: %v", requests)
	}
	if code, _, errOut := f.run("--help"); code != 0 || !strings.Contains(errOut, "It removes\nnothing.") ||
		!strings.Contains(errOut, "-remove-commands") {
		t.Errorf("--help: exit %d\n%s", code, errOut)
	}

	// The cloud comes from the environment the manager reads, and a flag wins over it.
	f.env["SUBNET_OPERATOR_AZURE_CLOUD"] = "AzureMars"
	if code, _, errOut := f.run("--subscription", orphanSubscription); code != 2 ||
		!strings.Contains(errOut, `unknown Azure cloud "AzureMars"`) {
		t.Errorf("a cloud from the environment: exit %d, stderr %q", code, errOut)
	}
	if code, _, errOut := f.run("--subscription", orphanSubscription, "--azure-cloud", "AzureChina"); code != 0 {
		t.Errorf("a flag over the environment: exit %d, stderr %q", code, errOut)
	}
}
