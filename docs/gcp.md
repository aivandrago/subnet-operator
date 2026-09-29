# Google Cloud

The Google Cloud provider is what 2.0 adds ([ADR 0002](adr/0002-multi-cloud-model.md), milestone
"2.0 — Google Cloud"), released in 2.0.0. The
[upgrade guide](operations/upgrades.md#upgrading-from-1x-to-20) lists what 2.0 changes for an
existing installation.

Like 1.0 on AWS, the GCP provider has been tested against an emulator (an in-repository fake of
the Compute Engine, Resource Manager and IAM Credentials APIs, driven through the real Google
client libraries), not yet against a real Google Cloud project. That conformance run is #50;
the places below that depend on it say so. If you try the provider in a project of your own,
a report of what worked and what did not is the most useful contribution there is
([CONTRIBUTING.md](../CONTRIBUTING.md)).

- [What the provider does](#what-the-provider-does)
- [1. Enable the provider](#1-enable-the-provider)
- [2. The operator's own Google identity](#2-the-operators-own-google-identity)
- [3. Read and write service accounts per project](#3-read-and-write-service-accounts-per-project)
- [4. Roles](#4-roles)
- [5. Tag keys and values](#5-tag-keys-and-values)
- [6. A first scope, claim and import](#6-a-first-scope-claim-and-import)
- [7. Verify](#7-verify)
- [Change events](#change-events)
- [How GCP differs from AWS](#how-gcp-differs-from-aws)

## What the provider does

| | AWS (from 0.1) | GCP (from 2.0) |
|---|---|---|
| Account | AWS account, 12 digits | project ID (for a Shared VPC, the host project) |
| Network | VPC, regional, has CIDR blocks | VPC network, **global**, no address space of its own |
| Subnet | subnet, zonal | subnetwork, **regional**, with optional secondary ranges |
| Ownership | resource tags `hs/owner`, `hs/env`, `hs/tier` | Resource Manager tags bound to the network or subnetwork: `hs-owner`, `hs-env`, `hs-tier` |
| Identity per account | `accounts[].aws.roleARN` / `writeRoleARN`, assumed | `accounts[].gcp.serviceAccount` / `writeServiceAccount`, impersonated |
| Operator's own identity | EKS Pod Identity or IRSA | GKE Workload Identity or Workload Identity Federation |
| Change events (optional) | CloudTrail → EventBridge → SQS | Admin Activity audit logs → log sink → Pub/Sub |
| Overlaps (`NetworkCIDROverlap`) | between VPCs' CIDR blocks | between the subnetworks' ranges (primary, secondary, IPv6) of different networks; peered networks also fire `NetworkPeeredCIDROverlap` |
| `status.capabilities` | `CreateSubnet`, `ChangeEvents` (with events), `IPUsage` | `CreateSubnet`, `ChangeEvents` (with events), `IPUsage` |

Discovery reads, per project and region, the project's VPC networks with their peerings
(`networks.list`), the
region's subnetworks with their IP usage (`subnetworks.list` with `views=WITH_UTILIZATION`),
the project's number (`projects.get`) and the tags bound to each network and subnetwork
(`effectiveTags.list`). With writes on, a `SubnetClaim` in `Create` mode creates a subnetwork
with its tags bound at creation (`subnetworks.insert`), and a `ResourceImport` binds tags
(`tagBindings.create`). With change events set up, the leader also pulls a Pub/Sub subscription
of audit log entries and resyncs the project and region a change names. Nothing on GCP is ever
deleted or unbound: there is no delete call in `internal/cloud/gcp`.

## 1. Enable the provider

The provider runs when the chart's `providers.gcp.enabled` is set (the manager's
`--providers=aws,gcp`). AWS stays enabled unless you turn it off; a chart with neither refuses
to render. For a GCP-only installation:

```yaml
# values-gcp.yaml (the same file is examples/values-gcp.yaml)
providers:
  aws:
    enabled: false
  gcp:
    enabled: true
    workloadIdentity:
      serviceAccount: subnet-operator@ops-tools.iam.gserviceaccount.com
```

```sh
helm install subnet-operator hypersurgery/subnet-operator \
  -n subnet-operator-system --create-namespace -f values-gcp.yaml
```

The chart has `providers.gcp` from 2.0.0; a 1.x chart does not know it.

A `NetworkScope` with `provider: GCP` on an operator that does not run the provider is reported
`Ready=False` with reason `ProviderNotEnabled` and is not synced.

## 2. The operator's own Google identity

The operator authenticates with Application Default Credentials; nothing in its code differs
between the options below, only the chart's rendering does. Set one of them, not both (the
chart refuses both).

### GKE Workload Identity

On GKE with Workload Identity enabled on the cluster and the node pool:

1. Create a Google service account for the operator, e.g.
   `subnet-operator@ops-tools.iam.gserviceaccount.com`.
2. Let the operator's Kubernetes service account act as it. With the chart the Kubernetes
   service account is `<release namespace>/<fullname>`, `subnet-operator-system/subnet-operator`
   for the commands in this guide:

   ```sh
   gcloud iam service-accounts add-iam-policy-binding \
     subnet-operator@ops-tools.iam.gserviceaccount.com \
     --role=roles/iam.workloadIdentityUser \
     --member="serviceAccount:ops-tools.svc.id.goog[subnet-operator-system/subnet-operator]"
   ```

3. Set `providers.gcp.workloadIdentity.serviceAccount` to its email. The chart writes it as the
   service account's `iam.gke.io/gcp-service-account` annotation.

Leaving `workloadIdentity.serviceAccount` empty uses the Kubernetes service account's own
principal (`principal://iam.googleapis.com/projects/<project number>/locations/global/workloadIdentityPools/<project ID>.svc.id.goog/subject/ns/<namespace>/sa/<service account>`),
which then holds the roles itself.

### Workload Identity Federation from another cluster

On EKS, AKS or a self-managed cluster, a workload identity pool that trusts the cluster's
service account token issuer lets the operator exchange a projected token at Google's STS:

```yaml
providers:
  gcp:
    enabled: true
    wif:
      # projects/<project number>/locations/global/workloadIdentityPools/<pool>/providers/<provider>
      provider: projects/123456789012/locations/global/workloadIdentityPools/k8s/providers/eks-prod
      # Optional: a service account the federated identity impersonates. Empty uses the
      # federated principal itself, which then holds the roles.
      serviceAccount: subnet-operator@ops-tools.iam.gserviceaccount.com
```

The chart renders the credential configuration (an `external_account` file in a ConfigMap),
mounts a projected service account token with the provider's audience at
`providers.gcp.wif.tokenPath` (`/var/run/secrets/gcp-wif/token`) and sets
`GOOGLE_APPLICATION_CREDENTIALS`. A configuration of your own, from
`gcloud iam workload-identity-pools create-cred-config --credential-source-file=<tokenPath>`, goes
in a ConfigMap or Secret (`providers.gcp.wif.credentialConfig.configMap` or `.secret`) together
with `providers.gcp.wif.audience`. The federated principal, when it holds the roles or impersonates
`wif.serviceAccount`, is
`principal://iam.googleapis.com/projects/<pool project number>/locations/global/workloadIdentityPools/<pool>/subject/system:serviceaccount:<namespace>:<service account>`
with the usual attribute mapping `google.subject=assertion.sub`.

### The metadata server and NetworkPolicy

With `networkPolicy.enabled`, the chart allows egress to the GKE metadata server, which hands out
Workload Identity's credentials (`169.254.169.254:80` for Dataplane V2, `169.254.169.252:988`
otherwise; `providers.gcp.metadataServer.endpoints`). It is on by default with GCP, and off with
`wif`: outside GKE that address is the node's own instance metadata service, which the operator
has no business reaching. Set `providers.gcp.metadataServer.enabled: false` on any cluster that
is not GKE. The Google APIs are reached on 443 through the general egress rule; where
`networkPolicy.egress.cidrs` is narrowed, add their ranges with `providers.gcp.apiCIDRs`, e.g.
`[199.36.153.8/30]` for `private.googleapis.com`.

## 3. Read and write service accounts per project

Each project in a scope is reached either with the operator's own identity or by impersonating
service accounts named on the account entry, the GCP counterpart of the roles the operator
assumes on AWS:

```yaml
accounts:
  - id: net-host-prod
    gcp:
      # Impersonated to read the project. Empty: the operator's own identity reads it.
      serviceAccount: subnet-reader@net-host-prod.iam.gserviceaccount.com
      # Impersonated for SubnetClaims in Create mode and for ResourceImports, never for reads.
      writeServiceAccount: subnet-writer@net-host-prod.iam.gserviceaccount.com
```

- **Impersonation** is the IAM Service Account Credentials API's `generateAccessToken`, called
  by the operator's own identity, which needs `iam.serviceAccounts.getAccessToken` on each
  service account it impersonates (`deploy/gcp/impersonator-role.yaml`, or the broader
  `roles/iam.serviceAccountTokenCreator`), and the API
  (`iamcredentials.googleapis.com`) enabled in its own project. Tokens last an hour and are
  renewed five minutes before they expire, one per service account.
- **Read and write stay apart.** Discovery never uses `writeServiceAccount`, and writes never
  use `serviceAccount`. Naming the same service account for both is accepted with a warning,
  because discovery then carries the write permissions.
- **A project read through a service account but without a `writeServiceAccount`** is not the
  operator's own, so the operator's own identity does not write there either: claims for it can
  only be allocated (reason `NoWriteRole`), and imports stay `Pending` (`NoWriteRole`). This is
  the same rule as an AWS account with `roleARN` and no `writeRoleARN`.
- **A service account may live in any project.** Unlike an AWS role it is not tied to the
  account's ID, so a central reader per team, granted on a folder, can read many projects.

If the operator cannot get a token, the target fails before any Compute call, with an error that
names the service account and what to grant (runbook:
[a service account the operator cannot impersonate](operations/runbook.md#subnetinventorytargetdown-on-gcp)).

## 4. Roles

`deploy/gcp/` has one custom role definition per job, least privilege, each with the `gcloud`
commands to create and grant it in its header. Every permission and why it is there is in the
[IAM reference](reference/iam.md#google-cloud).

| Role file | Grant to | On | Needed for |
|---|---|---|---|
| [`reader-role.yaml`](../deploy/gcp/reader-role.yaml) | each account's `gcp.serviceAccount`, or the operator's own identity | every project in the scope (or a folder / the organization above them) | discovery |
| [`impersonator-role.yaml`](../deploy/gcp/impersonator-role.yaml) | the operator's own identity | each service account it impersonates | accounts with `gcp.serviceAccount` / `writeServiceAccount` |
| [`writer-role.yaml`](../deploy/gcp/writer-role.yaml) | each account's `gcp.writeServiceAccount` | the host project | `SubnetClaim` in `Create` mode, `ResourceImport` |
| [`tag-user-role.yaml`](../deploy/gcp/tag-user-role.yaml) | the write identity | the operator's tag keys, or `gcp.tagParent` | binding tags (claims and imports) |
| [`tag-value-creator-role.yaml`](../deploy/gcp/tag-value-creator-role.yaml) | the write identity | the operator's tag keys only | only with `spec.gcp.createTagValues: true` |
| [`events-subscriber-role.yaml`](../deploy/gcp/events-subscriber-role.yaml) | the operator's own identity | the change events subscription | only with [change events](#change-events) |

Create a role once per organization (`gcloud iam roles create <id> --organization=<number>
--file=deploy/gcp/<file>`), or per project with `--project`. What the conformance run (#50)
still has to confirm: that `views=WITH_UTILIZATION` and `effectiveTags.list` need nothing beyond
the reader role (if tags come back without their names, also grant
`roles/resourcemanager.tagViewer` on `gcp.tagParent`), the exact `createTagBinding` permission
names, and whether a custom role may carry `resourcemanager.tagValueBindings.create`; the
predefined fallback for the tag user role is `roles/resourcemanager.tagUser`.

## 5. Tag keys and values

Ownership on GCP is kept in Resource Manager tags bound directly to a network or subnetwork.
Tag keys and values are resources of their own, owned by an organization or a project, and the
scope names that owner:

```yaml
spec:
  provider: GCP
  gcp:
    # organizations/<organization number> or projects/<project ID>
    tagParent: organizations/123456789012
    # Let the write identity create a missing tag value. Off by default.
    createTagValues: false
    # Bind the per-claim tag hs-claim to subnetworks claims create. Skip by default.
    claimTag: Skip
```

- **Keys are reported by their short name.** A tag whose key belongs to `tagParent` shows up as
  `hs-owner`; a key of any other parent keeps its namespaced name, `<parent>/<key>`, so two
  parents' `hs-owner` cannot be confused. `spec.tagKeys`, `requiredSubnetTags`,
  `networkSelector.matchTags` and the auto-import keys name the short names.
- **Only direct bindings count.** A tag a network inherits from its project, folder or
  organization says nothing about who owns that network and is ignored.
- **One value per key.** A resource carries one value of each key, so the operator only ever
  adds bindings: a key already bound to another value refuses the whole write
  (`TagValueConflict`); replacing an owner means removing the binding by hand first.
- **Names.** Keys and values are 1 to 63 letters and digits, with `-`, `_` and `.` between them;
  `/` is not allowed. That is why the GCP keys are `hs-*` and not `hs/*`.

**The operator never creates tag keys.** Create them under `tagParent` before the first claim or
import, as someone holding `roles/resourcemanager.tagAdmin` there. The keys the operator uses on
GCP:

| Key | Holds | Written by |
|---|---|---|
| `hs-owner`, `hs-env`, `hs-tier` | owner, environment, tier (the defaults of `spec.tagKeys`) | claims and imports |
| `hs-managed` | the managed marker (the default of `autoImport.managedTag`, and a usual `networkSelector`) | imports, the auto-import policy |
| `hs-managed-by` | `subnet-operator` on subnetworks the operator created | claims |
| `hs-claim` | `<namespace>_<name>` of the claim a subnetwork was created for | claims, only with `claimTag: Bind` |

```sh
ORG=123456789012
for key in hs-owner hs-env hs-tier hs-managed hs-managed-by; do
  gcloud resource-manager tags keys create "$key" --parent="organizations/$ORG"
done
# Only for scopes with claimTag: Bind.
gcloud resource-manager tags keys create hs-claim --parent="organizations/$ORG"
gcloud resource-manager tags values create subnet-operator --parent="$ORG/hs-managed-by"
gcloud resource-manager tags values create true --parent="$ORG/hs-managed"
gcloud resource-manager tags values create team-payments --parent="$ORG/hs-owner"
```

**Values** must exist before they are bound, too. With `spec.gcp.createTagValues` off (the
default), a claim or import that needs a value nobody created fails with `TagValueMissing`,
naming the value (`<parent>/<key>/<value>`), and binds nothing. With `createTagValues: true`,
the write identity creates missing values of the existing keys (and needs
`tag-value-creator-role.yaml` on those keys; grant it on the keys, never on the organization, so
that it cannot create values of anybody else's keys). A key holds at most 1,000 values; past
that, creating one fails with `TagValueLimitReached`. The operator never deletes a value.

**The claim tag is optional on GCP** (`spec.gcp.claimTag`, owner decision for 2.0). On AWS every
subnet a claim creates carries `hs/claim=<namespace>/<name>`, which costs nothing there. On GCP
the same tag would need a tag value per claim: created by hand before each claim, or by the
operator with `createTagValues`, in which case values of claims long gone pile up towards the
1,000-value limit of the key, since the operator never deletes one. So:

- **`Skip` (the default)**: a subnetwork a `Create` claim creates carries `hs-owner`, `hs-env`,
  `hs-tier`, `hs-managed-by=subnet-operator` and the claim's own `spec.tags`, all values the
  teams share, and no `hs-claim`. The `hs-claim` key does not have to exist. Which claim a
  subnetwork belongs to is kept in Kubernetes: the claim's `status.allocations[].subnetID`. If
  the claim loses its status (a status update that failed after the create), it finds its
  subnetwork again by name: the subnetwork `projects/<account>/regions/<region>/subnetworks/<namePrefix>`
  is adopted when it carries `hs-managed-by=subnet-operator` and the claim's owner, has no
  `hs-claim` of another claim, and no other claim leads to the same name or holds it in its
  status. Give every claim its own `namePrefix` in a project and region; two claims that lead to
  the same name adopt nothing by name, and the second one's create fails with
  `exists already … choose another namePrefix`.
- **`Bind`**: as on AWS, the subnetwork also carries `hs-claim=<namespace>_<name>` and is adopted
  by it. Choose it when something outside Kubernetes (a tag policy, billing export, an audit)
  needs to find the claim from the cloud. The webhook warns about the cost: without
  `createTagValues` every claim fails with `TagValueMissing` until its value exists; with it, the
  values accumulate.

A scope can switch between the two at any time. Subnetworks created under `Bind` keep their
`hs-claim` binding (nothing is unbound) and are still adopted by it; subnetworks created under
`Skip` are adopted by name, also after a switch to `Bind`.

## 6. A first scope, claim and import

The same manifests are [`examples/08-gcp-project.yaml`](../examples/08-gcp-project.yaml) and
[`examples/09-gcp-subnet-claim.yaml`](../examples/09-gcp-subnet-claim.yaml), which the tests send
to the API server and run through the provider's validation. The scope can be applied with
`kubectl`, or created by the chart with the release (below).

With `kubectl`:

```yaml
apiVersion: network.hypersurgery.dev/v1
kind: NetworkScope
metadata:
  name: gcp-shared-vpc
spec:
  provider: GCP
  gcp:
    tagParent: organizations/123456789012
  accounts:
    # A Shared VPC host project. Its networks are global; its subnetworks are read per region.
    - id: net-host-prod
      gcp:
        serviceAccount: subnet-reader@net-host-prod.iam.gserviceaccount.com
        writeServiceAccount: subnet-writer@net-host-prod.iam.gserviceaccount.com
  regions: [europe-west1, europe-west4]
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
file is [`examples/values-gcp.yaml`](../examples/values-gcp.yaml):

```yaml
networkScope:
  create: true
  name: gcp-shared-vpc
  provider: GCP
  gcp:
    tagParent: organizations/123456789012
    createTagValues: false
    claimTag: Skip
  accounts:
    - id: net-host-prod
      gcp:
        serviceAccount: subnet-reader@net-host-prod.iam.gserviceaccount.com
        writeServiceAccount: subnet-writer@net-host-prod.iam.gserviceaccount.com
  regions: [europe-west1, europe-west4]
  networkSelector:
    matchTags:
      hs-managed: "true"
  requiredSubnetTags: [hs-owner, hs-env]
  namespaceSelector:
    matchLabels:
      kubernetes.io/metadata.name: default
  resyncInterval: 10m
```

The chart refuses to render a GCP scope without `providers.gcp.enabled` or `gcp.tagParent`, and
with a tag key containing `/`. Its default `networkSelector.matchTags` and
`requiredSubnetTags` are the AWS keys; on GCP it renders them as `hs-managed` and `hs-owner`,
`hs-env`, `hs-tier`.

A claim on GCP is for **one** regional subnetwork: no `zones`, and a pool to carve its range from,
since a VPC network has no address space of its own. `namePrefix` (default: the claim's name) is
the subnetwork's name. The ranges already taken are those of the network's subnetworks, primary
and secondary, in the scope's regions, and every other claim's reservations; a subnetwork in a
region the scope does not cover is not known to the operator, so cover every region the network
has subnetworks in, or name a pool they do not use.

```yaml
apiVersion: network.hypersurgery.dev/v1
kind: SubnetClaim
metadata:
  name: checkout
  namespace: default
spec:
  scopeRef: gcp-shared-vpc
  account: net-host-prod
  region: europe-west1
  networkID: projects/net-host-prod/global/networks/shared-prod
  prefixLength: 24
  mode: Create
  owner: team-payments
  env: prod
  tier: private
  namePrefix: payments-checkout
  gcp:
    poolCIDRs: [10.60.0.0/16]
    privateIPGoogleAccess: true
```

An import binds tags to an existing network (`region` may be empty: a network is global) or
subnetwork (`region` is the subnetwork's):

```yaml
apiVersion: network.hypersurgery.dev/v1
kind: ResourceImport
metadata:
  name: shared-prod-import
  namespace: default
spec:
  scopeRef: gcp-shared-vpc
  account: net-host-prod
  resourceID: projects/net-host-prod/global/networks/shared-prod
  tags:
    hs-managed: "true"
    hs-owner: team-payments
    hs-env: prod
  requestedBy: platform team (ticket NET-512)
```

Writes need `--enable-writes` (`writes.enabled: true` in the chart) as on AWS.

## 7. Verify

```sh
kubectl get nscope gcp-shared-vpc
kubectl get nscope gcp-shared-vpc -o jsonpath='{range .status.targets[*]}{.account}/{.region}{"\t"}{.error}{"\n"}{end}'
kubectl get hsnet -l network.hypersurgery.dev/provider=gcp
kubectl get hssubnet -l network.hypersurgery.dev/account=net-host-prod,network.hypersurgery.dev/region=europe-west1 -o wide
```

`READY` is `True` (reason `Synced`) once every project/region pair was read. `SyncFailed` means
one could not be read; `status.targets[].error` says why, and the
[runbook](operations/runbook.md#subnetinventorytargetdown-on-gcp) has the GCP causes.
`Network` and `Subnet` objects of GCP are named `<name>-<10 hex digits>` (a GCP resource name is
not a valid object name), their `spec.id` is the resource name
(`projects/<project>/global/networks/<name>`,
`projects/<project>/regions/<region>/subnetworks/<name>`), a network's `spec.region` is empty,
and a subnetwork's secondary ranges are in `status.secondaryCIDRBlocks` and, with their usage,
in `status.gcp.secondaryRanges`. A network's VPC Network Peerings, with their state, are in
`status.gcp.peerings`. The metrics carry `provider="gcp"`.

## Change events

Optional, recommended. Without them a change in a project shows up at the next resync
(`resyncInterval`, 10 minutes by default); with them, within seconds. It is the GCP counterpart
of the AWS EventBridge → SQS path:

```
network and subnetwork calls,      Admin Activity    aggregated log sink       Pub/Sub topic
tag bindings, in every project  ─► audit logs     ─► (organization, folder, ─► and subscription
                                                      or one per project)            │
                                     operator (leader) ◄── streaming pull ──────────┘
```

1. Create the topic, the subscription and the log sink, and grant the sink's writer identity
   `roles/pubsub.publisher` on the topic. [`deploy/gcp/events.md`](../deploy/gcp/events.md) has
   the filter and every command, with gcloud and with Terraform. Admin Activity audit logs are
   always on, so nothing needs enabling in the projects.
2. Grant the operator's own identity (Workload Identity, or Workload Identity Federation; not a
   per-project service account) [`deploy/gcp/events-subscriber-role.yaml`](../deploy/gcp/events-subscriber-role.yaml)
   (`pubsub.subscriptions.consume`) or `roles/pubsub.subscriber` **on the subscription**.
3. Set the chart values (the operator flags are `--gcp-events-subscription` and
   `--gcp-events-debounce`):

   ```yaml
   providers:
     gcp:
       events:
         subscription: projects/ops-tools/subscriptions/subnet-operator-events
         debounce: 10s   # empty means 10s
   ```

The operator resyncs only the projects and regions of a `NetworkScope` that an entry names (a
subnetwork's region, or every region of the project for a global network), collects entries for
the debounce so a burst causes one resync per target, and acknowledges every message it reads.
Scopes then list `ChangeEvents` in `status.capabilities`. It also remembers who inserted a network
or subnetwork (the entry's `principalEmail`), so the auto-import policy's `fromCreator` and
`skip` principal rules work on GCP too; without events, only `accountDefaults` and
`inheritFromNetwork` can name an owner there. Free-address counts still come from the resync.
Anyone who can publish to the topic can make the operator resync early or name a creator, so
keep `roles/pubsub.publisher` to the sink's writer identity.

`hs_change_events_total{provider="gcp"}` counts what was read, by result, and a failing pull
counts in `hs_change_event_errors_total{provider="gcp"}` and is retried while the periodic
resync keeps running: the alert and what to do are in the runbook,
[SubnetInventoryChangeEventsFailing](operations/runbook.md#subnetinventorychangeeventsfailing).

## How GCP differs from AWS

- **Networks are global.** Every project/region target reports the project's networks; the
  operator keeps one `Network` per network with an empty region, adds up its subnets over every
  region of the scope, and deletes it only after every target of its project synced without it.
- **Subnetworks are regional**, so a claim lists no zones and creates one subnetwork.
- **The claim tag is optional.** A subnetwork a claim creates carries no `hs-claim` unless the
  scope sets `gcp.claimTag: Bind`; the claim finds it by name instead
  ([tag keys and values](#5-tag-keys-and-values)). On AWS `hs/claim` is always written.
- **Secondary ranges** (such as a GKE cluster's Pod and Service ranges) count as taken when a
  claim allocates, and are reported with their own usage, not added to the subnet's
  `totalIPs`/`availableIPs`. A subnetwork's usable addresses are its primary range less the
  4 addresses Google reserves.
- **Overlaps are between subnetworks.** A VPC network has no range to compare, so two networks
  of the scope overlap when a range of a subnetwork of one — primary, secondary or IPv6 —
  overlaps a range of the other's, in any region or project of the scope. `status.overlapsWith`,
  `hs_network_cidr_overlaps` and `NetworkCIDROverlap` report them as they report VPCs on AWS;
  `status.gcp.overlaps` adds which ranges overlap and whether the two networks are peered, and
  `hs_network_peered_cidr_overlaps` with the alert `NetworkPeeredCIDROverlap` single out the
  peered ones, whose peering cannot become active. Only networks in the scope are compared: put
  the projects of peered networks in the same scope. The runbook has
  [what to do](operations/runbook.md#networkcidroverlap-on-gcp).
- **Special-purpose subnetworks** (proxy-only, Private Service Connect) are discovered like any
  other and say so in `status.gcp.purpose`.
- **Two GCP scopes over different regions of the same project** are accepted with a warning:
  both report the project's global networks. The same project and region is refused, as on AWS.
- **Rolling back to 1.x**: delete the GCP scopes first; see the
  [upgrade guide](operations/upgrades.md#rolling-back-to-1x).
