package casino

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// mockWebhookKeyID is the only KeyID the mock resolver/adapter ever uses.
// Stage 10.2 (CAS-WH-TENANT-1) ships exactly one generation; a future key
// rotation would add a second, coexisting id, never replace this one
// in-place.
const mockWebhookKeyID = webhookauth.MockKeyID

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

	// masterSecret is this adapter INSTANCE's own process-local, per-
	// process random secret (webhookauth.NewMockMaster - never config,
	// never in the repository, never recoverable outside the process). It
	// is NEVER itself the signing key for any callback: deriveKey below
	// derives a distinct per-(tenant, provider) key from it, which is what
	// binds the tenant into WHAT is verified (Stage 10.2, CAS-WH-TENANT-1,
	// ADR 0091, design §C2) - the single per-process key this replaced let
	// any captured callback verify for every tenant on the real
	// provider-facing route.
	masterSecret []byte
}

// deriveKey computes this instance's per-(tenantID, providerID) webhook
// signing key: HMAC-SHA256(masterSecret, casino mock label 0x00 tenant_id
// 0x00 provider_id) - delegated to webhookauth.DeriveMockKey.
func (m *MockCasinoProvider) deriveKey(tenantID uuid.UUID, providerID string) []byte {
	return webhookauth.DeriveMockKey(m.masterSecret, webhookauth.CasinoMockKeyLabel, tenantID, providerID)
}

// SyntheticComponent implements providerkind.Synthetic (Stage 10.3,
// MOCK-ADAPTER-PROD-1) - a structural marker only, satisfied without this
// package importing internal/providerkind.
func (m *MockCasinoProvider) SyntheticComponent() {}

// NewMockWebhookCredentials constructs the casino domain's MOCK inbound-
// webhook credential resolver bound to provider (design §C2). It resolves
// ONLY KeyID=="mock-v1" for provider's own bound provider id - any other
// providerID or keyID is webhookauth.ErrCredentialUnavailable, which the
// Orchestrator folds into ReasonCredentialUnavailable (fail closed, never
// a fallback to unauthenticated verification).
//
// MOCK: labeled per CLAUDE.md's "No fake completion" rule. The real
// resolver (a FORCE-RLS handle table plus an external, tenant-scoped
// secret store) is NOT IMPLEMENTED. cmd/platform-api/wiring.go wires this
// only when cfg.TestSupportRoutesEnabled(); otherwise the Orchestrator's
// resolver is nil and every callback fails closed.
func NewMockWebhookCredentials(provider *MockCasinoProvider) webhookauth.MockResolver {
	if provider == nil {
		return webhookauth.MockResolver{}
	}
	return webhookauth.MockResolver{
		Master:     provider.masterSecret,
		Label:      webhookauth.CasinoMockKeyLabel,
		ProviderID: provider.providerID,
	}
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
	return &MockCasinoProvider{
		masterSecret: webhookauth.NewMockMaster(),
		providerID:   providerID,
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
		LaunchURL: fmt.Sprintf("https://mock-casino.invalid/launch/%s?token=%s&session=%s", req.ProviderGameID, req.LaunchToken, req.SessionID),
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

// nextReference mints a provider reference unique across every replica of
// this process, not just within one - see internal/payments.MockProvider.
// nextReference's identical rationale. Today's orchestrator.go simulation
// path (LaunchGame/PlaceBet/etc.) doesn't call this - it derives its own
// provider-side identifiers - so this mock was safe by accident rather
// than by construction; NextProviderTxID below is a direct test helper, so
// a bare per-process counter here would collide the moment two replicas
// (or two parallel tests sharing an instance) called it. Fixed for
// symmetry with the payments mock rather than leaving a second instance of
// the same bug shape in the codebase.
func (m *MockCasinoProvider) nextReference() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	return fmt.Sprintf("%s-%d-%s", m.providerID, m.seq, uuid.NewString())
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
// Stage 10.2 (CAS-WH-TENANT-1): no Signature field - the signature moves
// into the X-Casino-Signature/X-Casino-Key-Id headers exclusively (design
// §C2), verified over the raw body bytes by webhookauth.Scheme.Verify
// before this shape is ever parsed.
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
	// SessionID echoes back the LaunchRequest.SessionID the platform
	// handed this provider at launch time - required on a bet callback so
	// ReceiveCallback can resolve player/wallet/asset/mode from the
	// platform's own casino_launch_sessions row (ADR 0025 §3/§6).
	SessionID string `json:"session_id,omitempty"`
}

// hasLegacySignatureField reports whether the top-level JSON object still
// carries a "signature" field - CAS-WH-TENANT-1 moves the signature into
// the X-Casino-Signature header exclusively (design §C2); a body that
// still embeds one (e.g. a caller replaying a pre-Stage-10.2-shaped
// payload) is rejected rather than silently ignored, mirroring
// internal/payments' identical hasLegacySignatureField rationale.
func hasLegacySignatureField(generic any) bool {
	obj, ok := generic.(map[string]any)
	if !ok {
		return false
	}
	_, present := obj["signature"]
	return present
}

// callbackBodyHasNULByte reports whether any string field of body contains
// a literal NUL byte (a JSON string may legally decode one via a unicode
// escape). Kept as a post-verification structural validation (design §C2
// point 2): a NUL byte in an identifying field is still rejected, even
// though it is no longer part of any MAC-framing canonicalization now that
// the signature covers the raw body bytes directly.
func callbackBodyHasNULByte(body mockCasinoCallbackBody) bool {
	for _, s := range []string{
		body.EventType, body.ProviderTxID, body.OriginalProviderTxID, body.RoundID, body.ProviderGameID,
		body.AssetCode, body.Outcome, body.DeclineReason, body.PlayerAccountID, body.SessionID,
	} {
		if strings.ContainsRune(s, 0) {
			return true
		}
	}
	return false
}

// CallbackPayload builds a synthetic webhook callback, as a
// webhookauth.Inbound (raw JSON body plus X-Casino-Signature/X-Casino-
// Key-Id headers), in the shape MockCasinoProvider.HandleCallback verifies
// and parses - a test/simulation-route helper standing in for "the
// provider's real callback delivery" (Stage 10.2, CAS-WH-TENANT-1, design
// §C2, mirroring internal/payments.MockProvider.CallbackPayload exactly).
//
// tenantID is ALWAYS the caller's own route/JWT-resolved tenant (never
// accepted from anywhere a caller could name a different one) - it is
// bound into the signature via deriveKey/SigningInput exactly as a real
// verification would rebuild it, so this helper cannot itself mint a
// payload that verifies for any tenant other than the one named here.
//
// sessionID is required for a bet event (ReceiveCallback's postBet rejects
// a bet with no session binding) and ignored by win/rollback; pass
// uuid.Nil for those.
func (m *MockCasinoProvider) CallbackPayload(tenantID uuid.UUID, eventType CallbackEventType, providerTxID, originalProviderTxID, roundID, providerGameID string, amount int64, assetCode string, outcome Outcome, declineReason string, playerAccountID, sessionID uuid.UUID) webhookauth.Inbound {
	body := mockCasinoCallbackBody{
		EventType: string(eventType), ProviderTxID: providerTxID, OriginalProviderTxID: originalProviderTxID,
		RoundID: roundID, ProviderGameID: providerGameID, Amount: amount, AssetCode: assetCode,
		Outcome: string(outcome), DeclineReason: declineReason, PlayerAccountID: playerAccountID.String(),
	}
	if sessionID != uuid.Nil {
		body.SessionID = sessionID.String()
	}
	raw, _ := json.Marshal(body)
	return m.signRawBody(tenantID, raw)
}

// SignRawBody is CallbackPayload's counterpart for tests that need a
// correctly-signed callback whose body carries something outside
// mockCasinoCallbackBody's fixed shape - e.g. a legacy-shape rejection
// test that must exercise HandleCallback's post-verification parsing.
// MOCK/TEST-ONLY, exactly like CallbackPayload - no production code path
// calls this.
func (m *MockCasinoProvider) SignRawBody(tenantID uuid.UUID, body []byte) webhookauth.Inbound {
	return m.signRawBody(tenantID, body)
}

func (m *MockCasinoProvider) signRawBody(tenantID uuid.UUID, body []byte) webhookauth.Inbound {
	key := m.deriveKey(tenantID, m.providerID)
	sig := casinoScheme.Sign(key, tenantID, m.providerID, mockWebhookKeyID, body)

	header := make(http.Header)
	casinoScheme.SetHeaders(header, mockWebhookKeyID, sig)

	return webhookauth.Inbound{TenantID: tenantID, ProviderID: m.providerID, Header: header, Body: body}
}

// HandleCallback implements CasinoProvider. Verification (over the raw
// body bytes, via webhookauth.Scheme.Verify) runs BEFORE any parsing
// (Stage 10.2, CAS-WH-TENANT-1, ADR 0091, design §C2 point 1) - a caller
// who does not know cred.Secret cannot get past this line regardless of
// body shape, so a non-JSON body is indistinguishable from any other
// pre-verification failure. Every rejection after this line has proven
// knowledge of cred.Secret - it is a POST-verification failure. Almost all
// of them are the structural ErrCallbackMalformedBody, with one deliberate
// carve-out (Stage 10.2 final review, K7/F-8): a body still carrying the
// legacy `signature` field returns ErrCallbackSignatureInvalid (401), even
// though the sender already verified. That is a known, accepted false
// forgery-alert signal for a verified sender - see ADR 0022 §3 amendment.
func (m *MockCasinoProvider) HandleCallback(_ context.Context, in webhookauth.Inbound, cred webhookauth.Credential) (CallbackEvent, error) {
	if err := casinoScheme.Verify(cred, in); err != nil {
		return CallbackEvent{}, ErrCallbackSignatureInvalid
	}

	var generic any
	if err := json.Unmarshal(in.Body, &generic); err != nil {
		return CallbackEvent{}, fmt.Errorf("%w: parse callback: %v", ErrCallbackMalformedBody, err)
	}
	if hasLegacySignatureField(generic) {
		return CallbackEvent{}, ErrCallbackSignatureInvalid
	}

	var body mockCasinoCallbackBody
	if err := json.Unmarshal(in.Body, &body); err != nil {
		return CallbackEvent{}, fmt.Errorf("%w: parse callback: %v", ErrCallbackMalformedBody, err)
	}
	if callbackBodyHasNULByte(body) {
		return CallbackEvent{}, fmt.Errorf("%w: field contains a NUL byte", ErrCallbackMalformedBody)
	}
	if body.ProviderTxID == "" {
		return CallbackEvent{}, fmt.Errorf("%w: callback missing provider_tx_id", ErrCallbackMalformedBody)
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
		return CallbackEvent{}, fmt.Errorf("%w: unknown callback event_type %q", ErrCallbackMalformedBody, body.EventType)
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
		return CallbackEvent{}, fmt.Errorf("%w: unknown callback outcome %q", ErrCallbackMalformedBody, body.Outcome)
	}

	var playerAccountID uuid.UUID
	if body.PlayerAccountID != "" {
		parsed, err := uuid.Parse(body.PlayerAccountID)
		if err != nil {
			return CallbackEvent{}, fmt.Errorf("%w: invalid player_account_id: %v", ErrCallbackMalformedBody, err)
		}
		playerAccountID = parsed
	}
	var sessionID uuid.UUID
	if body.SessionID != "" {
		parsed, err := uuid.Parse(body.SessionID)
		if err != nil {
			return CallbackEvent{}, fmt.Errorf("%w: invalid session_id: %v", ErrCallbackMalformedBody, err)
		}
		sessionID = parsed
	}

	return CallbackEvent{
		EventType: eventType, ProviderTxID: body.ProviderTxID, OriginalProviderTxID: body.OriginalProviderTxID,
		RoundID: body.RoundID, ProviderGameID: body.ProviderGameID, Amount: body.Amount, AssetCode: body.AssetCode,
		Outcome: outcome, DeclineReason: body.DeclineReason, PlayerAccountID: playerAccountID, SessionID: sessionID,
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
