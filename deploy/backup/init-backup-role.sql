-- Dedicated, audited logical-backup role (ADR 0122, BACKUP-ROLE-1 decision D-BACKUP).
--
-- Run ONCE per database by an operator who holds CREATEROLE (and, on stock PostgreSQL, SUPERUSER because
-- BYPASSRLS can only be granted by a superuser; on RDS the master/rds_superuser role - NOT VERIFIED here, see
-- the ADR). This file is applied by a human or by the drill to a THROWAWAY cluster; nothing in this repository
-- applies it to a shared or production database on its own.
--
-- WHY THIS ROLE EXISTS. Every tenant table is FORCE ROW LEVEL SECURITY. A plain owner (`igaming`) or the runtime
-- role (`igaming_runtime`) sees only the rows its tenant GUC admits, so `pg_dump` either FAILS ("query would be
-- affected by row-level security policy") or, with --enable-row-security, silently produces an INCOMPLETE dump.
-- Weakening RLS to dump (NO FORCE, session_replication_role, row_security=off) is forbidden. The remaining
-- supported answer is a role that is *exempt by attribute*: BYPASSRLS, and nothing else.
--
-- WHAT IT IS ALLOWED TO DO: log in (from the backup runner only; see pg_hba / security group guidance in the ADR),
-- connect to the one database, USAGE on the public schema, SELECT on every table and sequence (and on future ones
-- created by the migration owner). Read-only is enforced twice: no write privilege exists AND the role's sessions
-- default to read-only transactions.
--
-- WHAT IT IS NOT: not a superuser, no CREATEDB / CREATEROLE / REPLICATION, no membership in `igaming` or
-- `igaming_runtime` (so it inherits no write privilege), no CREATE on any schema, never used by the application,
-- not usable for restore (restore is a separate, human-run operation as a different role).
--
-- CREDENTIAL. The password is read from the environment variable IGAMING_BACKUP_PASSWORD with \getenv (never an
-- argv value, never in this file); the production source is a secret in the secret store referenced by ARN
-- (BACKUP_SECRET_ARN, runbook). The role is TIME-BOXED: VALID UNTIL is set from BACKUP_ROLE_VALID_UNTIL
-- (a timestamptz literal, default now() + 24 hours) and the job that mints a credential re-runs this file to
-- extend it. An expired credential makes the backup FAIL CLOSED (drilled).
--
-- IDEMPOTENT: safe to re-run (re-applies password, expiry, attributes and grants).

\getenv backup_password IGAMING_BACKUP_PASSWORD
\getenv backup_valid_until BACKUP_ROLE_VALID_UNTIL
\if :{?backup_valid_until}
\else
\set backup_valid_until ''
\endif
\if :{?backup_password}
\else
\set backup_password ''
\endif

\o /dev/null
SELECT set_config('igaming.backup_password', :'backup_password', false);
SELECT set_config('igaming.backup_valid_until',
                  CASE WHEN :'backup_valid_until' = '' THEN (now() + interval '24 hours')::text ELSE :'backup_valid_until' END,
                  false);
\o

DO $$
DECLARE
    pw    text := current_setting('igaming.backup_password');
    until text := current_setting('igaming.backup_valid_until');
BEGIN
    IF pw IS NULL OR length(pw) < 16 THEN
        RAISE EXCEPTION 'IGAMING_BACKUP_PASSWORD must be set (>= 16 characters)';
    END IF;
    BEGIN
        IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_backup') THEN
            EXECUTE format('CREATE ROLE igaming_backup LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION BYPASSRLS NOINHERIT CONNECTION LIMIT 2 PASSWORD %L VALID UNTIL %L', pw, until);
        ELSE
            EXECUTE format('ALTER ROLE igaming_backup WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION BYPASSRLS NOINHERIT CONNECTION LIMIT 2 PASSWORD %L VALID UNTIL %L', pw, until);
        END IF;
    EXCEPTION WHEN OTHERS THEN
        -- never echo the statement or SQLERRM: either can carry the password
        RAISE EXCEPTION 'igaming_backup role create/alter failed (SQLSTATE %)', SQLSTATE;
    END;
    PERFORM set_config('igaming.backup_password', '', false);
END
$$;

-- Defence in depth on the session itself: read-only transactions by default, bounded statements, no idle
-- transactions holding the dump snapshot open forever. (A dump is one long REPEATABLE READ transaction, so the
-- statement timeout stays generous.)
ALTER ROLE igaming_backup SET default_transaction_read_only = on;
ALTER ROLE igaming_backup SET statement_timeout = '4h';
ALTER ROLE igaming_backup SET idle_in_transaction_session_timeout = '30min';
ALTER ROLE igaming_backup SET lock_timeout = '30s';

-- Audit: statement logging for this role. `log_statement` is superuser-settable on stock PostgreSQL (this works on
-- the throwaway cluster). On RDS use pgaudit instead (shared_preload_libraries, `pgaudit.log` / role-based
-- auditing: deploy/aws modules/database parameter group) - the ADR records this as a deploy-time item.
-- Connections and disconnections are logged cluster-wide (log_connections / log_disconnections).
ALTER ROLE igaming_backup SET log_statement = 'all';

DO $$
BEGIN
    EXECUTE format('REVOKE ALL ON DATABASE %I FROM igaming_backup', current_database());
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO igaming_backup', current_database());
END
$$;

-- No privilege on schemas other than USAGE on public; explicitly no CREATE.
REVOKE ALL ON SCHEMA public FROM igaming_backup;
GRANT USAGE ON SCHEMA public TO igaming_backup;

-- Exactly SELECT, nothing else, on everything that exists ...
REVOKE ALL ON ALL TABLES IN SCHEMA public FROM igaming_backup;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO igaming_backup;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA public FROM igaming_backup;
GRANT SELECT ON ALL SEQUENCES IN SCHEMA public TO igaming_backup;

-- ... and on whatever the migration owner creates later. The migration owner is `igaming` by convention
-- (on RDS: the master user; this file runs connected as that role there, so current_user is used).
DO $$
BEGIN
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public GRANT SELECT ON TABLES TO igaming_backup',
                   (SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = current_database()));
    EXECUTE format('ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public GRANT SELECT ON SEQUENCES TO igaming_backup',
                   (SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = current_database()));
END
$$;

-- Never a member of the owner or runtime roles (membership would import write privileges).
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_auth_members m JOIN pg_roles r ON r.oid = m.roleid
               JOIN pg_roles u ON u.oid = m.member WHERE u.rolname = 'igaming_backup') THEN
        RAISE EXCEPTION 'igaming_backup must not be a member of any role';
    END IF;
END
$$;
