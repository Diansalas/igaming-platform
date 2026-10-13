package staffmfa

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/staffmfa/totp"
)

// Redacted is what every secret-bearing type of this package renders as,
// under fmt (every verb, including %#v), slog and encoding/json.
const Redacted = "[REDACTED]"

// Each secret type below defines String, GoString, Format, LogValue and
// MarshalJSON with a value receiver, so values and pointers both redact.

// ---- FactorSecret -----------------------------------------------------------

// FactorSecret is a plaintext factor secret (the 20-byte TOTP key). It exists
// only in memory between Open and verification, or between generation and
// sealing.
type FactorSecret struct{ f func() factorSecretData }

// factorSecretData is reachable only through an unexported closure, so a
// FactorSecret held in an UNEXPORTED field of another struct (where fmt and slog
// cannot call its redacting methods) renders as a function address under every
// verb, never as bytes. A plain pointer is not enough: fmt's bad-verb path
// (%s, %d, %x on a pointer) prints the pointee (security review C2).
type factorSecretData struct{ b []byte }

func newFactorSecret(b []byte) FactorSecret {
	v := factorSecretData{b: b}
	return FactorSecret{f: func() factorSecretData { return v }}
}

// d returns the material; the zero FactorSecret has none.
func (s FactorSecret) d() factorSecretData {
	if s.f == nil {
		return factorSecretData{}
	}
	return s.f()
}

// String redacts.
func (FactorSecret) String() string { return Redacted }

// GoString redacts.
func (FactorSecret) GoString() string { return Redacted }

// Format redacts under every verb.
func (FactorSecret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(Redacted)) }

// LogValue redacts.
func (FactorSecret) LogValue() slog.Value { return slog.StringValue(Redacted) }

// MarshalJSON redacts.
func (FactorSecret) MarshalJSON() ([]byte, error) { return json.Marshal(Redacted) }

// generateTOTPSecret reads totp.SecretBytes bytes from r.
func generateTOTPSecret(r io.Reader) (FactorSecret, error) {
	b := make([]byte, totp.SecretBytes)
	if _, err := io.ReadFull(r, b); err != nil {
		return FactorSecret{}, fmt.Errorf("staffmfa: generate secret: %w", err)
	}
	return newFactorSecret(b), nil
}

// GenerateTOTPSecret returns a fresh 160-bit TOTP secret from crypto/rand.
func GenerateTOTPSecret() (FactorSecret, error) { return generateTOTPSecret(rand.Reader) }

// secretLen is the plaintext length of a factor type's secret.
func secretLen(t FactorType) int {
	if t == FactorTOTP {
		return totp.SecretBytes
	}
	return -1
}

// base32NoPad is RFC 4648 base32 without padding, the otpauth secret format.
var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

// ---- Assertion --------------------------------------------------------------

// AssertionKind is the closed set of second-factor assertion kinds.
type AssertionKind string

// Assertion kinds.
const (
	AssertionTOTP         AssertionKind = "totp"
	AssertionRecoveryCode AssertionKind = "recovery_code"
)

// Assertion is a presented second-factor value (a TOTP code or a recovery
// code). It is a request value only and redacts everywhere.
type Assertion struct{ f func() assertionData }

// assertionData is behind a closure for the same reason as factorSecretData.
type assertionData struct {
	kind  AssertionKind
	value string
}

func (a Assertion) d() assertionData {
	if a.f == nil {
		return assertionData{}
	}
	return a.f()
}

// NewTOTPAssertion wraps a presented TOTP code. It does not validate: a
// malformed code fails closed at verification and counts as a failure.
func NewTOTPAssertion(code string) Assertion {
	return newAssertion(assertionData{kind: AssertionTOTP, value: code})
}

// NewRecoveryCodeAssertion wraps a presented recovery code.
func NewRecoveryCodeAssertion(code string) Assertion {
	return newAssertion(assertionData{kind: AssertionRecoveryCode, value: code})
}

// Kind returns the assertion kind (not secret).
func (a Assertion) Kind() AssertionKind { return a.d().kind }

// String redacts.
func (Assertion) String() string { return Redacted }

// GoString redacts.
func (Assertion) GoString() string { return Redacted }

// Format redacts under every verb.
func (Assertion) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(Redacted)) }

// LogValue redacts.
func (Assertion) LogValue() slog.Value { return slog.StringValue(Redacted) }

// MarshalJSON redacts.
func (Assertion) MarshalJSON() ([]byte, error) { return json.Marshal(Redacted) }

// ---- EnrolmentMaterial ------------------------------------------------------

// EnrolmentMaterial is what a staff member needs to enrol a factor, returned
// exactly once: for TOTP, the otpauth URI and the base32 secret. It redacts
// everywhere; the one exported read is RevealForEnrolment, for the single
// no-store enrolment response (and tests). Callers of RevealFor* must be
// allow-listed by a static test (S3 entry gate G5).
type EnrolmentMaterial struct{ f func() enrolmentData }

// enrolmentData is behind a closure for the same reason as factorSecretData.
type enrolmentData struct {
	uri    string
	base32 string
}

func (m EnrolmentMaterial) d() enrolmentData {
	if m.f == nil {
		return enrolmentData{}
	}
	return m.f()
}

// NewTOTPEnrolmentMaterial builds the enrolment material for secret, with the
// otpauth label issuer:account (ADR 0119 section 4.3).
func NewTOTPEnrolmentMaterial(issuer, account string, secret FactorSecret) (EnrolmentMaterial, error) {
	if len(secret.d().b) != totp.SecretBytes {
		return EnrolmentMaterial{}, ErrInvalidInput
	}
	uri, err := otpauthURI(issuer, account, secret)
	if err != nil {
		return EnrolmentMaterial{}, err
	}
	return newEnrolmentMaterial(enrolmentData{uri: uri, base32: base32NoPad.EncodeToString(secret.d().b)}), nil
}

func (m EnrolmentMaterial) otpauthURI() string   { return m.d().uri }
func (m EnrolmentMaterial) secretBase32() string { return m.d().base32 }

// RevealForEnrolment returns the otpauth URI and the base32 secret, for the
// single no-store enrolment response (ADR 0119 section 6.2), exactly as
// RecoveryCode.RevealForDisplay and Token.RevealForDelivery do for their
// types. The S1 design left the raw read to an accessor that no other package
// could call; the store layer (S2) and the enrolment handler (S3) both need
// this one. Never log, audit or cache the result.
func (m EnrolmentMaterial) RevealForEnrolment() (otpauthURI, secretBase32 string) {
	return m.d().uri, m.d().base32
}

// String redacts.
func (EnrolmentMaterial) String() string { return Redacted }

// GoString redacts.
func (EnrolmentMaterial) GoString() string { return Redacted }

// Format redacts under every verb.
func (EnrolmentMaterial) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(Redacted)) }

// LogValue redacts.
func (EnrolmentMaterial) LogValue() slog.Value { return slog.StringValue(Redacted) }

// MarshalJSON redacts.
func (EnrolmentMaterial) MarshalJSON() ([]byte, error) { return json.Marshal(Redacted) }

// ---- Token (login/enrolment challenges, enrolment tokens) -------------------

// tokenSecretBytes is the random part of a token (ADR 0119 section 6.3).
const tokenSecretBytes = 32

// TokenLen is the length of an encoded token: base64url without padding of
// 16 + tokenSecretBytes bytes (pinned by a test against EncodedLen).
const TokenLen = 64

// Token is a challenge or enrolment token. Its wire form is
// base64url(staff_user_id[16] || random[32]), 64 characters, no padding.
//
// The staff id travels inside the token so the keyed hash can bind
// staff_user_id (ADR 0119 section 5.3) while still allowing a lookup by MAC
// before the staff member is known (section 13.1 item 6). The id is not
// secret from the token holder, who is that staff member.
type Token struct{ f func() tokenData }

// tokenData is behind a closure for the same reason as factorSecretData.
type tokenData struct {
	staff  uuid.UUID
	secret []byte
}

func (t Token) d() tokenData {
	if t.f == nil {
		return tokenData{}
	}
	return t.f()
}

// NewToken returns a fresh token for staff.
func NewToken(staff uuid.UUID) (Token, error) { return newToken(rand.Reader, staff) }

func newToken(r io.Reader, staff uuid.UUID) (Token, error) {
	if staff == uuid.Nil {
		return Token{}, ErrInvalidInput
	}
	b := make([]byte, tokenSecretBytes)
	if _, err := io.ReadFull(r, b); err != nil {
		return Token{}, fmt.Errorf("staffmfa: generate token: %w", err)
	}
	return newTokenValue(tokenData{staff: staff, secret: b}), nil
}

// ParseToken parses a presented token. Anything but the exact wire form fails
// closed with ErrInvalidInput, which never carries the value.
func ParseToken(s string) (Token, error) {
	if len(s) != TokenLen {
		return Token{}, ErrInvalidInput
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || len(raw) != 16+tokenSecretBytes {
		return Token{}, ErrInvalidInput
	}
	staff, err := uuid.FromBytes(raw[:16])
	if err != nil || staff == uuid.Nil {
		return Token{}, ErrInvalidInput
	}
	return newTokenValue(tokenData{staff: staff, secret: raw[16:]}), nil
}

// StaffID is the staff member the token was issued to. A row found by the
// token's keyed hash must carry the same staff_user_id.
func (t Token) StaffID() uuid.UUID { return t.d().staff }

// RevealForDelivery returns the wire form, for the single place that delivers
// it (the email body, or the one HTTP response that returns a challenge). It
// must never reach a log, an audit record or an error.
func (t Token) RevealForDelivery() string {
	td := t.d()
	if td.staff == uuid.Nil || len(td.secret) != tokenSecretBytes {
		return ""
	}
	raw := make([]byte, 0, 16+tokenSecretBytes)
	raw = append(raw, td.staff[:]...)
	raw = append(raw, td.secret...)
	return base64.RawURLEncoding.EncodeToString(raw)
}

// String redacts.
func (Token) String() string { return Redacted }

// GoString redacts.
func (Token) GoString() string { return Redacted }

// Format redacts under every verb.
func (Token) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(Redacted)) }

// LogValue redacts.
func (Token) LogValue() slog.Value { return slog.StringValue(Redacted) }

// MarshalJSON redacts.
func (Token) MarshalJSON() ([]byte, error) { return json.Marshal(Redacted) }

func tokenKindValid(k MACKind) bool {
	switch k {
	case MACChallengeMFA, MACChallengeEnrol, MACEnrolInvite, MACEnrolMFAReset, MACEnrolPasswordReset, MACEnrolBootstrap:
		return true
	}
	return false
}

// HashToken returns the keyed hash of t under the active MAC kid, for the
// INSERT of the challenge or enrolment-token row (kind selects the purpose).
func (k *Keys) HashToken(kind MACKind, t Token) (KeyedHash, error) {
	if !tokenKindValid(kind) || t.d().staff == uuid.Nil || len(t.d().secret) != tokenSecretBytes {
		return KeyedHash{}, ErrInvalidInput
	}
	m, err := k.macUnder(k.m().macActive, kind, t.d().staff, t.d().secret)
	if err != nil {
		return KeyedHash{}, err
	}
	return KeyedHash{KID: k.m().macActive, MAC: m}, nil
}

// TokenLookupHashes returns the keyed hash of t under every retained MAC kid,
// for the lookup by MAC.
func (k *Keys) TokenLookupHashes(kind MACKind, t Token) ([]KeyedHash, error) {
	if !tokenKindValid(kind) || t.d().staff == uuid.Nil || len(t.d().secret) != tokenSecretBytes {
		return nil, ErrInvalidInput
	}
	return k.lookupHashes(kind, t.d().staff, t.d().secret)
}

// VerifyTokenHash confirms that a row found by lookup belongs to t: the row's
// staff_user_id equals the token's staff id, and the stored MAC under the
// row's kid equals the MAC of t (constant time). An unknown (retired) kid is
// a mismatch.
func (k *Keys) VerifyTokenHash(kind MACKind, t Token, rowStaff uuid.UUID, stored KeyedHash) bool {
	if !tokenKindValid(kind) || t.d().staff == uuid.Nil || len(t.d().secret) != tokenSecretBytes {
		return false
	}
	sameStaff := rowStaff == t.d().staff
	macOK := k.verifyUnder(stored.KID, kind, t.d().staff, t.d().secret, stored.MAC)
	return sameStaff && macOK
}

func newAssertion(v assertionData) Assertion { return Assertion{f: func() assertionData { return v }} }

func newEnrolmentMaterial(v enrolmentData) EnrolmentMaterial {
	return EnrolmentMaterial{f: func() enrolmentData { return v }}
}

func newTokenValue(v tokenData) Token { return Token{f: func() tokenData { return v }} }
