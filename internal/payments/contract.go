// PRH-I1 step (b): the provider-neutral payment contract additions from
// ADR 0095 §9 (CallContext, the closed ErrorClass set, and the
// operation manifest), plus the provider-call gate (§3.2, gate.go).
//
// PROV-OUTBOUND-CRED-1 (phase 2 orchestrator wiring): OutboundCredential
// resolution now goes through the REAL internal/providercred subsystem,
// mirroring casino/KYC's identical wiring - a payments-domain
// providercred.OutboundResolver for a real adapter, MockOutboundResolver
// (outbound_resolver.go) for a `MOCK`/synthetic one, chosen per adapter
// kind by OutboundKindSplitResolver (never by "is any mock wired anywhere
// in this process"). The gate (gate.go) still calls Resolve() OUTSIDE any
// transaction (txscope.Held enforced) and independently re-checks the
// binding (Credential.TenantID/ProviderID/Domain) before ever calling an
// adapter. The REAL credential STORE itself (secretstore) remains MOCK/
// sandbox in this deployment - no real vendor credentials, no IRSA/KMS/
// proxy infrastructure - only the resolution PATH is now the real one.
package payments

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/providercred"
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

// OutboundCredentialResolver resolves the credential for one outbound
// call - the payments package's own copy of casino.OutboundCredentialResolver
// / kyc.OutboundCredentialResolver's identical contract (PROV-OUTBOUND-
// CRED-1, phase 2 orchestrator wiring). pool is threaded through from the
// caller's own *db.Pool (never held across the provider call itself - the
// real providercred.OutboundResolver's own short, committed-before-return
// tenant-scoped transaction is the only DB access this makes). A real
// resolver (providercred.Subsystem.Outbound("payments")) and
// MockOutboundResolver below (the MOCK/sandbox stand-in) are both chosen
// per adapter kind by OutboundKindSplitResolver, never by "is any mock
// wired anywhere in this process" - mirrors casino/KYC's identical
// resolver-selection rule exactly.
type OutboundCredentialResolver interface {
	Resolve(ctx context.Context, pool providercred.TenantTxRunner, tenantID uuid.UUID, providerID string) (providercred.OutboundCredential, error)
}

// MockCredentialResolver is a `MOCK` OutboundCredentialResolver: it
// synthesizes a deterministic, non-secret credential handle per
// (tenant, provider) pair with no external I/O and no storage, exactly
// mirroring §9.1's "MOCK: synthetic credential" note. It is never
// wired to a real provider.
type MockCredentialResolver struct{}

// SyntheticComponent implements providerkind.Synthetic. This type is
// reachable from cmd/platform-api's production wiring on two independent
// paths: paymentsOutboundCredentials (PRH-I1 deposit cutover, whenever
// only the MOCK payments adapter is registered) and the MOCK payment
// statement source (PRH-I5, MOCK-ADAPTER-PROD-1) - RefuseSyntheticInProduction's
// own completeness scan requires every mock-like type to declare this
// marker, mirroring casino.MockOutboundResolver's identical declaration.
func (MockCredentialResolver) SyntheticComponent() {}

// Resolve ignores pool entirely (never any real I/O) - it is accepted
// only to satisfy OutboundCredentialResolver's shape, exactly like
// casino.MockOutboundResolver.Resolve's identical signature.
func (MockCredentialResolver) Resolve(_ context.Context, _ providercred.TenantTxRunner, tenantID uuid.UUID, providerID string) (providercred.OutboundCredential, error) {
	return providercred.NewMockOutboundCredential(tenantID, "payments", providerID), nil
}

// CallContext is ADR 0095 §9.1: every outbound request type embeds one.
// TenantID and ProviderID MUST come from the committed attempt row the
// caller just loaded (never a payload, never a cache, never an earlier
// attempt - S95-C9(i)). Credential is the REAL providercred.OutboundCredential
// type (not a package-local seam type) - its own String/GoString/
// MarshalJSON already redact the secret (S95-C8(c)), so this type's own
// render methods need no special handling for it.
type CallContext struct {
	TenantID       uuid.UUID
	ProviderID     string
	Credential     providercred.OutboundCredential
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
		TenantID       uuid.UUID                       `json:"tenant_id"`
		ProviderID     string                          `json:"provider_id"`
		Credential     providercred.OutboundCredential `json:"credential"`
		IdempotencyKey string                          `json:"idempotency_key,omitempty"`
		Deadline       time.Time                       `json:"deadline"`
	}{c.TenantID, c.ProviderID, c.Credential, c.IdempotencyKey, c.Deadline})
}

// OperationManifest is the code-declared, adapter-owned subset of ADR
// 0095 §10.1. It is still NOT the full manifest §10.1 specifies:
// SupportsPayout, SupportsDepositReversalEvents, the remaining
// WebhookRetrySemantics fields (already covered separately - see
// webhookauth.WebhookRetrySemantics/MustRequireRetrySemantics, wired at
// NewOrchestrator), RedeliveryOn401/5xx, ErrorClassMapping and
// StatementSource are deliberately still NOT modeled here (PRH-I1 round 2):
// none of them yet has a real, tested enforcement point that reads the
// field - adding an unread struct field would be exactly the "capability
// nothing can actually use" CLAUDE.md's no-fake-completion rule warns
// against. Each is registered as a deferred item in
// docs/governance/task-registry.md (PRH-I1-MANIFEST-*) instead of being
// added here unread. SupportsRefund and CallbackEchoesMerchantReference ARE
// added below because each gets a real registration-time refusal
// (validateManifest, capability.go) that is unit-tested.
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
	// SupportsRefund is ADR 0095 §5.5: NOT IMPLEMENTED, and not built by
	// PRH. Registration (validateManifest) refuses ANY adapter declaring
	// true, regardless of anything else about it - "the contract reserves
	// Refund(...), and the manifest has supports_refund = false for every
	// adapter. Registration refuses an adapter declaring true until a flow
	// exists."
	SupportsRefund bool
	// CallbackEchoesMerchantReference is §10.1's LF95-C5 field: true means
	// this adapter's callback always carries back the merchant_reference
	// the platform sent it, so a success can always be convergeable even if
	// the provider_reference alone is ambiguous. Registration
	// (validateManifest) refuses a PRODUCTION-ELIGIBLE (non-Synthetic)
	// adapter that supports deposit or withdrawal unless this is true (B7:
	// StatusQuery "by_provider_or_merchant_reference" no longer satisfies it -
	// the interface has no merchant-reference status method until adapter
	// acceptance criterion A7) - a MOCK/Synthetic adapter is exempt (there is
	// no real vendor contract to check yet).
	CallbackEchoesMerchantReference bool
}

// DefaultCallTimeout/DefaultSettlementWindow are §7.3's referenced
// defaults, applied when a manifest leaves the duration at zero.
const (
	DefaultCallTimeout      = 30 * time.Second
	DefaultSettlementWindow = 24 * time.Hour

	// MaxSettlementWindow is the registration ceiling for a manifest's
	// SettlementWindow (A1): a huge value would silently disable the B6/B7
	// escalation (and could overflow window + lease arithmetic). 30 days is far
	// beyond any card/bank/crypto settlement horizon; a vendor needing more is a
	// decision for the architect, not a manifest value.
	MaxSettlementWindow = 30 * 24 * time.Hour
)
