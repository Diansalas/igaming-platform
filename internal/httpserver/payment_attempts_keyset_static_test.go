package httpserver

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// DB-free keyset pins (ADR 0114 R1): two rows with an identical created_at keep distinct cursors that
// round-trip exactly, and the list query orders and compares on the full (created_at, id) pair in the
// same direction. The behavioural tie test (a forced created_at tie paged one row at a time) is
// TestStaffReadR1_PaginationTies.
func TestPaymentAttemptKeyset_TiesAndOrderingPinned(t *testing.T) {
	at := time.Date(2026, 10, 12, 1, 2, 3, 456789000, time.UTC)
	a, b := uuid.MustParse("00000000-0000-0000-0000-00000000000a"), uuid.MustParse("00000000-0000-0000-0000-00000000000b")
	ca, cb := encodePaymentAttemptCursor(paymentAttemptCursor{CreatedAt: at, ID: a}), encodePaymentAttemptCursor(paymentAttemptCursor{CreatedAt: at, ID: b})
	if ca == cb {
		t.Fatal("cursors of two tied rows must differ")
	}
	for tok, id := range map[string]uuid.UUID{ca: a, cb: b} {
		got, err := decodePaymentAttemptCursor(tok)
		if err != nil || got.ID != id || !got.CreatedAt.Equal(at) {
			t.Fatalf("tie cursor round trip: %+v %v", got, err)
		}
	}
	src, err := os.ReadFile("payment_attempts_read_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	for _, want := range []string{"(a.created_at, a.id) < ($", "ORDER BY a.created_at DESC, a.id DESC LIMIT"} {
		if strings.Count(s, want) != 1 {
			t.Errorf("payment_attempts_read_routes.go must contain exactly one %q", want)
		}
	}
	// strictPage is the one limit parser of the list route.
	if !strings.Contains(s, "strictPage(q)") {
		t.Error("the R1 list must parse limit through strictPage")
	}
	// R1 and R1b read in READ ONLY transactions (no write can be issued from a read route).
	if !strings.Contains(s, "deps.DB.WithTenantReadOnly(ctx, c.target") {
		t.Error("R1 must read through WithTenantReadOnly")
	}
	wd, err := os.ReadFile("withdrawal_handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(wd), "WithTenantReadOnly(r.Context(), tc.TenantID") || strings.Contains(string(wd), "WithPrincipalScope") {
		t.Error("R1b must read through WithTenantReadOnly (and withdrawal_handlers.go must not open a principal-scope transaction)")
	}
	if strings.Contains(s, "WithPrincipalScope") {
		t.Error("the R1 reads must never open a principal-scope transaction")
	}
	// C1: the principal is set for tenant callers only.
	if !strings.Contains(s, "if !c.platform() {\n\t\t\t// Tenant callers only") {
		t.Error("readScope must set the principal for tenant callers only (security C1)")
	}
	if strings.Count(s, "attemptReadSelect(!c.platform())") != 2 {
		t.Error("both R1 reads must ask for the open resolution only for tenant callers")
	}
}
