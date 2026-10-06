# Azure change events: Event Grid → Storage queue

Optional, recommended: without change events the operator sees a change at the next resync
(`resyncInterval`, 10m by default); with them, within seconds. It is the Azure counterpart of
the AWS EventBridge → SQS path (`deploy/events/`) and of the GCP audit logs → Pub/Sub path
(`deploy/gcp/events.md`).

```
 PUT, PATCH, DELETE on            Event Grid system topic      event subscription:          Storage queue
 management.azure.com     ─────►  of the subscription     ───► write and delete successes ─► subnet-operator-events
 (every resource of the           (one per subscription)       of virtual networks,                │
  subscription)                                                 as the topic's own identity         ▼
                                    operator ◄── Get Messages / Delete Message, as its own identity ┘
```

Azure Resource Manager publishes an event for every write and delete it carries out, to the
subscription's Event Grid system topic; nothing needs enabling in the subscriptions, and the
event subscription's filter keeps the queue to the rate of virtual network changes. The
operator:

- reads `Microsoft.Resources.ResourceWriteSuccess` and `ResourceDeleteSuccess` events whose
  subject is a virtual network or anything below one: a subnet, a peering, and the network's
  tags, which is where the ownership of its subnets lives. Failed and cancelled operations,
  action events (a NIC joining a subnet) and every other resource type are ignored; free IP
  counts follow the resync, as on AWS and GCP. Both delivery schemas are read (Event Grid's own
  and CloudEvents 1.0), and resource IDs are matched without regard to case;
- resyncs the subscription and location the network is in. **An event names the resource and
  no location**, so the operator looks the network up in what its last discovery of the
  subscription listed, which costs no call. A network that listing did not have (just created,
  or outside the scope's `spec.azure.resourceGroups`) resyncs every location of the
  subscription that a `NetworkScope` names. Events of subscriptions no scope names are ignored.
  Events are collected for `--azure-events-debounce` (10s) so a burst of calls causes one resync
  per target;
- deletes a message once the resync it asked for is enqueued, and at once when it reports
  nothing or cannot be read. Until then the message stays in the queue, hidden, and comes back
  if the operator stops. A message that was delivered more than 5 times is dropped unread, so a
  message whose delete keeps failing cannot cause a resync for ever;
- counts every message in `hs_change_events_total{provider="azure"}` by result (`resync`,
  `ignored`, and `malformed` for the unreadable, the oversized and the ones delivered too
  often). A failed receive or delete (no such queue, a missing role assignment, no token) is
  logged, counted in `hs_change_event_errors_total{provider="azure"}` and retried, a receive
  with backoff up to a minute; the periodic resync keeps running meanwhile. The alert is
  [SubnetInventoryChangeEventsFailing](../../docs/operations/runbook.md#subnetinventorychangeeventsfailing).

A Storage queue has no long polling: the operator asks every 5 seconds while the queue is
empty, and again at once while it is not. Only the leader polls. It does not record who created
a network, unlike on AWS and GCP: a write event does not say whether it created or updated, so
the auto-import policy's `fromCreator` rules do not apply to Azure.

## Why a Storage queue

Event Grid delivers to a Storage queue or to a Service Bus queue. The operator reads a Storage
queue only:

| | Storage queue | Service Bus queue |
|---|---|---|
| Read as the operator's Workload Identity, no keys | yes: `Storage Queue Data Message Processor` on the queue | yes: `Azure Service Bus Data Receiver` |
| Client | REST over HTTPS on 443, a small SDK module | AMQP (5671, or WebSockets on 443), a second protocol stack in the operator and through the NetworkPolicy and proxies |
| Delivery | at least once, unordered; a received message is hidden for a visibility timeout | at least once, peek-lock; ordering and sessions the operator has no use for |
| Dead-lettering | none; the operator drops a message after 5 deliveries | built in |
| Waiting for a message | polling (every 5s when idle) | long polling |
| Cost | per transaction: about 17,000 polls a day when idle, a few cents a month | a namespace billed by the hour or per operation |
| Message size | 64 KiB, about 48 KiB of event after Base64 | 256 KiB |

Events are hints, as on the other clouds: a lost or late one costs a wait for the resync, never
a wrong inventory, so what Service Bus adds (ordering, dead-letter queues, larger messages) buys
nothing here, and the 5 seconds of polling disappear in the 10 seconds of debounce. An event
larger than the queue takes is retried by Event Grid and then dropped; the resync covers it.

## Setup

Two Bicep templates in [`events/`](events): [`queue.bicep`](events/queue.bicep) once, and
[`topic.bicep`](events/topic.bicep) in every subscription the operator discovers.

```sh
OPS_SUB=11111111-0000-4000-8000-000000000001     # holds the queue
RG=rg-subnet-operator
# The object (principal) ID of the operator's own identity, not its client ID.
OPERATOR=$(az identity show --resource-group rg-identities --name subnet-operator --query principalId -o tsv)

az deployment group create --subscription "$OPS_SUB" --resource-group "$RG" \
  --template-file deploy/azure/events/queue.bicep \
  --parameters storageAccountName=subnetopevents$RANDOM operatorPrincipalId="$OPERATOR" \
  --query properties.outputs
```

It creates a storage account without shared key access, the queue, and the role assignment
that lets the operator's identity receive and delete its messages, on the queue only. It prints
`queueUrl` and `storageAccountId`.

```sh
STORAGE_ACCOUNT_ID=...   # the storageAccountId output above
for SUB in 5b3c2a10-0000-4000-8000-00000000a2e1 5b3c2a10-0000-4000-8000-00000000a2e2; do
  az provider register --subscription "$SUB" --namespace Microsoft.EventGrid --wait
  az group create --subscription "$SUB" --name "$RG" --location westeurope
  az deployment group create --subscription "$SUB" --resource-group "$RG" \
    --template-file deploy/azure/events/topic.bicep --parameters storageAccountId="$STORAGE_ACCOUNT_ID"
done
```

It creates the subscription's system topic with a managed identity of its own, grants that
identity `Storage Queue Data Message Sender` on the queue, and subscribes the queue to:

```
includedEventTypes: Microsoft.Resources.ResourceWriteSuccess, Microsoft.Resources.ResourceDeleteSuccess
advancedFilters:    subject StringContains /providers/Microsoft.Network/virtualNetworks/
```

### One scope's resource groups only

A scope that names `spec.azure.resourceGroups` discovers the virtual networks of those groups
and no others, but the event subscription above still delivers the events of every virtual
network of the subscription: the operator reads each of them, finds a network it does not know,
and resyncs every location of the subscription. In a busy subscription that is a resync for
every change somebody else makes. Give the template the same groups, and Event Grid delivers
only theirs:

```sh
az deployment group create --subscription "$SUB" --resource-group "$RG" \
  --template-file deploy/azure/events/topic.bicep \
  --parameters storageAccountId="$STORAGE_ACCOUNT_ID" resourceGroupNames='["rg-network-prod","rg-hub"]'
```

```
advancedFilters:    subject StringContains   /providers/Microsoft.Network/virtualNetworks/
                    subject StringBeginsWith /subscriptions/<id>/resourceGroups/rg-network-prod/,
                                             /subscriptions/<id>/resourceGroups/rg-hub/
```

Both filters must match, and the second matches when any of its values does. Keep the list the
same as the scope's: a group in the scope and not in the filter is still discovered, only at
the resync interval, and nothing says so. Several scopes over one subscription share its one
event subscription, so name the groups of all of them, or leave the parameter out if one of
them discovers the whole subscription. An event subscription takes 25 filter values in all, so
the template takes at most 24 groups; for more, deploy it a second time with another
`eventSubscriptionName`. Event Grid's advanced filters compare strings without regard to case,
which this relies on: Azure does not spell `resourceGroups`, or a group's name, the same way in
every subject.

A subscription has one system topic for its resource events; if it has one already, pass its
name as `systemTopicName` (it must have a system-assigned identity). Whoever deploys needs
`Microsoft.EventGrid/*` in the watched subscription and
`Microsoft.Authorization/roleAssignments/write` on the queue; the operator gets neither.

Then set the chart values (the operator flags are `--azure-events-queue-url` and
`--azure-events-debounce`):

```yaml
providers:
  azure:
    enabled: true
    events:
      queueUrl: https://<storage account>.queue.core.windows.net/subnet-operator-events
      debounce: 10s
```

The operator's own identity reads the queue (`providers.azure.workloadIdentity.clientId`), also
when every subscription is read with identities of its own (`accounts[].azure.clientID`): the
queue belongs to the operator, not to a subscription of a scope. The URL carries no SAS token;
the operator and the chart refuse one.

## Who may write to the queue

Anything in the queue is read as a resource event and decides which targets are resynced
early; nothing else of a message is used. Keep `Storage Queue Data Message Sender` to the
system topics' identities, shared key access off, and nothing broader than
`Storage Queue Data Message Processor` for the operator
([threat model](../../docs/security/threat-model.md#b4c--event-grid--storage-queue--operator-azure)).

## Network policy

With `networkPolicy.enabled`, the polling goes to `<storage account>.queue.core.windows.net:443`:
allowed by default, or through `providers.azure.apiCIDRs` when the egress is narrowed (the
`Storage.<region>` service tag, or the account's private endpoint).

## Still to be confirmed against real subscriptions (#56)

The event fields are from Microsoft's schema reference; no subscription has been watched yet.
In particular: the subject of a subnet write and of a tag write through the Tags API, that
Event Grid writes the message as Base64, and that a system topic's identity may deliver to a
queue in another subscription. For the resource group filter (`resourceGroupNames`): that
`StringBeginsWith` matches a subject whatever case Azure spells `resourceGroups` and the group's
name in, as Event Grid documents for its advanced filters, that the subject of a tag write
through the Tags API begins with the virtual network's ID like any other, and that 25 is the
number of filter values an event subscription takes. Until then a change the events miss waits for the resync.
