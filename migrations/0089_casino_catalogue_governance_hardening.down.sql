-- Reverses migration 0089: restores migration 0086's original three
-- function bodies verbatim (the weaker principal-eligibility/self-approval
-- checks and the oldest-first, non-payload-matched consume function), and
-- drops the 3-argument overload of casino_catalogue_change_consume_
-- approved_request this migration introduced, restoring the single
-- 2-argument function 0086 originally defined.

CREATE OR REPLACE FUNCTION casino_catalogue_change_requests_require_platform_principal() RETURNS TRIGGER AS $$
DECLARE
    v_is_platform_scoped BOOLEAN;
BEGIN
    SELECT su.tenant_id IS NULL INTO v_is_platform_scoped
      FROM staff_users su
     WHERE su.id = NEW.requested_by_principal_id;
    IF NOT FOUND OR NOT v_is_platform_scoped THEN
        RAISE EXCEPTION 'casino_catalogue_change_requests: requesting principal % is not a platform-scoped staff principal (ADR 0081 §5.2: catalogue governance is never reachable by a tenant-scoped role)', NEW.requested_by_principal_id;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION casino_catalogue_change_approvals_deny_self_approval() RETURNS TRIGGER AS $$
DECLARE
    v_requester_principal_id UUID;
    v_requester_person_id    UUID;
    v_approver_person_id     UUID;
    v_approver_is_platform   BOOLEAN;
BEGIN
    SELECT r.requested_by_principal_id INTO v_requester_principal_id
      FROM casino_catalogue_change_requests r
     WHERE r.id = NEW.request_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'casino_catalogue_change_approvals: request % is not visible in this scope', NEW.request_id;
    END IF;

    IF NEW.approver_principal_id = v_requester_principal_id THEN
        RAISE EXCEPTION 'casino_catalogue_change_approvals: the requesting principal may not approve or reject its own catalogue change (self-approval)';
    END IF;

    SELECT su.tenant_id IS NULL, su.person_id INTO v_approver_is_platform, v_approver_person_id
      FROM staff_users su
     WHERE su.id = NEW.approver_principal_id;
    IF NOT FOUND OR NOT v_approver_is_platform THEN
        RAISE EXCEPTION 'casino_catalogue_change_approvals: approving principal % is not a platform-scoped staff principal', NEW.approver_principal_id;
    END IF;

    SELECT su.person_id INTO v_requester_person_id
      FROM staff_users su
     WHERE su.id = v_requester_principal_id;

    IF v_requester_person_id IS NOT NULL
        AND v_approver_person_id IS NOT NULL
        AND v_requester_person_id = v_approver_person_id
    THEN
        RAISE EXCEPTION 'casino_catalogue_change_approvals: approver resolves to the same person as the requester (self-approval through a second staff account)';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION casino_games_enforce_dual_control() RETURNS TRIGGER AS $$
DECLARE
    v_removed    TEXT[];
    v_request_id UUID;
    v_payload    JSONB;
BEGIN
    IF NEW.status = 'active' AND OLD.status = 'disabled' THEN
        PERFORM casino_catalogue_change_consume_approved_request(NEW.id, 'status_activate');
    END IF;

    v_removed := ARRAY(
        SELECT unnest(OLD.jurisdiction_blocklist)
        EXCEPT
        SELECT unnest(NEW.jurisdiction_blocklist)
    );

    IF array_length(v_removed, 1) IS NOT NULL THEN
        v_request_id := casino_catalogue_change_consume_approved_request(NEW.id, 'jurisdiction_unblock');

        SELECT payload INTO v_payload FROM casino_catalogue_change_requests WHERE id = v_request_id;

        IF (SELECT ARRAY(SELECT jsonb_array_elements_text(v_payload -> 'removed_codes') ORDER BY 1))
            IS DISTINCT FROM
           (SELECT ARRAY(SELECT unnest(v_removed) ORDER BY 1))
        THEN
            RAISE EXCEPTION 'casino_games: approved jurisdiction_unblock request % payload removed_codes does not match the codes actually being removed from game % (approving "unblock X" must never authorize "unblock X, Y")', v_request_id, NEW.id;
        END IF;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP FUNCTION IF EXISTS casino_catalogue_change_consume_approved_request(UUID, TEXT, JSONB);

CREATE OR REPLACE FUNCTION casino_catalogue_change_consume_approved_request(p_game_id UUID, p_operation TEXT) RETURNS UUID AS $$
DECLARE
    v_request_id UUID;
BEGIN
    SELECT r.id INTO v_request_id
      FROM casino_catalogue_change_requests r
     WHERE r.game_id = p_game_id
       AND r.operation = p_operation
       AND r.state = 'pending'
       AND EXISTS (
           SELECT 1
             FROM casino_catalogue_change_approvals a
            WHERE a.request_id = r.id
              AND a.decision = 'approve'
              AND a.approver_principal_id <> r.requested_by_principal_id
       )
       AND NOT EXISTS (
           SELECT 1 FROM casino_catalogue_change_approvals a
            WHERE a.request_id = r.id AND a.decision = 'reject'
       )
     ORDER BY r.requested_at
     FOR UPDATE
     LIMIT 1;

    IF v_request_id IS NULL THEN
        RAISE EXCEPTION 'casino_games: % of game % requires a pending casino_catalogue_change_requests row approved by a DIFFERENT platform principal (four-eyes, ADR 0081 §5)', p_operation, p_game_id;
    END IF;

    UPDATE casino_catalogue_change_requests
       SET state = 'applied',
           applied_at = now(),
           applied_by_principal_id = NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid
     WHERE id = v_request_id;

    RETURN v_request_id;
END;
$$ LANGUAGE plpgsql;
