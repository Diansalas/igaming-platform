-- Restores the 0123 guard body exactly (tolerates NULL/NULL on INSERT again). Rows
-- inserted while 0126 was applied keep their bindings; nothing is rewritten.

CREATE OR REPLACE FUNCTION withdrawal_requests_payout_binding_guard() RETURNS TRIGGER AS $$
DECLARE
    v_i payout_instruments%ROWTYPE;
    v_ver payout_instrument_verifications%ROWTYPE;
BEGIN
    IF NEW.payout_instrument_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT * INTO v_i FROM payout_instruments i
     WHERE i.id = NEW.payout_instrument_id AND i.tenant_id = NEW.tenant_id FOR SHARE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'withdrawal_requests: payout instrument not found' USING ERRCODE = 'PI040';
    END IF;
    IF v_i.player_account_id <> NEW.player_account_id OR v_i.brand_id <> NEW.brand_id
       OR v_i.fingerprint <> NEW.payout_instrument_fingerprint THEN
        RAISE EXCEPTION 'withdrawal_requests: payout instrument does not belong to this player/brand' USING ERRCODE = 'PI041';
    END IF;
    IF v_i.state <> 'verified' THEN
        RAISE EXCEPTION 'withdrawal_requests: payout instrument is not verified' USING ERRCODE = 'PI042';
    END IF;
    IF NOT (NEW.asset_code = ANY (v_i.asset_codes)) THEN
        RAISE EXCEPTION 'withdrawal_requests: asset is not listed on the payout instrument' USING ERRCODE = 'PI043';
    END IF;
    SELECT * INTO v_ver FROM payout_instrument_verifications v
     WHERE v.instrument_id = v_i.id AND v.tenant_id = v_i.tenant_id AND v.outcome = 'verified'
     ORDER BY v.verified_at DESC, v.id DESC LIMIT 1;
    IF NOT FOUND OR v_ver.id IS DISTINCT FROM v_i.current_verification_id OR v_ver.expires_at <= now() THEN
        RAISE EXCEPTION 'withdrawal_requests: payout instrument has no in-force verification' USING ERRCODE = 'PI044';
    END IF;
    IF EXISTS (SELECT 1 FROM payout_instrument_blocking_events e
                WHERE e.instrument_id = v_i.id AND e.tenant_id = v_i.tenant_id
                  AND (e.event = 'revoke' OR (e.event = 'suspend' AND e.occurred_at > v_ver.verified_at)))
    THEN
        RAISE EXCEPTION 'withdrawal_requests: payout instrument is blocked' USING ERRCODE = 'PI045';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;
