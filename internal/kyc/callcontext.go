package kyc

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/txscope"
)

// defaultProviderCallTimeout is CallContext.Deadline's default budget for a
// phase-B provider call (ADR 0095 §15.2/§15.3, §9.1) - informational only
// for the MOCK adapter (which never does real I/O); a real adapter consults
// it to bound its own transport timeout. Mirrors casino's
// defaultLaunchCallTimeout exactly.
const defaultProviderCallTimeout = 10 * time.Second

// CallContext is KYC's own copy of ADR 0095 §9.1's call-context shape,
// scoped to this domain (payments/casino/kyc each get their own copy until
// PRH-I1 lands the shared version - see the ADR 0095 §15.1 implementation
// note, which this mirrors exactly for KYC). Every outbound
// CreateVerification/SubmitVerification call carries one, built fresh per
// call by the phase-B step - never cached, never reused across calls.
type CallContext struct {
	// TenantID is taken from the verification's own tenant (server-side),
	// never a payload.
	TenantID   uuid.UUID
	ProviderID string
	// Credential is resolved per call (PROV-OUTBOUND-CRED-1) outside any
	// database transaction; MOCK adapters get a synthetic credential
	// (providercred.NewMockOutboundCredential) instead of a real
	// handle-table read.
	Credential providercred.OutboundCredential
	// IdempotencyKey is the deterministic external reference/idempotency
	// key ADR 0095 §15.2/§15.3 name for each call: "kv:" + verification id
	// for CreateVerification (PROVIDER DEPENDENT whether the vendor
	// actually honors it - see CreateVerification's own doc comment), and
	// "ks:" + verification id + ":" + sha256(sorted document ids) for
	// SubmitVerification.
	IdempotencyKey string
	Deadline       time.Time
}

// callContextRedacted renders only the credential's own already-redacted
// form plus the non-sensitive fields - never a secret (ADR 0095 §9.1,
// mirroring OutboundCredential's/casino.CallContext's identical redactor
// set).
func (c CallContext) callContextRedacted() string {
	return fmt.Sprintf("CallContext{TenantID:%s ProviderID:%s Credential:%s IdempotencyKey:%s Deadline:%s}",
		c.TenantID, c.ProviderID, c.Credential.String(), c.IdempotencyKey, c.Deadline)
}

func (c CallContext) String() string { return c.callContextRedacted() }

func (c CallContext) GoString() string { return c.callContextRedacted() }

func (c CallContext) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(c.callContextRedacted())) }

// LogValue implements slog.LogValuer (never the secret).
func (c CallContext) LogValue() slog.Value { return slog.StringValue(c.callContextRedacted()) }

// MarshalJSON implements json.Marshaler (never the secret).
func (c CallContext) MarshalJSON() ([]byte, error) { return json.Marshal(c.callContextRedacted()) }

// OutboundCredentialResolver is what CreateVerification/SubmitVerification's
// phase B needs to resolve a per-call outbound credential (PROV-OUTBOUND-
// CRED-1) before calling the KYCProvider. providercred's own
// *(*Subsystem).Outbound("kyc") return value (*providercred.OutboundResolver)
// satisfies this exactly, by having an identical method signature;
// MockOutboundResolver (below) is the "MOCK: synthetic credential" case ADR
// 0095 §9.1 names, wired only behind a synthetic/MOCK provider adapter.
// Mirrors casino.OutboundCredentialResolver exactly.
type OutboundCredentialResolver interface {
	Resolve(ctx context.Context, pool providercred.TenantTxRunner, tenantID uuid.UUID, providerID string) (providercred.OutboundCredential, error)
}

// syntheticKYCAdapter mirrors casino's syntheticCasinoAdapter - a
// structural restatement of providerkind.Synthetic for this package's own
// kind-split resolver, so internal/kyc need not import internal/providerkind.
type syntheticKYCAdapter interface{ SyntheticComponent() }

// MockOutboundResolver implements OutboundCredentialResolver with a
// synthetic, in-memory credential (ADR 0095 §9.1's "MOCK: synthetic
// credential") - it never touches providercred's handle table or secret
// store, mirroring casino.MockOutboundResolver exactly. Wired only behind a
// MOCK/synthetic provider adapter (cmd/platform-api/registrations.go),
// never for a production-eligible one.
type MockOutboundResolver struct{}

// SyntheticComponent implements providerkind.Synthetic (Stage 10.3,
// MOCK-ADAPTER-PROD-1) - the same structural marker MockKYCProvider already
// carries, so the AST completeness scan (internal/providerkind) recognizes
// this as a mock-like type that must never reach production wiring
// unmarked.
func (MockOutboundResolver) SyntheticComponent() {}

// NewMockOutboundResolver constructs a MockOutboundResolver.
func NewMockOutboundResolver() MockOutboundResolver { return MockOutboundResolver{} }

// Resolve implements OutboundCredentialResolver. It ignores pool entirely -
// there is no store to read - and otherwise never fails, matching the MOCK
// adapter's own "never a real credential, never a real failure mode"
// framing - EXCEPT for the one refusal ADR 0095 §11 requires of a MOCK
// resolver too (IO-1B, architect review, INV-IO-1(b)): txscope.Held(ctx)
// refuses exactly like the real providercred.OutboundResolver.Resolve
// does, mirroring casino.MockOutboundResolver's identical fix.
func (MockOutboundResolver) Resolve(ctx context.Context, _ providercred.TenantTxRunner, tenantID uuid.UUID, providerID string) (providercred.OutboundCredential, error) {
	if txscope.Held(ctx) {
		return providercred.OutboundCredential{}, ErrProviderCallRefused
	}
	return providercred.NewMockOutboundCredential(tenantID, "kyc", providerID), nil
}

// OutboundKindSplitResolver is KYC's own copy of casino.OutboundKindSplitResolver
// (security review RV-PRH-I2 C3's fix, applied here too): the outbound-
// credential resolver is chosen by the ADAPTER's own kind (synthetic/MOCK
// vs real), never by "is any mock wired anywhere in this process".
type OutboundKindSplitResolver struct {
	synthetic map[string]bool
	mock      OutboundCredentialResolver
	real      OutboundCredentialResolver
}

// NewOutboundKindSplitResolver builds the split over adapters (the same
// registry NewOrchestrator was built with). mock serves every provider id
// whose adapter is synthetic (nil means none is wired: those calls fail
// closed with providercred.ErrOutboundCredentialUnavailable); real serves
// every other registered adapter (nil likewise). Returns a TRUE nil
// interface when both are nil.
func NewOutboundKindSplitResolver(adapters map[string]KYCProvider, mock, real OutboundCredentialResolver) OutboundCredentialResolver {
	if mock == nil && real == nil {
		return nil
	}
	s := &OutboundKindSplitResolver{synthetic: map[string]bool{}, mock: mock, real: real}
	for id, a := range adapters {
		_, isSynthetic := any(a).(syntheticKYCAdapter)
		s.synthetic[id] = isSynthetic
	}
	return s
}

// Resolve implements OutboundCredentialResolver. A provider id never
// registered in the adapters map this resolver was built from fails closed,
// exactly like casino.OutboundKindSplitResolver.Resolve's identical
// unregistered-id case.
func (s *OutboundKindSplitResolver) Resolve(ctx context.Context, pool providercred.TenantTxRunner, tenantID uuid.UUID, providerID string) (providercred.OutboundCredential, error) {
	isSynthetic, registered := s.synthetic[providerID]
	if !registered {
		return providercred.OutboundCredential{}, providercred.ErrOutboundCredentialUnavailable
	}
	target := s.real
	if isSynthetic {
		target = s.mock
	}
	if target == nil {
		return providercred.OutboundCredential{}, providercred.ErrOutboundCredentialUnavailable
	}
	return target.Resolve(ctx, pool, tenantID, providerID)
}
