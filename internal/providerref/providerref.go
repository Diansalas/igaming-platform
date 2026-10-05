// Package providerref is the single, platform-wide bound on provider-
// supplied references (PROVIDER-REF-BOUND-1; security review
// 10-gate-w2w3-review-security.md R-2; design note
// docs/plans/payment-readiness/prh-ref-provider-reference-bound.md).
//
// A "provider reference" is any identifier a provider (casino aggregator,
// PSP, sportsbook feed, KYC vendor) supplies and the platform stores,
// indexes or compares: provider_tx_id, a rollback's original reference,
// round ids, payment provider references, sportsbook external refs, and an
// asset code when the provider supplies it. Every one of them is opaque:
// the platform never parses, trims, normalises or truncates it.
//
// The rule (identical to migration 0099's CHECK constraints):
//
//   - 1..MaxBytes bytes (octets, not runes) - so every UNIQUE/idempotency
//     btree index that includes a reference stays far below PostgreSQL's
//     ~2.7 KB btree tuple limit, including the composite ones;
//   - valid UTF-8;
//   - no control character: U+0000..U+001F, U+007F and U+0080..U+009F.
//
// A violation is a deterministic, non-retryable rejection. It is NEVER
// truncated: two distinct long references sharing a prefix would collide
// on the idempotency key and one provider event would silently be treated
// as a replay of another.
//
// This package is a dependency-free leaf on purpose, so the payments
// orchestrator redesign (ADR 0095) and every other domain can call it
// without importing a domain package.
package providerref

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"unicode/utf8"
)

// MaxBytes is the platform-wide maximum length of a provider reference, in
// bytes (octet_length in PostgreSQL). Changing it requires a new migration
// that replaces every 0099 CHECK constraint, and ledger-finance + security
// sign-off.
const MaxBytes = 255

// ErrInvalid is the sentinel every validation failure wraps. Domain
// packages wrap it in their own sentinel so their HTTP layer can map it to
// the domain's existing non-retryable rejection class.
var ErrInvalid = errors.New("providerref: invalid provider reference")

// Reason is the closed set of validation failure reasons. It is safe to
// log (it never carries any part of the value).
type Reason string

const (
	ReasonEmpty       Reason = "empty"
	ReasonTooLong     Reason = "too_long"
	ReasonInvalidUTF8 Reason = "invalid_utf8"
	ReasonControlChar Reason = "control_char"
	// ReasonReservedNamespace is the payments-only refusal of the reserved
	// provider-tx namespace (ADR 0101 5.4, F14): a reference starting with
	// ReservedOperatorPrefix can never come from a provider.
	ReasonReservedNamespace Reason = "reserved_namespace"
)

// ReservedOperatorPrefix is the provider-tx namespace reserved for an M2
// "declare paid" Step B posting (ADR 0101 5.4). It is exactly 27 bytes and is
// pinned equal to migration 0115's payment_reserved_ref_prefix() by a test.
// Only the payments validators below refuse it: the shared Validate is
// unchanged, so casino and sportsbook semantics do not change (their
// postings are covered by the all-sessions ledger trigger instead).
const ReservedOperatorPrefix = "platform-operator-declared:"

// Error describes one rejected reference. It deliberately does NOT carry
// the value: only the field name, the reason, the byte length and a short
// hash prefix, so no log line or error string built from it can amplify an
// oversize or hostile value.
type Error struct {
	Field  string
	Reason Reason
	// Length is the value's length in bytes.
	Length int
	// HashPrefix is Fingerprint(value): enough to correlate repeated
	// deliveries of the same value across log lines, never enough to
	// reconstruct it.
	HashPrefix string
}

func (e *Error) Error() string {
	return fmt.Sprintf("providerref: %s rejected (%s, %d bytes, sha256 %s)", e.Field, e.Reason, e.Length, e.HashPrefix)
}

func (e *Error) Unwrap() error { return ErrInvalid }

// LogAttrs returns the allow-listed slog key/value pairs for this
// rejection: field, reason, length and hash prefix. Never the value.
func (e *Error) LogAttrs() []any {
	return []any{"ref_field", e.Field, "ref_reason", string(e.Reason), "ref_len", e.Length, "ref_sha256_prefix", e.HashPrefix}
}

// fingerprintHexLen is the number of hex characters of SHA-256 kept by
// Fingerprint (48 bits).
const fingerprintHexLen = 12

// Fingerprint returns the first 12 hex characters of SHA-256(value). It is
// the only representation of a rejected value that may be logged.
func Fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:fingerprintHexLen]
}

// Validate checks a REQUIRED reference: an empty value is rejected.
func Validate(field, value string) error {
	if value == "" {
		return &Error{Field: field, Reason: ReasonEmpty, Length: 0, HashPrefix: Fingerprint(value)}
	}
	return check(field, value)
}

// hasReservedPrefix is the byte-prefix test (never a wildcard match): the
// Go twin of the database's left(col, 27) = payment_reserved_ref_prefix().
func hasReservedPrefix(value string) bool {
	return len(value) >= len(ReservedOperatorPrefix) && value[:len(ReservedOperatorPrefix)] == ReservedOperatorPrefix
}

func reservedErr(field, value string) error {
	return &Error{Field: field, Reason: ReasonReservedNamespace, Length: len(value), HashPrefix: Fingerprint(value)}
}

// ValidatePaymentReference is Validate plus the refusal of the reserved
// operator-declared prefix (ADR 0101 5.4, F14). Every payments ingress of a
// provider-supplied reference uses it. It returns the same *Error type, so
// every caller's existing AsError branch and 4xx mapping applies unchanged.
func ValidatePaymentReference(field, value string) error {
	if err := Validate(field, value); err != nil {
		return err
	}
	if hasReservedPrefix(value) {
		return reservedErr(field, value)
	}
	return nil
}

// ValidatePaymentReferenceOptional is ValidateOptional plus the same refusal.
func ValidatePaymentReferenceOptional(field, value string) error {
	if err := ValidateOptional(field, value); err != nil {
		return err
	}
	if hasReservedPrefix(value) {
		return reservedErr(field, value)
	}
	return nil
}

// ValidatePaymentReferences is ValidateAll plus the reserved-prefix refusal.
func ValidatePaymentReferences(fields ...Field) error {
	for _, f := range fields {
		var err error
		if f.Required {
			err = ValidatePaymentReference(f.Name, f.Value)
		} else {
			err = ValidatePaymentReferenceOptional(f.Name, f.Value)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ValidateOptional checks an OPTIONAL reference: an empty value means
// "absent" and is accepted; a present value must satisfy the full rule.
// Whether the field is required for a given event is the domain's own
// decision, made elsewhere.
func ValidateOptional(field, value string) error {
	if value == "" {
		return nil
	}
	return check(field, value)
}

// Field names one reference for ValidateAll.
type Field struct {
	Name     string
	Value    string
	Required bool
}

// ValidateAll checks every field in order and returns the first failure.
func ValidateAll(fields ...Field) error {
	for _, f := range fields {
		var err error
		if f.Required {
			err = Validate(f.Name, f.Value)
		} else {
			err = ValidateOptional(f.Name, f.Value)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func check(field, value string) error {
	// Length first: it is O(1), and an oversize value is rejected without
	// scanning up to 1 MiB of it.
	if len(value) > MaxBytes {
		return &Error{Field: field, Reason: ReasonTooLong, Length: len(value), HashPrefix: Fingerprint(value)}
	}
	if !utf8.ValidString(value) {
		return &Error{Field: field, Reason: ReasonInvalidUTF8, Length: len(value), HashPrefix: Fingerprint(value)}
	}
	for _, r := range value {
		if isControl(r) {
			return &Error{Field: field, Reason: ReasonControlChar, Length: len(value), HashPrefix: Fingerprint(value)}
		}
	}
	return nil
}

// isControl is the Unicode Cc category spelled out explicitly (C0, DEL,
// C1), so the rule is identical to migration 0099's regex and never
// depends on a locale or a Unicode table version.
func isControl(r rune) bool {
	return r <= 0x1F || (r >= 0x7F && r <= 0x9F)
}

// AsError returns the *Error inside err, if any.
func AsError(err error) (*Error, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
