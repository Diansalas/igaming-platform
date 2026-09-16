-- Reverses 0042: restores the pre-Stage-4G-FINAL immutability function
-- (without licensing_mode in its core-fields check), then drops both
-- added columns. A risk_rules row that already has licensing_mode set is
-- a one-way door in the same sense migration 0041's own down migration
-- documents for the risk_manager staff role: this DROP COLUMN discards
-- that scoping information outright rather than attempting to preserve
-- it in a shape the pre-0042 schema has nowhere to put.

CREATE OR REPLACE FUNCTION risk_rules_enforce_immutability() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
        RAISE EXCEPTION 'risk_rules is append-only: % is not permitted', TG_OP;
    END IF;
    IF NEW.id <> OLD.id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.brand_id IS DISTINCT FROM OLD.brand_id
        OR NEW.jurisdiction_code IS DISTINCT FROM OLD.jurisdiction_code
        OR NEW.player_account_id IS DISTINCT FROM OLD.player_account_id
        OR NEW.product IS DISTINCT FROM OLD.product
        OR NEW.operation <> OLD.operation
        OR NEW.provider_id IS DISTINCT FROM OLD.provider_id
        OR NEW.game_id IS DISTINCT FROM OLD.game_id
        OR NEW.asset_code IS DISTINCT FROM OLD.asset_code
        OR NEW.payment_method IS DISTINCT FROM OLD.payment_method
        OR NEW.limit_kind <> OLD.limit_kind
        OR NEW.time_window <> OLD.time_window
        OR NEW.threshold <> OLD.threshold
        OR NEW.rule_kind <> OLD.rule_kind
        OR NEW.action <> OLD.action
        OR NEW.effective_from <> OLD.effective_from
        OR NEW.created_by_actor_type <> OLD.created_by_actor_type
        OR NEW.created_by_actor_id <> OLD.created_by_actor_id
        OR NEW.created_at <> OLD.created_at
    THEN
        RAISE EXCEPTION 'risk_rules: core rule fields are immutable after creation - only status/description/effective_until may change; a policy change is a new rule';
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP INDEX IF EXISTS idx_risk_rules_licensing_mode;
ALTER TABLE risk_rules DROP COLUMN IF EXISTS licensing_mode;

-- Restore migration 0036's original casino_launch_sessions immutability
-- function verbatim (without jurisdiction_code in its core-fields check)
-- before dropping the column.
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

ALTER TABLE casino_launch_sessions DROP COLUMN IF EXISTS jurisdiction_code;
