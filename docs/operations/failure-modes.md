# Failure modes

The project's promise is that it degrades honestly: it reports what it could not do, keeps the
last known state instead of erasing it, and never deletes a cloud resource. This page says,
for each way things go wrong, what the operator does, what you see, and what it explicitly
does **not** do — with the file that implements it, so the claims can be checked.

## A spoke account whose role cannot be assumed

**What the operator does.** Discovery of the targets runs in an `errgroup` with a concurrency
limit, and every goroutine returns `nil` on purpose — "one failing account must not stop the
others" (`discoverAll`, `internal/controller/networkscope_controller.go`). The failed target is
skipped in the sync loop, the error is logged once, and the sync continues with the rest.

**What you see.**

- `status.targets[].error` for that account/region carries the AWS error verbatim, while
  `networks`, `subnets` and `lastSyncTime` keep their **last good** values — "so a flapping
  account does not look empty" (`updateStatus`).
- The `Ready` condition goes `False` with reason `SyncFailed` and a message naming the failed
  targets.
- `hs_target_up == 0` and `hs_target_sync_errors_total` increments (once per failed
  *attempted* sync, not once per reconcile).
- Alert: [`SubnetInventoryTargetDown`](runbook.md#subnetinventorytargetdown).

**What it does not do.**

- It does not delete the target's `VPC` and `Subnet` objects. `deleteGone` only runs for a
  target whose snapshot succeeded, so the last known inventory of an unreachable account stays
  in the cluster, visibly stale rather than silently missing.
- It does not zero the numbers, and it does not mark the whole scope failed — the other
  targets sync normally and `status.networks` / `status.subnets` still count everything present.
- It does not retry in a tight loop: the next attempt is the next resync or the next event.
- It does not touch AWS, and it never falls back to another set of credentials.

**One deliberate gap.** The unmanaged gauges of a failed target are *not* carried over: the
sync clears them and only refills them for targets that succeeded
(`reportUnmanaged`, `internal/controller/autoimport.go`). A missing series says "we do not
know right now", which is true; a held-over number would read as current. `hs_target_up`
says why the gap is there.

## An account that AWS keeps throttling

**What the operator does.** Every EC2 call is retried by the SDK's adaptive retry mode (5
attempts, a client-side rate limiter per account/region that remembers earlier throttles;
`internal/cloud/aws/discoverer.go`). A discovery that is still throttled after that comes back
wrapping `inventory.ErrThrottled`, and the controller backs the target off
(`internal/controller/backoff.go`): it is left out of full syncs and change events and retried
on its own after about 1, 2, 4, 8, 16 and then every 30 minutes, jittered. The first discovery
that gets through resets the backoff.

**What you see.**

- The same stale-not-gone picture as for an unreachable account: objects kept, last good
  numbers and `lastSyncTime` in `status.targets[]`, the error text there starting with
  `throttled by the cloud API`.
- The `Ready` condition goes `False` with reason `Throttled` (or `SyncFailed`, when another
  target is unreachable at the same time — that is the more urgent one).
- A `TargetThrottled` Warning Event naming the time of the next attempt.
- `hs_target_throttled == 1` while `hs_target_up` **stays 1**: the account answered.
  `hs_api_throttled_total` counts every throttled attempt, including those a retry rode
  out, so pressure is visible before anything goes stale. `hs_target_sync_errors_total`
  does not count throttled discoveries.
- Alert: [`SubnetInventoryTargetThrottled`](runbook.md#subnetinventorytargetthrottled), not
  `SubnetInventoryTargetDown`.

**What it does not do.** It does not retry the target in a tight loop or at the cadence of the
healthy ones, it does not let an EC2 event bring a waiting target forward, and it does not hold
a discovery slot while the target waits — the other targets sync normally. The backoff lives in
memory: after a restart each throttled target costs one throttled discovery before it is
backed off again.

## A region or account dropped from a scope

**What the operator does.** Removing a region or an account from `spec` bumps the generation,
which forces a full sync (`needsFullSync`). At the end of that sync `deleteRemovedTargets`
deletes the `VPC` and `Subnet` objects whose `account/region` labels are no longer in the
spec. Deleting the whole `NetworkScope` is the same story through owner references: the
objects are garbage-collected by Kubernetes and `metrics.Forget(scope)` drops every series.

**What you see.** The objects disappear from `kubectl get hssubnet`, the counts in the scope
status drop, and the metric series for that scope/account/region end.

**What it does not do.** Nothing in AWS changes. Not one API call is made as a result of the
removal — `deleteObjects` only ever calls the Kubernetes client, and there is no EC2 delete
call anywhere in the codebase (see the greps in the [operations index](README.md)). A region
removed by accident is restored by putting it back: the next full sync rediscovers it.

**Worth knowing.** Removed targets are only cleaned up on a *full* sync. An event-driven
partial sync deletes nothing outside the targets it synced.

## A claim that cannot be satisfied

`SubnetClaim` never fails loudly; it parks in a status that says why and retries every minute
(`claimRetryInterval`). The reason on the `Ready` condition tells you which wall it hit:

| Reason | Meaning | What to do |
|---|---|---|
| `ScopeNotFound` | `spec.scopeRef` names no `NetworkScope` | fix the reference |
| `AccountNotInScope` | the account/region pair is not covered by that scope | add it to the scope |
| `ProviderNotEnabled` | the scope's `spec.provider` is not one the operator was started with (`--providers`, chart `providers.<name>.enabled`); the scope itself says the same | enable the provider |
| `NetworkNotFound` | the network (VPC) has not been discovered (wrong ID, wrong account, or the network selector excludes it) | check `kubectl get hsnet`, the selector, and that the target is healthy |
| `NoSpace` | the allocator found no free block: `wanted 3 x /24, found 1` | ask for a smaller prefix, fewer AZs, or add a CIDR to the VPC |
| `WritesDisabled` | `mode: Create` but the manager runs without `--enable-writes` | the CIDRs *are* reserved — create the subnets yourself, or enable writes |
| `NoWriteRole` | the account has no `writeRoleARN` | add one (`deploy/iam/spoke-write-role.cfn.yaml`) |
| `CreateNotSupported` | `mode: Create` on a provider that cannot create subnets (none today; `status.capabilities` of the scope lacks `CreateSubnet`) | use `mode: Allocate` |
| `CreateFailed` | AWS rejected at least one `CreateSubnet` | read `status.allocations[].error` |

**What the operator does with an unsatisfiable claim.** Allocation is all-or-nothing per pass:
if the pool cannot serve every missing AZ, no CIDR is reserved for any of them and the
`Allocated` condition goes `False` with the allocator's message. Allocations that already
exist are kept. The pool it allocates from is the VPC's CIDRs minus every existing subnet
**and** every other claim's reservations, read straight from the API server rather than the
cache, so two claims racing in the same VPC do not hand out the same block.

**What it does not do.**

- It does not widen the pool, add a CIDR to the VPC, or steal space from another claim.
- It does not delete or resize anything to make room.
- It does not give up: the claim keeps retrying, so freeing space is enough to make it
  succeed without touching the object.
- Deleting the claim does not delete its subnets, and removing an AZ from the claim does not
  delete that AZ's subnet — the object is dropped from `status.allocations` and the subnet
  stays in AWS (`Reconcile`: "Subnets stay: deleting them is a human decision, made in AWS").

**A partial `Create` is possible and is reported.** If `CreateSubnet` succeeds but the
follow-up `ModifySubnetAttribute` or `AssociateRouteTable` fails, the subnet ID is recorded
with the error, state `Failed` — precisely so the next pass does not create a second subnet.

## The operator is down while somebody creates subnets by hand

**Nothing is lost, and nothing is reverted.** The operator holds no state that AWS depends on.

- **During the outage** the inventory freezes. `hs_*` series stop being scraped, which is
  why you want the `absent()` alert from the [runbook](runbook.md#subnetinventorystale) — the
  shipped `SubnetInventoryStale` rule cannot fire when the metric itself is gone.
- **Events queue up.** EventBridge keeps delivering to SQS; messages are only deleted after
  the poller has read them, so an outage shorter than the queue's retention loses nothing.
  (A crash *between* deleting a message and finishing the sync does lose that event — the
  periodic full resync is the backstop, which is exactly what it is for.)
- **On start** `status.lastSyncTime` is old, so the first reconcile is a full sync of every
  target, and the hand-made subnet appears within one sync.
- **If the subnet was created without the managed tag**, it is counted as unmanaged, the
  `UnmanagedNetworkResource` alert fires, and an auto-import policy may tag it. If it was
  created inside a VPC the selector matches, it becomes a `Subnet` object like any other,
  including its missing-tag findings.
- **Attribution may be missing.** The creator cache is in memory (512 entries, 24h TTL). If
  the CloudTrail event arrived while the operator was down and the queue delivered it after
  the restart, attribution survives; if the event was consumed before the crash, it does not,
  and the policy falls back to VPC inheritance, then account defaults, then `no_owner`.

**What it does not do on recovery.** It does not delete the hand-made subnet, does not undo
manual tag changes, and does not "correct" AWS to match anything. It mirrors what it finds.
The only writes it can ever do are the two opt-in paths (claims and imports).

## A resync that races an EventBridge event

The two paths are designed to overlap harmlessly.

- **Events are taken before discovery starts.** `changed := r.pending.take(scope.Name)` runs
  before the AWS calls, so an event that arrives *during* the sync lands in a fresh pending
  set and triggers another reconcile afterwards rather than being swallowed.
- **A failed reconcile puts the events back.** The deferred `r.pending.add(scope.Name, changed)`
  restores them, so an error does not drop the notification.
- **Duplicate syncs are harmless.** Object writes go through `CreateOrUpdate` plus a status
  update that is skipped when the new status is deep-equal to the old one, so a redundant
  resync produces no API-server writes and no metric churn.
- **A partial sync cannot delete a healthy target's objects.** `deleteGone` is scoped to the
  account/region it just synced, and `deleteRemovedTargets` runs only on full syncs.
- **Events are debounced** for `--aws-events-debounce` (10s), so a burst of API calls causes one
  resync per target, and the poller only runs on the leader.
- **A stale snapshot cannot resurrect a deleted subnet as a permanent object**: the next sync
  of that target reconciles the difference. The worst case is one resync interval of a subnet
  object that AWS no longer has, or a missing one that AWS already has.

**Where a race does cost something.** A subnet created by someone else between the operator's
inventory read and its own `CreateSubnet` makes AWS reject the CIDR
(`InvalidSubnet.Conflict`). The operator drops the reservation, says so in the log
(`cidr taken, reallocating`), and the next pass allocates from an inventory that includes the
winner. No subnet is deleted and no CIDR is reused.

**Events the operator ignores on purpose.** Failed API calls (`errorCode` set), non-EC2
sources, tag changes on resources that are not `vpc-`/`subnet-`/`rtb-`/`igw-`, and anything
outside the list in `internal/cloud/aws/events/events.go`. ENI churn is not an event: free-IP counts
move with every pod, so they are refreshed by the periodic resync instead.

## Two scopes covering the same account and region

The first scope to create a `VPC` or `Subnet` object owns it. The second sees the
`network.hypersurgery.dev/scope` label of another scope, skips the object and logs
`object belongs to another NetworkScope, skipping` (`upsert`). Its own counts will be lower
than reality and nothing says so in the status. Do not overlap scopes; split the organization
by account or region instead.

## The Google Sheet export

`SheetExport` is an output. The sheet is rewritten on every refresh and never read back, so a
broken export cannot corrupt the inventory; it only stops updating and reports the failure in
its own status. Nothing else in the operator waits on it.
