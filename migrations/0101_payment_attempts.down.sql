-- Down for 0101_payment_attempts: drop both tables (triggers, indexes,
-- policies and constraints go with them), in FK-safe order.
DROP TABLE IF EXISTS payment_provider_events;
DROP TABLE IF EXISTS payment_attempts;
DROP FUNCTION IF EXISTS payment_provider_events_guard();
DROP FUNCTION IF EXISTS payment_attempts_guard();
