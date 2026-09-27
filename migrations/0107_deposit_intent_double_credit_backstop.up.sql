-- ADR 0095 §28 (AM-2, Financial Hardening FH-3, PAY-DOUBLE-CREDIT-1):
-- INV-DEP-1. For every deposit_intents row there is at most ONE
-- payment_attempts row with operation='deposit' in state 'succeeded', and
-- at most ONE ledger_transactions row with transaction_type='deposit' and
-- correlation_id = deposit_intents.id. This migration adds the DATABASE
-- BACKSTOP for both halves; the primary control is the application-level
-- choke point (internal/payments' postDepositSuccess, ledger-finance
-- ruling §3/§28.3) - a verified provider success never, by itself,
-- authorizes a second posting for an already financially resolved intent.
--
-- Three independent, reviewed changes in one migration (ledger-finance
-- owns the ledger half, payments owns the attempts half and the guard
-- function, architect confirmed the reviewed choices in ADR 0095 revision
-- 4 §28 AM-2):
--
--   1. payment_attempts_one_succeeded_deposit_per_intent: a partial unique
--      index enforcing the state-machine half of INV-DEP-1 for every
--      transition source (including a future M1 disputed->succeeded on a
--      resolved intent).
--   2. ledger_transactions_one_deposit_per_intent: a partial unique index
--      enforcing the ledger half. Required because the legacy
--      InitiateDeposit path posts with no attempt row at all, and
--      postDepositSuccess posts to the ledger BEFORE ApplySuccess updates
--      the attempt - only the ledger is authoritative for money.
--   3. payment_attempts_guard() is CREATE OR REPLACE'd (identical to its
--      0101 body except ONE line): a deposit declined->disputed
--      transition (T13t / new T13d) now accepts
--      terminal_reason IN ('reversal_tombstone_precedes_success',
--      'multiple_success_for_intent') instead of only the former. Nothing
--      else in the 0101 body changes.
--
-- Refusal, not a pre-check (the 0092 pattern, ledger-finance ruling §3,
-- confirmed by the architect's AM-2 revision-4 note correcting the
-- ledger-finance ruling's own §3 GROUP BY pre-check): each index build is
-- ITSELF the check, inside a DO/EXCEPTION block. Deliberately NOT a
-- `SELECT ... GROUP BY ... HAVING count(*) > 1` pre-check: both tables
-- carry FORCE ROW LEVEL SECURITY and this migration's connection sets no
-- app.tenant_id, so a SELECT pre-check would see zero rows under RLS and
-- let real duplicates through (the migration 0048/0092 lesson). Building
-- a unique index scans every row unconditionally, regardless of RLS, so
-- it cannot be fooled this way. The ledger-finance ruling's own GROUP BY
-- query is retained below only as the OPERATOR'S diagnostic (run as a
-- role that sees every tenant), never as this migration's own gate.
--
-- If this refuses: STOP. Per CLAUDE.md, ledger rows and attempt history
-- are never deleted - escalate to the human. Synthetic dev/scratch
-- databases produced by pre-§28 T13 "second capture posts" tests are the
-- expected place this refuses; recreate those databases rather than
-- trying to resolve the refusal in place. Never delete rows to make this
-- migration pass.
--
-- Operational note (binding, architect AM-2 revision-4 confirmation):
-- these are NOT `CREATE UNIQUE INDEX CONCURRENTLY` builds (this
-- codebase's migration runner applies every file inside one transaction,
-- and CONCURRENTLY cannot run inside a transaction block). A
-- non-concurrent build inside a transaction blocks writes to the target
-- table for the build's duration - acceptable at current synthetic
-- scale; before any production-size ledger, devops must plan the window
-- or use a separate concurrent-build procedure with the same fail-closed
-- outcome.

-- Diagnostic ONLY (not this migration's gate - see above). An operator
-- who sees this migration refuse can run this manually, as a role that
-- bypasses RLS (e.g. the table owner), to find the offending intent:
--   SELECT tenant_id, correlation_id FROM ledger_transactions
--     WHERE transaction_type = 'deposit' GROUP BY 1, 2 HAVING count(*) > 1;

DO $$
BEGIN
    CREATE UNIQUE INDEX payment_attempts_one_succeeded_deposit_per_intent
        ON payment_attempts (tenant_id, deposit_intent_id)
        WHERE operation = 'deposit' AND state = 'succeeded';
EXCEPTION WHEN unique_violation THEN
    RAISE EXCEPTION 'migration 0107: more than one succeeded deposit attempt exists for a deposit intent; never delete ledger or attempt rows; escalate to the human; synthetic dev/scratch databases produced by pre-ADR-0095-§28 T13 "second capture posts" tests are the expected place this refuses, so recreate them';
END $$;

DO $$
BEGIN
    CREATE UNIQUE INDEX ledger_transactions_one_deposit_per_intent
        ON ledger_transactions (tenant_id, correlation_id)
        WHERE transaction_type = 'deposit';
EXCEPTION WHEN unique_violation THEN
    RAISE EXCEPTION 'migration 0107: more than one deposit posting exists for a deposit intent; never delete ledger or attempt rows; escalate to the human; synthetic dev/scratch databases produced by pre-ADR-0095-§28 T13 "second capture posts" tests are the expected place this refuses, so recreate them';
END $$;

-- CREATE OR REPLACE, verbatim 0101 body except the single T13t/T13d
-- terminal_reason line noted above. This function is also attached to
-- the same BEFORE INSERT/BEFORE UPDATE triggers 0101 created; REPLACE
-- changes its body in place without touching the trigger attachments.
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
        IF OLD.state = 'declined' AND NEW.state = 'disputed' AND OLD.operation = 'deposit'
            AND NEW.terminal_reason NOT IN ('reversal_tombstone_precedes_success', 'multiple_success_for_intent')
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

-- 4. reconciliation_mismatches.mismatch_kind: strict superset of
--    migration 0102's list, plus 'pay_captured_unposted' (ADR 0095 §28.9:
--    "provider captured, platform disputed, not posted").
ALTER TABLE reconciliation_mismatches DROP CONSTRAINT reconciliation_mismatches_mismatch_kind_check;
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
        'pay_duplicate', 'pay_unresolved', 'pay_captured_unposted'));
