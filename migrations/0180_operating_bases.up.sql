-- 0180: explicit OPERATING BASES per tenant / brand / country / operation (lic9). SECURITY-SENSITIVE (governed configuration, a new
-- permission-extending table): FOR `security`, `architect`, `code-reviewer`, `qa` REVIEW BEFORE MERGE.
-- Proposed ADR text: docs/plans/lic9-operating-basis-record.md. Runbook: docs/runbooks/operating-basis.md.
--
-- WHY. Before this migration a market could be opened only through the LICENCE ceiling of the tenant's ONE licence
-- (tenants.licence_id): a tenant with no licence resolved licensing_unknown, a brand could not rely on a different licence than
-- the tenant's, and a documented decision that no licence is needed for a country / operation could not be expressed at all.
-- The platform must not hard-code a country allowlist, must not assume every brand uses our licence and must not assume no
-- licence is needed: the operating arrangement is BUSINESS CONFIGURATION chosen later by authorised admins under maker-checker.
--
-- WHAT THIS ADDS (no value is seeded: zero operating_bases rows, no country, no jurisdiction declared licence-exempt)
--   1. operating_bases: governed, append-only, versioned, FORCE RLS. A row states, for (tenant, brand?, country, operation,
--      product?), WHICH BASIS the permission relies on: platform_licence (a licensee='platform' licence), partner_licence (a
--      licensee='tenant' licence, exclusive to one tenant) or no_licence_documented (an explicit, reviewed, dated determination
--      that names the jurisdiction relied on; review_due_at is mandatory; the resolver fails closed once it is past). Country
--      configuration alone is never proof of legality: the row carries the determination reference, the responsible function, the
--      evidence reference, restrictions and the executed change request (whose approvals ARE the decision-makers).
--   2. Three new licensing change kinds (closed registry): operating_basis_set (WIDENING, 2 distinct platform_admin Persons),
--      operating_basis_withdraw (NARROWING, single actor), operating_basis_review_record (widening iff the review date moves later
--      or needs_rereview is cleared). licensing_change_classify and licensing_change_row_matches gain the branches (0178 / 0179
--      bodies otherwise unchanged); the governed-write guard (LC040) admits an INSERT / close only with an EXECUTED request of the
--      matching kind and payload in THIS transaction. The actor proof is the existing licensing_change:* family (no new family).
--   3. licences_read and licence_country_ceilings_read ALSO admit a licence named by the reading tenant's own operating_bases
--      row (the resolver runs under the tenant's session and must read that licence and its ceilings).
--   4. operating_country_policies_enforce_ceiling: step (1) (the licence ceiling at policy-write time) is skipped for a country that
--      has an in-force, approved operating basis for the tenant; the RESOLVER stays the authoritative decider at read time. The
--      function also gains a pinned search_path (otherwise the 0076 body).
--
-- SQLSTATEs: LC040 (governed write without an executed matching request), LC041 (append-only violation on operating_bases),
-- LC043 (basis content invalid at write: licensee / kind mismatch, licence bound to another tenant, predecessor drift),
-- LC095 down refused while rows or requests exist. No SECURITY DEFINER function is added; every function pins search_path.
-- No role, password or attribute change. The only grant: SELECT, INSERT and UPDATE (effective_to) on operating_bases to
-- igaming_runtime (mirrored in deploy/init-app-role.sql).

SET LOCAL lock_timeout = '5s';

-- =========================================================================
-- 0. Pre-flight
-- =========================================================================
DO $$
BEGIN
    IF to_regclass('public.operating_bases') IS NOT NULL THEN
        RAISE EXCEPTION 'migration 0180 pre-flight: operating_bases already exists' USING ERRCODE = 'LC098';
    END IF;
    IF EXISTS (SELECT 1 FROM licensing_change_kinds WHERE kind LIKE 'operating\_basis\_%') THEN
        RAISE EXCEPTION 'migration 0180 pre-flight: an operating_basis kind already exists' USING ERRCODE = 'LC098';
    END IF;
END
$$;

-- =========================================================================
-- 1. operating_bases
-- =========================================================================
CREATE TABLE operating_bases (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id               UUID NOT NULL REFERENCES tenants (id) ON DELETE CASCADE,
    -- NULL = tenant-wide. A brand-specific row wins for that brand (resolver). The composite FK refuses a foreign brand.
    brand_id                UUID NULL,
    country_code            TEXT NOT NULL CHECK (country_code ~ '^[A-Z]{2}$'),
    -- Explicit, never defaulted: a basis is stated per operation. NULL product = every product (minus restricted_product_codes).
    operation_code          TEXT NOT NULL REFERENCES platform_operations (code),
    product_code            TEXT NULL REFERENCES platform_products (code),
    basis_kind              TEXT NOT NULL CHECK (basis_kind IN ('platform_licence', 'partner_licence', 'no_licence_documented')),
    licence_id              UUID NULL REFERENCES licences (id),
    -- The jurisdiction whose regime the basis relies on. Required for no_licence_documented; derived from the licence otherwise.
    jurisdiction_id         UUID NULL REFERENCES jurisdictions (id),
    -- OPAQUE document id of the recorded determination (no names, contact details or document content).
    determination_reference TEXT NOT NULL CHECK (determination_reference = btrim(determination_reference) AND octet_length(determination_reference) BETWEEN 1 AND 200
                                                 AND determination_reference !~ '[[:cntrl:]]' AND position('@' IN determination_reference) = 0),
    -- The responsible FUNCTION (for example head_of_compliance), never a person's name.
    determined_by_role      TEXT NOT NULL CHECK (determined_by_role ~ '^[a-z][a-z0-9_.:-]{1,99}$'),
    evidence_reference      TEXT NULL CHECK (evidence_reference IS NULL OR (evidence_reference = btrim(evidence_reference) AND octet_length(evidence_reference) BETWEEN 1 AND 200
                                                 AND evidence_reference !~ '[[:cntrl:]]' AND position('@' IN evidence_reference) = 0)),
    restrictions_note       TEXT NULL CHECK (restrictions_note IS NULL OR (btrim(restrictions_note) <> '' AND octet_length(restrictions_note) <= 1000
                                                 AND restrictions_note !~ '[[:cntrl:]]' AND position('@' IN restrictions_note) = 0)),
    restricted_product_codes TEXT[] NOT NULL DEFAULT '{}' CHECK (cardinality(restricted_product_codes) <= 64),
    review_due_at           DATE NULL,
    needs_rereview          BOOLEAN NOT NULL DEFAULT false,
    last_review_reference   TEXT NULL CHECK (last_review_reference IS NULL OR (last_review_reference = btrim(last_review_reference) AND octet_length(last_review_reference) BETWEEN 1 AND 200
                                                 AND last_review_reference !~ '[[:cntrl:]]' AND position('@' IN last_review_reference) = 0)),
    status                  TEXT NOT NULL CHECK (status IN ('active', 'withdrawn')),
    reason_code             TEXT NOT NULL CHECK (reason_code ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$'),
    policy_version          TEXT NOT NULL,
    effective_from          TIMESTAMPTZ NOT NULL DEFAULT now(),
    effective_to            TIMESTAMPTZ NULL,
    -- Set by the guard trigger from the executed request that authorised the row (never from the writer). NULL only for an
    -- owner-path (provisioning / fixture) row, which the resolver does NOT accept as an approved basis.
    change_request_id       UUID NULL REFERENCES licensing_change_requests (id) ON DELETE RESTRICT,
    created_by_actor_id     UUID NULL,
    created_at              TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT operating_bases_effective_to_after_from CHECK (effective_to IS NULL OR effective_to > effective_from),
    CONSTRAINT operating_bases_kind_shape CHECK (
        (basis_kind = 'no_licence_documented' AND licence_id IS NULL AND jurisdiction_id IS NOT NULL AND review_due_at IS NOT NULL)
        OR (basis_kind IN ('platform_licence', 'partner_licence') AND licence_id IS NOT NULL AND jurisdiction_id IS NULL)),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES brands (id, tenant_id)
);

-- One OPEN version per scope (database-enforced, never check-then-insert). Two indexes rather than a sentinel UUID for the brand.
CREATE UNIQUE INDEX uq_operating_bases_open_tenantwide
    ON operating_bases (tenant_id, country_code, operation_code, COALESCE(product_code, '*'))
    WHERE brand_id IS NULL AND effective_to IS NULL;
CREATE UNIQUE INDEX uq_operating_bases_open_brand
    ON operating_bases (tenant_id, brand_id, country_code, operation_code, COALESCE(product_code, '*'))
    WHERE brand_id IS NOT NULL AND effective_to IS NULL;
CREATE INDEX idx_operating_bases_lookup ON operating_bases (tenant_id, country_code, operation_code, effective_from DESC);
CREATE INDEX idx_operating_bases_licence ON operating_bases (tenant_id, licence_id) WHERE licence_id IS NOT NULL;

COMMENT ON TABLE operating_bases IS '0180 (lic9): the explicit, governed OPERATING BASIS a tenant / brand relies on per country and operation: platform_licence, partner_licence or no_licence_documented. Append-only versions; written only through an executed licensing change request (operating_basis_set / _withdraw / _review_record); the executing and approving Persons are derivable from change_request_id. NOT proof of legality: a recorded, reviewed business decision. Zero rows are shipped; no jurisdiction is declared licence-exempt anywhere in code or data.';

-- Does p_licence (a licensee = 'tenant' licence) already belong to a DIFFERENT tenant, by tenants.licence_id or by another tenant's
-- in-force basis? Used by the classifier (early answer) and by the write trigger (authoritative). Platform-licensee licences are
-- shared by design and never conflict.
CREATE FUNCTION operating_bases_licence_conflict(p_licence uuid, p_tenant uuid) RETURNS boolean
    LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT EXISTS (SELECT 1 FROM public.licences l WHERE l.id = p_licence AND l.licensee = 'tenant')
       AND (EXISTS (SELECT 1 FROM public.tenants t WHERE t.licence_id = p_licence AND t.id <> p_tenant)
            OR EXISTS (SELECT 1 FROM public.operating_bases b WHERE b.licence_id = p_licence AND b.tenant_id <> p_tenant
                        AND b.effective_to IS NULL AND b.status = 'active'))
$$;

-- BEFORE INSERT: forge-proof stamping (a non-owner row starts now and open), restricted product codes must exist, and the
-- licence must match the kind (platform_licence -> licensee platform; partner_licence -> licensee tenant and not bound to another
-- tenant). A tenant-licensee licence is serialised by an advisory lock so two tenants cannot claim it concurrently.
CREATE FUNCTION operating_bases_validate() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_licensee text;
    v_expect   text;
BEGIN
    IF NOT public.launch_is_table_owner() THEN
        NEW.effective_from := now();
        NEW.effective_to := NULL;
        NEW.created_at := now();
    END IF;
    IF EXISTS (SELECT 1 FROM unnest(NEW.restricted_product_codes) c WHERE NOT EXISTS (SELECT 1 FROM public.platform_products pp WHERE pp.code = c)) THEN
        RAISE EXCEPTION 'operating_bases: unknown restricted product code' USING ERRCODE = 'LC043';
    END IF;
    IF NEW.licence_id IS NOT NULL THEN
        SELECT l.licensee INTO v_licensee FROM public.licences l WHERE l.id = NEW.licence_id;
        IF NOT FOUND THEN
            RAISE EXCEPTION 'operating_bases: unknown licence' USING ERRCODE = 'LC043';
        END IF;
        v_expect := CASE NEW.basis_kind WHEN 'platform_licence' THEN 'platform' ELSE 'tenant' END;
        IF v_licensee <> v_expect THEN
            RAISE EXCEPTION 'operating_bases: the licence licensee does not match the basis kind' USING ERRCODE = 'LC043';
        END IF;
        IF v_licensee = 'tenant' THEN
            PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('operating_bases:licence:' || NEW.licence_id::text, 0));
            IF public.operating_bases_licence_conflict(NEW.licence_id, NEW.tenant_id) THEN
                RAISE EXCEPTION 'operating_bases: this tenant licence is already bound to a different tenant' USING ERRCODE = 'LC043';
            END IF;
        END IF;
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER operating_bases_a_validate BEFORE INSERT ON operating_bases
    FOR EACH ROW EXECUTE FUNCTION operating_bases_validate();

-- BEFORE UPDATE / TRUNCATE: append-only. The only change ever allowed is effective_to, once, NULL -> now() (the close of a version
-- that is replaced in the same transaction). DELETE has no policy and no grant; the tenants ON DELETE CASCADE is the only remover
-- (referential actions bypass RLS - the 0076 operating_country_policies precedent, hence no DELETE trigger here either).
CREATE FUNCTION operating_bases_append_only() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'operating_bases is append-only: TRUNCATE is not permitted' USING ERRCODE = 'LC041';
    END IF;
    IF OLD.effective_to IS NOT NULL THEN
        RAISE EXCEPTION 'operating_bases: a closed version may not be reopened or re-closed' USING ERRCODE = 'LC041';
    END IF;
    IF NEW.effective_to IS NULL THEN
        RAISE EXCEPTION 'operating_bases: only effective_to may change, and only to close the version' USING ERRCODE = 'LC041';
    END IF;
    IF NOT public.launch_is_table_owner() THEN
        NEW.effective_to := now();
    END IF;
    IF (pg_catalog.to_jsonb(NEW) - 'effective_to') IS DISTINCT FROM (pg_catalog.to_jsonb(OLD) - 'effective_to') THEN
        RAISE EXCEPTION 'operating_bases: only effective_to may change after insert' USING ERRCODE = 'LC041';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER operating_bases_b_append_only BEFORE UPDATE ON operating_bases
    FOR EACH ROW EXECUTE FUNCTION operating_bases_append_only();
CREATE TRIGGER operating_bases_deny_truncate BEFORE TRUNCATE ON operating_bases
    FOR EACH STATEMENT EXECUTE FUNCTION operating_bases_append_only();

-- The governed-write guard (LC040). Runs LAST (zz_). An INSERT or close by a non-owner needs an EXECUTED request of the matching kind
-- and payload executed in THIS transaction. For an INSERT it stamps change_request_id and created_by_actor_id from that request
-- (the writer never supplies them); a withdrawal / review record must equal its predecessor except for the columns the kind
-- changes, and a row that is widening by content (later review date / cleared needs_rereview) additionally needs a request that
-- was itself classified widening and holds the kind's approvals. The table owner keeps its provisioning path (fixtures) but its
-- rows carry NULL change_request_id, which the resolver does not accept as an approved basis.
CREATE FUNCTION operating_bases_governed_guard() RETURNS trigger
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
CREATE TRIGGER zz_licensing_change_governed BEFORE INSERT OR UPDATE ON operating_bases
    FOR EACH ROW EXECUTE FUNCTION operating_bases_governed_guard();

-- =========================================================================
-- 2. RLS: platform writes (executor), tenant reads its own rows, platform reads all
-- =========================================================================
ALTER TABLE operating_bases ENABLE ROW LEVEL SECURITY;
ALTER TABLE operating_bases FORCE ROW LEVEL SECURITY;

-- The leading player conjunct is mandatory: a player-scoped connection also sets app.tenant_id.
CREATE POLICY operating_bases_tenant_read ON operating_bases FOR SELECT
    USING (NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY operating_bases_platform_read ON operating_bases FOR SELECT
    USING (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present());
CREATE POLICY operating_bases_platform_insert ON operating_bases FOR INSERT
    WITH CHECK (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
                AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
                AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
                AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
                AND NOT financial_acting_gucs_present());
CREATE POLICY operating_bases_platform_close ON operating_bases FOR UPDATE
    USING (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
           AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
           AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
           AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
           AND NOT financial_acting_gucs_present())
    WITH CHECK (NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
                AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
                AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
                AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
                AND NOT financial_acting_gucs_present());
-- No DELETE policy, no FOR ALL policy, no tenant write policy, no player policy.

-- The resolver runs under the TENANT's session: it must read the licence (and its ceilings) a basis of that tenant names.
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

-- =========================================================================
-- 3. The kind registry rows, classifier and row-match branches
-- =========================================================================
-- The registry is FORCE RLS with no write policy (0177): the migration owner lifts FORCE for the INSERT only (inside this transaction).
ALTER TABLE licensing_change_kinds NO FORCE ROW LEVEL SECURITY;
INSERT INTO licensing_change_kinds (kind, platform_only, widening_approvals, approver_roles, description) VALUES
    ('operating_basis_set', true, 2, ARRAY['platform_admin'], 'Set an operating basis (platform licence, partner licence or documented no-licence basis) for a tenant / brand, country and operation (always widening).'),
    ('operating_basis_withdraw', true, 2, ARRAY['platform_admin'], 'Withdraw the active operating basis of a scope (always narrowing: single actor).'),
    ('operating_basis_review_record', true, 2, ARRAY['platform_admin'], 'Record a review of an operating basis (widening iff the review date moves later or the re-review flag is cleared).');
ALTER TABLE licensing_change_kinds FORCE ROW LEVEL SECURITY;

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
    v_basis  public.operating_bases%ROWTYPE;
    v_bkind  text;
    v_licensee text;
    v_expect text;
    v_due    date;
    v_flag   boolean;
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

    -- 0180 (lic9, operating bases). The DATABASE classifies: a set is always WIDENING (it creates or replaces a permission); a
    -- withdrawal is always narrowing; a review record is widening iff it moves the review date later or clears needs_rereview.
    IF p_kind IN ('operating_basis_set', 'operating_basis_withdraw', 'operating_basis_review_record') THEN
        IF NOT public.licensing_change_keys_are(p, CASE p_kind
                WHEN 'operating_basis_set' THEN ARRAY['tenant_id', 'brand_id', 'country_code', 'operation_code', 'product_code', 'basis_kind',
                    'licence_id', 'jurisdiction_id', 'determination_reference', 'determined_by_role', 'basis_evidence_reference',
                    'restrictions_note', 'restricted_product_codes', 'review_due_at']
                WHEN 'operating_basis_withdraw' THEN ARRAY['tenant_id', 'brand_id', 'country_code', 'operation_code', 'product_code']
                ELSE ARRAY['tenant_id', 'brand_id', 'country_code', 'operation_code', 'product_code', 'review_due_at', 'needs_rereview',
                    'review_reference'] END)
           OR NOT public.licensing_change_is_uuid(p -> 'tenant_id')
           OR NOT (COALESCE(pg_catalog.jsonb_typeof(p -> 'brand_id') = 'null', false) OR public.licensing_change_is_uuid(p -> 'brand_id'))
           OR NOT COALESCE((p ->> 'country_code') ~ '^[A-Z]{2}$', false)
           OR NOT public.licensing_change_is_text(p -> 'operation_code', 64)
           OR NOT public.licensing_change_is_opt_text(p -> 'product_code', 64) THEN
            RAISE EXCEPTION 'licensing change: invalid % payload', p_kind USING ERRCODE = 'LC010';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM public.tenants t WHERE t.id = (p ->> 'tenant_id')::uuid) THEN
            RAISE EXCEPTION 'licensing change: unknown tenant' USING ERRCODE = 'LC010';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM public.platform_operations o WHERE o.code = p ->> 'operation_code' AND o.active) THEN
            RAISE EXCEPTION 'licensing change: unknown or inactive operation' USING ERRCODE = 'LC010';
        END IF;
        IF p ->> 'product_code' IS NOT NULL
           AND NOT EXISTS (SELECT 1 FROM public.platform_products pp WHERE pp.code = p ->> 'product_code' AND pp.active) THEN
            RAISE EXCEPTION 'licensing change: unknown or inactive product' USING ERRCODE = 'LC010';
        END IF;
        subject_tenant := (p ->> 'tenant_id')::uuid;

        IF p_kind = 'operating_basis_set' THEN
            v_bkind := p ->> 'basis_kind';
            IF pg_catalog.jsonb_typeof(p -> 'basis_kind') IS DISTINCT FROM 'string'
               OR NOT COALESCE(v_bkind IN ('platform_licence', 'partner_licence', 'no_licence_documented'), false)
               OR NOT public.licensing_change_is_text(p -> 'determination_reference', 200)
               OR (p ->> 'determination_reference') <> pg_catalog.btrim(p ->> 'determination_reference')
               OR (p ->> 'determination_reference') LIKE '%@%'
               OR pg_catalog.jsonb_typeof(p -> 'determined_by_role') IS DISTINCT FROM 'string'
               OR NOT COALESCE((p ->> 'determined_by_role') ~ '^[a-z][a-z0-9_.:-]{1,99}$', false)
               OR NOT public.licensing_change_is_opt_text(p -> 'basis_evidence_reference', 200) OR (p ->> 'basis_evidence_reference') LIKE '%@%'
               OR NOT public.licensing_change_is_opt_text(p -> 'restrictions_note', 1000) OR (p ->> 'restrictions_note') LIKE '%@%'
               OR NOT (COALESCE(pg_catalog.jsonb_typeof(p -> 'jurisdiction_id') = 'null', false) OR public.licensing_change_is_uuid(p -> 'jurisdiction_id'))
               OR NOT (COALESCE(pg_catalog.jsonb_typeof(p -> 'licence_id') = 'null', false) OR public.licensing_change_is_uuid(p -> 'licence_id'))
               OR NOT (COALESCE(pg_catalog.jsonb_typeof(p -> 'review_due_at') = 'null', false)
                       OR (pg_catalog.jsonb_typeof(p -> 'review_due_at') = 'string' AND (p ->> 'review_due_at') ~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}$'))
               OR NOT public.licensing_change_is_text_array(p -> 'restricted_product_codes', 64) THEN
                RAISE EXCEPTION 'licensing change: invalid operating_basis_set payload' USING ERRCODE = 'LC010';
            END IF;
            BEGIN
                v_due := (p ->> 'review_due_at')::date;
            EXCEPTION WHEN others THEN
                RAISE EXCEPTION 'licensing change: invalid operating_basis_set review_due_at' USING ERRCODE = 'LC010';
            END;
            IF v_due IS NOT NULL AND v_due <= (pg_catalog.now() AT TIME ZONE 'UTC')::date THEN
                RAISE EXCEPTION 'licensing change: review_due_at must be in the future' USING ERRCODE = 'LC010';
            END IF;
            IF EXISTS (SELECT 1 FROM pg_catalog.jsonb_array_elements_text(p -> 'restricted_product_codes') c
                        WHERE NOT EXISTS (SELECT 1 FROM public.platform_products pp WHERE pp.code = c)) THEN
                RAISE EXCEPTION 'licensing change: unknown restricted product code' USING ERRCODE = 'LC010';
            END IF;
            IF v_bkind = 'no_licence_documented' THEN
                -- A documented no-licence basis: no licence, the jurisdiction whose regime it relies on, and a mandatory review date.
                IF pg_catalog.jsonb_typeof(p -> 'licence_id') <> 'null' OR NOT public.licensing_change_is_uuid(p -> 'jurisdiction_id') OR v_due IS NULL THEN
                    RAISE EXCEPTION 'licensing change: a no_licence_documented basis needs a jurisdiction_id and a review_due_at and carries no licence_id' USING ERRCODE = 'LC010';
                END IF;
                IF NOT EXISTS (SELECT 1 FROM public.jurisdictions j WHERE j.id = (p ->> 'jurisdiction_id')::uuid) THEN
                    RAISE EXCEPTION 'licensing change: unknown jurisdiction' USING ERRCODE = 'LC010';
                END IF;
            ELSE
                IF NOT public.licensing_change_is_uuid(p -> 'licence_id') OR pg_catalog.jsonb_typeof(p -> 'jurisdiction_id') <> 'null' THEN
                    RAISE EXCEPTION 'licensing change: a licence basis needs a licence_id and carries no jurisdiction_id' USING ERRCODE = 'LC010';
                END IF;
                SELECT l.licensee INTO v_licensee FROM public.licences l WHERE l.id = (p ->> 'licence_id')::uuid;
                IF NOT FOUND THEN
                    RAISE EXCEPTION 'licensing change: unknown licence' USING ERRCODE = 'LC010';
                END IF;
                v_expect := CASE v_bkind WHEN 'platform_licence' THEN 'platform' ELSE 'tenant' END;
                IF v_licensee <> v_expect THEN
                    RAISE EXCEPTION 'licensing change: the licence licensee does not match the basis kind' USING ERRCODE = 'LC010';
                END IF;
                IF public.operating_bases_licence_conflict((p ->> 'licence_id')::uuid, subject_tenant) THEN
                    RAISE EXCEPTION 'licensing change: this tenant licence is already bound to a different tenant' USING ERRCODE = 'LC010';
                END IF;
                subject_licence := (p ->> 'licence_id')::uuid;
            END IF;
            widening := true;
            RETURN;
        END IF;

        -- withdraw / review record: both act on the OPEN, ACTIVE version of the scope (locked for a platform session).
        IF NULLIF(pg_catalog.current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL THEN
            SELECT * INTO v_basis FROM public.operating_bases b
             WHERE b.tenant_id = subject_tenant AND b.brand_id IS NOT DISTINCT FROM (p ->> 'brand_id')::uuid
               AND b.country_code = p ->> 'country_code' AND b.operation_code = p ->> 'operation_code'
               AND b.product_code IS NOT DISTINCT FROM (p ->> 'product_code') AND b.effective_to IS NULL
             ORDER BY b.effective_from DESC LIMIT 1 FOR UPDATE;
        ELSE
            SELECT * INTO v_basis FROM public.operating_bases b
             WHERE b.tenant_id = subject_tenant AND b.brand_id IS NOT DISTINCT FROM (p ->> 'brand_id')::uuid
               AND b.country_code = p ->> 'country_code' AND b.operation_code = p ->> 'operation_code'
               AND b.product_code IS NOT DISTINCT FROM (p ->> 'product_code') AND b.effective_to IS NULL
             ORDER BY b.effective_from DESC LIMIT 1;
        END IF;
        IF NOT FOUND OR v_basis.status <> 'active' THEN
            RAISE EXCEPTION 'licensing change: there is no active operating basis for this scope' USING ERRCODE = 'LC010';
        END IF;
        subject_licence := v_basis.licence_id;

        IF p_kind = 'operating_basis_withdraw' THEN
            widening := false;
            RETURN;
        END IF;

        -- operating_basis_review_record
        IF pg_catalog.jsonb_typeof(p -> 'review_due_at') IS DISTINCT FROM 'string' OR (p ->> 'review_due_at') !~ '^[0-9]{4}-[0-9]{2}-[0-9]{2}$'
           OR pg_catalog.jsonb_typeof(p -> 'needs_rereview') IS DISTINCT FROM 'boolean'
           OR NOT public.licensing_change_is_text(p -> 'review_reference', 200)
           OR (p ->> 'review_reference') <> pg_catalog.btrim(p ->> 'review_reference') OR (p ->> 'review_reference') LIKE '%@%' THEN
            RAISE EXCEPTION 'licensing change: invalid operating_basis_review_record payload' USING ERRCODE = 'LC010';
        END IF;
        BEGIN
            v_due := (p ->> 'review_due_at')::date;
        EXCEPTION WHEN others THEN
            RAISE EXCEPTION 'licensing change: invalid operating_basis_review_record review_due_at' USING ERRCODE = 'LC010';
        END;
        v_flag := (p ->> 'needs_rereview')::boolean;
        IF v_due IS NOT DISTINCT FROM v_basis.review_due_at AND v_flag = v_basis.needs_rereview THEN
            RAISE EXCEPTION 'licensing change: the review date and the re-review flag are unchanged' USING ERRCODE = 'LC010';
        END IF;
        -- Later review date, or clearing needs_rereview, extends the permission (widening). A first review date on a basis that had
        -- none, an earlier date and marking needs_rereview all add oversight (narrowing).
        widening := (v_basis.needs_rereview AND NOT v_flag) OR (v_basis.review_due_at IS NOT NULL AND v_due > v_basis.review_due_at);
        IF widening AND v_due <= (pg_catalog.now() AT TIME ZONE 'UTC')::date THEN
            RAISE EXCEPTION 'licensing change: an extended review date must be in the future' USING ERRCODE = 'LC010';
        END IF;
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
        WHEN p_table = 'operating_bases' AND p_op = 'INSERT' THEN
            p_new ->> 'tenant_id' = p ->> 'tenant_id'
            AND (p_new ->> 'brand_id') IS NOT DISTINCT FROM (p ->> 'brand_id')
            AND p_new ->> 'country_code' = p ->> 'country_code'
            AND p_new ->> 'operation_code' = p ->> 'operation_code'
            AND (p_new ->> 'product_code') IS NOT DISTINCT FROM (p ->> 'product_code')
            AND p_new ->> 'reason_code' = p_reason
            AND CASE p_kind
                WHEN 'operating_basis_set' THEN
                    p_new ->> 'status' = 'active'
                    AND p_new ->> 'basis_kind' = p ->> 'basis_kind'
                    AND (p_new ->> 'licence_id') IS NOT DISTINCT FROM (p ->> 'licence_id')
                    AND (p_new ->> 'jurisdiction_id') IS NOT DISTINCT FROM (p ->> 'jurisdiction_id')
                    AND p_new ->> 'determination_reference' = p ->> 'determination_reference'
                    AND p_new ->> 'determined_by_role' = p ->> 'determined_by_role'
                    AND (p_new ->> 'evidence_reference') IS NOT DISTINCT FROM (p ->> 'basis_evidence_reference')
                    AND (p_new ->> 'restrictions_note') IS NOT DISTINCT FROM (p ->> 'restrictions_note')
                    AND p_new -> 'restricted_product_codes' = p -> 'restricted_product_codes'
                    AND (p_new ->> 'review_due_at') IS NOT DISTINCT FROM (p ->> 'review_due_at')
                    AND p_new ->> 'needs_rereview' = 'false'
                    AND p_new ->> 'last_review_reference' IS NULL
                WHEN 'operating_basis_withdraw' THEN
                    p_new ->> 'status' = 'withdrawn'
                WHEN 'operating_basis_review_record' THEN
                    p_new ->> 'status' = 'active'
                    AND p_new ->> 'review_due_at' = p ->> 'review_due_at'
                    AND p_new ->> 'needs_rereview' = p ->> 'needs_rereview'
                    AND p_new ->> 'last_review_reference' = p ->> 'review_reference'
                ELSE false END
        WHEN p_table = 'operating_bases' AND p_op = 'UPDATE' THEN
            p_kind IN ('operating_basis_set', 'operating_basis_withdraw', 'operating_basis_review_record')
            AND p_old ->> 'tenant_id' = p ->> 'tenant_id'
            AND (p_old ->> 'brand_id') IS NOT DISTINCT FROM (p ->> 'brand_id')
            AND p_old ->> 'country_code' = p ->> 'country_code'
            AND p_old ->> 'operation_code' = p ->> 'operation_code'
            AND (p_old ->> 'product_code') IS NOT DISTINCT FROM (p ->> 'product_code')
        ELSE false
    END, false)
$$;

-- =========================================================================
-- 4. Policy-write ceiling check: skipped where an approved, in-force basis governs the country
-- =========================================================================
-- Step (1) required the tenant's single licence to enable the country. A country governed by an explicit operating basis is decided
-- by the resolver (per brand and operation, from the basis' own licence or documented no-licence determination); the write-time
-- trigger would otherwise refuse every policy of a tenant with no licence or a per-brand partner licence. Steps (2)-(4) are
-- byte-for-byte the 0076 body. The function gains a pinned search_path.
CREATE OR REPLACE FUNCTION operating_country_policies_enforce_ceiling() RETURNS TRIGGER
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_licence_id      UUID;
    v_ceiling_state   TEXT;
    v_tenant_state    TEXT;
    v_brand_state     TEXT;
BEGIN
    IF NEW.state = 'disabled' OR NEW.status = 'withdrawn' THEN
        -- Narrowing-or-neutral AS FAR AS THIS FUNCTION'S UPWARD CEILING CHECKS GO (see the 0076 comment: a withdrawal at the brand
        -- or operation rung DOES widen; it is controlled by ocp_inherit_rung_withdrawal_requires_authorization).
        RETURN NEW;
    END IF;

    -- (1) THE CEILING, unless an approved in-force operating basis governs this country for the tenant.
    IF NOT EXISTS (
        SELECT 1 FROM operating_bases b
         WHERE b.tenant_id = NEW.tenant_id AND b.country_code = NEW.country_code
           AND b.status = 'active' AND b.change_request_id IS NOT NULL
           AND b.effective_from <= now() AND (b.effective_to IS NULL OR b.effective_to > now())) THEN
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
    END IF;

    IF NEW.scope_kind = 'tenant' THEN
        RETURN NEW;
    END IF;

    SELECT p.state INTO v_tenant_state
      FROM operating_country_policies p
     WHERE p.tenant_id = NEW.tenant_id AND p.scope_kind = 'tenant'
       AND p.country_code = NEW.country_code
       AND p.status = 'active' AND p.effective_to IS NULL
     LIMIT 1;
    IF NOT FOUND OR v_tenant_state <> 'enabled' THEN
        RAISE EXCEPTION 'operating_country_policies: a % policy for country % cannot be enabled before/beyond the tenant-scope policy it narrows (Stage 4I Phase E)', NEW.scope_kind, NEW.country_code;
    END IF;

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
END; $$;

-- =========================================================================
-- 5. Grants to igaming_runtime (mirrored in deploy/init-app-role.sql)
-- =========================================================================
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON operating_bases FROM igaming_runtime';
        EXECUTE 'GRANT SELECT, INSERT ON operating_bases TO igaming_runtime';
        EXECUTE 'GRANT UPDATE (effective_to) ON operating_bases TO igaming_runtime';
    END IF;
END
$$;
