-- Payment Readiness PRH-REF, PROVIDER-REF-BOUND-1 (security review
-- docs/plans/stage-10.3-planning/10-gate-w2w3-review-security.md R-2;
-- design/evidence note docs/plans/payment-readiness/
-- prh-ref-provider-reference-bound.md).
--
-- One platform-wide bound on provider-supplied references, identical to
-- internal/providerref (applied in application code at each domain's
-- verified boundary, before any write):
--
--   octet_length(col) BETWEEN 1 AND 255   -- bytes, not characters
--   AND col !~ '[\x01-\x1F\x7F-\x9F]'      -- no C0 control, DEL or C1
--                                          -- control (NUL cannot exist
--                                          -- in a PostgreSQL text value)
--
-- UTF-8 validity is the database encoding's own guarantee (UTF8); the
-- application rejects invalid UTF-8 before it ever reaches a query.
-- NULL stays allowed wherever the column is already nullable (a CHECK is
-- satisfied by NULL).
--
-- Why 255 bytes: every UNIQUE/idempotency btree index that contains one of
-- these columns stays far below PostgreSQL's btree tuple limit (~2704
-- bytes on 8 KB pages). The widest composite key is casino_callback_
-- rejections' UNIQUE (tenant_id, provider_id, event_type, provider_tx_id,
-- reason_class): 16 + 255 + ~9 + 255 + ~32 + per-column headers/alignment
-- < 600 bytes. ledger_transactions' (tenant_id, idempotency_key) key
-- embeds a casino reference as "tombstone:<provider_id>:<ref>" - at most
-- 10 + 255 + 1 + 255 = 521 bytes. provider_id is bounded by the same rule
-- in every table whose idempotency index includes it, so the proof holds
-- at the database level, not only through the webhook route's provider_id
-- charset (webhookauth: at most 63 bytes).
--
-- Never truncation: an existing over-bound row makes this migration FAIL,
-- loudly, with a per-column count. It never deletes or rewrites a row.
--
-- NOT VALID + VALIDATE is deliberately NOT used: the migration runner
-- (internal/db/migrate.go) runs each file in ONE transaction, so the
-- ACCESS EXCLUSIVE lock taken by ADD CONSTRAINT is held until commit
-- either way and splitting the validation buys no lock-time reduction.
-- If a production-sized table ever needs it, the split must be two
-- separate migrations (NOT VALID in one, VALIDATE in the next); that is
-- a deployment decision for the first real-provider go-live, recorded in
-- the design note.

-- --- Pre-flight ---------------------------------------------------------
--
-- Every tenant-owned table here carries FORCE ROW LEVEL SECURITY, and the
-- migration connection has no app.tenant_id: a plain count(*) would see
-- ZERO rows and silently pass (migration 0048's lesson). The pre-flight
-- therefore iterates over every tenant (tenants has no RLS) and sets
-- app.tenant_id to it - satisfying each table's staff-scope policy,
-- never bypassing it, never toggling FORCE (migration 0095's pattern).
-- Platform tables (casino_games, sb_*) are readable as-is (FOR SELECT
-- USING (true)). The ADD CONSTRAINT statements below are the second,
-- RLS-independent line: constraint validation scans the physical table.
DO $$
DECLARE
    pred CONSTANT TEXT := '(%1$I IS NOT NULL AND (octet_length(%1$I) NOT BETWEEN 1 AND 255 OR %1$I ~ ''[\x01-\x1F\x7F-\x9F]''))';
    tenant_cols CONSTANT TEXT[][] := ARRAY[
        ['ledger_transactions', 'provider_id'],
        ['ledger_transactions', 'provider_tx_id'],
        ['deposit_intents', 'provider_id'],
        ['deposit_intents', 'provider_reference'],
        ['withdrawal_requests', 'provider_id'],
        ['withdrawal_requests', 'provider_reference'],
        ['kyc_verifications', 'provider_id'],
        ['kyc_verifications', 'provider_reference'],
        ['casino_launch_sessions', 'provider_id'],
        ['casino_launch_sessions', 'provider_game_id'],
        ['casino_provider_rounds', 'provider_id'],
        ['casino_provider_rounds', 'provider_round_id'],
        ['casino_provider_rounds', 'provider_session_id'],
        ['casino_callback_rejections', 'provider_id'],
        ['casino_callback_rejections', 'provider_tx_id'],
        ['casino_callback_rejections', 'original_provider_tx_id'],
        ['casino_callback_rejections', 'round_id'],
        ['casino_callback_rejections', 'asset_code'],
        ['sportsbook_bets', 'provider_id'],
        ['sportsbook_bets', 'provider_bet_reference']
    ];
    platform_cols CONSTANT TEXT[][] := ARRAY[
        ['casino_games', 'provider_id'],
        ['casino_games', 'provider_game_id'],
        ['sb_sports', 'external_ref'],
        ['sb_competitions', 'external_ref'],
        ['sb_events', 'external_ref'],
        ['sb_markets', 'external_ref'],
        ['sb_selections', 'external_ref']
    ];
    tenant_rec RECORD;
    i INT;
    n BIGINT;
    col_total BIGINT;
    grand_total BIGINT := 0;
    report TEXT := '';
BEGIN
    PERFORM set_config('app.player_account_id', '', true);
    FOR i IN 1 .. array_length(tenant_cols, 1) LOOP
        col_total := 0;
        FOR tenant_rec IN SELECT id FROM tenants LOOP
            PERFORM set_config('app.tenant_id', tenant_rec.id::text, true);
            EXECUTE format('SELECT count(*) FROM %I WHERE tenant_id = $1 AND ' || format(pred, tenant_cols[i][2]), tenant_cols[i][1])
               INTO n USING tenant_rec.id;
            col_total := col_total + n;
        END LOOP;
        IF col_total > 0 THEN
            report := report || format(' %s.%s=%s', tenant_cols[i][1], tenant_cols[i][2], col_total);
            grand_total := grand_total + col_total;
        END IF;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);

    FOR i IN 1 .. array_length(platform_cols, 1) LOOP
        EXECUTE format('SELECT count(*) FROM %I WHERE ' || format(pred, platform_cols[i][2]), platform_cols[i][1]) INTO n;
        IF n > 0 THEN
            report := report || format(' %s.%s=%s', platform_cols[i][1], platform_cols[i][2], n);
            grand_total := grand_total + n;
        END IF;
    END LOOP;

    IF grand_total > 0 THEN
        RAISE EXCEPTION 'migration 0099 pre-flight: % existing row(s) violate the provider reference bound (1..255 bytes, no control characters):%. Nothing was changed. These rows are never truncated or deleted automatically: resolve them under a reviewed data-correction procedure, then re-run.', grand_total, report;
    END IF;
END $$;

-- --- Ledger -------------------------------------------------------------
ALTER TABLE ledger_transactions
    ADD CONSTRAINT ledger_transactions_provider_id_ref_bound
        CHECK (octet_length(provider_id) BETWEEN 1 AND 255 AND provider_id !~ '[\x01-\x1F\x7F-\x9F]'),
    ADD CONSTRAINT ledger_transactions_provider_tx_id_ref_bound
        CHECK (octet_length(provider_tx_id) BETWEEN 1 AND 255 AND provider_tx_id !~ '[\x01-\x1F\x7F-\x9F]');

-- --- Payments -----------------------------------------------------------
ALTER TABLE deposit_intents
    ADD CONSTRAINT deposit_intents_provider_id_ref_bound
        CHECK (octet_length(provider_id) BETWEEN 1 AND 255 AND provider_id !~ '[\x01-\x1F\x7F-\x9F]'),
    ADD CONSTRAINT deposit_intents_provider_reference_ref_bound
        CHECK (octet_length(provider_reference) BETWEEN 1 AND 255 AND provider_reference !~ '[\x01-\x1F\x7F-\x9F]');

ALTER TABLE withdrawal_requests
    ADD CONSTRAINT withdrawal_requests_provider_id_ref_bound
        CHECK (octet_length(provider_id) BETWEEN 1 AND 255 AND provider_id !~ '[\x01-\x1F\x7F-\x9F]'),
    ADD CONSTRAINT withdrawal_requests_provider_reference_ref_bound
        CHECK (octet_length(provider_reference) BETWEEN 1 AND 255 AND provider_reference !~ '[\x01-\x1F\x7F-\x9F]');

-- --- KYC ----------------------------------------------------------------
ALTER TABLE kyc_verifications
    ADD CONSTRAINT kyc_verifications_provider_id_ref_bound
        CHECK (octet_length(provider_id) BETWEEN 1 AND 255 AND provider_id !~ '[\x01-\x1F\x7F-\x9F]'),
    ADD CONSTRAINT kyc_verifications_provider_reference_ref_bound
        CHECK (octet_length(provider_reference) BETWEEN 1 AND 255 AND provider_reference !~ '[\x01-\x1F\x7F-\x9F]');

-- --- Casino -------------------------------------------------------------
ALTER TABLE casino_games
    ADD CONSTRAINT casino_games_provider_id_ref_bound
        CHECK (octet_length(provider_id) BETWEEN 1 AND 255 AND provider_id !~ '[\x01-\x1F\x7F-\x9F]'),
    ADD CONSTRAINT casino_games_provider_game_id_ref_bound
        CHECK (octet_length(provider_game_id) BETWEEN 1 AND 255 AND provider_game_id !~ '[\x01-\x1F\x7F-\x9F]');

ALTER TABLE casino_launch_sessions
    ADD CONSTRAINT casino_launch_sessions_provider_id_ref_bound
        CHECK (octet_length(provider_id) BETWEEN 1 AND 255 AND provider_id !~ '[\x01-\x1F\x7F-\x9F]'),
    ADD CONSTRAINT casino_launch_sessions_provider_game_id_ref_bound
        CHECK (octet_length(provider_game_id) BETWEEN 1 AND 255 AND provider_game_id !~ '[\x01-\x1F\x7F-\x9F]');

ALTER TABLE casino_provider_rounds
    ADD CONSTRAINT casino_provider_rounds_provider_id_ref_bound
        CHECK (octet_length(provider_id) BETWEEN 1 AND 255 AND provider_id !~ '[\x01-\x1F\x7F-\x9F]'),
    ADD CONSTRAINT casino_provider_rounds_provider_round_id_ref_bound
        CHECK (octet_length(provider_round_id) BETWEEN 1 AND 255 AND provider_round_id !~ '[\x01-\x1F\x7F-\x9F]'),
    ADD CONSTRAINT casino_provider_rounds_provider_session_id_ref_bound
        CHECK (octet_length(provider_session_id) BETWEEN 1 AND 255 AND provider_session_id !~ '[\x01-\x1F\x7F-\x9F]');

ALTER TABLE casino_callback_rejections
    ADD CONSTRAINT casino_callback_rejections_provider_id_ref_bound
        CHECK (octet_length(provider_id) BETWEEN 1 AND 255 AND provider_id !~ '[\x01-\x1F\x7F-\x9F]'),
    ADD CONSTRAINT casino_callback_rejections_provider_tx_id_ref_bound
        CHECK (octet_length(provider_tx_id) BETWEEN 1 AND 255 AND provider_tx_id !~ '[\x01-\x1F\x7F-\x9F]'),
    ADD CONSTRAINT casino_callback_rejections_original_provider_tx_id_ref_bound
        CHECK (octet_length(original_provider_tx_id) BETWEEN 1 AND 255 AND original_provider_tx_id !~ '[\x01-\x1F\x7F-\x9F]'),
    ADD CONSTRAINT casino_callback_rejections_round_id_ref_bound
        CHECK (octet_length(round_id) BETWEEN 1 AND 255 AND round_id !~ '[\x01-\x1F\x7F-\x9F]'),
    ADD CONSTRAINT casino_callback_rejections_asset_code_ref_bound
        CHECK (octet_length(asset_code) BETWEEN 1 AND 255 AND asset_code !~ '[\x01-\x1F\x7F-\x9F]');

-- --- Sportsbook ---------------------------------------------------------
ALTER TABLE sportsbook_bets
    ADD CONSTRAINT sportsbook_bets_provider_id_ref_bound
        CHECK (octet_length(provider_id) BETWEEN 1 AND 255 AND provider_id !~ '[\x01-\x1F\x7F-\x9F]'),
    ADD CONSTRAINT sportsbook_bets_provider_bet_reference_ref_bound
        CHECK (octet_length(provider_bet_reference) BETWEEN 1 AND 255 AND provider_bet_reference !~ '[\x01-\x1F\x7F-\x9F]');

ALTER TABLE sb_sports
    ADD CONSTRAINT sb_sports_external_ref_ref_bound
        CHECK (octet_length(external_ref) BETWEEN 1 AND 255 AND external_ref !~ '[\x01-\x1F\x7F-\x9F]');
ALTER TABLE sb_competitions
    ADD CONSTRAINT sb_competitions_external_ref_ref_bound
        CHECK (octet_length(external_ref) BETWEEN 1 AND 255 AND external_ref !~ '[\x01-\x1F\x7F-\x9F]');
ALTER TABLE sb_events
    ADD CONSTRAINT sb_events_external_ref_ref_bound
        CHECK (octet_length(external_ref) BETWEEN 1 AND 255 AND external_ref !~ '[\x01-\x1F\x7F-\x9F]');
ALTER TABLE sb_markets
    ADD CONSTRAINT sb_markets_external_ref_ref_bound
        CHECK (octet_length(external_ref) BETWEEN 1 AND 255 AND external_ref !~ '[\x01-\x1F\x7F-\x9F]');
ALTER TABLE sb_selections
    ADD CONSTRAINT sb_selections_external_ref_ref_bound
        CHECK (octet_length(external_ref) BETWEEN 1 AND 255 AND external_ref !~ '[\x01-\x1F\x7F-\x9F]');
