# Runbooks

Operational incident runbooks now exist (Stage 9) — see:

- `operational-runbooks.md` — sections 1-14: deployment, rollback, financial incident,
  provider/payment/auth/database outages, security incident, §10 capability
  grants (K1), §11 manual adjustment (K2), §12 payments sweeper (H), §13 KYC
  outbox (E1), §14 payment force-resolution M1/M2 (K3).
- `migration-0101-payment-attempts-remediation.md` — migration 0101 remediation.
- `stage-9-3-staging-deployment-runbook.md`, `stage-9-4-aws-account-verification.md`
  and `stage-9-4-staging-lifecycle-runbook.md` — staging (AWS is currently OFF;
  see `../governance/staging-teardown-2026-09-26.md`).
- `backup-and-disaster-recovery.md` — honest RPO/RTO evidence status
  (currently NOT MET/NOT IMPLEMENTED — see that file for why and what's
  needed).
- `observability-and-alerting.md` — minimum alert-rule inventory.
- `production-configuration-checklist.md` — field-by-field production
  config audit.
- `self-hosted-runner.md` — PREPARED, NOT ACTIVE: Mac self-hosted CI runner
  plan for CI-BILLING-1 (paired with `.github/workflows/ci-selfhosted.yml`,
  manual dispatch only); no setup until the owner separately authorizes it.

This directory also still carries local development setup, below.

## Local development setup (Stage 1)

Prerequisites: Go (see `go.mod` for version), a local PostgreSQL 16
instance (native install or via `deploy/docker-compose.dev.yml` if Docker
is available), `golangci-lint`.

```sh
# 1. Start Postgres (native service) ...
make dev-db-up
# ... or, where Docker is available:
#   docker compose -f deploy/docker-compose.dev.yml up -d

# 2. Create the dev role/database once. The role MUST be created without
#    SUPERUSER and without BYPASSRLS - either attribute silently disables
#    row-level security regardless of policy correctness (see
#    docs/decisions/0002-multi-tenancy-isolation-strategy.md and
#    internal/db/db.go's verifyNotPrivileged, which refuses to connect as
#    such a role at all). See deploy/init-app-role.sql for the same setup
#    via docker-compose.
#   sudo -u postgres psql -c "CREATE ROLE igaming LOGIN PASSWORD 'igaming_dev_password' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;"
#   sudo -u postgres psql -c "CREATE DATABASE igaming_platform_dev OWNER igaming;"

# 2b. The runtime-role and migration tests also need `make dev-db-init-roles`
#     (igaming_runtime) and `make dev-db-init-test-admin` (igaming_test_admin)
#     plus the TEST_*_DATABASE_URL variables (see Makefile, .env.example).
#     Role/sudo steps are for humans only; sub-agents must never create or
#     alter database roles (CLAUDE.md environment safety).

# 3. Apply migrations
make migrate-up

# 4. Configure environment
cp .env.example .env   # then `set -a; source .env; set +a` or export manually

# 5. Run the service. `make run` defaults APP_ENV=development (an exported
#    APP_ENV wins): the synthetic-component startup guard treats a MISSING
#    APP_ENV as production and refuses to start with the mock adapters.
make run

# 6. In another shell, exercise it
curl -s localhost:8080/healthz
curl -s localhost:8080/readyz
```

## Verifying the foundation

```sh
make ci                 # fmt-check, vet, lint, build, unit tests
make test-integration    # RLS tenant-isolation + HTTP integration tests,
                          # against the running local database
```

See `docs/architecture/00-system-overview.md` for what "the foundation"
currently covers, and `docs/active-stage.md` for what's deferred to later
stages.
