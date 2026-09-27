// PROV-OUTBOUND-CRED-1, phase 2 orchestrator wiring: the payments-domain
// outbound-credential kind split, mirroring casino.OutboundKindSplitResolver
// and kyc.OutboundKindSplitResolver exactly - the resolver used for a
// given call is chosen by the ADAPTER's own kind (synthetic/MOCK vs
// real), never by "is any mock wired anywhere in this process".
package payments

import (
	"context"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/providercred"
)

// syntheticPaymentAdapter is providerkind.Synthetic restated structurally,
// mirroring casino.syntheticCasinoAdapter/kyc's identical role.
type syntheticPaymentAdapter interface{ SyntheticComponent() }

// OutboundKindSplitResolver splits outbound credential resolution by
// adapter kind, exactly like casino.OutboundKindSplitResolver.
type OutboundKindSplitResolver struct {
	synthetic map[string]bool
	mock      OutboundCredentialResolver
	real      OutboundCredentialResolver
}

// NewOutboundKindSplitResolver builds the split over adapters (the same
// registry NewOrchestrator was built with). mock serves every provider id
// whose adapter is synthetic (nil means none is wired: those calls fail
// closed with ErrOutboundCredentialUnavailable, the gate's own T5/not-sent
// convention); real serves every other registered adapter (nil likewise).
// Returns a TRUE nil interface when both are nil, so a caller's own
// nil-resolver check still applies exactly as it does today.
func NewOutboundKindSplitResolver(adapters map[string]PaymentProvider, mock, real OutboundCredentialResolver) OutboundCredentialResolver {
	if mock == nil && real == nil {
		return nil
	}
	s := &OutboundKindSplitResolver{synthetic: map[string]bool{}, mock: mock, real: real}
	for id, a := range adapters {
		_, isSynthetic := any(a).(syntheticPaymentAdapter)
		s.synthetic[id] = isSynthetic
	}
	return s
}

// Resolve implements OutboundCredentialResolver. A provider id never
// registered in the adapters map this resolver was built from fails
// closed (ErrOutboundCredentialUnavailable) - this can only be reached if
// the caller's own registry check and this resolver were built from
// different adapter maps, which never happens in this codebase's wiring,
// but the fail-closed default is kept rather than assumed.
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
