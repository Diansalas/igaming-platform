//go:build integration

// A3 pre-adapter fixes (docs/plans/platform-completion-audit-2026-10-14.md
// A3): CAS-GAME-KILL-BET-1, CAS-BET-REQUIRES-BOOTSTRAP-1 and
// CAS-WIN-IDEMP-1. Written before the production change (red first).
//
// Statuses that block a NEW bet (postBet), documented once here and pinned
// by the tests below:
//
//	casino_games.status != 'active'                           -> declined "game_unavailable"
//	no casino_game_availability row for the session's brand   -> declined (fail closed, like launch)
//	most-specific availability row (brand, else tenant-wide) enabled = false -> declined
//	session status != 'consumed' (active/expired/revoked/unknown) -> ErrLaunchSessionRequired
//
// Settlement of exposure that already exists (win, rollback of a bet, the
// replay of an already-posted bet) is NEVER blocked by any of these.
package casino

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

type a3Env struct {
	pool      *db.Pool
	f         casinoFixture
	provider  *MockCasinoProvider
	orch      *Orchestrator
	game      Game
	sessionID uuid.UUID
}

// a3SetupConsumed seeds a funded fixture, an enabled game, a capability and a
// session in the 'consumed' state (the post-bootstrap state ADR 0103 defines).
func a3SetupConsumed(t *testing.T, funds int64) *a3Env {
	t.Helper()
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, funds)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	e := &a3Env{pool: pool, f: f, provider: provider, game: game,
		orch: NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))}
	e.sessionID = a3Session(t, e, true)
	return e
}

// a3Session mints a session for e.game; consume=true runs the single-use
// token CAS (ResolveLaunchToken, the test-only stand-in for the vendor
// bootstrap), consume=false leaves it 'active'.
func a3Session(t *testing.T, e *a3Env, consume bool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	var token string
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		s, tok, err := CreateLaunchSession(ctx, tx, CreateLaunchSessionParams{
			TenantID: e.f.tenantID, BrandID: e.f.brandID, PlayerAccountID: e.f.playerAccountID, WalletID: e.f.walletID,
			GameID: e.game.ID, ProviderID: "mock-casino", ProviderGameID: e.game.ProviderGameID,
			AssetCode: "EUR", Mode: ModeReal,
		})
		id, token = s.ID, tok
		return err
	}); err != nil {
		t.Fatalf("mint session: %v", err)
	}
	if consume {
		if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := ResolveLaunchToken(ctx, tx, token)
			return err
		}); err != nil {
			t.Fatalf("consume session: %v", err)
		}
	}
	return id
}

func (e *a3Env) receive(t *testing.T, eventType CallbackEventType, txID, origTxID, round string, amount int64, sessionID uuid.UUID) (ReceiveCallbackResult, error) {
	t.Helper()
	outcome := OutcomeSucceeded
	if eventType == CallbackEventRollback {
		outcome = ""
	}
	payload := e.provider.CallbackPayload(e.f.tenantID, eventType, txID, origTxID, round, "game-1", amount, "EUR", outcome, "", e.f.playerRef, sessionID)
	var res ReceiveCallbackResult
	err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = e.orch.receiveCallbackInTx(ctx, tx, e.f.tenantID, "mock-casino", payload)
		return err
	})
	return res, err
}

func (e *a3Env) countTx(t *testing.T, providerTxID string) int {
	t.Helper()
	var n int
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND provider_id = 'mock-casino' AND provider_tx_id = $2`,
			e.f.tenantID, providerTxID).Scan(&n)
	}); err != nil {
		t.Fatalf("count ledger transactions: %v", err)
	}
	return n
}

func (e *a3Env) countAudit(t *testing.T, action string) int {
	t.Helper()
	var n int
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2`, e.f.tenantID, action).Scan(&n)
	}); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

func (e *a3Env) setGameStatus(t *testing.T, status GameStatus) {
	t.Helper()
	if err := e.pool.WithPlatformAdmin(context.Background(), seedPlatformAdminStaffPrincipal(t, e.pool), func(ctx context.Context, tx pgx.Tx) error {
		_, err := UpsertGame(ctx, tx, UpsertGameInput{
			ProviderID: e.game.ProviderID, ProviderGameID: e.game.ProviderGameID, Name: e.game.Name, GameType: e.game.GameType,
			SupportedAssets: e.game.SupportedAssets, Status: status,
		})
		return err
	}); err != nil {
		t.Fatalf("set game status %s: %v", status, err)
	}
}

func (e *a3Env) setAvailability(t *testing.T, brandID *uuid.UUID, enabled bool) {
	t.Helper()
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetGameAvailability(ctx, tx, e.f.tenantID, brandID, e.game.ID, enabled)
		return err
	}); err != nil {
		t.Fatalf("set availability: %v", err)
	}
}

func (e *a3Env) requireBalance(t *testing.T, want int64) {
	t.Helper()
	if got := cashBalance(t, e.pool, e.f); got != want {
		t.Fatalf("cash balance: want %d, got %d", want, got)
	}
	if d, c := sumDebitsCredits(t, e.pool, e.f.tenantID); d != c {
		t.Fatalf("ledger invariant violated: debits=%d credits=%d", d, c)
	}
}

// ---- CAS-GAME-KILL-BET-1 ----

func TestA3_GameKill_DisabledGameDeclinesNewBet_ButSettlementAndReplayStillWork(t *testing.T) {
	e := a3SetupConsumed(t, 10000)

	// Exposure that exists BEFORE the kill: two open rounds.
	if r, err := e.receive(t, CallbackEventBet, "bet-A", "", "round-A", 1000, e.sessionID); err != nil || r.Outcome != OutcomeSucceeded {
		t.Fatalf("bet A: %+v %v", r, err)
	}
	if r, err := e.receive(t, CallbackEventBet, "bet-B", "", "round-B", 500, e.sessionID); err != nil || r.Outcome != OutcomeSucceeded {
		t.Fatalf("bet B: %+v %v", r, err)
	}
	e.requireBalance(t, 8500)

	e.setGameStatus(t, GameStatusDisabled)

	// NEW exposure is refused: a deterministic decline, nothing posted.
	r, err := e.receive(t, CallbackEventBet, "bet-new", "", "round-new", 700, e.sessionID)
	if err != nil {
		t.Fatalf("a new bet on a killed game must be a decline, not an error: %v", err)
	}
	if r.Outcome != OutcomeDeclined || r.DeclineReason != "game_unavailable" || r.LedgerTransactionID != nil {
		t.Fatalf("expected declined/game_unavailable with no ledger tx, got %+v", r)
	}
	if n := e.countTx(t, "bet-new"); n != 0 {
		t.Fatalf("expected no ledger transaction for the declined bet, got %d", n)
	}
	e.requireBalance(t, 8500)
	if n := e.countAudit(t, "casino_bet.denied_game_unavailable"); n != 1 {
		t.Fatalf("expected exactly one denial audit row, got %d", n)
	}

	// A replay of an already-posted bet is not new exposure.
	if r, err := e.receive(t, CallbackEventBet, "bet-A", "", "round-A", 1000, e.sessionID); err != nil || r.Outcome != OutcomeSucceeded || !r.Replayed {
		t.Fatalf("replay of a posted bet on a killed game must return the original: %+v %v", r, err)
	}
	e.requireBalance(t, 8500)

	// Settlement of existing exposure is NEVER blocked: a win and a rollback.
	if r, err := e.receive(t, CallbackEventWin, "win-A", "", "round-A", 2000, uuid.Nil); err != nil || r.Outcome != OutcomeSucceeded {
		t.Fatalf("win for existing exposure on a killed game must settle: %+v %v", r, err)
	}
	e.requireBalance(t, 10500)
	if r, err := e.receive(t, CallbackEventRollback, "rb-B", "bet-B", "round-B", 0, uuid.Nil); err != nil || r.Outcome != OutcomeSucceeded {
		t.Fatalf("rollback of existing exposure on a killed game must settle: %+v %v", r, err)
	}
	e.requireBalance(t, 11000)
}

func TestA3_GameKill_TenantAndBrandAvailabilityGateNewBets(t *testing.T) {
	e := a3SetupConsumed(t, 10000)
	brand := e.f.brandID

	// Tenant-wide opt-out blocks.
	e.setAvailability(t, nil, false)
	if r, err := e.receive(t, CallbackEventBet, "bet-t1", "", "round-t1", 100, e.sessionID); err != nil || r.Outcome != OutcomeDeclined || r.DeclineReason != "game_unavailable" {
		t.Fatalf("tenant-wide disabled: %+v %v", r, err)
	}
	// Most-specific wins: a brand row enabling it re-opens it.
	e.setAvailability(t, &brand, true)
	if r, err := e.receive(t, CallbackEventBet, "bet-t2", "", "round-t2", 100, e.sessionID); err != nil || r.Outcome != OutcomeSucceeded {
		t.Fatalf("brand row enabled must win over tenant-wide disabled: %+v %v", r, err)
	}
	// A brand opt-out blocks even when the tenant-wide row is enabled.
	e.setAvailability(t, nil, true)
	e.setAvailability(t, &brand, false)
	if r, err := e.receive(t, CallbackEventBet, "bet-t3", "", "round-t3", 100, e.sessionID); err != nil || r.Outcome != OutcomeDeclined || r.DeclineReason != "game_unavailable" {
		t.Fatalf("brand disabled: %+v %v", r, err)
	}
	if n := e.countTx(t, "bet-t1") + e.countTx(t, "bet-t3"); n != 0 {
		t.Fatalf("declined bets must not post, got %d", n)
	}
	// The earlier posted bet still settles with the game now disabled for the brand.
	if r, err := e.receive(t, CallbackEventWin, "win-t2", "", "round-t2", 300, uuid.Nil); err != nil || r.Outcome != OutcomeSucceeded {
		t.Fatalf("win must settle regardless of availability: %+v %v", r, err)
	}
}

func TestA3_GameKill_NoAvailabilityRowFailsClosed(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	game := seedGame(t, pool, "mock-casino", "EUR") // never enabled for the tenant
	e := &a3Env{pool: pool, f: f, provider: provider, game: game,
		orch: NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))}
	e.sessionID = a3Session(t, e, true)
	r, err := e.receive(t, CallbackEventBet, "bet-noavail", "", "round-noavail", 100, e.sessionID)
	if err != nil || r.Outcome != OutcomeDeclined || r.DeclineReason != "game_unavailable" {
		t.Fatalf("a game with no availability row must not accept a new bet: %+v %v", r, err)
	}
	e.requireBalance(t, 5000)
}

// ---- CAS-BET-REQUIRES-BOOTSTRAP-1 ----

func TestA3_BetRequiresConsumedSession(t *testing.T) {
	e := a3SetupConsumed(t, 10000)
	active := a3Session(t, e, false)

	_, err := e.receive(t, CallbackEventBet, "bet-active", "", "round-active", 100, active)
	if !errors.Is(err, ErrLaunchSessionRequired) {
		t.Fatalf("a bet on an unconsumed (active) session must be refused with ErrLaunchSessionRequired, got %v", err)
	}
	if n := e.countTx(t, "bet-active"); n != 0 {
		t.Fatalf("nothing may post for a bet on an active session, got %d", n)
	}

	// A consumed session that is then revoked refuses new bets...
	if r, err := e.receive(t, CallbackEventBet, "bet-ok", "", "round-ok", 100, e.sessionID); err != nil || r.Outcome != OutcomeSucceeded {
		t.Fatalf("bet on consumed session: %+v %v", r, err)
	}
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, revoked, err := RevokeLaunchSession(ctx, tx, e.sessionID)
		if err == nil && !revoked {
			t.Fatalf("expected the consumed session to be revoked")
		}
		return err
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := e.receive(t, CallbackEventBet, "bet-revoked", "", "round-revoked", 100, e.sessionID); !errors.Is(err, ErrLaunchSessionRequired) {
		t.Fatalf("a bet on a revoked session must be refused, got %v", err)
	}
	// ...but the already-open round still settles.
	if r, err := e.receive(t, CallbackEventWin, "win-ok", "", "round-ok", 250, uuid.Nil); err != nil || r.Outcome != OutcomeSucceeded {
		t.Fatalf("win after revoke must settle: %+v %v", r, err)
	}
	e.requireBalance(t, 10150)
}

// ---- CAS-WIN-IDEMP-1 ----

func TestA3_WinRedelivery_ReturnsOriginal_NoSecondPosting(t *testing.T) {
	e := a3SetupConsumed(t, 10000)
	if _, err := e.receive(t, CallbackEventBet, "bet-1", "", "round-1", 1000, e.sessionID); err != nil {
		t.Fatalf("bet: %v", err)
	}
	first, err := e.receive(t, CallbackEventWin, "win-1", "", "round-1", 2500, uuid.Nil)
	if err != nil || first.Outcome != OutcomeSucceeded || first.Replayed {
		t.Fatalf("first win: %+v %v", first, err)
	}
	e.requireBalance(t, 11500)

	for i := 0; i < 3; i++ {
		again, err := e.receive(t, CallbackEventWin, "win-1", "", "round-1", 2500, uuid.Nil)
		if err != nil || again.Outcome != OutcomeSucceeded || !again.Replayed || again.LedgerTransactionID == nil || *again.LedgerTransactionID != *first.LedgerTransactionID {
			t.Fatalf("redelivery %d must return the original result: %+v %v", i, again, err)
		}
	}
	e.requireBalance(t, 11500)
	if n := e.countTx(t, "win-1"); n != 1 {
		t.Fatalf("expected exactly one win posting, got %d", n)
	}
	if n := e.countAudit(t, "casino_win.posted"); n != 1 {
		t.Fatalf("expected exactly one casino_win.posted audit row, got %d", n)
	}
}

// The intended outcome of a win redelivered AFTER a rollback of its bet
// (ledger-finance semantics): the win is an already-recorded fact, so the
// redelivery returns the ORIGINAL result and posts nothing - it is neither a
// 400 (ErrBetNotFound) nor a 409 (ErrLockAlreadyReleased). A FIRST delivery of
// a win whose bet was already rolled back is still refused (never pays).
func TestA3_WinRedeliveredAfterBetRollback_ReturnsOriginal(t *testing.T) {
	e := a3SetupConsumed(t, 10000)
	if _, err := e.receive(t, CallbackEventBet, "bet-r", "", "round-r", 1000, e.sessionID); err != nil {
		t.Fatalf("bet: %v", err)
	}
	first, err := e.receive(t, CallbackEventWin, "win-r", "", "round-r", 1500, uuid.Nil)
	if err != nil {
		t.Fatalf("win: %v", err)
	}
	if _, err := e.receive(t, CallbackEventRollback, "rb-r", "bet-r", "round-r", 0, uuid.Nil); err != nil {
		t.Fatalf("rollback of the bet: %v", err)
	}
	balanceBefore := cashBalance(t, e.pool, e.f)

	again, err := e.receive(t, CallbackEventWin, "win-r", "", "round-r", 1500, uuid.Nil)
	if err != nil {
		t.Fatalf("redelivered win after a bet rollback must not error (was 400/409): %v", err)
	}
	if again.Outcome != OutcomeSucceeded || !again.Replayed || *again.LedgerTransactionID != *first.LedgerTransactionID {
		t.Fatalf("expected the original result, got %+v (original %v)", again, first.LedgerTransactionID)
	}
	e.requireBalance(t, balanceBefore)
	if n := e.countTx(t, "win-r"); n != 1 {
		t.Fatalf("expected one win posting, got %d", n)
	}

	// A NEW win (different provider_tx_id) for the rolled-back round still never pays.
	if _, err := e.receive(t, CallbackEventWin, "win-r2", "", "round-r", 1500, uuid.Nil); !errors.Is(err, ErrBetNotFound) {
		t.Fatalf("a first-delivery win for a rolled-back bet must still be ErrBetNotFound, got %v", err)
	}
	e.requireBalance(t, balanceBefore)
}

func TestA3_WinRedeliveredAfterWinRollback_ReturnsOriginal(t *testing.T) {
	e := a3SetupConsumed(t, 10000)
	if _, err := e.receive(t, CallbackEventBet, "bet-w", "", "round-w", 1000, e.sessionID); err != nil {
		t.Fatalf("bet: %v", err)
	}
	first, err := e.receive(t, CallbackEventWin, "win-w", "", "round-w", 1500, uuid.Nil)
	if err != nil {
		t.Fatalf("win: %v", err)
	}
	if _, err := e.receive(t, CallbackEventRollback, "rb-w", "win-w", "round-w", 0, uuid.Nil); err != nil {
		t.Fatalf("rollback of the win: %v", err)
	}
	balanceBefore := cashBalance(t, e.pool, e.f)
	again, err := e.receive(t, CallbackEventWin, "win-w", "", "round-w", 1500, uuid.Nil)
	if err != nil || !again.Replayed || *again.LedgerTransactionID != *first.LedgerTransactionID {
		t.Fatalf("redelivered win after the win's own rollback must return the original: %+v %v", again, err)
	}
	e.requireBalance(t, balanceBefore)
}

func TestA3_WinRedelivery_PayloadMismatchIsRefused(t *testing.T) {
	e := a3SetupConsumed(t, 10000)
	if _, err := e.receive(t, CallbackEventBet, "bet-m", "", "round-m", 1000, e.sessionID); err != nil {
		t.Fatalf("bet: %v", err)
	}
	if _, err := e.receive(t, CallbackEventBet, "bet-m2", "", "round-m2", 1000, e.sessionID); err != nil {
		t.Fatalf("bet 2: %v", err)
	}
	if _, err := e.receive(t, CallbackEventWin, "win-m", "", "round-m", 1500, uuid.Nil); err != nil {
		t.Fatalf("win: %v", err)
	}
	balanceBefore := cashBalance(t, e.pool, e.f)
	for name, call := range map[string]func() (ReceiveCallbackResult, error){
		"amount": func() (ReceiveCallbackResult, error) {
			return e.receive(t, CallbackEventWin, "win-m", "", "round-m", 1501, uuid.Nil)
		},
		"round": func() (ReceiveCallbackResult, error) {
			return e.receive(t, CallbackEventWin, "win-m", "", "round-m2", 1500, uuid.Nil)
		},
	} {
		if _, err := call(); !errors.Is(err, ErrProviderTxPayloadMismatch) {
			t.Fatalf("%s mismatch on a redelivered win must be ErrProviderTxPayloadMismatch, got %v", name, err)
		}
	}
	e.requireBalance(t, balanceBefore)
}

// A win whose provider_tx_id was already used by a BET (a different posting
// kind) is not a replay of a win and must not be answered as one.
func TestA3_WinRedelivery_SameReferenceDifferentKindIsNotAReplay(t *testing.T) {
	e := a3SetupConsumed(t, 10000)
	if _, err := e.receive(t, CallbackEventBet, "shared-ref", "", "round-k", 1000, e.sessionID); err != nil {
		t.Fatalf("bet: %v", err)
	}
	r, err := e.receive(t, CallbackEventWin, "shared-ref", "", "round-k", 1000, uuid.Nil)
	if err == nil && r.Replayed {
		t.Fatalf("a win must not be answered with a bet posting as 'replayed': %+v", r)
	}
	if n := e.countTx(t, "shared-ref"); n != 1 {
		t.Fatalf("expected the one original (bet) posting, got %d", n)
	}
}

// Duplicates verified UNDER CONCURRENCY, not only sequentially: N parallel
// deliveries of one win post exactly once and all return the same result.
func TestA3_WinRedelivery_Concurrent_PostsOnce(t *testing.T) {
	e := a3SetupConsumed(t, 10000)
	if _, err := e.receive(t, CallbackEventBet, "bet-c", "", "round-c", 1000, e.sessionID); err != nil {
		t.Fatalf("bet: %v", err)
	}
	const n = 12
	var wg sync.WaitGroup
	results := make([]ReceiveCallbackResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = e.receive(t, CallbackEventWin, "win-c", "", "round-c", 1700, uuid.Nil)
		}(i)
	}
	close(start)
	wg.Wait()
	var first uuid.UUID
	replays := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil || results[i].Outcome != OutcomeSucceeded || results[i].LedgerTransactionID == nil {
			t.Fatalf("delivery %d: %+v %v", i, results[i], errs[i])
		}
		if first == uuid.Nil {
			first = *results[i].LedgerTransactionID
		} else if *results[i].LedgerTransactionID != first {
			t.Fatalf("deliveries returned different ledger transactions")
		}
		if results[i].Replayed {
			replays++
		}
	}
	if replays != n-1 {
		t.Fatalf("expected exactly %d replays and one first posting, got %d replays", n-1, replays)
	}
	if c := e.countTx(t, "win-c"); c != 1 {
		t.Fatalf("expected one win posting, got %d", c)
	}
	e.requireBalance(t, 10700)
}
