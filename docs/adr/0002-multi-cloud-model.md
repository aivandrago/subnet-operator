# ADR 0002: The multi-cloud model

Status: accepted (2026-09-24); implemented for AWS. The group, `v1beta1` and the migration from
the old group shipped in 0.8; the provider registry, the neutral metrics and the removal of
`aws.hypersurgery` in 0.9; `v1` with the conversion webhook and the storage migration in 1.0
(§10). The GCP provider (#46–#51) shipped in 2.0: discovery (#46), writes (#47), credentials
(#48) and change events (#49), see their implementation notes; the run against a real project
(#50) is still open. The Azure provider (#52–#57) shipped in 3.0: discovery (#52), writes
(#53), credentials (#54) and change events (#55), see their implementation notes; the run
against real subscriptions (#56) is still open. Both are on the v1 API
(owner decision of 2026-09-25, in the [decision record](#decision-record)). Written before
the implementation; the implementation notes at the end record where it decided or deviated.

Refs: #39 (this ADR), research spikes #40 ([GCP](../research/gcp-network-model.md)) and #41
([Azure](../research/azure-network-model.md)), implementation #42–#45 (Phase 5a), #46–#51 (GCP),
#52–#57 (Azure), #58 (v1).

## Context

The API is `aws.hypersurgery/v1alpha1` and everything in it assumes AWS: 12-digit account IDs,
`roleARN`, `vpc-`/`subnet-` ID patterns, availability zones on every subnet, route tables, the
`hs/owner` tag key with a `/` in it, and metric names prefixed `hs_aws_`. The code under the API
is less tied to AWS than the API is: `internal/inventory` already defines neutral `Discoverer`,
`SubnetWriter` and `TagWriter` interfaces with `ErrThrottled` and `ErrCIDRConflict`, and only
`internal/cloud/aws` and `internal/events` speak AWS.

The owner decided (2026-09-24) to rename the API group to a neutral one **before 1.0** and to
graduate to v1 on it (#58). GCP and Azure ship later as 1.x releases, so the 1.0 API must be able
to carry them **without another breaking change**. This ADR fixes the shape of that API.
(Since 2026-09-25 they ship as 2.0 and 3.0 instead, see the [decision record](#decision-record);
the requirement on the API stands: both are added to v1 without breaking it.)

What the spikes found that constrains the shape:

| Question | AWS (today) | GCP ([#40](../research/gcp-network-model.md)) | Azure ([#41](../research/azure-network-model.md)) |
|---|---|---|---|
| Metadata on the network | tags | no labels; Resource Manager tags | tags |
| Metadata on the subnet | tags | no labels; Resource Manager tags, values must be pre-created, not returned by `subnetworks.get` | **none**: subnets cannot carry tags |
| Key rules | `hs/owner` valid | keys are pre-created `TagKey`s; 1,000 values per key; 50 tags per resource | no `/` in names; 50 tags per resource; value ≤ 256 chars |
| Free IPs per subnet | `AvailableIpAddressCount` | `views=WITH_UTILIZATION` → `utilizationDetails` per range | `virtualNetworks/{vnet}/usages`; `-1` means unknown |
| Zones | subnet is zonal | network global, subnet regional | VNet regional, subnet has no zone |
| Account | account | project (Shared VPC: host project) | subscription (+ resource group in the ID) |
| Change events | EventBridge → SQS | audit logs → sink → Pub/Sub | Event Grid → Storage queue |
| Throttling | `Throttling` errors | 403 `rateLimitExceeded`/429, per-minute per project and region | 429 + `Retry-After`, token bucket per subscription and principal |

Two findings shape the API most. First, **ownership metadata cannot always sit on the subnet**:
Azure subnets have no tags at all, and GCP subnet tags are resources of their own (values must
exist before they are bound, and are read through a separate API). Second, **free IPs can be
unknown**: Azure reports `-1` for some subnets, and neither new source's exact accounting of
reserved addresses is confirmed yet.

What the API holds today, and what is state rather than cache:

| Kind | Scope | Written by | Holds state that exists nowhere else |
|---|---|---|---|
| `NetworkScope` | Cluster | user | spec only |
| `VPC`, `Subnet` | Cluster | operator | no: rebuilt by discovery, owned by the scope |
| `SubnetClaim` | Namespaced | user | **yes**: CIDR reservations in `status.allocations` (in `Allocate` mode the only record of them) |
| `ResourceImport` | Namespaced | user / auto-import policy | history: `status.appliedTags`, `appliedTime` |
| `SheetExport` | Cluster | user | no |

## Decisions

### 1. API group: `network.hypersurgery.dev`

One neutral group, `network.hypersurgery.dev`, for every kind and every provider.

- Kubernetes asks for group names that are DNS subdomains and recommends "a subdomain your group
  or organization owns" ([API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#api-conventions)).
  `hypersurgery.dev` is the project's domain (site, chart repository, Go module path);
  `hypersurgery` on its own is not a domain anyone owns.
- The `network.` prefix leaves room for other groups later without another rename.
- CRD names become `<plural>.network.hypersurgery.dev`; the API server only requires a group with
  at least one dot ([apiextensions validation](https://github.com/kubernetes/apiextensions-apiserver/blob/master/pkg/apis/apiextensions/validation/validation.go)).

Alternatives:

- `network.hypersurgery` (the name in #39). Shorter and closer to today's prefix, but not under
  an owned domain. The rename is the one chance to fix that; after 1.0 it would cost another
  migration.
- One group per cloud (`aws.`, `gcp.`, `azure.hypersurgery.dev`). Rejected: every controller,
  webhook, dashboard view and sheet would handle three sets of types for the same concepts, and
  a cross-cloud inventory (the product) would need three lists.
- `subnets.hypersurgery.dev`. Rejected: too narrow for `NetworkScope`, `Network`, imports.

Object labels move with the group: `network.hypersurgery.dev/{scope,provider,account,region,network,resource}`.

### 2. Provider: a discriminator on the scope, typed per-provider sub-structs

A `NetworkScope` selects exactly one provider:

```yaml
spec:
  provider: AWS            # AWS | GCP | Azure
  aws: {...}               # only the member matching provider may be set
```

- `provider` is the union discriminator; the provider-specific settings are optional typed
  members (`aws`, `gcp`, `azure`), following the Kubernetes union convention of mutually
  exclusive optional fields ([API conventions: unions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#unions)).
  A CEL rule ([validation rules](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/#validation-rules))
  enforces "no member other than the one `provider` names", one rule per member:
  `!has(self.gcp) || self.provider == 'GCP'`.
- `provider` is immutable (`self == oldSelf`): a scope does not change clouds; a new scope does.
- Discovered objects (`Network`, `Subnet`) carry `spec.provider`, written by the operator, so they
  can be listed, printed and exported by provider without looking up the scope.
- `SubnetClaim` and `ResourceImport` do **not** restate the provider: it comes from `scopeRef`.
  Their provider-specific options are the same kind of typed members (`aws:` today); the
  validating webhook, which already reads the scope, checks the member matches the scope's
  provider. CEL cannot do that check because it cannot read another object.
- Anything that is the same concept in all three clouds is a neutral field. Anything that exists
  in one cloud only (route table association, `mapPublicIPOnLaunch`, AZ IDs, GCP secondary
  ranges, Azure delegations) lives in the provider member. A field graduates from a provider
  member to neutral only when a second provider has the same concept.

The enum is **open**: its documentation says from the first release that more values will be
added and that clients must treat an unknown value as "a provider this client does not know"
(skip, do not fail). The Kubernetes compatibility rules make exactly this the condition for adding
enum values later ([API changes: enums](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api_changes.md#backward-compatibility-gotchas)),
and adding a member to a union is compatible when the union followed the conventions from the
start (same section).

**1.0 ships `provider: AWS` and the `aws` member only.** `GCP`/`Azure` and their members are added
in the releases that implement them, 2.0 and 3.0; both are additive to v1. The sketches below
exist to prove the neutral fields fit them, not to ship unused schema.

Alternatives:

- **Separate kinds per provider** (`AWSNetworkScope`, `GCPNetworkScope`, ...), as Cluster API
  does for infrastructure. Rejected: Cluster API needs it because each provider has its own
  controller binary; here one operator reconciles all of them, and `Network`/`Subnet`/claims are
  meant to be one inventory. Per-provider kinds would triple CRDs, webhooks and RBAC, and every
  consumer (dashboard, sheet, alerts) would join three lists.
- **Untyped `providerConfig: map[string]string` or `RawExtension`.** Rejected: no schema, no
  defaults, no `kubectl explain`, validation only in code.
- **Provider inferred from the account ID format.** Rejected: implicit, and ambiguous as soon as
  two formats can collide.

### 3. Account: one neutral `id`, provider identity in the member

| Concept | AWS | GCP | Azure |
|---|---|---|---|
| `accounts[].id` | account ID (`^[0-9]{12}$`) | project ID (`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`) | subscription ID (UUID) |
| read identity | `aws.roleARN` (+ `externalID`) | `gcp.serviceAccount` to impersonate | `azure.clientID` (+ `tenantID`) |
| write identity | `aws.writeRoleARN` | `gcp.writeServiceAccount` | `azure.writeClientID` |
| empty identity means | operator's own (Pod Identity/IRSA) | operator's own (Workload Identity Federation) | operator's own (Workload Identity) |

- One `id` field keeps everything keyed by account uniform: `inventory.TargetKey`, the `account`
  metric label, the dashboard, the sheet, `accountDefaults` in the auto-import policy. The format
  is validated per provider by CEL on the scope (the scope knows its provider).
- The Azure resource group is **not** part of the account: discovery covers the whole
  subscription unless the scope limits it to some resource groups (`spec.azure.resourceGroups`,
  see the #52 notes), and the resource group is part of each network's identity
  (`Network.spec.azure.resourceGroup`, and inside the ARM resource ID).
- GCP Shared VPC: subnets live in the host project, so the host project is the account; service
  projects that only use the subnets are not listed.
- Separate read and write identities stay a rule for every provider (the read identity never
  carries write permissions).

### 4. Location: `regions` and optional `zones`

- `regions` keeps its name and means the provider's region (AWS region, GCP region, Azure
  location such as `westeurope`). It remains required on the scope.
- `Network.spec.region` is **optional**: GCP VPC networks are global resources; AWS VPCs and Azure
  VNets are regional.
- `Subnet.status.zone` is **optional**: only AWS subnets are zonal. GCP subnetworks are regional,
  Azure subnets have no zone.
- `SubnetClaim.spec.availabilityZones` becomes `spec.zones`: optional in the schema, required
  (1–6) for AWS and forbidden for GCP/Azure by the webhook. A claim for a provider without zones
  creates `count` subnets (default 1). Allocations are keyed by the subnet **name** instead of by
  AZ, because a GCP/Azure claim has no AZ to key on.

### 5. Kinds

| v1alpha1 (`aws.hypersurgery`) | new (`network.hypersurgery.dev`) | Change |
|---|---|---|
| `NetworkScope` | `NetworkScope` | `provider` + members; `vpcTagSelector` → `networkSelector.matchTags` |
| `VPC` | **`Network`** | AWS VPC, GCP VPC network, Azure VNet |
| `Subnet` | `Subnet` | neutral identity, provider details in `status.<provider>` |
| `SubnetClaim` | `SubnetClaim` | `vpcID` → `networkID`, `availabilityZones` → `zones`, AWS options → `aws:` |
| `ResourceImport` | `ResourceImport` | `tags` keep their name; where they land depends on the ownership model (§6) |
| `SheetExport` | `SheetExport` | unchanged, plus a provider column; it was already cloud-agnostic |

- `VPC` → `Network`: "VPC" is the AWS and GCP term but wrong for Azure; "VirtualNetwork" is the
  Azure term. `Network` is neutral. `kubectl get networks` is ambiguous on clusters with other
  `networks` CRDs (for example OpenShift's config and operator groups), and `subnets` already is
  on clusters with Kube-OVN, so every kind joins a `hypersurgery` category (`kubectl get
  hypersurgery`) and gets a prefixed short name (`hsnet`, `hssubnet`); `nscope` stays.
- Object names: AWS keeps the resource ID (`vpc-…`, `subnet-…`), so existing dashboards and
  habits survive. GCP and Azure names are not unique across projects/regions/VNets and ARM IDs are
  too long for object names, so their objects are named
  `<readable name, truncated>-<first 10 hex of sha256(canonical ID)>`; the canonical ID is always
  in `spec.id`.
- `spec.id` is the canonical provider ID: AWS resource ID, GCP relative resource name
  (`projects/p/regions/r/subnetworks/n`), Azure ARM resource ID. `ResourceImport.spec.resourceID`
  takes the same value.
- IP counts: `status.totalIPs`/`availableIPs` become `*int64`, so "unknown" (Azure's `-1`, a
  provider that could not read usage) is distinguishable from "full". `status.ipUsageTime` records
  when the counts were observed. GCP secondary ranges (GKE pods and services) are reported per
  range in `status.gcp`, not added to the subnet's free IPs.
- `status.public` and `status.routeTableID` move to `status.aws`: "public subnet" is an AWS
  routing concept (default route to an internet gateway) with no per-subnet equivalent in GCP.
- `tagKeys` defaults become provider-dependent (filled by the defaulting webhook, not by a static
  CRD default), because `hs/owner` is not a valid key everywhere: AWS keeps `hs/owner`, `hs/env`,
  `hs/tier`, `hs/managed`; GCP and Azure use `hs-owner`, `hs-env`, `hs-tier`, `hs-managed` (see
  the spikes for the key/value character rules). Values are validated per provider too (Azure:
  the encoded per-subnet value must fit 256 characters; GCP: the value must exist as a
  `TagValue`, §6).

### 6. Ownership when subnets cannot carry tags

Ownership (owner, env, tier, the managed marker, `hs/claim`) keeps living **in the cloud**. The
cluster stays a cache that can be rebuilt from discovery, other tools (Terraform, the console,
cost reports) keep seeing who owns what, and several clusters can read the same inventory. What
changes is *where* in the cloud: each provider declares an ownership model, and the core only sees
decoded key/value pairs plus where they came from.

| Model | Used by | Where the metadata for a subnet is |
|---|---|---|
| `ResourceTags` | AWS (VPCs, subnets); Azure VNets; GCP networks and subnets | on the resource: AWS tags, Azure tags, GCP Resource Manager tag bindings |
| `ParentNetworkTags` | Azure subnets | on the parent VNet, one tag per subnet: `hs-subnet-<subnet name>` = `hs-owner=payments;hs-env=prod;hs-tier=db;hs-managed=true` |

- **Azure subnets** use the parent VNet. The entry's value carries the full tag keys, so any
  `tagKeys`, `requiredSubnetTags` or import key round-trips (owner decision 2026-09-29). Tag
  names, the entry's and the keys inside it, are case-insensitive on Azure and matched that way;
  values are case-sensitive. Subnet names (1–80 characters of alphanumerics, `_`,
  `.`, `-`) are always valid in a tag name; the encoded value must fit 256 characters, which the
  webhook checks for claims and imports. The VNet has 50 tags shared with its owners' own tags, so
  the operator writes a per-subnet entry **only when the subnet differs from the VNet's own
  `hs-owner`/`hs-env`** (a subnet without an entry inherits the VNet's, reported as
  `ownershipSource: Network`). When the budget is exhausted, the import or claim fails with reason
  `TagBudgetExceeded` and the `Network` gets a condition; nothing is written half-way. Writes go
  through the Tags API with `operation: Merge` and the Tag Contributor role, so the write identity
  does not need network write permission for imports.
- **GCP** uses Resource Manager tags on both networks and subnets (`params.resourceManagerTags` on
  create, `tagBindings.create` on import; both only add). Keys are pre-created `TagKey`s under an
  organization or project named in the scope (`gcp.tagParent`). Every value must exist as a
  `TagValue`; by default the operator only binds existing values and fails an import with
  `TagValueMissing`, and `gcp.createTagValues: true` lets the write identity create them
  (`roles/resourcemanager.tagAdmin` on those keys). Only direct bindings count as ownership;
  inherited effective tags do not, so a project-level tag does not make every subnet "owned".
  Reading bindings is one call per resource: the provider caches them and refreshes on events
  and on the resync.
- **Tag keys differ per provider** and are defaulted by the webhook (§5): `hs/*` stays on AWS so
  nothing changes for existing tags; `hs-*` on GCP and Azure. `requiredSubnetTags`, `tagKeys`,
  `networkSelector.matchTags` and the auto-import policy keep working on the decoded keys.
- `Subnet.status.ownershipSource` (`Subnet`, `Network`) says where the values came from; the
  contract suite (#45) checks every provider reports it and that writes only add.
- In code, `TagWriter` becomes an `OwnershipWriter` whose provider decides the destination;
  `ResourceImport.spec.tags` keeps its meaning ("these key/values, added, never removed").

Alternatives:

- **Ownership held in the cluster** (a mapping kind, or `ResourceImport` status as the record).
  Rejected as the primary store: losing the cluster loses ownership, it is invisible to every
  other tool, and two clusters would disagree. It stays available as what it already is, the
  audit trail of who imported what.
- **Resource `description`.** Rejected: create-only on GCP, absent on Azure subnets.
- **A taggable resource associated with each subnet** (Azure NSG or route table). Rejected:
  associations change data-plane behaviour and are shared between subnets.
- **One JSON blob per VNet** for all subnets. Rejected: 256 characters is a handful of subnets.
- **GCP labels.** Not available: networks and subnetworks have no labels in v1, beta or alpha.

### 7. Metrics: `hs_*` with a `provider` label

- Every metric is renamed from `hs_aws_<name>` to `hs_<name>` and gains a `provider` label
  (`aws`, `gcp`, `azure`, lowercase like other label values).
- Labels that named AWS concepts are renamed: `vpc_id` → `network_id`, `az` → `zone`.
  `account`, `region`, `scope` keep their names.
- No deprecation window (#44, amended 2026-09-25): 0.9 exports only `hs_*`. The accepted plan
  exported both families for one minor release, with the old one keeping its names and labels;
  the owner dropped the window because the project has no users yet, so nobody's alerts depend
  on the old names and the doubled series would buy nothing. Alerts, the Grafana dashboard, the
  promtool tests and the web dashboard use `hs_*` from 0.9 on, and the upgrade notes carry the
  mapping for rules of one's own.

### 8. What stays provider-specific, and how the API says so

- In the schema: by being inside a provider member (`aws.roleARN`, `aws.writeRoleARN`,
  `aws.externalID`, `SubnetClaim.spec.aws.routeTableID`, `aws.mapPublicIPOnLaunch`,
  `Subnet.status.aws.*`). There is no "AWS only" prose on neutral fields, because there are no
  AWS-only neutral fields.
- Change events are **not** API: they are operator configuration (then `--events-queue-url`,
  from 0.9 `--aws-events-queue-url`).
  Each provider registers an optional event source (#43): AWS EventBridge→SQS, GCP audit logs→
  Pub/Sub (#49), Azure Event Grid→queue (#55). The chart gets `providers.<name>.events.*`.
- What a provider can do is reported, not guessed: `NetworkScope.status.capabilities` lists
  `CreateSubnet`, `ChangeEvents`, `IPUsage`, and the ownership model (§6), so the dashboard and
  the webhooks can refuse, say, a `Create` claim on a provider that cannot create yet.
- Auto-import `fromCreator.principalPrefix` stays neutral: it is matched against the principal as
  the provider's audit trail reports it (AWS session ARN from CloudTrail, GCP
  `authenticationInfo.principalEmail`, Azure caller from the activity log). The docs list the
  format per provider.
- `SheetExport` is cloud-agnostic and unchanged apart from a provider column. Its credentials are
  Google Sheets credentials, independent of the scope's provider.

### 9. Migration from `aws.hypersurgery/v1alpha1`

Conversion webhooks convert between **versions of one CRD**; a CRD is one group, so no webhook can
turn an `aws.hypersurgery` object into a `network.hypersurgery.dev` one
([CRD versioning](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definition-versioning/)).
The objects have to be copied.

Only user-authored objects need moving. `VPC` and `Subnet` are a cache: the new scope rediscovers
them as `Network`/`Subnet`; the old ones stop being updated once their scope is migrated and go
away when the old CRDs are deleted. Cloud resources are untouched: the AWS tag keys stay `hs/*`.

The mapping is mechanical: `provider: AWS`; `roleARN`/`externalID`/`writeRoleARN` into `aws:`;
`vpcTagSelector` into `networkSelector.matchTags`; `tagKeys` written out explicitly as today's
`hs/*` defaults (so the new provider-dependent defaulting cannot change them); `vpcID` →
`networkID`; `availabilityZones` → `zones`; each allocation keyed by the subnet name the
controller already derives (`<namePrefix>-<AZ suffix>`); `routeTableID`/`mapPublicIPOnLaunch` into
`aws:`; `inheritFromVPC` → `inheritFromNetwork`.

Options:

1. **Documented export/import.** `kubectl get -o yaml`, rewrite, `kubectl apply`. Cheapest to
   build. But `SubnetClaim` reservations live in `status`, which `apply` ignores; restoring them
   needs `--subresource=status` per object, and forgetting it makes the next reconcile hand out
   CIDRs again from scratch. Between delete and re-create nothing reconciles.
2. **In-operator migration controller, both groups served for one minor release.** The new
   operator installs both sets of CRDs. A migration reconciler watches old-group `NetworkScope`,
   `SubnetClaim`, `ResourceImport` and `SheetExport` objects and, for each one without a
   counterpart, creates the new-group object with the same name and namespace, then copies the
   status through the status subresource (claim allocations, import state and history). It then
   marks the old object `network.hypersurgery.dev/migrated-to: <name>`. From that moment only the
   new object is reconciled; the validating webhook rejects spec changes to a migrated old object
   with a message naming the new one. The old controllers never run next to the new ones for the
   same object, so nothing is written twice.
3. **Clean break** (uninstall, install the new chart, recreate objects). Rejected: loses
   `Allocate`-mode reservations, and needs a window with no reconciliation. That there are no
   known external users would justify it on cost, but not on correctness, and the owner asked for
   a proper path.

**Decision: option 2**, plus the same conversion function exposed offline as
`manager migrate-manifests < old.yaml > new.yaml`, for GitOps repositories (otherwise Argo CD or
Flux would keep re-applying the old manifests). The old→new mapping is one pure, table-tested Go
function used by both, with round-trip fixtures taken from `examples/`. Created `Create`-mode
subnets are re-adopted through their `hs/claim` tag even if status copying failed, as today.

The migration controller is on by default in the release that introduces the new group and gone
in the next; that release refuses to start (with a clear error and a metric) while unmigrated
old-group objects exist. The chart never deletes CRDs; the upgrade note tells users to delete the
old ones after that release. (Done in 0.9; "refuses to start" became "starts without its
controllers and stays not ready", see the implementation notes for 0.9.)

### 10. Versioning to v1 (#58)

| Release | Served | Storage | Notes |
|---|---|---|---|
| 0.8 | `aws.hypersurgery/v1alpha1` (deprecated), `network.hypersurgery.dev/v1beta1` | v1beta1 | migration controller |
| 0.9 | `network.hypersurgery.dev/v1beta1` | v1beta1 | old group removed (done; see the implementation notes for 0.9); `hs_aws_*` renamed to `hs_*`, with no overlap |
| 1.0 | `v1`, `v1beta1` (deprecated) | v1 | conversion webhook; storage migrated to v1 (done; see the implementation notes for #58) |
| ≥1.2 and ≥6 months after 1.0 | `v1` | v1 | `v1beta1` no longer served |

- The new group starts at **v1beta1**, not v1alpha1: this ADR fixes its shape for 1.0, and
  another alpha would promise less than we intend to keep. Old-group CRD versions get
  `deprecated: true` and a `deprecationWarning`, so `kubectl` warns.
- v1 equals v1beta1 minus whatever beta deprecated. If the schemas are identical, the `None`
  conversion strategy suffices; #58 still ships and tests a conversion webhook (v1 as hub), so the
  first real difference does not have to introduce the machinery under pressure.
- Before `v1beta1` stops being served, every stored object is rewritten at v1 (the operator does
  a no-op update of each object on startup) and `status.storedVersions` is trimmed to `[v1]`, as
  the Kubernetes docs require before removing a version.
- The 1.0 compatibility promise (#58) states: `provider` is an open enum; provider members may be
  added; new optional fields may be added; nothing is removed or renamed within v1.

## Sketch of the new types

Not code: the shape for #42. Kubebuilder markers only where they carry a decision.

```go
// +groupName=network.hypersurgery.dev
package v1beta1

// Provider is an open enum: more values will be added. Clients must skip objects with a
// provider they do not know instead of failing.
// +kubebuilder:validation:Enum=AWS            // GCP, Azure added by the releases that implement them
type Provider string

// One rule per member: a member may only be set for its own provider.
// +kubebuilder:validation:XValidation:rule="!has(self.aws) || self.provider == 'AWS'",message="aws is only valid for provider AWS"
type NetworkScopeSpec struct {
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="provider is immutable"
	Provider Provider `json:"provider"`

	// +listType=map
	// +listMapKey=id
	// +kubebuilder:validation:MinItems=1
	Accounts []Account `json:"accounts"`
	// +kubebuilder:validation:MinItems=1
	Regions []string `json:"regions"`

	NetworkSelector    *NetworkSelector `json:"networkSelector,omitempty"`
	RequiredSubnetTags []string         `json:"requiredSubnetTags,omitempty"`
	TagKeys            TagKeys          `json:"tagKeys,omitzero"` // defaults per provider (webhook)
	ResyncInterval     *metav1.Duration `json:"resyncInterval,omitempty"`
	DiscoverUnmanaged  *bool            `json:"discoverUnmanaged,omitempty"`
	AutoImport         *AutoImportPolicy `json:"autoImport,omitempty"` // inheritFromVPC -> inheritFromNetwork
	// Namespaces whose claims and imports may use the scope. Unset allows none (v1alpha1: all).
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	AWS *AWSScope `json:"aws,omitempty"` // provider-wide AWS settings; empty today, reserved
	// GCP   *GCPScope   `json:"gcp,omitempty"`   // 2.0
	// Azure *AzureScope `json:"azure,omitempty"` // 3.0
}

type NetworkSelector struct {
	// matchTags: all must be present; an empty value matches any value of the key.
	MatchTags map[string]string `json:"matchTags,omitempty"`
}

type Account struct {
	// id: AWS account ID, GCP project ID, Azure subscription ID; format checked per provider.
	ID      string   `json:"id"`
	Regions []string `json:"regions,omitempty"`

	AWS *AWSAccount `json:"aws,omitempty"`
	// GCP   *GCPAccount   `json:"gcp,omitempty"`
	// Azure *AzureAccount `json:"azure,omitempty"`
}

type AWSAccount struct {
	RoleARN      string `json:"roleARN,omitempty"`
	ExternalID   string `json:"externalID,omitempty"`
	WriteRoleARN string `json:"writeRoleARN,omitempty"`
}

// 2.0 and 3.0, shown to check the shape:
type GCPScope struct {
	TagParent       string `json:"tagParent"`                 // organizations/123 or projects/p: owns the hs-* TagKeys
	CreateTagValues bool   `json:"createTagValues,omitempty"` // allow the write identity to create TagValues
}
type GCPAccount struct {
	ServiceAccount      string `json:"serviceAccount,omitempty"`      // impersonated for reads
	WriteServiceAccount string `json:"writeServiceAccount,omitempty"` // impersonated for writes
}
type AzureAccount struct {
	TenantID     string `json:"tenantID,omitempty"` // defaults to the operator's tenant
	ClientID     string `json:"clientID,omitempty"`
	WriteClientID string `json:"writeClientID,omitempty"`
}

type NetworkScopeStatus struct {
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	LastSyncTime       *metav1.Time       `json:"lastSyncTime,omitempty"`
	Networks           int32              `json:"networks,omitempty"` // was vpcs
	Subnets            int32              `json:"subnets,omitempty"`
	Unmanaged          int32              `json:"unmanaged,omitempty"`
	Capabilities       []Capability       `json:"capabilities,omitempty"` // CreateSubnet, ChangeEvents, IPUsage, ...
	Ownership          *Ownership         `json:"ownership,omitempty"`    // §6: {networks: ResourceTags, subnets: ParentNetworkTags}
	Targets            []TargetStatus     `json:"targets,omitempty"`      // account, region, networks, subnets, unmanaged...
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// Network: AWS VPC, GCP VPC network, Azure VNet. Cluster-scoped, operator-written.
type NetworkSpec struct {
	Provider Provider `json:"provider"`
	ID       string   `json:"id"`      // vpc-…, projects/p/global/networks/n, /subscriptions/…/virtualNetworks/n
	Account  string   `json:"account"`
	Region   string   `json:"region,omitempty"` // empty for global networks (GCP)

	// Azure *AzureNetworkRef `json:"azure,omitempty"` // resourceGroup
}

type NetworkStatus struct {
	Name           string            `json:"name,omitempty"`
	State          string            `json:"state,omitempty"`
	CIDRBlocks     []string          `json:"cidrBlocks,omitempty"` // AWS VPC CIDRs, Azure address space; empty for GCP
	IPv6CIDRBlocks []string          `json:"ipv6CIDRBlocks,omitempty"`
	Owner, Env     string            // json tags omitted in the sketch
	Tags           map[string]string `json:"tags,omitempty"`
	Subnets        int32             `json:"subnets,omitempty"`
	TotalIPs       *int64            `json:"totalIPs,omitempty"`
	AvailableIPs   *int64            `json:"availableIPs,omitempty"`
	OverlapsWith   []string          `json:"overlapsWith,omitempty"`

	AWS *AWSNetworkStatus `json:"aws,omitempty"` // isDefault
}

type SubnetSpec struct {
	Provider  Provider `json:"provider"`
	ID        string   `json:"id"`
	NetworkID string   `json:"networkID"`
	Account   string   `json:"account"`
	Region    string   `json:"region"`
}

type SubnetStatus struct {
	Name               string            `json:"name,omitempty"`
	State              string            `json:"state,omitempty"`
	CIDRBlock          string            `json:"cidrBlock,omitempty"`
	SecondaryCIDRBlocks []string         `json:"secondaryCIDRBlocks,omitempty"` // GCP secondary ranges, extra Azure prefixes
	IPv6CIDRBlocks     []string          `json:"ipv6CIDRBlocks,omitempty"`
	Zone               string            `json:"zone,omitempty"` // AWS only today
	TotalIPs           *int64            `json:"totalIPs,omitempty"`
	AvailableIPs       *int64            `json:"availableIPs,omitempty"` // nil: unknown
	UtilizationPercent *int32            `json:"utilizationPercent,omitempty"`
	IPUsageTime        *metav1.Time      `json:"ipUsageTime,omitempty"`
	Owner, Env, Tier   string
	OwnershipSource    string            `json:"ownershipSource,omitempty"` // see §6
	Tags               map[string]string `json:"tags,omitempty"`
	MissingTags        []string          `json:"missingTags,omitempty"`

	AWS *AWSSubnetStatus `json:"aws,omitempty"` // availabilityZoneID, public, routeTableID
	// GCP   *GCPSubnetStatus   // purpose, secondaryRanges{name,cidr}, privateIPGoogleAccess
	// Azure *AzureSubnetStatus // resourceGroup, delegations, routeTableID, networkSecurityGroupID
}

type SubnetClaimSpec struct {
	ScopeRef     string            `json:"scopeRef"`
	Account      string            `json:"account"`
	Region       string            `json:"region"`
	NetworkID    string            `json:"networkID"` // was vpcID; canonical ID
	PrefixLength int32             `json:"prefixLength"` // bounds checked per provider by the webhook
	Zones        []string          `json:"zones,omitempty"`  // AWS: required, 1-6; others: forbidden
	Count        *int32            `json:"count,omitempty"`  // providers without zones; default 1
	Mode         ClaimMode         `json:"mode,omitempty"`   // Allocate | Create
	Owner        string            `json:"owner"`
	Env, Tier    string
	NamePrefix   string            `json:"namePrefix,omitempty"`
	Tags         map[string]string `json:"tags,omitempty"`

	AWS *AWSClaimOptions `json:"aws,omitempty"` // routeTableID, mapPublicIPOnLaunch
}

type SubnetAllocation struct {
	Name      string `json:"name"`           // list map key (was availabilityZone)
	Zone      string `json:"zone,omitempty"`
	CIDRBlock string `json:"cidrBlock"`
	SubnetID  string `json:"subnetID,omitempty"` // canonical ID
	State     string `json:"state,omitempty"`
	Error     string `json:"error,omitempty"`
}

type ResourceImportSpec struct {
	ScopeRef    string            `json:"scopeRef"`
	Account     string            `json:"account"`
	Region      string            `json:"region,omitempty"`
	ResourceID  string            `json:"resourceID"` // canonical ID of a Network or Subnet
	Tags        map[string]string `json:"tags"`       // where they are written: §6
	RequestedBy string            `json:"requestedBy,omitempty"`
	DryRun      bool              `json:"dryRun,omitempty"`
}
```

## Decision record

Accepted by the owner on 2026-09-24 with these answers to the open questions: the group is
`network.hypersurgery.dev`; the project is renamed to `subnet-operator` (#76); the old group and
the `hs_aws_*` metrics get one minor release of overlap (0.8) and are removed in 0.9. (Planned
as 0.7/0.8 when accepted; moved by one on 2026-09-25, when 0.7.0 shipped the security fixes
and `namespaceSelector` on the old API first. The metrics then moved by one more, to an
overlap in 0.9 and removal in 0.10, because 0.8 shipped without them. On 2026-09-25 the owner
dropped that overlap: with no users yet, 0.9 renames the metrics outright and renames the alert
`VPCCIDROverlap` to `NetworkCIDROverlap` with them; see the implementation notes for #44 below.)
Whether the GCP write identity may create tag values, and the Azure VNet tag budget, are decided
in phases 5b and 5c; neither affects the 1.0 API. (Those phases are now the 2.0 and 3.0
releases, below; neither decision changes v1 beyond the additive `gcp` and `azure` members.)

**Providers ship as major releases (owner decision, 2026-09-25).** The GCP provider ships as
2.0.0 and the Azure provider as 3.0.0, not as 1.x releases as this ADR first planned. Under
semver a major release may carry breaking changes, and GCP needs one already: the ownership tag
keys the operator writes (`hs/owner`, `hs/managed`, `hs/managed-by`, `hs/claim`) are AWS keys for
every provider today and must become provider-dependent, `hs/*` on AWS and `hs-*` on GCP and
Azure (§5, §6, and the implementation notes for #43/#45). A major release adds a provider and
may include the breaking changes that provider needs, each with its own section in the upgrade
guide, under the [deprecation rules](../policy.md#deprecation); 1.x keeps its promise as
written. The API design is unchanged: 2.0 and 3.0 stay on `network.hypersurgery.dev/v1` and add
the `provider` value and the provider members to it, which is additive; their breaking changes
are to behaviour and tag keys, not to v1's fields. A `v2` comes only if the API itself has to
break, served next to v1 with a conversion. The policy is in
[policy.md](../policy.md#major-releases-and-new-providers).

## Consequences

- #42 implements `network.hypersurgery.dev/v1beta1` with the migration controller and the
  offline converter, both served for one minor release; the chart ships both CRD sets for that
  release.
- #43 moves the account identity into per-provider members and the event sources into provider
  registration; `inventory.Target` gains `Provider` and carries a provider-specific credential
  reference instead of `RoleARN`/`ExternalID`.
- #44 uses the names and labels in §7.
- #45's contract suite covers the ownership model a provider declares (§6), nil IP usage, and
  the per-provider tag key/value rules.
- GCP (#46, #47): ownership through Resource Manager tag bindings with pre-created keys; IP usage
  from `WITH_UTILIZATION`; discovery through the Compute API per project (Cloud Asset Inventory is
  hours stale and only fit for finding projects).
- Azure (#52, #53): subnet ownership in parent-VNet tags with the budget rule above; IP usage from
  the VNet usages API with `-1` as unknown; Resource Graph may list, but allocation and writes
  always read ARM first (Resource Graph is eventually consistent). (#52 lists with ARM too; see
  its implementation notes.)
- The chart and repository are renamed from `aws-subnet-operator` to `subnet-operator` (#76), the
  name the image, the Go module and the public mirror already use. It ships in the same release as
  the new group, so users migrate once.
- `NetworkScope.spec.namespaceSelector` (#69) carries over unchanged, with one difference: in
  `aws.hypersurgery/v1alpha1` an unset selector allows every namespace, for compatibility, and in
  the new group it allows **none**. The migration controller and the offline converter (#42)
  must therefore turn an unset v1alpha1 selector into an explicit empty selector (`{}`, every
  namespace) rather than dropping it, so that migrating does not lock teams out; they should
  say so, because the result is the permissive setting the new default exists to avoid.
- The `aws.hypersurgery/created-by` annotation (#70) becomes `network.hypersurgery.dev/created-by`.
  The migration controller creates the new objects as the operator, and the webhook stamps the
  requesting user on every create, so a migrated object would name the operator. #42 has to keep
  the original creator: either the new-group webhook accepts a copied value when the requesting
  user is the operator itself, or the original goes into a second annotation that the webhook
  guards the same way. **Decided in #42: the first**, narrowed to objects the operator creates
  with the `network.hypersurgery.dev/migrated-from` marker (see below).
- Adding a provider after 1.0 is additive to the API: one enum value, one member per kind that
  needs one, one provider registration. The operator release that adds it is a major release
  all the same (2.0 for GCP, 3.0 for Azure; see the decision record), because of what it
  changes outside the API, such as the tag keys. Anything a new provider needs in the API beyond
  that is a sign this ADR was wrong, and a new ADR.

## Implementation notes (#42, #76)

What the implementation decided where this ADR left room, or deviates from its letter:

- **Created-by.** The new group's webhook keeps the `created-by` value an object carries only when
  the requesting user is the operator itself (as found by a `SelfSubjectReview` at startup) and
  the object carries `network.hypersurgery.dev/migrated-from`; anybody else setting the marker
  gets their own name. Such a copy also skips the webhook's content checks, which the old group
  already made: a migrated claim whose subnets fill its network would otherwise be refused before
  its reservations are copied in. An object from before 0.7 without a creator gets none.
- **Ordering.** Scopes migrate first; claims, imports and exports wait for their scope. Until an
  object's old counterpart is marked `migrated-to`, its new-group controller does nothing, and it
  then reads the object past the cache, because the status is copied before the mark. Claims also
  count the CIDRs of unmigrated old claims as used.
- **Old `VPC` and `Subnet` objects** of a migrated scope are deleted by the migration instead of
  waiting for the old CRDs to go: left in place they would keep reporting a stale inventory, and
  `kubectl get subnets` resolves to the old group while it is installed.
- **The old group keeps its webhooks** in 0.8. They stamp and guard `aws.hypersurgery/created-by`
  (the value the migration copies), check an old object with the new group's validators on its
  converted form, and refuse spec changes to migrated objects.
- **Deferred, additive later:** `SubnetClaim.spec.count`, `Subnet.status.secondaryCIDRBlocks`,
  `ipUsageTime`, `ownershipSource`, `NetworkScope.status.capabilities` and `ownership`, and every
  GCP and Azure member. Only what would be breaking to change later ships in v1beta1. (#43/#45
  then added `capabilities`, `ownership` and `ownershipSource`; see below.)
- **Per-provider validation.** `prefixLength` is 1–32 in the schema, 16–28 for AWS in the
  webhook and the controller; account ID formats are CEL rules on the scope, and on claims and
  imports webhook and controller checks, because those do not know their provider. `managedTag`
  lost its static CRD default for the same reason; the webhook and the policy default it per
  provider.
- **Metrics** keep their `hs_aws_*` names in this change; §7 is #44, below.
- **The leader election lease** keeps its name across the rename (#76), so a 0.7 and a 0.8 pod
  never lead at once during the rolling upgrade.

## Implementation notes (#44)

- **Window.** 0.8 shipped the new group without the new metrics. The first implementation
  exported both families in 0.9, recorded from one set of labels, and planned the removal of
  `hs_aws_*` for 0.10, as the [policy](../policy.md#deprecation) asks of a deprecated metric.
  The owner then dropped the window (no users yet): 0.9 exports `hs_*` only, the policy records
  the exception, and §10 is updated to match. A test checks that nothing is exported under
  `hs_aws_` any more.
- **Names.** `hs_aws_vpc_cidr_overlaps` becomes `hs_network_cidr_overlaps`, not a name that
  keeps `vpc`: the kind is `Network`, and a neutral name that says VPC would be renamed
  again. Every other metric is `hs_<name>` as §7 says.
- **Label values.** The unmanaged metrics' `kind` is `network` (`vpc` in 0.8), for the same
  reason. `provider` on the claim and import readiness gauges is their
  scope's, empty while the scope does not exist.
- **Alerts** keep their names, except `VPCCIDROverlap`, which becomes `NetworkCIDROverlap` like
  the metric it reads. With the window gone there is no release in which the old name could
  have kept routes working anyway, so it is renamed now rather than as a separate breaking
  change later. Their labels and text are neutral.
- **Counters start at zero.** `hs_target_sync_errors_total` and `hs_unmanaged_resources_total`
  exist at 0 from a target's first sync, and `hs_auto_imports_total` at 0 for each of its four
  results once the target's scope runs the auto-import policy. A series that first appears at 1
  is invisible to `increase()`, so without this the first unmanaged resource or the first
  auto-import after a restart did not alert.

## Implementation notes (#43, #45)

- **`internal/provider`** defines `Provider` and a `Registry`. A provider bundles what the core
  needs from one cloud: its name, capabilities and ownership model; the identity for an account
  (read or write, from the account's member); the provider-specific checks of scopes, claims,
  imports and tags (used by the webhooks and, as condition reasons, by the controllers); the
  `Discoverer`, `SubnetWriter` and `OwnershipWriter`; and an optional change-event source. The
  controllers and webhooks look the provider up by `spec.provider` and no longer name AWS. A
  scope of a provider the operator does not run is refused by the webhook and reported
  `ProviderNotEnabled` by the controllers, never synced.
- **`inventory` is neutral**: `Network`/`Subnet`/`Snapshot` with `NetworkID`, `Zone`,
  `TotalIPs`/`AvailableIPs` as pointers (unknown is nil; the provider computes the total, since
  reserved addresses differ per cloud), `OwnershipSource`, and the provider's own details in a
  member typed with the API's status structs (`AWS *AWSSubnetStatus`), which the controllers copy
  unread. `Target` has `Provider` and an `Identity` (an interface each provider implements; the
  AWS one is role ARN plus external ID) instead of `RoleARN`/`ExternalID`, and `NetworkSelector`
  instead of `VPCTagSelector`. `TargetKey` stays account/region: account IDs of the three clouds
  cannot collide (see §3). `TagWriter` became `OwnershipWriter` (§6).
- **The AWS provider** is `internal/cloud/aws.Provider`; its change events moved to
  `internal/cloud/aws/events`. The operator runs the providers named by `--providers` (default
  `aws`); per-provider flags are prefixed (`--aws-events-queue-url`), and the chart has
  `providers.aws.{enabled,irsaRoleARN,region,endpointURL,events.*}` with the 0.8 names kept until
  0.10 (removed in 1.0 instead, the release after 0.9; see the notes for #58). GCP Workload
  Identity Federation and Azure Workload Identity become
  `providers.gcp.*`/`providers.azure.*` values that render the service account annotations and pod
  labels those need.
- **API additions** (optional status fields, so additive): `NetworkScope.status.capabilities`
  (`CreateSubnet`, `ChangeEvents`, `IPUsage`; an open set, `ChangeEvents` only while an event source
  runs), `NetworkScope.status.ownership` (`{networks, subnets}` of `ResourceTags` or
  `ParentNetworkTags`) and `Subnet.status.ownershipSource` (`Subnet` or `Network`). A Create-mode
  claim on a provider without `CreateSubnet` is refused (`CreateNotSupported`). `ipUsageTime`,
  `secondaryCIDRBlocks` and `count` stay deferred: nothing reports them yet.
- **The contract suite** (`internal/provider/providertest`) runs against a `Fixture` (the provider
  plus a way to arrange its cloud) and never assumes an empty account, so it runs against the
  in-memory EC2 in `make test` and against Moto in the e2e job. Clauses a fixture cannot
  arrange (throttling, unknown IP usage on Moto) are skipped there, not dropped. It checks that the
  tag keys the operator writes (`hs/owner`, `hs/managed`, `hs/claim`, ...) are valid for the
  provider: they are AWS keys today, so GCP and Azure must make them provider-dependent before
  they can pass (§5). That is the breaking change already known for 2.0 (decision record).

## Implementation notes (0.9: the old group removed)

- **Removed:** the `aws.hypersurgery` CRDs from `config/crd` and the chart, `api/v1alpha1`, the
  old group's webhooks, the migration controller and `--migrate-v1alpha1`, the controllers'
  wait for an object's old counterpart and the claim allocator's view of unmigrated old claims,
  and the old group's RBAC but `list`. The webhooks' exception for a copy the operator creates
  with `network.hypersurgery.dev/migrated-from` (keep its creator, skip the checks) went with the
  migration: nothing creates such copies any more, and an exception nobody needs is only a way
  in. The marker stays on the objects 0.8 migrated, as a record that grants nothing.
- **Kept:** `manager migrate-manifests`, for GitOps repositories, with the mapping of §9. It
  converts metadata and spec only (manifests carry no state), and reads the old kinds with Go
  types kept in `internal/migration/v1alpha1`, which is no API: no scheme, no CRD, no deepcopy.
  Its removal is not scheduled; it has no cluster access and costs little to keep.
- **The guard, instead of refusing to start.** An operator that exits is restarted by the
  kubelet into a crash loop, whose metrics nobody scrapes and whose reason is a log line away.
  So the operator lists the four user-written old kinds (metadata only) before it builds its
  manager. With an object that lacks `migrated-to` (or when it cannot list them), it runs a
  manager with nothing but the metrics and the probes: no controllers, no webhooks and no leader
  election, so a 0.8 replica still running keeps the lease; the readiness probe fails with the
  reason, the log names the objects, each gets a `MigrationPending` Warning Event, and
  `hs_migration_pending_objects` counts them. It looks again every 30 seconds and exits once
  nothing is left, to be restarted into normal operation. A rolling upgrade from 0.8 therefore
  stops at the first new pod, with the 0.8 pods still serving. A running operator keeps counting
  every minute and warns about an old object that appears later, without stopping.
- **No chart-side check.** A pre-upgrade check in the chart (a `lookup` of the old objects, or a
  hook Job) was considered and left out: `lookup` sees nothing under `helm template`, Argo CD or
  Flux and needs the person running Helm to be able to list the old group cluster-wide, and a
  hook Job needs a service account, RBAC and a pod of its own, which the namespace's Pod Security
  and network policies must admit. The operator's
  guard covers every way of installing, and the rollout it stops is the same signal
  `helm upgrade --wait` reports.
- **Upgrade test.** It starts from the published 0.8.0, creates old-group objects and lets 0.8
  migrate them, then checks after the upgrade that what 0.8 migrated is unchanged, that the guard
  blocks a restarted pod while an unmigrated old object exists and lets go once it is deleted,
  and that the operator carries on after the old CRDs are deleted. 0.7 to 0.9 directly is not
  supported and not tested.
- **For #58 (v1).** `network.hypersurgery.dev` has one version and one set of webhooks; v1 is
  added next to v1beta1 in the same group, where a conversion webhook can do what §9 had to do
  by copying. `migrated-from` and `migrated-to` are annotations, not fields, so they need no
  conversion. The guard and `hs_migration_pending_objects` are about the old group only and can
  stay as they are until the old group is gone from supported upgrade paths.

## Implementation notes (#58: v1)

- **v1 next to v1beta1.** Both versions are in `network.hypersurgery.dev`, scaffolded with
  `kubebuilder create api` and `create webhook --conversion --spoke v1beta1`. v1 is the storage
  version and the hub; v1beta1 is served with `deprecated: true` and a warning. The operator,
  its controllers, webhooks, dashboard, examples, samples and `migrate-manifests` use v1; only
  the conversion and the upgrade test know v1beta1.
- **A real conversion, for identical fields.** v1 has v1beta1's fields, so the spoke converts a
  spec or a status through its JSON form, and refuses a field the other side lacks instead of
  dropping it. The first field one version has and the other lacks therefore fails the round
  trip loudly, which is the moment to write that conversion by hand. Random objects of every
  kind, status included, go through both round trips in the tests. Going through Go types
  respells values (`5m` becomes `5m0s`, an explicit `false` disappears), which a GitOps tool
  reading at v1beta1 would report as drift, so the webhook returns the object as it was
  written, with the new apiVersion, wherever that decodes to exactly what the typed conversion
  produced (`ExactConversion` in `internal/webhook/v1`). The upgrade test found this.
- **Schema fixes before freezing.** Cheap, and impossible later: `regions`,
  `accounts[].regions` and `requiredSubnetTags` became sets, `autoImport.accountDefaults` a map
  keyed by `account`, and `scopeRef` 1–253 characters. The admission webhook refuses the same
  duplicates at v1beta1, which the v1beta1 schema cannot. Reviewed and left as they are:
  `provider` stays an enum listing only `AWS`, documented as open (adding values is additive);
  every top-level status field is optional (inside a status list, only what the operator
  always writes is required); `TargetStatus` stays an atomic list the operator alone writes; the claim and import `state` fields stay open strings. `ipUsageTime`,
  `secondaryCIDRBlocks` and `SubnetClaim.spec.count` stay deferred: each is an optional field
  that can be added in a 1.x release.
- **Storage migration in the operator.** On every start the leader looks at each CRD's
  `status.storedVersions`. Where it lists more than `[v1]` and the CRD already stores v1, it
  writes every object back unchanged through the status subresource (the API server re-encodes
  it at v1; neither the generation nor the admission webhooks are involved), then trims the
  list to `[v1]`, with an Event on the CRD, a log line and two metrics
  (`hs_crd_stored_versions`, `hs_storage_migration_rewritten_objects_total`). A later start
  finds `[v1]` and does nothing. The Kubernetes storage version migrator was the alternative;
  it is one more component for users to run, for a job that happens once.
- **The conversion is configured by the operator, not by the CRD files.** Helm installs
  `crds/` verbatim, so the files cannot name the release's Service or carry its CA. The CRDs
  ship with the `None` strategy, exact while the fields are the same. Once the storage is
  migrated, the leader points each CRD at its own webhook with the CA from `ca.crt` of the
  webhook certificate (the chart's own or cert-manager's) and keeps it current every minute;
  with the webhook off it sets `None`. The operator may `get` and `patch` its six CRDs, and
  `get` and `update` their status, by name and nothing else. The kustomize install instead
  has kubebuilder's conversion patches and cert-manager's CA injector among its `[WEBHOOK]` and
  `[CERTMANAGER]` sections, off by default like the admission webhooks, and there the operator
  leaves the conversion alone (`--crd-conversion` empty). The OLM bundle (#74) does the
  same, with OLM configuring the conversion and injecting the CA ([OLM](../olm.md)).
- **Admission for v1 only.** The webhooks are registered for v1; the API server converts a
  v1beta1 request before it calls them (`matchPolicy: Equivalent`), so the checks are the same
  at both versions.
- **Upgrade test.** It starts from the published 0.9.0, creates objects at v1beta1, applies the
  1.0 CRDs, upgrades, and checks that `storedVersions` ends as `[v1]`, that the conversion
  points at the release's webhook with the chart's CA, that every object and its status
  survived, that v1beta1 reads return what v1 reads do, and that a second start rewrites
  nothing.

Owner decisions on the open questions of #58 (2026-09-25):

- **The operator keeps patching its own CRDs' conversion**, by name. Rendering the CRDs as chart
  templates was the alternative; it would hand them to Helm, so that `helm uninstall` deletes
  them with every object. The residual risk is in the
  [threat model](../security/threat-model.md#residual-risks-accepted-for-10).
- **Kustomize stays webhook-less by default.** An e2e deployment with cert-manager is deferred,
  not a 1.0 blocker (#86).
- **`mode` and `autoImport.mode` are closed enums**: a new value is a behaviour change and needs
  a new API version or an opt-in field ([API compatibility](../api-compatibility.md#closed-enums)).
- **A security fix may tighten validation within v1**, announced as breaking, with stored objects
  kept working on unchanged updates ([API compatibility](../api-compatibility.md#security-fixes)).
- **What 0.9 deprecated for removal in 0.10 is removed in 1.0**: `events.*`, `aws.*`,
  `networkPolicy.egress.podIdentity`, `--events-queue-url`, `--events-debounce` and
  `EVENTS_QUEUE_URL`. The chart refuses the values and the operator the flags, naming the
  replacement ([policy](../policy.md#deprecation)).
- **Rollback to 0.9 stays a documented `kubectl patch`** of the six CRDs back to `None`, run as
  written by `internal/crdversions/rollback_test.go`.
- **The frozen v1 schema baseline** (`api/v1/testdata/crds-1.0`) is refreshed in the 1.0
  release commit and never after ([release steps](../release.md)).

## Implementation notes (#46: GCP discovery, 2.0)

What the first part of 2.0 decided where this ADR left room. Writes (#47), credentials (#48) and
events (#49) come next and are not in it.

- **Tag keys per provider.** `networkv1.OperatorTagKeysFor(provider)` holds every key the
  operator itself writes or defaults to: owner, env, tier, the managed marker, managed-by and the
  claim key, and how the claim's value is spelled. AWS keeps `hs/*` and `<namespace>/<name>`,
  byte for byte; GCP has `hs-*` and `<namespace>_<name>` (a tag value cannot contain `/`, and
  neither a namespace nor a name can contain `_`). The hard-coded constants left `api/v1`, and
  the claim controller, the auto-import policy and the webhooks take the keys of the scope's
  provider. GCP tag keys and values are checked against the rule both have always met (1–63
  letters, digits, `-`, `_`, `.`, alphanumeric at both ends); Google has at times allowed more in
  values, and the research note could not confirm the current rule.
- **API.** `provider: GCP`; `NetworkScope.spec.gcp.tagParent` (`organizations/<number>` or
  `projects/<project ID>`), required with GCP by a CEL rule, like the rules for the project ID
  format; `Network.status.gcp` (routing mode, auto mode); `Subnet.status.gcp` (purpose, role,
  stack type, IPv6 access type, Private Google Access, secondary ranges with their usage) and the
  neutral `Subnet.status.secondaryCIDRBlocks`. v1beta1 gets the same fields, so the conversion
  stays exact. Left for the issues that need them, all additive: `accounts[].gcp` (service
  accounts, #48), `gcp.createTagValues` and a claim's `gcp` options (#47), `SubnetClaim.spec.count`
  and `ipUsageTime`.
- **Object names** follow §5: `networkv1.ObjectName(id)` is the ID itself when it is a valid
  name and label value (every AWS ID), and `<last segment>-<10 hex of sha256(id)>` otherwise. The
  `network` and `resource` labels carry it too, since a GCP ID is no label value.
- **Global networks.** GCP targets stay project × region. `networks.list` is global, so every
  target of a project reports the project's networks with an empty region; the controller keeps
  one `Network` per network, labelled with an empty region, adds up its subnets over every region
  of the scope after all targets are synced, keeps it while its project is in the scope, and
  deletes it only after a reconcile in which every target of the project synced and none reported
  it. `status.unmanaged` counts a resource once; the per-target unmanaged gauges still count a
  global network in each region's series, which is what that region's target reports.
- **Discovery** per target: `networks.list` and the region's `subnetworks.list` with
  `views=WITH_UTILIZATION` (rather than `aggregatedList`: the targets are regional, and so are the
  read quotas). Ownership comes from `effectiveTags.list`, one call per network and subnetwork,
  eight at a time, at the global endpoint for networks and the region's endpoint for
  subnetworks, with resources named by project number (`projects.get`, cached). Only direct
  bindings count (§6); a key of `tagParent` is reported by its short name, any other by its
  namespaced name, so two parents' `hs-owner` cannot be confused. Tags are not cached yet: a
  change shows at the next sync, which without events (#49) is the resync interval.
- **IP usage.** The primary range's total is its size less the 4 reserved addresses, its free
  count Compute's `totalFreeIp`, capped at the total (whether Google counts the reserved
  addresses as allocated is for the conformance run, #50). Secondary ranges report their own
  counts in `status.gcp.secondaryRanges` and are not added to the subnet's. A subnetwork without
  utilization in the answer reports its free addresses as unknown.
- **Throttling.** 429, 403 with reason `rateLimitExceeded`/`userRateLimitExceeded`, and gRPC
  `RESOURCE_EXHAUSTED` are throttling; any other 403 is not. Each call is tried five times,
  with a delay per quota bucket (Compute per project and region, Resource Manager per endpoint)
  that doubles from 1 s up to a minute while calls are throttled and halves as they get through;
  what is still throttled after that is `ErrThrottled`, and the controller's per-target backoff
  takes over.
- **Credentials.** Every project is reached with the operator's own identity (Application
  Default Credentials). `gcp.Identity` has the service account #48 will impersonate, and the
  discoverer refuses one it cannot use yet rather than fall back to its own. (#48 implemented
  it, see below.)
- **What it refused** until #47 (below). Capabilities were `IPUsage` only. `CreateSubnet` and `WriteOwnership`
  return `ErrNotSupported`; the webhooks and controllers refuse every claim and import of a GCP
  scope (`NotSupported`) and a GCP scope with an auto-import policy other than `Off`.
- **Tests.** There is no Compute emulator, so `internal/cloud/gcp/gcpfake` serves the Compute and
  Resource Manager REST calls over HTTP, and the provider runs against it with the real Google
  client libraries. The contract suite gained two fixture questions (the region networks are
  reported in, and whether networks have address space of their own), and a provider whose
  `WriteOwnership` returns `ErrNotSupported` skips the ownership-write clause only if it refuses
  imports too.

## Implementation notes (#47: GCP writes, 2.0)

- **Creating a subnetwork.** `subnetworks.insert` in the claim's region, no zone, with the tags
  in `params.resourceManagerTags` (tag key and value IDs), so a subnetwork never exists without
  its ownership. `privateIPGoogleAccess` comes from the claim. A range Compute refuses because
  it conflicts or overlaps with another is `ErrCIDRConflict` (Compute's exact error is not
  documented; it is recognised by status 400 and its message, to be confirmed by the
  conformance run, #50). A name already taken by a subnetwork of the same network and range is
  the one an earlier attempt created, and counts as created; any other taken name is an error.
- **Claims without zones.** A claim on a provider without zones has one zone-less slot: one
  allocation, keyed by the subnetwork's name (`namePrefix`, without a suffix; §4). The CIDR is
  carved from `spec.gcp.poolCIDRs`, because a GCP network has no address space of its own (the
  ADR assumed the network's blocks). The primary and secondary ranges of the network's discovered
  subnetworks and other claims' reservations are taken; subnetworks in regions the scope does
  not cover are not known, and a clash with one comes back from Compute as a conflict. There is
  no `Name` tag on GCP: the subnetwork carries its name itself. `count` and secondary ranges to
  create stay deferred; both are additive.
- **Claim tag value.** `<namespace>_<name>`; a GCP tag value is at most 63 characters, so a
  longer one keeps its first characters, then `-` and the first 10 hex digits of the SHA-256 of
  the whole value. It stays unique and valid, and the adoption of a created subnetwork by its
  claim tag works the same.
- **The claim tag is optional on GCP** (owner decision before 2.0.0). A value per claim either
  has to be created before each claim or piles up towards the key's 1,000 values, since the
  operator never deletes one. `NetworkScope.spec.gcp.claimTag` is `Skip` by default: the
  subnetwork carries the ownership tags only, the claim linkage is the claim's
  `status.allocations[].subnetID`, and a claim that lost its status adopts the subnetwork its
  `namePrefix` names when it carries `hs-managed-by` and the claim's owner, no other claim's
  tag, and no other claim leads to that name or holds it. `Bind` keeps the claim tag and the
  adoption by it. Binding only when the value already exists was considered and rejected: the
  tag would then depend on who created which value when, and adoption could not rely on it.
  AWS always writes `hs/claim`.
- **Ownership writes only add.** `tagBindings.create` at the resource's location (global for a
  network, the region for a subnetwork). The direct bindings are read first: a key bound to the
  same value is left alone, and a key bound to another value refuses the whole write with
  `ErrOwnershipConflict` (`TagValueConflict`) before anything is bound, because GCP binds one
  value per key and replacing it means deleting the binding. AWS keeps replacing values
  (`CreateTags`); `Provider.ReplacesOwnershipValues` says which, the import webhook refuses the
  conflict on GCP where it warns on AWS, and the contract clause "writes ownership by adding"
  checks both behaviours. A network import may leave `spec.region` empty.
- **Tag values.** Keys and values are resolved by namespaced name
  (`<organization number or project ID>/<key>/<value>`) under `gcp.tagParent`. Keys are never
  created (`TagKeyMissing`). A missing value fails with `TagValueMissing`, naming it, unless the
  scope sets `gcp.createTagValues: true` (owner decision: an explicit opt-in); then the write
  identity creates it. A key holds at most 1,000 values; Resource Manager then refuses the
  create, which is `TagValueLimitReached` (recognised by its message, #50 to confirm). Nothing
  is cached: tag values are resolved per write, and bindings are read again before each
  import, until events (#49) give a reason to cache.
- **Scopes.** Auto-import in `DryRun` and `Apply` works on GCP. Two GCP scopes over different
  regions of the same project are accepted with a webhook warning naming the other scope (owner
  decision), since both report the project's global networks; the same project and region is
  refused as on AWS.
- **Identity.** Every write call (subnetwork insert, tag lookups, tag value creation, bindings
  and the binding reads before them) uses `Provider.Identity(account, Write)`: the account's
  `gcp.writeServiceAccount` (#48, below), or the operator's own identity when empty. The write
  identity gets the permissions below and the read identity none of them. The contract suite
  checks it: with the merge of #48 a fixture's account takes writes from its write identity
  alone (`gcpfake` `RestrictProjectWrites`), a clause creates a subnet and writes ownership
  through it, and one the operator cannot act as fails those writes as it fails discovery.
- **Permissions of the write identity** (the `deploy/gcp` writer, tag user and tag value creator
  roles, #48; #51):
  - creating subnetworks: `compute.subnetworks.create` and `compute.networks.updatePolicy` on
    the host project (`roles/compute.networkAdmin`), plus `compute.subnetworks.createTagBinding`
    for the tags given at creation;
  - binding tags: `resourcemanager.tagValueBindings.create` on the tag values
    (`roles/resourcemanager.tagUser` on the keys or the tag parent) and
    `compute.subnetworks.createTagBinding` / `compute.networks.createTagBinding` on the resources;
  - looking up tags: `resourcemanager.tagKeys.get` and `resourcemanager.tagValues.get`
    (`roles/resourcemanager.tagViewer`, included in `tagUser`), `resourcemanager.projects.get`;
  - with `gcp.createTagValues`: `resourcemanager.tagValues.create` on the `hs-*` keys
    (`roles/resourcemanager.tagAdmin` on those keys, not on the parent).
  Which predefined roles are the narrowest is for the conformance run (#50) to confirm.

## Implementation notes (#48: GCP credentials, 2.0)

- **API.** `accounts[].gcp` is `GCPAccount{serviceAccount, writeServiceAccount}`, both optional
  service account emails (`…@….gserviceaccount.com`, checked by the schema and again by the
  webhook), in v1 and v1beta1. A CEL rule allows the member only with provider GCP; it is in
  `allowedNewRules`, since 1.0 had no `gcp` member. Reading and writing as the same service
  account is accepted with a warning. Unlike an AWS role, a service account is not tied to the
  account's ID: a central one per team may read many projects.
- **Identities.** `Provider.Identity` gives reads the account's `serviceAccount` and writes its
  `writeServiceAccount`, each the operator's own identity when empty. So, as on AWS, a project
  read through a service account but without a write one is reported by
  `MissingWriteIdentity`, and its claims can only be allocated.
- **The operator's own identity** is Application Default Credentials, with no code of the
  operator's: GKE Workload Identity (the chart's `providers.gcp.workloadIdentity`, the
  `iam.gke.io/gcp-service-account` annotation) or Workload Identity Federation (`providers.gcp.wif`:
  an `external_account` credential configuration, rendered by the chart from the pool provider
  or brought in a ConfigMap or Secret, and a projected service account token with the provider's
  audience). The research note's recommendation (direct principal bindings, optional
  impersonation per project) is what this supports; which it is, is a matter of IAM bindings.
- **Impersonation** is the AssumeRole of GCP: the own identity calls the IAM Service Account
  Credentials API's `generateAccessToken` for the service account (scope `cloud-platform`; IAM
  roles limit the token, see `deploy/gcp`), one token source per service account, reused until
  five minutes before it expires. The API clients are kept per identity as before, so the read
  and the write service account never share a token. `google.golang.org/api/impersonate` was
  not used: its endpoint cannot be changed, so no test could run against a fake, and its errors
  are strings. The clients are built with a context that outlives the call that built them,
  since a Workload Identity Federation token exchange keeps its context for every renewal.
- **Failures.** A token that cannot be had fails the target before any Compute call, with an
  `ImpersonationError` that names the service account and the permission to grant
  (`iam.serviceAccounts.getAccessToken`); it is `SyncFailed` on the scope and the target's
  `error`, never throttling, and is retried on the next sync. A throttled token request is
  `ErrThrottled`. The contract suite gained two clauses for fixtures that can arrange identities
  of an account's own: discovery goes through the read identity (kept apart from the write
  one), and one the operator cannot act as fails without being reported as throttling. The AWS
  fixtures cannot arrange roles and skip them.
- **Least privilege.** `deploy/gcp` has custom role definitions: reader (list networks and
  subnetworks, their effective tags, get the project), writer (create subnetworks, bind tags,
  never delete), tag user (bind the operator's tag values), tag value creator (only with
  `gcp.createTagValues`) and impersonator (`iam.serviceAccounts.getAccessToken` only, narrower
  than `roles/iam.serviceAccountTokenCreator`). `test/deploy` checks them as it checks the AWS
  roles. The permission names follow the IAM references; the tag permissions, what
  `views=WITH_UTILIZATION` needs, and whether custom roles may carry
  `resourcemanager.tagValueBindings.create` are for the conformance run (#50).
- **NetworkPolicy.** With GCP on, egress to the GKE metadata server (`169.254.169.254:80` for
  Dataplane V2, `169.254.169.252:988` otherwise) is allowed by default, except with Workload
  Identity Federation, where that address would be another cloud's instance metadata service.
  Google APIs are reached through the general egress rule on 443; `providers.gcp.apiCIDRs` adds
  the Private Google Access ranges where that rule is narrowed.

## Implementation notes (#49: GCP change events, 2.0)

- **Source.** Admin Activity audit logs (always on, free) → an aggregated log sink with a filter
  on networks, subnetworks and tag bindings → a Pub/Sub topic → a pull subscription the operator
  reads with the client library's streaming pull (`cloud.google.com/go/pubsub/v2`), as the
  research note recommended. The setup is documented, not shipped as code of the operator's
  (`deploy/gcp/events.md`, gcloud and Terraform), like the CloudFormation of the AWS path.
  Configuration is `--gcp-events-subscription` (the full name: the subscription usually lives in a
  central project, not one of the scope's) and `--gcp-events-debounce`, in the chart
  `providers.gcp.events.*`, as §8 planned. The default is off.
- **Identity.** The subscriber is the operator's own Google identity (Application Default
  Credentials: GKE Workload Identity, or Workload Identity Federation with or without a service
  account to impersonate), which needs `pubsub.subscriptions.consume` on the subscription:
  `roles/pubsub.subscriber`, or the custom role `deploy/gcp/events-subscriber-role.yaml`. There is
  no per-subscription service account to impersonate: the subscription is one resource in one
  project, and the chart's `wif.serviceAccount` already covers acting as a service account.
- **Mapping.** A Compute entry names its resource by project ID (`resourceName`); a subnetwork
  maps to its project and region, a network to every region of its project. That is a
  `TargetKey` with an empty region, which `NotifyChanged` matches against every region of the
  account a scope covers; `EventSink.Changed` documents it. Methods count as changes unless they
  are known not to be (reads, IAM policy calls), so a method Google adds later triggers a resync
  rather than being missed. Tag bindings (`CreateTagBinding`, `DeleteTagBinding` of
  `cloudresourcemanager.googleapis.com`) name the bound resource by its full name with a project
  number; the provider maps the number to the ID of a project discovery has looked up
  (`Discoverer.projectID`), and falls back to the project the entry was logged in. Where
  Resource Manager logs tag binding changes is for the conformance run (#50). Compute's
  long-running operations log twice, at the start and at the end; both resync, and the
  debounce usually merges them.
- **Attribution.** An `insert` reports a `Creation` with the ID the inventory uses
  (`projects/<p>/regions/<r>/subnetworks/<name>`, `projects/<p>/global/networks/<name>`) and
  `authenticationInfo.principalEmail`, or the first principal of the delegation chain when that
  is empty, as §8 planned for `fromCreator`.
- **Failure behaviour, as on AWS.** Every message is acknowledged once read, including the ones
  that cannot be parsed or mapped; nothing is redelivered that would never read better, and the
  periodic resync covers what is lost. `Receive` returns only what the client library does not
  retry itself (a deleted subscription, a missing permission); the source logs it and reconnects
  with backoff up to a minute, so fixing the subscription needs no restart. Only the leader pulls.
- **Metrics, for both providers.** `hs_change_events_total{provider,result}` (`resync`,
  `ignored`, `malformed`) and `hs_change_event_errors_total{provider}`, per provider rather than
  per scope, since one source serves every scope and an ignored event belongs to none. They exist
  at 0 while a source runs. The SQS poller reports the same through the same `EventSink`
  callbacks. The alert `SubnetInventoryChangeEventsFailing` fires when reading keeps failing for
  a quarter of an hour; before, a deleted queue showed only in the log.
- **Tests.** The parser against entries shaped like what a sink publishes; the subscriber, the
  provider's source and an envtest spec of the scope controller against `pstest`, the in-memory
  Pub/Sub the client library ships (wrapped as `gcpfake.PubSub`), with the real client and its
  streaming pull. There is no e2e job for it: nothing emulates Cloud Logging, and pstest would
  only repeat the envtest spec.

## Implementation notes (GCP network overlaps, 2.0)

The docs pass for 2.0 found that `NetworkCIDROverlap` compared the networks' own CIDR blocks,
which a GCP VPC network does not have, so it could never fire on GCP. The owner decided to
compare subnetwork ranges instead.

- **What is compared.** A network's ranges are its CIDR blocks (AWS, and a GCP legacy network)
  and, on GCP, every range of its subnetworks in the scope: primary, secondary and IPv6. Two
  networks overlap when a range of one overlaps a range of the other. Compute refuses overlaps
  within a network, so ranges of the same network are never paired. AWS is compared exactly as
  in 1.x (IPv4 CIDR blocks only).
- **One metric and alert for every provider, one more for peerings.** `status.overlapsWith`,
  `hs_network_cidr_overlaps` and `NetworkCIDROverlap` keep their names, labels and meaning
  and now cover GCP networks too; AWS series are unchanged. Whether two networks are peered is
  not a label on that metric: a network overlapping one peered and one unpeered network would
  need two series, and every AWS series would gain a label that is always empty. Instead
  `hs_network_peered_cidr_overlaps` (same labels) counts the overlapping networks that are also
  peered, and `NetworkPeeredCIDROverlap` alerts on it after ten minutes, so an overlap that
  already breaks a peering can be routed before the others. It is exported only where peerings
  are read (GCP): a 0 on AWS would claim a check nobody made.
- **Peerings** come with `networks.list` (no further call or permission) and are kept in
  `Network.status.gcp.peerings` (name, peer network, state, state details). Two networks are
  peered when either has a peering with the other, in any state: an `INACTIVE` one is the
  typical broken case, since Compute refuses to activate a peering between overlapping networks.
- **Details** go to `status.gcp.overlaps`: per other network, `peered`, `peeringState`,
  `rangeCount` and the first five pairs of ranges with their subnetworks. Every peered network is
  listed, and the first 50 others: every auto mode network has the same ranges, so a scope over
  many projects' `default` networks makes all of them overlap, and the object has to stay small.
- **Computation.** `scopeOverlaps` sorts all ranges of the scope by address and compares each
  with the ranges that start inside it (CIDR prefixes overlap only by containment), instead of
  every network against every other.

## Implementation notes (#52: Azure discovery, 3.0)

What the first part of 3.0 decided where this ADR and the research note (#41) left room. Writes
(#53), identities per subscription and tenant (#54), change events (#55) and the run against real
subscriptions (#56) come next and are not in it.

- **ARM, not Resource Graph.** The spike recommended Resource Graph for inventory and ARM before
  writes. Discovery reads ARM alone: `virtualNetworks` List All per subscription (subnets come
  inline, with their prefixes, delegations, service endpoints, associations and IP
  configurations), or `virtualNetworks` List per resource group when the scope names resource
  groups (below), and `virtualNetworks/{name}/usages` per reported virtual network, through the
  official SDK (`armnetwork/v12`, api-version 2026-01-01). Reasons: the usages are ARM-only, so
  Resource Graph would add a second API rather than replace one; ARM reads its own writes, and a
  subnet a claim just created (#53) must not vanish from the inventory while Resource Graph
  catches up, which the controllers would take for a deletion; Resource Graph has a quota of its
  own per user (about 15 queries per 5 s); and a target is one subscription and location, which
  one paged List All call covers. Resource Graph stays an option for scopes of hundreds of subscriptions, as a lister in
  front of ARM, never for allocation. ARM has no location filter, so each target lists the
  subscription and keeps its location's virtual networks; a scope over many locations lists a
  subscription once per location, which is what the adaptive pacing below absorbs.
- **Tag keys.** `OperatorTagKeysFor(Azure)` is `hs-owner`, `hs-env`, `hs-tier`, `hs-managed`,
  `hs-managed-by` and `hs-claim` (§5): an Azure tag name cannot contain `<`, `>`, `%`, `&`, `\`,
  `?` or `/`. A value can contain `/`, so the claim value is `<namespace>/<name>` as on AWS, cut
  to the 256 characters of a value like the GCP one is cut to 63. `SubnetEntryPrefix`
  (`hs-subnet-`) names the per-subnet entries. `ValidateTags` refuses the characters above,
  names over 512 and values over 256 characters, two names that differ only in case (Azure
  treats names case-insensitively), and names starting with `hs-subnet-`, which only the
  operator writes.
- **Subnet ownership (§6).** The entry is the tag `hs-subnet-<subnet name>` on the virtual
  network, found whatever the case of either. Its value holds the subnet's tags as `key=value`
  pairs, sorted by key and separated by `;`, with `%`, `;` and `=` percent-encoded:
  `hs-owner=payments;hs-env=prod;hs-tier=db`. It carries the full keys rather than the short
  `owner=…;env=…` the sketch in §6 first had, because `tagKeys`, `requiredSubnetTags` and an
  import's tags are arbitrary keys and must round-trip; it costs a few characters of the 256.
  The owner confirmed this format on 2026-09-29, and §6 shows it now. A subnet
  with an entry reports `ownershipSource: Subnet` and the network's tags with the entry laid
  over them key by key, so an entry only needs what differs from the network (which is what the
  budget rule of §6 writes); a subnet without one reports the network's tags and `Network`. The
  entries are left out of the network's `status.tags` and of what `networkSelector` matches, and
  `status.azure` of the network reports `tagCount` (of Azure's 50) and `subnetOwnershipEntries`,
  so the budget #53 has to respect is visible before it is exhausted.
- **Tag names are case-insensitive (owner decision 2026-09-29).** Azure documents that "tag
  names are case-insensitive for operations" and "tag values are case-sensitive", and a name
  keeps the case it was set in. So on Azure the operator matches every tag name it reads
  without regard to case: the scope's `tagKeys`, `networkSelector.matchTags`,
  `requiredSubnetTags` and auto-import keys, the operator's own keys (`hs-managed-by`,
  `hs-claim`), the `hs-subnet-` entries and the keys inside them, and a claim's or import's
  tags against what the resource carries. The controllers look tags up by name, so the core is
  unchanged: the scope's reader names (`NetworkScope.TagNames`) travel in `Target.TagNames`,
  and the Azure discoverer reports a tag whose name equals one of them in any case under the
  scope's spelling; other names keep Azure's, and an entry's name replaces the network's tag
  of the same name in any case. Values are compared exactly. `networkv1.CaseInsensitiveTagNames`
  says which provider does this, for the webhooks. A scope that names one tag in two spellings
  is refused, as it would be one Azure tag; `ValidateTags` still refuses a claim's or import's
  tags that do. When #53 writes, it uses the operator's keys as they are (lowercase for the
  defaults, `hs-subnet-` for the entries). AWS and GCP compare names exactly, as before.
- **Resource groups.** `NetworkScope.spec.azure.resourceGroups` (a set of at most 100 names in
  ARM's resource group syntax, compared without regard to case like ARM, empty for the whole
  subscription) limits discovery to those groups in every subscription of the scope: one paged
  `virtualNetworks` List call per group instead of List All, so the identity only needs Reader
  on those groups, not on the subscription. It is scope-wide rather than per account:
  `accounts[].azure` does not exist before #54, and a landing-zone layout usually repeats its
  network resource group names across subscriptions; a per-account override can be added to
  `accounts[].azure` later without changing this field. A group that does not exist
  (`ResourceGroupNotFound`) fails the target, like a subscription that does not, rather than
  being taken for an empty one. Usages, status and ownership are unaffected.
- **IDs and names.** `spec.id` is the ARM resource ID in lowercase: ARM IDs are case-insensitive,
  and not every API spells them alike (`resourceGroups` or `resourcegroups`, the group's own
  case), while object names, labels and the IDs of claims and imports compare them as strings.
  A subscription ID in `accounts[].id` and `autoImport.accountDefaults` may be written in either
  case (owner decision 2026-09-29) and is used in lowercase (`networkv1.CanonicalAccountID`) for
  targets, `spec.account`, labels, metrics and `status.targets`; `NetworkScope.Account` and the
  auto-import policy compare it without regard to case, and the webhook refuses two entries
  that differ only in case (v1 declares both lists sets) and a second scope over the same
  subscription in another spelling. It is normalised where it is read, not by the defaulting
  webhook: the controllers must not depend on the webhooks (they refuse the same claims and
  imports themselves), and a spec rewritten on admission would show as drift to GitOps tools
  forever. The defaulting webhook only fills in fields a spec leaves empty (`tagKeys`,
  `managedTag`), never rewrites a value somebody set.
  Objects are named as in §5, `<name>-<10 hex of sha256(id)>`. The resource group, as Azure
  spells it, is in `status.azure.resourceGroup` of both kinds rather than in
  `Network.spec.azure`, as the sketch had it: it is part of the ID already, and the spec stays
  the neutral identity every provider shares. A virtual network is regional, so an Azure target
  and its networks behave like AWS ones (no global networks).
- **IP usage.** `totalIPs` is every IPv4 address prefix of the subnet less the 5 addresses Azure
  reserves in each. `availableIPs` is the usages' `limit − currentValue`, capped at the total:
  whether both count the reserved addresses is unconfirmed (#41), but they are on the same
  footing either way, which subtracting `currentValue` from our own total would not be. `-1`
  (gateway subnets) is unknown. A subnet the usages leave out falls back to its usable addresses
  less its IP configurations, unless it is service-managed (delegated, or with service
  association or resource navigation links: App Service, SQL Managed Instance), whose services
  use addresses that are not IP configurations; then it is unknown. `status.azure.ipUsageSource`
  (`VirtualNetworkUsage`, `IPConfigurations`) and `serviceManaged` say which applies. Further
  IPv4 prefixes are the neutral `secondaryCIDRBlocks` and, unlike GCP secondary ranges, are
  counted: Azure places addresses in any prefix of a subnet and reports one usage for all of
  them. IPv6 prefixes are `ipv6CIDRBlocks`; an IPv6-only subnet has 0 of 0 IPv4 addresses.
  Whether the usages count delegated and IPv6 usage is for #56.
- **Throttling.** HTTP 429 from ARM (subscription or tenant buckets, and Microsoft.Network's
  `RetryableErrorDueToAnotherOperation`, which for a read is as good a reason to wait) is
  throttling; a 403 `AuthorizationFailed` is not. The SDK's own retries are off, since it would
  wait out each `Retry-After` inside one call. Each call (a whole paged listing: an SDK pager
  cannot resume after an error) is tried five times, with a delay per bucket (identity and
  subscription) that doubles from 1 s to a minute, is at least what `Retry-After`
  (`retry-after-ms`, `x-ms-retry-after-ms`) asks for, halves as calls get through, and stays at
  1 s while `x-ms-ratelimit-remaining-subscription-reads` reports fewer than 25 reads left, so
  discovery slows down before ARM throttles it. What is still throttled after that is
  `ErrThrottled`, and the controller's per-target backoff takes over.
- **Credentials.** Every subscription is read with the operator's own identity, the SDK's
  `DefaultAzureCredential` (Microsoft Entra Workload ID on Kubernetes, which the research note
  recommends, or a managed identity or the `AZURE_*` variables), built at the first discovery,
  so the operator starts without Azure credentials and reports their absence per scope.
  `azure.Identity` holds the tenant and client ID #54 adds as `accounts[].azure`
  (`tenantID`, `clientID`, `writeClientID`, §3), and the discoverer refuses one it cannot use
  yet rather than fall back to its own. `WriteIdentityField` already names `azure.writeClientID`.
  (#54 implemented it, below.)
- **What it refuses** until #53: capabilities are `IPUsage` only; `CreateSubnet` and
  `WriteOwnership` return `ErrNotSupported`; the webhooks and controllers refuse every claim and
  import of an Azure scope (`NotSupported`) and an Azure scope with an auto-import policy other
  than `Off`. `ReplacesOwnershipValues` is true for now, as the Tags API's `Merge` replaces a
  name's value; #53 decides whether an ownership write may replace a subnet entry's values or
  must refuse like GCP.
- **API.** `provider: Azure`; `NetworkScope.spec.azure` with `resourceGroups` (the tenant is
  per account, §3); CEL rules for the member and for subscription IDs (a UUID in either case;
  the rules first accepted lowercase only and were relaxed before any release, which
  `allowedNewRules` records as the rules now read), in `allowedNewRules`; `status.azure` on `Network` and
  `Subnet`. v1beta1 gets the same fields. Left for the issues that need them, all additive:
  `accounts[].azure` (#54), a claim's `azure` options (#53).
- **Tests.** There is no network emulator, and the SDK's `fake` package has no state, so
  `internal/cloud/azure/azurefake` serves the three ARM calls (List All, List per resource
  group, usages) over HTTPS (the SDK refuses bearer tokens over plain HTTP), stateful, with
  `nextLink` paging, the remaining-reads header, 429 with `Retry-After`, 403
  `AuthorizationFailed`, 404 `SubscriptionNotFound` and `ResourceGroupNotFound`, and 401 without
  the token its static credential hands out. The provider passes the contract suite against it with the
  real SDK clients; the fixture writes a subnet's tags as its entry on the virtual network, the
  way somebody following the convention would. The ownership-write clause is skipped again for a
  provider whose `WriteOwnership` returns `ErrNotSupported`, provided it refuses imports too
  (the clause #46 had, which #47 removed with GCP's writes). An envtest spec reconciles an Azure
  scope end to end. The chart has `providers.azure.enabled`; values for the identity are #54. The chart does
  not render an Azure `networkScope` yet (it accepts AWS and GCP), so `networkScope.azure`, with
  `resourceGroups`, is left for #57.

## Implementation notes (#53: Azure writes, 3.0)

What the Azure writes decided where §6 and the notes for #52 left room.

- **Creating a subnet.** The Subnets API (`PUT …/virtualNetworks/{vnet}/subnets/{name}`,
  `armnetwork` `BeginCreateOrUpdate`), a long-running operation polled to its end: each poll is
  one call of the pacing (a throttled poll waits and polls again), and between polls the
  provider waits what `Retry-After` asks for. The PUT says `If-None-Match: *`, because a PUT on
  an existing subnet replaces its settings (route table, NSG, delegations): a subnet is read
  first, and one that appears between the read and the PUT is refused, never changed. The
  subnet is regional (no zone) and named `namePrefix`, as on GCP; its prefix comes from the
  virtual network's address space like an AWS subnet's (no pool of its own). ARM refusing the
  prefix (`NetcfgSubnetRangesOverlap`, `NetcfgSubnetRangeOutsideVnet`, from the synchronous
  answer or the operation's end) is `ErrCIDRConflict`, and the claim reserves another range.
  409 `AnotherOperationInProgress` (Microsoft.Network allows one change of a virtual network at
  a time) is retried like throttling, five times with backoff, and then reported; its 429
  `RetryableErrorDueToAnotherOperation` is throttling already. The codes follow the Azure
  documentation and the research note; the conformance run (#56) confirms them.
- **The entry goes first.** A claim writes its subnet's entry before the PUT, so a subnet the
  operator creates never exists without its owner, and a full virtual network (below) creates
  nothing. An entry left by a create that then failed names no subnet and is used again by the
  next attempt, which has the same name; the operator never removes it. A subnet that exists
  with the requested prefix and an entry naming the same claim (`hs-claim`) is the one an
  earlier attempt created, and counts as created; any other taken name is "exists already".
- **The claim tag is always written on Azure**, as on AWS: an Azure tag value needs no
  creating, unlike a GCP one, so there is no `claimTag` switch, and a claim that lost its status
  adopts its subnet by `hs-claim` in the entry (no adoption by name, and no namePrefix
  uniqueness check in the webhook).
- **Ownership writes only add, and refuse like GCP.** Writes go through the Tags API with
  `operation: Merge` on the virtual network's scope, which sends only the names written, so
  nothing reads, modifies and writes back the virtual network object. `Merge` replaces a name's
  value, so every write reads the network's tags first (Tags API `GET`) and refuses the whole
  write with `ErrOwnershipConflict` (`TagValueConflict`) when a name it would write carries
  another value, in any case. `Provider.ReplacesOwnershipValues` is false for Azure, and the
  import webhook refuses such an import at apply time as on GCP. For an existing subnet the
  values compared are its effective tags, the network's with its entry laid over them — what
  discovery reports and the webhook compares against — so an import cannot take over an owner
  the network gives its subnets. A subnet being created has no ownership yet, so only an entry
  left under its name counts there.
- **An entry holds what differs from its network** (§6's budget rule): a pair the network
  carries with the same value is not written, and an import whose tags the subnet already has
  (inherited or in its entry) writes nothing. Names are written in the operator's spelling
  (`hs-subnet-<name>`, lowercase default keys), except that a name the network already carries
  in another case keeps that spelling, since Azure holds one tag for both.
- **The race window.** The read and the `Merge` are two calls, and the Tags API has no
  precondition on one tag, so a value somebody writes to the same name between them is
  replaced. For a subnet that is a concurrent change of the same subnet's entry within one ARM
  round trip; the operator itself writes one resource from one reconcile at a time. Accepted and
  documented rather than worked around: the alternative, a PUT of the whole virtual network with
  its ETag, would need write access to the network and race with every change of it.
- **The tag budget (no breaking change).** A write that would take the virtual network past 50
  tags refuses before anything is written, `ErrTagBudgetExceeded` (`TagBudgetExceeded`); a
  subnet that already has an entry takes more pairs in the same tag. An entry longer than 256
  characters is `ErrOwnershipEntryTooLong` (`OwnershipEntryTooLong`); the webhook and the
  controllers' refusals check a claim's full tag set and a subnet import's tags against it at
  apply time (the entry written may be shorter, never longer). Instead of a condition on the
  `Network`, which §6 sketched, the budget is visible before it runs out: `status.azure.tagCount`
  and `subnetOwnershipEntries` (#52), the gauge `hs_network_tags` (Azure networks only) and the
  alert `NetworkTagBudgetLow` (45 of 50 by default, `prometheusRule.thresholds.networkTags`).
  Nothing about it is breaking for AWS or GCP, and Azure scopes are new in 3.0, so the "per-VNet
  tag budget" the milestone allowed a breaking change for needs none.
- **Claims and imports.** `ValidateClaim` checks the subscription (either case), the location,
  `spec.networkID` as a virtual network's ID in the claim's subscription (in any case, see
  "IDs in any case" below; #53 first refused an ID that was not in lowercase), no zones, a
  prefix of /2 to /29, a `namePrefix` that is an Azure subnet name and not one Azure reserves (`GatewaySubnet`,
  `AzureFirewallSubnet`, `AzureFirewallManagementSubnet`, `AzureBastionSubnet`,
  `RouteServerSubnet`), and no `aws` or `gcp` options. `ValidateImport` checks a virtual network
  or subnet ID the same way. The auto-import policy is allowed on Azure scopes. The audit trail
  records `spec.account` in the canonical (lowercase) spelling.
- **IDs in any case (owner decision 2026-10-05: accept any case).** ARM compares resource IDs
  without regard to case and the portal shows them in camel case
  (`/subscriptions/<GUID>/resourceGroups/My-RG/providers/Microsoft.Network/virtualNetworks/vnet-1`),
  so a claim's `networkID` and an import's `resourceID` are normalised instead of refused. There
  is one canonical spelling, the inventory's: the whole ID in lowercase
  (`networkv1.CanonicalResourceID`, which the Azure provider's `canonicalID` is), next to
  `CanonicalAccountID` for the subscription. It is applied in three places, each for a reason:
  the mutating webhooks rewrite `spec.account` and `spec.networkID`/`spec.resourceID`
  (`CanonicalizeIDs`), so the stored object, `kubectl get` and the import's `resource` and
  `account` labels show what the operator uses; the validating webhooks do the same to their own
  copy, since the mutating ones are `failurePolicy: Ignore`, and take another spelling of the
  same ID on an update for no change of the immutable field; and the claim and import
  controllers do it to the copy they reconcile, for objects written while the webhooks were
  off, without rewriting the stored spec. Other claims' network IDs are compared with
  `SameResourceID` when the free space of a network is worked out. AWS and GCP IDs are
  case-sensitive and stay exactly as written (`vpc-0ABC` is not `vpc-0abc`). A `NetworkScope`'s
  `accounts[].id` is still left as written, as decided on 2026-09-29.
- **Identity.** Every write call uses the target's identity, which the controllers take from
  `Provider.Identity(account, Write)`, as on AWS and GCP; the discoverer builds the write
  clients (`SubnetsClient`, `armresources` `TagsClient`) per identity and subscription and
  refuses an identity it cannot use. Until #54 that is the operator's own identity for every
  subscription; #54 fills `Identity` and `credentialFor` and nothing in the writer changes (#54 did, below).
- **Permissions.** `deploy/azure/writer-role.json` (#54 adds the reader role and the identity
  setup): `Microsoft.Network/virtualNetworks/subnets/read` and `/write`,
  `Microsoft.Network/virtualNetworks/read`, `Microsoft.Network/locations/operations/read` for
  polling, `Microsoft.Resources/tags/read` and `/write`; no delete, no
  `Microsoft.Network/virtualNetworks/write`, no role assignments (`test/deploy` checks it).
  Imports alone need the Tags API permissions only, which the built-in Tag Contributor grants.
- **Tests.** `azurefake` serves the subnet GET and PUT (a long-running operation with
  `Azure-AsyncOperation`, `If-None-Match`, the prefix checks, operations that fail on demand),
  the Tags API `GET` and `PATCH` (Merge only: a `Replace` or `Delete` is refused, so a test fails
  if the provider ever sends one; names case-insensitive; 50 tags), 409
  `AnotherOperationInProgress`, and 403 on writes alone. The contract suite's create and
  ownership clauses now run for Azure; its identity clauses wait for #54's fixture. Envtest
  specs create a subnet from a claim with its entry, adopt it after a lost status, refuse a
  taken name, import into entries and refuse replacing values (inherited ones too), refuse on a
  full network, and run the auto-import policy in `Apply` mode.

## Implementation notes (#54: Azure credentials, 3.0)

- **API.** `accounts[].azure` is `AzureAccount{clientID, writeClientID, tenantID}`, each an
  optional UUID in either case (schema pattern, webhook check), in v1 and v1beta1, so the
  conversion stays exact. A CEL rule allows the member only with provider Azure (in
  `allowedNewRules`), another refuses `tenantID` without a client ID. As with subscription IDs
  (#52), the spec is not rewritten: `Provider.Identity` lowercases the IDs, so one identity is one
  credential and one ARM quota bucket however scopes spell it. Reading and writing as the same
  identity is accepted with a warning, as on GCP. `tenantID` is a UUID only, not a domain name:
  it is a cache key, and a domain would name the same tenant twice.
- **No AssumeRole on Azure.** The research note (#41, Q4) found no delegation API: one
  credential reaches every subscription it has role assignments in, and another tenant needs a
  multitenant app registration provisioned there. So the default is one operator identity with
  RBAC in many subscriptions, needing no `azure` member at all. An account's identity is a
  user-assigned managed identity or app registration with a federated identity credential for
  the operator's service account, and the operator exchanges the same projected token for a
  token of that client ID (`azidentity.WorkloadIdentityCredential` with the account's client ID,
  its tenant or the operator's, and `AZURE_FEDERATED_TOKEN_FILE`). That gives what `roleARN` /
  `writeRoleARN` give on AWS: reads and writes apart, least privilege per subscription or landing
  zone, other tenants, and an operator identity that holds no role. The cost is one federated
  credential per identity and cluster (an identity holds at most 20).
- **Credentials** are built at their first use and kept per resolved (tenant, client ID); the
  SDK caches each token until shortly before it expires and rereads the token file as the
  kubelet rotates it. The operator's own identity stays `DefaultAzureCredential`. A token that
  cannot be had fails the target with a `CredentialError` naming the client ID, the issuer and
  subject the federated credential must name (read from the token, unverified), the audience and
  Entra's `AADSTS` line; without a token file or tenant a `NoTokenFileError` says which is
  missing. Neither is throttling, and nothing falls back to the operator's own identity. A 429
  from Entra is `ErrThrottled`. ARM refusing an account's identity (`AuthorizationFailed`) is
  wrapped with the client ID, since ARM names the principal by its object ID.
- **Clouds.** `--azure-cloud` (`AzurePublic`, `AzureUSGovernment`, `AzureChina`; the chart's
  `providers.azure.cloud`) selects the Resource Manager endpoint, audience and authority host for
  ARM clients and credentials alike; `--azure-authority-host` replaces the authority host. They
  are flags rather than `AZURE_AUTHORITY_HOST`, because the ARM endpoint has to move with it.
- **Chart.** `providers.azure.workloadIdentity`: with the AKS webhook (default) the pod label
  `azure.workload.identity/use: "true"` and the service account annotations
  `azure.workload.identity/client-id` and `tenant-id`; with `webhook: false` (EKS, GKE,
  self-managed) the chart projects the token (audience `api://AzureADTokenExchange`) and sets
  `AZURE_FEDERATED_TOKEN_FILE`, `AZURE_TENANT_ID` and `AZURE_CLIENT_ID` itself, as the GCP
  `wif` does. The operator's own client ID may be left out when every account names identities.
  `providers.azure.apiCIDRs` adds a 443 rule for Entra ID and ARM where `egress.cidrs` is
  narrowed; no metadata-server rule, since Workload ID needs none.
- **Least privilege.** `deploy/azure` has custom role definitions (JSON for `az role definition
  create`, which Terraform's `azurerm_role_definition` can read too): reader
  (`virtualNetworks/read`, `virtualNetworks/usages/read`, `subnets/read`) next to #53's writer,
  and the federated credential setup with the az CLI and Terraform. `test/deploy` checks the
  files like the AWS and GCP roles, and runs discovery and the writes against azurefake to check
  that the reader, and the writer, hold the action of every call each makes; a new call fails
  that test until it is mapped.
- **Tests.** azurefake gained Microsoft Entra ID on a host of its own: the OpenID configuration
  and the token endpoint MSAL calls, identities per tenant that are federated or not
  (`AADSTS70021`, `AADSTS700016`), tokens that name their tenant and principal, subscriptions in
  a tenant (`InvalidAuthenticationTokenTenant` for another's token), and per-principal
  authorization (`RestrictSubscription`, `RestrictSubscriptionWrites`), so the SDK's real
  `WorkloadIdentityCredential` runs against it. The Azure fixture implements the contract's
  `Identities`: its subscription answers only the read and write identities and takes writes
  from the write identity alone (subnet PUTs and Tags API PATCHes included), so a discovery that
  did not go through the read identity, or a write that did not go through the write identity,
  fails its clause.

## Implementation notes (#55: Azure change events, 3.0)

- **Source.** Each subscription's Event Grid system topic (`Microsoft.Resources.Subscriptions`)
  → an event subscription for `ResourceWriteSuccess` and `ResourceDeleteSuccess` whose subject
  contains `/providers/Microsoft.Network/virtualNetworks/` → one Storage queue the operator
  polls with the SDK's queue client (`azqueue`). The setup is documented, not shipped as code of
  the operator's (`deploy/azure/events.md`, Bicep in `deploy/azure/events/`), like the other two.
  Configuration is `--azure-events-queue-url` and `--azure-events-debounce`, in the chart
  `providers.azure.events.*`, as §8 planned, and `SUBNET_OPERATOR_AZURE_EVENTS_*` under OLM. The
  default is off.
- **A Storage queue, not a Service Bus queue**, and not both. Both are read with the operator's
  Workload Identity through a data role, without keys. A Storage queue is REST on 443 with a
  small SDK module, which the in-repo fake can serve to the real client; Service Bus is AMQP, a
  second protocol stack in the operator and through NetworkPolicies and proxies, and nothing
  in-repo could fake it. What Service Bus adds (long polling, dead-lettering, ordering, messages
  over 64 KiB) is worth little where an event is a hint the periodic resync backs up: polling
  every 5 s while idle disappears in the 10 s debounce, and the poller bounds redelivery itself.
- **Identity.** The poller is the operator's own identity, as on GCP, also when every
  subscription is read as identities of its own: the queue is one resource of the operator's,
  not of a scope's subscription. It needs the built-in Storage Queue Data Message Processor role
  on the queue. A queue URL with a query is refused by the operator and the chart, so a SAS
  token cannot become the way in.
- **Mapping.** A resource event names the resource (`subject`, repeated in `data.resourceUri`)
  and no location, and the target is subscription and location. The provider keeps what every
  discovery already lists, the subscription's virtual networks with their locations, and looks
  the event's network up there: known, it resyncs that location; unknown (created since, or
  outside the scope's resource groups), every location of the subscription, the `TargetKey`
  with an empty region #49 introduced; a subscription no discovery was asked for is ignored.
  Asking ARM per event was rejected: it spends the read quota discovery is paced by, and cannot
  answer for a deleted network. The memory is the leader's and starts empty. IDs are matched
  without regard to case and lowercased, the spelling the inventory uses. Anything below a
  virtual network counts as a change of it, which covers subnets, peerings and the tag writes
  that carry subnet ownership (§6); action events are left out, since every NIC that joins a
  subnet raises one.
- **No attribution.** A write event does not say whether it created or updated its resource, so
  the source reports no `Creation`; `fromCreator` rules never match on Azure. Whether
  `data.httpRequest` or `data.status` tells the two apart reliably is for #56.
- **Failure behaviour, stricter than the other two.** A message that reports a change is deleted
  only after `EventSink.Changed` took its target; until then it stays in the queue, hidden for a
  visibility timeout of two debounce intervals and 30 s, so an operator that stops, or a sink that
  fails, loses nothing. Messages that report nothing or cannot be read (not JSON or Base64 of it,
  no event, over 64 KiB, a topic or `data.subscriptionId` of another subscription than the
  resource's) are deleted at once. A Storage queue has no dead-letter queue, so the poller drops a
  message delivered more than 5 times, counted as `malformed`. A failed receive is retried with
  backoff up to a minute; it and a failed delete count in `hs_change_event_errors_total`, so an
  identity that may read but not delete shows in `SubnetInventoryChangeEventsFailing`.
- **Schema.** Both delivery schemas are read (Event Grid's and CloudEvents 1.0), a single event
  or an array, as JSON or Base64. The fields are from Microsoft's schema reference; the subjects
  of subnet and tag writes and the encoding in the queue are for #56 to confirm.
- **Tests.** azurefake serves Azure Queue Storage (Get Messages with visibility timeouts,
  dequeue counts and pop receipts, Delete Message, the service's XML errors, per-principal
  authorization) to the real `azqueue` client; the parser, the poller, the provider's source and
  an envtest spec of the scope controller run against it. There is no e2e job: nothing emulates
  Event Grid.

## Implementation notes (#116, #117: Azure imports read their virtual network first, 3.0)

- **Why a read.** §6 keeps a subnet's ownership on its virtual network, and the Tags API writes
  `hs-subnet-<name>` there whether or not the network has a subnet of that name: an import of a
  misspelt subnet ended `Applied` and left an entry that names nothing and counts against the 50
  tags (#116). And an ARM resource ID names no location, while `spec.region` was optional and
  unchecked on Azure (#117). One call answers both: `virtualNetworks` Get returns the location
  and the subnets inline.
- **A provider seam, not an Azure branch.** `provider.ImportLocator` (`ImportNetwork`,
  `LocateImport`) is optional and only Azure implements it; the import controller and webhooks
  ask for it by type assertion. AWS and GCP are untouched: their tag writes name the resource
  itself, so the cloud refuses one that does not exist (`TagsNotApplied`, with the cloud's
  not-found error), AWS requires the region and GCP reads it from the subnetwork's ID.
- **As the read identity.** The read is made with `Provider.Identity(account, Read)`: the reader
  role already holds `Microsoft.Network/virtualNetworks/read`, Tag Contributor (all an import's
  write identity needs) does not, and a dry run has to report the same without a write identity.
  It uses discovery's quota bucket and pacing.
- **When.** After the checks that cost no call (`WritesDisabled`, `NoWriteRole`) and before the
  write; in a dry run before its result; never for an import that is `Applied`. One read per
  attempt, so a failing import costs one a minute.
- **Outcomes.** `inventory.ErrResourceNotFound` is reason `ResourceNotFound`, state `Failed`,
  retried, nothing written (a subnet created later is imported); in a dry run the same with
  `dry run:` before the message. A `spec.region` that is not the network's location is
  `RegionMismatch`. Any other failure of the read is `TagsNotApplied`, as for the write.
- **The region.** Required by `ValidateImport`, as on AWS, after the mutating webhook filled it
  in from the `Network` object of the ID's virtual network; the validating webhook refuses one
  that contradicts that object, and warns specifically about a subnet a discovered network does
  not have. The controller does not require it (`ImportRefusal`): for an import written while
  the webhooks were off it takes the location it read, on its own copy, so the target, the audit
  line and the resync carry it and the stored spec stays as written. `spec.region` stays
  immutable, except that an empty one may be filled in on such a provider.
- **What stays open.** The subnet can be deleted between the read and the `Merge`, one ARM round
  trip later; the Tags API has no precondition to close that with. A claim in `Create` mode still
  writes its entry before the subnet exists, on purpose (above).
