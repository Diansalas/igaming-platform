-- 0183: operating-basis hardening (lic14; independent security review of lic9). SECURITY-SENSITIVE: for `security`, `code-reviewer`.
--   M-1  the table owner may no longer close an operating_bases version without an executed request (LC041), and EVERY close needs a
--        successor row in the same scope with effective_from = the close time, checked by a DEFERRED constraint trigger (LC044).
--   M-2 / R3  partner-licence exclusivity is permanent: a licensee=tenant licence is refused for a tenant if ANY other tenant has ever
--        had an operating_bases row on it, and tenant_licence_bind (tenants.licence_id) is refused symmetrically; both take the same
--        advisory lock 'operating_bases:licence:<id>'. Every write of tenants.licence_id and of operating_bases is already bound to
--        the platform-admin session by row-level security, so the checking functions (pinned search_path) see every tenant's rows.
--   L-1  licences_read / licence_country_ceilings_read admit a licence named by the reading tenant's own basis only if it is
--        platform-licensee or not bound to / used by another tenant.
--   L-5  DB-level character-class and digit-run CHECKs on the opaque references and the note (consistent with the Go lint).
-- No role, password or grant change.

SET LOCAL lock_timeout = '5s';

-- ---- M-2: permanent exclusivity (replaces the 0180 function) ---------------------------------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION operating_bases_licence_conflict(p_licence uuid, p_tenant uuid) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT EXISTS (SELECT 1 FROM public.licences l WHERE l.id = p_licence AND l.licensee = 'tenant')
       AND (EXISTS (SELECT 1 FROM public.tenants t WHERE t.licence_id = p_licence AND t.id <> p_tenant)
            OR EXISTS (SELECT 1 FROM public.operating_bases b WHERE b.licence_id = p_licence AND b.tenant_id <> p_tenant))
$$;

CREATE FUNCTION tenants_licence_basis_exclusive() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF NEW.licence_id IS NULL OR (TG_OP = 'UPDATE' AND NEW.licence_id IS NOT DISTINCT FROM OLD.licence_id) THEN
        RETURN NEW;
    END IF;
    IF EXISTS (SELECT 1 FROM public.licences l WHERE l.id = NEW.licence_id AND l.licensee = 'tenant') THEN
        PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('operating_bases:licence:' || NEW.licence_id::text, 0));
        IF EXISTS (SELECT 1 FROM public.operating_bases b WHERE b.licence_id = NEW.licence_id AND b.tenant_id <> NEW.id) THEN
            RAISE EXCEPTION 'tenants: this tenant licence has been used by another tenant''s operating basis and cannot be bound to a different tenant (lic14)'
                USING ERRCODE = 'LC043';
        END IF;
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER tenants_licence_basis_exclusive BEFORE INSERT OR UPDATE OF licence_id ON tenants
    FOR EACH ROW EXECUTE FUNCTION tenants_licence_basis_exclusive();

-- ---- L-1: narrowed read disjuncts ---------------------------------------------------------------------------------------------
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
                   AND (licences.licensee = 'platform'
                        OR NOT EXISTS (SELECT 1 FROM tenants t2 WHERE t2.licence_id = licences.id AND t2.id <> b.tenant_id))
            )
        )
    );
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
                   AND EXISTS (SELECT 1 FROM licences l
                                WHERE l.id = b.licence_id
                                  AND (l.licensee = 'platform'
                                       OR NOT EXISTS (SELECT 1 FROM tenants t2 WHERE t2.licence_id = l.id AND t2.id <> b.tenant_id)))
            )
        )
    );

-- ---- M-1: owner close refused; every close needs a successor ----------------------------------------------------------------
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
                RETURN NEW;
            END IF;
            -- 0183 (lic14 M-1): the owner path may PROVISION a record, but it may never close a version without an executed
            -- request: closing a masking or restricting row is how a permission would be created.
            RAISE EXCEPTION 'operating_bases: a close needs a licensing change request of the matching kind executed in this transaction, even for the table owner (lic14, migration 0183)'
                USING ERRCODE = 'LC041';
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


CREATE FUNCTION operating_bases_close_requires_successor() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    PERFORM 1 FROM public.operating_bases s
     WHERE s.tenant_id = NEW.tenant_id AND s.brand_id IS NOT DISTINCT FROM NEW.brand_id AND s.country_code = NEW.country_code
       AND s.operation_code = NEW.operation_code AND s.product_code IS NOT DISTINCT FROM NEW.product_code
       AND s.effective_from = NEW.effective_to AND s.id <> NEW.id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'operating_bases: a closed version needs a successor version of the same scope that starts when it ended (a bare close would remove a masking or restricting row; lic14, migration 0183)'
            USING ERRCODE = 'LC044';
    END IF;
    RETURN NULL;
END
$$;
CREATE CONSTRAINT TRIGGER operating_bases_close_requires_successor
    AFTER UPDATE ON operating_bases
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    WHEN (OLD.effective_to IS NULL AND NEW.effective_to IS NOT NULL)
    EXECUTE FUNCTION operating_bases_close_requires_successor();

-- ---- L-5: character classes (no free prose, no contact or account shapes) ---------------------------------------------------
ALTER TABLE operating_bases
    ADD CONSTRAINT operating_bases_ref_charset CHECK (
        determination_reference ~ '^[A-Za-z0-9][A-Za-z0-9._:/#-]*$'
        AND (evidence_reference IS NULL OR evidence_reference ~ '^[A-Za-z0-9][A-Za-z0-9._:/#-]*$')
        AND (last_review_reference IS NULL OR last_review_reference ~ '^[A-Za-z0-9][A-Za-z0-9._:/#-]*$')),
    ADD CONSTRAINT operating_bases_note_charset CHECK (
        restrictions_note IS NULL OR restrictions_note ~ '^[A-Za-z0-9][A-Za-z0-9 .,;:()/#_''-]*$'),
    ADD CONSTRAINT operating_bases_no_long_digit_runs CHECK (
        pg_catalog.regexp_replace(determination_reference, '[^0-9A-Za-z]', '', 'g') !~ '[0-9]{9}'
        AND (evidence_reference IS NULL OR pg_catalog.regexp_replace(evidence_reference, '[^0-9A-Za-z]', '', 'g') !~ '[0-9]{9}')
        AND (last_review_reference IS NULL OR pg_catalog.regexp_replace(last_review_reference, '[^0-9A-Za-z]', '', 'g') !~ '[0-9]{9}')
        AND (restrictions_note IS NULL OR pg_catalog.regexp_replace(restrictions_note, '[^0-9A-Za-z]', '', 'g') !~ '[0-9]{9}'));
