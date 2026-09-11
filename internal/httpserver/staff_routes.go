package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/observability"
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

func newStaffLoginHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

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
				apierror.Write(w, requestID, apierror.CodeInternal, "login failed")
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
		)
		outcome := outcomeSuccess
		var tokens tokenPairResponse

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
			session, err := auth.IssueSession(ctx, tx, auth.PrincipalStaff, staff.ID, tenantID, deps.RefreshTokenTTL, ua, ip)
			if err != nil {
				return err
			}
			tokens, err = newTokenPair(deps, session, auth.Role(staff.Role), auth.PrincipalStaff)
			if err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tenantID, ActorType: audit.ActorStaff, ActorID: staff.ID,
				Action: "staff.login_succeeded", TargetType: "staff_user", TargetID: staff.ID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: ip, UserAgent: ua, RequestID: requestID,
			})
		})
		if err != nil {
			logger.Error("staff_login_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "login failed")
			return
		}

		switch outcome {
		case outcomeLocked:
			apierror.Write(w, requestID, apierror.CodeRateLimited, "too many failed attempts; try again later")
		case outcomeInvalidCredentials:
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid email or password")
		case outcomeNotActive:
			apierror.Write(w, requestID, apierror.CodeForbidden, "this account cannot currently log in")
		default:
			writeJSON(w, http.StatusOK, tokens)
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
