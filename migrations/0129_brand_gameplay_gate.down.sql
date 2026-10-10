-- Reverses 0129 (ADR 0112 slice 2, LF1). Restores migration 0128's
-- launch_subject_status_guard body byte for byte (the brand lock is removed; every
-- 0128 guard stays installed and unchanged), drops the deferred new-stake brand
-- backstop and the brand lock-key function. No data depends on 0129 (it adds no
-- table, column or row), so the down never refuses. After it, brand status no longer
-- gates new wagering (the pre-slice-2 LF1 gap returns); the tenant gate (0118/0121)
-- is unaffected.

DROP TRIGGER ledger_wager_brand_active_guard ON ledger_transactions;
DROP FUNCTION ledger_wager_brand_active_guard();

-- 0128's body, verbatim (lines 836-890 of 0128's up, CREATE -> CREATE OR REPLACE).
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
BEGIN
    IF NEW.status IS NOT DISTINCT FROM OLD.status THEN
        RETURN NEW;
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
    IF v_prev_tenant IS NOT NULL THEN
        PERFORM set_config('app.tenant_id', v_prev_tenant, true);
    END IF;
    IF v_n <> 1 THEN
        RAISE EXCEPTION '% status change refused: no same-transaction governed transition of an executing request (ADR 0112)', TG_TABLE_NAME
            USING ERRCODE = 'LA020';
    END IF;
    RETURN NEW;
END
$$;

DROP FUNCTION brand_status_gate_key(uuid);
