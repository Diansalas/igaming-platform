package providercred

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
)

// FingerprintLabel is the HMAC domain-separation label (ADR 0093 §2).
const FingerprintLabel = "igaming/provider-credential-fingerprint/v1"

// FingerprintPrefix marks the fingerprint scheme version. Rotating the
// fingerprint key requires a new prefix (fp2:) and an ADR 0093 amendment.
const FingerprintPrefix = "fp1:"

// MinFingerprintKeyBytes mirrors config.MinProviderCredentialFingerprintKeyBytes.
const MinFingerprintKeyBytes = 32

// FingerprintKey is the platform fingerprint HMAC key. It never renders.
type FingerprintKey struct {
	k []byte
}

// NewFingerprintKey copies k. It refuses a key shorter than 32 bytes.
func NewFingerprintKey(k []byte) (FingerprintKey, error) {
	if len(k) < MinFingerprintKeyBytes {
		return FingerprintKey{}, errors.New("providercred: fingerprint key must be at least 32 bytes")
	}
	c := make([]byte, len(k))
	copy(c, k)
	return FingerprintKey{k: c}, nil
}

// Fingerprint returns "fp1:" + hex(HMAC-SHA256(key, label || 0x00 ||
// secret)). The input contains NO tenant id (ADR 0093 §2): the global
// unique key (domain, provider_id, purpose, fingerprint) must stop one
// secret being bound to two tenants.
func (k FingerprintKey) Fingerprint(secret []byte) string {
	mac := hmac.New(sha256.New, k.k)
	_, _ = mac.Write([]byte(FingerprintLabel))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write(secret)
	return FingerprintPrefix + hex.EncodeToString(mac.Sum(nil))
}

// IsSet reports whether the key is usable.
func (k FingerprintKey) IsSet() bool { return len(k.k) >= MinFingerprintKeyBytes }

const redactedKey = "[REDACTED-FINGERPRINT-KEY]"

// String implements fmt.Stringer.
func (k FingerprintKey) String() string { return redactedKey }

// GoString implements fmt.GoStringer.
func (k FingerprintKey) GoString() string { return redactedKey }

// Format implements fmt.Formatter.
func (k FingerprintKey) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redactedKey)) }

// LogValue implements slog.LogValuer.
func (k FingerprintKey) LogValue() slog.Value { return slog.StringValue(redactedKey) }

// MarshalJSON implements json.Marshaler.
func (k FingerprintKey) MarshalJSON() ([]byte, error) { return json.Marshal(redactedKey) }

var (
	fingerprintPattern  = regexp.MustCompile(`^fp1:[0-9a-f]{64}$`)
	confirmationPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// ValidFingerprint reports whether s has the stored fingerprint shape.
func ValidFingerprint(s string) bool { return fingerprintPattern.MatchString(s) }

// ValidConfirmationShape reports whether c is "sha256:" + 64 lowercase hex.
// The value itself is transient: it is never persisted, logged, audited or
// echoed (ADR 0093 A3).
func ValidConfirmationShape(c string) bool { return confirmationPattern.MatchString(c) }

// ConfirmationMatches compares the operator's confirmation value (an
// UNKEYED sha256 of the secret bytes, computed at provisioning) with the
// fetched secret, in constant time. A malformed value never matches.
func ConfirmationMatches(secret []byte, confirmation string) bool {
	if !ValidConfirmationShape(confirmation) {
		return false
	}
	want, err := hex.DecodeString(confirmation[len("sha256:"):])
	if err != nil {
		return false
	}
	got := sha256.Sum256(secret)
	return hmac.Equal(got[:], want)
}
