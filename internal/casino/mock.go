package casino

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/uuid"
)

// MockCasinoProvider is a CasinoProvider implementation with synthetic,
// test-controllable behavior - Stage 4A's only adapter, mirroring
// internal/payments.MockProvider's own framing exactly: it implements the
// SAME canonical CasinoProvider contract as a real aggregator would, going
// through Orchestrator.LaunchGame/ReceiveCallback with no
// MockCasinoProvider-specific branch anywhere in orchestrator.go (ADR 0025
// §7/§8's "must implement the same canonical contract, not a shortcut").
//
// MOCK: labeled per CLAUDE.md's "No fake completion" rule - a synthetic
// double for development/testing, never a real casino aggregator.
//
// Bet outcome is selected by Amount, exactly like MockProvider.Deposit:
//
//	amount == MockBetAmountDecline    -> synchronous OutcomeDeclined (simulates a provider-side stake rejection, e.g. above a provider limit)
//	amount == MockBetAmountAmbiguous  -> synchronous OutcomeAmbiguous (simulates a timed-out/unknown-outcome request - directive item L)
//	any other amount                  -> OutcomeSucceeded
//
// Launch outcome is selected by ProviderGameID:
//
//	providerGameID == MockGameIDDeclineLaunch -> OutcomeDeclined
//	any other value                           -> OutcomeSucceeded
type MockCasinoProvider struct {
	providerID string
	capability AdapterCapability

	mu       sync.Mutex
	health   ProviderHealth
	seq      int
	games    []CatalogueEntry
	balances map[string]BalanceResult // keyed by playerAccountID+assetCode
	failNext bool

	// signingSecret is this adapter instance's HMAC key, generated at
	// construction - see MockProvider.signingSecret's identical rationale
	// in internal/payments/mock.go: HandleCallback must verify a signature
	// before acting on anything else (ADR 0025 §5's "provider
	// authentication must be adapter-specific"), and a callback endpoint
	// with no other auth mechanism is trivially forgeable without this.
	signingSecret []byte
}

// Magic values driving MockCasinoProvider's synthetic behavior - see the
// type doc comment above.
const (
	MockBetAmountDecline   int64 = 111
	MockBetAmountAmbiguous int64 = 333

	MockGameIDDeclineLaunch = "mock-decline-launch"
)

// NewMockCasinoProvider constructs a mock adapter registered under
// providerID. assetCodes controls Capabilities()'s declared support -
// callers pass the assets this mock instance should accept in a given
// test.
func NewMockCasinoProvider(providerID string, assetCodes ...string) *MockCasinoProvider {
	if len(assetCodes) == 0 {
		assetCodes = []string{"EUR", "USD"}
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		// crypto/rand failing is a fatal platform problem, not a
		// recoverable one - never fall back to a predictable secret.
		panic(fmt.Sprintf("casino/mock: failed to generate signing secret: %v", err))
	}
	return &MockCasinoProvider{
		signingSecret: secret,
		providerID:    providerID,
		capability: AdapterCapability{
			ProviderID:           providerID,
			SupportsCatalogue:    true,
			SupportsLaunch:       true,
			SupportsBalance:      true,
			SupportsBet:          true,
			SupportsWin:          true,
			SupportsRollback:     true,
			SupportedAssets:      assetCodes,
			SupportedGameTypes:   []string{"slot", "table", "live"},
			CallbackCapabilities: CallbackWebhookOnly,
		},
		health: ProviderHealth{
			ProviderID:         providerID,
			RollingSuccessRate: 1.0,
			CircuitState:       CircuitClosed,
		},
		games: []CatalogueEntry{
			{
				ProviderGameID: "mock-slot-001", Name: "Mock Fortune Slot", GameType: "slot",
				RTPVariant: "96.5", Volatility: "medium", SupportedAssets: assetCodes,
				MobileSupported: true, DemoSupported: true,
			},
			{
				ProviderGameID: "mock-table-001", Name: "Mock Blackjack", GameType: "table",
				RTPVariant: "99.5", Volatility: "low", SupportedAssets: assetCodes,
				MobileSupported: true, DemoSupported: false,
			},
		},
		balances: make(map[string]BalanceResult),
	}
}

// SetHealth lets a test drive routing/circuit-breaker behavior
// deterministically (directive item Q - "disabled provider").
func (m *MockCasinoProvider) SetHealth(h ProviderHealth) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h.ProviderID = m.providerID
	m.health = h
}

// FailNextCall makes the next Launch/Bet/Win/Rollback call return a
// transport-level error (directive item K - "provider failure"), rather
// than a well-formed OutcomeDeclined/OutcomeAmbiguous result. Resets
// itself after firing once.
func (m *MockCasinoProvider) FailNextCall() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failNext = true
}

func (m *MockCasinoProvider) consumeFailure() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext {
		m.failNext = false
		return true
	}
	return false
}

// SetBalance lets a test seed the value MockCasinoProvider.Balance returns
// for a given (playerAccountID, assetCode) pair - the mock has no access
// to the platform's own wallet/ledger tables, so this is a purely synthetic
// stand-in for a provider's own game-round balance (never authoritative -
// see ADR 0025 §8: "never trust provider-supplied balance as financial
// truth").
func (m *MockCasinoProvider) SetBalance(playerAccountID uuid.UUID, assetCode string, amount int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.balances[playerAccountID.String()+":"+assetCode] = BalanceResult{Amount: amount, AssetCode: assetCode}
}

// Catalogue implements CasinoProvider.
func (m *MockCasinoProvider) Catalogue(_ context.Context) ([]CatalogueEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]CatalogueEntry, len(m.games))
	copy(out, m.games)
	return out, nil
}

// Launch implements CasinoProvider. Never receives or requires the
// player's platform JWT - only the already-minted, single-use LaunchToken
// (ADR 0025 §3, §6).
func (m *MockCasinoProvider) Launch(_ context.Context, req LaunchRequest) (LaunchResult, error) {
	if req.LaunchToken == "" {
		return LaunchResult{}, fmt.Errorf("%w: launch token is required", ErrInvalidInput)
	}
	if m.consumeFailure() {
		return LaunchResult{}, fmt.Errorf("casino/mock: simulated provider transport failure")
	}
	if req.ProviderGameID == MockGameIDDeclineLaunch {
		return LaunchResult{Outcome: OutcomeDeclined, DeclineReason: "mock_provider_declined"}, nil
	}
	return LaunchResult{
		Outcome:   OutcomeSucceeded,
		LaunchURL: fmt.Sprintf("https://mock-casino.invalid/launch/%s?token=%s", req.ProviderGameID, req.LaunchToken),
	}, nil
}

// Balance implements CasinoProvider - read-only, posts nothing, and (per
// ADR 0025 §8) never consulted as financial truth by the orchestrator.
func (m *MockCasinoProvider) Balance(_ context.Context, req BalanceRequest) (BalanceResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.balances[req.PlayerAccountID.String()+":"+req.AssetCode]; ok {
		return b, nil
	}
	return BalanceResult{Amount: 0, AssetCode: req.AssetCode}, nil
}

// Bet implements CasinoProvider's synchronous, adapter-own-contract shape
// (directive item D) - independent of, and never called by, the
// Orchestrator itself (see CasinoProvider.HandleCallback's doc comment:
// the orchestrator only ever consumes HandleCallback's output). Exists so
// a provider conformance suite can exercise Bet/Win/Rollback's own request/
// response contract directly.
func (m *MockCasinoProvider) Bet(_ context.Context, req BetRequest) (BetResult, error) {
	if req.ProviderTxID == "" || req.Amount <= 0 || req.AssetCode == "" {
		return BetResult{}, fmt.Errorf("%w: provider_tx_id, amount, and asset_code are required", ErrInvalidInput)
	}
	if m.consumeFailure() {
		return BetResult{}, fmt.Errorf("casino/mock: simulated provider transport failure")
	}
	switch req.Amount {
	case MockBetAmountDecline:
		return BetResult{Outcome: OutcomeDeclined, DeclineReason: "mock_stake_rejected"}, nil
	case MockBetAmountAmbiguous:
		return BetResult{Outcome: OutcomeAmbiguous}, nil
	default:
		return BetResult{Outcome: OutcomeSucceeded}, nil
	}
}

// Win implements CasinoProvider's synchronous adapter-own-contract shape
// (directive item E).
func (m *MockCasinoProvider) Win(_ context.Context, req WinRequest) (WinResult, error) {
	if req.ProviderTxID == "" || req.Amount <= 0 || req.AssetCode == "" {
		return WinResult{}, fmt.Errorf("%w: provider_tx_id, amount, and asset_code are required", ErrInvalidInput)
	}
	if m.consumeFailure() {
		return WinResult{}, fmt.Errorf("casino/mock: simulated provider transport failure")
	}
	return WinResult{Outcome: OutcomeSucceeded}, nil
}

// Rollback implements CasinoProvider's synchronous adapter-own-contract
// shape (directive item F).
func (m *MockCasinoProvider) Rollback(_ context.Context, req RollbackRequest) (RollbackResult, error) {
	if req.ProviderTxID == "" || req.OriginalProviderTxID == "" {
		return RollbackResult{}, fmt.Errorf("%w: provider_tx_id and original_provider_tx_id are required", ErrInvalidInput)
	}
	if m.consumeFailure() {
		return RollbackResult{}, fmt.Errorf("casino/mock: simulated provider transport failure")
	}
	return RollbackResult{Outcome: OutcomeSucceeded}, nil
}

func (m *MockCasinoProvider) nextReference() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	return fmt.Sprintf("%s-%d", m.providerID, m.seq)
}

// NextProviderTxID is a test helper for minting a fresh, deterministic-
// per-instance provider transaction reference, mirroring MockProvider.
// nextReference's role in payments tests.
func (m *MockCasinoProvider) NextProviderTxID() string {
	return m.nextReference()
}

// mockCasinoCallbackBody is the synthetic wire shape MockCasinoProvider's
// webhook parser accepts - the mock's stand-in for "whatever JSON shape a
// real casino aggregator's own wallet-callback actually uses," which a real
// adapter would parse into the same canonical CallbackEvent (ADR 0025 §1).
type mockCasinoCallbackBody struct {
	EventType            string `json:"event_type"`
	ProviderTxID         string `json:"provider_tx_id"`
	OriginalProviderTxID string `json:"original_provider_tx_id,omitempty"`
	RoundID              string `json:"round_id,omitempty"`
	ProviderGameID       string `json:"provider_game_id,omitempty"`
	Amount               int64  `json:"amount"`
	AssetCode            string `json:"asset_code"`
	Outcome              string `json:"outcome"`
	DeclineReason        string `json:"decline_reason,omitempty"`
	PlayerAccountID      string `json:"player_account_id"`
	// Signature is this instance's HMAC-SHA256 (hex-encoded) over the
	// other fields via MockCasinoProvider.sign - HandleCallback verifies it
	// before acting on anything else. A real adapter's equivalent field (or
	// header) would carry the vendor's own signature scheme instead
	// (ADR 0025 §5: "provider authentication must be adapter-specific").
	Signature string `json:"signature,omitempty"`
}

// sign computes this instance's HMAC-SHA256 over the callback's own
// identifying/effect-bearing fields (never including Signature itself,
// which would make verification vacuous) - mirrors MockProvider.sign
// exactly, separator-joined so no combination of field values can be
// reinterpreted as a different set of fields.
func (m *MockCasinoProvider) sign(body mockCasinoCallbackBody) string {
	mac := hmac.New(sha256.New, m.signingSecret)
	fmt.Fprintf(mac, "%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%s\x00%s\x00%s\x00%s",
		body.EventType, body.ProviderTxID, body.OriginalProviderTxID, body.RoundID, body.ProviderGameID,
		body.Amount, body.AssetCode, body.Outcome, body.DeclineReason, body.PlayerAccountID)
	return hex.EncodeToString(mac.Sum(nil))
}

// CallbackPayload builds a synthetic webhook body, as JSON, in the shape
// MockCasinoProvider.HandleCallback parses - a test helper standing in for
// "the provider's real callback delivery." Signs the body with this
// instance's own signingSecret so it passes HandleCallback's verification
// exactly as a real, correctly-authenticated provider delivery would; a
// test wanting to exercise directive item J (provider authentication
// failure) should instead hand-construct a payload with a wrong/missing
// signature.
func (m *MockCasinoProvider) CallbackPayload(eventType CallbackEventType, providerTxID, originalProviderTxID, roundID, providerGameID string, amount int64, assetCode string, outcome Outcome, declineReason string, playerAccountID uuid.UUID) []byte {
	body := mockCasinoCallbackBody{
		EventType: string(eventType), ProviderTxID: providerTxID, OriginalProviderTxID: originalProviderTxID,
		RoundID: roundID, ProviderGameID: providerGameID, Amount: amount, AssetCode: assetCode,
		Outcome: string(outcome), DeclineReason: declineReason, PlayerAccountID: playerAccountID.String(),
	}
	body.Signature = m.sign(body)
	marshalled, _ := json.Marshal(body)
	return marshalled
}

// HandleCallback implements CasinoProvider. It verifies the embedded HMAC
// signature (see sign) before acting on any other field - the property
// ADR 0025 §5/§12 require: a caller who does not know this instance's
// signingSecret cannot post a payload HandleCallback will accept, and a
// malformed payload (directive item: "malformed callbacks") is rejected by
// json.Unmarshal before signature verification is even attempted.
// hmac.Equal is constant-time; a plain == comparison here would leak
// timing information about how many leading bytes matched.
func (m *MockCasinoProvider) HandleCallback(_ context.Context, rawPayload []byte) (CallbackEvent, error) {
	var body mockCasinoCallbackBody
	if err := json.Unmarshal(rawPayload, &body); err != nil {
		return CallbackEvent{}, fmt.Errorf("casino/mock: parse callback: %w", err)
	}
	if body.ProviderTxID == "" {
		return CallbackEvent{}, fmt.Errorf("casino/mock: callback missing provider_tx_id")
	}

	expected := m.sign(body)
	if body.Signature == "" || !hmac.Equal([]byte(expected), []byte(body.Signature)) {
		return CallbackEvent{}, ErrCallbackSignatureInvalid
	}

	var eventType CallbackEventType
	switch body.EventType {
	case string(CallbackEventBet):
		eventType = CallbackEventBet
	case string(CallbackEventWin):
		eventType = CallbackEventWin
	case string(CallbackEventRollback):
		eventType = CallbackEventRollback
	default:
		return CallbackEvent{}, fmt.Errorf("casino/mock: unknown callback event_type %q", body.EventType)
	}

	var outcome Outcome
	switch body.Outcome {
	case string(OutcomeSucceeded):
		outcome = OutcomeSucceeded
	case string(OutcomeDeclined):
		outcome = OutcomeDeclined
	case string(OutcomeAmbiguous):
		outcome = OutcomeAmbiguous
	case string(OutcomePending):
		outcome = OutcomePending
	case "":
		// Rollback events carry no outcome of their own (they are always
		// an unconditional reversal) - only Bet/Win populate this field.
	default:
		return CallbackEvent{}, fmt.Errorf("casino/mock: unknown callback outcome %q", body.Outcome)
	}

	var playerAccountID uuid.UUID
	if body.PlayerAccountID != "" {
		parsed, err := uuid.Parse(body.PlayerAccountID)
		if err != nil {
			return CallbackEvent{}, fmt.Errorf("casino/mock: invalid player_account_id: %w", err)
		}
		playerAccountID = parsed
	}

	return CallbackEvent{
		EventType: eventType, ProviderTxID: body.ProviderTxID, OriginalProviderTxID: body.OriginalProviderTxID,
		RoundID: body.RoundID, ProviderGameID: body.ProviderGameID, Amount: body.Amount, AssetCode: body.AssetCode,
		Outcome: outcome, DeclineReason: body.DeclineReason, PlayerAccountID: playerAccountID,
	}, nil
}

// Capabilities implements CasinoProvider.
func (m *MockCasinoProvider) Capabilities() AdapterCapability {
	return m.capability
}

// HealthStatus implements CasinoProvider.
func (m *MockCasinoProvider) HealthStatus(_ context.Context) (ProviderHealth, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.health, nil
}
