# Project Progress

Last updated: 2026-09-11 (Stage 2)

## Status: Stage 2 (Identity + tenancy + security) — implementation and verification complete, pending specialist review reconciliation and human approval to start Stage 3

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
  `player_accounts`, `staff_users` (dual-scope RLS), `sessions` (three-
  policy RLS: public SELECT by token possession + tenant-scoped
  INSERT/UPDATE, required because a refresh token carries no tenant hint
  — see `docs/decisions/0013`), `login_attempts` (dual-scope RLS),
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
- **Documented, not code-changed**: `sessions`' public-read RLS policy
  (necessary — a refresh token carries no tenant hint to scope a lookup
  by) gives any tenant-scoped connection read access to every tenant's
  session metadata (IPs, user agents), safe today only because
  `ListActiveSessions`/`RevokeSession` filter by `principal_id` in
  application code. `security` and `code-reviewer` both flagged this as
  should-fix, not blocking; documented explicitly in ADR 0013 as accepted
  technical debt rather than silently left unmentioned. `architect` also
  flagged `brands`' public-read policy as broader than strictly needed
  (cross-tenant brand enumeration) — same disposition, tracked as debt,
  not fixed this stage.
- No specialist found scope creep against the Stage 2 DO-NOT-BUILD list,
  no fake-completion claims, and no contradiction between the new ADRs
  and Stage 0/1 decisions.

### Explicitly NOT built in Stage 2 (by design, per the Stage 2 DO-NOT-BUILD list)

Wallet/ledger, deposits, withdrawals, real payment providers, casino
providers, sportsbook, bonus engine, production KYC provider, production
AML provider, complete responsible-gaming engine, complete B2C frontend,
Partner Console. `kyc_tier`/`person_key_hash`/`verified_at` are hooks
only — unpopulated and uninterpreted by any Stage 2 code.

## Next stage

Stage 3 — not started; requires explicit human authorization per the
stage-gate rule in `CLAUDE.md`, and Stage 2's specialist-review
reconciliation and human approval first.
