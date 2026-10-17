-- 0172: SRF2R closure (ledger-finance, 2026-10-17). Two independent, provider-independent hardenings.
--
-- 1. SRF2R-3 / residual 6.10 - the MA020 preventive guard fails closed.
--    payment_ref_cleared (0119 R-MA-3) cleared an open captured-unposted exposure on a ledger tombstone OR on an
--    ELIGIBLE completed deposit_reversal statement line. A statement line is written by the runtime role and its
--    import seal (0125) can only be verified in Go, so a SQL-only actor could plant an unsealed completed reversal
--    line and make 0115's ledger_adjustment_payload_refusal admit a manual credit (MA020) while the captured deposit
--    can still post or be refunded: a double credit. From this migration ONLY a ledger tombstone on
--    (tenant, provider, reference) clears in SQL. Ledger rows are written by the ledger posting path only, keyed and
--    idempotent (0115/0082 append-only), and a deposit reversal received through the provider's verified callback
--    for a never-posted original already writes exactly that tombstone (payments.postDepositReversalTombstone).
--
--    Intentional behaviour change (accepted operational cost, orchestrator decision 2026-10-17): a reversal seen ONLY
--    in a statement (its callback never arrived) no longer clears the SQL guard; manual credits for that player stay
--    blocked (MA020) until a tombstone exists. The keyed Go reconciliation still clears its detective standing
--    finding on a SEALED completed reversal line, so after this migration SQL-cleared is a SUBSET of Go-cleared
--    (TestMA020_Parity_* pin the subset). No reconciliation -> ledger write path is added.
--    The signature (uuid, text, text) RETURNS boolean, STABLE, the pinned search_path, NOT SECURITY DEFINER and the
--    owner are unchanged, so every caller (payment_attempt_open_exposure -> player_open_payment_exposure -> 0115's
--    refusal and adjustment/execute.go's audit read) is unchanged.
--
-- 2. SRF2R-1 - keyset indexes matching the streaming candidate reads exactly (the cursor read orders sealed runs by
--    (coverage_end, run_at, id) DESC; the M2 evidence read and the stall progress read by (run_at, id) DESC). With
--    them a run-row flood is read in index order, row by row, so the per-read scan budget (Go, CursorScanBudget /
--    EvidenceScanBudget) bounds the work itself, not only the rows handed to Go. The 0168 index
--    reconciliation_runs_payment_sealed_cursor is kept (it is a prefix of the first one; dropping it is not needed).
SET LOCAL lock_timeout = '5s';

CREATE OR REPLACE FUNCTION payment_ref_cleared(p_tenant uuid, p_provider text, p_ref text) RETURNS boolean AS $$
    SELECT p_ref IS NOT NULL AND p_ref <> '' AND
        EXISTS (SELECT 1 FROM ledger_transactions t
                 WHERE t.tenant_id = p_tenant AND t.provider_id = p_provider
                   AND t.transaction_type = 'tombstone' AND t.provider_tx_id = p_ref);
$$ LANGUAGE sql STABLE
    SET search_path = pg_catalog, public, pg_temp;

CREATE INDEX reconciliation_runs_payment_sealed_keyset
    ON reconciliation_runs (tenant_id, provider_id, coverage_end DESC, run_at DESC, id DESC)
    WHERE stream = 'payment_statement' AND run_seal IS NOT NULL;

CREATE INDEX reconciliation_runs_payment_sealed_by_run_at
    ON reconciliation_runs (tenant_id, provider_id, run_at DESC, id DESC)
    WHERE stream = 'payment_statement' AND run_seal IS NOT NULL;
