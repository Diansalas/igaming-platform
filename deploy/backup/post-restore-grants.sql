-- Database-level privileges that a pg_dump/pg_restore does NOT carry (the dump has no CREATE DATABASE / database ACL;
-- finding of OPS-READINESS-LOCAL: after a raw restore PUBLIC holds TEMPORARY again, which migration 0116 / ADR 0108
-- removed to close the pg_temp shadowing hole). Mirrors the DATABASE-level statements of deploy/init-app-role.sql
-- (and deploy/aws/sql/init-runtime-role.rds.sql) and adds the backup role's CONNECT. It is intentionally small:
-- production must ALSO re-run the environment's complete role-provisioning script (it re-asserts every object-level
-- grant); object-level grants are restored from the dump itself and verified by verify.sh against the backup baseline.
--
-- Run by restore.sh against the NEW database as the restore role acting as the database owner. Idempotent. It creates
-- no role, sets no password and changes no role attribute.
\set ON_ERROR_STOP on

-- A new database from template0 owns schema public as pg_database_owner; the platform's role model has the migration
-- owner own it (deploy/init-app-role.sql: ALTER SCHEMA public OWNER TO igaming) and a plain restore does not carry
-- schema ownership (pg_restore --no-owner). Found by verify.sh (SCHEMAACL differed) on the first drill run.
DO $$
BEGIN
    EXECUTE format('ALTER SCHEMA public OWNER TO %I', (SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = current_database()));
END
$$;

DO $$
BEGIN
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO igaming_runtime', current_database());
    EXECUTE format('REVOKE TEMPORARY ON DATABASE %I FROM PUBLIC', current_database());
    EXECUTE format('REVOKE TEMPORARY ON DATABASE %I FROM igaming_runtime', current_database());
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_backup') THEN
        EXECUTE format('REVOKE ALL ON DATABASE %I FROM igaming_backup', current_database());
        EXECUTE format('GRANT CONNECT ON DATABASE %I TO igaming_backup', current_database());
    END IF;
END
$$;
