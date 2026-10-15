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
clusters), `init-backup-role.sql` (takes a SCRAM verifier, never a password), `harden-backup-surface.sql`, `post-restore-grants.sql`, `baseline.sql`,
`scramhash/` (Go; computes the verifier on the client), `lib-sign.sh` (Ed25519 signing/verification of the set), `seed.sh` (+ the seeder
`internal/httpserver/drillseed_integration_test.go`, tags `integration drillseed`), `restorecheck/` (Go; platform reconciliation + startup guards),
libraries `lib-*.sh`. Static guards: `go test ./deploy/backup/` (no sudo/pkill, no RLS/trigger weakening outside the throwaway-only drill
scripts, no role DDL outside provisioning, no literal passwords, target guard present, role grants limited to SELECT/USAGE/CONNECT).

## 2. Roles and credentials (by reference, never in git)

| Role | Used for | Notes |
|---|---|---|
| `igaming` (migration owner) | `cmd/migrate`, owning objects, restore target owner | on RDS the master user; subject to FORCE RLS: **cannot** produce a complete dump |
| `igaming_runtime` | the application | non-owning; cannot dump (`permission denied for table actor_proof_keys`) |
| **`igaming_backup`** | the backup runner and `verify.sh` baseline/count queries | NOSUPERUSER, **BYPASSRLS**, SELECT-only, CONNECTION LIMIT 2, no membership, read-only sessions, **VALID UNTIL** (time-boxed), audited; never used by the application |
| restore role | `restore.sh --phase all` or `load` | the **non-superuser** owner role `igaming`, or a CREATEDB role that is a member of it (drill: `drill_restore`). It must NOT be a superuser or `rds_superuser` member: the archive is executable SQL (ADR 0122 section 5a). `--owner-role` is `igaming` unless `RESTORE_OWNER_ROLE_OVERRIDE` names another; postgres / igaming_backup / igaming_runtime / rdsadmin / rds_superuser are always refused |
| provisioning role | `restore.sh --phase provision` | on a brand-new cluster the bootstrap superuser (RDS: the master - unverified): creates the three project roles WITHOUT passwords and the EMPTY database only; no archive content runs as this role |

Credentials reach the scripts only as a **0600/0400 passfile** (`BACKUP_PGPASSFILE`) materialised by the runner from the secret store
(`backup_runner_secret_arn`); the scripts take no password in argv, environment or URL, and refuse a missing or too-open passfile.
An **expired** credential fails the backup closed with a message naming the likely cause (drilled). Re-mint: generate a fresh random
password, compute its verifier on the runner (`printf '%s' "$pw" | scramhash`) and re-run `init-backup-role.sql` with
`IGAMING_BACKUP_PASSWORD_VERIFIER` and `BACKUP_ROLE_VALID_UNTIL` (at most now + 48 h; the SQL refuses `infinity`). The password never
appears in statement text, so statement logging cannot capture it (drilled with `log_statement=all`). **On RDS the credential is IAM
database authentication, not a password** (ADR 0122 section 3, M8).

Signing and verification keys (by reference, never in git, never in the backup storage): `BACKUP_SIGNING_KEY_FILE` (Ed25519 private key, 0600, on the
backup runner only) signs every set; the restorer supplies `RESTORE_VERIFY_PUBKEY_FILE` plus `RESTORE_VERIFY_PUBKEY_SHA256` (the SHA-256 of the public
key's DER encoding, recorded in change control when the key pair is generated). A set that is unsigned, signed by another key, or altered after signing is
refused **before** any checksum, decryption or database contact; there is no switch to allow it. Generate a pair: `openssl genpkey -algorithm ED25519 -out
sign.pem; openssl pkey -in sign.pem -pubout -out sign.pub; openssl pkey -pubin -in sign.pub -outform DER | sha256sum`.

### 2a. Who may mint, extend or use the backup credential (break-glass, L6)

- **Routine use**: only the backup runner identity, on its schedule. A human never holds the credential by default.
- **Minting / extending `VALID UNTIL`**: the secret-store job under the runner's identity, or `devops` with a ticket. Maximum validity 48 h (enforced by the
  SQL); the routine mint is 24 h.
- **Break-glass (any human use of `igaming_backup`, any use outside the runner, any extension beyond the routine window)** needs: a requester, a
  **different** approver (four-eyes: `devops` + `security` or the owner delegate), a ticket stating the reason, a TTL of at most 1 hour, the
  session recorded (connection/disconnection lines and per-role statement logs/pgaudit), and a **re-mint of the credential straight afterwards**
  (so the credential used is dead). Human queries may put PII in `WHERE` clauses into the statement logs: keep break-glass sessions to the
  pre-agreed statements and review the log lines afterwards.
- On RDS with IAM database authentication the same rule applies to who may obtain a token: the `rds-db:connect` permission on the one database user is
  the break-glass grant, time-boxed and logged in CloudTrail.

Target guard (all scripts): the host is **canonicalised** (lower-cased, brackets and trailing dots stripped; a comma list, whitespace, userinfo, a slash, a
backslash or `%` is rejected), IPv6 literals are parsed by a real parser (`::` compression, leading zeros, embedded IPv4; an invalid literal is refused) and names are resolved (`getent ahosts`);
a remote target additionally requires `sslmode=verify-full` with `BACKUP_PGSSLROOTCERT` (the CA bundle) and a private `BACKUP_TMPDIR`; anything that resolves to this machine (`127.0.0.0/8` including `127.1` and `127.0.0.2`, `0.0.0.0`,
`::`, `::1`, `::ffff:127.x`, `LOCALHOST`, `localhost.`, `[::1]`, this host's own name) is "local", and local on port 5432 (the shared development/CI cluster) is
**always refused with no acknowledgement that overrides it**; a local target must be a drill port (`BACKUP_DRILL_PORTS`, default 5499 5498 5497); a remote host
(staging RDS, later) is refused unless `BACKUP_ALLOW_REMOTE_TARGET=<exactly that host>` is set for that invocation. Production is not authorised.

## 3. Taking a logical backup (secondary mechanism)

```sh
export BACKUP_PGHOST=<host> BACKUP_PGPORT=<port> BACKUP_PGDATABASE=<db> BACKUP_PGUSER=igaming_backup
export BACKUP_PGPASSFILE=<0600 file from the secret store>
export BACKUP_ENCRYPT_AGE_RECIPIENT=<age public recipient>      # or BACKUP_ENCRYPT_GPG_RECIPIENT=<FULL 40-hex fingerprint>; public references only
export BACKUP_SIGNING_KEY_FILE=<0600 Ed25519 private key from the secret store>
export BACKUP_TMPDIR=<tmpfs or encrypted scratch directory>      # working material; 0700 per-run subdirectories, swept at the next start
deploy/backup/backup.sh --out-dir <backup volume> --label <reason>
```

`backup.sh` prints `RUN_DIR=...` and, after signing, **`SET_SHA256=<hex>`** (the SHA-256 of `SHA256SUMS`). **The runner records that value out of band** (the backup job's
log / the ticket for the run, in a store the backup storage's writers cannot edit); at restore time it is supplied as `RESTORE_EXPECT_SET_SHA256` and pins WHICH signed
set may be restored (rollback / substitution protection). Without a recorded value a non-drill restore is refused.

The recipient and the signing key are **validated before anything is dumped** (a probe encryption and a probe signature): a mistyped recipient, an unknown
gpg key or an unusable signing key stops the run with nothing written. The dump is streamed through the encryptor, so no plaintext archive is ever on the
backup volume; if anything fails (including a killed encryptor, SIGTERM/SIGINT, a closed stdout) the whole run directory is removed. The next run also sweeps
stale temp directories and orphaned `.incomplete` run directories of killed (SIGKILL/OOM) runs.

Operational notes (re-review informational items):
- `BACKUP_IAM_TOKEN_COMMAND` is executed with `bash -c`: it is **trusted operator configuration** and must never be taken from a backup set, a ticket field or any
  other input an outsider can influence.
- `BACKUP_ROLE_IAM_ONLY=1` creates a role with **no password and no expiry**. On RDS that is the intended state (login needs an IAM token). On any other server it is safe
  only where `pg_hba.conf` has **no `trust` (or `peer`/`ident` mapping) rule** that admits that role; do not use it on non-RDS targets without checking `pg_hba`.
- `verify.sh` passes the `VERIFY_*_DATABASE_URL` values to the driver, and an explicit `sslmode=` in a URL overrides the environment: use `sslmode=verify-full` with
  `sslrootcert=<CA bundle>` in those URLs for a remote target (the local drill uses `sslmode=disable` on loopback only).
- Backup-directory entries are copied into the private work directory **by content** and checksummed there; a symlink swapped in after the scan only yields bytes that
  must still match the signed hash (drilled).

### 3a. Production: dump from a clone, and detect a long snapshot (M7)

A logical dump holds one `REPEATABLE READ` snapshot for its whole duration (about 40 minutes at 50 GB by the extrapolation in section 6), which holds back vacuum on
the instance it runs on. **In production take the logical dump from a short-lived instance restored from PITR or a snapshot (or from a replica), never from the
primary**, and destroy the clone afterwards; staging and the local drill dump the instance directly. Detection for any instance where a dump runs:

```sql
SELECT pid, usename, state, now() - xact_start AS xact_age, age(backend_xmin) AS xmin_age, left(query, 60)
  FROM pg_stat_activity WHERE usename = 'igaming_backup' ORDER BY xact_start;
```

Alert/page when `xact_age` exceeds the planned dump time plus a margin, when a session of `igaming_backup` exists outside the backup window, or when it holds a lock
mode other than AccessShare (`pg_locks`). No metric exports this yet (`OBS-BACKUP-ABUSE-1`, ADR 0122 section 5b); until it does this query is run by hand during a dump.
The backup keeps its snapshot-holder session alive with a keepalive (`BACKUP_KEEPALIVE_SECONDS`, default 60) and fails loudly if the holder dies.

Output (one 0700 directory): `db.dump[.age|.gpg]`, `globals.sql[.age|.gpg]` (roles and attributes, **no passwords**), `baseline.txt[.age|.gpg]`
(exact row counts and content hashes of every table, ledger invariants, sequences, roles and ACLs by name, triggers, policies, RLS flags,
catalog fingerprints, all evaluated in the **same exported snapshot as the dump**, so a live source is fine), `manifest.json` (includes the
**source `environment_anchor`** and an `encryption` field derived from the files actually written), `SHA256SUMS` (lists every artefact including `manifest.json`)
and `SHA256SUMS.sig` (the Ed25519 signature). It refuses: a non-BYPASSRLS or superuser role, a role that cannot SELECT every table, an unencrypted dump
(unless `BACKUP_ALLOW_UNENCRYPTED=synthetic-data-only` **and** the target is a local throwaway drill instance), an encryption recipient or signing key that
does not work, insufficient disk, an existing output directory. Exit 4 = written but the source
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
3. Fetch the newest backup set. Keep the decryption identity in the restorer's secret store (`RESTORE_AGE_IDENTITY_FILE=<path>` for age; gpg uses the
   restorer's keyring), and have the **pinned verification key** and its fingerprint (`RESTORE_VERIFY_PUBKEY_FILE`, `RESTORE_VERIFY_PUBKEY_SHA256`) from change
   control, and `RESTORE_EXPECT_SET_SHA256` (the SHA-256 of `SHA256SUMS` from the backup runner's out-of-band log: it pins WHICH set; `restore.sh` prints `created_utc` and the
   hash). The backup directory must contain only the signed files (no extra file, symlink or sub-directory: refused). State what you believe you are restoring: `RESTORE_EXPECT_SOURCE_ENVIRONMENT=<production|development|test|staging|unrecorded>` (it must equal the set's
   signed source anchor) and `RESTORE_TARGET_ENVIRONMENT=<environment of the target>`, which **must equal the source**: a development/staging copy can never enter another
   environment, and nobody ever has to record `production` on a copy.
4. **Two phases, two identities** (a dump is executable SQL; ADR 0122 section 5a). On the new empty cluster:
   - `RESTORE_CONFIRM=<new db> deploy/backup/restore.sh --backup-dir <set> --new-database <new db> --apply-globals --phase provision` as the bootstrap
     superuser: verifies the **signature first** (before checksums, decryption or any database contact), then the checksums, then creates the three project roles
     **without passwords** from allow-listed statements and an **empty** database owned by `igaming`. No archive content runs as this role.
   - Re-provision the owner's (and `igaming_runtime`'s) credential from the secret store.
   - `RESTORE_CONFIRM=<new db> deploy/backup/restore.sh --backup-dir <set> --new-database <new db> --phase load` as **`igaming` (a non-superuser)**:
     decrypt into a private 0700 temp directory, archive must parse, anchor expectation checked, `pg_restore --single-transaction --exit-on-error --no-owner
     --role=igaming` (all or nothing), the `database_environment` data is **not** restored, `post-restore-grants.sql`, `ANALYZE`. A superuser or
     `rds_superuser` member is refused for this phase. On any failure the database this run created is dropped.
   On a cluster where the roles and an ordinary non-superuser restore role already exist, `--phase all` (the default) does both with that one identity.
5. **Re-provision what a dump cannot carry**: role passwords (`igaming_runtime`, `igaming`, `igaming_backup`) from the secret store;
   the complete environment role-provisioning script (`deploy/aws/sql/init-runtime-role.rds.sql`); **`deploy/backup/harden-backup-surface.sql`** (the
   `pg_catalog` function REVOKEs are dumped but a non-superuser restore cannot apply them: drilled, the backup role could take advisory locks on the restored cluster until it was re-run);
   the **`database_environment` anchor** (section 5);
   the secret-store material that is not in any database (JWT secrets, staff MFA key families, payout-instrument keys,
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
| **`database_environment` anchor** | a logical restore does **NOT carry the anchor** (its data is excluded from the restore): the restored database is **blank/unrecorded** and a production `platform-api` **refuses to start** until the owner records it (drilled with a development-source backup: `verify.sh` asserts 0 rows and that the production guard refuses; recorded truthfully as `development` the guard still refuses) | **the owner first verifies that the SOURCE environment of the backup was production** (the set's `manifest.json` `environment_anchor`, the value `restore.sh` required in `RESTORE_EXPECT_SOURCE_ENVIRONMENT`, and the ticket for the incident), and only then, once: `INSERT INTO database_environment (environment) VALUES ('production');` then re-run `restorecheck -expect-anchor production`. Never record `production` on a restored development/test/staging copy (that turns the guard into a rubber stamp); never restore such a copy into production (`restore.sh` refuses it). Physical recovery (PITR) keeps the anchor: it is the same database |
| Backup-surface hardening | pg_catalog function ACLs are dumped but cannot be applied by the non-superuser load | re-run `deploy/backup/harden-backup-surface.sql` as the bootstrap superuser (ADR 0122 section 5b) |
| Secret-store material | not in any database backup | restore from the secret store: JWT secrets, `STAFF_MFA_*` key families, payout-instrument keys, provider credentials. (`actor_proof_keys` IS in the database: the dump is therefore a signing-key-bearing artefact) |
| Reconciliation | prove no drift in the running system | run the scheduler once (or `restorecheck -persist`) so a `reconciliation_runs` row exists; require 0 mismatches before traffic |
| Provider-side state | the restore point is earlier than provider events | reconcile PSP statements and casino rounds since the restore time; do not assume vendors rolled back |
| Alert routes, credential handles | live in the DB; a restore reverts them to the restore time | check `/v1/admin/alerting/status`, re-apply changes |
| Communication | | incident channel update at: restore started, restore point chosen, verify PASS, traffic restored; notify affected tenants and compliance if committed financial history was discarded |

### 5a. After ANY suspected exposure of a backup set, a dump or the backup credential: rotate

A logical dump (and so a backup set, a decrypted copy, a restored database) carries `actor_proof_keys.secret` in the clear (inside the encrypted artefact) and every
password hash. Treat a suspected exposure (lost or unencrypted set, leaked decryption identity or signing key, credential used outside the runner, an
untrusted restore target) as a security incident and, without waiting for proof:

1. **Rotate the signed-actor-proof keys**: follow `operational-runbooks.md` section 15 (mint a new `actor_proof_keys` row, switch the application to it, retire the
   old kid); in-flight proofs under the old key stop verifying, by design.
2. **Rotate the other signing material that was in or protected next to the dump**: JWT signing secrets, the staff MFA key families, the payout-instrument keys, provider
   credentials and webhook secrets, per their own runbooks; revoke sessions (the dump holds `sessions.refresh_token_hash` and token MACs).
3. **Reset staff passwords** and force MFA re-enrolment where the staff MFA ciphertext and its key family could both have been reached; player password hashes are
   offline-crackable: require password resets when the exposure is credible.
4. Rotate the backup **signing key** and re-pin the verification fingerprint if the signing key may have been exposed; rotate the **age/gpg** recipient and re-encrypt or
   destroy old sets if the decryption identity may have been exposed; re-mint (or revoke the IAM grant of) the backup credential.
5. Record the incident, the sets involved and their disposition. Whether logical dumps should exclude `actor_proof_keys.secret` altogether is an open architect
   decision (ADR 0122 section 10; recommendation: exclude and re-key on restore, with PITR covering physical recovery).

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
| Tampered `globals.sql`, re-sealed with a VALID signature (`ALTER ROLE igaming WITH SUPERUSER`; a compromised pipeline) | `--apply-globals` refuses it (allow-list `lib-globals.sh`, defence in depth); no role and no database created | PASS |
| Unsigned set; set re-sealed and signed by an attacker's key; verification key placed inside the backup directory; wrong or missing pinned fingerprint; manifest altered after signing; checksum list altered; garbage signature | refused **before** checksums, decryption or any database contact (the drill counts new connections: 0) | PASS |
| Archive whose expression-index function runs `COPY ... TO PROGRAM` (validly signed) | fails harmlessly as the non-superuser restore role: transaction rolled back, database dropped, no file created; the superuser is refused outright; **control**: the naive `pg_restore` as a superuser DID run the command | PASS (incl. control) |
| Mistyped / malformed age recipient, unknown or short gpg id, `=yes` or remote-target unencrypted override | `backup.sh` refuses **before dumping**; no run directory, no plaintext, no temp files | PASS |
| Encryptor killed mid-stream | backup fails, run directory removed, no plaintext | PASS |
| Snapshot holder killed, or killed by the server's idle timeout | backup fails loudly and keeps nothing; **with** the keepalive a 3x-timeout delay is survived (control without it fails) | PASS |
| SIGTERM / SIGINT during the dump with a `.partial` ciphertext present; stdout closed (SIGPIPE) | run directory and temp directory removed, holder session gone, encryptor child terminated, no plaintext | PASS |
| SIGKILL/OOM leavings (stale temp dir, orphaned `.incomplete` run directory) | swept at the next start | PASS |
| `--owner-role` postgres / igaming_backup / igaming_runtime / rdsadmin / rds_superuser / an unlisted name | refused (the unlisted name needs `RESTORE_OWNER_ROLE_OVERRIDE`); nothing created | PASS |
| Restore of a development-source set asserting `production`; dev-source restore then starting as production | refused / anchor blank so the production guard refuses until the owner records it | PASS |
| Aliases of the shared cluster (`127.0.0.2`, `127.1`, `0.0.0.0`, `::ffff:127.0.0.1`, `LOCALHOST`, `localhost.`, `[::1]`, `a,127.0.0.1`, this host's name) | refused before any connection (24 spellings unit-tested in `go test ./deploy/backup/`) | PASS |
| Restore as the wrong role (`igaming_runtime`, `igaming_backup`) | refused with an explanation, nothing created | PASS |
| Restore into an existing (non-empty) database | refused, the existing database is untouched | PASS |
| Insufficient disk | `backup.sh` / `restore.sh` refuse up front (`BACKUP_MIN_FREE_MB`, `RESTORE_MIN_FREE_MB`). **A real `ENOSPC` part-way through is NOT simulated**: a failed `pg_restore --single-transaction` rolls back, and `restore.sh` drops the database it created; a full disk on the *server* can still stop PostgreSQL itself - provision restore targets with at least the database size plus WAL headroom | check PASS; ENOSPC documented only |
| WAL gap in the archive | the continuity check names the missing segment; recovery to the target does not promote (`recovery ended before configured recovery target was reached`) | PASS |
| Backup role credential expired | `backup.sh` fails closed with an "expired credential" message, no partial output | PASS |
| Backup credential file missing / too open | refuses, nothing connected | PASS |
| Backup run as the owner role (or a superuser) | refused before dumping: not BYPASSRLS / superuser | PASS |
| Wrong target (5432, unknown loopback port, remote host without acknowledgement) | refused without connecting | PASS |
| The backup role's abuse primitives (advisory locks, `pg_logical_emit_message`, large objects, `NOTIFY`, `CONNECT` to postgres/template1) | denied after `harden-backup-surface.sql`; the platform roles keep their advisory locks; the revokes are **gone after a full-loss restore until the script is re-run** | PASS |
| Backup credential minted with statement logging on | the password is not in the server log (control: a naive `PASSWORD :'pw'` is) | PASS |
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
- Backup-of-the-backup; automatic scheduling and alerting on backup failure or stale `latest_restorable_time`; the `OBS-BACKUP-ABUSE-1` signal.
- An asymmetric-**KMS** signing key (the drill signs with an ephemeral Ed25519 key file, the pinned fingerprint is supplied by the operator); gpg end to end (only
  its refusals are drilled); IAM database authentication and `REVOKE` on `pg_catalog` functions **on RDS**; a restore load as a non-`rds_superuser` owner on RDS.
- The security review is recorded (`docs/plans/prh2-hardening-round/a12-security-review.md`); its dispositions are in ADR 0122 section 9. Open: the architect
  decision on excluding `actor_proof_keys.secret` from logical dumps (ADR 0122 section 10).

## 9. Running the drill

```sh
deploy/backup/drill.sh                     # full: ~4 tenants x 24 players, >=100k ledger entries (about half an hour on a busy VM)
deploy/backup/drill.sh --small             # smoke: ~1k entries, about 3 minutes
deploy/backup/drill.sh --skip-pitr         # without the physical PITR phase
go test ./deploy/backup/                   # static guards (cheap, no database)
```

Needs: PostgreSQL 16 server binaries at `/usr/lib/postgresql/16/bin`, `runuser` and the OS user `postgres` (the clusters run as `postgres`
via `runuser -u postgres`, a privilege **drop**; initdb refuses root), `age`/`age-keygen`, OpenSSL >= 3.0 (Ed25519 signing), `ps`, Go, ~1.5 GB free in `/tmp` (the drill checks and aborts
cleanly below its thresholds). It creates `/tmp/pgdrill-XXXXXX`, clusters on ports 5499 (source), 5498 (full-loss target), 5497 (PITR recovery),
stops them with `pg_ctl` and removes the directory on exit, failure, INT or TERM. It never contacts 5432, uses no sudo or pkill, creates roles only
inside its own clusters, and uses random per-run passwords that live only in 0600 passfiles inside the drill directory.
