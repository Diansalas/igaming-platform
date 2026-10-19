-- 0178: licence lifecycle WIDENING changes through the licensing change primitive (lic4 slice 4a; audit gaps G-1 / G-2 for the
-- lifecycle writers that migration 0176 left single actor). SECURITY-SENSITIVE: for `security`, `architect`, `code-reviewer`.
--
-- Follows the 6-step extension recipe of docs/plans/lic2-maker-checker-record.md section 6:
--   1. four licensing_change_kinds rows (platform_only, 2 approvals, platform_admin: the interim HDR-M-1 answer);
--   2. licensing_change_classify branches: the DATABASE computes widening against the locked current licence row
--      (status toward active; later or cleared expires_at; earlier or cleared issued_at; postponed or cleared review_due_at;
--      an unrecognised stored status is widening); a no-op is LC010; the evidence reference is neutral;
--   3. a licensing_change_row_matches branch for licences UPDATE (only the columns of the kind may change);
--   4. BEFORE UPDATE trigger zz_licensing_change_governed_upd on licences (licences_update_guard): identity columns immutable for
--      every role (LC042); a non-owner UPDATE needs an executed request of the matching kind in the same transaction (LC040),
--      and a widening row change needs a widening request that holds its approvals. The table owner keeps its path for the
--      lifecycle columns (fixtures, seeding), never for the identity columns.
--   5. Go: internal/licensingchange/licence_lifecycle_kinds.go register()s the four kinds.
-- Narrowing and neutral changes requested by a platform admin execute in the same transaction (0 approvals, single actor).
--
-- Security review secrev3 (docs/plans/licensing-security-review-record.md) folded into this migration:
--   F-1  licences_update_guard (above).
--   F-2  licence_versions: version_no bigint, INSERT policy requires pg_trigger_depth() > 0, INSERT revoked from igaming_runtime,
--        SECURITY DEFINER writer that records changed_by only for an ACTIVE platform staff row.
--   F-3  alert kinds licence.sweep_failed (p1) and licence.unmonitored (p2) (the 0176 dispatcher-only pattern).
-- The only grant change is the REVOKE of INSERT on licence_versions from igaming_runtime. No role, password or attribute change.

SET LOCAL lock_timeout = '5s';

INSERT INTO licensing_change_kinds (kind, platform_only, widening_approvals, approver_roles, description) VALUES
    ('licence_status_change', true, 2, ARRAY['platform_admin'], 'Set licences.status (widening iff it moves toward active, narrowing is single actor).'),
    ('licence_validity_dates_change', true, 2, ARRAY['platform_admin'], 'Set licences.issued_at / expires_at (widening iff expiry is later or cleared, or issue date earlier or cleared).'),
    ('licence_review_due_change', true, 2, ARRAY['platform_admin'], 'Set licences.review_due_at (widening iff postponed or cleared).'),
    ('licence_evidence_reference_change', true, 2, ARRAY['platform_admin'], 'Set licences.evidence_reference (neutral, never widening, single actor).');

CREATE OR REPLACE FUNCTION licensing_change_classify(p_kind text, p jsonb,
    OUT widening boolean, OUT subject_tenant uuid, OUT subject_licence uuid)
    LANGUAGE plpgsql VOLATILE
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_open   public.tenant_jurisdiction_configs%ROWTYPE;
    v_lic    public.licences%ROWTYPE;
    v_new_issued  date;
    v_new_expires date;
    v_new_review  date;
    v_rank_old int;
    v_rank_new int;
    v_w      boolean;
    v_state  text := p ->> 'state';
    v_status text := p ->> 'status';
    v_scope  text := p ->> 'scope';
BEGIN
    IF p_kind = 'licence_create' THEN
        IF NOT public.licensing_change_keys_are(p, ARRAY['jurisdiction_id', 'licensee', 'licence_number', 'permitted_products', 'permitted_markets'])
           OR NOT public.licensing_change_is_uuid(p -> 'jurisdiction_id')
           OR NOT COALESCE(p ->> 'licensee' IN ('platform', 'tenant'), false)
           OR NOT public.licensing_change_is_text(p -> 'licence_number', 200)
           OR NOT public.licensing_change_is_text_array(p -> 'permitted_products', 64)
           OR NOT public.licensing_change_is_text_array(p -> 'permitted_markets', 64) THEN
            RAISE EXCEPTION 'licensing change: invalid licence_create payload' USING ERRCODE = 'LC010';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM public.jurisdictions j WHERE j.id = (p ->> 'jurisdiction_id')::uuid) THEN
            RAISE EXCEPTION 'licensing change: unknown jurisdiction' USING ERRCODE = 'LC010';
        END IF;
        widening := true;
        RETURN;
    END IF;

    IF p_kind IN ('tenant_licence_bind', 'tenant_licence_unbind') THEN
        IF (p_kind = 'tenant_licence_bind' AND (NOT public.licensing_change_keys_are(p, ARRAY['tenant_id', 'licence_id'])
                                                OR NOT public.licensing_change_is_uuid(p -> 'licence_id')))
           OR (p_kind = 'tenant_licence_unbind' AND NOT public.licensing_change_keys_are(p, ARRAY['tenant_id']))
           OR NOT public.licensing_change_is_uuid(p -> 'tenant_id') THEN
            RAISE EXCEPTION 'licensing change: invalid % payload', p_kind USING ERRCODE = 'LC010';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM public.tenants t WHERE t.id = (p ->> 'tenant_id')::uuid) THEN
            RAISE EXCEPTION 'licensing change: unknown tenant' USING ERRCODE = 'LC010';
        END IF;
        subject_tenant := (p ->> 'tenant_id')::uuid;
        IF p_kind = 'tenant_licence_bind' THEN
            IF NOT EXISTS (SELECT 1 FROM public.licences l WHERE l.id = (p ->> 'licence_id')::uuid) THEN
                RAISE EXCEPTION 'licensing change: unknown licence' USING ERRCODE = 'LC010';
            END IF;
            subject_licence := (p ->> 'licence_id')::uuid;
            widening := true;
        ELSE
            widening := false;
        END IF;
        RETURN;
    END IF;

    IF p_kind IN ('licence_ceiling_version', 'operating_policy_version') THEN
        IF (p_kind = 'licence_ceiling_version'
                AND (NOT public.licensing_change_keys_are(p, ARRAY['licence_id', 'country_code', 'state', 'status', 'authorization_reference'])
                     OR NOT public.licensing_change_is_uuid(p -> 'licence_id')))
           OR (p_kind = 'operating_policy_version'
                AND (NOT public.licensing_change_keys_are(p, ARRAY['tenant_id', 'scope', 'brand_id', 'operation_code', 'product_code',
                                                                    'country_code', 'state', 'status', 'authorization_reference'])
                     OR NOT public.licensing_change_is_uuid(p -> 'tenant_id')
                     OR NOT COALESCE(v_scope IN ('tenant', 'brand', 'operation'), false)
                     OR NOT (COALESCE(pg_catalog.jsonb_typeof(p -> 'brand_id') = 'null', false) OR public.licensing_change_is_uuid(p -> 'brand_id'))
                     OR NOT public.licensing_change_is_opt_text(p -> 'operation_code', 64)
                     OR NOT public.licensing_change_is_opt_text(p -> 'product_code', 64)
                     OR (v_scope = 'tenant' AND (p ->> 'brand_id' IS NOT NULL OR p ->> 'operation_code' IS NOT NULL OR p ->> 'product_code' IS NOT NULL))
                     OR (v_scope = 'brand' AND (p ->> 'brand_id' IS NULL OR p ->> 'operation_code' IS NOT NULL OR p ->> 'product_code' IS NOT NULL))
                     OR (v_scope = 'operation' AND p ->> 'operation_code' IS NULL)))
           OR NOT COALESCE((p ->> 'country_code') ~ '^[A-Z]{2}$', false)
           OR NOT COALESCE(v_state IN ('enabled', 'disabled'), false)
           OR NOT COALESCE(v_status IN ('active', 'withdrawn'), false)
           OR NOT public.licensing_change_is_opt_text(p -> 'authorization_reference', 500)
           OR (v_status = 'active' AND v_state = 'enabled' AND p ->> 'authorization_reference' IS NULL)
           OR (v_status = 'withdrawn' AND v_state <> 'disabled')
           OR (p_kind = 'operating_policy_version' AND v_status = 'withdrawn' AND v_scope IN ('brand', 'operation')
               AND p ->> 'authorization_reference' IS NULL) THEN
            RAISE EXCEPTION 'licensing change: invalid % payload', p_kind USING ERRCODE = 'LC010';
        END IF;
        IF p_kind = 'licence_ceiling_version' THEN
            IF NOT EXISTS (SELECT 1 FROM public.licences l WHERE l.id = (p ->> 'licence_id')::uuid) THEN
                RAISE EXCEPTION 'licensing change: unknown licence' USING ERRCODE = 'LC010';
            END IF;
            subject_licence := (p ->> 'licence_id')::uuid;
            -- A withdrawn or absent ceiling resolves not_permitted_by_licence: only enabled + active widens.
            widening := (v_state = 'enabled' AND v_status = 'active');
        ELSE
            IF NOT EXISTS (SELECT 1 FROM public.tenants t WHERE t.id = (p ->> 'tenant_id')::uuid) THEN
                RAISE EXCEPTION 'licensing change: unknown tenant' USING ERRCODE = 'LC010';
            END IF;
            subject_tenant := (p ->> 'tenant_id')::uuid;
            -- 0178 (security review F-4): a TENANT session's brand-rung policy must name a brand of its own tenant (checked at
            -- filing, under the requester's RLS; a platform requester cannot read tenant brands and the composite FK still
            -- refuses a foreign brand at execution).
            IF p ->> 'brand_id' IS NOT NULL AND NULLIF(pg_catalog.current_setting('app.tenant_id', true), '') IS NOT NULL
               AND NOT EXISTS (SELECT 1 FROM public.brands b WHERE b.id = (p ->> 'brand_id')::uuid AND b.tenant_id = subject_tenant) THEN
                RAISE EXCEPTION 'licensing change: unknown brand' USING ERRCODE = 'LC010';
            END IF;
            -- ADR 0045 3.5-A AMENDMENT-2: absence at the brand / operation rung inherits, so a withdrawal there widens.
            widening := (v_state = 'enabled' AND v_status = 'active')
                        OR (v_status = 'withdrawn' AND v_scope IN ('brand', 'operation'));
        END IF;
        RETURN;
    END IF;

    IF p_kind = 'tenant_jurisdiction_config_version' THEN
        IF NOT public.licensing_change_keys_are(p, ARRAY['tenant_id', 'jurisdiction_id', 'action', 'kyc_ruleset_id', 'aml_ruleset_id',
                                                          'rg_ruleset_id', 'reporting_ruleset_id', 'allowed_currencies',
                                                          'allowed_payment_methods', 'geo_block_list'])
           OR NOT public.licensing_change_is_uuid(p -> 'tenant_id')
           OR NOT public.licensing_change_is_uuid(p -> 'jurisdiction_id')
           OR NOT COALESCE(p ->> 'action' IN ('set', 'close'), false)
           OR (p ->> 'action' = 'set' AND (
                   NOT public.licensing_change_is_opt_text(p -> 'kyc_ruleset_id', 128)
                OR NOT public.licensing_change_is_opt_text(p -> 'aml_ruleset_id', 128)
                OR NOT public.licensing_change_is_opt_text(p -> 'rg_ruleset_id', 128)
                OR NOT public.licensing_change_is_opt_text(p -> 'reporting_ruleset_id', 128)
                OR NOT public.licensing_change_is_text_array(p -> 'allowed_currencies', 64)
                OR NOT public.licensing_change_is_text_array(p -> 'allowed_payment_methods', 64)
                OR NOT public.licensing_change_is_text_array(p -> 'geo_block_list', 300)))
           OR (p ->> 'action' = 'close' AND (
                   p ->> 'kyc_ruleset_id' IS NOT NULL OR p ->> 'aml_ruleset_id' IS NOT NULL OR p ->> 'rg_ruleset_id' IS NOT NULL
                OR p ->> 'reporting_ruleset_id' IS NOT NULL OR p ->> 'allowed_currencies' IS NOT NULL
                OR p ->> 'allowed_payment_methods' IS NOT NULL OR p ->> 'geo_block_list' IS NOT NULL)) THEN
            RAISE EXCEPTION 'licensing change: invalid tenant_jurisdiction_config_version payload' USING ERRCODE = 'LC010';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM public.tenants t WHERE t.id = (p ->> 'tenant_id')::uuid)
           OR NOT EXISTS (SELECT 1 FROM public.jurisdictions j WHERE j.id = (p ->> 'jurisdiction_id')::uuid) THEN
            RAISE EXCEPTION 'licensing change: unknown tenant or jurisdiction' USING ERRCODE = 'LC010';
        END IF;
        subject_tenant := (p ->> 'tenant_id')::uuid;
        -- A platform session (the executor, and a platform requester) locks the open version; a tenant session has no UPDATE
        -- policy on the table (FOR UPDATE would hide the row) and reads it plainly: its classification is re-done, locked,
        -- by the platform executor (LC024).
        IF NULLIF(pg_catalog.current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL THEN
            SELECT * INTO v_open FROM public.tenant_jurisdiction_configs c
             WHERE c.tenant_id = subject_tenant AND c.jurisdiction_id = (p ->> 'jurisdiction_id')::uuid AND c.effective_to IS NULL
             ORDER BY c.effective_from DESC LIMIT 1 FOR UPDATE;
        ELSE
            SELECT * INTO v_open FROM public.tenant_jurisdiction_configs c
             WHERE c.tenant_id = subject_tenant AND c.jurisdiction_id = (p ->> 'jurisdiction_id')::uuid AND c.effective_to IS NULL
             ORDER BY c.effective_from DESC LIMIT 1;
        END IF;
        IF p ->> 'action' = 'close' THEN
            IF NOT FOUND THEN
                RAISE EXCEPTION 'licensing change: there is no open configuration to close' USING ERRCODE = 'LC010';
            END IF;
            widening := false;   -- no configuration in effect: H-3 and the AML jurisdiction check refuse (fail closed)
            RETURN;
        END IF;
        widening := NOT (FOUND
            AND v_open.kyc_ruleset_id IS NOT DISTINCT FROM p ->> 'kyc_ruleset_id'
            AND v_open.aml_ruleset_id IS NOT DISTINCT FROM p ->> 'aml_ruleset_id'
            AND v_open.rg_ruleset_id IS NOT DISTINCT FROM p ->> 'rg_ruleset_id'
            AND v_open.reporting_ruleset_id IS NOT DISTINCT FROM p ->> 'reporting_ruleset_id'
            AND (p -> 'allowed_currencies') <@ v_open.allowed_currencies
            AND (p -> 'allowed_payment_methods') <@ v_open.allowed_payment_methods
            AND v_open.geo_block_list <@ (p -> 'geo_block_list'));
        RETURN;
    END IF;

    -- 0178 (licence lifecycle through the primitive). The database computes widening against the CURRENT licence row (locked
    -- for a platform session); the Go ChangeKind classification (internal/jurisdiction/licence_lifecycle.go) is the same
    -- rule. A no-op is invalid (LC010). An unrecognised stored status classifies as widening (fail closed).
    IF p_kind IN ('licence_status_change', 'licence_validity_dates_change', 'licence_review_due_change', 'licence_evidence_reference_change') THEN
        IF NOT public.licensing_change_keys_are(p, CASE p_kind
                WHEN 'licence_status_change' THEN ARRAY['licence_id', 'status']
                WHEN 'licence_validity_dates_change' THEN ARRAY['licence_id', 'issued_at', 'expires_at']
                WHEN 'licence_review_due_change' THEN ARRAY['licence_id', 'review_due_at']
                ELSE ARRAY['licence_id', 'licence_evidence_reference'] END)
           OR NOT public.licensing_change_is_uuid(p -> 'licence_id') THEN
            RAISE EXCEPTION 'licensing change: invalid % payload', p_kind USING ERRCODE = 'LC010';
        END IF;
        IF NULLIF(pg_catalog.current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL THEN
            SELECT * INTO v_lic FROM public.licences l WHERE l.id = (p ->> 'licence_id')::uuid FOR UPDATE;
        ELSE
            SELECT * INTO v_lic FROM public.licences l WHERE l.id = (p ->> 'licence_id')::uuid;
        END IF;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'licensing change: unknown licence' USING ERRCODE = 'LC010';
        END IF;
        subject_licence := v_lic.id;

        IF p_kind = 'licence_status_change' THEN
            IF NOT COALESCE(p ->> 'status' IN ('active', 'suspended', 'expired'), false) OR pg_catalog.jsonb_typeof(p -> 'status') <> 'string' THEN
                RAISE EXCEPTION 'licensing change: invalid licence_status_change payload' USING ERRCODE = 'LC010';
            END IF;
            IF v_lic.status = p ->> 'status' THEN
                RAISE EXCEPTION 'licensing change: the licence already has this status' USING ERRCODE = 'LC010';
            END IF;
            v_rank_new := CASE p ->> 'status' WHEN 'active' THEN 0 WHEN 'suspended' THEN 1 ELSE 2 END;
            v_rank_old := CASE v_lic.status WHEN 'active' THEN 0 WHEN 'suspended' THEN 1 WHEN 'expired' THEN 2 ELSE NULL END;
            widening := (v_rank_old IS NULL OR v_rank_new < v_rank_old);
            RETURN;
        END IF;

        IF p_kind = 'licence_validity_dates_change' THEN
            IF NOT ((pg_catalog.jsonb_typeof(p -> 'issued_at') = 'null') OR (pg_catalog.jsonb_typeof(p -> 'issued_at') = 'string' AND (p ->> 'issued_at') ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}$'))
               OR NOT ((pg_catalog.jsonb_typeof(p -> 'expires_at') = 'null') OR (pg_catalog.jsonb_typeof(p -> 'expires_at') = 'string' AND (p ->> 'expires_at') ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}$')) THEN
                RAISE EXCEPTION 'licensing change: invalid licence_validity_dates_change payload' USING ERRCODE = 'LC010';
            END IF;
            BEGIN
                v_new_issued := (p ->> 'issued_at')::date;
                v_new_expires := (p ->> 'expires_at')::date;
            EXCEPTION WHEN others THEN
                RAISE EXCEPTION 'licensing change: invalid licence_validity_dates_change dates' USING ERRCODE = 'LC010';
            END;
            IF v_new_issued IS NOT NULL AND v_new_expires IS NOT NULL AND v_new_expires <= v_new_issued THEN
                RAISE EXCEPTION 'licensing change: expires_at must be after issued_at' USING ERRCODE = 'LC010';
            END IF;
            IF v_new_issued IS NOT DISTINCT FROM v_lic.issued_at AND v_new_expires IS NOT DISTINCT FROM v_lic.expires_at THEN
                RAISE EXCEPTION 'licensing change: validity dates are unchanged' USING ERRCODE = 'LC010';
            END IF;
            v_w := false;
            IF v_new_expires IS DISTINCT FROM v_lic.expires_at AND (v_new_expires IS NULL OR (v_lic.expires_at IS NOT NULL AND v_new_expires > v_lic.expires_at)) THEN
                v_w := true;
            END IF;
            IF v_new_issued IS DISTINCT FROM v_lic.issued_at AND (v_new_issued IS NULL OR (v_lic.issued_at IS NOT NULL AND v_new_issued < v_lic.issued_at)) THEN
                v_w := true;
            END IF;
            widening := v_w;
            RETURN;
        END IF;

        IF p_kind = 'licence_review_due_change' THEN
            IF NOT ((pg_catalog.jsonb_typeof(p -> 'review_due_at') = 'null') OR (pg_catalog.jsonb_typeof(p -> 'review_due_at') = 'string' AND (p ->> 'review_due_at') ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}$')) THEN
                RAISE EXCEPTION 'licensing change: invalid licence_review_due_change payload' USING ERRCODE = 'LC010';
            END IF;
            BEGIN
                v_new_review := (p ->> 'review_due_at')::date;
            EXCEPTION WHEN others THEN
                RAISE EXCEPTION 'licensing change: invalid licence_review_due_change date' USING ERRCODE = 'LC010';
            END;
            IF v_new_review IS NOT DISTINCT FROM v_lic.review_due_at THEN
                RAISE EXCEPTION 'licensing change: review_due_at is unchanged' USING ERRCODE = 'LC010';
            END IF;
            -- Postponing or clearing a review reduces oversight (widening); setting or bringing it forward tightens it.
            widening := (v_new_review IS NULL OR (v_lic.review_due_at IS NOT NULL AND v_new_review > v_lic.review_due_at));
            RETURN;
        END IF;

        -- licence_evidence_reference_change: neutral (an opaque reference; it widens nothing).
        IF NOT public.licensing_change_is_opt_text(p -> 'licence_evidence_reference', 200)
           OR (p ->> 'licence_evidence_reference') <> pg_catalog.btrim(p ->> 'licence_evidence_reference')
           OR (p ->> 'licence_evidence_reference') LIKE '%@%' THEN
            RAISE EXCEPTION 'licensing change: invalid licence_evidence_reference_change payload' USING ERRCODE = 'LC010';
        END IF;
        IF (p ->> 'licence_evidence_reference') IS NOT DISTINCT FROM v_lic.evidence_reference THEN
            RAISE EXCEPTION 'licensing change: the evidence reference is unchanged' USING ERRCODE = 'LC010';
        END IF;
        widening := false;
        RETURN;
    END IF;

    RAISE EXCEPTION 'licensing change: kind % has no classification (closed registry)', p_kind USING ERRCODE = 'LC010';
END
$$;

CREATE OR REPLACE FUNCTION licensing_change_row_matches(p_kind text, p jsonb, p_reason text, p_subject uuid,
    p_table text, p_op text, p_new jsonb, p_old jsonb) RETURNS boolean
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT COALESCE(CASE
        WHEN p_table = 'licences' AND p_op = 'INSERT' THEN
            p_kind = 'licence_create'
            AND p_new ->> 'jurisdiction_id' = p ->> 'jurisdiction_id' AND p_new ->> 'licensee' = p ->> 'licensee'
            AND p_new ->> 'licence_number' = p ->> 'licence_number'
            AND p_new -> 'permitted_products' = p -> 'permitted_products' AND p_new -> 'permitted_markets' = p -> 'permitted_markets'
            AND p_new ->> 'status' = 'active' AND p_new ->> 'issued_at' IS NULL AND p_new ->> 'expires_at' IS NULL
        WHEN p_table = 'licences' AND p_op = 'UPDATE' THEN
            p_old ->> 'id' = p ->> 'licence_id' AND p_new ->> 'id' = p ->> 'licence_id'
            AND CASE p_kind
                WHEN 'licence_status_change' THEN
                    p_new ->> 'status' = p ->> 'status'
                    AND (p_new - ARRAY['updated_at', 'status']) = (p_old - ARRAY['updated_at', 'status'])
                WHEN 'licence_validity_dates_change' THEN
                    (p_new ->> 'issued_at') IS NOT DISTINCT FROM (p ->> 'issued_at')
                    AND (p_new ->> 'expires_at') IS NOT DISTINCT FROM (p ->> 'expires_at')
                    AND (p_new - ARRAY['updated_at', 'issued_at', 'expires_at']) = (p_old - ARRAY['updated_at', 'issued_at', 'expires_at'])
                WHEN 'licence_review_due_change' THEN
                    (p_new ->> 'review_due_at') IS NOT DISTINCT FROM (p ->> 'review_due_at')
                    AND (p_new - ARRAY['updated_at', 'review_due_at']) = (p_old - ARRAY['updated_at', 'review_due_at'])
                WHEN 'licence_evidence_reference_change' THEN
                    (p_new ->> 'evidence_reference') IS NOT DISTINCT FROM (p ->> 'licence_evidence_reference')
                    AND (p_new - ARRAY['updated_at', 'evidence_reference']) = (p_old - ARRAY['updated_at', 'evidence_reference'])
                ELSE false END
        WHEN p_table = 'tenants' THEN
            p_subject::text = p_new ->> 'id'
            AND ((p_kind = 'tenant_licence_bind' AND p_new ->> 'licence_id' = p ->> 'licence_id')
                 OR (p_kind = 'tenant_licence_unbind' AND p_op = 'UPDATE' AND p_new ->> 'licence_id' IS NULL))
        WHEN p_table = 'licence_country_ceilings' AND p_op = 'INSERT' THEN
            p_kind = 'licence_ceiling_version'
            AND p_new ->> 'licence_id' = p ->> 'licence_id' AND p_new ->> 'country_code' = p ->> 'country_code'
            AND p_new ->> 'state' = p ->> 'state' AND p_new ->> 'status' = p ->> 'status'
            AND (p_new ->> 'authorization_reference') IS NOT DISTINCT FROM (p ->> 'authorization_reference')
            AND p_new ->> 'reason_code' = p_reason
        WHEN p_table = 'licence_country_ceilings' AND p_op = 'UPDATE' THEN
            p_kind = 'licence_ceiling_version'
            AND p_old ->> 'licence_id' = p ->> 'licence_id' AND p_old ->> 'country_code' = p ->> 'country_code'
        WHEN p_table = 'operating_country_policies' AND p_op = 'INSERT' THEN
            p_kind = 'operating_policy_version'
            AND p_new ->> 'tenant_id' = p ->> 'tenant_id' AND p_new ->> 'scope_kind' = p ->> 'scope'
            AND (p_new ->> 'brand_id') IS NOT DISTINCT FROM (p ->> 'brand_id')
            AND (p_new ->> 'operation_code') IS NOT DISTINCT FROM (p ->> 'operation_code')
            AND (p_new ->> 'product_code') IS NOT DISTINCT FROM (p ->> 'product_code')
            AND p_new ->> 'country_code' = p ->> 'country_code'
            AND p_new ->> 'state' = p ->> 'state' AND p_new ->> 'status' = p ->> 'status'
            AND (p_new ->> 'authorization_reference') IS NOT DISTINCT FROM (p ->> 'authorization_reference')
            AND p_new ->> 'reason_code' = p_reason
        WHEN p_table = 'operating_country_policies' AND p_op = 'UPDATE' THEN
            p_kind = 'operating_policy_version'
            AND p_old ->> 'tenant_id' = p ->> 'tenant_id' AND p_old ->> 'scope_kind' = p ->> 'scope'
            AND (p_old ->> 'brand_id') IS NOT DISTINCT FROM (p ->> 'brand_id')
            AND (p_old ->> 'operation_code') IS NOT DISTINCT FROM (p ->> 'operation_code')
            AND (p_old ->> 'product_code') IS NOT DISTINCT FROM (p ->> 'product_code')
            AND p_old ->> 'country_code' = p ->> 'country_code'
        WHEN p_table = 'tenant_jurisdiction_configs' AND p_op = 'INSERT' THEN
            p_kind = 'tenant_jurisdiction_config_version' AND p ->> 'action' = 'set'
            AND p_new ->> 'tenant_id' = p ->> 'tenant_id' AND p_new ->> 'jurisdiction_id' = p ->> 'jurisdiction_id'
            AND (p_new ->> 'kyc_ruleset_id') IS NOT DISTINCT FROM (p ->> 'kyc_ruleset_id')
            AND (p_new ->> 'aml_ruleset_id') IS NOT DISTINCT FROM (p ->> 'aml_ruleset_id')
            AND (p_new ->> 'rg_ruleset_id') IS NOT DISTINCT FROM (p ->> 'rg_ruleset_id')
            AND (p_new ->> 'reporting_ruleset_id') IS NOT DISTINCT FROM (p ->> 'reporting_ruleset_id')
            AND p_new -> 'allowed_currencies' = p -> 'allowed_currencies'
            AND p_new -> 'allowed_payment_methods' = p -> 'allowed_payment_methods'
            AND p_new -> 'geo_block_list' = p -> 'geo_block_list'
        WHEN p_table = 'tenant_jurisdiction_configs' AND p_op = 'UPDATE' THEN
            p_kind = 'tenant_jurisdiction_config_version'
            AND p_old ->> 'tenant_id' = p ->> 'tenant_id' AND p_old ->> 'jurisdiction_id' = p ->> 'jurisdiction_id'
        ELSE false
    END, false)
$$;


-- =========================================================================
-- F-1 (security review secrev3, HIGH): the licences UPDATE guard
-- =========================================================================
-- (a) The identity columns (id, jurisdiction_id, licensee, licence_number, permitted_products, permitted_markets, created_at)
--     are IMMUTABLE for every role, the table owner included (LC042): no Go writer touches them, so re-pointing a licence to
--     another jurisdiction / number / market set would be licence_create + tenant_licence_bind with no approvals. A future
--     need is a reviewed migration (disable the trigger there) or a new licensing change kind, never a runtime write.
-- (b) Any other role than the table owner needs an EXECUTED licensing change request of the matching lifecycle kind, executed in
--     THIS transaction, whose payload matches the written row (only the kind's columns may differ; LC040). A made-up platform
--     GUC alone therefore never authorises a licence UPDATE: the request and its execution each carry a signed actor proof
--     (licensing_change:request, licensing_change:execute; narrowing changes too), verified by the 0177 request guard. No new
--     proof operation is needed: the executed request IS the proof-bound authorisation (see docs/plans/lic4-widening-and-reads-record.md).
-- (c) The change is classified HERE too, from OLD to NEW (not trusted from the request): a WIDENING row change (status toward
--     active or from an unrecognised status, a later or cleared expires_at, an earlier or cleared issued_at, a postponed or
--     cleared review_due_at) additionally needs an executed request that was itself classified widening and holds at least the
--     kind's widening_approvals approvals (so a request classified narrowing can never carry a widening row write).
CREATE FUNCTION licences_update_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_old        jsonb := pg_catalog.to_jsonb(OLD);
    v_new        jsonb := pg_catalog.to_jsonb(NEW);
    v_rank_old   int;
    v_rank_new   int;
    v_widening   boolean;
    v_n          bigint;
BEGIN
    IF NEW.id IS DISTINCT FROM OLD.id OR NEW.jurisdiction_id IS DISTINCT FROM OLD.jurisdiction_id
       OR NEW.licensee IS DISTINCT FROM OLD.licensee OR NEW.licence_number IS DISTINCT FROM OLD.licence_number
       OR NEW.permitted_products IS DISTINCT FROM OLD.permitted_products OR NEW.permitted_markets IS DISTINCT FROM OLD.permitted_markets
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'licences: the identity columns (id, jurisdiction_id, licensee, licence_number, permitted_products, permitted_markets, created_at) are immutable'
            USING ERRCODE = 'LC042';
    END IF;
    IF public.launch_is_table_owner() THEN
        RETURN NEW;
    END IF;
    v_rank_old := CASE OLD.status WHEN 'active' THEN 0 WHEN 'suspended' THEN 1 WHEN 'expired' THEN 2 ELSE NULL END;
    v_rank_new := CASE NEW.status WHEN 'active' THEN 0 WHEN 'suspended' THEN 1 WHEN 'expired' THEN 2 ELSE NULL END;
    v_widening :=
        (NEW.status IS DISTINCT FROM OLD.status AND (v_rank_old IS NULL OR v_rank_new IS NULL OR v_rank_new < v_rank_old))
        OR (NEW.expires_at IS DISTINCT FROM OLD.expires_at AND (NEW.expires_at IS NULL OR (OLD.expires_at IS NOT NULL AND NEW.expires_at > OLD.expires_at)))
        OR (NEW.issued_at IS DISTINCT FROM OLD.issued_at AND (NEW.issued_at IS NULL OR (OLD.issued_at IS NOT NULL AND NEW.issued_at < OLD.issued_at)))
        OR (NEW.review_due_at IS DISTINCT FROM OLD.review_due_at AND (NEW.review_due_at IS NULL OR (OLD.review_due_at IS NOT NULL AND NEW.review_due_at > OLD.review_due_at)));
    SELECT count(*) INTO v_n
      FROM public.licensing_change_requests r
      JOIN public.licensing_change_kinds k ON k.kind = r.kind
     WHERE r.status = 'executed' AND r.executed_txid = pg_catalog.txid_current()
       AND public.licensing_change_row_matches(r.kind, r.payload, r.reason_code, r.subject_tenant_id, 'licences', 'UPDATE', v_new, v_old)
       AND (NOT v_widening OR (r.widening AND r.required_approvals >= k.widening_approvals));
    IF v_n < 1 THEN
        RAISE EXCEPTION 'licences: an UPDATE needs a licensing change request of the matching lifecycle kind executed in this transaction (widening: with its approvals) (MKT-DUAL-1, migration 0178)'
            USING ERRCODE = 'LC040';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER zz_licensing_change_governed_upd BEFORE UPDATE ON licences
    FOR EACH ROW EXECUTE FUNCTION licences_update_guard();

-- =========================================================================
-- F-2 (MEDIUM): licence_versions is written ONLY by the licences trigger
-- =========================================================================
-- version_no becomes bigint (a forged huge value can no longer overflow the next increment); the INSERT policy additionally
-- requires pg_trigger_depth() > 0 (a direct INSERT is refused for every role); INSERT is revoked from igaming_runtime; the
-- writer is SECURITY DEFINER (pinned search_path) and records changed_by ONLY when the session's platform principal is an
-- ACTIVE platform staff row (otherwise NULL: an unverifiable GUC is never recorded as an actor).
ALTER TABLE licence_versions ALTER COLUMN version_no TYPE bigint;

DROP POLICY licence_versions_platform_admin_insert ON licence_versions;
CREATE POLICY licence_versions_platform_admin_insert ON licence_versions
    FOR INSERT WITH CHECK (
        pg_trigger_depth() > 0
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE OR REPLACE FUNCTION licences_write_version() RETURNS TRIGGER AS $$
DECLARE
    v_no  BIGINT;
    v_by  UUID;
    v_guc TEXT := NULLIF(current_setting('app.platform_admin_principal_id', true), '');
BEGIN
    IF TG_OP = 'UPDATE' AND ROW(NEW.status, NEW.issued_at, NEW.expires_at, NEW.review_due_at, NEW.evidence_reference,
                                NEW.licence_number, NEW.permitted_products, NEW.permitted_markets)
        IS NOT DISTINCT FROM ROW(OLD.status, OLD.issued_at, OLD.expires_at, OLD.review_due_at, OLD.evidence_reference,
                                 OLD.licence_number, OLD.permitted_products, OLD.permitted_markets) THEN
        RETURN NEW;
    END IF;
    IF v_guc IS NOT NULL THEN
        SELECT su.id INTO v_by FROM public.staff_users su
         WHERE su.id::text = v_guc AND su.tenant_id IS NULL AND su.status = 'active';
    END IF;
    PERFORM 1 FROM public.licences l WHERE l.id = NEW.id FOR UPDATE;
    SELECT COALESCE(max(version_no), 0) + 1 INTO v_no FROM public.licence_versions WHERE licence_id = NEW.id;
    INSERT INTO public.licence_versions (licence_id, version_no, operation, changed_by, status, issued_at, expires_at,
                                  review_due_at, evidence_reference, licence_number, permitted_products, permitted_markets)
    VALUES (NEW.id, v_no, lower(TG_OP), v_by,
            NEW.status, NEW.issued_at, NEW.expires_at, NEW.review_due_at, NEW.evidence_reference,
            NEW.licence_number, NEW.permitted_products, NEW.permitted_markets);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE INSERT ON licence_versions FROM igaming_runtime';
    END IF;
END
$$;

-- =========================================================================
-- F-5 (LOW): an open request of one requester class never blocks the identical request of the other
-- =========================================================================
-- The open-request uniqueness was on payload_hash alone (kind + payload + reason code), so a tenant's open request blocked a
-- platform admin's identical single-actor narrowing with LICENSING_CHANGE_DUPLICATE_PENDING. It is now per requester scope
-- (tenant / platform): duplicates within one class are still refused (same index name, same Go mapping).
DROP INDEX uq_licensing_change_requests_open_payload;
CREATE UNIQUE INDEX uq_licensing_change_requests_open_payload ON licensing_change_requests (payload_hash, requested_scope) WHERE status = 'requested';

-- =========================================================================
-- F-3 (LOW): the sweep reports its own failure and the licences it cannot monitor
-- =========================================================================
-- licence.sweep_failed (p1, like reconciliation.sweep_failed) and licence.unmonitored (p2), platform scope, subjectless,
-- raised only under the alert_dispatcher identity (the 0176 pattern). Ids, counts and a closed phase / trigger only.
ALTER TABLE alert_kinds DROP CONSTRAINT alert_kinds_check1;
ALTER TABLE alert_kinds ADD CONSTRAINT alert_kinds_check1 CHECK (
    requires_subject = true
    OR kind IN ('alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed',
                'staff.platform_mfa_integrity', 'staff.platform_session_integrity', 'staff.platform_mfa_hard_locked',
                'staff.platform_mfa_recovery_used', 'staff.platform_mfa_failure_pattern',
                'staff.platform_break_glass_used',
                'reconciliation.sweep_failed',
                'licence.expired', 'licence.expiring', 'licence.sweep_failed', 'licence.unmonitored'));

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM public.alert_kinds WHERE kind IN ('licence.sweep_failed', 'licence.unmonitored')) THEN
        RAISE EXCEPTION 'migration 0178 pre-flight: a licence.sweep_failed / licence.unmonitored alert kind already exists';
    END IF;
    CREATE POLICY alert_kinds_seed_0178 ON alert_kinds
        FOR INSERT TO CURRENT_USER
        WITH CHECK (kind IN ('licence.sweep_failed', 'licence.unmonitored'));
    INSERT INTO alert_kinds (kind, severity, scope, simulation, requires_subject, in_tx_raisable_by_tenant, allowed_keys, raise_mode) VALUES
        ('licence.sweep_failed', 'p1', 'platform', false, false, false,
         ARRAY['phase', 'error_count'], 'detached'),
        ('licence.unmonitored', 'p2', 'platform', false, false, false,
         ARRAY['licence_id', 'trigger', 'expires_at', 'review_due_at', 'tenant_count', 'brand_count', 'tenant_ids', 'brand_ids', 'truncated'], 'detached');
    DROP POLICY alert_kinds_seed_0178 ON alert_kinds;
END
$$;

DROP POLICY alerts_platform_service_dispatcher_raise ON alerts;
CREATE POLICY alerts_platform_service_dispatcher_raise ON alerts
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
        AND tenant_id IS NULL
        AND subject_tenant_id IS NULL
        AND kind IN ('alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed', 'reconciliation.sweep_failed',
                     'licence.expired', 'licence.expiring', 'licence.sweep_failed', 'licence.unmonitored')
    );
DROP POLICY alerts_platform_service_dispatcher_raise ON alert_occurrences;
CREATE POLICY alerts_platform_service_dispatcher_raise ON alert_occurrences
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
        AND tenant_id IS NULL
        AND subject_tenant_id IS NULL
        AND kind IN ('alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed', 'reconciliation.sweep_failed',
                     'licence.expired', 'licence.expiring', 'licence.sweep_failed', 'licence.unmonitored')
    );

DROP POLICY alerts_licence_sweep_dispatcher_only ON alerts;
CREATE POLICY alerts_licence_sweep_dispatcher_only ON alerts
    AS RESTRICTIVE
    FOR INSERT
    WITH CHECK (
        kind NOT IN ('licence.expired', 'licence.expiring', 'licence.sweep_failed', 'licence.unmonitored')
        OR NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
    );
DROP POLICY alerts_licence_sweep_dispatcher_only ON alert_occurrences;
CREATE POLICY alerts_licence_sweep_dispatcher_only ON alert_occurrences
    AS RESTRICTIVE
    FOR INSERT
    WITH CHECK (
        kind NOT IN ('licence.expired', 'licence.expiring', 'licence.sweep_failed', 'licence.unmonitored')
        OR NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
    );

