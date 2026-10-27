-- d1 (security closure before merge). COMPLIANCE / SECURITY MIGRATION: FOR `security` (BLOCKING) and `identity-compliance` REVIEW.
--
-- Part 1 (D7): REMOVE the runtime role's EXECUTE on the unrouted break-glass setter aml_screening_policy_set (0154).
-- That function can LOOSEN the screening policy (remove a list, lengthen the interval or the grace) with one officer chain and no
-- four-eyes step; since 0189 the HTTP route uses the tighten-only aml_screening_policy_author and since 0190 loosening has a governed
-- two-Person path, so the setter is the last single-officer loosening path reachable with the runtime credential (LIC30 L-3, C4 D7).
-- After this migration the setter is an OWNER-ONLY function: it exists for the database owner (break-glass / fixtures, documented in
-- docs/runbooks/aml-screening-triggers.md) and is never granted to the runtime role or PUBLIC again. No new runtime path is added.
-- The function body is unchanged (an owner call still authorises the actor, the officer quorum and the signed proof as before).
SET LOCAL lock_timeout = '5s';

DO $$
BEGIN
    IF to_regprocedure('public.aml_screening_policy_set(uuid, uuid, text[], bigint, bigint, timestamptz, text, text)') IS NULL
       OR to_regprocedure('public.aml_screening_policy_author(uuid, uuid, text[], bigint, bigint, timestamptz, text, text)') IS NULL
       OR to_regclass('public.aml_screening_loosening_settings') IS NULL THEN
        RAISE EXCEPTION 'migration 0191 pre-flight: migrations 0154, 0189 and 0190 are not applied' USING ERRCODE = 'LC098';
    END IF;
END
$$;

REVOKE ALL ON FUNCTION aml_screening_policy_set(uuid, uuid, text[], bigint, bigint, timestamptz, text, text) FROM PUBLIC;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON FUNCTION aml_screening_policy_set(uuid, uuid, text[], bigint, bigint, timestamptz, text, text) FROM igaming_runtime';
    END IF;
END
$$;
COMMENT ON FUNCTION aml_screening_policy_set(uuid, uuid, text[], bigint, bigint, timestamptz, text, text) IS '0154; since 0191 (d1, D7) OWNER-ONLY break-glass: NOT executable by the runtime role or PUBLIC. It can loosen the screening policy with one officer chain; the routed author is aml_screening_policy_author (tighten-only) and loosening is the governed licensing change aml_screening_policy_loosen.';

-- =========================================================================
-- Part 2 (D4): LOOSENING of the screening TRIGGER policy is a governed, two-Person licensing change.
-- =========================================================================
-- Until now POST .../screening/trigger-policy (aml_screening_trigger_policy_set, 0181) let ONE officer chain (quorum, critical step-up, proof,
-- legal reference) remove trigger events, turn gate_withdrawal OFF, or lengthen gate_alert_after_seconds. None of those is a two-Person decision.
-- Defined from the schema of aml_screening_trigger_policies (not guessed), a version is LESS STRICT than a previous one iff
--   * events: a previous event is no longer in `events`                          (fewer automatic screenings);
--   * gate_withdrawal: previous true, new false                                  (a withdrawal gate is switched OFF);
--   * gate_alert_after_seconds: both gate, new threshold LONGER than the previous (the trapped-funds alert fires later);
--   * effective_from: earlier than the version in force                          (rewrites a period already governed; checked on the route only).
-- legal_review_reference and reason_code are provenance, not strictness. Enabling a gate (off -> on), shorter threshold, more events are STRICTER.
--
-- What this part does, and only this:
--   1. aml_screening_trigger_policy_author (new, runtime): the 0181 setter plus ONE rule: a version that is less strict than the latest version
--      OR the version in force is refused (AT003, HINT trigger_policy_loosening_refused). The routed Go method now calls it.
--   2. aml_screening_trigger_policy_set (0181): OWNER-ONLY from now on (the same break-glass shape as the screening policy setter, D7): the runtime
--      role and PUBLIC lose EXECUTE, so no runtime session can loosen the trigger policy except through the governed kind below.
--   3. The licensing change kind aml_screening_trigger_policy_loosen (the 0190 pattern): ALWAYS widening, two distinct platform_admin Persons,
--      requester never decides, approvals at most 24 h old, request lifetime 72 h, signed proofs on request / approve / execute, officer grants and the
--      owner-scope quorum for requester / counted approvers / executor, gated by the SAME inert per-tenant capability row as the screening policy
--      loosening (aml_screening_loosening_settings): no row = loosening_not_configured. It can NEVER enable a withdrawal gate
--      (trigger_gate_enable_refused; the table guard repeats it). Executes by writing the next trigger policy version in the transaction of the final
--      approval (screening_trigger_loosen_apply), admitted by the table guard only with an executed request of this transaction.
-- The production guard of the gate (aml_screening_gate_environment_ok, worker heartbeat) is not redefined; aml_screening_trigger_policy_set keeps its
-- body. aml_screening_trigger_policy_author repeats the same environment and heartbeat refusals for any version that sets gate_withdrawal.

ALTER TABLE aml_screening_trigger_policies ADD COLUMN change_request_id UUID NULL REFERENCES licensing_change_requests (id) ON DELETE RESTRICT;
ALTER TABLE aml_screening_trigger_policies DROP CONSTRAINT aml_screening_trigger_policies_created_by_scope_check;
ALTER TABLE aml_screening_trigger_policies ADD CONSTRAINT aml_screening_trigger_policies_created_by_scope_check CHECK (created_by_scope IN ('platform_acting', 'platform'));
ALTER TABLE aml_screening_trigger_policies ADD CONSTRAINT aml_screening_trigger_policies_scope_request CHECK ((created_by_scope = 'platform') = (change_request_id IS NOT NULL));
CREATE UNIQUE INDEX aml_screening_trigger_policies_change_request ON aml_screening_trigger_policies (change_request_id) WHERE change_request_id IS NOT NULL;

-- ---- the strictness comparison (one definition for the route function and the governed validator) --------------------------------------
CREATE FUNCTION screening_trigger_cmp(p_pevents text[], p_pgate boolean, p_palert bigint, p_events text[], p_gate boolean, p_alert bigint,
                                      OUT o_looser boolean, OUT o_stricter boolean)
    LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT (NOT (p_events @> p_pevents)) OR (p_pgate AND NOT p_gate) OR (p_pgate AND p_gate AND p_alert > p_palert),
           (NOT (p_pevents @> p_events)) OR (NOT p_pgate AND p_gate) OR (p_pgate AND p_gate AND p_alert < p_palert)
$$;

-- ---- the table guard: a platform-authored row needs an executed governed loosening of THIS transaction ------------------------------------
CREATE OR REPLACE FUNCTION aml_screening_trigger_policies_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor record;
    v_prev  text;
    v_ok    boolean;
    v_pgate boolean;
BEGIN
    IF NEW.created_by_scope = 'platform' THEN
        v_prev := NULLIF(pg_catalog.current_setting('app.tenant_id', true), '');
        IF v_prev IS NOT NULL AND NULLIF(pg_catalog.current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL THEN
            PERFORM pg_catalog.set_config('app.tenant_id', '', true);
        ELSE
            v_prev := NULL;
        END IF;
        SELECT EXISTS (
            SELECT 1 FROM public.licensing_change_requests r
             WHERE r.id = NEW.change_request_id AND r.kind = 'aml_screening_trigger_policy_loosen' AND r.status = 'executed'
               AND r.executed_txid = pg_catalog.txid_current() AND r.executed_by = NEW.created_by
               AND r.payload ->> 'tenant_id' = NEW.tenant_id::text AND r.payload ->> 'jurisdiction_id' = NEW.licensing_jurisdiction_id::text
               AND ARRAY(SELECT pg_catalog.jsonb_array_elements_text(r.payload -> 'events')) = NEW.events
               AND (r.payload -> 'gate_withdrawal') = pg_catalog.to_jsonb(NEW.gate_withdrawal)
               AND COALESCE(r.payload -> 'gate_alert_after_seconds', 'null'::jsonb) = COALESCE(pg_catalog.to_jsonb(NEW.gate_alert_after_seconds), 'null'::jsonb)
               AND r.payload ->> 'legal_review_reference' = NEW.legal_review_reference
               AND NEW.effective_from >= r.executed_at AND NEW.effective_from <= r.executed_at + interval '30 days 1 minute'
               AND NEW.reason_code = 'governed_loosening'
               AND EXISTS (SELECT 1 FROM public.staff_users s WHERE s.id = NEW.created_by AND s.person_id = NEW.created_by_person_id AND s.tenant_id IS NULL)
        ) INTO v_ok;
        IF v_prev IS NOT NULL THEN
            PERFORM pg_catalog.set_config('app.tenant_id', v_prev, true);
        END IF;
        IF NOT v_ok THEN
            RAISE EXCEPTION 'aml: a platform-authored trigger policy version needs an executed aml_screening_trigger_policy_loosen licensing change of this transaction' USING ERRCODE = 'LC040';
        END IF;
        -- Defence in depth: a loosening can never ENABLE a withdrawal gate (the previous latest version must already gate).
        SELECT p.gate_withdrawal INTO v_pgate FROM public.aml_screening_trigger_policies p
         WHERE p.tenant_id = NEW.tenant_id AND p.licensing_jurisdiction_id = NEW.licensing_jurisdiction_id ORDER BY p.created_at DESC, p.id DESC LIMIT 1;
        IF NEW.gate_withdrawal AND NOT COALESCE(v_pgate, false) THEN
            RAISE EXCEPTION 'aml: a governed loosening never enables a withdrawal gate' USING ERRCODE = 'LC050', HINT = 'trigger_gate_enable_refused';
        END IF;
        RETURN NEW;
    END IF;
    SELECT * INTO v_actor FROM public.financial_actor_session();
    IF NEW.created_by IS DISTINCT FROM v_actor.actor OR NEW.created_by_scope IS DISTINCT FROM v_actor.scope
       OR NEW.created_by_person_id IS DISTINCT FROM v_actor.person_id THEN
        RAISE EXCEPTION 'aml: the trigger policy author columns are not the proven session actor' USING ERRCODE = 'AT001';
    END IF;
    RETURN NEW;
END
$$;

-- ---- 1. the route's tighten-only author (the 0181 setter body + the strictness rule) ----------------------------------------------------
CREATE FUNCTION aml_screening_trigger_policy_author(
    p_tenant uuid, p_jurisdiction uuid, p_events text[], p_gate_withdrawal boolean, p_gate_alert_after_seconds bigint,
    p_effective_from timestamptz, p_legal_ref text, p_reason_code text
) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a        record;
    v_id     uuid;
    v_events text;
    v_cfg    boolean;
    v_tenant text;
    v_off    boolean;
    v_ev     text[];
    v_prev   record;
    v_has    boolean;
    v_force  record;
    v_hasf   boolean;
    v_loose  boolean;
    v_floor  timestamptz;
BEGIN
    IF p_events IS NULL OR p_gate_withdrawal IS NULL OR p_jurisdiction IS NULL OR p_effective_from IS NULL THEN
        RAISE EXCEPTION 'aml: a trigger policy needs its jurisdiction, events, gate flag and effective time' USING ERRCODE = 'AT001';
    END IF;
    SELECT COALESCE(string_agg(x, ',' ORDER BY x), '') INTO v_events FROM (SELECT DISTINCT unnest(p_events) AS x) u;
    -- The SAME proof operation and argument digest as the setter: the Go caller signs one proof shape for both.
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_screening_trigger_policy:set', 'new',
        public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_jurisdiction::text, v_events, p_gate_withdrawal::text, p_gate_alert_after_seconds::text,
            to_char(p_effective_from AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), p_legal_ref, p_reason_code)));
    IF a.o_scope <> 'platform_acting' THEN
        RAISE EXCEPTION 'aml: screening trigger configuration is authored by a platform officer' USING ERRCODE = 'AM002', HINT = 'platform_officer_required';
    END IF;
    PERFORM public.aml_assert_quorum(p_tenant, public.aml_owner_scope_of_tenant(p_tenant));
    IF p_effective_from < pg_catalog.clock_timestamp() - interval '2 minutes' OR p_effective_from > pg_catalog.clock_timestamp() + interval '30 days' THEN
        RAISE EXCEPTION 'aml: a trigger policy takes effect between now (2 minutes tolerance) and 30 days ahead' USING ERRCODE = 'AT004', HINT = 'effective_from_out_of_range';
    END IF;
    IF p_gate_withdrawal THEN
        IF NOT ('scheduled' = ANY (p_events)) THEN
            RAISE EXCEPTION 'aml: a withdrawal gate needs the scheduled re-screen event' USING ERRCODE = 'AT004', HINT = 'gate_needs_scheduled_event';
        END IF;
        IF p_gate_alert_after_seconds IS NULL THEN
            RAISE EXCEPTION 'aml: a withdrawal gate needs gate_alert_after_seconds' USING ERRCODE = 'AT004', HINT = 'gate_needs_alert_threshold';
        END IF;
        IF NOT public.screening_worker_available() THEN
            RAISE EXCEPTION 'aml: no screening worker is running in this environment, so a withdrawal gate cannot be enabled (it would trap funds)' USING ERRCODE = 'AT004', HINT = 'screening_worker_heartbeat_missing';
        END IF;
        IF NOT public.aml_screening_gate_environment_ok() THEN
            RAISE EXCEPTION 'aml: a withdrawal gate cannot be enabled in a database whose environment anchor is production or unrecorded' USING ERRCODE = 'AT004', HINT = 'gate_environment_refused';
        END IF;
    ELSIF p_gate_alert_after_seconds IS NOT NULL THEN
        RAISE EXCEPTION 'aml: gate_alert_after_seconds belongs to a gating policy only' USING ERRCODE = 'AT004', HINT = 'alert_threshold_without_gate';
    END IF;
    -- d1 D4: TIGHTEN ONLY. Serialise authors of the same tenant and jurisdiction (and the governed executor, same key), then refuse a version that
    -- is less strict than the latest version OR than the version in force (events removed, a gate switched off, a longer alert threshold, or an
    -- effective time earlier than the version in force). The first version of a jurisdiction has nothing to loosen.
    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('screening_trigger:' || p_tenant::text || p_jurisdiction::text, 191));
    v_ev := COALESCE(pg_catalog.string_to_array(NULLIF(v_events, ''), ','), ARRAY[]::text[]);
    SELECT p.events, p.gate_withdrawal, p.gate_alert_after_seconds, p.effective_from INTO v_prev FROM public.aml_screening_trigger_policies p
     WHERE p.tenant_id = p_tenant AND p.licensing_jurisdiction_id = p_jurisdiction ORDER BY p.created_at DESC, p.id DESC LIMIT 1;
    v_has := FOUND;
    IF v_has THEN
        SELECT p.events, p.gate_withdrawal, p.gate_alert_after_seconds, p.effective_from INTO v_force FROM public.aml_screening_trigger_policies p
         WHERE p.tenant_id = p_tenant AND p.licensing_jurisdiction_id = p_jurisdiction AND p.effective_from <= pg_catalog.clock_timestamp()
         ORDER BY p.created_at DESC, p.id DESC LIMIT 1;
        v_hasf := FOUND;
        SELECT c.o_looser INTO v_loose FROM public.screening_trigger_cmp(v_prev.events, v_prev.gate_withdrawal, v_prev.gate_alert_after_seconds, v_ev, p_gate_withdrawal, p_gate_alert_after_seconds) c;
        IF NOT v_loose AND v_hasf THEN
            SELECT c.o_looser INTO v_loose FROM public.screening_trigger_cmp(v_force.events, v_force.gate_withdrawal, v_force.gate_alert_after_seconds, v_ev, p_gate_withdrawal, p_gate_alert_after_seconds) c;
        END IF;
        v_floor := COALESCE(v_force.effective_from, v_prev.effective_from);
        IF v_loose OR p_effective_from < v_floor THEN
            RAISE EXCEPTION 'aml: the new trigger policy version is less strict than the latest one; this route only tightens, loosening is a governed licensing change'
                USING ERRCODE = 'AT003', HINT = 'trigger_policy_loosening_refused';
        END IF;
    END IF;
    -- N-1 (0181): an EMPTY non-gating version for a jurisdiction that already has a trigger policy needs no scope checks. After the rule above it can
    -- only be EQUAL to the latest and in-force versions (already empty and off), so it loosens nothing.
    v_off := NOT p_gate_withdrawal AND v_events = '' AND v_has;
    IF NOT v_off THEN
        IF NOT EXISTS (SELECT 1 FROM public.aml_screening_jurisdictions(p_tenant) j WHERE j = p_jurisdiction) THEN
            RAISE EXCEPTION 'aml: the trigger policy jurisdiction must be the jurisdiction of the tenant''s licence or of an approved operating basis' USING ERRCODE = 'AT004', HINT = 'jurisdiction_not_in_scope';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM public.aml_screening_policies sp WHERE sp.tenant_id = p_tenant AND sp.licensing_jurisdiction_id = p_jurisdiction
                        AND sp.effective_from <= p_effective_from) THEN
            RAISE EXCEPTION 'aml: set the screening policy for this jurisdiction, in effect at or before the trigger policy, first' USING ERRCODE = 'AT004', HINT = 'screening_policy_missing';
        END IF;
        v_tenant := pg_catalog.current_setting('app.tenant_id', true);
        PERFORM pg_catalog.set_config('app.tenant_id', p_tenant::text, true);
        SELECT EXISTS (SELECT 1 FROM public.tenant_jurisdiction_configs c
                        WHERE c.tenant_id = p_tenant AND c.jurisdiction_id = p_jurisdiction
                          AND (c.effective_to IS NULL OR c.effective_to > pg_catalog.clock_timestamp())) INTO v_cfg;
        PERFORM pg_catalog.set_config('app.tenant_id', COALESCE(v_tenant, ''), true);
        IF NOT v_cfg THEN
            RAISE EXCEPTION 'aml: the tenant is not configured to operate in this jurisdiction' USING ERRCODE = 'AT004', HINT = 'tenant_not_in_jurisdiction';
        END IF;
    END IF;
    INSERT INTO public.aml_screening_trigger_policies (tenant_id, licensing_jurisdiction_id, events, gate_withdrawal, gate_alert_after_seconds, effective_from,
                                                       legal_review_reference, reason_code, created_by, created_by_person_id, created_by_scope)
    VALUES (p_tenant, p_jurisdiction, v_ev, p_gate_withdrawal, p_gate_alert_after_seconds, p_effective_from, p_legal_ref, p_reason_code,
            a.o_actor, a.o_person_id, a.o_scope)
    RETURNING id INTO v_id;
    RETURN v_id;
END
$$;

-- ---- 2. the 0181 setter becomes OWNER-ONLY (its body is unchanged) ---------------------------------------------------------------------
REVOKE ALL ON FUNCTION aml_screening_trigger_policy_set(uuid, uuid, text[], boolean, bigint, timestamptz, text, text) FROM PUBLIC;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON FUNCTION aml_screening_trigger_policy_set(uuid, uuid, text[], boolean, bigint, timestamptz, text, text) FROM igaming_runtime';
    END IF;
END
$$;
COMMENT ON FUNCTION aml_screening_trigger_policy_set(uuid, uuid, text[], boolean, bigint, timestamptz, text, text) IS '0181; since 0191 (d1, D4) OWNER-ONLY break-glass: NOT executable by the runtime role or PUBLIC. It can loosen the trigger policy (fewer events, gate off, longer alert threshold) with one officer chain. The routed author is aml_screening_trigger_policy_author (tighten-only); loosening is the governed licensing change aml_screening_trigger_policy_loosen.';

-- ---- 3. the governed kind -------------------------------------------------------------------------------------------------------------
ALTER TABLE licensing_change_kinds NO FORCE ROW LEVEL SECURITY;
INSERT INTO licensing_change_kinds (kind, platform_only, widening_approvals, approver_roles, description) VALUES
    ('aml_screening_trigger_policy_loosen', true, 2, ARRAY['platform_admin'], 'Author a LESS strict screening TRIGGER policy version (an event removed, a withdrawal gate switched off, a longer gate alert threshold) for a tenant and jurisdiction (always widening, refused unless the tenant loosening capability is configured, enabled and its cooling period has elapsed; never enables a gate).');
ALTER TABLE licensing_change_kinds FORCE ROW LEVEL SECURITY;

-- Validates an aml_screening_trigger_policy_loosen payload against the database state (p_filed_at NULL: request phase; otherwise execute phase,
-- which adds the cooling period counted from filing). Every refusal is LC050 with a closed HINT, except a malformed payload (LC010).
CREATE FUNCTION screening_trigger_loosen_assert(p jsonb, p_filed_at timestamptz)
    RETURNS void
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a          record;
    v_tenant   uuid;
    v_jur      uuid;
    v_events   text[];
    v_gate     boolean;
    v_alert    bigint;
    v_off      bigint;
    v_prevten  text;
    v_conf     boolean;
    v_enabled  boolean;
    v_cool     bigint;
    v_has_prev boolean;
    v_prev     record;
    v_resc_min bigint;
    v_looser   boolean;
    v_stricter boolean;
BEGIN
    SELECT * INTO a FROM public.licensing_change_actor();
    IF a.scope <> 'platform' THEN
        RAISE EXCEPTION 'licensing change: only a platform session handles a screening trigger loosening' USING ERRCODE = 'LC002';
    END IF;
    IF NOT public.licensing_change_keys_are(p, ARRAY['tenant_id', 'jurisdiction_id', 'events', 'gate_withdrawal', 'gate_alert_after_seconds',
                                                      'effective_offset_seconds', 'legal_review_reference', 'evidence_reference'])
       OR NOT public.licensing_change_is_uuid(p -> 'tenant_id') OR NOT public.licensing_change_is_uuid(p -> 'jurisdiction_id')
       OR pg_catalog.jsonb_typeof(p -> 'events') <> 'array'
       OR pg_catalog.jsonb_typeof(p -> 'gate_withdrawal') <> 'boolean'
       OR ((p -> 'gate_withdrawal') = 'true'::jsonb AND (pg_catalog.jsonb_typeof(p -> 'gate_alert_after_seconds') <> 'number' OR (p ->> 'gate_alert_after_seconds') !~ '^[0-9]{1,7}$'))
       OR ((p -> 'gate_withdrawal') = 'false'::jsonb AND pg_catalog.jsonb_typeof(p -> 'gate_alert_after_seconds') <> 'null')
       OR pg_catalog.jsonb_typeof(p -> 'effective_offset_seconds') <> 'number' OR (p ->> 'effective_offset_seconds') !~ '^[0-9]{1,10}$'
       OR NOT public.licensing_change_is_text(p -> 'legal_review_reference', 200) OR (p ->> 'legal_review_reference') <> pg_catalog.btrim(p ->> 'legal_review_reference')
       OR (p ->> 'legal_review_reference') ~ '[\x01-\x1F\x7F]'
       OR NOT public.licensing_change_is_text(p -> 'evidence_reference', 200) OR (p ->> 'evidence_reference') !~ '^[A-Za-z0-9][A-Za-z0-9._:/#-]{0,199}$'
       OR EXISTS (SELECT 1 FROM pg_catalog.jsonb_array_elements(p -> 'events') e WHERE pg_catalog.jsonb_typeof(e) <> 'string') THEN
        RAISE EXCEPTION 'licensing change: invalid aml_screening_trigger_policy_loosen payload' USING ERRCODE = 'LC010';
    END IF;
    v_tenant := (p ->> 'tenant_id')::uuid;
    v_jur := (p ->> 'jurisdiction_id')::uuid;
    v_gate := (p -> 'gate_withdrawal') = 'true'::jsonb;
    v_alert := CASE WHEN v_gate THEN (p ->> 'gate_alert_after_seconds')::bigint END;
    v_off := (p ->> 'effective_offset_seconds')::bigint;
    SELECT ARRAY(SELECT pg_catalog.jsonb_array_elements_text(p -> 'events')) INTO v_events;
    -- Canonical form (so equal decisions hash equally): distinct known events in ascending order, possibly none.
    IF NOT (v_events <@ ARRAY['registration', 'kyc_approval', 'scheduled']::text[])
       OR v_events IS DISTINCT FROM ARRAY(SELECT DISTINCT x FROM pg_catalog.unnest(v_events) x ORDER BY x) THEN
        RAISE EXCEPTION 'licensing change: events must be distinct known events in ascending order' USING ERRCODE = 'LC010';
    END IF;
    IF v_gate AND (NOT ('scheduled' = ANY (v_events)) OR v_alert NOT BETWEEN 60 AND 2592000) THEN
        RAISE EXCEPTION 'licensing change: a gating version needs the scheduled event and an alert threshold of 60..2592000 seconds' USING ERRCODE = 'LC010';
    END IF;
    IF v_off > 2592000 THEN
        RAISE EXCEPTION 'aml: a trigger policy takes effect between now and 30 days ahead' USING ERRCODE = 'LC050', HINT = 'effective_from_out_of_range';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.tenants t WHERE t.id = v_tenant) OR NOT EXISTS (SELECT 1 FROM public.jurisdictions j WHERE j.id = v_jur) THEN
        RAISE EXCEPTION 'licensing change: unknown tenant or jurisdiction' USING ERRCODE = 'LC010';
    END IF;
    -- The capability lock first (the key of the 0190 configure classifier, the validator and the executors), then the capability is read.
    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('screening_loosening:' || v_tenant::text, 190));
    v_prevten := NULLIF(pg_catalog.current_setting('app.tenant_id', true), '');
    PERFORM pg_catalog.set_config('app.tenant_id', v_tenant::text, true);
    SELECT s.enabled, s.cooling_seconds INTO v_enabled, v_cool FROM public.aml_screening_loosening_settings s
     WHERE s.tenant_id = v_tenant ORDER BY s.created_at DESC, s.id DESC LIMIT 1;
    v_conf := FOUND;
    SELECT tp.events, tp.gate_withdrawal, tp.gate_alert_after_seconds INTO v_prev FROM public.aml_screening_trigger_policies tp
     WHERE tp.tenant_id = v_tenant AND tp.licensing_jurisdiction_id = v_jur ORDER BY tp.created_at DESC, tp.id DESC LIMIT 1;
    v_has_prev := FOUND;
    -- The smallest re-screen interval among the latest and the in-force screening policy versions (the alert-threshold floor below).
    SELECT min(x.rescreen_interval_seconds) INTO v_resc_min FROM (
        (SELECT sp.rescreen_interval_seconds FROM public.aml_screening_policies sp WHERE sp.tenant_id = v_tenant AND sp.licensing_jurisdiction_id = v_jur
          ORDER BY sp.created_at DESC, sp.id DESC LIMIT 1)
        UNION ALL
        (SELECT sp.rescreen_interval_seconds FROM public.aml_screening_policies sp WHERE sp.tenant_id = v_tenant AND sp.licensing_jurisdiction_id = v_jur
            AND sp.effective_from <= pg_catalog.clock_timestamp() ORDER BY sp.created_at DESC, sp.id DESC LIMIT 1)) x;
    PERFORM pg_catalog.set_config('app.tenant_id', COALESCE(v_prevten, ''), true);

    IF NOT v_conf THEN
        RAISE EXCEPTION 'aml: screening loosening is not configured for this tenant' USING ERRCODE = 'LC050', HINT = 'loosening_not_configured';
    END IF;
    IF NOT v_enabled THEN
        RAISE EXCEPTION 'aml: screening loosening is disabled for this tenant' USING ERRCODE = 'LC050', HINT = 'loosening_disabled';
    END IF;
    PERFORM public.screening_loosening_officers_assert(v_tenant, a.actor, a.person_id);
    IF NOT v_has_prev THEN
        RAISE EXCEPTION 'aml: there is no trigger policy version to loosen (author the first version through the trigger policy route)' USING ERRCODE = 'LC050', HINT = 'no_trigger_policy_to_loosen';
    END IF;
    -- A loosening can NEVER enable a withdrawal gate (the production guard of the gate is not redefined here and not reachable from here).
    IF v_gate AND NOT v_prev.gate_withdrawal THEN
        RAISE EXCEPTION 'aml: a governed loosening never enables a withdrawal gate' USING ERRCODE = 'LC050', HINT = 'trigger_gate_enable_refused';
    END IF;
    -- A version that keeps a gate (a longer alert threshold) writes gate_withdrawal = true: only where the environment anchor allows a gate.
    IF v_gate AND NOT public.aml_screening_gate_environment_ok() THEN
        RAISE EXCEPTION 'aml: a withdrawal gate cannot be written in a database whose environment anchor is production or unrecorded' USING ERRCODE = 'LC050', HINT = 'gate_environment_refused';
    END IF;
    SELECT c.o_looser, c.o_stricter INTO v_looser, v_stricter
      FROM public.screening_trigger_cmp(v_prev.events, v_prev.gate_withdrawal, v_prev.gate_alert_after_seconds, v_events, v_gate, v_alert) c;
    IF NOT v_looser THEN
        RAISE EXCEPTION 'aml: the version is not less strict than the latest one; use the trigger policy route' USING ERRCODE = 'LC050', HINT = 'not_a_loosening';
    END IF;
    IF v_stricter THEN
        RAISE EXCEPTION 'aml: the version tightens one dimension while loosening another' USING ERRCODE = 'LC050', HINT = 'loosening_mixes_tightening';
    END IF;
    -- A longer alert threshold must still leave the screening policy interval at least twice the threshold (0189: clears must not expire before
    -- the trapped-funds alert can fire).
    IF v_gate AND v_resc_min IS NOT NULL AND v_resc_min < 2 * v_alert THEN
        RAISE EXCEPTION 'aml: the re-screen interval is below twice the withdrawal gate alert threshold' USING ERRCODE = 'LC050', HINT = 'rescreen_interval_below_alert_threshold';
    END IF;
    IF p_filed_at IS NOT NULL AND pg_catalog.clock_timestamp() < p_filed_at + pg_catalog.make_interval(secs => v_cool) THEN
        RAISE EXCEPTION 'aml: the cooling period of the loosening has not elapsed' USING ERRCODE = 'LC050', HINT = 'loosening_cooling_period';
    END IF;
END
$$;

-- An approval of a widening request of the screening loosening kinds is recorded only from an officer (now three kinds).
CREATE OR REPLACE FUNCTION licensing_change_approvals_screening_officer() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    r public.licensing_change_requests%ROWTYPE;
BEGIN
    SELECT * INTO r FROM public.licensing_change_requests q WHERE q.id = NEW.request_id;
    IF FOUND AND r.kind IN ('aml_screening_policy_loosen', 'aml_screening_loosening_configure', 'aml_screening_trigger_policy_loosen') AND r.required_approvals > 0 THEN
        PERFORM public.screening_loosening_officers_assert((r.payload ->> 'tenant_id')::uuid, NEW.approver_id, NEW.approver_person_id);
    END IF;
    RETURN NEW;
END
$$;

-- The classifier wrapper of 0190 plus the third kind (the 0180 body stays untouched as licensing_change_classify_base).
CREATE OR REPLACE FUNCTION licensing_change_classify(p_kind text, p jsonb,
    OUT widening boolean, OUT subject_tenant uuid, OUT subject_licence uuid)
    LANGUAGE plpgsql VOLATILE
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_tenant uuid;
    v_en     boolean;
    v_cool   bigint;
    st       record;
    a        record;
BEGIN
    IF p_kind = 'aml_screening_policy_loosen' THEN
        PERFORM public.screening_policy_loosen_assert(p, NULL);
        widening := true;
        RETURN;
    END IF;
    IF p_kind = 'aml_screening_trigger_policy_loosen' THEN
        PERFORM public.screening_trigger_loosen_assert(p, NULL);
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
        -- c4 fix (security HIGH-1): enabling loosening, or shortening its cooling period, is an AML configuration change: the acting platform admin
        -- needs the in-force aml_officer grant and the owner-scope quorum must hold. Disabling or lengthening (narrowing) stays open to any
        -- platform_admin (the fail-closed direction).
        IF widening THEN
            SELECT * INTO a FROM public.licensing_change_actor();
            PERFORM public.screening_loosening_officers_assert(v_tenant, a.actor, a.person_id);
        END IF;
        RETURN;
    END IF;
    SELECT b.widening, b.subject_tenant, b.subject_licence INTO widening, subject_tenant, subject_licence
      FROM public.licensing_change_classify_base(p_kind, p) b;
END
$$;
COMMENT ON FUNCTION licensing_change_classify(text, jsonb) IS '0190 (c4) + 0191 (d1): wraps licensing_change_classify_base (the 0180 body) and adds the three screening loosening kinds.';

-- The executor (called by the Go kind after the request is marked executed, in the same transaction).
CREATE FUNCTION screening_trigger_loosen_apply(p_request uuid) RETURNS jsonb
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a        record;
    r        public.licensing_change_requests%ROWTYPE;
    v_prev   text;
    v_old    record;
    v_inf    record;
    v_id     uuid;
    v_tenant uuid;
    v_jur    uuid;
    v_from   timestamptz;
    v_events text[];
    v_gate   boolean;
    v_alert  bigint;
    v_cool   bigint;
    v_off    bigint;
BEGIN
    SELECT * INTO a FROM public.licensing_change_actor();
    IF a.scope <> 'platform' OR a.role <> 'platform_admin' THEN
        RAISE EXCEPTION 'licensing change: only a platform_admin session executes' USING ERRCODE = 'LC030';
    END IF;
    SELECT * INTO r FROM public.licensing_change_requests q WHERE q.id = p_request;
    IF NOT FOUND OR r.kind <> 'aml_screening_trigger_policy_loosen' OR r.status <> 'executed' OR r.executed_txid IS DISTINCT FROM pg_catalog.txid_current()
       OR r.executed_by IS DISTINCT FROM a.actor THEN
        RAISE EXCEPTION 'licensing change: no executed aml_screening_trigger_policy_loosen request of this transaction and actor' USING ERRCODE = 'LC040';
    END IF;
    v_tenant := (r.payload ->> 'tenant_id')::uuid;
    v_jur := (r.payload ->> 'jurisdiction_id')::uuid;
    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('screening_loosening:' || v_tenant::text, 190));
    PERFORM public.screening_loosening_approvers_assert(r.id, v_tenant, a.actor, a.person_id);
    -- Serialise with the routed author of this tenant and jurisdiction, then re-validate against the state NOW, including the cooling period.
    PERFORM pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('screening_trigger:' || v_tenant::text || v_jur::text, 191));
    PERFORM public.screening_trigger_loosen_assert(r.payload, r.created_at);
    SELECT ARRAY(SELECT pg_catalog.jsonb_array_elements_text(r.payload -> 'events')) INTO v_events;
    v_gate := (r.payload -> 'gate_withdrawal') = 'true'::jsonb;
    v_alert := CASE WHEN v_gate THEN (r.payload ->> 'gate_alert_after_seconds')::bigint END;
    -- The version takes effect at least the cooling period after the decision (the later of the payload offset and the cooling period), so a
    -- stricter counter-version can be authored in between.
    SELECT s.o_cooling INTO v_cool FROM public.screening_loosening_state(v_tenant) s;
    v_off := GREATEST((r.payload ->> 'effective_offset_seconds')::bigint, COALESCE(v_cool, 0));
    v_from := pg_catalog.clock_timestamp() + pg_catalog.make_interval(secs => v_off);
    v_prev := NULLIF(pg_catalog.current_setting('app.tenant_id', true), '');
    PERFORM pg_catalog.set_config('app.tenant_id', v_tenant::text, true);
    SELECT p2.id, p2.events, p2.gate_withdrawal, p2.gate_alert_after_seconds, p2.effective_from INTO v_old FROM public.aml_screening_trigger_policies p2
     WHERE p2.tenant_id = v_tenant AND p2.licensing_jurisdiction_id = v_jur ORDER BY p2.created_at DESC, p2.id DESC LIMIT 1;
    SELECT p2.id, p2.events, p2.gate_withdrawal, p2.gate_alert_after_seconds, p2.effective_from INTO v_inf FROM public.aml_screening_trigger_policies p2
     WHERE p2.tenant_id = v_tenant AND p2.licensing_jurisdiction_id = v_jur AND p2.effective_from <= pg_catalog.clock_timestamp()
     ORDER BY p2.created_at DESC, p2.id DESC LIMIT 1;
    INSERT INTO public.aml_screening_trigger_policies (tenant_id, licensing_jurisdiction_id, events, gate_withdrawal, gate_alert_after_seconds, effective_from,
                                                       legal_review_reference, reason_code, created_by, created_by_person_id, created_by_scope, change_request_id)
    VALUES (v_tenant, v_jur, v_events, v_gate, v_alert, v_from, r.payload ->> 'legal_review_reference', 'governed_loosening', a.actor, a.person_id, 'platform', r.id)
    RETURNING id INTO v_id;
    PERFORM pg_catalog.set_config('app.tenant_id', COALESCE(v_prev, ''), true);
    RETURN pg_catalog.jsonb_build_object('policy_id', v_id, 'tenant_id', v_tenant, 'jurisdiction_id', v_jur, 'effective_from', v_from,
        'previous_policy_id', v_old.id, 'previous_events', v_old.events, 'previous_gate_withdrawal', v_old.gate_withdrawal,
        'previous_gate_alert_after_seconds', v_old.gate_alert_after_seconds, 'previous_effective_from', v_old.effective_from,
        'in_force_policy_id', v_inf.id, 'in_force_events', v_inf.events, 'in_force_gate_withdrawal', v_inf.gate_withdrawal,
        'in_force_gate_alert_after_seconds', v_inf.gate_alert_after_seconds, 'in_force_effective_from', v_inf.effective_from,
        'events', v_events, 'gate_withdrawal', v_gate, 'gate_alert_after_seconds', v_alert);
END
$$;

-- The reconciliation of 0190 (LF N-2), extended to the third kind: executed requests against the rows they must have written.
CREATE OR REPLACE FUNCTION screening_loosening_reconcile(OUT o_problem text, OUT o_kind text, OUT o_request_id uuid, OUT o_tenant_id uuid)
    RETURNS SETOF record
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a        record;
    v_prev   text;
    v_reqs   jsonb;
    v_ten    uuid[];
    t        uuid;
    q        record;
    n        integer;
BEGIN
    SELECT * INTO a FROM public.licensing_change_actor();
    IF a.scope <> 'platform' OR a.role <> 'platform_admin' THEN
        RAISE EXCEPTION 'licensing change: reconciliation is for platform_admin sessions' USING ERRCODE = 'LC030';
    END IF;
    v_prev := NULLIF(pg_catalog.current_setting('app.tenant_id', true), '');
    SELECT COALESCE(pg_catalog.jsonb_agg(pg_catalog.jsonb_build_object('id', r.id, 'kind', r.kind, 'tenant', r.payload ->> 'tenant_id')), '[]'::jsonb) INTO v_reqs
      FROM public.licensing_change_requests r
     WHERE r.status = 'executed' AND r.kind IN ('aml_screening_policy_loosen', 'aml_screening_loosening_configure', 'aml_screening_trigger_policy_loosen');
    SELECT COALESCE(pg_catalog.array_agg(x.id), ARRAY[]::uuid[]) INTO v_ten FROM public.tenants x;
    FOREACH t IN ARRAY v_ten LOOP
        PERFORM pg_catalog.set_config('app.tenant_id', t::text, true);
        FOR q IN SELECT e ->> 'id' AS id, e ->> 'kind' AS kind FROM pg_catalog.jsonb_array_elements(v_reqs) e WHERE e ->> 'tenant' = t::text LOOP
            IF q.kind = 'aml_screening_policy_loosen' THEN
                SELECT count(*) INTO n FROM public.aml_screening_policies p WHERE p.change_request_id = q.id::uuid;
            ELSIF q.kind = 'aml_screening_trigger_policy_loosen' THEN
                SELECT count(*) INTO n FROM public.aml_screening_trigger_policies p WHERE p.change_request_id = q.id::uuid;
            ELSE
                SELECT count(*) INTO n FROM public.aml_screening_loosening_settings s WHERE s.change_request_id = q.id::uuid;
            END IF;
            IF n <> 1 THEN
                o_problem := 'executed_request_without_exactly_one_row'; o_kind := q.kind; o_request_id := q.id::uuid; o_tenant_id := t;
                RETURN NEXT;
            END IF;
        END LOOP;
        FOR q IN SELECT p.change_request_id AS id FROM public.aml_screening_policies p
                  WHERE p.created_by_scope = 'platform'
                    AND NOT EXISTS (SELECT 1 FROM pg_catalog.jsonb_array_elements(v_reqs) e WHERE e ->> 'id' = p.change_request_id::text AND e ->> 'kind' = 'aml_screening_policy_loosen')
        LOOP
            o_problem := 'policy_row_without_executed_request'; o_kind := 'aml_screening_policy_loosen'; o_request_id := q.id; o_tenant_id := t;
            RETURN NEXT;
        END LOOP;
        FOR q IN SELECT p.change_request_id AS id FROM public.aml_screening_trigger_policies p
                  WHERE p.created_by_scope = 'platform'
                    AND NOT EXISTS (SELECT 1 FROM pg_catalog.jsonb_array_elements(v_reqs) e WHERE e ->> 'id' = p.change_request_id::text AND e ->> 'kind' = 'aml_screening_trigger_policy_loosen')
        LOOP
            o_problem := 'trigger_policy_row_without_executed_request'; o_kind := 'aml_screening_trigger_policy_loosen'; o_request_id := q.id; o_tenant_id := t;
            RETURN NEXT;
        END LOOP;
        FOR q IN SELECT s.change_request_id AS id FROM public.aml_screening_loosening_settings s
                  WHERE NOT EXISTS (SELECT 1 FROM pg_catalog.jsonb_array_elements(v_reqs) e WHERE e ->> 'id' = s.change_request_id::text AND e ->> 'kind' = 'aml_screening_loosening_configure')
        LOOP
            o_problem := 'settings_row_without_executed_request'; o_kind := 'aml_screening_loosening_configure'; o_request_id := q.id; o_tenant_id := t;
            RETURN NEXT;
        END LOOP;
    END LOOP;
    PERFORM pg_catalog.set_config('app.tenant_id', COALESCE(v_prev, ''), true);
    RETURN;
END
$$;

-- ---- grants ---------------------------------------------------------------------------------------------------------------------------------
DO $$
DECLARE
    f text;
BEGIN
    FOREACH f IN ARRAY ARRAY['screening_trigger_cmp(text[], boolean, bigint, text[], boolean, bigint)'] LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC', f);
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
            EXECUTE format('REVOKE ALL ON FUNCTION %s FROM igaming_runtime', f);
        END IF;
    END LOOP;
    FOREACH f IN ARRAY ARRAY[
        'aml_screening_trigger_policy_author(uuid, uuid, text[], boolean, bigint, timestamptz, text, text)',
        'screening_trigger_loosen_assert(jsonb, timestamptz)', 'screening_trigger_loosen_apply(uuid)'
    ] LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC', f);
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
            EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO igaming_runtime', f);
        END IF;
    END LOOP;
END
$$;

COMMENT ON FUNCTION aml_screening_trigger_policy_author(uuid, uuid, text[], boolean, bigint, timestamptz, text, text) IS '0191 (d1, D4): the HTTP route''s trigger policy author: the 0181 setter plus a tighten-only rule (events removed, a gate switched off, a longer alert threshold or an earlier effective time are refused with trigger_policy_loosening_refused).';
COMMENT ON FUNCTION screening_trigger_loosen_apply(uuid) IS '0191 (d1, D4): the executor of an aml_screening_trigger_policy_loosen licensing change. Writes the next trigger policy version (scope platform) in the transaction that executed the request, after re-validating everything including the cooling period. Never enables a gate.';
