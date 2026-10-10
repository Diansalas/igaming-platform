package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

type tokenPairResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
}

func newTokenPair(deps Deps, session auth.Session, role auth.Role, principalType auth.PrincipalType) (tokenPairResponse, error) {
	accessToken, err := deps.AuthIssuer.Issue(session.PrincipalID.String(), session.TenantID, role, principalType, deps.AccessTokenTTL)
	if err != nil {
		return tokenPairResponse{}, err
	}
	return tokenPairResponse{
		AccessToken: accessToken, RefreshToken: session.RefreshToken,
		TokenType: "Bearer", ExpiresIn: int(deps.AccessTokenTTL.Seconds()),
	}, nil
}

// --- Registration ---

type registerRequest struct {
	BrandSlug string `json:"brand_slug"`
	Email     string `json:"email"`
	Password  string `json:"password"`
}

const minPasswordLen = 8

func newRegisterHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		// Stage 4E: registration must never again create a Person "blindly"
		// ahead of identity resolution (ADR 0027 §6, closing Stage 4D-RG's
		// own P0 finding) - a nil resolver is a deployment/configuration
		// error, not silently-skip-resolution, so this fails closed rather
		// than falling through to Stage 2's old resolution-blind behavior.
		if deps.PersonResolver == nil {
			logger.Error("register_person_resolver_not_configured")
			apierror.Write(w, requestID, apierror.CodeUnavailable, "registration is temporarily unavailable")
			return
		}

		var req registerRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}

		v := validation.New()
		v.RequireNonEmpty("brand_slug", req.BrandSlug)
		v.RequireNonEmpty("email", req.Email)
		if len(req.Password) < minPasswordLen {
			v.Add("password", fmt.Sprintf("must be at least %d characters", minPasswordLen))
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		brand, err := identity.GetBrandBySlug(r.Context(), deps.DB, req.BrandSlug)
		if errors.Is(err, identity.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "unknown brand")
			return
		}
		if err != nil {
			logger.Error("register_brand_lookup_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "registration failed")
			return
		}

		passwordHash, err := auth.HashPassword(req.Password)
		if err != nil {
			logger.Error("register_hash_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "registration failed")
			return
		}

		ip, ua := clientIP(r), r.UserAgent()
		var tokens tokenPairResponse
		err = deps.DB.WithTenant(r.Context(), brand.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			account, outcome, err := identityresolution.RegisterPlayerWithResolution(ctx, tx, deps.PersonResolver, identityresolution.RegisterPlayerWithResolutionParams{
				Brand: brand, Email: req.Email, PasswordHash: passwordHash,
				// Verified is deliberately left empty - this HTTP request
				// body carries only unverified, client-supplied claims
				// (email/password), never verified identity evidence
				// (ADR 0027 §3/§10: no real KYC source is integrated yet
				// to populate this from).
				RequestID: requestID, IPAddress: ip, UserAgent: ua,
			})
			if err != nil {
				return err
			}
			session, err := auth.IssueSession(ctx, tx, auth.PrincipalPlayer, account.ID, brand.TenantID, deps.RefreshTokenTTL, ua, ip)
			if err != nil {
				return err
			}
			tokens, err = newTokenPair(deps, session, auth.RolePlayer, auth.PrincipalPlayer)
			if err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: brand.TenantID, ActorType: audit.ActorPlayer, ActorID: account.ID,
				Action: "player.registered", TargetType: "player_account", TargetID: account.ID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: ip, UserAgent: ua, RequestID: requestID,
				Metadata: map[string]any{"registration_outcome": string(outcome)},
			})
		})
		if errors.Is(err, identity.ErrEmailTaken) {
			apierror.Write(w, requestID, apierror.CodeConflict, "an account with this email already exists for this brand")
			return
		}
		if errors.Is(err, identity.ErrNotAcceptingRegistrations) {
			apierror.Write(w, requestID, apierror.CodeBrandNotAcceptingRegistrations, "this brand is not accepting registrations")
			return
		}
		if err != nil {
			logger.Error("register_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "registration failed")
			return
		}

		writeJSON(w, http.StatusCreated, tokens)
	}
}

// --- Login ---

type loginRequest struct {
	BrandSlug string `json:"brand_slug"`
	Email     string `json:"email"`
	Password  string `json:"password"`
}

func newLoginHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		var req loginRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("brand_slug", req.BrandSlug)
		v.RequireNonEmpty("email", req.Email)
		v.RequireNonEmpty("password", req.Password)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		brand, err := identity.GetBrandBySlug(r.Context(), deps.DB, req.BrandSlug)
		if errors.Is(err, identity.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "unknown brand")
			return
		}
		if err != nil {
			logger.Error("login_brand_lookup_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "login failed")
			return
		}
		// ADR 0112 section 7.3: a pending_launch brand has never been launched. It answers
		// exactly like an unknown brand (404), so an unlaunched brand is not discoverable.
		// Login on a suspended or closed brand stays allowed so players can see balances.
		if brand.Status == identity.StatusPendingLaunch {
			apierror.Write(w, requestID, apierror.CodeNotFound, "unknown brand")
			return
		}

		email := strings.ToLower(strings.TrimSpace(req.Email))
		identifier := brand.ID.String() + ":" + email
		ip, ua := clientIP(r), r.UserAgent()

		const (
			outcomeSuccess = iota
			outcomeLocked
			outcomeInvalidCredentials
			outcomeAccountNotActive
		)
		outcome := outcomeSuccess
		var tokens tokenPairResponse

		err = deps.DB.WithTenant(r.Context(), brand.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			locked, err := identity.IsLockedOut(ctx, tx, identifier)
			if err != nil {
				return err
			}
			if locked {
				outcome = outcomeLocked
				return audit.Record(ctx, tx, audit.Entry{
					TenantID: brand.TenantID, ActorType: audit.ActorSystem,
					Action: "player.login_blocked_lockout", Outcome: audit.OutcomeDenied,
					IPAddress: ip, UserAgent: ua, RequestID: requestID,
					Metadata: map[string]any{"identifier": identifier},
				})
			}

			account, err := identity.GetPlayerAccountByEmail(ctx, tx, brand.ID, email)
			if errors.Is(err, identity.ErrNotFound) {
				// Spend the same Argon2 cost as a real wrong-password
				// check would, so this branch isn't distinguishable from
				// one by response timing (see auth.DummyPasswordHash).
				_, _ = auth.VerifyPassword(req.Password, auth.DummyPasswordHash)
				outcome = outcomeInvalidCredentials
				if err := identity.RecordLoginAttempt(ctx, tx, &brand.TenantID, "player", identifier, ip, false); err != nil {
					return err
				}
				// Deliberately no "identifier" in metadata here (unlike
				// the branches below): this email never had an account,
				// so logging it would put a non-user's email address into
				// a record tenant compliance staff can read - see
				// docs/architecture/16-privacy.md's data-minimization
				// stance. The action name, tenant, and timestamp are
				// enough to spot a credential-stuffing pattern.
				return audit.Record(ctx, tx, audit.Entry{
					TenantID: brand.TenantID, ActorType: audit.ActorSystem,
					Action: "player.login_failed", Outcome: audit.OutcomeFailure,
					IPAddress: ip, UserAgent: ua, RequestID: requestID,
					Metadata: map[string]any{"reason": "unknown_email"},
				})
			}
			if err != nil {
				return err
			}

			passwordOK, err := auth.VerifyPassword(req.Password, account.PasswordHash)
			if err != nil {
				return err
			}
			if !passwordOK {
				outcome = outcomeInvalidCredentials
				if err := identity.RecordLoginAttempt(ctx, tx, &brand.TenantID, "player", identifier, ip, false); err != nil {
					return err
				}
				return audit.Record(ctx, tx, audit.Entry{
					TenantID: brand.TenantID, ActorType: audit.ActorPlayer, ActorID: account.ID,
					Action: "player.login_failed", TargetType: "player_account", TargetID: account.ID.String(),
					Outcome: audit.OutcomeFailure, IPAddress: ip, UserAgent: ua, RequestID: requestID,
					Metadata: map[string]any{"reason": "invalid_password"},
				})
			}

			if account.Status != identity.PlayerStatusActive && account.Status != identity.PlayerStatusPendingVerification {
				outcome = outcomeAccountNotActive
				if err := identity.RecordLoginAttempt(ctx, tx, &brand.TenantID, "player", identifier, ip, false); err != nil {
					return err
				}
				return audit.Record(ctx, tx, audit.Entry{
					TenantID: brand.TenantID, ActorType: audit.ActorPlayer, ActorID: account.ID,
					Action: "player.login_denied", TargetType: "player_account", TargetID: account.ID.String(),
					Outcome: audit.OutcomeDenied, IPAddress: ip, UserAgent: ua, RequestID: requestID,
					Metadata: map[string]any{"reason": "account_status", "status": string(account.Status)},
				})
			}

			if err := identity.RecordLoginAttempt(ctx, tx, &brand.TenantID, "player", identifier, ip, true); err != nil {
				return err
			}
			session, err := auth.IssueSession(ctx, tx, auth.PrincipalPlayer, account.ID, brand.TenantID, deps.RefreshTokenTTL, ua, ip)
			if err != nil {
				return err
			}
			tokens, err = newTokenPair(deps, session, auth.RolePlayer, auth.PrincipalPlayer)
			if err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: brand.TenantID, ActorType: audit.ActorPlayer, ActorID: account.ID,
				Action: "player.login_succeeded", TargetType: "player_account", TargetID: account.ID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: ip, UserAgent: ua, RequestID: requestID,
			})
		})
		if err != nil {
			logger.Error("login_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "login failed")
			return
		}

		switch outcome {
		case outcomeLocked:
			apierror.Write(w, requestID, apierror.CodeRateLimited, "too many failed attempts; try again later")
		case outcomeInvalidCredentials:
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid email or password")
		case outcomeAccountNotActive:
			apierror.Write(w, requestID, apierror.CodeForbidden, "this account cannot currently log in")
		default:
			writeJSON(w, http.StatusOK, tokens)
		}
	}
}

// --- Refresh ---

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

func newRefreshHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		var req refreshRequest
		if err := decodeJSON(r, &req); err != nil || req.RefreshToken == "" {
			apierror.Write(w, requestID, apierror.CodeValidation, "refresh_token is required")
			return
		}
		ip, ua := clientIP(r), r.UserAgent()

		// The refresh token alone determines the principal and tenant -
		// it is presented without a bearer access token (the access
		// token may already be expired, which is the whole point of
		// refreshing). See internal/auth/session.go's RotateSession for
		// the two-phase lookup-then-scoped-mutate this requires.
		session, err := auth.RotateSession(r.Context(), deps.DB, req.RefreshToken, deps.RefreshTokenTTL, ua, ip)
		switch {
		case errors.Is(err, auth.ErrSessionReused):
			logger.Warn("refresh_token_reuse_detected", "request_id", requestID)
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "refresh token already used; session revoked")
			return
		case errors.Is(err, auth.ErrSessionNotFound):
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid or expired refresh token")
			return
		case err != nil:
			logger.Error("refresh_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "refresh failed")
			return
		}

		// Re-check the principal's CURRENT role/status rather than
		// trusting anything about the old token - a staff user suspended
		// since their last login must not get a working access token
		// just because their refresh token was still technically valid.
		var role auth.Role
		var scopeFn = deps.DB.WithoutTenant
		if session.TenantID != uuid.Nil {
			scopeFn = func(ctx context.Context, fn db.TxFunc) error { return deps.DB.WithTenant(ctx, session.TenantID, fn) }
		}
		err = scopeFn(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
			switch session.PrincipalType {
			case auth.PrincipalPlayer:
				account, err := identity.GetPlayerAccountByID(ctx, tx, session.PrincipalID)
				if err != nil {
					return err
				}
				if account.Status != identity.PlayerStatusActive && account.Status != identity.PlayerStatusPendingVerification {
					return errPrincipalNotActive
				}
				role = auth.RolePlayer
			case auth.PrincipalStaff:
				staff, err := identity.GetStaffUserByID(ctx, tx, session.PrincipalID)
				if err != nil {
					return err
				}
				if staff.Status != "active" {
					return errPrincipalNotActive
				}
				role = auth.Role(staff.Role)
			}
			return nil
		})
		if errors.Is(err, errPrincipalNotActive) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "this account cannot currently authenticate")
			return
		}
		if err != nil {
			logger.Error("refresh_role_lookup_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "refresh failed")
			return
		}

		tokens, err := newTokenPair(deps, session, role, session.PrincipalType)
		if err != nil {
			logger.Error("refresh_token_issue_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "refresh failed")
			return
		}
		writeJSON(w, http.StatusOK, tokens)
	}
}

var errPrincipalNotActive = errors.New("httpserver: principal is not active")

// --- Logout ---

func newLogoutHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		var req refreshRequest
		if err := decodeJSON(r, &req); err != nil || req.RefreshToken == "" {
			apierror.Write(w, requestID, apierror.CodeValidation, "refresh_token is required")
			return
		}

		if err := auth.RevokeSessionByToken(r.Context(), deps.DB, req.RefreshToken); err != nil {
			logger.Error("logout_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "logout failed")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// --- Self-service profile and sessions ---

type meResponse struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Status   string `json:"status"`
	KYCTier  int    `json:"kyc_tier"`
	TenantID string `json:"tenant_id"`
	BrandID  string `json:"brand_id"`
}

func newMeHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil || tc.PrincipalType != string(auth.PrincipalPlayer) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "this endpoint is for player accounts only")
			return
		}
		subjectID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid token subject")
			return
		}

		var resp meResponse
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			account, err := identity.GetPlayerAccountByID(ctx, tx, subjectID)
			if err != nil {
				return err
			}
			resp = meResponse{
				ID: account.ID.String(), Email: account.Email, Status: string(account.Status),
				KYCTier: account.KYCTier, TenantID: account.TenantID.String(), BrandID: account.BrandID.String(),
			}
			return nil
		})
		if errors.Is(err, identity.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "account not found")
			return
		}
		if err != nil {
			logger.Error("me_lookup_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to load profile")
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

type sessionResponse struct {
	ID        string `json:"id"`
	ExpiresAt string `json:"expires_at"`
}

func newListSessionsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		subjectID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid token subject")
			return
		}

		var resp []sessionResponse
		err = deps.DB.WithPrincipalScope(r.Context(), tc.TenantID, subjectID, func(ctx context.Context, tx pgx.Tx) error {
			sessions, err := auth.ListActiveSessions(ctx, tx, auth.PrincipalType(tc.PrincipalType), subjectID)
			if err != nil {
				return err
			}
			resp = make([]sessionResponse, 0, len(sessions))
			for _, s := range sessions {
				resp = append(resp, sessionResponse{ID: s.ID.String(), ExpiresAt: s.ExpiresAt.Format(time.RFC3339)})
			}
			return nil
		})
		if err != nil {
			logger.Error("list_sessions_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list sessions")
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func newRevokeSessionHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		subjectID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid token subject")
			return
		}
		sessionID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid session id")
			return
		}

		err = deps.DB.WithPrincipalScope(r.Context(), tc.TenantID, subjectID, func(ctx context.Context, tx pgx.Tx) error {
			return auth.RevokeSession(ctx, tx, sessionID, subjectID)
		})
		if errors.Is(err, auth.ErrSessionNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "session not found")
			return
		}
		if err != nil {
			logger.Error("revoke_session_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to revoke session")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
