-- d1 (security closure before merge). COMPLIANCE / SECURITY MIGRATION: FOR `security` (BLOCKING) and `identity-compliance` REVIEW.
--
-- Part 1 (D7, this commit): REMOVE the runtime role's EXECUTE on the unrouted break-glass setter aml_screening_policy_set (0154).
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
