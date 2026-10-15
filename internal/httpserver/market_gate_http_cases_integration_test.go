//go:build integration

package httpserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/marketgate"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

func TestMarketGate_HTTP_AllowedMarketPassesAtAllThreePoints(t *testing.T) {
	w := newMarketWorld(t) // newMarketWorld itself already registered a player under locator MT

	resp := w.register(t)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register: want 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = w.deposit(t)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("deposit: want 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = w.launch(t)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("launch: want 201, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	for _, op := range []string{"registration", "deposit", "launch"} {
		if n := w.refusalAudits(t, op); n != 0 {
			t.Fatalf("%s: an allowed request wrote %d refusal audit rows", op, n)
		}
	}
}

// The three refusal causes (country outside the market, unknown evidence,
// configuration withdrawn) at the three points: ONE status, ONE body, no
// side effects, one audit row per refusal.
func TestMarketGate_HTTP_EveryCauseIsOneIdenticalRefusalAndCreatesNothing(t *testing.T) {
	w := newMarketWorld(t)
	causes := []struct {
		name  string
		apply func()
	}{
		{"country outside the operating market", func() { w.locator.SetCountry("DE") }},
		{"unknown location evidence", func() { w.locator.SetCountry("") }},
		{"market configuration disabled", func() { w.locator.SetCountry("MT"); w.setTenant(operatingmarket.StateDisabled) }},
	}
	var canonical string
	audits := map[string]int{}
	for _, c := range causes {
		c.apply()
		p0, i0, s0, wl0 := w.counts(t)
		points := map[string]*http.Response{"registration": w.register(t), "deposit": w.deposit(t), "launch": w.launch(t)}
		for op, resp := range points {
			status, body := refusalBody(t, resp)
			if status != http.StatusForbidden {
				t.Fatalf("%s / %s: want 403, got %d (%s)", c.name, op, status, body)
			}
			var m struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			_ = json.Unmarshal([]byte(body), &m)
			if m.Code != string(apierror.CodeMarketNotAvailable) || m.Message != marketgate.PlayerMessage {
				t.Fatalf("%s / %s: unexpected refusal %s", c.name, op, body)
			}
			if canonical == "" {
				canonical = body
			} else if body != canonical {
				t.Fatalf("%s / %s: refusal differs from the canonical one (oracle):\n got  %s\n want %s", c.name, op, body, canonical)
			}
			audits[op]++
			if got := w.refusalAudits(t, op); got != audits[op] {
				t.Fatalf("%s / %s: want %d refusal audit rows, got %d", c.name, op, audits[op], got)
			}
		}
		p1, i1, s1, wl1 := w.counts(t)
		if p1 != p0 || i1 != i0 || s1 != s0 || wl1 != wl0 {
			t.Fatalf("%s: a refused request created rows: players %d->%d intents %d->%d sessions %d->%d wallets %d->%d",
				c.name, p0, p1, i0, i1, s0, s1, wl0, wl1)
		}
	}
}

func TestMarketGate_HTTP_ConfigurationChangeTakesEffectOnTheNextRequest(t *testing.T) {
	w := newMarketWorld(t)
	status := func(resp *http.Response) int { resp.Body.Close(); return resp.StatusCode }

	if got := status(w.deposit(t)); got != http.StatusCreated {
		t.Fatalf("open market: want 201, got %d", got)
	}
	w.setTenant(operatingmarket.StateDisabled)
	if got := status(w.deposit(t)); got != http.StatusForbidden {
		t.Fatalf("immediately after disable: want 403, got %d", got)
	}
	if got := status(w.launch(t)); got != http.StatusForbidden {
		t.Fatalf("immediately after disable (launch): want 403, got %d", got)
	}
	if got := status(w.register(t)); got != http.StatusForbidden {
		t.Fatalf("immediately after disable (register): want 403, got %d", got)
	}
	w.setTenant(operatingmarket.StateEnabled)
	if got := status(w.deposit(t)); got != http.StatusCreated {
		t.Fatalf("immediately after re-enable: want 201, got %d", got)
	}
	if got := status(w.launch(t)); got != http.StatusCreated {
		t.Fatalf("immediately after re-enable (launch): want 201, got %d", got)
	}
}

// A brand with no operating-market configuration is refused (ADR 0045: a
// tenant must opt in), and another tenant's open market does not leak.
func TestMarketGate_HTTP_UnconfiguredBrandIsRefusedAndTenantsAreIsolated(t *testing.T) {
	w := newMarketWorld(t)
	otherTenant := mustCreateTenant(t, w.pool)
	otherBrand := mustCreateBrand(t, w.pool, otherTenant)

	resp := postJSON(t, w.srv, "/v1/auth/register", "", map[string]string{
		"brand_slug": otherBrand.Slug, "email": "mg-iso-" + uuid.NewString() + "@example.com", "password": "a-decent-password-1",
	})
	status, body := refusalBody(t, resp)
	if status != http.StatusForbidden {
		t.Fatalf("unconfigured brand: want 403, got %d (%s)", status, body)
	}
	if n := countTenantRows(t, w.pool, otherTenant.ID, `SELECT count(*) FROM player_accounts WHERE tenant_id = $1`, otherTenant.ID); n != 0 {
		t.Fatalf("a refused registration created %d player accounts", n)
	}
	if n := countTenantRows(t, w.pool, otherTenant.ID,
		`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2`, otherTenant.ID, marketgate.AuditActionRefused); n != 1 {
		t.Fatalf("want 1 refusal audit row in the other tenant, got %d", n)
	}
	// Tenant A's own state is untouched by B's refusal.
	if n := w.refusalAudits(t, "registration"); n != 0 {
		t.Fatalf("tenant A saw %d refusal rows it did not cause", n)
	}
}

// The country is never read from the client: a body field and spoofed
// geo/forwarding headers cannot move a request into the operating market.
func TestMarketGate_HTTP_ClientSuppliedCountryIsIgnored(t *testing.T) {
	w := newMarketWorld(t)
	w.locator.SetCountry("DE") // the server-side evidence says: outside the market
	p0, _, _, _ := w.counts(t)

	body, _ := json.Marshal(map[string]string{
		"brand_slug": w.brand.Slug, "email": "mg-spoof-" + uuid.NewString() + "@example.com", "password": "a-decent-password-1",
		"country": "MT", "country_code": "MT",
	})
	req, _ := http.NewRequest(http.MethodPost, w.srv.URL+"/v1/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range map[string]string{
		"X-Country": "MT", "CF-IPCountry": "MT", "X-Forwarded-For": "203.0.113.7", "X-Real-IP": "203.0.113.7", "Accept-Language": "mt-MT",
	} {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusCreated {
		t.Fatal("a client-supplied country/header moved a request into the operating market")
	}
	if p1, _, _, _ := w.counts(t); p1 != p0 {
		t.Fatalf("a spoofed registration created %d player accounts", p1-p0)
	}
}

// Deps.MarketGate nil is a misconfiguration, not "ungated": the three
// routes answer 503 and write nothing.
func TestMarketGate_HTTP_NilGateFailsClosedAtAllThreePoints(t *testing.T) {
	w := newMarketWorld(t)
	deps := w.deps
	deps.MarketGate = nil
	srv := httptest.NewServer(New(deps))
	t.Cleanup(srv.Close)

	p0, i0, s0, wl0 := w.counts(t)
	for name, resp := range map[string]*http.Response{
		"register": postJSON(t, srv, "/v1/auth/register", "", map[string]string{
			"brand_slug": w.brand.Slug, "email": "mg-nil-" + uuid.NewString() + "@example.com", "password": "a-decent-password-1"}),
		"deposit": postJSON(t, srv, "/v1/me/deposits", w.player.Tokens.AccessToken, map[string]any{
			"asset_code": "EUR", "amount": 15000, "payment_method": "card", "idempotency_key": uuid.NewString()}),
		"launch": postJSON(t, srv, "/v1/me/casino/games/"+w.gameID.String()+"/launch", w.player.Tokens.AccessToken,
			map[string]string{"asset_code": "EUR", "mode": "demo"}),
	} {
		resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("%s with no gate: want 503, got %d", name, resp.StatusCode)
		}
	}
	if p1, i1, s1, wl1 := w.counts(t); p1 != p0 || i1 != i0 || s1 != s0 || wl1 != wl0 {
		t.Fatal("a request with no market gate wrote rows")
	}
}
