//go:build integration

// A3 pre-adapter fixes at the HTTP layer: CAS-BET-REQUIRES-BOOTSTRAP-1 (a
// signed webhook bet needs a bootstrapped session), CAS-GAME-KILL-BET-1 (a
// killed game declines new bets and still settles), CAS-PLAYER-REF-1 (the
// vendor is handed only the opaque reference; a raw account id in a callback
// is refused) and the MOCK play-simulation routes' stand-in for the vendor
// bootstrap (ADR 0048 amendment).
package httpserver

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
)

func a3SessionStatus(t *testing.T, pool *db.Pool, tenantID uuid.UUID, sessionID string) string {
	t.Helper()
	var status string
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM casino_launch_sessions WHERE id = $1`, sessionID).Scan(&status)
	}); err != nil {
		t.Fatalf("read session status: %v", err)
	}
	return status
}

func a3Count(t *testing.T, pool *db.Pool, tenantID uuid.UUID, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, args...).Scan(&n)
	}); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// A signed webhook bet on a launched-but-not-bootstrapped session is refused
// (400), posts nothing, and works once the REAL vendor bootstrap route has
// consumed the session.
func TestA3_HTTP_WebhookBetRequiresBootstrap(t *testing.T) {
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
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)

	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	session := uuid.MustParse(launched.SessionID)
	ref := mustCasinoPlayerRef(t, pool, tenant.ID, player.ID)

	bet := func(txID string) *http.Response {
		return rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino",
			mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, txID, "", "round-"+txID, game.ProviderGameID, 500, "EUR", casino.OutcomeSucceeded, "", ref, session))
	}
	resp := bet("a3-before-bootstrap")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a bet before the bootstrap must be refused with 400, got %d", resp.StatusCode)
	}
	if n := a3Count(t, pool, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE provider_tx_id = 'a3-before-bootstrap'`); n != 0 {
		t.Fatalf("nothing may post for a bet before the bootstrap, got %d", n)
	}

	token := extractLaunchToken(t, launched.LaunchURL)
	bs := rawPostCasinoBootstrap(t, srv, tenant.Slug, mock.BootstrapPayload(tenant.ID, token, "req-a3-bs-1", game.ProviderGameID, "EUR", "real"))
	_ = bs.Body.Close()
	if bs.StatusCode != http.StatusOK {
		t.Fatalf("bootstrap: %d", bs.StatusCode)
	}
	resp = bet("a3-after-bootstrap")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a bet after the bootstrap must succeed, got %d", resp.StatusCode)
	}
	if n := a3Count(t, pool, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE provider_tx_id = 'a3-after-bootstrap'`); n != 1 {
		t.Fatalf("expected the bet posted once, got %d", n)
	}

	// The vendor was launched with the opaque reference, never the account id.
	reqs := mock.LaunchRequests()
	if len(reqs) != 1 || reqs[0].PlayerRef != ref || reqs[0].PlayerRef == player.ID {
		t.Fatalf("the vendor must be launched with the opaque reference %s only, got %+v", ref, reqs)
	}
	if strings.Contains(launched.LaunchURL, player.ID.String()) {
		t.Fatalf("the launch URL must not carry the account id: %s", launched.LaunchURL)
	}
}

// A callback naming the raw account id as player_ref gets a deterministic 400
// and posts nothing.
func TestA3_HTTP_WebhookRawAccountIDAsPlayerRefIsRefused(t *testing.T) {
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
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	launched := mustLaunchAndConsumeCasinoGame(t, srv, pool, tenant.ID, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")

	resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino",
		mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "a3-raw-id", "", "round-a3-raw", game.ProviderGameID, 500, "EUR",
			casino.OutcomeSucceeded, "", player.ID, uuid.MustParse(launched.SessionID)))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a raw account id as player_ref must be refused with 400, got %d", resp.StatusCode)
	}
	if n := a3Count(t, pool, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE provider_tx_id = 'a3-raw-id'`); n != 0 {
		t.Fatalf("nothing may post, got %d", n)
	}
}

// A killed game declines a new webhook bet (200 / declined / game_unavailable),
// while the win for an already-open round still settles.
func TestA3_HTTP_KilledGameDeclinesNewBetsButSettlesExistingRounds(t *testing.T) {
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
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	launched := mustLaunchAndConsumeCasinoGame(t, srv, pool, tenant.ID, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	session := uuid.MustParse(launched.SessionID)
	ref := mustCasinoPlayerRef(t, pool, tenant.ID, player.ID)
	post := func(ev casino.CallbackEventType, txID, round string, amount int64, sess uuid.UUID) map[string]any {
		resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino",
			mock.CallbackPayload(tenant.ID, ev, txID, "", round, game.ProviderGameID, amount, "EUR", casino.OutcomeSucceeded, "", ref, sess))
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: expected 200, got %d", ev, txID, resp.StatusCode)
		}
		var body map[string]any
		decodeBody(t, resp, &body)
		return body
	}
	if b := post(casino.CallbackEventBet, "a3-kill-open", "round-kill-open", 500, session); b["outcome"] != "succeeded" {
		t.Fatalf("bet before the kill: %v", b)
	}

	if err := pool.WithPlatformAdmin(context.Background(), mustSeedPlatformAdminStaffPrincipal(t, pool), func(ctx context.Context, tx pgx.Tx) error {
		_, err := casino.UpsertGame(ctx, tx, casino.UpsertGameInput{
			ProviderID: game.ProviderID, ProviderGameID: game.ProviderGameID, Name: game.Name, GameType: game.GameType,
			SupportedAssets: game.SupportedAssets, Status: casino.GameStatusDisabled,
		})
		return err
	}); err != nil {
		t.Fatalf("kill the game: %v", err)
	}

	b := post(casino.CallbackEventBet, "a3-kill-new", "round-kill-new", 500, session)
	if b["outcome"] != "declined" || b["decline_reason"] != casino.DeclineReasonGameUnavailable {
		t.Fatalf("a new bet on a killed game must be declined/game_unavailable, got %v", b)
	}
	if n := a3Count(t, pool, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE provider_tx_id = 'a3-kill-new'`); n != 0 {
		t.Fatalf("the declined bet must not post, got %d", n)
	}
	if w := post(casino.CallbackEventWin, "a3-kill-win", "round-kill-open", 900, uuid.Nil); w["outcome"] != "succeeded" {
		t.Fatalf("the win for an already-open round must settle on a killed game: %v", w)
	}
	// A redelivered win is the original result (CAS-WIN-IDEMP-1), 200 not 400.
	if w := post(casino.CallbackEventWin, "a3-kill-win", "round-kill-open", 900, uuid.Nil); w["outcome"] != "succeeded" {
		t.Fatalf("a redelivered win must return the original result: %v", w)
	}
	if n := a3Count(t, pool, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE provider_tx_id = 'a3-kill-win'`); n != 1 {
		t.Fatalf("expected the win posted exactly once, got %d", n)
	}
}

// The MOCK play-simulation routes stand in for the vendor bootstrap: the first
// wager on a launched session consumes it (audited), later calls reuse it,
// concurrent first wagers both succeed, and the session ends up 'consumed'
// with no vendor bootstrap record (so the P2-2 pin - never drive a vendor's
// session - still holds).
func TestA3_HTTP_SimulationConsumesSessionAsTheMockVendor(t *testing.T) {
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
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 100_000)

	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	if s := a3SessionStatus(t, pool, tenant.ID, launched.SessionID); s != "active" {
		t.Fatalf("a launched session starts active, got %q", s)
	}

	// Two concurrent first wagers: both succeed, the loser of the consume CAS
	// is benign.
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/wager", player.Tokens.AccessToken, wagerBody(100))
			defer func() { _ = resp.Body.Close() }()
			codes[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Fatalf("concurrent first wagers must both succeed, got %v", codes)
	}
	if s := a3SessionStatus(t, pool, tenant.ID, launched.SessionID); s != "consumed" {
		t.Fatalf("the simulation must have consumed the session, got %q", s)
	}
	resp := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/wager", player.Tokens.AccessToken, wagerBody(100))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a later wager on the simulation-consumed session must succeed, got %d", resp.StatusCode)
	}
	if n := a3Count(t, pool, tenant.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'casino_play_simulation.session_consumed'`, tenant.ID); n != 1 {
		t.Fatalf("expected exactly one session_consumed audit row, got %d", n)
	}
	if n := a3Count(t, pool, tenant.ID, `SELECT count(*) FROM casino_launch_bootstraps WHERE launch_session_id = $1`, launched.SessionID); n != 0 {
		t.Fatalf("the simulation must not write a vendor bootstrap record, got %d", n)
	}

	// The simulation signs with the opaque reference only (it is the mock
	// vendor): a launch request recorded the reference, not the account id.
	reqs := mock.LaunchRequests()
	if len(reqs) != 1 || reqs[0].PlayerRef == player.ID || reqs[0].PlayerRef == uuid.Nil {
		t.Fatalf("expected one launch with the opaque reference only, got %+v", reqs)
	}

	// A session ended by revoke is refused by the handler for every action as
	// before - win and rollback too, which postWin/postRollback themselves
	// would not refuse (they never consult the session).
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := casino.RevokeLaunchSession(ctx, tx, uuid.MustParse(launched.SessionID))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	txsBefore := a3Count(t, pool, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE provider_id = 'mock-casino'`)
	for _, path := range []string{"/wager", "/win"} {
		body := wagerBody(100)
		if path == "/win" {
			body = winBody(100)
		}
		resp = postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+path, player.Tokens.AccessToken, body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("%s on a revoked session must be refused by the handler with 400, got %d", path, resp.StatusCode)
		}
	}
	if txsAfter := a3Count(t, pool, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE provider_id = 'mock-casino'`); txsAfter != txsBefore {
		t.Fatalf("a refused simulation call posted: %d -> %d", txsBefore, txsAfter)
	}

	// A never-consumed session that is revoked is refused as before.
	launched2 := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := casino.RevokeLaunchSession(ctx, tx, uuid.MustParse(launched2.SessionID))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	resp = postJSON(t, srv, "/v1/me/casino/sessions/"+launched2.SessionID+"/wager", player.Tokens.AccessToken, wagerBody(100))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a revoked session must still be refused by the simulation route, got %d", resp.StatusCode)
	}
}
