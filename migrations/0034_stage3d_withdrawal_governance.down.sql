DROP TRIGGER IF EXISTS withdrawal_approvals_enforce_governance ON withdrawal_approvals;
DROP FUNCTION IF EXISTS withdrawal_approvals_enforce_governance();

-- Restore migration 0033's version of the self-approval-only trigger.
CREATE OR REPLACE FUNCTION withdrawal_approvals_deny_self_approval() RETURNS TRIGGER AS $$
DECLARE
    requester_person_id UUID;
    approver_person_id  UUID;
BEGIN
    IF NEW.decision != 'approve' THEN
        RETURN NEW;
    END IF;

    SELECT pa.person_id INTO requester_person_id
    FROM withdrawal_requests wr
    JOIN player_accounts pa ON pa.id = wr.player_account_id
    WHERE wr.id = NEW.withdrawal_request_id;

    SELECT su.person_id INTO approver_person_id
    FROM staff_users su
    WHERE su.id = NEW.approver_principal_id;

    IF requester_person_id IS NOT NULL
        AND approver_person_id IS NOT NULL
        AND requester_person_id = approver_person_id
    THEN
        RAISE EXCEPTION 'withdrawal_approvals: approver resolves to the same person as the withdrawing player (self-approval)';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER withdrawal_approvals_deny_self_approval
    BEFORE INSERT ON withdrawal_approvals
    FOR EACH ROW EXECUTE FUNCTION withdrawal_approvals_deny_self_approval();

DROP TRIGGER IF EXISTS staff_users_person_id_append_only ON staff_users;
DROP FUNCTION IF EXISTS staff_users_person_id_append_only();

DROP TRIGGER IF EXISTS withdrawal_policies_deny_update ON withdrawal_policies;
DROP FUNCTION IF EXISTS withdrawal_policies_deny_update();
