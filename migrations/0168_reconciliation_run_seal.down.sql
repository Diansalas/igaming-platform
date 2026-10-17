-- Reverses 0168 (DEV/STAGING ONLY, per the migrate-down policy). Refuses while any run row carries a run seal:
-- dropping the columns would destroy the evidence the payment_statement cursor and the M2 gate rest on, on an
-- append-only table. With only NULLs the columns are dropped and the table returns to its 0167 shape.
--
-- reconciliation_runs carries tenant-scoped FORCE ROW LEVEL SECURITY, which binds even the migration role (the 0152 /
-- 0160 down pattern): RLS is disabled for the duration of this transaction so the guard can see every row; it is
-- transactional DDL, so a raised exception rolls the DISABLE back, and ENABLE + FORCE are re-applied below otherwise.
SET LOCAL lock_timeout = '5s';

ALTER TABLE reconciliation_runs DISABLE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM reconciliation_runs WHERE run_seal IS NOT NULL OR run_import_id IS NOT NULL) THEN
        RAISE EXCEPTION '0168 down refused: reconciliation_runs holds sealed payment_statement runs that would be lost';
    END IF;
END
$$;

DROP INDEX reconciliation_runs_payment_sealed_cursor;
ALTER TABLE reconciliation_runs
    DROP CONSTRAINT reconciliation_runs_run_import_fk,
    DROP CONSTRAINT reconciliation_runs_run_seal_shape,
    DROP CONSTRAINT reconciliation_runs_run_seal_all_or_none,
    DROP COLUMN run_seal_kid,
    DROP COLUMN run_seal,
    DROP COLUMN run_import_id;

ALTER TABLE reconciliation_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_runs FORCE ROW LEVEL SECURITY;
