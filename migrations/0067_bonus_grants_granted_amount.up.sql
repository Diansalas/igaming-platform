-- Closes a real, disclosed gap found by Stage 4H-B1 Wave 2's own fix
-- dispatch DR-4HB1W2-02 (commit af4f9cd) and confirmed by Phase 11's final
-- ledger-finance certification: internal/economicop.ConsumeRootBudget's
-- value-budget branch (enforce.go) hardcodes "granted_amount" as the
-- column it sums over a Grant EOI's consumption-record table
-- (consumptionShapes[OperationBonusManualGrant] =
-- consumptionShapes[OperationAPIInitiatedGrant] = "bonus_grants") - a
-- column bonus_grants never actually had (migration 0057). This is a
-- LIVE bug, not a hypothetical: any root EconomicOperation minted for
-- OperationBonusManualGrant/OperationAPIInitiatedGrant with a non-null
-- intended_aggregate_value (an ordinary, supported four-eyes-approved
-- value-budget cap, doc 34 §3.2) makes every subsequent
-- IssueSingleManualGrant against that root fail at the database with
-- "column c.granted_amount does not exist" the instant ConsumeRootBudget's
-- value-budget query runs - the recipient-ceiling half of the same check
-- already works today (DR-4HB1W2-02 fixed that half); this migration
-- closes the value-budget half.
--
-- Docs already named and specified this exact column three times, before
-- any migration ever created it: docs/architecture/10-bonus-engine-
-- architecture.md ("valued by granted_amount (value budget)"),
-- docs/architecture/29-bonus-implementation-contract.md's BI-3 ("no
-- bonus_* table stores a balance... bonus_grants.granted_amount is an
-- immutable computation input, not a balance - the distinguishing test is
-- whether any code path ever UPDATEs it"), docs/decisions/0034-economic-
-- operation-identity.md §N2.4a. This migration is not a new design - it
-- completes an already-specified column the schema was simply missing,
-- mirroring bulk_grant_job_items.granted_amount's own precedent (migration
-- 0060) type-for-type: NUMERIC(38,0), never int64/float64/BIGINT cents
-- (CLAUDE.md's money rule), non-negative, nullable (NULL until the Grant
-- actually activates - a Grant the T.1 gate cancels before ever posting
-- never gets one, exactly mirroring bulk_grant_job_items' own "NULL until
-- the item actually issues a Grant").
--
-- granted_amount is an IMMUTABLE COMPUTATION INPUT, never a balance
-- (doc 29 BI-3's own distinguishing test, restated here as the actual DB
-- rule rather than left as a documentation-only promise): it is written
-- EXACTLY ONCE, by ActivateGrant, in the same transaction as the Grant's
-- sole bonus_grant ledger posting (lifecycle.go) - the one point in the
-- Grant lifecycle where the Grant's face value is decided - and never
-- again. The immutable-fields trigger (migration 0057) is extended, not
-- replaced, to reject any attempted change to a once-set value, the same
-- fail-closed-by-construction posture every other T.2-frozen field on
-- this table already has; setting it from NULL to a value (the one
-- legitimate write) is deliberately still permitted, which is why this
-- column could not simply be added to migration 0057's existing NEW/OLD
-- equality list (every field on that list is frozen from INSERT, this one
-- is frozen from its own first UPDATE instead).
ALTER TABLE bonus_grants
    ADD COLUMN granted_amount NUMERIC(38, 0) CHECK (granted_amount IS NULL OR granted_amount >= 0);

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
