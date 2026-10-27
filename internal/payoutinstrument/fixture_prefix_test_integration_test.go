//go:build integration

package payoutinstrument

import "testing"

// The fixture helper never returns its input, for every prefix the tests use, including when the input already starts with it.
func TestWithPrefixNeverReturnsTheOriginal(t *testing.T) {
	for _, prefix := range []string{"11", "22", "33", "55", "aa", "40", "41", "42", "43", "44", "45", "dd", "ee"} {
		for _, original := range []string{prefix + "0123456789abcdef", "ee0123456789abcdef", "dd0123456789abcdef", "000123456789abcdef"} {
			got := withPrefix(original, prefix)
			if got == original {
				t.Fatalf("withPrefix(%q,%q) returned the original", original, prefix)
			}
			if len(got) != len(original) {
				t.Fatalf("length changed: %q -> %q", original, got)
			}
		}
	}
}
