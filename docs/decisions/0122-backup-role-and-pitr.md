# ADR 0122 - Dedicated audited backup role (BACKUP-ROLE-1) and managed point-in-time recovery

- **Status:** ACCEPTED (2026-10-15, owner decision 7: "backup architecture = dedicated audited backup role + managed
  point-in-time recovery (RDS PITR) with logical dumps as secondary"). Recorded as PROPOSED by the A12 work and accepted on
  that owner decision. **The security review of the role design is still to follow** (probes listed in section 9); until it
  is recorded this ADR is accepted for the DESIGN and the LOCAL drill only. **Nothing here is applied to any AWS or shared
  environment.**
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
| credential | password from the secret store by **reference** (`BACKUP_SECRET_ARN`), never in the repository, argv or a URL; `VALID UNTIL` set (time-boxed, default 24 h in the SQL, 1 h in the drill) and re-minted by the backup job; written to a 0600 passfile by the runner | JIT/short-TTL; expiry fails the backup **closed** (drilled) |
| use | the backup runner only; never the application, never a human by default; use outside the runner is a break-glass event with four-eyes approval (runbook) | blast-radius control |
| network | reachable only from the backup runner's security group / host (pg_hba or SG rule at deploy time) | a BYPASSRLS credential is useless from elsewhere |
| audit | `log_connections`, `log_disconnections` cluster-wide; per-role statement logging (`ALTER ROLE ... SET log_statement='all'` on stock PostgreSQL, **pgaudit** on RDS: `pgaudit.log` set for the role, logs exported to CloudWatch); every dump is also a signed-off event in the runbook (who/when/why) | non-repudiation; the drill counts the log lines |

**What `backup.sh` enforces before dumping** (fail closed): the role is not a superuser, has `BYPASSRLS`, has none of
CREATEROLE/CREATEDB/REPLICATION, runs read-only by default, and can `SELECT` every table and sequence (a table added by a later
migration without the default privilege would otherwise produce a dump that silently omits it); an unencrypted dump is refused
unless the operator acknowledges synthetic data.

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

A logical backup set contains PII and financial data. Required: encryption before it leaves the runner (`BACKUP_ENCRYPT_AGE_RECIPIENT`
or `BACKUP_ENCRYPT_GPG_RECIPIENT`, a public reference; the private key lives in the restorer's secret store/KMS and **no key
material is committed**), a SHA-256 manifest verified before any restore, restricted storage (versioned, encrypted, access logged),
a defined retention, and deletion on expiry. `SHA256SUMS` detects corruption and truncation, **not** a malicious replacement of
both the file and the manifest: signing the manifest (KMS signature or `age`/minisign signature) is an open hardening item (section 9).

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
other secret-store material.

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
  `BYPASSRLS` role, pgaudit availability and per-role configuration on RDS PostgreSQL 16, CloudWatch export of audit lines).
- KMS key policy, cross-account/cross-region copies, S3 storage of logical backup sets, a real backup runner identity and schedule.
- Production-size timings (the numbers in the record are an extrapolation at a small synthetic scale, labelled as such).
- The security review of this design and the signed-manifest hardening (section 9).

## 9. Security review probes (to follow)

1. Attempt to use `igaming_backup` as a stepping stone: `SET ROLE`, function calls with side effects (SECURITY DEFINER functions that
   write, `EXECUTE` granted to PUBLIC by default on functions), large-object access, `pg_stat_*`/`pg_read_*` reach, `COPY ... PROGRAM`,
   `dblink`/FDW if present, extension functions, `LISTEN/NOTIFY`, advisory-lock denial of service against production writers.
2. Does a `REPEATABLE READ` dump transaction held for hours bloat/hold back vacuum (availability) - acceptable bound? `statement_timeout=4h` appropriate?
3. Is `BYPASSRLS` + `SELECT` on `actor_proof_keys`, `staff_mfa_*`, `payout_instruments*`, `kyc_documents` acceptable, or must those
   tables be excluded from logical dumps (and covered only by PITR)? Concretely (columns read on the drill database):
   `actor_proof_keys.secret` is the signing secret itself (a stolen dump can forge ADR 0110 actor proofs until the key is rotated -
   `operational-runbooks.md` section 15); `staff_mfa_factors.secret_ct/secret_nonce` and `payout_instruments.detail_ciphertext` are
   ciphertext under keys held in the secret store (a dump alone does not open them); `staff_*`/`player_accounts.password_hash`,
   `sessions.refresh_token_hash` and `*_token_hash/_mac` are hashes/MACs; `kyc_documents` holds references, not content.
   Exclusion (`pg_dump --exclude-table-data`) would make the logical set incomplete by design and needs a documented key
   re-provisioning step on restore; the alternative is to accept the dump as a signing-key-bearing secret (encryption mandatory, as built).
4. Credential lifecycle: who may mint/extend `VALID UNTIL`, where the passfile is materialised, residue in process lists, the runner's logs.
5. Manifest forgery (replace dump + SHA256SUMS): require a signature before restore.
6. `restore.sh` privilege: the restore role's reach, `--apply-globals` creating `BYPASSRLS` roles from a (possibly tampered) globals.sql -
   the script filters to the three named project roles; review that filter.
7. Break-glass use of the backup credential outside the runner and its four-eyes control.
8. Logging: that `log_statement='all'`/pgaudit output for the role does not capture data values that are themselves sensitive.
9. Network exposure of the database to the backup runner (security group scope) and TLS enforcement (`rds.force_ssl` already on).
