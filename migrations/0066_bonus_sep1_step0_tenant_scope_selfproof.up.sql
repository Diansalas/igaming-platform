-- Stage 4H-B1 Wave 2 Phase 6 (security, independent review): closes a
-- genuine non-conformance between migration 0063's
-- bonus_change_approvals_enforce_separation() (the SEP-1 trigger,
-- REQ-SEP-BONUS-1/4) and its own frozen contract,
-- docs/security/security-architecture.md §W15.1.9 (Fix Round 2,
-- RK-W15P2-1's cardinality-assertion/step-0 amendment).
--
-- THE GAP, STATED PRECISELY. §W15.1.9 is unconditional: "Every
-- `<table>_enforce_separation()` trigger runs this [step 0] BEFORE step
-- 1 of §W15.1.3, not as a replacement for the 'run under the operation's
-- own tenant context' mitigation but as its enforcement." Migration
-- 0063's bonus_change_approvals_enforce_separation() has NO step 0 at
-- all: it resolves the beneficiary Person via ordinary (non-SECURITY
-- DEFINER, RLS-subject) SELECTs against bonus_grants/bulk_grant_jobs/
-- player_accounts/staff_users, relying entirely on "the caller opened
-- the right kind of transaction" - exactly the discipline-not-database
-- pattern this whole document (and internal/risk/evaluator.go's own
-- verifyConnectionScope, which §W15.1.9 names as the thing to port) was
-- written to eliminate. Practical severity is reduced, not eliminated,
-- by bonus_change_approvals' own RLS INSERT policy (migration 0063's
-- tenant_staff_insert: tenant_id = app.tenant_id AND
-- app.player_account_id IS NULL) - but that policy's WITH CHECK is
-- evaluated AFTER this BEFORE-INSERT trigger runs, never before it, so
-- it is not a substitute for the trigger proving its own scope, only a
-- second, independent backstop the trigger must not lean on as if it
-- already ran. This migration adds Step 0 exactly as specified, closing
-- the non-conformance rather than assuming Phase 3's own report ("SEP-1
-- applies... step 0 tenant-scope self-proof") was accurate without
-- reading the actual trigger SQL - it was not: no step 0 existed in the
-- shipped function body.
--
-- Only bonus_change_approvals_enforce_separation() is touched.
-- bonus_change_approvals_enforce_governance() (the ORDINARY four-eyes
-- requester<>approver check, modeled verbatim on migration 0034's
-- already-reviewed precedent) is a different control - §W15.1.9's step 0
-- is specified for the SEP-1 mechanism (§W15.1.3) specifically, and
-- migration 0034's own governance shape is the template SEP-1 reuses,
-- not a second SEP-1 instance that also needs it.
--
-- No cardinality-assertion machinery (resolver_shape/expected_count) is
-- added on top of step 0: for bonus's two resolvable shapes here,
-- single_subject's cardinality is already fully covered by the existing
-- NULL check (0 or 1 row, nothing to truncate), and the pinned_set
-- (player_list) shape here is a direct EXISTS membership test, not a
-- materialize-then-count query - once step 0 has PROVEN the connection
-- carries no player-account narrowing and matches the row's own tenant,
-- every player_accounts row in that tenant is visible to it (the same
-- "prove total by construction, before running, rather than counting
-- after" argument §W15.1.9 uses for doc 32's ancestor_closure shape),
-- so a separate numeric assertion would add complexity without closing
-- a distinct failure mode.

CREATE OR REPLACE FUNCTION bonus_change_approvals_enforce_separation() RETURNS TRIGGER AS $$
DECLARE
    v_request            bonus_change_requests%ROWTYPE;
    v_subject_person_id  UUID;
    v_target_kind        TEXT;
    v_single_player      UUID;
    v_player_list        UUID[];
    v_requester_person   UUID;
    v_approver_person    UUID;
    v_scoped_tenant      TEXT;
    v_scoped_player      TEXT;
BEGIN
    -- Step 0 (security-architecture.md §W15.1.9, Fix Round 2) - tenant-
    -- scope self-proof, ported directly from
    -- internal/risk/evaluator.go's verifyConnectionScope/
    -- ErrTenantScopeMismatch/ErrPlayerScopedConnection. Proves the
    -- connection this function's own resolver queries below are about to
    -- run under is scoped to EXACTLY this row's own tenant and carries
    -- no narrower player scope, BEFORE any of those queries run - closing
    -- the RLS-scope-mismatch precondition (RK-W15P2-1) rather than only
    -- reacting to its symptom.
    v_scoped_player := NULLIF(current_setting('app.player_account_id', true), '');
    IF v_scoped_player IS NOT NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'SP001',
            MESSAGE = 'SEP-1: refuse — app.player_account_id is set on this transaction (ErrPlayerScopedConnection analogue); the SEP-1 beneficiary resolver requires an unnarrowed staff connection to prove its reads are total';
    END IF;

    v_scoped_tenant := NULLIF(current_setting('app.tenant_id', true), '');
    IF v_scoped_tenant IS NULL THEN
        RAISE EXCEPTION USING ERRCODE = 'SP001',
            MESSAGE = 'SEP-1: refuse — app.tenant_id is not set on this transaction (ErrTenantScopeMismatch analogue)';
    END IF;
    IF v_scoped_tenant::uuid <> NEW.tenant_id THEN
        RAISE EXCEPTION USING ERRCODE = 'SP001',
            MESSAGE = format('SEP-1: refuse — transaction scoped to %s, row is for %s', v_scoped_tenant, NEW.tenant_id);
    END IF;

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
            -- segment / segment_set: NAMED GAP (unchanged by this
            -- migration) - no materialized set to compare against at
            -- this trigger's own level. Not refused, not silently
            -- treated as passed either - see this function's own doc
            -- comment (migration 0063) and internal/bonus's compensating
            -- per-item runtime check (targeting.go).
            RETURN NEW;
        END IF;

    ELSE
        -- campaign_activate / offer_publish: not SEP-1-scoped.
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
