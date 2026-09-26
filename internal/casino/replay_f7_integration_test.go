//go:build integration

package casino

// Stage 10 F-7 remediation (ADR 0020 amendment 2026-09-25,
// docs/governance/stage-10-f7-ledger-replay-audit.md §3 sites #6-#9, §6.4
// "per caller"): legitimate redeliveries still resolve to the original
// ledger transaction; a reused provider reference with a different
// payload - which used to be answered with the ORIGINAL result as
// success - is now ErrProviderTxPayloadMismatch with zero ledger effect.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

type f7CasinoEnv struct {
	pool      *db.Pool
	f         casinoFixture
	provider  *MockCasinoProvider
	orch      *Orchestrator
	sessionID uuid.UUID
}

func newF7CasinoEnv(t *testing.T, funding int64) f7CasinoEnv {
	t.Helper()
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, funding)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	return f7CasinoEnv{pool: pool, f: f, provider: provider, orch: NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider)), sessionID: sessionID}
}

func (e f7CasinoEnv) deliver(payload webhookauth.Inbound) (ReceiveCallbackResult, error) {
	var res ReceiveCallbackResult
	err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = e.orch.receiveCallbackInTx(ctx, tx, e.f.tenantID, "mock-casino", payload)
		return err
	})
	return res, err
}

func (e f7CasinoEnv) mustDeliver(t *testing.T, payload webhookauth.Inbound) ReceiveCallbackResult {
	t.Helper()
	res, err := e.deliver(payload)
	if err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if res.LedgerTransactionID == nil {
		t.Fatalf("deliver: no ledger transaction id in %+v", res)
	}
	return res
}

func (e f7CasinoEnv) bet(ref, round string, amount int64, session uuid.UUID) webhookauth.Inbound {
	return e.provider.CallbackPayload(e.f.tenantID, CallbackEventBet, ref, "", round, "game-1", amount, "EUR", OutcomeSucceeded, "", e.f.playerAccountID, session)
}

func (e f7CasinoEnv) win(ref, round string, amount int64) webhookauth.Inbound {
	return e.provider.CallbackPayload(e.f.tenantID, CallbackEventWin, ref, "", round, "game-1", amount, "EUR", OutcomeSucceeded, "", e.f.playerAccountID, uuid.Nil)
}

func (e f7CasinoEnv) rollback(ref, original, round string) webhookauth.Inbound {
	return e.provider.CallbackPayload(e.f.tenantID, CallbackEventRollback, ref, original, round, "game-1", 0, "EUR", "", "", e.f.playerAccountID, uuid.Nil)
}

func (e f7CasinoEnv) ledgerTxCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, e.f.tenantID).Scan(&n)
	}); err != nil {
		t.Fatalf("count ledger transactions: %v", err)
	}
	return n
}

// expectRejected asserts a class-C replay is now rejected with the typed
// error, posts nothing and leaves the balance and SUM(D)==SUM(C) intact.
func (e f7CasinoEnv) expectRejected(t *testing.T, payload webhookauth.Inbound) {
	t.Helper()
	before, balance := e.ledgerTxCount(t), cashBalance(t, e.pool, e.f)
	res, err := e.deliver(payload)
	if !errors.Is(err, ErrProviderTxPayloadMismatch) {
		t.Fatalf("want ErrProviderTxPayloadMismatch, got res=%+v err=%v", res, err)
	}
	if got := e.ledgerTxCount(t); got != before {
		t.Fatalf("rejected replay changed ledger transaction count %d -> %d", before, got)
	}
	if got := cashBalance(t, e.pool, e.f); got != balance {
		t.Fatalf("rejected replay changed player_cash %d -> %d", balance, got)
	}
	if d, c := sumDebitsCredits(t, e.pool, e.f.tenantID); d != c {
		t.Fatalf("SUM(debits)=%d != SUM(credits)=%d", d, c)
	}
}

// Legitimate redelivery of bet, direct-cash win and rollback each still
// return success with the ORIGINAL ledger transaction id (sites #6, #7,
// #9 happy path) - the property ADR 0088 §11.2 requires per caller.
func TestF7Casino_LegitimateRedeliveriesStillResolveToOriginal(t *testing.T) {
	e := newF7CasinoEnv(t, 5_000)
	for _, payload := range []webhookauth.Inbound{
		e.bet("f7-bet-1", "f7-round-1", 1_000, e.sessionID),
		e.win("f7-win-1", "f7-round-1", 2_500),
		e.rollback("f7-rb-1", "f7-win-1", "f7-round-1"),
	} {
		first := e.mustDeliver(t, payload)
		count := e.ledgerTxCount(t)
		second := e.mustDeliver(t, payload)
		if *second.LedgerTransactionID != *first.LedgerTransactionID || second.Outcome != first.Outcome {
			t.Fatalf("redelivery resolved to %s/%s, want %s/%s", *second.LedgerTransactionID, second.Outcome, *first.LedgerTransactionID, first.Outcome)
		}
		if got := e.ledgerTxCount(t); got != count {
			t.Fatalf("redelivery posted: %d -> %d ledger transactions", count, got)
		}
	}
	if got := cashBalance(t, e.pool, e.f); got != 4_000 {
		t.Fatalf("player_cash = %d, want 4000 (bet once, win once, win rolled back once)", got)
	}
}

// Site #8: concurrent rollbacks naming one never-seen original all resolve
// to the one tombstone. Each attempt mints a fresh tombstone correlation
// id, so this relies on the TxTombstone correlation exemption.
func TestF7Casino_ConcurrentTombstoneRedeliveryIsIdempotent(t *testing.T) {
	e := newF7CasinoEnv(t, 1_000)
	const n = 6
	results := make([]ReceiveCallbackResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = e.deliver(e.rollback("f7-rb-tomb", "f7-never-seen", "f7-round-tomb"))
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("attempt %d: %v", i, errs[i])
		}
		if !results[i].Tombstoned || *results[i].LedgerTransactionID != *results[0].LedgerTransactionID {
			t.Fatalf("attempt %d: %+v, want the one tombstone %s", i, results[i], *results[0].LedgerTransactionID)
		}
	}
	if got := e.ledgerTxCount(t); got != 2 { // funding + one tombstone
		t.Fatalf("ledger transactions = %d, want 2", got)
	}
}

// Site #6 (caller-level; ledger.Post is never reached): a bet reference
// redelivered with a different amount, round, or without its session.
func TestF7Casino_BetReplayWithDifferentPayloadRejected(t *testing.T) {
	e := newF7CasinoEnv(t, 5_000)
	e.mustDeliver(t, e.bet("f7-bet-c6", "f7-round-c6", 1_000, e.sessionID))

	e.expectRejected(t, e.bet("f7-bet-c6", "f7-round-c6", 1_001, e.sessionID))
	e.expectRejected(t, e.bet("f7-bet-c6", "f7-round-other", 1_000, e.sessionID))
	e.expectRejected(t, e.bet("f7-bet-c6", "f7-round-c6", 1_000, uuid.Nil))
	e.expectRejected(t, e.bet("f7-bet-c6", "f7-round-c6", 1_000, uuid.New()))

	// The exact redelivery is still fine after the rejections.
	e.mustDeliver(t, e.bet("f7-bet-c6", "f7-round-c6", 1_000, e.sessionID))
	if got := cashBalance(t, e.pool, e.f); got != 4_000 {
		t.Fatalf("player_cash = %d, want 4000", got)
	}
}

// Site #9: the only production-reachable win path. A win reference
// redelivered with a different amount used to return the original as
// success while the provider believed the new amount was paid.
func TestF7Casino_WinReplayWithDifferentAmountRejected(t *testing.T) {
	e := newF7CasinoEnv(t, 5_000)
	e.mustDeliver(t, e.bet("f7-bet-c9", "f7-round-c9", 1_000, e.sessionID))
	e.mustDeliver(t, e.win("f7-win-c9", "f7-round-c9", 2_000))

	e.expectRejected(t, e.win("f7-win-c9", "f7-round-c9", 9_999))
	if got := cashBalance(t, e.pool, e.f); got != 6_000 {
		t.Fatalf("player_cash = %d, want 6000 (only the first win amount)", got)
	}
}

// Site #7: a rollback reference R1 that reversed bet B1, redelivered
// naming a DIFFERENT, unreversed bet B2. It used to return R1's reversal
// of B1 as success while B2 was never refunded.
func TestF7Casino_RollbackRefReusedForDifferentOriginalRejected(t *testing.T) {
	e := newF7CasinoEnv(t, 5_000)
	e.mustDeliver(t, e.bet("f7-bet-c7a", "f7-round-c7a", 1_000, e.sessionID))
	e.mustDeliver(t, e.bet("f7-bet-c7b", "f7-round-c7b", 1_000, e.sessionID))
	e.mustDeliver(t, e.rollback("f7-rb-c7", "f7-bet-c7a", "f7-round-c7a"))
	if got := cashBalance(t, e.pool, e.f); got != 4_000 {
		t.Fatalf("player_cash = %d, want 4000 before the replay", got)
	}

	e.expectRejected(t, e.rollback("f7-rb-c7", "f7-bet-c7b", "f7-round-c7b"))

	// B2 is still unreversed: a genuine rollback of it under its own new
	// reference still works.
	e.mustDeliver(t, e.rollback("f7-rb-c7-b", "f7-bet-c7b", "f7-round-c7b"))
	if got := cashBalance(t, e.pool, e.f); got != 5_000 {
		t.Fatalf("player_cash = %d, want 5000", got)
	}
}
