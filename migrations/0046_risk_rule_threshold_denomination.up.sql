-- Stage 4H-B0-R6 Workstream D (risk fail-closed hardening): records which
-- asset exponent an amount-shaped risk rule's threshold is denominated in.
-- See docs/decisions/0031-risk-and-limits-engine.md §35 for the full
-- decision; §32(d) is the gap it closes.
--
-- The gap: risk_rules.threshold is NUMERIC(38,0) minor units, but a rule
-- with asset_code IS NULL means "applies regardless of asset" - so the
-- same numeric threshold silently meant a different real-world cap at
-- every asset exponent. A cap authored against an 18-exponent crypto
-- asset is effectively unlimited once read against a 2-exponent fiat
-- asset (fail-OPEN); the reverse direction denies every ordinary amount.
--
-- Deliberately NOT solved by scaling thresholds between exponents:
-- "N major units of asset A" is not a comparable limit to "N major units
-- of asset B" (decimal precision says nothing about value - the identical
-- design ledger-finance already rejected in withdrawal.defaultApproval
-- Policy), and a value-equivalent normalization would need FX data that
-- must never be consulted on an evaluation path.
--
-- The rule instead declares its denomination exactly once:
--   * asset_code IS NOT NULL  -> the SCOPE is the denomination; the
--     exponent is looked up from the `assets` registry at evaluation time
--     and is never copied onto this row (one source of exponent truth,
--     per CLAUDE.md). threshold_exponent MUST be NULL.
--   * asset_code IS NULL      -> threshold_exponent is REQUIRED and says
--     which exponent's minor units the threshold is expressed in. The
--     rule binds every asset of that exponent (one rule still covers
--     EUR/USD/GBP/BRL/MXN) and FAILS CLOSED for an asset of any other
--     exponent, rather than silently re-denominating or silently going
--     inert.

ALTER TABLE risk_rules
    ADD COLUMN threshold_exponent SMALLINT
        CHECK (threshold_exponent IS NULL OR threshold_exponent BETWEEN 0 AND 18);

COMMENT ON COLUMN risk_rules.threshold_exponent IS
    'Decimal exponent the threshold''s minor units are expressed in, for an asset-agnostic (asset_code IS NULL) amount rule. NULL for an asset-scoped rule, whose denomination is its own asset_code, resolved from the assets registry at evaluation time. See ADR 0031 §35.';

-- Exactly one denomination source for an amount-shaped rule: either the
-- rule is asset-scoped, or it states its exponent - never both, never
-- neither. The limit_kind guard keeps this correct if a non-amount limit
-- kind (count/velocity - ADR 0031 §4/§12) is ever added, since such a
-- rule needs no denomination at all.
--
-- NOT VALID is deliberate (ADR 0031 §35): rows created before this
-- migration may be asset-agnostic amount rules whose author's intended
-- denomination is genuinely unknown, and a migration must not guess it.
-- New and updated rows are fully checked; pre-existing ambiguous rows are
-- left as data and are refused by the evaluator itself
-- (risk.ErrMissingThresholdDenomination - fail-closed, so such a rule
-- denies its operation until it is re-authored with an explicit
-- denomination; risk_rules is append-only, so "re-authored" means a new
-- row plus disabling the old one).
ALTER TABLE risk_rules
    ADD CONSTRAINT risk_rules_threshold_denomination_check
    CHECK (
        limit_kind NOT IN ('min_amount', 'max_amount', 'cumulative_amount')
        OR ((asset_code IS NULL) <> (threshold_exponent IS NULL))
    ) NOT VALID;

-- threshold_exponent is a core rule field: like every other scope/
-- threshold column it can never change in place (migration 0041's
-- append-only contract - a policy change is a NEW rule row, so a past
-- RiskDecision stays explainable against the exact rule that produced
-- it). Re-declared in full rather than patched, since PostgreSQL has no
-- "add a condition to an existing function" operation - which means this
-- copy must carry forward EVERY field earlier migrations added to the
-- list, including migration 0042's licensing_mode (an earlier draft of
-- this migration dropped it, and internal/risk's own
-- TestRiskRules_CoreFieldsAreImmutableAndAppendOnly caught the
-- regression immediately; the down migration restores exactly 0042's
-- version).
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
        OR NEW.threshold_exponent IS DISTINCT FROM OLD.threshold_exponent
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
