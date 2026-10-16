-- A5 / WITHDRAWAL-REVERSAL-1 (owner decision 2026-10-15; ADR 0113 s9.7): the governed payout reversal
-- (a payout the provider later returns or reverses), as a COMPENSATING ledger entry, never an edit.
-- MONEY-PATH TRIGGERS: FOR `ledger-finance`, `security`, `code-reviewer`, `qa` REVIEW BEFORE MERGE.
--
-- NUMBERING: 0162 (0159 and 0161 stay reserved and unused; internal/db/migrate.go is unchanged).
--
-- The posting (withdrawal_reversed, the type ledger.go already declares and 0132 refused end to end):
--   Dr psp_clearing (house, asset) / Cr player_cash (the request's wallet), amount = the request amount,
--   idempotency key '<request id>:reversed', correlation_id = request id, reverses_transaction_id = the
--   request's withdrawal_completed transaction (its release link), provider_id = the request's
--   provider_id, provider_tx_id = the provider's RETURN reference. No hold leg: the hold was already
--   closed by the completion, so B-i (0132) keeps refusing every hold leg of this type.
-- The transition: E11 completed -> reversed (the edge 0132 drew but refused), setting the new
-- reversal_ledger_transaction_id link exactly once; the release link (the completion) is unchanged.
-- The governance: E11 needs at least TWO distinct Persons (non-automated 'reverse' decisions in
-- withdrawal_approvals, linked active staff, never the beneficiary) that agreed on ONE payload digest
-- (reason_code 'payout_reversal:<sha256 hex>'). The Go layer requires max(2, the withdrawal policy's
-- required approvals) and the policy's step-up; the database floor is two (WD026). As for WD023, this
-- proves distinct-Person decision records exist; it does not prove who typed them (ADR 0113 s9.3).
--
-- SQLSTATEs (class WD; 0131/0132 own WD010-WD043, WD098):
--   WD026 E11 without two distinct-Person 'reverse' decisions on one payload digest
--   WD035 A: the reversal link / reversal transaction is not the s4.2 shape for the state
--   WD044 a withdrawal_reversed transaction that no 'reversed' request links at COMMIT (or a hold leg on it,
--         or check A fails for its request)
--   WD097 down-migration refused while reversal rows exist (rows are never deleted to make a down pass)
--   23505 ledger_transactions_one_withdrawal_reversal: a second reversal of the same completion
--
-- Replaced IN PLACE (the 0132 bodies plus the marked A5 lines; the down restores the 0132 / 0034 bodies
-- byte for byte - whole-schema snapshot test internal/withdrawal/a5_migration_0162_integration_test.go):
--   withdrawal_requests_state_transition_guard()        (0132)  + E11, the reversal link column, WD026
--   withdrawal_request_ledger_link_violation(uuid,uuid)  (0132)  + the 'reversed' arm (WD033 / WD035)
--   ledger_transactions_withdrawal_hold_leg_at_commit()  (0132)  withdrawal_reversed: no hold leg, linked (WD044)
--   withdrawal_approvals_enforce_governance()            (0034)  the beneficiary exclusion covers 'reverse'
-- Added: withdrawal_requests.reversal_ledger_transaction_id (+ FK), the 'reverse' decision on
-- withdrawal_approvals (+ its reason_code shape), the per-decision-class uniqueness that replaces the
-- old (request, approver) UNIQUE (an approver of the payout may also take part in its reversal; one vote
-- per person per class), and ledger_transactions_one_withdrawal_reversal.
--
-- No role is exempt; no function is SECURITY DEFINER; every function pins search_path. No new lock: the
-- writer already holds L1 (the request FOR UPDATE), L2 (the completion FOR UPDATE) and ledger.Post's L3.

-- =========================================================================
-- 0. Pre-flight: no request is 'reversed' yet and no withdrawal_reversed transaction exists (0132 refused
--    both end to end). Per tenant under set_config (FORCE RLS: the 0132 pattern); fail loud, never fix.
-- =========================================================================

DO $$
DECLARE
    tenant_rec RECORD;
    c_reversed BIGINT := 0;
    c_tx       BIGINT := 0;
    n BIGINT;
BEGIN
    LOCK TABLE withdrawal_requests, withdrawal_approvals, ledger_transactions IN SHARE ROW EXCLUSIVE MODE;
    PERFORM set_config('app.player_account_id', '', true);
    PERFORM set_config('app.platform_admin_principal_id', '', true);
    PERFORM set_config('app.acting_tenant_id', '', true);
    PERFORM set_config('app.acting_platform_principal_id', '', true);
    FOR tenant_rec IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.tenant_id', tenant_rec.id::text, true);
        SELECT count(*) INTO n FROM withdrawal_requests w WHERE w.tenant_id = tenant_rec.id AND w.state = 'reversed';
        c_reversed := c_reversed + n;
        SELECT count(*) INTO n FROM ledger_transactions t WHERE t.tenant_id = tenant_rec.id AND t.transaction_type = 'withdrawal_reversed';
        c_tx := c_tx + n;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
    IF c_reversed + c_tx > 0 THEN
        RAISE EXCEPTION 'migration 0162 pre-flight: reversed requests=% withdrawal_reversed transactions=% exist although 0132 refused both; nothing was changed; escalate (never delete or edit rows)',
            c_reversed, c_tx USING ERRCODE = 'WD098';
    END IF;
END $$;

-- =========================================================================
-- 1. Schema: the reversal link, the 'reverse' decision, the reversal uniqueness.
-- =========================================================================

ALTER TABLE withdrawal_requests ADD COLUMN reversal_ledger_transaction_id UUID NULL;
ALTER TABLE withdrawal_requests ADD CONSTRAINT withdrawal_requests_reversal_ledger_transaction_id_fkey
    FOREIGN KEY (reversal_ledger_transaction_id) REFERENCES ledger_transactions (id);

ALTER TABLE withdrawal_approvals DROP CONSTRAINT withdrawal_approvals_decision_check;
ALTER TABLE withdrawal_approvals ADD CONSTRAINT withdrawal_approvals_decision_check
    CHECK (decision IN ('approve', 'reject', 'reverse'));
-- A 'reverse' decision names the payload it agrees to (the Go layer's sha256 digest); nothing else does.
ALTER TABLE withdrawal_approvals ADD CONSTRAINT withdrawal_approvals_reverse_reason_shape
    CHECK ((decision = 'reverse') = (reason_code IS NOT NULL AND reason_code ~ '^payout_reversal:[0-9a-f]{64}$'));
-- One vote per approver per decision class: the review decisions (approve / reject) keep the 0026 rule
-- exactly; a 'reverse' decision is a separate, later governance act on the same request.
ALTER TABLE withdrawal_approvals DROP CONSTRAINT withdrawal_approvals_withdrawal_request_id_approver_princip_key;
CREATE UNIQUE INDEX withdrawal_approvals_one_review_decision_per_approver
    ON withdrawal_approvals (withdrawal_request_id, approver_principal_id) WHERE decision IN ('approve', 'reject');
CREATE UNIQUE INDEX withdrawal_approvals_one_reverse_decision_per_approver
    ON withdrawal_approvals (withdrawal_request_id, approver_principal_id) WHERE decision = 'reverse';

-- At most one withdrawal_reversed per completion (the 0092 / 0121 pattern): the DB backstop behind the
-- '<id>:reversed' key and the L2 lock on the completion.
CREATE UNIQUE INDEX ledger_transactions_one_withdrawal_reversal
    ON ledger_transactions (tenant_id, reverses_transaction_id)
    WHERE transaction_type = 'withdrawal_reversed';

-- =========================================================================
-- 2. withdrawal_approvals_enforce_governance (0034 body + the A5 line): the beneficiary exclusion also
--    covers 'reverse' (a staff member who is the paid player never votes to credit themselves).
-- =========================================================================

CREATE OR REPLACE FUNCTION withdrawal_approvals_enforce_governance() RETURNS TRIGGER AS $$
DECLARE
    requester_person_id UUID;
    approver_person_id  UUID;
    approver_status     TEXT;
BEGIN
    SELECT su.person_id, su.status INTO approver_person_id, approver_status
    FROM staff_users su
    WHERE su.id = NEW.approver_principal_id;

    IF NOT FOUND THEN
        IF NEW.is_automated_approval AND NEW.decision <> 'reverse' THEN -- A5: a reversal is never automated
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'withdrawal_approvals: approver identity cannot be resolved and is not eligible to record withdrawal decisions';
    END IF;

    IF approver_person_id IS NULL THEN
        RAISE EXCEPTION 'withdrawal_approvals: approver has no confirmed Person linkage and is not eligible to record withdrawal decisions';
    END IF;

    IF approver_status IS DISTINCT FROM 'active' THEN
        RAISE EXCEPTION 'withdrawal_approvals: approver staff account is not active';
    END IF;

    IF NEW.decision IN ('approve', 'reverse') THEN -- A5: + 'reverse'
        SELECT pa.person_id INTO requester_person_id
        FROM withdrawal_requests wr
        JOIN player_accounts pa ON pa.id = wr.player_account_id
        WHERE wr.id = NEW.withdrawal_request_id;

        IF requester_person_id IS NOT NULL AND requester_person_id = approver_person_id THEN
            RAISE EXCEPTION 'withdrawal_approvals: approver resolves to the same person as the withdrawing player (self-approval)';
        END IF;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- =========================================================================
-- 3. The state-transition guard (0132 body + the marked A5 lines).
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
                   AND su.person_id IS NOT NULL
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
-- 4. Check A (0132 body + the marked A5 lines): a 'reversed' request keeps the exact completion as its
--    release link (WD033) and links exactly one withdrawal_reversed transaction of the pinned shape
--    (WD035); every other state carries no reversal link.
-- =========================================================================

CREATE OR REPLACE FUNCTION withdrawal_request_ledger_link_violation(p_id uuid, p_tenant uuid) RETURNS text[]
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    w       withdrawal_requests%ROWTYPE;
    t       ledger_transactions%ROWTYPE;
    v_id    text;
    v_term  boolean;
    v_ok    boolean;
    v_n     bigint;
    v_dr    bigint;
    v_cr    bigint;
    v_sum   numeric;
BEGIN
    -- C-A: the session shape first. A player-scoped session sees the request but only its own
    -- entries, so an entry count could pass blind; a session of another tenant sees nothing.
    IF NULLIF(current_setting('app.player_account_id', true), '') IS NOT NULL
       OR COALESCE(NULLIF(current_setting('app.tenant_id', true), ''),
                   NULLIF(current_setting('app.acting_tenant_id', true), '')) IS DISTINCT FROM p_tenant::text THEN
        RETURN ARRAY['WD030', 'the session is not the tenant (or acting) scope of the request'];
    END IF;

    SELECT * INTO w FROM withdrawal_requests r WHERE r.id = p_id AND r.tenant_id = p_tenant;
    IF NOT FOUND THEN
        RETURN ARRAY['WD030', 'the request is not visible'];
    END IF;
    v_id := w.id::text;
    v_term := w.state IN ('rejected', 'cancelled', 'failed', 'completed', 'reversed');

    -- The hold (E0b).
    IF w.hold_ledger_transaction_id IS NULL THEN
        RETURN ARRAY['WD031', 'the request has no hold link'];
    END IF;
    SELECT * INTO t FROM ledger_transactions x WHERE x.id = w.hold_ledger_transaction_id AND x.tenant_id = w.tenant_id;
    IF NOT FOUND THEN
        RETURN ARRAY['WD030', 'the hold transaction is not visible'];
    END IF;
    IF t.transaction_type IS DISTINCT FROM 'withdrawal_requested' OR t.idempotency_key IS DISTINCT FROM v_id
       OR t.correlation_id IS DISTINCT FROM w.id OR t.reverses_transaction_id IS NOT NULL THEN
        RETURN ARRAY['WD032', 'the hold transaction is not this request''s withdrawal_requested leg'];
    END IF;
    SELECT count(*),
           count(*) FILTER (WHERE e.direction = 'debit' AND la.account_type = 'player_cash'
                              AND e.wallet_id = w.wallet_id AND e.asset_code = w.asset_code AND e.amount = w.amount),
           count(*) FILTER (WHERE e.direction = 'credit' AND la.account_type = 'player_withdrawal_hold'
                              AND e.wallet_id = w.wallet_id AND e.asset_code = w.asset_code AND e.amount = w.amount)
      INTO v_n, v_dr, v_cr
      FROM ledger_entries e LEFT JOIN ledger_accounts la ON la.id = e.ledger_account_id
     WHERE e.ledger_transaction_id = t.id;
    IF v_n = 0 THEN
        RETURN ARRAY['WD030', 'the hold entries are not visible'];
    END IF;
    IF v_n <> 2 OR v_dr <> 1 OR v_cr <> 1 THEN
        RETURN ARRAY['WD032', 'the hold transaction is not exactly Dr player_cash / Cr player_withdrawal_hold on the wallet, asset and amount'];
    END IF;

    -- The release.
    IF NOT v_term THEN
        IF w.release_ledger_transaction_id IS NOT NULL THEN
            RETURN ARRAY['WD033', 'a non-terminal request carries a release link'];
        END IF;
    ELSE
        IF w.release_ledger_transaction_id IS NULL THEN
            RETURN ARRAY['WD033', 'a terminal request has no release link'];
        END IF;
        SELECT * INTO t FROM ledger_transactions x WHERE x.id = w.release_ledger_transaction_id AND x.tenant_id = w.tenant_id;
        IF NOT FOUND THEN
            RETURN ARRAY['WD030', 'the release transaction is not visible'];
        END IF;
        v_ok := t.correlation_id = w.id AND CASE w.state
            WHEN 'cancelled' THEN t.transaction_type = 'withdrawal_rejected' AND t.idempotency_key = v_id || ':cancelled'
                                  AND t.reverses_transaction_id IS NOT DISTINCT FROM w.hold_ledger_transaction_id
            WHEN 'rejected'  THEN t.transaction_type = 'withdrawal_rejected'
                                  AND t.idempotency_key IN (v_id || ':rejected', v_id || ':kyc_denied', v_id || ':governed_hold_released')
                                  AND t.reverses_transaction_id IS NOT DISTINCT FROM w.hold_ledger_transaction_id
            WHEN 'failed'    THEN t.transaction_type = 'withdrawal_failed' AND t.idempotency_key = v_id || ':failed'
                                  AND t.reverses_transaction_id IS NOT DISTINCT FROM w.hold_ledger_transaction_id
            WHEN 'completed' THEN t.transaction_type = 'withdrawal_completed' AND w.provider_id IS NOT NULL
                                  AND t.provider_id = w.provider_id AND t.idempotency_key = w.provider_id || ':' || t.provider_tx_id
                                  AND t.reverses_transaction_id IS NULL
            -- A5: a reversed request keeps its completion as the release link.
            WHEN 'reversed'  THEN t.transaction_type = 'withdrawal_completed' AND w.provider_id IS NOT NULL
                                  AND t.provider_id = w.provider_id AND t.idempotency_key = w.provider_id || ':' || t.provider_tx_id
                                  AND t.reverses_transaction_id IS NULL
            ELSE false
        END;
        IF NOT COALESCE(v_ok, false) THEN
            RETURN ARRAY['WD033', format('the release transaction is not the %s shape (type, key, provider, correlation or reversal link)', w.state)];
        END IF;
        SELECT count(*),
               count(*) FILTER (WHERE e.direction = 'debit' AND la.account_type = 'player_withdrawal_hold'
                                  AND e.wallet_id = w.wallet_id AND e.asset_code = w.asset_code AND e.amount = w.amount),
               count(*) FILTER (WHERE e.direction = 'credit' AND e.asset_code = w.asset_code AND e.amount = w.amount
                                  AND CASE WHEN w.state IN ('completed', 'reversed') -- A5: + reversed
                                           THEN la.account_type = 'psp_clearing' AND e.wallet_id IS NULL
                                           ELSE la.account_type = 'player_cash' AND e.wallet_id = w.wallet_id END)
          INTO v_n, v_dr, v_cr
          FROM ledger_entries e LEFT JOIN ledger_accounts la ON la.id = e.ledger_account_id
         WHERE e.ledger_transaction_id = t.id;
        IF v_n = 0 THEN
            RETURN ARRAY['WD030', 'the release entries are not visible'];
        END IF;
        IF v_n <> 2 OR v_dr <> 1 OR v_cr <> 1 THEN
            RETURN ARRAY['WD033', 'the release transaction is not exactly Dr player_withdrawal_hold / Cr the state''s counter-account on the wallet, asset and amount'];
        END IF;
    END IF;

    -- A5 (WD035): the reversal link.
    IF w.state IS DISTINCT FROM 'reversed' THEN
        IF w.reversal_ledger_transaction_id IS NOT NULL THEN
            RETURN ARRAY['WD035', 'only a reversed request carries a reversal link'];
        END IF;
    ELSE
        IF w.reversal_ledger_transaction_id IS NULL THEN
            RETURN ARRAY['WD035', 'a reversed request has no reversal link'];
        END IF;
        SELECT * INTO t FROM ledger_transactions x WHERE x.id = w.reversal_ledger_transaction_id AND x.tenant_id = w.tenant_id;
        IF NOT FOUND THEN
            RETURN ARRAY['WD030', 'the reversal transaction is not visible'];
        END IF;
        IF t.transaction_type IS DISTINCT FROM 'withdrawal_reversed' OR t.idempotency_key IS DISTINCT FROM v_id || ':reversed'
           OR t.correlation_id IS DISTINCT FROM w.id
           OR t.reverses_transaction_id IS DISTINCT FROM w.release_ledger_transaction_id
           OR t.provider_id IS DISTINCT FROM w.provider_id OR t.provider_tx_id IS NULL THEN
            RETURN ARRAY['WD035', 'the reversal transaction is not this request''s withdrawal_reversed shape (type, key, correlation, reversed completion or provider)'];
        END IF;
        SELECT count(*),
               count(*) FILTER (WHERE e.direction = 'debit' AND la.account_type = 'psp_clearing' AND e.wallet_id IS NULL
                                  AND e.asset_code = w.asset_code AND e.amount = w.amount),
               count(*) FILTER (WHERE e.direction = 'credit' AND la.account_type = 'player_cash'
                                  AND e.wallet_id = w.wallet_id AND e.asset_code = w.asset_code AND e.amount = w.amount)
          INTO v_n, v_dr, v_cr
          FROM ledger_entries e LEFT JOIN ledger_accounts la ON la.id = e.ledger_account_id
         WHERE e.ledger_transaction_id = t.id;
        IF v_n = 0 THEN
            RETURN ARRAY['WD030', 'the reversal entries are not visible'];
        END IF;
        IF v_n <> 2 OR v_dr <> 1 OR v_cr <> 1 THEN
            RETURN ARRAY['WD035', 'the reversal transaction is not exactly Dr psp_clearing / Cr player_cash on the wallet, asset and amount'];
        END IF;
    END IF;

    -- WD034: every correlated hold leg, linked or not (idx_ledger_transactions_correlation, 0021).
    SELECT COALESCE(sum(CASE WHEN e.direction = 'credit' THEN e.amount ELSE -e.amount END), 0)
      INTO v_sum
      FROM ledger_transactions x
      JOIN ledger_entries e ON e.ledger_transaction_id = x.id
      JOIN ledger_accounts la ON la.id = e.ledger_account_id
     WHERE x.correlation_id = w.id AND x.tenant_id = w.tenant_id
       AND la.account_type = 'player_withdrawal_hold' AND e.wallet_id = w.wallet_id AND e.asset_code = w.asset_code;
    IF v_sum <> (CASE WHEN v_term THEN 0 ELSE w.amount END) THEN
        RETURN ARRAY['WD034', format('the request''s correlated hold legs net to %s, expected %s', v_sum, CASE WHEN v_term THEN 0 ELSE w.amount END)];
    END IF;
    RETURN NULL;
END
$$;

-- =========================================================================
-- 5. WD043 / WD044 (0132 body + the A5 branch): withdrawal_requested / _rejected / _failed / _completed
--    still need a player_withdrawal_hold leg (WD043, unchanged). withdrawal_reversed carries NO hold leg
--    (B-i refuses one anyway: WD040) and must, at COMMIT, be the reversal link of a 'reversed' request
--    that passes check A (WD044); a reversal leg nobody links, or a second one, cannot commit.
-- =========================================================================

CREATE OR REPLACE FUNCTION ledger_transactions_withdrawal_hold_leg_at_commit() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v text[]; -- A5
BEGIN
    IF NEW.transaction_type NOT IN ('withdrawal_requested', 'withdrawal_rejected', 'withdrawal_failed',
                                    'withdrawal_completed', 'withdrawal_reversed') THEN
        RETURN NULL;
    END IF;
    -- A5: withdrawal_reversed (WITHDRAWAL-REVERSAL-1).
    IF NEW.transaction_type = 'withdrawal_reversed' THEN
        IF NULLIF(current_setting('app.player_account_id', true), '') IS NOT NULL
           OR COALESCE(NULLIF(current_setting('app.tenant_id', true), ''),
                       NULLIF(current_setting('app.acting_tenant_id', true), '')) IS DISTINCT FROM NEW.tenant_id::text THEN
            RAISE EXCEPTION 'ledger_transactions: the session is not the tenant (or acting) scope of the withdrawal_reversed transaction at commit' USING ERRCODE = 'WD030';
        END IF;
        IF EXISTS (SELECT 1 FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
                    WHERE e.ledger_transaction_id = NEW.id AND e.tenant_id = NEW.tenant_id
                      AND la.account_type = 'player_withdrawal_hold') THEN
            RAISE EXCEPTION 'ledger_transactions: a withdrawal_reversed transaction carries no player_withdrawal_hold leg' USING ERRCODE = 'WD044';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM withdrawal_requests r
                        WHERE r.id = NEW.correlation_id AND r.tenant_id = NEW.tenant_id
                          AND r.state = 'reversed' AND r.reversal_ledger_transaction_id = NEW.id) THEN
            RAISE EXCEPTION 'ledger_transactions: withdrawal_reversed % is not the reversal link of a reversed request at commit', NEW.id
                USING ERRCODE = 'WD044';
        END IF;
        v := withdrawal_request_ledger_link_violation(NEW.correlation_id, NEW.tenant_id);
        IF v IS NOT NULL THEN
            RAISE EXCEPTION 'ledger_transactions: the request of withdrawal_reversed % fails check A at commit (%: %)', NEW.id, v[1], v[2]
                USING ERRCODE = 'WD044';
        END IF;
        RETURN NULL;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
                    WHERE e.ledger_transaction_id = NEW.id AND e.tenant_id = NEW.tenant_id
                      AND la.account_type = 'player_withdrawal_hold') THEN
        RAISE EXCEPTION 'ledger_transactions: a % transaction must carry a player_withdrawal_hold leg', NEW.transaction_type
            USING ERRCODE = 'WD043';
    END IF;
    RETURN NULL;
END
$$;
