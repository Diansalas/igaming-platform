package kyc

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A clean, already-short string passes through unchanged - no
// false-positive mutation on the identity case (QA W1d binding
// requirement).
func TestNormalizeReason_CleanShortStringUnchanged(t *testing.T) {
	const in = "document_expired"
	got, truncated := NormalizeReason(in)
	if got != in {
		t.Fatalf("expected %q unchanged, got %q", in, got)
	}
	if truncated {
		t.Fatal("expected truncated=false for an already-clean, short string")
	}
}

func TestNormalizeReason_EmptyStringUnchanged(t *testing.T) {
	got, truncated := NormalizeReason("")
	if got != "" || truncated {
		t.Fatalf("expected empty/untruncated for empty input, got %q truncated=%v", got, truncated)
	}
}

// --- Length boundary: 511/512/513 bytes, plain ASCII ---

func TestNormalizeReason_LengthBoundary_511BytesNotTruncated(t *testing.T) {
	in := strings.Repeat("a", 511)
	got, truncated := NormalizeReason(in)
	if truncated {
		t.Fatal("511 bytes must not be truncated")
	}
	if len(got) != 511 {
		t.Fatalf("expected 511 bytes, got %d", len(got))
	}
}

func TestNormalizeReason_LengthBoundary_512BytesNotTruncated(t *testing.T) {
	in := strings.Repeat("a", 512)
	got, truncated := NormalizeReason(in)
	if truncated {
		t.Fatal("exactly 512 bytes (the bound itself) must not be truncated")
	}
	if len(got) != 512 {
		t.Fatalf("expected 512 bytes, got %d", len(got))
	}
}

func TestNormalizeReason_LengthBoundary_513BytesTruncatedTo512(t *testing.T) {
	in := strings.Repeat("a", 513)
	got, truncated := NormalizeReason(in)
	if !truncated {
		t.Fatal("513 bytes must be truncated")
	}
	if len(got) != MaxReasonBytes {
		t.Fatalf("expected exactly %d bytes, got %d", MaxReasonBytes, len(got))
	}
}

// --- Multi-byte UTF-8 straddling the cut point: never split a rune ---

func TestNormalizeReason_MultibyteNeverSplitsARune(t *testing.T) {
	// "é" (U+00E9) is 2 bytes in UTF-8. 300 repeats = 600 bytes, so the
	// naive byte-512 cut point falls squarely inside one of the 2-byte
	// characters (600 is even, 512 is even, but the cut logic must not
	// assume that - vary with an odd-width prefix to force a genuine
	// straddle).
	in := "x" + strings.Repeat("é", 300) // 1 + 600 = 601 bytes total
	got, truncated := NormalizeReason(in)
	if !truncated {
		t.Fatal("expected truncation for a 601-byte input")
	}
	if len(got) > MaxReasonBytes {
		t.Fatalf("expected at most %d bytes, got %d", MaxReasonBytes, len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("expected valid UTF-8 after truncation, got invalid string %q", got)
	}
}

func TestNormalizeReason_MultibyteFourByteCharacterNeverSplit(t *testing.T) {
	// U+1F600 (grinning face emoji) is 4 bytes in UTF-8.
	in := strings.Repeat("\U0001F600", 200) // 800 bytes
	got, truncated := NormalizeReason(in)
	if !truncated {
		t.Fatal("expected truncation")
	}
	if !utf8.ValidString(got) {
		t.Fatalf("expected valid UTF-8 after truncation, got invalid string %q", got)
	}
	if len(got) > MaxReasonBytes {
		t.Fatalf("expected at most %d bytes, got %d", MaxReasonBytes, len(got))
	}
	// Every rune in the truncated result must be a complete, correctly
	// decoded 4-byte character - never a partial/replacement rune
	// introduced by a mid-character cut.
	for _, r := range got {
		if r != '\U0001F600' {
			t.Fatalf("expected only complete grinning-face runes, found %q (truncation split a rune)", r)
		}
	}
}

// --- Control character stripping ---

func TestNormalizeReason_StripsNUL(t *testing.T) {
	in := "before\x00after"
	got, _ := NormalizeReason(in)
	if strings.ContainsRune(got, 0x00) {
		t.Fatalf("expected NUL stripped, got %q", got)
	}
	if got != "beforeafter" {
		t.Fatalf("expected %q, got %q", "beforeafter", got)
	}
}

func TestNormalizeReason_StripsCR(t *testing.T) {
	in := "line1\rline2"
	got, _ := NormalizeReason(in)
	if strings.ContainsRune(got, '\r') {
		t.Fatalf("expected CR stripped, got %q", got)
	}
}

func TestNormalizeReason_StripsESC_ANSIEscape(t *testing.T) {
	in := "\x1b[31mFAKE ADMIN MESSAGE\x1b[0m"
	got, _ := NormalizeReason(in)
	if strings.ContainsRune(got, 0x1b) {
		t.Fatalf("expected ESC stripped, got %q", got)
	}
	if !strings.Contains(got, "FAKE ADMIN MESSAGE") {
		t.Fatalf("expected the printable text to survive, got %q", got)
	}
}

func TestNormalizeReason_StripsOtherC0Controls(t *testing.T) {
	var b strings.Builder
	for r := rune(0x00); r <= 0x1F; r++ {
		b.WriteRune(r)
	}
	got, _ := NormalizeReason("x" + b.String() + "y")
	if got != "xy" {
		t.Fatalf("expected all C0 controls stripped leaving %q, got %q", "xy", got)
	}
}

func TestNormalizeReason_StripsDELAndC1Controls(t *testing.T) {
	in := "x\x7Fy\u0080z\u009Fw"
	got, _ := NormalizeReason(in)
	if got != "xyzw" {
		t.Fatalf("expected DEL/C1 controls stripped leaving %q, got %q", "xyzw", got)
	}
}

// --- Bidi/format control stripping (security review C16) ---

func TestNormalizeReason_StripsRightToLeftOverride(t *testing.T) {
	in := "evil\u202Ereversed-text"
	got, _ := NormalizeReason(in)
	if strings.ContainsRune(got, 0x202E) {
		t.Fatalf("expected RIGHT-TO-LEFT OVERRIDE stripped, got %q", got)
	}
}

func TestNormalizeReason_StripsLRMAndRLM(t *testing.T) {
	in := "a\u200Eb\u200Fc"
	got, _ := NormalizeReason(in)
	if got != "abc" {
		t.Fatalf("expected LRM/RLM stripped leaving %q, got %q", "abc", got)
	}
}

func TestNormalizeReason_StripsZeroWidthAndIsolateControls(t *testing.T) {
	in := "a\u200Bb\u2066c\u2069d"
	got, _ := NormalizeReason(in)
	if got != "abcd" {
		t.Fatalf("expected zero-width/isolate controls stripped leaving %q, got %q", "abcd", got)
	}
}

// --- Invalid UTF-8 ---

func TestNormalizeReason_InvalidUTF8Replaced(t *testing.T) {
	in := "before\xff\xfeafter"
	got, _ := NormalizeReason(in)
	if !utf8.ValidString(got) {
		t.Fatalf("expected valid UTF-8 output, got invalid string %q", got)
	}
	if !strings.Contains(got, "�") {
		t.Fatalf("expected the invalid bytes replaced with U+FFFD, got %q", got)
	}
	if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Fatalf("expected surrounding valid text to survive, got %q", got)
	}
}

// --- Combined: the E6 pre-fix evidence shape, all at once ---

func TestNormalizeReason_CombinedOversizeControlAndBidi_BoundedAndClean(t *testing.T) {
	controlLaden := "\r\x1b[31mFAKE ADMIN MESSAGE\x1b[0m\u202Eevil-reversed-text"
	huge := controlLaden + strings.Repeat("A", 4096) + controlLaden
	got, truncated := NormalizeReason(huge)
	if !truncated {
		t.Fatal("expected truncation for an oversized input")
	}
	if len(got) > MaxReasonBytes {
		t.Fatalf("expected at most %d bytes, got %d", MaxReasonBytes, len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("expected valid UTF-8, got %q", got)
	}
	for _, bad := range []string{"\r", "\x1b", "\u202E"} {
		if strings.Contains(got, bad) {
			t.Fatalf("expected control/bidi character %q stripped, got %q", bad, got)
		}
	}
}
