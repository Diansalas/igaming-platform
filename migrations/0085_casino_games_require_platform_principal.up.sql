-- Stage 9.1 fix round, closes security P2 finding SEC-S91-3 against
-- migration 0084 (ADR 0081, ARCH-DB-2's catalogue-write-authorization
-- ruling).
--
-- SEC-S91-3: casino_games's write policies
-- (casino_games_platform_admin_insert/_update/_delete_visibility, both
-- migration 0084) only check that app.platform_admin_principal_id is set
-- to SOME non-null uuid - they never verify that uuid actually resolves
-- to a real staff_users row with the right scope. In principle, ANY
-- handler that happens to call db.Pool.WithPlatformAdmin for an unrelated
-- purpose would also satisfy casino_games's write policy at the RLS
-- layer, because the database-level check does not pin a specific
-- principal IDENTITY the way the five sb_* tables' policies pin the
-- exact literal string 'sportsbook_catalogue_sync' (migration 0084 §3-7).
-- This is exactly the gap migration 0044 already closed for `assets` via
-- asset_change_requests_require_platform_principal - the same defense-
-- in-depth shape is applied here, adapted for the fact that casino_games
-- has no requester column of its own to validate: the principal lives
-- only in the session GUC, so the trigger resolves the GUC's value
-- directly rather than a NEW.<column>.
--
-- Deliberately NOT applied to the five sb_* tables (per this fix round's
-- explicit instruction): their write policies already pin the exact
-- literal service-identity string 'sportsbook_catalogue_sync', which is a
-- stronger, already-correct check with no equivalent "any non-null value
-- satisfies it" gap.

-- ----------------------------------------------------------------------
-- casino_games_require_platform_principal
-- ----------------------------------------------------------------------
-- The GUC must resolve to an actual staff_users row with tenant_id IS
-- NULL (a genuine platform-scoped staff member), mirroring migration
-- 0044's asset_change_requests_require_platform_principal exactly: under
-- staff_users' own dual_scope_isolation policy (migration 0011), a
-- platform-scoped transaction (app.tenant_id unset, which
-- db.Pool.WithPlatformAdmin guarantees) sees exactly the tenant_id IS
-- NULL rows, so a bogus/nonexistent uuid - or a tenant-scoped staff id,
-- were one ever reachable here - is simply not resolvable and the write
-- is refused.
--
-- An UNSET GUC (v_principal_id IS NULL, e.g. WithTenant/WithPlayerScope/
-- WithoutTenant/WithPlatformService - none of which touch
-- app.platform_admin_principal_id at all) is deliberately left for the
-- EXISTING RLS policies to refuse, at their own SQLSTATE 42501, rather
-- than raised here: this trigger's added value is narrowly "the GUC is
-- set to a non-null value that isn't a real platform principal" (SEC-
-- S91-3's exact finding), not "re-deny the case RLS already denies
-- correctly" - doing the latter would change a pre-existing, relied-upon
-- error shape (see e.g. TestCatalogueRLS_DeniedScopesCannotWriteAnySixTables)
-- for a scope this trigger has nothing new to add for.
--
-- FOR EACH ROW (not STATEMENT) to mirror casino_games_immutable_identity
-- and migration 0044's own row-level trigger shape in this codebase,
-- even though the check itself does not depend on NEW - the per-row cost
-- is one indexed primary-key lookup against staff_users, negligible next
-- to the INSERT/UPDATE it guards.
CREATE FUNCTION casino_games_require_platform_principal() RETURNS TRIGGER AS $$
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
        RAISE EXCEPTION 'casino_games: platform admin principal % does not resolve to a real platform-scoped (tenant_id IS NULL) staff_users row (SEC-S91-3: the RLS policy alone only checks the GUC is set to some non-null uuid, not that it names a genuine platform-scoped principal)', v_principal_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER casino_games_platform_principal
    BEFORE INSERT OR UPDATE ON casino_games
    FOR EACH ROW EXECUTE FUNCTION casino_games_require_platform_principal();
