# Limits

What one instance is expected to carry, and what it costs. The formulas come from the code;
where a number is arithmetic rather than a measurement, it says so. The documented capacity —
400 account/region targets — is measured by a scale test against a fake EC2 and a real
kube-apiserver ([below](#measured-at-the-documented-capacity)); nothing has been measured
against a real AWS organization of that size, so the EC2 latencies are assumptions.

## Accounts and regions per instance

A **target** is one account/region pair: `targets = Σ accounts × regions`
(`expandTargets`). Targets are discovered in parallel up to `--discovery-concurrency`
(`discovery.concurrency`, default 4); the calls *within* one target are sequential. The cap is
one semaphore for the whole instance (`discoverySlots`), not one per scope, so it holds however
many scopes are being synced at once.

```
full sync wall clock ≈ ceil(targets / concurrency) × per-target latency      # discovery
                     + throttled targets × ~17 s / concurrency               # SDK retries
                     + Kubernetes writes of the sync                          # after discovery
```

The Kubernetes part is not overlapped with discovery: a sync discovers every target first and
then writes the objects target by target. It is small when little changed and large on the
first sync (see the measurements).

Per-target latency against real EC2 is roughly 0.5–2 s for the three or four paginated calls
below, more with pagination or throttling. With the defaults (concurrency 4, 1.5 s per
target), the discovery part alone:

| Targets | Example | Full sync | Against a 10m `resyncInterval` |
|---|---|---|---|
| 40 | 10 accounts × 4 regions | ~15 s | comfortable |
| 200 | 50 accounts × 4 regions | ~75 s | comfortable |
| 600 | 150 accounts × 4 regions | ~4 min | raise concurrency |
| 1000+ | 250 accounts × 4 regions | ~6 min+ | too close: raise concurrency *and* the interval |

**The default to plan for: 100 accounts at up to 4 regions each — 400 targets — per
instance**, with the shipped defaults (concurrency 4, `resyncInterval` 10m). The reasoning:

- **A full sync should fit in a quarter of the resync interval.** 400 targets at 1.5 s each
  over 4 slots is about 150 s, a quarter of 10 minutes; measured with nothing changing, it is
  157 s. The rest is headroom for the slow cases, and it is needed: the first sync writes every
  object (199 s measured), and throttling is expensive. A throttled call is retried up to 5
  times with the SDK's backoff (about 15 s in all on average, 30 s at worst) before discovery
  gives up, so each throttled target holds its slot for about 17 s instead of 1.5 s. With a
  tenth of the targets throttled the measured sync took 315 s — more than half the interval.
- **EC2 rate limits do not set this number.** EC2 throttles per account and per region, and
  discovery sends each account/region at most one request at a time — 3 or 4 per full sync.
  An instance cannot exhaust an account's API budget by serving more accounts; what throttles
  it is everything else calling EC2 there. That is why throttled targets are backed off rather
  than the whole instance slowed down, and why adding accounts is limited by wall clock, not
  by AWS.
- **Concurrency scales it almost linearly,** for the same reason: more slots means more
  different accounts in parallel, not more pressure on any one of them. It does not shorten
  the Kubernetes part, which is sequential and grows with the number of objects that changed.

So for more than 100 accounts, in this order: raise `discovery.concurrency` in proportion
(300 accounts × 4 regions → 12), then raise `resyncInterval` and rely on EventBridge for
freshness. Past a few thousand targets the Kubernetes API side below becomes the limit.

To check a running instance against this, the reconcile duration of the scope controller is
on the metrics endpoint already (it includes the event-driven partial syncs, which only make
it look faster):

```promql
histogram_quantile(0.9, sum by (le) (rate(controller_runtime_reconcile_time_seconds_bucket{controller="networkscope"}[1h])))
```

Keep it under a quarter of the resync interval.

Nothing overlaps or piles up if a sync is slow: controller-runtime reconciles one
`NetworkScope` at a time and the next resync is scheduled *after* the previous one finishes.
The price of a slow sync is staleness, not a thundering herd.

**Two things that do not scale the way you might hope.**

- `MaxConcurrentReconciles` is not set for any controller, so it is 1. Several
  `NetworkScope`s in one instance are reconciled **one after another**, not in parallel.
  Splitting a big scope into several small ones does not buy throughput — raising
  `discovery.concurrency` does. (Were it ever raised, the concurrency cap would still hold for
  the instance as a whole.)
- There is no flag to make an instance watch only some scopes (no namespace or label
  selector), and the CRDs are cluster-scoped. Two operator installs in one cluster would both
  reconcile every `NetworkScope` and fight over the same `VPC`/`Subnet` objects. Shard across
  clusters, not within one.

## Measured at the documented capacity

`make test-scale` (`internal/controller/scale_test.go`, build tag `scale`, about 15 minutes)
reconciles one `NetworkScope` of **100 accounts × 4 regions = 400 targets** with the shipped
defaults (concurrency 4, `resyncInterval` 10m) through a controller-runtime manager like the
one `cmd/main.go` builds — informer cache, API reader, event recorder, no client-side rate
limit — against envtest's real kube-apiserver and etcd. EC2 is an in-process fake that takes
375 ms per Describe call (4 calls, so 1.5 s per target, the figure the guidance assumes) and,
for a throttled target, holds the slot for 5 calls plus 15 s of SDK backoff before it returns
`ErrThrottled`.

The organization: per target 2–5 managed VPCs and 6–20 managed subnets (3.5 and 13 on
average: a VPC per environment plus a shared one, subnets per AZ and tier), plus the AWS
default VPC with three default subnets left unmanaged, so discovery makes the four calls of
`discoverUnmanaged: true`. That is **1,400 VPCs and 5,205 subnets** mirrored, 1,600 unmanaged
resources counted. Every 25th target reuses one CIDR, so 16 VPCs report overlaps; one subnet
in ten lacks the required owner tag. Six tags per resource.

Measured on an Intel Core i9-9900K (8 cores, 16 threads), 62 GiB RAM, Linux 7.0, Go 1.27.1,
with the API server and etcd (Kubernetes 1.37) on the same machine, which was running other
builds at the same time. Wall-clock numbers from a run with the fake latencies above; each
phase is one reconcile, the fake clock moving between them so each one is the sync it says:

| Sync | Reconcile | of which discovery | Kubernetes writes | Operator CPU | Peak heap / RSS |
|---|---|---|---|---|---|
| First full sync, empty cluster | **199 s** | 150 s | 13,227 (6,605 creates, 6,621 status) | 13.5 s | 88 / 146 MiB |
| Full sync, nothing changed | **157 s** | 150 s | **1** (the scope's status) | 3.5 s | 146 / 201 MiB |
| Full sync, free IPs moved in 20% of subnets | **192 s** | 150 s | 1,871 (1,095 subnets, 775 VPCs, the scope) | 5.1 s | 173 / 230 MiB |
| Full sync, 10% of targets (40) throttled | **315 s** | 310 s | 1 | 3.1 s | 148 / 226 MiB |
| Their retry, alone, when the backoff ran out | 16 s | 15 s (40 targets) | 1 | 0.8 s | 154 / 231 MiB |
| Event-driven sync of 10 targets | 5.5 s | 4.5 s | 1 | 0.6 s | 175 / 234 MiB |

What the numbers say:

- **The discovery formula is exact** — 150.1 s for 400 × 1.5 s over 4 slots — and the
  Kubernetes part comes on top of it: 7 s for the 804 uncached `LIST`s of a quiet sync, about
  50 s for the 13,000 writes of the first one here (3.7 ms each against a local etcd). A real
  control plane with remote etcd is slower per write; at 10 ms the first sync would take
  about 5 minutes. It is a one-off: afterwards only changed objects are written.
- **Unchanged objects are not rewritten.** A full sync of an unchanged organization writes one
  object, the scope's status (its `lastSyncTime` moves). When free IPs move, exactly the
  subnets that moved and their VPCs are written; the test asserts both.
- **Throttling costs once per backoff, not per sync.** The 40 throttled targets added 160 s to
  the sync they were throttled in; the 360 healthy ones were all synced in that same pass
  (asserted). The throttled ones were then retried on their own after their backoff (16 s for
  all 40), and a full sync leaves out a target still waiting. With the SDK's worst-case 30 s of
  backoff the same sync would take about 7.5 minutes, three quarters of the interval — the
  headroom the guidance keeps is used up by one bad sync, not exceeded.
- **Goroutines stay flat**: at most 60 in the whole process, the same as in a run with 20
  targets; discovery never has more than `discovery.concurrency` in flight.
- **CPU is not the limit**: 13.5 s of CPU for the heaviest sync, at most about 0.3 cores while
  the writes run, inside the chart's 500m limit.
- **Memory is**: see [Memory](#memory). The chart's old 256Mi limit left too little room.

So the guidance holds for the steady state: 400 targets per instance with the defaults,
full sync at about a quarter of the interval. It holds with less margin than it claimed: the
first sync takes a third of the interval, a tenth of the accounts throttled takes half. With
throttling common in your organization, or a slow control plane, go to concurrency 6–8 for
400 targets rather than waiting for syncs to run long.

To run it yourself, `make test-scale`; `SCALE_ACCOUNTS`, `SCALE_REGIONS`,
`SCALE_CONCURRENCY`, `SCALE_CALL_LATENCY`, `SCALE_THROTTLED_FRACTION`, `SCALE_THROTTLED_HOLD`
and `SCALE_CHURN_FRACTION` change the shape, and `SCALE_HEAP_PROFILE=<file>` writes a heap
profile of the steady state.

## API-call cost of a resync

### EC2, per target, per full sync

From `DiscoverTarget` (`internal/cloud/aws/discover.go`), with `M` = VPCs matching the tag
selector, `U` = VPCs it leaves out, and 100 VPC IDs per filter (`maxFilterValues`):

| Call | Requests |
|---|---|
| `DescribeVpcs` | 1 (+1 per extra page) |
| `DescribeSubnets` for unmanaged VPCs | `ceil(U / 100)` — only when `discoverUnmanaged` is on and `U > 0` |
| `DescribeRouteTables` | `ceil(M / 100)`, or 1 when the scope has no tag selector |
| `DescribeSubnets` | `ceil(M / 100)`, or 1 when the scope has no tag selector |

So a typical target — fewer than 100 managed and fewer than 100 unmanaged VPCs, no
pagination — costs:

- **4 requests** per full sync with `discoverUnmanaged: true` (the 0.3.x default),
- **3 requests** with it off, or with no tag selector at all.

Pagination adds one request per extra page; these calls return up to the EC2 page maximum
(1000 items) per page, so it only matters for accounts with thousands of subnets.

At the default 10-minute interval that is 144 syncs per day:

```
EC2 requests/day ≈ 144 × 4 × targets      # 576 per target per day
```

200 targets ≈ 115k Describe calls per day, spread over 200 separate per-account, per-region
rate limits: under 600 per day for any one of them. If a target is throttled anyway, something
else is using that account's budget; the operator backs that target off on its own (see
[failure modes](failure-modes.md#an-account-that-aws-keeps-throttling)). Lowering
`discovery.concurrency` does not help it: concurrency spreads over different accounts, not
more requests at the same one.

Throttling adds requests, not just time: every throttled attempt is retried, up to 5 attempts
per call, and each one counts in `hs_api_throttled_total`.

Event-driven partial syncs cost the same per target, but only for the targets an event named,
after a `--aws-events-debounce` window (10s).

### STS

`sts:AssumeRole` once per `roleARN|externalID`, cached for the life of the credentials
(`aws.NewCredentialsCache`) — in practice about one call per account per session lifetime
(an hour by default), not one per sync. `sts:GetCallerIdentity` is called at most once per
process, and only for an account configured without a `roleARN`.

### SQS (only with events enabled, only on the leader)

Long polling with `WaitTimeSeconds: 20` and `MaxNumberOfMessages: 10`: up to 3
`ReceiveMessage` calls per minute, about 4,300 per day, plus one `DeleteMessageBatch` per
non-empty batch. This is a steady, billable baseline independent of how much changes.

### Writes

Only on the opt-in paths: one `CreateSubnet` per allocation (plus one
`ModifySubnetAttribute` if `mapPublicIPOnLaunch`, plus one `AssociateRouteTable` if
`routeTableID` is set), and one `CreateTags` per `ResourceImport`. An import that already
reached `Applied` makes no call at all on later reconciles.

### Kubernetes API

Usually the real ceiling, because it grows with the size of the inventory rather than with the
number of accounts. Per sync of a scope:

- uncached `LIST`s of the scope's `Subnet`s and `VPC`s — the controllers use
  `mgr.GetAPIReader()`, so these go to the API server, not the informer cache (that is
  deliberate: objects created earlier in the same sync must be visible);
- one more `LIST` pair per target inside `deleteGone`;
- one status update per object whose status actually changed — the update is skipped when the
  new status is deep-equal to the old one, so a quiet organization writes almost nothing (one
  write per sync at 400 targets, measured), but free-IP counts move constantly, so expect a
  status write for most busy subnets, and their VPCs, on every full sync;
- `updateOverlaps` compares every VPC of the scope against every other: O(V²) prefix
  comparisons per sync. Fine at hundreds of VPCs, noticeable at tens of thousands.

For a scope with 10,000 subnets on a 10-minute interval, that is on the order of 10,000
status writes per resync in the worst case. If etcd complains, raise `resyncInterval` and let
EventBridge carry the freshness.

## Memory

**What the chart asks for** (`charts/subnet-operator/values.yaml`): 128Mi request,
512Mi limit, 50m/500m CPU. The limit was 256Mi until the scale test measured the documented
capacity close to it.

**What actually holds memory:**

1. the controller-runtime informer cache: every `NetworkScope`, `VPC`, `Subnet`,
   `SubnetClaim` and `ResourceImport` in the cluster, with full status including the complete
   tag map of every resource;
2. the metrics: every subnet has its own `hs_subnet_*` series with a dozen labels, and
   those label sets are the largest single item in a heap profile of the scale test: three per
   subnet (one for a subnet without IPv4), about 15,600 at the documented capacity, plus one
   `hs_network_cidr_overlaps` per network. Per account/region target there are a few more:
   `hs_target_up`, `hs_target_throttled` and `hs_target_sync_errors_total` (3),
   `hs_unmanaged_resources` and `hs_unmanaged_resources_total` for each `kind` (4), and in a scope that runs the auto-import policy `hs_auto_imports_total` for
   each of its four results, at 0 until something is counted (4) — at most 11 per target, 4,400
   at 400 targets. `hs_api_throttled_total` adds one per throttled operation;
3. the uncached lists each sync decodes (roughly a second copy of the inventory, transient);
4. the `inventory.Snapshot` of **every** target of the sync: discovery finishes for all
   targets before the first object is written, so all snapshots are held until the sync
   ends, not just one per discovery slot;
5. the small fixed caches: `CreatorCache` (512 entries, 24h TTL) and `seenUnmanaged`, which
   holds one resource ID string per unmanaged resource ever seen, per scope.

Measured at the documented capacity (1,400 VPCs and 5,205 subnets, 400 targets; see
[above](#measured-at-the-documented-capacity)): **about 72 MiB of live heap** after a sync,
about 11 KiB per mirrored object, and **up to 175 MiB of heap in use and 234 MiB resident** at
the peak of a sync, because the garbage collector lets the heap grow to about twice the live
size between collections. That is too close to a 256Mi limit, which is why the chart now
defaults to 512Mi. Setting `GOMEMLIMIT` below the peak does not help: at 200MiB the process
still peaked at 223 MiB resident and the syncs took twice as long, the collector running
almost continuously.

Scaled by the measured 11 KiB per object:

| VPCs + subnets in the instance | Live heap | Peak resident | Against the 512Mi default |
|---|---|---|---|
| ~1,000 | ~25 MiB | ~80 MiB | generous |
| ~6,600 (400 targets, measured) | 72 MiB | 234 MiB | fine |
| ~15,000 | ~170 MiB | ~500 MiB | too close: raise the limit to 1Gi |
| ~50,000 | ~550 MiB | 1.2–1.8 GiB | raise the limit to 2Gi, set `GOMEMLIMIT`, measure |

The rows other than the measured one are extrapolated. Before committing to a large rollout,
measure your own:

```sh
kubectl -n subnet-operator-system top pod
```

```promql
process_resident_memory_bytes{job=~".*subnet-operator.*"}
go_memstats_heap_inuse_bytes{job=~".*subnet-operator.*"}
```

Both Go metrics are on the operator's own metrics endpoint, next to the `hs_*` ones. If
you raise the memory limit, consider setting `GOMEMLIMIT` through `extraEnv` to roughly 80% of
it so the garbage collector works with the cgroup rather than against it.

## Other ceilings worth knowing

| Thing | Limit | Where it comes from |
|---|---|---|
| VPC IDs per EC2 filter | 100 per request, chunked automatically | `maxFilterValues` |
| Resync interval | floor of 1 minute, whatever the spec says | `minResyncInterval` |
| Attempts of one EC2 call | 5, backoff capped at 20 s, adaptive rate limit per account/region | `retryMaxAttempts`, `retryMaxBackoff` |
| Backoff of a throttled target | 1 minute, doubling to 30 minutes, jittered down to half | `throttleBackoffBase`, `throttleBackoffMax` |
| Creator attribution | 512 resources, 24h | `creatorCacheSize`, `creatorCacheTTL` |
| Pending change events | 1024 buffered scope notifications | `changesChannel` |
| `ResourceImport` object name | truncated to 253 characters | `importName` |
| Replicas | one active, always | leader election; `replicaCount > 1` only buys failover |
