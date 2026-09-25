# The audit trail

The operator changes tags on real infrastructure. A Prometheus counter says how often that
happened, and the object's `status` says what the last attempt did, but neither answers *who
took this subnet under management, when, and on whose behalf* once a resync has overwritten
the status. Two things do.

## 1. Kubernetes Events

Events are on the object people already run `kubectl describe` against, they cost nothing to
turn on, and they are kept for as long as the cluster keeps Events (an hour by default).

| Object | Type | Reason | When |
|---|---|---|---|
| `ResourceImport` | Normal | `Imported` | tags reached AWS |
| `ResourceImport` | Normal | `DryRun` | the tags were computed, not applied |
| `ResourceImport` | Warning | the condition's reason (`WritesDisabled`, `NoWriteRole`, `TagsNotApplied`, `ScopeNotFound`, `NamespaceNotAllowed`, `AccountNotInScope`, `ProviderNotEnabled`, `RegionRequired`, `InvalidResourceID`) | the import did not happen, with the reason |
| `SubnetClaim` | Normal | `Allocated` | a CIDR was reserved for an availability zone |
| `SubnetClaim` | Normal | `SubnetCreated` | the subnet exists in AWS |
| `SubnetClaim` | Warning | the condition's reason (`NoSpace`, `WritesDisabled`, `NoWriteRole`, `CreateFailed`, `CreateNotSupported`, `NetworkNotFound`, `ZonesRequired`, `InvalidPrefixLength`, `ScopeNotFound`, `NamespaceNotAllowed`, `AccountNotInScope`, `ProviderNotEnabled`) | the claim did not get what it asked for |
| `NetworkScope` | Normal | `AutoImportRequested` | the auto-import policy wrote a `ResourceImport` |
| `NetworkScope` | Warning | `NoOwner` | the policy found a resource no rule could attribute |
| `NetworkScope` | Warning | `TargetUnreachable` | an account/region could not be read |
| `NetworkScope` | Warning | `TargetThrottled` | an account/region was rate-limited by its cloud and is backed off; it stays in the inventory with its last known state |
| `NetworkScope` | Warning | `NamespaceNotAllowed` | the auto-import policy was not run, because the scope does not allow the namespace it writes its imports to |
| `NetworkScope` | Warning | `NamespacesUnrestricted` | the scope has no `namespaceSelector`, so every namespace may use it; once per change of the spec |

The operator also records Events on its own CustomResourceDefinitions (`kubectl describe crd
subnets.network.hypersurgery.dev`): `StorageVersionMigrated` (Normal) when it has rewritten what
an earlier release stored and trimmed `status.storedVersions`, and `ConversionConfigured`
(Normal) when it changes a CRD's conversion: to its webhook (`--crd-conversion=webhook`, the
chart's default) or to the API server's own (`--crd-conversion=none`). These are about the upgrade, not about a decision on a cloud resource, and have no
audit line ([operations/upgrades.md](operations/upgrades.md#upgrading-from-09-to-10)). The same
goes for `MigrationPending` (Warning), on an `aws.hypersurgery/v1alpha1` object 0.8 never
migrated, while it blocks the operator from starting its controllers.

A Warning Event reuses the reason of the status condition it accompanies, so the conditions
and the Events answer a question with the same word.

Two decisions deliberately produce no Event. A policy `skip` repeats on every sync and says
that nothing changed — the counter and the audit line are its record. And each failed
availability zone of a claim does not get its own Event: the claim gets one `CreateFailed`,
with the per-zone errors in `status.allocations`.

Events need `create` and `patch` on `events` in the namespaces the objects live in. Without
that permission the operator still works and still writes the audit log; the Events are
dropped with a message in the operational log.

## 2. The audit log

One JSON object per line, on **stdout**, while the operational log goes to stderr. That is the
whole separation mechanism: point the two streams at different places, or filter on
`.action`, and the SIEM gets decisions without the operational noise.

```
--audit-sink=stdout   # default: one JSON line per decision on stdout
--audit-sink=off      # no audit log at all
```

In the Helm chart the same switch is `audit.sink`. An unknown value stops the operator at
startup rather than letting it run while believing it keeps a record.

### The fields

Fields whose value is not known are left out of the line rather than written empty. The one
exception is `created_by`, which says `unknown` instead: a missing identity should read as a
gap in the record, not as a field this version of the operator does not write.

| Field | Meaning |
|---|---|
| `time` | when the decision was taken, RFC 3339 with nanoseconds, UTC |
| `action` | `import`, `allocate` or `policy_decision` |
| `result` | `applied`, `dryrun`, `reserved`, `skipped`, `no_owner` or `failed` |
| `scope` | the `NetworkScope` the resource belongs to |
| `account` | the AWS account id |
| `region` | the AWS region |
| `resource_id` | the `vpc-…` or `subnet-…` the decision was about |
| `object` | the Kubernetes object that carries the decision, as `Kind/namespace/name` |
| `principal` | who the change is attributed to (see below) |
| `created_by` | the Kubernetes identity the change was done for, as the API server authenticated it, or `unknown` (see below) |
| `reason` | the policy's own sentence, or why the attempt ended the way it did |
| `tags_before` | the tags the operator last saw on the resource |
| `tags_after` | the tags it carries, or would carry, after the change |
| `cidr` | the block a claim reserved or created |
| `error` | the cloud's refusal, for `result: failed` |

### What `principal` means

There is one source for it, the same one the object shows:

- **`ResourceImport`** — `spec.requestedBy`. An import applied by hand carries whoever wrote
  it; one written by the auto-import policy carries `auto-import policy (created by <the
  CloudTrail principal that created the resource>)`, from the creator cache the EC2 event
  poller fills. When no creator is known it is just `auto-import policy`.
- **`SubnetClaim`** — `spec.owner`, which is also the `hs/owner` tag the subnet itself gets in
  AWS.

`principal` is the operator's best attribution, not an authenticated identity. `requestedBy`
is free text that whoever applies the import may fill with anything — a ticket, a team, or
somebody else's name — and the creator comes from CloudTrail events seen in the last 24 hours,
so a resource older than that, or created while the poller was down, has none.

### What `created_by` means

`created_by` is the answer to *which Kubernetes identity made the operator do this*. Nobody
who applies an object can choose it:

- **`ResourceImport` and `SubnetClaim`** — the `network.hypersurgery.dev/created-by` annotation. The
  mutating admission webhook writes the user the API server authenticated into it when the
  object is created, replacing whatever the object carried, and the validating webhook refuses
  any update that changes, adds or removes it. The mutating webhook is `failurePolicy: Ignore`,
  so the validating one (`Fail`, served by the same process) also refuses a create whose
  annotation is not the requesting user: while the webhooks are installed, an object cannot be
  created without the right value.
- **Imports the auto-import policy writes** — the operator's own identity
  (`system:serviceaccount:<namespace>:<service account>`), which the operator looks up with a
  `SelfSubjectReview` at startup and sets on the import; the webhook writes the same value.
- **`policy_decision`** — the operator's own identity too: the policy runs as the operator.
- **Objects 0.8 migrated from `aws.hypersurgery/v1alpha1`** — the creator the old object
  recorded in `aws.hypersurgery/created-by`, which the old group's webhooks wrote and guarded
  the same way, and which 0.8's migration copied. An old object that had no creator (from
  before 0.7) got none, and its lines say `unknown`. 0.9 migrates nothing: an object created now
  gets its creator's name, the `network.hypersurgery.dev/migrated-from` marker
  notwithstanding.

The operator only repeats the annotation while it serves the admission webhooks itself. With
`webhook.enabled=false` the annotation is whatever the object's author wrote, so every line
says `unknown`, as does a line about an object created before the webhooks recorded a creator.
The Kubernetes audit log remains the authority on who created an object; `created_by` saves
the SIEM a join, and is only as good as the webhooks.

When `principal` and `created_by` disagree — `principal: "network team (NET-42)"`,
`created_by: "jane@example.com"` — both are true: Jane applied an import on behalf of the
network team. What a reviewer should not do is read `principal` as the person who authenticated.

### What `tags_before` does not mean

`tags_before` is what the operator last saw. A resource nobody has tagged is not mirrored as a
`VPC` or `Subnet` object, so an import of one may have no `tags_before` at all. Absent means
*not known to us*, not *the resource had no tags*. The policy's own lines are the exception:
there the resource's current tags come straight from discovery.

### Repetition

The auto-import policy runs on every sync, so a resource that stays in `skipped` or `no_owner`
produces one line per sync — exactly like the `hs_auto_imports_total` counter beside it.
Imports, allocations and created subnets are written once, when they happen. Deduplicate
downstream on `resource_id` + `result` if the volume matters.

### Example

```json
{"time":"2026-09-23T10:31:02.114887Z","action":"policy_decision","result":"applied","scope":"org","account":"111111111111","region":"eu-central-1","resource_id":"vpc-0fee1dead","object":"NetworkScope/org","principal":"auto-import policy (created by arn:aws:sts::111111111111:assumed-role/payments-deploy/maria.k)","created_by":"system:serviceaccount:subnet-operator-system:subnet-operator","reason":"tags from creator rule \"arn:aws:sts::111111111111:assumed-role/payments-\"","tags_before":{"Name":"legacy"},"tags_after":{"Name":"legacy","hs/managed":"true","hs/owner":"team-payments"}}
{"time":"2026-09-23T10:31:04.982401Z","action":"import","result":"applied","scope":"org","account":"111111111111","region":"eu-central-1","resource_id":"vpc-0fee1dead","object":"ResourceImport/default/vpc-0fee1dead-import","principal":"auto-import policy (created by arn:aws:sts::111111111111:assumed-role/payments-deploy/maria.k)","created_by":"system:serviceaccount:subnet-operator-system:subnet-operator","reason":"tags from creator rule \"arn:aws:sts::111111111111:assumed-role/payments-\"","tags_before":{"Name":"legacy"},"tags_after":{"Name":"legacy","hs/managed":"true","hs/owner":"team-payments"}}
```

Two lines for one resource is intentional: the policy decided, and the import applied. They
are separate events, minutes apart, and either can happen without the other — a dry run
decides and never applies, and a hand-written import applies with no policy decision at all.

## What is not in either

- **Reads.** Discovery is not audited; it changes nothing. That an account could not be read
  is an Event and `hs_target_up`, not an audit line.
- **Deletions.** The operator never deletes a cloud resource, so there is nothing to record.
- **Delivery guarantees.** Both outputs are best-effort. A line that cannot be written is
  dropped rather than failing a reconcile that has already changed AWS, and Events are subject
  to the API server's own rate limiting. If the trail has to survive the loss of a pod, ship
  stdout off the node.
