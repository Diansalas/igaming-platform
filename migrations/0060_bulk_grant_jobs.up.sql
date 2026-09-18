-- BulkGrantJob / BulkGrantJobItem (doc 10 §W5, widened by §N2.2's
-- segment_set targeting mode and §N2.4a's EconomicOperationIdentity
-- adoption). Schema and plumbing only (Stage 4H-B1 Wave 2 Phase 2) - no
-- job-runner, no resumability logic, no live segment resolution, no
-- four-eyes trigger. That is Phase 3.
CREATE TABLE bulk_grant_jobs (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NOT NULL,
    brand_id                  UUID NOT NULL,
    campaign_id               UUID NOT NULL,
    offer_version_id          UUID NOT NULL,

    -- target (doc 10 §W5, widened by §N2.2 to add 'segment_set'): a
    -- single player_account_id, an explicit list, a Segment+SegmentVersion
    -- reference resolved LIVE at run time, or a bounded, explicit list of
    -- (segment_id, segment_version_id) pairs resolved as a UNION. Exactly
    -- one shape is populated per target_kind, enforced below.
    target_kind               TEXT NOT NULL CHECK (target_kind IN ('single_player', 'player_list', 'segment', 'segment_set')),
    target_player_account_id  UUID,
    target_player_list        UUID[],
    target_segment_id         UUID,
    target_segment_version_id UUID,
    -- [{"segment_id": "...", "segment_version_id": "..."}, ...] - a
    -- bounded, explicit list (§N2.2). Segmentation itself
    -- (internal/segment) is out of this Wave's scope; this column only
    -- stores the reference pairs, no criteria evaluation.
    target_segment_set        JSONB,
    CHECK ((target_segment_id IS NULL) = (target_segment_version_id IS NULL)),
    CHECK (
        (target_kind = 'single_player' AND target_player_account_id IS NOT NULL AND target_player_list IS NULL AND target_segment_id IS NULL AND target_segment_set IS NULL)
        OR (target_kind = 'player_list' AND target_player_list IS NOT NULL AND target_player_account_id IS NULL AND target_segment_id IS NULL AND target_segment_set IS NULL)
        OR (target_kind = 'segment' AND target_segment_id IS NOT NULL AND target_player_account_id IS NULL AND target_player_list IS NULL AND target_segment_set IS NULL)
        OR (target_kind = 'segment_set' AND target_segment_set IS NOT NULL AND target_player_account_id IS NULL AND target_player_list IS NULL AND target_segment_id IS NULL)
    ),

    requested_by_principal_id UUID NOT NULL,
    requested_at              TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    approval_state            TEXT NOT NULL DEFAULT 'pending_four_eyes' CHECK (approval_state IN ('pending_four_eyes', 'approved', 'rejected')),
    status                    TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued', 'running', 'completed', 'partially_completed', 'failed')),

    -- A resubmission of the identical job spec is a no-op against this
    -- key, never a second job (§W5).
    idempotency_key           TEXT NOT NULL,

    -- Mint point for the bonus_bulk_grant EconomicOperationIdentity
    -- (doc 34 §3.1: "the job's own four-eyes approval" is the root
    -- authorization) - nullable at the schema level for the same reason
    -- bonus_grants.parent_operation_id is (the live approve/resolve/
    -- in-budget check is Phase 3's, not a structural CHECK).
    parent_operation_id       UUID,
    originating_suggestion_id UUID,

    created_at                TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),

    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id),
    FOREIGN KEY (campaign_id, tenant_id) REFERENCES bonus_campaigns (id, tenant_id),
    FOREIGN KEY (offer_version_id, tenant_id) REFERENCES bonus_offer_versions (id, tenant_id),
    FOREIGN KEY (target_player_account_id, tenant_id) REFERENCES player_accounts (id, tenant_id),
    FOREIGN KEY (originating_suggestion_id, tenant_id) REFERENCES bonus_suggestions (id, tenant_id),
    FOREIGN KEY (parent_operation_id, tenant_id) REFERENCES economic_operations (operation_id, tenant_id)
);

ALTER TABLE bulk_grant_jobs ADD CONSTRAINT bulk_grant_jobs_id_tenant_key UNIQUE (id, tenant_id);
ALTER TABLE bulk_grant_jobs ADD CONSTRAINT bulk_grant_jobs_idempotency_key UNIQUE (tenant_id, idempotency_key);

CREATE INDEX idx_bulk_grant_jobs_tenant ON bulk_grant_jobs (tenant_id);
CREATE INDEX idx_bulk_grant_jobs_campaign ON bulk_grant_jobs (tenant_id, campaign_id);
CREATE INDEX idx_bulk_grant_jobs_status ON bulk_grant_jobs (tenant_id, status);
CREATE INDEX idx_bulk_grant_jobs_parent_operation ON bulk_grant_jobs (parent_operation_id) WHERE parent_operation_id IS NOT NULL;

-- BulkGrantJobItem: append-only, one row per targeted player, the
-- resumability/per-player-isolation primitive (§W5). Also the
-- consumption-record shape for the bonus_bulk_grant EconomicOperationIdentity
-- (doc 10 §N2.4a: "counted by player_account_id (recipient budget),
-- valued by granted_amount (value budget)").
CREATE TABLE bulk_grant_job_items (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NOT NULL,
    bulk_grant_job_id     UUID NOT NULL,
    player_account_id     UUID NOT NULL,
    outcome               TEXT NOT NULL DEFAULT 'pending' CHECK (outcome IN ('pending', 'issued', 'denied', 'already_granted', 'error')),
    reason_code           TEXT,
    grant_id              UUID,
    -- The value budget's own measure (doc 10 §N2.4a) - NUMERIC(38,0),
    -- never int64/float64. NULL until the item actually issues a Grant.
    granted_amount        NUMERIC(38, 0) CHECK (granted_amount IS NULL OR granted_amount >= 0),
    error_detail          TEXT,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    processed_at          TIMESTAMPTZ,
    CHECK ((outcome = 'pending') = (processed_at IS NULL)),
    CHECK (outcome NOT IN ('issued', 'already_granted') OR grant_id IS NOT NULL),

    -- Per-player isolation and resumability (§W5): "each keyed
    -- (tenant_id, bulk_grant_job_id, player_account_id) (DB-unique)... a
    -- crashed/resumed job re-walks its target list and skips every
    -- player who already has a BulkGrantJobItem row of any outcome."
    UNIQUE (tenant_id, bulk_grant_job_id, player_account_id),
    FOREIGN KEY (bulk_grant_job_id, tenant_id) REFERENCES bulk_grant_jobs (id, tenant_id),
    FOREIGN KEY (player_account_id, tenant_id) REFERENCES player_accounts (id, tenant_id),
    FOREIGN KEY (grant_id, tenant_id) REFERENCES bonus_grants (id, tenant_id)
);

CREATE INDEX idx_bulk_grant_job_items_tenant ON bulk_grant_job_items (tenant_id);
CREATE INDEX idx_bulk_grant_job_items_job ON bulk_grant_job_items (tenant_id, bulk_grant_job_id);
CREATE INDEX idx_bulk_grant_job_items_outcome ON bulk_grant_job_items (tenant_id, bulk_grant_job_id, outcome);

ALTER TABLE bulk_grant_jobs ENABLE ROW LEVEL SECURITY;
ALTER TABLE bulk_grant_jobs FORCE ROW LEVEL SECURITY;
ALTER TABLE bulk_grant_job_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE bulk_grant_job_items FORCE ROW LEVEL SECURITY;

-- Staff-only, per-command (security-architecture.md §B1.3's
-- bonus_bulk_jobs shape). No DELETE policy.
CREATE POLICY tenant_isolation_read ON bulk_grant_jobs
    FOR SELECT
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
CREATE POLICY tenant_isolation_insert ON bulk_grant_jobs
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
CREATE POLICY tenant_isolation_update ON bulk_grant_jobs
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

CREATE POLICY tenant_isolation_read ON bulk_grant_job_items
    FOR SELECT
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
CREATE POLICY tenant_isolation_insert ON bulk_grant_job_items
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
CREATE POLICY tenant_isolation_update ON bulk_grant_job_items
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

CREATE TRIGGER bulk_grant_jobs_no_truncate
    BEFORE TRUNCATE ON bulk_grant_jobs
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

CREATE TRIGGER bulk_grant_job_items_no_truncate
    BEFORE TRUNCATE ON bulk_grant_job_items
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();
