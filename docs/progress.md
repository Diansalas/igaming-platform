# Project Progress

Last updated: 2026-09-11 (Stage 1)

## Status: Stage 1 (Architecture + engineering foundation) — complete, pending human approval to start Stage 2

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

## Next stage

Stage 2 — Identity + tenancy + security. Not started; requires explicit
human authorization per the stage-gate rule in `CLAUDE.md`.
