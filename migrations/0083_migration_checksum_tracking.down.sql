-- Reverses 0083_migration_checksum_tracking.up.sql. Dropping this column
-- discards every recorded checksum (including backfilled ones) - a
-- rollback of this migration means `migrate verify` reports "no checksum
-- column" again until `migrate up` re-adds and re-backfills it.
ALTER TABLE schema_migrations DROP COLUMN IF EXISTS checksum;
