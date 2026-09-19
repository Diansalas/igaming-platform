-- Stage 4I: platform-wide jurisdiction resolution foundation. Implements
-- the canonical, binding specification at
-- docs/governance/stage-4i-canonical-model.md:
--
--   * B-2 (§5.2, SEC AR-1/AR-2)      - jurisdiction_resolutions
--   * B-6 (§4.2)                     - jurisdiction_resolution_active
--   * B-5 (§3.4)                     - jurisdiction_precedence_configs (SHAPE ONLY)
--
-- This migration does not implement the resolver itself (internal/
-- jurisdiction, Go code, same commit) and populates no precedence
-- content - §3.4/§11.2 record that the precedence configuration's ROWS
-- remain blocked on HDR-J-2, a human decision this migration does not
-- make.

-- ======================================================================
-- 1. jurisdiction_resolutions (canonical-model §5.2)
-- ======================================================================
-- One row per resolution ATTEMPT, including unresolved/refused outcomes
-- (§5.1 - "including failures"). Append-only, tenant-scoped, mirrors
-- audit_log's own immutability mechanism (migrations 0014/0016) rather
-- than inventing a new one: RLS alone is not sufficient (it does not
-- stop the table owner), so a BEFORE UPDATE OR DELETE / BEFORE TRUNCATE
-- trigger backstops it exactly as audit_log_immutable does.

CREATE TABLE jurisdiction_resolutions (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID NOT NULL REFERENCES tenants (id),
    brand_id                 UUID,
    player_account_id        UUID,
    operation_class          TEXT NOT NULL CHECK (operation_class IN (
                                 'play', 'catalogue_availability', 'bonus_issuance', 'bonus_conversion'
                             )),
    requested_by_actor_type  TEXT NOT NULL CHECK (requested_by_actor_type IN ('player', 'staff', 'service', 'system')),
    requested_by_actor_id    UUID,
    outcome                  TEXT NOT NULL CHECK (outcome IN ('resolved', 'unresolved', 'refused')),
    reason                   TEXT NOT NULL CHECK (reason IN (
                                 'determined', 'insufficient_confidence', 'no_signal', 'irreconcilable_bases',
                                 'dependency_unavailable', 'unsupported_operation_class', 'scope_mismatch',
                                 'registry_unknown_code'
                             )),
    jurisdiction_code        TEXT REFERENCES jurisdictions (code),
    selected_basis           TEXT CHECK (selected_basis IN (
                                 'player_verified_residence', 'player_declared_residence', 'kyc_corroboration',
                                 'geo_signal', 'retail_node', 'tenant_licence', 'tenant_asserted',
                                 'platform_fallback', 'staff_supplied'
                             )),
    -- Per-basis STATUS only (selected / rejected_lower_precedence /
    -- unavailable / disagreed) - NEVER the evidentiary VALUE a basis
    -- held (canonical-model §5.3 item 1, the governing principle: "a
    -- resolution record persists the DECISION and REFERENCES to its
    -- evidence, never the evidence VALUES"). The shape is
    -- application-enforced (internal/jurisdiction), not mechanically
    -- checkable in a CHECK constraint.
    considered_bases         JSONB NOT NULL DEFAULT '[]'::jsonb,
    -- The confidence BUCKET the resolver applied for this operation
    -- class - never a raw vendor score (§5.2).
    confidence_class         TEXT CHECK (confidence_class IN ('authoritative', 'declared', 'corroborated', 'verified')),
    resolver_policy_version  TEXT NOT NULL,
    registry_version         TEXT,
    config_effective_from    TIMESTAMPTZ,
    as_of                    TIMESTAMPTZ NOT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- §5.2: resolved <=> a code is carried. Closes RISK §3.3's
    -- empty/unknown-code collapse at the persistence layer
    -- (canonical-model §2.5 item 2) - a code that is not in
    -- `jurisdictions` can never be stored, and an outcome other than
    -- `resolved` can never carry one.
    CHECK ((jurisdiction_code IS NOT NULL) = (outcome = 'resolved')),
    -- §3.2: the ONE database-enforced rule preventing HDR-J-1 from being
    -- silently answered "yes" by a future regression - a player-scoped
    -- resolution may never carry the tenant's own licensing basis.
    CHECK (player_account_id IS NULL OR selected_basis <> 'tenant_licence'),
    -- audit_log's own vocabulary/shape (migration 0014): actor_id is
    -- NULL if and only if actor_type = 'system'.
    CHECK ((requested_by_actor_type = 'system') = (requested_by_actor_id IS NULL)),

    -- Composite FKs (migration 0043's precedent, re-verified at 0045) - a
    -- row can never name a brand or player account belonging to a
    -- DIFFERENT tenant than the one this row itself names.
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id),
    FOREIGN KEY (player_account_id, tenant_id) REFERENCES player_accounts (id, tenant_id)

    -- Deliberately NO UNIQUE constraint keyed on an operation
    -- (canonical-model §5.2 note 1): more than one resolution may
    -- legitimately exist for one operation today (a retry), and in a
    -- future phase (one row per MROC candidate, §7.3/§7.4). Adding a
    -- uniqueness constraint now for tidiness would foreclose that and
    -- require a migration on an append-only table to undo it.
);

CREATE INDEX idx_jurisdiction_resolutions_tenant_time ON jurisdiction_resolutions (tenant_id, created_at DESC);
CREATE INDEX idx_jurisdiction_resolutions_player ON jurisdiction_resolutions (tenant_id, player_account_id) WHERE player_account_id IS NOT NULL;

ALTER TABLE jurisdiction_resolutions ENABLE ROW LEVEL SECURITY;
ALTER TABLE jurisdiction_resolutions FORCE ROW LEVEL SECURITY;

-- Staff/service/system, own tenant only. Deliberately NO player-read
-- policy at all (canonical-model §6.1, §6.2 scenario 3): a player who
-- could read this table would learn which signal the platform trusted
-- and which it rejected - directly attack-useful. The leading
-- "app.player_account_id IS NULL" conjunct mirrors asset_authorizations'
-- own tenant_isolation policy exactly, for the identical reason: without
-- it, a player-scoped connection (which sets BOTH GUCs) would also
-- satisfy a plain tenant-match predicate and read/write this table.
CREATE POLICY jurisdiction_resolutions_staff_insert ON jurisdiction_resolutions
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

CREATE POLICY jurisdiction_resolutions_staff_select ON jurisdiction_resolutions
    FOR SELECT
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

-- No UPDATE policy. No DELETE policy. Append-only is enforced by BOTH
-- the absence of those policies AND a trigger that fires regardless of
-- role or table ownership (audit_log's own precedent, migration 0014 -
-- "a trigger is not bypassed by ownership, unlike REVOKE").
CREATE FUNCTION jurisdiction_resolutions_deny_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'jurisdiction_resolutions is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER jurisdiction_resolutions_immutable
    BEFORE UPDATE OR DELETE ON jurisdiction_resolutions
    FOR EACH ROW EXECUTE FUNCTION jurisdiction_resolutions_deny_mutation();

-- Row-level triggers do not fire on TRUNCATE (migration 0016's own
-- finding) - a separate statement-level trigger is required.
CREATE TRIGGER jurisdiction_resolutions_deny_truncate
    BEFORE TRUNCATE ON jurisdiction_resolutions
    FOR EACH STATEMENT EXECUTE FUNCTION jurisdiction_resolutions_deny_mutation();

COMMENT ON TABLE jurisdiction_resolutions IS 'Append-only record of every jurisdiction resolution ATTEMPT, including unresolved/refused outcomes (canonical-model Sec 5.1). One row per attempt - never edited, never deleted. See docs/governance/stage-4i-canonical-model.md Sec 5.';

-- ======================================================================
-- 2. jurisdiction_resolution_active (canonical-model Sec 4.2, item B-6)
-- ======================================================================
-- Resolver-owned, tenant-scoped fact recording whether jurisdiction
-- resolution is genuinely active for a (tenant, operation_class) pair.
-- Supplies the fact RISK Sec 2.4b's future risk.CreateRule precondition
-- (R-2b) reads via a narrow read-only accessor (internal/jurisdiction,
-- same commit) - internal/risk defines no table, flag or role of its
-- own for this (canonical-model Sec 4.2, explicit).
--
-- Unlike jurisdiction_resolutions, this is a mutable CURRENT-STATE fact
-- (like asset_operation_eligibility's `eligible` flag), not an
-- append-only log - every change is audited in the application layer
-- (internal/jurisdiction.SetResolutionActive writes an
-- "jurisdiction_resolution_active.changed" audit_log entry in the same
-- transaction, canonical-model Sec 5.1).

CREATE TABLE jurisdiction_resolution_active (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    operation_class       TEXT NOT NULL CHECK (operation_class IN (
                              'play', 'catalogue_availability', 'bonus_issuance', 'bonus_conversion'
                          )),
    active                BOOLEAN NOT NULL DEFAULT false,
    effective_from        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by_actor_type TEXT NOT NULL CHECK (created_by_actor_type IN ('staff', 'system')),
    created_by_actor_id   UUID NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, operation_class)
);

ALTER TABLE jurisdiction_resolution_active ENABLE ROW LEVEL SECURITY;
ALTER TABLE jurisdiction_resolution_active FORCE ROW LEVEL SECURITY;

-- Tenant isolation, staff/system only (no player-facing path reads or
-- writes this table) - identical shape to asset_authorizations'
-- tenant_isolation policy (migration 0045).
CREATE POLICY jurisdiction_resolution_active_tenant_isolation ON jurisdiction_resolution_active
    FOR ALL
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

CREATE FUNCTION jurisdiction_resolution_active_touch_updated_at() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER jurisdiction_resolution_active_updated_at
    BEFORE UPDATE ON jurisdiction_resolution_active
    FOR EACH ROW EXECUTE FUNCTION jurisdiction_resolution_active_touch_updated_at();

COMMENT ON TABLE jurisdiction_resolution_active IS 'Resolver-owned, tenant-scoped fact: is jurisdiction resolution genuinely active for this (tenant, operation_class) pair. Consumed read-only by internal/risk''s future CreateRule precondition (R-2b, canonical-model Sec 4.2) - risk defines no table/flag/role of its own for this.';

-- ======================================================================
-- 3. jurisdiction_precedence_configs (canonical-model Sec 3.4, item B-5)
-- ======================================================================
-- SHAPE ONLY. No rows are inserted by this migration and none may be
-- inserted until HDR-J-2 (which player-side signal is legally
-- authoritative for which operation class) is answered - see
-- canonical-model Sec 3.4/Sec 11.2. Keyed on the TENANT'S LICENSING
-- JURISDICTION plus operation_class, NOT tenant_id - RISK Sec 4.2's
-- bootstrap-circularity finding, adopted verbatim: selecting a
-- per-jurisdiction precedence rule cannot itself depend on the
-- jurisdiction the rule would determine, but the tenant's OWN licensing
-- jurisdiction is knowable before any player-side resolution runs
-- (tenants.licence_id -> licences.jurisdiction_id).
--
-- Platform-wide reference configuration - same no-RLS posture as
-- `jurisdictions`/`licences` (canonical-model Sec 6.1: "jurisdictions
-- stays platform-scoped with no RLS... It is a platform fact and an FK
-- target every tenant-scoped transaction must read"). This table has no
-- tenant_id column at all: two tenants that happen to share a licensing
-- jurisdiction legitimately share one precedence configuration.

CREATE TABLE jurisdiction_precedence_configs (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    licensing_jurisdiction_id UUID NOT NULL REFERENCES jurisdictions (id),
    operation_class           TEXT NOT NULL CHECK (operation_class IN (
                                  'play', 'catalogue_availability', 'bonus_issuance', 'bonus_conversion'
                              )),
    -- Ordered list of basis values, most-authoritative first. CONTENT is
    -- blocked on HDR-J-2 (canonical-model Sec 3.4/Sec 11.2) - this
    -- migration creates the shape only and inserts no rows.
    precedence                JSONB NOT NULL DEFAULT '[]'::jsonb,
    resolver_policy_version   TEXT NOT NULL,
    effective_from            TIMESTAMPTZ NOT NULL DEFAULT now(),
    effective_to              TIMESTAMPTZ,
    created_by_actor_type     TEXT NOT NULL CHECK (created_by_actor_type IN ('staff', 'system')),
    created_by_actor_id       UUID NOT NULL,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (licensing_jurisdiction_id, operation_class, effective_from)
);

COMMENT ON TABLE jurisdiction_precedence_configs IS 'SHAPE ONLY - no rows in Stage 4I. Keyed on (tenant licensing jurisdiction, operation_class), NOT tenant_id, per RISK Sec 4.2''s bootstrap-circularity finding (canonical-model Sec 3.4). Platform-wide reference configuration, no RLS - same posture as jurisdictions/licences. Content is blocked on HDR-J-2.';
