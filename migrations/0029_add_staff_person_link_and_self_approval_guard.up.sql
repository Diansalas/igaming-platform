-- Stage 3C hardening: closes the withdrawal self-approval bypass
-- (withdrawal-state-machine.md §5 bypass #2) left documented-but-open at
-- the end of Stage 3B, because staff_users had no relationship to
-- persons - there was no data to check "the approving staff member is
-- also the withdrawing player" against. This migration adds exactly that
-- relationship (nothing more - no new identity model, per the Stage 3C
-- directive's own instruction to preserve the existing Person/
-- PlayerAccount/Staff architecture) and enforces the rule at the
-- database itself, not merely in application code that a future change
-- could silently stop calling.
--
-- staff_users.person_id is nullable and OPTIONAL: the overwhelming
-- majority of staff accounts have no corresponding player account and
-- never will, so most rows keep it NULL forever - that's expected, not
-- an incomplete migration. It is set only for the rare, deliberately
-- identified case of a real person who is both a platform/tenant staff
-- member and a player at some brand.
ALTER TABLE staff_users ADD COLUMN person_id UUID REFERENCES persons (id);
CREATE INDEX idx_staff_users_person ON staff_users (person_id) WHERE person_id IS NOT NULL;

-- The authoritative enforcement: a BEFORE INSERT trigger on
-- withdrawal_approvals, the one row a self-approval must always create.
-- This is deliberately NOT "check in Go, hope every future call site
-- remembers to pass a beneficiaryCheck" - the Stage 3C directive requires
-- enforcement that is "authoritative, not merely audit-detectable", and
-- a trigger is the only mechanism that holds regardless of what any
-- current or future Go code path does. internal/withdrawal's own
-- service-level BeneficiaryCheck hook (already designed in Stage 3B,
-- now given a real implementation at the HTTP layer) is kept as the
-- first line of defense purely so a self-approval attempt fails with a
-- clean application error (ErrSelfApproval) instead of a raw Postgres
-- exception - this trigger is the backstop that holds even if that
-- service-level check is ever bypassed, disabled, or has a bug.
--
-- Automated (risk-engine/service-identity) approvals are exempt by
-- design: is_automated_approval rows never correspond to a human staff
-- member who could "be" the withdrawing player - approver_principal_id
-- for those rows is a service identity with no staff_users row at all
-- (see withdrawal_approvals' own comment on why approver_principal_id
-- carries no FK to staff_users), so the LEFT JOIN below simply finds no
-- match and the check is a no-op for them, same effect as an explicit
-- skip but without a redundant branch.
--
-- Runs inside the SAME transaction, and therefore the SAME RLS scope
-- (app.tenant_id already set to this withdrawal's own tenant by
-- db.Pool.WithTenant), as every existing call to internal/withdrawal.Approve
-- - so the joins below see exactly the rows a correctly-scoped INSERT
-- already has permission to see; no SECURITY DEFINER, no privilege
-- escalation.
CREATE FUNCTION withdrawal_approvals_deny_self_approval() RETURNS TRIGGER AS $$
DECLARE
    requester_person_id UUID;
    approver_person_id  UUID;
BEGIN
    IF NEW.decision != 'approve' OR NEW.is_automated_approval THEN
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
