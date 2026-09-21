package httpserver

import "net/http"

// corsAllowedHeaders/corsAllowedMethods are the exact request shapes this
// API's own routes actually use (see openapi/platform-api.yaml) - kept as
// a fixed allowlist rather than echoing whatever the browser asks for in
// Access-Control-Request-Headers/-Method, since a wider allowlist buys
// nothing here and only widens what a compromised or malicious page on an
// allowlisted origin could attempt.
const (
	corsAllowedHeaders = "Authorization, Content-Type, X-Request-Id"
	corsAllowedMethods = "GET, POST, PATCH, PUT, DELETE, OPTIONS"
)

// corsMiddleware adds a minimal, exact-origin-allowlist CORS layer. It
// exists for Stage 9.3 staging, where the B2C and Back Office frontends
// are deliberately deployed on different subdomains than the API
// (docs/architecture/38-deployment-architecture.md's "production hosting
// is a deployment-time decision" note, made concrete) - without this, a
// browser blocks every cross-origin fetch() call before it ever reaches
// this server. An empty allowlist (the default for every existing dev/
// CI/test environment, none of which are cross-origin) makes this a
// complete no-op: no header is ever set, and every request passes through
// unchanged.
//
// This is a browser-enforced convenience, never a security boundary: the
// Origin request header is caller-supplied and unauthenticated - any
// non-browser HTTP client can send an arbitrary value or omit it
// entirely, and this middleware does not gate access based on it (it only
// ever ADDS response headers or short-circuits a preflight; it never
// rejects a request outright). Every route's real authorization decision
// (JWT validation, tenant/permission checks) is completely unaffected by
// Origin and is enforced identically regardless of this middleware's
// presence - see internal/auth for where that actually happens. No
// Access-Control-Allow-Credentials header is ever sent because this
// platform's browser clients never send cookies (Bearer tokens only,
// held in application memory/storage - see b2c/backoffice's
// src/api/client.ts): CORS-with-credentials would additionally require
// per-request Vary/credential handling this API has no reason to carry.
func corsMiddleware(allowedOrigins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(allowedOrigins))
	for _, origin := range allowedOrigins {
		allowed[origin] = struct{}{}
	}

	return func(next http.Handler) http.Handler {
		if len(allowed) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin == "" {
				next.ServeHTTP(w, r)
				return
			}

			// Every response that carries an Origin-dependent header must
			// vary on Origin, or a shared cache (or the browser's own HTTP
			// cache) could serve one origin's CORS-approved response to a
			// different, non-allowlisted origin.
			w.Header().Add("Vary", "Origin")

			if _, ok := allowed[origin]; !ok {
				// Not allowlisted: no CORS headers, so the browser enforces
				// same-origin policy and blocks script access to the
				// response. Still let the request through to the real
				// handler (including OPTIONS, which falls through to the
				// mux like any other method) rather than rejecting it here
				// - this is not an authorization control, and a non-browser
				// caller ignores CORS entirely regardless of what this
				// branch does.
				next.ServeHTTP(w, r)
				return
			}

			w.Header().Set("Access-Control-Allow-Origin", origin)

			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				w.Header().Set("Access-Control-Allow-Methods", corsAllowedMethods)
				w.Header().Set("Access-Control-Allow-Headers", corsAllowedHeaders)
				w.Header().Set("Access-Control-Max-Age", "600")
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
