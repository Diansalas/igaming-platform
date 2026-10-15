//go:build integration

package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/marketgate"
)

// flippingGate allows the handler's own (first) evaluation and refuses every later one, standing in
// for "the operating-market configuration was disabled between the handler's identity transaction and
// the orchestrator's phase A transaction". The orchestrator-owned evaluation is the real enforcement
// point (it runs in the transaction that writes the intent / mints the session); the handler's earlier
// one only avoids side effects for an already-refused request.
type flippingGate struct{ calls atomic.Int32 }

func (g *flippingGate) Locate(context.Context, marketgate.LocateRequest) marketgate.Evidence {
	return marketgate.Evidence{}
}

func (g *flippingGate) Guard(marketgate.Request, marketgate.Evidence) marketgate.Guard {
	return func(context.Context, pgx.Tx) (marketgate.Decision, error) {
		if g.calls.Add(1) == 1 {
			return marketgate.Decision{Allowed: true}, nil
		}
		return marketgate.Decision{Allowed: false, Code: marketgate.ReasonNotPermitted}, nil
	}
}

func TestMarketGate_HTTP_OrchestratorEvaluationRefusesWhenConfigurationFlipsMidRequest(t *testing.T) {
	w := newMarketWorld(t)

	for _, tc := range []struct {
		name string
		do   func(srv *httptest.Server) *http.Response
	}{
		{"deposit", func(srv *httptest.Server) *http.Response {
			return postJSON(t, srv, "/v1/me/deposits", w.player.Tokens.AccessToken, map[string]any{
				"asset_code": "EUR", "amount": 15000, "payment_method": "card", "idempotency_key": uuid.NewString()})
		}},
		{"launch", func(srv *httptest.Server) *http.Response {
			return postJSON(t, srv, "/v1/me/casino/games/"+w.gameID.String()+"/launch", w.player.Tokens.AccessToken,
				map[string]string{"asset_code": "EUR", "mode": "demo"})
		}},
	} {
		gate := &flippingGate{}
		deps := w.deps
		deps.MarketGate = gate
		srv := httptest.NewServer(New(deps))
		defer srv.Close()

		_, intents0, sessions0, _ := w.counts(t)
		status, body := refusalBody(t, tc.do(srv))
		if status != http.StatusForbidden {
			t.Fatalf("%s: the orchestrator's evaluation must refuse a request the handler's evaluation allowed, got %d (%s)", tc.name, status, body)
		}
		if got := gate.calls.Load(); got != 2 {
			t.Fatalf("%s: want exactly 2 gate evaluations (handler tx, orchestrator tx), got %d", tc.name, got)
		}
		_, intents1, sessions1, _ := w.counts(t)
		if intents1 != intents0 || sessions1 != sessions0 {
			t.Fatalf("%s: the orchestrator-level refusal created rows: intents %d->%d sessions %d->%d", tc.name, intents0, intents1, sessions0, sessions1)
		}
		// The same canonical refusal body as the handler-level one (request_id aside).
		const canonical = `{"code":"MARKET_NOT_AVAILABLE","message":"this service is not available in your region"}`
		if body != canonical {
			t.Fatalf("%s: refusal body differs from the canonical one: %s", tc.name, body)
		}
	}
}
