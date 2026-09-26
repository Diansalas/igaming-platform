// KYC-REASON-BOUND-1 (Stage 10.3, security review C16, ruling R10): the
// verified sender's `reason` string is otherwise unbounded in length and
// charset (up to the KYC webhook body cap, ~256 KiB) and flows verbatim
// into kyc_verifications.reason, audit_log.metadata, and - before this
// fix - a player-facing HTTP response. ADR 0028 §5 already states the
// INTENT ("a real adapter must never put raw KYC evidence into
// ProviderResult.Reason - a short, non-sensitive, machine-readable code
// only") but nothing enforced it. NormalizeReason is the one place that
// intent becomes an actual bound, applied once at the ingestion boundary
// (an adapter's HandleCallback - see MockKYCProvider.HandleCallback in
// mock_provider.go - and the staff review write path,
// ReviewVerification in verification_service.go) rather than scattered
// across every downstream reader.
//
// Per HD-10.3-3 (binding human ruling): a player never sees ANY provider
// reason text, under any field name - this file bounds the value staff/
// compliance workflows and the audit trail retain, never a player-facing
// field.
package kyc

import (
	"strings"
	"unicode/utf8"
)

// MaxReasonBytes is the maximum length, in bytes, kyc_verifications.reason
// may hold (migration 0095's `octet_length(reason) <= 512` CHECK) - a
// generous but finite cap for a short status/rejection code, mirroring
// MaxDocumentSizeBytes' own "a reasonable foundation-stage default, not
// derived from any specific vendor's own limit" convention
// (internal/kyc/validate.go).
const MaxReasonBytes = 512

// bidiAndFormatControlRanges are the Unicode bidirectional-override and
// zero-width format control characters security review C16 requires
// stripped: U+200B-U+200F (ZERO WIDTH SPACE .. RIGHT-TO-LEFT MARK),
// U+202A-U+202E (LEFT-TO-RIGHT EMBEDDING .. RIGHT-TO-LEFT OVERRIDE), and
// U+2066-U+2069 (LEFT-TO-RIGHT ISOLATE .. POP DIRECTIONAL ISOLATE). These
// can visually reorder or hide characters in a rendered string even
// though correct HTML escaping (see backoffice/src) otherwise makes the
// byte content itself safe to render - stripping them at ingestion closes
// that residual "spoofed status text" surface for the staff/compliance
// audience this bounded reason is retained for.
var bidiAndFormatControlRanges = [][2]rune{
	{0x200B, 0x200F},
	{0x202A, 0x202E},
	{0x2066, 0x2069},
}

// isBidiOrFormatControl reports whether r is one of the Unicode
// bidi-override/zero-width format control characters this package strips.
func isBidiOrFormatControl(r rune) bool {
	for _, rg := range bidiAndFormatControlRanges {
		if r >= rg[0] && r <= rg[1] {
			return true
		}
	}
	return false
}

// isC0OrC1Control reports whether r is a C0 control (U+0000-U+001F),
// DEL (U+007F), or C1 control (U+0080-U+009F) - NUL, CR and ESC among
// them. A single-line status/reason code has no legitimate need for any
// of these.
func isC0OrC1Control(r rune) bool {
	return (r >= 0x0000 && r <= 0x001F) || r == 0x007F || (r >= 0x0080 && r <= 0x009F)
}

// NormalizeReason bounds a raw provider (or staff-entered) reason string
// before it is ever stored in kyc_verifications.reason, written to
// audit_log.metadata, or (staff-only) rendered in a back-office UI. It:
//
//  1. validates UTF-8, replacing any invalid byte sequence with the
//     Unicode replacement character U+FFFD (never silently drops bytes or
//     panics on malformed input - strings.ToValidUTF8's documented
//     behavior);
//  2. strips every C0/C1 control character, including NUL, CR and ESC;
//  3. strips the Unicode bidi-override/zero-width format control
//     characters named above;
//  4. bounds the result to MaxReasonBytes (512) bytes, cutting on a rune
//     boundary so a multi-byte character is never split in half.
//
// truncated reports whether step 4 actually cut anything, so a caller
// could record that truncation occurred rather than silently discarding
// information without any trace - this stage's schema has no separate
// truncation-marker column, so callers are not required to act on it
// today, but the signal is not hidden from them.
//
// Applied ONCE at the ingestion boundary (see this file's own doc
// comment) - never re-applied by every downstream reader.
func NormalizeReason(raw string) (bounded string, truncated bool) {
	valid := strings.ToValidUTF8(raw, "�")

	var b strings.Builder
	b.Grow(len(valid))
	for _, r := range valid {
		if isC0OrC1Control(r) || isBidiOrFormatControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	cleaned := b.String()

	if len(cleaned) <= MaxReasonBytes {
		return cleaned, false
	}
	cut := MaxReasonBytes
	for cut > 0 && !utf8.RuneStart(cleaned[cut]) {
		cut--
	}
	return cleaned[:cut], true
}
