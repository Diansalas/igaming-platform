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
	mux.Handle("POST /v1/me/email-verification/request", auth.Middleware(deps.AuthIssuer)(newRequestEmailVerificationHandler(deps)))
	mux.HandleFunc("POST /v1/auth/email-verification/confirm", newConfirmEmailVerificationHandler(deps))

	mux.HandleFunc("POST /v1/auth/password-reset/request", newRequestPasswordResetHandler(deps))
	mux.HandleFunc("POST /v1/auth/password-reset/confirm", newConfirmPasswordResetHandler(deps))
}
