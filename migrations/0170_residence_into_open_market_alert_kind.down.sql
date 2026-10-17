-- Reverses 0170: removes the alert Kind kyc.residence_into_open_market. REFUSES (the alerts.kind foreign key) while any
-- alert of that Kind exists: alert history is never deleted by a down migration.
DO $$
BEGIN
    ALTER TABLE alert_kinds DISABLE TRIGGER alert_kinds_deny_update_delete;
    CREATE POLICY alert_kinds_unseed_0170 ON alert_kinds
        FOR DELETE TO CURRENT_USER USING (kind = 'kyc.residence_into_open_market');
    DELETE FROM alert_kinds WHERE kind = 'kyc.residence_into_open_market';
    DROP POLICY alert_kinds_unseed_0170 ON alert_kinds;
    ALTER TABLE alert_kinds ENABLE TRIGGER alert_kinds_deny_update_delete;
EXCEPTION WHEN foreign_key_violation THEN
    RAISE EXCEPTION 'migration 0170 down refused: alerts of kind kyc.residence_into_open_market exist';
END
$$;
