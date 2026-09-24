# Contributing

Thanks for looking. This is a young project, so the fastest way to help is to run it against
your own accounts and say what broke or what was confusing.

## What is most useful right now

- **Running it.** The operator has been exercised against [Moto](https://github.com/getmoto/moto)
  in CI, not against a real AWS organization. Reports from a real one — especially with many
  accounts, unusual tagging conventions or a strict Terraform setup — are worth more than code.
- **Feedback on the API** before `v1alpha1` freezes: the tag schema, `SubnetClaim`,
  `ResourceImport` and the auto-import policy.
- **The Google Cloud and Azure providers.** The inventory model is already cloud-neutral; the
  discovery side is not written.

## Opening an issue

Say what you expected and what happened. For anything about discovery, the output of
`kubectl get networkscope <name> -o yaml` (redact account IDs if you like — the shape matters,
not the numbers) answers most questions on its own. For a suspected wrong CIDR allocation,
include the VPC's CIDRs and the subnets that already exist in it.

## Changes

1. Open an issue first for anything that changes the API or adds a CRD. Everything else can go
   straight to a pull request.
2. `make test` runs unit tests and envtest; `make lint` runs golangci-lint with the
   Kubernetes API linter; `make test-e2e` runs the suite in Kind against Moto and needs Docker.
   All three run in CI on every pull request.
3. Changes to `api/` need `make manifests generate`, and `make helm-crds` so the chart's copies
   of the CRDs match. CI fails if they drift.
4. Commit messages: a short imperative subject, then why the change exists, not what the diff
   already shows. English.

## Conventions worth knowing

- **Tags are the source of truth, not the CRDs.** Anything that would turn a CRD into a second
  inventory people have to maintain by hand is the wrong direction.
- **The operator never deletes a cloud resource,** and it never writes at all unless
  `--enable-writes` is set and a separate write role exists. Keep it that way: read paths and
  write paths use different IAM roles on purpose.
- **Degrade honestly.** An account that cannot be reached is reported, not erased; the last
  known state stays, and status conditions say why.
- New behaviour comes with a test that fails without it. Tests are also documentation — name
  them after the behaviour, not the function.

## Security

Please do not open a public issue for a vulnerability. See [SECURITY.md](SECURITY.md).

## License and sign-off

The project is Apache-2.0. Contributions are accepted under the same license, with a
[Developer Certificate of Origin](https://developercertificate.org/) sign-off:

```sh
git commit -s -m "..."
```

That line certifies you wrote the change or have the right to submit it. Add yourself to
[CONTRIBUTORS.md](CONTRIBUTORS.md) in the same pull request.
