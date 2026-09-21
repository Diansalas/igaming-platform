-- Stage 9.2, Part C (docs/decisions/0083-sportsbook-jurisdiction-gating-
-- and-cumulative-exposure.md §5.2, §9.1 items 1-2). Lifts ADR 0047's
-- "MUST FIX BEFORE PRODUCTION/B2B, not now" catalogue/jurisdiction
-- deferral.
--
-- Numbering note: ADR 0083 §5.2.2/§9.1 expected this migration to be
-- "0087"; a parallel Stage 9.2 workstream (casino-catalogue-dual-control)
-- had already claimed 0086 by the time this wave landed, exactly as
-- predicted, so 0087 is in fact the next free number and no renumbering
-- was required (contrast ADR 0081 §6 / migration 0084's own renumbering
-- note, where the expected number WAS already taken).
--
-- Two independent schema changes:
--
--   1. A new, platform-wide, deny-only table, sb_jurisdiction_restrictions
--      (§5.2.2) - NOT a column on any of the five sb_* catalogue tables
--      (§5.2.1 gives five reasons ADR 0047 §3's column prescription is now
--      the wrong answer, chief among them that migration 0084 scoped every
--      sb_* write to the 'sportsbook_catalogue_sync' service principal, so
--      a column there would make a vendor feed the platform's own
--      jurisdiction-blocking authority). This migration adds NO column, NO
--      policy, NO trigger and NO GUC check to casino_games, sb_sports,
--      sb_competitions, sb_events, sb_markets or sb_selections themselves -
--      it only adds foreign keys POINTING AT three of them, and Postgres
--      bypasses RLS for referential-integrity checks (migration 0084's own
--      header, citing ADR 0081 §2.3), so those FKs create no policy
--      interaction whatsoever. Migration 0084/0085's write-authorization
--      model on the six catalogue tables stays closed.
--
--   2. A historical-stability snapshot column, sportsbook_bets.
--      jurisdiction_code (§5.2.4), mirroring casino_launch_sessions.
--      jurisdiction_code (migration 0042) exactly: nullable (NULL means
--      "the resolution did not resolve at placement time", never
--      "unknown"), frozen by extending sportsbook_bets_enforce_immutable_
--      fields' existing first IF (migration 0082) - not replacing that
--      function's other logic.
--
-- RLS/trigger shape for sb_jurisdiction_restrictions is BYTE-IDENTICAL, in
-- predicate structure, to casino_games' own platform-admin write policies
-- (migration 0084 §2) - an authorized human platform admin, never a
-- service principal, never a tenant, never a player. Triggers reuse the
-- EXISTING catalogue_enforce_immutable_identity and ledger_deny_mutation
-- functions (migration 0084 §1) - no new trigger function is created.

-- ----------------------------------------------------------------------
-- 1. sb_jurisdiction_restrictions
-- ----------------------------------------------------------------------

CREATE TABLE sb_jurisdiction_restrictions (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    scope_kind              TEXT NOT NULL CHECK (scope_kind IN ('event','market','selection')),
    event_id                UUID REFERENCES sb_events (id),
    market_id               UUID REFERENCES sb_markets (id),
    selection_id            UUID REFERENCES sb_selections (id),
    jurisdiction_code       TEXT NOT NULL REFERENCES jurisdictions (code),
    -- Deny-only by construction (INV-SB-JUR-2). A single admitted value,
    -- not a free enum: widening this CHECK is an ADR amendment, not a
    -- migration someone can write on a Tuesday.
    restriction_kind        TEXT NOT NULL DEFAULT 'blocked' CHECK (restriction_kind = 'blocked'),
    status                  TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','withdrawn')),
    -- Required on every row (not only on an "enable", because every row
    -- here IS a restriction): ADR 0045 §4's authorization discipline.
    authorization_reference TEXT NOT NULL CHECK (btrim(authorization_reference) <> ''),
    reason_code             TEXT NOT NULL CHECK (btrim(reason_code) <> ''),
    created_by_actor_type   TEXT NOT NULL CHECK (created_by_actor_type = 'staff'),
    created_by_actor_id     UUID NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at              TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (num_nonnulls(event_id, market_id, selection_id) = 1),
    CHECK (
        (scope_kind = 'event'     AND event_id     IS NOT NULL) OR
        (scope_kind = 'market'    AND market_id    IS NOT NULL) OR
        (scope_kind = 'selection' AND selection_id IS NOT NULL)
    )
);

-- One admitted active restriction per (scope row, jurisdiction) at each of
-- the three levels - a withdrawn row does not count, so the same scope/
-- jurisdiction pair can be re-armed after a withdrawal without a unique-
-- index collision.
CREATE UNIQUE INDEX idx_sb_jur_restr_event_open
    ON sb_jurisdiction_restrictions (event_id, jurisdiction_code)
    WHERE status = 'active' AND event_id IS NOT NULL;
CREATE UNIQUE INDEX idx_sb_jur_restr_market_open
    ON sb_jurisdiction_restrictions (market_id, jurisdiction_code)
    WHERE status = 'active' AND market_id IS NOT NULL;
CREATE UNIQUE INDEX idx_sb_jur_restr_selection_open
    ON sb_jurisdiction_restrictions (selection_id, jurisdiction_code)
    WHERE status = 'active' AND selection_id IS NOT NULL;

-- No tenant_id column, deliberately (§5.2.2): the catalogue this table
-- restricts is platform-wide, so a restriction on it is a platform-level
-- statement applying identically to every tenant. A tenant's own
-- narrowing belongs in operating_country_policies, which already has a
-- tenant rung and its own widening-refusal trigger.

ALTER TABLE sb_jurisdiction_restrictions ENABLE ROW LEVEL SECURITY;
ALTER TABLE sb_jurisdiction_restrictions FORCE ROW LEVEL SECURITY;

-- Read-open, byte-identical to casino_games_read (migration 0084 §2) for
-- the identical data class: platform-uniform catalogue-availability
-- policy with no tenant dimension, therefore nothing tenant-confidential
-- to leak between tenants.
CREATE POLICY sb_jurisdiction_restrictions_read ON sb_jurisdiction_restrictions
    FOR SELECT USING (true);

CREATE POLICY sb_jurisdiction_restrictions_platform_admin_insert ON sb_jurisdiction_restrictions
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY sb_jurisdiction_restrictions_platform_admin_update ON sb_jurisdiction_restrictions
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
-- what actually refuses (casino_games_platform_admin_delete_visibility's
-- identical precedent/rationale, migration 0084 §2).
CREATE POLICY sb_jurisdiction_restrictions_platform_admin_delete_visibility ON sb_jurisdiction_restrictions
    FOR DELETE
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- Immutable: id, scope_kind, event_id, market_id, selection_id,
-- jurisdiction_code, restriction_kind, created_at, created_by_actor_type,
-- created_by_actor_id. Mutable: status, reason_code (a restriction is
-- withdrawn, never deleted, so its compliance history survives).
CREATE TRIGGER sb_jurisdiction_restrictions_immutable_identity
    BEFORE UPDATE ON sb_jurisdiction_restrictions
    FOR EACH ROW EXECUTE FUNCTION catalogue_enforce_immutable_identity(
        'id', 'scope_kind', 'event_id', 'market_id', 'selection_id',
        'jurisdiction_code', 'restriction_kind', 'created_at',
        'created_by_actor_type', 'created_by_actor_id');

CREATE TRIGGER sb_jurisdiction_restrictions_deny_delete
    BEFORE DELETE ON sb_jurisdiction_restrictions
    FOR EACH ROW EXECUTE FUNCTION catalogue_enforce_immutable_identity();

CREATE TRIGGER sb_jurisdiction_restrictions_no_truncate
    BEFORE TRUNCATE ON sb_jurisdiction_restrictions
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- ----------------------------------------------------------------------
-- 2. sportsbook_bets.jurisdiction_code - historical-stability snapshot
-- ----------------------------------------------------------------------

ALTER TABLE sportsbook_bets
    ADD COLUMN jurisdiction_code TEXT REFERENCES jurisdictions (code);

-- jurisdiction_code joins this table's own immutability trigger's
-- existing first IF (migration 0082) - extended, not replaced. The
-- provider-reference conditional block (the second IF) is untouched.
CREATE OR REPLACE FUNCTION sportsbook_bets_enforce_immutable_fields() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.brand_id IS DISTINCT FROM OLD.brand_id
        OR NEW.player_account_id IS DISTINCT FROM OLD.player_account_id
        OR NEW.wallet_id IS DISTINCT FROM OLD.wallet_id
        OR NEW.selection_id IS DISTINCT FROM OLD.selection_id
        OR NEW.asset_code IS DISTINCT FROM OLD.asset_code
        OR NEW.stake_amount IS DISTINCT FROM OLD.stake_amount
        OR NEW.odds_numerator IS DISTINCT FROM OLD.odds_numerator
        OR NEW.odds_denominator IS DISTINCT FROM OLD.odds_denominator
        OR NEW.potential_return IS DISTINCT FROM OLD.potential_return
        OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
        OR NEW.ledger_transaction_id IS DISTINCT FROM OLD.ledger_transaction_id
        OR NEW.placed_at IS DISTINCT FROM OLD.placed_at
        OR NEW.jurisdiction_code IS DISTINCT FROM OLD.jurisdiction_code
    THEN
        RAISE EXCEPTION 'sportsbook_bets: identity/stake/odds/idempotency columns are immutable after insert';
    END IF;
    IF OLD.provider_id IS NOT NULL
        AND (NEW.provider_id IS DISTINCT FROM OLD.provider_id
             OR NEW.provider_bet_reference IS DISTINCT FROM OLD.provider_bet_reference)
    THEN
        RAISE EXCEPTION 'sportsbook_bets: the provider reference is immutable once set';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
