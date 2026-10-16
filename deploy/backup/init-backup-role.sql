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
-- default to read-only transactions (an accident guard: a holder of the credential can reset session defaults).
--
-- WHAT IT IS NOT: not a superuser, no CREATEDB / CREATEROLE / REPLICATION, no membership in `igaming` or
-- `igaming_runtime` (so it inherits no write privilege), no CREATE on any schema, never used by the application,
-- not usable for restore (restore is a separate, human-run operation as a different role).
--
-- CREDENTIAL (A12 security review M2/L1). The password never appears in this file or in any statement text. The caller supplies
-- a SCRAM-SHA-256 VERIFIER computed on the client (deploy/backup/scramhash, from the password read on stdin) in the environment
-- variable IGAMING_BACKUP_PASSWORD_VERIFIER; the server stores it as given, so statement logging (log_statement,
-- log_min_duration_statement) and pgaudit see only the verifier, never the password. The role is TIME-BOXED: VALID UNTIL comes
-- from BACKUP_ROLE_VALID_UNTIL (a timestamptz literal, default now() + 24 hours) and is REJECTED if it is not within the next
-- 48 hours ('infinity' and far-future dates are refused). An expired credential makes the backup FAIL CLOSED (drilled).
-- On RDS prefer IAM database authentication (GRANT rds_iam, no password at all; ADR 0122 section 3).
--
-- IDEMPOTENT: safe to re-run (re-applies credential, expiry, attributes and grants).
\set ON_ERROR_STOP on

\getenv backup_verifier IGAMING_BACKUP_PASSWORD_VERIFIER
\getenv backup_valid_until BACKUP_ROLE_VALID_UNTIL
\getenv backup_iam_only BACKUP_ROLE_IAM_ONLY
\if :{?backup_iam_only}
\else
\set backup_iam_only ''
\endif
\if :{?backup_valid_until}
\else
\set backup_valid_until ''
\endif
\if :{?backup_verifier}
\else
\set backup_verifier ''
\endif

\o /dev/null
SELECT set_config('igaming.backup_verifier', :'backup_verifier', false);
SELECT set_config('igaming.iam_only', :'backup_iam_only', false);
SELECT set_config('igaming.backup_valid_until',
                  CASE WHEN :'backup_valid_until' = '' THEN (now() + interval '24 hours')::text ELSE :'backup_valid_until' END,
                  false);
\o

DO $$
DECLARE
    v     text := current_setting('igaming.backup_verifier');
    iam   boolean := current_setting('igaming.iam_only') = '1';
    until timestamptz;
BEGIN
    -- IAM-ONLY PATH (BACKUP_ROLE_IAM_ONLY=1; staging/production on RDS, ADR 0122 section 3): the role has NO password at all (a NULL password cannot be used to
    -- log in), validity is not time-boxed in the database (the 15-minute IAM token is the time box, scoped by rds-db:connect), and rds_iam is granted when it exists.
    IF iam THEN
        BEGIN
            IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_backup') THEN
                EXECUTE 'CREATE ROLE igaming_backup LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION BYPASSRLS NOINHERIT CONNECTION LIMIT 2 PASSWORD NULL';
            ELSE
                EXECUTE format('ALTER ROLE igaming_backup WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION BYPASSRLS NOINHERIT CONNECTION LIMIT 2 PASSWORD NULL VALID UNTIL %L', 'infinity');
            END IF;
            IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'rds_iam') THEN
                EXECUTE format('%s %I TO %I', 'GRANT', 'rds_iam', 'igaming_backup');
            END IF;
        EXCEPTION WHEN OTHERS THEN
            RAISE EXCEPTION 'igaming_backup IAM-only role create/alter failed (SQLSTATE %)', SQLSTATE;
        END;
        RETURN;
    END IF;
    IF v IS NULL OR v !~ '^SCRAM-SHA-256\$[0-9]+:[A-Za-z0-9+/=]+\$[A-Za-z0-9+/=]+:[A-Za-z0-9+/=]+$' THEN
        RAISE EXCEPTION 'IGAMING_BACKUP_PASSWORD_VERIFIER must be a SCRAM-SHA-256 verifier (compute it with deploy/backup/scramhash; the clear-text password is never accepted)';
    END IF;
    BEGIN
        until := current_setting('igaming.backup_valid_until')::timestamptz;
    EXCEPTION WHEN OTHERS THEN
        RAISE EXCEPTION 'BACKUP_ROLE_VALID_UNTIL is not a timestamp';
    END;
    IF until IS NULL OR until > now() + interval '48 hours' THEN
        RAISE EXCEPTION 'BACKUP_ROLE_VALID_UNTIL must be within the next 48 hours (time-boxed credential)';
    END IF;
    BEGIN
        IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_backup') THEN
            EXECUTE format('CREATE ROLE igaming_backup LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION BYPASSRLS NOINHERIT CONNECTION LIMIT 2 PASSWORD %L VALID UNTIL %L', v, until);
        ELSE
            EXECUTE format('ALTER ROLE igaming_backup WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION BYPASSRLS NOINHERIT CONNECTION LIMIT 2 PASSWORD %L VALID UNTIL %L', v, until);
        END IF;
    EXCEPTION WHEN OTHERS THEN
        -- never echo the statement or SQLERRM
        RAISE EXCEPTION 'igaming_backup role create/alter failed (SQLSTATE %)', SQLSTATE;
    END;
    PERFORM set_config('igaming.backup_verifier', '', false);
END
$$;

-- Defence in depth on the session itself: read-only transactions by default, bounded statements, no idle
-- transactions holding a snapshot forever. These are ACCIDENT GUARDS, not controls: the holder of the credential can
-- SET/ALTER ROLE them back (documented residual, ADR 0122 section 9); backup.sh re-checks the read-only default at start and
-- keeps its snapshot-holder session alive with a keepalive query instead of relying on a long idle timeout.
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

-- NOTE (owner decision 5, ADR 0122 section 10; F1, migration 0166): "everything" is everything in schema PUBLIC. The actor-proof signing secrets
-- are NOT there any more: migration 0166 moved actor_proof_keys into the owner-only schema actorproof_private, on which this role holds NO
-- privilege (no USAGE, no table grant). backup.sh dumps with --exclude-schema=actorproof_private (pg_dump then neither locks nor reads it) and
-- refuses to run if this role can reach that schema. The denial is re-asserted on every run (below); the only actor-proof object this role may
-- use is the SECURITY DEFINER inventory public.actor_proof_key_inventory(), which returns the NON-secret columns (kid, status, created_at,
-- retired_at) for the baseline. (Before 0166 the role had table-level SELECT on public.actor_proof_keys - residual R1 of
-- docs/plans/backup-exclude-actor-secret-record.md - because pg_dump's LOCK TABLE needs it; a column-level grant broke the dump.)
DO $$
BEGIN
    IF to_regnamespace('actorproof_private') IS NOT NULL THEN
        REVOKE ALL ON SCHEMA actorproof_private FROM igaming_backup;
        REVOKE ALL ON ALL TABLES IN SCHEMA actorproof_private FROM igaming_backup;
        REVOKE ALL ON ALL SEQUENCES IN SCHEMA actorproof_private FROM igaming_backup;
        REVOKE ALL ON ALL FUNCTIONS IN SCHEMA actorproof_private FROM igaming_backup;
    END IF;
    IF to_regprocedure('public.actor_proof_key_inventory()') IS NOT NULL THEN
        GRANT EXECUTE ON FUNCTION public.actor_proof_key_inventory() TO igaming_backup;
    END IF;
END
$$;
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
               JOIN pg_roles u ON u.oid = m.member WHERE u.rolname = 'igaming_backup' AND r.rolname <> 'rds_iam') THEN
        RAISE EXCEPTION 'igaming_backup must not be a member of any role';
    END IF;
END
$$;
