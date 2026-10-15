//go:build integration

// CAS-WIN-IDEMP-1 (A3) on the locked-funding branches. Before the win
// short-circuit, a redelivery of an already-posted locked-cash or locked-bonus
// win re-entered resolveWinOrigin, found the stake lock already released, and
// came back as ErrLockAlreadyReleased (409) instead of the recorded result.
// It never paid twice; these tests pin that the redelivery now returns the
// ORIGINAL result and leaves every balance, ledger row, audit row and held
// disposition untouched, for each posting shape postWin has.
package casino

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/bonus"
	"github.com/Diansalas/igaming-platform/internal/db"
)

func a3Win(t *testing.T, pool *db.Pool, f casinoFixture, orch *Orchestrator, provider *MockCasinoProvider, txID, round string, amount int64, asset string) (ReceiveCallbackResult, error) {
	t.Helper()
	payload := provider.CallbackPayload(f.tenantID, CallbackEventWin, txID, "", round, "game-1", amount, asset, OutcomeSucceeded, "", f.playerRef, uuid.Nil)
	var res ReceiveCallbackResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	return res, err
}

func a3Rows(t *testing.T, pool *db.Pool, f casinoFixture) (txs, entries, audits int) {
	t.Helper()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, f.tenantID).Scan(&txs); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE tenant_id = $1`, f.tenantID).Scan(&entries); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1`, f.tenantID).Scan(&audits)
	}); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	return
}

func TestA3_LockedCashWinRedelivery_ReturnsOriginal(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 1_000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	postLockedCashBet(t, pool, f, "mock-casino", "a3-lc-bet", "a3-lc-round", 400)
	first, err := a3Win(t, pool, f, orch, provider, "a3-lc-win", "a3-lc-round", 100, "EUR")
	if err != nil || first.Outcome != OutcomeSucceeded || first.Replayed {
		t.Fatalf("first win: %+v %v", first, err)
	}
	txs, entries, audits := a3Rows(t, pool, f)
	cash, locked := cashBalance(t, pool, f), s9LockedCashBalance(t, pool, f)
	if locked != 0 {
		t.Fatalf("fixture: the first win must release the lock, locked=%d", locked)
	}

	again, err := a3Win(t, pool, f, orch, provider, "a3-lc-win", "a3-lc-round", 100, "EUR")
	if errors.Is(err, ErrLockAlreadyReleased) {
		t.Fatalf("a redelivered locked-cash win must not be refused as ErrLockAlreadyReleased (409)")
	}
	if err != nil || !again.Replayed || *again.LedgerTransactionID != *first.LedgerTransactionID {
		t.Fatalf("expected the original result, got %+v %v", again, err)
	}
	if t2, e2, a2 := a3Rows(t, pool, f); t2 != txs || e2 != entries || a2 != audits {
		t.Fatalf("a redelivery wrote rows: txs %d->%d entries %d->%d audits %d->%d", txs, t2, entries, e2, audits, a2)
	}
	if cashBalance(t, pool, f) != cash || s9LockedCashBalance(t, pool, f) != locked {
		t.Fatal("a redelivery moved a balance")
	}
	if d, c := sumDebitsCredits(t, pool, f.tenantID); d != c {
		t.Fatalf("ledger invariant violated: debits=%d credits=%d", d, c)
	}
	// A different amount under the same reference is a payload mismatch, not a replay.
	if _, err := a3Win(t, pool, f, orch, provider, "a3-lc-win", "a3-lc-round", 101, "EUR"); !errors.Is(err, ErrProviderTxPayloadMismatch) {
		t.Fatalf("expected ErrProviderTxPayloadMismatch for a changed amount, got %v", err)
	}
}

func TestA3_LockedBonusWinRedelivery_ReturnsOriginal_NonTerminalAndTerminalGrant(t *testing.T) {
	for _, tc := range []struct {
		name     string
		terminal bool
	}{{"non_terminal_grant", false}, {"terminal_grant_hold_capture", true}} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testPool(t)
			f := seedCasinoFixture(t, pool)
			provider := NewMockCasinoProvider("mock-casino", "EUR")
			registerCasinoCapability(t, pool, f, provider, 100)
			orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

			co := seedBonusCampaignOffer(t, pool, f.tenantID, f.brandID)
			grantID := seedActivatedGrant(t, pool, f, co, "a3-lb-"+tc.name)
			postLockedBonusBet(t, pool, f, "mock-casino", "a3-lb-bet-"+tc.name, "a3-lb-round-"+tc.name, grantID, 400)
			if tc.terminal {
				setGrantTerminal(t, pool, f.tenantID, grantID, bonus.GrantExpired)
			}
			first, err := a3Win(t, pool, f, orch, provider, "a3-lb-win-"+tc.name, "a3-lb-round-"+tc.name, 250, "EUR")
			if err != nil || first.Outcome != OutcomeSucceeded || first.Replayed {
				t.Fatalf("first win: %+v %v", first, err)
			}
			txs, entries, audits := a3Rows(t, pool, f)
			before := bonusBalances(t, pool, f)

			again, err := a3Win(t, pool, f, orch, provider, "a3-lb-win-"+tc.name, "a3-lb-round-"+tc.name, 250, "EUR")
			if err != nil || !again.Replayed || *again.LedgerTransactionID != *first.LedgerTransactionID {
				t.Fatalf("expected the original result, got %+v %v", again, err)
			}
			if t2, e2, a2 := a3Rows(t, pool, f); t2 != txs || e2 != entries || a2 != audits {
				t.Fatalf("a redelivery wrote rows: txs %d->%d entries %d->%d audits %d->%d", txs, t2, entries, e2, audits, a2)
			}
			after := bonusBalances(t, pool, f)
			for k, v := range before {
				if after[k] != v {
					t.Fatalf("a redelivery moved the %s balance: %d -> %d", k, v, after[k])
				}
			}
			if d, c := sumDebitsCredits(t, pool, f.tenantID); d != c {
				t.Fatalf("ledger invariant violated: debits=%d credits=%d", d, c)
			}
		})
	}
}

func TestA3_WinRedelivery_AssetMismatchIsRefused(t *testing.T) {
	e := a3SetupConsumed(t, 10000)
	if _, err := e.receive(t, CallbackEventBet, "a3-asset-bet", "", "a3-asset-round", 1000, e.sessionID); err != nil {
		t.Fatalf("bet: %v", err)
	}
	if _, err := e.receive(t, CallbackEventWin, "a3-asset-win", "", "a3-asset-round", 1500, uuid.Nil); err != nil {
		t.Fatalf("win: %v", err)
	}
	before := cashBalance(t, e.pool, e.f)
	payload := e.provider.CallbackPayload(e.f.tenantID, CallbackEventWin, "a3-asset-win", "", "a3-asset-round", "game-1", 1500, "USD", OutcomeSucceeded, "", e.f.playerRef, uuid.Nil)
	err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := e.orch.receiveCallbackInTx(ctx, tx, e.f.tenantID, "mock-casino", payload)
		return err
	})
	if !errors.Is(err, ErrProviderTxPayloadMismatch) {
		t.Fatalf("a redelivered win in another asset must be ErrProviderTxPayloadMismatch, got %v", err)
	}
	e.requireBalance(t, before)
}
