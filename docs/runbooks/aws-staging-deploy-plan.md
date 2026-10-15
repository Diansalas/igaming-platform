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
| backup-runner credential | **reference only**: Secrets Manager ARN, nothing created, no value | `backup_runner_secret_arn` |
| deployer IAM (prepared, needs security review and the bootstrap credential to apply) | `rds:CreateDBSnapshot` on `snapshot:igaming-staging-*`; `logs:*` group actions on `/aws/rds/instance/igaming-staging-*` | `deploy/aws/iam/staging-deployer-infra-policy.json` |

Consequences to accept knowingly: `deploy.sh down` is no longer one step (section 14); the final snapshot and the retained
automated backups are deliberately left behind and `verify-teardown.sh` will list them; a destroy refuses if the snapshot name
already exists.

## 0. Before anything is applied

- [ ] 0.1 Record the **named commit** (full SHA) and the authorisation (who, when) in the evidence record at the end of this file.
- [ ] 0.2 `terraform fmt -recursive -check deploy/aws` and `deploy/aws/tests/run-static-checks.sh` (fmt, validate, `terraform test`
      with mock providers, no credentials). Expect green. **First-ever run for this change: fix what it finds, re-run, commit.**
- [ ] 0.3 `go test ./deploy/backup/ ./deploy/drill/` and the local drill `deploy/backup/drill.sh` green on the same commit.
- [ ] 0.4 Security review of ADR 0122 (probes in its section 9) recorded as accepted, and of the **IAM policy delta**; then the
      bootstrap credential applies the policy change (`deploy/aws/iam/`); `python3 deploy/aws/tests/simulate-deployer-policies.py`
      (read-only IAM simulation) shows every expectation held, including the five new A12 cases.
- [ ] 0.5 Decide `db_kms_key_arn` (null = AWS-managed key is acceptable for staging; a CMK needs a key policy that lets RDS and the
      restore principal use it: not designed here).

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
- [ ] 4.2 **Backup-role secret, out of band** (Terraform never holds it): create a Secrets Manager secret
      `igaming-staging/db-backup` with JSON `{"username":"igaming_backup","password":"<generated by the secret store>"}` using a
      generator, not a typed value; pass its ARN as `-var backup_runner_secret_arn=<arn>` on the next apply (reference only).
      The secret's resource policy allows only the backup runner identity.
- [ ] 4.3 Nothing secret in logs: CloudWatch `/ecs/igaming-staging/role-init` contains no password (the script never echoes it).

## 5. Database permissions (the backup role)

- [ ] 5.1 Provision `igaming_backup` with `deploy/backup/init-backup-role.sql` as the master user, from a one-off task or a
      bastion **inside the VPC** (**gap G1: no ECS task definition for this exists yet; building it is part of this deploy's
      scope or the step is done from an allow-listed operator session, recorded**). `IGAMING_BACKUP_PASSWORD` comes from the secret
      of 4.2, `BACKUP_ROLE_VALID_UNTIL` = now + 24 h.
      **Unverified on RDS: whether the master user may `CREATE ROLE ... BYPASSRLS`.** If RDS refuses, STOP: ADR 0122 section 3 then
      has no logical-backup role on RDS and logical dumps are not available there (PITR remains the primary path).
- [ ] 5.2 Attributes: `SELECT rolname,rolsuper,rolbypassrls,rolcreatedb,rolcreaterole,rolreplication,rolconnlimit,rolvaliduntil FROM pg_roles WHERE rolname='igaming_backup';` = f,t,f,f,f,2,<future>.
- [ ] 5.3 Probes (all must be denied): as `igaming_backup` run the 15 statements of `drill.sh` phase 2 (`INSERT`, `UPDATE`, `DELETE`,
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
      `BACKUP_ENCRYPT_*` set; check `manifest.json`, `SHA256SUMS`; time it.
- [ ] 9.2 `deploy/backup/restore.sh --new-database igaming_restore_check` as the master user (`igaming`) then `verify.sh` PASS, then
      `DROP DATABASE igaming_restore_check`. **Unverified: `pg_restore` extension creation (`pgcrypto`) and `ALTER SCHEMA public OWNER`
      as the RDS master.**

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
