//go:build integration

package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// SR8R-3: the configuration changes between the registration PRE-CHECK (which allowed) and the account-creation
// transaction. The authoritative in-transaction check must refuse, and nothing may be created.
func TestMarketGate_HTTP_RegistrationInTransactionCheckRefusesWhenConfigurationFlipsAfterThePreCheck(t *testing.T) {
	w := newMarketWorld(t)
	gate := &flippingGate{}
	deps := w.deps
	deps.MarketGate = gate
	srv := httptest.NewServer(New(deps))
	t.Cleanup(srv.Close)

	p0, _, _, wl0 := w.counts(t)
	email := "flip-" + uuid.NewString() + "@example.com"
	status, body := refusalBody(t, postJSON(t, srv, "/v1/auth/register", "", map[string]string{
		"brand_slug": w.brand.Slug, "email": email, "password": "a-decent-password-1"}))
	if status != http.StatusForbidden || body != canonicalMarketRefusal {
		t.Fatalf("want the canonical 403 from the in-transaction check, got %d %s", status, body)
	}
	if got := gate.calls.Load(); got != 2 {
		t.Fatalf("want 2 evaluations (pre-check allowed, in-transaction refused), got %d", got)
	}
	if p1, _, _, wl1 := w.counts(t); p1 != p0 || wl1 != wl0 {
		t.Fatalf("the refused registration created rows: players %d->%d wallets %d->%d", p0, p1, wl0, wl1)
	}
	if n := countTenantRows(t, w.pool, w.tenant.ID, `SELECT count(*) FROM player_accounts WHERE tenant_id = $1 AND lower(email) = lower($2)`, w.tenant.ID, email); n != 0 {
		t.Fatalf("the refused registration left %d accounts", n)
	}
}
