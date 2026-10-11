#!/usr/bin/env python3
"""Slice-4 mutation-kill driver. Each mutant: apply a textual replacement to ONE file of the worktree,
run the named test command, require FAILURE (killed), restore the file from the pre-mutation bytes.
Usage: s4_mut.py <group>   (A = DB-free httpserver, B = http+launchgov integration, C = migration 0131)
"""
import os, subprocess, sys, time

WT = "/home/user/igaming-platform/.claude/worktrees/agent-ab3c4f92c6e8fbb5a"
SP = "/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad"
OUT = SP + "/s4_mut_%s.txt" % sys.argv[1]
os.environ["PRIV_DB"] = "igaming_s4_mut"
os.environ["PRIV_SRC"] = WT

def run(cmd, timeout=1500):
    p = subprocess.run(cmd, shell=True, cwd=WT, capture_output=True, text=True, timeout=timeout)
    return p.returncode, (p.stdout + p.stderr)

def rebuild():
    rc, out = run(SP + "/priv_db.sh")
    if rc != 0:
        raise SystemExit("rebuild failed: " + out[-500:])

IT = SP + "/priv_test.sh -tags integration -p 1 -count=1 "

A_CMD = "go test -count=1 ./internal/httpserver -run 'TestLaunchErrorCode|TestWriteLaunchError|TestLaunchRoutes|TestOpenAPI_Launch' 2>&1 | tail -15"
B_CMD = IT + "./internal/httpserver -run 'TestLaunchHTTP' 2>&1 | tail -25"
BL_CMD = IT + "./internal/launchgov -run 'TestFundsCheck|TestLaunchReads|TestConcurrency_TwoFinal' 2>&1 | tail -25"
C_CMD = IT + "./internal/launchgov -run 'TestWithdrawal' 2>&1 | tail -25"

R = "internal/httpserver/launch_authorisation_routes.go"
mutants = {
 "A": [
  ("A1 approve route perm -> request", R, 'launch-authorisations/{requestID}/approve",\n\t\tstaff(auth.PermLaunchAuthorisationApprove', 'launch-authorisations/{requestID}/approve",\n\t\tstaff(auth.PermLaunchAuthorisationRequest', A_CMD),
  ("A2 reject route perm -> read", R, 'launch-authorisations/{requestID}/reject",\n\t\tstaff(auth.PermLaunchAuthorisationApprove', 'launch-authorisations/{requestID}/reject",\n\t\tstaff(auth.PermLaunchAuthorisationRead', A_CMD),
  ("A3 brand suspend perm -> request", R, 'brands/{brandID}/suspend",\n\t\tstaff(auth.PermLaunchAuthorisationSuspend', 'brands/{brandID}/suspend",\n\t\tstaff(auth.PermLaunchAuthorisationRequest', A_CMD),
  ("A4 queue perm -> request", R, '"GET /v1/admin/launch-authorisations",\n\t\tstaff(auth.PermLaunchAuthorisationRead', '"GET /v1/admin/launch-authorisations",\n\t\tstaff(auth.PermLaunchAuthorisationRequest', A_CMD),
  ("A5 wrapper drops RequireStaffPrincipal", R, 'auth.RequireStaffPrincipal(auth.RequirePermission(perm)(h))', 'auth.RequirePermission(perm)(h)', A_CMD),
  ("A6 foreign tenant 404 -> 403", R, 'apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")\n\t\treturn c, false', 'apierror.Write(w, c.requestID, apierror.CodeForbidden, "not found")\n\t\treturn c, false', A_CMD),
  ("A7 H-6 mapping -> precondition", R, 'return apierror.CodeLaunchWithdrawalsHolding, "closure refused: withdrawal requests still hold player funds", preCodes(err, &pre)', 'return apierror.CodeLaunchPreconditionFailed, "closure refused: withdrawal requests still hold player funds", preCodes(err, &pre)', A_CMD),
  ("A8 LA024 -> LA023 code", R, 'case code == "LA024":', 'case code == "LA02X":', A_CMD),
  ("A9 AP -> guard refused", R, 'case strings.HasPrefix(code, "AP"):\n\t\t\treturn apierror.CodeLaunchProofRefused', 'case strings.HasPrefix(code, "APX"):\n\t\t\treturn apierror.CodeLaunchProofRefused', A_CMD),
  ("A10 already decided -> self approval", R, 'return apierror.CodeLaunchAlreadyDecided, "this person already decided this request", nil', 'return apierror.CodeLaunchSelfApproval, "this person already decided this request", nil', A_CMD),
  ("A11 not found -> forbidden", R, 'return apierror.CodeNotFound, "not found", nil', 'return apierror.CodeForbidden, "not found", nil', A_CMD),
  ("A12 acting session -> internal", R, 'case errors.Is(err, launchgov.ErrActingSession), errors.Is(err, launchgov.ErrForbidden):', 'case errors.Is(err, launchgov.ErrForbidden):', A_CMD),
  ("A13 GP020 mapping dropped", R, 'case errors.As(err, &blocked):\n\t\treturn apierror.CodeTenantCloseBlockedOpenRounds', 'case errors.As(err, &blocked) && false:\n\t\treturn apierror.CodeTenantCloseBlockedOpenRounds', A_CMD),
  ("A14 lock timeout -> guard refused", R, 'return apierror.CodeLaunchRetryableLockTimeout, "the subject is busy; retry", nil', 'return apierror.CodeLaunchGuardRefused, "the subject is busy; retry", nil', A_CMD),
  ("A15 internal message leaks error text", R, 'c.logger.Error("launch_authorisation_failed", "op", op, "err", err.Error())', 'c.logger.Error("launch_authorisation_failed", "op", op, "err", err.Error()); msg = err.Error()', A_CMD),
  ("A16 refused-at-execution check code dropped", R, '[]string{refused.Code}', 'nil', A_CMD),
  ("A18 no canActOnTenant (route check)", R, 'if !canActOnTenant(tc, target) {', 'if false && !canActOnTenant(tc, target) {', A_CMD),
  ("A17 apierror 409->500 for guard refused", "internal/apierror/apierror.go", 'CodeLaunchPriorRejecter, CodeLaunchNotLegacyBaseline, CodeLaunchRefusedAtExecution, CodeLaunchGuardRefused:', 'CodeLaunchPriorRejecter, CodeLaunchNotLegacyBaseline, CodeLaunchRefusedAtExecution:', "go test -count=1 ./internal/apierror ./internal/httpserver -run 'TestLaunch|TestWrite' 2>&1 | tail -15"),
 ],
 "B": [
  ("B1 no canActOnTenant", R, 'if !canActOnTenant(tc, target) {', 'if false && !canActOnTenant(tc, target) {', B_CMD),
  ("B2 actor scope always platform", R, 'if tc.TenantID != uuid.Nil {\n\t\tc.actor.Scope, c.actor.TenantID = launchgov.ScopeTenant, tc.TenantID\n\t}', '', B_CMD),
  ("B3 create uses body tenant", R, 'sub := launchgov.Subject{TenantID: c.target, BrandID: brandID}', 'sub := launchgov.Subject{TenantID: c.target, BrandID: brandID}\n\t\tif body.TenantID != nil {\n\t\t\tif u, e := uuid.Parse(*body.TenantID); e == nil {\n\t\t\t\tsub.TenantID = u\n\t\t\t}\n\t\t}', B_CMD),
  ("B4 suspend brand from body", R, 'b, err := uuid.Parse(r.PathValue("brandID"))', 'b, err := uuid.Parse(r.PathValue("brandID"))\n\t\t\tif err == nil {\n\t\t\t\tif bb := r.URL.Query().Get("x"); bb != "" {\n\t\t\t\t\tb, _ = uuid.Parse(bb)\n\t\t\t\t}\n\t\t\t}', None),
  ("B5 no disclaimer in results", R, 'launchEnvelope: launchEnvelope{launchgov.Disclaimer},\n\t\tRequest:        toLaunchRequestDTO(res.Request), Readiness:', 'launchEnvelope: launchEnvelope{""},\n\t\tRequest:        toLaunchRequestDTO(res.Request), Readiness:', B_CMD),
  ("B6 get request no current readiness", R, 'if req.Status == launchgov.ReqPending {', 'if false && req.Status == launchgov.ReqPending {', B_CMD),
  ("B7 list ignores status filter", R, 'launchgov.ListFilter{BrandID: brandID, Statuses: statuses, Limit: limit}', 'launchgov.ListFilter{BrandID: brandID, Limit: limit}', B_CMD),
  ("B8 decide drops AlreadyDecided check", "internal/launchgov/decide.go", 'if ap.DecidedByPersonID == meNow {\n\t\t\t\treturn ErrAlreadyDecided', 'if false && ap.DecidedByPersonID == meNow {\n\t\t\t\treturn ErrAlreadyDecided', B_CMD),
  ("B9 queue allows tenant scope", "internal/launchgov/read.go", 'if a.Scope != ScopePlatform {\n\t\treturn nil, fmt.Errorf("%w: the platform-wide queue is for platform staff", ErrForbidden)\n\t}', '', B_CMD),
  ("B10 cancel by id skips subject (tenant staff on tenant subject)", "internal/launchgov/read.go", 'return s.Cancel(ctx, a, pre.Subject(), requestID, reasonCode)', 'return s.Cancel(ctx, a, Subject{TenantID: tenantID, BrandID: nilUUIDPtr()}, requestID, reasonCode)', None),
  ("B11 funds notice swapped", "internal/launchgov/readiness.go", 'if action == ActionClose {\n\t\tr.Notices = append(r.Notices, NoticeFundsStranded)', 'if action != ActionClose {\n\t\tr.Notices = append(r.Notices, NoticeFundsStranded)', BL_CMD),
  ("B12 history ignores brand filter", "internal/launchgov/read.go", 'WHERE tenant_id = $1 AND ($2::uuid IS NULL OR brand_id = $2)\n\t\t\t ORDER BY created_at DESC, id DESC LIMIT $3', 'WHERE tenant_id = $1 AND ($2::uuid IS NULL OR $2::uuid IS NOT NULL)\n\t\t\t ORDER BY created_at DESC, id DESC LIMIT $3', B_CMD),
 ],
 "C": [
  ("C1 no brand check", "migrations/0131_withdrawal_requests_subject_active_guard.up.sql", "IF v_status IS DISTINCT FROM 'active' THEN\n        RAISE EXCEPTION 'withdrawal request refused: brand is not active'", "IF false THEN\n        RAISE EXCEPTION 'withdrawal request refused: brand is not active'", C_CMD),
  ("C2 no tenant check", "migrations/0131_withdrawal_requests_subject_active_guard.up.sql", "IF v_status IS DISTINCT FROM 'active' THEN\n        -- Also fails closed", "IF false THEN\n        -- Also fails closed", C_CMD),
  ("C3 replay short-circuit re-added (security C1 exploit)", "migrations/0131_withdrawal_requests_subject_active_guard.up.sql", "    PERFORM pg_advisory_xact_lock_shared(public.tenant_status_gate_key(NEW.tenant_id));\n    SELECT t.status", "    IF EXISTS (SELECT 1 FROM public.withdrawal_requests x WHERE x.tenant_id = NEW.tenant_id AND x.player_account_id = NEW.player_account_id AND x.idempotency_key = NEW.idempotency_key) THEN RETURN NEW; END IF;\n    PERFORM pg_advisory_xact_lock_shared(public.tenant_status_gate_key(NEW.tenant_id));\n    SELECT t.status", C_CMD),
  ("C4 tenant key not taken", "migrations/0131_withdrawal_requests_subject_active_guard.up.sql", "PERFORM pg_advisory_xact_lock_shared(public.tenant_status_gate_key(NEW.tenant_id));", "NULL;", C_CMD),
  ("C5 brand key not taken", "migrations/0131_withdrawal_requests_subject_active_guard.up.sql", "PERFORM pg_advisory_xact_lock_shared(public.brand_status_gate_key(NEW.brand_id));", "NULL;", C_CMD),
  ("C6 only closed refused", "migrations/0131_withdrawal_requests_subject_active_guard.up.sql", "IF v_status IS DISTINCT FROM 'active' THEN\n        -- Also fails closed", "IF v_status = 'closed' THEN\n        -- Also fails closed", C_CMD),
  ("C7 trigger fires after binding guard", "migrations/0131_withdrawal_requests_subject_active_guard.up.sql", "CREATE TRIGGER withdrawal_requests_aa_subject_active_guard", "CREATE TRIGGER withdrawal_requests_zz_subject_active_guard", C_CMD),
  ("C8 owner exempt", "migrations/0131_withdrawal_requests_subject_active_guard.up.sql", "    PERFORM pg_advisory_xact_lock_shared(public.tenant_status_gate_key(NEW.tenant_id));\n    SELECT t.status", "    IF current_user = 'igaming' THEN RETURN NEW; END IF;\n    PERFORM pg_advisory_xact_lock_shared(public.tenant_status_gate_key(NEW.tenant_id));\n    SELECT t.status", C_CMD),
  ("C9 shared lock -> exclusive-free (no lock for brand) via wrong key", "migrations/0131_withdrawal_requests_subject_active_guard.up.sql", "public.brand_status_gate_key(NEW.brand_id)", "public.brand_status_gate_key(NEW.tenant_id)", C_CMD),
 ],
 "C2": [
  ("C10 deny-delete trigger not created", "migrations/0131_withdrawal_requests_subject_active_guard.up.sql", "CREATE TRIGGER withdrawal_requests_deny_delete\n    BEFORE DELETE ON withdrawal_requests\n    FOR EACH ROW EXECUTE FUNCTION withdrawal_requests_deny_delete();", "", C_CMD),
  ("C11 deny-truncate trigger not created", "migrations/0131_withdrawal_requests_subject_active_guard.up.sql", "CREATE TRIGGER withdrawal_requests_deny_truncate\n    BEFORE TRUNCATE ON withdrawal_requests\n    FOR EACH STATEMENT EXECUTE FUNCTION withdrawal_requests_deny_delete();", "", C_CMD),
 ],
 "E": [
  ("E1 launch audit actor shown to tenants", "internal/httpserver/audit_platform_actions_handlers.go", "if row.actorID != nil && !strings.HasPrefix(row.action, \"launch_authorisation.\") {", "if row.actorID != nil {", B_CMD),
  ("E2 URL key case not folded", "internal/db/idle_bound.go", "lk := strings.ToLower(k)", "lk := k", "go test -count=1 ./internal/db -run 'TestConnectWithRuntimeParams_URLBound' 2>&1 | tail -15"),
  ("E3 options parameter not inspected", "internal/db/idle_bound.go", "if lk == \"options\" {", "if lk == \"options-x\" {", "go test -count=1 ./internal/db -run 'TestConnectWithRuntimeParams_URLBound' 2>&1 | tail -15"),
  ("E4 line/paragraph separators accepted", "internal/launchgov/lint.go", "|| unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)", "", "go test -count=1 ./internal/launchgov -run 'TestLintFreeText' 2>&1 | tail -15"),
  ("E5 hangul fillers accepted", "internal/launchgov/lint.go", "return r == 0x115F || r == 0x1160 || r == 0x3164 || r == 0xFFA0", "return false", "go test -count=1 ./internal/launchgov -run 'TestLintFreeText' 2>&1 | tail -15"),
  ("E6 CI revoke missing", ".github/workflows/ci.yml", "REVOKE DELETE, TRUNCATE ON withdrawal_requests FROM igaming_runtime;", "", "go test -count=1 ./internal/withdrawal -run 'TestDeleteTruncateRevokePinned' 2>&1 | tail -15"),
 ],
 "D": [
  ("D1 tenantView always false", R, "func (c launchCall) tenantView() bool { return c.actor.Scope == launchgov.ScopeTenant }", "func (c launchCall) tenantView() bool { return false }", B_CMD),
  ("D2 request redaction dropped", R, "if c.tenantView() && r.RequestedByScope != launchgov.ScopeTenant {\n\t\td.RequestedBy = \"\"", "if false {\n\t\td.RequestedBy = \"\"", B_CMD),
  ("D3 history redaction dropped", R, "if c.tenantView() && t.ActorType != launchgov.ScopeTenant {", "if false {", B_CMD),
  ("D4 readiness coarsening dropped", R, "d.Environment = \"redacted\"", "d.Environment = d.Environment", B_CMD),
  ("D5 create accepts suspend again", R, "if body.Action == launchgov.ActionSuspend {\n\t\t\t// A suspension has its own routes", "if false && body.Action == launchgov.ActionSuspend {\n\t\t\t// A suspension has its own routes", B_CMD),
  ("D6 wire-shape validation dropped", R, "func validLaunchCreateShape(b launchCreateBody) bool {", "func validLaunchCreateShape(b launchCreateBody) bool {\n\tif true {\n\t\treturn true\n\t}", B_CMD),
  ("D7 decision reason not stored under reason_code", "internal/launchgov/decide.go", "\"reason_code\": in.ReasonCode, \"request_reason_code\": req.ReasonCode, \"decided_by\": a.StaffID.String()})", "\"request_reason_code\": req.ReasonCode, \"decided_by\": a.StaffID.String()})", B_CMD),
  ("D8 format characters accepted", "internal/launchgov/lint.go", "if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {", "if unicode.IsControl(r) {", "go test -count=1 ./internal/launchgov -run 'TestLintFreeText' 2>&1 | tail -15"),
  ("D9 URL may disable the idle timeout", "internal/db/db.go", "if err := checkIdleInTxBound(cur); err != nil {", "if err := checkIdleInTxBound(cur); false && err != nil {", "go test -count=1 ./internal/db -run 'TestConnectWithRuntimeParams_URL' 2>&1 | tail -15"),
  ("D10 idle bound accepts zero", "internal/db/idle_bound.go", "if d <= 0 {", "if d < 0 {", "go test -count=1 ./internal/db -run 'TestCheckIdleInTxBound' 2>&1 | tail -15"),
  ("D11 WD011 not the brand sentinel", "internal/withdrawal/withdrawal.go", "tenant.ErrNotActiveForPaymentInitiation, tenant.ErrBrandNotActive, pgErr.Code)", "tenant.ErrNotActiveForPaymentInitiation, tenant.ErrNotActiveForPaymentInitiation, pgErr.Code)", "go test -count=1 ./internal/withdrawal -run 'TestTranslateInsertError' 2>&1 | tail -15"),
  ("D12 ErrNoPerson mapping dropped", R, "case errors.Is(err, launchgov.ErrNoPerson):", "case false:", A_CMD),
  ("D13 two-asset: zero-netting asset counted", "internal/launchgov/readiness.go", "HAVING SUM(p.credit_total - p.debit_total) <> 0", "HAVING true", BL_CMD),
 ],
}

def main():
    group = sys.argv[1]
    only = sys.argv[2:]
    res = []
    for name, f, old, new, cmd in mutants[group]:
        if cmd is None:
            continue
        if only and not any(name.startswith(o) for o in only):
            continue
        path = os.path.join(WT, f)
        orig = open(path).read()
        if old not in orig:
            res.append((name, "NOT-APPLIED (pattern missing)"))
            continue
        open(path, "w").write(orig.replace(old, new, 1))
        try:
            if group in ("C", "C2"):
                rebuild()
            rc, out = run(cmd)
            killed = ("FAIL" in out) or ("build failed" in out) or ("panic" in out)
            res.append((name, "KILLED" if killed else "SURVIVED"))
            if not killed:
                res[-1] = (name, "SURVIVED :: " + out[-300:].replace("\n", " | "))
        finally:
            open(path, "w").write(orig)
        with open(OUT, "w") as fh:
            for r in res:
                fh.write("%s -> %s\n" % r)
    if group in ("C", "C2"):
        rebuild()
    with open(OUT, "w") as fh:
        for r in res:
            fh.write("%s -> %s\n" % r)
        fh.write("DONE\n")

main()
