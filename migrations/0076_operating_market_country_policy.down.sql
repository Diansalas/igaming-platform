-- Reverses migration 0076. Refuses outright while operating_country_
-- policies or licence_country_ceilings hold any row (both are
-- append-only and may hold real authoring history and provenance -
-- mirroring migration 0075's own precedent, and migrations 0048/0052's
-- established pattern for a destructive rollback on a table that may
-- hold real data), and refuses to drop jurisdictions.country_code if any
-- row has a non-NULL value (that value is operator-authored administrative
-- metadata, not migration-created content).
--
-- platform_operations is a seeded vocabulary table with no application
-- writer beyond this migration's own INSERT, so it is safe to drop
-- unconditionally alongside its four seeded rows.

-- IMPORTANT: unlike migration 0075's jurisdiction_precedence_configs
-- (which has a deliberately permissive `FOR SELECT USING (true)` read
-- policy), BOTH new tables here have a narrower read policy than a
-- scopeless migration connection can ever satisfy - licence_country_
-- ceilings requires platform-admin-or-own-licence-tenant scope, and
-- operating_country_policies has NO platform-wide read policy AT ALL, by
-- design (ADR 0045 Section 7.3: a tenant's operating footprint is not
-- platform-readable). Migrations run via a plain connection with none of
-- those GUCs set (db.Pool.MigrateDown acquires a bare connection, it does
-- not go through WithTenant/WithPlatformAdmin), so an EXISTS check against
-- either table under standard RLS enforcement would ALWAYS see zero rows
-- regardless of real content - silently defeating this very guard.
-- Temporarily disabling RLS for this check is therefore required, not a
-- shortcut. This is safe and self-contained: both ALTER TABLE statements
-- run inside this migration's own transaction, so if either EXISTS check
-- below fires a RAISE EXCEPTION, the ENTIRE transaction - including both
-- ALTER TABLEs - rolls back, leaving RLS posture completely untouched on
-- a refused rollback. If neither fires, the migration proceeds to DROP
-- both tables outright, so their RLS posture is moot from that point on.
DO $$
BEGIN
    ALTER TABLE operating_country_policies DISABLE ROW LEVEL SECURITY;
    ALTER TABLE licence_country_ceilings DISABLE ROW LEVEL SECURITY;

    IF EXISTS (SELECT 1 FROM operating_country_policies) THEN
        RAISE EXCEPTION 'operating_country_policies is append-only and may hold policy-authoring history; refusing to roll back migration 0076 while rows exist. Manually verify no history must be preserved, then delete all rows (this destroys audit-relevant provenance) before retrying, or restore from backup instead of rolling back.';
    END IF;
    IF EXISTS (SELECT 1 FROM licence_country_ceilings) THEN
        RAISE EXCEPTION 'licence_country_ceilings is append-only and may hold ceiling-authoring history; refusing to roll back migration 0076 while rows exist. Manually verify no history must be preserved, then delete all rows (this destroys audit-relevant provenance) before retrying, or restore from backup instead of rolling back.';
    END IF;
    IF EXISTS (SELECT 1 FROM jurisdictions WHERE country_code IS NOT NULL) THEN
        RAISE EXCEPTION 'jurisdictions.country_code holds operator-authored administrative metadata on at least one row; refusing to roll back migration 0076 while any such value exists. Manually clear it (SetJurisdictionCountryCode with an empty value) before retrying, or restore from backup instead of rolling back.';
    END IF;
END $$;

-- --- operating_country_policies ---

-- ADR 0045 §3.5-A AMENDMENT-3. A CONSTRAINT TRIGGER, dropped like any
-- other trigger (DROP TABLE below would cascade it away regardless; this
-- explicit drop mirrors this file's existing style of naming every
-- trigger/function it removes rather than relying on the cascade alone).
DROP TRIGGER IF EXISTS ocp_inherit_rung_close_requires_successor ON operating_country_policies;
DROP FUNCTION IF EXISTS operating_country_policies_enforce_close_successor();

DROP TRIGGER IF EXISTS operating_country_policies_ceiling ON operating_country_policies;
DROP FUNCTION IF EXISTS operating_country_policies_enforce_ceiling();

DROP TRIGGER IF EXISTS operating_country_policies_deny_truncate ON operating_country_policies;
DROP TRIGGER IF EXISTS operating_country_policies_immutable ON operating_country_policies;
DROP FUNCTION IF EXISTS operating_country_policies_enforce_append_only();

DROP TRIGGER IF EXISTS operating_country_policies_stamp_times ON operating_country_policies;
DROP FUNCTION IF EXISTS operating_country_policies_stamp_times();

DROP POLICY IF EXISTS operating_country_policies_tenant_close ON operating_country_policies;
DROP POLICY IF EXISTS operating_country_policies_tenant_insert ON operating_country_policies;
DROP POLICY IF EXISTS operating_country_policies_tenant_read ON operating_country_policies;

ALTER TABLE operating_country_policies NO FORCE ROW LEVEL SECURITY;
ALTER TABLE operating_country_policies DISABLE ROW LEVEL SECURITY;

DROP TABLE operating_country_policies;

-- --- licence_country_ceilings ---

DROP TRIGGER IF EXISTS licence_country_ceilings_deny_truncate ON licence_country_ceilings;
DROP TRIGGER IF EXISTS licence_country_ceilings_immutable ON licence_country_ceilings;
DROP FUNCTION IF EXISTS licence_country_ceilings_enforce_append_only();

DROP TRIGGER IF EXISTS licence_country_ceilings_stamp_times ON licence_country_ceilings;
DROP FUNCTION IF EXISTS licence_country_ceilings_stamp_times();

DROP POLICY IF EXISTS licence_country_ceilings_platform_close ON licence_country_ceilings;
DROP POLICY IF EXISTS licence_country_ceilings_platform_insert ON licence_country_ceilings;
DROP POLICY IF EXISTS licence_country_ceilings_read ON licence_country_ceilings;

ALTER TABLE licence_country_ceilings NO FORCE ROW LEVEL SECURITY;
ALTER TABLE licence_country_ceilings DISABLE ROW LEVEL SECURITY;

DROP TABLE licence_country_ceilings;

-- --- platform_operations ---

DROP POLICY IF EXISTS platform_operations_platform_admin_update ON platform_operations;
DROP POLICY IF EXISTS platform_operations_platform_admin_insert ON platform_operations;
DROP POLICY IF EXISTS platform_operations_read ON platform_operations;

ALTER TABLE platform_operations NO FORCE ROW LEVEL SECURITY;
ALTER TABLE platform_operations DISABLE ROW LEVEL SECURITY;

DROP TABLE platform_operations;

-- --- licences.permitted_markets comment ---

COMMENT ON COLUMN licences.permitted_markets IS NULL;

-- --- jurisdictions.country_code ---
-- Already confirmed above (this migration's own guard) that no row holds
-- a non-NULL value, so this DROP COLUMN destroys nothing.

ALTER TABLE jurisdictions DROP COLUMN IF EXISTS country_code;
