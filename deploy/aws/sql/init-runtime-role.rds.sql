-- RDS-specific adaptation of deploy/init-app-role.sql
-- (docs/security/runtime-role-separation.md, PLAT-ROLESPLIT-1).
--
-- Run as the RDS MASTER user ("igaming" by convention — see
-- deploy/aws/modules/database, which names the master_username "igaming"
-- deliberately: an RDS master user already owns the database/schema it
-- creates and holds CREATEROLE via the rds_superuser pseudo-role, even
-- though it is NOT a true Postgres SUPERUSER (AWS deliberately withholds
-- that). This means, unlike deploy/init-app-role.sql's dev/CI bootstrap
-- (which runs as an initdb superuser distinct from "igaming" and must
-- explicitly ALTER DATABASE/SCHEMA OWNER TO igaming), this script does
-- NOT need to hand off ownership — the connecting role already owns
-- everything. It only needs to create the second, non-owning
-- "igaming_runtime" role and grant it exactly the privileges in
-- docs/security/runtime-role-separation.md §3.
--
-- PASSWORD HANDLING: the igaming_runtime password is read from the
-- IGAMING_RUNTIME_PASSWORD environment variable via psql's `\getenv`
-- meta-command — never passed as a `-v` CLI argument (which can leak via
-- process listings) and never hardcoded in this file. The ECS one-off
-- "role-init" task definition injects IGAMING_RUNTIME_PASSWORD from the
-- Terraform-generated Secrets Manager secret at task run time (see
-- deploy/aws/modules/ecs and the runbook).
--
-- IDEMPOTENT by design: safe to re-run (e.g. after a credential rotation)
-- against an already-initialized database. Creation is guarded by
-- existence checks; the password is (re-)applied every run via
-- ALTER ROLE, so re-running this script after rotating
-- IGAMING_RUNTIME_PASSWORD in Secrets Manager is the supported way to
-- propagate that rotation into Postgres itself (platform-api's own
-- running tasks still need a forced new deployment afterward to pick up
-- the new PGPASSWORD value — `deploy.sh up`/`migrate` do both; see the
-- Stage 9.4 lifecycle runbook §10).
--
-- ORDERING (do not run this file's REVOKE block as the *only* narrowing
-- step — read this before assuming this file alone is sufficient):
-- this script runs BEFORE `cmd/migrate up` in the deployment sequence
-- (role/grants must exist before migrations create tables the runtime
-- role needs DML on). At that point `schema_migrations` does not exist
-- yet, so the guarded REVOKE block below is a no-op on first run. The
-- AUTHORITATIVE narrowing of igaming_runtime's access to
-- schema_migrations happens as a SEPARATE, LATER step chained onto the
-- end of the migration task's own command (`migrate up && psql ... -c
-- "REVOKE ..."`), run AFTER migrations have created that table — see
-- deploy/aws/modules/ecs's migrate task definition and the runbook's
-- "Run database migrations" step. This file's own guarded REVOKE below
-- exists only as a defensive backstop for re-runs of THIS script after
-- migrations already exist (e.g. a later credential rotation), not as
-- the mechanism that closes the gap the first time.

\getenv runtime_password IGAMING_RUNTIME_PASSWORD

-- psql's `:'var'` textual substitution does NOT reach inside a
-- dollar-quoted ($$ ... $$) DO-block body — psql treats dollar-quoting
-- as opaque, exactly like it does single-quoted strings, so referencing
-- `:'runtime_password'` directly inside the DO block below would send
-- the literal, unsubstituted text to the server. This top-level
-- `SELECT set_config(...)` statement is NOT inside dollar-quoting, so
-- the substitution happens correctly here; the DO block then reads the
-- value back server-side via `current_setting()` at execution time. The
-- password still never appears as a CLI argument and is never hardcoded
-- — it flows psql-variable -> session GUC -> plpgsql variable, entirely
-- within this one psql session.
--
-- NEVER ECHO THE PASSWORD (Stage 9.4 security review): psql prints the
-- result row of a SELECT, and set_config() returns the value it sets — so
-- this statement's output is redirected to /dev/null (\o), otherwise the
-- runtime password would land in the role-init task's stdout, i.e. in
-- CloudWatch Logs. Likewise, a failing CREATE/ALTER ROLE inside EXECUTE
-- would print the full dynamic statement (password included) in the error
-- CONTEXT; the DO block below catches any failure and re-raises with the
-- SQLSTATE only. The password is also cleared from the session GUC as soon
-- as it has been applied.
\o /dev/null
SELECT set_config('igaming.runtime_password', :'runtime_password', false);
\o

DO $$
DECLARE
    runtime_password text := current_setting('igaming.runtime_password');
BEGIN
    BEGIN
        IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
            EXECUTE format(
                'CREATE ROLE igaming_runtime LOGIN PASSWORD %L NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS',
                runtime_password
            );
        ELSE
            EXECUTE format('ALTER ROLE igaming_runtime WITH PASSWORD %L', runtime_password);
        END IF;
    EXCEPTION WHEN OTHERS THEN
        -- Deliberately NOT including SQLERRM or the statement: either can
        -- echo the password.
        RAISE EXCEPTION 'igaming_runtime role create/alter failed (SQLSTATE %)', SQLSTATE;
    END;
    PERFORM set_config('igaming.runtime_password', '', false);
END
$$;

DO $$
BEGIN
    EXECUTE format('GRANT CONNECT ON DATABASE %I TO igaming_runtime', current_database());
END
$$;

GRANT USAGE ON SCHEMA public TO igaming_runtime;

-- Direct grants cover any table/sequence that already exists (relevant on
-- a re-run after migrations have already created tables). The ALTER
-- DEFAULT PRIVILEGES statements below cover every table/sequence a FUTURE
-- migration creates.
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO igaming_runtime;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO igaming_runtime;

-- Uses current_user (dynamic) rather than a hardcoded "igaming" literal,
-- so this script keeps working unmodified if the operator ever renames
-- the master_username Terraform variable — this script always runs
-- connected AS the master/migration-owner role, whatever it's named.
DO $$
BEGIN
    EXECUTE format(
        'ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO igaming_runtime',
        current_user
    );
    EXECUTE format(
        'ALTER DEFAULT PRIVILEGES FOR ROLE %I IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO igaming_runtime',
        current_user
    );
END
$$;

-- Defensive backstop only — see the ORDERING note above. The
-- authoritative revoke runs later, chained onto the migration task.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'schema_migrations'
    ) THEN
        EXECUTE 'REVOKE INSERT, UPDATE, DELETE ON schema_migrations FROM igaming_runtime';
    END IF;
END
$$;
