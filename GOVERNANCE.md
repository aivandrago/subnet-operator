# Governance

The project is young and small, and this document says what is actually true today rather than
describing a structure that does not exist yet.

## Today

Decisions are made by the maintainers in [MAINTAINERS.md](MAINTAINERS.md), in the open, on pull
requests and issues. Anything that changes an API, adds a CRD or changes what the operator is
allowed to do in a cloud account is discussed in an issue before it is written.

Changes need one maintainer's approval and green CI. A maintainer may merge their own change
once CI is green when it is documentation or an obvious fix; anything touching behaviour waits
for a second pair of eyes once there is a second maintainer to provide them.

## Becoming a maintainer

Sustained, useful contribution — code, review, documentation or the unglamorous work of
reproducing other people's problems. An existing maintainer proposes, the others agree, and the
change is recorded in `MAINTAINERS.md`. There is no minimum commit count; judgement about what
belongs in the project matters more than volume.

## Direction

The roadmap lives in the README and on https://hypersurgery.dev. Two principles are not up for
casual revision, because the project stops making sense without them:

1. **Cloud tags are the source of truth.** The cluster mirrors them; it does not become a second
   inventory people maintain by hand.
2. **Read-only by default, and never destructive.** Writing requires an explicit flag and a
   separate IAM role, and no code path deletes a cloud resource.

Changing either needs agreement among all maintainers and a written rationale in an issue.

## Releases

Maintainers cut releases when there is something worth shipping. Each release has a tag, notes
that say what was verified and what was not, a chart version and a container image.

The steps, from the release commit to publishing, are in [docs/release.md](docs/release.md).
