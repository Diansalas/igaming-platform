package kyc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

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

	// KYC-REASON-BOUND-1 (Stage 10.3, security review C16): mandatory,
	// fail-not-skip, exactly like the tenant-binding case above - a real
	// adapter's own HandleCallback must return a bounded (<=512 byte),
	// control-character/bidi-control-free reason, never raw vendor text
	// verbatim. The mock proves this via its own CallbackPayload fixture
	// hook; any other provider type must supply its own equivalent proof.
	t.Run("HandleCallback returns a bounded, control-character-free reason (conformance)", func(t *testing.T) {
		provider := factory()
		mock, ok := provider.(*MockKYCProvider)
		if !ok {
			t.Fatalf("reason normalization is mandatory (KYC-REASON-BOUND-1): %T must supply its own fixture proving HandleCallback returns a bounded (<=%d byte), control-character-free reason; a real adapter cannot skip it", provider, MaxReasonBytes)
		}
		ctx := context.Background()
		tenantID := uuid.New()
		cred := mockCredentialFor(t, mock, tenantID)

		dirty := "\r\x1b[31mFAKE ADMIN MESSAGE\x1b[0m\u202Eevil-reversed-text" + strings.Repeat("A", 4096)
		inbound := mock.CallbackPayload(tenantID, "conformance-reason-bound-1", ProviderRejected, dirty)
		result, err := provider.HandleCallback(ctx, inbound, cred)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if err := reasonConformanceViolation(result.Reason); err != nil {
			t.Fatalf("HandleCallback returned a non-conforming reason: %v (reason=%q)", err, result.Reason)
		}
	})
}

// reasonConformanceViolation reports whether reason satisfies
// KYC-REASON-BOUND-1's bound (mirroring NormalizeReason's own rules,
// reason_normalize.go): at most MaxReasonBytes bytes, valid UTF-8, and
// free of C0/C1 controls and the Unicode bidi/format controls
// NormalizeReason strips. Returns nil for a conforming reason, or a
// descriptive error naming the violation otherwise.
//
// Factored out of the conformance case above so it can ALSO be exercised
// directly by this file's own self-test
// (TestReasonBoundConformanceSelfTest_DetectsNonConformingReason) against
// a deliberately non-conforming raw string - proving this check actually
// catches non-conformance, not merely that it gates on the adapter's Go
// type (ruling J1's self-test obligation, extended to this wave's own
// conformance addition per the QA binding test plan).
func reasonConformanceViolation(reason string) error {
	if !utf8.ValidString(reason) {
		return fmt.Errorf("reason is not valid UTF-8")
	}
	if len(reason) > MaxReasonBytes {
		return fmt.Errorf("reason is %d bytes, exceeds the %d byte bound", len(reason), MaxReasonBytes)
	}
	for _, r := range reason {
		if isC0OrC1Control(r) {
			return fmt.Errorf("reason contains a C0/C1 control character %U", r)
		}
		if isBidiOrFormatControl(r) {
			return fmt.Errorf("reason contains a bidi/format control character %U", r)
		}
	}
	return nil
}

// TestReasonBoundConformanceSelfTest_DetectsNonConformingReason is the
// self-test the QA binding plan requires for this wave's new conformance
// case: a deliberately non-conforming reason (oversized AND
// control-character-laden - the same shape as the pre-fix E6 evidence)
// must be detected as a violation, and an already-normalized reason must
// not be - proving reasonConformanceViolation's assertions actually catch
// something, not just always pass.
func TestReasonBoundConformanceSelfTest_DetectsNonConformingReason(t *testing.T) {
	dirty := "\r\x1b[31mFAKE ADMIN MESSAGE\x1b[0m\u202Eevil-reversed-text" + strings.Repeat("A", 4096)
	if err := reasonConformanceViolation(dirty); err == nil {
		t.Fatal("expected the deliberately non-conforming (oversized, control/bidi-laden) reason to be detected as a violation, got nil")
	}

	clean, truncated := NormalizeReason(dirty)
	if !truncated {
		t.Fatal("test precondition failed: expected NormalizeReason to have truncated the dirty fixture")
	}
	if err := reasonConformanceViolation(clean); err != nil {
		t.Fatalf("expected an already-normalized reason to pass, got violation: %v (reason=%q)", err, clean)
	}
}

func TestMockKYCProvider_ConformsToKYCProvider(t *testing.T) {
	RunProviderConformanceSuite(t, func() KYCProvider {
		return NewMockKYCProvider()
	})
}
