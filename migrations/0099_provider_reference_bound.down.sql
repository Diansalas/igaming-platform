-- Reverses migration 0099 (PROVIDER-REF-BOUND-1). Dropping a CHECK loses
-- no data, so this down is unconditionally reversible. The application-
-- level bound (internal/providerref) is unaffected and keeps rejecting
-- over-bound references at each domain boundary.
ALTER TABLE sb_selections DROP CONSTRAINT IF EXISTS sb_selections_external_ref_ref_bound;
ALTER TABLE sb_markets DROP CONSTRAINT IF EXISTS sb_markets_external_ref_ref_bound;
ALTER TABLE sb_events DROP CONSTRAINT IF EXISTS sb_events_external_ref_ref_bound;
ALTER TABLE sb_competitions DROP CONSTRAINT IF EXISTS sb_competitions_external_ref_ref_bound;
ALTER TABLE sb_sports DROP CONSTRAINT IF EXISTS sb_sports_external_ref_ref_bound;

ALTER TABLE sportsbook_bets
    DROP CONSTRAINT IF EXISTS sportsbook_bets_provider_bet_reference_ref_bound,
    DROP CONSTRAINT IF EXISTS sportsbook_bets_provider_id_ref_bound;

ALTER TABLE casino_callback_rejections
    DROP CONSTRAINT IF EXISTS casino_callback_rejections_asset_code_ref_bound,
    DROP CONSTRAINT IF EXISTS casino_callback_rejections_round_id_ref_bound,
    DROP CONSTRAINT IF EXISTS casino_callback_rejections_original_provider_tx_id_ref_bound,
    DROP CONSTRAINT IF EXISTS casino_callback_rejections_provider_tx_id_ref_bound,
    DROP CONSTRAINT IF EXISTS casino_callback_rejections_provider_id_ref_bound;

ALTER TABLE casino_provider_rounds
    DROP CONSTRAINT IF EXISTS casino_provider_rounds_provider_session_id_ref_bound,
    DROP CONSTRAINT IF EXISTS casino_provider_rounds_provider_round_id_ref_bound,
    DROP CONSTRAINT IF EXISTS casino_provider_rounds_provider_id_ref_bound;

ALTER TABLE casino_launch_sessions
    DROP CONSTRAINT IF EXISTS casino_launch_sessions_provider_game_id_ref_bound,
    DROP CONSTRAINT IF EXISTS casino_launch_sessions_provider_id_ref_bound;

ALTER TABLE casino_games
    DROP CONSTRAINT IF EXISTS casino_games_provider_game_id_ref_bound,
    DROP CONSTRAINT IF EXISTS casino_games_provider_id_ref_bound;

ALTER TABLE kyc_verifications
    DROP CONSTRAINT IF EXISTS kyc_verifications_provider_reference_ref_bound,
    DROP CONSTRAINT IF EXISTS kyc_verifications_provider_id_ref_bound;

ALTER TABLE withdrawal_requests
    DROP CONSTRAINT IF EXISTS withdrawal_requests_provider_reference_ref_bound,
    DROP CONSTRAINT IF EXISTS withdrawal_requests_provider_id_ref_bound;

ALTER TABLE deposit_intents
    DROP CONSTRAINT IF EXISTS deposit_intents_provider_reference_ref_bound,
    DROP CONSTRAINT IF EXISTS deposit_intents_provider_id_ref_bound;

ALTER TABLE ledger_transactions
    DROP CONSTRAINT IF EXISTS ledger_transactions_provider_tx_id_ref_bound,
    DROP CONSTRAINT IF EXISTS ledger_transactions_provider_id_ref_bound;
