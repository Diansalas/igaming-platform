-- Fix: persons (migration 0009) had no tenant_id and no RLS, reasoned
-- about as "platform-level, like jurisdictions/assets". That analogy
-- doesn't hold: jurisdictions/assets are read-only reference data never
-- mutated on a request path, while persons is INSERTed by unauthenticated
-- registration inside a TENANT-SCOPED transaction (see
-- internal/identity/player_account.go's RegisterPlayer, which calls
-- CreatePerson in the same tx as the player_accounts insert). Without
-- RLS, any tenant-scoped connection could SELECT/UPDATE/DELETE every
-- other tenant's persons rows - including the platform-level
-- self-exclusion/AML status (persons.status) this table exists to
-- protect (see docs/architecture/05-identity-architecture.md). Caught in
-- Stage 2 specialist review before any KYC/AML code (Stage 4) starts
-- reading or writing person_key_hash/status for real.
--
-- persons genuinely is platform-wide data, not tenant-owned - a Person
-- can span brands/tenants by design. So the fix is not tenant-scoped RLS
-- (there is no tenant_id column to scope by); it's restricting
-- SELECT/UPDATE/DELETE to the platform scope (WithoutTenant,
-- app.tenant_id unset) while still allowing INSERT unconditionally, since
-- every current caller creates a person from within a tenant-scoped
-- registration transaction. No Stage 2 code path ever SELECTs or UPDATEs
-- persons, so this is a pure tightening with no behavior change today;
-- Stage 4's KYC/AML processing is expected to run platform-scoped when it
-- starts reading/writing this table.

ALTER TABLE persons ENABLE ROW LEVEL SECURITY;
ALTER TABLE persons FORCE ROW LEVEL SECURITY;

CREATE POLICY persons_insert_any_scope ON persons
    FOR INSERT
    WITH CHECK (true);

CREATE POLICY persons_platform_scope_read_write ON persons
    FOR SELECT
    USING (NULLIF(current_setting('app.tenant_id', true), '') IS NULL);

CREATE POLICY persons_platform_scope_update ON persons
    FOR UPDATE
    USING (NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    WITH CHECK (NULLIF(current_setting('app.tenant_id', true), '') IS NULL);

CREATE POLICY persons_platform_scope_delete ON persons
    FOR DELETE
    USING (NULLIF(current_setting('app.tenant_id', true), '') IS NULL);
