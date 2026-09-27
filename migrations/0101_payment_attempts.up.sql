-- PRH-I1 step (a): ADR 0095 (docs/decisions/0095-provider-io-transaction-
-- boundary-and-payment-contract.md) §13.1 - the payment_attempts state
-- machine and payment_provider_events receipt table.
--
-- Built to the `ledger-finance` re-verification of ADR 0095
-- (docs/plans/payment-readiness/rv-0095-ledger-reverify.md), which is
-- binding ahead of the architect's own text fix landing on the ADR:
--   - N2: payment_attempts_guard fires BEFORE INSERT as well as BEFORE
--     UPDATE. The only INSERT shapes the DB accepts are T1 (created) and
--     T1+T2/T1p (submitting): never legacy_backfill, never
--     last_evidence_kind='legacy', never ever_possibly_sent, never a
--     ledger link or a provider_reference already set. There is no
--     session-setting escape hatch: the backfill below runs BEFORE the
--     INSERT guard trigger exists, using plain INSERTs, and the INSERT
--     guard is only created afterwards.
--   - N4: every backfilled row sets next_action_at (non-terminal rows),
--     first_submitted_at, last_sent_at, interactive=true, and
--     ever_possibly_sent=true for every payout attempt, explicitly,
--     rather than leaving them NULL/false and relying on the sweeper
--     never running against them.
--   - C6(d): the architect's stated preferred resolution (T13t: a
--     tombstone discovered on a late deposit success moves
--     declined -> disputed, `reversal_tombstone_precedes_success`, P1,
--     no posting, no loop) is implemented directly in the guard's
--     whitelist, so a tombstone-preceding-success can never hit a
--     trigger exception and force a rollback/5xx-redelivery loop for
--     either operation kind.
--
-- This migration builds ONLY the data model and its guard triggers: no
-- application wiring changes in this step (that is PRH-I1 steps b-i).
-- Every invariant this migration is responsible for is load-bearing at
-- the database layer, not by application discipline alone (CLAUDE.md
-- "Financial / ledger rules"):
--   - INV-IO-3 (deterministic external identity): UNIQUE indexes on
--     merchant_reference and external_idempotency_key.
--   - INV-IO-4 (CAS-only transitions): payment_attempts_guard rejects any
--     (OLD.state, NEW.state) pair not in ADR 0095 §4.3 (as fixed by
--     C6(d)), and rejects any INSERT shape or UPDATE to an immutable
--     column outside that list.
--   - INV-IO-7 (no payout failure/success without definite evidence):
--     payment_attempts_guard checks last_evidence_kind on every
--     ->succeeded/->declined transition.
--   - INV-IO-8 (at most one live attempt per intent; exactly one payout
--     attempt per withdrawal): partial unique indexes.
--   - INV-IO-9 (created never proven sent): CHECK plus the guard's T5-only
--     rule for moves back into created.
--   - INV-IO-10 (no verified callback ever lost): payment_provider_events,
--     append-only, UNIQUE on (tenant_id, provider_id, event_fingerprint).
--
-- Depends on migration 0099 (PROVIDER-REF-BOUND-1): every provider
-- reference column below carries the same CHECK shape as 0099's, using
-- the same 255-byte/control-character rule (internal/providerref.MaxBytes,
-- ADR 0095 calls this PROVIDER_REF_MAX). 0099 does not define a reusable
-- SQL macro, so the bound is repeated verbatim per column, exactly as
-- 0099 itself does across tables.
--
-- Migration numbering (ADR 0095 §27, orchestrator re-allocation
-- 2026-09-27): 0100 is ADR 0096's KYC enforcement migration, landing in
-- parallel on a sibling branch and not yet present in this tree. This
-- migration is intentionally numbered 0101 regardless (the ADR fixes the
-- number), so the on-disk chain has a transient gap at 0100 until PRH-I3
-- merges. internal/db.LoadMigrations does not require a real, checked-out
-- migrations/ directory to be gap-free (findVersionGaps is unit-tested
-- against synthetic input only), and this migration's own tests derive
-- "the current tip" from the filesystem rather than hard-coding 101, so
-- they remain correct whether or not 0100 has landed yet in a given
-- checkout.

-- --- payment_attempts ---------------------------------------------------

CREATE TABLE payment_attempts (
    id                        UUID PRIMARY KEY,
    tenant_id                 UUID NOT NULL,
    operation                 TEXT NOT NULL CHECK (operation IN ('deposit', 'payout')),
    deposit_intent_id         UUID NULL REFERENCES deposit_intents (id),
    withdrawal_request_id     UUID NULL REFERENCES withdrawal_requests (id),
    CHECK ((operation = 'deposit') = (deposit_intent_id IS NOT NULL)),
    CHECK ((operation = 'payout') = (withdrawal_request_id IS NOT NULL)),
    attempt_no                INT NOT NULL CHECK (attempt_no >= 1),
    provider_id               TEXT NULL,
    excluded_provider_ids     TEXT[] NOT NULL DEFAULT '{}',
    payment_method            TEXT NOT NULL,
    legacy_backfill           BOOLEAN NOT NULL DEFAULT false,
    CHECK (payment_method <> 'legacy_unknown' OR legacy_backfill),
    asset_code                TEXT NOT NULL CHECK (octet_length(asset_code) BETWEEN 1 AND 16),
    amount                    NUMERIC(38, 0) NOT NULL CHECK (amount > 0),
    interactive               BOOLEAN NOT NULL,
    merchant_reference        TEXT NOT NULL CHECK (octet_length(merchant_reference) BETWEEN 1 AND 64),
    external_idempotency_key  TEXT NOT NULL CHECK (octet_length(external_idempotency_key) BETWEEN 1 AND 128),
    provider_reference        TEXT NULL CHECK (
        provider_reference IS NULL
        OR (octet_length(provider_reference) BETWEEN 1 AND 255 AND provider_reference !~ '[\x01-\x1F\x7F-\x9F]')
    ),
    state                     TEXT NOT NULL CHECK (state IN (
        'created', 'submitting', 'pending', 'ambiguous',
        'succeeded', 'declined', 'rejected', 'disputed'
    )),
    last_evidence_kind        TEXT NOT NULL CHECK (last_evidence_kind IN (
        'sync', 'callback', 'query_status', 'sweeper', 'operator', 'platform', 'legacy'
    )),
    CHECK (last_evidence_kind <> 'legacy' OR legacy_backfill),          -- N2/L1
    ever_possibly_sent        BOOLEAN NOT NULL DEFAULT false,
    CHECK (state <> 'created' OR NOT ever_possibly_sent),                 -- INV-IO-9
    CHECK (state IN ('created', 'rejected') OR provider_id IS NOT NULL),
    CHECK (state <> 'pending' OR provider_reference IS NOT NULL),
    CHECK (state <> 'succeeded' OR provider_reference IS NOT NULL),       -- LF95-C4
    CHECK (operation <> 'deposit' OR state <> 'succeeded' OR ledger_transaction_id IS NOT NULL),
    submit_count              INT NOT NULL DEFAULT 0 CHECK (submit_count >= 0),
    claim_token               UUID NULL,
    lease_owner               TEXT NULL,
    lease_until               TIMESTAMPTZ NULL,
    first_submitted_at        TIMESTAMPTZ NULL,
    last_sent_at              TIMESTAMPTZ NULL,
    next_action_at            TIMESTAMPTZ NULL,
    CHECK (state NOT IN ('succeeded', 'declined', 'rejected', 'disputed') OR next_action_at IS NULL),
    poll_count                INT NOT NULL DEFAULT 0 CHECK (poll_count >= 0),
    escalated_at              TIMESTAMPTZ NULL,
    decline_reason            TEXT NULL CHECK (decline_reason IS NULL OR octet_length(decline_reason) <= 64),
    decline_stage             TEXT NULL CHECK (decline_stage IN ('at_submission', 'after_acceptance')),
    cascadable                BOOLEAN NULL,
    terminal_reason           TEXT NULL CHECK (terminal_reason IS NULL OR octet_length(terminal_reason) <= 64),
    ledger_transaction_id     UUID NULL REFERENCES ledger_transactions (id),
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    accepted_at               TIMESTAMPTZ NULL,
    resolved_at               TIMESTAMPTZ NULL,
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX payment_attempts_tenant_merchant_ref ON payment_attempts (tenant_id, merchant_reference);
CREATE UNIQUE INDEX payment_attempts_tenant_idem_key ON payment_attempts (tenant_id, external_idempotency_key);
CREATE UNIQUE INDEX payment_attempts_tenant_provider_ref ON payment_attempts (tenant_id, provider_id, provider_reference)
    WHERE provider_reference IS NOT NULL;
CREATE UNIQUE INDEX payment_attempts_tenant_ledger_tx ON payment_attempts (tenant_id, ledger_transaction_id)
    WHERE ledger_transaction_id IS NOT NULL;
CREATE UNIQUE INDEX payment_attempts_tenant_intent_attempt_no ON payment_attempts (tenant_id, deposit_intent_id, attempt_no)
    WHERE operation = 'deposit';
-- INV-IO-8.
CREATE UNIQUE INDEX payment_attempts_one_live_per_intent ON payment_attempts (tenant_id, deposit_intent_id)
    WHERE operation = 'deposit' AND state IN ('created', 'submitting', 'pending', 'ambiguous');
CREATE UNIQUE INDEX payment_attempts_one_per_withdrawal ON payment_attempts (tenant_id, withdrawal_request_id)
    WHERE operation = 'payout';
CREATE INDEX payment_attempts_due ON payment_attempts (tenant_id, next_action_at) WHERE next_action_at IS NOT NULL;
CREATE INDEX payment_attempts_tenant ON payment_attempts (tenant_id);

-- payment_attempts_guard: the single CAS/immutability backstop
-- (INV-IO-4), firing on both INSERT (N2) and UPDATE. Every allowed
-- (OLD.state, NEW.state) pair from ADR 0095 §4.3, as fixed by the
-- ledger-finance re-verification's C6(d) resolution, is listed
-- explicitly; anything else, including every entry in the §4.3
-- "Forbidden" list, is rejected. A same-state write (T16 escalate, T17
-- touch, or a lease-only claim) is always allowed at the state-pair
-- level; the immutable-column checks below still apply to it.
CREATE FUNCTION payment_attempts_guard() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        -- N2: the only two INSERT shapes ADR 0095 §4.3 allows are T1
        -- (created) and T1+T2/T1p (submitting). No legacy row, no row
        -- claiming to have already been sent, evidenced or linked, may
        -- ever be inserted directly - the 0101 backfill below runs
        -- BEFORE this trigger exists, so there is no escape hatch for
        -- application code.
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
            (OLD.state = 'declined'   AND NEW.state = 'disputed')                                OR -- T13t (deposit tombstone) / T14 (payout)
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

        -- T13t (RV-0095 ledger C6(d)): the tombstone variant is deposit
        -- only and must carry its named terminal_reason so the M1 queue
        -- can distinguish it from every other declined->disputed cause.
        IF OLD.state = 'declined' AND NEW.state = 'disputed' AND OLD.operation = 'deposit'
            AND NEW.terminal_reason IS DISTINCT FROM 'reversal_tombstone_precedes_success'
        THEN
            RAISE EXCEPTION 'payment_attempts: a deposit declined->disputed transition (T13t) requires terminal_reason=reversal_tombstone_precedes_success (id=%)', OLD.id;
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

-- N2: the guard is created as BEFORE UPDATE only for now, so the 0101
-- backfill below (plain INSERTs, run once, in this same migration
-- transaction) is not subject to the INSERT shape restriction. The
-- BEFORE INSERT half of the same function is attached in a second
-- CREATE TRIGGER statement AFTER the backfill has run, so no
-- session-setting or role-based escape hatch is needed for application
-- code ever again.
CREATE TRIGGER payment_attempts_update_guard
    BEFORE UPDATE ON payment_attempts
    FOR EACH ROW EXECUTE FUNCTION payment_attempts_guard();

CREATE TRIGGER payment_attempts_no_delete
    BEFORE DELETE ON payment_attempts
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();

CREATE TRIGGER payment_attempts_no_truncate
    BEFORE TRUNCATE ON payment_attempts
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

ALTER TABLE payment_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_attempts FORCE ROW LEVEL SECURITY;

-- Staff/system only (the 0025 tenant_staff_scope pattern): players read
-- their deposit_intents/withdrawal_requests projection, never the attempt
-- ledger directly (§13 "no player policy").
CREATE POLICY tenant_staff_scope ON payment_attempts
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- --- payment_provider_events (verified callback receipts, INV-IO-10) ----

CREATE TABLE payment_provider_events (
    id                           UUID PRIMARY KEY,
    tenant_id                    UUID NOT NULL,
    provider_id                  TEXT NOT NULL,
    event_type                   TEXT NOT NULL CHECK (event_type IN ('deposit', 'deposit_reversal', 'payout', 'payout_returned')),
    provider_reference           TEXT NOT NULL CHECK (octet_length(provider_reference) BETWEEN 1 AND 255 AND provider_reference !~ '[\x01-\x1F\x7F-\x9F]'),
    original_provider_reference  TEXT NULL CHECK (
        original_provider_reference IS NULL
        OR (octet_length(original_provider_reference) BETWEEN 1 AND 255 AND original_provider_reference !~ '[\x01-\x1F\x7F-\x9F]')
    ),
    merchant_reference           TEXT NULL CHECK (merchant_reference IS NULL OR octet_length(merchant_reference) <= 64),
    settlement_reference         TEXT NULL CHECK (
        settlement_reference IS NULL
        OR (octet_length(settlement_reference) BETWEEN 1 AND 255 AND settlement_reference !~ '[\x01-\x1F\x7F-\x9F]')
    ),
    outcome                      TEXT NOT NULL CHECK (outcome IN ('pending', 'succeeded', 'declined', 'ambiguous')),
    amount                       NUMERIC(38, 0) NULL,
    asset_code                   TEXT NULL CHECK (asset_code IS NULL OR octet_length(asset_code) <= 16),
    CHECK (outcome <> 'succeeded' OR (amount IS NOT NULL AND asset_code IS NOT NULL)),  -- LF95-C4
    cascadable                   BOOLEAN NULL,
    decline_stage                TEXT NULL CHECK (decline_stage IN ('at_submission', 'after_acceptance')),
    decline_reason               TEXT NULL CHECK (decline_reason IS NULL OR octet_length(decline_reason) <= 64),
    CHECK (outcome <> 'declined' OR (cascadable IS NOT NULL AND decline_stage IS NOT NULL)),
    event_fingerprint            BYTEA NOT NULL,
    disposition_at_receipt       TEXT NOT NULL CHECK (disposition_at_receipt IN (
        'applied', 'duplicate_effect', 'deferred_unresolved', 'anomaly', 'unsupported_event'
    )),
    attempt_id                   UUID NULL REFERENCES payment_attempts (id),
    resolution                   TEXT NULL CHECK (resolution IN (
        'applied', 'anomaly_cross_provider', 'anomaly_reference_conflict', 'anomaly_predates_submission', 'anomaly_other'
    )),
    resolved_at                  TIMESTAMPTZ NULL,
    received_at                  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, provider_id, event_fingerprint)
);

CREATE INDEX payment_provider_events_unresolved ON payment_provider_events (tenant_id, provider_id, provider_reference)
    WHERE resolved_at IS NULL;
CREATE INDEX payment_provider_events_tenant ON payment_provider_events (tenant_id);

-- Append-only except the three one-shot columns (NULL -> value exactly
-- once each); no DELETE (INV-IO-10: a receipt, once durably committed,
-- is never lost).
CREATE FUNCTION payment_provider_events_guard() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id
        OR NEW.tenant_id IS DISTINCT FROM OLD.tenant_id
        OR NEW.provider_id IS DISTINCT FROM OLD.provider_id
        OR NEW.event_type IS DISTINCT FROM OLD.event_type
        OR NEW.provider_reference IS DISTINCT FROM OLD.provider_reference
        OR NEW.original_provider_reference IS DISTINCT FROM OLD.original_provider_reference
        OR NEW.merchant_reference IS DISTINCT FROM OLD.merchant_reference
        OR NEW.settlement_reference IS DISTINCT FROM OLD.settlement_reference
        OR NEW.outcome IS DISTINCT FROM OLD.outcome
        OR NEW.amount IS DISTINCT FROM OLD.amount
        OR NEW.asset_code IS DISTINCT FROM OLD.asset_code
        OR NEW.cascadable IS DISTINCT FROM OLD.cascadable
        OR NEW.decline_stage IS DISTINCT FROM OLD.decline_stage
        OR NEW.decline_reason IS DISTINCT FROM OLD.decline_reason
        OR NEW.event_fingerprint IS DISTINCT FROM OLD.event_fingerprint
        OR NEW.disposition_at_receipt IS DISTINCT FROM OLD.disposition_at_receipt
        OR NEW.received_at IS DISTINCT FROM OLD.received_at
    THEN
        RAISE EXCEPTION 'payment_provider_events: append-only except attempt_id/resolution/resolved_at (one-shot)';
    END IF;
    IF OLD.attempt_id IS NOT NULL AND NEW.attempt_id IS DISTINCT FROM OLD.attempt_id THEN
        RAISE EXCEPTION 'payment_provider_events: attempt_id is one-shot (NULL -> value)';
    END IF;
    IF OLD.resolution IS NOT NULL AND NEW.resolution IS DISTINCT FROM OLD.resolution THEN
        RAISE EXCEPTION 'payment_provider_events: resolution is one-shot (NULL -> value)';
    END IF;
    IF OLD.resolved_at IS NOT NULL AND NEW.resolved_at IS DISTINCT FROM OLD.resolved_at THEN
        RAISE EXCEPTION 'payment_provider_events: resolved_at is one-shot (NULL -> value)';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER payment_provider_events_guard
    BEFORE UPDATE ON payment_provider_events
    FOR EACH ROW EXECUTE FUNCTION payment_provider_events_guard();

CREATE TRIGGER payment_provider_events_no_delete
    BEFORE DELETE ON payment_provider_events
    FOR EACH ROW EXECUTE FUNCTION ledger_deny_mutation();

CREATE TRIGGER payment_provider_events_no_truncate
    BEFORE TRUNCATE ON payment_provider_events
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_deny_mutation();

ALTER TABLE payment_provider_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_provider_events FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_staff_scope ON payment_provider_events
    FOR ALL
    USING (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- --- Backfill (LF95-C11, as fixed by N4; synthetic/dev data only, but --
-- correct regardless) -----------------------------------------------------
--
-- Runs inside this migration's own transaction, using plain INSERTs,
-- BEFORE the payment_attempts_insert_guard trigger (below) exists - so
-- legacy_backfill/last_evidence_kind='legacy' rows, and rows inserted
-- directly into a terminal state, need no escape hatch at all (N2).
-- Writes NO ledger row, and never coerces a value. Pre-flight aborts,
-- listing offending ids, before any write.
-- Migration connections have no app.tenant_id set, and deposit_intents/
-- withdrawal_requests/payment_attempts all carry FORCE ROW LEVEL
-- SECURITY: a plain, unscoped query would see ZERO rows and silently
-- pass every pre-flight check (migration 0048's lesson, restated by
-- 0099's own pre-flight comment). This backfill therefore loops over
-- every tenant and sets app.tenant_id (and clears app.player_account_id,
-- which tenant_staff_scope also requires unset) before touching any of
-- these tables, exactly like 0099's pre-flight - never toggling FORCE,
-- never bypassing RLS.
DO $$
DECLARE
    tenant_rec                       RECORD;
    pf_deposit_ambiguous_no_provider INT;
    pf_deposit_succeeded_missing     INT;
    pf_withdrawal_submitted_no_ref   INT;
    pf_ids                           TEXT;
    live_check                       INT;
    backfill_now                     TIMESTAMPTZ := now();
    grand_total                      INT := 0;
    report                           TEXT := '';
BEGIN
    PERFORM set_config('app.player_account_id', '', true);

    -- Pass 1: pre-flight, across every tenant, before any write.
    FOR tenant_rec IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.tenant_id', tenant_rec.id::text, true);

        -- Pre-flight 1: a non-terminal deposit intent with no
        -- provider_reference AND no provider_id can never be classified
        -- pending/ambiguous with confidence about ever_possibly_sent.
        SELECT count(*) INTO pf_deposit_ambiguous_no_provider
        FROM deposit_intents
        WHERE status IN ('pending', 'ambiguous') AND provider_reference IS NULL AND provider_id IS NULL;
        IF pf_deposit_ambiguous_no_provider > 0 THEN
            SELECT string_agg(id::text, ', ') INTO pf_ids FROM (
                SELECT id FROM deposit_intents
                WHERE status IN ('pending', 'ambiguous') AND provider_reference IS NULL AND provider_id IS NULL
                ORDER BY id LIMIT 20
            ) s;
            report := report || format(' tenant=%s deposit_intents.pending_ambiguous_no_provider=%s (ids: %s)', tenant_rec.id, pf_deposit_ambiguous_no_provider, pf_ids);
            grand_total := grand_total + pf_deposit_ambiguous_no_provider;
        END IF;

        -- Pre-flight 2: a succeeded deposit intent must carry both a
        -- ledger link and a provider reference, or the backfilled
        -- attempt cannot satisfy the succeeded-state CHECKs.
        SELECT count(*) INTO pf_deposit_succeeded_missing
        FROM deposit_intents
        WHERE status = 'succeeded' AND (ledger_transaction_id IS NULL OR provider_reference IS NULL);
        IF pf_deposit_succeeded_missing > 0 THEN
            SELECT string_agg(id::text, ', ') INTO pf_ids FROM (
                SELECT id FROM deposit_intents
                WHERE status = 'succeeded' AND (ledger_transaction_id IS NULL OR provider_reference IS NULL)
                ORDER BY id LIMIT 20
            ) s;
            report := report || format(' tenant=%s deposit_intents.succeeded_missing_link=%s (ids: %s)', tenant_rec.id, pf_deposit_succeeded_missing, pf_ids);
            grand_total := grand_total + pf_deposit_succeeded_missing;
        END IF;

        -- Pre-flight 3: a submitted withdrawal must already carry its
        -- provider_reference (today's application code sets it before
        -- MarkSubmitted commits); otherwise this migration cannot know
        -- whether it was ever sent.
        SELECT count(*) INTO pf_withdrawal_submitted_no_ref
        FROM withdrawal_requests
        WHERE state = 'submitted' AND provider_reference IS NULL;
        IF pf_withdrawal_submitted_no_ref > 0 THEN
            SELECT string_agg(id::text, ', ') INTO pf_ids FROM (
                SELECT id FROM withdrawal_requests WHERE state = 'submitted' AND provider_reference IS NULL ORDER BY id LIMIT 20
            ) s;
            report := report || format(' tenant=%s withdrawal_requests.submitted_no_reference=%s (ids: %s)', tenant_rec.id, pf_withdrawal_submitted_no_ref, pf_ids);
            grand_total := grand_total + pf_withdrawal_submitted_no_ref;
        END IF;
    END LOOP;

    IF grand_total > 0 THEN
        PERFORM set_config('app.tenant_id', '', true);
        RAISE EXCEPTION 'migration 0101 pre-flight: % existing row(s) cannot be backfilled unambiguously:%. Nothing was changed.', grand_total, report;
    END IF;

    -- Pass 2: the backfill itself, per tenant (so both the SELECT source
    -- and the payment_attempts INSERT run inside that tenant's RLS scope).
    FOR tenant_rec IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.tenant_id', tenant_rec.id::text, true);

        -- Backfill: deposit_intents (every row, whatever its status).
        -- next_action_at is set to backfill_now for the two non-terminal
        -- states (pending, ambiguous) so the sweeper picks them up (N4);
        -- first_submitted_at/last_sent_at are set whenever the row was
        -- ever routed to a provider; interactive is true (N4's "unknown
        -- means true" default) so an async decline of a legacy intent
        -- finalizes rather than cascading unattended.
        INSERT INTO payment_attempts (
            id, tenant_id, operation, deposit_intent_id, attempt_no,
            provider_id, payment_method, legacy_backfill, asset_code, amount, interactive,
            merchant_reference, external_idempotency_key, provider_reference,
            state, last_evidence_kind, ever_possibly_sent,
            decline_stage, cascadable,
            first_submitted_at, last_sent_at, next_action_at, submit_count,
            ledger_transaction_id, created_at, accepted_at, resolved_at, updated_at
        )
        SELECT
            di.id, di.tenant_id, 'deposit', di.id, 1,
            di.provider_id, di.payment_method, true, di.asset_code, di.amount, true,
            di.id::text, 'pa:' || di.id::text, di.provider_reference,
            CASE
                WHEN di.status IN ('pending', 'ambiguous') AND di.provider_reference IS NOT NULL THEN di.status
                WHEN di.status IN ('pending', 'ambiguous') AND di.provider_reference IS NULL THEN 'ambiguous'
                WHEN di.status = 'succeeded' THEN 'succeeded'
                WHEN di.status IN ('declined', 'failed') AND di.provider_id IS NOT NULL THEN 'declined'
                WHEN di.status IN ('declined', 'failed') AND di.provider_id IS NULL THEN 'rejected'
            END,
            'legacy',
            (di.provider_id IS NOT NULL),
            CASE WHEN di.status IN ('declined', 'failed') THEN 'after_acceptance' ELSE NULL END,
            CASE WHEN di.status IN ('declined', 'failed') AND di.provider_id IS NOT NULL THEN false ELSE NULL END,
            CASE WHEN di.provider_id IS NOT NULL THEN di.created_at ELSE NULL END,
            CASE WHEN di.provider_id IS NOT NULL THEN backfill_now ELSE NULL END,
            CASE WHEN di.status IN ('pending', 'ambiguous') THEN backfill_now ELSE NULL END,
            CASE WHEN di.provider_id IS NOT NULL THEN 1 ELSE 0 END,
            di.ledger_transaction_id, di.created_at, NULL, NULL, di.updated_at
        FROM deposit_intents di
        WHERE di.tenant_id = tenant_rec.id;

        -- Backfill: withdrawal_requests, only for states that reached
        -- dispatch. Every one of these was, by definition, submitted, so
        -- ever_possibly_sent=true unconditionally (N4).
        INSERT INTO payment_attempts (
            id, tenant_id, operation, withdrawal_request_id, attempt_no,
            provider_id, payment_method, legacy_backfill, asset_code, amount, interactive,
            merchant_reference, external_idempotency_key, provider_reference,
            state, last_evidence_kind, ever_possibly_sent,
            decline_stage,
            first_submitted_at, last_sent_at, next_action_at, submit_count,
            created_at, accepted_at, resolved_at, updated_at
        )
        SELECT
            wr.id, wr.tenant_id, 'payout', wr.id, 1,
            wr.provider_id, 'legacy_unknown', true, wr.asset_code, wr.amount, true,
            wr.id::text, 'pa:' || wr.id::text, wr.provider_reference,
            CASE wr.state
                WHEN 'submitted' THEN 'pending'
                WHEN 'completed' THEN 'succeeded'
                WHEN 'failed' THEN 'declined'
                WHEN 'reversed' THEN 'succeeded'
            END,
            'legacy', true,
            CASE WHEN wr.state = 'failed' THEN 'after_acceptance' ELSE NULL END,
            wr.requested_at, backfill_now,
            CASE WHEN wr.state = 'submitted' THEN backfill_now ELSE NULL END,
            1,
            wr.requested_at, NULL, NULL, wr.updated_at
        FROM withdrawal_requests wr
        WHERE wr.tenant_id = tenant_rec.id AND wr.state IN ('submitted', 'completed', 'failed', 'reversed');
    END LOOP;

    -- Pass 3: post-check (LF95-C11(g)), per tenant. Every non-terminal
    -- deposit intent and every submitted withdrawal now has exactly one
    -- live attempt - this catches a backfill mapping bug that a unique
    -- index alone would not (a missing row, not a duplicate one).
    FOR tenant_rec IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.tenant_id', tenant_rec.id::text, true);

        SELECT count(*) INTO live_check
        FROM deposit_intents di
        WHERE di.status IN ('pending', 'ambiguous')
          AND NOT EXISTS (
            SELECT 1 FROM payment_attempts pa
            WHERE pa.deposit_intent_id = di.id AND pa.state IN ('created', 'submitting', 'pending', 'ambiguous')
          );
        IF live_check > 0 THEN
            RAISE EXCEPTION 'migration 0101 backfill post-check failed: tenant % has % non-terminal deposit_intents row(s) with no live payment_attempts row', tenant_rec.id, live_check;
        END IF;

        SELECT count(*) INTO live_check
        FROM withdrawal_requests wr
        WHERE wr.state = 'submitted'
          AND NOT EXISTS (SELECT 1 FROM payment_attempts pa WHERE pa.withdrawal_request_id = wr.id);
        IF live_check > 0 THEN
            RAISE EXCEPTION 'migration 0101 backfill post-check failed: tenant % has % submitted withdrawal_requests row(s) with no payment_attempts row', tenant_rec.id, live_check;
        END IF;
    END LOOP;

    PERFORM set_config('app.tenant_id', '', true);
END $$;

-- N2: only now, after the backfill has run, is the INSERT half of the
-- guard attached. From this point on, no INSERT of any shape other than
-- T1 (created) or T1+T2/T1p (submitting) is possible for any session,
-- including a future migration.
CREATE TRIGGER payment_attempts_insert_guard
    BEFORE INSERT ON payment_attempts
    FOR EACH ROW EXECUTE FUNCTION payment_attempts_guard();
