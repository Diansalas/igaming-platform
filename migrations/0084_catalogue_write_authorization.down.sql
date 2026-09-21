-- Clean inverse of 0084_catalogue_write_authorization.up.sql. Drops the
-- eighteen triggers, then the twenty-four policies, then disables RLS
-- (NO FORCE first, then DISABLE) on all six tables, then drops the shared
-- trigger function - reverse order of creation.

-- ----------------------------------------------------------------------
-- Triggers
-- ----------------------------------------------------------------------

DROP TRIGGER IF EXISTS sb_selections_no_truncate ON sb_selections;
DROP TRIGGER IF EXISTS sb_selections_deny_delete ON sb_selections;
DROP TRIGGER IF EXISTS sb_selections_immutable_identity ON sb_selections;

DROP TRIGGER IF EXISTS sb_markets_no_truncate ON sb_markets;
DROP TRIGGER IF EXISTS sb_markets_deny_delete ON sb_markets;
DROP TRIGGER IF EXISTS sb_markets_immutable_identity ON sb_markets;

DROP TRIGGER IF EXISTS sb_events_no_truncate ON sb_events;
DROP TRIGGER IF EXISTS sb_events_deny_delete ON sb_events;
DROP TRIGGER IF EXISTS sb_events_immutable_identity ON sb_events;

DROP TRIGGER IF EXISTS sb_competitions_no_truncate ON sb_competitions;
DROP TRIGGER IF EXISTS sb_competitions_deny_delete ON sb_competitions;
DROP TRIGGER IF EXISTS sb_competitions_immutable_identity ON sb_competitions;

DROP TRIGGER IF EXISTS sb_sports_no_truncate ON sb_sports;
DROP TRIGGER IF EXISTS sb_sports_deny_delete ON sb_sports;
DROP TRIGGER IF EXISTS sb_sports_immutable_identity ON sb_sports;

DROP TRIGGER IF EXISTS casino_games_no_truncate ON casino_games;
DROP TRIGGER IF EXISTS casino_games_deny_delete ON casino_games;
DROP TRIGGER IF EXISTS casino_games_immutable_identity ON casino_games;

-- ----------------------------------------------------------------------
-- Policies
-- ----------------------------------------------------------------------

DROP POLICY IF EXISTS sb_selections_catalogue_sync_delete_visibility ON sb_selections;
DROP POLICY IF EXISTS sb_selections_catalogue_sync_update ON sb_selections;
DROP POLICY IF EXISTS sb_selections_catalogue_sync_insert ON sb_selections;
DROP POLICY IF EXISTS sb_selections_read ON sb_selections;

DROP POLICY IF EXISTS sb_markets_catalogue_sync_delete_visibility ON sb_markets;
DROP POLICY IF EXISTS sb_markets_catalogue_sync_update ON sb_markets;
DROP POLICY IF EXISTS sb_markets_catalogue_sync_insert ON sb_markets;
DROP POLICY IF EXISTS sb_markets_read ON sb_markets;

DROP POLICY IF EXISTS sb_events_catalogue_sync_delete_visibility ON sb_events;
DROP POLICY IF EXISTS sb_events_catalogue_sync_update ON sb_events;
DROP POLICY IF EXISTS sb_events_catalogue_sync_insert ON sb_events;
DROP POLICY IF EXISTS sb_events_read ON sb_events;

DROP POLICY IF EXISTS sb_competitions_catalogue_sync_delete_visibility ON sb_competitions;
DROP POLICY IF EXISTS sb_competitions_catalogue_sync_update ON sb_competitions;
DROP POLICY IF EXISTS sb_competitions_catalogue_sync_insert ON sb_competitions;
DROP POLICY IF EXISTS sb_competitions_read ON sb_competitions;

DROP POLICY IF EXISTS sb_sports_catalogue_sync_delete_visibility ON sb_sports;
DROP POLICY IF EXISTS sb_sports_catalogue_sync_update ON sb_sports;
DROP POLICY IF EXISTS sb_sports_catalogue_sync_insert ON sb_sports;
DROP POLICY IF EXISTS sb_sports_read ON sb_sports;

DROP POLICY IF EXISTS casino_games_platform_admin_delete_visibility ON casino_games;
DROP POLICY IF EXISTS casino_games_platform_admin_update ON casino_games;
DROP POLICY IF EXISTS casino_games_platform_admin_insert ON casino_games;
DROP POLICY IF EXISTS casino_games_read ON casino_games;

-- ----------------------------------------------------------------------
-- Row-level security state (NO FORCE before DISABLE, mirroring migration
-- 0081 §3.4's binding rule against toggling FORCE alone; here we restore
-- the pre-migration "no RLS at all" state completely)
-- ----------------------------------------------------------------------

ALTER TABLE sb_selections NO FORCE ROW LEVEL SECURITY;
ALTER TABLE sb_selections DISABLE ROW LEVEL SECURITY;

ALTER TABLE sb_markets NO FORCE ROW LEVEL SECURITY;
ALTER TABLE sb_markets DISABLE ROW LEVEL SECURITY;

ALTER TABLE sb_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE sb_events DISABLE ROW LEVEL SECURITY;

ALTER TABLE sb_competitions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE sb_competitions DISABLE ROW LEVEL SECURITY;

ALTER TABLE sb_sports NO FORCE ROW LEVEL SECURITY;
ALTER TABLE sb_sports DISABLE ROW LEVEL SECURITY;

ALTER TABLE casino_games NO FORCE ROW LEVEL SECURITY;
ALTER TABLE casino_games DISABLE ROW LEVEL SECURITY;

-- ----------------------------------------------------------------------
-- Shared trigger function
-- ----------------------------------------------------------------------

DROP FUNCTION IF EXISTS catalogue_enforce_immutable_identity();
