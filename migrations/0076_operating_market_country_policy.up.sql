-- Stage 4I Phase E: Operating Market & Country Policy Foundation
-- (architect design ruling, recorded in full at
-- docs/decisions/0045-operating-market-and-country-policy-foundation.md).
--
-- Builds the MECHANISM ONLY for "which countries may a tenant/brand
-- actually operate in, for which product/operation, within the ceiling
-- its licence permits". This migration inserts ZERO country rows, ZERO
-- licence_country_ceilings rows, and ZERO operating_country_policies
-- rows - only the four seeded platform_operations vocabulary rows
-- (registration/deposit/withdrawal/wagering), which are vocabulary, not
-- production country/market content.
--
-- Four independent pieces, each with its own rationale recorded in the
-- ADR:
--   1. jurisdictions.country_code - administrative metadata only, NOT a
--      country->jurisdiction resolver (fenced by INV-M-4).
--   2. platform_operations - a new, extensible OPERATION vocabulary,
--      deliberately disjoint from jurisdiction.OperationClass (4 values,
--      the player-jurisdiction resolver's call-site taxonomy),
--      asset_operation_eligibility.operation (6 values, frozen by ADR
--      0037 C.2), and risk_rules.operation (7 values). platform_products
--      (migration 0045) is reused UNCHANGED for the product dimension -
--      product and operation are two independent dimensions (ADR 0045
--      §1.3).
--   3. licence_country_ceilings - the platform-wide, append-only,
--      effective-dated ceiling a licence places on which countries may
--      ever be enabled beneath it. A NEW table, not a column on
--      `licences` and not a row in the policy table below - this is what
--      makes "the licence is a ceiling, not a link in an inheritance
--      chain" structurally true.
--   4. operating_country_policies - the tenant/brand/operation-scoped,
--      append-only, effective-dated statement of whether a tenant
--      actually operates in a country, narrowing (never exceeding) the
--      licence ceiling. A NEW package, internal/operatingmarket, is the
--      only application-layer reader/writer - internal/jurisdiction gets
--      ZERO diff from this migration.
--
-- Every triggered mechanism below (forge-proof timestamps, append-only
-- enforcement, per-command RLS, partial-unique-open-version indexes) is
-- migration 0075's own established shape, applied a second and third
-- time - this is deliberately NOT a new pattern.

-- ======================================================================
-- 1. jurisdictions.country_code (ADR 0045 §1.1)
-- ======================================================================

ALTER TABLE jurisdictions
    ADD COLUMN country_code TEXT
        CHECK (country_code IS NULL OR country_code ~ '^[A-Z]{2}$');

COMMENT ON COLUMN jurisdictions.country_code IS
'ADMINISTRATIVE METADATA ONLY (Stage 4I Phase E): the ISO-3166-1 alpha-2 country this REGULATORY jurisdiction sits inside, where that is unambiguous. NULLABLE and NULL by default. This column is NOT a country->jurisdiction mapping and MUST NEVER be used to derive a player''s jurisdiction from a residence/location country, nor to compare, cast or join a country code to jurisdictions.code (internal/validation/country.go''s own governing rule: jurisdictions.code is a DIFFERENT code space - KM-ANJ is sub-national; MT/CO match ISO alpha-2 only coincidentally). Shape is CHECK-enforced here; ISO-3166 ASSIGNMENT is enforced in Go by validation.IsISO3166Alpha2 on the write path. Not UNIQUE: several regulatory jurisdictions may legitimately sit in one country (sub-national regulators).';

-- RULING: zero rows are auto-assigned. No migration seeds `jurisdictions`
-- (grep confirms this), so in a clean environment there is nothing to
-- backfill; in a dev environment with operator-authored rows, every
-- existing jurisdictions.code is ambiguous under jurisdictions.code's own
-- different-code-space rule (KM-ANJ could naively map to KM, which is
-- arguably right only by coincidence) and must not be decided silently by
-- a migration. jurisdictions.code itself is untouched, byte for byte.

-- ======================================================================
-- 2. platform_operations (ADR 0045 §1.2) - new OPERATION vocabulary
-- ======================================================================

CREATE TABLE platform_operations (
    code         TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    -- Fail-closed default, identical rationale to platform_products.active
    -- and assets.active (migration 0044 finding S-4): a typo'd operation
    -- code must not silently become a namable policy grain.
    active       BOOLEAN NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE platform_operations IS
'Extensible OPERATION vocabulary for operating-market country policy (Stage 4I Phase E). A new operation is a ROW here, never a new code path and never a widened CHECK constraint. Deliberately DISJOINT from three pre-existing vocabularies, each of which answers a different question: jurisdiction.OperationClass (4 values, the player-jurisdiction resolver''s call-site taxonomy), asset_operation_eligibility.operation (6 values, frozen by ADR 0037 C.2, money-movement eligibility for one ASSET), and risk_rules.operation (7 values, risk-evaluation call sites). This table answers "which operational ACT is being permitted or withheld in a country". The PRODUCT dimension is NOT here - it is platform_products, reused unchanged.';

-- Values set EXPLICITLY, never left to the default (migration 0044's rule).
INSERT INTO platform_operations (code, display_name, active) VALUES
    ('registration', 'Account registration', true),
    ('deposit',      'Deposit',              true),
    ('withdrawal',   'Withdrawal',           true),
    ('wagering',     'Wagering',             true);

ALTER TABLE platform_operations ENABLE ROW LEVEL SECURITY;
ALTER TABLE platform_operations FORCE ROW LEVEL SECURITY;

-- Read-open: this is a vocabulary/FK-target table, not operating strategy.
-- It reveals nothing about which countries anyone operates in - the same
-- read posture platform_products and assets already carry, for the
-- identical reason, and NOT the permissive-cross-tenant-read that is
-- prohibited on the POLICY tables below (§7.3 of the ADR).
CREATE POLICY platform_operations_read ON platform_operations
    FOR SELECT USING (true);

CREATE POLICY platform_operations_platform_admin_insert ON platform_operations
    FOR INSERT WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
CREATE POLICY platform_operations_platform_admin_update ON platform_operations
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
-- No DELETE policy. No FOR ALL policy.

-- ======================================================================
-- 3. licence_country_ceilings (ADR 0045 §2) - the licence's ceiling
-- ======================================================================

CREATE TABLE licence_country_ceilings (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    licence_id              UUID NOT NULL REFERENCES licences (id),
    -- ISO-3166-1 alpha-2. Shape CHECK here; ASSIGNMENT validated in Go by
    -- validation.IsISO3166Alpha2. Deliberately NOT an FK to jurisdictions:
    -- a licence's permitted country is a COUNTRY, not a regulatory
    -- jurisdiction, and conflating the two code spaces is the exact trap
    -- internal/validation/country.go forbids.
    country_code            TEXT NOT NULL CHECK (country_code ~ '^[A-Z]{2}$'),
    -- EXACTLY TWO STORED VALUES (Phase D.1 rulings 8/9). "inherited" and
    -- "not_configured" are NEVER stored - they are derived from row absence.
    state                   TEXT NOT NULL CHECK (state IN ('enabled', 'disabled')),
    -- Two values only (ADR 0045 INC-5 - formally revises Phase D's
    -- three-valued status for this table: Phase E has no HDR-blocked
    -- content to park as 'draft').
    status                  TEXT NOT NULL CHECK (status IN ('active', 'withdrawn')),
    -- The recorded human/compliance authorization for this ceiling grant.
    -- Phase D's legal_review_reference device, applied to the direction
    -- that actually widens the footprint.
    authorization_reference TEXT CHECK (authorization_reference IS NULL OR btrim(authorization_reference) <> ''),
    reason_code             TEXT NOT NULL CHECK (btrim(reason_code) <> ''),
    policy_version          TEXT NOT NULL,
    effective_from          TIMESTAMPTZ NOT NULL DEFAULT now(),
    effective_to            TIMESTAMPTZ,
    created_by_actor_type   TEXT NOT NULL CHECK (created_by_actor_type IN ('staff', 'system')),
    created_by_actor_id     UUID NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT licence_country_ceilings_effective_to_after_from
        CHECK (effective_to IS NULL OR effective_to > effective_from),
    -- An ENABLE (the widening direction) can never be recorded without a
    -- named authorization. A DISABLE (fail-closed direction) never needs
    -- one - ADR 0037 C.5.3's asymmetry, adopted verbatim.
    CONSTRAINT licence_country_ceilings_enable_requires_authorization
        CHECK (NOT (status = 'active' AND state = 'enabled') OR authorization_reference IS NOT NULL),
    -- A tombstone carries no content.
    CONSTRAINT licence_country_ceilings_withdrawn_is_disabled
        CHECK (status <> 'withdrawn' OR state = 'disabled')
);

-- THE authoritative concurrency control: at most one OPEN version per
-- (licence, country). Database-enforced, never application check-then-insert.
CREATE UNIQUE INDEX uq_licence_country_ceilings_open
    ON licence_country_ceilings (licence_id, country_code)
    WHERE effective_to IS NULL;

CREATE INDEX idx_licence_country_ceilings_lookup
    ON licence_country_ceilings (licence_id, country_code, effective_from DESC);

-- --- Triggers: byte-for-byte migration 0075's mechanism (names changed) ---

-- (a) Forge-proof stamping. effective_from is DB-set unconditionally on
-- INSERT; no write API accepts one; backdating and future-dating are
-- both structurally unavailable. Scheduled future-dated activation is a
-- real future capability, DEFERRED, not built.
CREATE FUNCTION licence_country_ceilings_stamp_times() RETURNS TRIGGER AS $$
BEGIN
    NEW.effective_from := now();
    NEW.created_at     := now();
    NEW.effective_to   := NULL;
    RETURN NEW;
END; $$ LANGUAGE plpgsql;
CREATE TRIGGER licence_country_ceilings_stamp_times
    BEFORE INSERT ON licence_country_ceilings
    FOR EACH ROW EXECUTE FUNCTION licence_country_ceilings_stamp_times();

-- (b) Append-only. Identical body to
-- jurisdiction_precedence_configs_enforce_append_only(), INCLUDING the
-- `to_jsonb(NEW) - 'effective_to' IS DISTINCT FROM to_jsonb(OLD) -
-- 'effective_to'` comparison (so a future added column is immutable BY
-- DEFAULT rather than by remembering to extend a column list) and the
-- forced `NEW.effective_to := now()` on the NULL->non-NULL transition.
--
-- THE SAME NARROW, RAW-SQL-ONLY RESIDUAL disclosed for migration 0075
-- (PHASE-D-ARCH/SEC-P3-2) applies here in the identical terms: two
-- separate raw-platform-admin transactions, one holding a bare close
-- open, can still construct a window gap because each transaction's
-- now() is pinned at its own BEGIN. This is UNREACHABLE via the
-- sanctioned write path (CreateLicenceCountryCeilingVersion, which
-- always closes and inserts in ONE transaction). The btree_gist
-- `EXCLUDE USING gist` hardening remains the named, deferred fix. This is
-- disclosed here deliberately, not silently re-discovered.
CREATE FUNCTION licence_country_ceilings_enforce_append_only() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'licence_country_ceilings is append-only: TRUNCATE is not permitted';
    END IF;
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'licence_country_ceilings is append-only: DELETE is not permitted';
    END IF;
    IF OLD.effective_to IS NULL AND NEW.effective_to IS NOT NULL THEN
        NEW.effective_to := now();
    END IF;
    IF (to_jsonb(NEW) - 'effective_to') IS DISTINCT FROM (to_jsonb(OLD) - 'effective_to') THEN
        RAISE EXCEPTION 'licence_country_ceilings: only effective_to may change after insert';
    END IF;
    IF OLD.effective_to IS NOT NULL THEN
        RAISE EXCEPTION 'licence_country_ceilings: a closed version may not be reopened or re-closed';
    END IF;
    IF NEW.effective_to IS NULL THEN
        RAISE EXCEPTION 'licence_country_ceilings: effective_to may not be cleared';
    END IF;
    RETURN NEW;
END; $$ LANGUAGE plpgsql;

CREATE TRIGGER licence_country_ceilings_immutable
    BEFORE UPDATE OR DELETE ON licence_country_ceilings
    FOR EACH ROW EXECUTE FUNCTION licence_country_ceilings_enforce_append_only();
CREATE TRIGGER licence_country_ceilings_deny_truncate
    BEFORE TRUNCATE ON licence_country_ceilings
    FOR EACH STATEMENT EXECUTE FUNCTION licence_country_ceilings_enforce_append_only();

-- --- RLS: explicitly NOT `USING (true)` (ADR 0045 §7.2) ---

ALTER TABLE licence_country_ceilings ENABLE ROW LEVEL SECURITY;
ALTER TABLE licence_country_ceilings FORCE ROW LEVEL SECURITY;

-- SELECT: platform admin, OR a tenant-scoped connection whose OWN tenant
-- is bound to THIS licence (composite ownership check). Deliberately
-- NARROWER than migration 0075's `FOR SELECT USING (true)`: a BYOL
-- tenant's own licence ceiling must not be readable by an unrelated
-- tenant. The EXISTS works from any scope because `tenants` carries no
-- RLS (verified). Required (not merely permitted) because the write-time
-- ceiling trigger runs under the TENANT's own connection (triggers here
-- are not SECURITY DEFINER, deliberately, per PHASE-B-ARCH-1's rejection
-- of an RLS-bypass surface) and the read-time resolve path runs
-- tenant/brand-scoped - without this policy both would see zero rows and
-- fail closed with a MISLEADING cause.
CREATE POLICY licence_country_ceilings_read ON licence_country_ceilings
    FOR SELECT USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            (
                NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
                AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
            )
            OR EXISTS (
                SELECT 1 FROM tenants t
                 WHERE t.id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
                   AND t.licence_id = licence_country_ceilings.licence_id
            )
        )
    );

CREATE POLICY licence_country_ceilings_platform_insert ON licence_country_ceilings
    FOR INSERT WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
CREATE POLICY licence_country_ceilings_platform_close ON licence_country_ceilings
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
-- No DELETE policy. No FOR ALL policy. No player policy of any kind.

COMMENT ON TABLE licence_country_ceilings IS 'The platform-wide, append-only, effective-dated ceiling a LICENCE places on which countries may ever be enabled beneath it (Stage 4I Phase E). Not a column on `licences` (a ceiling is versioned; licences is flat current-state) and not a row in operating_country_policies (a different table, key, RLS posture and write permission is what makes "licence is a ceiling, not a link in an inheritance chain" structurally true, not merely documentary). licences.permitted_markets is DEPRECATED and non-authoritative as of this migration - this table is the sole authoritative expression of a licence''s permitted countries. Zero rows exist as of this migration.';

-- ======================================================================
-- 4. operating_country_policies (ADR 0045 §3) - tenant/brand/operation
-- ======================================================================

CREATE TABLE operating_country_policies (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- ALWAYS tenant-owned. There is no platform-wide (NULL tenant_id) row
    -- shape here at all: a platform-wide statement about a country is a
    -- licence_country_ceilings row, never a row in this table.
    tenant_id               UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    -- WHICH RUNG this row is. asset_authorizations' scope_kind
    -- discriminator pattern (migration 0045), applied verbatim.
    scope_kind              TEXT NOT NULL CHECK (scope_kind IN ('tenant', 'brand', 'operation')),
    brand_id                UUID,
    operation_code          TEXT REFERENCES platform_operations (code),
    -- NULL = every product. A product-specific row may only NARROW a
    -- broader (NULL-product) row's effect - never widen past it. In
    -- particular, a product-specific ENABLED row can never resolve
    -- `permitted` while a broader in-force DISABLED row applies to it
    -- (ADR 0045 §3.5-A AMENDMENT-1): the write-time trigger
    -- (operating_country_policies_enforce_ceiling, step 4) refuses to
    -- create such a row, and the resolver (internal/operatingmarket
    -- resolve.go) treats the operation rung as a SET evaluated with
    -- first-disabled-wins, not most-specific-wins, so even a row that
    -- bypassed the trigger (e.g. raw SQL) cannot unmask a broader disable.
    product_code            TEXT REFERENCES platform_products (code),
    country_code            TEXT NOT NULL CHECK (country_code ~ '^[A-Z]{2}$'),
    state                   TEXT NOT NULL CHECK (state IN ('enabled', 'disabled')),
    status                  TEXT NOT NULL CHECK (status IN ('active', 'withdrawn')),
    authorization_reference TEXT CHECK (authorization_reference IS NULL OR btrim(authorization_reference) <> ''),
    reason_code             TEXT NOT NULL CHECK (btrim(reason_code) <> ''),
    policy_version          TEXT NOT NULL,
    effective_from          TIMESTAMPTZ NOT NULL DEFAULT now(),
    effective_to            TIMESTAMPTZ,
    created_by_actor_type   TEXT NOT NULL CHECK (created_by_actor_type IN ('staff', 'system')),
    created_by_actor_id     UUID NOT NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- EXACTLY ONE LEGAL SHAPE PER scope_kind - migration 0045's own
    -- three-CHECK device, extended to four columns.
    CONSTRAINT ocp_shape_tenant CHECK (scope_kind <> 'tenant'
        OR (brand_id IS NULL AND operation_code IS NULL AND product_code IS NULL)),
    CONSTRAINT ocp_shape_brand CHECK (scope_kind <> 'brand'
        OR (brand_id IS NOT NULL AND operation_code IS NULL AND product_code IS NULL)),
    -- Operation rows are an INDEPENDENT dimension: brand_id is OPTIONAL
    -- here, so a tenant-wide operation policy is expressible without
    -- naming a brand.
    CONSTRAINT ocp_shape_operation CHECK (scope_kind <> 'operation'
        OR operation_code IS NOT NULL),

    CONSTRAINT ocp_effective_to_after_from
        CHECK (effective_to IS NULL OR effective_to > effective_from),
    CONSTRAINT ocp_enable_requires_authorization
        CHECK (NOT (status = 'active' AND state = 'enabled') OR authorization_reference IS NOT NULL),
    CONSTRAINT ocp_withdrawn_is_disabled
        CHECK (status <> 'withdrawn' OR state = 'disabled'),

    -- ADR 0045 §3.5-A AMENDMENT-2 (finding SEC-E-REV-1). At the BRAND and
    -- OPERATION rungs, absence INHERITS from the rung above (ruling 6,
    -- re-confirmed UNCHANGED by AMENDMENT-2). A withdrawal there is
    -- therefore NOT the fail-closed direction: it removes this rung's
    -- opinion and lets the parent's decision through, so withdrawing an
    -- in-force active+disabled row at these two rungs is functionally an
    -- ENABLE and can flip a resolution from disabled_by_brand /
    -- disabled_by_operation to permitted. It carries the ENABLE
    -- direction's recorded-authorization requirement.
    --
    -- THE KILL-SWITCH ASYMMETRY (ADR 0037 §C.5.3, ADR 0045 §7.1 bullet 4)
    -- IS UNTOUCHED: writing state='disabled', status='active' - the
    -- emergency kill-switch - still requires NO authorization_reference,
    -- at any scope, in any order. A withdrawal is not a disable; it is the
    -- REMOVAL of one. Do not generalize this constraint to cover disables.
    --
    -- SCOPE IS DELIBERATELY NARROW AND IS DERIVED, NOT ARBITRARY: the
    -- TENANT rung is EXCLUDED because absence there is TERMINAL
    -- (not_configured, never permitted - resolve.go STEP 2), so a
    -- tenant-rung withdrawal can only ever narrow. licence_country_ceilings
    -- is excluded for the same kind of reason (its absence/withdrawal is
    -- not_permitted_by_licence). IF EITHER OF THOSE ABSENCE SEMANTICS EVER
    -- CHANGES, THIS CONSTRAINT MUST BE WIDENED IN THE SAME CHANGE - fenced
    -- by TestOperatingCountryPolicy_TenantRungWithdrawalIsTerminalSoNeedsNoAuthorization
    -- and TestLicenceCountryCeiling_WithdrawalIsFailClosedAndNeedsNoAuthorization.
    --
    -- A CHECK, not a step in operating_country_policies_enforce_ceiling(),
    -- for three independent reasons: (a) by the time that BEFORE INSERT
    -- trigger runs, the writer has ALREADY closed the predecessor version
    -- in the same transaction, so the trigger cannot see the row being
    -- withdrawn via `effective_to IS NULL` at all; (b) this is a pure
    -- per-row predicate with no cross-row lookup, so it is race-free by
    -- construction under READ COMMITTED, unlike every lookup-based step in
    -- that trigger (SEC-E-REV-3); (c) session_replication_role='replica'
    -- and ALTER TABLE ... DISABLE TRIGGER both disable triggers but NEITHER
    -- disables a check constraint.
    CONSTRAINT ocp_inherit_rung_withdrawal_requires_authorization
        CHECK (NOT (status = 'withdrawn' AND scope_kind IN ('brand', 'operation'))
               OR authorization_reference IS NOT NULL),

    -- Composite FK: a row can NEVER name a different tenant's brand
    -- (migration 0041/0045/0071's unanimous precedent).
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id)
);

-- Four partial unique indexes - one open version per key per rung. Two
-- indexes for the operation rung rather than a COALESCE(brand_id, sentinel)
-- because a NULL-UUID sentinel is a value a real column could
-- theoretically hold and the two-index form has no such ambiguity. If
-- these indexes' key columns ever change,
-- operating_country_policies_enforce_close_successor()'s key-match
-- predicate must change in the same commit.
CREATE UNIQUE INDEX uq_ocp_open_tenant
    ON operating_country_policies (tenant_id, country_code)
    WHERE scope_kind = 'tenant' AND effective_to IS NULL;

CREATE UNIQUE INDEX uq_ocp_open_brand
    ON operating_country_policies (tenant_id, brand_id, country_code)
    WHERE scope_kind = 'brand' AND effective_to IS NULL;

-- COALESCE(product_code, '*') because NULLs are not equal to each other
-- in a unique index, which would otherwise allow two contradictory
-- "every product" rows (migration 0045's own words, same fix).
CREATE UNIQUE INDEX uq_ocp_open_operation_tenantwide
    ON operating_country_policies (tenant_id, country_code, operation_code, COALESCE(product_code, '*'))
    WHERE scope_kind = 'operation' AND brand_id IS NULL AND effective_to IS NULL;

CREATE UNIQUE INDEX uq_ocp_open_operation_brand
    ON operating_country_policies (tenant_id, brand_id, country_code, operation_code, COALESCE(product_code, '*'))
    WHERE scope_kind = 'operation' AND brand_id IS NOT NULL AND effective_to IS NULL;

CREATE INDEX idx_ocp_lookup
    ON operating_country_policies (tenant_id, country_code, scope_kind, effective_from DESC);

-- --- Triggers ---

CREATE FUNCTION operating_country_policies_stamp_times() RETURNS TRIGGER AS $$
BEGIN
    NEW.effective_from := now();
    NEW.created_at     := now();
    NEW.effective_to   := NULL;
    RETURN NEW;
END; $$ LANGUAGE plpgsql;
CREATE TRIGGER operating_country_policies_stamp_times
    BEFORE INSERT ON operating_country_policies
    FOR EACH ROW EXECUTE FUNCTION operating_country_policies_stamp_times();

-- Append-only, WITH ONE DELIBERATE ASYMMETRY vs. licence_country_ceilings
-- (ADR 0045 §7.3): this table declares `tenant_id ... ON DELETE CASCADE`,
-- and PostgreSQL runs referential-integrity actions with RLS bypassed, so
-- deleting a tenant still removes its rows (migration 0047's own recorded
-- reasoning for why asset_authorizations gets no BEFORE DELETE trigger
-- while assets does). A full deny-DELETE trigger here WOULD block that
-- cascade. RULING: the DELETE arm is OMITTED on this table; protection is
-- the absence of a DELETE policy alone (an application DELETE sees zero
-- rows and removes nothing). licence_country_ceilings has no cascade (it
-- references licences(id) with no cascade) and therefore DOES get the
-- full deny-DELETE trigger above. This asymmetry is deliberate and
-- established precedent - "fixing" it to be symmetric would break tenant
-- deletion.
--
-- ADR 0045 §18 (finding F2, documentation-only disclosure): the
-- consequence is that the REMOVAL direction on this table is wholly
-- uncontrolled for any role that bypasses RLS - no trigger, no CHECK, no
-- audit record. This is a DISCLOSED RESIDUAL, not an oversight. The
-- genuine fix is the migration-owner/runtime-role separation recorded in
-- ADR 0026, a platform-wide operational change out of this stage's scope.
CREATE FUNCTION operating_country_policies_enforce_append_only() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'operating_country_policies is append-only: TRUNCATE is not permitted';
    END IF;
    -- NOTE: this function is deliberately wired to BEFORE UPDATE only (not
    -- BEFORE UPDATE OR DELETE) - see the trigger definition's own comment
    -- above for why the DELETE arm is omitted on THIS table. TG_OP is
    -- therefore always 'UPDATE' or 'TRUNCATE' here, never 'DELETE'.
    IF OLD.effective_to IS NULL AND NEW.effective_to IS NOT NULL THEN
        NEW.effective_to := now();
    END IF;
    IF (to_jsonb(NEW) - 'effective_to') IS DISTINCT FROM (to_jsonb(OLD) - 'effective_to') THEN
        RAISE EXCEPTION 'operating_country_policies: only effective_to may change after insert';
    END IF;
    IF OLD.effective_to IS NOT NULL THEN
        RAISE EXCEPTION 'operating_country_policies: a closed version may not be reopened or re-closed';
    END IF;
    IF NEW.effective_to IS NULL THEN
        RAISE EXCEPTION 'operating_country_policies: effective_to may not be cleared';
    END IF;
    RETURN NEW;
END; $$ LANGUAGE plpgsql;

CREATE TRIGGER operating_country_policies_immutable
    BEFORE UPDATE ON operating_country_policies
    FOR EACH ROW EXECUTE FUNCTION operating_country_policies_enforce_append_only();
CREATE TRIGGER operating_country_policies_deny_truncate
    BEFORE TRUNCATE ON operating_country_policies
    FOR EACH STATEMENT EXECUTE FUNCTION operating_country_policies_enforce_append_only();

-- The write-time ceiling-narrowing trigger. Reuses
-- asset_operation_eligibility_enforce_narrowing()'s exact mechanism:
-- early return on the fail-closed direction, a SELECT...INTO of the
-- parent fact with IF NOT FOUND OR NOT <parent enabled> THEN RAISE, and a
-- RAISE EXCEPTION message naming the rule so classifyTriggerError-style
-- Go code can map it to a sentinel by substring.
--
-- PLACEMENT: BEFORE, not AFTER. Migration 0047's comment establishes that
-- AFTER is mandatory when the write is an upsert (a BEFORE INSERT trigger
-- fires twice on INSERT ... ON CONFLICT DO UPDATE). Phase E has NO upsert
-- anywhere (internal/operatingmarket never issues one), so BEFORE INSERT
-- fires exactly once and is correct. Do not "fix" this to AFTER.
CREATE FUNCTION operating_country_policies_enforce_ceiling() RETURNS TRIGGER AS $$
DECLARE
    v_licence_id      UUID;
    v_ceiling_state   TEXT;
    v_tenant_state    TEXT;
    v_brand_state     TEXT;
BEGIN
    IF NEW.state = 'disabled' OR NEW.status = 'withdrawn' THEN
        -- Narrowing-or-neutral AS FAR AS THIS FUNCTION'S UPWARD CEILING
        -- CHECKS GO: none of them can be violated by a row that blocks
        -- (disabled) or asserts nothing (withdrawn).
        --
        -- THIS IS NOT A CLAIM THAT SUCH A WRITE CANNOT WIDEN. A WITHDRAWAL
        -- AT THE BRAND OR OPERATION RUNG DOES WIDEN - absence at those
        -- rungs inherits, so removing an in-force disable there lets the
        -- parent's permit through (ADR 0045 §3.5-A AMENDMENT-2, finding
        -- SEC-E-REV-1). That case is controlled by the CHECK constraint
        -- ocp_inherit_rung_withdrawal_requires_authorization, evaluated
        -- independently of this function. Do NOT re-broaden this comment to
        -- "fail-closed direction, always allowed" - that claim was false
        -- and was the defect.
        RETURN NEW;
    END IF;

    -- (1) THE CEILING. Evaluated FIRST, unconditionally (Phase D.1 ruling 2).
    SELECT t.licence_id INTO v_licence_id FROM tenants t WHERE t.id = NEW.tenant_id;
    IF NOT FOUND OR v_licence_id IS NULL THEN
        RAISE EXCEPTION 'operating_country_policies: tenant % has no licence bound; no country may be enabled for it (Stage 4I Phase E, licence ceiling)', NEW.tenant_id;
    END IF;

    SELECT c.state INTO v_ceiling_state
      FROM licence_country_ceilings c
     WHERE c.licence_id = v_licence_id
       AND c.country_code = NEW.country_code
       AND c.status = 'active'
       AND c.effective_to IS NULL
     LIMIT 1;
    IF NOT FOUND OR v_ceiling_state <> 'enabled' THEN
        RAISE EXCEPTION 'operating_country_policies: country % is not permitted by the licence ceiling for licence %; a tenant/brand/operation policy may only narrow within the ceiling and may never exceed it (Stage 4I Phase E)', NEW.country_code, v_licence_id;
    END IF;

    IF NEW.scope_kind = 'tenant' THEN
        RETURN NEW;
    END IF;

    -- (2) The tenant rung must EXIST and be enabled. Absence at tenant
    -- scope is terminal (ruling 5) - so a brand/operation enable above an
    -- absent tenant row could never resolve permitted, and is refused at
    -- write time. This is asset_authorizations_enforce_narrowing()'s own
    -- brand rule.
    SELECT p.state INTO v_tenant_state
      FROM operating_country_policies p
     WHERE p.tenant_id = NEW.tenant_id AND p.scope_kind = 'tenant'
       AND p.country_code = NEW.country_code
       AND p.status = 'active' AND p.effective_to IS NULL
     LIMIT 1;
    IF NOT FOUND OR v_tenant_state <> 'enabled' THEN
        RAISE EXCEPTION 'operating_country_policies: a % policy for country % cannot be enabled before/beyond the tenant-scope policy it narrows (Stage 4I Phase E)', NEW.scope_kind, NEW.country_code;
    END IF;

    -- (3) WRITE-TIME NARROWING ENFORCEMENT (ADR 0045 §3.5-A AMENDMENT-1).
    -- A more-specific operation/product row must never be able to widen
    -- past a broader, in-force, active DISABLED operation row for the
    -- same operation. This is enforced here IN ADDITION TO (not instead
    -- of) the resolver's own first-disabled-wins rewrite (resolve.go) -
    -- the resolver alone is correct even if this trigger is bypassed
    -- (e.g. raw SQL), but this trigger keeps the sanctioned write path
    -- from ever recording a self-contradictory configuration in the
    -- first place.
    --
    -- PLACEMENT, LOAD-BEARING: this check must run for EVERY
    -- scope_kind='operation' row regardless of whether NEW.brand_id is
    -- NULL - it is placed HERE, immediately after the tenant-rung check
    -- and BEFORE the brand-check block's own early
    -- `IF NEW.scope_kind = 'brand' OR NEW.brand_id IS NULL THEN RETURN
    -- NEW` below. Placing it after that block (textually closer to the
    -- final RETURN NEW) would make it structurally unreachable for every
    -- tenant-wide (brand_id IS NULL) operation write - exactly the most
    -- common shape - because that branch returns before ever reaching
    -- code below it. Caught by
    -- TestOperatingCountryPolicy_MoreSpecificEnableUnderBroaderDisableIsRefused's
    -- "product-specific under every-product disable" sub-case actually
    -- failing against the first (wrongly-placed) version of this trigger.
    IF NEW.scope_kind = 'operation' THEN
        PERFORM 1
          FROM operating_country_policies d
         WHERE d.tenant_id = NEW.tenant_id
           AND d.scope_kind = 'operation'
           AND d.country_code = NEW.country_code
           AND d.operation_code = NEW.operation_code
           AND d.status = 'active'
           AND d.state = 'disabled'
           AND d.effective_to IS NULL
           AND (d.brand_id IS NULL OR d.brand_id = NEW.brand_id)
           AND (d.product_code IS NULL OR d.product_code = NEW.product_code);
        IF FOUND THEN
            RAISE EXCEPTION 'operating_country_policies: an operation policy for country % cannot be enabled while a broader in-force operation policy for the same operation is disabled; a more-specific operation/product row may only narrow and may never widen (Stage 4I Phase E, ADR 0045 §3.5-A)', NEW.country_code;
        END IF;
    END IF;

    IF NEW.scope_kind = 'brand' OR NEW.brand_id IS NULL THEN
        RETURN NEW;
    END IF;

    -- (4) Brand-specific operation row: if a brand rung exists it must be
    -- enabled; if it is absent it legitimately INHERITS the tenant rung,
    -- already verified enabled above (ruling 6).
    SELECT p.state INTO v_brand_state
      FROM operating_country_policies p
     WHERE p.tenant_id = NEW.tenant_id AND p.scope_kind = 'brand'
       AND p.brand_id = NEW.brand_id AND p.country_code = NEW.country_code
       AND p.status = 'active' AND p.effective_to IS NULL
     LIMIT 1;
    IF FOUND AND v_brand_state <> 'enabled' THEN
        RAISE EXCEPTION 'operating_country_policies: an operation policy for country % cannot be enabled under a disabled brand policy (Stage 4I Phase E)', NEW.country_code;
    END IF;

    RETURN NEW;
END; $$ LANGUAGE plpgsql;

CREATE TRIGGER operating_country_policies_ceiling
    BEFORE INSERT ON operating_country_policies
    FOR EACH ROW EXECUTE FUNCTION operating_country_policies_enforce_ceiling();

-- ADR 0045 §3.5-A AMENDMENT-3 (finding SEC-E-REV-2, "the bare close").
-- AMENDMENT-2 fenced the INSERT-shaped way of removing a live block at an
-- inheriting rung (write a `withdrawn` successor). It did NOT fence the
-- UPDATE-shaped way: setting `effective_to` on the open row and writing NO
-- successor at all. resolve() windows every rung on
-- `effective_from <= asOf AND (effective_to IS NULL OR effective_to > asOf)`,
-- so a closed row simply leaves the window - byte-for-byte the same effect
-- as a withdrawal, reached by a different verb. Every other control on this
-- table is INSERT-shaped (both CHECKs above, operating_country_policies_
-- enforce_ceiling(), and the Go audit computation in policy_admin.go), and
-- operating_country_policies_enforce_append_only() does not gate the close
-- at all - it BLESSES it (it forces effective_to := now() to prevent
-- backdating, and has no opinion on whether the close should occur).
--
-- WHY REQUIRING A SUCCESSOR IS SUFFICIENT, AND WHY THIS TRIGGER
-- DELIBERATELY DOES NOT RE-IMPLEMENT AN AUTHORIZATION TEST: every legal
-- successor shape is either non-widening or ALREADY gated -
--   active+disabled    -> the block persists; no widening.
--   active+enabled     -> ocp_enable_requires_authorization.
--   withdrawn+disabled -> ocp_inherit_rung_withdrawal_requires_authorization
--                         (AMENDMENT-2).
--   withdrawn+enabled  -> forbidden by ocp_withdrawn_is_disabled.
-- The successor set is closed by those three CHECKs. AMENDMENT-3 forces the
-- VERB through the SHAPES that AMENDMENT-2 fences; the two compose and
-- neither is redundant.
--
-- WHY A CONSTRAINT TRIGGER AND NOT A CHECK (AMENDMENT-2's mechanism): the
-- predicate is inherently CROSS-ROW - it compares the closed row's key
-- against whatever, if anything, replaced it - which a CHECK cannot express
-- at all. §17's argument that a CHECK is harder to bypass than a trigger
-- still stands and is NOT withdrawn; it simply does not decide this case,
-- because (a) no CHECK formulation exists, and (b) any actor able to
-- disable triggers is also able to defeat AMENDMENT-2's CHECK more cheaply
-- (with the append-only trigger disabled, one UPDATE can set
-- status='withdrawn' AND forge authorization_reference in the same
-- statement). Trigger-disabling capability is outside BOTH amendments'
-- threat model - see ADR 0026's recorded migration-owner/runtime-role
-- residual, the genuine fix, which is out of this stage's scope.
--
-- DEFERRABLE INITIALLY DEFERRED IS LOAD-BEARING, NOT DECORATION. The
-- sanctioned writer (CreateOperatingCountryPolicyVersion) closes the
-- predecessor and THEN inserts the successor, both in ONE transaction; an
-- immediate check would reject the only legitimate write path. A
-- transaction that issues `SET CONSTRAINTS ALL IMMEDIATE` before that
-- writer will fail - fail-CLOSED (the write is rejected; nothing widens) -
-- so this is a liveness hazard, not a safety one. Do not add such a call.
--
-- SCOPE IS DERIVED, NOT ARBITRARY - the SAME derivation as AMENDMENT-2's.
-- Only rows whose disappearance WIDENS are covered:
--   * scope_kind IN ('brand','operation') ONLY. A TENANT-rung bare close
--     resolves to `policy_expired`/`not_configured` (absence is TERMINAL,
--     ruling 5) and a bare close on licence_country_ceilings resolves to
--     `not_permitted_by_licence`; both NARROW. IF EITHER ABSENCE SEMANTIC
--     EVER CHANGES, THIS TRIGGER MUST BE WIDENED IN THE SAME CHANGE -
--     fenced by TestOperatingCountryPolicy_TenantRungBareCloseNarrowsSoIsPermitted
--     and TestLicenceCountryCeiling_BareCloseIsFailClosedSoIsPermitted.
--   * status='active' AND state='disabled' ONLY. Closing an `enabled` or an
--     already-`withdrawn` row removes nothing that was blocking.
--   * AFTER UPDATE ONLY, NEVER DELETE - the DELETE arm is omitted for
--     exactly the reason operating_country_policies_enforce_append_only()
--     omits it (tenants' ON DELETE CASCADE, §7.3). A DELETE by an
--     RLS-bypassing role remains an uncontrolled removal path: a DISCLOSED
--     RESIDUAL (ADR 0045 §18 finding F2), not an oversight. It requires
--     bypassing RLS entirely - a strictly higher bar than the ordinary
--     tenant-scoped DML this trigger closes.
--
-- The successor lookup does not need `s.id <> OLD.id`: OLD.effective_to is
-- non-NULL by the time this fires, and enforce_append_only() refuses to
-- reopen a closed version, so OLD can never satisfy `effective_to IS NULL`.
-- The function is deliberately NOT SECURITY DEFINER: FORCE ROW LEVEL
-- SECURITY applies to the owner anyway, so it would buy nothing and would
-- add a search_path hazard. Its lookup runs under
-- operating_country_policies_tenant_read, which is correct: OLD.tenant_id
-- necessarily equals app.tenant_id (the UPDATE policy's USING enforced it),
-- and an RLS-bypassing closer's lookup bypasses RLS identically.
CREATE FUNCTION operating_country_policies_enforce_close_successor()
RETURNS TRIGGER AS $$
BEGIN
    PERFORM 1
      FROM operating_country_policies s
     WHERE s.tenant_id      =                    OLD.tenant_id
       AND s.scope_kind     =                    OLD.scope_kind
       AND s.country_code   =                    OLD.country_code
       AND s.brand_id       IS NOT DISTINCT FROM OLD.brand_id
       AND s.operation_code IS NOT DISTINCT FROM OLD.operation_code
       AND s.product_code   IS NOT DISTINCT FROM OLD.product_code
       AND s.effective_to IS NULL;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'operating_country_policies: closing an in-force disabled %-scope policy for country % without an open successor version in the same transaction is not permitted: absence at this rung inherits from the rung above, so a bare close is itself a widening act (Stage 4I Phase E, ADR 0045 §3.5-A AMENDMENT-3, ocp_inherit_rung_close_requires_successor)', OLD.scope_kind, OLD.country_code;
    END IF;
    RETURN NULL;
END; $$ LANGUAGE plpgsql;

CREATE CONSTRAINT TRIGGER ocp_inherit_rung_close_requires_successor
    AFTER UPDATE ON operating_country_policies
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    WHEN (OLD.effective_to IS NULL
          AND NEW.effective_to IS NOT NULL
          AND OLD.scope_kind IN ('brand', 'operation')
          AND OLD.status = 'active'
          AND OLD.state  = 'disabled')
    EXECUTE FUNCTION operating_country_policies_enforce_close_successor();

-- --- RLS: own tenant only, no platform-wide read (ADR 0045 §7.3) ---

ALTER TABLE operating_country_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE operating_country_policies FORCE ROW LEVEL SECURITY;

-- The leading player_account_id-IS-NULL conjunct is mandatory and is not
-- decoration: without it, a player-scoped connection (which sets BOTH
-- GUCs) would also satisfy a plain tenant match and could read or write
-- policy rows (migration 0045's asset_authorizations shape).
CREATE POLICY operating_country_policies_tenant_read ON operating_country_policies
    FOR SELECT USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
CREATE POLICY operating_country_policies_tenant_insert ON operating_country_policies
    FOR INSERT WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
CREATE POLICY operating_country_policies_tenant_close ON operating_country_policies
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
-- No DELETE policy. No FOR ALL policy. No player-read policy.
-- No platform-admin read policy, DELIBERATELY: a tenant's own operating
-- footprint is commercially sensitive and is not platform-readable
-- (ADR 0045 §7.3) - a future operational need for that is a security-owned
-- decision requiring its own ruling, not a quiet policy addition.

COMMENT ON TABLE operating_country_policies IS 'The tenant/brand/operation-scoped, append-only, effective-dated statement of whether a tenant actually operates in a country, narrowing (never exceeding) licence_country_ceilings (Stage 4I Phase E). scope_kind discriminates which independent rung a row answers (tenant/brand/operation - asset_authorizations'' scope_kind pattern, migration 0045). Absence at TENANT scope is terminal (not_configured - a licence GRANTS permission, it does not INSTRUCT); absence at BRAND/OPERATION scope INHERITS the rung above - which is precisely why a WITHDRAWAL at those two rungs requires an authorization_reference (ocp_inherit_rung_withdrawal_requires_authorization): at an inheriting rung a withdrawal is a widening act, not a fail-closed one. Resolution is internal/operatingmarket.ResolveOperatingCountryPolicy, a NEW package structurally separate from internal/jurisdiction (player-jurisdiction resolution) - this table has no player-evidence dimension of any kind and cannot acquire one without a schema change. No resolution is ever cached or persisted (see internal/operatingmarket''s own package doc). Zero rows exist as of this migration.';

-- ======================================================================
-- 5. licences.permitted_markets - deprecated in place (ADR 0045 §2.6)
-- ======================================================================
-- RETAIN + DEPRECATE IN PLACE NOW; remove later under the named trigger
-- MKT-PM-1 (docs/governance/task-registry.md). The column is
-- JSONB NOT NULL DEFAULT '[]', written only by CreateLicence, read only
-- by ListLicences, has ZERO enforcement readers, and its content is
-- HDR-J-6-blocked ("Not yet determined") - there is nothing to migrate.
-- licence_country_ceilings becomes the SOLE authoritative expression of a
-- licence's permitted countries; no authorization path may read this
-- column.

COMMENT ON COLUMN licences.permitted_markets IS 'DEPRECATED (Stage 4I Phase E) and NON-AUTHORITATIVE. Descriptive free-form metadata recording what the paper licence says; it has never had a defined value vocabulary and has ZERO enforcement readers. The authoritative, validated, effective-dated, append-only expression of a licence''s permitted countries is licence_country_ceilings. No authorization path may read this column. Scheduled for removal by the phase that answers HDR-J-6 and populates real ceiling content (task registry item MKT-PM-1).';
