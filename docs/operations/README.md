# Operations

What to do when the operator is on your pager. Written from the code as of chart 0.3.1
(appVersion v0.3.0); every claim here names the file it comes from, so it can be checked
rather than believed.

- [Runbook](runbook.md) — one entry per alert the chart ships: what it means, how to confirm
  it, what to do, and when it is safe to ignore.
- [Upgrades and rollback](upgrades.md) — which release added which CRD, what happens to
  existing objects, and how to go back.
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
