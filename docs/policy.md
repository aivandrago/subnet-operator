# Versioning, deprecation and support policy

What you can rely on when you run this operator, and what you cannot yet. Every statement here
is meant to be true of the code and of CI as they are; where something is a plan rather than a
fact, it says so. If you find a gap between this page and reality, that is a bug — please open
an issue.

## At a glance

| | Today (0.x, `v1alpha1`) | From 1.0 |
|---|---|---|
| Breaking changes | Only in a minor release (0.**N**.0), listed in the release notes, with an upgrade note | Only in a major release |
| API stability | None promised; changes are additive in practice and every release upgrades from the previous one | Per API version, see [below](#api-versions) |
| Deprecated fields, flags, metrics, values | Kept for at least one minor release after the one that deprecates them | Kept for at least two minor releases or six months, whichever is longer |
| Tested upgrade path | From the latest published release to the next, in CI on every pull request | Same |
| Kubernetes | 1.34 – 1.37, oldest and newest tested in CI | The Kubernetes minors upstream supports, oldest and newest tested |
| Security fixes | On `master` and in the next release; no backports | Latest minor release, see [below](#security-fixes) |

## Versions

There are two version numbers, both [semantic versions](https://semver.org/):

- **The app version** — the operator's image, `ghcr.io/aivandrago/subnet-operator`, tagged with
  both spellings (`v0.6.0` and `0.6.0`) because Helm's `appVersion` carries no `v`. It is also the
  Git tag.
- **The chart version** — `charts/aws-subnet-operator`, `version:` in `Chart.yaml`, published to
  `oci://ghcr.io/aivandrago/charts/aws-subnet-operator` and to `https://charts.hypersurgery.dev`.
  A release normally moves both together (`version: 0.6.0`, `appVersion: "0.6.0"`). A change to
  the chart alone — a dashboard panel, a new value with a default that changes nothing — gets a
  chart-only patch release with the same `appVersion` (0.3.1 was one). The chart's `image.tag`
  defaults to its `appVersion`, so installing a chart version installs the image it was released
  with.

Images and charts are signed with cosign (keyless, by the release workflow in
`.github/workflows/release.yml`); the chart's `values.yaml` shows how to verify.

### What the numbers mean

- **Patch** (0.6.**1**): fixes and additions that need nothing from you. Never breaking.
- **Minor** (0.**7**.0): new features. **Before 1.0 a minor release may also contain breaking
  changes** (semver allows anything below 1.0; this project limits it to minor releases). Each one
  is listed in the release notes under its own heading with what to do, and in
  [operations/upgrades.md](operations/upgrades.md) when it affects an existing install.
- **Major** (**2**.0.0): from 1.0 on, the only place a breaking change may appear.

### What counts as breaking

A change is breaking when an existing, valid install could stop working, change behaviour, or
need a human to act after `helm upgrade`. In particular:

- **CRDs**: removing or renaming a field, changing its type or meaning, making an optional field
  required, tightening validation so that an object stored today is refused on its next update,
  changing a default in a way that changes behaviour (for example `spec.discoverUnmanaged`
  defaulting to `true` in 0.3.0 was one, and was announced as one), removing a kind or an API
  version.
- **Behaviour towards AWS**: needing an IAM permission the documented policies do not grant,
  writing to AWS where the previous release did not, under the same configuration.
- **Metrics and alerts**: removing or renaming a metric, a label or an alert, or changing what a
  metric measures. Dashboards and alert rules outside the chart depend on these.
- **Chart**: removing or renaming a value, changing a default so that the rendered install
  behaves differently, renaming resources the chart creates (a selector change forces a
  Deployment to be recreated), raising the chart's `kubeVersion` floor.
- **Command-line flags** of the manager: removing or renaming one.
- **Kubernetes support**: dropping a Kubernetes minor from the supported range (see
  [below](#kubernetes-versions) for how that range moves).

Not breaking: new optional fields, new kinds, new metrics, labels on new metrics, new alerts
(each is listed in the release notes, since it can fire where nothing did before), new chart
values whose default renders the same install, bug fixes (even when someone depended on the bug —
those are called out in the release notes), log messages, Kubernetes Event wording, and the audit
stream gaining fields ([audit.md](audit.md) documents the ones that exist).

## API versions

Every kind is `aws.hypersurgery/v1alpha1`, the only version served and stored.

**`v1alpha1` comes with no stability promise.** A field may still change or go. What the project
does promise, and tests, is that **upgrading from one release to the next keeps your objects and
their status**: the [upgrade test](#the-upgrade-test) installs the latest published release,
creates one of every kind, upgrades to the build under test and checks that nothing was lost,
recreated or reported again. Changes so far have been additive; when one is not, it comes with
a conversion path in the release notes, not with a broken cluster.

What the next versions will promise, once they exist:

- **`v1beta1`**: no field removed or changed in meaning within `v1beta1`. Fields may be
  deprecated and are then kept for the deprecation period below. When `v1beta1` is introduced,
  `v1alpha1` is served alongside it with a conversion webhook for at least one minor release,
  and existing objects are migrated to the new storage version by the operator, not by hand.
- **`v1`**: no breaking change without a new API version (`v2`), and every served version
  converts losslessly to every other. A version is served for at least two minor releases
  after it is deprecated.

`v1` is the precondition for the operator's 1.0, not a separate step.

## Deprecation

A deprecated field, flag, metric, alert or chart value:

1. is announced in the release notes of the release that deprecates it, with its replacement;
2. is marked *Deprecated* where it is documented — the CRD field's description (what
   `kubectl explain` shows), the flag's help text, the metric's help text, the comment in
   `values.yaml`;
3. keeps working, unchanged, for the deprecation period:
   - **before 1.0**: at least **one minor release** after the one that deprecates it — deprecated
     in 0.7.0 means removed in 0.8.0 at the earliest;
   - **from 1.0**: at least **two minor releases or six months**, whichever is longer;
4. is removed in a release whose notes list the removal as a breaking change.

A renamed metric is exported under both names during the deprecation period, so dashboards and
alerts can move at their own pace.

Nothing is deprecated today.

## Upgrades

The supported path is **from the previous release to the next one**. Upgrade one minor release
at a time; skipping releases is not tested. How to upgrade — apply the new CRDs with
`kubectl apply`, then `helm upgrade`, because Helm never upgrades CRDs — and how to roll back are
in [operations/upgrades.md](operations/upgrades.md).

### The upgrade test

`make test-upgrade` (and the `upgrade` job in `.gitea/workflows/ci.yml`, on every pull request,
every push to `master` and weekly) runs `test/e2e/upgrade_test.go` in Kind against Moto:

1. installs the **latest published chart** from `oci://ghcr.io/aivandrago/charts/aws-subnet-operator`
   with its own published image (`UPGRADE_FROM=<version>` picks another; `UPGRADE_CHART` another
   chart reference);
2. creates a `NetworkScope` across two accounts, a `SubnetClaim` that creates subnets, applied and
   dry-run `ResourceImport`s and a `SheetExport`, and waits for the `VPC` and `Subnet` objects;
3. applies the CRDs of the build under test and runs `helm upgrade` to the local chart and image;
4. checks that every object is still there — the same objects, not recreated — with its status;
   that the scope keeps syncing and stays Ready; that claims and imports stay Ready and nothing is
   created or tagged again in AWS; that the new release's webhooks accept an update of every
   existing object; that `hs_aws_unmanaged_resources_total` does not rise for unmanaged resources
   the previous release already knew, and does rise, by one, for one created after the upgrade.

It skips itself, saying why, only when the checkout is the release commit of the latest
published version, where there is nothing to upgrade from.

## Kubernetes versions

**Supported: Kubernetes 1.34, 1.35, 1.36 and 1.37.**

| | Kubernetes | Kind node image | When CI runs e2e on it |
|---|---|---|---|
| Newest | 1.37 | `kindest/node:v1.37.0` | every pull request, every push to `master`, weekly (e2e and the upgrade test) |
| Oldest | 1.34 | `kindest/node:v1.34.11` | every push to `master`, weekly |

The images are pinned by digest in `.gitea/workflows/ci.yml` and come from the Kind release the
Makefile pins (`KIND_VERSION`). Locally, `make test-e2e KIND_NODE_IMAGE=kindest/node:v1.34.11@sha256:…`
runs the suite on another version.

Why these:

- The operator is built against `k8s.io/client-go` v0.37 and controller-runtime v0.25, which
  targets the same Kubernetes 1.37 libraries. client-go's official promise is one minor version of
  skew either way. The operator only uses APIs that have been GA for years —
  `apiextensions.k8s.io/v1`, `admissionregistration.k8s.io/v1`, `coordination.k8s.io/v1` leases,
  `apps/v1`, `policy/v1`, `events.k8s.io/v1` — so servers older than that window work too, and
  the oldest supported one is run in CI to keep that claim tested rather than assumed.
- 1.34 is the oldest minor the Kubernetes project still maintains at the time of writing, and
  the oldest Kind (v0.33) publishes a node image for; 1.37 is the newest release.
- Testing only the two ends keeps CI affordable: e2e needs a Kind cluster on a dedicated host.
  Pull requests run the newest version, where removals and deprecations show up first; the
  oldest runs after the merge and weekly, so a break there is found within a day of landing, not
  at release time.

The range moves with upstream: when a new Kubernetes minor is released and Kind publishes a node
image for it, it becomes the newest tested version; when upstream stops maintaining the oldest,
it leaves the range in the next minor release, announced in the notes.

The chart's `kubeVersion` is `>=1.28.0-0`: Helm refuses anything older, and nothing the chart
renders needs a newer API server. **1.28 to 1.33 install but are not supported**: nothing is
tested there, and a bug that only reproduces there is fixed on a best-effort basis.

The Helm chart is tested with the Helm version the Makefile pins (`HELM_VERSION`, v4); Helm 3.8
or newer is needed for installing from an OCI registry.

## Releases

There is no fixed release cadence: maintainers cut a release when there is something worth
shipping ([GOVERNANCE.md](../GOVERNANCE.md#releases)), and a patch release as soon as a fix is
worth having. Each release has a tag, notes that say what was verified and what was not, a chart
version and a signed, multi-arch image. The release notes and
[operations/upgrades.md](operations/upgrades.md) are where breaking changes and upgrade steps
are written down.

## Security fixes

How to report a vulnerability, and what the operator can and cannot do by design, is in
[SECURITY.md](../SECURITY.md).

- **Before 1.0** (now): fixes land on `master` and ship in the next release; a security fix is
  reason enough to cut a patch release rather than wait for features. There are no backports to
  older minor releases: the fix for 0.5.x is to upgrade to the release that carries it.
- **From 1.0**: the latest minor release gets every security fix as a patch release; the minor
  release before it gets fixes for vulnerabilities rated high or critical for three months after
  its successor is released.
