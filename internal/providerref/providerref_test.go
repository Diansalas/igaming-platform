package providerref

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func reasonOf(t *testing.T, err error) Reason {
	t.Helper()
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("expected *providerref.Error, got %v", err)
	}
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected the error to wrap ErrInvalid, got %v", err)
	}
	return e.Reason
}

func TestMaxBytesIs255(t *testing.T) {
	// Pinned: migration 0099's CHECK constraints hard-code the same value.
	// Changing one without the other would make the app accept what the
	// database refuses (a generic 500 instead of the deterministic 4xx).
	if MaxBytes != 255 {
		t.Fatalf("MaxBytes changed to %d: update migration 0099's successor, the OpenAPI maxLength and the design note together", MaxBytes)
	}
}

func TestValidate_ExactMaxAccepted(t *testing.T) {
	v := strings.Repeat("a", MaxBytes)
	if err := Validate("provider_tx_id", v); err != nil {
		t.Fatalf("exact max must be accepted: %v", err)
	}
	if err := ValidateOptional("provider_tx_id", v); err != nil {
		t.Fatalf("exact max must be accepted (optional): %v", err)
	}
}

func TestValidate_MaxPlusOneRejected(t *testing.T) {
	v := strings.Repeat("a", MaxBytes+1)
	err := Validate("provider_tx_id", v)
	if got := reasonOf(t, err); got != ReasonTooLong {
		t.Fatalf("reason: got %q want too_long", got)
	}
	e, _ := AsError(err)
	if e.Length != MaxBytes+1 || e.Field != "provider_tx_id" {
		t.Fatalf("unexpected error detail %+v", e)
	}
	if err := ValidateOptional("provider_tx_id", v); reasonOf(t, err) != ReasonTooLong {
		t.Fatalf("optional must apply the same bound")
	}
}

func TestValidate_MultibyteAtBoundary(t *testing.T) {
	// "é" is 2 bytes; "€" is 3 bytes. The bound is BYTES, not runes.
	exact := strings.Repeat("é", 127) + "a" // 254 + 1 = 255 bytes, 128 runes
	if len(exact) != MaxBytes {
		t.Fatalf("fixture: %d bytes", len(exact))
	}
	if err := Validate("round_id", exact); err != nil {
		t.Fatalf("255-byte multibyte value must be accepted: %v", err)
	}
	over := strings.Repeat("é", 128) // 256 bytes, only 128 runes
	if got := reasonOf(t, Validate("round_id", over)); got != ReasonTooLong {
		t.Fatalf("256-byte multibyte value (128 runes) must be too_long, got %q", got)
	}
	// A 3-byte rune straddling the boundary: 253 ASCII + "€" = 256 bytes.
	straddle := strings.Repeat("a", 253) + "€"
	if got := reasonOf(t, Validate("round_id", straddle)); got != ReasonTooLong {
		t.Fatalf("rune straddling the boundary must be too_long, got %q", got)
	}
	fits := strings.Repeat("a", 252) + "€" // 255 bytes
	if err := Validate("round_id", fits); err != nil {
		t.Fatalf("252 ASCII + 3-byte rune = 255 bytes must be accepted: %v", err)
	}
}

func TestValidate_InvalidUTF8Rejected(t *testing.T) {
	for name, v := range map[string]string{
		"lone continuation": "abc\x80def",
		"truncated 2-byte":  "abc\xc3",
		"overlong":          "\xc0\xaf",
		"surrogate":         "\xed\xa0\x80",
		"0xff":              "ref-\xff",
	} {
		if got := reasonOf(t, Validate("provider_reference", v)); got != ReasonInvalidUTF8 {
			t.Errorf("%s: reason %q want invalid_utf8", name, got)
		}
	}
}

func TestValidate_ControlCharsRejected(t *testing.T) {
	for name, v := range map[string]string{
		"NUL": "a\x00b", "newline": "a\nb", "CR": "a\rb", "tab": "a\tb",
		"ESC": "a\x1bb", "unit sep": "a\x1fb", "DEL": "a\x7fb",
		"C1 first": "a\u0080b", "C1 last": "a\u009fb",
	} {
		if got := reasonOf(t, Validate("provider_tx_id", v)); got != ReasonControlChar {
			t.Errorf("%s: reason %q want control_char", name, got)
		}
	}
	// Just outside the control ranges: accepted.
	for _, v := range []string{"a b", "a~b", "a b", "ref-é", "日本語-123", "a:b/c=d+e"} {
		if err := Validate("provider_tx_id", v); err != nil {
			t.Errorf("%q must be accepted: %v", v, err)
		}
	}
}

func TestValidate_EmptyRequiredVsOptional(t *testing.T) {
	if got := reasonOf(t, Validate("provider_tx_id", "")); got != ReasonEmpty {
		t.Fatalf("required empty: got %q", got)
	}
	if err := ValidateOptional("original_provider_tx_id", ""); err != nil {
		t.Fatalf("optional empty must be accepted: %v", err)
	}
}

func TestValidateAll_FirstFailureWins(t *testing.T) {
	long := strings.Repeat("x", MaxBytes+1)
	err := ValidateAll(
		Field{Name: "a", Value: "ok", Required: true},
		Field{Name: "b", Value: "", Required: false},
		Field{Name: "c", Value: long},
		Field{Name: "d", Value: "", Required: true},
	)
	e, ok := AsError(err)
	if !ok || e.Field != "c" || e.Reason != ReasonTooLong {
		t.Fatalf("expected c/too_long, got %v", err)
	}
	if err := ValidateAll(Field{Name: "a", Value: "ok", Required: true}, Field{Name: "b"}); err != nil {
		t.Fatalf("all valid: %v", err)
	}
	if e, _ := AsError(ValidateAll(Field{Name: "d", Required: true})); e == nil || e.Reason != ReasonEmpty {
		t.Fatalf("required empty in ValidateAll must be rejected")
	}
}

// TestError_NeverCarriesTheValue is the log-amplification guard: neither
// Error() nor LogAttrs() (nor a slog line built from them) contains any
// part of the rejected value.
func TestError_NeverCarriesTheValue(t *testing.T) {
	marker := "SECRETMARKER"
	v := marker + strings.Repeat("Z", 4096)
	err := Validate("provider_tx_id", v)
	e, ok := AsError(err)
	if !ok {
		t.Fatalf("expected *Error")
	}
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Warn("x", e.LogAttrs()...)
	for _, s := range []string{err.Error(), buf.String()} {
		if strings.Contains(s, marker) || strings.Contains(s, "ZZZZ") {
			t.Fatalf("rejected value leaked: %q", s)
		}
		if len(s) > 512 {
			t.Fatalf("log/error output is not bounded: %d bytes", len(s))
		}
	}
	if !strings.Contains(buf.String(), `"ref_len":4108`) || !strings.Contains(buf.String(), `"ref_sha256_prefix":"`+Fingerprint(v)+`"`) {
		t.Fatalf("log must carry length and hash prefix: %s", buf.String())
	}
}

func TestFingerprint(t *testing.T) {
	// sha256("abc") = ba7816bf8f01cfea...
	if got := Fingerprint("abc"); got != "ba7816bf8f01" {
		t.Fatalf("Fingerprint(abc) = %q", got)
	}
	if Fingerprint("a") == Fingerprint("b") {
		t.Fatalf("distinct values must fingerprint differently")
	}
}
