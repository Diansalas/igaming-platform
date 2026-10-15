package stafflifecycle

import "testing"

// Security review SR-A10a-6: a staff email must not carry control or Unicode format (bidi / zero-width) characters.
// The characters are built from code points so the source file holds no invisible text.
func TestValidEmail_RejectsControlAndFormatCharacters(t *testing.T) {
	if !validEmail("ops@example.com") {
		t.Fatal("plain address must be accepted")
	}
	with := func(r rune) string { return "a" + string(r) + "b@x.com" }
	for name, e := range map[string]string{
		"rlo U+202E":   with(0x202E),
		"lre U+202A":   with(0x202A),
		"isolate 2066": with(0x2066),
		"isolate 2069": with(0x2069),
		"zwsp U+200B":  with(0x200B),
		"lrm U+200E":   with(0x200E),
		"rlm U+200F":   with(0x200F),
		"bom U+FEFF":   with(0xFEFF),
		"nul":          with(0x00),
		"del":          with(0x7F),
		"c1 U+0085":    with(0x85),
		"invalid utf8": "a\xffb@x.com",
	} {
		if validEmail(e) {
			t.Errorf("%s: %q must be refused", name, e)
		}
	}
}
