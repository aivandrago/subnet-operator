# API compatibility

What may change in `network.hypersurgery.dev/v1`, and what may not, from 1.0 on. It adds detail
to the [support policy](policy.md#api-versions); where the two seem to disagree, this page is
the one to hold us to, and the disagreement is a bug.

## What the promise covers

- **The six kinds** at `network.hypersurgery.dev/v1`: `NetworkScope`, `Network`, `Subnet`,
  `SubnetClaim`, `ResourceImport` and `SheetExport`. Their fields, what each one means, its
  type, its default, whether it is required, and its validation, as the CRDs in the chart's
  `crds/` directory define them. The [API reference](reference/api.md) lists them field by
  field, generated from `api/v1`.
- **The labels and annotations the operator documents**: `network.hypersurgery.dev/scope`,
  `/provider`, `/account`, `/region`, `/network` and `/resource` on the objects it writes,
  `network.hypersurgery.dev/created-by` and `/reason`, and the record `/migrated-from` and
  `/migrated-to` that 0.8 left.
- **The condition types**: `Ready` on every kind that has conditions, and `Allocated` on
  `SubnetClaim`.

Not covered: the Go packages under `api/` beyond the JSON they produce (a Go field or a helper
method may be added, renamed or removed in any release), Event reasons and messages, log lines,
and the text of condition messages. Metrics, alerts, chart values and flags follow the
[support policy](policy.md#what-counts-as-breaking).

## What may change within v1

In a minor release, and listed in its notes:

- **`provider` gains values.** It is an **open enum**: today `AWS`; `GCP` and `Azure` are added
  by the releases that implement them ([ADR 0002](adr/0002-multi-cloud-model.md) §2). A client
  that reads `spec.provider` of a `NetworkScope`, `Network` or `Subnet` must treat a value it
  does not know as "a provider this client does not know": skip the object, or show it as
  such, and never fail on it.
- **Provider members are added.** Each kind keeps its provider-specific settings in a member
  named after the provider (`aws` today); `gcp` and `azure` members are added next to it, in
  spec and in status. A member is only ever valid with its own provider.
- **Optional fields are added**, in spec or in status. A new spec field either has no default or
  a default that keeps every existing object behaving as before. A client must ignore fields it
  does not know, and must not drop them when it writes an object back (use a patch, or
  server-side apply, rather than replacing the object from an older type).
- **Other open sets gain values**: `NetworkScope.status.capabilities`, the ownership models in
  `status.ownership`, `Subnet.status.ownershipSource`, the `state` of an allocation or an
  import, and condition reasons. Their documentation says so; a client must ignore a value it
  does not know.
- **Validation is loosened**, for example a longer maximum or a wider pattern. This does not
  extend to the closed enums [below](#closed-enums): a new value there is not a loosening.
- **A field is deprecated**: its description says so (`kubectl explain` shows it), it keeps
  working, and it stays until v2.
- **New kinds are added**, in the same group.

## What never changes within v1

- A field, a kind or a value of an enumeration is **not removed or renamed**.
- A field's **type or meaning does not change**. Neither does a default, in a way that would
  change what an existing object does.
- A **closed enum** (`SubnetClaim.spec.mode`, `NetworkScope.spec.autoImport.mode`) does not
  gain a value ([below](#closed-enums)).
- An optional field is **not made required**.
- Validation is **not tightened** for what v1 already has: a stored object that the API server
  accepted keeps being accepted when it is written back unchanged. (Rules that only concern a
  field added later are new, not tighter.) The one exception is a security fix,
  [below](#security-fixes).
- The **list types and keys** stay as they are (`atomic`, `set`, or `map` with its keys), since
  server-side apply and GitOps tools merge by them.
- The **status of an object is not reset** by an upgrade: allocations of a `SubnetClaim`, the
  state and history of a `ResourceImport`, the known unmanaged resources of a `NetworkScope`.

### Closed enums

`SubnetClaim.spec.mode` (`Allocate`, `Create`) and `NetworkScope.spec.autoImport.mode` (`Off`,
`DryRun`, `Apply`) are **closed**, unlike `provider`. Each value says how far the operator goes
on its own — whether it creates subnets, whether it writes tags — and a client, a policy engine
or an admission rule of your own may rely on knowing every value: "not `Create`" must keep
meaning "creates nothing". So **adding a value to either one is a behaviour change**, not an
addition. It needs a new API version, or an opt-in field next to `mode` whose absence keeps
today's behaviour (for example a separate field that a claim sets to ask for an adoption mode);
it is never a new value of `mode` within v1. The difference from `provider`: a new provider
value only appears on objects written for that new cloud, which a client that does not know it
skips; a new `mode` would change what a client that reads an existing value understands about
the whole enum.

### Security fixes

One exception to "validation is not tightened": **a security fix may tighten validation within
v1**, when an object the API accepts today lets someone do what the
[threat model](security/threat-model.md) says they must not, and no looser fix closes the hole.
Such a change:

- is released as a patch or minor release under the [security policy](policy.md#security-fixes),
  and announced as a breaking change in its release notes and in the security advisory, naming
  the field and what is now refused;
- **keeps existing objects working**: an object stored before the fix is still accepted when it
  is written back with the tightened field unchanged, so an update that does not touch it, a
  status write and a delete never fail because of it. A webhook check is written to compare
  with the old object, and a schema rule relies on the API server's validation ratcheting,
  which does the same. Only a create, or an update that changes that field, is refused. 1.0 treats duplicate list entries at v1beta1 the same way
  ([below](#v1beta1));
- says in the notes what the operator does with such stored objects meanwhile — whether it keeps
  acting on them, or stops and reports it in a condition — and how to find them;
- when it tightens the CRD schema rather than a webhook, gets an explicit exception in
  `TestV1OnlyGrowsFromTheBaseline` that names the advisory, so the test keeps failing on every
  other tightening.

Anything else needs `v2`, served next to `v1` with a conversion between them, and `v1` then
stays served for at least two minor releases or six months after it is deprecated, whichever is
longer ([deprecation](policy.md#deprecation)).

`TestV1OnlyGrowsFromTheBaseline` (`api/v1/compat_test.go`) compares every generated CRD with the
v1 schema 1.0 ships and fails on a removed or renamed field, a changed type, a new required
field, a dropped enum value, a changed list type or key, a changed default, tightened limits or
a new validation rule. The round-trip tests in `api/v1beta1/conversion_test.go` fill every
field, status included, with random values and check that nothing is lost between v1beta1 and
v1 in either direction.

## v1beta1

`network.hypersurgery.dev/v1beta1` was the API from 0.8 to 0.9. From 1.0:

- **It is deprecated and still served.** `kubectl` prints the API server's warning on every
  request at v1beta1. Objects are stored at v1, and v1beta1 is converted to and from v1 by the
  operator's conversion webhook. The two versions have the same fields, so the conversion loses
  nothing; with the webhook off (`webhook.enabled=false`, or `webhook.conversion.enabled=false`)
  the API server converts on its own, with the same result.
- **It is served until at least 1.2, and at least six months after 1.0**, whichever is later.
  The release that stops serving it lists that as a breaking change.
- **Nothing is stored at v1beta1 after the first start of 1.0.** The operator rewrites every
  object at v1 and trims each CRD's `status.storedVersions` to `[v1]`, which the API server
  requires before a version can be removed from a CRD ([upgrades](operations/upgrades.md#upgrading-from-09-to-10)).
- **What changed from v1beta1 to v1**: nothing in the fields. v1 declares a few lists by what
  they are, which v1beta1 declared `atomic`: `spec.regions`, `spec.accounts[].regions` and
  `spec.requiredSubnetTags` of a `NetworkScope` are sets, and `spec.autoImport.accountDefaults`
  is a map keyed by `account`. Each entry can appear only once, at either version: the
  admission webhook refuses a duplicate at v1beta1 too. A `scopeRef` is 1 to 253 characters.

Move manifests to v1 by changing their `apiVersion`; `manager migrate-manifests` does it for a
whole repository and keeps everything else, comments included
([upgrades](operations/upgrades.md#manifests-in-git-2)).

## What a client should do

- Use `network.hypersurgery.dev/v1`.
- Treat `provider` and the other open sets above as open: skip or show what you do not know.
- Ignore unknown fields, and do not write an object back from a type that lacks them.
- For server-side apply, rely on the list keys the CRDs declare: `accounts` by `id`,
  `allocations` by `name`, `accountDefaults` by `account`, `conditions` by `type`.
