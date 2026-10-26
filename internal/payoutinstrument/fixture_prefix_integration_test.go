//go:build integration

package payoutinstrument

import "strings"

// withPrefix returns value with its first byte (two hex characters) replaced by prefix, but never returns a value equal to the
// original: the world's fingerprint and seal are random per run, so a fixed replacement prefix would collide with the original
// about once in 256 runs. When the original already starts with prefix, a different prefix is used.
func withPrefix(value, prefix string) string {
	if strings.HasPrefix(value, prefix) {
		if prefix == "ee" {
			prefix = "dd"
		} else {
			prefix = "ee"
		}
	}
	return prefix + value[2:]
}
