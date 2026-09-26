package payments

import (
	"net/http"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// SigningInputPrefix is the platform-defined (MOCK) webhook signing
// scheme's domain-separation prefix for the payments domain
// (docs/decisions/0022 §3 amendment). Exported so a conformance test (ADR
// 0022 §6) can build the identical signing_input an adapter's own
// verification recomputes, without duplicating the literal string.
// Stage 10.2 (ADR 0091): defined by internal/webhookauth's per-domain
// parameters; the value is unchanged.
const SigningInputPrefix = webhookauth.PaymentsSigningPrefix

// HeaderSignature and HeaderKeyID are the two request headers a verified
// inbound payments callback must carry (docs/decisions/0022 §3 amendment
// §2.2). Values unchanged by the Stage 10.2 extraction.
const (
	HeaderSignature = webhookauth.PaymentsSignatureHeader
	HeaderKeyID     = webhookauth.PaymentsKeyIDHeader
)

// paymentsScheme is the payments domain's platform-defined MOCK wire
// scheme (webhookauth package doc: never a vendor format).
var paymentsScheme = webhookauth.PaymentsScheme()

// WebhookScheme returns the payments domain's platform-defined MOCK wire
// scheme - what the HTTP layer's shared webhook preamble checks headers
// against.
func WebhookScheme() webhookauth.Scheme {
	return paymentsScheme
}

// WebhookScheme implements PaymentProvider: the payments platform MOCK
// scheme (byte-identical to Stage 10.1/10.2) exposed as a synthetic
// webhookauth.VerificationScheme. Never a vendor protocol.
func (m *MockProvider) WebhookScheme() webhookauth.VerificationScheme {
	return paymentsScheme.VerificationScheme()
}

// MultiWebhookCredentialResolver composes several per-provider resolvers
// behind the single WebhookCredentialResolver the Orchestrator is
// constructed with (ruling C2/C3). An alias of webhookauth.MultiResolver
// (Stage 10.2): a providerID absent from the map, or mapped to a nil entry
// (architect review PW-6.1), fails closed with
// ErrWebhookCredentialUnavailable, never falling back to any other entry
// and never panicking.
//
// MOCK/TEST WIRING ONLY (Stage 10.1 architect review PW-6, ADR 0022 §3
// amendment Status): NOT a template for the real resolver, which is a
// single platform component - one FORCE-RLS handle table plus a secret
// store, keyed by (tenant_id, provider_id, key_id).
type MultiWebhookCredentialResolver = webhookauth.MultiResolver

// ParseWebhookAuthHeaders extracts and format-validates the payments
// domain's two inbound-callback authentication headers. It never touches
// the database or any tenant-scoped state - a pure, cheap check callers
// run as early as possible (the HTTP handler's shared preamble, before
// any tenant/DB work; the orchestrator again, since payments-package tests
// exercise ReceiveCallback directly without going through the HTTP layer
// at all).
//
// ok is false for every malformed/missing case; reason is
// ReasonSignatureMissing for an absent header and ReasonSignatureInvalid
// for a present-but-malformed one.
func ParseWebhookAuthHeaders(h http.Header) (keyID, sigHex string, reason CallbackAuthReason, ok bool) {
	return paymentsScheme.ParseHeaders(h)
}
