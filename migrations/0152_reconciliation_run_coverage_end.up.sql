-- A3 security re-review SR3B-3: the casino_statement stream's coverage
-- monotonicity anchor must not be read from audit rows (forgeable by the
-- runtime role, which may INSERT audit_log with any created_at and metadata).
-- The run record the stream itself writes under its advisory lock carries the
-- statement's validated CoverageEnd instead.
--
-- Purely additive: one nullable column. NULL for every existing run and for
-- every stream/source that states no coverage (Synthetic in-house sources, all
-- other streams). reconciliation_runs keeps everything it had: FORCE ROW LEVEL
-- SECURITY and the tenant_isolation policy (0027/0028), the total append-only
-- trigger pair reconciliation_runs_immutable / _no_truncate (0082; a new column
-- does not touch them - rows are still only ever INSERTed), and the runtime
-- role's existing table-level grants (a table-level GRANT covers the new
-- column; no column-level privilege is introduced).
SET LOCAL lock_timeout = '5s';

ALTER TABLE reconciliation_runs ADD COLUMN coverage_end TIMESTAMPTZ;
COMMENT ON COLUMN reconciliation_runs.coverage_end IS
    'Validated CoverageEnd of the provider statement a casino_statement run compared (A3 SR3B-3); NULL when the source stated none.';
