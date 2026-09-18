-- EconomicOperationIdentity (docs/architecture/34-economic-operation-
-- identity.md, "doc 34"). A cross-cutting authorization-lineage
-- mechanism, NOT a Bonus Engine table: Bonus is its first (and, this
-- stage, only) consumer, but doc 34 §4.5 names it a shared primitive
-- CRM/Affiliate are also expected to mint operation_type values under
-- (crm_engagement_campaign_activation, affiliate_commission_settlement,
-- affiliate_reattribution - §3.1), so it lives in its own package
-- (internal/economicop) and its own migration, ahead of every Bonus
-- table that references it (Bonus's bonus_grants.parent_operation_id and
-- bulk_grant_jobs/bulk_grant_job_items reference this table - migrations
-- 0056/0060).
--
-- Doc 34 §2.1's one binding sentence: "An authorization mints exactly
-- one EconomicOperationIdentity. Every execution it causes - directly or
-- transitively, first attempt or thousandth - inherits that identity as
-- its parent_operation_id. No executor ever mints." This migration
-- builds ONLY the storage for that model (doc 34 §2.2's field list) - it
-- does not implement §5's enforcement (the fail-closed entry check, the
-- locking consume, the canonical lock ordering). That is explicitly
-- Phase 3/the architect's later integration work, per this dispatch's
-- own scope (Stage 4H-B1 Wave 2 Phase 2: schema and plumbing only).
--
-- operation_type is the closed, compiled-in enum doc 34 §3.1 names -
-- every value that document currently mints, not only Bonus's four, so
-- that CRM/Affiliate need no migration of their own to start using this
-- table once their own Phase 2-equivalent work lands (doc 34's own
-- "additive extension discipline, mirroring ADR 0031 §12" governs any
-- FUTURE value beyond this closed list).
CREATE TABLE economic_operations (
    operation_id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                       UUID NOT NULL,
    brand_id                        UUID,

    operation_type                  TEXT NOT NULL CHECK (operation_type IN (
        'bonus_bulk_grant', 'bonus_manual_grant', 'bonus_campaign_activation',
        'bonus_held_disposition_resolution',
        'crm_engagement_campaign_activation',
        'affiliate_commission_settlement', 'affiliate_reattribution',
        'manual_balance_adjustment', 'api_initiated_grant'
    )),

    -- internal/audit.ActorType verbatim (doc 10 N2.3: "there is no
    -- 'provider' value, and no domain invents a parallel enum" -
    -- doc 34 §2.2 adopts this by explicit cross-reference).
    initiating_actor_type           TEXT NOT NULL CHECK (initiating_actor_type IN ('player', 'staff', 'service', 'system')),
    initiating_actor_id             UUID NOT NULL,
    initiating_principal_id         UUID,

    subject_scope                   TEXT NOT NULL CHECK (subject_scope IN ('single_subject', 'enumerated_set', 'criteria_defined', 'none')),
    subject_ref                     UUID,
    -- Both hashes doc 34 §2.2 requires, pending the DEP-CRM-7
    -- reconciliation it names as unconfirmed: subject_set_hash over the
    -- materialized ceiling, subject_definition_hash over the definition
    -- (segment-reference pins). Hex/base64 text, not BYTEA - matches
    -- this codebase's existing preference for TEXT-typed opaque
    -- identifiers over binary columns (no BYTEA column exists anywhere
    -- in migrations/ today).
    subject_set_hash                TEXT,
    subject_definition_hash         TEXT,
    subject_set_count               INTEGER CHECK (subject_set_count IS NULL OR subject_set_count >= 0),
    beneficiary_class               TEXT CHECK (beneficiary_class IS NULL OR beneficiary_class IN ('player', 'affiliate', 'staff', 'platform')),

    -- 'tenant' | 'platform' | 'provider:<id>' | 'affiliate:<node_id>' -
    -- an open-ended value under a closed set of prefixes, the identical
    -- shape ADR 0032's own funding_source/fulfillment_owner fields
    -- already use in this codebase (doc 10 §3.1) rather than a second,
    -- normalized provider-reference table this dispatch has no mandate
    -- to design.
    economic_owner                  TEXT NOT NULL CHECK (
                                         economic_owner IN ('tenant', 'platform')
                                         OR economic_owner LIKE 'provider:%'
                                         OR economic_owner LIKE 'affiliate:%'
                                     ),

    asset_code                      TEXT REFERENCES assets (code),
    -- NUMERIC(38,0), never int64/float64 (CLAUDE.md; doc 34 §2.2's own
    -- explicit "never int64, never floating point" instruction).
    intended_aggregate_value        NUMERIC(38, 0) CHECK (intended_aggregate_value IS NULL OR intended_aggregate_value >= 0),
    -- RK-W15P2-5 (doc 34 §2.2): a null asset_code EOI has no enforceable
    -- value budget. Enforced structurally, not left to caller discipline.
    CHECK (asset_code IS NOT NULL OR intended_aggregate_value IS NULL),

    recipient_ceiling                INTEGER CHECK (recipient_ceiling IS NULL OR recipient_ceiling >= 0),
    per_window_ceiling               INTEGER CHECK (per_window_ceiling IS NULL OR per_window_ceiling >= 0),
    ceiling_window                   INTERVAL,
    CHECK ((per_window_ceiling IS NULL) = (ceiling_window IS NULL)),
    value_measure_basis              JSONB,

    parent_operation_id              UUID REFERENCES economic_operations (operation_id),
    -- Denormalized (doc 34 §2.2: "the field the whole enforcement
    -- mechanism turns on... every budget is a subtree-wide aggregate
    -- keyed on root_operation_id, never on parent_operation_id alone").
    -- Equals operation_id for a root row itself (set by the inserting
    -- caller, since a self-referencing default is not expressible
    -- declaratively - Phase 3's repository layer is responsible for
    -- this, mirrored by the CHECK below for a root row specifically).
    root_operation_id                UUID NOT NULL,
    lineage_kind                     TEXT NOT NULL CHECK (lineage_kind IN ('root', 'retry', 'resume', 'page', 'item', 'compensation')),
    CHECK ((lineage_kind = 'root') = (parent_operation_id IS NULL)),
    CHECK (lineage_kind <> 'root' OR root_operation_id = operation_id),
    batch_ordinal                    INTEGER,
    batch_total                      INTEGER,

    approval_state                   TEXT NOT NULL DEFAULT 'pending' CHECK (approval_state IN (
        'not_required', 'pending', 'approved', 'rejected', 'expired', 'consumed', 'revoked'
    )),
    -- Fail-closed default mirrors internal/withdrawal/policy.go's
    -- defaultApprovalPolicy (2 required approvals, threshold 0) - doc 34
    -- §2.2's own cited precedent.
    required_approvals               INTEGER NOT NULL DEFAULT 2 CHECK (required_approvals >= 0),
    approvals_received               INTEGER NOT NULL DEFAULT 0 CHECK (approvals_received >= 0),
    threshold_at_decision            NUMERIC(38, 0),
    required_approvals_at_decision   INTEGER,
    -- Referenced here, CONSUMED at the enforcement point (doc 34 §2.2:
    -- "a reference is not a control"). No FK: the concrete approval-row
    -- table is each adopting domain's own (e.g. a future
    -- bonus_change_approvals row, doc 25/security §B1.2) - naming that
    -- table is Phase 3's decision, not this migration's.
    approval_refs                    UUID[] NOT NULL DEFAULT '{}',
    pinned_payload                   JSONB,

    -- The MINTING idempotency key (doc 34 §3.3) - distinct from any
    -- per-call idempotency key of the executions this EOI authorizes.
    idempotency_key                  TEXT NOT NULL,
    correlation_id                   UUID NOT NULL,
    audit_record_id                  UUID,
    -- clock_timestamp(), not now() - doc 10 W1's common object contract,
    -- adopted verbatim by doc 34 (a control object with the identical
    -- time-sensitivity discipline as every other bonus-adjacent record).
    created_at                       TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    created_by                       UUID,

    status                           TEXT NOT NULL DEFAULT 'open' CHECK (status IN ('open', 'exhausted', 'completed', 'aborted', 'superseded')),
    expires_at                       TIMESTAMPTZ NOT NULL,

    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id)
);

-- Mint-once (doc 34 §3.3): a retried minting call must resolve to the
-- SAME EOI, never create a second one.
ALTER TABLE economic_operations ADD CONSTRAINT economic_operations_tenant_idempotency_key UNIQUE (tenant_id, idempotency_key);

-- Composite FK targets, mirroring brands'/wallets'/ledger_transactions'
-- own (id, tenant_id) precedent throughout this codebase - so a row can
-- never name a parent/root belonging to a DIFFERENT tenant.
ALTER TABLE economic_operations ADD CONSTRAINT economic_operations_id_tenant_key UNIQUE (operation_id, tenant_id);
ALTER TABLE economic_operations
    ADD CONSTRAINT economic_operations_parent_tenant_fk
        FOREIGN KEY (parent_operation_id, tenant_id) REFERENCES economic_operations (operation_id, tenant_id);
ALTER TABLE economic_operations
    ADD CONSTRAINT economic_operations_root_tenant_fk
        FOREIGN KEY (root_operation_id, tenant_id) REFERENCES economic_operations (operation_id, tenant_id);

CREATE INDEX idx_economic_operations_tenant ON economic_operations (tenant_id);
-- "EOI lookup by root_operation_id for the budget-consumption join"
-- (this dispatch's own stated requirement) - the read every enforcement
-- consume and every reconciliation sweep performs.
CREATE INDEX idx_economic_operations_root ON economic_operations (root_operation_id);
CREATE INDEX idx_economic_operations_parent ON economic_operations (parent_operation_id) WHERE parent_operation_id IS NOT NULL;
CREATE INDEX idx_economic_operations_type_status ON economic_operations (tenant_id, operation_type, status);

ALTER TABLE economic_operations ENABLE ROW LEVEL SECURITY;
ALTER TABLE economic_operations FORCE ROW LEVEL SECURITY;

-- EOI-10 (doc 34 §6): tenant_id NOT NULL, FORCE RLS, the
-- app.player_account_id IS NULL conjunct, and NO player-facing or
-- affiliate-facing read policy at all - staff/system only, mirroring
-- risk_rules'/asset_change_requests' per-command split (no FOR ALL,
-- migration 0047 Fix 3's lesson) rather than withdrawal_requests' dual
-- scope, since an EOI is never itself a player-visible record.
CREATE POLICY tenant_isolation_read ON economic_operations
    FOR SELECT
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

CREATE POLICY tenant_isolation_insert ON economic_operations
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

CREATE POLICY tenant_isolation_update ON economic_operations
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    )
    WITH CHECK (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );

-- No DELETE policy at all (migration 0047 Fix 3's precedent, restated by
-- security-architecture.md §B1.3: deleting an authorization-lineage row
-- would silently erase the ceiling it was created to enforce). A
-- DELETE therefore affects zero rows under RLS rather than succeeding.

CREATE TRIGGER economic_operations_no_truncate
    BEFORE TRUNCATE ON economic_operations
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();
