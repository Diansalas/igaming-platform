-- Stage 4D-RG specialist review fixes (security + PostgreSQL/RLS reviews,
-- both independently reproduced against a live database):
--
-- 1. player_self_read (migration 0037) matched on person_id alone, with
--    no tenant predicate. Because a Person is deliberately cross-tenant,
--    a player with accounts at two DIFFERENT tenants could read a
--    tenant-scoped restriction belonging to the OTHER tenant via their
--    own "my RG status" self-service endpoint - leaking that tenant's
--    confidential reason_code, and reporting "active": true for a
--    restriction that internal/rg.EvaluateEligibility does NOT actually
--    enforce at the CURRENT tenant (the status endpoint and the
--    enforcement boundary disagreeing is itself a correctness bug, not
--    just a confidentiality one). Fixed by adding the identical
--    tenant-scoping predicate EvaluateEligibility's own query already
--    uses: a platform-wide row is always visible; a tenant-scoped row is
--    visible only to that same tenant.
DROP POLICY player_self_read ON player_restrictions;

CREATE POLICY player_self_read ON player_restrictions
    FOR SELECT
    USING (
        person_id = (
            SELECT person_id FROM player_accounts
            WHERE id = NULLIF(current_setting('app.player_account_id', true), '')::uuid
        )
        AND (
            tenant_id IS NULL
            OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
        )
    );

-- 2. The staff_and_system_update_visibility/..._delete_visibility policies
--    (migration 0037) granted broader UPDATE/DELETE row visibility than
--    audit_log's own dual-scope pattern they claimed to mirror - a
--    tenant-scoped connection's mutation scope included every OTHER
--    tenant's platform-wide rows too (USING has no per-command WITH
--    CHECK re-homing guard for UPDATE, only for INSERT). If a future
--    migration ever disabled the row-level deny-mutation trigger, this
--    would let any one tenant silently neuter every platform-wide
--    self-exclusion on the entire platform. Replaced with a
--    STATEMENT-level trigger, which fires unconditionally per statement
--    regardless of how many rows RLS makes visible (including zero) -
--    the mutation attempt fails LOUDLY with the same named exception
--    even when RLS itself would have silently matched no rows, without
--    needing any UPDATE/DELETE visibility policy at all. The existing
--    row-level trigger is left in place as defense-in-depth (two
--    independent triggers must both be disabled to open a gap, not one).
DROP POLICY staff_and_system_update_visibility ON player_restrictions;
DROP POLICY staff_and_system_delete_visibility ON player_restrictions;

CREATE TRIGGER player_restrictions_immutable_statement
    BEFORE UPDATE OR DELETE ON player_restrictions
    FOR EACH STATEMENT EXECUTE FUNCTION player_restrictions_deny_mutation();

-- 3. Identity/architect review finding: player_accounts had no composite
--    (id, tenant_id) key for player_restrictions.player_account_id to
--    reference alongside tenant_id, so a forged row pairing a real
--    player_account_id with a WRONG tenant_id was not rejected by any FK
--    (only by RLS's WITH CHECK, and only for the insert path this
--    package's own Go code takes - not a defense-in-depth guarantee at
--    the database level the way brands' own (id, tenant_id) FK already
--    is). Adding this composite FK is safe under Postgres's MATCH SIMPLE
--    default: trivially satisfied when tenant_id IS NULL (the
--    platform-wide case, already covered by the existing single-column
--    FK to player_accounts(id)), and newly enforced whenever tenant_id is
--    set - mirroring the (brand_id, tenant_id) -> brands(id, tenant_id)
--    FK migration 0037 already uses for exactly this reason.
ALTER TABLE player_accounts ADD CONSTRAINT player_accounts_id_tenant_id_key UNIQUE (id, tenant_id);

ALTER TABLE player_restrictions
    ADD CONSTRAINT player_restrictions_player_account_id_tenant_id_fkey
    FOREIGN KEY (player_account_id, tenant_id) REFERENCES player_accounts (id, tenant_id);
