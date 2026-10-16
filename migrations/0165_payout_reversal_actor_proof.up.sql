-- ADR 0110 T8 amendment (owner decision 2026-10-16): payout-reversal decisions are bound to the signed actor
-- proof. Closes A5 R1 / SR5-2 / LF-A5-2 (docs/plans/a5-payout-reversal-record.md s11.3 + s12.3).
-- MONEY-PATH TRIGGERS: FOR `ledger-finance`, `security`, `code-reviewer`, `qa` REVIEW BEFORE MERGE.
--
-- NUMBERING: 0165 (next free; 0159 and 0161 stay reserved and unused).
--
-- What changes:
--   1. withdrawal_approvals.actor_proof_nonce (NULL): the nonce of the actor proof a 'reverse' decision
--      consumed. Written ONLY by the proof guard below (any caller-supplied value is overwritten on a
--      'reverse' row and refused on any other row); shape-checked; UNIQUE when present.
--   2. zz_actor_proof_guard: a BEFORE INSERT trigger on withdrawal_approvals, the LAST BEFORE ROW trigger on
--      the table (catalog-pinned). For decision = 'reverse' it calls actor_proof_require(actor, 'tenant',
--      tenant, 'withdrawal_payout_reversal:decide', <withdrawal request id>, <the 64-hex payout digest that
--      is the reason_code after 'payout_reversal:'>), exactly the binding, freshness (60 s cap, 5 s skew),
--      replay (nonce UNIQUE) and key-id rules of every other proof-bound operation (ADR 0110 s4). The actor is
--      the one financial_actor_session() resolves (a tenant staff session of the row's tenant only); the
--      approver column must equal it. A 'reverse' row is never automated. approve / reject rows (ADR 0113
--      s9.3, WD023) are NOT bound by this migration and pass through unchanged.
--   3. WD026 (E11, the state guard) and WD027 (the tombstone branch) count ONLY proof-bound decisions
--      (actor_proof_nonce IS NOT NULL). 'reverse' rows recorded before this migration keep a NULL nonce and
--      no longer count anywhere (they stay as historical evidence; nothing is edited).
--
-- Not changed: actor_proof_require (the trigger pins scope 'tenant'; the Go signer refuses any other scope
-- for this operation), the decision CHECKs and uniqueness of 0162, check A, WD035 / WD044, the 0034/0162
-- governance trigger. Not closed by this migration: ADR 0110 T5 / ADR 0113 E10 (the tombstone branch's
-- hold release rides the ordinary fail path) and ADR 0113 s9.3 (approve / reject records).
--
-- SQLSTATEs: AP001-AP005 (the verifier, ADR 0110 s4); AP004 also for a non-tenant session or an approver
-- column that is not the proven actor; CG001 an unresolvable session (financial_actor_session); 23514 a
-- 'reverse' row without the payout digest shape (raised by the guard itself, same code as the 0162 CHECK);
-- WD028 an automated 'reverse' row, or a proof nonce on a non-'reverse' row; WD096 down refused while
-- proof-bound decisions exist.
--
-- Replaced IN PLACE (the 0162 bodies plus ONE marked line each; the down restores the 0162 bodies byte for
-- byte - whole-schema snapshot test internal/withdrawal/t8_migration_0165_integration_test.go):
--   withdrawal_requests_state_transition_guard()   WD026 counts proof-bound decisions only
--   ledger_transactions_payout_tombstone_guard()   WD027 counts proof-bound decisions only
--
-- No role is exempt; the guard is not SECURITY DEFINER (only actor_proof_require is, unchanged); every
-- function pins search_path. No new lock.

-- =========================================================================
-- 1. The proof nonce column.
-- =========================================================================

ALTER TABLE withdrawal_approvals ADD COLUMN actor_proof_nonce TEXT NULL;
ALTER TABLE withdrawal_approvals ADD CONSTRAINT withdrawal_approvals_actor_proof_nonce_shape
    CHECK (actor_proof_nonce IS NULL OR (decision = 'reverse' AND actor_proof_nonce ~ '^[A-Za-z0-9_-]{16,64}$'));
CREATE UNIQUE INDEX withdrawal_approvals_actor_proof_nonce_key
    ON withdrawal_approvals (actor_proof_nonce) WHERE actor_proof_nonce IS NOT NULL;

-- =========================================================================
-- 2. The proof guard (LAST BEFORE ROW trigger: 'zz_' sorts after every withdrawal_approvals_* trigger).
-- =========================================================================

CREATE FUNCTION actor_proof_withdrawal_approvals_guard() RETURNS TRIGGER AS $$
DECLARE
    v_actor RECORD;
BEGIN
    IF NEW.decision IS DISTINCT FROM 'reverse' THEN
        -- approve / reject (ADR 0113 s9.3) are outside the T8 amendment: unchanged, never proof-marked.
        IF NEW.actor_proof_nonce IS NOT NULL THEN
            RAISE EXCEPTION 'withdrawal_approvals: only a proof-bound reverse decision carries an actor proof nonce'
                USING ERRCODE = 'WD028';
        END IF;
        RETURN NEW;
    END IF;
    IF NEW.reason_code IS NULL OR NEW.reason_code !~ '^payout_reversal:[0-9a-f]{64}$' THEN
        RAISE EXCEPTION 'withdrawal_approvals: a reverse decision names its payout digest (payout_reversal:<sha256 hex>)'
            USING ERRCODE = '23514';
    END IF;
    IF NEW.is_automated_approval THEN
        RAISE EXCEPTION 'withdrawal_approvals: a reverse decision is a proven human decision, never automated'
            USING ERRCODE = 'WD028';
    END IF;
    SELECT * INTO v_actor FROM financial_actor_session();
    IF v_actor.scope IS DISTINCT FROM 'tenant' OR v_actor.tenant IS DISTINCT FROM NEW.tenant_id THEN
        RAISE EXCEPTION 'actor_proof: a reverse decision is provable only in a tenant staff session of its tenant'
            USING ERRCODE = 'AP004';
    END IF;
    IF NEW.approver_principal_id IS DISTINCT FROM v_actor.actor THEN
        RAISE EXCEPTION 'actor_proof: the approver column is not the proven actor' USING ERRCODE = 'AP004';
    END IF;
    PERFORM actor_proof_require(v_actor.actor, 'tenant', v_actor.tenant, 'withdrawal_payout_reversal:decide',
        NEW.withdrawal_request_id::text, pg_catalog.substr(NEW.reason_code, 17));
    -- The proof just verified and consumed is the one in the GUC (same statement): record its nonce, so
    -- the counting rules can tell a proof-bound decision from an unproven one.
    NEW.actor_proof_nonce := pg_catalog.split_part(pg_catalog.current_setting('app.actor_proof', true), '|', 11);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER zz_actor_proof_guard
    BEFORE INSERT ON withdrawal_approvals
    FOR EACH ROW EXECUTE FUNCTION actor_proof_withdrawal_approvals_guard();

-- =========================================================================
-- 3. The state-transition guard (0162 body + the marked T8 line in WD026).
-- =========================================================================

CREATE OR REPLACE FUNCTION withdrawal_requests_state_transition_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    -- The columns a legal edge may change; every other column (today's and any future one) is frozen.
    c_dyn CONSTANT text[] := ARRAY['state', 'hold_ledger_transaction_id', 'release_ledger_transaction_id',
                                   'provider_id', 'provider_reference', 'updated_at',
                                   'reversal_ledger_transaction_id']; -- A5
    v_edge     text;
    v_new_term boolean;
    v_persons  bigint; -- A5
BEGIN
    IF TG_OP = 'INSERT' THEN
        -- Item 0 (B1): a row is born only as E0.
        IF NEW.state IS DISTINCT FROM 'requested'
           OR NEW.hold_ledger_transaction_id IS NOT NULL
           OR NEW.release_ledger_transaction_id IS NOT NULL
           OR NEW.reversal_ledger_transaction_id IS NOT NULL -- A5
           OR NEW.provider_id IS NOT NULL
           OR NEW.provider_reference IS NOT NULL THEN
            RAISE EXCEPTION 'withdrawal_requests: a request is born only as requested, with no ledger link and no provider columns (inserted state %)', NEW.state
                USING ERRCODE = 'WD025';
        END IF;
        RETURN NEW;
    END IF;

    -- Item 1: a terminal row is immutable (updated_at included). A5: except E11 out of 'completed'.
    IF OLD.state IN ('rejected', 'cancelled', 'failed', 'completed', 'reversed')
       AND NOT (OLD.state = 'completed' AND NEW.state = 'reversed') THEN
        RAISE EXCEPTION 'withdrawal_requests: request % is terminal (%) and immutable', OLD.id, OLD.state
            USING ERRCODE = 'WD020';
    END IF;

    -- Item 2: the edge allowlist (ADR 0113 s2, derived from internal/withdrawal/withdrawal.go).
    v_edge := CASE
        WHEN NEW.state = OLD.state THEN 'same'
        WHEN OLD.state = 'requested'      AND NEW.state = 'pending_review' THEN 'E1'
        WHEN OLD.state = 'requested'      AND NEW.state = 'cancelled'      THEN 'E2'
        WHEN OLD.state = 'pending_review' AND NEW.state = 'approved'       THEN 'E3'
        WHEN OLD.state = 'pending_review' AND NEW.state = 'rejected'       THEN 'E4'
        WHEN OLD.state = 'approved'       AND NEW.state = 'rejected'       THEN 'E5E6'
        WHEN OLD.state = 'approved'       AND NEW.state = 'submitted'      THEN 'E7'
        WHEN OLD.state = 'submitted'      AND NEW.state = 'completed'      THEN 'E9'
        WHEN OLD.state = 'submitted'      AND NEW.state = 'failed'         THEN 'E10'
        WHEN OLD.state = 'completed'      AND NEW.state = 'reversed'       THEN 'E11' -- A5
    END;
    IF v_edge IS NULL THEN
        RAISE EXCEPTION 'withdrawal_requests: % -> % is not a legal withdrawal transition', OLD.state, NEW.state
            USING ERRCODE = 'WD021';
    END IF;
    v_new_term := NEW.state IN ('rejected', 'cancelled', 'failed', 'completed', 'reversed');

    -- Item 3: column discipline.
    IF (to_jsonb(NEW) - c_dyn) IS DISTINCT FROM (to_jsonb(OLD) - c_dyn) THEN
        RAISE EXCEPTION 'withdrawal_requests: only state, the ledger links, the provider columns and updated_at may change'
            USING ERRCODE = 'WD022';
    END IF;
    -- hold: NULL -> value only while the row stays 'requested'; set-once.
    IF NEW.hold_ledger_transaction_id IS DISTINCT FROM OLD.hold_ledger_transaction_id
       AND NOT (OLD.hold_ledger_transaction_id IS NULL AND NEW.hold_ledger_transaction_id IS NOT NULL
                AND v_edge = 'same' AND OLD.state = 'requested') THEN
        RAISE EXCEPTION 'withdrawal_requests: the hold link is set once, only while the request is requested'
            USING ERRCODE = 'WD022';
    END IF;
    -- release: NULL -> value exactly on an edge into a terminal state; NULL on every non-terminal state.
    -- A5: E11 keeps the completion as the release link, unchanged.
    IF v_edge = 'E11' THEN
        IF OLD.release_ledger_transaction_id IS NULL OR NEW.release_ledger_transaction_id IS DISTINCT FROM OLD.release_ledger_transaction_id THEN
            RAISE EXCEPTION 'withdrawal_requests: completed -> reversed keeps the completion as the release link'
                USING ERRCODE = 'WD022';
        END IF;
    ELSIF v_new_term THEN
        IF OLD.release_ledger_transaction_id IS NOT NULL OR NEW.release_ledger_transaction_id IS NULL THEN
            RAISE EXCEPTION 'withdrawal_requests: an edge into % must set the release link exactly once', NEW.state
                USING ERRCODE = 'WD022';
        END IF;
    ELSIF NEW.release_ledger_transaction_id IS NOT NULL OR OLD.release_ledger_transaction_id IS NOT NULL THEN
        RAISE EXCEPTION 'withdrawal_requests: a non-terminal request carries no release link'
            USING ERRCODE = 'WD022';
    END IF;
    -- A5: reversal: NULL -> value exactly on E11; NULL everywhere else, never changed.
    IF v_edge = 'E11' THEN
        IF OLD.reversal_ledger_transaction_id IS NOT NULL OR NEW.reversal_ledger_transaction_id IS NULL THEN
            RAISE EXCEPTION 'withdrawal_requests: completed -> reversed must set the reversal link exactly once'
                USING ERRCODE = 'WD022';
        END IF;
    ELSIF NEW.reversal_ledger_transaction_id IS DISTINCT FROM OLD.reversal_ledger_transaction_id THEN
        RAISE EXCEPTION 'withdrawal_requests: the reversal link is set only on completed -> reversed'
            USING ERRCODE = 'WD022';
    END IF;
    -- provider_id: set on E7 (required NOT NULL there), immutable otherwise.
    IF v_edge = 'E7' THEN
        IF OLD.provider_id IS NOT NULL OR NEW.provider_id IS NULL THEN
            RAISE EXCEPTION 'withdrawal_requests: approved -> submitted must set provider_id (and only there)'
                USING ERRCODE = 'WD022';
        END IF;
    ELSIF NEW.provider_id IS DISTINCT FROM OLD.provider_id THEN
        RAISE EXCEPTION 'withdrawal_requests: provider_id is set only on approved -> submitted'
            USING ERRCODE = 'WD022';
    END IF;
    -- provider_reference: NULL -> value only on E7 or on a same-state submitted row (E8); set-once.
    IF NEW.provider_reference IS DISTINCT FROM OLD.provider_reference
       AND NOT (OLD.provider_reference IS NULL
                AND (v_edge = 'E7' OR (v_edge = 'same' AND OLD.state = 'submitted'))) THEN
        RAISE EXCEPTION 'withdrawal_requests: provider_reference is set once, on submission or on a submitted request'
            USING ERRCODE = 'WD022';
    END IF;

    -- Item 4: the workflow record (proves a record exists, not who acted: C-WD023).
    IF v_edge = 'E3' AND NOT EXISTS (
        SELECT 1 FROM withdrawal_approvals a
         WHERE a.withdrawal_request_id = OLD.id AND a.tenant_id = OLD.tenant_id AND a.decision = 'approve') THEN
        RAISE EXCEPTION 'withdrawal_requests: pending_review -> approved needs an approve record'
            USING ERRCODE = 'WD023';
    END IF;
    IF v_edge = 'E4' AND NOT EXISTS (
        SELECT 1 FROM withdrawal_approvals a
         WHERE a.withdrawal_request_id = OLD.id AND a.tenant_id = OLD.tenant_id AND a.decision = 'reject') THEN
        RAISE EXCEPTION 'withdrawal_requests: pending_review -> rejected needs a reject record'
            USING ERRCODE = 'WD023';
    END IF;
    -- A5 (WD026): E11 needs two distinct Persons (linked staff, human decisions) agreeing on ONE payload.
    IF v_edge = 'E11' THEN
        SELECT COALESCE(max(x.n), 0) INTO v_persons
          FROM (SELECT a.reason_code, count(DISTINCT su.person_id) AS n
                  FROM withdrawal_approvals a
                  JOIN staff_users su ON su.id = a.approver_principal_id
                 WHERE a.withdrawal_request_id = OLD.id AND a.tenant_id = OLD.tenant_id
                   AND a.decision = 'reverse' AND NOT a.is_automated_approval
                   AND su.person_id IS NOT NULL AND su.status = 'active' -- SR5-4 / LF-A5-3
                   AND a.actor_proof_nonce IS NOT NULL -- T8 (0165): proof-bound decisions only
                 GROUP BY a.reason_code) x;
        IF v_persons < 2 THEN
            RAISE EXCEPTION 'withdrawal_requests: completed -> reversed needs two distinct-Person reverse decisions on one payload (found %)', v_persons
                USING ERRCODE = 'WD026';
        END IF;
    END IF;
    -- Item 5 (C5): E5 is admitted on the edge and the ledger shape only (no WD024).
    RETURN NEW;
END
$$;

-- =========================================================================
-- 4. WD027, the tombstone branch (0162 body + the marked T8 line).
-- =========================================================================

CREATE OR REPLACE FUNCTION ledger_transactions_payout_tombstone_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_persons bigint;
BEGIN
    IF NEW.transaction_type IS DISTINCT FROM 'tombstone' THEN
        RETURN NULL;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM withdrawal_requests r WHERE r.id = NEW.correlation_id AND r.tenant_id = NEW.tenant_id) THEN
        RETURN NULL;
    END IF;
    SELECT COALESCE(max(x.n), 0) INTO v_persons
      FROM (SELECT a.reason_code, count(DISTINCT su.person_id) AS n
              FROM withdrawal_approvals a
              JOIN staff_users su ON su.id = a.approver_principal_id
             WHERE a.withdrawal_request_id = NEW.correlation_id AND a.tenant_id = NEW.tenant_id
               AND a.decision = 'reverse' AND NOT a.is_automated_approval
               AND su.person_id IS NOT NULL AND su.status = 'active'
               AND ledger_adjustment_eligible_grant(a.tenant_id, a.approver_principal_id, 'payment_force_resolve:approve') IS NOT NULL
               AND a.actor_proof_nonce IS NOT NULL -- T8 (0165): proof-bound decisions only
             GROUP BY a.reason_code) x;
    IF v_persons < 2 THEN
        RAISE EXCEPTION 'ledger_transactions: a payout tombstone needs two distinct active Persons with payment_force_resolve:approve on one reverse decision (found %)', v_persons
            USING ERRCODE = 'WD027';
    END IF;
    RETURN NULL;
END
$$;
