-- Down of 0179 (DEV/STAGING ONLY). Restores the 0178 function bodies and the licences ACL; drops licence_versions.change_request_id
-- (refused, LC095, while any version row carries a change_request_id: that attribution would be lost).
SET LOCAL lock_timeout = '5s';

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM licence_versions WHERE change_request_id IS NOT NULL) THEN
        RAISE EXCEPTION 'migration 0179 down refused: licence_versions rows carry a change_request_id' USING ERRCODE = 'LC095';
    END IF;
END
$$;

DO $$
BEGIN
    -- Mirror of the up: only where the runtime role holds table access at all (the up revoked DELETE from a blanket grant).
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime')
       AND has_table_privilege('igaming_runtime', 'licences', 'SELECT') THEN
        EXECUTE 'GRANT DELETE ON licences TO igaming_runtime';
    END IF;
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

ALTER TABLE licence_versions DROP COLUMN change_request_id;
