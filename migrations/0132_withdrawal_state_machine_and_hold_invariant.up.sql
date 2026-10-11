-- ADR 0113 slice 1 (M-A): the withdrawal database state machine and the hold invariant (#12).
-- MONEY-PATH TRIGGERS: FOR `ledger-finance`, `security`, `code-reviewer`, `qa` REVIEW BEFORE MERGE.
--
-- NUMBERING: 0132 (gap-free after 0131; the next free number in migrations/ and every doc reference at
-- authoring time). Migration numbers are allocated by the orchestrator; confirm before merge.
--
-- SQLSTATE class "WD" (0131 owns WD010-WD012). New here:
--   WD020 a terminal withdrawal request is immutable (any UPDATE, updated_at included)
--   WD021 the (from, to) state pair is not one of the legal edges E1-E10 (ADR 0113 s2)
--   WD022 column discipline on an UPDATE (ADR 0113 s3 item 3)
--   WD023 E3 / E4 without the workflow record (approve / reject row) - NOT a four-eyes control (C-WD023)
--   WD025 an INSERT that is not born as E0 (state 'requested', no ledger links, no provider columns)
--   WD030 deferred check A / B-ii: the session shape is wrong (C-A) or a row it must read is not visible
--   WD031 A: no hold link at commit
--   WD032 A: the hold transaction is not the s4.2 shape
--   WD033 A: the release link / release transaction is not the s4.2 shape for the state
--   WD034 A: the request's correlated hold legs do not net to amount (non-terminal) / 0 (terminal)
--   WD040 B-i: an entry on a player_withdrawal_hold account that is not a withdrawal leg, or whose
--         account, transaction or request is not visible to the session (fail closed)
--   WD041 B-i: a withdrawal hold leg that is not the pinned leg of the named request in its pre-state
--   WD042 B-ii: at COMMIT the named request does not link this hold leg's transaction, or A fails for it
--   WD098 up-migration refused: the pre-flight found existing rows that violate the new rules
--
-- What it adds (ADR 0113 s3, s4.2, s6 M-A):
--   1. withdrawal_requests_aa_state_transition_guard: BEFORE INSERT OR UPDATE, FOR EACH ROW, no WHEN
--      clause. "aa_state" sorts before "aa_subject_active_guard" (0131), "approved_hold_freeze" and
--      "governed_release_guard" (0124), "immutable_fields" (0026) and "payout_binding_guard" (0126), so
--      an illegal shape is refused before any gate key or instrument row lock is taken (catalog test).
--   2. withdrawal_requests_ledger_link_at_commit (A): CONSTRAINT TRIGGER AFTER INSERT OR UPDATE,
--      DEFERRABLE INITIALLY DEFERRED; re-reads the row by id and checks the ledger links.
--   3. ledger_entries_withdrawal_hold_guard (B-i): BEFORE INSERT, no WHEN clause, sorts after
--      ledger_entries_populate_from_account_trigger (0022, which fills wallet_id); fast exit on a NULL
--      wallet_id, then exactly ONE ledger_accounts primary-key read, exit unless player_withdrawal_hold.
--   4. ledger_entries_withdrawal_hold_link_at_commit (B-ii): CONSTRAINT TRIGGER AFTER INSERT,
--      DEFERRABLE INITIALLY DEFERRED, the same fast exits. Deferred triggers fire in event-queue order,
--      not name order: no order is claimed between A and B-ii.
--   5. idx_withdrawal_requests_open_wallet_asset: (wallet_id, asset_code) WHERE state is non-terminal.
--
-- No role is exempt (owner included, while the triggers are enabled: ADR 0110 9.4 residual). No
-- function is SECURITY DEFINER; every function pins search_path. Nothing takes a lock: writers already
-- hold L1 (the request FOR UPDATE) and ledger.Post's L3 projection locks (ADR 0082), so the lock order
-- is unchanged. Every read is RLS-bound to the session: a row the session cannot see fails closed.
--
-- NO existing function, policy, trigger or row is changed. The down migration drops exactly what this
-- file creates (whole-schema snapshot test: internal/withdrawal/migration_0132_integration_test.go).
--
-- Literal pin (docs/security/launch-status-readers.md): this file reads no tenant / brand status.

-- =========================================================================
-- 0. Pre-flight (ADR 0113 s6): assert, BEFORE creating anything, that every existing row already
--    satisfies the new rules. Fail loud with per-class counts; NEVER fix, edit or delete a row (the
--    remedy is a human decision). The tables are FORCE RLS and the migration session carries no
--    app.tenant_id, so a plain SELECT would be blind (the 0048 / 0092 / 0107 lesson): the scan iterates
--    tenants with set_config (the 0115:69-108 pattern). Scope: every row whose tenant_id names a row
--    of tenants (withdrawal_requests reach tenants through the brands FK; a ledger row of a tenant id
--    with no tenants row is invisible to this loop and to every tenant session alike - residual,
--    covered by reconciliation D in slice 3).
-- =========================================================================

DO $$
DECLARE
    tenant_rec RECORD;
    c_reversed  BIGINT := 0;
    c_columns   BIGINT := 0;
    c_hold_link BIGINT := 0;
    c_release   BIGINT := 0;
    c_hold_sum  BIGINT := 0;
    c_entry     BIGINT := 0;
    c_balance   BIGINT := 0;
    n BIGINT;
BEGIN
    PERFORM set_config('app.player_account_id', '', true);
    PERFORM set_config('app.platform_admin_principal_id', '', true);
    PERFORM set_config('app.acting_tenant_id', '', true);
    PERFORM set_config('app.acting_platform_principal_id', '', true);
    FOR tenant_rec IN SELECT id FROM tenants LOOP
        PERFORM set_config('app.tenant_id', tenant_rec.id::text, true);

        -- (1) no request is 'reversed' (no legal edge reaches it).
        SELECT count(*) INTO n FROM withdrawal_requests w WHERE w.tenant_id = tenant_rec.id AND w.state = 'reversed';
        c_reversed := c_reversed + n;

        -- (2) s3 items 0 and 3: the columns a row may carry in its state.
        SELECT count(*) INTO n FROM withdrawal_requests w
         WHERE w.tenant_id = tenant_rec.id
           AND ((w.state IN ('requested', 'pending_review', 'approved')
                 AND (w.release_ledger_transaction_id IS NOT NULL OR w.provider_id IS NOT NULL OR w.provider_reference IS NOT NULL))
             OR (w.state = 'submitted' AND (w.release_ledger_transaction_id IS NOT NULL OR w.provider_id IS NULL))
             OR (w.state IN ('cancelled', 'rejected') AND (w.provider_id IS NOT NULL OR w.provider_reference IS NOT NULL))
             OR (w.state IN ('completed', 'failed') AND w.provider_id IS NULL));
        c_columns := c_columns + n;

        -- (3) A / WD031-WD032: the hold link names the exact E0b two-leg transaction.
        SELECT count(*) INTO n FROM withdrawal_requests w
         WHERE w.tenant_id = tenant_rec.id
           AND NOT EXISTS (
               SELECT 1 FROM ledger_transactions t
                WHERE t.id = w.hold_ledger_transaction_id AND t.tenant_id = w.tenant_id
                  AND t.transaction_type = 'withdrawal_requested' AND t.idempotency_key = w.id::text
                  AND t.correlation_id = w.id AND t.reverses_transaction_id IS NULL
                  AND (SELECT count(*) FROM ledger_entries e WHERE e.ledger_transaction_id = t.id) = 2
                  AND EXISTS (SELECT 1 FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
                               WHERE e.ledger_transaction_id = t.id AND e.direction = 'debit'
                                 AND la.account_type = 'player_cash' AND e.wallet_id = w.wallet_id
                                 AND e.asset_code = w.asset_code AND e.amount = w.amount)
                  AND EXISTS (SELECT 1 FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
                               WHERE e.ledger_transaction_id = t.id AND e.direction = 'credit'
                                 AND la.account_type = 'player_withdrawal_hold' AND e.wallet_id = w.wallet_id
                                 AND e.asset_code = w.asset_code AND e.amount = w.amount));
        c_hold_link := c_hold_link + n;

        -- (4) A / WD033: release NULL while non-terminal; on a terminal row the exact release shape.
        SELECT count(*) INTO n FROM withdrawal_requests w
         WHERE w.tenant_id = tenant_rec.id
           AND CASE
                 WHEN w.state IN ('requested', 'pending_review', 'approved', 'submitted') THEN w.release_ledger_transaction_id IS NOT NULL
                 WHEN w.state = 'reversed' THEN true
                 ELSE NOT EXISTS (
                   SELECT 1 FROM ledger_transactions t
                    WHERE t.id = w.release_ledger_transaction_id AND t.tenant_id = w.tenant_id AND t.correlation_id = w.id
                      AND CASE w.state
                            WHEN 'cancelled' THEN t.transaction_type = 'withdrawal_rejected' AND t.idempotency_key = w.id::text || ':cancelled'
                                                  AND t.reverses_transaction_id IS NOT DISTINCT FROM w.hold_ledger_transaction_id
                            WHEN 'rejected'  THEN t.transaction_type = 'withdrawal_rejected'
                                                  AND t.idempotency_key IN (w.id::text || ':rejected', w.id::text || ':kyc_denied', w.id::text || ':governed_hold_released')
                                                  AND t.reverses_transaction_id IS NOT DISTINCT FROM w.hold_ledger_transaction_id
                            WHEN 'failed'    THEN t.transaction_type = 'withdrawal_failed' AND t.idempotency_key = w.id::text || ':failed'
                                                  AND t.reverses_transaction_id IS NOT DISTINCT FROM w.hold_ledger_transaction_id
                            WHEN 'completed' THEN t.transaction_type = 'withdrawal_completed' AND w.provider_id IS NOT NULL
                                                  AND t.provider_id = w.provider_id AND t.idempotency_key = w.provider_id || ':' || t.provider_tx_id
                                                  AND t.reverses_transaction_id IS NULL
                            ELSE false
                          END
                      AND (SELECT count(*) FROM ledger_entries e WHERE e.ledger_transaction_id = t.id) = 2
                      AND EXISTS (SELECT 1 FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
                                   WHERE e.ledger_transaction_id = t.id AND e.direction = 'debit'
                                     AND la.account_type = 'player_withdrawal_hold' AND e.wallet_id = w.wallet_id
                                     AND e.asset_code = w.asset_code AND e.amount = w.amount)
                      AND EXISTS (SELECT 1 FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
                                   WHERE e.ledger_transaction_id = t.id AND e.direction = 'credit'
                                     AND e.asset_code = w.asset_code AND e.amount = w.amount
                                     AND CASE WHEN w.state = 'completed'
                                              THEN la.account_type = 'psp_clearing' AND e.wallet_id IS NULL
                                              ELSE la.account_type = 'player_cash' AND e.wallet_id = w.wallet_id END))
               END;
        c_release := c_release + n;

        -- (5) A / WD034: the request's correlated hold legs net to amount (non-terminal) or 0 (terminal).
        SELECT count(*) INTO n FROM withdrawal_requests w
         WHERE w.tenant_id = tenant_rec.id
           AND (SELECT COALESCE(sum(CASE WHEN e.direction = 'credit' THEN e.amount ELSE -e.amount END), 0)
                  FROM ledger_transactions t
                  JOIN ledger_entries e ON e.ledger_transaction_id = t.id
                  JOIN ledger_accounts la ON la.id = e.ledger_account_id
                 WHERE t.correlation_id = w.id AND t.tenant_id = w.tenant_id
                   AND la.account_type = 'player_withdrawal_hold' AND e.wallet_id = w.wallet_id AND e.asset_code = w.asset_code)
               <> CASE WHEN w.state IN ('requested', 'pending_review', 'approved', 'submitted') THEN w.amount ELSE 0 END;
        c_hold_sum := c_hold_sum + n;

        -- (6) B-i / B-ii: every hold entry is the pinned leg of a named request (type, key, wallet,
        --     asset, amount, direction) and that request links its transaction.
        SELECT count(*) INTO n FROM ledger_entries e
          JOIN ledger_accounts la ON la.id = e.ledger_account_id
         WHERE e.tenant_id = tenant_rec.id AND la.account_type = 'player_withdrawal_hold'
           AND NOT EXISTS (
               SELECT 1 FROM ledger_transactions t
                 JOIN withdrawal_requests w ON w.id = t.correlation_id AND w.tenant_id = t.tenant_id
                WHERE t.id = e.ledger_transaction_id AND t.tenant_id = e.tenant_id
                  AND w.wallet_id = e.wallet_id AND w.asset_code = e.asset_code AND w.amount = e.amount
                  AND CASE t.transaction_type
                        WHEN 'withdrawal_requested' THEN e.direction = 'credit' AND t.idempotency_key = w.id::text
                                                         AND w.hold_ledger_transaction_id = t.id
                        WHEN 'withdrawal_rejected'  THEN e.direction = 'debit'
                                                         AND t.idempotency_key IN (w.id::text || ':cancelled', w.id::text || ':rejected',
                                                                                   w.id::text || ':kyc_denied', w.id::text || ':governed_hold_released')
                                                         AND w.release_ledger_transaction_id = t.id
                        WHEN 'withdrawal_failed'    THEN e.direction = 'debit' AND t.idempotency_key = w.id::text || ':failed'
                                                         AND w.release_ledger_transaction_id = t.id
                        WHEN 'withdrawal_completed' THEN e.direction = 'debit' AND w.provider_id IS NOT NULL
                                                         AND t.provider_id = w.provider_id
                                                         AND t.idempotency_key = w.provider_id || ':' || t.provider_tx_id
                                                         AND w.release_ledger_transaction_id = t.id
                        ELSE false
                      END);
        c_entry := c_entry + n;

        -- (7) invariant #12: every hold account's balance rebuilt from ledger_entries equals the sum
        --     of the non-terminal requests on that wallet and asset (and a non-terminal request on a
        --     wallet/asset with no hold account at all counts too).
        SELECT count(*) INTO n FROM (
            SELECT la.wallet_id, la.asset_code,
                   COALESCE((SELECT sum(CASE WHEN e.direction = 'credit' THEN e.amount ELSE -e.amount END)
                               FROM ledger_entries e WHERE e.ledger_account_id = la.id), 0) AS bal
              FROM ledger_accounts la
             WHERE la.tenant_id = tenant_rec.id AND la.account_type = 'player_withdrawal_hold'
        ) h
        FULL JOIN (
            SELECT w.wallet_id, w.asset_code, sum(w.amount) AS open_sum
              FROM withdrawal_requests w
             WHERE w.tenant_id = tenant_rec.id AND w.state IN ('requested', 'pending_review', 'approved', 'submitted')
             GROUP BY w.wallet_id, w.asset_code
        ) o ON o.wallet_id = h.wallet_id AND o.asset_code = h.asset_code
         WHERE COALESCE(h.bal, 0) <> COALESCE(o.open_sum, 0);
        c_balance := c_balance + n;
    END LOOP;
    PERFORM set_config('app.tenant_id', '', true);

    IF c_reversed + c_columns + c_hold_link + c_release + c_hold_sum + c_entry + c_balance > 0 THEN
        RAISE EXCEPTION 'migration 0132 pre-flight: existing rows violate the withdrawal state machine / hold invariant: reversed=% columns=% hold_link=% release_link=% hold_sum=% hold_entry=% hold_balance=%. Nothing was changed. Rows are never deleted or edited to make this pass; escalate to the human (ADR 0113 s6).',
            c_reversed, c_columns, c_hold_link, c_release, c_hold_sum, c_entry, c_balance
            USING ERRCODE = 'WD098';
    END IF;
END $$;

-- =========================================================================
-- 1. The state-transition guard (ADR 0113 s3).
-- =========================================================================

CREATE FUNCTION withdrawal_requests_state_transition_guard() RETURNS trigger
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

CREATE TRIGGER withdrawal_requests_aa_state_transition_guard
    BEFORE INSERT OR UPDATE ON withdrawal_requests
    FOR EACH ROW EXECUTE FUNCTION withdrawal_requests_state_transition_guard();

-- =========================================================================
-- 2. Check A (ADR 0113 s4.2): the per-request ledger link rules, shared by A and B-ii. Returns NULL
--    when the request satisfies them, else ARRAY[sqlstate, message]. Read-only, no lock, RLS-bound.
-- =========================================================================

CREATE FUNCTION withdrawal_request_ledger_link_violation(p_id uuid, p_tenant uuid) RETURNS text[]
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

-- A: at COMMIT, for every request inserted or updated in the transaction (re-read by id: the
-- event's own tuple may be superseded by a later UPDATE in the same transaction).
CREATE FUNCTION withdrawal_requests_ledger_link_at_commit() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v text[];
BEGIN
    v := withdrawal_request_ledger_link_violation(NEW.id, NEW.tenant_id);
    IF v IS NOT NULL THEN
        RAISE EXCEPTION 'withdrawal_requests %: %', NEW.id, v[2] USING ERRCODE = v[1];
    END IF;
    RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER withdrawal_requests_ledger_link_at_commit
    AFTER INSERT OR UPDATE ON withdrawal_requests
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION withdrawal_requests_ledger_link_at_commit();

-- =========================================================================
-- 3. B-i: every entry on a player_withdrawal_hold account is the pinned leg of a named request in the
--    right pre-state (ADR 0113 s4.2, rev 2). Hot path (C4): a house entry pays one NULL check, a
--    player entry one ledger_accounts primary-key read. No lock.
-- =========================================================================

CREATE FUNCTION ledger_entries_withdrawal_hold_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_type text;
    v_tx   RECORD;
    w      withdrawal_requests%ROWTYPE;
    v_id   text;
    v_ok   boolean;
BEGIN
    IF NEW.wallet_id IS NULL THEN
        RETURN NEW;
    END IF;
    SELECT la.account_type INTO v_type FROM ledger_accounts la WHERE la.id = NEW.ledger_account_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ledger_entries: the account of a wallet entry is not visible' USING ERRCODE = 'WD040';
    END IF;
    IF v_type IS DISTINCT FROM 'player_withdrawal_hold' THEN
        RETURN NEW;
    END IF;

    SELECT t.transaction_type, t.idempotency_key, t.correlation_id, t.provider_id, t.provider_tx_id INTO v_tx
      FROM ledger_transactions t WHERE t.id = NEW.ledger_transaction_id AND t.tenant_id = NEW.tenant_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ledger_entries: the transaction of a withdrawal hold entry is not visible' USING ERRCODE = 'WD040';
    END IF;
    IF v_tx.transaction_type NOT IN ('withdrawal_requested', 'withdrawal_rejected', 'withdrawal_failed', 'withdrawal_completed') THEN
        RAISE EXCEPTION 'ledger_entries: a % transaction may not touch a player_withdrawal_hold account', v_tx.transaction_type
            USING ERRCODE = 'WD040';
    END IF;
    SELECT * INTO w FROM withdrawal_requests r WHERE r.id = v_tx.correlation_id AND r.tenant_id = NEW.tenant_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ledger_entries: the withdrawal request named by a hold entry is not visible' USING ERRCODE = 'WD040';
    END IF;
    v_id := w.id::text;

    IF w.wallet_id IS DISTINCT FROM NEW.wallet_id OR w.asset_code IS DISTINCT FROM NEW.asset_code
       OR w.amount IS DISTINCT FROM NEW.amount OR w.release_ledger_transaction_id IS NOT NULL THEN
        RAISE EXCEPTION 'ledger_entries: a hold entry must match its request''s wallet, asset and amount, before any release'
            USING ERRCODE = 'WD041';
    END IF;
    -- Key pinned per type; Cr only for E0b, Dr only from the edge's own pre-state (stricter than
    -- "non-terminal": each release key is admitted only in the one state its writer posts it from).
    v_ok := CASE v_tx.transaction_type
        WHEN 'withdrawal_requested' THEN NEW.direction = 'credit' AND v_tx.idempotency_key = v_id
                                         AND w.state = 'requested' AND w.hold_ledger_transaction_id IS NULL
        WHEN 'withdrawal_rejected'  THEN NEW.direction = 'debit' AND w.hold_ledger_transaction_id IS NOT NULL
                                         AND ((v_tx.idempotency_key = v_id || ':cancelled' AND w.state = 'requested')
                                           OR (v_tx.idempotency_key = v_id || ':rejected' AND w.state = 'pending_review')
                                           OR (v_tx.idempotency_key IN (v_id || ':kyc_denied', v_id || ':governed_hold_released')
                                               AND w.state = 'approved'))
        WHEN 'withdrawal_failed'    THEN NEW.direction = 'debit' AND w.hold_ledger_transaction_id IS NOT NULL
                                         AND v_tx.idempotency_key = v_id || ':failed' AND w.state = 'submitted'
        WHEN 'withdrawal_completed' THEN NEW.direction = 'debit' AND w.hold_ledger_transaction_id IS NOT NULL
                                         AND w.state = 'submitted' AND w.provider_id IS NOT NULL
                                         AND v_tx.provider_id = w.provider_id
                                         AND v_tx.idempotency_key = w.provider_id || ':' || v_tx.provider_tx_id
        ELSE false
    END;
    IF NOT COALESCE(v_ok, false) THEN
        RAISE EXCEPTION 'ledger_entries: a % hold entry is not the pinned leg of request % in state % (key, direction, provider or pre-state)',
            v_tx.transaction_type, w.id, w.state
            USING ERRCODE = 'WD041';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER ledger_entries_withdrawal_hold_guard
    BEFORE INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_withdrawal_hold_guard();

-- =========================================================================
-- 4. B-ii: at COMMIT, the request named by every hold entry links the entry's transaction (Cr: hold
--    link; Dr: release link) and satisfies A. Catches the standalone second leg whose request is never
--    re-touched (A alone would not fire for it).
-- =========================================================================

CREATE FUNCTION ledger_entries_withdrawal_hold_link_at_commit() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_type text;
    v_corr uuid;
    w      RECORD;
    v      text[];
BEGIN
    IF NEW.wallet_id IS NULL THEN
        RETURN NULL;
    END IF;
    SELECT la.account_type INTO v_type FROM ledger_accounts la WHERE la.id = NEW.ledger_account_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ledger_entries: the account of a wallet entry is not visible at commit' USING ERRCODE = 'WD042';
    END IF;
    IF v_type IS DISTINCT FROM 'player_withdrawal_hold' THEN
        RETURN NULL;
    END IF;
    IF NULLIF(current_setting('app.player_account_id', true), '') IS NOT NULL
       OR COALESCE(NULLIF(current_setting('app.tenant_id', true), ''),
                   NULLIF(current_setting('app.acting_tenant_id', true), '')) IS DISTINCT FROM NEW.tenant_id::text THEN
        RAISE EXCEPTION 'ledger_entries: the session is not the tenant (or acting) scope of the hold entry at commit' USING ERRCODE = 'WD030';
    END IF;
    SELECT t.correlation_id INTO v_corr FROM ledger_transactions t
     WHERE t.id = NEW.ledger_transaction_id AND t.tenant_id = NEW.tenant_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ledger_entries: the transaction of a hold entry is not visible at commit' USING ERRCODE = 'WD042';
    END IF;
    SELECT r.id, r.hold_ledger_transaction_id, r.release_ledger_transaction_id INTO w
      FROM withdrawal_requests r WHERE r.id = v_corr AND r.tenant_id = NEW.tenant_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'ledger_entries: the request of a hold entry is not visible at commit' USING ERRCODE = 'WD042';
    END IF;
    IF (NEW.direction = 'credit' AND w.hold_ledger_transaction_id IS DISTINCT FROM NEW.ledger_transaction_id)
       OR (NEW.direction = 'debit' AND w.release_ledger_transaction_id IS DISTINCT FROM NEW.ledger_transaction_id) THEN
        RAISE EXCEPTION 'ledger_entries: request % does not link the hold leg''s transaction % at commit', w.id, NEW.ledger_transaction_id
            USING ERRCODE = 'WD042';
    END IF;
    v := withdrawal_request_ledger_link_violation(w.id, NEW.tenant_id);
    IF v IS NOT NULL THEN
        RAISE EXCEPTION 'ledger_entries: request % fails check A at commit (%: %)', w.id, v[1], v[2] USING ERRCODE = 'WD042';
    END IF;
    RETURN NULL;
END
$$;

CREATE CONSTRAINT TRIGGER ledger_entries_withdrawal_hold_link_at_commit
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_withdrawal_hold_link_at_commit();

-- =========================================================================
-- 5. The open-request index (ADR 0113 s4.4 / s6 M-A; used by reconciliation D in slice 3).
-- =========================================================================

CREATE INDEX idx_withdrawal_requests_open_wallet_asset ON withdrawal_requests (wallet_id, asset_code)
    WHERE state IN ('requested', 'pending_review', 'approved', 'submitted');
