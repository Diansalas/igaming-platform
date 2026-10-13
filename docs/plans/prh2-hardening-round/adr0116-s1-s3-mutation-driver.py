"""ADR 0116 S1+S3 mutation driver (migrations 0140-0142 + Go). Each mutant replaces ONE anchor (which must occur
exactly once), runs the relevant suite, restores the file. KILLED = the suite failed. The integration suites migrate
throwaway scratch databases from the worktree's migration files on every run, so a mutated migration is exercised as
written. Run from the worktree root (no other test run may be active):
    python3 docs/plans/prh2-hardening-round/adr0116-s1-s3-mutation-driver.py [name-prefix ...]
Output is the kill list; save with `> docs/plans/prh2-hardening-round/adr0116-s1-s3-mutation-log-*.txt`.
"""
import os
import subprocess
import sys

W = os.getcwd()
S = '/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad'
U0 = 'migrations/0140_aml_access_foundation.up.sql'
U1 = 'migrations/0141_aml_holds_and_enforcement.up.sql'
U2 = 'migrations/0142_aml_cases_and_four_eyes.up.sql'


def db(pkg, run):
    return ['env', 'PRIV_DB=adr0116mut', 'PRIV_SRC=' + W, 'bash', S + '/priv_test.sh', '-tags', 'integration', '-p', '1',
            '-count=1', '-run', run, pkg]


AML = db('./internal/aml/', 'Test')
KYC = db('./internal/kyc/', 'TestHold|TestDecisionReader|TestEnforcement')
ADJ = db('./internal/adjustment/', 'TestAMLHold')
WD = db('./internal/withdrawal/', 'TestAMLHold')
LINT = ['go', 'test', '-count=1', './internal/aml/', '-run', 'Lint']

M = [
    # ---- 0140 authorisation primitive ----
    ('A1 quorum threshold 1', U0, "IF public.aml_officer_quorum(p_tenant, p_owner_scope) < 2 THEN", "IF public.aml_officer_quorum(p_tenant, p_owner_scope) < 1 THEN", AML),
    ('A2 quorum counts grants not Persons', U0, "count(DISTINCT r.grantee_person_id)::integer", "count(g.id)::integer", AML),
    ('A3 quorum counts revoked grants', U0, "       AND g.revoked_at IS NULL\n       AND g.valid_from <= pg_catalog.now()\n       AND (g.valid_until IS NULL OR pg_catalog.now() < g.valid_until)\n       AND g.grantee_scope = CASE",
     "       AND g.valid_from <= pg_catalog.now()\n       AND (g.valid_until IS NULL OR pg_catalog.now() < g.valid_until)\n       AND g.grantee_scope = CASE", AML),
    ('A4 quorum counts expired grants', U0, "       AND (g.valid_until IS NULL OR pg_catalog.now() < g.valid_until)\n       AND g.grantee_scope = CASE",
     "       AND g.grantee_scope = CASE", AML),
    ('A5 quorum ignores the grantee scope', U0, "AND g.grantee_scope = CASE p_owner_scope WHEN 'tenant' THEN 'tenant' WHEN 'platform' THEN 'platform' ELSE '?' END", "", AML),
    ('A5b quorum counts suspended grantees', U0, "       AND NOT EXISTS (SELECT 1 FROM public.staff_users su WHERE su.id = g.grantee_staff_id AND su.status <> 'active')\n", "", AML),
    ('A6 authorize: session-tenant mismatch allowed', U0, "    IF v.tenant IS DISTINCT FROM p_tenant THEN", "    IF false THEN", AML),
    ('A7 authorize: SQL role re-check dropped', U0, "    IF NOT EXISTS (\n        SELECT 1 FROM public.staff_capability_grants g\n         WHERE g.tenant_id = p_tenant AND g.grantee_staff_id = v.actor",
     "    IF false AND NOT EXISTS (\n        SELECT 1 FROM public.staff_capability_grants g\n         WHERE g.tenant_id = p_tenant AND g.grantee_staff_id = v.actor", AML),
    ('A8 authorize: revoked grant accepted', U0, "AND g.grantee_scope = v_grant_scope AND g.revoked_at IS NULL", "AND g.grantee_scope = v_grant_scope", AML),
    ('A9 authorize: grantee scope not matched', U0, "AND g.grantee_scope = v_grant_scope AND g.revoked_at IS NULL", "AND g.revoked_at IS NULL", AML),
    ('A10 authorize: proof never consumed', U0, "    PERFORM public.actor_proof_require(v.actor, v.scope, v.tenant, p_operation, p_target, p_payload_hash);",
     "    NULL;", AML),
    ('A11 authorize: platform (non-acting) session admitted', U0, "    ELSIF v.scope = 'platform_acting' THEN\n        v_grant_scope := 'platform';",
     "    ELSIF v.scope IN ('platform_acting', 'platform') THEN\n        v_grant_scope := 'platform';", AML),
    ('A12 authorize: operation name unchecked', U0, "    IF p_operation !~ '^aml_[a-z_]+:[a-z_]+$' THEN", "    IF false THEN", AML),
    ('A13 authorize: personless actor admitted', U0, "    IF v.person_id IS NULL THEN\n        RAISE EXCEPTION 'aml: the officer has no linked person'", "    IF false THEN\n        RAISE EXCEPTION 'aml: the officer has no linked person'", AML),
    ('A14 scope compat: tenant actor reaches platform rows', U0, "WHEN 'tenant' THEN p_owner_scope = 'tenant'", "WHEN 'tenant' THEN p_owner_scope IN ('tenant', 'platform')", AML),
    ('A15 scope compat: acting actor limited', U0, "WHEN 'platform_acting' THEN p_owner_scope IN ('platform', 'tenant')", "WHEN 'platform_acting' THEN p_owner_scope = 'platform'", AML),
    ('A16 row scope assertion off', U0, "    IF p_owner_scope IS NULL OR NOT public.aml_scope_compatible(p_actor_scope, p_owner_scope) THEN\n        RAISE EXCEPTION 'aml: not found'",
     "    IF false THEN\n        RAISE EXCEPTION 'aml: not found'", AML),
    # ---- 0141 holds ----
    ('H1 match ignores lifts', U1, "       AND NOT EXISTS (SELECT 1 FROM public.aml_hold_lifts l WHERE l.hold_id = h.id)\n     ORDER BY", "     ORDER BY", AML),
    ('H2 active ignores lifts', U1, "           AND NOT EXISTS (SELECT 1 FROM public.aml_hold_lifts l WHERE l.hold_id = h.id)\n    );", "    );", AML),
    ('H3 active ignores the person', U1, "         WHERE h.tenant_id = p_tenant AND h.person_id = p_person\n           AND (h.player_account_id IS NULL OR h.player_account_id = p_account)",
     "         WHERE h.tenant_id = p_tenant\n           AND (h.player_account_id IS NULL OR h.player_account_id = p_account)", AML),
    ('H4 active ignores the tenant', U1, "         WHERE h.tenant_id = p_tenant AND h.person_id = p_person\n           AND (h.player_account_id IS NULL OR h.player_account_id = p_account)",
     "         WHERE h.person_id = p_person\n           AND (h.player_account_id IS NULL OR h.player_account_id = p_account)", AML),
    ('H5 active: account-scoped hold applies to every account', U1, "           AND (h.player_account_id IS NULL OR h.player_account_id = p_account)\n           AND (h.hold_scope", "           AND (h.hold_scope", AML),
    ('H6 active: withdrawal-only hold gates play', U1, "           AND (h.hold_scope = 'all_value_movement' OR p_operation IN ('withdrawal', 'payout', 'manual_adjustment'))\n           AND NOT EXISTS",
     "           AND true\n           AND NOT EXISTS", AML),
    ('H7 active: withdrawal-only hold does not gate payout', U1, "           AND (h.hold_scope = 'all_value_movement' OR p_operation IN ('withdrawal', 'payout', 'manual_adjustment'))\n           AND NOT EXISTS",
     "           AND (h.hold_scope = 'all_value_movement' OR p_operation IN ('withdrawal', 'manual_adjustment'))\n           AND NOT EXISTS", AML),
    ('H8 active: withdrawal-only hold does not gate adjustments', U1, "           AND (h.hold_scope = 'all_value_movement' OR p_operation IN ('withdrawal', 'payout', 'manual_adjustment'))\n           AND NOT EXISTS",
     "           AND (h.hold_scope = 'all_value_movement' OR p_operation IN ('withdrawal', 'payout'))\n           AND NOT EXISTS", AML),
    ('H9 active: session tenant unchecked', U1, "    IF p_tenant IS DISTINCT FROM public.aml_session_tenant() THEN\n        RAISE EXCEPTION 'aml: the session tenant is not the requested tenant' USING ERRCODE = 'AM026';\n    END IF;\n    RETURN EXISTS (",
     "    RETURN EXISTS (", AML),
    ('H10 place: quorum not asserted', U1, "    PERFORM public.aml_assert_quorum(p_tenant, v_owner);\n    INSERT INTO public.aml_holds", "    INSERT INTO public.aml_holds", AML),
    ('H11 place: owner scope not enforced', U1, "    IF public.aml_actor_owner_scope(a.o_scope) IS DISTINCT FROM v_owner THEN\n        RAISE EXCEPTION 'aml: the actor does not own this tenant''s AML scope' USING ERRCODE = 'AM002';\n    END IF;\n    PERFORM public.aml_assert_quorum",
     "    PERFORM public.aml_assert_quorum", AML),
    ('H12 guard: owner scope not forced', U1, "    IF NEW.owner_scope IS DISTINCT FROM public.aml_owner_scope_of_tenant(NEW.tenant_id) THEN\n        RAISE EXCEPTION 'aml: the hold owner scope",
     "    IF false THEN\n        RAISE EXCEPTION 'aml: the hold owner scope", AML),
    ('H13 guard: C2 account/person binding off', U1, "        IF v_acct IS NULL OR v_acct <> NEW.person_id THEN", "        IF false THEN", AML),
    ('H14 guard: C2 person-without-account off', U1, "    ELSIF NOT EXISTS (SELECT 1 FROM public.player_accounts pa WHERE pa.tenant_id = NEW.tenant_id AND pa.person_id = NEW.person_id) THEN\n        RAISE EXCEPTION 'aml: the person has no account in this tenant'",
     "    ELSIF false THEN\n        RAISE EXCEPTION 'aml: the person has no account in this tenant'", AML),
    ('H15 guard: placing actor columns unverified', U1, "RAISE EXCEPTION 'aml: the placing actor columns are not the proven session actor'", "NULL; --", AML),
    ('H16 lift guard: other owner scope may lift', U1, "    IF NEW.owner_scope <> v_hold.owner_scope THEN\n        RAISE EXCEPTION 'aml: only the hold''s owner scope may lift it'", "    IF false THEN\n        RAISE EXCEPTION 'aml: only the hold''s owner scope may lift it'", AML),
    ('H17 lift guard: sanctions lift without approval', U1, "    IF v_hold.reason_class = 'confirmed_sanctions' AND NEW.approval_event_id IS NULL THEN", "    IF false THEN", AML),
    ('H18 lift guard: approval accepted for manual hold', U1, "    IF v_hold.reason_class <> 'confirmed_sanctions' AND NEW.approval_event_id IS NOT NULL THEN", "    IF false THEN", AML),
    ('H19 lift: sanctions hold liftable through the plain path', U1, "    IF v_hold.reason_class = 'confirmed_sanctions' THEN\n        RAISE EXCEPTION 'aml: a confirmed sanctions hold is lifted only", "    IF false THEN\n        RAISE EXCEPTION 'aml: a confirmed sanctions hold is lifted only", AML),
    ('H20 record decision: account/person pairing unchecked', U1, "    IF NOT EXISTS (SELECT 1 FROM public.player_accounts pa WHERE pa.id = p_account AND pa.tenant_id = p_tenant AND pa.person_id = p_person) THEN", "    IF false THEN", AML),
    ('H21 record decision: session tenant unchecked', U1, "    IF p_tenant IS DISTINCT FROM public.aml_session_tenant() THEN\n        RAISE EXCEPTION 'aml: the session tenant is not the requested tenant' USING ERRCODE = 'AM026';\n    END IF;\n    IF NOT EXISTS (SELECT 1 FROM public.player_accounts pa WHERE pa.id = p_account",
     "    IF NOT EXISTS (SELECT 1 FROM public.player_accounts pa WHERE pa.id = p_account", AML),
    ('H22 append-only deny: update allowed', U1, "    RAISE EXCEPTION 'aml: % is append-only; % is not permitted', TG_TABLE_NAME, TG_OP USING ERRCODE = 'AM005';", "    RETURN NEW;", AML),
    ('H23 runtime lacks EXECUTE on aml_hold_active', U1, "        GRANT EXECUTE ON FUNCTION aml_hold_active(uuid, uuid, uuid, text) TO igaming_runtime;\n", "", AML),
    # ---- 0142 cases / four-eyes ----
    ('C1 events: sequence gap allowed', U2, "    IF NEW.seq <> v_last + 1 THEN", "    IF false THEN", AML),
    ('C2 events: reopen of opened case allowed', U2, "        IF v_last <> 0 THEN\n            RAISE EXCEPTION 'aml: a case is opened exactly once'", "        IF false THEN\n            RAISE EXCEPTION 'aml: a case is opened exactly once'", AML),
    ('C3 transition: from_status unchecked', U2, "        IF NEW.from_status IS DISTINCT FROM v_status THEN\n            RAISE EXCEPTION 'aml: the transition does not start", "        IF false THEN\n            RAISE EXCEPTION 'aml: the transition does not start", AML),
    ('C4 transition: SAR closure without approval', U2, "        IF NEW.to_status IN ('closed_sar_not_filed', 'closed_sar_filed') THEN", "        IF false THEN", AML),
    ('C5 transition: illegal edge allowed', U2, "        IF NOT public.aml_case_transition_allowed(NEW.from_status, NEW.to_status) THEN", "        IF false THEN", AML),
    ('C6 transition: SAR-flagged case closes as no action', U2, "        IF NEW.to_status = 'closed_no_action' AND v_flag THEN", "        IF false THEN", AML),
    ('C7 sar flag: reversible/repeatable', U2, "        IF v_flag THEN\n            RAISE EXCEPTION 'aml: the case is already SAR-flagged", "        IF false THEN\n            RAISE EXCEPTION 'aml: the case is already SAR-flagged", AML),
    ('C8 proposal: subject may propose', U2, "        IF NEW.actor_person_id = v_case.person_id THEN\n            RAISE EXCEPTION 'aml: the case subject may not propose", "        IF false THEN\n            RAISE EXCEPTION 'aml: the case subject may not propose", AML),
    ('C9 proposal: content hash unchecked', U2, "        IF NEW.content_hash IS DISTINCT FROM public.aml_proposal_content_hash(", "        IF false AND NEW.content_hash IS DISTINCT FROM public.aml_proposal_content_hash(", AML),
    ('C10 approval: proposer may approve (actor)', U2, "        IF NEW.actor_id = v_prop.actor_id THEN", "        IF false THEN", AML),
    ('C11 approval: same Person may approve', U2, "        IF NEW.actor_person_id = v_prop.actor_person_id THEN", "        IF false THEN", AML),
    ('C12 approval: subject may approve', U2, "        IF NEW.actor_person_id = v_case.person_id THEN\n            RAISE EXCEPTION 'aml: the case subject may not approve", "        IF false THEN\n            RAISE EXCEPTION 'aml: the case subject may not approve", AML),
    ('C13 approval: content binding unchecked', U2, "        IF v_prop.proposal_kind <> NEW.proposal_kind OR v_prop.content_hash <> NEW.content_hash", "        IF false AND v_prop.proposal_kind <> NEW.proposal_kind OR v_prop.content_hash <> NEW.content_hash", AML),
    ('C14 approval: hold change does not invalidate', U2, "        IF EXISTS (SELECT 1 FROM public.aml_holds h WHERE h.tenant_id = NEW.tenant_id AND h.person_id = v_case.person_id AND h.placed_at > v_prop.created_at)",
     "        IF false AND EXISTS (SELECT 1 FROM public.aml_holds h WHERE h.tenant_id = NEW.tenant_id AND h.person_id = v_case.person_id AND h.placed_at > v_prop.created_at)", AML),
    ('C15 approval: already-lifted hold re-approvable', U2, "        IF NEW.proposal_kind = 'lift_hold' THEN\n            IF EXISTS (SELECT 1 FROM public.aml_hold_lifts l WHERE l.hold_id = NEW.subject_hold_id) THEN",
     "        IF NEW.proposal_kind = 'lift_hold' THEN\n            IF false AND EXISTS (SELECT 1 FROM public.aml_hold_lifts l WHERE l.hold_id = NEW.subject_hold_id) THEN", AML),
    ('C16 approval: from_status unchecked for closures', U2, "        ELSIF NEW.from_status IS DISTINCT FROM v_status THEN\n            RAISE EXCEPTION 'aml: the approval does not start", "        ELSIF false THEN\n            RAISE EXCEPTION 'aml: the approval does not start", AML),
    ('C17 lift guard 0142: approval event not verified', U2, "RAISE EXCEPTION 'aml: the approval event does not authorise this hold lift'", "NULL; --", AML),
    ('C18 lift guard 0142: sanctions lift without approval', U2, "    IF v_hold.reason_class = 'confirmed_sanctions' AND NEW.approval_event_id IS NULL THEN", "    IF false THEN", AML),
    ('C19 note: owner scope of note not the case', U2, "    IF NEW.owner_scope <> v_case.owner_scope THEN\n        RAISE EXCEPTION 'aml: the note owner scope", "    IF false THEN\n        RAISE EXCEPTION 'aml: the note owner scope", AML),
    ('C20 event: owner scope of event not the case', U2, "    IF NEW.owner_scope <> v_case.owner_scope THEN\n        RAISE EXCEPTION 'aml: the event owner scope", "    IF false THEN\n        RAISE EXCEPTION 'aml: the event owner scope", AML),
    ('C21 case: owner scope not forced', U2, "    IF NEW.owner_scope IS DISTINCT FROM public.aml_owner_scope_of_tenant(NEW.tenant_id) THEN\n        RAISE EXCEPTION 'aml: the case owner scope", "    IF false THEN\n        RAISE EXCEPTION 'aml: the case owner scope", AML),
    ('C22 case link: cross-person alert allowed', U2, "    IF v_case.person_id IS NULL OR v_alert.person_id IS NULL OR v_case.person_id <> v_alert.person_id OR v_case.owner_scope <> v_alert.owner_scope THEN", "    IF false THEN", AML),
    ('C23 CAS: stale expected seq allowed', U2, "    IF p_expected_seq IS DISTINCT FROM v_last THEN", "    IF false THEN", AML),
    ('C24 open: owner scope not enforced', U2, "    IF public.aml_actor_owner_scope(a.o_scope) IS DISTINCT FROM v_owner THEN\n        RAISE EXCEPTION 'aml: the actor does not own this tenant''s AML scope' USING ERRCODE = 'AM002';\n    END IF;\n    IF p_case_kind = 'alert'",
     "    IF p_case_kind = 'alert'", AML),
    ('C25 IP header regex loosened', U2, "'^[0-9a-fA-F:.]{2,45}$'", "'.*'", AML),
    ('C26 request-id regex loosened', U2, "current_setting('app.aml_request_id', true) ~ '^[A-Za-z0-9_.:-]{1,128}$'", "current_setting('app.aml_request_id', true) ~ '.*'", AML),
    ('C27 queue limit unbounded', U2, "p_limit > 100 THEN", "p_limit > 100000 THEN", AML),
    # ---- Go: lint ----
    ('L1 lint: email check off', 'internal/aml/lint.go', "\tcase emailRE.MatchString(n), obfEmail.MatchString(n):", "\tcase false:", LINT),
    ('L2 lint: obfuscated email check off', 'internal/aml/lint.go', "\tcase emailRE.MatchString(n), obfEmail.MatchString(n):", "\tcase emailRE.MatchString(n):", LINT),
    ('L3 lint: IBAN check off', 'internal/aml/lint.go', "\tcase ibanRE.MatchString(n):", "\tcase false:", LINT),
    ('L4 lint: wallet check off', 'internal/aml/lint.go', "\tcase walletRE.MatchString(n):", "\tcase false:", LINT),
    ('L5 lint: control characters allowed', 'internal/aml/lint.go', "unicode.IsControl(r) || unicode.Is(unicode.Cf, r)", "unicode.Is(unicode.Cf, r)", LINT),
    ('L6 lint: format characters allowed', 'internal/aml/lint.go', "unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || ", "unicode.IsControl(r) || ", LINT),
    ('L7 lint: size limit off', 'internal/aml/lint.go', "\tif len(s) > maxBytes {", "\tif false {", LINT),
    ('L8 lint: digit run disabled', 'internal/aml/lint.go', "phoneRE.MatchString(n) || docRE.MatchString(n) || longDigitRun(n) || panShaped(n)", "phoneRE.MatchString(n) || docRE.MatchString(n) || panShaped(n)", LINT),
    ('L9 lint: PAN check disabled', 'internal/aml/lint.go', "phoneRE.MatchString(n) || docRE.MatchString(n) || longDigitRun(n) || panShaped(n)", "phoneRE.MatchString(n) || docRE.MatchString(n) || longDigitRun(n)", LINT),
    ('L10 lint: Luhn doubling off', 'internal/aml/lint.go', "\t\t\t\td *= 2\n", "\t\t\t\td *= 1\n", LINT),
    ('L11 lint: PAN length lower bound 20', 'internal/aml/lint.go', "len(digits) < 13 ||", "len(digits) < 20 ||", LINT),
    ('L12 lint: empty note accepted', 'internal/aml/lint.go', "\tif strings.TrimSpace(s) == \"\" {", "\tif false {", LINT),
    ('L13 lint: invalid UTF-8 accepted', 'internal/aml/lint.go', "\tif !utf8.ValidString(s) {", "\tif false {", LINT),
    ('L14 lint: separators break digit runs', 'internal/aml/lint.go', "\t\tif unicode.IsLetter(r) || unicode.IsNumber(r) {\n\t\t\trun = 0", "\t\tif true {\n\t\t\trun = 0", LINT),
    ('L15 lint: filing reference pattern open', 'internal/aml/lint.go', "regexp.MustCompile(`^[A-Za-z0-9._/-]{1,64}$`)", "regexp.MustCompile(`^.*$`)", LINT),
    # ---- Go: evaluator ----
    ('E1 eval: probe error is not a deny', 'internal/aml/hold_eval.go', "\t\treturn HoldVerdict{EvalFailed: true}, raiseEvalFailed(ctx, tx, tenantID, op)", "\t\treturn HoldVerdict{}, raiseEvalFailed(ctx, tx, tenantID, op)", AML),
    ('E2 eval: invalid argument allowed', 'internal/aml/hold_eval.go', "\t\treturn HoldVerdict{EvalFailed: true}, nil\n\t}\n\theld, err", "\t\treturn HoldVerdict{}, nil\n\t}\n\theld, err", AML),
    ('E3 eval: held verdict dropped', 'internal/aml/hold_eval.go', "\tv := HoldVerdict{Held: true}", "\tv := HoldVerdict{}", AML),
    ('E4 eval: alert result dropped', 'internal/aml/hold_eval.go', "return HoldVerdict{EvalFailed: true}, raiseEvalFailed(ctx, tx, tenantID, op)", "_ = raiseEvalFailed(ctx, tx, tenantID, op)\n\t\treturn HoldVerdict{EvalFailed: true}, nil", AML),
    ('E5 eval: probe without savepoint rollback', 'internal/aml/hold_eval.go', "\tb.Queue(\"ROLLBACK TO SAVEPOINT \" + sp)\n", "", AML),
    ('E6 eval: timeout 0 (unbounded)', 'internal/aml/hold_eval.go', "const HoldEvalTimeout = 2 * time.Second", "const HoldEvalTimeout = 0 * time.Second", AML),
    ('E7 eval: probe against the wrong operation', 'internal/aml/hold_eval.go', "b.Queue(`SELECT aml_hold_active($1, $2, $3, $4)`, tenantID, personID, accountID, string(op))", "b.Queue(`SELECT aml_hold_active($1, $2, $3, $4)`, tenantID, personID, accountID, string(OpDeposit))", AML),
    ('E8 eval: wrong person', 'internal/aml/hold_eval.go', "b.Queue(`SELECT aml_hold_active($1, $2, $3, $4)`, tenantID, personID, accountID, string(op))", "b.Queue(`SELECT aml_hold_active($1, $2, $3, $4)`, tenantID, accountID, accountID, string(op))", AML),
    # ---- Go: composition / gates ----
    ('K1 compose: hold error not a deny', 'internal/kyc/enforcement.go', "\t\treturn EnforcementDecision{}, fmt.Errorf(\"%w: %w\", ErrEnforcementUnavailable, hErr)", "\t\treturn kycDecision, nil", KYC),
    ('K2 compose: held does not deny', 'internal/kyc/enforcement.go', "\tif !verdict.Denied() {\n\t\treturn kycDecision, nil", "\tif true {\n\t\treturn kycDecision, nil", KYC),
    ('K3 compose: pending code leaks', 'internal/kyc/enforcement.go', "\t\tkycDecision.Code = AccountReviewCode\n\t\tkycDecision.Message = accountReviewMessage\n\t}\n\tif !verdict", "\t}\n\tif !verdict", KYC),
    ('K4 compose: held output code leaks', 'internal/kyc/enforcement.go', "\tout.Code = AccountReviewCode\n", "\tout.Code = \"kyc_hold\"\n", KYC),
    ('K5 compose: held output still allowed', 'internal/kyc/enforcement.go', "\tout.Allowed = false\n\tout.Code = AccountReviewCode", "\tout.Allowed = true\n\tout.Code = AccountReviewCode", KYC),
    ('K6 compose: true outcome not recorded', 'internal/kyc/enforcement.go', "\tif verdict.Held {\n\t\tout.TrueOutcome = OutcomeHeld\n\t}", "", KYC),
    ('K7 compose: matched trigger leaks', 'internal/kyc/enforcement.go', "\tout.MatchedTrigger = \"\"\n", "", KYC),
    ('K8 compose: failed KYC loses its result', 'internal/kyc/enforcement.go', "\tif kycDecision.Outcome == OutcomeFailed || kycDecision.Outcome == OutcomeUnavailable {", "\tif false {", KYC),
    ('K9 op map: sportsbook ungated', 'internal/kyc/enforcement.go', "\t\treturn aml.OpSportsbookStake", "\t\treturn \"\"", KYC),
    ('K10 op map: payout maps to withdrawal', 'internal/kyc/enforcement.go', "\t\treturn aml.OpPayout", "\t\treturn aml.OpWithdrawal", KYC),
    ('J1 adjustment: submission not gated', 'internal/adjustment/request.go', "\t} else if v.Denied() {\n\t\treturn Request{}, ErrAccountUnderReview", "\t} else if false {\n\t\treturn Request{}, ErrAccountUnderReview", ADJ),
    ('J2 adjustment: execution not gated', 'internal/adjustment/execute.go', "\tif holdVerdict.Denied() {", "\tif false {", ADJ),
    ('J3 adjustment: wallet unresolved treated as held', 'internal/adjustment/aml_hold.go', "\tif errors.Is(err, pgx.ErrNoRows) {\n\t\treturn aml.HoldVerdict{}, nil", "\tif errors.Is(err, pgx.ErrNoRows) {\n\t\treturn aml.HoldVerdict{Held: true}, nil", ADJ),
    ('J4 adjustment: resolve error allows', 'internal/adjustment/aml_hold.go', "\t\treturn aml.HoldVerdict{EvalFailed: true}, fmt.Errorf(\"adjustment: resolve", "\t\treturn aml.HoldVerdict{}, nil //", ADJ),
    ('J5 adjustment: wrong operation', 'internal/adjustment/aml_hold.go', "accountID, aml.OpManualAdjustment)", "accountID, aml.OpDeposit)", ADJ),
]

only = sys.argv[1:]
_targets = sorted({m[1] for m in M})
_dirty = subprocess.run(['git', 'status', '--porcelain', '--'] + _targets, cwd=W, capture_output=True, text=True).stdout.strip()
if _dirty:
    sys.exit('mutation targets are not clean (restore them from git first):\n' + _dirty)
for name, f, old, new, cmd in M:
    if only and not any(name.startswith(o) for o in only):
        continue
    path = os.path.join(W, f)
    src = open(path).read()
    if src.count(old) != 1:
        print('SKIP (anchor count %d): %s' % (src.count(old), name), flush=True)
        continue
    if os.environ.get('DRY'):
        continue
    open(path, 'w').write(src.replace(old, new))
    try:
        r = subprocess.run(cmd, cwd=W, capture_output=True, text=True, timeout=2400)
        out = r.stdout + r.stderr
        verdict = 'KILLED' if r.returncode != 0 else 'SURVIVED'
        first = ''
        for line in out.splitlines():
            if ('--- FAIL' in line) or 'want SQLSTATE' in line or ('.go:' in line and 'must' in line):
                first = line.strip()[:160]
                break
    finally:
        open(path, 'w').write(src)
    print('%s: %s%s' % (verdict, name, ('  [' + first + ']') if first else ''), flush=True)
