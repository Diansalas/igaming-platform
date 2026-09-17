-- Reverses migration 0046: drops the threshold-denomination column and
-- its CHECK, and restores migration 0041's immutability trigger function
-- exactly as it was (without the threshold_exponent clause).

ALTER TABLE risk_rules DROP CONSTRAINT risk_rules_threshold_denomination_check;
ALTER TABLE risk_rules DROP COLUMN threshold_exponent;

CREATE OR REPLACE FUNCTION risk_rules_enforce_immutability() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
        RAISE EXCEPTION 'risk_rules is append-only: % is not permitted', TG_OP;
    END IF;
    IF NEW.id <> OLD.id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.brand_id IS DISTINCT FROM OLD.brand_id
        OR NEW.jurisdiction_code IS DISTINCT FROM OLD.jurisdiction_code
        OR NEW.licensing_mode IS DISTINCT FROM OLD.licensing_mode
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
