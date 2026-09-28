package identity

import (
	"errors"
	"strings"
	"testing"
)

// TestValidateDisplayName covers ADR 0104 §3's hygiene CHECK, mirrored in
// Go, plus N-3's bidi-override/isolate and zero-width additions.
//
// The Unicode format characters under test are built via string(rune(...))
// rather than literal \u escapes in this file's own source text, so a
// linter (or an editor) never silently normalizes/strips them and the
// exact code point under test is unambiguous at the call site.
func TestValidateDisplayName(t *testing.T) {
	bidiOverrideRLO := "Bad" + string(rune(0x202E)) + "name"
	bidiOverridePDF := "Bad" + string(rune(0x202C)) + "name"
	bidiIsolate := "Bad" + string(rune(0x2066)) + "name"
	zeroWidthSpace := "Bad" + string(rune(0x200B)) + "name"
	zeroWidthBoundary := "Bad" + string(rune(0x200F)) + "name"

	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"normal name", "Alice Admin", false},
		{"single character", "A", false},
		{"exactly 100 characters", strings.Repeat("a", 100), false},
		{"empty string", "", true},
		{"101 characters", strings.Repeat("a", 101), true},
		{"control character (bell)", "Bad\u0007name", true},
		{"newline", "Bad\nname", true},
		// N-3: bidi overrides U+202A-U+202E.
		{"bidi override RLO (U+202E)", bidiOverrideRLO, true},
		{"bidi override PDF (U+202C)", bidiOverridePDF, true},
		// N-3: bidi isolates U+2066-U+2069.
		{"bidi isolate (U+2066)", bidiIsolate, true},
		// N-3: zero-width characters U+200B-U+200F.
		{"zero width space (U+200B)", zeroWidthSpace, true},
		{"zero width, boundary char (U+200F)", zeroWidthBoundary, true},
		// A <script> tag contains no control/bidi/zero-width characters at
		// all - it is a valid display_name at the hygiene layer; output
		// encoding (the JSON encoder) is what neutralizes it on the read
		// path, per ADR 0104 §5.4. Confirms hygiene validation does not
		// (and must not) attempt HTML-escaping.
		{"script tag is hygiene-valid", "<script>alert(1)</script>", false},
		{"unicode letters", "Sonia Nunez", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidateDisplayName(c.value)
			if c.wantErr && err == nil {
				t.Errorf("expected an error for %q, got nil", c.value)
			}
			if !c.wantErr && err != nil {
				t.Errorf("expected no error for %q, got %v", c.value, err)
			}
			if c.wantErr && err != nil && !errors.Is(err, ErrInvalidDisplayName) {
				t.Errorf("expected ErrInvalidDisplayName, got %v", err)
			}
		})
	}
}
