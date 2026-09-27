-- B2 (rv-prh-i3-code-review.md §1): both tables are append-only compliance
-- history (a four-eyes policy-authoring trail and the KYC decision/SAR-
-- adjacent audit trail respectively) and must not be silently destroyed
-- by a rollback - mirrors this repository's own established precedent for
-- a destructive rollback on a table that may hold real data (migrations
-- 0048, 0052, 0075 all RAISE EXCEPTION and refuse rather than DROPping
-- history). This migration refuses outright whenever EITHER table is
-- non-empty; it only proceeds with the drop logic below when both are
-- genuinely empty.
-- kyc_enforcement_decisions carries tenant-scoped FORCE ROW LEVEL
-- SECURITY (tenant_isolation, migration 0100's own CREATE POLICY above) -
-- FORCE means even this table's OWNER (the migration role, which the
-- deploy/init-app-role.sql convention deliberately never grants
-- BYPASSRLS) is subject to it. A migration-time check that must see
-- EVERY tenant's rows, not just one, therefore cannot run the plain
-- EXISTS query directly: with no app.tenant_id GUC set, tenant_isolation
-- silently returns zero rows regardless of what the table actually
-- holds, which would make this guard a no-op that never fires. Disabling
-- RLS for the DURATION OF THIS TRANSACTION is safe specifically because
-- it is transactional DDL: if the guard below raises, the whole
-- transaction (including this ALTER) rolls back and FORCE RLS is
-- restored exactly as it was; if the guard does not fire, the table is
-- dropped immediately afterward anyway, so its RLS posture stops
-- mattering. kyc_enforcement_policies carries no such tenant-scoped
-- policy (platform-wide, `USING (true)` for reads) and needs no such
-- bypass.
ALTER TABLE kyc_enforcement_decisions DISABLE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM kyc_enforcement_decisions) THEN
        RAISE EXCEPTION 'kyc_enforcement_decisions is append-only and holds the KYC enforcement decision/SAR-adjacent audit trail; refusing to roll back migration 0100 while rows exist. Manually verify no history must be preserved, then delete all rows (this destroys audit-relevant provenance) before retrying, or restore from backup instead of rolling back.';
    END IF;
    IF EXISTS (SELECT 1 FROM kyc_enforcement_policies) THEN
        RAISE EXCEPTION 'kyc_enforcement_policies is append-only and holds four-eyes policy-authoring provenance; refusing to roll back migration 0100 while rows exist. Manually verify no history must be preserved, then delete all rows (this destroys audit-relevant provenance) before retrying, or restore from backup instead of rolling back.';
    END IF;
END $$;

DROP TABLE kyc_enforcement_decisions;
DROP FUNCTION kyc_enforcement_decisions_deny_mutation();
DROP TABLE kyc_enforcement_policies;
DROP FUNCTION kyc_enforcement_policies_enforce_lifecycle();
DROP FUNCTION kyc_enforcement_policies_stamp_times();
