-- Stage 9.1, closes ARCH-DB-2 (HIGH, FIX BEFORE PRODUCTION), per
-- docs/decisions/0081-arch-db-2-catalogue-write-authorization.md.
--
-- Numbering note: ADR 0081 §6 specified this migration as "0083". By the
-- time this wave implemented it, a separate, parallel Stage 9.1 devops
-- workstream (PLAT-MIGDRIFT-1) had already landed
-- migrations/0083_migration_checksum_tracking as the new tip, so this
-- migration is 0084 instead. The ADR's own text (updated alongside this
-- file) now reads 0084 wherever it means this migration. No design
-- change resulted from the renumbering.
--
-- Six platform-wide catalogue tables - `casino_games` (migration 0035),
-- `sb_sports`/`sb_competitions`/`sb_events`/`sb_markets`/`sb_selections`
-- (migration 0078) - carry NO row-level security today: no ENABLE, no
-- policies, no write-side triggers. None of the six carries a `tenant_id`
-- or `brand_id` column, and ADR 0081 §2.2 rules that adding one would be
-- semantically WRONG, not merely unnecessary - they are single-canonical-
-- row platform catalogue data; per-tenant scoping already lives in
-- `casino_game_availability` for casino, and a future, separate
-- tenant-owned table for sportsbook (out of scope here). That "no
-- tenant_id column" half of the prior exception is therefore permanent
-- and intentional and stays true after this migration. The "no RLS at
-- all" half is what this migration closes, in the `assets` registry's own
-- ENABLE+FORCE shape (migration 0044 §3), adapted per ADR 0081 §2.5 where
-- the semantics genuinely differ:
--
--   * Read stays open (`FOR SELECT USING (true)`) on all six - all five
--     `sb_*` tables have genuinely anonymous readers (GET
--     /v1/sportsbook/sports, GET /v1/sportsbook/events/{id}), and
--     PostgreSQL bypasses RLS for foreign-key/referential-integrity
--     checks regardless of a narrower predicate (ADR 0081 §2.3).
--   * `casino_games` writes are scoped to the EXISTING
--     `app.platform_admin_principal_id` GUC (db.Pool.WithPlatformAdmin) -
--     a request-scoped human platform admin. No new concept.
--   * The five `sb_*` tables' writes are scoped to a NEW, closed-
--     vocabulary `app.platform_service_id = 'sportsbook_catalogue_sync'`
--     GUC, set only by the new db.Pool.WithPlatformService - the
--     sportsbook catalogue sync is a startup-time background process with
--     no authenticated principal, and WithoutTenant (ADR 0014 §1's prior
--     platform-service pattern) is the SAME scope every ordinary platform
--     read already uses, so a write policy keyed on it would grant
--     nothing (ADR 0081 §3.2).
--   * Unlike `assets`, creation is NOT dual-controlled (ADR 0081 §2.5.1):
--     a `casino_games` row authorizes nothing on its own without a
--     tenant's own `casino_game_availability` opt-in, so four-eyes-on-
--     create would be friction with no control behind it.
--   * Unlike `assets.decimal_exponent`, `sb_selections.odds_*` are
--     DELIBERATELY left mutable (ADR 0081 §2.5.2) - updating live odds is
--     the catalogue sync's entire job; already-placed bets are protected
--     by the acceptance-time odds freeze on `sportsbook_bets` (migration
--     0082 §1.1), not by freezing the catalogue's own current price.
--
-- Immutable-identity triggers freeze each row's own identity and parent
-- links (never its presentational/price fields) - ADR 0081 §4.2's table,
-- reproduced at each block below. Deny-DELETE and deny-TRUNCATE triggers
-- make the catalogue delete-free: a `casino_games`/`sb_events` row is
-- referenced by bets, launch sessions, provider rounds and risk rules, so
-- a silent zero-row no-op (RLS's default DELETE behaviour with no policy)
-- would be a worse answer than a loud refusal - `status` is the intended
-- disable/cancel mechanism instead.
--
-- No data statement touches any of the six tables in this migration, so
-- the migration-time ordering rule (ADR 0081 §3.4 - any data statement
-- must precede ENABLE/FORCE) does not apply here.

-- ----------------------------------------------------------------------
-- 1. Shared immutable-identity + deny-delete/truncate trigger function
-- ----------------------------------------------------------------------
-- One generic function driven by TG_ARGV, in the spirit of the shared
-- ledger_deny_mutation() (migration 0021) the codebase already reuses
-- across migrations 0022/0026/0044/0053-0063/0068/0069/0082. The
-- to_jsonb conversions happen INSIDE the body, after the DELETE/TRUNCATE
-- branch, deliberately: in a BEFORE DELETE FOR EACH ROW trigger NEW is
-- unassigned, and touching it earlier raises a confusing plpgsql error
-- before the intended message is reached (migration 0044's
-- assets_enforce_immutable_identity has the identical structure for the
-- identical reason).
CREATE FUNCTION catalogue_enforce_immutable_identity() RETURNS TRIGGER AS $$
DECLARE
    v_col TEXT;
    v_old JSONB;
    v_new JSONB;
BEGIN
    IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
        RAISE EXCEPTION '%: % is not permitted - catalogue rows are referenced by bets, launch sessions, provider rounds and risk rules; disable (casino_games.status) or cancel (sb_events.status) the row instead (ADR 0081)', TG_TABLE_NAME, TG_OP;
    END IF;
    v_old := to_jsonb(OLD);
    v_new := to_jsonb(NEW);
    FOREACH v_col IN ARRAY TG_ARGV LOOP
        IF v_old -> v_col IS DISTINCT FROM v_new -> v_col THEN
            RAISE EXCEPTION '%.% is immutable after creation (ADR 0081) - a catalogue row''s identity may never be repointed at a different provider entity', TG_TABLE_NAME, v_col;
        END IF;
    END LOOP;
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ----------------------------------------------------------------------
-- 2. casino_games
-- ----------------------------------------------------------------------
-- Immutable: id, provider_id, provider_game_id, created_at.
-- Mutable (untouched by this migration): name, game_type, rtp_variant,
-- volatility, feature_flags, supported_assets, mobile_supported,
-- demo_supported, jurisdiction_blocklist, status.

ALTER TABLE casino_games ENABLE ROW LEVEL SECURITY;
ALTER TABLE casino_games FORCE ROW LEVEL SECURITY;

CREATE POLICY casino_games_read ON casino_games
    FOR SELECT USING (true);

CREATE POLICY casino_games_platform_admin_insert ON casino_games
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY casino_games_platform_admin_update ON casino_games
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- DELETE gets scope VISIBILITY only - the deny-delete trigger below is
-- what actually refuses. Without any DELETE policy at all, RLS makes
-- every row invisible to DELETE, so the statement silently affects zero
-- rows instead of failing loudly (migration 0044's identical
-- assets_platform_admin_delete_visibility precedent/rationale).
CREATE POLICY casino_games_platform_admin_delete_visibility ON casino_games
    FOR DELETE
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE TRIGGER casino_games_immutable_identity
    BEFORE UPDATE ON casino_games
    FOR EACH ROW EXECUTE FUNCTION catalogue_enforce_immutable_identity('id', 'provider_id', 'provider_game_id', 'created_at');

CREATE TRIGGER casino_games_deny_delete
    BEFORE DELETE ON casino_games
    FOR EACH ROW EXECUTE FUNCTION catalogue_enforce_immutable_identity();

CREATE TRIGGER casino_games_no_truncate
    BEFORE TRUNCATE ON casino_games
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- ----------------------------------------------------------------------
-- 3. sb_sports
-- ----------------------------------------------------------------------
-- Immutable: id, external_ref, created_at. Mutable: code, name.
-- Deliberately NOT granted: platform-admin write on any sb_* table -
-- there is no admin HTTP write path for these today, and pre-granting a
-- capability with no handler would be an unaudited write surface (ADR
-- 0081 §3.3).

ALTER TABLE sb_sports ENABLE ROW LEVEL SECURITY;
ALTER TABLE sb_sports FORCE ROW LEVEL SECURITY;

CREATE POLICY sb_sports_read ON sb_sports
    FOR SELECT USING (true);

CREATE POLICY sb_sports_catalogue_sync_insert ON sb_sports
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'sportsbook_catalogue_sync'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY sb_sports_catalogue_sync_update ON sb_sports
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'sportsbook_catalogue_sync'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'sportsbook_catalogue_sync'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY sb_sports_catalogue_sync_delete_visibility ON sb_sports
    FOR DELETE
    USING (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'sportsbook_catalogue_sync'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE TRIGGER sb_sports_immutable_identity
    BEFORE UPDATE ON sb_sports
    FOR EACH ROW EXECUTE FUNCTION catalogue_enforce_immutable_identity('id', 'external_ref', 'created_at');

CREATE TRIGGER sb_sports_deny_delete
    BEFORE DELETE ON sb_sports
    FOR EACH ROW EXECUTE FUNCTION catalogue_enforce_immutable_identity();

CREATE TRIGGER sb_sports_no_truncate
    BEFORE TRUNCATE ON sb_sports
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- ----------------------------------------------------------------------
-- 4. sb_competitions
-- ----------------------------------------------------------------------
-- Immutable: id, external_ref, sport_id, created_at. Mutable: name.
-- sport_id is included for structural-integrity reasons one level below
-- sb_events' own market_id/event_id (browse-tree/GetEventDetail join
-- integrity, not money - ADR 0081 §4.2).

ALTER TABLE sb_competitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE sb_competitions FORCE ROW LEVEL SECURITY;

CREATE POLICY sb_competitions_read ON sb_competitions
    FOR SELECT USING (true);

CREATE POLICY sb_competitions_catalogue_sync_insert ON sb_competitions
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'sportsbook_catalogue_sync'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY sb_competitions_catalogue_sync_update ON sb_competitions
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'sportsbook_catalogue_sync'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'sportsbook_catalogue_sync'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY sb_competitions_catalogue_sync_delete_visibility ON sb_competitions
    FOR DELETE
    USING (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'sportsbook_catalogue_sync'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE TRIGGER sb_competitions_immutable_identity
    BEFORE UPDATE ON sb_competitions
    FOR EACH ROW EXECUTE FUNCTION catalogue_enforce_immutable_identity('id', 'external_ref', 'sport_id', 'created_at');

CREATE TRIGGER sb_competitions_deny_delete
    BEFORE DELETE ON sb_competitions
    FOR EACH ROW EXECUTE FUNCTION catalogue_enforce_immutable_identity();

CREATE TRIGGER sb_competitions_no_truncate
    BEFORE TRUNCATE ON sb_competitions
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- ----------------------------------------------------------------------
-- 5. sb_events
-- ----------------------------------------------------------------------
-- Immutable: id, external_ref, competition_id, created_at. Mutable: name,
-- start_time, status. competition_id is the financially load-bearing
-- parent link: sportsbook_bets.selection_id resolves its event through
-- sb_selections -> sb_markets -> sb_events (getSelectionWithContext), so
-- re-parenting an event would silently move an accepted bet's resolution
-- context onto the wrong fixture. A genuine provider re-parent now fails
-- the startup sync loudly rather than corrupting bet resolution silently
-- - an accepted, intentional fail-closed trade (ADR 0081 §4.2).

ALTER TABLE sb_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE sb_events FORCE ROW LEVEL SECURITY;

CREATE POLICY sb_events_read ON sb_events
    FOR SELECT USING (true);

CREATE POLICY sb_events_catalogue_sync_insert ON sb_events
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'sportsbook_catalogue_sync'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY sb_events_catalogue_sync_update ON sb_events
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'sportsbook_catalogue_sync'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'sportsbook_catalogue_sync'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY sb_events_catalogue_sync_delete_visibility ON sb_events
    FOR DELETE
    USING (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'sportsbook_catalogue_sync'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE TRIGGER sb_events_immutable_identity
    BEFORE UPDATE ON sb_events
    FOR EACH ROW EXECUTE FUNCTION catalogue_enforce_immutable_identity('id', 'external_ref', 'competition_id', 'created_at');

CREATE TRIGGER sb_events_deny_delete
    BEFORE DELETE ON sb_events
    FOR EACH ROW EXECUTE FUNCTION catalogue_enforce_immutable_identity();

CREATE TRIGGER sb_events_no_truncate
    BEFORE TRUNCATE ON sb_events
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- ----------------------------------------------------------------------
-- 6. sb_markets
-- ----------------------------------------------------------------------
-- Immutable: id, external_ref, event_id, created_at. Mutable: name,
-- status. event_id is the same financially load-bearing parent-link
-- reasoning as sb_events.competition_id above.

ALTER TABLE sb_markets ENABLE ROW LEVEL SECURITY;
ALTER TABLE sb_markets FORCE ROW LEVEL SECURITY;

CREATE POLICY sb_markets_read ON sb_markets
    FOR SELECT USING (true);

CREATE POLICY sb_markets_catalogue_sync_insert ON sb_markets
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'sportsbook_catalogue_sync'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY sb_markets_catalogue_sync_update ON sb_markets
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'sportsbook_catalogue_sync'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'sportsbook_catalogue_sync'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY sb_markets_catalogue_sync_delete_visibility ON sb_markets
    FOR DELETE
    USING (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'sportsbook_catalogue_sync'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE TRIGGER sb_markets_immutable_identity
    BEFORE UPDATE ON sb_markets
    FOR EACH ROW EXECUTE FUNCTION catalogue_enforce_immutable_identity('id', 'external_ref', 'event_id', 'created_at');

CREATE TRIGGER sb_markets_deny_delete
    BEFORE DELETE ON sb_markets
    FOR EACH ROW EXECUTE FUNCTION catalogue_enforce_immutable_identity();

CREATE TRIGGER sb_markets_no_truncate
    BEFORE TRUNCATE ON sb_markets
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- ----------------------------------------------------------------------
-- 7. sb_selections
-- ----------------------------------------------------------------------
-- Immutable: id, external_ref, market_id, created_at. Mutable: name,
-- odds_numerator, odds_denominator, status - odds are DELIBERATELY left
-- mutable (see this file's header and ADR 0081 §2.5.2): updating live
-- odds is the catalogue sync's entire job, and already-placed bets are
-- protected by the acceptance-time freeze on sportsbook_bets
-- (migration 0082 §1.1), not by freezing the catalogue's current price.
-- market_id is the same financially load-bearing parent-link reasoning as
-- sb_events.competition_id/sb_markets.event_id above - it is the one
-- closest to sportsbook_bets.selection_id's own resolution path.

ALTER TABLE sb_selections ENABLE ROW LEVEL SECURITY;
ALTER TABLE sb_selections FORCE ROW LEVEL SECURITY;

CREATE POLICY sb_selections_read ON sb_selections
    FOR SELECT USING (true);

CREATE POLICY sb_selections_catalogue_sync_insert ON sb_selections
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'sportsbook_catalogue_sync'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY sb_selections_catalogue_sync_update ON sb_selections
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'sportsbook_catalogue_sync'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'sportsbook_catalogue_sync'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY sb_selections_catalogue_sync_delete_visibility ON sb_selections
    FOR DELETE
    USING (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'sportsbook_catalogue_sync'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE TRIGGER sb_selections_immutable_identity
    BEFORE UPDATE ON sb_selections
    FOR EACH ROW EXECUTE FUNCTION catalogue_enforce_immutable_identity('id', 'external_ref', 'market_id', 'created_at');

CREATE TRIGGER sb_selections_deny_delete
    BEFORE DELETE ON sb_selections
    FOR EACH ROW EXECUTE FUNCTION catalogue_enforce_immutable_identity();

CREATE TRIGGER sb_selections_no_truncate
    BEFORE TRUNCATE ON sb_selections
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();
