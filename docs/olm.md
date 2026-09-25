# Installing with OLM and publishing on OperatorHub

The operator is also packaged as an [Operator Lifecycle Manager](https://olm.operatorframework.io/)
(OLM) bundle, for clusters that install operators through OLM (OpenShift, OKD, or any cluster
with OLM installed) and, from 1.0, for [OperatorHub.io](https://operatorhub.io).

**Status.** The bundle is built, validated and installed through OLM on Kind in CI on every pull
request. From 1.0 every release pushes a signed bundle image,
`ghcr.io/aivandrago/subnet-operator-bundle:<tag>`, and attaches the bundle to its GitHub release.
The OperatorHub.io listing is **not part of the release itself**: it follows once the first
pull request to community-operators, opened after the release is published, is merged. Until
then, install from the bundle image (below). The Helm chart stays the primary, fuller way to
install.

## What the bundle contains

| | Helm chart | OLM bundle |
|---|---|---|
| CRDs, RBAC, the manager | yes | yes |
| Admission webhooks | yes: a certificate the chart signs, or cert-manager | yes: OLM issues and rotates the certificate |
| Conversion webhook (v1beta1 ↔ v1) | yes: the operator points the CRDs at it | yes: OLM points the CRDs at it and injects the CA |
| Replicas | 2, with a PodDisruptionBudget | 1 |
| Metrics Service (HTTPS, authenticated) | yes | yes |
| ServiceMonitor, PrometheusRule, Grafana dashboard | optional | no: add them from the chart's templates if you need them |
| NetworkPolicies | optional | no |
| Dashboard | optional | no |
| Manager flags | chart values | fixed (see [Limits](#limits)) |

The bundle is generated, not committed. Its sources are:

- `config/manifests/bases/subnet-operator.clusterserviceversion.yaml`: the ClusterServiceVersion
  (CSV) base: description, icon, keywords, maintainers, links, install modes, `minKubeVersion`.
- The `+operator-sdk:csv:customresourcedefinitions` markers in `api/v1` and `api/v1beta1`: the
  display names and the spec and status descriptors of the six kinds, at both versions.
- `config/default` (Deployment, RBAC, metrics Service), `config/webhook` (the admission
  webhooks), the conversion stanza in `config/manifests/kustomization.yaml`, `config/samples`
  (the `alm-examples`) and `config/scorecard`.

`Network` and `Subnet` are marked as internal objects (the operator writes them; users read
them), so OLM consoles offer no form to create them.

## Decisions

**Install mode: AllNamespaces only.** The operator watches cluster-scoped kinds (`NetworkScope`,
`Network`, `Subnet`, `SheetExport`) and namespaced claims and imports in every namespace, and a
`NetworkScope`'s `namespaceSelector` decides which namespaces may use it. OLM also allows
conversion webhooks only for operators installed in AllNamespaces mode. OwnNamespace,
SingleNamespace and MultiNamespace are declared unsupported.

**Channel: `alpha` for 0.x and pre-releases, `stable` from 1.0.** The release workflow picks the
channel from the tag (the same rule that marks a chart as a pre-release on Artifact Hub) and sets
the CSV's `maturity` to match. The first bundle published on OperatorHub.io will be 1.0.0 in
`stable`; `make bundle` defaults to `alpha`.

**Capability level: Basic Install.** The operator has no operands whose upgrades it manages;
its own upgrades are OLM's. The storage migration it runs on its own objects (v1beta1 to v1) is
not the "Seamless Upgrades" level of an operand. Revisit once there is a 1.x to 1.y upgrade path
that needs more than OLM replacing the CSV.

**Upgrade graph: `replaces` plus `olm.skipRange`.** Each bundle `replaces` the newest earlier
release that has a published bundle (the release workflow finds it from the tags and the
registry), which keeps the channel a single chain as community-operators' `replaces-mode`
expects. Each bundle also carries `olm.skipRange: <VERSION`, so a cluster several releases behind
upgrades straight to the newest one instead of stepping through each. The first bundle replaces
nothing.

**Conversion and certificates belong to OLM.** The bundle's `webhookdefinitions` declare the
three validating and three mutating admission webhooks and one `ConversionWebhook` for all six
CRDs. OLM creates the Service, issues the certificate, mounts it at
`/tmp/k8s-webhook-server/serving-certs` (where the manager looks when `--webhook-cert-path` is not
set), injects the CA into the webhook configurations and rewrites each CRD's
`spec.conversion` to its Service and CA. The manager runs with `--crd-conversion` unset, as in the
kustomize install, so it never touches the conversion OLM configured. The cert-manager sections of
`config/default` are not used.

**Images are pinned by digest.** A release generates the bundle with the operator image by digest,
so the Deployment and `relatedImages` (read by mirroring tools for disconnected clusters) name
exactly the signed image.

**The CRD permissions are expected.** `operator-sdk bundle validate` warns that the CSV "contains
permissions to create CRD". It does not: the operator may `get` and `patch` its own six CRDs and
`update` their status, by name. It trims `status.storedVersions` to `[v1]` after rewriting stored
objects at v1 (see [the upgrade guide](operations/upgrades.md)); the `patch` is only used with
`--crd-conversion=webhook`, which the bundle does not set.

## Installing through OLM

OLM must be installed. OpenShift and OKD have it; elsewhere:

```sh
operator-sdk olm install
```

### From OperatorHub.io (once listed)

Once the package is listed on OperatorHub.io, install it into its own namespace, watching all namespaces:

```sh
kubectl create namespace subnet-operator-system
kubectl apply -f - <<'EOF'
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: subnet-operator
  namespace: subnet-operator-system
spec: {}          # no targetNamespaces: AllNamespaces
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: subnet-operator
  namespace: subnet-operator-system
spec:
  name: subnet-operator
  channel: stable
  source: operatorhubio-catalog
  sourceNamespace: olm
EOF
kubectl get csv -n subnet-operator-system -w
```

OperatorHub.io's own instructions (`kubectl create -f https://operatorhub.io/install/subnet-operator.yaml`)
install into the `operators` namespace instead; that works the same, but the IAM association
below then names that namespace.

### From the bundle image

Before the listing, or to try a specific release:

```sh
cosign verify ghcr.io/aivandrago/subnet-operator-bundle:<tag> \
  --certificate-identity-regexp '^https://github.com/aivandrago/subnet-operator/\.github/workflows/release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
kubectl create namespace subnet-operator-system
operator-sdk run bundle ghcr.io/aivandrago/subnet-operator-bundle:<tag> \
  --namespace subnet-operator-system --install-mode AllNamespaces
```

`<tag>` is the release tag, e.g. `v1.0.0`. `operator-sdk cleanup subnet-operator -n subnet-operator-system`
removes it again.

### AWS credentials

The operator runs as the service account `subnet-operator-controller-manager` in the namespace
it was installed in. OLM creates and owns that account, so the chart's way of setting IRSA (an
annotation on the service account, from values) is not available.

**EKS Pod Identity (recommended).** Associate the role from [deploy/iam](../deploy/iam) with the
service account; nothing in the cluster changes:

```sh
aws eks create-pod-identity-association --cluster-name <cluster> \
  --namespace subnet-operator-system --service-account subnet-operator-controller-manager \
  --role-arn arn:aws:iam::111111111111:role/aws-subnet-operator
kubectl rollout restart -n subnet-operator-system deployment/subnet-operator-controller-manager
```

**IRSA.** Give the pod the web identity itself, through the Subscription's `config`, which OLM
applies to the Deployment and keeps across upgrades. The role's trust policy must allow
`system:serviceaccount:subnet-operator-system:subnet-operator-controller-manager` with audience
`sts.amazonaws.com`, as for the chart:

```yaml
spec:
  config:
    env:
      - name: AWS_ROLE_ARN
        value: arn:aws:iam::111111111111:role/aws-subnet-operator
      - name: AWS_WEB_IDENTITY_TOKEN_FILE
        value: /var/run/secrets/eks.amazonaws.com/serviceaccount/token
      - name: AWS_REGION
        value: eu-central-1
    volumes:
      - name: aws-iam-token
        projected:
          sources:
            - serviceAccountToken:
                audience: sts.amazonaws.com
                expirationSeconds: 86400
                path: token
    volumeMounts:
      - name: aws-iam-token
        mountPath: /var/run/secrets/eks.amazonaws.com/serviceaccount
        readOnly: true
```

The Google Sheet credentials Secret of a `SheetExport` lives in the operator's namespace, as with
the chart.

### Limits

- **The manager's flags are fixed.** OLM passes a Subscription's `config.env` to the manager but
  not arguments, and the manager reads its settings from flags only. So `--enable-writes` stays
  off (`SubnetClaim` in `Create` mode, `ResourceImport` and the auto-import policy report that
  writes are disabled) and there is no change-events queue (`--aws-events-queue-url`, README step
  2): the operator discovers on its resync interval. Read-only discovery, compliance, metrics,
  `Allocate`-mode claims and the sheet export work. Use the chart for writes and change events
  until the manager can take these settings another way.
- One replica: OLM's Deployment has no PodDisruptionBudget, and a second replica would only wait
  for the leader lease.
- No ServiceMonitor, PrometheusRule or Grafana dashboard. The metrics Service
  (`subnet-operator-controller-manager-metrics-service`, port 8443, authenticated with a token
  allowed by the `subnet-operator-metrics-reader` ClusterRole) is there to scrape.

## Development

```sh
make bundle            # generate bundle/ and bundle.Dockerfile (VERSION defaults to the chart's appVersion)
make bundle-validate   # the same, then operator-sdk bundle validate with the operatorframework suite
make bundle-build bundle-push BUNDLE_IMG=<registry>/subnet-operator-bundle:v<version>
make catalog BUNDLE_IMGS=<pushed bundle images>   # a file-based catalog in catalog/, then catalog-build catalog-push
make test-olm          # Kind + OLM: install the bundle, check the webhooks and conversion, run scorecard
```

`make bundle` takes `VERSION`, `BUNDLE_OPERATOR_IMG` (by digest for a release),
`CHANNELS`/`DEFAULT_CHANNEL` and `BUNDLE_REPLACES`. It changes no committed file; it rewrites
`config/manifests/bases` from the API markers, and CI fails when that leaves a diff. After
changing a descriptor marker or the CSV base, run `make bundle` and commit the base.

`make test-olm` (CI job `olm`) creates a Kind cluster, starts a registry container on the Kind
network, builds and pushes the operator image and a bundle that pins it by digest, installs OLM
(release manifests checked against pinned checksums), installs the bundle with
`operator-sdk run bundle` in AllNamespaces mode, and checks that:

- the CSV succeeds and the Deployment rolls out;
- every CRD converts through OLM's Service with OLM's CA, and the operator raised no
  `ConversionConfigured` Event (it left the conversion alone);
- OLM created the three validating and three mutating webhook configurations with its CA;
- a `NetworkScope` is admitted and reads back at v1beta1 (conversion), and a duplicate region
  written at v1beta1 is converted and then refused by the validating webhook;
- `operator-sdk scorecard` passes with the basic and OLM suites (`config/scorecard`), except
  `olm-crds-have-resources`, left out because `Network` and `Subnet` create no objects to list.

Scorecard can be run by hand against any cluster with OLM the operator is installed in:
`operator-sdk scorecard ./bundle --namespace subnet-operator-system`.

## Publishing on OperatorHub.io

OperatorHub.io is built from [k8s-operatorhub/community-operators](https://github.com/k8s-operatorhub/community-operators).
Each release is one pull request there that adds a directory for the version. The first one is
reviewed by hand; later ones merge automatically once their checks pass, when opened by a
reviewer listed in `ci.yaml`. OpenShift's embedded OperatorHub reads a different repository,
[redhat-openshift-ecosystem/community-operators-prod](https://github.com/redhat-openshift-ecosystem/community-operators-prod),
with the same layout and process; submitting there is optional.

For each release, after the release workflow has finished:

1. **Get the bundle.** The release has `subnet-operator-olm-bundle-<version>.tar.gz` attached,
   generated with the image digest, the channel and the `replaces` of that release: the same
   files as the bundle image. The tarball itself carries no signature; verify the bundle image
   (above) before submitting. Unpack it:

   ```sh
   gh release download v<version> -R aivandrago/subnet-operator -p 'subnet-operator-olm-bundle-*.tar.gz'
   tar -xzf subnet-operator-olm-bundle-<version>.tar.gz     # bundle/ and bundle.Dockerfile
   ```

   Check `bundle/manifests/subnet-operator.clusterserviceversion.yaml`: `spec.version`, the
   image digest in the Deployment and `relatedImages`, `spec.replaces` (the previous version on
   OperatorHub.io, absent for the first) and `metadata/annotations.yaml` (the channel).

2. **Fork and branch** `k8s-operatorhub/community-operators`, and copy the bundle in:

   ```sh
   git clone https://github.com/<you>/community-operators && cd community-operators
   git checkout -b subnet-operator-<version>
   mkdir -p operators/subnet-operator/<version>
   cp -R ../bundle/manifests ../bundle/metadata ../bundle/tests operators/subnet-operator/<version>/
   ```

   The result is:

   ```
   operators/subnet-operator/
     ci.yaml                     # first pull request only
     <version>/
       manifests/                # CSV, the six CRDs, the metrics Service, the ClusterRoles
       metadata/annotations.yaml
       tests/scorecard/config.yaml
   ```

3. **First pull request only: `ci.yaml`.** It declares the upgrade graph mode and who may
   approve updates:

   ```yaml
   ---
   updateGraph: replaces-mode
   reviewers:
     - aivandrago
   ```

4. **Commit once, signed off** (the repository requires the DCO sign-off and a single commit):

   ```sh
   git add operators/subnet-operator
   git commit -s -m "operator subnet-operator (<version>)"
   git push -u origin subnet-operator-<version>
   ```

   Open the pull request with the title `operator subnet-operator (<version>)` and fill in the
   template's checklist. Their pipeline validates the bundle, installs it, and for updates also
   upgrades from the previous version.

5. **After the merge** the version appears on OperatorHub.io and in
   `quay.io/operatorhubio/catalog` within a few hours. After the first merge, change the OLM
   paragraphs of the README (Getting started, Install) and of the site's install page, which say
   the listing follows, to point at OperatorHub.io and the install instructions above, and the
   Status paragraph at the top of this page.

The package name `subnet-operator` is taken with the first pull request and cannot be renamed
afterwards; community-operators treats a rename as removing one operator and adding another.
