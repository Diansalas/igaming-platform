//go:build stagingacceptance

package main

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // RFC 6238 default; SHA-1 inside HMAC (the platform's staff MFA uses exactly this).
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
)

// The suite deliberately re-implements RFC 6238 instead of importing internal/staffmfa/totp: it is a black-box
// client, and an independent implementation also proves the server's code against the RFC rather than against itself.
const (
	totpDigits = 6
	totpPeriod = 30 // seconds
	totpSkew   = 1  // the server accepts steps T-1, T, T+1
)

// totpSecret is a TOTP shared secret held in memory. It never prints: String/GoString/Format redact.
type totpSecret struct {
	key      []byte
	lastUsed int64 // highest step already presented (the server rejects steps <= its last accepted step)
	// persist, when set, is called with every step presented so a re-run within the replay window does not reuse a step.
	persist func(step int64)
}

func (totpSecret) String() string             { return "[redacted-totp-secret]" }
func (totpSecret) GoString() string           { return "[redacted-totp-secret]" }
func (totpSecret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte("[redacted-totp-secret]")) }
func (t *totpSecret) valid() bool             { return t != nil && len(t.key) > 0 }
func (t *totpSecret) base32() string          { return base32NoPad.EncodeToString(t.key) }
func newTOTPSecretFromBase32(s string) (*totpSecret, error) {
	s = strings.ToUpper(strings.NewReplacer(" ", "", "\n", "", "\r", "", "\t", "", "=", "").Replace(s))
	b, err := base32NoPad.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("totp secret is not valid base32")
	}
	if len(b) < 10 {
		return nil, fmt.Errorf("totp secret is too short")
	}
	return &totpSecret{key: b}, nil
}

var base32NoPad = base32.StdEncoding.WithPadding(base32.NoPadding)

func hotp(key []byte, counter uint64) string {
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], counter)
	m := hmac.New(sha1.New, key)
	m.Write(msg[:])
	sum := m.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	bin := (uint32(sum[off])&0x7f)<<24 | uint32(sum[off+1])<<16 | uint32(sum[off+2])<<8 | uint32(sum[off+3])
	v := bin % 1000000
	return fmt.Sprintf("%0*d", totpDigits, v)
}

// codeAtStep is what an authenticator app shows for the given step.
func (t *totpSecret) codeAtStep(step int64) string { return hotp(t.key, uint64(step)) }

func stepAt(now time.Time) int64 { return now.Unix() / totpPeriod }

// nextStep returns the step to present at `now`: the lowest step in [now-1, now+1] that is strictly greater than the
// last one presented (the server rejects replays). ok=false means every acceptable step is used up: wait for the
// clock (the caller sleeps until `waitUntil`).
func (t *totpSecret) nextStep(now time.Time) (step int64, ok bool, waitUntil time.Time) {
	cur := stepAt(now)
	for s := cur - totpSkew; s <= cur+totpSkew; s++ {
		if s > t.lastUsed {
			// Prefer the current step; only fall forward when it is already used (never a stale past step if a newer one is valid).
			if s < cur && cur > t.lastUsed {
				continue
			}
			return s, true, time.Time{}
		}
	}
	// Used up through cur+1: the next acceptable step is lastUsed+1, valid once the clock reaches (lastUsed+1-skew).
	need := t.lastUsed + 1 - totpSkew
	return 0, false, time.Unix(need*totpPeriod, 0)
}

// next returns a fresh code, sleeping (via sleep) if every acceptable step was already used. It marks the step used.
func (t *totpSecret) next(now func() time.Time, sleep func(time.Duration)) string {
	for {
		step, ok, until := t.nextStep(now())
		if ok {
			t.lastUsed = step
			if t.persist != nil {
				t.persist(step)
			}
			return t.codeAtStep(step)
		}
		d := until.Sub(now()) + 500*time.Millisecond
		if d < 0 {
			d = 500 * time.Millisecond
		}
		sleep(d)
	}
}
