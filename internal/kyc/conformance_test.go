package kyc

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// mockCredentialFor resolves the mock's own tenant-bound webhook
// credential for tenantID - the conformance suite's stand-in for "the
// credential the Orchestrator would have already resolved and equality-
// checked before calling HandleCallback" (Stage 10.2, KYC-WH-1, ADR 0091),
// since this suite deliberately exercises HandleCallback directly, without
// an Orchestrator/DB in the loop. Mirrors internal/casino/internal/payments'
// identical helper exactly.
func mockCredentialFor(t *testing.T, mock *MockKYCProvider, tenantID uuid.UUID) webhookauth.Credential {
	t.Helper()
	cred, err := NewMockWebhookCredentials(mock).Resolve(context.Background(), tenantID, mock.ID(), webhookauth.MockKeyID)
	if err != nil {
		t.Fatalf("resolve mock webhook credential: %v", err)
	}
	return cred
}

// RunProviderConformanceSuite is the parameterized test suite ADR 0028
// requires, mirroring ADR 0022 §6/ADR 0025 §8's identical binding rule for
// payments/casino: "one parameterized test suite, run identically against
// MockKYCProvider now and any future real adapter later."
//
// Stage 10.2 final review (K3, M3/F-9): this suite previously did not
// exist at all - ADR 0022 §3's amendment claimed a KYC tenant-binding
// conformance case that was never committed. This closes that gap with the
// SAME mandatory, skip-to-fail tenant-binding case internal/casino and
// internal/payments already have (including the MAC-level sub-case that
// closes the "rejected by the early TenantID metadata check, not the
// actual HMAC comparison" gap).
func RunProviderConformanceSuite(t *testing.T, factory func() KYCProvider) {
	t.Helper()

	// KYC-WH-1 / design §C12 (casino's own naming) / ADR 0022 §3 amendment:
	// the tenant-binding conformance case is mandatory for the first real
	// adapter, not mock-only.
	//
	// Stage 10.2 final review (K3): mandatory means this case must FAIL,
	// not skip, for any non-mock adapter. A provider that is not
	// *MockKYCProvider must fail here until it supplies its own per-tenant
	// signed-fixture hook proving tenant binding the same way the mock's
	// does.
	t.Run("a credential resolved for one tenant is rejected for another (conformance)", func(t *testing.T) {
		provider := factory()
		mock, ok := provider.(*MockKYCProvider)
		if !ok {
			t.Fatalf("tenant-binding conformance is mandatory (ADR 0022 §3 amendment): %T must supply its own per-tenant signed-fixture hook for this case; a real adapter cannot skip it", provider)
		}
		ctx := context.Background()
		tenantA, tenantB := uuid.New(), uuid.New()
		credA := mockCredentialFor(t, mock, tenantA)

		// A payload signed (via credA) FOR tenantA, delivered as if it were
		// tenantB's Inbound (TenantID overwritten to tenantB, mirroring
		// what an Orchestrator would do when it verifies against the ROUTE
		// tenant, never a payload-asserted one).
		inbound := mock.CallbackPayload(tenantA, "conformance-cross-tenant-1", ProviderApproved, "x")
		inbound.TenantID = tenantB
		if _, err := provider.HandleCallback(ctx, inbound, credA); !errors.Is(err, ErrCallbackSignatureInvalid) {
			t.Fatalf("expected ErrCallbackSignatureInvalid for a tenant-A-signed callback delivered as tenant B, got %v", err)
		}

		// MAC-level sub-case: the sub-case immediately above is rejected by
		// webhookauth.Scheme.Verify's OWN early cred.TenantID != in.TenantID
		// check (webhookauth.go's Verify, before it ever recomputes the
		// HMAC) - it never actually exercises the MAC comparison itself.
		// This sub-case closes that gap: a credential deliberately RE-BOUND
		// to claim tenantB (so the early check passes) while still
		// carrying tenantA's own derived secret - exactly the shape a
		// broken resolver that mislabels a credential's TenantID field but
		// forgets to re-derive Secret would produce. Verify must then fail
		// at the ACTUAL HMAC comparison, not merely on the metadata check.
		credAKeyMaterialClaimingB := credA
		credAKeyMaterialClaimingB.TenantID = tenantB
		inboundForB := mock.CallbackPayload(tenantA, "conformance-cross-tenant-2", ProviderApproved, "x")
		inboundForB.TenantID = tenantB
		if _, err := provider.HandleCallback(ctx, inboundForB, credAKeyMaterialClaimingB); !errors.Is(err, ErrCallbackSignatureInvalid) {
			t.Fatalf("expected ErrCallbackSignatureInvalid at the MAC comparison itself (not merely the TenantID metadata check) for a credential carrying tenant A's key material mislabeled as tenant B, got %v", err)
		}
	})
}

func TestMockKYCProvider_ConformsToKYCProvider(t *testing.T) {
	RunProviderConformanceSuite(t, func() KYCProvider {
		return NewMockKYCProvider()
	})
}
