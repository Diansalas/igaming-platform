-- Stage 9.2, Part B2 / Wave 3 (docs/decisions/0083-sportsbook-jurisdiction-
-- gating-and-cumulative-exposure.md §6.2.3, §9.1 item 3). Lands AFTER Wave
-- 2 (0087, Part C) per that ADR's own §9.4 wave split - both waves edit
-- PlaceBet's composed call order, and the ADR is explicit that landing
-- them concurrently is not fine.
--
-- Numbering note: ADR 0083 §6.2.3/§9.1 expected this migration to land as
-- part of "the next free number"; 0087 was claimed by Wave 2 (see that
-- migration's own numbering note), so 0088 is in fact the next free
-- number.
--
-- One new table, sb_exposure_limits (the cross-player, per-(scope_kind,
-- asset_code) trading-book exposure ceiling - ADR 0083 §6.0/§6.2), plus
-- one supporting partial index on sportsbook_bets for the aggregate's
-- source data (§6.2.3's closing paragraph). This migration touches NO
-- other table - no column, no policy, no trigger and no GUC to
-- casino_games, sb_sports, sb_competitions, sb_events, sb_markets,
-- sb_selections or sb_jurisdiction_restrictions.
--
-- RLS shape is the TWO-POLICY shape of sportsbook_bets itself (migration
-- 0078) - tenant_staff_scope FOR ALL under tenant-only scope, and
-- EXPLICITLY NO player policy at all: a limit is trading-book intelligence
-- with no player-facing read path (mirrors risk_rules' own posture), unlike
-- sb_jurisdiction_restrictions (Wave 2, platform-admin-write, read-open -
-- a different data class entirely, since this table carries a tenant_id
-- and IS tenant-owned commercial configuration, per ADR 0083 §6.2.3's own
-- RLS paragraph).
--
-- Triggers reuse the EXISTING catalogue_enforce_immutable_identity and
-- ledger_deny_mutation functions (migration 0084 §1) - no new trigger
-- function is created, exactly like migration 0087 for
-- sb_jurisdiction_restrictions.

-- ----------------------------------------------------------------------
-- 1. sb_exposure_limits
-- ----------------------------------------------------------------------

CREATE TABLE sb_exposure_limits (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NOT NULL REFERENCES tenants (id),
    -- NULL = every brand in this tenant. Mirrors casino_game_availability's
    -- own (brand_id = $ OR brand_id IS NULL) precedence precedent
    -- (internal/casino/catalogue.go's IsGameAvailable/ListAvailableGames).
    brand_id                  UUID,
    scope_kind                TEXT NOT NULL CHECK (scope_kind IN ('event', 'market', 'selection')),
    asset_code                TEXT NOT NULL REFERENCES assets (code),
    -- NUMERIC(38,0), matching risk_rules.threshold - never BIGINT, never
    -- float (CLAUDE.md: an 18-exponent asset's aggregate exceeds int64).
    -- Compared in Go via *big.Int, reusing internal/risk's
    -- numericToBigInt discipline (a negative NUMERIC exponent is refused,
    -- a positive one is scaled, never truncated).
    max_open_potential_payout NUMERIC(38,0) NOT NULL CHECK (max_open_potential_payout > 0),
    status                    TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    authorization_reference   TEXT NOT NULL CHECK (btrim(authorization_reference) <> ''),
    reason_code               TEXT NOT NULL CHECK (btrim(reason_code) <> ''),
    created_by_actor_type     TEXT NOT NULL CHECK (created_by_actor_type = 'staff'),
    created_by_actor_id       UUID NOT NULL,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id)
);

-- Brand-specific wins over tenant-wide (ADR 0083 §6.2.4 step 1's
-- precedence rule) - enforced structurally by two disjoint partial unique
-- indexes rather than by a single non-partial one, exactly like
-- sb_jurisdiction_restrictions' three-level shape (migration 0087) and
-- casino_game_availability's own tenant/brand precedent.
CREATE UNIQUE INDEX idx_sb_exposure_limits_brand_key
    ON sb_exposure_limits (tenant_id, brand_id, scope_kind, asset_code)
    WHERE status = 'active' AND brand_id IS NOT NULL;
CREATE UNIQUE INDEX idx_sb_exposure_limits_tenant_key
    ON sb_exposure_limits (tenant_id, scope_kind, asset_code)
    WHERE status = 'active' AND brand_id IS NULL;

ALTER TABLE sb_exposure_limits ENABLE ROW LEVEL SECURITY;
ALTER TABLE sb_exposure_limits FORCE ROW LEVEL SECURITY;

-- sportsbook_bets' OWN two-policy shape (migration 0078), not
-- sb_jurisdiction_restrictions' platform-admin/read-open shape: this table
-- IS tenant-owned (carries tenant_id, unlike sb_jurisdiction_restrictions),
-- and a limit is trading-book intelligence with NO player-facing read path
-- at all (ADR 0083 §6.2.3) - deliberately no player_self_scope-equivalent
-- policy exists here.
CREATE POLICY tenant_staff_scope ON sb_exposure_limits
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- Immutable: id, tenant_id, brand_id, scope_kind, asset_code,
-- max_open_potential_payout, created_at, created_by_actor_type,
-- created_by_actor_id. Mutable: status, reason_code, updated_at - a limit
-- is DISABLED and SUPERSEDED (a new row with a new ceiling), never edited
-- in place (ADR 0083 §6.2.3: "max_open_potential_payout itself is
-- immutable - changing a ceiling means a new row").
CREATE TRIGGER sb_exposure_limits_immutable_identity
    BEFORE UPDATE ON sb_exposure_limits
    FOR EACH ROW EXECUTE FUNCTION catalogue_enforce_immutable_identity(
        'id', 'tenant_id', 'brand_id', 'scope_kind', 'asset_code', 'max_open_potential_payout',
        'created_at', 'created_by_actor_type', 'created_by_actor_id');

CREATE TRIGGER sb_exposure_limits_deny_delete
    BEFORE DELETE ON sb_exposure_limits
    FOR EACH ROW EXECUTE FUNCTION catalogue_enforce_immutable_identity();

CREATE TRIGGER sb_exposure_limits_no_truncate
    BEFORE TRUNCATE ON sb_exposure_limits
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- ----------------------------------------------------------------------
-- 2. Supporting index on the aggregate's source table (ADR 0083 §6.2.3's
--    closing paragraph) - partial on status = 'open' so it stays
--    proportional to the OPEN book rather than to all history. Serves the
--    selection-level exposure lookup directly, and the market/event-level
--    lookups as a tenant+asset scan hash-joined to the small selection set
--    idx_sb_selections_market (migration 0078) produces.
-- ----------------------------------------------------------------------

CREATE INDEX idx_sportsbook_bets_open_exposure
    ON sportsbook_bets (tenant_id, asset_code, selection_id)
    WHERE status = 'open';
