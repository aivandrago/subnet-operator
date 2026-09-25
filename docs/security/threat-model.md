# Threat model

What the operator protects, who it trusts for what, how each trust boundary can be abused,
what stops that today (with the file that does it), and what is left. It was written for the
1.0 security review (#62) against the code of v0.6.0 plus the fixes listed under
[Findings](#findings-of-the-10-review), and it is meant to be kept current: a change that
moves a boundary — a new identity, a new input, a new write — updates this file in the same
pull request.

Threats are sorted with STRIDE (**S**poofing, **T**ampering, **R**epudiation, **I**nformation
disclosure, **D**enial of service, **E**levation of privilege). Paths are relative to the
repository root.

## The system in one picture

```
 Kubernetes cluster
 ──────────────────────────────────────────────────────────────────────────────────────────
  cluster admins ──── NetworkScope, SheetExport ─┐
  namespace users ─── SubnetClaim, ResourceImport ┼──► kube-apiserver ──B5──► admission webhooks
  viewer's browser ─B6─► dashboard pod ───────────┘        ▲   (viewer's token)    (manager pod)
  Prometheus ──────B8─► metrics endpoint                   │
                                                  operator manager ──┬──B2──► spoke read roles
                                                                     ├──B3──► spoke write roles
                                                                     └──B7──► Google Sheets API
 ──────────────────────────────────────────────────────────────────────────────────────────
  EC2 API calls ─► CloudTrail ─► EventBridge (spoke bus → hub bus) ─► SQS ──B4──► operator
  release workflow ─► ghcr.io image + OCI chart (cosign keyless, SBOM, provenance) ──B9──► cluster
```

## Assets

| Asset | Why it matters |
|---|---|
| **The write roles** (`accounts[].writeRoleARN`) | Create subnets and tag VPCs and subnets in every account that has one. The most powerful thing the operator holds. |
| **The read roles** (`accounts[].roleARN`) and the operator's own AWS identity | Read the network layout of the whole organization; reach every spoke account through `sts:AssumeRole`. |
| **Integrity of tags in AWS** | Ownership (`hs/owner`), environment and the managed tag decide who is paged, who pays and, in organizations that use tag-based IAM conditions, who may do what. |
| **Integrity of the inventory** (`VPC`, `Subnet` objects, scope status, metrics) | Alerts, capacity planning and CIDR allocation (`SubnetClaim`) trust it. A wrong free-IP count or a hidden overlap leads to a wrong subnet. |
| **The audit trail** (`--audit-sink`, `internal/audit`) | The record of what the operator changed in AWS and on whose behalf ([docs/audit.md](../audit.md)). |
| **Confidentiality of the inventory** | Account IDs, CIDRs, resource IDs and tags are reconnaissance material. |
| **Viewers' Kubernetes tokens** | Pass through the dashboard on every request. |
| **The Google service account key** | Writes to every spreadsheet shared with that account. |
| **The webhook serving key** | Terminates the API server's admission calls. |
| **The release artifacts** | The image and chart everybody installs. |

## Identities

| Identity | Holds | Defined in |
|---|---|---|
| Operator ServiceAccount | Cluster-wide get/list/watch on the `aws.hypersurgery` CRDs and on namespaces (labels, for `namespaceSelector`), create on `resourceimports`, full control of `vpcs`/`subnets` and all status subresources, create/patch on `events.k8s.io` Events; Secrets **only** in its own namespace and the namespaces listed in `rbac.credentialSecretNamespaces` | `charts/aws-subnet-operator/templates/rbac.yaml`, kubebuilder markers in `internal/controller/*.go` |
| Operator AWS identity (IRSA / Pod Identity, hub account) | `ec2:Describe{Vpcs,Subnets,RouteTables}` in its own account, `sts:AssumeRole` on roles named `aws-subnet-operator-readonly` / `-write`, SQS receive/delete on the events queue | `deploy/iam/operator-policy.json` |
| Read role, per spoke account | The three `ec2:Describe*` calls only; trust: the operator role, optionally with `sts:ExternalId` | `deploy/iam/spoke-readonly-role.cfn.yaml` |
| Write role, per spoke account (opt-in) | `ec2:CreateSubnet`, `ec2:CreateTags`, `ec2:ModifySubnetAttribute`, `ec2:AssociateRouteTable`, each limited to VPC, subnet and route-table ARNs; no `Delete*` | `deploy/iam/spoke-write-role.cfn.yaml` |
| Dashboard pod | Nothing: `automountServiceAccountToken: false`, no RBAC. Acts only with the viewer's bearer token | `charts/aws-subnet-operator/templates/dashboard.yaml`, `internal/dashboard/proxy.go` |
| Google service account | Whatever spreadsheets were shared with it | Secret named by `SheetExport.spec.credentialsSecretRef` |
| Release workflow | GitHub OIDC token (`id-token: write`) used by cosign keyless; `packages: write` on ghcr.io | `.github/workflows/release.yml` |

## Trust boundaries and threats

### B1 — Kubernetes users → the operator (custom resources)

Who may create what is the first control. `NetworkScope` and `SheetExport` are cluster-scoped
and name AWS roles and credential Secrets: treat them as cluster-admin objects. `SubnetClaim`
and `ResourceImport` are namespaced and are how teams ask for writes; a scope's
`spec.namespaceSelector` says which namespaces may ask it (the RBAC model is in the
[chart README](../../charts/aws-subnet-operator/README.md#permissions)).

| | Threat | Mitigation today | Residual |
|---|---|---|---|
| E | A user who can create a `SubnetClaim`/`ResourceImport` in *any* namespace makes the operator write in *any* account a scope has a write role for — the operator acts as a confused deputy across tenants. | `NetworkScope.spec.namespaceSelector` (#69) names the namespaces that may refer to the scope. The validating webhooks refuse a claim or import from any other namespace (`validateNamespace` in `internal/webhook/v1alpha1/common.go`); the controllers refuse it in the status with `NamespaceNotAllowed` before reserving a CIDR or calling the `SubnetWriter`/`TagWriter`, for objects created while the webhooks were off (`namespaceRefusal` in `internal/controller/subnetclaim_controller.go`, used by both controllers; tests in `internal/controller/tenancy_test.go`); the auto-import policy only writes into a namespace the scope allows (webhook and `autoImportAllowed` in `internal/controller/autoimport.go`); a selector that does not parse allows no namespace (`AllowsNamespace` in `api/v1alpha1/networkscope_types.go`). Writes still need `--enable-writes` **and** a `writeRoleARN`; the webhooks refuse accounts the scope does not cover; no aggregated `edit`/`admin` roles are shipped. | For compatibility, a v1alpha1 scope **without** a selector still allows every namespace; the webhook warns and the scope gets a `NamespacesUnrestricted` Event. The v1 API allows none by default (ADR 0002). Namespace labels are only as trustworthy as the RBAC on `namespaces`: select by `kubernetes.io/metadata.name`, which the API server sets, or by labels tenants cannot write. Narrowing a selector stops new writes but leaves what existing objects already did in place. |
| T | An import overwrites a tag value somebody set (owner, environment, a tag an IAM condition keys on). `CreateTags` replaces the value of each key it names. | The auto-import policy never replaces a value the resource already carries (`internal/policy/autoimport.go`, fixed in this review); the webhook warns when a hand-written import replaces a value the inventory shows (`validateAgainstInventory`); the write role can only tag VPCs and subnets. | A hand-written import may still correct a value on purpose; only a warning stands in its way. |
| T | A claim or import points at a resource in another account than it names. | CRD patterns (`api/v1alpha1/*_types.go`: 12-digit accounts, `vpc-`/`subnet-` IDs); webhook compares the inventory's account/region with the spec; the role ARN's account must equal the target account (`credentialsFor` in `internal/cloud/aws/discoverer.go`), so a write can only ever happen in the account the object names. | — |
| T | Editing mirrored `VPC`/`Subnet` objects to mislead allocation. | They are outputs; the next sync overwrites them. README: give users read-only RBAC on them. | A user with write RBAC on `subnets` can hide a CIDR from the allocator until the next sync; `CreateSubnet` then fails with a conflict and the claim reallocates (`ErrCIDRConflict`). |
| R | The operator's audit line names whoever `spec.requestedBy` says, and that field is free text. | Since #70 the line also carries `created_by`: the user the API server authenticated, which the mutating webhook writes into the `aws.hypersurgery/created-by` annotation on CREATE (overwriting any value sent), and which the validating webhook requires to match the requesting user on CREATE and refuses to change on UPDATE (`stampCreatedBy`, `validateCreatedByOnCreate`, `validateCreatedByOnUpdate` in `internal/webhook/v1alpha1/common.go`). Imports written by the auto-import policy carry the operator's own identity. `principal` stays the free-text attribution and is documented as such ([docs/audit.md](../audit.md#what-created_by-means)). | With `webhook.enabled=false` nothing guards the annotation, so the audit line says `created_by: unknown`; the Kubernetes audit log is then the only record of who created an object. |
| D | Many objects or a huge scope slow the reconcilers. | One reconcile per controller at a time, bounded discovery concurrency, per-target backoff (`internal/controller/backoff.go`); limits in [docs/operations/limits.md](../operations/limits.md). | API-server quotas and RBAC are the real limit on object counts. |
| I | Tags mirrored into `Subnet`/`VPC` status are readable by anyone with read RBAC on them. | Documented in `SECURITY.md` ("put nothing secret in tags"). | By design. |

### B2 — Operator → AWS, read path

| | Threat | Mitigation today | Residual |
|---|---|---|---|
| S / E | A scope author points `roleARN` at a role in another account and reads it. | `credentialsFor` refuses a role whose ARN account differs from the declared account; the webhook refuses it at apply time (`validateAccount` in `networkscope_webhook.go`); only one account may go without a role. | Anyone who can create a `NetworkScope` can make the operator assume any role that trusts the operator role. That is why `NetworkScope` is an admin object. |
| S | Another party tricks a spoke role into trusting them (confused deputy at the AWS level). | Role trust names the operator role ARN; optional `sts:ExternalId` (`deploy/iam/spoke-*-role.cfn.yaml`), passed as `ExternalID` by the operator. | ExternalID is optional and shared by the read and write role of an account. |
| T | Discovery changes something. | Read role grants only `ec2:Describe*`, asserted by `test/deploy/iam_test.go`; `inventory.Discoverer` is read-only by contract and the reconciler never calls a writer. | — |
| I | Credentials leak through logs or status. | Credentials come from the SDK credential chain and are never logged; errors carry ARNs and error codes only. | Role ARNs and account IDs are in status and metrics by design. |
| D | Throttling in one account slows the whole instance. | Adaptive retryer per account/region, per-target backoff that does not hold a discovery slot while it waits (`internal/cloud/aws/discoverer.go`, `internal/controller/backoff.go`); measured in [limits](../operations/limits.md#measured-at-the-documented-capacity). | A throttled target holds its slot for the SDK's retries (about 15 s) once per backoff period. |

### B3 — Operator → AWS, write path

| | Threat | Mitigation today | Residual |
|---|---|---|---|
| E | The write role is used beyond subnet creation and tagging. | Separate role per account, off unless `--enable-writes`; the role's writes are limited to VPC, subnet and route-table ARNs (`deploy/iam/spoke-write-role.cfn.yaml`, tightened in this review, asserted by `test/deploy/iam_test.go`); the CRD only admits `vpc-`/`subnet-` IDs for imports. | Organizations that write their own role must keep the resource limits; nothing in the operator checks the role it is given. |
| T | A subnet is created twice or in the wrong place. | CIDRs are reserved in status before the call; a created ID is recorded even if a follow-up call fails (`createPending`); `vpcID`, account, region and scope are immutable on update. | — |
| T | Something is deleted. | No code path deletes an AWS resource or a tag; the write role has no `Delete*`. | — |
| T | The auto-import policy tags on the word of a forged creator (see B4). | Applies only in `mode: Apply`; creators come only from the events queue; existing tag values are never replaced. | See B4. |
| R | A write without a record. | Every allocation, creation, import and policy decision emits an audit line (`internal/audit`, [docs/audit.md](../audit.md)) with the authenticated `created_by`, plus an Event on the object; AWS CloudTrail records the `aws-subnet-operator` session name. | — |

### B4 — EventBridge → SQS → operator (untrusted input)

The queue body is parsed JSON from outside the cluster. It decides which targets are re-synced
early and, for `CreateVpc`/`CreateSubnet`, who is recorded as the creator — which the
auto-import policy may turn into tags.

| | Threat | Mitigation today | Residual |
|---|---|---|---|
| S / T | A forged message claims a resource was created by a principal of the attacker's choosing, steering `fromCreator` rules. | The queue policy admits only `events.amazonaws.com` for the one rule (`aws:SourceArn`, `deploy/events/hub-events.cfn.yaml`); EventBridge refuses `aws.*` sources from `PutEvents`, so organization members can forward real EC2 events but not forge them; only `source: aws.ec2` events without `errorCode` are read (`internal/events/events.go`). | Principals in the hub account with `sqs:SendMessage` through an identity policy bypass the queue policy. Creator attribution is keyed by resource ID only, not by account (`internal/controller/creators.go`). |
| D | A flood of messages. | Parsing is a single `json.Unmarshal` of a message SQS caps in size; unparsable messages are dropped and deleted (`internal/events/poller.go`); changes are debounced (`--events-debounce`, 10 s) and only targets of existing scopes are synced; the creator cache is bounded (512 entries, 24 h). | A steady forged stream can make every target sync every debounce interval: up to 4 EC2 calls per target per 10 s. |
| I | — | The queue is SSE-encrypted, including the dead-letter queue. | — |

### B5 — kube-apiserver → admission webhooks

| | Threat | Mitigation today | Residual |
|---|---|---|---|
| S | Someone impersonates the webhook and approves or rewrites objects. | TLS with a CA bundle in the webhook configurations, from cert-manager or a chart-signed certificate (`charts/aws-subnet-operator/templates/webhook.yaml`). cert-manager is the documented setup for production and GitOps (chart README, `values.yaml`, install guide). | **Open: #71** — without cert-manager, the chart-signed certificate is valid for 10 years, never rotated, its key is stored in every Helm release Secret of the namespace, and `helm template` (Argo CD, Flux with post-rendering) signs a new CA on every render. Accepted for installs that follow the recommendation; rotation is tracked in #71. |
| T | A webhook outage lets invalid objects in. | Validating webhooks are `failurePolicy: Fail` by default; mutating ones are `Ignore` but only fill defaults the controllers also apply, and the one thing they write that nothing else does — the created-by annotation — is checked again by the validating webhook; the controllers re-check what decides a write — the account is in the scope, the write role exists, the VPC is where the claim says — and report it in the status (`internal/webhook/v1alpha1/common.go`, package comment). | With `webhook.enabled=false` the only checks are the controllers'. |
| D | A slow webhook blocks writes of these CRDs. | `timeoutSeconds: 10`; the webhooks read through the manager's cache. | Scope and import validation list all `NetworkScope`s per request. |

### B6 — Browser → dashboard proxy → kube-apiserver

| | Threat | Mitigation today | Residual |
|---|---|---|---|
| E | The dashboard reads or writes with an identity of its own (confused deputy). | It has none: no token mounted, no RBAC; a request without a bearer token gets 401 (`internal/dashboard/proxy.go`, `test/chart/dashboard_test.go`). | — |
| E | A forwarded request reaches something other than this API group. | Allowlist: GETs under `/apis/aws.hypersurgery/<version>/…`, POST of `resourceimports` in a namespace, and POST of an empty `SelfSubjectReview` without query parameters (to show who is signed in; its body is checked field by field); escaped or non-canonical paths refused; headers rebuilt from an allowlist, so `Impersonate-*` and front-proxy headers never pass (`Allowed`, `checkSelfSubjectReview`, `rewrite`). | The query string of the aws.hypersurgery calls is forwarded unchanged (harmless for GET and a create; revisit if methods are added). |
| S | Cross-site request forgery through an SSO proxy that turns a cookie into a token. | Writes need `application/json`, the `X-Subnet-Dashboard` header (forces a CORS preflight that is never answered), a same-origin `Sec-Fetch-Site`/`Origin` (`checkWrite`). | Relies on browsers enforcing preflights and Fetch Metadata. |
| I | The viewer's token or inventory data leaks. | Strict CSP (`script-src 'self'`, `connect-src 'self'`), no CORS, `no-store` on API responses, no fonts from third parties in the cluster build (`internal/dashboard/server.go`); optional TLS in the pod; the token is never logged. | A token pasted into the app lives in the browser tab. Tags from AWS are rendered by the app (`site/assets/dashboard.js`), which relies on escaping plus the CSP. |
| D | Watches pin connections. | `ReadHeaderTimeout`, `MaxHeaderBytes`, body cap (`maxBodyBytes`), upstream timeouts (`cmd/dashboard/main.go`). | No per-client connection limit; put it behind the ingress's limits. |

### B7 — Operator → Google Sheets

| | Threat | Mitigation today | Residual |
|---|---|---|---|
| E | A crafted credentials file makes the Google client run a local command (`external_account` with `credential_source.executable`). | Only `type: service_account` keys are accepted (`internal/sheets/service.go`). | — |
| E | A `SheetExport` author reads another Secret. | The operator can read Secrets only in its own namespace and in `rbac.credentialSecretNamespaces` (`templates/rbac.yaml`). | A `SheetExport` author can make the operator use any Secret in those namespaces as a Google key — and export the inventory to any sheet shared with that account. `SheetExport` is an admin object. |
| T | Formula injection through tag values written to cells. | Values are written with `ValueInputOption("RAW")`, never interpreted as formulas. | — |
| I | The inventory goes to a spreadsheet with wider access than the cluster. | — | Whoever the sheet is shared with sees it; that is the feature. |

### B8 — Metrics

| | Threat | Mitigation today | Residual |
|---|---|---|---|
| I | Account IDs, CIDRs and resource IDs from the metrics endpoint. | HTTPS with Kubernetes authn/authz (`--metrics-secure`, `filters.WithAuthenticationAndAuthorization` in `cmd/main.go`), HTTP/2 off by default; optional NetworkPolicy. | `networkPolicy.enabled` is off by default. |

### B9 — Supply chain

| | Threat | Mitigation today | Residual |
|---|---|---|---|
| T | A tampered image or chart. | Built by `.github/workflows/release.yml` from the tag; image and chart signed keyless with cosign; the job verifies its own signature the way users are told to; buildx SBOM and `mode=max` provenance attached; actions pinned by commit SHA; distroless, non-root, read-only root filesystem (`Dockerfile.release`, `values.yaml`). | — |
| S | A signature made by another workflow of the repository passes verification. | The documented `--certificate-identity-regexp` now pins `.github/workflows/release.yml` (tightened in this review: the Scorecard workflow also holds an OIDC token and matched the old repository-wide pattern). | Any ref of `release.yml` can sign (tags and `workflow_dispatch`); branch protection on the GitHub mirror is the control. |
| T | A vulnerable dependency. | govulncheck on every change and weekly, CodeQL, OpenSSF Scorecard (`.github/workflows/security.yml`, `codeql.yml`, `scorecard.yml`). | The GitHub workflows run on the public mirror; the primary CI does not repeat them. |

## Findings of the 1.0 review

Reviewed by reading: `internal/cloud/aws/writer.go` and `discoverer.go`,
`internal/controller/subnetclaim_controller.go`, `resourceimport_controller.go`,
`autoimport.go`, `internal/policy`, `internal/webhook/v1alpha1`, `internal/events`,
`internal/dashboard/proxy.go` and `server.go`, `deploy/iam`, `deploy/events`, the chart's RBAC
and webhook templates, and the release workflow.

**Fixed with this review:**

1. **The write role could tag anything in the account.** `ec2:CreateTags`,
   `ModifySubnetAttribute` and `AssociateRouteTable` were allowed on `"*"`. Now each write is
   limited to the VPC, subnet and route-table ARNs it needs; `test/deploy/iam_test.go` fails
   on any write on `"*"` and on any write action the operator does not call, and checks that
   the read role only describes.
2. **The auto-import policy replaced tag values.** A creator rule, inheritance or an account
   default overwrote a value the resource already carried (say, `hs/owner` set by a person),
   although the policy is documented as only ever adding. Existing non-empty values now win
   and the decision's reason says which were kept (`internal/policy/autoimport.go`, test in
   `autoimport_test.go`).
3. **A hand-written import replaced values silently.** The webhook now warns, naming the key,
   the old and the new value (`resourceimport_webhook.go`, test in
   `resourceimport_webhook_test.go`).
4. **Signature verification accepted any workflow of the repository.** The documented
   identity and the release job's own check now pin `release.yml` (README, chart README and
   values, site).

**Filed for 1.0:**

- #69 — Restrict which namespaces may use a NetworkScope's write roles. *Fixed:*
  `spec.namespaceSelector`, enforced by the webhooks and the controllers (B1).
- #70 — Audit trail takes an import's principal from the user-supplied `requestedBy`. *Fixed:*
  the `created-by` annotation and the `created_by` audit field (B1).
- #71 — Self-signed webhook certificate lives 10 years and is never rotated. *Open;* cert-manager
  is documented as the production setup in the meantime (B5).

**Checked and found sound:** role-ARN/account binding for both roles; no delete path anywhere;
immutability of the fields that locate AWS resources; CIDR reservation before creation and
recording of partially created subnets; the dashboard proxy's allowlist, header rebuild,
bearer-only rule and CSRF checks; SQS parsing (bounded, dropped on error, never retried);
Sheets credential type check and `RAW` writes; Secret access limited to named namespaces.

## Residual risks, accepted for 1.0

- Anyone who can create a `NetworkScope` or `SheetExport` is effectively an administrator of
  the AWS roles and Google credentials those objects name. Grant them like cluster-admin.
- Hub-account principals with `sqs:SendMessage` can forge change events and creator
  attribution; keep the queue's identity-policy access as narrow as its resource policy.
- Tags are public to anyone who can read the inventory, in the cluster, in metrics and in the
  sheet.
- The operator trusts the roles it is given; a hand-written write role without resource limits
  gives the operator more than it needs.
- Throttled accounts cost each full sync about 15 s of one discovery slot per throttled target
  (see [limits](../operations/limits.md#measured-at-the-documented-capacity)).
- A `NetworkScope` without `namespaceSelector` can still be used from every namespace in
  v1alpha1 (warned about at apply time and by an Event). Set one on every scope that holds a
  write role.
- Without cert-manager, the webhook serving certificate is long-lived and never rotated (#71).

## Brief for an external reviewer

A lightweight review should take one or two days. The code is small (about 8,000 lines of Go
outside tests and generated code); the interesting parts are the boundaries above, not the volume. Please focus on:

1. **The write path end to end.** `SubnetClaim` → `SubnetClaimReconciler.reconcile` →
   `targetFor` → `Discoverer.CreateSubnet` → `credentialsFor`; `ResourceImport` →
   `ResourceImportReconciler.reconcile` → `ApplyTags`. Can any input make the operator write in
   an account, or to a resource, other than the one the object names? Can a write happen with
   `--enable-writes` off or without a `writeRoleARN`?
2. **The auto-import policy as an attack surface.** `internal/controller/autoimport.go`,
   `internal/policy/autoimport.go`, `internal/controller/creators.go`: what can somebody who
   can only create EC2 resources in a spoke account, or only send to the queue, make the
   operator tag?
3. **SQS input handling.** `internal/events/events.go` and `poller.go`: parsing of untrusted
   JSON, what a forged or oversized message can do, and whether `deploy/events/hub-events.cfn.yaml`
   is the right queue and bus policy.
4. **The dashboard proxy.** `internal/dashboard/proxy.go`: path allowlist (encoding, dot
   segments, alternate API paths, the one `SelfSubjectReview` exception), header allowlist, CSRF checks behind common SSO proxies
   (oauth2-proxy, nginx `auth_request`), and whether forwarding the query string unchanged is
   safe. And the app's rendering of AWS-controlled strings in `site/assets/dashboard.js`.
5. **Kubernetes RBAC and tenancy.** `charts/aws-subnet-operator/templates/rbac.yaml` and the
   generated `config/rbac`: is the operator's ClusterRole the least it needs, and is the
   namespace model (`namespaceSelector`, `internal/tenancy`) acceptable for a multi-team
   cluster? Can a claim or import reach a writer from a namespace the scope does not select,
   or be created with a `created-by` annotation that is not its creator?
6. **IAM.** `deploy/iam/*`: the trust policies, the optional ExternalID, and the resource
   limits of the write role.
7. **Admission webhooks.** Fail-open/closed behaviour, and the self-signed certificate (#71).
8. **Supply chain.** `.github/workflows/release.yml`: signing identity, SBOM and provenance,
   and the verification commands users are given.

Out of scope: AWS itself, Kubernetes itself, the Google API client, and denial of service by
cluster administrators.

Useful to run: `make test` (envtest with real kube-apiserver), `make test-e2e` (Kind + Moto,
including AssumeRole and SQS), `make test-scale` (400 targets). Report findings as described
in [SECURITY.md](../../SECURITY.md).
