package casino

import (
	"net/http"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// casinoScheme is the casino domain's platform-defined MOCK wire scheme
// (webhookauth package doc: never a vendor format). Stage 10.2
// (CAS-WH-TENANT-1, ADR 0091, design §A/§C).
var casinoScheme = webhookauth.CasinoScheme()

// WebhookScheme returns the casino domain's platform-defined MOCK wire
// scheme - what the HTTP layer's shared webhook preamble checks headers
// against.
func WebhookScheme() webhookauth.Scheme {
	return casinoScheme
}

// ParseWebhookAuthHeaders extracts and format-validates the casino
// domain's two inbound-callback authentication headers. It never touches
// the database or any tenant-scoped state - a pure, cheap check callers
// run as early as possible (the HTTP handler's shared preamble, before any
// tenant/DB work; the orchestrator again, since casino-package tests
// exercise ReceiveCallback directly without going through the HTTP layer
// at all).
//
// ok is false for every malformed/missing case; reason is
// ReasonSignatureMissing for an absent header and ReasonSignatureInvalid
// for a present-but-malformed one.
func ParseWebhookAuthHeaders(h http.Header) (keyID, sigHex string, reason webhookauth.Reason, ok bool) {
	return casinoScheme.ParseHeaders(h)
}
