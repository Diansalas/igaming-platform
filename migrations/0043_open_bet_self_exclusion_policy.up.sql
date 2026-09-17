-- Stage 4H-B0-R6, Workstream E (identity-compliance): implements ADR 0034
-- §14's OpenBetSelfExclusionPolicy architecture and closes security
-- findings S-8 and S-9 (docs/security/security-architecture.md, Stage
-- 4H-B0-R5 section) against real schema/code, per the directive's own
-- naming reconciliation: this migration uses 'VOID_ON_SELF_EXCLUSION',
-- the value name ADR 0034 §14.1 actually adopted.
--
-- WHAT THIS MIGRATION DOES NOT DO, on purpose: it does not select the
-- platform-wide DEFAULT policy value (SETTLE_NORMALLY vs
-- VOID_ON_SELF_EXCLUSION) for a jurisdiction/tenant/brand that has
-- configured nothing - ADR 0034 §14.9 leaves that an explicit human/legal
-- decision, and this migration enforces the opposite: an absent row is a
-- FAIL-CLOSED state a caller must detect and act on (deny/refuse), never
-- an implicit default resolved in SQL or Go. There is also no
-- `open_bet`/sportsbook table anywhere below - sportsbook does not exist
-- yet (ADR 0034 §14, Consequences addendum); this migration is the
-- general policy-configuration/resolution primitive a future sportsbook
-- implementation will call, not a stub of sportsbook itself.

-- ---------------------------------------------------------------------
-- 1. open_bet_self_exclusion_policies - the scoped, versioned,
--    auditable configuration table ADR 0034 §14.8 specifies.
-- ---------------------------------------------------------------------
--
-- Scope shape mirrors risk_rules' (migration 0041) proven
-- platform-wide-vs-tenant/brand-owned pattern: tenant_id NULL means this
-- row IS the jurisdiction's own floor (ADR 0034 §14.2); tenant_id set
-- (brand_id optionally further narrowing it) means a tenant/brand
-- override of that floor, permitted to TIGHTEN only, never loosen.
--
-- Versioned by insertion, mirroring risk_rules' own append-only-plus-
-- effective_until pattern (migration 0041) rather than an UPDATE-in-place
-- model: a "policy change" is always a NEW row; the previous row for the
-- exact same scope is closed by setting ITS effective_until to the new
-- row's effective_from (the one permitted mutation, exactly like
-- risk_rules allows effective_until to change post-creation). The row's
-- own `id` doubles as ADR 0034 §14.8's "stable version identifier" -
-- already unique and immutable, so no separate incrementing sequence is
-- introduced (ADR 0021's own versioned-rounding-rule precedent uses its
-- row id the same way).
CREATE TABLE open_bet_self_exclusion_policies (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    jurisdiction_code     TEXT NOT NULL REFERENCES jurisdictions (code),
    -- NULL = this row IS the jurisdiction floor (ADR 0034 §14.2).
    tenant_id             UUID REFERENCES tenants (id),
    brand_id              UUID,
    policy_value          TEXT NOT NULL CHECK (policy_value IN ('SETTLE_NORMALLY', 'VOID_ON_SELF_EXCLUSION')),
    -- Authoritative DB time (directive item 4): the actual wall-clock
    -- instant this version was written, re-evaluated at insert time
    -- (clock_timestamp(), never now()/transaction_timestamp()) so that a
    -- write queued behind another writer's advisory lock (see
    -- internal/rg's lockOpenBetSelfExclusionPolicyScope) is stamped with
    -- the instant it ACTUALLY committed, not the instant its own
    -- transaction happened to begin - the identical reasoning
    -- internal/rg.EvaluateEligibility's own clock_timestamp() fix
    -- (Stage 4G-FINAL) already established for this exact
    -- lock-queuing hazard. Callers may instead supply an explicit
    -- effective_from for a scheduled/backdated regulatory change; the
    -- DEFAULT only applies when they do not.
    effective_from        TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    effective_to          TIMESTAMPTZ,
    reason_code           TEXT NOT NULL,
    created_by_actor_type TEXT NOT NULL CHECK (created_by_actor_type IN ('staff', 'system')),
    created_by_actor_id   UUID NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CHECK (brand_id IS NULL OR tenant_id IS NOT NULL),
    CHECK (effective_to IS NULL OR effective_to > effective_from),
    -- Composite FK so a row can never name a different tenant's brand
    -- than its own tenant_id (migration 0041/0040's own precedent).
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id)
);

CREATE INDEX idx_open_bet_self_exclusion_policies_scope
    ON open_bet_self_exclusion_policies (jurisdiction_code, tenant_id, brand_id, effective_from);

-- ---------------------------------------------------------------------
-- Single source of truth for the two-valued strictness ordering ADR 0034
-- §14.2 defines (VOID_ON_SELF_EXCLUSION >= SETTLE_NORMALLY). Used by the
-- tighten-only trigger below. internal/rg mirrors this exact mapping in
-- Go (openBetSelfExclusionPolicyStrictness) - see
-- TestOpenBetSelfExclusionPolicyStrictness_MatchesDatabase for the parity
-- check that keeps the two definitions from silently drifting apart.
-- ---------------------------------------------------------------------
CREATE FUNCTION open_bet_self_exclusion_policy_strictness(p_value TEXT)
RETURNS SMALLINT
LANGUAGE sql
IMMUTABLE
AS $$
    SELECT CASE p_value
        WHEN 'SETTLE_NORMALLY' THEN 0
        WHEN 'VOID_ON_SELF_EXCLUSION' THEN 1
        ELSE NULL
    END;
$$;

-- ---------------------------------------------------------------------
-- Tighten-only enforcement AT WRITE TIME (directive item 2, first half).
-- This is the database-level backstop - internal/rg's SetOpenBet
-- SelfExclusionPolicy performs the identical check in Go first (for a
-- fast, cleanly-audited rejection), but this trigger is what makes the
-- constraint a REAL one per CLAUDE.md ("enforced by ... not by discipline
-- in application code"): it fires for any INSERT against this table
-- through any code path, not only the one Go function that happens to
-- remember to check.
--
-- A tenant/brand-scoped row (tenant_id IS NOT NULL) may never be
-- strictly LOOSER than the strictest currently-effective BROADER-scope
-- row that applies to it at its own effective_from: for a tenant-level
-- row, that is the jurisdiction floor; for a brand-level row, that is
-- the MAX of the jurisdiction floor and that tenant's own tenant-level
-- override, if one exists - not only the jurisdiction floor. ADR 0034
-- §14.2's text states the jurisdiction-floor case explicitly; this
-- trigger applies the identical "never loosen a broader scope's already-
-- tightened value" reasoning one level further, to a brand row sitting
-- under a tenant override, since the ADR's own general principle ("a
-- tenant or brand adopting a stricter, more player-protective posture...
-- is always permitted... the reverse... is never permitted") does not
-- carve out an exception for that particular pair of scopes.
--
-- A tenant/brand row additionally REQUIRES a broader-scope row to exist
-- at all (a jurisdiction floor, or - for a brand row - the jurisdiction
-- floor and/or a tenant override): "tighten-only" is meaningless without
-- something to tighten FROM, and ADR 0034 §14.2 frames the jurisdiction
-- row as the PRIMARY, foundational scope a tenant/brand only ever
-- narrows. Rejecting an override with nothing to tighten is the same
-- fail-closed posture the resolution side takes when no jurisdiction row
-- exists at all (directive's "no row for a jurisdiction = fail-closed").
CREATE FUNCTION open_bet_self_exclusion_policies_enforce_tighten_only() RETURNS TRIGGER AS $$
DECLARE
    floor_strictness  SMALLINT;
    tenant_strictness SMALLINT;
    new_strictness    SMALLINT;
    found_floor       BOOLEAN := FALSE;
BEGIN
    -- Jurisdiction-scoped rows ARE the floor - a regulator changing its
    -- own mandated value, in either direction, is a compliance
    -- configuration change (ADR 0034 §14.8), not something this trigger
    -- gates.
    IF NEW.tenant_id IS NULL THEN
        RETURN NEW;
    END IF;

    new_strictness := open_bet_self_exclusion_policy_strictness(NEW.policy_value);

    SELECT open_bet_self_exclusion_policy_strictness(policy_value)
      INTO floor_strictness
      FROM open_bet_self_exclusion_policies
     WHERE jurisdiction_code = NEW.jurisdiction_code
       AND tenant_id IS NULL
       AND brand_id IS NULL
       AND effective_from <= NEW.effective_from
       AND (effective_to IS NULL OR effective_to > NEW.effective_from)
     ORDER BY effective_from DESC
     LIMIT 1;

    IF floor_strictness IS NOT NULL THEN
        found_floor := TRUE;
    END IF;

    IF NEW.brand_id IS NOT NULL THEN
        SELECT open_bet_self_exclusion_policy_strictness(policy_value)
          INTO tenant_strictness
          FROM open_bet_self_exclusion_policies
         WHERE jurisdiction_code = NEW.jurisdiction_code
           AND tenant_id = NEW.tenant_id
           AND brand_id IS NULL
           AND effective_from <= NEW.effective_from
           AND (effective_to IS NULL OR effective_to > NEW.effective_from)
         ORDER BY effective_from DESC
         LIMIT 1;

        IF tenant_strictness IS NOT NULL THEN
            found_floor := TRUE;
            IF floor_strictness IS NULL OR tenant_strictness > floor_strictness THEN
                floor_strictness := tenant_strictness;
            END IF;
        END IF;
    END IF;

    IF NOT found_floor THEN
        RAISE EXCEPTION 'open_bet_self_exclusion_policies: a tenant/brand-scoped row requires an existing broader-scope floor for jurisdiction % effective at % - none found (fail-closed: cannot tighten a floor that does not exist)',
            NEW.jurisdiction_code, NEW.effective_from;
    END IF;

    IF new_strictness < floor_strictness THEN
        RAISE EXCEPTION 'open_bet_self_exclusion_policies: % would loosen the applicable floor (strictness % < required %) for jurisdiction=% tenant=% brand=%',
            NEW.policy_value, new_strictness, floor_strictness, NEW.jurisdiction_code, NEW.tenant_id, NEW.brand_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER open_bet_self_exclusion_policies_tighten_only
    BEFORE INSERT ON open_bet_self_exclusion_policies
    FOR EACH ROW EXECUTE FUNCTION open_bet_self_exclusion_policies_enforce_tighten_only();

-- Append-only except for effective_to (mirrors risk_rules_enforce_
-- immutability, migration 0041, exactly): a policy change is a new
-- version row, never an edit of a past one, so a past disposition
-- decision (ADR 0034 §14.5) remains explainable against the exact policy
-- version that was actually in force when it was made.
CREATE FUNCTION open_bet_self_exclusion_policies_enforce_immutability() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP IN ('DELETE', 'TRUNCATE') THEN
        RAISE EXCEPTION 'open_bet_self_exclusion_policies is append-only: % is not permitted', TG_OP;
    END IF;
    IF NEW.id <> OLD.id
        OR NEW.jurisdiction_code <> OLD.jurisdiction_code
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.brand_id IS DISTINCT FROM OLD.brand_id
        OR NEW.policy_value <> OLD.policy_value
        OR NEW.effective_from <> OLD.effective_from
        OR NEW.reason_code <> OLD.reason_code
        OR NEW.created_by_actor_type <> OLD.created_by_actor_type
        OR NEW.created_by_actor_id <> OLD.created_by_actor_id
        OR NEW.created_at <> OLD.created_at
    THEN
        RAISE EXCEPTION 'open_bet_self_exclusion_policies: only effective_to may change after creation - a policy change is a new versioned row';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER open_bet_self_exclusion_policies_immutable_core
    BEFORE UPDATE ON open_bet_self_exclusion_policies
    FOR EACH ROW EXECUTE FUNCTION open_bet_self_exclusion_policies_enforce_immutability();

CREATE TRIGGER open_bet_self_exclusion_policies_deny_delete
    BEFORE DELETE ON open_bet_self_exclusion_policies
    FOR EACH ROW EXECUTE FUNCTION open_bet_self_exclusion_policies_enforce_immutability();

CREATE TRIGGER open_bet_self_exclusion_policies_no_truncate
    BEFORE TRUNCATE ON open_bet_self_exclusion_policies
    FOR EACH STATEMENT EXECUTE FUNCTION open_bet_self_exclusion_policies_enforce_immutability();

ALTER TABLE open_bet_self_exclusion_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE open_bet_self_exclusion_policies FORCE ROW LEVEL SECURITY;

-- READ/WRITE/UPDATE/DELETE-visibility policies mirror risk_rules'
-- tenant_and_platform_* pattern (migration 0041) verbatim, including its
-- leading "app.player_account_id IS NULL" conjunct - this table has no
-- legitimate player-facing access path either.
CREATE POLICY tenant_and_platform_read ON open_bet_self_exclusion_policies
    FOR SELECT
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            tenant_id IS NULL
            OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        )
    );

CREATE POLICY tenant_and_platform_write ON open_bet_self_exclusion_policies
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
            OR (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        )
    );

CREATE POLICY tenant_and_platform_update ON open_bet_self_exclusion_policies
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

CREATE POLICY tenant_and_platform_delete_visibility ON open_bet_self_exclusion_policies
    FOR DELETE
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
            OR (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        )
    );

-- ---------------------------------------------------------------------
-- 2. self_exclusion_enumeration_runs - the per-self-exclusion-event
--    completion record security finding S-9 requires (ADR 0034 §14.5).
-- ---------------------------------------------------------------------
--
-- One row per (restriction, tenant): the same self-exclusion event can
-- have open bets under several of the excluded Person's tenants (ADR
-- 0034 §14.4), and sportsbook data is itself tenant-scoped/RLS-isolated,
-- so a future enumeration listener naturally produces one run per tenant
-- it examines, not one global row per restriction. This table proves
-- ENUMERATION ran to completion (or is still in flight, or failed) - it
-- deliberately does NOT reference any per-bet table, since no sportsbook
-- open-bet concept exists yet in this codebase (this stage's directive:
-- "do not invent a fake open bet concept or stub sportsbook tables").
-- bets_in_scope_count is a plain integer the future listener reports,
-- not a foreign-keyed count.
CREATE TABLE self_exclusion_enumeration_runs (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    restriction_id      UUID NOT NULL REFERENCES player_restrictions (id),
    tenant_id           UUID NOT NULL REFERENCES tenants (id),
    person_id           UUID NOT NULL REFERENCES persons (id),
    -- The as-of instant used to resolve OpenBetSelfExclusionPolicy for
    -- this run - expected to equal the triggering restriction's own
    -- starts_at (ADR 0034 §14.3), stored explicitly rather than joined
    -- every time so a later change to the restriction row (there is none
    -- today - player_restrictions is append-only) could never retroactively
    -- change what this run is proven to have resolved against.
    policy_as_of        TIMESTAMPTZ NOT NULL,
    bets_in_scope_count INTEGER NOT NULL DEFAULT 0 CHECK (bets_in_scope_count >= 0),
    dispatch_status     TEXT NOT NULL DEFAULT 'pending'
                            CHECK (dispatch_status IN ('pending', 'in_progress', 'completed', 'failed')),
    started_at          TIMESTAMPTZ,
    completed_at        TIMESTAMPTZ,
    failure_reason      TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    -- Idempotency: a dropped/retried listener invocation for the SAME
    -- (restriction, tenant) pair must not create a second, competing run
    -- row - mirrors CLAUDE.md's financial-write idempotency rule applied
    -- to this compliance-critical dispatch record.
    UNIQUE (restriction_id, tenant_id),
    CHECK (dispatch_status = 'pending' OR started_at IS NOT NULL),
    CHECK (dispatch_status <> 'completed' OR completed_at IS NOT NULL),
    CHECK (dispatch_status <> 'failed' OR failure_reason IS NOT NULL)
);

CREATE INDEX idx_self_exclusion_enumeration_runs_restriction ON self_exclusion_enumeration_runs (restriction_id);
CREATE INDEX idx_self_exclusion_enumeration_runs_tenant ON self_exclusion_enumeration_runs (tenant_id);
CREATE INDEX idx_self_exclusion_enumeration_runs_incomplete
    ON self_exclusion_enumeration_runs (dispatch_status)
    WHERE dispatch_status <> 'completed';

ALTER TABLE self_exclusion_enumeration_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE self_exclusion_enumeration_runs FORCE ROW LEVEL SECURITY;

-- Plain tenant-owned table (migration 0004's reference tenant_isolation
-- pattern) - unlike the policy table above, a run row always belongs to
-- exactly one concrete tenant, never platform-wide.
CREATE POLICY tenant_isolation ON self_exclusion_enumeration_runs
    FOR ALL
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

-- Identity fields are immutable once written (restriction/tenant/person/
-- policy_as_of/created_at); progress fields (count/status/timestamps/
-- failure_reason/updated_at) may change as the run advances - and once a
-- run reaches 'completed' it is frozen outright, since that is the exact
-- fact S-9's completion record exists to make tamper-evident.
CREATE FUNCTION self_exclusion_enumeration_runs_enforce_rules() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'self_exclusion_enumeration_runs is append-only: TRUNCATE is not permitted';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'self_exclusion_enumeration_runs is append-only: DELETE is not permitted';
    END IF;
    IF OLD.dispatch_status = 'completed' THEN
        RAISE EXCEPTION 'self_exclusion_enumeration_runs: row % is already completed and is now immutable', OLD.id;
    END IF;
    IF NEW.id <> OLD.id
        OR NEW.restriction_id <> OLD.restriction_id
        OR NEW.tenant_id <> OLD.tenant_id
        OR NEW.person_id <> OLD.person_id
        OR NEW.policy_as_of <> OLD.policy_as_of
        OR NEW.created_at <> OLD.created_at
    THEN
        RAISE EXCEPTION 'self_exclusion_enumeration_runs: restriction_id/tenant_id/person_id/policy_as_of/created_at are immutable after creation';
    END IF;
    NEW.updated_at := clock_timestamp();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER self_exclusion_enumeration_runs_immutable_core
    BEFORE UPDATE ON self_exclusion_enumeration_runs
    FOR EACH ROW EXECUTE FUNCTION self_exclusion_enumeration_runs_enforce_rules();

CREATE TRIGGER self_exclusion_enumeration_runs_deny_delete
    BEFORE DELETE ON self_exclusion_enumeration_runs
    FOR EACH ROW EXECUTE FUNCTION self_exclusion_enumeration_runs_enforce_rules();

CREATE TRIGGER self_exclusion_enumeration_runs_no_truncate
    BEFORE TRUNCATE ON self_exclusion_enumeration_runs
    FOR EACH STATEMENT EXECUTE FUNCTION self_exclusion_enumeration_runs_enforce_rules();
