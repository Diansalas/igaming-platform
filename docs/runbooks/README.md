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
- `backup-restore-dr.md` — A12 / ADR 0122: backup, restore and point-in-time-recovery runbooks (full loss, operator
  error -> PITR to a timestamp, single tenant), post-restore checklist, failure handling, measured local RTO/RPO
  (`deploy/backup/drill.sh`; throwaway cluster, synthetic data; managed RDS NOT proven).
- `aws-staging-deploy-plan.md` — PREPARED, NOT APPLIED, NOT AUTHORISED TO RUN: the ONE named-commit AWS staging deployment plan (owner decision 8):
  preconditions, owner inputs, go/no-go, roles, secrets, migrations, PITR, logical restore, observability, alert delivery, evidence, teardown.
- `backup-and-disaster-recovery.md` — honest RPO/RTO evidence status
  (production NOT MET; a local restore drill is DONE for the dev dataset only).
- `restore-from-backup.md` — restore procedure matching the local drill
  (`deploy/drill/backup-restore-drill.sh`), findings and measured dev-dataset numbers.
- `launch-and-suspension.md` — ADR 0112 brand/tenant launch, suspension, reactivation, closure.
- `operating-basis.md` — lic9: how an authorised admin adds, reviews and withdraws an operating basis (platform licence, partner licence, documented no-licence basis) per tenant / brand / country / operation with no release; what the evidence fields mean; country configuration is not legal proof; no value is shipped.
- `licence-lifecycle.md` — licensing slice 1: recording a regulator suspension / expiry / renewal, evidence reference, history, the expiry sweep alerts, interim rules and pending decisions.
- `incident-response-and-oncall.md` — TEMPLATE: no rota or recipient exists (ALERT-DELIVERY-1 open).
- `kill-switch-and-containment.md` — A13: stop something now (payment kill switch engage/release, casino game status, market
  disable, brand/tenant suspension); `payment-kill-switch-release-recovery.md` is its wedged-release companion.
- `rg-enforcement-activation.md` — A13: A6 RG floors, the one-way enforcement switch, readiness `R-RG-CONFIG`, failure modes.
- `market-blocking-configuration.md` — A13: A8 operating-market gate, no-route configuration model, staging prerequisites.
- `payout-reversal-and-amount-dispute.md` — A13: A5 governed payout reversal, amount/asset mismatch exit, negative-balance block.
- `withdrawal-dispatch-gate.md` — F4: withdrawal gate re-run at payout dispatch, `withdrawal_gate_decisions` backstop, symptoms and residuals.
- `withdrawal-restriction-policy.md` — lic11 (migration 0182): per-tenant withdrawal restriction policy (licence-state causes), interim default, governed changes, WD056, provenance.
- `staff-lifecycle-and-mfa-recovery.md` — A13: joiner/mover/leaver, MFA reset and recovery, `cmd/seed-admin` break-glass.
- `secrets-and-credential-rotation.md` — A13: inventory of every secret family and where its rotation procedure lives.
- `test-database-hygiene.md` — A13: TEST-HYGIENE-1 scratch database policy (scratch-database reaper IMPLEMENTED in A13a).
- Audit of this whole directory against the finish line: `docs/plans/a13-runbook-audit.md`.
- `expand-contract-migrations.md` — forward-fix versus restore policy, pre-flight refusals, write-quiet window.
- `load-and-soak-test-plan.md` — load/soak/chaos plan plus one small local smoke (`deploy/loadtest/`).
- `observability-and-alerting.md` — minimum alert-rule inventory; section 4 the A9 signals and the response for
  `reconciliation.sweep_failed`; rule files and a local Prometheus scrape example in `deploy/observability/`
  (metric rules: emitted and scrapeable, NOT DEPLOYED; log-event rules NOT wired).
- `alert-delivery-channels.md` — A9: email + one webhook alert channel (provider-neutral), the interim recipient and its
  configuration, SR-7 four-eyes route procedure, the webhook contract, metrics exposure (`METRICS_ADDR`), alert retention.
- `production-configuration-checklist.md` — field-by-field production
  config audit.
- `staff-mfa-keys.md` — ADR 0119 S1: provisioning and rotation of the
  `STAFF_MFA_FACTOR_KEYS` / `STAFF_MFA_MAC_KEYS` families (required in every
  environment since S3).
- `staff-mfa-s3-cutover.md` — ADR 0119 S3: deploy order of the two-step staff
  login and migration 0138, re-seeding staff, the `cmd/seed-admin` bootstrap and
  break-glass path (the S6 MFA reset now exists: `staff-lifecycle-and-mfa-recovery.md`), required database logging
  settings.
- `aml-officer-bootstrap.md`, `aml-adjustment-approver-confidentiality.md` — A7: AML officer bootstrap; confidentiality duty of the adjustment approver.
- `payout-instrument-keys.md` — payout instrument key families (ADR 0111).
- `production-startup-database-config.md` — production database/startup configuration and role requirements.
- `plat-rolesplit-staging-verification.md` — PLAT-ROLESPLIT-1 staging verification procedure (AWS-gated).
- `developer-handover.md` — developer onboarding notes (see also `docs/HANDOVER.md`).
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
- `residence-maker-checker.md` — lic10: configurable per-tenant second-Person approval of residence determinations (none / into_open_market / all), default unchanged, governance of the mode itself, pending is not evidence.
- `aml-screening-triggers.md` — lic10: governed automatic screening triggers, the durable queue, the MOCK-only worker and the withdrawal/payout gate; NOT transaction monitoring.
- `kyc-enforcement-policy-authoring.md` — lic10: the governed KYC enforcement policy routes (create draft, four-eyes activate, supersede), no shipped thresholds.
