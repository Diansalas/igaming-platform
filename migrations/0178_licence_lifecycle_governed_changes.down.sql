-- Down of 0178 (DEV/STAGING ONLY). Refused (LC095) while any licensing change request of a 0178 kind exists (the decision
-- records would be lost). Otherwise restores exactly the 0177 function bodies and drops the trigger and kind rows.
SET LOCAL lock_timeout = '5s';

DO $$
DECLARE
    v_any boolean;
BEGIN
    ALTER TABLE licensing_change_requests NO FORCE ROW LEVEL SECURITY;
    SELECT EXISTS (SELECT 1 FROM licensing_change_requests WHERE kind IN
        ('licence_status_change', 'licence_validity_dates_change', 'licence_review_due_change', 'licence_evidence_reference_change')) INTO v_any;
    ALTER TABLE licensing_change_requests FORCE ROW LEVEL SECURITY;
    IF v_any THEN
        RAISE EXCEPTION 'migration 0178 down refused: licence lifecycle licensing change requests exist' USING ERRCODE = 'LC095';
    END IF;
    ALTER TABLE alerts NO FORCE ROW LEVEL SECURITY;
    ALTER TABLE alert_occurrences NO FORCE ROW LEVEL SECURITY;
    SELECT EXISTS (SELECT 1 FROM alerts WHERE kind IN ('licence.sweep_failed', 'licence.unmonitored'))
        OR EXISTS (SELECT 1 FROM alert_occurrences WHERE kind IN ('licence.sweep_failed', 'licence.unmonitored')) INTO v_any;
    ALTER TABLE alerts FORCE ROW LEVEL SECURITY;
    ALTER TABLE alert_occurrences FORCE ROW LEVEL SECURITY;
    IF v_any THEN
        RAISE EXCEPTION 'migration 0178 down refused: alerts of kind licence.sweep_failed / licence.unmonitored exist' USING ERRCODE = 'LC095';
    END IF;
END
$$;


-- F-5: the open-request uniqueness back to payload_hash alone.
DROP INDEX uq_licensing_change_requests_open_payload;
CREATE UNIQUE INDEX uq_licensing_change_requests_open_payload ON licensing_change_requests (payload_hash) WHERE status = 'requested';

-- F-3: the alert kinds and policies back to the 0176 state.
DROP POLICY alerts_licence_sweep_dispatcher_only ON alert_occurrences;
CREATE POLICY alerts_licence_sweep_dispatcher_only ON alert_occurrences
    AS RESTRICTIVE
    FOR INSERT
    WITH CHECK (
        kind NOT IN ('licence.expired', 'licence.expiring')
        OR NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
    );
DROP POLICY alerts_licence_sweep_dispatcher_only ON alerts;
CREATE POLICY alerts_licence_sweep_dispatcher_only ON alerts
    AS RESTRICTIVE
    FOR INSERT
    WITH CHECK (
        kind NOT IN ('licence.expired', 'licence.expiring')
        OR NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
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
                     'licence.expired', 'licence.expiring')
    );
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
                     'licence.expired', 'licence.expiring')
    );
DO $$
BEGIN
    ALTER TABLE alert_kinds DISABLE TRIGGER alert_kinds_deny_update_delete;
    CREATE POLICY alert_kinds_unseed_0178 ON alert_kinds
        FOR DELETE TO CURRENT_USER USING (kind IN ('licence.sweep_failed', 'licence.unmonitored'));
    DELETE FROM alert_kinds WHERE kind IN ('licence.sweep_failed', 'licence.unmonitored');
    DROP POLICY alert_kinds_unseed_0178 ON alert_kinds;
    ALTER TABLE alert_kinds ENABLE TRIGGER alert_kinds_deny_update_delete;
END
$$;
ALTER TABLE alert_kinds DROP CONSTRAINT alert_kinds_check1;
ALTER TABLE alert_kinds ADD CONSTRAINT alert_kinds_check1 CHECK (
    requires_subject = true
    OR kind IN ('alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed',
                'staff.platform_mfa_integrity', 'staff.platform_session_integrity', 'staff.platform_mfa_hard_locked',
                'staff.platform_mfa_recovery_used', 'staff.platform_mfa_failure_pattern',
                'staff.platform_break_glass_used',
                'reconciliation.sweep_failed',
                'licence.expired', 'licence.expiring'));

-- F-2: licence_versions back to the 0176 state (the 0176 writer text, the 0176 INSERT policy, integer version_no, the grant).
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM licence_versions WHERE version_no > 2147483647) THEN
        RAISE EXCEPTION 'migration 0178 down refused: a licence_versions row has a version_no beyond integer range' USING ERRCODE = 'LC095';
    END IF;
END
$$;

CREATE OR REPLACE FUNCTION licences_write_version() RETURNS TRIGGER AS $$
DECLARE
    v_no INTEGER;
BEGIN
    IF TG_OP = 'UPDATE' AND ROW(NEW.status, NEW.issued_at, NEW.expires_at, NEW.review_due_at, NEW.evidence_reference,
                                NEW.licence_number, NEW.permitted_products, NEW.permitted_markets)
        IS NOT DISTINCT FROM ROW(OLD.status, OLD.issued_at, OLD.expires_at, OLD.review_due_at, OLD.evidence_reference,
                                 OLD.licence_number, OLD.permitted_products, OLD.permitted_markets) THEN
        RETURN NEW;
    END IF;
    SELECT COALESCE(max(version_no), 0) + 1 INTO v_no FROM licence_versions WHERE licence_id = NEW.id;
    INSERT INTO licence_versions (licence_id, version_no, operation, changed_by, status, issued_at, expires_at,
                                  review_due_at, evidence_reference, licence_number, permitted_products, permitted_markets)
    VALUES (NEW.id, v_no, lower(TG_OP), NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid,
            NEW.status, NEW.issued_at, NEW.expires_at, NEW.review_due_at, NEW.evidence_reference,
            NEW.licence_number, NEW.permitted_products, NEW.permitted_markets);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;
DROP POLICY licence_versions_platform_admin_insert ON licence_versions;
CREATE POLICY licence_versions_platform_admin_insert ON licence_versions
    FOR INSERT WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
ALTER TABLE licence_versions ALTER COLUMN version_no TYPE integer;
DO $$
BEGIN
    -- Only where the runtime role holds table access at all (the up revoked INSERT from a blanket grant); a database where it never
    -- had any grant on the table must not gain one.
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime')
       AND has_table_privilege('igaming_runtime', 'licence_versions', 'SELECT') THEN
        EXECUTE 'GRANT INSERT ON licence_versions TO igaming_runtime';
    END IF;
END
$$;

DROP TRIGGER zz_licensing_change_governed_upd ON licences;
DROP FUNCTION licences_update_guard();

ALTER TABLE licensing_change_kinds NO FORCE ROW LEVEL SECURITY;
DELETE FROM licensing_change_kinds WHERE kind IN
    ('licence_status_change', 'licence_validity_dates_change', 'licence_review_due_change', 'licence_evidence_reference_change');
ALTER TABLE licensing_change_kinds FORCE ROW LEVEL SECURITY;

CREATE OR REPLACE FUNCTION licensing_change_classify(p_kind text, p jsonb,
    OUT widening boolean, OUT subject_tenant uuid, OUT subject_licence uuid)
    LANGUAGE plpgsql VOLATILE
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_open   public.tenant_jurisdiction_configs%ROWTYPE;
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
