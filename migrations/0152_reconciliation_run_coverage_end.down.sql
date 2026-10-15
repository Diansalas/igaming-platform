-- Reverses 0152 (DEV/STAGING ONLY, per the migrate-down policy). Refuses when any
-- run recorded a coverage_end: dropping the column would destroy evidence on an
-- append-only table. With only NULLs the column is dropped and the table returns
-- to its 0151 shape exactly.
--
-- reconciliation_runs carries tenant-scoped FORCE ROW LEVEL SECURITY, which binds
-- even the migration role: with no app.tenant_id set the plain EXISTS below would
-- see zero rows and the guard would never fire (the same reasoning as migration
-- 0100's down). RLS is therefore disabled for the duration of this transaction -
-- safe because it is transactional DDL: if the guard raises, the whole transaction
-- including the DISABLE rolls back; otherwise ENABLE + FORCE are re-applied below.
SET LOCAL lock_timeout = '5s';

ALTER TABLE reconciliation_runs DISABLE ROW LEVEL SECURITY;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM reconciliation_runs WHERE coverage_end IS NOT NULL) THEN
        RAISE EXCEPTION '0152 down refused: reconciliation_runs.coverage_end holds recorded statement coverage that would be lost';
    END IF;
END
$$;

ALTER TABLE reconciliation_runs DROP COLUMN coverage_end;

ALTER TABLE reconciliation_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE reconciliation_runs FORCE ROW LEVEL SECURITY;
