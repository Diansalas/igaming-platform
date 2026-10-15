-- Reverses 0160 (DEV/STAGING ONLY, per the migrate-down policy). Refuses when any run recorded a payment
-- cursor (provider_id / window_truncated): dropping the columns would destroy evidence on an append-only
-- table. With only NULLs the columns are dropped and the table returns to its 0159-or-earlier shape.
--
-- reconciliation_runs carries tenant-scoped FORCE ROW LEVEL SECURITY, which binds even the migration role
-- (see 0152 down): RLS is disabled for the duration of this transaction so the guard can see every row;
-- it is transactional DDL, so a raised exception rolls the DISABLE back, and ENABLE + FORCE are re-applied
-- below otherwise.
SET LOCAL lock_timeout = '5s';

ALTER TABLE reconciliation_runs DISABLE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM reconciliation_runs WHERE provider_id IS NOT NULL OR window_truncated IS NOT NULL) THEN
        RAISE EXCEPTION '0160 down refused: reconciliation_runs holds payment_statement cursor evidence that would be lost';
    END IF;
END
$$;

DROP INDEX reconciliation_runs_payment_cursor;
ALTER TABLE reconciliation_runs
    DROP CONSTRAINT reconciliation_runs_truncated_needs_provider,
    DROP CONSTRAINT reconciliation_runs_provider_id_shape,
    DROP COLUMN window_truncated,
    DROP COLUMN provider_id;

ALTER TABLE reconciliation_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_runs FORCE ROW LEVEL SECURITY;
