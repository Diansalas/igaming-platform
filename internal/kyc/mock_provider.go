package kyc

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/google/uuid"
)

// MockKYCProvider is the only KYCProvider implementation this stage
// ships - there is no real vendor contracted (directive §1: "do not
// select a vendor, do not integrate a real KYC provider, do not invent a
// vendor API"). Mirrors casino.MockCasinoProvider/payments.
// MockPaymentProvider's identical "magic value" testing convention: a
// test configures a specific outcome for a specific provider_reference
// via SetOutcome, never a real document-verification algorithm.
//
// Its callback payload is the platform's OWN minimal JSON shape
// ({"provider_reference": "...", "outcome": "...", "reason": "..."}),
// HMAC-signed with webhookSecret - standing in for whatever
// signature/shared-secret scheme a real vendor would use. This is NOT a
// real vendor's webhook format; it exists solely so this stage can test
// "callback authentication" end to end (directive §21/§23) without
// inventing and then having to maintain a fictional but
// vendor-shaped API.
type MockKYCProvider struct {
	mu            sync.Mutex
	webhookSecret string
	outcomes      map[string]ProviderResult // providerReference -> configured GetVerification/callback result
	created       map[string]bool
	unavailable   bool
}

// NewMockKYCProvider returns a mock with a fixed webhookSecret used to
// HMAC-sign/verify callback payloads (MockCallbackPayload/HandleCallback).
func NewMockKYCProvider(webhookSecret string) *MockKYCProvider {
	return &MockKYCProvider{
		webhookSecret: webhookSecret,
		outcomes:      make(map[string]ProviderResult),
		created:       make(map[string]bool),
	}
}

func (m *MockKYCProvider) ID() string { return "mock" }

// SetOutcome configures GetVerification and MockCallbackPayload to
// report result for providerReference - a test-only configuration hook,
// mirroring MockCasinoProvider's SetGameConfig/MockPersonResolver's
// SetMatch conventions exactly.
func (m *MockKYCProvider) SetOutcome(providerReference string, result ProviderResult) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result.ProviderReference = providerReference
	m.outcomes[providerReference] = result
}

// SetUnavailable makes every subsequent call fail, simulating a vendor
// outage (mirrors identityresolution.MockPersonResolver.SetUnavailable).
func (m *MockKYCProvider) SetUnavailable(unavailable bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.unavailable = unavailable
}

func (m *MockKYCProvider) CreateVerification(ctx context.Context, input CreateVerificationInput) (ProviderResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unavailable {
		return ProviderResult{}, fmt.Errorf("kyc: mock provider unavailable")
	}
	// A real vendor's own reference is globally unique across every one
	// of its customers - kyc_verifications' own UNIQUE(provider_id,
	// provider_reference) index (migration 0040) assumes exactly that.
	// Generating one from a real random UUID (rather than a simple
	// per-instance sequential counter) is what actually keeps that
	// promise true across multiple independent MockKYCProvider instances
	// sharing one database (e.g. two different test runs) - a sequential
	// counter reset to 0 in each instance would collide (adversarial
	// testing specialist review finding, Stage 4F, caught by
	// TestKYC_ReuploadCreatesNewVersion failing with a genuine unique-
	// constraint violation across test runs).
	ref := "mock-ref-" + uuid.NewString()
	m.created[ref] = true
	return ProviderResult{ProviderReference: ref, Outcome: ProviderPending, Reason: "created"}, nil
}

func (m *MockKYCProvider) GetVerification(ctx context.Context, providerReference string) (ProviderResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unavailable {
		return ProviderResult{}, fmt.Errorf("kyc: mock provider unavailable")
	}
	if r, ok := m.outcomes[providerReference]; ok {
		return r, nil
	}
	if !m.created[providerReference] {
		return ProviderResult{}, fmt.Errorf("kyc: unknown provider reference %q", providerReference)
	}
	return ProviderResult{ProviderReference: providerReference, Outcome: ProviderPending, Reason: "awaiting_submission"}, nil
}

func (m *MockKYCProvider) SubmitVerification(ctx context.Context, providerReference string, documents []SubmittedDocument) (ProviderResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unavailable {
		return ProviderResult{}, fmt.Errorf("kyc: mock provider unavailable")
	}
	if !m.created[providerReference] {
		return ProviderResult{}, fmt.Errorf("kyc: unknown provider reference %q", providerReference)
	}
	if r, ok := m.outcomes[providerReference]; ok {
		return r, nil
	}
	if len(documents) == 0 {
		return ProviderResult{ProviderReference: providerReference, Outcome: ProviderReviewRequired, Reason: "no_documents_submitted"}, nil
	}
	// Default, honest behavior with no test configuration: a real vendor
	// would actually inspect the documents; this mock has none to
	// inspect, so it reports review_required rather than fabricating an
	// approval - mirrors identityresolution.MockPersonResolver's own
	// "cannot decide, so report the safe uncertain outcome" default.
	return ProviderResult{ProviderReference: providerReference, Outcome: ProviderReviewRequired, Reason: "manual_review_default"}, nil
}

// mockCallbackPayload is the platform's OWN minimal callback shape (see
// this type's own doc comment) - never a real vendor's format.
type mockCallbackPayload struct {
	ProviderReference string `json:"provider_reference"`
	Outcome           string `json:"outcome"`
	Reason            string `json:"reason"`
}

func (m *MockKYCProvider) sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte(m.webhookSecret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// MockCallbackPayload builds a signed callback body a test can POST to
// the platform's webhook endpoint - the mock's own stand-in for
// whatever a real vendor's webhook request would look like.
func (m *MockKYCProvider) MockCallbackPayload(providerReference string, outcome ProviderOutcome, reason string) (body []byte, signatureHeader string) {
	body, _ = json.Marshal(mockCallbackPayload{ProviderReference: providerReference, Outcome: string(outcome), Reason: reason})
	return body, m.sign(body)
}

// HandleCallback verifies the payload's signature (passed via a
// convention this mock defines for itself: the LAST line of rawPayload,
// after a newline, is the hex HMAC signature of everything before it -
// see MockSignedCallbackBody) before trusting anything else in it -
// mirrors casino.MockCasinoProvider.HandleCallback's identical
// "authenticate before parsing" discipline (ADR 0025 §7).
func (m *MockKYCProvider) HandleCallback(ctx context.Context, rawPayload []byte) (ProviderResult, error) {
	body, signature, ok := splitSignedPayload(rawPayload)
	if !ok {
		return ProviderResult{}, fmt.Errorf("kyc: mock callback missing signature")
	}
	if !hmac.Equal([]byte(m.sign(body)), []byte(signature)) {
		return ProviderResult{}, fmt.Errorf("kyc: mock callback signature invalid")
	}

	var payload mockCallbackPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return ProviderResult{}, fmt.Errorf("kyc: mock callback malformed payload")
	}
	if payload.ProviderReference == "" {
		return ProviderResult{}, fmt.Errorf("kyc: mock callback missing provider_reference")
	}
	return ProviderResult{ProviderReference: payload.ProviderReference, Outcome: ProviderOutcome(payload.Outcome), Reason: payload.Reason}, nil
}

func (m *MockKYCProvider) GetCapabilities() Capabilities {
	return Capabilities{
		SupportedDocumentTypes: []DocumentType{DocumentPassport, DocumentNationalID, DocumentDriversLicense, DocumentProofOfAddress, DocumentSelfie},
		SupportsCallback:       true,
	}
}

func (m *MockKYCProvider) HealthStatus(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.unavailable {
		return fmt.Errorf("kyc: mock provider unavailable")
	}
	return nil
}

// MockSignedCallbackBody wraps body with MockKYCProvider's own
// signature-append convention (see HandleCallback) - the exact bytes an
// HTTP test posts to the webhook endpoint.
func MockSignedCallbackBody(body []byte, signature string) []byte {
	return append(append([]byte{}, body...), append([]byte("\n"), []byte(signature)...)...)
}

func splitSignedPayload(raw []byte) (body []byte, signature string, ok bool) {
	for i := len(raw) - 1; i >= 0; i-- {
		if raw[i] == '\n' {
			return raw[:i], string(raw[i+1:]), true
		}
	}
	return nil, "", false
}
