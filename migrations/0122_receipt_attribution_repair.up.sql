-- PAY-RECEIPT-ANOMALY-APPLIED-1 Option A (owner decisions 24-30, ADR 0095 s44; ADR 0095 s45).
--
-- The ONLY change: payment_provider_events_guard (0101) gains one rule for the
-- post-resolution NULL -> value write of attempt_id. Everything else in the guard is
-- byte-identical to 0101 (the immutable evidence columns, the one-shot resolution and
-- resolved_at, no DELETE, no TRUNCATE are all unchanged).
--
-- Before: attempt_id could go NULL -> any value, once, even after the receipt was resolved
-- (the 0101 guard only refused a change of an already non-NULL value). The target was not
-- checked at all (no tenant, provider or reference check; the FK is on the id alone).
--
-- After: when a RESOLVED receipt with a NULL attempt_id is attributed, the database itself
-- requires that
--   (1) the receipt was closed as an anomaly (resolution anomaly_*): a genuinely applied
--       receipt, or one resolved by any other path, can never be re-attributed;
--   (2) the target attempt is in the SAME tenant and the SAME provider as the receipt;
--   (3) the target attempt carries the receipt's OWN provider_reference, and, when the
--       receipt carries a merchant_reference, that merchant_reference too (so the target is
--       derived from the receipt's immutable evidence, never a caller-chosen attempt);
--   (4) the receipt's event type matches the attempt's operation (deposit/payout).
-- resolution and resolved_at stay one-shot, so the original anomaly label is preserved: the
-- repair is recorded as an append-only audit fact, not as a relabelling. An UPDATE that
-- attributes a receipt before it is resolved is unchanged (ResolveReceipt sets attempt_id,
-- resolution and resolved_at together while resolved_at is still NULL).
--
-- No table, no column, no grant, no policy is added or changed. The guard is not SECURITY
-- DEFINER: the target is read under the caller's own RLS scope, so a target in another
-- tenant is invisible and therefore refused.
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
    -- PAY-RECEIPT-ANOMALY-APPLIED-1: the narrow post-resolution attribution.
    IF OLD.resolved_at IS NOT NULL AND OLD.attempt_id IS NULL AND NEW.attempt_id IS NOT NULL THEN
        IF OLD.resolution IS NULL OR left(OLD.resolution, 8) <> 'anomaly_' THEN
            RAISE EXCEPTION 'payment_provider_events: only an anomaly-closed receipt may be attributed after resolution';
        END IF;
        IF NOT EXISTS (
            SELECT 1 FROM payment_attempts a
             WHERE a.id = NEW.attempt_id
               AND a.tenant_id = OLD.tenant_id
               AND a.provider_id = OLD.provider_id
               AND a.provider_reference = OLD.provider_reference
               AND (OLD.merchant_reference IS NULL OR a.merchant_reference = OLD.merchant_reference)
               AND a.operation = CASE OLD.event_type WHEN 'deposit' THEN 'deposit' WHEN 'payout' THEN 'payout' END
        ) THEN
            RAISE EXCEPTION 'payment_provider_events: the attributed attempt must match the receipt evidence (tenant, provider, references, operation)';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
