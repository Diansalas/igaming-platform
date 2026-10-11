package launchgov

import (
	"errors"
	"testing"
)

// Review condition (d): bidirectional-override and other format (Cf) characters are refused in free
// text, not stored (a stored U+202E would reorder what a console renders).
func TestLintFreeText_RefusesFormatCharacters(t *testing.T) {
	for name, s := range map[string]string{
		"right-to-left override": "approved‮ by counsel",
		"left-to-right isolate":  "see ⁦note⁩",
		"zero width space":       "re​view",
		"word joiner":            "a⁠b",
		"BOM":                    "x\ufeffnote",
		"soft hyphen":            "co­nditions",
		"line separator":         "one two",
		"paragraph separator":    "one two",
		"hangul filler U+3164":   "okㅤnote",
		"hangul choseong filler": "okᅟnote",
		"hangul jungseong fill":  "okᅠnote",
		"halfwidth hangul fill":  "okﾠnote",
	} {
		for _, field := range []string{"conditions_note", "responsible_operator_name", "determination_reference"} {
			if err := LintFreeText(field, s, 2000); !errors.Is(err, ErrInvalidInput) {
				t.Errorf("%s in %s must be refused: %v", name, field, err)
			}
		}
		if err := LintFreeText("conditions_note", s, 2000); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s must be refused: %v", name, err)
		}
	}
	for _, ok := range []string{"review again after the first quarter", "line one\nline two", "Überprüfung nach Q1"} {
		if err := LintFreeText("conditions_note", ok, 2000); err != nil {
			t.Errorf("%q must be accepted: %v", ok, err)
		}
	}
}
