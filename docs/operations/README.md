# Operations

What to do when the operator is on your pager. Written from the code of the release these
pages ship with (1.0 and later: the `network.hypersurgery.dev/v1` API, the `hs_*` metrics);
every claim here names the file it comes from, so it can be checked rather than believed.

1.0 is the first stable release: its API and the [compatibility promise](../api-compatibility.md)
are what is stable. Everything here has been exercised against [Moto](https://github.com/getmoto/moto)
in Kind and a real kube-apiserver, not yet in a conformance run against a real AWS organization.
If you run it against one, what you see — especially where it differs from these pages — is the
most useful report the project can get ([CONTRIBUTING.md](../../CONTRIBUTING.md)).

- [Runbook](runbook.md) — one entry per alert the chart ships: what it means, how to confirm
  it, what to do, and when it is safe to ignore.
- [Upgrades and rollback](upgrades.md) — the path from every 0.x release to 1.0, which
  release added which CRD, what happens to existing objects, and how to go back.
- [Failure modes](failure-modes.md) — what the operator does when things break, what you see,
  and what it deliberately does *not* do.
- [Limits](limits.md) — accounts and regions per instance, the API-call cost of a resync, and
  the memory footprint.

## The two promises

Everything below rests on two properties that hold in the code today:

1. **The read path only calls `ec2:Describe*`.** The discovery client is the three paginators
   in `EC2API` (`internal/cloud/aws/discover.go`): `DescribeVpcs`, `DescribeSubnets`,
   `DescribeRouteTables`. Nothing else.
2. **The operator never deletes a cloud resource.** There is no EC2 `Delete*` or `DeleteTags`
   call anywhere in the write path; `internal/cloud/aws/writer.go` has exactly `CreateSubnet`,
   `ModifySubnetAttribute`, `AssociateRouteTable` and `CreateTags`. The only `Delete*` call in
   the whole codebase is `DeleteMessageBatch` against the operator's own SQS queue
   (`internal/cloud/aws/events/poller.go`).

Both are greppable, and that is the point:

```sh
# Expect: no hits. (CreateTags/DeleteTags in internal/cloud/aws/events are CloudTrail event
# names the operator reacts to, not calls it makes.)
grep -rn "ec2.Delete" --include='*.go' internal cmd
grep -rn "DeleteTags(" --include='*.go' internal cmd

# Expect: only the four write calls above.
grep -n "api\." internal/cloud/aws/writer.go
```

Writes are additionally gated twice: the manager needs `--enable-writes`
(`writes.enabled` in the chart) *and* the account needs its own `writeRoleARN` in the
`NetworkScope`. Without either, `SubnetClaim`s still allocate CIDRs and `ResourceImport`s stay
`Pending` with a reason — they do not silently do nothing.

## First five minutes

```sh
NS=subnet-operator-system

kubectl -n $NS get pods -l app.kubernetes.io/name=subnet-operator
kubectl -n $NS logs deployment/subnet-operator -c manager --tail=100
kubectl get nscope                      # Provider / Networks / Subnets / Unmanaged / Ready / Last sync
kubectl get nscope <scope> -o yaml | yq '.status.targets'   # per account/region
```

`status.targets[].error` is the single most useful field: it carries the last discovery error
of that account/region verbatim, and the `Ready` condition summarises how many targets failed
(`internal/controller/networkscope_controller.go`, `updateStatus`).

## Troubleshooting index

Every alert the chart ships (`charts/subnet-operator/templates/prometheusrule.yaml`), and where
its runbook entry is. `internal/metrics/references_test.go` fails when an alert is missing here.

| Alert | Severity | Runbook entry |
|---|---|---|
| `SubnetOperatorDown` | critical | [runbook.md#subnetoperatordown](runbook.md#subnetoperatordown) |
| `SubnetFull` | critical | [runbook.md#subnetfull](runbook.md#subnetfull) |
| `SubnetNearlyFull` | warning | [runbook.md#subnetnearlyfull](runbook.md#subnetnearlyfull) |
| `SubnetInventoryTargetDown` | warning | [runbook.md#subnetinventorytargetdown](runbook.md#subnetinventorytargetdown) |
| `SubnetInventoryTargetThrottled` | warning | [runbook.md#subnetinventorytargetthrottled](runbook.md#subnetinventorytargetthrottled) |
| `SubnetInventoryStale` | warning | [runbook.md#subnetinventorystale](runbook.md#subnetinventorystale) |
| `NetworkCIDROverlap` | warning | [runbook.md#networkcidroverlap](runbook.md#networkcidroverlap) |
| `UnmanagedNetworkResource` | warning | [runbook.md#unmanagednetworkresource](runbook.md#unmanagednetworkresource) |
| `SubnetClaimNotReady` | warning | [runbook.md#subnetclaimnotready](runbook.md#subnetclaimnotready) |
| `ResourceImportNotSettled` | warning | [runbook.md#resourceimportnotsettled](runbook.md#resourceimportnotsettled) |
| `AutoImportedResources` | info | [runbook.md#autoimportedresources](runbook.md#autoimportedresources) |

And the symptoms that have no alert of their own:

| Symptom | Where to look |
|---|---|
| A `NetworkScope` is not `Ready`: reason `SyncFailed` | [runbook: SubnetInventoryTargetDown](runbook.md#subnetinventorytargetdown), [failure modes](failure-modes.md#a-spoke-account-whose-role-cannot-be-assumed) |
| A `NetworkScope` is not `Ready`: reason `Throttled` | [runbook: SubnetInventoryTargetThrottled](runbook.md#subnetinventorytargetthrottled), [failure modes](failure-modes.md#an-account-that-aws-keeps-throttling) |
| A `NetworkScope`, claim or import says `ProviderNotEnabled` | [failure modes: a claim that cannot be satisfied](failure-modes.md#a-claim-that-cannot-be-satisfied) |
| Objects of an account or region vanished | [failure modes: a region or account dropped from a scope](failure-modes.md#a-region-or-account-dropped-from-a-scope), [two scopes covering the same account](failure-modes.md#two-scopes-covering-the-same-account-and-region) |
| A `SubnetClaim` stays `Allocated=False` or `Ready=False` | [runbook: SubnetClaimNotReady](runbook.md#subnetclaimnotready), [failure modes: the reasons](failure-modes.md#a-claim-that-cannot-be-satisfied) |
| A `ResourceImport` stays `Pending` or `Failed` | [runbook: ResourceImportNotSettled](runbook.md#resourceimportnotsettled) |
| A claim or import is refused with `NamespaceNotAllowed` | [runbook: SubnetClaimNotReady](runbook.md#subnetclaimnotready) (the scope's `namespaceSelector`) |
| The operator runs but is never ready; the log says old-group objects were never migrated | [upgrades: objects 0.8 never migrated](upgrades.md#objects-08-never-migrated) |
| Requests at `v1beta1` fail with `conversion webhook for ... failed` | [failure modes: the conversion webhook is unreachable](failure-modes.md#the-conversion-webhook-is-unreachable), [upgrades: reading and writing v1beta1](upgrades.md#reading-and-writing-v1beta1) |
| A CRD's `status.storedVersions` still lists `v1beta1`, or the log says `Could not migrate the stored objects to v1 yet` | [upgrades: what the operator does on its first start](upgrades.md#what-the-operator-does-on-its-first-start) |
| The log says `Could not configure the CRDs' conversion` | [failure modes: the conversion webhook is unreachable](failure-modes.md#the-conversion-webhook-is-unreachable) |
| `helm upgrade` fails with `... was removed in 1.0` | [upgrades: values and flags removed in 1.0](upgrades.md#values-and-flags-removed-in-10) |
| A full sync takes longer than a quarter of the resync interval | [limits: accounts and regions per instance](limits.md#accounts-and-regions-per-instance) |
| The operator is OOM-killed | [limits: memory](limits.md#memory) |
| The Google Sheet stopped updating | [failure modes: the Google Sheet export](failure-modes.md#the-google-sheet-export) (the `SheetExport`'s own status says why) |
