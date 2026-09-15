// Stage 4F: email verification and password reset. See
// docs/decisions/0030-email-verification-and-password-reset.md.
//
// Anti-enumeration discipline (directive §12/§13's explicit requirement)
// runs through every handler here: password-reset-request ALWAYS returns
// the same response whether or not the email matches an account, and
// rate-limiting a request for an email that DOES match an account is
// applied by silently skipping token issuance rather than returning a
// distinguishable status code - a different response for "rate limited"
// vs "no such account" would itself leak which one applied.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/email"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

const (
	emailVerificationTokenTTL   = 24 * time.Hour
	passwordResetTokenTTL       = 1 * time.Hour
	credentialTokenRateWindow   = 1 * time.Hour
	credentialTokenRateMaxCount = 3
)

// --- Email verification ---

func newRequestEmailVerificationHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if deps.EmailProvider == nil {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "email verification is temporarily unavailable")
			return
		}

		tc, err := tenant.FromContext(r.Context())
		if err != nil || tc.PrincipalType != string(auth.PrincipalPlayer) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "this endpoint is for player accounts only")
			return
		}
		playerAccountID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid token subject")
			return
		}
		ip := clientIP(r)

		var recipient, rawToken string
		var rateLimited bool
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			account, err := identity.GetPlayerAccountByID(ctx, tx, playerAccountID)
			if err != nil {
				return err
			}
			count, err := auth.CountRecentCredentialTokens(ctx, tx, playerAccountID, auth.PurposeEmailVerification, credentialTokenRateWindow)
			if err != nil {
				return err
			}
			if count >= credentialTokenRateMaxCount {
				rateLimited = true
				return nil
			}
			tok, err := auth.IssueCredentialToken(ctx, tx, tc.TenantID, playerAccountID, auth.PurposeEmailVerification, emailVerificationTokenTTL, ip)
			if err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, audit.Entry{
				TenantID: tc.TenantID, ActorType: audit.ActorPlayer, ActorID: playerAccountID,
				Action: "email_verification.requested", TargetType: "player_account", TargetID: playerAccountID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: ip, UserAgent: r.UserAgent(), RequestID: requestID,
			}); err != nil {
				return err
			}
			recipient, rawToken = account.Email, tok.RawToken
			return nil
		})
		if errors.Is(err, identity.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "player not found")
			return
		}
		if err != nil {
			logger.Error("request_email_verification_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to request email verification")
			return
		}
		if rateLimited {
			// Same 204 as success - see this file's own top-level doc
			// comment on why rate-limiting must not be distinguishable
			// from success at the HTTP layer.
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if err := deps.EmailProvider.Send(r.Context(), email.Message{
			To: recipient, Subject: "Verify your email",
			Body: fmt.Sprintf("Use this code to verify your email: %s", rawToken),
		}); err != nil {
			logger.Error("send_email_verification_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to send verification email")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

type confirmEmailVerificationRequest struct {
	Token string `json:"token"`
}

func newConfirmEmailVerificationHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		var req confirmEmailVerificationRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("token", req.Token)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		err := auth.ValidateAndConsumeCredentialToken(r.Context(), deps.DB, req.Token, auth.PurposeEmailVerification,
			func(ctx context.Context, tx pgx.Tx, tenantID, playerAccountID uuid.UUID) error {
				if err := identity.SetPlayerAccountEmailVerified(ctx, tx, playerAccountID); err != nil {
					return err
				}
				// Best-effort: only applies if the account is still
				// pending_verification (e.g. not if it's already active,
				// suspended, self_excluded, or identity_review_required) -
				// email verification and account status are otherwise
				// orthogonal facts once the account has moved on from its
				// initial post-registration state.
				if _, err := identity.SetPlayerAccountStatusIfCurrent(ctx, tx, playerAccountID, identity.PlayerStatusPendingVerification, identity.PlayerStatusActive); err != nil {
					return err
				}
				return audit.Record(ctx, tx, audit.Entry{
					TenantID: tenantID, ActorType: audit.ActorPlayer, ActorID: playerAccountID,
					Action: "email_verification.completed", TargetType: "player_account", TargetID: playerAccountID.String(),
					Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				})
			})
		if errors.Is(err, auth.ErrCredentialTokenNotFound) {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid or expired verification token")
			return
		}
		if err != nil {
			logger.Error("confirm_email_verification_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to confirm email verification")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// --- Password reset ---

type requestPasswordResetRequest struct {
	BrandSlug string `json:"brand_slug"`
	Email     string `json:"email"`
}

// newRequestPasswordResetHandler ALWAYS returns 204 for a request against
// a valid, active brand (directive §13's "do not reveal whether an email
// belongs to an account") - whether the email matched an account, was
// rate-limited, or genuinely doesn't exist is never distinguishable from
// the response alone.
func newRequestPasswordResetHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if deps.EmailProvider == nil {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "password reset is temporarily unavailable")
			return
		}

		var req requestPasswordResetRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("brand_slug", req.BrandSlug)
		v.RequireNonEmpty("email", req.Email)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		brand, err := identity.GetBrandBySlug(r.Context(), deps.DB, req.BrandSlug)
		if errors.Is(err, identity.ErrNotFound) {
			// Brand slug is a PUBLIC identifier (identical precedent to
			// register/login) - unlike email, revealing an unknown brand
			// slug is not an account-enumeration risk.
			apierror.Write(w, requestID, apierror.CodeNotFound, "unknown brand")
			return
		}
		if err != nil {
			logger.Error("password_reset_request_brand_lookup_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to process request")
			return
		}

		ip := clientIP(r)
		var recipient, rawToken string
		err = deps.DB.WithTenant(r.Context(), brand.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			account, err := identity.GetPlayerAccountByEmail(ctx, tx, brand.ID, req.Email)
			if errors.Is(err, identity.ErrNotFound) {
				return nil // silently no-op - see this handler's own doc comment
			}
			if err != nil {
				return err
			}
			count, err := auth.CountRecentCredentialTokens(ctx, tx, account.ID, auth.PurposePasswordReset, credentialTokenRateWindow)
			if err != nil {
				return err
			}
			if count >= credentialTokenRateMaxCount {
				return nil // silently no-op - see this handler's own doc comment
			}
			tok, err := auth.IssueCredentialToken(ctx, tx, brand.TenantID, account.ID, auth.PurposePasswordReset, passwordResetTokenTTL, ip)
			if err != nil {
				return err
			}
			if err := audit.Record(ctx, tx, audit.Entry{
				TenantID: brand.TenantID, ActorType: audit.ActorPlayer, ActorID: account.ID,
				Action: "password_reset.requested", TargetType: "player_account", TargetID: account.ID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: ip, UserAgent: r.UserAgent(), RequestID: requestID,
			}); err != nil {
				return err
			}
			recipient, rawToken = account.Email, tok.RawToken
			return nil
		})
		if err != nil {
			logger.Error("password_reset_request_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to process request")
			return
		}

		if rawToken != "" {
			if err := deps.EmailProvider.Send(r.Context(), email.Message{
				To: recipient, Subject: "Reset your password",
				Body: fmt.Sprintf("Use this code to reset your password: %s", rawToken),
			}); err != nil {
				logger.Error("send_password_reset_email_failed", "error", err)
				// Still respond 204 - see this handler's own doc comment;
				// a delivery failure is logged, not surfaced to the caller.
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

type confirmPasswordResetRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

func newConfirmPasswordResetHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		var req confirmPasswordResetRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("token", req.Token)
		if len(req.NewPassword) < minPasswordLen {
			v.Add("new_password", fmt.Sprintf("must be at least %d characters", minPasswordLen))
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		// Token validated/consumed BEFORE the Argon2id hash is computed -
		// security specialist review finding, Stage 4F: an earlier version
		// of this handler hashed first and its own comment claimed that
		// let an invalid token "fail fast without paying that cost," which
		// was backwards - hashing ran on EVERY request regardless of
		// whether the token was ever valid, since it happened unconditionally
		// before validation. On this unauthenticated, rate-limit-free route,
		// that made Argon2id's own deliberate memory/CPU cost (internal/
		// auth.HashPassword's fixed parameters) trivially triggerable by
		// anyone posting garbage tokens - a real, uncosted DoS surface.
		// Consuming the token first means only a request that ALREADY holds
		// a genuinely valid, unconsumed token ever reaches the hash at all;
		// the anti-timing rationale the old comment invoked does not apply
		// here regardless, since the token is a 256-bit unguessable secret,
		// not a low-entropy value an attacker is trying to distinguish by
		// response time.
		err := auth.ValidateAndConsumeCredentialToken(r.Context(), deps.DB, req.Token, auth.PurposePasswordReset,
			func(ctx context.Context, tx pgx.Tx, tenantID, playerAccountID uuid.UUID) error {
				passwordHash, err := auth.HashPassword(req.NewPassword)
				if err != nil {
					return fmt.Errorf("hash new password: %w", err)
				}
				if err := identity.SetPlayerAccountPasswordHash(ctx, tx, playerAccountID, passwordHash); err != nil {
					return err
				}
				// Directive §13's "session invalidation where appropriate" -
				// a completed reset forces re-authentication everywhere,
				// the same protective response as detected refresh-token
				// reuse (internal/auth.RevokeAllSessionsForPrincipal's own
				// doc comment).
				if err := auth.RevokeAllSessionsForPrincipal(ctx, tx, auth.PrincipalPlayer, playerAccountID); err != nil {
					return err
				}
				return audit.Record(ctx, tx, audit.Entry{
					TenantID: tenantID, ActorType: audit.ActorPlayer, ActorID: playerAccountID,
					Action: "password_reset.completed", TargetType: "player_account", TargetID: playerAccountID.String(),
					Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				})
			})
		if errors.Is(err, auth.ErrCredentialTokenNotFound) {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid or expired reset token")
			return
		}
		if err != nil {
			logger.Error("confirm_password_reset_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to reset password")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
