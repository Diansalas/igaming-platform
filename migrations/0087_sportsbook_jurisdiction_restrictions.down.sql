-- Clean inverse of 0087_sportsbook_jurisdiction_restrictions.up.sql,
-- reverse order of creation.

-- ----------------------------------------------------------------------
-- 2. sportsbook_bets.jurisdiction_code
-- ----------------------------------------------------------------------

-- Restore the pre-0087 function body exactly as migration 0082 created it
-- (without the jurisdiction_code conjunct) BEFORE dropping the column, so
-- an in-flight statement never briefly references a column about to
-- disappear.
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

ALTER TABLE sportsbook_bets DROP COLUMN IF EXISTS jurisdiction_code;

-- ----------------------------------------------------------------------
-- 1. sb_jurisdiction_restrictions
-- ----------------------------------------------------------------------

DROP TRIGGER IF EXISTS sb_jurisdiction_restrictions_no_truncate ON sb_jurisdiction_restrictions;
DROP TRIGGER IF EXISTS sb_jurisdiction_restrictions_deny_delete ON sb_jurisdiction_restrictions;
DROP TRIGGER IF EXISTS sb_jurisdiction_restrictions_immutable_identity ON sb_jurisdiction_restrictions;

DROP POLICY IF EXISTS sb_jurisdiction_restrictions_platform_admin_delete_visibility ON sb_jurisdiction_restrictions;
DROP POLICY IF EXISTS sb_jurisdiction_restrictions_platform_admin_update ON sb_jurisdiction_restrictions;
DROP POLICY IF EXISTS sb_jurisdiction_restrictions_platform_admin_insert ON sb_jurisdiction_restrictions;
DROP POLICY IF EXISTS sb_jurisdiction_restrictions_read ON sb_jurisdiction_restrictions;

ALTER TABLE sb_jurisdiction_restrictions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE sb_jurisdiction_restrictions DISABLE ROW LEVEL SECURITY;

DROP TABLE IF EXISTS sb_jurisdiction_restrictions;
