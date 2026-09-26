-- Runs once against a freshly initialized Postgres cluster, as the
-- bootstrap superuser the official postgres image creates from
-- POSTGRES_USER. That bootstrap role is ALWAYS a superuser (initdb
-- requires it) and superusers ALWAYS bypass row-level security, even on
-- a table with FORCE ROW LEVEL SECURITY - so it must never be the role
-- platform-api or its tests actually connect as. This script creates a
-- second, ordinary role for that purpose and hands it ownership of the
-- application database.
--
-- See docs/decisions/0002-multi-tenancy-isolation-strategy.md and the
-- Stage 1 specialist review that caught the original config connecting
-- as the superuser directly (docs/active-stage.md).
--
-- PLAT-ROLESPLIT-1 (docs/security/runtime-role-separation.md): this file
-- also provisions a second, non-owning role, "igaming_runtime". Table
-- ownership grants ALTER/DROP/TRUNCATE/DISABLE ROW LEVEL SECURITY
-- unconditionally, independent of any RLS policy - RLS never applies to
-- a table's owner. "igaming" therefore remains ONLY the migration-owner
-- role (used by cmd/migrate at deploy time); the running application
-- should connect as "igaming_runtime" instead in any environment where
-- this split has actually been rolled out. In this dev/CI bootstrap both
-- roles are created side by side, but DATABASE_URL/TEST_DATABASE_URL
-- still point at "igaming" - a large share of this repo's own
-- integration suite (every internal/*/migration_*_test.go file) calls
-- Pool.MigrateUp/MigrateDown directly and needs owner (DDL) privileges
-- to do that at all. See internal/db/runtime_role_separation_test.go for
-- the permanent regression test that exercises "igaming_runtime"
-- specifically (opt-in via TEST_RUNTIME_DATABASE_URL).
--
-- The password below ("igaming_runtime_dev_password") is a placeholder
-- exactly like "igaming"'s own "igaming_dev_password" above: this exact
-- password must never be reused anywhere near production. Production
-- provisioning follows docs/security/runtime-role-separation.md §6, run
-- once by an operator holding CREATEROLE, with a freshly generated
-- secret from a real secrets manager - never a value copied from this
-- file.
--
-- Everything from "CREATE ROLE igaming_runtime" down is written to be
-- idempotent (a DO-block-guarded CREATE ROLE, and GRANT/ALTER DEFAULT
-- PRIVILEGES statements, which are safe to re-run by nature) so this
-- same file can be used unmodified both for a fresh
-- docker-entrypoint-initdb.d run (no tables exist yet - see the ALTER
-- DEFAULT PRIVILEGES statements below) AND to backfill "igaming_runtime"
-- onto an already-bootstrapped, already-migrated cluster, such as this
-- repository's native-Postgres sandbox/CI environments (see the
-- Makefile's dev-db-init-roles target).

CREATE ROLE igaming
    LOGIN
    PASSWORD 'igaming_dev_password'
    NOSUPERUSER
    NOCREATEDB
    NOCREATEROLE
    NOBYPASSRLS;

ALTER DATABASE igaming_platform_dev OWNER TO igaming;
ALTER SCHEMA public OWNER TO igaming;

-- --- PLAT-ROLESPLIT-1: non-owning runtime application role ---

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        CREATE ROLE igaming_runtime
            LOGIN
            PASSWORD 'igaming_runtime_dev_password'
            NOSUPERUSER
            NOCREATEDB
            NOCREATEROLE
            NOBYPASSRLS;
    END IF;
END
$$;

GRANT CONNECT ON DATABASE igaming_platform_dev TO igaming_runtime;
GRANT USAGE ON SCHEMA public TO igaming_runtime;

-- Direct grants cover any table/sequence that already exists (relevant
-- when this file is re-run against an already-migrated database, e.g.
-- this repo's native sandbox dev environment). The ALTER DEFAULT
-- PRIVILEGES statements below cover every table/sequence a FUTURE
-- migration creates - the only path that matters on a genuinely fresh
-- docker-entrypoint-initdb.d run, since no tables exist yet at that
-- point (this script runs before cmd/migrate ever does).
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO igaming_runtime;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO igaming_runtime;

ALTER DEFAULT PRIVILEGES FOR ROLE igaming IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO igaming_runtime;
ALTER DEFAULT PRIVILEGES FOR ROLE igaming IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO igaming_runtime;

-- Narrow write access on the migration ledger back to read-only: the
-- blanket grant above also covers schema_migrations, which only
-- cmd/migrate (running as "igaming") should ever write to. Guarded
-- because schema_migrations does not exist yet on a fresh bootstrap (no
-- migration has run) - a harmless no-op there, and effective the moment
-- this same file is re-run after migrations have created the table.
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

-- ADR 0088 §3.5 (Stage 10 W1, sportsbook settlement): the deny triggers on
-- sportsbook_bet_settlements (migration 0091) are the BINDING control -
-- they bind the table owner too, so this REVOKE is defence in depth only,
-- not the mechanism that actually prevents mutation. It exists because the
-- blanket backfill GRANT above (:85, "ALL TABLES IN SCHEMA public") would
-- otherwise silently re-grant UPDATE/DELETE/TRUNCATE on this table to
-- igaming_runtime every time this idempotent script is re-run against an
-- already-migrated database - migration 0091 itself only runs once, so it
-- cannot re-assert this narrowing on every subsequent run of this file the
-- way this block can. Guarded exactly like the schema_migrations
-- narrowing above because the table does not exist yet on a fresh
-- docker-entrypoint-initdb.d run (migrations run after this script).
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'sportsbook_bet_settlements'
    ) THEN
        EXECUTE 'REVOKE UPDATE, DELETE, TRUNCATE ON sportsbook_bet_settlements FROM igaming_runtime';
    END IF;
END
$$;

-- Stage 10.3 W2a (ADR 0093 §1; migration 0096, provider credentials): the
-- runtime role's least-privilege grants on the three provider-credential
-- tables, re-asserted on every run. The blanket backfill GRANT above
-- ("ALL TABLES IN SCHEMA public") would otherwise silently re-grant
-- table-level UPDATE/DELETE on them every time this idempotent script is
-- re-run against an already-migrated database; migration 0096 itself runs
-- only once. The statements are EXACTLY migration 0096's own block: REVOKE
-- ALL (which also drops any column privileges), then SELECT/INSERT, then
-- UPDATE on the listed columns only - never DELETE or TRUNCATE, and never
-- activation_request_id. The DB triggers remain the binding control; this
-- is defence in depth. Guarded per table because the tables do not exist
-- yet on a fresh docker-entrypoint-initdb.d run (migrations run after this
-- script). Re-running leaves the privileges unchanged
-- (TestInitAppRole_RerunKeepsProviderCredentialGrants).
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'provider_credential_handles'
    ) THEN
        EXECUTE 'REVOKE ALL ON provider_credential_handles FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON provider_credential_handles TO igaming_runtime';
        EXECUTE 'GRANT UPDATE (status, status_changed_at, not_after, revoked_at, revoked_by, revoke_reason) '
             || 'ON provider_credential_handles TO igaming_runtime';
    END IF;
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'provider_credential_change_requests'
    ) THEN
        EXECUTE 'REVOKE ALL ON provider_credential_change_requests FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON provider_credential_change_requests TO igaming_runtime';
        EXECUTE 'GRANT UPDATE (state, applied_at, applied_by_principal_id, applied_handle_id) '
             || 'ON provider_credential_change_requests TO igaming_runtime';
    END IF;
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'provider_credential_change_approvals'
    ) THEN
        EXECUTE 'REVOKE ALL ON provider_credential_change_approvals FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON provider_credential_change_approvals TO igaming_runtime';
    END IF;
END
$$;

-- Stage 10.3 W2b (migration 0097, casino_callback_rejections): the same
-- treatment as the provider-credential tables above, re-asserted on every
-- run. The blanket backfill GRANT above ("ALL TABLES IN SCHEMA public")
-- would otherwise silently re-grant table-level UPDATE/DELETE on this
-- append-only table every time this idempotent script is re-run against
-- an already-migrated database; migration 0097 itself runs only once. The
-- statements are EXACTLY migration 0097's own grant: REVOKE ALL, then
-- SELECT/INSERT only - never UPDATE, DELETE or TRUNCATE (there is no
-- mutable column at all; the table is pure append-only history). The
-- deny triggers created by migration 0097 remain the binding control;
-- this is defence in depth. Guarded because the table does not exist yet
-- on a fresh docker-entrypoint-initdb.d run (migrations run after this
-- script).
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'casino_callback_rejections'
    ) THEN
        EXECUTE 'REVOKE ALL ON casino_callback_rejections FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON casino_callback_rejections TO igaming_runtime';
    END IF;
END
$$;
