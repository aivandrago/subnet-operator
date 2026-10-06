# GCP change events: audit logs → log sink → Pub/Sub

Optional, recommended: without change events the operator sees a change at the next resync
(`resyncInterval`, 10m by default); with them, within seconds. It is the GCP counterpart of the
AWS EventBridge → SQS path (`deploy/events/`).

```
 compute.networks.*, compute.subnetworks.*,          aggregated log sink         Pub/Sub topic
 Resource Manager tag bindings  ─► Admin Activity ─► (organization or folder, ─► subnet-operator-events
 (every project below the org)      audit logs        or one per project)                │
                                                                                          ▼
                                     operator ◄── streaming pull, as its own identity ── subscription
```

Admin Activity audit logs are always on and free, and the sink routes only what the operator
reads, so nothing needs enabling in the projects and the Pub/Sub volume is the rate of network
changes. The operator:

- reads the entries of `compute.googleapis.com` for networks and subnetworks (insert, delete,
  patch, expandIpCidrRange, peering, ...; reads and IAM policy calls are ignored) and of
  `cloudresourcemanager.googleapis.com` for `CreateTagBinding` and `DeleteTagBinding` on a
  network or subnetwork;
- resyncs the project and region the entry is about: a subnetwork's region, or every region of
  the project for a network, which is global. Only projects and regions of a `NetworkScope`
  are resynced, and entries are collected for `--gcp-events-debounce` (10s) so a burst of calls
  causes one resync per target. A long-running operation logs at its start and at its end; the
  second entry's resync sees the result;
- remembers who inserted a network or subnetwork (`authenticationInfo.principalEmail`), for the
  auto-import policy's `fromCreator` rules;
- acknowledges every message once read, including the ones it cannot read or does not need,
  and counts them in `hs_change_events_total{provider="gcp"}` by result. A failed pull (a
  deleted subscription, a missing grant) is logged, counted in
  `hs_change_event_errors_total{provider="gcp"}` and retried with backoff; the periodic resync
  keeps running meanwhile. The alert is
  [SubnetInventoryChangeEventsFailing](../../docs/operations/runbook.md#subnetinventorychangeeventsfailing).

Only the leader pulls. The operator's own identity reads the subscription: the GKE Workload
Identity service account, the service account a Workload Identity Federation configuration
impersonates, or the federated principal itself (the chart's `providers.gcp.workloadIdentity`
and `providers.gcp.wif`). It needs `pubsub.subscriptions.consume` on the subscription:
`roles/pubsub.subscriber`, or the narrower custom role in
[events-subscriber-role.yaml](events-subscriber-role.yaml). Nothing else of the setup is granted
to the operator.

Tag bindings name their resource by project number. The operator maps the number to the
project ID of a project it has discovered, falling back to the project the entry was logged in.
That Resource Manager logs tag binding changes to the project of the bound resource is still to
be confirmed against a real organization (#50); until then, a tag change may wait for the
resync.

## With gcloud

The sink is aggregated at the organization (or a folder, with `--folder`), so it covers every
project below it, including the ones created later. `OPS_PROJECT_ID` holds the topic and the
subscription; `OPERATOR` is the operator's own identity as an IAM member
(`serviceAccount:subnet-operator@OPERATOR_PROJECT_ID.iam.gserviceaccount.com`, or a
`principal://` of a workload identity pool).

```sh
FILTER='logName:"cloudaudit.googleapis.com%2Factivity" AND (
  (protoPayload.serviceName="compute.googleapis.com" AND
    (protoPayload.methodName:".compute.networks." OR protoPayload.methodName:".compute.subnetworks."))
  OR (protoPayload.serviceName="cloudresourcemanager.googleapis.com" AND
    protoPayload.methodName:"TagBinding"))'

gcloud pubsub topics create subnet-operator-events --project=OPS_PROJECT_ID \
  --message-retention-duration=1d

# The subscription keeps undelivered events for a day and never expires, so an operator that
# was down for a while catches up (the resync covers anything older anyway).
gcloud pubsub subscriptions create subnet-operator-events --project=OPS_PROJECT_ID \
  --topic=subnet-operator-events --ack-deadline=30 \
  --message-retention-duration=1d --expiration-period=never

gcloud logging sinks create subnet-operator-events \
  pubsub.googleapis.com/projects/OPS_PROJECT_ID/topics/subnet-operator-events \
  --organization=ORG_NUMBER --include-children --log-filter="$FILTER"

# The sink publishes as its own writer identity, which the create command prints.
WRITER=$(gcloud logging sinks describe subnet-operator-events --organization=ORG_NUMBER \
  --format='value(writerIdentity)')
gcloud pubsub topics add-iam-policy-binding subnet-operator-events --project=OPS_PROJECT_ID \
  --role=roles/pubsub.publisher --member="$WRITER"

gcloud pubsub subscriptions add-iam-policy-binding subnet-operator-events \
  --project=OPS_PROJECT_ID --role=roles/pubsub.subscriber --member="$OPERATOR"
```

Without access to the organization, create one sink per project instead
(`gcloud logging sinks create ... --project=PROJECT_ID`, same filter and topic) and grant each
sink's writer identity the publisher role on the topic.

Then set the chart value (the operator flag is `--gcp-events-subscription`):

```yaml
providers:
  gcp:
    enabled: true
    events:
      subscription: projects/OPS_PROJECT_ID/subscriptions/subnet-operator-events
      debounce: 10s
```

## With Terraform

The same, with the Google provider:

```hcl
variable "org_id" { type = string }            # organization number
variable "ops_project" { type = string }       # holds the topic and the subscription
variable "operator_member" { type = string }   # e.g. "serviceAccount:subnet-operator@p.iam.gserviceaccount.com"

locals {
  filter = <<-EOT
    logName:"cloudaudit.googleapis.com%2Factivity" AND (
      (protoPayload.serviceName="compute.googleapis.com" AND
        (protoPayload.methodName:".compute.networks." OR protoPayload.methodName:".compute.subnetworks."))
      OR (protoPayload.serviceName="cloudresourcemanager.googleapis.com" AND
        protoPayload.methodName:"TagBinding"))
  EOT
}

resource "google_pubsub_topic" "events" {
  project                    = var.ops_project
  name                       = "subnet-operator-events"
  message_retention_duration = "86400s"
}

resource "google_pubsub_subscription" "events" {
  project                    = var.ops_project
  name                       = "subnet-operator-events"
  topic                      = google_pubsub_topic.events.id
  ack_deadline_seconds       = 30
  message_retention_duration = "86400s"
  expiration_policy {
    ttl = "" # never expires
  }
}

resource "google_logging_organization_sink" "events" {
  name             = "subnet-operator-events"
  org_id           = var.org_id
  include_children = true
  destination      = "pubsub.googleapis.com/${google_pubsub_topic.events.id}"
  filter           = local.filter
}

resource "google_pubsub_topic_iam_member" "sink_publishes" {
  project = var.ops_project
  topic   = google_pubsub_topic.events.name
  role    = "roles/pubsub.publisher"
  member  = google_logging_organization_sink.events.writer_identity
}

resource "google_pubsub_subscription_iam_member" "operator_subscribes" {
  project      = var.ops_project
  subscription = google_pubsub_subscription.events.name
  role         = "roles/pubsub.subscriber"
  member       = var.operator_member
}

output "subscription" {
  description = "Set as the chart value providers.gcp.events.subscription."
  value       = google_pubsub_subscription.events.id
}
```

For a folder, `google_logging_folder_sink` takes the same arguments with `folder` instead of
`org_id`; for single projects, `google_logging_project_sink` with `unique_writer_identity = true`.

## Who may publish

Anything published to the topic is read as an audit log entry. It decides which targets are
resynced early and, for an insert, who is recorded as the creator, which the auto-import policy
may turn into tags. Keep `roles/pubsub.publisher` on the topic to the sink's writer identity,
and nothing broader than `roles/pubsub.subscriber` for the operator on the subscription
([threat model](../../docs/security/threat-model.md#b4b--log-sink--pubsub--operator-gcp)).

## Network policy

With `networkPolicy.enabled`, the pull goes to `pubsub.googleapis.com:443` like every other
Google API call: allowed by default, or through `providers.gcp.apiCIDRs` when the egress is
narrowed (`private.googleapis.com` and `restricted.googleapis.com` serve Pub/Sub too).
