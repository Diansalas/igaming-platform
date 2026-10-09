-- MUST RUN INSIDE A SINGLE TRANSACTION (db.MigrateDown does this). By hand, use
-- `psql --single-transaction -v ON_ERROR_STOP=1 -f <this file>`: an MR099 refusal
-- below would otherwise leave FORCE ROW LEVEL SECURITY lifted on the checked tables.
--
-- Reverses 0125 (ADR 0111 section 4, PAY-PAYOUT-UNBOUND-RESOLVE-1). REFUSES (MR099)
-- while any M4 resolution row exists or any statement import carries a seal: once
-- either exists 0125 is effectively irreversible (evidence and its integrity would
-- be discarded). Otherwise it restores BYTE FOR BYTE the 0124 bodies of the four
-- shared objects (ledger_governed_fence_allows, ledger_entries_governed_fence, the
-- acting ledger_accounts INSERT policy, the acting withdrawal_requests UPDATE
-- policy), the 0115 payment_manual_resolutions_guard /
-- payment_manual_resolution_execution_status / payment_manual_resolutions_no_executing_commit
-- and tenant_system_read_executed, the 0120 actor_proof_payment_manual_resolutions_guard
-- and the 0102 statement-table policies, and drops everything else 0125 added.
-- financial_policy_required_approvals and actor_proof_require are not touched by
-- 0125 and keep their 0124 bodies. The whole-schema snapshot test verifies this.

ALTER TABLE payment_manual_resolutions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE payment_statement_imports NO FORCE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM payment_manual_resolutions WHERE kind IN ('m4_evidence_paid', 'm4_evidence_not_paid'))
       OR EXISTS (SELECT 1 FROM payment_statement_imports WHERE import_seal IS NOT NULL) THEN
        RAISE EXCEPTION '0125 down refused: M4 resolution rows or sealed statement imports exist' USING ERRCODE = 'MR099';
    END IF;
END $$;

ALTER TABLE payment_manual_resolutions FORCE ROW LEVEL SECURITY;
ALTER TABLE payment_statement_imports FORCE ROW LEVEL SECURITY;

-- 10. tenant_system_read_executed back to 0115.
DROP POLICY tenant_system_read_executed ON payment_manual_resolutions;
CREATE POLICY tenant_system_read_executed ON payment_manual_resolutions FOR SELECT
    USING (tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
           AND state = 'executed'
           AND kind IN ('m2_declare_paid', 'm2_declare_not_paid')
           AND NULLIF(current_setting('app.principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());

-- 9. The four shared objects back to their 0124 bodies.
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

-- 8. The 0120 K3 proof guard.
CREATE OR REPLACE FUNCTION actor_proof_payment_manual_resolutions_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor RECORD;
BEGIN
    IF TG_OP = 'INSERT' THEN
        SELECT * INTO v_actor FROM financial_actor_session();
        IF NEW.requested_by IS DISTINCT FROM v_actor.actor THEN
            RAISE EXCEPTION 'actor_proof: the requested_by column is not the proven actor' USING ERRCODE = 'AP004';
        END IF;
        -- The id is server-forced by the 0115 guard, so the target is the
        -- literal 'new'; the digest binds the caller-supplied payload columns.
        PERFORM actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant,
            'payment_force_resolve:request', 'new',
            k2_sha256_hex(k2_canonical(NEW.tenant_id::text, NEW.attempt_id::text, NEW.kind, NEW.finding_code,
                NEW.basis_code, NEW.context_code, NEW.evidence_ref_hash, NEW.reason_code)));
    ELSIF OLD.state = 'pending' AND NEW.state = 'cancelled' THEN
        SELECT * INTO v_actor FROM financial_actor_session();
        IF v_actor.actor IS DISTINCT FROM OLD.requested_by THEN
            RAISE EXCEPTION 'actor_proof: only the requester may cancel' USING ERRCODE = 'AP004';
        END IF;
        PERFORM actor_proof_require(v_actor.actor, v_actor.scope, v_actor.tenant,
            'payment_force_resolve:cancel', OLD.id::text, OLD.payload_hash);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

-- 7. MR041 back to 0115.
CREATE OR REPLACE FUNCTION payment_manual_resolutions_no_executing_commit() RETURNS TRIGGER AS $$
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

-- 6. The resolution guard back to 0115.
CREATE OR REPLACE FUNCTION payment_manual_resolutions_guard() RETURNS TRIGGER AS $$
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

-- 5. The counting function back to 0115 (OUT list differs: DROP + CREATE).
DROP FUNCTION payment_manual_resolution_execution_status(uuid);
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

-- 4. The M4 functions.
DROP FUNCTION payout_m4_evidence(uuid, uuid);
DROP FUNCTION payment_m4_scope_visible(uuid);
DROP FUNCTION payment_m4_in_scope(text, text, text, text);

-- 3. Statement-table policies back to 0102 (and no acting read).
DROP POLICY acting_read ON payment_attempt_reference_evidence;
DROP POLICY acting_read ON payment_statement_lines;
DROP POLICY system_insert ON payment_statement_lines;
DROP POLICY tenant_staff_scope ON payment_statement_lines;
DROP POLICY acting_read ON payment_statement_imports;
DROP POLICY system_insert ON payment_statement_imports;
DROP POLICY tenant_staff_scope ON payment_statement_imports;
CREATE POLICY tenant_staff_scope ON payment_statement_imports
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

ALTER TABLE payment_statement_lines ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_statement_lines FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_staff_scope ON payment_statement_lines
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- 2. Import seal columns.
ALTER TABLE payment_statement_imports
    DROP CONSTRAINT payment_statement_imports_sealed_service_check,
    DROP CONSTRAINT payment_statement_imports_imported_by_service_check,
    DROP CONSTRAINT payment_statement_imports_seal_pair_check,
    DROP CONSTRAINT payment_statement_imports_seal_kid_check,
    DROP CONSTRAINT payment_statement_imports_import_seal_check;
ALTER TABLE payment_statement_imports
    DROP COLUMN imported_by_service,
    DROP COLUMN payout_lines_carry_merchant_reference,
    DROP COLUMN seal_kid,
    DROP COLUMN import_seal;

-- 1. Resolution kinds and evidence columns.
DROP INDEX payment_manual_resolutions_evidence_line;
ALTER TABLE payment_manual_resolutions
    DROP CONSTRAINT payment_manual_resolutions_non_m4_no_evidence_check,
    DROP CONSTRAINT payment_manual_resolutions_m4_reference_check,
    DROP CONSTRAINT payment_manual_resolutions_m4_shape_check,
    DROP CONSTRAINT payment_manual_resolutions_evidence_verdict_check,
    DROP CONSTRAINT payment_manual_resolutions_pinned_ref_no_reserved_prefix,
    DROP CONSTRAINT payment_manual_resolutions_evidence_reference_check,
    DROP CONSTRAINT payment_manual_resolutions_evidence_line_fk;
ALTER TABLE payment_manual_resolutions
    DROP COLUMN provider_reference_at_submission,
    DROP COLUMN evidence_import_ids,
    DROP COLUMN evidence_verdict,
    DROP COLUMN evidence_reference,
    DROP COLUMN evidence_line_id;
ALTER TABLE payment_manual_resolutions DROP CONSTRAINT payment_manual_resolutions_kind_check;
ALTER TABLE payment_manual_resolutions ADD CONSTRAINT payment_manual_resolutions_kind_check
    CHECK (kind IN ('m1_deposit_evidence', 'm2_declare_paid', 'm2_declare_not_paid'));

-- 0. The composite FK target.
ALTER TABLE payment_statement_lines DROP CONSTRAINT payment_statement_lines_id_tenant_key;
