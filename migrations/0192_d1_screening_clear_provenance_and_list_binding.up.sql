-- d1 D10 (security closure before merge): the screening FRESHNESS and LIST BINDING of a clear. COMPLIANCE / SECURITY MIGRATION: FOR `security`
-- (BLOCKING) and `ledger-finance` REVIEW (it changes when a clear expires, i.e. who is held at the withdrawal / payout gate).
--
-- THE DEFECT (c4 owner decision D10). aml_screening_gate_denies (0181) judged a clear "fresh" by comparing its age with the re-screen interval of the
-- policy version IN FORCE AT EVALUATION TIME, and never looked at WHICH LISTS the clear covered. So (1) lengthening the interval (a governed
-- loosening, or the break-glass setter) made an EXPIRED clear fresh again and released withdrawals that were waiting behind the gate, with no new
-- screening; and (2) a clear taken against `pep` only still cleared the person once `sanctions` was added to the policy.
--
-- THE FIX (only ever STRICTER; no release path is added, no hard restriction is touched):
--   1. Every screening result that can count as a clear records the policy version it was produced under, the lists screened and the re-screen
--      interval in force: aml_screening_requests (written by the existing definer finish function; the lists and the interval are READ FROM THE
--      POLICY ROW the caller names, never taken from the caller) and aml_screening_results (written by aml_screening_record; the lists are the
--      officer's p_lists, the policy and the interval are stamped by the database). Append-only is unchanged (terminal requests are final).
--   2. aml_screening_covers() + aml_screening_since(): a screening covers the person only if (a) its provenance is recorded, (b) its recorded lists
--      are a SUPERSET of the lists now in force, (c) no policy version that was ACTUALLY IN FORCE since the screening lacked a list now in force (a list
--      removed and re-added invalidates it), and (d) its age is below LEAST(recorded interval, interval now in force, the SMALLEST interval of any version
--      that came into force after the screening). Age runs from LEAST(completion/recording time on the database clock, the screening time stated by
--      the provider / officer). A version "came into force" at GREATEST(effective_from, created_at); a version never counts if a later-created version
--      took over at or before that moment (a pending version neutralised by a counter-version).
--      GUARANTEED: once a screening has been older than ANY interval that was in force at some moment since it was taken, it never counts again; lengthening
--      the interval (also after a shortening) can never revive it. NOT guaranteed: a screening that is still younger than every such interval is fresh
--      (obviously), and a version that was never in force (neutralised before it took effect) is ignored on purpose. A screening with NO recorded provenance
--      (every row written before this migration) is NOT trusted: the person is not covered and is DUE for re-screening.
--   3. aml_screening_gate_denies applies it to all four routes by which a screening satisfies the policy (a worker clear, an auto-reviewed
--      re-flag, an officer clear, an officer match whose hold was lifted). aml_screening_enqueue_due uses the IDENTICAL predicate, so persons whose
--      clear no longer covers them are enqueued (the idempotency key also names the policy version, so a tightened policy can queue again inside
--      the same re-screen bucket; a key that already exists is excluded BEFORE the batch limit, so a permanently failed period does not starve others).
--   4. aml_screening_request_finish takes the policy version the worker screened under (new last argument) and requires it to be the version in force
--      at CLAIM time (aml_screening_request_claim stamps claim_policy_id and claimed_at). A clear / match is refused when its screened_at is more than
--      120 s in the future or more than 120 s before the claim (engineering tolerances, not business values) and is NOT applied after its lease passed
--      (the request is reclaimed and retried). The 12-argument function is DROPPED: no path can create an unprovenanced clear. Rolling deploy: old
--      callers of the 12-argument finish get 42883; migrate and deploy the worker together.
-- Unchanged on purpose: unresolved matches, sanctions holds, officer holds, the kill switch, closed accounts, the backfill grace and the first-gating
-- baseline. A withdrawal already held for a stale clear stays held until the person is re-screened. NO sanctions false-positive policy and NO release
-- policy is defined here; no gate is enabled.
--
-- EFFECT ON EXISTING (DEV / STAGING) DATA, stated plainly: every clear that exists today has no provenance. After this migration none of them counts:
-- under a gating trigger policy every person who relied on one is denied at the gate (withdrawals wait) until the next scheduled enqueue pass queues
-- a re-screen and the worker records a clear WITH provenance. Nothing is deleted or rewritten. No production database has a gate (the gate cannot be
-- enabled where the environment anchor is production or unrecorded), so there is no production effect.
SET LOCAL lock_timeout = '5s';

DO $$
BEGIN
    IF to_regprocedure('public.aml_screening_request_finish(uuid, uuid, uuid, text, text[], text, text, text, text, timestamptz, integer, text)') IS NULL
       OR to_regprocedure('public.aml_screening_gate_denies(uuid, uuid, text)') IS NULL OR to_regclass('public.aml_screening_requests') IS NULL THEN
        RAISE EXCEPTION 'migration 0192 pre-flight: migration 0181 is not applied' USING ERRCODE = 'AT098';
    END IF;
    IF to_regprocedure('public.aml_screening_covers(timestamptz, bigint, text[], double precision, text[], bigint)') IS NOT NULL
       OR EXISTS (SELECT 1 FROM pg_catalog.pg_attribute WHERE attrelid = 'public.aml_screening_requests'::regclass AND attname = 'screened_policy_id' AND NOT attisdropped) THEN
        RAISE EXCEPTION 'migration 0192 pre-flight: the screening provenance already exists' USING ERRCODE = 'AT098';
    END IF;
END
$$;

-- =========================================================================
-- 1. Provenance columns (nullable: rows written before this migration have none and are never trusted)
-- =========================================================================
ALTER TABLE aml_screening_requests
    ADD COLUMN screened_policy_id        UUID NULL,
    ADD COLUMN screened_lists            TEXT[] NULL CHECK (screened_lists IS NULL OR (cardinality(screened_lists) BETWEEN 1 AND 2 AND screened_lists <@ ARRAY['sanctions', 'pep']::text[])),
    ADD COLUMN screened_interval_seconds BIGINT NULL CHECK (screened_interval_seconds IS NULL OR screened_interval_seconds > 0);
ALTER TABLE aml_screening_requests ADD CONSTRAINT aml_screening_requests_provenance_all_or_none
    CHECK ((screened_policy_id IS NULL) = (screened_lists IS NULL) AND (screened_lists IS NULL) = (screened_interval_seconds IS NULL));
ALTER TABLE aml_screening_requests ADD CONSTRAINT aml_screening_requests_screened_policy_fk
    FOREIGN KEY (tenant_id, screened_policy_id) REFERENCES aml_screening_policies (tenant_id, id) ON DELETE RESTRICT;
-- Every request that ENDS in done / needs_officer from now on carries its provenance. NOT VALID: the rows that already exist (which cannot be
-- updated again: terminal states are final) are not judged; every new row and every update is.
ALTER TABLE aml_screening_requests ADD CONSTRAINT aml_screening_requests_provenance_required
    CHECK (status NOT IN ('done', 'needs_officer') OR screened_policy_id IS NOT NULL) NOT VALID;

ALTER TABLE aml_screening_results
    ADD COLUMN screened_policy_id        UUID NULL,
    ADD COLUMN screened_interval_seconds BIGINT NULL CHECK (screened_interval_seconds IS NULL OR screened_interval_seconds > 0);
ALTER TABLE aml_screening_results ADD CONSTRAINT aml_screening_results_provenance_pair
    CHECK ((screened_policy_id IS NULL) = (screened_interval_seconds IS NULL));
ALTER TABLE aml_screening_results ADD CONSTRAINT aml_screening_results_screened_policy_fk
    FOREIGN KEY (tenant_id, screened_policy_id) REFERENCES aml_screening_policies (tenant_id, id) ON DELETE RESTRICT;
ALTER TABLE aml_screening_requests ADD COLUMN claimed_at TIMESTAMPTZ NULL, ADD COLUMN claim_policy_id UUID NULL;
ALTER TABLE aml_screening_requests ADD CONSTRAINT aml_screening_requests_claim_policy_fk
    FOREIGN KEY (tenant_id, claim_policy_id) REFERENCES aml_screening_policies (tenant_id, id) ON DELETE RESTRICT;
COMMENT ON COLUMN aml_screening_requests.claim_policy_id IS '0192 (d1, SEC L-1): the screening policy version in force when the request was (last) claimed; a clear must name it.';
COMMENT ON COLUMN aml_screening_requests.screened_policy_id IS '0192 (d1, D10): the screening policy version this clear / match was screened under (NULL = written before 0192: never trusted as a fresh clear).';
COMMENT ON COLUMN aml_screening_results.screened_policy_id IS '0192 (d1, D10): the screening policy version in force when the officer result was recorded (NULL = written before 0192 or none in force: never trusted as a fresh clear).';

-- =========================================================================
-- 2. The coverage predicate (owner-only; reached through the gate and the scheduler)
-- =========================================================================
-- The smallest re-screen interval (seconds) among the policy versions of the jurisdiction that CAME INTO FORCE after p_at (and not after now), or 0 when
-- one of them lacked a list in p_cur_lists (a list dropped since the screening). A version came into force at GREATEST(effective_from, created_at);
-- it is ignored when a later-created version took over at or before that moment (it never governed anything). NULL = no such version.
CREATE FUNCTION aml_screening_since(p_tenant uuid, p_jur uuid, p_at timestamptz, p_cur_lists text[]) RETURNS bigint
    LANGUAGE sql VOLATILE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT CASE WHEN COALESCE(pg_catalog.bool_or(NOT (v.lists @> COALESCE(p_cur_lists, ARRAY[]::text[]))), false) THEN 0
                ELSE pg_catalog.min(v.rescreen_interval_seconds) END
      FROM (SELECT sp.lists, sp.rescreen_interval_seconds, sp.created_at, GREATEST(sp.effective_from, sp.created_at) AS started
              FROM public.aml_screening_policies sp
             WHERE sp.tenant_id = p_tenant AND sp.licensing_jurisdiction_id = p_jur) v
     WHERE v.started > p_at AND v.started <= pg_catalog.clock_timestamp()
       AND NOT EXISTS (SELECT 1 FROM public.aml_screening_policies w
                        WHERE w.tenant_id = p_tenant AND w.licensing_jurisdiction_id = p_jur AND w.created_at > v.created_at
                          AND GREATEST(w.effective_from, w.created_at) <= v.started)
$$;

CREATE FUNCTION aml_screening_covers(p_at timestamptz, p_rec_interval bigint, p_rec_lists text[], p_cur_interval double precision, p_cur_lists text[], p_since bigint)
RETURNS boolean
    LANGUAGE sql VOLATILE
    SET search_path = pg_catalog, public, pg_temp
AS $$
    SELECT p_rec_interval IS NOT NULL AND p_rec_lists IS NOT NULL
       AND p_rec_lists @> COALESCE(p_cur_lists, ARRAY[]::text[])
       AND p_at > pg_catalog.clock_timestamp() - pg_catalog.make_interval(secs => LEAST(p_rec_interval::double precision, p_cur_interval, p_since::double precision))
$$;

-- =========================================================================
-- 3. The gate, the scheduler, the finish function and the officer record (bodies of 0181 plus the provenance rules)
-- =========================================================================
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

CREATE OR REPLACE FUNCTION aml_screening_request_claim(p_tenant uuid, p_limit integer, p_lease_seconds integer)
RETURNS TABLE (o_id uuid, o_person_id uuid, o_jurisdiction_id uuid, o_trigger text, o_idempotency_key text, o_attempts integer, o_claim_token uuid)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
BEGIN
    IF p_tenant IS NULL OR p_limit IS NULL OR p_limit < 1 OR p_limit > 200 OR p_lease_seconds IS NULL OR p_lease_seconds < 5 OR p_lease_seconds > 3600 THEN
        RAISE EXCEPTION 'aml: invalid claim arguments' USING ERRCODE = 'AT001';
    END IF;
    IF p_tenant IS DISTINCT FROM public.aml_session_tenant() THEN
        RAISE EXCEPTION 'aml: the session tenant is not the requested tenant' USING ERRCODE = 'AT002';
    END IF;
    RETURN QUERY
        WITH due AS (
            SELECT r.id AS rid FROM public.aml_screening_requests r
             WHERE r.tenant_id = p_tenant
               AND ((r.status = 'pending' AND r.next_attempt_at <= pg_catalog.clock_timestamp())
                    OR (r.status = 'claimed' AND r.lease_until <= pg_catalog.clock_timestamp()))
             ORDER BY r.next_attempt_at, r.id
             LIMIT p_limit
             FOR UPDATE SKIP LOCKED
        )
        UPDATE public.aml_screening_requests u
           SET status = 'claimed', attempts = u.attempts + 1, claim_token = gen_random_uuid(),
               lease_until = pg_catalog.clock_timestamp() + pg_catalog.make_interval(secs => p_lease_seconds::double precision),
               -- d1 SEC L-1 / M-1: the claim time and the screening policy version in force NOW (the version the finish must name)
               claimed_at = pg_catalog.clock_timestamp(),
               claim_policy_id = (SELECT sp.id FROM public.aml_screening_policies sp
                                   WHERE sp.tenant_id = p_tenant AND sp.licensing_jurisdiction_id = u.licensing_jurisdiction_id
                                     AND sp.effective_from <= pg_catalog.clock_timestamp() ORDER BY sp.created_at DESC, sp.id DESC LIMIT 1),
               updated_at = pg_catalog.clock_timestamp()
          FROM due
         WHERE u.id = due.rid
        RETURNING u.id, u.person_id, u.licensing_jurisdiction_id, u.trigger, u.idempotency_key, u.attempts, u.claim_token;
END
$$;

DROP FUNCTION aml_screening_request_finish(uuid, uuid, uuid, text, text[], text, text, text, text, timestamptz, integer, text);
CREATE FUNCTION aml_screening_request_finish(
    p_tenant uuid, p_id uuid, p_claim_token uuid, p_result text, p_match_lists text[], p_error_code text,
    p_provider_id text, p_provider_reference text, p_subject_ref text, p_screened_at timestamptz, p_backoff_seconds integer,
    p_match_fingerprint text, p_policy_id uuid
) RETURNS TABLE (o_applied boolean, o_status text, o_reviewed boolean)
    LANGUAGE plpgsql SECURITY DEFINER
    SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v     record;
    v_rev uuid;
    v_ri  bigint;
    v_age interval;
    v_pol record;
BEGIN
    IF p_tenant IS NULL OR p_id IS NULL OR p_claim_token IS NULL OR p_result IS NULL
       OR p_result NOT IN ('clear', 'potential_match', 'confirmed', 'retry', 'terminal') THEN
        RAISE EXCEPTION 'aml: invalid finish arguments' USING ERRCODE = 'AT001';
    END IF;
    IF p_tenant IS DISTINCT FROM public.aml_session_tenant() THEN
        RAISE EXCEPTION 'aml: the session tenant is not the requested tenant' USING ERRCODE = 'AT002';
    END IF;
    SELECT * INTO v FROM public.aml_screening_requests r WHERE r.id = p_id AND r.tenant_id = p_tenant FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'aml: screening request not found' USING ERRCODE = 'AT002';
    END IF;
    IF v.status <> 'claimed' OR v.claim_token IS DISTINCT FROM p_claim_token THEN
        RETURN QUERY SELECT false, v.status, false;  -- someone else holds it, or it already ended: nothing is applied
        RETURN;
    END IF;
    -- d1 SEC M-1: a clear or a match finished AFTER its lease passed is not applied (the request is reclaimed and retried; a slow finish is
    -- retried, not lost). A retry or a terminal failure never makes a person pass, so those are still applied.
    IF p_result IN ('clear', 'potential_match', 'confirmed') AND v.lease_until <= pg_catalog.clock_timestamp() THEN
        RETURN QUERY SELECT false, v.status, false;
        RETURN;
    END IF;
    IF p_result = 'clear' THEN
        IF p_match_lists IS NOT NULL OR p_provider_id IS NULL OR p_screened_at IS NULL OR p_provider_reference IS NULL THEN
            RAISE EXCEPTION 'aml: a clear carries no match and names its provider, its provider reference and time' USING ERRCODE = 'AT001';
        END IF;
        PERFORM public.aml_screening_assert_worker_provider(p_provider_id);
        -- d1 D10: a clear (and a match, which may be auto-reviewed into a clear) records WHICH screening policy version it was screened under: the
        -- lists and the re-screen interval come from that row (never from the caller), and the row must be a version of this tenant and of the
        -- request's jurisdiction that is in effect.
        -- d1 SEC M-1 / LOW-1: the screening cannot be stated to have happened in the future, or before this claim (engineering tolerances of 120 s,
        -- not business values; a vendor answer older than that is not a fresh screening for this request).
        IF p_screened_at > pg_catalog.clock_timestamp() + interval '120 seconds'
           OR p_screened_at < COALESCE(v.claimed_at, pg_catalog.clock_timestamp()) - interval '120 seconds' THEN
            RAISE EXCEPTION 'aml: the screening time is in the future or long before the claim' USING ERRCODE = 'AT001';
        END IF;
        -- d1 SEC L-1: the policy named is the version that was in force when the request was claimed (the version the lists came from).
        IF v.claim_policy_id IS DISTINCT FROM p_policy_id THEN
            RAISE EXCEPTION 'aml: the screening policy version is not the one in force when the request was claimed' USING ERRCODE = 'AT001';
        END IF;
        IF p_policy_id IS NULL THEN
            RAISE EXCEPTION 'aml: a clear or a match names the screening policy version it was screened under' USING ERRCODE = 'AT001';
        END IF;
        SELECT sp.lists, sp.rescreen_interval_seconds INTO v_pol FROM public.aml_screening_policies sp
         WHERE sp.id = p_policy_id AND sp.tenant_id = p_tenant AND sp.licensing_jurisdiction_id = v.licensing_jurisdiction_id
           AND sp.effective_from <= pg_catalog.clock_timestamp();
        IF NOT FOUND THEN
            RAISE EXCEPTION 'aml: the screening policy version is not a version in effect for this request''s jurisdiction' USING ERRCODE = 'AT001';
        END IF;
        UPDATE public.aml_screening_requests SET status = 'done', outcome = 'clear', provider_id = p_provider_id, provider_reference = p_provider_reference,
               subject_ref = p_subject_ref, screened_at = p_screened_at, completed_at = pg_catalog.clock_timestamp(), claim_token = NULL, lease_until = NULL,
               last_error_code = NULL, screened_policy_id = p_policy_id, screened_lists = v_pol.lists, screened_interval_seconds = v_pol.rescreen_interval_seconds,
               updated_at = pg_catalog.clock_timestamp() WHERE id = p_id;
        RETURN QUERY SELECT true, 'done'::text, false;
    ELSIF p_result IN ('potential_match', 'confirmed') THEN
        IF p_match_lists IS NULL OR p_provider_id IS NULL OR p_screened_at IS NULL THEN
            RAISE EXCEPTION 'aml: a match names its list, provider and time' USING ERRCODE = 'AT001';
        END IF;
        PERFORM public.aml_screening_assert_worker_provider(p_provider_id);
        -- d1 D10: a clear (and a match, which may be auto-reviewed into a clear) records WHICH screening policy version it was screened under: the
        -- lists and the re-screen interval come from that row (never from the caller), and the row must be a version of this tenant and of the
        -- request's jurisdiction that is in effect.
        -- d1 SEC M-1 / LOW-1: the screening cannot be stated to have happened in the future, or before this claim (engineering tolerances of 120 s,
        -- not business values; a vendor answer older than that is not a fresh screening for this request).
        IF p_screened_at > pg_catalog.clock_timestamp() + interval '120 seconds'
           OR p_screened_at < COALESCE(v.claimed_at, pg_catalog.clock_timestamp()) - interval '120 seconds' THEN
            RAISE EXCEPTION 'aml: the screening time is in the future or long before the claim' USING ERRCODE = 'AT001';
        END IF;
        -- d1 SEC L-1: the policy named is the version that was in force when the request was claimed (the version the lists came from).
        IF v.claim_policy_id IS DISTINCT FROM p_policy_id THEN
            RAISE EXCEPTION 'aml: the screening policy version is not the one in force when the request was claimed' USING ERRCODE = 'AT001';
        END IF;
        IF p_policy_id IS NULL THEN
            RAISE EXCEPTION 'aml: a clear or a match names the screening policy version it was screened under' USING ERRCODE = 'AT001';
        END IF;
        SELECT sp.lists, sp.rescreen_interval_seconds INTO v_pol FROM public.aml_screening_policies sp
         WHERE sp.id = p_policy_id AND sp.tenant_id = p_tenant AND sp.licensing_jurisdiction_id = v.licensing_jurisdiction_id
           AND sp.effective_from <= pg_catalog.clock_timestamp();
        IF NOT FOUND THEN
            RAISE EXCEPTION 'aml: the screening policy version is not a version in effect for this request''s jurisdiction' USING ERRCODE = 'AT001';
        END IF;
        IF p_match_fingerprint IS NOT NULL AND p_match_fingerprint !~ '^[0-9a-f]{64}$' THEN
            RAISE EXCEPTION 'aml: malformed match fingerprint' USING ERRCODE = 'AT001';
        END IF;
        -- lic23 N-2: the SAME match (same provider-stated identity) for the same person that an officer already REVIEWED AND CLEARED is
        -- recorded as such instead of holding the person again for ever. A different or new hit still needs an officer.
        v_rev := NULL;
        IF p_match_fingerprint IS NOT NULL THEN
            -- lic26 F-2: the earlier match must have the SAME outcome and cover every list now named (an officer-cleared potential
            -- sanctions match later upgraded to confirmed, or a new list, is a NEW finding), and the ORIGINAL officer review must be no
            -- older than FOUR re-screen intervals of the request's jurisdiction (capped at 180 days; 180 days where no screening policy
            -- exists): an ENGINEERING bound, so a review never carries forward for ever however many auto-reviews chain.
            SELECT sp.rescreen_interval_seconds INTO v_ri FROM public.aml_screening_policies sp
             WHERE sp.tenant_id = p_tenant AND sp.licensing_jurisdiction_id = v.licensing_jurisdiction_id
               AND sp.effective_from <= pg_catalog.clock_timestamp()
             ORDER BY sp.created_at DESC, sp.id DESC LIMIT 1;
            v_age := CASE WHEN v_ri IS NULL THEN interval '180 days'
                          ELSE pg_catalog.make_interval(secs => LEAST(v_ri::double precision * 4, 15552000)) END;
            SELECT COALESCE(q.reviewed_from_request_id, q.id) INTO v_rev FROM public.aml_screening_requests q
             WHERE q.tenant_id = p_tenant AND q.person_id = v.person_id AND q.id <> p_id AND q.status = 'needs_officer'
               AND q.match_fingerprint = p_match_fingerprint
               AND q.outcome = p_result AND p_match_lists <@ q.match_lists
               AND public.aml_screening_review_cleared_at(COALESCE(q.reviewed_from_request_id, q.id))
                   > pg_catalog.clock_timestamp() - v_age
             ORDER BY q.completed_at DESC LIMIT 1;
        END IF;
        UPDATE public.aml_screening_requests SET status = 'needs_officer', outcome = p_result, match_lists = p_match_lists, provider_id = p_provider_id,
               provider_reference = p_provider_reference, subject_ref = p_subject_ref, screened_at = p_screened_at,
               match_fingerprint = p_match_fingerprint, reviewed_from_request_id = v_rev,
               completed_at = pg_catalog.clock_timestamp(), claim_token = NULL, lease_until = NULL, last_error_code = NULL,
               screened_policy_id = p_policy_id, screened_lists = v_pol.lists, screened_interval_seconds = v_pol.rescreen_interval_seconds,
               updated_at = pg_catalog.clock_timestamp() WHERE id = p_id;
        RETURN QUERY SELECT true, 'needs_officer'::text, v_rev IS NOT NULL;
    ELSIF p_result = 'retry' THEN
        IF p_error_code IS NULL OR p_backoff_seconds IS NULL OR p_backoff_seconds < 0 OR p_backoff_seconds > 86400 THEN
            RAISE EXCEPTION 'aml: a retry names its error code and a bounded backoff' USING ERRCODE = 'AT001';
        END IF;
        UPDATE public.aml_screening_requests SET status = 'pending', next_attempt_at = pg_catalog.clock_timestamp() + pg_catalog.make_interval(secs => p_backoff_seconds::double precision),
               last_error_code = p_error_code, claim_token = NULL, lease_until = NULL, updated_at = pg_catalog.clock_timestamp() WHERE id = p_id;
        RETURN QUERY SELECT true, 'pending'::text, false;
    ELSE
        IF p_error_code IS NULL THEN
            RAISE EXCEPTION 'aml: a terminal failure names its error code' USING ERRCODE = 'AT001';
        END IF;
        UPDATE public.aml_screening_requests SET status = 'failed_terminal', last_error_code = p_error_code, completed_at = pg_catalog.clock_timestamp(),
               claim_token = NULL, lease_until = NULL, updated_at = pg_catalog.clock_timestamp() WHERE id = p_id;
        RETURN QUERY SELECT true, 'failed_terminal'::text, false;
    END IF;
END
$$;

CREATE OR REPLACE FUNCTION aml_screening_record(
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
    v_req    record;
    v_hold   uuid;
    v_result uuid;
    v_hash   text;
    v_class  text;
    v_jur    uuid;
    v_prov   record;
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
    SELECT r.id, r.hold_id, r.payload_hash INTO v_prior
      FROM public.aml_screening_results r WHERE r.tenant_id = p_tenant AND r.idempotency_key = p_idempotency_key;
    IF FOUND THEN
        IF v_prior.payload_hash <> v_hash THEN
            RAISE EXCEPTION 'aml: the idempotency key was used with different content' USING ERRCODE = 'AM004';
        END IF;
        RETURN QUERY SELECT v_prior.id, v_prior.hold_id, true;
        RETURN;
    END IF;
    -- d1 SEC M-1: an officer cannot state a screening in the future (120 s clock tolerance, not a business value); an OLD screened_at only shortens freshness.
    IF p_screened_at > pg_catalog.clock_timestamp() + interval '120 seconds' THEN
        RAISE EXCEPTION 'aml: the screening time is in the future' USING ERRCODE = 'AM004';
    END IF;
    SELECT q.person_id, q.trigger, q.status, q.outcome, q.match_lists, q.licensing_jurisdiction_id INTO v_req
      FROM public.aml_screening_requests q WHERE q.tenant_id = p_tenant AND q.idempotency_key = p_idempotency_key;
    IF FOUND THEN
        v_jur := v_req.licensing_jurisdiction_id;
        IF v_req.person_id <> p_person OR v_req.trigger <> p_trigger THEN
            RAISE EXCEPTION 'aml: the key belongs to a screening request of another person or trigger' USING ERRCODE = 'AM004', HINT = 'request_key_mismatch';
        END IF;
        -- SN-2: only a request that is waiting for an officer (a match, or a terminal failure) can be resolved by a record under its
        -- key. A pending / claimed / done / cancelled request is not: a record there would pre-resolve a later result.
        IF v_req.status NOT IN ('needs_officer', 'failed_terminal') THEN
            RAISE EXCEPTION 'aml: the key belongs to a screening request that is not awaiting an officer' USING ERRCODE = 'AM004', HINT = 'request_not_awaiting_officer';
        END IF;
        IF v_req.status = 'needs_officer' THEN
            IF p_outcome = 'error' THEN
                RAISE EXCEPTION 'aml: an error result cannot resolve a match awaiting an officer' USING ERRCODE = 'AM004', HINT = 'error_cannot_resolve';
            END IF;
            PERFORM public.aml_assert_quorum(p_tenant, v_owner);
            IF (v_req.outcome = 'confirmed' OR COALESCE('sanctions' = ANY (v_req.match_lists), false))
               AND NOT (p_outcome = 'confirmed' OR (p_outcome = 'potential_match' AND COALESCE('sanctions' = ANY (p_match_lists), false))) THEN
                RAISE EXCEPTION 'aml: a sanctions-class match is resolved only by recording a sanctions match (a hold lifted by four-eyes), never by a clear' USING ERRCODE = 'AM006', HINT = 'sanctions_clear_refused';
            END IF;
            -- lic26 NEW-1: and that record must PLACE the sanctions-class hold (a sanctions list named, a hold decision). A sanctions
            -- match recorded with decision `none` would "resolve" the request without any hold and without the four-eyes lift.
            IF (v_req.outcome = 'confirmed' OR COALESCE('sanctions' = ANY (v_req.match_lists), false))
               AND (p_decision = 'none' OR NOT COALESCE('sanctions' = ANY (p_match_lists), false)) THEN
                RAISE EXCEPTION 'aml: a sanctions-class match is resolved only by a sanctions match that places a hold' USING ERRCODE = 'AM006', HINT = 'sanctions_match_needs_hold';
            END IF;
        END IF;
    ELSIF p_idempotency_key ~ '^(reg|kyc|sched|repl):' THEN
        -- SN-2: those key prefixes belong to the automatic screening queue; an officer record may not squat a key the system will use
        -- (reg:<account>, kyc:<verification>, sched:<person>:<bucket>, repl:<person>:<id>).
        RAISE EXCEPTION 'aml: this key prefix is reserved for the automatic screening queue' USING ERRCODE = 'AM004', HINT = 'key_prefix_reserved';
    END IF;
    IF p_decision <> 'none' THEN
        IF p_hold_reason_code IS NULL OR p_hold_reason_code !~ '^[a-z0-9_]{1,64}$' THEN
            RAISE EXCEPTION 'aml: a hold decision needs a reason code' USING ERRCODE = 'AM004';
        END IF;
        IF p_decision = 'hold_all_value_movement' AND NOT (p_outcome = 'confirmed' OR COALESCE('sanctions' = ANY (p_match_lists), false)) THEN
            RAISE EXCEPTION 'aml: all-value-movement is the decision of a confirmed outcome or a sanctions-list match only' USING ERRCODE = 'AM004';
        END IF;
        v_class := CASE WHEN COALESCE('sanctions' = ANY (p_match_lists), false)
                        THEN CASE WHEN p_outcome = 'confirmed' THEN 'confirmed_sanctions' ELSE 'sanctions_potential' END
                        ELSE 'manual' END;
        PERFORM public.aml_assert_quorum(p_tenant, v_owner);
        INSERT INTO public.aml_holds (tenant_id, owner_scope, person_id, player_account_id, hold_scope, reason_class, reason_code,
                                      placed_by, placed_by_person_id, placed_by_scope)
        VALUES (p_tenant, v_owner, p_person, NULL,
                CASE p_decision WHEN 'hold_all_value_movement' THEN 'all_value_movement' ELSE 'withdrawal_and_payout' END,
                v_class,
                p_hold_reason_code, a.o_actor, a.o_person_id, a.o_scope)
        RETURNING id INTO v_hold;
    END IF;
    -- d1 D10: the provenance of this result. The database stamps the screening policy version in force NOW (for the jurisdiction of the request this
    -- key resolves; otherwise the version with the SHORTEST re-screen interval among the tenant's jurisdictions: conservative) and its interval. The
    -- lists are the ones the officer states in p_lists. No policy in force: nothing is stamped, and the result is never trusted as a fresh clear.
    SELECT cur.id, cur.rescreen_interval_seconds INTO v_prov FROM (
        SELECT DISTINCT ON (sp.licensing_jurisdiction_id) sp.id, sp.rescreen_interval_seconds, sp.licensing_jurisdiction_id
          FROM public.aml_screening_policies sp
         WHERE sp.tenant_id = p_tenant AND sp.effective_from <= pg_catalog.clock_timestamp()
         ORDER BY sp.licensing_jurisdiction_id, sp.created_at DESC, sp.id DESC) cur
     WHERE v_jur IS NULL OR cur.licensing_jurisdiction_id = v_jur
     ORDER BY cur.rescreen_interval_seconds, cur.id LIMIT 1;
    INSERT INTO public.aml_screening_results (tenant_id, owner_scope, person_id, idempotency_key, trigger, provider_id, provider_reference,
                                              subject_ref, lists, outcome, match_lists, error_code, screened_at, decision, hold_id,
                                              hold_reason_class, payload_hash, recorded_by, recorded_by_person_id, recorded_by_scope,
                                              screened_policy_id, screened_interval_seconds)
    VALUES (p_tenant, v_owner, p_person, p_idempotency_key, p_trigger, p_provider_id, p_provider_reference, p_subject_ref, p_lists,
            p_outcome, p_match_lists, p_error_code, p_screened_at, p_decision, v_hold, v_class, v_hash, a.o_actor, a.o_person_id, a.o_scope,
            v_prov.id, v_prov.rescreen_interval_seconds)
    RETURNING id INTO v_result;
    RETURN QUERY SELECT v_result, v_hold, false;
END
$$;

-- =========================================================================
-- 4. Grants (mirrored by deploy/init-app-role.sql)
-- =========================================================================
REVOKE ALL ON FUNCTION aml_screening_covers(timestamptz, bigint, text[], double precision, text[], bigint) FROM PUBLIC;
REVOKE ALL ON FUNCTION aml_screening_since(uuid, uuid, timestamptz, text[]) FROM PUBLIC;
REVOKE ALL ON FUNCTION aml_screening_request_finish(uuid, uuid, uuid, text, text[], text, text, text, text, timestamptz, integer, text, uuid) FROM PUBLIC;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON FUNCTION aml_screening_since(uuid, uuid, timestamptz, text[]) FROM igaming_runtime';
        EXECUTE 'REVOKE ALL ON FUNCTION aml_screening_covers(timestamptz, bigint, text[], double precision, text[], bigint) FROM igaming_runtime';
        EXECUTE 'GRANT EXECUTE ON FUNCTION aml_screening_request_finish(uuid, uuid, uuid, text, text[], text, text, text, text, timestamptz, integer, text, uuid) TO igaming_runtime';
    END IF;
END
$$;
COMMENT ON FUNCTION aml_screening_covers(timestamptz, bigint, text[], double precision, text[], bigint) IS '0192 (d1, D10): does a screening (time, RECORDED interval and lists) still cover the person under the interval and lists NOW in force? Fail closed: no provenance, a list missing, a list dropped since, or older than LEAST(recorded, current interval) is not covered.';
