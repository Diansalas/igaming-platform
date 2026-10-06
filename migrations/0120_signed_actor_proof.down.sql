-- Reverses 0120: drops the proof triggers, the verifier, and the OWNER-ONLY key
-- and nonce tables (DEV/STAGING ONLY, per the migrate-down policy). WARNING:
-- after this, four-eyes actor identity is again derived from GUCs alone and the
-- arbitrary-SQL impersonation exposure (THREAT-MODEL-ARBITRARY-SQL-1) is back.
-- Dropping actor_proof_keys destroys the provisioned key material; re-apply 0120
-- and re-provision keys to restore the control.
DROP TRIGGER IF EXISTS zz_actor_proof_guard ON financial_approval_policy_change_approvals;
DROP TRIGGER IF EXISTS zz_actor_proof_guard ON financial_approval_policy_changes;
DROP TRIGGER IF EXISTS zz_actor_proof_guard ON staff_capability_grants;
DROP TRIGGER IF EXISTS zz_actor_proof_guard ON staff_capability_grant_approvals;
DROP TRIGGER IF EXISTS zz_actor_proof_guard ON staff_capability_grant_requests;
DROP TRIGGER IF EXISTS zz_actor_proof_guard ON payment_manual_resolution_approvals;
DROP TRIGGER IF EXISTS zz_actor_proof_guard ON payment_manual_resolutions;
DROP TRIGGER IF EXISTS zz_actor_proof_guard ON ledger_adjustment_approvals;
DROP TRIGGER IF EXISTS zz_actor_proof_guard ON ledger_adjustment_requests;
DROP FUNCTION IF EXISTS actor_proof_financial_approval_policy_change_approvals_guard();
DROP FUNCTION IF EXISTS actor_proof_financial_approval_policy_changes_guard();
DROP FUNCTION IF EXISTS actor_proof_staff_capability_grants_guard();
DROP FUNCTION IF EXISTS actor_proof_staff_capability_grant_approvals_guard();
DROP FUNCTION IF EXISTS actor_proof_staff_capability_grant_requests_guard();
DROP FUNCTION IF EXISTS actor_proof_payment_manual_resolution_approvals_guard();
DROP FUNCTION IF EXISTS actor_proof_payment_manual_resolutions_guard();
DROP FUNCTION IF EXISTS actor_proof_ledger_adjustment_approvals_guard();
DROP FUNCTION IF EXISTS actor_proof_ledger_adjustment_requests_guard();
DROP FUNCTION IF EXISTS actor_proof_k1_request_digest(uuid);
DROP FUNCTION IF EXISTS actor_proof_ts(timestamptz);
DROP FUNCTION IF EXISTS actor_proof_key_active(text);
DROP FUNCTION IF EXISTS actor_proof_require(uuid, text, uuid, text, text, text);
DROP TABLE IF EXISTS actor_proof_nonces;
DROP TABLE IF EXISTS actor_proof_keys;
