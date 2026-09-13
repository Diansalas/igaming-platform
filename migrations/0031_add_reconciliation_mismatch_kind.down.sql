DROP INDEX IF EXISTS idx_reconciliation_mismatches_kind;
ALTER TABLE reconciliation_mismatches DROP COLUMN IF EXISTS mismatch_kind;
