-- Reverses migration 0066, restoring migration 0063's original
-- bonus_change_approvals_enforce_separation() body verbatim (no step 0).
--
-- What reversing this migration actually means, stated plainly: it
-- reinstates a confirmed non-conformance with
-- docs/security/security-architecture.md §W15.1.9's step-0 tenant-scope
-- self-proof requirement for this SEP-1 trigger. It exists because the
-- migration chain must round-trip, not because rolling back is a safe
-- operational choice.

CREATE OR REPLACE FUNCTION bonus_change_approvals_enforce_separation() RETURNS TRIGGER AS $$
DECLARE
    v_request            bonus_change_requests%ROWTYPE;
    v_subject_person_id  UUID;
    v_target_kind        TEXT;
    v_single_player      UUID;
    v_player_list        UUID[];
    v_requester_person   UUID;
    v_approver_person    UUID;
BEGIN
    SELECT * INTO v_request FROM bonus_change_requests WHERE id = NEW.request_id;

    IF v_request.operation IN ('manual_grant_issue', 'bonus_adjustment_write', 'grant_forced_conversion', 'grant_cancel_completed') THEN
        SELECT pa.person_id INTO v_subject_person_id
          FROM bonus_grants g JOIN player_accounts pa ON pa.id = g.player_account_id
         WHERE g.id = v_request.target_id;
        IF v_subject_person_id IS NULL THEN
            RAISE EXCEPTION 'SEP-1: refuse — beneficiary Person could not be resolved for bonus change request %', v_request.id;
        END IF;

    ELSIF v_request.operation = 'held_disposition_resolve' THEN
        SELECT pa.person_id INTO v_subject_person_id
          FROM bonus_held_dispositions hd
          JOIN bonus_grants g ON g.id = hd.grant_id
          JOIN player_accounts pa ON pa.id = g.player_account_id
         WHERE hd.id = v_request.target_id;
        IF v_subject_person_id IS NULL THEN
            RAISE EXCEPTION 'SEP-1: refuse — beneficiary Person could not be resolved for held-disposition resolution request %', v_request.id;
        END IF;

    ELSIF v_request.operation = 'bulk_job_execute' THEN
        SELECT j.target_kind, j.target_player_account_id, j.target_player_list
          INTO v_target_kind, v_single_player, v_player_list
          FROM bulk_grant_jobs j WHERE j.id = v_request.target_id;

        IF v_target_kind IS NULL THEN
            RAISE EXCEPTION 'SEP-1: refuse — bulk grant job % could not be resolved', v_request.target_id;
        END IF;

        IF v_target_kind = 'single_player' THEN
            SELECT pa.person_id INTO v_subject_person_id FROM player_accounts pa WHERE pa.id = v_single_player;
            IF v_subject_person_id IS NULL THEN
                RAISE EXCEPTION 'SEP-1: refuse — beneficiary Person could not be resolved for bulk grant job %', v_request.target_id;
            END IF;
            SELECT su.person_id INTO v_requester_person FROM staff_users su WHERE su.id = v_request.requested_by_principal_id;
            SELECT su.person_id INTO v_approver_person FROM staff_users su WHERE su.id = NEW.approver_principal_id;
            IF v_requester_person = v_subject_person_id OR v_approver_person = v_subject_person_id THEN
                RAISE EXCEPTION 'SEP-1: refuse — requester or approver resolves to the same person as the bulk job''s sole targeted player (self-dealing)';
            END IF;
            RETURN NEW;

        ELSIF v_target_kind = 'player_list' THEN
            SELECT su.person_id INTO v_requester_person FROM staff_users su WHERE su.id = v_request.requested_by_principal_id;
            SELECT su.person_id INTO v_approver_person FROM staff_users su WHERE su.id = NEW.approver_principal_id;
            IF EXISTS (
                SELECT 1 FROM player_accounts pa
                 WHERE pa.id = ANY (v_player_list)
                   AND pa.person_id IN (v_requester_person, v_approver_person)
            ) THEN
                RAISE EXCEPTION 'SEP-1: refuse — requester or approver resolves to the same person as a player on the bulk job''s pinned recipient list (self-dealing)';
            END IF;
            RETURN NEW;
        ELSE
            RETURN NEW;
        END IF;

    ELSE
        RETURN NEW;
    END IF;

    SELECT su.person_id INTO v_requester_person FROM staff_users su WHERE su.id = v_request.requested_by_principal_id;
    SELECT su.person_id INTO v_approver_person FROM staff_users su WHERE su.id = NEW.approver_principal_id;

    IF v_requester_person = v_subject_person_id THEN
        RAISE EXCEPTION 'SEP-1: refuse — requester resolves to the same person as the operation''s own beneficiary (self-dealing)';
    END IF;
    IF v_approver_person = v_subject_person_id THEN
        RAISE EXCEPTION 'SEP-1: refuse — approver resolves to the same person as the operation''s own beneficiary (self-dealing)';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
