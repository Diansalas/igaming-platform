-- ALERT-RETENTION-1 (A9): bounded retention for the durable alert tables.
--
-- alerts, alert_occurrences and alert_deliveries (0110) are append-only: their guard triggers refuse every
-- DELETE and TRUNCATE, so they grow without bound. This migration adds ONE sanctioned deletion path and
-- changes nothing else about their append-only behaviour.
--
-- DESIGN (chosen over time-partitioning: converting three populated, FK-linked, FORCE-RLS tables to
-- partitioned tables would rewrite the alert keys and the dedup unique index and gains nothing the purge below
-- does not already give; registry text of ALERT-RETENTION-1 named partition detach as one example, not a
-- requirement):
--
--   * Only RESOLVED alerts older than a caller-supplied window (floor 7 days, enforced here independently of the
--     application's own floor) are ever deleted, together with their occurrences and deliveries. Open and
--     acknowledged alerts are never pruned, whatever their age.
--   * The only code path is alerting_purge_resolved(), SECURITY DEFINER, owned by the table owner. The runtime
--     role keeps NO DELETE privilege on any alert table (0110 grants stay untouched); it can only EXECUTE this
--     function, and the function refuses any session that is not exactly the alert_dispatcher platform-service
--     scope (alerting_session_scope()).
--   * The append-only guard triggers are split: INSERT/UPDATE stay with the 0110 guard functions unchanged;
--     DELETE gets its own BEFORE DELETE trigger that refuses unless (a) the function set the transaction-local
--     app.alert_retention_purge flag, (b) the session user inside the function is the table OWNER (a plain
--     runtime-role session can set the flag but is never the owner), and (c) the row is itself eligible
--     (resolved alert older than the 7-day floor; an occurrence or delivery whose parent alert is). TRUNCATE
--     stays refused by the unchanged 0110 statement triggers.
--   * FORCE RLS is kept on all tables. The only RLS additions are DELETE policies (and one INSERT policy on the
--     audit table) that require the alert_dispatcher service identity AND the transaction-local flag: no SELECT
--     policy is added anywhere (the purge reads through the existing 0110 dispatcher SELECT policies), and no
--     tenant, platform-admin or dispatcher policy is widened. These policies are GUC-based like every other alert
--     RLS family and deliberately do NOT depend on the evaluating role (the KYC worker catalogue gate requires
--     every policy to be role-independent); the role-aware layers are the missing DELETE privilege of the runtime
--     role and the owner check in the delete trigger.
--   * Every run is recorded in the new append-only alert_retention_runs (cutoff, counts, capped flag): the audit
--     of what was removed. It holds counts only, never alert content.
--   * The retention HORIZON is configuration (ALERT_RETENTION_DAYS), not a code constant. The default is
--     "no purge job"; choosing the horizon is a legal/compliance decision (CLAUDE.md: jurisdiction-specific
--     retention) that this migration does not make.
--
-- Residual (documented, not hidden): the session scope is asserted by GUC like every other alert RLS family, so
-- a compromised application process could invoke the function; it can then remove only resolved alerts older
-- than the floor, and every call is audited in alert_retention_runs. Occurrences of a long-lived OPEN alert are
-- retained until it is resolved.

-- ======================================================================
-- 1. alert_retention_runs - append-only purge audit
-- ======================================================================
CREATE TABLE alert_retention_runs (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    ran_at               TIMESTAMPTZ NOT NULL DEFAULT now(),
    older_than           INTERVAL NOT NULL CHECK (older_than >= INTERVAL '7 days'),
    cutoff               TIMESTAMPTZ NOT NULL,
    batch_limit          INT NOT NULL CHECK (batch_limit BETWEEN 1 AND 50000),
    alerts_deleted       BIGINT NOT NULL CHECK (alerts_deleted >= 0),
    occurrences_deleted  BIGINT NOT NULL CHECK (occurrences_deleted >= 0),
    deliveries_deleted   BIGINT NOT NULL CHECK (deliveries_deleted >= 0),
    capped               BOOLEAN NOT NULL
);
COMMENT ON TABLE alert_retention_runs IS 'ALERT-RETENTION-1: append-only audit of alerting_purge_resolved() runs (counts only; no alert content, no tenant ids).';

CREATE FUNCTION alert_retention_runs_guard() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        RAISE EXCEPTION 'alert_retention_runs is append-only: TRUNCATE is not permitted';
    END IF;
    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        RAISE EXCEPTION 'alert_retention_runs is append-only: % is not permitted', TG_OP;
    END IF;
    NEW.ran_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;
CREATE TRIGGER alert_retention_runs_guard
    BEFORE INSERT OR UPDATE OR DELETE ON alert_retention_runs
    FOR EACH ROW EXECUTE FUNCTION alert_retention_runs_guard();
CREATE TRIGGER alert_retention_runs_deny_truncate
    BEFORE TRUNCATE ON alert_retention_runs
    FOR EACH STATEMENT EXECUTE FUNCTION alert_retention_runs_guard();

ALTER TABLE alert_retention_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE alert_retention_runs FORCE ROW LEVEL SECURITY;

-- Platform admins read the audit; the dispatcher identity reads it (the loop reports the last run).
CREATE POLICY alert_retention_runs_platform_admin_read ON alert_retention_runs
    FOR SELECT
    USING (
        NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
        AND EXISTS (SELECT 1 FROM staff_users su WHERE su.id = NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid AND su.tenant_id IS NULL)
    );
CREATE POLICY alert_retention_runs_dispatcher_read ON alert_retention_runs
    FOR SELECT
    USING (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    );
-- The purge function's own INSERT (it runs as the table owner inside a dispatcher-identity session with the flag set).
-- The runtime role has no INSERT privilege on this table (section 5), so only the SECURITY DEFINER function can write it.
CREATE POLICY alert_retention_runs_purge_insert ON alert_retention_runs
    FOR INSERT
    WITH CHECK (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
        AND NULLIF(current_setting('app.alert_retention_purge', true), '') = 'on'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    );

-- ======================================================================
-- 2. Split the DELETE out of the 0110 guard triggers
-- ======================================================================
-- The 0110 guard functions are NOT modified. Their DELETE branch becomes unreachable because the triggers below
-- no longer fire them for DELETE; the dedicated trigger refuses every DELETE except the sanctioned one.
DROP TRIGGER alerts_guard ON alerts;
CREATE TRIGGER alerts_guard BEFORE INSERT OR UPDATE ON alerts FOR EACH ROW EXECUTE FUNCTION alerts_guard();
DROP TRIGGER alert_occurrences_guard ON alert_occurrences;
CREATE TRIGGER alert_occurrences_guard BEFORE INSERT OR UPDATE ON alert_occurrences FOR EACH ROW EXECUTE FUNCTION alert_occurrences_guard();
DROP TRIGGER alert_deliveries_guard ON alert_deliveries;
CREATE TRIGGER alert_deliveries_guard BEFORE INSERT OR UPDATE ON alert_deliveries FOR EACH ROW EXECUTE FUNCTION alert_deliveries_guard();

CREATE FUNCTION alert_retention_delete_guard() RETURNS TRIGGER AS $$
DECLARE
    v_owner TEXT;
    v_ok    BOOLEAN;
BEGIN
    SELECT pg_catalog.pg_get_userbyid(c.relowner) INTO v_owner FROM pg_catalog.pg_class c WHERE c.oid = TG_RELID;
    IF NULLIF(current_setting('app.alert_retention_purge', true), '') IS DISTINCT FROM 'on'
       OR current_user::text IS DISTINCT FROM v_owner THEN
        RAISE EXCEPTION '% is append-only: DELETE is permitted only through alerting_purge_resolved()', TG_TABLE_NAME
            USING ERRCODE = 'AR201';
    END IF;
    IF TG_TABLE_NAME = 'alerts' THEN
        v_ok := OLD.state = 'resolved' AND OLD.resolved_at IS NOT NULL AND OLD.resolved_at < now() - INTERVAL '7 days';
    ELSE
        -- alert_occurrences / alert_deliveries: the parent alert must itself be an eligible resolved alert.
        EXECUTE 'SELECT EXISTS (SELECT 1 FROM public.alerts a WHERE a.id = $1 AND a.state = ''resolved'' AND a.resolved_at < now() - INTERVAL ''7 days'')'
            INTO v_ok USING OLD.alert_id;
    END IF;
    IF NOT COALESCE(v_ok, false) THEN
        RAISE EXCEPTION '% row is not eligible for retention deletion (only resolved alerts older than 7 days)', TG_TABLE_NAME
            USING ERRCODE = 'AR201';
    END IF;
    RETURN OLD;
END;
$$ LANGUAGE plpgsql
    SET search_path = pg_catalog, public, pg_temp;

CREATE TRIGGER alerts_retention_delete_guard BEFORE DELETE ON alerts FOR EACH ROW EXECUTE FUNCTION alert_retention_delete_guard();
CREATE TRIGGER alert_occurrences_retention_delete_guard BEFORE DELETE ON alert_occurrences FOR EACH ROW EXECUTE FUNCTION alert_retention_delete_guard();
CREATE TRIGGER alert_deliveries_retention_delete_guard BEFORE DELETE ON alert_deliveries FOR EACH ROW EXECUTE FUNCTION alert_retention_delete_guard();

-- ======================================================================
-- 3. DELETE policies for the purge (no SELECT policy is added: reads use the existing 0110 dispatcher SELECT policies)
-- ======================================================================
CREATE POLICY alerts_retention_purge_delete ON alerts
    FOR DELETE
    USING (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
        AND NULLIF(current_setting('app.alert_retention_purge', true), '') = 'on'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
        AND state = 'resolved'
    );
CREATE POLICY alerts_retention_purge_delete ON alert_occurrences
    FOR DELETE
    USING (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
        AND NULLIF(current_setting('app.alert_retention_purge', true), '') = 'on'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    );
CREATE POLICY alerts_retention_purge_delete ON alert_deliveries
    FOR DELETE
    USING (
        NULLIF(current_setting('app.platform_service_id', true), '') = 'alert_dispatcher'
        AND NULLIF(current_setting('app.alert_retention_purge', true), '') = 'on'
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_tenant_id', true), '') IS NULL
        AND NULLIF(current_setting('app.acting_platform_principal_id', true), '') IS NULL
    );

-- ======================================================================
-- 4. The purge function
-- ======================================================================
CREATE FUNCTION alerting_purge_resolved(p_older_than INTERVAL, p_batch INT)
RETURNS TABLE (alerts_deleted BIGINT, occurrences_deleted BIGINT, deliveries_deleted BIGINT, capped BOOLEAN)
LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path = pg_catalog, public, pg_temp
AS $$
DECLARE
    v_cutoff TIMESTAMPTZ;
    v_ids    UUID[];
    v_a      BIGINT := 0;
    v_o      BIGINT := 0;
    v_d      BIGINT := 0;
    v_capped BOOLEAN := false;
BEGIN
    IF alerting_session_scope() IS DISTINCT FROM 'platform_service' THEN
        RAISE EXCEPTION 'alerting_purge_resolved: only the alert_dispatcher platform-service session may run retention'
            USING ERRCODE = 'AR202';
    END IF;
    IF p_older_than IS NULL OR p_older_than < INTERVAL '7 days' THEN
        RAISE EXCEPTION 'alerting_purge_resolved: the retention window must be at least 7 days'
            USING ERRCODE = 'AR203';
    END IF;
    IF p_batch IS NULL OR p_batch < 1 OR p_batch > 50000 THEN
        RAISE EXCEPTION 'alerting_purge_resolved: batch must be between 1 and 50000'
            USING ERRCODE = 'AR203';
    END IF;
    v_cutoff := now() - p_older_than;

    PERFORM set_config('app.alert_retention_purge', 'on', true);

    SELECT COALESCE(array_agg(s.id), ARRAY[]::UUID[]) INTO v_ids
      FROM (SELECT a.id FROM alerts a
             WHERE a.state = 'resolved' AND a.resolved_at < v_cutoff
             ORDER BY a.resolved_at, a.id
             LIMIT p_batch) s;
    v_capped := COALESCE(array_length(v_ids, 1), 0) = p_batch;

    IF COALESCE(array_length(v_ids, 1), 0) > 0 THEN
        DELETE FROM alert_deliveries WHERE alert_id = ANY (v_ids);
        GET DIAGNOSTICS v_d = ROW_COUNT;
        DELETE FROM alert_occurrences WHERE alert_id = ANY (v_ids);
        GET DIAGNOSTICS v_o = ROW_COUNT;
        DELETE FROM alerts WHERE id = ANY (v_ids);
        GET DIAGNOSTICS v_a = ROW_COUNT;
    END IF;

    INSERT INTO alert_retention_runs (older_than, cutoff, batch_limit, alerts_deleted, occurrences_deleted, deliveries_deleted, capped)
    VALUES (p_older_than, v_cutoff, p_batch, v_a, v_o, v_d, v_capped);

    PERFORM set_config('app.alert_retention_purge', 'off', true);
    RETURN QUERY SELECT v_a, v_o, v_d, v_capped;
END;
$$;
COMMENT ON FUNCTION alerting_purge_resolved(INTERVAL, INT) IS 'ALERT-RETENTION-1: the only sanctioned deletion of alert history. Resolved alerts older than the window (>= 7 days), with their occurrences and deliveries; dispatcher-identity sessions only; audited in alert_retention_runs.';

-- ======================================================================
-- 5. Grants
-- ======================================================================
REVOKE ALL ON FUNCTION alerting_purge_resolved(INTERVAL, INT) FROM PUBLIC;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'igaming_runtime') THEN
        EXECUTE 'REVOKE ALL ON alert_retention_runs FROM igaming_runtime';
        EXECUTE 'GRANT SELECT ON alert_retention_runs TO igaming_runtime';
        EXECUTE 'GRANT EXECUTE ON FUNCTION alerting_purge_resolved(INTERVAL, INT) TO igaming_runtime';
        -- The runtime role must still have NO deletion privilege on the alert tables (0110 grants).
        EXECUTE 'REVOKE DELETE, TRUNCATE ON alerts, alert_occurrences, alert_deliveries, alert_routes, alert_kinds FROM igaming_runtime';
    END IF;
END $$;
