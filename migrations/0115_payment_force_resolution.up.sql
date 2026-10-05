-- PRH-2 K3 (ADR 0101 revision 4 + section 26): payment force-resolution M1/M2,
-- plus the folded PAY-RECON-PARKED-CAPTURE-STANDING-1 and
-- PAY-RECON-POLL-REF-CLEAR-1 persisted-evidence substrate.
--
-- MUST RUN INSIDE A SINGLE TRANSACTION (db.MigrateUp does this).
--
-- What this adds (exactly the ADR 0101 section 20.1 object list, plus the two
-- additive K2-table triggers of section 26 RC-1):
--   1. payment_attempts_id_tenant_key (D-8): the composite FK target.
--   2. payment_manual_resolution_codes (family R, 7 rows).
--   3. payment_reserved_ref_prefix(), payment_m2_admits(...),
--      payment_manual_resolution_execution_status(...) (K3-S2).
--   4. payment_manual_resolutions / payment_manual_resolution_approvals with
--      the families T and A (no P), the R-2 system-shape read of EXECUTED
--      resolutions, the DB-guard parity triggers (R-3 (i)-(viii)), the
--      beneficiary guard (S-12), the deferred no-executing-commit check.
--   5. payment_attempt_reference_evidence (the poll's returned reference Y;
--      R-4/D-6: split system-shape policies, live-state BEFORE INSERT,
--      DEFERRABLE park binding, append-only).
--   6. Indexes on payment_statement_lines (non-concurrent, LF Q-LF-5).
--   7. payment_attempts_guard(): the 0107 body plus EXACTLY the section 8.3
--      diff (two whitelist lines, two rewritten evidence gates).
--   8. payment_attempts_operator_column_discipline (R-7, K3-S1 scope).
--   9. The reserved provider-tx namespace: CHECKs on every provider-supplied
--      reference column and the all-sessions ledger trigger.
--  10. ledger_governed_fence_allows (a)+(b)+(c), ledger_entries_governed_fence
--      (per-entry shapes), the widened acting ledger_accounts INSERT (D-1/D-2).
--  11. The acting policies on payment_attempts, withdrawal_requests,
--      deposit_intents.
--  12. ledger_adjustment_payload_refusal (Step B causation arm + the MA020
--      exemption term) and the two Step B Person-separation triggers (RC-1).
--  13. The reconciliation kinds (+3, constraint name kept).
--  14. The REVOKE ALL + GRANT block for the four new tables.
--
-- SQLSTATE class 'MR' (manual resolution). Application code classifies by
-- code only:
--   MR001 session/actor not permitted for this operation
--   MR002 actor has no linked person_id
--   MR003 actor lacks the eligible role or an in-force capability grant
--   MR010 precondition failed (state, withdrawal state, reference, basis, codes)
--   MR011 a contributing policy's author/approver may not request or approve (S-2(iii))
--   MR012 the dispute reason is not M2-resolvable (the closed allow-list)
--   MR014 payment_force_resolve is disabled (no in-force platform baseline)
--   MR020 reserved provider-tx namespace used outside an executing m2_declare_paid
--   MR030 resolution guard (immutability, state machine, closed-tenant actor scope)
--   MR031 approval guard
--   MR032 beneficiary exclusion (S-12)
--   MR040 operator-evidence column discipline
--   MR041 a resolution may never commit in state 'executing'; or its link is wrong
--   MR042 a governed posting exists: only the executed exit is allowed
--   MR050 reference-evidence guard
--   MR098 up-migration refused: a reserved-prefix value already exists
--   MR099 down-migration refused while rows exist
--   MA033 (K2 code space) Step B Person separation on a K2 compensation
--   CG030 ledger fence (ADR 0099 6.6)
--
-- No SECURITY DEFINER. FORCE ROW LEVEL SECURITY on every new table. No FOR
-- ALL permissive policy. DELETE and TRUNCATE refused on every new table. No
-- threshold value anywhere (HD-PRH2-3).

-- =========================================================================
-- 0. Up-time refusal (ADR 0101 5.4, security O-4): any reserved-prefix value
--    already present in a provider-supplied reference column. The tables are
--    FORCE RLS and this session has no app.tenant_id, so the scan iterates
--    tenants (the 0099 pattern); the ADD CONSTRAINT statements below are the
--    RLS-independent second line (they validate the physical table).
-- =========================================================================

DO $$
DECLARE
    pfx CONSTANT TEXT := 'platform-operator-declared:';
    cols CONSTANT TEXT[][] := ARRAY[
        ['payment_attempts', 'provider_reference'],
        ['payment_provider_events', 'provider_reference'],
        ['payment_provider_events', 'original_provider_reference'],
        ['payment_provider_events', 'settlement_reference'],
        ['payment_statement_lines', 'provider_reference'],
        ['payment_statement_lines', 'original_provider_reference'],
        ['payment_statement_lines', 'settlement_reference'],
        ['deposit_intents', 'provider_reference'],
        ['withdrawal_requests', 'provider_reference'],
        ['ledger_transactions', 'provider_tx_id']
    ];
    tenant_rec RECORD;
    i INT;
    n BIGINT;
    total BIGINT := 0;
    report TEXT := '';
BEGIN
    PERFORM set_config('app.player_account_id', '', true);
    PERFORM set_config('app.platform_admin_principal_id', '', true);
    FOR i IN 1 .. array_length(cols, 1) LOOP
        FOR tenant_rec IN SELECT id FROM tenants LOOP
            PERFORM set_config('app.tenant_id', tenant_rec.id::text, true);
            EXECUTE format('SELECT count(*) FROM %I WHERE tenant_id = $1 AND left(%I, 27) = $2', cols[i][1], cols[i][2])
               INTO n USING tenant_rec.id, pfx;
            IF n > 0 THEN
                report := report || format(' %s.%s=%s', cols[i][1], cols[i][2], n);
                total := total + n;
            END IF;
        END LOOP;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
    IF total > 0 THEN
        RAISE EXCEPTION 'migration 0115 pre-flight: reserved provider-tx namespace (%) already used by existing row(s):%. Nothing was changed. Never delete ledger or attempt rows; escalate to the human.', pfx, report
            USING ERRCODE = 'MR098';
    END IF;
END $$;

-- =========================================================================
-- 1. Prerequisite (LF D-8 / security R-8(e)): the composite FK target.
-- =========================================================================

ALTER TABLE payment_attempts ADD CONSTRAINT payment_attempts_id_tenant_key UNIQUE (id, tenant_id);

-- =========================================================================
-- 2. Reference data (family R). Rows inserted BEFORE FORCE RLS.
-- =========================================================================

CREATE TABLE payment_manual_resolution_codes (
    code      TEXT PRIMARY KEY,
    code_type TEXT NOT NULL CHECK (code_type IN ('finding', 'basis', 'context'))
);

-- LF ruling 1, literal. pending_suspense_allocation_b is NOT seeded
-- (product-owner-proxy, plan 11): it arrives with LEDGER-SUSPENSE-B-1.
INSERT INTO payment_manual_resolution_codes (code, code_type) VALUES
    ('awaiting_psp_refund',             'finding'),
    ('refund_requested_from_psp',       'finding'),
    ('investigated_no_platform_action', 'finding'),
    ('provider_confirmed_out_of_band',  'basis'),
    ('reconciliation_exhausted',        'basis'),
    ('provider_unqueryable',            'context'),
    ('past_resubmission_horizon',       'context');

ALTER TABLE payment_manual_resolution_codes ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_manual_resolution_codes FORCE ROW LEVEL SECURITY;
CREATE POLICY reference_read ON payment_manual_resolution_codes FOR SELECT USING (true);

-- =========================================================================
-- 3. The reserved provider-tx namespace function (27 bytes). The Go constant
--    providerref.ReservedOperatorPrefix is pinned equal by a test.
-- =========================================================================

CREATE FUNCTION payment_reserved_ref_prefix() RETURNS text AS $$
    SELECT 'platform-operator-declared:'::text;
$$ LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp;

-- =========================================================================
-- 4. payment_manual_resolutions (families T, A; no P)
-- =========================================================================

CREATE TABLE payment_manual_resolutions (
    -- Server-forced (viii): the column default is the nil UUID; any other
    -- client-supplied value is refused by the guard, which then replaces the
    -- nil with gen_random_uuid(). reserved_provider_tx_id = prefix || id is
    -- therefore never client-chosen.
    id                              UUID PRIMARY KEY DEFAULT '00000000-0000-0000-0000-000000000000',
    tenant_id                       UUID NOT NULL REFERENCES tenants (id),
    attempt_id                      UUID NOT NULL,
    operation                       TEXT NOT NULL CHECK (operation IN ('deposit', 'payout')),
    kind                            TEXT NOT NULL CHECK (kind IN ('m1_deposit_evidence', 'm2_declare_paid', 'm2_declare_not_paid')),
    target_state                    TEXT NULL CHECK (target_state IS NULL OR target_state IN ('succeeded', 'declined')),
    finding_code                    TEXT NULL REFERENCES payment_manual_resolution_codes (code),
    basis_code                      TEXT NULL REFERENCES payment_manual_resolution_codes (code),
    context_code                    TEXT NULL REFERENCES payment_manual_resolution_codes (code),
    evidence_ref_hash               TEXT NULL CHECK (evidence_ref_hash ~ '^[0-9a-f]{64}$'),
    amount                          NUMERIC(38,0) NOT NULL CHECK (amount > 0),
    asset_code                      TEXT NOT NULL,
    brand_id                        UUID NOT NULL,
    deposit_intent_id               UUID NULL,
    withdrawal_request_id           UUID NULL,
    provider_id                     TEXT NULL,
    reserved_provider_tx_id         TEXT NULL,
    reason_code                     TEXT NOT NULL CHECK (octet_length(reason_code) BETWEEN 1 AND 64),
    -- R-6: the factual basis, DB-forced from the attempt at insert and hashed.
    attempt_state_at_submission     TEXT NOT NULL,
    terminal_reason_at_submission   TEXT NULL,
    ever_possibly_sent_at_submission BOOLEAN NOT NULL,
    payload_hash                    TEXT NOT NULL,
    -- Forced actor.
    requested_by                    UUID NOT NULL,
    requested_by_scope              TEXT NOT NULL CHECK (requested_by_scope IN ('tenant', 'platform_acting')),
    requested_by_person_id          UUID NOT NULL,
    tenant_status_at_submission     TEXT NOT NULL,
    tenant_status_at_execution      TEXT NULL,
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
    FOREIGN KEY (attempt_id, tenant_id) REFERENCES payment_attempts (id, tenant_id),
    FOREIGN KEY (ledger_transaction_id, tenant_id) REFERENCES ledger_transactions (id, tenant_id),
    CHECK ((kind = 'm1_deposit_evidence') = (operation = 'deposit')),
    CHECK ((operation = 'deposit') = (deposit_intent_id IS NOT NULL)),
    CHECK ((operation = 'payout') = (withdrawal_request_id IS NOT NULL)),
    -- M1: evidence only; never a target, a link, a basis or a reserved id.
    CHECK (kind <> 'm1_deposit_evidence' OR (target_state IS NULL AND ledger_transaction_id IS NULL
                                              AND basis_code IS NULL AND reserved_provider_tx_id IS NULL
                                              AND finding_code IS NOT NULL AND context_code IS NULL)),
    -- M2: a target, a basis, evidence (C-101-2), a provider, no finding.
    CHECK (kind NOT IN ('m2_declare_paid', 'm2_declare_not_paid')
           OR (target_state = CASE kind WHEN 'm2_declare_paid' THEN 'succeeded' ELSE 'declined' END
               AND basis_code IS NOT NULL AND evidence_ref_hash IS NOT NULL AND finding_code IS NULL
               AND provider_id IS NOT NULL)),
    CHECK ((kind = 'm2_declare_paid') = (reserved_provider_tx_id IS NOT NULL)),
    CHECK (ledger_transaction_id IS NULL OR state = 'executed'),
    CHECK (state <> 'executed' OR (operation = 'payout') = (ledger_transaction_id IS NOT NULL)),
    CHECK (state NOT IN ('executing', 'executed') OR executed_txid IS NOT NULL),
    CHECK ((state = 'refused_at_execution') = (refusal_code IS NOT NULL))
);

CREATE UNIQUE INDEX payment_manual_resolutions_one_executed ON payment_manual_resolutions (attempt_id) WHERE state = 'executed';
CREATE UNIQUE INDEX payment_manual_resolutions_one_pending ON payment_manual_resolutions (attempt_id) WHERE state = 'pending';
CREATE INDEX payment_manual_resolutions_tenant_state ON payment_manual_resolutions (tenant_id, state);
CREATE INDEX payment_manual_resolutions_tenant_ledger_tx ON payment_manual_resolutions (tenant_id, ledger_transaction_id) WHERE ledger_transaction_id IS NOT NULL;

-- payment_m2_admits: ADR 0101 5.2 (used by payment_attempts_guard below).
-- Returns false, never an error, in a session that cannot see resolutions
-- (the sweeper's WithTenant shape; T-12). The allow-list is a closed literal
-- set (F9; also in the resolution guard and in the Go executor; C-5b, C-47).
CREATE FUNCTION payment_m2_admits(
    p_attempt_id uuid, p_old_state text, p_old_reason text, p_withdrawal_request_id uuid,
    p_new_state text, p_new_evidence text
) RETURNS boolean AS $$
    SELECT COALESCE(
        p_new_evidence = 'operator'
        AND p_new_state IN ('succeeded', 'declined')
        AND (p_old_state = 'ambiguous'
             OR (p_old_state = 'disputed'
                 AND p_old_reason IN ('provider_reference_mismatch', 'success_for_never_sent_attempt')))
        AND EXISTS (SELECT 1 FROM payment_attempts a
                     WHERE a.id = p_attempt_id AND a.operation = 'payout'
                       AND a.withdrawal_request_id = p_withdrawal_request_id
                       AND a.provider_id IS NOT NULL
                       AND (p_new_state <> 'succeeded' OR a.provider_reference IS NOT NULL))
        AND EXISTS (SELECT 1 FROM withdrawal_requests w
                     WHERE w.id = p_withdrawal_request_id AND w.state = 'submitted')
        AND EXISTS (SELECT 1 FROM payment_manual_resolutions m
                     WHERE m.attempt_id = p_attempt_id AND m.state = 'executing'
                       AND m.executed_txid = txid_current()
                       AND m.target_state = p_new_state
                       AND m.kind = CASE p_new_state WHEN 'succeeded' THEN 'm2_declare_paid' ELSE 'm2_declare_not_paid' END),
        false);
$$ LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp;

-- =========================================================================
-- 5. payment_manual_resolution_approvals (families T, A; no P)
-- =========================================================================

CREATE TABLE payment_manual_resolution_approvals (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID NOT NULL,
    resolution_id         UUID NOT NULL,
    decision              TEXT NOT NULL CHECK (decision IN ('approve', 'reject')),
    payload_hash          TEXT NOT NULL,
    decided_by            UUID NOT NULL,
    decided_by_scope      TEXT NOT NULL CHECK (decided_by_scope IN ('tenant', 'platform_acting')),
    decided_by_person_id  UUID NOT NULL,
    decided_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    decided_txid          BIGINT NOT NULL,
    reason_code           TEXT NOT NULL CHECK (octet_length(reason_code) BETWEEN 1 AND 64),
    UNIQUE (resolution_id, decided_by),
    FOREIGN KEY (tenant_id, resolution_id) REFERENCES payment_manual_resolutions (tenant_id, id)
);

CREATE INDEX payment_manual_resolution_approvals_tenant ON payment_manual_resolution_approvals (tenant_id);

-- =========================================================================
-- 6. Counting at execution (K3-S2) - the ONE implementation, used by the
--    executor after its FOR SHARE locks AND by the -> executing guard.
-- =========================================================================

-- An approval counts only if: decision approve; payload_hash equal; the
-- approver's in-force :approve grant at now() (eligible grant, or the
-- invisible-platform residual) with an active, eligible, tenant-consistent
-- staff row whose LIVE person equals the grant's request-time snapshot; the
-- Person is non-NULL, distinct from the requester's live Person and from
-- every other counted approver's, not the beneficiary, and not an author or
-- approver of a contributing policy; and (R-5) when the tenant status read
-- NOW is 'closed', the approval is a platform_acting one. K3-S2:
-- requester_valid re-checks the requester the same way (staff row live,
-- :request grant in force, Person unchanged, not the beneficiary, not a
-- policy author, and for a closed tenant the requester scope is
-- platform_acting).
CREATE FUNCTION payment_manual_resolution_execution_status(p_resolution uuid,
    OUT required int, OUT counted int, OUT counted_approval_ids uuid[], OUT requester_valid boolean,
    OUT contributing_policy_ids uuid[], OUT tenant_status text, OUT enabled boolean
) AS $$
DECLARE
    r              payment_manual_resolutions%ROWTYPE;
    v_policy       RECORD;
    v_authors      uuid[];
    v_owner_person uuid;
    v_req_person   uuid;
BEGIN
    SELECT * INTO r FROM payment_manual_resolutions WHERE id = p_resolution;
    IF NOT FOUND THEN
        required := 1; counted := 0; counted_approval_ids := '{}'; requester_valid := false;
        contributing_policy_ids := '{}'; enabled := false;
        RETURN;
    END IF;
    SELECT * INTO v_policy FROM financial_policy_required_approvals('payment_force_resolve', r.tenant_id, r.brand_id, r.asset_code,
        CASE WHEN r.operation = 'payout' THEN r.amount ELSE NULL END, now());
    -- Never below the pinned value (as K2 3.6).
    required := GREATEST(r.required_at_submission, v_policy.required);
    contributing_policy_ids := v_policy.contributing_policy_ids;
    tenant_status := v_policy.tenant_status;
    enabled := v_policy.enabled;
    v_authors := financial_policy_author_persons(r.contributing_policy_ids || v_policy.contributing_policy_ids);

    IF r.operation = 'deposit' THEN
        SELECT pa.person_id INTO v_owner_person
          FROM deposit_intents i JOIN player_accounts pa ON pa.id = i.player_account_id AND pa.tenant_id = i.tenant_id
         WHERE i.id = r.deposit_intent_id AND i.tenant_id = r.tenant_id;
    ELSE
        SELECT pa.person_id INTO v_owner_person
          FROM withdrawal_requests w JOIN player_accounts pa ON pa.id = w.player_account_id AND pa.tenant_id = w.tenant_id
         WHERE w.id = r.withdrawal_request_id AND w.tenant_id = r.tenant_id;
    END IF;
    v_req_person := ledger_adjustment_live_person(r.requested_by, r.requested_by_person_id);

    requester_valid := v_req_person IS NOT NULL
        AND v_req_person = r.requested_by_person_id
        AND v_owner_person IS NOT NULL
        AND v_req_person <> v_owner_person
        AND NOT (v_req_person = ANY (v_authors))
        AND (tenant_status IS DISTINCT FROM 'closed' OR r.requested_by_scope = 'platform_acting')
        AND COALESCE(ledger_adjustment_eligible_grant(r.tenant_id, r.requested_by, 'payment_force_resolve:request'),
                     ledger_adjustment_invisible_platform_grant(r.tenant_id, r.requested_by, 'payment_force_resolve:request', r.requested_by_person_id)) IS NOT NULL;

    WITH cand AS (
        SELECT a.id, a.decided_at,
               ledger_adjustment_live_person(a.decided_by, a.decided_by_person_id) AS person
          FROM payment_manual_resolution_approvals a
         WHERE a.resolution_id = r.id
           AND a.decision = 'approve'
           AND a.payload_hash = r.payload_hash
           AND a.decided_by <> r.requested_by
           AND (tenant_status IS DISTINCT FROM 'closed' OR a.decided_by_scope = 'platform_acting')
           AND COALESCE(ledger_adjustment_eligible_grant(r.tenant_id, a.decided_by, 'payment_force_resolve:approve'),
                        ledger_adjustment_invisible_platform_grant(r.tenant_id, a.decided_by, 'payment_force_resolve:approve', a.decided_by_person_id)) IS NOT NULL
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
    -- Never NULL: a NULL here would make the recount's NOT requester_valid test
    -- silently pass (the K2 initiator_valid lesson).
    requester_valid := COALESCE(requester_valid, false);
END;
$$ LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp;

-- =========================================================================
-- 7. The resolution guard (R-3 (i)-(viii); K2's ledger_adjustment_requests_guard
--    pattern). Both the INSERT shape and the UPDATE state machine live here.
-- =========================================================================

CREATE FUNCTION payment_manual_resolutions_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor        RECORD;
    v_att          RECORD;
    v_w            RECORD;
    v_intent       RECORD;
    v_policy       RECORD;
    v_exec         RECORD;
    v_code_type    text;
    v_found        boolean;
BEGIN
    SELECT * INTO v_actor FROM financial_actor_session();

    IF TG_OP = 'INSERT' THEN
        -- (i) scope tenant or platform_acting only; the session tenant.
        IF v_actor.scope NOT IN ('tenant', 'platform_acting') THEN
            RAISE EXCEPTION 'payment_manual_resolutions: only a tenant or acting session may request (HD-PRH2-6)' USING ERRCODE = 'MR001';
        END IF;
        IF v_actor.person_id IS NULL THEN
            RAISE EXCEPTION 'payment_manual_resolutions: requester has no linked person_id' USING ERRCODE = 'MR002';
        END IF;
        IF NEW.tenant_id IS DISTINCT FROM v_actor.tenant THEN
            RAISE EXCEPTION 'payment_manual_resolutions: resolution tenant must be the session tenant' USING ERRCODE = 'MR001';
        END IF;
        -- (viii) server-forced id.
        IF NEW.id IS DISTINCT FROM '00000000-0000-0000-0000-000000000000'::uuid THEN
            RAISE EXCEPTION 'payment_manual_resolutions: id is server-forced and may not be supplied' USING ERRCODE = 'MR030';
        END IF;
        NEW.id := gen_random_uuid();
        -- (ii) the capability-specific grant (T-6).
        IF ledger_adjustment_eligible_grant(NEW.tenant_id, v_actor.actor, 'payment_force_resolve:request') IS NULL THEN
            RAISE EXCEPTION 'payment_manual_resolutions: requester lacks an eligible role or an in-force payment_force_resolve:request grant' USING ERRCODE = 'MR003';
        END IF;

        SELECT a.operation, a.state, a.terminal_reason, a.amount, a.asset_code, a.provider_id, a.provider_reference,
               a.ever_possibly_sent, a.deposit_intent_id, a.withdrawal_request_id INTO v_att
          FROM payment_attempts a WHERE a.id = NEW.attempt_id AND a.tenant_id = NEW.tenant_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'payment_manual_resolutions: attempt % not found in the session tenant', NEW.attempt_id USING ERRCODE = 'MR010';
        END IF;
        IF (NEW.kind = 'm1_deposit_evidence') <> (v_att.operation = 'deposit') THEN
            RAISE EXCEPTION 'payment_manual_resolutions: kind % does not fit a % attempt', NEW.kind, v_att.operation USING ERRCODE = 'MR010';
        END IF;
        NEW.operation := v_att.operation;
        NEW.amount := v_att.amount;
        NEW.asset_code := v_att.asset_code;
        NEW.attempt_state_at_submission := v_att.state;
        NEW.terminal_reason_at_submission := v_att.terminal_reason;
        NEW.ever_possibly_sent_at_submission := v_att.ever_possibly_sent;
        IF v_att.operation = 'deposit' THEN
            SELECT i.brand_id, i.player_account_id INTO v_intent
              FROM deposit_intents i WHERE i.id = v_att.deposit_intent_id AND i.tenant_id = NEW.tenant_id;
            IF NOT FOUND THEN
                RAISE EXCEPTION 'payment_manual_resolutions: deposit intent not found' USING ERRCODE = 'MR010';
            END IF;
            NEW.brand_id := v_intent.brand_id;
            NEW.deposit_intent_id := v_att.deposit_intent_id;
            NEW.withdrawal_request_id := NULL;
            NEW.provider_id := NULL;
        ELSE
            SELECT w.brand_id, w.state INTO v_w
              FROM withdrawal_requests w WHERE w.id = v_att.withdrawal_request_id AND w.tenant_id = NEW.tenant_id;
            IF NOT FOUND THEN
                RAISE EXCEPTION 'payment_manual_resolutions: withdrawal request not found' USING ERRCODE = 'MR010';
            END IF;
            NEW.brand_id := v_w.brand_id;
            NEW.withdrawal_request_id := v_att.withdrawal_request_id;
            NEW.deposit_intent_id := NULL;
            NEW.provider_id := v_att.provider_id;
        END IF;

        -- The code vocabulary (type checks by trigger).
        IF NEW.finding_code IS NOT NULL THEN
            SELECT c.code_type INTO v_code_type FROM payment_manual_resolution_codes c WHERE c.code = NEW.finding_code;
            IF v_code_type IS DISTINCT FROM 'finding' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: finding_code is not a finding code' USING ERRCODE = 'MR010';
            END IF;
        END IF;
        IF NEW.basis_code IS NOT NULL THEN
            SELECT c.code_type INTO v_code_type FROM payment_manual_resolution_codes c WHERE c.code = NEW.basis_code;
            IF v_code_type IS DISTINCT FROM 'basis' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: basis_code is not a basis code' USING ERRCODE = 'MR010';
            END IF;
        END IF;
        IF NEW.context_code IS NOT NULL THEN
            SELECT c.code_type INTO v_code_type FROM payment_manual_resolution_codes c WHERE c.code = NEW.context_code;
            IF v_code_type IS DISTINCT FROM 'context' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: context_code is not a context code' USING ERRCODE = 'MR010';
            END IF;
        END IF;

        -- The ADR 0101 5.1 preconditions (also re-run at -> executing below).
        IF NEW.kind = 'm1_deposit_evidence' THEN
            IF v_att.state <> 'disputed' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: M1 applies only to a disputed deposit (state %)', v_att.state USING ERRCODE = 'MR010';
            END IF;
        ELSE
            IF v_att.provider_id IS NULL THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the attempt has no provider' USING ERRCODE = 'MR010';
            END IF;
            -- F9: the closed allow-list (a literal set; C-5b/C-47 pin it).
            IF NOT COALESCE(v_att.state = 'ambiguous'
                            OR (v_att.state = 'disputed'
                                AND v_att.terminal_reason IN ('provider_reference_mismatch', 'success_for_never_sent_attempt')), false) THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the attempt state/dispute reason is not M2-resolvable' USING ERRCODE = 'MR012';
            END IF;
            IF v_w.state IS DISTINCT FROM 'submitted' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the withdrawal is not submitted (state %)', v_w.state USING ERRCODE = 'MR010';
            END IF;
            -- D-3: declare paid needs the reference the provider confirmed.
            IF NEW.kind = 'm2_declare_paid' AND v_att.provider_reference IS NULL THEN
                RAISE EXCEPTION 'payment_manual_resolutions: declare paid requires the attempt to hold a provider reference (D-3)' USING ERRCODE = 'MR010';
            END IF;
            -- L-3: declare not paid after a possible dispatch needs the out-of-band confirmation basis.
            IF NEW.kind = 'm2_declare_not_paid' AND v_att.ever_possibly_sent
               AND NEW.basis_code IS DISTINCT FROM 'provider_confirmed_out_of_band' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: declare not paid after a possible dispatch requires basis provider_confirmed_out_of_band (L-3)' USING ERRCODE = 'MR010';
            END IF;
        END IF;

        NEW.requested_by := v_actor.actor;
        NEW.requested_by_scope := v_actor.scope;
        NEW.requested_by_person_id := v_actor.person_id;
        NEW.state := 'pending';
        NEW.created_at := now();
        NEW.closed_at := NULL;
        -- (vi) expires_at is DB-forced; never client-supplied.
        NEW.expires_at := now() + interval '24 hours';
        NEW.executed_txid := NULL;
        NEW.ledger_transaction_id := NULL;
        NEW.refusal_code := NULL;
        NEW.tenant_status_at_execution := NULL;
        NEW.required_at_execution := NULL;
        NEW.contributing_policy_ids_at_execution := NULL;
        NEW.target_state := CASE NEW.kind WHEN 'm2_declare_paid' THEN 'succeeded' WHEN 'm2_declare_not_paid' THEN 'declined' ELSE NULL END;
        NEW.reserved_provider_tx_id := CASE WHEN NEW.kind = 'm2_declare_paid' THEN payment_reserved_ref_prefix() || NEW.id::text ELSE NULL END;

        -- Policy (M2 by the attempt's amount and asset; M1 the base only).
        SELECT * INTO v_policy FROM financial_policy_required_approvals('payment_force_resolve', NEW.tenant_id, NEW.brand_id, NEW.asset_code,
            CASE WHEN NEW.operation = 'payout' THEN NEW.amount ELSE NULL END, now());
        NEW.tenant_status_at_submission := COALESCE(v_policy.tenant_status, 'unknown');
        IF NOT v_policy.enabled THEN
            RAISE EXCEPTION 'payment_manual_resolutions: payment_force_resolve is disabled for this tenant (no in-force platform baseline, HD-PRH2-3)' USING ERRCODE = 'MR014';
        END IF;
        -- R-5: a closed tenant admits only platform_acting actors.
        IF v_policy.tenant_status = 'closed' AND v_actor.scope <> 'platform_acting' THEN
            RAISE EXCEPTION 'payment_manual_resolutions: a closed tenant admits only platform_acting requesters (R-5)' USING ERRCODE = 'MR030';
        END IF;
        NEW.required_at_submission := v_policy.required;
        NEW.contributing_policy_ids := v_policy.contributing_policy_ids;
        -- (iii) S-2(iii).
        IF v_actor.person_id = ANY (financial_policy_author_persons(NEW.contributing_policy_ids)) THEN
            RAISE EXCEPTION 'payment_manual_resolutions: the requester authored or approved a contributing policy (S-2(iii))' USING ERRCODE = 'MR011';
        END IF;

        NEW.payload_hash := k2_sha256_hex(k2_canonical(
            NEW.tenant_id::text, NEW.attempt_id::text, NEW.kind, NEW.target_state, NEW.finding_code, NEW.basis_code,
            NEW.context_code, NEW.evidence_ref_hash, NEW.amount::text, NEW.asset_code, NEW.reason_code,
            NEW.attempt_state_at_submission, NEW.terminal_reason_at_submission));
        RETURN NEW;
    END IF;

    -- UPDATE ----------------------------------------------------------------
    IF OLD.state NOT IN ('pending', 'executing') THEN
        RAISE EXCEPTION 'payment_manual_resolutions: resolution % is terminal (%)', OLD.id, OLD.state USING ERRCODE = 'MR030';
    END IF;
    -- The payload, actor and pins are immutable (LF-10): only the state columns may change.
    IF (to_jsonb(NEW) - ARRAY['state', 'executed_txid', 'ledger_transaction_id', 'refusal_code', 'closed_at',
                              'tenant_status_at_execution', 'required_at_execution', 'contributing_policy_ids_at_execution'])
       IS DISTINCT FROM
       (to_jsonb(OLD) - ARRAY['state', 'executed_txid', 'ledger_transaction_id', 'refusal_code', 'closed_at',
                              'tenant_status_at_execution', 'required_at_execution', 'contributing_policy_ids_at_execution']) THEN
        RAISE EXCEPTION 'payment_manual_resolutions: the payload, actor and pinned policy are immutable' USING ERRCODE = 'MR030';
    END IF;
    IF NEW.payload_hash IS DISTINCT FROM k2_sha256_hex(k2_canonical(
            NEW.tenant_id::text, NEW.attempt_id::text, NEW.kind, NEW.target_state, NEW.finding_code, NEW.basis_code,
            NEW.context_code, NEW.evidence_ref_hash, NEW.amount::text, NEW.asset_code, NEW.reason_code,
            NEW.attempt_state_at_submission, NEW.terminal_reason_at_submission)) THEN
        RAISE EXCEPTION 'payment_manual_resolutions: payload_hash does not match the payload' USING ERRCODE = 'MR030';
    END IF;
    IF NEW.state IN ('executed', 'refused_at_execution', 'rejected', 'cancelled', 'expired') THEN
        NEW.closed_at := now();
    END IF;

    IF OLD.state = 'pending' AND NEW.state = 'cancelled' THEN
        -- (vii) only the requester may cancel. O-K1: no governed-posting check
        -- here (a wr.id:failed posting written by evidence after submission
        -- must not make a pending M2 uncancellable).
        IF v_actor.actor IS DISTINCT FROM OLD.requested_by THEN
            RAISE EXCEPTION 'payment_manual_resolutions: only the requester may cancel' USING ERRCODE = 'MR030';
        END IF;
        NEW.executed_txid := NULL; NEW.ledger_transaction_id := NULL; NEW.refusal_code := NULL;
        RETURN NEW;
    ELSIF OLD.state = 'pending' AND NEW.state = 'expired' THEN
        IF now() < OLD.expires_at THEN
            RAISE EXCEPTION 'payment_manual_resolutions: resolution has not expired' USING ERRCODE = 'MR030';
        END IF;
        NEW.executed_txid := NULL; NEW.ledger_transaction_id := NULL; NEW.refusal_code := NULL;
        RETURN NEW;
    ELSIF OLD.state = 'pending' AND NEW.state = 'rejected' THEN
        IF NOT EXISTS (SELECT 1 FROM payment_manual_resolution_approvals a
                        WHERE a.resolution_id = OLD.id AND a.decision = 'reject' AND a.decided_txid = txid_current()) THEN
            RAISE EXCEPTION 'payment_manual_resolutions: rejected only via a same-transaction reject decision' USING ERRCODE = 'MR030';
        END IF;
        NEW.executed_txid := NULL; NEW.ledger_transaction_id := NULL; NEW.refusal_code := NULL;
        RETURN NEW;
    ELSIF OLD.state = 'pending' AND NEW.state IN ('refused_at_execution', 'executing') THEN
        -- (vii) only in the final approval's own transaction (LF-13).
        IF NOT EXISTS (SELECT 1 FROM payment_manual_resolution_approvals a
                        WHERE a.resolution_id = OLD.id AND a.decision = 'approve' AND a.decided_txid = txid_current()) THEN
            RAISE EXCEPTION 'payment_manual_resolutions: execution happens only in the final approval''s transaction' USING ERRCODE = 'MR030';
        END IF;
        IF now() >= OLD.expires_at THEN
            RAISE EXCEPTION 'payment_manual_resolutions: resolution has expired' USING ERRCODE = 'MR030';
        END IF;
        IF NEW.state = 'refused_at_execution' THEN
            IF NEW.refusal_code IS NULL THEN
                RAISE EXCEPTION 'payment_manual_resolutions: refused_at_execution needs a refusal_code' USING ERRCODE = 'MR030';
            END IF;
            NEW.executed_txid := NULL; NEW.ledger_transaction_id := NULL;
            RETURN NEW;
        END IF;
        -- -> executing: the DB recounts with the SAME function the executor
        -- used (R-3 (iv); K3-S2) and re-runs the preconditions (R-6 pins).
        SELECT * INTO v_exec FROM payment_manual_resolution_execution_status(OLD.id);
        IF NOT v_exec.enabled OR NOT v_exec.requester_valid OR v_exec.counted < v_exec.required THEN
            RAISE EXCEPTION 'payment_manual_resolutions: fewer counted approvals (%) than required (%), or the requester no longer qualifies (K3-S2)',
                v_exec.counted, v_exec.required USING ERRCODE = 'MR030';
        END IF;
        SELECT a.operation, a.state, a.terminal_reason, a.provider_id, a.provider_reference, a.ever_possibly_sent,
               a.withdrawal_request_id INTO v_att
          FROM payment_attempts a WHERE a.id = OLD.attempt_id AND a.tenant_id = OLD.tenant_id;
        IF NOT FOUND
           OR v_att.state IS DISTINCT FROM OLD.attempt_state_at_submission
           OR v_att.terminal_reason IS DISTINCT FROM OLD.terminal_reason_at_submission THEN
            RAISE EXCEPTION 'payment_manual_resolutions: the attempt state or dispute reason changed since submission (R-6)' USING ERRCODE = 'MR030';
        END IF;
        IF OLD.kind = 'm1_deposit_evidence' THEN
            IF v_att.state <> 'disputed' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: M1 applies only to a disputed deposit' USING ERRCODE = 'MR010';
            END IF;
        ELSE
            IF v_att.provider_id IS DISTINCT FROM OLD.provider_id THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the attempt provider changed' USING ERRCODE = 'MR010';
            END IF;
            IF NOT COALESCE(v_att.state = 'ambiguous'
                            OR (v_att.state = 'disputed'
                                AND v_att.terminal_reason IN ('provider_reference_mismatch', 'success_for_never_sent_attempt')), false) THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the attempt state/dispute reason is not M2-resolvable' USING ERRCODE = 'MR012';
            END IF;
            SELECT w.state INTO v_w FROM withdrawal_requests w WHERE w.id = OLD.withdrawal_request_id AND w.tenant_id = OLD.tenant_id;
            IF NOT FOUND OR v_w.state <> 'submitted' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the withdrawal is not submitted' USING ERRCODE = 'MR010';
            END IF;
            IF OLD.kind = 'm2_declare_paid' AND v_att.provider_reference IS NULL THEN
                RAISE EXCEPTION 'payment_manual_resolutions: declare paid requires a provider reference (D-3)' USING ERRCODE = 'MR010';
            END IF;
            IF OLD.kind = 'm2_declare_not_paid' AND v_att.ever_possibly_sent
               AND OLD.basis_code IS DISTINCT FROM 'provider_confirmed_out_of_band' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: declare not paid after a possible dispatch requires provider_confirmed_out_of_band (L-3)' USING ERRCODE = 'MR010';
            END IF;
        END IF;
        NEW.required_at_execution := v_exec.required;
        NEW.contributing_policy_ids_at_execution := v_exec.contributing_policy_ids;
        NEW.tenant_status_at_execution := v_exec.tenant_status;
        NEW.executed_txid := txid_current();
        NEW.ledger_transaction_id := NULL;
        NEW.refusal_code := NULL;
        RETURN NEW;
    ELSIF OLD.state = 'executing' AND NEW.state = 'executed' THEN
        IF OLD.executed_txid IS DISTINCT FROM txid_current() OR NEW.executed_txid IS DISTINCT FROM OLD.executed_txid THEN
            RAISE EXCEPTION 'payment_manual_resolutions: executing -> executed only in the executing transaction' USING ERRCODE = 'MR030';
        END IF;
        IF OLD.operation = 'payout' AND NEW.ledger_transaction_id IS NULL THEN
            RAISE EXCEPTION 'payment_manual_resolutions: an executed M2 needs its ledger_transaction_id' USING ERRCODE = 'MR041';
        END IF;
        IF OLD.operation = 'deposit' AND NEW.ledger_transaction_id IS NOT NULL THEN
            RAISE EXCEPTION 'payment_manual_resolutions: M1 never links a ledger transaction (LF-1)' USING ERRCODE = 'MR041';
        END IF;
        RETURN NEW;
    ELSIF OLD.state = 'executing' AND NEW.state = 'refused_at_execution' THEN
        -- R-3 (v) / O-K1: a transition OUT OF executing other than executed is
        -- refused once a governed posting exists (the reserved-key posting, or
        -- the wr.id:failed posting of this withdrawal).
        IF OLD.executed_txid IS DISTINCT FROM txid_current() THEN
            RAISE EXCEPTION 'payment_manual_resolutions: executing -> % only in the executing transaction', NEW.state USING ERRCODE = 'MR030';
        END IF;
        IF NEW.refusal_code IS NULL THEN
            RAISE EXCEPTION 'payment_manual_resolutions: refused_at_execution needs a refusal_code' USING ERRCODE = 'MR030';
        END IF;
        IF EXISTS (SELECT 1 FROM ledger_transactions t
                    WHERE t.tenant_id = OLD.tenant_id
                      AND ((OLD.kind = 'm2_declare_paid' AND t.idempotency_key = OLD.provider_id || ':' || OLD.reserved_provider_tx_id)
                        OR (OLD.kind = 'm2_declare_not_paid' AND t.idempotency_key = OLD.withdrawal_request_id::text || ':failed'
                            AND t.correlation_id = OLD.withdrawal_request_id))) THEN
            RAISE EXCEPTION 'payment_manual_resolutions: % refused - a governed ledger transaction exists', NEW.state USING ERRCODE = 'MR042';
        END IF;
        NEW.ledger_transaction_id := NULL;
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'payment_manual_resolutions: invalid transition % -> %', OLD.state, NEW.state USING ERRCODE = 'MR030';
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

-- The beneficiary guard (S-12), a separate named trigger on BOTH tables
-- (ADR 0101 6.2). Self-contained (it derives the owner from the attempt's
-- parent and the actor's Person from the session), so it does not depend on
-- BEFORE-trigger firing order. A NULL staff Person is refused; a missing
-- lookup row fails closed.
CREATE FUNCTION payment_manual_resolutions_beneficiary_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor  RECORD;
    v_owner  uuid;
    v_op     text;
    v_ref    uuid;
BEGIN
    SELECT * INTO v_actor FROM financial_actor_session();
    IF v_actor.scope NOT IN ('tenant', 'platform_acting') THEN
        RAISE EXCEPTION '%: only a tenant or acting session may act (HD-PRH2-6)', TG_TABLE_NAME USING ERRCODE = 'MR001';
    END IF;
    IF TG_TABLE_NAME = 'payment_manual_resolutions' THEN
        SELECT a.operation, COALESCE(a.deposit_intent_id, a.withdrawal_request_id) INTO v_op, v_ref
          FROM payment_attempts a WHERE a.id = NEW.attempt_id AND a.tenant_id = NEW.tenant_id;
    ELSE
        SELECT a.operation, COALESCE(a.deposit_intent_id, a.withdrawal_request_id) INTO v_op, v_ref
          FROM payment_manual_resolutions r JOIN payment_attempts a ON a.id = r.attempt_id AND a.tenant_id = r.tenant_id
         WHERE r.id = NEW.resolution_id;
    END IF;
    IF v_op = 'deposit' THEN
        SELECT pa.person_id INTO v_owner
          FROM deposit_intents i JOIN player_accounts pa ON pa.id = i.player_account_id AND pa.tenant_id = i.tenant_id
         WHERE i.id = v_ref;
    ELSIF v_op = 'payout' THEN
        SELECT pa.person_id INTO v_owner
          FROM withdrawal_requests w JOIN player_accounts pa ON pa.id = w.player_account_id AND pa.tenant_id = w.tenant_id
         WHERE w.id = v_ref;
    END IF;
    IF v_owner IS NULL OR v_actor.person_id IS NULL OR v_actor.person_id = v_owner THEN
        RAISE EXCEPTION '%: the actor may not be the beneficiary (S-12), and an unlinked or unresolvable Person is refused', TG_TABLE_NAME USING ERRCODE = 'MR032';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER payment_manual_resolutions_guard
    BEFORE INSERT OR UPDATE ON payment_manual_resolutions
    FOR EACH ROW EXECUTE FUNCTION payment_manual_resolutions_guard();
CREATE TRIGGER payment_manual_resolutions_beneficiary_guard
    BEFORE INSERT ON payment_manual_resolutions
    FOR EACH ROW EXECUTE FUNCTION payment_manual_resolutions_beneficiary_guard();

-- A DEFERRABLE INITIALLY DEFERRED constraint trigger: no resolution commits
-- in state 'executing', and an executed M2 commits only with its exact link
-- (attempt = target_state; withdrawal completed/failed; the same-tenant
-- ledger transaction of the right type, key, correlation and two-entry shape).
CREATE FUNCTION payment_manual_resolutions_no_executing_commit() RETURNS TRIGGER AS $$
DECLARE
    r       payment_manual_resolutions%ROWTYPE;
    v_a     RECORD;
    v_w     RECORD;
    v_tx    RECORD;
    v_n     int;
    v_ok    int;
BEGIN
    SELECT * INTO r FROM payment_manual_resolutions WHERE id = NEW.id;
    IF NOT FOUND THEN
        RETURN NULL;
    END IF;
    IF r.state = 'executing' THEN
        RAISE EXCEPTION 'payment_manual_resolutions: resolution % may not commit in state executing', NEW.id USING ERRCODE = 'MR041';
    END IF;
    IF r.state = 'executed' AND r.operation = 'payout' THEN
        SELECT a.state INTO v_a FROM payment_attempts a WHERE a.id = r.attempt_id AND a.tenant_id = r.tenant_id;
        SELECT w.id, w.state, w.wallet_id, w.release_ledger_transaction_id INTO v_w
          FROM withdrawal_requests w WHERE w.id = r.withdrawal_request_id AND w.tenant_id = r.tenant_id;
        IF v_a.state IS DISTINCT FROM r.target_state
           OR v_w.state IS DISTINCT FROM (CASE r.kind WHEN 'm2_declare_paid' THEN 'completed' ELSE 'failed' END)
           OR v_w.release_ledger_transaction_id IS DISTINCT FROM r.ledger_transaction_id THEN
            RAISE EXCEPTION 'payment_manual_resolutions: executed M2 % does not match its attempt/withdrawal outcome', NEW.id USING ERRCODE = 'MR041';
        END IF;
        SELECT t.transaction_type, t.idempotency_key, t.correlation_id, t.provider_id, t.provider_tx_id INTO v_tx
          FROM ledger_transactions t WHERE t.id = r.ledger_transaction_id AND t.tenant_id = r.tenant_id;
        IF NOT FOUND
           OR v_tx.correlation_id IS DISTINCT FROM r.withdrawal_request_id
           OR NOT ((r.kind = 'm2_declare_paid' AND v_tx.transaction_type = 'withdrawal_completed'
                    AND v_tx.idempotency_key = r.provider_id || ':' || r.reserved_provider_tx_id
                    AND v_tx.provider_id = r.provider_id AND v_tx.provider_tx_id = r.reserved_provider_tx_id)
                OR (r.kind = 'm2_declare_not_paid' AND v_tx.transaction_type = 'withdrawal_failed'
                    AND v_tx.idempotency_key = r.withdrawal_request_id::text || ':failed'
                    AND v_tx.provider_id IS NULL AND v_tx.provider_tx_id IS NULL)) THEN
            RAISE EXCEPTION 'payment_manual_resolutions: executed M2 % links a ledger transaction that does not carry its keys', NEW.id USING ERRCODE = 'MR041';
        END IF;
        SELECT count(*) INTO v_n FROM ledger_entries e WHERE e.ledger_transaction_id = r.ledger_transaction_id;
        SELECT count(*) INTO v_ok
          FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
         WHERE e.ledger_transaction_id = r.ledger_transaction_id
           AND e.tenant_id = r.tenant_id AND e.asset_code = r.asset_code AND e.amount = r.amount
           AND ((la.account_type = 'player_withdrawal_hold' AND la.wallet_id = v_w.wallet_id AND e.direction = 'debit')
             OR (r.kind = 'm2_declare_paid' AND la.account_type = 'psp_clearing' AND la.wallet_id IS NULL AND e.direction = 'credit')
             OR (r.kind = 'm2_declare_not_paid' AND la.account_type = 'player_cash' AND la.wallet_id = v_w.wallet_id AND e.direction = 'credit'));
        IF v_n <> 2 OR v_ok <> 2 THEN
            RAISE EXCEPTION 'payment_manual_resolutions: executed M2 % entries are not exactly the approved two-leg shape', NEW.id USING ERRCODE = 'MR041';
        END IF;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE CONSTRAINT TRIGGER payment_manual_resolutions_no_executing_commit
    AFTER INSERT OR UPDATE ON payment_manual_resolutions
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION payment_manual_resolutions_no_executing_commit();

-- =========================================================================
-- 8. The approvals guard (R-3 (ii), R-5, S-2(iii), LF-11 distinct-Person floor)
-- =========================================================================

CREATE FUNCTION payment_manual_resolution_approvals_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor  RECORD;
    v_res    payment_manual_resolutions%ROWTYPE;
    v_policy RECORD;
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'payment_manual_resolution_approvals: immutable' USING ERRCODE = 'MR031';
    END IF;
    SELECT * INTO v_actor FROM financial_actor_session();
    IF v_actor.scope NOT IN ('tenant', 'platform_acting') THEN
        RAISE EXCEPTION 'payment_manual_resolution_approvals: only a tenant or acting session may decide (HD-PRH2-6)' USING ERRCODE = 'MR001';
    END IF;
    IF v_actor.person_id IS NULL THEN
        RAISE EXCEPTION 'payment_manual_resolution_approvals: approver has no linked person_id' USING ERRCODE = 'MR002';
    END IF;

    SELECT * INTO v_res FROM payment_manual_resolutions WHERE id = NEW.resolution_id;
    IF NOT FOUND OR v_res.tenant_id IS DISTINCT FROM v_actor.tenant THEN
        RAISE EXCEPTION 'payment_manual_resolution_approvals: resolution % not found in the session tenant', NEW.resolution_id USING ERRCODE = 'MR031';
    END IF;
    IF v_res.state <> 'pending' OR now() >= v_res.expires_at THEN
        RAISE EXCEPTION 'payment_manual_resolution_approvals: resolution % is not pending or has expired', NEW.resolution_id USING ERRCODE = 'MR031';
    END IF;
    IF NEW.payload_hash IS DISTINCT FROM v_res.payload_hash THEN
        RAISE EXCEPTION 'payment_manual_resolution_approvals: payload_hash does not match the resolution' USING ERRCODE = 'MR031';
    END IF;
    IF ledger_adjustment_eligible_grant(v_res.tenant_id, v_actor.actor, 'payment_force_resolve:approve') IS NULL THEN
        RAISE EXCEPTION 'payment_manual_resolution_approvals: approver lacks an eligible role or an in-force payment_force_resolve:approve grant' USING ERRCODE = 'MR003';
    END IF;
    -- LF-11 distinct-Person floor (non-configurable).
    IF v_actor.actor = v_res.requested_by
       OR v_actor.person_id = ledger_adjustment_live_person(v_res.requested_by, v_res.requested_by_person_id)
       OR v_actor.person_id = v_res.requested_by_person_id THEN
        RAISE EXCEPTION 'payment_manual_resolution_approvals: the approver must be a distinct principal and Person from the requester' USING ERRCODE = 'MR031';
    END IF;
    IF EXISTS (SELECT 1 FROM payment_manual_resolution_approvals a
                WHERE a.resolution_id = v_res.id AND a.decided_by_person_id = v_actor.person_id) THEN
        RAISE EXCEPTION 'payment_manual_resolution_approvals: this Person already decided this resolution' USING ERRCODE = 'MR031';
    END IF;
    -- S-2(iii) for approvers, pinned and current.
    SELECT * INTO v_policy FROM financial_policy_required_approvals('payment_force_resolve', v_res.tenant_id, v_res.brand_id, v_res.asset_code,
        CASE WHEN v_res.operation = 'payout' THEN v_res.amount ELSE NULL END, now());
    IF v_actor.person_id = ANY (financial_policy_author_persons(v_res.contributing_policy_ids || v_policy.contributing_policy_ids)) THEN
        RAISE EXCEPTION 'payment_manual_resolution_approvals: the approver authored or approved a contributing policy (S-2(iii))' USING ERRCODE = 'MR011';
    END IF;
    -- R-5: a closed tenant admits only platform_acting approvers.
    IF v_policy.tenant_status = 'closed' AND v_actor.scope <> 'platform_acting' THEN
        RAISE EXCEPTION 'payment_manual_resolution_approvals: a closed tenant admits only platform_acting approvers (R-5)' USING ERRCODE = 'MR030';
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

CREATE FUNCTION payment_manual_resolution_approvals_apply_reject() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.decision = 'reject' THEN
        UPDATE payment_manual_resolutions SET state = 'rejected' WHERE id = NEW.resolution_id;
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER payment_manual_resolution_approvals_guard
    BEFORE INSERT OR UPDATE ON payment_manual_resolution_approvals
    FOR EACH ROW EXECUTE FUNCTION payment_manual_resolution_approvals_guard();
CREATE TRIGGER payment_manual_resolution_approvals_beneficiary_guard
    BEFORE INSERT ON payment_manual_resolution_approvals
    FOR EACH ROW EXECUTE FUNCTION payment_manual_resolutions_beneficiary_guard();
CREATE TRIGGER payment_manual_resolution_approvals_apply_reject
    AFTER INSERT ON payment_manual_resolution_approvals
    FOR EACH ROW EXECUTE FUNCTION payment_manual_resolution_approvals_apply_reject();

-- =========================================================================
-- 9. payment_attempt_reference_evidence (folded POLL-REF-CLEAR-1; R-4, D-6)
--    The poll's returned reference Y, as typed evidence - never audit JSON
--    (INV-M-6). Written only by the sweeper's poll_reference_mismatch park.
-- =========================================================================

CREATE TABLE payment_attempt_reference_evidence (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     UUID NOT NULL,
    attempt_id    UUID NOT NULL,
    provider_id   TEXT NOT NULL CHECK (octet_length(provider_id) BETWEEN 1 AND 255 AND provider_id !~ '[\x01-\x1F\x7F-\x9F]'),
    evidence_kind TEXT NOT NULL CHECK (evidence_kind IN ('poll_returned_reference')),
    reference     TEXT NOT NULL CHECK (reference <> ''
                                       AND octet_length(reference) BETWEEN 1 AND 255
                                       AND reference !~ '[\x01-\x1F\x7F-\x9F]'),
    recorded_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    FOREIGN KEY (attempt_id, tenant_id) REFERENCES payment_attempts (id, tenant_id),
    -- A park happens exactly once: the writer uses a plain INSERT and any
    -- duplicate raises (D-6; there is no ON CONFLICT DO NOTHING).
    UNIQUE (tenant_id, attempt_id, evidence_kind)
);

CREATE FUNCTION payment_attempt_reference_evidence_guard() RETURNS TRIGGER AS $$
DECLARE
    v_att RECORD;
BEGIN
    -- O-5: lock the attempt row so its state cannot move between this check
    -- and the park that must follow in the same transaction.
    SELECT a.operation, a.state, a.provider_id, a.provider_reference INTO v_att
      FROM payment_attempts a WHERE a.id = NEW.attempt_id AND a.tenant_id = NEW.tenant_id FOR SHARE;
    IF NOT FOUND
       OR v_att.operation <> 'deposit'
       OR v_att.state NOT IN ('submitting', 'pending', 'ambiguous')
       OR v_att.provider_reference IS NULL
       OR v_att.provider_reference = NEW.reference
       OR v_att.provider_id IS DISTINCT FROM NEW.provider_id THEN
        RAISE EXCEPTION 'payment_attempt_reference_evidence: evidence is admissible only for a live deposit attempt with a different bound reference of the same provider' USING ERRCODE = 'MR050';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER payment_attempt_reference_evidence_guard
    BEFORE INSERT ON payment_attempt_reference_evidence
    FOR EACH ROW EXECUTE FUNCTION payment_attempt_reference_evidence_guard();

-- Live at insert and parked at commit means the park happened IN THIS
-- transaction. A row without its park is refused at commit (T-7).
CREATE FUNCTION payment_attempt_reference_evidence_bound_to_park() RETURNS TRIGGER AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM payment_attempts a
                    WHERE a.id = NEW.attempt_id AND a.tenant_id = NEW.tenant_id
                      AND a.state = 'disputed' AND a.terminal_reason = 'poll_reference_mismatch') THEN
        RAISE EXCEPTION 'payment_attempt_reference_evidence: row % is not bound to its poll_reference_mismatch park', NEW.id USING ERRCODE = 'MR050';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE CONSTRAINT TRIGGER payment_attempt_reference_evidence_bound_to_park
    AFTER INSERT ON payment_attempt_reference_evidence
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION payment_attempt_reference_evidence_bound_to_park();

CREATE TRIGGER payment_attempt_reference_evidence_immutable
    BEFORE UPDATE OR DELETE ON payment_attempt_reference_evidence
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();
CREATE TRIGGER payment_attempt_reference_evidence_no_truncate
    BEFORE TRUNCATE ON payment_attempt_reference_evidence
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- =========================================================================
-- 10. Indexes on payment_statement_lines (the cross-import lookup). The
--     build is non-concurrent (LF Q-LF-5, ACCEPTED): the migration runs in a
--     transaction, inside the deploy window.
-- =========================================================================

CREATE INDEX payment_statement_lines_ref ON payment_statement_lines (tenant_id, provider_id, provider_reference);
CREATE INDEX payment_statement_lines_merchant ON payment_statement_lines (tenant_id, provider_id, merchant_reference)
    WHERE merchant_reference IS NOT NULL;
CREATE INDEX payment_statement_lines_reversal_original ON payment_statement_lines (tenant_id, provider_id, original_provider_reference)
    WHERE kind = 'deposit_reversal';

-- =========================================================================
-- 11. The reserved provider-tx namespace (ADR 0101 5.4): CHECKs on every
--     provider-supplied reference column (left(), never LIKE: the prefix has
--     no wildcard so LIKE would agree, but left() is the mandated form), and
--     the all-sessions ledger trigger.
-- =========================================================================

ALTER TABLE payment_attempts
    ADD CONSTRAINT payment_attempts_provider_reference_no_reserved_prefix
        CHECK (provider_reference IS NULL OR left(provider_reference, 27) <> payment_reserved_ref_prefix());
ALTER TABLE payment_provider_events
    ADD CONSTRAINT payment_provider_events_provider_reference_no_reserved_prefix
        CHECK (left(provider_reference, 27) <> payment_reserved_ref_prefix()),
    ADD CONSTRAINT payment_provider_events_original_provider_reference_no_reserved_prefix
        CHECK (original_provider_reference IS NULL OR left(original_provider_reference, 27) <> payment_reserved_ref_prefix()),
    ADD CONSTRAINT payment_provider_events_settlement_reference_no_reserved_prefix
        CHECK (settlement_reference IS NULL OR left(settlement_reference, 27) <> payment_reserved_ref_prefix());
ALTER TABLE payment_statement_lines
    ADD CONSTRAINT payment_statement_lines_provider_reference_no_reserved_prefix
        CHECK (left(provider_reference, 27) <> payment_reserved_ref_prefix()),
    ADD CONSTRAINT payment_statement_lines_original_provider_reference_no_reserved_prefix
        CHECK (original_provider_reference IS NULL OR left(original_provider_reference, 27) <> payment_reserved_ref_prefix()),
    ADD CONSTRAINT payment_statement_lines_settlement_reference_no_reserved_prefix
        CHECK (settlement_reference IS NULL OR left(settlement_reference, 27) <> payment_reserved_ref_prefix());
ALTER TABLE payment_attempt_reference_evidence
    ADD CONSTRAINT payment_attempt_reference_evidence_reference_no_reserved_prefix
        CHECK (left(reference, 27) <> payment_reserved_ref_prefix());
-- Revision 4 R-8(a), unconditional. Safe for M2: withdrawal.Complete does not
-- write withdrawal_requests.provider_reference.
ALTER TABLE deposit_intents
    ADD CONSTRAINT deposit_intents_provider_reference_no_reserved_prefix
        CHECK (provider_reference IS NULL OR left(provider_reference, 27) <> payment_reserved_ref_prefix());
ALTER TABLE withdrawal_requests
    ADD CONSTRAINT withdrawal_requests_provider_reference_no_reserved_prefix
        CHECK (provider_reference IS NULL OR left(provider_reference, 27) <> payment_reserved_ref_prefix());

-- All sessions (casino and sportsbook postings too, security O-1): a
-- provider-sent provider_tx_id carrying the prefix raises MR020 unless the
-- row is exactly the Step B posting of an executing m2_declare_paid
-- resolution (ADR 0099 6.6 predicate (b)).
CREATE FUNCTION ledger_transactions_reserved_prefix_guard() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.provider_tx_id IS NOT NULL AND left(NEW.provider_tx_id, 27) = payment_reserved_ref_prefix() THEN
        IF NEW.transaction_type <> 'withdrawal_completed'
           OR NOT EXISTS (SELECT 1 FROM payment_manual_resolutions m
                           WHERE m.tenant_id = NEW.tenant_id AND m.kind = 'm2_declare_paid'
                             AND m.state = 'executing' AND m.executed_txid = txid_current()
                             AND NEW.correlation_id = m.withdrawal_request_id
                             AND NEW.provider_id = m.provider_id
                             AND NEW.provider_tx_id = m.reserved_provider_tx_id
                             AND NEW.idempotency_key = m.provider_id || ':' || m.reserved_provider_tx_id) THEN
            RAISE EXCEPTION 'ledger_transactions: the reserved provider-tx namespace may be used only by an executing m2_declare_paid Step B posting' USING ERRCODE = 'MR020';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER ledger_transactions_reserved_prefix_guard
    BEFORE INSERT ON ledger_transactions
    FOR EACH ROW EXECUTE FUNCTION ledger_transactions_reserved_prefix_guard();

-- =========================================================================
-- 12. payment_attempts_guard(): the 0107 body plus EXACTLY the ADR 0101 8.3
--     diff (C-17 pins it).
-- =========================================================================

CREATE OR REPLACE FUNCTION payment_attempts_guard() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF NEW.state NOT IN ('created', 'submitting') THEN
            RAISE EXCEPTION 'payment_attempts: an attempt may only be inserted in state created or submitting, got %', NEW.state;
        END IF;
        IF NEW.legacy_backfill THEN
            RAISE EXCEPTION 'payment_attempts: legacy_backfill may only be set by the 0101 backfill, before this trigger existed';
        END IF;
        IF NEW.last_evidence_kind <> 'platform' THEN
            RAISE EXCEPTION 'payment_attempts: an inserted attempt must carry last_evidence_kind=platform, got %', NEW.last_evidence_kind;
        END IF;
        IF NEW.ever_possibly_sent THEN
            RAISE EXCEPTION 'payment_attempts: an inserted attempt must have ever_possibly_sent=false';
        END IF;
        IF NEW.ledger_transaction_id IS NOT NULL THEN
            RAISE EXCEPTION 'payment_attempts: an inserted attempt must not already carry a ledger_transaction_id';
        END IF;
        IF NEW.provider_reference IS NOT NULL THEN
            RAISE EXCEPTION 'payment_attempts: an inserted attempt must not already carry a provider_reference';
        END IF;
        RETURN NEW;
    END IF;

    IF TG_OP <> 'UPDATE' THEN
        RETURN NEW;
    END IF;

    -- Immutable columns.
    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.operation IS DISTINCT FROM OLD.operation
        OR NEW.deposit_intent_id IS DISTINCT FROM OLD.deposit_intent_id
        OR NEW.withdrawal_request_id IS DISTINCT FROM OLD.withdrawal_request_id
        OR NEW.attempt_no IS DISTINCT FROM OLD.attempt_no
        OR NEW.amount IS DISTINCT FROM OLD.amount
        OR NEW.asset_code IS DISTINCT FROM OLD.asset_code
        OR NEW.payment_method IS DISTINCT FROM OLD.payment_method
        OR NEW.interactive IS DISTINCT FROM OLD.interactive
        OR NEW.legacy_backfill IS DISTINCT FROM OLD.legacy_backfill
        OR NEW.merchant_reference IS DISTINCT FROM OLD.merchant_reference
        OR NEW.external_idempotency_key IS DISTINCT FROM OLD.external_idempotency_key
        OR NEW.created_at IS DISTINCT FROM OLD.created_at
    THEN
        RAISE EXCEPTION 'payment_attempts: identity/subject/amount/reference columns are immutable after insert';
    END IF;
    IF OLD.first_submitted_at IS NOT NULL AND NEW.first_submitted_at IS DISTINCT FROM OLD.first_submitted_at THEN
        RAISE EXCEPTION 'payment_attempts: first_submitted_at is immutable once set';
    END IF;
    IF OLD.provider_id IS NOT NULL AND NEW.provider_id IS DISTINCT FROM OLD.provider_id THEN
        RAISE EXCEPTION 'payment_attempts: provider_id is immutable once set';
    END IF;
    IF OLD.provider_reference IS NOT NULL AND NEW.provider_reference IS DISTINCT FROM OLD.provider_reference THEN
        RAISE EXCEPTION 'payment_attempts: provider_reference is immutable once set';
    END IF;
    IF OLD.ledger_transaction_id IS NOT NULL AND NEW.ledger_transaction_id IS DISTINCT FROM OLD.ledger_transaction_id THEN
        RAISE EXCEPTION 'payment_attempts: ledger_transaction_id is immutable once set';
    END IF;
    IF OLD.ever_possibly_sent AND NOT NEW.ever_possibly_sent THEN
        RAISE EXCEPTION 'payment_attempts: ever_possibly_sent may only move false to true';
    END IF;
    IF NEW.submit_count < OLD.submit_count THEN
        RAISE EXCEPTION 'payment_attempts: submit_count is monotonically non-decreasing';
    END IF;
    IF OLD.last_sent_at IS NOT NULL AND NEW.last_sent_at IS NOT NULL AND NEW.last_sent_at < OLD.last_sent_at THEN
        RAISE EXCEPTION 'payment_attempts: last_sent_at is monotonically non-decreasing once set';
    END IF;

    -- State-pair whitelist (ADR 0095 §4.3, with the C6(d) fix: a
    -- tombstone discovered on a late deposit success is T13t,
    -- declined -> disputed, exactly like the existing payout-only T14
    -- pair, so the case can never hit this trigger as a rejection).
    IF NEW.state IS DISTINCT FROM OLD.state THEN
        IF NOT (
            (OLD.state = 'created'    AND NEW.state = 'submitting') OR  -- T2
            (OLD.state = 'created'    AND NEW.state = 'rejected')  OR  -- T3, M3
            (OLD.state = 'created'    AND NEW.state = 'disputed')  OR  -- T15
            (OLD.state = 'submitting' AND NEW.state = 'pending')   OR  -- T4
            (OLD.state = 'submitting' AND NEW.state = 'created')   OR  -- T5
            (OLD.state = 'submitting' AND NEW.state = 'ambiguous') OR  -- T6
            (OLD.state = 'submitting' AND NEW.state = 'succeeded') OR  -- T7
            (OLD.state = 'submitting' AND NEW.state = 'declined')  OR  -- T8
            (OLD.state = 'submitting' AND NEW.state = 'disputed')  OR  -- T10
            (OLD.state = 'pending'    AND NEW.state = 'ambiguous') OR  -- T11
            (OLD.state = 'pending'    AND NEW.state = 'succeeded') OR  -- T7
            (OLD.state = 'pending'    AND NEW.state = 'declined')  OR  -- T8
            (OLD.state = 'pending'    AND NEW.state = 'disputed')  OR  -- T10
            (OLD.state = 'ambiguous'  AND NEW.state = 'pending')   OR  -- T9
            (OLD.state = 'ambiguous'  AND NEW.state = 'submitting') OR -- T12
            (OLD.state = 'ambiguous'  AND NEW.state = 'succeeded') OR  -- T7
            (OLD.state = 'ambiguous'  AND NEW.state = 'declined')  OR  -- T8
            (OLD.state = 'ambiguous'  AND NEW.state = 'disputed')  OR  -- T10
            (OLD.state = 'declined'   AND NEW.state = 'succeeded' AND OLD.operation = 'deposit') OR -- T13
            (OLD.state = 'declined'   AND NEW.state = 'disputed')                                OR -- T13t (deposit tombstone) / T13d (deposit multiple-success) / T14 (payout)
            (OLD.state = 'rejected'   AND NEW.state = 'disputed')                                   -- T15
            OR (OLD.state = 'disputed' AND NEW.state = 'succeeded' AND OLD.operation = 'payout' AND payment_m2_admits(OLD.id, OLD.state, OLD.terminal_reason, OLD.withdrawal_request_id, NEW.state, NEW.last_evidence_kind))
            OR (OLD.state = 'disputed' AND NEW.state = 'declined'  AND OLD.operation = 'payout' AND payment_m2_admits(OLD.id, OLD.state, OLD.terminal_reason, OLD.withdrawal_request_id, NEW.state, NEW.last_evidence_kind))
        ) THEN
            RAISE EXCEPTION 'payment_attempts: transition % -> % is not permitted (id=%)', OLD.state, NEW.state, OLD.id;
        END IF;

        -- T5: back into created only when the attempt was never proven sent.
        IF NEW.state = 'created' AND OLD.ever_possibly_sent THEN
            RAISE EXCEPTION 'payment_attempts: a move into created requires ever_possibly_sent=false (id=%)', OLD.id;
        END IF;

        -- M3 (S95-C13): a payout may reach rejected only from created,
        -- never sent. Structurally already guaranteed by the CHECK that
        -- ties state=created to ever_possibly_sent=false, restated here
        -- as defense in depth per the ADR's explicit instruction.
        IF NEW.state = 'rejected' AND OLD.operation = 'payout' AND (OLD.state <> 'created' OR OLD.ever_possibly_sent) THEN
            RAISE EXCEPTION 'payment_attempts: a payout may reach rejected only from created, never sent (id=%)', OLD.id;
        END IF;

        -- T12 forbidden on a legacy-backfilled row (LF95-C11(c)): the
        -- provider never received a pa:<id> key for it.
        IF OLD.state = 'ambiguous' AND NEW.state = 'submitting' AND OLD.legacy_backfill THEN
            RAISE EXCEPTION 'payment_attempts: T12 resubmission is forbidden on a legacy_backfill row (id=%)', OLD.id;
        END IF;

        -- N3 (RV-0095 ledger; MX23): a deposit's T12 resend is refused
        -- once a sibling attempt of the same intent has already
        -- succeeded, so a platform-initiated resend can never mint a
        -- second capture for an intent that is already paid. This is
        -- defense in depth; the primary control is the same predicate in
        -- the T12 CAS statement (application code, PRH-I1 step b/c).
        IF OLD.state = 'ambiguous' AND NEW.state = 'submitting' AND OLD.operation = 'deposit'
            AND EXISTS (
                SELECT 1 FROM payment_attempts sib
                WHERE sib.deposit_intent_id = OLD.deposit_intent_id AND sib.state = 'succeeded' AND sib.id <> OLD.id
            )
        THEN
            RAISE EXCEPTION 'payment_attempts: T12 resubmission is forbidden once a sibling attempt of the same intent has succeeded (id=%)', OLD.id;
        END IF;

        -- T13t/T13d (RV-0095 ledger C6(d); ADR 0095 §28.4 AM-2): a
        -- deposit declined->disputed transition must carry one of the
        -- two named terminal reasons the M1 queue distinguishes -
        -- reversal_tombstone_precedes_success (T13t) or, new in §28,
        -- multiple_success_for_intent (T13d, a verified matching success
        -- arriving after this attempt declined, while the intent is
        -- already financially resolved by ANOTHER attempt or posting).
        -- Any other reason on a deposit declined->disputed move is
        -- refused - this is the ONLY line in this function that changed
        -- from its 0101 body.
        --
        -- Security review F-M1 (rv-fh3-security.md, 81dd4b7): SQL's
        -- `x NOT IN (...)` is NULL (neither true nor false), never TRUE,
        -- whenever x is NULL - so a bare `NEW.terminal_reason NOT IN
        -- (...)` would let a NULL terminal_reason through this IF
        -- entirely (the 0101 predicate this replaces used `IS DISTINCT
        -- FROM`, which IS NULL-safe: NULL IS DISTINCT FROM 'x' is TRUE).
        -- Explicit NULL branch restores that NULL-safety while still
        -- accepting exactly the two named reasons.
        IF OLD.state = 'declined' AND NEW.state = 'disputed' AND OLD.operation = 'deposit'
            AND (NEW.terminal_reason IS NULL
                 OR NEW.terminal_reason NOT IN ('reversal_tombstone_precedes_success', 'multiple_success_for_intent'))
        THEN
            RAISE EXCEPTION 'payment_attempts: a deposit declined->disputed transition (T13t/T13d) requires terminal_reason in (reversal_tombstone_precedes_success, multiple_success_for_intent), got % (id=%)', NEW.terminal_reason, OLD.id;
        END IF;

        -- LF95-C2 / INV-IO-7: evidence-kind gating on the two outcomes
        -- that move real money or release a hold.
        IF NEW.state = 'succeeded' AND NEW.last_evidence_kind NOT IN ('sync', 'callback', 'query_status')
           AND NOT (OLD.operation = 'payout' AND payment_m2_admits(OLD.id, OLD.state, OLD.terminal_reason, OLD.withdrawal_request_id, NEW.state, NEW.last_evidence_kind)) THEN
            RAISE EXCEPTION 'payment_attempts: ->succeeded requires last_evidence_kind in (sync, callback, query_status), got % (id=%)', NEW.last_evidence_kind, OLD.id;
        END IF;
        IF NEW.state = 'declined' AND OLD.operation = 'payout' AND NEW.last_evidence_kind NOT IN ('sync', 'callback', 'query_status')
           AND NOT (OLD.operation = 'payout' AND payment_m2_admits(OLD.id, OLD.state, OLD.terminal_reason, OLD.withdrawal_request_id, NEW.state, NEW.last_evidence_kind)) THEN
            RAISE EXCEPTION 'payment_attempts: a payout ->declined requires last_evidence_kind in (sync, callback, query_status), got % (id=%)', NEW.last_evidence_kind, OLD.id;
        END IF;
        IF NEW.state = 'declined' AND OLD.operation = 'deposit' AND NEW.last_evidence_kind = 'operator' THEN
            RAISE EXCEPTION 'payment_attempts: a deposit ->declined may never carry last_evidence_kind=operator (id=%)', OLD.id;
        END IF;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

-- R-7 / K3-S1: column discipline for operator-evidence UPDATEs. NOT an edit of
-- payment_attempts_guard() (so the 8.3 diff stays exact). Fires on (i) an
-- operator-evidence state change in ANY session, and (ii) EVERY payment_attempts
-- UPDATE of an acting session, state change or not (K3-S1: a same-state acting
-- UPDATE must not set provider_reference or ledger_transaction_id from NULL).
CREATE FUNCTION payment_attempts_operator_column_discipline() RETURNS TRIGGER AS $$
BEGIN
    IF (NEW.last_evidence_kind = 'operator' AND NEW.state IS DISTINCT FROM OLD.state)
       OR financial_acting_gucs_present() THEN
        IF (to_jsonb(NEW) - ARRAY['state', 'last_evidence_kind', 'resolved_at', 'next_action_at', 'updated_at'])
           IS DISTINCT FROM
           (to_jsonb(OLD) - ARRAY['state', 'last_evidence_kind', 'resolved_at', 'next_action_at', 'updated_at']) THEN
            RAISE EXCEPTION 'payment_attempts: an operator-evidence (or acting-session) update may change only state, last_evidence_kind, resolved_at, next_action_at and updated_at (R-7, id=%)', OLD.id
                USING ERRCODE = 'MR040';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER payment_attempts_operator_column_discipline
    BEFORE UPDATE ON payment_attempts
    FOR EACH ROW EXECUTE FUNCTION payment_attempts_operator_column_discipline();

-- =========================================================================
-- 13. The ledger fences: ADR 0099 6.6 (a)+(b)+(c) and the per-entry shapes
--     (LF D-1 = security R-1); the widened acting ledger_accounts INSERT
--     (LF D-2). Created in THIS migration, before/with every acting policy
--     below (LF C-K1-2).
-- =========================================================================

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
           AND p_idempotency_key = m.withdrawal_request_id::text || ':failed'));
$$ LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp;

-- Entries can never be appended to a pre-existing (or non-governed)
-- transaction by an acting session: the parent must pass the same fence, and
-- EACH acting-inserted entry must be one leg of the closed shape of the
-- parent's transaction type (manual_adjustment: the 0113 body, unchanged;
-- withdrawal_completed / withdrawal_failed: the ADR 0101 24.1 shapes). Any
-- other type raises CG030. Every branch needs state = 'executing'.
CREATE OR REPLACE FUNCTION ledger_entries_governed_fence() RETURNS TRIGGER AS $$
DECLARE
    v_tx          RECORD;
    v_req         RECORD;
    v_m           RECORD;
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

-- LF D-2 / security R-1: GetOrCreateAccount always runs INSERT ... ON CONFLICT
-- DO NOTHING, and the RLS WITH CHECK fires even when the row exists. Dropped
-- and re-created, widened ONLY by the two M2 account types, each bound to an
-- executing M2 resolution of this transaction.
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
                     OR (account_type = 'psp_clearing' AND wallet_id IS NULL
                         AND EXISTS (SELECT 1 FROM payment_manual_resolutions m
                                       JOIN withdrawal_requests w ON w.id = m.withdrawal_request_id AND w.tenant_id = m.tenant_id
                                      WHERE m.tenant_id = ledger_accounts.tenant_id
                                        AND m.kind = 'm2_declare_paid'
                                        AND m.state = 'executing' AND m.executed_txid = txid_current()
                                        AND w.asset_code = ledger_accounts.asset_code))));

-- =========================================================================
-- 14. RLS on the new tables (ADR 0101 8.2, 8.6(a)); acting policies on the
--     existing payment tables (6.4).
-- =========================================================================

ALTER TABLE payment_manual_resolutions ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_manual_resolutions FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope_select ON payment_manual_resolutions FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY tenant_scope_insert ON payment_manual_resolutions FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY tenant_scope_update ON payment_manual_resolutions FOR UPDATE
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present())
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
-- R-2: the reconciliation stream runs as WithTenantSnapshot (app.tenant_id
-- only, no principal). It may read ONLY executed M2 resolutions (security/LF
-- R-2 tightening: kind IN the two M2 kinds, so an executed M1 is never exposed), never pending
-- payloads, and never write.
CREATE POLICY tenant_system_read_executed ON payment_manual_resolutions FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND state = 'executed'
           AND kind IN ('m2_declare_paid', 'm2_declare_not_paid')
           AND NULLIF(current_setting('app.principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY acting_read ON payment_manual_resolutions FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));
CREATE POLICY acting_insert ON payment_manual_resolutions FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));
CREATE POLICY acting_update ON payment_manual_resolutions FOR UPDATE
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));

ALTER TABLE payment_manual_resolution_approvals ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_manual_resolution_approvals FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_scope_select ON payment_manual_resolution_approvals FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY tenant_scope_insert ON payment_manual_resolution_approvals FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY acting_read ON payment_manual_resolution_approvals FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));
CREATE POLICY acting_insert ON payment_manual_resolution_approvals FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));

-- R-4: system-shape policies only (the sweeper's WithTenant shape inserts, the
-- reconciliation WithTenantSnapshot shape reads). No NULL arm, no UPDATE or
-- DELETE policy, no tenant-staff, player, acting or platform policy.
ALTER TABLE payment_attempt_reference_evidence ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_attempt_reference_evidence FORCE ROW LEVEL SECURITY;
CREATE POLICY system_insert ON payment_attempt_reference_evidence FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY system_select ON payment_attempt_reference_evidence FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND NULLIF(current_setting('app.principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());

-- ADR 0101 6.4: the acting UPDATE policies. USING is row visibility (it
-- allows SELECT ... FOR UPDATE locking); WITH CHECK is what an actual UPDATE
-- may write: operator evidence, and an executing M2 resolution of this txid.
CREATE POLICY acting_update ON payment_attempts FOR UPDATE
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid
                AND (SELECT financial_acting_session_valid())
                AND last_evidence_kind = 'operator'
                AND EXISTS (SELECT 1 FROM payment_manual_resolutions m
                             WHERE m.attempt_id = payment_attempts.id AND m.tenant_id = payment_attempts.tenant_id
                               AND m.state = 'executing' AND m.executed_txid = txid_current()
                               AND m.kind IN ('m2_declare_paid', 'm2_declare_not_paid')));
CREATE POLICY acting_read ON withdrawal_requests FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()));
CREATE POLICY acting_update ON withdrawal_requests FOR UPDATE
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()))
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid
                AND (SELECT financial_acting_session_valid())
                AND EXISTS (SELECT 1 FROM payment_manual_resolutions m
                             WHERE m.withdrawal_request_id = withdrawal_requests.id AND m.tenant_id = withdrawal_requests.tenant_id
                               AND m.state = 'executing' AND m.executed_txid = txid_current()
                               AND m.kind IN ('m2_declare_paid', 'm2_declare_not_paid')));
-- M1 never updates an intent: the policy exists only so the executor's
-- SELECT ... FOR UPDATE lock works under an acting session.
CREATE POLICY acting_update ON deposit_intents FOR UPDATE
    USING (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid AND (SELECT financial_acting_session_valid()))
    WITH CHECK (false);

-- DELETE / TRUNCATE refused on every new table.
DO $$
DECLARE
    t TEXT;
BEGIN
    FOREACH t IN ARRAY ARRAY['payment_manual_resolutions', 'payment_manual_resolution_approvals']
    LOOP
        EXECUTE format('CREATE TRIGGER %I BEFORE DELETE ON %I FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation()', t || '_no_delete', t);
        EXECUTE format('CREATE TRIGGER %I BEFORE TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation()', t || '_no_truncate', t);
    END LOOP;
END $$;
CREATE TRIGGER payment_manual_resolution_codes_immutable
    BEFORE UPDATE OR DELETE ON payment_manual_resolution_codes FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();
CREATE TRIGGER payment_manual_resolution_codes_no_truncate
    BEFORE TRUNCATE ON payment_manual_resolution_codes FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

-- =========================================================================
-- 15. ledger_adjustment_payload_refusal: the 0113 body plus (i) the M2 Step B
--     compensating-credit causation arm (ADR 0100 5.4 adopted; security C-4
--     (a)-(e)) and (ii) the MA020 exemption term (ADR 0101 18.3, section 26
--     RC-1). v_step_b_arm is the PAYLOAD-ARM conditions only, assigned only on
--     the Step B path after every one of them has passed, never a parameter.
--     Person separation is enforced by the two additive triggers below and,
--     at execution, by the re-check inside the arm (using p_self).
-- =========================================================================

CREATE OR REPLACE FUNCTION ledger_adjustment_payload_refusal(
    p_self uuid, p_tenant uuid, p_wallet uuid, p_player uuid, p_asset text, p_direction text,
    p_amount numeric, p_reason text, p_causation uuid, p_evidence text, p_tenant_status text
) RETURNS text AS $$
DECLARE
    v_rc         ledger_adjustment_reason_codes%ROWTYPE;
    v_wallet     RECORD;
    v_cause_type text;
    v_cause_ptx  text;
    v_leg_sum    numeric;
    v_leg_count  int;
    v_executed   numeric;
    v_step_b_arm boolean := false;
    v_m          uuid;
BEGIN
    SELECT * INTO v_rc FROM ledger_adjustment_reason_codes WHERE reason_code = p_reason;
    IF NOT FOUND THEN
        RETURN 'MA022:unknown_reason_code';
    END IF;
    IF NOT (p_direction = ANY (v_rc.allowed_directions)) THEN
        RETURN 'MA022:direction_not_allowed_for_reason';
    END IF;

    SELECT w.asset_code, w.player_account_id, w.tenant_id INTO v_wallet FROM wallets w WHERE w.id = p_wallet;
    IF NOT FOUND OR v_wallet.tenant_id <> p_tenant OR v_wallet.player_account_id <> p_player THEN
        RETURN 'MA021:wallet_not_found';
    END IF;
    IF v_wallet.asset_code <> p_asset OR NOT EXISTS (SELECT 1 FROM assets WHERE code = p_asset) THEN
        RETURN 'MA021:asset_mismatch';
    END IF;
    IF NOT v_rc.allowed_in_suspended_asset AND ledger_adjustment_asset_suspended(p_tenant, p_asset) THEN
        RETURN 'MA021:asset_suspended';
    END IF;
    IF NOT v_rc.allowed_for_non_active_tenant AND p_tenant_status IS DISTINCT FROM 'active' THEN
        RETURN 'MA023:tenant_not_active';
    END IF;
    IF v_rc.evidence_required AND p_evidence IS NULL THEN
        RETURN 'MA022:evidence_required';
    END IF;

    IF v_rc.causation_rule = 'forbidden' AND p_causation IS NOT NULL THEN
        RETURN 'MA022:causation_forbidden';
    END IF;
    IF v_rc.causation_rule = 'required_compensation' AND p_causation IS NULL THEN
        RETURN 'MA022:causation_required';
    END IF;
    IF p_causation IS NOT NULL THEN
        SELECT t.transaction_type, t.provider_tx_id INTO v_cause_type, v_cause_ptx
          FROM ledger_transactions t WHERE t.id = p_causation AND t.tenant_id = p_tenant;
        IF NOT FOUND THEN
            RETURN 'MA022:causation_not_found';
        END IF;
        -- Causation to deposits, reversals and tombstones is refused for
        -- every code (LF ruling 1; INV-DEP-1).
        IF v_cause_type IN ('deposit', 'deposit_reversal', 'tombstone') THEN
            RETURN 'MA022:causation_type_refused';
        END IF;
        -- The M2 Step B arm (security C-4 (a)-(e)): reason compensating_entry,
        -- credit only; the causation is a withdrawal_completed under the
        -- reserved prefix (left(), never LIKE) and is the ledger_transaction_id
        -- of an EXECUTED m2_declare_paid of this tenant.
        IF p_reason = 'compensating_entry' AND p_direction = 'credit_player'
           AND v_cause_type = 'withdrawal_completed'
           AND left(v_cause_ptx, 27) = payment_reserved_ref_prefix() THEN
            SELECT m.id INTO v_m FROM payment_manual_resolutions m
             WHERE m.tenant_id = p_tenant AND m.kind = 'm2_declare_paid' AND m.state = 'executed'
               AND m.ledger_transaction_id = p_causation;
        END IF;
        IF v_m IS NOT NULL THEN
            -- The player_withdrawal_hold leg on THIS wallet and asset
            -- replaces the player_cash leg test for this arm only.
            SELECT COALESCE(sum(e.amount), 0), count(*) INTO v_leg_sum, v_leg_count
              FROM ledger_entries e
              JOIN ledger_accounts la ON la.id = e.ledger_account_id
             WHERE e.ledger_transaction_id = p_causation AND e.tenant_id = p_tenant
               AND la.wallet_id = p_wallet AND la.account_type = 'player_withdrawal_hold' AND e.asset_code = p_asset
               AND e.direction = 'debit';
            IF v_leg_count = 0 THEN
                RETURN 'MA022:causation_not_on_wallet';
            END IF;
            -- INV-ADJ-6: cumulative compensation per (causation, direction)
            -- never exceeds the hold leg (serialized by the executor's L2 lock).
            SELECT COALESCE(sum(r.amount), 0) INTO v_executed
              FROM ledger_adjustment_requests r
             WHERE r.tenant_id = p_tenant AND r.causation_transaction_id = p_causation
               AND r.direction = p_direction AND r.reason_code = 'compensating_entry'
               AND r.state = 'executed' AND r.id IS DISTINCT FROM p_self;
            IF v_executed + p_amount > v_leg_sum THEN
                RETURN 'MA022:compensation_cap_exceeded';
            END IF;
            -- RC-1: Person separation at execution (p_self, visible at
            -- -> executing): no Person who requested or approved the M2 may
            -- initiate or approve its compensation. The insert-time homes are
            -- the two step_b_person_sep triggers; this is the fail-closed
            -- re-check (the arm stays false and the function refuses).
            IF EXISTS (
                SELECT 1 FROM ledger_adjustment_requests q
                 WHERE q.id = p_self
                   AND (q.initiated_by_person_id IN (SELECT m.requested_by_person_id FROM payment_manual_resolutions m WHERE m.id = v_m)
                        OR q.initiated_by_person_id IN (SELECT a.decided_by_person_id FROM payment_manual_resolution_approvals a
                                                          WHERE a.resolution_id = v_m AND a.decision = 'approve')
                        OR EXISTS (SELECT 1 FROM ledger_adjustment_approvals ka
                                    WHERE ka.request_id = q.id AND ka.decision = 'approve'
                                      AND (ka.decided_by_person_id IN (SELECT m.requested_by_person_id FROM payment_manual_resolutions m WHERE m.id = v_m)
                                           OR ka.decided_by_person_id IN (SELECT a.decided_by_person_id FROM payment_manual_resolution_approvals a
                                                                           WHERE a.resolution_id = v_m AND a.decision = 'approve'))))) THEN
                RETURN 'MA033:step_b_person_separation';
            END IF;
            v_step_b_arm := true;
        ELSE
            SELECT COALESCE(sum(e.amount), 0), count(*) INTO v_leg_sum, v_leg_count
              FROM ledger_entries e
              JOIN ledger_accounts la ON la.id = e.ledger_account_id
             WHERE e.ledger_transaction_id = p_causation AND e.tenant_id = p_tenant
               AND la.wallet_id = p_wallet AND la.account_type = 'player_cash' AND e.asset_code = p_asset;
            IF v_leg_count = 0 THEN
                RETURN 'MA022:causation_not_on_wallet';
            END IF;
            IF v_rc.causation_rule = 'required_compensation' THEN
                -- INV-ADJ-6: cumulative compensation per (causation, direction)
                -- never exceeds the causation's player_cash leg on this wallet.
                SELECT COALESCE(sum(r.amount), 0) INTO v_executed
                  FROM ledger_adjustment_requests r
                 WHERE r.tenant_id = p_tenant AND r.causation_transaction_id = p_causation
                   AND r.direction = p_direction AND r.reason_code = 'compensating_entry'
                   AND r.state = 'executed' AND r.id IS DISTINCT FROM p_self;
                IF v_executed + p_amount > v_leg_sum THEN
                    RETURN 'MA022:compensation_cap_exceeded';
                END IF;
            END IF;
        END IF;
    END IF;

    -- LF F4 / ruling 2 (PREVENTIVE): every credit is refused while the player
    -- has an open captured-unposted exposure - EXCEPT the M2 Step B credit
    -- (ADR 0101 18.3; LF Q-LF-2; security Q-SEC-2 (a)-(d)): v_step_b_arm is
    -- true only after every C-4 payload condition and the Person-separation
    -- re-check held above; it is never a free-standing "is compensating" flag.
    IF p_direction = 'credit_player' AND player_open_payment_exposure(p_tenant, p_player) AND NOT v_step_b_arm THEN
        RETURN 'MA020:open_payment_exposure';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp;

-- RC-1: additive Person-separation triggers (neither edits a K2 body). The
-- names sort after ledger_adjustment_requests_guard / _approvals_guard so each
-- sees the forced initiated_by_person_id / decided_by_person_id.
CREATE FUNCTION ledger_adjustment_requests_step_b_person_sep() RETURNS TRIGGER AS $$
DECLARE
    v_m uuid;
BEGIN
    IF NEW.causation_transaction_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT m.id INTO v_m FROM payment_manual_resolutions m
     WHERE m.tenant_id = NEW.tenant_id AND m.kind = 'm2_declare_paid' AND m.state = 'executed'
       AND m.ledger_transaction_id = NEW.causation_transaction_id;
    IF v_m IS NULL THEN
        RETURN NEW;
    END IF;
    IF NEW.initiated_by_person_id IN (SELECT m.requested_by_person_id FROM payment_manual_resolutions m WHERE m.id = v_m)
       OR NEW.initiated_by_person_id IN (SELECT a.decided_by_person_id FROM payment_manual_resolution_approvals a
                                          WHERE a.resolution_id = v_m AND a.decision = 'approve') THEN
        RAISE EXCEPTION 'ledger_adjustment_requests: a Person who requested or approved the M2 Step B may not initiate its compensation (RC-1)' USING ERRCODE = 'MA033';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER ledger_adjustment_requests_step_b_person_sep
    BEFORE INSERT ON ledger_adjustment_requests
    FOR EACH ROW EXECUTE FUNCTION ledger_adjustment_requests_step_b_person_sep();

CREATE FUNCTION ledger_adjustment_approvals_step_b_person_sep() RETURNS TRIGGER AS $$
DECLARE
    v_cause uuid;
    v_m     uuid;
BEGIN
    SELECT r.causation_transaction_id INTO v_cause FROM ledger_adjustment_requests r WHERE r.id = NEW.request_id;
    IF v_cause IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT m.id INTO v_m FROM payment_manual_resolutions m
     WHERE m.tenant_id = NEW.tenant_id AND m.kind = 'm2_declare_paid' AND m.state = 'executed'
       AND m.ledger_transaction_id = v_cause;
    IF v_m IS NULL THEN
        RETURN NEW;
    END IF;
    IF NEW.decided_by_person_id IN (SELECT m.requested_by_person_id FROM payment_manual_resolutions m WHERE m.id = v_m)
       OR NEW.decided_by_person_id IN (SELECT a.decided_by_person_id FROM payment_manual_resolution_approvals a
                                        WHERE a.resolution_id = v_m AND a.decision = 'approve') THEN
        RAISE EXCEPTION 'ledger_adjustment_approvals: a Person who requested or approved the M2 Step B may not approve its compensation (RC-1)' USING ERRCODE = 'MA033';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER ledger_adjustment_approvals_step_b_person_sep
    BEFORE INSERT ON ledger_adjustment_approvals
    FOR EACH ROW EXECUTE FUNCTION ledger_adjustment_approvals_step_b_person_sep();

-- =========================================================================
-- 16. Reconciliation kinds (LF K3-a): a strict superset of 0113's list. The
--     constraint NAME is kept (the 0097/0098 tests match it).
-- =========================================================================

ALTER TABLE reconciliation_mismatches DROP CONSTRAINT reconciliation_mismatches_mismatch_kind_check;
ALTER TABLE reconciliation_mismatches ADD CONSTRAINT reconciliation_mismatches_mismatch_kind_check
    CHECK (mismatch_kind IN (
        'missing_projection', 'balance_mismatch',
        'sb_locked_mismatch', 'sb_bet_net_mismatch', 'sb_orphan_ledger', 'sb_orphan_history',
        'sb_status_mismatch', 'sb_mock_statement_mismatch',
        'cas_round_binding_mismatch', 'cas_posting_shape_mismatch', 'cas_orphan_win',
        'cas_rollback_linkage_mismatch', 'cas_tombstone_conflict', 'cas_unposted_provider_event',
        'cas_tombstone_late_original', 'cas_mock_statement_mismatch',
        'pay_missing_platform_record', 'pay_missing_provider_record', 'pay_amount_mismatch',
        'pay_asset_mismatch', 'pay_reference_mismatch', 'pay_status_mismatch', 'pay_duplicate',
        'pay_unresolved', 'pay_captured_unposted',
        'ledger_unlinked_manual_adjustment',
        'pay_declared_paid_unconfirmed', 'pay_declared_not_paid_but_paid', 'pay_declared_paid_compensated_but_paid'
    ));

-- =========================================================================
-- 17. Runtime role grants (mirrors deploy/init-app-role.sql's 0115 block).
-- =========================================================================

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON payment_manual_resolution_codes FROM igaming_runtime';
        EXECUTE 'GRANT SELECT ON payment_manual_resolution_codes TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON payment_manual_resolutions FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT, UPDATE ON payment_manual_resolutions TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON payment_manual_resolution_approvals FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON payment_manual_resolution_approvals TO igaming_runtime';
        EXECUTE 'REVOKE ALL ON payment_attempt_reference_evidence FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON payment_attempt_reference_evidence TO igaming_runtime';
    END IF;
END $$;
