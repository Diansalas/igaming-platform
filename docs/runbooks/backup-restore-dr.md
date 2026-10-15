# Backup, restore and point-in-time recovery (A12, ADR 0122)

Status: **LOCAL DRILL IMPLEMENTED AND PASSING on throwaway PostgreSQL 16 clusters with synthetic data (2026-10-15). Managed
(RDS) backup and PITR: PREPARED in Terraform, NEVER APPLIED, NOT PROVEN. Production RPO/RTO: NOT MET / NOT DEMONSTRATED.**
Read `backup-and-disaster-recovery.md` for the standing status table and `docs/decisions/0122-backup-role-and-pitr.md` for the design.
Nothing in this file authorises touching a shared or production database: every command below is rehearsed on a throwaway
cluster; during a real incident a human with database authority runs the privileged steps and an agent does not.

## 1. What exists, and which mechanism for which failure

| Failure | Mechanism | Tooling (tested locally) | RPO | Where it is proven |
|---|---|---|---|---|
| Total loss of the database host / cluster | Managed snapshot or PITR to "latest" into a NEW instance (RDS); or logical restore into a new cluster | RDS (prepared, not applied); `backup.sh` + `restore.sh --apply-globals` + `verify.sh` | PITR: minutes (archive lag, see section 6); logical: the backup interval | logical: local drill PASS; RDS: NOT proven |
| Logical corruption / operator error / bad data fix | **PITR to a timestamp just before the damage** into a NEW instance, verify, cut over (or extract) | `pitr-drill.sh` (physical base backup + WAL archive), RDS `restore-db-instance-to-point-in-time` (prepared) | bounded by WAL archive lag | physical PITR: local drill PASS; RDS: NOT proven |
| One tenant's data wrong | Restore to a side instance and **investigate**; correct with compensating ledger entries through the governed path. No row is ever copied back into the live ledger | PITR/logical restore to a side database + read-only comparison | n/a | guidance only (section 4.3), not drilled |
| Projection drift only | Recompute from the ledger (`internal/ledger.RebuildBalance`, reconciliation) - not a restore | existing | 0 | existing tests |
| Bad migration / bad deploy | Fix forward (`expand-contract-migrations.md`); restore only for data loss | - | - | policy |

Artefacts: `deploy/backup/` - `backup.sh`, `restore.sh`, `verify.sh`, `pitr-drill.sh`, `drill.sh` (orchestrates everything on throwaway
clusters), `init-backup-role.sql`, `post-restore-grants.sql`, `baseline.sql`, `seed.sh` (+ the seeder
`internal/httpserver/drillseed_integration_test.go`, tags `integration drillseed`), `restorecheck/` (Go; platform reconciliation + startup guards),
libraries `lib-*.sh`. Static guards: `go test ./deploy/backup/` (no sudo/pkill, no RLS/trigger weakening outside the throwaway-only drill
scripts, no role DDL outside provisioning, no literal passwords, target guard present, role grants limited to SELECT/USAGE/CONNECT).

## 2. Roles and credentials (by reference, never in git)

| Role | Used for | Notes |
|---|---|---|
| `igaming` (migration owner) | `cmd/migrate`, owning objects, restore target owner | on RDS the master user; subject to FORCE RLS: **cannot** produce a complete dump |
| `igaming_runtime` | the application | non-owning; cannot dump (`permission denied for table actor_proof_keys`) |
| **`igaming_backup`** | the backup runner and `verify.sh` baseline/count queries | NOSUPERUSER, **BYPASSRLS**, SELECT-only, CONNECTION LIMIT 2, no membership, read-only sessions, **VALID UNTIL** (time-boxed), audited; never used by the application |
| restore role | `restore.sh` | the owner role, or a CREATEDB role that is a member of it (drill: `drill_restore`); on a brand-new cluster a role able to create the project roles (drill: the bootstrap superuser; RDS: the master user - unverified) |

Credentials reach the scripts only as a **0600/0400 passfile** (`BACKUP_PGPASSFILE`) materialised by the runner from the secret store
(`backup_runner_secret_arn`); the scripts take no password in argv, environment or URL, and refuse a missing or too-open passfile.
An **expired** credential fails the backup closed with a message naming the likely cause (drilled). Re-mint: re-run
`init-backup-role.sql` with a fresh `IGAMING_BACKUP_PASSWORD` and `BACKUP_ROLE_VALID_UNTIL` from the secret store job.

Target guard (all scripts): `127.0.0.1:5432` (the shared development/CI cluster) is always refused; loopback targets must be a
drill port; a remote host (staging RDS, later) is refused unless `BACKUP_ALLOW_REMOTE_TARGET=<exactly that host>` is set for
that invocation. Production is not authorised.

## 3. Taking a logical backup (secondary mechanism)

```sh
export BACKUP_PGHOST=<host> BACKUP_PGPORT=<port> BACKUP_PGDATABASE=<db> BACKUP_PGUSER=igaming_backup
export BACKUP_PGPASSFILE=<0600 file from the secret store>
export BACKUP_ENCRYPT_AGE_RECIPIENT=<age public recipient>      # or BACKUP_ENCRYPT_GPG_RECIPIENT; public references only
deploy/backup/backup.sh --out-dir <backup volume> --label <reason>
```

Output (one 0700 directory): `db.dump[.age|.gpg]`, `globals.sql[.age|.gpg]` (roles and attributes, **no passwords**), `baseline.txt[.age|.gpg]`
(exact row counts and content hashes of every table, ledger invariants, sequences, roles and ACLs by name, triggers, policies, RLS flags,
catalog fingerprints, all evaluated in the **same exported snapshot as the dump**, so a live source is fine), `SHA256SUMS`, `manifest.json`.
It refuses: a non-BYPASSRLS or superuser role, a role that cannot SELECT every table, an unencrypted dump (unless
`BACKUP_ALLOW_UNENCRYPTED=synthetic-data-only`), insufficient disk, an existing output directory. Exit 4 = written but the source
violated a ledger invariant (artefacts kept as evidence; open a P1).

## 4. Recovery runbooks

Common rule: **a restore is a declared incident** (`incident-response-and-oncall.md`; recipients are `<NOT ASSIGNED>` until ALERT-DELIVERY-1
closes). Approval before the restore point is chosen or traffic is cut over: database authority (devops) **and** finance/compliance, and
for a restore point that discards committed financial history also the owner/CRO delegate. Writers are stopped first. Everything after
the restore point is lost from the database and must be reconstructed from provider statements and the audit trail, never by editing
the restored ledger.

### 4.1 Full loss: new cluster from a logical backup (drilled)

1. Stop writers (scale `platform-api` to zero / fence at the load balancer). Do not let a half-restored database take traffic.
2. Provision the empty PostgreSQL 16 server (same major version; the dump records `server_version_num`).
3. Fetch the newest backup set that passes the checksum; keep the decryption identity in the restorer's secret store
   (`RESTORE_AGE_IDENTITY_FILE=<path>` for age; gpg uses the restorer's keyring).
4. `RESTORE_CONFIRM=<new db> deploy/backup/restore.sh --backup-dir <set> --new-database <new db> --apply-globals` as the restore role.
   Sequence enforced: checksums -> decrypt -> archive parses -> roles exist (or the three project roles are created **without
   passwords** from `globals.sql`) -> database must not exist -> `CREATE DATABASE ... OWNER igaming TEMPLATE template0` with the source
   encoding/collation -> `pg_restore --single-transaction --exit-on-error` (all or nothing) -> `post-restore-grants.sql` (database ACLs, schema
   ownership) -> `ANALYZE`. On any failure the database this run created is dropped.
5. **Re-provision what a dump cannot carry**: role passwords (`igaming_runtime`, `igaming`, `igaming_backup`) from the secret store;
   the complete environment role-provisioning script (`deploy/aws/sql/init-runtime-role.rds.sql`); the **`database_environment` anchor**
   (section 5); the secret-store material that is not in any database (JWT secrets, staff MFA key families, payout-instrument keys,
   provider credentials).
6. `deploy/backup/verify.sh --baseline <baseline.txt>` (after decrypting the baseline) as described in section 5. **Do not open traffic
   until it prints `VERIFY RESULT: PASS`.**
7. Post-restore checklist (section 5), then bring one writer back, watch `/readyz` and the 5xx ratio, then scale up.

### 4.2 Logical corruption / operator error: PITR to a timestamp (drilled physically; RDS prepared)

Choose the target **before** the damage: from `audit_log`, the application logs or the operator's statement time, minus a safety margin;
you can only move earlier than the damage, never "just after it ignoring it". Writes after the target are lost (section 6 gives the loss bound).

1. Declare the incident; stop writers; record the chosen UTC timestamp, who approved it and why.
2. **RDS (prepared, unproven):** `aws rds restore-db-instance-to-point-in-time --source-db-instance-identifier <db> --target-db-instance-identifier <db>-pitr-<id> --restore-time <UTC>`
   with the same subnet group, security group, parameter group and key; it creates a NEW instance. The old instance stays untouched for forensics.
   **Local drill equivalent (proven):** a base backup (`pg_basebackup -X stream`, `pg_verifybackup`) plus the WAL archive; recovery instance =
   copy of the base backup + `restore_command` + `recovery_target_time` (or `recovery_target_name` for a restore point created with
   `pg_create_restore_point`) + `recovery_target_action = promote` + **`archive_mode = off`** (a recovered instance must never archive into the
   source's archive). Before recovering, run the **WAL continuity check** (`wal_continuity` in `pitr-drill.sh`) from the base backup's
   last segment to the target segment: a missing segment makes recovery stop with `recovery ended before configured recovery target was
   reached` and the server does **not** promote (drilled).
3. Wait for promotion (`pg_is_in_recovery() = false`). Re-provision credentials if the new instance is not a byte copy of the old roles.
4. `verify.sh --seq-mode ge --baseline <baseline.sql output taken at the target, if you have one>`; otherwise run the invariants-only
   gates (`INV` lines of `baseline.sql`, `migrate verify`, `restorecheck`). Sequences may legitimately be *ahead* of the baseline after WAL
   replay (PostgreSQL pre-logs sequence values), never behind.
5. Cut over: point the application at the recovered instance (secret/endpoint change), run the post-restore checklist (section 5), bring
   one writer back, then scale. Keep the damaged instance read-only for investigation until the incident closes.

### 4.3 Single-tenant recovery (guidance; NOT drilled, no tooling)

The ledger is append-only and multi-tenant in one database: **there is no supported "restore one tenant in place".** To investigate or
correct one tenant: restore a side instance (4.2 for the time, 4.1 for a logical set), read that tenant's rows through the backup role
(BYPASSRLS) or the tenant GUC, diff against production, and apply the correction as **compensating entries through the governed manual-adjustment
path** (K2, four-eyes, reason code, audit). Never `INSERT` historical rows into the live ledger, never `UPDATE` a balance, never replay a
subset of rows through triggers. A tenant-wide revert needs an ADR (it contradicts append-only history).

## 5. Post-restore checklist and verification gates

`verify.sh` is fail-closed; each line is `CHK|name|PASS|FAIL|detail`. Gates:

1. `baseline.sql` re-evaluated on the restored database as `igaming_backup` and compared with the backup's baseline, line for line: `TABLE`
   (exact row count **and** content hash of every one of the 153 tables, including FORCE-RLS tables), `SEQ`, `ROLE`/`MEMBER`/`DBACL`/`SCHEMAACL`/`ACL`/`COLACL`/`FUNCACL`/`DEFACL`
   (roles and grants **by name**), `TRG` (all guard/append-only triggers present **and enabled**), `POL`, `RLS` (enabled + forced + owner), `FP` (catalog fingerprints).
2. Invariants (`INV`, all must be 0): ledger `SUM(debits)=SUM(credits)` per transaction+asset and per tenant+asset; no ledger entry without a
   transaction; **balance projection == recompute from `ledger_entries` (drift 0)**; no sequence behind its column; FORCE RLS + a policy on every tenant table; no trigger disabled.
3. `deploy/drill/verify.sql` as the owner: RLS forced, runtime-role pins including the database-level `TEMPORARY` revoke (migration 0116), append-only grants.
4. `cmd/migrate verify`: applied-migration checksums.
5. `restorecheck` as `igaming_runtime`: the platform's own `ledger_vs_projection` reconciliation per tenant (rolled back by default so the verified
   database stays byte-identical), real RLS behaviour (no scope -> no tenant rows; a scope -> only that tenant), and the **startup guard**
   `db.VerifyEnvironmentAnchor`.

What must be re-provisioned after a restore (the drill proves each of these is needed):

| Item | Why | How |
|---|---|---|
| Role passwords | a dump carries none; restored roles cannot log in (drilled) | secret store -> `ALTER ROLE` by the DB admin; `init-backup-role.sql` for the backup role |
| Database-level ACL, `public` schema owner | not in a dump (the verifier caught the schema owner on the first run) | `post-restore-grants.sql` (done by `restore.sh`) + the environment's full role-provisioning script |
| **`database_environment` anchor** | a restore into a new database is **unrecorded** (or carries the source's anchor); a production `platform-api` **refuses to start** on an unrecorded anchor and on a dev/test anchor (drilled: refused, then admitted after the owner records it) | owner, once: `INSERT INTO database_environment (environment) VALUES ('production');` then re-run `restorecheck -expect-anchor production`. Never restore a dev/test backup into production; never record `production` on a scratch copy |
| Secret-store material | not in any database backup | restore from the secret store: JWT secrets, `STAFF_MFA_*` key families, payout-instrument keys, provider credentials. (`actor_proof_keys` IS in the database: the dump is therefore a signing-key-bearing artefact) |
| Reconciliation | prove no drift in the running system | run the scheduler once (or `restorecheck -persist`) so a `reconciliation_runs` row exists; require 0 mismatches before traffic |
| Provider-side state | the restore point is earlier than provider events | reconcile PSP statements and casino rounds since the restore time; do not assume vendors rolled back |
| Alert routes, credential handles | live in the DB; a restore reverts them to the restore time | check `/v1/admin/alerting/status`, re-apply changes |
| Communication | | incident channel update at: restore started, restore point chosen, verify PASS, traffic restored; notify affected tenants and compliance if committed financial history was discarded |

## 6. Measured RTO / RPO (LOCAL, SYNTHETIC, THIS MACHINE - not production)

Source: the final full run of `deploy/backup/drill.sh` (2026-10-15), also `docs/plans/a12-backup-drill-record.md` section 3 for the verbatim log. Machine: a **shared** 4-core
sandbox VM, local disk, PostgreSQL 16.13, other agents' test suites running concurrently (run-to-run variation was about 1.5x; the previous full run is quoted in brackets).
**These are numbers for THIS synthetic dataset on THIS machine. They are not production RTO/RPO.**

Dataset (loaded through the platform's own services): **112,688 ledger entries** in 56,312 transactions (casino bets 38,400 / wins 17,288, deposits 320, sportsbook bets 192 and
settlements 64, withdrawal requests 24, K2 manual adjustments 16, rejected withdrawals 8), 286,433 rows in 153 tables, 113,698 audit rows, 12 bonus grants, 4 alerts,
withdrawals in 5 states, adjustments in 3 states, 4 tenants / 96 players, database size **165.7 MB**.

| Step | Wall clock | Notes |
|---|---|---|
| `migrate up` (145 migrations) | 5.8 s (2.6 s earlier) | |
| Seed through HTTP handlers and services | 910 s (836 s earlier) | ~124 ledger entries/s on a contended VM; **not** a throughput claim |
| **Logical backup** (`backup.sh`) | **6.5 s** (8.4 s) | = baseline queries in the snapshot 3.6 s + `pg_dump -Fc` 2.5 s + globals/checksums 0.4 s; dump 18.4 MB (9x smaller than the database); age-encrypted variant 6.2 s (encryption step 0.2 s) |
| **Logical restore** into a new database on the same cluster (`restore.sh`) | **10.6 s** (16.4 s) | integrity+decrypt 0.7 s, `pg_restore --single-transaction` 7.4 s, grants 0.1 s, `ANALYZE` 2.1 s |
| `verify.sh` (all gates) | 6.2 s (9.1 s) | dominated by hashing every table (`baseline.sql`) |
| **Full loss: new empty cluster** | initdb+start 1.2 s; restore 12.4 s (incl. creating the project roles); credentials re-provisioned 0.3 s; verify 6.2 s; **= 20.1 s** | secret-store retrieval, provisioning of real infrastructure, DNS/cut-over, humans: **not measured** |
| **Base backup** (`pg_basebackup -X stream`) | **2.6 s** (4.9 s), 206.6 MB | `pg_verifybackup` PASS |
| WAL archive volume produced by the two PITR write phases | 470 MB (incl. padding segments from forced switches) | mostly full-page images right after a checkpoint |
| **WAL archive lag** (last commit's segment visible in the archive) | **6.6 s** (3.5 s, 8.1 s, 14.6 s in other runs) | bounded by `archive_timeout = 30 s` |
| Copy of the base backup into the recovery directory | 0.5 s | local `cp -a`; production: restore from object storage / volume |
| **PITR recovery to a named restore point**, start -> promoted | **4.1 s** (2.1 s); 4.6 s including the copy | replayed 5 WAL segments (~80 MB) after the base backup's end |
| **PITR recovery to a timestamp** | **3.0 s** (1.7 s); 3.7 s including the copy | same data, same result |
| Verification of the recovered instance | PASS, 31 gates | exact content hash and count of every table equal to the state recorded at the target; rows written after the target absent (ledger_entries 114,888 recovered = expected; the source had moved on to 115,882); operator deletion undone |

**RPO statements (what a loss can cost):**

- *Logical backup*: RPO = the age of the newest successful backup set when the loss happens (backup interval plus job duration, plus any time the job was failing unnoticed - there is
  no alerting for that yet). Everything committed after the backup's snapshot is lost. This is the SECONDARY mechanism.
- *Physical recovery (WAL archive / RDS PITR)*: RPO = the WAL not yet archived. With `archive_timeout = 30 s` a quiet system archives its tail within 30 s; the drill measured 3.5-14.6 s for the last commit's
  segment to appear. A busy system fills 16 MB segments faster than that. **This is not RPO 0.** RDS ships transaction logs about every 5 minutes (AWS-documented, **not measured here**).
- **RPO 0 for the ledger (the directive) is NOT MET** and needs synchronous replication / a standby, which does not exist.

**RTO**: the directive (< 15 minutes from a declared database-loss incident to a restored, serving primary) is **NOT DEMONSTRATED**. At this size the *mechanical* part (restore + verify) is about 17-26 s (logical) or 4-5 s plus
verification (PITR). The parts that dominate a real RTO were not measured: detection, the approval decision (section 4), provisioning a real instance (RDS: a restored instance is a new instance whose creation time and
storage warm-up were not measured), re-provisioning real credentials, application cut-over, post-restore reconciliation, provider-side reconciliation.

**Extrapolation to a larger database (an ESTIMATE, not a measurement).** Method: scale the measured per-gigabyte rates linearly - dump 22 s/GB (43-66 MB/s), baseline hashing 21-25 s/GB, `pg_restore` 45-73 s/GB (14-22 MB/s)
plus `ANALYZE` 13-17 s/GB, verify 38-55 s/GB, local base backup 13-36 s/GB, WAL replay ~20-25 MB/s. It ignores everything that makes real systems worse (index builds are super-linear, I/O and network limits, RDS volume warm-up after a
restore, concurrent load, human time) and better (parallel `pg_dump -Fd -j`/`pg_restore -j`, which the single-transaction guarantee here deliberately gives up). Worked example: a 5 GB database -> logical backup ~5 min, restore
~5-8 min, verify ~3-5 min (RTO order of 15-20 min mechanical); a 50 GB database -> backup ~40 min, restore 50-75 min, verify 30-45 min (**RTO order of 2-2.5 h**), so **logical restore cannot be the disaster-recovery primary
beyond a few GB**; the physical path (snapshots / PITR) is the primary, and its production-size timing is exactly what `aws-staging-deploy-plan.md` section 8.3 must measure.

## 7. Failure handling (each scripted in `drill.sh`, expected outcome asserted)

| Failure | Expected outcome | Drilled result |
|---|---|---|
| Corrupted dump (bit flip) | `restore.sh` stops on **checksum mismatch before any database is created**, exit 2 | PASS |
| Truncated dump | checksum mismatch, nothing created | PASS |
| Truncated dump with recomputed checksum | `pg_restore --list` refuses the unreadable archive, nothing created | PASS |
| Corrupted dump with recomputed checksum (checksum cannot help) | zlib/archive error **part-way through the restore**; single transaction rolls back; the half-created database is dropped, exit 3 | PASS |
| Injected mid-restore failure (the `pg_restore` client is killed once the new database has grown: an operator Ctrl-C or a lost connection) | same rollback and drop, exit 3 | PASS |
| Tampered `globals.sql` with a recomputed checksum (`ALTER ROLE igaming WITH SUPERUSER`) | `--apply-globals` refuses it (allow-list `lib-globals.sh`); no role and no database created. The unsigned manifest is the residual (ADR 0122 section 9) | PASS |
| Restore as the wrong role (`igaming_runtime`, `igaming_backup`) | refused with an explanation, nothing created | PASS |
| Restore into an existing (non-empty) database | refused, the existing database is untouched | PASS |
| Insufficient disk | `backup.sh` / `restore.sh` refuse up front (`BACKUP_MIN_FREE_MB`, `RESTORE_MIN_FREE_MB`). **A real `ENOSPC` part-way through is NOT simulated**: a failed `pg_restore --single-transaction` rolls back, and `restore.sh` drops the database it created; a full disk on the *server* can still stop PostgreSQL itself - provision restore targets with at least the database size plus WAL headroom | check PASS; ENOSPC documented only |
| WAL gap in the archive | the continuity check names the missing segment; recovery to the target does not promote (`recovery ended before configured recovery target was reached`) | PASS |
| Backup role credential expired | `backup.sh` fails closed with an "expired credential" message, no partial output | PASS |
| Backup credential file missing / too open | refuses, nothing connected | PASS |
| Backup run as the owner role (or a superuser) | refused before dumping: not BYPASSRLS / superuser | PASS |
| Wrong target (5432, unknown loopback port, remote host without acknowledgement) | refused without connecting | PASS |
| Restored data differs from the baseline | `verify.sh` FAILS naming the differing categories; verified by a **negative control** (one ledger amount changed by 7 -> `equal_TABLE` and `invariants_all_zero` fail) and by reverting it (passes again) | PASS |
| Unencrypted dump requested | refused unless the synthetic-data acknowledgement is set | PASS |

## 8. NOT proven (read before relying on this)

- **Managed RDS behaviour**: automated backup/PITR mechanics and timing, snapshot restore, `latest_restorable_time` delay, parameter-group effects,
  whether the RDS master can create a `BYPASSRLS` role, pgaudit on RDS PostgreSQL 16, CloudWatch export. The Terraform is **unvalidated** here
  (`terraform` was not installed): `terraform fmt/validate/test` still have to be run (`aws-staging-deploy-plan.md` step 0.2).
- **Cross-region / cross-account copies**, snapshot sharing, **KMS** key policy and rotation, S3 storage of logical sets, a real backup runner identity and schedule, retention enforcement.
- **Production-size timing**: the numbers in section 6 are from a small synthetic dataset on a shared sandbox VM; production RTO is dominated by data volume, network/volume
  throughput, detection and decision time, none of which were measured. The extrapolation is an **estimate** and is labelled as such.
- **Real ENOSPC** mid-restore; **concurrent live traffic** during the dump beyond the snapshot semantics (the seed finished before the backup);
  a **multi-hour** `REPEATABLE READ` dump's effect on vacuum.
- The **application restarted** against a restored database (the drill exercises the platform's startup guard function and reconciliation, not a full `platform-api` boot).
- Signed backup manifests; backup-of-the-backup; automatic scheduling and alerting on backup failure or stale `latest_restorable_time`.
- The security review of ADR 0122 (probes in its section 9) is still to follow.

## 9. Running the drill

```sh
deploy/backup/drill.sh                     # full: ~4 tenants x 24 players, >=100k ledger entries (about half an hour on a busy VM)
deploy/backup/drill.sh --small             # smoke: ~1k entries, about 3 minutes
deploy/backup/drill.sh --skip-pitr         # without the physical PITR phase
go test ./deploy/backup/                   # static guards (cheap, no database)
```

Needs: PostgreSQL 16 server binaries at `/usr/lib/postgresql/16/bin`, `runuser` and the OS user `postgres` (the clusters run as `postgres`
via `runuser -u postgres`, a privilege **drop**; initdb refuses root), `age`/`age-keygen`, Go, ~1.5 GB free in `/tmp` (the drill checks and aborts
cleanly below its thresholds). It creates `/tmp/pgdrill-XXXXXX`, clusters on ports 5499 (source), 5498 (full-loss target), 5497 (PITR recovery),
stops them with `pg_ctl` and removes the directory on exit, failure, INT or TERM. It never contacts 5432, uses no sudo or pkill, creates roles only
inside its own clusters, and uses random per-run passwords that live only in 0600 passfiles inside the drill directory.
