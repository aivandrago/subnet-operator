# IAM reference

Every permission the operator's cloud identities hold, per role, and the call that needs it. The
roles themselves are in the repository: [`deploy/iam/`](../../deploy/iam) for AWS (a policy and
two CloudFormation templates), [`deploy/gcp/`](../../deploy/gcp) for Google Cloud (custom role
definitions for `gcloud iam roles create --file`), [`deploy/azure/`](../../deploy/azure) for
Azure (custom role definitions for `az role definition create --role-definition`).
`test/deploy` holds them to the rules below: the read roles only read, the write roles never
delete, and the operator's own identity only reaches the per-account identities. When this page and a role file disagree, the role file is
what the tests check, and this page is wrong.

The GCP provider is part of 2.0; see the [GCP guide](../gcp.md). Its
permission names follow the Compute Engine and Resource Manager IAM references, and a run
against a real project (#50) still has to confirm the ones marked *to confirm*. The Azure
provider is part of 3.0; see the [Azure guide](../azure.md). Its
actions follow the Microsoft.Network and Microsoft.Resources operation references, and a run
against real subscriptions (#56) still has to confirm the ones marked *to confirm*.

## Side by side

| Job | AWS | GCP | Azure |
|---|---|---|---|
| The operator's own identity | a role in the hub account, attached with EKS Pod Identity or IRSA: [`operator-policy.json`](../../deploy/iam/operator-policy.json) | GKE Workload Identity or Workload Identity Federation: [`impersonator-role.yaml`](../../deploy/gcp/impersonator-role.yaml) on each service account it impersonates | Microsoft Entra Workload ID (a managed identity or app registration with a federated credential for the service account); it needs no role when every account names identities of its own |
| Reaching another account | `sts:AssumeRole` into `accounts[].aws.roleARN` / `writeRoleARN` | `iam.serviceAccounts.getAccessToken` on `accounts[].gcp.serviceAccount` / `writeServiceAccount` | no permission: the same service account token is exchanged for a token of `accounts[].azure.clientID` / `writeClientID` (in `tenantID`), each with a federated identity credential that trusts it |
| Discovery | [`spoke-readonly-role.cfn.yaml`](../../deploy/iam/spoke-readonly-role.cfn.yaml) | [`reader-role.yaml`](../../deploy/gcp/reader-role.yaml) | [`reader-role.json`](../../deploy/azure/reader-role.json) on the subscription, or on the scope's resource groups |
| Creating subnets, tagging | [`spoke-write-role.cfn.yaml`](../../deploy/iam/spoke-write-role.cfn.yaml) | [`writer-role.yaml`](../../deploy/gcp/writer-role.yaml) in the project, and [`tag-user-role.yaml`](../../deploy/gcp/tag-user-role.yaml) on the tag keys | [`writer-role.json`](../../deploy/azure/writer-role.json); imports alone need only Tag Contributor |
| Creating tag values | — (a tag value is free text) | [`tag-value-creator-role.yaml`](../../deploy/gcp/tag-value-creator-role.yaml), only with `spec.gcp.createTagValues` | — (a tag value is free text) |
| Change events (optional) | SQS in `operator-policy.json` | [`events-subscriber-role.yaml`](../../deploy/gcp/events-subscriber-role.yaml) on the Pub/Sub subscription | the built-in Storage Queue Data Message Processor role on the Storage queue ([`deploy/azure/events.md`](../../deploy/azure/events.md)) |
| Never granted | any `Delete*`, `DeleteTags`, IAM changes | any `delete`, `tagValueBindings.delete`, `tagValues.delete`/`update`, `setIamPolicy` | any `*/delete`, `Microsoft.Network/virtualNetworks/write`, `Microsoft.Authorization/*` |

## AWS

### Operator policy (`deploy/iam/operator-policy.json`)

Attached to the operator's own role in the hub account.

| Permission | Why |
|---|---|
| `ec2:DescribeVpcs`, `ec2:DescribeSubnets`, `ec2:DescribeRouteTables` | discovery of the account the operator runs in, for scope accounts without a `roleARN` |
| `sts:AssumeRole` on `aws-subnet-operator-readonly` and `aws-subnet-operator-write` | reaching every other account through its read or write role |
| `sqs:ReceiveMessage`, `sqs:DeleteMessage`, `sqs:GetQueueAttributes` on `aws-subnet-operator-events` | change events: reading the hub queue and deleting what was handled (the only delete the operator makes, on its own queue) |

### Read-only role (`deploy/iam/spoke-readonly-role.cfn.yaml`)

`aws-subnet-operator-readonly`, trusted by the operator's role (and an optional external ID), in
every account a scope lists.

| Permission | Why |
|---|---|
| `ec2:DescribeVpcs` | the account's VPCs in each region |
| `ec2:DescribeSubnets` | their subnets, with free addresses |
| `ec2:DescribeRouteTables` | whether a subnet is public (a default route to an internet gateway) |

### Write role (`deploy/iam/spoke-write-role.cfn.yaml`)

`aws-subnet-operator-write`, only in the accounts where the operator may create subnets or tag.
Every write is limited to VPCs, subnets and route tables of the account.

| Permission | Why |
|---|---|
| `ec2:CreateSubnet` | `SubnetClaim` in `Create` mode, with the tags given at creation |
| `ec2:CreateTags` | tags on create, and `ResourceImport`s (a VPC or a subnet only) |
| `ec2:ModifySubnetAttribute` | `aws.mapPublicIPOnLaunch` of a claim |
| `ec2:AssociateRouteTable` | `aws.routeTableID` of a claim |
| `ec2:DescribeSubnets`, `ec2:DescribeVpcs`, `ec2:DescribeRouteTables` | reading back what the write path touches |

## Google Cloud

A Google Cloud project is reached either with the operator's own identity, which then needs the
reader (and for writes the writer and tag roles) itself, or through service accounts named on
the scope's account entry, which the own identity impersonates. The roles are the same either
way; only who holds them differs.

### Impersonator (`deploy/gcp/impersonator-role.yaml`)

Granted to the operator's own identity (the GKE Workload Identity service account, the service
account a Workload Identity Federation configuration impersonates, or the federated principal)
**on each service account** it impersonates, not on a project. The IAM Service Account
Credentials API (`iamcredentials.googleapis.com`) must be enabled in the own identity's project.

| Permission | Why |
|---|---|
| `iam.serviceAccounts.getAccessToken` | `generateAccessToken` for `accounts[].gcp.serviceAccount` and `writeServiceAccount`: an hour-long token, renewed five minutes before it expires. Narrower than `roles/iam.serviceAccountTokenCreator`: no signing of blobs or JWTs, no ID tokens |

### Reader (`deploy/gcp/reader-role.yaml`)

Granted to each account's `gcp.serviceAccount`, or to the operator's own identity for accounts
without one, in every project a scope lists (for a Shared VPC, the host project). Granted on a
folder or the organization, it covers every project below.

| Permission | Call | Why |
|---|---|---|
| `compute.networks.list` | `networks.list` | the project's VPC networks, once per target (they are global), with their peerings |
| `compute.subnetworks.list` | `subnetworks.list`, `views=WITH_UTILIZATION` | the region's subnetworks with free addresses per range (*to confirm* that the utilization view needs nothing more) |
| `compute.networks.listEffectiveTags` | `effectiveTags.list` on a network | ownership of each network |
| `compute.subnetworks.listEffectiveTags` | `effectiveTags.list` on a subnetwork, at the region's Resource Manager endpoint | ownership of each subnetwork |
| `resourcemanager.projects.get` | `projects.get` | the project number, which Resource Manager names resources by; cached |

Two things outside the role, found in the code: when `spec.gcp.tagParent` is
`projects/<project ID>`, discovery also looks that project's number up, so the read identity
needs `resourcemanager.projects.get` on the tag parent project too, if it is not one of the
scope's projects. And if tags come back without their names, grant
`roles/resourcemanager.tagViewer` on the tag parent (*to confirm*).

### Writer (`deploy/gcp/writer-role.yaml`)

Granted to each account's `gcp.writeServiceAccount` (or the operator's own identity, for an
account the operator reads with it) in the host project. Never to the read identity.

| Permission | Call | Why |
|---|---|---|
| `compute.subnetworks.create` | `subnetworks.insert` | `SubnetClaim` in `Create` mode |
| `compute.networks.updatePolicy` | `subnetworks.insert` | how Compute checks adding a subnetwork to a network; not a change of the network |
| `compute.subnetworks.createTagBinding` | `subnetworks.insert` with `params.resourceManagerTags`; `tagBindings.create` | tags bound at creation, and imports of a subnetwork (*to confirm*: the name follows the Compute IAM reference) |
| `compute.networks.createTagBinding` | `tagBindings.create` | imports of a network (*to confirm*, as above) |
| `compute.regionOperations.get` | the insert's operation | waiting for the subnetwork to exist |
| `compute.subnetworks.get` | `subnetworks.get` | reading back a subnetwork whose name is taken (an earlier attempt's), and resolving a subnetwork an import names |
| `compute.networks.get` | `networks.get` | resolving a network an import names to its numeric ID |
| `compute.subnetworks.listEffectiveTags`, `compute.networks.listEffectiveTags` | `effectiveTags.list` | reading what is bound before binding, so that a conflicting value refuses the whole write |
| `compute.subnetworks.listTagBindings`, `compute.networks.listTagBindings` | none today | kept for the tag bindings API; the code reads bindings through `effectiveTags.list` |
| `resourcemanager.projects.get` | `projects.get` | the project number of the resource to bind to (and of a `projects/<ID>` tag parent, which needs this permission there too) |

### Tag user (`deploy/gcp/tag-user-role.yaml`)

Granted to the write identity on the operator's tag keys (`hs-owner`, `hs-env`, `hs-tier`,
`hs-managed`, `hs-managed-by`, `hs-claim` with `spec.gcp.claimTag: Bind`, and any key a claim's
`spec.tags` or an import names),
or on the scope's `gcp.tagParent`. It is `roles/resourcemanager.tagUser` without removing
bindings; if a custom role may not carry `resourcemanager.tagValueBindings.create` (*to
confirm*), `roles/resourcemanager.tagUser` is the fallback.

| Permission | Call | Why |
|---|---|---|
| `resourcemanager.tagKeys.get` | `tagKeys.getNamespaced` | resolving `<parent>/<key>`; a key that does not exist is `TagKeyMissing` |
| `resourcemanager.tagValues.get` | `tagValues.getNamespaced` | resolving `<parent>/<key>/<value>`; a value that does not exist is `TagValueMissing` |
| `resourcemanager.tagKeys.list`, `resourcemanager.tagValues.list` | none today | part of the role's read side, as in `tagUser` |
| `resourcemanager.tagValueBindings.create` | `subnetworks.insert` with tags; `tagBindings.create` | binding a value to a network or subnetwork |

### Tag value creator (`deploy/gcp/tag-value-creator-role.yaml`)

Only for scopes with `spec.gcp.createTagValues: true`. Granted to the write identity **on the
operator's tag keys only**, never on the organization, so that it cannot create values of
anybody else's keys. Tag keys are never created by the operator.

| Permission | Call | Why |
|---|---|---|
| `resourcemanager.tagValues.create` | `tagValues.create` | a value a claim or import needs that does not exist yet, such as a new owner, or a claim's `hs-claim` value with `spec.gcp.claimTag: Bind`; a key holding 1,000 values refuses it (`TagValueLimitReached`) |
| `resourcemanager.tagKeys.get`, `resourcemanager.tagValues.get`, `resourcemanager.tagValues.list` | lookups | resolving the key, and the value a concurrent writer created in the meantime |

### Events subscriber (`deploy/gcp/events-subscriber-role.yaml`)

Only with change events (`--gcp-events-subscription`, the chart's
`providers.gcp.events.subscription`). Granted to the operator's own identity **on the
subscription**, not on the project; only the leader pulls. The log sink, the topic and the
publisher grant for the sink's writer identity are part of the setup in
[`deploy/gcp/events.md`](../../deploy/gcp/events.md), not permissions of the operator.

| Permission | Call | Why |
|---|---|---|
| `pubsub.subscriptions.consume` | streaming pull, acknowledge, modify ack deadline | reading the audit log entries of network and tag binding changes. Narrower than `roles/pubsub.subscriber`, which also allows attaching subscriptions to topics and seeking to snapshots |

### Tag keys and their owner

Creating the tag keys under `gcp.tagParent` is a one-time job for someone holding
`roles/resourcemanager.tagAdmin` there, not for the operator; the [GCP guide](../gcp.md#5-tag-keys-and-values)
has the commands.

## Azure

A subscription is reached either with the operator's own identity, which then needs the reader
(and for writes the writer) role itself, or with identities named on the scope's account entry
(`accounts[].azure.clientID`, `writeClientID`, `tenantID`). Azure has no AssumeRole: every
identity, the operator's own included, is a user-assigned managed identity or an app
registration with a federated identity credential (issuer: the cluster's service account
issuer; subject: `system:serviceaccount:<namespace>:<service account>`; audience:
`api://AzureADTokenExchange`), and the operator exchanges its one service account token for a
token of whichever identity a target uses. Reaching an identity is therefore no permission of
the operator's but the identity's trust; the roles below are the same whoever holds them. Setup
with the az CLI and Terraform: [`deploy/azure/README.md`](../../deploy/azure/README.md); the
whole path, from the identities to a first scope, in the
[Azure guide](../azure.md#3-read-and-write-identities-per-subscription).

**Least privilege, per subscription.** A custom role is defined once, with the subscriptions or
management groups it may be assigned in as its `AssignableScopes`, and assigned where it is
needed: the reader on each subscription of a scope (or on a management group above them, or
only on the resource groups a scope names), the writer only where claims create subnets and
imports tag, down to the resource groups of the virtual networks. An identity may hold the
roles in many subscriptions; a subscription read through `azure.clientID` without an
`azure.writeClientID` is never written to (`NoWriteRole`), also not by the operator's own
identity.

### Reader (`deploy/azure/reader-role.json`)

Assigned to each account's `azure.clientID`, or to the operator's own identity for accounts
without one, on every subscription a scope lists, or on a management group above them, or only
on the resource groups of a scope that names them (`spec.azure.resourceGroups`).
`test/deploy/azure_iam_test.go` runs discovery and the read before an import against the in-repo
fake and checks that the role holds the action of every call they make.

| Action | Call | Why |
|---|---|---|
| `Microsoft.Network/virtualNetworks/read` | `virtualNetworks` List All (per subscription), or List (per resource group) | the virtual networks with their subnets, address prefixes and tags |
| `Microsoft.Network/virtualNetworks/read` | `virtualNetworks` Get, once per attempt of a `ResourceImport` that is not `Applied` yet | that the virtual network, and the subnet an import names, exist, and the location they are in; read as this identity so that an import's write identity needs the Tags API alone |
| `Microsoft.Network/virtualNetworks/usages/read` | `virtualNetworks/{name}/usages` | addresses used per subnet |
| `Microsoft.Network/virtualNetworks/subnets/read` | none today | subnets come inline with their virtual network; kept in case ARM filters them without it (*to confirm*, #56) |
| `Microsoft.Resources/subscriptions/locations/read` (from 3.2) | Subscriptions List Locations, at most once a day per subscription, and only for a scope with a location that has no virtual network | whether such a location exists: one that does not fails its target instead of syncing empty (#119) |

**A custom reader role created before 3.2 does not hold
`Microsoft.Resources/subscriptions/locations/read`: update it** (`az role definition update`
with the 3.2 file). Until then nothing fails: the targets sync as in 3.1, location names are not
checked, and the scope gets a `TargetWarning` Event that says so
([runbook](../operations/runbook.md#targetwarning-on-azure-location-names-are-not-checked)).
The built-in Reader role has the action. It is a call on the subscription, so a role assigned
only on resource groups may not be enough for it (*to confirm*, #56), with the same result.

`Microsoft.Resources/subscriptions/resourceGroups/read` is not needed: discovery never reads a
resource group itself, and a group that does not exist fails the target either way.

### Writer (`deploy/azure/writer-role.json`)

Assigned to each account's `azure.writeClientID` (or the operator's own identity, for an
account the operator reads with it) where claims create subnets and imports tag, on the
subscription or the network resource groups. Never to the read identity. The same test runs
the writes against azurefake and checks that the role holds the action of every call they make.

| Action | Call | Why |
|---|---|---|
| `Microsoft.Network/virtualNetworks/subnets/write` | `subnets` Create Or Update with `If-None-Match: *` | `SubnetClaim` in `Create` mode; never an update of an existing subnet |
| `Microsoft.Network/locations/operations/read` | the create's `Azure-AsyncOperation` | waiting for the subnet to exist |
| `Microsoft.Network/virtualNetworks/subnets/read` | `subnets` Get | whether the name is taken, and by the claim's own earlier attempt |
| `Microsoft.Resources/tags/read` | Tags `Get At Scope` on the virtual network | what is there before writing, so a conflicting value refuses the whole write |
| `Microsoft.Resources/tags/write` | Tags `Update At Scope` (`Merge`) on the virtual network | ownership: the network's tags and the `hs-subnet-<name>` entries, without `virtualNetworks/write` |
| `Microsoft.Network/virtualNetworks/read` | none on the write path today | part of reading the network, as the reader role |

Imports alone need only the two `Microsoft.Resources/tags` actions, which the built-in Tag
Contributor role grants: the read of the virtual network before an import is the read
identity's (above). A claim that associates a route table or network security group
would also need their `join/action` (*to confirm*, #56).

### Change events (the built-in Storage Queue Data Message Processor role)

Only with change events (`--azure-events-queue-url`, the chart's
`providers.azure.events.queueUrl`). Assigned to the operator's own identity **on the queue**,
not on the storage account or a subscription, by `deploy/azure/events/queue.bicep`. It is a
built-in role with data actions only, so there is no custom role file for it.

| Data action | Call | Why |
|---|---|---|
| `Microsoft.Storage/storageAccounts/queueServices/queues/messages/process/action` | Get Messages, Delete Message | receiving the events and deleting the ones that are done with |
| `Microsoft.Storage/storageAccounts/queueServices/queues/messages/read` | none (Peek Messages) | part of the built-in role; the operator never peeks |

The operator holds no account key and no SAS token, and cannot add messages. Creating the
system topics, the event subscriptions and the topics' `Storage Queue Data Message Sender`
assignment is the setup in [`deploy/azure/events.md`](../../deploy/azure/events.md), not
permissions of the operator. That the role is enough against a real queue is *to confirm*
(#56).
