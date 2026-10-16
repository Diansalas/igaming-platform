package httpserver

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestValidIdempotencyKey(t *testing.T) {
	for k, want := range map[string]bool{
		"a": true, "k1-create": true, "with space and ~": true, strings.Repeat("k", 120): true,
		"": false, strings.Repeat("k", 121): false, "tab\there": false, "nl\n": false, "del\x7f": false, "unié": false, "\x00": false,
	} {
		if got := validIdempotencyKey(k); got != want {
			t.Errorf("validIdempotencyKey(%q) = %v, want %v", k, got, want)
		}
	}
}

// A10c freeze: staffIdempotent wraps exactly the 13 mutating K1 / K3 / HSEC routes - every POST of those three route
// files, no GET, and no route of any other file (adoption elsewhere is a reviewed decision, not an accident).
func TestStaffIdempotent_WrapsExactlyTheK1K3HSECMutations(t *testing.T) {
	wrapped := map[string]int{"capability_routes.go": 5, "payment_force_resolution_routes.go": 4, "withdrawal_hold_resolution_routes.go": 4}
	reg := regexp.MustCompile(`(?m)mux\.Handle\("(GET|POST) [^"]+",\s*\n\s*staffRoute\(deps, \w+, ([^\n]*)\)\)$`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "staff_idempotency.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		got := 0
		for _, m := range reg.FindAllStringSubmatch(string(b), -1) {
			has := strings.Contains(m[2], "staffIdempotent(deps, ")
			switch {
			case has && m[1] != "POST":
				t.Errorf("%s: a %s route is wrapped by staffIdempotent: %s", f, m[1], m[2])
			case has:
				got++
			case m[1] == "POST" && wrapped[f] > 0:
				t.Errorf("%s: a mutating route of a K1/K3/HSEC file is not wrapped by staffIdempotent: %s", f, m[2])
			}
		}
		if got != wrapped[f] {
			t.Errorf("%s: %d staffIdempotent-wrapped routes, want %d", f, got, wrapped[f])
		}
	}
}
