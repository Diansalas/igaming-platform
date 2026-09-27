// Package webhookauth holds the provider-neutral inbound-webhook
// authentication contract shared by every provider-facing callback route
// (payments, KYC, casino): docs/decisions/0022 §3 as amended by Stage 10.1
// (points 1-7) and extracted by Stage 10.2 (ADR 0091; design
// docs/plans/stage-10.2-planning/01-webhook-trust-design.md §A).
//
// The code here was MOVED out of internal/payments, not rewritten:
// Credential, Resolver, Inbound, the single closed Reason enum, AuthError,
// the auth sentinels, the platform-defined MOCK wire Scheme, the
// verify-before-parse preamble checks, and the MOCK key-derivation/resolver
// helpers (mock.go). internal/payments keeps type aliases and sentinel
// variables pointing here, so errors.Is/errors.As behave identically across
// the package boundary.
//
// # Scheme is the platform-defined MOCK wire format only (architect R1)
//
// Scheme (Prefix‖0x00‖tenant_id‖0x00‖provider_id‖0x00‖key_id‖0x00‖raw
// body, HMAC-SHA256, header "v1=<64 lowercase hex>") is the wire format the
// platform's own MOCK adapters and in-process test-support signers use. It
// is NEVER a vendor wire format and must not be offered to, or imposed on,
// a real vendor: the platform never invents a generic signature scheme for
// third parties. A real adapter verifies with its vendor's own scheme, but
// still consumes Credential and Inbound and still obeys ADR 0022 §3 points
// 1-7:
//
//  1. the route tenant slug is only a lookup hint;
//  2. exactly one candidate credential, resolved per (tenant, provider,
//     key id), is tried - never a cross-tenant trial;
//  3. the tenant (and provider) is bound into what is verified, or the
//     credential is tenant-unique, and a timestamp tolerance applies;
//  4. no tenant-scoped write (and only the permitted configuration read)
//     happens before verification succeeds;
//  5. every pre-verification failure is one uniform response, the reason
//     going only to an allow-listed log line;
//  6. every posting/effect uses the route-resolved tenant and provider the
//     credential verified, never a payload-asserted value;
//  7. verification runs over the raw bytes before any parsing.
//
// # Per-adapter verification schemes (Stage 10.3 W1a, WH-VENDOR-SCHEME-1)
//
// A real adapter never uses Scheme. It supplies its own VerificationScheme
// (scheme.go: Name/Extract/Verify/Properties) implementing its vendor's
// DOCUMENTED algorithm, and the platform - not the adapter - runs it:
// the shared HTTP preamble selects the scheme by provider id
// (CheckInboundPreamble), and every domain orchestrator's ReceiveCallback
// calls ExtractInbound, ResolveCredentials and VerifyInbound itself before
// the adapter's HandleCallback parses anything, so an adapter cannot skip
// verification. Properties() is validated when the adapter registers
// (NewSchemeSet; a bad declaration fails startup); a real scheme must sign
// a timestamp with MaxSkew <= MaxSkewCap, and must be listed in the
// conformance manifest, which the registry-driven test in
// internal/webhookauth/webhookauthtest only allows once the scheme passes
// RunSchemeConformance (SC1-SC13) with a vendor known-answer vector.
//
// The MOCK Scheme is exposed through the same interface
// (Scheme.VerificationScheme, mock_scheme.go) as a Synthetic scheme with
// its bytes unchanged. A domain accepts only its own canonical MOCK as
// Synthetic, and only from an adapter that is itself a synthetic component
// (NewAdapterSchemeSet). It is NOT the real-provider protocol: no real
// vendor scheme exists in this repository, none is invented here, and a
// real provider is declared supported only once its actual documentation/
// contract is implemented and passes the conformance suite (proposal §22).
//
// # Domain separation (ADR 0022 §3 point 8)
//
// Each domain (payments, KYC, casino) has its own Prefix, its own header
// names and its own mock key label (domains.go), so a signature valid in
// one domain never verifies in another, even under an equal key.
//
// This package deliberately has no orchestrator and no vendor
// implementation, and imports no domain package. Its only registry is the
// per-domain SchemeSet each orchestrator builds from its own adapters.
package webhookauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	// ErrSignatureInvalid is returned by an adapter's callback handling
	// (and by Scheme.Verify) when the inbound callback's authentication
	// does not verify. Never wrap it with the raw payload or any field
	// from it.
	ErrSignatureInvalid = errors.New("webhookauth: callback signature verification failed")

	// ErrAuthFailed is the single sentinel every pre-verification
	// inbound-callback rejection wraps (ADR 0022 §3 point 5). Every
	// reason gets the SAME response on a public webhook route; callers
	// needing the specific reason (for allow-listed structured logging
	// only, never the HTTP response) use errors.As to *AuthError.
	ErrAuthFailed = errors.New("webhookauth: callback authentication failed")

	// ErrCredentialUnavailable is returned by a Resolver when it has no
	// credential for the given (tenantID, providerID, keyID). Callers fold
	// it into ReasonCredentialUnavailable, never surfacing it on its own.
	ErrCredentialUnavailable = errors.New("webhookauth: no webhook credential available for this tenant/provider/key")

	// ErrTenantReaderUnavailable is returned by a TenantReader
	// implementation (ADR 0097 PAYWH-RL-1: internal/httpserver's gatedReader,
	// wrapping the pre-verification DB admission gate) when a
	// WithTenantReadOnly call was refused because of a CAPACITY limit -
	// never because of a real database error or a missing/invalid
	// credential. This is deliberately a DIFFERENT sentinel from
	// ErrCredentialUnavailable's "fold everything else into the same
	// closed reason" rule (security review C1 of PRH-I4, HIGH): a Resolver
	// MUST check for this specifically, with errors.Is, BEFORE it folds
	// any other WithTenantReadOnly error into ErrCredentialUnavailable, and
	// propagate it distinctly (never wrapped inside ErrCredentialUnavailable)
	// so the eventual caller can answer a retryable 503 instead of the
	// uniform pre-verification 401 - a capacity rejection is not an
	// authentication failure, and collapsing it into one would make a
	// legitimate, correctly-signed callback indistinguishable from a
	// deliberate attack every time the platform is merely busy.
	ErrTenantReaderUnavailable = errors.New("webhookauth: tenant reader unavailable (admission capacity)")
)

// Reason is the single, closed, allow-listed reason enum behind
// ErrAuthFailed. Logged (never returned to an unauthenticated caller).
// Each domain documents the subset it emits; ReasonProviderNotConfigured
// and ReasonKeyMaterial are payments-only (ADR 0022 §4.1). Do not split
// this enum per domain (architect review, Stage 10.2).
type Reason string

const (
	ReasonTenantUnknown         Reason = "tenant_unknown"
	ReasonTenantInactive        Reason = "tenant_inactive"
	ReasonProviderInvalid       Reason = "provider_invalid"
	ReasonProviderUnregistered  Reason = "provider_unregistered"
	ReasonProviderNotConfigured Reason = "provider_not_configured"
	ReasonNoResolver            Reason = "no_resolver"
	ReasonCredentialUnavailable Reason = "credential_unavailable"
	ReasonSignatureMissing      Reason = "signature_missing"
	ReasonSignatureInvalid      Reason = "signature_invalid"
	ReasonKeyMaterial           Reason = "key_material"
	// ReasonBodyTooLarge is used by the HTTP preamble only (never by an
	// adapter) for an oversized/unreadable body, checked BEFORE any tenant
	// lookup, so it gets the same uniform rejection as every other
	// pre-verification failure, tenant-independent.
	ReasonBodyTooLarge Reason = "body_too_large"
	// ReasonAdmissionUnavailable (ADR 0097 PAYWH-RL-1, security review C1)
	// is distinct from every reason above: it means the pre-verification
	// DB admission gate refused a WithTenantReadOnly call (capacity, not
	// authentication). A caller MUST map this to a retryable 503 +
	// Retry-After, never the uniform 401 - it is the ONE reason in this
	// enum that is not part of the uniform-401 contract, precisely because
	// it never depends on anything the signature proves.
	ReasonAdmissionUnavailable Reason = "admission_unavailable"
)

// AuthError is ErrAuthFailed's concrete carrier, with the extra, still
// allow-listed context a caller needs for its auth-failure log line: KeyID
// only once it has passed the charset check, CredentialFingerprint only for
// ReasonSignatureInvalid (never the secret itself).
type AuthError struct {
	Reason                Reason
	KeyID                 string
	CredentialFingerprint string
}

func (e *AuthError) Error() string {
	return fmt.Sprintf("webhookauth: callback authentication failed: %s", e.Reason)
}

// Is lets errors.Is(err, ErrAuthFailed) succeed for any *AuthError
// regardless of its specific Reason.
func (e *AuthError) Is(target error) bool {
	return target == ErrAuthFailed
}

// Credential is a resolved, per-(tenant, provider, key) inbound webhook
// verification credential. Secret is never logged, errored, or audited -
// String()/GoString()/Format()/LogValue()/MarshalJSON() redact it, and
// Fingerprint is the only loggable form: hex(sha256(Secret))[:16] for the
// MOCK credentials (see Fingerprint), the keyed "fp1:" HMAC fingerprint of
// ADR 0093 §2 for a real, handle-backed credential.
type Credential struct {
	TenantID    uuid.UUID
	ProviderID  string
	KeyID       string
	Secret      []byte
	Fingerprint string
	// BoundAccountID is the vendor merchant account id this credential is
	// bound to, for BindingPerMerchantKeySignedAccount schemes (ADR 0022
	// §3 point 3; conformance SC8). "" for every other binding. Not secret.
	BoundAccountID string
	// NotAfter bounds a verify_only (Previous) credential's rotation-overlap
	// window (security C4; conformance SC9). Zero for an active credential;
	// a Previous credential with a zero NotAfter is never used.
	NotAfter time.Time
	// HandleID is the provider_credential_handles row this credential was
	// resolved from (ADR 0094 §4.1, security condition C4): the domain
	// transaction re-checks exactly this handle after verification
	// (Resolver.Recheck). uuid.Nil for a MOCK credential, which has no
	// handle; a real Recheck fails closed on uuid.Nil. Not secret.
	HandleID uuid.UUID
}

// String implements fmt.Stringer so %v/%+v/Println of a Credential
// (including inside a larger struct) never renders Secret's raw bytes.
func (c Credential) String() string {
	return fmt.Sprintf("WebhookCredential{TenantID:%s ProviderID:%s KeyID:%s Fingerprint:%s}",
		c.TenantID, c.ProviderID, c.KeyID, c.Fingerprint)
}

// LogValue implements slog.LogValuer for the identical reason as String.
func (c Credential) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("tenant_id", c.TenantID.String()),
		slog.String("provider_id", c.ProviderID),
		slog.String("key_id", c.KeyID),
		slog.String("fingerprint", c.Fingerprint),
	)
}

// GoString implements fmt.GoStringer so `%#v` (which bypasses Stringer)
// also never renders Secret's raw bytes.
func (c Credential) GoString() string {
	return c.String()
}

// Fingerprint returns the only loggable form of a secret:
// hex(sha256(secret))[:16].
func Fingerprint(secret []byte) string {
	sum := sha256.Sum256(secret)
	return hex.EncodeToString(sum[:])[:16]
}

// Inbound is the platform-wide inbound-provider-callback shape. Header
// carries the raw request headers and Body the raw wire bytes - both input
// only, never persisted or logged verbatim. TenantID/ProviderID are always
// the ROUTE-resolved values being verified against, never a value read
// from Body.
type Inbound struct {
	TenantID   uuid.UUID
	ProviderID string
	Header     http.Header
	Body       []byte
}

var (
	// keyIDPattern is the key id charset every scheme's key-id header and
	// every resolver's key ids must satisfy.
	keyIDPattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	// signatureHeaderPattern matches the WHOLE signature header value:
	// "v1=" plus exactly 64 lowercase hex characters (32 bytes).
	signatureHeaderPattern = regexp.MustCompile(`^v1=[0-9a-f]{64}$`)
	// providerIDPattern is the path-segment charset a provider_id must
	// satisfy before any tenant/DB work runs. Unexported (Stage 10.2 final
	// review, K9/L5): an exported *regexp.Regexp is a package-level
	// variable a caller could reassign, weakening the charset for the
	// whole process. Callers use ValidProviderID.
	providerIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
)

// ValidProviderID reports whether providerID satisfies the webhook
// path-segment charset, checked BEFORE any tenant lookup or DB work.
func ValidProviderID(providerID string) bool {
	return providerIDPattern.MatchString(providerID)
}

// Scheme is the platform-defined MOCK webhook wire scheme for one domain.
// See the package doc: it is never a vendor wire format.
type Scheme struct {
	// Prefix is the domain-separation prefix at the head of the signing
	// input, e.g. "igaming.payments.webhook.v1".
	Prefix string
	// SignatureHeader carries "v1=<64 lowercase hex>".
	SignatureHeader string
	// KeyIDHeader carries the key id (^[a-z0-9-]{1,32}$).
	KeyIDHeader string
}

// ParseHeaders extracts and format-validates the scheme's two
// authentication headers. It never touches the database or any
// tenant-scoped state. ok is false for every malformed/missing case;
// reason is ReasonSignatureMissing for an absent header and
// ReasonSignatureInvalid for a present-but-malformed one.
func (s Scheme) ParseHeaders(h http.Header) (keyID, sigHex string, reason Reason, ok bool) {
	sigHeader := h.Get(s.SignatureHeader)
	keyID = h.Get(s.KeyIDHeader)
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

// SigningInput builds Prefix 0x00 tenant_id 0x00 provider_id 0x00 key_id
// 0x00 <raw body bytes>. None of the leading values can contain 0x00 (a
// canonical lowercase UUID string; provider_id and key_id are
// charset-restricted), so body is an unambiguous tail - the raw bytes are
// signed directly, never a re-serialization of parsed fields.
func (s Scheme) SigningInput(tenantID uuid.UUID, providerID, keyID string, body []byte) []byte {
	buf := make([]byte, 0, len(s.Prefix)+36+len(providerID)+len(keyID)+len(body)+8)
	buf = append(buf, s.Prefix...)
	buf = append(buf, 0)
	buf = append(buf, tenantID.String()...)
	buf = append(buf, 0)
	buf = append(buf, providerID...)
	buf = append(buf, 0)
	buf = append(buf, keyID...)
	buf = append(buf, 0)
	buf = append(buf, body...)
	return buf
}

// Sign returns hex(HMAC-SHA256(key, SigningInput(...))). For MOCK
// adapters and in-process test-support signers only.
func (s Scheme) Sign(key []byte, tenantID uuid.UUID, providerID, keyID string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	// hash.Hash.Write never returns an error.
	_, _ = mac.Write(s.SigningInput(tenantID, providerID, keyID, body))
	return hex.EncodeToString(mac.Sum(nil))
}

// SetHeaders writes the scheme's two authentication headers.
func (s Scheme) SetHeaders(h http.Header, keyID, sigHex string) {
	h.Set(s.SignatureHeader, "v1="+sigHex)
	h.Set(s.KeyIDHeader, keyID)
}

// Verify checks in's signature against cred over the raw body, BEFORE any
// parsing (ADR 0022 §3 point 7). The header key id must equal cred.KeyID,
// and cred must be bound to the same (TenantID, ProviderID) as in - the
// signing input is rebuilt from in.TenantID/in.ProviderID (the route-
// resolved values), never from the body. The comparison is constant-time
// (hmac.Equal). Every failure returns exactly ErrSignatureInvalid.
func (s Scheme) Verify(cred Credential, in Inbound) error {
	if s.Prefix == "" || len(cred.Secret) == 0 {
		return ErrSignatureInvalid
	}
	keyID, sigHex, _, ok := s.ParseHeaders(in.Header)
	if !ok || keyID != cred.KeyID {
		return ErrSignatureInvalid
	}
	if cred.TenantID != in.TenantID || cred.ProviderID != in.ProviderID {
		return ErrSignatureInvalid
	}
	expectedHex := s.Sign(cred.Secret, in.TenantID, in.ProviderID, keyID, in.Body)
	if !hmac.Equal([]byte(expectedHex), []byte(sigHex)) {
		return ErrSignatureInvalid
	}
	return nil
}

// PreambleResult is the outcome of CheckInboundPreamble. On failure,
// Reason is set and ProviderIDValid/BodyLen carry exactly the allow-listed
// context the caller's auth-failure log line may include.
type PreambleResult struct {
	// Body is the raw request body (only when ok).
	Body []byte
	// BodyLen is the number of body bytes read (0 if the body was never
	// read, or reading failed).
	BodyLen int
	// ProviderIDValid reports whether providerID passed the charset check
	// (only then may it be logged).
	ProviderIDValid bool
	// Reason is the rejection reason (only when !ok).
	Reason Reason
}
