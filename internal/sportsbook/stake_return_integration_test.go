//go:build integration

// PRH-2 R5, Q-GP-5 (owner decision 2026-10-06, ADR 0095 section 40.6):
// TERMINAL STAKE RETURNS stay allowed on a suspended or closed tenant: a void
// of an OPEN bet, and a void AFTER settlement (the rollback of the current
// settlement chained to the void). Everything else for a non-active tenant is
// still refused (new bet, settlement with payout, a rollback that reopens the
// bet). The operations run on the RUNTIME role pool (asserted NOT rolsuper AND
// NOT rolbypassrls); fixtures are seeded through the owner pool.
package sportsbook

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

func srAssertProjectionMatchesRebuild(t *testing.T, pool *db.Pool, tenantID uuid.UUID) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM ledger_accounts WHERE tenant_id = $1 ORDER BY id`, tenantID)
		if err != nil {
			return err
		}
		var ids []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, id := range ids {
			rebuilt, err := ledger.RebuildBalance(ctx, tx, id)
			if err != nil {
				return err
			}
			projected, err := ledger.GetProjectedBalance(ctx, tx, id)
			if err != nil {
				return err
			}
			if !projected.Found {
				if rebuilt.DebitTotal != 0 || rebuilt.CreditTotal != 0 {
					return errors.New("account has entries but no projection row")
				}
				continue
			}
			if rebuilt.DebitTotal != projected.DebitTotal || rebuilt.CreditTotal != projected.CreditTotal {
				return errors.New("projection drifted from the ledger")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("projection vs. rebuild: %v", err)
	}
}

func srInvariants(t *testing.T, pool *db.Pool, tenantID uuid.UUID) {
	t.Helper()
	loAssertBalancedSportsbook(t, pool, tenantID)
	srAssertProjectionMatchesRebuild(t, pool, tenantID)
}

func countByType(t *testing.T, pool *db.Pool, tenantID uuid.UUID, txType string) int {
	t.Helper()
	var n int
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = $2`, tenantID, txType).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func srPgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestStakeReturn_SportsbookVoidOpenBetAllowed(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	for _, status := range nonActiveStatuses {
		t.Run(status, func(t *testing.T) {
			f, actor, betID := newStdBet(t, owner)
			sel := seedSelection(t, owner, seedSelectionParams{})
			other, err := placeBet(t, owner, f, sel, stdStake, "second-"+uuid.NewString())
			if err != nil || !other.Accepted {
				t.Fatalf("second bet: %v", err)
			}
			setTenantStatus(t, owner, f.tenantID, status)
			txs0, _ := ledgerCounts(t, owner, f.tenantID)

			res, err := simulateSettlement(t, rt, f.tenantID, voidEvent(betID, actor, "market_cancelled"))
			if err != nil || res.Rejected() || res.Result != SettlementResultApplied || res.BetStatus != BetStatusVoid {
				t.Fatalf("a void of an open bet must apply on a %s tenant: %+v err=%v", status, res, err)
			}
			if got := betStatusOf(t, owner, f, betID); got != BetStatusVoid {
				t.Fatalf("bet status %s", got)
			}
			// Stake returned: the second (open) bet still holds its own stake.
			if got := lockedCashBalance(t, owner, f); got != stdStake {
				t.Fatalf("locked cash %d, want only the other bet's stake %d", got, stdStake)
			}
			if txs, _ := ledgerCounts(t, owner, f.tenantID); txs != txs0+1 {
				t.Fatalf("expected exactly one new transaction, %d -> %d", txs0, txs)
			}
			srInvariants(t, owner, f.tenantID)

			// Duplicate: an idempotent replay, nothing posts.
			again, err := simulateSettlement(t, rt, f.tenantID, voidEvent(betID, actor, "market_cancelled"))
			if err != nil || again.Rejected() || again.Result != SettlementResultReplayed {
				t.Fatalf("a duplicate void must replay: %+v err=%v", again, err)
			}
			if txs, _ := ledgerCounts(t, owner, f.tenantID); txs != txs0+1 {
				t.Fatalf("replay posted")
			}
			// A second DISTINCT reference (another reason) is refused.
			diff, err := simulateSettlement(t, rt, f.tenantID, voidEvent(betID, actor, "push"))
			if err != nil || !diff.Rejected() || diff.RejectionCode != SettlementRejectPayloadMismatch {
				t.Fatalf("a second distinct void must be refused as a payload mismatch: %+v err=%v", diff, err)
			}
			if got := countByType(t, owner, f.tenantID, "sportsbook_void"); got != 1 {
				t.Fatalf("exactly one void expected, got %d", got)
			}

			// Everything that creates exposure or pays out is still refused.
			settle, err := simulateSettlement(t, rt, f.tenantID, settleEvent(other.Bet.ID, actor, 1, SettlementOutcomeWon, stdPayout))
			if err != nil || settle.RejectionCode != SettlementRejectTenantNotActive {
				t.Fatalf("settlement with payout must stay refused: %+v err=%v", settle, err)
			}
			bet, err := placeBetRT(t, rt, f, sel, 100, "new-wager-"+status)
			if err != nil || bet.Accepted || bet.RejectionCategory != RejectionTenantNotActive {
				t.Fatalf("a new wager must stay refused: %+v err=%v", bet, err)
			}
			if got := betStatusOf(t, owner, f, other.Bet.ID); got != BetStatusOpen {
				t.Fatalf("refused settlement changed the other bet: %s", got)
			}
			srInvariants(t, owner, f.tenantID)
		})
	}
}

func TestStakeReturn_SportsbookVoidAfterSettlementAllowed(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	for _, status := range nonActiveStatuses {
		for _, outcome := range []string{SettlementOutcomeWon, SettlementOutcomeLost} {
			t.Run(status+"/"+outcome, func(t *testing.T) {
				f, actor, betID := newStdBet(t, owner)
				payout := int64(0)
				if outcome == SettlementOutcomeWon {
					payout = stdPayout
				}
				mustSimulate(t, owner, f.tenantID, settleEvent(betID, actor, 1, outcome, payout))
				setTenantStatus(t, owner, f.tenantID, status)
				txs0, _ := ledgerCounts(t, owner, f.tenantID)

				res, err := simulateSettlement(t, rt, f.tenantID, voidEvent(betID, actor, "data_error"))
				if err != nil || res.Rejected() || res.Result != SettlementResultApplied || res.BetStatus != BetStatusVoid {
					t.Fatalf("a void after settlement must apply on a %s tenant: %+v err=%v", status, res, err)
				}
				if len(res.LedgerTransactionIDs) != 2 {
					t.Fatalf("expected the rollback leg and the void, got %d postings", len(res.LedgerTransactionIDs))
				}
				if txs, _ := ledgerCounts(t, owner, f.tenantID); txs != txs0+2 {
					t.Fatalf("expected exactly two new transactions, %d -> %d", txs0, txs)
				}
				// The net effect is the bet never happened.
				assertNets(t, owner, f, 0, 0, 0)
				srInvariants(t, owner, f.tenantID)

				again, err := simulateSettlement(t, rt, f.tenantID, voidEvent(betID, actor, "data_error"))
				if err != nil || again.Rejected() || again.Result != SettlementResultReplayed {
					t.Fatalf("a duplicate must replay: %+v err=%v", again, err)
				}
				if txs, _ := ledgerCounts(t, owner, f.tenantID); txs != txs0+2 {
					t.Fatalf("replay posted")
				}
				srInvariants(t, owner, f.tenantID)
			})
		}
	}
}

// buildVoidInput builds the void exactly as voidBet does, from the bet's own
// placement entries (credit = locked cash account, debit = cash account).
func buildVoidInput(t *testing.T, owner *db.Pool, tenantID, betID uuid.UUID, amount int64) ledger.TransactionInput {
	t.Helper()
	var cash, locked uuid.UUID
	if err := owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT e.ledger_account_id, e.direction FROM ledger_transactions t
			  JOIN ledger_entries e ON e.ledger_transaction_id = t.id
			 WHERE t.tenant_id = $1 AND t.correlation_id = $2 AND t.transaction_type = 'sportsbook_bet'`, tenantID, betID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			var dir string
			if err := rows.Scan(&id, &dir); err != nil {
				return err
			}
			if dir == "credit" {
				locked = id
			} else {
				cash = id
			}
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return ledger.TransactionInput{
		TenantID: tenantID, TransactionType: ledger.TxSportsbookVoid,
		IdempotencyKey: voidIdempotencyKey(betID), CorrelationID: betID,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: locked, Direction: ledger.Debit, Amount: amount},
			{LedgerAccountID: cash, Direction: ledger.Credit, Amount: amount},
		},
	}
}

func lockAndPostRT(rt *db.Pool, tenantID uuid.UUID, chain bool, ins ...ledger.TransactionInput) error {
	return rt.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := lockAndPost(ctx, tx, chain, ins...)
		return err
	})
}

// A void that is not exactly the inverse of the bet's own placement never
// posts on a non-active tenant, and neither do postings of the wrong shape.
func TestStakeReturn_SportsbookMismatchedReturnsNeverPost(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	for _, status := range nonActiveStatuses {
		t.Run(status, func(t *testing.T) {
			f, actor, betID := newStdBet(t, owner)
			fb, _, betB := newStdBet(t, owner) // another tenant's bet
			setTenantStatus(t, owner, f.tenantID, status)
			setTenantStatus(t, owner, fb.tenantID, status)
			txs0, entries0 := ledgerCounts(t, owner, f.tenantID)
			txsB0, entriesB0 := ledgerCounts(t, owner, fb.tenantID)

			good := buildVoidInput(t, owner, f.tenantID, betID, stdStake)

			// Amount mismatch (more than the placed stake, and less).
			for _, amt := range []int64{stdStake + 1, stdStake - 1} {
				bad := buildVoidInput(t, owner, f.tenantID, betID, amt)
				if err := lockAndPostRT(rt, f.tenantID, false, bad); !errors.Is(err, ErrSettlementTenantNotActive) {
					t.Fatalf("amount %d: expected a refusal, got %v", amt, err)
				}
			}
			// Wrong account (a different bet's accounts, same tenant is not
			// available, so use the other TENANT's accounts under this tenant).
			foreign := buildVoidInput(t, owner, fb.tenantID, betB, stdStake)
			foreign.TenantID = f.tenantID
			foreign.CorrelationID = betID
			if err := lockAndPostRT(rt, f.tenantID, false, foreign); err == nil {
				t.Fatalf("a void naming another tenant's accounts must not post")
			}
			// Wrong tenant: tenant B's bet id under tenant A's scope has no
			// placement posting in A.
			crossBet := buildVoidInput(t, owner, fb.tenantID, betB, stdStake)
			crossBet.TenantID = f.tenantID
			if err := lockAndPostRT(rt, f.tenantID, false, crossBet); !errors.Is(err, ErrSettlementTenantNotActive) {
				t.Fatalf("a void for a bet that was never placed in this tenant must be refused, got %v", err)
			}
			// Wrong shapes: a settlement, a lone rollback, a rollback without a
			// void chain, a void chained wrongly.
			settle := ledger.TransactionInput{TenantID: f.tenantID, TransactionType: ledger.TxSportsbookSettlement, Entries: good.Entries}
			rollback := ledger.TransactionInput{TenantID: f.tenantID, TransactionType: ledger.TxSportsbookRollback, Entries: good.Entries}
			for name, c := range map[string]struct {
				chain bool
				ins   []ledger.TransactionInput
			}{
				"settlement":            {false, []ledger.TransactionInput{settle}},
				"lone_rollback":         {false, []ledger.TransactionInput{rollback}},
				"rollback_void_unchain": {false, []ledger.TransactionInput{rollback, good}},
				"void_then_rollback":    {true, []ledger.TransactionInput{good, rollback}},
				"chained_lone_void":     {true, []ledger.TransactionInput{good}},
			} {
				if err := lockAndPostRT(rt, f.tenantID, c.chain, c.ins...); !errors.Is(err, ErrSettlementTenantNotActive) {
					t.Fatalf("%s: expected a refusal, got %v", name, err)
				}
			}
			// A settlement with payout through the public path is refused too,
			// and the bet is untouched.
			res, err := simulateSettlement(t, rt, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
			if err != nil || res.RejectionCode != SettlementRejectTenantNotActive {
				t.Fatalf("settlement with payout must stay refused: %+v err=%v", res, err)
			}

			if txs, entries := ledgerCounts(t, owner, f.tenantID); txs != txs0 || entries != entries0 {
				t.Fatalf("a refused return changed the ledger: tx %d->%d", txs0, txs)
			}
			if txs, entries := ledgerCounts(t, owner, fb.tenantID); txs != txsB0 || entries != entriesB0 {
				t.Fatalf("the other tenant was touched")
			}
			srInvariants(t, owner, f.tenantID)
			srInvariants(t, owner, fb.tenantID)
		})
	}
}

// The 0121 backstop, below the application: a void of a SETTLED bet without
// its rollback leg, a rollback that is not part of a void, and an unrelated
// raw insert are refused with GP010; the composed rollback+void passes.
func TestStakeReturn_SportsbookDatabaseBackstop(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	f, actor, betID := newStdBet(t, owner)
	mustSimulate(t, owner, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	var settlementTx uuid.UUID
	if err := owner.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'sportsbook_settlement'`, f.tenantID).Scan(&settlementTx)
	}); err != nil {
		t.Fatal(err)
	}
	// A second, still OPEN bet (for the "rollback of a non-settlement" case).
	sel2 := seedSelection(t, owner, seedSelectionParams{})
	open2, err := placeBet(t, owner, f, sel2, stdStake, "open2-"+uuid.NewString())
	if err != nil || !open2.Accepted {
		t.Fatalf("second bet: %v", err)
	}
	var placement2 uuid.UUID
	if err := owner.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'sportsbook_bet' AND correlation_id = $2`, f.tenantID, open2.Bet.ID).Scan(&placement2)
	}); err != nil {
		t.Fatal(err)
	}
	setTenantStatus(t, owner, f.tenantID, "suspended")

	insert := func(tx pgx.Tx, ctx context.Context, txType, key string, correlation uuid.UUID, reverses, causation *uuid.UUID) (uuid.UUID, error) {
		var id uuid.UUID
		err := tx.QueryRow(ctx,
			`INSERT INTO ledger_transactions (tenant_id, transaction_type, idempotency_key, correlation_id, reverses_transaction_id, causation_id)
			 VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, f.tenantID, txType, key, correlation, reverses, causation).Scan(&id)
		return id, err
	}
	run := func(fn func(ctx context.Context, tx pgx.Tx) error) error {
		return rt.WithTenant(context.Background(), f.tenantID, fn)
	}

	// 1. A bare void of a bet that still has an outstanding settlement.
	err = run(func(ctx context.Context, tx pgx.Tx) error {
		_, err := insert(tx, ctx, "sportsbook_void", "raw-void-settled", betID, nil, nil)
		return err
	})
	if srPgCode(err) != "GP010" {
		t.Fatalf("a void of a settled bet without its rollback must be GP010, got %v", err)
	}
	// 2. A rollback that is not paired with a void (refused at COMMIT).
	err = run(func(ctx context.Context, tx pgx.Tx) error {
		_, err := insert(tx, ctx, "sportsbook_rollback", "raw-rollback-alone", betID, &settlementTx, nil)
		return err
	})
	if srPgCode(err) != "GP010" {
		t.Fatalf("a rollback that is not part of a void must be GP010 at commit, got %v", err)
	}
	// 3. A void for a bet that was never placed in this tenant.
	err = run(func(ctx context.Context, tx pgx.Tx) error {
		_, err := insert(tx, ctx, "sportsbook_void", "raw-void-ghost", uuid.New(), nil, nil)
		return err
	})
	if srPgCode(err) != "GP010" {
		t.Fatalf("a void for an unplaced bet must be GP010, got %v", err)
	}
	// 4. A rollback reversing something that is not a settlement.
	var placement uuid.UUID
	if err := owner.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'sportsbook_bet'`, f.tenantID).Scan(&placement)
	}); err != nil {
		t.Fatal(err)
	}
	err = run(func(ctx context.Context, tx pgx.Tx) error {
		_, err := insert(tx, ctx, "sportsbook_rollback", "raw-rollback-bet", betID, &placement, nil)
		return err
	})
	if srPgCode(err) != "GP010" {
		t.Fatalf("a rollback of a non-settlement must be GP010, got %v", err)
	}
	// 4b. Even PAIRED with a void, a rollback that reverses a non-settlement
	// (the bet's own placement) is refused by the BEFORE guard.
	err = run(func(ctx context.Context, tx pgx.Tx) error {
		rb, err := insert(tx, ctx, "sportsbook_rollback", "raw-rollback-placement", open2.Bet.ID, &placement2, nil)
		if err != nil {
			return err
		}
		_, err = insert(tx, ctx, "sportsbook_void", "raw-void-after-placement-rollback", open2.Bet.ID, nil, &rb)
		return err
	})
	if srPgCode(err) != "GP010" {
		t.Fatalf("a rollback of a bet's placement must be GP010 even when paired with a void, got %v", err)
	}
	// 5. The composed rollback + void (void.causation = rollback) passes.
	err = run(func(ctx context.Context, tx pgx.Tx) error {
		rb, err := insert(tx, ctx, "sportsbook_rollback", "raw-rollback-composed", betID, &settlementTx, nil)
		if err != nil {
			return err
		}
		_, err = insert(tx, ctx, "sportsbook_void", "raw-void-composed", betID, nil, &rb)
		return err
	})
	if srPgCode(err) == "GP010" {
		t.Fatalf("the composed rollback+void must pass the gameplay backstop: %v", err)
	}
	// 6. A void whose causation is NOT the rollback does not satisfy the pairing.
	err = run(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := insert(tx, ctx, "sportsbook_rollback", "raw-rollback-unpaired", betID, &settlementTx, nil); err != nil {
			return err
		}
		_, err := insert(tx, ctx, "sportsbook_void", "raw-void-unpaired", betID, nil, nil)
		return err
	})
	if srPgCode(err) != "GP010" {
		t.Fatalf("a void that is not caused by the rollback must not pair it, got %v", err)
	}
}

// Closing tenant A (or suspending it) does not affect tenant B's void, and
// tenant B cannot reach tenant A's bet.
func TestStakeReturn_SportsbookCrossTenantIsolation(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	fa, actorA, betA := newStdBet(t, owner)
	fb, actorB, betB := newStdBet(t, owner)
	setTenantStatus(t, owner, fa.tenantID, "closed")

	x, err := simulateSettlement(t, rt, fb.tenantID, voidEvent(betA, actorB, "market_cancelled"))
	if err != nil || x.RejectionCode != SettlementRejectBetNotFound {
		t.Fatalf("tenant B must not reach tenant A's bet: %+v err=%v", x, err)
	}
	if got := betStatusOf(t, owner, fa, betA); got != BetStatusOpen {
		t.Fatalf("tenant A's bet changed: %s", got)
	}
	if r := mustSimulate(t, rt, fb.tenantID, voidEvent(betB, actorB, "market_cancelled")); r.BetStatus != BetStatusVoid {
		t.Fatalf("tenant B's own void: %+v", r)
	}
	if r := mustSimulate(t, rt, fa.tenantID, voidEvent(betA, actorA, "market_cancelled")); r.BetStatus != BetStatusVoid {
		t.Fatalf("tenant A's own void on a closed tenant: %+v", r)
	}
	srInvariants(t, owner, fa.tenantID)
	srInvariants(t, owner, fb.tenantID)
}

// Duplicate concurrent returns post exactly one void.
func TestStakeReturn_SportsbookConcurrentDuplicateVoidsPostOnce(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	f, actor, betID := newStdBet(t, owner)
	setTenantStatus(t, owner, f.tenantID, "suspended")
	const n = 6
	var wg sync.WaitGroup
	results := make([]SettlementResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = simulateSettlement(t, rt, f.tenantID, voidEvent(betID, actor, "push"))
		}()
	}
	close(start)
	wg.Wait()
	applied := 0
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("delivery %d: %v", i, errs[i])
		}
		switch {
		case !results[i].Rejected() && results[i].Result == SettlementResultApplied:
			applied++
		case !results[i].Rejected() && results[i].Result == SettlementResultReplayed:
		default:
			t.Fatalf("delivery %d: unexpected %+v", i, results[i])
		}
	}
	if applied != 1 {
		t.Fatalf("exactly one delivery must apply, got %d", applied)
	}
	if got := countByType(t, owner, f.tenantID, "sportsbook_void"); got != 1 {
		t.Fatalf("exactly one void posting expected, got %d", got)
	}
	assertNets(t, owner, f, 0, 0, 0)
	srInvariants(t, owner, f.tenantID)
}

// A status change racing a void, both orders, two real connections.
func TestStakeReturn_SportsbookStatusChangeRace(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)

	t.Run("status_change_in_flight_then_void", func(t *testing.T) {
		f, actor, betID := newStdBet(t, owner)
		updated, release, closerDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		var once sync.Once
		releaseOnce := func() { once.Do(func() { close(release) }) }
		t.Cleanup(releaseOnce)
		go func() {
			closerDone <- owner.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
				// Suspend (not close): the closure guard is exercised elsewhere.
				if _, err := tx.Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, f.tenantID); err != nil {
					return err
				}
				close(updated)
				<-release
				return nil
			})
		}()
		<-updated
		type out struct {
			res SettlementResult
			err error
		}
		posted := make(chan out, 1)
		go func() {
			r, err := simulateSettlement(t, rt, f.tenantID, voidEvent(betID, actor, "market_cancelled"))
			posted <- out{r, err}
		}()
		select {
		case o := <-posted:
			t.Fatalf("the void must wait for the in-flight status change: %+v err=%v", o.res, o.err)
		case <-time.After(500 * time.Millisecond):
		}
		releaseOnce()
		if err := <-closerDone; err != nil {
			t.Fatalf("status change: %v", err)
		}
		o := <-posted
		if o.err != nil || o.res.Rejected() || o.res.BetStatus != BetStatusVoid {
			t.Fatalf("the void must apply after the suspension commits: %+v err=%v", o.res, o.err)
		}
		srInvariants(t, owner, f.tenantID)
	})

	t.Run("void_in_flight_then_status_change_waits", func(t *testing.T) {
		f, actor, betID := newStdBet(t, owner)
		setTenantStatus(t, owner, f.tenantID, "suspended")
		inTx, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		releaseOnce := func() { once.Do(func() { close(release) }) }
		t.Cleanup(releaseOnce)
		restore := SetSettlementAfterPostHookForTest(func(context.Context, pgx.Tx) error {
			close(inTx)
			<-release
			return nil
		})
		defer restore()
		posted := make(chan error, 1)
		go func() {
			_, err := simulateSettlement(t, rt, f.tenantID, voidEvent(betID, actor, "data_error"))
			posted <- err
		}()
		select {
		case <-inTx:
		case err := <-posted:
			t.Fatalf("the void finished before reaching the in-flight hook: %v", err)
		}
		closed := make(chan error, 1)
		go func() {
			closed <- owner.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE tenants SET status = 'closed' WHERE id = $1`, f.tenantID)
				return err
			})
		}()
		select {
		case err := <-closed:
			t.Fatalf("the status change must wait for the in-flight void: %v", err)
		case <-time.After(500 * time.Millisecond):
		}
		releaseOnce()
		if err := <-posted; err != nil {
			t.Fatalf("void: %v", err)
		}
		if err := <-closed; err != nil {
			t.Fatalf("status change (the bet is now void, so closure is allowed): %v", err)
		}
		if got := betStatusOf(t, owner, f, betID); got != BetStatusVoid {
			t.Fatalf("bet status %s", got)
		}
		srInvariants(t, owner, f.tenantID)
	})
}
