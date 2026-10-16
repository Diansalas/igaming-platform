-- Reverses 0165 (ADR 0110 T8 amendment; DEV/STAGING ONLY, per the migrate-down policy).
--
-- Refuses (WD096) while any proof-bound 'reverse' decision exists (actor_proof_nonce IS NOT NULL): dropping
-- the column would erase the only record that a decision was proven, and the 0162 counting rules would then
-- count those rows again. Rows are never deleted or edited to make a down pass. Otherwise the schema returns
-- to 0164 byte for byte (whole-schema snapshot test): the two 0162 function bodies below are verbatim.

DO $$
DECLARE
    tenant_rec RECORD;
    c BIGINT := 0;
    n BIGINT;
BEGIN
    LOCK TABLE withdrawal_approvals IN SHARE ROW EXCLUSIVE MODE;
    PERFORM set_config('app.player_account_id', '', true);
    PERFORM set_config('app.platform_admin_principal_id', '', true);
    PERFORM set_config('app.acting_tenant_id', '', true);
    PERFORM set_config('app.acting_platform_principal_id', '', true);
    FOR tenant_rec IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.tenant_id', tenant_rec.id::text, true);
        SELECT count(*) INTO n FROM withdrawal_approvals a
         WHERE a.tenant_id = tenant_rec.id AND a.actor_proof_nonce IS NOT NULL;
        c := c + n;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
    IF c > 0 THEN
        RAISE EXCEPTION 'migration 0165 down refused: % proof-bound reverse decisions exist; rows are never deleted', c
            USING ERRCODE = 'WD096';
    END IF;
END $$;

-- The 0162 tombstone guard (WD027), verbatim.
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
             GROUP BY a.reason_code) x;
    IF v_persons < 2 THEN
        RAISE EXCEPTION 'ledger_transactions: a payout tombstone needs two distinct active Persons with payment_force_resolve:approve on one reverse decision (found %)', v_persons
            USING ERRCODE = 'WD027';
    END IF;
    RETURN NULL;
END
$$;

-- The 0162 state-transition guard, verbatim.

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

DROP TRIGGER zz_actor_proof_guard ON withdrawal_approvals;
DROP FUNCTION actor_proof_withdrawal_approvals_guard();
DROP INDEX withdrawal_approvals_actor_proof_nonce_key;
ALTER TABLE withdrawal_approvals DROP CONSTRAINT withdrawal_approvals_actor_proof_nonce_shape;
ALTER TABLE withdrawal_approvals DROP COLUMN actor_proof_nonce;
