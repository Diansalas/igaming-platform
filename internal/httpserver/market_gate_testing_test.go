//go:build integration

package httpserver

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/marketgate"
)

// allowAllMarketGate is the test harness's operating-market gate for the
// many tests whose subject is NOT market blocking: it allows everything and
// writes nothing. It lives in a _test.go file on purpose (the precedent is
// payments' AllowAllDepositKYCGate): production code can never reach it, and
// production wiring (cmd/platform-api) builds the real marketgate.Gate. The
// market-blocking behaviour itself is tested with the REAL gate over a
// static Locator and real configuration rows (market_gate_http_integration_test.go,
// internal/marketgate).
type allowAllMarketGate struct{}

func (allowAllMarketGate) Locate(context.Context, marketgate.LocateRequest) marketgate.Evidence {
	return marketgate.Evidence{}
}

func (allowAllMarketGate) Guard(marketgate.Request, marketgate.Evidence) marketgate.Guard {
	return func(context.Context, pgx.Tx) (marketgate.Decision, error) {
		return marketgate.Decision{Allowed: true}, nil
	}
}
