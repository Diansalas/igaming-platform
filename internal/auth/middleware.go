package auth

import (
	"net/http"
	"strings"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// Middleware verifies the bearer JWT on every request and, on success,
// attaches the resulting tenant.Context. It is the ONLY place tenant_id
// is derived for a request - nothing downstream reads a tenant id from a
// header, path parameter, or body, satisfying CLAUDE.md's "tenant_id is
// authoritative from server-side authenticated context only" rule.
//
// Note that claims.TenantID may be uuid.Nil (a platform-scoped
// principal) - this middleware attaches it as-is; RequireTenantScope (or
// a handler's own check) is what denies a nil-tenant caller from a
// tenant-scoped operation, per docs/decisions/0011.
func Middleware(issuer *Issuer) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := observability.RequestIDFromContext(r.Context())

			authHeader := r.Header.Get("Authorization")
			// The Bearer scheme name is case-insensitive per RFC 7235 -
			// only the prefix casing is normalized here, never the token
			// itself.
			const prefixLen = len("Bearer ")
			if len(authHeader) < prefixLen || !strings.EqualFold(authHeader[:prefixLen], "Bearer ") {
				w.Header().Set("WWW-Authenticate", `Bearer realm="platform-api"`)
				apierror.Write(w, requestID, apierror.CodeUnauthorized, "missing bearer token")
				return
			}
			tokenString := authHeader[prefixLen:]

			claims, err := issuer.Verify(tokenString)
			if err != nil {
				w.Header().Set("WWW-Authenticate", `Bearer realm="platform-api", error="invalid_token"`)
				apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid or expired token")
				return
			}

			ctx := tenant.WithContext(r.Context(), tenant.Context{
				TenantID:      claims.TenantID,
				Role:          string(claims.Role),
				PrincipalType: string(claims.PrincipalType),
				Subject:       claims.Subject,
			})
			observability.SetTenantIDForLogging(ctx, claims.TenantID.String())

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequirePlayerPrincipal denies a request unless the authenticated
// principal is a PLAYER. It is the structural counterpart to
// RequirePermission for the player self-service surface: those routes
// (/v1/me/..., /v1/bonus/...) carry no permission gate at all by design -
// "a player has an inherent right to act on their own account" - and
// instead derive the acting player_account_id from the token's own
// subject.
//
// Before Stage 9 §8 that left the entire /v1/me surface reachable by a
// STAFF bearer token. Nothing was actually disclosed: every such handler
// feeds tc.Subject into a player_accounts lookup or a player-scoped RLS
// GUC, and a staff user's id matches no player_account, so the handlers
// failed safe - some with 404, some with an empty 200. But that safety was
// INCIDENTAL, resting on "no staff id will ever collide with a player id"
// and on every future /v1/me handler remembering to do a lookup that
// happens to fail. CLAUDE.md's rule is that authorization is enforced
// server-side, not inferred - so the principal type a route is written for
// is asserted here, once, rather than re-derived by accident in each
// handler. newMeHandler and the /v1/me/residence handlers already made this
// check inline; this middleware is that same check, applied uniformly.
//
// Returns 403 (not 404): a staff token is a valid, authenticated
// credential presented to an endpoint it has no authority over, which is
// exactly what 403 means. There is no enumeration concern - the response
// is identical for every staff caller and reveals nothing about the path's
// underlying resource.
func RequirePlayerPrincipal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		if tc.PrincipalType != string(PrincipalPlayer) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "this endpoint is for player accounts only")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RequireStaffPrincipal is RequirePlayerPrincipal's structural mirror for
// the opposite surface: it admits EXACTLY PrincipalStaff, denying
// PrincipalPlayer and PrincipalService alike with 403, in
// RequirePlayerPrincipal's own style (same "valid, authenticated
// credential presented to an endpoint it has no authority over" reasoning,
// same 403-not-404 rationale - no enumeration concern, the response is
// identical for every non-staff caller).
//
// Introduced for ADR 0088 §9.1's non-production test-support sportsbook
// settlement-simulation route: RequirePermission alone is not enough there,
// because a permission check only asks "does this role hold the
// permission" - it says nothing about what KIND of principal is
// presenting the token. A permission-bearing role is only ever meant to be
// held by a staff principal, but nothing before this middleware existed
// stopped a differently-typed token from being minted with that role
// string and reaching the handler anyway. This is the same "assert the
// principal type a route is written for, once, rather than re-derive it by
// accident" argument RequirePlayerPrincipal's own doc comment makes.
func RequireStaffPrincipal(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		if tc.PrincipalType != string(PrincipalStaff) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "this endpoint is for staff accounts only")
			return
		}
		next.ServeHTTP(w, r)
	})
}
