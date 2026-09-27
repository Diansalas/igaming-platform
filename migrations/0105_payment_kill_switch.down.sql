ALTER TABLE payment_kill_switches DROP CONSTRAINT IF EXISTS payment_kill_switches_release_request_fk;
DROP TABLE IF EXISTS payment_kill_switch_release_requests;
DROP TABLE IF EXISTS payment_kill_switches;
DROP FUNCTION IF EXISTS payment_kill_switch_release_requests_guard();
DROP FUNCTION IF EXISTS payment_kill_switches_guard();
DROP FUNCTION IF EXISTS payment_kill_switch_session();
