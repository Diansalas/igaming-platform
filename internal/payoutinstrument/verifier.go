package payoutinstrument

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Verifier errors.
var (
	// ErrVerifierSourceRefused: a non-Synthetic verifier declared the
	// synthetic source or an unknown one (M-3).
	ErrVerifierSourceRefused = errors.New("payoutinstrument: verifier source refused")
	// ErrNoVerifier: no registered verifier supports the kind and rail.
	ErrNoVerifier = errors.New("payoutinstrument: no verifier supports this instrument")
)

// SecretDetail wraps the decrypted detail handed to a verifier. It redacts
// under every rendering path; the verifier reads it through Bytes.
type SecretDetail struct{ raw []byte }

// NewSecretDetail wraps raw (copied).
func NewSecretDetail(raw []byte) SecretDetail { return SecretDetail{raw: append([]byte(nil), raw...)} }

// Bytes returns the detail JSON for the vendor call.
func (d SecretDetail) Bytes() []byte { return d.raw }

// String never renders the detail.
func (d SecretDetail) String() string { return "SecretDetail{redacted}" }

// GoString never renders the detail.
func (d SecretDetail) GoString() string { return d.String() }

// Format redacts under every verb.
func (d SecretDetail) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(d.String())) }

// MarshalJSON never renders the detail.
func (d SecretDetail) MarshalJSON() ([]byte, error) { return []byte(`"[REDACTED]"`), nil }

// VerifyRequest is what a verifier receives.
type VerifyRequest struct {
	TenantID     uuid.UUID
	InstrumentID uuid.UUID
	Kind         string
	Rail         string
	Detail       SecretDetail
}

// VerifyResult is what a verifier returns. It never sets the source (the
// platform forces that) and never carries payer-identifying fields.
type VerifyResult struct {
	// Verified: the vendor confirmed the destination exists and is usable.
	Verified bool
	// AccountHolderMatchesVerifiedIdentity: the vendor asserts the account
	// holder equals the Person's verified identity (A-5). Required for a
	// non-synthetic verified outcome.
	AccountHolderMatchesVerifiedIdentity bool
	// SourceExpiry is the vendor's own expiry; the platform takes
	// min(SourceExpiry, jurisdiction max age).
	SourceExpiry time.Time
	// ReferenceHash is an optional SHA-256 hex of the vendor's reference.
	ReferenceHash string
}

// PayoutInstrumentVerifier is the provider-neutral ownership-verification
// interface (ADR 0111 2.5). Source is FORCED by this package from the
// verifier's providerkind.Synthetic marker: a Synthetic verifier can only
// produce `synthetic`; a non-Synthetic verifier declares one of the three
// non-synthetic sources via DeclaredSource and is refused if it declares
// `synthetic`.
type PayoutInstrumentVerifier interface {
	// ID is the verifier_provider_id recorded on verifications.
	ID() string
	// DeclaredSource is consulted ONLY for a non-Synthetic verifier.
	DeclaredSource() VerificationSource
	// Supports reports whether the verifier can verify the kind on the rail.
	Supports(kind, rail string) bool
	// Verify performs the check. A Synthetic verifier performs no external I/O.
	Verify(ctx context.Context, req VerifyRequest) (VerifyResult, error)
}

// ForcedSource derives the verification source from the verifier's type
// marker (M-3).
func ForcedSource(v PayoutInstrumentVerifier) (VerificationSource, error) {
	if IsSyntheticComponent(v) {
		return SourceSynthetic, nil
	}
	s := v.DeclaredSource()
	if !s.Valid() || s.IsSynthetic() {
		return "", ErrVerifierSourceRefused
	}
	return s, nil
}

// ---- MOCK verifier ----------------------------------------------------------

// MockVerifierID is the verifier_provider_id of the MOCK verifier.
const MockVerifierID = "mock-payout-verifier"

// MockVerifier is the MOCK PayoutInstrumentVerifier (CLAUDE.md: labelled MOCK).
// It is Synthetic: no external I/O, never wraps a real verifier (pinned by a
// static test), and it is registered with RefuseSyntheticInProduction. It
// verifies every instrument except a synthetic_test one whose label is
// "reject" (a deterministic rejection for tests).
type MockVerifier struct {
	// Now is injectable for tests; nil means time.Now.
	Now func() time.Time
}

// NewMockVerifier returns the MOCK verifier.
func NewMockVerifier() *MockVerifier { return &MockVerifier{} }

// SyntheticComponent implements providerkind.Synthetic.
func (*MockVerifier) SyntheticComponent() {}

// ID implements PayoutInstrumentVerifier.
func (*MockVerifier) ID() string { return MockVerifierID }

// DeclaredSource is ignored for a Synthetic verifier.
func (*MockVerifier) DeclaredSource() VerificationSource { return SourceSynthetic }

// Supports implements PayoutInstrumentVerifier: every kind and rail.
func (*MockVerifier) Supports(string, string) bool { return true }

// Verify implements PayoutInstrumentVerifier.
func (m *MockVerifier) Verify(_ context.Context, req VerifyRequest) (VerifyResult, error) {
	now := time.Now
	if m.Now != nil {
		now = m.Now
	}
	if req.Kind == KindSyntheticTest {
		var d struct {
			Label string `json:"label"`
		}
		if err := json.Unmarshal(req.Detail.Bytes(), &d); err == nil && d.Label == "reject" {
			return VerifyResult{Verified: false, SourceExpiry: now().Add(24 * time.Hour)}, nil
		}
	}
	return VerifyResult{Verified: true, AccountHolderMatchesVerifiedIdentity: true, SourceExpiry: now().Add(90 * 24 * time.Hour)}, nil
}
