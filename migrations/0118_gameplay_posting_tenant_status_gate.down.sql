-- Reverses 0118: drops the gameplay tenant-status gate triggers and functions.
-- Existing ledger and tenant rows are untouched. WARNING (DEV/CI ONLY): after
-- this, only the application-level check protects gameplay postings and it is
-- no longer race-free against a concurrent tenant status change.
DROP TRIGGER IF EXISTS ledger_gameplay_tenant_active_guard ON ledger_transactions;
DROP FUNCTION IF EXISTS ledger_gameplay_tenant_active_guard();
DROP TRIGGER IF EXISTS tenants_status_change_gate ON tenants;
DROP FUNCTION IF EXISTS tenants_status_change_gate();
DROP FUNCTION IF EXISTS tenant_status_gate_key(uuid);
