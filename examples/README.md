# Examples

Manifests you can copy. They use the `hs/` tag prefix; if your organization already tags
differently, keep your keys and point `networkSelector.matchTags`, `requiredSubnetTags` and `tagKeys`
at them. They are written for `network.hypersurgery.dev/v1beta1`; manifests for the older
`aws.hypersurgery/v1alpha1` convert with `manager migrate-manifests` (see the upgrade guide).

| File | What it shows |
|---|---|
| `01-single-account.yaml` | The smallest scope: the operator's own account, one region |
| `02-organization.yaml` | Several accounts through `sts:AssumeRole`, per-account regions, custom tag keys |
| `03-sheet-export.yaml` | Mirroring the inventory into a Google Sheet |
| `04-read-only-access.yaml` | RBAC so people can read the inventory but not edit it |
| `05-subnet-claim.yaml` | Subnets on demand: a `SubnetClaim` for three AZs |
| `06-resource-import.yaml` | Taking an existing subnet under management by tagging it |
| `07-auto-import-policy.yaml` | The auto-import policy: creator rules, network inheritance, skip rules, a namespace selector |
| `values-minimal.yaml` | Helm values: one account, IRSA, nothing else |
| `values-organization.yaml` | Helm values: change events, ServiceMonitor, alerts, dashboard, a scope |

```sh
kubectl apply -f examples/01-single-account.yaml
kubectl get subnets.network.hypersurgery.dev -o wide

helm install subnet-operator charts/subnet-operator \
  -n subnet-operator-system --create-namespace \
  -f examples/values-organization.yaml
```

The AWS side (IAM roles, EventBridge and SQS) lives in `deploy/` in CloudFormation form.
