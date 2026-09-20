-- Stage 4I Phase E-SECURITY: row-level security on the tenant/licence/
-- jurisdiction ROOT layer (architect ruling; task-registry MKT-SCOPE-1 and
-- MKT-SCOPE-1(b); supersedes ADR 0045 §18 finding F3's "NOT AUTHORIZED
-- THIS DISPATCH" disposition, which is now DISCHARGED).
--
-- WHY: `licence_country_ceilings_read` (migration 0076) anchors its
-- composite-ownership EXISTS on `tenants.licence_id`, and
-- `operating_country_policies_enforce_ceiling()` step 1 reads the same
-- column. Both ran under the TENANT's own connection, and that connection
-- could WRITE that column. Live-reproduced: one ordinary tenant-scoped
-- transaction repointed its own tenants.licence_id at another tenant's
-- BYOL licence and committed an `enabled` tenant-rung policy for a country
-- its own licence never permitted. Nothing in migration 0076 is defective;
-- its premise ("tenants carries no RLS (verified)") was.
--
-- READ POSTURE IS DELIBERATELY ASYMMETRIC AND DERIVED, NOT UNIFORM:
--   tenants       -> USING (true): identity.GetTenantBySlug (staff-login,
--                    no tenant context by construction), three
--                    WithoutTenant active-tenant sweeps (reconciliation /
--                    bonus / rg), AND migration 0076's own EXISTS + ceiling
--                    trigger all read it from scopes that have no tenant
--                    match to offer. Migration 0044's `assets` precedent,
--                    same reasoning, same words.
--   jurisdictions -> USING (true): resolver.go's licence->jurisdiction JOIN
--                    is on the PLAYER-JURISDICTION path (HDR-J-5) and must
--                    not change behaviour. Canonical-model §6.1's posture.
--   licences      -> NARROW: platform-admin, or the tenant whose own
--                    tenants.licence_id names this row. ADR 0045 §4's
--                    already-ruled "a BYOL tenant's own licence ceiling
--                    must not be readable by an unrelated tenant", applied
--                    one level down. Verified against the full integration
--                    suite: no player-scoped path reads this table.
--
-- WRITE POSTURE IS UNIFORM: platform-admin GUC set AND app.tenant_id AND
-- app.player_account_id both UNSET - migration 0044/0045/0075/0076's
-- identical predicate, deliberately NOT a new pattern.

-- ======================================================================
-- tenants
-- ======================================================================
ALTER TABLE tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenants FORCE  ROW LEVEL SECURITY;

-- Fix 5 (Stage 4I Phase E-SECURITY fix round, DB/RLS review): every sibling
-- read policy in this family (licences_read, licence_country_ceilings_read,
-- operating_country_policies_tenant_read) already excludes player scope
-- with a leading NULLIF(app.player_account_id) IS NULL conjunct.
-- tenants_read did not. All three reviewers who found this independently
-- traced every production read site of `tenants` and confirmed none is
-- player-scoped, and DB/RLS empirically verified this predicate is safe by
-- temporarily applying it and running the full whole-repo integration
-- suite with zero failures. Deliberately does NOT go further than this -
-- tenant-to-tenant read visibility is left exactly as USING (true)
-- (tracked separately as PLAT-TENANTREAD-1, owner architect/security).
CREATE POLICY tenants_read ON tenants
    FOR SELECT USING (
        NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY tenants_platform_admin_insert ON tenants
    FOR INSERT WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY tenants_platform_admin_update ON tenants
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

-- A DELETE policy IS required here (unlike licences/jurisdictions): tenant
-- deletion is the ONE legitimate DELETE on this table, and blocking it
-- entirely would break every integration fixture's teardown AND the
-- ON DELETE CASCADE path that migration 0076 §7.3 deliberately preserves
-- for operating_country_policies. Restricting it to platform-admin scope
-- is a NET TIGHTENING: pre-migration, an ordinary tenant-scoped connection
-- could DELETE a DIFFERENT tenant and cascade away that tenant's entire
-- operating-market policy set (verified live) - which is ADR 0045 §18
-- finding F2 reachable at a STRICTLY LOWER bar than F2 itself disclosed.
CREATE POLICY tenants_platform_admin_delete ON tenants
    FOR DELETE USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
-- No FOR ALL policy. No tenant-scoped write policy of any kind - a tenant
-- must NEVER be able to write its own row, which is the entire defect.

-- ======================================================================
-- licences
-- ======================================================================
ALTER TABLE licences ENABLE ROW LEVEL SECURITY;
ALTER TABLE licences FORCE  ROW LEVEL SECURITY;

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
        )
    );
-- The EXISTS above is migration 0076's licence_country_ceilings_read shape,
-- reused verbatim. It cannot recurse: tenants_read is USING (true) and
-- references no other table. Verified live from tenant, platform-admin,
-- player and scopeless connections.

CREATE POLICY licences_platform_admin_insert ON licences
    FOR INSERT WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY licences_platform_admin_update ON licences
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
-- NO DELETE POLICY, DELIBERATELY: no production or test code deletes a
-- licence (`rg "DELETE FROM licences"` -> zero hits), this table is the
-- root of licence_country_ceilings' FK (no cascade), and a licence is a
-- record of a real-world legal instrument. Same posture as
-- licence_country_ceilings itself. No FOR ALL policy.

-- ======================================================================
-- jurisdictions
-- ======================================================================
ALTER TABLE jurisdictions ENABLE ROW LEVEL SECURITY;
ALTER TABLE jurisdictions FORCE  ROW LEVEL SECURITY;

CREATE POLICY jurisdictions_read ON jurisdictions
    FOR SELECT USING (true);

CREATE POLICY jurisdictions_platform_admin_insert ON jurisdictions
    FOR INSERT WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );

CREATE POLICY jurisdictions_platform_admin_update ON jurisdictions
    FOR UPDATE
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    )
    WITH CHECK (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
    );
-- No DELETE policy, no FOR ALL policy. SetJurisdictionCountryCode's
-- existing in-function assertPlatformScope now finally has the
-- database-level backstop its own comment says it lacks.

-- ======================================================================
-- BYOL licence exclusivity (Task 3(B)/(C)) - NOT an RLS matter
-- ======================================================================
-- Live-verified gap, recorded nowhere before this migration: two distinct
-- tenants, BOTH licensing_model='own_licence', can bind the SAME
-- licensee='tenant' licence. `tenants_licence_matches_model` (migration
-- 0007) only checks licensee KIND, never exclusivity. Because
-- licence_country_ceilings is keyed on licence_id with NO tenant column,
-- the second tenant silently inherits the first's entire country ceiling.
--
-- SCOPE IS DELIBERATELY MINIMAL AND INVENTS NO BYOL ONBOARDING: this index
-- says only what ADR 0006 already says - a licence a TENANT brought is that
-- tenant's; the PLATFORM's own licence (licensee='platform') is shared
-- across every under_platform_licence tenant and is deliberately NOT
-- constrained. It keys on the GENERATED column expected_licensee, so it
-- cannot drift from licensing_model.
CREATE UNIQUE INDEX uq_tenants_exclusive_own_licence
    ON tenants (licence_id)
    WHERE licence_id IS NOT NULL AND expected_licensee = 'tenant';

-- ======================================================================
-- Deny-TRUNCATE triggers (Fix 4, Stage 4I Phase E-SECURITY fix round)
-- ======================================================================
-- `licence_country_ceilings`, `operating_country_policies`,
-- `platform_operations`, and `audit_log` all already carry explicit
-- `BEFORE TRUNCATE FOR EACH STATEMENT` deny triggers in this codebase.
-- `tenants`, `licences`, and `jurisdictions` had none: PostgreSQL RLS does
-- NOT apply to TRUNCATE at all, so an ordinary tenant-scoped connection's
-- `TRUNCATE tenants CASCADE` was not blocked by any of the policies above -
-- it currently happens to fail only by accident, hitting an unrelated
-- table's append-only trigger partway through the cascade, which is not a
-- real control. One dedicated deny function per table, mirroring
-- audit_log_deny_mutation()'s unconditional-RAISE shape (these tables,
-- unlike audit_log, still permit ordinary UPDATE/INSERT/DELETE, so the
-- function must reject TRUNCATE specifically, not every mutation).
CREATE FUNCTION tenants_deny_truncate() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'tenants may not be truncated';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER tenants_deny_truncate
    BEFORE TRUNCATE ON tenants
    FOR EACH STATEMENT EXECUTE FUNCTION tenants_deny_truncate();

CREATE FUNCTION licences_deny_truncate() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'licences may not be truncated';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER licences_deny_truncate
    BEFORE TRUNCATE ON licences
    FOR EACH STATEMENT EXECUTE FUNCTION licences_deny_truncate();

CREATE FUNCTION jurisdictions_deny_truncate() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'jurisdictions may not be truncated';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER jurisdictions_deny_truncate
    BEFORE TRUNCATE ON jurisdictions
    FOR EACH STATEMENT EXECUTE FUNCTION jurisdictions_deny_truncate();
