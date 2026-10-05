-- Reverses 0114 (ADR 0106 section 5.3). ONE atomic DO block: under standalone
-- psql autocommit either everything is undone or nothing is (security F7: the
-- r1 two-block ordering hazard, Kind removed and table kept, cannot occur).
--
-- 1. Refusal FIRST, on ANY outbox row (outbox rows are the only durable record
--    of worker claims; retention is an open human decision, HQ-E1-4). The
--    owner bypasses RLS only for this one count, inside this one statement; a
--    refusal rolls the ALTER back with the rest.
-- 2. Drops the table, its functions and the 36 worker fence policies.
-- 3. Unseeds the Kind through a temporary single-Kind FOR DELETE policy with the
--    alert_kinds deny trigger disabled and re-enabled inside the same block.
--    FK checks bypass RLS, so a referencing alert or occurrence makes this fail
--    un-blindably; the handler names the remedy.
DO $$
DECLARE
    v_count BIGINT;
BEGIN
    ALTER TABLE kyc_submission_outbox NO FORCE ROW LEVEL SECURITY;
    SELECT count(*) INTO v_count FROM kyc_submission_outbox;
    IF v_count > 0 THEN
        RAISE EXCEPTION 'migration 0114 down: refusing - % kyc_submission_outbox row(s) exist', v_count;
    END IF;
    DROP TABLE kyc_submission_outbox;
    DROP FUNCTION kyc_submission_outbox_guard();
    DROP FUNCTION kyc_submission_outbox_session();

    -- The fence family (36 DROP POLICY statements, literal).
    DROP POLICY kyc_worker_fence_select ON staff_users;
    DROP POLICY kyc_worker_fence_insert ON staff_users;
    DROP POLICY kyc_worker_fence_update ON staff_users;
    DROP POLICY kyc_worker_fence_delete ON staff_users;
    DROP POLICY kyc_worker_fence_select ON audit_log;
    DROP POLICY kyc_worker_fence_insert ON audit_log;
    DROP POLICY kyc_worker_fence_update ON audit_log;
    DROP POLICY kyc_worker_fence_delete ON audit_log;
    DROP POLICY kyc_worker_fence_select ON sessions;
    DROP POLICY kyc_worker_fence_insert ON sessions;
    DROP POLICY kyc_worker_fence_update ON sessions;
    DROP POLICY kyc_worker_fence_delete ON sessions;
    DROP POLICY kyc_worker_fence_select ON login_attempts;
    DROP POLICY kyc_worker_fence_insert ON login_attempts;
    DROP POLICY kyc_worker_fence_update ON login_attempts;
    DROP POLICY kyc_worker_fence_delete ON login_attempts;
    DROP POLICY kyc_worker_fence_select ON risk_rules;
    DROP POLICY kyc_worker_fence_insert ON risk_rules;
    DROP POLICY kyc_worker_fence_update ON risk_rules;
    DROP POLICY kyc_worker_fence_delete ON risk_rules;
    DROP POLICY kyc_worker_fence_select ON player_restrictions;
    DROP POLICY kyc_worker_fence_insert ON player_restrictions;
    DROP POLICY kyc_worker_fence_update ON player_restrictions;
    DROP POLICY kyc_worker_fence_delete ON player_restrictions;
    DROP POLICY kyc_worker_fence_select ON persons;
    DROP POLICY kyc_worker_fence_insert ON persons;
    DROP POLICY kyc_worker_fence_update ON persons;
    DROP POLICY kyc_worker_fence_delete ON persons;
    DROP POLICY kyc_worker_fence_select ON asset_operation_eligibility;
    DROP POLICY kyc_worker_fence_insert ON asset_operation_eligibility;
    DROP POLICY kyc_worker_fence_update ON asset_operation_eligibility;
    DROP POLICY kyc_worker_fence_delete ON asset_operation_eligibility;
    DROP POLICY kyc_worker_fence_select ON open_bet_self_exclusion_policies;
    DROP POLICY kyc_worker_fence_insert ON open_bet_self_exclusion_policies;
    DROP POLICY kyc_worker_fence_update ON open_bet_self_exclusion_policies;
    DROP POLICY kyc_worker_fence_delete ON open_bet_self_exclusion_policies;

    -- Unseed the Kind.
    ALTER TABLE alert_kinds DISABLE TRIGGER alert_kinds_deny_update_delete;
    CREATE POLICY alert_kinds_unseed_0114 ON alert_kinds
        FOR DELETE TO CURRENT_USER USING (kind = 'kyc.submission_failed_terminal');
    DELETE FROM alert_kinds WHERE kind = 'kyc.submission_failed_terminal';
    DROP POLICY alert_kinds_unseed_0114 ON alert_kinds;
    ALTER TABLE alert_kinds ENABLE TRIGGER alert_kinds_deny_update_delete;
EXCEPTION WHEN foreign_key_violation THEN
    RAISE EXCEPTION 'migration 0114 down: refusing - alerts of kind kyc.submission_failed_terminal exist; resolve and archive them under an authorized procedure first';
END
$$;
