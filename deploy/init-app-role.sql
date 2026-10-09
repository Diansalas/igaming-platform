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

-- PRH-2 R2 (TRIGGER-SEARCH-PATH-1, ADR 0108, migration 0116): the runtime role
-- must not be able to create TEMP objects. PostgreSQL grants TEMPORARY on every
-- database to PUBLIC by default and the runtime role inherits it from there, so
-- revoking from PUBLIC is what matters (a role-only revoke is a no-op). Done
-- here as well as in migration 0116 so a freshly provisioned database is safe
-- before any migration runs. The owner keeps TEMPORARY as the database owner.
-- No role, password or attribute is changed.
DO $$
BEGIN
    EXECUTE format('REVOKE TEMPORARY ON DATABASE %I FROM PUBLIC', current_database());
    EXECUTE format('REVOKE TEMPORARY ON DATABASE %I FROM igaming_runtime', current_database());
END
$$;
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

-- PRH-2 R5 (SIGNED-ACTOR-PROOF, ADR 0110, migration 0120): actor_proof_keys and
-- actor_proof_nonces are OWNER-ONLY. The blanket backfill GRANT above ("ALL
-- TABLES IN SCHEMA public") would re-grant the runtime role SELECT/INSERT/
-- UPDATE/DELETE on them every time this idempotent script is re-run against an
-- already-migrated database; migration 0120 itself runs only once. Re-assert the
-- denial here (both tables also have RLS enabled with NO policy, a second
-- independent denial), and re-assert EXECUTE on the two SECURITY DEFINER
-- functions the runtime role needs (a database provisioned after migrations
-- ran has not yet received it). Guarded: neither exists before migration 0120.
DO $$
BEGIN
    IF to_regclass('public.actor_proof_keys') IS NOT NULL THEN
        EXECUTE 'REVOKE ALL ON actor_proof_keys FROM igaming_runtime';
    END IF;
    IF to_regclass('public.actor_proof_nonces') IS NOT NULL THEN
        EXECUTE 'REVOKE ALL ON actor_proof_nonces FROM igaming_runtime';
    END IF;
    IF to_regprocedure('public.actor_proof_require(uuid, text, uuid, text, text, text)') IS NOT NULL THEN
        EXECUTE 'GRANT EXECUTE ON FUNCTION actor_proof_require(uuid, text, uuid, text, text, text) TO igaming_runtime';
    END IF;
    IF to_regprocedure('public.actor_proof_key_active(text)') IS NOT NULL THEN
        EXECUTE 'GRANT EXECUTE ON FUNCTION actor_proof_key_active(text) TO igaming_runtime';
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

-- PRH-I5 (migration 0102, payment_statement_imports/payment_statement_lines;
-- ADR 0095 §12/§13.3): the same treatment as casino_callback_rejections
-- above, re-asserted on every run. The blanket backfill GRANT above ("ALL
-- TABLES IN SCHEMA public") would otherwise silently re-grant table-level
-- UPDATE/DELETE on these append-only statement tables every time this
-- idempotent script is re-run against an already-migrated database;
-- migration 0102 itself runs only once. The statements are EXACTLY
-- migration 0102's own grant: REVOKE ALL, then SELECT/INSERT only - never
-- UPDATE, DELETE or TRUNCATE. The deny triggers created by migration 0102
-- remain the binding control; this is defence in depth. Guarded per table
-- because the tables do not exist yet on a fresh docker-entrypoint-initdb.d
-- run (migrations run after this script).
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'payment_statement_imports'
    ) THEN
        EXECUTE 'REVOKE ALL ON payment_statement_imports FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON payment_statement_imports TO igaming_runtime';
    END IF;
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'payment_statement_lines'
    ) THEN
        EXECUTE 'REVOKE ALL ON payment_statement_lines FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON payment_statement_lines TO igaming_runtime';
    END IF;
END
$$;

-- PRH-I1 (migration 0105, payment_kill_switches/
-- payment_kill_switch_release_requests; ADR 0095 §10.2): re-asserted on
-- every run for the same reason as the block above. The application needs
-- SELECT/INSERT/UPDATE (engage, request, approve, cancel) but never
-- DELETE or TRUNCATE - both tables reject those at the trigger layer
-- (payment_kill_switches_no_delete / payment_kill_switch_release_requests_
-- no_delete); this GRANT is defence in depth, not the binding control.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'payment_kill_switches'
    ) THEN
        EXECUTE 'REVOKE ALL ON payment_kill_switches FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT, UPDATE ON payment_kill_switches TO igaming_runtime';
    END IF;
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'payment_kill_switch_release_requests'
    ) THEN
        EXECUTE 'REVOKE ALL ON payment_kill_switch_release_requests FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT, UPDATE ON payment_kill_switch_release_requests TO igaming_runtime';
    END IF;
END
$$;

-- CAS-PLAY-BOOTSTRAP-1 (migration 0111, casino_launch_bootstraps/
-- casino_provider_player_refs; ADR 0103 §5): the same treatment as
-- casino_callback_rejections above, re-asserted on every run. The blanket
-- backfill GRANT above ("ALL TABLES IN SCHEMA public") would otherwise
-- silently re-grant table-level UPDATE/DELETE on these append-only tables
-- every time this idempotent script is re-run against an already-migrated
-- database; migration 0111 itself runs only once. The statements are
-- EXACTLY migration 0111's own guarded grant: REVOKE ALL, then
-- SELECT/INSERT only - never UPDATE, DELETE or TRUNCATE. The deny
-- triggers created by migration 0111
-- (casino_launch_bootstraps_immutable/_no_truncate,
-- casino_provider_player_refs_immutable/_no_truncate) remain the binding
-- control; this is defence in depth (architect review F-5 /
-- security review B-C1, docs/plans/prh2-hardening-round/reviews/). Guarded
-- per table because the tables do not exist yet on a fresh
-- docker-entrypoint-initdb.d run (migrations run after this script).
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'casino_launch_bootstraps'
    ) THEN
        EXECUTE 'REVOKE ALL ON casino_launch_bootstraps FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON casino_launch_bootstraps TO igaming_runtime';
    END IF;
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'casino_provider_player_refs'
    ) THEN
        EXECUTE 'REVOKE ALL ON casino_provider_player_refs FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON casino_provider_player_refs TO igaming_runtime';
    END IF;
END
$$;

-- PRH-2 I-core (migration 0110; ADR 0102, ALERT-DELIVERY-1):
-- least-privilege, re-asserted on every run for the same reason as the
-- blocks above. RLS (FORCE on all five tables) is the binding control;
-- these grants are defence in depth, narrower than the blanket
-- ALTER DEFAULT PRIVILEGES backfill.
--   alert_kinds       - immutable seed vocabulary: SELECT only. No role,
--                       including the table owner, can INSERT/UPDATE/
--                       DELETE it outside this migration (FORCE RLS with
--                       no write policy, plus the deny-write trigger).
--   alerts            - SELECT/INSERT/UPDATE (the ack/resolve state
--                       guard). Never DELETE - the append-mostly guard
--                       trigger refuses it anyway.
--   alert_occurrences - append-only: SELECT/INSERT only.
--   alert_routes      - versioned with one-way supersession: SELECT/
--                       INSERT/UPDATE (superseding a route is an UPDATE
--                       of exactly superseded_at/superseded_by, enforced
--                       by the guard trigger). Never DELETE.
--   alert_deliveries  - append-only: SELECT/INSERT only.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'alert_kinds') THEN
        EXECUTE 'REVOKE ALL ON alert_kinds FROM igaming_runtime';
        EXECUTE 'GRANT SELECT ON alert_kinds TO igaming_runtime';
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'alerts') THEN
        EXECUTE 'REVOKE ALL ON alerts FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT, UPDATE ON alerts TO igaming_runtime';
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'alert_occurrences') THEN
        EXECUTE 'REVOKE ALL ON alert_occurrences FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON alert_occurrences TO igaming_runtime';
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'alert_routes') THEN
        EXECUTE 'REVOKE ALL ON alert_routes FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT, UPDATE ON alert_routes TO igaming_runtime';
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'alert_deliveries') THEN
        EXECUTE 'REVOKE ALL ON alert_deliveries FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON alert_deliveries TO igaming_runtime';
    END IF;
END
$$;

-- PRH-2 K1 (migration 0112; ADR 0099): least-privilege, re-asserted on
-- every run for the same reason as the blocks above. RLS (FORCE on every
-- one of these tables) is the binding control; these grants are defence
-- in depth, narrower than the blanket ALTER DEFAULT PRIVILEGES backfill.
--   financial_capability_catalogue,
--   financial_governance_permissions,
--   financial_capability_settings   - immutable seed vocabulary written
--                                     only by migration 0112 itself:
--                                     SELECT only, no write grant at all.
--   staff_capability_grant_requests - SELECT/INSERT/UPDATE (the
--                                     pending -> cancelled/expired/
--                                     approved/rejected transition; append-
--                                     only otherwise, enforced by the
--                                     guard trigger). Never DELETE.
--   staff_capability_grant_approvals - append-only: SELECT/INSERT only.
--   staff_capability_grants          - SELECT/INSERT/UPDATE (the one-way
--                                     revoke). Never DELETE - the guard
--                                     trigger refuses it anyway.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'financial_capability_catalogue') THEN
        EXECUTE 'REVOKE ALL ON financial_capability_catalogue FROM igaming_runtime';
        EXECUTE 'GRANT SELECT ON financial_capability_catalogue TO igaming_runtime';
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'financial_governance_permissions') THEN
        EXECUTE 'REVOKE ALL ON financial_governance_permissions FROM igaming_runtime';
        EXECUTE 'GRANT SELECT ON financial_governance_permissions TO igaming_runtime';
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'financial_capability_settings') THEN
        EXECUTE 'REVOKE ALL ON financial_capability_settings FROM igaming_runtime';
        EXECUTE 'GRANT SELECT ON financial_capability_settings TO igaming_runtime';
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'staff_capability_grant_requests') THEN
        EXECUTE 'REVOKE ALL ON staff_capability_grant_requests FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT, UPDATE ON staff_capability_grant_requests TO igaming_runtime';
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'staff_capability_grant_approvals') THEN
        EXECUTE 'REVOKE ALL ON staff_capability_grant_approvals FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON staff_capability_grant_approvals TO igaming_runtime';
    END IF;
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'staff_capability_grants') THEN
        EXECUTE 'REVOKE ALL ON staff_capability_grants FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT, UPDATE ON staff_capability_grants TO igaming_runtime';
    END IF;
END
$$;

-- PRH-2 K2 (migration 0113; ADR 0100): least-privilege, re-asserted on
-- every run, mirroring migration 0113's own in-migration grant block:
--   financial_control_classifications,
--   ledger_adjustment_reason_codes     - migration-written reference data:
--                                        SELECT only, no write grant.
--   financial_approval_policy_changes  - SELECT/INSERT/UPDATE (status only:
--                                        cancel/expire/decided by trigger).
--   financial_approval_policy_change_approvals,
--   financial_approval_policies,
--   tenant_financial_policy_profiles   - append-only: SELECT/INSERT.
--   ledger_adjustment_requests         - SELECT/INSERT/UPDATE (the state
--                                        machine; payload immutable by
--                                        trigger). Never DELETE.
--   ledger_adjustment_approvals        - append-only: SELECT/INSERT.
DO $$
DECLARE
    t RECORD;
BEGIN
    FOR t IN SELECT * FROM (VALUES
        ('financial_control_classifications', 'SELECT'),
        ('ledger_adjustment_reason_codes', 'SELECT'),
        ('financial_approval_policy_changes', 'SELECT, INSERT, UPDATE'),
        ('financial_approval_policy_change_approvals', 'SELECT, INSERT'),
        ('financial_approval_policies', 'SELECT, INSERT'),
        ('tenant_financial_policy_profiles', 'SELECT, INSERT'),
        ('ledger_adjustment_requests', 'SELECT, INSERT, UPDATE'),
        ('ledger_adjustment_approvals', 'SELECT, INSERT')) AS v(name, privs)
    LOOP
        IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = t.name) THEN
            EXECUTE format('REVOKE ALL ON %I FROM igaming_runtime', t.name);
            EXECUTE format('GRANT %s ON %I TO igaming_runtime', t.privs, t.name);
        END IF;
    END LOOP;
END
$$;

-- PRH-2 E1 (migration 0114; ADR 0106 section 6): least-privilege, re-asserted on
-- every run, mirroring migration 0114's own in-migration grant block:
--   kyc_submission_outbox - the KYC create/submit outbox: SELECT/INSERT/UPDATE
--                           only (the state machine is enforced by the guard
--                           trigger and RLS). Never DELETE, never TRUNCATE.
-- Grants on the nine tables fenced by the kyc_worker_fence_* policies are NOT
-- changed (the fence is RLS, not grants). No role, password or attribute change.
-- PRH-2 K3 (migration 0115; ADR 0101): least-privilege, re-asserted on every
-- run, mirroring migration 0115's own in-migration grant block:
--   payment_manual_resolution_codes     - migration-written reference data:
--                                         SELECT only, no write grant.
--   payment_manual_resolutions          - SELECT/INSERT/UPDATE (the state
--                                         machine; payload immutable by
--                                         trigger). Never DELETE.
--   payment_manual_resolution_approvals - append-only: SELECT/INSERT.
--   payment_attempt_reference_evidence  - append-only: SELECT/INSERT (the
--                                         poll's returned reference Y).
DO $$
DECLARE
    t RECORD;
BEGIN
    FOR t IN SELECT * FROM (VALUES
        ('payment_manual_resolution_codes', 'SELECT'),
        ('payment_manual_resolutions', 'SELECT, INSERT, UPDATE'),
        ('payment_manual_resolution_approvals', 'SELECT, INSERT'),
        ('payment_attempt_reference_evidence', 'SELECT, INSERT'),
        ('kyc_submission_outbox', 'SELECT, INSERT, UPDATE')) AS v(name, privs)
    LOOP
        IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = t.name) THEN
            EXECUTE format('REVOKE ALL ON %I FROM igaming_runtime', t.name);
            EXECUTE format('GRANT %s ON %I TO igaming_runtime', t.privs, t.name);
        END IF;
    END LOOP;
END
$$;

-- HSEC-APPROVED-HOLD-RELEASE-1 (migration 0124; ADR 0111 section 6): least-privilege,
-- re-asserted on every run, mirroring migration 0124's own in-migration grant block:
--   withdrawal_hold_resolutions          - SELECT/INSERT/UPDATE (the state machine;
--                                          payload immutable by trigger). Never DELETE.
--   withdrawal_hold_resolution_approvals - append-only: SELECT/INSERT.
-- No role, password or attribute change.
-- B13 (migration 0123; ADR 0111 section 2.1): least-privilege, re-asserted on
-- every run, mirroring migration 0123's own in-migration grant block:
--   payout_instrument_kinds, payout_instrument_verification_max_age
--                         - migration/owner-written reference data: SELECT only.
--   payout_instruments    - SELECT/INSERT/UPDATE (state machine and column
--                           discipline are triggers). Never DELETE.
--   payout_instrument_verifications, _fingerprint_owners, _blocking_events,
--   payout_attempt_destination_snapshots
--                         - append-only / write-once: SELECT/INSERT.
DO $$
DECLARE
    t RECORD;
BEGIN
    FOR t IN SELECT * FROM (VALUES
        ('withdrawal_hold_resolutions', 'SELECT, INSERT, UPDATE'),
        ('withdrawal_hold_resolution_approvals', 'SELECT, INSERT'),
        ('payout_instrument_kinds', 'SELECT'),
        ('payout_instrument_verification_max_age', 'SELECT'),
        ('payout_instruments', 'SELECT, INSERT, UPDATE'),
        ('payout_instrument_verifications', 'SELECT, INSERT'),
        ('payout_instrument_fingerprint_owners', 'SELECT, INSERT'),
        ('payout_instrument_blocking_events', 'SELECT, INSERT'),
        ('payout_attempt_destination_snapshots', 'SELECT, INSERT')) AS v(name, privs)
    LOOP
        IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = t.name) THEN
            EXECUTE format('REVOKE ALL ON %I FROM igaming_runtime', t.name);
            EXECUTE format('GRANT %s ON %I TO igaming_runtime', t.privs, t.name);
        END IF;
    END LOOP;
END
$$;
