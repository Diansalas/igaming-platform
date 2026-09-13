package payments

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// TestMockProvider_ConformanceSuite proves the mock is not exempt from
// docs/decisions/0022 §6's conformance requirement - "the mock is not
// exempt from this suite and does not get its own bespoke test path."
func TestMockProvider_ConformanceSuite(t *testing.T) {
	RunProviderConformanceSuite(t, func() PaymentProvider {
		return NewMockProvider("mock-psp")
	})
}

func TestMockProvider_DepositMagicAmounts(t *testing.T) {
	ctx := context.Background()
	provider := NewMockProvider("mock-psp")

	cases := []struct {
		name    string
		amount  int64
		outcome Outcome
	}{
		{"player decline", MockAmountPlayerDeclineNoCascade, OutcomeDeclined},
		{"provider decline", MockAmountProviderDeclineCascade, OutcomeDeclined},
		{"ambiguous", MockAmountAmbiguous, OutcomeAmbiguous},
		{"pending", 5000, OutcomePending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := provider.Deposit(ctx, DepositRequest{Amount: tc.amount, AssetCode: "EUR", PaymentMethod: "card"})
			if err != nil {
				t.Fatalf("Deposit: %v", err)
			}
			if result.Outcome != tc.outcome {
				t.Fatalf("expected outcome %v, got %v", tc.outcome, result.Outcome)
			}
		})
	}
}

func TestMockProvider_CascadableFlagMatchesTaxonomy(t *testing.T) {
	ctx := context.Background()
	provider := NewMockProvider("mock-psp")

	playerDecline, _ := provider.Deposit(ctx, DepositRequest{Amount: MockAmountPlayerDeclineNoCascade, AssetCode: "EUR", PaymentMethod: "card"})
	if playerDecline.Cascadable {
		t.Fatal("a player-specific decline (insufficient funds) must not be cascadable")
	}

	providerDecline, _ := provider.Deposit(ctx, DepositRequest{Amount: MockAmountProviderDeclineCascade, AssetCode: "EUR", PaymentMethod: "card"})
	if !providerDecline.Cascadable {
		t.Fatal("a provider-specific decline (outage) must be cascadable")
	}
}

func TestMockProvider_ResolveAmbiguousViaQueryStatus(t *testing.T) {
	ctx := context.Background()
	provider := NewMockProvider("mock-psp")

	result, err := provider.Deposit(ctx, DepositRequest{Amount: MockAmountAmbiguous, AssetCode: "EUR", PaymentMethod: "card"})
	if err != nil {
		t.Fatalf("Deposit: %v", err)
	}

	status, err := provider.QueryStatus(ctx, result.ProviderReference)
	if err != nil {
		t.Fatalf("QueryStatus: %v", err)
	}
	if status.Outcome != OutcomeAmbiguous {
		t.Fatalf("expected still-ambiguous before Resolve, got %v", status.Outcome)
	}

	provider.Resolve(result.ProviderReference, OutcomeSucceeded, "", false)

	status, err = provider.QueryStatus(ctx, result.ProviderReference)
	if err != nil {
		t.Fatalf("QueryStatus after resolve: %v", err)
	}
	if status.Outcome != OutcomeSucceeded {
		t.Fatalf("expected OutcomeSucceeded after Resolve, got %v", status.Outcome)
	}
}

func TestMockProvider_HandleCallback_RejectsNestedKeyMaterial(t *testing.T) {
	provider := NewMockProvider("mock-psp")
	ctx := context.Background()

	nested := []byte(`{
		"event_type": "deposit",
		"provider_reference": "ref-1",
		"outcome": "succeeded",
		"amount": 1000,
		"asset_code": "EUR",
		"wallet_details": {"mnemonic": "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"}
	}`)
	_, err := provider.HandleCallback(ctx, nested)
	if !errors.Is(err, ErrInboundKeyMaterial) {
		t.Fatalf("expected ErrInboundKeyMaterial for nested key material, got %v", err)
	}
}

func TestMockProvider_HandleCallback_UnknownEventTypeRejected(t *testing.T) {
	provider := NewMockProvider("mock-psp")
	ctx := context.Background()

	_, err := provider.HandleCallback(ctx, []byte(`{"event_type":"withdrawal_sent","provider_reference":"ref-1","outcome":"succeeded"}`))
	if err == nil {
		t.Fatal("expected an error for an event_type this stage does not implement")
	}
}

// TestMockProvider_HandleCallback_RejectsForgedCallback closes the P0
// finding from the Stage 3B security review: the webhook route
// (POST /v1/webhooks/payments/{tenantSlug}/{providerID}) has no bearer-
// auth middleware by design, and provider_reference values are
// sequential and even handed to the player in InitiateDeposit's
// redirect_url - so without signature verification, ANYONE could forge a
// "succeeded" callback for any reference and mint an arbitrary ledger
// credit. This proves HandleCallback rejects a payload with no signature,
// a payload with a garbage signature, and a payload signed by a
// DIFFERENT MockProvider instance's own (different) secret - none of
// which should ever reach outcome/amount interpretation.
func TestMockProvider_HandleCallback_RejectsForgedCallback(t *testing.T) {
	provider := NewMockProvider("mock-psp")
	other := NewMockProvider("mock-psp") // distinct instance -> distinct secret
	ctx := context.Background()

	unsigned := []byte(`{"event_type":"deposit","provider_reference":"mock-psp-1","outcome":"succeeded","amount":100000000,"asset_code":"EUR"}`)
	if _, err := provider.HandleCallback(ctx, unsigned); !errors.Is(err, ErrCallbackSignatureInvalid) {
		t.Fatalf("expected ErrCallbackSignatureInvalid for an unsigned callback, got %v", err)
	}

	garbageSig := []byte(`{"event_type":"deposit","provider_reference":"mock-psp-1","outcome":"succeeded","amount":100000000,"asset_code":"EUR","signature":"deadbeef"}`)
	if _, err := provider.HandleCallback(ctx, garbageSig); !errors.Is(err, ErrCallbackSignatureInvalid) {
		t.Fatalf("expected ErrCallbackSignatureInvalid for a garbage signature, got %v", err)
	}

	// Signed correctly, but by a different adapter instance entirely - the
	// forger who controls "other" cannot produce a signature "provider"
	// will accept merely by running the same adapter code.
	wrongSecret := other.CallbackPayload(CallbackEventDeposit, "mock-psp-1", "", OutcomeSucceeded, 100000000, "EUR", "", false)
	if _, err := provider.HandleCallback(ctx, wrongSecret); !errors.Is(err, ErrCallbackSignatureInvalid) {
		t.Fatalf("expected ErrCallbackSignatureInvalid for a callback signed with a different instance's secret, got %v", err)
	}

	// Sanity check: the identical payload, correctly signed by "provider"
	// itself, must be accepted - proves the rejections above are actually
	// about the signature, not some other malformation.
	genuine := provider.CallbackPayload(CallbackEventDeposit, "mock-psp-1", "", OutcomeSucceeded, 100000000, "EUR", "", false)
	if _, err := provider.HandleCallback(ctx, genuine); err != nil {
		t.Fatalf("expected a genuinely-signed callback to be accepted, got %v", err)
	}
}

// TestMockProvider_HandleCallback_TamperedFieldAfterSigningRejected proves
// the signature actually covers the effect-bearing fields, not just an
// opaque token: a genuine signature computed over one outcome/amount must
// not verify once amount or outcome is changed afterward - the exact
// "attacker captures a legitimate low-value callback and edits the amount
// upward" scenario the signature exists to prevent.
func TestMockProvider_HandleCallback_TamperedFieldAfterSigningRejected(t *testing.T) {
	provider := NewMockProvider("mock-psp")
	ctx := context.Background()

	genuine := provider.CallbackPayload(CallbackEventDeposit, "mock-psp-1", "", OutcomeSucceeded, 500, "EUR", "", false)
	tampered := bytes.Replace(genuine, []byte(`"amount":500`), []byte(`"amount":500000000`), 1)
	if bytes.Equal(genuine, tampered) {
		t.Fatal("test setup bug: tampering did not change the payload")
	}
	if _, err := provider.HandleCallback(ctx, tampered); !errors.Is(err, ErrCallbackSignatureInvalid) {
		t.Fatalf("expected ErrCallbackSignatureInvalid for a tampered amount, got %v", err)
	}
}
