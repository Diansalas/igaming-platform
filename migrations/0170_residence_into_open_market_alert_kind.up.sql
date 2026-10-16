-- F5(b) (docs/plans/f5-f6-record.md; SRME-1 follow-up in docs/plans/market-evidence-kyc-record.md).
--
-- Seeds ONE alert Kind, kyc.residence_into_open_market: a reviewer recorded a KYC-verified residence in a country the
-- brand currently serves. The residence is the evidence that admits a player to money and games and there is no
-- maker-checker on it (an OWNER decision, not built), so the determination must be a durable, reviewable signal and not
-- only an audit row. Severity p2 is an ENGINEERING DEFAULT (alert delivery to a person is an owner configuration, see
-- docs/runbooks/alert-delivery-channels.md). The payload allows ids only: never the country. The alert is raised in the
-- SAME transaction as the review and the market.residence_into_open_market audit row (in_tx), so a determination
-- cannot commit without it.
--
-- Seeded under alert_kinds' FORCE RLS through a temporary literal single-Kind FOR INSERT policy created and dropped in one
-- DO block (the 0114 / 0141 pattern). No table, column, grant or function changes.

SET LOCAL lock_timeout = '5s';

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM public.alert_kinds WHERE kind = 'kyc.residence_into_open_market') THEN
        RAISE EXCEPTION 'migration 0170 pre-flight: alert kind kyc.residence_into_open_market already exists';
    END IF;
    CREATE POLICY alert_kinds_seed_0170 ON alert_kinds
        FOR INSERT TO CURRENT_USER
        WITH CHECK (kind = 'kyc.residence_into_open_market');
    INSERT INTO alert_kinds (kind, severity, scope, simulation, requires_subject,
                             in_tx_raisable_by_tenant, allowed_keys, raise_mode)
    VALUES ('kyc.residence_into_open_market', 'p2', 'platform', false, true, true,
            ARRAY['verification_id', 'brand_id', 'request_id'], 'in_tx');
    DROP POLICY alert_kinds_seed_0170 ON alert_kinds;
END
$$;
