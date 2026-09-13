package payments

import (
	"context"
	"errors"
	"testing"
)

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
			t.Skip("callback payload construction is mock-specific; a real adapter's own test supplies its own wire fixtures")
		}
		ctx := context.Background()

		declinePayload := mock.CallbackPayload(CallbackEventDeposit, "ref-decline-1", "", OutcomeDeclined, 1000, "EUR", "issuer_declined", false)
		declineEvent, err := provider.HandleCallback(ctx, declinePayload)
		if err != nil {
			t.Fatalf("handle decline callback: %v", err)
		}
		if declineEvent.Outcome != OutcomeDeclined {
			t.Fatalf("expected OutcomeDeclined, got %v", declineEvent.Outcome)
		}

		ambiguousPayload := mock.CallbackPayload(CallbackEventDeposit, "ref-ambiguous-1", "", OutcomeAmbiguous, 1000, "EUR", "", false)
		ambiguousEvent, err := provider.HandleCallback(ctx, ambiguousPayload)
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
			t.Skip("callback payload construction is mock-specific")
		}
		ctx := context.Background()

		payload := mock.CallbackPayload(CallbackEventDeposit, "ref-redelivered-1", "", OutcomeSucceeded, 5000, "EUR", "", false)
		first, err := provider.HandleCallback(ctx, payload)
		if err != nil {
			t.Fatalf("first delivery: %v", err)
		}
		second, err := provider.HandleCallback(ctx, payload)
		if err != nil {
			t.Fatalf("redelivered: %v", err)
		}
		if first != second {
			t.Fatalf("redelivered callback parsed differently: %+v vs %+v", first, second)
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

		poisoned := []byte(`{"event_type":"deposit","provider_reference":"ref-poison-1","outcome":"succeeded","amount":1000,"asset_code":"EUR","private_key":"L1aW4thKtHz9GcpB4rMPvz3gK6yD7f9j5Kf2vSvB3wKz9c2CJ2f"}`)
		_, err := provider.HandleCallback(ctx, poisoned)
		if err == nil {
			t.Fatal("expected an error for a payload carrying apparent key material")
		}
		if !errors.Is(err, ErrInboundKeyMaterial) {
			t.Fatalf("expected ErrInboundKeyMaterial, got %v", err)
		}
	})
}
