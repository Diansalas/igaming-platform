//go:build integration

// R1 (ledger-finance re-verification after fix round A, gate 10.3-W1):
// proves the "casino_callback_replayed" structured Info line the webhook
// handler now emits (casino_handlers.go) appears on a REDELIVERY of an
// already-posted bet callback (the postBet E2 idempotency short-circuit,
// casino.ReceiveCallbackResult.Replayed's own doc comment) and is ABSENT
// on the first delivery of that same callback - so a redelivery can be
// told apart from a first delivery from this one allow-listed log line
// alone, without a database join.
package httpserver

import (
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/casino"
)

// findCasinoCallbackReplayedLines returns every "casino_callback_replayed"
// line captured, in order.
func findCasinoCallbackReplayedLines(lines []capturedLogLine) []capturedLogLine {
	var matches []capturedLogLine
	for _, l := range lines {
		if l.msg == "casino_callback_replayed" {
			matches = append(matches, l)
		}
	}
	return matches
}

func TestCasinoWebhook_ReplayedLogging_AbsentOnFirstDeliveryPresentOnRedelivery(t *testing.T) {
	pool, issuer := testEnv(t)
	logger, captured := newCapturingLogger()
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServerWithLogger(t, pool, issuer, orchestrator, logger)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)

	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	mustEnableCasinoCapability(t, srv, pool, tenant)

	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	sessionID := uuid.MustParse(launched.SessionID)

	const roundID = "round-replayed-log"
	const providerTxID = "bet-replayed-log-1"

	payload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, providerTxID, "", roundID, game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)

	// First delivery: succeeds, posts the bet, must NOT be logged as
	// replayed.
	first := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", payload)
	defer first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("expected the first delivery to succeed, got %d", first.StatusCode)
	}
	if lines := findCasinoCallbackReplayedLines(captured()); len(lines) != 0 {
		t.Fatalf("expected NO casino_callback_replayed line on the first delivery, got %d: %+v", len(lines), lines)
	}

	// Redelivery: the IDENTICAL callback (same provider_tx_id, same
	// payload) - postBet's E2 idempotency short-circuit returns the
	// original result without re-posting, and must now be logged as
	// replayed.
	second := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", payload)
	defer second.Body.Close()
	if second.StatusCode != http.StatusOK {
		t.Fatalf("expected the redelivery to also report success (idempotent replay), got %d", second.StatusCode)
	}

	lines := findCasinoCallbackReplayedLines(captured())
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 casino_callback_replayed line after the redelivery, got %d: %+v", len(lines), lines)
	}
	line := lines[0]

	if got, _ := line.attrs["tenant_id"].(string); got != tenant.ID.String() {
		t.Errorf("expected tenant_id=%q, got %q", tenant.ID.String(), got)
	}
	if got, _ := line.attrs["provider_id"].(string); got != "mock-casino" {
		t.Errorf("expected provider_id=%q, got %q", "mock-casino", got)
	}
	if got, _ := line.attrs["event_type"].(string); got != string(casino.CallbackEventBet) {
		t.Errorf("expected event_type=%q, got %q", string(casino.CallbackEventBet), got)
	}
	if _, present := line.attrs["request_id"]; !present {
		t.Error("expected request_id to always be present")
	}

	// The line is allow-listed - it must never carry the provider_tx_id,
	// amount, or any other caller-supplied identifying value (matching this
	// handler's existing "never echo caller-supplied identifiers"
	// discipline for every other webhook log line - see
	// casino_webhook_auth_failure_logging_integration_test.go's identical
	// allow-list check).
	for _, forbiddenKey := range []string{"provider_tx_id", "amount", "round_id", "body", "header", "signature"} {
		if _, present := line.attrs[forbiddenKey]; present {
			t.Errorf("casino_callback_replayed must never carry a %q field", forbiddenKey)
		}
	}
}
