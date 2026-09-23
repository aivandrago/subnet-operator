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
| `replicaCount` | `1` | Manager replicas. More than one only makes sense with `leaderElection.enabled`. |
| `image.repository` / `image.tag` | `registry.hypersurgery.dev/gitadmin/aws-subnet-operator` / chart `appVersion` | Manager image. |
| `imagePullSecrets` | `[]` | The registry currently requires a login, so a pull secret is needed. |
| `serviceAccount.create` / `.name` / `.annotations` | `true` / `""` / `{}` | Service account; annotate for IRSA. |
| `discovery.concurrency` | `4` | Account/region pairs discovered in parallel. |
| `events.queueUrl` | `""` | SQS queue fed by EventBridge. Empty means the resync interval is the only trigger. |
| `events.debounce` | `10s` | How long events are collected before the affected targets resync. |
| `aws.region` | `""` | Region for the operator's own calls. |
| `aws.endpointURL` | `""` | Non-AWS endpoint, for testing against emulators. |
| `writes.enabled` | `false` | Let `SubnetClaim`s in Create mode create subnets. Allocation works without it. |
| `leaderElection.enabled` | `true` | Leader election lease in the release namespace. |
| `metrics.secure` / `.port` | `true` / `8443` | HTTPS with authn/authz, or plain HTTP when false. |
| `metrics.serviceMonitor.enabled` | `false` | ServiceMonitor for the Prometheus operator. |
| `prometheusRule.enabled` | `false` | Alerts: subnet nearly full or full, target down, CIDR overlap, stale inventory. |
| `prometheusRule.thresholds.subnetUsedRatio` | `0.85` | When a subnet counts as nearly full. |
| `prometheusRule.thresholds.staleSyncSeconds` | `3600` | When the inventory counts as stale. |
| `grafanaDashboard.enabled` | `false` | ConfigMap with the dashboard, for the Grafana sidecar. |
| `networkScope.create` | `false` | Also create a `NetworkScope` with the release. |
| `sheetExport.create` | `false` | Also create a `SheetExport` (Google Sheet mirror). |
| `resources` | 500m / 256Mi limits | Container resources. |
| `podSecurityContext`, `securityContext` | non-root, read-only rootfs | Pod and container security. |
| `extraArgs`, `extraEnv` | `[]` | Extra manager flags and environment variables. |
| `nodeSelector`, `tolerations`, `affinity`, `topologySpreadConstraints`, `priorityClassName` | empty | Scheduling. |

See `examples/values-minimal.yaml` and `examples/values-organization.yaml` in the repository.

## Dashboard and alerts

`grafanaDashboard.enabled=true` ships `dashboards/subnet-inventory.json` as a ConfigMap labelled
`grafana_dashboard: "1"`, which the kube-prometheus-stack Grafana sidecar imports on its own.
`prometheusRule.enabled=true` adds the alerts; set the labels your Prometheus selects on
(usually `release: kube-prometheus-stack`).
