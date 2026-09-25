# Runbook

One entry per alert in `charts/subnet-operator/templates/prometheusrule.yaml`
(`prometheusRule.enabled=true`). The metrics behind them are defined in
`internal/metrics/metrics.go` and filled once per sync by `metrics.SetScope`.

Two facts worth knowing before reading any entry:

- **Every inventory gauge is rebuilt from scratch on each sync** (`forgetSeries` +
  `SetScope`). A series that disappears means "the operator no longer reports this", not
  "the value is zero".
- **Free-IP counts are not event-driven.** `internal/cloud/aws/events/events.go` deliberately ignores
  ENI churn, so `hs_subnet_available_ips` is only as fresh as the last full resync
  (`resyncInterval`, 10m by default).

Every metric here carries a `provider` label (`aws`), and names a network `network_id` and an
availability zone `zone`, so alerts carry those labels too. 0.9 renamed the `hs_aws_` metrics
of 0.8 (with `vpc_id` and `az`) and the alert `VPCCIDROverlap`, and no longer exports the old
names; [upgrades.md](upgrades.md#upgrading-from-08-to-09) has the mapping for rules, routes and
silences of your own.

Conventions used below: `NS` is the release namespace
(`subnet-operator-system` by default), `<fullname>` the name of the release's objects
(`subnet-operator` for a release called `subnet-operator`; `<release>-subnet-operator` for
others, and `<release>-aws-subnet-operator` before 0.8), `<scope>` a `NetworkScope` name.

| Alert | Severity | Pager? |
|---|---|---|
| [SubnetOperatorDown](#subnetoperatordown) | critical | yes |
| [SubnetFull](#subnetfull) | critical | yes |
| [SubnetNearlyFull](#subnetnearlyfull) | warning | ticket |
| [SubnetInventoryTargetDown](#subnetinventorytargetdown) | warning | ticket |
| [SubnetInventoryTargetThrottled](#subnetinventorytargetthrottled) | warning | ticket |
| [SubnetInventoryStale](#subnetinventorystale) | warning | ticket |
| [NetworkCIDROverlap](#networkcidroverlap) | warning | ticket |
| [UnmanagedNetworkResource](#unmanagednetworkresource) | warning | ticket |
| [SubnetClaimNotReady](#subnetclaimnotready) | warning | ticket |
| [ResourceImportNotSettled](#resourceimportnotsettled) | warning | ticket |
| [AutoImportedResources](#autoimportedresources) | info | no |

---

## SubnetOperatorDown

```promql
absent(up{job="<fullname>-metrics", namespace="NS"} == 1)
```
`for: 10m` (`prometheusRule.operatorDown.for`), severity `critical`.

**What it means.** No replica of the operator has been scraped successfully for ten minutes.
The inventory is not being maintained, the SQS queue is not being drained, and — the reason
this alert exists — **none of the other alerts can fire**: they are all computed from the
operator's own metrics, and when the operator is gone those series stop instead of turning
bad. A dead operator used to look exactly like a healthy estate.

It asks for *no* replica, not fewer: with two, one dying is not an outage, because the other
takes the leader-election lease and carries on.

**When it is rendered.** Only when there is something to evaluate it against: with the chart's
own `ServiceMonitor` (`metrics.serviceMonitor.enabled`), or with
`prometheusRule.operatorDown.job` set to the job your own scrape config uses. Without either it
is left out on purpose — `up` for that job would never exist and the alert would fire forever.

**Confirm.**

```sh
kubectl -n $NS get pods -l app.kubernetes.io/name=subnet-operator
kubectl -n $NS describe pods -l app.kubernetes.io/name=subnet-operator | tail -30
kubectl -n $NS logs deployment/<fullname> -c manager --previous --tail=100
```

Then, in Prometheus, `up{namespace="NS"}` — a target that exists but reports 0 means the pods
run and the scrape fails, which is a different problem from pods that do not run.

**Do.**

1. **No pods, or pods Pending.** Scaled to zero, evicted, or unschedulable — the topology
   spread and the disruption budget can leave both replicas waiting for a node. `describe`
   says which.
2. **CrashLoopBackOff.** The previous container's log says why. The usual causes: the webhook
   certificate Secret is missing and `--webhook-cert-path` was set explicitly (without it the
   operator carries on with webhooks off), or the AWS credentials cannot be loaded at all.
3. **Pods Running but not Ready, and the log says `aws.hypersurgery/v1alpha1 object(s) were
   never migrated`.** Since 0.9 the operator does not start its controllers next to an
   old-group object 0.8 never migrated, and a pod that is not ready is not scraped through the
   Service. The log and a `MigrationPending` Event on each object name them; the
   [upgrade guide](upgrades.md#objects-08-never-migrated) has what to do.
4. **Pods Running, scrape failing.** The metrics endpoint is HTTPS with authn/authz by default
   (`metrics.secure`). A rotated serving certificate, a changed `ServiceMonitor`, or a
   NetworkPolicy that does not allow the Prometheus namespace in on the metrics port all
   produce this. Fix the scrape; the operator itself is fine.
5. Nothing in AWS is touched while the operator is down, and nothing is lost: the next
   reconcile after it comes back is a full sync.

**Safe to ignore when.** You scaled the operator to zero on purpose. Silence it for the
duration rather than disabling it — the next time it fires will not be on purpose.

---

## SubnetFull

```promql
hs_subnet_available_ips == 0
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

**IPv6-only subnets do not fire this.** They used to — zero usable and zero free IPv4 addresses
read as full — so the operator no longer exports IPv4 capacity for a subnet that has no IPv4
CIDR. No silence is needed for them.

**Safe to ignore when.**

- The subnet is a deliberately packed fixed-size subnet (a `/28` for VPC endpoints, a
  transit subnet) that nobody will ever place another ENI in.

There is no per-subnet exception mechanism in the rule; exceptions belong in Alertmanager,
keyed on `subnet_id`, `tier` or `owner`, all of which are metric labels.

---

## SubnetNearlyFull

```promql
1 - hs_subnet_available_ips / hs_subnet_total_ips >= 0.85   # prometheusRule.thresholds.subnetUsedRatio
```
`for: 15m`, severity `warning`.

**What it means.** Less than 15% of the subnet's usable IPv4 addresses are free.
`hs_subnet_total_ips` is *usable* addresses — the CIDR size minus the five AWS reserves —
so the ratio matches what you would compute by hand, and `status.utilizationPercent` on the
`Subnet` object is the same number rounded down.

**Confirm.** As for `SubnetFull`. The fullest subnets are also the second panel row of the
shipped Grafana dashboard.

```promql
topk(20, 1 - hs_subnet_available_ips / hs_subnet_total_ips)
```

**Do.** Plan capacity before it becomes `SubnetFull`: allocate the next subnet now
(`SubnetClaim` with `mode: Allocate` gives you a reserved CIDR without touching AWS), or tell
the owner to clean up. Raising `prometheusRule.thresholds.subnetUsedRatio` is a legitimate
answer for an organization that runs its subnets hot on purpose.

**Safe to ignore when.** Small subnets are structurally "nearly full": a `/28` has 11 usable
addresses, so three ENIs put it past 0.85. If your `hs/tier` convention marks these, silence
them by `tier`.

**Be aware.** IPv6-only subnets export no IPv4 capacity at all, so this rule has nothing to
evaluate for them — correctly, since there is no IPv4 space to run out of. AWS does not allow an
IPv4 subnet smaller than `/28`, so `total_ips` is at least 11 for every series that exists and
the ratio is always defined.

---

## SubnetInventoryTargetDown

```promql
hs_target_up == 0
```
`for: 15m`, severity `warning`.

**What it means.** The last discovery of one account/region pair failed for a reason other than
throttling. The gauge is set from the per-target status (`metricTargets` →
`TargetStatus.Error != ""`), so it is exactly as truthful as `kubectl get nscope <scope> -o
yaml`. A target that AWS throttled answered, only not fast enough: it keeps `hs_target_up`
at 1 and has [its own alert](#subnetinventorytargetthrottled). The inventory for that target is
**stale, not gone** — see [failure modes](failure-modes.md#a-spoke-account-whose-role-cannot-be-assumed).

**Confirm.**

```sh
kubectl get nscope <scope> -o jsonpath='{range .status.targets[*]}{.account}/{.region}{"\t"}{.error}{"\n"}{end}'
kubectl get nscope <scope> -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}'
kubectl -n $NS logs deployment/subnet-operator -c manager | grep "discovery failed"
```

**Common causes, in the order they are worth checking.** The error text is produced by
`internal/cloud/aws/discoverer.go` and tells you which one it is:

| Error text | Cause | Fix |
|---|---|---|
| `roleARN … belongs to account X, not Y` | `accounts[].aws.roleARN` and `accounts[].id` disagree | fix the `NetworkScope` |
| `account X has no roleARN and the operator runs in account Y` | cross-account target without a role | add `aws.roleARN` |
| `AccessDenied` on `sts:AssumeRole` | trust policy, or a missing/wrong `externalID` | redeploy the spoke role StackSet (`deploy/iam/spoke-readonly-role.cfn.yaml`) |
| `UnauthorizedOperation` on `DescribeVpcs` | the spoke role lost `ec2:Describe*` | fix the role policy |
| `AuthFailure` / endpoint errors | region not enabled in that account | remove the region from the scope, or enable it |
| `throttled by the cloud API: …` | not this alert: see [SubnetInventoryTargetThrottled](#subnetinventorytargetthrottled) | — |

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

## SubnetInventoryTargetThrottled

```promql
hs_target_throttled == 1
```
`for: 30m` (`prometheusRule.thresholds.throttledFor`), severity `warning`.

**What it means.** EC2 keeps answering one account/region pair with `RequestLimitExceeded` (or
another throttling code), and has done so on every attempt for the last 30 minutes. The
account is reachable — credentials and permissions work — but its inventory is stale: the
`VPC`/`Subnet` objects and `status.targets[]` keep the last good numbers and `lastSyncTime`,
exactly as for an unreachable target.

EC2 rate-limits per account and per region, with one budget shared by everything that calls
the API there: Terraform, autoscalers, Karpenter, security scanners, the console. Discovery
itself sends one account/region at most one request at a time (the three or four calls of a
target are sequential), so it is rarely the cause on its own; it is the caller that gives way.

**How the operator gives way**, from the inside out:

1. Every EC2 call uses the SDK's adaptive retry mode: up to 5 attempts with exponential
   backoff capped at 20 s, and a client-side rate limiter that slows the operator's
   requests to that account/region down after a throttle and remembers it across syncs
   (`internal/cloud/aws/discoverer.go`). Each throttled attempt counts in
   `hs_api_throttled_total`, whether or not a retry then got through.
2. When a discovery is still throttled after those attempts, the target is **backed off**
   (`internal/controller/backoff.go`): it is left out of full syncs and ignores change events,
   and is retried on its own after about 1 minute, then 2, 4, 8, 16, and every 30 minutes at
   most. Each delay is jittered between half and all of it, so targets throttled together do
   not come back together.
3. The first discovery that gets through resets the backoff; the target is back on the scope's
   normal cadence, `hs_target_throttled` drops to 0 and the alert resolves on its own.

**Confirm.**

```sh
kubectl get nscope <scope> -o jsonpath='{.status.conditions[?(@.type=="Ready")].message}'
kubectl get nscope <scope> -o jsonpath='{range .status.targets[*]}{.account}/{.region}{"\t"}{.error}{"\n"}{end}'
kubectl describe nscope <scope>            # TargetThrottled events carry the next attempt time
```

The `Ready` condition has reason `Throttled` when throttling is all that is wrong (`SyncFailed`
wins when some target is also unreachable). To see which calls are throttled and how hard:

```promql
sum by (account, region, operation) (rate(hs_api_throttled_total[15m]))
```

**Do.**

1. Find what else is calling EC2 in that account and region — CloudTrail shows the callers,
   including the ones being throttled alongside the operator (their events carry a
   `RequestLimitExceeded` error code). A runaway script or a scanner with no backoff is the
   usual culprit.
2. If the account is just busy, give the operator less to do there: raise the scope's
   `resyncInterval` (and rely on EC2 change events for freshness), or move the account to a
   scope with a longer interval.
3. If many accounts are throttled at the same time, the problem is not one account: check the
   instance's size against the [capacity guidance](limits.md#accounts-and-regions-per-instance).
   Lowering `discovery.concurrency` does **not** help a single throttled account — every
   account/region has its own budget and discovery never sends it more than one request at a
   time.
4. AWS can raise an account's EC2 API rate limits through a support case, if the load is
   legitimate.

**The operator does not** retry a throttled target in a tight loop, report it unreachable,
delete or zero its objects, or touch AWS in any way beyond the reads it already makes.

**Safe to ignore when.** A known batch job (a large Terraform apply, a migration) is hammering
the account and will end; the alert resolves by itself once a retry gets through. After a
restart the backoff starts empty, so each throttled target costs one throttled discovery at the
next full sync before it is backed off again.

---

## SubnetInventoryStale

```promql
time() - hs_scope_last_sync_timestamp_seconds > 3600   # prometheusRule.thresholds.staleSyncSeconds
```
`for: 10m`, severity `warning`.

**What it means.** The scope has not completed a reconcile for an hour. The timestamp is set
at the very end of a successful reconcile, so staleness means reconciles are *failing* (a
Kubernetes API error returns before `SetScope`), the controller is wedged, or nothing is
triggering it. AWS errors alone do **not** cause this: a failed target is recorded and the
sync still completes.

**Confirm.**

```sh
kubectl get nscope                       # "Last sync" column
kubectl -n $NS get pods
kubectl -n $NS get lease                        # leader election, if enabled
kubectl -n $NS logs deployment/subnet-operator -c manager --tail=200
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

**Blind spot, and what covers it.** If the operator is **gone** (crash-loop, evicted, scaled
to zero) the series disappears and this alert cannot fire — it is a comparison against a metric
that no longer exists. [SubnetOperatorDown](#subnetoperatordown) is the alert for that case;
`hack/alerts/alerts_test.yaml` pins both halves, that this one stays silent and that one fires.

**Safe to ignore when.** You have just scaled the operator down on purpose, or during a
cluster upgrade that drains the node — expect it back within a resync interval.

---

## NetworkCIDROverlap

```promql
hs_network_cidr_overlaps > 0
```
`for: 30m`, severity `warning`.

Called `VPCCIDROverlap` up to 0.8: routes, inhibitions and silences that match on the old
`alertname` need the new one ([upgrades.md](upgrades.md#upgrading-from-08-to-09)). It covers
networks of every provider. Labels: `provider`, `scope`, `account`, `region`, `network_id`,
`name`, `owner`, `env`.

**What it means.** At least one other network (a VPC on AWS) **in the same `NetworkScope`**
has a CIDR that overlaps this one. Overlaps are computed in `updateOverlaps` across every
`Network` object of the
scope — across accounts and regions — using `inventory.Overlaps`, which compares all
associated IPv4 and IPv6 prefixes. Overlapping ranges cannot be joined by VPC peering and
break Transit Gateway routing.

**Confirm.**

```sh
kubectl get hsnet <network-id> -o jsonpath='{.status.cidrBlocks}{"\n"}{.status.overlapsWith}{"\n"}'
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
the same `10.0.0.0/16` by convention and are never peered. Silence by `network_id` or `env`
(`vpc_id` before 0.9).

---

## UnmanagedNetworkResource

```promql
increase(hs_unmanaged_resources_total[30m]) > 0
```
`for: 10m`, severity `warning`, `runbook_url: https://hypersurgery.dev/docs/#unmanaged`.
Labels: `provider`, `scope`, `account`, `region`, `kind` (`network` or `subnet`).

It fires ten minutes after a resource first turns up and resolves about half an hour after.
Up to 0.8 the window was `[10m]`, as long as the `for`, and one new resource — the usual case —
raised the increase for one evaluation less than the `for` needed, so the alert never fired
for it; only a steady stream of new resources did. The counter also exists at zero from a
target's first sync now, so that the first resource after a restart is a rise Prometheus can
see, not a series that starts at one.

**What it means.** A VPC or subnet without the managed tag was seen **for the first time**.
The counter rises once per resource ID, not once per resync: `internal/metrics/metrics.go`
keeps a `seenUnmanaged` set per scope for exactly that reason, and each sync writes the current
unmanaged IDs to the scope's status (`status.targets[].unmanagedIDs`) so the set survives the
process. Somebody created network infrastructure that nobody has taken responsibility for.

**Confirm.**

```sh
kubectl get nscope                                     # UNMANAGED column
kubectl get nscope <scope> -o jsonpath='{range .status.targets[*]}{.account}/{.region}{"\t"}{.unmanagedNetworks}{"\t"}{.unmanagedSubnets}{"\n"}{end}'
```

```promql
hs_unmanaged_resources{account="…",region="…"}   # how many there are right now
```

The identities are not in the metrics (only counts, by `kind`). The scope's status has them,
and the dashboard app lists them with an Import button (through `kubectl proxy`, or the chart's
`dashboard.enabled=true`; see the chart README):

```sh
kubectl get nscope <scope> -o jsonpath='{range .status.targets[*]}{.account}/{.region}{"\t"}{.unmanagedIDs}{"\n"}{end}'
```

Or find them in AWS:

```sh
aws ec2 describe-subnets --region <region> \
  --query 'Subnets[?!not_null(Tags[?Key==`hs/managed`])].[SubnetId,VpcId,CidrBlock]' --output table
```

**Do.** Give it an owner. Either import it by hand:

```sh
kubectl apply -f examples/06-resource-import.yaml   # edit resourceID, account, region, tags
kubectl get resourceimports.network.hypersurgery.dev -A
```

or add a rule to `spec.autoImport` on the scope so the next one is handled without a human
(`examples/07-auto-import-policy.yaml`). An import applies exactly the tags it names with one
`ec2:CreateTags` call and changes nothing else; the resource appears in the inventory on the
resync the import triggers.

If the import sits in `Pending`, the reason field says which gate stopped it:
`WritesDisabled` (no `--enable-writes`) or `NoWriteRole` (no `writeRoleARN` for that account —
a role holding only `ec2:CreateTags` is enough).

**A restart or a change of leader is quiet.** The new process starts from the IDs the previous
sync wrote to the scope's status, so resources that were already known do not count again. A
resource created *while the operator was down* is not in that list, so it still fires — which is
the point of the alert. It used to be the other way round: the set lived only in memory, and
every restart reported every known unmanaged resource as new at once.

A scope that is deleted and recreated starts with an empty status, so its first sync counts
everything unmanaged as new, exactly as a first install does.

**Safe to ignore when.** The resources are known and deliberately unmanaged — default VPCs,
another team's account. They keep being *counted* in `hs_unmanaged_resources` (that gauge
is a current census, by design), but the counter behind this alert will not rise for them
again, across restarts too. If you have an auto-import policy, a `skip` rule by tag or
by creating principal is the durable way to say "not ours".

---

## SubnetClaimNotReady

```promql
hs_subnet_claim_ready == 0
```
`for: 30m` (`prometheusRule.thresholds.notReadyFor`), severity `warning`. Labels: `provider`
(the scope's; empty while the scope does not exist), `namespace`, `name`, `reason` — the
reason of the claim's `Ready` condition.

**What it means.** Somebody asked for subnets and has not got them for half an hour. The gauge
is written from the status the controller has just saved, so its reason and the object's
always agree.

**Confirm.**

```sh
kubectl describe subnetclaims.network.hypersurgery.dev -n <namespace> <name>     # Ready condition: reason and message
kubectl get subnetclaims.network.hypersurgery.dev -n <namespace> <name> -o jsonpath='{.status.allocations}'
```

**Do, by reason.**

- `NoSpace` — the VPC has no free block of the requested size in that AZ. Ask for a smaller
  prefix, another AZ, or add a secondary CIDR to the VPC; the claim retries on its own.
- `WritesDisabled` — the operator runs without `--enable-writes`. Either enable writes with a
  write role for that account, or switch the claim to `mode: Allocate` and let Terraform create.
- `NoWriteRole` / `CreateFailed` — IAM, or AWS refused the call; the message carries the AWS
  error. A CIDR conflict is retried with a new allocation automatically, so a persistent
  `CreateFailed` is something else.
- `ScopeNotFound` / `AccountNotInScope` — the claim points at a scope that does not cover it.
  The admission webhook refuses these at `kubectl apply`, so this only appears if the scope
  changed after the claim was accepted.

**Safe to ignore when.** The claim is deliberately parked — then delete it instead of leaving it
to page; allocations it reserved are released, and no subnet is ever deleted by the operator.

---

## ResourceImportNotSettled

```promql
hs_resource_import_ready == 0
```
`for: 30m`, severity `warning`. Labels: `provider` (the scope's; empty while the scope does
not exist), `namespace`, `name`, `state` (`Pending`, `Failed`), `reason`.

**What it means.** Tags someone asked for — or the auto-import policy asked for — have not
reached AWS for half an hour. A dry run is settled by definition and never fires this.

**Confirm.**

```sh
kubectl describe resourceimports.network.hypersurgery.dev -n <namespace> <name>   # status.error has the AWS message
```

**Do, by reason.** `WritesDisabled` and `NoWriteRole` (state `Pending`), `ScopeNotFound` and
`AccountNotInScope` (state `Failed`) as for claims above. `TagsNotApplied` (state `Failed`) is
AWS refusing `ec2:CreateTags` — usually the write role lacks it, or an SCP denies tagging. A
resource deleted after the import was requested lands here too, with `InvalidSubnetID.NotFound`
or `InvalidVpcID.NotFound` in `status.error`: delete the import as well.

**Also** the outcome side of [AutoImportedResources](#autoimportedresources): that counter
records decisions, this gauge records what happened to them.

---

## AutoImportedResources

```promql
increase(hs_auto_imports_total{result="applied"}[24h]) > 0
```
No `for:`, severity `info`.

**What it means.** In the last 24 hours the auto-import policy created `ResourceImport`
objects in `Apply` mode. It is a digest, not an incident: route it to a chat channel, never to
a pager. The other `result` values are `dryrun`, `skipped` (a `skip` rule matched) and
`no_owner` (no rule could resolve the required tags, so the resource was deliberately left
alone — those show up as `UnmanagedNetworkResource` instead).

All four `result` series exist at zero for every target of a scope that runs the policy (mode
`DryRun` or `Apply`), from its first sync on. That is what lets the first import after a
restart or a change of leader count: a series that first appeared at 1, as it did up to 0.8,
had not increased as far as `increase()` could tell, and the digest left that import out.

**Confirm.**

```sh
kubectl get resourceimports.network.hypersurgery.dev -A -o custom-columns=\
NAME:.metadata.name,RESOURCE:.spec.resourceID,BY:.spec.requestedBy,CREATED-BY:'.metadata.annotations.aws\.hypersurgery/created-by',STATE:.status.state
kubectl get resourceimports.network.hypersurgery.dev <name> -o jsonpath='{.metadata.annotations.network\.hypersurgery\.dev/reason}'
```

`spec.requestedBy` names the CloudTrail principal the tags were derived from, the
`network.hypersurgery.dev/reason` annotation says which rule won, and the
`network.hypersurgery.dev/created-by` annotation should be the operator's own service account — an
import in that list created by anybody else was written by hand, not by the policy.

**Do.** Review that the attribution is right. If a team's resources are being labelled with
the wrong owner, fix `fromCreator` / `accountDefaults` on the scope and correct the tags in
AWS — the operator will not remove a tag it applied (or any other).

**What this metric does not tell you.** It counts *decisions that became objects*, not tags
that reached AWS. An import whose `CreateTags` failed still counted here; its state is
`Failed` with the error in `status.error`, and `hs_resource_import_ready` is the metric for
that outcome — [ResourceImportNotSettled](#resourceimportnotsettled) pages on it.

**Safe to ignore when.** Always, as an alert. The one case worth acting on is seeing
`result="applied"` at all when the policy was supposed to be `Off` or `DryRun`: check
`spec.autoImport.mode` on the scope.

---

## Alerts the chart does not ship

Worth adding locally; the metrics exist, the rules do not. (Operator down used to be the first row here; the chart ships it now as [SubnetOperatorDown](#subnetoperatordown).)

| Condition | Expression |
|---|---|
| A target failing repeatedly rather than once | `increase(hs_target_sync_errors_total[1h]) > 3` (throttled discoveries do not count here) |
| Throttling pressure before anything goes stale | `sum by (account, region) (rate(hs_api_throttled_total[15m])) > 0.05` |
| Subnets missing required tags | `hs_subnet_missing_required_tags > 0` |
| Policy cannot attribute anything | `increase(hs_auto_imports_total{result="no_owner"}[24h]) > 0` |
| Old-group objects nothing migrates (a GitOps tool re-applying an unconverted manifest after 0.9) | `sum(hs_migration_pending_objects) > 0` |

`ResourceImport` and `SubnetClaim` failures used to have no metric at all; they have
[SubnetClaimNotReady](#subnetclaimnotready) and [ResourceImportNotSettled](#resourceimportnotsettled)
now.
