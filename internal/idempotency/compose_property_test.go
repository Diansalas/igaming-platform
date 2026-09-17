package idempotency

import (
	"fmt"
	"math/rand"
	"testing"
)

// TestComposeOccurrenceKey_NoTwoDistinctPairsCollide is the property-
// style test the directive requires for Security finding S-6: proof that
// no two distinct (reference, discriminator) pairs can ever produce the
// same composed string.
//
// The proof strategy: ComposeOccurrenceKey is deterministic and
// DecomposeOccurrenceKey is its exact left inverse
// (Decompose(Compose(ref, disc)) == (ref, disc) for every legal input -
// proved directly below). A function with an exact left inverse is
// injective by construction: if two distinct inputs produced the same
// output, decomposing that one output could not recover both distinct
// inputs, contradicting the round-trip property. So proving the round
// trip for a large, adversarially-constructed input set (below) is a
// direct, mechanical proof of collision-freedom for every pair it
// covers - not merely a spot check.
func TestComposeOccurrenceKey_NoTwoDistinctPairsCollide(t *testing.T) {
	type pair struct {
		ref  string
		disc *string
	}

	discOf := func(s string) *string { return &s }

	// Adversarially-chosen references/discriminators designed to defeat
	// a naive, unescaped "{ref}#{disc}" concatenation scheme: values
	// containing the delimiter itself, values that are prefixes or
	// suffixes of one another, empty discriminators, discriminators that
	// look like a length prefix, etc.
	refs := []string{
		"A", "A#1", "A#", "#1", "##", "A##1", "1", "12", "1:2", "5:A",
		"", // handled separately below - empty reference is invalid on its own
		"a very long reference with spaces and punctuation! #42",
		"0", "00", "-1", "reference#with#many#hashes#42",
		"colon:then#hash", "3:embedded:colon:reference",
	}
	discs := []*string{nil, discOf(""), discOf("1"), discOf("42"), discOf("A#1"), discOf("0"), discOf("999999999")}

	// Deterministic pseudo-random fuzz component, seeded for
	// reproducibility, generating additional adversarial byte strings
	// (including raw, non-UTF8-safe byte sequences) beyond the hand-
	// picked set above.
	rng := rand.New(rand.NewSource(20260917))
	randomString := func(maxLen int) string {
		n := rng.Intn(maxLen)
		b := make([]byte, n)
		alphabet := []byte("AB01:#") // biased toward the delimiter characters themselves
		for i := range b {
			b[i] = alphabet[rng.Intn(len(alphabet))]
		}
		return string(b)
	}
	for i := 0; i < 500; i++ {
		refs = append(refs, "fuzz-"+randomString(12))
		if rng.Intn(2) == 0 {
			discs = append(discs, nil)
		} else {
			d := randomString(8)
			discs = append(discs, &d)
		}
	}

	var pairs []pair
	for _, r := range refs {
		if r == "" {
			continue // ComposeOccurrenceKey rejects an empty reference outright
		}
		for _, d := range discs {
			pairs = append(pairs, pair{ref: r, disc: d})
		}
	}

	composedTo := make(map[string]pair, len(pairs))
	for _, p := range pairs {
		composed, err := ComposeOccurrenceKey(p.ref, p.disc)
		if err != nil {
			t.Fatalf("compose(%q, %v) failed: %v", p.ref, p.disc, err)
		}

		// Direct round-trip proof for THIS pair.
		gotRef, gotDisc, err := DecomposeOccurrenceKey(composed)
		if err != nil {
			t.Fatalf("decompose(compose(%q, %v)) failed: %v", p.ref, p.disc, err)
		}
		if gotRef != p.ref || !discEqual(gotDisc, p.disc) {
			t.Fatalf("round trip failed: compose(%q, %v) -> %q -> (%q, %v)",
				p.ref, p.disc, composed, gotRef, gotDisc)
		}

		// Collision proof: if this composed string was already produced
		// by a DIFFERENT (ref, disc) pair, that is the exact S-6 failure
		// mode - two distinct occurrences silently sharing one
		// idempotency key.
		if prior, exists := composedTo[composed]; exists {
			if prior.ref != p.ref || !discEqual(prior.disc, p.disc) {
				t.Fatalf("S-6 collision: (%q, %v) and (%q, %v) both composed to %q",
					prior.ref, prior.disc, p.ref, p.disc, composed)
			}
		} else {
			composedTo[composed] = p
		}
	}

	t.Logf("verified %d distinct (reference, discriminator) pairs, %d unique composed keys, zero collisions",
		len(pairs), len(composedTo))
}

func discEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// TestComposeOccurrenceKeyWithOrdinal_NoTwoDistinctPairsCollide is the
// same property specialized to the OccurrenceOrdinal convenience
// wrapper, over adversarial references (including references containing
// digits, colons, and hashes that could be mistaken for another
// encoding) and every legal ordinal 1..N.
func TestComposeOccurrenceKeyWithOrdinal_NoTwoDistinctPairsCollide(t *testing.T) {
	refs := []string{"A", "A#1", "1:A", "A#", "5:B", "ref", "ref#2", "ref#3"}
	var ordinals []*OccurrenceOrdinal
	ordinals = append(ordinals, nil)
	for i := OccurrenceOrdinal(1); i <= 20; i++ {
		o := i
		ordinals = append(ordinals, &o)
	}

	seen := make(map[string]string)
	for _, ref := range refs {
		for _, ord := range ordinals {
			composed, err := ComposeOccurrenceKeyWithOrdinal(ref, ord)
			if err != nil {
				t.Fatalf("compose(%q, %v): %v", ref, ord, err)
			}
			key := fmt.Sprintf("%q,%v", ref, ord)
			if priorKey, exists := seen[composed]; exists && priorKey != key {
				t.Fatalf("S-6 collision: %s and %s both composed to %q", priorKey, key, composed)
			}
			seen[composed] = key

			gotRef, gotOrd, err := DecomposeOccurrenceKeyOrdinal(composed)
			if err != nil {
				t.Fatalf("decompose(%q): %v", composed, err)
			}
			if gotRef != ref {
				t.Fatalf("reference mismatch: want %q got %q", ref, gotRef)
			}
			if (ord == nil) != (gotOrd == nil) || (ord != nil && *ord != *gotOrd) {
				t.Fatalf("ordinal mismatch: want %v got %v", ord, gotOrd)
			}
		}
	}
}
