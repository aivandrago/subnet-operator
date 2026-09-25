# Versioning, deprecation and support policy

What you can rely on when you run this operator, and what you cannot yet. Every statement here
is meant to be true of the code and of CI as they are; where something is a plan rather than a
fact, it says so. If you find a gap between this page and reality, that is a bug — please open
an issue.

**1.0 is the first stable release.** "Stable" means the promises on this page and in
[API compatibility](api-compatibility.md): the `v1` API only grows, breaking changes wait for a
major release, deprecations run their full period, security fixes reach the latest minor
release. It does not mean the operator has been proven in a large real AWS organization: CI
tests every change against [Moto](https://github.com/getmoto/moto) in Kind, including an upgrade
from the previous release, and a conformance run against a real AWS organization has not been
done yet. If you run it against one, a report is the most useful contribution there is
([CONTRIBUTING.md](../CONTRIBUTING.md)).

## At a glance

| | 0.x (up to 0.9, `network.hypersurgery.dev/v1beta1`) | From 1.0 (`network.hypersurgery.dev/v1`) |
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
- **The chart version** — `charts/subnet-operator`, `version:` in `Chart.yaml`, published to
  `oci://ghcr.io/aivandrago/charts/subnet-operator` and to `https://charts.hypersurgery.dev`
  (up to 0.7 the chart was called `aws-subnet-operator`, and is published under that name).
  A release normally moves both together (`version: 0.6.0`, `appVersion: "0.6.0"`). A change to
  the chart alone — a dashboard panel, a new value with a default that changes nothing — gets a
  chart-only patch release with the same `appVersion` (0.3.1 was one). The chart's `image.tag`
  defaults to its `appVersion`, so installing a chart version installs the image it was released
  with.

Images and charts are signed with cosign (keyless, by the release workflow in
`.github/workflows/release.yml`); the chart's `values.yaml` shows how to verify. From 1.0 each
release also publishes an OLM bundle image, `ghcr.io/aivandrago/subnet-operator-bundle:vX.Y.Z`,
signed the same way, with the operator image pinned by digest ([olm.md](olm.md)).

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
  required, tightening validation so that an object stored today is refused on its next update
  (within `v1` a security fix is the one exception, under the conditions in
  [API compatibility](api-compatibility.md#security-fixes)),
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

From 1.0 every kind is `network.hypersurgery.dev/v1`, the version stored, and
`network.hypersurgery.dev/v1beta1`, the API of 0.8 and 0.9, is served next to it, deprecated.
Both have the same fields; the operator's conversion webhook converts between them, and the
operator rewrites what 0.9 stored at v1 on its first start
([operations/upgrades.md](operations/upgrades.md#upgrading-from-09-to-10)).

Up to 0.7 the API was `aws.hypersurgery/v1alpha1`. A conversion webhook cannot move objects
between two groups, so 0.8 served both, deprecated the old one and migrated every old object
into the new group itself, status included ([operations/upgrades.md](operations/upgrades.md#upgrading-from-07-to-08)).
0.9 removes the old group ([ADR 0002](adr/0002-multi-cloud-model.md) §10): it neither serves nor
migrates it, and does not start its controllers while an old object 0.8 never migrated exists, so
**0.7 upgrades through 0.8** ([operations/upgrades.md](operations/upgrades.md#upgrading-from-08-to-09)).
`manager migrate-manifests`, which rewrites manifests kept in git, stays, and from 1.0 also moves
v1beta1 manifests to v1.

What each version promises:

- **`v1alpha1`** (the old group, removed in 0.9) promised nothing, and what the project promised
  and tested instead held: **upgrading from one release to the next keeps your objects and their
  status**. The [upgrade test](#the-upgrade-test) checked that across the move to the new group
  (0.7 to 0.8) and to 0.9.
- **`v1beta1`** (0.8 and 0.9; deprecated in 1.0): no field was removed or changed in meaning
  within it, and it converts losslessly to `v1`. It is **served until at least 1.2 and at least
  six months after 1.0**, whichever is later; the release that stops serving it lists that as a
  breaking change.
- **`v1`**: no breaking change without a new API version (`v2`), and every served version
  converts losslessly to every other. `provider` is an open enum: values for new clouds are
  added, and a client must skip a value it does not know. The spec enums `SubnetClaim.spec.mode`
  and `NetworkScope.spec.autoImport.mode` are closed: a new value there changes behaviour and
  needs a new API version or an opt-in field. Provider members (`aws`, later `gcp`,
  `azure`) and optional fields may be added; nothing is removed or renamed. The whole promise,
  with what a client must do, is [API compatibility](api-compatibility.md). A version is served
  for at least two minor releases or six months after it is deprecated, whichever is longer.

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

Deprecated in 0.8, removed in 0.9:

- the `aws.hypersurgery/v1alpha1` API group: its CRDs are no longer in the chart (the ones in a
  cluster stay until somebody deletes them), and nothing serves or migrates it;
- the chart values `networkScope.vpcTagSelector` and `roleARN`, `externalID` and `writeRoleARN`
  next to an account's `id` (now `networkScope.networkSelector.matchTags` and an `aws` member),
  which the chart now refuses;
- the manager flag `--migrate-v1alpha1`, which went with the migration.

Removed in 0.9 without a deprecation period, as an exception to the rule above: the project
had no users yet when the neutral metrics were ready, and keeping both names would have doubled
every series for a release that nobody needed.

- the `hs_aws_*` metrics, with their labels `vpc_id` and `az` and the unmanaged `kind="vpc"`,
  replaced by `hs_*` with `provider`, `network_id`, `zone` and `kind="network"`;
- the alert `VPCCIDROverlap`, renamed `NetworkCIDROverlap`.

[upgrades.md](operations/upgrades.md#upgrading-from-08-to-09) has the mapping and what to change
in rules, dashboards, routes and silences of your own.

Deprecated in 0.9, removed in 1.0 (announced for 0.10; 1.0 is the minor release after 0.9, so
the one-release period holds):

- the chart values `events.queueUrl`, `events.debounce`, `aws.region` and `aws.endpointURL`
  (now under `providers.aws`), and the manager flags `--events-queue-url` and
  `--events-debounce` with the environment variable `EVENTS_QUEUE_URL` (now
  `--aws-events-queue-url` and `--aws-events-debounce`), deprecated by the provider registry
  (#43);
- the chart value `networkPolicy.egress.podIdentity` (now `providers.aws.podIdentity`).

The chart refuses to render with any of these values set, or with the old flags or
`EVENTS_QUEUE_URL` in `extraArgs`/`extraEnv`, and names the replacement; the manager refuses to
start with the old flags or with `EVENTS_QUEUE_URL` set, and says which to use instead. Ignoring
them would leave the operator running without its event queue, in another region, or cut off
from the EKS Pod Identity agent, without a word. Move them before upgrading
([operations/upgrades.md](operations/upgrades.md#values-and-flags-removed-in-10)).

Deprecated in 1.0, served until at least 1.2 and at least six months after 1.0:

- the API version `network.hypersurgery.dev/v1beta1`, replaced by `network.hypersurgery.dev/v1`
  with the same fields ([API compatibility](api-compatibility.md#v1beta1)).

The chart's old name, `aws-subnet-operator`, is not published any more from 0.8 on.

## Upgrades

The supported path is **from the previous release to the next one**. Upgrade one minor release
at a time; skipping releases is not tested. How to upgrade — apply the new CRDs with
`kubectl apply`, then `helm upgrade`, because Helm never upgrades CRDs — and how to roll back are
in [operations/upgrades.md](operations/upgrades.md).

### The upgrade test

`make test-upgrade` (and the `upgrade` job in `.gitea/workflows/ci.yml`, on every pull request,
every push to `master` and weekly) runs `test/e2e/upgrade_test.go` in Kind against Moto:

1. installs the **latest published chart**, `oci://ghcr.io/aivandrago/charts/subnet-operator`,
   with its own published image (`UPGRADE_FROM=<version>` picks another version; `UPGRADE_CHART`
   another chart reference). For 1.0 that is 0.9, the last release that stores
   `network.hypersurgery.dev/v1beta1`;
2. creates a `NetworkScope` across two accounts, a `SubnetClaim` that creates subnets, applied and
   dry-run `ResourceImport`s and a `SheetExport` at v1beta1, and waits until they have settled;
3. applies the CRDs of the build under test, checks that every CRD now lists `[v1beta1 v1]` in
   `status.storedVersions`, checks that a `helm upgrade` which still sets the values 0.9
   deprecated is refused, naming their replacements, and changes nothing, and then runs
   `helm upgrade` of the same release to the local chart and image, the way the upgrade guide
   says. Both releases are installed with the `providers.aws` names, which 0.9 already reads;
4. checks that the operator trims `storedVersions` to `[v1]` on every CRD, with an Event and the
   metrics, and points their conversion at the release's webhook with the chart's CA; that the
   scope keeps syncing and stays Ready with the same networks and subnets, claims keep their
   reservations and creator and stay Ready, imports are not applied again, and nothing is created
   or tagged again in AWS; that every object reads the same at v1beta1 as at v1, with the
   deprecation warning, and can be written at v1beta1; that the new release's webhooks accept an
   update of every object; that `hs_unmanaged_resources_total` does not rise for unmanaged
   resources the previous release already knew, and does rise, by one, for one created after the
   upgrade;
5. restarts the operator and checks that it rewrites nothing the second time.

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

- **Before 1.0**: fixes landed on `master` and shipped in the next release; a security fix was
  reason enough to cut a patch release rather than wait for features. There were no backports to
  older minor releases: the fix for 0.5.x is to upgrade to the release that carries it. That
  stays so for every 0.x release, 0.9 included: the fix is to upgrade to 1.0 or later.
- **From 1.0**: the latest minor release gets every security fix as a patch release; the minor
  release before it gets fixes for vulnerabilities rated high or critical for three months after
  its successor is released. A security fix may tighten the validation of `v1` where nothing
  looser closes the hole, announced as breaking in the release notes and the advisory, with
  objects already stored kept working as long as the tightened field is not changed
  ([API compatibility](api-compatibility.md#security-fixes)).
