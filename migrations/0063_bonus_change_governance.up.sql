-- Dual-control (four-eyes) governance infrastructure for Bonus Engine
-- (docs/security/security-architecture.md "security doc" §B1.2/§B1.3),
-- deferred by Phase 2, built here by bonus-engine's own Phase 3
-- implementation stage: bonus_change_requests / bonus_change_approvals /
-- bonus_approval_policies, modeled on withdrawal_requests/
-- withdrawal_approvals/withdrawal_policies (migrations 0018/0034) and
-- asset_change_requests/asset_change_approvals (migrations 0044/0047),
-- per the security doc's own explicit instruction to reuse that pattern
-- rather than invent one.
--
-- The seven dual-controlled bonus operations (security doc §B1.2, items
-- 1-7): manual Grant issuance above threshold, BulkJob execution
-- (always), direct bonus Adjustment above threshold, staff-forced
-- conversion / manual release override (same threshold as Adjustment),
-- Campaign activation above a cost-tier bound, Offer version publication
-- above the item-1 threshold, and cancellation/forfeiture of a
-- `completed` Grant above threshold. Plus the doc-34-driven
-- bonus_held_disposition_resolution item this document's own §W15.1.12/
-- doc 34 §3.1 add as an eighth, distinct dual-controlled operation
-- (`held_disposition_resolve`).
--
-- security doc §B1.3's universal RLS rules (tenant_id NOT NULL, FORCE
-- RLS, composite brand FK, no player_account_id as an independent
-- client-supplied id, BEFORE TRUNCATE deny, no FOR ALL policy) apply to
-- every table below.

-- bonus_approval_policies: append-only threshold configuration, shaped on
-- withdrawal_policies (security doc §B1.2 item 6). A change is always a
-- NEW row, never an edit - "an editable threshold row leaves a loosened
-- threshold in place with no trace" (withdrawal_policies_deny_update's
-- own rationale, reused verbatim here).
CREATE TABLE bonus_approval_policies (
    id                            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                     UUID NOT NULL,
    brand_id                      UUID,
    asset_code                    TEXT REFERENCES assets (code),
    operation                     TEXT NOT NULL CHECK (operation IN (
        'manual_grant_issue', 'bulk_job_execute', 'bonus_adjustment_write',
        'grant_forced_conversion', 'campaign_activate', 'offer_publish',
        'grant_cancel_completed', 'held_disposition_resolve'
    )),
    approval_threshold_minor_units NUMERIC(38, 0) NOT NULL CHECK (approval_threshold_minor_units >= 0),
    required_approvals            INTEGER NOT NULL DEFAULT 2 CHECK (required_approvals >= 1),
    effective_from                TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    created_by_principal_id       UUID,
    created_at                    TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),

    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id)
);

CREATE INDEX idx_bonus_approval_policies_lookup
    ON bonus_approval_policies (tenant_id, operation, brand_id, asset_code, effective_from DESC);

CREATE FUNCTION bonus_approval_policies_deny_update() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'bonus_approval_policies: rows are insert-only - create a new row instead of updating an existing one (security-architecture.md §B1.2 item 6)';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER bonus_approval_policies_deny_update
    BEFORE UPDATE ON bonus_approval_policies
    FOR EACH ROW EXECUTE FUNCTION bonus_approval_policies_deny_update();
CREATE TRIGGER bonus_approval_policies_deny_delete
    BEFORE DELETE ON bonus_approval_policies
    FOR EACH ROW EXECUTE FUNCTION bonus_approval_policies_deny_update();
CREATE TRIGGER bonus_approval_policies_no_truncate
    BEFORE TRUNCATE ON bonus_approval_policies
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

ALTER TABLE bonus_approval_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE bonus_approval_policies FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_staff_read ON bonus_approval_policies
    FOR SELECT
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
CREATE POLICY tenant_staff_insert ON bonus_approval_policies
    FOR INSERT
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- bonus_change_requests
CREATE TABLE bonus_change_requests (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NOT NULL,
    brand_id                  UUID,
    operation                 TEXT NOT NULL CHECK (operation IN (
        'manual_grant_issue', 'bulk_job_execute', 'bonus_adjustment_write',
        'grant_forced_conversion', 'campaign_activate', 'offer_publish',
        'grant_cancel_completed', 'held_disposition_resolve'
    )),
    -- Polymorphic reference to the row this request governs: bonus_grants
    -- (manual_grant_issue/bonus_adjustment_write/grant_forced_conversion/
    -- grant_cancel_completed), bulk_grant_jobs (bulk_job_execute),
    -- bonus_campaigns (campaign_activate), bonus_offer_versions
    -- (offer_publish), or bonus_held_dispositions
    -- (held_disposition_resolve). No FK (the referent table varies by
    -- operation) - the application layer is responsible for resolving
    -- target_id against the correct table for target_type, mirroring
    -- economic_operations' own "no FK, each adopting domain's own table"
    -- posture for approval_refs.
    target_type               TEXT NOT NULL,
    target_id                 UUID NOT NULL,
    -- The exact content the approval is FOR (security doc: "the JSONB
    -- payload match is not optional decoration: for a bulk job it pins
    -- the recipient-set hash, for an adjustment the amount and asset, for
    -- an activation the Offer version id").
    payload                   JSONB NOT NULL,
    amount_at_request         NUMERIC(38, 0),
    asset_code                TEXT REFERENCES assets (code),
    reason_code               TEXT NOT NULL,
    requested_by_principal_id UUID NOT NULL,
    requested_at              TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    state                     TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'applied', 'rejected', 'cancelled')),
    applied_by_principal_id   UUID,
    applied_at                TIMESTAMPTZ,
    CHECK ((state = 'applied') = (applied_at IS NOT NULL)),

    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id)
);

CREATE INDEX idx_bonus_change_requests_tenant ON bonus_change_requests (tenant_id);
CREATE INDEX idx_bonus_change_requests_lookup ON bonus_change_requests (tenant_id, operation, target_id, state);
CREATE INDEX idx_bonus_change_requests_state ON bonus_change_requests (tenant_id, state);

-- Immutability (security doc, item 2): BEFORE UPDATE rejects any change
-- to the request-defining fields, and rejects any transition OUT of a
-- non-pending state - the only mutation this table ever legitimately
-- undergoes after insert is the consume function's own pending->applied
-- transition (which sets applied_by_principal_id/applied_at), or an
-- explicit reject/cancel (state only). "Without this, the control is
-- bypassed by filing a harmless request, collecting the approval, then
-- rewriting the payload."
CREATE FUNCTION bonus_change_requests_enforce_immutability() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.brand_id IS DISTINCT FROM OLD.brand_id
        OR NEW.operation IS DISTINCT FROM OLD.operation
        OR NEW.target_type IS DISTINCT FROM OLD.target_type
        OR NEW.target_id IS DISTINCT FROM OLD.target_id
        OR NEW.payload IS DISTINCT FROM OLD.payload
        OR NEW.amount_at_request IS DISTINCT FROM OLD.amount_at_request
        OR NEW.asset_code IS DISTINCT FROM OLD.asset_code
        OR NEW.reason_code IS DISTINCT FROM OLD.reason_code
        OR NEW.requested_by_principal_id IS DISTINCT FROM OLD.requested_by_principal_id
        OR NEW.requested_at IS DISTINCT FROM OLD.requested_at
    THEN
        RAISE EXCEPTION 'bonus_change_requests: request-defining fields are immutable after insert';
    END IF;
    IF OLD.state <> 'pending' AND NEW.state IS DISTINCT FROM OLD.state THEN
        RAISE EXCEPTION 'bonus_change_requests: cannot transition out of a non-pending state (current state %)', OLD.state;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER bonus_change_requests_enforce_immutability
    BEFORE UPDATE ON bonus_change_requests
    FOR EACH ROW EXECUTE FUNCTION bonus_change_requests_enforce_immutability();
CREATE TRIGGER bonus_change_requests_deny_delete
    BEFORE DELETE ON bonus_change_requests
    FOR EACH ROW EXECUTE FUNCTION bonus_approval_policies_deny_update();
CREATE TRIGGER bonus_change_requests_no_truncate
    BEFORE TRUNCATE ON bonus_change_requests
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

ALTER TABLE bonus_change_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE bonus_change_requests FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_staff_read ON bonus_change_requests
    FOR SELECT
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
CREATE POLICY tenant_staff_insert ON bonus_change_requests
    FOR INSERT
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
CREATE POLICY tenant_staff_update ON bonus_change_requests
    FOR UPDATE
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- bonus_change_approvals
CREATE TABLE bonus_change_approvals (
    id                         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                  UUID NOT NULL,
    request_id                 UUID NOT NULL REFERENCES bonus_change_requests (id),
    approver_principal_id      UUID NOT NULL,
    decision                   TEXT NOT NULL CHECK (decision IN ('approve', 'reject')),
    reason_code                TEXT,
    CHECK (decision <> 'reject' OR reason_code IS NOT NULL),
    decided_at                 TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    -- withdrawal_approvals' *_at_decision precedent (security doc): "what
    -- stops a later threshold change from retroactively making a past
    -- decision look compliant... when the record is read during a
    -- dispute."
    threshold_at_decision      NUMERIC(38, 0),
    amount_at_decision         NUMERIC(38, 0),

    UNIQUE (request_id, approver_principal_id)
);

CREATE INDEX idx_bonus_change_approvals_tenant ON bonus_change_approvals (tenant_id);
CREATE INDEX idx_bonus_change_approvals_request ON bonus_change_approvals (tenant_id, request_id);

CREATE TRIGGER bonus_change_approvals_deny_update
    BEFORE UPDATE ON bonus_change_approvals
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();
CREATE TRIGGER bonus_change_approvals_deny_delete
    BEFORE DELETE ON bonus_change_approvals
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();
CREATE TRIGGER bonus_change_approvals_no_truncate
    BEFORE TRUNCATE ON bonus_change_approvals
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- Governance trigger (security doc, item 3): modeled on migration 0034's
-- withdrawal_approvals_enforce_governance(), explicitly NOT on migration
-- 0029. For BOTH requester and approver: refuse if unresolvable, refuse
-- if not active, refuse if no person linkage, refuse if wrong tenant;
-- then compare person_id UNCONDITIONALLY. No service-identity carve-out
-- (unlike withdrawal's below-threshold automated-approval exemption): "no
-- service identity files or decides a bonus adjustment."
CREATE FUNCTION bonus_change_approvals_enforce_governance() RETURNS TRIGGER AS $$
DECLARE
    v_request               bonus_change_requests%ROWTYPE;
    v_requester_person_id   UUID;
    v_requester_status      TEXT;
    v_requester_tenant_id   UUID;
    v_approver_person_id    UUID;
    v_approver_status       TEXT;
    v_approver_tenant_id    UUID;
BEGIN
    SELECT * INTO v_request FROM bonus_change_requests WHERE id = NEW.request_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'bonus_change_approvals: request % does not exist', NEW.request_id;
    END IF;
    IF NEW.tenant_id IS DISTINCT FROM v_request.tenant_id THEN
        RAISE EXCEPTION 'bonus_change_approvals: approval tenant does not match request tenant';
    END IF;

    SELECT su.person_id, su.status, su.tenant_id
      INTO v_requester_person_id, v_requester_status, v_requester_tenant_id
      FROM staff_users su WHERE su.id = v_request.requested_by_principal_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'bonus_change_approvals: requester identity cannot be resolved and is not eligible to have filed a bonus change request';
    END IF;
    IF v_requester_person_id IS NULL THEN
        RAISE EXCEPTION 'bonus_change_approvals: requester has no confirmed Person linkage';
    END IF;
    IF v_requester_status IS DISTINCT FROM 'active' THEN
        RAISE EXCEPTION 'bonus_change_approvals: requester staff account is not active';
    END IF;
    IF v_requester_tenant_id IS DISTINCT FROM v_request.tenant_id THEN
        RAISE EXCEPTION 'bonus_change_approvals: requester belongs to a different tenant than the request';
    END IF;

    SELECT su.person_id, su.status, su.tenant_id
      INTO v_approver_person_id, v_approver_status, v_approver_tenant_id
      FROM staff_users su WHERE su.id = NEW.approver_principal_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'bonus_change_approvals: approver identity cannot be resolved and is not eligible to record bonus change decisions';
    END IF;
    IF v_approver_person_id IS NULL THEN
        RAISE EXCEPTION 'bonus_change_approvals: approver has no confirmed Person linkage';
    END IF;
    IF v_approver_status IS DISTINCT FROM 'active' THEN
        RAISE EXCEPTION 'bonus_change_approvals: approver staff account is not active';
    END IF;
    IF v_approver_tenant_id IS DISTINCT FROM v_request.tenant_id THEN
        RAISE EXCEPTION 'bonus_change_approvals: approver belongs to a different tenant than the request';
    END IF;

    -- Unconditional comparison (no "IS NOT NULL AND" guard) - both sides
    -- were already proven non-NULL above by the refusal branches, but the
    -- comparison itself carries no additional guard per §W15.1.3's own
    -- warning against reproducing migration 0029's inertness.
    IF v_requester_person_id = v_approver_person_id THEN
        RAISE EXCEPTION 'bonus_change_approvals: approver resolves to the same person as the requester (self-approval)';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER bonus_change_approvals_enforce_governance
    BEFORE INSERT ON bonus_change_approvals
    FOR EACH ROW EXECUTE FUNCTION bonus_change_approvals_enforce_governance();

-- SEP-1 (security-architecture.md §W15.1, REQ-SEP-BONUS-1/4): the
-- ADDITIONAL, distinct check that neither the requester nor the approver
-- of a dual-controlled bonus operation resolves to the SAME PERSON as the
-- operation's own BENEFICIARY (the player the value moves to/from) - a
-- wholly different comparison than bonus_change_approvals_enforce_
-- governance() above (which checks requester != approver, ordinary
-- four-eyes). Both triggers fire on the same INSERT; both must pass.
--
-- Resolvable, materialized beneficiary sets only (security doc §W15.1.2):
-- single_subject for manual_grant_issue/bonus_adjustment_write/
-- grant_forced_conversion/grant_cancel_completed (target bonus_grants)
-- and held_disposition_resolve (target bonus_held_dispositions, joined to
-- its Grant); an ENUMERATED, PINNED set for bulk_job_execute when
-- target_kind IN ('single_player', 'player_list') (both fully
-- materialized on the bulk_grant_jobs row itself at approval time).
--
-- NAMED, DELIBERATE GAP (not silently worked around): bulk_job_execute
-- for target_kind IN ('segment', 'segment_set') has NO materialized
-- recipient set at approval time (doc 10 W5: "resolved LIVE at run
-- time", precisely so a since-excluded player is never included from a
-- stale list) - there is structurally nothing for THIS trigger to compare
-- the approver against for those two target kinds, since segmentation
-- (internal/segment) is out of this Wave's scope. This trigger explicitly
-- does NOT fail closed on that case (doing so would make every
-- segment-targeted bulk job unapprovable, which is not this dispatch's
-- call to make unilaterally) - instead, internal/bonus's own Go
-- job-runner performs an ADDITIONAL, per-item SEP-1 check at the actual
-- moment each item's Grant would issue (comparing the requester's AND
-- every approver's Person against that specific item's player), which is
-- a live, per-item, un-bypassable gate at the true value-moving instant
-- even though it is not this migration's DB-trigger shape. See
-- internal/bonus's own doc comment on this for the full citation.
-- campaign_activate/offer_publish are NOT in SEP-1's scope at all (no
-- single beneficiary - four-eyes governance above already covers them).
CREATE FUNCTION bonus_change_approvals_enforce_separation() RETURNS TRIGGER AS $$
DECLARE
    v_request            bonus_change_requests%ROWTYPE;
    v_subject_person_id  UUID;
    v_target_kind        TEXT;
    v_single_player      UUID;
    v_player_list        UUID[];
    v_requester_person   UUID;
    v_approver_person    UUID;
BEGIN
    SELECT * INTO v_request FROM bonus_change_requests WHERE id = NEW.request_id;

    IF v_request.operation IN ('manual_grant_issue', 'bonus_adjustment_write', 'grant_forced_conversion', 'grant_cancel_completed') THEN
        SELECT pa.person_id INTO v_subject_person_id
          FROM bonus_grants g JOIN player_accounts pa ON pa.id = g.player_account_id
         WHERE g.id = v_request.target_id;
        IF v_subject_person_id IS NULL THEN
            RAISE EXCEPTION 'SEP-1: refuse — beneficiary Person could not be resolved for bonus change request %', v_request.id;
        END IF;

    ELSIF v_request.operation = 'held_disposition_resolve' THEN
        SELECT pa.person_id INTO v_subject_person_id
          FROM bonus_held_dispositions hd
          JOIN bonus_grants g ON g.id = hd.grant_id
          JOIN player_accounts pa ON pa.id = g.player_account_id
         WHERE hd.id = v_request.target_id;
        IF v_subject_person_id IS NULL THEN
            RAISE EXCEPTION 'SEP-1: refuse — beneficiary Person could not be resolved for held-disposition resolution request %', v_request.id;
        END IF;

    ELSIF v_request.operation = 'bulk_job_execute' THEN
        SELECT j.target_kind, j.target_player_account_id, j.target_player_list
          INTO v_target_kind, v_single_player, v_player_list
          FROM bulk_grant_jobs j WHERE j.id = v_request.target_id;

        IF v_target_kind IS NULL THEN
            RAISE EXCEPTION 'SEP-1: refuse — bulk grant job % could not be resolved', v_request.target_id;
        END IF;

        IF v_target_kind = 'single_player' THEN
            SELECT pa.person_id INTO v_subject_person_id FROM player_accounts pa WHERE pa.id = v_single_player;
            IF v_subject_person_id IS NULL THEN
                RAISE EXCEPTION 'SEP-1: refuse — beneficiary Person could not be resolved for bulk grant job %', v_request.target_id;
            END IF;
            SELECT su.person_id INTO v_requester_person FROM staff_users su WHERE su.id = v_request.requested_by_principal_id;
            SELECT su.person_id INTO v_approver_person FROM staff_users su WHERE su.id = NEW.approver_principal_id;
            IF v_requester_person = v_subject_person_id OR v_approver_person = v_subject_person_id THEN
                RAISE EXCEPTION 'SEP-1: refuse — requester or approver resolves to the same person as the bulk job''s sole targeted player (self-dealing)';
            END IF;
            RETURN NEW;

        ELSIF v_target_kind = 'player_list' THEN
            SELECT su.person_id INTO v_requester_person FROM staff_users su WHERE su.id = v_request.requested_by_principal_id;
            SELECT su.person_id INTO v_approver_person FROM staff_users su WHERE su.id = NEW.approver_principal_id;
            IF EXISTS (
                SELECT 1 FROM player_accounts pa
                 WHERE pa.id = ANY (v_player_list)
                   AND pa.person_id IN (v_requester_person, v_approver_person)
            ) THEN
                RAISE EXCEPTION 'SEP-1: refuse — requester or approver resolves to the same person as a player on the bulk job''s pinned recipient list (self-dealing)';
            END IF;
            RETURN NEW;
        ELSE
            -- segment / segment_set: NAMED GAP above - no materialized
            -- set to compare against at this trigger's own level. Not
            -- refused, not silently treated as passed either - see this
            -- function's own doc comment and internal/bonus's
            -- compensating per-item runtime check.
            RETURN NEW;
        END IF;

    ELSE
        -- campaign_activate / offer_publish: not SEP-1-scoped.
        RETURN NEW;
    END IF;

    SELECT su.person_id INTO v_requester_person FROM staff_users su WHERE su.id = v_request.requested_by_principal_id;
    SELECT su.person_id INTO v_approver_person FROM staff_users su WHERE su.id = NEW.approver_principal_id;

    IF v_requester_person = v_subject_person_id THEN
        RAISE EXCEPTION 'SEP-1: refuse — requester resolves to the same person as the operation''s own beneficiary (self-dealing)';
    END IF;
    IF v_approver_person = v_subject_person_id THEN
        RAISE EXCEPTION 'SEP-1: refuse — approver resolves to the same person as the operation''s own beneficiary (self-dealing)';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER bonus_change_approvals_enforce_separation
    BEFORE INSERT ON bonus_change_approvals
    FOR EACH ROW EXECUTE FUNCTION bonus_change_approvals_enforce_separation();

-- Single consume function (security doc, item 4), mirroring
-- asset_change_consume_approved_request's 3-argument form: select the
-- oldest pending request matching (tenant, operation, target) whose
-- payload @> p_payload_match, FOR UPDATE, requiring at least
-- required_approvals distinct approve rows all with
-- approver_principal_id <> requested_by_principal_id, and NO reject row
-- at all; mark it applied IN THE SAME STATEMENT.
CREATE FUNCTION bonus_change_consume_approved_request(
    p_tenant_id UUID, p_operation TEXT, p_target_id UUID, p_payload_match JSONB, p_required_approvals INTEGER, p_applied_by UUID
) RETURNS UUID AS $$
DECLARE
    v_request_id UUID;
BEGIN
    SELECT r.id INTO v_request_id
      FROM bonus_change_requests r
     WHERE r.tenant_id = p_tenant_id
       AND r.operation = p_operation
       AND r.target_id = p_target_id
       AND r.state = 'pending'
       AND r.payload @> p_payload_match
       AND (
           SELECT COUNT(DISTINCT a.approver_principal_id)
             FROM bonus_change_approvals a
            WHERE a.request_id = r.id
              AND a.decision = 'approve'
              AND a.approver_principal_id <> r.requested_by_principal_id
       ) >= p_required_approvals
       AND NOT EXISTS (
           SELECT 1 FROM bonus_change_approvals a
            WHERE a.request_id = r.id AND a.decision = 'reject'
       )
     ORDER BY r.requested_at
     FOR UPDATE
     LIMIT 1;

    IF v_request_id IS NULL THEN
        RAISE EXCEPTION 'bonus_change_requests: % of % requires a pending bonus_change_requests row (matching %) with % distinct approve decisions from principals other than the requester, and no reject', p_operation, p_target_id, p_payload_match, p_required_approvals;
    END IF;

    UPDATE bonus_change_requests
       SET state = 'applied', applied_at = clock_timestamp(), applied_by_principal_id = p_applied_by
     WHERE id = v_request_id;

    RETURN v_request_id;
END;
$$ LANGUAGE plpgsql;

ALTER TABLE bonus_change_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE bonus_change_approvals FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_staff_read ON bonus_change_approvals
    FOR SELECT
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
CREATE POLICY tenant_staff_insert ON bonus_change_approvals
    FOR INSERT
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
