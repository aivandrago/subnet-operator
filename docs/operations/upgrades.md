# Upgrades and rollback

## From which version?

Every upgrade uses the same two steps, [the new CRDs first, then the release](#upgrading).
Some releases have to be passed through rather than skipped, because they are the ones that move
objects: 0.8 moves every `aws.hypersurgery/v1alpha1` object to `network.hypersurgery.dev`, and
nothing after it can; 1.0 expects what 0.9 stored, and moves it to `v1`. Find the release you run and follow its row
from left to right, finishing each hop (the rollout completes, the checks in
[Verifying an upgrade](#verifying-an-upgrade) pass) before starting the next:

| You run | Hops to 3.0 | Read, in this order |
|---|---|---|
| 0.1, 0.2 | 0.7.1 → 0.8 → 0.9 → 1.0 → 2.0 → 3.0 | [0.2 to 0.3](#upgrading-02x--03x-one-behaviour-change-to-plan-for), [0.3 to 0.7](#upgrading-from-03-to-07), [0.6 to 0.7](#upgrading-from-06-to-07-namespace-restricted-scopes-and-created_by), [0.7 to 0.8](#upgrading-from-07-to-08), [0.8 to 0.9](#upgrading-from-08-to-09), [0.9 to 1.0](#upgrading-from-09-to-10), [1.x to 2.0](#upgrading-from-1x-to-20), [2.x to 3.0](#upgrading-from-2x-to-30) |
| 0.3 to 0.6 | 0.7.1 → 0.8 → 0.9 → 1.0 → 2.0 → 3.0 | [0.3 to 0.7](#upgrading-from-03-to-07), [0.6 to 0.7](#upgrading-from-06-to-07-namespace-restricted-scopes-and-created_by), then as above from 0.7 to 0.8 |
| 0.7 | 0.8 → 0.9 → 1.0 → 2.0 → 3.0 | [0.7 to 0.8](#upgrading-from-07-to-08), [0.8 to 0.9](#upgrading-from-08-to-09), [0.9 to 1.0](#upgrading-from-09-to-10), [1.x to 2.0](#upgrading-from-1x-to-20), [2.x to 3.0](#upgrading-from-2x-to-30) |
| 0.8 | 0.9 → 1.0 → 2.0 → 3.0 | [0.8 to 0.9](#upgrading-from-08-to-09), [0.9 to 1.0](#upgrading-from-09-to-10), [1.x to 2.0](#upgrading-from-1x-to-20), [2.x to 3.0](#upgrading-from-2x-to-30) |
| 0.9 | 1.0 → 2.0 → 3.0 | [0.9 to 1.0](#upgrading-from-09-to-10), [1.x to 2.0](#upgrading-from-1x-to-20), [2.x to 3.0](#upgrading-from-2x-to-30) |
| 1.x | 2.0 → 3.0 | [1.x to 2.0](#upgrading-from-1x-to-20), [2.x to 3.0](#upgrading-from-2x-to-30) |
| 2.x | 3.0 | [2.x to 3.0](#upgrading-from-2x-to-30) |

Up to 0.7 the chart is `aws-subnet-operator`, and the hop to 0.7.1 is made with that chart
(`hypersurgery/aws-subnet-operator --version 0.7.1`, or
`oci://ghcr.io/aivandrago/charts/aws-subnet-operator`); from 0.8 on it is `subnet-operator`.
The CRDs of 0.1 to 0.7 only ever gained fields, so one hop from any of them to 0.7.1 is enough.
CI tests the upgrade from the latest release on every change, and from 0.9 on every push to
`master` ([policy](../policy.md#the-upgrade-test)); the longer paths are the same steps, one
after another.

## What each release added

Taken from the tags in this repository (`git show <tag>:charts/aws-subnet-operator/Chart.yaml`,
`git ls-tree -r <tag> -- charts/aws-subnet-operator/crds`; from 0.8 on, `charts/subnet-operator`)
and the release notes.

| App version | Chart | CRDs after the upgrade | What is new that an operator notices |
|---|---|---|---|
| v0.1.0 | 0.1.0 (0.1.1 adds the registry default) | `networkscopes`, `vpcs`, `subnets`, `sheetexports` | Read-only discovery, metrics, Grafana dashboard, five alerts, Google Sheet export |
| v0.2.0 | 0.2.0 | **+ `subnetclaims`** | `SubnetClaim`; `accounts[].writeRoleARN` on `NetworkScope`; `writes.enabled` → `--enable-writes`; RBAC for claims |
| v0.3.0 | 0.3.0 | **+ `resourceimports`** | `ResourceImport`; `spec.discoverUnmanaged` (**default `true`**) and `spec.autoImport` on `NetworkScope`; `UnmanagedNetworkResource` and `AutoImportedResources` alerts; unmanaged and auto-import metrics; RBAC for imports |
| v0.3.0 | 0.3.1 | unchanged | Dashboard panels for unmanaged resources and policy decisions — chart only, same image |
| v0.4.0 | 0.4.0 | unchanged | Admission webhooks (the chart serves them, with or without cert-manager), Kubernetes Events and the JSON audit trail, credential Secrets read only in their own namespace, two replicas with a disruption budget |
| v0.4.1 | 0.4.1 | unchanged | The image is public and signed, at `ghcr.io/aivandrago/subnet-operator` |
| v0.5.0 | 0.5.0 | `networkscopes` gains `status.targets[].unmanagedIDs` | Known unmanaged resources survive a restart; `SubnetOperatorDown`, `SubnetClaimNotReady` and `ResourceImportNotSettled` alerts with their metrics; no IPv4 capacity reported for IPv6-only subnets |
| v0.6.0 | 0.6.0 | unchanged | Backoff for throttled accounts, the throttling metrics and `SubnetInventoryTargetThrottled`; the chart published to ghcr.io and signed; the dashboard app in the chart (`dashboard.enabled`) |
| v0.7.0 | 0.7.0 (0.7.1: the same code, the last `aws-subnet-operator` chart) | `networkscopes` gains `spec.namespaceSelector` | Namespace-restricted scopes, the authenticated creator on claims and imports — [its own section](#upgrading-from-06-to-07-namespace-restricted-scopes-and-created_by); memory limit 512Mi |
| v0.8.0 | **`subnet-operator`** 0.8.0 | **+ `network.hypersurgery.dev`**: `networkscopes`, `networks`, `subnets`, `subnetclaims`, `resourceimports`, `sheetexports`; the `aws.hypersurgery` CRDs stay, deprecated | The cloud-neutral API, the migration of every old object, the rename of the project and the chart — [its own section](#upgrading-from-07-to-08) |
| v0.9.0 | `subnet-operator` 0.9.0 | `network.hypersurgery.dev` only. The chart no longer ships the `aws.hypersurgery` CRDs; the ones 0.8 installed stay in the cluster until you delete them | The old group removed, a guard against objects 0.8 never migrated, metrics renamed to `hs_*`, providers — [its own section](#upgrading-from-08-to-09) |
| v1.0.0 | `subnet-operator` 1.0.0 | The same six CRDs, each with **`v1`** (stored) next to `v1beta1` (deprecated, served) | The API graduated to v1 with a [compatibility promise](../api-compatibility.md), stored objects rewritten at v1 by the operator, the conversion webhook (`webhook.conversion.enabled`, `--crd-conversion`), duplicate list entries refused in a `NetworkScope`, the operator's RBAC on its own CRDs, the `hs_crd_stored_versions` and `hs_storage_migration_rewritten_objects_total` metrics, an [OLM bundle](../olm.md); the values and flags 0.9 deprecated are removed — [its own section](#upgrading-from-09-to-10) |
| v2.0.0 | `subnet-operator` 2.0.0 | The same six CRDs; `v1` and `v1beta1` gain the GCP fields (`spec.gcp` on `NetworkScope` and `SubnetClaim`, `status.gcp` on `Network` and `Subnet`, `Subnet.status.secondaryCIDRBlocks`) | The Google Cloud provider (`providers.gcp.*`, `--providers=aws,gcp`): discovery, subnetworks created by claims and tags bound by imports, Workload Identity and impersonation, change events through Pub/Sub; `hs_change_events_total`, `hs_change_event_errors_total`, `hs_network_peered_cidr_overlaps` and the alerts `SubnetInventoryChangeEventsFailing` and `NetworkPeeredCIDROverlap`; `networkScope.provider` and `networkScope.gcp` in the chart; **the operator's tag keys depend on the provider** (`hs/*` on AWS as before, `hs-*` on GCP) — [its own section](#upgrading-from-1x-to-20) |
| v2.0.1 | `subnet-operator` 2.0.1 | unchanged | `NetworkPeeredCIDROverlap` is `critical` (was `warning`): an overlap with a peered network already breaks the peering, so it pages — check your Alertmanager routes. The SubnetClaim webhook refuses a GCP claim that leads to the same subnetwork ID (project, region, `namePrefix`) as another claim of its scope, naming it (#102); a clash between existing claims is only warned about on update. The manager reads `--providers`, `--enable-writes`, `--aws-events-queue-url`, `--aws-events-debounce`, `--gcp-events-subscription` and `--gcp-events-debounce` also from `SUBNET_OPERATOR_*` environment variables (a flag wins), so an OLM Subscription can enable writes, change events and GCP through `spec.config.env` (#89, [docs/olm.md](../olm.md#settings-through-the-subscription)). Without cert-manager, **the chart-signed webhook certificate is renewed by this upgrade**: it now lives `webhook.certificate.duration` (default one year, was 10 years) and is renewed by the first upgrade within `renewBefore` of its expiry, and a certificate with no record of when it was signed — every one from 2.0.0 and earlier — is renewed on the first upgrade. Nothing to do: `ca.crt` carries the previous CA next to the new one for the pods still serving the old certificate, and the operator moves the CRDs' conversion to the new bundle within a minute. Upgrade at least once a year from now on, or use cert-manager. New: `webhook.certificate.existingSecret` and `caBundle` for a certificate you manage ([chart README](../../charts/subnet-operator/README.md#admission-webhooks)) |
| v3.0.0 | `subnet-operator` 3.0.0 | The same six CRDs; `v1` and `v1beta1` gain the Azure fields (`provider: Azure`, `spec.azure` and `accounts[].azure` on `NetworkScope`, `status.azure` on `Network` and `Subnet`) | The Azure provider (`providers.azure.enabled`, `--providers=azure`): discovery, claims and imports, identities per subscription, change events through Event Grid and a Storage queue (`providers.azure.events.*`), and `--azure-cloud`, `--azure-authority-host`, `--azure-events-queue-url` and `--azure-events-debounce` also read from `SUBNET_OPERATOR_AZURE_*` under OLM; `hs_network_tags` and the alert `NetworkTagBudgetLow` (`prometheusRule.thresholds.networkTags`); Azure IDs in a claim's `networkID` and an import's `resourceID` are accepted in any case and stored in lowercase; `networkScope.provider: Azure` and `networkScope.azure.resourceGroups` in the chart; the dashboard app and the Grafana dashboard show Azure, with a panel for the tag budget; a webhook warning for `autoImport.fromCreator` rules in an Azure scope, which never match (no creator attribution on Azure). **No breaking change**, and nothing to do for AWS and GCP scopes — [its own section](#upgrading-from-2x-to-30) |
| v3.0.1 | `subnet-operator` 3.0.1 | unchanged | The OpenTelemetry modules the Kubernetes and Google clients bring in are at 1.45.0, which fixes GO-2026-6505 (CVE-2026-81870): an OTLP trace exporter could write its endpoint URL to the info log. The operator configures no exporter of its own, so this only mattered where `OTEL_EXPORTER_OTLP_*` was set in its environment with a credential in the URL |
| v3.1.0 | `subnet-operator` 3.1.0 | unchanged | With cert-manager and the self-signed Issuer the chart or the kustomize install creates, **the webhook certificates are re-issued by this upgrade**: their Certificates now ask for a subject (`commonName`), which ends cert-manager's `BadConfig ... contravenes RFC 5280` warning on every request (#124). Nothing to do. A chart Certificate with `webhook.certificate.issuerRef` is unchanged and not re-issued, and so are the chart-signed certificate and one of your own. From 3.1 on, a webhook certificate renewed under a new CA is not noticed by clients: the operator watches the Secret its certificate is mounted from (`--conversion-ca-secret`, which the chart sets; the Role it already has for Secrets in its namespace covers it), puts a new CA into the CRDs' conversion before any pod serves under it, and keeps the previous one for an hour (#123, [failure modes](failure-modes.md#cert-manager-issues-a-new-webhook-certificate)). The renewal this upgrade itself causes is the last one of the old kind: until a 3.1 replica holds the lease, the CRDs' CA is still kept by 3.0, which replaces it. A request at **v1beta1** can therefore fail while the upgrade rolls out and for up to a minute and a half after, as with every renewal before 3.1 (measured once in Kind: 11 seconds in which the last 3.0 pod served a certificate the CRDs no longer trusted, and no failed request); requests at v1 and admission are not affected. The `caBundle` of the CRDs' conversion may now hold previous CAs, four at most, next to the current one, and anything put there by hand is still removed within a minute |
| v3.2.0 | `subnet-operator` 3.2.0 | `networks` gains `status.azure.orphanedSubnetOwnershipEntries` (optional, `v1` and `v1beta1`) | On Azure, a token refused for the operator's own identity is reported like one refused for a subscription's identity, naming the client and tenant ID of its environment and the issuer and subject its federated credential must name (#120; the text of `status.targets[].error` changes, nothing matches on it), and the ownership entries whose subnet no longer exists are counted per virtual network (`status.azure.orphanedSubnetOwnershipEntries`, the metric `hs_network_orphaned_ownership_entries`) and listed by `manager azure-orphaned-entries`, which only reads and prints the `az` commands that remove them for you to run: the operator still never removes a tag (#121, [the Azure guide](../azure.md#orphaned-ownership-entries)). Also on Azure, and nothing changes for AWS and GCP scopes: **With the custom reader role of `deploy/azure`, update the role definition** (`az role definition update` with the 3.2 `reader-role.json`): it gained `Microsoft.Resources/subscriptions/locations/read`. Until you do, nothing fails: discovery works as in 3.1 and each Azure scope with an empty location gets a `TargetWarning` Event saying that location names are not checked ([runbook](runbook.md#targetwarning-on-azure-location-names-are-not-checked)); the built-in Reader role already has the action. **A location of an Azure scope that does not exist now fails its target** (#119): `westeuropa` used to sync cleanly with no networks and is now `SyncFailed` with `location "westeuropa" does not exist in subscription …: did you mean westeurope?`, a `TargetUnreachable` Event and `SubnetInventoryTargetDown`, so a scope with a misspelt location that was `Ready` in 3.1 is not `Ready` in 3.2 until the name is corrected; a location that exists and is empty stays as quiet as before, and a location with a virtual network in it is never checked. A subscription's virtual networks are listed **once per sync, not once per location** (#118): fewer Resource Manager reads for a subscription in several locations ([limits](limits.md#azure-resource-manager-per-subscription-per-sync)), and when that one listing is throttled or refused every location of the subscription reports it together. New Event reason `TargetWarning` on `NetworkScope` ([audit](../audit.md)); `hs_api_throttled_total` gains the `operation` value `subscriptions.listLocations` |

Up to 0.7 all kinds are `aws.hypersurgery/v1alpha1`; 0.8 serves both groups, 0.9 only
`network.hypersurgery.dev/v1beta1`. Up to 0.9 there is only ever one API version per group and no
conversion webhook (the admission webhooks that arrived in 0.4.0 validate and default, they do not
convert). 1.0 serves `v1` and `v1beta1` of the same group, stores `v1`, and converts between
them with a webhook. Every field added from 0.2.0 to 0.7.0 is optional. The CRD diffs between
those tags are additions only:

```sh
git diff v0.1.0 v0.2.0 --stat -- config/crd/bases   # + subnetclaims, + writeRoleARN
git diff v0.2.0 v0.3.0 --stat -- config/crd/bases   # + resourceimports, + autoImport/discoverUnmanaged
git diff v0.3.0 v0.7.1 --stat -- config/crd/bases   # + unmanagedIDs, + namespaceSelector
```

## Upgrading

**Helm never upgrades CRDs.** They live in the chart's `crds/` directory, which Helm installs
once and then leaves alone (it also never removes them on uninstall). Apply them yourself
*before* `helm upgrade`, or the new operator will crash-loop looking for a kind the API server
does not know:

```sh
helm repo update hypersurgery
helm pull hypersurgery/subnet-operator --untar
kubectl apply -f subnet-operator/crds/

helm upgrade --install subnet-operator hypersurgery/subnet-operator \
  -n subnet-operator-system --create-namespace \
  -f my-values.yaml
```

(Before 0.8 the chart was `aws-subnet-operator` and the namespace in the examples
`aws-subnet-operator-system`; an existing release keeps its namespace, see
[below](#upgrading-from-07-to-08).)

`kubectl apply` on the CRDs is safe at any time: they are additive, and applying a newer CRD
never touches the objects already stored.

Contributors upgrading from a checkout: `make helm-crds` refreshes the chart's copies from
`config/crd/bases`, and CI fails if they drift.

## What happens to existing objects

- **`Network` and `Subnet` objects (`VPC` and `Subnet` up to 0.7) are outputs, not state.** They are rebuilt from AWS on the
  first sync after the restart and owned by their `NetworkScope`. Losing them costs one
  resync, nothing else.
- **`NetworkScope` spec and status survive untouched.** New optional fields simply appear with
  their defaults on the next write.
- **`SubnetClaim` status survives**, including `status.allocations`. That matters: the
  allocations are the record of which CIDRs are reserved. If they were lost, the operator
  re-adopts subnets it created by their `hs/claim=<namespace>/<name>` tag (on GCP with
  `claimTag: Skip`, by the subnetwork's name), so a claim in `Create` mode heals rather than
  creating a second set of subnets.
- **`ResourceImport` objects survive**, and one that already reached `Applied` is not
  re-applied: the controller returns early when the state and tags match, so an upgrade does
  not produce a wave of `CreateTags` calls in anybody's CloudTrail.
- **Nothing in AWS changes because of an upgrade.** No cloud resource is created, modified or
  deleted by the upgrade itself.

**In-memory state is lost on every restart**, which is what an upgrade is:

| Lost | Consequence |
|---|---|
| `seenUnmanaged` (which unmanaged resources were already counted) | Since 0.5.0, rebuilt from `status.targets[].unmanagedIDs` on the first sync, so only resources that appeared meanwhile count as new. Before 0.5.0, `UnmanagedNetworkResource` fired again for every one you already knew about — see the [runbook](runbook.md#unmanagednetworkresource) |
| `CreatorCache` (who created what, from CloudTrail) | auto-import falls back to VPC inheritance, then account defaults, then `no_owner`; it never guesses |
| targets a change event marked changed and not yet synced (the poller's debounce buffer, the controller's `pending` set) | the first sync after start is a full sync anyway, so nothing is missed |
| cached `AssumeRole` credentials | one extra `sts:AssumeRole` per account |

Events already delivered to SQS are **not** lost: they stay in the queue until the new leader
consumes them (messages are deleted only after being parsed).

## Upgrading 0.2.x → 0.3.x: one behaviour change to plan for

*History: for a 0.1 or 0.2 install on its way to 0.7.1.*

`spec.discoverUnmanaged` defaults to `true`. With it on, discovery asks EC2 for *every* VPC in
the target and applies the tag selector in Go, then makes one extra `DescribeSubnets` round
for the VPCs the selector left out (`internal/cloud/aws/discover.go`). Consequences on the
first sync after the upgrade:

- more data per target and one more API call per target (see [limits](limits.md));
- `status.unmanaged` becomes non-zero and the `UNMANAGED` column appears in
  `kubectl get networkscopes`;
- `UnmanagedNetworkResource` fires once for every untagged resource in the organization —
  which is the point, but tell whoever is on call first.

Set `discoverUnmanaged: false` on the scope if you want the 0.2.x behaviour back.
Auto-import stays off unless you configure `spec.autoImport`, and `Apply` mode additionally
requires `--enable-writes` and a `writeRoleARN`.

## Upgrading from 0.3 to 0.7

*History: for an install older than 0.7 on its way to 0.8.* Nothing between 0.3 and 0.7 changes
existing objects, and every hop is the usual two steps, with the `aws-subnet-operator` chart.
What to look at on the way:

- **0.4**: the admission webhooks. The chart serves them with a certificate of its own, or one
  from cert-manager (`webhook.certificate.certManager`); objects that were accepted before and that the
  webhooks would refuse keep working until they are changed. The chart runs two replicas with a
  disruption budget, and credential Secrets (`SheetExport`) are read only in the namespace they
  live in.
- **0.4.1**: the image is public at `ghcr.io/aivandrago/subnet-operator`. A values file that
  sets `image.repository` to another registry keeps pulling from there; drop it, or mirror the
  new image.
- **0.5**: apply the CRDs before the chart as always: `status.targets[].unmanagedIDs` is what
  lets a restart tell known unmanaged resources from new ones. New alerts arrive with it
  (`SubnetOperatorDown`, `SubnetClaimNotReady`, `ResourceImportNotSettled`).
- **0.6**: throttled accounts are backed off rather than reported unreachable, and alert as
  `SubnetInventoryTargetThrottled`. The chart is published to ghcr.io as well.
- **0.7**: the section below.

## Upgrading from 0.6 to 0.7: namespace-restricted scopes and `created_by`

*History: this is what 0.7 changed in the `aws.hypersurgery` group. In
`network.hypersurgery.dev` (0.8 and later) an unset `namespaceSelector` allows **no**
namespace; the move is in [0.7 to 0.8](#field-by-field).*

The release that adds `NetworkScope.spec.namespaceSelector` and the `created-by` annotation
changes nothing for existing objects on its own:

- **Scopes without a selector keep allowing every namespace.** Each gets a
  `NamespacesUnrestricted` Warning Event, and applying one prints a warning. Add a selector to
  every scope that has a `writeRoleARN` — first check where its claims and imports live
  (`kubectl get subnetclaims,resourceimports -A -o wide`), so the selector does not leave any of
  them out.
- **Existing claims and imports carry no `created-by` annotation**, so their audit lines say
  `created_by: unknown`. The webhook refuses adding one later: nobody can vouch for who created
  them.
- **The chart's ClusterRole gains `get`/`list`/`watch` on `namespaces`**, for their labels.
  Installs that manage the operator's RBAC themselves must add it, or every claim and import that
  uses a scope with a selector is refused by the webhook and retried, without progress, by the
  controller.
- **An update that drops annotations is refused.** `kubectl apply`, server-side apply and GitOps
  tools keep annotations they do not manage; a full `kubectl replace` of a claim or import from a
  manifest without the annotation is refused with a message naming it.

## Upgrading from 0.7 to 0.8

0.8 is the release that moves the API and renames the project, so users do both once
([ADR 0002](../adr/0002-multi-cloud-model.md), #42, #76):

| What | 0.7 | 0.8 |
|---|---|---|
| API group | `aws.hypersurgery/v1alpha1` | `network.hypersurgery.dev/v1beta1`; the old group is still served, deprecated, and removed in 0.9 |
| Kinds | `NetworkScope`, `VPC`, `Subnet`, `SubnetClaim`, `ResourceImport`, `SheetExport` | the same, with `VPC` → `Network` |
| Chart | `aws-subnet-operator` (`hypersurgery/aws-subnet-operator`, `oci://ghcr.io/aivandrago/charts/aws-subnet-operator`) | `subnet-operator` (`hypersurgery/subnet-operator`, `oci://ghcr.io/aivandrago/charts/subnet-operator`) |
| Namespace in the docs and the kustomize install | `aws-subnet-operator-system` | `subnet-operator-system` (an existing release stays where it is) |
| Object labels and annotations | `aws.hypersurgery/*` | `network.hypersurgery.dev/*` (`scope`, `provider`, `account`, `region`, `network`, `resource`, `created-by`, `reason`) |
| Grafana dashboard | UID `aws-subnet-operator`, "Subnet inventory (AWS)" | UID `subnet-operator`, "Subnet inventory": bookmarks to the old UID stop working |
| Google Sheet columns | Account, Region, VPC, VPC name, …, AZ, … | Provider, Account, Region, Network, Network name, …, Zone, …: one column more at the front |
| Metrics, alerts | `hs_aws_*` | unchanged; 0.9 renames them to `hs_*`, [below](#upgrading-from-08-to-09) |
| AWS side | IAM roles, SQS queue, EventBridge rules, CloudFormation stacks named `aws-subnet-operator-*`, and the `aws-subnet-operator` session name CloudTrail shows for the operator's calls | **unchanged**: nothing to redeploy, no policy or CloudTrail query to change. They are AWS resources, and renaming them would replace them |
| Leader election lease | `1095b947.hypersurgery` | unchanged, so a 0.7 and a 0.8 pod never lead at the same time during the rollout |

### Field by field

What `manager migrate-manifests` and the in-cluster migration both do, with one Go function:

| `aws.hypersurgery/v1alpha1` | `network.hypersurgery.dev/v1beta1` |
|---|---|
| (implicit AWS) | `spec.provider: AWS` |
| `spec.accounts[].roleARN`, `externalID`, `writeRoleARN` | `spec.accounts[].aws.roleARN`, `aws.externalID`, `aws.writeRoleARN` |
| `spec.vpcTagSelector` | `spec.networkSelector.matchTags` |
| `spec.tagKeys` (defaulted by the CRD) | written out in full, so the provider-dependent defaults of the new group cannot change them |
| `spec.namespaceSelector` **unset: every namespace** | unset: **no** namespace. A migrated scope that had none gets `{}` (every namespace) and an Event saying so |
| `spec.autoImport.inheritFromVPC` | `spec.autoImport.inheritFromNetwork`; `managedTag` written out |
| `SubnetClaim` `spec.vpcID`, `spec.availabilityZones` | `spec.networkID`, `spec.zones` |
| `SubnetClaim` `spec.routeTableID`, `spec.mapPublicIPOnLaunch` | `spec.aws.routeTableID`, `spec.aws.mapPublicIPOnLaunch` |
| `status.allocations[]` keyed by `availabilityZone` | keyed by `name` (`<namePrefix>-<zone suffix>`, the Name tag the subnet already has), with `zone` |
| `VPC` `spec.vpcID`, `status.isDefault` | `Network` `spec.id`, `status.aws.isDefault` |
| `Subnet` `spec.subnetID`, `spec.vpcID`, `status.availabilityZone`, `status.public`, `status.routeTableID`, `status.availabilityZoneID` | `spec.id`, `spec.networkID`, `status.zone`, `status.aws.public`, `status.aws.routeTableID`, `status.aws.availabilityZoneID` |
| `status.totalIPs`, `availableIPs`, `utilizationPercent` (0 when unknown) | the same, unset when unknown |
| `NetworkScope` `status.vpcs`, `targets[].vpcs`, `targets[].unmanagedVPCs` | `status.networks`, `targets[].networks`, `targets[].unmanagedNetworks` |

AWS-only details are refused per provider by the webhooks and the controllers instead of the
schema: a 12-digit account ID, `vpc-`/`subnet-` IDs, one to six availability zones, a /16 to
/28 prefix, a region on every import.

### Before you start

- **The service account's name changes with the release's objects** (below), and EKS Pod
  Identity associations and IRSA trust policies name the service account. Either create the
  association (or add the new `system:serviceaccount:<namespace>:<name>` to the trust policy)
  for the new name before upgrading, or keep the old name with
  `--set serviceAccount.name=<old name>`, e.g. `subnet-operator-aws-subnet-operator`.
  `kubectl -n <namespace> get serviceaccount` shows the current one.
- **A scope the chart creates (`networkScope.create`) gets no `namespaceSelector` unless you set
  one**, and in the new group that means no namespace may use it. 0.7 values keep rendering
  (`roleARN` next to the `id`, and `vpcTagSelector`, are still accepted), but set
  `networkScope.namespaceSelector` — `{}` for every namespace, as 0.7 had it, or better a real
  selector. Scopes you applied yourself are migrated with `{}`.
- **GitOps repositories**: convert the manifests, after the upgrade or before it — both orders
  work (see [below](#manifests-in-git)).

### Steps

```sh
# 1. The CRDs of both groups. Applying them adds network.hypersurgery.dev and marks
#    aws.hypersurgery deprecated; the replace moves the nscope short name to the new group.
helm pull oci://ghcr.io/aivandrago/charts/subnet-operator --version 0.8.0 --untar
kubectl apply -f subnet-operator/crds/
kubectl replace -f subnet-operator/crds/aws.hypersurgery_networkscopes.yaml

# 2. The release, in place, to the renamed chart. Helm cannot rename a release: it keeps its
#    name and namespace.
helm upgrade subnet-operator oci://ghcr.io/aivandrago/charts/subnet-operator --version 0.8.0 \
  -n aws-subnet-operator-system -f my-values.yaml
#    From the chart repository: helm repo update hypersurgery, then hypersurgery/subnet-operator.

# 3. Wait for the migration: hs_migration_pending_objects is 0 for every kind, or, without
#    Prometheus, every old object carries network.hypersurgery.dev/migrated-to.
kubectl get networkscopes.aws.hypersurgery,sheetexports.aws.hypersurgery \
  -o custom-columns='NAME:.metadata.name,MIGRATED-TO:.metadata.annotations.network\.hypersurgery\.dev/migrated-to'
kubectl get subnetclaims.aws.hypersurgery,resourceimports.aws.hypersurgery -A \
  -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,MIGRATED-TO:.metadata.annotations.network\.hypersurgery\.dev/migrated-to'
```

**What the release's objects are called afterwards** depends on its name, because the chart
names them `<release>-<chart>` unless the release name already contains the chart's name:

| Release name | Deployment, Services, ServiceAccount, RBAC, webhooks in 0.7 | in 0.8 |
|---|---|---|
| `subnet-operator` (the name the install docs use) | `subnet-operator-aws-subnet-operator…` | `subnet-operator…`: Helm creates them under the new names and deletes the old ones |
| `aws-subnet-operator` | `aws-subnet-operator…` | unchanged. The Deployment keeps the `app.kubernetes.io/name` selector it was created with — the chart reads it back from the cluster — because a selector cannot change |
| anything else, e.g. `inventory` | `inventory-aws-subnet-operator…` | `inventory-subnet-operator…` |

During the rollout an old and a new Deployment can exist side by side; they share the leader
election lease, so only one of them reconciles. The webhook serving certificate is signed
again for the new Service name, unless cert-manager issues it.

**Reinstalling instead** (a new release name or namespace, e.g. to get to
`subnet-operator-system`): `helm uninstall` the old release first — it leaves the CRDs and
therefore every claim, import and scope you applied, but deletes a `NetworkScope` or
`SheetExport` the chart created, together with that scope's `VPC` and `Subnet` objects, which the
new scope rediscovers — then install the new chart. Two operators must not run side by side in
different namespaces: they would not share a lease.

### What the migration does

The 0.8 operator runs one migration controller per old kind while the API server serves the old
group (`--migrate-v1alpha1`, on by default):

- For every `NetworkScope`, `SubnetClaim`, `ResourceImport` and `SheetExport` of
  `aws.hypersurgery/v1alpha1` it creates the `network.hypersurgery.dev/v1beta1` object with the
  same name and namespace, and copies the status through the status subresource: a claim's
  reservations (in `Allocate` mode the only record of them), an import's state and history, a
  scope's known unmanaged resources (so none of them counts as new and
  `UnmanagedNetworkResource` stays quiet), an export's last run.
- It then marks the old object `network.hypersurgery.dev/migrated-to: <name>` and records a
  `Migrated` Event on it, with notes such as the `namespaceSelector` it made explicit
  (`kubectl describe networkscopes.aws.hypersurgery <name>`). A copy the API server refuses
  leaves a `MigrationFailed` Event and is retried.
- Scopes go first. Until an object's old counterpart is migrated, the new object's controller
  leaves it alone, so nothing is allocated, applied or counted twice; a claim also keeps clear
  of the CIDRs that unmigrated old claims still hold.
- The old scope's `VPC` and `Subnet` objects are deleted: the new scope rebuilds them as
  `Network` and `Subnet` objects on its first sync. Nothing in AWS changes.
- **Who created a claim or an import is kept.** The copy carries the old
  `aws.hypersurgery/created-by` as `network.hypersurgery.dev/created-by`. The new group's
  webhook normally writes the requesting user into that annotation; it keeps a copied value
  only when the operator's own service account creates an object marked
  `network.hypersurgery.dev/migrated-from`, and it skips the checks the old group's webhook
  already made on it, so a full network cannot strand a claim's reservations. Nobody else can
  use that way in. An object from before 0.7, which never had a creator, gets none: the audit
  trail says `unknown` rather than naming the operator.
- Once migrated, **the old object is a record.** The old group's webhook refuses a change to its
  spec, naming the object to change instead; labels and re-applies of the same manifest pass.
  A copy that already existed — applied from a converted repository first — keeps its spec and
  gets the old status if it had none of its own.

### Manifests in git

Argo CD or Flux would keep re-applying `aws.hypersurgery` manifests. Convert them with the
same mapping the operator uses:

```sh
docker run --rm -i ghcr.io/aivandrago/subnet-operator:0.8.0 migrate-manifests < old.yaml > new.yaml
```

Old-group `NetworkScope`, `SubnetClaim`, `ResourceImport` and `SheetExport` documents are
converted; `VPC` and `Subnet` documents are left out (the operator writes those); everything
else is copied byte for byte. Converted documents lose their comments and their status. Notes
go to stderr — an unset `namespaceSelector` made explicit, and documents that still mention
`aws.hypersurgery`, such as RBAC rules with `vpcs`, which need a hand edit.

Either order works: upgrade first and commit the conversion afterwards (the tool's apply then
updates the migrated copies), or commit the new manifests first (the migration fills in their
status). A tool that prunes will delete the old-group objects once they leave the repository,
which is what should happen to them anyway.

### While both groups are installed

`kubectl` resolves a plain resource name to the group that sorts first, and `aws.hypersurgery`
sorts before `network.hypersurgery.dev`: `kubectl get subnetclaims` shows the old claims until
the old CRDs are gone. Use `kubectl get hypersurgery` (a category only the new kinds are in),
the short names `nscope`, `hsnet`, `hssubnet`, or the full name, e.g.
`kubectl get subnetclaims.network.hypersurgery.dev -A`.

### Removing the old group

The chart never deletes CRDs. When every old object is migrated (`hs_migration_pending_objects`
is 0 for every kind), the old CRDs can go, and with them the old objects; nothing in the cloud
changes:

```sh
kubectl delete crd networkscopes.aws.hypersurgery vpcs.aws.hypersurgery subnets.aws.hypersurgery \
  subnetclaims.aws.hypersurgery resourceimports.aws.hypersurgery sheetexports.aws.hypersurgery
kubectl -n <namespace> rollout restart deployment/<deployment>   # 0.8 stops watching the old group
```

Or leave them until after the upgrade to 0.9, which no longer ships them but does not delete
them either ([below](#the-old-crds)). Do not re-apply the 0.8 `crds/` directory after deleting
them — it contains the old group too.

### Rolling back to 0.7

`helm rollback` to the 0.7 revision restores the old chart's objects and names. The 0.7
operator reconciles the old objects again (it ignores the `migrated-to` marker); the new-group
objects stay, unreconciled. Anything done through the new group after the upgrade — a claim's
new reservations, an import applied since — is not in the old objects, so roll back soon or not
at all. Leave the CRDs as they are.

## Upgrading from 0.8 to 0.9

0.9 removes `aws.hypersurgery/v1alpha1`, as announced when 0.8 deprecated it
([ADR 0002](../adr/0002-multi-cloud-model.md) §10): its CRDs are no longer in the chart, nothing
serves webhooks for it, and nothing migrates it any more. It also renames the metrics
([below](#metrics-and-alerts)) and moves the AWS settings under `providers.aws`
([below](#providers)).

**Upgrade from 0.8 only.** Coming from 0.7, upgrade to 0.8 first
([above](#upgrading-from-07-to-08)), wait until it has migrated every object, then upgrade to
0.9. Going from 0.7 straight to 0.9 is not supported: nothing in 0.9 can move an object out of
the old group, and it will not run its controllers next to one that was never moved
([below](#objects-08-never-migrated)).

### Before you start

- **The migration is finished.** On 0.8, `hs_migration_pending_objects` is 0 for every kind, or
  every old object carries `network.hypersurgery.dev/migrated-to` (the `kubectl get` commands in
  step 3 [above](#steps) list them). An object you do not want migrated, delete instead.
- **Git no longer holds `aws.hypersurgery` manifests.** Convert them first
  ([below](#manifests-in-git-1)). A GitOps tool that re-creates an old object after the upgrade
  does no harm at once — the running operator ignores it and says so — but the next time the
  operator starts, that object keeps it from starting its controllers.
- **Values and flags.** The 0.7 forms of the chart's scope values, which 0.8 still accepted,
  are refused ([below](#values-and-flags)), and `--migrate-v1alpha1` is gone: remove it from
  `extraArgs`, or the manager exits on an unknown flag.
- **Metrics and alerts.** Rules, dashboards and routes of your own need the new names
  ([below](#metrics-and-alerts)).
- **RBAC you manage yourself.** The operator lists the old group's `networkscopes`,
  `subnetclaims`, `resourceimports` and `sheetexports` while their CRDs exist (`list` only, see
  `config/rbac/role.yaml`); everything else it had on the old group can go.

### Steps

The usual two, [above](#upgrading): the new CRDs first, then the release.

```sh
helm repo update hypersurgery
helm pull hypersurgery/subnet-operator --untar      # or oci://ghcr.io/aivandrago/charts/subnet-operator
kubectl apply -f subnet-operator/crds/
helm upgrade subnet-operator hypersurgery/subnet-operator -n <namespace> -f my-values.yaml
kubectl -n <namespace> rollout status deployment/<deployment>
```

Applying the 0.9 CRDs changes only `network.hypersurgery.dev` (three optional status fields,
[below](#providers)). The `aws.hypersurgery` CRDs are not touched: `kubectl apply` does not
delete what a directory no longer contains, and Helm never deletes CRDs. The release keeps its
name, namespace and object names; `helm upgrade` removes the old group's webhooks and RBAC.

### Objects 0.8 never migrated

On every start, the operator lists the old group's `NetworkScope`, `SubnetClaim`,
`ResourceImport` and `SheetExport` objects, if their CRDs exist, and looks for ones without the
`network.hypersurgery.dev/migrated-to` marker that 0.8 set once an object's copy existed
(objects being deleted do not count; `VPC` and `Subnet` objects were a cache and are ignored).
Such an object is state that nothing will ever read — a claim's reservations, an import's
history — and a claim in the new group could be handed a CIDR the old one still holds. So while
one exists, the operator:

- starts **no controllers and no webhooks**, and does not take part in leader election;
- **fails its readiness probe**, with the reason as the answer;
- **logs** how many objects there are, names the first five, and says what to do;
- records a **`MigrationPending` Warning Event** on each such object
  (`kubectl describe subnetclaims.aws.hypersurgery <name> -n <namespace>`);
- sets **`hs_migration_pending_objects{kind}`** to the count per kind;
- **looks again every 30 seconds**, and once nothing is left it exits, so that the container is
  restarted into normal operation (its restart count goes up by one).

It does the same when it cannot tell — for example when its RBAC lacks `list` on the old group
— rather than assume there is nothing.

**During `helm upgrade`** that means the rollout stops: the first new pod never becomes ready,
and with the Deployment's default rolling update (up to three replicas: one pod more, none
fewer; the chart runs two) the 0.8 pods keep running and serving the new group. They can no longer migrate, though: the upgrade has already replaced
their RBAC and removed the old group's webhooks. `helm upgrade --wait` times out. Then either

- **go back and let 0.8 finish**: `helm rollback <release> <0.8 revision> -n <namespace>`, wait
  until nothing is pending, and upgrade again; or
- **delete the objects** the log and the Events name, if they are not needed (or were never meant
  to be migrated). The waiting pod notices within 30 seconds and the rollout completes on its own.

The metric of a waiting pod is not scraped through the chart's `ServiceMonitor`: a pod that is
not ready is not an endpoint of the Service. The log, the Events, and the metric read from the
pod itself (`kubectl port-forward`) all say the same.

**While the operator runs**, it counts the old objects every minute, as long as their CRDs exist.
An old object that appears later — a GitOps tool applying a manifest nobody converted — is not
acted on; it gets the same Event and count, and a log line. It does not stop a running operator,
but it would stop the next start, so convert the manifest and delete the object.

### The old CRDs

They stay in the cluster, with the old objects in them, until you delete them. 0.9 needs
nothing from them: once the operator runs, every old object carries `migrated-to` and its copy
in `network.hypersurgery.dev` is the one reconciled. Deleting them deletes the old objects and
nothing else; the new objects and the cloud are not affected, and no restart is needed (0.9
does not watch the old group).

```sh
# Nothing pending: every line shows a MIGRATED-TO. (No output at all: nothing is left anyway.)
kubectl get networkscopes.aws.hypersurgery,sheetexports.aws.hypersurgery \
  -o custom-columns='NAME:.metadata.name,MIGRATED-TO:.metadata.annotations.network\.hypersurgery\.dev/migrated-to'
kubectl get subnetclaims.aws.hypersurgery,resourceimports.aws.hypersurgery -A \
  -o custom-columns='NAMESPACE:.metadata.namespace,NAME:.metadata.name,MIGRATED-TO:.metadata.annotations.network\.hypersurgery\.dev/migrated-to'

kubectl delete crd networkscopes.aws.hypersurgery vpcs.aws.hypersurgery subnets.aws.hypersurgery \
  subnetclaims.aws.hypersurgery resourceimports.aws.hypersurgery sheetexports.aws.hypersurgery
```

Afterwards `kubectl get subnetclaims` (and `subnets`, `networkscopes`) mean the new group
again, and `hs_migration_pending_objects` stays 0. Keep a backup of the old objects
(`kubectl get … -o yaml`) if you want the record; nothing reads it.

### Rolling back to 0.8

`helm rollback` to the 0.8 revision brings back 0.8's RBAC, the old group's webhooks and the
migration, and the `hs_aws_*` metric names ([below](#metrics-and-alerts)). If the old CRDs are
still there, 0.8 carries on where it left off; if you deleted them, 0.8 runs without the old
group, as it does whenever the API server does not serve it. Leave the CRDs as they are either
way: the 0.8 `crds/` directory would bring the old group back, empty.

### Manifests in git

`manager migrate-manifests` is still in the 0.9 image and converts `aws.hypersurgery/v1alpha1`
manifests exactly as it did in 0.8 ([above](#manifests-in-git)):

```sh
docker run --rm -i ghcr.io/aivandrago/subnet-operator:<version> migrate-manifests < old.yaml > new.yaml
```

It needs no cluster and converts metadata and spec only. Applying the result updates the copy
0.8 made of each object, which is what a repository should do after the upgrade. It is not a
way to migrate objects in a cluster: a manifest carries no status, so a claim applied from it
where 0.8 never made a copy starts without its reservations (and in `Create` mode re-adopts its
subnets by their `hs/claim` tag).

### Values and flags

| Removed in 0.9 | Instead |
|---|---|
| `networkScope.vpcTagSelector` | `networkScope.networkSelector.matchTags` |
| `roleARN`, `externalID`, `writeRoleARN` next to an account's `id` in `networkScope.accounts` | the same keys in the account's `aws` member |
| `--migrate-v1alpha1` | nothing: there is no migration |

A values file that still sets one of the chart values fails to render, with a message naming the
replacement; the API server would otherwise drop the unknown fields without a word, and an
account would be read with the operator's own identity instead of its role.

### Metrics and alerts

0.9 exports every metric under a cloud-neutral name with a `provider` label, as
[ADR 0002](../adr/0002-multi-cloud-model.md) §7 decides (#44), and **no longer exports the 0.8
names**. There is no release that exports both: rules, dashboards and alert routes of your own
that read `hs_aws_*` go quiet on 0.9 until you move them, so move them as part of the upgrade.
One alert is renamed too: `VPCCIDROverlap` is `NetworkCIDROverlap`.

| 0.8 (removed in 0.9) | 0.9 |
|---|---|
| `hs_aws_subnet_available_ips` | `hs_subnet_available_ips` |
| `hs_aws_subnet_total_ips` | `hs_subnet_total_ips` |
| `hs_aws_subnet_missing_required_tags` | `hs_subnet_missing_required_tags` |
| `hs_aws_vpc_cidr_overlaps` | `hs_network_cidr_overlaps` |
| `hs_aws_target_up` | `hs_target_up` |
| `hs_aws_target_sync_errors_total` | `hs_target_sync_errors_total` |
| `hs_aws_target_throttled` | `hs_target_throttled` |
| `hs_aws_api_throttled_total` | `hs_api_throttled_total` |
| `hs_aws_scope_last_sync_timestamp_seconds` | `hs_scope_last_sync_timestamp_seconds` |
| `hs_aws_unmanaged_resources` | `hs_unmanaged_resources` |
| `hs_aws_unmanaged_resources_total` | `hs_unmanaged_resources_total` |
| `hs_aws_auto_imports_total` | `hs_auto_imports_total` |
| `hs_aws_subnet_claim_ready` | `hs_subnet_claim_ready` |
| `hs_aws_resource_import_ready` | `hs_resource_import_ready` |
| `hs_migration_pending_objects` | kept, and neutral from the start. It now counts old objects 0.8 never migrated: above 0, the operator does not start its controllers ([above](#objects-08-never-migrated)); 0 once the old CRDs are gone |

| Label | 0.8 | 0.9 |
|---|---|---|
| cloud | none: the prefix said `aws` | `provider="aws"` on every metric; on the claim and import gauges it is the scope's, empty while the scope does not exist |
| network | `vpc_id` | `network_id` |
| zone | `az` | `zone` |
| `kind` of the unmanaged metrics | `vpc`, `subnet` | `network`, `subnet` |
| `scope`, `account`, `region` and the rest | | unchanged |

**The chart's alerts** move to the new names in 0.9. What changes is their name, what they
carry and what they say:

- **One name.** `VPCCIDROverlap` is now `NetworkCIDROverlap`, like the metric it reads,
  `hs_network_cidr_overlaps`. Alertmanager routes, inhibitions and silences that match on
  `alertname="VPCCIDROverlap"` need the new name; the runbook anchor is
  `runbook.md#networkcidroverlap`. Every other alert keeps its name.
- **Labels.** Every alert computed from the operator's metrics carries `provider`.
  `SubnetFull` and `SubnetNearlyFull` carry `network_id` and `zone` instead of `vpc_id` and
  `az`, `NetworkCIDROverlap` carries `network_id`, and `UnmanagedNetworkResource` has
  `kind="network"` where it had `kind="vpc"`. Alertmanager routes, inhibitions and silences
  that match on `vpc_id`, `az` or `kind="vpc"` need the new label; silences are the easy one to
  miss, because they expire rather than fail.
- **Text.** Summaries and descriptions name networks and the provider rather than VPCs, ENIs,
  EC2 and AWS: `NetworkCIDROverlap`'s summary reads "Network … overlaps another network",
  `SubnetInventoryTargetThrottled`'s "… is throttled by the cloud provider". Templates that
  match on the text, which is rare, need a look.
- **A fix in `UnmanagedNetworkResource`.** Its window was `[10m]` with `for: 10m`, and one new
  resource raised the increase for one evaluation less than the `for` needed, so it never
  fired for a single resource. It is `increase(hs_unmanaged_resources_total[30m]) > 0` now:
  it fires ten minutes after a new resource appears and resolves about half an hour after.
  Expect it where 0.8 was silent.
- **`AutoImportedResources` sees the first import after a restart.** `hs_auto_imports_total`
  now exists at 0 for every result as soon as a scope runs the auto-import policy, so the first
  import after the operator starts is a rise `increase()` can see. In 0.8 that series first
  appeared at 1 and the digest left the import out. `hs_target_sync_errors_total` and
  `hs_unmanaged_resources_total` start at 0 the same way.

**The Grafana dashboard** keeps its UID and panels. It reads the new names and gains a
`Provider` variable in front of `Scope`; the tables show `Zone` where they showed `AZ`.

**Rules and dashboards of your own** stop getting data on 0.9 if they read the old names. Move
them with the upgrade: switch the queries, then check them against the 0.9 instance once it is
scraped. For most, the rename is mechanical:

```sh
# Review the result: \baz\b and \bvpc_id\b also match words in comments and annotations.
sed -E -i \
  -e 's/hs_aws_vpc_cidr_overlaps/hs_network_cidr_overlaps/g' \
  -e 's/hs_aws_/hs_/g' \
  -e 's/\bvpc_id\b/network_id/g' \
  -e 's/\baz\b/zone/g' \
  -e 's/kind="vpc"/kind="network"/g' \
  -e 's/VPCCIDROverlap/NetworkCIDROverlap/g' \
  my-rules.yaml my-dashboard.json alertmanager.yaml

grep -rn 'hs_aws_\|VPCCIDROverlap' .   # nothing should be left once you are done
```

A rule that aggregates away every label but a few (`sum by (account)`) is unaffected by the
label renames. A rule on the new names that should only see one cloud adds `provider="aws"`.

Silences on `VPCCIDROverlap` do not carry over: recreate the ones you still need for
`NetworkCIDROverlap`.

**During the rollout and a rollback.** `helm upgrade` replaces the rules when it replaces the
operator. For the minute or two until a 0.9 replica is scraped, the new names have no series:
the chart's alerts other than `SubnetOperatorDown` go quiet for that time rather than fire,
exactly as they do while the operator restarts. Rolling back to 0.8 restores the 0.8 rules with
the 0.8 release, and with them the 0.8 names: rules of your own that you already moved go quiet
until you roll forward again.

### Providers

The operator now runs clouds as registered providers (#43); AWS is the only one. Nothing
changes in AWS, and every 0.8 values file keeps working.

- **CRDs**: apply them as always. They add three optional status fields the operator writes:
  `NetworkScope.status.capabilities` (`CreateSubnet`, `IPUsage`, and `ChangeEvents` when a queue
  is configured), `NetworkScope.status.ownership` (`ResourceTags` for networks and subnets on
  AWS) and `Subnet.status.ownershipSource` (`Subnet` on AWS).
- **Values**: AWS settings move under `providers.aws`. The 0.8 names still work in 0.9, and
  `helm install`/`upgrade` prints a note while they are set; the new name wins when both are.
  They were announced for removal in 0.10 and are **removed in 1.0**, the release after 0.9
  ([below](#values-and-flags-removed-in-10)).

  | 0.8 (deprecated in 0.9, removed in 1.0) | 0.9 |
  |---|---|
  | `events.queueUrl`, `events.debounce` | `providers.aws.events.queueUrl`, `providers.aws.events.debounce` |
  | `aws.region`, `aws.endpointURL` | `providers.aws.region`, `providers.aws.endpointURL` |
  | `networkPolicy.egress.podIdentity` (`enabled`, `cidr`, `port`) | `providers.aws.podIdentity`, the same keys; a key set there wins |
  | `serviceAccount.annotations."eks.amazonaws.com/role-arn"` (not deprecated; it still wins) | `providers.aws.irsaRoleARN` |

- **Flags** (only if you pass them yourself, e.g. with `extraArgs`): `--providers` (default
  `aws`) chooses the clouds; `--events-queue-url` and `--events-debounce` became
  `--aws-events-queue-url` and `--aws-events-debounce`. The old flags and `EVENTS_QUEUE_URL`
  still work in 0.9, and are removed in 1.0.
- **New condition reasons**: `ProviderNotEnabled` on a scope, claim or import whose provider the
  operator is not started with (replaces `ProviderNotSupported`, which no release could
  produce for a valid object), and `CreateNotSupported` on a Create-mode claim of a provider
  that cannot create subnets (none today).
- **An EC2 subnet without an `AvailableIpAddressCount`** is now reported with unknown free IPs
  (unset `availableIPs` and `utilizationPercent`) instead of as full. EC2 always sends the count,
  so this only matters for emulators.

## Upgrading from 0.9 to 1.0

1.0 graduates the API to `network.hypersurgery.dev/v1` ([ADR 0002](../adr/0002-multi-cloud-model.md)
§10, #58). v1 has the same fields as v1beta1; what changes is where objects are stored and what
is promised about them ([API compatibility](../api-compatibility.md)):

- **v1 is the storage version.** Every object is stored at v1 once the upgrade is done.
- **v1beta1 is deprecated and still served**, until at least 1.2 and six months after 1.0.
  `kubectl` prints a warning for every request at v1beta1; reads and writes keep working, and
  are converted to and from v1.
- **The operator reads and writes v1**, and its admission webhooks are registered for v1. The
  API server hands them requests made at v1beta1 converted to v1, so both versions get the same
  defaults and checks.

**Upgrade from 0.9.** Coming from 0.8 or older, upgrade to 0.9 first ([above](#upgrading-from-08-to-09)).

### Before you start

- **Move the values and flags 0.9 deprecated.** `events.*`, `aws.*` and
  `networkPolicy.egress.podIdentity` are removed, and so are `--events-queue-url`,
  `--events-debounce` and `EVENTS_QUEUE_URL`. The 1.0 chart refuses to render while one is set,
  and the 1.0 operator refuses to start with one, so move them before you upgrade
  ([below](#values-and-flags-removed-in-10)). 0.9 already reads the new names, so the moved
  values can go in first, on 0.9.

- **RBAC you manage yourself.** The operator now needs, on its own six CRDs by name and on
  nothing else of `apiextensions.k8s.io`: `get` and `patch` on `customresourcedefinitions`, and
  `get` and `update` on `customresourcedefinitions/status` (see `config/rbac/role.yaml`). The
  chart's ClusterRole has them.
- **The kustomize install** (`make deploy`) still has no webhooks by default, so its CRDs keep
  the API server's own conversion. Uncommenting its `[WEBHOOK]` and `[CERTMANAGER]` sections
  (with [cert-manager](https://cert-manager.io/) in the cluster) now also points every CRD's
  conversion at the webhook, with the CA cert-manager injects. That wiring is covered by a
  render test, envtest and an end-to-end run with cert-manager (`make test-e2e-certmanager`).
  The Helm chart needs nothing new.
- **A GitOps tool that manages the CRDs** may report `spec.conversion` as drift, because the
  operator sets it and the CRDs in `crds/` do not. Tell it to ignore that field (Argo CD:
  `ignoreDifferences` with `jsonPointers: [/spec/conversion]` on the six CRDs).

### Steps

The usual two, [above](#upgrading): the new CRDs first, then the release.

```sh
helm repo update hypersurgery
helm pull hypersurgery/subnet-operator --untar      # or oci://ghcr.io/aivandrago/charts/subnet-operator
kubectl apply -f subnet-operator/crds/
helm upgrade subnet-operator hypersurgery/subnet-operator -n <namespace> -f my-values.yaml
kubectl -n <namespace> rollout status deployment/<deployment>
```

Applying the 1.0 CRDs adds v1 to each of them as the storage version and marks v1beta1
deprecated. The objects already in the cluster are untouched by that: they stay stored as
v1beta1 until they are written again, and each CRD's `status.storedVersions` now lists both,
`[v1beta1 v1]`.

### What the operator does on its first start

Nothing to do by hand; this is what to expect, and what to check.

1. **It rewrites every object at v1.** The leader writes each object of each kind back
   unchanged, through its status subresource, so the API server stores it again at v1. Nothing
   in it changes but its `resourceVersion`: not the spec, not the generation, not the status.
   The admission webhooks are not called for it. It takes about one write per object.
2. **It trims `status.storedVersions` to `[v1]`** on each CRD whose objects it rewrote. A
   version that is still listed there cannot be removed from the CRD, which is what a release
   after 1.2 does with v1beta1.
3. **It records it**: an Event `StorageVersionMigrated` on each CRD (`kubectl get events -n
   default --field-selector reason=StorageVersionMigrated`), a log line per CRD, the counter
   `hs_storage_migration_rewritten_objects_total{kind}`, and `hs_crd_stored_versions{kind,
   version}`, which is 1 for each listed version: `v1` alone once it is done.
4. **It then points the CRDs' conversion at its webhook** (strategy `Webhook`, the Service
   `<fullname>-webhook` — `subnet-operator-webhook` for a release called `subnet-operator` —,
   path `/convert`, the CA from `ca.crt` in the webhook certificate's Secret), with an Event `ConversionConfigured`, and keeps it so every minute, following a
   renewed CA. Until the objects are rewritten, the CRDs keep the `None` strategy they are
   installed with: v1beta1 and v1 have the same fields, so the API server converts exactly on
   its own, and the operator's reads do not depend on its own webhook while it starts. With
   `webhook.enabled=false` or `webhook.conversion.enabled=false` the CRDs stay at `None`.

Every later start finds `[v1]` and rewrites nothing. If the operator cannot finish, it logs
`Could not migrate the stored objects to v1 yet` with the reason and tries again every minute;
the usual reasons are CRDs that were not applied (the message says so) and RBAC of your own
that lacks the rules above. Nothing else waits for it: the controllers run meanwhile.

```sh
for crd in networkscopes networks subnets subnetclaims resourceimports sheetexports; do
  kubectl get crd $crd.network.hypersurgery.dev \
    -o jsonpath='{.metadata.name}: {.status.storedVersions} {.spec.conversion.strategy}{"\n"}'
done
# networkscopes.network.hypersurgery.dev: ["v1"] Webhook
# ...
```

### Reading and writing v1beta1

`kubectl get subnetclaims` now shows v1, the preferred version. v1beta1 is still there, by name:
`kubectl get subnetclaims.v1beta1.network.hypersurgery.dev`. With the conversion webhook, a
request at v1beta1 needs a running operator pod to answer it (a request at v1 does not, since
everything is stored at v1); while none is ready, v1beta1 requests fail and v1 requests work.
That includes clients that still ask for v1beta1 after the operator is uninstalled: use v1, or
set the CRDs back to `None` as below.

One thing v1 checks that v1beta1 did not: `spec.regions`, `spec.accounts[].regions` and
`spec.requiredSubnetTags` of a `NetworkScope` list each value once, and
`spec.autoImport.accountDefaults` has one entry per account. The webhook refuses a duplicate at
either version. A scope that already has one keeps working, and an update that leaves that list
as it was is accepted.

### Rolling back to 0.9

Set the CRDs' conversion back to `None` first, then roll the release back. 0.9 reads and writes
v1beta1 and does not serve the conversion webhook, so with `Webhook` every request it makes
would fail:

```sh
for crd in networkscopes networks subnets subnetclaims resourceimports sheetexports; do
  kubectl patch crd $crd.network.hypersurgery.dev --type=merge \
    -p '{"spec":{"conversion":{"strategy":"None","webhook":null}}}'
done
helm rollback subnet-operator <revision> -n <namespace>
```

Keep the 1.0 CRDs. The 0.9 ones cannot be applied any more once `storedVersions` is `[v1]` (the
API server refuses a CRD that drops a stored version), and they do not need to be: 0.9 works
with v1beta1 as the 1.0 CRDs serve it, and what it writes is stored at v1. Upgrading to 1.0
again later finds nothing to rewrite. `helm rollback` restores the values the 0.9 revision was
installed with; values you moved under `providers.aws` for 1.0 are read by 0.9 as well.

`internal/crdversions/rollback_test.go` runs the `kubectl` lines above, as they are written
here, against a real API server in the state 1.0 leaves it in, and checks each of these
statements: the conversion is `None` afterwards, v1beta1 reads and writes work, the 0.9 CRDs
are refused, and an upgrade after that rewrites nothing.

### Manifests in git

Change `apiVersion: network.hypersurgery.dev/v1beta1` to `network.hypersurgery.dev/v1`; nothing
else changes. `manager migrate-manifests` does it for a whole directory and keeps every document
as it was written otherwise, comments included (it still converts `aws.hypersurgery/v1alpha1`
too):

```sh
docker run --rm -i ghcr.io/aivandrago/subnet-operator:<version> migrate-manifests < old.yaml > new.yaml
```

A manifest left at v1beta1 keeps applying, with a warning, until v1beta1 is no longer served.

### Values and flags removed in 1.0

Deprecated in 0.9 ([Providers](#providers)), announced for removal in 0.10, and removed in 1.0,
which follows 0.9 ([policy](../policy.md#deprecation)):

| Removed in 1.0 | Instead |
|---|---|
| `events.queueUrl`, `events.debounce` | `providers.aws.events.queueUrl`, `providers.aws.events.debounce` |
| `aws.region`, `aws.endpointURL` | `providers.aws.region`, `providers.aws.endpointURL` |
| `networkPolicy.egress.podIdentity` (`enabled`, `cidr`, `port`) | `providers.aws.podIdentity`, the same keys |
| `--events-queue-url`, `EVENTS_QUEUE_URL` | `--aws-events-queue-url` (the chart sets it from `providers.aws.events.queueUrl`) |
| `--events-debounce` | `--aws-events-debounce` (the chart: `providers.aws.events.debounce`) |

A values file that still sets one of these fails to render, naming the replacement, for example
`Error: execution error at (subnet-operator/templates/deployment.yaml:1:4): aws.region was
removed in 1.0; use providers.aws.region`; so do the old flags in `extraArgs` and
`EVENTS_QUEUE_URL` in `extraEnv`. Nothing is changed in the cluster by the refused upgrade. The
operator itself, outside the chart, refuses to start with an old flag or with
`EVENTS_QUEUE_URL` set, and logs which replaces it. Without that, it would run without its event
queue, in another region or against another endpoint, or the NetworkPolicy would cut it off from
the EKS Pod Identity agent, and nothing would say so. An empty value (`queueUrl: ""`), as 0.9's
own defaults have, is not refused.

**`helm upgrade --reuse-values`** reuses the values the 0.9 release was installed with, old
names included. Check them, and upgrade with a values file instead where they need moving:

```sh
helm get values subnet-operator -n <namespace> -o yaml > my-values.yaml
# move events.*, aws.* and networkPolicy.egress.podIdentity under providers.aws, then:
helm upgrade subnet-operator hypersurgery/subnet-operator -n <namespace> -f my-values.yaml
```

The [upgrade test](../policy.md#the-upgrade-test) from 0.9 installs it with the `providers.aws` names,
checks that an upgrade with the old ones is refused and changes nothing, and then upgrades.

### Values, flags and metrics

| New in 1.0 | What it does |
|---|---|
| `webhook.conversion.enabled` (default `true`) | The operator points the CRDs' conversion at its webhook; `false` leaves it to the API server (`None`) |
| `--crd-conversion` (`webhook`, `none`, or empty) | What the chart's value turns into; empty, the default outside the chart, leaves the CRDs' conversion as installed (the kustomize install sets it with cert-manager's CA injector) |
| `--conversion-webhook-service` (`<namespace>/<name>`) | The Service in front of the webhook, for `--crd-conversion=webhook` |
| `hs_crd_stored_versions{kind, version}` | 1 for each version a CRD's `status.storedVersions` lists |
| `hs_storage_migration_rewritten_objects_total{kind}` | Objects the operator rewrote at the storage version |

## Upgrading from 1.x to 2.0

2.0 adds the Google Cloud provider ([ADR 0002](../adr/0002-multi-cloud-model.md), milestone
"2.0 — Google Cloud"). A new provider ships as a major release because of
what it changes outside the API ([policy](../policy.md#major-releases-and-new-providers)); the
API stays `network.hypersurgery.dev/v1` and only gains fields:

- `spec.provider: GCP`, and `spec.gcp.tagParent` on a `NetworkScope` (required with `GCP`,
  refused with any other provider, like `aws`), `spec.gcp.createTagValues`, and
  `spec.gcp.claimTag` (`Skip`, the default, or `Bind`; see
  [claims and imports on GCP](#claims-and-imports-on-gcp));
- `status.gcp` on `Network` and `Subnet` (on a `Network`, with its `peerings` and the details of
  its `overlaps`), and `Subnet.status.secondaryCIDRBlocks`;
- `spec.gcp` on a `SubnetClaim` (`poolCIDRs`, `privateIPGoogleAccess`), refused by the webhook
  on a scope of any other provider, like `aws` on a GCP one.

`v1beta1` gains the same fields, so the two versions still convert without loss. Nothing is
migrated, and the GCP provider only runs when it is enabled (`providers.gcp.enabled`,
`--providers=aws,gcp`); the upgrade itself is the usual [two steps](#upgrading).

The chart's `networkScope.create` can create a GCP scope: `networkScope.provider` (default
`AWS`, so an existing release renders the same scope as before), and with `GCP`,
`networkScope.gcp.tagParent` (required), `.createTagValues` and `.claimTag`
([chart README](../../charts/subnet-operator/README.md), [`examples/values-gcp.yaml`](../../examples/values-gcp.yaml)).

### The tag keys the operator writes depend on the provider

The breaking change of 2.0. Up to 1.x the tag keys the operator writes and defaults to were AWS
keys, the same for every provider. From 2.0 they are the provider's, because a Google Cloud tag
key cannot contain `/` ([ADR 0002](../adr/0002-multi-cloud-model.md) §5 and §6):

| Key | AWS (unchanged) | GCP |
|---|---|---|
| owner, env, tier: the defaults of `spec.tagKeys` | `hs/owner`, `hs/env`, `hs/tier` | `hs-owner`, `hs-env`, `hs-tier` |
| the managed marker: the default of `spec.autoImport.managedTag`, and the owner key `autoImport.requiredTags` defaults to | `hs/managed` | `hs-managed` |
| marks a subnet the operator created (value `subnet-operator`) | `hs/managed-by` | `hs-managed-by` |
| names the claim a subnet was created for | `hs/claim` = `<namespace>/<name>` | `hs-claim` = `<namespace>_<name>`, only with `spec.gcp.claimTag: Bind` |

**On AWS nothing changes.** The keys and values the operator writes and reads on AWS are those
of 1.x, and the defaulting webhook fills in the same `spec.tagKeys` and `autoImport.managedTag`
as before, so tags already on your VPCs and subnets keep their meaning and subnets created by a
claim are still adopted by their `hs/claim` tag.

**What to check** is anything that takes the operator's keys to be the same everywhere:

- tag policies, compliance rules, dashboards or scripts that look for `hs/owner` or `hs/claim`
  on every `Subnet` object or in every cloud: on GCP they are `hs-owner` and `hs-claim`, and
  `hs-claim` is only there when the scope sets `spec.gcp.claimTag: Bind`: by default the claim
  a GCP subnetwork belongs to is recorded in the claim's `status.allocations[].subnetID`, not
  in the cloud;
- a `NetworkScope` copied from an AWS one to make a GCP one: its `tagKeys`,
  `requiredSubnetTags`, `networkSelector.matchTags` and auto-import keys have to name the GCP
  keys. On GCP they are the short names of Resource Manager tag keys that exist under
  `spec.gcp.tagParent` (a tag from any other parent is reported as `<parent>/<key>`), and only
  tags bound to a network or subnet directly count, not inherited ones;
- Go code that imports `api/v1`: the constants `DefaultOwnerTagKey`, `DefaultEnvTagKey`,
  `DefaultTierTagKey`, `DefaultManagedTag`, `TagManagedBy` and `TagClaim` are gone (the Go
  packages are not part of the [API promise](../api-compatibility.md#what-the-promise-covers));
  `OperatorTagKeysFor(provider)` returns the keys of a provider, and `DefaultManagedValue` and
  `TagManagedByValue` stay.

### Claims and imports on GCP

A `SubnetClaim` on a GCP scope is for one regional subnetwork: it lists no `zones`, names the
ranges it is carved from in `spec.gcp.poolCIDRs` (a VPC network has no address space of its
own), and its allocation is keyed by the subnetwork's name, `namePrefix` (default: the claim's
name), which must be a valid Compute name. A `Create` claim creates the subnetwork with its tags
bound at creation. The claim tag `hs-claim` = `<namespace>_<name>` (where that is longer than 63
characters, its first characters followed by `-` and 10 hex digits of its SHA-256) is optional:
`spec.gcp.claimTag` is `Skip` by default, so a subnetwork carries only the ownership tags
(`hs-owner`, `hs-env`, `hs-tier`, `hs-managed-by` and the claim's `spec.tags`) and needs no tag
value per claim; the claim finds it again by name if it loses its status. `Bind` adds
`hs-claim`, which needs the key and a value per claim, created beforehand or by the operator with
`createTagValues`, and the operator never deletes one (a key holds at most 1,000). The field is
new in 2.0 and additive, in `v1` and `v1beta1` alike. There is no `Name` tag on GCP. The ranges
already taken are those of the network's subnetworks in the
scope's regions and other claims' reservations; a subnetwork in a region the scope does not
cover is not known, and a claim whose range clashes with one keeps failing with the conflict, so
cover every region the network has subnetworks in, or name a pool they do not use.

A `ResourceImport` binds tags to a network (`spec.region` may be empty) or a subnetwork, and only
ever adds: a key the resource already carries with another value is refused, by the webhook and
by the controller (`TagValueConflict`), because on GCP the old binding would have to be deleted
first; on AWS the value is replaced, as before.

Tag keys and values are Resource Manager resources that must exist under `spec.gcp.tagParent`
before they are bound. The operator never creates keys (`TagKeyMissing`). It creates a missing
value only when the scope sets `spec.gcp.createTagValues: true`; otherwise the claim or import
fails with `TagValueMissing`, naming the value to create. A key holds at most 1,000 values; past
that, creating one fails with `TagValueLimitReached`. What the write identity needs is in the
[IAM reference](../reference/iam.md#google-cloud) (and the reasoning in the
[ADR's implementation notes for #47](../adr/0002-multi-cloud-model.md#implementation-notes-47-gcp-writes-20));
setting the provider up from scratch is in the [GCP guide](../gcp.md).

Two GCP scopes over different regions of the same project are accepted with a warning: networks
are global, so both report them.

### Names and labels of GCP objects

A GCP resource name (`projects/<project>/regions/<region>/subnetworks/<name>`) is not a valid
object name, and a subnetwork's name repeats across projects and regions. `Network` and `Subnet`
objects of GCP are named `<name>-<first 10 hex digits of the SHA-256 of spec.id>`, and the
`network.hypersurgery.dev/network` label on a subnet, like `/resource` on an import, carries that
object name. AWS objects keep their VPC and subnet IDs as names and label values, as before. A
GCP network is global: its object has `spec.region` empty and an empty
`network.hypersurgery.dev/region` label, and its subnet count and IP totals add up the subnets of
every region the scope covers.

### Change events: GCP, and new metrics for every provider

With GCP enabled, change events are optional and off by default: `providers.gcp.events.subscription`
(`--gcp-events-subscription`) names a Pub/Sub subscription a log sink fills with the audit logs
of network changes ([deploy/gcp/events.md](../../deploy/gcp/events.md)). Nothing changes for a
release that does not set it.

For every provider with an event source, including an AWS queue set up under 1.x, the operator
now exports `hs_change_events_total{provider,result}` and `hs_change_event_errors_total{provider}`,
and `prometheusRule.enabled` adds the alert
[`SubnetInventoryChangeEventsFailing`](runbook.md#subnetinventorychangeeventsfailing), which fires
when the queue or subscription cannot be read for a quarter of an hour. Under 1.x that showed
only in the log. Route it like the other `warning` alerts of the chart.

### Overlaps on GCP, and one alert more

`NetworkCIDROverlap` covers GCP networks: a VPC network has no range of its own, so the operator
compares its subnetworks' ranges (primary, secondary, IPv6) with those of every other network of
the scope, and `status.overlapsWith` and `hs_network_cidr_overlaps` report them as they report
VPCs. `status.gcp.overlaps` says which ranges overlap and whether the two networks are peered.
The new metric `hs_network_peered_cidr_overlaps` counts the overlapping networks that are also
peered, and `prometheusRule.enabled` adds the alert
[`NetworkPeeredCIDROverlap`](runbook.md#networkpeeredcidroverlap) on it (`warning` in 2.0.0,
`critical` from 2.0.1): an overlap with a peered network is one that already breaks the peering. Route it before
`NetworkCIDROverlap`, or inhibit that one while this one fires for the same `network_id`.

**On AWS nothing changes.** The VPCs' CIDR blocks are compared as in 1.x, `hs_network_cidr_overlaps`
keeps its name, labels and values, and the new metric is not exported for AWS networks (the
operator does not read VPC peerings), so the new alert never fires for them.

A scope that discovers many projects' `default` networks sees them all overlap each other (every
auto mode network has the same ranges): keep them out with `networkSelector`, or silence
`NetworkCIDROverlap` by `name="default"`.

### Rolling back to 1.x

Delete the GCP `NetworkScope`s first: a 1.x operator does not know the provider and reports them
`ProviderNotEnabled`, and the objects they mirrored stay until the scope is deleted. The 2.0 CRDs
can stay installed; 1.x ignores the fields it does not know. Then roll back the release as
[below](#rollback).

## Upgrading from 2.x to 3.0

3.0 adds the Azure provider ([ADR 0002](../adr/0002-multi-cloud-model.md), milestone
"3.0 — Azure"). It has **no breaking change**:
the `hs-*` tag keys Azure uses were announced with 2.0 (the
[tag key change](#the-tag-keys-the-operator-writes-depend-on-the-provider)), and nothing changes
for AWS or GCP scopes. It is a major release all the same, because each new provider ships as
one ([policy](../policy.md#major-releases-and-new-providers)): a major release adds a provider
and may carry the breaking changes that provider needs, and Azure needed none.
[What a 2.x installation has to do](#what-a-2x-installation-has-to-do) is short.

The provider has been exercised against in-repository fakes of Azure Resource Manager,
Microsoft Entra ID and Queue Storage, and **not yet against real subscriptions** (#56);
[what is not done yet](#what-is-not-done-yet) says what that leaves open.

The API stays `network.hypersurgery.dev/v1` and only gains fields, in
`v1` and `v1beta1` alike, so the two versions still convert without loss:

- `spec.provider: Azure` on a `NetworkScope`, with an optional `spec.azure` (refused with any
  other provider, like `aws` and `gcp`) whose `resourceGroups` limits discovery to those resource
  groups. An Azure account `id` is a subscription ID, a UUID in either case (the operator uses it
  in lowercase), and `regions` are Azure locations as ARM spells them (`westeurope`,
  `germanywestcentral`);
- `accounts[].azure` on a `NetworkScope` (`clientID`, `writeClientID`, `tenantID`: Microsoft
  Entra client and tenant IDs, UUIDs in either case), refused with any other provider; see
  [identities per subscription](#identities-per-subscription-and-tenant);
- `status.azure` on `Network` (`resourceGroup`, `tagCount`, `subnetOwnershipEntries`) and on
  `Subnet` (`resourceGroup`, `delegations`, `serviceEndpoints`, the associated route table, network
  security group and NAT gateway, the number of `ipConfigurations`, `serviceManaged` and
  `ipUsageSource`).

It also adds the metric `hs_network_tags` (Azure networks only: the tags a virtual network
carries, of Azure's 50 per resource) and the alert `NetworkTagBudgetLow` with its threshold
`prometheusRule.thresholds.networkTags` (45), and the condition reasons `TagBudgetExceeded` and
`OwnershipEntryTooLong` on claims and imports, and `ResourceNotFound` and `RegionMismatch` on
imports. None of them changes anything for AWS or GCP.

### What a 2.x installation has to do

The usual [two steps](#upgrading), and nothing else: apply the 3.0 CRDs, then upgrade the
release with the values it has. Checked against the [list of what counts as
breaking](../policy.md#what-counts-as-breaking), from 2.0.1 to 3.0.0:

- **CRDs.** The same six kinds and the same two versions. They gain the optional Azure fields
  above and reworded descriptions; no field is removed, renamed, made required or validated more
  tightly, and no default changes. Nothing is migrated and no stored object is rewritten.
- **Chart values.** None is removed or renamed and no default changes. The new ones
  (`providers.azure.*`, `networkScope.azure`, `prometheusRule.thresholds.networkTags`) default
  to off, empty or, for the threshold, 45. With the values of a 2.x release the chart renders
  the same Deployment, arguments, RBAC, webhooks and NetworkPolicies as 2.0.1, with two
  additions for those who enabled them: `prometheusRule.enabled` gains one rule and
  `grafanaDashboard.enabled` one panel (both below).
- **RBAC.** The operator's ClusterRole and Roles are unchanged; there is no new rule to grant.
- **Cloud permissions.** No new IAM permission on AWS and no new role or permission on GCP;
  the policies in `deploy/iam` and the roles in `deploy/gcp` are those of 2.0.1.
- **Flags and environment variables.** None is removed or renamed. New, all for Azure:
  `--azure-cloud`, `--azure-authority-host`, `--azure-events-queue-url` and
  `--azure-events-debounce`, with `SUBNET_OPERATOR_AZURE_CLOUD`, `_AUTHORITY_HOST`,
  `_EVENTS_QUEUE_URL` and `_EVENTS_DEBOUNCE`; `--providers` accepts `azure`.
- **Metrics.** No metric or label is removed or renamed, and none changes what it measures.
  `hs_network_tags` is new and exported for Azure networks only; the existing metrics carry
  `provider="azure"` once there is an Azure scope.
- **Alerts.** One new rule, `NetworkTagBudgetLow` (`warning`). It is built on `hs_network_tags`,
  so it cannot fire without an Azure scope. No other rule changed, and
  `NetworkPeeredCIDROverlap` stays `critical`, as since 2.0.1.
- **Webhooks.** Claims and imports of AWS and GCP scopes are validated and defaulted as in
  2.0.1; IDs are rewritten to lowercase only in an Azure scope.

So an installation that does not enable Azure runs 3.0 as it ran 2.0.1. One that upgrades from
2.0.0 also takes what 2.0.1 changed (the row above: `NetworkPeeredCIDROverlap` is `critical`,
and the chart-signed webhook certificate is renewed).

### Enabling Azure

The Azure provider only runs when it is enabled (`providers.azure.enabled`, `--providers=azure`
next to the others); the default stays AWS alone, and the upgrade itself is the usual
[two steps](#upgrading). The chart gains `providers.azure.cloud`, `authorityHost`, `tenantId`,
`workloadIdentity`, `apiCIDRs` and `events` (below); a release that set up Workload ID by hand through
`podLabels`, `serviceAccount.annotations` or `extraEnv` keeps working, and can move to
`providers.azure.workloadIdentity` (hand-set service account annotations still win).

`networkScope.create` can create an Azure scope with the release (#57):
`networkScope.provider: Azure`, with `networkScope.azure.resourceGroups` for `spec.azure` and
`azure: {clientID, writeClientID, tenantID}` on the accounts
([`examples/values-azure.yaml`](../../examples/values-azure.yaml)). The default
`networkSelector.matchTags` and `requiredSubnetTags` are rendered as the `hs-*` keys there, as on
GCP. Nothing changes for a release whose values name `AWS` or `GCP`, or no provider: the scope
renders as it did. Three refusals of the chart are reworded or new, and matter only to values
that were already wrong: an unknown `networkScope.provider` now names Azure among the choices, an
account with an `azure` member in a scope of another provider fails the render, and so does
`networkScope.azure` there. Setting the provider up from scratch is in the
[Azure guide](../azure.md).

The Grafana dashboard the chart ships (`grafanaDashboard.enabled`) gains one panel, "Azure
virtual networks: tags used of 50", which is empty without an Azure scope, and its panel
descriptions name Azure's terms; no panel was removed or renamed, and its `uid` is the same. The
dashboard app (`dashboard.enabled`, or through `kubectl proxy`) shows Azure scopes with their
subscriptions, locations and virtual networks, offers imports with the `hs-*` keys, and adds a
tile for virtual networks at 45 or more of their 50 tags when there is an Azure network;
`?provider=azure` shows Azure alone. It needs nothing new in RBAC.

### What an Azure scope does in this release

- **Discovery.** It reads the virtual networks of each subscription in the scope's
  locations and their subnets through Azure Resource Manager, as the operator's own Azure
  identity (the SDK's `DefaultAzureCredential`, on Kubernetes Microsoft Entra Workload ID) or an
  identity of the subscription's own (below), which needs the reader role of
  `deploy/azure/reader-role.json` (or Reader) on each subscription.
- **Change events (#55).** Optional and off by default: `providers.azure.events.queueUrl`
  (`--azure-events-queue-url`, under OLM `SUBNET_OPERATOR_AZURE_EVENTS_QUEUE_URL` and
  `SUBNET_OPERATOR_AZURE_EVENTS_DEBOUNCE`) names a Storage
  queue that the subscriptions' Event Grid system topics fill with the write and delete events of
  virtual networks ([deploy/azure/events.md](../../deploy/azure/events.md)). The operator polls
  it as its own identity, which needs the Storage Queue Data Message Processor role on the queue;
  the chart refuses a URL with a SAS token. A changed subscription and location is then resynced
  within seconds, and `status.capabilities` lists `ChangeEvents`. The dependency
  `github.com/Azure/azure-sdk-for-go/sdk/storage/azqueue` is new. Nothing changes without the
  value.
- **Claims (#53).** With `--enable-writes`, a `mode: Create` claim creates one subnet named
  `namePrefix` (no zones: an Azure subnet is regional) in the virtual network's address space, as
  on AWS, after writing its ownership entry on the virtual network, `hs-claim` included, so a
  claim that lost its status finds its subnet again. `spec.networkID` is the virtual network's ID
  in lowercase, as `kubectl get hsnet` shows it; `spec.account` may be in either case. A
  `namePrefix` Azure reserves (`GatewaySubnet`, `AzureBastionSubnet`, …) is refused. The operator
  never changes an existing subnet: a name that is taken fails the claim with "exists already".
- **Imports and auto-import (#53).** An import of a virtual network adds its own tags; an import
  of a subnet adds pairs to its entry. Both go through the Tags API's `Merge` operation and only
  ever add: a name that already has another value refuses the whole import
  (`TagValueConflict`), and for a subnet that includes what it inherits from its network, so an
  import cannot take over an owner the network gives its subnets. The auto-import policy works
  in `DryRun` and `Apply`. Imports need `Microsoft.Resources/tags/read` and `/write` only (Tag
  Contributor); creating subnets needs the writer role in `deploy/azure/writer-role.json`.
- **An import reads its virtual network first (#116, #117).** Before it writes, and in a dry
  run, the operator reads the virtual network once as the account's read identity. A virtual
  network that does not exist, or one without the subnet the import names, fails the import
  with `ResourceNotFound` and nothing is written (the Tags API would otherwise leave the entry
  of a misspelt subnet on the network for good); it is retried every minute, so a subnet created
  later is imported. The same read gives the location: `spec.region` of an Azure import is its
  virtual network's location, the mutating webhook fills it in for a virtual network that is in
  the inventory, and the validating webhook requires it otherwise and refuses a wrong one. An
  import whose region is not where Azure has the virtual network fails with `RegionMismatch`.
  On AWS `spec.region` stays required and typed, and on GCP it stays what the subnetwork's ID
  says; neither reads the resource first, since their tag writes name the resource itself.
- **The tag budget.** A virtual network carries at most 50 tags, and its subnets' entries share
  them with its own. An entry holds only what differs from its network, and a claim or import
  that would need a 51st tag fails with `TagBudgetExceeded` before anything is written (a
  claim's subnet is not created either). An entry holds at most 256 characters
  (`OwnershipEntryTooLong`; the webhook refuses a claim or subnet import whose tags cannot fit).
  `status.azure.tagCount`, `hs_network_tags` and `NetworkTagBudgetLow` show a network running out.
  None of this is a breaking change: it applies to Azure scopes only, which are new in 3.0.
- <a id="identities-per-subscription-and-tenant"></a>**Identities per subscription and
  tenant.** By default one operator identity with role assignments in every subscription reads
  them all. An account entry may instead name identities of its own: `azure.clientID` reads,
  `azure.writeClientID` creates subnets and writes tags, `azure.tenantID` is their tenant when it is not the
  operator's. Azure has no AssumeRole: the operator exchanges its one service account token for
  a token of each, so every such identity needs a federated identity credential for the
  operator's service account (issuer, subject `system:serviceaccount:<namespace>:<name>`,
  audience `api://AzureADTokenExchange`), and the operator needs Workload ID set up
  (`providers.azure.workloadIdentity`; without the AKS webhook, `webhook: false` and
  `providers.azure.tenantId`). A token Entra refuses fails the target (`SyncFailed`, the error
  naming the client ID and what the federated credential must say); the operator never reads or
  writes as itself instead. Another tenant's subscription is reached through a multitenant app
  registration provisioned there. [`deploy/azure`](../../deploy/azure/README.md) has the roles
  and the setup.
- **No creator attribution (an accepted limitation of 3.0).** An Event Grid resource event does
  not say whether a write created the resource or updated it, so the operator records no creator
  on Azure, with or without change events. In an Azure scope `spec.autoImport.fromCreator`
  rules, and `skip` rules with a `principalPrefix`, therefore never match: a resource such a
  rule was meant for falls through to `inheritFromNetwork` and `accountDefaults`, or stays
  unmanaged with `UnmanagedNetworkResource` and a `no_owner` decision, and one a `skip` rule by
  principal was meant to keep the policy away from is not skipped by it. The scope is accepted,
  and the webhook warns when it is applied
  (`spec.autoImport.fromCreator: the N creator rule(s) never match on Azure: …`); a scope
  applied while the webhooks were off gets no such notice, and the scope's status does not
  repeat it. Keep the policy away from Terraform's resources on Azure with a `skip` rule by tag.
- **Change events for some resource groups only.** `deploy/azure/events/topic.bicep` takes
  `resourceGroupNames` (and `eventSubscriptionName`): when set, the event subscription delivers
  the virtual network events of those resource groups only. A scope that names
  `spec.azure.resourceGroups` in a busy subscription should set it to the same groups;
  otherwise every virtual network change elsewhere in the subscription reaches the queue and
  resyncs every location of the subscription
  ([deploy/azure/events.md](../../deploy/azure/events.md#one-scopes-resource-groups-only)). The
  default, every resource group, is what the template did before.
- **Sovereign clouds.** `providers.azure.cloud` (`--azure-cloud`: `AzurePublic`,
  `AzureUSGovernment`, `AzureChina`) selects the Resource Manager and Entra endpoints;
  `authorityHost` replaces the latter alone. Under OLM, which passes no arguments, they are
  `SUBNET_OPERATOR_AZURE_CLOUD` and `SUBNET_OPERATOR_AZURE_AUTHORITY_HOST`
  ([docs/olm.md](../olm.md#settings-through-the-subscription)); a flag wins over its variable.
- **Resource groups.** Without `spec.azure.resourceGroups` a scope discovers the whole
  subscription with one List All call per subscription and location (per subscription and
  sync from 3.2). With it, discovery lists
  the virtual networks of those groups only, one call per group, and the identity then needs
  the read permission on those resource groups alone rather than on the subscription (Reader
  assigned at resource group scope is enough). Names are compared without regard to case, as
  ARM does, and a group listed twice in two spellings is refused. A group that does not exist
  fails the target (`ResourceGroupNotFound` in `status.targets[].error`), like a subscription
  that does not; the inventory it had stays until the scope is corrected.
- **Subnet ownership lives on the virtual network.** Azure subnets cannot carry tags. A
  virtual network carries one tag per subnet that has ownership of its own,
  `hs-subnet-<subnet name>`, whose value holds the subnet's tags as `key=value` pairs separated
  by `;` (for example `hs-owner=payments;hs-tier=db`); its keys override the network's tags for
  that subnet, and `status.ownershipSource` is `Subnet`. A subnet without one inherits its
  network's tags (`hs-owner`, `hs-env`, ...) and reports `Network`. The entries are left out of
  the network's own `status.tags`, and a tag whose name starts with `hs-subnet-` is refused in a
  claim's or import's `tags`.
- **Tag names are case-insensitive, values are not.** Azure treats `HS-Owner` and `hs-owner` as
  one tag name, and so does the operator on Azure: the scope's `tagKeys`, `networkSelector`,
  `requiredSubnetTags` and auto-import keys, the operator's own keys, the `hs-subnet-` entries
  and the names inside them all match whatever case the tag was set in, and a matched tag is
  reported in `status.tags` in the scope's spelling (other tags keep Azure's). Values are
  compared exactly, as Azure keeps them case-sensitive. A scope that spells one tag name two
  ways (`tagKeys.owner: Team` and `requiredSubnetTags: [team]`) is refused. What the operator
  writes uses its own keys as they are, lowercase for the defaults, and a name the network
  already carries in another case keeps that spelling. AWS and GCP keep comparing tag names
  exactly.
- **Subscription IDs in any case.** `accounts[].id` and `autoImport.accountDefaults[].account`
  may spell a subscription ID in capitals, as the portal sometimes shows it. The spec is left as
  written; the operator uses the ID in lowercase in `spec.account` of `Network` and `Subnet`, the
  `account` label and metric label, and `status.targets`. Two entries that differ only in case
  are refused, and so is a second scope over the same subscription in another spelling.
- **Names and IDs.** ARM resource IDs are case-insensitive: `spec.id` of an Azure `Network` or
  `Subnet` is its resource ID in lowercase, and the objects are named like GCP's,
  `<name>-<first 10 hex digits of the SHA-256 of spec.id>`. The resource group, as Azure spells it,
  is in `status.azure.resourceGroup`.
- **Resource IDs in any case in claims and imports.** A `SubnetClaim`'s `networkID` and a
  `ResourceImport`'s `resourceID` may be written as the portal shows them
  (`/subscriptions/<ID>/resourceGroups/My-RG/providers/Microsoft.Network/virtualNetworks/vnet-1`).
  The mutating webhook
  rewrites `networkID`/`resourceID` and `spec.account` to lowercase, so `kubectl get` shows the ID
  the operator uses, and an import's `account` and `resource` labels are in that spelling too,
  whatever case the subscription ID was typed in. An object created while the webhooks were off
  keeps its spec as written and the controllers read it in lowercase; another spelling of the
  same ID is not a change of the immutable field. A GitOps tool that compares the stored object
  with the manifest field by field (Argo CD without server-side diff) reports a claim or import
  written in another case as out of sync: write the ID in lowercase there. AWS and GCP IDs stay
  case-sensitive and are never rewritten.
- **Addresses.** `totalIPs` counts every IPv4 address prefix of a subnet less the 5 addresses
  Azure reserves in each; a subnet's further prefixes are in `secondaryCIDRBlocks` and, unlike a
  GCP secondary range, are counted. `availableIPs` is the virtual network's usage for the subnet
  (limit less current value); a gateway subnet, for which Azure reports none, stays unknown.
  `status.azure.ipUsageSource` says where the number came from, and `serviceManaged` marks a
  delegated subnet whose services may use addresses Azure does not count as IP configurations.

### What is not done yet

- **A run against real subscriptions (#56).** Everything above is tested against in-repository
  fakes of Azure Resource Manager, Microsoft Entra ID and the Storage queue API, through the real
  Azure SDK clients. The error codes the operator recognises, the fields of the events, and that
  the roles in `deploy/azure` are enough, are from Microsoft's references and still to be
  confirmed; the [Azure guide](../azure.md), the [IAM reference](../reference/iam.md#azure) and
  [deploy/azure/events.md](../../deploy/azure/events.md) say where. In practice: start an Azure
  scope read-only, look at what `status.targets` reports before enabling writes, and expect
  that a role may need an action the files do not list yet. A report of what you find is what
  closes #56.
- **Virtual network peerings are not read**, so an overlap between peered virtual networks is
  reported by `NetworkCIDROverlap` like any other and `NetworkPeeredCIDROverlap` never fires for
  Azure.
- **No creator attribution**, as [above](#what-an-azure-scope-does-in-this-release):
  `autoImport.fromCreator` rules and `skip` rules by `principalPrefix` never match in an Azure
  scope.
- **Ownership entries are never removed (#121).** The operator never deletes a tag, so the
  `hs-subnet-<name>` entry of a subnet that has been deleted since stays on its virtual network
  and keeps counting against the 50 tags, and there is no tooling yet that lists such entries:
  compare the network's tags with its subnets and remove the leftovers yourself when
  `NetworkTagBudgetLow` fires ([runbook](runbook.md#networktagbudgetlow)).
- **One listing per location, not per subscription (#118).** Resource Manager has no location
  filter, so every location of a subscription lists all its virtual networks on each sync. A
  subscription in many locations spends that many listings of its read limit; the operator
  slows down before it is throttled ([limits](limits.md)). Done in 3.2: one listing per
  subscription and sync.
- **A location without virtual networks is not reported (#119).** Only the shape of a location
  name is checked, so a misspelt one (`westeuropa`) syncs cleanly with no networks. Done in
  3.2: a location the subscription does not have fails its target.
- **The operator's own credential errors come as the SDK words them (#120).** A token refused
  for an identity of a subscription's own names the federated credential it needs; one refused
  for the operator's own identity does not yet.

### Rolling back to 2.x

Delete the Azure `NetworkScope`s first: a 2.x operator does not know the provider and reports
them `ProviderNotEnabled`, and the objects they mirrored stay until the scope is deleted. The 3.0
CRDs can stay installed; 2.x ignores the fields it does not know. Nothing the operator wrote on
Azure is removed by a rollback: subnets a claim created stay, and so do the tags on the virtual
networks, the `hs-subnet-<name>` entries included. Then roll back the release as
[below](#rollback).

## Rollback

```sh
helm history subnet-operator -n subnet-operator-system
helm rollback subnet-operator <revision> -n subnet-operator-system
```

What that does and does not do:

- **Leave the CRDs alone.** Helm does not roll them back, and you should not either. A newer
  CRD under an older operator is harmless: the older binary does not watch the kind, so its
  objects simply stop being reconciled and their status freezes at the last value.
- **Do not `kubectl apply` an older CRD to "match" the rollback.** Kubernetes prunes fields
  that are not in the schema, so applying the 0.2.0 `networkscopes` CRD would silently delete
  `spec.autoImport` and `spec.discoverUnmanaged` from every stored scope. If you have already
  done it, re-apply the newer CRD and restore the fields from your values or git.
- **AWS keeps whatever was done.** Tags applied by an import stay applied; subnets created by
  a claim stay. Rolling back the operator is not an undo for the cloud, and there is no code
  path that would make it one.
- **Objects of a kind the older version does not know keep existing.** Rolling back to 0.2.x
  leaves `ResourceImport` objects in the cluster, unreconciled. Delete them if the clutter
  bothers you — deleting a `ResourceImport` does not remove any tag.
- **Pin the image if you roll back only the chart.** `image.tag` defaults to the chart's
  `appVersion`, so a chart rollback also rolls the image back unless your values pin
  `image.tag`.

After a rollback, check the same things as after an upgrade.

## Verifying an upgrade

The upgrade from the latest published release to every change is tested in CI before it is
merged, and the upgrade from 0.9 on every push to `master` (`make test-upgrade`; what it checks
is in the [support policy](../policy.md#the-upgrade-test)).
On your own cluster:

```sh
kubectl -n subnet-operator-system rollout status deployment/subnet-operator
kubectl get crds | grep hypersurgery                     # the kinds of the target version (and aws.hypersurgery until you delete it)
kubectl get crd subnetclaims.network.hypersurgery.dev -o jsonpath='{.status.storedVersions}'   # from 1.0: ["v1"]
kubectl get nscope                                       # Ready=True, "Last sync" within a resync interval
kubectl get nscope <scope> -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}'
```

Then wait one `resyncInterval` and confirm `hs_scope_last_sync_timestamp_seconds` is
moving and no target has flipped to `hs_target_up == 0` (before 0.9, `hs_aws_scope_last_sync_timestamp_seconds`
and `hs_aws_target_up`).
