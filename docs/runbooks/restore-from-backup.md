# Restore from backup (matches the local drill)

Status: **drill procedure IMPLEMENTED and exercised LOCALLY against synthetic data. Production restore: NOT MET.**
This runbook is written to match what `deploy/drill/backup-restore-drill.sh` actually does and verifies, so each step
here has been run at least once against a scratch database. It is not evidence that any production backup exists:
there is no managed backup, no PITR, no replica and no cross-region copy anywhere (AWS is OFF and unauthorised; see
`backup-and-disaster-recovery.md`). Read that file first for the honest status.

Nothing in this document authorises touching a production or shared database. Rehearse on a scratch database; during a
real incident, follow section 8 and have a human with database authority run the privileged steps.

## 1. What a restore must prove

A restore is successful only if the restored database is a **correct platform database**, not merely a database with
rows. The drill (`deploy/drill/verify.sql`) checks, on the source and on the restored copy:

| Check | Why |
|---|---|
| row counts per table per RLS scope | nothing silently missing. Counts are taken once per tenant scope because the owner role is subject to FORCE RLS |
| `SUM(debits) = SUM(credits)` per tenant and asset | the ledger invariant (CLAUDE.md) |
| balance projection equals a recompute from `ledger_entries`, per account | no drift (any drift is a P1) |
| RLS enabled **and forced** on every table that has a `tenant_id` column | the isolation guarantee survived |
| runtime role pins: exists and unprivileged, owns nothing, `CONNECT`, **no `TEMPORARY`** (0116), no schema `CREATE`, no `TRUNCATE/TRIGGER/REFERENCES`, ledger/settlement append-only grants, narrowed insert-only tables, cannot write `schema_migrations` | `docs/security/runtime-role-separation.md` |
| `schema_migrations` head and checksum fingerprint equal the source | same schema version |
| `pg_catalog` fingerprints equal the source: tables+RLS flags+owners, columns, **triggers**, **functions** (including `search_path` pins and `SECURITY DEFINER`), **policies**, constraints, indexes, object ACLs, default ACLs, schema ACL | no guard silently lost |
| `go run ./cmd/migrate verify` passes on the restored database | checksums of applied up-files |

A negative control then damages the restored copy (lifts FORCE RLS on one table, re-grants `TEMPORARY`, drops a ledger
trigger, corrupts a checksum, over-grants `TRUNCATE`) and requires each verifier to fail. If a check cannot fail it
proves nothing.

## 2. The local drill

```sh
# environment: the existing harness credentials (never put them in a file in git)
export TEST_ADMIN_DATABASE_URL=...   # igaming_test_admin, CREATEDB, NOSUPERUSER
export TEST_DATABASE_URL=...         # the migration-owner role `igaming`; only user/password/host are used

deploy/drill/backup-restore-drill.sh --dry-run          # preflight only, creates nothing
deploy/drill/backup-restore-drill.sh --deposits 1000    # full drill, ~25 s, ~30 MB, cleans up after itself
deploy/drill/backup-restore-drill.sh --cleanup-stale    # drops leftover drill_bk_* scratch databases
make drill-backup-restore                                # same, using the Makefile's variables
```

It never alters a role or password, never uses `sudo`/`pkill`, refuses non-local hosts, only drops databases named
`drill_bk_*` that it created, and removes its dump files. Idempotent: every run uses fresh names.

### What the drill found (findings are part of the evidence)

1. **`BACKUP-ROLE-1` (OPEN, needs a human/infra decision).** A full-data `pg_dump` by the migration-owner role `igaming`
   is **refused**: `query would be affected by row-level security policy for table "alert_deliveries"`. The tables are
   `FORCE ROW LEVEL SECURITY` and the owner is not `BYPASSRLS` (correctly so). A logical data dump therefore needs a
   dedicated backup role with `BYPASSRLS` (or a superuser / managed-service master). The drill is forbidden to create
   roles. Lifting FORCE RLS on a scratch database just to dump it was considered while building the drill, abandoned
   as an unacceptable weakening of the control under test (the throwaway database involved was dropped), and is **not**
   a supported workaround. Consequence: the supported data-bearing backup mechanisms are physical
   (managed snapshots / base backup + WAL), which do not go through RLS; choosing and provisioning them is AWS-gated.
2. **A dump or clone does not carry the database ACL.** Immediately after `pg_restore` (schema) or `CREATE DATABASE ...
   TEMPLATE`, `runtime_role_cannot_create_temp_0116` is **FAIL** (PUBLIC again holds `TEMPORARY`): exactly what
   `operational-runbooks.md` section 7 step 5 predicts, and `cmd/migrate up` will not repair it because
   `schema_migrations` already lists 0116. The drill applies `deploy/drill/post-restore.sql` (the same grants/revokes as
   `deploy/init-app-role.sql` and migration 0116) and the check turns PASS. **Never skip that step.**
3. **Constraint text is regrouped by a dump/restore round trip** (`pg_get_constraintdef` re-parenthesises nested
   `AND`). The constraint fingerprint ignores brackets only; names, columns and operators are still compared.
4. Default and explicit owner ACLs are equivalent when the explicit form equals the default; the fingerprint
   normalises with `acldefault` so a dump (which emits NULL for the default) compares equal.

### The two restore paths the drill exercises

| Path | What it is | Data? | What it proves | What it does NOT prove |
|---|---|---|---|---|
| Logical, schema-only: `pg_dump -Fc --schema-only` plus the data of `schema_migrations`, `pg_restore` into a new database | a portable DDL backup | no (only `schema_migrations`) | every table, trigger, function (search_path pins), policy, constraint, index, ACL and RLS/FORCE flag restores and is fingerprint-identical; `migrate verify` passes; the database-level ACL gap exists and is repaired | any financial data restore |
| Snapshot clone: `CREATE DATABASE ... TEMPLATE source` | a same-cluster copy: **not a backup** (same failure domain, same disk) | yes | all data checks above on a data-bearing restore; a realistic restore time shape for the dataset; snapshot RPO | durability, off-host copy, PITR, anything about a managed service |

Base backup / PITR (`pg_basebackup`, WAL archiving) is **not attempted**: the local test-admin role has no
`REPLICATION` attribute, `archive_mode` is `off`, and enabling either needs role or server-configuration changes the
drill must not make. Documented as AWS/managed-PITR-gated.

## 3. Procedure (what to do for a real restore, in order)

Applies to any restored copy once a backup mechanism exists. Steps 1-4 are what the drill automates.

1. **Stop writers.** Scale `platform-api` to zero or fence it at the load balancer. Do not let a half-restored database
   take traffic. (Drill analogue: nothing connects to the scratch databases.)
2. **Create the target database owned by the migration-owner role** and restore into it, as the migration-owner role
   (never as the runtime role). Use `--exit-on-error` and `--no-owner`; a restore that logs errors and continues is a
   partial restore.
3. **Re-apply database-level privileges** (`deploy/drill/post-restore.sql`; on AWS the environment's role-provisioning
   script `deploy/aws/sql/init-runtime-role.rds.sql`). Then follow `operational-runbooks.md` section 7 step 5 to recycle
   runtime backends and confirm there is no live `pg_temp` shadow.
4. **Verify** with `deploy/drill/verify.sql` against the restored database (compare against a baseline taken from the
   source if it is still reachable; otherwise rely on the CHK invariants and `migrate verify`) and
   `DATABASE_URL=<restored> go run ./cmd/migrate verify`. Every CHK must be PASS.
5. **Run the runtime-role adversarial suite** against it (`make test-integration-runtime-role` with
   `RUNTIME_DATABASE_URL` pointed at the restored scratch copy in a rehearsal; in an incident, the equivalent probes
   from `operational-runbooks.md` section 7).
6. **Reconcile.** Let the reconciliation scheduler run one cycle (or trigger it) and require zero drift before
   declaring the system serving. Provider-side state (PSP statements, casino rounds) after a restore to an earlier
   point must be reconciled against the vendors; a restore to time T loses everything after T, and vendors may have
   acted on those events (see section 6).
7. **Bring writers back** one instance first; watch `/readyz` and the 5xx ratio; then scale up.
8. **Record** the incident, the restore point, the measured wall time and any check that was not PASS.

## 4. Measured evidence (THIS synthetic dev dataset only)

Not production RPO/RTO. Local Postgres 16.13 on a shared sandbox VM, one run on 2026-10-12 at HEAD of this branch, 1000
deposits per wallet (16,000 ledger entries, 24,653 RLS-visible rows, source database 31.7 MB), 74 gating checks passed.

| Measurement | Value |
|---|---|
| `migrate up` of 132 migrations + grants | 4.64 s |
| seed (16,000 ledger entries through the real triggers) | 10.17 s |
| baseline verification of the source | 0.57 s |
| logical schema dump (1.7 MB, custom format) | 0.53 s |
| logical restore (schema) | 3.37 s |
| logical dump + restore + post-restore + verify + `migrate verify` | 4.69 s |
| snapshot clone (`CREATE DATABASE ... TEMPLATE`) | 0.68 s |
| snapshot restore to fully verified (clone + post-restore + all checks + `migrate verify`) | 1.72 s |
| snapshot RPO on the run | 2 committed ledger entries written after the snapshot were absent from the restore |

What these numbers mean: for a ~30 MB dataset the mechanics (restore, privilege repair, verification) take seconds.
They say nothing about a production-sized database, network transfer, a managed service's restore time or
detection/decision time, which dominate a real RTO. The directive RTO of under 15 minutes and RPO of 0 are **not
demonstrated**: the RPO row shows a snapshot loses committed writes by construction; RPO 0 needs continuous WAL
archiving or synchronous replication.

## 5. After a restore: things `migrate up` will not do for you

- `TEMPORARY` revoke (0116) and `CONNECT`: section 3 step 3.
- Runtime role grants made outside migrations (the CI/dev harness narrows `schema_migrations` writes; production
  provisioning is `docs/security/runtime-role-separation.md` section 6).
- Alert routes, provider credential handles and secrets: these live in the database and the secret store. A restore to
  an earlier time reverts routing and credential-handle state to that time; check them (`/v1/admin/alerting/status`).
- Signed-actor-proof keys, JWT secrets and payout-instrument keys are environment/secret-store material, not database
  content; they are not restored from a database backup.

## 6. Restore versus fix-forward

Policy (CLAUDE.md "corrections are compensating entries", HANDOVER section 22, `expand-contract-migrations.md`):

- **Default is fix forward.** A bad migration or a bad deploy is corrected with a new migration or a roll-forward build.
  A schema `down` is a development/staging tool only (`cmd/migrate down` refuses elsewhere).
- **Restore only for data loss or corruption that cannot be corrected by compensating entries**, and only with a
  declared incident and a human decision (database authority plus finance/compliance). A restore discards committed
  financial history after the restore point: those ledger entries, and the player/provider events behind them, must be
  reconstructed from provider statements and the audit trail, never by editing the restored ledger.
- Never "fix" a ledger or a balance by `UPDATE` in a restored database either. Corrections are compensating entries.

## 7. Limits of this runbook (explicit)

- No managed backup, PITR, replica, cross-region or off-site copy exists. `NOT MET`, AWS-gated.
- `BACKUP-ROLE-1` is open: no supported logical data backup path exists for the owner role.
- The drill's snapshot path shares a failure domain with its source and is not a backup.
- The row-count and invariant checks see data through the owner role under FORCE RLS, per tenant scope. A table whose
  rows are visible only under another scope (a staff or platform GUC) shows 0 on both sides; the catalog fingerprints
  and `migrate verify` still apply to it.
- The drill does not restart `platform-api` against the restored database (the load/soak plan has the local smoke).

## 8. During a real incident

Use `incident-response-and-oncall.md` for roles and communication, `operational-runbooks.md` sections 3 and 7 for the
financial and database-outage procedures, and this runbook for the restore steps. A human with database authority
runs steps 2-3; an agent never performs privileged restore steps against a shared or production database.
