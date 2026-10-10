package identity

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// ADR 0112 section 7.3 / S4: the registration gate is a plain, NON-LOCKING read. Registration
// moves no money and must not contend with a suspension, so the gate may use no row lock
// (FOR SHARE / FOR UPDATE / FOR NO KEY UPDATE / FOR KEY SHARE) and no advisory lock. The
// behavioural twin is TestLaunchGate_Registration_ReadIsNonLocking_S4 (internal/httpserver).
func TestRegistrationGate_SourceTakesNoLock(t *testing.T) {
	b, err := os.ReadFile("player_account.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	i := strings.Index(src, "func RequireBrandAcceptingRegistrations(")
	if i < 0 {
		t.Fatal("RequireBrandAcceptingRegistrations not found; the pin would be vacuous")
	}
	body := src[i:]
	if j := strings.Index(body[1:], "\nfunc "); j >= 0 {
		body = body[:j+1]
	}
	if !strings.Contains(body, "SELECT b.status, t.status FROM brands b JOIN tenants t") {
		t.Fatal("the gate no longer reads brand and tenant status in one plain SELECT; re-review S4")
	}
	if m := regexp.MustCompile(`(?i)\bFOR\s+(NO\s+KEY\s+)?(SHARE|UPDATE|KEY\s+SHARE)\b|pg_advisory|pg_try_advisory|LOCK\s+TABLE`).FindString(body); m != "" {
		t.Fatalf("the registration gate must be a plain non-locking read (ADR 0112 S4), found %q", m)
	}
}
