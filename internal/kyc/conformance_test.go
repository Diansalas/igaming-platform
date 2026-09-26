package kyc

import (
	"context"
	"encoding/json"
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
		if err := checkReasonBoundCase(provider, mockReasonCallback(t, mock)); err != nil {
			t.Fatal(err)
		}
	})
}

// reasonBoundDirtyFixture is the oversized, control/bidi-laden reason the
// reason-bound case sends (the same shape as the pre-fix E6 evidence).
var reasonBoundDirtyFixture = "\r\x1b[31mFAKE ADMIN MESSAGE\x1b[0m\u202Eevil-reversed-text" + strings.Repeat("A", 4096)

// mockReasonCallback returns the MOCK's signed-callback hook for the
// reason-bound case: a callback carrying rawReason, signed for a fresh
// tenant, plus the credential the Orchestrator would have resolved.
func mockReasonCallback(t *testing.T, mock *MockKYCProvider) func(rawReason string) (webhookauth.Inbound, webhookauth.Credential) {
	return func(rawReason string) (webhookauth.Inbound, webhookauth.Credential) {
		tenantID := uuid.New()
		return mock.CallbackPayload(tenantID, "conformance-reason-bound-1", ProviderRejected, rawReason), mockCredentialFor(t, mock, tenantID)
	}
}

// checkReasonBoundCase is the reason-bound conformance case's body, shared
// by the suite and its self-test (gate 10.3-W1 code review #12): it drives
// provider.HandleCallback with reasonBoundDirtyFixture through signed
// (the adapter's signed-callback hook) and returns a non-nil error if the
// returned reason violates KYC-REASON-BOUND-1.
func checkReasonBoundCase(provider KYCProvider, signed func(rawReason string) (webhookauth.Inbound, webhookauth.Credential)) error {
	inbound, cred := signed(reasonBoundDirtyFixture)
	result, err := provider.HandleCallback(context.Background(), inbound, cred)
	if err != nil {
		return fmt.Errorf("unexpected HandleCallback error: %w", err)
	}
	if err := reasonConformanceViolation(result.Reason); err != nil {
		return fmt.Errorf("HandleCallback returned a non-conforming reason: %w (reason=%q)", err, result.Reason)
	}
	return nil
}

// reasonConformanceViolation reports whether reason satisfies
// KYC-REASON-BOUND-1's bound (mirroring NormalizeReason's own rules,
// reason_normalize.go): at most MaxReasonBytes bytes, valid UTF-8, and
// free of C0/C1 controls and the Unicode bidi/format controls
// NormalizeReason strips. Returns nil for a conforming reason, or a
// descriptive error naming the violation otherwise.
//
// Used by checkReasonBoundCase, which this file's own self-test
// (TestReasonBoundConformanceSelfTest_DetectsNonConformingReason) drives
// with deliberately broken fixture ADAPTERS - proving the case actually
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

// brokenReasonAdapter is the reason-bound self-test's deliberately
// non-conforming fixture ADAPTER (gate 10.3-W1 code review #12: a broken
// adapter, not a raw string). It wraps the MOCK - so verification and
// parsing are the real ones - and then returns the callback's reason with
// only part (or none) of KYC-REASON-BOUND-1 applied.
type brokenReasonAdapter struct {
	*MockKYCProvider
	mode string // "raw", "length_only" (keeps controls), "controls_only" (no length bound)
}

func (a brokenReasonAdapter) HandleCallback(ctx context.Context, in webhookauth.Inbound, cred webhookauth.Credential) (ProviderResult, error) {
	result, err := a.MockKYCProvider.HandleCallback(ctx, in, cred)
	if err != nil {
		return result, err
	}
	var payload struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(in.Body, &payload); err != nil {
		return ProviderResult{}, err
	}
	switch a.mode {
	case "raw":
		result.Reason = payload.Reason
	case "length_only":
		result.Reason = payload.Reason[:MaxReasonBytes]
	case "controls_only":
		var b strings.Builder
		for _, r := range payload.Reason {
			if !isC0OrC1Control(r) && !isBidiOrFormatControl(r) {
				b.WriteRune(r)
			}
		}
		result.Reason = b.String()
	}
	return result, nil
}

// TestReasonBoundConformanceSelfTest_DetectsNonConformingReason is the
// self-test the QA binding plan requires for this wave's conformance case:
// the SAME case body the suite runs (checkReasonBoundCase) must go red for
// each deliberately broken adapter - unbounded raw text, length-bounded
// but control-laden, control-stripped but oversized - and green for the
// conforming MOCK, proving the case catches non-conformance through an
// adapter's HandleCallback rather than merely gating on its Go type.
func TestReasonBoundConformanceSelfTest_DetectsNonConformingReason(t *testing.T) {
	for _, mode := range []string{"raw", "length_only", "controls_only"} {
		t.Run(mode, func(t *testing.T) {
			broken := brokenReasonAdapter{MockKYCProvider: NewMockKYCProvider(), mode: mode}
			if err := checkReasonBoundCase(broken, mockReasonCallback(t, broken.MockKYCProvider)); err == nil {
				t.Fatalf("broken adapter (%s) must be detected as non-conforming, got nil", mode)
			}
		})
	}
	mock := NewMockKYCProvider()
	if err := checkReasonBoundCase(mock, mockReasonCallback(t, mock)); err != nil {
		t.Fatalf("the conforming MOCK must pass the reason-bound case, got %v", err)
	}
}

func TestMockKYCProvider_ConformsToKYCProvider(t *testing.T) {
	RunProviderConformanceSuite(t, func() KYCProvider {
		return NewMockKYCProvider()
	})
}
