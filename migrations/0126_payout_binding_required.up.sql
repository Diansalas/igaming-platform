-- B13-B (ADR 0111 section 18; ADR 0095 section 44 decisions 1-8): closes the
-- transitional NULL/NULL arm that 0123 tolerated (ADR 0111 15.2 B13A-2). The
-- guard function is REPLACED (CREATE OR REPLACE; the trigger from 0123 stays) so
-- an INSERT into withdrawal_requests without a payout instrument binding is refused
-- (SQLSTATE PI046). Every other check is byte-identical to 0123.
--
-- LEGACY ROWS ARE UNAFFECTED: nothing is updated or deleted, the trigger fires
-- on INSERT only, and the 0123 immutability trigger still forbids adding a
-- binding later. A pre-0126 NULL-binding row stays dispatchable only to a
-- providerkind.Synthetic adapter (Go tiering predicate; ADR 0111 A-11).
--
-- NUMBER: 0126 is a PLACEHOLDER (0124 = HSEC, 0125 = RESOLVE-1); the
-- orchestrator renumbers at merge. The function body does not depend on 0124/0125.

CREATE OR REPLACE FUNCTION withdrawal_requests_payout_binding_guard() RETURNS TRIGGER AS $$
DECLARE
    v_i payout_instruments%ROWTYPE;
    v_ver payout_instrument_verifications%ROWTYPE;
BEGIN
    -- B13-B (ADR 0111 section 18): a NEW withdrawal ALWAYS carries a verified payout
    -- instrument binding, in MOCK as well (owner decision 8: MOCK flexibility is the
    -- verification SOURCE only). Only NULL/NULL is refused here; a HALF binding is
    -- still refused by the both-or-neither CHECK (23514), as in 0123. Rows that
    -- predate this migration are legacy and are NOT touched (no UPDATE/backfill;
    -- the trigger fires on INSERT only).
    IF NEW.payout_instrument_id IS NULL THEN
        IF NEW.payout_instrument_fingerprint IS NULL THEN
            RAISE EXCEPTION 'withdrawal_requests: a payout instrument binding is required' USING ERRCODE = 'PI046';
        END IF;
        RETURN NEW; -- a fingerprint without an instrument: the both-or-neither CHECK refuses it (23514)
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
