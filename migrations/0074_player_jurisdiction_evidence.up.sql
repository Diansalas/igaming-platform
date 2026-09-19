-- Stage 4I Phase B: schema foundation for player jurisdiction evidence
-- (declared residence, verified residence) plus the activation boundary
-- that gates whether collecting either is even switched on for a given
-- tenant.
--
-- This migration answers no legal/privacy question itself and grants no
-- new read/write CAPABILITY to any existing package - it only:
--
--   1. Adds the declared/verified-residence COLUMNS the residence
--      write/read paths (a LATER dispatch) will use, per
--      docs/decisions/0042-human-decision-response.md HDR-J-3b/3c/3f/3h
--      (declared residence lives on the player's own brand-account
--      record; verified residence is set only by an explicit reviewer
--      determination alongside the existing KYC verification record;
--      never auto-derived from a KYC document's issuing country).
--   2. Creates jurisdiction_evidence_collection_active - the Phase B
--      "activation boundary": a per-tenant, per-evidence-type switch,
--      default OFF, that must be explicitly turned on before any
--      declared-residence write or verified-residence determination is
--      accepted. This exists because HDR-J-3e is explicit that lawful
--      basis / permitted use is a legal/compliance judgment, not an
--      engineering one, and the hybrid licensing model
--      (docs/decisions/0006) means that judgment is inherently
--      PER-TENANT (an operator bringing its own licence in its own
--      jurisdiction may clear this at a different time, or never, versus
--      the platform's own B2C brand) - there is no single platform-wide
--      "go" moment this could instead be a global flag for.
--
-- See docs/plans/stage-4i-jurisdiction-implementation-plan.md for the
-- overall Stage 4I phasing. Deliberately does NOT touch
-- internal/jurisdiction/resolver.go, jurisdiction_precedence_configs, or
-- the Basis enum - none of those are in this migration's scope.

-- ======================================================================
-- 1. player_accounts: declared residence
-- ======================================================================
-- HDR-J-3b/3f: self-declared, unverified, lives on the player's own
-- brand-specific account record (never the shared cross-brand person
-- record). Captured-at is required exactly when a country is present -
-- there is no way to record "we asked and got an answer" without also
-- recording when.

ALTER TABLE player_accounts
    ADD COLUMN declared_residence_country     TEXT        NULL CHECK (declared_residence_country ~ '^[A-Z]{2}$'),
    ADD COLUMN declared_residence_captured_at TIMESTAMPTZ NULL;

ALTER TABLE player_accounts
    ADD CONSTRAINT player_accounts_declared_residence_pair
    CHECK ((declared_residence_country IS NULL) = (declared_residence_captured_at IS NULL));

COMMENT ON COLUMN player_accounts.declared_residence_country IS 'HDR-J-3b: self-declared, unverified residence (ISO-3166-1 alpha-2). Never sufficient on its own for an enforcement-grade decision unless an explicitly approved operation-specific policy permits it. Written only via the (later-dispatch) declared-residence write path, gated by jurisdiction_evidence_collection_active.';
COMMENT ON COLUMN player_accounts.declared_residence_captured_at IS 'Set if and only if declared_residence_country is set - when the declaration was captured.';

-- ======================================================================
-- 2. kyc_verifications: verified residence
-- ======================================================================
-- HDR-J-3c: set only by an explicit reviewer determination, never
-- auto-derived from a KYC document field (HDR-J-3h). verified_residence_
-- source is a closed, single-valued enum today (only
-- 'reviewer_determination' is a permitted origin) so a future corroboration
-- source (HDR-J-3h's "if that corroboration role is ever activated") is an
-- additive CHECK-constraint change, not a silent widening of what this
-- column can already mean. set_by/set_at mirror this table's own existing
-- reviewed_by/reviewed_at pair-completeness convention exactly.

ALTER TABLE kyc_verifications
    ADD COLUMN verified_residence_country TEXT        NULL CHECK (verified_residence_country ~ '^[A-Z]{2}$'),
    ADD COLUMN verified_residence_source  TEXT        NULL CHECK (verified_residence_source IN ('reviewer_determination')),
    ADD COLUMN verified_residence_set_by  UUID        NULL,
    ADD COLUMN verified_residence_set_at  TIMESTAMPTZ NULL;

ALTER TABLE kyc_verifications
    ADD CONSTRAINT kyc_verifications_verified_residence_country_pair
        CHECK ((verified_residence_country IS NULL) = (verified_residence_set_at IS NULL)),
    ADD CONSTRAINT kyc_verifications_verified_residence_setby_pair
        CHECK ((verified_residence_country IS NULL) = (verified_residence_set_by IS NULL)),
    ADD CONSTRAINT kyc_verifications_verified_residence_source_pair
        CHECK ((verified_residence_country IS NULL) = (verified_residence_source IS NULL)),
    ADD CONSTRAINT kyc_verifications_verified_residence_set_by_fk
        FOREIGN KEY (verified_residence_set_by, tenant_id) REFERENCES staff_users (id, tenant_id);

COMMENT ON COLUMN kyc_verifications.verified_residence_country IS 'HDR-J-3c: KYC-reviewer-verified residence (ISO-3166-1 alpha-2), set only by an explicit reviewer determination - never auto-derived from a document''s issuing country (HDR-J-3h). Written only via the (later-dispatch) verified-residence write path, gated by jurisdiction_evidence_collection_active.';

-- ======================================================================
-- 3. jurisdiction_evidence_collection_active (Phase B activation boundary)
-- ======================================================================
-- Mirrors jurisdiction_resolution_active's FINAL, post-0072/post-0073
-- shape exactly (per-command SELECT/INSERT/UPDATE policies, no FOR ALL,
-- no DELETE policy, plus a BEFORE TRUNCATE deny trigger from day one -
-- RLS does not govern TRUNCATE at all, so building this table with the
-- two defects jurisdiction_resolution_active shipped with and had to fix
-- in migrations 0072/0073 would just be reopening the identical, already-
-- litigated hazard on a second table).
--
-- 'location_signal' is declared now as a reserved evidence_type value
-- (HDR-J-3a) even though this Phase B dispatch adds no Go reader for it -
-- a future phase that does add one then needs no migration just to widen
-- this CHECK constraint.

CREATE TABLE jurisdiction_evidence_collection_active (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    evidence_type         TEXT NOT NULL CHECK (evidence_type IN
                              ('declared_residence', 'verified_residence', 'location_signal')),
    active                BOOLEAN NOT NULL DEFAULT false,
    effective_from        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by_actor_type TEXT NOT NULL CHECK (created_by_actor_type IN ('staff', 'system')),
    created_by_actor_id   UUID NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, evidence_type)
);

ALTER TABLE jurisdiction_evidence_collection_active ENABLE ROW LEVEL SECURITY;
ALTER TABLE jurisdiction_evidence_collection_active FORCE ROW LEVEL SECURITY;

-- Per-command policies, no FOR ALL, no DELETE policy - jurisdiction_
-- resolution_active's current (post-0072) shape, verified against the
-- actual live schema, not migration 0071's superseded original. Same
-- leading "app.player_account_id IS NULL" conjunct that keeps a
-- player-scoped connection (which sets BOTH GUCs) out of this table
-- entirely - no player-facing path ever reads or writes this table.

CREATE POLICY jurisdiction_evidence_collection_active_tenant_isolation_read ON jurisdiction_evidence_collection_active
    FOR SELECT
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

CREATE POLICY jurisdiction_evidence_collection_active_tenant_isolation_insert ON jurisdiction_evidence_collection_active
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

-- UPDATE is permitted (mutable current-state fact, same as jurisdiction_
-- resolution_active) - SetEvidenceCollectionActive's own
-- INSERT ... ON CONFLICT DO UPDATE upsert needs it. Both USING and WITH
-- CHECK so a row can neither be updated out of this tenant's scope nor
-- updated from outside it.
CREATE POLICY jurisdiction_evidence_collection_active_tenant_isolation_update ON jurisdiction_evidence_collection_active
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

-- Deliberately NO DELETE policy. Collection for an evidence type is
-- turned off by SetEvidenceCollectionActive(active=false), which is
-- audited with before/after and a reason code; it is never removed.

CREATE FUNCTION jurisdiction_evidence_collection_active_touch_updated_at() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER jurisdiction_evidence_collection_active_updated_at
    BEFORE UPDATE ON jurisdiction_evidence_collection_active
    FOR EACH ROW EXECUTE FUNCTION jurisdiction_evidence_collection_active_touch_updated_at();

-- BEFORE TRUNCATE deny trigger from day one (migration 0073's own fix for
-- jurisdiction_resolution_active, built in here instead of as a follow-up
-- migration): PostgreSQL RLS does not apply to TRUNCATE at all, and the
-- application role owns this table (deploy/init-app-role.sql:
-- NOSUPERUSER, NOBYPASSRLS, but owner of the application database and
-- schema), so without this trigger a plain tenant-scoped connection could
-- erase every tenant's evidence-collection-active facts in one statement
-- with no audit_log trace at all.

CREATE FUNCTION jurisdiction_evidence_collection_active_deny_truncate() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '%: TRUNCATE is not permitted - an evidence-collection-active fact is turned off with SetEvidenceCollectionActive(active = false), which is audited with before/after and a reason code, never erased', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER jurisdiction_evidence_collection_active_no_truncate
    BEFORE TRUNCATE ON jurisdiction_evidence_collection_active
    FOR EACH STATEMENT EXECUTE FUNCTION jurisdiction_evidence_collection_active_deny_truncate();

COMMENT ON TABLE jurisdiction_evidence_collection_active IS 'Stage 4I Phase B activation boundary: per-tenant, per-evidence-type switch (declared_residence/verified_residence/location_signal), default OFF, that must be explicitly turned on before any privacy-sensitive jurisdiction evidence of that type is collected. Legal/privacy clearance under HDR-J-3e (docs/decisions/0042-human-decision-response.md) is inherently per-tenant given the hybrid licensing model (docs/decisions/0006), so this is never a single platform-wide flag. See docs/plans/stage-4i-jurisdiction-implementation-plan.md.';
