# Cutting a release

The steps a maintainer follows to release `vX.Y.Z`, in order. [GOVERNANCE.md](../GOVERNANCE.md#releases)
says when a release is cut, [policy.md](policy.md) what a version number promises. Most of what
is listed here is checked by the release workflow (`.github/workflows/release.yml`) and stops the
release if it is wrong; the rest is on this list because nothing checks it.

## 1. Before the release commit

- `master` is green in CI, including the `upgrade` job, which upgrades from the latest published
  release to `master`, and `upgrade-from-0-9`, which upgrades from 0.9 (it runs on `master`
  only, so look at the last push there).
- Every breaking change of the release is in [operations/upgrades.md](operations/upgrades.md)
  under "Upgrading from <previous> to <this>", and every deprecation or removal in the lists of
  [policy.md](policy.md#deprecation).
- `artifacthub.io/changes` in `charts/subnet-operator/Chart.yaml` lists this release's changes
  (`added`, `changed`, `deprecated`, `removed`, `fixed`, `security`), and nothing of the previous
  one. It may land in its own commit before the release commit, as it did for 0.9.

## 2. The release commit

One commit, `Release vX.Y.Z`, with a short summary of the release in its message:

| File | What changes |
|---|---|
| `charts/subnet-operator/Chart.yaml` | `version: X.Y.Z` and `appVersion: "X.Y.Z"`; the image in `artifacthub.io/images` (`ghcr.io/aivandrago/subnet-operator:X.Y.Z`); `artifacthub.io/prerelease`: `"true"` for 0.x and for versions with a pre-release suffix, `"false"` otherwise. **From 1.0 it is `"false"`**, and the comment above it, which explains the `"true"`, goes |
| `README.md` | The status section: "The latest release is vX.Y.Z", and the maturity it states (**in 1.0**: "Stable from 1.0." becomes "Stable."), and the roadmap phase the release completes (**in 1.0**: phase 6 gets its ✅) |
| `site/index.html` | The version pill in the navigation (`Alpha · vX.Y.Z` up to 0.9; `Stable · vX.Y.Z` from 1.0), the roadmap phase the release completes, and **in 1.0** "alpha" in the footer; `<!-- RELEASE: ... -->` comments mark each spot |
| `site/docs/index.html` | The tag in the `cosign verify ghcr.io/aivandrago/subnet-operator:vX.Y.Z` line; **in 1.0** also the pill (`Alpha · API v1` becomes `Stable · API v1`) and "alpha" in the footer, marked like the above |
| `docs/operations/upgrades.md` | The release's row in "What each release added": `vX.Y.Z (next)` becomes `vX.Y.Z` with its chart version |
| `api/v1/testdata/crds-1.0/` | **In the 1.0 release commit only**: refreshed from the CRDs the release ships, `cp config/crd/bases/*.yaml api/v1/testdata/crds-1.0/`. This is the frozen v1 schema that `TestV1OnlyGrowsFromTheBaseline` holds every later change to ([API compatibility](api-compatibility.md)). Never refreshed after 1.0 |

Before committing, run `make manifests generate api-docs` and look at `git status`: the release
commit changes the files above and nothing else. (The 0.9 release commit also carried a regenerated
`config/webhook/manifests.yaml` by accident, which had to be undone in the next commit.)

Then the same checks as for any pull request:

```sh
make manifests generate api-docs && git diff --exit-code
make lint test test-alerts test-dashboard helm-lint artifacthub-lint bundle-validate
```

`make api-docs` regenerates the [API reference](reference/api.md) from `api/v1` with a pinned
`crd-ref-docs`, so a diff there means a field changed without its reference. `make test` includes
the documentation link check (`test/docs`), which fails on a relative link or an `#anchor` in the
Markdown or on the site that points nowhere; a release commit that renames a heading is caught
there.

and CI on the release commit, the `upgrade` job included: until the release is published, it
still upgrades from the previous one. (On a checkout that carries the tag, `make test-upgrade`
skips itself: the latest published release is then the build under test.)

## 3. Tag

An annotated tag on the release commit, pushed to the forge:

```sh
git tag -a vX.Y.Z -m vX.Y.Z <release commit>
git push origin vX.Y.Z
```

If something has to be fixed after the release commit and before publishing, fix it in a new
commit and tag that one; a tag that has been published is never moved.

## 4. Publish

1. **GitHub**: `hack/publish-public.sh` pushes `master` as a single snapshot commit to the public
   repository's `main`, pushes the tag if it is not there yet, and marks the release latest.
2. **Release workflow**: the tag starts `.github/workflows/release.yml`, which builds the
   multi-arch image, signs it with cosign (keyless) and attaches its SBOM, checks that the chart's
   `version`, `appVersion`, `artifacthub.io/images` and `artifacthub.io/prerelease` match the tag,
   runs `make artifacthub-lint`, then packages, pushes and signs the chart at
   `oci://ghcr.io/aivandrago/charts/subnet-operator` and publishes the Artifact Hub repository
   metadata.
   A `bundle` job then generates the OLM bundle with the image pinned by the digest just pushed
   (channel `alpha` for 0.x and pre-releases, `stable` from 1.0, replacing the newest earlier
   release that has a bundle image), validates it, pushes and signs
   `ghcr.io/aivandrago/subnet-operator-bundle:vX.Y.Z` and attaches
   `subnet-operator-olm-bundle-X.Y.Z.tar.gz` to the release ([olm.md](olm.md)).
3. **Chart repository**: `make helm-publish` (with `CHARTMUSEUM_USER`/`CHARTMUSEUM_PASSWORD`)
   uploads the chart to `https://charts.hypersurgery.dev`.
4. **Site**: `hack/publish-pages.sh` publishes `site/`.

## 5. After publishing

- Verify the signatures the way the install guide does (the `cosign verify` line in
  `site/docs/index.html`), and that `helm show chart oci://ghcr.io/aivandrago/charts/subnet-operator`
  shows the new version.
- From then on the `upgrade` job upgrades from this release, on every pull request and on
  `master`, without a change to the test: it reads the latest published version and picks its
  checks from it ([policy.md](policy.md#the-upgrade-test)). Check the next run of the job on
  `master` (or start one): its log says `Upgrading from chart ... X.Y.Z`, and the "Upgrade from
  1.x" specs ran rather than skipped. If it fails, the release broke the upgrade to whatever
  comes next, and every pull request fails until that is fixed. A release that changes what is
  stored (a new storage version, a migration the operator runs itself) needs a path of its own
  in `test/e2e/upgrade_test.go`, the way 0.9 has one; the upgrade from the previous such release
  then moves to a job on `master`, like `upgrade-from-0-9`, and
  [policy.md](policy.md#the-upgrade-test) says so.
- **OperatorHub.io** (from 1.0): check the bundle job's output (`relatedImages` by digest,
  `replaces`, channel `stable` for 1.0 and later), verify the bundle image's signature, and open
  the pull request to `k8s-operatorhub/community-operators` with the attached bundle, as
  [olm.md](olm.md#publishing-on-operatorhubio) describes: one signed-off commit adding
  `operators/subnet-operator/X.Y.Z/`, plus `ci.yaml` in the first one. The listing is not part
  of the release: after the first merge, change the OLM paragraphs of the README, of the install
  page (`site/docs/index.html`) and the status of [olm.md](olm.md), which say the listing
  follows, to the OperatorHub.io install instructions.
- **Artifact Hub**: once the chart repository is registered there, check that the new version
  shows up with its images, changes and signature.
- Add the next release's row to "What each release added" in
  [operations/upgrades.md](operations/upgrades.md), marked `(next)`, once something lands for it.
