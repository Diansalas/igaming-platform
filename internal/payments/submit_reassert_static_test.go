package payments

// ADR 0119 S5 + S6 (the S4b-R3 / ADR 0082 A9 hard gate), DB-free pins for the withdrawal-submit re-assertion:
//   - the production build has NO fallback (submit_reassert_prod.go: `!integration`, returns false);
//   - the fallback switch exists only in the integration build;
//   - ClaimForDispatch calls reassertSubmitter right after InsertSubmittingAttempt (the A8 position) and before the
//     destination snapshot and the staff-attribution audit;
//   - the re-assertion reads the submitter's own row FOR SHARE and compares it through db.CheckActorStaffLive.
// The behaviour itself is proven by adr0119_submit_reassert_integration_test.go in both orders.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestStatic_SubmitReassertionPins(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	prod := read("submit_reassert_prod.go")
	if !strings.HasPrefix(prod, "//go:build !integration\n") {
		t.Fatal("submit_reassert_prod.go must be the !integration build")
	}
	if !regexp.MustCompile(`(?m)^func submitReassertionFallbackOn\(\) bool \{ return false \}$`).MatchString(prod) {
		t.Fatal("the production submitReassertionFallbackOn must return false")
	}
	if strings.Count(prod, "func ") != 1 {
		t.Fatal("submit_reassert_prod.go must declare nothing else")
	}
	integ := read("submit_reassert_integration.go")
	if !strings.HasPrefix(integ, "//go:build integration\n") {
		t.Fatal("the fallback switch must exist only in the integration build")
	}

	src := read("payout.go")
	body := s56Body(t, src, "func reassertSubmitter(")
	if !strings.Contains(body, "FROM staff_users WHERE id = $1 FOR SHARE") {
		t.Fatal("reassertSubmitter must read the submitter's own row FOR SHARE")
	}
	if !strings.Contains(body, "db.CheckActorStaffLive(") || !strings.Contains(body, "StaffVersion: actor.StaffVersion") ||
		!strings.Contains(body, "Role: actor.Role") {
		t.Fatal("reassertSubmitter must compare role and staff_version through db.CheckActorStaffLive")
	}
	if !strings.Contains(body, `actor.Role == "" && submitReassertionFallbackOn()`) {
		t.Fatal("the empty-role skip must be gated by the integration-only fallback")
	}
	claim := s56Body(t, src, "func (o *Orchestrator) ClaimForDispatch(")
	iAttempt := strings.Index(claim, "InsertSubmittingAttempt(")
	iReassert := strings.Index(claim, "reassertSubmitter(")
	iSnapshot := strings.Index(claim, "WriteSnapshot(")
	iAudit := strings.Index(claim, `Outcome: audit.OutcomeSuccess`)
	if iAttempt < 0 || iReassert < 0 || iSnapshot < 0 || iAudit < 0 || !(iAttempt < iReassert && iReassert < iSnapshot && iSnapshot < iAudit) {
		t.Fatalf("ClaimForDispatch order must be attempt < re-assertion < snapshot < success audit, got %d %d %d %d",
			iAttempt, iReassert, iSnapshot, iAudit)
	}
	if strings.Count(claim, "reassertSubmitter(") != 1 {
		t.Fatal("exactly one re-assertion call in ClaimForDispatch")
	}
}

// s56Body returns the source from the declaration starting with decl to the next top-level "\n}\n".
func s56Body(t *testing.T, src, decl string) string {
	t.Helper()
	i := strings.Index(src, decl)
	if i < 0 {
		t.Fatalf("%s not found", decl)
	}
	j := strings.Index(src[i:], "\n}\n")
	if j < 0 {
		t.Fatalf("%s: no end", decl)
	}
	return src[i : i+j]
}
