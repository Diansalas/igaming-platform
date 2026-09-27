// PRH-I1 step (b): the provider-neutral payment contract additions from
// ADR 0095 §9 (CallContext, the closed ErrorClass set, and the
// operation manifest), plus the provider-call gate (§3.2, gate.go).
//
// PROV-OUTBOUND-CRED-1 STATUS (S95-C8, INV-IO-11): OutboundCredential and
// OutboundCredentialResolver here are a SEAM, not the finished
// credential-resolution subsystem. ADR 0095 §11 requires the credential
// to be resolved per call, outside any transaction, never held in an
// adapter, and refused while a financial lock is held - none of that
// storage/rotation/derivation machinery is built yet (it lands in a
// later PRH-I1 step). What IS load-bearing now, and tested now: the gate
// (gate.go) calls Resolve() OUTSIDE any transaction (txscope.Held
// enforced), and independently re-checks the binding
// (Credential.TenantID/ProviderID/Domain) before ever calling an
// adapter - so wiring a real resolver in later than this seam is a
// drop-in replacement, not a redesign.
package payments

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// ErrorClass is ADR 0095 §8's closed outcome set. Every outbound call is
// mapped to exactly one of these before phase C ever sees it - a bare Go
// error with no class is never passed to applyEvidence; the gate maps it
// to ErrorClassAmbiguous for a money-moving operation (§3.2 step 7).
type ErrorClass string

const (
	// ErrorClassNotSent means the call provably never reached the
	// provider (credential unavailable, gate refusal, connection refused
	// before any byte was written, or a T12 NotSent that the ADR still
	// maps to Ambiguous - see MapNotSent).
	ErrorClassNotSent ErrorClass = "not_sent"
	// ErrorClassNotProcessed means the vendor documents this specific
	// response as "not processed". Whether that implies NotSent (T5) or
	// Ambiguous (T6) is adapter-declared per code (§8); this package does
	// not resolve that distinction - the adapter returns NotSent or
	// Ambiguous directly, never ErrorClassNotProcessed, so applyEvidence
	// (attempt.go) never needs a third case here. Retained as a named
	// constant only for adapters/tests that want to log the vendor's own
	// classification before translating it.
	ErrorClassNotProcessed ErrorClass = "not_processed"
	// ErrorClassDefiniteDecline is a definite negative from the provider.
	ErrorClassDefiniteDecline ErrorClass = "definite_decline"
	// ErrorClassAmbiguous is anything else: timeout after a possible
	// send, reset, unmapped 5xx, malformed response, panic.
	ErrorClassAmbiguous ErrorClass = "ambiguous"
	// ErrorClassPending is a definite "accepted, with a reference".
	ErrorClassPending ErrorClass = "pending"
	// ErrorClassSucceeded is a definite synchronous success (only valid
	// when the manifest declares SyncSuccessPossible).
	ErrorClassSucceeded ErrorClass = "succeeded"
)

// OutboundCredential is the per-call, resolved-outside-any-tx credential
// handle ADR 0095 §9.1/§11 describes. SEE THE PACKAGE-LEVEL NOTE ABOVE:
// this is a seam. Secret material never appears on this type - only the
// redacted handle/key/fingerprint a real resolver would also expose.
type OutboundCredential struct {
	TenantID   uuid.UUID
	ProviderID string
	// Domain is the caller's own domain name ("payments", "casino" or
	// "kyc" per §3.2 step 4) - never inferred from ProviderID.
	Domain      string
	HandleID    string
	KeyID       string
	Fingerprint string
}

// String/GoString/MarshalJSON never render anything beyond the already-
// redacted fields (S95-C8(c)) - there is no secret field to redact
// FROM on this seam type, but the contract is written the same way a
// real resolver's type must satisfy, so a future swap-in cannot
// regress it.
func (c OutboundCredential) String() string {
	return "OutboundCredential{provider=" + c.ProviderID + " handle=" + c.HandleID + " key=" + c.KeyID + " fp=" + c.Fingerprint + "}"
}
func (c OutboundCredential) GoString() string { return c.String() }
func (c OutboundCredential) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		ProviderID  string `json:"provider_id"`
		HandleID    string `json:"handle_id"`
		KeyID       string `json:"key_id"`
		Fingerprint string `json:"fingerprint"`
	}{c.ProviderID, c.HandleID, c.KeyID, c.Fingerprint})
}

// OutboundCredentialResolver resolves the credential for one outbound
// call. A real implementation (PROV-OUTBOUND-CRED-1, a later PRH-I1
// step) must resolve outside any transaction and never cache the secret
// itself across calls (INV-IO-11); MockCredentialResolver below is the
// synthetic stand-in the MOCK adapter path uses today.
type OutboundCredentialResolver interface {
	Resolve(ctx CallContext, domain string) (OutboundCredential, error)
}

// MockCredentialResolver is a `MOCK` OutboundCredentialResolver: it
// synthesizes a deterministic, non-secret credential handle per
// (tenant, provider) pair with no external I/O and no storage, exactly
// mirroring §9.1's "MOCK: synthetic credential" note. It is never
// wired to a real provider.
type MockCredentialResolver struct{}

// SyntheticComponent implements providerkind.Synthetic (MOCK-ADAPTER-PROD-1):
// PRH-I5 wires this resolver into the MOCK payment statement source, and
// providerkind's completeness scan requires every Mock* type to carry the
// marker.
func (MockCredentialResolver) SyntheticComponent() {}

func (MockCredentialResolver) Resolve(cc CallContext, domain string) (OutboundCredential, error) {
	return OutboundCredential{
		TenantID: cc.TenantID, ProviderID: cc.ProviderID, Domain: domain,
		HandleID: "mock-handle:" + cc.ProviderID, KeyID: "mock-key:" + cc.ProviderID,
		Fingerprint: "mockfp",
	}, nil
}

// CallContext is ADR 0095 §9.1: every outbound request type embeds one.
// TenantID and ProviderID MUST come from the committed attempt row the
// caller just loaded (never a payload, never a cache, never an earlier
// attempt - S95-C9(i)).
type CallContext struct {
	TenantID       uuid.UUID
	ProviderID     string
	Credential     OutboundCredential
	IdempotencyKey string
	Deadline       time.Time
}

// String/GoString/MarshalJSON render only tenant/provider/idempotency
// key and the deadline - the embedded Credential renders through its own
// redacted form, so nothing here can ever leak a secret (S95-C8(c)).
func (c CallContext) String() string {
	b, _ := json.Marshal(c)
	return string(b)
}
func (c CallContext) GoString() string { return c.String() }
func (c CallContext) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		TenantID       uuid.UUID          `json:"tenant_id"`
		ProviderID     string             `json:"provider_id"`
		Credential     OutboundCredential `json:"credential"`
		IdempotencyKey string             `json:"idempotency_key,omitempty"`
		Deadline       time.Time          `json:"deadline"`
	}{c.TenantID, c.ProviderID, c.Credential, c.IdempotencyKey, c.Deadline})
}

// OperationManifest is the code-declared, adapter-owned subset of ADR
// 0095 §10.1 this step needs to drive deposit initiation. It is
// intentionally a STRICT SUBSET of the full manifest §10.1 specifies:
// SupportsPayout, SupportsRefund, SupportsDepositReversalEvents, the
// remaining WebhookRetrySemantics fields, RedeliveryOn401/5xx,
// ErrorClassMapping and StatementSource are NOT modeled here because no
// code in this step reads them - the capability fail-closed-at-
// registration behaviour §10.1 also specifies is PRH-I1 step (d)'s scope
// (kill switch and capability persistence), not this one. Adding those
// fields later is additive, never a breaking change to this type.
type OperationManifest struct {
	SupportsDeposit bool
	// StatusQuery mirrors §10.1's enum as a string for now
	// ("none" | "by_provider_reference" | "by_provider_or_merchant_reference").
	StatusQuery string
	// MerchantLookupAuthoritativeAfter: 0 means a deposit not_found is
	// never treated as authoritative (§4.5).
	MerchantLookupAuthoritativeAfter time.Duration
	IdempotentSubmission             bool
	SyncSuccessPossible              bool
	// Interactive is this step's simplification of §10.1's per-method
	// map: whether THIS request's payment method requires a player to be
	// present for the result (a redirect/hosted-field token). Unknown
	// (zero value) means true (§10.1's own fail-safe default) - callers
	// must set it explicitly, never rely on the zero value meaning false.
	Interactive      bool
	CallTimeout      time.Duration
	SettlementWindow time.Duration
}

// DefaultCallTimeout/DefaultSettlementWindow are §7.3's referenced
// defaults, applied when a manifest leaves the duration at zero.
const (
	DefaultCallTimeout      = 30 * time.Second
	DefaultSettlementWindow = 24 * time.Hour
)
