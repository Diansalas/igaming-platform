-- Down of 0193 (DEV/STAGING ONLY): restores exactly the 0192 gate and scheduler bodies and the 0192 comment of aml_screening_covers; the other comments
-- (which 0192 did not have) are removed. No data is touched.
SET LOCAL lock_timeout = '5s';

CREATE OR REPLACE FUNCTION aml_screening_gate_denies(p_tenant uuid, p_person uuid, p_operation text) RETURNS boolean
    LANGUAGE plpgsql STABLE
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_tp         record;
    v_sp         record;
    v_reg        timestamptz;
    v_interval   double precision;
    v_grace      double precision;
    v_first      timestamptz;
    v_last_match timestamptz;
    v_sanc       boolean;
    v_lists      text[];
BEGIN
    IF p_operation NOT IN ('withdrawal', 'payout') THEN
        RETURN false;
    END IF;
    -- L1 / S1: a match the system found and no officer has RESOLVED holds the person whatever the policy says now (a new version
    -- with the gate off, a licence rebind or a removed licence must not release it). A resolution is a NON-ERROR result of the same
    -- person under the request's key; an `error` never resolves.
    -- SN-2: "resolved" is aml_screening_request_resolved (recorded after the request, and a sanctions-class match only by a sanctions
    -- match); N-2: a re-flag of a match an officer already reviewed and cleared is resolved by that review.
    IF EXISTS (SELECT 1 FROM public.aml_screening_requests r
                WHERE r.tenant_id = p_tenant AND r.person_id = p_person AND r.status = 'needs_officer'
                  AND NOT public.aml_screening_request_resolved(r.id)) THEN
        RETURN true;
    END IF;
    SELECT min(pa.created_at) INTO v_reg FROM public.player_accounts pa WHERE pa.tenant_id = p_tenant AND pa.person_id = p_person;
    -- Every jurisdiction whose LATEST effective trigger policy gates withdrawal (the policy is the tenant's, not tied to the licence
    -- that exists today: an unreadable or removed licence cannot release a gate, L2).
    FOR v_tp IN
        SELECT l.jur FROM (SELECT DISTINCT ON (tp.licensing_jurisdiction_id) tp.licensing_jurisdiction_id AS jur, tp.gate_withdrawal
                             FROM public.aml_screening_trigger_policies tp
                            WHERE tp.tenant_id = p_tenant AND tp.effective_from <= pg_catalog.clock_timestamp()
                            ORDER BY tp.licensing_jurisdiction_id, tp.created_at DESC, tp.id DESC) l
         WHERE l.gate_withdrawal
    LOOP
        -- H-1 (d) / L3: the baseline is the FIRST gating version of this jurisdiction, so a new version never resets the grace and
        -- never discards clears.
        SELECT min(tp.effective_from) INTO v_first FROM public.aml_screening_trigger_policies tp
         WHERE tp.tenant_id = p_tenant AND tp.licensing_jurisdiction_id = v_tp.jur AND tp.gate_withdrawal;
        SELECT sp.rescreen_interval_seconds, sp.backfill_grace_seconds, sp.lists INTO v_sp FROM public.aml_screening_policies sp
         WHERE sp.tenant_id = p_tenant AND sp.licensing_jurisdiction_id = v_tp.jur AND sp.effective_from <= pg_catalog.clock_timestamp()
         ORDER BY sp.created_at DESC, sp.id DESC LIMIT 1;
        IF FOUND THEN
            v_interval := v_sp.rescreen_interval_seconds::double precision;
            v_grace := v_sp.backfill_grace_seconds::double precision;
            v_lists := v_sp.lists;
        ELSE
            v_interval := NULL;
            v_grace := 0;
            v_lists := NULL;
        END IF;
        IF v_reg IS NULL THEN
            RETURN true;  -- an unknown person is never "screened"
        END IF;
        -- An existing person is not held for a missing result during the backfill grace after the gate FIRST took effect (C1).
        IF v_reg < v_first AND pg_catalog.clock_timestamp() < v_first + pg_catalog.make_interval(secs => v_grace) THEN
            CONTINUE;
        END IF;
        -- The latest match the system found that no earlier review covers (an auto-reviewed re-flag is not a new finding).
        SELECT max(r.completed_at) INTO v_last_match FROM public.aml_screening_requests r
         WHERE r.tenant_id = p_tenant AND r.person_id = p_person AND r.status = 'needs_officer' AND r.reviewed_from_request_id IS NULL;
        -- lic26 NEW-1: is the latest unreviewed match SANCTIONS-class? Then an officer's clear (no four-eyes) and a lift of a manual-class
        -- hold do not count: only a worker clear, an auto-reviewed re-flag, or the four-eyes lift of a sanctions hold.
        SELECT COALESCE(bool_or(r.outcome = 'confirmed' OR COALESCE('sanctions' = ANY (r.match_lists), false)), false) INTO v_sanc
          FROM public.aml_screening_requests r
         WHERE r.tenant_id = p_tenant AND r.person_id = p_person AND r.status = 'needs_officer' AND r.reviewed_from_request_id IS NULL
           AND r.completed_at = v_last_match;
        -- Only a FRESH screening that cleared the person, NEWER than the latest match, satisfies the policy: a completed clear, an
        -- auto-reviewed re-flag (N-2), an officer's clear, or an officer-recorded match whose hold was LIFTED (a sanctions hold only by
        -- the proof-bound four-eyes approval) from the lift time. A pending, retrying, failed, error or stale result does not.
        -- Officer results are timed by the database clock (recorded_at / lifted_at), never by the officer's screened_at.
        IF NOT EXISTS (
            SELECT 1 FROM public.aml_screening_requests r
             WHERE r.tenant_id = p_tenant AND r.person_id = p_person AND r.status = 'done' AND r.outcome = 'clear'
               AND r.completed_at >= v_first AND (v_last_match IS NULL OR r.completed_at > v_last_match)
               AND public.aml_screening_covers(LEAST(r.completed_at, r.screened_at), r.screened_interval_seconds, r.screened_lists, v_interval, v_lists, public.aml_screening_since(p_tenant, v_tp.jur, LEAST(r.completed_at, r.screened_at), v_lists))
            UNION ALL
            SELECT 1 FROM public.aml_screening_requests r
             WHERE r.tenant_id = p_tenant AND r.person_id = p_person AND r.status = 'needs_officer' AND r.reviewed_from_request_id IS NOT NULL
               AND r.completed_at >= v_first AND (v_last_match IS NULL OR r.completed_at > v_last_match)
               AND public.aml_screening_covers(LEAST(r.completed_at, r.screened_at), r.screened_interval_seconds, r.screened_lists, v_interval, v_lists, public.aml_screening_since(p_tenant, v_tp.jur, LEAST(r.completed_at, r.screened_at), v_lists))
            UNION ALL
            SELECT 1 FROM public.aml_screening_results x
             WHERE x.tenant_id = p_tenant AND x.person_id = p_person AND x.outcome = 'clear' AND NOT v_sanc
               AND x.recorded_at >= v_first AND (v_last_match IS NULL OR x.recorded_at > v_last_match)
               AND public.aml_screening_covers(LEAST(x.recorded_at, x.screened_at), x.screened_interval_seconds, x.lists, v_interval, v_lists, public.aml_screening_since(p_tenant, v_tp.jur, LEAST(x.recorded_at, x.screened_at), v_lists))
            UNION ALL
            SELECT 1 FROM public.aml_screening_results x
              JOIN public.aml_hold_lifts l ON l.tenant_id = x.tenant_id AND l.hold_id = x.hold_id
             WHERE x.tenant_id = p_tenant AND x.person_id = p_person AND x.outcome IN ('potential_match', 'confirmed')
               AND (NOT v_sanc OR x.hold_reason_class IN ('confirmed_sanctions', 'sanctions_potential'))
               AND l.lifted_at >= v_first AND (v_last_match IS NULL OR l.lifted_at > v_last_match)
               -- lic26 F-5: the rescreen interval runs from the SCREENING (recorded_at), not from the time the hold was lifted
               -- d1 D10: and only a screening that RECORDED its policy, its lists and its interval counts, and only while it still covers them
               AND public.aml_screening_covers(LEAST(x.recorded_at, x.screened_at), x.screened_interval_seconds, x.lists, v_interval, v_lists, public.aml_screening_since(p_tenant, v_tp.jur, LEAST(x.recorded_at, x.screened_at), v_lists))
        ) THEN
            RETURN true;
        END IF;
    END LOOP;
    RETURN false;
END
$$;

CREATE OR REPLACE FUNCTION aml_screening_enqueue_due(p_tenant uuid, p_limit integer) RETURNS integer
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_tp       record;
    v_sp       record;
    v_first    timestamptz;
    v_bucket   bigint;
    v_n        integer;
    v_total    integer := 0;
BEGIN
    IF p_tenant IS NULL OR p_limit IS NULL OR p_limit < 1 OR p_limit > 1000 THEN
        RAISE EXCEPTION 'aml: invalid scheduled enqueue arguments' USING ERRCODE = 'AT001';
    END IF;
    IF p_tenant IS DISTINCT FROM public.aml_session_tenant() THEN
        RAISE EXCEPTION 'aml: the session tenant is not the requested tenant' USING ERRCODE = 'AT002';
    END IF;
    FOR v_tp IN
        SELECT l.licensing_jurisdiction_id AS jur
          FROM (SELECT DISTINCT ON (tp.licensing_jurisdiction_id) tp.licensing_jurisdiction_id, tp.events
                  FROM public.aml_screening_trigger_policies tp
                 WHERE tp.tenant_id = p_tenant AND tp.effective_from <= pg_catalog.clock_timestamp()
                 ORDER BY tp.licensing_jurisdiction_id, tp.created_at DESC, tp.id DESC) l
         WHERE 'scheduled' = ANY (l.events)
    LOOP
        SELECT sp.id, sp.rescreen_interval_seconds, sp.lists INTO v_sp FROM public.aml_screening_policies sp
         WHERE sp.tenant_id = p_tenant AND sp.licensing_jurisdiction_id = v_tp.jur AND sp.effective_from <= pg_catalog.clock_timestamp()
         ORDER BY sp.created_at DESC, sp.id DESC LIMIT 1;
        IF NOT FOUND THEN
            CONTINUE;  -- no re-screen interval: nothing is due for this jurisdiction
        END IF;
        -- H-1 (d): clears count from the FIRST version of this jurisdiction's policy, so a new version never re-queues everyone.
        -- lic23 N-3: the SAME baseline the gate uses (the first GATING version), so a person cleared under an earlier non-gating version is
        -- re-enqueued exactly when the gate would hold them. A jurisdiction that never gated falls back to its first version.
        SELECT COALESCE(min(tp.effective_from) FILTER (WHERE tp.gate_withdrawal), min(tp.effective_from)) INTO v_first
          FROM public.aml_screening_trigger_policies tp
         WHERE tp.tenant_id = p_tenant AND tp.licensing_jurisdiction_id = v_tp.jur;
        v_bucket := pg_catalog.floor(pg_catalog.date_part('epoch', pg_catalog.clock_timestamp()) / v_sp.rescreen_interval_seconds)::bigint;
        -- d1 D10: a person is DUE unless a screening that recorded its policy, lists and interval still covers the lists and the interval in force
        -- (fresh under LEAST(recorded interval, current interval)). Legacy screenings with no recorded provenance never cover. The idempotency key
        -- names the policy version too, so a NEW policy version (a list added) can queue again inside the same bucket.
        INSERT INTO public.aml_screening_requests (tenant_id, person_id, licensing_jurisdiction_id, trigger, idempotency_key)
        SELECT p_tenant, c.person_id, v_tp.jur, 'scheduled', 'sched:' || c.person_id::text || ':' || v_bucket::text || ':' || v_sp.id::text
          FROM (
                SELECT pa.person_id,
                       (SELECT max(g.t) FROM (
                            SELECT r.completed_at AS t FROM public.aml_screening_requests r
                             WHERE r.tenant_id = p_tenant AND r.person_id = pa.person_id AND r.status IN ('done', 'needs_officer') AND r.completed_at >= v_first
                            UNION ALL
                            SELECT x.recorded_at FROM public.aml_screening_results x
                             WHERE x.tenant_id = p_tenant AND x.person_id = pa.person_id AND x.outcome <> 'error' AND x.recorded_at >= v_first) g) AS last_any,
                       EXISTS (
                            SELECT 1 FROM public.aml_screening_requests r
                             WHERE r.tenant_id = p_tenant AND r.person_id = pa.person_id AND r.status IN ('done', 'needs_officer') AND r.completed_at >= v_first
                               AND public.aml_screening_covers(LEAST(r.completed_at, r.screened_at), r.screened_interval_seconds, r.screened_lists, v_sp.rescreen_interval_seconds::double precision, v_sp.lists, public.aml_screening_since(p_tenant, v_tp.jur, LEAST(r.completed_at, r.screened_at), v_sp.lists))
                            UNION ALL
                            SELECT 1 FROM public.aml_screening_results x
                             WHERE x.tenant_id = p_tenant AND x.person_id = pa.person_id AND x.outcome <> 'error' AND x.recorded_at >= v_first
                               AND public.aml_screening_covers(LEAST(x.recorded_at, x.screened_at), x.screened_interval_seconds, x.lists, v_sp.rescreen_interval_seconds::double precision, v_sp.lists, public.aml_screening_since(p_tenant, v_tp.jur, LEAST(x.recorded_at, x.screened_at), v_sp.lists))) AS covered
                  FROM (SELECT DISTINCT a.person_id FROM public.player_accounts a WHERE a.tenant_id = p_tenant) pa
               ) c
         WHERE NOT c.covered
           AND NOT EXISTS (SELECT 1 FROM public.aml_screening_requests o WHERE o.tenant_id = p_tenant AND o.person_id = c.person_id AND o.status IN ('pending', 'claimed'))
           -- d1 SEC L-3: a key that already exists (this person's period under this policy version already ran, e.g. failed terminally) is excluded
           -- BEFORE the limit, so it cannot occupy the batch and starve the persons behind it.
           AND NOT EXISTS (SELECT 1 FROM public.aml_screening_requests k WHERE k.tenant_id = p_tenant
                            AND k.idempotency_key = 'sched:' || c.person_id::text || ':' || v_bucket::text || ':' || v_sp.id::text)
         ORDER BY c.last_any NULLS FIRST, c.person_id
         LIMIT (p_limit - v_total)
        ON CONFLICT (tenant_id, idempotency_key) DO NOTHING;
        GET DIAGNOSTICS v_n = ROW_COUNT;
        v_total := v_total + v_n;
        EXIT WHEN v_total >= p_limit;
    END LOOP;
    RETURN v_total;
END
$$;
COMMENT ON FUNCTION aml_screening_covers(timestamptz, bigint, text[], double precision, text[], bigint) IS '0192 (d1, D10): does a screening (time, RECORDED interval and lists) still cover the person under the interval and lists NOW in force? Fail closed: no provenance, a list missing, a list dropped since, or older than LEAST(recorded, current interval) is not covered.';

COMMENT ON FUNCTION aml_screening_since(uuid, uuid, timestamptz, text[]) IS NULL;
COMMENT ON FUNCTION aml_screening_gate_denies(uuid, uuid, text) IS NULL;
COMMENT ON FUNCTION aml_screening_enqueue_due(uuid, integer) IS NULL;
