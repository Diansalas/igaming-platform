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
				TenantID: claims.TenantID,
				Role:     string(claims.Role),
				Subject:  claims.Subject,
			})
			observability.SetTenantIDForLogging(ctx, claims.TenantID.String())

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireRole is the Stage 1 RBAC skeleton: it denies a request unless
// the authenticated caller's role is one of allowed. Fine-grained,
// per-action permission checks (beyond "is this role allowed to call this
// endpoint at all") belong to the back office/partner console work in
// later stages - this establishes the enforcement point and the pattern
// every future permission check follows: server-side, never inferred
// from what the UI shows.
func RequireRole(allowed ...Role) func(http.Handler) http.Handler {
	allowedSet := make(map[Role]bool, len(allowed))
	for _, r := range allowed {
		allowedSet[r] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := observability.RequestIDFromContext(r.Context())
			tc, err := tenant.FromContext(r.Context())
			if err != nil {
				apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated tenant context")
				return
			}
			if !allowedSet[Role(tc.Role)] {
				apierror.Write(w, requestID, apierror.CodeForbidden, "role not permitted for this operation")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
