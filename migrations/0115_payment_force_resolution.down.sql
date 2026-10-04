-- MUST RUN INSIDE A SINGLE TRANSACTION (db.MigrateDown does this; security
-- O-4). Run by hand under psql autocommit, an MR099 refusal below would leave
-- FORCE ROW LEVEL SECURITY lifted for the owner on the checked tables. By
-- hand, use `psql --single-transaction -v ON_ERROR_STOP=1 -f <this file>`.
--
-- Reverses 0115 (ADR 0101 8.5). Refuses (MR099) while any resolution (or
-- approval) row exists, any mismatch row of the three new kinds exists, or any
-- payment_attempt_reference_evidence row exists (dropping it would destroy
-- money evidence) - so once a force-resolution, a standing finding or a Y
-- evidence row exists, 0115 is effectively irreversible. Otherwise it restores
-- byte-for-byte: the 0107 payment_attempts_guard(), 0113's
-- ledger_governed_fence_allows / ledger_entries_governed_fence /
-- ledger_adjustment_payload_refusal, 0113's ledger_accounts acting_insert
-- policy and 0113's mismatch-kind CHECK; and drops everything else 0115 added.
-- The whole-schema snapshot test (D-10) verifies the restoration.

ALTER TABLE payment_manual_resolutions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE payment_manual_resolution_approvals NO FORCE ROW LEVEL SECURITY;
ALTER TABLE payment_attempt_reference_evidence NO FORCE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM payment_manual_resolutions)
       OR EXISTS (SELECT 1 FROM payment_manual_resolution_approvals)
       OR EXISTS (SELECT 1 FROM payment_attempt_reference_evidence) THEN
        RAISE EXCEPTION '0115 down refused: manual resolution / reference evidence rows exist' USING ERRCODE = 'MR099';
    END IF;
END $$;

ALTER TABLE reconciliation_mismatches NO FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM reconciliation_mismatches
                WHERE mismatch_kind IN ('pay_declared_paid_unconfirmed', 'pay_declared_not_paid_but_paid', 'pay_declared_paid_compensated_but_paid')) THEN
        RAISE EXCEPTION '0115 down refused: M2 standing mismatches exist' USING ERRCODE = 'MR099';
    END IF;
END $$;
ALTER TABLE reconciliation_mismatches FORCE ROW LEVEL SECURITY;

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
        'ledger_unlinked_manual_adjustment'
    ));

-- RC-1 triggers.
DROP TRIGGER ledger_adjustment_approvals_step_b_person_sep ON ledger_adjustment_approvals;
DROP TRIGGER ledger_adjustment_requests_step_b_person_sep ON ledger_adjustment_requests;
DROP FUNCTION ledger_adjustment_approvals_step_b_person_sep();
DROP FUNCTION ledger_adjustment_requests_step_b_person_sep();

-- 0113 bodies, byte-for-byte.
CREATE OR REPLACE FUNCTION ledger_adjustment_payload_refusal(
    p_self uuid, p_tenant uuid, p_wallet uuid, p_player uuid, p_asset text, p_direction text,
    p_amount numeric, p_reason text, p_causation uuid, p_evidence text, p_tenant_status text
) RETURNS text AS $$
DECLARE
    v_rc         ledger_adjustment_reason_codes%ROWTYPE;
    v_wallet     RECORD;
    v_cause_type text;
    v_leg_sum    numeric;
    v_leg_count  int;
    v_executed   numeric;
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
        SELECT t.transaction_type INTO v_cause_type FROM ledger_transactions t WHERE t.id = p_causation AND t.tenant_id = p_tenant;
        IF NOT FOUND THEN
            RETURN 'MA022:causation_not_found';
        END IF;
        -- Causation to deposits, reversals and tombstones is refused for
        -- every code (LF ruling 1; INV-DEP-1).
        IF v_cause_type IN ('deposit', 'deposit_reversal', 'tombstone') THEN
            RETURN 'MA022:causation_type_refused';
        END IF;
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

    -- LF F4 / ruling 2 (PREVENTIVE, no override): every credit is refused
    -- while the player has an open captured-unposted exposure.
    IF p_direction = 'credit_player' AND player_open_payment_exposure(p_tenant, p_player) THEN
        RETURN 'MA020:open_payment_exposure';
    END IF;
    RETURN NULL;
END;
$$ LANGUAGE plpgsql STABLE;

CREATE OR REPLACE FUNCTION ledger_entries_governed_fence() RETURNS TRIGGER AS $$
DECLARE
    v_tx          RECORD;
    v_req         RECORD;
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
        IF EXISTS (SELECT 1 FROM ledger_entries e
                    WHERE e.ledger_transaction_id = NEW.ledger_transaction_id
                      AND (e.direction = NEW.direction
                           OR (SELECT count(*) FROM ledger_entries e2 WHERE e2.ledger_transaction_id = NEW.ledger_transaction_id) >= 2)) THEN
            RAISE EXCEPTION 'ledger_entries_governed_fence: the linked transaction already holds this leg (at most two entries, one per direction)' USING ERRCODE = 'CG030';
        END IF;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION ledger_governed_fence_allows(
    p_tenant uuid, p_type text, p_idempotency_key text, p_correlation uuid, p_provider_id text, p_provider_tx_id text
) RETURNS boolean AS $$
    SELECT p_type = 'manual_adjustment' AND EXISTS (
        SELECT 1 FROM ledger_adjustment_requests r
         WHERE r.tenant_id = p_tenant
           AND p_idempotency_key = 'manual_adjustment:' || r.id::text
           AND p_correlation = r.id
           AND r.state = 'executing' AND r.executed_txid = txid_current());
$$ LANGUAGE sql STABLE;

DROP POLICY acting_insert ON ledger_accounts;
CREATE POLICY acting_insert ON ledger_accounts FOR INSERT
    WITH CHECK (tenant_id = NULLIF(current_setting('app.acting_tenant_id', true), '')::uuid
                AND account_type IN ('player_cash', 'manual_adjustment')
                AND (SELECT financial_acting_session_valid()));

-- Acting policies on the payment tables.
DROP POLICY acting_update ON payment_attempts;
DROP POLICY acting_read ON withdrawal_requests;
DROP POLICY acting_update ON withdrawal_requests;
DROP POLICY acting_update ON deposit_intents;

-- Column discipline.
DROP TRIGGER payment_attempts_operator_column_discipline ON payment_attempts;
DROP FUNCTION payment_attempts_operator_column_discipline();

-- The 0107 payment_attempts_guard(), verbatim.
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
        IF NEW.state = 'succeeded' AND NEW.last_evidence_kind NOT IN ('sync', 'callback', 'query_status') THEN
            RAISE EXCEPTION 'payment_attempts: ->succeeded requires last_evidence_kind in (sync, callback, query_status), got % (id=%)', NEW.last_evidence_kind, OLD.id;
        END IF;
        IF NEW.state = 'declined' AND OLD.operation = 'payout' AND NEW.last_evidence_kind NOT IN ('sync', 'callback', 'query_status') THEN
            RAISE EXCEPTION 'payment_attempts: a payout ->declined requires last_evidence_kind in (sync, callback, query_status), got % (id=%)', NEW.last_evidence_kind, OLD.id;
        END IF;
        IF NEW.state = 'declined' AND OLD.operation = 'deposit' AND NEW.last_evidence_kind = 'operator' THEN
            RAISE EXCEPTION 'payment_attempts: a deposit ->declined may never carry last_evidence_kind=operator (id=%)', OLD.id;
        END IF;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- The reserved namespace.
DROP TRIGGER ledger_transactions_reserved_prefix_guard ON ledger_transactions;
DROP FUNCTION ledger_transactions_reserved_prefix_guard();
ALTER TABLE payment_attempts DROP CONSTRAINT payment_attempts_provider_reference_no_reserved_prefix;
ALTER TABLE payment_provider_events
    DROP CONSTRAINT payment_provider_events_provider_reference_no_reserved_prefix,
    DROP CONSTRAINT payment_provider_events_original_provider_reference_no_reserved_prefix,
    DROP CONSTRAINT payment_provider_events_settlement_reference_no_reserved_prefix;
ALTER TABLE payment_statement_lines
    DROP CONSTRAINT payment_statement_lines_provider_reference_no_reserved_prefix,
    DROP CONSTRAINT payment_statement_lines_original_provider_reference_no_reserved_prefix,
    DROP CONSTRAINT payment_statement_lines_settlement_reference_no_reserved_prefix;
ALTER TABLE deposit_intents DROP CONSTRAINT deposit_intents_provider_reference_no_reserved_prefix;
ALTER TABLE withdrawal_requests DROP CONSTRAINT withdrawal_requests_provider_reference_no_reserved_prefix;

-- The folded indexes and evidence table.
DROP INDEX payment_statement_lines_ref;
DROP INDEX payment_statement_lines_merchant;
DROP INDEX payment_statement_lines_reversal_original;
DROP TABLE payment_attempt_reference_evidence;
DROP FUNCTION payment_attempt_reference_evidence_bound_to_park();
DROP FUNCTION payment_attempt_reference_evidence_guard();

-- Resolution tables (approvals first) and their functions.
DROP TABLE payment_manual_resolution_approvals;
DROP FUNCTION payment_manual_resolution_approvals_apply_reject();
DROP FUNCTION payment_manual_resolution_approvals_guard();
DROP FUNCTION payment_manual_resolution_execution_status(uuid);
DROP TABLE payment_manual_resolutions;
DROP FUNCTION payment_manual_resolutions_no_executing_commit();
DROP FUNCTION payment_manual_resolutions_beneficiary_guard();
DROP FUNCTION payment_manual_resolutions_guard();
DROP FUNCTION payment_m2_admits(uuid, text, text, uuid, text, text);

-- Prerequisite, reference data, prefix function.
ALTER TABLE payment_attempts DROP CONSTRAINT payment_attempts_id_tenant_key;
DROP TABLE payment_manual_resolution_codes;
DROP FUNCTION payment_reserved_ref_prefix();
