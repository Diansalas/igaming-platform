-- Reverses migration 0075, but REFUSES while jurisdiction_precedence_configs
-- holds any rows. That table is append-only and may hold real
-- policy-authoring history and provenance (every CreateEvaluationPolicyVersion
-- call - the sanctioned Go write path - inserts a row; the integration
-- tests exercise that exact path and leave rows behind), so silently
-- DROPping the columns that hold it would destroy audit-relevant history,
-- and reapplying up.sql afterward would fail with a NOT NULL violation on
-- the added NOT-NULL, no-DEFAULT columns the instant any row exists. This
-- mirrors this repository's own established precedent for a destructive
-- rollback on a table that may hold real data - migrations 0048 and 0052
-- both RAISE EXCEPTION and refuse to roll back rather than silently
-- destroying data - rather than migration 0075's own original (incorrect)
-- claim that the table is always empty.
--
-- This migration therefore refuses outright whenever the table is
-- non-empty; it only proceeds with the drop logic below on a genuinely
-- empty table.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM jurisdiction_precedence_configs) THEN
        RAISE EXCEPTION 'jurisdiction_precedence_configs is append-only and may hold policy-authoring history; refusing to roll back migration 0075 while rows exist. Manually verify no history must be preserved, then delete all rows (this destroys audit-relevant provenance) before retrying, or restore from backup instead of rolling back.';
    END IF;
END $$;

DROP TRIGGER IF EXISTS jurisdiction_precedence_configs_deny_truncate ON jurisdiction_precedence_configs;
DROP TRIGGER IF EXISTS jurisdiction_precedence_configs_immutable ON jurisdiction_precedence_configs;
DROP FUNCTION IF EXISTS jurisdiction_precedence_configs_enforce_append_only();

DROP TRIGGER IF EXISTS jurisdiction_precedence_configs_stamp_times ON jurisdiction_precedence_configs;
DROP FUNCTION IF EXISTS jurisdiction_precedence_configs_stamp_times();

DROP POLICY IF EXISTS jurisdiction_precedence_configs_platform_close ON jurisdiction_precedence_configs;
DROP POLICY IF EXISTS jurisdiction_precedence_configs_platform_insert ON jurisdiction_precedence_configs;
DROP POLICY IF EXISTS jurisdiction_precedence_configs_read ON jurisdiction_precedence_configs;

-- NO FORCE before DISABLE, so the table returns to its exact pre-0075
-- posture (relrowsecurity=false, relforcerowsecurity=false) rather than
-- leaving FORCE set on a table that no longer has ROW LEVEL SECURITY
-- enabled at all.
ALTER TABLE jurisdiction_precedence_configs NO FORCE ROW LEVEL SECURITY;
ALTER TABLE jurisdiction_precedence_configs DISABLE ROW LEVEL SECURITY;

DROP INDEX IF EXISTS idx_jurisdiction_precedence_configs_lookup;
DROP INDEX IF EXISTS uq_jurisdiction_precedence_configs_open;

ALTER TABLE jurisdiction_precedence_configs
    DROP CONSTRAINT IF EXISTS jurisdiction_precedence_configs_active_requires_legal_review,
    DROP CONSTRAINT IF EXISTS jurisdiction_precedence_configs_withdrawn_no_content,
    DROP CONSTRAINT IF EXISTS jurisdiction_precedence_configs_precedence_status_matches,
    DROP CONSTRAINT IF EXISTS jurisdiction_precedence_configs_effective_to_after_from;

ALTER TABLE jurisdiction_precedence_configs
    DROP COLUMN IF EXISTS reason_code,
    DROP COLUMN IF EXISTS legal_review_reference,
    DROP COLUMN IF EXISTS precedence_policy_version,
    DROP COLUMN IF EXISTS precedence_status,
    DROP COLUMN IF EXISTS max_location_signal_age_seconds,
    DROP COLUMN IF EXISTS location_requirement,
    DROP COLUMN IF EXISTS status;

COMMENT ON TABLE jurisdiction_precedence_configs IS 'SHAPE ONLY - no rows in Stage 4I. Keyed on (tenant licensing jurisdiction, operation_class), NOT tenant_id, per RISK Sec 4.2''s bootstrap-circularity finding (canonical-model Sec 3.4). Platform-wide reference configuration, no RLS - same posture as jurisdictions/licences. Content is blocked on HDR-J-2.';
