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
type FactorSecret struct{ b []byte }

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
	return FactorSecret{b: b}, nil
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
type Assertion struct {
	kind  AssertionKind
	value string
}

// NewTOTPAssertion wraps a presented TOTP code. It does not validate: a
// malformed code fails closed at verification and counts as a failure.
func NewTOTPAssertion(code string) Assertion { return Assertion{kind: AssertionTOTP, value: code} }

// NewRecoveryCodeAssertion wraps a presented recovery code.
func NewRecoveryCodeAssertion(code string) Assertion {
	return Assertion{kind: AssertionRecoveryCode, value: code}
}

// Kind returns the assertion kind (not secret).
func (a Assertion) Kind() AssertionKind { return a.kind }

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
// everywhere; only this package's (future, S3) enrolment response encoder
// reads it, through the unexported accessors.
type EnrolmentMaterial struct {
	uri    string
	base32 string
}

// NewTOTPEnrolmentMaterial builds the enrolment material for secret, with the
// otpauth label issuer:account (ADR 0119 section 4.3).
func NewTOTPEnrolmentMaterial(issuer, account string, secret FactorSecret) (EnrolmentMaterial, error) {
	if len(secret.b) != totp.SecretBytes {
		return EnrolmentMaterial{}, ErrInvalidInput
	}
	uri, err := otpauthURI(issuer, account, secret)
	if err != nil {
		return EnrolmentMaterial{}, err
	}
	return EnrolmentMaterial{uri: uri, base32: base32NoPad.EncodeToString(secret.b)}, nil
}

func (m EnrolmentMaterial) otpauthURI() string   { return m.uri }
func (m EnrolmentMaterial) secretBase32() string { return m.base32 }

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

// TokenLen is the length of an encoded token.
var TokenLen = base64.RawURLEncoding.EncodedLen(16 + tokenSecretBytes)

// Token is a challenge or enrolment token. Its wire form is
// base64url(staff_user_id[16] || random[32]), 64 characters, no padding.
//
// The staff id travels inside the token so the keyed hash can bind
// staff_user_id (ADR 0119 section 5.3) while still allowing a lookup by MAC
// before the staff member is known (section 13.1 item 6). The id is not
// secret from the token holder, who is that staff member.
type Token struct {
	staff  uuid.UUID
	secret []byte
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
	return Token{staff: staff, secret: b}, nil
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
	return Token{staff: staff, secret: raw[16:]}, nil
}

// StaffID is the staff member the token was issued to. A row found by the
// token's keyed hash must carry the same staff_user_id.
func (t Token) StaffID() uuid.UUID { return t.staff }

// RevealForDelivery returns the wire form, for the single place that delivers
// it (the email body, or the one HTTP response that returns a challenge). It
// must never reach a log, an audit record or an error.
func (t Token) RevealForDelivery() string {
	if t.staff == uuid.Nil || len(t.secret) != tokenSecretBytes {
		return ""
	}
	raw := make([]byte, 0, 16+tokenSecretBytes)
	raw = append(raw, t.staff[:]...)
	raw = append(raw, t.secret...)
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
	if !tokenKindValid(kind) || t.staff == uuid.Nil || len(t.secret) != tokenSecretBytes {
		return KeyedHash{}, ErrInvalidInput
	}
	m, err := k.macUnder(k.macActive, kind, t.staff, t.secret)
	if err != nil {
		return KeyedHash{}, err
	}
	return KeyedHash{KID: k.macActive, MAC: m}, nil
}

// TokenLookupHashes returns the keyed hash of t under every retained MAC kid,
// for the lookup by MAC.
func (k *Keys) TokenLookupHashes(kind MACKind, t Token) ([]KeyedHash, error) {
	if !tokenKindValid(kind) || t.staff == uuid.Nil || len(t.secret) != tokenSecretBytes {
		return nil, ErrInvalidInput
	}
	return k.lookupHashes(kind, t.staff, t.secret)
}

// VerifyTokenHash confirms that a row found by lookup belongs to t: the row's
// staff_user_id equals the token's staff id, and the stored MAC under the
// row's kid equals the MAC of t (constant time). An unknown (retired) kid is
// a mismatch.
func (k *Keys) VerifyTokenHash(kind MACKind, t Token, rowStaff uuid.UUID, stored KeyedHash) bool {
	if !tokenKindValid(kind) || t.staff == uuid.Nil || len(t.secret) != tokenSecretBytes {
		return false
	}
	sameStaff := rowStaff == t.staff
	macOK := k.verifyUnder(stored.KID, kind, t.staff, t.secret, stored.MAC)
	return sameStaff && macOK
}
