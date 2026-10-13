package httpserver

// ADR 0119 slice S4b: POST /v1/staff/auth/step-up (section 6.6) and
// POST /v1/staff/mfa/recovery-codes/regenerate (section 6.2). Both are staff
// routes (staffRoute: bearer, staff principal, liveness, aal2); the subject and
// the session come from the verified token, never from the body.
//
// The step-up follows the S3 second-factor shape (staff_auth_handlers.go): ONE
// transaction in the staff member's own scope takes its clock from the
// database, re-checks the staff member, verifies the TOTP code with the same
// factor, replay rule and failure counters as login (one budget for every
// second-factor guess, section 6.5), and records the outcome - the step burn,
// the session re-seal and the audit row on success; the counters and the audit
// row on refusal (the closure returns nil so a refusal COMMITS, gate G1). Only
// after COMMIT is the outcome answered and any alert raised; every fault is the
// one fixed 503 (gate G4). Recovery codes are not accepted for step-up.
//
// The session write goes ONLY to the caller's own session row, which must be
// the live head of its rotation chain (auth.StepUpStaffSession, FOR UPDATE):
// a step-up never writes mfa_at to a dead row (review N2). The failure that
// locks the factor (the fifth within 15 minutes, login failures included) also
// revokes the calling session's whole chain (section 6.5: a token thief cannot
// grind TOTP from inside a session).

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/store"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// staffCaller is the verified staff caller of a staff route.
type staffCaller struct {
	ref     staffmfa.StaffRef
	role    auth.Role
	session auth.StaffSession
}

// staffCallerFrom reads the verified caller; ok is false without a staff
// session in the context (the chain is mis-composed: fail closed).
func staffCallerFrom(r *http.Request) (staffCaller, bool) {
	tc, err := tenant.FromContext(r.Context())
	if err != nil || tc.PrincipalType != string(auth.PrincipalStaff) {
		return staffCaller{}, false
	}
	sub, err := uuid.Parse(tc.Subject)
	if err != nil || sub == uuid.Nil {
		return staffCaller{}, false
	}
	sess, ok := auth.StaffSessionFromContext(r.Context())
	if !ok {
		return staffCaller{}, false
	}
	return staffCaller{ref: staffmfa.StaffRef{StaffID: sub, TenantID: tc.TenantID}, role: auth.Role(tc.Role), session: sess}, true
}

const (
	stepUpOutcomeUnset = iota
	stepUpOutcomeSuccess
	stepUpOutcomeInvalidCode
	stepUpOutcomeLocked
	stepUpOutcomeSessionRevoked
)

// stepUpRequest is the step-up body; the code is redacted everywhere.
type stepUpRequest struct {
	Code redacted[string] `json:"code"`
}

// stepUpResponse carries the new access token (a secret).
type stepUpResponse struct {
	AccessToken redacted[string]
	ExpiresIn   int
	MFAAt       time.Time
	Financial   time.Time
	Critical    time.Time
}

type stepUpExpiriesWire struct {
	Financial string `json:"financial"`
	Critical  string `json:"critical"`
}

type stepUpWire struct {
	AccessToken     string             `json:"access_token"`
	TokenType       string             `json:"token_type"`
	ExpiresIn       int                `json:"expires_in"`
	MFAAt           string             `json:"mfa_at"`
	StepUpExpiresAt stepUpExpiriesWire `json:"step_up_expires_at"`
}

func (r stepUpResponse) wire() any {
	return stepUpWire{AccessToken: r.AccessToken.reveal(), TokenType: "Bearer", ExpiresIn: r.ExpiresIn,
		MFAAt:           r.MFAAt.UTC().Format(time.RFC3339),
		StepUpExpiresAt: stepUpExpiriesWire{Financial: r.Financial.UTC().Format(time.RFC3339), Critical: r.Critical.UTC().Format(time.RFC3339)}}
}

// recoveryCodesResponse carries the regenerated codes (secrets) exactly once.
type recoveryCodesResponse struct {
	RecoveryCodes redacted[[]string]
}

type recoveryCodesWire struct {
	RecoveryCodes []string `json:"recovery_codes"`
}

func (r recoveryCodesResponse) wire() any {
	return recoveryCodesWire{RecoveryCodes: r.RecoveryCodes.reveal()}
}

func newRecoveryCodesResponse(batch staffmfa.RecoveryCodeBatch) recoveryCodesResponse {
	codes := batch.Codes()
	out := make([]string, 0, len(codes))
	for _, c := range codes {
		out = append(out, c.RevealForDisplay())
	}
	return recoveryCodesResponse{RecoveryCodes: newRedacted(out)}
}

// stepUpAudit writes one step-up audit row (target: the session). Metadata
// never carries a code or a token (the static audit-metadata scan covers this
// file).
func stepUpAudit(ctx context.Context, tx pgx.Tx, r *http.Request, requestID string, c staffCaller, action string, outcome audit.Outcome, metadata map[string]any) error {
	return audit.Record(ctx, tx, audit.Entry{
		TenantID: c.ref.TenantID, ActorType: audit.ActorStaff, ActorID: c.ref.StaffID,
		Action: action, TargetType: "session", TargetID: c.session.SessionID.String(),
		Outcome: outcome, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID, Metadata: metadata,
	})
}

// ---- POST /v1/staff/auth/step-up -----------------------------------------------

func newStaffStepUpHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		m := deps.StaffMFA
		if m == nil {
			writeStaffAuthUnavailable(w, requestID)
			return
		}
		c, ok := staffCallerFrom(r)
		if !ok {
			writeSessionRevoked(w, requestID)
			return
		}
		var req stepUpRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		if req.Code.reveal() == "" {
			apierror.Write(w, requestID, apierror.CodeValidation, "code is required")
			return
		}

		var (
			out    = stepUpOutcomeUnset
			resp   stepUpResponse
			alerts []staffAlert
		)
		err := m.run(r.Context(), deps.DB, c.ref, func(ctx context.Context, tx pgx.Tx) error {
			alerts = nil
			now, err := dbNow(ctx, tx)
			if err != nil {
				return err
			}
			staff, live, err := liveStaff(ctx, tx, c.ref, c.session.StaffVersion)
			if err != nil {
				return err
			}
			if !live || auth.Role(staff.Role) != c.role {
				out = stepUpOutcomeSessionRevoked
				return stepUpAudit(ctx, tx, r, requestID, c, "staff.step_up_failed", audit.OutcomeDenied,
					map[string]any{"reason": "account_status"})
			}
			vr, err := m.factor.VerifyDetailed(ctx, tx, c.ref, staffmfa.NewTOTPAssertion(req.Code.reveal()), now)
			noFactor := errors.Is(err, store.ErrNoActiveFactor)
			if err != nil && !noFactor {
				return err
			}
			switch {
			case !noFactor && vr.Accepted():
				res, err := auth.StepUpStaffSession(ctx, tx, m.keys, c.session.SessionID, c.ref.StaffID, c.ref.TenantID, now)
				switch {
				case errors.Is(err, auth.ErrStepUpNotChainHead):
					// The code's step stays burned (the transaction commits): a code is accepted at most once.
					out = stepUpOutcomeSessionRevoked
					return stepUpAudit(ctx, tx, r, requestID, c, "staff.step_up_failed", audit.OutcomeDenied,
						map[string]any{"reason": "not_chain_head"})
				case errors.Is(err, auth.ErrStepUpSealInvalid):
					out = stepUpOutcomeSessionRevoked
					if res.Legacy {
						return stepUpAudit(ctx, tx, r, requestID, c, "staff.step_up_failed", audit.OutcomeDenied,
							map[string]any{"reason": "legacy_seal"})
					}
					alerts = append(alerts, newStaffAlert(signalSessionIntegrity, c.ref, requestID, map[string]alerting.AttrValue{
						"session_id": c.session.SessionID.String(), "cause": auth.IntegrityCauseBadSeal}))
					return stepUpAudit(ctx, tx, r, requestID, c, "auth.session_assurance_invalid", audit.OutcomeDenied,
						map[string]any{"cause": auth.IntegrityCauseBadSeal, "stage": "step_up"})
				case err != nil:
					return err
				}
				access, err := deps.AuthIssuer.IssueStaff(auth.StaffTokenParams{
					StaffID: c.ref.StaffID, TenantID: c.ref.TenantID, Role: c.role, SessionID: res.SessionID,
					AAL: res.AAL, AMR: res.AMR, AuthTime: res.AuthTime, MFAAt: res.MFAAt, StaffVersion: staffVersionUnversioned,
				}, m.accessTTL)
				if err != nil {
					return err
				}
				resp = stepUpResponse{AccessToken: newRedacted(access), ExpiresIn: int(m.accessTTL.Seconds()), MFAAt: res.MFAAt,
					Financial: res.MFAAt.Add(m.stepUp.Financial), Critical: res.MFAAt.Add(m.stepUp.Critical)}
				out = stepUpOutcomeSuccess
				return stepUpAudit(ctx, tx, r, requestID, c, "staff.step_up_verified", audit.OutcomeSuccess,
					map[string]any{"method": "totp", "session_id": res.SessionID.String()})
			case !noFactor && lockedOutcome(vr.Outcome):
				out = stepUpOutcomeLocked
				return auditLocked(ctx, tx, r, requestID, c.ref, "step_up", vr.Outcome == staffmfa.OutcomeHardLocked)
			default:
				out = stepUpOutcomeInvalidCode
				var revoked bool
				alerts, revoked, err = m.recordStepUpFailure(ctx, tx, r, requestID, c, vr, noFactor, now)
				if revoked {
					out = stepUpOutcomeSessionRevoked
				}
				return err
			}
		})
		if err != nil {
			logger.Error("staff_step_up_failed", "error", err)
			writeStaffAuthUnavailable(w, requestID)
			return
		}
		raiseStaffAlerts(r.Context(), deps.DB, alerts)
		switch out {
		case stepUpOutcomeSuccess:
			writeStaffSecretJSON(w, http.StatusOK, resp)
		case stepUpOutcomeInvalidCode:
			writeMFAOutcome(w, requestID, mfaInvalidCode)
		case stepUpOutcomeLocked:
			writeMFAOutcome(w, requestID, mfaLocked)
		case stepUpOutcomeSessionRevoked:
			writeSessionRevoked(w, requestID)
		default:
			writeStaffAuthUnavailable(w, requestID)
		}
	}
}

// recordStepUpFailure audits a failed step-up (the factor counters were
// already written by the store in this transaction), collects the alerts it
// triggers and, when this failure left the factor locked, revokes the calling
// session's whole chain (section 6.5); revoked reports that.
func (m *StaffMFA) recordStepUpFailure(ctx context.Context, tx pgx.Tx, r *http.Request, requestID string, c staffCaller, vr store.VerifyResult, noFactor bool, now time.Time) ([]staffAlert, bool, error) {
	var alerts []staffAlert
	st, err := m.factor.ActiveFactorState(ctx, tx, c.ref, now)
	if err != nil {
		return nil, false, err
	}
	locked := noFactor || !st.Exists || st.HardLocked || st.Lock == staffmfa.LockSoft || st.Lock == staffmfa.LockHard
	if err := stepUpAudit(ctx, tx, r, requestID, c, "staff.step_up_failed", audit.OutcomeFailure,
		map[string]any{"reason": "invalid_code", "method": "totp", "hard_lock_set": vr.HardLockSet,
			"integrity_failure": vr.IntegrityFailure, "no_active_factor": noFactor, "session_revoked": locked}); err != nil {
		return nil, false, err
	}
	if vr.HardLockSet {
		if err := staffAudit(ctx, tx, r, requestID, c.ref, "staff.mfa_hard_locked", audit.OutcomeDenied,
			map[string]any{"method": "step_up", "compromise_signal": true}); err != nil {
			return nil, false, err
		}
		alerts = append(alerts, newStaffAlert(signalMFAHardLocked, c.ref, requestID, nil))
	}
	if vr.IntegrityFailure {
		alerts = append(alerts, newStaffAlert(signalMFAIntegrity, c.ref, requestID, map[string]alerting.AttrValue{"cause": "aead_open_failed"}))
	}
	if noFactor {
		alerts = append(alerts, newStaffAlert(signalMFAIntegrity, c.ref, requestID, map[string]alerting.AttrValue{"cause": "factor_missing"}))
	}
	pattern, err := failurePatternAlert(ctx, tx, c.ref, requestID)
	if err != nil {
		return nil, false, err
	}
	if pattern != nil {
		alerts = append(alerts, *pattern)
	}
	if locked {
		if err := auth.RevokeSession(ctx, tx, c.session.SessionID, c.ref.StaffID); err != nil && !errors.Is(err, auth.ErrSessionNotFound) {
			return nil, false, err
		}
	}
	return alerts, locked, nil
}

// ---- POST /v1/staff/mfa/recovery-codes/regenerate --------------------------------

func newStaffRecoveryCodesRegenerateHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		m := deps.StaffMFA
		if m == nil {
			writeStaffAuthUnavailable(w, requestID)
			return
		}
		c, ok := staffCallerFrom(r)
		if !ok {
			writeSessionRevoked(w, requestID)
			return
		}
		a, ok := auth.StepUpAssertionFrom(r.Context())
		if !ok || a.Subject != c.ref.StaffID || a.SessionID != c.session.SessionID {
			// The route is step-up critical; reaching here without the assertion is a mis-composed chain.
			apierror.Write(w, requestID, apierror.CodeInternal, "internal error")
			return
		}
		var (
			resp    recoveryCodesResponse
			revoked bool
		)
		err := m.run(r.Context(), deps.DB, c.ref, func(ctx context.Context, tx pgx.Tx) error {
			now, err := dbNow(ctx, tx)
			if err != nil {
				return err
			}
			staff, live, err := liveStaff(ctx, tx, c.ref, c.session.StaffVersion)
			if err != nil {
				return err
			}
			if !live || auth.Role(staff.Role) != c.role {
				revoked = true
				return nil
			}
			batch, err := m.store.IssueRecoveryCodes(ctx, tx, c.ref, now)
			if errors.Is(err, store.ErrNoActiveFactor) {
				revoked = true
				return nil
			}
			if err != nil {
				return err
			}
			resp = newRecoveryCodesResponse(batch)
			return stepUpAudit(ctx, tx, r, requestID, c, "staff.mfa_recovery_codes_regenerated", audit.OutcomeSuccess,
				map[string]any{"count": len(batch.Codes()), "step_up_class": string(a.Class)})
		})
		if err != nil {
			logger.Error("staff_recovery_codes_regenerate_failed", "error", err)
			writeStaffAuthUnavailable(w, requestID)
			return
		}
		if revoked {
			writeSessionRevoked(w, requestID)
			return
		}
		writeStaffSecretJSON(w, http.StatusOK, resp)
	}
}
