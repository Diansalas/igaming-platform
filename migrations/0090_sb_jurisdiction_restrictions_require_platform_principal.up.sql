-- Stage 9.2 fix round, closes security P2 finding SEC-S92-2 against
-- migration 0087 (sb_jurisdiction_restrictions), which security reproduced
-- as the IDENTICAL gap SEC-S91-3 already found and fixed for casino_games
-- (migration 0085): sb_jurisdiction_restrictions' write policies
-- (sb_jurisdiction_restrictions_platform_admin_insert/_update/_delete_
-- visibility, migration 0087) only check that app.platform_admin_
-- principal_id is set to SOME non-null uuid - they never verify that uuid
-- actually resolves to a real, platform-scoped staff_users row. Security
-- reproduced, empirically, that a bogus/nonexistent principal uuid can
-- both INSERT a new restriction AND set an existing one's status to
-- 'withdrawn' (the fail-open direction that matters most for a deny-only
-- table like this one) today.
--
-- Numbering note: 0089 was claimed concurrently by a parallel Stage 9.2
-- fix-round workstream (0089_casino_catalogue_governance_hardening,
-- landed in this same fix round against migration 0086's casino
-- catalogue governance), so 0090 is the next free number, re-verified
-- against `ls migrations/` immediately before this file was named.
--
-- Mirrors migration 0085's casino_games_require_platform_principal
-- pattern EXACTLY - same structure, same "GUC unset is left for the
-- pre-existing RLS policies to refuse, at their own SQLSTATE 42501,
-- rather than re-denied here" rationale, same FOR EACH ROW shape even
-- though the check does not depend on NEW - adapted only for this
-- table's own name and RAISE message. See migration 0085's own header
-- comment for the full rationale; not repeated verbatim here.
--
-- Deliberately NOT applied to sb_exposure_limits (migration 0088) or the
-- five sb_* catalogue tables (migration 0084): sb_exposure_limits' own
-- tenant_staff_scope policy is a normal tenant-scoped RLS check with no
-- platform-admin-principal GUC involved at all, and the five sb_*
-- catalogue tables pin the exact literal service-identity string
-- 'sportsbook_catalogue_sync' (migration 0084), which has no equivalent
-- "any non-null value satisfies it" gap - migration 0087's own header
-- comment already makes this same distinction for why THIS trigger was
-- not added at that table's own creation time.

CREATE FUNCTION sb_jurisdiction_restrictions_require_platform_principal() RETURNS TRIGGER AS $$
DECLARE
    v_principal_id UUID;
    v_is_platform_scoped BOOLEAN;
BEGIN
    v_principal_id := NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid;

    IF v_principal_id IS NULL THEN
        RETURN NEW;
    END IF;

    SELECT su.tenant_id IS NULL INTO v_is_platform_scoped
      FROM staff_users su
     WHERE su.id = v_principal_id;

    IF NOT FOUND OR NOT v_is_platform_scoped THEN
        RAISE EXCEPTION 'sb_jurisdiction_restrictions: platform admin principal % does not resolve to a real platform-scoped (tenant_id IS NULL) staff_users row (SEC-S92-2: the RLS policy alone only checks the GUC is set to some non-null uuid, not that it names a genuine platform-scoped principal)', v_principal_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- BEFORE INSERT OR UPDATE covers both fail-open directions security
-- reproduced: a bogus principal both creating a new restriction (INSERT)
-- and withdrawing/reactivating an existing one (UPDATE, since status is
-- the only mutable column besides reason_code per migration 0087's own
-- immutable-identity trigger).
CREATE TRIGGER sb_jurisdiction_restrictions_platform_principal
    BEFORE INSERT OR UPDATE ON sb_jurisdiction_restrictions
    FOR EACH ROW EXECUTE FUNCTION sb_jurisdiction_restrictions_require_platform_principal();
