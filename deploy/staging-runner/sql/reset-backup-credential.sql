-- staging-runner mode pitr-verify (plan 7.4: "reset the backup-role credential on the restored instance if needed").
-- Run as the master on a DISPOSABLE PITR clone only. The clone is restored from the source's data, so igaming_backup exists there as an
-- IAM-only login (rds_iam, no password) and the runner's task role is not allowed to mint tokens for the clone's DbiResourceId (its
-- rds-db:connect statement names the primary only). The clone's copy of the role is therefore switched to a short-lived password credential:
-- rds_iam is revoked (a rds_iam member cannot use a password on RDS) and a client-computed SCRAM verifier (RN_BACKUP_VERIFIER, never a
-- clear password) with a 2-hour validity is set. The primary is never touched by this script.
\set ON_ERROR_STOP on
\getenv backup_verifier RN_BACKUP_VERIFIER

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'rds_iam') THEN
        EXECUTE 'REVOKE rds_iam FROM igaming_backup';
    END IF;
END
$$;
ALTER ROLE igaming_backup WITH LOGIN PASSWORD :'backup_verifier' VALID UNTIL :'valid_until';
