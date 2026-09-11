# Active Stage

## Stage 1 — Architecture + Engineering Foundation

Status: **Complete, pending human approval to authorize Stage 2.**

### Objectives (as instructed at the Stage 0→1 gate)

Build the technical foundation only: repo/service structure, database
foundation + migrations, API foundation, tenant-context foundation,
auth/authz foundations, logging/metrics/tracing, health checks, error
handling, validation, event infrastructure foundation, testing
infrastructure, CI, linting, formatting, dev environment. Explicitly not
wallet/ledger, payments, casino, sportsbook, bonus engine, KYC/AML,
frontend, back office, partner console, or production deployment.

### Completed work

See `docs/progress.md` for the full, labeled inventory. Summary: one Go
deployable (`platform-api`) with config/observability/db/auth/tenant/
http/eventbus foundations, 6 reversible migrations establishing the
tenant/jurisdiction/licensing/asset schema with row-level-security-
enforced tenant isolation, a CI pipeline that runs unit and integration
tests (the latter against a real Postgres service container), an OpenAPI
foundation spec, and full local dev tooling.

A five-specialist review pass (`architect`, `security`, `qa`, `devops`,
`code-reviewer`) found and this session fixed two blocking, empirically-
verified defects (a Postgres-superuser RLS bypass in CI/dev config, and a
Postgres custom-GUC lifecycle bug that could turn a clean RLS denial into
an unrelated cast error) plus one missing RLS policy
(`tenant_jurisdiction_configs`), several smaller correctness/hardening
fixes, and documentation drift between what was built and what the docs
claimed. All fixes are applied and re-verified; see `docs/progress.md`
for the itemized list.

### Verification performed (all against a real local PostgreSQL 16, not mocked)

- `go build`, `go vet`, `gofmt`, `golangci-lint`: clean.
- Unit test suite: all passing.
- Integration test suite (build tag `integration`, real Postgres):
  all passing, including the RLS tenant-isolation proofs (cross-tenant
  read denied, cross-tenant insert denied with the correct SQLSTATE,
  no-tenant-context denied, connected role confirmed non-superuser/
  non-bypassrls) and the same proof again at the full HTTP layer.
- All 6 migrations applied, fully rolled back, and re-applied cleanly
  (reversibility round-trip).
- The compiled `platform-api` binary manually smoke-tested: health/
  readiness endpoints, unauthenticated and malformed-token rejection on
  the protected endpoint, and graceful shutdown on SIGTERM.

### Pending (to close out Stage 1)

- Commit and push this work to `claude/focused-wright-jw88w9`.
- Stage 1 Completion Report delivered to the human, ending with the
  required approval question. No Stage 2 work begins until that approval
  is given.

### Blockers

None technical. Two items remain open business/commercial tracks that
don't block engineering: the specific hyperscale cloud provider and its
written gambling-AUP confirmation, and the specific crypto custodian
vendor (`docs/decisions/0005-open-business-decisions.md`).

### Decisions/input still useful from the human (non-blocking for Stage 2 start)

1. Approve Stage 1 and authorize Stage 2.
2. No new business decisions surfaced this stage beyond the residual
   items already tracked in ADR 0005.
