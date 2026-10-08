-- Restores payment_provider_events_guard to its 0101 definition exactly (no data dependency:
-- 0122 added no table or column; a receipt attributed by the repair keeps its attempt_id).
CREATE OR REPLACE FUNCTION payment_provider_events_guard() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.provider_id IS DISTINCT FROM OLD.provider_id
        OR NEW.event_type IS DISTINCT FROM OLD.event_type
        OR NEW.provider_reference IS DISTINCT FROM OLD.provider_reference
        OR NEW.original_provider_reference IS DISTINCT FROM OLD.original_provider_reference
        OR NEW.merchant_reference IS DISTINCT FROM OLD.merchant_reference
        OR NEW.settlement_reference IS DISTINCT FROM OLD.settlement_reference
        OR NEW.outcome IS DISTINCT FROM OLD.outcome
        OR NEW.amount IS DISTINCT FROM OLD.amount
        OR NEW.asset_code IS DISTINCT FROM OLD.asset_code
        OR NEW.cascadable IS DISTINCT FROM OLD.cascadable
        OR NEW.decline_stage IS DISTINCT FROM OLD.decline_stage
        OR NEW.decline_reason IS DISTINCT FROM OLD.decline_reason
        OR NEW.event_fingerprint IS DISTINCT FROM OLD.event_fingerprint
        OR NEW.disposition_at_receipt IS DISTINCT FROM OLD.disposition_at_receipt
        OR NEW.received_at IS DISTINCT FROM OLD.received_at
    THEN
        RAISE EXCEPTION 'payment_provider_events: append-only except attempt_id/resolution/resolved_at (one-shot)';
    END IF;
    IF OLD.attempt_id IS NOT NULL AND NEW.attempt_id IS DISTINCT FROM OLD.attempt_id THEN
        RAISE EXCEPTION 'payment_provider_events: attempt_id is one-shot (NULL -> value)';
    END IF;
    IF OLD.resolution IS NOT NULL AND NEW.resolution IS DISTINCT FROM OLD.resolution THEN
        RAISE EXCEPTION 'payment_provider_events: resolution is one-shot (NULL -> value)';
    END IF;
    IF OLD.resolved_at IS NOT NULL AND NEW.resolved_at IS DISTINCT FROM OLD.resolved_at THEN
        RAISE EXCEPTION 'payment_provider_events: resolved_at is one-shot (NULL -> value)';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
