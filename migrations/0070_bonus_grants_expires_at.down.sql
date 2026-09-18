DROP INDEX IF EXISTS idx_bonus_grants_expires_at;

CREATE OR REPLACE FUNCTION bonus_grants_enforce_immutable_fields() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.brand_id IS DISTINCT FROM OLD.brand_id
        OR NEW.player_account_id IS DISTINCT FROM OLD.player_account_id
        OR NEW.wallet_id IS DISTINCT FROM OLD.wallet_id
        OR NEW.campaign_id IS DISTINCT FROM OLD.campaign_id
        OR NEW.campaign_version_id IS DISTINCT FROM OLD.campaign_version_id
        OR NEW.offer_id IS DISTINCT FROM OLD.offer_id
        OR NEW.offer_version_id IS DISTINCT FROM OLD.offer_version_id
        OR NEW.asset_code IS DISTINCT FROM OLD.asset_code
        OR NEW.decimal_exponent IS DISTINCT FROM OLD.decimal_exponent
        OR NEW.funding_source IS DISTINCT FROM OLD.funding_source
        OR NEW.fulfillment_destination IS DISTINCT FROM OLD.fulfillment_destination
        OR NEW.fulfillment_owner IS DISTINCT FROM OLD.fulfillment_owner
        OR NEW.trigger_reference IS DISTINCT FROM OLD.trigger_reference
        OR NEW.eligibility_snapshot IS DISTINCT FROM OLD.eligibility_snapshot
        OR NEW.segment_id IS DISTINCT FROM OLD.segment_id
        OR NEW.segment_version_id IS DISTINCT FROM OLD.segment_version_id
        OR NEW.jurisdiction_code IS DISTINCT FROM OLD.jurisdiction_code
        OR NEW.licensing_mode IS DISTINCT FROM OLD.licensing_mode
        OR NEW.parent_operation_id IS DISTINCT FROM OLD.parent_operation_id
        OR NEW.originating_suggestion_id IS DISTINCT FROM OLD.originating_suggestion_id
        OR NEW.created_by_actor_type IS DISTINCT FROM OLD.created_by_actor_type
        OR NEW.created_by_actor_id IS DISTINCT FROM OLD.created_by_actor_id
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'bonus_grants: identity/scope/asset/funding/eligibility-snapshot/idempotency/audit fields are immutable after insert (doc10 T.2)';
    END IF;
    IF OLD.granted_amount IS NOT NULL AND NEW.granted_amount IS DISTINCT FROM OLD.granted_amount THEN
        RAISE EXCEPTION 'bonus_grants: granted_amount is immutable once set (an immutable computation input, not a balance - doc 29 BI-3)';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE bonus_grants DROP COLUMN IF EXISTS expires_at;
