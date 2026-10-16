-- Reverses 0162 (A5 / WITHDRAWAL-REVERSAL-1; DEV/STAGING ONLY, per the migrate-down policy).
--
-- Refuses (WD097) while any reversal row exists: a 'reversed' request, a withdrawal_reversed
-- transaction or a 'reverse' decision. Rows are never deleted or edited to make a down pass. Otherwise
-- the schema returns to 0160 byte for byte (whole-schema snapshot test): the 0132 and 0034 function
-- bodies below are verbatim copies.

DO $$
DECLARE
    tenant_rec RECORD;
    c BIGINT := 0;
    n BIGINT;
BEGIN
    LOCK TABLE withdrawal_requests, withdrawal_approvals, ledger_transactions IN SHARE ROW EXCLUSIVE MODE;
    PERFORM set_config('app.player_account_id', '', true);
    PERFORM set_config('app.platform_admin_principal_id', '', true);
    PERFORM set_config('app.acting_tenant_id', '', true);
    PERFORM set_config('app.acting_platform_principal_id', '', true);
    FOR tenant_rec IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.tenant_id', tenant_rec.id::text, true);
        SELECT count(*) INTO n FROM withdrawal_requests w
         WHERE w.tenant_id = tenant_rec.id AND (w.state = 'reversed' OR w.reversal_ledger_transaction_id IS NOT NULL);
        c := c + n;
        SELECT count(*) INTO n FROM ledger_transactions t WHERE t.tenant_id = tenant_rec.id AND t.transaction_type = 'withdrawal_reversed';
        c := c + n;
        SELECT count(*) INTO n FROM withdrawal_approvals a WHERE a.tenant_id = tenant_rec.id AND a.decision = 'reverse';
        c := c + n;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);
    IF c > 0 THEN
        RAISE EXCEPTION 'migration 0162 down refused: % payout reversal rows exist (reversed requests, withdrawal_reversed transactions, reverse decisions); rows are never deleted', c
            USING ERRCODE = 'WD097';
    END IF;
END $$;

DROP TRIGGER ledger_transactions_payout_tombstone_guard ON ledger_transactions;
DROP FUNCTION ledger_transactions_payout_tombstone_guard();

CREATE OR REPLACE FUNCTION ledger_transactions_withdrawal_hold_leg_at_commit() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF NEW.transaction_type NOT IN ('withdrawal_requested', 'withdrawal_rejected', 'withdrawal_failed',
                                    'withdrawal_completed', 'withdrawal_reversed') THEN
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
            ELSE false
        END;
        IF NOT COALESCE(v_ok, false) THEN
            RETURN ARRAY['WD033', format('the release transaction is not the %s shape (type, key, provider, correlation or reversal link)', w.state)];
        END IF;
        SELECT count(*),
               count(*) FILTER (WHERE e.direction = 'debit' AND la.account_type = 'player_withdrawal_hold'
                                  AND e.wallet_id = w.wallet_id AND e.asset_code = w.asset_code AND e.amount = w.amount),
               count(*) FILTER (WHERE e.direction = 'credit' AND e.asset_code = w.asset_code AND e.amount = w.amount
                                  AND CASE WHEN w.state = 'completed'
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

CREATE OR REPLACE FUNCTION withdrawal_requests_state_transition_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    -- The columns a legal edge may change; every other column (today's and any future one) is frozen.
    c_dyn CONSTANT text[] := ARRAY['state', 'hold_ledger_transaction_id', 'release_ledger_transaction_id',
                                   'provider_id', 'provider_reference', 'updated_at'];
    v_edge     text;
    v_new_term boolean;
BEGIN
    IF TG_OP = 'INSERT' THEN
        -- Item 0 (B1): a row is born only as E0.
        IF NEW.state IS DISTINCT FROM 'requested'
           OR NEW.hold_ledger_transaction_id IS NOT NULL
           OR NEW.release_ledger_transaction_id IS NOT NULL
           OR NEW.provider_id IS NOT NULL
           OR NEW.provider_reference IS NOT NULL THEN
            RAISE EXCEPTION 'withdrawal_requests: a request is born only as requested, with no ledger link and no provider columns (inserted state %)', NEW.state
                USING ERRCODE = 'WD025';
        END IF;
        RETURN NEW;
    END IF;

    -- Item 1: a terminal row is immutable (updated_at included).
    IF OLD.state IN ('rejected', 'cancelled', 'failed', 'completed', 'reversed') THEN
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
    IF v_new_term THEN
        IF OLD.release_ledger_transaction_id IS NOT NULL OR NEW.release_ledger_transaction_id IS NULL THEN
            RAISE EXCEPTION 'withdrawal_requests: an edge into % must set the release link exactly once', NEW.state
                USING ERRCODE = 'WD022';
        END IF;
    ELSIF NEW.release_ledger_transaction_id IS NOT NULL OR OLD.release_ledger_transaction_id IS NOT NULL THEN
        RAISE EXCEPTION 'withdrawal_requests: a non-terminal request carries no release link'
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
    -- Item 5 (C5): E5 is admitted on the edge and the ledger shape only (no WD024).
    RETURN NEW;
END
$$;

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
        -- No staff_users row at all for this principal. A genuine
        -- automated/service decision (ADR 0014's service-identity
        -- pattern, used for a below-threshold risk-engine auto-approval)
        -- has no staff account and is exempt from every rule below - it
        -- is not, and cannot be, a Person. A HUMAN decision naming an
        -- identity that cannot be resolved at all is refused outright:
        -- "identity cannot be resolved" is exactly as ineligible as
        -- "resolved but unlinked" per docs/decisions/0024 §1.
        IF NEW.is_automated_approval THEN
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'withdrawal_approvals: approver identity cannot be resolved and is not eligible to record withdrawal decisions';
    END IF;

    -- A REAL staff_users row resolved for this principal - every rule
    -- below applies REGARDLESS of the caller-supplied
    -- is_automated_approval flag (see this migration's own doc comment
    -- for why: that flag identifies nothing on its own once a real
    -- staff row exists for the id).
    IF approver_person_id IS NULL THEN
        RAISE EXCEPTION 'withdrawal_approvals: approver has no confirmed Person linkage and is not eligible to record withdrawal decisions';
    END IF;

    IF approver_status IS DISTINCT FROM 'active' THEN
        RAISE EXCEPTION 'withdrawal_approvals: approver staff account is not active';
    END IF;

    IF NEW.decision = 'approve' THEN
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

DROP INDEX ledger_transactions_one_withdrawal_reversal;
DROP INDEX withdrawal_approvals_one_reverse_decision_per_approver;
DROP INDEX withdrawal_approvals_one_review_decision_per_approver;
ALTER TABLE withdrawal_approvals ADD CONSTRAINT withdrawal_approvals_withdrawal_request_id_approver_princip_key
    UNIQUE (withdrawal_request_id, approver_principal_id);
ALTER TABLE withdrawal_approvals DROP CONSTRAINT withdrawal_approvals_reverse_reason_shape;
ALTER TABLE withdrawal_approvals DROP CONSTRAINT withdrawal_approvals_decision_check;
ALTER TABLE withdrawal_approvals ADD CONSTRAINT withdrawal_approvals_decision_check
    CHECK (decision IN ('approve', 'reject'));
ALTER TABLE withdrawal_requests DROP CONSTRAINT withdrawal_requests_reversal_ledger_transaction_id_fkey;
ALTER TABLE withdrawal_requests DROP COLUMN reversal_ledger_transaction_id;
