package staffmfa

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"

	"github.com/google/uuid"
)

// Recovery-code format (ADR 0119 section 6.2): 10 codes per batch, each 10
// Crockford base32 characters (50 bits), displayed as XXXXX-XXXXX.
const (
	RecoveryCodesPerBatch = 10
	RecoveryCodeChars     = 10
	// maxRecoveryInput bounds the presented string before normalisation.
	maxRecoveryInput = 64
)

// crockford is the Crockford base32 alphabet (no I, L, O, U).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// RecoveryCode is one normalised recovery code (10 Crockford characters). It
// redacts everywhere.
type RecoveryCode struct{ v string }

// String redacts.
func (RecoveryCode) String() string { return Redacted }

// GoString redacts.
func (RecoveryCode) GoString() string { return Redacted }

// Format redacts under every verb.
func (RecoveryCode) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(Redacted)) }

// LogValue redacts.
func (RecoveryCode) LogValue() slog.Value { return slog.StringValue(Redacted) }

// MarshalJSON redacts.
func (RecoveryCode) MarshalJSON() ([]byte, error) { return json.Marshal(Redacted) }

// RevealForDisplay returns the display form XXXXX-XXXXX, for the single
// response that shows a new batch once (no-store). Never log it.
func (c RecoveryCode) RevealForDisplay() string {
	if len(c.v) != RecoveryCodeChars {
		return ""
	}
	return c.v[:5] + "-" + c.v[5:]
}

// RecoveryCodeBatch is a freshly generated batch of distinct codes.
type RecoveryCodeBatch struct{ codes []RecoveryCode }

// Codes returns the codes of the batch (a copy of the slice).
func (b RecoveryCodeBatch) Codes() []RecoveryCode { return append([]RecoveryCode(nil), b.codes...) }

// String redacts.
func (RecoveryCodeBatch) String() string { return Redacted }

// GoString redacts.
func (RecoveryCodeBatch) GoString() string { return Redacted }

// Format redacts under every verb.
func (RecoveryCodeBatch) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(Redacted)) }

// LogValue redacts.
func (RecoveryCodeBatch) LogValue() slog.Value { return slog.StringValue(Redacted) }

// MarshalJSON redacts.
func (RecoveryCodeBatch) MarshalJSON() ([]byte, error) { return json.Marshal(Redacted) }

// GenerateRecoveryCodes returns RecoveryCodesPerBatch distinct codes from
// crypto/rand.
func GenerateRecoveryCodes() (RecoveryCodeBatch, error) { return generateRecoveryCodes(rand.Reader) }

func generateRecoveryCodes(r io.Reader) (RecoveryCodeBatch, error) {
	seen := map[string]bool{}
	codes := make([]RecoveryCode, 0, RecoveryCodesPerBatch)
	// A duplicate inside a batch is a 1-in-2^50 event; the bound keeps a
	// broken reader from looping forever.
	for attempts := 0; len(codes) < RecoveryCodesPerBatch; attempts++ {
		if attempts >= 4*RecoveryCodesPerBatch {
			return RecoveryCodeBatch{}, fmt.Errorf("staffmfa: generate recovery codes: random source repeats")
		}
		var buf [RecoveryCodeChars]byte
		if _, err := io.ReadFull(r, buf[:]); err != nil {
			return RecoveryCodeBatch{}, fmt.Errorf("staffmfa: generate recovery codes: %w", err)
		}
		// 256 is a multiple of 32, so the low five bits are uniform.
		out := make([]byte, RecoveryCodeChars)
		for i, b := range buf {
			out[i] = crockford[b&0x1f]
		}
		s := string(out)
		if seen[s] {
			continue
		}
		seen[s] = true
		codes = append(codes, RecoveryCode{v: s})
	}
	return RecoveryCodeBatch{codes: codes}, nil
}

// NormaliseRecoveryCode turns a presented code into its canonical form (ADR
// 0119 section 6.2): upper case, hyphens and spaces removed, I and L read as
// 1, O read as 0. Anything else outside the Crockford alphabet, any non-ASCII
// byte, or a result that is not exactly RecoveryCodeChars long fails closed
// with ErrInvalidInput (which never carries the value).
func NormaliseRecoveryCode(s string) (RecoveryCode, error) {
	if len(s) == 0 || len(s) > maxRecoveryInput {
		return RecoveryCode{}, ErrInvalidInput
	}
	out := make([]byte, 0, RecoveryCodeChars)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '-' || c == ' ':
			continue
		case c >= 'a' && c <= 'z':
			c -= 'a' - 'A'
		case c >= 0x80:
			return RecoveryCode{}, ErrInvalidInput
		}
		switch c {
		case 'I', 'L':
			c = '1'
		case 'O':
			c = '0'
		}
		if !isCrockford(c) {
			return RecoveryCode{}, ErrInvalidInput
		}
		out = append(out, c)
		if len(out) > RecoveryCodeChars {
			return RecoveryCode{}, ErrInvalidInput
		}
	}
	if len(out) != RecoveryCodeChars {
		return RecoveryCode{}, ErrInvalidInput
	}
	return RecoveryCode{v: string(out)}, nil
}

func isCrockford(c byte) bool {
	for i := 0; i < len(crockford); i++ {
		if crockford[i] == c {
			return true
		}
	}
	return false
}

// HashRecoveryCode returns the keyed hash of code for staff under the active
// MAC kid (the code_kid and code_mac columns).
func (k *Keys) HashRecoveryCode(staff uuid.UUID, code RecoveryCode) (KeyedHash, error) {
	if len(code.v) != RecoveryCodeChars {
		return KeyedHash{}, ErrInvalidInput
	}
	m, err := k.macUnder(k.macActive, MACRecoveryCode, staff, []byte(code.v))
	if err != nil {
		return KeyedHash{}, err
	}
	return KeyedHash{KID: k.macActive, MAC: m}, nil
}

// RecoveryCodeLookupHashes returns the keyed hash of code for staff under
// every retained MAC kid, for the lookup among the staff member's unconsumed
// codes.
func (k *Keys) RecoveryCodeLookupHashes(staff uuid.UUID, code RecoveryCode) ([]KeyedHash, error) {
	if len(code.v) != RecoveryCodeChars {
		return nil, ErrInvalidInput
	}
	return k.lookupHashes(MACRecoveryCode, staff, []byte(code.v))
}

// VerifyRecoveryCodeHash recomputes the keyed hash under the stored kid and
// compares it in constant time. An unknown (retired) kid is a mismatch.
func (k *Keys) VerifyRecoveryCodeHash(staff uuid.UUID, code RecoveryCode, stored KeyedHash) bool {
	if len(code.v) != RecoveryCodeChars {
		return false
	}
	return k.verifyUnder(stored.KID, MACRecoveryCode, staff, []byte(code.v), stored.MAC)
}

// RecoveryCodeFromAssertion normalises the code carried by a recovery-code
// assertion. Any other assertion kind fails closed.
func RecoveryCodeFromAssertion(a Assertion) (RecoveryCode, error) {
	if a.kind != AssertionRecoveryCode {
		return RecoveryCode{}, ErrInvalidInput
	}
	return NormaliseRecoveryCode(a.value)
}
