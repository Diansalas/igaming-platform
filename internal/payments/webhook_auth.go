package payments

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// SigningInputPrefix is the platform-defined webhook signing scheme's
// domain-separation prefix (docs/decisions/0022 §3 amendment). Exported so
// a conformance test (ADR 0022 §6) can build the identical signing_input
// an adapter's own verification recomputes, without duplicating the
// literal string.
const SigningInputPrefix = "igaming.payments.webhook.v1"

// HeaderSignature and HeaderKeyID are the two request headers a verified
// inbound callback must carry (docs/decisions/0022 §3 amendment §2.2).
const (
	HeaderSignature = "X-Payments-Signature"
	HeaderKeyID     = "X-Payments-Key-Id"
)

var (
	// keyIDPattern matches KeyID's own charset - also the format WHERE a
	// resolver's own key ids must satisfy it.
	keyIDPattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	// signatureHeaderPattern matches the WHOLE X-Payments-Signature header
	// value, "v1=" plus exactly 64 lowercase hex characters (32 bytes).
	signatureHeaderPattern = regexp.MustCompile(`^v1=[0-9a-f]{64}$`)
	// ProviderIDPattern is the path-segment charset a provider_id must
	// satisfy before any tenant/DB work runs (design §2.2).
	ProviderIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

// MultiWebhookCredentialResolver composes several per-provider resolvers
// behind the single WebhookCredentialResolver the Orchestrator is
// constructed with (ruling C2/C3: exactly one resolver is injected) -
// useful for a deployment or test fixture that registers more than one
// adapter, each with its own resolver, without the Orchestrator itself
// ever holding a provider-keyed map. A providerID absent from the map
// fails closed (ErrWebhookCredentialUnavailable), never falling back to
// any other entry.
type MultiWebhookCredentialResolver map[string]WebhookCredentialResolver

// Resolve implements WebhookCredentialResolver by dispatching to the
// resolver registered for providerID.
func (m MultiWebhookCredentialResolver) Resolve(ctx context.Context, tenantID uuid.UUID, providerID, keyID string) (WebhookCredential, error) {
	r, ok := m[providerID]
	if !ok {
		return WebhookCredential{}, ErrWebhookCredentialUnavailable
	}
	return r.Resolve(ctx, tenantID, providerID, keyID)
}

// ValidProviderIDFormat reports whether providerID satisfies the webhook
// path-segment charset, checked BEFORE any tenant lookup or DB work
// (design §3.1 step 1, ruling 5).
func ValidProviderIDFormat(providerID string) bool {
	return ProviderIDPattern.MatchString(providerID)
}

// ParseWebhookAuthHeaders extracts and format-validates the platform's two
// inbound-callback authentication headers. It never touches the database
// or any tenant-scoped state - a pure, cheap check callers run as early as
// possible (the HTTP handler, before any tenant/DB work; the orchestrator
// again, since payments-package tests exercise ReceiveCallback directly
// without going through the HTTP layer at all).
//
// ok is false for every malformed/missing case; reason is the
// CallbackAuthReason to report (ReasonSignatureMissing for an absent
// header, ReasonSignatureInvalid for a present-but-malformed one,
// including a legacy body-embedded "signature" field's absence not being
// checked here - that is HandleCallback's own job, since it requires
// parsing the body).
func ParseWebhookAuthHeaders(h http.Header) (keyID, sigHex string, reason CallbackAuthReason, ok bool) {
	sigHeader := h.Get(HeaderSignature)
	keyID = h.Get(HeaderKeyID)
	if sigHeader == "" || keyID == "" {
		return "", "", ReasonSignatureMissing, false
	}
	if !keyIDPattern.MatchString(keyID) {
		return "", "", ReasonSignatureInvalid, false
	}
	if !signatureHeaderPattern.MatchString(sigHeader) {
		return "", "", ReasonSignatureInvalid, false
	}
	return keyID, strings.TrimPrefix(sigHeader, "v1="), "", true
}
