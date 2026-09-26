package webhookauth

// ============================================================================
// MOCK. The platform-defined MOCK wire Scheme adapted to VerificationScheme
// (Stage 10.3 W1a, WH-VENDOR-SCHEME-1). It is NOT a real-provider protocol
// and must never be offered to a vendor (package doc; proposal §22). Its
// bytes - prefixes, header names, "v1=<64 lowercase hex>" format, key-id
// charset, HMAC-SHA256 over Prefix‖0x00‖tenant‖0x00‖provider‖0x00‖key‖0x00‖
// body - are exactly Scheme's, which is exactly Stage 10.1/10.2's; this
// file only exposes them through the per-adapter interface.
// ============================================================================

import (
	"time"
)

// VerificationScheme returns the MOCK scheme s adapted to the per-adapter
// VerificationScheme interface. Only this package can construct it, and it
// is the only implementation NewSchemeSet accepts with Synthetic=true.
func (s Scheme) VerificationScheme() VerificationScheme {
	return mockVerificationScheme{s: s}
}

// mockVerificationScheme is comparable (Scheme is three strings), which the
// Synthetic type check in ValidateScheme relies on.
type mockVerificationScheme struct{ s Scheme }

// Name is "platform-mock:<signing prefix>", e.g.
// "platform-mock:igaming.payments.webhook.v1".
func (m mockVerificationScheme) Name() string {
	return "platform-mock:" + m.s.Prefix
}

// Extract is Scheme.ParseHeaders: signature_missing for an absent header
// (including an absent key id - the MOCK is KeyFromHeader), signature_invalid
// for a malformed one. The signature hex is carried as private material.
func (m mockVerificationScheme) Extract(in Inbound) (AuthMaterial, Reason, bool) {
	keyID, sigHex, reason, ok := m.s.ParseHeaders(in.Header)
	if !ok {
		return AuthMaterial{}, reason, false
	}
	return NewAuthMaterial(keyID, sigHex), "", true
}

// Verify is Scheme.Verify over creds.Active only (KeyFromHeader: a Previous
// credential is never tried), plus the MinSecretBytes floor. The MOCK signs
// no timestamp, so now is unused (Properties discloses this; the MOCK is
// exempt from SC7 only because it is synthetic - see Properties).
func (m mockVerificationScheme) Verify(creds CredentialSet, in Inbound, am AuthMaterial, _ time.Time) (string, error) {
	cred := creds.Active
	if len(cred.Secret) < MinSecretBytes || am.KeyID == "" || am.KeyID != cred.KeyID {
		return "", ErrSignatureInvalid
	}
	if err := m.s.Verify(cred, in); err != nil {
		return "", ErrSignatureInvalid
	}
	return cred.KeyID, nil
}

// Properties discloses the MOCK's weaker-than-real declaration: the
// platform tenant id is signed (SignedTenant), the key id comes from a
// header, NO timestamp is signed, and replay is inert only because every
// callback effect is idempotent. Permitted only because it is Synthetic -
// ValidateProperties refuses this declaration from any real scheme (ADR
// 0022 §3 point 10), and the synthetic-component production guard (W1b,
// MOCK-ADAPTER-PROD-1) keeps every MOCK adapter out of production.
func (m mockVerificationScheme) Properties() SchemeProperties {
	return SchemeProperties{
		Binding:         BindingSignedTenant,
		KeySelection:    KeyFromHeader,
		SignedTimestamp: false,
		MaxSkew:         0,
		Replay:          ReplayIdempotencyOnly,
		Synthetic:       true,
	}
}

// MockScheme returns the underlying MOCK wire Scheme of a scheme built by
// Scheme.VerificationScheme (ok=false for any other implementation). The
// conformance fixtures use it to sign MOCK callbacks.
func MockScheme(v VerificationScheme) (Scheme, bool) {
	m, ok := v.(mockVerificationScheme)
	return m.s, ok
}
