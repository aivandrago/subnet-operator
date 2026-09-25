# Upgrades and rollback

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
| v0.8.0 | **`subnet-operator`** 0.8.0 | **+ `network.hypersurgery.dev`**: `networkscopes`, `networks`, `subnets`, `subnetclaims`, `resourceimports`, `sheetexports`; the `aws.hypersurgery` CRDs stay, deprecated | The cloud-neutral API, the migration of every old object, the rename of the project and the chart — [its own section](#upgrading-from-07-to-08) |

Up to 0.7 all kinds are `aws.hypersurgery/v1alpha1`. There is only ever one API version, there is no
conversion webhook (the admission webhooks that arrived in 0.4.0 validate and default, they do not
convert), and every field added in 0.2.0 and 0.3.0 is optional. The CRD diffs between tags are
additions only:

```sh
git diff v0.1.0 v0.2.0 --stat -- config/crd/bases   # + subnetclaims, + writeRoleARN
git diff v0.2.0 v0.3.0 --stat -- config/crd/bases   # + resourceimports, + autoImport/discoverUnmanaged
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

- **`VPC` and `Subnet` objects are outputs, not state.** They are rebuilt from AWS on the
  first sync after the restart and owned by their `NetworkScope`. Losing them costs one
  resync, nothing else.
- **`NetworkScope` spec and status survive untouched.** New optional fields simply appear with
  their defaults on the next write.
- **`SubnetClaim` status survives**, including `status.allocations`. That matters: the
  allocations are the record of which CIDRs are reserved. If they were lost, the operator
  re-adopts subnets it created by their `hs/claim=<namespace>/<name>` tag, so a claim in
  `Create` mode heals rather than creating a second set of subnets.
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
| `pendingTargets` (targets an event marked changed) | the first sync after start is a full sync anyway, so nothing is missed |
| cached `AssumeRole` credentials | one extra `sts:AssumeRole` per account |

Events already delivered to SQS are **not** lost: they stay in the queue until the new leader
consumes them (messages are deleted only after being parsed).

## Upgrading 0.2.x → 0.3.x: one behaviour change to plan for

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

## Upgrading to namespace-restricted scopes and `created_by`

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
| Metrics, alerts | `hs_aws_*` | unchanged; neutral names follow in a later release, next to these (#44) |
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
kubectl -n <namespace> rollout restart deployment/<deployment>   # stops watching the old group
```

Otherwise 0.9 does it for you: it no longer ships them, and it refuses to start while any
unmigrated old object exists. Do not re-apply the 0.8 `crds/` directory after deleting them —
it contains the old group too.

### Rolling back to 0.7

`helm rollback` to the 0.7 revision restores the old chart's objects and names. The 0.7
operator reconciles the old objects again (it ignores the `migrated-to` marker); the new-group
objects stay, unreconciled. Anything done through the new group after the upgrade — a claim's
new reservations, an import applied since — is not in the old objects, so roll back soon or not
at all. Leave the CRDs as they are.

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
  `appVersion`; chart 0.3.1 and 0.3.0 both carry appVersion `v0.3.0`.

After a rollback, check the same things as after an upgrade.

## Verifying an upgrade

The upgrade from the latest published release to every change is tested in CI before it is
merged (`make test-upgrade`; what it checks is in the [support policy](../policy.md#the-upgrade-test)).
On your own cluster:

```sh
kubectl -n subnet-operator-system rollout status deployment/subnet-operator
kubectl get crds | grep hypersurgery                     # expect the kinds of the target version
kubectl get nscope                                       # Ready=True, "Last sync" within a resync interval
kubectl get nscope <scope> -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}'
```

Then wait one `resyncInterval` and confirm `hs_aws_scope_last_sync_timestamp_seconds` is
moving and no target has flipped to `hs_aws_target_up == 0`.
