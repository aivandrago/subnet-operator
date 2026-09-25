# ADR 0002: The multi-cloud model

Status: accepted (2026-09-24)

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

What the spikes found that constrains the shape:

| Question | AWS (today) | GCP ([#40](../research/gcp-network-model.md)) | Azure ([#41](../research/azure-network-model.md)) |
|---|---|---|---|
| Metadata on the network | tags | no labels; Resource Manager tags | tags |
| Metadata on the subnet | tags | no labels; Resource Manager tags, values must be pre-created, not returned by `subnetworks.get` | **none**: subnets cannot carry tags |
| Key rules | `hs/owner` valid | keys are pre-created `TagKey`s; 1,000 values per key; 50 tags per resource | no `/` in names; 50 tags per resource; value ≤ 256 chars |
| Free IPs per subnet | `AvailableIpAddressCount` | `views=WITH_UTILIZATION` → `utilizationDetails` per range | `virtualNetworks/{vnet}/usages`; `-1` means unknown |
| Zones | subnet is zonal | network global, subnet regional | VNet regional, subnet has no zone |
| Account | account | project (Shared VPC: host project) | subscription (+ resource group in the ID) |
| Change events | EventBridge → SQS | audit logs → sink → Pub/Sub | Event Grid → Storage/Service Bus queue |
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
in the 1.x releases that implement them; both are additive. The sketches below exist to prove the
neutral fields fit them, not to ship unused schema.

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
  subscription, and the resource group is part of each network's identity
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
| `ParentNetworkTags` | Azure subnets | on the parent VNet, one tag per subnet: `hs-subnet-<subnet name>` = `owner=payments;env=prod;tier=db;managed=true` |

- **Azure subnets** use the parent VNet. Subnet names (1–80 characters of alphanumerics, `_`,
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
- Deprecation window (#44): the release that introduces the new group exports both families; the
  old family keeps its old names **and** labels unchanged, so existing alerts keep working. It is
  removed in the next minor release. Alerts, recording rules, the Grafana dashboard, the promtool
  tests and the web dashboard switch to `hs_*` in the release that introduces them. Series
  cardinality doubles during the window; the limits page documents it.

### 8. What stays provider-specific, and how the API says so

- In the schema: by being inside a provider member (`aws.roleARN`, `aws.writeRoleARN`,
  `aws.externalID`, `SubnetClaim.spec.aws.routeTableID`, `aws.mapPublicIPOnLaunch`,
  `Subnet.status.aws.*`). There is no "AWS only" prose on neutral fields, because there are no
  AWS-only neutral fields.
- Change events are **not** API: they are operator configuration (today `--events-queue-url`).
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
old ones after that release.

### 10. Versioning to v1 (#58)

| Release | Served | Storage | Notes |
|---|---|---|---|
| 0.8 | `aws.hypersurgery/v1alpha1` (deprecated), `network.hypersurgery.dev/v1beta1` | v1beta1 | migration controller; both metric families |
| 0.9 | `network.hypersurgery.dev/v1beta1` | v1beta1 | old group and `hs_aws_*` removed |
| 1.0 | `v1`, `v1beta1` (deprecated) | v1 | conversion webhook; storage migrated to v1 |
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
	// GCP   *GCPScope   `json:"gcp,omitempty"`   // 1.x
	// Azure *AzureScope `json:"azure,omitempty"` // 1.x
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

// 1.x, shown to check the shape:
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
and `namespaceSelector` on the old API first.) Whether the
GCP write identity may create tag values, and the Azure VNet tag budget, are decided in phases 5b
and 5c; neither affects the 1.0 API.

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
  always read ARM first (Resource Graph is eventually consistent).
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
- Adding a provider after 1.0 is additive: one enum value, one member per kind that needs one,
  one provider registration. Anything a new provider needs beyond that is a sign this ADR was
  wrong, and a new ADR.

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
  GCP and Azure member. Only what would be breaking to change later ships in v1beta1.
- **Per-provider validation.** `prefixLength` is 1–32 in the schema, 16–28 for AWS in the
  webhook and the controller; account ID formats are CEL rules on the scope, and on claims and
  imports webhook and controller checks, because those do not know their provider. `managedTag`
  lost its static CRD default for the same reason; the webhook and the policy default it per
  provider.
- **Metrics** keep their `hs_aws_*` names in this change; §7 is #44.
- **The leader election lease** keeps its name across the rename (#76), so a 0.7 and a 0.8 pod
  never lead at once during the rolling upgrade.
