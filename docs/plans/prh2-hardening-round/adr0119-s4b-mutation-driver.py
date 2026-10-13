# ADR 0119 S4b mutation driver. Usage: python3 -I adr0119-s4b-mutation-driver.py <out_log> [--check] [ID ...]
# Each mutant: (id, file, old, new, test package, -run regex). The driver runs in a COPY of the worktree (MUT_WT; no
# .git) against its own private database (MUT_DB), so the main worktree and its test databases are never touched.
# Go mutants: the original file content is written back after every mutant (and checked before the first). SQL
# mutants (file under migrations/): the private database is rebuilt from the mutated migrations before the test and
# rebuilt again from the restored ones afterwards. --check only verifies that every anchor occurs exactly once.
import os, subprocess, sys, time
WT = os.environ.get('MUT_WT', '/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad/s4b_mut_src')
S = '/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad'
os.chdir(WT)
ENV = dict(os.environ, PRIV_DB=os.environ.get('MUT_DB', 's4b_aa30_mut'), PRIV_SRC=WT, GOFLAGS='-buildvcs=false')

HS, AU, AP, SM, ST, WD, AD, PM, DB = ('./internal/httpserver/', './internal/auth/', './internal/actorproof/', './internal/staffmfa/',
                                      './internal/stepup/', './internal/withdrawal/', './internal/adjustment/', './internal/payments/',
                                      './internal/db/')
STEPUP = 'TestStepUp_'
UNIT_HS = 'TestStepUpClassifiers|TestClassifiers|TestStaffAssuranceVerdict|TestRequireStepUp|TestStatic_StepUp|TestStaffRoutes|TestStaffRoute_'
GATE = 'internal/httpserver/staff_stepup.go'
HND = 'internal/httpserver/staff_stepup_handlers.go'
LIV = 'internal/httpserver/staff_liveness.go'
STP = 'internal/stepup/stepup.go'
AUS = 'internal/auth/stepup.go'
JWT = 'internal/auth/jwt.go'
SES = 'internal/staffmfa/session.go'
SSS = 'internal/auth/staff_session.go'
APS = 'internal/actorproof/stepup.go'
APA = 'internal/actorproof/actorproof.go'
WDG = 'internal/withdrawal/withdrawal.go'
WDH = 'internal/httpserver/withdrawal_handlers.go'
K2 = 'internal/adjustment/execute.go'
K3 = 'internal/payments/manual_resolution.go'
HSEC = 'internal/payments/withdrawal_hold_resolution.go'
DBA = 'internal/db/actor_staff.go'
MIG = 'migrations/0151_staff_step_up.up.sql'

MUTANTS = [
 # --- the freshness rule (internal/stepup) ---
 ('S01', STP, '''	return age >= -ClockSkew && age <= maxAge''', '''	return age >= -ClockSkew && age <= maxAge+time.Minute''', ST, 'TestFresh'),
 ('S02', STP, '''	return age >= -ClockSkew && age <= maxAge''', '''	return age <= maxAge''', ST, 'TestFresh'),
 ('S03', STP, '''	return a.Present() && subject != uuid.Nil && a.Subject == subject && Fresh(a.MFAAt, now, maxAge)''',
  '''	return a.Present() && subject != uuid.Nil && Fresh(a.MFAAt, now, maxAge)''', ST, 'TestAssertion'),
 # --- the gate (17.8: "RequireStepUp uses the token iat instead of mfa_at" -> the claims-only and row-only forms) ---
 ('G01', GATE, '''	mfaAt := live.MFAAt
	if sess.MFAAt.Before(mfaAt) {
		mfaAt = sess.MFAAt
	}''', '''	mfaAt := sess.MFAAt''', HS, 'TestStepUp_ForgedAndIncoherentClaims'),
 ('G02', GATE, '''	if sess.MFAAt.Before(mfaAt) {
		mfaAt = sess.MFAAt
	}''', '', HS, 'TestStepUp_EndToEnd'),
 ('G03', GATE, '''			if !auth.StepUpFresh(a.MFAAt, live.DBNow, maxAge) {''', '''			if !auth.StepUpFresh(a.MFAAt, live.DBNow, maxAge+10*time.Minute) {''', HS, 'TestStepUp_GoldenTable|TestStepUp_Boundaries'),
 ('G04', 'internal/httpserver/staff_auth_routes.go', '''		staffRoute(deps, stepUpCritical, newStaffRecoveryCodesRegenerateHandler(deps)))''',
  '''		staffRoute(deps, stepUpFinancial, newStaffRecoveryCodesRegenerateHandler(deps)))''', HS, 'TestStaffRoutes_GoldenClassification|TestStepUp_GoldenTable'),
 ('G05', GATE, '''		if class.classifier == "" && class.static == auth.StepUpNone {''', '''		if class.classifier == "" && class.static != auth.StepUpCritical {''', HS, 'TestStepUp_GoldenTable'),
 ('G06', GATE, '''				if c == auth.StepUpNone {
					next.ServeHTTP(w, r)
					return
				}''', '', HS, 'TestStepUp_Classifiers'),
 ('G07', GATE, '''			next.ServeHTTP(w, r.WithContext(auth.ContextWithStepUpAssertion(ctx, a)))''', '''			next.ServeHTTP(w, r)''', HS, 'TestStepUp_EndToEnd|TestStepUp_SignerBackstop'),
 ('G08', GATE, '''			a, ok := stepUpAssertionFor(r)
			if !ok {''', '''			a, ok := stepUpAssertionFor(r)
			if !ok && false {''', HS, 'TestRequireStepUp'),
 # --- the four classifiers ---
 ('C01', GATE, '''	if body.Action == launchgov.ActionSuspend {''', '''	if body.Action == launchgov.ActionSuspend || body.Action != "" {''', HS, 'TestClassifiers|TestStepUp_Classifiers'),
 ('C02', GATE, '''	if auth.RoleHasPermission(auth.Role(tc.Role), auth.PermFinancialPolicyAuthor) {''',
  '''	if auth.RoleHasPermission(auth.Role(tc.Role), auth.PermFinancialPolicyAuthor) && false {''', HS, 'TestClassifiers|TestStepUp_Classifiers'),
 ('C03', GATE, '''	if severity == alertResolveCriticalSeverity {
		return true
	}''', '', HS, 'TestClassifiers|TestStepUp_Classifiers'),
 ('C04', GATE, '''var alertResolveCriticalKindPrefixes = []string{"staff."}''', '''var alertResolveCriticalKindPrefixes = []string{}''', HS, 'TestClassifiers|TestStepUp_Classifiers'),
 ('C05', GATE, '''	if errors.Is(err, pgx.ErrNoRows) {
		return auth.StepUpCritical, nil
	}''', '''	if errors.Is(err, pgx.ErrNoRows) {
		return auth.StepUpNone, nil
	}''', HS, 'TestStepUp_Classifiers|TestStepUp_GoldenTable'),
 ('C06', GATE, '''	if len(v) == 0 || bytes.Equal(v, []byte("null")) {''', '''	if len(v) == 0 {''', HS, 'TestClassifiers|TestStepUp_Classifiers'),
 ('C07', GATE, '''	r.Body = io.NopCloser(bytes.NewReader(b))''', '''	r.Body = io.NopCloser(bytes.NewReader(nil))''', HS, 'TestClassifiers|TestStepUp_Classifiers'),
 ('C08', GATE, '''	if !auth.RoleHasPermission(auth.Role(tc.Role), auth.PermAlertManage) || tc.TenantID != uuid.Nil {
		return auth.StepUpNone, nil
	}''', '', HS, 'TestStepUp_Classifiers'),
 # --- the step-up endpoint and the session write ---
 ('H01', AUS, '''		sessionID, next.MFAAt, newSeal.KID, newSeal.MAC)''', '''		sessionID, cur.MFAAt, newSeal.KID, newSeal.MAC)''', HS, 'TestStepUp_EndToEnd'),
 ('H02a', AUS, '''	if revoked || replaced || absolute == nil || !now.Before(*absolute) {''', '''	if absolute == nil || !now.Before(*absolute) {''', HS, 'TestStepUp_EndToEnd|TestStepUp_RacesRevocation'),
 ('H02b', AUS, '''	if revoked || replaced || absolute == nil || !now.Before(*absolute) {''', '''	if absolute == nil {''', HS, 'TestStepUp_EndToEnd|TestStepUp_RacesRevocation'),
 ('H03', AUS, '''	if !keys.VerifySessionSeal(cur, stored) {''', '''	if !keys.VerifySessionSeal(cur, stored) && false {''', HS, 'TestStepUp_'),
 ('H04', HND, '''	if locked {
		// The session row must be visible''', '''	if false {
		// The session row must be visible''', HS, 'TestStepUp_LockoutSharedWithLogin'),
 ('H05', HND, '''	locked := noFactor || !st.Exists || st.HardLocked || st.Lock == staffmfa.LockSoft || st.Lock == staffmfa.LockHard''',
  '''	locked := noFactor || !st.Exists || st.HardLocked''', HS, 'TestStepUp_LockoutSharedWithLogin'),
 ('H06', HND, '''func newStaffStepUpHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)''', '''func newStaffStepUpHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {''', HS, 'TestStepUp_EndToEnd'),
 ('H07', HND, '''		if err := db.SetSessionInternalOpID(ctx, tx, c.session.SessionID); err != nil {''',
  '''		if err := db.SetSessionInternalOpID(ctx, tx, uuid.Nil); err != nil {''', HS, 'TestStepUp_LockoutSharedWithLogin'),
 ('H08', HND, '''			case !noFactor && lockedOutcome(vr.Outcome):
				out = stepUpOutcomeLocked''', '''			case false:
				out = stepUpOutcomeLocked''', HS, 'TestStepUp_LockoutSharedWithLogin'),
 # --- S3-R13 on tokens ---
 ('V01', JWT, '''	if !HasSecondFactorAMR(c.AMR) {
		return fmt.Errorf("%w: staff token amr names no second factor", ErrInvalidToken)
	}''', '', AU, 'TestVerify_StaffAssuranceCoherence'),
 ('V02', JWT, '''	if mfaAt.Before(authTime) {
		return false
	}''', '', AU, 'TestVerify_StaffAssuranceCoherence'),
 ('V03', JWT, '''	if mfaAt.After(now.Add(StepUpClockSkew)) || authTime.After(now.Add(StepUpClockSkew)) {''', '''	if false {''', AU, 'TestVerify_StaffAssuranceCoherence'),
 ('V04', JWT, '''	return !authTime.Before(now.Add(-staffmfa.StaffSessionAbsoluteTTLBound.Max))''', '''	return !authTime.Before(now.Add(-100 * staffmfa.StaffSessionAbsoluteTTLBound.Max))''', AU, 'TestVerify_StaffAssuranceCoherence'),
 # --- liveness verifies the sealed row (S4a-R10) ---
 ('L01', LIV, '''	if !keys.VerifySessionSeal(a, stored) {
		if keys.IsLegacySessionSeal(a, stored) {''', '''	if false {
		if keys.IsLegacySessionSeal(a, stored) {''', HS, 'TestStaffAssuranceVerdict'),
 ('L02', LIV, '''	if a.AAL != auth.StaffAAL || !auth.HasSecondFactorAMR(a.AMR) || a.MFAAt.Before(a.AuthTime) ||
		a.MFAAt.After(row.dbNow.Add(auth.StepUpClockSkew)) || absoluteTTL <= 0 || a.AuthTime.Before(row.dbNow.Add(-absoluteTTL)) {''',
  '''	if a.AAL != auth.StaffAAL {''', HS, 'TestStaffAssuranceVerdict'),
 ('L03', LIV, '''		if keys.IsLegacySessionSeal(a, stored) {
			return observability.StaffLivenessReasonSealLegacy, liveStaffAssurance{}
		}''', '', HS, 'TestStaffAssuranceVerdict'),
 ('L04', LIV, '''			next.ServeHTTP(w, r.WithContext(withLiveStaffAssurance(ctx, live)))''', '''			next.ServeHTTP(w, r.WithContext(withLiveStaffAssurance(ctx, liveStaffAssurance{})))''', HS, 'TestStepUp_GoldenTable'),
 ('SE01', SES, '''	return []byte("ss-v2|" + body + "|" + a.RefreshTokenHash + "|" + us(a.ExpiresAt)), nil''',
  '''	return []byte("ss-v2|" + body + "|" + us(a.ExpiresAt)), nil''', SM, 'TestSealSession'),
 ('SE02', SES, '''	return []byte("ss-v2|" + body + "|" + a.RefreshTokenHash + "|" + us(a.ExpiresAt)), nil''',
  '''	return []byte("ss-v2|" + body + "|" + a.RefreshTokenHash), nil''', SM, 'TestSealSession'),
 ('SE03', SES, '''func (k *Keys) sessionMAC(kid string, a SessionAssurance) ([]byte, error) {
	return k.sessionMACVersion(kid, a, sealV2)''', '''func (k *Keys) sessionMAC(kid string, a SessionAssurance) ([]byte, error) {
	return k.sessionMACVersion(kid, a, sealV1)''', SM, 'TestSealSession'),
 ('SE04', SSS, '''		if keys.IsLegacySessionSeal(a, h) {
			return integrityCauseLegacySealV1
		}''', '''		if keys.IsLegacySessionSeal(a, h) {
			return ""
		}''', HS, 'TestStepUp_LegacyV1SealRetiredQuietly'),
 ('SE05', SSS, '''	a.RefreshTokenHash = hashRefreshToken(token)''', '''	a.RefreshTokenHash = hashRefreshToken(token + "x")''', HS, 'TestStaffMFA_RefreshBounds|TestStepUp_EndToEnd'),
 # --- the signer backstop (17.8 "signer backstop removed") ---
 ('B01', APA, '''	if err := checkStepUp(ctx, c, i.now()); err != nil {
		return err
	}''', '', AP, 'TestAttach_Backstop'),
 ('B01h', APA, '''	if err := checkStepUp(ctx, c, i.now()); err != nil {
		return err
	}''', '', HS, 'TestStepUp_SignerBackstopOverHTTP'),
 ('B02', APS, '''	OpGrantApprove:          stepup.Critical,
''', '', AP, 'TestStepUpOperations|TestCheckStepUp'),
 ('B03', APS, '''	return c.Operation == OpPolicyPropose && c.Scope == ScopeTenant''', '''	return c.Operation == OpPolicyPropose''', AP, 'TestCheckStepUp'),
 ('B04', APS, '''		return staffmfa.StepUpCriticalMaxAgeBound.Max + StepUpInFlightGrace, true''', '''		return staffmfa.StepUpFinancialMaxAgeBound.Max + StepUpInFlightGrace, true''', AP, 'TestCheckStepUp'),
 ('B05', APS, '''!present && testStepUpFallback.Load() && !stepup.IsStaffRequest(ctx) {''', '''!present && testStepUpFallback.Load() {''', HS, 'TestStepUp_SignerBackstopOverHTTP'),
 ('B06', APS, '''	if !ok || !a.FreshFor(c.Actor, now, maxAge) {''', '''	if !ok || !a.Present() {''', AP, 'TestCheckStepUp'),
 # --- the withdrawal policy (9.3) ---
 ('W01', WDG, '''		if !stepUp.FreshFor(approverPrincipalID, now, stepup.PolicyMaxAge) {''', '''		if !stepUp.FreshFor(approverPrincipalID, now, 10*time.Minute) {''', WD, 'TestApprove_RequireStepUp'),
 ('W02', WDG, '''		if isAutomated {
			return false, ErrStepUpRequired
		}''', '', WD, 'TestApprove_RequireStepUp_FreshnessMatrix'),
 ('W03', WDH, '''			auth.WriteStepUpRequired(w, requestID, auth.StepUpPolicyMaxAge)''', '''			apierror.Write(w, requestID, apierror.CodeForbidden, "step-up")''', HS, 'TestStepUp_WithdrawalPolicy'),
 # --- the scoped re-assertion (17.8 "FOR SHARE re-assertion removed") ---
 ('R01', K2, '''	if err := db.CheckActorStaffLive(locked, call.ActorID, call.ActorRole); err != nil {
		return err
	}''', '''	_ = locked''', AD, 'TestS4b_'),
 ('R03', K3, '''	if err := db.CheckActorStaffLive(locked, call.ActorID, call.ActorRole); err != nil {
		return err
	}''', '''	_ = locked''', PM, 'TestS4b_K3'),
 ('R04', HSEC, '''	if err := db.CheckActorStaffLive(locked, call.ActorID, call.ActorRole); err != nil {
		return err
	}''', '''	_ = locked''', PM, 'TestS4b_HSEC'),
 ('R06', WDH, '''`SELECT person_id, status, role FROM staff_users WHERE id = $1 FOR SHARE`''', '''`SELECT person_id, status, role FROM staff_users WHERE id = $1`''', HS, 'TestStepUp_WithdrawalApprove'),
 ('R07', WDH, '''		if status == "active" && role != tokenRole {''', '''		if false {''', HS, 'TestStepUp_WithdrawalApprove'),
 ('R08', DBA, '''		if r.Status == "active" && r.Role == expectedRole {''', '''		if r.Role == expectedRole {''', DB, 'TestCheckActorStaffLive'),
 ('R09', K2, '''	staff := append([]uuid.UUID{req.InitiatedBy, call.ActorID}, approvers...)''', '''	staff := append([]uuid.UUID{req.InitiatedBy}, approvers...)''', AD, 'TestS4b_'),
 # --- migration 0151 (SQL mutant: the private database is rebuilt from the mutated file) ---
 ('P03', MIG, '''        EXECUTE 'GRANT UPDATE (mfa_at, assurance_kid, assurance_seal) ON sessions TO igaming_runtime';''', '', HS, 'TestStepUp_EndToEnd'),
]

def sh(cmd, timeout):
    p = subprocess.run(cmd, env=ENV, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout, text=True)
    return p.returncode, p.stdout

def rebuild():
    rc, txt = sh(['bash', S + '/priv_db.sh'], 1800)
    if rc != 0:
        raise RuntimeError('rebuild failed: ' + txt[-2000:])

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
        sql = path.startswith('migrations/')
        open(path, 'w').write(src.replace(old, new))
        t0 = time.time()
        try:
            if sql:
                rebuild()
            rc, txt = sh(['bash', S + '/priv_test.sh', '-tags', 'integration', '-p', '1', '-count=1', '-timeout', '40m', pkg, '-run', run], 3000)
            fails = [l for l in txt.splitlines() if l.startswith('--- FAIL') or 'build failed' in l or l.startswith('panic')]
            if rc == 0:
                verdict = 'SURVIVED'
            elif any('build failed' in l for l in fails) or '[setup failed]' in txt:
                verdict = 'ERROR (build failed)'
            else:
                verdict = 'KILLED by ' + ', '.join(sorted(set(l.split()[2] for l in fails if l.startswith('--- FAIL')))[:5])
        except subprocess.TimeoutExpired:
            verdict = 'ERROR (timeout)'
        except RuntimeError as e:
            verdict = 'ERROR (%s)' % str(e)[:200]
        finally:
            open(path, 'w').write(originals[path])
            if sql:
                rebuild()
        out.write('%s %s [%s -run %s] %.0fs\n' % (mid, verdict, pkg, run, time.time() - t0)); out.flush()

main()
