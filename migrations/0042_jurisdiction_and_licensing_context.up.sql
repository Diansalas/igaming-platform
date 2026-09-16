-- Stage 4G-FINAL Parts C/D: closes two architectural gaps the Stage 4G
-- completion report explicitly disclosed rather than silently carried
-- forward. See docs/architecture/15-jurisdiction-and-licensing-model.md
-- (extended this stage) for the full design and
-- docs/decisions/0031-risk-and-limits-engine.md §9/§10 for how
-- internal/risk consumes both additions.
--
-- (C) casino_launch_sessions never persisted the jurisdiction resolved at
-- launch time, so a jurisdiction-scoped risk rule was reachable from
-- LaunchGame but never from postBet - a legal/jurisdiction HARD_LIMIT
-- could be evaluated once at launch and then never again for the same
-- session's own subsequent bets. Persisting it at launch (write-once,
-- like every other denormalized-at-launch column on this table -
-- provider_game_id, asset_code) closes that gap without inventing a new
-- per-bet jurisdiction source.
--
-- (D) risk_rules had no way to distinguish "this rule expresses our OWN
-- platform licence's legal ceiling" from "this rule is a general
-- commercial policy that should also bind a future bring-your-own-licence
-- tenant." Without this column, a platform-wide HARD_LIMIT is
-- indistinguishable from a genuinely universal one and would incorrectly
-- bind a BYOL tenant operating under a DIFFERENT licence's own legal
-- regime the moment one exists (directive Stage 4G-FINAL Part D). Mirrors
-- tenants.licensing_model's own two values exactly (migration 0001) -
-- this is not a new taxonomy, it is the existing canonical one made
-- visible to Risk.

ALTER TABLE casino_launch_sessions
    ADD COLUMN jurisdiction_code TEXT REFERENCES jurisdictions (code);

-- jurisdiction_code joins this table's OWN immutability trigger
-- (migration 0036) - specialist review finding: adding a column without
-- also adding it to casino_launch_sessions_enforce_immutable_fields()
-- would leave it uniquely mutable in place among this table's sibling
-- launch-time-denormalized columns (provider_game_id/asset_code/etc, all
-- already frozen), letting a tenant-staff-scoped connection (which has
-- FOR ALL access under the tenant_staff_scope policy) change which
-- jurisdiction-scoped risk rule applies to a still-active session
-- mid-round.
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

ALTER TABLE risk_rules
    ADD COLUMN licensing_mode TEXT CHECK (licensing_mode IN ('under_platform_licence', 'own_licence'));

-- No index on licensing_mode: unlike jurisdiction_code/operation, no
-- query path filters risk_rules by this column directly -
-- listEffectiveRules (internal/risk/policy_service.go) selects by
-- operation alone via idx_risk_rules_operation and filters every other
-- scope dimension, licensing_mode included, in application code
-- (Rule.matches()). Adding an index this stage's own code never uses
-- would be dead schema shipped speculatively - specialist review
-- finding, corrected before this migration's first commit.

-- licensing_mode joins the immutability trigger's core-fields check - a
-- rule's licensing-mode scope is a policy fact like every other scope
-- dimension, never mutable in place (a genuine change is a new rule row,
-- per the trigger's own existing rationale).
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
