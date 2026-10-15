//go:build integration

// SR8-1: sportsbook bet placement is market-gated with the same gate, two-layer pattern and refusal
// contract as registration, deposit and launch. Real gate, real configuration rows.
package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/marketgate"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

const canonicalMarketRefusal = `{"code":"MARKET_NOT_AVAILABLE","message":"this service is not available in your region"}`

func TestMarketGate_HTTP_SportsbookAllowedMarketAcceptsTheBet(t *testing.T) {
	w := newMarketWorld(t)
	resp := w.bet(t)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("an open market must accept a bet, got %d", resp.StatusCode)
	}
	if bets, tx := w.sportsbookRows(t); bets != 1 || tx != 1 {
		t.Fatalf("want 1 bet and 1 ledger transaction, got %d/%d", bets, tx)
	}
}

// Excluded country, unknown evidence and a closed configuration: ONE refusal, no stake locked, no bet,
// no ledger row, one audit row (operation sportsbook_wagering) per refusal.
func TestMarketGate_HTTP_SportsbookEveryCauseIsOneRefusalAndLocksNothing(t *testing.T) {
	w := newMarketWorld(t)
	causes := []struct {
		name  string
		apply func()
	}{
		{"country outside the operating market", func() { w.locator.SetCountry("DE") }},
		{"unknown location evidence", func() { w.locator.SetCountry("") }},
		{"market configuration disabled", func() { w.locator.SetCountry("MT"); w.setTenant(operatingmarket.StateDisabled) }},
	}
	audits := 0
	for _, c := range causes {
		c.apply()
		bets0, tx0 := w.sportsbookRows(t)
		status, body := refusalBody(t, w.bet(t))
		if status != http.StatusForbidden || body != canonicalMarketRefusal {
			t.Fatalf("%s: want the canonical 403, got %d %s", c.name, status, body)
		}
		if bets, tx := w.sportsbookRows(t); bets != bets0 || tx != tx0 {
			t.Fatalf("%s: a refused bet wrote rows: bets %d->%d ledger %d->%d", c.name, bets0, bets, tx0, tx)
		}
		audits++
		if got := w.refusalAudits(t, "sportsbook_wagering"); got != audits {
			t.Fatalf("%s: want %d refusal audit rows, got %d", c.name, audits, got)
		}
	}
	// The refusal is not the 200 accepted=false business rejection shape, and re-enabling reopens it at once.
	w.locator.SetCountry("MT")
	w.setTenant(operatingmarket.StateEnabled)
	resp := w.bet(t)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("after re-enable: want 201, got %d", resp.StatusCode)
	}
}

func TestMarketGate_HTTP_SportsbookNilGateFailsClosed(t *testing.T) {
	w := newMarketWorld(t)
	deps := w.deps
	deps.MarketGate = nil
	srv := httptest.NewServer(New(deps))
	t.Cleanup(srv.Close)
	bets0, tx0 := w.sportsbookRows(t)
	resp := postJSON(t, srv, "/v1/me/sportsbook/bets", w.player.Tokens.AccessToken, placeBetRequestBody(w.selection, 1000, 250, 100, "nil-gate-key"))
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no gate: want 503, got %d", resp.StatusCode)
	}
	if bets, tx := w.sportsbookRows(t); bets != bets0 || tx != tx0 {
		t.Fatal("a request with no market gate wrote rows")
	}
}

// The placement transaction's own (second) evaluation still refuses a request the handler's evaluation allowed.
func TestMarketGate_HTTP_SportsbookPlaceBetEvaluationRefusesWhenConfigurationFlipsMidRequest(t *testing.T) {
	w := newMarketWorld(t)
	gate := &flippingGate{}
	deps := w.deps
	deps.MarketGate = gate
	srv := httptest.NewServer(New(deps))
	t.Cleanup(srv.Close)

	bets0, tx0 := w.sportsbookRows(t)
	status, body := refusalBody(t, postJSON(t, srv, "/v1/me/sportsbook/bets", w.player.Tokens.AccessToken,
		placeBetRequestBody(w.selection, 1000, 250, 100, "flip-key")))
	if status != http.StatusForbidden || body != canonicalMarketRefusal {
		t.Fatalf("want the canonical 403 from PlaceBet's own evaluation, got %d %s", status, body)
	}
	if got := gate.calls.Load(); got != 2 {
		t.Fatalf("want 2 evaluations (handler, PlaceBet), got %d", got)
	}
	if bets, tx := w.sportsbookRows(t); bets != bets0 || tx != tx0 {
		t.Fatal("the orchestrator-level refusal wrote rows")
	}
}

// Client-supplied country/geo headers cannot move a sportsbook placement into the market.
func TestMarketGate_HTTP_SportsbookClientHeadersCannotMoveTheCountry(t *testing.T) {
	w := newMarketWorld(t)
	w.locator.SetCountry("DE")
	bets0, _ := w.sportsbookRows(t)
	req, _ := http.NewRequest(http.MethodPost, w.srv.URL+"/v1/me/sportsbook/bets",
		jsonBody(t, placeBetRequestBody(w.selection, 1000, 250, 100, "spoof-key")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+w.player.Tokens.AccessToken)
	for k, v := range map[string]string{"X-Country": "MT", "CF-IPCountry": "MT", "X-Forwarded-For": "203.0.113.7", "Accept-Language": "mt-MT"} {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("spoofed headers moved the request: %d", resp.StatusCode)
	}
	if bets, _ := w.sportsbookRows(t); bets != bets0 {
		t.Fatal("a spoofed placement wrote a bet")
	}
}

var _ = marketgate.PlayerMessage
