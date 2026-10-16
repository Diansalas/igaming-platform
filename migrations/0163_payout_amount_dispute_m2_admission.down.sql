-- Reverses 0163 (A5 / PAYOUT-AMOUNT-DISPUTE-1; DEV/STAGING ONLY). Restores the 0115 payment_m2_admits and the
-- 0125 payment_manual_resolutions_guard bodies byte for byte (whole-schema snapshot test). Refuses (MR099) while an
-- executed or pending M2 resolution of an amount/asset-mismatch park exists (rows are never deleted to make a
-- down pass).

DO $$
DECLARE
    tenant_rec RECORD;
    c BIGINT := 0;
    n BIGINT;
BEGIN
    PERFORM set_config('app.player_account_id', '', true);
    PERFORM set_config('app.platform_admin_principal_id', '', true);
    PERFORM set_config('app.acting_tenant_id', '', true);
    PERFORM set_config('app.acting_platform_principal_id', '', true);
    FOR tenant_rec IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.tenant_id', tenant_rec.id::text, true);
        SELECT count(*) INTO n FROM payment_manual_resolutions m
         WHERE m.tenant_id = tenant_rec.id AND m.kind IN ('m2_declare_paid', 'm2_declare_not_paid')
           AND m.terminal_reason_at_submission IN ('amount_asset_mismatch', 'callback_amount_asset_mismatch')
           AND m.state IN ('pending', 'executed');
        c := c + n;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
    IF c > 0 THEN
        RAISE EXCEPTION 'migration 0163 down refused: % M2 resolutions of amount/asset-mismatch parks exist; rows are never deleted', c
            USING ERRCODE = 'MR099';
    END IF;
END $$;

CREATE OR REPLACE FUNCTION payment_m2_admits(
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


CREATE OR REPLACE FUNCTION payment_manual_resolutions_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor        RECORD;
    v_att          RECORD;
    v_w            RECORD;
    v_intent       RECORD;
    v_policy       RECORD;
    v_exec         RECORD;
    v_ev           RECORD;
    v_code_type    text;
    v_found        boolean;
    v_m4           boolean;
BEGIN
    SELECT * INTO v_actor FROM financial_actor_session();

    IF TG_OP = 'INSERT' THEN
        v_m4 := NEW.kind IN ('m4_evidence_paid', 'm4_evidence_not_paid');
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
        -- ADR 0111 R-4 / 4.5: on an M4 the amount, the asset and every evidence
        -- column but the requested line are DB-forced; a client value is refused.
        IF v_m4 AND (NEW.amount IS NOT NULL OR NEW.asset_code IS NOT NULL OR NEW.evidence_reference IS NOT NULL
                     OR NEW.evidence_verdict IS NOT NULL OR NEW.evidence_import_ids IS NOT NULL
                     OR NEW.provider_reference_at_submission IS NOT NULL) THEN
            RAISE EXCEPTION 'payment_manual_resolutions: an M4 amount, asset and evidence columns are DB-forced; a client value is refused (R-4)' USING ERRCODE = 'MR010';
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
            SELECT w.brand_id, w.state, w.amount, w.asset_code INTO v_w
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
        ELSIF NOT v_m4 THEN
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
        ELSE
            -- M4 (ADR 0111 4.1/4.2; C-6, M-10, R-4).
            IF v_att.provider_id IS NULL THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the attempt has no provider' USING ERRCODE = 'MR010';
            END IF;
            IF NOT payment_m4_in_scope(NEW.kind, v_att.state, v_att.terminal_reason, v_att.provider_reference) THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the attempt is outside the M4 scope (ADR 0111 4.1: unbound reason with no reference; destination_mismatch not-paid only)' USING ERRCODE = 'MR012';
            END IF;
            IF v_w.state IS DISTINCT FROM 'submitted' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the withdrawal is not submitted (state %)', v_w.state USING ERRCODE = 'MR010';
            END IF;
            IF v_w.amount IS DISTINCT FROM v_att.amount OR v_w.asset_code IS DISTINCT FROM v_att.asset_code THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the withdrawal amount/asset differ from the attempt''s (R-4)' USING ERRCODE = 'MR010';
            END IF;
            IF NEW.basis_code IS DISTINCT FROM 'provider_confirmed_out_of_band' OR NEW.context_code IS NOT NULL
               OR NEW.finding_code IS NOT NULL OR NEW.evidence_ref_hash IS NULL OR NEW.evidence_line_id IS NULL THEN
                RAISE EXCEPTION 'payment_manual_resolutions: an M4 needs basis provider_confirmed_out_of_band, the portal confirmation hash and the evidence line, and no finding/context' USING ERRCODE = 'MR010';
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

        IF v_m4 THEN
            -- ADR 0111 4.5: the evidence is recomputed HERE (after every
            -- authorisation check above), and the requested line must be the
            -- deterministic one. C-6 / R-6: the reference is pinned.
            NEW.provider_reference_at_submission := v_att.provider_reference;
            SELECT e.verdict, e.line_id, e.reference, e.import_ids INTO v_ev FROM payout_m4_evidence(NEW.tenant_id, NEW.attempt_id) e;
            IF v_ev.verdict = 'evidence_overflow' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the M4 evidence read exceeds 64 statement lines (refused, never truncated)' USING ERRCODE = 'MR063';
            END IF;
            IF v_ev.verdict IS DISTINCT FROM (CASE NEW.kind WHEN 'm4_evidence_paid' THEN 'paid' ELSE 'not_paid' END) THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the M4 evidence verdict is % (the kind needs a positive verdict)', COALESCE(v_ev.verdict, '<none>') USING ERRCODE = 'MR062';
            END IF;
            IF v_ev.line_id IS DISTINCT FROM NEW.evidence_line_id THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the requested evidence line is not the deterministic evidence line' USING ERRCODE = 'MR061';
            END IF;
            NEW.evidence_verdict := v_ev.verdict;
            NEW.evidence_reference := v_ev.reference;
            NEW.evidence_import_ids := v_ev.import_ids;
            NEW.payload_hash := k2_sha256_hex(k2_canonical(
                NEW.tenant_id::text, NEW.attempt_id::text, NEW.kind, NEW.target_state, NEW.finding_code, NEW.basis_code,
                NEW.context_code, NEW.evidence_ref_hash, NEW.amount::text, NEW.asset_code, NEW.reason_code,
                NEW.attempt_state_at_submission, NEW.terminal_reason_at_submission,
                NEW.evidence_line_id::text, NEW.evidence_reference, NEW.evidence_verdict,
                array_to_string(NEW.evidence_import_ids, ','), NEW.provider_reference_at_submission));
            RETURN NEW;
        END IF;

        NEW.payload_hash := k2_sha256_hex(k2_canonical(
            NEW.tenant_id::text, NEW.attempt_id::text, NEW.kind, NEW.target_state, NEW.finding_code, NEW.basis_code,
            NEW.context_code, NEW.evidence_ref_hash, NEW.amount::text, NEW.asset_code, NEW.reason_code,
            NEW.attempt_state_at_submission, NEW.terminal_reason_at_submission));
        RETURN NEW;
    END IF;

    -- UPDATE ----------------------------------------------------------------
    v_m4 := OLD.kind IN ('m4_evidence_paid', 'm4_evidence_not_paid');
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
    IF NEW.payload_hash IS DISTINCT FROM (CASE WHEN v_m4 THEN k2_sha256_hex(k2_canonical(
            NEW.tenant_id::text, NEW.attempt_id::text, NEW.kind, NEW.target_state, NEW.finding_code, NEW.basis_code,
            NEW.context_code, NEW.evidence_ref_hash, NEW.amount::text, NEW.asset_code, NEW.reason_code,
            NEW.attempt_state_at_submission, NEW.terminal_reason_at_submission,
            NEW.evidence_line_id::text, NEW.evidence_reference, NEW.evidence_verdict,
            array_to_string(NEW.evidence_import_ids, ','), NEW.provider_reference_at_submission))
        ELSE k2_sha256_hex(k2_canonical(
            NEW.tenant_id::text, NEW.attempt_id::text, NEW.kind, NEW.target_state, NEW.finding_code, NEW.basis_code,
            NEW.context_code, NEW.evidence_ref_hash, NEW.amount::text, NEW.asset_code, NEW.reason_code,
            NEW.attempt_state_at_submission, NEW.terminal_reason_at_submission)) END) THEN
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
        -- ADR 0111 S-6: the M4 platform_acting approver floor is part of the recount.
        SELECT * INTO v_exec FROM payment_manual_resolution_execution_status(OLD.id);
        IF NOT v_exec.enabled OR NOT v_exec.requester_valid OR v_exec.counted < v_exec.required
           OR NOT v_exec.platform_floor_met THEN
            RAISE EXCEPTION 'payment_manual_resolutions: fewer counted approvals (%) than required (%), the requester no longer qualifies, or no counted platform_acting approval on an M4 (K3-S2, S-6)',
                v_exec.counted, v_exec.required USING ERRCODE = 'MR030';
        END IF;
        SELECT a.operation, a.state, a.terminal_reason, a.provider_id, a.provider_reference, a.ever_possibly_sent,
               a.withdrawal_request_id, a.amount, a.asset_code INTO v_att
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
        ELSIF NOT v_m4 THEN
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
        ELSE
            -- M4 (ADR 0111 4.5 step 8; S-2, R-3, R-4): Go is the first check,
            -- not the only one.
            IF v_att.provider_id IS DISTINCT FROM OLD.provider_id THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the attempt provider changed' USING ERRCODE = 'MR010';
            END IF;
            IF NOT payment_m4_in_scope(OLD.kind, v_att.state, v_att.terminal_reason, v_att.provider_reference)
               OR v_att.provider_reference IS DISTINCT FROM OLD.provider_reference_at_submission THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the attempt left the M4 scope or its reference changed since submission (C-6, R-3)' USING ERRCODE = 'MR012';
            END IF;
            SELECT w.state, w.amount, w.asset_code INTO v_w FROM withdrawal_requests w WHERE w.id = OLD.withdrawal_request_id AND w.tenant_id = OLD.tenant_id;
            IF NOT FOUND OR v_w.state <> 'submitted' THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the withdrawal is not submitted' USING ERRCODE = 'MR010';
            END IF;
            IF v_w.amount IS DISTINCT FROM OLD.amount OR v_w.asset_code IS DISTINCT FROM OLD.asset_code
               OR v_att.amount IS DISTINCT FROM OLD.amount OR v_att.asset_code IS DISTINCT FROM OLD.asset_code THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the withdrawal/attempt amount or asset differ from the resolution''s (R-4)' USING ERRCODE = 'MR010';
            END IF;
            SELECT e.verdict, e.line_id, e.reference, e.import_ids INTO v_ev FROM payout_m4_evidence(OLD.tenant_id, OLD.attempt_id) e;
            IF v_ev.verdict IS DISTINCT FROM OLD.evidence_verdict OR v_ev.line_id IS DISTINCT FROM OLD.evidence_line_id
               OR v_ev.reference IS DISTINCT FROM OLD.evidence_reference OR v_ev.import_ids IS DISTINCT FROM OLD.evidence_import_ids THEN
                RAISE EXCEPTION 'payment_manual_resolutions: the M4 evidence changed since submission (S-2)' USING ERRCODE = 'MR061';
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
        -- the wr.id:failed posting of this withdrawal; for M4 the provider:R
        -- completion or the wr.id:failed posting).
        IF OLD.executed_txid IS DISTINCT FROM txid_current() THEN
            RAISE EXCEPTION 'payment_manual_resolutions: executing -> % only in the executing transaction', NEW.state USING ERRCODE = 'MR030';
        END IF;
        IF NEW.refusal_code IS NULL THEN
            RAISE EXCEPTION 'payment_manual_resolutions: refused_at_execution needs a refusal_code' USING ERRCODE = 'MR030';
        END IF;
        IF EXISTS (SELECT 1 FROM ledger_transactions t
                    WHERE t.tenant_id = OLD.tenant_id
                      AND ((OLD.kind = 'm2_declare_paid' AND t.idempotency_key = OLD.provider_id || ':' || OLD.reserved_provider_tx_id)
                        OR (OLD.kind = 'm4_evidence_paid' AND t.idempotency_key = OLD.provider_id || ':' || OLD.evidence_reference)
                        OR (OLD.kind IN ('m2_declare_not_paid', 'm4_evidence_not_paid') AND t.idempotency_key = OLD.withdrawal_request_id::text || ':failed'
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

