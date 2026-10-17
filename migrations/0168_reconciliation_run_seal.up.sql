-- F2 / SRF2-1, LFF2-1, SRF2-2, LFF2-2 (platform-freeze work list; orchestrator decision 2026-10-17): the payment_statement
-- RUN SEAL. FOR `security`, `ledger-finance` REVIEW.
--
-- NUMBERING: 0168 (reserved for F2/F3 by 9378c91; removed from reservedUnusedVersions in the same change).
--
-- Why: the payment_statement cursor and the M2 reconciliation-currency gate read reconciliation_runs, which the
-- runtime role may INSERT. F2a sealed the IMPORT a run names; a forged RUN row naming a genuine sealed import whose
-- own match never committed (manufacturable by holding the stream's advisory lock) still moved the cursor, and a
-- copied run row with run_at = now() made a failed / stale / truncated reconciliation look current. The run row is
-- now sealed by the application (HMAC under the B13 "b13-run-v1" subkey, payoutinstrument.Keys.SealPaymentRun) in
-- the match transaction's own INSERT, over (run id, tenant, provider, stream, run_import_id, the import's coverage
-- window, window_truncated, run_at). With keys the cursor and the gate trust ONLY run rows whose seal verifies and
-- whose import's seal verifies. The key never enters the database: the database only sees "sealed or not".
--
-- Purely additive: three nullable columns, all NULL for every existing row and every other stream, plus CHECKs and a
-- foreign key that ties run_import_id to an import of the SAME tenant. reconciliation_runs keeps FORCE RLS, its
-- tenant_isolation policy, the append-only trigger pair (0082) and the runtime role's table grants (rows are still
-- only INSERTed; a sealed row can never be edited, and an edit would break its seal anyway).

SET LOCAL lock_timeout = '5s';

ALTER TABLE reconciliation_runs
    ADD COLUMN run_import_id UUID,
    ADD COLUMN run_seal      TEXT,
    ADD COLUMN run_seal_kid  TEXT,
    ADD CONSTRAINT reconciliation_runs_run_seal_all_or_none CHECK (
        (run_seal IS NULL) = (run_seal_kid IS NULL) AND (run_seal IS NULL) = (run_import_id IS NULL)
    ),
    ADD CONSTRAINT reconciliation_runs_run_seal_shape CHECK (
        run_seal IS NULL
        OR (run_seal ~ '^[0-9a-f]{64}$' AND run_seal_kid ~ '^[A-Za-z0-9._-]{1,32}$'
            AND stream = 'payment_statement' AND provider_id IS NOT NULL AND coverage_end IS NOT NULL)
    ),
    ADD CONSTRAINT reconciliation_runs_run_import_fk FOREIGN KEY (run_import_id, tenant_id)
        REFERENCES payment_statement_imports (id, tenant_id);

COMMENT ON COLUMN reconciliation_runs.run_import_id IS
    'payment_statement runs sealed by the application (F2, 0168): the import this run matched; NULL otherwise.';
COMMENT ON COLUMN reconciliation_runs.run_seal IS
    'payment_statement runs (F2, 0168): hex HMAC of the run under the B13 run subkey; the cursor and the M2 gate trust only rows whose seal verifies (with keys).';
COMMENT ON COLUMN reconciliation_runs.run_seal_kid IS
    'payment_statement runs (F2, 0168): the master key id of run_seal.';

CREATE INDEX reconciliation_runs_payment_sealed_cursor
    ON reconciliation_runs (tenant_id, provider_id, coverage_end DESC)
    WHERE stream = 'payment_statement' AND run_seal IS NOT NULL;
