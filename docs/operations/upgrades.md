# Upgrades and rollback

## What each release added

Taken from the tags in this repository (`git show <tag>:charts/aws-subnet-operator/Chart.yaml`,
`git ls-tree -r <tag> -- charts/aws-subnet-operator/crds`) and the release notes.

| App version | Chart | CRDs after the upgrade | What is new that an operator notices |
|---|---|---|---|
| v0.1.0 | 0.1.0 (0.1.1 adds the registry default) | `networkscopes`, `vpcs`, `subnets`, `sheetexports` | Read-only discovery, metrics, Grafana dashboard, five alerts, Google Sheet export |
| v0.2.0 | 0.2.0 | **+ `subnetclaims`** | `SubnetClaim`; `accounts[].writeRoleARN` on `NetworkScope`; `writes.enabled` → `--enable-writes`; RBAC for claims |
| v0.3.0 | 0.3.0 | **+ `resourceimports`** | `ResourceImport`; `spec.discoverUnmanaged` (**default `true`**) and `spec.autoImport` on `NetworkScope`; `UnmanagedNetworkResource` and `AutoImportedResources` alerts; unmanaged and auto-import metrics; RBAC for imports |
| v0.3.0 | 0.3.1 | unchanged | Dashboard panels for unmanaged resources and policy decisions — chart only, same image |

All kinds are `aws.hypersurgery/v1alpha1`. There is only ever one API version, there are no
conversion or admission webhooks, and every field added in 0.2.0 and 0.3.0 is optional. The
CRD diffs between tags are additions only:

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
helm pull hypersurgery/aws-subnet-operator --untar
kubectl apply -f aws-subnet-operator/crds/

helm upgrade --install subnet-operator hypersurgery/aws-subnet-operator \
  -n aws-subnet-operator-system --create-namespace \
  -f my-values.yaml
```

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
| `seenUnmanaged` (which unmanaged resources were already counted) | `UnmanagedNetworkResource` fires again for resources you already knew about — see the [runbook](runbook.md#unmanagednetworkresource) |
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

## Rollback

```sh
helm history subnet-operator -n aws-subnet-operator-system
helm rollback subnet-operator <revision> -n aws-subnet-operator-system
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

```sh
kubectl -n aws-subnet-operator-system rollout status deployment/aws-subnet-operator-controller-manager
kubectl get crds | grep aws.hypersurgery                 # expect the kinds of the target version
kubectl get networkscopes                                # Ready=True, "Last sync" within a resync interval
kubectl get networkscope <scope> -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}'
```

Then wait one `resyncInterval` and confirm `hs_aws_scope_last_sync_timestamp_seconds` is
moving and no target has flipped to `hs_aws_target_up == 0`.
