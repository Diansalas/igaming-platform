-- MUST RUN INSIDE A SINGLE TRANSACTION (db.MigrateDown does this). Run by hand, use
-- `psql --single-transaction -v ON_ERROR_STOP=1 -f <this file>`.
--
-- Reverses 0157: the DELETE sanction goes away and the 0110 triggers fire for DELETE again (the 0110 guard
-- functions were never modified, so there is nothing to restore in them). REFUSES (AR099) while
-- alert_retention_runs holds a row: it is the audit of data that was deleted, and dropping it would erase the
-- record of a purge. Purged alert history itself cannot be restored by any down.
ALTER TABLE alert_retention_runs NO FORCE ROW LEVEL SECURITY;
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM alert_retention_runs) THEN
        RAISE EXCEPTION '0157 down refused: alert_retention_runs holds the audit of past purges' USING ERRCODE = 'AR099';
    END IF;
END $$;
ALTER TABLE alert_retention_runs FORCE ROW LEVEL SECURITY;

DROP FUNCTION alerting_purge_resolved(INTERVAL, INT);

DROP POLICY alerts_retention_purge_delete ON alert_deliveries;
DROP POLICY alerts_retention_purge_delete ON alert_occurrences;
DROP POLICY alerts_retention_purge_delete ON alerts;

DROP TRIGGER alert_deliveries_retention_delete_guard ON alert_deliveries;
DROP TRIGGER alert_occurrences_retention_delete_guard ON alert_occurrences;
DROP TRIGGER alerts_retention_delete_guard ON alerts;
DROP FUNCTION alert_retention_delete_guard();

DROP TRIGGER alert_deliveries_guard ON alert_deliveries;
CREATE TRIGGER alert_deliveries_guard BEFORE INSERT OR UPDATE OR DELETE ON alert_deliveries FOR EACH ROW EXECUTE FUNCTION alert_deliveries_guard();
DROP TRIGGER alert_occurrences_guard ON alert_occurrences;
CREATE TRIGGER alert_occurrences_guard BEFORE INSERT OR UPDATE OR DELETE ON alert_occurrences FOR EACH ROW EXECUTE FUNCTION alert_occurrences_guard();
DROP TRIGGER alerts_guard ON alerts;
CREATE TRIGGER alerts_guard BEFORE INSERT OR UPDATE OR DELETE ON alerts FOR EACH ROW EXECUTE FUNCTION alerts_guard();

DROP TABLE alert_retention_runs;
DROP FUNCTION alert_retention_runs_guard();
