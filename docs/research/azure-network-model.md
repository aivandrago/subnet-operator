# Azure network model: metadata, IP usage, discovery, auth, events

Research for issue #41 (input to ADR #39 and issues #52–#56). Sources are Microsoft Learn
pages, Azure REST API reference (Virtual Networks api-version `2025-09-01`, Resources
`2021-04-01`) and the official Azure SDK for Go docs on pkg.go.dev. I checked the pages on
2026-09-24. Anything I could not confirm from a primary Microsoft source is marked
**Unverified**.

## Summary

- **Subnets cannot carry tags.** The `Subnet` schema in the REST reference has only `id`,
  `name`, `etag`, `type` and `properties.*`. It has no `tags` and no `location`. The
  "Tag support" table lists `virtualNetworks / subnets` as **No**. Ownership has to live
  elsewhere. The recommended place is **VNet tags keyed per subnet**, with the in-cluster
  CR as the source of intent. Confidence: high.
- **VNet tag limits**: 50 tags per resource, tag name 512 characters, value 256
  characters, names case-insensitive, values case-sensitive, and `< > % & \ ? /` are not
  allowed in names. **Additive writes** go through `PATCH {scope}/providers/Microsoft.Resources/tags/default`
  with `operation: Merge`. The **Tag Contributor** role (`Microsoft.Resources/tags/*`) can
  tag a resource without having write access to the resource itself. Confidence: high.
- **IP usage source**: `GET .../virtualNetworks/{vnet}/usages` (VirtualNetworks – List
  Usage) returns one entry per subnet: `id` = subnet ID, `currentValue` = "number of IPs
  used from the Subnet", `limit` = "size of the subnet". The documented example shows
  `-1/-1` for `GatewaySubnet`. This is one call per VNet, so it is much cheaper than
  counting `ipConfigurations`. Confidence: high for the shape of the response. Whether
  `currentValue` includes the 5 reserved addresses and service-injected usage is
  **Unverified**, so it needs a conformance test.
- `size − 5 − len(ipConfigurations)` is **not complete**. Delegated or service-injected
  subnets (App Service integration, SQL MI and similar) use addresses that are exposed
  through `serviceAssociationLinks` / `resourceNavigationLinks`, not as NIC
  ipConfigurations. Treat this formula only as a fallback.
- **Discovery**: use **Azure Resource Graph** (free, tenant or management-group scope,
  `resources` table, `mv-expand properties.subnets`) for inventory across subscriptions.
  Use **ARM per VNet** (GET VNet, List Usage) for authoritative reads before writes.
  Resource Graph is eventually consistent. Confidence: high.
- **Auth**: Entra Workload ID federation works from any OIDC-issuing Kubernetes cluster
  (AKS, EKS, GKE, on-prem) with a user-assigned managed identity or an app registration.
  In Go, use `azidentity.NewWorkloadIdentityCredential`. Other subscriptions only need RBAC
  role assignments (no assume-role). Another tenant needs a **multitenant app
  registration** provisioned into that tenant. Confidence: high.
- **Events**: an Event Grid system topic per subscription (`Microsoft.Resources.*`, or
  ARN `Microsoft.ResourceNotifications.Resources.*` with the resource payload) delivers to
  a Storage Queue or Service Bus queue. Delivery is at-least-once and unordered, with
  retries up to 24 h / 30 attempts. **IP consumption (NIC create/delete) does not write
  the subnet resource**, so events cannot replace periodic usage polling. Confidence:
  medium.
- **Throttling**: ARM uses per-region token buckets (subscription reads 250 tokens,
  refilled at 25/s; writes and deletes 200 tokens at 10/s; per subscription, per service
  principal, per operation type). Microsoft.Network adds its own limits: 1,000 PUT/DELETE
  per 5 min and 10,000 GET per 5 min. Throttled requests get 429 + `Retry-After`. Resource
  Graph has a separate per-user quota (for example 15 queries per 5 s, reported in
  `x-ms-user-quota-remaining` / `x-ms-user-quota-resets-after`). Confidence: high.
- **Tests**: there is no network emulator. `armnetwork/vN/fake` provides stub
  `*Server` structs with function fields and no state. The operator's own in-memory fake
  should implement those fields. Azurite covers the Storage Queue event path, and a Service
  Bus emulator exists. A real-subscription conformance suite is still required.

---

## Q1. Tags on subnets and where ownership can live

**Answer**

- **Subnet schema**: the REST reference for Subnets – Create Or Update / Get (2025-09-01)
  defines `Subnet` with `etag`, `id`, `name`, `type` and `properties.*`. The `properties`
  include `addressPrefix`, `addressPrefixes`, `delegations`, `ipConfigurations`,
  `ipConfigurationProfiles`, `ipAllocations`, `ipamPoolPrefixAllocations`, `natGateway`,
  `networkSecurityGroup`, `privateEndpoints`, `purpose` (read-only),
  `resourceNavigationLinks`, `routeTable`, `serviceAssociationLinks`, `serviceEndpoints`,
  `serviceEndpointPolicies`, `serviceGateway`, `sharingScope`, `defaultOutboundAccess`
  and `provisioningState`. **There is no `tags` and no `location`.** No property accepts
  arbitrary user text. `purpose` is read-only and service-derived.
- **Tag support table**: `Microsoft.Network / virtualNetworks` supports tags = Yes.
  `virtualNetworks / subnets` = **No**, and `virtualNetworks / virtualNetworkPeerings` =
  No.
- **Tags API at subnet scope**: `Microsoft.Resources/tags` operates on `{scope}`, which
  the reference describes as "a resource or subscription". Because the subnet type does
  not support tags, a Tags API call scoped to a subnet ID is expected to fail.
  **Unverified**: I found no documented error; confirm it in the conformance run.
- **Tag limits** (from "Use tags to organize..."): at most 50 tag name/value pairs per
  resource, resource group or subscription. Tag names are limited to 512 characters and
  values to 256 characters (storage accounts: 128/256). Names are case-insensitive for
  operations and values are case-sensitive. Names cannot contain `<`, `>`, `%`, `&`, `\`,
  `?`, `/`. Tags are plain text and show up in cost reports, deployment history and so on.
  For more than 50 values, the documented workaround is a JSON string in a single tag value.
- **Where ownership can live**:
  1. **VNet tags with one key per subnet**. This is the recommended option. Example:
     `hs-subnet-<subnet-name>` = `owner=<o>;env=<e>;tier=<t>` or compact JSON, which must
     fit in 256 characters. Subnet names are 1–80 characters of alphanumerics, `_`, `.`
     and `-` ([naming rules](https://learn.microsoft.com/en-us/azure/azure-resource-manager/management/resource-name-rules)),
     so they are always valid inside a tag name (both are case-insensitive). Tag names cannot
     contain `/`, so the AWS-style `hs/...` keys are not valid here: use `hs-owner`,
     `hs-managed` and so on. The 50-tag budget is shared with the customer's own tags. The
     operator should reserve a small number of keys, for example one `hs-managed` key plus
     one key per managed subnet, and fail clearly when it runs out of room. A VNet can have
     up to thousands of subnets, so per-subnet keys do not scale to large shared VNets.
  2. **In-cluster mapping** (the CR / ResourceImport status is the source of truth) plus
     VNet-level ownership tags. This is always possible and costs no tag space, but other
     tools cannot see it.
  3. **A separate taggable resource per subnet**, for example an NSG or route table
     associated with the subnet that carries the tags. This is intrusive because
     associations change data-plane behaviour, so it is not recommended.
  4. **Subnet free-form properties**: none exist.
- **Writing VNet tags**:
  - `VirtualNetworks – Update Tags` (`PATCH .../virtualNetworks/{vnet}` with
    `{"tags":{...}}`, TagsObject) needs `Microsoft.Network/virtualNetworks/write`. For
    Microsoft.Network, PATCH is generally understood to **replace** the whole tag
    dictionary. **Unverified** (the page does not state merge or replace semantics).
    Relying on it means read-modify-write.
  - `Tags – Update At Scope` (`PATCH https://management.azure.com/{scope}/providers/Microsoft.Resources/tags/default?api-version=2021-04-01`,
    body `{"operation":"Merge","properties":{"tags":{...}}}`) works as follows: "'merge'
    allows adding tags with new names and updating the values of tags with existing
    names". `Replace` and `Delete` (by name or name/value) also exist. The 50-tag limit
    applies at the end of the operation. Merge is server-side, so there is no
    read-modify-write race **between tag writers**. It does, however, overwrite the value
    of an existing key, so "add-only" still needs a read-before-write check. Only a lost
    race on the same key can overwrite.
  - **RBAC**: "write access to the `Microsoft.Resources/tags` resource type... lets you tag
    any resource, even if you don't have access to the resource itself." The **Tag
    Contributor** role grants `Microsoft.Resources/tags/*` plus read on resource groups
    and resources. The operator can therefore tag without `Microsoft.Network/virtualNetworks/write`.

**Confidence**: high for the schema, the tag-support row, the limits, Tags API semantics
and RBAC. Medium for the per-subnet-key design.

**Sources**
- Subnets – Create Or Update (Subnet definition): https://learn.microsoft.com/en-us/rest/api/virtualnetwork/subnets/create-or-update
- Subnets – Get: https://learn.microsoft.com/en-us/rest/api/virtualnetwork/subnets/get
- Tag support for Azure resources: https://learn.microsoft.com/en-us/azure/azure-resource-manager/management/tag-support
- Tag limits and required access: https://learn.microsoft.com/en-us/azure/azure-resource-manager/management/tag-resources
- Tags – Update At Scope: https://learn.microsoft.com/en-us/rest/api/resources/tags/update-at-scope
- Virtual Networks – Update Tags: https://learn.microsoft.com/en-us/rest/api/virtualnetwork/virtual-networks/update-tags
- Tag Contributor built-in role: https://learn.microsoft.com/en-us/azure/role-based-access-control/built-in-roles/management-and-governance

## Q2. Free IPs per subnet

**Answer**

- **Reserved addresses**: "Azure reserves the first four addresses and the last address,
  for a total of five IP addresses within each subnet" (network, default gateway, two for
  Azure DNS, broadcast). The smallest IPv4 subnet is /29 and the largest is /2. IPv6
  subnets must be exactly /64.
- **Usage API**: `GET /subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.Network/virtualNetworks/{vnet}/usages?api-version=2025-09-01`
  returns `VirtualNetworkListUsageResult { value[], nextLink }`. Each `VirtualNetworkUsage`
  contains:
  - `id`: "Subnet identifier" (the full subnet resource ID)
  - `currentValue`: "Indicates number of IPs used from the Subnet"
  - `limit`: "Indicates the size of the subnet"
  - `name.value = "SubnetSpace"`, `localizedValue = "Subnet size and usage"`
  - `unit = "Count"`
  - The documented example returns `currentValue: -1, limit: -1` for `GatewaySubnet` and
    `2 / 3` for a normal subnet. `-1` therefore means "not reported" and must be handled
    as unknown.
  - The response is paged, so the SDK pager follows `nextLink`.
  - The docs do not say whether `limit` already excludes the 5 reserved addresses. The
    example `limit: 3` for a normal subnet is too small for any real subnet, since /29
    minus 5 gives 3. That suggests `limit` = usable addresses after reservation, but this
    is **Unverified**. Whether `currentValue` counts service-injected addresses
    (delegations) and IPv6 is also **Unverified**.
- **`ipConfigurations` counting** (`size − 5 − len(properties.ipConfigurations)`):
  - `ipConfigurations` is documented as "references to the network interface IP
    configurations using subnet". This covers VM, VMSS and private endpoint NICs (a private
    endpoint creates a NIC and is also listed in `privateEndpoints`), plus internal load
    balancer frontends. The AKS doc confirms "internal Azure Load Balancer front-end IPs
    are allocated from the cluster subnet". Application Gateway uses
    `applicationGatewayIPConfigurations`, a separate list.
  - **Not covered**: service-injected or delegated subnets. App Service VNet integration
    uses "one address... for each App Service plan instance", doubles temporarily during
    scale operations and can hold addresses for up to 12 hours afterwards. These show up as
    `serviceAssociationLinks`, not as NIC ipConfigurations. The same applies to other
    delegated services such as SQL MI (`resourceNavigationLinks` / `serviceAssociationLinks`).
    `ipAllocations` and `ipamPoolPrefixAllocations` can also reserve ranges.
  - **AKS**: with **Azure CNI Overlay**, "pods are assigned IPs from a separate, private
    CIDR range and won't require VNet IPs", so only nodes (and ILB frontends) use subnet
    IPs. With **flat** networking (Azure CNI Pod Subnet), pods use VNet IPs. With the
    legacy flat Azure CNI, IPs are preallocated per node as secondary NIC ipConfigurations.
    That the preallocation is visible on the subnet is **Unverified**, though it is
    consistent with NIC semantics. For Pod Subnet static block allocation, how the blocks
    are represented on the subnet is **Unverified**.
  - Azure Firewall, VPN/ER gateway, Bastion and NAT: NAT Gateway uses public IPs and does
    not consume subnet addresses. Special subnets (`GatewaySubnet`, `AzureFirewallSubnet`)
    report `-1` in List Usage (documented for GatewaySubnet). How they appear in
    `ipConfigurations` is **Unverified**.
  - **Large subnets**: `ipConfigurations` is returned inline on GET subnet and GET VNet,
    with no paging documented. The response can be very large for big subnets. The
    `$expand` query parameter on Subnets – Get only expands referenced resources. Whether
    ARM truncates the list is **Unverified**.
- **CheckIPAddressAvailability**: `GET .../virtualNetworks/{vnet}/checkIPAddressAvailability?ipAddress=x.x.x.x`
  returns `available` (bool), `isPlatformReserved` (bool) and up to a few alternative
  `availableIPAddresses` (the example shows 5). It checks a single address and cannot
  report counts. It is useful only for pre-flight checks of a specific address.

**Recommendation**: use **List Usage** as the primary source, `freeIPs = limit −
currentValue`, and treat `-1` as unknown. If both numbers are valid, do not subtract 5
again until the conformance test confirms whether `limit` already excludes the reserved
addresses. Mark subnets with `delegations`, `serviceAssociationLinks` or
`resourceNavigationLinks` as "service-managed" and flag their free-IP value as low
confidence, unless conformance tests show that List Usage counts them. Keep the
`ipConfigurations` formula only as a fallback (for example for Resource Graph-only
inventory).

**Confidence**: high for the reserved-5 rule, the List Usage schema and the
CheckIPAddressAvailability schema. Medium for completeness gaps (documented for App
Service, AKS overlay and flat). Low/**Unverified** for exact List Usage semantics on
delegated subnets, IPv6 and whether reserved addresses are counted.

**Sources**
- VNet FAQ (reserved addresses, sizes, region, zones, IPv6): https://learn.microsoft.com/en-us/azure/virtual-network/virtual-networks-faq
- Virtual Networks – List Usage: https://learn.microsoft.com/en-us/rest/api/virtualnetwork/virtual-networks/list-usage
- Virtual Networks – Check IP Address Availability: https://learn.microsoft.com/en-us/rest/api/virtualnetwork/virtual-networks/check-ip-address-availability
- Subnets – Create Or Update (field descriptions): https://learn.microsoft.com/en-us/rest/api/virtualnetwork/subnets/create-or-update
- AKS IP address planning (overlay vs flat, ILB frontends): https://learn.microsoft.com/en-us/azure/aks/concepts-network-ip-address-planning
- App Service VNet integration subnet requirements: https://learn.microsoft.com/en-us/azure/app-service/overview-vnet-integration
- Subnet delegation: https://learn.microsoft.com/en-us/azure/virtual-network/subnet-delegation-overview

## Q3. Discovery API: ARM per subscription vs Azure Resource Graph

**Answer**

- **ARM**: `VirtualNetworks – List All` (per subscription) and `VirtualNetworks – Get`
  (includes `properties.subnets[]` with full subnet properties). These calls count against
  the ARM read bucket and the Microsoft.Network GET limit (10,000 per 5 min). They return
  authoritative, read-your-writes data. You need one call per subscription, plus one List
  Usage call per VNet for IP counts.
- **Azure Resource Graph (ARG)**: a single KQL query can cover many subscriptions, a
  management group or the tenant (`POST /providers/Microsoft.ResourceGraph/resources`).
  - **Cost**: "As a free service, queries to Resource Graph are throttled".
  - **Throttling**: a per-user quota over a time window: "a user can send at most 15
    queries within every 5-second window... The quota value is determined by many factors
    and is subject to change." The headers are `x-ms-user-quota-remaining` and
    `x-ms-user-quota-resets-after` (hh:mm:ss). If the principal can see more than 10,000
    subscriptions, the results are limited and `x-ms-tenant-subscription-limit-hit: true`
    is returned. Microsoft recommends grouping queries (fewer than 300 subscriptions per
    group) and staggering them.
  - **Paging**: 1,000 records by default and at most 1,000 per page (`$top`/`first`).
    Further pages use `$skipToken`.
  - **Freshness**: "When an Azure resource is updated, Azure Resource Manager notifies
    Azure Resource Graph... also does a regular full scan... Azure Resource Graph data
    isn't strongly consistent. Data is indexed with a short latency."
  - **Subnets in ARG**: subnets are not a separate type in `resources`. Microsoft's
    starter sample uses `Resources | where type == 'microsoft.network/virtualnetworks' |
    mv-expand subnets = properties.subnets | project subnets.name,
    subnets.properties.addressPrefix, ...`. Because ARG stores the provider's GET payload,
    `properties.subnets[].properties.ipConfigurations` should be present and can be
    counted with `array_length()`. That is **Unverified** for very large arrays, and List
    Usage data is **not** available in ARG. NIC → subnet mapping is available from
    `microsoft.network/networkinterfaces` (documented sample).
  - **Change history**: the `resourcechanges` table keeps property-level change records
    (`changeType`, `changes`, `changedBy`). Documented sample queries cover "past seven
    days". Retention of 14 days is **Unverified**.

**Recommendation**: use ARG for periodic discovery of VNets and subnets across all visible
subscriptions (one query per group of up to about 300 subscriptions, paged). Use ARM GET
VNet and List Usage for the VNets that the operator manages or that the CR selects, and
always use ARM before a write. This matches AWS `DescribeVpcs`/`DescribeSubnets` plus a
fresh read.

**Confidence**: high for ARG cost, throttling, headers, freshness and paging. Medium for
counting `ipConfigurations` in ARG.

**Sources**
- ARG overview (free, throttling, how it is kept current): https://learn.microsoft.com/en-us/azure/governance/resource-graph/overview
- ARG throttling guidance: https://learn.microsoft.com/en-us/azure/governance/resource-graph/concepts/guidance-for-throttled-requests
- ARG large data sets / paging: https://learn.microsoft.com/en-us/azure/governance/resource-graph/concepts/work-with-data
- ARG starter samples (VNets and subnets): https://learn.microsoft.com/en-us/azure/governance/resource-graph/samples/starter
- ARG networking samples: https://learn.microsoft.com/en-us/azure/networking/resource-graph-samples
- ARG resource changes: https://learn.microsoft.com/en-us/azure/governance/resource-graph/how-to/get-resource-changes
- ARG Resources REST: https://learn.microsoft.com/en-us/rest/api/azureresourcegraph/resourcegraph/resources/resources
- Virtual Networks – List All: https://learn.microsoft.com/en-us/rest/api/virtualnetwork/virtual-networks/list-all

## Q4. Auth from Kubernetes

**Answer**

- **Workload identity federation**: "configure a user-assigned managed identity or app
  registration in Microsoft Entra ID to trust tokens from an external identity provider".
  A supported scenario is "Workloads running on any Kubernetes cluster (AKS, Amazon Web
  Services EKS, GKE, or on-premises)". Setup: create a federated identity credential with
  `issuer` = cluster OIDC issuer URL, `subject` = `system:serviceaccount:<ns>:<sa>` and
  `audience` = `api://AzureADTokenExchange`. Limits:
  - At most **20 federated identity credentials** per app or user-assigned managed
    identity. Each cluster/SA pair uses one.
  - The issuer and subject pair must be unique.
  - Only issuers that sign with RS256 are supported.
  - Propagation takes a few minutes, and early token requests can fail.
- **Go SDK**: `azidentity.NewWorkloadIdentityCredential(*WorkloadIdentityCredentialOptions)`
  (since v1.3.0). `ClientID`, `TenantID` and `TokenFilePath` default to `AZURE_CLIENT_ID`,
  `AZURE_TENANT_ID` and `AZURE_FEDERATED_TOKEN_FILE`, which the AKS workload-identity
  webhook sets. On EKS or GKE, set these values yourself and mount a projected SA token
  with that audience. `AdditionallyAllowedTenants` enables multitenant token requests.
- **Other subscriptions** in the same tenant: assign RBAC roles to the identity at the
  subscription or management-group scope. There is no assume-role step. One credential
  can reach every subscription it has roles in, and the ARM client takes the subscription
  ID per call.
- **Other tenants**: managed identities are single-tenant. "If you need to access
  resources in another tenant, your app registration must be a multitenant application
  and provisioned into the other tenant." The same page states that an app can be
  configured to trust a managed identity only when both are in the same tenant. Pattern:
  a multitenant app registration in the home tenant with a federated credential for the
  cluster SA. Create a service principal (admin consent) in each target tenant, assign
  RBAC there, and request a token with `TenantID = <target>`. AKS documents this as
  "cross-tenant workload identity".

**Confidence**: high.

**Sources**
- Workload identity federation: https://learn.microsoft.com/en-us/entra/workload-id/workload-identity-federation
- Considerations and limits (20 credentials, RS256, propagation): https://learn.microsoft.com/en-us/entra/workload-id/workload-identity-federation-considerations
- Trust between UAMI and external IdP: https://learn.microsoft.com/en-us/entra/workload-id/workload-identity-federation-create-trust-user-assigned-managed-identity
- App trusting a managed identity (same-tenant, multitenant note): https://learn.microsoft.com/en-us/entra/workload-id/workload-identity-federation-config-app-trust-managed-identity
- AKS cross-tenant workload identity: https://learn.microsoft.com/en-us/azure/aks/workload-identity-cross-tenant
- azidentity (WorkloadIdentityCredential): https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/sdk/azidentity

## Q5. Change events

**Answer**

- **Event Grid system topic for Azure subscriptions** (source `/subscriptions/{id}`, one
  system topic per subscription): event types include `Microsoft.Resources.ResourceWriteSuccess`,
  `ResourceWriteFailure`, `ResourceWriteCancel`, `ResourceDeleteSuccess`/`Failure`/`Cancel`
  and `ResourceActionSuccess`/`Failure`/`Cancel`.
  - "The event subject is the resource ID of the resource that is the target of the
    operation." Filter by resource type with a subject prefix such as
    `/subscriptions/<id>/resourcegroups/<rg>/providers/Microsoft.Network/virtualNetworks`.
    Because the resource group is in the path, a pure type filter across all resource
    groups needs **advanced filters**. Example: `data.operationName` StringIn
    `Microsoft.Network/virtualNetworks/write`,
    `Microsoft.Network/virtualNetworks/subnets/write`, `.../delete`.
  - Advanced filter limits: 25 filters and 25 values per subscription, and 512 characters
    per value.
  - The payload has no resource body (`resourceUri`, `operationName`, `status`,
    `authorization`, `claims`).
- **ARN "Azure Resource Management" system topic** (`Microsoft.ResourceNotifications.Resources.CreatedOrUpdated`
  / `Deleted`) has the same subscription scope but **includes the resource payload**
  (`resourceInfo.properties`, `tags`, `location`). It does "not yet support all the
  resource types" from ARG. It needs `Microsoft.ResourceNotifications/systemTopics/subscribeToResources/action`.
- **Do subnet writes raise events on the subnet ID?** A PUT or DELETE on
  `.../virtualNetworks/{v}/subnets/{s}` is an ARM write, so a `ResourceWriteSuccess` with
  subject = the subnet ID and `operationName = Microsoft.Network/virtualNetworks/subnets/write`
  is expected. **Unverified**: no Microsoft page shows a subnet example. Important: when a
  NIC or private endpoint consumes an IP, the write is on the NIC, not on the subnet. **IP
  usage changes therefore do not produce subnet events.** Tag writes through
  `Microsoft.Resources/tags` produce `Microsoft.Resources/tags/write` on the tags scope.
  The exact subject is **Unverified**.
- **Destinations**: Storage Queue and Service Bus queue are supported Event Grid handlers.
  The operator can poll either one. SQS maps most closely to a Storage Queue (simplest,
  Azurite-testable). A Service Bus queue gives dead-lettering and sessions.
- **Delivery**: "tries to deliver each message at least once for each matching
  subscription". Order is not guaranteed. The retry schedule runs 10 s, 30 s, 1 m, 5 m,
  10 m, 30 m, 1 h, 3 h, 6 h, then every 12 h up to 24 h. The retry policy allows at most 30
  attempts and a TTL of up to 1,440 min. Dead-lettering to a blob is optional, and
  "duplicates might still be received".
- **Alternative**: poll the ARG `resourcechanges` table on an interval, which covers the
  tenant in one query. It has higher latency and uses the ARG quota.

**Confidence**: high for event types, subject semantics, filtering and delivery. Medium
for subnet-level subjects (**Unverified**).

**Sources**
- Subscription system topic events: https://learn.microsoft.com/en-us/azure/event-grid/event-schema-subscriptions
- ARN Resources system topic: https://learn.microsoft.com/en-us/azure/event-grid/event-schema-resources
- Event filtering: https://learn.microsoft.com/en-us/azure/event-grid/event-filtering
- Delivery and retry: https://learn.microsoft.com/en-us/azure/event-grid/delivery-and-retry
- ARG resource changes: https://learn.microsoft.com/en-us/azure/governance/resource-graph/how-to/get-resource-changes

## Q6. Throttling

**Answer**

- **ARM** has used regional throttling with a **token bucket** since 2024:

  | Scope | Operations | Bucket | Refill/s |
  |---|---|---|---|
  | Subscription | reads | 250 | 25 |
  | Subscription | deletes | 200 | 10 |
  | Subscription | writes | 200 | 10 |
  | Tenant | reads | 250 | 25 |
  | Tenant | deletes | 200 | 10 |
  | Tenant | writes | 200 | 10 |

  These limits apply "per subscription, per service principal, and per operation type",
  plus global subscription limits of 15× across all principals. Free and trial
  subscriptions may be lower.
- **Headers**: `x-ms-ratelimit-remaining-subscription-reads|writes|deletes`,
  `x-ms-ratelimit-remaining-tenant-reads|writes` and
  `x-ms-ratelimit-remaining-subscription-resource-requests` (only when the resource
  provider overrides the limits).
- **429**: "The response includes a Retry-After value, which specifies the number of
  seconds your application should wait... If you send a request before the retry value
  elapses, your request isn't processed and a new retry value is returned."
- **Microsoft.Network** adds its own limits: write/delete (PUT) **1,000 per 5 minutes**
  and read (GET) **10,000 per 5 minutes**. It also "returns 429 with the
  `RetryableErrorDueToAnotherOperation` error code when another operation locks the target
  resource". **That is a lock conflict, not quota**, and the error body must be inspected.
- **Resource Graph** uses its own quota (Q3).
- **Go SDK**: `azcore/policy.RetryOptions` has MaxRetries (default 3), RetryDelay (800 ms,
  used "only if the HTTP response does not contain a Retry-After header") and
  MaxRetryDelay (60 s). The SDK therefore already honours `Retry-After`.
- **Mapping to the operator**:
  - Keep SDK retries low (for example `MaxRetries: 1–2`) so throttling surfaces to the
    controller.
  - Classify errors with `azcore.ResponseError`:
    - `StatusCode == 429`, with error code not `RetryableErrorDueToAnotherOperation` →
      `ErrThrottled` carrying `Retry-After`.
    - 429 with `RetryableErrorDueToAnotherOperation`, and 409
      `AnotherOperationInProgress` → a distinct `ErrConflictRetryable` with a short
      requeue.
  - Feed `x-ms-ratelimit-remaining-*` (ARM) and `x-ms-user-quota-remaining` (ARG) into the
    adaptive limiter so it slows down before hitting 429.
  - Keep one limiter per (subscription, principal, region, op-class). Keep a separate ARG
    limiter per principal.

**Confidence**: high.

**Sources**
- ARM throttling: https://learn.microsoft.com/en-us/azure/azure-resource-manager/management/request-limits-and-throttling
- ARG throttling: https://learn.microsoft.com/en-us/azure/governance/resource-graph/concepts/guidance-for-throttled-requests
- azcore policy.RetryOptions: https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/sdk/azcore/policy

## Q7. Test strategy

**Answer**

- **No networking emulator** is documented by Microsoft.
- **Azurite** "supports only the Blob, Queue, and Table storage services", which is enough
  for the Storage Queue event consumer (#55-style tests). An **Azure Service Bus
  emulator** also exists for local testing if Service Bus is chosen.
- **`armnetwork/vN/fake`**: at the time of writing, v12.0.0 was published 2026-09-16, and
  the REST reference samples use v11 with api-version 2025-09-01. The package provides one
  `XxxServer` struct per client, such as `VirtualNetworksServer` and `SubnetsServer`, whose
  **fields are function stubs**. Example: `SubnetsServer.BeginCreateOrUpdate func(ctx,
  rg, vnet, subnet string, params armnetwork.Subnet, opts) (azfake.PollerResponder[...],
  azfake.ErrorResponder)`.
  - `NewXxxServerTransport(&srv)` or `ServerFactory` + `NewServerFactoryTransport` return
    a `policy.Transporter` that is plugged into `arm.ClientOptions.Transport`. The real
    SDK client and its serialization, pagers and LRO pollers run against the fake.
  - There is **no built-in state**. Tests or an in-repo fake must implement storage,
    overlap checks, etags, usage counts and error injection (429 with Retry-After,
    `AnotherOperationInProgress`, overlap errors) through `azfake.ErrorResponder` /
    `SetResponseError`.
- **Plan**:
  1. Write an in-repo stateful fake: a map of VNets and subnets that implements the
     `fake.VirtualNetworksServer` and `fake.SubnetsServer` function fields plus
     `Microsoft.Resources/tags` (armresources fake), with fault injection. This mirrors the
     current Moto + fakes setup.
  2. Test the event consumer against Azurite Queue.
  3. Run a gated **conformance run** against a real sandbox subscription (nightly or
     manual). It should settle every **Unverified** item: List Usage semantics on
     delegated, overlay and IPv6 subnets; Tags API on a subnet scope; event subject for
     subnet writes; overlap and in-use error codes; `If-Match` behaviour; and whether VNet
     PATCH merges or replaces tags.

**Confidence**: high for the structure of the fake package and Azurite scope. The
testing plan itself is a recommendation.

**Sources**
- armnetwork fake (v12): https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/network/armnetwork/v12/fake
- Azurite: https://learn.microsoft.com/en-us/azure/storage/common/storage-use-azurite
- Service Bus emulator: https://learn.microsoft.com/en-us/azure/service-bus-messaging/overview-emulator

## Other model facts

- **Regional VNet, zone-less subnet**: "A virtual network is limited to a single region.
  But a virtual network does span availability zones." The Subnet schema has no
  `location` or zone. Zonal placement belongs to the resources inside the subnet, unlike
  AWS, where subnets are per-AZ. (VNet FAQ; Subnet schema.) High confidence.
- **Resource IDs**: `/subscriptions/{sub}/resourceGroups/{rg}/providers/Microsoft.Network/virtualNetworks/{vnet}/subnets/{subnet}`.
  The resource group is part of the identity. Subscription, RG and names are
  case-insensitive, so the operator must normalise IDs (lowercase) for map keys.
- **Subnet creation**: `PUT .../subnets/{name}` is a long-running operation (200 or 201
  with `Azure-AsyncOperation` and `Retry-After` headers), which the SDK exposes as
  `BeginCreateOrUpdate` + poller. Set `addressPrefix`, or `addressPrefixes` for dual-stack.
  IPv6 prefixes must be /64 (FAQ). "Subnet address spaces can't overlap one another"
  (FAQ).
  - The exact overlap error code (`NetcfgSubnetRangesOverlap` or similar) is
    **Unverified**; no Microsoft Learn page documents it.
  - `InUseSubnetCannotBeDeleted` is documented for deleting subnets that are still in
    use, including by service association links.
  - `sharingScope` and `defaultOutboundAccess` "can only be set if subnet is empty".
- **Concurrency**: Microsoft.Network serialises operations on a VNet and its children.
  Concurrent subnet PUTs on the same VNet fail with `AnotherOperationInProgress` (Q&A
  guidance: create subnets serially / `@batchSize(1)`) or with 429
  `RetryableErrorDueToAnotherOperation` (ARM throttling doc).
  - The Subnets – Create Or Update reference **does not document an `If-Match`
    header**, although every resource returns an `etag`. Whether Microsoft.Network honours
    `If-Match` for optimistic concurrency is **Unverified**. The operator must not rely on
    it without a conformance test.
  - **Unverified but widely reported**: a PUT on the whole VNet whose `subnets` array
    omits existing subnets tries to delete them. The operator must never PUT the VNet
    body. It should use the subnet child PUT and the tags APIs only.
- **Delegation**: `properties.delegations[]` (serviceName such as `Microsoft.Web/serverFarms`)
  dedicates a subnet to a service. The service may add network intent policies and
  `serviceAssociationLinks`, which block deletion. The operator should treat delegated
  subnets as service-managed for IP accounting and never delegate implicitly.
- **Sources**:
  - VNet FAQ: https://learn.microsoft.com/en-us/azure/virtual-network/virtual-networks-faq
  - Subnets – Create Or Update: https://learn.microsoft.com/en-us/rest/api/virtualnetwork/subnets/create-or-update
  - In-use subnet error (AKS troubleshooting): https://learn.microsoft.com/en-us/troubleshoot/azure/azure-kubernetes/create-upgrade-delete/cannot-delete-ip-subnet-nsg
  - AnotherOperationInProgress (Microsoft Q&A, not primary docs): https://learn.microsoft.com/en-us/answers/questions/259861/error-anotheroperationinprogress-during-vnet-creat
  - Subnet delegation: https://learn.microsoft.com/en-us/azure/virtual-network/subnet-delegation-overview

## Implications for the operator

For ADR #39 and issues #52–#56:

1. **Ownership model (ADR #39)**:
   - Azure cannot tag subnets, so the provider abstraction must not assume per-subnet
     tags. Define `OwnershipStore` per provider:
     - AWS: subnet tags.
     - Azure: VNet tags, using the reserved key `hs-managed` plus one key per subnet
       (`hs-subnet-<sanitised-name-or-hash>` → compact `owner|env|tier`).
   - The CR is the source of intent in both clouds.
   - Document the 50-tag budget and fail with a clear condition when there is no room.
2. **Additive tag writes (ResourceImport)**:
   - Use `Tags – Update At Scope` with `operation: Merge` on the VNet ID.
   - Never use VNet PATCH/PUT.
   - Before writing, read the tags and refuse to overwrite a different existing value.
     This keeps add-only semantics.
   - RBAC: **Tag Contributor**, plus `Network Contributor` (or a custom role with
     `Microsoft.Network/virtualNetworks/subnets/write|read`) only where Create mode is
     enabled.
3. **IP usage (#52/#53-type issue)**:
   - Use `VirtualNetworks – List Usage` once per VNet per reconcile.
   - `free = limit − currentValue`, with `-1` → unknown.
   - Surface a `ServiceManaged` or low-confidence flag when the subnet has `delegations`,
     `serviceAssociationLinks` or `resourceNavigationLinks`.
   - Keep `size − 5 − len(ipConfigurations)` only as a fallback.
   - Add conformance tests that pin the semantics.
4. **Discovery**:
   - Use ARG (tenant or management-group scope, groups of up to 300 subscriptions, paged
     by 1,000) for inventory.
   - Use ARM GET VNet + List Usage for managed VNets and before any write.
   - Treat ARG results as eventually consistent. Never use them for a create/overlap
     decision.
5. **Create mode**:
   - Use a subnet child PUT with LRO polling. Serialise writes per VNet with an
     in-process mutex keyed by VNet ID. This avoids `AnotherOperationInProgress` from
     our own requests.
   - Map 409 `AnotherOperationInProgress` and 429 `RetryableErrorDueToAnotherOperation`
     to a retryable-conflict error rather than `ErrThrottled`.
   - Validate overlap client-side against the fresh VNet GET before the PUT.
6. **Events (#55-type issue)**:
   - Subscription system topic (or ARN topic for payloads) → Storage Queue, with advanced
     filter on `data.operationName` for VNet and subnet write/delete and tags write.
   - Handlers must be idempotent and tolerate duplicates and out-of-order delivery.
     Events trigger a re-read and never carry the state themselves.
   - Keep periodic resync for IP usage, because NIC churn does not emit subnet events.
7. **Throttling**:
   - Classify `azcore.ResponseError` into `ErrThrottled` (429 quota, with Retry-After) or
     a retryable conflict.
   - Reduce SDK `MaxRetries` so the controller owns the backoff.
   - Use the remaining-quota headers to drive the adaptive limiter, keyed by
     (subscription, principal, op-class) for ARM and by principal for ARG.
8. **Auth (#54-type issue)**:
   - Use `WorkloadIdentityCredential` on every cluster type.
   - One UAMI or app per trust domain; at most 20 federated credentials per identity.
   - Cross-subscription access uses RBAC only.
   - Cross-tenant access uses a multitenant app registration plus a per-tenant service
     principal, and `TenantID` is selected per target in config.
9. **Tests (#56-type issue)**:
   - Build a stateful in-repo fake on top of `armnetwork/v12/fake` function fields, with
     fault injection.
   - Test the Storage Queue consumer with Azurite.
   - Add a gated real-subscription conformance suite covering the Unverified items below.

## Unverified / open

1. `List Usage` semantics: whether `limit` already excludes the 5 reserved addresses
   (the example `limit: 3` suggests yes); whether `currentValue` counts delegated or
   service-injected usage (App Service, SQL MI), AKS Pod Subnet blocks and IPv6; and when
   values other than GatewaySubnet return `-1`.
2. Whether `ipConfigurations` on GET subnet/VNet is ever truncated for very large subnets,
   and whether ARG keeps the full array.
3. How Azure Firewall, Bastion and VPN/ER gateway addresses appear in `ipConfigurations`
   for their special subnets.
4. The error returned by `Tags – Update At Scope` when the scope is a subnet ID (the type
   does not support tags).
5. Whether `VirtualNetworks – Update Tags` (PATCH) replaces or merges tags. The working
   assumption is replace.
6. The exact Event Grid subject and `operationName` for subnet PUT/DELETE, and the subject
   for `Microsoft.Resources/tags/write` events. Whether the ARN Resources topic covers
   `virtualNetworks` changes that include subnet edits.
7. The exact error code for overlapping subnet prefixes (`NetcfgSubnetRangesOverlap` is
   community-reported and not found on Microsoft Learn).
8. Whether Microsoft.Network honours `If-Match` (etag) on subnet PUT.
9. The behaviour of a VNet PUT that omits existing subnets (reported to attempt deletion).
10. Retention period of ARG `resourcechanges` (the docs show 7-day queries; 14 days is
    commonly cited).
11. ARG per-user quota values beyond the documented example (15 per 5 s), which are "subject
    to change".
