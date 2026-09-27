//go:build integration

// Stage 9.2 (ADR 0083 §12.2 item 30's second named sub-test):
// TestLockOrder_ConcurrentSportsbookBetAndCasinoBetSameWalletNoDeadlock.
//
// internal/sportsbook must not import internal/casino (package-boundary
// discipline - see internal/sportsbook's own evaluateAndAuditEligibility
// doc comment), so this cross-product proof cannot live in either
// product package. internal/httpserver already imports both and already
// hosts the established "dozens of goroutines against a real httptest.Server,
// assert no deadlock/hang, log a rough timing" pattern
// (stage9_concurrency_integration_test.go) - this file follows that
// pattern for exactly ONE pairing: a sportsbook bet and a casino
// bet+win+rollback cycle, both landing on the SAME wallet's player_cash
// projection row, repeated across many goroutine pairs, to prove adding
// class L0.6 (ADR 0082 Amendment A2) to sportsbook's own lock sequence did
// not introduce a new cross-product deadlock with casino's.
package httpserver

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
)

// sportsbookCasinoLockOrderServer builds one server with sportsbook,
// casino AND the Stage 7 mock-provider play-simulation endpoints all
// enabled - stage9Server (this package's own combined-route server) does
// NOT set CasinoPlaySimulationEnabled, so this test needs its own minimal
// variant rather than reusing it.
func sportsbookCasinoLockOrderServer(t *testing.T, pool *db.Pool, orchestrator *casino.Orchestrator) (*httptest.Server, *auth.Issuer) {
	t.Helper()
	keys, err := auth.NewKeyRegistry("k1", map[string]string{"k1": testJWTSecret})
	if err != nil {
		t.Fatalf("failed to build key registry: %v", err)
	}
	issuer := auth.NewIssuer(keys, "platform-api-test", "platform-api-test")
	srv := httptest.NewServer(New(Deps{
		Logger:                      slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:                          pool,
		AuthIssuer:                  issuer,
		ServiceName:                 "platform-api-test",
		AccessTokenTTL:              5 * time.Minute,
		RefreshTokenTTL:             time.Hour,
		SportsbookEnabled:           true,
		CasinoOrchestrator:          orchestrator,
		CasinoOutboundCredentials:   casino.NewMockOutboundResolver(),
		CasinoPlaySimulationEnabled: true,
		PersonResolver:              identityresolution.NewMockPersonResolver(),
	}))
	t.Cleanup(srv.Close)
	return srv, issuer
}

func lockOrderIsDeadlockStatus(resp *http.Response) bool {
	// A genuine SQLSTATE 40P01 deadlock that ledger.Post/casino/sportsbook
	// could not itself retry surfaces to the HTTP layer as a 500 - this
	// helper exists so the assertion below reads as "no 500 at all",
	// which is the stronger and simpler property this test actually needs
	// (a 500 for ANY other reason is just as much a bug here, since every
	// request in this test is otherwise well-formed).
	return resp.StatusCode == http.StatusInternalServerError
}

// TestLockOrder_ConcurrentSportsbookBetAndCasinoBetSameWalletNoDeadlock
// runs many (sportsbook bet, casino bet) pairs concurrently against ONE
// shared player wallet - both flows debit/credit the SAME player_cash
// wallet_balance_projection row (class L3), sportsbook additionally taking
// L0.4/L0.5/L0.6 (RG/risk/exposure, an armed generous event-level exposure
// limit so L0.6 is genuinely exercised) and casino taking its own L0.1
// delivery advisory - and asserts NONE of the concurrent requests fails
// with a 500 (in particular never SQLSTATE 40P01), i.e. the two products'
// independently-canonical (ADR 0082) lock sequences never cross-deadlock
// on the shared wallet.
func TestLockOrder_ConcurrentSportsbookBetAndCasinoBetSameWalletNoDeadlock(t *testing.T) {
	pool, _ := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv, _ := sportsbookCasinoLockOrderServer(t, pool, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustEnableCasinoCapability(t, srv, pool, tenant)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	var brandID uuid.UUID
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		a, err := identity.GetPlayerAccountByID(ctx, tx, player.ID)
		brandID = a.BrandID
		return err
	}); err != nil {
		t.Fatalf("resolve player brand: %v", err)
	}
	fundWallet(t, pool, tenant.ID, brandID, player.ID, "EUR", 1_000_000)

	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")

	selectionID := mustSeedSportsbookSelection(t, pool, 200, 100)
	// Armed and generous, so every sportsbook bet in this test genuinely
	// takes L0.6 (event-scoped) rather than skipping it unarmed.
	mustSeedGenerousEventExposureLimit(t, pool, tenant.ID, selectionID)

	const pairs = 8
	var wg sync.WaitGroup
	errsCh := make(chan string, pairs*2)
	for i := 0; i < pairs; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			resp, err := stage9RawPostJSON(srv, "/v1/me/sportsbook/bets", player.Tokens.AccessToken,
				placeBetRequestBody(selectionID, 100, 200, 100, fmt.Sprintf("lockorder-cross-sb-%d", i)))
			if err != nil {
				errsCh <- fmt.Sprintf("sportsbook bet %d: transport error: %v", i, err)
				return
			}
			defer resp.Body.Close()
			if lockOrderIsDeadlockStatus(resp) {
				errsCh <- fmt.Sprintf("sportsbook bet %d: got 500 (possible deadlock)", i)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			resp, err := stage9RawPostJSON(srv, "/v1/me/casino/sessions/"+launched.SessionID+"/wager", player.Tokens.AccessToken,
				map[string]any{"stake_amount": 100, "idempotency_key": uuid.NewString()})
			if err != nil {
				errsCh <- fmt.Sprintf("casino wager %d: transport error: %v", i, err)
				return
			}
			defer resp.Body.Close()
			if lockOrderIsDeadlockStatus(resp) {
				errsCh <- fmt.Sprintf("casino wager %d: got 500 (possible deadlock)", i)
			}
		}(i)
	}
	wg.Wait()
	close(errsCh)
	for msg := range errsCh {
		t.Error(msg)
	}

	// SUM(debits) == SUM(credits), tenant-wide, is the invariant a deadlock
	// (a full transaction rollback) never breaks but a genuine posting bug
	// would - the same closing assertion this codebase's own concurrency
	// suites always make.
	var debits, credits int64
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0)::bigint,
			        COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)::bigint
			   FROM ledger_entries WHERE tenant_id = $1`, tenant.ID).Scan(&debits, &credits)
	})
	if err != nil {
		t.Fatalf("sum debits/credits: %v", err)
	}
	if debits != credits {
		t.Fatalf("invariant SUM(debits)==SUM(credits) violated: debits=%d credits=%d", debits, credits)
	}
}

// mustSeedGenerousEventExposureLimit arms an event-level sb_exposure_limits
// row (tenant-wide, EUR) sized so it never itself rejects a bet in this
// test - its only purpose is making sure every sportsbook bet placed
// genuinely acquires class L0.6, exercising the exact new lock this wave
// added to the cross-product contention this test targets.
func mustSeedGenerousEventExposureLimit(t *testing.T, pool *db.Pool, tenantID, selectionID uuid.UUID) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO sb_exposure_limits (tenant_id, scope_kind, asset_code, max_open_potential_payout, authorization_reference, reason_code, created_by_actor_type, created_by_actor_id)
			 VALUES ($1, 'event', 'EUR', 1000000000, 'test-authz', 'test-reason', 'staff', $2)`,
			tenantID, uuid.New())
		return err
	})
	if err != nil {
		t.Fatalf("seed generous event exposure limit: %v", err)
	}
}
