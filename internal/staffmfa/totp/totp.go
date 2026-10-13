// Package totp is the in-repo RFC 6238 TOTP implementation for staff MFA
// (ADR 0119 section 4.3): HMAC-SHA1, 6 digits, a 30 second step, T0 = 0, and
// an acceptance window of the steps T-1, T and T+1. The only dependency is the
// Go standard library.
//
// Everything here is a pure function of its inputs: the caller passes the
// clock value, so tests are deterministic. Every malformed input fails closed
// with an error that never contains the secret or the code.
package totp

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // RFC 6238 default; SHA-1 is sound inside HMAC (ADR 0119 section 4.3).
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"time"
)

// Parameters fixed by ADR 0119 section 4.3. They are constants, not
// configuration: every authenticator app supports exactly these.
const (
	// Digits is the code length.
	Digits = 6
	// Period is the time step in seconds (T0 = 0).
	Period = 30
	// Skew is the number of steps accepted on either side of the current one.
	Skew = 1
	// SecretBytes is the factor secret length (160 bit, RFC 4226 section 4).
	SecretBytes = 20
)

var (
	// ErrInvalidSecret: the secret is not exactly SecretBytes long.
	ErrInvalidSecret = errors.New("totp: invalid secret")
	// ErrInvalidCode: the presented code is not exactly Digits ASCII digits.
	ErrInvalidCode = errors.New("totp: invalid code format")
	// ErrInvalidState: the clock is before the Unix epoch or the last used
	// step is negative.
	ErrInvalidState = errors.New("totp: invalid verification state")
	// ErrRejected: no candidate step matched, or every matching step is at or
	// below the last used step (replay). The two are deliberately not
	// distinguished.
	ErrRejected = errors.New("totp: code rejected")
)

// pow10 holds 10^n for the truncation modulus.
var pow10 = [...]uint32{1, 10, 100, 1000, 10000, 100000, 1000000, 10000000, 100000000}

// hotp is RFC 4226 section 5.3 with a configurable digit count (the RFC 6238
// Appendix B vectors are 8 digits; production always uses Digits).
func hotp(key []byte, counter uint64, digits int) []byte {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[off])&0x7f)<<24 | uint32(sum[off+1])<<16 | uint32(sum[off+2])<<8 | uint32(sum[off+3])
	v := bin % pow10[digits]
	out := make([]byte, digits)
	for i := digits - 1; i >= 0; i-- {
		out[i] = byte('0' + v%10)
		v /= 10
	}
	return out
}

// StepAt returns the RFC 6238 time step of t: floor(unix(t) / Period). A time
// before the Unix epoch is refused.
func StepAt(t time.Time) (int64, error) {
	u := t.Unix()
	if u < 0 {
		return 0, ErrInvalidState
	}
	return u / Period, nil
}

// CodeAtStep returns the Digits-digit code of secret for step. It is what an
// authenticator app computes; the platform itself only verifies.
func CodeAtStep(secret []byte, step int64) (string, error) {
	if len(secret) != SecretBytes {
		return "", ErrInvalidSecret
	}
	if step < 0 {
		return "", ErrInvalidState
	}
	return string(hotp(secret, uint64(step), Digits)), nil
}

// CodeAt returns the code of secret at time t.
func CodeAt(secret []byte, t time.Time) (string, error) {
	step, err := StepAt(t)
	if err != nil {
		return "", err
	}
	return CodeAtStep(secret, step)
}

// validCode reports whether code is exactly Digits ASCII digits. The length
// and the character class are not secret, so this check may exit early.
func validCode(code string) bool {
	if len(code) != Digits {
		return false
	}
	for i := 0; i < len(code); i++ {
		if code[i] < '0' || code[i] > '9' {
			return false
		}
	}
	return true
}

// Verify checks code against secret at time now, with the replay rule of ADR
// 0119 section 4.3: a candidate step s in {T-1, T, T+1} is accepted only when
// s > lastUsedStep. On success it returns the accepted step, which the caller
// must persist as the new last used step (a monotonic conditional UPDATE).
//
// All three candidates are always computed and compared in constant time; the
// loop never exits early. When more than one candidate matches, the highest
// one is returned, so the largest possible step is burned.
func Verify(secret []byte, code string, now time.Time, lastUsedStep int64) (int64, error) {
	if len(secret) != SecretBytes {
		return 0, ErrInvalidSecret
	}
	if !validCode(code) {
		return 0, ErrInvalidCode
	}
	if lastUsedStep < 0 {
		return 0, ErrInvalidState
	}
	current, err := StepAt(now)
	if err != nil {
		return 0, err
	}
	got := []byte(code)
	var accepted int64
	found := 0
	for d := int64(-Skew); d <= Skew; d++ {
		s := current + d
		// A negative candidate only exists in the first 30 seconds after the
		// epoch; it is computed as step 0 and can never be eligible.
		cs := s
		if cs < 0 {
			cs = 0
		}
		want := hotp(secret, uint64(cs), Digits)
		eq := subtle.ConstantTimeCompare(want, got)
		// The step numbers are not secret; only the code comparison must be
		// constant time.
		eligible := boolToInt(s > lastUsedStep)
		match := eq & eligible
		mask := -int64(match)
		accepted = (s & mask) | (accepted &^ mask)
		found |= match
	}
	if found != 1 {
		return 0, ErrRejected
	}
	return accepted, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
