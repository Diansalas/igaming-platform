-- ADR 0116 SLICE S2 (foundation) + S7 (readiness reader), A7 operability. COMPLIANCE / SECURITY MIGRATION: FOR `security`
-- (BLOCKING), `identity-compliance`, `qa`, `code-reviewer` REVIEW BEFORE MERGE.
--
-- NUMBERING: 0154 (0152 / 0153 belong to other in-flight work; 0155 is intentionally unused). Depends on 0140-0142.
--
-- NO LEGAL VALUE, NO AML VENDOR AND NO SEED. Every screening policy value (lists, re-screen interval, grace period) is
-- configuration that a human compliance decision owns; this migration inserts no row. The screening provider is a MOCK
-- (internal/aml, Go side); nothing here talks to a vendor. Software capability only: nothing here is a regulatory approval.
--
-- What it adds, and only this:
--   0. Pre-flight (AM098).
--   1. aml_screening_policies and aml_screening_results: ENABLE + FORCE ROW LEVEL SECURITY, NO privilege and NO policy for
--      igaming_runtime, an OWNER-only policy bound to the session tenant, FKs ON DELETE RESTRICT (C9), append-only
--      (UPDATE / DELETE / TRUNCATE refused, AM005). The results table is the audit trail of every screening attempt
--      (including failed ones) and carries DATABASE CHECKs that make a match impossible to record as "no action" and an
--      error impossible to record as a clear.
--   2. aml_screening_policy_set (officer, platform_acting only, quorum-gated, proof-bound): one policy row per tenant and
--      licensing jurisdiction VERSION; the latest effective row wins. A legal review reference is required.
--   3. aml_screening_policy_effective: the proof-less, session-tenant-bound read of the effective policy (not confidential: the
--      Back Office shows it read-only to tenants, ADR 0116 s6). It discloses no screening result.
--   4. aml_screening_record (officer, proof-bound, idempotent per (tenant, key)): records a screening result and, in the SAME
--      transaction, places the hold the outcome requires (ADR 0116 s4.3: a sanctions match gates all value movement, a PEP
--      match or an unavailable result gates withdrawal and payout). SR7-2: the hold class is DERIVED from the outcome and the
--      list: reason_class confirmed_sanctions (lifted only by the proof-bound four-eyes approval) ONLY for a CONFIRMED outcome
--      naming the sanctions list; a potential match, a PEP-only match and an error are manual-class (a potential sanctions
--      match keeps the all_value_movement SCOPE). Recording a hold needs the officer quorum of the owner scope; the ADR 0116 s6
--      "automatic screening hold" exemption returns with the F1 system-actor path. SR7-7: replay compares a digest of every field.
--   5. aml_screening_list (officer, proof-bound read) of a person's results.
--   6. aml_readiness (S7): the session-tenant-bound readiness read used by internal/launchgov: owner scope, whether the officer
--      quorum is met (a boolean, never the count or the officers) and how many of the referenced jurisdictions have an
--      effective screening policy. It discloses no case, hold, person or match.
--
-- SQLSTATE class AM (see 0140): AM001 AM002 AM004 AM005 AM007 AM011 AM026 AM040 AM098 AM099.

SET LOCAL lock_timeout = '5s';

-- =========================================================================
-- 0. Pre-flight
-- =========================================================================
DO $$
BEGIN
    IF to_regclass('public.aml_cases') IS NULL OR to_regprocedure('public.aml_authorize(uuid, text, text, text)') IS NULL THEN
        RAISE EXCEPTION 'migration 0154 pre-flight: migrations 0140-0142 are not applied' USING ERRCODE = 'AM098';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_class WHERE relnamespace = 'public'::regnamespace
                AND relname IN ('aml_screening_policies', 'aml_screening_results')) THEN
        RAISE EXCEPTION 'migration 0154 pre-flight: an AML screening table already exists' USING ERRCODE = 'AM098';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_catalog.pg_proc WHERE pronamespace = 'public'::regnamespace
                AND proname IN ('aml_screening_policy_set', 'aml_screening_policy_effective', 'aml_screening_record',
                                'aml_screening_list', 'aml_readiness', 'aml_screening_policies_guard', 'aml_screening_results_guard')) THEN
        RAISE EXCEPTION 'migration 0154 pre-flight: an AML screening function already exists' USING ERRCODE = 'AM098';
    END IF;
END
$$;

-- =========================================================================
-- 1. Tables
-- =========================================================================
CREATE TABLE aml_screening_policies (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id                 UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    licensing_jurisdiction_id UUID NOT NULL REFERENCES jurisdictions (id) ON DELETE RESTRICT,
    -- Closed vocabulary; at least one list. No default: an unconfigured jurisdiction is a readiness signal, never "clear".
    lists                     TEXT[] NOT NULL CHECK (cardinality(lists) BETWEEN 1 AND 2 AND lists <@ ARRAY['sanctions', 'pep']::text[]),
    rescreen_interval_seconds BIGINT NOT NULL CHECK (rescreen_interval_seconds > 0),
    -- Existing persons are not held for a missing result during the grace period after effective_from (ADR 0116 s4.3 C1).
    backfill_grace_seconds    BIGINT NOT NULL CHECK (backfill_grace_seconds >= 0),
    effective_from            TIMESTAMPTZ NOT NULL,
    legal_review_reference    TEXT NOT NULL CHECK (octet_length(legal_review_reference) BETWEEN 1 AND 200
                                                   AND legal_review_reference !~ '[\x01-\x1F\x7F]'),
    reason_code               TEXT NOT NULL CHECK (reason_code ~ '^[a-z0-9_]{1,64}$'),
    created_by                UUID NOT NULL,
    created_by_person_id      UUID NOT NULL,
    created_by_scope          TEXT NOT NULL CHECK (created_by_scope = 'platform_acting'),
    created_at                TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (tenant_id, id)
);
CREATE INDEX aml_screening_policies_effective ON aml_screening_policies (tenant_id, licensing_jurisdiction_id, effective_from DESC, created_at DESC);

CREATE TABLE aml_screening_results (
    id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id              UUID NOT NULL REFERENCES tenants (id) ON DELETE RESTRICT,
    owner_scope            TEXT NOT NULL CHECK (owner_scope IN ('platform', 'tenant')),
    person_id              UUID NOT NULL REFERENCES persons (id) ON DELETE RESTRICT,
    idempotency_key        TEXT NOT NULL CHECK (idempotency_key ~ '^[A-Za-z0-9_.:-]{1,128}$'),
    trigger                TEXT NOT NULL CHECK (trigger IN ('manual', 'registration', 'kyc_approval', 'attribute_change', 'scheduled')),
    provider_id            TEXT NOT NULL CHECK (provider_id ~ '^[a-z0-9_.-]{1,64}$'),
    provider_reference     TEXT NULL CHECK (provider_reference ~ '^[A-Za-z0-9_.:-]{1,128}$'),
    -- C7: the per-tenant pseudonymous reference the vendor saw. Never persons.id / player_account_id.
    subject_ref            TEXT NOT NULL CHECK (subject_ref ~ '^[0-9a-f]{64}$'),
    lists                  TEXT[] NOT NULL CHECK (cardinality(lists) BETWEEN 1 AND 2 AND lists <@ ARRAY['sanctions', 'pep']::text[]),
    outcome                TEXT NOT NULL CHECK (outcome IN ('clear', 'potential_match', 'confirmed', 'error')),
    match_lists            TEXT[] NULL CHECK (match_lists IS NULL OR (cardinality(match_lists) BETWEEN 1 AND 2 AND match_lists <@ ARRAY['sanctions', 'pep']::text[])),
    error_code             TEXT NULL CHECK (error_code IN ('timeout', 'unavailable', 'invalid_response', 'rate_limited', 'unknown')),
    screened_at            TIMESTAMPTZ NOT NULL,
    decision               TEXT NOT NULL CHECK (decision IN ('none', 'hold_withdrawal_and_payout', 'hold_all_value_movement')),
    hold_id                UUID NULL,
    -- SR7-2 / SR7-8: the class of the hold this result placed, DERIVED strictly from the outcome and the list (never from the
    -- caller's say-so): `confirmed_sanctions` (the four-eyes-lift class) ONLY for a CONFIRMED outcome that names the sanctions
    -- list. A potential match, a PEP-only match and an error are `manual` (their SCOPE can still be all_value_movement for a
    -- potential sanctions match). The word "confirmed" in the class name therefore always means a confirmed outcome.
    hold_reason_class      TEXT NULL CHECK (hold_reason_class IN ('manual', 'confirmed_sanctions')),
    -- SR7-7: the digest of the full proof payload (every input field). An idempotent replay must present the same digest.
    payload_hash           TEXT NOT NULL CHECK (payload_hash ~ '^[0-9a-f]{64}$'),
    recorded_by            UUID NOT NULL,
    recorded_by_person_id  UUID NOT NULL,
    recorded_by_scope      TEXT NOT NULL CHECK (recorded_by_scope IN ('tenant', 'platform_acting')),
    recorded_at            TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (tenant_id, idempotency_key),
    UNIQUE (tenant_id, id),
    FOREIGN KEY (hold_id) REFERENCES aml_holds (id) ON DELETE RESTRICT,
    CHECK ((recorded_by_scope = 'tenant') = (owner_scope = 'tenant')),
    -- FAIL CLOSED, in the database: a clear carries no match, no error and no action; an error is never a clear and names its
    -- closed code; a match names the list(s) and can NEVER be recorded as "no action"; a sanctions match and a confirmed
    -- outcome gate ALL value movement; every hold decision names the hold that enforces it.
    CHECK (outcome <> 'clear' OR (decision = 'none' AND match_lists IS NULL AND error_code IS NULL AND hold_id IS NULL)),
    CHECK ((outcome = 'error') = (error_code IS NOT NULL)),
    CHECK (outcome <> 'error' OR match_lists IS NULL),
    CHECK (outcome NOT IN ('potential_match', 'confirmed') OR (match_lists IS NOT NULL AND error_code IS NULL AND decision <> 'none')),
    CHECK (outcome <> 'confirmed' OR decision = 'hold_all_value_movement'),
    CHECK (NOT ('sanctions' = ANY (match_lists)) OR decision = 'hold_all_value_movement'),
    CHECK ((decision = 'none') = (hold_id IS NULL)),
    CHECK ((decision = 'none') = (hold_reason_class IS NULL)),
    -- SR7-2: all-value-movement is the scope of a CONFIRMED outcome or of a sanctions-list match, and of nothing else (an error
    -- or a PEP-only match is never recorded with it); the four-eyes class is exactly "confirmed outcome naming sanctions".
    CHECK (decision <> 'hold_all_value_movement' OR outcome = 'confirmed' OR COALESCE('sanctions' = ANY (match_lists), false)),
    CHECK (hold_reason_class IS NULL
           OR ((hold_reason_class = 'confirmed_sanctions') = (outcome = 'confirmed' AND COALESCE('sanctions' = ANY (match_lists), false))))
);
CREATE INDEX aml_screening_results_person ON aml_screening_results (tenant_id, person_id, recorded_at DESC);

-- RLS: enabled and FORCED; no privilege and no policy for the runtime role; an owner-only policy bound to the session tenant.
DO $$
DECLARE
    t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['aml_screening_policies', 'aml_screening_results'] LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', t);
        EXECUTE format('REVOKE ALL ON %I FROM PUBLIC', t);
        EXECUTE format($f$CREATE POLICY aml_owner_session ON %I FOR ALL TO CURRENT_USER
                          USING (tenant_id = public.aml_session_tenant()) WITH CHECK (tenant_id = public.aml_session_tenant())$f$, t);
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
            EXECUTE format('REVOKE ALL ON %I FROM igaming_runtime', t);
        END IF;
        EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE OR DELETE ON %I FOR EACH ROW EXECUTE FUNCTION aml_deny_mutation()', t || '_append_only', t);
        EXECUTE format('CREATE TRIGGER %I BEFORE TRUNCATE ON %I FOR EACH STATEMENT EXECUTE FUNCTION aml_deny_mutation()', t || '_no_truncate', t);
    END LOOP;
END
$$;

-- =========================================================================
-- 2. Guards (the second, independent check behind the definer functions)
-- =========================================================================
CREATE FUNCTION aml_screening_policies_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor record;
BEGIN
    SELECT * INTO v_actor FROM public.financial_actor_session();
    IF NEW.created_by IS DISTINCT FROM v_actor.actor OR NEW.created_by_scope IS DISTINCT FROM v_actor.scope
       OR NEW.created_by_person_id IS DISTINCT FROM v_actor.person_id THEN
        RAISE EXCEPTION 'aml: the policy author columns are not the proven session actor' USING ERRCODE = 'AM001';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER aml_screening_policies_10_guard BEFORE INSERT ON aml_screening_policies FOR EACH ROW EXECUTE FUNCTION aml_screening_policies_guard();

CREATE FUNCTION aml_screening_results_guard() RETURNS trigger
    LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_actor record;
BEGIN
    IF NEW.owner_scope IS DISTINCT FROM public.aml_owner_scope_of_tenant(NEW.tenant_id) THEN
        RAISE EXCEPTION 'aml: the result owner scope must be the tenant''s licensing owner scope' USING ERRCODE = 'AM002';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM public.player_accounts pa WHERE pa.tenant_id = NEW.tenant_id AND pa.person_id = NEW.person_id) THEN
        RAISE EXCEPTION 'aml: the person has no account in this tenant' USING ERRCODE = 'AM007';
    END IF;
    IF NEW.hold_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM public.aml_holds h WHERE h.id = NEW.hold_id AND h.tenant_id = NEW.tenant_id AND h.person_id = NEW.person_id
           AND h.reason_class = NEW.hold_reason_class
           AND h.hold_scope = CASE NEW.decision WHEN 'hold_all_value_movement' THEN 'all_value_movement' ELSE 'withdrawal_and_payout' END) THEN
        RAISE EXCEPTION 'aml: the named hold is not a hold of this person with this class and scope' USING ERRCODE = 'AM007';
    END IF;
    SELECT * INTO v_actor FROM public.financial_actor_session();
    IF NEW.recorded_by IS DISTINCT FROM v_actor.actor OR NEW.recorded_by_scope IS DISTINCT FROM v_actor.scope
       OR NEW.recorded_by_person_id IS DISTINCT FROM v_actor.person_id THEN
        RAISE EXCEPTION 'aml: the recording actor columns are not the proven session actor' USING ERRCODE = 'AM001';
    END IF;
    RETURN NEW;
END
$$;
CREATE TRIGGER aml_screening_results_10_guard BEFORE INSERT ON aml_screening_results FOR EACH ROW EXECUTE FUNCTION aml_screening_results_guard();

-- =========================================================================
-- 3. The officer / reader API
-- =========================================================================

-- aml_screening_policy_set: a platform_acting officer (screening configuration is platform-authored, ADR 0116 s6) writes the
-- next VERSION of a tenant's policy for a licensing jurisdiction. Needs the officer quorum of the tenant's owner scope (a
-- policy cannot be ACTIVATED for a scope without one). Seconds, not intervals, so the proof payload is unambiguous.
CREATE FUNCTION aml_screening_policy_set(
    p_tenant uuid, p_jurisdiction uuid, p_lists text[], p_rescreen_seconds bigint, p_grace_seconds bigint,
    p_effective_from timestamptz, p_legal_ref text, p_reason_code text
) RETURNS uuid
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a        record;
    v_id     uuid;
    v_lists  text;
    v_tenant text;
    v_cfg    boolean;
BEGIN
    IF p_lists IS NULL OR cardinality(p_lists) NOT BETWEEN 1 AND 2 THEN
        RAISE EXCEPTION 'aml: a screening policy names one or two lists' USING ERRCODE = 'AM004';
    END IF;
    SELECT string_agg(x, ',' ORDER BY x) INTO v_lists FROM unnest(p_lists) AS x;
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_screening_policy:set', 'new',
        public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_jurisdiction::text, v_lists, p_rescreen_seconds::text, p_grace_seconds::text,
            to_char(p_effective_from AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), p_legal_ref, p_reason_code)));
    IF a.o_scope <> 'platform_acting' THEN
        RAISE EXCEPTION 'aml: screening configuration is authored by a platform officer' USING ERRCODE = 'AM002';
    END IF;
    PERFORM public.aml_assert_quorum(p_tenant, public.aml_owner_scope_of_tenant(p_tenant));
    -- SR7-3: the policy jurisdiction must be one the tenant is configured to operate in (an active tenant_jurisdiction_configs row);
    -- a policy for a jurisdiction the tenant does not operate in is refused, not stored. tenant_jurisdiction_configs is FORCE RLS
    -- bound to app.tenant_id (an acting session does not carry it), so the tenant setting is set and restored by hand around the
    -- ONE read (the pattern of aml_readiness); the read can only reach rows of p_tenant, which aml_authorize already proved the
    -- caller may act on.
    v_tenant := pg_catalog.current_setting('app.tenant_id', true);
    PERFORM pg_catalog.set_config('app.tenant_id', p_tenant::text, true);
    SELECT EXISTS (SELECT 1 FROM public.tenant_jurisdiction_configs c
                    WHERE c.tenant_id = p_tenant AND c.jurisdiction_id = p_jurisdiction
                      AND (c.effective_to IS NULL OR c.effective_to > pg_catalog.clock_timestamp())) INTO v_cfg;
    PERFORM pg_catalog.set_config('app.tenant_id', COALESCE(v_tenant, ''), true);
    IF NOT v_cfg THEN
        RAISE EXCEPTION 'aml: the tenant is not configured to operate in this jurisdiction' USING ERRCODE = 'AM004';
    END IF;
    INSERT INTO public.aml_screening_policies (tenant_id, licensing_jurisdiction_id, lists, rescreen_interval_seconds, backfill_grace_seconds,
                                               effective_from, legal_review_reference, reason_code, created_by, created_by_person_id, created_by_scope)
    VALUES (p_tenant, p_jurisdiction, p_lists, p_rescreen_seconds, p_grace_seconds, p_effective_from, p_legal_ref, p_reason_code,
            a.o_actor, a.o_person_id, a.o_scope)
    RETURNING id INTO v_id;
    RETURN v_id;
END
$$;

-- aml_screening_policy_effective: the policy in force NOW for (tenant, jurisdiction): the latest created row among those whose
-- effective_from has passed. No proof (a tenant session may read its own policy; the Back Office shows it read-only), but bound
-- to the session tenant (AM026). Zero rows means UNCONFIGURED, which callers must treat as not-ready, never as clear.
CREATE FUNCTION aml_screening_policy_effective(p_tenant uuid, p_jurisdiction uuid)
RETURNS TABLE (policy_id uuid, lists text[], rescreen_interval_seconds bigint, backfill_grace_seconds bigint, effective_from timestamptz)
    LANGUAGE plpgsql STABLE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF p_tenant IS NULL OR p_jurisdiction IS NULL THEN
        RAISE EXCEPTION 'aml: invalid policy read arguments' USING ERRCODE = 'AM004';
    END IF;
    IF p_tenant IS DISTINCT FROM public.aml_session_tenant() THEN
        RAISE EXCEPTION 'aml: the session tenant is not the requested tenant' USING ERRCODE = 'AM026';
    END IF;
    RETURN QUERY
        SELECT p.id, p.lists, p.rescreen_interval_seconds, p.backfill_grace_seconds, p.effective_from
          FROM public.aml_screening_policies p
         WHERE p.tenant_id = p_tenant AND p.licensing_jurisdiction_id = p_jurisdiction AND p.effective_from <= pg_catalog.clock_timestamp()
         ORDER BY p.created_at DESC, p.id DESC
         LIMIT 1;
END
$$;

-- aml_screening_record: see the header (4). The hold decision is validated twice (here and by the table CHECKs).
CREATE FUNCTION aml_screening_record(
    p_tenant uuid, p_person uuid, p_idempotency_key text, p_trigger text, p_provider_id text, p_provider_reference text,
    p_subject_ref text, p_lists text[], p_outcome text, p_match_lists text[], p_error_code text, p_screened_at timestamptz,
    p_decision text, p_hold_reason_code text
) RETURNS TABLE (o_result_id uuid, o_hold_id uuid, o_replayed boolean)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a        record;
    v_owner  text;
    v_lists  text;
    v_match  text;
    v_prior  record;
    v_hold   uuid;
    v_result uuid;
    v_hash   text;
    v_class  text;
BEGIN
    SELECT string_agg(x, ',' ORDER BY x) INTO v_lists FROM unnest(p_lists) AS x;
    SELECT string_agg(x, ',' ORDER BY x) INTO v_match FROM unnest(p_match_lists) AS x;
    v_hash := public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_person::text, p_idempotency_key, p_trigger, p_provider_id, p_provider_reference,
            p_subject_ref, v_lists, p_outcome, v_match, p_error_code,
            to_char(p_screened_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), p_decision, p_hold_reason_code));
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_screening:record', 'new', v_hash);
    v_owner := public.aml_owner_scope_of_tenant(p_tenant);
    IF public.aml_actor_owner_scope(a.o_scope) IS DISTINCT FROM v_owner THEN
        RAISE EXCEPTION 'aml: the actor does not own this tenant''s AML scope' USING ERRCODE = 'AM002';
    END IF;
    PERFORM public.aml_assert_not_subject(a.o_person_id, p_person);
    PERFORM public.aml_person_lock(p_tenant, p_person);
    -- Idempotent replay: the same key with the same content returns the original result and places nothing; the same key with
    -- different content is refused (a key never silently re-binds).
    -- SR7-7: the comparison is over the digest of EVERY input field, not a subset.
    SELECT r.id, r.hold_id, r.payload_hash INTO v_prior
      FROM public.aml_screening_results r WHERE r.tenant_id = p_tenant AND r.idempotency_key = p_idempotency_key;
    IF FOUND THEN
        IF v_prior.payload_hash <> v_hash THEN
            RAISE EXCEPTION 'aml: the idempotency key was used with different content' USING ERRCODE = 'AM004';
        END IF;
        RETURN QUERY SELECT v_prior.id, v_prior.hold_id, true;
        RETURN;
    END IF;
    IF p_decision <> 'none' THEN
        IF p_hold_reason_code IS NULL OR p_hold_reason_code !~ '^[a-z0-9_]{1,64}$' THEN
            RAISE EXCEPTION 'aml: a hold decision needs a reason code' USING ERRCODE = 'AM004';
        END IF;
        -- SR7-2: the scope and the class are DERIVED from the outcome and the list, never taken from the caller. All-value-movement
        -- belongs to a confirmed outcome or a sanctions-list match; the four-eyes class (confirmed_sanctions) to a CONFIRMED outcome
        -- that names the sanctions list only. An error, a PEP-only match and a potential match are manual-class.
        IF p_decision = 'hold_all_value_movement' AND NOT (p_outcome = 'confirmed' OR COALESCE('sanctions' = ANY (p_match_lists), false)) THEN
            RAISE EXCEPTION 'aml: all-value-movement is the decision of a confirmed outcome or a sanctions-list match only' USING ERRCODE = 'AM004';
        END IF;
        v_class := CASE WHEN p_outcome = 'confirmed' AND COALESCE('sanctions' = ANY (p_match_lists), false) THEN 'confirmed_sanctions' ELSE 'manual' END;
        -- SR7-2: a hold recorded through an officer needs the officer quorum of the owner scope (no single officer can place a
        -- hold, in particular none of the four-eyes class, by recording a result). The ADR 0116 s6 exemption for holds placed by
        -- the screening ORCHESTRATOR returns with the F1 system-actor path (a separate, non-officer principal); until then every
        -- record is an officer-proved record, and the orchestrator's record is one of them.
        PERFORM public.aml_assert_quorum(p_tenant, v_owner);
        INSERT INTO public.aml_holds (tenant_id, owner_scope, person_id, player_account_id, hold_scope, reason_class, reason_code,
                                      placed_by, placed_by_person_id, placed_by_scope)
        VALUES (p_tenant, v_owner, p_person, NULL,
                CASE p_decision WHEN 'hold_all_value_movement' THEN 'all_value_movement' ELSE 'withdrawal_and_payout' END,
                v_class,
                p_hold_reason_code, a.o_actor, a.o_person_id, a.o_scope)
        RETURNING id INTO v_hold;
    END IF;
    INSERT INTO public.aml_screening_results (tenant_id, owner_scope, person_id, idempotency_key, trigger, provider_id, provider_reference,
                                              subject_ref, lists, outcome, match_lists, error_code, screened_at, decision, hold_id,
                                              hold_reason_class, payload_hash, recorded_by, recorded_by_person_id, recorded_by_scope)
    VALUES (p_tenant, v_owner, p_person, p_idempotency_key, p_trigger, p_provider_id, p_provider_reference, p_subject_ref, p_lists,
            p_outcome, p_match_lists, p_error_code, p_screened_at, p_decision, v_hold, v_class, v_hash, a.o_actor, a.o_person_id, a.o_scope)
    RETURNING id INTO v_result;
    RETURN QUERY SELECT v_result, v_hold, false;
END
$$;

CREATE FUNCTION aml_screening_list(p_tenant uuid, p_person uuid)
RETURNS TABLE (o_result_id uuid, o_owner_scope text, o_trigger text, o_provider_id text, o_lists text[], o_outcome text, o_match_lists text[],
               o_error_code text, o_screened_at timestamptz, o_decision text, o_hold_id uuid, o_recorded_at timestamptz)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    a record;
BEGIN
    SELECT * INTO a FROM public.aml_authorize(p_tenant, 'aml_screening:read', p_person::text,
        public.k2_sha256_hex(public.k2_canonical(p_tenant::text, p_person::text)));
    PERFORM public.aml_assert_not_subject(a.o_person_id, p_person);
    RETURN QUERY
        SELECT r.id, r.owner_scope, r.trigger, r.provider_id, r.lists, r.outcome, r.match_lists, r.error_code, r.screened_at, r.decision,
               r.hold_id, r.recorded_at
          FROM public.aml_screening_results r
         WHERE r.tenant_id = p_tenant AND r.person_id = p_person AND public.aml_scope_compatible(a.o_scope, r.owner_scope)
         ORDER BY r.recorded_at, r.id;
END
$$;

-- aml_readiness: the launch readiness reader (S7). Session-tenant bound, proof-less, and deliberately coarse: a boolean for the
-- quorum (never the count, never the officers), a count of referenced jurisdictions with an effective policy. VOLATILE because
-- the platform half of the quorum count runs the pinned owner-side liveness helper (it restores the session settings it clears).
CREATE FUNCTION aml_readiness(p_tenant uuid, p_jurisdictions uuid[])
RETURNS TABLE (o_owner_scope text, o_quorum_met boolean, o_jurisdictions_total integer, o_jurisdictions_with_policy integer)
    LANGUAGE plpgsql VOLATILE SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_owner     text;
    v_total     integer;
    v_with      integer;
    v_principal text;
    v_quorum    boolean;
BEGIN
    IF p_tenant IS NULL THEN
        RAISE EXCEPTION 'aml: invalid readiness arguments' USING ERRCODE = 'AM004';
    END IF;
    IF p_tenant IS DISTINCT FROM public.aml_session_tenant() THEN
        RAISE EXCEPTION 'aml: the session tenant is not the requested tenant' USING ERRCODE = 'AM026';
    END IF;
    v_owner := public.aml_owner_scope_of_tenant(p_tenant);
    SELECT count(DISTINCT j) INTO v_total FROM unnest(COALESCE(p_jurisdictions, ARRAY[]::uuid[])) AS j;
    SELECT count(DISTINCT j) INTO v_with FROM unnest(COALESCE(p_jurisdictions, ARRAY[]::uuid[])) AS j
     WHERE EXISTS (SELECT 1 FROM public.aml_screening_policies p
                    WHERE p.tenant_id = p_tenant AND p.licensing_jurisdiction_id = j AND p.effective_from <= pg_catalog.clock_timestamp());
    -- The grant tables are FORCE RLS and their tenant-scope SELECT policies admit only a PRINCIPAL-bound tenant session; the
    -- launch readiness read is a plain tenant read (and is evaluated for a tenant by platform staff), so the quorum count is
    -- taken under a placeholder principal, set and restored by hand around the ONE count (the pattern of
    -- aml_platform_grantee_live; a function-level SET of this setting is not permitted to the migration role). The count can
    -- only ever reach rows of the session tenant (the policy keeps tenant_id = app.tenant_id), so a caller that could set
    -- this setting itself gains nothing it did not have under any forged tenant session (THREAT-MODEL-ARBITRARY-SQL-1).
    v_principal := pg_catalog.current_setting('app.principal_id', true);
    PERFORM pg_catalog.set_config('app.principal_id', COALESCE(NULLIF(v_principal, ''), '00000000-0000-0000-0000-000000000000'), true);
    v_quorum := public.aml_officer_quorum(p_tenant, v_owner) >= 2;
    PERFORM pg_catalog.set_config('app.principal_id', COALESCE(v_principal, ''), true);
    RETURN QUERY SELECT v_owner, v_quorum, v_total, v_with;
END
$$;

-- =========================================================================
-- 4. Grants (mirrored by deploy/init-app-role.sql)
-- =========================================================================
DO $$
DECLARE
    f text;
BEGIN
    FOREACH f IN ARRAY ARRAY['aml_screening_policies_guard()', 'aml_screening_results_guard()'] LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC', f);
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
            EXECUTE format('REVOKE ALL ON FUNCTION %s FROM igaming_runtime', f);
        END IF;
    END LOOP;
    FOREACH f IN ARRAY ARRAY[
        'aml_screening_policy_set(uuid, uuid, text[], bigint, bigint, timestamptz, text, text)',
        'aml_screening_policy_effective(uuid, uuid)',
        'aml_screening_record(uuid, uuid, text, text, text, text, text, text[], text, text[], text, timestamptz, text, text)',
        'aml_screening_list(uuid, uuid)', 'aml_readiness(uuid, uuid[])'
    ] LOOP
        EXECUTE format('REVOKE ALL ON FUNCTION %s FROM PUBLIC', f);
        IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
            EXECUTE format('GRANT EXECUTE ON FUNCTION %s TO igaming_runtime', f);
        END IF;
    END LOOP;
END
$$;

COMMENT ON TABLE aml_screening_results IS 'ADR 0116 s4: the append-only audit trail of every screening attempt. A match can never be recorded as no action; an error is never a clear.';
COMMENT ON TABLE aml_screening_policies IS 'ADR 0116 s4.2: per tenant and licensing jurisdiction screening policy VERSIONS. No default, no seed; legal values are configuration.';
