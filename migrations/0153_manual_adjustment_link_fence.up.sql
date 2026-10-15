-- MANUAL-ADJ-LINK-1 (= LEDGER-MANUAL-ADJ-LINK-1; ADR 0100 amendment
-- "Section 21: the session-independent manual_adjustment link fence";
-- security K2-2; ledger-finance ruling 4).
--
-- GAP CLOSED. Migration 0113's ledger_transactions_governed_fence /
-- ledger_entries_governed_fence fire ONLY when an acting GUC is present
-- (financial_acting_gucs_present()). A session WITHOUT acting GUCs - the
-- runtime role with no GUCs at all, a plain tenant/system session, or the
-- table owner - could INSERT a ledger_transactions row of type
-- 'manual_adjustment' (and its entries) with no governed request behind it.
-- Until now only a Go static single-caller test (B-21) and the DETECTIVE
-- reconciliation kind ledger_unlinked_manual_adjustment guarded that.
--
-- WHAT THIS ADDS (preventive, for EVERY session, GUC-independent):
--   1. ledger_transactions_manual_adjustment_link - a NOT DEFERRABLE
--      constraint trigger, AFTER INSERT, WHEN transaction_type =
--      'manual_adjustment'. The new row must be the posting of a governed
--      ledger_adjustment_requests row that
--        (a) is visible to the posting session, same tenant, id =
--            correlation_id, idempotency_key = 'manual_adjustment:' || id;
--        (b) is in state 'executing' with executed_txid = txid_current()
--            (i.e. it entered 'executing' in THIS transaction, which the
--            0113 request guard allows only in the transaction of a
--            same-transaction final APPROVE decision - itself bound to a
--            signed actor proof by 0120 - after re-verifying the count);
--        (c) still carries an intact payload_hash;
--        (d) is still counted as satisfied: ledger_adjustment_execution_
--            status() (the ONE 0113 implementation of the four-eyes /
--            threshold count) reports enabled, initiator_valid and
--            counted >= required, evaluated again here - AS OF now(),
--            exactly like the 0113 guard (policies effective_from <= now(),
--            grants in force at now()): a policy tightening or grant revocation
--            committed while the decision is in flight is NOT caught (same
--            semantics as 0113's "tightened after submission"); only
--            non-time-versioned inputs can differ between the two evaluations,
--            and any divergence fails closed (MA050, the decision rolls back);
--        (e) matches the row's keys: reason_code and causation_id equal
--            the request's, provider_id / provider_tx_id /
--            reverses_transaction_id NULL.
--      Refusal: SQLSTATE MA050.
--   2. ledger_entries_manual_adjustment_link - a NOT DEFERRABLE constraint
--      trigger, AFTER INSERT on ledger_entries. For an entry whose parent
--      (looked up in the entry's own tenant) is a manual_adjustment: the
--      parent's request must be 'executing' in THIS transaction (so no
--      entry can ever be appended to a committed manual_adjustment), and
--      the entry must be one leg of the request's closed ADR 0100 section
--      4 shape - the request wallet's player_cash in the player direction,
--      or the tenant's manual_adjustment house account (wallet_id NULL) in
--      the opposite direction, in the request's asset and amount - with at
--      most one leg per direction. A parent the session cannot see in the
--      entry's tenant fails closed (the parent's type is unknowable).
--      Refusal: SQLSTATE MA051.
--   3. In the same entry trigger: an entry whose parent is NOT a
--      manual_adjustment may not post to an account of type
--      'manual_adjustment' (security A4 F1: that account carries governed
--      adjustments only; no legitimate other type posts to it - the full
--      integration suites were the evidence). Refusal: SQLSTATE MA052.
--
-- WHY AFTER (constraint) TRIGGERS, NOT BEFORE. An AFTER ROW trigger fires
-- only for a row that was actually inserted. A replay of an already-posted
-- key (ledger.Post's db.IdempotentInsert savepoint) fails on the unique
-- (tenant_id, idempotency_key) index first and never reaches the fence, so
-- the ADR 0020 idempotency contract (AlreadyPosted / ErrIdempotencyKeyReused /
-- ErrIdempotencyPayloadMismatch) is unchanged. NOT DEFERRABLE: the check runs
-- at the end of the INSERT statement and SET CONSTRAINTS cannot postpone it.
--
-- WHY NO SECURITY DEFINER. The checks run with the caller's rights and RLS.
-- A session that cannot see the request (no GUCs, the system-tenant shape,
-- a foreign tenant) is refused - fail closed. A definer would add nothing
-- (FORCE ROW LEVEL SECURITY binds the owner too) and would widen the attack
-- surface. Every function pins search_path (ADR 0108) and schema-qualifies
-- its relations.
--
-- LOCK ORDER (ADR 0082). The fence takes NO lock: it only reads rows the
-- executor already holds (the request row, L1 FOR UPDATE) or rows that are
-- immutable (ledger rows, 0082). The A8 order request -> staff -> grants ->
-- causation -> projections -> ledger.Post is unchanged.
--
-- WHAT THIS DOES NOT CHANGE. The acting fences (CG030) still fire first for
-- acting sessions and keep their K3/HSEC/M4 branches. The MA020 open-payment-
-- exposure rule and its M2 Step B exemption (0115) live in
-- ledger_adjustment_payload_refusal(), which the 0113 '-> executing' guard
-- runs in the same transaction; this fence does not re-implement it. The
-- detective reconciliation kind ledger_unlinked_manual_adjustment is kept.
--
-- OUT OF SCOPE (ADR 0100 section 21.5 residuals): the table owner (and a
-- superuser) can ALTER TABLE ... DISABLE TRIGGER - the existing ADR 0110
-- section 9.4 owner residual, also used by integration-only fixtures; it is
-- not a new exemption and this migration adds no owner/GUC bypass. Only a
-- superuser (or a role granted SET on it) can use session_replication_role;
-- the non-superuser owner gets 42501 (PG16). Ordinary non-governed posting
-- types (ADR 0110 T5) are not fenced here beyond item 3. A session
-- holding a genuine, proof-backed final approval in its own transaction is
-- the governed path itself.
--
-- SQLSTATEs (class MA, 0113's space): MA050 transaction fence, MA051 entry
-- fence, MA052 manual_adjustment account used by another transaction type.

CREATE OR REPLACE FUNCTION ledger_manual_adjustment_link_check() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_req  public.ledger_adjustment_requests%ROWTYPE;
    v_exec RECORD;
BEGIN
    -- (a) the governed request this posting claims, as THIS session sees it.
    SELECT r.* INTO v_req
      FROM public.ledger_adjustment_requests r
     WHERE r.id = NEW.correlation_id AND r.tenant_id = NEW.tenant_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ledger_transactions: manual_adjustment % is not linked to a governed adjustment request visible to this session', NEW.id
            USING ERRCODE = 'MA050';
    END IF;
    IF NEW.idempotency_key IS DISTINCT FROM 'manual_adjustment:' || v_req.id::text THEN
        RAISE EXCEPTION 'ledger_transactions: manual_adjustment % does not carry its request''s governed key', NEW.id
            USING ERRCODE = 'MA050';
    END IF;
    -- (b) executing, entered in THIS transaction.
    IF v_req.state <> 'executing' OR v_req.executed_txid IS DISTINCT FROM pg_catalog.txid_current() THEN
        RAISE EXCEPTION 'ledger_transactions: request % is not executing in this transaction (state %)', v_req.id, v_req.state
            USING ERRCODE = 'MA050';
    END IF;
    -- (c) the approved payload is intact.
    IF v_req.payload_hash IS DISTINCT FROM public.ledger_adjustment_payload_hash(v_req) THEN
        RAISE EXCEPTION 'ledger_transactions: request % payload_hash does not match its payload', v_req.id
            USING ERRCODE = 'MA050';
    END IF;
    -- (e) the posting's keys are the request's.
    IF NEW.provider_id IS NOT NULL OR NEW.provider_tx_id IS NOT NULL OR NEW.reverses_transaction_id IS NOT NULL
       OR NEW.reason_code IS DISTINCT FROM v_req.reason_code
       OR NEW.causation_id IS DISTINCT FROM v_req.causation_transaction_id THEN
        RAISE EXCEPTION 'ledger_transactions: manual_adjustment % keys do not match request %', NEW.id, v_req.id
            USING ERRCODE = 'MA050';
    END IF;
    -- (d) the four-eyes / threshold count still holds (the one 0113 implementation).
    SELECT * INTO v_exec FROM public.ledger_adjustment_execution_status(v_req.id);
    IF NOT v_exec.enabled OR NOT v_exec.initiator_valid OR v_exec.counted < v_exec.required THEN
        RAISE EXCEPTION 'ledger_transactions: request % is not approved (counted %, required %)', v_req.id, v_exec.counted, v_exec.required
            USING ERRCODE = 'MA050';
    END IF;
    RETURN NULL;
END;
$$;

CREATE OR REPLACE FUNCTION ledger_manual_adjustment_entry_check() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_tx          RECORD;
    v_req         RECORD;
    v_acct        RECORD;
    v_player_dir  text;
    v_house_dir   text;
BEGIN
    SELECT t.transaction_type, t.idempotency_key, t.correlation_id INTO v_tx
      FROM public.ledger_transactions t
     WHERE t.id = NEW.ledger_transaction_id AND t.tenant_id = NEW.tenant_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ledger_entries: parent transaction % is not visible in the entry''s tenant', NEW.ledger_transaction_id
            USING ERRCODE = 'MA051';
    END IF;
    IF v_tx.transaction_type <> 'manual_adjustment' THEN
        -- (3) The manual_adjustment account type belongs to governed adjustments only: no other transaction
        -- type may post to it (security A4 F1; ADR 0110 T5 narrowed for this account).
        IF EXISTS (SELECT 1 FROM public.ledger_accounts la
                    WHERE la.id = NEW.ledger_account_id AND la.account_type = 'manual_adjustment') THEN
            RAISE EXCEPTION 'ledger_entries: a % transaction may not post to a manual_adjustment account', v_tx.transaction_type
                USING ERRCODE = 'MA052';
        END IF;
        RETURN NULL;
    END IF;

    SELECT r.wallet_id, r.asset_code, r.amount, r.direction INTO v_req
      FROM public.ledger_adjustment_requests r
     WHERE r.id = v_tx.correlation_id AND r.tenant_id = NEW.tenant_id
       AND v_tx.idempotency_key = 'manual_adjustment:' || r.id::text
       AND r.state = 'executing' AND r.executed_txid = pg_catalog.txid_current();
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ledger_entries: manual_adjustment % has no request executing in this transaction', NEW.ledger_transaction_id
            USING ERRCODE = 'MA051';
    END IF;

    v_player_dir := CASE WHEN v_req.direction = 'credit_player' THEN 'credit' ELSE 'debit' END;
    v_house_dir  := CASE WHEN v_req.direction = 'credit_player' THEN 'debit' ELSE 'credit' END;
    SELECT la.account_type, la.wallet_id INTO v_acct
      FROM public.ledger_accounts la
     WHERE la.id = NEW.ledger_account_id AND la.tenant_id = NEW.tenant_id;
    IF NOT FOUND
       OR NEW.asset_code IS DISTINCT FROM v_req.asset_code
       OR NEW.amount IS DISTINCT FROM v_req.amount
       OR NOT COALESCE((v_acct.account_type = 'player_cash' AND v_acct.wallet_id = v_req.wallet_id AND NEW.direction = v_player_dir)
                    OR (v_acct.account_type = 'manual_adjustment' AND v_acct.wallet_id IS NULL AND NEW.direction = v_house_dir), false) THEN
        RAISE EXCEPTION 'ledger_entries: entry is not a leg of the approved manual_adjustment shape (account, asset, amount or direction)'
            USING ERRCODE = 'MA051';
    END IF;
    -- At most one leg per direction (with debit/credit only, at most two
    -- entries). The AFTER trigger sees every row of the statement.
    IF (SELECT count(*) FROM public.ledger_entries e
         WHERE e.ledger_transaction_id = NEW.ledger_transaction_id AND e.direction = NEW.direction) > 1 THEN
        RAISE EXCEPTION 'ledger_entries: manual_adjustment % already holds a % leg', NEW.ledger_transaction_id, NEW.direction
            USING ERRCODE = 'MA051';
    END IF;
    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS ledger_transactions_manual_adjustment_link ON ledger_transactions;
CREATE CONSTRAINT TRIGGER ledger_transactions_manual_adjustment_link
    AFTER INSERT ON ledger_transactions
    NOT DEFERRABLE INITIALLY IMMEDIATE
    FOR EACH ROW
    WHEN (NEW.transaction_type = 'manual_adjustment')
    EXECUTE FUNCTION ledger_manual_adjustment_link_check();

DROP TRIGGER IF EXISTS ledger_entries_manual_adjustment_link ON ledger_entries;
CREATE CONSTRAINT TRIGGER ledger_entries_manual_adjustment_link
    AFTER INSERT ON ledger_entries
    NOT DEFERRABLE INITIALLY IMMEDIATE
    FOR EACH ROW
    EXECUTE FUNCTION ledger_manual_adjustment_entry_check();
