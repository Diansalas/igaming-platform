# Gate 10.3-W2/W3 — Security review of W2b, W3a, W3b

- Reviewer: `security` specialist
- Date: 2026-09-26
- Branch: `claude/focused-wright-jw88w9`
- Reviewed: committed code at `becc5c2`, range `3ef18f2..becc5c2`, read with
  `git show` / `git diff` (the working tree was not read, because another agent
  is editing it)
  - W2b `a41acdf`
  - W3a `8a69412`
  - merge fix `a01f9b3`
  - runtime fix `7054e6e`
  - W3b `becc5c2`
- Mode: read-only. The only file written is this one. Nothing was committed.

## Verdict

**APPROVE WITH CONDITIONS.**

W2b, W3a, the merge fix and the runtime fix are approved with no blocking
finding.

W3b is approved **as delivered**. That means `awssm` is not wired into any
binary, and it is labelled accordingly. Two conditions must be closed before
`awssm` is wired into `cmd/platform-api` or into any staging or production
path:

- **S-1** (Medium): the static-credential refusal can be bypassed.
- **S-2** (Medium): the endpoint/CA refusal does not cover every way to redirect
  the client or its trust roots.

Neither finding is exploitable today, because nothing constructs `awssm.New`.
But the delivery claim "static credentials refused in every environment" is
**not true as implemented**. It must be corrected in the W3b delivery report and
in ADR 0093's status text until S-1 is fixed.

**Launch-blocking flags**:

- S-1 and S-2 block the **wiring** of `awssm`. They do not block this gate.
- S-3 (Go toolchain) should be resolved before any production build. See below.

## Evidence I ran

All runs used a clean `git archive becc5c2` extraction in the scratchpad, with
`GOPROXY=off` and no network.

- `go vet ./...` exited 0.
- `go test` passed (unit tests, no integration tag) for:
  - `./internal/secretstore/...`
  - `./internal/casino/`
  - `./internal/auth/`
  - `./internal/reconciliation/...`
  - `./internal/db/`
  - `./internal/httpserver/`
- `go mod tidy` was a no-op against the committed `go.mod`/`go.sum`, so the
  drift step should pass.
- I did **not** run integration tests or govulncheck. vuln.go.dev is blocked by
  egress. The CI run will be the first govulncheck evidence (see S-3).

---

## 1. Rejection write path (W2b)

These points hold:

**Verified-only (I1)**
- `wrapRejection` is applied in exactly three places, all in the dispatch
  switch of `ReceiveCallback` (`internal/casino/orchestrator.go`, around lines
  760–771). They are reached only after `verifyCallback` and the adapter's
  `HandleCallback` have both succeeded.
- `recordCasinoCallbackRejection` (`internal/httpserver/casino_callback_rejection.go:36-38`)
  writes only when it gets a `*casino.CallbackRejectedError`. For an
  `AuthError` or any other error it does nothing.
- The in-transaction writes (E3 in `postBet`, E9 distinct-reference in
  `postRollback`) are also post-verification.
- Tests cover both the tampered-body case and the signed-for-another-tenant case
  (`TestCasinoRejectionRecord_UnverifiedCallbackWritesZeroRows`,
  `TestCallbackRejection_UnverifiedCallbackNeverWrappedNorRecorded`).

**No forged tenant or cross-tenant write**
- `tenantID` is the route-resolved `t.ID` for the webhook, or `tc.TenantID`
  from the JWT for play simulation. It never comes from the body.
- The credential is resolved for `(tenant, provider)`, and the signature input
  binds the route tenant.
- The separate transaction is `WithTenant(tenantID)`. Migration 0097's
  `tenant_staff_insert` `WITH CHECK` pins `tenant_id = app.tenant_id` and
  excludes player-scoped connections.
- `ProviderID` is the route value, not the body value.

**Amplification is bounded**
- A row requires a sender that holds a valid tenant credential.
- There is one extra short transaction per *rejected verified* callback.
- The `ON CONFLICT DO NOTHING` key is
  `(tenant_id, provider_id, event_type, provider_tx_id, reason_class)`, so
  redelivery adds nothing.
- Play simulation can also record rows (player-authenticated). But it only works
  with the mock provider, which is `Synthetic` and refused by the production
  startup guard. Each row costs the player a real rejected action of their own.

**Idempotency**
- It is enforced by a DB `UNIQUE` constraint plus `ON CONFLICT`, not by
  check-then-insert.
- Concurrent identical deliveries collapse to one row, and a test covers this
  (`TestCallbackRejection_E10_ConcurrentIdenticalDeliveriesRecordOnce`).
- Including `reason_class` in the key is correct: one reference can
  legitimately be rejected for two different reasons over time.

**No PII or secrets**
- Row contents:
  - provider id
  - event type
  - provider refs
  - round id
  - asset
  - provider-asserted amount
  - class
  - request id
  - timestamp
- There is no player identifier, credential, key id or signature in the row.
- The failure log line (`casino_callback_rejection_record_failed`) is
  allow-listed and never echoes the reference, amount or error text. A test
  asserts this.
- `CallbackRejectedError.Error()` returns the original text unchanged, so the
  existing handler log lines are unchanged.

**RLS and append-only**
- `ENABLE` + `FORCE ROW LEVEL SECURITY` are set.
- The only policies are SELECT and INSERT. There is no FOR ALL policy.
- `ledger_deny_mutation` triggers are `BEFORE UPDATE OR DELETE` (row) and
  `BEFORE TRUNCATE` (statement). They bind the owner too.
- The guarded `REVOKE UPDATE, DELETE, TRUNCATE` from `igaming_runtime` is in
  place.
- A test proves the trigger binds even under a permissive UPDATE policy
  (`TestMigration0097_DenyTriggerBindsEvenWithAPermissiveUpdatePolicy`).
- The down migration refuses once rows exist.

### Findings

**R-1 (Low): evidence is lost when the caller disconnects.**
- Location: `internal/httpserver/casino_callback_rejection.go:40`.
- The separate write uses `r.Context()`. A provider with a short HTTP timeout,
  or anything that cancels the request context after the callback transaction
  rolls back, cancels the rejection write. The row is then lost and only a log
  line remains.
- `TestCasinoRejectionRecord_WriteFailureIsLoggedNotPropagated` shows this
  directly: a cancelled context gives no row.
- The rejection record is reconciliation evidence (the input to C6/C7), so
  losing rows under timeout pressure weakens detection.
- Fix: use `context.WithoutCancel(ctx)` with a short bounded timeout (a few
  seconds) for the separate write.
- Not blocking.

**R-2 (Low): text columns have no length bound, so large values can be lost.**
- Location: migration 0097, lines 29 onward (`provider_tx_id`,
  `original_provider_tx_id`, `round_id`, `asset_code`).
- A verified sender can put a reference of several KB (up to the 1 MiB body
  cap) into `provider_tx_id`. That exceeds the btree tuple limit (~2.7 KB) of
  the `UNIQUE` key and `idx_casino_callback_rejections_provider_tx`, so the
  INSERT fails. The rejection is then unrecordable: a logged failure, no row.
- The same unbounded `provider_tx_id` already exists on the ledger side, so this
  is a pre-existing platform gap, not new to W2b.
- Fix: register a follow-up for a platform-wide bound on provider reference
  length, validated at the adapter boundary after verification.
- Not blocking.

**R-3 (Info): `request_id` is caller-controlled.**
- `request_id` is the inbound `X-Request-Id` when it is 128 printable ASCII
  characters or fewer (`internal/httpserver/middleware.go:30`). It is not
  covered by the signature.
- It is correctly left out of the admin response (`casinoCallbackRejectionResponse`).
- Any future Back Office rendering must treat it as untrusted text, because
  printable ASCII includes `<>`.

---

## 2. Admin read endpoints (W2b/W3a)

These points hold:

**Routes and permission**
- All three routes use the `auth.Middleware → RequireTenantScope →
  RequirePermission(PermCasinoReconciliationRead)` chain
  (`internal/httpserver/casino_routes.go:56-61`).
- GET is the only verb. A test asserts POST returns 405.
- `casino_reconciliation:read` is granted to exactly `tenant_admin`, `finance`
  and `compliance`. It is not granted to `support`, `risk_manager`,
  `platform_admin` or `player`. A unit test pins this
  (`casino_reconciliation_permission_test.go`).
- An integration test checks the HTTP result: allowed roles get 200,
  support/risk_manager get 403, a player gets 403/401, and no token gets 401.

**Tenant scoping**
- The tenant comes from `tenant.FromContext` (verified JWT).
- Every query runs inside `WithTenant(tc.TenantID)` on FORCE-RLS tables
  (`casino_callback_rejections`, `reconciliation_runs`,
  `reconciliation_mismatches`).
- A cross-tenant test shows tenant B's admin sees zero of tenant A's rows on
  all three routes.

**Inputs**
- `?stream=` and `?status=` are closed allow-lists, and anything else gets 400.
- Pagination is clamped (limit ≤ 200).
- The kinds and streams arrays are bound parameters.

**No secret material**
- The responses contain no credential, key id, fingerprint or `request_id`.
- Mismatch evidence contains internal player-account UUIDs (C1 round-player
  check). These are pseudonymous and appropriate for finance/compliance within
  the tenant, the same as the existing sportsbook stream.

**Info**
- The read queries rely on RLS alone. There is no redundant
  `WHERE tenant_id = $1`. That matches platform convention and is correct under
  FORCE RLS with a NOBYPASSRLS role. An explicit predicate would add defence in
  depth if isolation later moves to schema-per-tenant; this is optional.

---

## 3. `deploy/init-app-role.sql` (merge fix a01f9b3)

These points hold:

- **Least privilege:** `REVOKE ALL` then `GRANT SELECT, INSERT` on
  `casino_callback_rejections` to `igaming_runtime`. There is no UPDATE,
  DELETE, TRUNCATE, REFERENCES or TRIGGER.
- **Effective result:** the migration path (default privileges S/I/U/D, then
  0097's `REVOKE UPDATE, DELETE, TRUNCATE`) also leaves exactly S/I. The two
  paths converge.
- **Idempotence:** the block is guarded by an `information_schema.tables`
  existence check, so it does nothing on a fresh initdb, and REVOKE/GRANT are
  safe to repeat. It sits *after* the blanket `GRANT ... ON ALL TABLES`
  (line 85), which is the order needed to re-narrow.
  `TestInitAppRole_RerunKeepsCasinoCallbackRejectionsGrants` covers it.
- **No widening elsewhere:** the diff is purely additive, 25 lines appended at
  the end of the file. No existing statement changed.
- **Permission union:** in the reviewed range, `permission.go` changes only in
  `a41acdf`, and those changes are the three additive grants described above.
  The merge introduced no other role or permission change.

**Info (doc drift):**
- `deploy/init-app-role.sql:183` says the statements are "EXACTLY migration
  0097's own grant: REVOKE ALL…". Migration 0097 actually does
  `REVOKE UPDATE, DELETE, TRUNCATE`. The effective privileges are the same, but
  the wording is inaccurate.
- `migrations/0097_...up.sql:115-120` still says the init-app-role change is
  "carried forward, not done here". It is now done.
- Both comments should be corrected at the next edit of those files.

---

## 4. `RunSweepTenants` (7054e6e)

These points hold:

- **No client path:** there is no non-test caller anywhere in the module. The
  only production sweep caller is `RunSchedulerLoop → RunSweep`
  (`internal/reconciliation/scheduler.go:567`). No HTTP route reaches either
  function.
- **Active tenants only:** `activeTenantIDs` filters `status = 'active'`, and
  when a list is given it also filters `id = ANY($1)`.
  - Suspended, closed and unknown ids are dropped.
  - Duplicates collapse.
  - `nil`/empty returns an empty outcome, never "all tenants", and is
    short-circuited before the query.
  - `TestSweepTenantSelection_ActiveOnlyScopedAndUnscoped` covers these cases.
- **Per-tenant body unchanged:** the per-tenant work is the shared
  `sweepTenants` body, so each stream still runs under its own `WithTenant` or
  `WithTenantSnapshot`.

**Condition on any future caller (not a finding today):** if an admin
"re-run reconciliation" route is ever added, it must pass only
`tc.TenantID` from the verified JWT (a single-element list). It must never pass
a list taken from the request, and it needs its own permission and audit
record.

---

## 5. `db.Pool.WithTenantSnapshot` (W3a)

These points hold:

- **Same scoping as `WithTenant`:** it matches `WithTenant` line for line, with
  one difference: `BeginTx(RepeatableRead)`.
  - It refuses a nil tenant.
  - It sets `set_config('app.tenant_id', $1, true)` (transaction-local and
    bound).
  - It sets nothing else: no role, no `app.player_account_id`, no
    platform-admin setting.
  - It uses the same pool, so the same runtime role.
- **No privilege change:** isolation level does not affect RLS evaluation.
- **Single caller:** `runCasinoStatementStreamForTenant`
  (`scheduler.go:482`). `RunCasinoStatement` refuses a non-REPEATABLE-READ
  transaction (`casino_statement.go:173`).

**Info (doc drift):** the `WithTenant` doc comment (`internal/db/tenant_rls.go:27`)
says it is "the single place that sets that value". That is no longer true.
Update it to name `WithTenantSnapshot`, so a future auditor grepping for
`app.tenant_id` writers finds both.

**Note (W3a MOCK):** `casino.MockStatementSource` carries the MOCK label and
the `SyntheticComponent` marker, so the production startup guard refuses a
binary that wires it. The statement stream against it is **MOCK**, meaning
tautological. Real casino statement matching is **PROVIDER DEPENDENT**.

---

## 6. W3b `awssm` (becc5c2)

These points hold:

- **Pinned version:**
  - `GetSecretValueInput` sets only `SecretId` and `VersionId`; `VersionStage`
    is never set.
  - `ParseRef` requires a 32–64 character `versionId` token.
  - `validateVersion` also refuses empty values and the
    `AWSCURRENT`/`AWSPENDING`/`AWSPREVIOUS` stage labels.
- **SDK logging off:** `WithClientLogMode(0)` and `o.ClientLogMode = 0`. The
  default smithy logger is Nop.
- **Error mapping:**
  - Every SDK error is mapped to a closed `secretstore.ErrorClass` through
    `NewError`. No SDK text, request id or body is attached.
  - `DecryptionFailure` maps to `access_denied`, so it does not trip the
    breaker.
  - Unknown client faults map to `store_config`.
  - Network and other failures map to `unavailable`.
  - `TestAWSSM_RedactionNoSecretInErrorsOrLogs` covers redaction.
- **Only in the constructor path:** `New` wraps the `LoadDefaultConfig` error
  with `%w`, which can carry a file path or profile name. That is not secret
  material and happens only at startup. It is acceptable.
- **Secret handling:**
  - There is a size cap.
  - The JSON fragment extraction never echoes the body.
  - Buffers are zeroed where Go allows. `SecretString` itself cannot be zeroed;
    that is accepted.
- **Import confinement:**
  - The AST-based `TestImportBoundary_NoOtherPackageImportsAWSSDK` has a
    negative control and an anti-string-match control.
  - A CI grep guard is at `ci.yml:133-138`.
- **Not wired:** nothing outside the package and its tests constructs
  `awssm.New`, and `cmd/platform-api` is untouched. The honest label is
  **IMPLEMENTED (unwired; STAGING REQUIRED)** / **PROVIDER DEPENDENT** on IAM
  (HD-10.3-2).

### Findings

**S-1 (Medium, blocks wiring): the static-credential refusal is incomplete.**
- Location: `internal/secretstore/awssm/awssm.go:138-143` (and `:165-190`).
- The refusal list is `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`,
  `AWS_SESSION_TOKEN`, `AWS_PROFILE`, plus shared file checks.
- The pinned SDK (`config@v1.28.6/env_config.go:24-28, 88-104`) also reads:
  - **`AWS_ACCESS_KEY`** and **`AWS_SECRET_KEY`** as aliases for static
    credentials (`credAccessEnvKeys`/`credSecretEnvKeys`);
  - **`AWS_DEFAULT_PROFILE`** as an alias for the profile.
- Failure scenario: an operator sets `AWS_ACCESS_KEY=AKIA…` and
  `AWS_SECRET_KEY=…` in a staging task definition. `New` accepts it, and the
  SDK authenticates with long-lived static keys. This is exactly what ADR 0093
  §6 condition 4 and the W2a review §4.1 require to be refused in every
  environment.
- Fix, preferred (allow-list): stop inferring safety from a deny-list.
  - Load with `WithSharedConfigFiles([]string{})` and
    `WithSharedCredentialsFiles([]string{})`.
  - Supply the credential provider explicitly: the ECS container provider
    (`AWS_CONTAINER_CREDENTIALS_RELATIVE_URI`), or whichever single source
    HD-10.3-2 authorizes.
  - Or, at minimum, `Retrieve` once at construction and refuse unless
    `Credentials.Source` is the authorized provider.
- Fix, minimum: add the three alias variables to the refusal list, with tests.

**S-2 (Medium, blocks wiring): the endpoint/CA refusal does not cover all
redirection or trust-root inputs.**
- Location: `internal/secretstore/awssm/awssm.go:147-151`.
- The list refuses `AWS_ENDPOINT_URL`, `AWS_ENDPOINT_URL_SECRETS_MANAGER` and
  `AWS_CA_BUNDLE`. These inputs are not refused:
  - **`SSL_CERT_FILE` / `SSL_CERT_DIR`**: Go's `crypto/x509` on Linux honours
    these for the system root pool. Together with **`HTTPS_PROXY`** (which the
    SDK's default HTTP client honours), they give exactly the TLS interception
    that refusing `AWS_CA_BUNDLE` is meant to prevent.
  - **Credential-source redirection:**
    - `AWS_CONTAINER_CREDENTIALS_FULL_URI` (+ `AWS_CONTAINER_AUTHORIZATION_TOKEN`)
    - `AWS_EC2_METADATA_SERVICE_ENDPOINT`
    - `AWS_WEB_IDENTITY_TOKEN_FILE` + `AWS_ROLE_ARN`
    - `AWS_ENDPOINT_URL_STS`
  - Any of these can hand the SDK credentials for an attacker's AWS account.
    Because `awssm://` refs accept a bare secret *name*, not only a full ARN
    (`secretstore.go:194`, `[^?#]+`), the name then resolves in the attacker's
    account. The attacker also controls the `VersionId`, because
    `ClientRequestToken` is caller-chosen.
- Impact is bounded by the backstop. The resolver checks namespace and the
  keyed fingerprint on every resolve (`providercred/resolver.go:200-221`), and
  the attacker cannot produce a value that matches the HMAC fingerprint without
  the real secret. So the outcome is **fail-closed**
  (`credential_integrity`, P1), not credential substitution.
- Why it is still Medium: the control exists specifically to refuse these
  inputs at startup, and as written it gives a false sense of coverage.
- Fix:
  - Refuse `SSL_CERT_FILE`, `SSL_CERT_DIR`, `AWS_CONTAINER_CREDENTIALS_FULL_URI`,
    `AWS_EC2_METADATA_SERVICE_ENDPOINT`, `AWS_ENDPOINT_URL_STS` and any
    `AWS_ENDPOINT_URL_*`. The S-1 allow-list approach mostly subsumes the
    credential items.
  - Decide in HD-10.3-2 whether IRSA/web-identity is an authorized source.
  - **Require `awssm://` refs to be full ARNs**, which pins the account, or
    check the account id of the resolved credentials against configuration at
    `New`.
  - Record the explicit decision on `HTTPS_PROXY` for the Secrets Manager
    client. The recommendation is to use the VPC endpoint and not honour the
    proxy for this client.

**S-3 (Medium, not blocking this gate; resolve before any production build):
the Go toolchain pin, and a likely failure on the first govulncheck run.**
- Location: `go.mod:3` (`go 1.25.0`, no `toolchain` line) and `ci.yml:71`
  (`go-version-file: go.mod`).
- CI (and this sandbox: `go version go1.25.0`) builds with **Go 1.25.0**, the
  first release of that line. Several later 1.25.x point releases carried
  standard-library security fixes (net/http, net/url, crypto/x509, crypto/tls
  and others). I cannot confirm the specific advisory IDs offline.
- Expected outcome: the new `govulncheck ./...` step will very likely **fail on
  its first run** on reachable stdlib vulnerabilities, not on the AWS SDK.
- The correct fix is to pin the latest patched toolchain (`toolchain go1.25.N`,
  or bump the `go` line). Do **not** suppress or allow-list the findings.
- The production builder (`deploy/docker/platform-api.Dockerfile:51`,
  `golang:1.25-alpine`) floats to the latest 1.25 patch. So the binary CI
  tests is built with a different toolchain than the one that ships. It is
  better to pin both to the same exact patch.
- Inference to confirm (architect / human): Go supports the two latest major
  releases, and the release cadence is February and August. If Go 1.27 shipped
  in August 2026, **Go 1.25 is now out of upstream security support**. Moving
  to a supported major should then be registered as a platform task before
  launch.
- **Resolved (GO-TOOLCHAIN-VULN-1, `docs/governance/task-registry.md`):** CI
  run #337's first `govulncheck` run confirmed the predicted stdlib findings
  (11 symbols across net/url, crypto/tls, net/http, encoding/xml,
  encoding/asn1, net/textproto, crypto/x509, x/net/idna, net, os) plus two
  module findings (`golang.org/x/text`, `go.opentelemetry.io/otel/sdk`). Go
  1.27 was confirmed to exist (`golang.org/toolchain` module list), so Go 1.25
  is out of support per this inference; moved to **go1.26.8** (not the latest
  1.25.x), pinned via `go.mod`'s `toolchain` line, and the same exact tag in
  `platform-api.Dockerfile`, closing the "different toolchain than the one
  that ships" gap this finding raised.

**S-4 (Low): govulncheck is installed unpinned.**
- Location: `ci.yml:192`.
- `go install golang.org/x/vuln/cmd/govulncheck@latest` means the gating tool
  changes underneath CI without review. The lint step deliberately pins
  golangci-lint "so results are reproducible; bump it deliberately"; this step
  does not follow the same rule.
- Checksum-DB verification limits the supply-chain risk. Pin a version anyway.
- **Resolved (GO-TOOLCHAIN-VULN-1):** `ci.yml` now installs
  `golang.org/x/vuln/cmd/govulncheck@v1.8.0` (exact version, requires go
  >= 1.26, satisfied by the go1.26.8 toolchain pin above).

**S-5 (Low): the import guard has small gaps.**
- Location: `import_boundary_test.go:18`, `ci.yml:137`.
- Both checks match only the prefix `github.com/aws/aws-sdk-go-v2/`. Neither
  catches:
  - `github.com/aws/smithy-go`, which `awssm` imports directly;
  - `github.com/aws/aws-sdk-go` (the v1 SDK).
- The CI grep is a string match, so it also flags comments. That fails safe.
- Fix: widen both checks to `github.com/aws/` so they cover the intent: "no
  other runtime AWS path".

**CI step integrity (item 6, "cannot be bypassed")**
- The drift, govulncheck and SDK-guard steps run on every push and PR to every
  branch.
- They have no `continue-on-error`.
- They are multi-line `run` blocks under the default `bash -e`, so any failing
  command fails the job.
- The drift step correctly runs `go mod verify` and a `go mod tidy` diff, and my
  offline run shows it is a no-op today.
- The remaining bypass is inherent: a PR can edit `.github/workflows/ci.yml`
  itself. That has to be closed by branch protection: required status check
  plus CODEOWNERS on `.github/`. I cannot verify repository settings from here;
  **recommend the orchestrator confirm them with the human.**

### SDK version ruling

These modules are pinned:

| Module | Version |
|---|---|
| `aws-sdk-go-v2` | v1.32.6 |
| `config` | v1.28.6 |
| `credentials` | v1.17.47 |
| `service/secretsmanager` | v1.34.7 |
| `service/sts` | v1.33.2 |
| `smithy-go` | v1.22.1 |

- These date from about December 2024.
- As far as I know (offline, no vuln DB access), there is **no Go vulnerability
  database advisory** against the aws-sdk-go-v2 core, config, credentials,
  secretsmanager or sts modules, or against smithy-go. The well-known AWS Go SDK
  advisories (the s3crypto issues) are in the **v1** SDK (`aws-sdk-go`), which
  is not a dependency.
- `go mod verify` integrity and the tidy state both pass.

**Ruling: no security-driven bump is required for this gate.** The versions are
about 21 months old, so bump them deliberately in a maintenance pass once the
first govulncheck run has set a baseline. Pin exact versions and re-run the
awssm tests. The authoritative evidence is the CI govulncheck run, and today I
have none. If that run reports a reachable finding in any of these modules, it
supersedes this ruling and blocks.

---

## 7. Other points

- **Carry-forward from W2a (not re-reviewed):** `awssm` introduces the SDK's
  own retry policy (default 3 attempts) underneath the `secretstore` breaker.
  When it is wired, confirm the breaker's timing assumptions account for SDK
  retries, so that one logical failure is not counted, or delayed, three times.
- **Mismatch evidence and future UI (Info):** mismatch `actual_value` strings
  and rejection `provider_tx_id`/`round_id` are provider-controlled text. Any
  Back Office UI must render them escaped. This is standard, but it is stated
  here because these are the first provider-authored strings on an admin read
  path in this stage.
- **The rejection record never touches money:** confirmed. The only write is
  the single INSERT, and no ledger, projection, round, session or capability
  row is written by `rejections.go` or by the HTTP helper.

## Required tests (security specification)

These tests already exist and must remain:

- unverified or cross-tenant-signed callback → zero rows;
- tenant B's admin → zero of tenant A's rows on all three routes;
- exact grantee set for `casino_reconciliation:read`;
- deny triggers bind under a permissive policy;
- init-app-role re-run leaves S/I only.

These must be added when the related conditions are closed:

1. **S-1:** `New` refuses when only `AWS_ACCESS_KEY` + `AWS_SECRET_KEY` are set,
   and when only `AWS_DEFAULT_PROFILE` is set. If the allow-list fix is taken, a
   test shows a non-authorized `Credentials.Source` is refused.
2. **S-2:** `New` refuses each of `SSL_CERT_FILE`, `SSL_CERT_DIR`,
   `AWS_CONTAINER_CREDENTIALS_FULL_URI`, `AWS_EC2_METADATA_SERVICE_ENDPOINT`
   and `AWS_ENDPOINT_URL_STS`. `ParseRef` refuses an `awssm://` ref that is not
   a full ARN, if that option is chosen.
3. **R-1:** a rejection write still commits when the request context is
   cancelled right after the callback transaction rolls back.

## Scope statement

**In scope:** code-level and design-level review of the committed diff
`3ef18f2..becc5c2` for items 1–7 of the brief, plus reading the pinned SDK
source in the local module cache.

**Not in scope:**
- live AWS behaviour;
- the IAM/KMS architecture (HD-10.3-2);
- running govulncheck (egress blocked);
- integration-test execution;
- repository branch-protection settings;
- W2a code that was already reviewed in `09-gate-w2-review-security-w2a.md`.

Passing this review does not make any component "secure" in general. `awssm`
in particular must be reviewed again at the point it is wired.
