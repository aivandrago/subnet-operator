# GCP network model: metadata, IP usage, discovery, auth, events

Research for issue #40 (input to ADR #39 and issues #46–#50). Checked on 2026-09-24 against the
Compute Engine discovery documents (`compute v1`/`beta`/`alpha`, revision 20260910/20260916), the REST
reference, and product docs. Links point to `docs.cloud.google.com`, where `cloud.google.com/...` now redirects.

Confidence levels:
- **High**: confirmed in the API schema or reference docs.
- **Medium**: stated in official docs, but the exact behaviour is not spelled out.
- **Unverified**: not confirmed from primary Google documentation.

## Summary

- **Labels on networks/subnets: no.** In v1, beta and alpha, neither `Network` nor `Subnetwork` has a
  `labels`/`labelFingerprint` field or a `setLabels` method. (High)
- **Resource Manager tags on networks/subnets: yes.** Tags can be set at creation through the input-only
  `params.resourceManagerTags` field, or later through Resource Manager `tagBindings`. Tags are **not returned**
  by `subnetworks.get`. To read them, call `tagBindings.list` on the regional endpoint, call `effectiveTags.list`, or use
  Cloud Asset Inventory search, which returns `tags`/`effectiveTags`. (High)
- **`description` is create-only** on both subnetworks and networks, and `name` is immutable, so neither can hold
  ownership metadata that the operator changes later. (High)
- **Used/free IPs: the Compute API returns them directly.** `subnetworks.get|list|aggregatedList` accept
  `views=WITH_UTILIZATION`. The response then includes `utilizationDetails.ipv4Utilizations[]`
  (`rangeName`, `totalAllocatedIp`, `totalFreeIp`) per primary and secondary range, plus IPv6 utilization. This is the
  GCP equivalent of `AvailableIpAddressCount`. The Go client supports it through the `Views` field (`*_WITH_UTILIZATION`). (High for the
  field's existence. Exact semantics are Medium/Unverified, for example whether the 4 reserved IPs count as allocated.)
- Network Analyzer IP insights (Recommender `google.networkanalyzer.vpcnetwork.ipAddressInsight`) are a poor
  primary source. They run about 10 minutes after a change and at least once a day, only flag ranges above 75 %, and have a small
  free-tier quota.
- **Discovery:** use the Compute API per project (`subnetworks.aggregatedList` + `networks.list`) as the source of truth.
  Cloud Asset Inventory (CAI) data is "synchronized once every few hours", so use it only for
  org-wide *project enumeration* or *tag search*, never for IP state.
- **Auth:** use Workload Identity Federation from any OIDC-capable cluster (EKS/AKS/self-hosted; GKE has its own
  Workload Identity). Grant roles to the federated principal directly, or impersonate one GSA. The AWS AssumeRole analogue
  is IAM bindings for one principal on many projects/folders, or per-project GSA impersonation.
- **Events:** Admin Activity audit logs → aggregated org sink (`--include-children`) → Pub/Sub → the operator
  pulls. The alternative is CAI real-time feeds to Pub/Sub. No end-to-end latency SLO is documented for either.
- **Throttling:** Compute returns **403 `rateLimitExceeded`** (sometimes 429) per project per minute. Reads are
  counted per region. The response maps onto the existing `ErrThrottled` and adaptive backoff with a refill window of about 1 minute.
- **Tests:** there is no Compute emulator (the gcloud emulators are Bigtable, Datastore, Firestore, Pub/Sub and Spanner). Use an
  in-repo fake behind the operator's interface, the Pub/Sub emulator for the event path, and a real-project conformance run.

---

## 1. Metadata: labels, tags, description

**Answer**

- `Subnetwork` fields (v1) include `description`, `fingerprint`, `params`, `purpose`, `role`, `stackType`,
  `secondaryIpRanges` and `utilizationDetails`, among others. There is **no `labels`**. Subnetwork methods are
  `aggregatedList, delete, expandIpCidrRange, get, getIamPolicy, insert, list, listUsable, patch, setIamPolicy,
  setPrivateIpGoogleAccess, testIamPermissions`, with **no `setLabels`**. This is the same in beta and alpha.
- `Network` fields have **no `labels`**. `networks.patch` states: "Only routingConfig can be modified."
- Compute resources that do carry labels include `Address`, `ForwardingRule`, `Instance`, `Disk`, `VpnTunnel`,
  `Interconnect(Attachment)`, `SecurityPolicy` and others, but not networks or subnetworks. The Network Connectivity
  **`InternalRange`** resource does have `labels`, and a subnet can reference one through `reservedInternalRange`.
- **Resource Manager tags:** the supported-services list includes VPC **Networks** and **Subnetworks**. There are two ways to attach them:
  - At creation: `params.resourceManagerTags` (`SubnetworkParams`/`NetworkParams`). The field is "allowed for INSERT
    only" and is input-only (not persisted in the payload, so `get` does not return it).
  - After creation: `tagBindings.create` with the parent
    `//compute.googleapis.com/projects/PROJECT_NUMBER/regions/REGION/subnetworks/SUBNET_ID` or
    `//compute.googleapis.com/projects/PROJECT_NUMBER/global/networks/NETWORK_ID`. The parent must use **numeric IDs**, not names.
  - To read tags, call `tagBindings.list` on the **regional** endpoint
    `https://REGION-cloudresourcemanager.googleapis.com/v3/tagBindings?parent=...` for subnets. Alternatively, call
    `effectiveTags.list`, which includes inherited tags, or use CAI `searchAllResources`, which returns `tags`, `effectiveTags`,
    `tagKeys` and `tagValues`.
  - IAM: binding needs `resourcemanager.tagValueBindings.create` on the tag value (`roles/resourcemanager.tagUser`)
    **and** `createTagBinding` on the resource (for VPC, `roles/compute.networkAdmin` per the VPC docs).
    Tag keys and values must be pre-created at org or project level (`roles/resourcemanager.tagAdmin`):
    "The tag key and tag value must be defined before a tag can be attached to a resource."
  - Limits: at most 50 tags attached per resource, 1,000 keys per organization or project, and **1,000 values per
    key**. Because a value is a resource of its own, every distinct owner/env/tier value (for example each team name)
    must exist as a `TagValue` before it can be bound. The operator either creates values on demand, which needs
    `tagAdmin` on the key, or requires them to be provisioned up front and refuses unknown ones. The exact character
    rules for key and value short names are not on the pages I checked (**Unverified**).
- **Tags vs labels:** tags are separate resources with their own IAM, and they are inherited down the hierarchy. They can be
  used in IAM conditions and org policy, allow up to 256 characters, and must be pre-defined. A tag value cannot be deleted
  while bindings exist. Labels are free-form key/value metadata on the resource (63 characters), are not inherited, and have
  no policy support.
- **`description`** (subnet): "This field can be set only at resource creation time." `subnetworks.patch` updates
  only "certain fields … as indicated in the field descriptions". The fields documented as patchable are
  `secondaryIpRanges`, `stackType` (and `ipv6AccessType` on the first dual-stack switch), `role`, flow logs/`logConfig`,
  `privateIpGoogleAccess`, and `ipCidrRange` expansion through `expandIpCidrRange`. A patch requires the current
  `fingerprint`, or it fails with 412 `conditionNotMet`.
- **What the operator can write and read back:** Resource Manager tags only, through `tagBindings`, applied additively, which fits
  ResourceImport. The alternatives are a naming convention plus create-time `description`, which cannot change later, or
  keeping ownership in the operator's own CRD state.

**Confidence:** High. The absence of labels, the tags support, and description being create-only come from the schema and docs. The
exact IAM permission name `compute.subnetworks.createTagBinding` is Medium (inferred from the docs pattern).

**Sources**
- Subnetworks REST: https://docs.cloud.google.com/compute/docs/reference/rest/v1/subnetworks
- Networks REST: https://docs.cloud.google.com/compute/docs/reference/rest/v1/networks
- Discovery documents (schema used for the field checks): `https://compute.googleapis.com/$discovery/rest?version=v1` (also `beta` and `alpha`)
- Tags supported services: https://docs.cloud.google.com/resource-manager/docs/tags/tags-supported-services
- Tags for VPC resources: https://docs.cloud.google.com/vpc/docs/create-manage-tags-vpc-resources
- Tags overview (tags vs labels): https://docs.cloud.google.com/resource-manager/docs/tags/tags-overview
- Resource Manager limits (tags per resource, keys, values per key): https://docs.cloud.google.com/resource-manager/docs/limits
- Creating and managing tags (regional tagBindings endpoint): https://docs.cloud.google.com/resource-manager/docs/tags/tags-creating-and-managing
- effectiveTags.list: https://docs.cloud.google.com/resource-manager/reference/rest/v3/effectiveTags/list
- InternalRange (labels): `https://networkconnectivity.googleapis.com/$discovery/rest?version=v1`, https://docs.cloud.google.com/vpc/docs/internal-ranges

## 2. Used/free IP counts per subnetwork

**Answer**

- **Direct API field (recommended):** `Subnetwork.utilizationDetails` is "Output only. The current IP utilization of
  all subnetwork ranges. Contains the total number of allocated and free IPs in each range." It is populated only
  when the request passes `views=WITH_UTILIZATION`, which is supported on `subnetworks.get`, `subnetworks.list` and
  `subnetworks.aggregatedList`. The structure is:
  - `ipv4Utilizations[]`: `{rangeName (empty = primary), totalAllocatedIp, totalFreeIp}` (int64 as JSON string)
  - `internalIpv6Utilization`, `externalIpv6InstanceUtilization`, `externalIpv6LbUtilization`: `{totalAllocatedIp,
    totalFreeIp}` as `Uint128`
  - The Go client exposes `Views` on `GetSubnetworkRequest`, `ListSubnetworksRequest` and `AggregatedListSubnetworksRequest`,
    for example `computepb.ListSubnetworksRequest_WITH_UTILIZATION`. gcloud: `--view=WITH_UTILIZATION`.
  - Freshness: a community article published by the `googlecloud` account on dev.to (not Google's reference documentation) says the data comes "directly from Google Cloud's internal IP allocator" and describes
    it as real-time. The same article claims the 4 reserved IPs are included in `totalAllocatedIp`. **Unverified**: this is not
    stated in the API reference, and the article is not primary documentation.
  - Latency and quota: this is an ordinary read or list call. It uses the same Compute read quotas as discovery (see §6), and
    no separate quota is documented.
- **Network Analyzer IP utilization insights:** Recommender insight type
  `google.networkanalyzer.vpcnetwork.ipAddressInsight`. The insights report the allocation ratio for primary, secondary and PSA ranges.
  The primary-range ratio includes the 4 unusable IPs. A "high utilization" insight is raised when the ratio exceeds 75 %.
  Network Analyzer runs about 10 minutes after a config change and at least once a day. The Recommender read quota is
  100/min per org on the free tier and 6,000/min on the full tier. This source is too coarse and too slow for allocation decisions.
- **`subnetworks.listUsable`:** lists the subnets the caller may use, including Shared VPC subnets visible to service
  projects. It returns no IP counts. Its quota is a system limit ("List usable requests").
- **Counting ourselves (fallback only):** sum instance NIC primary IPs and alias ranges, internal forwarding rules,
  `addresses` with `addressType=INTERNAL` (`GCE_ENDPOINT`, `SHARED_LOADBALANCER_VIP`, etc.), GKE node and pod ranges, and PSC
  endpoints. Then subtract the reserved addresses. This is many list calls per region and error-prone. It is not needed now that
  `WITH_UTILIZATION` exists.
- **Reserved addresses:** the primary IPv4 range reserves the first two and last two addresses: network, default gateway,
  second-to-last (reserved for potential future use), and broadcast. Secondary ranges have none: "Google Cloud lets you use all
  addresses in secondary IPv4 ranges." The minimum range size is /29.
- **Secondary ranges and GKE:** GKE VPC-native clusters consume secondary ranges for Pods and Services.
  `ipv4Utilizations[]` reports each secondary range by `rangeName`, so the operator can report them separately and
  should not count them towards "free IPs for VMs".

**Confidence:** High that the field, the view and the Go support exist (schema and Go source). Medium for the real-time
freshness. **Unverified** whether `totalFreeIp` already excludes the 4 reserved IPs. Measure this in the conformance run
(#50). Network Analyzer figures: High.

**Sources**
- subnetworks.get/list (`views`): https://docs.cloud.google.com/compute/docs/reference/rest/v1/subnetworks/get ,
  https://docs.cloud.google.com/compute/docs/reference/rest/v1/subnetworks/list
- Go client source (`Views`, `SubnetworkUtilizationDetails`): https://github.com/googleapis/google-cloud-go/blob/main/compute/apiv1/computepb/compute.pb.go
- Community article on dev.to (non-reference): https://dev.to/googlecloud/real-time-ip-capacity-in-google-cloud-subnets-4m9j
- Reserved IPs: https://docs.cloud.google.com/vpc/docs/subnets#reserved_ip_addresses_in_every_subnet
- IP utilization insights: https://docs.cloud.google.com/network-intelligence-center/docs/network-analyzer/insights/vpc-network/ip-utilization
- Network Analyzer cadence: https://docs.cloud.google.com/network-intelligence-center/docs/network-analyzer/overview
- Recommender quotas: https://docs.cloud.google.com/recommender/quotas

## 3. Discovery API: Compute per project vs Cloud Asset Inventory

**Answer**

- **Compute API:** call `networks.list` (global) and `subnetworks.aggregatedList` (all regions in one call, paged) per
  project. Google recommends `returnPartialSuccess=true` for aggregatedList, which also accepts `views=WITH_UTILIZATION`. It is
  strongly consistent: the API reflects the resource itself. `aggregatedList` counts against the "heavy-weight read" quota
  (legacy metric) or the read quotas (simplified metrics). There is no per-call charge for Compute API reads (Medium). Projects
  must be enumerated separately, through Resource Manager `projects.search` or CAI.
- **Cloud Asset Inventory:** `searchAllResources` (scope org, folder or project; `assetTypes=compute.googleapis.com/Subnetwork`),
  `listAssets`, and `exportAssets` (to GCS or BigQuery). Freshness: "Unless noted …, data is synchronized once every few
  hours." Network and Subnetwork carry no exception note, so CAI can be hours behind. Search results include
  `labels`, `tags`, `effectiveTags` and `additionalAttributes`, but CAI does not return `utilizationDetails`. Quotas:
  - SearchAllResources: 400/min per consumer project, 1,500/min per org
  - ListAssets: 100/min per consumer project, 800/min and 650,000/day per org
  - ExportAssets: 60/min and 6,000/day per consumer project; 75/min and 13,000/day per org
  - Real-time feed APIs: 600/min per consumer project, 30/min per org
  - Cost: resource search and list are not billed separately (**Unverified**, see pricing page).
- **Recommendation:** use the Compute API as the source of truth for subnets, IPs and reconcile. Optionally use CAI
  `searchAllResources` (or `projects.search`) for org-wide project discovery and tag-based filtering. Do not rely on CAI for
  anything that decides allocation.

**Confidence:** High (quotas and freshness quoted from docs). CAI cost: Unverified.

**Sources**
- subnetworks.aggregatedList: https://docs.cloud.google.com/compute/docs/reference/rest/v1/subnetworks/aggregatedList
- CAI quotas: https://docs.cloud.google.com/asset-inventory/docs/quota
- CAI supported asset types and freshness: https://docs.cloud.google.com/asset-inventory/docs/supported-asset-types
- CAI search: https://docs.cloud.google.com/asset-inventory/docs/searching-resources
- CAI pricing: https://cloud.google.com/asset-inventory/pricing

## 4. Auth from Kubernetes

**Answer**

- **Workload Identity Federation with Kubernetes** supports EKS (no prerequisites), AKS (with the OIDC issuer enabled) and
  self-hosted clusters (1.20+ with projected SA tokens). For GKE, use GKE Workload Identity Federation instead. Setup:
  1. Create a workload identity pool and an OIDC provider whose issuer is the cluster issuer.
  2. Map `google.subject=assertion.sub`, which gives `system:serviceaccount:NS:KSA`.
  3. Mount a projected SA token with the provider audience, for example at `/var/run/service-account/token`.
  4. Provide a non-secret credential configuration file (`type: external_account`) in a ConfigMap, and point
     `GOOGLE_APPLICATION_CREDENTIALS` at it.
- **Direct resource access:** grant roles to
  `principal://iam.googleapis.com/projects/PROJECT_NUMBER/locations/global/workloadIdentityPools/POOL/subject/system:serviceaccount:NS:KSA`.
  The docs warn that "certain API methods might have limitations" with federated principals.
- **SA impersonation:** grant the principal `roles/iam.workloadIdentityUser` on a GSA and add
  `service_account_impersonation_url` to the config. This is the safe fallback if some API rejects federated principals.
- **Cross-project (AssumeRole analogue):** there is no per-call role assumption. Options:
  - (a) Bind the single federated principal or GSA at folder or org level, or on each target project. This is simplest,
    because each API call just names the target project.
  - (b) Use one GSA per project and impersonate it (`iamcredentials.generateAccessToken` or the Go
    `impersonate.CredentialsTokenSource`). This is closest to per-account AssumeRole, for blast-radius isolation or audit.
- **Go:** `golang.org/x/oauth2/google/externalaccount` (and `cloud.google.com/go/auth`) support `external_account` credentials
  with file, URL, executable and AWS sources and optional SA impersonation. ADC picks them up automatically, so
  `compute/apiv1` needs no code changes.
- **Minimum roles:**
  - Read: `roles/compute.networkViewer`. It includes subnet get/list (Medium).
  - Create subnets: `roles/compute.networkAdmin` on the host project.
  - Tags: `roles/resourcemanager.tagUser`.
  - Events: `roles/pubsub.subscriber` on the subscription.

**Confidence:** High for the WIF flow and the Go support. **Unverified**: whether every Compute, Resource Manager
tagBindings and CAI method works with direct principal access. The "products and limitations" page body could not be
fetched, so check https://docs.cloud.google.com/iam/docs/federated-identity-supported-services before choosing direct
access.

**Sources**
- WIF with Kubernetes: https://docs.cloud.google.com/iam/docs/workload-identity-federation-with-kubernetes
- Supported products/limitations: https://docs.cloud.google.com/iam/docs/federated-identity-supported-services
- Go externalaccount: https://pkg.go.dev/golang.org/x/oauth2/google/externalaccount
- Go impersonation: https://pkg.go.dev/google.golang.org/api/impersonate

## 5. Change events

**Answer**

- **Audit logs → sink → Pub/Sub (EventBridge→SQS analogue):** Admin Activity audit logs are always on and free.
  Compute writes ADMIN_WRITE methods there with `serviceName=compute.googleapis.com` and
  `methodName` such as `v1.compute.subnetworks.insert`, `…patch`, `…delete`, `…expandIpCidrRange`,
  `v1.compute.networks.insert`, and `beta.`/`alpha.` variants. Tag binding changes are logged by
  `cloudresourcemanager.googleapis.com` (TagBindings methods).
  - Create an **aggregated sink** at org or folder level with `--include-children` and a Pub/Sub topic destination. Grant the
    sink writer identity `roles/pubsub.publisher`. Example filter:
    `logName:"cloudaudit.googleapis.com%2Factivity" AND protoPayload.serviceName="compute.googleapis.com" AND protoPayload.methodName:("subnetworks" OR "networks")`.
  - Sink changes "might take a few minutes to apply". There is no documented delivery-latency SLO.
- Long-running Compute operations produce a first and a last log entry (`operation.first`/`operation.last`). Filter on
  `operation.last=true` or de-duplicate (Medium).
- **Pub/Sub pull from the operator:** use StreamingPull with at-least-once delivery. Handlers must be idempotent, as with SQS today.
- **Alternative: CAI real-time feeds** (org, folder or project scope; `assetTypes` Network/Subnetwork; optional CEL
  `condition`). Each message is a `TemporalAsset` with the current and prior state. Up to 200 feeds per parent. Feed
  create, update or delete takes up to 10 minutes to apply. The feed API quota is 30/min per org. There is no documented
  per-message latency SLO. Advantage: resource-shaped payloads. Disadvantage: no actor or request detail, and freshness
  semantics are less clear.
- **Recommendation:** use events only as a reconcile trigger (enqueue the subnet key) and treat periodic
  `aggregatedList` as the source of truth, as with the AWS design.

**Confidence:** Medium-High for the mechanism. Latency: **Unverified** (none documented). The `operation.first/last` detail is Medium.

**Sources**
- Compute audit logging: https://docs.cloud.google.com/compute/docs/logging/audit-logging
- Audit log types: https://docs.cloud.google.com/logging/docs/audit
- Aggregated sinks: https://docs.cloud.google.com/logging/docs/export/aggregated_sinks
- CAI feeds: https://docs.cloud.google.com/asset-inventory/docs/monitor-asset-changes
- Pub/Sub pull: https://docs.cloud.google.com/pubsub/docs/pull

## 6. Throttling model

**Answer**

- Compute rate quotas are per project and per minute. There are two sets of metrics:
  - Simplified metrics: global `compute.googleapis.com/global_reads` and `global_writes`; regional `reads_per_region` and
    `writes_per_region`. Regional and zonal get/list count per region.
  - Legacy metrics (still active): "Read requests", "List requests", "Operation read requests", "Heavy-weight read requests"
    (aggregatedList), and "Default/Queries".
  - System limits: "List usable requests", "750k resources filtered out of the list requests per region per minute", and overall
    requests per minute per region.
- Default values are shown per project in the console or Cloud Quotas API. The reference page does not list them.
- When a quota is exceeded, the API returns HTTP **403 with reason `rateLimitExceeded`**. The best-practices page also mentions **429**. Buckets refill
  "usually every minute". Retry on a 403 only when the reason is `rateLimitExceeded`. A 403 with any other reason is permission denied and must not be retried.
- Mutations are asynchronous Operations. Poll them with `regionOperations.wait`/`get`, which uses the operation-read quota. Concurrent
  operation limits also apply.
- **Mapping:**
  - Classify `googleapi.Error` (REST), or gRPC `ResourceExhausted`, with reason `rateLimitExceeded`/`userRateLimitExceeded`, plus
    HTTP 429, as `ErrThrottled`.
  - The adaptive backoff needs a floor that can reach roughly 60 s, because quotas are per-minute buckets.
  - Keep token-bucket limits per project **and per region**, because read quotas are regional.
  - Prefer one `aggregatedList` with a filter over many per-region lists.

**Confidence:** High for the error code and quota names. Default numeric limits: **Unverified** (not on the reference page).

**Sources**
- Rate quotas: https://docs.cloud.google.com/compute/api-quota
- Best practices: https://docs.cloud.google.com/compute/docs/api/best-practices
- Simplified quota metrics: https://docs.cloud.google.com/compute/docs/api/how-tos/use-simplified-quota
- Quota troubleshooting: https://docs.cloud.google.com/docs/quotas/troubleshoot
- Concurrent operations: https://docs.cloud.google.com/compute/docs/troubleshooting/troubleshoot-operation-limits

## 7. Test strategy

**Answer**

- `gcloud beta emulators` covers only **Bigtable, Datastore, Firestore, Pub/Sub, Spanner**. There is no Compute or VPC emulator.
- **Pub/Sub emulator:** Go clients use it automatically when `PUBSUB_EMULATOR_HOST` is set. Limitations: no IAM,
  indefinite retention, no UpdateTopic. It is good enough for the event path (#48/#49).
- **Plan:**
  1. An in-repo fake behind the operator's provider interface, like the AWS fakes. It should model `fingerprint` and 412, CIDR
     overlap errors, `utilizationDetails`, 403 `rateLimitExceeded`, aggregatedList partial success, and async
     operations.
  2. Optionally, an `httptest` REST fake for the `compute/apiv1` REST client, using `option.WithEndpoint` +
     `option.WithoutAuthentication` + `option.WithHTTPClient`, to test request shapes such as `views` and filters.
  3. A gated conformance suite against a real sandbox project: create a subnet, bind a tag, check utilization numbers,
     receive an audit-log event, and trigger throttling classification.

**Confidence:** High (emulator list from gcloud reference).

**Sources**
- gcloud emulators: https://docs.cloud.google.com/sdk/gcloud/reference/beta/emulators
- Pub/Sub emulator: https://docs.cloud.google.com/pubsub/docs/emulator
- Go client options: https://pkg.go.dev/google.golang.org/api/option

## Other model facts

- **Scope:** "VPC networks are global resources … Subnets are regional resources." There are no zones or AZs on subnets, so
  the AWS AZ dimension collapses to region. (High)
- **Shared VPC:** all subnets live in the host project. Only the host project's Shared VPC Admin or Network Admin can create them.
  Service projects use subnets through `roles/compute.networkUser`, granted at project or **subnet level**
  (`subnetworks.setIamPolicy`), and discover them through `listUsable`. The operator should therefore discover and create in host
  projects and treat service projects as consumers. (High)
- **Creation constraints:** primary and secondary ranges must be unique across the network and must not overlap peered networks'
  subnet ranges. The minimum size is /29. A primary range can be expanded but not shrunk or replaced. Overlap with custom static
  routes is rejected unless `allowSubnetCidrRoutesOverlap` is set. Dynamic routes are not checked. The **error code for a
  CIDR conflict** is expected to be HTTP 400 with reason `invalid`/`badRequest` and a message naming the conflicting
  subnet (**Unverified**, confirm in conformance). (High for the constraints)
- **IPv6:**
  - `stackType` is `IPV4_ONLY` (the default), `IPV4_IPV6` or `IPV6_ONLY`, and can be patched.
  - `ipv6AccessType` is `INTERNAL` or `EXTERNAL`. It is immutable once set.
  - Ranges are output in `internalIpv6Prefix` and `externalIpv6Prefix` (/64), and BYOIP ranges come through `ipCollection`.
  - Utilization is reported as `Uint128`.
  (High)
- **`purpose`:** `PRIVATE` (regular), `PRIVATE_RFC_1918` (legacy regular), `REGIONAL_MANAGED_PROXY`,
  `GLOBAL_MANAGED_PROXY`, `INTERNAL_HTTPS_LOAD_BALANCER` (legacy), `PRIVATE_SERVICE_CONNECT`, `PRIVATE_NAT`,
  `PEER_MIGRATION`. Only `PRIVATE` and `PRIVATE_RFC_1918` host VMs. Report the others as non-allocatable, and exclude them from claims.
  Proxy-only subnets also have `role` (`ACTIVE`/`BACKUP`) and `state` (`READY`/`DRAINING`). (High)

Sources: https://docs.cloud.google.com/vpc/docs/subnets , https://docs.cloud.google.com/vpc/docs/shared-vpc ,
https://docs.cloud.google.com/vpc/docs/vpc , https://docs.cloud.google.com/sdk/gcloud/reference/compute/networks/subnets/create

## Implications for the operator

For ADR #39 and issues #46–#50:

1. **Ownership model (ADR #39):**
   - GCP has no mutable labels on subnets, so AWS-style tag ownership maps to **Resource Manager tags**.
   - Pre-create org-level tag keys `hs-owner`, `hs-env`, `hs-tier` and `hs-managed`. The `/` in the AWS names is not
     allowed, so tag keys need a mapping.
   - On create, pass `params.resourceManagerTags`, which is atomic with insert. On import, call `tagBindings.create`, which is
     additive and matches the ResourceImport semantics.
   - Read ownership through the regional `tagBindings.list` or `effectiveTags.list`, or in bulk through CAI search, which can be hours stale.
   - Every owner/env/tier value must exist as a `TagValue` first (1,000 values per key). Decide whether the write
     identity may create values (`tagAdmin` on the `hs-*` keys) or values are provisioned by the platform team.
   - Account for tag inheritance: a project-level `hs-env` tag will appear as effective on every subnet.
   - Ownership reads cost one Resource Manager call per subnet. Cache aggressively, or use CAI search for bulk
     reads with Compute as the tie-breaker.
2. **IP usage (#46/#47):** use `subnetworks.aggregatedList?views=WITH_UTILIZATION&returnPartialSuccess=true` per host
   project. Report free IPs per range: the primary range, plus each secondary range by `rangeName`. Validate the reserved-IP
   accounting in conformance. Do not use the Recommender insights.
3. **Provider interface:** make "zone/AZ" optional in the cloud-neutral model. Model region-scoped subnets, global
   networks, the host/service project split, `purpose` filtering, and multiple ranges per subnet (primary + secondary + IPv6).
4. **Discovery:** reconcile against the Compute API per project. Enumerate projects through `projects.search` or CAI.
   Use per-project and per-region rate limiters.
5. **Auth (#48):** use WIF with direct principal bindings at folder or org level as the default. Support optional per-project GSA
   impersonation as the "AssumeRole" equivalent. Use ADC with an `external_account` config, which needs no custom credential code.
6. **Events (#49):** use an org aggregated log sink to a Pub/Sub topic plus a pull subscription as the trigger. Keep the periodic full
   resync. The event payload only yields a key to enqueue.
7. **Errors:** map 403 `rateLimitExceeded` and 429 to `ErrThrottled`, other 403s to `ErrForbidden`, 412 `conditionNotMet` to a
   conflict/retry, and 400 CIDR overlap to a terminal "range conflict" condition on the SubnetClaim.
8. **Tests (#50):** use an in-repo fake, the Pub/Sub emulator in CI, and a nightly or manual conformance job against a sandbox org or project.

## Unverified / open

- Whether `utilizationDetails.totalAllocatedIp` includes the 4 reserved primary-range IPs (the dev.to article says yes), and how
  fresh the data is. Measure in conformance.
- Which resources count as "allocated" in `utilizationDetails`: reserved-but-unused internal addresses, GKE pod ranges
  assigned to nodes, and PSC endpoints.
- IAM permission required for `views=WITH_UTILIZATION`. Presumably the same as get/list (`compute.subnetworks.get`/`list`).
- Default numeric values for Compute read, list and heavy-weight quotas (not published on the reference page; check them in the console).
- Exact HTTP status, reason and message for a subnet CIDR overlap on `subnetworks.insert`.
- Whether all needed methods (Compute, `tagBindings`, CAI, Pub/Sub) accept direct WIF principals without SA
  impersonation.
- End-to-end latency of audit log → sink → Pub/Sub, and of CAI feed notifications (no documented SLO).
- CAI pricing for search and list calls.
- Exact IAM permission name for binding tags to subnetworks (`compute.subnetworks.createTagBinding` assumed) and whether
  `roles/compute.networkAdmin` is the narrowest predefined role that includes it.
