# subnet-operator

Discovers cloud networks and subnets from resource tags (AWS VPCs and subnets today) and keeps
a live inventory in the cluster: a `NetworkScope` selects a provider's accounts and regions, and
the operator mirrors what it finds as `Network` and `Subnet` objects
(`network.hypersurgery.dev/v1`), reports missing tags and CIDR overlaps, and exports
Prometheus metrics.

Upgrading: from 0.9, apply the new CRDs and move any `events.*`, `aws.*` or
`networkPolicy.egress.podIdentity` values under `providers.aws` first, which 1.0 requires
([0.9 to 1.0](https://github.com/aivandrago/subnet-operator/blob/main/docs/operations/upgrades.md#upgrading-from-09-to-10)).
Until 0.7 this chart was called `aws-subnet-operator` and the API was
`aws.hypersurgery/v1alpha1`; 0.8 migrated the old objects and 0.9 removed that group, so older
installs upgrade one release at a time through 0.8 and 0.9 (the
[upgrade guide](https://github.com/aivandrago/subnet-operator/blob/main/docs/operations/upgrades.md)
has every step).

By default the operator only calls `ec2:Describe*` and modifies nothing in your cloud. Creating
subnets (`SubnetClaim`) and tagging (`ResourceImport`, the auto-import policy) need
`writes.enabled=true` and a separate write role per account; nothing ever deletes a cloud
resource.

From 1.0 the API (`network.hypersurgery.dev/v1`) is stable, with a written
[compatibility promise](https://github.com/aivandrago/subnet-operator/blob/main/docs/api-compatibility.md).
It is tested against [Moto](https://github.com/getmoto/moto) in Kind, not yet against a real AWS
organization; reports from one are very welcome.

## Install

From the chart repository:

```sh
helm repo add hypersurgery https://charts.hypersurgery.dev
helm install subnet-operator hypersurgery/subnet-operator \
  -n subnet-operator-system --create-namespace \
  -f examples/values-minimal.yaml
```

Since v0.6.0 the chart is also published to ghcr.io as an OCI artifact, signed with cosign
keyless by the release workflow like the image. Helm 3.8 or newer installs it without a
repository:

```sh
helm install subnet-operator oci://ghcr.io/aivandrago/charts/subnet-operator \
  --version <version> \
  -n subnet-operator-system --create-namespace \
  -f examples/values-minimal.yaml
```

To check the chart before installing it:

```sh
cosign verify ghcr.io/aivandrago/charts/subnet-operator:<version> \
  --certificate-identity-regexp '^https://github.com/aivandrago/subnet-operator/\.github/workflows/release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The same `.tgz` is attached to each GitHub release.

The step-by-step guide is at https://hypersurgery.dev/docs/, and the panels can be previewed
with demo data at https://hypersurgery.dev/dashboard/. The same app can run in your cluster and
show your own inventory: see [The dashboard app](#the-dashboard-app).

CRDs are installed from `crds/` and, as Helm requires, are **not** removed on uninstall and
**not** upgraded by `helm upgrade`. Apply them yourself when upgrading across versions:

```sh
kubectl apply -f charts/subnet-operator/crds/
```

The CRDs serve `network.hypersurgery.dev/v1`, which they store, and `v1beta1`, the API of 0.8
and 0.9, deprecated; the two have the same fields ([API compatibility](https://github.com/aivandrago/subnet-operator/blob/main/docs/api-compatibility.md)).
After an upgrade from 0.9 the operator rewrites the stored objects at v1 itself
([upgrade guide](https://github.com/aivandrago/subnet-operator/blob/main/docs/operations/upgrades.md#upgrading-from-09-to-10)),
and it sets the CRDs' conversion ([below](#conversion-between-api-versions)): the files in `crds/`
cannot, since they cannot name the release's Service or its CA.

`crds/` holds `network.hypersurgery.dev` only. The `aws.hypersurgery` CRDs that 0.8 installed
are not removed by an upgrade; delete them once nothing is left to migrate, as the
[upgrade guide](https://github.com/aivandrago/subnet-operator/blob/main/docs/operations/upgrades.md#the-old-crds)
says.

## AWS access

The operator needs `ec2:DescribeVpcs`, `ec2:DescribeSubnets` and `ec2:DescribeRouteTables` in
its own account and `sts:AssumeRole` on the read-only roles of the others
(`deploy/iam/` in the repository has the policy and a StackSet template).
Attach the role with EKS Pod Identity (nothing to set here), or with IRSA through
`providers.aws.irsaRoleARN`, which becomes the service account's `eks.amazonaws.com/role-arn`
annotation.

## Values

| Key | Default | Description |
|---|---|---|
| `replicaCount` | `2` | Manager replicas: one leader, one standby. More than one only makes sense with `leaderElection.enabled`. |
| `image.repository` / `image.tag` | `ghcr.io/aivandrago/subnet-operator` / chart `appVersion` | Manager image. Public, multi-arch, signed with cosign. |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy. |
| `nameOverride` / `fullnameOverride` | `""` | Override the chart name / the release's full name used for resource names. |
| `imagePullSecrets` | `[]` | Not needed: the image is public. Set it only if you mirror the image somewhere that requires a login. |
| `serviceAccount.create` / `.name` / `.annotations` | `true` / `""` / `{}` | Service account. Extra annotations win over the ones `providers.*` set. Empty `name` uses the release's full name, which changed with the chart's name in 0.8: pin the old one when upgrading if a Pod Identity association or an IRSA trust policy names it. |
| `rbac.credentialSecretNamespaces` | `[]` | Extra namespaces where the operator may read credential Secrets. The release namespace, and the namespace of a `SheetExport` created by this chart, are covered already. |
| `discovery.concurrency` | `4` | Account/region pairs discovered in parallel, across all `NetworkScope`s. The defaults are sized for about 100 accounts at 4 regions; raise it in proportion for more (see the [capacity notes](../../docs/operations/limits.md#accounts-and-regions-per-instance)). |
| `providers.aws.enabled` | `true` | Run the AWS provider (`--providers=aws`). A `NetworkScope` of a provider that is not enabled is reported `ProviderNotEnabled` and not synced. Only AWS exists in this release. |
| `providers.aws.irsaRoleARN` | `""` | The operator's own role with IRSA, written as the `eks.amazonaws.com/role-arn` annotation. EKS Pod Identity needs nothing. |
| `providers.aws.events.queueUrl` | `""` | SQS queue fed by EventBridge with EC2 change events. Empty means the resync interval is the only trigger. |
| `providers.aws.events.debounce` | `""` (10s) | How long events are collected before the affected targets resync. |
| `providers.aws.region` | `""` | Region for the operator's own calls (STS, SQS). |
| `providers.aws.endpointURL` | `""` | Non-AWS endpoint, for testing against emulators. |
| `providers.aws.podIdentity.enabled` / `.cidr` / `.port` | `true` / `169.254.170.23/32` / `80` | With `networkPolicy.enabled`, egress to the EKS Pod Identity agent; IRSA does not need it. Its 0.8 name, `networkPolicy.egress.podIdentity`, deprecated in 0.9, is refused from 1.0. |
| `events.queueUrl` / `.debounce`, `aws.region` / `.endpointURL` | — | Removed in 1.0 (the 0.8 names of the `providers.aws` values above, deprecated in 0.9). The chart refuses to render while one is set, naming the replacement; so it does for `--events-queue-url` or `--events-debounce` in `extraArgs` and `EVENTS_QUEUE_URL` in `extraEnv`. See the [upgrade guide](https://github.com/aivandrago/subnet-operator/blob/main/docs/operations/upgrades.md#values-and-flags-removed-in-10). |
| `audit.sink` | `stdout` | Audit trail: one JSON line per decision on stdout, or `off`. See [docs/audit.md](../../docs/audit.md). |
| `writes.enabled` | `false` | Let `SubnetClaim`s in Create mode create subnets. Allocation works without it. |
| `leaderElection.enabled` | `true` | Leader election lease in the release namespace. |
| `leaderElection.leaseDuration` | `15s` | How long a standby waits before replacing a leader that crashed. A leader that is stopped hands the lease over at once, so this does not slow a drain or a rollout. |
| `leaderElection.renewDeadline` | `10s` | How long the leader retries a renewal before giving leadership up. Must be below `leaseDuration`. |
| `leaderElection.retryPeriod` | `2s` | How often the lease is acted on. `renewDeadline` must exceed 1.2 × this; the operator refuses to start otherwise. |
| `podDisruptionBudget.enabled` / `.minAvailable` / `.maxUnavailable` | `true` / `1` / `""` | Budget for node drains. Not created while `replicaCount` is 1, where it would block the drain. |
| `health.port` | `8081` | Port of the liveness and readiness probes. |
| `webhook.enabled` | `true` | Admission webhooks for `SubnetClaim`, `NetworkScope` and `ResourceImport` (at every served version), and the conversion webhook. |
| `webhook.port` / `.timeoutSeconds` | `9443` / `10` | Port the webhook server listens on; how long the API server waits for an admission review. |
| `webhook.service.annotations` | `{}` | Annotations on the webhook Service. |
| `webhook.conversion.enabled` | `true` | The operator points its CRDs' conversion between v1beta1 and v1 at its webhook, with the webhook certificate's CA; `false`, or `webhook.enabled=false`, leaves the conversion to the API server (strategy `None`). See [below](#conversion-between-api-versions). |
| `webhook.failurePolicy` | `Fail` | Validation while the operator is unreachable: `Fail` refuses the object, `Ignore` lets it through to the controller (and lets an object in without a verified `created-by` annotation). Defaulting is always `Ignore`. |
| `webhook.certificate.certManager` | `auto` | `auto` uses cert-manager when its API is present, otherwise the chart signs the certificate itself. `true`/`false` decide it outright. `true` is the recommended setting for production and GitOps, see [below](#admission-webhooks). |
| `webhook.certificate.issuerRef` | `{}` | An existing cert-manager issuer instead of the self-signed one the chart creates. |
| `webhook.certificate.duration` / `.renewBefore` | `8760h` / `720h` | cert-manager certificate lifetime. |
| `metrics.secure` / `.port` | `true` / `8443` | HTTPS with authn/authz, or plain HTTP when false. |
| `metrics.service.annotations` | `{}` | Annotations on the metrics Service. |
| `metrics.serviceMonitor.enabled` | `false` | ServiceMonitor for the Prometheus operator. |
| `metrics.serviceMonitor.labels` / `.interval` / `.scrapeTimeout` | `{}` / `60s` / `30s` | Labels your Prometheus selects on, and the scrape timing. |
| `metrics.serviceMonitor.insecureSkipVerify` | `true` | Skip verifying the metrics endpoint's certificate, which is self-signed by default. |
| `prometheusRule.enabled` | `false` | Alerts: operator down, subnet nearly full or full, target down, target throttled, CIDR overlap, stale inventory, unmanaged resources, resources the auto-import policy tagged, claims and imports stuck unfulfilled. Each has a [runbook entry](https://github.com/aivandrago/subnet-operator/blob/main/docs/operations/runbook.md). |
| `prometheusRule.labels` | `{}` | Labels your Prometheus selects rules on. |
| `prometheusRule.thresholds.subnetUsedRatio` | `0.85` | When a subnet counts as nearly full. |
| `prometheusRule.thresholds.staleSyncSeconds` | `3600` | When the inventory counts as stale. |
| `prometheusRule.thresholds.notReadyFor` | `30m` | How long a `SubnetClaim` or `ResourceImport` may stay unfulfilled before it alerts. |
| `prometheusRule.thresholds.throttledFor` | `30m` | How long an account/region may stay throttled by its cloud, and backed off, before `SubnetInventoryTargetThrottled` fires. |
| `prometheusRule.operatorDown.job` | `""` | Scrape job the operator's metrics arrive under. Empty uses the chart's `ServiceMonitor` job; without either, `SubnetOperatorDown` is not rendered. |
| `prometheusRule.operatorDown.for` | `10m` | How long no replica may be up before `SubnetOperatorDown` fires. |
| `grafanaDashboard.enabled` | `false` | ConfigMap with the dashboard, for the Grafana sidecar. |
| `grafanaDashboard.labels` / `.annotations` | `grafana_dashboard: "1"` / `{}` | Labels the Grafana sidecar selects on; annotations on the ConfigMap. |
| `dashboard.enabled` | `false` | The dashboard app, reading the cluster as each viewer. See [The dashboard app](#the-dashboard-app). |
| `dashboard.replicaCount` / `.port` / `.healthPort` | `1` / `8080` / `8081` | Replicas, the app port and the kubelet probe port. |
| `dashboard.service.type` / `.port` | `ClusterIP` / `80` | The dashboard's Service. |
| `dashboard.service.annotations` | `{}` | Annotations on the dashboard's Service. |
| `dashboard.ingress.enabled` / `.className` / `.annotations` / `.hosts` / `.tls` | off | An Ingress for it. Put your SSO in front through the annotations. |
| `dashboard.tls.secretName` | `""` | A `kubernetes.io/tls` Secret; the pod then serves HTTPS, so tokens are encrypted up to it. |
| `dashboard.maxBodyBytes` | `65536` | Largest request body passed to the API server. |
| `dashboard.networkPolicy.from` | `[]` | With `networkPolicy.enabled`, who may reach the app port. Empty means any source. |
| `dashboard.resources` | 200m / 64Mi limits, 10m / 32Mi requests | Container resources. |
| `dashboard.podSecurityContext`, `dashboard.securityContext` | non-root, read-only rootfs, no capabilities | Pod and container security. |
| `dashboard.podAnnotations` / `.podLabels` / `.nodeSelector` / `.tolerations` / `.affinity` | empty | Pod metadata and scheduling of the dashboard. |
| `networkPolicy.enabled` | `false` | Restrict the operator to the API server, the AWS endpoints, DNS, the metrics scrape, the kubelet probes and, with the webhooks on, admission review calls. |
| `networkPolicy.metricsFrom` | `[]` | Sources allowed to scrape metrics. Empty means any source. |
| `networkPolicy.egress.cidrs` / `.ports` | `0.0.0.0/0` / `443, 6443` | Where the API server and the AWS endpoints are reached. |
| `networkPolicy.egress.dns.enabled` / `.namespace` / `.ports` | `true` / `kube-system` / `[53]` | Egress to DNS in the namespace CoreDNS runs in. |
| `networkPolicy.extraIngress` / `.extraEgress` | `[]` | Rules appended verbatim to the operator's NetworkPolicy. |
| `networkScope.create` | `false` | Also create a `NetworkScope` with the release (provider AWS). `networkScope.accounts[]` take their roles in an `aws` member. `networkScope.networkSelector.matchTags` selects the VPCs. The 0.7 forms, `roleARN`, `externalID` and `writeRoleARN` next to the `id` and `vpcTagSelector`, were removed in 0.9 and fail the render. |
| `networkScope.namespaceSelector` | `null` | Namespaces whose `SubnetClaim`s and `ResourceImport`s may use that scope. `null` leaves the field unset, which allows **no** namespace; `{}` allows every namespace (what 0.7 did with it unset) and is warned about; see [Permissions](#permissions). |
| `networkScope.name` / `.accounts` / `.regions` / `.networkSelector.matchTags` / `.requiredSubnetTags` / `.resyncInterval` | `organization` / `[]` / `[]` / `hs/managed: "true"` / `[hs/owner, hs/env, hs/tier]` / `10m` | The fields of that `NetworkScope`, as in its spec. |
| `sheetExport.create` | `false` | Also create a `SheetExport` (Google Sheet mirror). |
| `sheetExport.name` / `.spreadsheetID` / `.sheetName` / `.extraTagColumns` / `.refreshInterval` | `organization` / `""` / `Subnets` / `[]` / `5m` | The fields of that `SheetExport`. |
| `sheetExport.credentialsSecret.name` / `.namespace` / `.key` | `google-sheets` / release namespace / `credentials.json` | The Secret with the Google service account key. |
| `resources` | 500m / 512Mi limits, 50m / 128Mi requests | Container resources. |
| `podAnnotations` / `podLabels` | `{}` | Extra pod metadata. |
| `podSecurityContext`, `securityContext` | non-root, read-only rootfs | Pod and container security. |
| `extraArgs`, `extraEnv` | `[]` | Extra manager flags and environment variables. |
| `nodeSelector`, `tolerations`, `affinity`, `priorityClassName` | empty | Scheduling. |
| `topologySpreadConstraints` | host and zone, `ScheduleAnyway` | Keeps leader and standby apart. Entries without a `labelSelector` get the release's selector labels. |
| `terminationGracePeriodSeconds` | `45` | Longer than the manager's 30s graceful shutdown, so an in-flight sync is not killed mid-write. |

See `examples/values-minimal.yaml` and `examples/values-organization.yaml` in the repository.

## Permissions

### Who should be able to do what

The operator acts with its own AWS roles on behalf of whoever creates its objects, so the
Kubernetes RBAC on those objects is part of the AWS permission model:

| Resource | Scope | Who should hold `create`/`update` |
|---|---|---|
| `networkscopes`, `sheetexports` | cluster | Cluster administrators only. A scope names the read and write roles of AWS accounts and decides which namespaces may use them; a `SheetExport` names a credential Secret. Whoever writes them effectively administers those roles and credentials. |
| `subnetclaims`, `resourceimports` | namespace | The team that owns the namespace, in the namespaces a scope's `namespaceSelector` allows. Grant it with a namespaced `Role`, not by adding the resources to a wildcard or aggregated role. |
| `networks`, `subnets` | cluster | Nobody: the operator writes them. Give people read access only (`examples/04-read-only-access.yaml`). |
| `namespaces` (`update`, `patch`) | cluster | Nobody who should not choose which scopes a namespace may use, if a `namespaceSelector` matches labels other than `kubernetes.io/metadata.name`. |

The chart ships no aggregated roles, so nobody gets these resources through the built-in
`edit` or `admin` roles by accident.

A scope's `spec.namespaceSelector` is the second half. A `SubnetClaim` or `ResourceImport` in a
namespace the selector does not select is refused by the webhook, and — if it got in while the
webhooks were off — refused by the controller with the reason `NamespaceNotAllowed`, before it
reserves a CIDR, creates a subnet or applies a tag. The auto-import policy's `namespace` must be
one the selector allows too.

```yaml
spec:
  namespaceSelector:
    matchExpressions:
      - key: kubernetes.io/metadata.name   # set by the API server on every namespace
        operator: In
        values: [team-payments, team-data]
```

Selecting by name is the safe default, because nobody can change that label. Selecting by
another label (`team: payments`) is only as strong as the RBAC on updating namespaces.
Without a selector no namespace may use the scope; it only takes inventory, and the webhook
says so. An empty selector (`{}`) allows every namespace on purpose, and the webhook and a
`NamespacesUnrestricted` Event on the scope say so. (In `aws.hypersurgery/v1alpha1` an unset
selector allowed every namespace; the migration in 0.8 and `manager migrate-manifests` write
`{}` into such scopes so nobody is locked out, and leave it to you to narrow it.) `networkScope.namespaceSelector` in the values
follows the same rule: `null`, the default, leaves it unset.

Narrowing a selector later never deletes anything and never undoes what AWS already has. A
claim in a namespace that is no longer allowed keeps its reservations and subnets, reports
`NamespaceNotAllowed` the next time it is reconciled and creates nothing more; an import that
has already applied its tags stays `Applied`, and one that has not is refused. The webhook
refuses updates to such objects; deleting them is always allowed.

### What the operator itself holds

The ClusterRole holds only what is cluster-scoped: the operator's own objects, which are read
and written across namespaces, read access to namespaces, for their labels, and, by name, its
six CustomResourceDefinitions: `get` and `patch` to set their conversion, `get` and `update` on
their status to trim `storedVersions` after an upgrade. Secrets are not in it. The Google service account key for
`SheetExport` is read through a Role bound in the release namespace, so a compromised
operator cannot read Secrets anywhere else. Point a `SheetExport` at a Secret in another
namespace and you have to add that namespace to `rbac.credentialSecretNamespaces`, which
creates the same Role and binding there.

## Availability

The default is two replicas with leader election: one holds the lease and reconciles, the
other waits. A `PodDisruptionBudget` keeps one of them alive through a node drain, and the
default `topologySpreadConstraints` spread them over nodes and zones where the cluster has
more than one of each (`ScheduleAnyway`, so small clusters still schedule both pods).

Losing the lease mid-sync is safe: the operator writes each discovered network and subnet
idempotently, and a scope's `status.lastSyncTime` and `observedGeneration` only advance
after the sync has finished, so the next leader starts a full sync instead of trusting a
partial one. Set `replicaCount: 1` and `leaderElection.enabled: false` together if you would
rather run a single manager.

## Admission webhooks

An invalid object is refused by `kubectl apply` instead of being accepted and explained in a
status a reconcile later: a prefix length that does not fit the network, a claim for an account no
`NetworkScope` discovers, two scopes over the same account and region, an import whose tags AWS
would reject. Anything that depends on discovery having run — a network that is not in the
inventory yet — is a warning, not a refusal, so the apply order of a GitOps repository does not
matter. The controllers check the same rules regardless, so `webhook.enabled=false` costs
feedback, not correctness.

The webhooks also record who created each `SubnetClaim` and `ResourceImport`: the mutating
webhook writes the user the API server authenticated into the `network.hypersurgery.dev/created-by`
annotation, whatever the object carried, and the validating webhook refuses a create where it
does not match and any update that changes it, with no exception. (0.8 made one for the copies
its migration from `aws.hypersurgery` created; 0.9 migrates nothing.) The audit trail's
`created_by` field comes from
that annotation, and says `unknown` with `webhook.enabled=false` ([docs/audit.md](https://github.com/aivandrago/subnet-operator/blob/main/docs/audit.md)).

**Use cert-manager in production and with GitOps** (`webhook.certificate.certManager=true`). The
serving certificate comes from cert-manager when the cluster has it
(`webhook.certificate.certManager=auto` looks for the `cert-manager.io/v1` API), and from the
chart itself otherwise. The self-signed path needs nothing in the cluster and no extra RBAC,
which makes it right for trying the operator out, and wrong for keeping it:

- the chart-signed CA and certificate are valid for **10 years**;
- they are **never rotated**: on an upgrade the chart reads the existing secret back rather
  than rotating the CA out from under the API server, so a key that leaked once stays good.
  Renewing is manual: delete the secret and upgrade again;
- the private key is rendered into the manifest, so it is also stored in every Helm release
  Secret (`sh.helm.release.v1.*`) of the namespace, readable by anyone who can read Secrets
  there, and in whatever your GitOps tool keeps of the rendered output;
- `helm template` has no cluster to read the secret from, so Argo CD, Flux with
  post-rendering or a `helm template | kubectl apply` pipeline sign a **new** CA and key on
  every render, and the webhook configuration and the secret can drift apart until the next
  sync.

cert-manager issues a short-lived certificate (`duration`, `renewBefore`), renews it on its own,
keeps the key out of the Helm release and renders the same manifest every time. Automatic
rotation of the self-signed certificate is not implemented yet (issue #71).

### Conversion between API versions

The API server converts objects between v1beta1 and v1 whenever a client asks for the version
an object is not stored at. With `webhook.conversion.enabled=true` (the default) the operator
points each CRD's conversion at its webhook (strategy `Webhook`, the `<release>-webhook` Service,
path `/convert`) with the CA from `ca.crt` of the webhook certificate's Secret, which both the
chart's own certificate and cert-manager's carry, and follows a renewed CA within a minute. It
does so once nothing is stored at v1beta1 any more, and until then leaves the `None` strategy
the CRDs are installed with. Without a CA in the Secret (an issuer that does not fill `ca.crt`)
it keeps `None` and logs why.

Why the operator and not the chart: Helm installs `crds/` once, verbatim, and never upgrades
or deletes them, which is what keeps `helm uninstall` from deleting every object. CRDs rendered as chart templates could name the Service and carry the CA, but Helm
would then own them: `helm uninstall` would delete them and every object in them, and each
upgrade would rewrite them. So the operator sets the conversion itself, with RBAC that names
its six CRDs and allows nothing else on CustomResourceDefinitions
([What the operator itself holds](#what-the-operator-itself-holds)). The cost is recorded in
the [threat model](https://github.com/aivandrago/subnet-operator/blob/main/docs/security/threat-model.md#residual-risks-accepted-for-10):
whoever holds the operator's token can change those six CRDs.

With cert-manager (`webhook.certificate.certManager`) the same `ca.crt` mechanism applies; it
is covered by render tests and envtest, and an end-to-end run with cert-manager is tracked in
issue #86.

`None` is also what the CRDs get with `webhook.enabled=false` or
`webhook.conversion.enabled=false`. The API server then converts on its own, which is exact
because the two versions have the same fields. With the webhook, a request at v1beta1 needs a
ready operator pod; a request at v1 does not.

With `networkPolicy.enabled=true` the webhook port is open to every source, because admission
review calls come from the API server, which has no pod or namespace to select on and no
address the chart can know in advance. Narrow it through `networkPolicy.extraIngress` if your
API server's addresses are fixed.

Identifying fields are immutable after creation — a claim's scope, account, region and network, an
import's resource — because the operator never deletes what it created in AWS, and letting them
change would leave those resources behind with nothing pointing at them.

## The dashboard app

`dashboard.enabled=true` runs the app from https://hypersurgery.dev/dashboard/ in the cluster,
showing the cluster's own `NetworkScope`, `Network`, `Subnet`, `SubnetClaim` and `ResourceImport`
objects instead of demo data. It is a Deployment and Service of its own, from the operator's
image (the `/dashboard` entrypoint), so the image signature and SBOM cover it.

It reads **as the person using it**, never as itself. Its service account has no RBAC and no
token (`automountServiceAccountToken: false`); it checks the API server against the cluster CA
from the `kube-root-ca.crt` ConfigMap and authenticates only with the `Authorization: Bearer`
header of the incoming request, passed on untouched. No token means 401, not a fallback
identity. Viewers need RBAC of their own — `get`, `list`, `watch` on the `network.hypersurgery.dev`
resources, and `create` on `resourceimports` in a namespace to import.

What it forwards is a short list: reads and watches of any resource in the `network.hypersurgery.dev`
group, creating a `ResourceImport` in a namespace, and creating a `SelfSubjectReview` — an empty
one, with no query parameters — so the app can show who the viewer is signed in as. Everything
else — Secrets, other groups, the rest of `authentication.k8s.io`, updates, deletes, discovery —
is refused with 403 before the API server is asked. Every authenticated user may create a
`SelfSubjectReview` (Kubernetes 1.28 and later); on an older API server the name is not shown. Only `Authorization`,
`Accept` and `Content-Type` are passed on, so `Impersonate-*`, cookies and front-proxy headers
never reach the API server; request bodies are capped at `dashboard.maxBodyBytes`. A write must
carry a JSON body and the `X-Subnet-Dashboard` header, which a page on another origin cannot
make a browser send, so an SSO session cookie alone never creates an import; the
`SelfSubjectReview` is held to the same rule, as every POST is. The app is served with a strict
Content-Security-Policy and loads nothing from elsewhere.

The app watches `Subnet`s and `ResourceImport`s and lists the other kinds every 15 seconds. A
watch is a response that stays open: the dashboard passes it on as it streams and marks it
`X-Accel-Buffering: no`, which nginx-based ingress controllers honour. A proxy in front with a
read timeout shorter than the watch (ingress-nginx defaults to 60 s) only makes the app
reconnect more often, from where it left off; raise
`nginx.ingress.kubernetes.io/proxy-read-timeout` to spare the reconnects. Only two kinds are
watched because over HTTP/1.1 a browser opens at most six connections to a server, shared by
all its tabs, and a tab in the background closes its watches. Served over TLS
(`dashboard.tls.secretName`, or an ingress that speaks HTTP/2 to the browser), requests share
one connection and the limit does not apply.

The token reaches it from an SSO proxy in front — oauth2-proxy with
`--pass-authorization-header`, or `--set-authorization-header` behind ingress-nginx's
`auth-url` and `auth-response-headers: Authorization`, against an API server that trusts the
same OIDC issuer — or the viewer pastes one into the app, which keeps it in `sessionStorage`
for that tab only. For a quick look:

```sh
kubectl -n subnet-operator-system port-forward svc/subnet-operator-dashboard 8080:80
kubectl -n subnet-operator-system create token <a-service-account-with-read-access>
# open http://127.0.0.1:8080/ and paste the token
```

Without installing anything, the same app reads a cluster through `kubectl proxy` from a
checkout of this repository: `kubectl proxy --www=site --www-prefix=/ui/`, then
`http://127.0.0.1:8001/ui/dashboard/?source=kubectl`.

## Dashboard and alerts

`grafanaDashboard.enabled=true` ships `dashboards/subnet-inventory.json` as a ConfigMap labelled
`grafana_dashboard: "1"`, which the kube-prometheus-stack Grafana sidecar imports on its own.
`prometheusRule.enabled=true` adds the alerts; set the labels your Prometheus selects on
(usually `release: kube-prometheus-stack`).

Both read the `hs_*` metrics, which carry a `provider` label. 0.9 no longer exports the
`hs_aws_*` names of 0.8 and renamed the alert `VPCCIDROverlap` to `NetworkCIDROverlap`; see the
[upgrade notes](../../docs/operations/upgrades.md#upgrading-from-08-to-09) for moving rules,
dashboards, routes and silences of your own.
