-- R3-GAME-POSTINGS-NONACTIVE-1 (owner decision 2026-10-05; ADR 0095 section
-- 40.4 item 2; engineering design recorded in ADR 0095 section 40.5).
--
-- NUMBERING: 0118 is reserved for another workstream; until it merges
-- `migrate verify` on this branch alone shows a gap at 0118. Never commit a
-- 0118 file from here.
--
-- What this does (and ONLY this): makes "a NEW gameplay money movement for a
-- suspended/closed tenant" impossible to race with a status change.
--
-- Why a migration at all: the application cannot lock the tenants row. A
-- `SELECT ... FOR SHARE` on tenants under the runtime role also applies the
-- UPDATE policy (tenants_platform_admin_update), which a tenant-scoped
-- connection never satisfies, so the row is silently filtered out (verified
-- empirically on this schema). A plain read of tenants.status is therefore a
-- check-then-act race against a concurrent status change. The race-free
-- primitive here is a per-tenant ADVISORY lock pair:
--   * a gameplay posting takes it SHARED, then reads tenants.status (a fresh
--     statement, so it sees every committed status change);
--   * a status change takes it EXCLUSIVE (BEFORE UPDATE trigger on tenants)
--     and holds it to the end of the changing transaction.
-- So either the status change commits first and the posting reads the new
-- status, or the posting holds the shared lock and the status change waits
-- until the posting transaction ends.
--
--   1. tenant_status_gate_key(uuid): the one lock-key derivation (the Go
--      helper internal/tenant.RequireActiveForGameplay calls it too).
--   2. tenants_status_change_gate: BEFORE UPDATE OF status trigger on tenants.
--   3. ledger_gameplay_tenant_active_guard: defence-in-depth BEFORE INSERT
--      trigger on ledger_transactions for the gameplay types ONLY
--      (casino_bet, casino_win, casino_rollback, sportsbook_bet,
--      sportsbook_settlement, sportsbook_void, sportsbook_rollback). It takes
--      the same shared lock, reads the status and refuses (SQLSTATE GP010)
--      unless it is 'active'. Deposits, withdrawals, manual adjustments,
--      bonus and tombstone types are NOT covered: payments and staff
--      resolution on a non-active tenant are unchanged (owner decision).
--      A replay of an already-posted transaction (same tenant and idempotency
--      key) is let through: ledger.Post answers a redelivery with an INSERT
--      that conflicts on the unique key and writes nothing, and BEFORE INSERT
--      triggers fire before that check. Existing ledger rows are never
--      touched.
--
-- Every function pins search_path (ADR 0108 / security H-1).

CREATE FUNCTION tenant_status_gate_key(p_tenant_id uuid) RETURNS bigint
    LANGUAGE sql IMMUTABLE PARALLEL SAFE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT hashtextextended('tenant_status_gate:' || p_tenant_id::text, 0)
$$;

CREATE FUNCTION tenants_status_change_gate() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF NEW.status IS DISTINCT FROM OLD.status THEN
        PERFORM pg_advisory_xact_lock(public.tenant_status_gate_key(OLD.id));
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER tenants_status_change_gate
    BEFORE UPDATE OF status ON tenants
    FOR EACH ROW EXECUTE FUNCTION tenants_status_change_gate();

CREATE FUNCTION ledger_gameplay_tenant_active_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_status text;
BEGIN
    -- A replay: ledger.Post answers an idempotent redelivery with an INSERT
    -- that hits the (tenant_id, idempotency_key) unique index and writes
    -- nothing. BEFORE INSERT triggers fire before that unique check, so the
    -- replay must be let through here (ledger rows are immutable, so an
    -- existing row proves the insert will conflict). This is a read, not a
    -- new movement. Anything else falls through to the gate.
    IF EXISTS (SELECT 1 FROM public.ledger_transactions x
                WHERE x.tenant_id = NEW.tenant_id AND x.idempotency_key = NEW.idempotency_key) THEN
        RETURN NEW;
    END IF;
    PERFORM pg_advisory_xact_lock_shared(public.tenant_status_gate_key(NEW.tenant_id));
    SELECT t.status INTO v_status FROM public.tenants t WHERE t.id = NEW.tenant_id;
    IF v_status IS DISTINCT FROM 'active' THEN
        -- Fail closed also when the row is not visible (a missing status is
        -- never a licence to post).
        RAISE EXCEPTION 'gameplay posting refused: tenant is not active'
            USING ERRCODE = 'GP010';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER ledger_gameplay_tenant_active_guard
    BEFORE INSERT ON ledger_transactions
    FOR EACH ROW
    WHEN (NEW.transaction_type IN (
        'casino_bet', 'casino_win', 'casino_rollback',
        'sportsbook_bet', 'sportsbook_settlement', 'sportsbook_void', 'sportsbook_rollback'))
    EXECUTE FUNCTION ledger_gameplay_tenant_active_guard();
