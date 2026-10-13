-- Reverses 0138 (ADR 0119 S3; DEV/STAGING ONLY, per the migrate-down policy).
--
-- Refuses (SF042), BEFORE dropping anything, while
--   - any alert of a staff.* Kind exists (a P1 / P2 security record is never dropped by a migration), or
--   - any LIVE MFA-assured staff session exists (assurance_seal set, not revoked, not past its absolute expiry):
--     dropping the assurance columns under a running S3 deployment would turn every staff refresh into a failure,
--     and running pre-S3 code against them would re-open password-only staff sessions.
-- FORCE ROW LEVEL SECURITY on sessions is lifted inside this transaction for the count only (the 0137 down
-- precedent) and restored; a refusal rolls the lift back.
--
-- NOT reversed: the revocation of the staff sessions that existed when 0138 ran (they stay revoked; their owners
-- log in again). Everything else returns to 0137 exactly (whole-schema snapshot test:
-- internal/httpserver/staff_mfa_migration_0138_integration_test.go).
SET LOCAL lock_timeout = '5s';

ALTER TABLE sessions NO FORCE ROW LEVEL SECURITY;
DO $$
DECLARE
    v_alerts   bigint;
    v_sessions bigint;
BEGIN
    SELECT pg_catalog.count(*) INTO v_sessions FROM public.sessions
     WHERE principal_type = 'staff' AND assurance_seal IS NOT NULL AND revoked_at IS NULL AND absolute_expires_at > now();
    -- alerts / alert_occurrences reference alert_kinds; count them through the owner's view of the table.
    ALTER TABLE alerts NO FORCE ROW LEVEL SECURITY;
    SELECT pg_catalog.count(*) INTO v_alerts FROM public.alerts WHERE kind LIKE 'staff.%';
    ALTER TABLE alerts FORCE ROW LEVEL SECURITY;
    IF v_alerts > 0 OR v_sessions > 0 THEN
        RAISE EXCEPTION 'migration 0138 down refused: staff security alerts or live MFA-assured staff sessions exist (alerts=%, live_sessions=%). Nothing was changed (ADR 0119 S3); on a local / dev database revoke the sessions or rebuild from migrations.',
            v_alerts, v_sessions USING ERRCODE = 'SF042';
    END IF;
END
$$;
ALTER TABLE sessions FORCE ROW LEVEL SECURITY;

-- 8. The alert Kinds (and the widened CHECK)
DO $$
BEGIN
    ALTER TABLE alert_kinds DISABLE TRIGGER alert_kinds_deny_update_delete;
    CREATE POLICY alert_kinds_unseed_0138 ON alert_kinds
        FOR DELETE TO CURRENT_USER USING (kind LIKE 'staff.%');
    DELETE FROM alert_kinds WHERE kind LIKE 'staff.%';
    DROP POLICY alert_kinds_unseed_0138 ON alert_kinds;
    ALTER TABLE alert_kinds ENABLE TRIGGER alert_kinds_deny_update_delete;
END
$$;
ALTER TABLE alert_kinds DROP CONSTRAINT alert_kinds_check1;
ALTER TABLE alert_kinds ADD CONSTRAINT alert_kinds_check1 CHECK (
    requires_subject = true
    OR kind IN ('alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed'));

-- 7. SF008
DROP TRIGGER sf_factor_lockout_clock ON staff_mfa_factors;
DROP FUNCTION sf_factor_lockout_clock();

-- 6. Grants: back to the table-level UPDATE, only where the up narrowed it.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime')
       AND NOT pg_catalog.has_table_privilege('igaming_runtime', 'public.sessions', 'UPDATE')
       AND pg_catalog.has_column_privilege('igaming_runtime', 'public.sessions', 'revoked_at', 'UPDATE') THEN
        EXECUTE 'REVOKE UPDATE (revoked_at, last_used_at, replaced_by_session_id) ON sessions FROM igaming_runtime';
        EXECUTE 'GRANT UPDATE ON sessions TO igaming_runtime';
    END IF;
END
$$;

-- 5. SF040
DROP TRIGGER sessions_staff_assurance_guard ON sessions;
DROP FUNCTION sessions_staff_assurance_guard();

-- 3. The cutover marker
DROP TABLE staff_session_cutover;
DROP FUNCTION sf_cutover_immutable();

-- 2. The assurance columns
ALTER TABLE sessions DROP CONSTRAINT sessions_assurance_shape;
ALTER TABLE sessions DROP CONSTRAINT sessions_assurance_staff_only;
ALTER TABLE sessions
    DROP COLUMN assurance_seal,
    DROP COLUMN assurance_kid,
    DROP COLUMN absolute_expires_at,
    DROP COLUMN mfa_at,
    DROP COLUMN auth_time,
    DROP COLUMN amr,
    DROP COLUMN aal;
