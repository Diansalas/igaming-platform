# AWS staging deploy plan: the ONE named-commit verification deploy (A12, ADR 0122)

Status: **PREPARED, NOT APPLIED, NOT AUTHORISED YET.** The owner authorised exactly one AWS staging deploy *later* (decision
2026-10-15). Until that authorisation is given, nobody runs `terraform apply`, `deploy.sh`, any `aws` command or touches AWS from
this checklist. No AWS credentials were used to write it, `terraform` was not installed where it was written (so **none of the
Terraform in this change was formatted, validated or tested**: first step 0.2 below), and nothing here was exercised against RDS.

Purpose: one deploy of one **named commit** that verifies, in this order, everything the local drill could not: runtime roles,
`search_path`, secrets, database permissions, migrations, **PITR restore to a timestamp**, **logical backup/restore**, observability
and alert delivery. Every item is a checkbox with the command and the expected result; any unexpected result means **STOP and
report** (no workaround, no privilege escalation, no production credential, no real data). Synthetic data only.

Read first: `docs/runbooks/stage-9-4-staging-lifecycle-runbook.md`, `stage-9-4-aws-account-verification.md`,
`plat-rolesplit-staging-verification.md`, `backup-restore-dr.md`, `docs/decisions/0122-backup-role-and-pitr.md`,
`docs/decisions/0086-stage-9-4-staging-hardening-and-cost-optimization.md`.

## What the prepared Terraform changes (staging root `deploy/aws/environments/staging`, module `modules/database`)

| Setting | Value | Variable |
|---|---|---|
| automated backups = PITR window | 14 days | `db_backup_retention_days` |
| backup window | 01:00-02:00 UTC (maintenance 04:30-05:30 Sun) | `db_backup_window` |
| deletion protection | on | `db_deletion_protection` |
| final snapshot on destroy | yes, named | `db_skip_final_snapshot=false`, `db_final_snapshot_identifier` |
| `copy_tags_to_snapshot` | on (already) | module |
| storage / snapshot / PITR encryption | on; AWS-managed `aws/rds` key unless `db_kms_key_arn` references an existing CMK | `db_kms_key_arn` |
| CloudWatch log export | `postgresql`, `upgrade`, in pre-created groups with retention | `db_log_exports`, `log_retention_days` |
| parameter group | `rds.force_ssl`, `log_connections`, `log_disconnections`, `shared_preload_libraries=pgaudit` (reboot) | `db_enable_pgaudit` |
| backup-runner credential | **IAM database authentication is REQUIRED** (`iam_database_authentication_enabled = true` in the module; `GRANT rds_iam TO igaming_backup`, no password; ADR 0122 section 3, review M8). The Secrets Manager ARN below is the fallback reference for a password-based role ONLY if IAM auth is shown unusable (record why; this is then a conscious exception) | `backup_runner_secret_arn` (reference only: nothing created, no value) |
| deployer IAM (prepared, needs security review and the bootstrap credential to apply) | `rds:CreateDBSnapshot` on `snapshot:igaming-staging-*`; `logs:*` group actions on `/aws/rds/instance/igaming-staging-*` | `deploy/aws/iam/staging-deployer-infra-policy.json` |

Consequences to accept knowingly: `deploy.sh down` is no longer one step (section 14); the final snapshot and the retained
automated backups are deliberately left behind and `verify-teardown.sh` will list them; a destroy refuses if the snapshot name
already exists.

## 0. Before anything is applied

- [ ] 0.1 Record the **named commit** (full SHA) and the authorisation (who, when) in the evidence record at the end of this file.
- [ ] 0.2 `terraform fmt -recursive -check deploy/aws` and `deploy/aws/tests/run-static-checks.sh` (fmt, validate, `terraform test`
      with mock providers, no credentials). Expect green. **First-ever run for this change: fix what it finds, re-run, commit.**
- [ ] 0.3 `go test ./deploy/backup/ ./deploy/drill/` and the local drill `deploy/backup/drill.sh` green on the same commit.
- [ ] 0.4 The security review of ADR 0122 is recorded (`docs/plans/prh2-hardening-round/a12-security-review.md`; dispositions in ADR 0122
      section 9); the **IAM policy delta** is reviewed with the M6 caveat below; then the
      bootstrap credential applies the policy change (`deploy/aws/iam/`); `python3 deploy/aws/tests/simulate-deployer-policies.py`
      (read-only IAM simulation) shows every expectation held, including the five new A12 cases.
- [ ] 0.5 Decide `db_kms_key_arn` (null = AWS-managed key is acceptable for staging; a CMK needs a key policy that lets RDS and the
      restore principal use it: not designed here). **If a CMK is used, the deployer needs `kms:DescribeKey` and `kms:CreateGrant`
      (RDS creates grants for the instance, snapshots and the PITR restore) on that key, and the restore principal needs
      `kms:Decrypt`, `kms:GenerateDataKey*`, `kms:ReEncrypt*`, `kms:CreateGrant`, `kms:DescribeKey`; neither is in
      `staging-deployer-infra-policy.json` today** (functional gap found by the security review, not a security finding). Add them scoped to
      that one key ARN in the key policy and the deployer policy before the apply; do not use `kms:*` or `Resource: "*"`.
- [ ] 0.6 **Accepted staging-only risk (review M6), with its production requirement.** The staging deployer role can
      (a) delete a log group or shorten its retention (`logs:DeleteLogGroup`, `PutRetentionPolicy`, `DeleteRetentionPolicy` on
      `/aws/rds/instance/igaming-staging-*`) and read the audit lines (`GetLogEvents`, `FilterLogEvents`), and (b) set
      `BackupRetentionPeriod = 0` or delete the instance with `--skip-final-snapshot --delete-automated-backups`
      (`rds:ModifyDBInstance`, `rds:DeleteDBInstance` on `db:igaming-staging-*`); the Terraform preconditions bind only Terraform, not
      the IAM principal. This is **accepted for staging** (synthetic data, disposable). **Production requires**: (i) audit log groups in a
      separate account, or behind a resource policy the deployer cannot change, with a CloudWatch Logs data-protection policy masking
      secrets; (ii) immutable recovery points (AWS Backup vault lock with cross-account copy, or an SCP denying `DeleteDBInstance`
      without a final snapshot and denying retention 0); (iii) a **separate break-glass role** for retention/log changes, with
      four-eyes approval and CloudTrail alerting, that the routine deployer does not hold; (iv) the deployer has no read access to the
      audit logs. Do not copy the staging statement into a production policy.
- [ ] 0.7 `deploy/backup/` prerequisites on the runner: OpenSSL >= 3.0 (Ed25519 signing), `age`, the **signing private key** (secret
      store, 0600, never in the backup storage) and the **pinned verification fingerprint** held in change control, not next to the
      backups (ADR 0122 section 5).

## 1. Plan

- [ ] 1.1 `deploy/aws/scripts/deploy.sh up` (interactive). Read the plan: only the database module, its parameter group, the two
      log groups and outputs change on an existing deployment; on a fresh one everything is created. Expect: `backup_retention_period=14`,
      `deletion_protection=true`, `skip_final_snapshot=false`, `enabled_cloudwatch_logs_exports=[postgresql,upgrade]`, parameter group
      with `shared_preload_libraries=pgaudit` as `pending-reboot`. **Any change to the VPC, security groups or IAM roles that you
      did not expect: stop.**
- [ ] 1.2 Apply (type `yes`). Capture `terraform output database_recovery`: `backup_retention_days=14`, `deletion_protection=true`.

## 2. pgaudit reboot (only if `db_enable_pgaudit=true`)

- [ ] 2.1 `aws rds describe-db-instances --db-instance-identifier igaming-staging-db --query 'DBInstances[0].DBParameterGroups'`
      shows `pending-reboot`. Reboot in the maintenance window or now (staging): `aws rds reboot-db-instance --db-instance-identifier igaming-staging-db`.
- [ ] 2.2 After reboot: parameter group status `in-sync`; as the master user `SHOW shared_preload_libraries;` contains `pgaudit`;
      `CREATE EXTENSION pgaudit;` succeeds (**if the engine refuses, record it: ADR 0122 falls back to connection logging only**).

## 3. Runtime roles and `search_path`

- [ ] 3.1 Run the role-init one-off task (`deploy.sh` role-init / `init-runtime-role.rds.sql`). Expected: role `igaming_runtime`
      LOGIN, NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS NOREPLICATION, owns nothing:
      `SELECT rolname,rolsuper,rolbypassrls,rolcreaterole,rolcreatedb,rolreplication FROM pg_roles WHERE rolname LIKE 'igaming%';`
- [ ] 3.2 `search_path`: `SHOW search_path` for `igaming` and `igaming_runtime` sessions is the expected `"$user", public`, and the
      SECURITY DEFINER functions carry their `search_path` pin (the `FP|functions_md5` / `secdef_functions_without_search_path_pin`
      value from `baseline.sql` is the number to compare with the local drill's).
- [ ] 3.3 `REVOKE TEMPORARY` state (migration 0116): `SELECT has_database_privilege('igaming_runtime', current_database(), 'TEMPORARY');` = `f`.

## 4. Secrets

- [ ] 4.1 The three Terraform-generated secrets exist and none appears in state (`terraform state pull | grep -c secret_string` = 0
      apart from names/ARNs): `db_runtime`, `jwt_signing`, `seed_admin_password`; the RDS-managed master secret exists.
- [ ] 4.2 **Backup-role credential.** Preferred and required: IAM database authentication (no secret exists). Create the runner's IAM policy
      allowing `rds-db:connect` on `arn:aws:rds-db:eu-central-1:<account>:dbuser:<DbiResourceId>/igaming_backup` ONLY, attach it to the backup
      runner identity (**gap G1: the runner identity does not exist yet**), and confirm `aws rds generate-db-auth-token` + `psql` works
      (token valid 15 minutes, `sslmode=require`; the passfile for the scripts is built from the token at run time, never stored).
      *Exception path only if IAM auth is unusable (record why)*: create a Secrets Manager secret `igaming-staging/db-backup`
      (`{"username":"igaming_backup","password":"<generated>"}`) with a generator, never a typed value; pass its ARN as
      `-var backup_runner_secret_arn=<arn>`; resource policy limited to the runner. The password then NEVER goes to the server as text:
      compute the SCRAM verifier on the runner (`go build ./deploy/backup/scramhash`; `printf '%s' "$pw" | scramhash`) and pass
      `IGAMING_BACKUP_PASSWORD_VERIFIER` to `init-backup-role.sql`.
- [ ] 4.3 Nothing secret in logs: CloudWatch `/ecs/igaming-staging/role-init` contains no password (the script never echoes it).

## 5. Database permissions (the backup role)

- [ ] 5.1 Provision `igaming_backup` with `deploy/backup/init-backup-role.sql` as the master user, from a one-off task or a
      bastion **inside the VPC** (**gap G1: no ECS task definition for this exists yet; building it is part of this deploy's
      scope or the step is done from an allow-listed operator session, recorded**). With IAM auth: run the SQL with a throwaway verifier
      (compute it from a random value you then discard, so nobody knows the password), then `GRANT rds_iam TO igaming_backup;`. The SQL bounds
      the password validity to 48 h, so after expiry only the IAM token path can log in.
      `BACKUP_ROLE_VALID_UNTIL` = now + 24 h. **The minting session must not be statement-logged with the password**: the SQL only ever
      receives a verifier, but still run it from a session whose `log_statement`/pgaudit settings you have checked (the verifier is
      non-secret but the habit matters), and read the CloudWatch lines for the minting session afterwards (step 4.3).
      **Unverified on RDS: whether the master user may `CREATE ROLE ... BYPASSRLS`.** If RDS refuses, STOP: ADR 0122 section 3 then
      has no logical-backup role on RDS and logical dumps are not available there (PITR remains the primary path).
      Then apply `deploy/backup/harden-backup-surface.sql` as the master (`REVOKE ... FROM PUBLIC` on the `pg_catalog` advisory-lock /
      `pg_logical_emit_message` / large-object / `pg_notify` functions; `REVOKE CONNECT` on `postgres` and `template1`).
      **Unverified on RDS: whether the master may revoke on `pg_catalog` functions; record the result**, and `GRANT CONNECT ON DATABASE postgres`
      back to the operator roles that need it.
- [ ] 5.2 Attributes: `SELECT rolname,rolsuper,rolbypassrls,rolcreatedb,rolcreaterole,rolreplication,rolconnlimit,rolvaliduntil FROM pg_roles WHERE rolname='igaming_backup';` = f,t,f,f,f,2,<future>.
- [ ] 5.3 Probes (all must be denied): as `igaming_backup` run the 15 statements of `drill.sh` phase 2 **and the 8 abuse probes after them**
      (`pg_advisory_lock`, `pg_try_advisory_lock`, `pg_advisory_xact_lock`, `pg_logical_emit_message`, `lo_create`, `pg_notify`, connect to
      `postgres`, connect to `template1`; any that succeeds is recorded as a residual with its detection, ADR 0122 section 5b) (`INSERT`, `UPDATE`, `DELETE`,
      `TRUNCATE`, `CREATE TABLE`, `CREATE TEMP TABLE`, `DROP TABLE`, `ALTER TABLE ... DISABLE TRIGGER`, `SET ROLE igaming`,
      `SET session_replication_role`, `CREATE ROLE`, `ALTER ROLE ... SUPERUSER`, `COPY ... PROGRAM`, `pg_read_file`, `pg_reload_conf`).
- [ ] 5.4 Per-role audit: `ALTER ROLE igaming_backup SET pgaudit.log = 'read, write, ddl, role';` (if pgaudit is present), then a dump
      produces `AUDIT:` lines in `/aws/rds/instance/igaming-staging-db/postgresql`.

## 6. Migrations

- [ ] 6.1 `deploy.sh migrate` then `cmd/migrate verify` (all applied migrations verified clean, no version gaps).
- [ ] 6.2 `database_environment`: staging is not production; leave unrecorded or record `staging` per ADR 0115 (and note the restore
      consequence in `backup-restore-dr.md`).

## 7. Load synthetic data

- [ ] 7.1 Run the staging acceptance flows (registration, deposit via `simulate-callback`, casino play simulation, withdrawal
      four-eyes) so the database has ledger, audit and alert rows; record `SELECT count(*) FROM ledger_entries` through the backup role.
      (The local volume of >=100k entries is not required here; the point is real RDS behaviour.)

## 8. PITR restore to a timestamp (the item the local drill could not prove)

- [ ] 8.1 `aws rds describe-db-instances ... --query 'DBInstances[0].[BackupRetentionPeriod,LatestRestorableTime]'` shows 14 and a time
      within ~5 minutes of now (**record the delay: it is the measured managed RPO**).
- [ ] 8.2 Run `baseline.sql` as `igaming_backup` on the source -> `expected.txt`; record `T = SELECT now()` (UTC) immediately after. Write more
      synthetic rows after T (run section 7 again).
- [ ] 8.3 Wait until `LatestRestorableTime > T`. Restore: `aws rds restore-db-instance-to-point-in-time --source-db-instance-identifier igaming-staging-db
      --target-db-instance-identifier igaming-staging-db-pitr-<short-sha> --restore-time <T> --no-publicly-accessible
      --db-subnet-group-name <from terraform state> --vpc-security-group-ids <rds sg> --db-parameter-group-name igaming-staging-pg16 --no-multi-az`
      (**unverified details: which settings are inherited, whether the managed master secret follows, KMS key use**; record each).
      Time it: `aws rds wait db-instance-available` -> **measured managed PITR recovery time for the real engine** (the local number is an
      estimate and says nothing about this).
- [ ] 8.4 Reset the backup-role credential on the restored instance if needed, then `deploy/backup/verify.sh --seq-mode ge --baseline expected.txt`
      against it (target = the PITR instance, `BACKUP_ALLOW_REMOTE_TARGET=<its endpoint>` acknowledged per invocation). Expect PASS and
      rows written after T **absent**.
- [ ] 8.5 `aws rds delete-db-instance --db-instance-identifier igaming-staging-db-pitr-<short-sha> --skip-final-snapshot --delete-automated-backups`.

## 9. Logical backup and restore on RDS

- [ ] 9.1 `deploy/backup/backup.sh` from the runner (inside the VPC; **gap G1: runner placement/schedule undecided**) with
      `BACKUP_ENCRYPT_AGE_RECIPIENT` (public) and `BACKUP_SIGNING_KEY_FILE` set; check `manifest.json` (`"encryption": "age"`, `signed`),
      `SHA256SUMS` + `SHA256SUMS.sig`, and that no plaintext artefact exists in the output; time it. **Production dumps are taken from a
      PITR/snapshot clone, not from the primary** (ADR 0122 section 5c); in staging the instance is dumped directly (small data).
- [ ] 9.2 Restore in the **two-phase** form, never as the master user for the data load: `RESTORE_VERIFY_PUBKEY_FILE`/`RESTORE_VERIFY_PUBKEY_SHA256`
      pinned, `RESTORE_EXPECT_SOURCE_ENVIRONMENT=<the source anchor>`; `restore.sh --phase provision` as the master (`igaming`, a member of
      `rds_superuser`: creates only the empty database; roles already exist from step 3/5), set the owner's credential, then `restore.sh --phase load`
      as a **non-`rds_superuser`** owner login (**unverified on RDS: the master `igaming` IS an `rds_superuser` member, so a separate
      NOSUPERUSER owner login that is a member of `igaming`, or `igaming` itself without `rds_superuser`, has to exist; the script refuses a
      privileged data-load role, so if RDS cannot provide one this step is BLOCKED and the finding goes back to the architect**).
      Then `verify.sh` PASS, then `DROP DATABASE igaming_restore_check`. **Unverified: `pg_restore` extension creation (`pgcrypto`) and
      `ALTER SCHEMA public OWNER` as a non-superuser owner on RDS.** After the restore the `database_environment` anchor is BLANK by
      design; do not record `production` on a staging copy.

## 10. Observability

- [ ] 10.1 Log groups `/aws/rds/instance/igaming-staging-db/postgresql` and `.../upgrade` exist with the configured retention and receive lines.
- [ ] 10.2 Connection lines for `igaming_backup` (authorized / disconnection) are visible there within minutes of section 9.
- [ ] 10.3 Existing alarms (`modules/observability`) are in `OK`; note that **no alarm exists for backup failure, stale
      `LatestRestorableTime`, or low free storage during a restore (gap G2)**; add or record.

## 11. Alert delivery

- [ ] 11.1 With `alarm_email` set: force an alarm state (`aws cloudwatch set-alarm-state --alarm-name <name> --state-value ALARM --state-reason "A12 test"`)
      and confirm the e-mail arrives and the state returns to `OK`. (ALERT-DELIVERY-1 remains open for the platform's own alert routes.)

## 12. Evidence record

Fill in and commit to `docs/plans/` (no secrets, no endpoints beyond the staging names):

| Item | Result | Measured | By / when |
|---|---|---|---|
| named commit | | | |
| 0.2 static checks | | | |
| 2.2 pgaudit | | | |
| 5.1 BYPASSRLS role on RDS | | | |
| 8.1 latest restorable delay (RPO) | | | |
| 8.3 PITR recovery time | | | |
| 8.4 verify against PITR instance | | | |
| 9.2 logical restore on RDS | | | |
| 10.x observability | | | |
| 11.1 alert delivery | | | |

## 13. Stop conditions

Stop and report (do not work around) if: a step needs a credential beyond the staging deployer/bootstrap/master roles; RDS
refuses a role attribute or extension; verification fails; anything touches production; a real customer datum appears.

## 14. Teardown with the new protections

1. Apply with protection lifted deliberately: `deploy.sh up -var db_deletion_protection=false` (interactive), or edit the tfvars.
2. Choose a unique final snapshot name if one from an earlier deploy still exists: `-var db_final_snapshot_identifier=igaming-staging-db-final-<short-sha>`.
3. `deploy/aws/scripts/deploy.sh down`; the destroy takes the final snapshot.
4. `verify-teardown.sh` will list the final snapshot and retained automated backups; **delete them once the verification has been
   recorded** (`aws rds delete-db-snapshot ...`, `aws rds delete-db-instance-automated-backup ...`), then re-run it to a clean result.
