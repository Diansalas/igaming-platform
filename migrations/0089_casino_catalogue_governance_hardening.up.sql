-- Stage 9.2 fix round: closes two confirmed defects independent security
-- and code review found in migration 0086's casino catalogue four-eyes
-- governance (ADR 0081 §5.2). Migration 0086 itself is deliberately NOT
-- edited in place - it is an applied migration, and rewriting history
-- would make the schema a different thing from what the migration chain
-- says it is. This mirrors migration 0047's own precedent for fixing
-- migration 0044 the same way.
--
-- ======================================================================
-- FIX 1 (P1, SEC-S92-1) - self-approval/principal-eligibility check was
-- migration 0044's ORIGINAL (weak) shape, not 0047's hardened shape.
-- ======================================================================
-- Migration 0086's casino_catalogue_change_requests_require_platform_
-- principal() and casino_catalogue_change_approvals_deny_self_approval()
-- mirrored migration 0044's original person-linkage check: the person
-- comparison only fired when BOTH staff_users.person_id values were
-- non-NULL, and there was no staff_users.status = 'active' check
-- anywhere. Security reproduced, empirically, that two platform_admin
-- staff accounts with person_id IS NULL (seed-admin's actual default
-- output) can file -> approve -> apply the SAME change end to end - one
-- human, two accounts, four-eyes defeated - and separately that a
-- SUSPENDED staff principal is accepted as a valid second approver.
--
-- The justification migration 0086 gave for choosing 0044's shape over
-- 0047's - "casino_games has no person-linking deployment dependency to
-- manage", the same reasoning migration 0085 used - is no longer valid:
-- POST /v1/admin/platform-staff/{id}/person-link ships today, seed-admin
-- accepts a person argument, and migration 0047's hardened trigger
-- (mandatory Person linkage + active status on both principals) is
-- already live elsewhere in this codebase (asset_change_requests/
-- asset_change_approvals). Migration 0085's own check (casino_games'
-- principal-RESOLUTION trigger) is a different, already-correct check
-- per security's own analysis and is NOT touched here - only the two
-- catalogue-governance functions below are brought to 0047's shape.
--
-- Both functions below are a verbatim structural copy of migration
-- 0047's asset_change_requests_require_platform_principal() and asset_
-- change_approvals_deny_self_approval(), adapted to this table's own
-- columns/messages: a principal that fails to resolve to a staff row, is
-- tenant-scoped, has no confirmed Person linkage, or is not 'active' is
-- REJECTED OUTRIGHT (fail closed), never silently treated as having no
-- person link.
--
-- ----------------------------------------------------------------------
-- *** DEPLOYMENT ORDERING NOTE, mirroring migration 0047's own ***
-- ----------------------------------------------------------------------
-- Applied against a platform_admin staff population with no Person
-- linkage at all, this migration makes catalogue governance PERMANENTLY
-- UNUSABLE (fail-closed, correct direction, but a total outage of this
-- administrative surface) until the affected accounts are person-linked
-- via POST /v1/admin/platform-staff/{id}/person-link. Verification query
-- before deploy - this should return zero rows, or catalogue governance
-- is unusable for that account:
--
--   SELECT id, email FROM staff_users
--    WHERE tenant_id IS NULL AND status = 'active' AND person_id IS NULL;

CREATE OR REPLACE FUNCTION casino_catalogue_change_requests_require_platform_principal() RETURNS TRIGGER AS $$
DECLARE
    v_tenant_id UUID;
    v_person_id UUID;
    v_status    TEXT;
BEGIN
    SELECT su.tenant_id, su.person_id, su.status
      INTO v_tenant_id, v_person_id, v_status
      FROM staff_users su
     WHERE su.id = NEW.requested_by_principal_id;

    -- FOUND, checked immediately after the SELECT INTO, mirrors migration
    -- 0034/0044/0047's own idiom: with no matching row PL/pgSQL sets every
    -- INTO target to NULL, so a naive "IF v_tenant_id IS NULL" would treat
    -- an unresolvable principal identically to a resolvable, platform-
    -- scoped one - failing open.
    IF NOT FOUND THEN
        RAISE EXCEPTION 'casino_catalogue_change_requests: requesting principal % cannot be resolved to a staff account and is not eligible to request a catalogue change', NEW.requested_by_principal_id;
    END IF;

    IF v_tenant_id IS NOT NULL THEN
        RAISE EXCEPTION 'casino_catalogue_change_requests: requesting principal % is not a platform-scoped staff principal (ADR 0081 §5.2: catalogue governance is never reachable by a tenant-scoped role)', NEW.requested_by_principal_id;
    END IF;

    -- THE Fix-1 condition on this side. Without a confirmed Person
    -- linkage the four-eyes control cannot tell two staff accounts held
    -- by one human apart from two humans, so an unlinked principal is
    -- refused rather than silently exempted from the comparison.
    IF v_person_id IS NULL THEN
        RAISE EXCEPTION 'casino_catalogue_change_requests: requesting principal % has no confirmed Person linkage and is not eligible to request a catalogue change (four-eyes cannot be evaluated without it)', NEW.requested_by_principal_id;
    END IF;

    IF v_status IS DISTINCT FROM 'active' THEN
        RAISE EXCEPTION 'casino_catalogue_change_requests: requesting staff account % is not active', NEW.requested_by_principal_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION casino_catalogue_change_approvals_deny_self_approval() RETURNS TRIGGER AS $$
DECLARE
    v_requester_principal_id UUID;
    v_requester_person_id    UUID;
    v_approver_person_id     UUID;
    v_approver_tenant_id     UUID;
    v_approver_status        TEXT;
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

    SELECT su.tenant_id, su.person_id, su.status
      INTO v_approver_tenant_id, v_approver_person_id, v_approver_status
      FROM staff_users su
     WHERE su.id = NEW.approver_principal_id;

    IF NOT FOUND THEN
        RAISE EXCEPTION 'casino_catalogue_change_approvals: approver identity % cannot be resolved and is not eligible to decide catalogue changes', NEW.approver_principal_id;
    END IF;

    IF v_approver_tenant_id IS NOT NULL THEN
        RAISE EXCEPTION 'casino_catalogue_change_approvals: approving principal % is not a platform-scoped staff principal', NEW.approver_principal_id;
    END IF;

    -- THE Fix-1 conditions on the approver side: mandatory Person linkage
    -- and active status. Neither can be skipped by either side being
    -- NULL any more - both sides are required to be non-NULL before the
    -- comparison below is even reached.
    IF v_approver_person_id IS NULL THEN
        RAISE EXCEPTION 'casino_catalogue_change_approvals: approver has no confirmed Person linkage and is not eligible to decide catalogue changes (four-eyes cannot be evaluated without it)';
    END IF;

    IF v_approver_status IS DISTINCT FROM 'active' THEN
        RAISE EXCEPTION 'casino_catalogue_change_approvals: approver staff account is not active';
    END IF;

    -- The requester's person_id is re-resolved (rather than trusted from
    -- an earlier check) because a request filed before this migration
    -- existed can still be 'pending' with an unlinked requester -
    -- comparing against NULL is exactly the inert check being fixed.
    SELECT su.person_id INTO v_requester_person_id
      FROM staff_users su
     WHERE su.id = v_requester_principal_id;

    IF v_requester_person_id IS NULL THEN
        RAISE EXCEPTION 'casino_catalogue_change_approvals: the requesting principal has no confirmed Person linkage, so this request cannot be four-eyes approved (file a new request from a person-linked principal)';
    END IF;

    -- Unconditional now: two staff accounts held by one human are one
    -- human, and this is no longer a check that quietly does nothing
    -- because both sides happened to be NULL.
    IF v_requester_person_id = v_approver_person_id THEN
        RAISE EXCEPTION 'casino_catalogue_change_approvals: approver resolves to the same person as the requester (self-approval through a second staff account)';
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ======================================================================
-- FIX 2 (P1-equivalent correctness bug, SEC-S92-5) - consume-approved-
-- request selected the OLDEST pending-with-approval request for
-- (game_id, operation), ignoring payload, which could deadlock a
-- genuinely distinct, independently-approved request permanently (there
-- is no cancel/withdraw endpoint).
-- ======================================================================
-- Reproduced by both reviewers: two pending jurisdiction_unblock requests
-- for the same game (one approved to unblock DE, a separate one later
-- approved to unblock FR) - a PUT removing only FR consumed the OLDER DE
-- request instead, failed the payload-match check, and aborted the whole
-- UPDATE, leaving the FR approval permanently stuck behind the unrelated
-- DE one.
--
-- Fixed by selection matching payload correlation, not oldest-first -
-- migration 0047's own established pattern for the identical problem
-- (asset_change_consume_approved_request's p_payload_match/jsonb
-- containment). The 2-argument form is kept (status_activate's payload is
-- always '{}', and internal/casino's own
-- TestCasinoCatalogueChangeConsumeApprovedRequest_ConcurrentCalls_
-- OnlyOneSucceeds calls it directly) and now delegates to the 3-argument
-- form, so there remains exactly ONE place that marks a request applied.
--
-- '{}'::jsonb containment-matches every payload, so the 2-arg delegation
-- below behaves exactly as migration 0086's original 2-arg-only form did
-- for status_activate (whose requests never carry a distinguishing
-- payload in the first place - only one game-wide re-activation can ever
-- be pending at a time in practice, so oldest-first was never actually
-- wrong for that operation; the bug was jurisdiction_unblock-specific).
CREATE OR REPLACE FUNCTION casino_catalogue_change_consume_approved_request(
    p_game_id UUID, p_operation TEXT, p_payload_match JSONB
) RETURNS UUID AS $$
DECLARE
    v_request_id UUID;
BEGIN
    SELECT r.id INTO v_request_id
      FROM casino_catalogue_change_requests r
     WHERE r.game_id = p_game_id
       AND r.operation = p_operation
       AND r.state = 'pending'
       -- JSONB containment: every key/value (and, for the removed_codes
       -- array, every element) in p_payload_match must be present in the
       -- approved request's own payload. This is what makes the FR
       -- request selectable independently of an older, unrelated DE
       -- request instead of always picking the oldest pending row.
       AND r.payload @> p_payload_match
       AND EXISTS (
           SELECT 1
             FROM casino_catalogue_change_approvals a
            WHERE a.request_id = r.id
              AND a.decision = 'approve'
              -- Distinct from the requester: the UNIQUE constraint stops
              -- one principal counting twice, this stops the requester
              -- counting at all (defence in depth behind the
              -- BEFORE INSERT self-approval trigger).
              AND a.approver_principal_id <> r.requested_by_principal_id
       )
       -- A rejection by anyone blocks the request outright: a second
       -- approver's "no" is not something a third approver's "yes" may
       -- silently override.
       AND NOT EXISTS (
           SELECT 1 FROM casino_catalogue_change_approvals a
            WHERE a.request_id = r.id AND a.decision = 'reject'
       )
     ORDER BY r.requested_at
     FOR UPDATE
     LIMIT 1;

    IF v_request_id IS NULL THEN
        RAISE EXCEPTION 'casino_games: % of game % requires a pending casino_catalogue_change_requests row (matching %) approved by a DIFFERENT platform principal (four-eyes, ADR 0081 §5)', p_operation, p_game_id, p_payload_match;
    END IF;

    UPDATE casino_catalogue_change_requests
       SET state = 'applied',
           applied_at = now(),
           applied_by_principal_id = NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid
     WHERE id = v_request_id;

    RETURN v_request_id;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION casino_catalogue_change_consume_approved_request(
    p_game_id UUID, p_operation TEXT
) RETURNS UUID AS $$
BEGIN
    RETURN casino_catalogue_change_consume_approved_request(p_game_id, p_operation, '{}'::jsonb);
END;
$$ LANGUAGE plpgsql;

-- casino_games_enforce_dual_control now passes the sorted removed-codes
-- set as the payload-match argument for jurisdiction_unblock, so
-- selection itself is payload-correlated (not merely the after-the-fact
-- equality check, which stays in place unchanged as defence in depth -
-- containment can in principle admit a superset match, e.g. an approved
-- [DE, FR] request when only DE is being removed right now; the exact
-- sorted-set equality check below is what still refuses that case rather
-- than silently spending a broader approval on a narrower removal).
CREATE OR REPLACE FUNCTION casino_games_enforce_dual_control() RETURNS TRIGGER AS $$
DECLARE
    v_removed        TEXT[];
    v_removed_sorted TEXT[];
    v_request_id     UUID;
    v_payload        JSONB;
BEGIN
    IF NEW.status = 'active' AND OLD.status = 'disabled' THEN
        PERFORM casino_catalogue_change_consume_approved_request(NEW.id, 'status_activate', '{}'::jsonb);
    END IF;

    v_removed := ARRAY(
        SELECT unnest(OLD.jurisdiction_blocklist)
        EXCEPT
        SELECT unnest(NEW.jurisdiction_blocklist)
    );

    IF array_length(v_removed, 1) IS NOT NULL THEN
        v_removed_sorted := ARRAY(SELECT unnest(v_removed) ORDER BY 1);

        v_request_id := casino_catalogue_change_consume_approved_request(
            NEW.id, 'jurisdiction_unblock',
            jsonb_build_object('removed_codes', to_jsonb(v_removed_sorted))
        );

        SELECT payload INTO v_payload FROM casino_catalogue_change_requests WHERE id = v_request_id;

        IF (SELECT ARRAY(SELECT jsonb_array_elements_text(v_payload -> 'removed_codes') ORDER BY 1))
            IS DISTINCT FROM
           v_removed_sorted
        THEN
            RAISE EXCEPTION 'casino_games: approved jurisdiction_unblock request % payload removed_codes does not match the codes actually being removed from game % (approving "unblock X" must never authorize "unblock X, Y")', v_request_id, NEW.id;
        END IF;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ======================================================================
-- On the "operator has no way to unstick a stale/superseded request"
-- question the fix for FIX 2 exposes: DELIBERATELY not building a cancel
-- endpoint here. The payload-matching fix above fully resolves the
-- practical deadlock the reviewers reproduced - two correctly
-- payload-matched, independently-approved requests for the same game can
-- now both be applied independently, in either order, with no
-- interference (proved by TestApplyDualControl_TwoDistinctApprovedRequests_
-- ApplyIndependently{FRFirst,DEFirst} in internal/casino). A request only
-- remains genuinely "stuck" if it is superseded by a DIFFERENT, broader
-- or narrower removal than what was actually approved - which is not a
-- new problem this fix introduces, is not the deadlock the reviewers
-- reported, and per this stage's "smallest correct API surface"
-- principle does not justify a new mutating admin endpoint on its own.
-- ======================================================================
