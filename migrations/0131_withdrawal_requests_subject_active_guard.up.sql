-- ADR 0112 slice 4 / slice-3 review residual 14(b): a database-level status guard on the CREATION
-- of a withdrawal request. MONEY-PATH TRIGGER: FOR `ledger-finance` REVIEW BEFORE MERGE.
--
-- NUMBERING: 0131 (gap-free after 0130). Migration numbers are allocated by the orchestrator;
-- confirm before merge.
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
-- What this does (and ONLY this):
--   withdrawal_requests_subject_active_guard: BEFORE INSERT ... FOR EACH ROW on withdrawal_requests.
--     1. A REPLAY (the same tenant, player and idempotency key already exist) is let through: the
--        INSERT then conflicts on withdrawal_requests_tenant_player_idempotency_key and writes
--        nothing (BEFORE INSERT triggers fire before that unique check; the same reasoning as 0118).
--        withdrawal.RequestWithdrawal already answers a replay of an existing request on a non-active
--        subject read-only (ADR 0096 s8 item 3), before this INSERT is ever attempted.
--     2. Otherwise it takes the tenant status gate key SHARED (tenant_status_gate_key, 0118), reads
--        tenants.status in a fresh statement and refuses (SQLSTATE WD010) unless 'active'; then it
--        takes the brand status gate key SHARED (brand_status_gate_key, 0129), reads brands.status
--        for (brand_id, tenant_id) and refuses (SQLSTATE WD011) unless 'active'. Lock order is the
--        ADR 0112 order: tenant key, then brand key, before any ledger lock (the hold posting runs
--        after this INSERT). A subject row that is not visible to the session is NOT a licence to
--        insert: the read yields no status and the guard refuses (fail closed).
--     A governed status change takes the same keys EXCLUSIVE (tenants_status_change_gate for the
--     tenant, launch_subject_status_guard for the brand) and holds them to the end of its
--     transaction, so either the change commits first and this guard reads the new status, or the
--     insert holds the key and the change waits (then the LA023 closure backstop of 0130 counts the
--     committed request and refuses the closure). Both orders were race-tested.
--
-- What it does NOT do: it does not touch an existing row, a state transition, the hold posting, any
-- balance or projection, or the owner (there is no owner exemption: the owner role is bound by the
-- same refusal). A withdrawal that exists keeps progressing on a suspended subject (approve, reject,
-- cancel, hold release): this guard fires on INSERT only.
--
-- Trigger name and order: PostgreSQL fires same-event BEFORE row triggers in name order. The name
-- carries an "aa" infix so the status refusal fires BEFORE withdrawal_requests_payout_binding_guard
-- (0123/0126, which takes the payout instrument FOR SHARE): the advisory status keys are taken before
-- any row lock, and a refusal on a non-active subject never depends on the instrument. A catalog
-- test pins the order.
--
-- Every function pins search_path (ADR 0108 / security H-1); none is SECURITY DEFINER.

CREATE FUNCTION withdrawal_requests_subject_active_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_status text;
BEGIN
    IF EXISTS (SELECT 1 FROM public.withdrawal_requests x
                WHERE x.tenant_id = NEW.tenant_id
                  AND x.player_account_id = NEW.player_account_id
                  AND x.idempotency_key = NEW.idempotency_key) THEN
        RETURN NEW;
    END IF;

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
