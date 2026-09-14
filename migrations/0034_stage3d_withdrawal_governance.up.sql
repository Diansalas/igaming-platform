-- Stage 3D (Withdrawal Governance Final Gate) - approved business policy:
-- any staff identity that can approve, reject, or submit a withdrawal
-- MUST be linked to a Person, a withdrawal decision must always be
-- attributable to a real Person, and an approver must never be the
-- same Person as the withdrawing beneficiary. "Linked" here means a
-- confirmed staff_users.person_id association, established by an admin
-- action (staff creation or the person-link remediation endpoint) - NOT
-- KYC/identity verification of the Person itself, which this platform
-- does not perform for staff and is out of this stage's scope; see ADR
-- 0024's own note on this interpretation. This migration closes the
-- Stage 3C residual gap (ADR 0023 §1): person_id linkage was optional
-- and, once set, could be silently changed to launder an approval trail.
--
-- Two changes:
-- 1. staff_users.person_id becomes append-only: settable from NULL to a
--    value (the legitimate "link a legacy/unlinked account" remediation
--    path Stage 3D's own policy requires be possible), never re-settable
--    to a DIFFERENT value once linked. Directive item 3.G ("approver
--    attempts to modify own linkage to bypass the rule") is closed by
--    this alone - there is no value the linkage can be changed TO once
--    established, regardless of who attempts it or what permission they
--    hold.
-- 2. The withdrawal_approvals trigger (migration 0029, redefined by
--    0033) is broadened from "deny self-approval" to "enforce withdrawal
--    decision governance": the approver_principal_id must resolve to a
--    staff_users row that (a) exists, (b) has a non-NULL person_id, and
--    (c) has status = 'active' - failing any of those raises an
--    exception before the row is ever inserted. The self-approval
--    equality check remains scoped to decision = 'approve' only
--    (rejecting your own request is not the self-dealing risk approving
--    it is).
--
--    Whether these rules apply is decided by whether a staff_users row
--    actually resolves for approver_principal_id - NEVER by the
--    caller-supplied is_automated_approval flag alone. Migration 0033
--    already removed an identical flag-trusting shortcut from this
--    trigger's predecessor for exactly this reason ("a future caller
--    that mislabels a staff principal as automated would have disabled
--    the DB-level backstop by that label alone") - an earlier draft of
--    this migration reintroduced that same shortcut by exempting every
--    is_automated_approval = true row outright, which would let a real
--    staff member's own self-approval through simply by setting that one
--    boolean. A genuine service identity (ADR 0014) has NO staff_users
--    row at all, so "no row resolved" is what actually, correctly
--    identifies it - not a self-asserted flag.
-- 3. withdrawal_policies gains a BEFORE UPDATE deny-mutation trigger,
--    matching the same append-only pattern this codebase already uses
--    for audit_log (ADR 0013) and withdrawal_approvals (migration 0026):
--    the table's own documentation (internal/httpserver/withdrawal_
--    policy_handlers.go) and its admin API (POST-only, never PUT/PATCH)
--    already describe it as an insert-only history of policy-in-force-
--    over-time, but nothing at the database layer enforced that until
--    now - specialist review (ledger-finance/security) found a direct
--    SQL UPDATE, or any future handler, could otherwise rewrite a
--    threshold in place with no trace. DELETE remains allowed (the
--    admin API's own removal endpoint uses it, and it is safe per that
--    file's own doc comment: no past decision depends on a
--    withdrawal_policies row still existing).

CREATE OR REPLACE FUNCTION staff_users_person_id_append_only() RETURNS TRIGGER AS $$
BEGIN
    IF OLD.person_id IS NOT NULL AND NEW.person_id IS DISTINCT FROM OLD.person_id THEN
        RAISE EXCEPTION 'staff_users: person_id is append-only once set - it cannot be changed to a different value (Stage 3D governance policy)';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER staff_users_person_id_append_only
    BEFORE UPDATE ON staff_users
    FOR EACH ROW EXECUTE FUNCTION staff_users_person_id_append_only();

CREATE OR REPLACE FUNCTION withdrawal_approvals_enforce_governance() RETURNS TRIGGER AS $$
DECLARE
    requester_person_id UUID;
    approver_person_id  UUID;
    approver_status     TEXT;
BEGIN
    SELECT su.person_id, su.status INTO approver_person_id, approver_status
    FROM staff_users su
    WHERE su.id = NEW.approver_principal_id;

    IF NOT FOUND THEN
        -- No staff_users row at all for this principal. A genuine
        -- automated/service decision (ADR 0014's service-identity
        -- pattern, used for a below-threshold risk-engine auto-approval)
        -- has no staff account and is exempt from every rule below - it
        -- is not, and cannot be, a Person. A HUMAN decision naming an
        -- identity that cannot be resolved at all is refused outright:
        -- "identity cannot be resolved" is exactly as ineligible as
        -- "resolved but unlinked" per docs/decisions/0024 §1.
        IF NEW.is_automated_approval THEN
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'withdrawal_approvals: approver identity cannot be resolved and is not eligible to record withdrawal decisions';
    END IF;

    -- A REAL staff_users row resolved for this principal - every rule
    -- below applies REGARDLESS of the caller-supplied
    -- is_automated_approval flag (see this migration's own doc comment
    -- for why: that flag identifies nothing on its own once a real
    -- staff row exists for the id).
    IF approver_person_id IS NULL THEN
        RAISE EXCEPTION 'withdrawal_approvals: approver has no confirmed Person linkage and is not eligible to record withdrawal decisions';
    END IF;

    IF approver_status IS DISTINCT FROM 'active' THEN
        RAISE EXCEPTION 'withdrawal_approvals: approver staff account is not active';
    END IF;

    IF NEW.decision = 'approve' THEN
        SELECT pa.person_id INTO requester_person_id
        FROM withdrawal_requests wr
        JOIN player_accounts pa ON pa.id = wr.player_account_id
        WHERE wr.id = NEW.withdrawal_request_id;

        IF requester_person_id IS NOT NULL AND requester_person_id = approver_person_id THEN
            RAISE EXCEPTION 'withdrawal_approvals: approver resolves to the same person as the withdrawing player (self-approval)';
        END IF;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS withdrawal_approvals_deny_self_approval ON withdrawal_approvals;
DROP FUNCTION IF EXISTS withdrawal_approvals_deny_self_approval();

CREATE TRIGGER withdrawal_approvals_enforce_governance
    BEFORE INSERT ON withdrawal_approvals
    FOR EACH ROW EXECUTE FUNCTION withdrawal_approvals_enforce_governance();

CREATE OR REPLACE FUNCTION withdrawal_policies_deny_update() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'withdrawal_policies: rows are insert-only - create a new row instead of updating an existing one (Stage 3D governance policy)';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER withdrawal_policies_deny_update
    BEFORE UPDATE ON withdrawal_policies
    FOR EACH ROW EXECUTE FUNCTION withdrawal_policies_deny_update();
