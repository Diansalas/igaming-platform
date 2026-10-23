-- c4: a GOVERNED path to LOOSEN the AML screening policy. COMPLIANCE / SECURITY MIGRATION: FOR `security` (BLOCKING), `identity-compliance`
-- and `qa` REVIEW BEFORE MERGE.
--
-- Today the screening policy route is tighten-only (0189 aml_screening_policy_author: policy_loosening_refused) and the unrouted
-- 0154 setter is an operator action. This migration adds the loosening path as TWO new licensing change kinds (the 0177 primitive:
-- two distinct platform_admin Persons, requester never decides, approvals at most 24 h old, request expiry 72 h, signed actor
-- proof on request / approve / execute) and nothing else:
--
--   aml_screening_policy_loosen        ALWAYS widening. Executes by writing the next aml_screening_policies version IN THE SAME
--                                      TRANSACTION as the final approval (screening_policy_loosen_apply). Refused unless the
--                                      tenant's loosening capability is configured AND enabled AND the cooling period (measured
--                                      from filing) has elapsed. The engineering floors of 0189 (interval >= 3600 s and >= twice
--                                      the withdrawal gate alert threshold, caps of one year) still apply: loosening never goes
--                                      below them.
--   aml_screening_loosening_configure  Sets the per-tenant capability (enabled + cooling period) as an append-only version.
--                                      WIDENING (two Persons) iff it enables loosening or shortens the cooling period; DISABLING
--                                      or LENGTHENING the cooling period is narrowing (single platform_admin, executes at once).
--
-- INERT BY DEFAULT: no settings row ships. "Not configured" and "disabled" refuse every loosening request (LC050 with a closed
-- HINT). NO VALUE SHIPS: the cooling period is configuration written by a recorded, two-person decision; there is no default.
--
-- This migration touches NEITHER the withdrawal gate nor its production guard: aml_screening_trigger_policy_set, the heartbeat and
-- aml_screening_gate_environment_ok() are unchanged; a loosening request also REFUSES (gate_environment_refused) when the
-- jurisdiction's latest trigger policy gates withdrawals in a database whose environment anchor is not development / staging / test.
--
-- The rename-and-wrap of licensing_change_classify is deliberate: the 0180 body is not copied (a 500-line duplicate). The base
-- function keeps its body byte for byte under the name licensing_change_classify_base; a later migration that replaces
-- licensing_change_classify must keep the two new branches (the migration test and the static pin fail otherwise).
--
-- SQLSTATE: LC050 (new; a screening loosening refusal carrying a closed HINT listed in aml.RefusalCodes), LC010 invalid payload,
-- LC001/LC002 session, LC040 governed write without an executed request, LC095 down refused.
SET LOCAL lock_timeout = '5s';

DO $$
BEGIN
    IF to_regprocedure('public.licensing_change_classify(text, jsonb)') IS NULL OR to_regclass('public.aml_screening_policies') IS NULL
       OR to_regclass('public.licensing_change_requests') IS NULL OR to_regprocedure('public.aml_screening_gate_environment_ok()') IS NULL THEN
        RAISE EXCEPTION 'migration 0190 pre-flight: migrations 0154, 0177 and 0181 are not applied' USING ERRCODE = 'LC098';
    END IF;
    IF to_regclass('public.aml_screening_loosening_settings') IS NOT NULL OR to_regprocedure('public.licensing_change_classify_base(text, jsonb)') IS NOT NULL THEN
        RAISE EXCEPTION 'migration 0190 pre-flight: a loosening object already exists' USING ERRCODE = 'LC098';
    END IF;
END
$$;

-- =========================================================================
-- 1. The per-tenant capability (append-only versions; the latest row wins; no row = not configured)
-- =========================================================================
CREATE TABLE aml_screening_loosening_settings (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    enabled              BOOLEAN NOT NULL,
    -- Seconds between FILING a loosening request and the earliest moment it can execute. 0 is a stated value, not a default. The upper
    -- bound is structural, not a policy choice: a request expires 72 h after filing, so a longer period could never be satisfied.
    cooling_seconds      BIGINT NULL CHECK (cooling_seconds IS NULL OR cooling_seconds BETWEEN 0 AND 255600),
    evidence_reference   TEXT NOT NULL CHECK (evidence_reference ~ '^[A-Za-z0-9][A-Za-z0-9._:/#-]{0,199}$'),
    reason_code          TEXT NOT NULL CHECK (reason_code ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$'),
    change_request_id    UUID NOT NULL UNIQUE REFERENCES licensing_change_requests (id) ON DELETE RESTRICT,
    created_by           UUID NOT NULL,
    created_by_person_id UUID NOT NULL,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    CONSTRAINT aml_screening_loosening_settings_state CHECK (enabled = (cooling_seconds IS NOT NULL))
);
CREATE INDEX aml_screening_loosening_settings_latest ON aml_screening_loosening_settings (tenant_id, created_at DESC, id DESC);
COMMENT ON TABLE aml_screening_loosening_settings IS '0190 (c4): append-only per-tenant versions of the screening-policy loosening capability. No row ships (not configured = loosening refused). Written only by screening_loosening_settings_apply, i.e. by an executed aml_screening_loosening_configure licensing change.';

ALTER TABLE aml_screening_loosening_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE aml_screening_loosening_settings FORCE ROW LEVEL SECURITY;
CREATE POLICY aml_owner_session ON aml_screening_loosening_settings FOR ALL TO CURRENT_USER
    USING (tenant_id = public.aml_session_tenant()) WITH CHECK (tenant_id = public.aml_session_tenant());
REVOKE ALL ON aml_screening_loosening_settings FROM PUBLIC;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON aml_screening_loosening_settings FROM igaming_runtime';
    END IF;
END
$$;
CREATE TRIGGER aml_screening_loosening_settings_append_only BEFORE UPDATE OR DELETE ON aml_screening_loosening_settings
    FOR EACH ROW EXECUTE FUNCTION aml_deny_mutation();
CREATE TRIGGER aml_screening_loosening_settings_no_truncate BEFORE TRUNCATE ON aml_screening_loosening_settings
    FOR EACH STATEMENT EXECUTE FUNCTION aml_deny_mutation();

-- The second, independent check behind the apply function: an INSERT is admitted only with a licensing change request of the
-- configure kind, executed in THIS transaction by the row's author, whose payload is this row.
CREATE FUNCTION aml_screening_loosening_settings_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_prev text;
    v_ok   boolean;
BEGIN
    v_prev := NULLIF(pg_catalog.current_setting('app.tenant_id', true), '');
    IF v_prev IS NOT NULL AND NULLIF(pg_catalog.current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL THEN
        PERFORM pg_catalog.set_config('app.tenant_id', '', true);
    ELSE
        v_prev := NULL;
    END IF;
    SELECT EXISTS (
        SELECT 1 FROM public.licensing_change_requests r
         WHERE r.id = NEW.change_request_id AND r.kind = 'aml_screening_loosening_configure' AND r.status = 'executed'
           AND r.executed_txid = pg_catalog.txid_current() AND r.executed_by = NEW.created_by
           AND r.reason_code = NEW.reason_code
           AND r.payload ->> 'tenant_id' = NEW.tenant_id::text
           AND (r.payload -> 'enabled') = pg_catalog.to_jsonb(NEW.enabled)
           AND COALESCE(r.payload -> 'cooling_seconds', 'null'::jsonb) = COALESCE(pg_catalog.to_jsonb(NEW.cooling_seconds), 'null'::jsonb)
           AND r.payload ->> 'evidence_reference' = NEW.evidence_reference
           AND EXISTS (SELECT 1 FROM public.staff_users s WHERE s.id = NEW.created_by AND s.person_id = NEW.created_by_person_id AND s.tenant_id IS NULL)
    ) INTO v_ok;
    IF v_prev IS NOT NULL THEN
        PERFORM pg_catalog.set_config('app.tenant_id', v_prev, true);
    END IF;
    IF NOT v_ok THEN
        RAISE EXCEPTION 'aml_screening_loosening_settings: a row needs an executed aml_screening_loosening_configure licensing change of this transaction' USING ERRCODE = 'LC040';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER aml_screening_loosening_settings_10_guard BEFORE INSERT ON aml_screening_loosening_settings
    FOR EACH ROW EXECUTE FUNCTION aml_screening_loosening_settings_guard();
REVOKE ALL ON FUNCTION aml_screening_loosening_settings_guard() FROM PUBLIC;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON FUNCTION aml_screening_loosening_settings_guard() FROM igaming_runtime';
    END IF;
END
$$;

-- =========================================================================
-- 2. aml_screening_policies: a version may be authored by an executed loosening request (scope 'platform')
-- =========================================================================
ALTER TABLE aml_screening_policies ADD COLUMN change_request_id UUID NULL REFERENCES licensing_change_requests (id) ON DELETE RESTRICT;
ALTER TABLE aml_screening_policies DROP CONSTRAINT aml_screening_policies_created_by_scope_check;
ALTER TABLE aml_screening_policies ADD CONSTRAINT aml_screening_policies_created_by_scope_check CHECK (created_by_scope IN ('platform_acting', 'platform'));
ALTER TABLE aml_screening_policies ADD CONSTRAINT aml_screening_policies_scope_request CHECK ((created_by_scope = 'platform') = (change_request_id IS NOT NULL));
CREATE UNIQUE INDEX aml_screening_policies_change_request ON aml_screening_policies (change_request_id) WHERE change_request_id IS NOT NULL;

CREATE OR REPLACE FUNCTION aml_screening_policies_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor record;
    v_prev  text;
    v_ok    boolean;
BEGIN
    IF NEW.created_by_scope = 'platform' THEN
        -- c4 (0190): a version authored by an executed aml_screening_policy_loosen licensing change of THIS transaction, whose payload
        -- is this row (the request rows are platform-scoped: the tenant setting is cleared for the read and restored).
        v_prev := NULLIF(pg_catalog.current_setting('app.tenant_id', true), '');
        IF v_prev IS NOT NULL AND NULLIF(pg_catalog.current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL THEN
            PERFORM pg_catalog.set_config('app.tenant_id', '', true);
        ELSE
            v_prev := NULL;
        END IF;
        SELECT EXISTS (
            SELECT 1 FROM public.licensing_change_requests r
             WHERE r.id = NEW.change_request_id AND r.kind = 'aml_screening_policy_loosen' AND r.status = 'executed'
               AND r.executed_txid = pg_catalog.txid_current() AND r.executed_by = NEW.created_by
               AND r.payload ->> 'tenant_id' = NEW.tenant_id::text AND r.payload ->> 'jurisdiction_id' = NEW.licensing_jurisdiction_id::text
               AND ARRAY(SELECT pg_catalog.jsonb_array_elements_text(r.payload -> 'lists')) = NEW.lists
               AND (r.payload ->> 'rescreen_interval_seconds')::bigint = NEW.rescreen_interval_seconds
               AND (r.payload ->> 'backfill_grace_seconds')::bigint = NEW.backfill_grace_seconds
               AND r.payload ->> 'legal_review_reference' = NEW.legal_review_reference
               AND EXISTS (SELECT 1 FROM public.staff_users s WHERE s.id = NEW.created_by AND s.person_id = NEW.created_by_person_id AND s.tenant_id IS NULL)
        ) INTO v_ok;
        IF v_prev IS NOT NULL THEN
            PERFORM pg_catalog.set_config('app.tenant_id', v_prev, true);
        END IF;
        IF NOT v_ok THEN
            RAISE EXCEPTION 'aml: a platform-authored policy version needs an executed aml_screening_policy_loosen licensing change of this transaction' USING ERRCODE = 'LC040';
        END IF;
        RETURN NEW;
    END IF;
    SELECT * INTO v_actor FROM public.financial_actor_session();
    IF NEW.created_by IS DISTINCT FROM v_actor.actor OR NEW.created_by_scope IS DISTINCT FROM v_actor.scope
       OR NEW.created_by_person_id IS DISTINCT FROM v_actor.person_id THEN
        RAISE EXCEPTION 'aml: the policy author columns are not the proven session actor' USING ERRCODE = 'AM001';
    END IF;
    RETURN NEW;
END
$$;

-- =========================================================================
-- 3. Kind registry rows (closed; written by migrations only)
-- =========================================================================
ALTER TABLE licensing_change_kinds NO FORCE ROW LEVEL SECURITY;
INSERT INTO licensing_change_kinds (kind, platform_only, widening_approvals, approver_roles, description) VALUES
    ('aml_screening_policy_loosen', true, 2, ARRAY['platform_admin'], 'Author a LESS strict screening policy version (a list removed, a longer re-screen interval or backfill grace) for a tenant and jurisdiction (always widening, refused unless the tenant loosening capability is configured, enabled and its cooling period has elapsed).'),
    ('aml_screening_loosening_configure', true, 2, ARRAY['platform_admin'], 'Set the per-tenant screening-policy loosening capability (enabled + cooling period); widening iff it enables loosening or shortens the cooling period, otherwise narrowing (single actor).');
ALTER TABLE licensing_change_kinds FORCE ROW LEVEL SECURITY;

-- =========================================================================
-- 4. Helpers (SECURITY DEFINER, platform principal sessions only)
-- =========================================================================
-- The capability of a tenant NOW: o_configured = a row exists; o_enabled / o_cooling = the latest row. Platform principal sessions only.
CREATE FUNCTION screening_loosening_state(p_tenant uuid, OUT o_configured boolean, OUT o_enabled boolean, OUT o_cooling bigint)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a      record;
    v_prev text;
    r      record;
BEGIN
    SELECT * INTO a FROM public.licensing_change_actor();
    IF a.scope <> 'platform' THEN
        RAISE EXCEPTION 'licensing change: only a platform session reads the screening loosening capability' USING ERRCODE = 'LC002';
    END IF;
    v_prev := NULLIF(pg_catalog.current_setting('app.tenant_id', true), '');
    PERFORM pg_catalog.set_config('app.tenant_id', p_tenant::text, true);
    SELECT s.enabled, s.cooling_seconds INTO r FROM public.aml_screening_loosening_settings s
     WHERE s.tenant_id = p_tenant ORDER BY s.created_at DESC, s.id DESC LIMIT 1;
    o_configured := FOUND;
    PERFORM pg_catalog.set_config('app.tenant_id', COALESCE(v_prev, ''), true);
    o_enabled := COALESCE(r.enabled, false);
    o_cooling := r.cooling_seconds;
END
$$;

-- Validates an aml_screening_policy_loosen payload against the database state. p_filed_at IS NULL: the request phase (every rule except
-- the cooling period); otherwise the execute phase (adds the cooling period measured from the filing time). Every refusal is LC050
-- with a closed HINT (aml.RefusalCodes), except a malformed payload (LC010).
CREATE FUNCTION screening_policy_loosen_assert(p jsonb, p_filed_at timestamptz)
    RETURNS void
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a          record;
    v_tenant   uuid;
    v_jur      uuid;
    v_lists    text[];
    v_resc     bigint;
    v_grace    bigint;
    v_off      bigint;
    v_prevten  text;
    v_conf     boolean;
    v_enabled  boolean;
    v_cool     bigint;
    v_cfg      boolean;
    v_has_prev boolean;
    v_prev     record;
    v_alert    bigint;
    v_gate     boolean;
BEGIN
    SELECT * INTO a FROM public.licensing_change_actor();
    IF a.scope <> 'platform' THEN
        RAISE EXCEPTION 'licensing change: only a platform session handles a screening loosening' USING ERRCODE = 'LC002';
    END IF;
    IF NOT public.licensing_change_keys_are(p, ARRAY['tenant_id', 'jurisdiction_id', 'lists', 'rescreen_interval_seconds', 'backfill_grace_seconds',
                                                      'effective_offset_seconds', 'legal_review_reference', 'evidence_reference'])
       OR NOT public.licensing_change_is_uuid(p -> 'tenant_id') OR NOT public.licensing_change_is_uuid(p -> 'jurisdiction_id')
       OR pg_catalog.jsonb_typeof(p -> 'lists') <> 'array'
       OR pg_catalog.jsonb_typeof(p -> 'rescreen_interval_seconds') <> 'number' OR (p ->> 'rescreen_interval_seconds') !~ '^[0-9]{1,10}$'
       OR pg_catalog.jsonb_typeof(p -> 'backfill_grace_seconds') <> 'number' OR (p ->> 'backfill_grace_seconds') !~ '^[0-9]{1,10}$'
       OR pg_catalog.jsonb_typeof(p -> 'effective_offset_seconds') <> 'number' OR (p ->> 'effective_offset_seconds') !~ '^[0-9]{1,10}$'
       OR NOT public.licensing_change_is_text(p -> 'legal_review_reference', 200) OR (p ->> 'legal_review_reference') <> pg_catalog.btrim(p ->> 'legal_review_reference')
       OR (p ->> 'legal_review_reference') ~ '[\x01-\x1F\x7F]'
       OR NOT public.licensing_change_is_text(p -> 'evidence_reference', 200) OR (p ->> 'evidence_reference') !~ '^[A-Za-z0-9][A-Za-z0-9._:/#-]{0,199}$' THEN
        RAISE EXCEPTION 'licensing change: invalid aml_screening_policy_loosen payload' USING ERRCODE = 'LC010';
    END IF;
    v_tenant := (p ->> 'tenant_id')::uuid;
    v_jur := (p ->> 'jurisdiction_id')::uuid;
    v_resc := (p ->> 'rescreen_interval_seconds')::bigint;
    v_grace := (p ->> 'backfill_grace_seconds')::bigint;
    v_off := (p ->> 'effective_offset_seconds')::bigint;
    SELECT ARRAY(SELECT pg_catalog.jsonb_array_elements_text(p -> 'lists')) INTO v_lists;
    -- Canonical form (so equal decisions hash equally): one or two distinct known lists in ascending order.
    IF v_lists NOT IN (ARRAY['pep']::text[], ARRAY['sanctions']::text[], ARRAY['pep', 'sanctions']::text[]) THEN
        RAISE EXCEPTION 'licensing change: lists must be [pep], [sanctions] or [pep, sanctions]' USING ERRCODE = 'LC010';
    END IF;
    IF v_resc > 31536000 OR v_grace > 31536000 OR v_resc < 1 THEN
        RAISE EXCEPTION 'licensing change: the re-screen interval and the backfill grace are 1..31536000 seconds' USING ERRCODE = 'LC010';
    END IF;
    IF v_off > 2592000 THEN
        RAISE EXCEPTION 'aml: a screening policy takes effect between now and 30 days ahead' USING ERRCODE = 'LC050', HINT = 'policy_effective_from_out_of_range';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.tenants t WHERE t.id = v_tenant) OR NOT EXISTS (SELECT 1 FROM public.jurisdictions j WHERE j.id = v_jur) THEN
        RAISE EXCEPTION 'licensing change: unknown tenant or jurisdiction' USING ERRCODE = 'LC010';
    END IF;
    -- The 0189 engineering floor (not a legal value): loosening never goes below it.
    IF v_resc < 3600 THEN
        RAISE EXCEPTION 'aml: the re-screen interval is below the safety floor of 3600 seconds' USING ERRCODE = 'LC050', HINT = 'rescreen_interval_below_floor';
    END IF;

    -- Reads of tenant-RLS tables: the tenant setting is bound to the subject tenant for the reads below and restored; refusals are
    -- decided after the restore.
    v_prevten := NULLIF(pg_catalog.current_setting('app.tenant_id', true), '');
    PERFORM pg_catalog.set_config('app.tenant_id', v_tenant::text, true);
    SELECT s.enabled, s.cooling_seconds INTO v_enabled, v_cool FROM public.aml_screening_loosening_settings s
     WHERE s.tenant_id = v_tenant ORDER BY s.created_at DESC, s.id DESC LIMIT 1;
    v_conf := FOUND;
    SELECT EXISTS (SELECT 1 FROM public.tenant_jurisdiction_configs c
                    WHERE c.tenant_id = v_tenant AND c.jurisdiction_id = v_jur
                      AND (c.effective_to IS NULL OR c.effective_to > pg_catalog.clock_timestamp())) INTO v_cfg;
    SELECT p2.lists, p2.rescreen_interval_seconds, p2.backfill_grace_seconds INTO v_prev FROM public.aml_screening_policies p2
     WHERE p2.tenant_id = v_tenant AND p2.licensing_jurisdiction_id = v_jur ORDER BY p2.created_at DESC, p2.id DESC LIMIT 1;
    v_has_prev := FOUND;
    SELECT tp.gate_alert_after_seconds INTO v_alert FROM public.aml_screening_trigger_policies tp
     WHERE tp.tenant_id = v_tenant AND tp.licensing_jurisdiction_id = v_jur AND tp.gate_withdrawal ORDER BY tp.created_at DESC, tp.id DESC LIMIT 1;
    SELECT tp.gate_withdrawal INTO v_gate FROM public.aml_screening_trigger_policies tp
     WHERE tp.tenant_id = v_tenant AND tp.licensing_jurisdiction_id = v_jur ORDER BY tp.created_at DESC, tp.id DESC LIMIT 1;
    PERFORM pg_catalog.set_config('app.tenant_id', COALESCE(v_prevten, ''), true);

    -- One literal HINT per refusal (the closed table aml.RefusalCodes is pinned against these literals).
    IF NOT v_conf THEN
        RAISE EXCEPTION 'aml: screening policy loosening is not configured for this tenant' USING ERRCODE = 'LC050', HINT = 'loosening_not_configured';
    END IF;
    IF NOT v_enabled THEN
        RAISE EXCEPTION 'aml: screening policy loosening is disabled for this tenant' USING ERRCODE = 'LC050', HINT = 'loosening_disabled';
    END IF;
    IF NOT v_cfg THEN
        RAISE EXCEPTION 'aml: the tenant is not configured to operate in this jurisdiction' USING ERRCODE = 'LC050', HINT = 'tenant_not_in_jurisdiction';
    END IF;
    IF NOT v_has_prev THEN
        RAISE EXCEPTION 'aml: there is no screening policy version to loosen (author the first version through the policy route)' USING ERRCODE = 'LC050', HINT = 'no_screening_policy_to_loosen';
    END IF;
    IF v_alert IS NOT NULL AND v_resc < 2 * v_alert THEN
        RAISE EXCEPTION 'aml: the re-screen interval is below twice the withdrawal gate alert threshold' USING ERRCODE = 'LC050', HINT = 'rescreen_interval_below_alert_threshold';
    END IF;
    -- The production guard of the withdrawal gate also covers this path: where the jurisdiction's latest trigger policy gates withdrawals
    -- and the database environment anchor is not development / staging / test (an unrecorded anchor is production), nothing is loosened.
    IF COALESCE(v_gate, false) AND NOT public.aml_screening_gate_environment_ok() THEN
        RAISE EXCEPTION 'aml: a withdrawal gate exists in a database whose environment anchor is production or unrecorded' USING ERRCODE = 'LC050', HINT = 'gate_environment_refused';
    END IF;
    IF v_lists @> v_prev.lists AND v_resc <= v_prev.rescreen_interval_seconds AND v_grace <= v_prev.backfill_grace_seconds THEN
        RAISE EXCEPTION 'aml: the version is not less strict than the latest one; use the screening policy route' USING ERRCODE = 'LC050', HINT = 'not_a_loosening';
    END IF;
    IF p_filed_at IS NOT NULL AND pg_catalog.clock_timestamp() < p_filed_at + pg_catalog.make_interval(secs => v_cool) THEN
        RAISE EXCEPTION 'aml: the cooling period of the loosening has not elapsed' USING ERRCODE = 'LC050', HINT = 'loosening_cooling_period';
    END IF;
END
$$;

-- =========================================================================
-- 5. The classifier: rename-and-wrap (the 0180 body is not copied)
-- =========================================================================
ALTER FUNCTION licensing_change_classify(text, jsonb) RENAME TO licensing_change_classify_base;

CREATE FUNCTION licensing_change_classify(p_kind text, p jsonb,
    OUT widening boolean, OUT subject_tenant uuid, OUT subject_licence uuid)
    LANGUAGE plpgsql VOLATILE
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_tenant uuid;
    v_en     boolean;
    v_cool   bigint;
    st       record;
BEGIN
    IF p_kind = 'aml_screening_policy_loosen' THEN
        PERFORM public.screening_policy_loosen_assert(p, NULL);
        widening := true;
        RETURN;
    END IF;
    IF p_kind = 'aml_screening_loosening_configure' THEN
        IF NOT public.licensing_change_keys_are(p, ARRAY['tenant_id', 'enabled', 'cooling_seconds', 'evidence_reference'])
           OR NOT public.licensing_change_is_uuid(p -> 'tenant_id')
           OR pg_catalog.jsonb_typeof(p -> 'enabled') <> 'boolean'
           OR NOT public.licensing_change_is_text(p -> 'evidence_reference', 200) OR (p ->> 'evidence_reference') !~ '^[A-Za-z0-9][A-Za-z0-9._:/#-]{0,199}$'
           OR ((p -> 'enabled') = 'true'::jsonb AND (pg_catalog.jsonb_typeof(p -> 'cooling_seconds') <> 'number' OR (p ->> 'cooling_seconds') !~ '^[0-9]{1,6}$'
                                                      OR (p ->> 'cooling_seconds')::bigint > 255600))
           OR ((p -> 'enabled') = 'false'::jsonb AND pg_catalog.jsonb_typeof(p -> 'cooling_seconds') <> 'null') THEN
            RAISE EXCEPTION 'licensing change: invalid aml_screening_loosening_configure payload' USING ERRCODE = 'LC010';
        END IF;
        v_tenant := (p ->> 'tenant_id')::uuid;
        IF NOT EXISTS (SELECT 1 FROM public.tenants t WHERE t.id = v_tenant) THEN
            RAISE EXCEPTION 'licensing change: unknown tenant' USING ERRCODE = 'LC010';
        END IF;
        -- Serialise changes of one tenant's capability, so "widening" is decided against the version a concurrent change leaves.
        PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('screening_loosening:' || v_tenant::text, 190));
        SELECT * INTO st FROM public.screening_loosening_state(v_tenant);
        v_en := (p -> 'enabled') = 'true'::jsonb;
        v_cool := CASE WHEN v_en THEN (p ->> 'cooling_seconds')::bigint END;
        IF v_en = st.o_enabled AND v_cool IS NOT DISTINCT FROM st.o_cooling THEN
            RAISE EXCEPTION 'licensing change: the loosening capability would be unchanged' USING ERRCODE = 'LC010';
        END IF;
        -- Enabling, or a SHORTER cooling period, makes loosening easier (widening). Disabling or a longer period is narrowing.
        widening := v_en AND (NOT st.o_enabled OR v_cool < st.o_cooling);
        RETURN;
    END IF;
    SELECT b.widening, b.subject_tenant, b.subject_licence INTO widening, subject_tenant, subject_licence
      FROM public.licensing_change_classify_base(p_kind, p) b;
END
$$;

-- =========================================================================
-- 6. The executors (called by the Go kind after the request is marked executed, in the same transaction)
-- =========================================================================
CREATE FUNCTION screening_loosening_settings_apply(p_request uuid) RETURNS jsonb
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a       record;
    r       public.licensing_change_requests%ROWTYPE;
    v_prev  text;
    v_old   record;
    v_id    uuid;
    v_en    boolean;
    v_cool  bigint;
    v_tenant uuid;
BEGIN
    SELECT * INTO a FROM public.licensing_change_actor();
    IF a.scope <> 'platform' OR a.role <> 'platform_admin' THEN
        RAISE EXCEPTION 'licensing change: only a platform_admin session executes' USING ERRCODE = 'LC030';
    END IF;
    SELECT * INTO r FROM public.licensing_change_requests q WHERE q.id = p_request;
    IF NOT FOUND OR r.kind <> 'aml_screening_loosening_configure' OR r.status <> 'executed' OR r.executed_txid IS DISTINCT FROM pg_catalog.txid_current()
       OR r.executed_by IS DISTINCT FROM a.actor THEN
        RAISE EXCEPTION 'licensing change: no executed aml_screening_loosening_configure request of this transaction and actor' USING ERRCODE = 'LC040';
    END IF;
    v_tenant := (r.payload ->> 'tenant_id')::uuid;
    v_en := (r.payload -> 'enabled') = 'true'::jsonb;
    v_cool := CASE WHEN v_en THEN (r.payload ->> 'cooling_seconds')::bigint END;
    v_prev := NULLIF(pg_catalog.current_setting('app.tenant_id', true), '');
    PERFORM pg_catalog.set_config('app.tenant_id', v_tenant::text, true);
    SELECT s.id, s.enabled, s.cooling_seconds INTO v_old FROM public.aml_screening_loosening_settings s
     WHERE s.tenant_id = v_tenant ORDER BY s.created_at DESC, s.id DESC LIMIT 1;
    INSERT INTO public.aml_screening_loosening_settings (tenant_id, enabled, cooling_seconds, evidence_reference, reason_code, change_request_id, created_by, created_by_person_id)
    VALUES (v_tenant, v_en, v_cool, r.payload ->> 'evidence_reference', r.reason_code, r.id, a.actor, a.person_id)
    RETURNING id INTO v_id;
    PERFORM pg_catalog.set_config('app.tenant_id', COALESCE(v_prev, ''), true);
    RETURN pg_catalog.jsonb_build_object('settings_id', v_id, 'tenant_id', v_tenant, 'enabled', v_en, 'cooling_seconds', v_cool,
        'previous_enabled', v_old.enabled, 'previous_cooling_seconds', v_old.cooling_seconds);
END
$$;

CREATE FUNCTION screening_policy_loosen_apply(p_request uuid) RETURNS jsonb
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a        record;
    r        public.licensing_change_requests%ROWTYPE;
    v_prev   text;
    v_old    record;
    v_id     uuid;
    v_tenant uuid;
    v_jur    uuid;
    v_from   timestamptz;
    v_lists  text[];
BEGIN
    SELECT * INTO a FROM public.licensing_change_actor();
    IF a.scope <> 'platform' OR a.role <> 'platform_admin' THEN
        RAISE EXCEPTION 'licensing change: only a platform_admin session executes' USING ERRCODE = 'LC030';
    END IF;
    SELECT * INTO r FROM public.licensing_change_requests q WHERE q.id = p_request;
    IF NOT FOUND OR r.kind <> 'aml_screening_policy_loosen' OR r.status <> 'executed' OR r.executed_txid IS DISTINCT FROM pg_catalog.txid_current()
       OR r.executed_by IS DISTINCT FROM a.actor THEN
        RAISE EXCEPTION 'licensing change: no executed aml_screening_policy_loosen request of this transaction and actor' USING ERRCODE = 'LC040';
    END IF;
    v_tenant := (r.payload ->> 'tenant_id')::uuid;
    v_jur := (r.payload ->> 'jurisdiction_id')::uuid;
    -- Serialise with every other author of this tenant and jurisdiction (the 0189 lock key), then re-validate against the state NOW,
    -- including the cooling period measured from the filing time.
    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended(v_tenant::text || v_jur::text, 0));
    PERFORM public.screening_policy_loosen_assert(r.payload, r.created_at);
    SELECT ARRAY(SELECT pg_catalog.jsonb_array_elements_text(r.payload -> 'lists')) INTO v_lists;
    v_from := pg_catalog.clock_timestamp() + pg_catalog.make_interval(secs => (r.payload ->> 'effective_offset_seconds')::bigint);
    v_prev := NULLIF(pg_catalog.current_setting('app.tenant_id', true), '');
    PERFORM pg_catalog.set_config('app.tenant_id', v_tenant::text, true);
    SELECT p2.id, p2.lists, p2.rescreen_interval_seconds, p2.backfill_grace_seconds, p2.effective_from INTO v_old FROM public.aml_screening_policies p2
     WHERE p2.tenant_id = v_tenant AND p2.licensing_jurisdiction_id = v_jur ORDER BY p2.created_at DESC, p2.id DESC LIMIT 1;
    INSERT INTO public.aml_screening_policies (tenant_id, licensing_jurisdiction_id, lists, rescreen_interval_seconds, backfill_grace_seconds,
                                               effective_from, legal_review_reference, reason_code, created_by, created_by_person_id, created_by_scope, change_request_id)
    VALUES (v_tenant, v_jur, v_lists, (r.payload ->> 'rescreen_interval_seconds')::bigint, (r.payload ->> 'backfill_grace_seconds')::bigint,
            v_from, r.payload ->> 'legal_review_reference', 'governed_loosening', a.actor, a.person_id, 'platform', r.id)
    RETURNING id INTO v_id;
    PERFORM pg_catalog.set_config('app.tenant_id', COALESCE(v_prev, ''), true);
    RETURN pg_catalog.jsonb_build_object('policy_id', v_id, 'tenant_id', v_tenant, 'jurisdiction_id', v_jur, 'effective_from', v_from,
        'previous_policy_id', v_old.id, 'previous_lists', v_old.lists, 'previous_rescreen_interval_seconds', v_old.rescreen_interval_seconds,
        'previous_backfill_grace_seconds', v_old.backfill_grace_seconds, 'previous_effective_from', v_old.effective_from,
        'lists', v_lists, 'rescreen_interval_seconds', (r.payload ->> 'rescreen_interval_seconds')::bigint,
        'backfill_grace_seconds', (r.payload ->> 'backfill_grace_seconds')::bigint);
END
$$;

-- =========================================================================
-- 7. Grants (mirrored by deploy/init-app-role.sql)
-- =========================================================================
DO $$
DECLARE
    f text;
BEGIN
    FOREACH f IN ARRAY ARRAY[
        'screening_loosening_state(uuid)', 'screening_policy_loosen_assert(jsonb, timestamptz)',
        'screening_loosening_settings_apply(uuid)', 'screening_policy_loosen_apply(uuid)'
    ] LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC', f);
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
            EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO igaming_runtime', f);
        END IF;
    END LOOP;
END
$$;

COMMENT ON FUNCTION screening_policy_loosen_apply(uuid) IS '0190 (c4): the executor of an aml_screening_policy_loosen licensing change. Writes the next policy version (scope platform) in the transaction that executed the request, after re-validating everything including the cooling period.';
COMMENT ON FUNCTION screening_loosening_settings_apply(uuid) IS '0190 (c4): the executor of an aml_screening_loosening_configure licensing change.';
COMMENT ON FUNCTION licensing_change_classify(text, jsonb) IS '0190 (c4): wraps licensing_change_classify_base (the 0180 body) and adds the two screening loosening kinds.';
