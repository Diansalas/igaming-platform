-- Down of 0180 (DEV/STAGING ONLY). Refused (LC095) while any operating_bases row or any licensing change request of a 0180 kind
-- exists (the decision records would be lost). Otherwise restores exactly the 0178 classifier, the 0179 row matcher, the 0076
-- policy-write trigger and the 0076 / 0077 read policies, drops the kind rows and the table, and leaves the schema byte for byte
-- as at 0179.
SET LOCAL lock_timeout = '5s';

DO $$
DECLARE
    v_any boolean;
BEGIN
    ALTER TABLE operating_bases NO FORCE ROW LEVEL SECURITY;
    SELECT EXISTS (SELECT 1 FROM operating_bases) INTO v_any;
    ALTER TABLE operating_bases FORCE ROW LEVEL SECURITY;
    IF v_any THEN
        RAISE EXCEPTION 'migration 0180 down refused: operating_bases rows exist' USING ERRCODE = 'LC095';
    END IF;
    ALTER TABLE licensing_change_requests NO FORCE ROW LEVEL SECURITY;
    SELECT EXISTS (SELECT 1 FROM licensing_change_requests WHERE kind IN
        ('operating_basis_set', 'operating_basis_withdraw', 'operating_basis_review_record')) INTO v_any;
    ALTER TABLE licensing_change_requests FORCE ROW LEVEL SECURITY;
    IF v_any THEN
        RAISE EXCEPTION 'migration 0180 down refused: operating basis licensing change requests exist' USING ERRCODE = 'LC095';
    END IF;
END
$$;

-- The policy-write trigger back to the 0076 body (no search_path pin, as at 0076).
CREATE OR REPLACE FUNCTION operating_country_policies_enforce_ceiling() RETURNS TRIGGER AS $$
DECLARE
    v_licence_id      UUID;
    v_ceiling_state   TEXT;
    v_tenant_state    TEXT;
    v_brand_state     TEXT;
BEGIN
    IF NEW.state = 'disabled' OR NEW.status = 'withdrawn' THEN
        -- Narrowing-or-neutral AS FAR AS THIS FUNCTION'S UPWARD CEILING
        -- CHECKS GO: none of them can be violated by a row that blocks
        -- (disabled) or asserts nothing (withdrawn).
        --
        -- THIS IS NOT A CLAIM THAT SUCH A WRITE CANNOT WIDEN. A WITHDRAWAL
        -- AT THE BRAND OR OPERATION RUNG DOES WIDEN - absence at those
        -- rungs inherits, so removing an in-force disable there lets the
        -- parent's permit through (ADR 0045 §3.5-A AMENDMENT-2, finding
        -- SEC-E-REV-1). That case is controlled by the CHECK constraint
        -- ocp_inherit_rung_withdrawal_requires_authorization, evaluated
        -- independently of this function. Do NOT re-broaden this comment to
        -- "fail-closed direction, always allowed" - that claim was false
        -- and was the defect.
        RETURN NEW;
    END IF;

    -- (1) THE CEILING. Evaluated FIRST, unconditionally (Phase D.1 ruling 2).
    SELECT t.licence_id INTO v_licence_id FROM tenants t WHERE t.id = NEW.tenant_id;
    IF NOT FOUND OR v_licence_id IS NULL THEN
        RAISE EXCEPTION 'operating_country_policies: tenant % has no licence bound; no country may be enabled for it (Stage 4I Phase E, licence ceiling)', NEW.tenant_id;
    END IF;

    SELECT c.state INTO v_ceiling_state
      FROM licence_country_ceilings c
     WHERE c.licence_id = v_licence_id
       AND c.country_code = NEW.country_code
       AND c.status = 'active'
       AND c.effective_to IS NULL
     LIMIT 1;
    IF NOT FOUND OR v_ceiling_state <> 'enabled' THEN
        RAISE EXCEPTION 'operating_country_policies: country % is not permitted by the licence ceiling for licence %; a tenant/brand/operation policy may only narrow within the ceiling and may never exceed it (Stage 4I Phase E)', NEW.country_code, v_licence_id;
    END IF;

    IF NEW.scope_kind = 'tenant' THEN
        RETURN NEW;
    END IF;

    -- (2) The tenant rung must EXIST and be enabled. Absence at tenant
    -- scope is terminal (ruling 5) - so a brand/operation enable above an
    -- absent tenant row could never resolve permitted, and is refused at
    -- write time. This is asset_authorizations_enforce_narrowing()'s own
    -- brand rule.
    SELECT p.state INTO v_tenant_state
      FROM operating_country_policies p
     WHERE p.tenant_id = NEW.tenant_id AND p.scope_kind = 'tenant'
       AND p.country_code = NEW.country_code
       AND p.status = 'active' AND p.effective_to IS NULL
     LIMIT 1;
    IF NOT FOUND OR v_tenant_state <> 'enabled' THEN
        RAISE EXCEPTION 'operating_country_policies: a % policy for country % cannot be enabled before/beyond the tenant-scope policy it narrows (Stage 4I Phase E)', NEW.scope_kind, NEW.country_code;
    END IF;

    -- (3) WRITE-TIME NARROWING ENFORCEMENT (ADR 0045 §3.5-A AMENDMENT-1).
    -- A more-specific operation/product row must never be able to widen
    -- past a broader, in-force, active DISABLED operation row for the
    -- same operation. This is enforced here IN ADDITION TO (not instead
    -- of) the resolver's own first-disabled-wins rewrite (resolve.go) -
    -- the resolver alone is correct even if this trigger is bypassed
    -- (e.g. raw SQL), but this trigger keeps the sanctioned write path
    -- from ever recording a self-contradictory configuration in the
    -- first place.
    --
    -- PLACEMENT, LOAD-BEARING: this check must run for EVERY
    -- scope_kind='operation' row regardless of whether NEW.brand_id is
    -- NULL - it is placed HERE, immediately after the tenant-rung check
    -- and BEFORE the brand-check block's own early
    -- `IF NEW.scope_kind = 'brand' OR NEW.brand_id IS NULL THEN RETURN
    -- NEW` below. Placing it after that block (textually closer to the
    -- final RETURN NEW) would make it structurally unreachable for every
    -- tenant-wide (brand_id IS NULL) operation write - exactly the most
    -- common shape - because that branch returns before ever reaching
    -- code below it. Caught by
    -- TestOperatingCountryPolicy_MoreSpecificEnableUnderBroaderDisableIsRefused's
    -- "product-specific under every-product disable" sub-case actually
    -- failing against the first (wrongly-placed) version of this trigger.
    IF NEW.scope_kind = 'operation' THEN
        PERFORM 1
          FROM operating_country_policies d
         WHERE d.tenant_id = NEW.tenant_id
           AND d.scope_kind = 'operation'
           AND d.country_code = NEW.country_code
           AND d.operation_code = NEW.operation_code
           AND d.status = 'active'
           AND d.state = 'disabled'
           AND d.effective_to IS NULL
           AND (d.brand_id IS NULL OR d.brand_id = NEW.brand_id)
           AND (d.product_code IS NULL OR d.product_code = NEW.product_code);
        IF FOUND THEN
            RAISE EXCEPTION 'operating_country_policies: an operation policy for country % cannot be enabled while a broader in-force operation policy for the same operation is disabled; a more-specific operation/product row may only narrow and may never widen (Stage 4I Phase E, ADR 0045 §3.5-A)', NEW.country_code;
        END IF;
    END IF;

    IF NEW.scope_kind = 'brand' OR NEW.brand_id IS NULL THEN
        RETURN NEW;
    END IF;

    -- (4) Brand-specific operation row: if a brand rung exists it must be
    -- enabled; if it is absent it legitimately INHERITS the tenant rung,
    -- already verified enabled above (ruling 6).
    SELECT p.state INTO v_brand_state
      FROM operating_country_policies p
     WHERE p.tenant_id = NEW.tenant_id AND p.scope_kind = 'brand'
       AND p.brand_id = NEW.brand_id AND p.country_code = NEW.country_code
       AND p.status = 'active' AND p.effective_to IS NULL
     LIMIT 1;
    IF FOUND AND v_brand_state <> 'enabled' THEN
        RAISE EXCEPTION 'operating_country_policies: an operation policy for country % cannot be enabled under a disabled brand policy (Stage 4I Phase E)', NEW.country_code;
    END IF;

    RETURN NEW;
END; $$ LANGUAGE plpgsql;

-- The read policies back to 0076 / 0077 (they reference operating_bases, so they go before the table).
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
        )
    );

-- The classifier (0178 body) and the row matcher (0179 body).
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

ALTER TABLE licensing_change_kinds NO FORCE ROW LEVEL SECURITY;
DELETE FROM licensing_change_kinds WHERE kind IN ('operating_basis_set', 'operating_basis_withdraw', 'operating_basis_review_record');
ALTER TABLE licensing_change_kinds FORCE ROW LEVEL SECURITY;

DROP TABLE operating_bases;
DROP FUNCTION operating_bases_licence_conflict(uuid, uuid);
DROP FUNCTION operating_bases_validate();
DROP FUNCTION operating_bases_append_only();
DROP FUNCTION operating_bases_governed_guard();
