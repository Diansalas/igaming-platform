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

-- ADR 0112 slice 4 (migration 0131, residual 14(b2)): a withdrawal request is never deleted. The deny
-- trigger of 0131 (WD012) is the BINDING control and binds the owner too; this REVOKE is defence in
-- depth and re-asserts the narrowing every time this idempotent script re-runs (the blanket GRANT above
-- would otherwise re-grant DELETE). Nothing in the application deletes withdrawal_requests. Guarded like
-- the blocks below: the table does not exist yet on a fresh initdb run. This script is applied by a
-- human/CI to a database; nothing here is applied to a live role by an agent.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM information_schema.tables
        WHERE table_schema = 'public' AND table_name = 'withdrawal_requests'
    ) THEN
        EXECUTE 'REVOKE DELETE, TRUNCATE ON withdrawal_requests FROM igaming_runtime';
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

-- F1 (migration 0166): the key table lives in the OWNER-ONLY schema actorproof_private. The blanket grants above cover schema public only, so
-- nothing here re-grants it; the denial (no USAGE, no table privilege) is still re-asserted on every run. Guarded: the schema does not exist
-- before migration 0166.
DO $$
BEGIN
    IF to_regnamespace('actorproof_private') IS NOT NULL THEN
        EXECUTE 'REVOKE ALL ON SCHEMA actorproof_private FROM igaming_runtime';
        EXECUTE 'REVOKE ALL ON ALL TABLES IN SCHEMA actorproof_private FROM igaming_runtime';
        EXECUTE 'REVOKE ALL ON ALL SEQUENCES IN SCHEMA actorproof_private FROM igaming_runtime';
        EXECUTE 'REVOKE ALL ON ALL FUNCTIONS IN SCHEMA actorproof_private FROM igaming_runtime';
    END IF;
END
$$;

-- ADR 0116 (migrations 0140-0142, AML holds / cases / four-eyes): every aml_* table is DEFINER-ONLY. The blanket backfill
-- GRANT above ("ALL TABLES IN SCHEMA public") would re-grant the runtime role SELECT/INSERT/UPDATE/DELETE on them every time
-- this idempotent script is re-run against an already-migrated database; migration 0141/0142 themselves run only once. The
-- tables also carry FORCE ROW LEVEL SECURITY with ONE policy, for the table OWNER only, so a re-granted privilege would still
-- yield zero rows (a second, independent denial); the REVOKE below re-asserts the first. The runtime role keeps EXECUTE on the
-- SECURITY DEFINER officer / enforcement API only; the helper, guard and trigger functions stay owner-only. Guarded: nothing
-- exists before the migrations ran.
DO $$
DECLARE
    t text;
    f text;
BEGIN
    FOR t IN SELECT c.relname FROM pg_class c WHERE c.relnamespace = 'public'::regnamespace AND c.relkind IN ('r', 'p') AND c.relname LIKE 'aml\_%' LOOP
        EXECUTE format('REVOKE ALL ON %I FROM igaming_runtime', t);
    END LOOP;
    -- Sequences too (the schema default privileges would otherwise leave USAGE on them for the runtime role).
    FOR t IN SELECT c.relname FROM pg_class c WHERE c.relnamespace = 'public'::regnamespace AND c.relkind = 'S' AND c.relname LIKE 'aml\_%' LOOP
        EXECUTE format('REVOKE ALL ON SEQUENCE %I FROM igaming_runtime', t);
    END LOOP;
    FOREACH f IN ARRAY ARRAY[
        'aml_hold_active(uuid, uuid, uuid, text)', 'aml_hold_match(uuid, uuid, uuid, text)',
        'aml_record_enforcement_decision(uuid, uuid, uuid, text, text, text, text, boolean, uuid, uuid)',
        'aml_hold_place(uuid, uuid, uuid, text, text, text)', 'aml_hold_lift(uuid, uuid, text)', 'aml_hold_list(uuid, uuid)',
        'aml_alert_raise(uuid, uuid, text, text)', 'aml_case_open(uuid, uuid, text, text, uuid[])',
        'aml_case_transition(uuid, uuid, integer, text, text)', 'aml_case_add_note(uuid, uuid, integer, text, text)',
        'aml_case_flag_sar(uuid, uuid, integer, text)', 'aml_case_propose(uuid, uuid, integer, text, uuid, text, text)',
        'aml_case_approve(uuid, uuid, integer, text, text)', 'aml_case_read(uuid, uuid)', 'aml_case_events_list(uuid, uuid)',
        'aml_case_notes_list(uuid, uuid)', 'aml_case_queue(uuid, integer)',
        -- Migration 0154 (A7): screening policy / record / list and the launch readiness reader.
        'aml_screening_policy_set(uuid, uuid, text[], bigint, bigint, timestamptz, text, text)', 'aml_screening_policy_effective(uuid, uuid)',
        'aml_screening_record(uuid, uuid, text, text, text, text, text, text[], text, text[], text, timestamptz, text, text)',
        'aml_screening_list(uuid, uuid)', 'aml_readiness(uuid, uuid[])',
        -- Migration 0181 (lic10, G1): the automatic screening trigger policy, queue and worker API.
        'aml_screening_trigger_policy_set(uuid, uuid, text[], boolean, bigint, timestamptz, text, text)', 'aml_screening_trigger_policy_effective(uuid, uuid)',
        'aml_screening_gate_overdue(uuid, integer)', 'screening_worker_heartbeat(text, boolean)',
        'aml_screening_request_list(uuid, uuid)', 'aml_screening_enqueue_due(uuid, integer)', 'aml_screening_request_claim(uuid, integer, integer)',
        'aml_screening_request_finish(uuid, uuid, uuid, text, text[], text, text, text, text, timestamptz, integer, text)',
        'aml_screening_request_replace(uuid, uuid, uuid, text)', 'aml_screening_parked_summary(uuid, timestamptz)'
    ] LOOP
        IF to_regprocedure('public.' || f) IS NOT NULL THEN
            EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO igaming_runtime', f);
        END IF;
    END LOOP;
    -- NOT granted until the HD-RET-1 erasure job exists as its caller (security F5): re-assert the revoke.
    IF to_regprocedure('public.aml_person_has_records(uuid, uuid)') IS NOT NULL THEN
        REVOKE ALL ON FUNCTION aml_person_has_records(uuid, uuid) FROM igaming_runtime;
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

-- ADR 0112 slice 1 (migration 0128, launch authorisation status governance):
-- least-privilege, re-asserted on every run, mirroring migration 0128's own
-- in-migration grant block. RLS (FORCE on all three) plus the guard triggers are the
-- binding controls; these grants are defence in depth, narrower than the blanket
-- backfill GRANT above, which would otherwise re-grant DELETE on re-run:
--   launch_authorisation_requests  - SELECT/INSERT, and UPDATE on the four state
--                                    columns only (the state machine is the guard
--                                    trigger's; request content is immutable). Never
--                                    DELETE or TRUNCATE.
--   launch_authorisation_approvals,
--   launch_status_transitions      - append-only: SELECT/INSERT only.
-- The owner-only transition kinds (legacy_baseline, owner_provisioned) are written by
-- the table owner role, never by igaming_runtime (guard LA013, S2). No role, password
-- or attribute change.
DO $$
BEGIN
    IF to_regclass('public.launch_authorisation_requests') IS NOT NULL THEN
        EXECUTE 'REVOKE ALL ON launch_authorisation_requests FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON launch_authorisation_requests TO igaming_runtime';
        EXECUTE 'GRANT UPDATE (status, decided_at, refusal_code, executing_txid) ON launch_authorisation_requests TO igaming_runtime';
    END IF;
    IF to_regclass('public.launch_authorisation_approvals') IS NOT NULL THEN
        EXECUTE 'REVOKE ALL ON launch_authorisation_approvals FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON launch_authorisation_approvals TO igaming_runtime';
    END IF;
    IF to_regclass('public.launch_status_transitions') IS NOT NULL THEN
        EXECUTE 'REVOKE ALL ON launch_status_transitions FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON launch_status_transitions TO igaming_runtime';
    END IF;
END
$$;

-- Migration 0177 (MKT-DUAL-1, licensing / jurisdiction maker-checker): least-privilege, re-asserted on every run,
-- mirroring migration 0177's own in-migration grant block. RLS (FORCE on all three), the guard triggers (proof-bound state
-- machine, append-only) are the binding controls; these grants are defence in depth, narrower than the blanket backfill
-- GRANT above (which would otherwise re-grant UPDATE / DELETE):
--   licensing_change_kinds     - the closed kind registry: SELECT only (written by migrations only).
--   licensing_change_requests  - SELECT/INSERT, and UPDATE on the eight state columns only (request content is immutable;
--                                the guard re-derives every state column). Never DELETE or TRUNCATE.
--   licensing_change_approvals - append-only: SELECT/INSERT only.
-- No role, password or attribute change.
DO $$
BEGIN
    IF to_regclass('public.licensing_change_kinds') IS NOT NULL THEN
        EXECUTE 'REVOKE ALL ON licensing_change_kinds FROM igaming_runtime';
        EXECUTE 'GRANT SELECT ON licensing_change_kinds TO igaming_runtime';
    END IF;
    IF to_regclass('public.licensing_change_requests') IS NOT NULL THEN
        EXECUTE 'REVOKE ALL ON licensing_change_requests FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON licensing_change_requests TO igaming_runtime';
        EXECUTE 'GRANT UPDATE (status, decided_by, decided_by_person_id, decided_at, decision_reason_code, executed_by, executed_at, executed_txid) ON licensing_change_requests TO igaming_runtime';
    END IF;
    IF to_regclass('public.licensing_change_approvals') IS NOT NULL THEN
        EXECUTE 'REVOKE ALL ON licensing_change_approvals FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON licensing_change_approvals TO igaming_runtime';
    END IF;
END
$$;

-- Migration 0178 (lic4 slice 4a, security review F-2): licence_versions is written ONLY by the licences trigger (a SECURITY
-- DEFINER function); the runtime role keeps SELECT, never INSERT (the blanket backfill GRANT above would re-grant it).
DO $$
BEGIN
    IF to_regclass('public.licence_versions') IS NOT NULL THEN
        EXECUTE 'REVOKE INSERT ON licence_versions FROM igaming_runtime';
    END IF;
END
$$;

-- Migration 0179 (lic6, security re-review I-2): no code path deletes a licence, and the runtime role must not be able to (the
-- blanket backfill GRANT above would re-grant DELETE). Re-asserted on every run.
DO $$
BEGIN
    IF to_regclass('public.licences') IS NOT NULL THEN
        EXECUTE 'REVOKE DELETE ON licences FROM igaming_runtime';
    END IF;
END
$$;

-- Migration 0180 (lic9, explicit operating bases): operating_bases is governed and append-only. The runtime role keeps SELECT and
-- INSERT and UPDATE on the single column effective_to (the close of a replaced version), all admitted only by the governed-write
-- guard (an executed licensing change request of this transaction, LC040); never DELETE, TRUNCATE or a blanket UPDATE (the blanket
-- backfill GRANT above would otherwise re-grant them). Re-asserted on every run.
DO $$
BEGIN
    IF to_regclass('public.operating_bases') IS NOT NULL THEN
        EXECUTE 'REVOKE ALL ON operating_bases FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON operating_bases TO igaming_runtime';
        EXECUTE 'GRANT UPDATE (effective_to) ON operating_bases TO igaming_runtime';
    END IF;
END
$$;

-- ADR 0115 S1 (migration 0135, RG limits schema): least-privilege, re-asserted on every run,
-- mirroring migration 0135's own in-migration grant block. RLS (FORCE on the three RG tables) and
-- the guard / deny triggers are the binding controls; these grants are defence in depth, narrower
-- than the blanket backfill GRANT above (which would otherwise re-grant UPDATE / DELETE):
--   database_environment            - the owner-written environment anchor: SELECT only. The runtime
--                                     role must NEVER write it (a dev/test anchor would admit
--                                     SYNTHETIC-NOT-LEGAL RG configuration); its insert guard also
--                                     refuses any role that is not a member of the owner role.
--   rg_limit_config_change_requests - SELECT/INSERT, and UPDATE on status only (decided_* are forced
--                                     by the guard trigger). Never DELETE or TRUNCATE.
--   rg_limit_configs,
--   rg_enforcement_activations (0158),
--   player_rg_limit_changes         - append-only: SELECT/INSERT only (+ the seq sequence).
-- No role, password or attribute change.
DO $$
DECLARE
    t RECORD;
BEGIN
    FOR t IN SELECT * FROM (VALUES
        ('database_environment', 'SELECT'),
        ('rg_limit_config_change_requests', 'SELECT, INSERT'),
        ('rg_limit_configs', 'SELECT, INSERT'),
        ('rg_enforcement_activations', 'SELECT, INSERT'),
        ('player_rg_limit_changes', 'SELECT, INSERT')) AS v(name, privs)
    LOOP
        IF to_regclass('public.' || t.name) IS NOT NULL THEN
            EXECUTE format('REVOKE ALL ON %I FROM igaming_runtime', t.name);
            EXECUTE format('GRANT %s ON %I TO igaming_runtime', t.privs, t.name);
        END IF;
    END LOOP;
    IF to_regclass('public.rg_limit_config_change_requests') IS NOT NULL THEN
        EXECUTE 'GRANT UPDATE (status) ON rg_limit_config_change_requests TO igaming_runtime';
    END IF;
    IF to_regclass('public.player_rg_limit_changes_seq_seq') IS NOT NULL THEN
        EXECUTE 'GRANT USAGE ON SEQUENCE player_rg_limit_changes_seq_seq TO igaming_runtime';
    END IF;
END
$$;

-- ADR 0119 S2 (migration 0137, staff MFA tables): least-privilege, re-asserted on every run, mirroring
-- migration 0137's own in-migration grant block. RLS (FORCE on the four tables), the guard triggers and
-- the append-only (no DELETE / TRUNCATE) trigger are the binding controls; these grants are defence in
-- depth, narrower than the blanket backfill GRANT above (which would otherwise re-grant UPDATE / DELETE).
-- SELECT and INSERT on each table; UPDATE is COLUMN-level only and never reaches the immutable columns
-- (secret_kid / secret_nonce / secret_ct, code_mac, token_mac, tenant_id, staff_user_id ...). The
-- runtime role can therefore never rewrite a factor secret, a recovery-code or token MAC, or delete a row.
-- No role, password or attribute change.
DO $$
BEGIN
    IF to_regclass('public.staff_mfa_factors') IS NOT NULL THEN
        EXECUTE 'REVOKE ALL ON staff_mfa_factors FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON staff_mfa_factors TO igaming_runtime';
        EXECUTE 'GRANT UPDATE (status, last_used_step, failed_count, failed_window_start, locked_until, failed_24h_count, failed_24h_start, hard_locked_at, pending_expires_at, activated_at, revoked_at, revoked_reason) ON staff_mfa_factors TO igaming_runtime';
    END IF;
    IF to_regclass('public.staff_mfa_recovery_codes') IS NOT NULL THEN
        EXECUTE 'REVOKE ALL ON staff_mfa_recovery_codes FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON staff_mfa_recovery_codes TO igaming_runtime';
        EXECUTE 'GRANT UPDATE (consumed_at, revoked_at) ON staff_mfa_recovery_codes TO igaming_runtime';
    END IF;
    IF to_regclass('public.staff_auth_challenges') IS NOT NULL THEN
        EXECUTE 'REVOKE ALL ON staff_auth_challenges FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON staff_auth_challenges TO igaming_runtime';
        EXECUTE 'GRANT UPDATE (attempts_left, consumed_at) ON staff_auth_challenges TO igaming_runtime';
    END IF;
    IF to_regclass('public.staff_enrolment_tokens') IS NOT NULL THEN
        EXECUTE 'REVOKE ALL ON staff_enrolment_tokens FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON staff_enrolment_tokens TO igaming_runtime';
        EXECUTE 'GRANT UPDATE (consumed_at, revoked_at) ON staff_enrolment_tokens TO igaming_runtime';
    END IF;
END
$$;

-- ADR 0119 S3 (migration 0138, staff session assurance): least-privilege, re-asserted on every run, mirroring
-- migration 0138's own in-migration grant block. The blanket backfill GRANT above re-grants table-level UPDATE on
-- sessions; this narrows it again to the columns the S3 application writes (revocation, rotation link, last use; the
-- step-up mfa_at + re-seal columns are granted once migration 0151 (S4b) exists, review F9). The runtime role can never UPDATE aal, amr,
-- auth_time, absolute_expires_at, expires_at or an identity column (mfa_at and the seal only after 0151) (the SF040 trigger refuses the immutable ones for every role). The cutover marker
-- is owner-written; the runtime role reads it only. No role, password or attribute change.
DO $$
DECLARE
    v_s4b boolean := false;
BEGIN
    IF to_regclass('public.staff_session_cutover') IS NOT NULL THEN
        EXECUTE 'REVOKE UPDATE ON sessions FROM igaming_runtime';
        EXECUTE 'GRANT UPDATE (revoked_at, last_used_at, replaced_by_session_id) ON sessions TO igaming_runtime';
        -- ADR 0119 S4b (migration 0151): the step-up writes mfa_at and the re-seal on the chain-head row only.
        -- Granted only once 0151 has been applied (review F9: the grant arrives with the slice that writes them).
        EXECUTE 'SELECT EXISTS (SELECT 1 FROM public.schema_migrations WHERE version = 151)' INTO v_s4b;
        IF v_s4b THEN
            EXECUTE 'GRANT UPDATE (mfa_at, assurance_kid, assurance_seal) ON sessions TO igaming_runtime';
        END IF;
        EXECUTE 'REVOKE ALL ON staff_session_cutover FROM igaming_runtime';
        EXECUTE 'GRANT SELECT ON staff_session_cutover TO igaming_runtime';
    END IF;
END
$$;

-- ADR 0119 S5 + S6 (migration 0139, staff lifecycle): least-privilege, re-asserted on every run, mirroring migration
-- 0139's own grant block. The blanket backfill GRANT above re-grants table-level UPDATE on staff_users; this narrows it
-- to the columns the application writes (status, role, password_hash, status_changed_at, offboarded_at, person_id,
-- display_name, updated_at) - NEVER staff_version (set only by the SF012 trigger), id, tenant_id, email, created_at or
-- invited_by. The lifecycle tables: SELECT and INSERT (the four-eyes requests: UPDATE of the state columns only); no
-- DELETE or TRUNCATE. Applied only once 0139 exists (the to_regclass test). No role, password or attribute change.
DO $$
BEGIN
    IF to_regclass('public.staff_lifecycle_events') IS NOT NULL THEN
        EXECUTE 'REVOKE UPDATE ON staff_users FROM igaming_runtime';
        EXECUTE 'GRANT UPDATE (status, role, password_hash, status_changed_at, offboarded_at, person_id, display_name, updated_at) ON staff_users TO igaming_runtime';
        EXECUTE 'REVOKE DELETE ON staff_users FROM igaming_runtime';
        EXECUTE 'REVOKE ALL ON staff_lifecycle_requests FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON staff_lifecycle_requests TO igaming_runtime';
        EXECUTE 'GRANT UPDATE (state, decided_at, executing_txid, target_staff_id) ON staff_lifecycle_requests TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON staff_lifecycle_approvals FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON staff_lifecycle_approvals TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON staff_lifecycle_events FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON staff_lifecycle_events TO igaming_runtime';
    END IF;
END
$$;

-- F4 (migration 0169, WD-RG-1 dispatch backstop): withdrawal_gate_decisions is append-only. Its deny trigger (WD055)
-- and FORCE RLS are the binding controls; this narrowing is defence in depth against the blanket backfill GRANT above
-- (which would otherwise re-grant UPDATE / DELETE). SELECT and INSERT only. Applied only once 0169 exists (the
-- to_regclass test). No role, password or attribute change.
DO $$
BEGIN
    IF to_regclass('public.withdrawal_gate_decisions') IS NOT NULL THEN
        EXECUTE 'REVOKE ALL ON withdrawal_gate_decisions FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON withdrawal_gate_decisions TO igaming_runtime';
    END IF;
END
$$;

-- Migration 0181 (lic10, G8/Q21): the residence determination maker-checker. The three tables are owner-written (SECURITY DEFINER
-- functions); the runtime role keeps SELECT on residence_determination_checks ONLY (tenant RLS: the evidence read and the pending
-- list) and EXECUTE on the four definer API functions. Re-asserted on every run because the blanket GRANT above would otherwise
-- re-grant the tables. Guarded: nothing exists before the migrations ran.
DO $$
DECLARE
    t text;
    f text;
BEGIN
    FOREACH t IN ARRAY ARRAY['residence_approval_policies', 'residence_approval_policy_approvals', 'residence_determination_checks'] LOOP
        IF to_regclass('public.' || t) IS NOT NULL THEN
            EXECUTE format('REVOKE ALL ON %I FROM igaming_runtime', t);
        END IF;
    END LOOP;
    IF to_regclass('public.residence_determination_checks') IS NOT NULL THEN
        GRANT SELECT ON residence_determination_checks TO igaming_runtime;
    END IF;
    FOREACH f IN ARRAY ARRAY['residence_policy_set(uuid, text, text)', 'residence_policy_approve(uuid, uuid, text)', 'residence_policy_list(uuid)',
                             'residence_determination_approve(uuid, uuid, timestamptz, text)'] LOOP
        IF to_regprocedure('public.' || f) IS NOT NULL THEN
            EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO igaming_runtime', f);
        END IF;
    END LOOP;
END $$;

-- Migration 0181 (lic18): the worker heartbeat table is written only by its SECURITY DEFINER function; the runtime role has no
-- table privilege (re-asserted: the blanket GRANT above would otherwise hand it out).
DO $$
BEGIN
    IF to_regclass('public.screening_worker_heartbeats') IS NOT NULL THEN
        REVOKE ALL ON screening_worker_heartbeats FROM igaming_runtime;
    END IF;
END $$;

-- Migration 0182 (lic11, withdrawal restriction policy): least-privilege, re-asserted on every run, mirroring 0182's own grant
-- block. FORCE RLS and the guard triggers (append-only, proof-bound maker-checker, WR040 governed write) are the binding
-- controls; this narrowing is defence in depth against the blanket backfill GRANT above (UPDATE / DELETE):
--   withdrawal_restriction_causes           - migration-written registry: SELECT only.
--   withdrawal_restriction_policies         - append-only: SELECT/INSERT only.
--   withdrawal_restriction_policy_changes   - SELECT/INSERT, UPDATE on the eight state columns only.
--   withdrawal_restriction_policy_approvals - append-only: SELECT/INSERT only.
-- No role, password or attribute change.
DO $$
BEGIN
    IF to_regclass('public.withdrawal_restriction_causes') IS NOT NULL THEN
        EXECUTE 'REVOKE ALL ON withdrawal_restriction_causes FROM igaming_runtime';
        EXECUTE 'GRANT SELECT ON withdrawal_restriction_causes TO igaming_runtime';
    END IF;
    IF to_regclass('public.withdrawal_restriction_policies') IS NOT NULL THEN
        EXECUTE 'REVOKE ALL ON withdrawal_restriction_policies FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON withdrawal_restriction_policies TO igaming_runtime';
    END IF;
    IF to_regclass('public.withdrawal_restriction_policy_changes') IS NOT NULL THEN
        EXECUTE 'REVOKE ALL ON withdrawal_restriction_policy_changes FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON withdrawal_restriction_policy_changes TO igaming_runtime';
        EXECUTE 'GRANT UPDATE (status, decided_by, decided_by_person_id, decided_at, decision_reason_code, executed_by, executed_at, executed_txid) ON withdrawal_restriction_policy_changes TO igaming_runtime';
    END IF;
    IF to_regclass('public.withdrawal_restriction_policy_approvals') IS NOT NULL THEN
        EXECUTE 'REVOKE ALL ON withdrawal_restriction_policy_approvals FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON withdrawal_restriction_policy_approvals TO igaming_runtime';
    END IF;
END
$$;
