DROP INDEX IF EXISTS idx_sportsbook_bets_tenant_provider_reference;
ALTER TABLE sportsbook_bets DROP CONSTRAINT IF EXISTS sportsbook_bets_provider_reference_symmetric_null;
ALTER TABLE sportsbook_bets DROP COLUMN IF EXISTS provider_bet_reference;
ALTER TABLE sportsbook_bets DROP COLUMN IF EXISTS provider_id;
