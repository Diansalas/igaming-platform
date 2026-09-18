DROP FUNCTION IF EXISTS bonus_change_consume_approved_request(UUID, TEXT, UUID, JSONB, INTEGER, UUID);
DROP TABLE IF EXISTS bonus_change_approvals;
DROP FUNCTION IF EXISTS bonus_change_approvals_enforce_separation();
DROP FUNCTION IF EXISTS bonus_change_approvals_enforce_governance();
DROP TABLE IF EXISTS bonus_change_requests;
DROP FUNCTION IF EXISTS bonus_change_requests_enforce_immutability();
DROP TABLE IF EXISTS bonus_approval_policies;
DROP FUNCTION IF EXISTS bonus_approval_policies_deny_update();
