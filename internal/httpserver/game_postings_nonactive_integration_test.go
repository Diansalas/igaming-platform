//go:build integration

// R3-GAME-POSTINGS-NONACTIVE-1 (owner decision 2026-10-05, ADR 0095 section
// 40.5), HTTP layer: a player or staff token that is still valid after the
// tenant became suspended/closed (auth does not refuse it, ADR 0107) cannot
// create a NEW gameplay posting. The server runs on the RUNTIME role pool
// (asserted NOT rolsuper AND NOT rolbypassrls); tenants and fixtures are
// seeded through the owner pool. The public casino WEBHOOK route already
// refuses a non-active tenant with the uniform pre-verification 401 (unchanged,
// covered by TestCasinoWebhook_UnknownAndSuspendedTenantIdenticalNotFound).
package httpserver

import (
	"context"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

func gateRuntimePool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_RUNTIME_DATABASE_URL must be set: these tests must run as the runtime role")
	}
	rt, err := db.Connect(context.Background(), url, 10, 5*time.Second)
	if err != nil {
		t.Fatalf("connect as runtime role: %v", err)
	}
	t.Cleanup(rt.Close)
	var super, bypass bool
	if err := rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass)
	}); err != nil {
		t.Fatalf("read runtime role flags: %v", err)
	}
	if super || bypass {
		t.Fatalf("runtime role must be NOT rolsuper and NOT rolbypassrls, got super=%v bypassrls=%v", super, bypass)
	}
	return rt
}

// gateSetTenantStatus sets a tenant status. Closing a tenant that still has an
// open sportsbook bet is refused by migration 0121 (Q-GP-1); the R3 tests need
// that state (a tenant closed before the guard existed, or by a privileged
// path), so for 'closed' the status-change trigger is disabled inside this one
// owner transaction only (transactional DDL; no other session sees it disabled).
func gateSetTenantStatus(t *testing.T, owner *db.Pool, tenantID uuid.UUID, status string) {
	t.Helper()
	err := owner.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		if status == "closed" {
			if _, err := tx.Exec(ctx, `ALTER TABLE tenants DISABLE TRIGGER tenants_status_change_gate`); err != nil {
				return err
			}
			defer func() { _, _ = tx.Exec(ctx, `ALTER TABLE tenants ENABLE TRIGGER tenants_status_change_gate`) }()
		}
		_, err := tx.Exec(ctx, `UPDATE tenants SET status = $1 WHERE id = $2`, status, tenantID)
		return err
	})
	if err != nil {
		t.Fatalf("set tenant status: %v", err)
	}
}

func gateCount(t *testing.T, owner *db.Pool, tenantID uuid.UUID, query string, args ...any) int {
	t.Helper()
	var n int
	if err := owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, args...).Scan(&n)
	}); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func TestGamePostingsNonActive_CasinoPlayRoutesRefuseAndRecord(t *testing.T) {
	owner, issuer := testEnv(t)
	rt := gateRuntimePool(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, rt, issuer, orchestrator)

	for _, status := range []string{"suspended", "closed"} {
		t.Run(status, func(t *testing.T) {
			tenant := mustCreateTenant(t, owner)
			brand := mustCreateBrand(t, owner, tenant)
			player := mustRegisterPlayer(t, srv, brand.Slug)
			mustActivatePlayer(t, owner, tenant.ID, player.ID)
			fundWallet(t, owner, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
			game := mustSeedCasinoGame(t, owner, "mock-casino", "EUR")
			mustEnableCasinoGameForTenant(t, owner, tenant.ID, game.ID)
			mustEnableCasinoCapability(t, srv, owner, tenant)
			launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")

			// One bet posted while ACTIVE (an open round at closure).
			wagerResp := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/wager", player.Tokens.AccessToken, wagerBody(1_000))
			if wagerResp.StatusCode != http.StatusOK {
				t.Fatalf("active wager: %d", wagerResp.StatusCode)
			}
			var wagerOut map[string]any
			decodeBody(t, wagerResp, &wagerOut)
			betRef, _ := wagerOut["provider_tx_id"].(string)
			gateSetTenantStatus(t, owner, tenant.ID, status)
			txs0 := gateCount(t, owner, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenant.ID)

			wager := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/wager", player.Tokens.AccessToken, wagerBody(500))
			if wager.StatusCode != http.StatusConflict {
				t.Fatalf("a new wager on a %s tenant must be a deterministic 409, got %d", status, wager.StatusCode)
			}
			win := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/win", player.Tokens.AccessToken, winBody(2_000))
			if win.StatusCode != http.StatusConflict {
				t.Fatalf("a new win on a %s tenant must be a deterministic 409, got %d", status, win.StatusCode)
			}
			if got := gateCount(t, owner, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenant.ID); got != txs0 {
				t.Fatalf("refused requests changed the ledger: %d -> %d", txs0, got)
			}
			if got := gateCount(t, owner, tenant.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'casino_callback.rejected_tenant_not_active'`, tenant.ID); got != 2 {
				t.Fatalf("expected 2 durable refusal audit records, got %d", got)
			}

			// Q-GP-5 (2026-10-06): the rollback of the posted bet is a terminal
			// stake return and is allowed on the non-active tenant, once.
			rb := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/rollback", player.Tokens.AccessToken,
				map[string]string{"original_provider_tx_id": betRef})
			if rb.StatusCode != http.StatusOK {
				t.Fatalf("a stake return on a %s tenant must be 200, got %d", status, rb.StatusCode)
			}
			_ = rb.Body.Close()
			if got := gateCount(t, owner, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'casino_rollback'`, tenant.ID); got != 1 {
				t.Fatalf("expected exactly one casino_rollback posting, got %d", got)
			}
			// A second rollback (a distinct reference minted by the route) is refused.
			rb2 := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/rollback", player.Tokens.AccessToken,
				map[string]string{"original_provider_tx_id": betRef})
			if rb2.StatusCode == http.StatusOK {
				t.Fatalf("a second stake return of the same bet must be refused")
			}
			_ = rb2.Body.Close()
			if got := gateCount(t, owner, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'casino_rollback'`, tenant.ID); got != 1 {
				t.Fatalf("the refused second return posted: %d rollbacks", got)
			}
		})
	}
}

func TestGamePostingsNonActive_SportsbookRoutesRefuseAndRecord(t *testing.T) {
	owner, issuer := testEnv(t)
	rt := gateRuntimePool(t)
	srv := newSettlementTestServer(t, rt, issuer, true)

	tenant := mustCreateTenant(t, owner)
	brand := mustCreateBrand(t, owner, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	bet := mustPlaceBetHTTP(t, srv, owner, tenant, brand, player.Tokens.AccessToken, player.ID, 1000)
	staff := mustCreateStaff(t, owner, tenant.ID, identity.StaffRoleRiskManager, "risk-password-1")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "risk-password-1")

	gateSetTenantStatus(t, owner, tenant.ID, "closed")
	txs0 := gateCount(t, owner, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenant.ID)

	// Staff simulation of a settlement with payout: 409, recorded.
	settle := mustSimulateRequest(t, srv, bet.ID, tokens.AccessToken, "gate-"+uuid.NewString(),
		`{"event_type":"settle","generation":1,"outcome":"won","payout_amount":2000,"asset_code":"EUR"}`)
	if settle.StatusCode != http.StatusConflict {
		t.Fatalf("a settlement with payout on a closed tenant must be 409, got %d", settle.StatusCode)
	}
	_ = settle.Body.Close()
	if got := gateCount(t, owner, tenant.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'sportsbook_bet.settlement_rejected' AND metadata->>'rejection_code' = 'SETTLEMENT_TENANT_NOT_ACTIVE'`, tenant.ID); got != 1 {
		t.Fatalf("expected 1 durable rejection audit record, got %d", got)
	}

	// A player's NEW bet: a deterministic decline (200 shape), nothing posted.
	selectionID := mustSeedSportsbookSelection(t, owner, 200, 100)
	resp := postJSON(t, srv, "/v1/me/sportsbook/bets", player.Tokens.AccessToken,
		placeBetRequestBody(selectionID, 500, 200, 100, "gate-new-"+uuid.NewString()))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a new bet on a closed tenant must be a 200 decline, got %d", resp.StatusCode)
	}
	var declined placeBetResponse
	decodeBody(t, resp, &declined)
	if declined.Accepted || declined.RejectionCategory != "tenant_not_active" {
		t.Fatalf("expected a tenant_not_active decline, got %+v", declined)
	}
	if got := gateCount(t, owner, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenant.ID); got != txs0 {
		t.Fatalf("refused requests changed the ledger: %d -> %d", txs0, got)
	}

	// Q-GP-5 (2026-10-06): the staff void of the open bet is a terminal stake
	// return and is allowed on the closed tenant: 200, one void posting.
	void := mustSimulateRequest(t, srv, bet.ID, tokens.AccessToken, "gate-"+uuid.NewString(),
		`{"event_type":"void","void_reason":"market_cancelled"}`)
	if void.StatusCode != http.StatusOK {
		t.Fatalf("a void of an open bet on a closed tenant must be 200, got %d", void.StatusCode)
	}
	_ = void.Body.Close()
	if got := gateCount(t, owner, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'sportsbook_void'`, tenant.ID); got != 1 {
		t.Fatalf("expected exactly one void posting, got %d", got)
	}
}

// TestGamePostingsNonActive_WebhookRaceWithClosure is the only way a PUBLIC
// webhook request reaches the in-transaction refusal: the route's active check
// (preamble) read the tenant as active, then the tenant is closed before the
// domain transaction posts. Two real connections: the closure is held
// uncommitted while the webhook request arrives; the request waits on the
// status gate, then must be refused with a deterministic 409, a durable audit
// record and no ledger row.
func TestGamePostingsNonActive_WebhookRaceWithClosure(t *testing.T) {
	owner, issuer := testEnv(t)
	rt := gateRuntimePool(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, rt, issuer, orchestrator)

	tenant := mustCreateTenant(t, owner)
	brand := mustCreateBrand(t, owner, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, owner, tenant.ID, player.ID)
	fundWallet(t, owner, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	game := mustSeedCasinoGame(t, owner, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, owner, tenant.ID, game.ID)
	mustEnableCasinoCapability(t, srv, owner, tenant)
	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	payload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "bet-webhook-race", "", "round-webhook-race", game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, uuid.MustParse(launched.SessionID))

	updated, release, closerDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var once sync.Once
	releaseOnce := func() { once.Do(func() { close(release) }) }
	t.Cleanup(releaseOnce)
	go func() {
		closerDone <- owner.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE tenants SET status = 'closed' WHERE id = $1`, tenant.ID); err != nil {
				return err
			}
			close(updated)
			<-release
			return nil
		})
	}()
	<-updated

	status := make(chan int, 1)
	go func() {
		resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", payload)
		_ = resp.Body.Close()
		status <- resp.StatusCode
	}()
	select {
	case code := <-status:
		t.Fatalf("the webhook must wait for the in-flight closure at the status gate, answered %d early", code)
	case <-time.After(700 * time.Millisecond):
	}
	releaseOnce()
	if err := <-closerDone; err != nil {
		t.Fatalf("closure: %v", err)
	}
	if code := <-status; code != http.StatusConflict {
		t.Fatalf("a webhook racing the closure must be refused with a deterministic 409, got %d", code)
	}
	if got := gateCount(t, owner, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE provider_id = 'mock-casino' AND provider_tx_id = 'bet-webhook-race'`); got != 0 {
		t.Fatalf("the refused bet must not be posted, found %d", got)
	}
	if got := gateCount(t, owner, tenant.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'casino_callback.rejected_tenant_not_active'`, tenant.ID); got != 1 {
		t.Fatalf("expected one durable refusal audit record, got %d", got)
	}
}
