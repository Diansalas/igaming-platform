package httpserver

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// ADR 0114 R1: the payment-attempt routes are gated on payment_force_resolve:read ALONE
// (withdrawal:review is not accepted, section 4). No current role holds withdrawal:review without
// payment_force_resolve:read, so a behavioural test cannot tell the two gates apart; this pins the
// registration text itself: each route is wrapped in exactly RequirePermission(PermPaymentForceResolveRead),
// and the file names no other permission and no any-of gate.
func TestPaymentAttemptRoutes_GatedOnForceResolveReadAlone(t *testing.T) {
	src, err := os.ReadFile("payment_attempts_read_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	// ADR 0119 S4a: registered through staffRoute (class none: reads); the inner gate is unchanged.
	handles := regexp.MustCompile(`(?m)^\s*mux\.Handle\("GET [^"]+",\n\s*staffRoute\(deps, stepUpNone, ([^\n]*)\)\)$`).FindAllStringSubmatch(s, -1)
	// A10c adds the attempt event history (GET .../{attemptID}/events), registered here with the same gate.
	if len(handles) != 3 {
		t.Fatalf("expected the three R1 route registrations, found %d", len(handles))
	}
	for _, h := range handles {
		if !strings.HasPrefix(h[1], "auth.RequirePermission(auth.PermPaymentForceResolveRead)(") {
			t.Errorf("R1 route is not gated on payment_force_resolve:read alone: %s", h[1])
		}
	}
	for _, banned := range []string{"RequireAnyPermission", "PermWithdrawalReview", "PermLedgerAdjustment", "PermAuditRead", "PermPlayerRead"} {
		// The doc comment names withdrawal:review in prose only ("withdrawal:review is not accepted").
		if strings.Contains(s, banned) {
			t.Errorf("payment_attempts_read_routes.go mentions %s", banned)
		}
	}
	// R2: the stream permission table is consulted by the handlers, and the route gate is the
	// any-of of exactly the three stream permissions.
	r2, err := os.ReadFile("staff_reconciliation_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(r2), "auth.RequireAnyPermission(auth.PermPaymentForceResolveRead, auth.PermLedgerAdjustmentRead, auth.PermSportsbookReconciliationRead)") {
		t.Error("the R2 route gate is not the any-of of the three stream permissions")
	}
	// A10c: the list handlers call staffStreamRequest directly (2) and the two single-item handlers through the
	// shared singleStaffReconciliationID helper, which calls it once (so 3 call sites, 2 helper uses).
	if strings.Count(string(r2), "staffStreamRequest(w, r, c)") != 3 || strings.Count(string(r2), "singleStaffReconciliationID(w, r, c, ") != 2 {
		t.Error("every R2 handler must authorise the requested stream through staffStreamRequest")
	}
}
