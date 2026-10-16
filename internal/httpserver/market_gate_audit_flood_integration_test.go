//go:build integration

package httpserver

import (
	"net/http"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/marketgate"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

// SR8-5: an anonymous caller in an excluded market cannot flood the append-only audit_log. Every refusal
// is still answered 403 (the refusal itself is never sampled); only the audit rows are, per
// (tenant, brand, operation, reason) for registration, and the suppressed count is recorded on a later row.
func TestMarketGate_HTTP_AnonymousRegistrationRefusalFloodWritesBoundedAuditRows(t *testing.T) {
	w := newMarketWorld(t)
	// Registration is a brand-level rule (owner decision 3): it is refused only when the brand's
	// market configuration does not permit registration, never because of a country.
	w.setTenant(operatingmarket.StateDisabled)
	const attempts = 12
	for i := 0; i < attempts; i++ {
		resp := w.register(t)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("attempt %d: want 403, got %d", i, resp.StatusCode)
		}
	}
	rows := w.refusalAudits(t, "registration")
	if rows < 1 || rows > 6 {
		t.Fatalf("%d refused registrations wrote %d audit rows; want a small bounded number (burst 5 plus at most one refill)", attempts, rows)
	}
	if rows >= attempts {
		t.Fatal("the audit is not sampled")
	}
}

// Another brand/operation is not starved by one flooded bucket.
func TestMarketGate_HTTP_ARefusalFloodDoesNotSuppressAnotherOperationsAudit(t *testing.T) {
	w := newMarketWorld(t)
	w.setTenant(operatingmarket.StateDisabled)
	for i := 0; i < 10; i++ {
		resp := w.register(t)
		resp.Body.Close()
	}
	resp := w.deposit(t)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("deposit: %d", resp.StatusCode)
	}
	if n := w.refusalAudits(t, "deposit"); n != 1 {
		t.Fatalf("the first deposit refusal must be audited regardless of the registration flood, got %d rows", n)
	}
}

var _ = marketgate.AuditActionRefused
