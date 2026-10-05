# Runbook

One entry per alert in `charts/subnet-operator/templates/prometheusrule.yaml`
(`prometheusRule.enabled=true`); symptoms without an alert are in the
[troubleshooting index](README.md#troubleshooting-index). The metrics behind them are defined in
`internal/metrics/metrics.go` and filled once per sync by `metrics.SetScope`.

Two facts worth knowing before reading any entry:

- **Every inventory gauge is rebuilt from scratch on each sync** (`forgetSeries` +
  `SetScope`). A series that disappears means "the operator no longer reports this", not
  "the value is zero".
- **Free-IP counts are not event-driven.** `internal/cloud/aws/events/events.go` deliberately ignores
  ENI churn, so `hs_subnet_available_ips` is only as fresh as the last full resync
  (`resyncInterval`, 10m by default).

Every metric here carries a `provider` label (`aws`, `gcp`, `azure`), and names a network `network_id` and an
availability zone `zone`, so alerts carry those labels too. (Coming from 0.8 or older: 0.9
renamed the `hs_aws_` metrics, the `vpc_id` and `az` labels and the alert `VPCCIDROverlap`;
[upgrades.md](upgrades.md#metrics-and-alerts) has the mapping for rules, routes and silences of
your own.)

The alerts are the same for every provider. Where the causes and fixes differ on Google Cloud,
an entry has a subsection of its own, *… on GCP*: a project stands where AWS has an account, a
service account the operator impersonates where AWS has a role it assumes, and Resource Manager
tag bindings where AWS has tags. The GCP guide is [docs/gcp.md](../gcp.md), its permissions are
in the [IAM reference](../reference/iam.md#google-cloud).

The same goes for Azure (from 3.0), in subsections *… on Azure*: a subscription
stands where AWS has an account, a location where AWS has a region, a Microsoft Entra identity
with a federated credential where AWS has a role it assumes, and a subnet's ownership is a tag
on its virtual network, since an Azure subnet cannot carry tags. The Azure guide is
[docs/azure.md](../azure.md), its permissions are in the
[IAM reference](../reference/iam.md#azure). The Azure provider has been tested against fakes of
the Azure APIs, not yet against real subscriptions (#56): error codes quoted below that the
fakes do not produce are marked *to confirm*.

Conventions used below: `NS` is the release namespace
(`subnet-operator-system` by default), `<fullname>` the name of the release's objects
(`subnet-operator` for a release called `subnet-operator`, `<release>-subnet-operator` for
others; a release first installed from the `aws-subnet-operator` chart of 0.7 or older may keep
its old names, see [upgrades](upgrades.md#upgrading-from-07-to-08)), `<scope>` a `NetworkScope`
name.

| Alert | Severity | Pager? |
|---|---|---|
| [SubnetOperatorDown](#subnetoperatordown) | critical | yes |
| [SubnetFull](#subnetfull) | critical | yes |
| [SubnetNearlyFull](#subnetnearlyfull) | warning | ticket |
| [SubnetInventoryTargetDown](#subnetinventorytargetdown) | warning | ticket |
| [SubnetInventoryTargetThrottled](#subnetinventorytargetthrottled) | warning | ticket |
| [SubnetInventoryStale](#subnetinventorystale) | warning | ticket |
| [SubnetInventoryChangeEventsFailing](#subnetinventorychangeeventsfailing) | warning | ticket |
| [NetworkCIDROverlap](#networkcidroverlap) | warning | ticket |
| [NetworkPeeredCIDROverlap](#networkpeeredcidroverlap) | critical | yes |
| [NetworkTagBudgetLow](#networktagbudgetlow) | warning | ticket |
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
   never migrated`.** Only after an upgrade from 0.7 or 0.8: the operator does not start its
   controllers next to an old-group object 0.8 never migrated, and a pod that is not ready is not scraped through the
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
kubectl get hssubnet <subnet-id> -o yaml | yq '.status | {cidrBlock, availableIPs, totalIPs, utilizationPercent, zone, owner}'
aws ec2 describe-subnets --subnet-ids <subnet-id> --query 'Subnets[0].AvailableIpAddressCount'
```

If `status.targets[].lastSyncTime` of the owning scope's target is old, the number may simply be stale — check
`SubnetInventoryStale` first.

**Do.** This is a capacity decision, not an operator problem. AWS cannot resize a subnet, so
the options are: free addresses (delete unused ENIs, scale down, remove stale load balancers),
or get another subnet — a `SubnetClaim` in the same VPC and AZ reserves a free CIDR and, with
writes enabled, creates it. Route the page to the team in the `owner` label.

**The operator will not** create, resize or delete anything on its own because of this alert.
It has no autoscaling behaviour of any kind.

**On GCP** the number is Compute's free-address count of the subnetwork's primary range
(`subnetworks.list` with `views=WITH_UTILIZATION`), capped at the range's size less the 4
addresses Google reserves; a subnetwork Compute reports no utilization for has no
`availableIPs` and no series, rather than a zero. Confirm with
`gcloud compute networks subnets describe <name> --region=<region> --project=<project>`. A
subnetwork can be widened on GCP (`gcloud compute networks subnets expand-ip-range`), which the
operator never does itself. Secondary ranges, such as a GKE cluster's Pod and Service ranges,
are not in this metric: their usage is in `status.gcp.secondaryRanges` of the `Subnet` only,
and no alert covers them in this release.

**On Azure** the number is Resource Manager's usage count of the subnet
(`virtualNetworks/{name}/usages`: its limit less its current value), capped at the subnet's
usable addresses: every IPv4 address prefix it has less the 5 addresses Azure reserves in each.
A subnet the usages leave out counts its usable addresses less its IP configurations
(`status.azure.ipUsageSource` says which of the two it was). Where neither is known, a gateway
subnet or a subnet a service manages (`status.azure.serviceManaged`), there is no `availableIPs`
and no series, rather than a zero. Confirm with
`az network vnet list-usage --ids <virtual network ID>`. The operator never changes an existing
subnet; the usual answer is another subnet in the same virtual network, which a `SubnetClaim`
creates.

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
kubectl describe nscope <scope>            # a TargetUnreachable Warning Event per failed discovery
kubectl -n $NS logs deployment/<fullname> -c manager | grep "discovery failed"
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

**The operator does not** delete the target's `Network`/`Subnet` objects while discovery fails,
does not zero its numbers, and does not touch AWS. It keeps the last good counts in
`status.targets[]` so a flapping account does not look empty.

**Safe to ignore when.** The account is being decommissioned or suspended — but then remove it
from the scope, which is the honest way to make the alert stop (and see
[failure modes](failure-modes.md#a-region-or-account-dropped-from-a-scope) for what that
deletes).

### SubnetInventoryTargetDown on GCP

The target is a project and region; the error text comes from `internal/cloud/gcp`
(`credentials.go`, `discover.go`) with Google's own error after it. A target whose token cannot
be had fails before any Compute call, and is never reported as throttling unless Google
throttled the token request itself.

| Error text | Cause | Fix |
|---|---|---|
| `cannot impersonate service account S: permission denied or no such service account; …` | the operator's own identity may not get tokens of `accounts[].gcp.serviceAccount` (or `writeServiceAccount`), the service account does not exist or is disabled, or the IAM Service Account Credentials API is off in the own identity's project | grant `deploy/gcp/impersonator-role.yaml` (or `roles/iam.serviceAccountTokenCreator`) on that service account to the own identity; `gcloud services enable iamcredentials.googleapis.com` in its project; check the email |
| `cannot impersonate service account S: …` with any other status | the own identity has no working credentials at all (see the next row), or the request failed on the way | as below; a 5xx is retried at the next sync |
| `could not find default credentials`, `metadata: GCE metadata "…" not defined`, a 401 from `sts.googleapis.com` | the operator has no Google identity: the `iam.gke.io/gcp-service-account` annotation is missing or its `roles/iam.workloadIdentityUser` binding is, the node pool runs without the GKE metadata server, the NetworkPolicy blocks it (`providers.gcp.metadataServer`), or the Workload Identity Federation pool does not accept the token (issuer, audience, attribute mapping) | check the chart's `providers.gcp` values against [the GCP guide](../gcp.md#2-the-operators-own-google-identity); `kubectl -n $NS get sa <fullname> -o yaml` shows the annotation |
| `look up project P: … 403` | the read identity lacks `resourcemanager.projects.get` in the project, or the project ID is wrong or deleted (Resource Manager answers 403 for a project the caller cannot see, whether or not it exists) | grant `deploy/gcp/reader-role.yaml` in the project; check `accounts[].id` |
| `look up tagParent projects/T: …` | `spec.gcp.tagParent` names a project the read identity cannot `projects.get` | grant `resourcemanager.projects.get` on that project too, or use `organizations/<number>` |
| `list networks: … 403` / `list subnetworks: … 403` | the reader role is missing in the project, or the Compute Engine API is disabled there (the message then says `has not been used in project … or it is disabled`) | grant the reader role; enable `compute.googleapis.com` |
| `list subnetworks: … 400` naming the region | a region that does not exist | fix `spec.regions` (the webhook checks the shape of a name, not that the region exists) |
| `read network tags: …` / `read subnetwork tags: …` | `compute.networks.listEffectiveTags` / `compute.subnetworks.listEffectiveTags` missing, or the region's Resource Manager endpoint (`<region>-cloudresourcemanager.googleapis.com`) unreachable through a narrowed NetworkPolicy | grant the reader role; add the Google API ranges to `providers.gcp.apiCIDRs` |
| `throttled by the cloud API: …` | not this alert: see [SubnetInventoryTargetThrottled on GCP](#subnetinventorytargetthrottled-on-gcp) | — |

Tokens are cached per service account until five minutes before they expire, and the API clients
per identity for the life of the process, so a grant fixed in IAM is picked up at the next sync
after it has propagated (IAM changes take a few minutes); a restart makes sure. A change to the
operator's own identity (a new annotation, a new credential configuration) needs the pods
restarted, which a `helm upgrade` that changes the values does.

### SubnetInventoryTargetDown on Azure

The target is a subscription and location; the error text comes from `internal/cloud/azure`
(`credentials.go`, `discover.go`) with Microsoft's own error after it. A target whose token
cannot be had fails before any Resource Manager call. When the subscription is read with an
identity of its own (`accounts[].azure.clientID`), a Resource Manager error starts with
`as client <client ID>`, because ARM names the principal by its object ID and the scope names it
by its client ID.

| Error text | Cause | Fix |
|---|---|---|
| `cannot authenticate as client C …: Microsoft Entra ID gave no token for the operator's service account; the identity needs a federated identity credential with issuer I, subject S and audience api://AzureADTokenExchange …: … AADSTS70021` | the identity has no federated identity credential that matches the operator's service account token, or one created in the last few minutes has not propagated | create the credential with exactly the issuer and subject the message names ([`deploy/azure/README.md`](../../deploy/azure/README.md#workload-identity)); a new one is picked up at the next sync |
| the same, with `AADSTS700016` | Entra knows no application with that client ID in the tenant: a typo in `azure.clientID` (or `writeClientID`), or an identity of another tenant without `azure.tenantID` | check the client ID with `az identity show … --query clientId`; for another tenant set `azure.tenantID` and provision the app registration there |
| `cannot authenticate as client C: the operator has no service account token to exchange (AZURE_FEDERATED_TOKEN_FILE is not set); …` | the account names an identity, but the operator runs without Microsoft Entra Workload ID | set `providers.azure.workloadIdentity.enabled` (on AKS the add-on's webhook must be installed; elsewhere `webhook: false`) |
| `cannot authenticate as client C: neither the account (azure.tenantID) nor the operator (AZURE_TENANT_ID) names a tenant; …` | no tenant to request the token from | set `providers.azure.tenantId`, or `azure.tenantID` on the account |
| `no Azure credentials for the operator (Workload Identity, managed identity or environment): …`, or the SDK's `DefaultAzureCredential: failed to acquire a token` | the operator's **own** identity, used for accounts without an `azure` member: no Workload ID, a wrong `workloadIdentity.clientId`, or its federated credential does not match | check the chart's `providers.azure` values against [the Azure guide](../azure.md#2-the-operators-own-azure-identity); `kubectl -n $NS get sa <fullname> -o yaml` shows the annotations, `kubectl -n $NS get pod -o yaml` the `AZURE_*` variables |
| `list virtual networks: … AuthorizationFailed` (HTTP 403) | the read identity has no role assignment that allows `Microsoft.Network/virtualNetworks/read` on the subscription; a new assignment takes a few minutes to apply | assign `deploy/azure/reader-role.json` (or Reader) to the identity on the subscription |
| `list virtual networks: resource group G: … AuthorizationFailed` | the scope names `spec.azure.resourceGroups` and the read identity has no role on that group | assign the reader role on each group the scope names |
| `list virtual networks: resource group G: … ResourceGroupNotFound` | a resource group in `spec.azure.resourceGroups` does not exist in this subscription: a typo, a group that was deleted, or a group that only some subscriptions of the scope have | fix `spec.azure.resourceGroups`; the groups are looked up in every subscription of the scope, so give subscriptions with other groups a scope of their own |
| `list virtual networks: … SubscriptionNotFound` (*to confirm* what ARM answers for a subscription of another tenant) | `accounts[].id` is not a subscription the identity's tenant has | check the subscription ID; a subscription of another tenant needs `azure.tenantID` and an identity provisioned there |
| `list usages of virtual network N: … AuthorizationFailed` | the read identity may list virtual networks but lacks `Microsoft.Network/virtualNetworks/usages/read`, as a hand-made role may | use the reader role of `deploy/azure` |
| `dial tcp …: i/o timeout` to `login.microsoftonline.com` or `management.azure.com` | a narrowed NetworkPolicy does not let the operator out | add the `AzureActiveDirectory` and `AzureResourceManager` ranges to `providers.azure.apiCIDRs` |
| `throttled by the cloud API: …` | not this alert: see [SubnetInventoryTargetThrottled on Azure](#subnetinventorytargetthrottled-on-azure) | — |

Two things that are **not** errors, and so not this alert:

- **A location that does not exist, or has no virtual network.** Resource Manager has no location
  filter: discovery lists the subscription's virtual networks and keeps those in the target's
  location. A misspelt location (`west-europe` is refused by the webhook for its shape,
  `westeuropa` is not) syncs without error and with no networks. `kubectl get nscope <scope> -o
  jsonpath='{.status.targets}'` shows `networks: 0` for it; `az account list-locations -o table`
  has the names.
- **A virtual network the selector leaves out.** Tag names are matched without regard to case
  and values exactly: `hs-managed: "True"` on the network does not match `"true"` in the scope.

One credential is kept per identity for the life of the process, and the SDK caches its tokens
until shortly before they expire and rereads the service account token as the kubelet rotates
it, so a federated credential or a role assignment fixed in Azure is picked up at the next sync
after it has propagated; a restart makes sure. A change to the operator's own identity
(`workloadIdentity.clientId`, the tenant) needs the pods restarted, which a `helm upgrade` that
changes the values does. Other tenants and sovereign clouds (`providers.azure.cloud`) have not
been exercised against real endpoints yet (#56).

---

## SubnetInventoryTargetThrottled

```promql
hs_target_throttled == 1
```
`for: 30m` (`prometheusRule.thresholds.throttledFor`), severity `warning`.

**What it means.** EC2 keeps answering one account/region pair with `RequestLimitExceeded` (or
another throttling code), and has done so on every attempt for the last 30 minutes. The
account is reachable — credentials and permissions work — but its inventory is stale: the
`Network`/`Subnet` objects and `status.targets[]` keep the last good numbers and `lastSyncTime`,
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

### SubnetInventoryTargetThrottled on GCP

Google throttles with HTTP 429, a 403 whose reason is `rateLimitExceeded` or
`userRateLimitExceeded`, or `RESOURCE_EXHAUSTED`; any other 403 is a missing permission and is
never retried as throttling (`internal/cloud/gcp/discoverer.go`, `isThrottle`). Quotas are per
project and per minute, one budget for everything calling the API in that project.

How the operator gives way on GCP, before the per-target backoff above takes over:

1. Every Google API call is tried up to 5 times. Before each attempt the operator waits a delay
   kept per quota bucket — Compute per project and region (`global` for networks), Resource
   Manager per endpoint location — that doubles from 1 s up to a minute while calls are
   throttled and halves as they get through. Every throttled attempt counts in
   `hs_api_throttled_total`, with `operation` naming the call: `networks.list`,
   `subnetworks.list`, `projects.get`, `effectiveTags.list` for discovery; `networks.get`,
   `subnetworks.get`, `subnetworks.insert`, `tagKeys.getNamespaced`, `tagValues.getNamespaced`,
   `tagValues.create`, `tagBindings.create` for writes.
2. A discovery still throttled after that fails with `throttled by the cloud API` and the target
   is backed off exactly as on AWS. A throttled impersonation token request counts too.

Discovery costs one `effectiveTags.list` per network and per subnetwork (eight at a time), so
a project with many subnetworks spends most of its calls on Resource Manager, not Compute. Find
the busy caller in the project's audit logs or on its API dashboard (APIs & Services → Compute
Engine API / Cloud Resource Manager API → Quotas); raising the project's quota, or the scope's
`resyncInterval`, are the levers, as step 2 above describes for AWS.

### SubnetInventoryTargetThrottled on Azure

Azure Resource Manager throttles with HTTP 429 and a `Retry-After`; Microsoft.Network's 429
`RetryableErrorDueToAnotherOperation` and a 429 from Microsoft Entra ID on a token request count
as throttling too. A 403 is a missing role assignment and is never retried as throttling
(`internal/cloud/azure/discoverer.go`, `throttling`). Resource Manager's limits are per
subscription and per principal, so a read identity of its own (`accounts[].azure.clientID`) has a
budget of its own, apart from whatever else reads the subscription as another identity.

How the operator gives way on Azure, before the per-target backoff above takes over:

1. Every Resource Manager call is tried up to 5 times; the SDK's own retries are off, so that
   each throttled attempt is seen and counted. Before each attempt the operator waits a delay
   kept per identity and subscription, apart for reads and for writes, that doubles from 1 s up
   to a minute while calls are throttled, is never shorter than the `Retry-After` ARM asked for,
   and halves as calls get through.
2. It slows down **before** it is throttled: while ARM reports fewer than 25 reads left for the
   subscription (`x-ms-ratelimit-remaining-subscription-reads`), every call waits at least a
   second.
3. Every throttled attempt counts in `hs_api_throttled_total`, with `operation` naming the call:
   `virtualNetworks.listAll`, `virtualNetworks.list` (with `spec.azure.resourceGroups`) and
   `virtualNetworks.listUsage` for discovery; `subnets.get`, `subnets.createOrUpdate`,
   `tags.getAtScope` and `tags.updateAtScope` for writes; the two long-running ones,
   `subnets.createOrUpdate` and `tags.updateAtScope`, also as `….poll` and `….result`.
4. A discovery still throttled after that fails with `throttled by the cloud API` and the target
   is backed off exactly as on AWS.

What discovery costs: Resource Manager has no location filter, so **every location of a
subscription lists all of the subscription's virtual networks** (one paged List All call, or one
List call per resource group the scope names), and then reads the usages of each virtual network
in that location that has subnets, one call each, eight at a time. A subscription with many
locations in the scope and many virtual networks therefore spends its reads on listing the same
networks once per location: list only the locations that have virtual networks, or raise the
scope's `resyncInterval` and rely on [change events](../azure.md#change-events). Find the busy
caller in the subscription's Activity log; a paged listing that is throttled half-way starts
over, since a pager cannot resume.

---

## NetworkTagBudgetLow

```promql
hs_network_tags >= 45
```
`for: 15m` (the threshold is `prometheusRule.thresholds.networkTags`), severity `warning`.
Labels: `provider`, `scope`, `account`, `region`, `network_id`, `name`, `owner`, `env`.

**What it means.** A virtual network carries 45 or more of the 50 tags Azure allows on one
resource. Azure subnets cannot carry tags, so each subnet with ownership of its own has its entry
`hs-subnet-<name>` among the network's tags, next to the network's own. When none is left,
claims and imports of its subnets fail with `TagBudgetExceeded`, and nothing is written half-way.
The metric is exported for Azure networks only.

**Confirm.**

```sh
kubectl get hsnet <object-name> -o jsonpath='{.status.azure}'     # tagCount, subnetOwnershipEntries
az tag list --resource-id <virtual network ID> -o json
```

**Do.** Remove tags the network does not need: entries of subnets that no longer exist
(`hs-subnet-<name>`), and tags of its own that nothing reads. A subnet whose entry only repeats
the network's values needs none (it inherits them). If the network is simply full of subnets
with owners of their own, new subnets go to another virtual network.

**Safe to ignore when.** The network will get no more subnets with owners of their own.

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
kubectl -n $NS logs deployment/<fullname> -c manager --tail=200
```

**Do.**

1. Is the pod running and is it the leader? With `leaderElection.enabled` (the default) only
   the leader reconciles and only the leader consumes the SQS queue
   (`Poller.NeedLeaderElection`).
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

## SubnetInventoryChangeEventsFailing

```promql
increase(hs_change_event_errors_total[15m]) > 3
```
`for: 15m`, severity `warning`.

**What it means.** The operator keeps failing to read the change events of a provider: the SQS
queue on AWS (`--aws-events-queue-url`), the Pub/Sub subscription on GCP
(`--gcp-events-subscription`), the Storage queue on Azure (`--azure-events-queue-url`, where a
message that cannot be deleted counts too). The event source retries on its own, with backoff up to a
minute, so this fires only when reading has failed for about half an hour. Nothing is lost for
good: the inventory still follows the full resyncs (`resyncInterval`, 10m by default), it is
only no longer updated within seconds of a change.

**Confirm.**

```sh
kubectl -n $NS logs deployment/<fullname> -c manager | grep 'failed to receive events'
```

The metric has the provider; `hs_change_events_total` shows whether anything was read before.

**Do.**

1. **GCP, `NotFound`**: the subscription is gone (deleted, or expired: create it with
   `--expiration-period=never`, [deploy/gcp/events.md](../../deploy/gcp/events.md)). Recreate
   it on the same topic; the operator picks it up without a restart.
2. **GCP, `PermissionDenied`**: the operator's own identity lacks
   `pubsub.subscriptions.consume` on the subscription (`roles/pubsub.subscriber`, or
   `deploy/gcp/events-subscriber-role.yaml`). With Workload Identity Federation, check that the
   grant names the identity the operator actually runs as: the impersonated service account
   (`providers.gcp.wif.serviceAccount`) or the federated `principal://`.
3. **GCP, credentials**: "could not find default credentials" or a token exchange error is
   the operator's own identity, which discovery uses too; see
   [SubnetInventoryTargetDown](#subnetinventorytargetdown).
4. **AWS**: `AWS.SimpleQueueService.NonExistentQueue` or `AccessDenied` on the queue; the hub
   stack (`deploy/events/hub-events.cfn.yaml`) and the operator policy's
   `ConsumeChangeEvents` statement (`deploy/iam/operator-policy.json`).
5. **Azure, `QueueNotFound`**: the queue or its storage account is gone; deploy
   `deploy/azure/events/queue.bicep` again ([deploy/azure/events.md](../../deploy/azure/events.md)).
   The operator picks it up without a restart.
6. **Azure, `AuthorizationPermissionMismatch`** on `failed to receive events`: the operator's
   own identity (`providers.azure.workloadIdentity.clientId`, not an account's `azure.clientID`)
   has no data role on the queue. On `failed to delete an event` only: it may read but not
   delete, e.g. Storage Queue Data Reader; it needs **Storage Queue Data Message Processor**.
   A role assignment takes a few minutes to apply.
7. **Azure, credentials**: `cannot authenticate` or `no Azure credentials for the operator` is
   the operator's own identity; see [SubnetInventoryTargetDown](#subnetinventorytargetdown).
8. **Egress**: with `networkPolicy.enabled` and narrowed egress, the operator must reach
   `sqs.<region>.amazonaws.com`, `pubsub.googleapis.com` or
   `<storage account>.queue.core.windows.net` on 443.

**Safe to ignore when.** You removed the event source on purpose but left the flag set; unset
`providers.aws.events.queueUrl`, `providers.gcp.events.subscription` or
`providers.azure.events.queueUrl` instead.

---

## NetworkCIDROverlap

```promql
hs_network_cidr_overlaps > 0
```
`for: 30m`, severity `warning`.

Called `VPCCIDROverlap` up to 0.8: routes, inhibitions and silences carried over from then that
match on the old `alertname` need the new one ([upgrades.md](upgrades.md#metrics-and-alerts)).
It covers networks of every provider. Labels: `provider`, `scope`, `account`, `region`, `network_id`,
`name`, `owner`, `env`.

**What it means.** At least one other network **in the same `NetworkScope`** has a range that
overlaps this one. Overlaps are computed by `scopeOverlaps` (`internal/controller/overlaps.go`)
across every `Network` object of the scope — across accounts, projects and regions — after each
sync. On AWS the ranges compared are the VPCs' IPv4 CIDR blocks. Overlapping ranges cannot be
joined by VPC peering and break Transit Gateway routing.

**Confirm.**

```sh
kubectl get hsnet <network-id> -o jsonpath='{.status.cidrBlocks}{"\n"}{.status.overlapsWith}{"\n"}'
```

`status.overlapsWith` lists the counterparts as `account/region/vpc-id`.

**Do.** Decide which side renumbers, or accept the overlap and keep the two VPCs out of the
same routing domain. The operator reports and never renumbers anything.

**Caveats that change the answer.**

- Overlaps are recomputed from the *objects*, including networks of targets whose last discovery
  failed. A stale network that no longer exists can therefore keep an overlap alive; check
  `SubnetInventoryTargetDown` for the same scope before chasing it.
- Two separate `NetworkScope`s never see each other's networks, so an overlap between them is
  invisible here.

**Safe to ignore when.** The overlap is intentional and isolated — sandbox accounts that use
the same `10.0.0.0/16` by convention and are never peered. Silence by `network_id` or `env`.

### NetworkCIDROverlap on GCP

A VPC network has no range of its own (`status.cidrBlocks` is empty; only a legacy network has
one): its addresses live in its regional subnetworks. So on GCP the ranges compared are the
subnetworks' — primary, secondary (GKE Pod and Service ranges among them) and IPv6 — of every
network of the scope, and two networks overlap when a range of one overlaps a range of the
other, in any region and any project of the scope. Compute already refuses overlaps inside one
network, so what this reports is always between two networks. `network_id` is the network's
resource name (`projects/<project>/global/networks/<name>`) and `region` is empty.

Every overlapping pair is reported, as on AWS, whether the two networks are peered or not; an
overlap between networks that are not peered only matters once someone peers them (or connects
them through Network Connectivity Center or a VPN), and Compute will refuse that peering. When
they are peered, [NetworkPeeredCIDROverlap](#networkpeeredcidroverlap) fires as well and comes
first.

**Confirm.** `status.gcp.overlaps` says, for each other network, whether the two are peered, the
state of the peering, and the first ranges that overlap with the subnetworks they belong to
(`rangeCount` says how many pairs there are):

```sh
kubectl get hsnet -l network.hypersurgery.dev/scope=<scope> \
  -o jsonpath='{range .items[?(@.status.overlapsWith)]}{.spec.id}{"\n"}{range .status.gcp.overlaps[*]}  {.network} peered={.peered} {.peeringState} pairs={.rangeCount}{"\n"}{range .ranges[*]}    {.cidrBlock} ({.subnet}) ~ {.otherCIDRBlock} ({.otherSubnet}){"\n"}{end}{end}{end}'
```

The details list every peered network and the first 50 others by name, with at most five pairs
of ranges each; `status.overlapsWith` and the metric count them all.

**Do.** For networks that are not peered and never will be, nothing is broken: silence by
`network_id` or `env`, as on AWS. Otherwise renumber one side before connecting them. A range
cannot be changed in place — a primary range can only be expanded, a secondary range removed
once nothing uses it — so renumbering means a new subnetwork (or a new secondary range for a
new GKE node pool) and moving the workloads.

**Safe to ignore when.** The networks are auto mode networks, such as each project's `default`
network: every one of them has the same `10.128.0.0/9` subnetworks, so a scope of many
projects reports every `default` network overlapping every other. Keep them out of the scope
with `networkSelector`, or silence by `name="default"`. Private Service Connect subnetworks
(`status.gcp.purpose: PRIVATE_SERVICE_CONNECT`) are only used for NAT on the producer side
and are compared like any other.

### NetworkCIDROverlap on Azure

A virtual network has an address space of its own, so Azure is compared like AWS: two virtual
networks of the scope overlap when an IPv4 address prefix of one (`status.cidrBlocks`) overlaps
one of the other, in any subscription and location of the scope. `network_id` is the virtual
network's resource ID in lowercase, and `status.overlapsWith` names the counterparts as
`<subscription>/<location>/<resource ID>`. Azure refuses to peer two virtual networks whose
address spaces overlap, and an overlap between networks that are never connected (to each other,
or to the same hub or on-premises network) breaks nothing. The operator does not read virtual
network peerings on Azure, so it cannot tell the two cases apart and
[NetworkPeeredCIDROverlap](#networkpeeredcidroverlap) never fires for Azure. Renumbering means
adding a prefix to the address space and moving the subnets; the operator never changes an
address space and holds no permission to (`Microsoft.Network/virtualNetworks/write` is not in
its roles).

---

## NetworkPeeredCIDROverlap

```promql
hs_network_peered_cidr_overlaps > 0
```
`for: 10m`, severity `critical`. Labels: `provider`, `scope`, `account`, `region`, `network_id`,
`name`, `owner`, `env`.

**What it means.** One of the overlaps [NetworkCIDROverlap](#networkcidroverlap) reports is
between two networks that are **peered**: either has a VPC Network Peering with the other,
active or not. It is the overlap that already breaks something, so handle it before the
others. The operator reads peerings on GCP only (from `networks.list`, which it already calls,
so it needs no further permission); the metric is not exported for AWS VPCs or Azure virtual
networks, whose peerings it does not read, and the alert never fires for them.

- A peering in state `INACTIVE` with an overlap cannot become active: one side peered, and
  either the other never did or Compute refused it because of the overlap, or one side deleted
  its peering or its network and something overlapping was created since. Nothing is routed
  between the two networks.
- A peering in state `ACTIVE` with an overlap happens when a range is left out of the exchange
  (privately used public IPv4 ranges, with `exportSubnetRoutesWithPublicIp` off) or the
  overlap came in through a way Compute does not check. Traffic to the overlapping range stays
  in the local network.

**Confirm.**

```sh
kubectl get hsnet -l network.hypersurgery.dev/scope=<scope> \
  -o jsonpath='{range .items[*]}{.metadata.name} {.spec.id}: {.status.gcp.overlaps[?(@.peered==true)].network}{"\n"}{end}'
kubectl get hsnet <object-name> -o jsonpath='{.status.gcp.peerings}'
gcloud compute networks peerings list --network=<name> --project=<project>
```

`status.gcp.peerings` is what Compute reports for the network: each peering's name, the peer
network's resource name, its state and `stateDetails`, Compute's reason.

**Do.** Renumber one side, as for [NetworkCIDROverlap on GCP](#networkcidroverlap-on-gcp), and
then create or re-create the missing half of the peering. If the peering is not wanted, delete
it: the overlap is then reported by `NetworkCIDROverlap` only.

**Caveats that change the answer.**

- Only networks **in the scope** are compared, and only their subnetworks in the scope's
  regions. A peering with a network of a project the scope does not cover is listed in
  `status.gcp.peerings`, but its ranges are not known, so an overlap with it is not reported.
  Put the peered projects, and every region they use, in the same scope.
- Peerings and ranges are as fresh as the last sync of both networks' projects: adding or
  removing a peering is a network change event, and with no event source it shows at the next
  resync.

**Safe to ignore when.** The peering is a leftover nobody uses; delete it instead of silencing.

---

## UnmanagedNetworkResource

```promql
increase(hs_unmanaged_resources_total[30m]) > 0
```
`for: 10m`, severity `warning`.
Labels: `provider`, `scope`, `account`, `region`, `kind` (`network` or `subnet`).

It fires ten minutes after a resource first turns up and resolves about half an hour after.
The window is longer than the `for` on purpose: with a window as long as the `for`, one new
resource — the usual case — raises the increase for one evaluation less than the `for` needs,
and the alert never fires for it (the rule up to 0.8 had that bug). The counter exists at zero
from a target's first sync, so that the first resource after a restart is a rise Prometheus can
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

On Google Cloud, a network or subnetwork without a direct `hs-managed` binding (or whatever the
scope's selector names) is unmanaged; its ID in the status is the resource name,
`projects/<project>/global/networks/<name>` or `projects/<project>/regions/<region>/subnetworks/<name>`:

```sh
# Resource Manager names a network by project number and numeric ID.
gcloud compute networks describe <name> --project=<project> --format='value(id)'
gcloud resource-manager tags bindings list --effective \
  --parent=//compute.googleapis.com/projects/<project number>/global/networks/<network id>
# A subnetwork: .../regions/<region>/subnetworks/<subnetwork id>, with --location=<region>.
```

On Azure, a virtual network whose own tags do not match the scope's selector (`hs-managed`, in
any case; the value exactly) is unmanaged, and so are all of its subnets: the selector is applied
to the network's own tags, never to its subnets' `hs-subnet-<name>` entries. The ID in the status
is the resource ID in lowercase:

```sh
az network vnet list --subscription <subscription> \
  --query "[?tags.\"hs-managed\"==null].{id:id, location:location, prefixes:addressSpace.addressPrefixes}" -o table
az tag list --resource-id <virtual network ID>
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
a role holding only `ec2:CreateTags` is enough). On Azure an import of a virtual network adds
the tags to the network itself with the Tags API and one of a subnet adds them to the subnet's
entry on its network; `NoWriteRole` there is a subscription read through `azure.clientID`
without an `azure.writeClientID`, and Tag Contributor is enough for that identity.

**Who created it is not known on Azure.** The resource events Azure delivers do not tell a create
from an update, so the operator records no creator there and the auto-import policy's
`fromCreator` rules, and `skip` rules by principal, never match an Azure resource (an accepted
limitation of 3.0; the webhook warns when such a scope is applied, and nothing else does). Use
`accountDefaults`, `inheritFromNetwork` and `skip` rules by tag, or import by hand.

**A restart or a change of leader is quiet.** The new process starts from the IDs the previous
sync wrote to the scope's status, so resources that were already known do not count again. A
resource created *while the operator was down* is not in that list, so it still fires — which is
the point of the alert. (Before 0.5 the set lived only in memory, and every restart reported
every known unmanaged resource as new at once.)

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
  changed after the claim was accepted (or the webhooks are off).
- `NamespaceNotAllowed` — the scope's `spec.namespaceSelector` does not select the claim's
  namespace (an unset selector selects none; `{}` selects all). Either the claim belongs in
  another namespace, or the scope's owner adds this one. Reservations the claim already has
  stay; it gets no new ones.
- `NetworkNotFound` — the network (VPC) in `spec.networkID` has not been discovered by that
  scope, or belongs to another account or region: check `kubectl get hsnet`, the scope's
  `networkSelector`, and that the target is healthy.
- `ProviderNotEnabled` / `CreateNotSupported` / `ZonesRequired` / `InvalidPrefixLength` — the
  scope's provider is not one the operator runs (`--providers`), or the claim asks for
  something the provider cannot do. The full table is in
  [failure modes](failure-modes.md#a-claim-that-cannot-be-satisfied).

**On GCP**, besides the reasons above (where `NoWriteRole` means the account has no
`gcp.writeServiceAccount`):

- `TagKeyMissing` — a tag key the subnetwork is created with does not exist under the scope's
  `gcp.tagParent`; the message names it. The operator never creates keys: have someone with
  `roles/resourcemanager.tagAdmin` on the tag parent create it
  ([keys the operator uses](../gcp.md#5-tag-keys-and-values)). Nothing was created.
- `TagValueMissing` — a value does not exist and the scope does not set
  `spec.gcp.createTagValues`; the message names `<parent>/<key>/<value>`. Create it
  (`gcloud resource-manager tags values create <value> --parent=<parent>/<key>`), or turn
  `createTagValues` on and grant `deploy/gcp/tag-value-creator-role.yaml` on the keys. With the
  default `spec.gcp.claimTag: Skip` the values a claim needs are its owner, env and tier (and
  `hs-managed-by=subnet-operator`), which the teams share, so this is a new owner or env nobody
  created yet. If the missing value is `<parent>/hs-claim/<namespace>_<name>`, the scope sets
  `claimTag: Bind`, which needs a value per claim: create it, turn `createTagValues` on, or set
  `claimTag: Skip` unless something outside Kubernetes needs the claim tag. Nothing was created.
- `TagValueLimitReached` — the key already holds 1,000 values, the most Resource Manager allows.
  The operator never deletes a value. On `hs-claim` (only written with `claimTag: Bind`) the
  values of claims long gone accumulate: set `claimTag: Skip` so new claims need no value, and
  delete the values of claims that no longer exist
  (`gcloud resource-manager tags values delete <parent>/hs-claim/<namespace>_<name>`; a value
  still bound to a subnetwork cannot be deleted, so unbind it first or leave it). On `hs-owner`
  and the other keys, delete values nobody uses, or use another key.
- `PoolRequired`, `ZonesNotSupported`, `InvalidClaim` — the claim has no `spec.gcp.poolCIDRs`,
  lists zones (a subnetwork is regional), or asks for something Compute cannot create (a prefix
  outside /8–/29, a `namePrefix` that is not a valid subnetwork name, an owner, env or tier that
  cannot be a tag value). The webhook refuses these at apply time.
- `CreateFailed` with `status.allocations[].error`:
  - `cannot impersonate service account …` — as in [SubnetInventoryTargetDown on GCP](#subnetinventorytargetdown-on-gcp),
    for the write service account;
  - a 403 on `subnetworks.insert` — the write identity lacks `deploy/gcp/writer-role.yaml` in the
    host project, or the tag user role on the keys (binding at creation needs both);
  - `subnetwork … exists already, in <network> with range <cidr>; choose another namePrefix` —
    the name is taken in that region by something else. That can be another claim: the webhook
    refuses a second claim with the same `namePrefix` in a project and region, but not two that
    were applied in the same moment, written before 2.0.1 or while the webhooks were off (an
    update of either then warns `SubnetClaim <namespace>/<name> leads to … too`). Give one of
    them another `namePrefix`;
  - a range conflict — Compute refused the range because it overlaps another one; the operator
    reserves a new range and tries again on its own. One that keeps coming back is a subnetwork
    in a region the scope does not cover, which the operator cannot see: add the region to the
    scope, or name a pool it does not use. (The conflict is recognised by Compute's message,
    which #50 has still to confirm against the real API.)

**On Azure** a claim creates one subnet named `namePrefix` in the virtual network, with no zone,
after writing its ownership entry `hs-subnet-<name>` on the virtual network (the claim tag
`hs-claim` included, which is how a claim that lost its status finds its subnet again).
`NoWriteRole` means the subscription is read through `azure.clientID` and has no
`azure.writeClientID`; `NoSpace` that the virtual network's address space has no free block of
that size (add a prefix to the address space, or ask for a smaller one):

- `TagBudgetExceeded` — the virtual network already carries 50 tags, Azure's limit per
  resource, and the new subnet's entry would be one more; the message names the network.
  Nothing was created. The entries of its subnets share the limit with the network's own tags
  (`status.azure.tagCount` and `subnetOwnershipEntries` on the `Network`, the
  [NetworkTagBudgetLow](#networktagbudgetlow) alert). Remove tags the network no longer needs,
  or put the subnet in another virtual network; entries of subnets deleted since are safe to
  remove (`hs-subnet-<name>` of a subnet that no longer exists).
- `OwnershipEntryTooLong` — the claim's tags (owner, env, tier, `hs-managed-by`, `hs-claim` and
  `spec.tags`, as `key=value;…`) take more than the 256 characters of one Azure tag value. Use
  shorter names and values or fewer `spec.tags`. The webhook refuses such a claim at apply time;
  the claim is refused before anything is reserved.
- `TagValueConflict` — an entry of that name already exists with other values: a subnet of that
  name existed before, or another claim's create failed half-way. The operator never replaces
  an ownership value. Choose another `namePrefix`, or remove the stale entry if its subnet is
  gone.
- `ZonesNotSupported`, `InvalidClaim` — the claim lists zones (an Azure subnet is regional), or
  asks for something Azure cannot create: a prefix outside /2–/29, a `namePrefix` that is not a
  subnet name or is one Azure reserves (`GatewaySubnet`, `AzureFirewallSubnet`,
  `AzureBastionSubnet`, …), a `spec.networkID` that is not a virtual network's ID in the claim's
  subscription, or `gcp`/`aws` options. The case of the ID is never the reason: it may be pasted
  from the portal (`/subscriptions/…/resourceGroups/…/providers/Microsoft.Network/virtualNetworks/…`),
  and the operator uses it in lowercase, as `kubectl get hsnet` shows it. `NetworkNotFound` with
  an ID that looks right means the scope has not discovered that virtual network.
- `CreateFailed` with `status.allocations[].error`:
  - `subnet … exists already, with <prefix>, and was not created for this claim; choose another
    namePrefix` — the name is taken in the virtual network. The operator never changes an
    existing subnet (its create says `If-None-Match: *`);
  - `another operation on the virtual network is still in progress` — Microsoft.Network allows
    one change of a virtual network at a time (`AnotherOperationInProgress`); the operator
    retried five times with backoff and tries again at the next reconcile;
  - `the write identity (client …) lacks a permission; grant it the writer role …` with
    `AuthorizationFailed` — the write identity lacks `Microsoft.Network/virtualNetworks/subnets/write`
    or `Microsoft.Resources/tags/write`: assign the writer role (`deploy/azure/writer-role.json`)
    on the subscription or the virtual network's resource group. A virtual network whose subnets
    must have a route table or network security group may also ask for their `join/action`,
    which the role does not hold (*to confirm*, #56);
  - `cannot authenticate as client …` — as in
    [SubnetInventoryTargetDown on Azure](#subnetinventorytargetdown-on-azure), for
    `azure.writeClientID`: the write identity needs a federated identity credential of its own;
  - `throttled by the cloud API` — Resource Manager kept answering 429 for the five attempts;
    the claim retries on its own
    ([SubnetInventoryTargetThrottled on Azure](#subnetinventorytargetthrottled-on-azure));
  - a range conflict (`NetcfgSubnetRangesOverlap`, `NetcfgSubnetRangeOutsideVnet`) is retried with
    a new range on its own. The entry written for the first attempt stays and is used again; it
    names no subnet until the create succeeds.

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

**Do, by reason.** `WritesDisabled` and `NoWriteRole` (state `Pending`), `ScopeNotFound`,
`AccountNotInScope`, `NamespaceNotAllowed` and `ProviderNotEnabled` (state `Failed`) as for
claims above; `RegionRequired` and `InvalidResourceID` (state `Failed`) mean the import names no
region, or a resource ID that is not a `vpc-…` or `subnet-…`. `TagsNotApplied` (state `Failed`) is
AWS refusing `ec2:CreateTags` — usually the write role lacks it, or an SCP denies tagging. A
resource deleted after the import was requested lands here too, with `InvalidSubnetID.NotFound`
or `InvalidVpcID.NotFound` in `status.error`: delete the import as well.

**On GCP** an import binds Resource Manager tag values and only ever adds (state `Failed`,
retried every minute until the cause is fixed or the import deleted):

- `TagValueConflict` — the resource already carries another value of one of the keys. A GCP
  resource carries one value per key and the operator never removes a binding, so nothing was
  bound. If the new value is right, remove the old binding by hand
  (`gcloud resource-manager tags bindings delete --tag-value=<parent>/<key>/<old value> --parent=//compute.googleapis.com/projects/<project number>/global/networks/<network id>`,
  or `.../regions/<region>/subnetworks/<subnetwork id>` with `--location=<region>`; the numeric
  IDs are in `gcloud compute networks describe` / `subnets describe`), and the import applies at
  its next retry.
  The webhook refuses such an import at apply time when the operator already knows the
  resource's tags.
- `TagKeyMissing`, `TagValueMissing`, `TagValueLimitReached` — as for claims above.
- `InvalidResourceID` — not `projects/<project>/global/networks/<name>` or
  `projects/<project>/regions/<region>/subnetworks/<name>`, a project other than `spec.account`,
  or a subnetwork in a region other than `spec.region`. A network's import may leave `spec.region`
  empty.
- `TagsNotApplied` — anything else Google refused: usually a 403 on `tagBindings.create` (the
  write identity lacks the tag user role on the key, or `compute.*.createTagBinding` in the
  project), an impersonation error, or a resource deleted since (`look up …: … 404`), in which
  case delete the import as well.

**On Azure** an import of a virtual network adds its own tags; an import of a subnet adds to its
entry `hs-subnet-<name>` on its virtual network, leaving out what the subnet already inherits
from the network with the same value. Both go through the Tags API (`Merge`), which needs only
`Microsoft.Resources/tags/read` and `/write` (Tag Contributor), and only ever add. Before it
writes, and in a dry run, the operator reads the virtual network once as the **read** identity
(`virtualNetworks` Get): the Tags API would write the entry of a subnet that does not exist, and
the resource ID names no location. State `Failed`, retried every minute:

- `ResourceNotFound` — the virtual network does not exist (`… was not found in Azure`, with
  ARM's `ResourceNotFound` or `ResourceGroupNotFound`), or it has no subnet of that name
  (`… has no subnet "app" (it has apps, data)`). Nothing was written. Most often a misspelt name:
  `spec.resourceID` cannot be changed, so delete the import and create it with the right ID. If
  the subnet is still to be created, leave the import: it applies at the first retry after the
  subnet exists. The webhook warns at apply time when the virtual network is in the inventory
  and had no such subnet at its last sync (`… is in the inventory, and it had no subnet "app"
  when it was last discovered`). A dry run reports the same, with `dry run:` before the message.
- `RegionMismatch` — `spec.region` is not the virtual network's location (`… is in westeurope,
  not in northeurope`). Nothing was written. `spec.region` cannot be changed either: delete the
  import and create it with the right region, or without one where the webhooks run (they fill
  it in for a virtual network that is in the inventory, and refuse a wrong one there; this
  reason is for a virtual network the operator has not discovered).
- `AccountNotInScope` with `… is in northeurope, and account … in northeurope is not covered` —
  an import created without a region while the webhooks were off, whose virtual network is in a
  location the scope does not list: add the location to the scope, or delete the import.

- `TagValueConflict` — the resource already has another value for one of the keys. For a subnet
  that includes what it inherits from its virtual network (`… (inherited from the virtual
  network)`): an import cannot take over an owner the network gives its subnets. If the new value
  is right, change it yourself (the network's tag, or the pair in the subnet's entry) and the
  import applies at its next retry. The webhook refuses such an import at apply time when the
  operator already knows the resource's tags.
- `TagBudgetExceeded` — as for claims above: the virtual network has no tag left for a new name
  or a subnet's first entry. A subnet that already has an entry takes more pairs in the same tag.
- `OwnershipEntryTooLong` — the subnet's entry with the import's tags would be longer than 256
  characters.
- `InvalidResourceID` — not a virtual network or subnet ID in `spec.account`'s subscription
  (in any case: the operator lowercases it).
- `TagsNotApplied` — anything else ARM refused. From the write: `AuthorizationFailed` means the
  write identity lacks the Tags API permissions on the virtual network, and `cannot authenticate
  as client …` that `azure.writeClientID` has no federated identity credential for the operator's
  service account ([SubnetInventoryTargetDown on Azure](#subnetinventorytargetdown-on-azure)).
  From the read before it (`read virtual network …`): `the read identity (…) may not read it`
  means the read identity has no `Microsoft.Network/virtualNetworks/read` on that virtual
  network, usually because the scope names `spec.azure.resourceGroups`, the reader role is
  assigned on those groups alone and the virtual network is in another one; throttling there is
  retried like every other read.

An entry `hs-subnet-<name>` whose subnet was deleted after the import (or that a build of
`master` from before this check wrote for a subnet that never existed) stays on the virtual
network and counts against its 50 tags: the operator never removes a tag. Remove it yourself
([NetworkTagBudgetLow](#networktagbudgetlow)).

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
restart or a change of leader count: a series that first appeared at 1 would not have
increased as far as `increase()` could tell, and the digest would leave that import out.

**Confirm.**

```sh
kubectl get resourceimports.network.hypersurgery.dev -A -o custom-columns=\
NAME:.metadata.name,RESOURCE:.spec.resourceID,BY:.spec.requestedBy,CREATED-BY:'.metadata.annotations.network\.hypersurgery\.dev/created-by',STATE:.status.state
kubectl get resourceimports.network.hypersurgery.dev <name> -o jsonpath='{.metadata.annotations.network\.hypersurgery\.dev/reason}'
```

`spec.requestedBy` names the CloudTrail principal the tags were derived from, the
`network.hypersurgery.dev/reason` annotation says which rule won, and the
`network.hypersurgery.dev/created-by` annotation should be the operator's own service account — an
import in that list created by anybody else was written by hand, not by the policy.

On Azure there is no creator to derive tags from (the events do not name one): imports the
policy creates there come from `accountDefaults` and `inheritFromNetwork` only.

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

Worth adding locally; the metrics exist, the rules do not.

| Condition | Expression |
|---|---|
| Change events that are mostly unreadable (a sink filter or a forwarder sending something else) | `rate(hs_change_events_total{result="malformed"}[1h]) > 0.1` |
| A target failing repeatedly rather than once | `increase(hs_target_sync_errors_total[1h]) > 3` (throttled discoveries do not count here) |
| Throttling pressure before anything goes stale | `sum by (account, region) (rate(hs_api_throttled_total[15m])) > 0.05` |
| Subnets missing required tags | `hs_subnet_missing_required_tags > 0` |
| Policy cannot attribute anything | `increase(hs_auto_imports_total{result="no_owner"}[24h]) > 0` |
| Old-group objects nothing migrates (a GitOps tool re-applying an unconverted `aws.hypersurgery` manifest) | `sum(hs_migration_pending_objects) > 0` |
| Objects still stored at v1beta1 an hour after the upgrade to 1.0 (the log says why: usually CRDs that were not applied, or RBAC of your own without the CRD rules; see [upgrades](upgrades.md#what-the-operator-does-on-its-first-start)) | `hs_crd_stored_versions{version!="v1"} == 1` with `for: 1h` (the series goes away once only v1 is stored) |
