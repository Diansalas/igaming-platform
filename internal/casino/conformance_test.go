package casino

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

// RunProviderConformanceSuite is the parameterized test suite ADR 0025 §8
// requires: "one parameterized test suite, run identically against
// MockCasinoProvider now and any future real adapter later" - mirroring
// docs/decisions/0022 §6's identical binding rule for payments. factory
// returns a fresh CasinoProvider instance per sub-test, so tests never
// share mutable adapter state with each other.
//
// This suite deliberately does NOT touch a database - it exercises only
// the CasinoProvider contract itself (directive items A/D/E/F/J/K/L plus
// the callback-parsing half of G/H), so it can run for any adapter (mock
// or a real sandbox-backed one) without an integration build tag. Items
// requiring wallet/ledger/RLS/transaction-state (B's full launch flow,
// C, G/H/I's ledger-level idempotency, M, N, O, P, Q, R, S, T) are covered
// by orchestrator_integration_test.go instead, per CLAUDE.md's "use real
// PostgreSQL integration tests wherever the operation touches wallet/
// ledger/idempotency/RLS/transaction state."
func RunProviderConformanceSuite(t *testing.T, factory func() CasinoProvider) {
	t.Helper()

	// A. Catalogue contract.
	t.Run("Catalogue returns well-formed entries", func(t *testing.T) {
		provider := factory()
		ctx := context.Background()

		entries, err := provider.Catalogue(ctx)
		if err != nil {
			t.Fatalf("Catalogue: %v", err)
		}
		if len(entries) == 0 {
			t.Fatal("expected at least one catalogue entry")
		}
		for _, e := range entries {
			if e.ProviderGameID == "" {
				t.Fatal("Catalogue: provider_game_id must not be empty")
			}
			if e.Name == "" {
				t.Fatalf("Catalogue: entry %s has no name", e.ProviderGameID)
			}
			if e.GameType == "" {
				t.Fatalf("Catalogue: entry %s has no game_type", e.ProviderGameID)
			}
		}
	})

	// B. Launch contract (adapter-level; full session-issuing flow is
	// integration-tested).
	t.Run("Launch succeeds for a normal request and declines the magic decline game id", func(t *testing.T) {
		provider := factory()
		ctx := context.Background()

		ok, err := provider.Launch(ctx, LaunchRequest{ProviderGameID: "any-game", PlayerAccountID: uuid.New(), AssetCode: "EUR", Mode: ModeReal, LaunchToken: "tok"})
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}
		if ok.Outcome != OutcomeSucceeded || ok.LaunchURL == "" {
			t.Fatalf("expected a successful launch with a non-empty URL, got %+v", ok)
		}

		declined, err := provider.Launch(ctx, LaunchRequest{ProviderGameID: MockGameIDDeclineLaunch, PlayerAccountID: uuid.New(), AssetCode: "EUR", Mode: ModeReal, LaunchToken: "tok"})
		if err != nil {
			t.Fatalf("Launch (decline): %v", err)
		}
		if declined.Outcome != OutcomeDeclined {
			t.Fatalf("expected OutcomeDeclined, got %v", declined.Outcome)
		}

		if _, err := provider.Launch(ctx, LaunchRequest{ProviderGameID: "any-game", PlayerAccountID: uuid.New(), AssetCode: "EUR", Mode: ModeReal}); err == nil {
			t.Fatal("expected Launch to reject a request with no launch token - a provider must never be handed the player's own JWT instead")
		}
	})

	// D/E/F. Bet/Win/Rollback adapter-own contract.
	t.Run("Bet/Win/Rollback contract", func(t *testing.T) {
		provider := factory()
		ctx := context.Background()

		bet, err := provider.Bet(ctx, BetRequest{ProviderTxID: "bet-1", RoundID: "round-1", Amount: 1000, AssetCode: "EUR"})
		if err != nil {
			t.Fatalf("Bet: %v", err)
		}
		if bet.Outcome != OutcomeSucceeded {
			t.Fatalf("expected OutcomeSucceeded, got %v", bet.Outcome)
		}

		win, err := provider.Win(ctx, WinRequest{ProviderTxID: "win-1", RoundID: "round-1", Amount: 2000, AssetCode: "EUR"})
		if err != nil {
			t.Fatalf("Win: %v", err)
		}
		if win.Outcome != OutcomeSucceeded {
			t.Fatalf("expected OutcomeSucceeded, got %v", win.Outcome)
		}

		rollback, err := provider.Rollback(ctx, RollbackRequest{ProviderTxID: "rollback-1", OriginalProviderTxID: "bet-1"})
		if err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		if rollback.Outcome != OutcomeSucceeded {
			t.Fatalf("expected OutcomeSucceeded, got %v", rollback.Outcome)
		}
	})

	// L. Timeout/ambiguous outcome (ADR 0025 §9/§8: driven by a magic
	// stake amount on the adapter-own Bet contract, mirroring payments'
	// identical "decline and ambiguous are distinguishable outcomes"
	// pattern for Deposit).
	t.Run("Bet decline and ambiguous are distinguishable outcomes", func(t *testing.T) {
		provider := factory()
		ctx := context.Background()

		decline, err := provider.Bet(ctx, BetRequest{ProviderTxID: "bet-decline", RoundID: "r", Amount: MockBetAmountDecline, AssetCode: "EUR"})
		if err != nil {
			t.Fatalf("Bet (decline): %v", err)
		}
		if decline.Outcome != OutcomeDeclined {
			t.Fatalf("expected OutcomeDeclined, got %v", decline.Outcome)
		}

		ambiguous, err := provider.Bet(ctx, BetRequest{ProviderTxID: "bet-ambiguous", RoundID: "r", Amount: MockBetAmountAmbiguous, AssetCode: "EUR"})
		if err != nil {
			t.Fatalf("Bet (ambiguous): %v", err)
		}
		if ambiguous.Outcome != OutcomeAmbiguous {
			t.Fatalf("expected OutcomeAmbiguous, got %v", ambiguous.Outcome)
		}
		if ambiguous.Outcome == decline.Outcome {
			t.Fatal("decline and ambiguous outcomes must never collapse into the same value")
		}
	})

	// K. Provider failure (transport-level, distinct from a well-formed
	// decline/ambiguous result).
	t.Run("a simulated transport failure surfaces as an error, not a result", func(t *testing.T) {
		provider := factory()
		mock, ok := provider.(*MockCasinoProvider)
		if !ok {
			t.Skip("FailNextCall is mock-specific; a real adapter's own test supplies its own failure fixture")
		}
		ctx := context.Background()

		mock.FailNextCall()
		if _, err := provider.Bet(ctx, BetRequest{ProviderTxID: "bet-fail", RoundID: "r", Amount: 1000, AssetCode: "EUR"}); err == nil {
			t.Fatal("expected an error for a simulated provider transport failure")
		}
		// FailNextCall fires exactly once.
		if _, err := provider.Bet(ctx, BetRequest{ProviderTxID: "bet-after-fail", RoundID: "r", Amount: 1000, AssetCode: "EUR"}); err != nil {
			t.Fatalf("expected the failure to be one-shot, got %v", err)
		}
	})

	// J. Provider authentication (callback signature verification).
	t.Run("HandleCallback rejects a missing or invalid signature", func(t *testing.T) {
		provider := factory()
		mock, ok := provider.(*MockCasinoProvider)
		if !ok {
			t.Skip("callback payload construction is mock-specific")
		}
		ctx := context.Background()

		valid := mock.CallbackPayload(CallbackEventBet, "bet-auth-1", "", "round-1", "game-1", 1000, "EUR", OutcomeSucceeded, "", uuid.New(), uuid.New())
		if _, err := provider.HandleCallback(ctx, valid); err != nil {
			t.Fatalf("expected a correctly-signed callback to be accepted, got %v", err)
		}

		// Tamper with the signature field specifically (not the JSON
		// structure at large), so this proves signature verification
		// itself rejects it, not merely that malformed JSON errors out.
		var fields map[string]any
		if err := json.Unmarshal(valid, &fields); err != nil {
			t.Fatalf("unmarshal fixture: %v", err)
		}
		fields["signature"] = "0000000000000000000000000000000000000000000000000000000000000000"
		tampered, err := json.Marshal(fields)
		if err != nil {
			t.Fatalf("remarshal fixture: %v", err)
		}
		if _, err := provider.HandleCallback(ctx, tampered); err != ErrCallbackSignatureInvalid {
			t.Fatalf("expected ErrCallbackSignatureInvalid for a tampered signature, got %v", err)
		}

		unsigned := []byte(`{"event_type":"bet","provider_tx_id":"bet-unsigned","amount":1000,"asset_code":"EUR","outcome":"succeeded","player_account_id":"` + uuid.New().String() + `"}`)
		if _, err := provider.HandleCallback(ctx, unsigned); err != ErrCallbackSignatureInvalid {
			t.Fatalf("expected ErrCallbackSignatureInvalid for an unsigned payload, got %v", err)
		}
	})

	// "Malformed callbacks" (directive item 14).
	t.Run("HandleCallback rejects a malformed payload", func(t *testing.T) {
		provider := factory()
		ctx := context.Background()

		if _, err := provider.HandleCallback(ctx, []byte(`not json`)); err == nil {
			t.Fatal("expected an error for a non-JSON payload")
		}
		if _, err := provider.HandleCallback(ctx, []byte(`{}`)); err == nil {
			t.Fatal("expected an error for a payload missing provider_tx_id")
		}
	})

	// G/H (parsing half). Ledger-level idempotency for a REDELIVERED
	// callback is proven in orchestrator_integration_test.go; this proves
	// the adapter's own HandleCallback parses a redelivered payload
	// identically both times, which idempotent posting depends on.
	t.Run("HandleCallback parses a redelivered payload identically", func(t *testing.T) {
		provider := factory()
		mock, ok := provider.(*MockCasinoProvider)
		if !ok {
			t.Skip("callback payload construction is mock-specific")
		}
		ctx := context.Background()

		payload := mock.CallbackPayload(CallbackEventWin, "win-redeliver-1", "", "round-1", "game-1", 500, "EUR", OutcomeSucceeded, "", uuid.New(), uuid.Nil)
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
		if len(cap.SupportedAssets) == 0 {
			t.Fatal("Capabilities: must declare at least one supported asset")
		}
		if len(cap.SupportedGameTypes) == 0 {
			t.Fatal("Capabilities: must declare at least one supported game type")
		}
		if cap.CallbackCapabilities != CallbackWebhookOnly && cap.CallbackCapabilities != CallbackPollingOnly && cap.CallbackCapabilities != CallbackBoth {
			t.Fatalf("Capabilities: invalid callback_capabilities %q", cap.CallbackCapabilities)
		}
	})

	t.Run("HealthStatus returns a well-formed value", func(t *testing.T) {
		provider := factory()
		health, err := provider.HealthStatus(context.Background())
		if err != nil {
			t.Fatalf("HealthStatus: %v", err)
		}
		if health.CircuitState != CircuitClosed && health.CircuitState != CircuitOpen && health.CircuitState != CircuitHalfOpen {
			t.Fatalf("HealthStatus: invalid circuit_state %q", health.CircuitState)
		}
	})
}

func TestMockCasinoProvider_ConformsToCasinoProvider(t *testing.T) {
	RunProviderConformanceSuite(t, func() CasinoProvider {
		return NewMockCasinoProvider("mock-casino", "EUR", "USD")
	})
}
