-- Grant (doc 10 §1.1/§1.2/§1.3/T.2, "the Grant lifecycle table"). Schema
-- and plumbing only (Stage 4H-B1 Wave 2 Phase 2) - no state-machine
-- enforcement of WHICH transitions are legal (that CHECK-constraining
-- transitions requires a trigger keyed on OLD.status/NEW.status pairs,
-- which is Phase 3's job once the transition table itself is being
-- implemented, not merely stored). This migration enforces only the
-- CLOSED SET of values (T.11) and the immutability of every field T.2
-- freezes at creation - a Grant can be fully reconstructed from its own
-- row without a live config read, per T.2's own requirement.
--
-- status's nine terminal/non-terminal values are §1.2's table PLUS
-- pending_settlement (doc 10 N1.4/N1.9's additive row, Fix Round 2):
-- "NewStakeEligibility(G, t) = open whenever G.status(t) IN {issued,
-- activated, in_progress}" - closed for every other value, including
-- pending_settlement, which this schema's CHECK constraint makes a
-- structurally distinguishable status precisely so NewStakeEligibility
-- is a pure function of this column, never a second derived flag.
CREATE TABLE bonus_grants (
    id                              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                       UUID NOT NULL,
    brand_id                        UUID NOT NULL,
    player_account_id               UUID NOT NULL,
    wallet_id                       UUID NOT NULL,

    campaign_id                     UUID NOT NULL,
    campaign_version_id             UUID NOT NULL,
    offer_id                        UUID NOT NULL,
    offer_version_id                UUID NOT NULL,

    -- Frozen once, never re-chosen (T.2): "a Grant is never
    -- re-denominated". decimal_exponent is looked up once from the Asset
    -- Registry's existence layer and frozen here, safe because ADR 0037
    -- §C.5.4 makes it immutable at the registry level too.
    asset_code                      TEXT NOT NULL REFERENCES assets (code),
    decimal_exponent                SMALLINT NOT NULL CHECK (decimal_exponent BETWEEN 0 AND 18),

    status                          TEXT NOT NULL DEFAULT 'issued' CHECK (status IN (
        'issued', 'activated', 'in_progress', 'pending_settlement',
        'completed', 'converted', 'expired', 'cancelled', 'forfeited', 'reversed'
    )),

    -- ADR 0032 §6: "fixed on the grant record at grant time and is
    -- immutable thereafter".
    funding_source                  TEXT NOT NULL CHECK (funding_source = 'operator' OR funding_source LIKE 'provider:%'),
    -- doc 10 §3.2/ADR 0032 §6(c).
    fulfillment_destination         TEXT NOT NULL CHECK (fulfillment_destination IN ('into_platform_wallet', 'inside_provider')),
    -- Denormalized from the owning Campaign at grant time (doc 10 §3.1).
    fulfillment_owner               TEXT NOT NULL CHECK (fulfillment_owner = 'internal' OR fulfillment_owner LIKE 'external:%'),

    -- The idempotency key components (doc 10 §9) - a real DB-enforced
    -- unique constraint below, never a concatenated string key.
    trigger_reference               TEXT NOT NULL,

    -- Denormalized snapshot of the Offer's eligibility-axis facts AS
    -- EVALUATED at issued time (T.2/T.4) - "a historical record, not a
    -- value any later checkpoint re-reads to make a new decision".
    eligibility_snapshot            JSONB NOT NULL DEFAULT '{}'::jsonb,
    -- Nullable segment reference (segment_id + segment_version_id),
    -- matching doc 10 §W7's "by reference, never inlined" pattern, one
    -- level up on the Grant's own audit trail (§W7: "records the
    -- segment_id + segment_version_id... that qualified the player at
    -- issued time"). Segmentation itself (internal/segment) is
    -- explicitly out of this Wave's scope (known Kleene-logic polarity
    -- bug, unpinned member_of) - this is a placeholder field only, no
    -- segment-evaluation logic anywhere in this schema or package.
    segment_id                      UUID,
    segment_version_id              UUID,
    CHECK ((segment_id IS NULL) = (segment_version_id IS NULL)),

    jurisdiction_code               TEXT REFERENCES jurisdictions (code),
    licensing_mode                  TEXT CHECK (licensing_mode IS NULL OR licensing_mode IN ('under_platform_licence', 'own_licence')),

    -- EconomicOperationIdentity (doc 34, doc 10 N2.4a): "every surface
    -- capable of causing a Grant to exist... now requires a resolvable
    -- parent_operation_id before it is honored." Nullable at the schema
    -- level because enforcing "must be non-null and must resolve/be
    -- approved/be in-budget" is the Phase 3 entry-check (doc 34 §5.1),
    -- not a database CHECK this migration can express (it requires a
    -- live lookup, not a structural predicate).
    parent_operation_id             UUID,

    -- Bidirectional link with BonusSuggestion Activation (doc 10 §W6/
    -- §N3.2): "the resulting object carries originating_suggestion_id
    -- back to this suggestion".
    originating_suggestion_id       UUID,

    created_by_actor_type           TEXT NOT NULL CHECK (created_by_actor_type IN ('player', 'staff', 'service', 'system')),
    created_by_actor_id             UUID NOT NULL,
    created_at                      TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),

    activated_at                    TIMESTAMPTZ,
    completed_at                    TIMESTAMPTZ,
    converted_at                    TIMESTAMPTZ,
    terminal_at                     TIMESTAMPTZ,

    -- pending_settlement's carried fields (doc 10 N1.4, additive to the
    -- Grant row - "mirroring GrantActivation's typed projection, not a
    -- side table discipline"). terminal_resolution never 'converted'
    -- (Path A's own concern, N1.4) - restated as a CHECK below.
    terminal_resolution             TEXT CHECK (terminal_resolution IS NULL OR terminal_resolution IN ('expired', 'cancelled', 'forfeited')),
    terminal_trigger_reason_code    TEXT,
    terminal_triggered_at           TIMESTAMPTZ,
    terminal_trigger_correlation_id UUID,
    CHECK (
        (status <> 'pending_settlement')
        OR (terminal_resolution IS NOT NULL AND terminal_trigger_reason_code IS NOT NULL AND terminal_triggered_at IS NOT NULL)
    ),

    -- 'reversed' (doc 10 §1.3's last row): "an additional Progress entry
    -- and a new Grant status", applied after another terminal state.
    reversed_at                     TIMESTAMPTZ,
    reversal_reason_code            TEXT,
    CHECK ((status = 'reversed') = (reversed_at IS NOT NULL)),

    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id),
    FOREIGN KEY (player_account_id, tenant_id, brand_id) REFERENCES player_accounts (id, tenant_id, brand_id),
    FOREIGN KEY (wallet_id, tenant_id) REFERENCES wallets (id, tenant_id),
    FOREIGN KEY (wallet_id, player_account_id) REFERENCES wallets (id, player_account_id),
    FOREIGN KEY (campaign_id, tenant_id) REFERENCES bonus_campaigns (id, tenant_id),
    FOREIGN KEY (campaign_version_id, tenant_id) REFERENCES bonus_campaign_versions (id, tenant_id),
    FOREIGN KEY (offer_id, tenant_id) REFERENCES bonus_offers (id, tenant_id),
    FOREIGN KEY (offer_version_id, tenant_id) REFERENCES bonus_offer_versions (id, tenant_id),
    FOREIGN KEY (originating_suggestion_id, tenant_id) REFERENCES bonus_suggestions (id, tenant_id),
    FOREIGN KEY (parent_operation_id, tenant_id) REFERENCES economic_operations (operation_id, tenant_id)
);

-- HR-9-style DB-enforced idempotency (doc 10 §9): "Grant issuance:
-- idempotency key = (tenant_id, campaign_id, offer_version_id,
-- player_account_id, trigger_reference)". A real composite UNIQUE
-- constraint, never a concatenated string, never "check then insert".
ALTER TABLE bonus_grants
    ADD CONSTRAINT bonus_grants_idempotency_key
        UNIQUE (tenant_id, campaign_id, offer_version_id, player_account_id, trigger_reference);

ALTER TABLE bonus_grants ADD CONSTRAINT bonus_grants_id_tenant_key UNIQUE (id, tenant_id);

-- "Grant lookup by player, by campaign, by status" - this dispatch's own
-- named required query patterns.
CREATE INDEX idx_bonus_grants_tenant ON bonus_grants (tenant_id);
CREATE INDEX idx_bonus_grants_player ON bonus_grants (tenant_id, player_account_id);
CREATE INDEX idx_bonus_grants_campaign ON bonus_grants (tenant_id, campaign_id);
CREATE INDEX idx_bonus_grants_offer ON bonus_grants (tenant_id, offer_id);
CREATE INDEX idx_bonus_grants_status ON bonus_grants (tenant_id, status);
CREATE INDEX idx_bonus_grants_originating_suggestion ON bonus_grants (originating_suggestion_id) WHERE originating_suggestion_id IS NOT NULL;
CREATE INDEX idx_bonus_grants_parent_operation ON bonus_grants (parent_operation_id) WHERE parent_operation_id IS NOT NULL;

-- Immutable-after-insert guard (T.2's "fixed vs. evaluated later" list) -
-- mirroring withdrawal_requests_enforce_immutable_fields (migration
-- 0026). Only the lifecycle/timestamp columns a real transition is
-- expected to change are excluded.
CREATE FUNCTION bonus_grants_enforce_immutable_fields() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.brand_id IS DISTINCT FROM OLD.brand_id
        OR NEW.player_account_id IS DISTINCT FROM OLD.player_account_id
        OR NEW.wallet_id IS DISTINCT FROM OLD.wallet_id
        OR NEW.campaign_id IS DISTINCT FROM OLD.campaign_id
        OR NEW.campaign_version_id IS DISTINCT FROM OLD.campaign_version_id
        OR NEW.offer_id IS DISTINCT FROM OLD.offer_id
        OR NEW.offer_version_id IS DISTINCT FROM OLD.offer_version_id
        OR NEW.asset_code IS DISTINCT FROM OLD.asset_code
        OR NEW.decimal_exponent IS DISTINCT FROM OLD.decimal_exponent
        OR NEW.funding_source IS DISTINCT FROM OLD.funding_source
        OR NEW.fulfillment_destination IS DISTINCT FROM OLD.fulfillment_destination
        OR NEW.fulfillment_owner IS DISTINCT FROM OLD.fulfillment_owner
        OR NEW.trigger_reference IS DISTINCT FROM OLD.trigger_reference
        OR NEW.eligibility_snapshot IS DISTINCT FROM OLD.eligibility_snapshot
        OR NEW.segment_id IS DISTINCT FROM OLD.segment_id
        OR NEW.segment_version_id IS DISTINCT FROM OLD.segment_version_id
        OR NEW.jurisdiction_code IS DISTINCT FROM OLD.jurisdiction_code
        OR NEW.licensing_mode IS DISTINCT FROM OLD.licensing_mode
        OR NEW.parent_operation_id IS DISTINCT FROM OLD.parent_operation_id
        OR NEW.originating_suggestion_id IS DISTINCT FROM OLD.originating_suggestion_id
        OR NEW.created_by_actor_type IS DISTINCT FROM OLD.created_by_actor_type
        OR NEW.created_by_actor_id IS DISTINCT FROM OLD.created_by_actor_id
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'bonus_grants: identity/scope/asset/funding/eligibility-snapshot/idempotency/audit fields are immutable after insert (doc10 T.2)';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER bonus_grants_immutable_fields
    BEFORE UPDATE ON bonus_grants
    FOR EACH ROW EXECUTE FUNCTION bonus_grants_enforce_immutable_fields();

ALTER TABLE bonus_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE bonus_grants FORCE ROW LEVEL SECURITY;

-- ledger_accounts' dual scope (security-architecture.md §B1.3): staff
-- access split per command (no FOR ALL), plus a read-only player_self_scope.
CREATE POLICY tenant_staff_read ON bonus_grants
    FOR SELECT
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
CREATE POLICY tenant_staff_insert ON bonus_grants
    FOR INSERT
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
CREATE POLICY tenant_staff_update ON bonus_grants
    FOR UPDATE
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- Player read-only (withdrawal_requests' precedent, migration 0026):
-- never INSERT/UPDATE - a player-writable Grant row would bypass every
-- gate T.1/T.3 require.
CREATE POLICY player_self_scope ON bonus_grants
    FOR SELECT
    USING (
        player_account_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
    );

CREATE TRIGGER bonus_grants_no_truncate
    BEFORE TRUNCATE ON bonus_grants
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();
