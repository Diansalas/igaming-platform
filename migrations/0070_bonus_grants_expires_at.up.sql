-- bonus_grants.expires_at (Stage 4H-B1 Wave 3 Phase 2, backend) - closes
-- docs/governance/wave-3-reconnaissance.md's gap-list item 7, verbatim:
-- "there is no field to expire against, not merely no sweep job" -
-- TerminateGrant's expired path (doc 10 N1.4) already exists as a
-- mechanism, but nothing on bonus_grants records WHEN a Grant expires,
-- so no sweep could ever call it. This migration adds only the column
-- and its structural invariants - no derivation logic, no sweep, no
-- caller. §7.18's own dependency map assigns computing/populating this
-- value (from the Offer's wagering_time_limit/payout_time_limit at
-- issuance or activation, doc 10's "common object contract" precedent)
-- and the sweep job itself to Phase 3 (bonus-engine), with ledger-finance
-- co-review named since it is a value-reducing transition, the same
-- posture as existing forfeiture - none of that is built here.
--
-- Nullable at creation (a Grant whose Offer sets no wagering_time_limit/
-- payout_time_limit legitimately never expires) and, once set,
-- WRITE-ONCE / IMMUTABLE - mirroring granted_amount's own precedent
-- (migration 0067) exactly, chosen deliberately as the safer default:
-- widening a write-once column to allow revision later (e.g. a future
-- four-eyes "extend Grant expiry" operation, which would need its own
-- ChangeOperation/audit trail, not a bare UPDATE) is a trivial additive
-- migration; starting mutable and retrofitting immutability onto a table
-- that may already hold operator-edited rows is not. If Phase 3/
-- architect decide expiry must be extendable, that is a follow-up
-- migration replacing this trigger clause, not a reason to leave this
-- column unconstrained now.
ALTER TABLE bonus_grants
    ADD COLUMN expires_at TIMESTAMPTZ CHECK (expires_at IS NULL OR expires_at > created_at);

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
    IF OLD.expires_at IS NOT NULL AND NEW.expires_at IS DISTINCT FROM OLD.expires_at THEN
        RAISE EXCEPTION 'bonus_grants: expires_at is immutable once set (frozen once decided, doc10 T.2 posture - see migration 0070)';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- The expiry sweep's own read pattern (§7.18's dependency map: "a
-- sweep... calls TerminateGrant" for every Grant whose expires_at has
-- passed). Partial on IS NOT NULL since most Grants may never expire.
CREATE INDEX idx_bonus_grants_expires_at ON bonus_grants (tenant_id, expires_at) WHERE expires_at IS NOT NULL;
