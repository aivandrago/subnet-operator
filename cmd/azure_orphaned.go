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
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkv1 "hypersurgery.dev/subnet-operator/api/v1"
	azurecloud "hypersurgery.dev/subnet-operator/internal/cloud/azure"
	"hypersurgery.dev/subnet-operator/internal/inventory"
	"hypersurgery.dev/subnet-operator/internal/provider"
)

// `manager azure-orphaned-entries` (#121) lists the ownership entries on Azure virtual networks
// whose subnet does not exist. A subnet's ownership is the tag hs-subnet-<subnet name> on its
// virtual network, the operator never removes one, and each counts against the 50 tags the
// network may carry, so somebody has to be able to find the ones that name nothing.
//
// The command reads and never writes. It makes the one listing call discovery makes, as the
// identity discovery uses, so the reader role is all it needs. For removing an entry it prints
// the az command (--remove-commands) and runs nothing: the operator's promise is that it never
// deletes anything in a cloud and never removes a tag, and a flag that made this binary do it
// would turn that promise into "unless asked". A person with az and the Tag Contributor role
// removes a tag; the operator's identities do not, and the writer role keeps no reason to.

// orphanedEntriesName is the subcommand's name.
const orphanedEntriesName = "azure-orphaned-entries"

// Claim states of an entry that was written for a SubnetClaim, known with --scope only.
const (
	claimExists   = "Exists"
	claimNotFound = "NotFound"
	claimUnknown  = "Unknown"
)

// Output formats.
const (
	outputTable = "table"
	outputJSON  = "json"
)

// idPattern is the shape of a subscription, client or tenant ID.
var idPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// orphanedEntriesCommand is the subcommand with what it reaches the outside through, which
// tests replace.
type orphanedEntriesCommand struct {
	stdout, stderr io.Writer
	getenv         func(string) string
	now            func() time.Time
	// options are the Azure provider's options for the cloud the flags name; tests point them
	// at the in-repo fake.
	options func(azurecloud.Options) azurecloud.Options
	// cluster reads the cluster the kubeconfig or the pod's service account names. It is only
	// called with --scope.
	cluster func() (client.Reader, error)
}

// azureOrphanedEntries is `manager azure-orphaned-entries`; it returns the exit code.
func azureOrphanedEntries(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	c := &orphanedEntriesCommand{
		stdout: os.Stdout, stderr: os.Stderr, getenv: os.Getenv, now: time.Now,
		options: func(o azurecloud.Options) azurecloud.Options { return o },
		cluster: func() (client.Reader, error) {
			cfg, err := ctrl.GetConfig()
			if err != nil {
				return nil, err
			}
			return client.New(cfg, client.Options{Scheme: scheme})
		},
	}
	return c.run(ctx, args)
}

// listFlag is a flag that may be given several times, and holds several values separated by
// commas each time.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(value string) error {
	for v := range strings.SplitSeq(value, ",") {
		if v = strings.TrimSpace(v); v != "" {
			*l = append(*l, v)
		}
	}
	return nil
}

// reportedEntry is an orphaned entry with what the command adds to it.
type reportedEntry struct {
	azurecloud.OrphanedEntry
	// ClaimState says whether the SubnetClaim the entry names exists; only with --scope.
	ClaimState string `json:"claimState,omitempty"`
	// Note says what to know before removing the entry, or why no command is proposed.
	Note string `json:"note,omitempty"`
	// RemoveCommand is the az command that removes the entry; empty when none is proposed.
	RemoveCommand string `json:"removeCommand,omitempty"`
}

// reportedNetwork is a virtual network that carries orphaned entries.
type reportedNetwork struct {
	azurecloud.NetworkOwnershipEntries
	Orphaned []reportedEntry `json:"orphaned"`
}

// orphanReport is what the command prints.
type orphanReport struct {
	// ReadAt is when the virtual networks were read: what is orphaned may have changed since.
	ReadAt time.Time `json:"readAt"`
	// NetworksRead is the number of virtual networks read, with or without orphaned entries.
	NetworksRead    int               `json:"networksRead"`
	OrphanedEntries int               `json:"orphanedEntries"`
	Networks        []reportedNetwork `json:"networks"`
	// Errors are the subscriptions that could not be read.
	Errors []string `json:"errors,omitempty"`
}

const orphanedEntriesUsage = `Usage:
  manager azure-orphaned-entries --scope <NetworkScope> [flags]
  manager azure-orphaned-entries --subscription <ID> [--resource-group <name>]... [flags]

Lists the ownership entries on Azure virtual networks whose subnet does not exist.

An Azure subnet cannot carry tags, so its ownership is the tag hs-subnet-<subnet name> on its
virtual network. The operator never removes one. An entry therefore stays when its subnet is
deleted, when the subnet is created again under another name, and when a create wrote the entry
and never succeeded; each counts against the 50 tags a virtual network may carry.

The command only reads: the virtual networks with their subnets and tags, with the call
discovery makes, so the reader role (deploy/azure/reader-role.json) is enough. It removes
nothing. --remove-commands prints the az commands that would, for you to read and run.

With --scope it reads the NetworkScope from the cluster (kubeconfig, or the pod's service
account) and every subscription of it, in the scope's resource groups and locations, as the
read identity the scope names for each (azure.clientID), which needs the operator's service
account token: run it in the operator's pod. On a workstation add --own-identity to read as
yourself (az login). An entry written for a SubnetClaim that still exists is listed and no
command is proposed for it: a create that failed and is retried uses that entry.

With --subscription nothing is read from a cluster, and the subscription is read with the
credentials of the environment (az login, Workload ID, a managed identity), or as --client-id
when the operator's service account token is there to exchange.

Exit status: 0 when everything was read, whether or not orphaned entries were found; 1 when a
subscription could not be read; 2 for wrong usage.

Flags:
`

// orphanFlags are the subcommand's flags.
type orphanFlags struct {
	scope, subscription, clientID, tenantID, network, output string
	azureCloud, authorityHost                                string
	groups, locations                                        listFlag
	ownIdentity, removeCommands                              bool
}

// errUsage is a command line the subcommand refuses; errors.Is(err, flag.ErrHelp) is --help.
type errUsage struct{ message string }

func (e *errUsage) Error() string { return e.message }

func usagef(format string, a ...any) error { return &errUsage{message: fmt.Sprintf(format, a...)} }

// parse reads the command line, with the cloud from the environment where no flag names it.
func (c *orphanedEntriesCommand) parse(args []string) (*orphanFlags, error) {
	fs := flag.NewFlagSet(orphanedEntriesName, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	f := &orphanFlags{}
	fs.StringVar(&f.scope, "scope", "", "The NetworkScope whose subscriptions are read, from the cluster.")
	fs.StringVar(&f.subscription, "subscription", "", "The subscription to read, without a cluster.")
	fs.Var(&f.groups, "resource-group", "With --subscription: only the virtual networks of this resource group. "+
		"May be repeated or hold several, separated by commas; the reader role is then needed on these groups only.")
	fs.StringVar(&f.clientID, "client-id", "", "With --subscription: the client ID to read as, exchanged for the "+
		"operator's service account token (AZURE_FEDERATED_TOKEN_FILE). Empty: the credentials of the environment.")
	fs.StringVar(&f.tenantID, "tenant-id", "", "With --client-id: its tenant, when not AZURE_TENANT_ID.")
	fs.BoolVar(&f.ownIdentity, "own-identity", false, "With --scope: read every subscription with the credentials "+
		"of the environment instead of the read identities the scope names.")
	fs.Var(&f.locations, "location", "Only the virtual networks in this location (westeurope). May be repeated or "+
		"hold several. Default: the scope's locations with --scope, every location with --subscription.")
	fs.StringVar(&f.network, "network", "", "Only this virtual network: its name or its resource ID, in any case.")
	fs.StringVar(&f.output, "output", outputTable, "Output format: table or json.")
	fs.StringVar(&f.output, "o", outputTable, "Short for --output.")
	fs.BoolVar(&f.removeCommands, "remove-commands", false, "Print, instead of the table, the az commands that "+
		"remove the orphaned entries in the operator's own format, one tag each, as a shell script. Nothing is run.")
	fs.StringVar(&f.azureCloud, "azure-cloud", "AzurePublic",
		"The cloud: AzurePublic, AzureUSGovernment or AzureChina.")
	fs.StringVar(&f.authorityHost, "azure-authority-host", "", "Replaces the cloud's Microsoft Entra authority host.")
	cloudFlags := []string{"azure-cloud", "azure-authority-host"}
	describeEnv(fs, cloudFlags)
	fs.Usage = func() {
		_, _ = fmt.Fprint(fs.Output(), orphanedEntriesUsage)
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, err
		}
		// The flag package has said what is wrong.
		return nil, &errUsage{}
	}
	given := map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { given[fl.Name] = true })
	if fs.NArg() > 0 {
		return nil, usagef("unexpected argument %q", fs.Arg(0))
	}
	if err := f.check(given); err != nil {
		return nil, err
	}
	// The cloud may come from the environment, as for the manager: in the operator's pod the
	// subcommand then talks to the cloud the operator does.
	if _, err := applyEnv(fs, cloudFlags, c.getenv); err != nil {
		return nil, usagef("%v", err)
	}
	return f, nil
}

// check refuses the combinations of flags that say nothing or two things.
func (f *orphanFlags) check(given map[string]bool) error {
	isID := idPattern.MatchString
	switch {
	case (f.scope == "") == (f.subscription == ""):
		return usagef("give either --scope or --subscription")
	case f.scope != "" && (given["resource-group"] || given["client-id"] || given["tenant-id"]):
		return usagef("--resource-group, --client-id and --tenant-id go with --subscription; with --scope they are " +
			"the scope's")
	case f.subscription != "" && f.ownIdentity:
		return usagef("--own-identity goes with --scope; with --subscription leave --client-id out")
	case f.subscription != "" && !isID(f.subscription):
		return usagef("--subscription %q is not a subscription ID (a UUID)", f.subscription)
	case f.clientID != "" && !isID(f.clientID), f.tenantID != "" && !isID(f.tenantID):
		return usagef("--client-id and --tenant-id are UUIDs")
	case f.tenantID != "" && f.clientID == "":
		return usagef("--tenant-id is the tenant of --client-id")
	case f.output != outputTable && f.output != outputJSON:
		return usagef("--output %q is not table or json", f.output)
	case f.removeCommands && (given["o"] || given["output"]):
		return usagef("--remove-commands prints a shell script; leave --output out (the commands are in the JSON too)")
	}
	return nil
}

func (c *orphanedEntriesCommand) run(ctx context.Context, args []string) int {
	report, f, err := c.read(ctx, args)
	usage, isUsage := errors.AsType[*errUsage](err)
	switch {
	case errors.Is(err, flag.ErrHelp):
		return 0
	case isUsage:
		if usage.message != "" {
			_, _ = fmt.Fprintf(c.stderr, "%s: %s\n", orphanedEntriesName, usage.message)
		}
		_, _ = fmt.Fprintln(c.stderr, "Run 'manager "+orphanedEntriesName+" --help' for the flags.")
		return 2
	case err != nil:
		_, _ = fmt.Fprintf(c.stderr, "%s: %v\n", orphanedEntriesName, err)
		return 1
	}
	switch {
	case f.removeCommands:
		printRemoveCommands(c.stdout, *report)
	case f.output == outputJSON:
		enc := json.NewEncoder(c.stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		if err := enc.Encode(report); err != nil {
			_, _ = fmt.Fprintf(c.stderr, "%s: %v\n", orphanedEntriesName, err)
			return 1
		}
	default:
		printOrphanTable(c.stdout, *report)
	}
	for _, e := range report.Errors {
		_, _ = fmt.Fprintf(c.stderr, "%s: %s\n", orphanedEntriesName, e)
	}
	if len(report.Errors) > 0 {
		return 1
	}
	return 0
}

// read parses the command line and reads what it asks for. A subscription that cannot be read
// is in the report's errors, and the others are still read.
func (c *orphanedEntriesCommand) read(ctx context.Context, args []string) (*orphanReport, *orphanFlags, error) {
	f, err := c.parse(args)
	if err != nil {
		return nil, nil, err
	}
	cloudConfig, err := azureEndpoints(f.azureCloud, f.authorityHost)
	if err != nil {
		return nil, nil, usagef("%v", err)
	}
	p := azurecloud.NewProvider(c.options(azurecloud.Options{Cloud: cloudConfig, AuthorityHost: f.authorityHost}))
	reads, cluster, err := c.reads(ctx, f, p)
	if err != nil {
		return nil, nil, err
	}
	report := &orphanReport{ReadAt: c.now().UTC().Truncate(time.Second), Networks: []reportedNetwork{}}
	for _, r := range reads {
		networks, err := p.OwnershipEntries(ctx, r.target, r.locations)
		if err != nil {
			report.Errors = append(report.Errors, fmt.Sprintf("subscription %s: %v", r.target.Account, err))
			continue
		}
		for _, n := range networks {
			if f.network != "" && !strings.EqualFold(f.network, n.Name) && !strings.EqualFold(f.network, n.ID) {
				continue
			}
			report.NetworksRead++
			if len(n.Orphaned) == 0 {
				continue
			}
			rn := reportedNetwork{NetworkOwnershipEntries: n}
			for _, e := range n.Orphaned {
				rn.Orphaned = append(rn.Orphaned, c.judge(ctx, cluster, n.ID, e))
			}
			rn.NetworkOwnershipEntries.Orphaned = nil
			report.OrphanedEntries += len(rn.Orphaned)
			report.Networks = append(report.Networks, rn)
		}
	}
	return report, f, nil
}

// reads are the subscriptions the command line asks for: the one it names, or the scope's, with
// the cluster the scope was read from, which is nil without --scope.
func (c *orphanedEntriesCommand) reads(ctx context.Context, f *orphanFlags, p *azurecloud.Provider) (
	[]orphanRead, client.Reader, error) {
	if f.scope == "" {
		target := inventory.Target{Provider: networkv1.ProviderAzure,
			Account: networkv1.CanonicalAccountID(networkv1.ProviderAzure, f.subscription)}
		if f.clientID != "" {
			target.Identity = p.Identity(networkv1.Account{ID: f.subscription,
				Azure: &networkv1.AzureAccount{ClientID: f.clientID, TenantID: f.tenantID}}, provider.Read)
		}
		if len(f.groups) > 0 {
			target.Azure = &networkv1.AzureScope{ResourceGroups: f.groups}
		}
		return []orphanRead{{target: target, locations: f.locations}}, nil, nil
	}
	cluster, err := c.cluster()
	if err != nil {
		return nil, nil, fmt.Errorf("cannot reach the cluster to read NetworkScope %s: %w", f.scope, err)
	}
	scope := &networkv1.NetworkScope{}
	if err := cluster.Get(ctx, types.NamespacedName{Name: f.scope}, scope); err != nil {
		return nil, nil, fmt.Errorf("read NetworkScope %s: %w", f.scope, err)
	}
	if scope.Spec.Provider != networkv1.ProviderAzure {
		return nil, nil, usagef("NetworkScope %s is of provider %s; ownership entries are Azure's", f.scope,
			scope.Spec.Provider)
	}
	return scopeReads(scope, p, f.ownIdentity, f.locations), cluster, nil
}

// orphanRead is one subscription to read, as an identity, in some locations.
type orphanRead struct {
	target    inventory.Target
	locations []string
}

// scopeReads are the reads of a scope: each of its subscriptions once, in the scope's resource
// groups, as the read identity discovery uses for it (or the caller's own), in the locations
// the scope discovers there unless the caller names others.
func scopeReads(scope *networkv1.NetworkScope, p provider.Provider, ownIdentity bool, locations []string) []orphanRead {
	var out []orphanRead
	seen := map[string]bool{}
	for _, a := range scope.Spec.Accounts {
		account := networkv1.CanonicalAccountID(scope.Spec.Provider, a.ID)
		if seen[account] {
			continue
		}
		seen[account] = true
		r := orphanRead{locations: locations, target: inventory.Target{Provider: scope.Spec.Provider, Scope: scope.Name,
			Account: account, Azure: scope.Spec.Azure}}
		if !ownIdentity {
			r.target.Identity = p.Identity(a, provider.Read)
		}
		if len(r.locations) == 0 {
			r.locations = a.Regions
		}
		if len(r.locations) == 0 {
			r.locations = scope.Spec.Regions
		}
		out = append(out, r)
	}
	return out
}

// judge adds to an orphaned entry what the command knows about removing it. No command is
// proposed for a tag that is not in the operator's own format, which somebody else wrote, nor,
// when the cluster can be asked, for an entry whose SubnetClaim exists or could not be read:
// a create writes its entry before the subnet, so the entry of a create that failed and will be
// tried again looks orphaned, and removing it would leave that subnet without its owner.
func (c *orphanedEntriesCommand) judge(ctx context.Context, cluster client.Reader, networkID string,
	e azurecloud.OrphanedEntry) reportedEntry {
	out := reportedEntry{OrphanedEntry: e}
	if !e.OperatorFormat {
		out.Note = "not in the operator's format (written or edited by something else): no command proposed"
		return out
	}
	namespace, name, isClaim := strings.Cut(e.Claim, "/")
	switch {
	case e.Claim == "":
	case cluster == nil || !isClaim || namespace == "" || name == "":
		out.Note = fmt.Sprintf("written for SubnetClaim %s: remove it only if that claim is gone or will never "+
			"create this subnet", e.Claim)
	default:
		err := cluster.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &networkv1.SubnetClaim{})
		switch {
		case err == nil:
			out.ClaimState = claimExists
			out.Note = fmt.Sprintf("SubnetClaim %s exists and may still create this subnet: no command proposed", e.Claim)
			return out
		case apierrors.IsNotFound(err):
			out.ClaimState = claimNotFound
			out.Note = fmt.Sprintf("written for SubnetClaim %s, which is gone", e.Claim)
		default:
			out.ClaimState = claimUnknown
			out.Note = fmt.Sprintf("SubnetClaim %s could not be read (%v): no command proposed", e.Claim, err)
			return out
		}
	}
	out.RemoveCommand = azurecloud.RemoveCommand(networkID, e)
	return out
}

func printOrphanTable(w io.Writer, report orphanReport) {
	for _, n := range report.Networks {
		_, _ = fmt.Fprintf(w, "%s (%s): %d of %d tags, %d ownership entries, %d orphaned\n", n.ID, n.Location, n.TagCount,
			n.TagLimit, n.OwnershipEntries, len(n.Orphaned))
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "  ENTRY\tSAYS\tNOTE")
		for _, e := range n.Orphaned {
			_, _ = fmt.Fprintf(tw, "  %s\t%s\t%s\n", e.Tag, e.Value, e.Note)
		}
		_ = tw.Flush()
		_, _ = fmt.Fprintln(w)
	}
	if report.OrphanedEntries == 0 {
		_, _ = fmt.Fprintf(w, "No orphaned ownership entries on the %d virtual networks read.\n", report.NetworksRead)
		return
	}
	_, _ = fmt.Fprintf(w, "%d orphaned ownership entries on %d of the %d virtual networks read. Nothing was changed; "+
		"--remove-commands prints the az commands that remove them.\n", report.OrphanedEntries, len(report.Networks),
		report.NetworksRead)
}

// printRemoveCommands prints the az commands as a shell script, with what to know before
// running one as its comments. A line of an entry no command is proposed for is a comment.
func printRemoveCommands(w io.Writer, report orphanReport) {
	_, _ = fmt.Fprintf(w, `# Ownership entries (hs-subnet-<subnet name>) whose subnet did not exist when the virtual
# networks were read, at %s. Nothing has been removed: the operator never removes a
# tag, and neither did this command. Each command below removes one tag from one virtual network,
# as you (it needs Microsoft.Resources/tags/write there, e.g. Tag Contributor), and cannot be
# undone other than by writing the tag again.
#
# Before running one: the subnet may have been created, or the entry changed, since the read,
# and nothing makes az check: the Tags API has no precondition. A command names the entry with
# the value it had then; whether Azure leaves an entry alone whose value has changed since has
# not been verified, so do not count on it. List again right before, and run only the commands
# you have read. Removing the entry of a subnet that is being created leaves that subnet
# without its owner.
`, report.ReadAt.Format(time.RFC3339))
	for _, n := range report.Networks {
		_, _ = fmt.Fprintf(w, "\n# %s (%s): %d of %d tags, %d orphaned\n", n.ID, n.Location, n.TagCount, n.TagLimit,
			len(n.Orphaned))
		for _, e := range n.Orphaned {
			if e.RemoveCommand == "" {
				_, _ = fmt.Fprintf(w, "# %s: %s\n", e.Tag, commentLine(e.Note))
				continue
			}
			if e.Note != "" {
				_, _ = fmt.Fprintf(w, "# %s: %s\n", e.Tag, commentLine(e.Note))
			}
			_, _ = fmt.Fprintln(w, e.RemoveCommand)
		}
	}
	if report.OrphanedEntries == 0 {
		_, _ = fmt.Fprintf(w, "\n# No orphaned ownership entries on the %d virtual networks read.\n", report.NetworksRead)
	}
}

// commentLine keeps a text on the one comment line it is printed on.
func commentLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
