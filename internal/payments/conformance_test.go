package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

// mockCredentialFor resolves the mock's own tenant-bound webhook
// credential for tenantID - the conformance suite's stand-in for "the
// credential the Orchestrator would have already resolved and equality-
// checked before calling HandleCallback" (docs/decisions/0022 §3
// amendment), since this suite deliberately exercises HandleCallback
// directly, without an Orchestrator/DB in the loop.
func mockCredentialFor(t *testing.T, mock *MockProvider, tenantID uuid.UUID) WebhookCredential {
	t.Helper()
	cred, err := NewMockWebhookCredentials(mock).Resolve(context.Background(), tenantID, mock.Capabilities().ProviderID, mockWebhookKeyID)
	if err != nil {
		t.Fatalf("resolve mock webhook credential: %v", err)
	}
	return cred
}

// RunProviderConformanceSuite is the parameterized test suite
// docs/decisions/0022 §6 requires: "a single test suite, parameterized
// over any PaymentProvider implementation, must pass identically for the
// mock adapter and for every real adapter added later." factory returns a
// fresh PaymentProvider instance per sub-test, so tests never share
// mutable adapter state with each other.
//
// This suite deliberately does NOT touch a database - it exercises only
// the PaymentProvider contract itself, so it can run for any adapter
// (mock or real-sandbox-backed) without an integration build tag.
func RunProviderConformanceSuite(t *testing.T, factory func() PaymentProvider) {
	t.Helper()

	t.Run("decline and ambiguous are distinguishable outcomes", func(t *testing.T) {
		provider := factory()
		ctx := context.Background()

		declineResult, err := provider.Deposit(ctx, DepositRequest{Amount: MockAmountPlayerDeclineNoCascade, AssetCode: "EUR", PaymentMethod: "card"})
		if err != nil {
			t.Fatalf("decline deposit: %v", err)
		}
		if declineResult.Outcome != OutcomeDeclined {
			t.Fatalf("expected OutcomeDeclined, got %v", declineResult.Outcome)
		}

		ambiguousResult, err := provider.Deposit(ctx, DepositRequest{Amount: MockAmountAmbiguous, AssetCode: "EUR", PaymentMethod: "card"})
		if err != nil {
			t.Fatalf("ambiguous deposit: %v", err)
		}
		if ambiguousResult.Outcome != OutcomeAmbiguous {
			t.Fatalf("expected OutcomeAmbiguous, got %v", ambiguousResult.Outcome)
		}
		if ambiguousResult.Outcome == declineResult.Outcome {
			t.Fatal("decline and ambiguous outcomes must never collapse into the same value")
		}

		// Both must also be independently visible via QueryStatus, not
		// just the synchronous Deposit return value.
		declineStatus, err := provider.QueryStatus(ctx, declineResult.ProviderReference)
		if err != nil {
			t.Fatalf("query decline status: %v", err)
		}
		if declineStatus.Outcome != OutcomeDeclined {
			t.Fatalf("QueryStatus: expected OutcomeDeclined, got %v", declineStatus.Outcome)
		}

		ambiguousStatus, err := provider.QueryStatus(ctx, ambiguousResult.ProviderReference)
		if err != nil {
			t.Fatalf("query ambiguous status: %v", err)
		}
		if ambiguousStatus.Outcome != OutcomeAmbiguous {
			t.Fatalf("QueryStatus: expected OutcomeAmbiguous, got %v", ambiguousStatus.Outcome)
		}
	})

	t.Run("HandleCallback surfaces decline and ambiguous distinguishably", func(t *testing.T) {
		provider := factory()
		mock, ok := provider.(*MockProvider)
		if !ok {
			// Stage 10.3 W1a (WH-VENDOR-SCHEME-1): fail, never skip - a real
			// adapter must supply its own signed-callback fixture hook.
			t.Fatalf("callback conformance is mandatory (ADR 0022 §6): %T must supply its own signed-callback fixture hook for this case; a real adapter cannot skip it", provider)
		}
		ctx := context.Background()
		tenantID := uuid.New()
		cred := mockCredentialFor(t, mock, tenantID)

		declinePayload := mock.CallbackPayload(tenantID, CallbackEventDeposit, "ref-decline-1", "", OutcomeDeclined, 1000, "EUR", "issuer_declined", false)
		declineEvent, err := provider.HandleCallback(ctx, declinePayload, cred)
		if err != nil {
			t.Fatalf("handle decline callback: %v", err)
		}
		if declineEvent.Outcome != OutcomeDeclined {
			t.Fatalf("expected OutcomeDeclined, got %v", declineEvent.Outcome)
		}

		ambiguousPayload := mock.CallbackPayload(tenantID, CallbackEventDeposit, "ref-ambiguous-1", "", OutcomeAmbiguous, 1000, "EUR", "", false)
		ambiguousEvent, err := provider.HandleCallback(ctx, ambiguousPayload, cred)
		if err != nil {
			t.Fatalf("handle ambiguous callback: %v", err)
		}
		if ambiguousEvent.Outcome != OutcomeAmbiguous {
			t.Fatalf("expected OutcomeAmbiguous, got %v", ambiguousEvent.Outcome)
		}
	})

	t.Run("redelivered callback is idempotent", func(t *testing.T) {
		provider := factory()
		mock, ok := provider.(*MockProvider)
		if !ok {
			// Stage 10.3 W1a (WH-VENDOR-SCHEME-1): fail, never skip.
			t.Fatalf("callback conformance is mandatory (ADR 0022 §6): %T must supply its own signed-callback fixture hook for this case; a real adapter cannot skip it", provider)
		}
		ctx := context.Background()
		tenantID := uuid.New()
		cred := mockCredentialFor(t, mock, tenantID)

		payload := mock.CallbackPayload(tenantID, CallbackEventDeposit, "ref-redelivered-1", "", OutcomeSucceeded, 5000, "EUR", "", false)
		first, err := provider.HandleCallback(ctx, payload, cred)
		if err != nil {
			t.Fatalf("first delivery: %v", err)
		}
		second, err := provider.HandleCallback(ctx, payload, cred)
		if err != nil {
			t.Fatalf("redelivered: %v", err)
		}
		if first != second {
			t.Fatalf("redelivered callback parsed differently: %+v vs %+v", first, second)
		}
	})

	// PAY-WH-TENANT-1 / ADR 0022 §3 amendment, ruling C4: the tenant-binding
	// tests are ADR 0022 §6 conformance tests, so any FUTURE real adapter
	// must pass them too - not mock-only tests. T5 (same secret, still
	// tenant-bound) is the one exception left mock-only, since a real
	// vendor cannot MAC our tenant id at all.
	//
	// Stage 10.2 final review (K3, ADR 0022 §3 amendment): this case is
	// mandatory for the first real adapter, not skip-eligible. A provider
	// that is not *MockProvider must fail here, not skip, until it supplies
	// its own per-tenant signed-fixture hook proving tenant binding the
	// same way the mock's does.
	t.Run("a credential resolved for one tenant is rejected for another (conformance)", func(t *testing.T) {
		provider := factory()
		mock, ok := provider.(*MockProvider)
		if !ok {
			t.Fatalf("tenant-binding conformance is mandatory (ADR 0022 §3 amendment): %T must supply its own per-tenant signed-fixture hook for this case; a real adapter cannot skip it", provider)
		}
		ctx := context.Background()
		tenantA, tenantB := uuid.New(), uuid.New()
		credA := mockCredentialFor(t, mock, tenantA)

		// A payload signed (via credA) FOR tenantA, delivered as if it were
		// tenantB's InboundCallback (TenantID overwritten to tenantB,
		// mirroring what an Orchestrator would do when it verifies against
		// the ROUTE tenant, never a payload-asserted one).
		inbound := mock.CallbackPayload(tenantA, CallbackEventDeposit, "conformance-cross-tenant-1", "", OutcomeSucceeded, 1000, "EUR", "", false)
		inbound.TenantID = tenantB
		if _, err := provider.HandleCallback(ctx, inbound, credA); !errors.Is(err, ErrCallbackSignatureInvalid) {
			t.Fatalf("expected ErrCallbackSignatureInvalid for a tenant-A-signed callback delivered as tenant B, got %v", err)
		}
	})

	t.Run("Capabilities returns a well-formed value", func(t *testing.T) {
		provider := factory()
		cap := provider.Capabilities()

		if cap.ProviderID == "" {
			t.Fatal("Capabilities: provider_id must not be empty")
		}
		if cap.ProviderKind != ProviderKindFiat && cap.ProviderKind != ProviderKindCryptoPayment {
			t.Fatalf("Capabilities: invalid provider_kind %q", cap.ProviderKind)
		}
		if len(cap.SupportedFiatCurrencies) == 0 && len(cap.SupportedCryptoAssets) == 0 {
			t.Fatal("Capabilities: must declare at least one fiat currency or crypto asset")
		}
		if len(cap.SupportedPaymentMethods) == 0 {
			t.Fatal("Capabilities: must declare at least one supported payment method")
		}
		if cap.CallbackCapabilities != CallbackWebhookOnly && cap.CallbackCapabilities != CallbackPollingOnly && cap.CallbackCapabilities != CallbackBoth {
			t.Fatalf("Capabilities: invalid callback_capabilities %q", cap.CallbackCapabilities)
		}
		// Every declared asset must carry a min/max amount limit pair
		// (docs/decisions/0022 §2: "a provider declaring several assets
		// carries one limit pair per asset").
		declaredAssets := append(append([]string{}, cap.SupportedFiatCurrencies...), cap.SupportedCryptoAssets...)
		for _, asset := range declaredAssets {
			found := false
			for _, lim := range cap.AmountLimits {
				if lim.AssetCode == asset {
					found = true
					if lim.MinAmount < 0 || lim.MaxAmount <= 0 || lim.MaxAmount < lim.MinAmount {
						t.Fatalf("Capabilities: invalid amount limit for %s: %+v", asset, lim)
					}
				}
			}
			if !found {
				t.Fatalf("Capabilities: declared asset %s has no amount_limits entry", asset)
			}
		}
	})

	t.Run("inbound key material is rejected, not stored or logged", func(t *testing.T) {
		provider := factory()
		ctx := context.Background()
		tenantID := uuid.New()

		poisoned := []byte(`{"event_type":"deposit","provider_reference":"ref-poison-1","outcome":"succeeded","amount":1000,"asset_code":"EUR","private_key":"L1aW4thKtHz9GcpB4rMPvz3gK6yD7f9j5Kf2vSvB3wKz9c2CJ2f"}`)
		// Stage 10.1 security review P2-1/code review F1/architect PW-1:
		// signature verification now runs BEFORE any body parsing
		// (§3 amendment point 7), so the key-material scan is only ever
		// reached for a body that genuinely, correctly verifies. Signing
		// it is mock-specific (a real adapter's own conformance fixture
		// supplies its own per-tenant credential/signature), mirroring
		// this suite's existing pattern for the other mock-specific cases
		// above.
		mock, ok := provider.(*MockProvider)
		if !ok {
			// Stage 10.3 W1a (WH-VENDOR-SCHEME-1): fail, never skip.
			t.Fatalf("key-material conformance is mandatory (ADR 0022 §4.1/§6): %T must supply its own per-tenant signed-callback fixture hook for this case; a real adapter cannot skip it", provider)
		}
		cred := mockCredentialFor(t, mock, tenantID)
		inbound := mock.SignRawBody(tenantID, poisoned)
		_, err := provider.HandleCallback(ctx, inbound, cred)
		if err == nil {
			t.Fatal("expected an error for a payload carrying apparent key material")
		}
		if !errors.Is(err, ErrInboundKeyMaterial) {
			t.Fatalf("expected ErrInboundKeyMaterial, got %v", err)
		}
	})
}
