-- Reverses migration 0107.
--
-- Fail-closed on the reconciliation kind restore (ADR 0095 §28.8): if any
-- 'pay_captured_unposted' row exists, restoring 0102's narrower CHECK
-- would violate it immediately - Postgres itself refuses the ALTER TABLE
-- ADD CONSTRAINT with a clear error naming the offending rows via the
-- constraint violation. This is deliberate: those rows record a REAL,
-- unposted PSP capture (§28.13's interim (A)) and must never be deleted
-- to make a rollback pass. If this refuses: STOP, escalate to the human,
-- and resolve or reclassify those rows (never delete them) before
-- retrying the rollback.
-- Ledger-finance review L1 (rv-fh3-ledger.md, 076e42e): wrapped in a DO/
-- EXCEPTION block, matching the up.sql migration's own runbook style, so
-- a refusal here gives the SAME kind of actionable, human-readable
-- message as every other fail-closed check in this migration pair,
-- instead of a bare constraint-violation error naming only the offending
-- rows.
ALTER TABLE reconciliation_mismatches DROP CONSTRAINT reconciliation_mismatches_mismatch_kind_check;
DO $$
BEGIN
    ALTER TABLE reconciliation_mismatches ADD CONSTRAINT reconciliation_mismatches_mismatch_kind_check
        CHECK (mismatch_kind IN ('missing_projection', 'balance_mismatch',
            'sb_locked_mismatch', 'sb_bet_net_mismatch', 'sb_orphan_ledger',
            'sb_orphan_history', 'sb_status_mismatch', 'sb_mock_statement_mismatch',
            'cas_round_binding_mismatch', 'cas_posting_shape_mismatch', 'cas_orphan_win',
            'cas_rollback_linkage_mismatch', 'cas_tombstone_conflict',
            'cas_unposted_provider_event', 'cas_tombstone_late_original',
            'cas_mock_statement_mismatch',
            'pay_missing_platform_record', 'pay_missing_provider_record', 'pay_amount_mismatch',
            'pay_asset_mismatch', 'pay_reference_mismatch', 'pay_status_mismatch',
            'pay_duplicate', 'pay_unresolved'));
EXCEPTION WHEN check_violation THEN
    RAISE EXCEPTION 'migration 0107 down: at least one pay_captured_unposted row exists (a REAL, unposted PSP capture, ADR 0095 §28.13 interim (A)); never delete or reclassify these rows to force this rollback through; STOP and escalate to the human to resolve or reclassify them (never delete) before retrying the rollback';
END $$;

-- Restores 0101's payment_attempts_guard() body verbatim (the ONE line
-- this migration changed, reverted). Rolling back leaves any
-- multiple_success_for_intent attempts already written in place
-- (append-only history, CLAUDE.md) - this trigger replace does not
-- re-validate existing rows, so no historical row is rejected by being
-- already present under a terminal_reason this narrower body would no
-- longer accept going forward.
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
            (OLD.state = 'declined'   AND NEW.state = 'disputed')                                OR -- T13t (deposit tombstone) / T14 (payout)
            (OLD.state = 'rejected'   AND NEW.state = 'disputed')                                   -- T15
        ) THEN
            RAISE EXCEPTION 'payment_attempts: transition % -> % is not permitted (id=%)', OLD.state, NEW.state, OLD.id;
        END IF;

        IF NEW.state = 'created' AND OLD.ever_possibly_sent THEN
            RAISE EXCEPTION 'payment_attempts: a move into created requires ever_possibly_sent=false (id=%)', OLD.id;
        END IF;

        IF NEW.state = 'rejected' AND OLD.operation = 'payout' AND (OLD.state <> 'created' OR OLD.ever_possibly_sent) THEN
            RAISE EXCEPTION 'payment_attempts: a payout may reach rejected only from created, never sent (id=%)', OLD.id;
        END IF;

        IF OLD.state = 'ambiguous' AND NEW.state = 'submitting' AND OLD.legacy_backfill THEN
            RAISE EXCEPTION 'payment_attempts: T12 resubmission is forbidden on a legacy_backfill row (id=%)', OLD.id;
        END IF;

        IF OLD.state = 'ambiguous' AND NEW.state = 'submitting' AND OLD.operation = 'deposit'
            AND EXISTS (
                SELECT 1 FROM payment_attempts sib
                WHERE sib.deposit_intent_id = OLD.deposit_intent_id AND sib.state = 'succeeded' AND sib.id <> OLD.id
            )
        THEN
            RAISE EXCEPTION 'payment_attempts: T12 resubmission is forbidden once a sibling attempt of the same intent has succeeded (id=%)', OLD.id;
        END IF;

        IF OLD.state = 'declined' AND NEW.state = 'disputed' AND OLD.operation = 'deposit'
            AND NEW.terminal_reason IS DISTINCT FROM 'reversal_tombstone_precedes_success'
        THEN
            RAISE EXCEPTION 'payment_attempts: a deposit declined->disputed transition (T13t) requires terminal_reason=reversal_tombstone_precedes_success (id=%)', OLD.id;
        END IF;

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

DROP INDEX IF EXISTS ledger_transactions_one_deposit_per_intent;
DROP INDEX IF EXISTS payment_attempts_one_succeeded_deposit_per_intent;
