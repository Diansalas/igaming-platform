DROP TRIGGER withdrawal_approvals_deny_self_approval ON withdrawal_approvals;
DROP FUNCTION withdrawal_approvals_deny_self_approval();
DROP INDEX idx_staff_users_person;
ALTER TABLE staff_users DROP COLUMN person_id;
