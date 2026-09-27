-- PRH-I3: implements docs/decisions/0096-kyc-enforcement-boundary.md §3.6.
-- Migration number RE-ALLOCATED from 0101 to 0100 (Orchestrator decision,
-- 2026-09-27, docs/governance/task-registry.md PRH allocation table) so
-- KYC enforcement - implemented first - keeps the migration chain
-- gap-free; ADR 0096's own text is updated in the same commit to say
-- 0100 everywhere it previously said 0101, with this renumbering noted in
-- its §14 revision record.
--
-- Two platform-wide tables (kyc_enforcement_policies,
-- read-everywhere/platform-admin-write-only, mirroring migration 0075's
-- jurisdiction_precedence_configs RLS/append-only conventions verbatim
-- per security condition 2) plus one tenant-scoped, append-only decision
-- audit table (kyc_enforcement_decisions).
--
-- No row is seeded here (ADR 0096 §3.7): no numeric threshold, no
-- default value, anywhere in this file.

CREATE TABLE kyc_enforcement_policies (
    id                          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    licensing_jurisdiction_id   UUID NOT NULL REFERENCES jurisdictions (id),
    trigger_type                TEXT NOT NULL
        CHECK (trigger_type IN ('cumulative_deposit', 'edd_amount', 'registration_tier', 'play')),
    -- Deliberately NOT ('first_withdrawal') - the "passed required on
    -- every withdrawal" rule is structural and compiled in (ADR 0096
    -- §3.2 point 1), never a configurable row, so it can never be
    -- silently disabled by an application-layer write.
    status                      TEXT NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft', 'active', 'withdrawn')),
    threshold_minor_units       NUMERIC(38,0)
        CHECK (threshold_minor_units IS NULL OR threshold_minor_units > 0),
    asset_code                  TEXT REFERENCES assets (code),
    required_tier               TEXT CHECK (required_tier IS NULL OR required_tier IN ('basic', 'full')),
    play_operation               TEXT CHECK (play_operation IS NULL OR play_operation IN ('casino_play', 'sportsbook_play')),
    effective_from               TIMESTAMPTZ NOT NULL DEFAULT now(),
    legal_review_reference       TEXT CHECK (legal_review_reference IS NULL OR btrim(legal_review_reference) <> ''),
    reason_code                  TEXT NOT NULL CHECK (btrim(reason_code) <> ''),
    created_at                   TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by_actor_type        TEXT NOT NULL,
    created_by_actor_id          UUID NOT NULL,
    CHECK (
        (trigger_type IN ('cumulative_deposit','edd_amount') AND threshold_minor_units IS NOT NULL AND asset_code IS NOT NULL AND required_tier IS NULL AND play_operation IS NULL)
        OR (trigger_type = 'registration_tier' AND required_tier IS NOT NULL AND threshold_minor_units IS NULL AND asset_code IS NULL AND play_operation IS NULL)
        OR (trigger_type = 'play' AND play_operation IS NOT NULL AND threshold_minor_units IS NULL AND asset_code IS NULL AND required_tier IS NULL)
    ),
    CHECK (status <> 'active' OR legal_review_reference IS NOT NULL)
);

-- Security re-verification N2 (2026-09-27): asset_code is now part of the
-- uniqueness key, so one active cumulative_deposit/edd_amount row per
-- (jurisdiction, trigger_type, asset) can coexist - one per asset, never
-- an implicit cross-asset aggregate (no FX/ConversionOperation basis
-- exists; that is KYC-FX-AGG-1, a separate, registered follow-up).
CREATE UNIQUE INDEX kyc_enforcement_policies_one_active
    ON kyc_enforcement_policies (licensing_jurisdiction_id, trigger_type, COALESCE(play_operation, ''), COALESCE(asset_code, ''))
    WHERE status = 'active';

CREATE INDEX kyc_enforcement_policies_lookup
    ON kyc_enforcement_policies (licensing_jurisdiction_id, trigger_type, status);

-- Forge-proof provenance/created_at exactly like migration 0075's own
-- stamp-times trigger - a caller-supplied created_at/effective_from is
-- never trusted.
CREATE FUNCTION kyc_enforcement_policies_stamp_times() RETURNS TRIGGER AS $$
BEGIN
    NEW.effective_from := now();
    NEW.created_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER kyc_enforcement_policies_stamp_times
    BEFORE INSERT ON kyc_enforcement_policies
    FOR EACH ROW EXECUTE FUNCTION kyc_enforcement_policies_stamp_times();

-- Security condition 2 + re-verification N3/C3 (2026-09-27): append-only
-- lifecycle trigger permitting exactly draft->active, draft->withdrawn,
-- active->withdrawn; every other column is immutable after insert. Two
-- ADDITIONAL, DB-enforced controls close the re-verification findings:
--   N3 - a trigger_type this evaluator does NOT yet consult
--        (edd_amount, registration_tier) can never be activated - it may
--        sit in 'draft' indefinitely, documenting an authored-but-not-yet-
--        wired policy, but the database itself refuses draft->active for
--        it, so no operator can be given false assurance that authoring a
--        row makes it govern anything. Only 'cumulative_deposit' and
--        'play' are evaluator-wired as of this migration.
--   C3  - four-eyes on every activation and every withdrawal-of-an-active-
--        row: the transitioning principal (current_setting('app.
--        platform_admin_principal_id')) must differ from the row's own
--        created_by_actor_id. This is a first-cut, DB-enforced
--        two-distinct-principals control (creator != the principal who
--        later activates/withdraws), not a full separate pending-approval
--        workflow - disclosed as such in ADR 0096's implementation
--        record. INSERT itself can never create a row with status =
--        'active' (see the INSERT policy below), so activation is always
--        a SECOND, later statement by construction.
CREATE FUNCTION kyc_enforcement_policies_enforce_lifecycle() RETURNS TRIGGER AS $$
DECLARE
    acting_principal UUID;
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'kyc_enforcement_policies is append-only: TRUNCATE is not permitted';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'kyc_enforcement_policies is append-only: DELETE is not permitted';
    END IF;
    IF (to_jsonb(NEW) - 'status') IS DISTINCT FROM (to_jsonb(OLD) - 'status') THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: only status may change after insert';
    END IF;
    IF NOT (
        (OLD.status = 'draft' AND NEW.status IN ('active', 'withdrawn'))
        OR (OLD.status = 'active' AND NEW.status = 'withdrawn')
    ) THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: illegal status transition % -> %', OLD.status, NEW.status;
    END IF;
    IF NEW.status = 'active' AND NEW.trigger_type NOT IN ('cumulative_deposit', 'play') THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: trigger_type % is not yet evaluator-wired and may not be activated', NEW.trigger_type;
    END IF;
    acting_principal := NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid;
    IF acting_principal IS NULL THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: a platform-admin principal is required to change status';
    END IF;
    IF acting_principal = OLD.created_by_actor_id THEN
        RAISE EXCEPTION 'kyc_enforcement_policies: four-eyes required - the activating/withdrawing principal must differ from the row''s creator';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER kyc_enforcement_policies_lifecycle
    BEFORE UPDATE ON kyc_enforcement_policies
    FOR EACH ROW EXECUTE FUNCTION kyc_enforcement_policies_enforce_lifecycle();

CREATE TRIGGER kyc_enforcement_policies_deny_delete
    BEFORE DELETE ON kyc_enforcement_policies
    FOR EACH ROW EXECUTE FUNCTION kyc_enforcement_policies_enforce_lifecycle();

CREATE TRIGGER kyc_enforcement_policies_deny_truncate
    BEFORE TRUNCATE ON kyc_enforcement_policies
    FOR EACH STATEMENT EXECUTE FUNCTION kyc_enforcement_policies_enforce_lifecycle();

ALTER TABLE kyc_enforcement_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE kyc_enforcement_policies FORCE ROW LEVEL SECURITY;

CREATE POLICY kyc_enforcement_policies_read ON kyc_enforcement_policies
    FOR SELECT USING (true);

-- Security condition 2: verbatim copy of migration 0075 lines 207-225's
-- predicate shape (NULLIF, platform_admin_principal_id GUC, tenant/player
-- unset). No DELETE policy, no FOR ALL policy.
-- C3: INSERT may never create an 'active' row directly - activation is
-- always a later, second UPDATE statement (necessarily by a DIFFERENT
-- principal, per the lifecycle trigger above), never a single INSERT.
CREATE POLICY kyc_enforcement_policies_platform_insert ON kyc_enforcement_policies
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND created_by_actor_id = NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid
        AND status = 'draft'
    );

CREATE POLICY kyc_enforcement_policies_platform_update ON kyc_enforcement_policies
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

COMMENT ON TABLE kyc_enforcement_policies IS 'ADR 0096 §3.6: platform-wide KYC enforcement threshold/structural/play trigger configuration, keyed on (licensing_jurisdiction_id, trigger_type[, play_operation]) - never tenant_id (same bootstrap-circularity reasoning as jurisdiction_precedence_configs, migration 0075). Zero rows seeded. Write requires a genuine platform-admin-scoped transaction (db.Pool.WithPlatformAdmin); a tenant-scoped compliance/platform_admin-shaped role can never satisfy the predicate.';

-- ======================================================================
-- kyc_enforcement_decisions - tenant-scoped, append-only decision audit
-- ======================================================================

CREATE TABLE kyc_enforcement_decisions (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL,
    brand_id             UUID NOT NULL,
    player_account_id    UUID NOT NULL,
    operation            TEXT NOT NULL CHECK (operation IN ('deposit','withdrawal_hold','withdrawal_payout','casino_play','sportsbook_play')),
    outcome              TEXT NOT NULL CHECK (outcome IN ('not_required','passed','pending','failed','unavailable')),
    allowed              BOOLEAN NOT NULL,
    matched_trigger      TEXT,
    policy_version        TEXT NOT NULL,
    correlation_id        UUID NOT NULL,
    decided_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (player_account_id, tenant_id) REFERENCES player_accounts (id, tenant_id)
);

CREATE INDEX kyc_enforcement_decisions_player ON kyc_enforcement_decisions (tenant_id, player_account_id, decided_at DESC);
CREATE INDEX kyc_enforcement_decisions_keyset ON kyc_enforcement_decisions (tenant_id, decided_at DESC, id DESC);

ALTER TABLE kyc_enforcement_decisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE kyc_enforcement_decisions FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON kyc_enforcement_decisions
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);

CREATE FUNCTION kyc_enforcement_decisions_deny_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'kyc_enforcement_decisions is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER kyc_enforcement_decisions_immutable
    BEFORE UPDATE OR DELETE ON kyc_enforcement_decisions
    FOR EACH ROW EXECUTE FUNCTION kyc_enforcement_decisions_deny_mutation();

CREATE TRIGGER kyc_enforcement_decisions_deny_truncate
    BEFORE TRUNCATE ON kyc_enforcement_decisions
    FOR EACH STATEMENT EXECUTE FUNCTION kyc_enforcement_decisions_deny_mutation();

COMMENT ON TABLE kyc_enforcement_decisions IS 'ADR 0096 §3.6/§7.6: immutable, tenant-scoped, PII-free record of every kyc.EvaluateEnforcement call - staff/SAR-adjacent read via GET /v1/admin/kyc/enforcement-decisions. Never the same table as audit_log; every enforcement point additionally writes one audit.Record call.';
