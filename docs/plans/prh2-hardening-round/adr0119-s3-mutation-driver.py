# ADR 0119 S3 mutation driver. Usage: python3 -I mutate.py <out_log> [ID ...]
# Each mutant: (id, file, old, new, test packages, -run regex, sql?) . The file is restored with `git checkout --`
# after every mutant (and before the first, so a crash mid-run never leaves a mutant behind).
import os, subprocess, sys, time
WT = os.environ.get('MUT_WT', '/home/user/igaming-platform/.claude/worktrees/agent-a8cb2de225b471305')
S = '/tmp/claude-0/-home-user-igaming-platform/82298384-cc24-5365-b2fc-220688ed9969/scratchpad'
os.chdir(WT)
ENV = dict(os.environ, PRIV_DB=os.environ.get('MUT_DB', 's3mfa_a8cb'), PRIV_SRC=WT)

HS = './internal/httpserver/'
MUTANTS = [
 # --- G1: refusals commit (the closure returns nil on a refusal) ---
 ('M01', 'internal/httpserver/staff_auth_handlers.go',
  '''				out = mfaInvalidCode
				alerts, err = m.recordSecondFactorFailure(ctx, tx, r, requestID, ch, "totp", vr, noFactor, now)
				return err''',
  '''				out = mfaInvalidCode
				alerts, err = m.recordSecondFactorFailure(ctx, tx, r, requestID, ch, "totp", vr, noFactor, now)
				if err == nil {
					err = errors.New("mutant: refusal rolled back")
				}
				return err''', HS, 'TestStaffMFA_G1_', False),
 ('M02', 'internal/httpserver/staff_auth_handlers.go',
  '''				alerts, err = m.recordSecondFactorFailure(ctx, tx, r, requestID, ch, "recovery_code", vr, noFactor, now)
				return err''',
  '''				alerts, err = m.recordSecondFactorFailure(ctx, tx, r, requestID, ch, "recovery_code", vr, noFactor, now)
				if err == nil {
					err = errors.New("mutant")
				}
				return err''', HS, 'TestStaffMFA_RecoveryFlow|TestStaffMFA_G4', False),
 ('M03', 'internal/httpserver/staff_auth_handlers.go',
  '''				alerts, err = m.recordSecondFactorFailure(ctx, tx, r, requestID, ch, "enrol_confirm", cr.VerifyResult, false, now)
				return err''',
  '''				alerts, err = m.recordSecondFactorFailure(ctx, tx, r, requestID, ch, "enrol_confirm", cr.VerifyResult, false, now)
				if err == nil {
					err = errors.New("mutant")
				}
				return err''', HS, 'TestStaffMFA_EnrolConfirm', False),
 # --- G3: challenge consumed with the session ---
 ('M04', 'internal/httpserver/staff_auth_handlers.go',
  '''				if err := m.store.ConsumeChallenge(ctx, tx, ch.ID, now); err != nil {
					return err
				}
				s, err := auth.IssueStaffSession(ctx, tx, m.keys, auth.StaffSessionParams{
					StaffID: ch.Staff.StaffID, TenantID: ch.Staff.TenantID, AMR: staffLoginAMR, Now: now,
					AbsoluteTTL: m.absoluteTTL, IdleTTL: m.idleTTL, UserAgent: r.UserAgent(), IPAddress: clientIP(r),
				})
				if err != nil {
					return err
				}
				if pair, err = m.mintStaffTokens''',
  '''				s, err := auth.IssueStaffSession(ctx, tx, m.keys, auth.StaffSessionParams{
					StaffID: ch.Staff.StaffID, TenantID: ch.Staff.TenantID, AMR: staffLoginAMR, Now: now,
					AbsoluteTTL: m.absoluteTTL, IdleTTL: m.idleTTL, UserAgent: r.UserAgent(), IPAddress: clientIP(r),
				})
				if err != nil {
					return err
				}
				if pair, err = m.mintStaffTokens''', HS, 'TestStaffMFA_FullFlow|TestStaffMFA_Replay|TestStaffMFA_G3', False),
 # --- G7: no factor is the same answer as a wrong code ---
 ('M05', 'internal/httpserver/staff_auth_handlers.go',
  '''			vr, err := m.factor.VerifyDetailed(ctx, tx, ch.Staff, staffmfa.NewTOTPAssertion(req.Code.reveal()), now)
			noFactor := errors.Is(err, store.ErrNoActiveFactor)
			if err != nil && !noFactor {''',
  '''			vr, err := m.factor.VerifyDetailed(ctx, tx, ch.Staff, staffmfa.NewTOTPAssertion(req.Code.reveal()), now)
			noFactor := false
			if err != nil && !noFactor {''', HS, 'TestStaffMFA_G7', False),
 # --- a locked answer spends no attempt and compares nothing ---
 ('M06', 'internal/httpserver/staff_auth_handlers.go',
  '''			case !noFactor && lockedOutcome(vr.Outcome):
				out = mfaLocked
				return auditLocked(ctx, tx, r, requestID, ch.Staff, "verify", vr.Outcome == staffmfa.OutcomeHardLocked)''',
  '''			case !noFactor && lockedOutcome(vr.Outcome):
				out = mfaLocked
				if _, err := m.store.DecrementChallengeAttempts(ctx, tx, ch.ID, now); err != nil {
					return err
				}
				return auditLocked(ctx, tx, r, requestID, ch.Staff, "verify", vr.Outcome == staffmfa.OutcomeHardLocked)''', HS, 'TestStaffMFA_G1_', False),
 # --- ADR 17.8: hard lock still issues a challenge ---
 ('M07', 'internal/httpserver/staff_routes.go',
  '''			case st.HardLocked:
				outcome = outcomeMFALocked
				return auditLocked(ctx, tx, r, requestID, ref, "password", true)
			}''', '''			}''', HS, 'TestStaffMFA_HardLock', False),
 # --- ADR 17.8: recovery code grants something other than an enrol challenge ---
 ('M08', 'internal/httpserver/staff_auth_handlers.go',
  '''				next, err := m.store.IssueChallenge(ctx, tx, ch.Staff, store.ChallengeEnrol, store.OriginRecoveryCode, staffVersionUnversioned, now)''',
  '''				next, err := m.store.IssueChallenge(ctx, tx, ch.Staff, store.ChallengeMFA, store.OriginRecoveryCode, staffVersionUnversioned, now)''', HS, 'TestStaffMFA_RecoveryFlow', False),
 # --- ADR 17.8: seal not verified on refresh ---
 ('M09', 'internal/auth/session.go',
  '''		switch cause := staffSealVerdict(row, policy.Staff.Keys); cause {''',
  '''		switch cause := ""; cause {''', HS, 'TestStaffMFA_SessionSealTamper', False),
 # --- ADR 17.8: cutover rule raises P1 for pre-cutover NULL seals ---
 ('M10', 'internal/auth/staff_session.go',
  '''		if s.cutoverAt != nil && s.createdAt.Before(*s.cutoverAt) {
			return integrityCausePreCutoverLegacy
		}''', '''''', HS, 'TestMigration0138_Cutover', False),
 # --- ADR 17.8: absolute_expires_at not copied on rotation ---
 ('M11', 'internal/auth/staff_session.go',
  '''	a.SessionID = uuid.New()
	a.AMR = append([]string(nil), a.AMR...)''',
  '''	a.SessionID = uuid.New()
	a.AMR = append([]string(nil), a.AMR...)
	a.AbsoluteExpiresAt = staffmfa.SessionTime(now.Add(12 * time.Hour))''', HS, 'TestStaffMFA_RefreshBounds', False),
 ('M12', 'internal/auth/staff_session.go',
  '''	if e.After(absolute) {
		return absolute
	}''', '''''', HS, 'TestStaffMFA_RefreshBounds', False),
 ('M13', 'internal/auth/session.go',
  '''	if staff && !now.Before(*row.staff.absoluteExpiry) {
		return Session{}, ErrSessionNotFound
	}''', '''''', HS, 'TestStaffMFA_RefreshBounds', False),
 # --- claims ---
 ('M14', 'internal/auth/jwt.go', '''	if c.AAL != StaffAAL {''', '''	if false {''', './internal/auth/', 'TestVerify_StaffToken', False),
 ('M15', 'internal/auth/jwt.go',
  '''		if c.SessionID != "" || c.AAL != 0 || c.AMR != nil || c.AuthTime != nil || c.MFAAt != nil || c.StaffVersion != nil {''',
  '''		if false {''', './internal/auth/', 'TestVerify_StaffToken', False),
 ('M16', 'internal/auth/jwt.go',
  '''	if principalType == PrincipalStaff {
		return "", ErrStaffAssuranceRequired
	}
	if !rolePrincipalValid(role, principalType) {''', '''	if !rolePrincipalValid(role, principalType) {''', './internal/auth/', 'TestIssue_RefusesStaffPrincipal', False),
 # --- seal ---
 ('M17', 'internal/staffmfa/session.go', '''+ "|" + us(a.MFAAt) +''', '''+ "|" +''', './internal/staffmfa/', 'TestSealSession', False),
 ('M18', 'internal/staffmfa/session.go', '''	want, err := k.sessionMAC(stored.KID, a)''', '''	want, err := k.sessionMAC(k.m().macActive, a)''', './internal/staffmfa/', 'TestSealSession', False),
 # --- gate f: redaction ---
 ('M19', 'internal/httpserver/staff_auth_secrets.go',
  '''func (redacted[T]) Format(f fmt.State, _ rune)      { _, _ = f.Write([]byte(staffmfa.Redacted)) }''',
  '''func (r redacted[T]) Format(f fmt.State, _ rune)   { _, _ = fmt.Fprint(f, r.reveal()) }''', HS, 'TestStaffMFA_SecretHygiene', False),
 ('M19b', 'internal/httpserver/staff_auth_secrets.go',
  '''func (redacted[T]) LogValue() slog.Value            { return slog.StringValue(staffmfa.Redacted) }''',
  '''func (r redacted[T]) LogValue() slog.Value         { return slog.AnyValue(r.reveal()) }''', HS, 'TestStaffMFA_SecretHygiene', False),
 # --- gate d ---
 ('M20', 'internal/httpserver/staff_auth_handlers.go',
  '''			if err != nil || staff.ID != row.Staff.StaffID || staff.TenantID != row.Staff.TenantID {''',
  '''			if err != nil {''', HS, 'TestStaffMFA_Redeem_GateD', False),
 ('M21', 'internal/httpserver/staff_auth_handlers.go',
  '''			if err != nil || t.ID != ref.Staff.TenantID {''', '''			if err != nil || t.ID == uuid.Nil {''', HS, 'TestStaffMFA_Redeem_GateD', False),
 ('M21b', 'internal/httpserver/staff_auth_handlers.go',
  '''		} else if ref.Staff.TenantID != uuid.Nil {
			refuse()
			return
		}''', '''		}''', HS, 'TestStaffMFA_Redeem_GateD', False),
 # --- staff re-check between steps ---
 ('M22', 'internal/httpserver/staff_mfa.go',
  '''	if s.TenantID != ref.TenantID || s.Status != "active" || versionAtIssue != staffVersionUnversioned {''',
  '''	if s.TenantID != ref.TenantID || versionAtIssue != staffVersionUnversioned {''', HS, 'TestStaffMFA_SuspendedBetweenSteps', False),
 # --- G4: the fault answer is independent of the outcome ---
 ('M23', 'internal/httpserver/staff_auth_handlers.go',
  '''		if err != nil {
			logger.Error("staff_mfa_verify_failed", "error", err)
			writeStaffAuthUnavailable(w, requestID)
			return
		}''',
  '''		if err != nil {
			logger.Error("staff_mfa_verify_failed", "error", err)
			if out == mfaInvalidCode {
				writeMFAOutcome(w, requestID, out)
				return
			}
			writeStaffAuthUnavailable(w, requestID)
			return
		}''', HS, 'TestStaffMFA_G4', False),
 # --- gate e ---
 ('M25', 'internal/httpserver/staff_mfa.go', '''	failurePatternDays   = 3''', '''	failurePatternDays   = 4''', HS, 'TestStaffMFA_GateE', False),
 ('M25b', 'internal/httpserver/staff_mfa.go', '''	failurePatternWindow = "7 days"''', '''	failurePatternWindow = "30 days"''', HS, 'TestStaffMFA_GateE', False),
 # --- confirm side effects ---
 ('M26', 'internal/httpserver/staff_auth_handlers.go',
  '''				if err := auth.RevokeAllSessionsForPrincipal(ctx, tx, auth.PrincipalStaff, ch.Staff.StaffID); err != nil {
					return err
				}''', '''''', HS, 'TestStaffMFA_RecoveryFlow', False),
 ('M28', 'internal/httpserver/ratelimit.go', '''	rateLimitMFAPerMin = 30''', '''	rateLimitMFAPerMin = 31''', HS, 'TestStaffMFA_RateLimit', False),
 ('M29', 'internal/httpserver/staff_routes.go',
  '''				if had {
					alerts = append(alerts''', '''				if false && had {
					alerts = append(alerts''', HS, 'TestStaffMFA_G7', False),
 ('M30', 'internal/httpserver/staff_auth_handlers.go',
  '''		alerts = append(alerts, newStaffAlert(signalMFAHardLocked, ch.Staff, requestID, nil))''', '''''', HS, 'TestStaffMFA_HardLock', False),
 ('M31', 'internal/httpserver/staff_auth_handlers.go',
  '''		alerts = append(alerts, newStaffAlert(signalMFAIntegrity, ch.Staff, requestID, map[string]alerting.AttrValue{"cause": "aead_open_failed"}))''',
  '''''', HS, 'TestStaffMFA_G7', False),
 ('M32', 'internal/httpserver/staff_auth_handlers.go',
  '''				alerts = append(alerts, newStaffAlert(signalMFARecoveryUsed, ch.Staff, requestID, nil))''', '''''', HS, 'TestStaffMFA_RecoveryFlow', False),
 ('M33', 'internal/httpserver/auth_routes.go',
  '''			raiseStaffAlerts(r.Context(), deps.DB, []staffAlert{newStaffAlert(signalSessionIntegrity, staff, requestID,''',
  '''			raiseStaffAlerts(r.Context(), deps.DB, nil); _ = ([]staffAlert{newStaffAlert(signalSessionIntegrity, staff, requestID,''', HS, 'TestStaffMFA_SessionSealTamper', False),
 ('M35', 'internal/httpserver/staff_auth_handlers.go',
  '''		err = m.run(r.Context(), deps.DB, ref.Staff, func(ctx context.Context, tx pgx.Tx) error {
			now, err := dbNow(ctx, tx)
			if err != nil {
				return err
			}
			ch, ok, err := m.lockChallenge(ctx, tx, store.ChallengeMFA, tok, now)
			if err != nil {
				return err
			}
			if !ok {
				out = mfaInvalidChallenge
				return nil
			}
			staff, live, err := liveStaff''',
  '''		err = m.run(r.Context(), deps.DB, ref.Staff, func(ctx context.Context, tx pgx.Tx) error {
			now := time.Now()
			ch, ok, err := m.lockChallenge(ctx, tx, store.ChallengeMFA, tok, now)
			if err != nil {
				return err
			}
			if !ok {
				out = mfaInvalidChallenge
				return nil
			}
			staff, live, err := liveStaff''', HS, 'TestStaffMFA_G1_|TestStaffMFA_HardLock|TestStaffMFA_Replay', False),
 ('M37', 'internal/httpserver/staff_auth_handlers.go',
  '''		ref, tok, err := m.resolveChallenge(r.Context(), deps.DB, store.ChallengeMFA, req.ChallengeToken.reveal())
		if errors.Is(err, store.ErrInvalidChallenge) {
			writeMFAOutcome(w, requestID, mfaInvalidChallenge)
			return
		}
		if err != nil {
			logger.Error("staff_mfa_verify_lookup_failed", "error", err)''',
  '''		ref, tok, err := m.resolveChallenge(r.Context(), deps.DB, store.ChallengeEnrol, req.ChallengeToken.reveal())
		if errors.Is(err, store.ErrInvalidChallenge) {
			writeMFAOutcome(w, requestID, mfaInvalidChallenge)
			return
		}
		if err != nil {
			logger.Error("staff_mfa_verify_lookup_failed", "error", err)''', HS, 'TestStaffMFA_FullFlow', False),
 ('M49', 'internal/httpserver/staff_auth_handlers.go',
  '''func newStaffMFAVerifyHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)''',
  '''func newStaffMFAVerifyHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {''', HS, 'TestStaffMFA_FullFlow', False),
 ('M50', 'internal/httpserver/staff_auth_handlers.go',
  '''			// The challenge is NOT consumed (section 6.2); Begin supersedes any earlier pending factor.
			id, material, err := m.factor.Begin(ctx, tx, ch.Staff, now)''',
  '''			if err := m.store.ConsumeChallenge(ctx, tx, ch.ID, now); err != nil {
				return err
			}
			id, material, err := m.factor.Begin(ctx, tx, ch.Staff, now)''', HS, 'TestStaffMFA_FullFlow', False),
 ('M51', 'internal/httpserver/staff_auth_handlers.go',
  '''	if err := m.store.ConsumeChallenge(ctx, tx, ch.ID, now); err != nil {
		return err
	}
	return staffAudit(ctx, tx, r, requestID, ch.Staff, "staff.login_denied"''',
  '''	return staffAudit(ctx, tx, r, requestID, ch.Staff, "staff.login_denied"''', HS, 'TestStaffMFA_SuspendedBetweenSteps', False),
 ('M52', 'internal/httpserver/staff_auth_handlers.go',
  '''			if !ok || staff.Status != "active" {''', '''			if !ok {''', HS, 'TestStaffMFA_Redeem_GateD', False),
 ('M53', 'internal/httpserver/staff_auth_handlers.go',
  '''				if err := recordAttempt(ctx, tx, row.Staff.TenantID, "staff", identifier, ip, false); err != nil {
					return err
				}
				return staffAudit(ctx, tx, r, requestID, row.Staff, "staff.enrolment_token_rejected", audit.OutcomeFailure,
					map[string]any{"reason": reason, "purpose": string(row.Purpose)})''',
  '''				return staffAudit(ctx, tx, r, requestID, row.Staff, "staff.enrolment_token_rejected", audit.OutcomeFailure,
					map[string]any{"reason": reason, "purpose": string(row.Purpose)})''', HS, 'TestStaffMFA_Redeem_GateD', False),
 ('M54', 'internal/httpserver/staff_auth_handlers.go',
  '''			if locked {
				out = mfaPasswordLockout''', '''			if false && locked {
				out = mfaPasswordLockout''', HS, 'TestStaffMFA_Redeem_Lockout', False),
 ('M55', 'internal/auth/session.go',
  '''	if errors.Is(err, ErrSessionNotFound) {
		// Already revoked''', '''	if false && errors.Is(err, ErrSessionNotFound) {
		// Already revoked''', HS, 'TestStaffMFA_SecretHygiene', False),
 ('M56', 'cmd/seed-admin/main.go', '''		if state.Exists {
			return errHasActiveFactor
		}''', '''		if state.Exists && false {
			return errHasActiveFactor
		}''', './cmd/seed-admin/', 'TestSeedAdmin', False),
 ('M57', 'cmd/platform-api/main.go',
  '''	staffMFAKeys, err := staffmfa.LoadKeys(cfg.StaffMFAFactorActiveKID, cfg.StaffMFAFactorKeys.Reveal(),
		cfg.StaffMFAMACActiveKID, cfg.StaffMFAMACKeys.Reveal())
	if err != nil {
		return fmt.Errorf("staff MFA keys: %w", err)
	}''',
  '''	staffMFAKeys, _ := staffmfa.NewKeysFromConfig(cfg.StaffMFAFactorActiveKID, cfg.StaffMFAFactorKeys.Reveal(),
		cfg.StaffMFAMACActiveKID, cfg.StaffMFAMACKeys.Reveal())''', './cmd/platform-api/', 'TestStaffMFAStartupGate', False),
 # --- SQL (migration 0138; the private DB is rebuilt from the mutated file) ---
 ('Q01', 'migrations/0138_staff_session_cutover.up.sql',
  '''            IF NEW.amr IS NULL THEN v_cols := v_cols || 'amr'::text; END IF;''', '''''', HS, 'TestSessions_SF040Matrix', True),
 ('Q02', 'migrations/0138_staff_session_cutover.up.sql',
  '''        IF NEW.auth_time IS DISTINCT FROM OLD.auth_time THEN v_cols := v_cols || 'auth_time'::text; END IF;''', '''''', HS, 'TestSessions_SF040Matrix', True),
 ('Q03', 'migrations/0138_staff_session_cutover.up.sql',
  '''        IF (OLD.assurance_seal IS NOT NULL AND NEW.assurance_seal IS NULL)
           OR (OLD.assurance_kid IS NOT NULL AND NEW.assurance_kid IS NULL) THEN''',
  '''        IF false THEN''', HS, 'TestStaffMFA_SessionSealTamper', True),
 ('Q04', 'migrations/0138_staff_session_cutover.up.sql',
  '''    IF NEW.failed_24h_count < OLD.failed_24h_count
       AND NOT (OLD.failed_24h_start IS NOT NULL AND OLD.failed_24h_start <= now() - interval '24 hours') THEN''',
  '''    IF false THEN''', HS, 'TestFactorLockoutClock_SF008', True),
 ('Q05', 'migrations/0138_staff_session_cutover.up.sql',
  '''    IF OLD.failed_24h_start IS NOT NULL AND NEW.failed_24h_start IS NOT NULL AND NEW.failed_24h_start < OLD.failed_24h_start THEN''',
  '''    IF false THEN''', HS, 'TestFactorLockoutClock_SF008', True),
 ('Q06', 'migrations/0138_staff_session_cutover.up.sql',
  '''OLD.failed_24h_start <= now() - interval '24 hours')''', '''OLD.failed_24h_start <= now() - interval '1 hour')''', HS, 'TestFactorLockoutClock_SF008', True),
 ('Q07', 'migrations/0138_staff_session_cutover.up.sql',
  '''    NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
    AND NULLIF(current_setting('app.player_account_id', true), '') IS NULL''',
  '''    NULLIF(current_setting('app.player_account_id', true), '') IS NULL''', HS, 'TestGrants_SessionsAndCutover', True),
 ('Q08', 'migrations/0138_staff_session_cutover.up.sql',
  '''        EXECUTE 'GRANT UPDATE (revoked_at, last_used_at, replaced_by_session_id) ON sessions TO igaming_runtime';''',
  '''        EXECUTE 'GRANT UPDATE (revoked_at, last_used_at, replaced_by_session_id, aal) ON sessions TO igaming_runtime';''', HS, 'TestGrants_SessionsAndCutover|TestStaffMFA_SessionSealTamper', True),
 ('Q09', 'migrations/0138_staff_session_cutover.up.sql',
  '''    UPDATE public.sessions SET revoked_at = now() WHERE principal_type = 'staff' AND revoked_at IS NULL;
    GET DIAGNOSTICS v_done = ROW_COUNT;''',
  '''    v_done := v_pre;''', HS, 'TestMigration0138_Cutover', False),
 ('Q10', 'migrations/0138_staff_session_cutover.up.sql',
  '''    AND (absolute_expires_at IS NULL OR expires_at <= absolute_expires_at)''', '''''', HS, 'TestSessions_SF040Matrix', True),
 ('Q11', 'migrations/0138_staff_session_cutover.up.sql',
  '''    (aal IS NULL OR aal = 2)''', '''    (aal IS NULL OR aal BETWEEN 1 AND 3)''', HS, 'TestSessions_SF040Matrix', True),
 # ===== review round (security C1-C3, R1-R4; code review F1, F2, F9) =====
 # --- code review X2 (enrol/confirm liveness), X3 (recover liveness), X6 (enrol/start liveness), X7 (enrol/confirm G4) ---
 ('X3', 'internal/httpserver/staff_auth_handlers.go',
  '''			if _, live, err := liveStaff(ctx, tx, ch.Staff, ch.StaffVersionAtIssue); err != nil || !live {
				if err != nil {
					return err
				}
				out = mfaNotActive
				return m.refuseNotActive(ctx, tx, r, requestID, ch, now)
			}
			vr, err := m.store.ConsumeRecoveryCode(''',
  '''			if _, live, err := liveStaff(ctx, tx, ch.Staff, ch.StaffVersionAtIssue); err != nil || (false && !live) {
				if err != nil {
					return err
				}
				out = mfaNotActive
				return m.refuseNotActive(ctx, tx, r, requestID, ch, now)
			}
			vr, err := m.store.ConsumeRecoveryCode(''', HS, 'TestStaffMFA_SuspendedBetweenSteps', False),
 ('X6', 'internal/httpserver/staff_auth_handlers.go',
  '''			if _, live, err := liveStaff(ctx, tx, ch.Staff, ch.StaffVersionAtIssue); err != nil || !live {
				if err != nil {
					return err
				}
				out = mfaNotActive
				return m.refuseNotActive(ctx, tx, r, requestID, ch, now)
			}
			// The challenge is NOT consumed''',
  '''			if _, live, err := liveStaff(ctx, tx, ch.Staff, ch.StaffVersionAtIssue); err != nil || (false && !live) {
				if err != nil {
					return err
				}
				out = mfaNotActive
				return m.refuseNotActive(ctx, tx, r, requestID, ch, now)
			}
			// The challenge is NOT consumed''', HS, 'TestStaffMFA_SuspendedBetweenSteps', False),
 ('X2', 'internal/httpserver/staff_auth_handlers.go',
  '''			if !live {
				out = mfaNotActive
				return m.refuseNotActive(ctx, tx, r, requestID, ch, now)
			}
			cr, err := m.factor.ConfirmDetailed(''',
  '''			if false && !live {
				out = mfaNotActive
				return m.refuseNotActive(ctx, tx, r, requestID, ch, now)
			}
			cr, err := m.factor.ConfirmDetailed(''', HS, 'TestStaffMFA_SuspendedBetweenSteps', False),
 ('X7', 'internal/httpserver/staff_auth_handlers.go',
  '''		if err != nil {
			logger.Error("staff_enrol_confirm_failed", "error", err)
			writeStaffAuthUnavailable(w, requestID)
			return
		}''',
  '''		if err != nil {
			logger.Error("staff_enrol_confirm_failed", "error", err)
			if out == mfaInvalidCode {
				writeMFAOutcome(w, requestID, out)
				return
			}
			writeStaffAuthUnavailable(w, requestID)
			return
		}''', HS, 'TestStaffMFA_G4_', False),
 # --- C1: role / principal binding ---
 ('C1a', 'internal/auth/jwt.go',
  '''	if !rolePrincipalValid(role, principalType) {
		return "", ErrRolePrincipalMismatch
	}''', '''''', './internal/auth/', 'TestIssue_RefusesStaffRole|TestVerify_RefusesHandSigned', False),
 ('C1b', 'internal/auth/jwt.go',
  '''	if !rolePrincipalValid(c.Role, c.PrincipalType) {
		return fmt.Errorf("%w: %v", ErrInvalidToken, ErrRolePrincipalMismatch)
	}''', '''''', './internal/auth/', 'TestVerify_RefusesHandSigned', False),
 ('C1c', 'internal/auth/jwt.go',
  '''	case PrincipalPlayer, PrincipalService:
		return role == RolePlayer''', '''	case PrincipalPlayer, PrincipalService:
		return role != ""''', './internal/auth/', 'TestIssue_RefusesStaffRole|TestVerify_RefusesHandSigned', False),
 ('C1d', 'internal/auth/jwt.go',
  '''	case PrincipalStaff:
		return role != "" && role != RolePlayer''', '''	case PrincipalStaff:
		return role != ""''', './internal/auth/', 'TestIssue_RefusesStaffRole|TestVerify_RefusesHandSigned', False),
 ('C1e', 'internal/auth/jwt.go',
  '''	default:
		return false
	}
}''', '''	default:
		return role == RolePlayer
	}
}''', './internal/auth/', 'TestIssue_RefusesStaffRole|TestVerify_RefusesHandSigned', False),
 ('C1f', 'internal/auth/jwt.go',
  '''	if !rolePrincipalValid(c.Role, c.PrincipalType) {
		return fmt.Errorf("%w: %v", ErrInvalidToken, ErrRolePrincipalMismatch)
	}''', '''''', HS, 'TestRolePrincipalBinding|TestSettlementSimulate_403ForServicePrincipal', False),
 # --- C2: seed-admin uses the config rules ---
 ('C2a', 'cmd/seed-admin/main.go',
  '''	if err := config.ValidateStaffMFAEnv(); err != nil {
		return fmt.Errorf("staff MFA configuration: %w", err)
	}''', '''	_ = config.ValidateStaffMFAEnv''', './cmd/seed-admin/', 'TestSeedAdmin', False),
 ('C2b', 'internal/config/staff_mfa.go',
  '''	if err := cfg.validateStaffMFA(); err != nil {
		return err
	}
	return cfg.validatePlatformKeyFamilies()
}''', '''	if err := cfg.validateStaffMFA(); err != nil {
		return err
	}
	return nil
}''', './internal/config/', 'TestValidateStaffMFAEnv', False),
 ('C2c', 'internal/config/staff_mfa.go',
  '''	cfg.PayoutInstrumentFPKeys = NewSecretValue(os.Getenv("PAYOUT_INSTRUMENT_FP_KEYS"))
}''', '''}''', './internal/config/', 'TestValidateStaffMFAEnv', False),
 # --- R3: bootstrap with an active factor ---
 ('R3a', 'internal/httpserver/staff_auth_handlers.go',
  '''			if row.Purpose == store.EnrolBootstrap {
				st, err := m.factor.ActiveFactorState(ctx, tx, row.Staff, now)''',
  '''			if false && row.Purpose == store.EnrolBootstrap {
				st, err := m.factor.ActiveFactorState(ctx, tx, row.Staff, now)''', HS, 'TestStaffMFA_Redeem_', False),
 ('R3b', 'internal/httpserver/staff_auth_handlers.go',
  '''					alerts = append(alerts, newStaffAlert(signalMFAIntegrity, row.Staff, requestID,
						map[string]alerting.AttrValue{"cause": "bootstrap_with_active_factor"}))''', '''''', HS, 'TestStaffMFA_Redeem_', False),
 # --- R4: operator / reason ---
 ('R4a', 'cmd/seed-admin/main.go',
  '''	if o.Reason != "" {
		m["reason"] = o.Reason
	}''', '''''', './cmd/seed-admin/', 'TestSeedAdmin', False),
 ('R4b', 'cmd/seed-admin/main.go',
  '''			if required {
				return fmt.Errorf("%s is required with -issue-bootstrap (recorded in the audit row)", name)
			}''', '''''', './cmd/seed-admin/', 'TestSeedAdmin', False),
 # --- R1 (SQL): write-once revocation and chain link; expires_at immutable ---
 ('Q12', 'migrations/0138_staff_session_cutover.up.sql',
  '''        IF OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at THEN v_cols := v_cols || 'revoked_at'::text; END IF;''',
  '''''', HS, 'TestSessions_SF040Matrix|TestStaffMFA_RevocationAndChainLink', True),
 ('Q13', 'migrations/0138_staff_session_cutover.up.sql',
  '''        IF OLD.replaced_by_session_id IS NOT NULL AND NEW.replaced_by_session_id IS DISTINCT FROM OLD.replaced_by_session_id THEN''',
  '''        IF OLD.replaced_by_session_id IS NOT NULL AND NEW.replaced_by_session_id IS NOT NULL AND NEW.replaced_by_session_id IS DISTINCT FROM OLD.replaced_by_session_id THEN''',
  HS, 'TestSessions_SF040Matrix|TestStaffMFA_RevocationAndChainLink', True),
 ('Q14', 'migrations/0138_staff_session_cutover.up.sql',
  '''        IF NEW.expires_at IS DISTINCT FROM OLD.expires_at THEN v_cols := v_cols || 'expires_at'::text; END IF;''',
  '''''', HS, 'TestSessions_SF040Matrix', True),
 ('Q15', 'migrations/0138_staff_session_cutover.up.sql',
  '''        IF OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS DISTINCT FROM OLD.revoked_at THEN v_cols := v_cols || 'revoked_at'::text; END IF;''',
  '''        IF OLD.revoked_at IS NOT NULL AND NEW.revoked_at IS NULL THEN v_cols := v_cols || 'revoked_at'::text; END IF;''',
  HS, 'TestSessions_SF040Matrix', True),
 ('Q16', 'migrations/0138_staff_session_cutover.up.sql',
  '''        EXECUTE 'GRANT UPDATE (revoked_at, last_used_at, replaced_by_session_id) ON sessions TO igaming_runtime';''',
  '''        EXECUTE 'GRANT UPDATE (revoked_at, last_used_at, replaced_by_session_id, mfa_at) ON sessions TO igaming_runtime';''',
  HS, 'TestGrants_SessionsAndCutover|TestSessions_SF040Matrix', True),
]

def sh(cmd, log, timeout=3600):
    log.write('$ ' + ' '.join(cmd) + '\n'); log.flush()
    p = subprocess.run(cmd, env=ENV, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout, text=True)
    return p.returncode, p.stdout

def main():
    out = open(sys.argv[1], 'a')
    only = set(sys.argv[2:])
    for (mid, path, old, new, pkg, run, sql) in MUTANTS:
        if only and mid not in only:
            continue
        subprocess.run(['git', 'checkout', '--', path], check=True)
        src = open(path).read()
        if src.count(old) != 1:
            out.write('%s ERROR (anchor found %d times in %s)\n' % (mid, src.count(old), path)); out.flush()
            continue
        open(path, 'w').write(src.replace(old, new))
        t0 = time.time()
        try:
            if sql:
                rc, txt = sh(['bash', S + '/priv_db.sh'], open(os.devnull, 'w'), 900)
                if rc != 0:
                    verdict = 'KILLED (migration refused to apply: %s)' % txt.strip().splitlines()[-1][:160]
                    raise StopIteration
            rc, txt = sh(['bash', S + '/priv_test.sh', '-tags', 'integration', '-p', '1', '-count=1', pkg, '-run', run], open(os.devnull, 'w'), 3000)
            fails = [l for l in txt.splitlines() if l.startswith('--- FAIL') or 'build failed' in l or l.startswith('panic')]
            if rc == 0:
                verdict = 'SURVIVED'
            elif any('build failed' in l for l in fails) or '[setup failed]' in txt:
                verdict = 'ERROR (build failed)'
            else:
                verdict = 'KILLED by ' + ', '.join(sorted(set(l.split()[2] for l in fails if l.startswith('--- FAIL')))[:4])
        except StopIteration:
            pass
        except subprocess.TimeoutExpired:
            verdict = 'ERROR (timeout)'
        finally:
            subprocess.run(['git', 'checkout', '--', path], check=True)
        out.write('%s %s [%s -run %s] %.0fs\n' % (mid, verdict, pkg, run, time.time() - t0)); out.flush()
    # leave the private DB built from the committed migrations
    if any(m[6] for m in MUTANTS if not only or m[0] in only):
        sh(['bash', S + '/priv_db.sh'], open(os.devnull, 'w'), 900)
        out.write('private DB rebuilt from the committed migrations\n'); out.flush()

main()
