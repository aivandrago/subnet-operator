# Runbook

One entry per alert in `charts/aws-subnet-operator/templates/prometheusrule.yaml`
(`prometheusRule.enabled=true`). The metrics behind them are defined in
`internal/metrics/metrics.go` and filled once per sync by `metrics.SetScope`.

Two facts worth knowing before reading any entry:

- **Every inventory gauge is rebuilt from scratch on each sync** (`forgetSeries` +
  `SetScope`). A series that disappears means "the operator no longer reports this", not
  "the value is zero".
- **Free-IP counts are not event-driven.** `internal/events/events.go` deliberately ignores
  ENI churn, so `hs_aws_subnet_available_ips` is only as fresh as the last full resync
  (`resyncInterval`, 10m by default).

Conventions used below: `NS` is the release namespace
(`aws-subnet-operator-system` by default), `<scope>` a `NetworkScope` name.

| Alert | Severity | Pager? |
|---|---|---|
| [SubnetFull](#subnetfull) | critical | yes |
| [SubnetNearlyFull](#subnetnearlyfull) | warning | ticket |
| [SubnetInventoryTargetDown](#subnetinventorytargetdown) | warning | ticket |
| [SubnetInventoryStale](#subnetinventorystale) | warning | ticket |
| [VPCCIDROverlap](#vpccidroverlap) | warning | ticket |
| [UnmanagedNetworkResource](#unmanagednetworkresource) | warning | ticket |
| [AutoImportedResources](#autoimportedresources) | info | no |

---

## SubnetFull

```promql
hs_aws_subnet_available_ips == 0
```
`for: 15m`, severity `critical`.

**What it means.** EC2 reported `AvailableIpAddressCount = 0` for this subnet at the last
resync: no ENI, pod, load balancer or endpoint can be placed in it any more. The value comes
straight from `DescribeSubnets` (`internal/cloud/aws/discover.go`) and is copied into
`Subnet.status.availableIPs`.

**Confirm.**

```sh
kubectl get subnet <subnet-id> -o yaml | yq '.status | {cidrBlock, availableIPs, totalIPs, utilizationPercent, availabilityZone, owner}'
aws ec2 describe-subnets --subnet-ids <subnet-id> --query 'Subnets[0].AvailableIpAddressCount'
```

If `status.lastSyncTime` of the owning target is old, the number may simply be stale — check
`SubnetInventoryStale` first.

**Do.** This is a capacity decision, not an operator problem. AWS cannot resize a subnet, so
the options are: free addresses (delete unused ENIs, scale down, remove stale load balancers),
or get another subnet — a `SubnetClaim` in the same VPC and AZ reserves a free CIDR and, with
writes enabled, creates it. Route the page to the team in the `owner` label.

**The operator will not** create, resize or delete anything on its own because of this alert.
It has no autoscaling behaviour of any kind.

**Safe to ignore when.**

- The subnet has no IPv4 CIDR at all (IPv6-only). `UsableIPv4("")` returns 0
  (`internal/inventory/cidr.go`) and EC2 reports 0 free IPv4 addresses, so the alert fires
  permanently and means nothing. Confirm with `status.cidrBlock == ""` /
  `status.totalIPs == 0` and silence by `subnet_id`.
- The subnet is a deliberately packed fixed-size subnet (a `/28` for VPC endpoints, a
  transit subnet) that nobody will ever place another ENI in.

There is no per-subnet exception mechanism in the rule; exceptions belong in Alertmanager,
keyed on `subnet_id`, `tier` or `owner`, all of which are metric labels.

---

## SubnetNearlyFull

```promql
1 - hs_aws_subnet_available_ips / hs_aws_subnet_total_ips >= 0.85   # prometheusRule.thresholds.subnetUsedRatio
```
`for: 15m`, severity `warning`.

**What it means.** Less than 15% of the subnet's usable IPv4 addresses are free.
`hs_aws_subnet_total_ips` is *usable* addresses — the CIDR size minus the five AWS reserves —
so the ratio matches what you would compute by hand, and `status.utilizationPercent` on the
`Subnet` object is the same number rounded down.

**Confirm.** As for `SubnetFull`. The fullest subnets are also the second panel row of the
shipped Grafana dashboard.

```promql
topk(20, 1 - hs_aws_subnet_available_ips / hs_aws_subnet_total_ips)
```

**Do.** Plan capacity before it becomes `SubnetFull`: allocate the next subnet now
(`SubnetClaim` with `mode: Allocate` gives you a reserved CIDR without touching AWS), or tell
the owner to clean up. Raising `prometheusRule.thresholds.subnetUsedRatio` is a legitimate
answer for an organization that runs its subnets hot on purpose.

**Safe to ignore when.** Small subnets are structurally "nearly full": a `/28` has 11 usable
addresses, so three ENIs put it past 0.85. If your `hs/tier` convention marks these, silence
them by `tier`.

**Be aware.** A subnet with `total_ips == 0` (IPv6-only, or a prefix of `/30` or longer) makes
the expression `NaN`, which never crosses the threshold. The absence of this alert is not
proof that a subnet has room; `SubnetFull` is the one that covers those.

---

## SubnetInventoryTargetDown

```promql
hs_aws_target_up == 0
```
`for: 15m`, severity `warning`.

**What it means.** The last discovery of one account/region pair failed. The gauge is set from
the per-target status (`metricTargets` → `TargetStatus.Error != ""`), so it is exactly as
truthful as `kubectl get networkscope <scope> -o yaml`. The inventory for that target is
**stale, not gone** — see [failure modes](failure-modes.md#a-spoke-account-whose-role-cannot-be-assumed).

**Confirm.**

```sh
kubectl get networkscope <scope> -o jsonpath='{range .status.targets[*]}{.account}/{.region}{"\t"}{.error}{"\n"}{end}'
kubectl get networkscope <scope> -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}'
kubectl -n $NS logs deployment/aws-subnet-operator-controller-manager -c manager | grep "discovery failed"
```

**Common causes, in the order they are worth checking.** The error text is produced by
`internal/cloud/aws/discoverer.go` and tells you which one it is:

| Error text | Cause | Fix |
|---|---|---|
| `roleARN … belongs to account X, not Y` | `accounts[].roleARN` and `accounts[].id` disagree | fix the `NetworkScope` |
| `account X has no roleARN and the operator runs in account Y` | cross-account target without a role | add `roleARN` |
| `AccessDenied` on `sts:AssumeRole` | trust policy, or a missing/wrong `externalID` | redeploy the spoke role StackSet (`deploy/iam/spoke-readonly-role.cfn.yaml`) |
| `UnauthorizedOperation` on `DescribeVpcs` | the spoke role lost `ec2:Describe*` | fix the role policy |
| `AuthFailure` / endpoint errors | region not enabled in that account | remove the region from the scope, or enable it |
| `Throttling`, `RequestLimitExceeded` | too many targets against one account | lower `discovery.concurrency`, raise `resyncInterval` (see [limits](limits.md)) |

Credentials are cached per `roleARN|externalID` for the life of the process
(`Discoverer.credentials`), so a role fixed in IAM is picked up on the next credential
refresh, and a restart guarantees it.

**Do.** Fix the access, then wait for the next resync (or `kubectl annotate networkscope
<scope> ops/resync="$(date +%s)" --overwrite` — any spec change forces a full sync; an
annotation alone does **not**, because the controller filters on generation changes. If you
need it now, restart the pod).

**The operator does not** delete the target's `VPC`/`Subnet` objects while discovery fails,
does not zero its numbers, and does not touch AWS. It keeps the last good counts in
`status.targets[]` so a flapping account does not look empty.

**Safe to ignore when.** The account is being decommissioned or suspended — but then remove it
from the scope, which is the honest way to make the alert stop (and see
[failure modes](failure-modes.md#a-region-or-account-dropped-from-a-scope) for what that
deletes).

---

## SubnetInventoryStale

```promql
time() - hs_aws_scope_last_sync_timestamp_seconds > 3600   # prometheusRule.thresholds.staleSyncSeconds
```
`for: 10m`, severity `warning`.

**What it means.** The scope has not completed a reconcile for an hour. The timestamp is set
at the very end of a successful reconcile, so staleness means reconciles are *failing* (a
Kubernetes API error returns before `SetScope`), the controller is wedged, or nothing is
triggering it. AWS errors alone do **not** cause this: a failed target is recorded and the
sync still completes.

**Confirm.**

```sh
kubectl get networkscopes                       # "Last sync" column
kubectl -n $NS get pods
kubectl -n $NS get lease                        # leader election, if enabled
kubectl -n $NS logs deployment/aws-subnet-operator-controller-manager -c manager --tail=200
```

**Do.**

1. Is the pod running and is it the leader? With `leaderElection.enabled` only the leader
   reconciles and only the leader consumes the SQS queue (`Poller.NeedLeaderElection`).
2. Look for repeated errors from `updateStatus`, `updateOverlaps` or `syncTarget` — these are
   Kubernetes API failures (RBAC, admission webhooks, etcd pressure), and they abort the
   reconcile before the timestamp moves.
3. Check the scrape itself: a broken `ServiceMonitor` or metrics certificate makes the series
   stale without the operator being unhealthy.
4. If the process is wedged, restart it. A restart is cheap: the next reconcile is a full sync
   and nothing in AWS depends on operator state.

**Configuration trap.** `staleSyncSeconds` defaults to 3600 while `resyncInterval` defaults to
`10m`. If you raise `resyncInterval` above an hour, raise `staleSyncSeconds` with it or this
alert fires forever.

**Blind spot you should fix yourself.** If the operator is **gone** (crash-loop, evicted,
scaled to zero) the series disappears and this alert cannot fire — it is a comparison against
a metric that no longer exists. The chart ships nothing for that case. Add, next to the chart
rules:

```promql
absent(hs_aws_scope_last_sync_timestamp_seconds) or up{job=~".*aws-subnet-operator.*"} == 0
```

**Safe to ignore when.** You have just scaled the operator down on purpose, or during a
cluster upgrade that drains the node — expect it back within a resync interval.

---

## VPCCIDROverlap

```promql
hs_aws_vpc_cidr_overlaps > 0
```
`for: 30m`, severity `warning`.

**What it means.** At least one other VPC **in the same `NetworkScope`** has a CIDR that
overlaps this one. Overlaps are computed in `updateOverlaps` across every `VPC` object of the
scope — across accounts and regions — using `inventory.Overlaps`, which compares all
associated IPv4 and IPv6 prefixes. Overlapping ranges cannot be joined by VPC peering and
break Transit Gateway routing.

**Confirm.**

```sh
kubectl get vpc <vpc-id> -o jsonpath='{.status.cidrBlocks}{"\n"}{.status.overlapsWith}{"\n"}'
```

`status.overlapsWith` lists the counterparts as `account/region/vpc-id`.

**Do.** Decide which side renumbers, or accept the overlap and keep the two VPCs out of the
same routing domain. The operator reports and never renumbers anything.

**Caveats that change the answer.**

- Overlaps are recomputed from the *objects*, including VPCs of targets whose last discovery
  failed. A stale VPC that no longer exists can therefore keep an overlap alive; check
  `SubnetInventoryTargetDown` for the same scope before chasing it.
- Two separate `NetworkScope`s never see each other's VPCs, so an overlap between them is
  invisible here.

**Safe to ignore when.** The overlap is intentional and isolated — sandbox accounts that use
the same `10.0.0.0/16` by convention and are never peered. Silence by `vpc_id` or `env`.

---

## UnmanagedNetworkResource

```promql
increase(hs_aws_unmanaged_resources_total[10m]) > 0
```
`for: 10m`, severity `warning`, `runbook_url: https://hypersurgery.dev/docs/#unmanaged`.

**What it means.** A VPC or subnet without the managed tag was seen **for the first time**.
The counter rises once per resource ID, not once per resync: `internal/metrics/metrics.go`
keeps a `seenUnmanaged` set per scope for exactly that reason. Somebody created network
infrastructure that nobody has taken responsibility for.

**Confirm.**

```sh
kubectl get networkscopes                                     # UNMANAGED column
kubectl get networkscope <scope> -o jsonpath='{range .status.targets[*]}{.account}/{.region}{"\t"}{.unmanagedVPCs}{"\t"}{.unmanagedSubnets}{"\n"}{end}'
```

```promql
hs_aws_unmanaged_resources{account="…",region="…"}   # how many there are right now
```

The identities are not in the metrics (only counts, by `kind`) — find them in AWS:

```sh
aws ec2 describe-subnets --region <region> \
  --query 'Subnets[?!not_null(Tags[?Key==`hs/managed`])].[SubnetId,VpcId,CidrBlock]' --output table
```

**Do.** Give it an owner. Either import it by hand:

```sh
kubectl apply -f examples/06-resource-import.yaml   # edit resourceID, account, region, tags
kubectl get resourceimports -A
```

or add a rule to `spec.autoImport` on the scope so the next one is handled without a human
(`examples/07-auto-import-policy.yaml`). An import applies exactly the tags it names with one
`ec2:CreateTags` call and changes nothing else; the resource appears in the inventory on the
resync the import triggers.

If the import sits in `Pending`, the reason field says which gate stopped it:
`WritesDisabled` (no `--enable-writes`) or `NoWriteRole` (no `writeRoleARN` for that account —
a role holding only `ec2:CreateTags` is enough).

**Expect a burst after every restart.** `seenUnmanaged` lives in memory. After a restart, a
leader change or a scope deletion, every unmanaged resource is "seen for the first time"
again and the counter climbs from zero, so this alert fires even though nothing new appeared.
Correlate with the pod's start time before treating it as news.

**Safe to ignore when.** The resources are known and deliberately unmanaged — default VPCs,
another team's account. They keep being *counted* in `hs_aws_unmanaged_resources` (that gauge
is a current census, by design), but the counter behind this alert will not rise for them
again until the process restarts. If you have an auto-import policy, a `skip` rule by tag or
by creating principal is the durable way to say "not ours".

---

## AutoImportedResources

```promql
increase(hs_aws_auto_imports_total{result="applied"}[24h]) > 0
```
No `for:`, severity `info`.

**What it means.** In the last 24 hours the auto-import policy created `ResourceImport`
objects in `Apply` mode. It is a digest, not an incident: route it to a chat channel, never to
a pager. The other `result` values are `dryrun`, `skipped` (a `skip` rule matched) and
`no_owner` (no rule could resolve the required tags, so the resource was deliberately left
alone — those show up as `UnmanagedNetworkResource` instead).

**Confirm.**

```sh
kubectl get resourceimports -A -o custom-columns=\
NAME:.metadata.name,RESOURCE:.spec.resourceID,BY:.spec.requestedBy,STATE:.status.state
kubectl get resourceimport <name> -o jsonpath='{.metadata.annotations.aws\.hypersurgery/reason}'
```

`spec.requestedBy` names the CloudTrail principal the tags were derived from, and the
`aws.hypersurgery/reason` annotation says which rule won.

**Do.** Review that the attribution is right. If a team's resources are being labelled with
the wrong owner, fix `fromCreator` / `accountDefaults` on the scope and correct the tags in
AWS — the operator will not remove a tag it applied (or any other).

**What this metric does not tell you.** It counts *decisions that became objects*, not tags
that reached AWS. An import whose `CreateTags` failed still counted here; its state is
`Failed` with the error in `status.error`. There is no metric for that — check
`kubectl get resourceimports -A` if the numbers and reality disagree.

**Safe to ignore when.** Always, as an alert. The one case worth acting on is seeing
`result="applied"` at all when the policy was supposed to be `Off` or `DryRun`: check
`spec.autoImport.mode` on the scope.

---

## Alerts the chart does not ship

Worth adding locally; the metrics exist, the rules do not:

| Condition | Expression |
|---|---|
| Operator gone (see above) | `absent(hs_aws_scope_last_sync_timestamp_seconds)` |
| A target failing repeatedly rather than once | `increase(hs_aws_target_sync_errors_total[1h]) > 3` |
| Subnets missing required tags | `hs_aws_subnet_missing_required_tags > 0` |
| Policy cannot attribute anything | `increase(hs_aws_auto_imports_total{result="no_owner"}[24h]) > 0` |

`ResourceImport` and `SubnetClaim` failures have no metric at all today — only object status.
A claim stuck on `NoSpace` or an import stuck on `TagsNotApplied` will not page anybody.
