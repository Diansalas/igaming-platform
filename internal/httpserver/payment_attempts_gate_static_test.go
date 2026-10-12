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
	handles := regexp.MustCompile(`(?m)^\s*mux\.Handle\("GET [^"]+",\n\s*staff\(([^\n]*)\)\)$`).FindAllStringSubmatch(s, -1)
	if len(handles) != 2 {
		t.Fatalf("expected the two R1 route registrations, found %d", len(handles))
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
	if strings.Count(string(r2), "staffStreamRequest(w, r, c)") != 2 {
		t.Error("both R2 handlers must authorise the requested stream through staffStreamRequest")
	}
}
