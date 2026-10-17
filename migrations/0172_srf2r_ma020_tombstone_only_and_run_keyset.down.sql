-- Reverses 0172 (DEV/STAGING ONLY, per the migrate-down policy): restores the 0119 body of payment_ref_cleared
-- (a tombstone OR an eligible completed deposit_reversal statement line clears; R-MA-3 / C3) and drops the two
-- keyset indexes. Nothing is stored by 0172, so nothing is lost.
SET LOCAL lock_timeout = '5s';

DROP INDEX reconciliation_runs_payment_sealed_by_run_at;
DROP INDEX reconciliation_runs_payment_sealed_keyset;

CREATE OR REPLACE FUNCTION payment_ref_cleared(p_tenant uuid, p_provider text, p_ref text) RETURNS boolean AS $$
    SELECT p_ref IS NOT NULL AND p_ref <> '' AND (
        EXISTS (SELECT 1 FROM ledger_transactions t
                 WHERE t.tenant_id = p_tenant AND t.provider_id = p_provider
                   AND t.transaction_type = 'tombstone' AND t.provider_tx_id = p_ref)
        OR EXISTS (SELECT 1 FROM payment_statement_lines l
                     JOIN payment_statement_imports i ON i.id = l.import_id AND i.tenant_id = l.tenant_id
                    WHERE l.tenant_id = p_tenant AND l.provider_id = p_provider
                      AND l.kind = 'deposit_reversal' AND l.original_provider_reference = p_ref
                      AND l.status IN ('succeeded', 'reversed')                       -- C3: completed only
                      AND (NOT i.is_mock
                           OR NOT EXISTS (SELECT 1 FROM payment_statement_imports r
                                           WHERE r.tenant_id = p_tenant AND r.provider_id = p_provider AND NOT r.is_mock))));
$$ LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp;
