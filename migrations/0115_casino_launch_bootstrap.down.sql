-- Reverses migration 0115. Refuses while rows exist in either table
-- (ADR 0103 §5 "Down: refuse while rows exist") - these are append-only
-- idempotency/reference records; a rollback that silently discarded them
-- could let a later re-bootstrap mint a SECOND casino_provider_player_refs
-- row (or a second casino_launch_bootstraps row against an
-- already-consumed session) after the tables are recreated, defeating the
-- very uniqueness these tables exist to guarantee. STOP and escalate to
-- the human if this refuses; never delete rows to force it through
-- (CLAUDE.md).
--
-- Both tables carry FORCE ROW LEVEL SECURITY with no policy that admits a
-- migration-context connection (no app.tenant_id is ever set while
-- migrations run) - a plain `SELECT ... FROM casino_launch_bootstraps`
-- guard would therefore see ZERO rows regardless of how many actually
-- exist (the exact migration 0048/0092/0107 lesson: "a SELECT pre-check
-- would see zero rows under RLS and let real duplicates/history through").
-- `ADD CONSTRAINT ... CHECK (false)` validates EVERY existing row at the
-- storage level, unconditionally, regardless of RLS - it fails
-- immediately if the table is non-empty, and is a no-op if it is empty
-- (nothing to validate). The table is dropped immediately after either
-- way, so the constraint itself is never meant to persist.
DO $$
BEGIN
    ALTER TABLE casino_launch_bootstraps ADD CONSTRAINT casino_launch_bootstraps_refuse_drop_nonempty CHECK (false);
EXCEPTION WHEN check_violation THEN
    RAISE EXCEPTION 'migration 0115 down: casino_launch_bootstraps has rows; refusing to drop an append-only idempotency table with history - escalate to the human, never delete rows to force this through';
END $$;

DO $$
BEGIN
    ALTER TABLE casino_provider_player_refs ADD CONSTRAINT casino_provider_player_refs_refuse_drop_nonempty CHECK (false);
EXCEPTION WHEN check_violation THEN
    RAISE EXCEPTION 'migration 0115 down: casino_provider_player_refs has rows; refusing to drop an append-only reference table with history - escalate to the human, never delete rows to force this through';
END $$;

DROP TRIGGER IF EXISTS casino_launch_bootstraps_insert_guard ON casino_launch_bootstraps;
DROP FUNCTION IF EXISTS casino_launch_bootstraps_insert_guard();
DROP TABLE casino_launch_bootstraps;
DROP TABLE casino_provider_player_refs;

ALTER TABLE casino_launch_sessions DROP CONSTRAINT casino_launch_sessions_id_tenant_id_key;
