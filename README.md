# subnet-operator

[![CI](https://github.com/aivandrago/subnet-operator/actions/workflows/ci.yml/badge.svg)](https://github.com/aivandrago/subnet-operator/actions/workflows/ci.yml)
[![Security](https://github.com/aivandrago/subnet-operator/actions/workflows/security.yml/badge.svg)](https://github.com/aivandrago/subnet-operator/actions/workflows/security.yml)
[![CodeQL](https://github.com/aivandrago/subnet-operator/actions/workflows/codeql.yml/badge.svg)](https://github.com/aivandrago/subnet-operator/actions/workflows/codeql.yml)
[![Coverage](https://img.shields.io/endpoint?url=https%3A%2F%2Fhypersurgery.dev%2Fbadges%2Fcoverage.json)](https://github.com/aivandrago/subnet-operator/actions/workflows/ci.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/aivandrago/subnet-operator/badge)](https://scorecard.dev/viewer/?uri=github.com/aivandrago/subnet-operator)
[![Go](https://img.shields.io/github/go-mod/go-version/aivandrago/subnet-operator)](go.mod)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue)](LICENSE)

A Kubernetes operator, running in EKS, that manages cloud networks and subnets based on
resource tags: AWS VPCs and subnets today, GCP and Azure next.
It replaces hand-maintained VPC/subnet inventories (the usual shared spreadsheet)
with an automatically discovered, always-current inventory and on-demand CIDR allocation.

Scope: AWS first. GCP (GKE) and Azure providers follow, behind a common provider interface.

Project page: https://hypersurgery.dev · install guide: https://hypersurgery.dev/docs/ ·
charts: https://charts.hypersurgery.dev · demo dashboard (installable):
https://hypersurgery.dev/dashboard/
(sources in [`site/`](site)).

## Status

**Alpha.** The latest release is v0.8.0. Read-only discovery is the default
and the part that has had the most use: the operator finds VPCs and subnets across accounts and
regions, mirrors them as `Network` and `Subnet` objects, reports compliance findings, exports them to a Google Sheet and publishes
Prometheus metrics, calling only EC2 `Describe*` APIs.

Everything that writes is opt-in and gated behind `--enable-writes` and a separate write role:
`SubnetClaim` allocates CIDRs and can create subnets, `ResourceImport` and the auto-import
policy bring untagged resources under management by tagging them. The operator never deletes a
cloud resource.

Invalid objects are refused at `kubectl apply` by admission webhooks; every import, allocation
and policy decision leaves a Kubernetes Event and a line in a JSON audit stream; the chart runs
two replicas; and the image is public, multi-arch and signed with cosign.

Tested against [Moto](https://github.com/getmoto/moto) in Kind, not yet against a real AWS
organization — that conformance run is the open item in the current milestone. Every change is
also tested as an upgrade from the latest release.

New in 0.8: the API is the cloud-neutral group `network.hypersurgery.dev/v1beta1`, where a
`NetworkScope` names its provider and a `VPC` is a `Network`, and the project and its chart are
called `subnet-operator` (until 0.7: `aws-subnet-operator`). The operator migrates every
`aws.hypersurgery/v1alpha1` object itself, status included; that group is deprecated and
removed in 0.9. Upgrading from 0.7: [the upgrade guide](docs/operations/upgrades.md#upgrading-from-07-to-08).
The design is [ADR 0002](docs/adr/0002-multi-cloud-model.md).

Versioning, what counts as a breaking change, how long deprecated things live, the supported
Kubernetes versions (1.34 to 1.37, oldest and newest tested in CI) and how security fixes ship:
[docs/policy.md](docs/policy.md).

## Getting started

### 1. IAM

- **Hub account** (where EKS runs): create a role for the operator's service account
  (with the chart, `<namespace>/<release name>`, e.g. `subnet-operator-system/subnet-operator`) with
  [EKS Pod Identity](https://docs.aws.amazon.com/eks/latest/userguide/pod-identities.html) or IRSA,
  and attach [`deploy/iam/operator-policy.json`](deploy/iam/operator-policy.json).
- **Spoke accounts**: deploy [`deploy/iam/spoke-readonly-role.cfn.yaml`](deploy/iam/spoke-readonly-role.cfn.yaml)
  to every account with a service-managed StackSet, passing the hub role ARN as `OperatorRoleArn`.

For IRSA, annotate the service account with `eks.amazonaws.com/role-arn`; Pod Identity needs no annotation.

### 2. Change events (optional, recommended)

Without events, changes show up at the next resync (`resyncInterval`, 10 minutes by default).
With events, a changed account/region is resynced about 10 seconds after the API call.

```
spoke account, every region                      hub account
CloudTrail ─► EventBridge rule ──PutEvents──►  default bus ─► rule ─► SQS ─► operator
```

1. Make sure CloudTrail records management (write) events everywhere, e.g. with an organization trail.
2. Hub account: deploy [`deploy/events/hub-events.cfn.yaml`](deploy/events/hub-events.cfn.yaml)
   in the operator's region. Pass `OrganizationId`.
3. Spoke accounts: deploy [`deploy/events/spoke-events.cfn.yaml`](deploy/events/spoke-events.cfn.yaml)
   with a StackSet to every account and region in the scope, passing the hub `EventBusArn` output.
4. Start the operator with `--events-queue-url=<QueueUrl output>` (or `EVENTS_QUEUE_URL`).
   The hub policy already allows the queue (`deploy/iam/operator-policy.json`).

The operator only resyncs the account/region an event names. It ignores failed calls and tags on
non-network resources, and it collects events for `--events-debounce` (10s) so a burst of calls
causes one resync. Free-IP counts change with every ENI and are not event-driven: they are
refreshed by the periodic full resync, which also covers lost events.

### 3. Install

With Helm, from the chart repository (values documented in the chart's
[README](charts/subnet-operator/README.md)):

```sh
helm repo add hypersurgery https://charts.hypersurgery.dev
helm install subnet-operator hypersurgery/subnet-operator \
  -n subnet-operator-system --create-namespace \
  -f examples/values-minimal.yaml
```

Until 0.7 the chart was called `aws-subnet-operator`; an existing release is upgraded in place to
the new chart, see [the upgrade guide](docs/operations/upgrades.md#upgrading-from-07-to-08).

`make helm-publish` packages the chart and pushes it to that repository.

Since v0.6.0 the chart is also published to ghcr.io as an OCI artifact, signed with cosign
keyless by the release workflow like the image, and the package is attached to the GitHub
release:

```sh
cosign verify ghcr.io/aivandrago/charts/subnet-operator:<version> \
  --certificate-identity-regexp '^https://github.com/aivandrago/subnet-operator/\.github/workflows/release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
helm install subnet-operator oci://ghcr.io/aivandrago/charts/subnet-operator \
  --version <version> \
  -n subnet-operator-system --create-namespace \
  -f examples/values-minimal.yaml
```

Releases up to 0.7 are at `oci://ghcr.io/aivandrago/charts/aws-subnet-operator`.

The identity pins the release workflow, not just the repository: another workflow of the
repository that holds an OIDC token (the Scorecard job does) cannot produce a signature that
passes this check.

Or with kustomize:

```sh
make docker-build docker-push IMG=<registry>/subnet-operator:<tag>
make deploy IMG=<registry>/subnet-operator:<tag>
kubectl apply -f examples/01-single-account.yaml   # edit accounts and regions first
```

Ready-made manifests and values live in [`examples/`](examples/).

### 4. Google Sheet export (optional)

For people who still want a table, the operator can mirror a scope's subnets into a Google Sheet.
The sheet is an output only: it is rewritten on every refresh and never read back, so the
spreadsheet stops being a thing anyone maintains by hand.

1. Create a Google Cloud service account, enable the Sheets API and download its JSON key.
2. Share the spreadsheet with the service account address (`...@...iam.gserviceaccount.com`) as an editor.
3. Store the key and create the export:

```sh
kubectl create secret generic google-sheets -n subnet-operator-system \
  --from-file=credentials.json=./service-account.json
kubectl apply -f config/samples/network_v1beta1_sheetexport.yaml   # edit spreadsheetID first
kubectl get sheetexports.network.hypersurgery.dev
```

Columns: provider, account, region, network (VPC) and its name, subnet and its name, CIDR, zone, public, tier, owner,
environment, total and free IPs, utilization, missing tags, route table and when that
account/region was last synced, plus one column per key in `extraTagColumns`.
The tab is created if missing, its header is frozen, and the range is protected with a warning
so nobody mistakes it for a document they can edit.

### 5. Use

```sh
kubectl get hypersurgery                      # every kind of the project
kubectl get nscope                            # NetworkScopes
kubectl get hsnet -o wide                     # Networks (AWS VPCs)
kubectl get hssubnet -o wide                  # Subnets
kubectl get hssubnet -l network.hypersurgery.dev/account=222222222222,network.hypersurgery.dev/region=eu-central-1
```

```
NAME              NETWORK    CIDR           ZONE            FREE IPS   USED %   OWNER
subnet-0a1b2c3d   vpc-0aaa   10.20.1.0/24   eu-central-1a   51         79       team-payments
```

`networks` and `subnets` are plain names other projects use too (OpenShift, Kube-OVN), so the
kinds have prefixed short names and the `hypersurgery` category. While the deprecated
`aws.hypersurgery` CRDs are still installed, `kubectl get subnets` means *their* subnets;
use the short names or the full resource name, e.g. `subnets.network.hypersurgery.dev`.

Users should have read-only RBAC on `networks` and `subnets`: the operator overwrites manual
edits on the next resync.

## Metrics

Served on the controller-runtime metrics endpoint (`config/prometheus` has a ServiceMonitor).
The names still carry `aws`; the neutral `hs_*` names with a `provider` label arrive next to
them in a later change (#44, ADR 0002 §7).

| Metric | Labels | Meaning |
|---|---|---|
| `hs_aws_subnet_available_ips` | scope, account, region, vpc_id, subnet_id, name, cidr, az, owner, env, tier, public | Free IPv4 addresses. Only for subnets with an IPv4 CIDR: an IPv6-only subnet has no IPv4 capacity to report |
| `hs_aws_subnet_total_ips` | same | Usable IPv4 addresses (CIDR size - 5); likewise only for subnets with an IPv4 CIDR |
| `hs_aws_subnet_missing_required_tags` | same | Required tags absent or empty |
| `hs_aws_subnet_claim_ready` | namespace, name, reason | 1 when a `SubnetClaim` is fulfilled, 0 while it is not; `reason` is its Ready condition's |
| `hs_aws_resource_import_ready` | namespace, name, state, reason | 1 when a `ResourceImport` has settled (applied, or a dry run), 0 while it is pending or failed |
| `hs_aws_vpc_cidr_overlaps` | scope, account, region, vpc_id, name, owner, env | VPCs in the scope with overlapping CIDRs |
| `hs_aws_target_up` | scope, account, region | 1 if the account/region is reachable: the last discovery succeeded or was only throttled |
| `hs_aws_target_sync_errors_total` | scope, account, region | Failed discoveries, not counting throttled ones |
| `hs_aws_target_throttled` | scope, account, region | 1 while the account/region is backed off because AWS throttled its last discovery |
| `hs_aws_api_throttled_total` | scope, account, region, operation | EC2 calls throttled, per attempt, including attempts a retry rode out |
| `hs_aws_scope_last_sync_timestamp_seconds` | scope | Last finished sync |
| `hs_aws_unmanaged_resources` | scope, account, region, kind | Resources without the managed tag, right now |
| `hs_aws_unmanaged_resources_total` | scope, account, region, kind | Unmanaged resources seen for the first time; alert on an increase |
| `hs_aws_auto_imports_total` | scope, account, region, result | Auto-import decisions: applied, dryrun, skipped, no_owner |
| `hs_migration_pending_objects` | kind | `aws.hypersurgery/v1alpha1` objects not yet migrated to `network.hypersurgery.dev`; 0.9 does not start while it is above zero |

### Dashboard and alerts

The same panels are browsable as a demo with synthetic data at
https://hypersurgery.dev/dashboard/, which also installs as an offline app.

The app shows your own cluster too, reading it with your own Kubernetes credentials — the API
server decides what you see, and an import is created in your name:

- **kubectl proxy**, nothing to install: `kubectl proxy --www=site --www-prefix=/ui/` from a
  checkout, then open `http://127.0.0.1:8001/ui/dashboard/?source=kubectl`.
- **In the cluster**: `dashboard.enabled=true` in the chart runs it as a second entrypoint of the
  operator's image. It has no permissions of its own and forwards each viewer's bearer token —
  from your SSO proxy, or pasted into the app — for reads of the `network.hypersurgery.dev` group
  and the creation of `ResourceImport`s, and nothing else. See the
  [chart README](charts/subnet-operator/README.md#the-dashboard-app).

The chart ships a Grafana dashboard
([`charts/subnet-operator/dashboards/subnet-inventory.json`](charts/subnet-operator/dashboards/subnet-inventory.json)):
headline counts, utilization by environment, free addresses by account, the fullest subnets,
subnets missing required tags, what is still unmanaged, what the auto-import policy decided,
account health and the age of the last full sync.
Set `grafanaDashboard.enabled=true` and the Grafana sidecar imports it; `prometheusRule.enabled=true`
adds alerts for nearly full subnets, unreachable accounts, accounts AWS keeps throttling, CIDR
overlaps and a stale inventory.
Every one of those alerts has a runbook entry — what it means, how to confirm it, what to do and
when to ignore it — in [`docs/operations/`](docs/operations/), next to upgrade and rollback notes,
the failure modes and the limits of one instance.

Example alerts:

```promql
# Subnet more than 80% used
1 - hs_aws_subnet_available_ips / hs_aws_subnet_total_ips > 0.8
# Account/region not reachable
hs_aws_target_up == 0
# Account/region reachable, but throttled by AWS and backed off
hs_aws_target_throttled == 1
# Overlapping VPC CIDRs
hs_aws_vpc_cidr_overlaps > 0
```

## Capacity

**One instance is sized for 100 accounts at up to 4 regions each (400 account/region targets)
with the defaults** — `discovery.concurrency` 4 and a 10-minute `resyncInterval`. At about 1.5 s
per target, a full sync of 400 targets takes about 150 s of discovery: a quarter of the
interval, which leaves the rest as headroom for the first sync, which writes every object, and
for accounts that EC2 throttles, whose calls are retried for about 17 s instead of 1.5 s.
`make test-scale` measures exactly that shape (1,400 VPCs, 5,205 subnets, fake EC2, real
kube-apiserver): 157 s for a full sync when nothing changed — one object written, the scope's
status — 199 s for the first sync, 315 s with a tenth of the accounts throttled, and up to
234 MiB resident, which is why the chart's memory limit is 512Mi.

EC2 rate limits are per account and per region, and discovery sends each one at most one
request at a time, 3 or 4 per sync — serving more accounts does not bring any single account
closer to its limit. The limit is wall clock, so it scales with concurrency: for 300 accounts ×
4 regions, set `discovery.concurrency` to 12. The cap is one for the whole instance, however
many `NetworkScope`s share it. An account that is throttled anyway (by whatever else calls EC2
there) is backed off and retried on its own schedule, stays in the inventory with its last
known state, and shows up as `hs_aws_target_throttled` rather than as unreachable.

The arithmetic, the measurements, the API cost per sync and the memory side are in
[docs/operations/limits.md](docs/operations/limits.md#accounts-and-regions-per-instance).

## Development

Requires Go (see `go.mod`) and make.

```sh
make test    # unit tests + envtest (real kube-apiserver and etcd, fake AWS)
make lint    # golangci-lint with kube-api-linter
make run     # run against the current kubeconfig with local AWS credentials
make test-e2e  # Kind cluster + Moto (AWS API emulator); needs Docker
make test-upgrade  # install the latest release, upgrade to this checkout, check nothing was lost
make test-scale  # 400 account/region targets against a fake EC2, about 15 minutes
```

The e2e suite builds the operator image, loads it into Kind, starts [Moto](https://github.com/getmoto/moto)
on the Kind network, seeds VPCs and subnets into a hub and a spoke account (reached with AssumeRole)
and checks the mirrored objects, resync deletion, failed targets, change events delivered
through SQS and metrics.
LocalStack is not used because its images now require a paid auth token.
`KIND_NODE_IMAGE` runs either suite on another Kubernetes version; CI runs the oldest and newest
supported ones (see [docs/policy.md](docs/policy.md#kubernetes-versions)).

To run the operator against any AWS-compatible endpoint, set `AWS_ENDPOINT_URL`.

Layout: `api/v1beta1` (the `network.hypersurgery.dev` CRDs), `api/v1alpha1` (the deprecated
`aws.hypersurgery` group, until 0.9), `internal/controller` (sync loops), `internal/migration`
(the move from the old group, in the cluster and for manifests), `internal/cloud/aws`
(EC2 discovery), `internal/events` (SQS change events), `internal/sheets` (Google Sheet export),
`internal/inventory` (cloud-neutral model), `internal/metrics`.
Packaging lives in `charts/subnet-operator` (`make helm-lint` renders it; `make helm-crds`
refreshes the chart's copy of the CRDs), AWS-side templates in `deploy/`, examples in `examples/`.

## Goals

1. **Inventory (read).** Discover every VPC and subnet across accounts and regions: CIDR, AZ,
   free IPs, utilization, owner, environment, tier. AWS tags are the source of truth.
2. **Compliance.** Report untagged or ownerless subnets, CIDR overlaps between VPCs
   (peering / Transit Gateway), subnets near exhaustion, and tag drift.
3. **Allocation (write).** A team requests a subnet (`SubnetClaim`); the operator picks a free
   CIDR from the right pool, creates the subnets and tags them.

## Contributing

Reports from a real AWS organization are the most useful thing right now: CI exercises the
operator against an emulator, not a real account. See [CONTRIBUTING.md](CONTRIBUTING.md) for
how to run the tests and what conventions to keep, [GOVERNANCE.md](GOVERNANCE.md) for how
decisions are made, [SECURITY.md](SECURITY.md) to report a vulnerability privately, and the
[threat model](docs/security/threat-model.md) for what the operator trusts and why.

## License

Apache-2.0, see [LICENSE](LICENSE). The project follows the
[CNCF Code of Conduct](CODE_OF_CONDUCT.md).

## Non-goals

- Deleting subnets automatically. Deletion is always manual (`deletionPolicy: Retain`).
- Modifying resources owned by Terraform (`hs/managed-by=terraform` is read-only).

## Architecture (draft)

- Go, kubebuilder / controller-runtime, aws-sdk-go-v2, Helm chart.
- **Hub and spoke.** One operator in a hub account reaches spoke accounts via
  EKS Pod Identity (or IRSA) → `sts:AssumeRole` into a role deployed to every account by
  CloudFormation StackSets. Read-only role first; the write role is separate and opt-in.
- **Change detection.** Periodic full resync plus near-real-time events:
  CloudTrail (`CreateSubnet`, `DeleteSubnet`, `CreateTags`, `DeleteTags`, ...) → EventBridge
  → SQS in the hub account → operator, which resyncs only the affected account/region.
- **Allocation backend.** Pluggable: Amazon VPC IPAM (preferred for multi-account, pools shared
  via RAM) or a built-in allocator over tagged VPC pools. See [ADR 0001](docs/adr/0001-allocation-backend.md).

### CRDs

| Kind | Scope | Purpose |
|---|---|---|
All in `network.hypersurgery.dev/v1beta1`:

| Kind | Scope | Purpose |
|---|---|---|
| `NetworkScope` | Cluster | Provider, accounts with their roles, regions, network selector, required tags, the namespaces that may use it, resync interval |
| `Network` | Cluster | Observed network (AWS VPC): CIDRs, owner, subnet count, free IPs, CIDR overlaps with other networks |
| `Subnet` | Cluster | Observed subnet: CIDR, zone, free IPs, utilization, tags, public/private, missing tags |
| `SheetExport` | Cluster | Optional read-only mirror of a scope in a Google Sheet |
| `SubnetClaim` | Namespaced | Desired subnets: prefix length, zones, tags, target network; reserved and optionally created |
| `ResourceImport` | Namespaced | Tags an existing network or subnet to take it under management |

`Network` and `Subnet` objects are named after their AWS IDs, labelled with
`network.hypersurgery.dev/{scope,provider,account,region,network}` and owned by their `NetworkScope`.
A subnet is public when its route table (explicit, or the VPC main table) has a default route
to an internet gateway.

### Outputs

- `kubectl get subnets -o wide`
- Prometheus metrics and a Grafana dashboard (utilization, free IPs, alert at 80%)
- Optional read-only Google Sheet export for people used to the spreadsheet

## Tag schema (draft)

| Tag | Example | Meaning |
|---|---|---|
| `hs/managed` | `true` | In scope of the operator |
| `hs/owner` | `team-payments` | Owning team |
| `hs/env` | `prod` | Environment |
| `hs/tier` | `private` / `public` / `db` | Purpose |
| `hs/pool` | `prod-eu-central-1` | Allocation pool (on VPC or IPAM pool) |
| `hs/managed-by` | `terraform` / `operator` | Who owns the lifecycle |

Enforced with AWS Organizations Tag Policies; violations are reported by the operator.

## Subnets on demand

A `SubnetClaim` asks for one subnet per availability zone. The operator reserves free CIDRs
in the VPC (first fit, lowest address first, skipping existing subnets and other claims'
reservations) and, in `Create` mode, creates the subnets with the organization's tags:

```yaml
apiVersion: network.hypersurgery.dev/v1beta1
kind: SubnetClaim
metadata:
  name: payments
  namespace: default
spec:
  scopeRef: organization
  account: "222222222222"
  region: eu-central-1
  networkID: vpc-0aa11bb2cc33dd44e
  prefixLength: 24
  zones: [eu-central-1a, eu-central-1b, eu-central-1c]
  owner: team-payments
  env: prod
  tier: private
```

`status.allocations` lists the CIDR, and once created the subnet ID, per subnet (named after
`namePrefix` and the zone, e.g. `payments-a`); the `Allocated`
and `Ready` conditions say how far the claim got. Subnets are tagged `hs/owner`, `hs/env`,
`hs/tier`, `hs/managed-by=subnet-operator` and `hs/claim=<namespace>/<name>`; the last one
lets the operator adopt a subnet it created even if the claim's status was lost.

The rules that keep this safe:

- **Writes are off by default.** The manager needs `--enable-writes` (`writes.enabled` in the
  chart) before any claim creates anything. Without it claims are still allocated, and the
  `Ready` condition says `WritesDisabled`.
- **A separate write role.** Discovery keeps its read-only role; creation uses
  `spec.accounts[].aws.writeRoleARN`, deployed from
  [`deploy/iam/spoke-write-role.cfn.yaml`](deploy/iam/spoke-write-role.cfn.yaml) only to the
  accounts that allow it. That role cannot delete subnets.
- **Only the namespaces a scope allows.** `spec.namespaceSelector` on the `NetworkScope` names
  the namespaces whose claims and imports may use its roles; a claim from anywhere else is
  refused by the webhook, or with `NamespaceNotAllowed` in its status, and never reaches AWS.
  Without a selector no namespace may; `{}` allows every namespace, and the operator warns about
  it. Who should hold `create` on claims and imports is in the
  [chart README](charts/subnet-operator/README.md#permissions).
- **Nothing is ever deleted.** Deleting a claim leaves its subnets in AWS; removing a zone from
  a claim leaves that subnet too.
- **Conflicts resolve themselves.** If someone takes a reserved CIDR before the subnet is
  created, AWS rejects it, the reservation is dropped and the next pass picks another one.
- **`mode: Allocate` for Terraform-first teams.** The operator only reserves the CIDRs; Terraform
  creates the subnets from `status.allocations`.

## Taking resources under management

Discovery does not stop at the tag selector. With `discoverUnmanaged` (on by default) the
operator also lists the VPCs the selector leaves out and their subnets, and reports them as
counts, status and metrics — not as objects, because they are not managed. That is what makes
`kubectl get networkscopes` show a non-zero `UNMANAGED` column, and what the
`UnmanagedNetworkResource` alert fires on.

A `ResourceImport` takes one of them under management:

```yaml
apiVersion: network.hypersurgery.dev/v1beta1
kind: ResourceImport
metadata:
  name: subnet-04d1c2b3a4e5f607-import
  namespace: default
spec:
  scopeRef: organization
  account: "333333333333"
  region: eu-central-1
  resourceID: subnet-04d1c2b3a4e5f607
  tags:
    hs/managed: "true"
    hs/owner: team-data
  requestedBy: anton (ticket NET-412)
  dryRun: false
```

The operator applies exactly those tags with `ec2:CreateTags` and does nothing else: the
resource keeps its configuration and every tag the import does not name. A tag the import
does name is set to the import's value even if the resource already had another one — that is
how `CreateTags` works — so the webhook warns when an import would change a value the
inventory shows. Running the same import twice changes nothing. A role holding only `ec2:CreateTags` is enough, so imports can
be allowed in accounts where subnet creation is not.

### The auto-import policy

The alternative to importing by hand is letting the scope do it. The CloudTrail event that
tells the operator a subnet was created also says *who* created it, so the tags can usually be
worked out without asking anybody:

```yaml
spec:
  autoImport:
    mode: DryRun                    # Off | DryRun | Apply
    namespace: platform
    fromCreator:                    # 1. who made it
      - principalPrefix: "arn:aws:sts::222222222222:assumed-role/payments-"
        tags: { hs/owner: team-payments }
    inheritFromNetwork: [hs/owner, hs/env]   # 2. the network (VPC) it lives in
    accountDefaults:                     # 3. the account it is in
      - account: "111111111111"
        tags: { hs/owner: team-platform }
    skip:
      - tagKey: managed-by
        tagValue: terraform
      - principalPrefix: "arn:aws:sts::222222222222:assumed-role/terraform-"
    requiredTags: [hs/owner]
```

The three sources are tried in that order and the first match wins. The policy's output is an
ordinary `ResourceImport`, so a decision it took and an import somebody wrote by hand travel
exactly the same path and leave the same audit trail (`kubectl get resourceimports -A` shows
who asked, `spec.requestedBy` names the creator the tags came from, and the
`network.hypersurgery.dev/created-by` annotation names the Kubernetes identity that created the
import — the operator's own service account for the policy's imports).

Five rules keep it from being a surprise:

- **Dry run first.** In `DryRun` the objects are created with `dryRun: true`, so a day of
  decisions can be read before anything in AWS changes.
- **Nothing owner-less is tagged.** If no rule resolves `requiredTags`, the resource stays
  unmanaged and the alert asks a human. Guessing an owner is worse than admitting there is none.
- **Terraform is left alone**, by tag or by the principal that created the resource.
- **Existing tags win.** A value the resource already carries beats one a rule inferred, so the
  policy only adds the tags that are missing (plus the managed tag, which is what imports it)
  and never reassigns a resource somebody already tagged.
- **Writes stay opt-in.** Without `--enable-writes` the imports are created and stay `Pending`,
  saying why.

## Terraform coexistence

Most existing VPCs and subnets are managed by Terraform. Rules:

- Terraform-owned resources are observed, never mutated.
- Operator-created subnets carry `hs/managed-by=subnet-operator` and must be excluded from
  Terraform (no `aws_subnet` resources for them; data sources select by tags).
- Option for Terraform-first teams: `mode: Allocate` only reserves a CIDR (written to the claim
  status); Terraform creates the subnet.

## Roadmap

| Phase | Content | Outcome |
|---|---|---|
| 0. Discovery | Map spreadsheet columns to tags, agree on tag schema, ADR on allocation backend | Tag schema, ADRs |
| 1. Read-only MVP ✅ | Multi-account and multi-region discovery, `VPC`/`Subnet` CRDs (now `Network`/`Subnet`), compliance findings, metrics | Spreadsheet no longer needed for viewing |
| 2. Scale out ✅ | Kind + Moto e2e, EventBridge → SQS events, Google Sheet export, Grafana dashboard, Helm chart | Near-real-time org-wide view |
| 3. Allocation ✅ | `SubnetClaim`, built-in first-fit allocator, opt-in creation behind a write role, route table association, Allocate mode for Terraform | Subnets on demand |
| 3b. Onboarding ✅ | Unmanaged discovery, `ResourceImport`, auto-import policy with creator attribution from CloudTrail | Nothing stays unowned by accident |
| 4. Hardening ✅ | Admission webhooks, audit trail, HA, namespaced Secret access, signed image and chart, alerts that notice silence, throttling backoff, live dashboard, upgrade tests, measured capacity, threat model, namespace-scoped writes, authenticated creator in the audit trail. Open: a conformance run against a real AWS organization | Production |
| 5. Cloud-neutral API (0.8) | `network.hypersurgery.dev/v1beta1` with the provider as a field, operator-driven migration from `aws.hypersurgery` (removed in 0.9), the project renamed to `subnet-operator` ([ADR 0002](docs/adr/0002-multi-cloud-model.md)); done: the API, the migration, the rename. Open: provider registry (#43), neutral metrics (#44), provider contract tests (#45) | One API for every cloud |
| 6. 1.0 | `v1` API with a compatibility promise, the [support policy](docs/policy.md) in full | Stable |
| 7. GCP and Azure (1.x) | Providers for GCP projects and Azure subscriptions on the 1.0 API | Same UX across clouds |

## Safety

- Read-only by default; subnet creation and tagging only with `--enable-writes` and a separate write role.
- A scope's write roles are limited to the namespaces its `namespaceSelector` selects; a scope
  without one lends them to no namespace.
- The audit trail records the authenticated Kubernetes user behind every claim and import
  (`created_by`), which nobody applying an object can choose.
- Imports never remove a tag. The auto-import policy only adds the tags a resource is missing
  and never tags a resource whose owner it cannot name; an import written by hand that changes
  an existing value gets a warning.
- The operator never deletes cloud resources; leader election keeps one writer at a time.
- CIDR conflicts are caught by AWS and resolved by reallocating.
- Identities, trust boundaries, threats and what mitigates them are in the
  [threat model](docs/security/threat-model.md).
