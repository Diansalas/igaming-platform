package kyc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
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
// authenticated via the shared internal/webhookauth MOCK scheme (Stage
// 10.2, ADR 0091) - standing in for whatever signature/shared-secret
// scheme a real vendor would use. This is NOT a real vendor's webhook
// format; it exists solely so this stage can test "callback
// authentication" end to end without inventing and then having to
// maintain a fictional but vendor-shaped API.
//
// MOCK ONLY (CLAUDE.md "No fake completion"): master is a per-process
// crypto/rand secret, NEVER config, NEVER the repository, and NEVER
// recoverable from outside this process (webhookauth.NewMockMaster). A
// caller CANNOT construct this type with an injected/literal secret -
// NewMockKYCProvider takes no argument - which is what actually closes
// KYC-WH-1's "committed constant" finding: there is no parameter through
// which a compile-time literal could ever reach this type again.
type MockKYCProvider struct {
	mu          sync.Mutex
	master      []byte
	outcomes    map[string]ProviderResult // providerReference -> configured GetVerification/callback result
	created     map[string]bool
	unavailable bool
}

// kycMockScheme is the KYC domain's platform-defined MOCK wire scheme
// (webhookauth package doc: never a vendor format).
var kycMockScheme = webhookauth.KYCScheme()

// NewMockKYCProvider returns a mock whose webhook signing/verification key
// is a fresh per-process crypto/rand master (webhookauth.NewMockMaster) -
// structurally impossible to inject a literal, since this constructor
// takes no argument (KYC-WH-1, Stage 10.2, ADR 0091).
func NewMockKYCProvider() *MockKYCProvider {
	return &MockKYCProvider{
		master:   webhookauth.NewMockMaster(),
		outcomes: make(map[string]ProviderResult),
		created:  make(map[string]bool),
	}
}

func (m *MockKYCProvider) ID() string { return "mock" }

// SyntheticComponent implements providerkind.Synthetic (Stage 10.3,
// MOCK-ADAPTER-PROD-1) - a structural marker only, satisfied without this
// package importing internal/providerkind.
func (m *MockKYCProvider) SyntheticComponent() {}

// SetOutcome configures GetVerification and CallbackPayload to
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
	// of its customers - kyc_verifications' own UNIQUE(tenant_id,
	// provider_id, provider_reference) index (migration 0040) assumes
	// exactly that. Generating one from a real random UUID (rather than a
	// simple per-instance sequential counter) is what actually keeps that
	// promise true across multiple independent MockKYCProvider instances
	// sharing one database (e.g. two different test runs).
	ref := "mock-ref-" + uuid.NewString()
	m.created[ref] = true
	reason, _ := NormalizeReason("created")
	return ProviderResult{ProviderReference: ref, Outcome: ProviderPending, Reason: reason}, nil
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
	reason, _ := NormalizeReason("awaiting_submission")
	return ProviderResult{ProviderReference: providerReference, Outcome: ProviderPending, Reason: reason}, nil
}

func (m *MockKYCProvider) SubmitVerification(ctx context.Context, providerReference string, documents []SubmittedDocument, call CallContext) (ProviderResult, error) {
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
		reason, _ := NormalizeReason("no_documents_submitted")
		return ProviderResult{ProviderReference: providerReference, Outcome: ProviderReviewRequired, Reason: reason}, nil
	}
	// Default, honest behavior with no test configuration: a real vendor
	// would actually inspect the documents; this mock has none to
	// inspect, so it reports review_required rather than fabricating an
	// approval.
	reason, _ := NormalizeReason("manual_review_default")
	return ProviderResult{ProviderReference: providerReference, Outcome: ProviderReviewRequired, Reason: reason}, nil
}

// mockCallbackPayload is the platform's OWN minimal callback shape (see
// this type's own doc comment) - never a real vendor's format.
type mockCallbackPayload struct {
	ProviderReference string `json:"provider_reference"`
	Outcome           string `json:"outcome"`
	Reason            string `json:"reason"`
}

// closedOutcomeEnum is the closed set B6(d) requires: any other "outcome"
// value is a malformed body, not a silently-accepted new status.
var closedOutcomeEnum = map[string]bool{
	string(ProviderApproved): true, string(ProviderRejected): true, string(ProviderPending): true,
	string(ProviderReviewRequired): true, string(ProviderExpired): true, string(ProviderError): true,
}

// deriveKey computes this instance's per-(tenantID, providerID) webhook
// signing key via the shared MOCK derivation (Stage 10.2, ADR 0091).
func (m *MockKYCProvider) deriveKey(tenantID uuid.UUID, providerID string) []byte {
	return webhookauth.DeriveMockKey(m.master, webhookauth.KYCMockKeyLabel, tenantID, providerID)
}

// CallbackPayload builds a signed webhookauth.Inbound a test/in-process
// caller can POST to the platform's webhook endpoint - the mock's own
// stand-in for whatever a real vendor's webhook request would look like.
// tenantID/providerID are bound into the signature (never trusted from
// the body) - a payload built for one tenant never verifies for another.
func (m *MockKYCProvider) CallbackPayload(tenantID uuid.UUID, providerReference string, outcome ProviderOutcome, reason string) webhookauth.Inbound {
	body, _ := json.Marshal(mockCallbackPayload{ProviderReference: providerReference, Outcome: string(outcome), Reason: reason})
	key := m.deriveKey(tenantID, m.ID())
	sigHex := kycMockScheme.Sign(key, tenantID, m.ID(), webhookauth.MockKeyID, body)
	in := webhookauth.Inbound{TenantID: tenantID, ProviderID: m.ID(), Header: http.Header{}, Body: body}
	kycMockScheme.SetHeaders(in.Header, webhookauth.MockKeyID, sigHex)
	return in
}

// HandleCallback implements KYCProvider. Verification order (design §B6
// (c)-(d), run only after the Orchestrator has already completed (a)-(b) -
// provider registered, credential resolved and equality-checked against
// req.TenantID/req.ProviderID):
//
//  1. verify the raw bytes against cred via the shared webhookauth.Scheme.
//     Verify (constant-time, over content that includes tenantID/
//     providerID - never trusted from the body). The ONLY failure value
//     is ErrCallbackSignatureInvalid.
//  2. only once (1) has succeeded: parse the body, reject a legacy
//     top-level "signature" field (the pre-Stage-10.2 wire shape moved the
//     signature into the header exclusively) as ErrCallbackSignatureInvalid,
//     then require provider_reference and a closed-enum outcome - any
//     failure here is ErrCallbackMalformedBody, a DIFFERENT, POST-
//     verification sentinel the Orchestrator/HTTP layer maps to 400, never
//     to the uniform 401.
//
// KYC-REASON-BOUND-1 (Stage 10.3): the raw payload's "reason" field is run
// through NormalizeReason before it is ever returned in ProviderResult -
// every future real adapter's own HandleCallback MUST do the same to its
// own vendor-specific status/reason text before returning (see
// ProviderResult's doc comment and RunProviderConformanceSuite's mandatory
// reason-bound case, internal/kyc/conformance_test.go).
func (m *MockKYCProvider) HandleCallback(ctx context.Context, in webhookauth.Inbound, cred webhookauth.Credential) (ProviderResult, error) {
	if err := kycMockScheme.Verify(cred, in); err != nil {
		return ProviderResult{}, ErrCallbackSignatureInvalid
	}

	// From here on the caller has proven knowledge of the resolved
	// credential - every subsequent rejection is a POST-verification,
	// structural failure (ErrCallbackMalformedBody -> 400), with one
	// deliberate carve-out: a body still carrying the legacy "signature"
	// field is rejected as ErrCallbackSignatureInvalid (401,
	// signature_invalid), matching payments and casino (design §B6(d),
	// ADR 0022 §3 Stage 10.2 amendment).
	var generic map[string]any
	if err := json.Unmarshal(in.Body, &generic); err != nil {
		return ProviderResult{}, fmt.Errorf("%w: parse callback: %v", ErrCallbackMalformedBody, err)
	}
	if _, present := generic["signature"]; present {
		return ProviderResult{}, ErrCallbackSignatureInvalid
	}

	var payload mockCallbackPayload
	if err := json.Unmarshal(in.Body, &payload); err != nil {
		return ProviderResult{}, fmt.Errorf("%w: parse callback: %v", ErrCallbackMalformedBody, err)
	}
	if payload.ProviderReference == "" {
		return ProviderResult{}, fmt.Errorf("%w: callback missing provider_reference", ErrCallbackMalformedBody)
	}
	if !closedOutcomeEnum[payload.Outcome] {
		return ProviderResult{}, fmt.Errorf("%w: unrecognized outcome %q", ErrCallbackMalformedBody, payload.Outcome)
	}
	normalizedReason, truncated := NormalizeReason(payload.Reason)
	return ProviderResult{ProviderReference: payload.ProviderReference, Outcome: ProviderOutcome(payload.Outcome), Reason: normalizedReason, ReasonTruncated: truncated}, nil
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

// NewMockWebhookCredentials returns the MOCK webhookauth.Resolver for the
// KYC domain (design §B2, Stage 10.2 final review K11): the shared
// webhookauth.MockResolver bound to p's own master/label/provider id,
// directly - not a KYC-local wrapper type around it (the earlier
// MockWebhookCredentials duplicated MockResolver's exact Resolve logic for
// no reason). It resolves ONLY KeyID=="mock-v1" for provider "mock" - any
// other providerID or keyID is webhookauth.ErrCredentialUnavailable, which
// the Orchestrator folds into ReasonCredentialUnavailable (fail closed,
// never a fallback to unauthenticated verification). Every caller of this
// constructor already guards against a nil p before calling it (see
// cmd/platform-api/wiring.go's kycWebhookResolver).
//
// MOCK ONLY: the real resolver (a FORCE-RLS handle table plus an external,
// tenant-scoped secret store) is NOT IMPLEMENTED.
func NewMockWebhookCredentials(p *MockKYCProvider) webhookauth.MockResolver {
	return webhookauth.MockResolver{
		Master:     p.master,
		Label:      webhookauth.KYCMockKeyLabel,
		ProviderID: p.ID(),
	}
}
