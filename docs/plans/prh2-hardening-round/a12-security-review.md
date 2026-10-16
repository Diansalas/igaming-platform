# A12 security review: backup role, logical backup/restore, PITR drill, staging Terraform/IAM (ADR 0122)

- Reviewer: `security` specialist. Date: 2026-10-15.
- Subject: branch `a12-backup-drill`, commit `d606e1c` (base `04a5f46`).
- Scope: ADR 0122; `deploy/backup/*` (`backup.sh`, `restore.sh`, `verify.sh`, `pitr-drill.sh`, `drill.sh`, `seed.sh`,
  `init-backup-role.sql`, `baseline.sql`, `post-restore-grants.sql`, `restorecheck/`, `lib-*.sh`, static test); the drill seeder
  `internal/httpserver/drillseed_integration_test.go`; `docs/runbooks/backup-restore-dr.md` and `aws-staging-deploy-plan.md`; the
  prepared changes under `deploy/aws` (database module, staging root, deployer IAM policy, simulator cases).
- Method: code and design review, plus live probes against the drill's **own throwaway cluster** (127.0.0.1:5499) while
  `deploy/backup/drill.sh --small` ran. The shared cluster on 5432 was not contacted. No role or password was changed anywhere. No
  sudo, no pkill, no AWS call, no terraform. `terraform` and `shellcheck` are not installed, so the Terraform review is static only.
- What this review is not: a penetration test, a certification-grade audit, or proof of RDS behaviour. Nothing here was run on RDS.

## Verdict

| Area | Verdict |
|---|---|
| Local drill tooling, run on synthetic data in throwaway clusters | **ACCEPTABLE.** I re-ran the drill independently: `drill.sh --small` gave 48 PASS / 0 FAIL. |
| `igaming_backup` role design (ADR 0122 section 3) | **ACCEPTED WITH CONDITIONS.** No privilege escalation was found. Several abuse primitives remain open to a stolen credential (M1, M8). |
| `backup.sh` / `restore.sh` on any non-synthetic data (staging with real data, production) | **BLOCKED.** H1, H2 and H3 must be fixed first. |
| Production restore from a logical backup | **BLOCKED** until the backup set is signed and verified before restore (H3). This was "an open hardening item"; it is now a **required control**. |
| Staging Terraform/IAM delta (prepared, not applied) | **ACCEPTABLE FOR STAGING** (static review only). M6 must be resolved before the same pattern goes to production. |

**Launch-blocking flags** (for the orchestrator to pass to the human): H1, H2 and H3 block any logical backup or restore of real data.
M2 blocks running `init-backup-role.sql` on RDS while statement-level logging or pgaudit is on for the minting role.

## Findings

### H1. HIGH: encryption failure is fail-open. `backup.sh` exits 0 with a plaintext dump and a manifest that says it is encrypted

`backup.sh` lines 137-144 use `age ... && rm -f <plaintext>`. Under `set -e`, a failing command on the left of `&&` does not stop
the script. A typo in `BACKUP_ENCRYPT_AGE_RECIPIENT`, a wrong gpg id, a missing key or ENOSPC during encryption therefore leaves
`db.dump` in plaintext. `SHA256SUMS` then covers the plaintext file, `manifest.json` records `"encryption": "age"`, and the script
exits 0. `db.dump` contains every tenant's PII and `actor_proof_keys.secret`. Nothing checks the recipient before the dump is taken.

Reproduction (verbatim copy of the encryption block, recipient `age1typo`, in a scratch directory):

```
age: error: malformed recipient "age1typo" ... (x3)
encryption=age dump_bytes=22
SCRIPT REACHED END (backup.sh would exit 0)     rc=0
run/: SHA256SUMS baseline.txt db.dump globals.sql     (db.dump = plaintext)
```

The gpg loop has the same defect.

Fix:
1. Validate the recipient during preflight. Encrypt a probe with `age -r` and fail if that fails.
2. Make every encryption step fail closed, e.g. `age ... || bk_die ...`, then `rm`.
3. After encryption, assert that no plaintext `db.dump`, `globals.sql` or `baseline.txt` exists before writing `SHA256SUMS`.
4. Preferably stream, so no plaintext ever reaches the backup volume:
   `pg_dump -Fc ... | tee >(sha256sum >plain.sha) | age -r ... > db.dump.age`.
5. Add a drill case for a bad recipient.

### H2. HIGH: plaintext dump left on disk on abnormal exit. Logical backups longer than about 30 minutes fail, leaving that plaintext behind

The plaintext dump is written to `RUN_DIR` first and encrypted afterwards. Cleanup depends on `trap fail_cleanup EXIT`. Bash does
**not** run the EXIT trap when it is killed by SIGPIPE, SIGKILL or the OOM killer. There is also a concrete trigger for SIGPIPE:

- The snapshot-holder `psql` session connects as `igaming_backup`, which has `idle_in_transaction_session_timeout = 30min`.
- That session sits idle in a transaction for the whole time `pg_dump` runs.
- If `pg_dump` takes longer than 30 minutes (the record's own estimate is about 40 minutes for a 50 GB database), the server
  terminates the holder session.
- `echo "COMMIT;" >&7` then writes to a FIFO that has no reader. Bash dies with SIGPIPE (exit 141) and the EXIT trap does not run.
- The run directory is left containing a **plaintext** `db.dump` and `baseline.txt`, with no `SHA256SUMS`.

Reproduction of the shell behaviour: a writer to a FIFO whose reader was killed, inside `set -euo pipefail` with an EXIT trap,
gives outer rc=141 and the trap output is never printed. I did not run the full path against a 30-minute dump; the trigger is
inferred from the role settings and the extrapolated timings.

Fix:
- Stream-encrypt (H1 item 4) so no plaintext is ever on disk.
- Close the holder transaction as soon as `pg_dump` has imported the snapshot, or set `idle_in_transaction_session_timeout = 0`
  for the holder session only.
- `trap '' PIPE` and handle EPIPE explicitly. Trap INT, TERM and HUP as well.
- Have the runner sweep orphaned run directories that have no `SHA256SUMS` (shred, then alert).
- `restore.sh` has the same residue problem for decrypted material in `$WORK` (see L9).

### H3. HIGH: an unauthenticated archive is replayed by a superuser / rds_superuser-member restore session. The globals allow-list does not bound this

Only `SHA256SUMS` protects the backup set, and it is unsigned. Anyone who can write to backup storage can therefore replace
`db.dump`, `globals.sql`, `baseline.txt` and `SHA256SUMS` together. The `--apply-globals` allow-list (`lib-globals.sh`) filters role
statements, but `db.dump` is itself executable SQL:

- `pg_restore` runs every TOC entry's definition verbatim.
- An attacker can also plant a function plus an expression index or CHECK constraint that runs during the restore.
- `--role=igaming` only issues `SET ROLE`. A planted entry can `RESET ROLE` back to the session user.

Who that session user is decides the damage:

- Full-loss path (runbook 4.1, `drill.sh` phase 4): the restore user is the bootstrap superuser `postgres`. The archive therefore
  gets superuser rights: `ALTER ROLE ... SUPERUSER`, `CREATE ROLE ... BYPASSRLS`, and `COPY ... TO PROGRAM`, which is OS command
  execution as the database server user.
- RDS (`aws-staging-deploy-plan.md` 9.2): the restore user is the master user `igaming`, a member of `rds_superuser`. The archive can
  re-password `igaming_runtime` and create roles.

`verify.sh` cannot detect this. Its reference `baseline.txt` comes from the same unsigned set, so it is attacker-controlled too.
Gate 2 (`deploy/drill/verify.sql`, from the repository) is the only independent check, and it covers only part of the schema.

I did not execute a malicious archive. This finding rests on documented `pg_restore` semantics.

Fix (required before any production restore):
1. **Sign the backup set.** Use a KMS asymmetric signature or minisign/age-plugin over `SHA256SUMS` plus `manifest.json`, made with a
   key held only by the backup runner. `restore.sh` must verify the signature against a pinned public key **before** checksumming,
   decrypting or touching any database, and must refuse an unsigned set except on loopback drill ports.
2. **Never run `pg_restore` as a superuser or rds_superuser member.** On a new cluster, create the project roles first from the
   repository's own scripts (`deploy/init-app-role.sql`, `init-backup-role.sql`), not from `globals.sql`. Then restore as a
   NOSUPERUSER CREATEDB role that is a member of the owner. Refuse `R_SUPER=1` in `restore.sh`, with a drill-only override.
3. Keep the globals allow-list as defence in depth, and drop the `ALTER ROLE ... SET` replay (see L4).

### M1. MEDIUM: abuse primitives a stolen `igaming_backup` credential still has (verified on the throwaway cluster)

No escalation path was found. These, however, all **succeeded** as `igaming_backup`:

| Probe | Result | Impact |
|---|---|---|
| `ALTER ROLE igaming_backup PASSWORD '...'` (inside `BEGIN READ WRITE`) | allowed | Attacker locks the backup runner out, so backups stop, until the next re-mint. |
| `ALTER ROLE igaming_backup SET default_transaction_read_only = off` / `SET statement_timeout = 0` | allowed | Persistently undoes the defence-in-depth defaults. `backup.sh` preflight only catches the read-only one. |
| `SET default_transaction_read_only = off`; `SET statement_timeout = 0`; `SET idle_in_transaction_session_timeout = 0` | allowed | The role defaults are an accident guard only, not a control. |
| `pg_advisory_lock(k)` / `pg_advisory_xact_lock(...)` in a read-only session | allowed | The platform's lock keys are deterministic (`internal/db/migrate.go` migration lock; `hashtextextended('reconciliation:'...)`, `sb_exposure:...`, `tenant_status_gate_key(...)`, `risk_cumulative`). A session-level lock held by an idle session never times out, so the attacker can stall migrations, withdrawals for a tenant, sportsbook bets on an event, or reconciliation. |
| `pg_logical_emit_message(...)` in a **read-only** transaction | allowed (returned an LSN) | Unbounded WAL generation, filling disk and archive (on RDS, storage-full means an outage). |
| `lo_create` / `lo_from_bytea` in `BEGIN READ WRITE` | allowed | Persistent large objects owned by the role (storage DoS; they also land in every later dump). |
| `LOCK TABLE ledger_entries IN ACCESS SHARE MODE` | allowed (SHARE mode denied) | Can hold up DDL, and every query then queues behind the waiting DDL. |
| `NOTIFY` / `LISTEN` | allowed | Harmless today: the platform uses no LISTEN channel. It becomes a risk if one is added. |
| CONNECT on `postgres` and `template1` | true (PUBLIC default) | Can connect to other databases on the cluster. |

Denied, as designed: `SET ROLE`, `VALID UNTIL`, `CONNECTION LIMIT`, `log_statement` changes, `CREATE SCHEMA`/`TEMP`/`CREATE` in
public, `pg_read_*`/`pg_ls_dir`/`pg_stat_file`, `COPY TO` file, `pg_cancel_backend` of other roles, every DML/DDL, and the 15 drill
probes. **None of the 21 SECURITY DEFINER functions is executable by the role** (`has_function_privilege` = false for all; PUBLIC
is revoked on every one). The role has no role memberships. The only extensions are `plpgsql` and `pgcrypto`: no dblink, no FDW.

Fix:
- **Database level:** in the platform database, `REVOKE EXECUTE ... FROM PUBLIC` on `pg_logical_emit_message(*)`, the
  `pg_advisory_*` family, `lo_create`, `lo_from_bytea`, `lo_import`/`lo_export` variants and `pg_notify`, then grant back to
  `igaming` and `igaming_runtime` only. This needs the function owner (bootstrap superuser); feasibility on RDS is UNVERIFIED and
  must be recorded.
- **Monitoring:** alert on any `pg_locks` row held by `igaming_backup` other than AccessShare, and on `age(backend_xmin)` and
  `state_change` for its sessions.
- **Connections:** `REVOKE CONNECT ON DATABASE postgres, template1 FROM PUBLIC`, or restrict in pg_hba.
- **Credential:** move the role to RDS IAM database authentication (see M8), so a self-password-change cannot lock the runner out.
- **Drill:** add these probes to the drill and to deploy-plan 5.3.

### M2. MEDIUM: `init-backup-role.sql` sends the cleartext password in statement text that logging can capture

- `SELECT set_config('igaming.backup_password', :'backup_password', false)` puts the password literal in a top-level statement.
- The `EXECUTE format('... PASSWORD %L ...')` inside the DO block sends a cleartext `ALTER/CREATE ROLE ... PASSWORD` substatement.

Either is written to the server log whenever the **minting** session is logged: `log_statement = all`,
`log_min_duration_statement = 0` (a common RDS slow-query setting), or pgaudit with READ/MISC/ROLE classes for the master user.
pgaudit logs substatements and does not promise to redact passwords. Those lines are then exported to CloudWatch, where the
deployer role holds `logs:GetLogEvents`/`FilterLogEvents` (M6). The exception handler that hides SQLERRM is good, but it does not
cover this path. Static finding; not reproduced with logging on.

Fix:
- Compute a SCRAM-SHA-256 verifier on the client (the runner, or `psql \password`, which hashes client-side) and pass only the
  verifier, through a psql variable used directly in `ALTER ROLE ... PASSWORD :'verifier'`. No `set_config` literal.
- Document "minting session must not be statement-logged" in deploy-plan 5.1.
- Preferably remove the password entirely (M8).

### M3. MEDIUM: `restore.sh --owner-role` accepts any role, including a superuser or `igaming_backup`

The guard checks only `current_user` against runtime/backup. It never checks the owner role's attributes.

- `--owner-role postgres` (when run as a superuser): every restored object, including the SECURITY DEFINER functions
  (`actor_proof_require`, `aml_*`), becomes superuser-owned. Definer code then runs as superuser.
- `--owner-role igaming_backup`: the BYPASSRLS backup role becomes the owner of the ledger (it can ALTER, DROP, TRUNCATE and
  disable triggers).

Static finding.

Fix: require the owner role to be NOSUPERUSER, NOBYPASSRLS and NOT `igaming_runtime`/`igaming_backup`, and `= igaming` unless an
explicit, logged override is given.

### M4. MEDIUM: the environment anchor is not checked on restore, and the runbook turns the production guard into a rubber stamp

`database_environment` is ordinary table data in the dump. Its write-once trigger is post-data, so the archive decides the anchor.
`restore.sh` neither reads nor compares it, and `baseline.sql` does not record it.

Runbook section 5 tells the owner to `INSERT ... 'production'` after a restore. So a backup with an unrecorded anchor (a dev or
staging copy, or a tampered set) restored into production passes the startup guard after a routine step. That puts synthetic
accounts and any known test staff passwords into production.

Fix:
- `baseline.sql` emits `META|environment_anchor|...`, and the manifest carries it.
- `restore.sh` requires `RESTORE_EXPECT_SOURCE_ENVIRONMENT` to equal it, and refuses to restore an unrecorded, development or test
  source into a non-drill target.
- The runbook must say: record `production` only if the source anchor was `production`.

### M5. MEDIUM: the target guard is bypassed by loopback aliases (needs the remote acknowledgement)

`bk_is_loopback` recognises only `127.0.0.1`, `localhost` and `::1`. Every other alias is treated as a remote host, and a remote
host is allowed once `BACKUP_ALLOW_REMOTE_TARGET` equals it. I tested the guard function only (no connection was made). With
port 5432, the following were all **allowed**:

`LOCALHOST`, `127.0.0.2`, `127.1`, `0.0.0.0`, `0`, `::ffff:127.0.0.1`, `0:0:0:0:0:0:0:1`, `localhost.`, `[::1]`, this machine's
hostname `vm` (which resolves to 127.0.0.1), and libpq multi-host `db.example,127.0.0.1`.

So "127.0.0.1:5432 is always refused" does not hold. One plausible acknowledgement (`BACKUP_ALLOW_REMOTE_TARGET=vm`) reaches the
shared cluster. Given incident 2026-09-27, this matters even though an acknowledgement is required.

Fix:
- Reject `,` in the host (libpq multi-host).
- Resolve the host (`getent ahosts`) and treat any address in 127.0.0.0/8, `::1`, `0.0.0.0`, `::` or a local interface address as
  loopback, subject to the drill-port rule.
- Refuse port 5432 on any address that resolves locally.
- Add these cases to `TestTargetGuard`.

### M6. MEDIUM (production pattern; acceptable for staging): the deployer can erase or read the audit trail and remove recovery points

- `LogsStagingOnly` now covers `/aws/rds/instance/igaming-staging-*` with `logs:DeleteLogGroup`, `DeleteRetentionPolicy`,
  `PutRetentionPolicy`, `GetLogEvents` and `FilterLogEvents`. The deployer can therefore delete, or set to 1-day retention, the log
  group that carries the backup role's connection and pgaudit lines, and can read anything leaked into it (M2).
- `rds:ModifyDBInstance` plus `rds:DeleteDBInstance` on `db:igaming-staging-*` let the deployer set `BackupRetentionPeriod = 0`
  (deletes automated backups and PITR) or delete with `--skip-final-snapshot --delete-automated-backups`. The Terraform
  preconditions bind only Terraform, not the IAM principal.

The additions themselves are tightly scoped: `rds:CreateDBSnapshot` only on `snapshot:igaming-staging-*` (the db ARN was already
listed); log groups only under the staging prefix; no new `*` resource; `RestoreDBInstanceToPointInTime` is not granted (and a
simulator case asserts that). For staging, accept.

Before production:
- Audit log groups go in a separate account or behind a resource policy the deployer cannot change, with a CloudWatch Logs data
  protection policy to mask secrets.
- Immutable recovery points: AWS Backup with vault lock and cross-account copy, or an SCP denying `DeleteDBInstance` without a final
  snapshot and denying retention 0.
- The deployer has no read access to the audit logs.

Also noted (functional, not security): if `db_kms_key_arn` points to a CMK, the deployer needs `kms:DescribeKey`/`CreateGrant` on
it, which the policy does not grant.

### M7. MEDIUM (availability): a long REPEATABLE READ dump on the primary, and what `statement_timeout = 4h` does and does not bound

- `statement_timeout` limits each statement, not the dump. The snapshot (and the vacuum xmin horizon on the primary) is held for
  the whole `pg_dump`. That is about 40 minutes at 50 GB by the record's extrapolation, and unbounded if the role holder overrides
  the timeouts (M1).
- Frequently-updated tables (sessions, outbox claim tokens, withdrawal/adjustment state machines) bloat during that window.
- `4h` is reasonable as an accident guard per statement; it is not a DoS control.

Recommendation:
- Take logical dumps from a short-lived instance restored from PITR or a snapshot (no impact on the primary, and no BYPASSRLS
  credential on the primary at all), or from a replica.
- Alert on `age(backend_xmin)` above N minutes for `igaming_backup`.
- Fix the 30-minute holder bug (H2).

### M8. MEDIUM (design): the claim "a BYPASSRLS credential is useless from elsewhere" is not achievable on RDS as written

RDS has no editable pg_hba, and security groups are per instance, not per role. Every principal that can reach the database (the
application tasks) can use a stolen backup password.

Recommendation: for RDS, use **IAM database authentication** for `igaming_backup` (`GRANT rds_iam`, no password). Login then needs
a 15-minute token signed by the runner's IAM identity, logged in CloudTrail, and scoped by an `rds-db:connect` policy on that one
db user. This replaces the passfile/VALID UNTIL mechanism on RDS and removes M2 and the self-password lockout. Record this in
ADR 0122 section 3.

### Low / informational

- **L1. `VALID UNTIL` has no upper bound.** `BACKUP_ROLE_VALID_UNTIL` accepts `infinity` or a date years ahead. Reject anything
  beyond now + 48 h in `init-backup-role.sql`.
- **L2. The unencrypted override is honoured on any target.** `BACKUP_ALLOW_UNENCRYPTED=synthetic-data-only` works against a remote
  host too. Honour it only for loopback drill ports.
- **L3. The "no password in env, argv or URL" claim is not true for `verify.sh`.** It requires `VERIFY_OWNER_DATABASE_URL` and
  `VERIFY_RUNTIME_DATABASE_URL`; the drill builds them with embedded passwords, and on RDS the owner is the master user. pgx honours
  `PGPASSFILE`, so pass password-less URLs and passfiles instead.
- **L4. Tampered `globals.sql` content that the allow-list accepts.** For `igaming_backup`: `log_statement TO 'none'`,
  `default_transaction_read_only TO 'off'`, `statement_timeout TO '0'`, `CONNECTION LIMIT -1`, `VALID UNTIL '9999-12-31'`.
  For `igaming`/`igaming_runtime`: any allow-listed SET. Re-running `init-backup-role.sql` neutralises the backup-role values; the
  others persist. Stop replaying `ALTER ROLE ... SET` (re-provision from repository scripts) and pin `CONNECTION LIMIT 2` for
  `igaming_backup`. (I checked the filter for psql meta-commands, `;` chaining, CR, quote smuggling, a missing trailing newline and
  token duplication: none passes.)
- **L5. Backup role can connect to other databases.** See M1 (CONNECT on `postgres`/`template1` via PUBLIC).
- **L6. Break-glass and minting authority are undocumented.** The four-eyes break-glass procedure that ADR 0122 refers to "(runbook)"
  does not exist in `backup-restore-dr.md`. Who may mint or extend the credential, and from where, is open (gap G1). Write it:
  requester, approver, ticket, TTL ≤ 1 h, session logged, re-mint after use.
- **L7. gpg recipient handling.** gpg uses `--trust-model always` with a free-form recipient. Require a full 40-hex fingerprint.
- **L8. Predictable temp file and unvalidated database name.** `${TMPDIR:-/tmp}/bk-pre.$$` is predictable (mitigated by
  `fs.protected_symlinks`); use `mktemp`. `BACKUP_PGDATABASE` goes unvalidated into the output path and into `manifest.json`;
  validate it as an identifier.
- **L9. Decrypted restore material can be left behind.** Decrypted material goes to `$TMPDIR` (0700, umask 077, removed by the EXIT
  trap), but SIGKILL or SIGPIPE leaves it in place. In production, use tmpfs or an encrypted scratch volume, or stream-decrypt into
  `pg_restore`.
- **L10. Seeder port check is loose.** The seeder refuses only ports 5432/5433 on 127.0.0.1, not "not a drill port". Align it with
  `BACKUP_DRILL_PORTS`.
- **I1. Seeder isolation (OK).** The seeder is a `_test.go` file with `//go:build integration && drillseed`, so it is never compiled
  into `platform-api`. Its staff-token minting (`authtest.ClaimedStaffToken`) and `testJWTSecret` exist only in test binaries.
  `internal/testsupport/authtest` is not imported by any non-test file under `cmd/` or `internal/`.
- **I2. credentialscan does not cover these files.** `internal/testsupport/credentialscan` is a Go reflection walker for
  provider-adapter values; it does not scan shell, SQL, Terraform or Markdown. The record's mention of running it is accurate but
  irrelevant to these files. The real guard is `deploy/backup/backup_static_test.go`. Its literal-password regex
  (`PASSWORD\s+'[^']`) misses `postgres://u:pw@`, `PGPASSWORD=`, dollar-quoted strings and `password=`, and it scans only
  `deploy/backup/`, not `deploy/aws/`, `docs/` or the seeder. I found no committed secret in the diff. A repository-wide secret
  scanner is still a gap.
- **I3. Accepted blast radius of the dump (ADR 0122 section 9 probe 3).** BYPASSRLS + SELECT reads, in plaintext:
  `actor_proof_keys.secret`, all PII, `player_accounts.password_hash` and `staff_users.password_hash` (open to offline cracking),
  `sessions.refresh_token_hash` and token MACs (not reversible without keys). It reads as ciphertext, with keys elsewhere:
  `staff_mfa_factors.secret_ct`, `payout_instruments.detail_ciphertext`, and recovery-code MACs. `provider_credential_handles`
  holds `secret_ref` references, not secrets. `pg_authid` is not readable, so dumps carry no other role's password hash. Keeping
  everything in the dump is acceptable **only** with H1 and H3 fixed. A runbook entry is also required: any suspected exposure of a
  backup set means rotating `actor_proof_keys` (operational-runbooks section 15) and resetting staff passwords. Excluding
  `actor_proof_keys` data and re-keying on restore remains a valid alternative; that is an architect decision.
- **I4. Statement logging of the backup role (OK).** It logs query text, not results. `pg_dump` and `baseline.sql` statements carry
  no data values. Break-glass human queries could put PII in WHERE clauses into the logs; cover this in L6.

## Required tests (for `qa` / devops)

1. Bad age or gpg recipient: `backup.sh` exits non-zero, no plaintext artifact remains, and no `SHA256SUMS` is written (H1).
2. The snapshot holder is killed mid-dump: non-zero exit, no plaintext left on the backup volume (H2).
3. Unsigned or wrongly signed set: `restore.sh` refuses before any database contact. A superuser restore user is refused outside
   drill ports (H3).
4. Every M1 abuse probe is denied after the database-level REVOKEs, or recorded as accepted residual with monitoring.
5. `--owner-role postgres` and `--owner-role igaming_backup` are refused (M3).
6. A source anchor that is unrecorded, development or test is refused for a production restore (M4).
7. Every alias in M5 is refused on 5432.

## Not in scope / not proven

- RDS behaviour: BYPASSRLS creation by the master, pgaudit redaction, IAM DB auth, and whether REVOKE on pg_catalog functions works.
- KMS key policy, S3 storage of backup sets, the real runner identity.
- Actual execution of a malicious archive (H3 is from `pg_restore` semantics).
- The full 30-minute path in H2 (the shell mechanism was reproduced).
- `terraform validate/test` (not installed).
- Penetration testing.

Passing this review does not make the subsystem "secure". It records what was checked at commit `d606e1c`.
