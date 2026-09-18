-- BonusSuggestion (doc 10 §W6/§N3) - a separate, non-financial domain.
-- "BonusSuggestion never creates a Grant, never moves money, and never
-- calls RG/Risk/AssetAuthorization's value-moving checkpoints" (§W6).
-- This table and its review-event log are the ONLY tables this migration
-- creates for it; there is no ledger reference anywhere in this schema,
-- which is what makes "Suggestion != Grant" true by construction (§N3.3
-- item 1: "No write path to ledger/wallet tables").
--
-- Ordered before bonus_grants (migration 0057) so Grant's
-- originating_suggestion_id back-reference (doc 10 §W6/§N3.2's
-- bidirectional link) can FK to it.
CREATE TABLE bonus_suggestions (
    id                             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                      UUID NOT NULL,
    brand_id                       UUID,
    -- Read-model pointer only (§N3.2) - the authoritative history is
    -- bonus_suggestion_review_events below.
    status                         TEXT NOT NULL DEFAULT 'generated' CHECK (status IN (
        'generated', 'under_review', 'approved', 'rejected', 'edited', 'activated', 'discarded'
    )),
    originating_kind               TEXT NOT NULL CHECK (originating_kind IN ('rule', 'model', 'manual')),
    originating_rule_id            UUID,
    originating_model_version      TEXT,
    originating_staff_actor_id     UUID,
    CHECK (
        (originating_kind = 'rule' AND originating_rule_id IS NOT NULL)
        OR (originating_kind = 'model' AND originating_model_version IS NOT NULL)
        OR (originating_kind = 'manual' AND originating_staff_actor_id IS NOT NULL)
    ),
    generated_at                   TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    -- The inert, structured proposal (§N3.1's proposed_config fields):
    -- proposed_bonus_type, proposed_offer_reference, proposed_reward,
    -- proposed_wagering_requirement, proposed_target_segment,
    -- proposed_campaign_reference, proposed_timing,
    -- proposed_player_population, proposed_activation_strategy - stored
    -- as one JSONB document since every field is itself a reference into
    -- an already-specified Wave 1 shape (§N3.1: "a suggestion invents no
    -- new configuration vocabulary") that this migration does not
    -- separately re-validate at the schema layer.
    proposed_config                JSONB NOT NULL,
    reason                         TEXT,

    reviewer_id                    UUID,
    review_claimed_at              TIMESTAMPTZ,
    decision                       TEXT CHECK (decision IS NULL OR decision IN ('approved', 'rejected', 'edited')),
    decided_at                     TIMESTAMPTZ,
    decided_by                     UUID,
    -- Field-by-field diff for an Edited round (§N3.1: "{field:
    -- (proposed_value, reviewer_value)}").
    modifications                  JSONB,
    rejection_reason               TEXT,

    activated_at                   TIMESTAMPTZ,
    -- Which of the four possible pipelines Activation produced (§N3.2:
    -- "the campaign_id/offer_version_id/grant_id or bulk_grant_job_id
    -- Activation produced"). No hard FK: the referenced row lives in
    -- one of four different tables depending on resulting_reference_kind,
    -- and this migration does not need a fifth polymorphic-reference
    -- table to express that - Phase 3's activation code is responsible
    -- for writing a value that resolves, and for the bidirectional
    -- originating_suggestion_id FK on the actual target table (below)
    -- being the side that IS enforced.
    resulting_reference_kind       TEXT CHECK (resulting_reference_kind IS NULL OR resulting_reference_kind IN (
        'campaign', 'offer', 'grant', 'bulk_grant_job'
    )),
    resulting_reference_id         UUID,
    CHECK ((resulting_reference_kind IS NULL) = (resulting_reference_id IS NULL)),

    discarded_at                   TIMESTAMPTZ,
    discard_reason                 TEXT,

    CHECK (brand_id IS NULL OR tenant_id IS NOT NULL),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id)
);

ALTER TABLE bonus_suggestions ADD CONSTRAINT bonus_suggestions_id_tenant_key UNIQUE (id, tenant_id);

CREATE INDEX idx_bonus_suggestions_tenant ON bonus_suggestions (tenant_id);
CREATE INDEX idx_bonus_suggestions_tenant_status ON bonus_suggestions (tenant_id, status);

-- SuggestionReviewEvent (§N3.2): the authoritative, append-only review
-- trail - "Generated is the creation event itself; each of UnderReview/
-- Approved/Rejected/Edited/Activated/Discarded is its own row" - mirroring
-- GrantActivation's "typed projection over an append-only trail" pattern.
CREATE TABLE bonus_suggestion_review_events (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NOT NULL,
    suggestion_id       UUID NOT NULL,
    event_type          TEXT NOT NULL CHECK (event_type IN (
        'generated', 'under_review', 'approved', 'rejected', 'edited', 'activated', 'discarded'
    )),
    actor_type          TEXT NOT NULL CHECK (actor_type IN ('player', 'staff', 'service', 'system')),
    actor_id            UUID NOT NULL,
    reason_code         TEXT,
    detail              JSONB NOT NULL DEFAULT '{}'::jsonb,
    occurred_at         TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (suggestion_id, tenant_id) REFERENCES bonus_suggestions (id, tenant_id)
);

CREATE INDEX idx_bonus_suggestion_review_events_tenant ON bonus_suggestion_review_events (tenant_id);
CREATE INDEX idx_bonus_suggestion_review_events_suggestion ON bonus_suggestion_review_events (suggestion_id);

CREATE TRIGGER bonus_suggestion_review_events_immutable
    BEFORE UPDATE OR DELETE ON bonus_suggestion_review_events
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();

CREATE TRIGGER bonus_suggestion_review_events_no_truncate
    BEFORE TRUNCATE ON bonus_suggestion_review_events
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

ALTER TABLE bonus_suggestions ENABLE ROW LEVEL SECURITY;
ALTER TABLE bonus_suggestions FORCE ROW LEVEL SECURITY;
ALTER TABLE bonus_suggestion_review_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE bonus_suggestion_review_events FORCE ROW LEVEL SECURITY;

-- Staff-only, no player policy at all (security-architecture.md §B1.3:
-- "a player-visible suggestion row leaks the operator's own segmentation
-- model back to the player").
CREATE POLICY tenant_isolation_read ON bonus_suggestions
    FOR SELECT
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
CREATE POLICY tenant_isolation_insert ON bonus_suggestions
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
CREATE POLICY tenant_isolation_update ON bonus_suggestions
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

CREATE POLICY tenant_isolation_read ON bonus_suggestion_review_events
    FOR SELECT
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
CREATE POLICY tenant_isolation_insert ON bonus_suggestion_review_events
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

CREATE TRIGGER bonus_suggestions_no_truncate
    BEFORE TRUNCATE ON bonus_suggestions
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();
