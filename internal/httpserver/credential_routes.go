package httpserver

import (
	"net/http"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// registerCredentialRoutes wires Stage 4F's email-verification and
// password-reset endpoints. Confirm/request-password-reset are
// unauthenticated (a player has no session yet when confirming, or may
// have forgotten their password entirely) - identical posture to
// /v1/auth/register and /v1/auth/login. Requesting email verification IS
// authenticated (the player already has a session at that point, from
// registration or login) and self-service (no permission gate beyond
// ordinary authentication, mirroring the RG self-exclusion endpoints'
// identical "a player has an inherent right to act on their own account"
// posture).
func registerCredentialRoutes(mux *http.ServeMux, deps Deps) {
	mux.Handle("POST /v1/me/email-verification/request", auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newRequestEmailVerificationHandler(deps))))

	// Stage 9 §21: the three UNAUTHENTICATED credential endpoints share one
	// per-IP bucket (ratelimit.go). The request endpoint is already
	// per-ACCOUNT limited (auth.CountRecentCredentialTokens) but nothing
	// stopped one caller walking a list of addresses to mail-bomb them or
	// probe brand membership; the two confirm endpoints had no per-caller
	// bound at all, and password-reset/confirm runs a full Argon2id hash on
	// every request that presents a valid token. The email-verification
	// REQUEST endpoint is deliberately not limited here - it is
	// authenticated and already per-account limited.
	mux.Handle("POST /v1/auth/email-verification/confirm",
		rateLimitFunc(deps.authLimiter, rateBucketCredential, rateLimitCredentialPerMin, newConfirmEmailVerificationHandler(deps)))

	mux.Handle("POST /v1/auth/password-reset/request",
		rateLimitFunc(deps.authLimiter, rateBucketCredential, rateLimitCredentialPerMin, newRequestPasswordResetHandler(deps)))
	mux.Handle("POST /v1/auth/password-reset/confirm",
		rateLimitFunc(deps.authLimiter, rateBucketCredential, rateLimitCredentialPerMin, newConfirmPasswordResetHandler(deps)))
}
