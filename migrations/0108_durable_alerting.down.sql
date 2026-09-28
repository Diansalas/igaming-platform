-- Down migration for 0108_durable_alerting (ADR 0102 §9 item 7): refuses
-- while any row exists in alerts, alert_occurrences, alert_deliveries or
-- alert_routes - alert history is never silently dropped by a rollback.
-- alert_kinds is pure seed vocabulary and is always safe to drop.
--
-- All four tables carry FORCE ROW LEVEL SECURITY, which - per this
-- repository's own established precedent (migration 0100's down file,
-- same reasoning) - means even the migration role (owner, never granted
-- BYPASSRLS per deploy/init-app-role.sql) is subject to every policy. A
-- migration-time guard with no app.tenant_id/app.platform_admin_
-- principal_id/app.platform_service_id GUC set would therefore see ZERO
-- rows from every one of these tables' permissive policies regardless of
-- what they actually hold, making the guard below a silent no-op. DISABLE
-- ROW LEVEL SECURITY for the duration of this transactional DDL is safe
-- for the identical reason migration 0100 documents: if the guard raises,
-- the whole transaction (including these ALTERs) rolls back and FORCE RLS
-- is restored exactly as it was; if the guard does not fire, every one of
-- these tables is dropped immediately afterward anyway, so its RLS
-- posture stops mattering.
ALTER TABLE alerts DISABLE ROW LEVEL SECURITY;
ALTER TABLE alert_occurrences DISABLE ROW LEVEL SECURITY;
ALTER TABLE alert_deliveries DISABLE ROW LEVEL SECURITY;
ALTER TABLE alert_routes DISABLE ROW LEVEL SECURITY;

DO $$
DECLARE
    v_count BIGINT;
BEGIN
    SELECT count(*) INTO v_count FROM alerts;
    IF v_count > 0 THEN
        RAISE EXCEPTION 'migration 0108 down: refusing - % row(s) exist in alerts', v_count;
    END IF;
    SELECT count(*) INTO v_count FROM alert_occurrences;
    IF v_count > 0 THEN
        RAISE EXCEPTION 'migration 0108 down: refusing - % row(s) exist in alert_occurrences', v_count;
    END IF;
    SELECT count(*) INTO v_count FROM alert_deliveries;
    IF v_count > 0 THEN
        RAISE EXCEPTION 'migration 0108 down: refusing - % row(s) exist in alert_deliveries', v_count;
    END IF;
    SELECT count(*) INTO v_count FROM alert_routes;
    IF v_count > 0 THEN
        RAISE EXCEPTION 'migration 0108 down: refusing - % row(s) exist in alert_routes', v_count;
    END IF;
END
$$;

DROP TABLE alert_deliveries;
DROP TABLE alert_routes;
DROP TABLE alert_occurrences;
DROP TABLE alerts;
DROP TABLE alert_kinds;

DROP FUNCTION alert_deliveries_guard();
DROP FUNCTION alert_routes_guard();
DROP FUNCTION alert_occurrences_guard();
DROP FUNCTION alerts_guard();
DROP FUNCTION alert_kinds_deny_write();
DROP FUNCTION alerting_attributes_are_flat_scalars(JSONB);
DROP FUNCTION alerting_session_scope();
DROP FUNCTION alerting_validated_platform_admin();
