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
  targets, and a `TargetUnreachable` Warning Event on the scope carries the error.
- `hs_target_up == 0` and `hs_target_sync_errors_total` increments (once per failed
  *attempted* sync, not once per reconcile).
- Alert: [`SubnetInventoryTargetDown`](runbook.md#subnetinventorytargetdown).

**What it does not do.**

- It does not delete the target's `Network` and `Subnet` objects. `deleteGone` only runs for a
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

## A GCP project whose service account cannot be impersonated

The Google Cloud counterpart of the section above (from 2.0). A project whose
`accounts[].gcp.serviceAccount` the operator's own identity cannot get a token of fails
before any Compute call, with an error naming the service account and the permission to grant
(`ImpersonationError`, `internal/cloud/gcp/credentials.go`). Everything above holds: the target
keeps its last good numbers, the scope goes `SyncFailed`, `hs_target_up` drops to 0, and the
next attempt is the next resync. It is reported as throttling only when Google throttled the
token request itself. The operator never falls back to its own identity for a project that
names a service account, and never uses the write service account for discovery or the read one
for writes. Runbook: [SubnetInventoryTargetDown on GCP](runbook.md#subnetinventorytargetdown-on-gcp).

## A GCP tag key or value that does not exist

Tag keys and values on Google Cloud are resources that must exist before they are bound
(`internal/cloud/gcp/writer.go`). Every key and value a claim or import needs is resolved
first, and a missing one refuses the whole write before anything is created or bound:
`TagKeyMissing` for a key (the operator never creates keys), `TagValueMissing` for a value
when the scope does not set `spec.gcp.createTagValues`, `TagValueLimitReached` when creating
one would exceed the key's 1,000 values. A subnetwork is therefore never created without its
ownership tags. The per-claim tag `hs-claim` is one of them only when the scope sets
`spec.gcp.claimTag: Bind`; by default (`Skip`) a claim needs no value of its own, only the
owner, env and tier values the teams share, and finds the subnetwork it created by name when its
status is lost (`internal/controller/subnetclaim_controller.go`, `adoptsByName`). Two claims
leading to the same subnetwork name adopt nothing by name: the operator does not guess which one
created it, and the second claim's create fails with "exists already". From 2.0.1 the SubnetClaim
webhook refuses the second claim at apply time, naming the first; the controller's check stays
for claims that got past it (applied in the same moment, or while the webhooks were off). Likewise a key a resource already carries with another value refuses the whole
import (`TagValueConflict`): nothing is bound, and no binding is ever removed. The claim or
import keeps retrying every minute, so creating the key or value is enough to let it through.
Runbook: [SubnetClaimNotReady](runbook.md#subnetclaimnotready),
[ResourceImportNotSettled](runbook.md#resourceimportnotsettled).

## An Azure subscription whose identity gets no token, or has no role

The Azure counterpart of the two sections above (from 3.0). Azure has no AssumeRole:
a subscription whose account entry names `azure.clientID` is read with a token of that identity,
which Microsoft Entra ID issues in exchange for the operator's service account token when the
identity has a federated identity credential that trusts it. When Entra refuses, the target
fails before any Resource Manager call, with an error naming the client ID and the issuer and
subject the credential must name (`CredentialError`, `internal/cloud/azure/credentials.go`);
when the token is fine and the identity has no role assignment, Resource Manager answers 403
`AuthorizationFailed`. Everything above holds either way: the target keeps its last good
numbers, the scope goes `SyncFailed`, `hs_target_up` drops to 0, and the next attempt is the
next resync. It is reported as throttling only when Entra or Resource Manager answered 429. The
operator never falls back to its own identity for a subscription that names one, and never uses
`azure.writeClientID` for discovery or `azure.clientID` for writes.

Two things fail a target on Azure that have no counterpart elsewhere, and one does not fail it
that might be expected to:

- A resource group named in `spec.azure.resourceGroups` that does not exist in a subscription
  fails that subscription's targets (`ResourceGroupNotFound`), like a subscription that does
  not exist: it is a typo or a group deleted, and the inventory it had stays until the scope is
  corrected. The groups apply to every subscription of the scope.
- A location that has no virtual network, or does not exist, is **not** an error: Resource
  Manager lists a subscription's virtual networks without a location filter and discovery keeps
  the ones in the target's location (`discoverTarget`, `internal/cloud/azure/discover.go`), so
  such a target syncs with nothing in it.

Runbook: [SubnetInventoryTargetDown on Azure](runbook.md#subnetinventorytargetdown-on-azure),
[SubnetInventoryTargetThrottled on Azure](runbook.md#subnetinventorytargetthrottled-on-azure).

## An Azure virtual network that has no tag left, or an entry that does not fit

Azure subnets cannot carry tags, so a subnet's ownership is one tag on its virtual network,
`hs-subnet-<name>`, whose value holds the subnet's pairs as `key=value;…`
(`internal/cloud/azure/writer.go`). Two Azure limits apply, and both refuse the whole write
before anything is written: a resource carries at most 50 tags, shared between the network's own
tags and its subnets' entries (`TagBudgetExceeded`), and a tag value holds at most 256 characters
(`OwnershipEntryTooLong`). A claim writes its subnet's entry **before** it creates the subnet, so
a subnet the operator created never exists without its owner; when the network is full, the
claim creates nothing. An entry written for a create that then failed (a range taken in the
meantime) stays, names no subnet, and is used again by the next attempt; entries of subnets that
were deleted stay too, since the operator never removes a tag. An import does not add to them:
it reads the virtual network first and is refused (`ResourceNotFound`) when the subnet it names
does not exist. Both count against the 50, which
`status.azure.tagCount` on the `Network`, the `hs_network_tags` metric and the
[NetworkTagBudgetLow](runbook.md#networktagbudgetlow) alert show before the limit is reached.

Every write reads the network's tags first and refuses with `TagValueConflict` when a name it
would write has another value, because the Tags API's `Merge` would replace it: the operator
never overwrites an ownership value. The read and the `Merge` are two calls, and the Tags API has
no precondition on one tag, so a value somebody else writes to the same name between them is
replaced; for a subnet that means a change to the same subnet's entry within that round trip.
Runbook: [SubnetClaimNotReady](runbook.md#subnetclaimnotready),
[ResourceImportNotSettled](runbook.md#resourceimportnotsettled).

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
deletes the `Network` and `Subnet` objects whose `account/region` labels are no longer in the
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
| `NamespaceNotAllowed` | the scope's `spec.namespaceSelector` does not select the claim's namespace (unset selects none, `{}` all) | move the claim, or have the scope's owner select the namespace |
| `AccountNotInScope` | the account/region pair is not covered by that scope | add it to the scope |
| `ProviderNotEnabled` | the scope's `spec.provider` is not one the operator was started with (`--providers`, chart `providers.<name>.enabled`); the scope itself says the same | enable the provider |
| `NetworkNotFound` | the network (VPC) has not been discovered (wrong ID, wrong account, or the network selector excludes it; on Azure also a virtual network outside the scope's `spec.azure.resourceGroups`) | check `kubectl get hsnet`, the selector, and that the target is healthy |
| `NoSpace` | the allocator found no free block: `wanted 3 x /24, found 1` | ask for a smaller prefix, fewer AZs, or add a CIDR to the VPC (on GCP: a larger or another `spec.gcp.poolCIDRs`; on Azure: a prefix added to the virtual network's address space) |
| `WritesDisabled` | `mode: Create` but the manager runs without `--enable-writes` | the CIDRs *are* reserved — create the subnets yourself, or enable writes |
| `NoWriteRole` | the account has no `writeRoleARN` (on GCP: is read through `gcp.serviceAccount` and has no `gcp.writeServiceAccount`; on Azure: is read through `azure.clientID` and has no `azure.writeClientID`) | add one (`deploy/iam/spoke-write-role.cfn.yaml`; on GCP `deploy/gcp/writer-role.yaml` and the tag user role; on Azure `deploy/azure/writer-role.json`) |
| `CreateNotSupported` | `mode: Create` on a provider that cannot create subnets (none today: AWS, GCP and Azure all list `CreateSubnet` in the scope's `status.capabilities`) | use `mode: Allocate` |
| `ZonesRequired`, `InvalidPrefixLength` | the claim asks for something an AWS subnet cannot be: no zones, or a prefix outside /16–/28. The webhook refuses these at apply time; the status only says so for a claim created while it was off | fix the spec |
| `CreateFailed` | the cloud rejected at least one create (`CreateSubnet` on AWS, `subnetworks.insert` on GCP, a subnet PUT on Azure) | read `status.allocations[].error` |
| `ZonesNotSupported`, `PoolRequired`, `InvalidClaim` | GCP and Azure: the claim lists zones (a subnetwork and an Azure subnet are regional), has no `spec.gcp.poolCIDRs` (GCP only: a VPC network has no address space of its own), or asks for what Compute or Azure cannot create (on Azure a prefix outside /2–/29, a `namePrefix` that is not a subnet name or is one Azure reserves, `aws` or `gcp` options); refused by the webhook at apply time | fix the spec |
| `TagKeyMissing`, `TagValueMissing`, `TagValueLimitReached` | GCP: a tag the subnetwork is created with has no key under `gcp.tagParent`, a value nobody created (and `createTagValues` is off), or a key already at 1,000 values; nothing was created | [runbook](runbook.md#subnetclaimnotready) |
| `TagBudgetExceeded`, `OwnershipEntryTooLong`, `TagValueConflict` | Azure: the virtual network already carries 50 tags, the claim's tags do not fit in one 256-character tag value, or an entry of that subnet name exists with other values; nothing was created | [runbook](runbook.md#subnetclaimnotready) |

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
  stays in AWS (`Reconcile`: "Subnets stay: deleting them is a human decision, made in the
  cloud").

**A partial `Create` is possible and is reported.** If `CreateSubnet` succeeds but the
follow-up `ModifySubnetAttribute` or `AssociateRouteTable` fails, the subnet ID is recorded
with the error, state `Failed` — precisely so the next pass does not create a second subnet.

## The operator is down while somebody creates subnets by hand

**Nothing is lost, and nothing is reverted.** The operator holds no state that AWS depends on.

- **During the outage** the inventory freezes. `hs_*` series stop being scraped, so the
  shipped `SubnetInventoryStale` rule cannot fire; [`SubnetOperatorDown`](runbook.md#subnetoperatordown),
  which fires on the absence of the scrape itself, is the alert for this.
- **Events queue up.** EventBridge keeps delivering to SQS; messages are only deleted after
  the poller has read them, so an outage shorter than the queue's retention loses nothing.
  (A crash *between* deleting a message and finishing the sync does lose that event — the
  periodic full resync is the backstop, which is exactly what it is for.) On GCP the Pub/Sub
  subscription keeps what nobody acknowledged for its message retention (a day in
  [`deploy/gcp/events.md`](../../deploy/gcp/events.md)), and the same holds for a message
  acknowledged just before a crash. On Azure the Storage queue keeps an event until the
  operator has enqueued the resync it asks for, so an event received just before a crash comes
  back to the next leader once its visibility timeout ends (under a minute).
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
- **Events are debounced** for `--aws-events-debounce` (10s; `--gcp-events-debounce` on GCP),
  so a burst of API calls causes one resync per target, and the poller only runs on the leader.
  On GCP a network is global: an event about it resyncs every region of its project the scope
  covers. On Azure (`--azure-events-debounce`) an event names no location: a network the last
  discovery listed resyncs its own location, any other every location of its subscription the
  scope covers.
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
outside the list in `internal/cloud/aws/events/events.go`. On GCP (`internal/cloud/gcp/events/events.go`):
failed calls (a non-zero `status.code`), entries that are not audit logs, Compute collections
other than networks and subnetworks, reads and IAM policy calls, and tag bindings on anything
but a network or subnetwork. ENI churn is not an event: free-IP counts
move with every pod, so they are refreshed by the periodic resync instead. Every message is
counted in `hs_change_events_total` by what became of it.

## The change events cannot be read

The queue or subscription was deleted, or the operator lost its grant on it. The event source
logs `failed to receive events`, counts `hs_change_event_errors_total`, and retries with backoff
up to a minute, so a fixed subscription or grant is picked up without a restart. Meanwhile the
inventory follows the full resyncs, as it would without events; nothing else degrades.
[SubnetInventoryChangeEventsFailing](runbook.md#subnetinventorychangeeventsfailing) fires when
the failures last. A message that cannot be parsed is not a failure of the source: it is
acknowledged (deleted, on SQS and on an Azure Storage queue), counted as `malformed` and
dropped, since redelivering it would never succeed. On Azure a message is deleted only after its
resync is enqueued, so a delete that fails (an identity that may read the queue but not delete
from it) counts as a failure of the source too, and a message that came back more than 5 times
is dropped as `malformed` rather than resynced for ever.

## Two GCP networks that overlap and are peered

**What happens.** Compute refuses a peering between networks whose subnetwork ranges overlap, and
a subnetwork that overlaps a network it is actively peered with. It does not stop one side from
peering while the other is not peered yet (the peering waits, `INACTIVE`), nor either network
from growing an overlapping range in the meantime, nor a network from being deleted and
re-created with other ranges. What is left is a peering that can never become active.

**What the operator does.** Discovery reads each network's peerings with `networks.list` and
compares the subnetwork ranges of all networks of the scope after each sync.
`hs_network_cidr_overlaps` counts every overlapping network, as for VPCs, and
`hs_network_peered_cidr_overlaps` those that are also peered; the alert
[`NetworkPeeredCIDROverlap`](runbook.md#networkpeeredcidroverlap) fires after ten minutes, and
`status.gcp.overlaps` on both `Network` objects names the subnetworks and ranges involved and the
state of the peering. The operator never renumbers anything nor touches a peering.

**What it cannot see.** A network of a project outside the scope: its peering is listed in
`status.gcp.peerings`, but its ranges are unknown, so an overlap with it is not reported.
Subnetworks in regions the scope does not cover are not discovered and not compared either.

## Two scopes covering the same account and region

The first scope to create a `Network` or `Subnet` object owns it. The second sees the
`network.hypersurgery.dev/scope` label of another scope, skips the object and logs
`object belongs to another NetworkScope, skipping` (`upsert`). Its own counts will be lower
than reality and nothing says so in the status. Do not overlap scopes; split the organization
by account or region instead.

## The Google Sheet export

`SheetExport` is an output. The sheet is rewritten on every refresh and never read back, so a
broken export cannot corrupt the inventory; it only stops updating and reports the failure in
its own status. Nothing else in the operator waits on it.

## The conversion webhook is unreachable

From 1.0 the API server converts between `network.hypersurgery.dev/v1beta1` and `v1` through
the operator's webhook, once the operator has rewritten every stored object at v1 (before that
the CRDs use the API server's own conversion, which is exact for the two versions). Everything
is stored at v1, so a request at v1 never needs the webhook: the controllers, the dashboard,
`kubectl get` without a version, garbage collection and namespace deletion carry on while the
operator is down. A request at v1beta1 fails with `conversion webhook for ... failed` until an
operator pod is ready again. A client that must keep working without the operator should use v1;
after the operator is uninstalled, set the CRDs back to `None`
([upgrades](upgrades.md#rolling-back-to-09)) or remove the clients that still ask for v1beta1.

If the webhook's certificate Secret has no `ca.crt` (a cert-manager issuer that does not fill
it), the operator keeps the CRDs at `None` and logs `Could not configure the CRDs' conversion`
every minute; conversion then works without the webhook.


## The webhook certificate has expired

Without cert-manager the chart signs the webhook serving certificate for
`webhook.certificate.duration` (a year by default) and only a `helm upgrade` renews it: the
first one within `renewBefore` of its expiry. On a release nobody upgrades for that long the
certificate expires, and the API server's calls to the webhooks fail with `x509: certificate
has expired or is not yet valid`. With the default `failurePolicy: Fail` every create and update
of a `SubnetClaim`, `NetworkScope` or `ResourceImport` is refused; the defaulting webhooks are
`Ignore`, and requests at v1beta1 fail the way they do while the
[conversion webhook is unreachable](#the-conversion-webhook-is-unreachable). The controllers
and everything already applied keep working.

The fix is the upgrade that was due: `helm upgrade` with the same chart version and values signs
a new certificate (the expiry is in the Secret's `network.hypersurgery.dev/webhook-not-after`
annotation: `kubectl get secret <release>-webhook-cert -o yaml`). Deleting the Secret and
upgrading does the same. To stop it happening, use cert-manager
(`webhook.certificate.certManager=true`), which renews on its own, or upgrade at least once per
`duration`.
