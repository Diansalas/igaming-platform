package capability

// ADR 0119 S5 + S6 review fix F7, DB-free pins: the production build has no K1 re-assertion fallback; the switch exists
// only in the integration build; reassertActor refuses without a ProofActor unless that fallback is on, locks the
// actor's own row FOR SHARE and compares role and staff_version; approve and revoke both call it after their L1 lock.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestStatic_K1ReassertionPins(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	prod := read("reassert_prod.go")
	if !strings.HasPrefix(prod, "//go:build !integration\n") ||
		!regexp.MustCompile(`(?m)^func reassertFallbackOn\(\) bool \{ return false \}$`).MatchString(prod) || strings.Count(prod, "func ") != 1 {
		t.Fatal("reassert_prod.go must be the !integration build and declare only a fallback that returns false")
	}
	if !strings.HasPrefix(read("reassert_integration.go"), "//go:build integration\n") {
		t.Fatal("the fallback switch must exist only in the integration build")
	}
	proof := read("proof.go")
	i := strings.Index(proof, "func reassertActor(")
	j := strings.Index(proof[i:], "\n}\n")
	body := proof[i : i+j]
	for _, want := range []string{"if !ok {", "if reassertFallbackOn() {", "return ErrActorNotLive", "FOR SHARE", "version != a.StaffVersion", "role != a.Role"} {
		if !strings.Contains(body, want) {
			t.Fatalf("reassertActor must contain %q", want)
		}
	}
	capSrc := read("capability.go")
	for _, fn := range []string{"func DecideAndGrant(", "func RevokeGrant("} {
		k := strings.Index(capSrc, fn)
		e := strings.Index(capSrc[k:], "\n}\n")
		f := capSrc[k : k+e]
		lock, re := strings.Index(f, "FOR UPDATE"), strings.Index(f, "reassertActor(ctx, tx)")
		if lock < 0 || re < 0 || re < lock {
			t.Fatalf("%s must call reassertActor after its L1 FOR UPDATE", fn)
		}
	}
}
