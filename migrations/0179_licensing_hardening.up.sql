-- 0179: licensing hardening (lic6; security re-review of 0176-0178: L-1, I-2, I-3). SECURITY-SENSITIVE: for `security`, `code-reviewer`.
--   L-1  licence_versions.changed_by comes from the licensing change request that authorised the row write (same transaction,
--        same row_matches predicate), never from app.platform_admin_principal_id; new nullable column change_request_id.
--        Owner-path writes (no request) record NULL / NULL.
--   I-2  igaming_runtime loses DELETE on licences (no code path deletes a licence; defence in depth next to the missing RLS DELETE policy).
--   I-3  the licence_create INSERT match additionally pins review_due_at and evidence_reference to NULL at creation.
-- No role, password or attribute change. The only grant change is the conditional REVOKE of DELETE on licences.

SET LOCAL lock_timeout = '5s';

ALTER TABLE licence_versions ADD COLUMN change_request_id UUID REFERENCES licensing_change_requests (id);
COMMENT ON COLUMN licence_versions.change_request_id IS '0179: the executed licensing change request that authorised this change (NULL for a baseline row and for an owner-path write).';

CREATE OR REPLACE FUNCTION licences_write_version() RETURNS TRIGGER AS $$
DECLARE
    v_no  BIGINT;
    v_by  UUID;
    v_req UUID;
    v_old JSONB;
    v_new JSONB := pg_catalog.to_jsonb(NEW);
BEGIN
    IF TG_OP = 'UPDATE' AND ROW(NEW.status, NEW.issued_at, NEW.expires_at, NEW.review_due_at, NEW.evidence_reference,
                                NEW.licence_number, NEW.permitted_products, NEW.permitted_markets)
        IS NOT DISTINCT FROM ROW(OLD.status, OLD.issued_at, OLD.expires_at, OLD.review_due_at, OLD.evidence_reference,
                                 OLD.licence_number, OLD.permitted_products, OLD.permitted_markets) THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' THEN
        v_old := pg_catalog.to_jsonb(OLD);
    END IF;
    -- 0179 (security re-review L-1): the actor is NOT read from a session setting. It is the executor of the licensing change
    -- request that authorised THIS row write (executed in this transaction, same row_matches predicate as the governed-write
    -- guards). The owner path (fixtures, seeding) has no request and records NULL / NULL.
    SELECT r.id, r.executed_by INTO v_req, v_by
      FROM public.licensing_change_requests r
     WHERE r.status = 'executed' AND r.executed_txid = pg_catalog.txid_current()
       AND public.licensing_change_row_matches(r.kind, r.payload, r.reason_code, r.subject_tenant_id, 'licences', TG_OP, v_new, v_old)
     ORDER BY r.executed_at DESC, r.id
     LIMIT 1;
    PERFORM 1 FROM public.licences l WHERE l.id = NEW.id FOR UPDATE;
    SELECT COALESCE(max(version_no), 0) + 1 INTO v_no FROM public.licence_versions WHERE licence_id = NEW.id;
    INSERT INTO public.licence_versions (licence_id, version_no, operation, changed_by, change_request_id, status, issued_at, expires_at,
                                  review_due_at, evidence_reference, licence_number, permitted_products, permitted_markets)
    VALUES (NEW.id, v_no, lower(TG_OP), v_by, v_req,
            NEW.status, NEW.issued_at, NEW.expires_at, NEW.review_due_at, NEW.evidence_reference,
            NEW.licence_number, NEW.permitted_products, NEW.permitted_markets);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp;

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
            AND p_new ->> 'review_due_at' IS NULL AND p_new ->> 'evidence_reference' IS NULL
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

DO $$
BEGIN
    -- Only when the role actually holds the privilege (a REVOKE on a role with no ACL entry would materialise the owner ACL and
    -- break the byte-for-byte down).
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime')
       AND has_table_privilege('igaming_runtime', 'licences', 'DELETE') THEN
        EXECUTE 'REVOKE DELETE ON licences FROM igaming_runtime';
    END IF;
END
$$;
