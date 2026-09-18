-- Bonus Engine domain model, Offer layer (doc 10 §1.1/§W2.2) - separate
-- from Campaign, no duplicated logic. Schema and plumbing only (Stage
-- 4H-B1 Wave 2 Phase 2).
CREATE TABLE bonus_offers (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NOT NULL,
    -- Same scope as the owning Campaign (doc 10 §8: "an Offer cannot be
    -- scoped more broadly than its Campaign - narrowing only"). Enforcing
    -- the narrowing rule itself is Phase 3 business logic; this migration
    -- only denormalizes brand_id for RLS/query convenience, matching the
    -- pattern every other tenant-owned child table in this codebase uses.
    brand_id            UUID,
    campaign_id         UUID NOT NULL,
    -- Immutable pin (doc 10 §1.1/T.2): an Offer's own CampaignVersion
    -- reference never changes after creation - enforced by the
    -- immutable-fields trigger below, mirroring withdrawal_requests'
    -- pattern (migration 0026).
    campaign_version_id UUID NOT NULL,
    status              TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'active', 'retired')),
    -- Selects which canonical trigger mechanic (doc 10 §W3) applies -
    -- "the one field that distinguishes, e.g., a Coupon Offer from a
    -- Deposit-bonus Offer" (§W2.2).
    grant_policy        TEXT NOT NULL CHECK (grant_policy IN (
        'auto_issue', 'manual_approval_required', 'code_redeemed', 'external_signal', 'manually_assigned'
    )),
    window_start        TIMESTAMPTZ,
    window_end          TIMESTAMPTZ,
    CHECK (window_end IS NULL OR window_start IS NULL OR window_end > window_start),
    current_version_id  UUID,
    created_by_actor_type TEXT NOT NULL CHECK (created_by_actor_type IN ('player', 'staff', 'service', 'system')),
    created_by_actor_id   UUID NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CHECK (brand_id IS NULL OR tenant_id IS NOT NULL),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id),
    FOREIGN KEY (campaign_id, tenant_id) REFERENCES bonus_campaigns (id, tenant_id),
    FOREIGN KEY (campaign_version_id, tenant_id) REFERENCES bonus_campaign_versions (id, tenant_id)
);

ALTER TABLE bonus_offers ADD CONSTRAINT bonus_offers_id_tenant_key UNIQUE (id, tenant_id);

CREATE INDEX idx_bonus_offers_tenant ON bonus_offers (tenant_id);
CREATE INDEX idx_bonus_offers_campaign ON bonus_offers (campaign_id);
CREATE INDEX idx_bonus_offers_tenant_status ON bonus_offers (tenant_id, status);

CREATE FUNCTION bonus_offers_enforce_immutable_fields() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.campaign_id IS DISTINCT FROM OLD.campaign_id
        OR NEW.campaign_version_id IS DISTINCT FROM OLD.campaign_version_id
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'bonus_offers: tenant_id/campaign_id/campaign_version_id/created_at are immutable after insert';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER bonus_offers_immutable_fields
    BEFORE UPDATE ON bonus_offers
    FOR EACH ROW EXECUTE FUNCTION bonus_offers_enforce_immutable_fields();

-- OfferVersion: the five axes (Eligibility/Reward/Wagering/Payout/
-- Abuse-control, doc 10 §2/§W2.2), immutable once written (unconditional,
-- same reasoning as bonus_campaign_versions above - "immutable once any
-- Grant references it" is a strict subset of "always immutable").
--
-- Structured axis content that this migration does not need to enforce
-- with a CHECK constraint (Phase 3's business-rule territory) is stored
-- as JSONB, mirroring this codebase's existing convention for
-- configuration blobs (brands.theme, tenant_jurisdiction_configs,
-- asset_change_requests.payload). Fields other tables or Phase 3's
-- financial posting must reference directly (reward asset/kind,
-- fulfillment/funding, rounding rule, wagering multiplier) are pulled
-- out into typed columns instead.
CREATE TABLE bonus_offer_versions (
    id                              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                       UUID NOT NULL,
    offer_id                        UUID NOT NULL,
    version_number                  INTEGER NOT NULL CHECK (version_number > 0),

    -- Eligibility axis
    eligibility_segment_id          UUID,
    eligibility_segment_version_id  UUID,
    CHECK ((eligibility_segment_id IS NULL) = (eligibility_segment_version_id IS NULL)),
    eligibility_jurisdiction_list   TEXT[] NOT NULL DEFAULT '{}',
    eligibility_deposit_methods     TEXT[] NOT NULL DEFAULT '{}',
    first_deposit_only              BOOLEAN NOT NULL DEFAULT false,
    min_qualifying_amount           NUMERIC(38, 0) CHECK (min_qualifying_amount IS NULL OR min_qualifying_amount >= 0),
    max_qualifying_amount           NUMERIC(38, 0) CHECK (max_qualifying_amount IS NULL OR max_qualifying_amount >= 0),
    -- VIP tier is itself a segment reference (doc 10 §W2.2: "never a
    -- Bonus-local enum").
    vip_tier_segment_id             UUID,
    vip_tier_segment_version_id     UUID,
    CHECK ((vip_tier_segment_id IS NULL) = (vip_tier_segment_version_id IS NULL)),
    opt_in_required                 BOOLEAN NOT NULL DEFAULT false,
    kyc_rg_level_required           TEXT,
    -- Coded-bonus fields (doc 10 §W4), present on every Offer version
    -- (null/unused unless grant_policy = 'code_redeemed').
    redemption_code                 TEXT,
    redemption_code_pool_ref        TEXT,
    redemption_validation_rule      JSONB NOT NULL DEFAULT '{}'::jsonb,
    redemption_limit_per_player     INTEGER CHECK (redemption_limit_per_player IS NULL OR redemption_limit_per_player > 0),
    redemption_limit_global         INTEGER CHECK (redemption_limit_global IS NULL OR redemption_limit_global > 0),
    stacking_conflict_predicate     JSONB NOT NULL DEFAULT '{}'::jsonb,

    -- Reward axis (BonusReward, §W2.6)
    reward_kind                     TEXT NOT NULL CHECK (reward_kind IN ('R1', 'R2', 'R3', 'R5', 'R6')),
    reward_asset_code               TEXT NOT NULL REFERENCES assets (code),
    reward_calculation              JSONB NOT NULL DEFAULT '{}'::jsonb,
    rounding_rule_id                UUID,
    -- 'internal' or 'external:<provider_id>', ADR 0032 §6(c)/doc 10 §3.2.
    fulfillment_destination         TEXT NOT NULL CHECK (fulfillment_destination IN ('into_platform_wallet', 'inside_provider')),
    funding_source                  TEXT NOT NULL CHECK (funding_source = 'operator' OR funding_source LIKE 'provider:%'),

    -- Wagering axis (WageringRequirement, §W2.7)
    wagering_multiplier_bp          INTEGER CHECK (wagering_multiplier_bp IS NULL OR wagering_multiplier_bp >= 0),
    contribution_weight_table       JSONB NOT NULL DEFAULT '{}'::jsonb,
    max_bet_while_wagering          NUMERIC(38, 0) CHECK (max_bet_while_wagering IS NULL OR max_bet_while_wagering >= 0),
    excluded_games                  JSONB NOT NULL DEFAULT '[]'::jsonb,
    wagering_time_limit             INTERVAL,

    -- Payout axis
    max_cashout_amount              NUMERIC(38, 0) CHECK (max_cashout_amount IS NULL OR max_cashout_amount >= 0),
    max_cashout_percent_bp          INTEGER CHECK (max_cashout_percent_bp IS NULL OR max_cashout_percent_bp >= 0),
    payout_ordering                 TEXT CHECK (payout_ordering IS NULL OR payout_ordering IN ('cash_first', 'bonus_first')),
    partial_release_thresholds      JSONB NOT NULL DEFAULT '[]'::jsonb,
    payout_time_limit                INTERVAL,

    -- Abuse-control axis
    velocity_cap_refs                JSONB NOT NULL DEFAULT '[]'::jsonb,
    device_fingerprint_linking_config JSONB NOT NULL DEFAULT '{}'::jsonb,
    manual_review_routing            JSONB NOT NULL DEFAULT '{}'::jsonb,

    -- Canonical mechanic codes (doc 10 §W3). Not yet CHECK-constrained to
    -- the closed T1-T5/R1-R6/C1-C4 vocabulary at the OfferVersion level -
    -- reward_kind above already enforces its own slice; trigger/
    -- completion validation against the full catalogue mapping (§W3's
    -- table) is Phase 3's job.
    trigger_mechanic                 TEXT,
    completion_mechanic              TEXT,

    terms_and_conditions_text        TEXT,
    created_by_actor_type            TEXT NOT NULL CHECK (created_by_actor_type IN ('player', 'staff', 'service', 'system')),
    created_by_actor_id              UUID NOT NULL,
    created_at                       TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (tenant_id, offer_id, version_number),
    FOREIGN KEY (offer_id, tenant_id) REFERENCES bonus_offers (id, tenant_id)
);

ALTER TABLE bonus_offer_versions ADD CONSTRAINT bonus_offer_versions_id_tenant_key UNIQUE (id, tenant_id);

ALTER TABLE bonus_offers
    ADD CONSTRAINT bonus_offers_current_version_fk
        FOREIGN KEY (current_version_id, tenant_id) REFERENCES bonus_offer_versions (id, tenant_id);

CREATE INDEX idx_bonus_offer_versions_tenant ON bonus_offer_versions (tenant_id);
CREATE INDEX idx_bonus_offer_versions_offer ON bonus_offer_versions (offer_id);

CREATE TRIGGER bonus_offer_versions_immutable
    BEFORE UPDATE OR DELETE ON bonus_offer_versions
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();

CREATE TRIGGER bonus_offer_versions_no_truncate
    BEFORE TRUNCATE ON bonus_offer_versions
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

ALTER TABLE bonus_offers ENABLE ROW LEVEL SECURITY;
ALTER TABLE bonus_offers FORCE ROW LEVEL SECURITY;
ALTER TABLE bonus_offer_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE bonus_offer_versions FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_read ON bonus_offers
    FOR SELECT
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
CREATE POLICY tenant_isolation_insert ON bonus_offers
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
CREATE POLICY tenant_isolation_update ON bonus_offers
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

CREATE POLICY tenant_isolation_read ON bonus_offer_versions
    FOR SELECT
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
CREATE POLICY tenant_isolation_insert ON bonus_offer_versions
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

CREATE TRIGGER bonus_offers_no_truncate
    BEFORE TRUNCATE ON bonus_offers
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();
