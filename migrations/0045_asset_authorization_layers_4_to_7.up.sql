-- Stage 4H-B0-R6, Workstream A (architect). Implements ADR 0037 Part A
-- LAYERS 4-7 (tenant / brand / jurisdiction authorization and per-product
-- operation eligibility) as genuinely independent, separately-queryable
-- facts, and adds the product/vertical dimension ADR 0037's own open
-- question 7 flagged as missing. Layers 1-3 are migration 0044.
--
-- ======================================================================
-- SCHEMA CHOICE AND ITS TRADEOFFS (required by this workstream's
-- dispatch, recorded here rather than only in the ADR)
-- ======================================================================
--
-- Two findings from Stage 4H-B0-R5's independent reviews force a change
-- to what ADR 0037 §A.5 originally specified:
--
--   * `qa` found layers 4 (tenant) and 6 (jurisdiction) cannot be
--     independently tested, because §A.5 resolves BOTH from a single
--     `tenant_jurisdiction_configs.allowed_currencies` JSONB array on one
--     `(tenant_id, jurisdiction_id)` row. One row cannot produce two
--     distinguishable per-layer ReasonCodes, which §C.2 explicitly
--     promises. The ADR's own words ("layers 4 and 6 are the same
--     underlying mechanism") are the defect, not the implementation of
--     them.
--   * `sportsbook` found no product/vertical dimension exists anywhere in
--     the eligibility surface, so "BTC is wagering-eligible for casino but
--     not for sportsbook in jurisdiction X" is inexpressible - while the
--     platform already models exactly that distinction one layer away, in
--     `licences.permitted_products` (migration 0002, doc 15). Licences are
--     product-specific in the real world; an eligibility surface that
--     cannot express it is not usable for a multi-product tenant.
--
-- OPTION 1 - extend `tenant_jurisdiction_configs`. Rejected. Adding a
-- product axis to a JSONB array of currency codes means encoding a
-- three-dimensional fact (asset x product x eligibility) inside one JSONB
-- column on a row whose grain is (tenant, jurisdiction, effective_from).
-- It cannot give layer 4 a separate fact from layer 6 at all (that is
-- precisely `qa`'s finding - the grain of the row IS the conflation), it
-- has no FK to assets(code), it cannot carry per-fact reason_code/actor/
-- timestamps that CLAUDE.md's audit rule requires per mutation, and each
-- eligibility toggle would rewrite an array shared with unrelated
-- configuration. The effective_from/effective_to versioning on that table
-- would also mean every asset toggle mints a new configuration version of
-- the tenant's whole jurisdiction ruleset.
--
-- OPTION 2 - one new fact table per layer (four tables). Rejected as
-- over-normalized: four near-identical tables, four sets of RLS policies
-- and triggers, with no query that benefits.
--
-- OPTION 3 - CHOSEN. Two tables, split on the boundary ADR 0037 itself
-- already draws between "does this scope offer this asset at all"
-- (layers 4-6, §A.5) and "for which operation" (layer 7, §A.6, which the
-- ADR already specifies as its own concept with its own shape):
--
--   * `asset_authorizations`  - layers 4/5/6. One row per
--     (tenant, scope_kind, scope target, asset, product). `scope_kind`
--     discriminates which layer the row IS, so each layer is an
--     independently present-or-absent, independently queryable,
--     independently deniable fact with its own ReasonCode. A tenant row
--     and a jurisdiction row for the same asset are two different rows -
--     `qa`'s finding closed structurally, not by convention.
--   * `asset_operation_eligibility` - layer 7, per ADR 0037 §A.6's own
--     specified shape (asset_code, operation, eligible, tenant_id NULL =
--     platform-wide default, reason_code, audit columns), extended with
--     the product dimension.
--
-- Tradeoffs of option 3, stated honestly:
--   - `scope_kind` plus three nullable scope columns makes the table
--     sparse. Mitigated by three CHECK constraints that make exactly one
--     shape legal per scope_kind, so an ill-formed row cannot be stored
--     (the same posture migration 0041 takes with its limit_kind/
--     time_window CHECK pairing).
--   - The layer chain is a walk of up to four queries rather than one row
--     read. That is the intended cost of the layers being genuinely
--     independent; it is also what makes per-layer reason codes real.
--   - `tenant_jurisdiction_configs.allowed_currencies` is now redundant
--     for authorization purposes. It is deliberately NOT dropped or
--     migrated here: it has zero readers in Go today (verified by
--     grep across internal/ and cmd/), and rewriting a human-approved
--     Stage-1 configuration column is outside this workstream. It is
--     recorded in ADR 0037 §C.6 as a follow-up, and
--     AssetAuthorization.CheckEligibility deliberately does not read it -
--     two mechanisms answering one question is the drift risk §C.2
--     exists to prevent.
--
-- PRODUCT DIMENSION. `product` is a nullable FK to a new
-- `platform_products` registry table, never a CHECK-constrained closed
-- enum: adding a product must be a data change, the same way adding an
-- asset is (CLAUDE.md's "nothing brand-specific may become a code path",
-- generalized to products, and ADR 0037 §A.1's "must be data, looked up,
-- never a compiled-in switch"). NULL means "every product" - explicit
-- breadth, chosen deliberately, and it never widens a product-specific
-- denial because resolution takes the MOST SPECIFIC matching row.
-- `operation` stays a CHECK constraint because ADR 0037 §C.2 fixes that
-- list at exactly six values; widening it is an ADR decision plus a
-- migration, which is the intended friction.

-- ----------------------------------------------------------------------
-- 1. Product registry (the extensible product/vertical vocabulary)
-- ----------------------------------------------------------------------

CREATE TABLE platform_products (
    code         TEXT PRIMARY KEY,
    display_name TEXT NOT NULL,
    -- Defaults false for the same fail-closed reason assets.active does
    -- (migration 0044 / S-4): a product row that appears without a
    -- deliberate activation decision must not silently become a namable
    -- eligibility scope. This table is vocabulary, not an authorization
    -- gate - authorization is always asset_operation_eligibility's answer -
    -- but a fail-open default here would let a typo'd product code become
    -- usable.
    active       BOOLEAN NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

COMMENT ON TABLE platform_products IS 'Extensible product/vertical vocabulary for asset authorization (ADR 0037 open question 7). A new product is a row here, never a new code path or a widened CHECK constraint.';

-- Seeded with the three the dispatch names as the minimum, plus
-- 'payments' - which is not scope expansion but alignment: risk_rules
-- (migration 0041) already constrains its own product column to exactly
-- casino/sportsbook/payments/bonus, and having two different product
-- vocabularies on one platform would be a worse outcome than seeding the
-- fourth row. Values set explicitly, never left to the default.
INSERT INTO platform_products (code, display_name, active) VALUES
    ('casino',     'Casino',     true),
    ('sportsbook', 'Sportsbook', true),
    ('bonus',      'Bonus',      true),
    ('payments',   'Payments',   true);

ALTER TABLE platform_products ENABLE ROW LEVEL SECURITY;
ALTER TABLE platform_products FORCE ROW LEVEL SECURITY;

-- Read-open, platform-admin-write - identical rationale to migration
-- 0044's treatment of `assets` (platform-wide reference data read from
-- every scope; written only through db.Pool.WithPlatformAdmin).
CREATE POLICY platform_products_read ON platform_products
    FOR SELECT
    USING (true);

CREATE POLICY platform_products_platform_admin_write ON platform_products
    FOR ALL
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

-- ----------------------------------------------------------------------
-- 2. Layers 4/5/6 - asset_authorizations
-- ----------------------------------------------------------------------

CREATE TABLE asset_authorizations (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Always tenant-owned: every one of layers 4-6 is a tenant-scoped
    -- mutation per ADR 0037 §C.1's two-tier split, so there is no
    -- platform-wide (NULL tenant_id) row shape here at all. A
    -- platform-wide statement about an asset is layers 2-3 on the `assets`
    -- row itself, never a row in this table.
    tenant_id             UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    -- WHICH LAYER this row is. The discriminator is what makes layers 4
    -- and 6 independent facts rather than one conflated row (see the
    -- schema-choice block above).
    scope_kind            TEXT NOT NULL CHECK (scope_kind IN ('tenant', 'brand', 'jurisdiction')),
    brand_id              UUID,
    jurisdiction_id       UUID REFERENCES jurisdictions (id),
    asset_code            TEXT NOT NULL REFERENCES assets (code),
    -- NULL = applies to every product. A product-specific row always wins
    -- over the NULL row (most-specific-match), so this can narrow but
    -- never widen.
    product               TEXT REFERENCES platform_products (code),
    eligible              BOOLEAN NOT NULL,
    -- Why (ADR 0037 §A.6's reason_code, e.g. 'licence_excludes_product').
    reason_code           TEXT,
    created_by_actor_type TEXT NOT NULL CHECK (created_by_actor_type IN ('staff', 'system')),
    created_by_actor_id   UUID NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Exactly one legal shape per scope_kind - an ill-formed row (e.g. a
    -- 'tenant' row carrying a jurisdiction_id, which would be ambiguous
    -- about which layer it answers) cannot be stored at all.
    CHECK (scope_kind <> 'tenant' OR (brand_id IS NULL AND jurisdiction_id IS NULL)),
    CHECK (scope_kind <> 'brand' OR (brand_id IS NOT NULL AND jurisdiction_id IS NULL)),
    CHECK (scope_kind <> 'jurisdiction' OR (jurisdiction_id IS NOT NULL AND brand_id IS NULL)),
    -- Composite FK so a row can never name a DIFFERENT tenant's brand -
    -- migration 0041's own precedent, applied from the start.
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id)
);

-- One fact per (layer, scope target, asset, product). COALESCE(product,
-- '*') is used rather than a nullable column in the index key because
-- NULLs are not equal to each other in a unique index, which would allow
-- two contradictory "every product" rows.
CREATE UNIQUE INDEX idx_asset_authorizations_tenant_scope
    ON asset_authorizations (tenant_id, asset_code, COALESCE(product, '*'))
    WHERE scope_kind = 'tenant';
CREATE UNIQUE INDEX idx_asset_authorizations_brand_scope
    ON asset_authorizations (tenant_id, brand_id, asset_code, COALESCE(product, '*'))
    WHERE scope_kind = 'brand';
CREATE UNIQUE INDEX idx_asset_authorizations_jurisdiction_scope
    ON asset_authorizations (tenant_id, jurisdiction_id, asset_code, COALESCE(product, '*'))
    WHERE scope_kind = 'jurisdiction';

CREATE INDEX idx_asset_authorizations_lookup
    ON asset_authorizations (tenant_id, asset_code, scope_kind);

ALTER TABLE asset_authorizations ENABLE ROW LEVEL SECURITY;
ALTER TABLE asset_authorizations FORCE ROW LEVEL SECURITY;

-- Tenant isolation, with the leading "app.player_account_id IS NULL"
-- conjunct every staff/config table in this codebase carries for the
-- reason migration 0041's own policy comment spells out: without it a
-- player-scoped connection (which sets BOTH GUCs) would also satisfy a
-- plain tenant-match predicate and could read or write authorization
-- configuration. No player-facing path reads or writes this table.
CREATE POLICY tenant_isolation ON asset_authorizations
    FOR ALL
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

-- READ-ONLY for a player-scoped connection (db.WithPlayerScope), scoped
-- to that player's own tenant. This is required for correctness, not
-- convenience: a player-initiated financial operation (deposit, bet,
-- conversion) runs inside a WithPlayerScope transaction, and CLAUDE.md
-- requires the authoritative read to happen in the SAME transaction as
-- the write it authorizes. Without this policy, AssetAuthorization.
-- CheckEligibility would see zero rows on that path and deny everything -
-- fail-closed, but a FALSE denial, and one that would push a future
-- implementer toward evaluating eligibility in a separate transaction,
-- which is exactly the stale-read pattern the same-transaction rule
-- exists to prevent.
--
-- SELECT only. The write policy above still requires
-- app.player_account_id to be UNSET, so no player-facing code path can
-- create or change an authorization row - only observe which assets its
-- own tenant offers, which is information the player-facing product
-- surfaces anyway (it is the deposit/wager currency list).
CREATE POLICY player_read ON asset_authorizations
    FOR SELECT
    USING (
        NULLIF(current_setting('app.player_account_id', true), '')::uuid IS NOT NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

-- No DELETE policy, deliberately (on this table and on
-- asset_operation_eligibility below): revocation is `eligible = false`,
-- which leaves the fact, its reason_code, and its actor visible, never a
-- vanished row. With no DELETE policy, RLS gives a DELETE zero row
-- visibility, so one silently affects nothing instead of erasing
-- configuration history.

-- ----------------------------------------------------------------------
-- 3. Layer 7 - asset_operation_eligibility
-- ----------------------------------------------------------------------

CREATE TABLE asset_operation_eligibility (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- NULL = the platform-wide default for this asset/operation/product
    -- (ADR 0037 §A.6). Platform-wide rows are platform-admin-only and,
    -- when granting, dual-controlled at the API level (§C.5.1 op 9);
    -- tenant rows may only ever narrow them.
    tenant_id             UUID REFERENCES tenants (id) ON DELETE CASCADE,
    asset_code            TEXT NOT NULL REFERENCES assets (code),
    product               TEXT REFERENCES platform_products (code),
    -- ADR 0037 §C.2's exact six-value operation list. CHECK rather than a
    -- reference table on purpose: the ADR fixes this list, and widening it
    -- should require an ADR amendment plus a migration, not a data insert.
    operation             TEXT NOT NULL CHECK (operation IN (
        'deposit', 'withdrawal', 'wagering', 'settlement', 'conversion', 'reporting'
    )),
    eligible              BOOLEAN NOT NULL,
    reason_code           TEXT,
    created_by_actor_type TEXT NOT NULL CHECK (created_by_actor_type IN ('staff', 'system')),
    created_by_actor_id   UUID NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX idx_asset_operation_eligibility_platform
    ON asset_operation_eligibility (asset_code, operation, COALESCE(product, '*'))
    WHERE tenant_id IS NULL;
CREATE UNIQUE INDEX idx_asset_operation_eligibility_tenant
    ON asset_operation_eligibility (tenant_id, asset_code, operation, COALESCE(product, '*'))
    WHERE tenant_id IS NOT NULL;

ALTER TABLE asset_operation_eligibility ENABLE ROW LEVEL SECURITY;
ALTER TABLE asset_operation_eligibility FORCE ROW LEVEL SECURITY;

-- READ: a tenant-scoped connection sees its own rows PLUS every
-- platform-wide default (tenant_id IS NULL) - it has to, or the tenant's
-- effective answer could not be computed, and the narrow-only trigger
-- below could not check what it is narrowing. Same shape as migration
-- 0041's tenant_and_platform_read.
CREATE POLICY tenant_and_platform_read ON asset_operation_eligibility
    FOR SELECT
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            tenant_id IS NULL
            OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        )
    );

-- READ-ONLY for a player-scoped connection - same rationale as
-- asset_authorizations' own player_read policy above (a player-initiated
-- financial operation must be able to resolve layer 7 in the same
-- transaction as its write). Platform-wide defaults are visible too,
-- since the effective answer cannot be computed without them.
CREATE POLICY player_read ON asset_operation_eligibility
    FOR SELECT
    USING (
        NULLIF(current_setting('app.player_account_id', true), '')::uuid IS NOT NULL
        AND (
            tenant_id IS NULL
            OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        )
    );

-- WRITE: a tenant-scoped connection may write only its OWN rows. A
-- platform-wide default row requires genuine platform-admin scope - and
-- unlike migration 0041's equivalent policy, which accepts any
-- connection with app.tenant_id merely unset, this one additionally
-- requires app.platform_admin_principal_id, closing finding S-3's shape
-- for this table too rather than repeating the weaker precedent.
CREATE POLICY tenant_and_platform_write ON asset_operation_eligibility
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            (tenant_id IS NULL
                AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
                AND NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL)
            OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        )
    );

CREATE POLICY tenant_and_platform_update ON asset_operation_eligibility
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            (tenant_id IS NULL
                AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
                AND NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL)
            OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        )
    )
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            (tenant_id IS NULL
                AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
                AND NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL)
            OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        )
    );

-- ----------------------------------------------------------------------
-- 4. Narrow-only-never-widen, enforced at WRITE time
-- ----------------------------------------------------------------------
-- ADR 0037 §A.5/§A.6 state narrowing is one-directional. The resolution
-- chain in internal/assetregistry ANDs every layer, so a widened row can
-- never take effect at read time either - but `security`'s Stage
-- 4H-B0-R5 test list requires specifically that such a row be "rejected
-- at write time AND cannot take effect at resolution time". Both halves
-- are implemented; this is the write-time half.

CREATE FUNCTION asset_authorizations_enforce_narrowing() RETURNS TRIGGER AS $$
DECLARE
    v_asset_platform_authorized BOOLEAN;
    v_tenant_eligible           BOOLEAN;
BEGIN
    NEW.updated_at := now();

    IF NOT NEW.eligible THEN
        -- A denial never widens anything - always allowed, at any layer,
        -- in any order (this is the fail-closed direction, and ADR 0037
        -- §C.5.3's asymmetry applies for the same reason).
        RETURN NEW;
    END IF;

    -- Layers 4-6 can only ever narrow within what platform layers 1-3
    -- already authorized (§C.1). Granting for a tenant an asset the
    -- platform has not itself cleared is exactly the widening case.
    SELECT a.platform_authorized INTO v_asset_platform_authorized
      FROM assets a WHERE a.code = NEW.asset_code;
    IF NOT FOUND OR NOT v_asset_platform_authorized THEN
        RAISE EXCEPTION 'asset_authorizations: asset % is not platform-authorized (layer 3); a tenant/brand/jurisdiction authorization may only narrow within what the platform has already authorized (ADR 0037 C.1)', NEW.asset_code;
    END IF;

    -- A brand authorization (layer 5) narrows the tenant's own layer-4
    -- answer and can never exceed it (§A.5).
    IF NEW.scope_kind = 'brand' THEN
        SELECT t.eligible INTO v_tenant_eligible
          FROM asset_authorizations t
         WHERE t.tenant_id = NEW.tenant_id
           AND t.scope_kind = 'tenant'
           AND t.asset_code = NEW.asset_code
           AND (t.product = NEW.product OR t.product IS NULL)
         ORDER BY (t.product IS NOT NULL) DESC
         LIMIT 1;
        IF NOT FOUND OR NOT v_tenant_eligible THEN
            RAISE EXCEPTION 'asset_authorizations: brand authorization for asset % cannot be granted before/beyond the tenant-level authorization it narrows (ADR 0037 A.5)', NEW.asset_code;
        END IF;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER asset_authorizations_narrowing
    BEFORE INSERT OR UPDATE ON asset_authorizations
    FOR EACH ROW EXECUTE FUNCTION asset_authorizations_enforce_narrowing();

CREATE FUNCTION asset_operation_eligibility_enforce_narrowing() RETURNS TRIGGER AS $$
DECLARE
    v_asset_platform_authorized BOOLEAN;
    v_platform_eligible         BOOLEAN;
BEGIN
    NEW.updated_at := now();

    IF NOT NEW.eligible THEN
        RETURN NEW;
    END IF;

    SELECT a.platform_authorized INTO v_asset_platform_authorized
      FROM assets a WHERE a.code = NEW.asset_code;
    IF NOT FOUND OR NOT v_asset_platform_authorized THEN
        RAISE EXCEPTION 'asset_operation_eligibility: asset % is not platform-authorized (layer 3); no operation may be made eligible for it (ADR 0037 C.1)', NEW.asset_code;
    END IF;

    IF NEW.tenant_id IS NULL THEN
        -- A platform-wide default row has nothing above it to narrow
        -- within, beyond the layer-3 check already performed.
        RETURN NEW;
    END IF;

    -- A tenant override may only narrow the platform-wide default (§A.6).
    -- Most-specific-match: a product-specific platform row governs if one
    -- exists, otherwise the every-product row. Note that when NEW.product
    -- IS NULL, `p.product = NEW.product` matches nothing, so an
    -- every-product tenant grant correctly requires an every-product
    -- platform grant and cannot be assembled out of one product's row.
    SELECT p.eligible INTO v_platform_eligible
      FROM asset_operation_eligibility p
     WHERE p.tenant_id IS NULL
       AND p.asset_code = NEW.asset_code
       AND p.operation = NEW.operation
       AND (p.product = NEW.product OR p.product IS NULL)
     ORDER BY (p.product IS NOT NULL) DESC
     LIMIT 1;
    IF NOT FOUND OR NOT v_platform_eligible THEN
        RAISE EXCEPTION 'asset_operation_eligibility: tenant override cannot make % eligible for % where the platform-wide default is absent or ineligible (ADR 0037 A.6 narrow-only)', NEW.asset_code, NEW.operation;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER asset_operation_eligibility_narrowing
    BEFORE INSERT OR UPDATE ON asset_operation_eligibility
    FOR EACH ROW EXECUTE FUNCTION asset_operation_eligibility_enforce_narrowing();
