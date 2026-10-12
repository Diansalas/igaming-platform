-- ADR 0113 SLICE 2 (M-B): the ledger closure arm LA025 (ADR 0113 s4.3; closes ADR 0112 s12 item 14(a)).
-- MONEY-PATH / LAUNCH GOVERNANCE: FOR `ledger-finance`, `security`, `code-reviewer`, `qa` REVIEW BEFORE MERGE.
--
-- NUMBERING: 0133 (gap-free after 0132; no file, branch or doc reference used 0133 at authoring time).
-- Migration numbers are allocated by the orchestrator; confirm before merge.
--
-- SQLSTATE: LA025 (new) - a closure is refused while a player_withdrawal_hold balance of the subject's
-- player wallets is non-zero.
--
-- What it does, and only this:
--   1. idx_ledger_accounts_withdrawal_hold: ledger_accounts (tenant_id) WHERE account_type =
--      'player_withdrawal_hold'. tenant_id and account_type of a ledger_accounts row never change (ledger_accounts_immutable_fields
--      trigger), so the partial index costs one insert per hold account and nothing on the posting path.
--      The per-account balance is then summed from ledger_entries through the existing
--      idx_ledger_entries_account (ledger_account_id, created_at). No index is added to
--      wallet_balance_projection or ledger_entries (no overlap with 0135's
--      idx_ledger_entries_tenant_player_asset_created). Built under SET LOCAL lock_timeout = '5s': the
--      SHARE lock on ledger_accounts fails fast instead of queueing writers behind a long wait.
--   2. launch_subject_status_guard: CREATE OR REPLACE of 0130's body, byte for byte, plus ONE marked
--      block (LA025) inside the closure branch, under the same RLS-bound read window (app.tenant_id
--      bound to the subject's tenant, app.player_account_id cleared, both restored) and under the gate
--      locks the closure already holds (brand: brand_status_gate_key EXCLUSIVE taken by this trigger;
--      tenant: tenant_status_gate_key EXCLUSIVE taken by tenants_status_change_gate in the same
--      statement). The balance is REBUILT FROM ledger_entries (append-only), NOT read from
--      wallet_balance_projection: security review C1 executed, as the runtime role, an UPDATE of a hold
--      projection row (debit_total = credit_total) followed by a committed brand closure; the projection
--      is runtime-writable outside the acting fence, the ledger is not. A tenant closure counts the hold
--      accounts of EVERY brand of the tenant; a brand closure counts only the hold accounts whose wallet
--      belongs to that brand (wallets.brand_id). LA023 (hold-bearing requests) stays and is checked
--      first; then LA024 (brand open bets); then LA025.
--
-- Race (ADR 0113 s4.3): the only hold-INCREASING leg is E0b (B-i, 0132), posted in the transaction of
-- E0, whose insert takes the tenant and brand gate keys SHARED (0131). A closure holds the key of its
-- subject EXCLUSIVE, so either the closure reads the committed hold (refused) or the insert waits and
-- then reads `closed` (WD010 / WD011). Releases only lower a balance.
--
-- Q1 (orchestrator default, ADR 0113 s8, reversible by the owner): the closure is refused even when
-- nothing can resolve the hold yet (a stranded hold with no hold-bearing request, the HD-CTF-4
-- population); suspension is the lever.
--
-- Not SECURITY DEFINER; search_path pinned (unchanged). Literal pin
-- (docs/security/launch-status-readers.md): this file restates 0130's `OLD.status = 'closed'` and
-- `NEW.status = 'closed'` and adds no other status literal or status reader.

-- ---------------------------------------------------------------------------
-- 1. The partial hold-account index.
-- ---------------------------------------------------------------------------
SET LOCAL lock_timeout = '5s';
CREATE INDEX idx_ledger_accounts_withdrawal_hold
    ON ledger_accounts (tenant_id)
    WHERE account_type = 'player_withdrawal_hold';

-- ---------------------------------------------------------------------------
-- 2. launch_subject_status_guard: 0130's body with one addition (LA025).
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION launch_subject_status_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_kind        text;
    v_tenant      uuid;
    v_brand       uuid;
    v_prev_tenant text;
    v_n           bigint;
    v_mixed       bigint := 0;
BEGIN
    IF NEW.status IS NOT DISTINCT FROM OLD.status THEN
        RETURN NEW;
    END IF;
    -- LF1 (ADR 0112 slice 2): a brand status change takes the per-brand gameplay
    -- gate lock EXCLUSIVE first, so it waits for in-flight new-stake placements of
    -- this brand (which hold it SHARED) and every later placement waits for it.
    IF TG_TABLE_NAME = 'brands' THEN
        PERFORM pg_advisory_xact_lock(public.brand_status_gate_key(OLD.id));
    END IF;
    IF OLD.status = 'closed' THEN
        RAISE EXCEPTION '% status: closed is terminal', TG_TABLE_NAME USING ERRCODE = 'LA020';
    END IF;
    IF NOT public.launch_status_move_legal(OLD.status, NEW.status) THEN
        RAISE EXCEPTION '% status: % -> % is not a legal move', TG_TABLE_NAME, OLD.status, NEW.status USING ERRCODE = 'LA020';
    END IF;
    IF TG_TABLE_NAME = 'tenants' THEN
        v_kind := 'tenant'; v_tenant := OLD.id; v_brand := NULL;
    ELSE
        v_kind := 'brand'; v_tenant := OLD.tenant_id; v_brand := OLD.id;
    END IF;

    -- S6 executor shape: a platform principal with app.tenant_id bound to the subject's
    -- own tenant (the brand_tenant_update technique). Reads of the decision records use
    -- the platform policies, so clear the tenant GUC for the reads and restore it.
    v_prev_tenant := NULLIF(current_setting('app.tenant_id', true), '');
    IF v_prev_tenant IS NOT NULL
       AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL THEN
        PERFORM set_config('app.tenant_id', '', true);
    ELSE
        v_prev_tenant := NULL;
    END IF;
    SELECT count(*) INTO v_n
      FROM public.launch_status_transitions t
      JOIN public.launch_authorisation_requests r ON r.id = t.request_id AND r.tenant_id = t.tenant_id
     WHERE t.tenant_id = v_tenant AND t.subject_kind = v_kind AND t.brand_id IS NOT DISTINCT FROM v_brand
       AND t.kind = 'governed' AND t.txid = txid_current()
       AND t.from_status = OLD.status AND t.to_status = NEW.status
       AND r.status = 'executing' AND r.executing_txid = txid_current()
       AND r.subject_kind = v_kind AND r.brand_id IS NOT DISTINCT FROM v_brand
       AND r.from_status = OLD.status AND r.to_status = NEW.status AND r.action <> 'ratify';
    -- C2 (security, ADR 0112 13): one subject per executing transaction; a tenant status
    -- change after a governed status change of one of its brands in this transaction is
    -- refused (brand key then tenant key inverts the placement lock order).
    IF v_kind = 'tenant' THEN
        SELECT count(*) INTO v_mixed
          FROM public.launch_status_transitions t
         WHERE t.tenant_id = v_tenant AND t.subject_kind = 'brand'
           AND t.kind = 'governed' AND t.txid = txid_current() AND t.from_status <> t.to_status;
    END IF;
    IF v_prev_tenant IS NOT NULL THEN
        PERFORM set_config('app.tenant_id', v_prev_tenant, true);
    END IF;
    IF v_n <> 1 THEN
        RAISE EXCEPTION '% status change refused: no same-transaction governed transition of an executing request (ADR 0112)', TG_TABLE_NAME
            USING ERRCODE = 'LA020';
    END IF;
    IF v_mixed > 0 THEN
        RAISE EXCEPTION 'tenants status change refused: a brand status change of this tenant was already executed in this transaction (one subject per executing transaction)'
            USING ERRCODE = 'LA020';
    END IF;
    -- CLOSURE BACKSTOP (security C1 / ledger-finance condition 2; ADR 0112 invariant 11): a
    -- closure is refused while withdrawal requests that still hold the players' funds exist
    -- (LA023) and, for a brand, while open sportsbook bets of that brand exist (LA024; a
    -- tenant's own open bets are refused by tenants_status_change_gate, GP020). Both reads
    -- are RLS-bound, so app.tenant_id is bound to the subject's tenant for the read and
    -- restored (the 0121 technique; TestClosureBackstop_* fail if the read goes blind). The
    -- reads run under the gate lock this trigger already holds (brand key above; the tenant
    -- key is taken by tenants_status_change_gate in the same statement). A request inserted
    -- by a concurrent transaction that has not yet committed is not visible: the residual is
    -- recorded in ADR 0112 (withdrawal creation does not take the gate lock).
    IF NEW.status = 'closed' THEN
        DECLARE
            v_prev_t text := current_setting('app.tenant_id', true);
            v_prev_p text := current_setting('app.player_account_id', true);
            v_wd     bigint;
            v_bets   bigint := 0;
            v_holds  bigint := 0;
        BEGIN
            PERFORM set_config('app.tenant_id', v_tenant::text, true);
            PERFORM set_config('app.player_account_id', '', true);
            SELECT count(*) INTO v_wd FROM public.withdrawal_requests w
             WHERE w.tenant_id = v_tenant AND (v_brand IS NULL OR w.brand_id = v_brand)
               AND w.state IN ('requested', 'pending_review', 'approved', 'submitted');
            IF v_kind = 'brand' THEN
                SELECT count(*) INTO v_bets FROM public.sportsbook_bets b
                 WHERE b.tenant_id = v_tenant AND b.brand_id = v_brand AND b.status = 'open';
            END IF;
            -- LA025 (ADR 0113 s4.3, migration 0133): the ledger closure arm. Non-zero
            -- player_withdrawal_hold balances of the subject's player wallets, REBUILT FROM
            -- ledger_entries (never the runtime-writable projection) inside this RLS-bound window; a
            -- brand closure joins wallets.brand_id (RLS-bound too).
            SELECT count(*) INTO v_holds FROM public.ledger_accounts la
             WHERE la.tenant_id = v_tenant AND la.account_type = 'player_withdrawal_hold'
               AND (v_brand IS NULL OR EXISTS (
                    SELECT 1 FROM public.wallets wl
                     WHERE wl.id = la.wallet_id AND wl.tenant_id = v_tenant AND wl.brand_id = v_brand))
               AND (SELECT COALESCE(sum(CASE e.direction WHEN 'credit' THEN e.amount ELSE -e.amount END), 0)
                      FROM public.ledger_entries e
                     WHERE e.ledger_account_id = la.id AND e.tenant_id = v_tenant) <> 0;
            PERFORM set_config('app.tenant_id', COALESCE(v_prev_t, ''), true);
            PERFORM set_config('app.player_account_id', COALESCE(v_prev_p, ''), true);
            IF v_wd > 0 THEN
                RAISE EXCEPTION '% closure refused: withdrawal requests still hold player funds', TG_TABLE_NAME
                    USING ERRCODE = 'LA023', DETAIL = format('hold_bearing_withdrawals=%s', v_wd);
            END IF;
            IF v_bets > 0 THEN
                RAISE EXCEPTION 'brand closure refused: open sportsbook bets exist'
                    USING ERRCODE = 'LA024', DETAIL = format('open_sportsbook_bets=%s', v_bets);
            END IF;
            IF v_holds > 0 THEN
                RAISE EXCEPTION '% closure refused: player withdrawal hold balances are not zero', TG_TABLE_NAME
                    USING ERRCODE = 'LA025', DETAIL = format('withdrawal_hold_accounts_non_zero=%s', v_holds);
            END IF;
        END;
    END IF;
    RETURN NEW;
END
$$;
