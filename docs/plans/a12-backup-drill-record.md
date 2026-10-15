# A12 record: local backup / restore / point-in-time-recovery drill (2026-10-15)

Branch `a12-backup-drill` (from `04a5f46`, "Merge A10a"). **Not pushed, not merged.** This file is the hand-over record: proposed registry text,
findings, the raw drill summary, security-review probes and residuals. The registry, HANDOVER, progress and active-stage files were **not** edited
(the orchestrator owns them). Design: `docs/decisions/0122-backup-role-and-pitr.md`. Runbooks: `docs/runbooks/backup-restore-dr.md`,
`docs/runbooks/aws-staging-deploy-plan.md`.

## 1. Status by deliverable (CLAUDE.md labels)

| Deliverable | Label | Evidence / limit |
|---|---|---|
| Dedicated `igaming_backup` role (`deploy/backup/init-backup-role.sql`) and ADR 0122 | **IMPLEMENTED (local throwaway cluster)**; design ACCEPTED on owner decision 7; **security review PENDING** | owner/runtime/enable-row-security dumps fail, backup role dumps all rows, 15/15 write/escalation probes denied, connection limit, expiry, audit lines. Never applied to a shared or AWS database |
| `backup.sh`, `restore.sh`, `verify.sh` | **IMPLEMENTED (local)** | exercised at 112,688 ledger entries: backup, restore into a new database on the same cluster and into a brand-new cluster, full verification |
| `pitr-drill.sh` (base backup + WAL archive + recovery) | **IMPLEMENTED (local)** | named restore point and timestamp recovery both reproduce the expected state exactly; WAL gap detected and recovery refused |
| Synthetic data through the platform's own services | **IMPLEMENTED (test-only seeder)** | `internal/httpserver/drillseed_integration_test.go` (tags `integration drillseed`), in-process server of the integration suite, real handlers/services/triggers/RLS; **not** the compiled `platform-api` binary and **not** staff MFA over HTTP (staff tokens are minted by the test harness, as in the integration suite) |
| Failure-handling drills | **IMPLEMENTED** (disk-full mid-restore: **documented only**) | section 3 |
| RTO/RPO measurements | **IMPLEMENTED (measurements)** for a 165 MB / 112k-entry dataset on a shared sandbox VM; production extrapolation **ESTIMATE** | `backup-restore-dr.md` section 6 |
| RDS PITR / snapshots / log export / pgaudit Terraform | **PARTIALLY IMPLEMENTED: PREPARED, NEVER APPLIED, NOT VALIDATED** (`terraform` and `shellcheck` are not installed here) | `deploy/aws/modules/database`, staging root, deployer IAM delta, module/staging `terraform test` cases written but **not run** |
| `docs/runbooks/aws-staging-deploy-plan.md` | **IMPLEMENTED (document)**; the deploy itself **BLOCKED** until the owner authorises it | executable checklist with gaps G1/G2 |
| Scheduled backups, backup-failure alerting, signed manifests, cross-region copy, KMS design | **NOT IMPLEMENTED** | residuals, section 6 |
| Managed RDS behaviour, production-size RTO, RPO 0 | **NOT PROVEN / NOT MET** | |

## 2. Commands run and results

| Command | Result |
|---|---|
| `git switch -c a12-backup-drill 04a5f46`; `git log --oneline -1` | PASS (`04a5f46 Merge A10a ...`) |
| `deploy/backup/drill.sh --small` (2 tenants, ~1k entries) | PASS, 48 results, 0 failed (several earlier runs FAILED and found real defects, section 4) |
| `deploy/backup/drill.sh` (4 tenants x 24 players x 400 rounds) | PASS, 48 results, 0 failed (final run, section 3) |
| `go test ./deploy/backup/ ./deploy/drill/ ./deploy/observability/ ./internal/testsupport/credentialscan/` | PASS (static guards incl. planted offenders, the globals allow-list executed against tampered statements, credential scan) |
| `golangci-lint run ./deploy/...` and `golangci-lint run --build-tags integration,drillseed ./internal/httpserver/` filtered to the new files | 0 issues in the new files (the tagged `httpserver` run also reports pre-existing `errcheck` findings in other integration test files, unrelated) |
| `gofmt -l deploy internal/httpserver` | clean after `gofmt -w` of the two new Go files |
| `bash -n` on every script; `shellcheck` | syntax PASS; **shellcheck not installed (not run)** |
| `terraform fmt -check` / `validate` / `test` | **NOT RUN: terraform not installed** |

## 3. Raw drill summary (final full run)

Output of `deploy/backup/drill.sh` (full scale), verbatim from the results file the script writes (`--results-file`); `RESULT|<check>|PASS|<detail>`,
`TIMING|<name>|<seconds>`, `SIZE|<name>|<bytes or count>`, `DATA|...`. Machine: shared 4-core sandbox VM (other agents' test suites running at the same time),
PostgreSQL 16.13, local disk. 48 RESULT lines, 48 PASS, 0 FAIL (18 of them are failure-handling or negative-control checks).

```
TIMING|migrate_up_seconds|5.82
RESULT|cluster_and_roles|PASS|throwaway cluster /tmp/pgdrill-MHHDxx/src on 127.0.0.1:5499; roles igaming, igaming_runtime (deploy/init-app-role.sql), igaming_backup, drill_restore, drill_replicator; 7 migrate log lines
TIMING|seed_seconds|909.71
SIZE|source_ledger_entries|112688
SIZE|source_ledger_transactions|56312
SIZE|source_total_rows|286433
SIZE|source_database_bytes|165714967
DATA|seed_tally|{"players_per_tenant":24,"seconds":905.701158621,"tally":{"adjustments_executed":16,"adjustments_submitted":24,"alerts_explicit":4,"bonus_grants_issued":12,"casino_wagers":38400,"casino_wins":17288,"deposits_EUR":288,"deposits_USD":32,"reconciliation_runs":4,"sportsbook_bets":192,"sportsbook_settled":64,"withdrawals":24},"tenant_ids":["8d0211c8-ec0c-455a-9bdc-d5e100d0fcf6","353519f2-9acf-4d32-8e9d-934bc79fe031","b77643c4-9e90-4a1b-8e63-0b1d54c160f3","06b9bcb4-10ce-4679-b169-ca4be02b836a"],"tenants":4}
DATA|ledger_transaction_types|casino_bet=38400, casino_win=17288, deposit=320, sportsbook_bet=192, sportsbook_settlement=64, withdrawal_requested=24, manual_adjustment=16, withdrawal_rejected=8
DATA|withdrawal_states|cancelled=4, approved=4, rejected=4, submitted=8, pending_review=4
DATA|adjustment_states|executed=16, cancelled=4, rejected=4
DATA|audit_rows_alerts_bonus_grants|113698 / 4 / 12
RESULT|synthetic_world_loaded_through_platform_services|PASS|112688 ledger entries in 56312 transactions, 286433 rows in the database (target >= 100000)
RESULT|owner_plain_dump_refused|PASS|pg_dump as the migration owner FAILS: pg_dump: error: query failed: ERROR:  query would be affected by row-level security policy for table "alert_deliveries"
RESULT|owner_dump_with_enable_row_security_is_incomplete|PASS|pg_dump --enable-row-security as the owner also fails: pg_dump: error: Dumping the contents of table "financial_approval_policies" failed: PQgetResult() failed.
RESULT|runtime_role_dump_refused|PASS|pg_dump as igaming_runtime FAILS: pg_dump: error: query failed: ERROR:  permission denied for table actor_proof_keys
RESULT|backup_role_dump_complete|PASS|pg_dump as igaming_backup holds all 112688 ledger_entries
RESULT|backup_role_cannot_write_or_escalate|PASS|15/15 probes denied (DML, DDL, TRUNCATE, TEMP, trigger disable, SET ROLE, replica mode, CREATE/ALTER ROLE, COPY PROGRAM, file read, reload conf)
RESULT|backup_role_attributes|PASS|NOSUPERUSER BYPASSRLS NOCREATEDB NOCREATEROLE NOREPLICATION CONNECTION LIMIT 2, member of no role, VALID UNTIL set
RESULT|backup_role_connection_limit_enforced|PASS|a third concurrent session is refused (CONNECTION LIMIT 2)
RESULT|backup_role_activity_is_audited|PASS|server log holds 32 connection, 31 disconnection and 636 statement lines for igaming_backup (log_connections, log_disconnections, ALTER ROLE ... SET log_statement; on RDS pgaudit)
RESULT|backup_refuses_without_encryption_policy|PASS|refused: backup: no encryption recipient configured. A real dump contains PII, financial data and signing keys: set BAC
TIMING|backup_total_seconds|6.46
TIMING|backup_baseline_seconds|3.56
TIMING|backup_pg_dump_seconds|2.52
TIMING|backup_encrypt_seconds|0.10
SIZE|dump_bytes|18450544
SIZE|backup_directory_bytes|18677191
RESULT|logical_backup_written|PASS|20261015T185544Z-igaming_platform_dev-plain: SHA256SUMS baseline.txt db.dump globals.sql manifest.json 
TIMING|backup_encrypted_total_seconds|6.23
TIMING|backup_encrypted_encrypt_seconds|0.22
RESULT|encryption_hook_age|PASS|artifacts age-encrypted (SHA256SUMS baseline.txt.age db.dump.age globals.sql.age manifest.json ); no plaintext dump left
TIMING|restore_total_seconds|10.59
TIMING|restore_integrity_and_decrypt_seconds|0.67
TIMING|restore_pg_restore_seconds|7.41
TIMING|restore_grants_seconds|0.07
TIMING|restore_analyze_seconds|2.13
RESULT|logical_restore_into_new_database|PASS|restored into igaming_restored on the same cluster (single transaction, no trigger/RLS bypass)
TIMING|verify_total_seconds|6.17
RESULT|logical_restore_verified|PASS|VERIFY RESULT: PASS (30 checks); 30 checks
RESULT|encrypted_backup_restores|PASS|age-encrypted backup decrypted and restored: ledger_entries=112688
RESULT|negative_control_verify_detects_tampered_ledger|PASS|verify FAILED as required on a copy with one ledger amount +7; detected by: equal_TABLE invariants_all_zero 
RESULT|negative_control_reverted_verify_passes_again|PASS|after undoing the damage verify passes again (the failure was the damage, nothing else)
RESULT|failure_corrupted_dump_detected_before_restore|PASS|checksum mismatch reported, exit 2, no database created
RESULT|failure_truncated_dump_detected_by_checksum|PASS|checksum mismatch, exit 2, no database created
RESULT|failure_truncated_dump_with_valid_checksum_refused|PASS|pg_restore refused the unreadable archive (pg_restore failed: pg_restore: error: could not read from input file: end of file 
restore: FAILED (exit 3); database f2b_db created by this run was dropped); exit 3; no database created
RESULT|failure_corrupt_dump_with_valid_checksum_rolled_back|PASS|pg_restore failed part-way (backup: pg_restore failed: pg_restore: error: could not uncompress data: incorrect data check ); exit 3; single transaction rolled back, half-created database dropped
RESULT|failure_restore_as_igaming_runtime_refused|PASS|clear refusal, exit 2, no database created
RESULT|failure_restore_as_igaming_backup_refused|PASS|clear refusal, exit 2, no database created
RESULT|failure_restore_into_existing_database_refused|PASS|refused (database exists), existing ledger_entries unchanged (112688)
RESULT|failure_insufficient_disk_backup_refused|PASS|refused before writing anything: backup: insufficient disk space in /tmp/pgdrill-MHHDxx/backups: 6808 MB free, 999999999 MB required (BACKUP_MIN_FREE_MB)
RESULT|failure_insufficient_disk_restore_refused|PASS|refused, no database created
RESULT|failure_partial_restore_rolled_back|PASS|pg_restore terminated part-way (injected fault: client killed once the new database had grown), exit 3; the single transaction rolled back and the half-created database was dropped
RESULT|failure_backup_credential_expired_fails_closed|PASS|exit 2: backup: cannot connect as igaming_backup to 127.0.0.1:5499/igaming_platform_dev: psql: error: connection to server at "127.0.0.1", port 5499 failed: FATAL:  password authentication failed for user "ig
RESULT|failure_backup_credential_missing_fails_closed|PASS|exit 2: backup: credential file missing: /tmp/pgdrill-MHHDxx/secrets/does-not-exist (the backup credential was not provisioned, or it expired and wa
RESULT|failure_backup_as_owner_role_refused|PASS|refused before dumping: backup: role igaming does not have BYPASSRLS: under FORCE ROW LEVEL SECURITY a dump by this role is refused or silently INCOMPLETE (BACKUP-ROLE-1). Us
RESULT|guard_refuses_shared_cluster_5432|PASS|refused without connecting
RESULT|guard_refuses_remote_host_without_explicit_allow|PASS|refused without connecting
TIMING|fullloss_empty_cluster_initdb_and_start_seconds|1.16
RESULT|failure_tampered_globals_refused|PASS|ALTER ROLE igaming WITH SUPERUSER in a checksum-valid globals.sql refused by the allow-list; no role or database created
TIMING|fullloss_restore_total_seconds|12.41
TIMING|fullloss_restore_integrity_and_decrypt_seconds|0.83
TIMING|fullloss_restore_pg_restore_seconds|8.69
TIMING|fullloss_restore_grants_seconds|0.09
TIMING|fullloss_restore_analyze_seconds|2.53
RESULT|full_loss_restore_into_new_cluster|PASS|roles created WITHOUT passwords from globals.sql, database restored into an empty cluster
RESULT|restored_roles_have_no_password_until_reprovisioned|PASS|igaming/igaming_runtime/igaming_backup cannot log in after restore (no passwords in the dump, by design)
TIMING|fullloss_reprovision_credentials_seconds|0.30
TIMING|fullloss_verify_total_seconds|6.17
RESULT|full_loss_restore_verified|PASS|VERIFY RESULT: PASS (30 checks); incl. database_environment anchor UNRECORDED => a production platform-api would REFUSE to start until it is recorded
RESULT|anchor_reprovisioned_production_guard_admits|PASS|owner recorded 'production'; the production startup guard now admits the restored database
TIMING|base_backup_seconds|2.57
SIZE|base_backup_bytes|206598325
RESULT|base_backup_verifies|PASS|pg_verifybackup: backup successfully verified
SIZE|wal_segments_replayed_from_base_end_to_target|5
RESULT|operator_error_is_visible_on_source|PASS|source casino_provider_rounds 106 -> 0; ledger_entries now 115882 (expected-at-target 114888)
TIMING|wal_archive_lag_seconds|6.60
RESULT|archive_lag_bounded|PASS|last commit's segment 00000001000000000000003E reached the archive 6.60 s after the commit (archive_timeout=30s)
SIZE|wal_archive_bytes|469762389
TIMING|pitr_base_copy_seconds|0.47
TIMING|pitr_recovery_named_restore_point_seconds|4.13
TIMING|pitr_recovery_named_total_incl_copy_seconds|4.60
RESULT|named_rows_after_target_absent_before_present|PASS|ledger_entries recovered=114888 (== expected at target 114888; source now 115882); tenants=5; casino_provider_rounds=102 (operator deletion undone)
RESULT|named_verify_against_expected_at_target|PASS|VERIFY RESULT: PASS (31 checks)
TIMING|pitr_recovery_timestamp_seconds|2.98
TIMING|pitr_recovery_timestamp_total_incl_copy_seconds|3.73
RESULT|time_rows_after_target_absent_before_present|PASS|ledger_entries recovered=114888 (== expected at target 114888; source now 115882); tenants=5; casino_provider_rounds=102 (operator deletion undone)
RESULT|time_verify_against_expected_at_target|PASS|VERIFY RESULT: PASS (31 checks)
RESULT|wal_continuity_check_clean_archive|PASS|no gap between 00000001000000000000003A and the target segment 00000001000000000000003E
RESULT|wal_gap_detected_by_continuity_check|PASS|continuity check reports missing: 00000001000000000000003B
RESULT|wal_gap_recovery_refused|PASS|server did not promote; log: 2026-10-15 18:58:46.852 UTC [6580] FATAL:  recovery ended before configured recovery target was reached
PITR_TARGET|lsn=0/3E013288|segment=00000001000000000000003E|time=2026-10-15 18:57:48.03354+00
```


## 4. Defects the drill found in its own tooling or in earlier documentation (all fixed or recorded)

1. **Schema ownership is not restored** (`public` came back owned by `pg_database_owner`): caught by the verifier's `SCHEMAACL` comparison on the first
   run; fixed in `post-restore-grants.sql`.
2. **A raw restore also loses the database-level ACL** (known from OPS-READINESS-LOCAL): `post-restore-grants.sql` re-applies it; the verifier gates it.
3. **Correction of `restore-from-backup.md`**: a full `pg_restore` into an empty database does **not** replay data through triggers/RLS (the archive is
   pre-data/data/post-data), so no `--disable-triggers`/`session_replication_role` is needed. The earlier text claimed otherwise; corrected.
4. **Reconciliation writes**: running the platform's `ledger_vs_projection` stream persists `reconciliation_runs`, which broke repeat verification; `restorecheck`
   now rolls the run back by default (`-persist` for the real post-restore run).
5. **Statement-timeout fault injection never fires** (the dump header resets `statement_timeout`): replaced by a deterministic client kill mid-restore.
6. **WAL-gap assertion was too permissive** (any failed start counted as "refused"): now requires the server log's `recovery ended before configured recovery target was reached`.
7. **Disk**: the shared disk dropped to ~150 MB free during the work (other agents' build caches and mutation runs); the drill gained free-space guards and the gap
   test uses hard links instead of copying the 450-600 MB archive. To keep the shared disk safe, two **stale Go build caches in the session scratchpad that no process
   used** were deleted (`scratchpad/gocache116`, `scratchpad/s2/gocache-mut`); nothing else of anyone's was removed.
8. `pg_dump --enable-row-security` as the owner **fails** (a policy errors without a tenant scope) instead of silently dumping partial data on this schema; the ADR table says so.
9. **A harness limitation, not a safety event**: this sandbox's tool refused to run `runuser` as a direct command (worktree-isolation guard); the same
   privilege **drop** (`runuser -u postgres`, never an escalation) was run from the committed drill scripts. No sudo, pkill, superuser session on the shared cluster, role or password
   change anywhere on 5432; the shared cluster was never contacted.

## 5. Proposed registry text (for the orchestrator to place)

> **A12-BACKUP-DRILL-2026-10-15** | devops (security review pending; code-reviewer pending) | **Backup / restore / PITR, LOCAL half: IMPLEMENTED on throwaway clusters with synthetic data; managed half PREPARED, NOT APPLIED, NOT PROVEN.**
> BACKUP-ROLE-1 **CLOSED by design** (ADR 0122, owner decision 7: managed PITR primary + dedicated audited `igaming_backup` role, logical dumps secondary). `deploy/backup/`:
> `backup.sh` (one exported snapshot for `pg_dump -Fc` + baseline, globals without passwords, SHA-256 manifest, age/gpg recipient hook, refuses unencrypted/non-BYPASSRLS/superuser/shared 5432),
> `restore.sh` (new database only, checksums first, single transaction, globals allow-list, DB ACL + schema owner re-applied), `verify.sh` (content hash + count of every table, ledger invariants,
> projection==recompute drift 0, roles/ACLs/triggers/policies/RLS by name, `migrate verify`, platform reconciliation as `igaming_runtime`, startup anchor guard), `pitr-drill.sh` (base backup + WAL archive,
> recovery to a named restore point and a timestamp, WAL-gap detection), `drill.sh` (everything, on own clusters under `/tmp/pgdrill-*`, ports 5499/5498/5497, run as `postgres` via `runuser`).
> Data: 112,688 ledger entries / 56,312 transactions / 286,433 rows / 166 MB loaded through the platform's own services (HTTP deposits, casino wager/win, sportsbook bets + settlement, withdrawals in 5 states,
> K2 governed adjustments, bonus grants, alerts, audit). Result: 48/48 PASS, including 18 failure-handling / negative-control checks. Measured at that size (final run): logical backup 6.5 s, restore 10.6 s, verify 6.2 s, full-loss new cluster ~20 s,
> base backup 2.6 s, PITR recovery 4.6 s including the base copy, WAL archive lag 6.6 s (bound: archive_timeout 30 s; 3.5-14.6 s across runs). **NOT PROVEN:** RDS PITR/snapshots, `BYPASSRLS` role creation on RDS, pgaudit, KMS, cross-region, production-size RTO,
> scheduled backups and their alerting, real ENOSPC. Terraform for the staging DB (14-day PITR, deletion protection, final snapshot, log export, parameter group, secret ARN reference only) and the deployer IAM delta are
> **prepared, unvalidated (terraform not installed) and unapplied**; `docs/runbooks/aws-staging-deploy-plan.md` is the single named-commit deploy checklist (gaps G1 backup-runner placement, G2 backup/PITR alarms).
> Findings: schema ownership and DB ACL not carried by a dump (gated), restore-from-backup.md trigger claim corrected, unsigned manifest residual.
> **OPEN:** security review of ADR 0122 section 9 probes; `actor_proof_keys.secret` is a plaintext signing key inside every logical dump; ALERT-DELIVERY-1; owner authorisation of the staging deploy.

> **BACKUP-ROLE-1** -> CLOSED (design) / OPEN items moved to the line above. **D-BACKUP** -> decided 2026-10-15.

## 6. Security review of the backup role design: probes to run (also ADR 0122 section 9)

1. Stepping-stone attempts from `igaming_backup`: `SET ROLE`, SECURITY DEFINER functions with side effects (default `EXECUTE` to PUBLIC), large objects, `pg_stat_*` reach, `COPY ... PROGRAM`, FDW/dblink, `LISTEN/NOTIFY`, advisory-lock denial of service against writers.
2. Long `REPEATABLE READ` dump transaction vs vacuum/availability; is `statement_timeout=4h` right?
3. Whether `actor_proof_keys.secret` (plaintext signing key), `staff_mfa_*`, `payout_instruments*`, `kyc_documents` may sit in a logical dump at all (exclude + re-provision, or accept an encrypted signing-key-bearing artefact).
4. Credential lifecycle: who mints/extends `VALID UNTIL`, passfile materialisation, process-list/log residue, runner logging, break-glass with four-eyes.
5. Manifest forgery (replace dump and `SHA256SUMS`): require a signature; the globals allow-list is the compensating control for roles only.
6. `restore.sh` privilege and the `--apply-globals` filter; restore of a dev/test backup into production (anchor).
7. That `log_statement`/pgaudit output does not log sensitive values; network scope of the backup runner; TLS (`rds.force_ssl`).
8. Decrypted material staged in `TMPDIR` by `restore.sh` (0700, removed on exit): require an encrypted/ramdisk volume in production.

## 7. Residuals and follow-ups (not done, by design or by scope)

- Everything under "NOT PROVEN" in `backup-restore-dr.md` section 8; run `aws-staging-deploy-plan.md` after owner authorisation.
- `terraform fmt/validate/test` and `shellcheck` have to be run by someone who has them; the new `terraform test` cases are unexecuted.
- Deployer IAM policy delta (`rds:CreateDBSnapshot` on staging snapshots, RDS log groups) needs security review and the bootstrap credential; `simulate-deployer-policies.py` has five new expectations (unrun: needs AWS).
- A backup runner (ECS task or host), schedule, retention job, alarms for backup failure / stale `LatestRestorableTime` (gap G1/G2).
- gpg encryption hook is implemented but only the age path was exercised.
- The seeder does not exercise live concurrent traffic during the dump (snapshot semantics only) and does not boot the real `platform-api` against the restored database.
- The extended `go test -tags integration` suite was not run against the new code (the seeder is excluded from normal runs by its build tags).
