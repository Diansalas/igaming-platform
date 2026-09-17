package idempotency

import (
	"errors"
	"testing"
)

func TestComposeOccurrenceKey_NoDiscriminatorRoundTrips(t *testing.T) {
	composed, err := ComposeOccurrenceKey("provider-ref-123", nil)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	ref, disc, err := DecomposeOccurrenceKey(composed)
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	if ref != "provider-ref-123" {
		t.Fatalf("expected reference %q, got %q", "provider-ref-123", ref)
	}
	if disc != nil {
		t.Fatalf("expected nil discriminator, got %q", *disc)
	}
}

func TestComposeOccurrenceKey_WithDiscriminatorRoundTrips(t *testing.T) {
	d := "7"
	composed, err := ComposeOccurrenceKey("settlement-ref-9", &d)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	ref, disc, err := DecomposeOccurrenceKey(composed)
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	if ref != "settlement-ref-9" {
		t.Fatalf("expected reference %q, got %q", "settlement-ref-9", ref)
	}
	if disc == nil || *disc != "7" {
		t.Fatalf("expected discriminator %q, got %v", "7", disc)
	}
}

func TestComposeOccurrenceKey_EmptyReferenceRejected(t *testing.T) {
	if _, err := ComposeOccurrenceKey("", nil); !errors.Is(err, ErrEmptyReference) {
		t.Fatalf("expected ErrEmptyReference, got %v", err)
	}
}

// TestComposeOccurrenceKey_S6ConcreteExampleDoesNotCollide is the exact
// scenario Security finding S-6 named: ref="A#1" with no ordinal, and
// ref="A" with ordinal 1. A naive "{ref}#{ordinal}" concatenation
// collapses both to the string "A#1". The length-prefixed encoding must
// produce two different composed strings.
func TestComposeOccurrenceKey_S6ConcreteExampleDoesNotCollide(t *testing.T) {
	a, err := ComposeOccurrenceKey("A#1", nil)
	if err != nil {
		t.Fatalf("compose a: %v", err)
	}
	one := "1"
	b, err := ComposeOccurrenceKey("A", &one)
	if err != nil {
		t.Fatalf("compose b: %v", err)
	}
	if a == b {
		t.Fatalf("S-6 regression: ref=%q (no ordinal) and ref=%q (ordinal 1) both composed to %q", "A#1", "A", a)
	}

	// Both must still decompose back to their OWN original pair, not
	// each other's.
	refA, discA, err := DecomposeOccurrenceKey(a)
	if err != nil || refA != "A#1" || discA != nil {
		t.Fatalf("decompose(a) = (%q, %v, %v), want (\"A#1\", nil, nil)", refA, discA, err)
	}
	refB, discB, err := DecomposeOccurrenceKey(b)
	if err != nil || refB != "A" || discB == nil || *discB != "1" {
		t.Fatalf("decompose(b) = (%q, %v, %v), want (\"A\", \"1\", nil)", refB, discB, err)
	}
}

func TestComposeOccurrenceKeyWithOrdinal_RoundTrips(t *testing.T) {
	ord := OccurrenceOrdinal(3)
	composed, err := ComposeOccurrenceKeyWithOrdinal("leg-ref", &ord)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	ref, gotOrd, err := DecomposeOccurrenceKeyOrdinal(composed)
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	if ref != "leg-ref" {
		t.Fatalf("expected reference %q, got %q", "leg-ref", ref)
	}
	if gotOrd == nil || *gotOrd != 3 {
		t.Fatalf("expected ordinal 3, got %v", gotOrd)
	}
}

func TestComposeOccurrenceKeyWithOrdinal_NilOrdinalRoundTrips(t *testing.T) {
	composed, err := ComposeOccurrenceKeyWithOrdinal("bet-ref", nil)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	ref, ord, err := DecomposeOccurrenceKeyOrdinal(composed)
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	if ref != "bet-ref" || ord != nil {
		t.Fatalf("expected (\"bet-ref\", nil), got (%q, %v)", ref, ord)
	}
}

func TestComposeOccurrenceKeyWithOrdinal_RejectsNonPositiveOrdinal(t *testing.T) {
	for _, bad := range []OccurrenceOrdinal{0, -1, -100} {
		if _, err := ComposeOccurrenceKeyWithOrdinal("ref", &bad); !errors.Is(err, ErrInvalidComposedKey) {
			t.Fatalf("ordinal %d: expected ErrInvalidComposedKey, got %v", bad, err)
		}
	}
}

func TestDecomposeOccurrenceKey_RejectsMalformedInput(t *testing.T) {
	cases := []string{
		"",
		"no-length-prefix-at-all",
		":missing-length",
		"abc:reference",     // non-decimal length prefix
		"0:",                // zero-length reference, never emitted by Compose
		"-1:x",              // negative-looking length prefix (non-decimal char '-')
		"100:short",         // declared length exceeds remaining input
		"3:abcXtrailing",    // trailing bytes after reference with no '#' separator
		"3:abc" + "garbage", // same shape, explicit
	}
	for _, c := range cases {
		if _, _, err := DecomposeOccurrenceKey(c); err == nil {
			t.Errorf("input %q: expected an error, got nil", c)
		} else if !errors.Is(err, ErrInvalidComposedKey) {
			t.Errorf("input %q: expected ErrInvalidComposedKey, got %v", c, err)
		}
	}
}

func TestDecomposeOccurrenceKeyOrdinal_RejectsNonNumericDiscriminator(t *testing.T) {
	d := "not-a-number"
	composed, err := ComposeOccurrenceKey("ref", &d)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	if _, _, err := DecomposeOccurrenceKeyOrdinal(composed); !errors.Is(err, ErrInvalidComposedKey) {
		t.Fatalf("expected ErrInvalidComposedKey, got %v", err)
	}
}

// TestComposeOccurrenceKey_ReferenceContainingBothDelimitersRoundTrips
// proves the reference may contain ANY byte sequence, including one that
// looks exactly like a fully-formed alternate composed key, and still
// round-trips correctly - the property a delimiter-based (rather than
// length-prefixed) scheme cannot offer without an escaping rule.
func TestComposeOccurrenceKey_ReferenceContainingBothDelimitersRoundTrips(t *testing.T) {
	adversarialRefs := []string{
		"A#1",
		"5:hello#1",
		"::::",
		"####",
		"3:abc#99",
		"contains:colon#and#hash",
	}
	for _, ref := range adversarialRefs {
		d := "42"
		composed, err := ComposeOccurrenceKey(ref, &d)
		if err != nil {
			t.Fatalf("compose(%q): %v", ref, err)
		}
		gotRef, gotDisc, err := DecomposeOccurrenceKey(composed)
		if err != nil {
			t.Fatalf("decompose(compose(%q)) failed: %v", ref, err)
		}
		if gotRef != ref {
			t.Fatalf("reference round-trip failed: want %q, got %q (composed=%q)", ref, gotRef, composed)
		}
		if gotDisc == nil || *gotDisc != "42" {
			t.Fatalf("discriminator round-trip failed for ref %q: got %v", ref, gotDisc)
		}
	}
}
