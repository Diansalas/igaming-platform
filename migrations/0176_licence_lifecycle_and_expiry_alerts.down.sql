-- Down of 0176 (DEV/STAGING ONLY). REFUSES (LV099) while any licence has MORE THAN ONE licence_versions row, that is, while
-- any recorded change history would be lost (history is never deleted by a down). A licence with exactly one version (its
-- baseline or creation snapshot) loses nothing: that snapshot is the licence row itself. Also REFUSES while any alert /
-- occurrence of licence.expired or licence.expiring exists.
-- Otherwise drops exactly what the up created and restores the 0156 alert_kinds CHECK and dispatcher policies.
SET LOCAL lock_timeout = '5s';

ALTER TABLE licence_versions NO FORCE ROW LEVEL SECURITY;
ALTER TABLE alerts NO FORCE ROW LEVEL SECURITY;
ALTER TABLE alert_occurrences NO FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM licence_versions GROUP BY licence_id HAVING count(*) > 1) THEN
        RAISE EXCEPTION 'migration 0176 down refused: licence_versions history exists (a licence has more than one version)' USING ERRCODE = 'LV099';
    END IF;
    IF EXISTS (SELECT 1 FROM alerts WHERE kind IN ('licence.expired', 'licence.expiring'))
       OR EXISTS (SELECT 1 FROM alert_occurrences WHERE kind IN ('licence.expired', 'licence.expiring')) THEN
        RAISE EXCEPTION 'migration 0176 down refused: alerts of kind licence.expired / licence.expiring exist' USING ERRCODE = 'LV099';
    END IF;
END
$$;
ALTER TABLE alerts FORCE ROW LEVEL SECURITY;
ALTER TABLE alert_occurrences FORCE ROW LEVEL SECURITY;

DROP POLICY alerts_licence_sweep_dispatcher_only ON alert_occurrences;
DROP POLICY alerts_licence_sweep_dispatcher_only ON alerts;

DROP POLICY alerts_platform_service_dispatcher_raise ON alert_occurrences;
CREATE POLICY alerts_platform_service_dispatcher_raise ON alert_occurrences
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
        AND tenant_id IS NULL
        AND subject_tenant_id IS NULL
        AND kind IN ('alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed', 'reconciliation.sweep_failed')
    );
DROP POLICY alerts_platform_service_dispatcher_raise ON alerts;
CREATE POLICY alerts_platform_service_dispatcher_raise ON alerts
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
        AND tenant_id IS NULL
        AND subject_tenant_id IS NULL
        AND kind IN ('alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed', 'reconciliation.sweep_failed')
    );

DO $$
BEGIN
    ALTER TABLE alert_kinds DISABLE TRIGGER alert_kinds_deny_update_delete;
    CREATE POLICY alert_kinds_unseed_0176 ON alert_kinds
        FOR DELETE TO CURRENT_USER USING (kind IN ('licence.expired', 'licence.expiring'));
    DELETE FROM alert_kinds WHERE kind IN ('licence.expired', 'licence.expiring');
    DROP POLICY alert_kinds_unseed_0176 ON alert_kinds;
    ALTER TABLE alert_kinds ENABLE TRIGGER alert_kinds_deny_update_delete;
END
$$;
ALTER TABLE alert_kinds DROP CONSTRAINT alert_kinds_check1;
ALTER TABLE alert_kinds ADD CONSTRAINT alert_kinds_check1 CHECK (
    requires_subject = true
    OR kind IN ('alerting.unrouted', 'alerting.delivery_dead', 'alerting.raise_failed',
                'staff.platform_mfa_integrity', 'staff.platform_session_integrity', 'staff.platform_mfa_hard_locked',
                'staff.platform_mfa_recovery_used', 'staff.platform_mfa_failure_pattern',
                'staff.platform_break_glass_used',
                'reconciliation.sweep_failed'));

DROP TRIGGER licences_write_version ON licences;
DROP FUNCTION licences_write_version();
DROP TABLE licence_versions;
DROP FUNCTION licence_versions_deny_mutation();
DROP TRIGGER licences_touch_updated_at ON licences;
DROP FUNCTION licences_touch_updated_at();
ALTER TABLE licences DROP CONSTRAINT licences_evidence_reference_shape;
ALTER TABLE licences DROP COLUMN updated_at;
ALTER TABLE licences DROP COLUMN evidence_reference;
ALTER TABLE licences DROP COLUMN review_due_at;
