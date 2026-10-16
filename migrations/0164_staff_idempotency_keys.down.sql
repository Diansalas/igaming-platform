-- Reverses 0164 (DEV/STAGING ONLY, per the migrate-down policy). The table holds only de-duplication state for
-- client retries (hash, status code, timestamps); dropping it loses no authority or audit evidence.
DROP TABLE IF EXISTS staff_idempotency_keys;
DROP FUNCTION IF EXISTS staff_idempotency_keys_guard();
