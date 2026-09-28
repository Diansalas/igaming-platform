-- Reverses migration 0108: restores 0042's casino_launch_sessions_enforce_
-- immutable_fields() body verbatim (every transition out of 'consumed',
-- 'expired' or 'revoked' raises unconditionally again, including
-- consumed -> revoked). Any row already sitting in 'revoked' as a result
-- of a consumed -> revoked transition made possible by 0108 is left in
-- place (append-only history, CLAUDE.md) - restoring the stricter trigger
-- does not re-validate historical rows, it only re-forbids the transition
-- going forward.
CREATE OR REPLACE FUNCTION casino_launch_sessions_enforce_immutable_fields() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.brand_id IS DISTINCT FROM OLD.brand_id
        OR NEW.player_account_id IS DISTINCT FROM OLD.player_account_id
        OR NEW.wallet_id IS DISTINCT FROM OLD.wallet_id
        OR NEW.game_id IS DISTINCT FROM OLD.game_id
        OR NEW.provider_id IS DISTINCT FROM OLD.provider_id
        OR NEW.provider_game_id IS DISTINCT FROM OLD.provider_game_id
        OR NEW.asset_code IS DISTINCT FROM OLD.asset_code
        OR NEW.jurisdiction_code IS DISTINCT FROM OLD.jurisdiction_code
        OR NEW.mode IS DISTINCT FROM OLD.mode
        OR NEW.token_hash IS DISTINCT FROM OLD.token_hash
        OR NEW.expires_at IS DISTINCT FROM OLD.expires_at
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'casino_launch_sessions: identity/token/expiry columns are immutable after insert';
    END IF;
    IF OLD.status IN ('consumed', 'expired', 'revoked') THEN
        RAISE EXCEPTION 'casino_launch_sessions: row is immutable once consumed, expired, or revoked';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
