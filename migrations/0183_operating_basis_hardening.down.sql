-- Down of 0183 (DEV/STAGING ONLY): restores the 0180 guard, conflict function and read policies byte for byte; no data is lost.
SET LOCAL lock_timeout = '5s';

ALTER TABLE operating_bases DROP CONSTRAINT operating_bases_no_long_digit_runs, DROP CONSTRAINT operating_bases_note_charset, DROP CONSTRAINT operating_bases_ref_charset;
DROP TRIGGER operating_bases_close_requires_successor ON operating_bases;
DROP FUNCTION operating_bases_close_requires_successor();
CREATE OR REPLACE FUNCTION operating_bases_governed_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_new      jsonb := pg_catalog.to_jsonb(NEW);
    v_old      jsonb;
    v_req      RECORD;
    v_pred     public.operating_bases%ROWTYPE;
    v_widening boolean;
    v_excl     text[];
BEGIN
    IF TG_OP = 'UPDATE' THEN
        v_old := pg_catalog.to_jsonb(OLD);
    END IF;
    SELECT r.id, r.kind, r.widening, r.required_approvals, r.executed_by, k.widening_approvals INTO v_req
      FROM public.licensing_change_requests r
      JOIN public.licensing_change_kinds k ON k.kind = r.kind
     WHERE r.status = 'executed' AND r.executed_txid = pg_catalog.txid_current()
       AND public.licensing_change_row_matches(r.kind, r.payload, r.reason_code, r.subject_tenant_id, TG_TABLE_NAME, TG_OP, v_new, v_old)
     ORDER BY r.executed_at DESC, r.id
     LIMIT 1;
    IF NOT FOUND THEN
        IF public.launch_is_table_owner() THEN
            IF TG_OP = 'INSERT' THEN
                NEW.change_request_id := NULL;
                NEW.created_by_actor_id := NULL;
            END IF;
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'operating_bases: a % needs a licensing change request of the matching kind executed in this transaction (lic9, migration 0180)', TG_OP
            USING ERRCODE = 'LC040';
    END IF;
    IF TG_OP = 'UPDATE' THEN
        RETURN NEW;
    END IF;
    NEW.change_request_id := v_req.id;
    NEW.created_by_actor_id := v_req.executed_by;
    IF v_req.kind IN ('operating_basis_withdraw', 'operating_basis_review_record') THEN
        -- The predecessor is the version closed in this very transaction (its effective_to equals this row's effective_from).
        SELECT * INTO v_pred FROM public.operating_bases b
         WHERE b.tenant_id = NEW.tenant_id AND b.brand_id IS NOT DISTINCT FROM NEW.brand_id AND b.country_code = NEW.country_code
           AND b.operation_code = NEW.operation_code AND b.product_code IS NOT DISTINCT FROM NEW.product_code
           AND b.effective_to = NEW.effective_from AND b.id <> NEW.id
         ORDER BY b.created_at DESC LIMIT 1;
        IF NOT FOUND OR v_pred.status <> 'active' THEN
            RAISE EXCEPTION 'operating_bases: a withdrawal or review record needs the active predecessor version closed in the same transaction' USING ERRCODE = 'LC043';
        END IF;
        v_excl := ARRAY['id', 'status', 'reason_code', 'policy_version', 'effective_from', 'effective_to', 'created_at', 'created_by_actor_id', 'change_request_id'];
        IF v_req.kind = 'operating_basis_review_record' THEN
            v_excl := v_excl || ARRAY['review_due_at', 'needs_rereview', 'last_review_reference'];
            v_widening := (v_pred.needs_rereview AND NOT NEW.needs_rereview)
                          OR (v_pred.review_due_at IS NOT NULL AND NEW.review_due_at > v_pred.review_due_at);
            IF v_widening AND NOT (v_req.widening AND v_req.required_approvals >= v_req.widening_approvals) THEN
                RAISE EXCEPTION 'operating_bases: a review record that extends the permission needs a widening request that holds its approvals' USING ERRCODE = 'LC040';
            END IF;
        END IF;
        IF (v_new - v_excl) IS DISTINCT FROM (pg_catalog.to_jsonb(v_pred) - v_excl) THEN
            RAISE EXCEPTION 'operating_bases: the new version differs from its predecessor beyond what the change kind may alter' USING ERRCODE = 'LC043';
        END IF;
    ELSIF NOT (v_req.widening AND v_req.required_approvals >= v_req.widening_approvals) THEN
        RAISE EXCEPTION 'operating_bases: operating_basis_set is widening and needs its approvals' USING ERRCODE = 'LC040';
    END IF;
    RETURN NEW;
END
$$;


DROP POLICY licence_country_ceilings_read ON licence_country_ceilings;
CREATE POLICY licence_country_ceilings_read ON licence_country_ceilings
    FOR SELECT USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            (
                NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
                AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
            )
            OR EXISTS (
                SELECT 1 FROM tenants t
                 WHERE t.id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
                   AND t.licence_id = licence_country_ceilings.licence_id
            )
            OR EXISTS (
                SELECT 1 FROM operating_bases b
                 WHERE b.tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
                   AND b.licence_id = licence_country_ceilings.licence_id
            )
        )
    );

DROP POLICY licences_read ON licences;
CREATE POLICY licences_read ON licences
    FOR SELECT USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND (
            (
                NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
                AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
            )
            OR EXISTS (
                SELECT 1 FROM tenants t
                 WHERE t.id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
                   AND t.licence_id = licences.id
            )
            OR EXISTS (
                SELECT 1 FROM operating_bases b
                 WHERE b.tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
                   AND b.licence_id = licences.id
            )
        )
    );

DROP TRIGGER tenants_licence_basis_exclusive ON tenants;
DROP FUNCTION tenants_licence_basis_exclusive();
CREATE OR REPLACE FUNCTION operating_bases_licence_conflict(p_licence uuid, p_tenant uuid) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT EXISTS (SELECT 1 FROM public.licences l WHERE l.id = p_licence AND l.licensee = 'tenant')
       AND (EXISTS (SELECT 1 FROM public.tenants t WHERE t.licence_id = p_licence AND t.id <> p_tenant)
            OR EXISTS (SELECT 1 FROM public.operating_bases b WHERE b.licence_id = p_licence AND b.tenant_id <> p_tenant
                        AND b.effective_to IS NULL AND b.status = 'active'))
$$;

DROP POLICY licences_definer_read ON licences;
DROP POLICY operating_bases_definer_read ON operating_bases;
