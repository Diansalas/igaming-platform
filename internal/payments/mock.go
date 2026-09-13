package payments

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// MockProvider is a PaymentProvider implementation with synthetic,
// test-controllable success/decline/ambiguous behavior - Stage 3B's only
// adapter (docs/architecture/payment-orchestration.md §2: "the mock PSP
// used in Stage 3B implementation is itself just another PaymentProvider
// implementation... not a special code path"). It goes through
// RouteProvider/ReceiveCallback exactly like a hypothetical real adapter
// would; the orchestrator contains no MockProvider-specific branch.
//
// MOCK: labeled per CLAUDE.md's "No fake completion" rule - this is a
// synthetic double for development/testing, never a real payment rail.
//
// Deposit outcome is selected by Amount, documented here rather than left
// implicit, so tests are self-explanatory:
//
//	amount == mockAmountPlayerDeclineNoCascade  -> synchronous decline, NOT cascadable (simulates "insufficient funds")
//	amount == mockAmountProviderDeclineCascade  -> synchronous decline, cascadable (simulates a provider-side outage another provider might handle)
//	amount == mockAmountAmbiguous               -> synchronous OutcomeAmbiguous, with a provider reference QueryStatus can later resolve
//	any other amount                            -> OutcomePending (normal async/hosted-redirect flow), resolved later via HandleCallback
type MockProvider struct {
	providerID string
	capability AdapterCapability
	health     ProviderHealth

	// AcceptAllAmounts, when true, makes this instance ignore the magic
	// amounts below and always return OutcomePending - a test-only knob
	// for standing up a second, "always healthy/accepting" mock instance
	// in a cascade-on-decline test, where the FIRST provider must decline
	// a given amount while the SECOND accepts the identical request.
	// Never read by anything except this adapter's own methods; it has no
	// analogue in a real adapter, which cannot be told to "ignore" its own
	// issuer/rail behavior.
	AcceptAllAmounts bool

	mu       sync.Mutex
	attempts map[string]*mockAttempt
	seq      int

	// signingSecret is this adapter instance's HMAC key, generated at
	// construction. A real adapter authenticates its webhook via whatever
	// mechanism its vendor actually uses (signature header, mTLS,
	// account-specific shared secret, ...) - HandleCallback existing only
	// to parse an already-trusted payload is not itself a security defect,
	// but this mock previously did NOT enforce any check at all, and it is
	// the one adapter cmd/platform-api/main.go actually registers. That
	// made POST /v1/webhooks/payments/{tenantSlug}/{providerID} - which
	// has no bearer-auth middleware by design, since a provider webhook
	// isn't an authenticated platform principal - trivially forgeable:
	// provider_reference values are sequential ("mock-1", "mock-2", ...)
	// and are even returned to the player in InitiateDeposit's
	// redirect_url, so anyone could post a synthetic "succeeded" callback
	// for any reference and mint an arbitrary ledger credit. This closes
	// that hole for the mock exactly as payment-orchestration.md §3
	// requires of every adapter: "webhook signature verification happens
	// inside HandleCallback before any payload field is used."
	signingSecret []byte
}

// Magic amounts (minor units) driving MockProvider.Deposit's synthetic
// behavior - see the type doc comment above.
const (
	MockAmountPlayerDeclineNoCascade int64 = 111
	MockAmountProviderDeclineCascade int64 = 222
	MockAmountAmbiguous              int64 = 333
)

type mockAttempt struct {
	kind          string // "deposit" | "withdraw"
	amount        int64
	assetCode     string
	outcome       Outcome
	declineReason string
	cascadable    bool
}

// NewMockProvider constructs a mock adapter registered under providerID
// (tests may register more than one instance under different ids to
// exercise cascade-on-decline against a second candidate). fiatCurrencies
// controls Capabilities()'s declared support - callers pass the assets
// this mock instance should accept in a given test.
func NewMockProvider(providerID string, fiatCurrencies ...string) *MockProvider {
	if len(fiatCurrencies) == 0 {
		fiatCurrencies = []string{"EUR", "USD"}
	}
	limits := make([]AmountLimit, 0, len(fiatCurrencies))
	for _, cur := range fiatCurrencies {
		limits = append(limits, AmountLimit{AssetCode: cur, MinAmount: 100, MaxAmount: 1_000_000_00})
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		// crypto/rand failing is a fatal platform problem, not a
		// recoverable one - never fall back to a predictable secret.
		panic(fmt.Sprintf("payments/mock: failed to generate signing secret: %v", err))
	}
	return &MockProvider{
		signingSecret: secret,
		providerID:    providerID,
		capability: AdapterCapability{
			ProviderID:              providerID,
			ProviderKind:            ProviderKindFiat,
			SupportedFiatCurrencies: fiatCurrencies,
			SupportedCryptoAssets:   nil,
			SupportedPaymentMethods: []string{"card", "bank_transfer"},
			SupportedCountries:      nil, // unrestricted
			SupportsDeposit:         true,
			SupportsWithdrawal:      true,
			SupportsRefundReversal:  true,
			AmountLimits:            limits,
			SettlementBehavior:      "instant",
			CallbackCapabilities:    CallbackWebhookOnly,
		},
		health: ProviderHealth{
			ProviderID:          providerID,
			RollingSuccessRate:  1.0,
			RollingLatencyP99Ms: 50,
			CircuitState:        CircuitClosed,
		},
		attempts: make(map[string]*mockAttempt),
	}
}

// SetHealth lets a test drive routing/circuit-breaker behavior
// deterministically.
func (m *MockProvider) SetHealth(h ProviderHealth) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h.ProviderID = m.providerID
	m.health = h
}

// Resolve lets a test simulate a delayed provider-side resolution of a
// previously-ambiguous or previously-pending attempt, observable on the
// next QueryStatus call - the synthetic equivalent of "the provider's
// backend eventually finds out what really happened."
func (m *MockProvider) Resolve(providerReference string, outcome Outcome, declineReason string, cascadable bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := m.attempts[providerReference]; ok {
		a.outcome = outcome
		a.declineReason = declineReason
		a.cascadable = cascadable
	}
}

// CallbackPayload builds a synthetic webhook body for providerReference,
// as JSON, in the shape MockProvider.HandleCallback parses - test helper
// standing in for "the provider's real webhook delivery", since Stage 3B
// has no real PSP to deliver one. Signs the body with this instance's own
// signingSecret so it passes HandleCallback's verification exactly as a
// real, correctly-authenticated provider delivery would.
func (m *MockProvider) CallbackPayload(eventType CallbackEventType, providerReference, originalProviderReference string, outcome Outcome, amount int64, assetCode, declineReason string, cascadable bool) []byte {
	body := mockCallbackBody{
		EventType:                 string(eventType),
		ProviderReference:         providerReference,
		OriginalProviderReference: originalProviderReference,
		Outcome:                   string(outcome),
		Amount:                    amount,
		AssetCode:                 assetCode,
		DeclineReason:             declineReason,
		Cascadable:                cascadable,
	}
	body.Signature = m.sign(body)
	marshalled, _ := json.Marshal(body)
	return marshalled
}

// sign computes this instance's HMAC-SHA256 over the callback's own
// identifying/effect-bearing fields (never including Signature itself,
// which would make verification vacuous). Field values are joined with a
// separator absent from any of them (provider_reference/asset_code are
// adapter-controlled identifiers, outcome/event_type are closed enums,
// decline_reason is adapter-controlled text) so no combination of field
// values can be reinterpreted as a different set of fields.
func (m *MockProvider) sign(body mockCallbackBody) string {
	mac := hmac.New(sha256.New, m.signingSecret)
	fmt.Fprintf(mac, "%s\x00%s\x00%s\x00%s\x00%d\x00%s\x00%s\x00%t",
		body.EventType, body.ProviderReference, body.OriginalProviderReference,
		body.Outcome, body.Amount, body.AssetCode, body.DeclineReason, body.Cascadable)
	return hex.EncodeToString(mac.Sum(nil))
}

func (m *MockProvider) nextReference() string {
	m.seq++
	return fmt.Sprintf("%s-%d", m.providerID, m.seq)
}

// Deposit implements PaymentProvider.
func (m *MockProvider) Deposit(_ context.Context, req DepositRequest) (DepositResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ref := m.nextReference()
	if m.AcceptAllAmounts {
		m.attempts[ref] = &mockAttempt{kind: "deposit", amount: req.Amount, assetCode: req.AssetCode, outcome: OutcomePending}
		return DepositResult{Outcome: OutcomePending, ProviderReference: ref, RedirectURL: "https://mock-psp.invalid/pay/" + ref}, nil
	}
	switch req.Amount {
	case MockAmountPlayerDeclineNoCascade:
		m.attempts[ref] = &mockAttempt{kind: "deposit", amount: req.Amount, assetCode: req.AssetCode, outcome: OutcomeDeclined, declineReason: "insufficient_funds"}
		return DepositResult{Outcome: OutcomeDeclined, ProviderReference: ref, DeclineReason: "insufficient_funds", Cascadable: false}, nil
	case MockAmountProviderDeclineCascade:
		m.attempts[ref] = &mockAttempt{kind: "deposit", amount: req.Amount, assetCode: req.AssetCode, outcome: OutcomeDeclined, declineReason: "provider_unavailable", cascadable: true}
		return DepositResult{Outcome: OutcomeDeclined, ProviderReference: ref, DeclineReason: "provider_unavailable", Cascadable: true}, nil
	case MockAmountAmbiguous:
		m.attempts[ref] = &mockAttempt{kind: "deposit", amount: req.Amount, assetCode: req.AssetCode, outcome: OutcomeAmbiguous}
		return DepositResult{Outcome: OutcomeAmbiguous, ProviderReference: ref}, nil
	default:
		m.attempts[ref] = &mockAttempt{kind: "deposit", amount: req.Amount, assetCode: req.AssetCode, outcome: OutcomePending}
		return DepositResult{Outcome: OutcomePending, ProviderReference: ref, RedirectURL: "https://mock-psp.invalid/pay/" + ref}, nil
	}
}

// Withdraw implements PaymentProvider. Not exercised by the orchestrator
// this stage (withdrawal orchestration is NOT IMPLEMENTED yet) but
// implemented fully so the conformance suite covers it for whichever
// future stage wires it up.
func (m *MockProvider) Withdraw(_ context.Context, req WithdrawRequest) (WithdrawResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ref := m.nextReference()
	switch req.Amount {
	case MockAmountPlayerDeclineNoCascade:
		m.attempts[ref] = &mockAttempt{kind: "withdraw", amount: req.Amount, assetCode: req.AssetCode, outcome: OutcomeDeclined, declineReason: "account_closed"}
		return WithdrawResult{Outcome: OutcomeDeclined, ProviderReference: ref, DeclineReason: "account_closed", Cascadable: false}, nil
	case MockAmountProviderDeclineCascade:
		m.attempts[ref] = &mockAttempt{kind: "withdraw", amount: req.Amount, assetCode: req.AssetCode, outcome: OutcomeDeclined, declineReason: "provider_unavailable", cascadable: true}
		return WithdrawResult{Outcome: OutcomeDeclined, ProviderReference: ref, DeclineReason: "provider_unavailable", Cascadable: true}, nil
	case MockAmountAmbiguous:
		m.attempts[ref] = &mockAttempt{kind: "withdraw", amount: req.Amount, assetCode: req.AssetCode, outcome: OutcomeAmbiguous}
		return WithdrawResult{Outcome: OutcomeAmbiguous, ProviderReference: ref}, nil
	default:
		m.attempts[ref] = &mockAttempt{kind: "withdraw", amount: req.Amount, assetCode: req.AssetCode, outcome: OutcomePending}
		return WithdrawResult{Outcome: OutcomePending, ProviderReference: ref}, nil
	}
}

// QueryStatus implements PaymentProvider.
func (m *MockProvider) QueryStatus(_ context.Context, providerReference string) (StatusResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	a, ok := m.attempts[providerReference]
	if !ok {
		return StatusResult{}, fmt.Errorf("payments/mock: unknown provider reference %q", providerReference)
	}
	return StatusResult{
		ProviderReference: providerReference,
		Outcome:           a.outcome,
		Amount:            a.amount,
		AssetCode:         a.assetCode,
		DeclineReason:     a.declineReason,
		Cascadable:        a.cascadable,
	}, nil
}

// mockCallbackBody is the synthetic wire shape MockProvider's webhook
// parser accepts - the mock's stand-in for "whatever JSON/form shape a
// real PSP's webhook actually uses", which a real adapter would parse
// into the same canonical CallbackEvent.
type mockCallbackBody struct {
	EventType                 string `json:"event_type"`
	ProviderReference         string `json:"provider_reference"`
	OriginalProviderReference string `json:"original_provider_reference,omitempty"`
	Outcome                   string `json:"outcome"`
	Amount                    int64  `json:"amount"`
	AssetCode                 string `json:"asset_code"`
	DeclineReason             string `json:"decline_reason,omitempty"`
	Cascadable                bool   `json:"cascadable,omitempty"`
	// Signature is this instance's HMAC-SHA256 (hex-encoded) over the
	// other fields via MockProvider.sign - HandleCallback verifies it
	// before acting on anything else. A real adapter's equivalent field
	// (or header) would carry the vendor's own signature scheme instead.
	Signature string `json:"signature,omitempty"`
}

// keyMaterialFieldNames is the deny-list HandleCallback scans every
// (possibly nested) JSON object key against, per docs/decisions/0022
// §4.1: "an API response or webhook returns a private key or WIF... a
// seed phrase/mnemonic, an xpub plus derivation path... or a
// local-signing SDK handle." Matching is case-insensitive and by
// substring, deliberately over-inclusive - a false positive here just
// rejects a legitimate field name an adapter should not have chosen
// anyway, while a false negative is a custody-boundary violation.
var keyMaterialFieldNames = []string{
	"private_key", "priv_key", "privatekey", "secret_key", "secretkey",
	"seed", "mnemonic", "xprv", "wif", "signing_key", "signingkey",
}

// HandleCallback implements PaymentProvider. It first scans the raw
// payload generically (before any typed parsing) for field names that
// look like key material and rejects the whole operation without ever
// logging or persisting the offending value (docs/decisions/0022 §4.1
// point 2) - a typed json.Unmarshal into mockCallbackBody alone would
// silently DROP an unrecognized field like "seed_phrase" rather than
// reject it, which is exactly the "silently ignoring the field is not
// sufficient" failure mode that ADR warns against. It then verifies the
// embedded HMAC signature (see sign) before acting on any other field -
// the property the security review demanded: a caller who does not know
// this instance's signingSecret cannot post a payload HandleCallback will
// accept, regardless of how it reaches the process (the HTTP webhook
// route has no bearer-auth middleware by design, since a provider webhook
// isn't an authenticated platform principal - this is what authenticates
// it instead).
func (m *MockProvider) HandleCallback(_ context.Context, rawPayload []byte) (CallbackEvent, error) {
	var generic any
	if err := json.Unmarshal(rawPayload, &generic); err != nil {
		return CallbackEvent{}, fmt.Errorf("payments/mock: parse callback: %w", err)
	}
	if containsKeyMaterialField(generic) {
		// Deliberately no field value, no raw payload, in this error or
		// anywhere else - see ErrInboundKeyMaterial's doc comment.
		return CallbackEvent{}, ErrInboundKeyMaterial
	}

	var body mockCallbackBody
	if err := json.Unmarshal(rawPayload, &body); err != nil {
		return CallbackEvent{}, fmt.Errorf("payments/mock: parse callback: %w", err)
	}
	if body.ProviderReference == "" {
		return CallbackEvent{}, fmt.Errorf("payments/mock: callback missing provider_reference")
	}

	// Authenticate before acting on a single other field - payment-
	// orchestration.md §3: "webhook signature verification happens inside
	// HandleCallback before any payload field is used... An unverified
	// payload never reaches the ledger posting API." hmac.Equal is
	// constant-time; a plain == comparison here would leak timing
	// information about how many leading bytes matched.
	expected := m.sign(body)
	if body.Signature == "" || !hmac.Equal([]byte(expected), []byte(body.Signature)) {
		return CallbackEvent{}, ErrCallbackSignatureInvalid
	}

	var eventType CallbackEventType
	switch body.EventType {
	case string(CallbackEventDeposit):
		eventType = CallbackEventDeposit
	case string(CallbackEventDepositReversal):
		eventType = CallbackEventDepositReversal
	default:
		return CallbackEvent{}, fmt.Errorf("payments/mock: unknown callback event_type %q", body.EventType)
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
	default:
		return CallbackEvent{}, fmt.Errorf("payments/mock: unknown callback outcome %q", body.Outcome)
	}

	return CallbackEvent{
		EventType:                 eventType,
		ProviderReference:         body.ProviderReference,
		OriginalProviderReference: body.OriginalProviderReference,
		Outcome:                   outcome,
		Amount:                    body.Amount,
		AssetCode:                 body.AssetCode,
		DeclineReason:             body.DeclineReason,
		Cascadable:                body.Cascadable,
	}, nil
}

func containsKeyMaterialField(v any) bool {
	switch val := v.(type) {
	case map[string]any:
		for k, sub := range val {
			lower := strings.ToLower(k)
			for _, bad := range keyMaterialFieldNames {
				if strings.Contains(lower, bad) {
					return true
				}
			}
			if containsKeyMaterialField(sub) {
				return true
			}
		}
	case []any:
		for _, sub := range val {
			if containsKeyMaterialField(sub) {
				return true
			}
		}
	}
	return false
}

// Capabilities implements PaymentProvider.
func (m *MockProvider) Capabilities() AdapterCapability {
	return m.capability
}

// HealthStatus implements PaymentProvider.
func (m *MockProvider) HealthStatus(_ context.Context) (ProviderHealth, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.health, nil
}
