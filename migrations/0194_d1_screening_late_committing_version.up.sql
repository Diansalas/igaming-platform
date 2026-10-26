-- 0194 (d1 round 4, ledger-finance R3-L1 on 0193): a policy version that COMMITS LATE was ignored. Its created_at is fixed at INSERT; if its transaction stays
-- open while a worker claims (claimed_at later), reads the old policy, screens and finishes, the version's start (created_at) is before claimed_at, so the
-- lower bound of aml_screening_since excluded it; once it committed and the clear went stale a lengthening revived the clear. The snapshot of a worker
-- screening is the claim policy (claim_policy_id), so for requests aml_screening_since now ALSO counts every version in force that was created after the claim
-- policy in VERSION ORDER (created_at, id), not by clock: (v.started > lower bound) OR (v.created_at, v.id) > (claim policy created_at, id). It is only ever
-- stricter (an OR). The never-in-force exclusion rule (a later-created version that started at or before it) is unchanged. Officer results pass NULL (unchanged).
-- Gate (worker-clear and auto-reviewed re-flag routes) and scheduler use the identical call. The 4-argument aml_screening_since is replaced by a 5-argument one
-- (owner-only like its predecessor; nothing else calls it). GUARANTEED after 0194: a worker screening that is, or has been, older than an interval of ANY version
-- in force that was created after its policy snapshot (by version order) or started after its screening began never counts again; lengthening cannot revive it.
-- NOT guaranteed: that a version still uncommitted NOW is already counted (it cannot be seen until it commits; the clear is fresh until then, by design), nor that
-- the provider screened the recorded lists (PROVIDER DEPENDENT).
SET LOCAL lock_timeout = '5s';

CREATE FUNCTION aml_screening_since(p_tenant uuid, p_jur uuid, p_at timestamptz, p_cur_lists text[], p_after_policy uuid) RETURNS bigint
    LANGUAGE sql VOLATILE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT CASE WHEN COALESCE(pg_catalog.bool_or(NOT (v.lists @> COALESCE(p_cur_lists, ARRAY[]::text[]))), false) THEN 0
                ELSE pg_catalog.min(v.rescreen_interval_seconds) END
      FROM (SELECT sp.id, sp.lists, sp.rescreen_interval_seconds, sp.created_at, GREATEST(sp.effective_from, sp.created_at) AS started
              FROM public.aml_screening_policies sp
             WHERE sp.tenant_id = p_tenant AND sp.licensing_jurisdiction_id = p_jur) v
     WHERE (v.started > p_at
            OR (p_after_policy IS NOT NULL
                AND (v.created_at, v.id) > (SELECT cp.created_at, cp.id FROM public.aml_screening_policies cp WHERE cp.tenant_id = p_tenant AND cp.id = p_after_policy)))
       AND v.started <= pg_catalog.clock_timestamp()
       AND NOT EXISTS (SELECT 1 FROM public.aml_screening_policies w
                        WHERE w.tenant_id = p_tenant AND w.licensing_jurisdiction_id = p_jur AND w.created_at > v.created_at
                          AND GREATEST(w.effective_from, w.created_at) <= v.started)
$$;

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
               AND public.aml_screening_covers(LEAST(r.completed_at, r.screened_at), r.screened_interval_seconds, r.screened_lists, v_interval, v_lists, public.aml_screening_since(p_tenant, v_tp.jur, LEAST(r.claimed_at, r.completed_at, r.screened_at), v_lists, r.claim_policy_id))
            UNION ALL
            SELECT 1 FROM public.aml_screening_requests r
             WHERE r.tenant_id = p_tenant AND r.person_id = p_person AND r.status = 'needs_officer' AND r.reviewed_from_request_id IS NOT NULL
               AND r.completed_at >= v_first AND (v_last_match IS NULL OR r.completed_at > v_last_match)
               AND public.aml_screening_covers(LEAST(r.completed_at, r.screened_at), r.screened_interval_seconds, r.screened_lists, v_interval, v_lists, public.aml_screening_since(p_tenant, v_tp.jur, LEAST(r.claimed_at, r.completed_at, r.screened_at), v_lists, r.claim_policy_id))
            UNION ALL
            SELECT 1 FROM public.aml_screening_results x
             WHERE x.tenant_id = p_tenant AND x.person_id = p_person AND x.outcome = 'clear' AND NOT v_sanc
               AND x.recorded_at >= v_first AND (v_last_match IS NULL OR x.recorded_at > v_last_match)
               AND public.aml_screening_covers(LEAST(x.recorded_at, x.screened_at), x.screened_interval_seconds, x.lists, v_interval, v_lists, public.aml_screening_since(p_tenant, v_tp.jur, LEAST(x.recorded_at, x.screened_at), v_lists, NULL::uuid))
            UNION ALL
            SELECT 1 FROM public.aml_screening_results x
              JOIN public.aml_hold_lifts l ON l.tenant_id = x.tenant_id AND l.hold_id = x.hold_id
             WHERE x.tenant_id = p_tenant AND x.person_id = p_person AND x.outcome IN ('potential_match', 'confirmed')
               AND (NOT v_sanc OR x.hold_reason_class IN ('confirmed_sanctions', 'sanctions_potential'))
               AND l.lifted_at >= v_first AND (v_last_match IS NULL OR l.lifted_at > v_last_match)
               -- lic26 F-5: the rescreen interval runs from the SCREENING (recorded_at), not from the time the hold was lifted
               -- d1 D10: and only a screening that RECORDED its policy, its lists and its interval counts, and only while it still covers them
               AND public.aml_screening_covers(LEAST(x.recorded_at, x.screened_at), x.screened_interval_seconds, x.lists, v_interval, v_lists, public.aml_screening_since(p_tenant, v_tp.jur, LEAST(x.recorded_at, x.screened_at), v_lists, NULL::uuid))
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
                               AND public.aml_screening_covers(LEAST(r.completed_at, r.screened_at), r.screened_interval_seconds, r.screened_lists, v_sp.rescreen_interval_seconds::double precision, v_sp.lists, public.aml_screening_since(p_tenant, v_tp.jur, LEAST(r.claimed_at, r.completed_at, r.screened_at), v_sp.lists, r.claim_policy_id))
                            UNION ALL
                            SELECT 1 FROM public.aml_screening_results x
                             WHERE x.tenant_id = p_tenant AND x.person_id = pa.person_id AND x.outcome <> 'error' AND x.recorded_at >= v_first
                               AND public.aml_screening_covers(LEAST(x.recorded_at, x.screened_at), x.screened_interval_seconds, x.lists, v_sp.rescreen_interval_seconds::double precision, v_sp.lists, public.aml_screening_since(p_tenant, v_tp.jur, LEAST(x.recorded_at, x.screened_at), v_sp.lists, NULL::uuid))) AS covered
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

REVOKE ALL ON FUNCTION aml_screening_since(uuid, uuid, timestamptz, text[], uuid) FROM PUBLIC;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON FUNCTION aml_screening_since(uuid, uuid, timestamptz, text[], uuid) FROM igaming_runtime';
    END IF;
END
$$;
DROP FUNCTION aml_screening_since(uuid, uuid, timestamptz, text[]);

COMMENT ON FUNCTION aml_screening_since(uuid, uuid, timestamptz, text[], uuid) IS '0194 (d1): smallest re-screen interval (seconds) among the policy versions IN FORCE for the jurisdiction that started after p_at OR (when p_after_policy, the claim policy of a worker screening, is given) were created after it in version order; 0 if one of them lacked a list in p_cur_lists; NULL if none. A version never in force (a later-created version started at or before it) is ignored. Officer results pass p_at = LEAST(recorded_at, screened_at) and NULL.';
COMMENT ON FUNCTION aml_screening_covers(timestamptz, bigint, text[], double precision, text[], bigint) IS '0194 (d1, D10): pure predicate. Fail closed: no provenance, a list missing, a list dropped since, or age >= LEAST(recorded interval, current interval, since) is not covered. GUARANTEED (with aml_screening_since): a worker screening that has been older than the interval of any version in force that started after its screening began or was created after its policy snapshot is never covered again; lengthening cannot revive it. NOT guaranteed: a version that has not yet committed is not seen (the clear is fresh until it commits); that the provider screened the recorded lists (PROVIDER DEPENDENT).';
