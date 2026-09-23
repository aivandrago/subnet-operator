# Limits

What one instance is expected to carry, and what it costs. The formulas come from the code;
where a number is arithmetic rather than a measurement, it says so. The only sizes ever
exercised in CI are the e2e ones (Kind + Moto, two accounts, one region, a handful of VPCs and
subnets), so treat everything below as an engineering envelope, not a benchmark.

## Accounts and regions per instance

A **target** is one account/region pair: `targets = Σ accounts × regions`
(`expandTargets`). Targets are discovered in parallel up to `--discovery-concurrency`
(`discovery.concurrency`, default 4); the calls *within* one target are sequential.

```
full sync wall clock ≈ ceil(targets / concurrency) × per-target latency
```

Per-target latency against real EC2 is roughly 0.5–2 s for the three or four paginated calls
below, more with pagination or throttling. With the defaults (concurrency 4, 1.5 s per
target):

| Targets | Example | Full sync | Against a 10m `resyncInterval` |
|---|---|---|---|
| 40 | 10 accounts × 4 regions | ~15 s | comfortable |
| 200 | 50 accounts × 4 regions | ~75 s | comfortable |
| 600 | 150 accounts × 4 regions | ~4 min | raise concurrency |
| 1000+ | 250 accounts × 4 regions | ~6 min+ | too close: raise concurrency *and* the interval |

**The envelope to plan for is a few hundred targets per instance** with the shipped defaults.
Beyond that, in this order: raise `discovery.concurrency` (each concurrent target is one
goroutine holding one snapshot — memory grows with it, and so does the risk of EC2
throttling), then raise `resyncInterval` and rely on EventBridge for freshness.

Nothing overlaps or piles up if a sync is slow: controller-runtime reconciles one
`NetworkScope` at a time and the next resync is scheduled *after* the previous one finishes.
The price of a slow sync is staleness, not a thundering herd.

**Two things that do not scale the way you might hope.**

- `MaxConcurrentReconciles` is not set for any controller, so it is 1. Several
  `NetworkScope`s in one instance are reconciled **one after another**, not in parallel.
  Splitting a big scope into several small ones does not buy throughput — raising
  `discovery.concurrency` does.
- There is no flag to make an instance watch only some scopes (no namespace or label
  selector), and the CRDs are cluster-scoped. Two operator installs in one cluster would both
  reconcile every `NetworkScope` and fight over the same `VPC`/`Subnet` objects. Shard across
  clusters, not within one.

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

200 targets ≈ 115k Describe calls per day, spread evenly — far below EC2's per-region rate
limits, but concentrated into bursts of `concurrency` parallel targets, which is what
throttling will hit first. If you see `RequestLimitExceeded` in `status.targets[].error`,
lower `discovery.concurrency` before anything else.

Event-driven partial syncs cost the same per target, but only for the targets an event named,
after a `--events-debounce` window (10s).

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
  new status is deep-equal to the old one, so a quiet organization writes almost nothing, but
  free-IP counts move constantly, so expect a status write for most busy subnets on every
  full sync;
- `updateOverlaps` compares every VPC of the scope against every other: O(V²) prefix
  comparisons per sync. Fine at hundreds of VPCs, noticeable at tens of thousands.

For a scope with 10,000 subnets on a 10-minute interval, that is on the order of 10,000
status writes per resync in the worst case. If etcd complains, raise `resyncInterval` and let
EventBridge carry the freshness.

## Memory

**What the chart asks for** (`charts/aws-subnet-operator/values.yaml`): 128Mi request,
256Mi limit, 50m/500m CPU. That is the starting point, not a measurement.

**What actually holds memory:**

1. the controller-runtime informer cache: every `NetworkScope`, `VPC`, `Subnet`,
   `SubnetClaim` and `ResourceImport` in the cluster, with full status including the complete
   tag map of every resource;
2. the uncached lists each sync decodes (roughly a second copy of the inventory, transient);
3. one `inventory.Snapshot` per target being discovered — up to `discovery.concurrency` at a
   time, holding that target's VPCs and subnets including unmanaged ones;
4. the small fixed caches: `CreatorCache` (512 entries, 24h TTL) and `seenUnmanaged`, which
   holds one resource ID string per unmanaged resource ever seen, per scope.

As arithmetic, a `Subnet` object with its tags is a couple of kilobytes decoded, so:

| Subnets in the scope | Cache + transient list | Verdict against the 256Mi limit |
|---|---|---|
| ~1,000 | under 10 MB | default is generous |
| ~10,000 | ~50–100 MB | default still works, watch it |
| ~50,000 | several hundred MB | raise `resources.limits.memory` to 1Gi and test |

**The honest part:** nobody has measured this against a real organization. The e2e suite runs
the operator inside the chart's defaults in Kind against Moto with a handful of objects and
has never been near the limit, and that is the only evidence that exists. Before committing to
a large rollout, measure your own:

```sh
kubectl -n aws-subnet-operator-system top pod
```

```promql
process_resident_memory_bytes{job=~".*aws-subnet-operator.*"}
go_memstats_heap_inuse_bytes{job=~".*aws-subnet-operator.*"}
```

Both Go metrics are on the operator's own metrics endpoint, next to the `hs_aws_*` ones. If
you raise the memory limit, consider setting `GOMEMLIMIT` through `extraEnv` to roughly 80% of
it so the garbage collector works with the cgroup rather than against it.

## Other ceilings worth knowing

| Thing | Limit | Where it comes from |
|---|---|---|
| VPC IDs per EC2 filter | 100 per request, chunked automatically | `maxFilterValues` |
| Resync interval | floor of 1 minute, whatever the spec says | `minResyncInterval` |
| Creator attribution | 512 resources, 24h | `creatorCacheSize`, `creatorCacheTTL` |
| Pending change events | 1024 buffered scope notifications | `changesChannel` |
| `ResourceImport` object name | truncated to 253 characters | `importName` |
| Replicas | one active, always | leader election; `replicaCount > 1` only buys failover |
