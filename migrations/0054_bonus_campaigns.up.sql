-- Bonus Engine domain model, Campaign layer (docs/architecture/10-bonus-
-- engine-architecture.md §1.1/§W2.1). Schema and plumbing only (Stage
-- 4H-B1 Wave 2 Phase 2) - no state-machine enforcement, no four-eyes
-- trigger, no business logic. That is Phase 3 (bonus-engine)'s job,
-- built on this table.
--
-- Campaign is identity + scope + top-level status + fulfillment_owner
-- (doc 10 §1.1/§W2.1). Its actual rule content lives in append-only
-- CampaignVersion rows below - "a change is always a new CampaignVersion
-- row, never an edit" (§W2.1) - so bonus_campaigns itself carries no
-- name/display/window/budget field.
--
-- tenant_id is NOT NULL here, departing from §8's own "Campaign may be
-- platform-wide (tenant_id IS NULL)" language, per
-- docs/security/security-architecture.md §B1.3's explicit Wave 1
-- recommendation: "Wave 1 should have no platform-wide Campaign... a
-- tenant_id IS NULL row is visible to every tenant, so a bug that writes
-- one is a cross-tenant data exposure, not a cosmetic defect." A
-- platform-wide Campaign is an explicitly deferred future consideration,
-- not built here - widening this column to nullable, with the
-- risk_rules-style dual-scope RLS policy, is the future migration that
-- would need to accompany it.
CREATE TABLE bonus_campaigns (
    id                       UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                UUID NOT NULL,
    brand_id                 UUID,
    status                   TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'active', 'paused', 'ended', 'archived')),
    -- 'internal' or 'external:<provider_id>' (doc 10 §3.1) - an
    -- open-ended value under a closed prefix set, mirroring ADR 0032's
    -- funding_source/economic_operations.economic_owner shape.
    fulfillment_owner        TEXT NOT NULL DEFAULT 'internal' CHECK (fulfillment_owner = 'internal' OR fulfillment_owner LIKE 'external:%'),
    -- Set once the first CampaignVersion exists; NULL until then. FK
    -- added below, after bonus_campaign_versions is created, to break
    -- the circular reference.
    current_version_id       UUID,
    created_by_actor_type    TEXT NOT NULL CHECK (created_by_actor_type IN ('player', 'staff', 'service', 'system')),
    created_by_actor_id      UUID NOT NULL,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CHECK (brand_id IS NULL OR tenant_id IS NOT NULL),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id)
);

ALTER TABLE bonus_campaigns ADD CONSTRAINT bonus_campaigns_id_tenant_key UNIQUE (id, tenant_id);

CREATE INDEX idx_bonus_campaigns_tenant ON bonus_campaigns (tenant_id);
CREATE INDEX idx_bonus_campaigns_tenant_status ON bonus_campaigns (tenant_id, status);

-- CampaignVersion: append-only, one active pin at a time (current_version_id
-- above), monotonically numbered. "Once any Offer version references a
-- CampaignVersion, that CampaignVersion's content is immutable forever"
-- (§W2.1) - implemented here as UNCONDITIONAL immutability from insert
-- (no UPDATE/DELETE ever, via the trigger below), which is a strict
-- superset of "immutable once referenced" and needs no reference-tracking
-- logic at the schema layer; a correction is always a new version row
-- (doc 10 W1's "no mutable financial history" common object contract).
--
-- The five axes' CONTENT (eligibility/reward/wagering/payout/abuse-
-- control) lives on bonus_offer_versions (migration 0055), per doc 10
-- §2/§W2.2 - CampaignVersion carries the marketing/compliance/scope
-- envelope only (name, window, segment target, jurisdiction/asset/
-- product restriction, budget cap, risk-rule references, the bonus
-- type/mechanic this Campaign issues, T&Cs).
CREATE TABLE bonus_campaign_versions (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NOT NULL,
    campaign_id               UUID NOT NULL,
    version_number            INTEGER NOT NULL CHECK (version_number > 0),
    name                      TEXT NOT NULL,
    display_copy              JSONB NOT NULL DEFAULT '{}'::jsonb,
    window_start              TIMESTAMPTZ,
    window_end                TIMESTAMPTZ,
    CHECK (window_end IS NULL OR window_start IS NULL OR window_end > window_start),
    -- Segment reference by id+version pin only, never an inlined
    -- criteria blob (doc 10 §W7's "hard boundary" - Segmentation itself
    -- is explicitly out of this Wave's scope; no FK to a segments table
    -- exists yet, so these are plain UUID columns pending that domain).
    target_segment_id         UUID,
    target_segment_version_id UUID,
    CHECK ((target_segment_id IS NULL) = (target_segment_version_id IS NULL)),
    jurisdiction_restriction  TEXT[] NOT NULL DEFAULT '{}',
    asset_restriction         TEXT[] NOT NULL DEFAULT '{}',
    product_restriction       TEXT[] NOT NULL DEFAULT '{}',
    -- NOT IMPLEMENTED enforcement anywhere (doc 10 §1.1's own flagged P2
    -- gap, unchanged by this migration) - advisory only.
    budget_cap_amount         NUMERIC(38, 0) CHECK (budget_cap_amount IS NULL OR budget_cap_amount >= 0),
    budget_cap_asset_code     TEXT REFERENCES assets (code),
    CHECK ((budget_cap_amount IS NULL) = (budget_cap_asset_code IS NULL)),
    -- Rule IDs/tags only - Risk owns the rule content (doc 10 §4).
    risk_rule_refs            JSONB NOT NULL DEFAULT '[]'::jsonb,
    -- The canonical mechanic this Campaign issues (doc 10 §W3): the
    -- trigger/reward/completion codes, plus the named catalogue item for
    -- reporting. Enforcement of the closed T1-T5/R1-R6/C1-C4 vocabulary
    -- is Phase 3's job (this column is TEXT, not yet CHECK-constrained,
    -- since Phase 2 builds no catalogue-mapping logic to validate
    -- against); documented here rather than silently omitted.
    trigger_mechanic          TEXT,
    reward_mechanic           TEXT,
    completion_mechanic       TEXT,
    catalogue_item            TEXT,
    terms_version             TEXT,
    terms_text                TEXT,
    created_by_actor_type     TEXT NOT NULL CHECK (created_by_actor_type IN ('player', 'staff', 'service', 'system')),
    created_by_actor_id       UUID NOT NULL,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (tenant_id, campaign_id, version_number),
    FOREIGN KEY (campaign_id, tenant_id) REFERENCES bonus_campaigns (id, tenant_id)
);

ALTER TABLE bonus_campaign_versions ADD CONSTRAINT bonus_campaign_versions_id_tenant_key UNIQUE (id, tenant_id);

ALTER TABLE bonus_campaigns
    ADD CONSTRAINT bonus_campaigns_current_version_fk
        FOREIGN KEY (current_version_id, tenant_id) REFERENCES bonus_campaign_versions (id, tenant_id);

CREATE INDEX idx_bonus_campaign_versions_tenant ON bonus_campaign_versions (tenant_id);
CREATE INDEX idx_bonus_campaign_versions_campaign ON bonus_campaign_versions (campaign_id);

CREATE TRIGGER bonus_campaign_versions_immutable
    BEFORE UPDATE OR DELETE ON bonus_campaign_versions
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();

CREATE TRIGGER bonus_campaign_versions_no_truncate
    BEFORE TRUNCATE ON bonus_campaign_versions
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

ALTER TABLE bonus_campaigns ENABLE ROW LEVEL SECURITY;
ALTER TABLE bonus_campaigns FORCE ROW LEVEL SECURITY;
ALTER TABLE bonus_campaign_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE bonus_campaign_versions FORCE ROW LEVEL SECURITY;

-- Staff-only, per-command (security-architecture.md §B1.3's chosen shape
-- for bonus_campaigns: no FOR ALL, no DELETE policy - a DELETE affects
-- zero rows under RLS rather than being a silent eligibility-widening
-- vector, migration 0047 Fix 3's lesson).
CREATE POLICY tenant_isolation_read ON bonus_campaigns
    FOR SELECT
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
CREATE POLICY tenant_isolation_insert ON bonus_campaigns
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
CREATE POLICY tenant_isolation_update ON bonus_campaigns
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

CREATE POLICY tenant_isolation_read ON bonus_campaign_versions
    FOR SELECT
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
CREATE POLICY tenant_isolation_insert ON bonus_campaign_versions
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
-- No UPDATE policy on bonus_campaign_versions: the immutability trigger
-- above already rejects every UPDATE, and a matching RLS UPDATE policy
-- would only ever be reached for a statement the trigger already denies -
-- omitted rather than written as dead code.

CREATE TRIGGER bonus_campaigns_no_truncate
    BEFORE TRUNCATE ON bonus_campaigns
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();
