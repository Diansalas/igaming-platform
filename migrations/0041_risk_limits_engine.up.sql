-- Stage 4G Part B: the platform's central Risk & Limits engine. See
-- docs/decisions/0031-risk-and-limits-engine.md for full design
-- rationale. ONE reusable rule/policy table with domain-specific
-- dimensions - never a separate limit engine per product (casino,
-- sportsbook, payments, bonus all consult the SAME table through
-- internal/risk.Evaluate).
--
-- Deliberately mirrors player_restrictions' (migration 0037) dual-scope
-- convention: tenant_id NULL means a platform-wide rule; non-NULL means
-- tenant-owned, optionally further narrowed by brand_id/player_account_id.
-- This is a proven, already-specialist-reviewed pattern for "platform-wide
-- vs tenant-owned in one table," reused here rather than inventing a
-- second shape.
--
-- Risk Management is kept a SEPARATE domain from Responsible Gaming
-- (internal/rg, player_restrictions) - this table has no self-exclusion
-- concept and rg.EvaluateEligibility is unchanged and remains the sole
-- authority for self-exclusion (ADR 0031 §1).

-- Stage 4G directive §24: "Risk configuration" is its own authority,
-- separate from Compliance/Finance/Tenant Admin/Platform Admin - a new
-- StaffRole is added rather than reusing an existing one, since no
-- existing role is meant to hold risk-configuration write access.
ALTER TABLE staff_users DROP CONSTRAINT staff_users_role_check;
ALTER TABLE staff_users ADD CONSTRAINT staff_users_role_check
    CHECK (role IN ('platform_admin', 'tenant_admin', 'support', 'compliance', 'finance', 'risk_manager'));

CREATE TABLE risk_rules (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- NULL = platform-wide (visible/enforced in every tenant context).
    -- Non-NULL = owned by, and enforced only within, that tenant.
    tenant_id             UUID,
    brand_id              UUID,
    jurisdiction_code     TEXT REFERENCES jurisdictions (code),
    -- A player-specific override (directive §18) - e.g. "Player X casino
    -- max stake = 50" narrower than "Brand default = 200". Always
    -- tenant-owned when set (a player belongs to exactly one tenant).
    player_account_id     UUID,
    -- Broad product grouping - optional; a rule may instead scope purely
    -- by `operation` (below), which is more specific and always required.
    product               TEXT CHECK (product IN ('casino', 'sportsbook', 'payments', 'bonus')),
    -- The specific action this rule governs - always required, since
    -- "no operation at all" would make a rule impossible to ever match
    -- deterministically against a request. Extensible via an additive
    -- migration as new integration points are wired (directive §21 lists
    -- casino_launch/casino_bet/deposit/withdrawal/sportsbook_bet/
    -- bonus_grant as the designed set - only casino_launch/casino_bet are
    -- actually ENFORCED this stage, see ADR 0031 §7).
    operation             TEXT NOT NULL CHECK (operation IN (
        'casino_launch', 'casino_bet', 'deposit', 'withdrawal', 'sportsbook_bet', 'bonus_grant'
    )),
    provider_id           TEXT,
    game_id               UUID REFERENCES casino_games (id),
    asset_code            TEXT REFERENCES assets (code),
    payment_method        TEXT,
    -- The mathematical shape of the limit - deliberately a SMALL,
    -- fully-implemented set this stage (directive §14: "do not implement
    -- every future rule unnecessarily, prioritize the architecture").
    -- count/velocity/exposure/loss are documented, designed extension
    -- points (ADR 0031 §4) - NOT accepted here, so a rule can never be
    -- configured that the evaluator does not know how to evaluate
    -- (a mis-evaluated rule would be exactly the "malformed rule" fail-
    -- closed case directive §22 warns about; refusing to store one at
    -- all is stronger than evaluating it defensively).
    limit_kind            TEXT NOT NULL CHECK (limit_kind IN ('min_amount', 'max_amount', 'cumulative_amount')),
    -- 'transaction' = this single operation's own amount (the only valid
    -- window for min_amount/max_amount). Every other value is a genuine
    -- rolling window ending now(), UTC - calendar-aligned windows
    -- (calendar_day/calendar_month) are a documented future extension
    -- requiring jurisdiction-configured timezone semantics (ADR 0031 §4),
    -- not implemented this stage.
    time_window           TEXT NOT NULL CHECK (time_window IN (
        'transaction', 'rolling_hour', 'rolling_day', 'rolling_week', 'rolling_month'
    )),
    -- Minor units, matching the asset's own registered exponent - NEVER
    -- floating point (CLAUDE.md's financial rules, applied identically
    -- here even though this table is not itself a ledger table).
    threshold             NUMERIC(38, 0) NOT NULL CHECK (threshold >= 0),
    -- HARD_LIMIT: a non-negotiable ceiling (e.g. a jurisdiction's legal
    -- maximum) - ALL matching hard limits are enforced regardless of
    -- specificity; a breach of ANY denies/reviews, never overridden by a
    -- more specific configurable rule. CONFIGURABLE_LIMIT: an ordinary
    -- commercial limit - the single MOST SPECIFIC matching rule per
    -- (limit_kind, time_window) wins, never "most restrictive wins"
    -- blindly (directive §16). RISK_SIGNAL: contributes to a REVIEW
    -- outcome but never a DENY by itself. See ADR 0031 §5 for the full
    -- precedence algorithm.
    rule_kind             TEXT NOT NULL DEFAULT 'configurable_limit'
                              CHECK (rule_kind IN ('hard_limit', 'configurable_limit', 'risk_signal')),
    -- What happens on breach - RISK_SIGNAL rules are stored with action
    -- 'review' by convention (enforced by the CHECK below) since a signal
    -- alone must never deny.
    action                TEXT NOT NULL DEFAULT 'deny' CHECK (action IN ('deny', 'review')),
    status                TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'disabled')),
    effective_from        TIMESTAMPTZ NOT NULL DEFAULT now(),
    effective_until       TIMESTAMPTZ,
    description           TEXT,
    created_by_actor_type TEXT NOT NULL CHECK (created_by_actor_type IN ('staff', 'system')),
    created_by_actor_id   UUID NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (brand_id IS NULL OR tenant_id IS NOT NULL),
    CHECK (player_account_id IS NULL OR tenant_id IS NOT NULL),
    CHECK (effective_until IS NULL OR effective_until > effective_from),
    CHECK (
        (limit_kind IN ('min_amount', 'max_amount') AND time_window = 'transaction')
        OR (limit_kind = 'cumulative_amount' AND time_window <> 'transaction')
    ),
    CHECK (rule_kind <> 'risk_signal' OR action = 'review'),
    -- Composite FKs so a row can never name a DIFFERENT tenant's brand/
    -- player_account than its own tenant_id - the exact PostgreSQL/RLS
    -- specialist-review fix applied retroactively to Stage 4F (migration
    -- 0040), applied here from the start rather than found later.
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id),
    FOREIGN KEY (player_account_id, tenant_id) REFERENCES player_accounts (id, tenant_id)
);

CREATE INDEX idx_risk_rules_tenant ON risk_rules (tenant_id);
CREATE INDEX idx_risk_rules_operation ON risk_rules (operation, status);
CREATE INDEX idx_risk_rules_player_account ON risk_rules (player_account_id) WHERE player_account_id IS NOT NULL;

ALTER TABLE risk_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE risk_rules FORCE ROW LEVEL SECURITY;

-- READ: a tenant-scoped connection sees its own tenant's rules PLUS every
-- platform-wide rule (tenant_id IS NULL) - identical shape to
-- player_restrictions' staff_and_system_read (migration 0037), which is
-- what actually makes a platform-wide HARD_LIMIT enforceable from inside
-- any tenant's own evaluation. The leading "app.player_account_id IS
-- NULL" conjunct on every policy below is required for the identical
-- reason migration 0037's own staff_and_system_read/staff_insert carry it
-- (internal/db/tenant_rls.go's own WithPlayerScope doc comment): without
-- it, a player-scoped connection (db.WithPlayerScope, which sets BOTH
-- app.tenant_id AND app.player_account_id) would ALSO satisfy this
-- table's plain tenant-match predicate, exposing every other player's
-- risk rules to read and - far worse - letting a compromised or
-- vulnerable player-facing code path read, insert, or disable
-- risk_rules rows at all. risk_rules has no legitimate player-facing
-- access path today (a player never reads or writes their own limits
-- directly), so this conjunct closes that surface outright rather than
-- relying on "no code happens to call WithPlayerScope for this table"
-- (PostgreSQL/RLS specialist review P1 finding, live-database
-- confirmed: a WithPlayerScope connection could insert and update
-- risk_rules before this fix).
CREATE POLICY tenant_and_platform_read ON risk_rules
    FOR SELECT
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            tenant_id IS NULL
            OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        )
    );

-- WRITE: a tenant-scoped connection may only write a rule scoped to ITS
-- OWN tenant; a platform-wide rule (tenant_id NULL) may only be written
-- by a genuinely platform-scoped connection (db.WithoutTenant -
-- app.tenant_id itself unset) - identical precedent to
-- player_restrictions.staff_insert (migration 0037) and
-- PermCasinoCatalogueManage (ADR 0025 §4).
CREATE POLICY tenant_and_platform_write ON risk_rules
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
            OR (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        )
    );

CREATE POLICY tenant_and_platform_update ON risk_rules
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
            OR (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        )
    )
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
            OR (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        )
    );

-- DELETE visibility (scope only, mirroring player_restrictions'
-- staff_and_system_delete_visibility - NOT a grant of mutation).
-- Immutability itself is enforced by the trigger below; without this
-- policy, RLS would make every row invisible to DELETE (no policy =
-- zero visibility for that command), and the DELETE would silently
-- affect 0 rows rather than failing loudly with the trigger's own
-- exception - exactly the "0 rows matched" ambiguity migration 0037's
-- own precedent explains and avoids.
CREATE POLICY tenant_and_platform_delete_visibility ON risk_rules
    FOR DELETE
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
            OR (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        )
    );

-- Append-only for history/explainability (directive §17's "risk
-- decisions must be explainable" extends to the rules that produced
-- them): a rule is DISABLED (status='disabled'), never deleted, and its
-- core scope/threshold fields never change in place - a genuine policy
-- change is a NEW rule row, so a past decision remains explainable
-- against the exact rule that was active when it ran. Only status and
-- description/effective_until may change (a rule which was mistakenly
-- left open-ended can still be given an end date, and pausing was always
-- meant to be reversible).
CREATE FUNCTION risk_rules_enforce_immutability() RETURNS TRIGGER AS $$
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

CREATE TRIGGER risk_rules_immutable_core
    BEFORE UPDATE ON risk_rules
    FOR EACH ROW EXECUTE FUNCTION risk_rules_enforce_immutability();

CREATE TRIGGER risk_rules_deny_delete
    BEFORE DELETE ON risk_rules
    FOR EACH ROW EXECUTE FUNCTION risk_rules_enforce_immutability();

CREATE TRIGGER risk_rules_no_truncate
    BEFORE TRUNCATE ON risk_rules
    FOR EACH STATEMENT EXECUTE FUNCTION risk_rules_enforce_immutability();
