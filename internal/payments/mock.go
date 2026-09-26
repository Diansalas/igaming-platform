package payments

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// mockWebhookKeyID is the only KeyID the mock resolver/adapter ever uses.
// Stage 10.1 (PAY-WH-TENANT-1) ships exactly one generation; a future key
// rotation would add a second, coexisting id, never replace this one
// in-place.
const mockWebhookKeyID = webhookauth.MockKeyID

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

	// masterSecret is this adapter INSTANCE's own process-local secret,
	// generated at construction (never config, never the repo, never
	// recoverable from outside this process). It is used ONLY to DERIVE
	// per-(tenant, provider) webhook signing keys (deriveKey below) - it is
	// never itself used to sign or verify a callback directly. Renamed
	// from Stage 3B's "signingSecret" for PAY-WH-TENANT-1 (ADR 0090; ADR
	// 0022 §3 amendment 2026-09-25/26): that earlier single key was shared
	// by every tenant, so a callback correctly signed for tenant A also
	// verified for tenant B (S-6) - closing this required moving the
	// tenant into WHAT is verified (deriveKey + the signing_input's own
	// tenant_id/provider_id prefix), not merely adding a tenant CHECK
	// after the fact.
	//
	// MOCK ONLY: deriving every tenant's key from one process-local master
	// via HMAC is acceptable ONLY because this is a synthetic development
	// double with no real money or real vendor relationship behind it. A
	// real adapter's per-tenant credentials come from an independent,
	// tenant-scoped secret store (docs/decisions/0022 §2.2,
	// WebhookCredentialResolver's real - NOT IMPLEMENTED - resolver), never
	// derived from a single shared platform-side secret.
	masterSecret []byte
}

// deriveKey computes this instance's per-(tenantID, providerID) webhook
// signing key: HMAC-SHA256(masterSecret, "igaming/payments-mock-webhook/v1"
// 0x00 tenant_id 0x00 provider_id) - delegated, byte-identically, to
// webhookauth.DeriveMockKey with the payments mock key label (Stage 10.2,
// ADR 0091). NUL-separated so no ambiguity exists between e.g. tenant "ab"
// + provider "c" and tenant "a" + provider "bc" - UUIDs never contain 0x00
// and provider_id is charset-restricted (webhookauth.ValidProviderID) to exclude it
// too.
func (m *MockProvider) deriveKey(tenantID uuid.UUID, providerID string) []byte {
	return webhookauth.DeriveMockKey(m.masterSecret, webhookauth.PaymentsMockKeyLabel, tenantID, providerID)
}

// SyntheticComponent implements providerkind.Synthetic (Stage 10.3,
// MOCK-ADAPTER-PROD-1) - a structural marker only, satisfied without this
// package importing internal/providerkind.
func (m *MockProvider) SyntheticComponent() {}

// MockWebhookCredentials is the MOCK WebhookCredentialResolver
// (docs/decisions/0022 §3 amendment; design §6). It resolves ONLY
// KeyID=="mock-v1" for its own bound provider id - any other providerID or
// keyID is ErrWebhookCredentialUnavailable, which the Orchestrator folds
// into ReasonCredentialUnavailable (fail closed, never a fallback to
// unauthenticated verification). Labeled MOCK per CLAUDE.md: the real
// resolver (a FORCE-RLS handle table plus an external secret store) is NOT
// IMPLEMENTED. Stage 10.2: a thin wrapper over webhookauth.MockResolver
// that keeps this API.
type MockWebhookCredentials struct {
	provider *MockProvider
}

// NewMockWebhookCredentials constructs the resolver bound to provider -
// cmd/platform-api/main.go wires exactly one of these per registered mock
// adapter instance into the Orchestrator's single injected
// WebhookCredentialResolver map (keyed by provider_id, since the mock is
// the only adapter that exists this stage).
func NewMockWebhookCredentials(provider *MockProvider) MockWebhookCredentials {
	return MockWebhookCredentials{provider: provider}
}

// SyntheticComponent implements providerkind.Synthetic (Stage 10.3,
// MOCK-ADAPTER-PROD-1) - a structural marker only, satisfied without this
// package importing internal/providerkind.
func (r MockWebhookCredentials) SyntheticComponent() {}

// Resolve implements WebhookCredentialResolver (ADR 0093 §4 signature).
// A MOCK resolver ignores tx and resolves KeyFromHeader only.
func (r MockWebhookCredentials) Resolve(ctx context.Context, _ pgx.Tx, tenantID uuid.UUID, providerID, keyID string, sel webhookauth.KeySelection) (webhookauth.CredentialSet, error) {
	return webhookauth.ResolveSingleKey(ctx, r, tenantID, providerID, keyID, sel)
}

// ResolveKey implements webhookauth.KeyResolver: the MOCK single-key lookup.
func (r MockWebhookCredentials) ResolveKey(ctx context.Context, tenantID uuid.UUID, providerID, keyID string) (WebhookCredential, error) {
	if r.provider == nil {
		return WebhookCredential{}, ErrWebhookCredentialUnavailable
	}
	return webhookauth.MockResolver{
		Master:     r.provider.masterSecret,
		Label:      webhookauth.PaymentsMockKeyLabel,
		ProviderID: r.provider.providerID,
	}.ResolveKey(ctx, tenantID, providerID, keyID)
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
	// crypto/rand failing is a fatal platform problem, not a recoverable
	// one - NewMockMaster panics rather than ever falling back to a
	// predictable secret.
	secret := webhookauth.NewMockMaster()
	return &MockProvider{
		masterSecret: secret,
		providerID:   providerID,
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

// SetConfirmedAmount lets a test simulate a provider whose QueryStatus
// response reports a DIFFERENT amount/asset than what was originally
// submitted (partial settlement, a fee-adjusted figure, a provider-side
// data error) - the synthetic equivalent of a real PSP's confirmed
// facts disagreeing with the request that was sent. Stage 3C hardening:
// this is what proves newResolveWithdrawalHandler's amount/asset
// cross-check (payments.ErrCallbackProviderMismatch) actually fires
// rather than blindly trusting the reference match.
func (m *MockProvider) SetConfirmedAmount(providerReference string, amount int64, assetCode string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a, ok := m.attempts[providerReference]; ok {
		a.amount = amount
		a.assetCode = assetCode
	}
}

// CallbackPayload builds a synthetic, TENANT-BOUND webhook delivery for
// providerReference: the raw JSON body (with no body-embedded signature
// field - PAY-WH-TENANT-1 removes that Stage 3B shape) plus the
// X-Payments-Signature/X-Payments-Key-Id headers, in the exact
// InboundCallback shape MockProvider.HandleCallback verifies - test/
// simulation-route helper standing in for "the provider's real webhook
// delivery", since Stage 3B/10.1 have no real PSP to deliver one.
//
// tenantID is ALWAYS the caller's own route/JWT-resolved tenant (never
// accepted from anywhere a caller could name a different one) - it is
// bound into the signature via deriveKey/signingInput exactly as a real
// verification would rebuild it, so this helper cannot itself mint a
// payload that verifies for any tenant other than the one named here.
func (m *MockProvider) CallbackPayload(tenantID uuid.UUID, eventType CallbackEventType, providerReference, originalProviderReference string, outcome Outcome, amount int64, assetCode, declineReason string, cascadable bool) InboundCallback {
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
	raw, _ := json.Marshal(body)

	key := m.deriveKey(tenantID, m.providerID)
	sig := signWithKey(key, tenantID, m.providerID, mockWebhookKeyID, raw)

	header := make(http.Header)
	paymentsScheme.SetHeaders(header, mockWebhookKeyID, sig)

	return InboundCallback{TenantID: tenantID, ProviderID: m.providerID, Header: header, Body: raw}
}

// SignRawBody is CallbackPayload's counterpart for tests that need a
// correctly-signed callback whose body carries something outside
// mockCallbackBody's fixed shape - e.g. a bogus extra field the key-
// material scan or a legacy-shape rejection test needs to exercise AFTER
// signature verification succeeds (Stage 10.1 security review P2-1/code
// review F1/architect PW-1's HandleCallback reorder means a body and its
// signature must now genuinely match for anything past verification to
// ever run). Like CallbackPayload, tenantID is always the caller's own
// route/JWT-resolved tenant; this cannot mint a payload that verifies for
// any tenant other than the one named here. MOCK/TEST-ONLY, exactly like
// CallbackPayload - no production code path calls this.
func (m *MockProvider) SignRawBody(tenantID uuid.UUID, body []byte) InboundCallback {
	key := m.deriveKey(tenantID, m.providerID)
	sig := signWithKey(key, tenantID, m.providerID, mockWebhookKeyID, body)

	header := make(http.Header)
	paymentsScheme.SetHeaders(header, mockWebhookKeyID, sig)

	return InboundCallback{TenantID: tenantID, ProviderID: m.providerID, Header: header, Body: body}
}

// signWithKey returns hex(HMAC-SHA256(key, signing_input)) over the
// payments domain's platform-defined MOCK scheme (docs/decisions/0022 §3
// amendment §2.2): SigningInputPrefix 0x00 tenant_id 0x00 provider_id 0x00
// key_id 0x00 <raw body bytes>. Stage 10.2: delegated, byte-identically, to
// webhookauth.Scheme.Sign - the raw body bytes are signed directly, never a
// re-serialization of parsed fields.
func signWithKey(key []byte, tenantID uuid.UUID, providerID, keyID string, body []byte) string {
	return paymentsScheme.Sign(key, tenantID, providerID, keyID, body)
}

// nextReference mints a provider_reference unique across every replica of
// this process, not just within one. A bare per-process counter (the
// original implementation) lets two ECS/Fargate replicas each mint
// "mock-payments-1" for their own first deposit; those references then
// collide on deposit_intents' (tenant_id, provider_id, provider_reference)
// uniqueness constraint and alias onto the same ledger idempotency key
// (provider_id, provider_tx_id) used by internal/payments/orchestrator.go -
// a genuine multi-replica correctness bug, not merely a cosmetic one,
// found during Stage 9.4's activation of desired_count=2 for platform-api.
// The sequence number is kept as a human-readable, per-process ordering
// hint for logs/debugging; uuid.NewString() is what actually guarantees
// cross-replica uniqueness, exactly as m.signingSecret's doc comment above
// already documents this mock relying on signature verification - not
// reference unpredictability - for forgery resistance, so widening the
// reference format here does not change that security property.
func (m *MockProvider) nextReference() string {
	m.seq++
	return fmt.Sprintf("%s-%d-%s", m.providerID, m.seq, uuid.NewString())
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
}

// hasLegacySignatureField reports whether the top-level JSON object still
// carries a "signature" field - PAY-WH-TENANT-1 moves the signature into
// the X-Payments-Signature header exclusively (design §2.2); a body that
// still embeds one (e.g. a caller replaying a Stage-3B-shaped payload) is
// rejected rather than silently ignored, mirroring the same "silently
// dropping an unexpected field is not sufficient" principle
// containsKeyMaterialField already applies (docs/decisions/0022 §4.1).
func hasLegacySignatureField(generic any) bool {
	obj, ok := generic.(map[string]any)
	if !ok {
		return false
	}
	_, present := obj["signature"]
	return present
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

// HandleCallback implements PaymentProvider. Verification order (§3.1
// steps (d)-(f), run only after the Orchestrator has already completed
// (a)-(c) - provider registered, ProviderAcceptsWebhook, and credential
// resolution/equality against req.TenantID/req.ProviderID):
//
//  1. extract and re-validate the X-Payments-Signature/X-Payments-Key-Id
//     headers (redundant with the Orchestrator's own check, but this
//     method must be self-sufficient for direct unit/conformance tests
//     that bypass the HTTP layer);
//  2. verify the HMAC over signingInput(req.TenantID, req.ProviderID,
//     keyID, req.Body) using cred.Secret - the credential the Orchestrator
//     already resolved and equality-checked, never re-resolved here.
//     hmac.Equal is constant-time; a plain == comparison would leak timing
//     information about how many leading bytes matched. This step runs
//     over the RAW body bytes and needs no parsing at all, which is
//     exactly why steps 1-2 can, and must, run before any JSON parsing
//     happens (see the note below);
//  3. only once (2) has SUCCEEDED: parse the raw body generically and scan
//     it for field names that look like key material, rejecting without
//     ever logging or persisting the offending value
//     (docs/decisions/0022 §4.1);
//  4. reject a body that still carries a legacy "signature" field
//     (hasLegacySignatureField) - Stage 3B's now-removed wire shape;
//  5. parse the typed fields.
//
// Security review P2-1 / code review F1 / architect PW-1 (Stage 10.1): the
// generic JSON parse (needed only for the key-material scan and the
// legacy-field check) used to run BEFORE step 2, so a non-JSON body with
// well-formed headers returned a bare, un-typed parse error instead of an
// auth failure - breaking the uniform-401 contract with a distinguishable
// 500 (and logging a fragment of the unauthenticated body). Every failure
// before HMAC verification succeeds must be indistinguishable from every
// other one, so the parse now happens strictly AFTER (2): an unparseable
// body can only be reached once the caller has already proven it knows the
// shared secret, at which point it is a genuine (if malformed) delivery
// from an authenticated source, never an unauthenticated attacker's probe.
// A parse failure at that point is reported as ErrCallbackMalformedBody -
// a DIFFERENT sentinel from the pre-verification auth failures - which the
// Orchestrator/HTTP layer maps to a 400, not the uniform 401 (PW-2).
func (m *MockProvider) HandleCallback(_ context.Context, req InboundCallback, cred WebhookCredential) (CallbackEvent, error) {
	// Steps 1-2 (header re-validation, key id == cred.KeyID, cred bound to
	// req's route tenant/provider, constant-time HMAC over the raw bytes)
	// are the shared webhookauth.Scheme.Verify (Stage 10.2, ADR 0091), whose
	// only failure value IS ErrCallbackSignatureInvalid.
	if err := paymentsScheme.Verify(cred, req); err != nil {
		return CallbackEvent{}, ErrCallbackSignatureInvalid
	}

	// From here on, the caller has proven knowledge of the shared secret -
	// every subsequent rejection is a POST-verification, structural
	// failure (ErrCallbackMalformedBody or ErrInboundKeyMaterial), never
	// one of the auth sentinels above.
	var generic any
	if err := json.Unmarshal(req.Body, &generic); err != nil {
		return CallbackEvent{}, fmt.Errorf("%w: parse callback: %v", ErrCallbackMalformedBody, err)
	}
	if containsKeyMaterialField(generic) {
		// Deliberately no field value, no raw payload, in this error or
		// anywhere else - see ErrInboundKeyMaterial's doc comment. Still
		// reported as a security event (ReasonKeyMaterial via
		// ErrInboundKeyMaterial), not a generic malformed-body 400, since a
		// VERIFIED sender emitting apparent key material is exactly the
		// ADR 0022 §4.1 scenario, independent of where in this function it
		// is detected.
		return CallbackEvent{}, ErrInboundKeyMaterial
	}

	if hasLegacySignatureField(generic) {
		return CallbackEvent{}, ErrCallbackSignatureInvalid
	}

	var body mockCallbackBody
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return CallbackEvent{}, fmt.Errorf("%w: parse callback: %v", ErrCallbackMalformedBody, err)
	}
	if body.ProviderReference == "" {
		return CallbackEvent{}, fmt.Errorf("%w: callback missing provider_reference", ErrCallbackMalformedBody)
	}

	var eventType CallbackEventType
	switch body.EventType {
	case string(CallbackEventDeposit):
		eventType = CallbackEventDeposit
	case string(CallbackEventDepositReversal):
		eventType = CallbackEventDepositReversal
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
	default:
		return CallbackEvent{}, fmt.Errorf("%w: unknown callback outcome %q", ErrCallbackMalformedBody, body.Outcome)
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
