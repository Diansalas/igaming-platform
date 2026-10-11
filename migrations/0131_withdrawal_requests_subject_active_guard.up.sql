-- ADR 0112 slice 4 / slice-3 review residuals 14(b) and 14(b2): database guards on the CREATION and the
-- DELETION of withdrawal requests. MONEY-PATH TRIGGERS: FOR `ledger-finance` REVIEW BEFORE MERGE.
--
-- NUMBERING: 0131 (gap-free after 0130). Migration numbers are allocated by the orchestrator;
-- confirm before merge. SQLSTATEs WD010 / WD011 / WD012: the class "WD" is used by no other migration or
-- Go file (grep verified at authoring time).
--
-- Why. H-SEC-11 (Go, internal/withdrawal.RequestWithdrawal -> tenant.RequireActiveForPaymentInitiation)
-- refuses a NEW withdrawal request for a tenant or brand that is not 'active', under the tenant status
-- gate key taken SHARED and a brand row FOR SHARE, so it serialises with a closure (LA023 backstop,
-- 0130). That is enforcement at the Go layer only: a RAW INSERT into withdrawal_requests by the
-- runtime credential on a pending_launch / suspended / closed subject is accepted by the database
-- today (slice-3 residual 14(b), MEDIUM defence in depth), and a request inserted that way is
-- exactly the one the closure guard races against. This mirrors what 0118 does for ledger postings
-- (ledger_gameplay_tenant_active_guard) and 0129 does for the brand (ledger_wager_brand_active_guard).
--
-- 1. withdrawal_requests_subject_active_guard: BEFORE INSERT ... FOR EACH ROW on withdrawal_requests.
--    It takes the tenant status gate key SHARED (tenant_status_gate_key, 0118), reads tenants.status in
--    a fresh statement and refuses (SQLSTATE WD010) unless 'active'; then it takes the brand status gate
--    key SHARED (brand_status_gate_key, 0129), reads brands.status for (brand_id, tenant_id) and refuses
--    (SQLSTATE WD011) unless 'active'. A subject row that is not visible to the session is NOT a licence
--    to insert: the read yields no status and the guard refuses (fail closed).
--    NO REPLAY SHORT-CIRCUIT (security C1, a live exploit of the first draft): letting an INSERT through
--    because a row with the same (tenant, player, idempotency key) already exists is unsound, because
--    igaming_runtime may DELETE that row in a still-open transaction and a raw INSERT on a SUSPENDED
--    brand then skipped the guard and was accepted once the delete committed. The guard therefore runs
--    for every INSERT. A genuine replay never reaches it: withdrawal.RequestWithdrawal answers a replay
--    read-only BEFORE it inserts (ADR 0096 s8 item 3), and a concurrent first-insert race on a
--    non-active subject is refused, which is the fail-closed answer.
--    A governed status change takes the same keys EXCLUSIVE (tenants_status_change_gate for the
--    tenant, launch_subject_status_guard for the brand) and holds them to the end of its
--    transaction, so either the change commits first and this guard reads the new status, or the
--    insert holds the key and the change waits (then the LA023 closure backstop of 0130 counts the
--    committed request and refuses the closure). Both orders were race-tested as igaming_runtime.
--
-- 2. withdrawal_requests_deny_delete (WD012): BEFORE DELETE (row) and BEFORE TRUNCATE (statement). A
--    withdrawal request is never deleted (the state machine ends in a terminal state; the hold is
--    released by a compensating ledger entry). No production code deletes it, and igaming_runtime had
--    DELETE only through the blanket default grant. The trigger binds EVERY role including the owner
--    (no exemption: no test fixture needs one; a scratch database is dropped, not emptied), and closes
--    the "delete the row inside the closure transaction" bypass of residual 14(b2). The deploy grant
--    script also revokes DELETE and TRUNCATE from igaming_runtime (defence in depth; deploy/init-app-role.sql).
--
-- What this does NOT do: it does not touch an existing row, a state transition, the hold posting, any
-- balance or projection. A withdrawal that exists keeps progressing on a suspended subject (approve,
-- reject, cancel, hold release): the status guard fires on INSERT only. A database-level withdrawal
-- STATE-transition trigger (the rest of 14(b2)) is NOT IMPLEMENTED.
--
-- Lock order. Through the Go path a request takes: the tenant status key SHARED and the brand row FOR
-- SHARE (H-SEC-11), then (this trigger) the tenant and brand keys SHARED again, then the payout
-- instrument FOR SHARE (0126 guard), then the ledger locks of the hold posting. A status writer holds the
-- brand/tenant row lock and then the key EXCLUSIVE and takes nothing of the above afterwards, so the pair
-- cannot invert. Through a raw insert (no Go gate) the order is the two keys, then the instrument row.
--
-- Trigger name and order: PostgreSQL fires same-event BEFORE row triggers in name order. The name
-- carries an "aa" infix so the status refusal fires BEFORE withdrawal_requests_payout_binding_guard
-- (0123/0126, which takes the payout instrument FOR SHARE): a refusal on a non-active subject never
-- depends on the instrument. A catalog test pins the order.
--
-- Every function pins search_path (ADR 0108 / security H-1); none is SECURITY DEFINER.

CREATE FUNCTION withdrawal_requests_subject_active_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_status text;
BEGIN
    PERFORM pg_advisory_xact_lock_shared(public.tenant_status_gate_key(NEW.tenant_id));
    SELECT t.status INTO v_status FROM public.tenants t WHERE t.id = NEW.tenant_id;
    IF v_status IS DISTINCT FROM 'active' THEN
        -- Also fails closed when the tenant row is not visible (a missing status is never a licence).
        RAISE EXCEPTION 'withdrawal request refused: tenant is not active'
            USING ERRCODE = 'WD010';
    END IF;

    PERFORM pg_advisory_xact_lock_shared(public.brand_status_gate_key(NEW.brand_id));
    SELECT b.status INTO v_status FROM public.brands b WHERE b.id = NEW.brand_id AND b.tenant_id = NEW.tenant_id;
    IF v_status IS DISTINCT FROM 'active' THEN
        RAISE EXCEPTION 'withdrawal request refused: brand is not active'
            USING ERRCODE = 'WD011';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER withdrawal_requests_aa_subject_active_guard
    BEFORE INSERT ON withdrawal_requests
    FOR EACH ROW EXECUTE FUNCTION withdrawal_requests_subject_active_guard();

CREATE FUNCTION withdrawal_requests_deny_delete() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    RAISE EXCEPTION 'withdrawal_requests: rows are never deleted or truncated'
        USING ERRCODE = 'WD012';
END
$$;

CREATE TRIGGER withdrawal_requests_deny_delete
    BEFORE DELETE ON withdrawal_requests
    FOR EACH ROW EXECUTE FUNCTION withdrawal_requests_deny_delete();

CREATE TRIGGER withdrawal_requests_deny_truncate
    BEFORE TRUNCATE ON withdrawal_requests
    FOR EACH STATEMENT EXECUTE FUNCTION withdrawal_requests_deny_delete();
