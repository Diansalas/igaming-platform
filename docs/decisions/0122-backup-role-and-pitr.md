# ADR 0122 - Dedicated audited backup role (BACKUP-ROLE-1) and managed point-in-time recovery

- **Status:** ACCEPTED (2026-10-15, owner decision 7: "backup architecture = dedicated audited backup role + managed
  point-in-time recovery (RDS PITR) with logical dumps as secondary"). Recorded as PROPOSED by the A12 work and accepted on
  that owner decision. **The security review is recorded** (`docs/plans/prh2-hardening-round/a12-security-review.md`, commit
  `d0d804b`): role design ACCEPTED WITH CONDITIONS; real-data logical backup/restore was BLOCKED by H1-H3. The fix round
  (sections 3, 5, 5a, 5b, 5c, 9) implements H1-H3, M2-M5, the mitigable part of M1, M7 as the production procedure, and L1-L10; M6 and
  M8 are recorded as staging/production requirements. **Nothing here is applied to any AWS or shared environment.**
- **Decision type:** architecture decision with a local implementation. Closes `BACKUP-ROLE-1` (OPEN since OPS-READINESS-LOCAL,
  `docs/runbooks/restore-from-backup.md`, finding 1) and decision `D-BACKUP` of `docs/plans/platform-completion-audit-2026-10-14.md`.
- **Owner:** `devops` (implementation), `security` (review), `architect` (this record). Related: ADR 0002 (tenancy/RLS),
  ADR 0108 (`TEMPORARY` revoked from the runtime role), ADR 0110 (signed actor proof), ADR 0086 (staging), ADR 0115 S1 (environment anchor).
- **Not a Blueprint change.** `CLAUDE.md` environment-safety rules stand: no production credentials, no real data, no role or
  password change on the shared cluster. Everything below was exercised on a throwaway cluster the drill creates and destroys.

## 1. Problem (proven, not assumed)

Every tenant table is `FORCE ROW LEVEL SECURITY`. A backup tool that connects as a role subject to RLS therefore either fails or
silently produces an incomplete dump. The drill (`deploy/backup/drill.sh`, results in `docs/plans/a12-backup-drill-record.md`)
shows all three outcomes on a database loaded through the platform's own services:

| Dumper | `pg_dump -Fc` | Result |
|---|---|---|
| migration owner `igaming` | refused: `query would be affected by row-level security policy for table "alert_deliveries"` | no backup |
| migration owner with `--enable-row-security` | fails part-way (`Dumping the contents of table "financial_approval_policies" failed`): with no tenant scope a policy errors or hides rows; the drill accepts either outcome and reports which | never a complete dump; where it does not fail it would silently omit every tenant's rows |
| `igaming_runtime` | `permission denied for table actor_proof_keys` | no backup |
| dedicated `igaming_backup` | complete: every `ledger_entries` row is in the dump | the only supported logical path |

Weakening the control to dump (`NO FORCE ROW LEVEL SECURITY`, `row_security=off`, `session_replication_role=replica` on the
dumping session, a superuser connection) is **rejected**: it removes the guarantee the dump is meant to protect.

## 2. Decision

1. **Primary recovery mechanism: managed RDS automated backups with point-in-time recovery** (continuous transaction-log
   archiving to S3, restore to any second inside the retention window) plus final/manual snapshots. It bypasses no RLS, needs no
   extra database credential and carries the whole cluster state. Staging configuration is prepared in `deploy/aws` (14-day window,
   deletion protection, final snapshot, `copy_tags_to_snapshot`, encryption, log export, connection logging, optional pgaudit) and
   is **not applied**; the single authorised staging deploy follows `docs/runbooks/aws-staging-deploy-plan.md`.
2. **Secondary mechanism: logical dumps** (`pg_dump -Fc`, `pg_dumpall --globals-only`) taken only by the dedicated role
   **`igaming_backup`**, for portability (cross-version/engine moves), restore into a new database, forensic copies and
   single-tenant recovery guidance. Logical dumps are **not** the disaster-recovery primary: their RPO is the backup interval.
3. **Restore is a separate, human-run operation** by a different role (the owner role or a CREATEDB role that is a member of it),
   into a **new** database only, followed by the verification gates of `deploy/backup/verify.sh`.

## 3. The `igaming_backup` role

Defined by `deploy/backup/init-backup-role.sql` (idempotent; applied by a human with CREATEROLE + the right to grant BYPASSRLS).

| Property | Value | Why |
|---|---|---|
| `NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION` | yes | nothing beyond reading |
| `BYPASSRLS` | **yes** | the single exemption that makes a complete `pg_dump` possible under FORCE RLS; cannot be avoided |
| membership in any role | none (asserted by the SQL and by the drill) | no inherited write privilege; not in `igaming` or `igaming_runtime` |
| privileges | `CONNECT` on the one database; `USAGE` on schema `public`; `SELECT` on all tables and sequences, plus default privileges for objects the migration owner creates later | what `pg_dump` needs; no `CREATE`, no `TEMPORARY`, no DML, no DDL, no `TRUNCATE`, no `EXECUTE` beyond PUBLIC defaults |
| session defaults | `default_transaction_read_only=on`, `statement_timeout=4h`, `idle_in_transaction_session_timeout=30min`, `lock_timeout=30s` | defence in depth; a dump is one long `REPEATABLE READ` transaction |
| `CONNECTION LIMIT 2` | yes | one dump needs one connection (the drill uses two: snapshot holder + `pg_dump`); a stolen credential cannot fan out |
| credential (local / non-RDS) | a **client-computed SCRAM-SHA-256 verifier** (`deploy/backup/scramhash`, password on stdin) is the only thing the server ever sees (`IGAMING_BACKUP_PASSWORD_VERIFIER`; the clear password is rejected by the SQL), so `log_statement=all` / pgaudit on the minting session cannot capture it (drilled with superuser statement logging on, with a control that a naive `PASSWORD :'pw'` does leak); password by **reference** from the secret store (`backup_runner_secret_arn`), never in the repository, argv or a URL; `VALID UNTIL` set, **default 24 h, REFUSED beyond now + 48 h** (`infinity` rejected, drilled); written to a 0600 passfile by the runner | JIT/short-TTL; expiry fails the backup **closed** (drilled); M2, L1 |
| credential (**staging / production on RDS: REQUIRED**, M8) | **IAM database authentication** for `igaming_backup`: `GRANT rds_iam TO igaming_backup`, no password at all. Login needs a 15-minute token signed by the runner's IAM identity, recorded in CloudTrail and scoped by an `rds-db:connect` policy on that one database user. A stolen password-based credential on RDS is usable by any principal that can reach the instance (security groups are per instance, not per role; RDS has no editable `pg_hba`), and a self-`ALTER ROLE ... PASSWORD` locks the runner out; IAM auth removes both and M2 entirely. The password path above is for the local drill and non-RDS targets only. **Not verified on RDS** (`aws-staging-deploy-plan.md` section 5) | M8 |
| use | the backup runner only; never the application, never a human by default; use outside the runner is a break-glass event with four-eyes approval (runbook) | blast-radius control |
| network | on a host you control: `pg_hba` restricts the role to the runner. **On RDS this cannot be achieved** (no editable `pg_hba`; SGs are per instance), so the claim "a BYPASSRLS credential is useless from elsewhere" is NOT made for RDS: the control there is IAM database authentication (row above) | M8 |
| audit | `log_connections`, `log_disconnections` cluster-wide; per-role statement logging (`ALTER ROLE ... SET log_statement='all'` on stock PostgreSQL, **pgaudit** on RDS: `pgaudit.log` set for the role, logs exported to CloudWatch); every dump is also a signed-off event in the runbook (who/when/why) | non-repudiation; the drill counts the log lines |

**What `backup.sh` enforces before dumping** (fail closed): the role is not a superuser, has `BYPASSRLS`, has none of
CREATEROLE/CREATEDB/REPLICATION, runs read-only by default, and can `SELECT` every table and sequence (a table added by a later
migration without the default privilege would otherwise produce a dump that silently omits it); encryption is validated **before**
anything is dumped (section 5); the signing key is validated before anything is dumped (section 5).

**What a stolen credential can still do (residuals, M1).** Session defaults set with `ALTER ROLE igaming_backup SET ...`
(`default_transaction_read_only`, `statement_timeout`, `idle_in_transaction_session_timeout`, `lock_timeout`, `log_statement` is
superuser-only) are **accident guards, not controls**: any login role can `SET`/`RESET` them for its own session and can
`ALTER ROLE <itself> SET ... / PASSWORD` (inside a read-write transaction). PostgreSQL has no privilege that prevents a role from altering
its own settings or password, so this cannot be closed in the database; the only closure is not to have a password (IAM database
authentication, M8) and to detect the abuse. The abuse primitives that CAN be removed are removed by `harden-backup-surface.sql` (section 5b).

**The role is high value.** `BYPASSRLS` + `SELECT` on everything reads every tenant's PII and money, the staff MFA ciphertext, the
payout-instrument records and `actor_proof_keys` (the signed-actor-proof signing keys, ADR 0110). A backup is therefore treated as
the most sensitive artefact the platform produces (section 5).

## 4. Physical path (primary) - what is and is not decided

Decided: RDS automated backups with PITR, retention 14 days in staging, deletion protection, final snapshot on destroy,
encrypted storage (KMS key by reference: `db_kms_key_arn`, otherwise the AWS-managed key, as in ADR 0086), log export, connection
logging, pgaudit preload. **Not decided / not proven here:** production retention and multi-AZ/replica topology, cross-region copy,
the KMS key policy and grants for snapshot sharing/restore, RDS restore timing at production size (RDS PITR creates a **new**
instance; the cut-over of the application to it is a deploy procedure, `docs/runbooks/backup-restore-dr.md`).

## 5. Artefact protection

A logical backup set contains PII and financial data. Required, and enforced by the scripts (A12 security review H1-H3):

1. **Encryption fails closed and the plaintext never reaches the backup volume (H1, H2).** The recipient (`BACKUP_ENCRYPT_AGE_RECIPIENT`
   or `BACKUP_ENCRYPT_GPG_RECIPIENT`, a public reference; the private key lives in the restorer's secret store/KMS and **no key
   material is committed**) is validated **before** the dump: an age recipient must be well formed and a probe encryption must succeed;
   a gpg recipient must be a full 40-hex fingerprint present in the keyring and a probe encryption must succeed. The dump is then
   **streamed**: `pg_dump -Fc | tee >(sha256sum) | age|gpg > db.dump.age.partial`, with `pipefail`, so a failing encryptor fails
   the backup and the run directory is removed (no `age ... && rm` pattern exists any more). After encryption the script asserts the magic
   bytes of every artefact, that no plaintext `db.dump`/`globals.sql`/`baseline.txt` and no `.partial` exists, and the manifest's
   `encryption` field is **derived from those files**, never from configuration. The only unencrypted path is
   `BACKUP_ALLOW_UNENCRYPTED=synthetic-data-only`, decided **before any connection** and honoured only for a local throwaway drill
   target (a remote or non-drill target is refused, L2).
2. **No plaintext at rest on abnormal exit (H2).** SIGPIPE is ignored and handled as an error; INT/TERM/HUP exit through the cleanup
   trap, which terminates the whole dump pipeline (children first), removes the private temp directory (`mktemp -d`, 0700, under
   `BACKUP_TMPDIR`) and the partial run directory. The next run sweeps what a SIGKILL/OOM left behind (stale temp directories and
   `.incomplete` run directories of dead pids). The snapshot-holder session is kept alive by a keepalive query (default every 60 s,
   so the role's `idle_in_transaction_session_timeout=30min` can no longer kill a long dump's holder) and is monitored: if it dies the
   backup fails loudly and keeps nothing. Decrypted material on the restore side lives only in a private 0700 temp directory that is
   removed on every exit path; in production put `BACKUP_TMPDIR` on tmpfs or an encrypted scratch volume (L9).
3. **The set is signed and verified before anything else (H3).** `SHA256SUMS` (which lists every artefact **including**
   `manifest.json`) is signed with an Ed25519 key (`openssl pkeyutl -sign -rawin`, OpenSSL >= 3; key by reference
   `BACKUP_SIGNING_KEY_FILE`, mode 0600/0400, never inside the backup directory). `restore.sh` verifies `SHA256SUMS.sig` against a
   **pinned** public key (`RESTORE_VERIFY_PUBKEY_FILE` plus its SHA-256 DER fingerprint `RESTORE_VERIFY_PUBKEY_SHA256`, supplied by
   the operator or change control, **never read from the backup directory**; the key is copied into the private work directory before it is
   fingerprinted and used) **before** checksumming, decrypting or contacting any database. **What is verified is what is used (re-review
   SR12R-1):** `SHA256SUMS` and `SHA256SUMS.sig` are copied into a private 0700 work directory and verified there; the backup directory must
   hold only regular files, every one listed in the signed list (an **unlisted file, a symlink or a sub-directory is refused**, so a plaintext
   `db.dump` placed beside a signed encrypted set cannot shadow it); the signed list must name exactly one form (plain, `.age` or `.gpg`) of
   each of `db.dump`, `globals.sql`, `baseline.txt` plus `manifest.json` and nothing else; each listed artefact is copied into the work
   directory, hashed there and only the copies are decrypted and restored (no re-read of the shared storage); the decrypted dump's hash must equal the
   signed manifest's `plaintext_dump_sha256`; the source environment anchor is taken from the signed manifest and must equal the baseline's. There is no "allow unsigned" switch. Drilled: unsigned set, attacker-signed replacement set, key placed inside the
   backup directory, wrong or missing pin, manifest altered after signing, checksum list altered, garbage signature: all refused with
   zero database connections. Signing-key custody: the runner holds the private key as a secret-store/KMS-wrapped file (an AWS KMS
   asymmetric signing key is the production target and is NOT built here: PROVIDER/INFRA DEPENDENT); the verification fingerprint is
   held outside the backup storage so an attacker with write access to storage cannot replace both.
4. Restricted storage (versioned, encrypted, access logged), a defined retention, deletion on expiry; a restore is a declared incident.

`SHA256SUMS` alone still only detects corruption; the signature is what makes the set authentic. Signed does not mean safe to
execute: the archive is still SQL, which is why the load never runs with superuser rights (section 5a).

## 5a. Restore design and a correction to earlier documentation

`pg_dump -Fc` orders every archive pre-data (tables, sequences) -> data (`COPY`) -> post-data (indexes, constraints, **triggers**,
**RLS enablement and policies**). A plain `pg_restore` into an empty database therefore loads the data **before** any trigger or
policy exists: nothing is bypassed, no superuser and no `session_replication_role` are needed, and the restoring role needs only to
own the objects. `deploy/backup/restore.sh` runs it as one transaction (`--single-transaction --exit-on-error --no-owner
--role=<owner>`): all or nothing. This **corrects** the assumption in `docs/runbooks/restore-from-backup.md` section 3 step 2 that a
data-bearing restore "fires the ledger triggers" and would need `--disable-triggers`; that text is updated. (Restoring **data only**
into an existing schema still would, and is not a supported path.)

What a dump does not carry and `restore.sh`/the checklist re-create: database-level ACLs (`CONNECT`, the 0116 `TEMPORARY` revoke), the
`public` schema ownership (found by the drill's verifier), role passwords, the `database_environment` anchor, signing/JWT keys and
other secret-store material, and **the `pg_catalog` function REVOKEs of `harden-backup-surface.sql`** (drilled: `pg_dump` DOES emit these system-object ACL entries, but a
non-superuser `pg_restore` cannot apply them and leaves a materialised default ACL instead, so after a full-loss restore the backup role could
take advisory locks until the hardening script was re-run; the script therefore tests "PUBLIC holds EXECUTE", not only "ACL is NULL"; the
post-restore checklist includes the step).

**Two privilege levels, never one (H3).** A dump is executable SQL and `--role=<owner>` only issues `SET ROLE`; a planted entry can
act as the session user. `restore.sh` therefore splits the operation:

| Phase | Identity | Does | Archive content executed |
|---|---|---|---|
| `provision` | bootstrap superuser / `rds_superuser` member (only to create roles; on a new cluster) and `CREATEDB` | creates the three project roles **without passwords** from allow-listed `CREATE ROLE` / `ALTER ROLE r WITH <attrs>` statements (no `SET` replay, `igaming_backup` pinned to `CONNECTION LIMIT 2`, BYPASSRLS only for it; L4) and an **empty** database owned by `igaming` | none |
| `load` | the **non-superuser** owner `igaming` (or a CREATEDB member of it) | `pg_restore --single-transaction --exit-on-error --no-owner --role=igaming` into the empty database | all, but with no privilege to abuse |
| `all` (default) | one identity that must satisfy `load` | both (the same-cluster case) | all |

A superuser or `rds_superuser` member is refused for the load (override `RESTORE_ALLOW_PRIVILEGED_DATA_ROLE=drill-only` exists for a
local throwaway target and is used by nothing). Drilled: a validly-signed archive whose expression-index function runs `COPY ... TO
PROGRAM` fails harmlessly as the non-superuser restore role (transaction rolled back, database dropped, no file created), is refused
outright for the superuser, and a **control** (the naive procedure: `pg_restore` by hand as the superuser) shows the same archive DOES
run the planted command. **CORRECTION (re-review SR12R-2):** the earlier text claimed the archive "cannot touch other databases or roles". That is false. Acting as
the owner role the archive can run `ALTER ROLE <owner> PASSWORD / SET ...`, which is cluster-global and commits with an otherwise successful
restore (reproduced); a loading identity with `CREATEROLE` or membership in `pg_execute_server_program` / `pg_*_server_files` / `pg_*_all_data` /
`pg_signal_backend` / `rds_superuser` would also be able to create login roles, run programs or read files. What the scripts now enforce, and what they do not:

- **Preventive:** the loading identity AND the owner role must hold none of SUPERUSER, CREATEROLE, BYPASSRLS, REPLICATION and must be a member of no `pg_*`
  (other than `pg_database_owner`) or `rds_*` role (recursive `pg_has_role`), refused before any database is created (drilled: pg_execute_server_program via the owner,
  CREATEROLE, five other pg_* roles). On RDS the master user is an `rds_superuser` member, so a separate plain owner login must exist (staging plan 9.2).
- **Detective:** a fingerprint of role attributes, per-role settings and role memberships is compared before and after the load; a change (e.g. a planted
  `ALTER ROLE igaming SET ...`) makes the load exit 3, drops the database and instructs the operator to treat it as an incident (drilled). When the connecting role is
  the owner, a fresh connection after the load proves its credential still works (a planted `ALTER ROLE igaming PASSWORD` is detected; drilled).
- **Not preventable in the database:** an owner can always alter its own password and settings; passwords are not readable by a plain role, so a password change
  by a connecting role other than the owner is not detected. Mitigation: the restore owner credential is single-use and **every project credential is re-provisioned
  after any restore** (runbook section 5), which also overwrites a planted password; a planted `ALTER ROLE ... SET` is caught by the fingerprint.
- A malicious archive can still consume resources and read/write within the new empty database.

**Owner role allow-list (M3).** `--owner-role` is `igaming` unless an explicit `RESTORE_OWNER_ROLE_OVERRIDE=<name>` is set;
`postgres`, `igaming_backup`, `igaming_runtime`, `rdsadmin` and `rds_superuser` are always refused, and the owner must be
NOSUPERUSER, NOBYPASSRLS and NOCREATEROLE (drilled).

**The environment anchor is not restored (M4).** `database_environment` is table data and its write-once guard is post-data, so the
archive would decide the anchor. `baseline.sql` now records the **source** anchor (`META|environment_anchor`, also in the signed
manifest); `restore.sh` requires `RESTORE_EXPECT_SOURCE_ENVIRONMENT` to equal it (the restorer must state what they believe they are
restoring), requires an explicit `RESTORE_TARGET_ENVIRONMENT` and allows a restore only when **source and target environments are equal** (re-review SR12R-5:
a staging backup restores into staging without anyone recording `production` on it; a development or staging set can never enter production; restoring a production
set into staging is refused and needs a deliberate, separately approved procedure), and **excludes the anchor's
TABLE DATA** from the restore (`pg_restore -L`). After a restore the anchor is therefore BLANK and a production `platform-api`
**refuses to start** until the owner records it (drilled with a development-source backup: `verify.sh` asserts 0 rows and that
the production guard refuses). The owner records `production` **only after verifying that the source environment was production**;
recording it on a restored development/staging copy would turn the guard into a rubber stamp (runbook section 5). Physical recovery
(PITR) preserves the anchor, which is correct: it is the same database.

## 5b. Narrowing what a stolen backup credential can abuse (M1)

`deploy/backup/harden-backup-surface.sql` (run by the bootstrap superuser per target cluster; applied by `lib-provision.sh` to the
**throwaway** clusters only, never to a shared cluster by anything in the repository; static test pins that) does what the database
allows:

- `REVOKE EXECUTE ... FROM PUBLIC` on the `pg_catalog` functions `pg_advisory_*` / `pg_try_advisory_*` (session and transaction
  locks, whose keys in this platform are deterministic: migration lock, reconciliation, sportsbook exposure, tenant status gates),
  `pg_logical_emit_message` (unbounded WAL), the `lo_*` large-object functions and `pg_notify`, granting back to `igaming` and
  `igaming_runtime` only (the roles that use advisory locks). Only functions whose ACL is still the default are touched.
- `REVOKE CONNECT ON DATABASE postgres, template1 FROM PUBLIC`; operators and provisioning get explicit grants.

Drilled: the backup role is denied all eight probes; `igaming` and `igaming_runtime` keep working (the full seed runs after the
hardening); after a full-loss restore the revokes are absent until the script is re-run. **Feasibility of REVOKE on `pg_catalog`
functions on RDS is UNVERIFIED** and must be recorded at the staging deploy.

**Residuals (cannot be removed in the database), with detection:**

| Residual primitive of a stolen credential | Why it remains | Detection to build | Closure |
|---|---|---|---|
| `ALTER ROLE igaming_backup PASSWORD / SET ...` on itself; `SET`/`RESET` of session defaults | PostgreSQL has no privilege preventing a role from altering itself | pgaudit/`log_statement` lines for `ALTER ROLE` by `igaming_backup`; the preflight re-checks `default_transaction_read_only` | IAM database authentication (M8) |
| `LOCK TABLE ... IN ACCESS SHARE MODE`, an idle open transaction | any role with SELECT may; a waiting DDL queues all readers behind it | `pg_locks` / `pg_stat_activity` rows of `igaming_backup` older than N minutes or holding a lock mode other than AccessShare; `age(backend_xmin)` | session limit (`CONNECTION LIMIT 2`), runner-side kill procedure (runbook) |
| `LISTEN` | harmless today (no channel is used) | n/a until a channel is added | re-check when one is |
| reading every row (it is the point of the role) | BYPASSRLS + SELECT | connection/disconnection logs, per-role statement logs, alert on a connection outside the runner window | IAM auth + CloudTrail; dump from a clone (5c) |

**Observability follow-up (not built; no such metric exists):** `OBS-BACKUP-ABUSE-1` (devops): export from a read-only monitoring
identity the oldest transaction age and lock modes held by `igaming_backup`, and alert on a session older than the longest expected
dump, any non-AccessShare lock, or any connection outside the backup window. Rules are added only when the metric exists
(listed in `deploy/observability/pending-signals.yml` as `BackupRoleAbuse`, guarded by its parity test); no alert rule was invented.

## 5c. Production procedure: dump from a clone, not from the primary (M7)

`statement_timeout` bounds a statement, not the dump: a logical dump is one `REPEATABLE READ` transaction that holds the primary's
vacuum horizon for its whole duration (about 40 minutes at 50 GB by the extrapolation in the record; unbounded if the role's
timeouts are overridden, M1) and bloats the hot tables (sessions, outbox, withdrawal and adjustment state machines). Therefore:

- **Production** logical dumps are taken from a **short-lived instance restored from PITR or a snapshot** (or a replica), never from
  the primary. This also means the BYPASSRLS credential does not exist on the primary at all. The clone is destroyed after the dump.
- Staging and the local drill dump the instance directly (small data, acceptable).
- Detection for a long snapshot on any instance where it is allowed: `SELECT pid, now() - xact_start AS xact_age, age(backend_xmin) FROM
  pg_stat_activity WHERE usename = 'igaming_backup'`; page when the transaction age exceeds the planned dump time by a margin
  (runbook section 3a). The keepalive (section 5) keeps the holder alive; it does not make a long snapshot harmless.

## 6. Alternatives rejected

| Alternative | Rejected because |
|---|---|
| owner dump with `NO FORCE ROW LEVEL SECURITY` for the dump window | removes the isolation guarantee during the window; violates `CLAUDE.md` and the PLAT-ROLESPLIT-1 intent; also needs a re-enable step whose failure leaves the control off |
| `session_replication_role = replica` / `--disable-triggers` for dump or restore | unnecessary (section 5a) and it disables the very guards the platform relies on |
| grant `BYPASSRLS` to `igaming` (the owner) | the owner already can ALTER/DROP/TRUNCATE; adding RLS bypass makes the migration credential able to read and rewrite everything with no policy |
| superuser/`rds_superuser` for backups | far wider than needed; not available on RDS as a true superuser |
| per-tenant dumps by looping the tenant GUC as the owner | misses platform-scope tables and gives no consistent cross-tenant snapshot; many credentials-in-use windows |
| physical-only, no logical path | loses portability and single-database forensic copies; the owner wants logical as a secondary path |

## 7. What was verified locally (throwaway cluster, 2026-10-15)

See `docs/plans/a12-backup-drill-record.md` for the numbers. In short: owner/runtime dumps fail or are incomplete; the backup role
dumps everything and 15 write/escalation probes are denied; connection limit, expiry and wrong-role refusals work; the baseline
captured in the dump's own snapshot is reproduced exactly by a restore (row counts and content hashes of every table, ledger
invariants, projection == recompute with drift 0, sequences, roles and ACLs by name, triggers incl. enabled state, policies, RLS
flags, catalog fingerprints), a negative control proves the verifier can fail, and a base backup + WAL archive recovers a second
instance to a named restore point and to a timestamp with rows after the target absent.

## 8. NOT proven / residual

- Managed RDS behaviour (PITR timing and mechanics, snapshot restore, parameter-group effects, `rds_superuser` ability to create a
  `BYPASSRLS` role, pgaudit availability and per-role configuration on RDS PostgreSQL 16, CloudWatch export of audit lines, IAM database
  authentication for `igaming_backup`, whether `REVOKE ... ON FUNCTION pg_catalog....` is permitted on RDS).
- KMS key policy, cross-account/cross-region copies, S3 storage of logical backup sets, a real backup runner identity and schedule, an
  asymmetric-KMS signing key (the drill signs with an ephemeral Ed25519 key file; the verification fingerprint is pinned by the operator).
- Production-size timings (the numbers in the record are an extrapolation at a small synthetic scale, labelled as such).
- gpg end-to-end (only its refusals are drilled: short id, unknown key; age is drilled end to end).
- The full 30-minute holder timeout was reproduced with a 3-second timeout (same mechanism), not with a 30-minute dump.
- Real `ENOSPC` mid-restore; a malicious archive that is not a function-in-index (only one planted-command fixture was built).

## 9. Security review disposition (`docs/plans/prh2-hardening-round/a12-security-review.md`, commit `d0d804b`)

| Finding | Disposition | Where / how it is proven |
|---|---|---|
| H1 encryption fail-open | **FIXED**: validated before dump, streamed, pipefail, plaintext assertions, truthful manifest field | drill: mistyped / malformed recipient, unknown and short gpg id, killed encryptor mid-stream, override refused for remote and for `=yes`; each leaves no run dir, no plaintext, no temp |
| H2 plaintext on abnormal exit; 30-min holder | **FIXED**: streaming, traps (SIGPIPE ignored, INT/TERM/HUP), keepalive and holder monitoring, 0700 `mktemp` temp root, startup sweep | drill: keepalive survives a 3 s idle timeout with a 9 s delay (control without keepalive fails loudly), killed holder, SIGTERM and SIGINT with a `.partial` present, closed stdout, sweep of stale temp and orphan run dir, PGDMP-magic scan of the whole working area |
| H3 unsigned set replayed by a superuser | **FIXED**: signing REQUIRED, pinned key, two-phase restore, superuser data load refused, allow-list kept | `go test ./deploy/backup/` (real Ed25519) and drill (see section 5, 5a) |
| M1 abuse primitives | **MITIGATED** where the database allows (section 5b); remainder RECORDED with detection; OBS follow-up `OBS-BACKUP-ABUSE-1` | drill: 8 probes denied, platform roles unaffected, hardening lost on full-loss restore |
| M2 password in statement text | **FIXED**: client-side SCRAM verifier | drill: superuser `log_statement=all` during minting, password absent from the server log; control leaks |
| M3 owner role unchecked | **FIXED**: allow-list, attribute checks | drill: 6 owner names |
| M4 anchor on restore | **FIXED**: source anchor recorded, expectation required, anchor data excluded, blank after restore | drill: dev-source restore, anchor 0 rows, production guard refuses; wrong expectation refused |
| M5 loopback aliases | **FIXED**: canonicalisation with resolution, multi-host and userinfo rejected | unit tests (24 spellings x 2 acknowledgement variants on 5432) and 9 drill aliases |
| M6 deployer can erase logs / shorten retention | **ACCEPTED for staging**, production requirement recorded in `aws-staging-deploy-plan.md` | n/a (Terraform/IAM, static) |
| M7 long snapshot on the primary | **PRODUCTION PROCEDURE RECORDED** (5c): dump from a PITR/snapshot clone; detection query in the runbook | n/a locally |
| M8 "useless from elsewhere" on RDS | **REQUIREMENT RECORDED** (section 3): IAM database authentication on RDS | NOT verified on RDS |
| L1 unbounded VALID UNTIL | **FIXED** (48 h) | drill |
| L2 override on any target | **FIXED** | drill |
| L3 passwords in verify URLs | **FIXED**: passfiles only; embedded password refused | drill (all verifies run password-less) |
| L4 replayed `SET` | **FIXED**: not replayed; `CONNECTION LIMIT 2` pinned | unit test |
| L5 CONNECT on postgres/template1 | **FIXED** in the hardening script (throwaway clusters) | drill |
| L6 break-glass / minting authority | **WRITTEN** in `backup-restore-dr.md` section 2a | n/a |
| L7 gpg recipient | **FIXED**: full 40-hex fingerprint | drill (refusal) |
| L8 temp names / db name | **FIXED**: `mktemp`, identifier validation | unit tests |
| L9 decrypted restore material | **FIXED** for normal and signal exits; tmpfs advised for production; SIGKILL leaves it until the next sweep | drill (empty temp root after restore) |
| L10 seeder port check | **FIXED**: drill ports only | static test |
| I2 secret scan | **EXTENDED**: all of `deploy/`, `docs/runbooks/`, ADR 0122 and the A12 plans, shell/SQL/Terraform/JSON/Markdown, with planted-secret tests | `go test ./deploy/backup/` |
| I3 blast radius of the dump | **OPEN DECISION (section 10)**; runbook entry for rotating signing material after any suspected exposure written | - |

## 10. Open decision for the `architect`: should logical dumps exclude `actor_proof_keys.secret`?

`actor_proof_keys.secret` is the signing secret of the signed actor proof (ADR 0110). A logical dump (and a backup set, and a
restored database) carries it in the clear (inside the encrypted artefact). Anyone who obtains a decrypted dump can forge actor proofs
until the key is rotated. Options:

1. **Keep it in the dump** (as built): encryption + signing mandatory; any suspected exposure means rotating the signing material.
2. **Exclude the secret from logical dumps and re-key on restore** (`pg_dump --exclude-table-data=actor_proof_keys`; restore creates a
   fresh key family through the application's key-rotation path; physical recovery (PITR/snapshots) still carries the real key, so
   disaster recovery of the live key is unaffected).

**Recommendation (`devops`, for the architect to decide; NOT implemented): option 2.** The logical dump is the secondary,
portability/forensic mechanism, so it should not carry the platform's signing secret; the cost is a documented re-key step that
invalidates in-flight proofs (short-lived by design) and a smaller blast radius for every copy of a dump. It needs: the exclusion in
`backup.sh` plus a baseline/verify adjustment (the table's count and hash would differ by design), a restore-time re-key step in the
runbook, and a test that a restored database cannot verify proofs signed by the source's key. Until decided, the runbook requires
rotating `actor_proof_keys` and resetting staff passwords after ANY suspected exposure of a backup set (`backup-restore-dr.md`
section 5a).

## 11. Re-review of the fix round (`docs/plans/a12-backup-drill-record.md` section 8): dispositions and recorded design decisions

| Finding | Disposition |
|---|---|
| SR12R-1 (HIGH) signature bypass by an unlisted plaintext file / TOCTOU | **FIXED**: private copies, strict listed-names-only directory, no symlinks, exact form per artefact, decrypted-bytes hash against the signed manifest, anchor from the signed manifest. Drilled: PoC (plaintext `db.dump` with a planted `ALTER ROLE ... PASSWORD`, `baseline.txt`, `globals.sql` beside a signed encrypted set) refused with 0 database connections and the owner credential unchanged; symlink, sub-directory, two-forms list, manifest hash and anchor mismatches; originals overwritten after verification (test hook) do not affect the restore |
| SR12R-2 (MED) role memberships | **FIXED** (preventive + detective) and the section 5a claim corrected; see there |
| SR12R-3 (MED) IPv6 spellings | **FIXED**: a real IPv6 parser (`bk_ipv6_expand`: `::` compression anywhere, leading zeros, embedded dotted quad) classifies `::`, `::1`, `::ffff:127.x`, `::127.x`; an invalid literal is refused; unit tests carry every spelling listed by the reviewer plus more, each with and without the acknowledgement, on 5432 |
| SR12R-4 (MED) `sslmode=require` | **FIXED**: a non-local target needs `sslmode=verify-full` and `BACKUP_PGSSLROOTCERT` (the RDS CA bundle); weaker modes refused (unit-tested, drilled); staging plan corrected |
| SR12R-5 anchor rule vs staging | **FIXED**: `RESTORE_TARGET_ENVIRONMENT` must equal the source |
| SR12R-6 sweep follows symlinks | **FIXED**: only real directories matching the run-name pattern; `.incomplete` records `host:pid`; another host's runs are never touched (drilled) |
| SR12R-7 chmod of the temp root | **FIXED**: a pre-existing root is never chmod-ed; a non-drill target must set `BACKUP_TMPDIR` to a 0700 directory owned by the invoking user |
| SR12R-8 DSN password | **FIXED**: any `password`/`passwd` key in a URL or DSN is refused (case-insensitive) |
| SR12R-9 IAM-only role path and token refresh | **IMPLEMENTED (simplest safe form), unproven on RDS**: `BACKUP_ROLE_IAM_ONLY=1` creates/alters `igaming_backup` with a NULL password and grants `rds_iam` where it exists; `BACKUP_IAM_TOKEN_COMMAND` (trusted operator configuration, e.g. `aws rds generate-db-auth-token ...`) is run before EVERY new connection (preflight, snapshot holder, `pg_dump`, `pg_dumpall`) and its token written to a private passfile in the work directory. Drilled with the role password standing in for a token; the real token flow, token lifetime vs. the longest single connection (a connection that outlives 15 minutes is unaffected: tokens are only checked at connect) and `rds-db:connect` scoping remain staging-plan items |
| SR12R-10 freshness / environment binding | **PARTIALLY IMPLEMENTED, rest recorded**: the restore prints `created_utc` and the SHA-256 of `SHA256SUMS` and **requires `RESTORE_EXPECT_SET_SHA256` (from the runner's out-of-band backup log) for any non-drill target**, which pins WHICH signed set may be restored (rollback protection); the verification key is copied before use; source = target environment is enforced (SR12R-5). **Decision recorded, not built:** one Ed25519 signing key and one pinned fingerprint **per environment** (a staging key can never verify a production set because the fingerprint is pinned per environment in change control); a maximum-age policy and a signed "latest" pointer need the backup runner and its log (gap G1) and are future work |
| Informational | `lib-globals.sh` header corrected; use of `RESTORE_OWNER_ROLE_OVERRIDE` is now logged |


## Amendment 2026-10-16 — section 10 DECIDED (owner decision 5)

**Decision: logical dumps exclude `actor_proof_keys.secret`; restore re-keys (owner decision 5, 2026-10-16; implemented and drilled locally)

Status: DECIDED. This section was an open question for the architect (option 1 keep, option 2 exclude); the owner chose option 2.

1. A logical dump never contains the rows of `public.actor_proof_keys`. `backup.sh` runs `pg_dump --exclude-table-data=public.actor_proof_keys` (structure kept) and verifies the
   exclusion on the encrypted stream itself by reading its table of contents; a data entry for the table fails the whole backup and keeps nothing. The signed manifest records
   `"actor_proof_keys_data": "excluded"`. The baseline hashes the table's non-secret columns only and `verify.sh` asserts the restored table instead of comparing it.
2. A restored database has no actor-proof key. `restore.sh` never restores the table's rows (a legacy set that carries them is restored without them, with a warning). Until a new key
   is provisioned every governed write fails closed (`AP002`) and a production `platform-api` refuses to start (`actorproof.VerifyConfiguredInProduction`).
3. Re-key is a documented, scripted step: `deploy/backup/rekey-actor-proof.sh` is `operational-runbooks.md` section 15 step 4 with mandatory preconditions (owner role, secret from a
   0600 tmpfs file as a bound parameter, statement logging checked, no existing key, a kid different from the source's). The key and kid come from the secret store; nothing is
   copied through a backup. The source's kid is retired by absence.
4. Physical base backups, snapshots and PITR contain the live key. They stay inside the AWS trust boundary and are what recovers the live key after a total loss; they are never
   copied out of it.
5. Residual: the backup role retains table-level SELECT on `actor_proof_keys` (pg_dump takes LOCK TABLE on every dumped table), so a stolen backup credential can read the key; the
   dump artefact cannot. Moving the secret to a table the backup role cannot read is a possible later change (a migration; architect).
6. Not proven: the re-key on RDS (pgaudit, parameter group, owner login), the HTTP K2/K3 flow on a re-keyed restored database (the drill proves the SQL verifier path and the startup gate).
Evidence: docs/plans/backup-exclude-actor-secret-record.md.

7. Security review (SRBX-3): the residual in item 5 is MEDIUM. Accepted for staging. It must be resolved BEFORE PRODUCTION by (a) a migration moving the key to a schema the backup role cannot read (architect), or (b) an owner-accepted interim of column-level pgaudit auditing on `secret` with a P1 alert, plus re-keying after any suspected backup-credential exposure. Option (c) (PostgreSQL 17 MAINTAIN) is unverified.


## Amendment 2026-10-16 (F1, migration 0166) - section 10 item 5 replaced

5. (F1, migration 0166) The actor-proof key table lives in the owner-only schema `actorproof_private`. `backup.sh` dumps with `--exclude-schema=actorproof_private`
   (pg_dump neither locks nor reads it), refuses to run if migration 0166 is not applied or if the backup role holds any privilege on that schema or its relations,
   and fails the backup if the stream carries any entry of that schema or any actor_proof_key* data entry in any schema. The backup role holds no privilege on the
   key at all (residual R1 is closed for databases at migration >= 0166; init-backup-role.sql re-asserts the denial). The baseline carries only the key schema's
   structure fingerprint and a privilege invariant (no key ids); `verify.sh` reads the key table's state as the owner. `restore.sh` recreates the schema with an EMPTY
   key table from the reviewed DDL `deploy/backup/actorproof-private-schema.sql` (compared with the source's structure by verify.sh). Physical backups, snapshots
   and PITR still contain the key (inside the AWS boundary). A backup credential that may have been exposed while a database was below 0166 still requires a re-key.
