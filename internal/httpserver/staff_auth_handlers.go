package httpserver

// ADR 0119 slice S3: the second-factor, recovery and enrolment endpoints
// (sections 6.1-6.3). Every endpoint follows the same shape:
//
//  1. phase 1 (pre-tenant): the presented challenge / enrolment token is
//     resolved by keyed MAC to its row and the staff member's scope;
//  2. phase 2: ONE transaction in that scope locks the row FOR UPDATE, takes
//     its clock from the database, re-checks the staff member, verifies, and
//     records the outcome - success (burn, consumption, session issue) and
//     refusal (failure counters, attempt decrement, audit) alike. A refusal
//     returns nil from the closure so it COMMITS (S3 entry gates G1, G3);
//  3. only after COMMIT succeeded is the outcome answered and any alert
//     raised. Every fault (an error, a serialization failure, a failed
//     COMMIT) is answered by writeStaffAuthUnavailable, the same 503 whatever
//     the verification outcome would have been (gate G4).
//
// Nothing here logs, audits or returns a code, a secret or a token except the
// one intended response body (staff_auth_secrets.go).

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/stafflifecycle"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/store"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// mfaOutcome is the closed set of answers of the second-factor endpoints.
type mfaOutcome int

const (
	mfaUnset mfaOutcome = iota
	mfaInvalidChallenge
	mfaNotActive
	mfaLocked
	mfaInvalidCode
	mfaEnrolmentExpired
	mfaInvalidEnrolmentToken
	mfaPasswordLockout
	mfaSuccess
)

// challengeExpiresIn is the expires_in of every issued challenge (section 7.4).
var challengeExpiresIn = int(staffmfa.ChallengeTTL.Seconds())

var (
	mfaChallengeMethods   = []string{"totp", "recovery_code"}
	enrolChallengeMethods = []string{"totp"}
	staffLoginAMR         = []string{staffmfa.AMRPassword, staffmfa.AMROTP}
)

// writeMFAOutcome answers a non-success outcome with its one fixed code and message.
func writeMFAOutcome(w http.ResponseWriter, requestID string, out mfaOutcome) {
	switch out {
	case mfaInvalidChallenge:
		apierror.Write(w, requestID, apierror.CodeInvalidChallenge, "the challenge is not valid; sign in again")
	case mfaNotActive:
		apierror.Write(w, requestID, apierror.CodeForbidden, "this account cannot currently log in")
	case mfaLocked:
		apierror.Write(w, requestID, apierror.CodeMFALocked, "too many incorrect codes; the authenticator is locked")
	case mfaInvalidCode:
		apierror.Write(w, requestID, apierror.CodeInvalidMFACode, "the code is not valid")
	case mfaEnrolmentExpired:
		apierror.Write(w, requestID, apierror.CodeMFAEnrolmentExpired, "the enrolment expired; start again")
	case mfaInvalidEnrolmentToken:
		apierror.Write(w, requestID, apierror.CodeInvalidEnrolmentToken, "the enrolment token or the credentials are not valid")
	case mfaPasswordLockout:
		apierror.Write(w, requestID, apierror.CodeRateLimited, "too many failed attempts; try again later")
	default:
		writeStaffAuthUnavailable(w, requestID)
	}
}

// staffAudit writes one staff authentication audit row in tx (section 10.6:
// authentication events go to audit.Record only). Metadata never carries a
// code, a secret, a token or a URI (a static test scans these files).
func staffAudit(ctx context.Context, tx pgx.Tx, r *http.Request, requestID string, staff staffmfa.StaffRef, action string, outcome audit.Outcome, metadata map[string]any) error {
	return audit.Record(ctx, tx, audit.Entry{
		TenantID: staff.TenantID, ActorType: audit.ActorStaff, ActorID: staff.StaffID,
		Action: action, TargetType: "staff_user", TargetID: staff.StaffID.String(),
		Outcome: outcome, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID, Metadata: metadata,
	})
}

// resolveChallenge is phase 1. Every token failure is store.ErrInvalidChallenge.
func (m *StaffMFA) resolveChallenge(ctx context.Context, pool *db.Pool, purpose store.ChallengePurpose, raw string) (store.ChallengeRef, staffmfa.Token, error) {
	ref, err := m.store.ResolveChallenge(ctx, pool, purpose, raw)
	if err != nil {
		return store.ChallengeRef{}, staffmfa.Token{}, err
	}
	tok, err := staffmfa.ParseToken(raw)
	if err != nil {
		return store.ChallengeRef{}, staffmfa.Token{}, store.ErrInvalidChallenge
	}
	return ref, tok, nil
}

// lockChallenge is phase 2's first step; ok is false for an unusable challenge.
func (m *StaffMFA) lockChallenge(ctx context.Context, tx pgx.Tx, purpose store.ChallengePurpose, tok staffmfa.Token, now time.Time) (store.Challenge, bool, error) {
	ch, err := m.store.LockChallenge(ctx, tx, purpose, tok, now)
	if errors.Is(err, store.ErrInvalidChallenge) {
		return store.Challenge{}, false, nil
	}
	if err != nil {
		return store.Challenge{}, false, err
	}
	return ch, true, nil
}

// refuseNotActive consumes the challenge of a staff member who is no longer
// active (or whose version moved) and audits the refusal.
func (m *StaffMFA) refuseNotActive(ctx context.Context, tx pgx.Tx, r *http.Request, requestID string, ch store.Challenge, now time.Time) error {
	if err := m.store.ConsumeChallenge(ctx, tx, ch.ID, now); err != nil {
		return err
	}
	return staffAudit(ctx, tx, r, requestID, ch.Staff, "staff.login_denied", audit.OutcomeDenied,
		map[string]any{"reason": "account_status", "stage": string(ch.Purpose)})
}

// auditLocked records an answer given without any comparison because the
// factor is locked (section 6.5).
func auditLocked(ctx context.Context, tx pgx.Tx, r *http.Request, requestID string, staff staffmfa.StaffRef, stage string, hard bool) error {
	return staffAudit(ctx, tx, r, requestID, staff, "staff.mfa_locked", audit.OutcomeDenied,
		map[string]any{"stage": stage, "hard_locked": hard})
}

func lockedOutcome(o staffmfa.Outcome) bool {
	return o == staffmfa.OutcomeLocked || o == staffmfa.OutcomeHardLocked
}

// recordSecondFactorFailure spends one challenge attempt (consuming it when
// none are left), audits the failure and collects the alerts it triggers. The
// factor counters were already written by the store in the same transaction.
// noFactor is S3 entry gate G7: an absent active factor at this stage gets the
// same answer as a wrong code and raises the integrity alert.
func (m *StaffMFA) recordSecondFactorFailure(ctx context.Context, tx pgx.Tx, r *http.Request, requestID string, ch store.Challenge, method string, vr store.VerifyResult, noFactor bool, now time.Time) ([]staffAlert, error) {
	left, err := m.store.DecrementChallengeAttempts(ctx, tx, ch.ID, now)
	if err != nil {
		return nil, err
	}
	if err := staffAudit(ctx, tx, r, requestID, ch.Staff, "staff.mfa_verification_failed", audit.OutcomeFailure,
		map[string]any{"method": method, "attempts_left": left, "hard_lock_set": vr.HardLockSet,
			"integrity_failure": vr.IntegrityFailure, "no_active_factor": noFactor}); err != nil {
		return nil, err
	}
	var alerts []staffAlert
	if vr.HardLockSet {
		// The password was right and ten second-factor guesses failed: a compromise signal (section 6.5).
		if err := staffAudit(ctx, tx, r, requestID, ch.Staff, "staff.mfa_hard_locked", audit.OutcomeDenied,
			map[string]any{"method": method, "compromise_signal": true}); err != nil {
			return nil, err
		}
		alerts = append(alerts, newStaffAlert(signalMFAHardLocked, ch.Staff, requestID, nil))
	}
	if vr.IntegrityFailure {
		alerts = append(alerts, newStaffAlert(signalMFAIntegrity, ch.Staff, requestID, map[string]alerting.AttrValue{"cause": "aead_open_failed"}))
	}
	if noFactor {
		alerts = append(alerts, newStaffAlert(signalMFAIntegrity, ch.Staff, requestID, map[string]alerting.AttrValue{"cause": "factor_missing"}))
	}
	pattern, err := failurePatternAlert(ctx, tx, ch.Staff, requestID)
	if err != nil {
		return nil, err
	}
	if pattern != nil {
		alerts = append(alerts, *pattern)
	}
	return alerts, nil
}

// ---- POST /v1/staff/auth/mfa/verify --------------------------------------------

func newStaffMFAVerifyHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		m := deps.StaffMFA
		if m == nil {
			writeStaffAuthUnavailable(w, requestID)
			return
		}
		var req mfaVerifyRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		if req.ChallengeToken.reveal() == "" || req.Code.reveal() == "" {
			apierror.Write(w, requestID, apierror.CodeValidation, "challenge_token and code are required")
			return
		}
		ref, tok, err := m.resolveChallenge(r.Context(), deps.DB, store.ChallengeMFA, req.ChallengeToken.reveal())
		if errors.Is(err, store.ErrInvalidChallenge) {
			writeMFAOutcome(w, requestID, mfaInvalidChallenge)
			return
		}
		if err != nil {
			logger.Error("staff_mfa_verify_lookup_failed", "error", err)
			writeStaffAuthUnavailable(w, requestID)
			return
		}

		var (
			out    mfaOutcome
			pair   staffTokenPair
			alerts []staffAlert
		)
		err = m.run(r.Context(), deps.DB, ref.Staff, func(ctx context.Context, tx pgx.Tx) error {
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
			staff, live, err := liveStaff(ctx, tx, ch.Staff, ch.StaffVersionAtIssue)
			if err != nil {
				return err
			}
			if !live {
				out = mfaNotActive
				return m.refuseNotActive(ctx, tx, r, requestID, ch, now)
			}
			vr, err := m.factor.VerifyDetailed(ctx, tx, ch.Staff, staffmfa.NewTOTPAssertion(req.Code.reveal()), now)
			noFactor := errors.Is(err, store.ErrNoActiveFactor)
			if err != nil && !noFactor {
				return err
			}
			switch {
			case !noFactor && vr.Accepted():
				// S3 entry gate G3: the step burn (above), the challenge consumption and the session issue
				// commit together or not at all.
				if err := m.store.ConsumeChallenge(ctx, tx, ch.ID, now); err != nil {
					return err
				}
				s, err := auth.IssueStaffSession(ctx, tx, m.keys, auth.StaffSessionParams{
					StaffID: ch.Staff.StaffID, TenantID: ch.Staff.TenantID, AMR: staffLoginAMR, Now: now,
					AbsoluteTTL: m.absoluteTTL, IdleTTL: m.idleTTL, UserAgent: r.UserAgent(), IPAddress: clientIP(r),
				})
				if err != nil {
					return err
				}
				if pair, err = m.mintStaffTokens(deps.AuthIssuer, s, auth.Role(staff.Role), staff.StaffVersion); err != nil {
					return err
				}
				out = mfaSuccess
				return staffAudit(ctx, tx, r, requestID, ch.Staff, "staff.login_succeeded", audit.OutcomeSuccess,
					map[string]any{"amr": strings.Join(staffLoginAMR, ","), "session_id": s.ID.String()})
			case !noFactor && lockedOutcome(vr.Outcome):
				out = mfaLocked
				return auditLocked(ctx, tx, r, requestID, ch.Staff, "verify", vr.Outcome == staffmfa.OutcomeHardLocked)
			default:
				out = mfaInvalidCode
				alerts, err = m.recordSecondFactorFailure(ctx, tx, r, requestID, ch, "totp", vr, noFactor, now)
				return err
			}
		})
		if err != nil {
			logger.Error("staff_mfa_verify_failed", "error", err)
			writeStaffAuthUnavailable(w, requestID)
			return
		}
		raiseStaffAlerts(r.Context(), deps.DB, alerts)
		if out == mfaSuccess {
			writeStaffSecretJSON(w, http.StatusOK, pair)
			return
		}
		writeMFAOutcome(w, requestID, out)
	}
}

// ---- POST /v1/staff/auth/mfa/recover -------------------------------------------

func newStaffMFARecoverHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		m := deps.StaffMFA
		if m == nil {
			writeStaffAuthUnavailable(w, requestID)
			return
		}
		var req mfaRecoverRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		if req.ChallengeToken.reveal() == "" || req.RecoveryCode.reveal() == "" {
			apierror.Write(w, requestID, apierror.CodeValidation, "challenge_token and recovery_code are required")
			return
		}
		ref, tok, err := m.resolveChallenge(r.Context(), deps.DB, store.ChallengeMFA, req.ChallengeToken.reveal())
		if errors.Is(err, store.ErrInvalidChallenge) {
			writeMFAOutcome(w, requestID, mfaInvalidChallenge)
			return
		}
		if err != nil {
			logger.Error("staff_mfa_recover_lookup_failed", "error", err)
			writeStaffAuthUnavailable(w, requestID)
			return
		}

		var (
			out    mfaOutcome
			resp   staffChallenge
			alerts []staffAlert
		)
		err = m.run(r.Context(), deps.DB, ref.Staff, func(ctx context.Context, tx pgx.Tx) error {
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
			if _, live, err := liveStaff(ctx, tx, ch.Staff, ch.StaffVersionAtIssue); err != nil || !live {
				if err != nil {
					return err
				}
				out = mfaNotActive
				return m.refuseNotActive(ctx, tx, r, requestID, ch, now)
			}
			vr, err := m.store.ConsumeRecoveryCode(ctx, tx, ch.Staff, staffmfa.NewRecoveryCodeAssertion(req.RecoveryCode.reveal()), now)
			noFactor := errors.Is(err, store.ErrNoActiveFactor)
			if err != nil && !noFactor {
				return err
			}
			switch {
			case !noFactor && vr.Accepted():
				// Section 6.1.3: a recovery code buys a re-enrolment, never a session. The old factor stays active
				// until the new one is confirmed.
				if err := m.store.ConsumeChallenge(ctx, tx, ch.ID, now); err != nil {
					return err
				}
				next, err := m.store.IssueChallenge(ctx, tx, ch.Staff, store.ChallengeEnrol, store.OriginRecoveryCode, ch.StaffVersionAtIssue, now)
				if err != nil {
					return err
				}
				resp = newStaffChallenge(string(store.ChallengeEnrol), next, challengeExpiresIn, enrolChallengeMethods)
				out = mfaSuccess
				alerts = append(alerts, newStaffAlert(signalMFARecoveryUsed, ch.Staff, requestID, nil))
				return staffAudit(ctx, tx, r, requestID, ch.Staff, "staff.mfa_recovery_used", audit.OutcomeSuccess,
					map[string]any{"next": "enrol_challenge"})
			case !noFactor && lockedOutcome(vr.Outcome):
				out = mfaLocked
				return auditLocked(ctx, tx, r, requestID, ch.Staff, "recover", vr.Outcome == staffmfa.OutcomeHardLocked)
			default:
				out = mfaInvalidCode
				alerts, err = m.recordSecondFactorFailure(ctx, tx, r, requestID, ch, "recovery_code", vr, noFactor, now)
				return err
			}
		})
		if err != nil {
			logger.Error("staff_mfa_recover_failed", "error", err)
			writeStaffAuthUnavailable(w, requestID)
			return
		}
		raiseStaffAlerts(r.Context(), deps.DB, alerts)
		if out == mfaSuccess {
			writeStaffSecretJSON(w, http.StatusOK, resp)
			return
		}
		writeMFAOutcome(w, requestID, out)
	}
}

// ---- POST /v1/staff/auth/enrol/start -------------------------------------------

func newStaffEnrolStartHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		m := deps.StaffMFA
		if m == nil {
			writeStaffAuthUnavailable(w, requestID)
			return
		}
		var req enrolStartRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		if req.ChallengeToken.reveal() == "" {
			apierror.Write(w, requestID, apierror.CodeValidation, "challenge_token is required")
			return
		}
		ref, tok, err := m.resolveChallenge(r.Context(), deps.DB, store.ChallengeEnrol, req.ChallengeToken.reveal())
		if errors.Is(err, store.ErrInvalidChallenge) {
			writeMFAOutcome(w, requestID, mfaInvalidChallenge)
			return
		}
		if err != nil {
			logger.Error("staff_enrol_start_lookup_failed", "error", err)
			writeStaffAuthUnavailable(w, requestID)
			return
		}

		var (
			out  mfaOutcome
			resp enrolStartResponse
		)
		err = m.run(r.Context(), deps.DB, ref.Staff, func(ctx context.Context, tx pgx.Tx) error {
			now, err := dbNow(ctx, tx)
			if err != nil {
				return err
			}
			ch, ok, err := m.lockChallenge(ctx, tx, store.ChallengeEnrol, tok, now)
			if err != nil {
				return err
			}
			if !ok {
				out = mfaInvalidChallenge
				return nil
			}
			if _, live, err := liveStaff(ctx, tx, ch.Staff, ch.StaffVersionAtIssue); err != nil || !live {
				if err != nil {
					return err
				}
				out = mfaNotActive
				return m.refuseNotActive(ctx, tx, r, requestID, ch, now)
			}
			// The challenge is NOT consumed (section 6.2); Begin supersedes any earlier pending factor.
			id, material, err := m.factor.Begin(ctx, tx, ch.Staff, now)
			if err != nil {
				return err
			}
			resp = newEnrolStartResponse(id, material)
			out = mfaSuccess
			return staffAudit(ctx, tx, r, requestID, ch.Staff, "staff.mfa_enrolment_started", audit.OutcomeSuccess,
				map[string]any{"factor_id": id.String(), "factor_type": string(staffmfa.FactorTOTP), "via": string(ch.Origin)})
		})
		if err != nil {
			logger.Error("staff_enrol_start_failed", "error", err)
			writeStaffAuthUnavailable(w, requestID)
			return
		}
		if out == mfaSuccess {
			writeStaffSecretJSON(w, http.StatusOK, resp)
			return
		}
		writeMFAOutcome(w, requestID, out)
	}
}

// ---- POST /v1/staff/auth/enrol/confirm -----------------------------------------

func newStaffEnrolConfirmHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		m := deps.StaffMFA
		if m == nil {
			writeStaffAuthUnavailable(w, requestID)
			return
		}
		var req enrolConfirmRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		factorID, idErr := uuid.Parse(req.FactorID)
		if req.ChallengeToken.reveal() == "" || req.Code.reveal() == "" || idErr != nil || factorID == uuid.Nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "challenge_token, factor_id (a UUID) and code are required")
			return
		}
		ref, tok, err := m.resolveChallenge(r.Context(), deps.DB, store.ChallengeEnrol, req.ChallengeToken.reveal())
		if errors.Is(err, store.ErrInvalidChallenge) {
			writeMFAOutcome(w, requestID, mfaInvalidChallenge)
			return
		}
		if err != nil {
			logger.Error("staff_enrol_confirm_lookup_failed", "error", err)
			writeStaffAuthUnavailable(w, requestID)
			return
		}

		var (
			out    mfaOutcome
			resp   enrolConfirmResponse
			alerts []staffAlert
		)
		err = m.run(r.Context(), deps.DB, ref.Staff, func(ctx context.Context, tx pgx.Tx) error {
			now, err := dbNow(ctx, tx)
			if err != nil {
				return err
			}
			ch, ok, err := m.lockChallenge(ctx, tx, store.ChallengeEnrol, tok, now)
			if err != nil {
				return err
			}
			if !ok {
				out = mfaInvalidChallenge
				return nil
			}
			staff, live, err := liveStaff(ctx, tx, ch.Staff, ch.StaffVersionAtIssue)
			if err != nil {
				return err
			}
			if !live {
				out = mfaNotActive
				return m.refuseNotActive(ctx, tx, r, requestID, ch, now)
			}
			cr, err := m.factor.ConfirmDetailed(ctx, tx, ch.Staff, factorID, staffmfa.NewTOTPAssertion(req.Code.reveal()), now)
			if errors.Is(err, store.ErrFactorNotFound) || errors.Is(err, store.ErrPendingExpired) {
				out = mfaEnrolmentExpired
				return staffAudit(ctx, tx, r, requestID, ch.Staff, "staff.mfa_enrolment_failed", audit.OutcomeFailure,
					map[string]any{"reason": "pending_factor_unavailable", "factor_id": factorID.String()})
			}
			if err != nil {
				return err
			}
			switch {
			case cr.Accepted():
				// Section 6.2, one transaction: the factor is active and the previous one revoked (ConfirmDetailed);
				// then new recovery codes, the challenge consumed, every other session revoked, an aal2 session.
				batch, err := m.store.IssueRecoveryCodes(ctx, tx, ch.Staff, now)
				if err != nil {
					return err
				}
				if err := m.store.ConsumeChallenge(ctx, tx, ch.ID, now); err != nil {
					return err
				}
				if err := auth.RevokeAllSessionsForPrincipal(ctx, tx, auth.PrincipalStaff, ch.Staff.StaffID); err != nil {
					return err
				}
				s, err := auth.IssueStaffSession(ctx, tx, m.keys, auth.StaffSessionParams{
					StaffID: ch.Staff.StaffID, TenantID: ch.Staff.TenantID, AMR: staffLoginAMR, Now: now,
					AbsoluteTTL: m.absoluteTTL, IdleTTL: m.idleTTL, UserAgent: r.UserAgent(), IPAddress: clientIP(r),
				})
				if err != nil {
					return err
				}
				pair, err := m.mintStaffTokens(deps.AuthIssuer, s, auth.Role(staff.Role), staff.StaffVersion)
				if err != nil {
					return err
				}
				resp = newEnrolConfirmResponse(batch, pair)
				out = mfaSuccess
				meta := map[string]any{"factor_type": string(staffmfa.FactorTOTP), "factor_id": factorID.String(),
					"via": string(ch.Origin), "session_id": s.ID.String()}
				if cr.ReplacedFactorID != uuid.Nil {
					meta["replaced_factor_id"] = cr.ReplacedFactorID.String()
				}
				return staffAudit(ctx, tx, r, requestID, ch.Staff, "staff.mfa_enrolled", audit.OutcomeSuccess, meta)
			case lockedOutcome(cr.Outcome):
				out = mfaLocked
				return auditLocked(ctx, tx, r, requestID, ch.Staff, "enrol_confirm", cr.Outcome == staffmfa.OutcomeHardLocked)
			default:
				out = mfaInvalidCode
				alerts, err = m.recordSecondFactorFailure(ctx, tx, r, requestID, ch, "enrol_confirm", cr.VerifyResult, false, now)
				return err
			}
		})
		if err != nil {
			logger.Error("staff_enrol_confirm_failed", "error", err)
			writeStaffAuthUnavailable(w, requestID)
			return
		}
		raiseStaffAlerts(r.Context(), deps.DB, alerts)
		if out == mfaSuccess {
			writeStaffSecretJSON(w, http.StatusOK, resp)
			return
		}
		writeMFAOutcome(w, requestID, out)
	}
}

// ---- POST /v1/staff/auth/enrol/redeem ------------------------------------------

// redeemPurposes are the enrolment tokens /enrol/redeem accepts (section 6.3):
// the MFA reset (S6) and the cmd/seed-admin bootstrap. invite and
// password_reset have their own endpoints (S6).
var redeemPurposes = []store.EnrolmentPurpose{store.EnrolMFAReset, store.EnrolBootstrap}

func redeemOrigin(p store.EnrolmentPurpose) store.ChallengeOrigin {
	if p == store.EnrolBootstrap {
		return store.OriginBootstrap
	}
	return store.OriginMFAReset
}

func newStaffEnrolRedeemHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		m := deps.StaffMFA
		if m == nil {
			writeStaffAuthUnavailable(w, requestID)
			return
		}
		var req enrolRedeemRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		if req.Token.reveal() == "" || strings.TrimSpace(req.Email) == "" || req.Password.reveal() == "" {
			apierror.Write(w, requestID, apierror.CodeValidation, "token, email and password are required")
			return
		}
		ip, ua := clientIP(r), r.UserAgent()
		// Every refusal below is the single answer invalid_enrolment_token, after the same Argon2 cost.
		refuse := func() {
			_, _ = auth.VerifyPassword(req.Password.reveal(), auth.DummyPasswordHash)
			writeMFAOutcome(w, requestID, mfaInvalidEnrolmentToken)
		}

		ref, err := m.store.ResolveEnrolmentToken(r.Context(), deps.DB, redeemPurposes, req.Token.reveal())
		if errors.Is(err, store.ErrInvalidEnrolmentToken) {
			// Wrong-token attempts are audited (section 6.3); no tenant is known, so the row is platform-scoped.
			if aerr := deps.DB.WithoutTenant(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
				return audit.Record(ctx, tx, audit.Entry{ActorType: audit.ActorSystem, Action: "staff.enrolment_token_rejected",
					Outcome: audit.OutcomeFailure, IPAddress: ip, UserAgent: ua, RequestID: requestID,
					Metadata: map[string]any{"reason": "unknown_or_unusable"}})
			}); aerr != nil {
				logger.Error("staff_enrol_redeem_audit_failed", "error", aerr)
				writeStaffAuthUnavailable(w, requestID)
				return
			}
			refuse()
			return
		}
		if err != nil {
			logger.Error("staff_enrol_redeem_lookup_failed", "error", err)
			writeStaffAuthUnavailable(w, requestID)
			return
		}
		// S3 entry gate (d): the tenant and the staff member come from the TOKEN ROW. The body's tenant_slug must
		// name that same tenant (omitted for platform staff) and its email must resolve, in that scope, to that same
		// staff member.
		if req.TenantSlug != "" {
			t, err := identity.GetTenantBySlug(r.Context(), deps.DB, req.TenantSlug)
			if err != nil && !errors.Is(err, identity.ErrNotFound) {
				logger.Error("staff_enrol_redeem_tenant_lookup_failed", "error", err)
				writeStaffAuthUnavailable(w, requestID)
				return
			}
			if err != nil || t.ID != ref.Staff.TenantID {
				refuse()
				return
			}
		} else if ref.Staff.TenantID != uuid.Nil {
			refuse()
			return
		}
		tok, err := staffmfa.ParseToken(req.Token.reveal())
		if err != nil {
			refuse()
			return
		}
		email := strings.ToLower(strings.TrimSpace(req.Email))
		identifier := "staff:" + email
		if ref.Staff.TenantID != uuid.Nil {
			identifier = ref.Staff.TenantID.String() + ":" + email
		}

		var (
			out          mfaOutcome
			resp         staffChallenge
			passwordPaid bool
			alerts       []staffAlert
		)
		err = m.run(r.Context(), deps.DB, ref.Staff, func(ctx context.Context, tx pgx.Tx) error {
			alerts = nil
			now, err := dbNow(ctx, tx)
			if err != nil {
				return err
			}
			locked, err := identity.IsLockedOut(ctx, tx, identifier)
			if err != nil {
				return err
			}
			if locked {
				out = mfaPasswordLockout
				return staffAudit(ctx, tx, r, requestID, ref.Staff, "staff.enrolment_token_rejected", audit.OutcomeDenied,
					map[string]any{"reason": "password_lockout"})
			}
			// ADR 0119 S5 + S6 review fix SR-2: an MFA-reset token whose issuer is no longer active is refused (the
			// issuer's row FOR SHARE before the token lock, the order a concurrent suspension also takes).
			if ref.Purpose == store.EnrolMFAReset {
				live, err := stafflifecycle.EnrolmentIssuersLiveInTx(ctx, tx, ref.ID)
				if err != nil {
					return err
				}
				if !live {
					out = mfaInvalidEnrolmentToken
					return staffAudit(ctx, tx, r, requestID, ref.Staff, "staff.enrolment_token_rejected", audit.OutcomeFailure,
						map[string]any{"reason": "issuer_not_live", "purpose": string(ref.Purpose)})
				}
			}
			row, err := m.store.LockEnrolmentToken(ctx, tx, ref.Purpose, tok, now)
			if errors.Is(err, store.ErrInvalidEnrolmentToken) {
				out = mfaInvalidEnrolmentToken
				return staffAudit(ctx, tx, r, requestID, ref.Staff, "staff.enrolment_token_rejected", audit.OutcomeFailure,
					map[string]any{"reason": "unusable", "purpose": string(ref.Purpose)})
			}
			if err != nil {
				return err
			}
			staff, err := identity.GetStaffUserByEmail(ctx, tx, email)
			if err != nil && !errors.Is(err, identity.ErrNotFound) {
				return err
			}
			if err != nil || staff.ID != row.Staff.StaffID || staff.TenantID != row.Staff.TenantID {
				out = mfaInvalidEnrolmentToken
				if err := recordAttempt(ctx, tx, row.Staff.TenantID, "staff", identifier, ip, false); err != nil {
					return err
				}
				return staffAudit(ctx, tx, r, requestID, row.Staff, "staff.enrolment_token_rejected", audit.OutcomeFailure,
					map[string]any{"reason": "identity_mismatch", "purpose": string(row.Purpose)})
			}
			passwordPaid = true
			ok, err := verifyStaffPassword(req.Password.reveal(), staff)
			if err != nil {
				return err
			}
			if !ok || staff.Status != "active" {
				out = mfaInvalidEnrolmentToken
				reason := "invalid_password"
				if ok {
					reason = "account_status"
				}
				if err := recordAttempt(ctx, tx, row.Staff.TenantID, "staff", identifier, ip, false); err != nil {
					return err
				}
				return staffAudit(ctx, tx, r, requestID, row.Staff, "staff.enrolment_token_rejected", audit.OutcomeFailure,
					map[string]any{"reason": reason, "purpose": string(row.Purpose)})
			}
			// Security review R3 (defence in depth): a bootstrap token is only ever issued to a staff member WITHOUT an
			// active factor (cmd/seed-admin refuses otherwise). One that meets an active factor was planted or raced:
			// the same single refusal, the token is not spent, and it is audited and alerted (P1).
			if row.Purpose == store.EnrolBootstrap {
				st, err := m.factor.ActiveFactorState(ctx, tx, row.Staff, now)
				if err != nil {
					return err
				}
				if st.Exists {
					out = mfaInvalidEnrolmentToken
					alerts = append(alerts, newStaffAlert(signalMFAIntegrity, row.Staff, requestID,
						map[string]alerting.AttrValue{"cause": "bootstrap_with_active_factor"}))
					return staffAudit(ctx, tx, r, requestID, row.Staff, "staff.enrolment_token_rejected", audit.OutcomeDenied,
						map[string]any{"reason": "active_factor", "purpose": string(row.Purpose)})
				}
			}
			if err := m.store.ConsumeEnrolmentToken(ctx, tx, row.ID, now); err != nil {
				return err
			}
			next, err := m.store.IssueChallenge(ctx, tx, row.Staff, store.ChallengeEnrol, redeemOrigin(row.Purpose), staff.StaffVersion, now)
			if err != nil {
				return err
			}
			if err := recordAttempt(ctx, tx, row.Staff.TenantID, "staff", identifier, ip, true); err != nil {
				return err
			}
			resp = newStaffChallenge(string(store.ChallengeEnrol), next, challengeExpiresIn, enrolChallengeMethods)
			out = mfaSuccess
			return staffAudit(ctx, tx, r, requestID, row.Staff, "staff.enrolment_token_redeemed", audit.OutcomeSuccess,
				map[string]any{"purpose": string(row.Purpose)})
		})
		if err != nil {
			logger.Error("staff_enrol_redeem_failed", "error", err)
			writeStaffAuthUnavailable(w, requestID)
			return
		}
		raiseStaffAlerts(r.Context(), deps.DB, alerts)
		if out == mfaSuccess {
			writeStaffSecretJSON(w, http.StatusOK, resp)
			return
		}
		// Every refusal pays one Argon2 verification (the lockout 429 included: review timing note), so the
		// refusal paths are not distinguishable by latency from a wrong password.
		if !passwordPaid {
			_, _ = auth.VerifyPassword(req.Password.reveal(), auth.DummyPasswordHash)
		}
		writeMFAOutcome(w, requestID, out)
	}
}

// ---- GET /v1/staff/me/mfa ------------------------------------------------------

func newStaffMFAStatusHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		m := deps.StaffMFA
		if m == nil {
			writeStaffAuthUnavailable(w, requestID)
			return
		}
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		subject, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid token subject")
			return
		}
		ref := staffmfa.StaffRef{StaffID: subject, TenantID: tc.TenantID}
		var resp staffMFAStatusResponse
		err = store.WithScope(r.Context(), deps.DB, ref, func(ctx context.Context, tx pgx.Tx) error {
			now, err := dbNow(ctx, tx)
			if err != nil {
				return err
			}
			st, err := m.factor.ActiveFactorState(ctx, tx, ref, now)
			if err != nil {
				return err
			}
			resp = staffMFAStatusResponse{Enrolled: st.Exists, HardLocked: st.HardLocked}
			if !st.Exists {
				return nil
			}
			ft := string(staffmfa.FactorTOTP)
			resp.FactorType = &ft
			if !st.ActivatedAt.IsZero() {
				a := st.ActivatedAt.UTC().Format(time.RFC3339)
				resp.ActivatedAt = &a
			}
			if !st.LockedUntil.IsZero() {
				l := st.LockedUntil.UTC().Format(time.RFC3339)
				resp.LockedUntil = &l
			}
			resp.RecoveryCodesRemaining, err = m.store.RecoveryCodesRemaining(ctx, tx, ref)
			return err
		})
		if err != nil {
			logger.Error("staff_mfa_status_failed", "error", err)
			writeStaffAuthUnavailable(w, requestID)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}
