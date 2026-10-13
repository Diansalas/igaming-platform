package main

import (
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/identity"
)

// ADR 0119 S3 re-review F3: -operator / -reason refuse control, format (bidi / zero-width) and line-separator
// characters, so an audit row cannot visually misrepresent who ran the job or why.
func TestOperatorContext_RefusesUnsafeCharacters(t *testing.T) {
	ok := operatorContext{Operator: "ops-alice", Reason: "INC-1 lost device (ticket #42) - café"}
	if err := ok.validate(true); err != nil {
		t.Fatalf("a plain operator / reason must pass: %v", err)
	}
	for _, r := range []rune{0x00, 0x07, 0x1B, 0x7F, 0x85, // control
		0x202A, 0x202B, 0x202C, 0x202D, 0x202E, // bidi overrides
		0x2066, 0x2067, 0x2068, 0x2069, // bidi isolates
		0x200B, 0x200C, 0x200D, 0x200E, 0x200F, // zero-width / marks
		0x2060, 0x2061, 0x2064, 0xFEFF, 0x061C, 0x00AD, // other format (Cf)
		0x2028, 0x2029, // line / paragraph separators
	} {
		for _, field := range []string{"operator", "reason"} {
			o := operatorContext{Operator: "ops-alice", Reason: "INC-1"}
			v := "ops" + string(r) + "alice"
			if field == "operator" {
				o.Operator = v
			} else {
				o.Reason = v
			}
			if err := o.validate(true); err == nil || !strings.Contains(err.Error(), "-"+field) {
				t.Errorf("%s with U+%04X must be refused, got %v", field, r, err)
			}
		}
	}
}

// Every rune migration 0109's staff_users_display_name_hygiene class refuses (probed through its Go mirror,
// identity.ValidateDisplayName) is refused here too.
func TestUnsafeAuditRune_CoversDisplayNameHygiene(t *testing.T) {
	checked := 0
	for r := rune(0); r <= 0x10FFFF; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		if identity.ValidateDisplayName("a"+string(r)+"b") != nil && !unsafeAuditRune(r) {
			t.Fatalf("U+%04X is refused for display names but not for -operator / -reason", r)
		}
		checked++
	}
	if checked < 1_000_000 {
		t.Fatalf("vacuous: %d runes", checked)
	}
}
