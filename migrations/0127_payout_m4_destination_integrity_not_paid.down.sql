-- MUST RUN INSIDE A SINGLE TRANSACTION (db.MigrateDown does this). By hand, use
-- `psql --single-transaction -v ON_ERROR_STOP=1 -f <this file>`: an MR099 refusal
-- below would otherwise leave FORCE ROW LEVEL SECURITY lifted on the checked table.
--
-- Reverses 0127 (GOV-R32, ADR 0111 section 23). REFUSES (MR099) while ANY M4 row
-- (any kind, any state) exists whose pinned dispute reason is
-- destination_integrity_failure: under the restored 0125 scope such a row would
-- be outside M4 (an executed one would describe a release the scope no longer
-- admits; a pending one would be stranded). Otherwise it restores the 0125 body of
-- payment_m4_in_scope BYTE FOR BYTE (the whole-schema snapshot test verifies it).

ALTER TABLE payment_manual_resolutions NO FORCE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM payment_manual_resolutions
                WHERE kind IN ('m4_evidence_paid', 'm4_evidence_not_paid')
                  AND terminal_reason_at_submission = 'destination_integrity_failure') THEN
        RAISE EXCEPTION '0127 down refused: M4 resolution rows on a destination_integrity_failure park exist' USING ERRCODE = 'MR099';
    END IF;
END $$;

ALTER TABLE payment_manual_resolutions FORCE ROW LEVEL SECURITY;

-- Section 4.1 / A-12 / C-6 / M-10: a disputed payout whose reason is
-- invalid_provider_reference, invalid_provider_reference:* or
-- provider_reference_conflict WITH NO provider reference (both kinds), or
-- destination_mismatch (not-paid only). Anything else is outside M4.
CREATE OR REPLACE FUNCTION payment_m4_in_scope(p_kind text, p_state text, p_reason text, p_ref text) RETURNS boolean AS $$
    SELECT COALESCE(p_kind IN ('m4_evidence_paid', 'm4_evidence_not_paid')
        AND p_state = 'disputed'
        AND ((p_ref IS NULL
              AND (p_reason = 'invalid_provider_reference'
                   OR starts_with(p_reason, 'invalid_provider_reference:')
                   OR p_reason = 'provider_reference_conflict'))
             OR (p_kind = 'm4_evidence_not_paid' AND p_reason = 'destination_mismatch')), false);
$$ LANGUAGE sql IMMUTABLE
    SET search_path = pg_catalog, public, pg_temp;
