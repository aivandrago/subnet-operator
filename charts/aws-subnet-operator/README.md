# aws-subnet-operator

Discovers AWS VPCs and subnets from resource tags and keeps a live inventory in the cluster:
`NetworkScope` selects accounts and regions, and the operator mirrors what it finds as `VPC`
and `Subnet` objects, reports missing tags and CIDR overlaps, and exports Prometheus metrics.

The core only calls `ec2:Describe*`. Nothing in your cloud is modified.

## Install

```sh
helm repo add hypersurgery https://charts.hypersurgery.dev
helm install subnet-operator hypersurgery/aws-subnet-operator \
  -n aws-subnet-operator-system --create-namespace \
  -f examples/values-minimal.yaml
```

The step-by-step guide is at https://hypersurgery.dev/docs/, and the panels can be previewed
with demo data at https://hypersurgery.dev/dashboard/.

CRDs are installed from `crds/` and, as Helm requires, are **not** removed on uninstall and
**not** upgraded by `helm upgrade`. Apply them yourself when upgrading across versions:

```sh
kubectl apply -f charts/aws-subnet-operator/crds/
```

## AWS access

The operator needs `ec2:DescribeVpcs`, `ec2:DescribeSubnets` and `ec2:DescribeRouteTables` in
its own account and `sts:AssumeRole` on the read-only roles of the others
(`deploy/iam/` in the repository has the policy and a StackSet template).
Attach the role with EKS Pod Identity, or with IRSA through
`serviceAccount.annotations."eks.amazonaws.com/role-arn"`.

## Values

| Key | Default | Description |
|---|---|---|
| `replicaCount` | `2` | Manager replicas: one leader, one standby. More than one only makes sense with `leaderElection.enabled`. |
| `image.repository` / `image.tag` | `registry.hypersurgery.dev/gitadmin/aws-subnet-operator` / chart `appVersion` | Manager image. |
| `imagePullSecrets` | `[]` | The registry currently requires a login, so a pull secret is needed. |
| `serviceAccount.create` / `.name` / `.annotations` | `true` / `""` / `{}` | Service account; annotate for IRSA. |
| `rbac.credentialSecretNamespaces` | `[]` | Extra namespaces where the operator may read credential Secrets. The release namespace, and the namespace of a `SheetExport` created by this chart, are covered already. |
| `discovery.concurrency` | `4` | Account/region pairs discovered in parallel. |
| `events.queueUrl` | `""` | SQS queue fed by EventBridge. Empty means the resync interval is the only trigger. |
| `events.debounce` | `10s` | How long events are collected before the affected targets resync. |
| `audit.sink` | `stdout` | Audit trail: one JSON line per decision on stdout, or `off`. See [docs/audit.md](../../docs/audit.md). |
| `aws.region` | `""` | Region for the operator's own calls. |
| `aws.endpointURL` | `""` | Non-AWS endpoint, for testing against emulators. |
| `writes.enabled` | `false` | Let `SubnetClaim`s in Create mode create subnets. Allocation works without it. |
| `leaderElection.enabled` | `true` | Leader election lease in the release namespace. |
| `podDisruptionBudget.enabled` / `.minAvailable` / `.maxUnavailable` | `true` / `1` / `""` | Budget for node drains. Not created while `replicaCount` is 1, where it would block the drain. |
| `health.port` | `8081` | Port of the liveness and readiness probes. |
| `webhook.enabled` | `true` | Admission webhooks for `SubnetClaim`, `NetworkScope` and `ResourceImport`. |
| `webhook.failurePolicy` | `Fail` | Validation while the operator is unreachable: `Fail` refuses the object, `Ignore` lets it through to the controller. Defaulting is always `Ignore`. |
| `webhook.certificate.certManager` | `auto` | `auto` uses cert-manager when its API is present, otherwise the chart signs the certificate itself. `true`/`false` decide it outright. |
| `webhook.certificate.issuerRef` | `{}` | An existing cert-manager issuer instead of the self-signed one the chart creates. |
| `webhook.certificate.duration` / `.renewBefore` | `8760h` / `720h` | cert-manager certificate lifetime. |
| `metrics.secure` / `.port` | `true` / `8443` | HTTPS with authn/authz, or plain HTTP when false. |
| `metrics.serviceMonitor.enabled` | `false` | ServiceMonitor for the Prometheus operator. |
| `prometheusRule.enabled` | `false` | Alerts: subnet nearly full or full, target down, CIDR overlap, stale inventory. |
| `prometheusRule.thresholds.subnetUsedRatio` | `0.85` | When a subnet counts as nearly full. |
| `prometheusRule.thresholds.staleSyncSeconds` | `3600` | When the inventory counts as stale. |
| `grafanaDashboard.enabled` | `false` | ConfigMap with the dashboard, for the Grafana sidecar. |
| `networkPolicy.enabled` | `false` | Restrict the operator to the API server, the AWS endpoints, DNS, the metrics scrape, the kubelet probes and, with the webhooks on, admission review calls. |
| `networkPolicy.metricsFrom` | `[]` | Sources allowed to scrape metrics. Empty means any source. |
| `networkPolicy.egress.cidrs` / `.ports` | `0.0.0.0/0` / `443, 6443` | Where the API server and the AWS endpoints are reached. |
| `networkPolicy.egress.dns` / `.podIdentity` | enabled | DNS in `kube-system`, and the EKS Pod Identity link-local address. |
| `networkScope.create` | `false` | Also create a `NetworkScope` with the release. |
| `sheetExport.create` | `false` | Also create a `SheetExport` (Google Sheet mirror). |
| `resources` | 500m / 256Mi limits | Container resources. |
| `podSecurityContext`, `securityContext` | non-root, read-only rootfs | Pod and container security. |
| `extraArgs`, `extraEnv` | `[]` | Extra manager flags and environment variables. |
| `nodeSelector`, `tolerations`, `affinity`, `priorityClassName` | empty | Scheduling. |
| `topologySpreadConstraints` | host and zone, `ScheduleAnyway` | Keeps leader and standby apart. Entries without a `labelSelector` get the release's selector labels. |
| `terminationGracePeriodSeconds` | `45` | Longer than the manager's 30s graceful shutdown, so an in-flight sync is not killed mid-write. |

See `examples/values-minimal.yaml` and `examples/values-organization.yaml` in the repository.

## Permissions

The ClusterRole holds only what is cluster-scoped: the operator's own CRDs, which are read
and written across namespaces. Secrets are not in it. The Google service account key for
`SheetExport` is read through a Role bound in the release namespace, so a compromised
operator cannot read Secrets anywhere else. Point a `SheetExport` at a Secret in another
namespace and you have to add that namespace to `rbac.credentialSecretNamespaces`, which
creates the same Role and binding there.

## Availability

The default is two replicas with leader election: one holds the lease and reconciles, the
other waits. A `PodDisruptionBudget` keeps one of them alive through a node drain, and the
default `topologySpreadConstraints` spread them over nodes and zones where the cluster has
more than one of each (`ScheduleAnyway`, so small clusters still schedule both pods).

Losing the lease mid-sync is safe: the operator writes each discovered VPC and subnet
idempotently, and a scope's `status.lastSyncTime` and `observedGeneration` only advance
after the sync has finished, so the next leader starts a full sync instead of trusting a
partial one. Set `replicaCount: 1` and `leaderElection.enabled: false` together if you would
rather run a single manager.

## Admission webhooks

An invalid object is refused by `kubectl apply` instead of being accepted and explained in a
status a reconcile later: a prefix length that does not fit the VPC, a claim for an account no
`NetworkScope` discovers, two scopes over the same account and region, an import whose tags AWS
would reject. Anything that depends on discovery having run — a VPC that is not in the
inventory yet — is a warning, not a refusal, so the apply order of a GitOps repository does not
matter. The controllers check the same rules regardless, so `webhook.enabled=false` costs
feedback, not correctness.

The serving certificate comes from cert-manager when the cluster has it
(`webhook.certificate.certManager=auto` looks for the `cert-manager.io/v1` API), and from the
chart itself otherwise. The self-signed path needs nothing in the cluster and no extra RBAC: the
chart signs a certificate, writes it to a secret and puts the CA in the webhook configurations.
On an upgrade it reads the existing secret back rather than rotating the CA out from under the
API server, so `helm upgrade` does not need a pod restart to keep admission working. Renewing
that certificate is manual: delete the secret and upgrade again. `helm template` has no cluster
to read the secret from, so a `helm template | kubectl apply` pipeline signs a new certificate
every run; install cert-manager, or use `helm upgrade`.

With `networkPolicy.enabled=true` the webhook port is open to every source, because admission
review calls come from the API server, which has no pod or namespace to select on and no
address the chart can know in advance. Narrow it through `networkPolicy.extraIngress` if your
API server's addresses are fixed.

Identifying fields are immutable after creation — a claim's scope, account, region and VPC, an
import's resource — because the operator never deletes what it created in AWS, and letting them
change would leave those resources behind with nothing pointing at them.

## Dashboard and alerts

`grafanaDashboard.enabled=true` ships `dashboards/subnet-inventory.json` as a ConfigMap labelled
`grafana_dashboard: "1"`, which the kube-prometheus-stack Grafana sidecar imports on its own.
`prometheusRule.enabled=true` adds the alerts; set the labels your Prometheus selects on
(usually `release: kube-prometheus-stack`).
