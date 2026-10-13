package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/store"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

// staffLoginRequest.TenantSlug is omitted entirely for a platform_admin
// login (there is no tenant to resolve); every other staff role must
// supply it, since staff_users' dual-scope RLS policy requires knowing
// the tenant to query a tenant-scoped row at all.
type staffLoginRequest struct {
	TenantSlug string `json:"tenant_slug,omitempty"`
	Email      string `json:"email"`
	Password   string `json:"password"`
}

// newStaffLoginHandler is step 1 of the ADR 0119 two-step staff login
// (section 6.1, slice S3): the password step. The password lockout, the
// dummy-hash timing equalisation, the scope check and the status check are
// unchanged from Stage 2. A correct password NEVER yields a token pair:
//
//   - an ACTIVE factor that is not hard-locked: a single-use `mfa` challenge
//     (5 minutes, 5 attempts) to complete at /v1/staff/auth/mfa/verify or
//     /mfa/recover;
//   - no active factor: 403 mfa_enrolment_required (a password alone never
//     opens enrolment). The answer is the same for a never-enrolled account and
//     for one whose factor disappeared; the latter also raises the integrity
//     alert (residual R3, S3 entry gate G7);
//   - a hard-locked factor: 403 mfa_locked, no challenge (only an MFA reset
//     clears it).
//
// There is no MFA bypass in any environment: a process without the staff MFA
// subsystem (Deps.StaffMFA nil) answers 503.
func newStaffLoginHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		noStore(w)
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		m := deps.StaffMFA
		if m == nil {
			writeStaffAuthUnavailable(w, requestID)
			return
		}

		var req staffLoginRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("email", req.Email)
		v.RequireNonEmpty("password", req.Password)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		tenantID := uuid.Nil
		if req.TenantSlug != "" {
			t, err := identity.GetTenantBySlug(r.Context(), deps.DB, req.TenantSlug)
			if errors.Is(err, identity.ErrNotFound) {
				apierror.Write(w, requestID, apierror.CodeNotFound, "unknown tenant")
				return
			}
			if err != nil {
				logger.Error("staff_login_tenant_lookup_failed", "error", err)
				writeStaffAuthUnavailable(w, requestID)
				return
			}
			tenantID = t.ID
		}

		// Normalize BEFORE building the lockout identifier -
		// GetStaffUserByEmail normalizes internally for its lookup, but
		// building the identifier from the raw request email let
		// case/whitespace variants of the same address (admin@x.com,
		// Admin@x.com, " admin@x.com") each get their own login_attempts
		// bucket, making lockout trivially bypassable. Caught in Stage 2
		// security review.
		email, ip, ua := strings.ToLower(strings.TrimSpace(req.Email)), clientIP(r), r.UserAgent()
		identifier := "staff:" + email
		if tenantID != uuid.Nil {
			identifier = tenantID.String() + ":" + email
		}

		scopeFn := deps.DB.WithoutTenant
		if tenantID != uuid.Nil {
			scopeFn = func(ctx context.Context, fn db.TxFunc) error { return deps.DB.WithTenant(ctx, tenantID, fn) }
		}

		const (
			outcomeSuccess = iota
			outcomeLocked
			outcomeInvalidCredentials
			outcomeNotActive
			outcomeEnrolmentRequired
			outcomeMFALocked
		)
		outcome := outcomeSuccess
		var (
			challenge staffChallenge
			alerts    []staffAlert
		)

		err := scopeFn(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
			locked, err := identity.IsLockedOut(ctx, tx, identifier)
			if err != nil {
				return err
			}
			if locked {
				outcome = outcomeLocked
				return audit.Record(ctx, tx, audit.Entry{
					TenantID: tenantID, ActorType: audit.ActorSystem,
					Action: "staff.login_blocked_lockout", Outcome: audit.OutcomeDenied,
					IPAddress: ip, UserAgent: ua, RequestID: requestID,
					Metadata: map[string]any{"identifier": identifier},
				})
			}

			staff, err := identity.GetStaffUserByEmail(ctx, tx, email)
			if errors.Is(err, identity.ErrNotFound) {
				// Spend the same Argon2 cost as a real wrong-password
				// check would (see auth.DummyPasswordHash).
				_, _ = auth.VerifyPassword(req.Password, auth.DummyPasswordHash)
				outcome = outcomeInvalidCredentials
				if err := recordAttempt(ctx, tx, tenantID, "staff", identifier, ip, false); err != nil {
					return err
				}
				return audit.Record(ctx, tx, audit.Entry{
					TenantID: tenantID, ActorType: audit.ActorSystem,
					Action: "staff.login_failed", Outcome: audit.OutcomeFailure,
					IPAddress: ip, UserAgent: ua, RequestID: requestID,
					Metadata: map[string]any{"reason": "unknown_email"},
				})
			}
			if err != nil {
				return err
			}

			// A tenant-scoped request must never authenticate a staff
			// row from a different tenant, and a platform-wide request
			// must never authenticate a tenant-scoped row - the RLS scope
			// already guarantees this (the row wouldn't have been
			// visible otherwise), but this makes the invariant explicit
			// rather than relying solely on that.
			if staff.TenantID != tenantID {
				outcome = outcomeInvalidCredentials
				if err := recordAttempt(ctx, tx, tenantID, "staff", identifier, ip, false); err != nil {
					return err
				}
				return audit.Record(ctx, tx, audit.Entry{
					TenantID: tenantID, ActorType: audit.ActorSystem,
					Action: "staff.login_failed", Outcome: audit.OutcomeFailure,
					IPAddress: ip, UserAgent: ua, RequestID: requestID,
					Metadata: map[string]any{"reason": "scope_mismatch"},
				})
			}

			passwordOK, err := auth.VerifyPassword(req.Password, staff.PasswordHash)
			if err != nil {
				return err
			}
			if !passwordOK {
				outcome = outcomeInvalidCredentials
				if err := recordAttempt(ctx, tx, tenantID, "staff", identifier, ip, false); err != nil {
					return err
				}
				return audit.Record(ctx, tx, audit.Entry{
					TenantID: tenantID, ActorType: audit.ActorStaff, ActorID: staff.ID,
					Action: "staff.login_failed", TargetType: "staff_user", TargetID: staff.ID.String(),
					Outcome: audit.OutcomeFailure, IPAddress: ip, UserAgent: ua, RequestID: requestID,
					Metadata: map[string]any{"reason": "invalid_password"},
				})
			}

			if staff.Status != "active" {
				outcome = outcomeNotActive
				if err := recordAttempt(ctx, tx, tenantID, "staff", identifier, ip, false); err != nil {
					return err
				}
				return audit.Record(ctx, tx, audit.Entry{
					TenantID: tenantID, ActorType: audit.ActorStaff, ActorID: staff.ID,
					Action: "staff.login_denied", TargetType: "staff_user", TargetID: staff.ID.String(),
					Outcome: audit.OutcomeDenied, IPAddress: ip, UserAgent: ua, RequestID: requestID,
					Metadata: map[string]any{"reason": "account_status", "status": staff.Status},
				})
			}

			if err := recordAttempt(ctx, tx, tenantID, "staff", identifier, ip, true); err != nil {
				return err
			}
			// ADR 0119 section 6.1 step 1: the second factor decides what a correct password buys.
			ref := staffmfa.StaffRef{StaffID: staff.ID, TenantID: tenantID}
			now, err := dbNow(ctx, tx)
			if err != nil {
				return err
			}
			st, err := m.factor.ActiveFactorState(ctx, tx, ref, now)
			if err != nil {
				return err
			}
			switch {
			case !st.Exists:
				outcome = outcomeEnrolmentRequired
				had, err := m.factor.HadEnrolledFactor(ctx, tx, ref)
				if err != nil {
					return err
				}
				if had {
					alerts = append(alerts, newStaffAlert(signalMFAIntegrity, ref, requestID,
						map[string]alerting.AttrValue{"cause": "factor_missing"}))
				}
				return audit.Record(ctx, tx, audit.Entry{
					TenantID: tenantID, ActorType: audit.ActorStaff, ActorID: staff.ID,
					Action: "staff.login_denied", TargetType: "staff_user", TargetID: staff.ID.String(),
					Outcome: audit.OutcomeDenied, IPAddress: ip, UserAgent: ua, RequestID: requestID,
					Metadata: map[string]any{"reason": "mfa_enrolment_required", "previously_enrolled": had},
				})
			case st.HardLocked:
				outcome = outcomeMFALocked
				return auditLocked(ctx, tx, r, requestID, ref, "password", true)
			}
			tok, err := m.store.IssueChallenge(ctx, tx, ref, store.ChallengeMFA, store.OriginLogin, staffVersionUnversioned, now)
			if err != nil {
				return err
			}
			challenge = newStaffChallenge(string(store.ChallengeMFA), tok, challengeExpiresIn, mfaChallengeMethods)
			// staff.login_succeeded now means "a session was issued" (after the second factor, ADR 0119 s14.4 N9);
			// the password step is staff.login_password_verified.
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tenantID, ActorType: audit.ActorStaff, ActorID: staff.ID,
				Action: "staff.login_password_verified", TargetType: "staff_user", TargetID: staff.ID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: ip, UserAgent: ua, RequestID: requestID,
				Metadata: map[string]any{"next": "mfa_challenge"},
			})
		})
		if err != nil {
			logger.Error("staff_login_failed", "error", err)
			writeStaffAuthUnavailable(w, requestID)
			return
		}
		raiseStaffAlerts(r.Context(), deps.DB, alerts)

		switch outcome {
		case outcomeLocked:
			apierror.Write(w, requestID, apierror.CodeRateLimited, "too many failed attempts; try again later")
		case outcomeInvalidCredentials:
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid email or password")
		case outcomeNotActive:
			apierror.Write(w, requestID, apierror.CodeForbidden, "this account cannot currently log in")
		case outcomeEnrolmentRequired:
			apierror.Write(w, requestID, apierror.CodeMFAEnrolmentRequired, "this account has no second factor; ask an administrator for an enrolment link")
		case outcomeMFALocked:
			writeMFAOutcome(w, requestID, mfaLocked)
		default:
			writeStaffSecretJSON(w, http.StatusOK, challenge)
		}
	}
}

// recordAttempt is a small shared wrapper converting a possibly-nil
// tenantID into the *uuid.UUID identity.RecordLoginAttempt expects.
func recordAttempt(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, principalType, identifier, ip string, succeeded bool) error {
	var tenantIDPtr *uuid.UUID
	if tenantID != uuid.Nil {
		tenantIDPtr = &tenantID
	}
	return identity.RecordLoginAttempt(ctx, tx, tenantIDPtr, principalType, identifier, ip, succeeded)
}
