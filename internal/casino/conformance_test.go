package casino

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
// checked before calling HandleCallback" (Stage 10.2, CAS-WH-TENANT-1,
// ADR 0091), since this suite deliberately exercises HandleCallback
// directly, without an Orchestrator/DB in the loop. Mirrors
// internal/payments' identical helper exactly.
func mockCredentialFor(t *testing.T, mock *MockCasinoProvider, tenantID uuid.UUID) webhookauth.Credential {
	t.Helper()
	cred, err := NewMockWebhookCredentials(mock).Resolve(context.Background(), tenantID, mock.Capabilities().ProviderID, mockWebhookKeyID)
	if err != nil {
		t.Fatalf("resolve mock webhook credential: %v", err)
	}
	return cred
}

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

	// J. Provider authentication (callback signature verification). Stage
	// 10.2 (CAS-WH-TENANT-1, ADR 0091): verification runs over the raw
	// body bytes via webhookauth.Scheme.Verify, BEFORE any parsing (design
	// §C2 point 1) - reachable only through an Orchestrator-resolved
	// Credential, which this suite stands in for via mockCredentialFor.
	t.Run("HandleCallback rejects a missing or invalid signature", func(t *testing.T) {
		provider := factory()
		mock, ok := provider.(*MockCasinoProvider)
		if !ok {
			t.Skip("callback payload construction is mock-specific")
		}
		ctx := context.Background()
		tenantID := uuid.New()
		cred := mockCredentialFor(t, mock, tenantID)

		valid := mock.CallbackPayload(tenantID, CallbackEventBet, "bet-auth-1", "", "round-1", "game-1", 1000, "EUR", OutcomeSucceeded, "", uuid.New(), uuid.New())
		if _, err := provider.HandleCallback(ctx, valid, cred); err != nil {
			t.Fatalf("expected a correctly-signed callback to be accepted, got %v", err)
		}

		// A tampered header signature is rejected even though the body
		// itself is untouched.
		tampered := valid
		tampered.Header = valid.Header.Clone()
		tampered.Header.Set(webhookauth.CasinoSignatureHeader, "v1="+"0000000000000000000000000000000000000000000000000000000000000000"[:64])
		if _, err := provider.HandleCallback(ctx, tampered, cred); !errors.Is(err, ErrCallbackSignatureInvalid) {
			t.Fatalf("expected ErrCallbackSignatureInvalid for a tampered signature, got %v", err)
		}

		// No signature headers at all.
		unsigned := webhookauth.Inbound{TenantID: tenantID, ProviderID: mock.Capabilities().ProviderID, Body: valid.Body}
		if _, err := provider.HandleCallback(ctx, unsigned, cred); !errors.Is(err, ErrCallbackSignatureInvalid) {
			t.Fatalf("expected ErrCallbackSignatureInvalid for an unsigned payload, got %v", err)
		}
	})

	// CAS-WH-TENANT-1 / design §C12: the tenant-binding conformance case is
	// mandatory for the first real adapter, not mock-only - mirrors
	// internal/payments' identical ruling (C4).
	t.Run("a credential resolved for one tenant is rejected for another (conformance)", func(t *testing.T) {
		provider := factory()
		mock, ok := provider.(*MockCasinoProvider)
		if !ok {
			t.Skip("credential/signature construction is mock-specific; a real adapter's own conformance fixture supplies its own per-tenant credentials")
		}
		ctx := context.Background()
		tenantA, tenantB := uuid.New(), uuid.New()
		credA := mockCredentialFor(t, mock, tenantA)

		// A payload signed (via credA) FOR tenantA, delivered as if it were
		// tenantB's Inbound (TenantID overwritten to tenantB, mirroring
		// what an Orchestrator would do when it verifies against the ROUTE
		// tenant, never a payload-asserted one).
		inbound := mock.CallbackPayload(tenantA, CallbackEventBet, "conformance-cross-tenant-1", "", "round-1", "game-1", 1000, "EUR", OutcomeSucceeded, "", uuid.New(), uuid.New())
		inbound.TenantID = tenantB
		if _, err := provider.HandleCallback(ctx, inbound, credA); !errors.Is(err, ErrCallbackSignatureInvalid) {
			t.Fatalf("expected ErrCallbackSignatureInvalid for a tenant-A-signed callback delivered as tenant B, got %v", err)
		}

		// Code-review finding (2026-09-26): the sub-case immediately above
		// is rejected by webhookauth.Scheme.Verify's OWN early
		// cred.TenantID != in.TenantID check (webhookauth.go's Verify,
		// before it ever recomputes the HMAC) - it never actually exercises
		// the MAC comparison itself. This sub-case closes that gap: a
		// credential deliberately RE-BOUND to claim tenantB (so the early
		// check passes) while still carrying tenantA's own derived secret -
		// exactly the shape a broken resolver that mislabels a credential's
		// TenantID field but forgets to re-derive Secret would produce.
		// Verify must then fail at the ACTUAL HMAC comparison, not merely
		// on the metadata check.
		credAKeyMaterialClaimingB := credA
		credAKeyMaterialClaimingB.TenantID = tenantB
		inboundForB := mock.CallbackPayload(tenantA, CallbackEventBet, "conformance-cross-tenant-2", "", "round-1", "game-1", 1000, "EUR", OutcomeSucceeded, "", uuid.New(), uuid.New())
		inboundForB.TenantID = tenantB
		if _, err := provider.HandleCallback(ctx, inboundForB, credAKeyMaterialClaimingB); !errors.Is(err, ErrCallbackSignatureInvalid) {
			t.Fatalf("expected ErrCallbackSignatureInvalid at the MAC comparison itself (not merely the TenantID metadata check) for a credential carrying tenant A's key material mislabeled as tenant B, got %v", err)
		}
	})

	// "Malformed callbacks" (directive item 14) - a VERIFIED callback (the
	// sender proved knowledge of the shared credential) whose body is
	// structurally malformed.
	t.Run("HandleCallback rejects a malformed payload", func(t *testing.T) {
		provider := factory()
		mock, ok := provider.(*MockCasinoProvider)
		if !ok {
			t.Skip("credential/signature construction is mock-specific")
		}
		ctx := context.Background()
		tenantID := uuid.New()
		cred := mockCredentialFor(t, mock, tenantID)

		if _, err := provider.HandleCallback(ctx, mock.SignRawBody(tenantID, []byte(`not json`)), cred); !errors.Is(err, ErrCallbackMalformedBody) {
			t.Fatalf("expected ErrCallbackMalformedBody for a non-JSON payload, got %v", err)
		}
		if _, err := provider.HandleCallback(ctx, mock.SignRawBody(tenantID, []byte(`{}`)), cred); !errors.Is(err, ErrCallbackMalformedBody) {
			t.Fatalf("expected ErrCallbackMalformedBody for a payload missing provider_tx_id, got %v", err)
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
		tenantID := uuid.New()
		cred := mockCredentialFor(t, mock, tenantID)

		payload := mock.CallbackPayload(tenantID, CallbackEventWin, "win-redeliver-1", "", "round-1", "game-1", 500, "EUR", OutcomeSucceeded, "", uuid.New(), uuid.Nil)
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
