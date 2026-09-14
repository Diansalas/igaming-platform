# Project Progress

Last updated: 2026-09-14 (Stage 3D)

## Status: Stage 3D (Withdrawal Governance Final Gate) — complete, pending human approval to start Stage 4

## Stage 0 — complete (approved)

Governance (`CLAUDE.md`, `MASTER-BUILD-PROMPT.md`), 17 specialist agents,
full Stage 0 requirements/architecture/risk inventory under `docs/`. See
git history for the Stage 0 commit and the Stage 0 completion report.

Six business decisions from Blueprint §10 were resolved by the human at
the Stage 0→1 gate (hybrid licensing, multi-wallet/multi-asset model,
Europe+LATAM jurisdiction-aware design, institutional crypto custody,
confirmed B2C-first plan, hyperscale cloud hosting) — recorded in
`docs/decisions/0005` through `0009`, with `docs/architecture/06`, `07`,
and the new `15-jurisdiction-and-licensing-model.md` updated accordingly.

## Stage 1 — complete

### Delivered (`IMPLEMENTED`, verified by real tests against a live Postgres)

- Go module foundation, single deployable `platform-api`
  (`docs/decisions/0010-stage1-single-service-foundation.md`).
- `internal/config`: env-based configuration with fail-fast validation.
- `internal/observability`: structured JSON logging (correctly carrying
  request id AND tenant id per log line, across middleware boundaries),
  OpenTelemetry tracing + metrics (stdout exporter).
- `internal/db`: pgx connection pool; a from-scratch, reversible
  (up/down), advisory-lock-guarded migration runner
  (`internal/db/migrate.go`, `cmd/migrate`); the tenant-scoped
  query pattern (`WithTenant`/`WithoutTenant`) that is the platform's
  actual row-level-security enforcement mechanism; a startup check that
  refuses to connect as a superuser or BYPASSRLS role.
- `internal/auth`: JWT issuance/verification (HMAC — explicitly
  `PROVIDER DEPENDENT`/foundation-only), tenant-context middleware, RBAC
  skeleton (`RequireRole`).
- `internal/tenant`: the tenant context type, populated only from a
  verified JWT.
- `internal/httpserver`: request routing, middleware chain, health/
  readiness endpoints, and one demonstration endpoint
  (`GET /v1/tenant-config`) proving the full auth → tenant-context →
  RLS chain end to end.
- `internal/eventbus`: `STUB` in-memory Publisher/Subscriber — no broker
  deployed (deliberate; see `docs/decisions/0003`'s Stage 1 validation
  section).
- Migrations 0001–0006: tenants, jurisdictions/licences/
  tenant_jurisdiction_configs (RLS-protected), assets registry (with
  per-asset decimal exponent and, since migration 0006, a `network`
  column for crypto), tenant_config (the RLS reference implementation).
- CI (GitHub Actions): non-superuser Postgres role provisioning, gofmt/
  vet/lint/build, unit tests, integration tests against a real Postgres
  service container, migration reversibility check.
- OpenAPI foundation spec (`docs/api/openapi/platform-api.yaml`),
  validated.
- Full local dev workflow (`Makefile`, `deploy/docker-compose.dev.yml`,
  `deploy/init-app-role.sql`, `.env.example`, `docs/runbooks/README.md`).

### Specialist review pass

`architect`, `security`, `qa`, `devops`, and `code-reviewer` each
independently reviewed the Stage 1 diff. Findings and how each was
resolved are in the Stage 1 completion report (see conversation history)
and reflected in the fixes below. Two findings were confirmed
independently by multiple reviewers via direct reproduction against a
live database, not just static reading — both fixed:

- **Blocking, fixed**: the original CI/dev-compose Postgres configuration
  connected as the cluster's bootstrap superuser, which bypasses
  row-level security entirely regardless of `FORCE ROW LEVEL SECURITY`.
  Fixed with a dedicated non-superuser, non-`BYPASSRLS` application role
  (`deploy/init-app-role.sql`, CI role-provisioning step) plus a
  connection-time check in `internal/db/db.go` that refuses to proceed as
  a privileged role, plus a standing regression test
  (`TestConnection_IsNotPrivileged`).
- **Blocking, fixed**: a Postgres custom-GUC lifecycle quirk meant
  `current_setting('app.tenant_id', true)` could return `''` (not `NULL`)
  on a reused pooled connection, causing a `uuid` cast error instead of a
  clean RLS denial. Fixed with `NULLIF(...,'')` in every RLS policy
  (migrations 0004, 0005); test assertions tightened to check the
  specific Postgres SQLSTATE (`42501`) rather than "any error."
- **Blocking, fixed**: `tenant_jurisdiction_configs` had a `tenant_id`
  column but no RLS policy, in violation of `CLAUDE.md`'s absolute
  multi-tenancy rule. Fixed via migration 0005.
- Several `should-fix`/`nice-to-have` items also addressed: JWT now
  requires an `exp` claim; JWT issuer decoupled from the OTel service-name
  config; migration runner gained an advisory lock (concurrent-run safety)
  and now orders rollbacks by `applied_at`; `steps` is validated positive;
  added unit tests for `RequireRole`/`Middleware`, `apierror`'s status
  mapping, and the migration file-pairing logic; Bearer scheme matching is
  now case-insensitive with a `WWW-Authenticate` header on 401;
  `X-Request-Id` is now length/charset-validated before being trusted;
  `eventbus.Event` gained an `EventID` and consistent `uuid.UUID` typing.
- Documentation drift caught and corrected: `docs/architecture/06` and
  ADR 0007 had overstated that Stage 1 delivers `Wallet` identity
  tables (it doesn't — only the `Asset` registry); `docs/architecture/03`
  claimed forward-only migrations while the tool built is reversible
  (doc corrected to describe actual practice: reversible up/down pairs,
  applied forward-only in staging/production); a new
  `docs/decisions/0010-stage1-single-service-foundation.md` documents the
  single-deployable choice that existed in code but nowhere in `docs/`.
- One `should-fix` explicitly deferred, not silently dropped: no
  cross-table constraint ties `tenants.licensing_model` to
  `licences.licensee` yet — recorded as a known gap in
  `docs/architecture/15-jurisdiction-and-licensing-model.md`, to be
  enforced by the Stage 2 tenant-config service rather than a database
  trigger.

### Explicitly NOT built in Stage 1 (by design)

Full wallet/ledger, real payment/casino/sportsbook provider integrations,
bonus engine, production KYC/AML, B2C frontend, back office, partner
console, production deployment, live-money operations. `PaymentProvider`/
`CryptoCustodyProvider` interfaces are documented
(`docs/architecture/07`) but not yet implemented in code — that begins
when their owning stage (3–4) does.

## Stage 2 — implementation and local verification complete

### Delivered (`IMPLEMENTED`, verified by real tests against a live Postgres)

- **Migrations 0007–0014** (all reversible, round-tripped up→down→up
  cleanly): tenant slug + a database-enforced
  `tenants.licensing_model`/`licences.licensee` consistency constraint
  (composite FK via a generated `expected_licensee` column — closes the
  Stage 1-flagged gap, see `docs/architecture/15`); `brands` (replaces
  `tenant_config`, public-read + tenant-scoped-write RLS,
  `docs/decisions/0012`); `persons` (platform-wide, no RLS); RLS-protected
  `player_accounts`, `staff_users` (dual-scope RLS), `sessions` (as
  originally shipped in Stage 2: public SELECT by token possession +
  tenant-scoped INSERT/UPDATE — **superseded by the pre-Stage-3 security
  hardening pass below, which closed this exposure**; see
  `docs/decisions/0016`), `login_attempts` (dual-scope RLS),
  `audit_log` (dual-scope RLS + an unconditional `BEFORE UPDATE OR DELETE`
  trigger enforcing append-only, even against the application's own
  non-superuser database role).
- `internal/audit`: `Record(ctx, tx, Entry)` — writes atomically inside
  the caller's transaction, never a separate/best-effort log.
- `internal/auth` rewrite: Argon2id password hashing
  (`password.go`); `KeyRegistry` for `kid`-based key rotation
  (`keys.go`); JWT reissued with `kid`/issuer/audience validation and a
  legitimately-nilable `tenant_id` claim for platform-scoped principals
  (`jwt.go`, `docs/decisions/0011`); single-use rotating refresh tokens
  with reuse detection that revokes the full session chain
  (`session.go`); permission-based RBAC (`permission.go`) replacing
  Stage 1's role-list `RequireRole` (deleted); `RequireTenantScope`
  middleware enforcing "must have a real tenant" per-route rather than
  at token verification.
- `internal/identity`: `Brand`, `Tenant`, `Person`, `PlayerAccount`,
  `StaffUser`, `login_attempt` (Postgres-backed lockout, 5 failures / 15
  min window).
- `internal/httpserver`: player auth (register/login/refresh/logout),
  player self-service (`/v1/me`, session listing/revocation), staff auth,
  platform-admin tenant/brand/staff provisioning
  (`docs/decisions/0011`'s `canActOnTenant` rule), tenant-scoped player
  administration (list/get/suspend), tenant-scoped audit-log read.
- `cmd/seed-admin`: CLI bootstrapping the first `platform_admin`, avoiding
  the chicken-and-egg problem of needing an admin to create the first
  admin (`docs/decisions/0014`).
- `docs/api/openapi/platform-api.yaml` rewritten for all 15 new Stage 2
  endpoints (17 paths total); validated (parses, every `$ref` resolves).
- Four new ADRs: `0011` (platform-scoped identity tokens), `0012` (Brand
  distinct from Tenant), `0013` (audit-log immutability + dual-scope
  RLS), `0014` (service-identity pattern for platform CLIs).
- `docs/architecture/16-privacy.md`: sensitive-field inventory, access
  controls, audit requirements, data-minimization notes, and a retention
  posture that deliberately defers to future jurisdiction-dependent
  configuration rather than inventing a legal retention period.
  `docs/architecture/05` and `15` updated with Stage 2
  implementation-status sections.

### Verification performed (all against a real local PostgreSQL 16, not mocked)

- `gofmt`, `go build` (including `-tags=integration`), `go vet`
  (including `-tags=integration`), `golangci-lint run ./...`: all clean,
  0 issues.
- Unit test suite: all passing (`internal/auth`'s password/permission/JWT/
  middleware tests, `internal/apierror`, `internal/config`, migration
  file-pairing logic, etc.).
- Integration test suite (build tag `integration`, real Postgres): all
  passing —
  - `internal/db/tenant_rls_integration_test.go`: general RLS proof,
    repointed from the now-dropped `tenant_config` table to
    `tenant_jurisdiction_configs` (same single-scope RLS pattern) after
    migration 0008 removed `tenant_config`.
  - `internal/audit/audit_integration_test.go`: tenant-scoped vs.
    platform-level entry visibility, system-actor validation, and the
    load-bearing immutability proof (`UPDATE`/`DELETE` against
    `audit_log` fail with a real Postgres error even for the owning
    role).
  - `internal/identity/identity_integration_test.go`: brand public-read/
    cross-tenant-write-denied, player registration + duplicate-email
    rejection + cross-tenant-read-denied, staff-user platform-vs-tenant
    visibility, login lockout threshold/reset, and the licensing-model/
    licence consistency constraint.
  - `internal/httpserver/identity_flow_integration_test.go`: full
    HTTP-level flows — player lifecycle end-to-end, login lockout,
    staff login for both `platform_admin` (no `tenant_slug`) and
    tenant-scoped roles, RBAC role-distinction enforcement (`support`
    denied on suspend, `tenant_admin` allowed), cross-tenant player
    access denied, tenant+brand creation restricted to `platform_admin`.
- All 8 new migrations (0007–0014) applied, fully rolled back, and
  re-applied cleanly. One real bug was found and fixed during this
  round-trip: migration 0008's down script tried to backfill
  `tenant_config` data via `INSERT` *after* enabling
  `FORCE ROW LEVEL SECURITY` on it, so its own insert failed the `WITH
  CHECK` policy (the migration runner connects without `app.tenant_id`
  set, by design — it is not a superuser/`BYPASSRLS` role). Fixed by
  reordering: backfill first, enable+force RLS second.
- Manual end-to-end smoke testing against the live `platform-api` binary
  (prior to writing the formal test suite, later formalized into the
  tests above): staff/platform-admin login and tenant/brand provisioning;
  player registration with duplicate rejection; `GET /v1/me`; refresh
  rotation; refresh-reuse detection revoking the full session chain;
  `tenant_admin` vs. `support` RBAC distinction; `platform_admin`
  correctly denied on tenant-scoped endpoints; login lockout after 5
  failures including "correct password, still locked out."

### Specialist review pass

`architect`, `identity-compliance`, `security`, `backend`, `qa`, and
`code-reviewer` each independently reviewed the Stage 2 diff. Full
itemized findings are in the Stage 2 completion report; summary of what
was found and fixed:

- **Blocking, fixed**: migration `0008`'s *up* script (not just its
  down script — see below) enabled `FORCE ROW LEVEL SECURITY` on `brands`
  and read `tenant_config` (which has its own RLS) before backfilling
  data between them, so on any real upgrade with existing `tenant_config`
  rows the backfill silently copied zero rows before dropping the source
  table — reproduced empirically by `code-reviewer` with seeded data.
  Fixed by disabling `tenant_config`'s RLS immediately before the
  backfill (it's dropped moments later anyway) and moving `brands`' own
  RLS enablement to after the backfill, mirroring the down script's
  already-fixed ordering. Re-verified by seeding a real `tenant_config`
  row, running the migration, and confirming the row survived as a
  `brands` row.
- **Blocking, fixed**: `persons` (migration `0009`) had no RLS at all,
  reasoned about as "platform-level like `jurisdictions`/`assets`" — but
  unlike those, it's mutated by unauthenticated registration inside a
  tenant-scoped transaction, so any tenant could read/modify another
  tenant's `persons` rows, including the platform-level self-exclusion
  status this table exists to protect. Fixed via migration `0015` (RLS:
  unconditional `INSERT`, platform-scope-only `SELECT`/`UPDATE`/`DELETE`)
  — see `docs/decisions/0015-persons-platform-scope-access-control.md`.
- **Blocking, fixed**: `POST /v1/admin/tenants` created its tenant and
  wrote its audit record in two *separate* transactions (only `CreateBrand`/
  `CreateStaffUser` correctly did this atomically), so a failed audit
  write was only logged while the handler still returned `201` — directly
  contradicting ADR 0013's atomicity claim, caught independently by both
  `architect` and `backend`. Fixed by giving `identity.CreateTenant` the
  same `pgx.Tx`-based signature the other two already used.
- **Blocking, fixed**: staff login's lockout identifier was built from the
  *raw* request email while the actual account lookup normalized it
  (lowercase + trim), so case/whitespace variants of the same address each
  got a fresh `login_attempts` bucket — lockout never tripped against a
  `platform_admin` account attacked this way. Fixed by normalizing once,
  up front, and using that value everywhere; regression test
  `TestStaffLogin_LockoutHoldsAcrossEmailCaseVariants` added.
- **Blocking, fixed**: `audit_log`'s immutability trigger was `BEFORE
  UPDATE OR DELETE FOR EACH ROW` only — Postgres row-level triggers never
  fire on `TRUNCATE`, so the same non-superuser application role that
  owns the table could have erased the entire trail with one statement.
  Fixed via migration `0016` (a statement-level `BEFORE TRUNCATE`
  trigger reusing the same deny function); manually verified `TRUNCATE
  audit_log` now raises.
- **Blocking, fixed**: `RotateSession`'s concurrent-refresh re-check read
  `replaced_by_session_id`/`revoked_at` without a row lock under READ
  COMMITTED, so two simultaneous refreshes of the same stolen token could
  both succeed, producing two live chains with reuse detection never
  triggering — flagged independently by both `architect` and
  `code-reviewer`. Fixed by adding `FOR UPDATE` to the re-check;
  regression test `TestRefreshRotation_ConcurrentRequestsRaceSafely`
  (8-way concurrent refresh of one token, exactly 1 success) added.
- **Should-fix, fixed**: `identity.CreateTenant`/`CreateBrand` didn't map
  a unique-slug-violation to a domain error, so a duplicate slug returned
  `500` instead of `409` (inconsistent with `RegisterPlayer`/
  `CreateStaffUser`'s existing `ErrEmailTaken` pattern). Fixed with a new
  `ErrSlugTaken`, wired into both handlers and the OpenAPI spec.
- **Should-fix, fixed**: login responses were correctly indistinguishable
  by status code between "unknown email" and "wrong password", but the
  unknown-email branch returned before the Argon2 check, creating a
  timing side-channel that could reveal account existence. Fixed with
  `auth.DummyPasswordHash` — a fixed, valid hash with no real account —
  verified against on the miss path so both branches pay the same cost.
- **Should-fix, fixed**: refresh-token reuse (the platform's strongest
  credential-theft signal) was only `logger.Warn`'d, landing solely in a
  mutable application log despite ADR 0013's explicit stance against
  that. Fixed by writing an `auth.session_reuse_detected` audit record
  atomically with the chain revocation.
- **Should-fix, fixed**: the login-failure audit entry for an email with
  no matching account embedded that raw email in `metadata`, logging a
  non-user's address — narrowed to drop it, documented in
  `docs/architecture/16-privacy.md`.
- **Should-fix, fixed**: `0007`'s `tenants.expected_licensee` `CASE`
  expression had no `ELSE`, and a `NULL` referencing column silently
  satisfies a composite FK — currently unreachable (the column-level
  `CHECK` on `licensing_model` prevents a third value), but a future
  migration adding one without updating the `CASE` would silently stop
  enforcing. Closed with a defense-in-depth `CHECK` via migration `0017`.
- **Should-fix, fixed**: the ADR-0013-documented `staff.login_failed`
  scope-mismatch branch skipped `recordAttempt`, unlike every sibling
  failure branch (currently unreachable given RLS, but inconsistent).
- **Test-coverage gaps, closed**: `qa` found `GET /v1/me/sessions`/
  `DELETE /v1/me/sessions/{id}` (including the session-ownership IDOR
  guard) and every `audit.Record` call site had zero test coverage, and
  no test exercised the refresh-rotation concurrency race. Closed with
  `TestSessionManagement_ListAndRevoke`, `TestAuditLog_RecordsSecurityEvents`,
  and `TestRefreshRotation_ConcurrentRequestsRaceSafely`; two integration
  assertions tightened to specific Postgres SQLSTATEs
  (`identity_integration_test.go`).
- **Documented at the time, since CLOSED**: `sessions`' public-read RLS
  policy (necessary — a refresh token carries no tenant hint to scope a
  lookup by) gave any tenant-scoped connection read access to every
  tenant's session metadata (IPs, user agents), safe at the time only
  because `ListActiveSessions`/`RevokeSession` filtered by `principal_id`
  in application code. `security` and `code-reviewer` both flagged this
  as should-fix, not blocking, and it was documented as accepted
  technical debt in ADR 0013 rather than silently left unmentioned — **this
  was fixed in the pre-Stage-3 security hardening pass; see the section
  below and `docs/decisions/0016`.** `architect` also flagged `brands`'
  public-read policy as broader than strictly needed (cross-tenant brand
  enumeration) — that one remains open debt, not fixed by the hardening
  pass either (out of its scope).
- No specialist found scope creep against the Stage 2 DO-NOT-BUILD list,
  no fake-completion claims, and no contradiction between the new ADRs
  and Stage 0/1 decisions.

### Explicitly NOT built in Stage 2 (by design, per the Stage 2 DO-NOT-BUILD list)

Wallet/ledger, deposits, withdrawals, real payment providers, casino
providers, sportsbook, bonus engine, production KYC provider, production
AML provider, complete responsible-gaming engine, complete B2C frontend,
Partner Console. `kyc_tier`/`person_key_hash`/`verified_at` are hooks
only — unpopulated and uninterpreted by any Stage 2 code.

## Security Hardening Pass (pre-Stage-3) — complete

Human-directed hardening pass addressing the Stage 2 completion report's
flagged debt, before authorizing Stage 3. Explicitly NOT Stage 3 — no
wallet/ledger/payments/casino/sportsbook/bonus/KYC/frontend/back-office
work.

### 1. Sessions RLS — closed

`sessions`' `FOR SELECT USING (true)` policy (Stage 2's accepted debt,
above) is replaced by three narrower policies (migration `0018`):
exact-token-hash match for the pre-auth lookup, tenant+principal match
for self-service session listing/revocation, and a tenant-scoped
internal-operation-id match for system code that already resolved a
specific row and needs to touch it without a caller-asserted principal.
Two new `internal/db.Pool` methods (`WithSessionLookup`,
`WithPrincipalScope`) and one helper (`SetSessionInternalOpID`) set the
corresponding Postgres session variables, mirroring `WithTenant`'s
existing `set_config(..., true)` pattern — no new database role, no
`BYPASSRLS`, no elevated privilege. Full design and the alternatives
considered/rejected: `docs/decisions/0016-sessions-rls-hardening.md`.

A follow-up `security`/`code-reviewer` pass on this exact change (before
treating it as final) found and this session fixed:

- **Blocking**: `revokeChainFrom` (the refresh-token-reuse chain-
  revocation walk) stopped early at the first already-revoked mid-chain
  node, silently leaving every session further down the chain live
  despite a confirmed theft signal. Fixed (unconditional `COALESCE`-based
  revoke so the walk always learns where the chain continues) and
  regression-tested with a 4-hop chain, one node pre-revoked.
- **Should-fix**: the chain-link `UPDATE` that marks a rotated-out
  session's successor discarded its result, so a 0-row outcome (should be
  unreachable, but silently possible had the SELECT-visibility fix above
  been missing/wrong) would mint an unlinked session with reuse detection
  defeated for that token. Fixed: `RowsAffected` is now checked, loud
  error on zero.
- **Should-fix**: the refresh-rotation concurrency race-loser branch
  produced no audit record. Fixed: writes `auth.refresh_rotation_race_lost`
  (deliberately does not also revoke the winner's chain — see the ADR for
  the reasoning and "Remaining security debt" below).
- **Should-fix**: the internal-op-id SELECT policy had no tenant
  conjunct (unexploitable today, a one-line future footgun). Fixed.
- **Should-fix**: GUC-setting SQL was duplicated inline in
  `internal/auth` instead of centralized in `internal/db`. Fixed.
- **Test gap, closed**: added direct-SQL (not HTTP-level) proofs that a
  cross-principal *write* (not just read) is denied at the database
  layer, that the internal-op-id scope sees only its one named row, and
  that none of the new GUCs leak across transactions on a reused pooled
  connection.

Full details, including the migration-0018 text corrections this
uncovered, are in `docs/decisions/0016`'s "Corrections" section.

### 2. Staff MFA / step-up authentication — architecture only

`docs/decisions/0017-staff-mfa-and-step-up-authentication.md`. Explicitly
`NOT IMPLEMENTED` — no code, schema, or endpoint. Documents session
assurance level (`amr`/`mfa_at` JWT claims), a `RequireStepUp` middleware
mirroring `RequireTenantScope`'s per-route pattern, enrollment/
verification/recovery flow shape, and the audit events it will produce.
Which operations require step-up and their thresholds are left as open
human/compliance decisions, not invented.

### 3. Production authentication signing architecture — architecture only

`docs/decisions/0018-production-authentication-signing-architecture.md`.
Explicitly `NOT IMPLEMENTED` — dev/test keeps the current
`internal/auth.KeyRegistry` (HS256, env-supplied secrets) unchanged.
Documents the recommended production model: platform-owned identity
issuance (no managed identity provider) with KMS/HSM-managed asymmetric
signing (RS256/ES256), private key never leaving the KMS/HSM boundary,
`kid`-based rotation generalizing the existing `KeyRegistry` design to
KMS key versions. No cloud provider named or required.

### Verification performed (real PostgreSQL, not mocked)

`gofmt`/`build`/`vet`/`golangci-lint` clean throughout; full unit and
integration suite passing after every fix, including the newly-written
regression tests; all 18 migrations round-tripped up/down/up cleanly.

### Remaining security debt after this pass

- `brands`' public-read RLS policy remains broader than strictly needed
  (cross-tenant brand enumeration) — flagged in Stage 2 review, out of
  this pass's scope, not fixed.
- The refresh-rotation race-loser path is now audited but does not
  revoke the winner's chain, a deliberate trade-off (see
  `docs/decisions/0016`) rather than an oversight — worth revisiting if
  production data ever shows this path correlating with confirmed theft
  rather than benign retries.
- MFA/step-up and KMS-based signing are architecture only; no
  implementation timeline is set (both ADRs list open human decisions).

## Stage 3A — Financial Architecture Freeze — complete

Human-directed design-only stage: freeze the full wallet/ledger/payments/
withdrawal/crypto-custody/reconciliation architecture and put it through
independent specialist review *before* any Stage 3B implementation
begins. Explicitly NOT Stage 3B — no wallet, ledger, payment, PSP, or
crypto tables/migrations/services were created; `git diff --stat` for
this pass shows exactly one non-new-file change (`docs/architecture/
03-database-architecture.md`, a 13-line "superseded in detail by Stage
3A" pointer) plus ten new documentation files. No `.go` file, migration,
or config file was touched.

### Delivered (`NOT IMPLEMENTED` — architecture/design documents only)

- `docs/architecture/financial-domain-model.md` — fixes `Wallet` as
  scoped to `PlayerAccount` (not `Person`), the full Person/Tenant/Brand/
  PlayerAccount/Asset/Wallet/LedgerAccount/LedgerTransaction/LedgerEntry
  scoping table, and why house-level accounts are tenant-scoped rather
  than brand-scoped.
- `docs/architecture/ledger-accounting-model.md` — the full
  `LedgerAccount`/`LedgerTransaction`/`LedgerEntry` object model,
  per-account-type modeling for all ten Blueprint account types plus one
  architectural addition (`player_withdrawal_hold`), the tombstone
  mechanism, and the 15-item Mandatory Financial Invariants table.
- `docs/architecture/financial-transaction-flows.md` — all 20 canonical
  flows (deposit through jackpot contribution), each with accounts,
  idempotency key, failure/retry/compensation behavior, and which
  invariants apply.
- `docs/architecture/payment-orchestration.md` — `PaymentProvider`/
  `PaymentOrchestrator` interfaces, routing dimensions, cascade-on-decline,
  provider health, no real or mock PSP integrated.
- `docs/architecture/withdrawal-state-machine.md` — the full withdrawal
  workflow state machine, four-eyes approval design and its bypass-closure
  requirements, and the ADR-0017 step-up hook point.
- `docs/architecture/crypto-custody-boundary.md` — `CryptoCustodyProvider`
  interface, deposit-address/confirmation/withdrawal-instruction shapes,
  reaffirming ADR 0008's no-private-keys boundary; no custodian selected
  or integrated.
- `docs/architecture/reconciliation-model.md` — eight reconciliation
  streams (ledger↔projection, wallet↔PSP, wallet↔casino/sportsbook
  provider, provider payable, PSP clearing/reserve, crypto custodian),
  balance-projection rebuild procedure, mismatch investigation workflow.
- `docs/decisions/0019-authoritative-ledger-and-balance-projection-architecture.md`
  — the concrete RLS/tenancy shape for every new financial table, the
  append-only enforcement mechanism (trigger pair, not `REVOKE`), the
  actor-authorization matrix for who may originate which posting, and the
  authorization/isolation test floor Stage 3B must implement.
- `docs/decisions/0020-financial-idempotency-and-concurrency-control.md`
  — idempotency key scope and retry/concurrency semantics, including the
  Postgres `SAVEPOINT` requirement for the idempotent-insert pattern.
- `docs/decisions/0021-multi-asset-accounting.md` — re-confirms
  `NUMERIC(38,0)` + per-asset exponent, the asset-identity/precision/
  display-amount distinction, and the `ConversionOperation` cross-asset
  design (not implemented; no flow in Stage 3B produces one).

### Specialist review pass

`ledger-finance`, `payments`, `security`, `architect`, `backend`, `qa`,
and `code-reviewer` each independently reviewed the ten documents above
(documentation-only review — no application code exists in this domain
yet). Each was authorized to edit the documents directly to fix
non-business defects, and to add explicit `OPEN DECISION` markers rather
than invent business/policy resolutions. Summary of what was found and
fixed (see each document's own text for full detail — this pass generated
substantially more cross-file rework than either the Stage 1 or Stage 2
review passes):

- **Real accounting defects, fixed**: several flows in
  `financial-transaction-flows.md` were unbalanced or had inverted
  debit/credit direction as originally drafted — Flow 9 (sportsbook win
  settlement debited `house_gaming` for winnings-only against a
  stake-plus-winnings credit), Flow 11 (partial cash-out didn't handle a
  payout exceeding the released stake), Flow 12 (bonus grant posted two
  same-direction entries), Flow 18 and Flow 19 (`psp_clearing`/
  `psp_reserve` entries were on the wrong side, and `ledger-accounting-
  model.md` §2's stated normal-balance directions for those two accounts
  contradicted their own purpose). All corrected with the general
  (not just the common-case) entries spelled out.
- **A real multi-tenancy defect, fixed**: idempotency/uniqueness keys
  (`(provider_id, provider_tx_id)`, `idempotency_key`) were specified as
  platform-global on RLS-protected, tenant-partitioned tables — found
  independently by both the `architect` and `backend` reviews. This
  allows cross-tenant key collision/denial and, under RLS, turns a unique
  violation against an invisible row into a cross-tenant existence
  oracle. Fixed by namespacing every such key by `tenant_id` (and, at the
  workflow layer, by `player_account_id` too, for client-supplied keys)
  across `ledger-accounting-model.md`, both new ADRs, and the withdrawal
  document.
- **A real RLS self-inflicted-P1 risk, found and fixed before it could
  ever ship**: the `security` review recognized that ADR 0016's own
  hard-won discovery — Postgres requires a row to be visible under some
  `SELECT` policy before an `UPDATE`/`DELETE` can affect it, `RETURNING`
  or not — was about to be silently reintroduced on
  `wallet_balance_projection` and `withdrawal_requests`. A provider-
  callback posting (no player session, hence no player-scope GUC) doing
  its same-transaction projection `UPDATE` under a player-scope-ANDed
  policy would affect zero rows: entries commit, the projection silently
  doesn't, manufacturing exactly the P1 drift the design otherwise exists
  to prevent. Fixed in ADR 0019 with the OR'd-permissive-policy pattern
  and a mandatory `RowsAffected()` check, generalizing ADR 0016's fix
  rather than repeating its discovery process in Stage 3B.
- **An unimplementable Postgres pattern, fixed**: the `backend` review
  found that the idempotent-insert helper's "catch the unique-violation,
  then look up and return the original result" pattern is not achievable
  in the same outer transaction without a `SAVEPOINT` — Postgres aborts
  the rest of a transaction after any statement error. Documented
  explicitly in ADR 0020, including the pgx nested-`Tx` mechanics.
- **Withdrawal four-eyes bypass paths, closed**: the `security` review
  found the original design's `UNIQUE(withdrawal_request_id,
  approver_principal_id)` alone did not deliver four-eyes — a mutable
  request amount (approve low, raise after), no requirement that the
  approver be distinct from the beneficiary or hold the right permission
  in the right tenant, self-service threshold manipulation by a dual-
  permission principal, and no requirement that the two-approver check
  run inside the same transaction as the state transition it gates. All
  closed in `withdrawal-state-machine.md` §5; the threshold-manipulation
  and sub-threshold-structuring risks are recorded as explicit `OPEN
  DECISION`s for business/compliance policy, not invented.
- **Two blocking accounting gaps surfaced, not invented resolutions
  for**: (1) `promo_liability`'s normal-balance direction cannot be made
  to work as a credit-normal liability given how bonus grant/forfeiture
  must post it — found independently by `ledger-finance` and `architect`
  — leaving either "reinterpret `promo_liability` as debit-normal" or "add
  an eleventh `bonus_expense` account" as the two coherent resolutions,
  neither invented here; (2) no ledger account exists for the crypto
  custodian leg (`psp_clearing` is fiat-only by its own stated
  definition) and no ledger account exists for the bank-treasury leg of
  PSP settlement batching (Flow 18) — both flagged as blocking Stage 3B
  implementation of the affected flows specifically, not the whole
  design.
- **A missing actor-authorization statement, added**: nothing in the
  original draft stated which actor class (player session, verified
  provider callback, internal service, staff principal) may originate
  which `transaction_type` — the `security` review added the binding
  matrix to ADR 0019, closing a privilege-escalation gap the design would
  otherwise have left implicit.
- Numerous smaller defects across every document: broken/stale cross-
  references (a `§8` pointing nowhere, section renumbering left behind by
  earlier edits), a fabricated CLAUDE.md quotation removed, an
  unenforceable composite foreign key (`MATCH SIMPLE` with a nullable
  column silently disabling the check it claimed to make), a stored
  `LedgerTransaction.status` column contradicting its own "never
  mutated" prose, `WithdrawalApproval`/`ReconciliationRun`/
  `ReconciliationMismatch` tables originally missing `tenant_id` entirely
  despite prose claiming tenant-scoped RLS, and a double-subtracted
  withdrawal hold in the available-balance formula.
- No specialist found scope creep against the Stage 3A DO-NOT-BUILD list
  (no wallet/ledger/payment table, migration, or service code exists),
  and no document claims implementation status beyond `NOT IMPLEMENTED`/
  `ARCHITECTURAL DECISION`/`OPEN DECISION`.

### Open decisions requiring human/business/finance input before the affected Stage 3B flows can be implemented

The full list lives in each document's own `OPEN DECISION` markers;
the ones that actually block implementing a specific flow (not just
refine it) are:

1. `promo_liability` framing (debit-normal contra-liability vs. a new
   `bonus_expense` account) — blocks Flows 12/14/15.
2. Bank-treasury ledger account for PSP settlement batching — blocks
   Flow 18 balancing inside the ledger at all.
3. Ledger account for the crypto custodian leg — blocks crypto deposit/
   withdrawal postings (`crypto-custody-boundary.md`).
4. FX/conversion clearing account for `ConversionOperation` — blocks
   implementing cross-asset conversion (not required for any Stage 3B
   flow currently designed).
5. Jackpot liability-vs-expense framing, provider-fee expense account
   (Flow 17), negative-cash-balance policy after a deposit reversal
   (Flow 2), four-eyes/step-up threshold values and role-disjointness,
   sub-threshold-structuring window (AML), PSP rounding tolerance,
   deposit-address reuse model — all recorded, none blocking a specific
   flow's implementability the way 1–3 do.

### Verification performed

Documentation-only stage: no build/test/migration verification applies.
Verification performed was `git status`/`git diff --stat` confirmation
(by the independent `code-reviewer` pass) that only the ten new documents
plus one 13-line addendum to an existing document changed — no `.go`
file, migration, or config file. Cross-reference integrity (every
`§`-reference across all ten documents resolves to a real section) was
checked and confirmed.

## Stage 3A addendum — Payment Provider Agnosticism and Capability Model

The business owner directed, as a core commercial requirement, that the
platform integrate multiple replaceable fiat and crypto payment
providers, with no core financial-system rewrite needed to add one. Still
documentation-only — no wallet/ledger/orchestrator/adapter code exists.

### Delivered (`NOT IMPLEMENTED` — architecture/design documents only)

- `docs/decisions/0022-payment-provider-agnosticism-and-capability-model.md`
  (new ADR) — the full `ProviderCapability` model (fiat currencies, crypto
  assets, payment methods, countries, deposit/withdrawal/refund support,
  min/max amount, settlement behavior, callback capability, priority,
  tenant/brand availability, status), the multi-tenant/brand routing
  requirement (different brands may route the same currency to different
  providers simultaneously, entirely via configuration), the explicit
  separation of **Crypto Payment Provider** (a `PaymentProvider`
  implementation, routed like any fiat rail) from **Crypto Custodian** (a
  `CryptoCustodyProvider` implementation, ADR 0008) — never collapsed into
  one object even when one vendor performs both roles — a six-item
  provider-addition checklist proving the core (`Wallet`, ledger, balance
  projection, invariants, transaction flows) never needs to change to add
  a provider, and a provider-agnosticism test plan for Stage 3B (provider-
  conformance suite including the mock adapter, a provider-swap test, a
  routing-matrix test, a no-provider-branch code-review gate, and a
  custody-boundary-preserved test).
- `payment-orchestration.md` updated: `Capabilities()` now returns the
  full `ProviderCapability` shape rather than an unspecified placeholder;
  §4's routing dimensions explicitly state that different brands may
  route the same currency to different providers; new §11 points to
  ADR 0022 as the canonical source rather than duplicating it.
- `crypto-custody-boundary.md` updated: new §1.1 restates the Crypto-
  Payment-Provider-vs-Crypto-Custodian separation as binding (with a
  Mermaid diagram showing both paths into Wallet/Ledger), specifically
  because this is the boundary most likely to be silently broken by a
  future "it's just a payment gateway" integration that quietly acquires
  custody-boundary trust.
- `ledger-accounting-model.md` and `financial-domain-model.md`: brief
  cross-reference additions confirming the ledger was already
  provider-agnostic (`provider_id`/`provider_tx_id` are opaque strings,
  never a provider type or enum) and pointing to ADR 0022 as the binding
  statement of that property going forward.

### Specialist review pass

`payments`, `architect`, `security`, and `code-reviewer` independently
reviewed this addendum (documentation-only — no application code exists
in this domain). This pass found and fixed more substantive defects than
the volume of new text might suggest — two reviewers independently caught
the same high-severity issue, and `security` found two further real
privilege-boundary gaps:

- **Blocking, fixed (found independently by both `payments` and
  `code-reviewer`)**: `ProviderCapability.provider_kind` originally
  included `'crypto_custodian'` as a valid value on a row read by
  `RouteProvider` — directly contradicting §4's own rule that a custodian
  implements `CryptoCustodyProvider`, not `PaymentProvider`, and is never
  a routing candidate. As written, a custodian row could enter the
  routing pool and be selected for a deposit/withdrawal rail: exactly the
  custody-boundary collapse this whole addendum exists to prevent.
  Restricted to `'fiat' | 'crypto_payment'`.
- **Blocking, fixed (`security`)**: the Crypto-Payment-Provider-vs-
  Custodian separation was interface-shaped only — "never implements
  `CryptoCustodyProvider`" constrains what an adapter is *declared as*,
  not what a vendor *hands it at runtime*. A non-custodial-mode crypto
  gateway can return a private key, seed phrase, spend-capable extended
  key, or a signing handle in an ordinary API response or webhook; an
  adapter that merely accepts/logs/persists it breaks ADR 0008 without
  the interface rule ever being violated. Fixed with a new §4.1: such
  material is rejected at parse time, never persisted or logged (not even
  hashed), the operation fails, and a security alert fires; the canonical
  request/response/callback shapes have no free-form passthrough field
  that could carry it.
- **Blocking, fixed (`security`)**: §4's "may share a vendor SDK/client
  internally if convenient" for a dual-role vendor was a privilege-
  escalation path — a shared authenticated client or API key gives the
  payment adapter transitive custody scope. Fixed with a new §4.2:
  sharing limited to stateless library code only, separate `provider_id`s
  and separately-scoped credentials (the payment credential carries no
  key-export/signing/transfer scope), independent rotation.
- **Should-fix, fixed (`security`)**: ADR 0019's actor-authorization
  matrix treated every "verified provider callback" as one undifferentiated
  class; with N replaceable providers, any verified webhook key gained
  the union of everything any provider could originate. Refined so a
  provider's credential may only originate what its own
  `ProviderCapability` declares, for the tenant/brand it's bound to.
- **Should-fix, fixed (`security`)**: the new `provider_capabilities`
  table was missing from ADR 0019's deliberately-enumerated RLS table
  list and had no stated `tenant_id`/RLS shape of its own. Added, with
  handle-not-material/CDC-exclusion/fingerprint-only-audit rules
  mirroring `payment-orchestration.md` §10.
- **Should-fix, fixed (`architect`)**: ADR 0022 §3 cited
  `02-domain-and-service-boundaries.md` as already establishing the
  nullable-`brand_id`-fallback configuration-override pattern it uses —
  that document establishes no such pattern, and ADR 0012 actually went
  the opposite way. Rewritten to state the pattern is established here
  (justified by the same tenant-vs-brand split `financial-domain-model.md`
  already uses for house-level accounts), with a composite FK added and
  an `OPEN DECISION` on whether to generalize it platform-wide.
- **Should-fix, fixed (`architect`, `payments`)**: the checklist's "adding
  a provider never touches the ledger" claim was overstated — the *first*
  crypto rail of either kind (custodian or crypto payment provider) has
  nowhere valid to post its in-flight clearing leg, since `psp_clearing`
  is fiat-only by definition. Recorded as a bounded, one-time exception
  tied to the existing `crypto-custody-boundary.md` §4.1 open decision,
  not a new one.
- **Should-fix, fixed (`architect`)**: `ProviderCapability` silently
  carried a `brand_id` column in violation of `financial-domain-model.md`'s
  own brand-denormalization rule (only `Wallet`/`WithdrawalRequest` were
  permitted to). That document's rule and scoping table are updated to
  name it as the second permitted exception, with the reasoning stated
  rather than a silent carve-out.
- Several smaller fixes: an unfalsifiable "byte-identical ledger effects"
  test claim narrowed to an enumerated field-level comparison; the mock-
  conformance suite (§6) given concrete minimum assertions (decline-vs-
  ambiguous distinguishability, callback idempotency, capability-shape
  validation) after `payments` found it too vague to build a real suite
  against; a stale reference to a `LedgerTransaction.status` column that
  doesn't exist (§1.2 has none, by design) corrected to compare the
  derived posted/reversed label instead; a per-asset `min_amount`/
  `max_amount` bug fixed (was two scalar columns on a multi-asset row,
  which would apply BTC satoshi limits to EUR cents).
- Open decisions correctly recorded rather than invented: whether one
  vendor may hold both payment and custody roles for a tenant at all
  (concentration/blast-radius — a business/risk call); whether raw
  provider payloads are retained at all and for how long; how a single
  adapter serving many tenants selects the correct webhook-verification
  key without trial-verifying against every tenant's key (a real gap in
  the existing `payment-orchestration.md` §10 design, sharpened rather
  than introduced by this addendum); where a crypto-payment-provider's
  self-issued deposit addresses live, since `DepositAddress.custodian_ref`
  is currently `NOT NULL`.
- No specialist found scope creep — no vendor name, no hardcoded
  currency/method/provider, and no credential-like value appears anywhere
  in the new content; provider selection and integration remain
  explicitly out of scope.

### Verification performed

Documentation-only: `git status`/`git diff --stat` confirmed only
documentation files changed (one new ADR, edits to five existing
documents including `docs/decisions/0019` for the RLS-table-list and
actor-matrix refinements) — no `.go` file, migration, or config file.
Cross-reference integrity re-checked after all four reviewers' concurrent
edits: every `docs/decisions/0022 §N` reference across the four edited
architecture documents resolves to a real section.

## Stage 3B — Core Financial Infrastructure Implementation

The human issued the Stage 3B Final Approval directive: implement the
frozen Stage 3A/3A-addendum architecture under a controlled 25-item
scope, behind a mock PSP only, with bonus/crypto financial posting and
any flow needing an unresolved accounting counter-account explicitly
blocked. This section records what was built, every specialist-review
finding, and its resolution — honestly, including the defects found in
this session's own work, per CLAUDE.md's "record important implementation
decisions honestly" rule.

### Migrations (`migrations/0019`-`0028`)

`0019_create_wallets` — `wallets` table (one row per player/asset),
two-policy RLS (`tenant_staff_scope` FOR ALL requiring the player GUC
unset, OR'd with a SELECT-only `player_self_scope`) — the pattern every
subsequent Stage 3B player-owned table follows, and the one three tables
initially got wrong (see 0028 below).

`0020_create_ledger_accounts` — `ledger_accounts`, with
`ledger_accounts_populate_from_wallet()` trigger-based denormalization
(never a nullable composite FK) and partial unique indexes distinguishing
player-owned from house-level accounts.

`0021_create_ledger_transactions` — `ledger_transactions` (status is
derived, never a stored/mutated column), `ledger_deny_mutation()` +
triggers for append-only enforcement, tenant-scoped unique constraints
including `(tenant_id, provider_id, provider_tx_id)`.

`0022_create_ledger_entries` — `ledger_entries`,
`ledger_entries_populate_from_account()` trigger, the deferred
`ledger_entries_balanced` constraint trigger enforcing debits=credits per
transaction/asset, composite FKs, two-policy RLS, append-only triggers.

`0023_create_wallet_balance_projection` — `wallet_balance_projection`
maintained exclusively by an `AFTER INSERT` trigger on `ledger_entries`,
in the same transaction as the posting — structurally prevents the
"forgot to update the projection" bug class.

`0024_create_provider_capabilities` — `provider_capabilities` +
`provider_capability_amount_limits`. **Known gap, not fixed this stage**:
the amount-limits child table has no `tenant_id` column of its own and
its RLS policy is a subquery into `provider_capabilities` — ADR 0019
otherwise forbids this shape. Not currently exploitable (the subquery
inherits the parent's tenant check, which is itself now correctly
restrictive after 0028), but structurally fragile; closing it needs a
column addition and backfill.

`0025_create_deposit_intents` — `deposit_intents`, two-policy RLS.

`0026_create_withdrawal_requests_and_approvals` — `withdrawal_requests`
(immutable-after-insert trigger on identifying fields including
`amount`, closing the raise-after-approval bypass) + `withdrawal_approvals`
(append-only).

`0027_create_reconciliation_tables` — `reconciliation_runs` +
`reconciliation_mismatches`.

`0028_harden_stage3b_staff_only_tables_rls` — **security-review fix**:
`ledger_transactions`, `provider_capabilities`, `withdrawal_approvals`,
`reconciliation_runs`, and `reconciliation_mismatches` originally carried
a bare `tenant_id = app.tenant_id` policy with no guard excluding a
player-scoped connection (unlike every other Stage 3B table's two-policy
pattern). `db.Pool.WithPlayerScope` sets BOTH `app.tenant_id` and
`app.player_account_id`, so a player-scoped connection satisfied these
five tables' policy completely — full tenant-wide read/write, despite
each table's own comment claiming staff/system-only. Not exploited by any
code path that existed (every query against these tables ran under
`WithTenant`), which is exactly the latent-trap character this closes:
the natural next feature (a player-facing "transaction history" endpoint
joining `ledger_entries` to `ledger_transactions` under player scope)
would have silently leaked every player's data tenant-wide. Fixed by
adding the same `AND app.player_account_id IS NULL` guard every other
table already had. Migration round-trip verified; a dedicated test
(`TestLedgerTransactions_PlayerScopeSeesNoRows`) proves a player-scoped
connection now sees zero rows and cannot forge an INSERT, for the exact
tenant/player that legitimately owns the underlying wallet.

### Go packages

`internal/ledger` — posting engine (`Post`, `GetOrCreateAccount`,
`RebuildBalance`/`RebuildProjectionRow`), `AccountType`/`TransactionType`/
`Direction` model, SAVEPOINT-based idempotent insert
(`db.IdempotentInsert`), `SET CONSTRAINTS ... IMMEDIATE` forcing the
deferred balance check synchronously inside `Post` (and, after a
code-review finding, explicitly reset to `DEFERRED` afterward — Postgres's
constraint-mode change otherwise lasts the whole transaction, which would
break a second `Post` call in the same caller-supplied transaction).

`internal/wallet` — `GetOrCreate`/`GetByPlayerAndAsset`/`GetSummary`,
multi-asset, no floating point anywhere.

`internal/payments` — `PaymentProvider` interface, `Orchestrator`
(routing, cascade-on-decline, ambiguous-outcome resolution via
`QueryStatus`, callback processing), `MockProvider`, the capability
model (`LoadCapability`/`WriteCapability`/`ListRoutingCandidates`, narrow
-never-widen enforcement). **Security-review P0 fixed**: `MockProvider`
previously performed no webhook signature verification at all — since
the webhook route (`POST /v1/webhooks/payments/{tenantSlug}/{providerID}`)
has no bearer-auth middleware by design (a provider callback isn't an
authenticated platform principal), and provider references are
sequential and even returned to the player in `InitiateDeposit`'s
`redirect_url`, anyone could forge a "succeeded" callback for any
reference and mint an arbitrary ledger credit. Fixed by giving
`MockProvider` a randomly-generated per-instance HMAC-SHA256 secret;
`HandleCallback` now verifies a constant-time signature over the
callback's effect-bearing fields before acting on anything else, exactly
as `payment-orchestration.md` §3 already specified ("webhook signature
verification happens inside `HandleCallback` before any payload field is
used"). Four dedicated tests prove forged/unsigned/wrong-secret/tampered
payloads are all rejected, and a genuinely-signed one is accepted.
**P1s fixed**: (1) a deposit-reversal callback's `amount`/`asset_code`
were payload-controlled and uncapped — a reversal declaring an amount
far exceeding the original deposit would debit `player_cash` arbitrarily
into deeply negative territory; fixed by rejecting any reversal whose
amount/asset don't exactly match the original, and by rejecting a second,
distinctly-referenced reversal of an already-reversed deposit (the
ledger's own `(tenant_id, provider_id, provider_tx_id)` uniqueness only
catches a *redelivery* of the identical reversal, not a second, different
one) — new `ErrDepositAlreadyReversed` sentinel, three new adversarial
tests. (2) A late/out-of-order decline callback for an intent that had
already succeeded had no terminal-state guard — it would flip a succeeded
intent's status to declined while the ledger credit stayed posted
(permanent disagreement between the intent row and the ledger), null out
`provider_id`/`provider_reference` via the cascade-exhausted path, and
with a second provider configured could even trigger a *fresh*
`provider.Deposit` call for a deposit that already succeeded; fixed with
a terminal-state short-circuit mirroring `postDepositSuccess`'s own
redelivered-success handling, with a dedicated test. (3) The ledger
idempotency key for deposit success/reversal postings was the bare
provider reference, with nothing requiring reference uniqueness *across*
providers (the `PaymentProvider` contract never promises that); with a
second real PSP configured, a colliding reference could silently
misattribute one provider's deposit to another's ledger transaction.
Fixed by namespacing the key with `providerID + ":" + reference`,
matching the DB-enforced `(tenant_id, provider_id, provider_tx_id)`
index's own scoping.

`internal/withdrawal` — state machine (`requested` → `pending_review` →
`approved` → `submitted` → `completed`/`rejected`/`cancelled`/`failed`),
four-eyes `Approve`/`Reject` (`is_automated_approval` excludes
service-identity auto-approvals from the two-human count;
`threshold_amount_at_decision`/`request_amount_at_decision` snapshot each
decision for audit even though threshold manipulation prevention at
write time remains an open policy decision). **P1 fixed**: the original
HTTP submission handler took no row lock before calling out to a
PaymentProvider — two concurrent submit attempts (a staff double-click,
or a client retry) could both observe `approved` via a plain read and
both call the provider, a real double payout, with the loser's evidence
lost to its own rolled-back transaction. Fixed by adding
`LockApprovedForSubmission` (takes the row lock and verifies `approved`
as the FIRST database operation, before the provider is ever called,
mirroring how the deposit orchestrator's own idempotent insert serializes
concurrent deposit attempts) and wiring the HTTP handler to use it in
place of a plain `GetByID`. A dedicated concurrency test
(`TestLockApprovedForSubmission_ConcurrentSubmitsOnlyOneReachesProvider`,
5 concurrent goroutines) proves the simulated "provider call" happens
exactly once.

`internal/reconciliation` — `RunLedgerVsProjection`/`ResolveMismatch`,
tested against both a clean state and injected drift. Not yet wired to
any scheduler (CLAUDE.md's hourly target is not automated this stage).

### HTTP layer (`internal/httpserver`)

Wallet/deposit/withdrawal player self-service routes, staff four-eyes
review queue (`GET /v1/admin/withdrawals` promotes `requested` rows to
`pending_review` on open — the actual wiring of the
`requested`→`pending_review` transition, since no automated KYC/risk
engine exists yet to trigger it, and doing so at request time instead
would have collapsed the documented player-cancellation window to zero)
+ approve/reject/submit routes, the provider webhook route, and a
provider-capability admin route.

**Runtime bug fixed before any specialist review** (found via live HTTP
smoke testing, not by any unit/integration test — those call the
packages directly with correctly-scoped transactions and could never
have caught an HTTP-layer wiring mistake): three handlers
(`newGetWalletHandler`, `newInitiateDepositHandler`,
`newRequestWithdrawalHandler`) used `db.Pool.WithPlayerScope` for
operations that WRITE to RLS-protected financial tables. By design,
every financial table's `player_self_scope` policy is SELECT-only, and
`tenant_staff_scope`'s `WITH CHECK` requires the player GUC to be unset —
so a write under `WithPlayerScope` satisfied neither policy and failed
with a raw Postgres RLS violation. Fixed by switching all three to
`WithTenant`, matching how `internal/ledger`/`internal/withdrawal`/
`internal/payments` always write under tenant-only scope internally, even
for player-triggered actions (isolation comes from the server-derived
`playerAccountID` being threaded explicitly as a parameter, never from
RLS row-filtering, for writes).

**Withdrawal PSP submission added this stage**
(`POST /v1/admin/withdrawals/{id}/submit`) — deliberately narrower than
the deposit orchestrator: routes through `PaymentOrchestrator.RouteProvider`
and calls `provider.Withdraw` once, no cascade-on-decline, and an
`OutcomePending`/`OutcomeAmbiguous` result leaves the request at
`submitted` with no further automated ledger effect (per
`payment-orchestration.md`'s "never automatically resubmit on timeout/
ambiguity if it could duplicate effect" — resolving those requires a
withdrawal callback path `internal/payments` does not implement yet, a
genuine, labeled Stage 3B scope boundary, not a silent gap).

**Security-review P2 fixed**: `withdrawal.Approve`/`Reject`/`MarkSubmitted`
's own internal audit records are `ActorSystem` (they have no access to
the `*http.Request`), so CLAUDE.md's "every mutating administrative/
financial action writes an audit record (actor... IP... reason code)"
requirement wasn't fully met — worst for the submit action, where the
audit log couldn't answer "which staff member pushed this money out the
door" at all. Fixed by adding a supplementary `audit.Record` call inside
each handler's own transaction (same atomicity guarantee), carrying the
authenticated staff principal, IP, user-agent, and request id — without
changing `internal/withdrawal`'s already-tested exported signatures.

**Also fixed**: the payment webhook now rejects callbacks for a
suspended tenant (previously `identity.GetTenantBySlug`'s `status` field
was never checked), with the same not-found response as an unknown slug
for enumeration resistance; a new `ErrCallbackSignatureInvalid` maps to a
4xx rather than a 500, so a real PSP retrying a bad signature learns it's
wrong instead of retrying forever.

### Mandatory 26-item adversarial test suite

All 26 items from the Stage 3B directive verified against real
PostgreSQL 16, split across three specialist passes (`ledger-finance` for
ledger/wallet items, `payments` for provider items, `backend` for
withdrawal/cross-tenant items) plus the security/architecture/code-review
findings above, each with its own dedicated test:

1 double spend, 2 duplicate provider callback, 3 concurrent duplicate
idempotency, 4 same-key-different-payload, 5 cross-tenant financial
access, 6/7 direct ledger UPDATE/DELETE rejection, 8 compensation
(reversal is a new transaction, history untouched), 9 withdrawal
self-approval (package-level mechanism proven; HTTP-layer gap honestly
documented, see `active-stage.md`), 10 withdrawal amount mutation, 11
duplicate approval (both `Approve` and `Reject` paths), 12 approval
substitution (approver id always server-derived from the JWT, never
client-suppliable), 13 threshold manipulation (the recording mechanism
proven; the underlying policy question remains open by design), 14
projection rebuild (including recreating a fully-deleted projection
row), 15 provider timeout, 16 ambiguous provider outcome (never
auto-cascaded without `QueryStatus` first, for both the synchronous and
callback-triggered paths), 17 provider retry, 18 provider swap, 19 asset
precision (independently-tracked exponents across EUR/USDT/BTC,
including a value at the float64-precision boundary), 20 insufficient
funds, 21 unbalanced transaction (including a same-transaction,
cross-asset variant a naive check would wrongly accept), 22 provider
capability mismatch, 23 custodian-shaped capability never enters payment
routing (including a direct-SQL attempt against the DB's own
`provider_kind` CHECK constraint), 24 key-material boundary at multiple
injection points, 25/26 dual-role vendor separation (architecturally
confirmed closed — no `CryptoCustodyProvider` code exists yet for a
credential to dual-acquire a role from).

One genuine bug was found and fixed while writing item 4's test:
`InitiateDeposit`'s idempotency-conflict branch unconditionally returned
the original intent on any key collision, never checking whether the
retried parameters actually matched — inconsistent with the identical,
already-tested pattern in `internal/ledger.Post` and
`internal/withdrawal.RequestWithdrawal`. Fixed with a new
`ErrIdempotencyKeyReused` sentinel and a parameter-match check mirroring
the sibling packages exactly.

### Specialist review (post-implementation)

`ledger-finance` and `payments` performed their review as part of the
adversarial-test pass above (no invariant violation found in either
domain beyond the one idempotency bug already listed). `security`,
`architect`, and `code-reviewer` performed independent reviews of the
full diff; `qa` independently assessed testing-strategy completeness
against CLAUDE.md's mandatory financial test matrix. Every P0/P1 finding
across all four reviews is fixed and tested, listed above by package.
Findings NOT fixed this stage, and why, are recorded honestly in
`active-stage.md`'s "Blockers / genuine scope boundaries" section rather
than silently dropped: the withdrawal self-approval HTTP-layer gap
(needs a Stage 2 schema change), `provider_capability_amount_limits`'
missing `tenant_id` (needs a column addition + backfill), the withdrawal
PSP-submission narrowing (needs a withdrawal callback path), and the
reconciliation-stream scope reduction (needs real PSP/casino/sportsbook
relationships that don't exist yet). `qa` additionally identified and
closed a zero-coverage gap in `internal/auth`'s own role-permission
tests for the two new Stage 3B permissions.

### Verification performed

`gofmt -l .`, `go build ./...`, `go vet ./...` clean throughout. Full
integration suite (`go test -tags=integration -count=1 ./...`) green
after every fix, re-run fresh (not cached) multiple times across the
session. `go test -race -tags=integration` clean on
`internal/ledger`/`internal/wallet`/`internal/withdrawal`. Migration
round-trip (`up`→`down`→`up`) verified for migrations 0019-0028. Live
HTTP smoke test performed end-to-end (register→login→configure mock
provider capability→deposit→webhook callback→wallet balance→withdrawal
request→four-eyes approve/reject→submit) before and after the RLS-scope
bug fix, which is what caught that bug in the first place.

### Not claimed

Per CLAUDE.md's "No fake completion" rule: this stage does NOT claim
production financial readiness, regulatory certification, real-PSP
integration readiness, real-crypto readiness, or production KMS/HSM
readiness. "Mock PSP success" is explicitly not "production PSP
integration readiness." Bonus financial posting, crypto deposit/
withdrawal financial posting, crypto-custodian ledger settlement, and any
PSP batch-settlement flow remain **NOT IMPLEMENTED** — no accounting
treatment was invented for any of them to make code compile.

## Stage 3C — Financial Hardening & Operational Controls — complete (approved-pending)

Stage 3B was substantively approved by the human, who issued a focused
Stage 3C directive: close five specific gaps Stage 3B's own specialist
review left documented but open, run a fresh specialist review
specifically attempting to break each fix, and fix every P0/P1 that
review found — before Stage 4 domain implementation begins. Full detail
of every decision: ADR 0023 (`docs/decisions/0023-stage3c-financial-
hardening-decisions.md`).

### 1. Withdrawal self-approval — closed, authoritatively, with a documented residual gap

- Migration `0029`: `staff_users.person_id` (nullable FK into the
  existing cross-tenant `persons` identity — no new identity model
  invented) plus a `BEFORE INSERT` trigger on `withdrawal_approvals`
  (`withdrawal_approvals_deny_self_approval`) denying any approval whose
  approver resolves to the same person as the withdrawing player.
  Authoritative at the database itself, not merely audit-detectable —
  proven by a direct-SQL-bypass test going around the HTTP/service
  layers entirely.
- `newApproveWithdrawalHandler` gained a real `BeneficiaryCheck` closure
  (defense-in-depth pairing with the trigger, for a clean HTTP 403
  instead of a raw Postgres exception).
- **Specialist review (code-reviewer) found a related, distinct P1**:
  the four-eyes *distinct-approver count* (separate from the
  self-approval check) was keyed on `approver_principal_id` alone, so
  one person holding two staff logins sharing a `person_id` could supply
  BOTH required approvals for a THIRD PARTY's withdrawal — the
  self-approval trigger never fires for this case (neither login is the
  beneficiary). Fixed: the count now dedupes via
  `COALESCE(staff_users.person_id, approver_principal_id)`. Proven by a
  new adversarial test (`TestWithdrawalApprove_MultiAccountIdentityBypassRejected`)
  covering exactly this "multi-account identity bypass" attack the
  Stage 3C directive named.
- Migration `0033` (specialist-review fixes) removed a redundant
  `is_automated_approval` trust-flag shortcut from the trigger (security
  review: it added no real protection and weakened the "purely
  data-driven" guarantee).
- **Documented residual gap, not closed this stage**: `person_id`
  linkage is optional, admin-asserted at staff creation, unverified, and
  has no update path. A staff account created without it (the default)
  is invisible to both enforcement layers. Labeled `PARTIALLY
  IMPLEMENTED` per CLAUDE.md's "no fake completion" rule — see ADR 0023
  §1 for the two paths (verified identity binding, or disjoint
  staff-management/approval permissions) that would close it fully.

### 2. `provider_capability_amount_limits` tenancy — resolved as tenant-owned

Migration `0030`: added `tenant_id NOT NULL` (backfilled from the parent
`provider_capabilities` row, requiring a temporary, transaction-scoped,
DDL-level RLS disable/enable — documented as safe and not a role-level
bypass), a composite `(provider_capability_id, tenant_id)` FK, and a
direct `tenant_isolation` RLS policy replacing the prior
ADR-0019-violating subquery-based one. Cross-tenant routing-isolation
and write-forgery adversarial tests both pass against real PostgreSQL
16. **Specialist review (security, ledger-finance) found this new
direct policy — and the sibling new `withdrawal_policies` table's —
omitted the player-scope exclusion guard every other staff/system-only
financial table has carried since migration `0028`.** Fixed in migration
`0033` on both tables.

### 3. Withdrawal stranded-hold resolution — bounded, query-only recovery

`LockSubmittedForResolution` + a single `provider.QueryStatus` call,
exposed via `POST /v1/admin/withdrawals/{id}/resolve` and `GET
/v1/admin/withdrawals/submitted`. Never resubmits (no `Withdraw` call)
and never re-routes to a different provider than `MarkSubmitted`
originally recorded. Idempotent — a duplicate or delayed resolve call
after the request already completed is a safe no-op (proven by counting
DISTINCT ledger transactions, not just reading the current state
column). **Specialist review (payments, ledger-finance) found the
handler did not cross-check the provider's confirmed `Amount`/
`AssetCode` against the original request before completing** — the same
class of check the deposit-reversal path already makes via
`payments.ErrCallbackProviderMismatch`. Fixed: the resolve handler now
makes the identical check and refuses (409) rather than completing on
mismatched data, proven by a new test using a new `MockProvider.
SetConfirmedAmount` test knob. This is a **manual, staff-triggered**
recovery path — no scheduler currently drives it automatically.

### 4. Reconciliation scheduling — operationalized, per-tenant-isolated

`internal/reconciliation.RunSchedulerLoop` (started in
`cmd/platform-api/main.go`, configurable via
`RECONCILIATION_INTERVAL_SECONDS`, default hourly) sweeps every active
tenant, each in its own transaction with a transaction-scoped advisory
lock. `reconciliation_mismatches` gained a `mismatch_kind` column
(migration `0031`) distinguishing a missing projection row from a
present-but-wrong one. **Specialist review found and fixed**: (a) the
scheduler goroutine had no panic recovery (backend, P0 — a panic inside
a sweep would have crashed the entire API process, not just
reconciliation); (b) graceful shutdown didn't actually wait for the
scheduler, only accidentally via `pool.Close()`'s blocking semantics
(backend, P1) — now a bounded, explicit wait; (c) the advisory lock used
a 32-bit `hashtext`, collision-prone at scale — switched to 64-bit
`hashtextextended` (architect/ledger-finance); (d) a mismatch found was
logged at `Info` level — CLAUDE.md treats non-zero drift as a P1
incident, now logged at `Error`; (e) the comparison covered only debit/
credit totals, missing a possible `AssetCode`/`AccountType` corruption
in the same projection row — now compared too; (f) a skipped (lock-
contention) tick's audit record showed an ambiguous empty status —
now explicit; (g) a suspended/closed tenant was swept identically to an
active one — now filtered out. Adversarial tests K-N (concurrent runs,
failure-and-retry, projection corruption-and-rebuild, mismatch detection
without ledger mutation) all pass against real PostgreSQL 16.

### 5. Withdrawal policy configuration — real boundary, fails closed

`internal/withdrawal/policy.go` + migration `0032`
(`withdrawal_policies`) replace the Stage 3B flat, asset-blind threshold
constant with a tenant/brand/asset/effective-time-scoped configuration
boundary. **Specialist review (ledger-finance) rejected the original
design on sign-off**: a "1000 major units of this asset" default
conflated decimal precision with real-world value — for BTC specifically
it computed a ~1000 BTC threshold, dramatically WEAKENING protection
relative to even the constant it replaced, and a genuinely
value-equivalent default requires FX/market-price data explicitly out of
scope. **Redesigned to fail closed instead**: the zero-config default is
now `ThresholdAmount = 0` for every asset, requiring full approval
scrutiny for any non-zero withdrawal until a tenant configures a real
threshold. Additional specialist findings fixed: a non-deterministic
final tiebreaker in the resolution query's `ORDER BY` (architect/
backend); a single-column `brand_id` FK inconsistent with every sibling
table's composite form, allowing a cross-tenant-dead policy row
(architect, fixed via migration `0033`); `required_approver_roles` and
`jurisdiction_code` writable but unenforced, a false-sense-of-protection
risk (security/architect) — both now `CHECK`-constrained to `NULL` until
real enforcement/resolution exists. A clean, fail-closed enforcement
boundary for a future step-up/MFA requirement
(`ApprovalPolicy.RequireStepUp`/`ErrStepUpRequired`) was added without
implementing MFA itself (ADR 0017 preserved).

### Adversarial tests (directive items A-O, plus specialist-review additions)

All items A-O pass against real PostgreSQL 16 (`internal/httpserver/
stage3c_self_approval_test.go`, `internal/withdrawal/adversarial_test.go`,
`internal/payments/capability_integration_test.go`,
`internal/httpserver/stage3c_withdrawal_resolution_test.go`,
`internal/reconciliation/scheduler_integration_test.go`,
`internal/withdrawal/policy_integration_test.go`), independently
verified letter-by-letter by the `qa` specialist against the actual test
bodies and a fresh, uncached, `-race` test run (209 tests passed).
Additional tests written in direct response to specialist findings:
`TestWithdrawalApprove_MultiAccountIdentityBypassRejected`,
`TestWithdrawalResolve_ProviderAmountMismatchRejected`.

### Specialist review

Seven specialists ran in parallel against the full Stage 3C diff:
`ledger-finance`, `payments`, `security`, `architect`, `backend`, `qa`,
`code-reviewer`. Confirmed findings and fixes are itemized above and in
ADR 0023. `ledger-finance` explicitly withheld sign-off pending three
findings (the value-blind default threshold, the RLS gap, the
`Info`-level drift logging) — all three fixed and re-verified.

### Verification performed

`gofmt -l .`, `go build ./...`, `go vet -tags=integration ./...` clean
throughout (one pre-existing, unrelated `errcheck` finding in
`internal/payments/mock.go` predates this stage). Full integration suite
(`go test -tags=integration ./...`) green after every fix, including a
full re-run after the complete 5-migration chain (`0029`-`0033`)
round-tripped down and back up together. `git status`/`git diff --stat`
confirmed no production credentials and no unrelated scope creep.

### Not claimed

Per CLAUDE.md's "No fake completion" rule: this stage does NOT claim
production PSP readiness, regulatory certification, production MFA
readiness, or real crypto readiness. Self-approval enforcement is
labeled `PARTIALLY IMPLEMENTED`, not `IMPLEMENTED` — see §1's residual
gap. Withdrawal resolution is a manual recovery path, not an automated
sweep. No production withdrawal approval threshold is proposed anywhere
in this stage's code, migrations, or documentation.

## Stage 3D — Withdrawal Governance Final Gate — complete (approved-pending)

Status: **Complete, pending human approval to authorize Stage 4.** Issued
immediately after the human approved Stage 3C, as an explicit "NOT a new
financial architecture stage" directive: a tightly-scoped governance-
hardening pass closing Stage 3C's own documented residual gap (self-
approval enforcement was `PARTIALLY IMPLEMENTED` because `staff_users.
person_id` linkage was optional and unenforced), under an approved
business decision. Casino, sportsbook, bonus, B2C frontend, partner
console, real PSP, real crypto, and production MFA were explicitly out of
scope; full detail (including the verbatim business decision) is in ADR
`0024`.

### Approved business decision (verbatim)

> Withdrawal approval requires attributable Person identity and
> approver/beneficiary separation.

With seven numbered requirements - mandatory staff→Person linkage,
attributability, approver/beneficiary separation, staff-management/
withdrawal-approval permission separation, no implicit grant via a broad
admin role, no eligibility for unlinked accounts, and no second identity
model. Full text in ADR `0024` §"The approved business decision."

### Completed work

Full itemized account in ADR `0024`. Summary:

- **One migration** (`0034`, iterated in place during specialist review -
  see below): `staff_users.person_id` becomes append-only (`NULL` → a
  value allowed; a value → a different value permanently refused); the
  Stage 3C self-approval-only trigger is replaced by `withdrawal_
  approvals_enforce_governance`, requiring every human decision's
  approver to resolve to a linked, active `staff_users` row; and
  `withdrawal_policies` gains a `BEFORE UPDATE` deny-mutation trigger.
- **RBAC separation**: `PermWithdrawalApprove` split into `PermWithdrawal
  Review/Approve/Reject/Submit`; `RoleTenantAdmin` loses all four
  (previously held `PermWithdrawalApprove` alongside `PermStaffManage` -
  the Stage 3C-identified escalation vector); `RoleFinance` is the sole
  grantee. A new `PermWithdrawalPolicyWrite` (granted only to
  `RoleTenantAdmin`, never `RoleFinance`) gates the new policy admin API.
- **Service-layer enforcement**: `internal/withdrawal.Approve`/`Reject`
  take a mandatory `ApproverEligibility` closure (fail-closed on `nil` for
  a human decision, mirroring the existing `BeneficiaryCheck` pattern but
  stricter); `newSubmitWithdrawalHandler`/`newResolveWithdrawalHandler`
  carry an equivalent explicit check directly (no database trigger covers
  those two transitions, since neither inserts into `withdrawal_
  approvals`).
- **Staff-person-link remediation endpoint**: `POST /v1/admin/tenants/
  {tenantID}/staff/{staffID}/person-link`, `PermStaffManage`-gated, the
  sanctioned path to link a legacy unlinked account (never to relink).
- **Minimal withdrawal-policy admin API**: `GET`/`POST`/`DELETE /v1/admin/
  withdrawal-policies` - insert-only creation (never `UPDATE`), `DELETE`
  requiring a `reason_code` and capturing a before-image in its audit
  record, backdated `effective_from` rejected, `jurisdiction_code`/
  `required_approver_roles` never accepted from the request body.
- **Adversarial tests A-H** (directive item 3's full list): A/B/D/H were
  already covered by Stage 3C's `stage3c_self_approval_test.go`
  (unchanged, still passing); C/E/F/G are newly covered end-to-end
  (`stage3d_withdrawal_governance_test.go`) plus at the database layer
  directly (`internal/withdrawal/adversarial_test.go`'s
  `TestWithdrawalApprovalsGovernance_*` tests, including direct-SQL
  bypass attempts). Policy security tests (unauthorized/cross-tenant
  access, invalid input, inactive/not-yet-effective policy, removal and
  its audit-trail integrity) in `stage3d_withdrawal_policy_admin_test.go`
  and `policy_integration_test.go`.
- **Independent specialist review** (`security`, `ledger-finance`,
  `payments`, `architect`, `backend`, `qa`, `code-reviewer`, all seven in
  parallel) found and this stage fixed, before being considered complete:
  a **P0** where an early draft of migration `0034` let a real staff
  member self-approve their own withdrawal by setting `is_automated_
  approval = true` (reopening a bypass Stage 3C's migration `0033`
  deliberately closed); a **P1**, confirmed independently by three
  specialists, where removing withdrawal permissions from
  `RoleTenantAdmin`'s role definition alone did not stop a tenant_admin
  from minting a brand-new `finance`-role staff account via
  `PermStaffManage` and self-escalating (closed by restricting `finance`-
  role staff creation to platform-scoped callers); and several P2s
  (an overclaiming "verified Person" wording, missing before-image/
  reason-code on policy deletion, missing DB-level append-only guard and
  backdating protection on `withdrawal_policies`, and test-coverage gaps
  in adversarial item G and the policy-removal/inactive-policy
  scenarios). **All were fixed, each with a dedicated regression test**;
  full detail and the specific fix for each finding is in ADR `0024`'s
  "Specialist review findings and fixes" section.

### Verification performed

`gofmt -l .`, `go build ./...`, `go vet -tags=integration ./...` clean.
Full test suite (`go test -tags=integration ./...`) passes against real
PostgreSQL 16 - 127 tests in the three most-affected packages
(`internal/httpserver`, `internal/withdrawal`, `internal/auth`) verified
individually with `-count=1 -v` (0 failures), plus the full-repo run
including `internal/reconciliation`'s scheduler suite. Migration `0034`
round-tripped (`up` → `down` → `up`) both mid-review (after the P0 fix)
and again as the final validation pass. `git status`/`git diff --stat`
confirmed no production credentials and every changed file traces to an
approved Stage 3D item or a specialist-review fix.

### Not claimed

Per CLAUDE.md's "No fake completion" rule: this stage does NOT claim
production readiness, regulatory certification, production MFA readiness,
PSP readiness, or crypto readiness. It does not claim the TOCTOU race on
submit/resolve eligibility (ADR `0024`'s open item 2) is closed - that
remains a known, documented residual risk with a low practical blast
radius, not a silently accepted one. No production withdrawal approval
threshold, required-approver-role rule, or step-up requirement is
proposed anywhere in this stage's code, migrations, or documentation.

## Stage 4A — Casino Integration Foundation — complete (approved-pending)

Status: **Complete, pending human approval to authorize Stage 4B.** Issued
after Stage 3D's approval, as a "CASINO INTEGRATION FOUNDATION" directive:
a production-grade, provider-agnostic casino integration layer - the
`CasinoProvider` interface, a `MockCasinoProvider`, a platform-wide/
tenant-opt-in game catalogue, an opaque single-use game-launch-token
mechanism, a two-layer provider capability/routing model, and bet/win/
rollback posting on top of the already-approved ledger
(`financial-transaction-flows.md` Flows 5-7). Real casino provider
contracts, production credentials, sportsbook, the Bonus Engine, KYC/AML,
RG, a lobby UI, and a back-office catalogue UI were explicitly out of
scope. Full design and the complete specialist-review findings/fixes list
are in `docs/decisions/0025-casino-provider-abstraction-and-game-session-
model.md`.

### Completed work

Full itemized account in ADR `0025`. Summary:

- **Two migrations**: `0035` (the four new tables - `casino_games`
  platform-wide no-RLS, `casino_game_availability`/`casino_provider_
  capabilities` tenant-owned RLS with the player-scope-exclusion guard,
  `casino_launch_sessions` tenant-owned RLS with the dual `tenant_staff_
  scope`/`player_self_scope` pattern mirroring `withdrawal_requests` -
  plus the additive `casino_bet`/`casino_win`/`casino_rollback` ledger
  transaction-type CHECK values) and `0036` (hardening added during
  specialist review: an immutability trigger on `casino_launch_sessions`
  mirroring `withdrawal_requests`'s, and a tenant-scoped `token_hash`
  uniqueness in place of a platform-global one).
- **`internal/casino`** (7 files): `types.go` (the `CasinoProvider`
  interface, every request/response shape, sentinel errors),
  `catalogue.go` (platform/tenant catalogue split), `capability.go`
  (two-layer capability model, narrowing-only enforcement), `launch.go`
  (single-use launch-token generation/resolution, mirroring `internal/
  auth`'s refresh-token pattern in a separate trust domain),
  `orchestrator.go` (`LaunchGame`'s full eligibility chain; `ReceiveCallback`
  dispatch with tenant-capability enforcement; `postBet`/`postWin`/
  `postRollback` implementing Flows 5-7), `mock.go` (`MockCasinoProvider`,
  HMAC-signed synthetic callbacks).
- **HTTP layer**: player-facing catalogue listing/launch, the provider
  callback webhook (no bearer-auth middleware, adapter-verified signature
  instead), and admin endpoints for platform catalogue management
  (platform_admin only), tenant provider-capability configuration, and
  tenant game-availability configuration - `casino_handlers.go`,
  `casino_admin_handlers.go`, `casino_routes.go`, two new permissions
  (`PermCasinoCatalogueManage`, `PermCasinoConfigWrite`).
- **Provider conformance suite**: `conformance_test.go` (adapter-contract-
  only, no DB - catalogue/launch/bet/win/rollback contracts, ambiguous/
  timeout outcome, provider authentication, provider failure, capability
  shape) and `orchestrator_integration_test.go` (real PostgreSQL - every
  directive item A-T, plus dedicated regression tests for every specialist-
  review finding below and concurrency tests for duplicate-bet idempotency,
  distinct-rollback exclusivity, and single-use launch-token resolution)
  plus `internal/httpserver/casino_flow_integration_test.go` (HTTP-layer
  authorization/tenant-isolation/webhook-authentication).
- **Independent 7-specialist review** (casino integration architecture,
  financial correctness, security, PostgreSQL/RLS, API/HTTP, multi-tenancy,
  adversarial testing, all seven in parallel) found and this stage fixed,
  before being considered complete: **six P1s**, several confirmed
  independently by multiple specialists and three empirically reproduced
  during review - (1) the launch-session credential was minted but never
  consulted on the bet path, letting a payload-supplied `player_account_id`
  authorize an arbitrary player's debit and making demo-vs-real-money
  indistinguishable at posting time; (2) a win callback credited whatever
  player its own payload named rather than the round's actual bettor,
  empirically reproduced; (3) two concurrent, distinct rollback references
  for the same bet both succeeded, doubling the reversal credit,
  empirically reproduced; (4) a tenant's own `CasinoProviderCapability` -
  documented as a kill switch - had no effect on the bet/win/rollback path;
  (5) a provider-declared `declined`/`ambiguous` `Outcome` on a bet/win
  callback posted identically to `succeeded`, empirically reproduced; (6)
  zero concurrency tests existed for the casino financial/launch paths, and
  zero HTTP-level tests existed for any casino route. **All six were fixed,
  each with a dedicated regression test**; full detail and the specific fix
  for each finding, plus the fixed P2s and the explicitly-deferred residual
  items, are in ADR `0025`'s "Specialist review findings and fixes" section.

### Verification performed

`gofmt -l .` clean. `go build ./...`, `go vet ./...`, `go vet -tags=integration
./...` clean. `go test ./...`, `go test -race ./...`, `go test -tags=integration
./...`, and `go test -race -tags=integration ./...` all pass across the
full repository, including 30 top-level tests (plus subtests) in
`internal/casino` and 6 new HTTP-level tests in `internal/httpserver`.
Migration `0036` round-tripped (`up` → `down` → `up`) cleanly. Migration
`0035`'s own round-trip was verified clean on a fresh database earlier in
the stage (by direct test and independently by the architect specialist
review); by the end of the stage the dev database has posted real
`casino_bet`/`casino_win`/`casino_rollback` rows from the test suite
itself, so `0035`'s down migration can no longer succeed there - documented
in `0035`'s own down-migration file as the correct, expected behavior for
an append-only ledger (CLAUDE.md: "Corrections are compensating entries,
never edits or deletions of historical entries"), not a defect.

### Not claimed

Per CLAUDE.md's "No fake completion" rule: this stage does NOT claim a
real casino provider integration, production readiness, regulatory
certification, or RG/self-exclusion enforcement at game launch (an
explicitly deferred, pre-existing platform-wide gap - see ADR `0025`'s
findings section). Free-round/bonus-stake normalization and jackpot-
contribution splits remain `OPEN DECISION`s owned by the Bonus Engine
stage. Per-tenant provider signing keys (today one secret per adapter
instance, shared across every tenant routed to it) are recorded as a
Stage 4B precondition, not solved here.

## Stage 4D-RG — Responsible Gaming Player-Status Enforcement Foundation — complete (approved-pending)

Issued directly after Stage 4A's own specialist review identified a
production-blocking gap: no authoritative platform-side player-account/
wallet-status or self-exclusion check existed at casino game launch or
bet time. Closes exactly that gap - not the full KYC/AML/RG subsystem.

### What was built

1. **`internal/rg` package** - `player_restrictions` (self-exclusion,
   bound to the platform-wide `Person` so it can be enforced across
   brands/tenants once two accounts share a `person_id` - see the
   important precondition noted below), and `EvaluateEligibility`, the
   single authoritative "may this player gamble right now" policy
   boundary composing `PlayerAccount.Status`, `Wallet.Status`, and the
   new restriction signal - no second identity model.
2. **Migrations `0037`/`0038`** - the `player_restrictions` table,
   append-only (RLS + a deny-mutation trigger, later hardened to a
   statement-level trigger), dual-scope RLS (platform-wide vs. tenant vs.
   brand), player-self-service vs. staff insert policies, a composite FK
   hardening pass.
3. **Casino launch/bet enforcement** - `internal/casino`'s `LaunchGame`
   and `postBet` both consult `EvaluateEligibility` before a session
   becomes usable / before a debit commits. A denial is a RESULT
   (`LaunchGameResult.Denied` / `ReceiveCallbackResult.OutcomeDeclined`),
   never a Go error - an earlier error-based attempt was found, by test,
   to silently roll back its own audit record.
4. **A person-keyed Postgres advisory lock** closes the self-exclusion-
   vs-concurrent-launch/bet TOCTOU race deterministically - proven under
   `-race` with real concurrent goroutines against real Postgres for all
   three directive-required race scenarios (self-exclusion during launch,
   during a bet, and during a duplicate bet delivery).
5. **New `PermRGRestrictionWrite`/`Read` permissions**, granted only to
   `RoleCompliance` (write) and `RoleCompliance`/`RoleTenantAdmin` (read)
   - never `RolePlatformAdmin`, which has no path to resolve a tenant's
   player account at all today.
6. **Minimal HTTP API** - player self-service self-exclusion + status;
   staff create/read restriction, tenant/brand-scoped only (a genuinely
   platform-wide staff-initiated restriction remains an open decision,
   not built - see ADR `0026` §4).

Full design, rationale, and the complete specialist-review findings/fixes
list: `docs/decisions/0026-responsible-gaming-player-status-enforcement-
foundation.md`. Updated architecture doc:
`docs/architecture/11-kyc-aml-rg-architecture.md`'s new "Implementation
status" section.

### Specialist review: one P0, four P1s found and fixed, plus a real
financial-idempotency bug the testing/review pass itself surfaced

An independent 7-specialist parallel review (RG architecture, security,
financial correctness, identity/Person model, PostgreSQL/RLS, API/HTTP,
adversarial testing) found:

- **One P0, reported independently by three reviewers**: the cross-brand/
  cross-tenant self-exclusion PROTECTION this stage's own Context section
  originally claimed to close is mechanism-correct but currently
  unreachable in production - `internal/identity.RegisterPlayer` mints a
  fresh, unlinked `Person` on every registration, with no resolution/
  dedup logic anywhere in this codebase, so a real player who
  self-excludes and re-registers today is NOT blocked. **Fixed as a
  documentation correction** (this ADR's own Context/§9 sections, this
  progress entry, and `active-stage.md` now state this precisely; the
  cross-brand/cross-tenant tests' own doc comments now say explicitly
  they prove the mechanism, not a reachable end-to-end state) and
  **tracked as an explicit, human-visible open decision** in ADR `0026`'s
  "Carried-forward limitations" - no code fix closes the underlying gap
  in this stage, since doing so is a substantial future body of work
  (Person-resolution/KYC-driven identity matching), not a small patch.
- **Four P1s, all fixed with regression tests**: (1) a bet callback
  redelivered after it had already succeeded, and after the RG state it
  depended on later changed, could incorrectly report a fresh decline
  instead of its original success - violating the documented idempotent-
  replay contract, empirically reproduced and fixed with an early
  already-posted-transaction short-circuit in `postBet`; (2) the
  player-facing "my RG status" endpoint leaked a DIFFERENT tenant's
  confidential restriction `reason_code` to a player with accounts at two
  tenants, empirically reproduced and fixed with an added RLS tenant
  predicate (migration `0038`); (3) `EvaluateEligibility`'s own SQL relied
  solely on RLS/caller scope rather than an explicit predicate to exclude
  a different tenant's rows, fixed defensively; (4) `EvaluateEligibility`
  silently skipped brand-scoped restrictions if a future caller forgot to
  resolve `BrandID`, fixed by making it a required, validated parameter.
- Several P2s fixed (a stale doc comment two reviewers independently
  flagged, an RLS UPDATE/DELETE visibility-scope hardening, a composite FK
  addition) and several P2s explicitly recorded as accepted/deferred
  (documented in ADR `0026`'s own findings section, not silently dropped).
- Untested `duration_days` at every layer, and several HTTP-layer
  authorization/validation edge cases, were found by the adversarial
  testing review and closed with new tests (Go-level duration parameter
  tests, HTTP-level duration/scope validation, `RolePlayer`/`RoleFinance`
  token denial, cross-tenant admin read returning 404).

Full detail for every finding, including the P2s explicitly accepted
rather than fixed and why, is in ADR `0026`'s own "Specialist review
findings and fixes" section.

### Verification performed

`gofmt -l .` clean. `go build ./...`, `go vet ./...`, `go vet
-tags=integration ./...` clean. `go test ./...`, `go test -tags=integration
./...`, and `go test -race -tags=integration ./...` all pass across the
full repository. Migrations `0037` and `0038` both round-tripped (`up` →
`down` → `up`) cleanly on a database already carrying prior stages' own
test data. New concurrency tests for all three directive-required race
scenarios pass repeatably under `-race` against real Postgres.

### Not claimed

Per CLAUDE.md's "No fake completion" rule: this stage does NOT claim the
cross-brand/cross-tenant self-exclusion PROTECTION is effective against a
real player re-registering today (see the P0 finding above - the
enforcement MECHANISM is correct and tested; the identity-linkage
precondition it depends on does not exist yet). It does NOT claim KYC/AML
implementation, player-level jurisdiction restriction, deposit/loss/
wagering/session limits, reality checks, or time-outs/cooling-off -
all remain documented extension points, not implemented (ADR `0026`
§14/§15). It does NOT claim a platform-wide staff-initiated restriction
capability (ADR `0026` §4). It does NOT claim regulatory certification or
production readiness.

## Next stage

Not started; requires explicit human authorization per the stage-gate
rule in `CLAUDE.md`. Candidates named in the Stage 3D/4A/4D-RG directives
(a real casino provider integration, sportsbook, bonus, B2C frontend,
partner console, production deployment, real PSP integrations, real
crypto integrations, cross-brand Person resolution) do not begin
automatically.
