-- CAS-REVOKE-CONSUMED-1 (docs/plans/prh2-hardening-round/plan.md §5-A;
-- security's required fix, docs/plans/payment-readiness/rv-prh-i2-casino-
-- security.md "Re-review (FH-7, 2026-09-28)"; ADR 0095 §15.1.5 C4).
--
-- §15.1.5 found this reachable gap: a session the vendor already
-- 'consumed' BEFORE phase C recorded the launch as having failed stays
-- bet-eligible forever, because RevokeLaunchSession's CAS only matches
-- status='active' and migration 0036/0042's
-- casino_launch_sessions_enforce_immutable_fields() trigger forbids EVERY
-- transition out of a terminal status ('consumed', 'expired', 'revoked'),
-- not merely a revival back to 'active'. Widening RevokeLaunchSession's
-- own WHERE clause to include 'consumed' is useless without also widening
-- this trigger, which is exactly what the security review found "blocked
-- at the database level, not implemented".
--
-- This migration replaces ONLY the terminal-status block of that
-- function with the security-specified exception: exactly the single
-- transition consumed -> revoked is allowed, and ONLY when every other
-- column is unchanged (the whole-row equality below, keyed off to_jsonb
-- minus the status key itself, so it also freezes any FUTURE column added
-- to this table without needing a matching migration here). Every other
-- transition out of 'consumed', 'expired' or 'revoked' - including
-- consumed -> active, consumed -> expired, consumed -> consumed (a no-op
-- UPDATE that still touches OLD.status = NEW.status = 'consumed', which
-- is not the allowed pair), and any transition at all out of 'expired' or
-- 'revoked' - still raises exactly as before.
--
-- The column-immutability block above the terminal-status block (0042's
-- own identity/token/expiry freeze, including jurisdiction_code) is kept
-- byte-identical: this migration touches only the terminal-status IF
-- block, per the security spec's own instruction ("keep 0042's column
-- block; replace only the terminal-status block").
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
        IF OLD.status = 'consumed' AND NEW.status = 'revoked'
           AND (to_jsonb(NEW) - 'status') = (to_jsonb(OLD) - 'status') THEN
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'casino_launch_sessions: row is immutable once consumed, expired, or revoked';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
