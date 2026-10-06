//go:build integration

// PRH-2 R5, Q-GP-5 (owner decision 2026-10-06, ADR 0095 section 40.6):
// TERMINAL STAKE RETURNS stay allowed on a suspended or closed tenant. A
// rollback of a posted casino_bet returns that stake exactly once; every
// other gameplay posting (new bet, win, rollback of a win) is still refused.
// The operations run on the RUNTIME role pool (asserted NOT rolsuper AND NOT
// rolbypassrls); fixtures are seeded through the owner pool.
package casino

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (w gateWorld) deliverRollbackAs(ref, original, round string, amount int64, player uuid.UUID) (ReceiveCallbackResult, error) {
	payload := w.provider.CallbackPayload(w.f.tenantID, CallbackEventRollback, ref, original, round, "game-1", amount, "EUR", "", "", player, uuid.Nil)
	var res ReceiveCallbackResult
	err := w.rt.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = w.orch.receiveCallbackInTx(ctx, tx, w.f.tenantID, "mock-casino", payload)
		return err
	})
	return res, err
}

func countTx(t *testing.T, w gateWorld, txType string) int {
	t.Helper()
	var n int
	if err := w.owner.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = $2`, w.f.tenantID, txType).Scan(&n)
	}); err != nil {
		t.Fatalf("count %s: %v", txType, err)
	}
	return n
}

func assertLedgerInvariants(t *testing.T, w gateWorld) {
	t.Helper()
	loAssertBalanced(t, w.owner, w.f.tenantID)
	loAssertProjectionMatchesRebuild(t, w.owner, w.f.tenantID)
}

func TestStakeReturn_CasinoRollbackOfBetAllowedOnNonActiveTenant(t *testing.T) {
	owner := testPool(t)
	rt := runtimePoolForGate(t)
	for _, status := range []string{"suspended", "closed"} {
		t.Run(status, func(t *testing.T) {
			w := newGateWorld(t, owner, rt)
			for _, d := range []struct {
				ev                   CallbackEventType
				ref, original, round string
				amount               int64
			}{
				{CallbackEventBet, "b-1", "", "round-1", 1000},
				{CallbackEventBet, "b-2", "", "round-2", 700},
				{CallbackEventWin, "w-2", "", "round-2", 2000},
				{CallbackEventBet, "b-3", "", "round-3", 500},
				{CallbackEventBet, "b-4", "", "round-4", 300},
			} {
				if _, err := w.deliver(t, d.ev, d.ref, d.original, d.round, d.amount); err != nil {
					t.Fatalf("active delivery %s: %v", d.ref, err)
				}
			}
			setStatusForGate(t, owner, w.f.tenantID, status)
			cash0 := cashBalance(t, owner, w.f)
			txs0, _ := ledgerCountsForGate(t, owner, w.f.tenantID)

			// 1. The stake return succeeds, once, balanced.
			res, err := w.deliver(t, CallbackEventRollback, "rb-1", "b-1", "round-1", 0)
			if err != nil || res.Replayed || res.LedgerTransactionID == nil {
				t.Fatalf("stake return of a posted bet must post on a %s tenant: %+v err=%v", status, res, err)
			}
			if got := cashBalance(t, owner, w.f); got != cash0+1000 {
				t.Fatalf("stake not returned: cash %d -> %d", cash0, got)
			}
			if txs, _ := ledgerCountsForGate(t, owner, w.f.tenantID); txs != txs0+1 {
				t.Fatalf("expected exactly one new transaction, %d -> %d", txs0, txs)
			}
			if n := countAuditRows(t, owner, w.f.tenantID, "casino_bet.rolled_back"); n != 1 {
				t.Fatalf("expected one audit row for the return, got %d", n)
			}
			assertLedgerInvariants(t, w)

			// 2. A duplicate delivery is an idempotent replay: nothing posts.
			again, err := w.deliver(t, CallbackEventRollback, "rb-1", "b-1", "round-1", 0)
			if err != nil || !again.Replayed {
				t.Fatalf("duplicate return must replay: %+v err=%v", again, err)
			}
			if again.LedgerTransactionID == nil || *again.LedgerTransactionID != *res.LedgerTransactionID {
				t.Fatalf("replay must name the original posting")
			}
			if txs, _ := ledgerCountsForGate(t, owner, w.f.tenantID); txs != txs0+1 || cashBalance(t, owner, w.f) != cash0+1000 {
				t.Fatalf("replay changed the ledger")
			}
			if n := countAuditRows(t, owner, w.f.tenantID, "casino_bet.rolled_back"); n != 1 {
				t.Fatalf("replay must not write a second audit row, got %d", n)
			}

			// 3. A second DISTINCT reference for the same bet is refused.
			if _, err := w.deliver(t, CallbackEventRollback, "rb-1-other", "b-1", "round-1", 0); !errors.Is(err, ErrAlreadyRolledBack) {
				t.Fatalf("a second distinct reference must be ErrAlreadyRolledBack, got %v", err)
			}
			if txs, _ := ledgerCountsForGate(t, owner, w.f.tenantID); txs != txs0+1 {
				t.Fatalf("a refused second return posted")
			}
			assertLedgerInvariants(t, w)

			// 4. Mismatched returns never post money (bet b-3 is untouched).
			cash1 := cashBalance(t, owner, w.f)
			txs1, entries1 := ledgerCountsForGate(t, owner, w.f.tenantID)
			for _, m := range []struct {
				name   string
				round  string
				amount int64
				player uuid.UUID
			}{
				{"amount_mismatch", "round-3", 501, w.f.playerAccountID},
				{"wrong_player", "round-3", 0, uuid.New()},
				{"wrong_round", "round-other", 0, w.f.playerAccountID},
			} {
				_, err := w.deliverRollbackAs("rb-bad-"+m.name, "b-3", m.round, m.amount, m.player)
				if !errors.Is(err, ErrTenantNotActive) || !errors.Is(err, ErrStakeReturnMismatch) {
					t.Fatalf("%s: expected a refusal (ErrTenantNotActive+ErrStakeReturnMismatch), got %v", m.name, err)
				}
			}
			// The original is a WIN, not a bet: still refused.
			if _, err := w.deliver(t, CallbackEventRollback, "rb-win", "w-2", "round-2", 0); !errors.Is(err, ErrTenantNotActive) {
				t.Fatalf("a rollback of a posted win must stay refused, got %v", err)
			}
			// New wager and a win for an open round: refused.
			if _, err := w.deliver(t, CallbackEventBet, "b-new", "", "round-new", 100); !errors.Is(err, ErrTenantNotActive) {
				t.Fatalf("a new bet must stay refused, got %v", err)
			}
			if _, err := w.deliver(t, CallbackEventWin, "w-3", "", "round-3", 900); !errors.Is(err, ErrTenantNotActive) {
				t.Fatalf("a win for an open round must stay refused, got %v", err)
			}
			if txs, entries := ledgerCountsForGate(t, owner, w.f.tenantID); txs != txs1 || entries != entries1 || cashBalance(t, owner, w.f) != cash1 {
				t.Fatalf("refused/mismatched returns changed the ledger or wallet")
			}

			// 5. A nonexistent original writes a tombstone as before (no money).
			tomb, err := w.deliver(t, CallbackEventRollback, "rb-ghost", "b-ghost", "round-ghost", 0)
			if err != nil || !tomb.Tombstoned {
				t.Fatalf("a rollback of an unseen original must tombstone: %+v err=%v", tomb, err)
			}
			if cashBalance(t, owner, w.f) != cash1 {
				t.Fatalf("a tombstone moved money")
			}

			// 6. The correct return of b-3 still works afterwards, with the
			// matching amount and player stated.
			ok, err := w.deliverRollbackAs("rb-3", "b-3", "round-3", 500, w.f.playerAccountID)
			if err != nil || ok.Replayed {
				t.Fatalf("a matching return must post: %+v err=%v", ok, err)
			}
			if got := cashBalance(t, owner, w.f); got != cash1+500 {
				t.Fatalf("cash %d, want %d", got, cash1+500)
			}
			assertLedgerInvariants(t, w)
		})
	}
}

// A rollback naming another tenant's bet reference never reaches that bet:
// the lookup is tenant-scoped, so the other (non-active) tenant writes a
// tombstone and tenant A's ledger is untouched.
func TestStakeReturn_CasinoCrossTenantIsolation(t *testing.T) {
	owner := testPool(t)
	rt := runtimePoolForGate(t)
	a := newGateWorld(t, owner, rt)
	b := newGateWorld(t, owner, rt)
	if _, err := a.deliver(t, CallbackEventBet, "b-iso", "", "round-iso", 400); err != nil {
		t.Fatal(err)
	}
	setStatusForGate(t, owner, a.f.tenantID, "closed")
	setStatusForGate(t, owner, b.f.tenantID, "suspended")
	cashA := cashBalance(t, owner, a.f)
	txsA, _ := ledgerCountsForGate(t, owner, a.f.tenantID)

	res, err := b.deliver(t, CallbackEventRollback, "rb-iso", "b-iso", "round-iso", 0)
	if err != nil || !res.Tombstoned {
		t.Fatalf("tenant B has no such bet: expected a tombstone, got %+v err=%v", res, err)
	}
	if txs, _ := ledgerCountsForGate(t, owner, a.f.tenantID); txs != txsA || cashBalance(t, owner, a.f) != cashA {
		t.Fatalf("tenant A was touched by tenant B's rollback")
	}
	// Tenant A's own return still works while closed.
	if _, err := a.deliver(t, CallbackEventRollback, "rb-iso-a", "b-iso", "round-iso", 0); err != nil {
		t.Fatalf("tenant A's own stake return: %v", err)
	}
	assertLedgerInvariants(t, a)
	assertLedgerInvariants(t, b)
}

// Duplicate concurrent returns: exactly one posting, whatever the references.
func TestStakeReturn_CasinoConcurrentReturnsPostExactlyOnce(t *testing.T) {
	owner := testPool(t)
	rt := runtimePoolForGate(t)
	for _, mode := range []string{"same_reference", "distinct_references"} {
		t.Run(mode, func(t *testing.T) {
			w := newGateWorld(t, owner, rt)
			if _, err := w.deliver(t, CallbackEventBet, "b-c", "", "round-c", 1000); err != nil {
				t.Fatal(err)
			}
			setStatusForGate(t, owner, w.f.tenantID, "suspended")
			cash0 := cashBalance(t, owner, w.f)
			const n = 8
			var wg sync.WaitGroup
			errs := make([]error, n)
			results := make([]ReceiveCallbackResult, n)
			start := make(chan struct{})
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					ref := "rb-c"
					if mode == "distinct_references" {
						ref = "rb-c-" + uuid.NewString()
					}
					<-start
					results[i], errs[i] = w.deliver(t, CallbackEventRollback, ref, "b-c", "round-c", 0)
				}()
			}
			close(start)
			wg.Wait()
			okCount := 0
			for i := range errs {
				switch {
				case errs[i] == nil && !results[i].Replayed:
					okCount++
				case errs[i] == nil && results[i].Replayed:
					if mode != "same_reference" {
						t.Fatalf("distinct references cannot replay")
					}
				case errors.Is(errs[i], ErrAlreadyRolledBack):
					if mode != "distinct_references" {
						t.Fatalf("the same reference must replay, not conflict: %v", errs[i])
					}
				default:
					t.Fatalf("unexpected outcome %d: %v", i, errs[i])
				}
			}
			if okCount != 1 {
				t.Fatalf("exactly one delivery must post, got %d", okCount)
			}
			if got := countTx(t, w, "casino_rollback"); got != 1 {
				t.Fatalf("exactly one casino_rollback expected, got %d", got)
			}
			if got := cashBalance(t, owner, w.f); got != cash0+1000 {
				t.Fatalf("stake must return exactly once: %d -> %d", cash0, got)
			}
			assertLedgerInvariants(t, w)
		})
	}
}

// A status change racing a stake return, both orders, two real connections:
// either way the return posts exactly once (a stake return is allowed on a
// closed tenant), and the status change is never lost.
func TestStakeReturn_CasinoStatusChangeRace(t *testing.T) {
	owner := testPool(t)
	rt := runtimePoolForGate(t)

	t.Run("status_change_in_flight_then_return", func(t *testing.T) {
		w := newGateWorld(t, owner, rt)
		if _, err := w.deliver(t, CallbackEventBet, "b-r1", "", "round-r1", 100); err != nil {
			t.Fatal(err)
		}
		updated, release, closerDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		var once sync.Once
		releaseOnce := func() { once.Do(func() { close(release) }) }
		t.Cleanup(releaseOnce)
		go func() {
			closerDone <- owner.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, w.f.tenantID); err != nil {
					return err
				}
				close(updated)
				<-release
				return nil
			})
		}()
		<-updated
		type out struct {
			res ReceiveCallbackResult
			err error
		}
		posted := make(chan out, 1)
		go func() {
			r, err := w.deliver(t, CallbackEventRollback, "rb-r1", "b-r1", "round-r1", 0)
			posted <- out{r, err}
		}()
		select {
		case o := <-posted:
			t.Fatalf("the return must wait for the in-flight status change: %+v err=%v", o.res, o.err)
		case <-time.After(500 * time.Millisecond):
		}
		releaseOnce()
		if err := <-closerDone; err != nil {
			t.Fatalf("status change: %v", err)
		}
		if o := <-posted; o.err != nil || o.res.Replayed {
			t.Fatalf("the return must post after the suspension commits: %+v err=%v", o.res, o.err)
		}
		assertLedgerInvariants(t, w)
	})

	t.Run("return_in_flight_then_status_change_waits", func(t *testing.T) {
		w := newGateWorld(t, owner, rt)
		if _, err := w.deliver(t, CallbackEventBet, "b-r2", "", "round-r2", 100); err != nil {
			t.Fatal(err)
		}
		setStatusForGate(t, owner, w.f.tenantID, "suspended")
		inTx, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		releaseOnce := func() { once.Do(func() { close(release) }) }
		t.Cleanup(releaseOnce)
		payload := w.provider.CallbackPayload(w.f.tenantID, CallbackEventRollback, "rb-r2", "b-r2", "round-r2", "game-1", 0, "EUR", "", "", w.f.playerAccountID, uuid.Nil)
		posted := make(chan error, 1)
		go func() {
			posted <- rt.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				if _, err := w.orch.receiveCallbackInTx(ctx, tx, w.f.tenantID, "mock-casino", payload); err != nil {
					return err
				}
				close(inTx)
				<-release
				return nil
			})
		}()
		<-inTx
		closed := make(chan error, 1)
		go func() {
			closed <- owner.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE tenants SET status = 'closed' WHERE id = $1`, w.f.tenantID)
				return err
			})
		}()
		select {
		case err := <-closed:
			t.Fatalf("the status change must wait for the in-flight stake return: %v", err)
		case <-time.After(500 * time.Millisecond):
		}
		releaseOnce()
		if err := <-posted; err != nil {
			t.Fatalf("return: %v", err)
		}
		if err := <-closed; err != nil {
			t.Fatalf("status change: %v", err)
		}
		assertLedgerInvariants(t, w)
	})
}

// The 0121 backstop passes exactly a casino_rollback that reverses a casino_bet
// of the same tenant and refuses every other raw gameplay insert.
func TestStakeReturn_CasinoDatabaseBackstopNarrowed(t *testing.T) {
	owner := testPool(t)
	rt := runtimePoolForGate(t)
	w := newGateWorld(t, owner, rt)
	for _, d := range []struct {
		ev                   CallbackEventType
		ref, original, round string
		amount               int64
	}{
		{CallbackEventBet, "b-db", "", "round-db", 500},
		{CallbackEventWin, "w-db", "", "round-db", 800},
	} {
		if _, err := w.deliver(t, d.ev, d.ref, d.original, d.round, d.amount); err != nil {
			t.Fatal(err)
		}
	}
	idOf := func(ref string) uuid.UUID {
		var id uuid.UUID
		if err := owner.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT id FROM ledger_transactions WHERE tenant_id = $1 AND provider_tx_id = $2`, w.f.tenantID, ref).Scan(&id)
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	betID, winID := idOf("b-db"), idOf("w-db")
	setStatusForGate(t, owner, w.f.tenantID, "suspended")

	insert := func(txType, key string, reverses *uuid.UUID) error {
		return rt.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO ledger_transactions (tenant_id, transaction_type, idempotency_key, correlation_id, reverses_transaction_id)
				 VALUES ($1, $2, $3, $4, $5)`, w.f.tenantID, txType, key, uuid.New(), reverses)
			return err
		})
	}
	assertPgCode(t, insert("casino_rollback", "raw-rb-win", &winID), "GP010")
	assertPgCode(t, insert("casino_rollback", "raw-rb-none", nil), "GP010")
	assertPgCode(t, insert("casino_bet", "raw-bet", nil), "GP010")
	assertPgCode(t, insert("casino_win", "raw-win", nil), "GP010")
	// A rollback of a bet that does not exist in this tenant (random id) is
	// refused by the foreign key or the backstop, never accepted as a return.
	ghost := uuid.New()
	if err := insert("casino_rollback", "raw-rb-ghost", &ghost); err == nil {
		t.Fatal("a rollback naming a nonexistent original must not insert")
	}
	// The one allowed shape: reverses a casino_bet of the same tenant.
	if err := insert("casino_rollback", "raw-rb-bet", &betID); err != nil {
		// The empty transaction may still be refused by a later balance
		// check, but never by GP010.
		var pgErr interface{ SQLState() string }
		if errors.As(err, &pgErr) && pgErr.SQLState() == "GP010" {
			t.Fatalf("a casino_rollback of a casino_bet must pass the gameplay backstop: %v", err)
		}
	}
}
