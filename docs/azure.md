# Azure

The Azure provider is what 3.0 adds ([ADR 0002](adr/0002-multi-cloud-model.md), milestone
"3.0 — Azure"), released in 3.0.0. The
[upgrade guide](operations/upgrades.md#upgrading-from-2x-to-30) lists what 3.0 changes for an
existing installation: nothing for its AWS and GCP scopes.

The provider has been tested against fakes (an in-repository fake of Azure Resource Manager,
Microsoft Entra ID's token endpoint and the Storage queue API, driven through the real Azure SDK
clients), **not yet against a real subscription**. That conformance run is #56. What only a
subscription can confirm is marked where it applies below: whether the roles in `deploy/azure`
hold every action Azure asks for ([Roles](#4-roles)), the error codes the operator recognises,
and the fields of the events ([Change events](#change-events)). So start read-only, and expect
that a role may need an action its file does not list yet. If you try the provider in a
subscription of your own, a report of what worked and what did not is the most useful
contribution there is ([CONTRIBUTING.md](../CONTRIBUTING.md)).

- [What the provider does](#what-the-provider-does)
- [1. Enable the provider](#1-enable-the-provider)
- [2. The operator's own Azure identity](#2-the-operators-own-azure-identity)
- [3. Read and write identities per subscription](#3-read-and-write-identities-per-subscription)
- [4. Roles](#4-roles)
- [5. Ownership: tags on the virtual network](#5-ownership-tags-on-the-virtual-network)
- [6. A first scope, claim and import](#6-a-first-scope-claim-and-import)
- [7. Verify](#7-verify)
- [Change events](#change-events)
- [How Azure differs from AWS and GCP](#how-azure-differs-from-aws-and-gcp)
- [Known gaps in 3.0](#known-gaps-in-30)

## What the provider does

| | AWS (from 0.1) | GCP (from 2.0) | Azure (from 3.0) |
|---|---|---|---|
| Account | AWS account, 12 digits | project ID | subscription ID, a UUID in any case |
| Network | VPC, regional, has CIDR blocks | VPC network, global, no address space of its own | virtual network, regional, has an address space |
| Subnet | subnet, zonal | subnetwork, regional | subnet, **regional, no zone**, one or more address prefixes |
| Region | `eu-central-1` | `europe-west1` | location, as ARM spells it: `westeurope` |
| Ownership | resource tags `hs/owner`, `hs/env`, `hs/tier` | Resource Manager tags bound to the network or subnetwork: `hs-owner`, `hs-env`, `hs-tier` | a virtual network's own tags `hs-owner`, `hs-env`, `hs-tier`; **a subnet cannot carry tags**, so its ownership is the tag `hs-subnet-<subnet name>` on its virtual network |
| Identity per account | `accounts[].aws.roleARN` / `writeRoleARN`, assumed | `accounts[].gcp.serviceAccount` / `writeServiceAccount`, impersonated | `accounts[].azure.clientID` / `writeClientID` (in `tenantID`), each trusting the operator's service account through a federated identity credential |
| Operator's own identity | EKS Pod Identity or IRSA | GKE Workload Identity or Workload Identity Federation | Microsoft Entra Workload ID, on AKS or any other cluster |
| Change events (optional) | CloudTrail → EventBridge → SQS | Admin Activity audit logs → log sink → Pub/Sub | Event Grid system topic → Storage queue |
| Who created a resource (`fromCreator` rules) | from CloudTrail | from the audit log entry | **not known**: an event does not say whether it created or updated |
| `status.capabilities` | `CreateSubnet`, `ChangeEvents` (with events), `IPUsage` | the same | the same |

Discovery reads, per subscription, its virtual networks with their subnets and tags (the
`virtualNetworks` List All call, or one List call per resource group when the scope names
`spec.azure.resourceGroups`), keeps the ones in the target's location, and reads the addresses
used per subnet of each (`virtualNetworks/{name}/usages`). With writes on, a `SubnetClaim` in
`Create` mode writes the new subnet's ownership on the virtual network and then creates the
subnet (a `subnets` PUT with `If-None-Match: *`), and a `ResourceImport` reads its virtual
network (that it and the subnet exist, and where they are) and adds tags through the Tags API
(`Merge`). With change events set up, the leader also polls a Storage queue of Event
Grid resource events and resyncs the subscription and location a change names. Nothing on
Azure is ever deleted, and no existing subnet or tag value is ever changed: the only delete in
`internal/cloud/azure` is of the operator's own messages on its queue.

## 1. Enable the provider

The provider runs when the chart's `providers.azure.enabled` is set (the manager's
`--providers=aws,azure`). AWS stays enabled unless you turn it off; a chart with no provider
refuses to render. For an Azure-only installation on AKS:

```yaml
# values-azure.yaml (the whole file is examples/values-azure.yaml)
providers:
  aws:
    enabled: false
  azure:
    enabled: true
    workloadIdentity:
      enabled: true
      clientId: 3f0c6a1e-0000-4000-8000-0000000000a1
```

```sh
helm install subnet-operator hypersurgery/subnet-operator \
  -n subnet-operator-system --create-namespace -f values-azure.yaml
```

The chart has `providers.azure` from 3.0.0; a 2.x chart does not know it.

A `NetworkScope` with `provider: Azure` on an operator that does not run the provider is reported
`Ready=False` with reason `ProviderNotEnabled` and is not synced.

`providers.azure.cloud` (`--azure-cloud`) selects the cloud: `AzurePublic` (the default),
`AzureUSGovernment` or `AzureChina`, each with its own Resource Manager and Microsoft Entra
endpoints. `providers.azure.authorityHost` (`--azure-authority-host`) replaces the Entra
authority host alone. Under OLM, which passes no arguments, these and the change event settings
are `SUBNET_OPERATOR_AZURE_*` environment variables ([docs/olm.md](olm.md#settings-through-the-subscription)).

## 2. The operator's own Azure identity

Every identity the operator uses is a **user-assigned managed identity** or an **app
registration** with a **federated identity credential** that trusts the operator's Kubernetes
service account. The operator holds no client secret and no certificate: it exchanges its
projected service account token at Microsoft Entra ID for a token of the identity
(Microsoft Entra Workload ID). The federated identity credential names

- the **issuer**: the cluster's service account issuer (its OIDC issuer URL),
- the **subject**: `system:serviceaccount:<release namespace>:<fullname>`,
  `system:serviceaccount:subnet-operator-system:subnet-operator` for the commands in this guide,
- the **audience**: `api://AzureADTokenExchange`.

```sh
ISSUER=$(az aks show --resource-group rg-aks --name prod --query oidcIssuerProfile.issuerUrl -o tsv)
az identity create --resource-group rg-identities --name subnet-operator
az identity federated-credential create --resource-group rg-identities --identity-name subnet-operator \
  --name subnet-operator --issuer "$ISSUER" \
  --subject system:serviceaccount:subnet-operator-system:subnet-operator \
  --audiences api://AzureADTokenExchange
az identity show --resource-group rg-identities --name subnet-operator --query clientId -o tsv
```

[`deploy/azure/README.md`](../deploy/azure/README.md) has the same with Terraform. A new
federated credential takes a few minutes to propagate; until then Entra answers `AADSTS70021`
and the target is retried at the next sync.

### Workload Identity on AKS

On AKS with the OIDC issuer and the workload identity add-on enabled, set
`providers.azure.workloadIdentity.enabled: true` and the identity's client ID in
`workloadIdentity.clientId`. The chart labels the pod `azure.workload.identity/use: "true"` and
writes the client ID (and `providers.azure.tenantId`, when set) as the service account's
`azure.workload.identity/client-id` and `tenant-id` annotations; the add-on's webhook then
projects the token and sets `AZURE_CLIENT_ID`, `AZURE_TENANT_ID` and
`AZURE_FEDERATED_TOKEN_FILE`. An annotation set by hand in `serviceAccount.annotations` wins.

### Federated from another cluster

On EKS, GKE or a self-managed cluster there is no such webhook, and the chart does its work:

```yaml
providers:
  azure:
    enabled: true
    # Required without the webhook: the tenant tokens are requested from.
    tenantId: 0f8fad5b-0000-4000-8000-00000000d0c5
    workloadIdentity:
      enabled: true
      clientId: 3f0c6a1e-0000-4000-8000-0000000000a1
      webhook: false
```

The chart projects a service account token with the audience `api://AzureADTokenExchange` at
`workloadIdentity.tokenPath` (`/var/run/secrets/azure/tokens/azure-identity-token`, valid for
`tokenExpirationSeconds`, 3600) and sets the three `AZURE_*` variables itself. The issuer of the
federated credential is that cluster's service account issuer: the EKS OIDC provider URL, or
`https://container.googleapis.com/v1/projects/<project>/locations/<location>/clusters/<cluster>`
on GKE. Entra must be able to fetch the issuer's discovery document and keys, so a self-managed
cluster needs a publicly reachable issuer.

With neither (`workloadIdentity.enabled: false`), the SDK's `DefaultAzureCredential` uses what
the environment has: `AZURE_*` variables given through `extraEnv`, or a managed identity of the
node. Identities per subscription (step 3) need Workload ID: without a token file they fail with
`the operator has no service account token to exchange`.

### NetworkPolicy

With `networkPolicy.enabled`, Microsoft Entra ID (`login.microsoftonline.com`) and Azure Resource
Manager (`management.azure.com`) are reached on 443 through the general egress rule. Where
`networkPolicy.egress.cidrs` is narrowed, add their ranges with `providers.azure.apiCIDRs`: the
`AzureActiveDirectory` and `AzureResourceManager` service tags, and with change events the
`Storage` service tag of the queue's region or its private endpoint.

## 3. Read and write identities per subscription

Azure has no AssumeRole and no impersonation API. A subscription is reached either with the
operator's own identity or with identities named on the account entry, which the operator
reaches the same way as its own: by exchanging the same service account token for a token of
that client ID. So each of them needs a federated identity credential like the one in step 2
(up to 20 per identity, one per cluster and service account), and its own role assignments:

```yaml
accounts:
  - id: 5b3c2a10-0000-4000-8000-00000000a2e1
    azure:
      # Authenticated as to read the subscription. Empty: the operator's own identity reads it.
      clientID: 3f0c6a1e-0000-4000-8000-0000000000b1
      # Authenticated as for SubnetClaims in Create mode and for ResourceImports, never for reads.
      writeClientID: 3f0c6a1e-0000-4000-8000-0000000000c1
      # Only for identities in another tenant than the operator's own.
      # tenantID: 0f8fad5b-0000-4000-8000-00000000d0c5
```

- **One identity for everything** is the default: account entries without an `azure` member are
  read, and with writes on written, by the operator's own identity, which then holds the reader
  (and writer) role in every subscription or on a management group above them. Simple, but
  discovery then carries the write permissions.
- **Read and write stay apart** with identities per account. Discovery never uses
  `writeClientID`, and writes never use `clientID`. The one read an import makes before it
  writes (of its virtual network, [below](#6-a-first-scope-claim-and-import)) is a read, and is
  `clientID`'s. Naming the same identity for both is
  accepted with a warning, because discovery then carries the write permissions. The operator's
  own identity then needs no role on the subscriptions at all, and
  `providers.azure.workloadIdentity.clientId` may stay empty (unless change events are on: the
  queue is read with it).
- **A subscription read through `clientID` but without a `writeClientID`** is not the
  operator's own, so the operator's own identity does not write there either: claims for it can
  only be allocated (reason `NoWriteRole`), and imports stay `Pending` (`NoWriteRole`). This is
  the same rule as an AWS account with `roleARN` and no `writeRoleARN`.
- **An identity may serve many subscriptions.** Unlike an AWS role it is not tied to the
  account's ID: a reader per landing zone, assigned on a management group, reads every
  subscription below it.
- **Another tenant.** Managed identities are single-tenant. Use a multitenant app registration
  in the operator's tenant with the federated credential, provision it in the other tenant
  (admin consent), assign the roles there, and name both: `azure: {clientID: <app>, tenantID:
  <other tenant>}`. Without `tenantID`, tokens are requested from the operator's own tenant
  (`providers.azure.tenantId`, `AZURE_TENANT_ID`).
- **IDs in any case.** Client, tenant and subscription IDs are UUIDs in either case; the
  operator uses them in lowercase.

If Entra refuses the token, the target fails before any Resource Manager call, with an error
that names the client ID, the issuer and subject the federated credential must name, and Entra's
`AADSTS` code; the operator never falls back to its own identity (runbook:
[SubnetInventoryTargetDown on Azure](operations/runbook.md#subnetinventorytargetdown-on-azure)).

## 4. Roles

`deploy/azure/` has one custom role definition per job, least privilege. Every action and why
it is there is in the [IAM reference](reference/iam.md#azure).

| Role file | Assign to | On | Needed for |
|---|---|---|---|
| [`reader-role.json`](../deploy/azure/reader-role.json) | each account's `azure.clientID`, or the operator's own identity | every subscription in the scope (or a management group above them), or only the resource groups a scope names in `spec.azure.resourceGroups` | discovery, and the read of the virtual network before a `ResourceImport` |
| [`writer-role.json`](../deploy/azure/writer-role.json) | each account's `azure.writeClientID`, or the operator's own identity for an account it reads itself | the subscription, or the resource groups of the virtual networks | `SubnetClaim` in `Create` mode, `ResourceImport` |
| the built-in Tag Contributor | the write identity, instead of the writer role | the same | `ResourceImport` alone: an import writes through the Tags API only |
| the built-in Storage Queue Data Message Processor | the operator's own identity | the change events queue | only with [change events](#change-events) |

A role is created once per tenant, with the subscriptions or management groups it may be
assigned in as its `AssignableScopes`, and then assigned **per subscription** (or per resource
group):

```sh
SUB=5b3c2a10-0000-4000-8000-00000000a2e1
sed "s#/subscriptions/00000000-0000-0000-0000-000000000000#/subscriptions/$SUB#" \
  deploy/azure/reader-role.json > /tmp/reader-role.json
az role definition create --role-definition @/tmp/reader-role.json
az role assignment create --assignee "$READER_CLIENT_ID" --role "Subnet operator reader" \
  --scope "/subscriptions/$SUB"

sed "s#/subscriptions/00000000-0000-0000-0000-000000000000#/subscriptions/$SUB#" \
  deploy/azure/writer-role.json > /tmp/writer-role.json
az role definition create --role-definition @/tmp/writer-role.json
az role assignment create --assignee "$WRITER_CLIENT_ID" --role "Subnet operator writer" \
  --scope "/subscriptions/$SUB/resourceGroups/rg-network-prod"
```

Neither role holds a `*/delete`, `Microsoft.Network/virtualNetworks/write` (which could change
a virtual network's address space or peerings) or anything of `Microsoft.Authorization`; the
writer writes tags through the Tags API precisely so that it needs no write access to the
virtual network. `test/deploy` checks the role files against the calls the provider makes. What
the conformance run (#56) still has to confirm: whether the reader needs `subnets/read` for
subnets that come inline with their virtual network, and whether creating a subnet in a virtual
network whose subnets must have a route table or network security group needs their
`join/action`, which the writer role does not hold.

## 5. Ownership: tags on the virtual network

A virtual network carries its ownership as its own tags, as an AWS VPC does. **An Azure subnet
cannot carry tags**, so the operator keeps a subnet's ownership on its virtual network, one tag
per subnet that has ownership of its own:

```
hs-subnet-<subnet name> = hs-claim=default/checkout;hs-env=prod;hs-managed-by=subnet-operator;hs-owner=team-payments;hs-tier=private
```

The value holds the subnet's tags as `key=value` pairs, sorted by key and separated by `;`
(`%`, `;` and `=` inside a key or value are percent-encoded). Unlike on GCP, nothing has to be
created first: a tag name and value exist once they are written.

- **A subnet without an entry inherits its network's tags** and reports
  `status.ownershipSource: Network`. A subnet with one reports `Subnet`: its tags are the
  network's with the entry's laid over them key by key, so an entry only needs what differs
  from the network, and the operator leaves out of an entry every pair the network already
  carries with the same value.
- **The entries are not the network's own tags.** They are left out of the network's
  `status.tags`, and a tag whose name starts with `hs-subnet-` is refused in a claim's or an
  import's `tags`: only the operator writes them.
- **Tag names are case-insensitive, values are not.** Azure treats `HS-Owner` and `hs-owner` as
  one name, and so does the operator on Azure: the scope's `tagKeys`, `networkSelector`,
  `requiredSubnetTags` and auto-import keys, the `hs-subnet-` entries and the names inside them
  match whatever case a tag was set in, and a matched tag is reported in the scope's spelling.
  A scope that spells one tag name two ways is refused.
- **Names.** A tag name cannot contain `<`, `>`, `%`, `&`, `\`, `?` or `/`, which is why the
  Azure keys are `hs-*` and not `hs/*`. A name is at most 512 characters, a value 256.
- **Only ever added.** A write reads the network's tags first, and a name it would write that
  already has another value refuses the whole write (`TagValueConflict`); for a subnet that
  includes what it inherits from its network, so an import cannot take over an owner the
  network gives its subnets. Replacing an owner means changing the tag by hand first.

The keys the operator uses on Azure:

| Key | Holds | Written by |
|---|---|---|
| `hs-owner`, `hs-env`, `hs-tier` | owner, environment, tier (the defaults of `spec.tagKeys`) | claims and imports |
| `hs-managed` | the managed marker (the default of `autoImport.managedTag`, and a usual `networkSelector`) | imports, the auto-import policy |
| `hs-managed-by` | `subnet-operator` on subnets the operator created | claims |
| `hs-claim` | `<namespace>/<name>` of the claim a subnet was created for, always written | claims |
| `hs-subnet-<subnet name>` | a subnet's ownership, on its virtual network | claims and imports of subnets |

### The tag budget

Azure allows **50 tags per resource**, and the entries of a virtual network's subnets share
them with the network's own tags. Two limits follow, and both refuse the whole write before
anything is written:

- `TagBudgetExceeded`: the write would need a 51st tag on the virtual network. A claim's subnet
  is not created either, since its entry is written first.
- `OwnershipEntryTooLong`: a subnet's entry would be longer than the 256 characters of one tag
  value. The webhook refuses such a claim or subnet import at apply time.

`status.azure.tagCount` and `status.azure.subnetOwnershipEntries` on the `Network`, the metric
`hs_network_tags` and the alert [`NetworkTagBudgetLow`](operations/runbook.md#networktagbudgetlow)
(45 of 50 by default, `prometheusRule.thresholds.networkTags`) show a network running out. The
operator never removes a tag: the entry of a subnet that was deleted stays until someone
removes it, and counts. The operator does not write an entry for a subnet that does not exist
(an import of one is refused, [below](#6-a-first-scope-claim-and-import)). A virtual network
with many subnets is best given its owner, env and
tier as its own tags, so that only the subnets that differ need an entry.

## 6. A first scope, claim and import

The same manifests are [`examples/10-azure-subscription.yaml`](../examples/10-azure-subscription.yaml)
and [`examples/11-azure-subnet-claim.yaml`](../examples/11-azure-subnet-claim.yaml), which the
tests send to the API server and run through the provider's validation. The scope can be applied
with `kubectl`, or created by the chart with the release (below).

With `kubectl`:

```yaml
apiVersion: network.hypersurgery.dev/v1
kind: NetworkScope
metadata:
  name: azure-landing-zone
spec:
  provider: Azure
  # Optional: only the virtual networks of these resource groups. The reader role is then
  # needed on these groups alone. Without it, the whole subscription is discovered.
  azure:
    resourceGroups: [rg-network-prod]
  accounts:
    - id: 5b3c2a10-0000-4000-8000-00000000a2e1
      azure:
        clientID: 3f0c6a1e-0000-4000-8000-0000000000b1
        writeClientID: 3f0c6a1e-0000-4000-8000-0000000000c1
  regions: [westeurope, northeurope]
  networkSelector:
    matchTags:
      hs-managed: "true"
  requiredSubnetTags: [hs-owner, hs-env]
  # Claims and imports may use the scope from the default namespace only (the one the claim
  # and import below live in); name your teams' namespaces here.
  namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: default
  resyncInterval: 10m
```

Or with the chart, next to the provider settings of [step 1](#1-enable-the-provider); the whole
file is [`examples/values-azure.yaml`](../examples/values-azure.yaml):

```yaml
networkScope:
  create: true
  name: azure-landing-zone
  provider: Azure
  azure:
    resourceGroups: [rg-network-prod]
  accounts:
    - id: 5b3c2a10-0000-4000-8000-00000000a2e1
      azure:
        clientID: 3f0c6a1e-0000-4000-8000-0000000000b1
        writeClientID: 3f0c6a1e-0000-4000-8000-0000000000c1
  regions: [westeurope, northeurope]
  networkSelector:
    matchTags:
      hs-managed: "true"
  requiredSubnetTags: [hs-owner, hs-env]
  namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: default
  resyncInterval: 10m
```

The chart refuses to render an Azure scope without `providers.azure.enabled`, with a tag name
containing `/`, with an account that has an `aws` or `gcp` member, and with two resource groups
that differ only in case. Its default `networkSelector.matchTags` and `requiredSubnetTags` are
the AWS keys; on Azure it renders them as `hs-managed` and `hs-owner`, `hs-env`, `hs-tier`.

`regions` are Azure locations as ARM spells them: lowercase, without spaces (`westeurope`, not
`West Europe`). A resource group that does not exist fails the subscription's targets
(`ResourceGroupNotFound` in `status.targets[].error`), like a subscription that does not.

A claim on Azure is for **one** subnet: no `zones`, since an Azure subnet is regional. Its range
is carved from the virtual network's address space, as on AWS, leaving out every prefix the
network's subnets already use and every other claim's reservation. `namePrefix` (default: the
claim's name) is the subnet's name; a name Azure reserves for a service of its own
(`GatewaySubnet`, `AzureFirewallSubnet`, `AzureFirewallManagementSubnet`, `AzureBastionSubnet`,
`RouteServerSubnet`) is refused. The prefix length is between /2 and /29.

```yaml
apiVersion: network.hypersurgery.dev/v1
kind: SubnetClaim
metadata:
  name: checkout
  namespace: default
spec:
  scopeRef: azure-landing-zone
  account: 5b3c2a10-0000-4000-8000-00000000a2e1
  region: westeurope
  networkID: /subscriptions/5b3c2a10-0000-4000-8000-00000000a2e1/resourceGroups/rg-network-prod/providers/Microsoft.Network/virtualNetworks/vnet-prod
  prefixLength: 24
  mode: Create
  owner: team-payments
  env: prod
  tier: private
  namePrefix: payments-checkout
```

With `mode: Create` the operator first writes the entry `hs-subnet-payments-checkout` on the
virtual network, `hs-claim=default/checkout` included, and then creates the subnet with nothing
but its address prefix: no route table, network security group, delegation or service endpoint
is set. The entry goes first so that the subnet never exists without its owner; if the create
then fails, the entry stays, names no subnet yet, and is used again by the next attempt. A claim
that lost its status finds its subnet again by its name, its prefix and its `hs-claim`. **The
operator never changes an existing subnet**: a name that is taken by anything else fails the
claim with `exists already … choose another namePrefix`.

An import adds tags to an existing virtual network (its own tags) or to an existing subnet (its
entry on the virtual network):

```yaml
apiVersion: network.hypersurgery.dev/v1
kind: ResourceImport
metadata:
  name: legacy-db-import
  namespace: default
spec:
  scopeRef: azure-landing-zone
  account: 5b3c2a10-0000-4000-8000-00000000a2e1
  region: westeurope
  resourceID: /subscriptions/5b3c2a10-0000-4000-8000-00000000a2e1/resourceGroups/rg-network-prod/providers/Microsoft.Network/virtualNetworks/vnet-prod/subnets/legacy-db
  tags:
    hs-owner: team-data
    hs-tier: db
  requestedBy: platform team (ticket NET-733)
```

**The operator reads the virtual network before it writes**, once per attempt, as the account's
read identity (`virtualNetworks` Get; an import that is `Applied` reads nothing more). The Tags
API would write `hs-subnet-<name>` whether or not the virtual network has a subnet of that
name, and the entry of a misspelt subnet would stay and count against the
[tag budget](#the-tag-budget). So:

- A virtual network that does not exist, or one that has no subnet of that name, fails the
  import with reason `ResourceNotFound` (`… has no subnet "legacy-bd" (it has apps,
  legacy-db)`), and nothing is written. It is retried every minute like every failed import, so
  an import applied before its subnet exists goes through once it does. A dry run
  (`spec.dryRun`) reads too and reports the same.
- The webhook cannot ask Azure, so at apply time it only warns: specifically when the virtual
  network is in the inventory and had no such subnet at its last sync, generally (`is not in the
  inventory`) when the virtual network itself is not.

**`spec.region` is the virtual network's location, and you may leave it out** for a virtual
network the operator has discovered: the mutating webhook fills it in from the inventory, so
the stored import, its `network.hypersurgery.dev/region` label and its audit lines carry the
real location. One that contradicts the discovered network is refused at apply time. For a
virtual network that is not in the inventory (one the scope's `networkSelector` leaves out,
which is what an import of a network is often for) the operator does not know the location
yet, and the webhook requires `spec.region`; the controller then checks it against what Azure
says and fails the import with `RegionMismatch` if it is another. An import created without a
region while the webhooks were off is applied with the location the controller read, provided
the scope lists it (`AccountNotInScope` otherwise). `spec.region` cannot be changed once set.

**Resource IDs may be written in any case**, as the portal shows them or in lowercase as
`kubectl get hsnet` does: ARM compares IDs without regard to case. The mutating webhook rewrites
a claim's `networkID`, an import's `resourceID` and their `account` to lowercase, which is the
spelling the operator uses everywhere (`spec.id` of `Network` and `Subnet`, labels, metrics). A
GitOps tool that compares the stored object with the manifest field by field reports an ID
written in another case as out of sync: write it in lowercase there.

Writes need `--enable-writes` (`writes.enabled: true` in the chart) as on AWS and GCP.

## 7. Verify

```sh
kubectl get nscope azure-landing-zone
kubectl get nscope azure-landing-zone -o jsonpath='{range .status.targets[*]}{.account}/{.region}{"\t"}{.error}{"\n"}{end}'
kubectl get hsnet -l network.hypersurgery.dev/provider=azure
kubectl get hssubnet -l network.hypersurgery.dev/account=5b3c2a10-0000-4000-8000-00000000a2e1,network.hypersurgery.dev/region=westeurope -o wide
kubectl get hsnet <object-name> -o jsonpath='{.status.azure}{"\n"}'   # resourceGroup, tagCount, subnetOwnershipEntries
```

`READY` is `True` (reason `Synced`) once every subscription/location pair was read. `SyncFailed`
means one could not be read; `status.targets[].error` says why, and the
[runbook](operations/runbook.md#subnetinventorytargetdown-on-azure) has the Azure causes.
`Network` and `Subnet` objects of Azure are named `<name>-<10 hex digits>` (a resource ID is not
a valid object name), and their `spec.id` is the resource ID in lowercase. The resource group,
as Azure spells it, is in `status.azure.resourceGroup`. A subnet's `status.ownershipSource` says
whether its tags are its own entry's (`Subnet`) or inherited (`Network`); its further address
prefixes are in `status.secondaryCIDRBlocks` and counted in `totalIPs`; and `status.azure` has
its delegations, service endpoints, route table, network security group and NAT gateway, and
where `availableIPs` came from (`ipUsageSource`). The metrics carry `provider="azure"`.

## Change events

Optional, recommended. Without them a change in a subscription shows up at the next resync
(`resyncInterval`, 10 minutes by default); with them, within seconds. It is the Azure counterpart
of the AWS EventBridge → SQS path and of the GCP audit logs → Pub/Sub path:

```
writes and deletes on            Event Grid system topic     event subscription:        Storage queue
management.azure.com       ─►    of each subscription    ─►  virtual network writes ─►  subnet-operator-events
                                                             and deletes that succeeded        │
                                   operator (leader) ◄── Get Messages / Delete Message ───────┘
```

1. Create the queue once, and a system topic with its event subscription in every subscription
   the operator discovers. [`deploy/azure/events.md`](../deploy/azure/events.md) has the two
   Bicep templates ([`deploy/azure/events/`](../deploy/azure/events)) and every command. Azure
   Resource Manager publishes the events on its own: nothing needs enabling in the
   subscriptions beyond registering `Microsoft.EventGrid`.
2. The queue template assigns the operator's **own** identity (not a per-subscription one) the
   built-in **Storage Queue Data Message Processor** role on the queue. The operator holds no
   account key and no SAS token.
3. Set the chart values (the operator flags are `--azure-events-queue-url` and
   `--azure-events-debounce`):

   ```yaml
   providers:
     azure:
       events:
         queueUrl: https://subnetopevents.queue.core.windows.net/subnet-operator-events
         debounce: 10s   # empty means 10s
   ```

   The chart and the operator refuse a URL with a SAS token.

The operator reads the write and delete events of virtual networks and everything below them (a
subnet, a peering, and the network's tags, which is where its subnets' ownership lives),
collects them for the debounce so a burst causes one resync per target, and deletes a message
once its resync is enqueued. **An event names the resource and no location**: the operator looks
the network up in what its last discovery of the subscription listed, and a network that
listing did not have (just created, or outside the scope's resource groups) resyncs every
location of the subscription that a scope names. Scopes then list `ChangeEvents` in
`status.capabilities`. A Storage queue has no long polling: the leader asks every 5 seconds while
the queue is empty (the interval is fixed). Free-address counts still come from the resync.

Unlike on AWS and GCP, **the events do not say who created a resource** (a write event does not
tell a create from an update), so the operator records no creator on Azure and the auto-import
policy's `fromCreator` rules, and `skip` rules with a `principalPrefix`, **never match** there.
This is an accepted limitation of 3.0. A scope that has such rules is accepted, since the rest
of its policy still applies, and the webhook warns at apply time
(`spec.autoImport.fromCreator: the 2 creator rule(s) never match on Azure: …`); nothing in the
scope's status repeats it. `accountDefaults`, `inheritFromNetwork` and `skip` rules by tag are
what can name an owner, or keep the policy away, on Azure.

**A scope limited to resource groups in a busy subscription** should limit the event
subscription too: without that, every virtual network change in the other resource groups
reaches the queue, names a network the operator does not know, and resyncs every location of
the subscription. `topic.bicep` takes the groups as `resourceGroupNames`
([deploy/azure/events.md](../deploy/azure/events.md#one-scopes-resource-groups-only)).

`hs_change_events_total{provider="azure"}` counts what was read, by result, and a failing
receive or delete counts in `hs_change_event_errors_total{provider="azure"}` and is retried
while the periodic resync keeps running: the alert and what to do are in the runbook,
[SubnetInventoryChangeEventsFailing](operations/runbook.md#subnetinventorychangeeventsfailing).
Anything that can add a message to the queue can make the operator resync early, so keep
`Storage Queue Data Message Sender` to the system topics' identities
([threat model](security/threat-model.md#b4c--event-grid--storage-queue--operator-azure)). The
event fields are from Microsoft's schema reference; no real subscription has been watched yet
(#56).

## How Azure differs from AWS and GCP

- **Subnets cannot be tagged.** Their ownership is an entry on the virtual network
  ([above](#5-ownership-tags-on-the-virtual-network)), which is why a virtual network has a
  [tag budget](#the-tag-budget) and why `requiredSubnetTags` are read from the entry or
  inherited from the network.
- **No zones.** A virtual network and its subnets are regional, so a claim lists no zones and
  creates one subnet, as on GCP; `status.zone` of a subnet is empty. Unlike on GCP the network
  is regional too and has an address space, so a claim needs no pool.
- **No creator attribution**, an accepted limitation of 3.0. Azure's events do not say who
  created a resource, so `autoImport.fromCreator` rules and `skip` rules by `principalPrefix`
  never match; the webhook warns about a scope that has them
  ([change events](#change-events)).
- **The claim tag is always written**, as on AWS: a tag value costs nothing to create on Azure,
  so there is no `claimTag` setting.
- **IDs and tag names in any case.** Subscription IDs, resource IDs, resource group names and
  tag names are compared without regard to case, as Azure does; the operator stores IDs in
  lowercase. Tag values are compared exactly.
- **Addresses.** A subnet's usable addresses are every IPv4 prefix it has less the 5 addresses
  Azure reserves in each. `availableIPs` is the virtual network's own usage count for the
  subnet; for a subnet that count leaves out, it is the usable addresses less the subnet's IP
  configurations, unless a service manages the subnet (`status.azure.serviceManaged`: a
  delegation, a service association or resource navigation link), in which case it stays
  unknown, as it does for a gateway subnet. An unknown count exports no
  `hs_subnet_available_ips` series, rather than a zero.
- **Overlaps** are between the address spaces of virtual networks of the scope, as between VPCs
  on AWS. Peerings are not read on Azure, so `NetworkPeeredCIDROverlap` never fires there.
- **Nothing to create before the first write.** No tag keys or values to set up, unlike GCP.
- **An import reads before it writes.** On AWS and GCP the tag write names the resource itself,
  so the cloud refuses one that does not exist. On Azure a subnet's ownership is a tag on its
  virtual network, which Azure writes for any name, so the operator reads the virtual network
  first (`ResourceNotFound`). The same read is where an import's region comes from: on AWS
  `spec.region` is required and on GCP it is part of a subnetwork's ID, while an Azure resource
  ID names no location ([above](#6-a-first-scope-claim-and-import)).
- **Throttling.** Resource Manager's limits are per subscription and per identity; the operator
  slows down before it is throttled, from the remaining-reads header ARM returns
  ([runbook](operations/runbook.md#subnetinventorytargetthrottled-on-azure)).
- **Rolling back to 2.x**: delete the Azure scopes first; see the
  [upgrade guide](operations/upgrades.md#rolling-back-to-2x).

## Known gaps in 3.0

Besides the run against real subscriptions (#56), peerings and creator attribution
([above](#how-azure-differs-from-aws-and-gcp)):

- **Ownership entries are never removed (#121).** The entry of a subnet that was deleted stays
  on its virtual network and counts against the [tag budget](#the-tag-budget) until someone
  removes it, and no tooling lists such entries yet.
- **Every location lists the whole subscription (#118).** Resource Manager has no location
  filter, so a subscription in several locations is listed once per location on each sync.
- **A location with no virtual network is not reported (#119).** Only the shape of a location
  name is checked: a misspelt one syncs cleanly with nothing in it.
- **Errors of the operator's own identity are the SDK's (#120).** Only an identity of a
  subscription's own gets the hint naming the federated credential it needs.
