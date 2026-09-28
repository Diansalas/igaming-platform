//go:build integration

// CAS-PLAY-BOOTSTRAP-1 (ADR 0103), HTTP-layer tests: the real route, the
// MOCK vendor's bootstrap client sending a genuine HTTP POST (ADR 0103
// §7: "Nothing calls BootstrapLaunch in-process as a stand-in for the
// vendor"), and the end-to-end proof that the B2C MOCK play route
// (casino_play_handlers.go) now works past the launch token's own 2-minute
// TTL once a real bootstrap has consumed the session - CAS-PLAY-BOOTSTRAP-1's
// own registry row named this gap explicitly ("no vendor token-bootstrap
// path exists... so the B2C MOCK play route accepts bets for about 2
// minutes per launch").
package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// extractLaunchToken pulls the launch token out of MockCasinoProvider's
// synthetic LaunchURL ("https://mock-casino.invalid/launch/<game>?token=<tok>") -
// this package's own copy of internal/casino's identical test helper
// (test helpers are not shared across package boundaries in this
// codebase).
func extractLaunchToken(t *testing.T, launchURL string) string {
	t.Helper()
	parsed, err := url.Parse(launchURL)
	if err != nil {
		t.Fatalf("parse launch url: %v", err)
	}
	token := parsed.Query().Get("token")
	if token == "" {
		t.Fatalf("launch url has no token query parameter: %s", launchURL)
	}
	return token
}

// rawPostCasinoBootstrap posts a webhookauth.Inbound's body to the real
// bootstrap route with its own signature headers - a genuine net/http
// round trip against the httptest server, exactly as a real vendor's own
// bootstrap call would arrive.
func rawPostCasinoBootstrap(t *testing.T, srv *httptest.Server, tenantSlug string, inbound webhookauth.Inbound) *http.Response {
	t.Helper()
	return rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenantSlug+"/mock-casino/launch-bootstrap", inbound)
}

// TestCasinoBootstrap_MockVendorOverHTTP_FreshSuccess drives the full
// flow over real HTTP: player launch -> the MOCK vendor's own bootstrap
// client, signed and posted as a genuine HTTP request to the real route.
func TestCasinoBootstrap_MockVendorOverHTTP_FreshSuccess(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustEnableCasinoCapability(t, srv, pool, tenant)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10000)

	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	token := extractLaunchToken(t, launched.LaunchURL)

	in := mock.BootstrapPayload(tenant.ID, token, "req-http-fresh-1", game.ProviderGameID, "EUR", "real")
	resp := rawPostCasinoBootstrap(t, srv, tenant.Slug, in)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from the bootstrap route, got %d", resp.StatusCode)
	}
	var body map[string]any
	decodeBody(t, resp, &body)
	for _, key := range []string{"session_id", "player_ref", "provider_game_id", "asset_code", "mode"} {
		if _, ok := body[key]; !ok {
			t.Fatalf("expected the response to carry %q, got %v", key, body)
		}
	}
	if body["session_id"] != launched.SessionID {
		t.Fatalf("expected session_id %s, got %v", launched.SessionID, body["session_id"])
	}

	var status string
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM casino_launch_sessions WHERE id = $1`, launched.SessionID).Scan(&status)
	})
	if err != nil {
		t.Fatalf("read session status: %v", err)
	}
	if status != "consumed" {
		t.Fatalf("expected the session consumed, got %q", status)
	}
}

// TestCasinoBootstrap_UnsignedRequest_UniformUnauthorized proves an
// unsigned bootstrap request gets the same uniform 401 every other casino
// webhook pre-verification failure gets.
func TestCasinoBootstrap_UnsignedRequest_UniformUnauthorized(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)

	body := `{"launch_token":"x","request_id":"req-unsigned-1","provider_game_id":"g","asset_code":"EUR","mode":"real"}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/webhooks/casino/"+tenant.Slug+"/mock-casino/launch-bootstrap", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an unsigned bootstrap request, got %d", resp.StatusCode)
	}
}

// TestCasinoBootstrap_UnknownTenantSlug_UniformUnauthorized is F-3's own
// missing case: a genuinely-signed-shaped bootstrap request against a
// tenant slug that does not exist gets refused before ever reaching
// VerifyCallback/BootstrapLaunch, and with the SAME uniform 401 body as
// every other pre-verification failure - mirroring
// TestCasinoWebhook_AuthFailureLogging_AllowListOnly's own
// "tenant_unknown" case for the bet/win/rollback callback route, but for
// the bootstrap route specifically.
func TestCasinoBootstrap_UnknownTenantSlug_UniformUnauthorized(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	in := mock.BootstrapPayload(uuid.New(), "some-token", "req-f3-unknown-tenant-1", "game-1", "EUR", "real")
	resp := rawPostCasinoBootstrap(t, srv, "does-not-exist-slug", in)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an unknown tenant slug, got %d", resp.StatusCode)
	}
	apiErr := decodeAPIError(t, resp)
	if apiErr.Message != "callback rejected" {
		t.Fatalf("expected the uniform 'callback rejected' message, got %q", apiErr.Message)
	}
}

// TestCasinoBootstrap_RealProviderBetWorksAfterBootstrap is
// CAS-PLAY-BOOTSTRAP-1's own DoD item, over the REAL provider webhook
// route (postBet, via a genuinely-signed callback - newCasinoWebhookHandler),
// not the player-authenticated B2C "play simulation" convenience routes in
// casino_play_handlers.go.
//
// Finding, reported rather than silently worked around: the play-
// simulation routes (POST /v1/me/casino/sessions/{id}/wager and its
// win/rollback siblings) have their OWN, separate precondition -
// requireActiveUnexpiredSession, documented in casino_play_handlers.go as
// security review finding P2-2 - that accepts ONLY 'active', never
// 'consumed'. That is a deliberate, already-reviewed control (the comment
// explains why: a player driving their own session directly needs a
// handler-layer check postBet itself does not provide for a real
// provider). A session a real bootstrap has consumed is therefore NOT
// usable through those simulation routes at all - confirmed empirically
// (400 "session is not active") while writing this test, not asserted
// from reading the code alone. ADR 0103 never names casino_play_handlers.go,
// and widening a named, reviewed security gate is not something this
// workstream's own scope covers or that should happen without its own
// review - not done here. This is reported to the orchestrator as a
// genuine, newly-reachable interaction between B and the pre-existing
// simulation routes, for a decision outside this change.
//
// What IS unaffected, and is what this test proves: the REAL provider
// path (postBet via a signed callback) already accepts a 'consumed'
// session regardless of the original launch token's own expires_at (the
// CAS-SESSION-EXPIRY-1 fix, pinned independently at the domain layer by
// postbet_session_expiry_integration_test.go's
// TestReceiveCallback_ConsumedSessionAcceptsBetAfterTokenTTLExpires) - so
// once CAS-PLAY-BOOTSTRAP-1 lets a session genuinely reach 'consumed' at
// all outside a test, a real provider's own bet callback against it works,
// which is what this test demonstrates end to end over HTTP.
func TestCasinoBootstrap_RealProviderBetWorksAfterBootstrap(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustEnableCasinoCapability(t, srv, pool, tenant)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10000)

	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	token := extractLaunchToken(t, launched.LaunchURL)

	in := mock.BootstrapPayload(tenant.ID, token, "req-http-ttl-1", game.ProviderGameID, "EUR", "real")
	resp := rawPostCasinoBootstrap(t, srv, tenant.Slug, in)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from the bootstrap route, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	sessionID, err := uuid.Parse(launched.SessionID)
	if err != nil {
		t.Fatalf("parse session id: %v", err)
	}
	betPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "bet-after-bootstrap-1", "", "round-after-bootstrap-1", game.ProviderGameID, 500, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	betResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", betPayload)
	defer betResp.Body.Close()
	if betResp.StatusCode != http.StatusOK {
		var errBody map[string]any
		decodeBody(t, betResp, &errBody)
		t.Fatalf("expected the real provider bet to succeed on a bootstrapped (consumed) session, got %d: %v", betResp.StatusCode, errBody)
	}

	var status string
	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM casino_launch_sessions WHERE id = $1`, sessionID).Scan(&status)
	})
	if err != nil {
		t.Fatalf("read session status: %v", err)
	}
	if status != "consumed" {
		t.Fatalf("expected the session to remain consumed, got %q", status)
	}
}

// TestCasinoBootstrap_ConsumedSessionRefusedBySimulationWagerRoute is the
// security review's P2-2 pin (docs/plans/prh2-hardening-round/reviews/
// b-security.md - "Recommended pin (Low): a test that a consumed session
// is refused by the simulation wager route"), for the interaction
// TestCasinoBootstrap_RealProviderBetWorksAfterBootstrap's own doc comment
// already reported as a finding: a session a real bootstrap has consumed
// is refused by the pre-existing B2C "play simulation" routes
// (POST /v1/me/casino/sessions/{id}/wager), because
// requireActiveUnexpiredSession (casino_play_handlers.go, security finding
// P2-2) accepts only 'active', never 'consumed'. Both the architect/QA/POP
// review and security's own final ruling on P2-2 confirm this is INTENDED
// - the simulation routes are a player-driven stand-in for a vendor call,
// reachable only outside production, and widening them to accept
// 'consumed' would create a second, unsigned driver for the same round's
// money. This test exists so a future change that accidentally widens
// requireActiveUnexpiredSession is caught here, not discovered in
// production.
func TestCasinoBootstrap_ConsumedSessionRefusedBySimulationWagerRoute(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustEnableCasinoCapability(t, srv, pool, tenant)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10000)

	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	token := extractLaunchToken(t, launched.LaunchURL)

	// A real bootstrap genuinely consumes the session - not a shortcut, the
	// same HTTP round trip TestCasinoBootstrap_MockVendorOverHTTP_
	// FreshSuccess exercises.
	in := mock.BootstrapPayload(tenant.ID, token, "req-p2-2-pin-1", game.ProviderGameID, "EUR", "real")
	bootstrapResp := rawPostCasinoBootstrap(t, srv, tenant.Slug, in)
	defer bootstrapResp.Body.Close()
	if bootstrapResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 from the bootstrap route, got %d", bootstrapResp.StatusCode)
	}

	var status string
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM casino_launch_sessions WHERE id = $1`, launched.SessionID).Scan(&status)
	})
	if err != nil {
		t.Fatalf("read session status: %v", err)
	}
	if status != "consumed" {
		t.Fatalf("precondition failed: expected the session consumed before exercising the pin, got %q", status)
	}

	wagerResp := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/wager", player.Tokens.AccessToken, wagerBody(500))
	defer wagerResp.Body.Close()
	if wagerResp.StatusCode != http.StatusBadRequest {
		var body map[string]any
		decodeBody(t, wagerResp, &body)
		t.Fatalf("expected the simulation wager route to refuse a bootstrapped (consumed) session with 400, got %d: %v",
			wagerResp.StatusCode, body)
	}
}
