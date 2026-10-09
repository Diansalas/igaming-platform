-- Reverses 0123 (B13). Drops only what 0123 created and restores the 0026 body of
-- withdrawal_requests_enforce_immutable_fields() byte-for-byte.
-- Refuses if any payout instrument or snapshot row exists: they are evidence
-- and are never deleted by a down migration (escalate instead).
DO $$
DECLARE
    tenant_rec RECORD;
    n BIGINT;
    total BIGINT := 0;
BEGIN
    -- FORCE RLS: iterate tenants (the 0099/0115 pattern).
    PERFORM set_config('app.player_account_id', '', true);
    PERFORM set_config('app.platform_admin_principal_id', '', true);
    FOR tenant_rec IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.tenant_id', tenant_rec.id::text, true);
        SELECT count(*) INTO n FROM payout_instruments WHERE tenant_id = tenant_rec.id;
        total := total + n;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
    -- F-7: the max-age configuration is owner-written governance data with no default
    -- content; dropping a non-empty table would silently discard a decision.
    SELECT count(*) INTO n FROM payout_instrument_verification_max_age;
    IF n > 0 THEN
        RAISE EXCEPTION 'migration 0123 down: % payout_instrument_verification_max_age row(s) exist; they are governance configuration and are not discarded (escalate to the human)', n
            USING ERRCODE = 'PI099';
    END IF;
    IF total > 0 THEN
        RAISE EXCEPTION 'migration 0123 down: % payout_instruments row(s) exist; they are evidence and are not deleted (escalate to the human)', total
            USING ERRCODE = 'PI099';
    END IF;
END $$;

DROP TRIGGER IF EXISTS payment_attempts_require_destination_snapshot ON payment_attempts;
DROP FUNCTION IF EXISTS payment_attempts_require_destination_snapshot();
DROP TABLE IF EXISTS payout_attempt_destination_snapshots;
DROP FUNCTION IF EXISTS payout_attempt_destination_snapshots_before_insert();

DROP TRIGGER IF EXISTS withdrawal_requests_payout_binding_guard ON withdrawal_requests;
DROP FUNCTION IF EXISTS withdrawal_requests_payout_binding_guard();

CREATE OR REPLACE FUNCTION withdrawal_requests_enforce_immutable_fields() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.brand_id IS DISTINCT FROM OLD.brand_id
        OR NEW.player_account_id IS DISTINCT FROM OLD.player_account_id
        OR NEW.wallet_id IS DISTINCT FROM OLD.wallet_id
        OR NEW.asset_code IS DISTINCT FROM OLD.asset_code
        OR NEW.amount IS DISTINCT FROM OLD.amount
        OR NEW.idempotency_key IS DISTINCT FROM OLD.idempotency_key
        OR NEW.requested_at IS DISTINCT FROM OLD.requested_at
    THEN
        RAISE EXCEPTION 'withdrawal_requests: amount/asset/wallet/player/tenant/brand/idempotency_key/requested_at are immutable after insert';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP INDEX IF EXISTS idx_withdrawal_requests_payout_instrument;
ALTER TABLE withdrawal_requests
    DROP CONSTRAINT IF EXISTS withdrawal_requests_payout_instrument_fk,
    DROP CONSTRAINT IF EXISTS withdrawal_requests_payout_binding_both_or_neither,
    DROP COLUMN IF EXISTS payout_instrument_fingerprint,
    DROP COLUMN IF EXISTS payout_instrument_id;

ALTER TABLE payout_instruments DROP CONSTRAINT IF EXISTS payout_instruments_current_verification_fk;
DROP TABLE IF EXISTS payout_instrument_blocking_events;
DROP FUNCTION IF EXISTS payout_instrument_blocking_events_before_insert();
DROP TABLE IF EXISTS payout_instrument_verifications;
DROP FUNCTION IF EXISTS payout_instrument_verifications_before_insert();
DROP TABLE IF EXISTS payout_instruments;
DROP FUNCTION IF EXISTS payout_instruments_before_update();
DROP FUNCTION IF EXISTS payout_instruments_before_insert();
DROP TABLE IF EXISTS payout_instrument_fingerprint_owners;
DROP TABLE IF EXISTS payout_instrument_verification_max_age;
DROP TABLE IF EXISTS payout_instrument_kinds;
