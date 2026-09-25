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
| v0.9.0 | `subnet-operator` 0.9.0 | `network.hypersurgery.dev` only. The chart no longer ships the `aws.hypersurgery` CRDs; the ones 0.8 installed stay in the cluster until you delete them | The old group removed, a guard against objects 0.8 never migrated, metrics renamed to `hs_*`, providers — [its own section](#upgrading-from-08-to-09) |

Up to 0.7 all kinds are `aws.hypersurgery/v1alpha1`; 0.8 serves both groups, 0.9 only
`network.hypersurgery.dev/v1beta1`. There is only ever one API version per group, there is no
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
- **Values**: AWS settings move under `providers.aws`. The 0.8 names still work **until 0.10**,
  and `helm install`/`upgrade` prints a note while they are set; the new name wins when both are.

  | 0.8 (deprecated, removed in 0.10) | 0.9 |
  |---|---|
  | `events.queueUrl`, `events.debounce` | `providers.aws.events.queueUrl`, `providers.aws.events.debounce` |
  | `aws.region`, `aws.endpointURL` | `providers.aws.region`, `providers.aws.endpointURL` |
  | `networkPolicy.egress.podIdentity` (`enabled`, `cidr`, `port`) | `providers.aws.podIdentity`, the same keys; a key set there wins |
  | `serviceAccount.annotations."eks.amazonaws.com/role-arn"` (not deprecated; it still wins) | `providers.aws.irsaRoleARN` |

- **Flags** (only if you pass them yourself, e.g. with `extraArgs`): `--providers` (default
  `aws`) chooses the clouds; `--events-queue-url` and `--events-debounce` became
  `--aws-events-queue-url` and `--aws-events-debounce`. The old flags and `EVENTS_QUEUE_URL`
  still work until 0.10.
- **New condition reasons**: `ProviderNotEnabled` on a scope, claim or import whose provider the
  operator is not started with (replaces `ProviderNotSupported`, which no release could
  produce for a valid object), and `CreateNotSupported` on a Create-mode claim of a provider
  that cannot create subnets (none today).
- **An EC2 subnet without an `AvailableIpAddressCount`** is now reported with unknown free IPs
  (unset `availableIPs` and `utilizationPercent`) instead of as full. EC2 always sends the count,
  so this only matters for emulators.

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
kubectl get crds | grep hypersurgery                     # the kinds of the target version (and aws.hypersurgery until you delete it)
kubectl get nscope                                       # Ready=True, "Last sync" within a resync interval
kubectl get nscope <scope> -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}'
```

Then wait one `resyncInterval` and confirm `hs_scope_last_sync_timestamp_seconds` is
moving and no target has flipped to `hs_target_up == 0` (before 0.9, `hs_aws_scope_last_sync_timestamp_seconds`
and `hs_aws_target_up`).
