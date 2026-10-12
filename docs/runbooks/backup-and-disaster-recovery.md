# Backup & Disaster Recovery — Stage 9 Evidence Status

This document is an honest evidence inventory, not a claim of achieved
RPO/RTO. Per CLAUDE.md's "no fake completion" rule, nothing below is
labeled done unless it has actually been built and exercised.

## Stated targets (directive, not yet met)

- **RPO (ledger): 0.** No committed financial write may ever be lost.
- **RTO: < 15 minutes** from a declared database-loss incident to a
  restored, serving primary.

**Status (updated 2026-10-12, OPS-READINESS-LOCAL): production RPO/RTO remain NOT MET. A LOCAL restore drill now
exists and has been exercised (IMPLEMENTED for the local dev dataset only). No data-bearing backup artefact has been
restored: the logical path is schema-only, and the data restore is `CREATE DATABASE ... TEMPLATE`, a same-cluster file copy
that is NOT a backup.** There is still no scheduled backup job, no
managed automated backup, no point-in-time recovery, no replica and no cross-region copy anywhere; those are
`PROVIDER DEPENDENT` and AWS-gated (AWS is OFF and unauthorised). This is expected at this point in the project: per
ADR 0009 no hyperscale cloud account has been provisioned, and managed automated backups (RDS/Cloud SQL/Azure Database
point-in-time-recovery) are the intended production mechanism, not something this sandbox can stand up or test.

### Evidence summary (honest, per item)

| Item | Status | Evidence / reason |
|---|---|---|
| Scripted local backup + restore drill on private scratch databases with synthetic data | **DONE (local, dev dataset only); no data-bearing backup artefact restored** | `deploy/drill/backup-restore-drill.sh`, runbook `restore-from-backup.md`. Logical path = schema-only dump + `schema_migrations` data (real dump and restore, no financial data). Data path = `CREATE DATABASE ... TEMPLATE` (same-cluster file copy, NOT a backup). 2026-10-12 run: 78 gating checks PASS, 0 FAIL (16 compare the clone with its own source and pass by construction); invariants: ledger balance per tenant/asset, projection == recompute, RLS enabled+FORCED on 98 tenant tables, runtime-role pins, `schema_migrations` head 132, catalog fingerprints, `cmd/migrate verify` |
| Verifiers proven able to fail (negative control) | DONE | the drill damages the restored copy (RLS, TEMP, TRUNCATE grant, dropped trigger, checksum, unbalanced ledger entry, projection mismatch, deleted row) and requires 8 detections |
| Measured copy/verify wall time / RPO | DONE for **this dataset only** | 22 MB, 3,200 ledger entries: clone to fully verified 1.18 s; logical schema dump+restore+verify 3.82 s; snapshot RPO = writes after the snapshot are lost (2 ledger entries in the run). **Not an RTO; not production RPO/RTO** |
| Full-data **logical** backup (`pg_dump`) as the migration owner | **BLOCKED (finding BACKUP-ROLE-1, OPEN: human/ADR decision, AWS-gated)** | refused by FORCE RLS (the owner has no RLS-bypass attribute, correctly). Security recommendation: managed physical snapshots + PITR as the primary path; a dedicated minimal `igaming_backup` role only if logical dumps are required (details in `restore-from-backup.md`). The drill may not create roles and does not weaken RLS to work around it |
| Base backup / WAL archiving / local PITR | **NOT ATTEMPTED** (AWS/managed-PITR-gated) | the local test-admin role has no `REPLICATION` attribute, `archive_mode` is off, `wal_level=replica`; enabling either needs role or server-configuration changes this work must not make |
| Scheduled / automated backups | NOT MET | none exists locally or elsewhere |
| Managed backups with PITR, retention policy | NOT MET, AWS-gated | no cloud account |
| Replica / standby / failover | NOT MET, AWS-gated | single primary only |
| Cross-region / off-site copy | NOT MET, AWS-gated | none |
| Restore of a database ACL (TEMPORARY revoke, CONNECT) | DONE in the drill | dump/clone do not carry it; `deploy/drill/post-restore.sql` repairs it; the drill shows the check FAIL before and PASS after |
| Directive **RPO 0** (ledger) | NOT MET | snapshots lose post-snapshot writes by construction; needs WAL archiving / sync replication |
| Directive **RTO < 15 min** | NOT DEMONSTRATED | seconds for a ~20 MB local dataset says nothing about a production-sized restore, transfer, detection and decision time |

**Production classification is unchanged: this remains a production blocker.** CLAUDE.md does not permit launching a
real-money ledger without a tested restore path *on the real topology*; the local drill proves the platform's schema,
guards and role model survive a dump/restore and gives the procedure and verifiers to reuse, but it is not that evidence.

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

1. **No automated (scheduled) backup job of any kind.** No `pg_dump`/`pg_basebackup`
   cron, no WAL archiving, no snapshot schedule — local, staging, or
   otherwise. `deploy/drill/` holds a manually run drill, not a backup job.
2. **No tested restore procedure on a production-like topology.** (Updated 2026-10-12: a restore HAS now been performed
   against this codebase's schema locally, on a scratch database, with a verified RLS-intact, role-correct result, see
   the evidence table above and `restore-from-backup.md`. That is local evidence only; no managed-service restore has
   been performed.)
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
3. Run a scheduled, automated **restore drill** (at minimum monthly; the manual local version is
   `deploy/drill/backup-restore-drill.sh`, runbook `restore-from-backup.md`) into
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

**Production backup/DR: NOT IMPLEMENTED. Local restore drill: IMPLEMENTED (dev dataset only).** Do not claim RPO=0 or RTO<15min are achieved. Treat
this as a **production blocker** (not a deferred nice-to-have) — CLAUDE.md
does not permit launching a real-money ledger without a tested restore
path, and none exists.
