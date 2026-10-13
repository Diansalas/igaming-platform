"""ADR 0116 S1+S3 mutation driver (migrations 0140-0142 + Go). Each mutant replaces ONE anchor (which must occur
exactly once), runs the relevant suite, restores the file. KILLED = the suite failed. The integration suites migrate
throwaway scratch databases from the worktree's migration files on every run, so a mutated migration is exercised as
written. Migration mutants rebuild the private DB (adr0116mut) first. Run from the worktree root (no other test run may be active):
    python3 docs/plans/prh2-hardening-round/adr0116-s1-s3-mutation-driver.py [name-prefix ...]
Output is the kill list; save with `> docs/plans/prh2-hardening-round/adr0116-s1-s3-mutation-log-*.txt`.
"""
import os
import shlex
import subprocess
import sys
import time

W = os.getcwd()
S = '/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad'
U0 = 'migrations/0140_aml_access_foundation.up.sql'
U1 = 'migrations/0141_aml_holds_and_enforcement.up.sql'
U2 = 'migrations/0142_aml_cases_and_four_eyes.up.sql'


def rebuild_db():
    """priv_db.sh with retries: a runtime-role connection of the previous test process may still be closing."""
    r = None
    for _ in range(4):
        r = subprocess.run(['env', 'PRIV_DB=adr0116mut', 'PRIV_SRC=' + W, 'bash', S + '/priv_db.sh'], cwd=W,
                           capture_output=True, text=True, timeout=1200)
        if r.returncode == 0 or 'pg_signal_backend' not in (r.stdout + r.stderr):
            break
        time.sleep(4)
    return r


def db(pkg, run):
    return ['env', 'PRIV_DB=adr0116mut', 'PRIV_SRC=' + W, 'bash', S + '/priv_test.sh', '-tags', 'integration', '-p', '1',
            '-count=1', '-run', run, pkg]


AML = db('./internal/aml/', 'Test')
KYC = db('./internal/kyc/', 'TestHold|TestDecisionReader|TestEnforcement|TestComposeHold|TestRetryableOnHoldEvalFailure')
ADJ = db('./internal/adjustment/', 'TestAMLHold')
WD = db('./internal/withdrawal/', 'TestAMLHold')
def chain(*cmds):
    return ['bash', '-c', ' && '.join(' '.join(shlex.quote(a) for a in c) for c in cmds)]


# Evaluator / composition / gate mutants run the aml suite AND every gate package that exercises the hold read.
HTTPF4 = chain(db('./internal/httpserver/', 'TestManualAdjustmentAPI_F4'), db('./internal/adjustment/', 'TestAMLHold'))
SIDE = db('./internal/adjustment/', 'TestSideRecord|TestAMLHold')
PAY = db('./internal/payments/', 'TestAMLHold')
GATES = chain(db('./internal/aml/', 'Test'), db('./internal/kyc/', 'TestHold|TestDecisionReader|TestEnforcement|TestComposeHold|TestRetryableOnHoldEvalFailure'),
              db('./internal/adjustment/', 'TestAMLHold'), db('./internal/withdrawal/', 'TestAMLHold'),
              db('./internal/casino/', 'TestAMLHold'), db('./internal/sportsbook/', 'TestAMLHold'),
              db('./internal/payments/', 'TestAMLHold'))
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
    # (A5b retired: superseded by the N-series live-eligibility / mark mutants below)
    ('A6 authorize: session-tenant mismatch allowed', U0, "    IF v.tenant IS DISTINCT FROM p_tenant THEN", "    IF false THEN", AML),
    ('A7 authorize: SQL role re-check dropped', U0, "    IF NOT EXISTS (\n        SELECT 1 FROM public.staff_capability_grants g\n          JOIN public.staff_capability_grant_requests gr",
     "    IF false AND NOT EXISTS (\n        SELECT 1 FROM public.staff_capability_grants g\n          JOIN public.staff_capability_grant_requests gr", AML),
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
    ('H10 place: quorum not asserted', U1, "    PERFORM public.aml_assert_quorum(p_tenant, v_owner);\n    PERFORM public.aml_person_lock(p_tenant, p_person);\n    INSERT INTO public.aml_holds",
     "    PERFORM public.aml_person_lock(p_tenant, p_person);\n    INSERT INTO public.aml_holds", AML),
    ('H11 place: owner scope not enforced', U1, "    IF public.aml_actor_owner_scope(a.o_scope) IS DISTINCT FROM v_owner THEN\n        RAISE EXCEPTION 'aml: the actor does not own this tenant''s AML scope' USING ERRCODE = 'AM002';\n    END IF;\n    PERFORM public.aml_assert_not_subject(a.o_person_id, p_person);\n    PERFORM public.aml_assert_quorum",
     "    PERFORM public.aml_assert_not_subject(a.o_person_id, p_person);\n    PERFORM public.aml_assert_quorum", AML),
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
    # (C14 retired: superseded by the N-series live-eligibility / mark mutants below)
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
    ('E1 eval: probe error is not a deny', 'internal/aml/hold_eval.go', "\t\treturn HoldVerdict{EvalFailed: true}, raiseEvalFailed(ctx, tx, tenantID, op)", "\t\treturn HoldVerdict{}, raiseEvalFailed(ctx, tx, tenantID, op)", GATES),
    ('E2 eval: invalid argument allowed', 'internal/aml/hold_eval.go', "\t\treturn HoldVerdict{EvalFailed: true}, nil\n\t}\n\theld, err", "\t\treturn HoldVerdict{}, nil\n\t}\n\theld, err", GATES),
    ('E3 eval: held verdict dropped', 'internal/aml/hold_eval.go', "\tv := HoldVerdict{Held: true}", "\tv := HoldVerdict{}", GATES),
    ('E4 eval: alert result dropped', 'internal/aml/hold_eval.go', "return HoldVerdict{EvalFailed: true}, raiseEvalFailed(ctx, tx, tenantID, op)", "_ = raiseEvalFailed(ctx, tx, tenantID, op)\n\t\treturn HoldVerdict{EvalFailed: true}, nil", GATES),
    ('E5 eval: probe without savepoint rollback', 'internal/aml/hold_eval.go', "\tb.Queue(\"ROLLBACK TO SAVEPOINT \" + sp)\n", "", GATES),
    ('E6 eval: timeout 0 (unbounded)', 'internal/aml/hold_eval.go', "const HoldEvalTimeout = 2 * time.Second", "const HoldEvalTimeout = 0 * time.Second", GATES),
    ('E7 eval: probe against the wrong operation', 'internal/aml/hold_eval.go', "b.Queue(`SELECT aml_hold_active($1, $2, $3, $4)`, tenantID, personID, accountID, string(op))", "b.Queue(`SELECT aml_hold_active($1, $2, $3, $4)`, tenantID, personID, accountID, string(OpDeposit))", GATES),
    ('E8 eval: wrong person', 'internal/aml/hold_eval.go', "b.Queue(`SELECT aml_hold_active($1, $2, $3, $4)`, tenantID, personID, accountID, string(op))", "b.Queue(`SELECT aml_hold_active($1, $2, $3, $4)`, tenantID, accountID, accountID, string(op))", GATES),
    # ---- Go: composition / gates ----
    ('K1 compose: hold error not a deny', 'internal/kyc/enforcement.go', "\t\treturn EnforcementDecision{}, fmt.Errorf(\"%w: %w\", ErrEnforcementUnavailable, hErr)", "\t\treturn kycDecision, nil", GATES),
    ('K2 compose: held does not deny', 'internal/kyc/enforcement.go', "\tif !verdict.Denied() {\n\t\treturn kycDecision, nil", "\tif true {\n\t\treturn kycDecision, nil", GATES),
    ('K3 compose: pending code leaks', 'internal/kyc/enforcement.go', "\t\tkycDecision.Code = AccountReviewCode\n\t\tkycDecision.Message = accountReviewMessage\n\t}\n\tif !verdict", "\t}\n\tif !verdict", GATES),
    ('K4 compose: held output code leaks', 'internal/kyc/enforcement.go', "\tout.Code = AccountReviewCode\n", "\tout.Code = \"kyc_hold\"\n", GATES),
    ('K5 compose: held output still allowed', 'internal/kyc/enforcement.go', "\tout.Allowed = false\n\tout.Code = AccountReviewCode", "\tout.Allowed = true\n\tout.Code = AccountReviewCode", GATES),
    ('K6 compose: true outcome not recorded', 'internal/kyc/enforcement.go', "\tif verdict.Held {\n\t\tout.TrueOutcome = OutcomeHeld\n\t}", "", GATES),
    ('K7 compose: matched trigger leaks', 'internal/kyc/enforcement.go', "\tout.MatchedTrigger = \"\"\n", "", GATES),
    ('K8 compose: failed KYC loses its result', 'internal/kyc/enforcement.go', "\tif kycDecision.Outcome == OutcomeFailed || kycDecision.Outcome == OutcomeUnavailable {", "\tif false {", GATES),
    ('K9 op map: sportsbook ungated', 'internal/kyc/enforcement.go', "\t\treturn aml.OpSportsbookStake", "\t\treturn \"\"", GATES),
    ('K10 op map: payout maps to withdrawal', 'internal/kyc/enforcement.go', "\t\treturn aml.OpPayout", "\t\treturn aml.OpWithdrawal", GATES),
    ('J1 adjustment: submission not gated', 'internal/adjustment/request.go', "	} else if h.denied() {", "	} else if false {", HTTPF4),
    # (J2 retired: the refusal path was unified by F4 option (b); see the F4 mutants)
    ('J3 adjustment: wallet unresolved treated as held', 'internal/adjustment/aml_hold.go', "	if errors.Is(err, pgx.ErrNoRows) {\n		return holdCheck{}, nil", "	if errors.Is(err, pgx.ErrNoRows) {\n		return holdCheck{verdict: aml.HoldVerdict{Held: true}}, nil", HTTPF4),
    ('J4 adjustment: wallet resolve error allows', 'internal/adjustment/aml_hold.go', "		h.verdict = aml.HoldVerdict{EvalFailed: true}\n		return h, fmt.Errorf(", "		h.verdict = aml.HoldVerdict{}\n		_ = fmt.Sprint(err)\n		return h, nil //", HTTPF4),
    ('J5 adjustment: wrong operation', 'internal/adjustment/aml_hold.go', "tenantID, h.personID, h.accountID, aml.OpManualAdjustment)", "tenantID, h.personID, h.accountID, aml.OpDeposit)", HTTPF4),
    # ---- review round 1 (security F1-F8, ledger-finance C1/C3): NEW logic only ----
    ('N1 tenant grantee: staff status not checked', U0, "WHERE s.id = p_staff AND s.tenant_id = p_tenant AND s.status = 'active'", "WHERE s.id = p_staff AND s.tenant_id = p_tenant", AML),
    ('N2 tenant grantee: eligible role not checked', U0, "AND s.role = ANY (c.eligible_tenant_roles))", "AND true)", AML),
    ('N3 tenant grantee: live person not compared', U0, "AND s.person_id IS NOT NULL AND s.person_id = p_person AND s.role = ANY", "AND s.role = ANY", AML),
    ('N4 platform grantee: staff status not checked', U0, "s.status = 'active' AND s.role = 'platform_admin'", "s.role = 'platform_admin'", AML),
    ('N5 platform grantee: role not checked', U0, "s.status = 'active' AND s.role = 'platform_admin'", "s.status = 'active'", AML),
    ('N6 platform grantee: live person not compared', U0, "s.person_id IS NOT NULL AND s.person_id = p_person) INTO v_live", "true) INTO v_live", AML),
    ('N7 quorum ignores liveness', U0, "       AND public.aml_grant_eligible(g.tenant_id, g.grantee_staff_id, g.grantee_scope, r.grantee_person_id)\n", "", AML),
    ('N8 authorize ignores liveness', U0, "           AND public.aml_grant_eligible(p_tenant, v.actor, v_grant_scope, gr.grantee_person_id)\n", "", AML),
    ('N9 authorize: request person not tied to the live person', U0, "           AND gr.grantee_person_id = v.person_id\n", "", AML),
    ('N10 not-subject assertion is a no-op', U0, "    IF p_actor_person IS NULL OR p_subject_person IS NULL OR p_actor_person = p_subject_person THEN", "    IF false THEN", AML),
    ('N11 not-subject: NULL person admitted', U0, "    IF p_actor_person IS NULL OR p_subject_person IS NULL OR p_actor_person = p_subject_person THEN", "    IF p_actor_person = p_subject_person THEN", AML),
    ('N12 hold place: no conflict check', U1, "    PERFORM public.aml_assert_not_subject(a.o_person_id, p_person);\n    PERFORM public.aml_assert_quorum", "    PERFORM public.aml_assert_quorum", AML),
    ('N13 hold lift: no conflict check', U1, "    PERFORM public.aml_assert_not_subject(a.o_person_id, v_hold.person_id);\n", "", AML),
    ('N14 hold list: no conflict check', U1, "    PERFORM public.aml_assert_not_subject(a.o_person_id, p_person);\n    RETURN QUERY", "    RETURN QUERY", AML),
    ('N15 alert raise: no conflict check', U2, "p_severity, p_reason_code)));\n    PERFORM public.aml_assert_not_subject(a.o_person_id, p_person);\n", "p_severity, p_reason_code)));\n", AML),
    ('N16 case open: no conflict check', U2, "FROM unnest(p_alerts) AS i))));\n    PERFORM public.aml_assert_not_subject(a.o_person_id, p_person);\n", "FROM unnest(p_alerts) AS i))));\n", AML),
    ('N17 case read: no conflict check', U2, "    PERFORM public.aml_assert_not_subject(a.o_person_id, c.person_id);\n    o_case_id", "    o_case_id", AML),
    ('N18 case events: no conflict check', U2, "    PERFORM public.aml_assert_not_subject(a.o_person_id, c.person_id);\n    RETURN QUERY\n        SELECT e.seq", "    RETURN QUERY\n        SELECT e.seq", AML),
    ('N19 case notes: no conflict check', U2, "    PERFORM public.aml_assert_not_subject(a.o_person_id, c.person_id);\n    RETURN QUERY\n        SELECT n.id", "    RETURN QUERY\n        SELECT n.id", AML),
    ('N20 case writes: no conflict check', U2, "    PERFORM public.aml_assert_not_subject(p_actor_person, v_case.person_id);\n", "", AML),
    ('N21 queue shows the officer own subject', U2, "           AND c.person_id IS DISTINCT FROM a.o_person_id\n", "", AML),
    ('N22 bare acting tenant binds the session', U1, "CASE WHEN public.financial_acting_gucs_exact()", "CASE WHEN true", AML),
    ('N23 decision log accepts another person hold', U1, "    IF p_hold_id IS NOT NULL AND NOT EXISTS (", "    IF false AND NOT EXISTS (", AML),
    ('N24 approval ignores hold changes (mark compare off)', U2, "IF public.aml_person_hold_mark(NEW.tenant_id, v_case.person_id) IS DISTINCT FROM v_prop.hold_mark THEN", "IF false THEN", AML),
    ('N25 proposal does not record the mark', U2, "NEW.hold_mark := public.aml_person_hold_mark(NEW.tenant_id, v_case.person_id);", "NEW.hold_mark := 0;", AML),
    ('N26 mark ignores new holds', U1, "(SELECT max(h.change_seq) FROM public.aml_holds h WHERE h.tenant_id = p_tenant AND h.person_id = p_person),", "NULL::bigint,", AML),
    ('N27 mark ignores lifts', U1, "(SELECT max(l.change_seq) FROM public.aml_hold_lifts l JOIN public.aml_holds h ON h.id = l.hold_id\n          WHERE h.tenant_id = p_tenant AND h.person_id = p_person)), 0);", "NULL::bigint), 0);", AML),
    ('N28 person lock is a no-op (concurrency only)', U1, "    SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended('aml_person:' || p_tenant::text || ':' || p_person::text, 116))", "    SELECT NULL::void", AML),
    ('N29 payout gate does not convert an evaluator fault', 'internal/payments/payout.go', "return decision.RetryableOnHoldEvalFailure(), params, nil", "return decision, params, nil", PAY),
    ('N30 conversion ignores the KYC outcome', 'internal/kyc/enforcement.go', "	if d.KYCOutcome != OutcomePassed && d.KYCOutcome != OutcomeNotRequired {\n		return d\n	}\n", "", PAY),
    ('N31 conversion applies to a definite hold too', 'internal/kyc/enforcement.go', "	if !d.Hold.EvalFailed || d.Hold.Held {\n		return d\n	}", "	if !d.Hold.Denied() {\n		return d\n	}", PAY),
    ('N32 adjustment refusal raises no detached evaluator alert', 'internal/adjustment/aml_hold.go', "aerr := aml.RaiseHoldEvalFailedDetached(context.WithoutCancel(ctx), s.pool, r.tenantID, aml.OpManualAdjustment)", "aerr := error(nil)", HTTPF4),
    # (N33 retired: the refusal path was unified by F4 option (b); see the F4 mutants)
    # (N34 retired: the refusal path was unified by F4 option (b); see the F4 mutants)
    # ---- F4 option (b): AML adjustment refusals render as an ordinary conflict ----
    ('F4a AML refusal no longer classified as the generic conflict', 'internal/adjustment/adjustment.go', "case errors.Is(err, ErrNotPending), errors.Is(err, ErrAccountUnderReview):", "case errors.Is(err, ErrNotPending):", HTTPF4),
    ('F4b execution refusal renders differently', 'internal/adjustment/execute.go', "		return Outcome{}, holdCheck.refusal(requestID)", "		return Outcome{}, ErrIntegrity", HTTPF4),
    ('F4c submission refusal renders differently', 'internal/adjustment/request.go', "		return Request{}, h.refusal(id)", "		return Request{}, ErrInvalidInput", HTTPF4),
    ('F4d real reason not recorded in the AML log', 'internal/adjustment/aml_hold.go', "	if !errors.As(err, &r) || r.tenantID == uuid.Nil {", "	if true {", HTTPF4),
    ('F4e execution hold check dropped', 'internal/adjustment/execute.go', "	if holdCheck.denied() {", "	if false {", HTTPF4),
    # ---- re-review conditions (C2, C3, 4, 5, 7) ----
    ('R1 runtime keeps USAGE on the AML sequence', U1, "        REVOKE ALL ON SEQUENCE aml_hold_change_seq FROM igaming_runtime;\n", "", AML),
    ('R2 hold guard does not force the change mark', U1, "    NEW.change_seq := pg_catalog.nextval('public.aml_hold_change_seq');\n    IF NEW.owner_scope IS DISTINCT", "    IF NEW.owner_scope IS DISTINCT", AML),
    ('R3 lift guard does not force the change mark', U2, "    NEW.change_seq := pg_catalog.nextval('public.aml_hold_change_seq');\n    IF NEW.owner_scope <> v_hold.owner_scope", "    IF NEW.owner_scope <> v_hold.owner_scope", AML),
    ('R4 READ COMMITTED not asserted', U2, "IF pg_catalog.current_setting('transaction_isolation') <> 'read committed' THEN", "IF false THEN", AML),
    ('R5 side writes unbounded (30 s)', 'internal/adjustment/aml_hold.go', "const sideRecordTimeout = 2 * time.Second", "const sideRecordTimeout = 30 * time.Second", SIDE),
    ('R6 side-write failure not logged', 'internal/adjustment/aml_hold.go', "	slog.Default().Error(\"adjustment_side_record_failed\", \"step\", step, \"correlation_id\", correlation.String(), \"failure\", kind)", "	_ = slog.Default()\n	_ = kind", SIDE),
    ('R7 evaluator reason names the hold', 'internal/kyc/enforcement.go', "unavailableDecision(\"evaluator\")", "unavailableDecision(\"hold_evaluator_failed\")", KYC),
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
        if f.startswith('migrations/'):
            # The aml integration world runs against the PRE-MIGRATED private DB, so a migration mutant must be re-applied:
            # rebuild the DB from the mutated files first. A mutant the migration itself refuses (pre-flight, syntax, a
            # CHECK) is reported as KILLED(migrate).
            rb = rebuild_db()
            if rb.returncode != 0 and ('recovery mode' in rb.stdout + rb.stderr or 'failed to connect' in rb.stdout + rb.stderr or 'connection to server' in rb.stdout + rb.stderr):
                print('INVALID(infra): %s  [%s]' % (name, (rb.stdout + rb.stderr).strip().splitlines()[-1][:140]), flush=True)
                open(path, 'w').write(src)
                continue
            if rb.returncode != 0:
                print('KILLED(migrate): %s  [%s]' % (name, (rb.stdout + rb.stderr).strip().splitlines()[-1][:140]), flush=True)
                open(path, 'w').write(src)
                continue
        r = subprocess.run(cmd, cwd=W, capture_output=True, text=True, timeout=2400)
        out = r.stdout + r.stderr
        verdict = 'KILLED' if r.returncode != 0 else 'SURVIVED'
        first = ''
        infra = ('recovery mode' in out) or ('connection slots' in out) or ('no space left' in out) or ('failed to connect' in out)
        if r.returncode != 0 and infra:
            verdict = 'INVALID(infra)'  # the DB or the disk failed: this run says nothing about the mutant, re-run it
        for line in out.splitlines():
            if ('--- FAIL' in line) or 'want SQLSTATE' in line or ('.go:' in line and 'must' in line):
                first = line.strip()[:160]
                break
    finally:
        open(path, 'w').write(src)
        if f.startswith('migrations/'):
            # Leave the private DB clean for the next (possibly Go-only) mutant.
            rebuild_db()
    if verdict == 'KILLED' and not first:
        tail = [l.strip() for l in out.splitlines() if l.strip() and 'level=ERROR' not in l]
        first = ('last: ' + tail[-1][:140]) if tail else ''
    print('%s: %s%s' % (verdict, name, ('  [' + first + ']') if first else ''), flush=True)
