# Backup & Disaster Recovery — Stage 9 Evidence Status

This document is an honest evidence inventory, not a claim of achieved
RPO/RTO. Per CLAUDE.md's "no fake completion" rule, nothing below is
labeled done unless it has actually been built and exercised.

## Stated targets (directive, not yet met)

- **RPO (ledger): 0.** No committed financial write may ever be lost.
- **RTO: < 15 minutes** from a declared database-loss incident to a
  restored, serving primary.

**Status: NOT MET. No backup mechanism exists yet, and no restore has ever
been exercised in this project.** This is the emptiest evidence category
in the platform (confirmed by the Stage 9 architect review). This is
expected at this point in the project, not a surprise: per ADR 0009, no
hyperscale cloud account has been provisioned yet — only local/dev
environments exist — and managed automated backups (RDS/Cloud SQL/Azure
Database point-in-time-recovery) are the intended production mechanism,
not something this sandbox can stand up or test.

## What actually exists today (IMPLEMENTED)

- **Append-only ledger design** (`docs/decisions/0019-...`): the ledger
  itself is structured so that, given an intact WAL/base-backup chain, a
  point-in-time restore reconstructs a fully self-consistent financial
  history — this is a property of the schema, not a backup mechanism.
- **Balance projection rebuild from the ledger**
  (`internal/ledger.RebuildBalance`, `internal/wallet`'s
  `RebuildProjectionRow`): if a `wallet_balance_projection` row is lost,
  corrupted, or drifts, it can be deterministically recomputed from
  `ledger_entries` alone. This is a *projection*-recovery tool, not a
  database-disaster-recovery tool — it assumes the ledger tables
  themselves are intact.
- **Reconciliation scheduler** (`internal/reconciliation`): runs on a
  schedule (target: hourly, per CLAUDE.md), diffs `SUM(ledger entries)`
  against the balance projection per wallet, and raises a mismatch record
  on any drift. This detects projection drift; it does not detect or
  repair loss of the ledger tables themselves, and it is not a backup.
- **Migration reversibility**: every migration in `migrations/` has a
  tested `.down.sql` (verified via up→down→up round-trip in this stage's
  validation gate). This protects against a bad *schema* migration, not
  against data loss.

## What does NOT exist (NOT IMPLEMENTED — PROVIDER DEPENDENT)

1. **No automated backup job of any kind.** No `pg_dump`/`pg_basebackup`
   cron, no WAL archiving, no snapshot schedule — local, staging, or
   otherwise. `deploy/` contains only `docker-compose.dev.yml` and
   `init-app-role.sql`; neither performs backups.
2. **No tested restore procedure.** A restore has never been performed
   against this codebase's schema, so there is no evidence a backup (even
   if one existed) would actually restore to a working, RLS-intact,
   role-correctly-separated database.
3. **No replica / standby / failover target.** A single primary is the
   only topology that has ever run.
4. **No cross-region or off-site copy of anything.**

## Why this is deferred rather than built now (PROVIDER DEPENDENT)

Real backup/DR for this platform is inseparable from the actual hosting
decision:

- The mechanism (managed point-in-time recovery vs. self-managed WAL
  archiving vs. logical dump-based) depends on which hyperscale provider
  and which managed database service is finally selected (ADR 0009 leaves
  this open, pending AUP confirmation — a human/legal task).
- RPO=0 for the ledger specifically requires either synchronous replication
  or continuous WAL shipping with a verified replay path — an
  infrastructure choice, not application code.
- Building and "testing" a backup/restore runbook against a database that
  will never be the production topology would produce evidence that does
  not transfer, which is itself a form of fake completion.

## Minimum action plan (for when a provider is selected — HUMAN/DEVOPS FOLLOW-UP)

1. Enable the provider's managed automated backups with point-in-time
   recovery, retention ≥ 35 days (adjust once a real regulatory retention
   period is set — see `docs/architecture/16-privacy.md`'s "Retention"
   section, itself HUMAN/LEGAL DECISION REQUIRED).
2. Enable continuous WAL archiving (or the provider's equivalent) so RPO
   can approach 0 for the ledger tables.
3. Run a scheduled, automated **restore drill** (at minimum monthly) into
   an isolated environment, followed by: `migrate status` verification,
   the runtime-role adversarial probe suite
   (`internal/db/runtime_role_separation_test.go`), and the reconciliation
   scheduler's drift check — a restore that leaves stale roles/RLS/schema
   is not a successful restore.
4. Measure actual RTO from that drill and record it here as evidence,
   replacing this document's "NOT MET" status with a dated result.
5. Add a standby/replica once the provider and topology are chosen, sized
   to the actual production traffic (not yet known).

## Classification

**NOT IMPLEMENTED.** Do not claim RPO=0 or RTO<15min are achieved. Treat
this as a **production blocker** (not a deferred nice-to-have) — CLAUDE.md
does not permit launching a real-money ledger without a tested restore
path, and none exists.
