-- PRH-2 HSEC-APPROVED-HOLD-RELEASE-1 (ADR 0111 revision 2 section 6 + section 11;
-- owner decisions ADR 0095 section 44, decisions 13-18): the four-eyes release of
-- an `approved` withdrawal hold on a tenant/brand that is NOT active.
--
-- MUST RUN INSIDE A SINGLE TRANSACTION (db.MigrateUp does this).
--
-- Independent of 0122 (receipt anomaly) and 0123 (B13): it references no object
-- either creates. The payout instrument id of the withdrawal is read through
-- to_jsonb(withdrawal_requests) so the column may or may not exist yet.
--
-- What this adds (exactly the ADR 0111 section 6 object list):
--   1.  The classification `withdrawal_hold_resolution` (mandatory_four_eyes) and
--       the capability pair `withdrawal_hold_resolution:request|approve`
--       (eligible_tenant_roles = '{}': no tenant role is ever eligible; platform
--       grantees only, with the existing NOT NULL valid_until rule).
--   2.  financial_policy_required_approvals(): the 0113 body plus ONE line (the
--       non-active special case also covers this operation). Inert until a
--       platform policy row is approved through the existing governed flow.
--   3.  withdrawal_hold_resolutions / withdrawal_hold_resolution_approvals
--       (family A only: no tenant-staff, no player, no plain-platform policy),
--       their guards, the beneficiary guard (S-12), the counting function, the
--       deferred shape check and the CT-R3 key/correlation guards.
--   4.  The approved-hold FREEZE trigger on withdrawal_requests (security M-6).
--   5.  The FOUR SHARED objects, REPLACED IN PLACE (ADR 0111 section 7.2; migration
--       0125 builds on THESE bodies and its down restores THESE bodies byte for
--       byte): ledger_governed_fence_allows (+ branch (e)),
--       ledger_entries_governed_fence (+ the withdrawal_rejected shape),
--       the acting ledger_accounts INSERT policy (+ the hold-release hold account)
--       and the acting withdrawal_requests UPDATE policy (+ the hold-release
--       transition). Every other line of those four is the 0115 text.
--   6.  actor_proof_require(): the 0120 body plus the ADR 0110 operation table
--       restriction for withdrawal_hold_resolution:* (scope platform_acting with a
--       tenant only), and zz_actor_proof_guard on both new tables (governed
--       tables nine -> eleven).
--   7.  An acting FOR UPDATE lock-only policy on brands (the H-SEC lock discipline
--       needs `brands ... FOR SHARE` inside an acting session; WITH CHECK false).
--   8.  The REVOKE ALL + GRANT block for the two new tables.
--
-- SQLSTATE class 'HR' (hold resolution). Application code classifies by code only:
--   HR001 session/actor not permitted (only a platform_acting session may act)
--   HR002 actor has no linked person_id
--   HR003 actor lacks an in-force withdrawal_hold_resolution grant
--   HR010 precondition failed (withdrawal state, attempt exists, tenant+brand active)
--   HR011 a contributing policy's author/approver may not request or approve (S-2(iii))
--   HR014 withdrawal_hold_resolution is disabled (no in-force platform baseline)
--   HR020 the governed ledger key was used outside an executing resolution (CT-R3)
--   HR030 resolution guard (immutability, state machine)
--   HR031 approval guard
--   HR032 beneficiary exclusion (S-12)
--   HR041 a resolution may never commit in state 'executing'; or its link is wrong
--   HR042 a governed posting exists: only the executed exit is allowed
--   HR050 approved-hold freeze (approved -> rejected|cancelled|failed on a non-active tenant/brand)
--   HR099 down-migration refused while rows exist
--   CG030 ledger fence (ADR 0099 6.6)
--
-- No SECURITY DEFINER (the verifier below is the existing 0120 definer, replaced in
-- place). FORCE ROW LEVEL SECURITY on both new tables. No FOR ALL policy. DELETE
-- and TRUNCATE refused on both. No threshold value anywhere (HD-PRH2-3).

-- =========================================================================
-- 1. Classification and capability catalogue (reference rows, migration-written)
-- =========================================================================

ALTER TABLE financial_control_classifications DROP CONSTRAINT financial_control_classifications_check;
ALTER TABLE financial_control_classifications ADD CONSTRAINT financial_control_classifications_check
    CHECK (operation_kind NOT IN ('ledger_adjustment', 'payment_force_resolve', 'withdrawal_hold_resolution') OR class = 'mandatory_four_eyes');
ALTER TABLE financial_capability_catalogue DROP CONSTRAINT financial_capability_catalogue_operation_kind_check;
ALTER TABLE financial_capability_catalogue ADD CONSTRAINT financial_capability_catalogue_operation_kind_check
    CHECK (operation_kind IN ('ledger_adjustment', 'payment_force_resolve', 'withdrawal_hold_resolution'));

-- The owner is bound by FORCE ROW LEVEL SECURITY and these tables' only policy
-- is SELECT-only, so the seed rows are written with FORCE lifted and restored
-- inside this transaction (the 0112/0113 "seed before FORCE" rule).
ALTER TABLE financial_control_classifications NO FORCE ROW LEVEL SECURITY;
ALTER TABLE financial_capability_catalogue NO FORCE ROW LEVEL SECURITY;
INSERT INTO financial_control_classifications (operation_kind, class, governed_since)
VALUES ('withdrawal_hold_resolution', 'mandatory_four_eyes', now());
INSERT INTO financial_capability_catalogue (capability, operation_kind, action, eligible_tenant_roles, platform_grantee_allowed) VALUES
    ('withdrawal_hold_resolution:request', 'withdrawal_hold_resolution', 'request', '{}', true),
    ('withdrawal_hold_resolution:approve', 'withdrawal_hold_resolution', 'approve', '{}', true);
ALTER TABLE financial_control_classifications FORCE ROW LEVEL SECURITY;
ALTER TABLE financial_capability_catalogue FORCE ROW LEVEL SECURITY;

-- =========================================================================
-- 2. financial_policy_required_approvals(): the 0113 body + one extension.
--    A non-active tenant ignores tenant/brand rows for payment_force_resolve
--    (K2-1) and, here, for withdrawal_hold_resolution when the tenant OR the
--    brand is non-active. Nothing else changes.
-- =========================================================================

CREATE OR REPLACE FUNCTION financial_policy_required_approvals(
    p_operation text, p_tenant uuid, p_brand uuid, p_asset text, p_amount numeric, p_as_of timestamptz,
    OUT enabled boolean, OUT required int, OUT contributing_policy_ids uuid[], OUT tenant_status text
) AS $$
DECLARE
    v_licence       uuid;
    v_jurisdiction  uuid;
    v_profile       text;
    v_max           int;
    v_has_platform  boolean;
    v_tenant_rows   boolean;
    v_brand_status  text;
BEGIN
    SELECT t.status, t.licence_id INTO tenant_status, v_licence FROM tenants t WHERE t.id = p_tenant;
    IF NOT FOUND THEN
        enabled := false; required := 1; contributing_policy_ids := '{}'; tenant_status := NULL;
        RETURN;
    END IF;
    IF v_licence IS NOT NULL THEN
        SELECT l.jurisdiction_id INTO v_jurisdiction FROM licences l WHERE l.id = v_licence;
    END IF;
    SELECT pp.profile_code INTO v_profile FROM tenant_financial_policy_profiles pp
     WHERE pp.tenant_id = p_tenant AND pp.effective_from <= p_as_of
     ORDER BY pp.effective_from DESC, pp.created_at DESC LIMIT 1;

    v_tenant_rows := NOT (p_operation = 'payment_force_resolve' AND tenant_status <> 'active');
    IF p_operation = 'withdrawal_hold_resolution' THEN
        SELECT b.status INTO v_brand_status FROM brands b WHERE b.id = p_brand AND b.tenant_id = p_tenant;
        v_tenant_rows := tenant_status = 'active' AND v_brand_status = 'active';
    END IF;

    WITH candidates AS (
        SELECT p.* FROM financial_approval_policies p
         WHERE p.operation_kind = p_operation
           AND p.effective_from <= p_as_of
           AND (p.asset_code IS NULL OR p.asset_code = p_asset)
           AND (p.level = 'platform'
                OR (p.level = 'jurisdiction' AND p.jurisdiction_id = v_jurisdiction)
                OR (p.level = 'profile' AND p.profile_code = v_profile)
                OR (v_tenant_rows AND p.level = 'tenant' AND p.tenant_id = p_tenant)
                OR (v_tenant_rows AND p.level = 'brand' AND p.tenant_id = p_tenant AND p.brand_id = p_brand))
    ), in_force AS (
        SELECT DISTINCT ON (c.level, c.tenant_id, c.brand_id, c.jurisdiction_id, c.profile_code, c.asset_code) c.*
          FROM candidates c
         ORDER BY c.level, c.tenant_id, c.brand_id, c.jurisdiction_id, c.profile_code, c.asset_code,
                  c.effective_from DESC, c.created_at DESC, c.id
    )
    SELECT max(CASE WHEN f.threshold_minor_units IS NULL OR p_amount IS NULL OR p_amount <= f.threshold_minor_units
                    THEN f.base_required_approvals ELSE f.required_approvals_above_threshold END),
           COALESCE(array_agg(f.id ORDER BY f.id), '{}'),
           COALESCE(bool_or(f.level = 'platform'), false)
      INTO v_max, contributing_policy_ids, v_has_platform
      FROM in_force f;

    required := GREATEST(1, COALESCE(v_max, 0));
    enabled := v_has_platform;
    IF v_jurisdiction IS NULL AND EXISTS (
        SELECT 1 FROM financial_approval_policies p
         WHERE p.operation_kind = p_operation AND p.level = 'jurisdiction' AND p.effective_from <= p_as_of
           AND (p.asset_code IS NULL OR p.asset_code = p_asset)) THEN
        enabled := false;
    END IF;
END;
$$ LANGUAGE plpgsql STABLE;

-- =========================================================================
-- 3. The H-SEC lock discipline helper (ADR 0111 6.4): tenant status advisory
--    lock SHARED, tenant status read in a fresh statement, brand row FOR SHARE.
--    VOLATILE (it takes locks). Used by the guards; Go uses the same function.
-- =========================================================================

CREATE FUNCTION withdrawal_hold_resolution_lock_scope(p_tenant uuid, p_brand uuid,
    OUT tenant_status text, OUT brand_status text
) AS $$
BEGIN
    PERFORM pg_advisory_xact_lock_shared(public.tenant_status_gate_key(p_tenant));
    SELECT t.status INTO tenant_status FROM tenants t WHERE t.id = p_tenant;
    SELECT b.status INTO brand_status FROM brands b WHERE b.id = p_brand AND b.tenant_id = p_tenant FOR SHARE;
END;
$$ LANGUAGE plpgsql VOLATILE
    SET search_path = pg_catalog, public, pg_temp;

-- The lock-only acting policy the FOR SHARE above needs inside an acting session
-- (the existing brand policies are tenant-GUC only). USING only; WITH CHECK false.
CREATE POLICY acting_lock ON brands FOR UPDATE
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()))
    WITH CHECK (false);

-- =========================================================================
-- 4. withdrawal_hold_resolutions (family A only)
-- =========================================================================

CREATE TABLE withdrawal_hold_resolutions (
    -- Server-forced: the column default is the nil UUID; any other client value is
    -- refused by the guard, which replaces the nil with gen_random_uuid().
    id                              UUID PRIMARY KEY DEFAULT '00000000-0000-0000-0000-000000000000',
    tenant_id                       UUID NOT NULL REFERENCES tenants (id),
    withdrawal_request_id           UUID NOT NULL,
    kind                            TEXT NOT NULL CHECK (kind IN ('release_hold_to_player')),
    -- DB-forced copies of the withdrawal (the approvers review exactly these).
    brand_id                        UUID NOT NULL,
    player_account_id               UUID NOT NULL,
    wallet_id                       UUID NOT NULL,
    amount                          NUMERIC(38,0) NOT NULL CHECK (amount > 0),
    asset_code                      TEXT NOT NULL,
    -- The B13 instrument id, copied through to_jsonb() so this migration does not
    -- depend on 0123 (NULL until the column exists and is set). A record only: no FK.
    payout_instrument_id            UUID NULL,
    -- Pinned at submission / recorded at execution.
    withdrawal_state_at_submission  TEXT NOT NULL,
    tenant_status_at_submission     TEXT NOT NULL,
    brand_status_at_submission      TEXT NOT NULL,
    tenant_status_at_execution      TEXT NULL,
    brand_status_at_execution       TEXT NULL,
    reason_code                     TEXT NOT NULL CHECK (reason_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    evidence_ref_hash               TEXT NOT NULL CHECK (evidence_ref_hash ~ '^[0-9a-f]{64}$'),
    payload_hash                    TEXT NOT NULL,
    -- Forced actor: a platform principal acting in the tenant, nothing else.
    requested_by                    UUID NOT NULL,
    requested_by_scope              TEXT NOT NULL CHECK (requested_by_scope = 'platform_acting'),
    requested_by_person_id          UUID NOT NULL,
    required_at_submission          INT NOT NULL CHECK (required_at_submission >= 1),
    contributing_policy_ids         UUID[] NOT NULL,
    required_at_execution           INT NULL,
    contributing_policy_ids_at_execution UUID[] NULL,
    state                           TEXT NOT NULL DEFAULT 'pending' CHECK (state IN (
                                        'pending', 'executing', 'executed', 'rejected', 'cancelled', 'expired', 'refused_at_execution')),
    expires_at                      TIMESTAMPTZ NOT NULL,
    executed_txid                   BIGINT NULL,
    ledger_transaction_id           UUID NULL UNIQUE,
    refusal_code                    TEXT NULL CHECK (refusal_code IS NULL OR octet_length(refusal_code) BETWEEN 1 AND 64),
    created_at                      TIMESTAMPTZ NOT NULL DEFAULT now(),
    closed_at                       TIMESTAMPTZ NULL,
    UNIQUE (tenant_id, id),
    FOREIGN KEY (withdrawal_request_id, tenant_id) REFERENCES withdrawal_requests (id, tenant_id),
    FOREIGN KEY (ledger_transaction_id, tenant_id) REFERENCES ledger_transactions (id, tenant_id),
    CHECK (ledger_transaction_id IS NULL OR state = 'executed'),
    CHECK ((state = 'executed') = (ledger_transaction_id IS NOT NULL)),
    CHECK (state NOT IN ('executing', 'executed') OR executed_txid IS NOT NULL),
    CHECK (state NOT IN ('executing', 'executed') OR (tenant_status_at_execution IS NOT NULL AND brand_status_at_execution IS NOT NULL
                                                      AND (tenant_status_at_execution <> 'active' OR brand_status_at_execution <> 'active'))),
    CHECK ((state = 'refused_at_execution') = (refusal_code IS NOT NULL))
);

-- Exactly-once (ADR 0111 6.5): one pending and one executed resolution per withdrawal.
CREATE UNIQUE INDEX withdrawal_hold_resolutions_one_executed ON withdrawal_hold_resolutions (withdrawal_request_id) WHERE state = 'executed';
CREATE UNIQUE INDEX withdrawal_hold_resolutions_one_pending ON withdrawal_hold_resolutions (withdrawal_request_id) WHERE state = 'pending';
CREATE INDEX withdrawal_hold_resolutions_tenant_state ON withdrawal_hold_resolutions (tenant_id, state);

CREATE TABLE withdrawal_hold_resolution_approvals (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NOT NULL,
    resolution_id         UUID NOT NULL,
    decision              TEXT NOT NULL CHECK (decision IN ('approve', 'reject')),
    payload_hash          TEXT NOT NULL,
    decided_by            UUID NOT NULL,
    decided_by_scope      TEXT NOT NULL CHECK (decided_by_scope = 'platform_acting'),
    decided_by_person_id  UUID NOT NULL,
    decided_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_txid          BIGINT NOT NULL,
    reason_code           TEXT NOT NULL CHECK (reason_code ~ '^[a-z][a-z0-9_]{0,63}$'),
    UNIQUE (resolution_id, decided_by),
    FOREIGN KEY (tenant_id, resolution_id) REFERENCES withdrawal_hold_resolutions (tenant_id, id)
);

CREATE INDEX withdrawal_hold_resolution_approvals_tenant ON withdrawal_hold_resolution_approvals (tenant_id);

-- =========================================================================
-- 5. Counting at execution - the ONE implementation, used by the executor after
--    its FOR SHARE locks AND by the -> executing guard (K3-S2 pattern). Every
--    actor must be platform_acting (a tenant-scope row never counts).
-- =========================================================================

CREATE FUNCTION withdrawal_hold_resolution_execution_status(p_resolution uuid,
    OUT required int, OUT counted int, OUT counted_approval_ids uuid[], OUT requester_valid boolean,
    OUT contributing_policy_ids uuid[], OUT tenant_status text, OUT brand_status text, OUT enabled boolean
) AS $$
DECLARE
    r              withdrawal_hold_resolutions%ROWTYPE;
    v_policy       RECORD;
    v_authors      uuid[];
    v_owner_person uuid;
    v_req_person   uuid;
BEGIN
    SELECT * INTO r FROM withdrawal_hold_resolutions WHERE id = p_resolution;
    IF NOT FOUND THEN
        required := 1; counted := 0; counted_approval_ids := '{}'; requester_valid := false;
        contributing_policy_ids := '{}'; enabled := false;
        RETURN;
    END IF;
    SELECT * INTO v_policy FROM financial_policy_required_approvals('withdrawal_hold_resolution', r.tenant_id, r.brand_id, r.asset_code,
        r.amount, now());
    required := GREATEST(r.required_at_submission, v_policy.required);
    contributing_policy_ids := v_policy.contributing_policy_ids;
    tenant_status := v_policy.tenant_status;
    enabled := v_policy.enabled;
    SELECT b.status INTO brand_status FROM brands b WHERE b.id = r.brand_id AND b.tenant_id = r.tenant_id;
    v_authors := financial_policy_author_persons(r.contributing_policy_ids || v_policy.contributing_policy_ids);

    SELECT pa.person_id INTO v_owner_person
      FROM withdrawal_requests w JOIN player_accounts pa ON pa.id = w.player_account_id AND pa.tenant_id = w.tenant_id
     WHERE w.id = r.withdrawal_request_id AND w.tenant_id = r.tenant_id;
    v_req_person := ledger_adjustment_live_person(r.requested_by, r.requested_by_person_id);

    requester_valid := v_req_person IS NOT NULL
        AND v_req_person = r.requested_by_person_id
        AND v_owner_person IS NOT NULL
        AND v_req_person <> v_owner_person
        AND NOT (v_req_person = ANY (v_authors))
        AND r.requested_by_scope = 'platform_acting'
        AND COALESCE(ledger_adjustment_eligible_grant(r.tenant_id, r.requested_by, 'withdrawal_hold_resolution:request'),
                     ledger_adjustment_invisible_platform_grant(r.tenant_id, r.requested_by, 'withdrawal_hold_resolution:request', r.requested_by_person_id)) IS NOT NULL;

    WITH cand AS (
        SELECT a.id, a.decided_at,
               ledger_adjustment_live_person(a.decided_by, a.decided_by_person_id) AS person
          FROM withdrawal_hold_resolution_approvals a
         WHERE a.resolution_id = r.id
           AND a.decision = 'approve'
           AND a.payload_hash = r.payload_hash
           AND a.decided_by <> r.requested_by
           AND a.decided_by_scope = 'platform_acting'
           AND COALESCE(ledger_adjustment_eligible_grant(r.tenant_id, a.decided_by, 'withdrawal_hold_resolution:approve'),
                        ledger_adjustment_invisible_platform_grant(r.tenant_id, a.decided_by, 'withdrawal_hold_resolution:approve', a.decided_by_person_id)) IS NOT NULL
    ), qualified AS (
        SELECT DISTINCT ON (c.person) c.id
          FROM cand c
         WHERE c.person IS NOT NULL
           AND c.person IS DISTINCT FROM v_req_person
           AND c.person IS DISTINCT FROM v_owner_person
           AND NOT (c.person = ANY (v_authors))
         ORDER BY c.person, c.decided_at, c.id
    )
    SELECT count(*)::int, COALESCE(array_agg(q.id ORDER BY q.id), '{}') INTO counted, counted_approval_ids FROM qualified q;
    -- Never NULL: a NULL would make the recount's NOT requester_valid test silently pass.
    requester_valid := COALESCE(requester_valid, false);
END;
$$ LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp;

-- =========================================================================
-- 6. The resolution guard (INSERT shape + UPDATE state machine)
-- =========================================================================

-- The payload digest: the ONE definition, used at insert and at every update.
CREATE FUNCTION withdrawal_hold_resolution_payload_hash(r withdrawal_hold_resolutions) RETURNS text AS $$
    SELECT k2_sha256_hex(k2_canonical(
        r.tenant_id::text, r.withdrawal_request_id::text, r.kind, r.amount::text, r.asset_code, r.reason_code,
        r.evidence_ref_hash, r.withdrawal_state_at_submission, r.tenant_status_at_submission, r.brand_status_at_submission));
$$ LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp;

CREATE FUNCTION withdrawal_hold_resolutions_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor   RECORD;
    v_w       RECORD;
    v_scope   RECORD;
    v_policy  RECORD;
    v_exec    RECORD;
BEGIN
    SELECT * INTO v_actor FROM financial_actor_session();

    IF TG_OP = 'INSERT' THEN
        -- Only a platform principal ACTING in the tenant: never a tenant role.
        IF v_actor.scope <> 'platform_acting' THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: only a platform_acting session may request (ADR 0111 A-17)' USING ERRCODE = 'HR001';
        END IF;
        IF v_actor.person_id IS NULL THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: requester has no linked person_id' USING ERRCODE = 'HR002';
        END IF;
        IF NEW.tenant_id IS DISTINCT FROM v_actor.tenant THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: resolution tenant must be the session tenant' USING ERRCODE = 'HR001';
        END IF;
        IF NEW.id IS DISTINCT FROM '00000000-0000-0000-0000-000000000000'::uuid THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: id is server-forced and may not be supplied' USING ERRCODE = 'HR030';
        END IF;
        NEW.id := gen_random_uuid();
        IF ledger_adjustment_eligible_grant(NEW.tenant_id, v_actor.actor, 'withdrawal_hold_resolution:request') IS NULL THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: requester lacks an in-force withdrawal_hold_resolution:request grant' USING ERRCODE = 'HR003';
        END IF;

        SELECT w.brand_id, w.player_account_id, w.wallet_id, w.amount, w.asset_code, w.state,
               NULLIF(to_jsonb(w) ->> 'payout_instrument_id', '')::uuid AS payout_instrument_id INTO v_w
          FROM withdrawal_requests w WHERE w.id = NEW.withdrawal_request_id AND w.tenant_id = NEW.tenant_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: withdrawal % not found in the session tenant', NEW.withdrawal_request_id USING ERRCODE = 'HR010';
        END IF;
        IF v_w.state <> 'approved' THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: the withdrawal is not approved (state %)', v_w.state USING ERRCODE = 'HR010';
        END IF;
        IF EXISTS (SELECT 1 FROM payment_attempts a WHERE a.withdrawal_request_id = NEW.withdrawal_request_id AND a.tenant_id = NEW.tenant_id) THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: a payment attempt exists for the withdrawal; a hold with an attempt is never released here' USING ERRCODE = 'HR010';
        END IF;

        -- H-SEC lock discipline, then the non-active precondition (tenant OR brand).
        SELECT * INTO v_scope FROM withdrawal_hold_resolution_lock_scope(NEW.tenant_id, v_w.brand_id);
        IF v_scope.tenant_status IS NULL OR v_scope.brand_status IS NULL THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: tenant or brand status is not readable (fail closed)' USING ERRCODE = 'HR010';
        END IF;
        IF v_scope.tenant_status = 'active' AND v_scope.brand_status = 'active' THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: the tenant and the brand are both active; resume through the normal submit path (A-19)' USING ERRCODE = 'HR010';
        END IF;

        NEW.brand_id := v_w.brand_id;
        NEW.player_account_id := v_w.player_account_id;
        NEW.wallet_id := v_w.wallet_id;
        NEW.amount := v_w.amount;
        NEW.asset_code := v_w.asset_code;
        NEW.payout_instrument_id := v_w.payout_instrument_id;
        NEW.withdrawal_state_at_submission := v_w.state;
        NEW.tenant_status_at_submission := v_scope.tenant_status;
        NEW.brand_status_at_submission := v_scope.brand_status;
        NEW.tenant_status_at_execution := NULL;
        NEW.brand_status_at_execution := NULL;
        NEW.requested_by := v_actor.actor;
        NEW.requested_by_scope := v_actor.scope;
        NEW.requested_by_person_id := v_actor.person_id;
        NEW.state := 'pending';
        NEW.created_at := now();
        NEW.closed_at := NULL;
        NEW.expires_at := now() + interval '24 hours';
        NEW.executed_txid := NULL;
        NEW.ledger_transaction_id := NULL;
        NEW.refusal_code := NULL;
        NEW.required_at_execution := NULL;
        NEW.contributing_policy_ids_at_execution := NULL;

        SELECT * INTO v_policy FROM financial_policy_required_approvals('withdrawal_hold_resolution', NEW.tenant_id, NEW.brand_id, NEW.asset_code,
            NEW.amount, now());
        IF NOT v_policy.enabled THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: withdrawal_hold_resolution is disabled (no in-force platform baseline, HD-PRH2-3)' USING ERRCODE = 'HR014';
        END IF;
        NEW.required_at_submission := v_policy.required;
        NEW.contributing_policy_ids := v_policy.contributing_policy_ids;
        IF v_actor.person_id = ANY (financial_policy_author_persons(NEW.contributing_policy_ids)) THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: the requester authored or approved a contributing policy (S-2(iii))' USING ERRCODE = 'HR011';
        END IF;
        NEW.payload_hash := withdrawal_hold_resolution_payload_hash(NEW);
        RETURN NEW;
    END IF;

    -- UPDATE ----------------------------------------------------------------
    IF OLD.state NOT IN ('pending', 'executing') THEN
        RAISE EXCEPTION 'withdrawal_hold_resolutions: resolution % is terminal (%)', OLD.id, OLD.state USING ERRCODE = 'HR030';
    END IF;
    IF (to_jsonb(NEW) - ARRAY['state', 'executed_txid', 'ledger_transaction_id', 'refusal_code', 'closed_at',
                              'tenant_status_at_execution', 'brand_status_at_execution', 'required_at_execution',
                              'contributing_policy_ids_at_execution'])
       IS DISTINCT FROM
       (to_jsonb(OLD) - ARRAY['state', 'executed_txid', 'ledger_transaction_id', 'refusal_code', 'closed_at',
                              'tenant_status_at_execution', 'brand_status_at_execution', 'required_at_execution',
                              'contributing_policy_ids_at_execution']) THEN
        RAISE EXCEPTION 'withdrawal_hold_resolutions: the payload, actor and pinned policy are immutable' USING ERRCODE = 'HR030';
    END IF;
    IF NEW.payload_hash IS DISTINCT FROM withdrawal_hold_resolution_payload_hash(NEW) THEN
        RAISE EXCEPTION 'withdrawal_hold_resolutions: payload_hash does not match the payload' USING ERRCODE = 'HR030';
    END IF;
    IF NEW.state IN ('executed', 'refused_at_execution', 'rejected', 'cancelled', 'expired') THEN
        NEW.closed_at := now();
    END IF;

    IF OLD.state = 'pending' AND NEW.state = 'cancelled' THEN
        IF v_actor.actor IS DISTINCT FROM OLD.requested_by THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: only the requester may cancel' USING ERRCODE = 'HR030';
        END IF;
        NEW.executed_txid := NULL; NEW.ledger_transaction_id := NULL; NEW.refusal_code := NULL;
        NEW.required_at_execution := NULL; NEW.contributing_policy_ids_at_execution := NULL;
        NEW.tenant_status_at_execution := NULL; NEW.brand_status_at_execution := NULL;
        RETURN NEW;
    ELSIF OLD.state = 'pending' AND NEW.state = 'expired' THEN
        IF now() < OLD.expires_at THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: resolution has not expired' USING ERRCODE = 'HR030';
        END IF;
        NEW.executed_txid := NULL; NEW.ledger_transaction_id := NULL; NEW.refusal_code := NULL;
        NEW.required_at_execution := NULL; NEW.contributing_policy_ids_at_execution := NULL;
        NEW.tenant_status_at_execution := NULL; NEW.brand_status_at_execution := NULL;
        RETURN NEW;
    ELSIF OLD.state = 'pending' AND NEW.state = 'rejected' THEN
        IF NOT EXISTS (SELECT 1 FROM withdrawal_hold_resolution_approvals a
                        WHERE a.resolution_id = OLD.id AND a.decision = 'reject' AND a.decided_txid = txid_current()) THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: rejected only via a same-transaction reject decision' USING ERRCODE = 'HR030';
        END IF;
        NEW.executed_txid := NULL; NEW.ledger_transaction_id := NULL; NEW.refusal_code := NULL;
        NEW.required_at_execution := NULL; NEW.contributing_policy_ids_at_execution := NULL;
        NEW.tenant_status_at_execution := NULL; NEW.brand_status_at_execution := NULL;
        RETURN NEW;
    ELSIF OLD.state = 'pending' AND NEW.state IN ('refused_at_execution', 'executing') THEN
        IF NOT EXISTS (SELECT 1 FROM withdrawal_hold_resolution_approvals a
                        WHERE a.resolution_id = OLD.id AND a.decision = 'approve' AND a.decided_txid = txid_current()) THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: execution happens only in the final approval''s transaction' USING ERRCODE = 'HR030';
        END IF;
        IF now() >= OLD.expires_at THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: resolution has expired' USING ERRCODE = 'HR030';
        END IF;
        IF NEW.state = 'refused_at_execution' THEN
            IF NEW.refusal_code IS NULL THEN
                RAISE EXCEPTION 'withdrawal_hold_resolutions: refused_at_execution needs a refusal_code' USING ERRCODE = 'HR030';
            END IF;
            NEW.executed_txid := NULL; NEW.ledger_transaction_id := NULL;
            RETURN NEW;
        END IF;
        -- -> executing: lock discipline, the SAME recount the executor used, and the
        -- preconditions re-run (withdrawal approved, no attempt, tenant OR brand non-active).
        SELECT * INTO v_scope FROM withdrawal_hold_resolution_lock_scope(OLD.tenant_id, OLD.brand_id);
        SELECT * INTO v_exec FROM withdrawal_hold_resolution_execution_status(OLD.id);
        IF NOT v_exec.enabled OR NOT v_exec.requester_valid OR v_exec.counted < v_exec.required THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: fewer counted approvals (%) than required (%), or the requester no longer qualifies',
                v_exec.counted, v_exec.required USING ERRCODE = 'HR030';
        END IF;
        SELECT w.state INTO v_w FROM withdrawal_requests w WHERE w.id = OLD.withdrawal_request_id AND w.tenant_id = OLD.tenant_id;
        IF NOT FOUND OR v_w.state <> 'approved' THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: the withdrawal is not approved' USING ERRCODE = 'HR010';
        END IF;
        IF EXISTS (SELECT 1 FROM payment_attempts a WHERE a.withdrawal_request_id = OLD.withdrawal_request_id AND a.tenant_id = OLD.tenant_id) THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: a payment attempt exists for the withdrawal' USING ERRCODE = 'HR010';
        END IF;
        IF v_scope.tenant_status IS NULL OR v_scope.brand_status IS NULL
           OR (v_scope.tenant_status = 'active' AND v_scope.brand_status = 'active') THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: the tenant and the brand are both active again (or unreadable); a release is refused (A-19)' USING ERRCODE = 'HR010';
        END IF;
        NEW.required_at_execution := v_exec.required;
        NEW.contributing_policy_ids_at_execution := v_exec.contributing_policy_ids;
        NEW.tenant_status_at_execution := v_scope.tenant_status;
        NEW.brand_status_at_execution := v_scope.brand_status;
        NEW.executed_txid := txid_current();
        NEW.ledger_transaction_id := NULL;
        NEW.refusal_code := NULL;
        RETURN NEW;
    ELSIF OLD.state = 'executing' AND NEW.state = 'executed' THEN
        IF OLD.executed_txid IS DISTINCT FROM txid_current() OR NEW.executed_txid IS DISTINCT FROM OLD.executed_txid THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: executing -> executed only in the executing transaction' USING ERRCODE = 'HR030';
        END IF;
        IF NEW.ledger_transaction_id IS NULL THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: an executed release needs its ledger_transaction_id' USING ERRCODE = 'HR041';
        END IF;
        RETURN NEW;
    ELSIF OLD.state = 'executing' AND NEW.state = 'refused_at_execution' THEN
        IF OLD.executed_txid IS DISTINCT FROM txid_current() THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: executing -> % only in the executing transaction', NEW.state USING ERRCODE = 'HR030';
        END IF;
        IF NEW.refusal_code IS NULL THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: refused_at_execution needs a refusal_code' USING ERRCODE = 'HR030';
        END IF;
        IF EXISTS (SELECT 1 FROM ledger_transactions t
                    WHERE t.tenant_id = OLD.tenant_id
                      AND t.idempotency_key = OLD.withdrawal_request_id::text || ':governed_hold_released'
                      AND t.correlation_id = OLD.withdrawal_request_id) THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: % refused - a governed ledger transaction exists', NEW.state USING ERRCODE = 'HR042';
        END IF;
        NEW.ledger_transaction_id := NULL;
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'withdrawal_hold_resolutions: invalid transition % -> %', OLD.state, NEW.state USING ERRCODE = 'HR030';
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

-- The beneficiary guard (S-12) on BOTH tables, self-contained like 0115's: a NULL
-- or equal Person is refused; a missing lookup fails closed.
CREATE FUNCTION withdrawal_hold_resolutions_beneficiary_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor  RECORD;
    v_owner  uuid;
    v_wr     uuid;
BEGIN
    SELECT * INTO v_actor FROM financial_actor_session();
    IF v_actor.scope <> 'platform_acting' THEN
        RAISE EXCEPTION '%: only a platform_acting session may act (ADR 0111 A-17)', TG_TABLE_NAME USING ERRCODE = 'HR001';
    END IF;
    IF TG_TABLE_NAME = 'withdrawal_hold_resolutions' THEN
        v_wr := NEW.withdrawal_request_id;
    ELSE
        SELECT r.withdrawal_request_id INTO v_wr FROM withdrawal_hold_resolutions r WHERE r.id = NEW.resolution_id;
    END IF;
    SELECT pa.person_id INTO v_owner
      FROM withdrawal_requests w JOIN player_accounts pa ON pa.id = w.player_account_id AND pa.tenant_id = w.tenant_id
     WHERE w.id = v_wr;
    IF v_owner IS NULL OR v_actor.person_id IS NULL OR v_actor.person_id = v_owner THEN
        RAISE EXCEPTION '%: the actor may not be the beneficiary (S-12), and an unlinked or unresolvable Person is refused', TG_TABLE_NAME USING ERRCODE = 'HR032';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER withdrawal_hold_resolutions_guard
    BEFORE INSERT OR UPDATE ON withdrawal_hold_resolutions
    FOR EACH ROW EXECUTE FUNCTION withdrawal_hold_resolutions_guard();
CREATE TRIGGER withdrawal_hold_resolutions_beneficiary_guard
    BEFORE INSERT ON withdrawal_hold_resolutions
    FOR EACH ROW EXECUTE FUNCTION withdrawal_hold_resolutions_beneficiary_guard();

-- Deferred, in ALL sessions: no resolution commits in state 'executing', and an
-- executed release commits only with its exact link - the withdrawal rejected with
-- the link, the same-tenant withdrawal_rejected of the exact key and correlation,
-- reversing the hold transaction, with EXACTLY TWO entries: debit
-- player_withdrawal_hold and credit player_cash, both on the withdrawal's wallet,
-- amount and asset equal (ADR 0111 6.4, ledger-finance L-2).
CREATE FUNCTION withdrawal_hold_resolutions_no_executing_commit() RETURNS TRIGGER AS $$
DECLARE
    r       withdrawal_hold_resolutions%ROWTYPE;
    v_w     RECORD;
    v_tx    RECORD;
    v_n     int;
    v_ok    int;
BEGIN
    SELECT * INTO r FROM withdrawal_hold_resolutions WHERE id = NEW.id;
    IF NOT FOUND THEN
        -- Fail closed (security C-1): a row that was just written but is not visible to the
        -- committing session (RLS / session settings changed before commit) is never skipped.
        RAISE EXCEPTION 'withdrawal_hold_resolutions: resolution % is not visible at commit; the deferred shape check cannot run', NEW.id USING ERRCODE = 'HR041';
    END IF;
    IF r.state = 'executing' THEN
        RAISE EXCEPTION 'withdrawal_hold_resolutions: resolution % may not commit in state executing', NEW.id USING ERRCODE = 'HR041';
    END IF;
    IF r.state = 'executed' THEN
        SELECT w.state, w.wallet_id, w.amount, w.asset_code, w.hold_ledger_transaction_id, w.release_ledger_transaction_id INTO v_w
          FROM withdrawal_requests w WHERE w.id = r.withdrawal_request_id AND w.tenant_id = r.tenant_id;
        IF NOT FOUND
           OR v_w.state IS DISTINCT FROM 'rejected'
           OR v_w.release_ledger_transaction_id IS DISTINCT FROM r.ledger_transaction_id
           OR v_w.wallet_id IS DISTINCT FROM r.wallet_id
           OR v_w.amount IS DISTINCT FROM r.amount
           OR v_w.asset_code IS DISTINCT FROM r.asset_code THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: executed release % does not match its withdrawal outcome', NEW.id USING ERRCODE = 'HR041';
        END IF;
        SELECT t.transaction_type, t.idempotency_key, t.correlation_id, t.provider_id, t.provider_tx_id, t.reverses_transaction_id INTO v_tx
          FROM ledger_transactions t WHERE t.id = r.ledger_transaction_id AND t.tenant_id = r.tenant_id;
        IF NOT FOUND
           OR v_tx.transaction_type <> 'withdrawal_rejected'
           OR v_tx.idempotency_key <> r.withdrawal_request_id::text || ':governed_hold_released'
           OR v_tx.correlation_id IS DISTINCT FROM r.withdrawal_request_id
           OR v_tx.provider_id IS NOT NULL OR v_tx.provider_tx_id IS NOT NULL
           OR v_tx.reverses_transaction_id IS DISTINCT FROM v_w.hold_ledger_transaction_id THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: executed release % links a ledger transaction that does not carry its keys', NEW.id USING ERRCODE = 'HR041';
        END IF;
        SELECT count(*) INTO v_n FROM ledger_entries e WHERE e.ledger_transaction_id = r.ledger_transaction_id;
        SELECT count(*) INTO v_ok
          FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
         WHERE e.ledger_transaction_id = r.ledger_transaction_id
           AND e.tenant_id = r.tenant_id AND e.asset_code = r.asset_code AND e.amount = r.amount
           AND ((la.account_type = 'player_withdrawal_hold' AND la.wallet_id = v_w.wallet_id AND e.direction = 'debit')
             OR (la.account_type = 'player_cash' AND la.wallet_id = v_w.wallet_id AND e.direction = 'credit'));
        IF v_n <> 2 OR v_ok <> 2
           OR (SELECT count(DISTINCT e.direction) FROM ledger_entries e WHERE e.ledger_transaction_id = r.ledger_transaction_id) <> 2 THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: executed release % entries are not exactly the approved two-leg shape', NEW.id USING ERRCODE = 'HR041';
        END IF;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE CONSTRAINT TRIGGER withdrawal_hold_resolutions_no_executing_commit
    AFTER INSERT OR UPDATE ON withdrawal_hold_resolutions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION withdrawal_hold_resolutions_no_executing_commit();

-- =========================================================================
-- 7. The approvals guard
-- =========================================================================

CREATE FUNCTION withdrawal_hold_resolution_approvals_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor  RECORD;
    v_res    withdrawal_hold_resolutions%ROWTYPE;
    v_policy RECORD;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'withdrawal_hold_resolution_approvals: immutable' USING ERRCODE = 'HR031';
    END IF;
    SELECT * INTO v_actor FROM financial_actor_session();
    IF v_actor.scope <> 'platform_acting' THEN
        RAISE EXCEPTION 'withdrawal_hold_resolution_approvals: only a platform_acting session may decide (ADR 0111 A-17)' USING ERRCODE = 'HR001';
    END IF;
    IF v_actor.person_id IS NULL THEN
        RAISE EXCEPTION 'withdrawal_hold_resolution_approvals: approver has no linked person_id' USING ERRCODE = 'HR002';
    END IF;

    SELECT * INTO v_res FROM withdrawal_hold_resolutions WHERE id = NEW.resolution_id;
    IF NOT FOUND OR v_res.tenant_id IS DISTINCT FROM v_actor.tenant THEN
        RAISE EXCEPTION 'withdrawal_hold_resolution_approvals: resolution % not found in the session tenant', NEW.resolution_id USING ERRCODE = 'HR031';
    END IF;
    IF v_res.state <> 'pending' OR now() >= v_res.expires_at THEN
        RAISE EXCEPTION 'withdrawal_hold_resolution_approvals: resolution % is not pending or has expired', NEW.resolution_id USING ERRCODE = 'HR031';
    END IF;
    IF NEW.payload_hash IS DISTINCT FROM v_res.payload_hash THEN
        RAISE EXCEPTION 'withdrawal_hold_resolution_approvals: payload_hash does not match the resolution' USING ERRCODE = 'HR031';
    END IF;
    IF ledger_adjustment_eligible_grant(v_res.tenant_id, v_actor.actor, 'withdrawal_hold_resolution:approve') IS NULL THEN
        RAISE EXCEPTION 'withdrawal_hold_resolution_approvals: approver lacks an in-force withdrawal_hold_resolution:approve grant' USING ERRCODE = 'HR003';
    END IF;
    -- LF-11 distinct-Person floor (non-configurable): four-eyes regardless of amount.
    IF v_actor.actor = v_res.requested_by
       OR v_actor.person_id = ledger_adjustment_live_person(v_res.requested_by, v_res.requested_by_person_id)
       OR v_actor.person_id = v_res.requested_by_person_id THEN
        RAISE EXCEPTION 'withdrawal_hold_resolution_approvals: the approver must be a distinct principal and Person from the requester' USING ERRCODE = 'HR031';
    END IF;
    IF EXISTS (SELECT 1 FROM withdrawal_hold_resolution_approvals a
                WHERE a.resolution_id = v_res.id AND a.decided_by_person_id = v_actor.person_id) THEN
        RAISE EXCEPTION 'withdrawal_hold_resolution_approvals: this Person already decided this resolution' USING ERRCODE = 'HR031';
    END IF;
    SELECT * INTO v_policy FROM financial_policy_required_approvals('withdrawal_hold_resolution', v_res.tenant_id, v_res.brand_id, v_res.asset_code,
        v_res.amount, now());
    IF v_actor.person_id = ANY (financial_policy_author_persons(v_res.contributing_policy_ids || v_policy.contributing_policy_ids)) THEN
        RAISE EXCEPTION 'withdrawal_hold_resolution_approvals: the approver authored or approved a contributing policy (S-2(iii))' USING ERRCODE = 'HR011';
    END IF;

    NEW.tenant_id := v_res.tenant_id;
    NEW.decided_by := v_actor.actor;
    NEW.decided_by_scope := v_actor.scope;
    NEW.decided_by_person_id := v_actor.person_id;
    NEW.decided_at := now();
    NEW.decided_txid := txid_current();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE FUNCTION withdrawal_hold_resolution_approvals_apply_reject() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.decision = 'reject' THEN
        UPDATE withdrawal_hold_resolutions SET state = 'rejected' WHERE id = NEW.resolution_id;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER withdrawal_hold_resolution_approvals_guard
    BEFORE INSERT OR UPDATE ON withdrawal_hold_resolution_approvals
    FOR EACH ROW EXECUTE FUNCTION withdrawal_hold_resolution_approvals_guard();
CREATE TRIGGER withdrawal_hold_resolution_approvals_beneficiary_guard
    BEFORE INSERT ON withdrawal_hold_resolution_approvals
    FOR EACH ROW EXECUTE FUNCTION withdrawal_hold_resolutions_beneficiary_guard();
CREATE TRIGGER withdrawal_hold_resolution_approvals_apply_reject
    AFTER INSERT ON withdrawal_hold_resolution_approvals
    FOR EACH ROW EXECUTE FUNCTION withdrawal_hold_resolution_approvals_apply_reject();

-- =========================================================================
-- 8. CT-R3 and the approved-hold freeze (ALL sessions)
-- =========================================================================

-- (a) The governed ledger key (right-anchored, so no prefix games): any ledger
-- transaction whose idempotency key ends ':governed_hold_released' is refused
-- unless it is exactly the withdrawal_rejected posting of an executing resolution
-- of THIS transaction: key = withdrawal id || the suffix, correlation = withdrawal
-- id, same tenant. The resolution table is visible only to an acting session, so
-- every other session fails closed.
CREATE FUNCTION ledger_transactions_hold_release_key_guard() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.transaction_type <> 'withdrawal_rejected'
       OR NOT EXISTS (SELECT 1 FROM withdrawal_hold_resolutions h
                       WHERE h.tenant_id = NEW.tenant_id AND h.kind = 'release_hold_to_player'
                         AND h.state = 'executing' AND h.executed_txid = txid_current()
                         AND NEW.correlation_id = h.withdrawal_request_id
                         AND NEW.idempotency_key = h.withdrawal_request_id::text || ':governed_hold_released') THEN
        RAISE EXCEPTION 'ledger_transactions: the governed hold-release key may be used only by an executing hold resolution (CT-R3)' USING ERRCODE = 'HR020';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER ledger_transactions_hold_release_key_guard
    BEFORE INSERT ON ledger_transactions
    FOR EACH ROW
    WHEN (right(NEW.idempotency_key, 23) = ':governed_hold_released')
    EXECUTE FUNCTION ledger_transactions_hold_release_key_guard();

-- (b) withdrawal_requests: (i) a state/release-link change that points at a
-- governed-key posting needs the executing resolution; (ii) while a hold
-- resolution is executing for this withdrawal in this transaction, the ONLY
-- admitted change is approved -> rejected with that posting as the release link and
-- no other column touched. It fires on EVERY update (no WHEN clause; ledger-finance C-1):
-- a second UPDATE of any column inside the executing transaction, after the release,
-- finds OLD.state = 'rejected' and is refused.
CREATE FUNCTION withdrawal_requests_governed_release_guard() RETURNS TRIGGER AS $$
DECLARE
    v_exec   boolean;
    v_governed boolean;
BEGIN
    v_exec := EXISTS (SELECT 1 FROM withdrawal_hold_resolutions h
                       WHERE h.withdrawal_request_id = OLD.id AND h.tenant_id = OLD.tenant_id
                         AND h.kind = 'release_hold_to_player'
                         AND h.state = 'executing' AND h.executed_txid = txid_current());
    v_governed := NEW.release_ledger_transaction_id IS NOT NULL AND EXISTS (
        SELECT 1 FROM ledger_transactions t
         WHERE t.id = NEW.release_ledger_transaction_id AND t.tenant_id = NEW.tenant_id
           AND right(t.idempotency_key, 23) = ':governed_hold_released');
    IF v_governed AND NOT v_exec THEN
        RAISE EXCEPTION 'withdrawal_requests: a governed hold-release posting may be linked only by an executing hold resolution (CT-R3)' USING ERRCODE = 'HR020';
    END IF;
    IF v_exec THEN
        IF NOT (OLD.state = 'approved' AND NEW.state = 'rejected' AND v_governed
                AND OLD.release_ledger_transaction_id IS NULL
                AND (to_jsonb(NEW) - ARRAY['state', 'release_ledger_transaction_id', 'updated_at'])
                    IS NOT DISTINCT FROM
                    (to_jsonb(OLD) - ARRAY['state', 'release_ledger_transaction_id', 'updated_at'])) THEN
            RAISE EXCEPTION 'withdrawal_requests: an executing hold resolution admits only approved -> rejected with the governed release link' USING ERRCODE = 'HR030';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER withdrawal_requests_governed_release_guard
    BEFORE UPDATE ON withdrawal_requests
    FOR EACH ROW
    EXECUTE FUNCTION withdrawal_requests_governed_release_guard();

-- (c) The approved-hold freeze (security M-6, widened by security C-3): ANY state change out
-- of `approved` (rejected, cancelled, failed, submitted, ...) is refused while the tenant OR the brand is not active (an unreadable
-- status counts as not active), unless the transition is approved -> rejected of
-- an executing hold resolution of this transaction. The normal KYC denial
-- (DenyForCompliance) runs only after the H-SEC gate, i.e. only while both are
-- active, so it is unaffected.
CREATE FUNCTION withdrawal_requests_approved_hold_freeze() RETURNS TRIGGER AS $$
DECLARE
    v_t text;
    v_b text;
BEGIN
    PERFORM pg_advisory_xact_lock_shared(public.tenant_status_gate_key(OLD.tenant_id));
    SELECT t.status INTO v_t FROM tenants t WHERE t.id = OLD.tenant_id;
    SELECT b.status INTO v_b FROM brands b WHERE b.id = OLD.brand_id AND b.tenant_id = OLD.tenant_id;
    IF v_t IS NOT DISTINCT FROM 'active' AND v_b IS NOT DISTINCT FROM 'active' THEN
        RETURN NEW;
    END IF;
    IF NEW.state = 'rejected' AND EXISTS (SELECT 1 FROM withdrawal_hold_resolutions h
                                           WHERE h.withdrawal_request_id = OLD.id AND h.tenant_id = OLD.tenant_id
                                             AND h.kind = 'release_hold_to_player'
                                             AND h.state = 'executing' AND h.executed_txid = txid_current()) THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'withdrawal_requests: an approved hold on a non-active tenant or brand is frozen (approved -> % refused; ADR 0111 6.4, security M-6)', NEW.state USING ERRCODE = 'HR050';
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER withdrawal_requests_approved_hold_freeze
    BEFORE UPDATE ON withdrawal_requests
    FOR EACH ROW
    WHEN (OLD.state = 'approved' AND NEW.state IS DISTINCT FROM OLD.state)
    EXECUTE FUNCTION withdrawal_requests_approved_hold_freeze();

-- =========================================================================
-- 9. THE FOUR SHARED OBJECTS, replaced in place (0115 text + the marked additions).
--    0125 builds on these bodies; its down restores exactly these.
-- =========================================================================

-- (1/4) ledger_governed_fence_allows: 0115 (a)+(b)+(c) verbatim + branch (e).
CREATE OR REPLACE FUNCTION ledger_governed_fence_allows(
    p_tenant uuid, p_type text, p_idempotency_key text, p_correlation uuid, p_provider_id text, p_provider_tx_id text
) RETURNS boolean AS $$
    SELECT (p_type = 'manual_adjustment' AND EXISTS (
        SELECT 1 FROM ledger_adjustment_requests r
         WHERE r.tenant_id = p_tenant
           AND p_idempotency_key = 'manual_adjustment:' || r.id::text
           AND p_correlation = r.id
           AND r.state = 'executing' AND r.executed_txid = txid_current()))
        -- (b) M2 declare paid
        OR (p_type = 'withdrawal_completed' AND EXISTS (
        SELECT 1 FROM payment_manual_resolutions m
         WHERE m.tenant_id = p_tenant AND m.kind = 'm2_declare_paid'
           AND m.state = 'executing' AND m.executed_txid = txid_current()
           AND p_correlation = m.withdrawal_request_id
           AND p_provider_id = m.provider_id
           AND p_provider_tx_id = m.reserved_provider_tx_id
           AND p_idempotency_key = m.provider_id || ':' || m.reserved_provider_tx_id))
        -- (c) M2 declare not paid
        OR (p_type = 'withdrawal_failed' AND EXISTS (
        SELECT 1 FROM payment_manual_resolutions m
         WHERE m.tenant_id = p_tenant AND m.kind = 'm2_declare_not_paid'
           AND m.state = 'executing' AND m.executed_txid = txid_current()
           AND p_correlation = m.withdrawal_request_id
           AND p_idempotency_key = m.withdrawal_request_id::text || ':failed'))
        -- (e) HSEC release_hold_to_player (ADR 0111 6.4): the exact key and correlation
        OR (p_type = 'withdrawal_rejected' AND EXISTS (
        SELECT 1 FROM withdrawal_hold_resolutions h
         WHERE h.tenant_id = p_tenant AND h.kind = 'release_hold_to_player'
           AND h.state = 'executing' AND h.executed_txid = txid_current()
           AND p_correlation = h.withdrawal_request_id
           AND p_idempotency_key = h.withdrawal_request_id::text || ':governed_hold_released'));
$$ LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp;

-- (2/4) ledger_entries_governed_fence: the 0115 body + the withdrawal_rejected shape.
CREATE OR REPLACE FUNCTION ledger_entries_governed_fence() RETURNS TRIGGER AS $$
DECLARE
    v_tx          RECORD;
    v_req         RECORD;
    v_m           RECORD;
    v_h           RECORD;
    v_acct        RECORD;
    v_player_dir  text;
    v_house_dir   text;
BEGIN
    IF financial_acting_gucs_present() THEN
        SELECT t.tenant_id, t.transaction_type, t.idempotency_key, t.correlation_id, t.provider_id, t.provider_tx_id INTO v_tx
          FROM ledger_transactions t WHERE t.id = NEW.ledger_transaction_id AND t.tenant_id = NEW.tenant_id;
        IF NOT FOUND OR NOT ledger_governed_fence_allows(v_tx.tenant_id, v_tx.transaction_type, v_tx.idempotency_key, v_tx.correlation_id,
                                                         v_tx.provider_id, v_tx.provider_tx_id) THEN
            RAISE EXCEPTION 'ledger_entries_governed_fence: an acting session may add entries only to a governed, executing request''s transaction' USING ERRCODE = 'CG030';
        END IF;
        IF v_tx.transaction_type = 'manual_adjustment' THEN
        SELECT r.wallet_id, r.asset_code, r.amount, r.direction INTO v_req
          FROM ledger_adjustment_requests r
         WHERE r.id = v_tx.correlation_id AND r.tenant_id = v_tx.tenant_id
           AND v_tx.idempotency_key = 'manual_adjustment:' || r.id::text;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'ledger_entries_governed_fence: governed request not found' USING ERRCODE = 'CG030';
        END IF;
        v_player_dir := CASE WHEN v_req.direction = 'credit_player' THEN 'credit' ELSE 'debit' END;
        v_house_dir  := CASE WHEN v_req.direction = 'credit_player' THEN 'debit' ELSE 'credit' END;
        SELECT la.account_type, la.wallet_id INTO v_acct
          FROM ledger_accounts la WHERE la.id = NEW.ledger_account_id AND la.tenant_id = NEW.tenant_id;
        IF NOT FOUND
           OR NEW.asset_code IS DISTINCT FROM v_req.asset_code
           OR NEW.amount IS DISTINCT FROM v_req.amount
           OR NOT ((v_acct.account_type = 'player_cash' AND v_acct.wallet_id = v_req.wallet_id AND NEW.direction = v_player_dir)
                OR (v_acct.account_type = 'manual_adjustment' AND v_acct.wallet_id IS NULL AND NEW.direction = v_house_dir)) THEN
            RAISE EXCEPTION 'ledger_entries_governed_fence: the entry is not a leg of the approved §4 shape (account, asset, amount or direction)' USING ERRCODE = 'CG030';
        END IF;
        ELSIF v_tx.transaction_type IN ('withdrawal_completed', 'withdrawal_failed') THEN
            -- (b) declare paid / (c) declare not paid: the resolution, the
            -- withdrawal and the entry, all in this transaction.
            SELECT m.amount AS m_amount, w.wallet_id, w.asset_code, w.amount AS w_amount INTO v_m
              FROM payment_manual_resolutions m
              JOIN withdrawal_requests w ON w.id = m.withdrawal_request_id AND w.tenant_id = m.tenant_id
             WHERE m.tenant_id = v_tx.tenant_id
               AND m.kind = CASE v_tx.transaction_type WHEN 'withdrawal_completed' THEN 'm2_declare_paid' ELSE 'm2_declare_not_paid' END
               AND m.state = 'executing' AND m.executed_txid = txid_current()
               AND m.withdrawal_request_id = v_tx.correlation_id
               AND v_tx.idempotency_key = CASE v_tx.transaction_type
                       WHEN 'withdrawal_completed' THEN m.provider_id || ':' || m.reserved_provider_tx_id
                       ELSE m.withdrawal_request_id::text || ':failed' END;
            IF NOT FOUND THEN
                RAISE EXCEPTION 'ledger_entries_governed_fence: governed M2 resolution not found' USING ERRCODE = 'CG030';
            END IF;
            SELECT la.account_type, la.wallet_id INTO v_acct
              FROM ledger_accounts la WHERE la.id = NEW.ledger_account_id AND la.tenant_id = NEW.tenant_id;
            IF NOT FOUND
               OR NEW.asset_code IS DISTINCT FROM v_m.asset_code
               OR NEW.amount IS DISTINCT FROM v_m.w_amount
               OR NEW.amount IS DISTINCT FROM v_m.m_amount
               OR NOT COALESCE((v_acct.account_type = 'player_withdrawal_hold' AND v_acct.wallet_id = v_m.wallet_id AND NEW.direction = 'debit')
                    OR (v_tx.transaction_type = 'withdrawal_completed' AND v_acct.account_type = 'psp_clearing'
                        AND v_acct.wallet_id IS NULL AND NEW.direction = 'credit')
                    OR (v_tx.transaction_type = 'withdrawal_failed' AND v_acct.account_type = 'player_cash'
                        AND v_acct.wallet_id = v_m.wallet_id AND NEW.direction = 'credit'), false) THEN
                RAISE EXCEPTION 'ledger_entries_governed_fence: the entry is not a leg of the approved M2 shape (account, asset, amount or direction)' USING ERRCODE = 'CG030';
            END IF;
        ELSIF v_tx.transaction_type = 'withdrawal_rejected' THEN
            -- (e) HSEC release_hold_to_player: hold debit and player_cash credit,
            -- both on the withdrawal's wallet, in the resolution's asset and amount.
            SELECT h.amount AS h_amount, w.wallet_id, w.asset_code, w.amount AS w_amount INTO v_h
              FROM withdrawal_hold_resolutions h
              JOIN withdrawal_requests w ON w.id = h.withdrawal_request_id AND w.tenant_id = h.tenant_id
             WHERE h.tenant_id = v_tx.tenant_id AND h.kind = 'release_hold_to_player'
               AND h.state = 'executing' AND h.executed_txid = txid_current()
               AND h.withdrawal_request_id = v_tx.correlation_id
               AND v_tx.idempotency_key = h.withdrawal_request_id::text || ':governed_hold_released';
            IF NOT FOUND THEN
                RAISE EXCEPTION 'ledger_entries_governed_fence: governed hold resolution not found' USING ERRCODE = 'CG030';
            END IF;
            SELECT la.account_type, la.wallet_id INTO v_acct
              FROM ledger_accounts la WHERE la.id = NEW.ledger_account_id AND la.tenant_id = NEW.tenant_id;
            IF NOT FOUND
               OR NEW.asset_code IS DISTINCT FROM v_h.asset_code
               OR NEW.amount IS DISTINCT FROM v_h.w_amount
               OR NEW.amount IS DISTINCT FROM v_h.h_amount
               OR NOT COALESCE((v_acct.account_type = 'player_withdrawal_hold' AND v_acct.wallet_id = v_h.wallet_id AND NEW.direction = 'debit')
                    OR (v_acct.account_type = 'player_cash' AND v_acct.wallet_id = v_h.wallet_id AND NEW.direction = 'credit'), false) THEN
                RAISE EXCEPTION 'ledger_entries_governed_fence: the entry is not a leg of the approved hold-release shape (account, asset, amount or direction)' USING ERRCODE = 'CG030';
            END IF;
        ELSE
            RAISE EXCEPTION 'ledger_entries_governed_fence: no governed shape for transaction type %', v_tx.transaction_type USING ERRCODE = 'CG030';
        END IF;
        IF EXISTS (SELECT 1 FROM ledger_entries e
                    WHERE e.ledger_transaction_id = NEW.ledger_transaction_id
                      AND (e.direction = NEW.direction
                           OR (SELECT count(*) FROM ledger_entries e2 WHERE e2.ledger_transaction_id = NEW.ledger_transaction_id) >= 2)) THEN
            RAISE EXCEPTION 'ledger_entries_governed_fence: the linked transaction already holds this leg (at most two entries, one per direction)' USING ERRCODE = 'CG030';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

-- (3/4) the acting ledger_accounts INSERT policy: the 0115 text + the hold account of
-- an executing hold resolution (player_cash is already admitted).
DROP POLICY acting_insert ON ledger_accounts;
CREATE POLICY acting_insert ON ledger_accounts FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid
                AND (SELECT financial_acting_session_valid())
                AND (account_type IN ('player_cash', 'manual_adjustment')
                     OR (account_type = 'player_withdrawal_hold'
                         AND EXISTS (SELECT 1 FROM payment_manual_resolutions m
                                       JOIN withdrawal_requests w ON w.id = m.withdrawal_request_id AND w.tenant_id = m.tenant_id
                                      WHERE m.tenant_id = ledger_accounts.tenant_id
                                        AND m.kind IN ('m2_declare_paid', 'm2_declare_not_paid')
                                        AND m.state = 'executing' AND m.executed_txid = txid_current()
                                        AND w.wallet_id = ledger_accounts.wallet_id
                                        AND w.asset_code = ledger_accounts.asset_code))
                     OR (account_type = 'player_withdrawal_hold'
                         AND EXISTS (SELECT 1 FROM withdrawal_hold_resolutions h
                                       JOIN withdrawal_requests w ON w.id = h.withdrawal_request_id AND w.tenant_id = h.tenant_id
                                      WHERE h.tenant_id = ledger_accounts.tenant_id
                                        AND h.kind = 'release_hold_to_player'
                                        AND h.state = 'executing' AND h.executed_txid = txid_current()
                                        AND w.wallet_id = ledger_accounts.wallet_id
                                        AND w.asset_code = ledger_accounts.asset_code))
                     OR (account_type = 'psp_clearing' AND wallet_id IS NULL
                         AND EXISTS (SELECT 1 FROM payment_manual_resolutions m
                                       JOIN withdrawal_requests w ON w.id = m.withdrawal_request_id AND w.tenant_id = m.tenant_id
                                      WHERE m.tenant_id = ledger_accounts.tenant_id
                                        AND m.kind = 'm2_declare_paid'
                                        AND m.state = 'executing' AND m.executed_txid = txid_current()
                                        AND w.asset_code = ledger_accounts.asset_code))));

-- (4/4) the acting withdrawal_requests UPDATE policy: the 0115 text + "or an executing
-- hold resolution, moving the withdrawal to rejected" (the trigger above pins the rest).
DROP POLICY acting_update ON withdrawal_requests;
CREATE POLICY acting_update ON withdrawal_requests FOR UPDATE
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid
                AND (SELECT financial_acting_session_valid())
                AND (EXISTS (SELECT 1 FROM payment_manual_resolutions m
                              WHERE m.withdrawal_request_id = withdrawal_requests.id AND m.tenant_id = withdrawal_requests.tenant_id
                                AND m.state = 'executing' AND m.executed_txid = txid_current()
                                AND m.kind IN ('m2_declare_paid', 'm2_declare_not_paid'))
                     OR (withdrawal_requests.state = 'rejected'
                         AND EXISTS (SELECT 1 FROM withdrawal_hold_resolutions h
                                      WHERE h.withdrawal_request_id = withdrawal_requests.id AND h.tenant_id = withdrawal_requests.tenant_id
                                        AND h.kind = 'release_hold_to_player'
                                        AND h.state = 'executing' AND h.executed_txid = txid_current()))));

-- =========================================================================
-- 10. RLS on the two new tables: family A only (no T, no P, no system read).
-- =========================================================================

ALTER TABLE withdrawal_hold_resolutions ENABLE ROW LEVEL SECURITY;
ALTER TABLE withdrawal_hold_resolutions FORCE ROW LEVEL SECURITY;
CREATE POLICY acting_read ON withdrawal_hold_resolutions FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));
CREATE POLICY acting_insert ON withdrawal_hold_resolutions FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));
CREATE POLICY acting_update ON withdrawal_hold_resolutions FOR UPDATE
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));

ALTER TABLE withdrawal_hold_resolution_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE withdrawal_hold_resolution_approvals FORCE ROW LEVEL SECURITY;
CREATE POLICY acting_read ON withdrawal_hold_resolution_approvals FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));
CREATE POLICY acting_insert ON withdrawal_hold_resolution_approvals FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));

DO $$
DECLARE
    t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['withdrawal_hold_resolutions', 'withdrawal_hold_resolution_approvals']
    LOOP
        EXECUTE format('CREATE TRIGGER %I BEFORE DELETE ON %I FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation()', t || '_no_delete', t);
        EXECUTE format('CREATE TRIGGER %I BEFORE TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation()', t || '_no_truncate', t);
    END LOOP;
END $$;

-- =========================================================================
-- 11. SIGNED-ACTOR-PROOF coverage (ADR 0110 verifier operation table, M-10).
--     actor_proof_require(): the 0120 body + the withdrawal_hold_resolution:*
--     restriction. zz_actor_proof_guard on both tables (nine -> eleven).
-- =========================================================================

CREATE OR REPLACE FUNCTION actor_proof_require(
    p_actor        uuid,
    p_scope        text,
    p_tenant       uuid,
    p_operation    text,
    p_target       text,
    p_payload_hash text
) RETURNS void AS $$
DECLARE
    v_token   text := NULLIF(pg_catalog.current_setting('app.actor_proof', true), '');
    v_parts   text[];
    v_secret  bytea;
    v_signed  text;
    v_mac     text;
    v_iat     bigint;
    v_exp     bigint;
    v_now     bigint := pg_catalog.floor(pg_catalog.date_part('epoch', pg_catalog.clock_timestamp()))::bigint;
    v_actor   uuid;
    v_tenant  uuid;
    v_n       int;
BEGIN
    IF v_token IS NULL OR p_actor IS NULL OR p_scope IS NULL OR p_operation IS NULL OR p_target IS NULL OR p_payload_hash IS NULL THEN
        RAISE EXCEPTION 'actor_proof: no proof presented' USING ERRCODE = 'AP001';
    END IF;
    IF p_scope NOT IN ('tenant', 'platform_acting', 'platform') THEN
        RAISE EXCEPTION 'actor_proof: scope is not provable' USING ERRCODE = 'AP004';
    END IF;
    -- ADR 0110 operation table (ADR 0111 M-10): withdrawal_hold_resolution:* is
    -- provable ONLY for scope platform_acting with a tenant.
    IF p_operation LIKE 'withdrawal\_hold\_resolution:%' AND (p_scope <> 'platform_acting' OR p_tenant IS NULL) THEN
        RAISE EXCEPTION 'actor_proof: this operation is provable only for a platform_acting scope with a tenant' USING ERRCODE = 'AP004';
    END IF;
    -- The NULL-tenant encoding (an EMPTY tenant field) exists only for scope
    -- 'platform' and only for the K1 capability-grant and financial-policy-change
    -- operations; every other scope and operation must carry a tenant.
    IF p_scope = 'platform' THEN
        IF p_tenant IS NOT NULL OR NOT (p_operation LIKE 'capability\_grant:%' OR p_operation LIKE 'financial\_policy\_change:%') THEN
            RAISE EXCEPTION 'actor_proof: platform scope is not provable for this operation' USING ERRCODE = 'AP004';
        END IF;
    ELSIF p_tenant IS NULL THEN
        RAISE EXCEPTION 'actor_proof: no proof presented' USING ERRCODE = 'AP001';
    END IF;

    v_parts := pg_catalog.string_to_array(v_token, '|');
    IF pg_catalog.array_length(v_parts, 1) IS DISTINCT FROM 12 OR v_parts[1] <> 'v1' THEN
        RAISE EXCEPTION 'actor_proof: malformed proof' USING ERRCODE = 'AP001';
    END IF;
    IF v_parts[9] !~ '^[0-9]{1,12}$' OR v_parts[10] !~ '^[0-9]{1,12}$'
       OR v_parts[11] !~ '^[A-Za-z0-9_-]{16,64}$' OR v_parts[12] !~ '^[0-9a-f]{64}$' THEN
        RAISE EXCEPTION 'actor_proof: malformed proof' USING ERRCODE = 'AP001';
    END IF;
    BEGIN
        v_actor  := v_parts[3]::uuid;
        v_tenant := NULLIF(v_parts[5], '')::uuid;
    EXCEPTION WHEN OTHERS THEN
        RAISE EXCEPTION 'actor_proof: malformed proof' USING ERRCODE = 'AP001';
    END;
    v_iat := v_parts[9]::bigint;
    v_exp := v_parts[10]::bigint;

    -- Key lookup (active keys only) and MAC.
    SELECT k.secret INTO v_secret FROM public.actor_proof_keys k WHERE k.kid = v_parts[2] AND k.status = 'active';
    IF NOT FOUND THEN
        RAISE EXCEPTION 'actor_proof: proof rejected' USING ERRCODE = 'AP002';
    END IF;
    v_signed := pg_catalog.array_to_string(v_parts[1:11], '|');
    v_mac := pg_catalog.encode(public.hmac(pg_catalog.convert_to(v_signed, 'UTF8'), v_secret, 'sha256'), 'hex');
    -- Compare HMACs of both values under the same key, so the comparison is
    -- not a byte-wise early-exit on attacker-controlled input.
    IF public.hmac(pg_catalog.convert_to(v_mac, 'UTF8'), v_secret, 'sha256')
       IS DISTINCT FROM public.hmac(pg_catalog.convert_to(v_parts[12], 'UTF8'), v_secret, 'sha256') THEN
        RAISE EXCEPTION 'actor_proof: proof rejected' USING ERRCODE = 'AP002';
    END IF;

    -- Time window: not expired, not issued in the future (5 s skew), and the
    -- lifetime is at most 60 s.
    IF v_exp <= v_now OR v_iat > v_now + 5 OR v_exp <= v_iat OR v_exp - v_iat > 60 THEN
        RAISE EXCEPTION 'actor_proof: proof expired or not yet valid' USING ERRCODE = 'AP003';
    END IF;

    -- Binding: the proof must be for exactly this actor/scope/tenant and this
    -- operation/target/payload.
    IF v_actor IS DISTINCT FROM p_actor OR v_parts[4] <> p_scope OR v_tenant IS DISTINCT FROM p_tenant
       OR v_parts[6] <> p_operation OR v_parts[7] <> p_target OR v_parts[8] <> p_payload_hash THEN
        RAISE EXCEPTION 'actor_proof: proof does not match the actor/operation/target' USING ERRCODE = 'AP004';
    END IF;

    -- Replay protection: one proof, one consumption. The row commits or rolls
    -- back with the governed write, so a failed attempt does not burn it and a
    -- committed one can never be replayed (the UNIQUE nonce key serialises two
    -- concurrent consumers).
    INSERT INTO public.actor_proof_nonces
        (nonce, kid, actor, scope, tenant, operation, target, payload_hash, expires_at, consumed_txid)
    VALUES (v_parts[11], v_parts[2], v_actor, p_scope, v_tenant, p_operation, p_target, p_payload_hash,
            pg_catalog.to_timestamp(v_exp), pg_catalog.txid_current())
    ON CONFLICT (nonce) DO NOTHING;
    GET DIAGNOSTICS v_n = ROW_COUNT;
    IF v_n = 0 THEN
        RAISE EXCEPTION 'actor_proof: proof already used' USING ERRCODE = 'AP005';
    END IF;
END;
$$ LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp;

CREATE FUNCTION actor_proof_withdrawal_hold_resolutions_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor RECORD;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT * INTO v_actor FROM financial_actor_session();
        IF NEW.requested_by IS DISTINCT FROM v_actor.actor THEN
            RAISE EXCEPTION 'actor_proof: the requested_by column is not the proven actor' USING ERRCODE = 'AP004';
        END IF;
        -- The id is server-forced by the guard, so the target is the literal 'new'; the
        -- digest binds the caller-supplied payload columns.
        PERFORM actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant,
            'withdrawal_hold_resolution:request', 'new',
            k2_sha256_hex(k2_canonical(NEW.tenant_id::text, NEW.withdrawal_request_id::text, NEW.kind,
                NEW.evidence_ref_hash, NEW.reason_code)));
    ELSIF OLD.state = 'pending' AND NEW.state = 'cancelled' THEN
        SELECT * INTO v_actor FROM financial_actor_session();
        IF v_actor.actor IS DISTINCT FROM OLD.requested_by THEN
            RAISE EXCEPTION 'actor_proof: only the requester may cancel' USING ERRCODE = 'AP004';
        END IF;
        PERFORM actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant,
            'withdrawal_hold_resolution:cancel', OLD.id::text, OLD.payload_hash);
    ELSIF OLD.state = 'pending' AND NEW.state = 'expired' THEN
        -- Proof-less ONLY once the resolution has actually expired.
        IF pg_catalog.now() < OLD.expires_at THEN
            RAISE EXCEPTION 'withdrawal_hold_resolutions: the resolution has not expired' USING ERRCODE = 'HR030';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE FUNCTION actor_proof_withdrawal_hold_resolution_approvals_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor RECORD;
BEGIN
    SELECT * INTO v_actor FROM financial_actor_session();
    IF NEW.decided_by IS DISTINCT FROM v_actor.actor THEN
        RAISE EXCEPTION 'actor_proof: the decided_by column is not the proven actor' USING ERRCODE = 'AP004';
    END IF;
    PERFORM actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant,
        'withdrawal_hold_resolution:' || NEW.decision, NEW.resolution_id::text, NEW.payload_hash);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER zz_actor_proof_guard
    BEFORE INSERT OR UPDATE ON withdrawal_hold_resolutions
    FOR EACH ROW EXECUTE FUNCTION actor_proof_withdrawal_hold_resolutions_guard();
CREATE TRIGGER zz_actor_proof_guard
    BEFORE INSERT ON withdrawal_hold_resolution_approvals
    FOR EACH ROW EXECUTE FUNCTION actor_proof_withdrawal_hold_resolution_approvals_guard();

-- =========================================================================
-- 12. Runtime role grants (mirrors deploy/init-app-role.sql's 0124 block)
-- =========================================================================

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON withdrawal_hold_resolutions FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT, UPDATE ON withdrawal_hold_resolutions TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON withdrawal_hold_resolution_approvals FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON withdrawal_hold_resolution_approvals TO igaming_runtime';
    END IF;
END $$;
