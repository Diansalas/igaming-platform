package payoutinstrument

import (
	"crypto/rand"
	"testing"
)

func randKey(t testing.TB) []byte {
	t.Helper()
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// newTestKeys builds Keys with one master kid and the given fingerprint kids
// (the first is active). Keys are random per call: never committed.
func newTestKeys(t testing.TB, fpKids ...string) *Keys {
	t.Helper()
	if len(fpKids) == 0 {
		fpKids = []string{"f1"}
	}
	fp := map[string][]byte{}
	for _, k := range fpKids {
		fp[k] = randKey(t)
	}
	k, err := NewKeys("m1", map[string][]byte{"m1": randKey(t)}, fpKids[0], fp)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
