-- Reverses 0121: restores the exact 0118 behaviour of both functions and
-- drops the deferred rollback/void constraint trigger. Existing ledger and
-- tenant rows are untouched. WARNING (DEV/CI ONLY): after this, terminal
-- stake returns are refused again on non-active tenants and a tenant can be
-- closed with open sportsbook bets.
DROP TRIGGER IF EXISTS ledger_sportsbook_rollback_requires_void ON ledger_transactions;
DROP FUNCTION IF EXISTS ledger_sportsbook_rollback_requires_void();

CREATE OR REPLACE FUNCTION tenants_status_change_gate() RETURNS trigger
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

CREATE OR REPLACE FUNCTION ledger_gameplay_tenant_active_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_status text;
BEGIN
    IF EXISTS (SELECT 1 FROM public.ledger_transactions x
                WHERE x.tenant_id = NEW.tenant_id AND x.idempotency_key = NEW.idempotency_key) THEN
        RETURN NEW;
    END IF;
    PERFORM pg_advisory_xact_lock_shared(public.tenant_status_gate_key(NEW.tenant_id));
    SELECT t.status INTO v_status FROM public.tenants t WHERE t.id = NEW.tenant_id;
    IF v_status IS DISTINCT FROM 'active' THEN
        RAISE EXCEPTION 'gameplay posting refused: tenant is not active'
            USING ERRCODE = 'GP010';
    END IF;
    RETURN NEW;
END
$$;
