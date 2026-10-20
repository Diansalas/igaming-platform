package aml

import "testing"

// lic26 F-2: a vendor reference outside the conservative alphabet gives no stable identity, and the fingerprint's pair encoding is
// unambiguous (no two different pair sets share a byte string).
func TestLic26_MatchRefPairs_RejectsAmbiguousReferences(t *testing.T) {
	ok := []ScreeningMatch{{List: ListSanctions, VendorMatchRef: "mock-match-1"}, {List: ListPEP, VendorMatchRef: "A.b_c-9"}}
	if got := matchRefPairs(ok); len(got) != 2 {
		t.Fatalf("valid references: %v", got)
	}
	for _, ref := range []string{"", "a:b", "a\nb", "a b", "a;b", "a\x00b", "é", string(make([]byte, 129))} {
		if got := matchRefPairs([]ScreeningMatch{{List: ListSanctions, VendorMatchRef: ref}}); got != nil {
			t.Errorf("reference %q must give no fingerprint, got %v", ref, got)
		}
	}
}

func TestLic26_MatchFingerprint_PairsCannotCollide(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	a := matchFingerprint(key, []string{"sanctions:x", "pep:y"})
	// The old encoding joined with "\n": one pair that CONTAINS the separator collided with two pairs.
	if b := matchFingerprint(key, []string{"sanctions:x\npep:y"}); a == b {
		t.Fatal("two pairs and one pair containing the old separator must not share a fingerprint")
	}
	if b := matchFingerprint(key, []string{"sanctions:x;pep:y"}); a == b {
		t.Fatal("a pair containing the new separator must not collide")
	}
	if b := matchFingerprint(key, []string{"sanctions:xp", "ep:y"}); a == b {
		t.Fatal("moving the boundary between pairs must change the fingerprint")
	}
	if a != matchFingerprint(key, []string{"sanctions:x", "pep:y"}) {
		t.Fatal("deterministic")
	}
}
