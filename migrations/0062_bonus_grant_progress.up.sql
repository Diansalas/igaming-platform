-- bonus_grant_progress - the general Grant lifecycle Progress trail
-- (docs/architecture/10-bonus-engine-architecture.md "doc 10" §1.1's
-- fourth persistent layer, §10.1's completeness requirement), DISTINCT
-- from bonus_wagering_progress (migration 0058, which is narrowly the
-- P_net/P_firm contribution record). Deferred by Phase 2, built here by
-- bonus-engine's own Phase 3 implementation stage.
--
-- Doc 10 §10.1, verbatim: "Every row in §1.3's transition table appends a
-- Progress entry - no transition is ever applied to a Grant without a
-- corresponding, immutable Progress row in the same transaction. Each
-- Progress entry carries: a monotonic sequence number..., the transition
-- type, the trigger type and its concrete reference..., the before-state
-- and after-state, any amounts/contribution detail relevant to that
-- transition, a reason code..., the Risk/RG decision(s) consulted...
-- where applicable, and - once a lifecycle event has been posted - the
-- resulting ledger transaction group id."
--
-- Also the storage backing W2.4's GrantActivation/W2.10's
-- BonusAdjustment/W2.11's BonusCancellation/W2.12's BonusExpiry "typed
-- projection, not a side table" pattern: those are all read-model VIEWS
-- over specific transition_type rows of this one append-only table, per
-- doc 10's own explicit instruction not to duplicate an already-
-- append-only record into a second table per named object.
CREATE TABLE bonus_grant_progress (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id         UUID NOT NULL,
    -- Denormalized from the owning Grant (mirrors bonus_wagering_progress's
    -- own "necessary denormalization... structural precondition for the
    -- RLS shape" rationale, migration 0058) so this table's own RLS does
    -- not need to subquery through bonus_grants for either scope.
    brand_id          UUID NOT NULL,
    player_account_id UUID NOT NULL,
    grant_id          UUID NOT NULL,

    -- Monotonic, append-only ordering WITHIN the Grant (§10.1). Assigned
    -- by the application as
    -- "1 + COALESCE(MAX(sequence_number) WHERE grant_id = ..., 0)" under
    -- the same (tenant_id, grant_id) advisory lock every mutating Grant
    -- transition already takes (doc10 §9) - never a bare SERIAL, which
    -- would be table-global rather than per-Grant.
    sequence_number   BIGINT NOT NULL CHECK (sequence_number > 0),

    -- Every transition doc10 §1.3's table names, PLUS the non-terminal
    -- gate-denial/administrative events §10's own audit table and N1's
    -- G-2 mechanism require a Progress entry for (activation/conversion
    -- denials that do not change status; a pending_settlement deferral;
    -- a held-disposition creation/resolution; a wagering contribution or
    -- its reversal per gap 9/§6.6.9; a manual adjustment; a suggestion
    -- activation back-reference).
    transition_type   TEXT NOT NULL CHECK (transition_type IN (
        'issued', 'activated', 'in_progress_contribution', 'in_progress_contribution_reversed',
        'completed', 'converted', 'conversion_blocked',
        'expired', 'cancelled', 'forfeited',
        'pending_settlement_deferred', 'pending_settlement_finalized',
        'reversed',
        'activation_denied', 'reward_credit_denied',
        'held_disposition_created', 'held_disposition_resolved',
        'adjustment_applied', 'suggestion_activation_linked'
    )),

    trigger_type      TEXT NOT NULL CHECK (trigger_type IN (
        'automated_rule_evaluation', 'player_action', 'staff_action', 'provider_callback', 'system'
    )),
    trigger_reference TEXT,

    before_status     TEXT,
    after_status      TEXT,

    -- internal/audit.ActorType verbatim (doc 10 N2.3), never a Bonus-
    -- local redefinition - see internal/bonus.ActorType's own doc
    -- comment.
    actor_type        TEXT NOT NULL CHECK (actor_type IN ('player', 'staff', 'service', 'system')),
    actor_id          UUID,
    CHECK ((actor_type = 'system') = (actor_id IS NULL)),

    -- Mandatory for forfeited/cancelled/reversed (§10.1), optional
    -- elsewhere - enforced at the application layer (this migration does
    -- not know, per row, which transition_type values are which without
    -- duplicating §1.3's whole table into a CHECK; the Go lifecycle layer
    -- is the single writer and enforces it before every insert).
    reason_code       TEXT,

    amount            NUMERIC(38, 0) CHECK (amount IS NULL OR amount >= 0),
    asset_code        TEXT REFERENCES assets (code),

    -- The Risk/RG/AssetAuthorization decision(s) consulted for this
    -- transition, by CODE only, never full internal detail (§10.1).
    risk_decision_code               TEXT,
    rg_decision_code                 TEXT,
    asset_authorization_reason_code  TEXT,

    -- "the resulting ledger transaction group id... so a Grant's Progress
    -- trail and its ledger footprint are always cross-referenceable
    -- without being the same record" (§10.1).
    ledger_transaction_id            UUID,

    correlation_id    UUID,
    -- Free-form structured detail (e.g. every outstanding exposure record
    -- enumerated at a pending_settlement deferral, per N1.4 step 4: "A
    -- Progress entry records the deferral, enumerating every outstanding
    -- exposure record... so the trail is inspectable, not merely
    -- asserted").
    detail            JSONB NOT NULL DEFAULT '{}'::jsonb,

    occurred_at       TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),

    FOREIGN KEY (grant_id, tenant_id) REFERENCES bonus_grants (id, tenant_id),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id),
    FOREIGN KEY (player_account_id, tenant_id) REFERENCES player_accounts (id, tenant_id),
    FOREIGN KEY (ledger_transaction_id, tenant_id) REFERENCES ledger_transactions (id, tenant_id)
);

ALTER TABLE bonus_grant_progress
    ADD CONSTRAINT bonus_grant_progress_grant_sequence_key UNIQUE (tenant_id, grant_id, sequence_number);

CREATE INDEX idx_bonus_grant_progress_tenant ON bonus_grant_progress (tenant_id);
CREATE INDEX idx_bonus_grant_progress_grant ON bonus_grant_progress (tenant_id, grant_id, sequence_number);
CREATE INDEX idx_bonus_grant_progress_player ON bonus_grant_progress (tenant_id, player_account_id);
CREATE INDEX idx_bonus_grant_progress_type ON bonus_grant_progress (tenant_id, transition_type);

-- Append-only (§1.1's common object contract / §10.1: "no transition is
-- ever applied... without a corresponding, IMMUTABLE Progress row").
CREATE FUNCTION bonus_grant_progress_deny_mutation() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'bonus_grant_progress: rows are immutable and append-only (doc 10 §10.1)';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER bonus_grant_progress_deny_update
    BEFORE UPDATE ON bonus_grant_progress
    FOR EACH ROW EXECUTE FUNCTION bonus_grant_progress_deny_mutation();

CREATE TRIGGER bonus_grant_progress_deny_delete
    BEFORE DELETE ON bonus_grant_progress
    FOR EACH ROW EXECUTE FUNCTION bonus_grant_progress_deny_mutation();

CREATE TRIGGER bonus_grant_progress_no_truncate
    BEFORE TRUNCATE ON bonus_grant_progress
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- SEP-1 (security-architecture.md §W15.1, REQ-SEP-BONUS-1): the
-- ordinary, non-four-eyes-gated half of "adopt SEP-1 at Grant issuance/
-- activation, adjustment, forced conversion" - the four-eyes-gated half
-- (an action that ALSO requires a bonus_change_approvals row, migration
-- 0063) gets its own, additional SEP-1 trigger there on the approval
-- itself. This trigger covers every staff-actioned, value-affecting
-- transition regardless of whether it happened to clear a four-eyes
-- threshold, so a below-threshold manual grant/adjustment/forced
-- conversion is never left unchecked. Modeled on migration 0034's
-- withdrawal_approvals_enforce_governance() person-resolution shape per
-- security-architecture.md §W15.1.3 - explicitly NOT on migration 0029's
-- inert, NULL-guarded comparison.
CREATE FUNCTION bonus_grant_progress_enforce_separation() RETURNS TRIGGER AS $$
DECLARE
    v_actor_person_id      UUID;
    v_actor_status         TEXT;
    v_actor_tenant_id      UUID;
    v_subject_person_id    UUID;
BEGIN
    -- Only a staff-actioned, value-affecting transition is in SEP-1's
    -- scope (REQ-SEP-BONUS-1: "Grant issuance/activation, adjustment,
    -- forced conversion"). A system/automated/player-initiated transition
    -- has no staff principal to self-deal as; 'held_disposition_created'
    -- is explicitly TECHNICAL, never SEP-1-gated (doc 10 N1.8.1 row 7:
    -- "recording-and-parking a fact"). 'converted' via a staff override
    -- is the "forced conversion" case and IS in scope.
    IF NEW.actor_type <> 'staff' THEN
        RETURN NEW;
    END IF;
    IF NEW.transition_type NOT IN ('issued', 'activated', 'converted', 'adjustment_applied') THEN
        RETURN NEW;
    END IF;

    SELECT su.person_id, su.status, su.tenant_id
      INTO v_actor_person_id, v_actor_status, v_actor_tenant_id
      FROM staff_users su
     WHERE su.id = NEW.actor_id;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'SEP-1: refuse — acting staff principal % cannot be resolved to a staff_users row', NEW.actor_id;
    END IF;
    IF v_actor_person_id IS NULL THEN
        RAISE EXCEPTION 'SEP-1: refuse — acting staff principal % has no confirmed Person linkage', NEW.actor_id;
    END IF;
    IF v_actor_status IS DISTINCT FROM 'active' THEN
        RAISE EXCEPTION 'SEP-1: refuse — acting staff principal % is not active', NEW.actor_id;
    END IF;
    -- IS DISTINCT FROM, not bare <> (security-architecture.md §W15.1.3's
    -- corrected step 4): a platform_admin row has tenant_id IS NULL,
    -- which a bare <> against NEW.tenant_id would evaluate to NULL (never
    -- refusing). IS DISTINCT FROM refuses a platform_admin actor on every
    -- tenant-owned authorizing row, unconditionally.
    IF v_actor_tenant_id IS DISTINCT FROM NEW.tenant_id THEN
        RAISE EXCEPTION 'SEP-1: refuse — acting staff principal % belongs to a different tenant than the operation', NEW.actor_id;
    END IF;

    SELECT pa.person_id INTO v_subject_person_id
      FROM player_accounts pa
     WHERE pa.id = NEW.player_account_id;

    -- Empty/NULL beneficiary resolution is a refusal, never a skip
    -- (security-architecture.md §W15.1.1/§W15.1.4's SEP-1-H1 discipline).
    IF v_subject_person_id IS NULL THEN
        RAISE EXCEPTION 'SEP-1: refuse — beneficiary Person could not be resolved for player_account %', NEW.player_account_id;
    END IF;

    -- The comparison is UNCONDITIONAL - no "IF x IS NOT NULL AND" guard
    -- on either side (security-architecture.md §W15.1.3's explicit
    -- warning: that shape is what made migration 0029's check inert).
    IF v_actor_person_id = v_subject_person_id THEN
        RAISE EXCEPTION 'SEP-1: refuse — acting staff principal resolves to the same person as the Grant''s own player (self-dealing)';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER bonus_grant_progress_enforce_separation
    BEFORE INSERT ON bonus_grant_progress
    FOR EACH ROW EXECUTE FUNCTION bonus_grant_progress_enforce_separation();

ALTER TABLE bonus_grant_progress ENABLE ROW LEVEL SECURITY;
ALTER TABLE bonus_grant_progress FORCE ROW LEVEL SECURITY;

-- Dual scope, mirroring bonus_grants'/bonus_wagering_progress' own
-- pattern (doc 10 §8): tenant staff scope (full read) + player self scope
-- (read-only, own rows only) - a disputing player's own Progress trail is
-- exactly what §10's "the exact record a disputing player's case is
-- resolved from" requires be player-readable.
CREATE POLICY tenant_staff_read ON bonus_grant_progress
    FOR SELECT
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
CREATE POLICY player_self_read ON bonus_grant_progress
    FOR SELECT
    USING (
        player_account_id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
    );
CREATE POLICY tenant_staff_insert ON bonus_grant_progress
    FOR INSERT
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
