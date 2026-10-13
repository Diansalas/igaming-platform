# ADR 0119 S4a mutation driver. Usage: python3 -I adr0119-s4a-mutation-driver.py <out_log> [--check] [ID ...]
# Each mutant: (id, file, old, new, test package, -run regex). The driver runs in a COPY of the worktree (MUT_WT; no
# .git) against its own private database (MUT_DB), so the main worktree and its test database are never touched.
# The original file content is written back after every mutant (and checked before the first). --check only verifies
# that every anchor occurs exactly once. S4a has no migration, so there are no SQL mutants.
import os, subprocess, sys, time
WT = os.environ.get('MUT_WT', '/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad/s4a_mut_src')
S = '/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad'
os.chdir(WT)
ENV = dict(os.environ, PRIV_DB=os.environ.get('MUT_DB', 's4a_mut_a4112'), PRIV_SRC=WT, GOFLAGS='-buildvcs=false')

HS = './internal/httpserver/'
AU = './internal/auth/'
LIVE = 'TestStaffLiveness|TestStaffRefresh_RechecksFactor|TestQueryRowPrincipalScopeReadOnly|TestStaffRoute|TestLiveIfStaff'
STATIC = 'TestStaffRoute|TestLiveIfStaff|TestStaffLivenessVerdict|TestStaffLivenessSQL'
AUTH = 'TestStaffRoles|TestStaffPrincipal|TestMiddleware|TestRequireAAL2|TestIssueStaff|TestVerify|TestRolePrincipal'
SL = 'internal/httpserver/staff_liveness.go'
CHAIN = 'TestStaffLiveness_SessionEndRevokesWholeChain'
WD = 'TestWithdrawalApprove_InactiveStaffAccountRejected|TestWithdrawalSubmit_SuspendedStaffAccountRejected|TestWithdrawalResolve_SuspendedStaffAccountRejected'
SR = 'internal/httpserver/staff_route.go'
MUTANTS = [
 # --- the liveness verdict: each refusal rule (ADR 17.8 "liveness skips the sv comparison" = L05) ---
 ('L01', SL, '''	case row.revoked:
		return observability.StaffLivenessReasonRevoked
''', '', HS, LIVE),
 ('L02', SL, '''	case row.absoluteExpired:
		return observability.StaffLivenessReasonAbsoluteExpired
''', '', HS, LIVE),
 ('L03', SL, '''	case row.status != "active":''', '''	case row.status == "deleted":''', HS, LIVE),
 ('L04', SL, '''	case auth.Role(row.role) != tokenRole:''', '''	case auth.Role(row.role) == "":''', HS, LIVE),
 ('L05', SL, '''	case row.staffVersion != tokenVersion:''', '''	case row.staffVersion < 0:''', HS, LIVE),
 ('L06', SL, '''	case !row.factorUsable:''', '''	case false:''', HS, LIVE),
 # --- the query ---
 ('L07', SL, '''	if errors.Is(err, pgx.ErrNoRows) {
		return observability.StaffLivenessReasonNoSession, nil
	}''', '''	if errors.Is(err, pgx.ErrNoRows) {
		return observability.StaffLivenessReasonOK, nil
	}''', HS, LIVE),
 ('L08', SL, '''const usableFactorPredicate = `f.status = 'active' AND f.hard_locked_at IS NULL`''',
  '''const usableFactorPredicate = `f.status = 'active'`''', HS, LIVE),
 ('L09', SL, '''const usableFactorPredicate = `f.status = 'active' AND f.hard_locked_at IS NULL`''',
  '''const usableFactorPredicate = `f.hard_locked_at IS NULL`''', HS, LIVE),
 ('L10', SL, '''       (s.absolute_expires_at IS NULL OR s.absolute_expires_at <= now()),''',
  '''       (s.absolute_expires_at IS NULL),''', HS, LIVE),
 ('L11', SL, '''SELECT s.revoked_at IS NOT NULL,''', '''SELECT false,''', HS, LIVE),
 ('L12', SL, ''' WHERE s.id = $1 AND s.principal_id = $2 AND s.principal_type = 'staff\'''',
  ''' WHERE s.principal_id = $2 AND s.principal_type = 'staff' AND (s.id = $1 OR true)''', HS, LIVE),
 ('L13', SL, ''' WHERE s.id = $1 AND s.principal_id = $2 AND s.principal_type = 'staff\'''',
  ''' WHERE s.id = $1 AND $2::uuid IS NOT NULL AND s.principal_type = 'staff\'''', HS, LIVE),
 ('L14', SL, '''   AND s.tenant_id IS NOT DISTINCT FROM $3::uuid
   AND su.tenant_id IS NOT DISTINCT FROM $3::uuid`''', '''   AND $3::uuid IS NOT DISTINCT FROM $3::uuid`''', HS, LIVE),
 # --- fail closed ---
 ('L15', SL, '''			if err != nil {
				if ctx.Err() != nil {''', '''			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			if false {
				if ctx.Err() != nil {''', HS, LIVE + '|' + STATIC),
 ('L16', SL, '''			if tcErr != nil || !ok || tc.PrincipalType != string(auth.PrincipalStaff) || idErr != nil || staffID == uuid.Nil {''',
  '''			if tcErr != nil || (!ok && false) || idErr != nil || staffID == uuid.Nil {''', HS, LIVE + '|' + STATIC + '|TestRequireLiveStaffSession'),
 ('L17', SL, '''	if pool == nil {
		return "", errStaffLivenessNoDB
	}''', '''	if pool == nil {
		return observability.StaffLivenessReasonOK, nil
	}''', HS, STATIC),
 # --- the db helper ---
 ('L18', 'internal/db/principal_readonly.go', '''	if _, err := tx.Exec(ctx, `SELECT set_config('statement_timeout', $1, true)`, strconv.FormatInt(statementTimeout.Milliseconds(), 10)+"ms"); err != nil {
		return fmt.Errorf("db: set read-only statement timeout: %w", err)
	}
	b := &pgx.Batch{}''', '''	_ = strconv.FormatInt
	b := &pgx.Batch{}''', HS, LIVE),
 # the defect the S4a fault test found: the timeout queued in the same pipeline as the query (does not bound Parse)
 ('L18b', 'internal/db/principal_readonly.go', '''	if _, err := tx.Exec(ctx, `SELECT set_config('statement_timeout', $1, true)`, strconv.FormatInt(statementTimeout.Milliseconds(), 10)+"ms"); err != nil {
		return fmt.Errorf("db: set read-only statement timeout: %w", err)
	}
	b := &pgx.Batch{}''', '''	b := &pgx.Batch{}
	b.Queue(`SELECT set_config('statement_timeout', $1, true)`, strconv.FormatInt(statementTimeout.Milliseconds(), 10)+"ms")''', HS, LIVE),
 ('L19', 'internal/db/principal_readonly.go', '''pgx.TxOptions{AccessMode: pgx.ReadOnly}''', '''pgx.TxOptions{}''', HS, LIVE),
 ('L20', 'internal/db/principal_readonly.go', '''		if errors.Is(scanErr, pgx.ErrNoRows) {''', '''		if false && errors.Is(scanErr, pgx.ErrNoRows) {''', HS, LIVE),
 ('L20b', 'internal/db/principal_readonly.go', '''			return fmt.Errorf("db: read-only principal-scope batch: %v; %w", scanErr, closeErr)''',
  '''			return fmt.Errorf("db: read-only principal-scope batch: %w; %v", scanErr, closeErr)''', HS, LIVE),
 ('L21', 'internal/db/principal_readonly.go', '''	b.Queue(`SELECT set_config('app.principal_id', $1, true)`, principalID.String())
''', '', HS, LIVE),
 ('L22', 'internal/db/principal_readonly.go', '''	if statementTimeout < time.Millisecond || statementTimeout > 10*time.Second {''',
  '''	if statementTimeout < 0 {''', HS, LIVE),
 # --- the chain ---
 ('L23', SR, '''return auth.Middleware(deps.AuthIssuer)(auth.RequireStaffPrincipal(requireLiveStaffSession(deps)(auth.RequireAAL2(inner))))''',
  '''return auth.Middleware(deps.AuthIssuer)(auth.RequireStaffPrincipal(auth.RequireAAL2(inner)))''', HS, LIVE + '|' + STATIC),
 ('L24', SR, '''return auth.Middleware(deps.AuthIssuer)(auth.RequireStaffPrincipal(requireLiveStaffSession(deps)(auth.RequireAAL2(inner))))''',
  '''return auth.Middleware(deps.AuthIssuer)(auth.RequireStaffPrincipal(requireLiveStaffSession(deps)(inner)))''', HS, STATIC),
 ('L25', SR, '''return auth.Middleware(deps.AuthIssuer)(auth.RequireStaffPrincipal(requireLiveStaffSession(deps)(auth.RequireAAL2(inner))))''',
  '''return auth.Middleware(deps.AuthIssuer)(requireLiveStaffSession(deps)(auth.RequireAAL2(inner)))''', HS, STATIC),
 ('L26', SR, '''		if isStaffPrincipal(r) {
			staff.ServeHTTP(w, r)''', '''		if false {
			staff.ServeHTTP(w, r)''', HS, STATIC),
 ('L27', SR, '''	if !class.valid() {
		panic(fmt.Sprintf("httpserver: staffRoute: invalid step-up class %+v", class))
	}''', '''	_ = fmt.Sprintf''', HS, STATIC),
 ('L28', SR, '''		return c.static == "" && routeClassifiers[c.classifier]''', '''		return routeClassifiers[c.classifier]''', HS, STATIC),
 # --- registrations: a reclassified route and a hand-registered route ---
 ('L29', 'internal/httpserver/capability_routes.go', '''staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermCapabilityGrantApprove)''',
  '''staffRoute(deps, stepUpFinancial, auth.RequirePermission(auth.PermCapabilityGrantApprove)''', HS, STATIC),
 ('L30', 'internal/httpserver/routes.go', '''	mux.Handle("POST /v1/admin/tenants/{tenantID}/staff",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermStaffManage)(newCreateStaffHandler(deps))))''',
  '''	mux.Handle("POST /v1/admin/tenants/{tenantID}/staff",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermStaffManage)(newCreateStaffHandler(deps))))''', HS, STATIC),
 ('L31', 'internal/httpserver/routes.go', '''	mux.Handle("GET /v1/me/sessions", liveIfStaff(deps, newListSessionsHandler(deps)))''',
  '''	mux.Handle("GET /v1/me/sessions", auth.Middleware(deps.AuthIssuer)(newListSessionsHandler(deps)))''', HS, STATIC),
 # --- refresh re-check (S3-R11) ---
 ('L32', 'internal/httpserver/auth_routes.go', '''				if !usable {
					return errPrincipalNotActive
				}''', '''				_ = usable''', HS, LIVE),
 # --- auth: closed staff role set (S3-R12), staff session context, RequireAAL2 ---
 ('A01', 'internal/auth/jwt.go', '''		return IsStaffRole(role)''', '''		return role != "" && role != RolePlayer''', AU, AUTH),
 ('A02', 'internal/auth/jwt.go', '''		return IsStaffRole(role)''', '''		return role != "" && role != RolePlayer''', HS, STATIC),
 ('A03', 'internal/auth/staff_chain.go', '''		if err != nil || tc.PrincipalType != string(PrincipalStaff) || !ok || s.AAL != StaffAAL {''',
  '''		if err != nil || tc.PrincipalType != string(PrincipalStaff) || !ok || s.AAL < 0 {''', AU, AUTH),
 ('A04', 'internal/auth/staff_chain.go', '''		if err != nil || tc.PrincipalType != string(PrincipalStaff) || !ok || s.AAL != StaffAAL {''',
  '''		if err != nil || tc.PrincipalType != string(PrincipalStaff) || s.AAL != StaffAAL && ok {''', AU, AUTH),
 ('A05', 'internal/auth/staff_chain.go', '''		if err != nil || tc.PrincipalType != string(PrincipalStaff) || !ok || s.AAL != StaffAAL {''',
  '''		if err != nil || tc.PrincipalType == "never" || !ok || s.AAL != StaffAAL {''', AU, AUTH),
 ('A06', 'internal/auth/middleware.go', '''			if s, ok := staffSessionFromClaims(claims); ok {
				ctx = withStaffSession(ctx, s)
			}''', '', HS, LIVE),
 ('A07', 'internal/auth/staff_chain.go', '''	if c.PrincipalType != PrincipalStaff || c.StaffVersion == nil || c.AuthTime == nil || c.MFAAt == nil {''',
  '''	if c.StaffVersion == nil || c.AuthTime == nil || c.MFAAt == nil {''', AU, AUTH),
 ('A08', 'internal/auth/staff_chain.go', '''		AuthTime: c.AuthTime.Time, MFAAt: c.MFAAt.Time, StaffVersion: *c.StaffVersion,''',
  '''		AuthTime: c.AuthTime.Time, MFAAt: c.MFAAt.Time, StaffVersion: 0,''', HS, LIVE),
 ('A09', 'internal/auth/staff_chain.go', '''	out := make([]Role, len(staffRoles))
	copy(out, staffRoles[:])
	return out''', '''	return staffRoles[:]''', AU, AUTH),
 ('A10', 'internal/auth/staff_chain.go', '''	RoleRiskManager, RolePromotionsManager, RoleBonusOperations,
}''', '''	RoleRiskManager, RolePromotionsManager,
}''', AU, AUTH),
 # --- review round (2026-10-13): security C1 (chain revocation), code review CR 1-CR 4 ---
 ('C1a', 'internal/auth/session.go', '''	if principalType == PrincipalStaff {
		return revokeLinkedStaffSessions(ctx, tx, sessionID, ownerPrincipalID)
	}''', '''	_ = principalType''', HS, CHAIN),
 ('C1b', 'internal/auth/session.go', '''JOIN sessions s ON s.id = p.replaced_by_session_id OR s.replaced_by_session_id = c.id''',
  '''JOIN sessions s ON s.id = p.replaced_by_session_id''', HS, CHAIN),
 ('C1c', 'internal/auth/session.go', '''JOIN sessions s ON s.id = p.replaced_by_session_id OR s.replaced_by_session_id = c.id''',
  '''JOIN sessions s ON s.replaced_by_session_id = c.id''', HS, CHAIN),
 ('C1d', 'internal/auth/session.go', '''		if errors.Is(err, ErrSessionNotFound) && row.principalType == PrincipalStaff {''',
  '''		if false && errors.Is(err, ErrSessionNotFound) && row.principalType == PrincipalStaff {''', HS, CHAIN),
 ('C1e', 'internal/auth/session.go', '''			if staff {
				// ADR 0119 S4a (security review C1)''', '''			if false {
				// ADR 0119 S4a (security review C1)''', HS, CHAIN),
 ('C1f', 'internal/auth/session.go', '''		   AND revoked_at IS NULL AND replaced_by_session_id IS NULL AND expires_at > now()''',
  '''		   AND revoked_at IS NULL AND expires_at > now()''', HS, CHAIN),
 ('X1', 'internal/withdrawal/withdrawal.go', '''		if !active {
			return false, ErrApproverInactive
		}''', '''		if !active && false {
			return false, ErrApproverInactive
		}''', HS, WD),
 ('X2', 'internal/httpserver/withdrawal_handlers.go', '''			if !active {
				return withdrawal.ErrApproverInactive
			}

			// Read-only''', '''			if !active && false {
				return withdrawal.ErrApproverInactive
			}

			// Read-only''', HS, WD),
 ('X2b', 'internal/httpserver/withdrawal_handlers.go', '''			if !active {
				return withdrawal.ErrApproverInactive
			}
			eligible = true''', '''			if !active && false {
				return withdrawal.ErrApproverInactive
			}
			eligible = true''', HS, WD),
 ('X3', 'internal/httpserver/routes.go', '''	mux.Handle("POST /v1/admin/tenants/{tenantID}/staff",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermStaffManage)(newCreateStaffHandler(deps))))''',
  '''	mux.Handle("POST\t/v1/admin/tenants/{tenantID}/staff",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermStaffManage)(newCreateStaffHandler(deps))))''', HS, STATIC),
 ('X3b', 'internal/httpserver/routes.go', '''	mux.Handle("POST /v1/admin/tenants/{tenantID}/staff",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermStaffManage)(newCreateStaffHandler(deps))))''',
  '''	mux.Handle("POST  /v1/admin/tenants/{tenantID}/staff",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermStaffManage)(newCreateStaffHandler(deps))))''', HS, STATIC),
 ('X4', 'internal/httpserver/routes.go', '''	mux.Handle("POST /v1/admin/tenants/{tenantID}/staff",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermStaffManage)(newCreateStaffHandler(deps))))''',
  '''	mux.Handle("POST /{version}/admin/tenants/{tenantID}/staff",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermStaffManage)(newCreateStaffHandler(deps))))''', HS, STATIC),
 ('X4b', 'internal/httpserver/routes.go', '''	mux.Handle("POST /v1/admin/tenants/{tenantID}/staff",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermStaffManage)(newCreateStaffHandler(deps))))''',
  '''	mux.Handle("POST api.internal/v1/admin/tenants/{tenantID}/staff",
		auth.Middleware(deps.AuthIssuer)(auth.RequirePermission(auth.PermStaffManage)(newCreateStaffHandler(deps))))''', HS, STATIC),
 ('K1', SL, '''				if ctx.Err() != nil {''', '''				if false && ctx.Err() != nil {''', HS, STATIC + '|TestRequireLiveStaffSession'),
 ('K2', SL, '''"method", r.Method, "route", r.Pattern, "client_ip"''', '''"method", r.Method, "client_ip"''', HS, LIVE + '|TestRequireLiveStaffSession'),
 ('K3', SL, '''				"staff_id", tc.Subject, "token_tenant", scope, "sid", sess.SessionID.String(),''',
  '''				"token_tenant", scope, "sid", sess.SessionID.String(),''', HS, LIVE + '|TestRequireLiveStaffSession'),
]

def sh(cmd, timeout):
    p = subprocess.run(cmd, env=ENV, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout, text=True)
    return p.returncode, p.stdout

def main():
    out = open(sys.argv[1], 'a')
    args = sys.argv[2:]
    check = '--check' in args
    only = set(a for a in args if a != '--check')
    originals = {}
    for (mid, path, old, new, pkg, run) in MUTANTS:
        if only and mid not in only:
            continue
        if path not in originals:
            originals[path] = open(path).read()
        src = originals[path]
        if src.count(old) != 1:
            out.write('%s ERROR (anchor found %d times in %s)\n' % (mid, src.count(old), path)); out.flush()
            continue
        if check:
            out.write('%s anchor ok\n' % mid); out.flush()
            continue
        open(path, 'w').write(src.replace(old, new))
        t0 = time.time()
        try:
            rc, txt = sh(['bash', S + '/priv_test.sh', '-tags', 'integration', '-p', '1', '-count=1', pkg, '-run', run], 3000)
            fails = [l for l in txt.splitlines() if l.startswith('--- FAIL') or 'build failed' in l or l.startswith('panic')]
            if rc == 0:
                verdict = 'SURVIVED'
            elif any('build failed' in l for l in fails) or '[setup failed]' in txt:
                verdict = 'ERROR (build failed)'
            else:
                verdict = 'KILLED by ' + ', '.join(sorted(set(l.split()[2] for l in fails if l.startswith('--- FAIL')))[:5])
        except subprocess.TimeoutExpired:
            verdict = 'ERROR (timeout)'
        finally:
            open(path, 'w').write(originals[path])
        out.write('%s %s [%s -run %s] %.0fs\n' % (mid, verdict, pkg, run, time.time() - t0)); out.flush()

main()
