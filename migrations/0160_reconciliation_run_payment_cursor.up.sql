-- A11 / R3-OBS-BOUND-1: a PERSISTED, per-(tenant, provider) continuation cursor for the payment_statement
-- stream, so a bounded run (a per-run line cap) is followed by a run that continues exactly where it stopped:
-- no statement window skipped, none counted twice.
--
-- The cursor is the validated coverage_end of the last payment_statement run that COMMITTED for the
-- (tenant, provider). A run commits only after its match transaction succeeded (a failed fetch, ingest or
-- match writes no run row), so the cursor can never advance past a window that was not matched. It is
-- written in the SAME insert as the run itself, under the stream's per-tenant advisory lock, exactly like
-- casino_statement's anchor (0152) - not read from audit rows.
--
-- Purely additive: two nullable columns plus one partial index. NULL for every existing run and for every
-- stream other than payment_statement. reconciliation_runs keeps everything it had: FORCE ROW LEVEL
-- SECURITY and the tenant_isolation policy (0027/0028), the total append-only trigger pair
-- reconciliation_runs_immutable / _no_truncate (0082; rows are still only ever INSERTed) and the runtime
-- role's table-level grants (no column-level privilege is introduced). coverage_end (0152) is now also
-- written by payment_statement runs.
SET LOCAL lock_timeout = '5s';

ALTER TABLE reconciliation_runs
    ADD COLUMN provider_id       TEXT,
    ADD COLUMN window_truncated  BOOLEAN,
    ADD CONSTRAINT reconciliation_runs_provider_id_shape CHECK (
        provider_id IS NULL
        OR (octet_length(provider_id) BETWEEN 1 AND 255 AND provider_id !~ '[\x01-\x1F\x7F-\x9F]')
    ),
    ADD CONSTRAINT reconciliation_runs_truncated_needs_provider CHECK (
        window_truncated IS NULL OR provider_id IS NOT NULL
    );

COMMENT ON COLUMN reconciliation_runs.provider_id IS
    'payment_statement runs only: the provider whose statement this run matched (the per-provider cursor key); NULL for every other stream.';
COMMENT ON COLUMN reconciliation_runs.window_truncated IS
    'payment_statement runs only: true when the source stopped at the per-run bound and the run covers less than the requested window (the next run continues from coverage_end); NULL for every other stream.';

CREATE INDEX reconciliation_runs_payment_cursor
    ON reconciliation_runs (tenant_id, provider_id, coverage_end DESC)
    WHERE stream = 'payment_statement' AND provider_id IS NOT NULL AND coverage_end IS NOT NULL;
