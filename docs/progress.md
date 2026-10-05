# Project Progress

Last updated: 2026-09-27; see the 2026-10-05 PRH-2 status note below (Stage 10.3 accepted; post-acceptance close-out done; awaiting human authorization)

**Status note 2026-10-05 (PRH-2 implementation state; the notes below are older).** PRH-2 workstreams merged to this branch since the 2026-09-28 planning gate: A, B, C, D1, D2, E1, E2, E3, F-kyc, F-pay, G1, H, I-core, I-wire, J, K1, K2 and K3 (git log `merge(prh2)` commits; per-merge detail is in the task-registry `PRH-2-*-MERGE-STATE` rows). This includes D1 (poll amount/reference evidence), D2 (reconciliation parked capture), F-pay (KYC gate on deposit/payout), H (payments sweeper process), I-wire (alert dispatcher loop, log sink only), E1 (KYC submission outbox, migration 0114) and K3 (payment force resolution M1/M2, migration 0115). Migrations 0108-0115 were added (0108 A, 0109 G1, 0110 I-core, 0111 B, 0112 K1, 0113 K2, 0114 E1, 0115 K3). Everything is implemented against MOCK providers only (PROVIDER DEPENDENT for real vendors); AWS is OFF. All verification is LOCAL; there is NO GitHub CI evidence (CI-BILLING-1 open). The final-gate timing lane (TEST-RESISO-RACE-1) is NOT GREEN: `TestResolutionIsolation_NormalOperation` failed 0/5 and 0/3 on an idle box (`docs/plans/payment-readiness/evidence/prh2-final-timing-lane.md`); thresholds are unchanged and a decision is needed. The final PRH-2 gate is PENDING owner authorization: this note does NOT say the stage is complete and nothing here is approved. ALERT-DELIVERY-1 stays OPEN (no one is paged). Current launch blockers and open human decisions: `docs/HANDOVER.md` sections "Launch blockers (current)" and "Human decisions awaiting the owner".

## Status: Stage 10.3 ACCEPTED AS COMPLETE by the human (2026-09-26). Post-acceptance close-out done: F-POOL-1 fixed (ADR 0094) and CLOSED WITH CONDITIONS — final condition K1 (green GitHub CI timing lane) outstanding because GitHub Actions jobs are not starting (billing, CI-BILLING-1); hygiene reconciled; next real-provider planning gate prepared (`docs/plans/next-real-provider-integration-planning-gate.md`). MOCK providers / local backends only; AWS staging OFF; no production or real-provider readiness claimed. Next stage NOT started — awaiting human authorization.

*Status note 2026-09-26: the "(approved-pending)" labels on the Stage 3C, 3D, 4A, 4D-RG, 4E, 4F, 4G, 4G-FINAL, 4H-B0-R7 and Stage 7 headers below are historical, recorded at the time each stage stopped for review. Later stages proceeded on explicit human authorization recorded in the subsequent stage sections (for example Stage 4H-B1 Wave 2 "human-authorized", Stage 9.1 "authorized by the human", and ADRs 0087/0090/0091 for Stages 10, 10.1 and 10.2). The headers are left unedited. Source: `docs/plans/stage-10.3-planning/00-roadmap-reconciliation.md` "Discrepancies" item 12.*

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

## Stage 4E — Person Resolution & Cross-Brand Identity Foundation — complete (approved-pending)

Status: **Complete, pending human approval to authorize the next stage.**
Issued because the human reviewing Stage 4D-RG did NOT approve it for
progression: Stage 4D-RG's own specialist review had found a P0 -
`internal/identity.RegisterPlayer` created a brand-new, unlinked `Person`
on every registration, so a Person self-excluded via one brand could
register again as a "new" Person at another brand and evade restriction.
The Stage 4D-RG self-exclusion mechanism itself (platform-wide
`player_restrictions`, `EvaluateEligibility`) was already correct; the
missing capability was reliable Person resolution. This stage builds
exactly that. No real KYC/AML integration, no production identity
document collection, no biometric data, no AML transaction monitoring,
and no jurisdiction-specific identity rule were in scope.

### What was built

1. **`internal/identityresolution` package** - a provider-neutral
   `PersonResolver` interface (`Match`/`NoMatch`/`Uncertain`/unavailable,
   mirroring `CasinoProvider`/`PaymentProvider`'s exact shape) and
   `RegisterPlayerWithResolution`, the ONE registration entry point every
   caller now uses - it consults the resolver BEFORE ever creating a
   Person, dispatching to link an existing Person, create a brand-new
   one, or land the account in a safe review state.
2. **No second identity model** - `internal/identity`'s `RegisterPlayer`
   was split into three entry points (`RegisterPlayer`,
   `RegisterPlayerLinkedToPerson`, `RegisterPlayerPendingReview`) sharing
   one `insertPlayerAccount` helper; `Person`/`PlayerAccount`/`Brand`/
   `Tenant` are otherwise completely unchanged.
3. **One new status, zero new RG code** - migration `0039` adds
   `identity_review_required` to the EXISTING `PlayerAccountStatus` enum.
   `internal/rg.EvaluateEligibility`'s pre-existing `status != active`
   check already denies it - the Uncertain/resolver-unavailable safe path
   needed no changes to Stage 4D-RG's own code at all.
4. **`MockPersonResolver`** - the only implementation shipped, an
   exact-match lookup table a test configures, mirroring
   `MockCasinoProvider`/`MockPaymentProvider`'s identical convention. A
   real vendor slots in behind the same interface later.
5. **New admin capability** - `POST /v1/admin/players/{id}/identity-
   review/clear`, gated by a new `identity_review:manage` permission
   (RoleCompliance only, mirroring `rg_restriction:write`'s separation-
   of-duties precedent), using an atomic conditional status transition
   (`identity.SetPlayerAccountStatusIfCurrent`) rather than a read-then-
   write, closing a TOCTOU the security review found in an earlier draft.
6. **Cross-brand/cross-tenant proof** - unlike Stage 4D-RG (which could
   only prove the mechanism was correct, not that it was reachable),
   Stage 4E's own integration test
   (`TestCrossBrandSelfExclusion_ResolvedPersonCannotEvadeViaSecondBrand`)
   proves, end to end against real Postgres, that a Person self-excluded
   via a Brand A account is denied (`CodeSelfExcluded` specifically) via
   a Brand B/Tenant B account once identity resolution correctly links
   them - using `EvaluateEligibility` completely unchanged.

Full design, rationale, every recorded open decision, and the complete
specialist-review findings/fixes list:
`docs/decisions/0027-person-resolution-and-cross-brand-identity-
foundation.md`. Updated architecture doc:
`docs/architecture/05-identity-architecture.md`'s "Implementation status
(Stage 4E)" section.

### The honest limit of what this stage closes

Every registration reachable over the live HTTP API resolves `NoMatch`
today, because no trusted source of verified identity evidence
(`VerifiedAttributes`) is wired into the register handler - no real
KYC/identity-verification vendor is integrated (explicitly out of scope,
per the governing directive's §3). The cross-brand evasion-prevention
mechanism is proven correct at the package/integration-test level against
a resolver a real vendor integration will replace; it is **not yet
actively preventing evasion for a real player** over the live API. This
is stated plainly, not buried, per CLAUDE.md's "no fake completion" rule
- see ADR `0027`'s own "Carried-forward limitations" section.

### Specialist review: zero P0s, four P1s found and fixed

An independent 7-specialist parallel review (identity architecture,
security/privacy, RG/self-exclusion integration, PostgreSQL/RLS, multi-
tenancy, API/HTTP, adversarial testing) found:

1. **P1 - TOCTOU**: the identity-review-clear handler's original read-
   then-write could be raced by a concurrent status change (e.g. a
   suspend), silently reactivating a since-suspended account. **Fixed**:
   a single atomic conditional `UPDATE`
   (`identity.SetPlayerAccountStatusIfCurrent`), plus a regression test.
2. **P1 - inaccurate documentation**: this ADR and a code comment
   incorrectly claimed a review-required account "can log in" - the
   existing login handler denies it exactly like a suspended account.
   **Fixed**: corrected in both places.
3. **P1 - a test overstated its own proof**: the concurrency test's name
   implied it closed the hardest possible race (two simultaneous
   first-time registrations for one real, still-unregistered person); it
   actually proves the narrower (still valuable) guarantee that
   concurrent registrations resolving to an ALREADY-EXISTING person never
   duplicate it. **Fixed**: rewrote the test's doc comment to state
   precisely what is and is not proven.
4. **P1 - missing test coverage**: no test exercised idempotency through
   the ACTUAL dispatch path (`RegisterPlayerWithResolution`, not the
   older `RegisterPlayer` directly), and no test proved cross-tenant
   identity-review-clearing is denied. **Fixed**: three new tests added
   (duplicate-email through the dispatch path, genuine concurrent
   duplicate-email registration, cross-tenant clearing denial).

Two further P2s were fixed (audit records for resolver failures were
hardcoded to `OutcomeSuccess`, hiding outages from an audit scan;
resolution audit records carried no `RequestID`/`IPAddress`/`UserAgent`
unlike every sibling audit record). Several more P2s were explicitly
recorded as accepted/deferred with reasoning (a benign orphan-Person row
from a losing registration race, inherited unchanged from Stage 2;
`Raw`/`Reason` fields trusted by doc comment rather than code for a
resolver that doesn't exist yet to misuse them; a false-positive Match's
trust boundary; migration lock/down-migration notes; a pre-existing
ADR-0026 cross-tenant restriction-metadata visibility now reachable in
practice; no per-tenant opt-in/opt-out for resolution; CI's `go test`
steps not passing `-race`) - full list, with reasoning for each, in ADR
`0027`'s own findings section.

### Newly discovered during this stage's validation, NOT introduced by it

`internal/casino`'s pre-existing `TestConcurrent_
DuplicateBetDeliveryDuringSelfExclusion` (Stage 4D-RG) is intermittently
flaky under `go test -race -tags=integration` - observed failing roughly
1 run in 3 in this session, reporting the two concurrent bet-delivery
goroutines disagreeing about whether the bet posted while self-exclusion
was concurrently being applied. No file this stage touches is on that
code path; this is a genuine, pre-existing race requiring `casino`/
`ledger-finance`/`identity-compliance` specialist attention in a future
stage, not fixed here (out of this stage's identity-resolution scope).
Recorded in ADR `0027`'s own "Carried-forward limitations."

### Verification performed

`gofmt -l .` clean. `go build ./...` clean. `go vet -tags=integration
./...` clean. `go test ./...`, `go test -tags=integration ./...`, `go test
-race ./...` all pass cleanly across the full repository. `go test -race
-tags=integration ./...` passes except for the pre-existing,
intermittently flaky casino test noted above (confirmed via repeated runs
to be pre-existing and unrelated to any file this stage touches, not a
regression). Migration `0039` round-tripped (`up` → `down` → `up`)
cleanly on a database already carrying prior stages' own test data before
this stage's tests populated the new status value (the down migration is
a documented one-way door once rows use the new status - verified live).

### Not claimed

Per CLAUDE.md's "No fake completion" rule: this stage does NOT claim
cross-brand self-exclusion evasion is actually prevented for a real
player over the live HTTP API today (see "The honest limit" above - the
mechanism and orchestration are correct and tested; no real KYC evidence
source exists yet to feed it). It does NOT claim full KYC/AML, real
identity-document verification, biometric data handling, AML transaction
monitoring, or jurisdiction-specific identity rules (directive §3). It
does NOT claim automatic reconciliation of pre-existing duplicate Persons
(directive §14 - explicitly not attempted). It does NOT claim regulatory
certification or production readiness.

## Stage 4F — Player Verification, Documents & Authentication Foundation — complete (approved-pending)

Builds the platform-owned KYC/identity-verification state model
(`kyc_verifications`/`kyc_documents`, migration `0040`), a document
storage/security boundary, a provider-neutral `KYCProvider` abstraction,
and email-verification/password-reset authentication flows. No real
KYC/AML vendor is selected or integrated (directive §1's explicit
non-goal) - `MockKYCProvider` is the only implementation, with its own
invented callback format, never modeled on a real vendor's API.

### What was built

- `kyc_verifications`/`kyc_documents` (migration `0040`) - tenant-owned,
  RLS-enforced, hanging off the existing Person/PlayerAccount/Tenant/
  Brand model via foreign keys. `kyc_verifications.status` is a fully
  independent 6-state machine, never written to or read from
  `PlayerAccountStatus`. `kyc_documents` is versioned and
  database-trigger-immutable (only status/reviewed_at/reviewed_by/
  rejection_reason may ever change; DELETE/TRUNCATE refused outright).
- `internal/kyc` package: `KYCProvider`/`Orchestrator`/`MockKYCProvider`,
  `DocumentStorageProvider`/`MockDocumentStorageProvider`,
  `MalwareScanner`/`MockMalwareScanner`, `ValidateUpload` (size/
  content-sniffing/extension-consistency/filename-sanitization),
  `CreateVerification`/`ReviewVerification`, `UploadDocument`/
  `ReviewDocument`/`GetDocumentContent` (every content read audited).
- `internal/auth/credential_token.go`: unified, purpose-discriminated
  `player_credential_tokens` (email_verification | password_reset),
  `IssueCredentialToken`/`CountRecentCredentialTokens`/
  `ValidateAndConsumeCredentialToken` - the atomic two-phase lookup-then-
  consume entry point every confirm handler uses.
- `internal/email` package: `Provider`/`MockProvider` - no production
  SMTP/API dependency introduced.
- `internal/httpserver/credential_handlers.go` +
  `kyc_handlers.go`/`kyc_admin_handlers.go`: player self-service
  (email-verification request/confirm, password-reset request/confirm,
  document upload/listing), compliance review (verification/document
  approve/reject), and the provider webhook endpoint, gated by two new
  permissions (`PermVerificationRead`: Compliance + TenantAdmin;
  `PermVerificationReview`: Compliance only).
- `docs/decisions/0028` (verification model/provider abstraction),
  `0029` (document storage/security/privacy), `0030` (email verification/
  password reset) - full rationale, including every recorded OPEN
  DECISION.

### Specialist review: findings and fixes

A 7-area independent review (identity architecture, KYC provider
architecture, security/privacy, document security, PostgreSQL/RLS,
API/RBAC, adversarial testing) found and fixed:

1. **P1 - `SubmitVerification`/`GetVerification` dead code** - declared
   on `KYCProvider` but never called. **Fixed**: wired into
   `UploadDocument` via a new `submitVerificationDocuments` helper - each
   upload resubmits the verification's current full document set.
2. **P1 - password hashed before token validation** in the password-
   reset confirm handler, an uncosted Argon2id DoS surface on an
   unauthenticated route. **Fixed**: hashing moved inside
   `ValidateAndConsumeCredentialToken`'s callback.
3. **P1 (PostgreSQL/RLS) - non-composite tenant FKs** on
   `kyc_verifications.player_account_id`, `player_credential_tokens.
   player_account_id`, `kyc_documents.verification_id`/`reviewed_by` -
   proven exploitable via direct SQL (a mislabelled cross-tenant row
   inserted successfully). **Fixed**: composite `(child_id, tenant_id)
   REFERENCES parent(id, tenant_id)` FKs throughout, backed by new
   `UNIQUE (id, tenant_id)` on `player_accounts`/`staff_users`/
   `kyc_verifications` (mirroring `brands`' own precedent, migration
   0008). Regression test:
   `TestKYCVerifications_CompositeTenantFKRejectsCrossTenantPlayerAccount`.
4. **P1 (PostgreSQL/RLS) - `kyc_verifications`/`player_credential_tokens`
   had zero append-only protection** - proven via direct SQL `DELETE`
   against a rejected verification/live token succeeding. **Fixed**:
   `BEFORE DELETE`/`BEFORE TRUNCATE` triggers on both tables (status
   mutation itself remains a normal, allowed UPDATE).
5. **P0 (adversarial) - concurrent double-confirm of a credential token
   was untested under real concurrency** (only sequential replay was
   proven). **Fixed**: `TestPasswordReset_ConcurrentDoubleConfirmAppliesExactlyOnce`
   (8 goroutines, `-race`) proves exactly one of many simultaneous
   confirms succeeds and exactly one row is ever consumed.
6. **P1 - `MalwareScanner`'s fail-closed contract untested** (the mock
   can never itself error). **Fixed**:
   `TestUploadDocument_FailsClosedOnScannerError` with a real
   error-returning scanner stub proves the upload is refused and nothing
   is stored.
7. **P2 (PostgreSQL/RLS) - globally-unique `provider_reference`** - a
   cross-tenant existence oracle and future B2B collision/denial risk.
   **Fixed**: rescoped the unique index to
   `(tenant_id, provider_id, provider_reference)`.
8. **P2 (PostgreSQL/RLS) - `token_lookup` policy lacked the
   tenant-unset conjunct** migration 0018 already established for the
   identical session-lookup pattern. **Fixed**: added.
9. Added a direct audit-trail assertion test
   (`TestKYC_ActionsProduceAuditRecords`) confirming
   `kyc.verification_submitted`/`kyc.document_uploaded`/
   `kyc.document_reviewed`/`kyc.document_accessed` actually produce
   `audit_log` rows, not merely return success from the Go call.

P2s explicitly recorded as accepted/deferred with reasoning (not fixed
this stage, not silently dropped): `kyc_documents` review decisions
(status/reviewed_by/reviewed_at) remain revertible in place; no per-IP/
global rate limit on credential-token endpoints; no upload-rate-limit on
`/v1/me/kyc/documents`; a password-reset timing side-channel (email sent
synchronously only on the "found" path); `ParseMultipartForm` may spill
to the OS temp directory before validation runs (narrows this package's
own "never touches disk" claim to the stored-content path specifically);
no retention/purge policy for `player_credential_tokens.requested_ip`
(PII); an access-JWT remains valid up to 15 minutes post-reset;
`IssuingCountry` is unvalidated free text; denied/cross-owner document-
access attempts are not themselves audited (only successful accesses
are); the app's own Postgres role owns these tables and could in
principle disable its own RLS/triggers (a pre-existing, platform-wide
limitation, not new to this stage) - full list with reasoning in ADR
0028/0029/0030's own findings sections.

### Verification performed

`gofmt -l .` clean. `go build ./...` clean. `go vet -tags=integration
./...` clean. `go test ./...`, `go test -race ./...`, and `go test
-tags=integration ./...` all pass cleanly across the full repository. `go
test -race -tags=integration ./...` passes except for the pre-existing,
already-documented `TestConcurrent_DuplicateBetDeliveryDuringSelfExclusion`
flake (Stage 4D-RG/4E, unrelated to this stage's own files, not fixed
here). Migration `0040` round-tripped (`up` -> `down` -> `up`, twice)
cleanly against the live dev database, including after the composite-FK/
append-only-trigger fixes.

### Not claimed

Per CLAUDE.md's "No fake completion" rule: this stage does NOT claim a
real KYC/AML vendor is integrated, or that document verification actually
happens against a real identity-verification service (directive §1's
explicit non-goal - `MockKYCProvider` only). It does NOT claim production-
grade document storage (encryption at rest, real malware scanning,
signed/short-lived access URLs, retention/legal-hold tooling are all
documented future requirements, not built - ADR 0029 §3/§6). It does NOT
claim cross-brand real-player protection is active (unchanged from Stage
4E - this stage does not wire KYC evidence into `PersonResolver`). It
does NOT claim regulatory certification or production readiness.

## Stage 4G — Project Orchestration Governance + Risk & Limits Engine — complete (approved-pending)

Two parts: (A) permanent multi-agent orchestration governance
(`docs/governance/*`), and (B) the platform's first central Risk & Limits
engine (`internal/risk`) - ONE reusable rule/policy architecture,
consumed by domain-specific integration points, never a separate limit
engine per product.

### Part A — Governance

`docs/governance/agent-registry.md`/`ownership.md`/
`integration-protocol.md`/`change-control.md`/`task-registry.md`/
`project-status.md` - permanent process documents every future stage
operates under. `.claude/agents/risk.md` - a new specialist definition.
The Master Orchestrator role formalizes this session's own established
working pattern (direct implementation for cross-cutting work, parallel
independent specialist review at stage end) rather than introducing an
untested multi-agent code-writing pipeline.

### Part B — Risk & Limits engine

- `risk.Evaluate(ctx, tx, RiskRequest) -> (RiskDecision, error)` - the
  single decision boundary, `Outcome` exactly `allow`/`deny`/`review`,
  fail-closed (any error MUST be treated as deny).
- `risk_rules` (migration 0041) - dual-scope (platform-wide/tenant-
  owned, mirroring `player_restrictions`), HARD_LIMIT/CONFIGURABLE_LIMIT/
  RISK_SIGNAL kinds, deterministic precedence.
- Limit kinds implemented: `min_amount`/`max_amount`/`cumulative_amount`
  only (`count`/`velocity`/`exposure`/`loss` are documented future
  extensions, not database-configurable yet).
- Enforcement wired into `internal/casino`'s `LaunchGame` (real-mode
  only) and `postBet` (after RG eligibility, before the balance lock) -
  the only two of six designed operations actually enforced this stage.
- Risk and Responsible Gaming kept strictly separate domains -
  `internal/rg.EvaluateEligibility` remains the sole self-exclusion
  authority; both consulted in a fixed order (RG first, always
  short-circuiting Risk on denial).
- New `risk_manager` StaffRole/Role, `risk_config:read`/`risk_config:manage`
  permissions, mirroring Stage 4F's verification-permission
  separation-of-duties precedent exactly (including the identical
  self-escalation guard in staff creation).

Full design: `docs/decisions/0031-risk-and-limits-engine.md`.

### Specialist review: 9 areas, real P0/P1s found and fixed

A 9-area independent parallel review (risk architecture, financial
correctness, casino integration, RG integration/domain separation,
security, PostgreSQL/RLS, API, adversarial testing, multi-tenancy) found
and this stage fixed:

1. **P1 (independently found by 4 reviewers) - specificity scoring bug**:
   a rule scoped by `(tenant)` and one scoped by `(tenant, asset_code)`
   were scored as an ARTIFICIAL TIE, forcing `ErrConflictingRules` - a
   fail-closed outage of the entire operation for that tenant, for rules
   never actually in conflict. **Fixed**: rescored as a bitmask summing
   every present scope dimension, not just the single highest-ranked one.
2. **P1 - non-deterministic deny-vs-review aggregation**: a Go map's
   randomized iteration order could let a later REVIEW-action breach
   silently downgrade an earlier DENY-action breach across independent
   configurable-rule groups. **Fixed**: deny-priority merge,
   order-independent by construction.
3. **P1 (independently found by 2 reviewers) - int64 overflow in the
   cumulative-amount check**: summing many `NUMERIC(38,0)` ledger entries
   into a plain `int64` could silently wrap negative for an 18-exponent
   asset, failing OPEN exactly where fail-closed matters most. **Fixed**:
   scanned as `pgtype.Numeric`, compared via `math/big`.
4. **P1 - rolled-back bets permanently consumed cumulative capacity**:
   the query counted a bet's debit even after `casino_rollback` reversed
   it. **Fixed**: nets debits minus credits across both transaction
   types.
5. **P1 (live-database confirmed) - `risk_rules` RLS missing the
   player-scope guard**: a `db.WithPlayerScope` connection could read
   every tenant's risk rules and successfully disable one - no
   player-facing code path reaches this today (defense-in-depth, not a
   live incident). **Fixed**: added the guard to all four RLS policies,
   mirroring `player_restrictions`/`sessions`' own established pattern.
6. **P0 (adversarial) - two real code paths had zero test coverage**:
   `RuleRiskSignal` contributing to `REVIEW`, and a `HARD_LIMIT` rule with
   `action=review`. **Fixed** with new tests proving both actually work.
7. **P0/P1 (adversarial) - missing integration-level tests**: `Evaluate`
   itself skipping an out-of-window rule (only a unit test of the helper
   existed), and cross-tenant denial of the DISABLE HTTP endpoint. Both
   added.
8. **P1 (self-referential) - ADR 0031 and this progress/active-stage
   entry did not exist** at the time code/migration comments already
   cited them. Closed by their own existence.

P2s fixed: OpenAPI documentation for the three new risk endpoints; audit
records for rule creation/disabling now carry IP/user-agent/request-id;
HTTP-layer enum validation for every rule field (was previously
unvalidated free text hitting DB CHECK/500s); risk-denial audit metadata
enriched with provider/game/asset/amount; the down migration documents
the `risk_manager`-row one-way-door. Several more P2s explicitly recorded
as accepted/deferred with reasoning (not fixed this stage, not silently
dropped) - full list with reasoning in ADR 0031's own findings section:
`CreateRule` does not cross-validate limit_kind/operation compatibility;
an oversized threshold inserted directly via SQL can brick evaluation for
a tenant+operation (consistent with the codebase's pre-existing int64-
minor-units convention); no uniqueness constraint on `risk_rules`; GET
`/v1/admin/risk/rules` has no pagination or default disabled-row filter;
`risk_signal` rules force `action=review` server-side with no HTTP-layer
feedback if the caller sent `action=deny`; no FK from `risk_rules.
tenant_id` to `tenants(id)` (matching `player_restrictions`' own
pre-existing gap); the immutability trigger permits re-enabling a
disabled rule and clearing `effective_until` with no dedicated audited Go
path for either.

### Newly disclosed limitations (not defects)

Jurisdiction-scoped rules are reachable only from `LaunchGame`, never
`postBet` (`casino_launch_sessions` does not persist launch-time
jurisdiction - the same pre-existing "TODO(jurisdiction)" gap named
elsewhere in this codebase). No licence-mode scoping dimension exists yet
- a platform-wide HARD_LIMIT would apply identically inside a future
bring-your-own-licence tenant under a different jurisdiction's own legal
regime; no such tenant exists yet, recorded before one does. No role can
create a genuinely platform-wide rule via HTTP this stage (mirrors
`internal/rg.CreateStaffRestriction`'s identical, already-established
precedent).

### Verification performed

`gofmt -l .` clean. `go build ./...` clean. `go vet -tags=integration
./...` clean. `go test ./...`, `go test -race ./...`, `go test
-tags=integration ./...` all pass cleanly across the full repository. `go
test -race -tags=integration ./...` passes except for the pre-existing,
already-documented `TestConcurrent_DuplicateBetDeliveryDuringSelfExclusion`
flake (Stage 4D-RG/4E, unrelated to this stage, confirmed via repeated
isolated re-runs to still be intermittent and pre-existing, not a
regression). Migration `0041` round-tripped (`up` -> `down` -> `up`)
cleanly against the live dev database, including after the RLS
player-scope-guard fix.

### Not claimed

Per CLAUDE.md's "No fake completion" rule: this stage does NOT claim
jurisdiction-scoped risk rules are enforceable at bet time (only at
launch time - see "Newly disclosed limitations" above). It does NOT
claim a platform-wide hard limit is safe for a hybrid-licensing (bring-
your-own-licence) tenant under a different jurisdiction - no such tenant
exists yet, and this is recorded as an open decision, not resolved. It
does NOT claim `deposit`/`withdrawal`/`sportsbook_bet`/`bonus_grant` risk
enforcement exists - only casino_launch/casino_bet are wired, though an
operator can configure rules for the other four (they are simply never
evaluated). It does NOT claim this closes the Bonus Engine's own need for
a risk/limits foundation - directive §32 explicitly blocks Bonus until
this architecture is proven stable, which is a human decision, not
self-certified here.

## Stage 4G-FINAL — Architectural Hardening & Final Gate — complete (approved-pending)

Explicitly NOT a business-functionality stage - the directive's own
objective was "harden Stage 4G so the platform core is genuinely
extensible, governed, and safe to build future domains on top of." No
new domain, no new business capability; every change either makes the
Stage 4G governance model operational or closes a gap Stage 4G's own
completion report already disclosed.

### Part A — Governance made operational

`docs/governance/agent-registry.md` gained an explicit "Absolute
constraint on every specialist" (no silent cross-domain edits, no
self-assigned scope, no self-reviewed work) and a concrete "How the
Orchestrator assigns every task to an owner" procedure. `task-registry.md`
gained two new permanent, append-only, cross-stage tables: the
**Dependency Request Log** (every cross-domain need, filed/resolved) and
the **Integration Approval Log** (every capability's actual integration
sign-off, citing the tests/reviews it rests on) - the concrete answer to
"how are dependency requests/integration approvals recorded," not just
prose. `ownership.md`'s same-file-conflict rule now points at these
logs. `change-control.md` gained a rule requiring the new PASS/FAIL/
FLAKE/NOT RUN/BLOCKED test-reporting standard (`docs/testing/
testing-strategy.md`, new section) in every future completion report.

### Part B — Risk Engine contract finalized, not redesigned

`docs/decisions/0031-risk-and-limits-engine.md` gained §9-§13: the
jurisdiction-context contract, the licensing-mode contract, an explicit
statement that `REVIEW` stays a distinct outcome from `DENY` in the
domain model (only today's two enforcement points collapse them, as an
enforcement-point choice, not an architecture limitation), the concrete
three-step extension model for any future `LimitKind`, and a table
mapping every future domain (deposit/withdrawal/sportsbook_bet/
bonus_grant) to its Risk-integration obligation. `risk.Evaluate`'s
signature, the rule table's shape, and every Stage 4G decision are
otherwise unchanged.

### Part C — Jurisdiction context gap structurally closed (PARTIALLY IMPLEMENTED)

Stage 4G's own completion report disclosed: "jurisdiction-scoped rules
are reachable only from `LaunchGame`, never from `postBet`." Migration
`0042_jurisdiction_and_licensing_context` adds
`casino_launch_sessions.jurisdiction_code`, populated once at launch time
(denormalized exactly like `provider_game_id`/`asset_code` already were)
and read back by `postBet` for every subsequent bet in that round. No
geolocation vendor invented - the same `TODO(jurisdiction)` root cause
(no per-player jurisdiction resolver exists yet) is unchanged; this
closes only the "resolved-but-then-discarded" gap. **Labeled PARTIALLY
IMPLEMENTED, not IMPLEMENTED**: `internal/httpserver/casino_handlers.go`'s
real `LaunchGame` HTTP call site does not populate `LaunchGameParams.
JurisdictionCode`, so every production launch persists a NULL
jurisdiction today - the wiring is proven correct only by tests that
populate it directly, not exercised by any real request path yet.
Regression tests:
`TestReceiveCallback_BetDeniedByJurisdictionScopedRiskRuleViaLaunchSession`,
`TestReceiveCallback_JurisdictionScopedRiskRuleDoesNotDenyADifferentJurisdiction`,
`TestReceiveCallback_SessionWithNoJurisdictionDoesNotMatchJurisdictionScopedRule`.

### Part D — Licensing-mode scoping added

`internal/risk` gained a new optional scope dimension, `LicensingMode`
(`Rule`/`RiskRequest`), mirroring `tenants.licensing_model`'s existing
two values (`under_platform_licence`/`own_licence` - ADR 0006, not a new
taxonomy). Resolved server-side by the caller via a new
`internal/identity.GetTenantByID` + `internal/casino`'s new
`resolveLicensingMode` helper, exactly like every other identity field on
`RiskRequest` already is - `risk.Evaluate` never looks it up itself. Lets
a platform-wide `HARD_LIMIT` expressing the PLATFORM's OWN licence's
legal ceiling avoid accidentally binding a future bring-your-own-licence
tenant operating under a different legal regime. No BYOL tenant onboarded
- this is the contract a future one will rely on. Regression test:
`TestEvaluate_LicensingModeScopedHardLimitNeverBindsADifferentLicensingMode`.

### Part F — Root-caused and fixed the carried-forward flake (two distinct bugs, both fixed)

`TestConcurrent_DuplicateBetDeliveryDuringSelfExclusion` (intermittent
since Stage 4D-RG/4E) was investigated to a real mechanism, not merely
re-labeled "known flake." Investigation surfaced **two separate, now-fixed**
issues, not one:

1. **Casino-level race (the originally suspected cause)**: `postBet`'s
   idempotency short-circuit only reliably serializes SEQUENTIAL
   redeliveries of the same `provider_tx_id` - two GENUINELY CONCURRENT
   deliveries could each start before the other committed, both see
   "not yet posted," and independently re-evaluate live RG state. If a
   self-exclusion became effective between the two evaluations, the two
   deliveries could report DIFFERENT outcomes for the identical bet, even
   though the ledger itself never posted more than once
   (`SUM(debits)==SUM(credits)` always held - this was a
   result-consistency gap, not a financial-integrity violation).
   **Fixed**: a `pg_advisory_xact_lock` scoped to
   `(tenant_id, provider_id, provider_tx_id)` (tenant-scoped after
   independent review by architect, security, and multi-tenancy all
   flagged the initial un-tenant-scoped key as a cross-tenant
   contention/timing risk), acquired before the idempotency check,
   forces a truly-concurrent second delivery to wait for the first's
   transaction to fully commit or roll back before re-reading.

2. **A second, deeper, previously-undiscovered bug in `internal/rg`
   (found only because this stage's adversarial review went beyond the
   directive's minimum and widened the shipped 2-delivery regression
   test to N=8 concurrent deliveries across 5 iterations
   (`internal/casino/adversarial_lock_stress_test.go`,
   `TestConcurrentStress_ManyDuplicateBetDeliveriesDuringSelfExclusion`,
   authored by the QA specialist)**: even after fix (1) above, this wider
   test still failed intermittently (~50% of full-repo
   `-race -tags=integration` runs) with theoretically-impossible
   divergent outcomes. Root-caused via temporary diagnostic
   instrumentation to `internal/rg.EvaluateEligibility`'s self-exclusion
   query using Postgres `now()`, which is STABLE per transaction (frozen
   at transaction start, never re-evaluated per statement) rather than
   `clock_timestamp()` (re-evaluated on every call). A transaction forced
   to wait on `rg.lockPerson`'s advisory lock - made significantly more
   likely by fix (1)'s own new queuing behavior - could fail to see a
   self-exclusion that had ALREADY committed by the time its query
   actually ran, because its `now()` was frozen at an earlier instant.
   This is a genuine, pre-existing correctness bug in the self-exclusion
   enforcement path, not a test artifact, and not something this stage's
   directive anticipated finding. **Fixed** by replacing `now()` with
   `clock_timestamp()` in both time-window comparisons in
   `internal/rg/rg.go`'s `EvaluateEligibility`. Verified two ways: (a) a
   new deterministic regression test,
   `TestEvaluateEligibility_DetectsSelfExclusionCommittedAfterTransactionBegan`
   (`internal/rg/rg_integration_test.go`), which manually opens a
   long-lived transaction, commits a self-exclusion from a separate
   transaction, then asserts the original transaction's
   `EvaluateEligibility` correctly detects it - passed 5/5 with `-race`;
   (b) repeated full-repo `go test -race -tags=integration -count=1 ./...`
   runs (5 total, including the wide N=8 stress test) with zero
   failures, versus the ~50% failure rate observed before this fix.
   Reverting the fix to confirm a red-before-fix baseline was attempted
   and explicitly declined by the Claude Code auto-mode safety
   classifier ("[Security Weaken]"); the fix's correctness rests instead
   on the mechanism proof above plus the empirical live-reproduction
   evidence already captured during investigation, not a reverted-and-
   reproduced control run.

Both fixes verified together via 5 consecutive full-repository
`go test -race -tags=integration -count=1 ./...` runs plus 4 additional
targeted `-race -tags=integration` runs of `internal/casino` and
`internal/rg` alone (9/9 clean) - previously this combination flaked
within a single run roughly half the time.

### Parts E, H, I, G — documentation-only, no code change

- **E**: `REVIEW` semantics preserved as a distinct outcome, documented
  explicitly (ADR 0031 §11) so a future compliance workflow can be added
  without changing `risk.Evaluate`'s contract.
- **H**: the extension model for `count`/`velocity`/`exposure`/`loss`
  and product-specific limit kinds is now explicit (ADR 0031 §12) - five
  additive steps (migration, `types.go`, `evaluator.go`, the HTTP
  handler's own enum allowlist, and the OpenAPI enum - corrected from an
  initial three-step version after documentation review found it
  understated the real cost), never a redesign. None implemented this
  stage.
- **I**: cross-domain boundaries (Casino/Payments/Ledger/RG/Identity/
  KYC/Risk) verified by inspection, not just convention -
  `docs/architecture/02-domain-and-service-boundaries.md`'s new
  verification section; zero cross-imports found between `internal/rg`
  and `internal/risk`.
- **G**: `docs/testing/testing-strategy.md`'s new "Test reporting
  standard" - PASS/FAIL/FLAKE/NOT RUN/BLOCKED per suite, applied to this
  stage's own completion report.

### Specialist review: 10 of 11 areas completed independently this stage; Financial/Ledger completed as a follow-up gate (see Stage 4G-FINAL-FINANCE-GATE below)

Architecture, risk, casino, RG, security/RBAC (x2), PostgreSQL/RLS,
API/HTTP, adversarial testing, multi-tenancy, and documentation/
governance were each reviewed independently and reported findings. Per
`change-control.md`'s gate, all P0/P1 were fixed before this entry was
written; every P2 was either fixed or recorded here with reasoning - none
silently dropped.

**Financial/ledger review did NOT complete during this stage's original
run** - the dedicated `ledger-finance` specialist agent stalled after
roughly an hour (its own attempts to clean up leftover test data were
correctly denied twice by the Claude Code auto-mode safety classifier)
and was stopped without producing a findings report. The Orchestrator
performed a direct financial-correctness self-review in its place at the
time, explicitly recorded as self-review, not independent sign-off. **This
gap was closed as a follow-up "Stage 4G-FINAL-FINANCE-GATE" - see its own
entry immediately below for the actual independent `ledger-finance`
review, its PASS verdict, and its findings.**

**P1s found and fixed:**

- `casino_launch_sessions_enforce_immutable_fields()` was not updated for
  the new `jurisdiction_code` column (found independently by architect and
  PostgreSQL/RLS review) - left it mutable in place under
  `tenant_staff_scope`'s `FOR ALL` policy, letting a live session's
  jurisdiction be silently repointed mid-round. Fixed in migration 0042.
- The `postBet` advisory-lock key was not tenant-scoped (found
  independently by architect, security, and multi-tenancy review) -
  `(provider_id, provider_tx_id)` alone is only unique WITHIN a tenant,
  creating a cross-tenant contention/timing side-channel. Fixed by adding
  `tenant_id` as the key's first component.
- `docs/architecture/02-domain-and-service-boundaries.md` cited
  `DR-4GF-01`/`DR-4GF-02` for the `internal/identity.GetTenantByID`
  dependency; those rows actually cover the risk/casino jurisdiction
  dependency, not identity (found by architect and code-reviewer). Fixed
  by filing `DR-4GF-03` and correcting the citation. The same doc also
  claimed `internal/casino` is the only importer of both `rg` and `risk`;
  `internal/httpserver` also imports both, in separate files. Corrected
  to "only file that composes both decisions."
- ADR 0031 §12's `LimitKind` extension model listed only 3 steps
  (migration, `types.go`, `evaluator.go`), omitting the HTTP handler's
  `RequireOneOf` allowlist, the OpenAPI enum, and migration 0041's second
  CHECK constraint coupling `limit_kind` to valid `time_window` values
  (found by code-reviewer). Expanded to 5 steps.
- `integration-protocol.md` self-contradicted on whether integration is
  approved after steps "3-6" or "1-6" (found by code-reviewer). Corrected
  to 1-6 throughout.
- `task-registry.md`'s Stage 4G-FINAL rows had incomplete file-ownership
  lists (several touched files unaccounted for) and `orchestrator.go`
  claimed by two rows without reconciliation (found by code-reviewer).
  Fixed by expanding the lists and adding the one-actor-two-concerns note
  reproduced above this stage's task table.
- ADR 0031 §13's status table used non-CLAUDE.md wording ("Enforced" /
  "Designed, not wired") instead of the mandated 7-value vocabulary
  (found by code-reviewer, Part G). Fixed.
- `project-status.md`/`progress.md`/`active-stage.md` and ADR 0031 §13
  described the jurisdiction gap as "closed"/"reachable end to end"
  (found by multi-tenancy review) - true only structurally; no HTTP call
  site populates `LaunchGameParams.JurisdictionCode` yet. Relabeled
  `PARTIALLY IMPLEMENTED` with the caveat spelled out everywhere the claim
  appeared.
- The `internal/rg` `now()`/`clock_timestamp()` self-exclusion-enforcement
  bug itself (Part F above) - the single most significant finding this
  stage, surfaced by QA's adversarial stress test rather than the
  directive's own minimum scope.

**P2s - fixed this stage:**

- `risk.rule_created`'s audit metadata omitted `jurisdiction_code` and
  `licensing_mode` even though they are now part of a rule's identity
  (found by security review) - added to the metadata map in
  `internal/risk/policy_service.go`.
- `idx_risk_rules_licensing_mode` was a dead index (found by
  PostgreSQL/RLS review) - `listEffectiveRules` selects by `operation`
  alone and filters every other scope dimension, `licensing_mode`
  included, in application code; no query path ever filters by
  `licensing_mode` directly. Removed from migration 0042 before its first
  commit rather than shipping known-dead schema; round-tripped
  (`up`->`down`->`up`) clean afterward.

**P2s - recorded as accepted, not fixed, with reasoning:**

- **Tenant-created "dead" rule cross-validation gap** (found by risk
  review): nothing stops a tenant from creating a rule whose
  `jurisdiction_code`/`licensing_mode` combination can never match any
  request that tenant will ever send (e.g. a jurisdiction the tenant has
  no presence in). This is a configuration-safety nicety, not a
  correctness or security issue - a "dead" rule is safe by construction
  (it simply never matches; the fail-closed `ErrMissingLicensingMode`/
  `ErrMissingAmount` gates handle the dangerous direction, a rule that
  SHOULD bind but silently doesn't). Adding tenant-config-aware
  cross-validation at rule-creation time is new business logic a
  hardening-only stage should not add unprompted (CLAUDE.md's "no
  uncontrolled scope expansion") - deferred to whichever future stage
  builds the operator-facing rule-management UI, where surfacing this as
  a warning belongs naturally.
- `risk.rule_disabled`'s audit metadata remains thin (`operation` only,
  no `limit_kind`/`threshold`/scope dimensions) - this predates Stage
  4G-FINAL (Stage 4G's own original code) and was not made worse by this
  stage's changes; left as pre-existing debt rather than expanding this
  stage's diff to fix an unrelated function.

## Stage 4G-FINAL-FINANCE-GATE — Independent financial correctness sign-off — complete

Final-gate-only follow-up to Stage 4G-FINAL: obtained the one specialist
review that did not complete in that stage's original run. No business
functionality, no new limit kinds, no other domain work - per the
directive's own "no scope expansion" instruction, only a fix required to
resolve a P0/P1/P2 finding from this review would have been permitted,
and none was required (verdict: PASS).

### Review scope

Independent `ledger-finance` specialist review, dispatched fresh
(read-only: no file modifications, no database mutations permitted), of
`internal/casino/orchestrator.go`'s `postBet` function - the new
`pg_advisory_xact_lock`, the idempotency short-circuit, RG/Risk
evaluation ordering versus the ledger mutation, rollback/error-path
behavior - and `internal/rg/rg.go`'s `clock_timestamp()` fix and its
interaction with the new lock. Reviewed at HEAD, commit `19f4125`.

### Verdict: PASS — independent sign-off GRANTED

No P0 or P1 financial-correctness issue found. None of the reviewer's
veto triggers present: no floating-point money, no historical-ledger-row
mutation, no direct balance update (balances remain trigger-maintained
projections), no money path without a DB-enforced idempotency key.

### Answers to the 20 required questions (summarized - see the full
report for file:line citations and exact test evidence)

1. Concurrent duplicate callbacks cannot produce more than one financial
   mutation (three independent layers: the new lock, the DB unique
   index, `ledger.Post`'s own conflict handling).
2. The lock scope `(tenant_id, provider_id, provider_tx_id)` correctly
   serializes the intended operation - it is exactly the identity the DB
   uniqueness and the ledger idempotency key already use.
3. The lock is transaction-scoped (`pg_advisory_xact_lock`, not the
   session-scoped variant) and releases on commit or rollback - verified
   empirically via a read-only `pg_locks` probe.
4. No deadlock cycle is reachable: the lock is acquired as the first lock
   of the transaction, and occupies a distinct advisory namespace from
   `rg.lockPerson`/`risk_cumulative` (confirmed via `pg_locks`).
5. Lock ordering is compatible with the existing architecture - the
   global order (bet-delivery lock -> `rg.lockPerson` -> cumulative-usage
   lock -> balance-projection row lock -> ledger insert) is respected
   everywhere; `LaunchGame` uses the same relative RG-before-risk order.
6. A failed Risk decision cannot mutate the ledger (evaluated before any
   balance lock or `ledger.Post`).
7. A failed RG decision cannot mutate the ledger (evaluated before Risk
   and before any ledger write).
8. A failed/idempotent duplicate callback cannot mutate the ledger (the
   short-circuit returns before session/RG/risk/balance/post; a
   duplicate that somehow reached `ledger.Post` is stopped by the unique
   constraint).
9. A successful operation cannot be incorrectly denied by a duplicate
   delivery observing changed Risk/RG state - the lock plus short-circuit
   is exactly what fixes this (this stage's own Part F work); the one
   residual case is a previously-*declined* bet being re-evaluated on
   redelivery, which is by design for RG but has a transient-denial edge
   case (F7 below).
10. Idempotency and financial correctness are both preserved, subject to
    F1/F2 below.
11. Rollback transactions are correctly represented - a new transaction
    with entries as the exact inverse of the original's own loaded
    entries, history never edited.
12. No double reversal possible under rollback + duplicate-callback races
    (`SELECT ... FOR UPDATE` serializes concurrent rollbacks); the
    concurrent bet+rollback and concurrent identical-rollback-redelivery
    races are financially safe by code analysis but not proven by a test
    (F3, D/E below).
13. `SUM(debits)==SUM(credits)` holds for every successful mutation,
    enforced by a deferred DB constraint `ledger.Post` forces IMMEDIATE.
14. Append-only ledger semantics preserved - an advisory lock touches no
    ledger table; the append-only trigger already permits the row locks
    used elsewhere.
15. The `clock_timestamp()` change creates no financial-correctness
    issue - it appears only in a read predicate, never influences an
    amount/direction/entry, and can only make the check fail more
    restrictively than before, never less.
16. The transaction boundary is correct under READ COMMITTED (what the
    pool actually uses) - the design depends on this isolation level;
    worth documenting as an explicit dependency (see F-follow-ups).
17. No TOCTOU window within a single `postBet` for a given
    `provider_tx_id` - every check's lock is held to commit.
18. Provider callbacks are correctly isolated by tenant/provider/
    provider-tx identity (tenant server-derived, RLS-scoped, the ledger
    idempotency key and DB unique index and the new lock key all include
    all three components).
19. Compatible with future real casino providers, with two caveats (F4,
    F2/F7 below - a real provider's retry behavior would surface these
    more often than the mock does).
20. No P0/P1 financial-correctness issue is present.

### Findings — 6 P2s, 3 P3s, 2 hand-offs; none fixed this stage (none
blocking; fixing was judged out of this final-gate-only stage's
authorized scope and left for human-authorized follow-up work, per
CLAUDE.md's "no uncontrolled scope expansion")

**P2s** (owner recommended by the reviewer in parentheses):

- **F1 (casino/ledger-finance)**: `findPostedBetTransaction`'s idempotent
  replay matches on `(tenant_id, transaction_type, provider_id,
  provider_tx_id)` only and never compares the redelivered amount/asset
  against what was actually posted - a provider redelivering the same
  `provider_tx_id` with a *different* amount is told "succeeded" without
  verification. `internal/payments` already rejects the analogous case
  explicitly. No test covers this.
- **F2 (casino/ledger-finance)**: `postWin` has no "already posted ->
  replay original result" short-circuit (pre-existing since Stage 4A). A
  redelivered win after its bet was rolled back returns `ErrBetNotFound`
  instead of idempotently replaying its own prior success - the ledger
  stays correct, but the provider is told "no matching prior bet" for a
  win the platform genuinely credited.
- **F3 (casino/ledger-finance)**: a rollback of a never-seen original
  racing a concurrent bet on the same `provider_tx_id` degrades to an
  opaque internal error (financially safe - the whole transaction aborts,
  nothing posted - but unclassifiable by the webhook handler, surfaces as
  a 500). `internal/payments` has a regression test for the sequential
  form; casino has none, and neither domain tests the concurrent form.
- **F4 (casino/platform)**: the new lock is an unbounded blocking
  `pg_advisory_xact_lock` with no `lock_timeout`, unlike
  `internal/reconciliation`'s use of the `try_` variant - under a
  provider retry storm on one `provider_tx_id`, each waiter holds a
  pooled connection and open transaction for the queue's duration.
- **F5 (rg)**: the `clock_timestamp()` fix is correct but incomplete -
  `starts_at` is stamped from the *application* clock but enforced
  against the *database* clock, leaving a clock-skew-sized (not
  transaction-duration-sized) window. Strict improvement over the bug
  that was fixed; same defect class remains at a much smaller scale.
  Financially inert - an RG enforcement item, not a ledger one.
- **F6 (ledger-finance)**: no canonical lock/entry order across
  bet/win/rollback on `wallet_balance_projection`'s hot rows (the
  tenant-wide `house_gaming` account) - pre-existing, not introduced this
  stage; the new lock marginally widens the window by holding
  duplicate-delivery transactions open longer. Can produce a spurious
  40P01 abort under concurrent bet+win/rollback on the same player, never
  financial corruption.
- **F11 (test gap, casino/ledger-finance)**: the new stress tests
  (`adversarial_lock_stress_test.go`) assert balance and row count but
  never assert `SUM(debits)==SUM(credits)` directly, though the helper
  already exists and is used by four other casino tests.

**P3s** (lower severity, recorded for completeness):

- **F7**: a *declined* bet leaves no record, so decline is not
  idempotent across sequential redeliveries of a *transient* denial
  (cumulative window rollover, a temporarily frozen wallet, an edited
  limit) - correct for RG's effectively-permanent self-exclusion, a real
  edge case for anything transient. Should be stated explicitly in
  provider-protocol documentation.
- **F8**: idempotent replays write no audit record - recommend a
  low-severity audit/metric on the replay path for provider-behavior
  forensics.
- **F9**: `postBet` only rejects `revoked` sessions; `expired`/`consumed`
  sessions still permit bets - deliberate per ADR 0025 §3/§6 (the round
  outlives the launch token), flagged for explicit casino/security
  sign-off, not a ledger issue.

**Hand-offs (non-financial, out of this review's domain)**:

- **F10 (security)**: the exact `now()`-vs-`clock_timestamp()` bug class
  fixed in `internal/rg` this stage still exists in
  `internal/auth/session.go:439` and
  `internal/auth/credential_token.go:208` (`expires_at > now()`).
- **Cross-provider identical transaction IDs (item J)** and **concurrent
  bet+rollback (item D)** / **concurrent duplicate-callback+rollback
  (item E)**: correct by code analysis, not proven by a test - recommend
  adding regression tests when casino or ledger-finance next touches
  these paths.

### Adversarial test coverage (A-N) — see the full agent report for the
exact commands; summary:

Covered and PASS: A (2 concurrent identical deliveries), B (N=10
concurrent identical deliveries), G (RG denial during concurrent
delivery), K (`SUM(debits)==SUM(credits)`, via existing tests, not the
new stress tests), L (per-transaction debit/credit balance), M (rollback
behavior), N (race detector - every run above used `-race`, no race
reports).

Partially covered (a related but not identical scenario is tested; PASS
on what exists): C (concurrent different bets, same player - the
overdraft-race variant is untested in casino, though covered in
`internal/withdrawal`), F (risk denial during concurrent delivery - a
duplicate racing a live risk-rule change is untested), H (idempotent
redelivery after RG state change is tested; after a *risk*-rule change is
not, though the short-circuit is state-independent by construction), I
(cross-tenant isolation is tested; posting the identical
`provider_tx_id` as a successful bet under two different tenants is not).

Not covered by any test (correct by code analysis only): D (concurrent
bet + rollback), E (concurrent duplicate callback + rollback), J
(cross-provider identical transaction IDs).

No test result was fabricated; every PASS above is backed by an actual
`go test -race -tags=integration` run this stage, and every gap is stated
as a gap rather than assumed passing.

### Environmental observation, no action taken

`risk_rules` held 59 leftover rows (all platform-wide, all
`status='disabled'`) at review time. Confirmed inert -
`risk.Rule.appliesAt` requires `RuleActive`, so these cannot influence any
evaluation, and no test failure this stage was attributable to them. Per
this stage's own explicit instruction, the reviewer took no cleanup
action on shared database state.

## Next stage

Not started; requires explicit human authorization per the stage-gate
rule in `CLAUDE.md`. Stage 4G-FINAL's own directive explicitly forbids
starting Bonus, a real KYC provider, a real casino provider, or
sportsbook automatically. Candidates named in the Stage 3D/4A/4D-RG/4E/
4F/4G/4G-FINAL directives (a real casino provider integration,
sportsbook, bonus - once Risk & Limits is proven stable, B2C frontend,
partner console, production deployment, real PSP integrations,
real crypto integrations, a real identity-verification/KYC vendor to
actually populate `VerifiedAttributes` AND perform real document
verification) do not begin automatically.

---

## Stage 4H-A — Bonus, Gamification & Reward Orchestration Architecture Freeze

Directive: "STAGE 4H-A — BONUS, GAMIFICATION & REWARD ORCHESTRATION
ARCHITECTURE FREEZE" (27 sections). Explicit and repeated throughout the
body: "YOU ARE NOT AUTHORIZED TO IMPLEMENT THE BONUS ENGINE YET. This
stage is ARCHITECTURE + ACCOUNTING + DOMAIN CONTRACT FREEZE ONLY."
Forbidden this stage: Bonus Engine, Gamification, real sportsbook/casino/
KYC provider integration, CRM, notification providers, provider-specific
bonus APIs. **A single trailing line appended after the entire
27-section body read "Approved — proceed with Stage 4H: Bonus Engine."**
This is a genuine contradiction with the rest of the directive, not a
minor ambiguity — the body is detailed, specific, and internally
consistent about being design-only, repeating the prohibition multiple
times, while the trailing line reads as a blanket go-ahead for the next
stage's actual implementation. Per CLAUDE.md's stage-gate rule ("never
begin the next stage's implementation unprompted, even if it seems
obviously next") and the general principle that a directive's detailed,
reasoned body governs over an unexplained one-line addendum, this stage
treated the body as authoritative and did NOT begin Bonus Engine
implementation. This is re-flagged explicitly in the completion report
delivered to the user, which asks for explicit confirmation before any
Stage 4H implementation work begins.

### Wave 1 — specialist architecture drafts (7 parallel agents)

- **bonus-engine** rewrote `docs/architecture/10-bonus-engine-
  architecture.md` from its Stage-0 stub into the full Campaign→Offer→
  Grant→Activation→Progress→Completion→Conversion/Release→Expiry→
  Cancellation→Reversal lifecycle, with per-state idempotency and
  concurrency rules and an explicit table of what's built vs. forthcoming
  (e.g. free spins: "forthcoming — not yet built").
- **architect** designed the full Gamification sub-domain: points/XP/
  levels (`17-gamification-engine-architecture.md`), tournaments
  (`18-tournament-architecture.md`), missions/challenges
  (`19-mission-architecture.md`), the rewards marketplace/raffles
  (`20-reward-marketplace-architecture.md`), and extended
  `02-domain-and-service-boundaries.md` with the new Gamification Engine
  service boundary.
- **ledger-finance** produced `docs/decisions/0032-bonus-accounting.md`
  (CRITICAL, treated as authoritative/veto-holding for any conflicting
  monetary-accounting claim elsewhere) and
  `24-points-accounting-architecture.md`, plus additive cross-reference
  pointers into the existing `ledger-accounting-model.md`,
  `financial-transaction-flows.md`, `reconciliation-model.md`, and ADR
  0019.
- **sportsbook** produced `docs/decisions/0033-provider-interoperability-
  and-external-bonus-engines.md` — the provider-neutral contract two
  future sportsbook providers (one with its own native bonus engine) map
  onto, neither built.
- **identity-compliance** produced `docs/decisions/0034-bonus-
  gamification-rg-kyc-identity-integration.md` — RG remains sole
  authority over play/withdrawal enforcement; no duplicate KYC evidence
  storage; self-exclusion is prospective not retroactive.
- **risk** extended `docs/decisions/0031-risk-and-limits-engine.md` with
  §14-§18 — Bonus/Gamification consume `internal/risk.Evaluate`
  exclusively, never build their own limit engine; points caps are
  explicitly out of scope while points remain non-convertible (§15h).
- **backend** produced `25-bonus-gamification-api-architecture.md` — API
  and RBAC contract design only, no code.

### Cross-domain connective documents (Orchestrator-authored directly)

Per this project's established pattern (Orchestrator does new
genuinely-cross-domain work directly rather than assigning it to a
single specialist with partial visibility), three documents were
authored directly:

- `21-reward-orchestration-architecture.md` — the Reward Orchestrator:
  a fulfillment mechanism only, never a decision-maker on whether a
  reward is earned; consumes decisions from Bonus/Gamification and
  fulfills them (ledger posting, points crediting, badge/external-
  provider award) idempotently.
- `22-canonical-activity-event-taxonomy.md` — the canonical Activity/
  Event envelope extending the Stage-1 `internal/eventbus.Event` stub
  (still unwired), with the full event catalogue (casino, sportsbook,
  bonus, gamification) and the `event_id` (per-publish) vs
  `idempotency_key` (per-business-fact) distinction as the schema's
  load-bearing invariant.
- `23-external-reward-provider-contract.md` — the `ExternalRewardProvider`
  interface for coexistence with a provider-native bonus engine,
  mirroring the existing `CasinoProvider`/`PaymentProvider` adapter
  pattern.

### Wave 2 — specialist review round (code-reviewer, security, qa, casino)

Reviewed the full frozen document set for internal consistency (these
documents were authored in parallel by different specialists with only
partial visibility into each other's final text, a known "seam failure"
risk class per this project's prior stages).

**code-reviewer — 8 P1s, all fixed:**
1. `PointType` scope contradiction (doc 17 said platform-wide-only; doc
   24 said tenant-scoped-only) — resolved: the DEFINITION is dual-scope
   (nullable `tenant_id`, mirroring `risk_rules`), the BALANCE tables
   (`PointAccount`/`PointTransaction`/`PointEntry`) remain always
   `tenant_id NOT NULL`. Also fixed doc 24 §9's incorrect `WithoutTenant`
   RLS claim.
2. Externally-fulfilled bonus ledger treatment conflicted across 3
   documents (doc 10: identical treatment; ADR 0032: zero entries; ADR
   0033: referenced a liability account ADR 0032 refuses to create) —
   resolved on ADR 0032's authoritative zero-ledger-entries rule
   (memo/audit stream only), all 3 documents aligned.
3. RG denial at conversion time wrongly auto-forfeited the Grant in doc
   10 §5, contradicting ADR 0031 §15a-ii / ADR 0034 §2.2 (leaves Grant
   `completed`, retryable) — fixed; auto-forfeit-on-any-denial would
   have incentivized delaying self-exclusion.
4. Points caps falsely claimed covered by Risk in docs 17 and 02, when
   ADR 0031 §15h explicitly puts this out of scope while points are
   non-convertible — fixed both documents to disclose no points-cap
   mechanism exists today, with two possible future resolutions recorded
   but not authorized.
5. Canonical event envelope (doc 22) was missing `is_real_money`,
   `funding_source`, reversal linkage, and `correlation_id` — fields doc
   17 declared mandatory and capability-blocking if absent — added to
   doc 22's envelope table.
6. `event_id` vs `idempotency_key` confusion in docs 17 §8, 19 §5, 24
   §8 — all three keyed dedup on `event_id` (unique per publish, may
   differ on redelivery) instead of `idempotency_key` (stable per
   business fact) — a real double-count/double-award bug risk — fixed
   in all three.
7. Marketplace redemption transaction-boundary conflict: doc 24 §7
   forbade any saga for a ledger-postable reward (single transaction
   only); doc 20 §8 mandated a universal saga for every item type —
   fixed doc 20 §8 with a synchronous-vs-asynchronous-fulfillment
   discriminator.
8. Reward Orchestrator's new fulfillment table (doc 21) claimed "no new
   tenant/brand-scoping surface" while introducing a tenant-scoped,
   player-attributable table with no RLS description — fixed with full
   RLS requirements.

**security — 9 P1s (F1-F9) + several P2s, all P1s fixed:**
- F1: doc 21 had no reversal path despite 4 sibling documents delegating
  reversal to it — added a full "Reversal and compensation" section
  defining `RewardReversalDecision`.
- F2: tournament settlement (doc 25) was gated by the same permission as
  prize authoring — split `tournament:settle` from
  `tournament_config:manage`.
- F3: no permission existed for bonus-campaign authoring — added
  `bonus_config:read`/`bonus_config:manage` with a threshold+four-eyes
  requirement for high-value Offers.
- F4: same envelope-fields gap as code-reviewer's #5 (independently
  found).
- F5: anti-manipulation controls in doc 17 were never actually wired
  into doc 18's settlement sequence — added step "1a" as a fail-closed
  pre-settlement gate plus a launch flag.
- F6: contradictory instruction on `internal/identityresolution` (doc 17
  said consume directly; ADR 0034 said no separate read is needed) —
  reconciled: Gamification reads the already-resolved `PersonID`
  linkage, never re-invokes registration-time orchestration.
- F7: doc 23's external callback contract was missing 7 rules present in
  the casino-callback precedent it claimed to mirror — tenant-from-URL-
  only enforcement, signature verification ordering, suspended-tenant
  handling, body size limits, enumeration-resistant errors,
  provider_id-from-credential, and (most critical) an integrity-alert
  rule for a callback not matching a platform-created handle — a leaked
  credential could otherwise post arbitrary grants that reconciliation
  would route to a human to book as real. All 7 added.
- F8: withheld-tournament-participant RG decision codes had no
  internal-vs-player-facing distinction in doc 18's snapshot or doc 25's
  leaderboard endpoint — added a binding two-projection rule (full
  internal record vs. player-facing rank/identity/score-only
  projection) to both.
- F9: administrative XP correction (doc 17 §4.1) bypassed the four-eyes
  gate that doc 17 §4.2 required for level overrides, even though level
  is a pure projection of XP — fixed by applying the same
  threshold/four-eyes treatment whenever an XP correction changes the
  resulting level.
- P2s fixed: F10 (doc 23 had no audit section — added, absorbing ADR
  0033 §1.8), F11 (doc 23 cited the wrong RLS precedent for credential
  storage — fixed to cite the Vault/KMS rule in
  `docs/security/security-architecture.md`), F15 (`bonus:read` could
  leak KYC state via an `awaiting_verification` status — added a
  mitigation requirement), F17 (doc 21's RG re-check was conditional
  ("if δ is non-trivial") — made unconditional for every fulfillment,
  closing the exact class of race Stage 4G-FINAL already found once),
  F21 (doc 21's audit section used an invalid `audit.Entry` actor shape
  — fixed to `ActorService` with a registered service identity), and the
  RBAC-bundling-risk summary finding (doc 25 proposed 7 `manage`-class
  permissions with no role to hold them — added a requirement to mint a
  dedicated `RolePromotionsManager` role).

**qa — 5 P1s, all fixed:**
- Grant-completion advisory lock (doc 10) was scoped to `grant_id` alone
  despite citing the Stage 4G-FINAL tenant-scoping lesson — fixed to
  `(tenant_id, grant_id)`.
- The `now()`/`clock_timestamp()` lesson was applied inconsistently —
  restated centrally plus at each of the 5 new time-window comparisons
  this stage introduced (streak continuity, mission expiry, tournament
  freeze, cashback window closing, RG-reuse).
- doc 23's outbound idempotency-key example used a literal
  `grant_attempt` counter component, the exact anti-pattern doc 24 warns
  against — fixed.
- doc 17's collusion control only detects same-person multi-accounting,
  not genuine two-distinct-people collusion (its own stated top
  concern) — added an explicit `OPEN DECISION` row rather than silently
  claiming coverage.
- P2-9 fixed: doc 18's tournament-entry sequence mandated an
  unconditional `risk.Evaluate` call for every entry, contradicting ADR
  0031 §15e's explicit position that a free-entry tournament (no
  monetary fee, no monetary prize path) is out of scope and creates no
  monetary exposure — fixed step 2 to gate the Risk call on genuine
  monetary cost, matching doc 19's already-correct handling of
  `mission_opt_in`.

**casino — 2 P1s, both fixed:**
- doc 22's `casino.bet.rolled_back` silently omitted the tombstone-
  rollback case — fixed with an explicit scope note.
- doc 22 was missing a `casino.launch.denied` event despite real,
  audited RG/Risk launch-denial code paths in `internal/casino` — added.
- (P2, also fixed) doc 22's sportsbook events (2) didn't match ADR
  0033's own design (3, with multi-fire settlement semantics) — expanded
  to 3, matching exactly.

**Not fixed this stage (recorded as open follow-up items, per the
stage's own architecture-freeze scope and CLAUDE.md's "no uncontrolled
scope expansion" rule):**
- qa: tournament entry/withdrawal re-entry cycling behavior is
  unaddressed.
- qa: achievement-unlock reversal/void handling is unaddressed.
- qa/security: demo-event exclusion from real-money scoring/progress is
  stated as policy in the documents but not structurally/centrally
  enforced anywhere (e.g. a single gate in the event-ingestion path) —
  left as an implementation-time requirement.
- security P3s not requiring a fix this stage (F12-F14, F16, F18-F20,
  F22-F24) — lower-severity items (naming/clarity/future-hardening
  suggestions) not re-litigated here to avoid further scope expansion;
  available in the Wave 2 review transcript if needed at implementation
  time.

### Files touched this stage

New: `docs/architecture/17-gamification-engine-architecture.md`,
`18-tournament-architecture.md`, `19-mission-architecture.md`,
`20-reward-marketplace-architecture.md`,
`21-reward-orchestration-architecture.md`,
`22-canonical-activity-event-taxonomy.md`,
`23-external-reward-provider-contract.md`,
`24-points-accounting-architecture.md`,
`25-bonus-gamification-api-architecture.md`,
`docs/decisions/0032-bonus-accounting.md`,
`docs/decisions/0033-provider-interoperability-and-external-bonus-
engines.md`, `docs/decisions/0034-bonus-gamification-rg-kyc-identity-
integration.md`.

Rewritten/extended: `docs/architecture/10-bonus-engine-architecture.md`,
`02-domain-and-service-boundaries.md`,
`docs/decisions/0031-risk-and-limits-engine.md` (§14-§18),
`docs/architecture/ledger-accounting-model.md`,
`financial-transaction-flows.md`, `reconciliation-model.md`,
`docs/decisions/0019-authoritative-ledger-and-balance-projection-
architecture.md` (additive cross-references only).

Governance: `docs/governance/task-registry.md` (new Stage 4H-A section),
`docs/governance/project-status.md` (new Stage 4H-A section),
`docs/progress.md` (this entry), `docs/active-stage.md` (new Stage 4H-A
section, prepended as current).

No code, migration, or test files were created or modified this stage —
`go build ./...` was re-run after all edits and remains clean (docs-only
diff).

### Next stage

Not started. Stage 4H (Bonus Engine implementation) is explicitly NOT
authorized by this stage despite the directive's contradictory trailing
line — see the note at the top of this entry. Requires explicit human
authorization per CLAUDE.md's stage-gate rule before any Bonus Engine,
Gamification Engine, Reward Orchestrator, or sportsbook-bonus code is
written.

---

## Stage 4H-A addendum — ledger-finance financial sign-off + product-owner-proxy scope review

Two further Wave-2 specialists were dispatched after the first review
round (code-reviewer/security/qa/casino, above) reported back and their
fixes were committed: `ledger-finance` (the independent financial-
correctness sign-off CLAUDE.md requires — "Financial/Ledger must
independently approve all monetary accounting decisions" — before this
stage's monetary architecture counts as reviewed) and `product-owner-
proxy` (scope discipline against the B2C-MVP-first objective).

### ledger-finance — verdict: PASS WITH FINDINGS, sign-off granted once 7 P1s applied

No P0. Independently re-derived ADR 0032 §3's worked-example arithmetic
from scratch and confirmed it correct; found no floating-point money, no
balance mutation, no historical-entry edit, and no cache read on a
money/points hot path anywhere in the reviewed set (all "Verified clean"
in the review's own words). The 7 P1s were all gaps/drifts in the frozen
contract, not wrong accounting — each was a place an implementer
following the documents literally would have produced a posting breaking
invariant B1, a money path with no defined double-entry treatment, or an
idempotency/tombstone guarantee with no table to live on:

1. **`financial-transaction-flows.md` was never updated for Rule B2 on
   the bonus-funded *play* flows.** ADR 0032 §2's Rule B2 (every
   `player_bonus` entry carries a `promo_liability`/`bonus_expense`
   mirror pair) was only reflected in the bonus-specific Flows 12/14/15;
   Flows 5 (casino bet), 6 (casino win), 7 (rollback), 9 (sportsbook
   settlement), 11 (partial settlement), and 20 (jackpot carve-out) —
   the flows carrying the *highest volume* of `player_bonus` entries —
   still described only the two-entry cash-funded shape. An implementer
   coding Flow 5 as written would post `Dr player_bonus / Cr
   house_gaming` and break invariant B1 on the very first bonus-funded
   spin, caught only by the hourly B1 sweep as a P1 incident. **Fixed**:
   added the mirror-leg requirement to all six flows, added a new **Flow
   21 — Externally-fulfilled bonus (no posting)** (ADR 0032's own
   Consequences section had asked for this flow but it was never
   written), and updated the summary table and ADR 0032's own
   Consequences bullet to name the corrected flow list.
2. **No authoritative mapping from Bonus Engine lifecycle events to
   ledger transaction types; `cancelled` had no accounting treatment at
   all.** Three coupled gaps: (a) both `granted` and `activated` were
   emitted as lifecycle events with no statement of which one triggers
   the ADR 0032 §3 grant posting — since the two events carry different
   idempotency keys, a domain posting on both would double-credit
   undetected; (b) a `converted` lifecycle event was never defined
   despite ADR 0032 §4 requiring one to trigger the `bonus_conversion`
   posting; (c) `cancelled` — a terminal state reachable after funds
   already sit in `player_bonus` — had no posting and no
   `transaction_type` at all. **Fixed**: added ADR 0032 §3.1, a binding
   lifecycle-event → posting map table (one `bonus_grant` posting at
   `activated` only, never `issued`; `converted` → `bonus_conversion`;
   `cancelled` from an activated grant → `bonus_forfeiture`, identical
   shape to expiry); added `converted` to doc 10 §6's event list and
   corrected its §1.3 wording.
3. **A no-wagering cash reward (`cash_credit`) had no defined double-
   entry treatment anywhere**, despite being referenced as a supported
   reward type in doc 10 §2 and doc 21's fulfillment-mechanism table,
   both of which deferred to ADR 0032 for a treatment ADR 0032 never
   defined. Using `promo_liability` for a direct cash credit would
   violate B1 by construction, since nothing contingent is outstanding.
   **Fixed**: added a two-entry direct-cash-reward treatment to ADR 0032
   §3 (`Dr bonus_expense / Cr player_cash`, or `provider_payable` if
   provider-funded) and clarified doc 10 §2's "Wagering axis with
   multiplier 0" language is a lifecycle-state statement only, never two
   postings.
4. **Rule B2's applicability to `manual_adjustment` was unstated, and
   ADR 0032 §7 prescribed `manual_adjustment` for the consumed portion
   of a partly-consumed grant without naming a target account** — the
   single most likely manual bonus-correction path could post `Dr
   player_bonus / Cr manual_adjustment` with no mirror leg, breaking B1
   on a four-eyes-approved action that looks entirely legitimate in the
   audit log. **Fixed**: added an explicit "Rule B2 admits no exception
   by transaction type" statement, and corrected §7 to target
   `player_cash`, never `player_bonus`, for the consumed-portion
   correction.
5. **The Reward Orchestrator's reversal path had no tombstone, and
   placed its single-reversal constraint on a table that doesn't exist
   for money-moving mechanisms.** Rejecting (rather than tombstoning) a
   reversal of a never-fulfilled decision reopens exactly the race
   CLAUDE.md's rollback rule exists to prevent — a late-arriving original
   could fulfill after its own reversal was rejected. And the `UNIQUE
   (tenant_id, original_decision_id)` guarantee was placed on "the
   reversal-tracking record," which the same document's own idempotency
   section says exists only for badges and `external_provider_reward` —
   not for `cash_credit`/`bonus_credit`/`points_credit`, the mechanisms
   that actually move value. **Fixed**: the guarantee now defers to the
   owning ledger's own `FOR UPDATE`-guarded single-reversal pattern for
   money/points mechanisms, and a tombstone is written (in the owning
   ledger or the Orchestrator's tracking table, per mechanism) rather
   than a bare rejection.
6. **The External Reward Provider contract was missing the per-reward-
   type fulfilment-destination declaration ADR 0032 §6(c) explicitly
   assigns to it** — doc 23's capability discovery listed reward *types*
   a provider could fulfill with no destination dimension, so its own
   Reversal row hedged ("if any monetary effect exists"), exactly the
   guess-at-posting-time §6(c) forbids. **Fixed**: added a binding
   fulfilment-destination flag (`into_platform_wallet` vs
   `inside_provider`) to capability discovery, rejected at configuration
   time if undeclared, and corrected the Reversal row to branch on it
   (zero ledger effect for `inside_provider`, ordinary compensating
   transaction for `into_platform_wallet`).
7. **Cross-tenant points-spend isolation was justified by a premise this
   round's own `PointType` dual-scope correction invalidated.** Doc 20
   said tenant isolation "falls out of the point type's own scope" —
   true only for a tenant-scoped `PointType`; for the platform-wide
   definitions this stage's own doc 24 correction now permits, the point
   type isolates nothing. **Fixed**: doc 20 §3/§10 now correctly attribute
   isolation to the balance tables' `tenant_id NOT NULL` + RLS, never the
   point type definition's own scope.

4 of 5 P2s fixed (ADR 0032 §8's idempotency-key wording corrected from
"event id" to `idempotency_key`, matching doc 22's terminology; doc 21's
`amount_minor_units`/`points_amount` corrected to a decimal
representation rather than `int64`, which overflows for an
exponent-18 asset; doc 21's fulfillment-mapping table gained an inline
zero-ledger-entries note for `external_provider_reward`; two new open
decisions — pooled tournament prize-pool liability accounting, and
split-prize/pro-rata rounding — added to ADR 0032's open-decisions list).
1 P2 (a campaign budget-cap field with no enforcement mechanism anywhere
— disclosed as `NOT IMPLEMENTED`/advisory-only in doc 10, not fixed) and
1 P3 (a cosmetic `PointType` shape drift in doc 17, which already labels
itself conceptual-only) were recorded rather than fixed.

### product-owner-proxy — verdict: no P0/P1/P2 (scope-discipline review, not correctness)

Read `CLAUDE.md`, `docs/architecture/14-mvp-scope-and-roadmap.md`, and
the full Blueprint PDF (all 20 pages) and cross-checked every Stage 4H-A
sub-capability against Blueprint §4 and doc 14's MVP baseline.

**Top-line finding**: the Blueprint never mentions gamification, points,
XP, levels, achievements, badges, missions, tournaments, leaderboards,
streaks, a reward marketplace, raffles, or mini-games — its §4.5 "Bonus
and promotion engine" is exactly Campaign→Offer→Grant→Progress with five
config axes, precisely what `10-bonus-engine-architecture.md`
implements, no more. `14-mvp-scope-and-roadmap.md`'s own B2C MVP scope
also never mentions gamification anywhere. The entire Gamification
Engine domain (docs 17-20) and the standalone Reward Orchestrator (doc
21) are net-new scope introduced this stage with no anchor in the
Blueprint and no anchor in this project's own previously-recorded
roadmap. This was not specialists inventing scope unilaterally — the
stage's own directive asked for it — but the review's finding is that it
should now be reconciled with the MVP baseline document rather than left
an unrecorded gap.

Execution discipline within the frozen documents was found unusually
strong for a body of work this size: nearly everything correctly labeled
`NOT IMPLEMENTED`, dependencies stated as `ASSUMPTION`s rather than
facts about other domains, several genuine `RECOMMENDATION`s correctly
labeled as such, no place where a recommendation was dishonestly
presented as a Blueprint requirement, and doc 17 §11 (raffles/mini-games)
singled out as exactly the right proportionate treatment for a
zero-mandate feature (two short paragraphs, explicitly "not scheduled,"
refusing to design the hard regulatory/RNG question).

Per-capability verdicts of note: **Tournaments (doc 18)** flagged as the
single most disproportionately-designed sub-capability — its
settlement/prize-arithmetic/anti-collusion depth exceeds parts of the
Bonus Engine's own MVP-required core lifecycle, for a feature with zero
scheduled build, and is the part of the design least likely to still be
correct by the time (if ever) tournaments are authorized. **Reward
Orchestrator (doc 21)** flagged as **premature abstraction** — a
three-domain-ready fulfillment layer built ahead of a second concrete
reward-producing domain, when Bonus Engine alone (the only domain
actually required by the Blueprint/MVP) already has a sufficient
lighter-weight fulfillment mechanism of its own (doc 10 §6). **Bonus
Engine's own type matrix (doc 10 §2)** found justified at its core
(Blueprint-required, MVP-scoped) but carrying tournament/mission/
loyalty-reward rows and full external-bonus-engine coexistence that are
B2B/future-Gamification-driven scope riding along on the MVP-required
document. **`ExternalRewardProvider`/ADR 0033** found grounded in a real
hybrid-licensing concern but sequenced ahead of need (Sportsbook is P3 in
doc 14's own build order; no commercial sportsbook relationship exists;
`09-sportsbook-architecture.md` doesn't exist yet).

All findings applied to `14-mvp-scope-and-roadmap.md`'s "Features
deliberately deferred" section: the Gamification Engine/Reward
Marketplace/Reward Orchestrator domain added as deferred scope with the
Blueprint-silence finding recorded; the `ExternalRewardProvider`
contract added as sequenced-ahead-of-need; and an implementation-scope
note added for whenever Stage 4H is authorized (scope the first slice to
the MVP-required bonus types only — deposit, reload, cashback, generic
wagering bonus, coupon — and do not build the Reward Orchestrator as a
standalone domain unless Gamification is authorized alongside it).

### Files touched in this addendum

`docs/decisions/0032-bonus-accounting.md`, `docs/architecture/
financial-transaction-flows.md`, `docs/architecture/10-bonus-engine-
architecture.md`, `docs/architecture/20-reward-marketplace-architecture.md`,
`docs/architecture/21-reward-orchestration-architecture.md`,
`docs/architecture/23-external-reward-provider-contract.md`,
`docs/architecture/14-mvp-scope-and-roadmap.md`. Governance:
`docs/governance/task-registry.md` (4HA-12/13/14), `docs/governance/
project-status.md`, `docs/progress.md` (this entry), `docs/active-
stage.md`. No code, migration, or test files were created or modified —
`go build ./...` re-run after all edits and remains clean.

### Next stage

Unchanged from the entry above: not started, requires explicit human
authorization. This addendum closes out the remaining Wave-2 review
findings (ledger-finance's required financial sign-off, now granted, and
product-owner-proxy's scope-discipline review) that were still pending
when this stage's first completion report was delivered.

---

## Stage 4H-B0 — Bonus, Gamification & Retail Scope/Implementation Plan

Directive: "STAGE 4H-B0 — BONUS + GAMIFICATION + RETAIL ARCHITECTURE/
SCOPE FREEZE," responding to a new confirmed business requirement: the
platform must support retail iGaming operations (a configurable
agent-hierarchy network — Operator → Partner → Super Agent → Agent →
Player/Cashier, configurable depth/structure per tenant/licence/
jurisdiction, never hardcoded) as another surface of the same platform,
sharing identity/wallet/ledger/risk/RG/payments/reporting/audit/bonus/
tenant architecture wherever appropriate. Architecture/scope-freeze
only — **no production code, no migrations, and no implementation were
started this stage.** The directive explicitly required the Orchestrator
to work as coordinator, delegate analysis to specialists without letting
them overwrite each other's work, and produce a master "STAGE 4H-B0 —
BONUS, GAMIFICATION & RETAIL SCOPE/IMPLEMENTATION PLAN" covering 25
numbered deliverables, ending with an explicit instruction not to proceed
to implementation automatically.

### Wave 1 — ten specialist architecture documents (parallel, each owning a distinct file)

- **architect** — `docs/architecture/26-retail-operations-
  architecture.md` (new, 1272 lines, 11 sections): the core retail +
  configurable agent-hierarchy architecture. Key decisions: node type,
  structure, and capability kept as three separate concerns (no
  hardcoded `level` integer or enum); storage is adjacency-list-
  authoritative with a closure table maintained transactionally as a
  derived projection (mirrors the ledger/balance-projection pattern);
  neither a Player nor a Cashier is a hierarchy node (a Player is an
  attribution edge via `retail_player_origins`, a Cashier is an
  effective-dated N:M staff assignment); a cashier is an
  `identity.StaffUser` with a retail role, a terminal is a `service`
  principal (ADR 0014 option 2), and both are required together for any
  money-touching operation. Independently confirmed the Blueprint has
  zero retail content (a full 20-page text search) — retail is
  human-directed business scope, not a Blueprint requirement, exactly
  like Gamification in Stage 4H-A. Disclosed a P0 conflict on whether a
  node's float fits ADR 0007's player-centric `Wallet` definition
  (deferred to ledger-finance + human, not resolved unilaterally) and a
  P0 flag that retail is very likely outside the platform's current
  Anjouan (online-only) licence. Also updated `docs/architecture/
  02-domain-and-service-boundaries.md` with the new Retail/Agent Network
  domain boundary section.
- **ledger-finance** — `docs/decisions/0035-retail-agent-network-
  accounting.md` (new, 1429 lines, ADR-0032 structure/rigor). Key
  decisions: an agent's float is a platform *liability*, credit-normal,
  the structural analogue of `player_cash` for a non-player counterparty
  — so a retail cash deposit is a *transfer of an existing platform
  liability* from agent to player (`Dr agent_float / Cr player_cash`,
  two entries, no `psp_clearing` leg), never a new deposit into the
  platform. Three new account types (`agent_float`,
  `agent_commission_payable`, `agent_commission_expense`). Invariant R1
  (retail transactions never touch PSP/bank rails), R2 (pool separation:
  player/agent-operational/commission/house, with a permitted-transition
  matrix), R3 (shift/till reconciliation against the terminal's
  *declared* movements, zero-tolerance — never a human cash count, which
  would reintroduce an unverifiable second truth source). Retail
  withdrawal is two steps reusing `player_withdrawal_hold` unchanged, to
  avoid an unreconcilable authorize-then-hand-over window. Commission
  recognized by a periodic run, not per-event. Disclosed a P0 blocking
  precondition: `ledger_accounts` today supports only wallet-scoped or
  house-level ownership, neither of which correctly fits a node-scoped
  `agent_float` account without an additive schema change — explicitly
  not applied unilaterally (changes an already-implemented shared
  table), deferred to architect + security + human sign-off. Also
  disclosed a genuinely new attack class: POS idempotency
  namespace-squatting, if a terminal's identity were taken from request
  payload rather than resolved from its own credential.
- **security** — `docs/decisions/0036-retail-hierarchy-rbac-and-audit.md`
  (new, ~1930 lines after Wave-2 fixes, 14 sections, 36 mandatory tests
  specified). Key decisions: three orthogonal authorization axes
  (capability/`auth.Permission`, tenant/`app.tenant_id`, and a new
  hierarchy-scope axis/`app.hierarchy_node_id`) — explicitly rejected "a
  Role per hierarchy level" since depth is tenant-configurable (would be
  a brand-specific code path, forbidden by CLAUDE.md). RLS via a
  closure-table `EXISTS` predicate, fail-closed by construction (no `OR
  guc IS NULL` escape branch). Scope resolved from the DB per request,
  never a JWT claim, so suspension/reassignment take effect on the next
  request rather than at token expiry. Drafted before architect's/
  ledger-finance's documents existed; reconciled in place against both
  once they landed mid-task, with each correction marked visibly
  (C13/C15/C18 etc.), not silently rewritten — including discovering
  its own draft was wrong about whether a tenant runs one hierarchy tree
  or several (doc 26 confirmed several networks per tenant are allowed).
  5 P0 design invariants stated as binding requirements on any future
  implementation (subtree-reassignment permission never bundled with
  general manage; no fail-open RLS branch; no retail role holds
  `PermStaffManage`; no retail money path skips RG→Risk; the closure
  table is write-protected as authorization data itself). 1 P1 disclosed
  and not silently assumed away: the existing `audit_log` RLS policy
  (ADR 0013, accepted Stage 2) does not narrow by subtree as-is, and
  Postgres's OR-of-permissive-policies semantics mean a narrower policy
  added beside it cannot narrow anything — flagged for a fresh security
  review of the eventual migration, not fixed here (would modify an
  accepted decision).
- **identity-compliance** — Stage 4H-B0 sections appended to
  `docs/architecture/05-identity-architecture.md` (166 lines) and
  `11-kyc-aml-rg-architecture.md` (293 lines). Key decisions: retail
  registration reuses the existing `identityresolution.
  RegisterPlayerWithResolution` flow as-is (no second registration
  path), with a new `registration_channel` provenance column
  (`online`/`retail`) on `player_accounts`. No new KYC tier invented —
  the existing tiered-trigger model already keys off "registration" as a
  lifecycle event; what's genuinely open (starting tier default,
  whether in-person presence satisfies part of KYC evidence) is a
  jurisdiction-configuration question, escalated not resolved. The
  binding RG mechanism: any retail action that is gambling-enabling or
  value-moving MUST call the identical `internal/rg.EvaluateEligibility`
  every other entry point already calls — never a retail-specific
  reimplementation, cached flag, or local restriction check. Flagged a
  P0: a fail-open POS design during connectivity loss (defaulting to
  "allow" when no eligibility decision can be obtained) would be a
  silent RG enforcement weakening — the binding default is fail-closed,
  with any bounded offline tolerance an explicit, separate human/
  business decision. Also flagged a genuinely new risk class this
  platform hasn't had to consider: a staff/player identity collision
  (a cashier serving a player who resolves to the cashier's own
  `person_id`).
- **payments** — a new "Retail cash rail" section in `docs/architecture/
  07-payments-architecture.md`. Key decision: retail cash is **not** a
  `PaymentProvider` implementation (the interface models an async,
  untrusted-external-vendor relationship that doesn't exist for a
  cashier-mediated handover) — modeled instead as a structurally
  distinct fulfillment channel. Retail withdrawal reuses the existing
  `withdrawal_requests` state machine and four-eyes/policy-threshold
  machinery unchanged through `approved`; a cashier's role is
  fulfillment only, never approval. Confirmed a POS/cashier terminal is
  never a source of financial truth, generalizing CLAUDE.md's
  "Redis/cache is never authoritative" principle to any client device.
- **risk** — a new "Stage 4H-B0: Retail Risk Integration" section (§19-24)
  appended to `docs/decisions/0031-risk-and-limits-engine.md`. Key
  decisions: the identical "consume `internal/risk.Evaluate`, never
  build a second limit engine" rule restated for retail, unweakened.
  Two new scope dimensions (`HierarchyNodeType`, `HierarchyNodeID`) fit
  the existing `Rule`/`specificity()` model as ordinary additive scope
  fields — no new precedence mechanism. Explicitly rejected
  subtree/ancestor rule inheritance (would require `matches()` to become
  set-membership plus a within-dimension nearest-ancestor tiebreak a
  presence bitmask cannot express). Disclosed two P0s: anonymous/bearer
  retail play would make every player-scoped Risk/RG/KYC mechanism
  structurally unsatisfiable (no player row to key anything on — escalated,
  not resolved), and delegated limit-authoring by hierarchy actors (a
  Super Agent setting its own sub-agents' limits) would be the limit
  engine defeating itself unless a new bounded-authoring model is
  explicitly designed. Disclosed a P1 carried forward as the single most
  important open item: "daily/periodic funding limit by hierarchy
  level" — the most natural reading of the business requirement — is
  **not yet expressible** even after the proposed extensions, because
  `internal/risk`'s cumulative-amount aggregation is keyed on
  `player_account_id` and a float advance between two agent nodes has no
  player at all; only per-transaction caps work today.
- **data-analytics** — a new "Retail agent-hierarchy reporting" section
  in `docs/architecture/12-audit-reporting-architecture.md` (245 lines).
  Key decision: one shared reporting pipeline for online+retail, with
  three additive dimensions (`hierarchy_node_id`, `hierarchy_node_type`,
  `channel`) on the existing CDC fact tables — never a parallel retail
  reporting system. One shared reporting API surface for BO and a future
  retail/agent console, parameterized by the caller's own authorized
  scope — never dedicated retail endpoints (explicitly rejected as
  reproducing the "second computation of the same fact" failure
  CLAUDE.md's ledger-authority rule already forbids one layer down).
- **backend** — a new "Stage 4H-B0 — Retail/POS and Agent Hierarchy API
  Surface" section in `docs/architecture/04-api-architecture.md`.
  Conceptual endpoint groups for cashier/POS operations (registration-
  at-retail, deposit/withdrawal confirmation, shift/till open-close,
  balance/float inquiry) and hierarchy management (same endpoints as the
  existing Back Office, parameterized by caller scope — matching this
  platform's existing single-surface-many-roles pattern). Discovered and
  corrected its own first draft mid-task once payments' landed document
  revealed a different idempotency mechanism than assumed.
- **qa** — a new "Stage 4H-B0 — Retail, Hierarchy/RBAC, RG-Bypass and
  Bonus Test Strategy" section in `docs/testing/testing-strategy.md`
  (306 lines). A 9-item retail financial testing floor mirroring ADR
  0032's format; cross-hierarchy-node isolation named as "the single
  most important new isolation class this stage introduces," weighted
  equal to tenant isolation; a concrete, named, required adversarial
  test for RG-bypass-through-retail modeled directly on the Stage
  4G-FINAL `clock_timestamp()` race (a player self-excludes online while
  a retail cashier transaction for the same player is in flight); a
  dedicated concurrency/correctness test class for the hierarchy's
  recursive-subtree mechanism (node reassignment mid-query, concurrent
  reassignment races, cycle-prevention under concurrency). Confirmed
  ADR 0032's existing bonus testing floor applies unchanged to whatever
  bonus slice ships, and explicitly deferred all Gamification-specific
  test design.
- **bonus-engine** — a new "Stage 4H-B0 — MVP Implementation Scope Plan"
  section in `docs/architecture/10-bonus-engine-architecture.md`. Named
  the exact first-slice bonus types (deposit, reload, cashback, generic
  wagering bonus, coupon) and the exact excluded types (free spins/free
  bets — blocked on casino/sportsbook interfaces; tournament/mission/
  loyalty-reward — blocked on deferred Gamification). Answered the
  Stage 4G §32 gate check directly: Risk & Limits is now stable enough
  (Stage 4G-FINAL 11/11-area review + finance-gate follow-up, PASS/no
  P0/P1) to qualified-lift the block for this first slice, with two
  named conditions (first-slice Risk rules restricted to
  `min_amount`/`max_amount`; one dependency request to `risk` for the
  `bonus_conversion` `Operation` value). Confirmed the Reward
  Orchestrator is not needed for this slice — Bonus Engine fulfills
  directly through `wallet`/`ledger`. Flagged a stale governance entry:
  `project-status.md`'s "Blocked stages" table still said Bonus Engine
  was fully blocked, inconsistent with Stage 4H-A's own completion —
  corrected in this stage's governance update.

### Wave 2 — cross-document consistency review (code-reviewer) and scope-discipline review (product-owner-proxy)

**code-reviewer** reviewed all ten Wave-1 documents plus the draft
master synthesis document for genuine cross-document contradictions,
mirroring the exact "seam failure" pattern Stage 4H-A's own Wave-2
review found (~20 P1 contradictions in that stage's smaller
parallel-authored set) — at larger scale here, given ~8,000+ lines
across 10 documents. Found 14 findings (F1-F14: 1 P0, 8 P1, 4 P2, 1
consolidated P3 list). **All P0/P1/P2 findings fixed in-place**, each
with an explicit "Wave-2 review correction" callout naming the finding
and the fix, never a silent edit:

- **F1 (P0)** — ADR 0036 required `app.hierarchy_node_id` set for an
  entire retail transaction (§2.5) while separately requiring it unset
  for the ledger posting engine and dual-scope config reads, and set
  again for the audit insert — an internal contradiction that would have
  manufactured P1 ledger drift by construction (the exact failure ADR
  0035 §1.3 warned about) or made every counter operation fail closed
  permanently. Fixed by adding an explicit three-phase transaction model
  (authorization phase scoped, posting phase unscoped under the existing
  tenant/service posting path, audit phase re-scoped or parameterized).
- **F2 (P1)** — payments' and backend's documents required only the
  cashier's own principal for a money-touching counter operation,
  contradicting doc 26/ADR 0036's two-principal (terminal + cashier)
  requirement, and silently defeating ADR 0035's terminal-identity-based
  idempotency namespace-squatting mitigation (which depends on a
  terminal credential existing to resolve identity from). Fixed across
  both documents.
- **F3 (P1)** — two incompatible deposit/withdrawal flow shapes (ADR
  0035/doc 26 assumed a counter-originated flow with no prior player
  session; payments/backend assumed only a player-pre-request flow,
  under which a player without an app session could never deposit at a
  counter at all) and three different idempotency-key encodings across
  four documents. Resolved: the counter-originated flow is primary, the
  player-pre-request flow is an optional second entry point converging
  on the identical posting and key; ADR 0035's `internal/ledger`-native
  `(tenant_id, idempotency_key)` encoding is authoritative (ledger-
  finance holds the financial-accounting veto per the ADR 0032
  precedent), corrected in docs 04/12/26 which had proposed a
  provider-callback-shaped key that doesn't fit a terminal (no
  `ProviderCapability`, no signature-verified callback, no counterparty
  statement).
- **F4 (P1)** — four independently-proposed, mutually inconsistent
  `risk.Operation` name sets across ADR 0031/ADR 0035/doc 26/payments,
  plus a discovery that an outbound agent-settlement money path has no
  `Operation` gating it under any of the proposed sets. Resolved:
  `retail_deposit`/`retail_withdrawal`/`retail_funding` is authoritative
  (risk owns `Operation` naming per its own extension model), corrected
  in all four documents including renaming ADR 0035's own
  `agent_float_transfer` to `retail_funding`; risk's two initially-
  conditional values (`retail_deposit`/`retail_withdrawal`) were also
  resolved from CONDITIONAL to unconditionally-required, since ADR
  0035's Invariant R1 had already definitively answered the decision
  rule risk's own document posed. The ungated settlement-path gap
  recorded as an open risk, not resolved.
- **F5 (P1)** — risk's `hierarchy_level` scope dimension and its
  "an `agent`-level node may fund at most €X" framing contradicted
  architect's explicit rejection of any level/ladder concept (a node has
  a type, never a position) — architect holds the structural veto.
  Renamed to `hierarchy_node_type` throughout ADR 0031, with a new
  tenant-required CHECK constraint added (a platform-wide rule cannot
  reference a tenant-authored type code, since two tenants may define
  the same code meaning different things).
- **F6 (P1)** — backend's API document contradicted ADR 0036 in three
  load-bearing ways: claimed hierarchy scope introduces "no second
  isolation primitive" when ADR 0036 makes closure-table RLS a genuine
  second, authoritative RLS dimension; proposed a `hierarchy_node_id`
  JWT claim that ADR 0036 explicitly rejects by name (converts
  revocation into an advisory one); proposed extending `staff_users`
  with a column that ADR 0036 explicitly rejects in favor of an
  effective-dated N:M assignment table (a single column cannot express
  one cashier assigned to more than one node over time). All three
  corrected to match ADR 0036, which holds the RBAC/RLS veto.
- **F7 (P1)** — player-registration provenance was a three-way
  disagreement: identity-compliance said audit metadata alone made a
  new table unnecessary; architect/security required a dedicated
  `retail_player_origins` table; backend independently invented a third,
  different FK. Resolved as two complementary, non-competing mechanisms
  — `retail_player_origins` (attribution source of record, what
  security's subtree-scoped player accessor and commission attribution
  actually query, since a JSONB audit value cannot be an indexable join
  target or RLS predicate) and `registration_channel` (identity-
  compliance's own additive column on `player_accounts`) — corrected in
  all three documents; backend's third mechanism withdrawn.
- **F8 (P1)** — ADR 0036's own ancestor-suspension check (suspending a
  Partner must cascade to its cashiers) was silently defeated by ADR
  0036's own closure-table RLS policy: the check's query, run during
  scope resolution, could only see rows the caller's *own* resolved
  scope already permitted — so it always returned zero rows regardless
  of real suspension data, making `NOT EXISTS` always true (fail-open,
  not fail-closed, exactly inverting the control's purpose). Fixed with
  a `SECURITY DEFINER`-style accessor design that reads the closure
  table for this one specific, audited check without going through the
  caller-scoped policy at all.
- **F9 (P1, safety-critical)** — payments' retail deposit-confirmation
  handler never called `rg.EvaluateEligibility` anywhere and ran the
  Risk/hierarchy-limit check first, contradicting the fixed
  RG-first-then-Risk gate order every sibling document (ADR 0035, ADR
  0036, ADR 0031, doc 11) states as binding. Fixed to the correct order,
  with an added clarification for why the withdrawal `complete` handler
  is the one legitimate exception (it discharges an already-RG/Risk-
  cleared decision from `approved`, not a new value-crediting event).
- P2s F10-F13 fixed: data-analytics' reporting document hard-coded a
  per-level report-scope table ("Agent = itself only, a leaf") and a
  fixed node-type enum, both contradicting architect's rejection of any
  level/ladder concept and doc 26's own seed data (an Agent is not a
  leaf) — replaced with the uniform "every node's scope is itself plus
  descendants" rule, and its hierarchy-storage open decision closed
  (architect had since settled on adjacency-list-plus-closure-table).
  Identity-compliance's cross-tenant self-exclusion framing was built on
  a premise (a hierarchy might span tenants) that architect's own
  document had since closed in the opposite direction (a network never
  spans tenants; a tenant may run several networks) — corrected in both
  doc 05 and doc 11. The anonymous-play assumption (every retail player
  is an identified `PlayerAccount`) was stated silently in doc 11's
  binding RG mechanism and ADR 0036's non-bypass guarantee, when three
  other documents (ADR 0031, ADR 0035, doc 26) already escalate it as a
  genuinely open human decision — made explicit in both places. Doc 11's
  offline-retail exception floated a specific mechanism shape (a
  pre-fetched "provisionally clear" token) that three other documents
  independently pre-reject by name — reframed so the human decision is
  "no offline" vs. "a genuinely new, explicitly-designed exception,"
  not between two options one of which is already vetoed.
- F14's consolidated one-line drift items: stale ADR 0031 §12-vs-§16
  citations, `scope_source: "tenant_root"` vs `"network_root"` naming
  drift from ADR 0036's own mid-task multi-network correction, closure-
  table column-name drift (`ancestor_id` vs `ancestor_node_id`), a qa
  `OPEN DECISION` (withdrawal exceeding float) that ADR 0035 had already
  resolved, and a stale "retail actors are likely not `StaffRole`s"
  hedge in ADR 0031 that ADR 0036 had since decided otherwise — all
  fixed. Three purely cosmetic naming-drift items (a shift/session table
  named three different ways across three documents; backend's
  float-inquiry endpoint conflating the ledger-side `agent_float`
  projection with the physical till, which ADR 0035 explicitly is never
  a ledger account; a stale note in bonus doc 10) recorded rather than
  fixed, to avoid further scope expansion in an architecture-freeze
  stage — no correctness impact, to be reconciled if/when retail
  implementation is scoped.

**product-owner-proxy** independently re-confirmed the Blueprint-anchor
finding (read the full 20-page Blueprint text directly rather than
trusting architect's/ledger-finance's prior claims: zero occurrences of
retail/land-based/agent/POS/terminal/kiosk/shop/outlet/voucher as either
a whole word or a concept) and credited every specialist for correctly
labeling retail as human-directed scope, never a Blueprint requirement.
Gave a concrete recommended MVP-vs-deferred split for the Orchestrator's
synthesis document (a fixed 2-3 level hierarchy for the first workflow
even though the schema stays depth-agnostic; counter deposit only, no
withdrawal in the first slice; prefunded float only, no credit line; a
single flat commission rate, no tiering/override cascade; no
agent-to-agent transfers; single currency; no offline; no anonymous
play; franchised-till model only) and one scope-creep finding: ADR
0035 §5's commission-accounting machinery (a periodic-run mechanism with
its own idempotency scheme, an accrual→payable→capitalization state
machine, full hierarchical-override-cascade posting) is more fully
designed than its own unresolved commercial terms (§5.2's rate/base)
justify — the document defers the one number that would make the design
real while fully designing the machinery around that unknown number.
Applied: ADR 0035 §5's status downgraded from "RESOLVED (architecture)"
to "documented for future reference, not binding" for §5.3/§5.4
specifically, while keeping §5.1's cheap, boundary-setting pool-
separation invariants frozen. Also confirmed no compounding dependency
on the already-deferred Gamification Engine anywhere in the retail
design (grepped every retail document for gamification/mission/
tournament/leaderboard/badge/achievement/streak — every hit was either a
citation of the Stage 4H-A precedent or an explicit "reuse as-is, no
retail-specific gamification concept proposed" statement).

### Master synthesis document

`docs/architecture/27-stage-4h-b0-scope-and-implementation-plan.md`
(new, authored directly by the Orchestrator per this project's
established "Orchestrator authors genuinely cross-cutting connective
documents directly" pattern), covering the directive's all 25 numbered
deliverables: exact MVP scope (Bonus Engine's 5-type first slice,
independently authorizable; Gamification and Retail implementation both
explicitly NOT authorized), exact deferred scope, retail architecture,
hierarchy/agent-network architecture, financial model, RBAC model,
reporting permission model, online+retail shared-domain model (an 8:7:1
reuse/extend/new ratio across 16 assessed domains — architect's own
stated test of whether "one platform, not two products" is real), bonus
implementation plan, Gamification implementation plan (fully deferred,
unchanged), canonical activity/event model, external sportsbook
boundary, Reward Orchestrator minimum boundary (not built, unchanged
from Stage 4H-A), database entities/relationships, API surface,
migration plan (only the Bonus Engine's is authorized to exist in
detail), package/domain ownership, test strategy, security/RLS strategy,
reconciliation strategy, audit requirements, dependencies between
stages, an aggregated P0/P1/P2 risk register (8 P0s, all human/legal/
cross-specialist decisions), 14 human business decisions still required,
and a recommended implementation order (Bonus Engine independently
ready; a "Retail-Legal" human/business workstream must resolve licensing/
node-float/anonymous-play questions before any retail engineering stage
can even be scoped).

### Governance updates

`docs/governance/ownership.md` — registered the new Retail/Agent Network
domain (architecture: architect; implementation owner: an open decision
between backend and a future dedicated `retail` specialist; financial
accounting: ledger-finance; RBAC/RLS/audit: security) and corrected the
Bonus Engine row's stale "explicitly blocked" note to reflect the
qualified-lifted gate. `docs/governance/task-registry.md` — new Stage
4H-B0 section, 14 rows (4HB0-01 through 4HB0-14). `docs/governance/
project-status.md` — new Stage 4H-B0 section; corrected the stale
"Blocked stages" table entry for Bonus Engine that bonus-engine's own
Wave-1 review flagged; added a new "Retail (agent-hierarchy network)"
blocked-stage entry; added three new items to "Human decisions required
before production launch."

### Files touched this stage

New: `docs/architecture/26-retail-operations-architecture.md`,
`docs/decisions/0035-retail-agent-network-accounting.md`,
`docs/decisions/0036-retail-hierarchy-rbac-and-audit.md`,
`docs/architecture/27-stage-4h-b0-scope-and-implementation-plan.md`.

Extended (new sections/subsections appended or corrected in place, prior
content preserved): `docs/architecture/02-domain-and-service-
boundaries.md`, `04-api-architecture.md`, `05-identity-architecture.md`,
`07-payments-architecture.md`, `10-bonus-engine-architecture.md`,
`11-kyc-aml-rg-architecture.md`, `12-audit-reporting-architecture.md`,
`docs/decisions/0031-risk-and-limits-engine.md`, `docs/testing/
testing-strategy.md`.

Governance: `docs/governance/ownership.md`, `task-registry.md`,
`project-status.md`, `docs/progress.md` (this entry), `docs/active-
stage.md`.

No code, migration, or test files were created or modified this stage —
`go build ./...` re-run after all edits and remains clean (docs-only
diff, verified at every intermediate commit).

### Next stage

Not started. Per this stage's own directive ("This stage must NOT
automatically proceed to implementation. Wait for explicit approval
before Stage 4H-B1") and CLAUDE.md's stage-gate rule, no implementation
stage has been started: not Stage 4H-B1 (Bonus Engine implementation),
not a "Retail-Legal" workstream, not Stage 4H-B2/4H-B3 (retail
architecture hardening/implementation). Explicit human authorization is
required to name which stage(s) to begin next.

---

## Stage 4H-B0-R1 — B0 Gate Corrections and Finalization

Directive: "STAGE 4H-B0-R1 — B0 GATE CORRECTIONS AND FINALIZATION,"
issued after review of the Stage 4H-B0 completion report identified
specific errors requiring correction before any implementation stage is
authorized. Explicit instruction: **do NOT start production
implementation** — this is a correction/finalization stage over the
existing B0 architecture set, not a new architecture-design stage. The
Master Orchestrator role, specialist-delegation rules (no silent
cross-domain overwrites, explicit correction callouts, no unilateral
redesign of previously-approved architecture), and the prohibition on
starting Bonus/Retail/Gamification production implementation this stage
all applied unchanged from prior stages.

### Correction 1 — Bonus Implementation Gate

Stage 4H-B0's report stated Stage 4H-B1 was "independently
authorizable" while its own §23 disclosed that ADR 0021's unresolved
rounding/precision decision blocks precise computation for 3 of the 5
first-slice bonus types (deposit, reload, cashback all multiply money by
a percentage) — a direct contradiction. Corrected in
`docs/architecture/27-stage-4h-b0-scope-and-implementation-plan.md` §1.1:
**Stage 4H-B1 is CONDITIONALLY READY, not independently ready**, gated
on all three of: (1) the ADR 0021 rounding/precision decision explicitly
resolved by a human, (2) the Risk dependency for the `bonus_conversion`
`Operation` completed and reviewed, (3) no other P0/P1 financial
dependency remaining.

`risk` verified item (2) directly against repository state rather than
inferring it from prior documentation: `bonus_conversion` is **NOT
STARTED — zero of ADR 0031 §16's six extension-process steps
complete** (no `Operation` Go constant, no migration 0041 CHECK entry,
no `risk_handlers.go` `RequireOneOf` entry, no OpenAPI enum entry —
confirmed missing in three locations, not the two an earlier draft
assumed: the POST request schema, the `RiskRule` response schema, and
the `GET .../rules` query-parameter enum — no ledger transaction-type
mapping, no `internal/bonus` enforcement call site since the package
does not exist). This sits on the first slice's critical path since all
five in-slice bonus types run through `completed → converted`. `risk`
also confirmed item (3): `bonus_conversion` is the only Risk-owned
P0/P1 dependency for the five-type first slice. Full verified findings
recorded as new ADR 0031 §16a.

`ledger-finance` resolved item (1) not by inventing or selecting a
rounding rule, but by stating the decision precisely as three separable
questions in `docs/decisions/0021-multi-asset-accounting.md`'s new
"Rounding and precision" section: **Q1** rounding direction (six
neutrally-presented options — round-half-up, round-half-even, truncate,
ceiling, directional-by-beneficiary, truncate-and-carry-remainder — each
with its trade-off stated, none selected); **Q2** rounding point/
precision handling (round once at the end vs. at each computation step;
how the sub-minor-unit residue is treated — discarding is stated as not
an available option, since it would break `SUM(DEBITS)==SUM(CREDITS)`);
**Q3** uniformity/scope (one platform-wide rule vs. per-asset/
per-jurisdiction/per-direction variants). A dependent-site table lists
every place the answer binds: FX, deposit/reload/cashback bonuses,
wagering-contribution weighting, agent commission (ADR 0035 §5.2),
physical-cash rounding (ADR 0035 §9.3), and split prizes. What's fixed
regardless of the answer: `NUMERIC` only, exact recomputability from
stored inputs including a stored/versioned rounding-rule identifier, no
silent truncation, one shared rounding helper across every consumer.
`ledger-finance` selected nothing — an industry-practice note is
explicitly labeled an observation, not a recommendation.

### Correction 2 — Retail P0 List Reclassified

The Stage 4H-B0 "8 P0 decisions block retail" framing conflated genuine
human/business/legal decisions with mandatory engineering acceptance
criteria. Corrected in doc 27, restructuring the former single P0 list
into two subsections: **§23A** (3 genuine human/business/legal
decisions — retail licensing/jurisdiction status; confirmation of the
proposed node-owned `agent_float` extension to ADR 0007, since the
design is now drafted and only approval remains a human decision;
anonymous/bearer retail play policy by jurisdiction) and **§23B**
(mandatory engineering acceptance criteria requiring no human input,
including the eight named in the directive — fail-closed hierarchy RLS
with no OR-NULL escape; no retail role holding `PermStaffManage`;
RG-before-Risk ordering; closure-table write protection; server-side
terminal credential resolution; POS idempotency namespace protection;
offline fail-closed baseline — plus the existing `audit_log` RLS
extension and the delegated-limit-authoring default, whose *default*
needs no human input and only a *change* to that default would).

### Correction 3 — Agent Float Accounting Formalized

`docs/decisions/0035-retail-agent-network-accounting.md` gained new
**§1.3.1** (`ledger-finance`), formalizing the ADR 0007/agent-float
relationship exactly per the directive's intended architecture: Player
Wallet (ADR 0007) is completely unchanged and remains strictly
player-owned; agent float is explicitly NOT a Player Wallet but a
hierarchy-node-owned operational financial account represented through
the SAME authoritative double-entry ledger, never a second ledger;
agent float must never be confused with a physical till/cash drawer
(physical cash stays a fulfillment/custody concern, re-confirmed against
§6.2, with the sharper argument added that a retail deposit *reduces*
float while *increasing* till cash — they cannot be two views of one
number); all financial movements stay in the one ledger.

`ledger-finance` drafted the minimum additive schema change: `ledger_
accounts` gaining a nullable `hierarchy_node_id` column, a `CHECK
(num_nonnulls(wallet_id, hierarchy_node_id) <= 1)` mutual-exclusion
constraint, and a second CHECK binding `account_type` to the correct
owner column — explicitly **NOT IMPLEMENTED**, illustrative SQL only, no
migration written. Reading the actual migration
(`migrations/0020_create_ledger_accounts.up.sql`) rather than trusting
the original draft, `ledger-finance` found and corrected a load-bearing
error: an earlier claim that the amendment "changes no existing
constraint" was **false** — three existing objects (the house-level
partial unique index, and both status CHECKs) encode "house-level" as
the bare predicate `wallet_id IS NULL`, which a node-owned row would
also satisfy, silently collapsing every hierarchy node's `agent_float`/
asset into one shared row per tenant — precisely the P0 failure the
amendment exists to prevent. The corrected proposal widens the index
predicate to `wallet_id IS NULL AND hierarchy_node_id IS NULL` and
replaces the two status CHECKs with one equivalent to the existing
behavior. A pre-existing latent hole (nothing today prevents a
`player_cash` row with `wallet_id IS NULL` from being silently treated
as house-level/tenant-shared) is closed as a side effect.

Because ADR 0007 is a human-approved decision, the amendment was routed
through review before being treated as anything more than a proposal.
**`architect`** reviewed and added ADR 0035 §1.3.2: verdict **sound,
with caveats** — the SQL was verified byte-for-byte accurate against the
actual migration, the shape does not redefine `Wallet` or create a
second ledger, `hierarchy_nodes` (doc 26 §5.1, itself still
`NOT IMPLEMENTED` — no migration exists yet) does carry `id`/`tenant_id`/
`status` as the proposal assumes, and the CHECK-enumeration approach
(vs. a registry table) matches the platform's existing precedent
(`ledger_transactions.transaction_type`). Confirmed the generic
hierarchy model and shared-platform-model statements remain accurate,
and confirmed the `ledger-accounting-model.md`/ADR 0032 §2 "house-level
== `wallet_id IS NULL`" shorthand would need matching edits in the same
future change if this amendment is approved (not edited now — out of
scope). **`security`** reviewed in parallel and added ADR 0035 §1.3.3:
verdict **safe, with implementation-time caveats** — confirmed by direct
reading of the RLS policies (not on trust) that a player cannot read an
agent_float row under current policy, that `ledger_accounts` has `FORCE
ROW LEVEL SECURITY` reinforcing this even for the table owner, that the
three new CHECKs are closed-form with no OR-NULL fail-open pattern, that
the composite FK's MATCH SIMPLE reasoning is sound with no cross-tenant
leakage (but must ship in the same migration as the column), that the
widened index predicate closes the schema-level collision (with the
caveat that the Go-level get-or-create lookup must also be updated to
filter on `hierarchy_node_id`), that the pre-existing owner-family hole
is real and closed, and that no audit-logging gap exists.

The amendment remains **`NOT IMPLEMENTED`** and explicitly requires
human approval before any migration is written, since it changes the
practical shape of a table whose broader design traces to human-approved
ADR 0007/0019. §1.3 stays an `OPEN DECISION` — this stage changed its
*form* (a concrete, reviewed proposal now exists), not its *status*.
`Wallet` itself was not redefined anywhere.

### Correction 4 — First Retail Product Baseline Formalized

New doc 27 §1.3a records the baseline for the first retail
implementation slice: identified players only, no anonymous/bearer
play, online connection required, no offline/store-and-forward
gambling, single retail currency initially, cash deposit, cash
withdrawal, a fixed shallow hierarchy for the first contracted operator
(configurable hierarchy architecture retained underneath), no automated
commissions initially, no agent-to-agent float transfer initially, no
direct bank agent settlement initially unless separately approved, no
proxy/assisted play initially. Recorded explicitly as implementation-
scope constraints for the first slice, not claims that every
jurisdiction permits this model — anonymous play, offline, and proxy
play remain evaluable later as separate jurisdiction-specific product/
legal decisions.

### Correction 5 — Generic Hierarchy Model Preserved

Reaffirmed unchanged from Stage 4H-B0: Node Type, Structure, and
Capability remain three separate concepts, never collapsed; Operator/
Partner/SuperAgent/Agent remain seed/configuration examples, never
hardcoded schema roles; Player is not a hierarchy node (attribution via
`retail_player_origins`); Cashier is an authenticated staff identity
assigned to a hierarchy node; Terminal is a service principal (ADR 0014
option 2); money-touching retail operations require both authenticated
principals per ADR 0036.

### Correction 6 — Online + Retail Shared Platform Model Formalized

Doc 27 §9 now states explicitly that Online and Retail are operating
channels/surfaces of one platform, sharing Person, PlayerAccount,
Identity Resolution, Wallet, Ledger, Risk, Responsible Gaming, KYC,
Payments architecture, Bonus, Audit, Reporting, and Reconciliation.
Retail terminals/POS are API clients of the platform. `architect`
independently confirmed this statement's accuracy against doc 26/doc 02
during its schema-amendment review. No second retail financial system
exists anywhere in the design.

### Correction 7 — Reporting Requirement Preserved

Doc 27 §8 restates that Back Office and future retail/agent frontends
use the same underlying reporting facts and reporting API surface, with
visibility controlled by authorized tenant + hierarchy subtree — never a
separate retail reporting calculation and never a hard-coded
per-level visibility rule. An illustrative table maps Operator/Partner/
Super Agent/Agent/Cashier onto the one uniform "itself plus authorized
descendants/permissions" rule.

### Correction 8 — Retail Financial Movement Model Preserved

Doc 27 §6 restates ADR 0035's headline principle (a retail cash deposit
is a transfer of existing liability — `Dr agent_float(node) / Cr
player_cash(wallet)` — never a PSP deposit) and that retail withdrawal
continues through the existing withdrawal hold/state-machine
architecture with a cash-at-cashier fulfillment channel. An explicit
"retains, without exception" list was added: double-entry, DB-enforced
idempotency, server-side authorization, RG-before-Risk ordering, audit
recording, reconciliation against the same ledger, and the same
concurrency-safety discipline as every other financial domain.

### Correction 9 — Commissions Kept as Future Architecture

New doc 27 §9a states explicitly that automated commission calculation,
accrual, payout, and cascade mechanics are **not built in the first
retail slice**, and lists the six commercial terms that must be defined
first: rate, base, hierarchy cascade, overrides, settlement frequency,
tax treatment. The commission accounting architecture itself remains
documented in ADR 0035 as future architecture, not implemented.

### Correction 10 — Stage Dependency Graph Updated

Doc 27 §22 (and §25's summary) rewritten into exactly two independent,
parallel paths: **Path A** — B0-R1 → Bonus financial gate resolution →
Stage 4H-B1 Bonus Engine; **Path B** (in parallel) — B0-R1 →
Retail-Legal/Business Gate → Stage 4H-B2 Retail Architecture Hardening
→ Stage 4H-B3 Retail First Implementation. Gamification remains
deferred in full; the Reward Orchestrator remains deferred until a
second concrete reward-producing domain exists. Neither path is
authorized to begin by this stage.

### Correction 11 — Human Decision Register

Doc 27 §24 rewritten as a clean 15-item register containing only
decisions genuinely requiring human/business/legal input: (1) retail
licensing/jurisdiction structure, (2) whether hierarchy agents are
independent legal entities/sub-licensees or platform/company-operated,
(3) confirmation of the proposed node-owned `agent_float` extension to
ADR 0007, (4) anonymous/bearer retail play policy by jurisdiction, (5)
offline retail policy, (6) proxy/assisted play policy, (7) commission
commercial terms, (8) agent credit/post-pay policy, (9) franchised vs.
company-owned retail model, (10) cash AML thresholds by jurisdiction,
(11) KYC evidence requirements for retail presence, (12) terminal
ownership/fleet model, (13) whether hierarchy actors may author
subordinate risk limits, (14) confirmation of the first contracted
retail market/operator when known, and (15) the ADR 0021 rounding/
precision decision. An earlier draft's "confirm which stage to
authorize next" item was removed from this register — it is the
ordinary end-of-stage authorization request every stage ends with, not
a business/legal decision.

### Governance updates

`docs/governance/project-status.md`: corrected the "Blocked stages"
Bonus Engine and Retail entries to match the corrected gates above,
replaced the "8 P0 decisions block retail" paragraph with a pointer to
the §23A/§23B reclassification, added a new Stage 4H-B0-R1 section, and
updated the Completed-stages table and Active-stage pointer.
`docs/active-stage.md`: added a full Stage 4H-B0-R1 section at the top
with all 11 corrections, and annotated the superseded Stage 4H-B0
section as corrected rather than deleting it. `docs/governance/
task-registry.md`: added Stage 4H-B0-R1 rows (Orchestrator, `risk`,
`ledger-finance`, `architect`, `security`, Orchestrator finalization).

### Files changed this stage

`docs/architecture/27-stage-4h-b0-scope-and-implementation-plan.md`
(extensive corrections across §1.1, new §1.3a, §6, §8, §9, new §9a,
§22, §23→§23A/§23B, §24, §25), `docs/decisions/0021-multi-asset-
accounting.md` (new "Rounding and precision" section, `ledger-finance`),
`docs/decisions/0031-risk-and-limits-engine.md` (new §16a, `risk`),
`docs/decisions/0035-retail-agent-network-accounting.md` (new §1.3.1
`ledger-finance`, §1.3.2 `architect` review, §1.3.3 `security` review),
`docs/governance/project-status.md`, `docs/governance/task-registry.md`,
`docs/active-stage.md`, `docs/progress.md` (this entry).

**No production code, no migrations, and no implementation were started
or authorized this stage** — `go build ./...` re-run after all edits and
remains clean (docs-only diff).

### Next stage

Not started. Two independently authorizable next steps per the corrected
dependency graph: (1) resolve the Bonus financial gate (ADR 0021
rounding decision by a human, plus the `bonus_conversion` Risk
dependency) then authorize Stage 4H-B1; (2) resolve the 3 Retail-Legal/
Business decisions (§23A) then authorize Stage 4H-B2. Per this stage's
own explicit instruction ("Do NOT start production implementation yet
... STOP after this report"), neither is started or assumed.

**Stage 4H-B0-R1 correction**: this entry's "independently ready per
bonus-engine's own gate-check" characterization of Stage 4H-B1 was
corrected in Stage 4H-B0-R1 (see that stage's entry below) — this
document's own §1.1/§23 disclosed the ADR 0021 rounding dependency that
contradicted "independently ready." Retained here unmodified for the
historical record of what this stage concluded before that correction.

---

## Stage 4H-B0-R2 — Bonus Financial Gate Clarification

Directive: "STAGE 4H-B0-R2 — BONUS FINANCIAL GATE CLARIFICATION,"
issued after Stage 4H-B0-R1's completion, with explicit instructions:
**do NOT start Stage 4H-B1 implementation, do NOT write production code
or migrations.** Purpose: close the remaining financial-design gate for
Bonus implementation by preparing an exact human decision package for
ADR 0021 and verifying the remaining Risk dependency. Six specialists
(`ledger-finance`, `bonus-engine`, `risk`, `architect`, `security`,
`qa`) were dispatched in parallel, each instructed to verify against
current repository state at HEAD rather than trust prior-stage prose,
and none was authorized to select an answer to the rounding decision on
the user's behalf.

### 1. Authoritative sources read

`docs/decisions/0021-multi-asset-accounting.md` (the rounding "OPEN
DECISION" section added in Stage 4H-B0-R1), `docs/architecture/
10-bonus-engine-architecture.md`, `docs/decisions/0031-risk-and-limits-
engine.md` §14-§18/§16a, `docs/decisions/0032-bonus-accounting.md`,
`docs/architecture/ledger-accounting-model.md`, `reconciliation-
model.md`, and Stage 4H-B0/4H-B0-R1 documentation, all read at the
current commit (`42d50da`, working tree clean at stage start).

### 2. The exact remaining ADR 0021 decision

Confirmed genuine, not manufactured: ADR 0021's rounding/precision
`OPEN DECISION` (added in full in Stage 4H-B0-R1) is stated as three
separable questions — **Q1** rounding direction (6 neutrally-presented
options: round-half-up, round-half-even, truncate, ceiling,
directional-by-beneficiary, truncate-and-carry-remainder), **Q2**
rounding point/precision handling (round once at the end vs. at each
step; how the sub-minor-unit residue is treated), **Q3** uniformity/
scope (one platform-wide rule vs. per-asset/jurisdiction/direction
variants). Answering only Q1 leaves the computation non-deterministic.
It gates deposit-match, reload, and cashback bonus grant-amount
computation directly, and the generic wagering bonus's and coupon's
wagering-requirement/contribution-tracking computation indirectly (see
§4 below for the precise breakdown — `bonus-engine` found this is
**not** a blanket "all five types identically affected" situation).

### 3. Options presented, none selected

`ledger-finance` produced two numerical worked examples grounded in the
platform's actual representation (`NUMERIC(38,0)` minor-unit integers,
per-asset `decimal_exponent`): a 50% deposit-match on a €133.33 deposit
landing exactly on a €66.665 tie (showing each of the six Q1 options'
result: €66.67/€66.66/€66.66/€66.67/€66.67/€66.66-plus-carried-remainder
respectively), and a repeating 7.3% weekly cashback on €16.90 net loss
(showing the truncate-and-carry mechanism accumulate a leftover fraction
across three weeks until it releases an extra cent on the third). Found
that options A-E have literally no separable residue to drop at the
bonus-grant/cashback posting site — the mirrored `promo_liability`/
`player_bonus` legs are always posted with the same already-rounded
integer (ADR 0032's Rule B2 mirror construction) — so `SUM(DEBITS)==
SUM(CREDITS)` is protected by construction for those five options.
Option F introduces genuine new financial state (a per-player remainder
accumulator) that would need the same concurrency-safe, idempotent,
auditable, reconciliation-capable discipline as the ledger itself, plus
an explicit, still-undecided policy for what happens to an unreleased
remainder if the originating bonus is reversed, forfeited, or the
player self-excludes before it's paid out. Flagged a concrete
implementation trap: PostgreSQL's default numeric-to-integer cast
silently implements round-half-up (Option A), so whichever option is
selected must be built as an explicit function in the platform's one
shared rounding helper (ADR 0021 point 4), never left to an implicit
cast or language/database default. No option was selected — `ledger-
finance` is not authorized to make this call.

### 4. Bonus type verification

`bonus-engine` produced a precise per-type table, correcting a
would-be blanket assumption: the rounding decision affects the **grant
amount itself** for Deposit bonus, Reload bonus, and Cashback (all
three compute `amount × percentage` directly), but for the generic
Wagering bonus and Coupon the grant/face amount is a flat, pre-set
value — rounding-independent — and the decision affects **only** the
derived wagering-requirement/contribution-tracking computation that
follows activation. `bonus-engine` also independently re-derived (not
merely trusted) that all five in-slice types reach `completed →
converted`, confirming `risk`'s ADR 0031 §16a claim that the
`bonus_conversion` Risk dependency blocks all five types, even though
only three have their payout amount affected by the rounding decision
itself. No new bonus type was added; the MVP was not expanded.

### 5. Bonus accounting compatibility confirmed

`ledger-finance` and `architect` jointly confirmed the five-type first
slice remains fully compatible with: the append-only ledger,
double-entry accounting, the existing wallet architecture, bonus
liability/expense treatment (`promo_liability`/`bonus_expense`),
idempotency, compensating entries (reversals), multi-asset
representation, Risk, RG, audit, and reconciliation — regardless of
which rounding option is eventually chosen. The one non-trivial
compatibility caveat is scoped to option F specifically (its remainder
accumulator needs new reconciliation/audit discipline of its own, and
its reversal-time policy remains a genuinely open, separate follow-up
question if F is chosen) — this does not block options A-E.

### 6. `bonus_conversion` Risk dependency verification

`risk` re-verified, against the current repository state at HEAD (no
code had changed since Stage 4H-B0-R1's commit), that `bonus_conversion`
remains **NOT STARTED — zero of ADR 0031 §16's six extension-process
steps complete**. No documentary correction was needed; §16a was left
unmodified. Produced a formatted six-step checklist (owning specialist,
affected file(s), whether documentation or code, upstream dependency,
required test, and whether the step can land before Stage 4H-B1 is
authorized) — steps 1-4 (migration CHECK widening, Go constant, HTTP
allowlist, OpenAPI enum in all three locations) are `risk`-owned and
structurally independent; step 5 (ledger transaction-type mapping) is
`risk`-owned but depends on `ledger-finance` finalizing the related ADR
0032 ledger CHECK widening, and may be deliberately deferred with a
disclosed fail-closed consequence; step 6 (the enforcement call site) is
`bonus-engine`-owned and cannot precede `internal/bonus` existing. This
checklist is explicitly **not implemented during this stage** and is
recorded in the decision sheet as informational context, not a decision
for the human.

### 7. Focused financial gate review — no additional blocker

`architect` performed a focused 12-area review (ledger accounting,
wallet architecture, idempotency, concurrency, Risk, RG, audit, RLS,
reconciliation, transaction/account types, multi-asset precision, bonus
conversion) against the current repository state, verifying claims
directly (e.g. reading the actual `ledger_accounts`/`ledger_transactions`
CHECK constraints in migrations 0020/0021) rather than trusting prior
documentation. **Result: no additional P0/P1 blocker found** beyond the
two already-known gates. One non-blocking documentation
cross-reference gap was flagged (ADR 0034 §2's RG mid-lifecycle nuance
is already functionally answered by doc 10 §5's mechanism but the two
documents don't explicitly cross-reference each other) — not a new gate,
not fixed this stage per the directive's "do not reopen unrelated
architecture" instruction. `security` and `qa` each independently
confirmed no additional blocker in their respective areas (security:
rounding-rule config permissions/audit, `bonus_conversion` call-site
authorization, bonus-lifecycle audit coverage, bonus-table RLS; qa:
financial test-matrix coverage), each flagging only small,
explicitly-non-blocking items for engineering's own backlog (a
`reversed`-transition audit-table row; a handful of test-plan
additions), none requiring a human decision or new architecture.

### 8. Human Decision Sheet

New document: `docs/architecture/28-bonus-financial-gate-decision-
sheet.md`. Written for a non-accountant business owner. Contains only
the three ADR 0021 rounding sub-decisions (DS-1 direction, DS-2
rounding point/precision, DS-3 uniformity/scope) with plain-language
options, financial consequences, and worked numerical examples for
each; the precise bonus-type impact table; the `bonus_conversion`
six-step checklist (explicitly labeled informational, not a decision
for the human); confirmation that no other blocker was found; and a
table of the six specialist reviews completed this stage. No rounding
option is selected or recommended anywhere in the document.

### 9. Governance update

`docs/governance/project-status.md`: updated the Bonus Engine "Blocked
stages" entry with a Stage 4H-B0-R2 update paragraph, added a full Stage
4H-B0-R2 section, and updated the Completed-stages table and Active-stage
pointer. `docs/active-stage.md`: added a full Stage 4H-B0-R2 section at
the top. `docs/governance/task-registry.md`: added Stage 4H-B0-R2 rows
(Orchestrator, `ledger-finance`, `bonus-engine`, `risk`, `architect`,
`security`, `qa`, Orchestrator finalization). Stage 4H-B1 is explicitly
recorded as remaining BLOCKED and NOT authorized — this stage produced a
decision package, it did not make the decision or approve the next stage.

### 10. Final verification

`go build ./...` re-run after all edits and remains clean (docs-only
diff — one new file, `docs/architecture/28-*.md`; the rest are edits to
`docs/governance/project-status.md`, `docs/active-stage.md`,
`docs/governance/task-registry.md`, `docs/progress.md`). No production
migrations, no production Bonus code, no production Risk code, no
production API changes were created.

### Files changed this stage

`docs/architecture/28-bonus-financial-gate-decision-sheet.md` (new),
`docs/governance/project-status.md`, `docs/governance/task-registry.md`,
`docs/active-stage.md`, `docs/progress.md` (this entry). No specialist
edited a file directly this stage — all six reported findings back to
the Orchestrator, who synthesized them into doc 28 and the governance
updates, per the directive's "no specialist may silently change an
approved financial rule" and "no specialist may choose a human
accounting/business decision on the user's behalf" rules.

### Next stage

Not started. The human reviews `docs/architecture/28-bonus-financial-
gate-decision-sheet.md` and answers DS-1/DS-2/DS-3. Once recorded in ADR
0021, and once the `bonus_conversion` six-step checklist is completed as
part of Stage 4H-B1's own work, Stage 4H-B1 can be authorized. **Stage
4H-B1 is NOT authorized by this stage.**

---

## Stage 4H-B0-R3 — Bonus Rounding Decision Validation and Financial Gate Closure

Directive: "STAGE 4H-B0-R3 — BONUS ROUNDING DECISION VALIDATION AND
FINANCIAL GATE CLOSURE," issued after the human reviewed Stage
4H-B0-R2's decision sheet and proposed answers to DS-1/DS-2/DS-3.
Explicit instructions: do NOT start Stage 4H-B1 implementation; validate
the proposed decisions rather than blindly accepting them; if
mathematically or architecturally unsafe, explain why and stop before
recording; no specialist may silently change an approved financial rule
or choose a human accounting decision on the user's behalf. Six
specialists (`ledger-finance`, `bonus-engine`, `risk`, `architect`,
`security`, `qa`) were dispatched in parallel, each instructed to
validate against current repository state at HEAD, not to rubber-stamp
the proposal.

### 1. Specialist validation results

All six specialists reported findings back to the Orchestrator rather
than editing any file directly (consistent with this stage's "no
specialist may silently change an approved financial rule" instruction).
No specialist selected or altered DS-1/DS-2/DS-3. Summary of each:

- **`ledger-finance`**: validated DS-1 against exact/below-half/exact-
  half/above-half/positive/zero cases and confirmed negative amounts are
  not applicable anywhere in the current architecture (`ledger_entries.
  amount` is always strictly positive; direction is encoded by debit/
  credit side, not sign). Specified the exact algorithm: round half away
  from zero, `sign(x) × floor(|x| + 0.5)`, via one named shared function,
  never a bare `ROUND()` call or an implicit `NUMERIC(38,0)` column-scale
  coercion (confirmed PostgreSQL's own implicit coercion happens to match
  round-half-up for positive values today — a coincidence of today's
  Postgres behavior, not a specification). Validated DS-2 per bonus type
  with all 9 required sub-questions (input, calculation, intermediates,
  final boundary, resulting integer, residual, retry, reversal,
  reconciliation) for Deposit/Reload/Cashback, and found **no debit/
  credit imbalance possible**: because rounding happens once, upstream of
  both ledger entries, and the identical resulting integer posts to both
  sides of the balanced transaction, there is structurally no residue
  that needs a home for these three types — a clarification of ADR
  0021's own Q2 framing, which was written with FX/commission's genuinely
  divergent two-sided arithmetic in mind. Found and flagged a genuine
  edge case: a bonus computing to exactly 0 minor units cannot post
  (`amount > 0` CHECK) — a Bonus Engine eligibility/config question, not
  a rounding-rule question. Confirmed the existing architecture has **no
  remainder-accumulation mechanism anywhere** (searched
  `ledger-accounting-model.md`, `reconciliation-model.md`, ADR 0032 — none
  exists), so DS-1+DS-2 as proposed imply cashback is rounded
  independently each time with no accumulation, and recommended this
  consequence be surfaced back to the human rather than silently assumed.
- **`bonus-engine`**: worked through all 9 sub-questions for all five
  bonus types against doc 10/ADR 0032's actual posting shapes (not
  assumed), finding the "final monetary boundary" for Deposit/Reload/
  Cashback is precisely after both the percentage multiply and the cap
  comparison (`round(min(exact_%_result, cap))`, once) — a genuine
  ordering ambiguity DS-2 as worded doesn't pin down by itself, now
  resolved. Disambiguated the wagering-requirement target (a non-posted
  comparison threshold per ADR 0032 §3.1: "conversion-eligibility is a
  decision, not a movement") from per-game contribution weighting (which
  **is** monetary — it determines the actual cash/bonus split posted for
  a wagering event, per doc 10 §6) — resolving an ambiguity `ledger-
  finance` had separately flagged. Verified bonus-conversion determinism:
  conversion's posting amount is usually already exact (a `min()` of two
  already-integer values), so DS-2's rounding boundary is often a no-op
  there; flagged that a percentage-derived max-cashout cap is unspecified
  by any document (non-blocking). Flagged Coupon's Reward-axis shape
  (flat vs. %-based) as an open Stage 4H-B1 template-authoring question,
  unrelated to the rounding decision.
- **`risk`**: confirmed Risk evaluates only the **post-rounded** amount,
  settled by construction rather than left ambiguous — `RiskRequest.
  Amount`'s `int64` type cannot hold a pre-rounding exact value, and the
  one live precedent (`postBet`) already passes Risk the identical
  integer that becomes the ledger posting two lines later. No
  clarification to ADR 0031/`internal/risk` was needed. Re-verified all
  six ADR 0031 §16 artifacts directly against current repository state
  (commit `aa4a926`, confirmed docs-only via `git show --stat`) — still
  **NOT STARTED, zero of six steps**, unchanged since Stage 4H-B0-R1.
  Confirmed the rounding decision requires no Risk-side implementation
  now, per the directive's explicit "prefer keeping implementation for
  Stage 4H-B1" instruction.
- **`architect`**: validated DS-3 against ADR 0021's Q3 and confirmed no
  tension with ADR 0035 §9.3's cash-rounding note (a future override is a
  legitimate path, not an ungoverned second convention). Ran a full
  cross-document consistency pass and found no additional contradiction.
  Analyzed the current `assets` schema (migrations 0003/0006) directly
  and found it already generic/extensible in storage shape but missing
  the entire operational surface (admin API, RBAC, audit, eligibility
  columns) the new Asset/Currency Registry requirement needs — an
  additive extension, not a redesign. Designed the FX/Conversion boundary
  on paper (Registry / FX Rate Provider / Conversion Service / Ledger,
  kept structurally separate) and specified the immutable fields any
  future conversion must retain. Recommended a future ADR 0037 and a
  deferred future implementation stage. Confirmed no impact on Bonus
  Stage 4H-B1 or new blocker for Retail.
- **`security`**: confirmed the proposed rule is deterministic given
  `NUMERIC`-only inputs and one shared implementation, with the residual
  risk that "round half up" is a specification, not yet a shared
  implementation, until the one-helper requirement (ADR 0021 item 4) is
  actually enforced. Specified exactly where the applied rule/version
  must be stored: an immutable, append-only `rounding_rules` table (one
  composite Q1+Q2 version per row, never two independently-versioned
  axes) plus the applied identifier denormalized onto `ledger_
  transactions` at post time — the same rationale already used for
  `tenant_id`/`wallet_id`/`player_account_id`/`asset_code` on
  `LedgerEntry`. Flagged the Asset Registry's asset-creation/activation
  authorization boundary as undefined and consequential (larger blast
  radius than the existing `risk_rules` platform-wide-write-path gap,
  ADR 0031 §8) and recommended the eventual design evaluate a two-tier
  platform-registration/tenant-activation split; confirmed mandatory
  audit logging applies with no exception.
- **`qa`**: confirmed the proposed rule is exponent-agnostic by
  construction — the shared rounding function's contract needs no
  exponent parameter at all, since every amount is already an integer
  number of minor units before rounding is applied. Validated across
  EUR/USD (2-decimal), a 0-decimal case, BTC (8-decimal), USDT (6-decimal,
  live), and an 18-decimal case — the last two schema-legal but not yet
  populated in the live registry, needing synthetic test fixtures rather
  than live rows. Flagged that ADR 0021/doc 28's "cents" language could
  bias an implementer toward hardcoding 2 decimals, and that the
  negative-input contract ("ties away from zero" for negative amounts)
  should be specified even though no current bonus call site exercises
  it. Refined the Stage 4H-B0-R2 property-based test recommendation into
  9 concrete, implementable test categories. Confirmed explicitly: the
  Asset Registry/FX analysis causes zero change to the five-type first
  slice's test plan.

### 2. DS-1 validation

**Round-half-up, ties away from zero.** Validated against exact-integer,
below-half, exact-half, above-half, positive, and zero cases — all
trivially correct except the exact-tie case, which is the only place
DS-1 actually decides anything. Negative amounts are confirmed not
applicable to any current bonus posting (all `ledger_entries.amount`
values are strictly positive by schema; reversals invert an already-
posted positive integer, never store a negative). The negative-input
contract ("ties away from zero" — `-2.5 → -3`) was specified anyway, for
DS-3's future reuse by FX/commission. Confirmed PostgreSQL's own
implicit numeric-to-integer coercion cannot be relied on to define this
rule, even though it happens to produce the same result for positive
values today — the exact algorithm (`sign(x) × floor(|x| + 0.5)`, one
named shared function) is specified explicitly in ADR 0021 to remove any
ambiguity.

### 3. DS-2 validation

**Round once, at the final monetary boundary, full `NUMERIC` precision
until then, explicit function, never an implicit cast.** Validated per
bonus type: for Deposit/Reload/Cashback, the final boundary is precisely
after both the percentage multiply and the cap comparison. The wagering-
requirement target is a non-posted comparison threshold, not subject to
this boundary in the ledger-posting sense; per-game contribution
weighting is genuinely monetary and does have a boundary, at the
split-instruction computation. Generic Wagering bonus's and Coupon's
flat grant amounts are unaffected entirely. No debit/credit imbalance is
possible for any of the three percentage-based grant/payout types,
because rounding happens once, upstream of both ledger legs, which
receive the identical resulting integer. A zero-rounding-result edge
case and a percentage-derived-cap-at-conversion question were flagged as
non-blocking Stage 4H-B1 implementation details.

### 4. DS-3 validation

**One platform-wide rule by default, room for a future per-asset/
jurisdiction override if genuinely required.** Confirmed this correctly
answers ADR 0021's Q3 and creates no tension with ADR 0035 §9.3's cash-
rounding note. Requires no extra implementation cost now, since ADR
0021's existing stored/versioned rounding-rule-identifier requirement
already makes a future override safe (a new rule version is a new row;
old transactions keep citing their original identifier).

### 5. Cashback residual conclusion

The existing architecture has no remainder-accumulation mechanism
anywhere. DS-1+DS-2 as proposed and recorded mean: **each cashback
calculation is rounded independently and immediately, round-half-up, no
accumulation** (Option A). Building a truncate-and-carry mechanism
(Option B) would be new, unauthorized architecture requiring its own ADR
and human sign-off — not something DS-1/DS-2 imply or require. Recorded
explicitly in ADR 0021 rather than left as a silent assumption.

### 6. Wagering calculation conclusion

The wagering-requirement target (`bonus_amount × multiplier`) is a
comparison threshold internal to the Bonus Engine, never posted to the
ledger (ADR 0032 §3.1) — DS-2's "final monetary boundary" does not apply
to it in the posting sense, though the same deterministic `NUMERIC`
discipline still governs it. Per-game contribution weighting (`stake ×
contribution_%`) **is** monetary — it determines the actual `player_
cash`/`player_bonus` split posted for a wagering event — and has its own
rounding boundary at the split-instruction computation, capped by the
Grant's remaining bonus balance. This distinction, not stated explicitly
by either doc 10 or ADR 0032 before this stage, is now recorded in ADR
0021's resolved decision.

### 7. Multi-asset conclusion

The recorded rule is exponent-agnostic by construction and requires no
floating-point arithmetic at any step (`NUMERIC` throughout, matching
existing standing rules for exchange rates/percentages). Validated
across 2-, 0-, 6-, 8-, and 18-decimal exponents; the 0- and 18-decimal
cases are schema-legal but not yet populated by any live asset, so
Stage 4H-B1's test suite must exercise them via synthetic fixtures.

### 8. Ledger/reconciliation conclusion

No debit/credit imbalance, no unexplained ledger residue, and no
non-deterministic replay is possible from the recorded decision, for
the three percentage-based grant/payout types — confirmed structurally,
not merely asserted. No new or existing account needs to absorb a
rounding residue for Deposit, Reload, or Cashback. The FX conversion-
clearing account remains a genuine, separately-tracked `OPEN DECISION`
in ADR 0021, unaffected by and not required for the Bonus Engine's first
slice.

### 9. Risk conclusion

Risk evaluates the post-rounded, already-posted minor-unit integer only,
settled by construction (`RiskRequest.Amount`'s `int64` type). No
ambiguity was found or manufactured; no clarification to ADR 0031 or
`internal/risk` was needed or made.

### 10. Security/audit conclusion

The recorded decision is deterministic given `NUMERIC`-only inputs and
one shared implementation (the residual risk of independent
reimplementation is exactly why ADR 0021's "one shared rounding helper"
requirement is binding, not optional). The applied rule/version must be
stored in an immutable `rounding_rules` reference table plus denormalized
onto `ledger_transactions` at post time, mirroring the platform's
existing denormalization rationale for tenant/wallet/player/asset
columns on `LedgerEntry`. Historical calculations remain reproducible
indefinitely under this scheme, tying directly to determinism.

### 11. Exact remaining `bonus_conversion` work

Unchanged from Stage 4H-B0-R2's six-step checklist (ADR 0031 §16a),
re-verified against current repository state: steps 1-4 (migration CHECK
widening, Go constant, HTTP allowlist, OpenAPI enum in all three
locations) are `risk`-owned and structurally independent; step 5 (ledger
transaction-type mapping) is `risk`-owned but depends on `ledger-
finance`'s ADR 0032 ledger CHECK widening and may be deliberately
deferred with a disclosed fail-closed consequence; step 6 (the
enforcement call site) is `bonus-engine`-owned and cannot precede
`internal/bonus` existing. **Not implemented this stage** — the
rounding decision did not require it, per the directive's own
instruction to prefer keeping it for Stage 4H-B1.

### 12. Final B1 status

**Stage 4H-B1 is READY FOR HUMAN AUTHORIZATION after completion of
`bonus_conversion`.** No P0, no P1 financial blocker, no unresolved
accounting decision, no unresolved precision decision, no unresolved
rounding ambiguity remains.

### 13. Asset/Currency Registry + FX architecture (new confirmed requirement, analysis only)

`architect` found the current `assets` schema (migrations 0003/0006) is
already open/extensible in storage shape (no closed enum, no hardcoded
decimal count anywhere in `internal/`) but lacks the operational surface
the requirement needs: an admin API, an authorization/RBAC model for who
may register/activate an asset, mandatory audit logging on that
mutation, and additional per-asset eligibility columns (wallet/deposit/
withdrawal/settlement) that don't exist today. **Conclusion: an
additive extension is required, not a redesign.** Recommended: a future
ADR (0037, parallel to how payments/KYC/risk each got their own ADR) and
a dedicated future implementation stage, both recorded as deferred
(`docs/architecture/14-mvp-scope-and-roadmap.md`'s "Features deliberately
deferred" section; `docs/architecture/27-*.md` §26). The FX/Conversion
boundary was designed on paper only, keeping four components separate
(Asset/Currency Registry, FX Rate Provider, Conversion Service, the
Ledger's existing `ConversionOperation`) and specifying the immutable
fields (source/destination asset and amount, exchange rate, rate
timestamp, provider identifiers, conversion operation ID, precision,
rounding-rule version, fee/spread placeholders) any future conversion
must retain — a live FX provider response must never be the sole
historical source of truth. Confirmed three separate financial
boundaries exist (Bonus rounding, FX-conversion rounding, ledger
minor-unit normalization) sharing one implementation but not one
decision automatically, with no hidden double-rounding permitted.
Confirmed fail-closed behavior for missing/unconfigured FX rates as a
binding rule for the new ADR. **Confirmed: no impact on Bonus Stage
4H-B1; no new blocker for Retail** beyond the pre-existing, independently
-tracked conversion-clearing-account open decision (ADR 0021,
`ledger-accounting-model.md` §2). **Confirmed: no expansion of the Bonus
MVP, no FX implementation, no new payment/custody provider, no retail
implementation this stage.** Two P1 risks flagged for the eventual
Registry design: the asset-creation/activation authorization boundary is
undefined (`security` recommends a two-tier platform-registration/
tenant-activation split, larger blast radius than ADR 0031 §8's existing
platform-wide risk-rule gap); fail-closed FX behavior should be written
into the new ADR as a binding rule now rather than improvised later. Two
P2 items also flagged (documentation drift risk, addressed this stage;
missing test coverage for future new-asset onboarding, recorded for
`qa`'s backlog).

### 14. Governance update

`docs/decisions/0021-multi-asset-accounting.md`: the rounding/precision
decision recorded as RESOLVED, with the full algorithm, storage
specification, per-bonus-type clarifications, cashback-residual
conclusion, and non-blocking implementation-time items; the gate
statement updated to "READY FOR HUMAN AUTHORIZATION after `bonus_
conversion`." `docs/architecture/27-*.md`: §1.1, §22, §24 #15, §25
updated to reflect the resolved gate; new §26 added for the Asset/
Currency Registry + FX architecture analysis. `docs/architecture/
28-bonus-financial-gate-decision-sheet.md`: marked RESOLVED, decision
recorded, original content retained unmodified below as the historical
record. `docs/architecture/financial-domain-model.md`: the `Asset`
scoping-table row corrected from "Stage-1 seed set" framing to "open,
extensible platform registry." `docs/architecture/14-mvp-scope-and-
roadmap.md`: new deferred-features entry for the Asset/Currency Registry
+ FX architecture. `docs/governance/project-status.md`,
`docs/governance/task-registry.md`, `docs/active-stage.md`: updated to
reflect the closed Bonus gate and the new deferred requirement. **Stage
4H-B1 is explicitly NOT marked as implemented or approved anywhere.**

### 15. Final verification

`go build ./...` re-run after all edits and remains clean (docs-only
diff — no code, no migrations, no `internal/bonus`/`internal/risk`
changes). No documentation-consistency issue found on review of all
cross-references (ADR 0021 ↔ doc 27 §1.1/§24/§26 ↔ doc 28 ↔ project-
status.md all cite the same resolved decision consistently).

### Files changed this stage

`docs/decisions/0021-multi-asset-accounting.md`, `docs/architecture/
27-stage-4h-b0-scope-and-implementation-plan.md`, `docs/architecture/
28-bonus-financial-gate-decision-sheet.md`, `docs/architecture/
financial-domain-model.md`, `docs/architecture/14-mvp-scope-and-
roadmap.md`, `docs/governance/project-status.md`,
`docs/governance/task-registry.md`, `docs/active-stage.md`,
`docs/progress.md` (this entry). No specialist edited a file directly
this stage — all six reported findings back to the Orchestrator, who
synthesized them into the recorded decision and governance updates.

### Next stage

Not started. Stage 4H-B1 (Bonus Engine implementation) is READY FOR
HUMAN AUTHORIZATION once `bonus_conversion`'s six-step checklist (ADR
0031 §16a) is completed as part of that stage's own work. **Stage 4H-B1
is NOT authorized by this stage.** The Asset/Currency Registry + FX
architecture work has no next stage scheduled — it is recorded as
deferred, awaiting a future confirmed priority, not blocking anything
currently open.

---

## Stage 4H-B0-R4 — Asset/Currency Registry, FX/Conversion, and Dual-Mode Sportsbook Architecture Closure

Directive: "STAGE 4H-B0-R4 — FINAL ARCHITECTURE CLOSURE: ASSET/CURRENCY
+ FX + SPORTSBOOK DUAL-MODE FOUNDATION." Architecture-only, explicit
instructions: no Bonus/sportsbook production code, no migrations, no
real sportsbook/data-feed/FX provider invented or integrated. Objective:
close the Asset/Currency Registry requirement Stage 4H-B0-R3 deferred,
and formalize a new mandatory requirement — the platform must
architecturally support both external sportsbook-provider integration
and a strong in-house sportsbook engine, provider-neutral, co-equal
from day one.

### 1. Governance and starting point

Current HEAD at stage start: `f22a9e5` (Stage 4H-B0-R3's commit). The
directive explicitly listed R3's closed decisions (DS-1/DS-2/DS-3,
cashback, multi-asset genericity, Risk's post-rounded-only evaluation,
the `rounding_rules`/`rounding_rule_id` audit design) as **not to be
reopened unless an actual contradiction is discovered** — none was
found or reopened this stage.

### 2. Wave 1 — five specialists, parallel authorship

Each specialist owned a distinct file, so nothing overwrote another's
work:

- **`architect`** — new `docs/decisions/0037-asset-currency-registry-
  and-fx-conversion-architecture.md`. **Part A (Asset Registry)**:
  confirmed the `assets` schema (migrations 0003/0006) is already
  open/extensible — unconstrained `TEXT` primary key, per-row
  `decimal_exponent` (0-18) already looked up everywhere, no hardcoded
  decimal count anywhere in `internal/`. Designed 8 explicitly distinct,
  non-collapsible authorization/eligibility layers (existence →
  activation → platform authorization → tenant authorization → brand
  authorization → jurisdiction authorization → operation eligibility →
  market-rate availability) in a strict AND-chain evaluation order —
  presence in the registry never implies depositable/withdrawable/
  wagerable/settleable/convertible. **Part B (FX/Conversion)**: four
  structurally separate components (Registry / `FXRateProvider`
  interface, no vendor named / Conversion Service / the Ledger's
  existing `ConversionOperation`), deterministic provider selection (an
  ordered priority list, the actually-used source persisted, never the
  first attempted), a canonical Conversion record, and an 8-condition
  fail-closed rule (no valid rate / stale — default-to-stale-if-
  unconfigured / malformed / unauthorized asset / inactive asset /
  source unavailable / insufficient precision / a financial-constraint
  hook). **Part C (Authorization Boundary)**: resolved Stage 4H-B0-R3's
  flagged P1 with a two-tier split (platform-admin-only for layers 1-3;
  tenant-scoped, narrow-only for layers 4-7) and one canonical
  `AssetAuthorization.CheckEligibility` service every downstream domain
  (wallet, payments, sportsbook, casino, bonus, FX, retail) must
  consume rather than reimplementing.
- **`sportsbook`** — rewrote `docs/architecture/09-sportsbook-
  architecture.md` from a 48-line Stage-0 proposal (which recommended
  "start with widget/iframe, defer in-house") to ~1050 lines of dual-
  mode architecture. Evaluated the directive's full candidate domain-
  model list and refined it: split Market into MarketType (template) vs.
  Market (instance, polymorphic subject — Event or Season, for
  outright/futures markets); folded Line into an attribute of a
  versioned Price; demoted BetSlip to an ephemeral, non-persisted
  request; separated Result (sports-data fact) from Settlement
  (financial decision); and — the distinction most likely to prevent a
  future "second wallet" mistake — explicitly separated Exposure
  (trading-book read-model) from Liability (the one authoritative,
  ledger-backed number). Defined provider-neutral `SportsbookProvider`
  (mirroring the existing `CasinoProvider`/`PaymentProvider` interface
  shape) and `DataFeedProvider` abstractions; a five-layer in-house
  engine (Sports Data → Sportsbook Business Logic → Ledger → Risk →
  Trading Operations, explicitly non-collapsing); tenant/brand/
  jurisdiction/asset/market/health mode-selection routing. Confirmed
  compatibility with Bonus/Gamification (citing ADR 0033's existing
  `ExternalBonusProvider` coexistence model), RG (ADR 0034), Retail
  (ADR 0035/doc 26), multi-asset (ADR 0021, anticipating ADR 0037), and
  jurisdiction/licensing (doc 15) rather than redesigning any of them.
  Found and flagged (not unilaterally fixed) two narrow precision gaps
  in ADR 0033, written before the in-house mode was confirmed:
  `provider_id`'s nullability convention, and the two-bonus-source
  model's degenerate single-source case for in-house bets.
- **`ledger-finance`** — new `docs/decisions/0038-sportsbook-accounting-
  and-ledger-integration.md`, mirroring ADR 0032's structure. **No new
  account types needed** — sportsbook reuses `player_cash`/
  `player_bonus`/`player_locked`/`house_gaming`/`promo_liability`/
  `bonus_expense`/`manual_adjustment`, all already defined. Defined
  `potential_return` as a field on the sportsbook domain's `OpenBet`
  record, never a ledger posting until settlement (mirroring ADR 0032's
  "contingent liability, not recognized cost" pattern) — distinguishing
  this from the ledger-derivable stake liability, flagged as a
  reporting trap if conflated. Specified placement/rejection/win/loss/
  void/partial-settlement/cashout/reversal-after-correction as distinct
  posting shapes, with void/partial-settlement/cashout kept as three
  distinct transaction types even though two share a posting formula,
  because reporting needs to distinguish a market-triggered event from
  a player's voluntary priced decision. Confirmed aggregate open-bet
  liability is a derived SQL read over `player_locked` balances, never
  a maintained counter (mirroring ADR 0032's wagering-progress
  precedent). Specified market-correction reversal as a two-transaction
  compensating sequence (a rollback of the wrong settlement, plus a
  fresh re-settlement under its own idempotency key) — never a balance
  edit. Specified idempotency as `(tenant_id, provider_id,
  provider_tx_id)` with `provider_tx_id` required to be the specific
  event's own reference, never a bet-slip reference reused across a
  bet's multiple lifecycle events — flagged as the most likely
  real-world provider-adapter defect. Confirmed reuse of ADR 0021's
  resolved rounding decision verbatim; identified the only two sites
  that could ever need rounding at all (an in-house proportional
  cashout or bet-builder combo payout) since the ordinary provider-
  driven case posts already-rounded provider-supplied integers.
  Confirmed the pre-existing `player_locked` origin-split gap
  (`ledger-accounting-model.md` §6.2 / ADR 0032 §10) still blocks
  bonus-funded sportsbook wagering specifically — not re-decided, just
  restated as inherited.
- **`risk`** — added new §25-31 to `docs/decisions/0031-risk-and-
  limits-engine.md` (pure append; §1-24 untouched). Confirmed
  `sportsbook_bet` already exists as a real `Operation` (migration
  0041, `internal/risk/types.go`) and `risk_rules.product` already
  accepts `'sportsbook'` — zero schema change needed for placement.
  Proposed two new `Operation` values, `sportsbook_settlement` and
  `sportsbook_cashout`, reasoning through which lifecycle events
  genuinely need their own Risk-gated checkpoint (settlement/cashout)
  versus which don't (void, cancellation, partial settlement,
  re-settlement — ledger reversal/reuse only). Determined market/
  selection-level trading-exposure management (odds-setting, market
  suspension) is a Sportsbook Engine trading concern, not gated by
  `risk.Evaluate` — applying the same player-keyed-vs-cross-player-
  aggregate test already used for Bonus's campaign-budget-cap
  exemption and Retail's subtree-exposure exemption — while per-player
  stake/payout/cashout limits remain ordinary Risk rules. Flagged two
  open questions (platform-registry vs. tenant-authored for sport/
  market dimensions; whether Market needs a type+instance split) as
  owned by sportsbook's domain model, since it started before
  `sportsbook`'s final rewrite landed — resolved in a Wave-2 follow-up
  (§5 below).
- **`identity-compliance`** — added new §9-13 to `docs/decisions/0034-
  bonus-gamification-rg-kyc-identity-integration.md` (pure append;
  §1-8 untouched). Reasoned through each sportsbook lifecycle event:
  RG evaluated fresh at placement (the direct analogue of `postBet`'s
  pre-posting check); cashout requires a fresh RG check (reasoned as a
  discretionary, time-displaced, value-crediting action, the same shape
  as Bonus's marketplace redemption); settlement is RG-exempt (citing
  `postWin`'s own doc comment — no intervening player-contributed
  progress between acceptance and outcome, unlike a Bonus wagering
  requirement). Restated the standing bypass discipline (fresh
  evaluation, same transaction, before commit, no override parameter).
  Reasoned through the genuinely novel case sportsbook introduces — an
  OPEN bet can remain unsettled for days or months, so what happens if
  a player self-excludes while a bet is still open — and recommended
  settling normally (the closest extension of ADR 0034's existing
  "prospective, not retroactive" principle), but **explicitly flagged
  this as a genuine human/compliance decision requiring confirmation,
  not decided unilaterally**, per CLAUDE.md's "when to stop and ask"
  rule. Confirmed sportsbook introduces no new KYC concept and that RG
  enforcement must be identical regardless of integration mode (never
  delegated to or inferred from an external provider's own widget).

### 3. Wave 2 — four specialists, independent review (no self-review)

- **`architect`** reviewed the other four Wave-1 outputs (not its own
  ADR 0037). Cross-checked `risk`'s Operation proposals against
  `ledger-finance`'s ADR 0038 transaction types and found **one real,
  substantive contradiction** (§4 below). Resolved `risk`'s two open
  questions using `sportsbook`'s final domain model: sport/competition/
  event/market are platform-registry rows (not tenant-authored, no
  §20-style ownership-CHECK guard needed, following the same precedent
  as `Product`/`LicensingMode`), and Market needs the type+instance
  split `sportsbook`'s doc 09 already defines (mirroring
  `HierarchyNodeType`/`HierarchyNodeID`). Verified `sportsbook`'s
  flagged ADR 0033 gaps were correct and fixed them directly (narrowly
  authorized to do so): `provider_id` is nullable, `NULL` = in-house;
  the two-bonus-source model correctly degenerates to one source for
  in-house bets. Confirmed `identity-compliance`'s flagged "Acceptance"
  reconciliation needed no fix — `sportsbook`'s final doc 09 collapses
  placement and acceptance into one step, exactly the fallback ADR 0034
  §9 already anticipated. Found one additional terminology collision
  ("Cancellation" meaning an Event/Market-level Trading Operation in
  doc 09 versus a player-initiated bet withdrawal in ADR 0038) and
  routed it to `ledger-finance` rather than fixing it unilaterally.
  Independently re-verified extensibility items 10-18, all YES with
  citations.
- **`ledger-finance`** independently reviewed `architect`'s ADR 0037
  (explicitly not its own ADR 0038 this pass). Found: the Conversion
  record → `ConversionOperation` field-correspondence list omitted
  `rounding_rule_id` despite the ADR's own stated rationale requiring
  it to cross — a real gap that would have silently violated ADR
  0021's reconstructability invariant for every FX conversion if
  implemented as literally listed; no stated source for
  `ConversionOperation`'s required tenant/player/wallet/idempotency
  context fields; the same rate-plausibility gap `security`
  independently found (below — convergent finding, not duplicated
  effort); and a mischaracterized precedent — ADR 0037 claimed the
  platform-admin/tenant-scoped authorization split was "the exact
  precedent already established and implemented" for Risk & Limits,
  when ADR 0031 §8 itself documents, in its own words, that the
  positive half (a working platform-admin write path) remains an open,
  unbuilt gap there.
- **`security`** independently reviewed all five Wave-1 outputs.
  Confirmed the Asset Authorization two-tier split is fail-closed by
  construction (a strict AND-chain re-evaluates platform authorization
  independently on every call, so a tenant-side RBAC bug alone couldn't
  widen past it) but found two real, closable gaps: no stated
  fail-closed default for an absent configuration row, and no explicit
  "non-nil error = ineligible" contract for `CheckEligibility`. Found
  the FX fail-closed rule's 8 conditions catch structural failure but
  never validate a rate's economic plausibility against a compromised
  or malfunctioning provider — the same gap `ledger-finance`
  independently found. Confirmed sportsbook provider-callback
  authentication is already correctly specified in doc 09, citing the
  platform's existing hardened casino-callback precedent (HMAC
  signature verified before any payload field is inspected, tenant/
  provider resolved from the authenticated URL, never the payload) —
  verified this precedent is real by reading `internal/casino/mock.go`
  directly. Confirmed tenant isolation and audit coverage are
  explicit, not implicit, throughout. Flagged one non-blocking
  operational gap: an RG denial occurring after a widget/iframe
  provider already accepted a bet on its own side should trigger the
  adapter's own cancel call back to the provider, so the two systems'
  books don't diverge — named as a pre-implementation requirement, not
  fixed this stage (no provider exists yet).
- **`qa`** ran the full, required 20-item architectural extensibility
  test against all five documents. **All 20 items answered YES**, each
  independently verified against actual document content rather than
  the documents' own summary claims (e.g. item 10, "add a second
  sportsbook provider," was checked by reading the `SportsbookProvider`
  interface's field list against the canonical domain model and
  confirming no provider-specific field leaks between them, not by
  trusting doc 09's own claim that it doesn't). Designed (architecture-
  level only, no test code) an adversarial idempotency-collision test
  category proving/disproving whether two genuinely distinct same-type
  lifecycle events with coincidentally identical payloads could
  collide under ADR 0038's idempotency scheme — found this is a real,
  named residual gap requiring a per-occurrence distinguishing field
  before implementation, not resolved by the current design alone.
  Designed an authorization-widening defense-in-depth test-plan sketch
  mirroring the historical `risk_rules` dual-scope RLS incident this
  platform has already hit once.

### 4. The one real contradiction, found and fixed

`ledger-finance`'s ADR 0038 §13 originally proposed netting
`sportsbook_rollback` — in addition to `sportsbook_void` — against a
player's cumulative daily stake usage for `cumulative_amount` Risk
rules. `architect`'s independent review found this directly contradicts
the same ADR's own §10, which is explicit that a rollback corrects a
wrongly-recorded settlement outcome without nullifying the underlying
staked bet ("the bet itself remains a real, resolved wagering fact") —
unlike void, which returns the full stake as if the bet never happened.
Tracing a real sequence (stake S, wrongly settled as a win, rolled back,
re-settled as a loss) showed netting the rollback would erase the
player's genuinely-staked S from their daily tally; in the ADR's own
named composite case (rollback then void), netting both legs would
double-subtract, driving the tally negative. `risk`'s own parallel ADR
0031 §26 never proposed netting `sportsbook_rollback` — only
`sportsbook_void` — confirming this was an error introduced only in
`ledger-finance`'s own §13. `ledger-finance` corrected it directly in a
follow-up pass: `operationLedgerRollbackTypes["sportsbook_bet"]` nets
only `sportsbook_void`; the scenario the original text was evidently
trying to solve (the composite case) is already handled correctly by
the void leg alone, with no replacement mechanism needed.

### 5. Follow-up corrections applied

- **`ledger-finance`** (own document, ADR 0038): fixed the rollback-
  netting contradiction (§4); disambiguated "Cancellation" — renamed
  its own §8.4 concept to "player-initiated bet withdrawal" so it stops
  colliding with `sportsbook`'s already-settled use of the same word
  for an Event/Market-level Trading Operation; added the missing ADR
  0037 cross-reference; recorded a non-blocking future mapping item
  (once `cumulative_amount` is ever wired for settlement payouts,
  `operationLedgerTransactionTypes["sportsbook_settlement"]` will need
  to widen to include `sportsbook_partial_settlement`).
- **`risk`** (own document, ADR 0031): closed its two flagged open
  questions in §27/§30 using `sportsbook`'s final domain model,
  verified directly against doc 09 rather than taking `architect`'s
  review at its word — sport/competition/event/market are platform-
  registry rows, no tenant-ownership guard needed; Market needs the
  `MarketTypeCode`/`MarketID` type+instance split, Selection does not.
- **Orchestrator** (ADR 0037, five security/ledger-finance-flagged
  gaps, applied directly since each was a precise, narrow addition with
  an exact recommended sentence already supplied by the reviewers): the
  absent-configuration-row fail-closed default; the non-nil-error-is-
  always-ineligible contract; the rate-plausibility caveat on the FX
  fail-closed rule; `rounding_rule_id` added to the Conversion→
  `ConversionOperation` field-correspondence list, plus the tenant/
  wallet-context sourcing clarification; and the corrected
  characterization of ADR 0031 §8's precedent.

### 6. Final gate

**A-K closed, L: no P0.** Asset/Currency Registry (A), FX/Conversion
(B), and the Asset Authorization Boundary (C) are closed via ADR 0037.
External sportsbook (D), in-house sportsbook (E), and sports-data-feed
(F) architecture are closed via doc 09. Financial integration (G) is
closed via the corrected ADR 0038. Risk/RG integration (H) is closed
via ADR 0031 §25-31 and ADR 0034 §9-13, with one genuine open human/
compliance decision recorded (open-bet self-exclusion policy), not
resolved. Bonus/Gamification integration (I) is closed, confirming no
separate sportsbook-only bonus system and using ADR 0033's existing
coexistence mechanism (clarified for the in-house case). Retail
integration (J) is closed — a retail-originated bet is an ordinary Bet
through the same pipeline. Future extensibility (K) is demonstrated —
all 20 required test items answered YES with independent verification.

**Five P1s disclosed, none blocking this stage, none hidden or
downgraded**: (1) a rate-plausibility/deviation-bound check is a
required implementation-time control before any live FX provider
connects; (2) the Asset Authorization boundary's concrete RBAC
permission names and admin API surface remain to be designed at
implementation time; (3) ADR 0038's idempotency scheme needs a
per-occurrence distinguishing mechanism before implementation to fully
close the same-type/coincidental-payload collision risk `qa` named;
(4) the pre-existing `player_locked` origin-split decision (ADR 0032
§10) still blocks bonus-funded (not cash-funded) sportsbook wagering;
(5) the open-bet self-exclusion policy is a genuine human/compliance
decision, not yet made.

**Architecture is READY FOR IMPLEMENTATION for the Asset/Currency
Registry, FX/Conversion, and Sportsbook (external + in-house) domains.
None of them are authorized to begin.** This stage does not affect
Bonus Stage 4H-B1's own gate (still READY FOR HUMAN AUTHORIZATION after
`bonus_conversion`) or Retail's Stage 4H-B2 gate (still awaiting the
Retail-Legal/Business decisions).

### 7. Recommended implementation sequence (once authorized — not started)

Not sequenced or scheduled by this stage; recorded for whichever future
stage is authorized to begin. The natural order, given dependencies
disclosed above: (1) resolve the two remaining human/compliance
decisions (open-bet self-exclusion; confirm or contract a first real
sportsbook provider or commit to in-house-first); (2) implement the
Asset/Currency Registry's admin API + `AssetAuthorization` service
(closes the P1 in item 2 of §6 above) — this has no dependency on
sportsbook and could proceed independently; (3) implement the
`bonus_conversion` Risk dependency and the sportsbook-specific
`sportsbook_settlement`/`sportsbook_cashout` Risk `Operation` values
together, since both follow the identical six-step ADR 0031 §16
extension process; (4) implement ADR 0038's ledger integration
(migration order: `sportsbook_expense`-equivalent account-type
consideration if bonus-funded wagering is in scope, six new
transaction-type CHECK widenings, the idempotency-key uniqueness
constraint with the per-occurrence distinguishing field the qa-flagged
P1 requires); (5) implement the first sportsbook provider adapter
(external mode) or the in-house engine's core acceptance pipeline,
per whichever the human/business decision in item 1 selects; (6) FX/
Conversion Service implementation is independently sequenced whenever
a real FX provider relationship exists — it has no hard dependency on
sportsbook or bonus.

### 8. Final verification

`go build ./...` re-run after every commit this stage and remains
clean (docs-only diff throughout — two new ADRs, one substantially
rewritten architecture doc, two extended ADRs, one narrowly-clarified
ADR, no code, no migrations, no provider SDK, no sportsbook/data-feed/
FX provider integration, no Bonus implementation).

### Files changed this stage

`docs/decisions/0037-asset-currency-registry-and-fx-conversion-
architecture.md` (new), `docs/architecture/09-sportsbook-
architecture.md` (rewritten), `docs/decisions/0038-sportsbook-
accounting-and-ledger-integration.md` (new), `docs/decisions/0031-
risk-and-limits-engine.md` (§25-31 added), `docs/decisions/0034-bonus-
gamification-rg-kyc-identity-integration.md` (§9-13 added),
`docs/decisions/0033-provider-interoperability-and-external-bonus-
engines.md` (2 narrow clarifications), `docs/governance/
project-status.md`, `docs/governance/task-registry.md`,
`docs/active-stage.md`, `docs/progress.md` (this entry). No specialist
edited a file outside its own ownership without an explicit "Wave-2
review correction"/"follow-up correction" attribution.

### Next stage

Not started. Architecture is ready for implementation for the Asset/
Currency Registry, FX/Conversion, and Sportsbook domains, but **none
is authorized by this stage.** Bonus Stage 4H-B1 and Retail Stage
4H-B2's own gates are unaffected and remain exactly as Stage 4H-B0-R3
left them.

## Stage 4H-B0-R5 — Implementation Readiness and Final P1 Closure

Purpose: close the five P1s Stage 4H-B0-R4 disclosed (FX rate-
plausibility, Asset Authorization RBAC surface, idempotency per-
occurrence design, the `player_locked` origin-split, the
`OpenBetSelfExclusionPolicy`) and reach a genuine implementation-
readiness verdict — not a "documents were created" verdict. Documentation/
ADR-only stage: no code, no migrations, no provider integration, no real
vendor named, no implementation authorized.

### 1. Wave 1 — closure authorship (4 specialists, distinct file ownership)

- `architect` closed P1-1 (new ADR 0037 §B.7: category A universal vs.
  category B configurable rate-plausibility checks, a 9th fail-closed
  condition for cross-provider disagreement) and P1-2 (new ADR 0037 §C.5:
  9 canonical administrative operations, four-eyes reasoning,
  immutable/mutable field split, minimum-creation-field-set forcing
  inactive/unauthorized/zero-eligibility defaults). Also added doc 09
  §2.5 (canonical sportsbook identity clarification for the external
  path's Market/Selection).
- `ledger-finance` closed P1-3 (new ADR 0038 §14: `occurrence_ordinal`,
  strictly increasing per `(tenant_id, correlation_id, transaction_type)`,
  composed into the existing `provider_tx_id` string, no schema change)
  and proposed the P1-4 resolution as new `ledger-accounting-model.md`
  §6.3 — Shape A: split `player_locked` into
  `player_locked_cash`/`player_locked_bonus` via additive `account_type`
  CHECK widening only. Explicitly marked PROPOSAL ONLY / NOT IMPLEMENTED,
  requiring independent review before being treated as decided, per
  CLAUDE.md's rule against a specialist unilaterally redesigning the
  human-approved ledger schema (the same discipline as the Stage
  4H-B0-R1 agent-float amendment). Cross-referenced from new ADR 0038
  §15 (sportsbook-specific instantiation).
- `identity-compliance` closed P1-5 (new ADR 0034 §14:
  `OpenBetSelfExclusionPolicy`, exactly 2 values — `SETTLE_NORMALLY` /
  `VOID_ON_SELF_EXCLUSION` — jurisdiction-primary tighten-only scope, new
  audit trigger point). Explicitly did not select the platform-wide
  default value: left as an open human/legal decision, per the
  directive's own instruction.
- `sportsbook` added doc 09 §15 (external-first vs. in-house-first
  sequencing recommendation: external-first, 7/9 dimensions favor it — a
  business/engineering recommendation, explicitly not a permanent
  architectural constraint).

### 2. Wave 2 — independent review of the P1-4 `player_locked` proposal

`sportsbook`, `architect`, and `bonus-engine` each independently
reviewed Shape A against `ledger-finance`'s own posed review questions
(none reviewed its own authored work). All three **approved Shape A**
(the schema shape, the extended Invariant B1, and worked cases A-G) —
verified directly against the live schema in architect's case
(`migrations/0020_create_ledger_accounts.up.sql`), not against the
proposal's own prose. Each found a distinct completeness gap:

- `sportsbook` found the worked-cases table proved the mixed-funded split
  at *lock* time (case C) but never worked through the corresponding
  *unlock*-side cases (void/settlement/partial-settlement/cashout), and
  that the settlement-time recovery mechanism for a bet's original
  cash/bonus split was never stated anywhere despite being genuinely
  needed (sportsbook's lock and settlement are time-separated, unlike
  casino's atomic resolution).
- `architect` found a factually incorrect precedent claim — the proposal
  asserted ADR 0032 had "already used this CHECK-widening pattern
  successfully" for `bonus_expense`; verified `bonus_expense` was never
  migrated (zero hits in `migrations/`) and ADR 0032 itself is `NOT
  IMPLEMENTED`. Also found a real silent-defect call site:
  `internal/wallet/wallet.go`'s `GetSummary` switch would silently zero
  `LockedBalance` for split accounts once they exist, since the switch
  has no `default` arm and the ledger/projection would still reconcile
  perfectly (only the read model would lie).
- `bonus-engine` found Rule B2 (not just Invariant B1) needed an explicit
  boundary-crossing restatement over `{player_bonus, player_locked_bonus}`
  — the literal original Rule B2 text would both wrongly require a mirror
  on the lock entry and wrongly omit it on the stake-absorption/payout
  legs. While answering ledger-finance's posed question about forfeiture
  of a currently-locked bonus-funded stake, found a **real, structurally
  triggered gap**: once a Grant goes terminal (expired/cancelled/
  forfeited) while a portion remains locked, a later settlement or
  self-exclusion-triggered void credit against that Grant has no defined
  Grant-state-machine transition. Recognized this as the *same*
  unresolved question ADR 0034 §2 already left open (completing an
  already-satisfied wagering requirement post-self-exclusion), reached by
  a second, concrete trigger path — not a new question requiring a
  separate answer.

`ledger-finance` (follow-up, same wave) independently closed a real
database-level idempotency hole that `bonus-engine`'s review surfaced: the
pre-existing partial unique index `UNIQUE (tenant_id, provider_id,
provider_tx_id) WHERE provider_id IS NOT NULL` never evaluates for
in-house-mode postings (`provider_id` NULL per ADR 0033 §2), leaving
in-house sportsbook postings with **no database-level idempotency
enforcement at all** — a live violation of CLAUDE.md's idempotency rule.
New ADR 0038 §14.6: in-house-mode postings route through `UNIQUE
(tenant_id, idempotency_key)` (unconditional, no `WHERE` clause) instead;
`provider_id`/`provider_tx_id` stay `NULL`, never a reserved sentinel —
justified against two independent existing precedents (ADR 0033 §2's own
"never a reserved sentinel string" rule, and `internal/audit`'s
`ActorType`/`ActorID` discriminator-plus-empty-field pattern).

### 3. Wave 2b — targeted gap closure

Three dispatches, each routed to the specialist owning the affected
document (never self-fixed by the Orchestrator except where a reviewer
supplied exact pre-specified wording):

- `ledger-finance` closed all four Wave 2 gaps: withdrew the false
  precedent claim (both in `ledger-accounting-model.md` §6.3.1 and ADR
  0038 §15); added the `wallet.go GetSummary` fix to the new §6.3.4
  implementation checklist (with the correct `+=`, not `=`, since two
  account types now feed one field); added new §6.3.3.1 (settlement-time
  split-recovery mechanism — two distinct queries, original ratio vs.
  currently-remaining per-origin amount) and §6.3.3.2 (mixed-funded
  unlock-side worked cases: C-void, C-loss, C-win, C-partial, each with a
  full entry table and invariant checks); restated Rule B2 as a
  boundary-crossing rule with a worked numeric combined-transaction proof
  cross-checked against ADR 0032 §3's casino precedent. In doing so,
  produced new, explicitly-unreviewed content requiring its own sign-off
  gate: a proposed C-win proportional payout-split rule (full payout
  splits cash:bonus in the lock's original ratio, ADR 0021 rounding
  rules, residual to cash), and an explicit **OPEN QUESTION** for
  C-cashout (two candidates, both ledger-balanced and B1-safe — the
  deciding factor is bonus-abuse/consumer-protection policy, not ledger
  mechanics).
- `sportsbook` added doc 09 §6.1 (in-house-mode ledger-posting
  idempotency-routing statement: `provider_id`/`provider_tx_id` stay
  `NULL`, the adapter mints a stable intrinsic per-event reference as
  `idempotency_key` — per the requirement `ledger-finance` flagged in
  §14.6 but explicitly left for sportsbook to state in doc 09).
- `bonus-engine` added the terminal-Grant cross-reference note (doc10 §5,
  ADR 0032 §5) explicitly marked **Human decision required**, naming
  three options (re-forfeit / route to `player_cash` / manual-review
  queue) without selecting one, and tying both trigger paths (post-self-
  exclusion wagering completion; sportsbook locked-stake settlement/void
  post-terminal) to the same single open question.

### 4. Wave 3 — joint C-win/C-cashout decision input, and the directive's
required independent challenge of all five P1s

Six specialists, each independent of every Wave 1/2 author for at least
one of their assigned P1s, per the directive's requirement that at least
one reviewer be independent of every authoring specialist:

- `bonus-engine`: **APPROVE-WITH-CHANGES** on the C-win rule. Wagering-
  requirement semantics and the recognition-timing generalization are
  sound. Found a real, concrete bonus-abuse/structuring vector: a player
  can fund a stake with an overwhelmingly cash-dominant mix and a
  deliberately tiny bonus sliver so `round_half_up(payout × B/(C+B))`
  rounds to zero for every plausible payout, converting the bonus sliver
  to fully withdrawable cash on every win with no mirror, no write-off,
  and no detection — repeated at scale, a one-directional value leak.
  Requires a bonus-engine-owned anti-structuring control before
  implementation. C-cashout input: reject all-to-cash outright (a direct,
  effort-free wagering-requirement bypass); prefer "not cashout-eligible"
  first, proportional-with-the-same-control as fallback. Confirmed the
  terminal-Grant forfeiture gap generalizes (asset deactivation mid-
  campaign hits the identical root cause via a different trigger). Found
  `VOID_ON_SELF_EXCLUSION` doesn't net the lock-time wagering-progress
  debit against its own reversal, giving full wagering credit for a stake
  that was never actually risked. P1-1: no gap (bonus conversion is
  always same-asset, never FX-routed).
- `sportsbook`: found the C-win split-recovery computation must live
  inside `internal/ledger`'s own settlement handler, not
  `internal/sportsbook` — sportsbook has no live, non-drifting source for
  a bet's original cash/bonus ratio at settlement time, which is exactly
  why the recovery-query mechanism exists. Clarified this doesn't violate
  ADR 0038 §5's "ledger never recomputes" rule, since that rule is scoped
  to the provider's own odds/settlement math, not the platform's internal
  wallet-attribution of an already-trusted total. C-cashout input:
  funding mix isn't known to (or needed by) the cashout-pricing layer;
  "not cashout-eligible" is mechanically simple but, read literally,
  disqualifies a mostly-cash mixed bet from cashout entirely — a real
  product/UX cost, so recommends scoping it to fully-bonus-funded bets if
  chosen. Found P1-2 has no casino-vs-sportsbook product dimension, in
  tension with the platform's own per-product-licensable model. Found
  P1-5's `VOID_ON_SELF_EXCLUSION` is unspecified for a multi-leg bet
  caught mid-partial-settlement (a real gap in ADR 0038 §8.1's void
  table, not the two-value split itself).
- `product-owner-proxy`: recommended "not cashout-eligible" as the right
  MVP-scope answer for C-cashout — sportsbook isn't even in current B2C
  MVP scope (doc 14, deferred to P3), nothing in the Blueprint requires
  bonus-funded cashout, and all three candidates are already proven
  ledger-safe so the choice doesn't foreclose the future path; the
  simpler option is trivially reversible later, the harder one is not.
  Ran a full scope-check across this stage's entire output (ADR 0037
  §B.7/§C.5, ADR 0038 §14.6/§15, `ledger-accounting-model.md` §6.3, ADR
  0034 §14) and found **no scope creep**: every section maps 1:1 onto one
  of the five named P1s, and the stage repeatedly shows explicit
  restraint (disclaiming authority to decide C-win, refusing to resolve
  C-cashout by extrapolation, ADR 0034 §14 rejecting a third policy enum
  value in favor of composing existing mechanisms).
- `security` (independent, first look at all five P1s, verified against
  live schema/code rather than document prose): found **9 P1-level
  gaps** — (P1-1) the FX control-plane's own bounds (deviation/
  staleness/spread thresholds) have no RBAC tier, no dual control, and no
  mandatory audit requirement, so a tenant-scoped actor can legally widen
  them until the fail-closed checks never fire; the single-provider
  plausibility check is circular (its baseline is supplied by the same
  provider it's checking); (P1-2) layers 1-3 (platform-admin-only) have
  no RLS backstop despite an "structurally cannot reach" claim, because
  the `assets` table isn't tenant-scoped (verified against
  `migrations/0003`); `assets.active` defaults to `true` in the live
  schema, directly contradicting the fail-closed design; four-eyes for
  create/activate/platform-authorize is asserted with no enforcement
  mechanism (no equivalent of the `withdrawal_approvals` precedent's
  distinct-approver constraint, immutability trigger, and self-approval
  guard); `CheckEligibility`'s tenant/jurisdiction inputs aren't required
  to be server-sourced, and no per-player jurisdiction resolver exists
  anywhere in the codebase yet; (P1-3) the `occurrence_ordinal` fallback
  path for a provider with no signed per-occurrence field derives
  distinguishability from an unauthenticated transport-level delivery
  observation rather than signed payload data — a real double-post
  vector via replayed-as-new-delivery attacks — and the composed-key
  string concatenation has no delimiter/escaping discipline (a crafted
  provider reference can collide two genuinely distinct events);
  (P1-4) no tenant-isolation defect found (verified RLS/FORCE RLS
  directly against `migrations/0020/0021/0022`), but a new concurrency
  gap in the settlement-time recovery query (no same-transaction locking,
  no fail-closed handling for a missing origin row) and a low-exponent
  amplification of bonus-engine's C-win structuring vector; (P1-5) the
  "resolved fresh, never cached" resolution mechanism has no as-of
  timestamp anchor, opening a tampering window between the self-exclusion
  instant and listener execution, and per-bet audit records cannot prove
  enumeration completeness (a dropped event is indistinguishable from "no
  open bets" under the current design).
- `qa` (independent, first look): found P1-1/P1-3/P1-4 testable-as-
  specified (P1-4's own §6.3.4 already names a full test list). Found
  P1-2's layers 4 and 6 (tenant/jurisdiction) cannot actually be tested
  as independently distinguishable despite §C.2's claim of per-layer
  distinct `ReasonCode`s, because ADR 0037 Part A.5 itself states they
  resolve from one `(tenant_id, jurisdiction_id)` row. Found P1-5's
  jurisdiction-floor enforcement point (config-write-time vs. resolution-
  time — these require different tests, and only one can be correct) is
  unspecified, and its new self-exclusion-commit listener has zero named
  test cases despite being explicitly new system behavior.
- `risk` (independent, first look, verified against actual code —
  `internal/risk/evaluator.go`, `internal/risk/types.go`,
  `internal/casino/orchestrator.go`, live migrations — not document
  claims): found **no gap** in P1-1 (correctly out of Risk's scope, same
  test ADR 0031 §28 already applies to market/trading exposure), P1-4 (no
  Risk read anywhere touches `player_locked`/`account_type`; the split
  introduces no regression), or P1-5 (stays correctly in RG's domain,
  needs no Risk notification on void). Found a real interaction gap in
  P1-2: asset-agnostic `risk_rules` thresholds have no decimal-exponent
  awareness, so authorizing a new asset with a different exponent for a
  tenant can silently turn an existing wildcard amount cap into an
  effectively-unlimited or an always-denying rule. While investigating
  P1-3 (confirmed no gap in the literal question — Risk maintains no
  exposure counter, only derived ledger reads), **discovered a
  pre-existing, previously-undocumented latent fail-open already live in
  `internal/risk`'s own code**: `Rule.breach()`'s cumulative-usage query
  never joins `ledger_accounts`, so it is blind to `account_type`. This
  works by accident for `casino_bet` only because its counterparty leg
  (`house_gaming`) is wallet-less; a `sportsbook_bet` cumulative-amount
  rule would always compute zero usage, since both its legs
  (`player_cash`/`player_locked`) are player-owned and net to zero. Not
  exploitable today (only `casino_bet` is mapped in
  `operationLedgerTransactionTypes`), but latent and must be fixed before
  that map ever widens to include sportsbook. Also flagged a real cross-
  document conflict: ADR 0031 §26/§31 call `sportsbook_settlement`/
  `sportsbook_cashout` "near-term, load-bearing" Risk Operations; ADR
  0038 §13 states they are "not additional Risk checkpoints" — escalated
  for Orchestrator assignment in a future stage, not resolved here.

### 5. Synthesis and overall P1 closure verdict

All five Stage 4H-B0-R4 P1s are **architecturally resolved** — every
specialist who reviewed each P1's core design approved its shape. **None
is fully implementation-ready.** Wave 3's own review process (exactly as
intended — a genuine independent challenge, not a rubber stamp) surfaced
roughly 20 concrete, specific residual findings across the five P1s, the
large majority self-described by their finders as "not blocking
architecture status, blocking implementation." Per this stage's own
directive not to hide or downgrade a P1, and CLAUDE.md's "no fake
completion" rule, this stage does not claim full closure: every finding
is attributed to its discovering specialist, routed to its owning
specialist, and catalogued in `docs/governance/project-status.md`'s
Blocked-stages section (Sportsbook and Asset/Currency Registry + FX/
Conversion entries) and ADR 0031 §32 / `docs/security/security-
architecture.md` / `docs/testing/testing-strategy.md`'s new Stage
4H-B0-R5 sections. This stage deliberately did not open a further wave to
fix these findings: the large majority require code or migration changes,
explicitly out of scope for a documentation-only stage per this stage's
own directive ("prefer documentation/ADR-only changes... do not create
migrations unless absolutely necessary").

### 6. Implementation dependency contract (summary — see completion
report for full A-F breakdown)

- Must fix before any Bonus (Stage 4H-B1) implementation touching
  sportsbook-funded wagering: the C-win anti-structuring control; the
  terminal-Grant settlement-credit human decision; the
  `VOID_ON_SELF_EXCLUSION` wagering-progress-netting gap.
- Must fix before any sportsbook financial integration: the
  `wallet.go GetSummary` call site; the mixed-funded unlock-side cases
  being formally reviewed (currently self-consistent but unreviewed by a
  second specialist beyond their author); the settlement-time recovery
  query's same-transaction locking; the `occurrence_ordinal`
  tamper-resistance fix (security S-5/S-6); the `internal/risk`
  `Rule.breach()` account_type-blindness fix, before any sportsbook
  cumulative-amount Risk rule is configured.
- Must fix before any live FX provider is connected: the FX
  control-plane RBAC/dual-control/audit gap (security S-1); the
  single-provider circularity gap (security S-2).
- Can be later (genuinely deferred, not currently blocking): C-cashout's
  final policy choice (sportsbook not in B2C MVP scope at all yet); the
  Asset Authorization product/vertical dimension (P1-2, sportsbook
  finding) — real but not urgent while sportsbook is unbuilt.
- Human decisions required (unchanged in kind from prior stages, some new
  this stage): `OpenBetSelfExclusionPolicy` default value; the
  terminal-Grant settlement-credit resolution rule; C-cashout's final
  policy (though all three specialists who gave input converged on the
  same recommendation).
- Vendor-docs required: unchanged — no real sportsbook or FX provider is
  contracted.

### 7. Final verification

`go build ./...` and `go vet` re-run after every commit this stage and
remained clean throughout (documentation-only diff: two ADRs extended
with major new sections, one ADR extended with a new §32, one
architecture doc extended with a large new §6.3, two smaller architecture
docs extended, `docs/security/security-architecture.md` and
`docs/testing/testing-strategy.md` each given a new Stage 4H-B0-R5
section — no code, no migrations, no provider SDK).

### Files changed this stage

`docs/decisions/0037-asset-currency-registry-and-fx-conversion-
architecture.md` (§B.7, §C.5 new; 2 further gap-fix notes from Wave 3),
`docs/decisions/0038-sportsbook-accounting-and-ledger-integration.md`
(§14, §14.6, §15 new; further Wave 3 notes), `docs/architecture/
ledger-accounting-model.md` (§6.3 new, extended across Waves 2/2b/3),
`docs/decisions/0034-bonus-gamification-rg-kyc-identity-integration.md`
(§14 new; further Wave 3 notes), `docs/architecture/09-sportsbook-
architecture.md` (§2.5, §6.1, §15 new), `docs/architecture/10-bonus-
engine-architecture.md` (§5 cross-reference, Grant lifecycle note),
`docs/architecture/financial-transaction-flows.md` (§13 gap note),
`docs/decisions/0032-bonus-accounting.md` (§5, §8 cross-reference notes),
`docs/decisions/0031-risk-and-limits-engine.md` (§32 new),
`docs/security/security-architecture.md` (new Stage 4H-B0-R5 section),
`docs/testing/testing-strategy.md` (new Stage 4H-B0-R5 section),
`docs/governance/project-status.md`, `docs/governance/task-registry.md`,
`docs/active-stage.md`, `docs/progress.md` (this entry). No specialist
edited a file outside its own ownership without an explicit attribution
(e.g. "Wave 2 review correction," "Wave 3 finding").

### Next stage

Not started. All five Stage 4H-B0-R4 P1s are architecturally resolved but
not fully implementation-ready. **No implementation stage is authorized
by this stage.** Stage 4H-B1 (Bonus Engine) and Stage 4H-B2 (Retail
Architecture Hardening) remain exactly as prior stages left them, unaffected
by this stage's work.

## Stage 4H-B0-R6 — Foundational Implementation Hardening

The first implementation stage since a long architecture-only period
(Stages 4H-B0-R3/R4/R5). Purpose: implement and independently verify the
foundational contracts that must exist before Bonus B1 and sportsbook
financial implementation can safely begin. Six authorized workstreams —
Asset Registry + Authorization (A), financial idempotency hardening (B),
`player_locked` accounting (C), Risk fail-closed hardening (D), RG
self-exclusion technical hardening (E), Bonus dependency contract
closure (F). Explicitly NOT authorized: Bonus Engine, Gamification,
Reward Orchestrator, any real sportsbook/data-feed/FX/KYC/PSP/custody
provider integration.

### 1. Governance setup

Before any implementation began: task registry rows created for all six
workstreams with named owners, and exclusive migration-number ranges
reserved per workstream (0043 for E, 0044-0045 for A, 0046 for D, 0047
reserved-but-unused for B, 0048 reserved for C phase 2) to prevent
collision across parallel dispatches — a real near-miss occurred anyway
(architect's and identity-compliance's fix-wave dispatches both
independently picked 0047; identity-compliance's own pre-write check
caught it and self-resolved to 0049).

### 2. Wave 1 — implementation

Six specialists, each with distinct file ownership and explicit
boundaries on what not to touch:

- **`architect` (Workstream A)** built `internal/assetregistry` and
  migrations 0044-0045: fixed `assets.active`'s fail-open default (now
  `false`; the 7 seeded assets explicitly grandfathered active but
  `platform_authorized=false`); added a real DB-level backstop for the
  platform-admin-only layers (RLS+FORCE+trigger, since no second
  Postgres role exists to do this properly — disclosed, not hidden);
  immutable identity-field enforcement (`code`/`decimal_exponent`/
  `asset_type`/`network`); a `(product, operation)` dimension on layers
  4-7 (closing sportsbook's own Stage 4H-B0-R5 finding that casino and
  sportsbook eligibility for the same asset couldn't be distinguished);
  and four-eyes (`asset_change_requests`/`asset_change_approvals`)
  mirroring the `withdrawal_approvals` precedent. 32+ adversarial
  integration tests (cross-tenant, cross-brand, self-approval,
  activation-bypass, inactive-asset-use, identity/exponent/network
  mutation, product/operation bypass), all against real PostgreSQL 16.
- **`identity-compliance` (Workstream E)** built migration 0043 and
  `internal/rg/self_exclusion_*.go`: a jurisdiction-primary, tenant/
  brand-tighten-only policy config schema enforced at BOTH write time
  (a DB trigger rejecting a raw-SQL loosening insert) and read time
  (`max(strictness)` aggregation, a genuine backstop verified by
  disabling the trigger and confirming resolution still surfaces the
  strict value); as-of temporal resolution (a later config change proven
  not to retroactively alter an earlier resolution); authoritative
  `clock_timestamp()` throughout (never app wall-clock or a stale
  transaction-start snapshot); a stalled-enumeration-run detection
  primitive using a previously-unread partial index. Explicitly did NOT
  select the platform-wide default policy value — where none is
  configured, resolution returns `Configured: false` and every caller
  must treat that as fail-closed.
- **`risk` (Workstream D)** fixed the ADR 0031 §32 latent fail-open
  found in Stage 4H-B0-R5 (`internal/risk/cumulative.go`'s leg-aware,
  self-defending cumulative-usage model — an undeclared player-side
  account_type now fails closed rather than silently under-counting);
  closed seven accidental-ALLOW paths in `evaluator.go` (unknown
  operation, missing player/jurisdiction/asset/scope context, an
  un-tenant-scoped or player-scoped connection, an unrecognized
  Outcome); added decimal-exponent awareness to `risk_rules`
  (`internal/risk/denomination.go`, migration 0046, validated at
  exponents 0/2/6/8/18 including two synthetic assets created through
  the real dual-control path); and resolved the ADR 0031/0038
  Risk-checkpoint conflict on Risk's own side (new ADR 0031 §36, with
  visible superseded-in-part markers on §26/§31, not a silent rewrite).
- **`ledger-finance` (Workstream C, phase 1 only)** produced the
  implementation-ready 12-case (A-L) accounting-flow reference
  (`ledger-accounting-model.md` §6.4) — explicitly NOT blind-implemented,
  per the stage's own instruction. Deferred mixed cash+bonus funding for
  this pass as a hard, fail-closed rejection at placement (HR-2) rather
  than a silent cash-only fallback, reasoning that the required
  anti-structuring control is bonus-engine's to design (per
  bonus-engine's own prior ownership statement from Stage 4H-B0-R5), not
  ledger-finance's to invent unilaterally. Cashout stays NOT IMPLEMENTED
  regardless (no sportsbook code exists to offer one). Found and queued
  five real cross-document defects for a later fix (an understated
  mirror count on the bonus-win case, a silently-zeroing aggregate
  open-liability query, an insufficiently-scoped double-release lock, a
  reversal mirror double-generation hazard, and pervasive bare
  `player_locked` references) without touching the other specialists'
  documents. No migration or code written this phase.
- **`integrations` (Workstream B)** built new `internal/idempotency`,
  closing the two vulnerabilities security found in Stage 4H-B0-R5: the
  fallback occurrence discriminator now must derive from an authenticated
  payload field, never a transport-level delivery observation
  (`ResolveOccurrence`/`OccurrenceSource`); the composed key uses a
  length-prefixed encoding proven collision-free over 262,119 adversarial
  `(reference, discriminator)` pairs. Explicitly disclosed as having zero
  production call sites — a shared primitive, not yet adopted by any
  domain (`internal/casino` still uses its own inline composition).
- **`bonus-engine` (Workstream F)** added a "Bonus Dependency Contract
  Freeze" section to `docs/architecture/10-bonus-engine-architecture.md`:
  the exact frozen contracts Bonus will depend on (Asset Registry,
  AssetAuthorization, Risk, RG, Wallet/Ledger, Activity/Event taxonomy,
  rounding_rules, conversion boundary, `player_locked` origin, external
  provider-native bonus coexistence), each cited against its source ADR,
  plus an explicit "Bonus must never build" list (no parallel wallet/
  ledger/risk engine/RG engine/asset registry). Named 8 genuine gaps
  rather than inventing answers. Documentation only.

### 3. Wave 2 — independent review

Six specialists, none reviewing their own Wave 1 work:

- `sportsbook` and `bonus-engine` each validated Workstream C's
  implementation ADR against ledger-finance's own posed questions.
  `bonus-engine` confirmed a **new, real P1** while answering one of
  them: bonus-funded sportsbook wagering progress (a derived read over
  lock-time debits to `player_bonus`) is never netted against a later
  void or rollback of the same lock, so progress stays counted for a bet
  that was never actually risked — a player-reachable farming vector
  that generalizes far beyond the Stage 4H-B0-R5 self-exclusion-specific
  finding it was traced from. Specified the precise fix (net a lock
  debit only against a same-`correlation_id` void, or a rollback whose
  `reverses_transaction_id` points at the lock transaction itself —
  never a rollback of a settlement) but did not build it — this is gate
  **G-3**. Also confirmed the pre-existing terminal-Grant gate (**G-2**)
  blocks bonus-only funding directly, not only the deferred
  mixed-funding case, widening what phase 1 leaves blocked.
- `product-owner-proxy` and `sportsbook` ran a sportsbook-readiness/
  scope-check pass: all three foundational primitives (idempotency,
  asset authorization, risk) confirmed sufficient for a future
  sportsbook slice in both provider modes, verified against actual
  integration tests rather than document prose; no scope creep found in
  the stage's output.
- `architect` (independent of its own Workstream A) ran a
  cross-workstream consistency pass: confirmed a single exponent source
  of truth, and — more importantly — confirmed the seam between Risk's
  direct `assets` read and `internal/assetregistry`'s `CheckEligibility`
  is load-bearing, not just tidy: routing Risk's exponent lookup through
  `CheckEligibility` would have denied every live casino bet today,
  since all seven seeded assets are `platform_authorized=false`. Also
  confirmed the RLS/backstop idiom is consistent between Workstreams A
  and E, and found no idempotency-routing conflict. Filed two real
  documentation cross-references via the Dependency Request Log (a stale
  `CheckEligibility` signature citation in Workstream F's contract
  freeze; confirmation that Workstream C's §6.4 neither resolves nor
  contradicts the ADR 0031/0038 conflict) rather than editing another
  specialist's document mid-review.
- `qa` independently re-ran the full suite and read actual test bodies
  (not just names) across Workstreams A/B/D/E. Confirmed the large
  majority genuine, but found three specific, real gaps: Workstream B's
  `TestIntegration_ReplayWithChangedAmountAssetOrPlayerIsWhyTheContractMatters`
  never actually varied the asset dimension despite its name; Workstream
  D had no TOCTOU race test for the cumulative-limit advisory lock (only
  independent per-transaction hard-cap tests); Workstream E had no
  cross-tenant RLS isolation test for either of its two new tables
  despite both carrying fresh RLS policies.
- `code-reviewer` found two real High-severity defects: **F1** the
  platform-wide layer-7 operation-eligibility grant (ADR 0037 §C.5.1 op
  9) had no four-eyes representation at all — no request type existed
  for it — despite the ADR explicitly requiring dual control; **F2**
  `self_exclusion_enumeration_runs`' RLS policy was missing the
  `app.player_account_id IS NULL` conjunct every sibling table in the
  same migration correctly carried, a real cross-player compliance-data
  exposure path. Plus **F3** (a silent fail-open on a wrongly-scoped
  reconciliation call — the identical shape Workstream D explicitly
  fixed elsewhere this same stage) and six lower-severity findings
  (dead code in `internal/idempotency`, misleading doc comments, an
  error-classification bug).
- `security` independently reproduced a **live, exploitable P1**: the
  four-eyes self-approval person-identity check was unconditionally
  inert, because **no code path anywhere in the platform** could ever
  set `person_id` on a `platform_admin` account (`seed-admin` always
  creates one unlinked; the HTTP admin-routes role allowlist excludes
  `platform_admin`; the existing person-link route is tenant-scoped and
  structurally can't reach platform accounts). Reproduced end-to-end
  against the live dev database: one human, two `seed-admin` runs,
  completed create → self-authorize → activate alone. Also independently
  found code-reviewer's F1/F2, a DELETE-based RLS-widening exploit on
  `asset_authorizations` (deleting a brand-level denial silently
  promoted that brand to eligible), and confirmed Workstream D clean —
  **IMPLEMENTED**, no findings.

### 4. Fix wave

Four dispatches, each to the specialist owning the affected code:

- **`architect`** hardened the four-eyes trigger (migration 0047) to
  require a resolved, non-NULL `person_id` and active status on both
  requester and approver, mirroring the withdrawal-governance precedent
  (migration 0034) instead of an earlier, since-withdrawn version this
  workstream had originally mirrored; added a fourth dual-controlled
  operation for the layer-7 eligibility grant, discovering along the way
  that the suggested `BEFORE` trigger placement double-fires on `ON
  CONFLICT DO UPDATE` (caught by a failing test, not by inspection) and
  correctly moving the consume logic to an `AFTER` trigger instead; split
  `asset_authorizations`' RLS into per-command policies with no DELETE
  policy, closing the widening exploit; added TRUNCATE deny-triggers.
  Verified fail-before/pass-after against a literal reproduction of
  security's exploit, at both the DB and HTTP layers.
- **`identity-compliance`** built the person-linking path
  architect's fix depends on (new `cmd/seed-admin -person-id`/
  `-create-person` flags; a new platform-scoped remediation route,
  explicitly checked against a `tenant_admin` caller who also holds the
  gating permission but must still be refused) — and proved the
  dependency is satisfied, not assumed, with an end-to-end test taking
  an account in `seed-admin`'s still-unchanged default state through
  refusal, remediation, and a successful dual-controlled create. Also
  closed the RLS conjunct gap (F2), added connection-scope verification
  and stalled-run detection to the reconciliation query (F3), closed a
  jurisdiction-floor backdating exploit (catching and fixing a real bug
  of its own along the way — a naive `effective_from < clock_timestamp()`
  check failed every legitimate write due to a real, deterministic clock
  read/evaluate gap, fixed with a parity-tested 5-second tolerance), and
  aligned an RLS-policy gap. Self-resolved a migration-number collision
  with architect's parallel dispatch by using 0049 after detecting 0047
  was already claimed in the shared working tree.
- **`integrations`** trimmed genuinely dead code from
  `internal/idempotency` per CLAUDE.md's no-uncontrolled-scope-expansion
  rule (four zero-value type aliases providing no compile-time
  distinction, an unimplemented interface, several functions with zero
  non-test callers — confirmed by repo-wide grep both before and after),
  and closed the changed-asset test gap with a genuine third wallet/
  asset case.
- **`risk`** consolidated the exponent lookup through
  `internal/assetregistry.GetAsset` (closing the consolidation its own
  prior code comment had promised but not delivered, since Workstream A
  landed after Workstream D's initial commit), and added a genuinely
  mutation-tested TOCTOU race test: 8 concurrent bet-placement sequences,
  each individually under a cumulative cap but collectively 2.4x over
  it, proving the advisory lock prevents overshoot — verified by
  temporarily removing the lock statement, confirming the new test
  failed 10/10 runs with a real overshoot, then restoring `evaluator.go`
  byte-identical (confirmed via empty `git diff`).

### 5. Final independent re-verification

- `security` re-ran the original P1 exploit against the fixed code and
  **could not reconstruct it by any route tried** — confirmed closed.
  Independently confirmed the person-linking path, the RLS DELETE fix,
  the RLS conjunct fix, the TRUNCATE-trigger soundness (no interference
  with the deliberately-preserved tenant-deletion CASCADE path), and the
  backdating fix (including checking the one gap not yet probed — the
  trigger is `BEFORE INSERT` only, confirmed not exploitable via UPDATE
  because `effective_from` is separately immutable). Concurrency-probed
  the new `AFTER`-trigger consume logic directly (3 concurrent grant
  attempts against 1 approval → exactly 1 grant) and found no new race.
  Found five new minor items while probing; only one is non-trivial —
  the stalled-run detection primitive has zero callers in a running
  system (the one live reconciliation scheduler only runs the
  ledger-vs-projection sweep) — labeled `PARTIALLY IMPLEMENTED`, not
  launch-blocking, per CLAUDE.md's no-fake-completion rule. Verdict:
  Workstream A's four-eyes control and RLS backstop can now be labeled
  `IMPLEMENTED`.
- `qa` independently re-verified all three originally-flagged test gaps
  are genuinely closed (real, non-tautological assertions, not
  relabeled or weakened tests) and confirmed the two new four-eyes
  bypass regression tests plausibly reproduce the described exploit.
  Full integration suite: 588 `PASS`, 0 `SKIP`, 0 `FAIL`. Independently
  ran a full 48-migration round-trip clean on a fresh database, and
  reproduced the one known limitation (migration 0039's down-migration
  fails against pre-existing `identity_review_required` data) to confirm
  it is real, pre-existing from Stage 4E (three days before this stage,
  unrelated to this stage's work), and does not affect a genuinely fresh
  database.

### 6. Final labels (CLAUDE.md's no-fake-completion rule)

Workstream A (Asset Registry) — **IMPLEMENTED**, layer 8 (market-rate
availability) NOT IMPLEMENTED (concluded to be substantially a runtime
FX-provider check, not a stored fact — no schema built for it).
Workstream B (idempotency) — **IMPLEMENTED** as a shared primitive, zero
production call sites. Workstream C (`player_locked`) — phase 1 (ADR)
DONE; phase 2 (migration 0048 + code) NOT STARTED, gated on G-2 (human
decision, unmade) and G-3 (fix design specified, not built) for
bonus-funded cases; cash-only cases and the schema widening itself have
no remaining objection. Workstream D (Risk) — **IMPLEMENTED**, security
sign-off granted, zero findings. Workstream E (RG self-exclusion) —
**PARTIALLY IMPLEMENTED** (the scheduler-wiring gap; the platform-wide
default policy value remains an unmade human/legal decision, unchanged
since Stage 4H-B0-R4). Workstream F (Bonus dependency contract) —
**DONE**.

### 7. Final verification

`gofmt -l .`, `go build ./...`, `go vet ./...` (and `-tags=integration`
variants) all clean at the final commit. `go test ./...` — all packages
pass. Full integration suite against real PostgreSQL 16 — 588 tests,
zero skips, zero failures, confirmed independently by `qa`'s final pass.
Migration round-trip (49 migrations) verified clean on a fresh database
by multiple independent specialists.

### Files changed this stage

New packages: `internal/assetregistry` (11 files), `internal/idempotency`
(9 files). New migrations: `0043`, `0044`, `0045`, `0046`, `0047`,
`0049` (0048 remains reserved and unused, for Workstream C phase 2).
Extended: `internal/risk/{evaluator,types,policy_service}.go` +
`cumulative.go`/`denomination.go` (new), `internal/rg/self_exclusion_*.go`
(new), `internal/casino/{orchestrator,types}.go`, `internal/httpserver/
{admin_routes,routes,risk_handlers,asset_registry_*}.go`,
`cmd/seed-admin/main.go`, `docs/decisions/{0031,0037}-*.md`,
`docs/architecture/{09,10}-*.md`, `docs/architecture/
ledger-accounting-model.md` (§6.4 new), `docs/api/openapi/
platform-api.yaml`, `docs/governance/{ownership,task-registry,
project-status}.md`, `docs/active-stage.md`, `docs/progress.md` (this
entry). No specialist edited a file outside its own ownership without
an explicit attribution, and the one migration-number near-collision was
caught and self-resolved without any data loss or wasted work.

### Next stage

Not started. **No implementation stage is authorized by this stage.**
Stage 4H-B1 (Bonus Engine) remains READY FOR HUMAN AUTHORIZATION after
the `bonus_conversion` Risk dependency (still NOT STARTED), unaffected
by this stage. A future stage should close: gate G-2 (the terminal-Grant
human decision), gate G-3 (the wagering-progress-netting fix design,
already specified), the stalled-enumeration-run scheduler wiring, and —
separately, whenever FX/Conversion implementation is authorized — the
still-untouched Part B security findings from Stage 4H-B0-R5.

## Stage 4H-B0-R7 — Final Financial/Bonus Implementation Gate — complete (approved-pending)

Closed the implementation-blocking financial dependencies Stage 4H-B0-R6
discovered: `player_locked` phase 2, gate G-3 (bonus-funded wagering-
progress farming after a later void/rollback), the Terminal-Grant and
self-exclusion technical contracts, and a formal Human Decision Register.
No Bonus Engine/Gamification/Reward Orchestrator/real-provider code
authorized or written this stage.

**Workstream A — `player_locked` phase 2: IMPLEMENTED.** `ledger-finance`
split the ledger account type `player_locked` into
`player_locked_cash`/`player_locked_bonus` (migration `0048`, invariant
L1 — locked-origin determinacy, enforced across a 5-layer stack), added
the HR-9 fail-closed posting guard rejecting any posting against
`player_bonus`/`player_locked_bonus` until `bonus_expense` and the Rule
B2 mirror generator both exist, and extended `wallet.GetSummary` with
per-origin balances and an erroring default arm.

A real defect was found and fixed **during** implementation: the
migration's designed pre-flight guard (`SELECT count(*)`) was silently
inert under `ledger_accounts`' `FORCE ROW LEVEL SECURITY`. The first fix
(toggling `NO FORCE`/`FORCE ROW LEVEL SECURITY` around the count) was
itself found blocking by independent `security` review — the restore is
transaction-local, so a standalone migration run could leave tenant
isolation silently, permanently off. Resolved by removing the RLS toggle
entirely, relying solely on the RLS-immune `ADD CONSTRAINT ... EXCEPTION
WHEN check_violation` mechanism; `security` independently re-confirmed
the fix in a dedicated follow-up pass.

Independently reviewed by `security`, `code-reviewer`, and `qa` (two
rounds — no self-review at any point): `qa` cleared coverage with no
blocking gaps; `security` found the blocking RLS-toggle defect plus three
Low/informational items; `code-reviewer` independently converged on the
same RLS defect from a different angle plus found a doc-completeness gap
(the completion-status note overclaimed invariant L1 as fully addressed
while five `ledger-finance`-owned documents still carried the pre-split
enumeration), a message that would go factually stale in HR-9's own
designed window, and a CI-reachable test-fixture race. All fixed in one
consolidated fix wave, during which `ledger-finance` also found and fixed
a second, previously-undetected flaky test
(`TestGetSummary_CombinesBothLockedOriginsInEitherRowOrder`, caused by
PostgreSQL's synchronized sequential scans rotating row order under
concurrent load — fixed with `SET LOCAL synchronize_seqscans = off` plus
a bounded, still-hard-failing retry).

New governance item **HR-15** (not implemented): a `BEFORE UPDATE`
trigger on `ledger_accounts` guarding `account_type`/`wallet_id`/
`asset_code`/`tenant_id` immutability is a required gate before any
`transaction_type` posts to a locked-origin account — nothing currently
prevents an `UPDATE` from retroactively falsifying invariant L1's origin
attribution.

Orchestrator independently verified throughout: `gofmt`, `go build
./...`, `go vet ./...`, `golangci-lint run` (0 issues) clean before and
after the fix wave; full `go test -tags=integration ./...` run repeatedly
(5+ times across both rounds) with zero failures and no flake recurrence;
migration round-trip confirmed against both a throwaway database and the
shared dev database.

**Workstreams B/C/D/E/F — design/validation only, no code authorized.**
Workstream B (wagering-progress integrity, gate G-3) closed at the design
level via Model C (dual-measure derived progress), independently
validated by `sportsbook`, `bonus-engine`, and `architect` across two
rounds — BLOCKED on Stage 4H-B1 authorization, not on further design.
Workstream C (Terminal-Grant technical contract, gate G-2) fully modeled,
does not select the human decision. Workstream D (self-exclusion
technical hardening) extended ADR 0034, does not select the
`OpenBetSelfExclusionPolicy` default. Workstream E (sportsbook conformance)
confirmed ADR 0038's accounting design is internally consistent with the
Workstream A/B changes — read-only, no sportsbook code. Workstream F
produced `docs/decisions/0039-human-decision-register-stage-4h-b0-r7.md`,
formalizing the three still-unmade human decisions.

**Disclosed, not fixed this stage**: `LF-0048-1` (a pre-existing
reconciliation false-positive for entry-less ledger accounts, unrelated
to this stage's changes, pinned by a characterization test); the ADR 0035
`ledger_accounts_owner_family` CHECK collision (0048 has now landed
first, so the collision is owed by ADR 0035's amendment when it lands);
two Low, optional security follow-ups from the final S-1 re-check; and a
pre-existing governance-doc gap found during this stage's own close-out —
`docs/governance/project-status.md` has no dedicated sections for Stage
4H-B0-R5 or Stage 4H-B0-R6 (this file and `docs/active-stage.md` and
`docs/governance/task-registry.md` do carry them) — not backfilled this
stage, disclosed rather than silently perpetuated.

### Files changed this stage

New: `migrations/0048_ledger_locked_account_origin_split.{up,down}.sql`,
`internal/ledger/locked_origin_split_integration_test.go`,
`internal/ledger/migration_0048_integration_test.go`,
`internal/wallet/locked_origin_summary_integration_test.go`,
`docs/decisions/0039-human-decision-register-stage-4h-b0-r7.md`. Extended:
`internal/ledger/ledger.go`, `internal/wallet/wallet.go`,
`docs/architecture/ledger-accounting-model.md` (§6.5, §6.6, HR-9 through
HR-15), `docs/architecture/reconciliation-model.md`,
`docs/architecture/03-database-architecture.md`,
`docs/architecture/06-wallet-ledger-architecture.md`,
`docs/architecture/financial-domain-model.md`,
`docs/architecture/financial-transaction-flows.md`,
`docs/architecture/09-sportsbook-architecture.md`,
`docs/architecture/10-bonus-engine-architecture.md` (Terminal-Grant
Technical Contract T.1-T.13), `docs/decisions/0034-*.md` (§14.10-§14.13),
`docs/governance/{task-registry,project-status}.md`,
`docs/active-stage.md`, `docs/progress.md` (this entry). Commit `17f1057`
on `claude/focused-wright-jw88w9`, pushed.

### Next stage

Not started. **Stage 4H-B1 (Bonus Engine) NOT authorized by this stage.**
B1 readiness: **NOT READY** — `player_locked` phase 2's cash-only ledger
capability is implementation-complete and reviewed, but Bonus Engine,
Gamification, and the Reward Orchestrator remain entirely unbuilt, and
three human decisions (G-2 Terminal-Grant settlement-credit resolution,
`OpenBetSelfExclusionPolicy` default, mixed/bonus-funded cashout policy
plus its companion FD-1 question) remain unmade. "Architecture/design
exists" is not treated as sufficient.

## Stage 4H-B1: Bonus Engine implementation — Wave 1, Wave 1.5, Wave 1.5 Fix Wave

Human-authorized in three successive directives. All work to date is
**design/documentation only** — no `internal/bonus`, `internal/crm`,
`internal/affiliate`, `internal/gamification`, or `internal/economicop`
package exists, no migration beyond `0049` was written, no route exists.

- **Wave 1** (9 specialists): full Bonus Engine domain model, catalogue,
  targeting/bulk-assignment, Bonus Suggestions, segmentation placement.
  Reviewed; 2 real corrections found and fixed.
- **Wave 1.5** (architecture reconciliation gate — CRM/Affiliate/
  Segmentation/G-2/Casino postWin): gate report returned **NOT READY**,
  4 P0s (LF-2, SEC-W15-01/02/03) + ~20 P1s.
- **Wave 1.5 Fix Wave, Phase 1** (6 dispatches, commits `3ce48f4`..`a1f6fd4`):
  redesigned the Grant terminal-state invariant (eligibility vs.
  disposition split, `HeldDispositionRecord`), fixed casino postWin's
  query/lock-release defects, designed `SEP-1` (actor≠beneficiary) and
  `AFF-4E-1` (affiliate four-eyes), created `EconomicOperationIdentity`
  (doc 34) to close the CRM bulk-decomposition vector.
- **Wave 1.5 Fix Wave, Phase 2** (11 independent re-verification
  dispatches, commit `08d10e3`): **Verdict: NOT READY.** Full report at
  `docs/governance/wave-1.5-fixwave-phase2-report.md`. None of the four
  original P0s independently certified closed; four new P0-severity
  findings surfaced, three of them defects inside this round's own
  fixes (a reachable ledger-invariant violation in casino's
  lock-release logic; the LF-2 fix's own disposition-resolution step
  is an ungated self-dealing surface; `SEP-1`'s shared resolver fails
  open on a partial RLS read); bonus-engine's and casino's two largest
  fixes were independently found architecturally incompatible by three
  reviewers from three angles. No Human Decision Register item was
  selected. Per the authorizing directive, the Orchestrator **stops**
  here — no Wave 2, no CRM/Affiliate/Gamification/bonus-funded-wagering
  implementation without a new human directive.
- **Wave 1.5 Fix Round 2** (24 specialist dispatches across three
  phases, HEAD `61a203d`): **Verdict: READY** for Wave 2 authorization,
  subject to two routed, non-blocking P1s (LF-10; `SEP-1`'s
  `ancestor_closure` resolver's `agentnetwork` tenant-edge dependency).
  Full report at `docs/governance/wave-1.5-fix-round-2-report.md`. All
  four original P0s and all four Phase-2-discovered new P0s
  independently certified closed by reviewers who did not author the
  fixes. This round also delivered the human-directed Product Surfaces
  roadmap gate: `docs/architecture/35-37` define Stage 6A (Back Office
  MVP), 6B (Partner Console MVP), 6C (B2C Brand Frontend MVP), 6D
  (Retail/POS), with a full domain-dependency graph and an explicit
  answer to when Back Office implementation may start. No Human
  Decision Register item was selected; no code, migration, or UI was
  written. Per the authorizing directive, the Orchestrator **stops**
  here — Wave 2, CRM, Affiliate, Gamification, and Back Office/Partner
  Console/B2C frontend implementation remain unauthorized pending a new
  human directive.

## Stage 4H-B1 Wave 2: Bonus Engine implementation — REAL CODE, human-authorized

The first real-code implementation stage for the Bonus Engine. 11
specialist phases plus 2 dependency-request fix dispatches, commits
`d145ba1`..`3526d87`. Full report: `docs/governance/wave-2-report.md`.

Built and tested: `internal/bonus` (Campaign/Offer/Grant lifecycle, the
G-2/AOE mechanism, 5 bonus types — Deposit/Reload/Cashback/Generic-Wagering/Coupon,
Bonus Conversion now functional end to end, static/pinned targeting,
bulk grant jobs, Bonus Suggestion lifecycle, four-eyes governance),
`internal/economicop` (`EconomicOperationIdentity`), real casino
integration (`postWin`/`postRollback` now call the G-2 seams), a landed
`bonus_conversion` Risk Operation, and player/staff HTTP surfaces.
Migrations `0050`-`0067`.

Seven real defects found and fixed during implementation/review (each
with a regression test proven to fail pre-fix and pass post-fix): an
RG/Risk/AssetAuthorization gate bypass on `ACTION_ROUTE_TO_CASH`; its
accompanying posting-shape bug; a missing `SEP-1` Step-0 self-proof; a
bulk-worker error-masking bug; an AOE-attribution gap and a
redelivery-idempotency bug in casino's integration; an EOI/Risk
lock-ordering reversal; and a structural no-op in the single-grant EOI
recipient-ceiling check that had reopened the SEC-W15-02 decomposition
vector through a different door.

**Verdict: READY**, subject to explicitly-open, non-blocking items:
LF-10's general case (still ledger-finance's, fails closed safely); no
KYC-tier taxonomy exists for Bonus to reference; no multi-account abuse
detector exists (schema-only); no HTTP admin surface exists yet for
four-eyes filing/EOI-root-minting/campaign-activation (the underlying
mechanisms are built and tested). Segmentation (dynamic), CRM,
Affiliate, Gamification, real sportsbook, and all real external
providers were correctly not implemented, per the directive's scope.

No Human Decision Register item was selected. Per the authorizing
directive, the Orchestrator **stops** here — Wave 3, CRM, Affiliate,
Gamification, sportsbook, Retail/POS, and Back Office/Partner
Console/B2C frontend implementation remain unauthorized pending a new
human directive.

## Stage 4H-B1 Wave 3: Bonus Engine completion, integration hardening & final financial gate

Human-authorized completion of Wave 2's remaining scope plus integration
hardening. Mandatory first action: `architect` produced
`docs/governance/wave-3-reconnaissance.md`, a from-code (not from-docs)
reconstruction of the actual state Wave 2 had left, before any code was
written. 11 specialist phases plus 1 out-of-band product-owner-proxy
dispatch, commits `61601ad`..`cd6ee62`. Full report:
`docs/governance/wave-3-report.md`.

Built and tested: real deposit/reload/cashback/expiry sweep jobs (not
just the mechanism — durable, tenant-scoped, watermarked); cash-funded
wagering-contribution event consumption wired into `postBet` in the same
transaction as the bet's own posting (bonus-funded/locked-stake staking
deliberately not built — a disclosed, gated scope decision); four-eyes
application-level wiring for 4 of 7 `ChangeOperation` types; new
HTTP/API admin surfaces for four-eyes filing/approval, EOI minting, and
campaign/offer/manual-grant/bulk-job operations (API only, no Back
Office UI, per the directive). Migrations `0068`-`0070` (schema only,
landed in Phase 2; no new migrations in later phases).

Twelve real defects found and fixed across the review chain, each with
a regression test proven to fail pre-fix and pass post-fix: two live
fail-opens in the cash-funded wagering path (cross-asset attribution; a
nil wagering-target read as "already satisfied"); a multi-account
first-deposit eligibility gap; five security defects (a subject-set
containment gap letting a `single_subject` EOI root authorize a grant to
the wrong player; an unpinned bulk-job four-eyes payload; an
unbounded-budget EOI-mint path; a tamperable four-eyes forensic-approval
record; a numeric-scan bug that made the forensic fix itself
unreachable); two qa-found defects (a deposit-sweep/cashback
cross-matching bug; an HTTP handler 500 on an omitted optional field);
two architect-found defects (a live betting-outage vector from an
unvalidated Offer-authoring field; the same EOI-budget-bound gap closed
structurally rather than only at the HTTP layer); and one
ledger-finance-found defect (`DR-4HB1W3-LF-01`: the cashback scheduler
and expiry sweep compared windows against the API host's clock instead
of the database's own, a silent platform-favoring underpayment/
early-expiry risk under clock skew).

This Wave's branch was interrupted by a container restart twice
(mid-bonus-engine's Phase 3, mid-security's first Phase 6 attempt).
Both times the interrupted dispatch's own report was lost but its code
survived uncommitted; the Orchestrator independently investigated and
fully re-verified each surviving diff (build/vet/fmt/full integration
suite/`-race`) before trusting and committing it, then re-dispatched a
fresh phase to complete the remaining scope — no work was lost or
discarded without investigation.

**Verdict: READY**, subject to explicitly-open, non-blocking, disclosed
items: a latent (not live) lock-order inversion gated on
`ConvertGrant`/held-disposition resolution becoming reachable alongside
a bet; a standing-authorization EOI type with no mint point, inert only
because of the jurisdiction gap; a per-Offer-version idempotency scoping
narrowness with no live exposure; the bulk-job HTTP-execute path
confirmed fail-closed but non-functional (F3); 3 of 7 `ChangeOperation`
types still unwired (posting shapes now specified for whoever builds
them next); the platform-wide jurisdiction-resolver gap (pre-existing,
shared with casino) that denies all three new sweeps' actual issuance;
the KYC-tier taxonomy gap (scheduled as a future cross-domain dispatch,
not resolved). CRM, Affiliate, Gamification, real sportsbook, Retail,
Back Office/Partner Console/B2C frontend UI, and real external
providers were correctly not implemented, confirmed by an explicit
sportsbook boundary review this Wave.

No pre-existing Human Decision Register item was selected, narrowed, or
defaulted. One new item was raised (not resolved) for human decision:
whether this platform may ever create a receivable from a customer by
clawing back cash from an already-`converted` Grant's cancellation —
blocks only that one future extension, nothing already shipped. Per
the authorizing directive, the Orchestrator **stops** here — Wave 4,
CRM, Affiliate, Gamification, sportsbook, Retail/POS, and Back
Office/Partner Console/B2C frontend implementation remain unauthorized
pending a new human directive.

## Stage 4I: Platform-wide jurisdiction resolution foundation

Human-authorized closure of the platform-wide jurisdiction-resolver gap
Wave 3 carried forward. Mandatory first action: `architect` produced
`docs/governance/stage-4i-reconnaissance.md`, a from-code reconstruction
finding the gap was not "a missing resolver" but a fully-built
configuration/consumption layer with a completely absent production
layer, split across four mutually incompatible absent-value contracts.
Thirteen specialist phases, commits `2bab29f`..`f1f8d13`. Full report:
`docs/governance/stage-4i-report.md`.

Built and tested: a canonical, provider-neutral `internal/jurisdiction`
package (a non-forgeable `Resolution` type, a 3-valued `Outcome` plus a
separate diagnostic `Reason` enum, a `ReadOnlyQuerier` interface making
the mandatory read-only-on-the-evaluation-path constraint a compile-time
property, not a convention); a registry admin surface (`jurisdictions`/
`licences`, previously writable only by direct DB access); an append-only
`jurisdiction_resolutions` table plus a `jurisdiction_resolution_active`
precondition table; migrations `0071`-`0073`. Four real consumers wired
correctly for the first time: AssetAuthorization (unchanged, confirmed
correct); Risk (reviewed and confirmed its existing conditional
fail-closed contract was already correct, not a defect); casino's
per-game jurisdiction blocklist (a genuine fail-open defect — the check
silently never executed since its one caller never set the field —
fully remediated, armed only per-game, with a byte-identical
player-facing response collapsing "unresolved" and "blocked" to prevent
an oracle); five Bonus admin surfaces (client-suppliable
`jurisdiction_code` removed entirely, replaced with server-side
resolution, closing a latent fail-open in the shared jurisdiction-lookup
helper along the way).

Twelve real defects found and fixed across the review chain, each with a
regression test proven to fail pre-fix and pass post-fix: an unclamped
four-eyes threshold letting staff lower a dual-control requirement to 1
on a money-moving disposition; a missing audit gap on a casino admin
write that was about to become a live denial control; two RLS policy
gaps on new tables (a `FOR ALL` policy silently granting DELETE, and a
missing TRUNCATE-deny trigger that let any tenant-scoped connection erase
every tenant's precondition rows unaudited); a missing required reason
code; a test-refactor defect that had silently orphaned a production
four-eyes wrapper from any test coverage; two further test-coverage gaps
(a hand-copied test helper duplicating enough production logic to mask a
real regression; missing forged-payload and unavailable-resolver
adversarial tests); and a missing tenant-scope assertion in the resolver
itself, found in the final architectural certification pass, where the
underlying tables carry no RLS at all and isolation had been resting on
caller discipline alone.

**Verdict: PARTIALLY IMPLEMENTED** (per CLAUDE.md's no-fake-completion
rule, deliberately — not a shortfall). The foundation is real, tested,
and independently certified (CERTIFIED WITH NAMED EXCEPTIONS, none
blocking), but does not yet resolve any player's actual jurisdiction: the
one producible basis has no application write path, so every
player-scoped resolution correctly returns "unresolved" today, leaving
Bonus deposit/cashback issuance blocked and no jurisdiction-based
regulatory claim possible. Six new Human Decision Register items were
opened (HDR-J-1 through HDR-J-6, `docs/decisions/0041-...md`), none
decided; HDR-J-3 (whether to collect a player residence/location/
nationality attribute at all) is the single highest-leverage item, since
nothing else can unblock the resolver's actual capability without it. No
pre-existing Human Decision Register item was selected, narrowed, or
defaulted — confirmed independently by a dedicated sportsbook boundary
review. CRM, Affiliate, Gamification, real sportsbook, Retail, Back
Office/Partner Console/B2C frontend UI, and every real external vendor
were correctly not implemented. Per the authorizing directive, the
Orchestrator **stops** here — the next stage, and every out-of-scope
domain named above, remain unauthorized pending a new human directive.

## Stage 4I Phase A: tenant-licence write path — IMPLEMENTED

Preceded by two governance-only gates, both delivered directly (no code):
`docs/decisions/0042-human-decision-response.md` recorded the human's
verbatim decisions for all 11 previously-open items from Stage 4I's own
register plus the pre-existing register (HDR-J-1 through HDR-J-6 incl.
HDR-J-3's 8 sub-items; G-2; `OpenBetSelfExclusionPolicy`; the mixed/
bonus-funded sportsbook cashout policy plus FD-1; the converted-Grant-
cancellation/receivable question); `docs/plans/stage-4i-jurisdiction-
implementation-plan.md` then traced every decision into concrete
technical consequences (a 24-domain impact matrix, a fail-closed
analysis, a Human Decision Traceability Matrix, and a 9-phase
implementation sequence, Phases A-I) — planning only, approved by the
human for Phase A specifically.

**What Phase A closes.** The plan's own current-state assessment found
the sharpest remaining gap in Stage 4I's foundation: the resolver's one
producible basis, `tenant_licence`, reads `tenants.licence_id`, but
nothing in the application had ever written that column — verified by
grep, zero write call sites existed anywhere in the repository. Phase A
adds exactly that write path and nothing else.

**Built:** `internal/jurisdiction.AssignTenantLicence` (new file
`tenant_licence_admin.go`), the platform-admin-only endpoint
`PUT /v1/admin/tenants/{tenantID}/licence`, and a new permission
`PermTenantLicenceAssign` (deliberately separate from both
`PermTenantWrite` and `PermJurisdictionRegistryManage` — an `architect`
ruling: binding a live tenant to a licence determines which
jurisdiction's rules govern that tenant, a materially different
authorizing act from either). No migration was needed — the schema
(`tenants.licence_id`, the composite FK `tenants_licence_matches_model`
from migration 0007) already existed and had simply never been writable.
The licensee/`licensing_model` invariant is enforced entirely by that
pre-existing FK, never re-implemented in Go, per an explicit
non-duplication ruling. The operation is generic across both the
platform-licensed and BYOL (`own_licence`) shapes of the hybrid licensing
model, with a dedicated, passing test proving each.

**Review chain, all independent, none self-certified:** `architect`
design ruling (8 numbered decisions: placement, permission model,
transaction-helper choice, API shape, the BYOL-genericity boundary, audit
shape, fail-closed requirements, in/out-of-scope items) → `backend`
implementation → three parallel independent reviews (`security`,
`architect`, `qa`) → `backend` fix round closing every review finding →
orchestrator integration and independent re-verification of every claim
(build/vet/fmt/unit/integration-against-real-Postgres/race, run directly
by the orchestrator, not merely trusted from agent reports).

**Findings and disposition** (full ledger:
`docs/governance/task-registry.md`'s new "Stage 4I Phase A" section):
one P2 finding fixed in the same session (the audit record for this
tenant-targeted mutation was initially written platform-scoped, via the
shared registry-audit helper's hardcoded convention, so the affected
tenant could never see it via its own `PermAuditRead` — fixed by
threading a `tenantID` parameter through the shared helper and switching
the handler's transaction wrapper to match the codebase's existing
"platform_admin acts on a target tenant" precedent, e.g. brand/staff
creation); two P1 test-coverage gaps closed (the BYOL success path had
zero coverage at either layer; none of the 10 original HTTP-layer tests
ever drove the endpoint's own domain-error branches, including the
codebase's only `ErrNotFound` producer); one convergent test-strength
finding (independently raised by both `architect` and `qa`) closed by
strengthening the concurrency test to assert an actual before/after audit
chain rather than merely "no deadlock." Three P2 findings were
deliberately NOT fixed and instead routed as open, named, non-blocking
decisions: `licences.status`/`expires_at` are checked at bind time only,
never at resolve time (amends SEC-4I-F10, whose existing hard trigger
already covers closing this — unchanged, still open); `licences` has no
tenant-ownership binding, so two distinct BYOL tenants could in principle
be pointed at the same licence row with no constraint violation (routed
to `architect`/compliance, relevant only once a BYOL tenant is actually
onboarded); and no dual control exists on this single-permission,
no-RLS-backed write (routed to `architect`/`product-owner-proxy`, an
open decision, not a Phase A blocker). Several P3 observational findings
(pre-existing, platform-wide `decodeJSON` characteristics; unaudited
denied attempts; unbounded reason-code text; controlled-but-verbatim
error messages) were recorded as not introduced by, and not blocking,
this phase.

**Verdicts:** `security` — CERTIFIED WITH NAMED EXCEPTIONS (no P0/P1
found). `architect` — ARCHITECTURALLY CERTIFIED, with named exceptions
(no blocking issues; several implementation choices — the single-
statement atomic CTE update, the absent-key-vs-null presence
distinction, auditing a no-op reassignment — independently judged to
exceed the ruling's own specification). `qa` — READY WITH NAMED GAPS,
both P1s closed in the fix round it flagged them in.

**Honest scope statement.** Phase A adds a write path, not a consumer.
Both of the platform's production `Resolve` call sites
(`internal/casino/orchestrator.go`, `internal/bonus/eligibility.go`)
structurally always pass a non-nil `PlayerAccountID` — a Go value type,
never nilable at that call site — so every player-scoped resolution
still returns `unresolved(no_signal)`, byte-for-byte identical to before
this phase. No tenant/brand-subject consumer exists anywhere in the
repository outside test code. Casino's per-game blocklist behaviour is
unchanged for every game. Bonus issuance remains blocked. The
`tenant_licence` basis is now technically producible for the first time
in the platform's history, but stays unobserved in production until (a)
a tenant/brand-subject consumer exists (a later phase) and (b) a human
operational data-entry step assigns the platform's real licence to the
real production tenant — explicitly not performed by this phase.
Confirmed strictly non-regressive for every tenant and every operation,
independently, by all three review passes.

**Explicitly deferred, not performed this phase, per the authorizing
directive's own named prohibitions:** player physical-location
collection, declared/verified residence collection, KYC verified-
residence workflow, nationality, jurisdiction precedence configuration
content, permitted-market population, G-2 bonus-brand policy
implementation, sportsbook cashout, converted-Grant clawback, BYOL
onboarding, and any resolver-side (evaluation-time) licence-status/
expiry check. No new Human Decision Register item was required or
invented — every open question found during review routes to an
existing specialist's ordinary authority.

Per the authorizing directive: the Orchestrator **stops** here. Phases
B through I, and every domain named above, remain unauthorized pending a
separate human directive reviewing this Phase A completion report.

## Stage 4I "Phase B" (human directive's own numbering — see the phase-lettering correction note in `docs/plans/stage-4i-jurisdiction-implementation-plan.md` §14) — player jurisdiction evidence foundation

Authorized narrowly: build the technical evidence *foundation* for
player-level jurisdiction determination (HDR-J-3a/b/c/e/f/g/h,
`docs/decisions/0042-human-decision-response.md`), with an explicit
activation boundary so building the capability never itself activates a
regulatory decision. Three named subsystems: declared residence
(self-reported, unverified), KYC-verified residence (reviewer-determined),
and a physical-location signal abstraction (interface + mock only).

**Built:** migration `0074` (four new nullable, paired-NULL-CHECK-
constrained columns on `player_accounts`/`kyc_verifications`, plus a new
`jurisdiction_evidence_collection_active` table with its final hardened
RLS/TRUNCATE-deny shape from day one — not reproducing either historical
defect `jurisdiction_resolution_active` needed migrations 0072/0073 to
fix); `internal/identity.SetPlayerAccountDeclaredResidence`/
`GetDeclaredResidence` and `GET`/`PUT /v1/me/residence`;
`internal/kyc.ReviewVerification`'s extension with an optional
`VerifiedResidenceCountry` (no new endpoint — the existing `POST
/v1/admin/kyc/verifications/{id}/review` gained one optional field) and
`GetVerifiedResidence`; `internal/jurisdiction/evidence_collection_active.go`
(the per-tenant, per-evidence-type activation switch) and its admin
surface (`GET`/`PUT /v1/admin/jurisdiction-evidence-collection[/{evidenceType}]`,
a new `RoleCompliance`-only permission `PermJurisdictionEvidenceCollectionActivate`);
`internal/geolocation` (a new package: `LocationProvider` interface +
`MockLocationProvider` only — no vendor, no HTTP route, no resolver
wiring); a new `PermPlayerResidenceRead` permission, defined and role-
scoped but wired to zero handlers this phase (deliberate — the staff-
facing read surface it would gate is a future phase); `internal/validation`'s
ISO-3166-1 alpha-2 allowlist (`country.go`, written directly rather than
via subagent dispatch after three reproducible content-filter failures
generating the same content). Also landed in this phase's own commit
series: the OpenAPI backfill for the pre-existing Stage 4I admin-endpoint
gap Phase A had flagged and deferred (`PHASE-A-ARCH-2`, now closed), plus
full OpenAPI documentation for this phase's own new surface.

**The decisive scope-control property, verified independently by all
three reviewers:** `internal/jurisdiction/resolver.go` has **zero diff**.
`Resolve` does not read either residence column and does not call either
new read accessor. Evidence is technically collectible (subject to the
activation switch) but is not yet consumed by any jurisdiction decision —
"evidence available to the resolver via well-defined interfaces" is
discharged entirely by two standalone, independently-tested read
accessors with no non-test callers, built for a future phase to wire in.
No player/tenant-subject behavior changes as a result of this phase.

**Review chain, all independent, none self-certified:** `architect`
design ruling (12 numbered decisions) → parallel `backend`/`integrations`/
`identity-compliance` implementation dispatches → three parallel
independent reviews (`security`, `architect`, `qa`) → orchestrator fix
round → orchestrator integration and independent re-verification
(build/vet/fmt/unit/integration-against-real-Postgres/race, across every
touched and downstream package, run directly, not merely trusted from
agent reports).

**Findings and disposition** (full ledger: `docs/governance/task-registry.md`'s
new "Stage 4I Phase B" section): one P1 finding, independently converged
on by all three reviewers from different angles, was fixed — the
`kyc.verified_residence_determined` audit entry had included the
reviewer's free-text `reason` field verbatim, the one channel that could
carry the country value the entry's own "never record the value" rule
existed to prevent; the `reason` key was removed from that specific
metadata map, and a new test deliberately puts the country in the
free-text reason and proves the raw audit row never contains it. Two
further P1 test-coverage gaps (`qa`) were closed: no test exercised the
new CHECK/FK constraints directly via raw SQL (four new tests added,
mirroring this codebase's own established pattern for exactly this kind
of test), and no cross-tenant isolation test existed for
`kyc.GetVerifiedResidence` — closed at the root by removing that
function's `tenantID` parameter entirely (it had zero non-test callers)
so it relies on RLS alone, exactly like its `identity` sibling, rather
than merely adding the missing test around the weaker shape. Several P2s
were fixed as low-risk, already-in-scope hardening (KYC audit entries now
carry IP/UA/request-id; both packages' audit timestamps now come from the
database's own `RETURNING` value instead of a separate Go-side clock,
closing a forensic clock-skew risk). One P2 — an enforcement-asymmetry
between the two write paths' activation gates (the KYC gate is
structurally unreachable-around; the declared-residence gate is enforced
only in the HTTP handler, with the underlying identity function ungated
save for a doc comment) — was **not** fixed: both `security` and
`architect` independently flagged it and converged on the same preferred
remedy (a database trigger), but implementing it is a cross-table,
joint `architect`+`security` design decision, now recorded as a **hard
prerequisite gate** on the still-deferred staff-correction-of-declared-
residence endpoint (`PHASE-B-ARCH-1`). A handful of P3/P4 items were
accepted as named, non-blocking exceptions (a stale ruling claim about
verified-residence determinations being terminal-only, corrected here
rather than repeated: a determination can be made on a non-terminal
review and is read back only once `approved`; `effective_from`/actor
provenance not updating on a later toggle of the activation switch,
routed to `architect` since the identical defect exists on the sibling
`jurisdiction_resolution_active` table; no row locks on two check-then-
act paths, one of them pre-existing; `PermPlayerResidenceRead` and the
two read accessors being wired to nothing yet; `GET /v1/me/residence`
not itself being gated by the activation switch).

**Verdicts:** `security` — CERTIFIED WITH NAMED EXCEPTIONS (one P1,
fixed; the enforcement-asymmetry exception now explicitly gates
`PHASE-B-ARCH-1`, not this phase). `architect` — CERTIFIED WITH NAMED
EXCEPTIONS (no blocking issues; all twelve design rulings conformed,
`resolver.go` and the `Basis` enum verified at literal zero diff). `qa`
— READY WITH NAMED GAPS, all three P1s closed in the fix round.

**Documentation updated this phase:** `docs/api/openapi/platform-api.yaml`
(the Phase A backfill plus this phase's own new surface);
`docs/architecture/16-privacy.md` (corrected the stale Stage-2-era "no
name/DOB/address collected" claim; added sensitive-fields-inventory rows
for the new residence columns and the pre-existing, previously-
uninventoried `kyc_documents.issuing_country`; added an explicit
lawful-basis/activation-boundary section stating collection/vendor/
retention/enforcement are all `NOT IMPLEMENTED` pending human/legal
decisions); `docs/security/security-architecture.md` (new §J4I.12);
`docs/governance/ownership.md` (the first genuine ownership overlap
between the jurisdiction and identity-compliance domains, split by
column not by table); `docs/plans/stage-4i-jurisdiction-implementation-plan.md`
(the phase-lettering correction note this document's §14 needed, per the
architect's own finding).

**Explicitly deferred, not performed this phase, per the authorizing
directive's own named prohibitions:** HDR-J-2's full precedence policy
(`jurisdiction_precedence_configs` remains shape-only, zero rows, zero
write surface); permitted-market population; nationality; G-2, sportsbook
cashout, converted-Grant clawback, BYOL onboarding; any resolver wiring
of the two new read accessors; staff correction of declared residence
(now also gated by the enforcement-asymmetry disposition above); a real
physical-location vendor and its own selection/security review; the
retention/erasure job (HDR-J-3f); a staff-facing read surface for
`player_residence:read`. No new Human Decision Register item was
required — every open question found during review either has an
existing owner or is recorded as a named prerequisite for a specific
future phase.

Per the authorizing directive: the Orchestrator **stops** here. Phase C
and beyond, HDR-J-2 precedence configuration, permitted-market
population, production jurisdiction enforcement, G-2, sportsbook
cashout, and converted-Grant clawback remain unauthorized pending a
separate human directive reviewing this Phase B completion report.

## Stage 4I "PHASE-B-ARCH-1" — activation-gate enforcement asymmetry hardening gate

A narrowly-scoped human directive closing the one P2 Phase B recorded as a
hard prerequisite before any Phase C-dependent work: the declared-residence
write path's activation gate was enforced only in the HTTP handler, while
the sibling KYC path enforced it structurally inside the domain function
itself — any future internal caller of the exported identity function
could have bypassed the gate entirely.

**Built:** the architect ruling (independently re-verifying every claim
from the prior Phase B reviews rather than trusting them, including the
import graph via `go list -deps`) moved the check INSIDE
`identity.SetPlayerAccountDeclaredResidence` itself, in the same
transaction as the write, mirroring `kyc.ReviewVerification`'s own gate
exactly — closing the asymmetry rather than doubling the check. A new
connection-scope assertion (tenant-scoped to `p.TenantID` specifically,
never player-scoped) runs first, closing a real, verified hazard: because
`jurisdiction_evidence_collection_active`'s RLS excludes player-scoped
connections while `player_accounts`' own RLS does not, a player-scoped
caller would otherwise have gotten a misleading "collection is off" 403
while collection was actually on. The HTTP handler's own duplicate check
was deleted, not kept as defence-in-depth — the same predicate checked
twice by the same package graph is how the two paths drifted apart in
the first place. A `BEFORE INSERT OR UPDATE` trigger — the remedy both
prior Phase B reviews had floated as "preferred" — was considered and
**rejected**: it would need to read the RLS-protected gate table from
inside a trigger body, which would either misfire on legitimate
player-scoped writes or require a `SECURITY DEFINER` RLS-bypassing
function, a net security regression, not a hardening.

**Independent review, no self-certification:** `security` — CERTIFIED
WITH NAMED EXCEPTIONS, having attempted 10 distinct bypass classes
(direct service-layer calls with the gate on/off, cross-tenant and
forged-tenant invocation, missing/player-scoped connections, concurrent
toggle races, HTTP bypass routes, background callers, transaction
rollback) via its own throwaway adversarial probes, not just reading the
tests — the P2 is genuinely closed, no bypass found. `qa` — READY WITH
NAMED GAPS, having read every test line by line and confirmed each
proves what its name claims (in particular verifying the two
mis-scoping tests correctly distinguish "some error" from "specifically
not the closed-gate sentinel," and that the gate's position in the
function still lets the pre-existing `UnknownPlayerAccountID`/
`ActorReasonCodeValidation` tests exercise their own named branches
rather than accidentally testing the gate instead).

**Five findings from the two reviews, all P3/P4 (no P0/P1/P2 — the pass's
one target P2 is closed), four fixed, one accepted and documented:**
ISO-3166 validation of the country code was caller-only inside the domain
function (the same doc-comment-as-control pattern this pass just removed
for the gate, on a different field) — fixed by validating in-function,
mirroring KYC's own pattern; a real, previously-untested cross-tenant
write-path gap (tenant B's self-consistent token targeting tenant A's
player_account_id) — fixed, new regression test added; a documentation
overclaim about "no caller can reach either write" — corrected to scope
it to Go callers of the domain functions specifically; the
trigger-rejection rationale's "would misfire on player-scoped writes"
argument — corrected in place (the new scope assertion actually
neutralizes that specific argument, though the trigger decision itself
was not reopened, since no specialist unilaterally overturns another's
ruling); the mis-scoped-transaction error was an unclassifiable bare
error — fixed with a dedicated sentinel. One accepted, documented,
not-fixed gap: a bounded TOCTOU window on the unlocked gate-check read,
identical to a pre-existing characteristic already on `kyc.
ReviewVerification`, not introduced or widened by this pass — a fix
would be a cross-path design change belonging to `architect`, out of
this hardening gate's scope.

**Deferred per the architect's own explicit ruling, not silently
dropped:** the separate `effective_from`/actor-provenance staleness
finding (Phase B's own PHASE-B-SEC-2) — the architect ruled this must
NOT be fixed in this pass (the identical defect exists on the older,
sibling `jurisdiction_resolution_active` table; fixing only the newer
one would create a fresh inconsistency; the right fix is an append-only-
history modelling decision, not a one-line upsert change) and recorded a
full verbatim disposition (owner, affected tables, why deferring weakens
no current enforcement, what future phase must resolve it) in
`docs/governance/task-registry.md`'s "Stage 4I PHASE-B-ARCH-1" section.

**Validation:** build/vet/gofmt clean; full integration suite for
`internal/identity` (29 tests, including 9 new/updated for this gate),
`internal/httpserver`, `internal/kyc`, `internal/jurisdiction`,
`internal/validation` green; race-clean for the touched packages; broader
regression (`casino`/`bonus`/`risk`/`ledger`/`payments`/`withdrawal`)
green; migration-chain round-trip re-confirmed (no new migration in this
pass). `git diff --name-only` confirmed to touch exactly the expected
6 files: no migration, no new HTTP endpoint, no OpenAPI change, and
`internal/kyc`'s own gate at literal zero diff.

Per the authorizing directive: the Orchestrator **stops** here. Phase C,
HDR-J-2 precedence configuration, permitted-market population, a real
geolocation provider, nationality, G-2, sportsbook cashout, converted-
Grant clawback, and BYOL remain unauthorized pending a separate human
directive.

## Stage 4I Phase C: jurisdiction precedence and resolution rules foundation — IMPLEMENTED

A human "MASTER ORCHESTRATOR" directive authorized building the
deterministic TECHNICAL FOUNDATION for jurisdiction resolution per
HDR-J-1 through HDR-J-6 (including HDR-J-3's 8 sub-items a-h,
`docs/decisions/0042-human-decision-response.md`), explicitly NOT
activating production enforcement: no production market list, no country
allow/deny content, no production activation, no real geolocation vendor,
no nationality, no retention/erasure implementation, no legal-basis
determination, no staff-correction endpoint (unless the architect deemed
a minimal seam strictly required — it did not), no G-2/sportsbook-
cashout/converted-Grant-clawback/BYOL, and no payment/casino/risk
behaviour change except where a resolver seam was technically required.

**Mandatory pre-implementation impact-map analysis**, per the directive's
own requirement, ran before any code: reviewed every consumer of
`internal/jurisdiction` (`internal/casino`, `internal/bonus`, `internal/
risk`) and confirmed none would be touched — this phase builds a
standalone rule-engine package with zero wiring into any of them.

**Architect design ruling** (persisted in full, cited throughout
implementation): a complete type/function signature specification for
`DeterminePlayerJurisdiction`, `PlayerJurisdictionResult`, `Candidate`,
`ConsideredEvidence`, `Purpose`, `EvidenceSet`, `EvaluationPolicy`,
`LocationRequirement`, and `ComposeRestrictions`/`AppliedRestriction`/
`ComposedRestriction` — including an explicit **CRITICAL STOP CONDITION**
clause instructing the implementation to build the correct abstraction/
seam and flag a human decision item, rather than invent legal/policy
content, wherever "more restrictive wins" or an evidence-validity
threshold could not be represented without a legal judgment. The ruling
also named PC-GAP-4 (tenant/jurisdiction-aware precedence keying per
canonical-model §3.4) as documentation-only debt, not a code gap.

**Backend implementation, exactly per the ruling:**
`internal/jurisdiction.DeterminePlayerJurisdiction` (new file
`precedence.go`) — a pure function: no `context.Context`, no database
handle, never calls `time.Now()` (the caller supplies `AsOf`), verified by
a reflection-based purity test walking `DetermineParams`' full field tree
for any DB/context-shaped type. Implements `PurposeIdentityDetermination`
and `PurposeMarketAccessControl`; unconditionally refuses
`PurposeHistoricalReporting` with `ErrHistoricalPurposeNotComputable` (a
historical jurisdiction must be read from the event-time record, never
recomputed — the event-time semantics the directive required). New
supporting files: `purpose.go` (the `Purpose` taxonomy — a real typed
enum, deliberately separate from the pre-existing `OperationClass` enum,
with no mapping function between them, PC-GAP-3); `evidence.go`
(`EvidenceSet` — exactly three fields, `VerifiedResidence`/
`DeclaredResidence`/`LocationSignal`, enforced by a reflection tripwire
test; `LocationSignalState`, a closed six-value enum); `player_result.go`
(`PlayerJurisdictionCode` — a struct with an unexported field, not a
named string type, so a bare string conversion cannot construct one from
outside the package; `Candidate`/`ConsideredEvidence`/
`PlayerJurisdictionResult`, all non-forgeable by construction, mirroring
`Resolution`'s own pattern); `restriction.go` (`ComposeRestrictions` — the
canonical-model §7.3 most-restrictive-outcome composition primitive,
returning every contributor tied at the winning severity, never an
arbitrary pick).

**The canonical resolution result distinguishes** (never collapsed to a
generic "unknown," per the directive's explicit requirement): resolved;
unresolved-no-signal; unresolved-no-applicable-evidence (new
`ReasonNoApplicableEvidence`); unresolved-insufficient-confidence;
unresolved-evidence-invalid (new `ReasonEvidenceInvalid`); unresolved-
location-signal-unusable (new `ReasonLocationSignalUnusable`); and
conflicting evidence (`HasDisagreement()`, a recorded fact about the
evidence, never a distinct outcome value, since verified residence still
authoritatively resolves over a disagreeing declared value).

**Fail-closed/unresolved, verified structurally, not by convention:**
`TestDetermine_NeverEmitsATenantOrFallbackBasisOnAnyInput`
(`precedence_invariants_test.go`) exhaustively cross-products every
verified/declared/location/purpose/policy combination the engine accepts
and asserts no result ever carries `BasisTenantLicence`,
`BasisTenantAsserted`, or `BasisPlatformFallback` — no player jurisdiction
can become a tenant jurisdiction via any path this engine has. Two
policy-gated knobs (`LocationRequirement`, `EvaluationPolicy.
MaxLocationSignalAge`) have zero values that fail closed with
`ErrPolicyUnset` rather than defaulting permissively — PC-GAP-1/PC-GAP-2,
unmade human/legal decisions that must surface as caller-visible errors,
never guessed defaults.

**Independent review — four specialists in parallel, none seeing the
others' findings:** `architect` fidelity review (checking the
implementation against its own ruling line by line), `security`,
`identity-compliance` (compliance/privacy), and `qa` (adversarial test
design). Two defects were independently found by three of the four
reviewers via different methods: a slice-aliasing non-forgeability break
(`Candidates()`/`ConsideredEvidence()`/`Contributors()` all returned their
internal backing array directly rather than a defensive copy — fixed via
`slices.Clone`) and an `fmt` `%#v` redaction bypass (dumps unexported
field values including the country code, bypassing every `String()`
method's redaction — fixed with `GoString()`/`fmt.GoStringer` on all five
affected types). Three further P1/P2 architect findings and five
lower-severity security findings were fixed in the same round; full
findings/disposition ledger: `docs/governance/task-registry.md`'s "Stage
4I Phase C" section.

**Validation, run to completion before reporting done:** `go build
./...` clean; `go vet ./...` and `go vet -tags=integration ./...` clean;
`gofmt -l .` clean; `internal/jurisdiction` unit suite green (45 tests,
including 15 new regression tests added this fix round); `go test -race
./internal/jurisdiction/...` clean; whole-repo `go test ./...` green
(every package, no regressions); whole-repo `go test -tags=integration
./...` green against a real local Postgres (every package, including
`internal/bonus`, `internal/casino`, `internal/httpserver`, `internal/rg`,
`internal/ledger`, `internal/kyc`, `internal/identity` — no regression
anywhere).

**The decisive scope-control property, unchanged from every prior Stage
4I phase:** `internal/jurisdiction/resolver.go` — the only resolver any
consuming domain (`casino`, `bonus`, `risk`) actually calls — has **zero
diff**, confirmed by `git diff --stat` after implementation, after all
four reviews, and after the fix round. Zero production call sites of
`DeterminePlayerJurisdiction` or `ComposeRestrictions` exist anywhere.
`git status --porcelain` after the fix round showed changes confined
entirely to `internal/jurisdiction/` — no unrelated file touched.

**Documentation/governance updated this phase:** new §14 in
`docs/governance/stage-4i-canonical-model.md` (the canonical resolution
contract, operation taxonomy, evidence precedence, unresolved/fail-closed
semantics, more-restrictive semantics, event-time semantics, tenant/
player separation, and the full PC-GAP register with owner/reason/
dependency/future-phase/security-impact for each of PC-GAP-1 through 4);
§7.3 amended (the `blocked > restricted > allowed` MROC severity
vocabulary, now anchored in real code); §7.4 corrected (withdrawing the
prior "MROC is NOT built in Stage 4I" claim, since `ComposeRestrictions`
now exists, with the honest caveat that it has zero production callers);
`docs/governance/task-registry.md`'s new "Stage 4I Phase C" section (full
findings ledger); this entry.

Per the directive's own mandatory stop condition: the Orchestrator
**stops** here. Phase D and any production jurisdiction activation remain
unauthorized pending a separate human directive reviewing this Phase C
completion report.

## Stage 4I Phase D: jurisdiction policy configuration and operational semantics — IMPLEMENTED

A human "MASTER ORCHESTRATOR" directive, issued after review of the Phase C
completion report, authorized Stage 4I Phase D to resolve/formalize the
four Phase C deferred gaps (PC-GAP-1 through 4) as far as they could be
resolved without inventing legal/policy content, and to build the config
infrastructure the already-recorded human decisions (`docs/decisions/
0042-human-decision-response.md`) made safe to build now.

**Mandatory pre-implementation reconnaissance**, per the directive's own
requirement: confirmed `internal/jurisdiction/resolver.go` still treats
`OperationClass` as an opaque validated string with zero semantic
branching; confirmed all four existing `OperationClass` consumers (the
casino per-game blocklist, `assetregistry` layer 6, Bonus issuance/
conversion) are eligibility/availability/restriction-type decisions;
discovered `jurisdiction_precedence_configs` (migration 0071) already
existed as a shape-only, zero-row, zero-Go-reference table with the
*correct* append-only, effective-dated pattern — a materially better
foundation for PC-GAP-4 than the upsert-in-place pattern the two older
activation tables use (the PHASE-B-ARCH-1-deferred defect).

**Architect design ruling**, dispatched with the full reconnaissance
findings: a complete, binding specification covering all four gaps. The
ruling **corrected a load-bearing error in the orchestrator's own
reconnaissance** — the tempting inference that every `OperationClass`
deterministically requires `PurposeMarketAccessControl`, since every
current consumer makes an availability/restriction decision. The
architect rejected this: `Purpose` selects which evidence hierarchy is
*legally authoritative*, not what kind of decision a consumer makes, and
a per-game blocklist keyed on a player's *verified residence* (identity
determination) is an equally coherent, and in several regulated markets
the legally correct, design. The ruling: extend
`jurisdiction_precedence_configs` in place (not a sibling table) with
seven new columns; key strictly on the tenant's licensing jurisdiction
(`tenants.licence_id → licences.jurisdiction_id`), never `tenant_id`
directly, per canonical-model §3.4's pre-existing bootstrap-circularity
fix; forge-proof `effective_from`/`effective_to` via BEFORE INSERT/UPDATE
triggers so no writer — sanctioned or raw SQL — can backdate or
reopen a version; add RLS to this table for the first time (permissive
read, platform-admin-only write); build the `RequiredPurposes` mapping
seam with **zero mapping content** (all four `OperationClass` values fail
closed with `ErrPurposeMappingUndetermined`); and register three new
human-decision items (HDR-J-7/8/9) for the genuinely undecided legal
content, rather than guess.

**Backend implementation, exactly per the ruling:** migration `0075`
(zero `INSERT`s — no seed jurisdiction, no example policy value);
`internal/jurisdiction/operation_purpose.go` (`RequiredPurposes`);
`evaluation_policy.go` (`ResolveEvaluationPolicy`, the read seam —
reuses the existing `assertTenantScope`, never returns a permissive
default); `evaluation_policy_admin.go` (`CreateEvaluationPolicyVersion`,
the write seam — no `ON CONFLICT DO UPDATE` anywhere, refuses `status =
'active'` outright); one new permission
(`PermJurisdictionEvaluationPolicyWrite`, `RolePlatformAdmin`-only, no
HTTP route). `internal/jurisdiction/resolver.go`/`precedence.go`/
`types.go`: zero diff, confirmed by `git diff --stat`. `purpose.go`:
doc-comment-only diff.

**Independent review — five specialists in parallel:** `architect`
fidelity review, `security` (also covering DB/RLS), `identity-compliance`
(compliance/privacy), `qa` (adversarial), `risk` (cross-domain
integration). Three of the five — `architect`, `security`, `qa` — each
independently and empirically (the defect was invisible from reading
alone) found the same P1: the integration test proving the config write
path's optimistic-concurrency control did not reliably force the race it
claimed to test, failing on a meaningful fraction of repeat runs.
`architect` also found three further P2s (a destructive/non-round-
trippable down-migration; a real cross-tenant read gap in
`ListEvaluationPolicyVersions`, which had no scope assertion at all
despite the table's permissive read RLS policy; two overstated claims in
the design record about what the code actually does). `identity-
compliance` found no violations. `risk` found no integration concerns
(and corrected an overstated premise in the orchestrator's own
reconnaissance about Risk's coupling to `OperationClass`).

**Fix round one** closed the P2s and attempted to close the P1 with a
`sync.WaitGroup` synchronization barrier — this closed the specific
failure the three reviewers had reproduced. **A dedicated `security`
re-verification pass** (deliberately scoped narrowly to the fix round's
own changes, not a full re-review) then reproduced the SAME class of
failure under artificial CPU contention (9 failures in 200 runs) and
diagnosed why: the barrier synchronized only "both transactions have
begun," not the actual write race, so under scheduling pressure one
transaction could still fully commit before the other even started.
This re-verification pass also caught that the fix round's own governance-
doc rewrite had, in the course of correcting an earlier self-certification
problem, introduced a NEW inaccuracy — asserting the P1 was closed when it
was not.

**Fix round two** replaced scheduling-dependent assertions entirely,
using exactly the technique `security`'s own report proposed: a loosened
invariant-only regression test asserting only what holds under every
legitimate interleaving, plus a new, genuinely deterministic test that
forces the race via real PostgreSQL unique-index locking semantics — an
uncommitted competing row blocks the real write, confirmed via a
`pg_stat_activity` poll (never a sleep, zero timing assumption) before
the blocking transaction is released. The Orchestrator independently
re-ran this fix directly (not merely trusting the report): 30 consecutive
passes under `-race` across two repeat batches, plus a clean whole-repo
`go test -tags=integration ./...` run. One pre-existing, unrelated test
outside this phase's own files
(`internal/bonus/wave3_phase2_migrations_integration_test.go`'s full-chain
round-trip test) required updating its hardcoded migration-count window by
one entry — the identical, already-established pattern each of migrations
0071-0074 required in turn when landing on the chain's tip, not a Phase D
defect — fixed and re-verified passing.

**Validation, run to completion:** `go build ./...` clean; `gofmt -l .`
clean; `go vet ./...` and `go vet -tags=integration ./...` clean;
`internal/jurisdiction`/`internal/auth` unit and integration suites green;
`go test -race` clean; whole-repo `go test ./...` and
`go test -tags=integration ./...` green (every package, no regressions).

**Documentation/governance updated this phase:** new §15 in
`docs/governance/stage-4i-canonical-model.md`, written to record the
concurrency-test defect's full, honest lifecycle (found → first fix →
re-found under load → second fix → independently re-verified) rather than
a premature certification; §3.4/§6.1 amendment notes; §14.6's PC-GAP
table gained a "status after Phase D" note per item; new ADR
`docs/decisions/0043-jurisdiction-evaluation-policy-configuration.md`; new
HDR register `docs/decisions/0044-human-decision-register-stage-4i-phase-d.md`
(HDR-J-7/8/9, all genuinely open, worded as neutral questions per
`identity-compliance`'s review); `docs/governance/task-registry.md`'s new
"Stage 4I Phase D" section (full findings ledger); this entry.

Per the directive's own mandatory stop condition: the Orchestrator
**stops** here. Any phase beyond this one, HDR-J-7/HDR-J-8/HDR-J-9's
content, and any production jurisdiction activation remain unauthorized
pending a separate human directive reviewing this Phase D completion
report.

## Stage 4I Phase E: operating market and country policy foundation — IMPLEMENTED (mechanism only)

**Status: IMPLEMENTED as a MECHANISM ONLY**, built exactly to the
architect design ruling recorded at `docs/decisions/0045-operating-
market-and-country-policy-foundation.md`. Full findings ledger in
`docs/governance/task-registry.md`'s "Stage 4I Phase E" section;
completion report in `docs/active-stage.md`.

**What this phase answers, and why it is a separate package rather than
an addition to `internal/jurisdiction`:** "for a tenant/brand, an
operation (registration/deposit/withdrawal/wagering), optionally a
product, a country, at an explicit `AsOf` — is this platform permitted to
operate, given the licence's ceiling and every narrower policy decision
beneath it?" — a structurally different question from "which regulatory
jurisdiction governs this player/tenant", which stays exclusively
`internal/jurisdiction`'s. The new package, `internal/operatingmarket`,
may import `internal/jurisdiction` for exactly one shared function
(`EvaluateLicenceValidity`, the single technical implementation of "is
this licence currently reliable" — new file `licence_validity.go`, every
other file in `internal/jurisdiction` has ZERO diff) and cannot import
`internal/identity`/`kyc`/`geolocation`/`rg` at all — mechanically
enforced via `go list -deps` (`TestOperatingMarket_ImportGraphInvariant`),
not merely by convention. A caller of `internal/operatingmarket`
structurally cannot reach `PlayerJurisdictionResult`, `EvidenceSet`, or
`Resolution`.

**Schema (migration `0076`):** `jurisdictions.country_code` (nullable
ISO-3166-1 alpha-2 administrative metadata, never auto-assigned, fenced
by a mechanical test so it can never become a country→jurisdiction
resolver); `platform_operations` (a new, extensible OPERATION vocabulary
— `registration`/`deposit`/`withdrawal`/`wagering` — deliberately
disjoint from `jurisdiction.OperationClass`/`asset_operation_eligibility.
operation`/`risk_rules.operation`; `platform_products`, migration 0045,
is reused unchanged for the PRODUCT dimension — product and operation are
two independent dimensions, not one flat, combinatorial enum);
`licence_country_ceilings` (the platform-wide, append-only, effective-
dated ceiling a LICENCE places on permitted countries — now the sole
authoritative source, `licences.permitted_markets` deprecated in place,
never migrated or dropped, per task-registry item `MKT-PM-1`); and
`operating_country_policies` (the tenant/brand/operation-scoped,
append-only narrowing beneath that ceiling, using `asset_authorizations`'
own `scope_kind` discriminator pattern verbatim). Both new policy tables
use the identical close-then-insert-in-one-transaction discipline as
migration 0075's `jurisdiction_precedence_configs` — there is NO
`ON CONFLICT DO UPDATE` anywhere in the package, the exact PHASE-B-ARCH-1
upsert-provenance defect this phase is built to avoid.

**Resolution algorithm:** one internal `resolve()` function implements
all five steps and feeds both `ResolveOperatingCountryPolicy` (discards
the diagnostic chain) and `ExplainOperatingCountryPolicy` (staff-only,
returns it) — never two implementations of the decision. Step 1 (the
ceiling) is evaluated unconditionally on every call, before any lower
row, so a licence contraction takes effect on the very next resolution
with no rewrite of any lower row. Step 2 (tenant) treats absence as
TERMINAL — a licence grants, it does not instruct. Steps 3-4 (brand,
operation) treat absence as INHERIT, with most-specific-first ranking at
the operation rung. The algorithm walks TOP-DOWN and stops at the FIRST
disabled rung (not most-specific-wins), closing a real defect class: the
write-time trigger only checks upward at write time, so a tenant disabled
*after* a brand row was validly enabled would otherwise leave an orphaned
"enabled" brand row that most-specific-wins would incorrectly resolve as
permitted. The result is an eleven-valued, non-forgeable `Result` — no
accessor for any blocking/source/licence provenance (a player-facing
handler cannot leak which scope blocked because the language will not let
it read the field), no `Code()`/`ID()` of any kind (cannot be substituted
for a `jurisdiction.Resolution`), and exactly one derived boolean,
`Permitted()`. `IsRegistrationPermitted` is the one sanctioned anonymous-
path read, `(bool, error)` only, structurally incapable of carrying an
outcome.

**Dual control: DEFINITIVE ruling, not built this phase.**
`asset_change_requests` cannot be safely extended (four independent
structural blockers verified against migrations 0044 AND 0047: no asset
column to key on, RLS makes it unreadable from a tenant-scoped connection,
the platform-principal-only trigger excludes a tenant compliance officer,
and the consume functions are asset-table-specific). `bonus_change_
requests` fails for different domain-mismatch reasons. Fail-closed means
concretely: zero production-reachable enable path exists at all (no HTTP
route, no console surface, no service-identity caller — the only way to
reach either write function is a direct Go call from a test), zero
resolver wiring (an enable written this phase has zero operational
effect), and a structurally required `authorization_reference` on every
enable (CHECK-enforced, both tables) with the disable direction needing
none (an emergency kill-switch must not need a second approver). New task
registry item **MKT-DUAL-1** names the required follow-up and the three
conditions that must all be resolved before any of: an HTTP route reaching
either write path, resolver wiring, or the first real country row.

**Testing — 32 tests/test-groups, the architect ruling's own coverage
floor, not a ceiling:** every test named in ADR 0045 was written and
passes, including the two states unreachable via any sanctioned write
path (`configuration_conflict`, `policy_expired` — constructed with raw
SQL against a temporarily-disabled trigger/CHECK, then restored), the
four licence behavioural cases (ON allows enable; OFF refuses enable with
nothing written and no audit row; a subsequent OFF blocks immediately
with no lower-row rewrite; a subsequent ON does not auto-re-enable a
previously-disabled lower scope), RLS per table including the asymmetric
no-platform-read posture on `operating_country_policies` (a tenant's
operating footprint is commercially sensitive, no platform-admin read
policy exists) and the asymmetric no-DELETE-trigger posture vs.
`licence_country_ceilings` (the former has `ON DELETE CASCADE` and
Postgres runs referential-integrity actions with RLS/triggers on that
path bypassed, so a full deny-DELETE trigger there would block tenant
deletion; the latter has no cascade and gets the full trigger), and
concurrency using ONLY the mandatory deterministic uncommitted-competing-
row-plus-`pg_stat_activity`-poll technique — never a `sync.WaitGroup`
barrier or `time.Sleep`, the exact mistake that cost Phase D two fix
rounds. All pass under `-race`, independently re-run 5 consecutive times
with zero failures.

**Validation, run to completion:** `go build ./...` clean; `gofmt -l .`
clean; `go vet ./...` and `go vet -tags=integration ./...` clean; the
full `internal/operatingmarket` and `internal/jurisdiction` suites green
under `-race`; whole-repo `go test ./...` and
`go test -tags=integration ./...` green across every package (including
`internal/bonus`'s own migration-chain-window test, updated for migration
0076 landing on the chain's tip — the identical, already-established
pattern every prior migration in this stage has required in turn, not a
Phase E defect).

**A real defect found and fixed during implementation, disclosed rather
than silently patched:** the down-migration's own existence-guard
(refusing a rollback while either new policy table holds rows) was
initially ineffective — `db.Pool.MigrateDown` runs over a plain, scopeless
connection, and (unlike migration 0075's deliberately permissive
`jurisdiction_precedence_configs` read policy) both new tables' RLS is
narrower than a scopeless connection can satisfy — `operating_country_
policies` in particular has NO platform-wide read policy at all, by
design. The guard's own `EXISTS` checks would therefore always see zero
rows regardless of real content. Fixed by temporarily disabling RLS on
both tables inside the SAME transaction as the checks — self-contained,
since a real finding's `RAISE EXCEPTION` rolls back the `ALTER TABLE` too,
and a clean pass proceeds to drop both tables outright regardless. Caught
by the down-migration test genuinely failing against a real database on
the first implementation attempt, not by code review.

**Documentation/governance updated this phase:** new ADR
`docs/decisions/0045-operating-market-and-country-policy-foundation.md`;
`docs/governance/task-registry.md`'s new "Stage 4I Phase E" section
(MKT-DUAL-1/MKT-PM-1/MKT-EXPIRY-1, SEC-4I-F10 re-scoped not closed,
PHASE-B-ARCH-1 re-evaluation — none of its three trigger conditions
fired); `docs/governance/stage-4i-canonical-model.md` §6.1's amendment
note extended to name the four new permissions;
`docs/architecture/15-jurisdiction-and-licensing-model.md` gained a new
section on the operating-market model and its separation from player
jurisdiction; `docs/active-stage.md`; this entry.

Per the directive's own mandatory stop condition: the Orchestrator
**stops** here. Any phase beyond this one, MKT-DUAL-1/MKT-PM-1/
MKT-EXPIRY-1, any HTTP route or resolver wiring, any country/market
content, and any answer to HDR-M-1/HDR-M-2/HDR-J-6/HDR-J-7/HDR-J-8/HDR-J-9
remain unauthorized pending a separate human directive reviewing this
Phase E completion report.

### Stage 4I Phase E fix round (post-independent-review) — RESOLVED

Six independent reviews (architect fidelity, security, DB/RLS,
compliance/privacy, QA/adversarial, code review) ran against the Phase E
implementation above. One BLOCKING defect (a genuine internal
contradiction in the operation-rung resolution algorithm, found
independently by three reviewers using three different techniques) plus
two confirmed pure bugs and a set of P2/P3 findings were raised; all are
now resolved, recorded in full at `docs/decisions/0045-operating-market-
and-country-policy-foundation.md` §16 (§3.5-A AMENDMENT-1) and
`docs/governance/task-registry.md`'s `MKT-NARROW-1` (RESOLVED) and
`MKT-SCOPE-1` (new deferred item, owned by `security`) entries.

**THE BLOCKING FIX — MKT-NARROW-1.** A more-specific operation/product
`enabled` row could unmask a broader, in-force, active `disabled` row once
the narrower row's own disable was withdrawn - reachable using ONLY
narrowing writes, falsifying the resolution algorithm's own central
correctness claim. Architect-ruled Option C amendment, implemented
exactly: (1) migration 0076 (amended in place - it was uncommitted and
unreleased, so no new migration file was created; still the only
migration file for this phase) gained a write-time step in
`operating_country_policies_enforce_ceiling()` refusing to enable a
more-specific operation/product row while a broader in-force active
disabled row for the same operation exists; (2)
`internal/operatingmarket/resolve.go`'s STEP 4 was rewritten to query the
FULL operation-rung candidate set (up to 4 rows, `LIMIT` removed) and
evaluate it as a SET with first-disabled-wins (duplicate-rank check over
the full set, parse check over the full set, a "live" set that excludes
withdrawn tombstones, the LEAST specific live-disabled row blocks, the
MOST specific live row sources a permit). `PolicyVersion` bumped
`stage-4i-e.v1` -> `stage-4i-e.v2` (zero rows existed in either table, so
zero backfill was needed or performed). `ChainStep` gained `BrandID`/
`ProductCode` fields, populated only on operation-rung steps, and the
diagnostic chain now emits one step per applicable candidate instead of
one.

A genuine placement bug in the trigger fix's own first draft was caught
by testing, not review: the new write-time step was placed textually
after the pre-existing brand-check block's own early `RETURN NEW` for
`brand_id IS NULL`, making it structurally unreachable for every
tenant-wide (non-brand-specific) operation write - the single most common
shape. `TestOperatingCountryPolicy_MoreSpecificEnableUnderBroaderDisableIsRefused`'s
"product-specific under every-product disable" sub-case failed against
that first version, and the trigger was corrected (the new step moved
before that early return) before this round closed. Disclosed here per
this project's own "flag what a fix round's own first draft did not
anticipate" discipline.

**Fix 1 (dead `prior_effective_to` audit field).** Both write paths
(`ceiling_admin.go`, `policy_admin.go`) read the "before" state BEFORE the
close `UPDATE` ran, so `prior_effective_to` was always JSON `null` past
the first version for a key. Fixed by adding `RETURNING effective_to` to
the close `UPDATE` and using that value; switched `Exec`+`RowsAffected()`
to `QueryRow`+`Scan`, mapping `pgx.ErrNoRows` to the existing
`ErrConcurrentPolicyWrite`. New test
`TestOperatingMarketAudit_SecondVersionRecordsPriorEffectiveTo` covers
both writers.

**Fix 2 (`SetJurisdictionCountryCode` missing scope assertion).**
`internal/jurisdiction/registry_admin.go`'s `SetJurisdictionCountryCode`
(the one Phase-E-added function affected) now calls
`assertPlatformScope` as its first statement - `jurisdictions` carries
zero RLS, so this Go-level check is the ONLY control. The identical,
PRE-EXISTING gap in `CreateJurisdiction`/`CreateLicence`/
`AssignTenantLicence`/`ListJurisdictions`/`ListLicences` is deliberately
NOT fixed here (out of this round's scope) and is now tracked as
`MKT-SCOPE-1`, owned by `security`, currently unexploited (no HTTP route/
console/service-identity caller reaches any of them yet) but required
before any does. New test
`TestSetJurisdictionCountryCode_RequiresPlatformScope`.

**Fix 3 (vacuous migration test).**
`TestMigration0076_LeavesExistingJurisdictionCountryCodesNull` only ever
created jurisdiction rows AFTER migration 0076 had already run, so it
never exercised the "no auto-assignment to PRE-EXISTING rows" property
its own name claimed to guard. Rewritten to insert a row via raw SQL on a
scratch database BEFORE running 0076, then assert that row's
`country_code` is still `NULL` after 0076 runs, plus an independent
static-source grep confirming 0076's `.up.sql` contains no `UPDATE
jurisdictions` statement.

**P3 items fixed:** the `TestJurisdictionCountryCode_IsNotAJurisdictionResolver`
guarded-file list now also covers `evaluation_policy.go`/
`evaluation_policy_admin.go`; `Explanation.LicenceValidity` is now the
typed `jurisdiction.LicenceValidity` enum, not a bare `string`;
`ExplainOperatingCountryPolicy`'s internal licence-status read no longer
silently swallows its own query error; the RLS posture test now asserts
policy predicate text (not just names/commands) and
`licence_country_ceilings` gained its own player-scoped negative-probe
test (previously only `operating_country_policies` had one); a new
`TestOperatingCountryPolicies_TenantDeleteCascadeStillWorks` guards the
deliberately-asymmetric append-only trigger's own documented risk (a
"well-meaning symmetry fix" silently breaking tenant-delete cascade).

**P3 items deliberately NOT fixed, with disposition:** the two
declared-but-never-returned error sentinels `ErrNotFound`/
`ErrLicenceNotDeterminable` (`internal/operatingmarket/types.go`) were
left in place with an explicit "RESERVED" doc comment explaining why each
is not yet wired to a return path, rather than force-wiring them to an
arbitrary call site or removing a currently-inert exported sentinel a
future caller could already be depending on having reserved - a judgment
call disclosed here rather than silently made.

**Validation gate re-run in full:** `go build ./...`, `go vet ./...`, `go
vet -tags=integration ./...`, `gofmt -l .` (all clean); the focused
`internal/operatingmarket` and `internal/jurisdiction` packages, plus 10
consecutive `-race` runs of the concurrency and new regression tests and
10 consecutive full-package runs (no flakes); the full whole-repo `go test
-tags=integration ./... -count=1` (every package green). `git diff
--stat` confirmed no RLS policy file, no permission role-wiring change,
and no diff to `internal/jurisdiction/resolver.go`/`precedence.go`/
`types.go`/`evaluation_policy.go`/`evaluation_policy_admin.go`/
`resolution_active.go`/`evidence_collection_active.go` - migration 0076
remains the only migration file for this phase.

Files touched this round: `migrations/0076_operating_market_country_
policy.up.sql` (amended in place); `internal/operatingmarket/resolve.go`,
`explain.go`, `types.go`, `ceiling_admin.go`, `policy_admin.go`, and new
test file `amendment_3_5a_integration_test.go` plus additions to
`rls_integration_test.go`/`registration_explain_audit_integration_test.go`/
`resolve_integration_test.go`; `internal/jurisdiction/registry_admin.go`,
`country_code_test.go`, `country_code_integration_test.go`;
`internal/auth/permission.go` (two doc-comment corrections only - no
permission/role-wiring change); `docs/decisions/0045-operating-market-
and-country-policy-foundation.md`; `docs/architecture/15-jurisdiction-
and-licensing-model.md`; `docs/governance/task-registry.md`;
`docs/active-stage.md`; this entry.

Per the directive's own mandatory stop condition: the Orchestrator
**stops** here, unchanged from before this fix round. Nothing beyond
independent re-verification of this fix round is authorized.

### Stage 4I Phase E — second and third fix rounds (ADR 0045 §3.5-A AMENDMENT-2, AMENDMENT-3) plus F2/F4

Two further, independently-found defects surfaced in this same rung
model during adversarial re-verification of the fix round above, each
resolved by a further architect-ruled amendment. Full detail in
`docs/decisions/0045-operating-market-and-country-policy-
foundation.md` §17 (AMENDMENT-2), §18 (AMENDMENT-3), `docs/active-
stage.md`'s "THIRD FIX ROUND" entry, and `docs/governance/task-
registry.md`'s `SEC-E-REV-1`/`SEC-E-REV-2`/`MKT-MIG76-1` entries.

**AMENDMENT-2 (finding SEC-E-REV-1):** withdrawing an active `disabled`
row at the BRAND/OPERATION rung (where absence inherits from the rung
above) was treated as unconditionally fail-closed-safe, but is actually
a widening act — reachable with no authorization and no audit
distinguishability. Fixed with a new CHECK constraint,
`ocp_inherit_rung_withdrawal_requires_authorization`, requiring a
non-blank `authorization_reference` on any such withdrawal; a mirroring
Go-side guard; two new audit metadata keys (`rung_block_transition`,
`widening_capable`) so a widening-via-withdrawal event is findable by
the correct filter (`widening_capable=true`, never `state='enabled'`
alone). The tenant rung and `licence_country_ceilings` are deliberately
excluded (their absence is terminal/fail-closed, never inherited).

**AMENDMENT-3 (finding SEC-E-REV-2, "the bare close"):** a third
independently-found variant of the same defect family — closing an
open `active`+`disabled` brand/operation-rung row WITHOUT writing any
successor removes the block exactly like a withdrawal, but every prior
control was INSERT-shaped, so this ordinary `UPDATE` bypassed all of
them. Fixed with a new `DEFERRABLE INITIALLY DEFERRED` constraint
trigger, `ocp_inherit_rung_close_requires_successor`, requiring that any
such close be followed, in the same transaction, by an open successor at
the same key — sufficient because every legal successor shape is either
non-widening or already gated by an existing CHECK. No `resolve.go` diff,
no `PolicyVersion` bump (this amendment constrains what's writable, not
what `resolve()` computes for a fixed row set). The architect gave a
structural argument for why this is the last variant reachable via
ordinary DML from this row-removal shape and explicitly recommended
against a fourth sweep of the same kind, naming three different targets
(`AsOf` provenance, `tenants`/`licences` RLS, `platform_operations`/
`platform_products` write governance) for any future, differently-scoped
review instead.

**F2 (disclosed, not fixed):** `DELETE` on `operating_country_policies`
remains uncontrolled for an RLS-bypassing role (by design, to preserve
`tenants ON DELETE CASCADE`) — documented as a residual pointing to ADR
0026's existing migration-owner/runtime-role separation item.

**F3 (out of scope, tracked not fixed):** `tenants`/`licences` carry no
RLS at all — pre-existing, but Phase E newly makes the licence the root
of the operating-market ceiling, so this is now a live path to widening
an operating-market answer. Amended into the existing `MKT-SCOPE-1` item
(not a new one) with a second trigger condition, `MKT-SCOPE-1(b)`. **A
genuine CLAUDE.md multi-tenancy-isolation concern, surfaced to the human
directly rather than left as only a registry line.**

**F4 (fixed):** `EvaluateLicenceValidity` never checked `licences.
issued_at` — a fail-open in the ceiling's own root predicate. Fixed with
a half-open `[issued_at, expires_at)` interval check. `PolicyVersion`
bumped `"stage-4i-e.v2"` → `"stage-4i-e.v3"` — attributable to F4 alone.

Each amendment went through the same discipline as the first: an
architect ruling precise enough for zero-ambiguity implementation,
backend implementation with the validation gate run to completion,
and independent re-verification (architect fidelity and/or security and
QA, depending on the round) before being accepted — including one
polish round closing a code-review/security finding that the
stale-schema regression guard (`TestMigration0076_
SchemaMatchesTheCurrentMigrationFile`) had itself not been extended to
check for AMENDMENT-3's own markers.

Final state: 78 top-level test functions in `internal/operatingmarket`
plus 9 in `internal/jurisdiction`'s `licence_validity`/`country_code`
test files. Full validation gate re-run clean multiple times against
independently-built fresh scratch databases (schema markers for all
three amendments confirmed directly against `pg_constraint`/
`pg_trigger`/`pg_proc` before trusting any result — this exact staleness
hazard, tracked as `MKT-MIG76-1`, was independently rediscovered by
several reviewers across this workstream and is now the single most
repeated lesson of this stage): `go build`/`go vet` (both tags)/
`gofmt -l`, the full package suite, `-race` runs of every
concurrency-sensitive and new regression test, and the whole-repo
`go test -tags=integration ./... -count=1` gate — all green, including
one final independent run performed directly by the Orchestrator (not
only trusted from agent self-reports) on a scratch database it built,
migrated, and schema-verified itself.

Per the directive's own mandatory stop condition: the Orchestrator
**stops** here. Nothing beyond independent re-verification of this
fix round is authorized — no HTTP route, no resolver wiring, no
production country content, no answer to HDR-M-1/HDR-M-2/HDR-J-6/
HDR-J-7/HDR-J-8/HDR-J-9, and no `MKT-DUAL-1`/`MKT-SCOPE-1`/`MKT-EXPIRY-1`/
`MKT-PM-1` resolution.

## Stage 4I Phase E-SECURITY: tenant/licence/jurisdiction registry RLS hardening — IMPLEMENTED

**Status: IMPLEMENTED**, closing task-registry items `MKT-SCOPE-1` and
`MKT-SCOPE-1(b)` per a binding architect ruling recorded in full at
`docs/decisions/0046-tenant-licence-registry-rls.md`. Completion report
in `docs/active-stage.md`'s "Stage 4I Phase E-SECURITY" entry.

**The defect.** `tenants`, `licences`, and `jurisdictions` had never had
row-level security applied, in any migration, since the platform's
earliest schema. The architect live-reproduced the full attack chain this
gap enabled: an ordinary tenant-scoped connection repointing its own
`tenants.licence_id` at another tenant's BYOL licence, after which
`internal/operatingmarket`'s ceiling check (itself not defective) read the
forged licence and resolved `permitted` for a country the tenant's real
licence never granted. Two further attacks, live-reproduced and not
previously recorded anywhere: (1) the composite FK
`tenants_licence_matches_model` can be defeated by changing
`licensing_model` and `licence_id` together in ONE UPDATE statement,
because `expected_licensee` is a `GENERATED` column recomputed from the
NEW `licensing_model` in the same statement; (2) an ordinary tenant-scoped
(or even scopeless) `DELETE FROM tenants` targeting a DIFFERENT tenant
cascades away that tenant's entire `operating_country_policies` set — a
materially worse, lower-bar version of ADR 0045 §18 finding F2, whose own
framing wrongly assumed the removal path required a role that bypasses
RLS entirely.

**The fix (migration `0077_tenant_licence_registry_rls`).** Row-level
security on all three tables, asymmetric by table (`tenants`/
`jurisdictions` read-open — several scopeless/cross-tenant readers
legitimately need it: staff-login slug lookup, three active-tenant sweeps
in `internal/rg`/`internal/reconciliation`/`internal/bonus`, and migration
0076's own ceiling checks; `licences` narrowed to platform-admin or the
tenant whose own `tenants.licence_id` names the row) but uniform on
writes (every INSERT/UPDATE on all three, and the one DELETE policy on
`tenants`, require a genuinely platform-admin-scoped transaction — no
tenant-scoped write policy of any kind on any of the three tables). A new
partial unique index, `uq_tenants_exclusive_own_licence`, independently
closes a live-verified BYOL exclusivity gap (two tenants able to bind the
same `licensee='tenant'` licence), keyed on the `GENERATED`
`expected_licensee` column so it cannot drift from `licensing_model`, and
deliberately not constraining shared `licensee='platform'` licences (ADR
0006).

**Go changes.** `internal/identity.CreateTenant` gained an
`assertPlatformScope` first statement (new sentinel
`ErrPlatformTransactionScope`); `internal/jurisdiction.
AssignTenantLicence`'s contract moved from `db.Pool.WithTenant` to
`db.Pool.WithPlatformAdmin`, its audit write moving from tenant-scoped to
platform-scoped as a direct consequence; `CreateJurisdiction`/
`CreateLicence`/`ListJurisdictions`/`ListLicences` each gained the
identical `assertPlatformScope` gate `SetJurisdictionCountryCode` already
had; `internal/httpserver.newCreateTenantHandler` moved from
`WithoutTenant` to `WithPlatformAdmin`, gained a required `reason_code`
field and a before/after audit shape (closing the pre-existing
`tenant.created` audit gap), and now surfaces a `uuid.Parse` failure
explicitly instead of silently swallowing it.
**`internal/operatingmarket` received ZERO executable diff** — one
comment-only correction in `resolve.go` explaining that its own
`assertTenantScope` remains load-bearing (read-side isolation on
`tenants` stays deliberately open) even though the write-side forgery
path is now closed. `internal/operatingmarket`'s own schema, triggers,
RLS policies, and resolution algorithm are unchanged — confirmed via
`git diff`.

**Task dispositions.** Fixed now: BYOL licence exclusivity. Deferred, each
recorded as its own new task-registry item: `PLAT-ROLESPLIT-1`
(migration-owner/runtime-role split — genuinely blocked on infrastructure
this repository cannot provide, `CREATEROLE` unavailable to the
application role, verified live; owner `security`; pre-production gate),
`MKT-LICSTATUS-1` (no sanctioned write path for `licences.status`; owner
`architect`; gated on the first real `licence_country_ceilings` row),
`MKT-AUDIT-1` (tenant-visible licence-assignment evidence; owner
`security`; not yet needed). `MKT-DUAL-1` unchanged. Not touched:
`EvaluateLicenceValidity` (already correctly fail-closed).

**Mechanical fixture migration.** Every existing integration test fixture
across roughly 30 files that seeded `tenants`/`licences`/`jurisdictions`
under a scopeless connection was moved to `db.Pool.WithPlatformAdmin`,
with rows-affected checked explicitly on every touched write — a denied
RLS write is a silent zero-row no-op, not an error, the exact failure
mode the architect's own reproduction found in
`TestResolveOperatingCountryPolicy_NotYetIssuedLicenceYieldsNotPermittedByLicence`'s
own `UPDATE licences` fixture. Migration-mechanics tests that assumed
migration 0076 was "the chain's tip" (`internal/jurisdiction/
migration_0075_integration_test.go`, `internal/operatingmarket/
migration_0076_integration_test.go`, `internal/operatingmarket/
qa_migration_rls_survives_failed_rollback_test.go`,
`internal/bonus/wave3_phase2_migrations_integration_test.go`) were updated
to account for migration 0077 landing on top of it. One additional
fixture gap (`internal/identityresolution/register_integration_test.go`)
was found only by actually running the whole-repo suite, not by static
`INSERT INTO tenants` text search — a `identity.CreateTenant` call under
`WithoutTenant` that the initial mechanical sweep's search pattern missed.

**New tests.** `internal/jurisdiction/migration_0077_integration_test.go`
(4 tests) and `internal/jurisdiction/registry_rls_integration_test.go`
(18 tests, including the crux regression
`TestTenantsRLS_TenantScopedConnectionCannotUpdateOwnLicenceID` and the
exact composite-FK-defeating shape
`TestTenantsRLS_TenantScopedConnectionCannotDefeatCompositeFKByChangingLicensingModel`),
plus three new tests in `internal/operatingmarket/rls_integration_test.go`
reproducing the full end-to-end attack and proving it is now refused.
`internal/jurisdiction/tenant_licence_admin_integration_test.go` and
`internal/jurisdiction/licence_validity_integration_test.go` substantially
updated for the new contract, each with one new regression test
(`TestAssignTenantLicence_TenantScopedTransactionIsRejected`,
`TestEvaluateLicenceValidity_ForeignLicenceIsInvisibleAndFailsClosedNotOpen`).
`internal/identity/identity_integration_test.go` and
`internal/httpserver/identity_flow_integration_test.go` each gained a
`TestCreateTenant*_NonPlatformScopedTransactionIsRejected`/
`TestCreateTenantAPI_AuditRecordsReasonCodeAndAfterState` pair.

**Validation.** Full gate run against a FRESH scratch database built via
`cmd/migrate up` from the current `migrations/` directory (never a reused
local database, per `MKT-MIG76-1`'s own repeated lesson): `go build`,
`go vet` (both tags), `gofmt -l` all clean; RLS state verified directly
against `pg_class`/`pg_policies`/`pg_indexes` (migration 0077's exact ten
policies and one partial unique index all present and correctly shaped)
before trusting any test result; `internal/jurisdiction` (154 tests),
`internal/identity` (28), `internal/operatingmarket` (80), and
`internal/httpserver` (173) all pass; 10 consecutive `-race` runs each of
`TestAssignTenantLicence_ConcurrentAssignmentsSerializeCleanly`,
`TestOperatingCountryPolicy_ConcurrentCloseCannotBeRescuedByAnotherTransactionsSuccessor`,
and `TestOperatingCountryPolicy_ConcurrentCreatesNeverCorruptState` all
pass; the FULL whole-repo `go test -tags=integration ./... -count=1`
passes with zero failures.

**Documentation.** New ADR `0046`; `docs/decisions/0045-*.md` §18
finding F2/F3 corrected (F3's "NOT AUTHORIZED THIS DISPATCH" disposition
DISCHARGED); `docs/decisions/0026-*.md` amended to redirect its own
one-sentence migration-owner/runtime-role deferral to `PLAT-ROLESPLIT-1`;
`docs/governance/stage-4i-canonical-model.md` §6.1 and its §13.3
resolver-scope narrative corrected; `docs/architecture/15-jurisdiction-
and-licensing-model.md` corrected (the composite FK's actual, narrower
coverage). `docs/governance/task-registry.md`'s `MKT-SCOPE-1`/
`MKT-SCOPE-1(b)` marked RESOLVED with the two previously-unrecorded
attacks recorded in full.

### Stage 4I Phase E-SECURITY — fix round (post six-way independent review)

Six independent reviews (architect fidelity, adversarial security,
DB/RLS, compliance/privacy, QA regression, code review) ran against the
implementation above. No P0/P1, but strong convergence on several real
findings — full detail in `docs/active-stage.md`'s "Stage 4I Phase
E-SECURITY" entry's own "FIX ROUND" subsection. Summary: the new
`uq_tenants_exclusive_own_licence` violation's unmapped SQLSTATE 23505
(surfacing as an HTTP 500 instead of a diagnosable 400) was found by four
reviewers independently and is now mapped; a dead `jurisdictions` DELETE
test-fixture cleanup (always a silent zero-row no-op under the new
no-DELETE-policy design, with a comment incorrectly asserting it worked)
was found by two and is now removed; `tenants_read`'s read posture being
broader than its own stated justification (open to player scope, unlike
every sibling policy) was found by three and is now narrowed to exclude
player scope specifically (full tenant-to-tenant enumeration deliberately
left open, tracked as new item `PLAT-TENANTREAD-1`); `docs/security/
security-architecture.md` — never updated despite ADR 0046 claiming
architecture docs were corrected — was found to still assert the exact
opposite of the current code and has been corrected; missing deny-
TRUNCATE triggers were added to all three tables (RLS does not govern
TRUNCATE at all, and the prior "protection" was an accident of the FK
graph); a concurrency test that used a bare `sync.WaitGroup` with no real
synchronization barrier — a direct violation of this codebase's own
binding rule, and independently found to flake under CPU contention due
to a `created_at`-based ordering assertion — was rewritten using the
established deterministic technique, with a new companion test covering
the additional race the exclusivity index itself introduces. One item was
explicitly escalated to the human rather than resolved: since the
application's runtime database role also owns every table (no migration-
owner/runtime-role split — `PLAT-ROLESPLIT-1`, genuinely blocked on
missing `CREATEROLE`), a tenant-scoped connection able to issue DDL can
disable RLS entirely; the adversarial reviewer demonstrated this as a
full-platform-wipe and explicitly flagged it launch-blocking rather than
a routine deferral. Full validation gate re-run clean multiple times
against independently-built fresh scratch databases, including one final
run performed directly by the orchestrator (not only trusted from agent
self-report).

No automatic progression. Per the stage-gate rule, the next phase requires
its own separate human authorization.

## Stage 4I Exit Triage — Exit Register and Production Integration Readiness

Following the Phase E-SECURITY completion report's acceptance, the human
issued a directive explicitly changing execution strategy: stop opening
further jurisdiction/KYC/security architecture review stages and instead
run a final Stage 4I exit/triage pass, closing every open item with a
concrete disposition and naming the next concrete implementation stage.
Player-jurisdiction, licensing, operating-market, KYC, bonus, sportsbook,
wallet/ledger, payments, and RG architecture were all explicitly declared
FROZEN — this stage reopened none of them; no concrete implementation
dependency was found that required it.

This stage produced two new documents and zero Go code / migrations:

**`docs/governance/stage-4i-exit-register.md`.** Every open Stage 4I item
was classified A (production blocker) / B (next-feature blocker) / C
(human decision, not urgent) / D (deferred, safe) / E (technical debt),
with owner, why it exists, concrete consequence, what actually needs it
fixed, whether it blocks production or the next stage, and the exact
trigger for reopening it. Rather than accept the prior record at face
value, several items were re-verified against their *actual* current
reachability: `PLAT-TENANTREAD-1` was re-checked by grepping every HTTP
handler and every `internal/identity` tenant-lookup function in the
repository — confirmed there is still no `ListTenants`-shaped function or
route anywhere, so the cross-tenant-enumeration exposure remains reachable
only via direct database access (i.e., already inside the
`PLAT-ROLESPLIT-1` trust boundary), not via any implemented product
surface — classified D, deferred safe. `MKT-DORMANT-1` was re-checked by
reading `internal/operatingmarket/ceiling_admin.go` directly: every
ceiling-version write, contraction or re-expansion alike, writes a
platform-scoped, `widening_capable`-tagged `audit_log` entry in the same
transaction, so the only path to a dormant tenant-rung policy "resuming"
is a platform-admin's own fully audited act at the ceiling rung — also
classified D, deferred safe, with the residual UX question (should
re-expansion also force a fresh tenant-rung affirmation?) left for
`architect` to decide only if/when real ceiling content exists.
`MKT-DUAL-1` was re-confirmed (again) to have zero HTTP/production reach.
Net result: **exactly one production blocker** (`PLAT-ROLESPLIT-1`), and
**nothing on the list blocks the recommended next stage**. The document
also states a fail-closed "integration contract" table — which future
call sites (registration, deposit, withdrawal, wagering, bonus issuance/
conversion, catalogue availability) will eventually need player-
jurisdiction/licence-validity/operating-country-policy checks, and what
each needs as input — purely informational, zero wiring performed, and
explicitly noting every one of the three underlying mechanisms already
fails closed on any unresolved result by construction.

**`docs/security/runtime-role-separation.md`.** The directive named
`PLAT-ROLESPLIT-1` (the migration-owner/runtime-role split, previously
escalated by the Phase E-SECURITY adversarial security reviewer as
launch-blocking) the critical item requiring a precise, implementation-
ready design rather than another deferral. The root cause was determined
precisely: the application's Postgres role (`igaming`) owns the database,
schema, and every table it migrated, and PostgreSQL row-level security
**never applies to a table's owner**, regardless of `FORCE ROW LEVEL
SECURITY` — the load-bearing fact is ownership, not the `rolcreaterole`
attribute (`igaming` lacking `CREATEROLE` is why it cannot self-provision
a new role, not why the new role would be safe). This was not left as
theory: a temporary, genuinely non-owning role was created against this
session's local development Postgres (not production) with only
`SELECT`/`INSERT`/`UPDATE`/`DELETE` grants, and every capability question
the directive posed was answered by direct reproduction rather than
inference — connected as that role and confirmed it cannot `ALTER TABLE
... DISABLE ROW LEVEL SECURITY`, cannot `DISABLE TRIGGER`, cannot
`TRUNCATE`, cannot `DROP TABLE`, cannot `ALTER TABLE ... ADD COLUMN`, and
cannot `CREATE TABLE` — while an ordinary `SELECT` still succeeds. This is
the exact escalation chain the Phase E-SECURITY adversarial security
reviewer used against the current single-role setup, now shown refused.
The document specifies the exact required migration-owner role (keep
`igaming` exactly as-is, restricted to the deploy/migration path only),
the exact required runtime role and its exact grant list, the exact
privileges that must never appear on it, and a ready-to-run four-statement
provisioning script for an operator holding `CREATEROLE` to execute once
per environment. **This is classified `PRODUCTION BLOCKER — EXTERNAL
INFRASTRUCTURE ACTION`** — the fix cannot be applied by this repository or
this session, because it requires a production database credential this
session does not have and, per CLAUDE.md's Environment Safety rule, must
not request. No Go code change is required for this fix (`internal/
db.Pool` makes no assumption about the connecting role's ownership); it is
deliberately not encoded as a migration file, since a migration executed
by `igaming` itself cannot be the thing that stops `igaming` being the
runtime identity.

**Independent review.** Per this stage's own efficiency rule (minimum
reviews for routine/documentation work, not a six-way dispatch), three
specialists reviewed both documents in parallel: `security` independently
reproduced the role-separation verification itself rather than trusting
the write-up, and reviewed the security-relevant classifications;
`architect` cross-checked every classification against the full
task-registry history and independently verified the `ceiling_admin.go`
audit-write claim and the "zero production wiring" claims, and evaluated
the Back-Office next-stage recommendation's dependency-readiness claim
against the actual repository state; `qa` re-ran the validation gate,
independently reproduced the role-separation verification a second time,
and spot-checked the `PLAT-TENANTREAD-1` reachability claim. The
architect review landed first: it independently re-verified every
load-bearing claim by reading the code directly and found six real
inaccuracies in the first draft, all fixed in place — the exit register
had silently narrowed `MKT-DUAL-1`'s own three-trigger scope down to one
(restored, matching this registry's existing record); `MKT-EXPIRY-1`'s
`issued_at` schema/write-surface sub-question had been dropped (restored);
the Back-Office next-stage recommendation's dependency-readiness claim
was verified line-by-line and found overstated for every named capability
except withdrawal approval (corrected here and in `active-stage.md`);
`PLAT-ROLESPLIT-1` and `MKT-LICSTATUS-1` needed the same explicit
conditional "blocks next stage" flag `MKT-AUDIT-1` already carried
(added); the `MKT-DORMANT-1` "only path to resumption" claim was
narrower than stated, since `resolve()` also gates on licence validity
(corrected, and cross-linked to `MKT-LICSTATUS-1`); and the runbook's
grant script over-granted write access to `schema_migrations` (fixed
directly in the script). Full detail in `docs/governance/task-
registry.md`'s "Stage 4I Exit Triage" section.
<!-- ORCHESTRATOR: append security/qa findings once they land. -->

**Next stage recommendation.** Operator Back-Office MVP. Selected on
dependency readiness, not subjective importance: it requires answering
zero open Human Decision Register items and zero jurisdiction/licensing/
operating-market wiring, and the repository verifiably has no frontend or
back-office code of any kind today. **Corrected per the independent
architect review**, which verified the claimed dependency readiness by
reading the actual handlers rather than accepting the "already-tested
backend, UI-only" framing this stage's first draft used: withdrawal
approval is genuinely ready to consume as-is, but KYC case-queue,
RG-admin, bonus-campaign-admin, tenant/brand listing, and player
management each need new tenant-wide list/query endpoints (today's
handlers require an already-known player-account ID, or are write-only
with no list/approval-queue route at all), the player list is hardcoded
to `LIMIT 50` with no pagination and no reinstate-after-suspend endpoint,
platform-scoped audit rows (where migration 0077 now places licence-
assignment and operating-market audit entries) are unreachable through
the one existing audit-read route because it filters to a single tenant,
and no pagination convention exists anywhere in the API today. The stage
is therefore correctly scoped as "back-office read/query API surface +
UI," not "UI over finished APIs" — the dependency-readiness verdict
(no policy blocker, no frozen architecture to reopen) still holds, but
the amount of new backend surface it requires does not. This option was
already named ("Stage 6A") as a candidate in prior stages' own "decisions
needed from the human" sections before this triage confirmed it as the
dependency-ready choice. **Not authorized to start; no code was written
for it this stage.**

**Security review** landed last and found the review round's most
consequential result: this session's own long-lived local development
database had drifted from the committed migration files — its live
`tenants_read` policy was the stale, pre-Phase-E-SECURITY-fix-round
`USING (true)` text with no player-scope exclusion, and ten
`internal/operatingmarket` tests were failing, despite `schema_migrations`
showing migration 0077 applied and the current committed file already
containing the fix. The orchestrator root-caused this directly: the local
database had migration 0077 applied at an earlier point in this session's
history, before that file's later in-place amendments landed — the exact
`MKT-MIG76-1` hazard this project already documents, now recurring
against the orchestrator's own working database rather than a reviewer's.
**Not a defect in the committed code.** Fixed by dropping and rebuilding
the database fresh from HEAD; the previously red tests and the full
30-package integration suite are all green against the rebuilt database.
A first-time deployment is not exposed to this specific drift, but the
underlying tooling gap it exposed — `cmd/migrate` has no live-schema-vs-
file-content verification — is real and generalizable, and is now tracked
as new item `PLAT-MIGDRIFT-1` (classified E, observation/technical debt,
not a blocker). The security review otherwise reproduced every
`runtime-role-separation.md` claim with 20 additional escalation probes
(all denied) and confirmed zero `SECURITY DEFINER` functions exist
anywhere in the database; it found no escalation path out of the proposed
design and approved it as the `PLAT-ROLESPLIT-1` fix. Several smaller
corrections were folded into both documents: `casino_games` (RLS
disabled, global catalogue data) named as an explicit exception to the
"ordinary DML is genuinely enforced by RLS" claim; the exit register's
"exactly one production blocker" line qualified to state it describes the
committed code, not the live state of every already-running schema
instance; the three actual scopeless multi-row `tenants` readers named
explicitly instead of citing "migration 0077's rationale" vaguely; and a
note that CI runs the integration suite as the migration-owner role only,
so the runtime-role split (once rolled out) would be verified once by
hand and not continuously unless a second CI job is added later.

All three minimum reviews (architect, qa, security) are complete and
folded in. No P0 found by any reviewer; the one P1 (the local database
drift) was root-caused and fixed within this same session, not deferred.

No automatic progression. Per the stage-gate rule, this stage's own
recommendation is not an authorization — the next stage requires its own
separate human directive.

## Stage 5 — Operator Back Office MVP

Authorized directly following the Exit Triage's Back Office recommendation
above. A large, single-authorization implementation stage per the
directive's own "one stage = one large product objective" instruction —
no intermediate approval gates, backend and frontend built together as
one coherent product.

**Backend**, four parallel agents on disjoint files (no migrations
needed — every table already existed): `backend` built tenant/brand
list+detail, paginated/searchable player list, a new
`POST /v1/admin/players/{id}/reinstate` (atomic-conditional, cannot clear
self-exclusion/identity-review statuses by design), a new platform-scoped
audit log endpoint (closing the `MKT-AUDIT-1`-adjacent HTTP-unreachability
the Exit Triage identified), and pagination/filters on the existing
tenant audit log. `identity-compliance` built the KYC case queue and
extended the RG restrictions endpoint with a tenant-wide mode — and, in
its own adversarial testing before shipping, found and fixed a genuine
cross-tenant PII leak: a naive tenant-wide RG query relying only on
`player_restrictions`' intentionally-broad RLS policy surfaced every
platform-wide self-exclusion row on the platform to any tenant's staff;
fixed with an `INNER JOIN` to `player_accounts`. `bonus-engine` built
read-only campaign/change-request/grant list endpoints. A second
`backend` instance built the two new withdrawal admin endpoints
(`GET /v1/admin/withdrawals/history`, `GET /v1/admin/withdrawals/{id}`),
pure reads with no state-transition side effect. My own combined
verification after all four merged: clean build/vet/gofmt, full
30-package `-tags=integration` suite green against a freshly built
scratch database.

**Frontend**: a brand-new `backoffice/` application (Vite/React/
TypeScript/TanStack Query/Tailwind) — the repository had zero frontend
code of any kind before this stage. Hard layering (`api/` is the only
HTTP-aware module; `components/` are domain-free primitives; `features/*`
holds domain logic; client-side permission checks are explicitly
non-authoritative) so a future visual redesign never has to touch
business logic. Correctly models the real constraint that
`db.Pool.WithTenant` errors on a nil tenant ID: platform_admin and
tenant-scoped staff are genuinely different navigation experiences, no
fake "view as tenant" capability was invented. All eight scope areas
(shell, tenants, players, KYC, RG, bonus, withdrawals, audit) built and
independently verified by me (`npm run build`, `npm test`: 17 tests, 6
files, both clean).

**Independent review** — architect, security, qa, plus a narrowly-scoped
ledger-finance review of the one page that moves money (withdrawal
approve/reject), per this stage's efficiency rule. Architect: PASS.
QA: full gate green, coverage judgment confirmed every new endpoint has
real authorized/unauthorized/cross-tenant tests. Ledger-finance: SIGN-OFF
on the withdrawal UI as a genuine pass-through to the already-certified
backend, no client-side financial logic found; one real P2 (amounts
rendered in raw minor units with no exponent — fixed: new
`decimal_exponent` field on the two new withdrawal endpoints plus a
shared frontend formatter). Security found the two most consequential
findings of the whole review round, both fixed: (1) "Sign out" never
revoked the session server-side, leaving a stolen refresh token valid for
its full 30-day TTL with no way for the user to stop it — fixed by
calling the existing `POST /v1/auth/logout` on logout; (2) the RG
cross-tenant leak's own regression test was itself vacuous (proved by
mutation testing — weakening the fix's join left the old test passing) —
rewritten to seed a genuinely platform-wide self-exclusion, verify the
fixture directly, and give the comparison tenant its own real data so the
isolation assertion is non-vacuous; both re-verified against a fresh
scratch database after the fix. A related P3 (StrictMode could present
the same refresh token twice during session bootstrap, which the backend
treats as reuse and revokes the whole session chain) was fixed by
routing bootstrap through the same single-flight refresh path `apiFetch`
already uses.

**Deferred, recorded, not fixed**: refresh-token-in-`sessionStorage`
combined with the 30-day TTL is a documented MVP tradeoff that security
flagged as launch-blocking for the Back Office specifically (production
needs httpOnly/Secure/SameSite cookies, a backend change) — tracked
alongside `PLAT-ROLESPLIT-1`, not resolved this stage; the refresh-retry
logic itself has no frontend test coverage; `tenants`/`brands` read
isolation is handler-only with no RLS backstop (correct, but should not
be assumed inherited); the RG tenant-wide queue has no player attribution
(a real functional gap, deferred as a fast-follow); the RG endpoint's two
response shapes (bare array vs. envelope) is the one inconsistency with
the otherwise-uniform Stage 5 pagination convention; withdrawal `amount`
is a JSON number, not yet the string convention `bonus.ts` already uses —
a latent precision hazard only once crypto-asset withdrawals reach this
endpoint.

No production wiring, no B2C frontend, no jurisdiction/operating-market
content, no HDR item answered. `PLAT-ROLESPLIT-1` remains the production
deployment blocker, unaddressed this stage per its own documented
instruction. No automatic progression — Stage 6 (B2C Player/Brand MVP)
is NOT authorized and was not started.

## Stage 6 — B2C Player/Brand MVP + First Sportsbook Vertical Slice

The first real B2C player product and first functioning sportsbook
vertical slice, authorized as one large stage. Dependency discovery
confirmed every non-sportsbook domain this stage needed (identity/auth,
wallet/ledger, RG, risk, KYC, audit, brand/tenant config, Back Office)
already existed and was reused; sportsbook itself had zero pre-existing
code beyond an architecture doc, so a minimum mock-provider vertical
slice was built rather than a full engine.

**Backend**: new `internal/sportsbook` package (catalogue synced from a
`Provider` interface at server startup, not baked into the migration;
`PlaceBet` following `internal/casino`'s orchestrator pattern — validate
→ RG → risk → wallet lock → ledger post (`TxSportsbookBet`) → bet insert,
one transaction, single synchronous response since the mock provider
needs no external round-trip); 5 new endpoints (public catalogue browse,
player bet placement/history, Back Office admin queue); new
`sportsbook_bet:read` permission (tenant_admin/compliance/support/finance,
never platform_admin); migration `0078`. Concurrency proven with the
codebase's own deterministic `pg_stat_activity`-poll technique, never a
bare `WaitGroup`/`time.Sleep`.

**Back Office**: a small, directly-built sportsbook bet visibility page
mirroring Stage 5's withdrawal queue pattern exactly, plus a matching
`decimal_exponent` field on the admin bet response.

**Frontend**: new `b2c/` app (same Vite/React/TypeScript/TanStack
Query/Tailwind architecture as `backoffice/`), correctly starting from
Stage 5's already-fixed session-refresh and logout patterns rather than
reintroducing old bugs. Ships app shell/auth, sportsbook browse, a
singles-only bet slip with full three-way outcome handling (accepted /
well-formed rejection / genuine error, each rendered distinctly), and
paginated bet history. 18 vitest tests, clean build, both independently
re-verified.

**The defining acceptance test**: one new Go integration test chaining
the full real HTTP path — register → activate → genuine signed-webhook
deposit → wallet check → catalogue browse → event/selection detail → bet
placement at server-returned odds → bet history → wallet debit → Back
Office visibility → audit-trail visibility — every assertion checking
exact field values, not "a row exists." Passed first run; full
`internal/httpserver` suite stayed green after adding it.

**Independent review — architect/security/qa.** Architect found a real
P0 (the B2C app's odds/return display formula added 1 to the server's
`numerator/denominator` ratio, which IS the decimal odds value directly —
every price and return shown was overstated by the stake's own size) and
two P1s (the sportsbook bet idempotency key was scoped only by
`(tenant_id, idempotency_key)`, unlike every other player-facing
financial idempotency key on the platform, so one player supplying a key
another player had already used could silently receive the OTHER
player's bet back as accepted, uncharged — independently confirmed by
security) plus two P2s (OpenAPI status enums didn't match the real Go
enums; the B2C money helper silently guessed an exponent for unknown
asset codes on the outbound path). **All fixed**: `lib/odds.ts` corrected
to match the server exactly (new regression test suite added); the
idempotency key rescoped to `(tenant_id, player_account_id,
idempotency_key)` across the migration, lookup, and the ledger's own key
namespace, `insertBet` rewritten onto `db.IdempotentInsert` as the real
concurrent-race backstop, and a new cross-player regression test proves
the fix; OpenAPI enums corrected; the money helper now refuses an unknown
asset code rather than guessing. One P2 deferred (sportsbook has no
cumulative risk-rule entry yet — needs dedicated risk/ledger-finance
design work, not a Stage 6 blocker). QA: clean sign-off, confirmed the
acceptance test and concurrency/idempotency tests are genuinely
non-vacuous, no P0/P1.

No production wiring beyond this stage's own scope. Jurisdiction was
correctly not reopened; bonus-funded sportsbook stakes remain
platform-wide blocked. `PLAT-ROLESPLIT-1` remains unaddressed per its own
documented instruction. No automatic progression — Stage 7 is NOT
authorized and was not started.

## Stage 6.1 — B2C/Sportsbook Hardening & Architectural Closure Gate

A focused hardening pass over Stage 6, not a feature stage. Baseline
re-verified independently (fresh migration, full suite, both frontends).
Five focused specialist reviews (architect, security, ledger-finance, a
DB/RLS-specific pass, qa).

**One P0-equivalent fix**: the B2C bet slip and deposit form minted a
FRESH idempotency key on every submit click rather than once per attempt,
so a retry after a network/timeout error (where the original request may
have actually committed) placed a genuine second bet or deposit with a
real second stake/amount debit — defeating the idempotency mechanism for
the exact failure mode it exists to cover. Fixed by minting the key once
per bet-slip/deposit-form composition and holding it across any retry,
regenerated only when the player starts a genuinely new attempt. New
regression test locks this in.

**Financial-integrity hardening**: the ledger idempotency key gained an
explicit transaction-type discriminator (not just player-scoping);
ledger-finance's own review then found that change itself could allow a
double stake-lock during a mixed-version rolling deploy, closed with a
3-line cross-check that aborts (and rolls back) a transaction whose
ledger posting doesn't match the idempotency-resolved bet.

**One real DB-level gap fixed**: `sportsbook_bets.brand_id` wasn't pinned
to the specific player's own brand at the database level (only "some
brand in the tenant") — not reachable via the application today, but a
genuine missing invariant per CLAUDE.md's RLS-not-application-discipline
rule. Fixed with a composite FK reusing the exact pattern `wallets` and
`bonus_grants` already established. Also added `asset_code`→`assets` FK.

**Test-coverage gaps fixed**: two of three sibling rejection branches
(market-not-open, selection-not-active) had zero test coverage — a
refactor could have silently deleted either. `brand_id`/`decimal_exponent`
were populated but never asserted against known-correct values anywhere.

**Two factually wrong doc comments corrected**: sportsbook's own comment
claimed no other domain applies jurisdiction blocklisting — casino
actually does (though inert, since every blocklist is empty). A stale
risk-package comment said "no internal/sportsbook exists."

**New ADR 0047** formally documents the jurisdiction/catalogue-gating
boundary and the cumulative-risk gap, both disposed as SAFE DEFERMENT —
required before a second jurisdiction/B2B tenant, not before Stage 7. All
four originally-deferred Stage 6 items received a formal disposition
(all SAFE DEFERMENT, none blocking).

Full validation gate re-run clean after every fix: 31 Go packages,
concurrency/idempotency tests re-run 3x fresh under `-race`, both
frontends (`backoffice/` 17 tests, `b2c/` 19 tests) build and pass.

No casino, settlement, cashout, real provider integration, or country
approval work was performed. No automatic progression — Stage 7 is NOT
authorized and was not started.

## Stage 7 — B2C Casino Player Experience + Casino Vertical Slice — complete (approved-pending)

First complete B2C casino vertical slice using the EXISTING casino
architecture (built Stage 4A onward): register → login → deposit → wallet
→ casino lobby → select game → launch → wager → win → rollback → wallet →
player history → Back Office visibility → audit. Not the complete casino
product — proves the existing provider/wallet/ledger architecture supports
a real player-facing casino flow without violating any financial/tenancy/
identity/security/provider-boundary invariant.

**Pre-stage security fix.** `casino_launch_sessions` had the identical
brand-pinning gap Stage 6.1 found and fixed in `sportsbook_bets` — the
original `(brand_id, tenant_id) → brands` FK never pinned brand
specifically to the launching player's own brand. Fixed via migration
`0079` (composite `(player_account_id, tenant_id, brand_id) →
player_accounts` FK, reusing the same platform pattern), verified by a
full round-trip on a scratch database and 6 new adversarial tests.

**New backend surface (additive, no orchestrator/schema redesign):**
`GET /v1/me/casino/rounds` and `GET /v1/admin/casino/rounds` (a new
`casino_transaction:read` permission, mirroring `sportsbook_bet:read`
exactly) reconstruct a casino "round" from `casino_launch_sessions` plus a
re-derivation of `roundCorrelationID` — `ledger_transactions` itself has
no player attribution or player-scope RLS at all, so the player-facing
endpoint authorizes via an explicit `WHERE player_account_id = $1` under
`WithTenant`, not RLS. Three new play-simulation endpoints (`POST
/v1/me/casino/sessions/{id}/wager|win|rollback`) stand in for a real
hosted game client (none exists this stage), each driving the exact same
`Orchestrator.ReceiveCallback` pipeline the public provider webhook uses.

**One P0, independently confirmed by FOUR specialist reviews** (architect,
security, ledger-finance, database/RLS, each reproducing it live over
HTTP): the play-simulation rollback endpoint let a player name ANY
transaction as the reversal target, since the mock adapter's signature is
self-issued by the platform on the player's own behalf and authenticates
nothing about which transaction is named — `postRollback`'s own
`(tenant_id, provider_id, provider_tx_id)`-scoped lookup is correct for a
real, independently-signed provider webhook and was never designed for a
player-supplied reference. Exploitable for cross-player fund reversal and
a tenant-wide, unrecoverable tombstone-poisoning denial-of-service (the
ledger is append-only). Fixed with an explicit same-round/same-wallet
ownership check before the payload is ever signed
(`requireRollbackTargetOwnedByRound`), a non-production-only route gate
(`Deps.CasinoPlaySimulationEnabled`), a missing demo-mode/session-active
guard on rollback, an amount cap, and — per ledger-finance's own
independent P1 judgment — a required client `idempotency_key` with a
deterministic derived `provider_tx_id` on wager/win (a retry after a lost
response was, before this fix, a genuinely new financial transaction,
exactly the Stage 6.1 B2C bet-slip finding recurring in new code). Also
fixed: a multi-leg round under-reported repeated bet/win legs by
overwriting instead of accumulating; pagination lacked a deterministic
tiebreaker; one doc comment cited the wrong migration for an RLS
rationale. Five new adversarial tests reproduce and close each exploit
path directly (cross-player rollback, cross-round rollback, forged-
reference tombstone poisoning, demo-session rollback, over-cap rejection),
plus an idempotent-retry test and a cross-player history-isolation test.
Recorded as ADR 0048, including its own removal condition.

**Frontend**: `b2c/` casino lobby/session/history pages and a
`backoffice/` minimum-visibility round queue (permission-gated identically
to the sportsbook precedent), built by dedicated specialists and
independently re-verified (40 combined tests, clean builds).

**Acceptance**: a new Stage 7 defining acceptance test exercises the full
real HTTP path with exact financial assertions at every boundary
(including a reversed WIN correctly restoring the pre-win balance); the
Stage 6 sportsbook acceptance test was re-run against the final state and
remains green.

Full validation gate re-run clean after every fix: 33+ Go packages
(`-tags=integration`, fresh 79-migration database), both frontends
(`b2c/` 31 tests, `backoffice/` 19 tests) build and pass, `gofmt`/`go vet`
clean.

No real casino provider, sportsbook settlement/cashout, real PSP
integration, production country approvals, B2B/partner console, or
jurisdiction/wallet-ledger/identity/RG redesign work was performed. No
automatic progression — Stage 8 is NOT authorized and was not started.

## Stage 8 — Provider Integration Readiness Without External Contracts — complete

Stage 8 was authorized by the human as "STAGE 8 — EXTERNAL PROVIDER
INTEGRATION FOUNDATION / DUMMY SPORTSBOOK + DUMMY CASINO," opening with
"STAGE 7 IS APPROVED." Reconnaissance at Stage 8's start (repo grep,
`.env.example`, `docker-compose.dev.yml`, `/etc/hosts`, filesystem)
found no trace of either dummy provider API anywhere reachable from this
environment. The platform owner confirmed the documentation was
temporarily unavailable and explicitly re-scoped the stage to
"PROVIDER-INTEGRATION READINESS WITHOUT EXTERNAL API DEPENDENCIES" —
harden the provider boundary and build everything a real adapter needs
later, with an explicit ban on inventing either API's contract or making
any external network call.

Full design record: `docs/decisions/0080-provider-integration-readiness-
without-external-contracts.md`. Summary of what was built, reviewed, and
fixed is in `docs/active-stage.md`'s Stage 8 section (not duplicated
here) and the Stage 8 completion report delivered to the user. In brief:
`casino_provider_rounds` (migration 0080, resolving ADR 0048's residual
round-visibility limitation), `sportsbook_bets.provider_id`/
`provider_bet_reference` (migration 0081), a generic
`internal/providers/httpclient` client + `internal/providers/config.go`
loader (no specific provider wired in), `docs/integrations/dummy-casino.md`/
`dummy-sportsbook.md` recording the pending-documentation status
honestly, and minimal Back Office visibility additions.

Three parallel specialist reviews (architect, security, qa) found three
P1s, independently confirmed by more than one reviewer in two cases,
all fixed before close: a credential-exfiltration path in the generic
HTTP client (no redirect policy — fixed by refusing to auto-follow any
redirect), an unmapped cross-player/cross-brand round-ownership-conflict
error surfacing as a retryable 500 with no integrity alert (fixed with
an explicit 409 + alert at both the public webhook and play-simulation
endpoints), and the new round-binding committing even for a bet declined
by RG/Risk/insufficient funds (fixed by moving the bind to immediately
before the ledger post). Several P2s were fixed while still cheap
(an over-strict same-session ownership predicate that would have
rejected a legitimate free-spins round continuation; a missing DB-level
immutability trigger on `casino_provider_rounds`; a context-cancellation
retry-loop bug; a `provider_bet_ref`→`provider_bet_reference` rename to
match this project's own pre-existing doc 09 canonical naming, done
while the column was still unwritten by any code).

Full repo test suite (32 packages, 1273 tests) passes against a fresh
81-migration database with a clean up/down/up round-trip on the two new
migrations; race-detector clean on every touched package; the Stage 6
sportsbook and Stage 7 casino acceptance tests were re-run unmodified and
remain green; both frontends build and test clean.

No external network call was made. No real commercial provider, B2B,
retail, full reconciliation/settlement platform, jurisdiction human
decision, country approval, or wallet/ledger/identity/RG/risk redesign
was performed. No automatic progression — Stage 9 is NOT authorized and
was not started.

## Stage 9 — Production Readiness, Security, Resilience & Launch Hardening — complete

Stage 9 was authorized by the human as "STAGE 9 — PRODUCTION READINESS,
SECURITY, RESILIENCE & LAUNCH HARDENING," opening with "STAGE 8 IS
APPROVED." Explicitly framed as a large, deliberately non-micro-staged
pass across 28 named sections moving the platform from an
architecturally-proven B2C MVP toward a production launch candidate.
Full task table and review findings: `docs/governance/task-registry.md`'s
Stage 9 section; full narrative in `docs/active-stage.md`'s Stage 9
section (not duplicated here) and the Stage 9 completion report
delivered to the human.

**Critical production blocker (`PLAT-ROLESPLIT-1`) closed at the
in-repo/mechanical level.** The platform previously ran all runtime
traffic as `igaming`, the table-owning migration role — a genuinely
non-owning role structurally cannot issue owner-only DDL
(`DISABLE ROW LEVEL SECURITY`, `DROP TABLE`, etc.) regardless of grants,
which ordinary RLS-scoped DML alone does not prevent for an owner. A new
`igaming_runtime` role, a fail-closed production-startup check, and a
12-probe adversarial test suite (all denied) close this structurally.
The actual production cutover still needs a human operator with real
production credentials.

**Two genuine, previously-undetected financial defects found and fixed**
(both reproduced empirically by stashing the fix): a double
stake-release race letting two concurrent win callbacks on one locked
round each credit the player (driving `player_locked_cash` negative while
every individual posting still balanced — invisible to the platform-wide
debit/credit invariant), and an unhandled Postgres deadlock between a win
and a rollback of the same round from a lock-order inversion. Both fixed
in `internal/casino/orchestrator.go`'s `postWin`.

**Other real gaps closed:** responsible-gaming enforcement wired into
deposit initiation (previously only checked on gameplay); a
brand-pinning integrity gap on 7 tenant-owned tables (`ARCH-DB-3`, same
defect class Stage 6.1/7 already fixed elsewhere — 6 fixed by
drop-and-replace, one by an additive composite FK since a drop-and-replace
would have silently weakened a nullable-column check, one correctly
excluded as already-fixed and semantically distinct); new immutability/
TRUNCATE-deny triggers on 6 more tables (migration 0082); a back-office
double-submit gap on the bonus four-eyes approval queue; a
`RequirePlayerPrincipal` middleware closing an incidental-only (not
enforced-by-design) block on staff tokens reaching all 31 player
self-service routes; and per-IP rate limiting on the 7 unauthenticated
credential routes.

**Deferred with full documented reasoning, not silently dropped:**
`ARCH-DB-2` (6 RLS-free catalogue tables have application-level-only
write authorization — a cross-domain casino+sportsbook+internal/db+cmd
fix, not attempted in a parallel window) and `LOCK-1` (a real ABBA
deadlock risk between `postBet` and `postWinDirectCash`; the architect's
suggested in-`ledger.Post` sort fix was rigorously proven not to close
the cycle, since `postBet` locks its cash account outside and before
calling `ledger.Post` — needs a cross-cutting lock-ordering discipline,
an architect-level decision for a future stage).

**Backup/disaster-recovery: confirmed, not assumed, near-empty.** No
automated backup mechanism, no tested restore, no replica/standby exist
anywhere in this codebase or its `deploy/` tooling — structurally blocked
on ADR 0009's still-open hyperscale-hosting-provider decision, not an
oversight of this stage. Documented honestly with a concrete minimum
action plan in the new `docs/runbooks/backup-and-disaster-recovery.md`.
Stated targets (RPO=0 for the ledger, RTO<15min) are **NOT MET**. Two new
operational runbook documents were also written
(`docs/runbooks/operational-runbooks.md`, 9 concise incident procedures)
alongside the pre-existing observability/alerting and production-config
checklist runbooks. Data-retention/audit (§19) was reconfirmed already
correctly modeled as deferred pending a human legal decision — no code
or doc change needed there.

**Mid-stage infrastructure event:** 6 of 9 initially-dispatched
specialist agents failed to a weekly API rate-limit error. The
Orchestrator diagnosed the actual repo damage (one build break, fixed
directly), stopped and explicitly asked the human before proceeding, and
resumed all 6 workstreams (instructed to inspect partial work first, not
restart or duplicate) once an empirical probe confirmed a plan upgrade
had resolved the block. No rework resulted.

**Full independent validation, run by the Orchestrator against a fresh
`stage9_final` database** (not merely trusted from specialist
self-reports): all 82 migrations apply cleanly with a verified
up→down→up round-trip; `go build`/`go vet`/`gofmt` clean; the full
integration suite (32 packages) green; the full `-race` suite green; the
12-probe runtime-role adversarial suite green; the Stage 6 sportsbook and
Stage 7 casino defining acceptance tests pass by exact name; both `b2c`
and `backoffice` frontends' test suites, typechecks, and production
builds are clean against the final merged state.

No B2B/partner/retail work was performed. No undocumented external
provider API was integrated. No jurisdiction human decision, licence
status/expiry/dual-licensing determination, or wallet/ledger/identity
redesign was made. No automatic progression — Stage 10 is NOT authorized
and was not started.

## Stage 9.1 — Production Blocker Closure — complete

Stage 9.1 was authorized by the human as "STAGE 9.1 — PRODUCTION BLOCKER
CLOSURE," opening with "STAGE 9 IS APPROVED." A focused pass closing the
concrete engineering gaps Stage 9 identified but deferred: `ARCH-DB-2`,
`LOCK-1`, the two rate-limiter launch gates, `PLAT-MIGDRIFT-1`, the live
shared-dev-DB drift it caused, a provider-error-body safety P3, and 6
named minor technical debt items. Full task table and review findings:
`docs/governance/task-registry.md`'s Stage 9.1 section; narrative in
`docs/active-stage.md`'s Stage 9.1 section (not duplicated here) and the
Stage 9.1 completion report delivered to the human.

**`ARCH-DB-2` closed** (new ADR 0081, migration `0084`, fix-round
migration `0085`): all 6 platform-wide catalogue tables (`casino_games` +
5 sportsbook tables) now carry `ENABLE`+`FORCE ROW LEVEL SECURITY` with
open reads and platform-scoped writes only — a new
`db.WithPlatformService` closed-vocabulary identity for the unattended
sportsbook sync, the existing `WithPlatformAdmin` for `casino_games`,
plus a new staff-principal-resolution trigger (`0085`, closing a security
review's defense-in-depth finding) mirroring the existing `assets` table
precedent. The architect's own analysis correctly rejected adding
`tenant_id` columns to these tables as semantically wrong, not merely
unnecessary, after verifying none of the 6 has any tenant/brand scope to
begin with.

**`LOCK-1` closed, scope honestly widened** (new ADR 0082): a full trace
of every money-touching lock site found three more genuine ABBA cycles
beyond the originally-known one (`LOCK-1b` payments, `LOCK-1c`
withdrawal, `LOCK-1d` a row-lock/advisory-lock cycle spanning casino and
bonus). A new canonical lock order and a single `ledger.
LockProjectionsForPosting` pre-lock step close all four; `ledger-finance`
made and documented an independent judgment call accepting the ADR's
proposed zero-row-materialization design. Every required deadlock-freedom
test was proven to fail on pre-fix code (`40P01`) before being shown to
pass after. Two exceptions (one pre-existing, one new) are named and
grep-testable rather than silently inconsistent.

**Mandatory security + code-reviewer review** of both large changes
found no P0/P1 security findings but one genuine correctness regression
(a grant-lookup error losing its `ErrNotFound` wrapping, 404→500 — fixed)
and two regression-guard tests meaningfully easier to defeat than
intended (both strengthened, one rewritten on `go/ast`). All P1/P2
findings closed in a fix round; P3s documented per this project's
established "fix P0/P1 always, document P3/P4" convention.

**Other closures:** the rate limiter now has an explicit trusted-proxy
client-identity model (default: never trust an untrusted `X-Forwarded-
For`) with its per-minute limit wired through real configuration;
`PLAT-MIGDRIFT-1` is closed with a migration content-hash column and a
new `migrate verify` subcommand, run for real in CI and against the exact
shared dev database that had drifted (separately rebuilt from a clean
chain); a provider-error-body safety net bounds and redacts captured
response content.

**Full independent validation, run by the Orchestrator against a fresh
`stage91_final` database** (not merely trusted from specialist
self-reports): all 85 migrations apply cleanly; `migrate verify` reports
the chain clean; the full integration and `-race` suites (32 packages)
are green; the runtime-role adversarial suite is green; the
lock-ordering concurrency tests were repeated 3x under `-race`; the
Stage 6 sportsbook and Stage 7 casino defining acceptance tests pass by
exact name; both frontends are clean. This independent run itself caught
and fixed one real, previously-missed instance of the project's own
migration-count-fixture maintenance pattern (recorded in the task
registry).

No B2B/partner/retail work was performed. No undocumented external
provider API was integrated. No jurisdiction, legal, or
production-infrastructure decision was made or attempted. No automatic
progression — Stage 10 is NOT authorized and was not started.

## Stage 9.2 — Sportsbook Risk + Jurisdiction Enforcement + Casino Governance — complete

Stage 9.2 closed the three items Stage 9.1 classified FIX BEFORE
PRODUCTION, per the "STAGE 9.2 — SPORTSBOOK RISK + JURISDICTION
ENFORCEMENT + CASINO GOVERNANCE" directive: Workstream A (casino
four-eyes governance for blocklist removal/widening), Workstream B
(sportsbook cumulative and cross-player exposure risk), and Workstream C
(sportsbook jurisdiction/market gating).

**Workstream A — casino four-eyes governance.** Implemented per the
already-approved design in ADR 0081 §5.2: two new platform-scoped
tables (`casino_catalogue_change_requests`/`_approvals`, migration
0086) mirroring migration 0044's `asset_change_requests` shape, with
immutability/no-delete/no-truncate/platform-principal/deny-self-approval
triggers and an atomic `casino_catalogue_change_consume_approved_request`
function invoked from a new `casino_games_dual_control` trigger. Removing
a jurisdiction-blocklist code or reactivating a disabled game now
requires a second, distinct staff principal; adding a blocklist code
remains single-actor (fail-closed direction). A security review found and
reproduced a genuine P1 (`SEC-S92-1`): the initial implementation used
migration 0044's weaker principal-resolution shape, which let two
`person_id IS NULL` platform-admin accounts, or a suspended principal,
defeat four-eyes entirely. Fixed in migration 0089 by upgrading to
migration 0047's stricter shape (both parties must resolve to an active,
platform-scoped staff row with a non-NULL person). A second, independently
corroborated bug (security's `SEC-S92-5` and code review both found it):
the consume function picked the oldest pending approved request rather
than the one whose payload matched, which could deadlock two legitimate,
independently-approved actions on the same game. Fixed in migration 0089
by adopting migration 0047's own payload-containment-matching pattern,
verified sufficient by a new test rather than by adding an unneeded cancel
endpoint.

**Workstream B — sportsbook cumulative and cross-player exposure risk.**
Player-scoped cumulative stake (B1) is one new map entry
(`OperationSportsbookBet`) in the existing, already-audited
`internal/risk` cumulative mechanism — no new table, no invented
threshold. Cross-player book exposure (B2) cannot live in that
player-scoped mechanism (no scope axis for a selection, and
`potential_return` is not a ledger-visible fact), so a new tenant-owned
table `sb_exposure_limits` (migration 0088) and a new advisory lock class
(ADR 0082 Amendment A2, L0.6, event-scoped) were added, evaluated only
when a limit is armed. Because the platform's own payout-liability model
for sportsbook is undecided, a new Human Decision Register entry
(HDR-SB-1) was opened rather than an invented ceiling; unarmed behavior
is fail-open-when-unconfigured (identical posture to every other
tenant-configurable risk control), armed behavior fails closed. Security
found a genuine information-leak channel (`SEC-S92-6`): even with no raw
amount ever reaching a player, the distinctness of the exposure-limit
rejection category was itself a side-channel an attacker could
binary-search. Fixed in migration 0090 by collapsing the player-facing
shape into an indistinguishable generic risk-decline shape while
preserving the internal category and audit trail.

**Workstream C — sportsbook jurisdiction/market gating.** Integrated
(not redesigned) the existing player-jurisdiction resolution path,
mirroring casino's `LaunchGame` gate exactly. A new platform-scoped,
deny-only table `sb_jurisdiction_restrictions` (migration 0087) enforces
catalogue-level and bet-placement-level jurisdiction blocking through one
shared, statically-verifiable evaluation path (INV-SB-JUR-1) — the
catalogue never omits an unavailable event/market/selection, only marks
it, since both catalogue routes are anonymous and hiding would be
UI-inferred authorization. Unresolved jurisdiction fails closed
(`jurisdiction_unresolved`, distinct from `jurisdiction_blocked`); no
client-supplied or tenant-overridden jurisdiction is accepted. The
operating-market/licence-ceiling rung (Rung 2) is specified in full in
ADR 0083 but deliberately not shipped, even as a stub, because it is
blocked on the pre-existing, unresolved HDR-J-7 (no player-scoped
operating-country determination exists anywhere in the codebase) — a
fail-open-shaped stub would itself violate the no-fake-completion rule.
Security found `sb_jurisdiction_restrictions` had reopened a
defense-in-depth gap Stage 9.1 already closed for `casino_games`: no
staff-principal-resolution trigger, so a bogus but well-formed UUID could
write or withdraw restrictions. Fixed in migration 0090 by mirroring
migration 0085's established pattern exactly.

**Architecture.** One combined ADR (0083, not two) was produced for
Workstreams B and C because both terminate in the same `PlaceBet` call
and need a single authoritative composed order. ADR 0082 gained two
amendments: A2 (the new L0.6 exposure lock) and A3 (naming a pre-existing,
previously-unnamed lock exception in the sportsbook-bet insert path). The
three implementation waves were sequenced so the two that touch
`PlaceBet`'s composed order (jurisdiction gating, then exposure) never ran
concurrently, per the architect's explicit warning that doing so risks a
file-content-level collision, not merely a mergeable conflict.

**Specialist review.** Security, ledger-finance, risk, sportsbook,
casino, QA, architecture, and code review each reviewed the relevant
surfaces. Beyond the findings above, code review found two tests that
proved nothing (an already-cancelled context failing before the code
under test ever ran) and ledger-finance found an idempotent-retry test
that could not have distinguished correct re-evaluation from a
coincidental pass; all were repaired with genuine fault injection and
verified by temporarily breaking the underlying fix and confirming the
repaired test then fails. QA found the new exposure-limits table and its
admin API had zero test coverage and withheld sign-off until closed.

**Full independent validation, run by the Orchestrator against a fresh
`stage92_final` database** (not merely trusted from specialist
self-reports): all 90 migrations apply cleanly; `migrate verify` reports
the chain clean; the full integration and `-race` suites (32 packages)
are green; the 12-probe runtime-role adversarial suite is green; every
new Stage 9.2 concurrency-sensitive test family was repeated 3x under
`-race` with identical results; the Stage 6 sportsbook and Stage 7 casino
defining acceptance tests pass by exact name; both `b2c` and `backoffice`
frontends are unchanged and clean; the OpenAPI spec validates clean.

No B2B/partner/retail work was performed. No undocumented external
provider API was integrated. No jurisdiction, legal, or
production-infrastructure decision was made or attempted — the sportsbook
payout-liability ceiling question (HDR-SB-1) and the pre-existing
player-jurisdiction-evidence question (HDR-J-7) remain open and were not
decided silently. No automatic progression — Stage 10 is NOT authorized
and was not started.

## Stage 9.3 — Staging Deployment + Real End-to-End Acceptance — complete

Directed as "STAGE 9.3 — STAGING DEPLOYMENT + REAL END-TO-END
ACCEPTANCE": deploy the existing B2C and Back Office MVPs into a real,
isolated, non-production staging environment and prove it with genuine
end-to-end acceptance — not a production launch, and not a re-read of
existing tests.

**Hosting.** The human explicitly declined this session's use of the
ambient AWS credentials present in the container (unconfirmed account/
billing ownership) and directed a production-grade AWS staging design
without provisioning anything. A complete, independently `terraform fmt`/
`validate`-clean Terraform package (10 modules, a staging environment,
Dockerfiles, an RDS role-init script correcting a real ordering mistake
found mid-session, and a full runbook) was delivered under
`deploy/aws/`/`deploy/docker/` and ADR 0084, with the exact one-time
operator action named rather than any credential fabricated. The
application itself was validated by actually running it — a fresh
Postgres with all 90 migrations, the real `igaming`/`igaming_runtime`
role split, `platform-api` under `APP_ENV=staging`, and both frontends
built and served against it — inside this sandbox, since no real cloud
account was authorized this stage.

**A real, load-bearing gap was found and closed**: no CORS middleware
existed anywhere in this codebase, and the directive's required
three-subdomain staging URL structure is cross-origin by construction.
Closed with a small, config-driven, exact-origin-allowlist mechanism
(`internal/httpserver/cors.go`) explicitly documented as a browser
convenience, never an authorization boundary.

**Two genuine testability gaps were found by real acceptance testing and
closed as small, reviewed, non-production-gated seams**, mirroring the
already-reviewed Stage 7 `CasinoPlaySimulationEnabled` precedent exactly:
no mock deposit could be completed through the real HTTP API alone (the
mock payment provider's signing secret is intentionally unrecoverable
outside its own process) — closed by `POST /v1/me/deposits/{id}/
simulate-callback`; and no player registered through the real flow could
ever reach `active` status, the gate every deposit/bet/casino-launch
action depends on (the mock email provider deliberately never exposes
verification tokens) — closed by `GET /v1/me/email-verification/
dev-token`, which only ever feeds the real, unmodified confirm endpoint.
Both are structurally absent in production (`cfg.Environment !=
"production"`) and both received an independent, adversarial security
review before being accepted — each APPROVED WITH MINOR NOTES, with one
real omission fixed (missing audit fields on the deposit seam) and one
real operational gap named but not fixed (the activation seam's
in-memory store is per-process and will silently fail on the staging
Terraform's own default multi-replica topology).

**One genuine, deterministic financial defect was found by real
end-to-end use — not a contrived test — and fixed.** The payments and
casino mock adapters shared the literal `provider_id` "mock" and each
independently generated transaction references via an identical
low-entropy sequential counter, so the ledger's `(provider_id,
provider_tx_id)` idempotency key aliased the first transaction from each
domain onto the same key. A funded deposit followed by the first-ever
casino rollback in the freshly-seeded environment failed as a result
(the ledger's own reused-key guard correctly refused it — no corruption
occurred, but the operation was unusable). Fixed by registering the two
adapters under distinct ids (`mock-payments`/`mock-casino`); verified by
a direct regression proving the exact previously-failing rollback now
posts correctly, with the full repo build and test suite re-run clean
afterward.

**Full acceptance, run for real** (headless-Chromium browser flows plus
direct HTTP calls, not existing test re-runs): every B2C and Back Office
checklist item passed, including a complete critical end-to-end chain
(deposit → ledger → audit → Back Office UI, and separately sportsbook,
casino wager/win/rollback, and a genuine two-approver withdrawal
four-eyes) with a non-trivial `SUM(debits) = SUM(credits)` verification.
A full, independent live security acceptance pass covering 18 named
items (anonymous/cross-player/cross-tenant/cross-staff access, forged
tenant/brand ids, JWT forgery/expiry/reuse, CORS, cookies, frontend
bypass, RLS, runtime DB role, secret/source-map leakage, rate limiting)
passed all 18 with no blocking findings.

**Remaining production blockers** (not staging blockers, and none newly
introduced): `APP_ENV` fails open for all three non-production
simulation flags if ever misconfigured in real production (pre-existing
since Stage 7, formally flagged this stage per the reviewing specialist's
recommendation, not fixed unilaterally since it changes platform-wide
config semantics); the new activation-seam token store's per-process
multi-replica gap (documented interim mitigation, no redesign applied);
ADR 0009's hyperscale-cloud gambling-AUP confirmation, unaffected by this
stage; and HDR-SB-1/HDR-J-7, neither decided nor worked around.

No B2B/partner/retail/Stage-10 work was performed. No undocumented
external provider API was integrated or invented. No production
credential was requested, created, or used — the ambient AWS credentials
present in this environment were explicitly identified and explicitly
not used, per the human's own direction. No production-readiness claim
is made. No automatic progression — Stage 10 is NOT authorized and was
not started.

## Stage 9.4 Part 1 — APP_ENV Fail-Closed Validation + Stateless Activation Seam — complete

Authorized as "STAGE 9.4 — STAGING DEPLOYMENT READINESS + AWS STAGING
DEPLOYMENT," Stage 9.3 approved. Closes the two issues Stage 9.3's own
"Remaining production blockers" deferred as needing a config/architecture
decision rather than a unilateral fix. Part 2 (AWS account verification)
and Parts 3-11 (actual provisioning) required authorized AWS credentials;
the human again declined to supply any ("produce operator instructions
only"), so **no AWS provisioning was attempted this stage** — see
`docs/runbooks/stage-9-4-aws-account-verification.md`. Full design: ADR
0085.

**Fix 1 (`APP_ENV` two-layer fail-closed gate).** `internal/config
.Load()` validates `Environment` against `{"development", "staging",
"production"}` whenever `APP_ENV` is explicitly set to any value —
including an explicit empty string, a residual gap found independently by
both the security and architecture reviews of this exact change and
closed via a dedicated `resolveAppEnv()` (`os.LookupEnv` directly, not
the existing `getEnvDefault` helper, which deliberately folds "unset" and
"empty" together for every other setting but must not for this one). A
second, independent, explicit opt-in
(`TestSupportEndpointsEnabled`/`TEST_SUPPORT_ENDPOINTS_ENABLED`, default
`false`) must also be true before any of the three non-production
simulation flags register, computed in exactly one place
(`Config.TestSupportRoutesEnabled()`). `Environment == "production" &&
TestSupportEndpointsEnabled == true` is a hard `Load()` startup failure.

**Fix 2 (stateless, multi-replica-safe activation seam).** `GET
/v1/me/email-verification/dev-token` (Stage 9.3's per-process in-memory
store) is removed. `POST /v1/me/email-verification/request`/`/resend`
now return the raw token directly in that same request's `200` response
body when `AccountActivationTestSupportEnabled` is true — no new shared
infrastructure introduced. Flag-off/production response shape unchanged
(`204`, no body). A new test,
`TestAccountActivationDevToken_MultiReplica_RequestOnReplicaA
_ConfirmOnReplicaB`, constructs two genuinely independent
`httptest.NewServer` instances sharing only the database and proves the
seam works correctly across them.

**A related, genuine multi-replica financial-correctness bug was found
and fixed in the same pass**, not merely as a hypothetical: the
`architect` review, while examining Fix 2, found the same bug shape on
the actual financial simulation path —
`internal/payments.MockProvider.nextReference()` and
`internal/casino.MockCasinoProvider.nextReference()` both minted
references from a bare per-process `seq int` counter, so two replicas
could mint the same reference for their own first transaction, colliding
on `deposit_intents`' uniqueness constraint and the ledger's
`(provider_id, provider_tx_id)` idempotency key. Both now append a
`uuid.NewString()` suffix (`google/uuid`, already a dependency used
elsewhere in both packages); no test hardcoded the old exact format
(confirmed by grep before changing it).

**Reviews.** `security` and `architect` independently reviewed the diff:
**APPROVED WITH MINOR NOTES** from both. Security fixed one doc-only
inaccuracy itself (a response-distinguishability claim in the OpenAPI
spec) and reconfirmed `internal/auth/credential_token.go` untouched.
Both independently found the `APP_ENV=""` gap, closed directly with a new
regression test rather than left deferred. Architect additionally
required a new ADR (0085, written this stage), an inline correction to
`docs/decisions/0048-casino-play-simulation-trust-boundary.md`'s stale
single-condition-gate description, and a trim to
`deploy/aws/modules/ecs/variables.tf`'s new variable's doc comment (it
described a scenario this module's own `app_environment` validation
block already structurally prevents).

**Validation.** `go build ./...`, `go vet`, `gofmt -l` clean. Focused
suites (`internal/config`, `internal/payments`, `internal/casino`,
`internal/httpserver`) re-run against real Postgres after each fix,
including the mock-provider fix — all pass.

No AWS resources were created or modified. No B2B/Partner/Retail/Stage 10
work performed. No HDR-SB-1/HDR-J-7 decision made or worked around. ADR
0009's open AUP/legal confirmation is untouched. The pre-existing,
non-9.4-caused `IssueCredentialToken` concurrent-request race (ADR 0085's
"What this decision does not do") remains open as a separate, narrower
deferred item. No automatic progression — Stage 10 is NOT authorized and
was not started.

## Stage 9.4 — Staging Infrastructure Hardening + Cost Optimization — complete (repository-side; not deployed)

Directed as "STAGE 9.4 — STAGING INFRASTRUCTURE HARDENING + COST
OPTIMIZATION" after the human configured a READ-ONLY AWS credential and
confirmed in writing that account **765578795051** is the authorized
staging account and **eu-central-1** the staging region. Design: ADR 0086
(amends ADR 0084 for staging only). Operator procedure:
`docs/runbooks/stage-9-4-staging-lifecycle-runbook.md`.

**AWS access this stage: read-only only.** Identity/permission checks
(user `claude-staging-readonly`, `ReadOnlyAccess` only; every simulated
write action denied, re-checked at the end), read-only `terraform plan`
(first on the Stage 9.3 package: 77 resources, ~$135–155/month, PostgreSQL
16.4 unavailable in eu-central-1; final: staging 74 to add, bootstrap 9 to
add, 0 change/destroy), AWS Pricing API, `describe-db-engine-versions`
(16.15 available, db.t4g.micro orderable), IAM Access Analyzer
`ValidatePolicy`, IAM `SimulateCustomPolicy`, and read-only
`verify-teardown.sh` runs (no staging resources exist). **No AWS resource
was created, modified or deleted; no `terraform apply`/`destroy`; no
credential created or requested.**

**Implemented (repository):** eu-central-1 canonical and validated;
PostgreSQL pinned to 16.15; remote S3 state with native lockfile locking
plus a one-time bootstrap (versioned, SSE-S3, TLS-only, deletion-protected
bucket; IAM permissions boundary; IAM Access Analyzer account analyzer);
secrets kept out of Terraform state (RDS-managed master password,
ephemeral + write-only runtime/JWT/seed-admin secrets, password-free
`DATABASE_URL` + `PGPASSWORD`); staging-only force-delete of secrets/ECR;
RDS alarms fixed to `DBInstanceIdentifier`; Container Insights off (ALB
healthy-host alarms instead); SNS only with `alarm_email`; no NAT
(public-IP tasks, ingress only from the ALB; RDS private); HTTPS without a
domain via CloudFront VPC origins in front of an internal ALB, with an
IPv4 allowlist (≥ /24) protecting the test-support endpoints;
`TRUSTED_PROXY_COUNT = 2`; Fargate Spot for frontends and a mixed strategy
for platform-api (1 replica by default, `deploy.sh scale 2` for the
multi-replica test); immutable full-SHA ECR tags; split service/one-off
execution roles; `deploy.sh` lifecycle (up/scale/migrate/seed-admin/
status/down, one commit per environment lifetime) and read-only
`verify-teardown.sh`; least-privilege deployer IAM policies that cannot
escalate to admin; ~$0.10/hour while running, ≈$0.00–0.01/month after
teardown.

**Real defects found by the reviews and fixed in this stage** (each fixed
and then re-verified by the reviewer who found it):
- **security P1**: the deployer policy allowed escalation to account
  admin through role creation/inline policies. Now fixed: exact role
  names, a mandatory permissions boundary, explicit denies, 28/28
  read-only simulations.
- **security P1**: the Stage 9.3 role-init SQL printed the runtime DB
  password to stdout (CloudWatch Logs). Fixed and proven on a real
  PostgreSQL 16: the old script leaked it once, the fixed script never
  does.
- **architect P1**: a redeploy of a new commit would serve traffic before
  migrations ran.
- **code-reviewer / architect P1**: ECS services lacked a dependency on
  the capacity-provider association.
- **code-reviewer P1**: the deployer lacked `GetSecretValue` needed for
  provider refresh.
- **qa P0/P1**: the security-group module and the root-level decisions
  were untested. Six mutations survived before the fix; all are caught
  now.
- **P2s**: Back Office admin seeding impossible; `sns:ListTopics` missing;
  rotation left services stale; stale verification runbook; stale
  frontend images possible; VPC-origin teardown leftovers unchecked;
  security-group rule changes not tag-scoped.

One review claim was rejected with evidence: backend questioned
`TRUSTED_PROXY_COUNT=2`. The primary AWS documentation shows CloudFront
adds or appends the viewer IP to `X-Forwarded-For` for custom origins.
Backend accepted this; architect and security concurred.

**Independent review verdicts** (no specialist reviewed its own work):
- **architect**: CHANGES REQUIRED → APPROVED WITH MINOR NOTES (its three
  P3 follow-ups are fixed in 5c4ac51).
- **security**: CHANGES REQUIRED → APPROVED WITH MINOR NOTES, then a final
  sign-off on the follow-up commits 09553fa/5c4ac51 (N-1 closed as a
  detected, documented residual; N-2 closed; SG tag-scoping closed) →
  **APPROVED WITH MINOR NOTES**. Remaining P3s: a session-end Access
  Analyzer review step (added to the runbook), and tag-scoping of the
  route/IGW actions after the first real apply (runbook §12).
- **FinOps**: APPROVED WITH MINOR NOTES (corrections applied).
- **backend**: CHANGES REQUIRED → APPROVED.
- **qa**: CHANGES REQUIRED → APPROVED (no surviving mutations).
- **code-reviewer**: CHANGES REQUIRED → APPROVED WITH MINOR NOTES.

**Verification:** `terraform fmt -check` / `validate` clean (staging,
bootstrap and all 11 modules); 42 `terraform test` runs with mock
providers + 7 CloudFront-function node tests; mutation checks; repository
guards (no state/plan/tfvars tracked, no Stage 9.3 regressions, role-init
password guard); new `infrastructure` CI job; Go build/vet/unit tests
clean (no Go code changed); secrets scan: false positives only; the AWS
credential is absent from git history and the working tree.

**Deferred (recorded in ADR 0086, not built):**
- audit records use `RemoteAddr` (the ALB's IP) instead of the client IP
  — a **production launch gate**, backend owner;
- joining duplicate `X-Forwarded-For` headers;
- CloudFront, ALB and VPC flow logs;
- RDS `verify-full` TLS verification;
- the FinOps recommendation to serve the SPAs from S3.

**Remaining before deployment (human):** authorize and create a deployment
principal with the four `deploy/aws/iam/*.json` policies; run the
bootstrap; supply `staging_access_cidrs`. First-apply verification items
are listed in the lifecycle runbook §12. No B2B/Partner/Retail/Stage 10
work was performed. **Stage 10 is NOT authorized and was not started.**

## Stage 9.4 — AWS staging deployment + acceptance (human-executed) — recorded

The AWS staging deployment authorized after Stage 9.4 was executed by
the **human** from the allowlisted workstation. Agent-sandbox attempts
were stopped before any AWS change: the sandbox's egress IP was not in
the approved `staging_access_cidrs`, and its TLS interception prevented
`docker build` package downloads without a Dockerfile change the human
had prohibited. Before deployment the agent verified, read-only: deployer
identity, bootstrap permissions absent, 42 offline `terraform test`
runs, `verify-teardown.sh` exit 0, and a real S3-backend `terraform
plan` of 74 to add / 0 / 0 using a signature-verified provider mirror
with the committed lock file unchanged.

Commits made during deployment and acceptance (previously unrecorded):
`221b6ef` (frontend nginx pid path for the non-root image + smoke test +
CI job `frontend-image`), `b22d5c4` (ALB ingress from the CloudFront
origin-facing prefix list only; deployer network policy adds read-only
`ec2:GetManagedPrefixListEntries`), `8ec3dc7` (B2C and Back Office
workflows for the acceptance run; UI only), `9190d5d` (B2C selection
pickability fix).

**Deployed commit: `9190d5d01da076141a1f70d7e5897a573d3b18f4`.** The
human reports the real B2C browser acceptance **PASSED** (including the
previously failing sportsbook selection flow) and the Back Office was
manually verified. This is **human-attested**; no acceptance evidence
artifact is in the repository and the Orchestrator could not re-verify
it from the agent environment. Staging remains running (≈ $0.10/hour);
its disposition is a pending human decision.

## Stage 10 planning gate — state reconstruction and proposal — PLANNING ONLY, awaiting human approval

The Master Orchestrator reconstructed project state from the repository
(four read-only passes: governance, human-decision/deferred registers,
stage history, code reality) and verified the deployed commit locally
against a fresh PostgreSQL 16 database set up exactly as CI does.
Results at `9190d5d`: build, vet, gofmt, unit tests, migrations
up/verify/round-trip PASS; the integration suite PASSES except six
packages whose scratch-database tests need `CREATE DATABASE` (PASS when
the local throwaway role is given `CREATEDB`); `golangci-lint` v2.5.0
reports 19 low-severity issues.

**Findings (recorded, not fixed):**
- **F-1 (P1)** — CI's `build-test-lint` job has never executed on this
  branch: `.github/workflows/ci.yml:99` references the non-existent
  action `golangci-lint/golangci-lint-action` (present since `4790de0`);
  all 60 most recent runs failed at "Set up job". All Go evidence in the
  Stage 4I–9.4 records is local, not CI.
- **F-2 (P1)** — scratch-database tests need `CREATEDB`, which neither CI
  nor `deploy/init-app-role.sql` grants.
- **F-3 (P3)** — 19 lint issues (unpinned `version: latest`).
- **F-4 (P2)** — stale project memory (`project-status.md` stops at 4I;
  stale headers; HR-15 recorded as not implemented although migration
  0082 implements it).
- **F-5 (P2)** — `b22d5c4` (Terraform + IAM policy) had no recorded
  review; static `security`/`devops` reviews this round found no material
  risk; live Access Analyzer + 30-case simulation still owed.
- **F-6** — staging acceptance human-attested only.
- **F-7 (P2)** — `ledger.Post` idempotent replay compares only the
  transaction type, not amounts (`internal/ledger/ledger.go:316-326`);
  caller exposure not yet audited.

**Proposal:** Stage 10 — CI Evidence Restoration (W0, hard gate: ≥5
consecutive green CI runs) + Sportsbook Settlement Lifecycle (W1:
cash-funded singles in in-house mock mode — settle won/lost, void before
and after settlement, rollback/re-settlement, rollback-then-void;
append-only DB-enforced history; payload-compared idempotency; tombstones;
sportsbook reconciliation stream; staff-only test-support settlement
route; no webhook, no cashout, no manual operator settlement, no
self-exclusion auto-void consumer). Seven independent specialist reviews
(architect CHANGES REQUIRED, qa CHANGES REQUIRED, ledger-finance /
sportsbook / security / devops / product-owner-proxy APPROVED WITH
CHANGES) were consolidated; every P1 is incorporated. Two conflicts were
resolved by the Orchestrator on evidence (R-1 webhook → staff route,
because bets are in-house mode; R-2 self-exclusion seeding not re-asked,
because ADR 0042 decided it).

**Human approvals required:** approve Stage 10; approve the W0
test-admin mechanism; name/supply the verification credential for the
`b22d5c4` live checks; decide staging disposition; acknowledge OB-1.

**No code, migration, Terraform, IAM or AWS change was made in this
gate. Stage 10 is NOT authorized and was not started.** Full record:
`docs/plans/stage-10-planning-gate-proposal.md` (draft `8797fcf`; consolidated record `1968b5938abf1d522b805ca357a519f5365f1fb2`).

## Stage 10 — W0 CI Evidence Restoration — complete (Gates 1 and 2 passed)

Approved by the human 2026-09-25 (ADR 0087). W0 restored the CI Go gate,
which had never executed on this branch (F-1).

- **Implemented** (`05e1990`, `77a9293`): correct lint action
  (`golangci/golangci-lint-action@v9`, per the upstream compatibility
  table) with the linter pinned to v2.5.0; CI/dev-only `igaming_test_admin`
  role and one integration-tagged scratch-database helper
  (`internal/testsupport/scratchdb`) — application roles unchanged; a CI
  guard and a self-proving integration-evidence assertion step; the
  root cause of an intermittent sportsbook concurrency test (database-wide
  lock-wait counting across parallel packages) fixed in sportsbook and
  casino; 19 lint findings fixed without behavior change; records
  refreshed (project status, HR-15 status, ownership, change control,
  testing strategy, runtime-role note).
- **Gate 2 — five consecutive green CI runs:** #239–#243 (run ids in
  `docs/governance/task-registry.md` "Stage 10"): each 32 packages ok,
  2,202 tests passed, 5 expected skips (`BLOCKED on HDR-J-7`), 0 failed;
  migrations, reversibility, lint, guard all green.
- **Reviews:** security, devops, architect, qa, code-reviewer — all
  findings addressed or recorded.
- **Isolated external dependency (BLOCKED):** live re-validation of
  `b22d5c4` (Access Analyzer `ValidatePolicy`, 30-case
  `simulate-deployer-policies.py`). The deployer credential was probed and
  denied `access-analyzer:ValidatePolicy` and `iam:SimulateCustomPolicy`;
  a credential with those two read-only actions is required. Offline
  checks PASS (42 terraform tests, 7 node tests).
- **Deferred (P3):** stale ALB security-group description (a change would
  replace the group in running staging); integration-tagged files not
  linted in CI (467 findings); three statement-text-filtered lock-wait
  helpers.
- **Record correction:** only five of the six planning-gate integration
  failures were `CREATEDB`; the sixth was the concurrency flake above.
- **W1 preparation (documentation only, permitted during W0):** ADR 0088
  (settlement implementation contract) drafted by `ledger-finance`,
  reviewed by architect/sportsbook/security/risk/qa (all ACCEPT WITH
  CHANGES, no P0/P1), revised and **ACCEPTED** (`1db27ec`); `qa`'s W1 test
  plan recorded in `docs/testing/testing-strategy.md`.

Staging was not touched. No AWS operation beyond two read-only
permission probes.

## Stage 10 — W1 Sportsbook Settlement Lifecycle — complete (Gates 3–7 passed); Stage 10 COMPLETE

- **Scope delivered (ADR 0088, in-house MOCK mode):** settle won/lost,
  void before/after settlement, rollback, re-settlement, rollback-then-void,
  tombstone — on the canonical ledger with DB-enforced idempotency, audit,
  staff authorization (sole grantee `risk_manager`), RLS, and a
  multi-posting canonical pre-lock. Migration 0091 (append-only history,
  T-1/T-2, deny triggers, guarded runtime REVOKE, refusing down migration).
  Reconciliation `sportsbook_settlement` stream (statement match MOCK).
  Read-only b2c/back-office surfaces. Not a real provider integration.
- **F-7:** audit found class C; remediated as its own item (`36616f1`,
  ADR 0020 amendment) — `ledger.Post` rejects same-key different-payload
  replays with `ErrIdempotencyPayloadMismatch`.
- **Reviews:** security APPROVE WITH FINDINGS (fixed), code-review APPROVE
  WITH CONDITIONS (B-1..B-3 closed), ledger-finance APPROVED; mutation pass
  sportsbook 92.44% / ledger 92.00% with no unjustified in-scope survivor;
  SQL branch checklist complete.
- **Evidence:** CI green on every W1 commit (#247–#259); local fresh-DB CI
  replay 3/3.
- **New out-of-scope finding:** PAY-REV-1 (P1, pre-existing since Stage
  3B) — concurrent payments deposit reversals under different references
  each post. Recorded; needs its own authorized stage.
- **Deferred:** SB-T1-XMIN, OI-5 named debt, L0.6 residual (before arming
  exposure limits), money width, reconciliation scale, route removal at the
  real-provider stage.
- Staging untouched; OB-1 and all prior human decisions OPEN. Completion
  report: `docs/governance/stage-10-completion-report.md`. **No automatic
  progression — the next stage requires explicit human authorization.**

## Stage 10.1 Planning Gate — PAY-REV-1 + SB-T1-XMIN (+ future AI-agent architecture record)

- Planning only, as authorized. Analyses by `payments` and `sportsbook`;
  cross-reviews by `ledger-finance`, `security`, `architect`, `qa`,
  `backend` (record: `docs/plans/stage-10.1-planning/`); seven Orchestrator
  rulings (report §O).
- **PAY-REV-1 plan:** ADR 0082 L2 `FOR UPDATE` on the original deposit +
  re-check after lock; migration 0092 tenant-leading, `deposit_reversal`-
  scoped partial unique index with an RLS-proof refusal; typed
  `ErrReversalAlreadyExists`; 409 + alert + separately committed denial
  audit. F-7 idempotency semantics re-audited: unchanged.
- **SB-T1-XMIN plan:** migration 0093 body-only, fail-closed
  `pg_xact_status` check (empirically verified on PostgreSQL 16.13).
- **ADR 0089** (future AI-agent boundary) written and reviewed by
  security/bonus-engine/identity-compliance/backend; NOT IMPLEMENTED.
- **New pre-existing finding:** PAY-WH-TENANT-1 (security S-6, High) —
  payments webhook tenant comes from the URL slug and the mock's global HMAC
  secret does not bind the tenant; launch-blocking for a real PSP; outside
  10.1 pending a human ruling.
- ADR 0090 (Stage 10.1 definition) PROPOSED. No code, no implementation
  migration, no AWS action. **Stop at gate G0.**

## Stage 10.1 — PAY-REV-1 + SB-T1-XMIN + PAY-WH-TENANT-1 — implemented; stopped at the staging-deployment gate

- Approved 2026-09-26 (ADR 0090 ACCEPTED) with PAY-WH-TENANT-1 added by
  the human.
- **PAY-REV-1 (IMPLEMENTED):** L2 lock + re-check, migration 0092 unique
  index with RLS-proof refusal, key-first `ErrReversalAlreadyExists`, 409 +
  allow-listed alert + separately committed denial audit. Defect
  reproduced before the fix (two reversals posted).
- **SB-T1-XMIN (IMPLEMENTED):** migration 0093 fail-closed `pg_xact_status`
  check; epoch-anchor deviation from ruling R-2 ratified by architect and
  ledger-finance.
- **PAY-WH-TENANT-1 (IMPLEMENTED — MOCK resolver only):** route tenant
  selects the single per-(tenant, provider) credential; signature binds
  tenant/provider/key/body; verify-before-parse; uniform 401; OpenAPI
  documents the payments webhook. Cross-tenant attack reproduced before the
  fix. Real resolver NOT IMPLEMENTED — launch-blocking for any real PSP.
- Reviews: security, ledger-finance, code-review and architecture reviews
  plus re-verifications; every P2 closed; residual P3s recorded.
- New pre-existing findings registered for a human ruling: **KYC-WH-1**
  (High, affects staging `9190d5d`), CAS-WH-TENANT-1 (Medium).
- AWS/staging untouched. **Stop: staging deployment requires separate
  human authorization.**

## Stage 10.2 — Webhook trust hardening — implemented; stopped at the deployment gate

- Approved 2026-09-26 (ADR 0091 ACCEPTED).
- **Shared contract:** provider-neutral `internal/webhookauth` extracted from
  payments (aliases, behaviour-identical; payments tests unedited); per-domain
  signing prefixes, headers and mock key labels; shared HTTP preamble.
- **KYC-WH-1 (IMPLEMENTED — MOCK only):** committed secret removed; mock KYC,
  resolver and route only with test support; `provider_reference` staff-only;
  tenant-bound verify-first; forward-only status with same-transaction audit;
  204. Pre-fix forge reproduced (evidence E1–E3).
- **CAS-WH-TENANT-1 (IMPLEMENTED — MOCK only):** per-(tenant, provider) credential
  in the signing input; zero statements before verification; mock resolver only
  with test support; money path unchanged. Pre-fix cross-tenant tombstone
  reproduced (E4).
- **PAYWH-GATE-1 (IMPLEMENTED):** payments mock resolver gated the same way.
- **CI-FLAKE-281:** not reproduced; most likely a fixed-timeout margin in the
  Argon2-heavy Stage 9 tests; failure-log artifact added.
- Reviews: QA, backend, identity-compliance, casino, architect/DB, security,
  ledger-finance, code review + re-verification; all conditions met.
- New registry items: MOCK-ADAPTER-PROD-1, CAS-CAP-ROLLBACK-1 (hard pre-condition
  for real casino resolvers), WH-VENDOR-SCHEME-1, KYC-REASON-BOUND-1.
- AWS/staging untouched; staging `9190d5d` stays exposed to KYC-WH-1 until the
  human-authorized refresh. **Stop: deployment gate.**

## Staging teardown — 2026-09-26 (human-authorized)

- Governed `./deploy/aws/scripts/deploy.sh down` against account 765578795051 / eu-central-1 as
  `claude-staging-deployer`; destroy plan reviewed first (0 add, 0 change, 74 destroy, all
  staging modules). Result: 74 destroyed; Terraform state empty. Previous staging commit
  `9190d5d`; `957a3e8` was never deployed.
- Verification, retained infrastructure, the access-analyzer check the deployer cannot run, and
  the tooling note: `docs/governance/staging-teardown-2026-09-26.md`.
- **Staging OFF.** Next: one governed staging deployment from the final approved commit, when the
  human authorizes it.

## Stage 10.3 planning gate — Real Provider Trust & Casino Financial Readiness (PLANNING ONLY)

- Roadmap reconciliation (`docs/plans/stage-10.3-planning/00-roadmap-reconciliation.md`) and
  records hygiene (stale statuses corrected; ACC-EVIDENCE-1, STAGING-9.4-VERIFY-1,
  STAGE-NAMING-1 registered).
- Specialist papers: provider trust (architect), casino financial readiness (ledger-finance),
  KYC reason bound (identity-compliance). Reviews: product-owner-proxy, qa, security — all
  APPROVE WITH CONDITIONS; rulings R1–R15 (security's four-eyes on credential activation adopted).
- Proposed waves W0 (ADRs) → W1a–d (vendor schemes, synthetic guard, casino capability contract
  + G-1, KYC reason bound) → W2a–b (credential resolver + four-eyes, outbound credentials, casino
  consistency reconciliation) → W3a–b (casino statement MOCK, Secrets Manager backend code).
  Migrations 0094–0097 provisional.
- Human decisions: HD-10.3-1 (scope), HD-10.3-2 (deploy/ IAM code), HD-10.3-3 (player KYC reason
  wording), HD-10.3-4 (suspended-tenant casino settlement). Carried HDRs unchanged.
- **Stop: Stage 10.3 implementation gate.** Nothing implemented.

## Stage 10.3 — Real Provider Trust & Casino Financial Readiness — complete (MOCK / local); awaiting human authorization

- Authorized 2026-09-26 (ADR 0092 ACCEPTED; credential model ADR 0093). Rulings: HD-10.3-1 full
  scope; HD-10.3-2 AWS IAM code excluded (`awssm` behind existing boundaries, local SDK fake only);
  HD-10.3-3 players see status only; HD-10.3-4 suspended-tenant casino settlement unchanged
  (documented in the ADR 0025 amendment).
- Gates: 10.3-W0 PASSED; 10.3-W1 PASSED; **10.3-W2/W3 PASSED** (open findings carried)
  (`docs/plans/stage-10.3-planning/05-gate-log.md`).
- **W1 (IMPLEMENTED, MOCK providers):** WH-VENDOR-SCHEME-1, MOCK-ADAPTER-PROD-1 (all-mock
  production binary refuses to start, by design), CAS-CAP-ROLLBACK-1 + CAS-MULTIBET-WIN-1 (MOCK
  provider only), KYC-REASON-BOUND-1.
- **W2a:** PROV-CRED-RESOLVER-1 `IMPLEMENTED` (migration 0096, four-eyes activation, admin API;
  `memory`/`devfile` backends); PROV-OUTBOUND-CRED-1 `PARTIALLY IMPLEMENTED` (launch-blocking
  precondition: no non-synthetic adapter until outbound calls leave the DB transaction; tripwire
  test); KYC-PROVIDER-SELECT-1 `IMPLEMENTED`.
- **W2b:** CAS-RECON-1 `IMPLEMENTED` (migration 0097, `casino_consistency` C1–C7 with the C6 class
  ruling, verified-only rejection record; no money writes).
- **W3a:** CAS-RECON-STMT-1 `MOCK` (migration 0098; tautological against the MOCK source; real
  statement `PROVIDER DEPENDENT`).
- **W3b:** SECRETSTORE-AWS-1 `PARTIALLY IMPLEMENTED` (code, wiring, fake tests; IAM `NOT
  IMPLEMENTED`; drills `STAGING REQUIRED`).
- **CI-FLAKE-281:** `IMPLEMENTED` (calibrated hang guard; no recurrence #331–#342).
  **GO-TOOLCHAIN-VULN-1:** resolved (go1.26.8, x/text v0.42.0, otel v1.46.0, govulncheck@v1.8.0,
  golangci-lint v2.9.0).
- Reviews: security W2a and W2b/W3a/W3b APPROVE WITH CONDITIONS → re-verification APPROVE WITH
  CONDITIONS (all prior conditions closed; N-1 fixed in `e80114b` and verified CLOSED; L-N1a fixed
  in `99bb5b2`, SDK retries 1; N-2/N-3 Info open under HD-10.3-2); code-reviewer NOT READY → READY WITH FOLLOW-UPS (CODE-HYGIENE-10.3-1). Mutation
  evidence: 24/24, 25/25, 34/34, 38/38 + 7/7 + M46, 15/15 killed.
- CI: #341 green incl. govulncheck; #342 and #347 FAILED `TestStoreOutage_DoesNotPinPool`
  (CI-342-STOREOUTAGE: CPU scheduling delay under concurrent `-race` package binaries on the
  4-vCPU runner). First two fixes superseded (`c5f05a9` slack increase rejected by the orchestrator;
  `9df5869` 64-conn pool rejected by `security`, ruling A). Final: shared 20-conn pool, bounds
  unchanged, richer diagnostics (`103b033`); test isolated in its own blocking CI step (`25a3537`,
  ruling B). #343–#346, #348, #349 green (all jobs). Local 3× CI replay at `103b033`: ALL PASSED
  (40 integration packages, 0 skips).
- **New finding F-POOL-1 (Medium, `security`): `NOT IMPLEMENTED`** — ADR 0093 §5 "store outage does
  not pin the pool" does not hold at production pool size 10; launch-blocking unless fixed or
  accepted by the human (architect + security decision).
- New registered items: F-POOL-1, CAS-RECON-SCALE-1, PROVIDER-REF-BOUND-1, CODE-HYGIENE-10.3-1,
  CAS-WIN-IDEMP-1, PAY-SB-REPLAY-AUDIT-1, CR-CHECKLIST-HMAC-1 (human), DEPLOY-FPKEY-1.
- AWS/staging untouched; staging OFF. No real provider supported; no production or provider
  readiness claimed. **Stop: Stage 10.3 completion gate — next stage requires explicit human
  authorization** (decisions listed in the completion report).

## Stage 10.3 — human acceptance and post-acceptance close-out (2026-09-26/27)

- **Accepted:** human instruction "MASTER ORCHESTRATOR — CLOSE STAGE 10.3 AND PREPARE THE NEXT PRODUCT
  PHASE": Stage 10.3 ACCEPTED AS COMPLETE (code `103b033`, docs `2876fa5`). F-POOL-1 must be fixed.
- **F-POOL-1:** ADR 0094 designed (architect), co-signed with conditions C1–C11 (security), test plan
  confirmed with changes (QA), implemented (`7773649`..`f85c0b8`), reviewed (security 17, code 18,
  ledger-finance 19). First CI run of the timing lane (#360) failed; root cause was the test's
  measurement span after the redesign (pool connection establishment), fixed in `cb7fb92` with every
  bound unchanged; security accepted and re-closed WITH CONDITIONS; fix round `4ab399f`..`49ee4ec`,
  ADR wording B1 `8fec18f`. **K1 outstanding** (CI-BILLING-1). Local CI replay at `8fec18f`: gofmt,
  vet, lint 0 issues, build, migrate up/verify, unit race 37 ok, 3× integration race (41 packages,
  0 skips) each followed by the 8-test timing lane, reversibility — ALL PASSED.
- **Registered:** F-POOL-2 (Medium; dual-write High once reachable), KYC-ENFORCE-1 (launch-blocking,
  vendor-independent), BRANCH-PROTECTION-1, ACCESS-ANALYZER-CHECK-1, CI-BILLING-1.
- **Hygiene:** CODE-HYGIENE-10.3-1 closed; CR-CHECKLIST-HMAC-1 IMPLEMENTED; CODEOWNERS added.
- **Planning gate:** `docs/plans/next-real-provider-integration-planning-gate.md` (PO-reviewed).
- Not done by design: no vendor selected; no real provider implemented; no AWS deployment; Bonus Wave 4,
  AI agents not started; ADR 0089 architecture only; HD-10.3-2 infrastructure follow-ups documented only.

## PRH — Payment Readiness & Provider-Independent Hardening, incl. Financial Hardening / double-credit fix (2026-09-27/28) — stopped at the final human gate

- **Authorized by the human:** PRH (registered `1560ad0`), then "MASTER ORCHESTRATOR — AUTHORIZE FINANCIAL
  HARDENING / DOUBLE-CREDIT FIX" (FH-1..FH-7). Human decision HD-LEDGER-UNALLOC-1 = "A now, B later".
  Full classification: `docs/governance/payment-readiness-completion-report.md`.
- **PAY-DOUBLE-CREDIT-1 CLOSED.** INV-DEP-1 (ADR 0095 §28 AM-2) is enforced by:
  - the single choke point `postDepositSuccess(OrDispute)`;
  - migration 0107's partial unique indexes and NULL-safe guard;
  - the standing reconciliation kind `pay_captured_unposted`.

  A second real success now goes to disputed (`multiple_success_for_intent`) with no posting.
  LF-Q1 is superseded for the multiple-success case.
- **Other workstreams:**

  | Workstream | Status |
  |---|---|
  | Callback security (S-H1, S-M1, FH-5 C2/C3) | CLOSED |
  | Payout security (FH-6) | Fixed; launch conditions deferred (PAY-SEC-LAUNCH-1) |
  | A7 lock-order suite | IMPLEMENTED |
  | Kill switch phase 2 (migration 0106) | IMPLEMENTED; KS-DEP-T2-T3-1 closed |
  | F-POOL-2 | Payments part IMPLEMENTED (MOCK) |
  | KYC enforcement (ADR 0096, migrations 0100/0103) | PARTIALLY IMPLEMENTED |
  | Webhook rate limiting (ADR 0097) | PARTIALLY IMPLEMENTED |
  | PROVIDER-REF-BOUND-1 (0099) | IMPLEMENTED |
  | Payment reconciliation (0102/0104) | IMPLEMENTED against MOCK |
- **Migrations** 0099–0107 are allocated gap-free, and `migrate verify` is clean.
- **Reviews:**
  - architect final APPROVE WITH CONDITIONS;
  - product-owner-proxy APPROVE WITH CONDITIONS (no scope creep);
  - QA final gate PASS except item O (CI);
  - code re-reviews: kill-switch phase 2 READY, PRH-I5 READY, PRH-I3 READY WITH CONDITIONS, PRH-I2
    casino NOT READY. That last one found the HIGH regression **CAS-SESSION-EXPIRY-1**: consumed
    casino sessions refused bets after the 2-minute launch-token TTL. See the completion report for
    the fix status.
- **Verification (local; GitHub CI BLOCKED by CI-BILLING-1):**
  - lint 0 issues;
  - full integration suite under `-race` green;
  - A–O matrix green, including 50× race repetitions;
  - timing lane on an idle machine 40/40.
- **Governance:**
  - DB credential incident recorded; the permanent CLAUDE.md rule was added.
  - 210 leaked scratch DBs dropped.
  - TEST-T11A-FLIP-1: the orchestrator pushed once with a masked FAIL; all verification now uses
    `pipefail`.
- **Not done by design:**
  - no real vendor selected or integrated, and no production credentials;
  - no staging or AWS deployment;
  - Bonus Wave 4 and AI agents not started.

  **Stop: final human gate. The next step needs explicit human authorization.**
