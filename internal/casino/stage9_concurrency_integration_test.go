//go:build integration

// Stage 9 §10 - adversarial concurrency re-verification for the casino
// money path. This file deliberately adds only what the existing suites
// do NOT already prove under genuine concurrency:
//
//   - orchestrator_integration_test.go already proves concurrent DUPLICATE
//     bet deliveries post exactly one effect, and concurrent DISTINCT
//     rollbacks of one bet resolve to exactly one reversal;
//     adversarial_lock_stress_test.go widens the duplicate case to N
//     deliveries and to a self-exclusion race; failure_mode_matrix_
//     integration_test.go covers torn writes, replay-after-commit, and
//     tombstones. None of those cover two DISTINCT bets racing for one
//     wallet's balance (internal/sportsbook has that test; casino did
//     not), nor a win racing a rollback of the SAME round, nor two
//     distinct wins racing a locked round's single stake release, nor a
//     bet racing a WITHDRAWAL for one wallet (every prior "only one can
//     have the money" test races two calls inside ONE package).
//
// Every test here uses this codebase's own established deterministic
// concurrency technique (an uncommitted blocker transaction holding a row
// lock the racing calls must also take, plus a pg_stat_activity poll) so
// the interleaving is forced rather than hoped for, and asserts the same
// financial properties throughout: SUM(debits) == SUM(credits) for the
// round and tenant-wide, no duplicate posting, no lost update, no
// negative locked balance, and no hang.
package casino

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// s9CashProjectionRowID resolves the wallet's player_cash
// wallet_balance_projection row id (its ledger_account_id) - the row every
// player_cash-touching posting's AFTER INSERT trigger must update, and
// therefore the row a blocker transaction holds to force a racing caller
// to queue at a KNOWN point (immediately before its own ledger write)
// rather than at a sleep-guessed one.
func s9CashAccountID(t *testing.T, pool *db.Pool, f casinoFixture) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
		return err
	})
	if err != nil {
		t.Fatalf("resolve player_cash account: %v", err)
	}
	return id
}

// s9Blocker holds a FOR UPDATE lock on ledgerAccountID's projection row
// until release() is called. Returns once the lock is genuinely held.
func s9Blocker(t *testing.T, pool *db.Pool, tenantID, ledgerAccountID uuid.UUID) (release func(), blockerPID int32) {
	t.Helper()
	ready := make(chan struct{})
	proceed := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var d, c int64
			if err := tx.QueryRow(ctx,
				`SELECT debit_total, credit_total FROM wallet_balance_projection WHERE ledger_account_id = $1 FOR UPDATE`,
				ledgerAccountID).Scan(&d, &c); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&blockerPID); err != nil {
				return err
			}
			close(ready)
			<-proceed
			return nil
		})
	}()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("blocker transaction failed before acquiring its lock: %v", err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			close(proceed)
			if err := <-done; err != nil {
				t.Errorf("blocker transaction: %v", err)
			}
		})
	}, blockerPID
}

// s9LockedBalance reads the wallet's player_locked_cash net balance.
func s9LockedCashBalance(t *testing.T, pool *db.Pool, f casinoFixture) int64 {
	t.Helper()
	var balance int64
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(credit_total - debit_total), 0)::bigint FROM wallet_balance_projection
			  WHERE wallet_id = $1 AND account_type = 'player_locked_cash'`, f.walletID).Scan(&balance)
	})
	if err != nil {
		t.Fatalf("read player_locked_cash balance: %v", err)
	}
	return balance
}

// s9CountTxOfType counts this tenant's ledger_transactions rows of one
// type under one correlation id - the "no duplicate posting" assertion
// made against the table, never inferred from a balance.
func s9CountTxOfType(t *testing.T, pool *db.Pool, tenantID, correlationID uuid.UUID, txType ledger.TransactionType) int {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions
			  WHERE tenant_id = $1 AND correlation_id = $2 AND transaction_type = $3`,
			tenantID, correlationID, txType).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count %s transactions: %v", txType, err)
	}
	return count
}

// postLockedCashBet simulates §16.4's Step-1 locked-CASH bet shape
// (Dr player_cash stake / Cr player_locked_cash stake, transaction_type
// casino_bet) - the exact shape a future stake-locking casino postBet
// would write, and the one resolveWinOrigin's AccountPlayerLockedCash arm
// and postWinLockedCash exist to settle. Deliberately mirrors this
// package's existing postLockedBonusBet helper (bonus_settlement_
// integration_test.go), which does the same for the locked-BONUS shape,
// rather than inventing a new convention. Casino's own postBet is
// cash-only today (ADR 0025 §6), so this path is not reachable from a
// provider callback yet - but it is implemented, guarded
// (ErrLockAlreadyReleased), and tested sequentially, so its behaviour
// under concurrency is in scope for a production-readiness re-audit.
func postLockedCashBet(t *testing.T, pool *db.Pool, f casinoFixture, providerID, providerTxID, roundID string, stake int64) uuid.UUID {
	t.Helper()
	var txID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cashAcct, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		lockedCashAcct, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerLockedCash, "EUR")
		if err != nil {
			return err
		}
		result, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxCasinoBet,
			IdempotencyKey: providerID + ":" + providerTxID,
			ProviderID:     &providerID, ProviderTxID: &providerTxID,
			CorrelationID: roundCorrelationID(f.tenantID, providerID, roundID),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: cashAcct, Direction: ledger.Debit, Amount: stake},
				{LedgerAccountID: lockedCashAcct, Direction: ledger.Credit, Amount: stake},
			},
		})
		if err != nil {
			return err
		}
		txID = result.TransactionID
		return nil
	})
	if err != nil {
		t.Fatalf("post locked-cash bet: %v", err)
	}
	return txID
}

// --- 1. Two DISTINCT bets racing one wallet's balance ---------------------

// TestStage9_ConcurrentDistinctBetsOneWallet_ExactlyOneAccepted is the
// casino counterpart of internal/sportsbook's
// TestPlaceBet_ConcurrentPlacementsOnlyOneSucceeds, which had no casino
// equivalent: casino's existing concurrency tests all race DUPLICATE
// deliveries of ONE bet (idempotency), never two genuinely different bets
// competing for the same money.
//
// A wallet holding exactly one stake's worth must accept exactly one of
// two simultaneous, distinct bets - never both (an overdraft), never
// neither (a spurious double decline). lockCashBalance's
// SELECT ... FOR UPDATE on the player_cash projection row is what has to
// serialize them.
func TestStage9_ConcurrentDistinctBetsOneWallet_ExactlyOneAccepted(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 1_000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	release, blockerPID := s9Blocker(t, pool, f.tenantID, s9CashAccountID(t, pool, f))

	const n = 2
	results := make([]ReceiveCallbackResult, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := provider.CallbackPayload(f.tenantID, CallbackEventBet,
				"s9-bet-race-"+string(rune('a'+i)), "", "s9-round-race-"+string(rune('a'+i)), "game-1",
				1_000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				results[i], err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
				return err
			})
		}(i)
	}

	if !fmWaitForLockWaiters(t, pool, blockerPID, n) {
		release()
		wg.Wait()
		t.Fatal("timed out waiting for both concurrent bet deliveries to block on the uncommitted blocker row")
	}
	release()
	wg.Wait()

	var accepted, declined int
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: unexpected error: %v", i, errs[i])
		}
		switch results[i].Outcome {
		case OutcomeSucceeded:
			accepted++
		case OutcomeDeclined:
			if results[i].DeclineReason != "insufficient_funds" {
				t.Fatalf("goroutine %d: expected an insufficient_funds decline, got %q", i, results[i].DeclineReason)
			}
			declined++
		default:
			t.Fatalf("goroutine %d: unexpected outcome %q", i, results[i].Outcome)
		}
	}
	if accepted != 1 || declined != 1 {
		t.Fatalf("a wallet funded for exactly one stake accepted %d and declined %d of %d simultaneous distinct bets", accepted, declined, n)
	}
	if got := cashBalance(t, pool, f); got != 0 {
		t.Fatalf("expected player_cash 0 after exactly one 1000 stake on 1000, got %d (a negative balance means both bets debited)", got)
	}
	debits, credits := sumDebitsCredits(t, pool, f.tenantID)
	if debits != credits {
		t.Fatalf("invariant #1 violated: debits=%d credits=%d", debits, credits)
	}
}

// --- 2. A win racing a rollback of the SAME round -------------------------

// TestStage9_ConcurrentWinAndRollbackSameRound_SerializesToALegalOrder
// races a win callback against a rollback of the same round's bet, both
// delivered at the same instant. Sequentially the platform's rule is
// unambiguous (TestReceiveCallback_WinOnRolledBackBetRejected: a win whose
// round's bet is already reversed is an integrity alert, never paid). This
// test pins down what happens when the two overlap: the outcome must equal
// one of the two legal serial orders and nothing else -
//
//	rollback first: the win is refused (ErrBetNotFound) and the round's
//	                net effect is zero;
//	win first:      both post (the platform reverses exactly the
//	                transaction the provider named - the bet - and the win
//	                it was already told to pay stands).
//
// What must NEVER happen is a third outcome: the win being paid while its
// own transaction observed the reversal, a double reversal, a duplicate
// win, an unbalanced round, or a hang.
func TestStage9_ConcurrentWinAndRollbackSameRound_SerializesToALegalOrder(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 1_000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	const (
		round = "s9-round-win-vs-rollback"
		stake = int64(400)
		win   = int64(500)
	)
	betPayload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "s9-bet-wvr", "", round, "game-1", stake, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", betPayload)
		return err
	}); err != nil {
		t.Fatalf("seed bet: %v", err)
	}

	winPayload := provider.CallbackPayload(f.tenantID, CallbackEventWin, "s9-win-wvr", "", round, "game-1", win, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)
	rollbackPayload := provider.CallbackPayload(f.tenantID, CallbackEventRollback, "s9-rb-wvr", "s9-bet-wvr", round, "game-1", 0, "EUR", "", "", f.playerAccountID, uuid.Nil)

	release, blockerPID := s9Blocker(t, pool, f.tenantID, s9CashAccountID(t, pool, f))

	var wg sync.WaitGroup
	var winErr, rollbackErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		winErr = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", winPayload)
			return err
		})
	}()
	go func() {
		defer wg.Done()
		rollbackErr = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", rollbackPayload)
			return err
		})
	}()

	if !fmWaitForLockWaiters(t, pool, blockerPID, 2) {
		release()
		wg.Wait()
		t.Fatal("timed out waiting for the win and the rollback to both block")
	}
	release()
	wg.Wait()

	if rollbackErr != nil {
		t.Fatalf("the rollback must always succeed (it names a real, not-yet-reversed bet): %v", rollbackErr)
	}
	correlationID := roundCorrelationID(f.tenantID, "mock-casino", round)
	balance := cashBalance(t, pool, f)

	switch {
	case winErr == nil:
		// Legal order: win first, then the rollback of the bet it settled.
		if want := 1_000 - stake + win + stake; balance != want {
			t.Fatalf("win-first serialization: expected player_cash %d, got %d", want, balance)
		}
		if got := s9CountTxOfType(t, pool, f.tenantID, correlationID, ledger.TxCasinoWin); got != 1 {
			t.Fatalf("expected exactly 1 casino_win posting, got %d", got)
		}
	case errors.Is(winErr, ErrBetNotFound):
		// Legal order: rollback first - the win is refused outright.
		if balance != 1_000 {
			t.Fatalf("rollback-first serialization: expected the round's net effect to be zero (player_cash 1000), got %d", balance)
		}
		if got := s9CountTxOfType(t, pool, f.tenantID, correlationID, ledger.TxCasinoWin); got != 0 {
			t.Fatalf("a win refused with ErrBetNotFound must post nothing, got %d casino_win rows", got)
		}
	default:
		t.Fatalf("the win must either post or be refused with ErrBetNotFound; got %v", winErr)
	}

	if got := s9CountTxOfType(t, pool, f.tenantID, correlationID, ledger.TxCasinoRollback); got != 1 {
		t.Fatalf("expected exactly 1 casino_rollback posting, got %d", got)
	}
	fmAssertCorrelationBalanced(t, pool, f.tenantID, correlationID)
	debits, credits := sumDebitsCredits(t, pool, f.tenantID)
	if debits != credits {
		t.Fatalf("invariant #1 violated: debits=%d credits=%d", debits, credits)
	}
}

// --- 3. Two distinct wins racing one locked round's single release --------

// TestStage9_ConcurrentDistinctWinsOnLockedRound_ReleasesLockExactlyOnce
// is the concurrency proof for LF-18's guard (ErrLockAlreadyReleased,
// resolveWinOrigin outcome 6, proven sequentially by
// TestPostWin_LockAlreadyReleased): a round's locked stake is released to
// the player exactly once, no matter how many win callbacks for that round
// arrive, and no matter how they interleave.
//
// Before the Stage 9 fix this test was written against, that guard was a
// check-then-act read with no row lock: two simultaneous DISTINCT win
// callbacks for one locked round each observed "400 still locked" and each
// posted the full release, crediting the player the same stake twice and
// driving player_locked_cash to -400. Each individual posting balanced, so
// the tenant-wide SUM(debits) == SUM(credits) invariant did NOT catch it -
// which is exactly why a per-account assertion is made here too.
func TestStage9_ConcurrentDistinctWinsOnLockedRound_ReleasesLockExactlyOnce(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 1_000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	const (
		round = "s9-round-locked"
		stake = int64(400)
		win   = int64(100)
	)
	postLockedCashBet(t, pool, f, "mock-casino", "s9-bet-locked", round, stake)
	if got := s9LockedCashBalance(t, pool, f); got != stake {
		t.Fatalf("fixture: expected %d locked, got %d", stake, got)
	}

	release, blockerPID := s9Blocker(t, pool, f.tenantID, s9CashAccountID(t, pool, f))

	const n = 2
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := provider.CallbackPayload(f.tenantID, CallbackEventWin,
				"s9-win-locked-"+string(rune('a'+i)), "", round, "game-1",
				win, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
				return err
			})
		}(i)
	}

	if !fmWaitForLockWaiters(t, pool, blockerPID, n) {
		release()
		wg.Wait()
		t.Fatal("timed out waiting for both concurrent win deliveries to block on the uncommitted blocker row")
	}
	release()
	wg.Wait()

	var settled, refused int
	for i, err := range errs {
		switch {
		case err == nil:
			settled++
		case errors.Is(err, ErrLockAlreadyReleased):
			refused++
		default:
			t.Fatalf("win %d: expected either a settlement or ErrLockAlreadyReleased, got %v", i, err)
		}
	}
	if settled != 1 || refused != 1 {
		t.Fatalf("expected exactly one of %d simultaneous wins to release the round's lock (got settled=%d refused=%d)", n, settled, refused)
	}

	if got := s9LockedCashBalance(t, pool, f); got != 0 {
		t.Fatalf("expected player_locked_cash 0 after exactly one release of a %d stake, got %d (a negative balance means the stake was released twice)", stake, got)
	}
	// 1000 funded - 400 locked at bet time + 100 win + 400 released.
	if want, got := int64(1_000-stake+win+stake), cashBalance(t, pool, f); got != want {
		t.Fatalf("expected player_cash %d, got %d", want, got)
	}
	correlationID := roundCorrelationID(f.tenantID, "mock-casino", round)
	if got := s9CountTxOfType(t, pool, f.tenantID, correlationID, ledger.TxCasinoWin); got != 1 {
		t.Fatalf("expected exactly 1 casino_win posting for the round, got %d", got)
	}
	fmAssertCorrelationBalanced(t, pool, f.tenantID, correlationID)
	debits, credits := sumDebitsCredits(t, pool, f.tenantID)
	if debits != credits {
		t.Fatalf("invariant #1 violated: debits=%d credits=%d", debits, credits)
	}
}

// --- 4. A casino bet racing a withdrawal request on ONE wallet ------------

// s9HoldBalance reads the wallet's player_withdrawal_hold net balance -
// the account internal/withdrawal.RequestWithdrawal credits when it
// reserves funds. Mirrors internal/withdrawal's own holdBalance helper
// rather than importing it (test helpers are per-package by this repo's
// convention).
func s9HoldBalance(t *testing.T, pool *db.Pool, f casinoFixture) int64 {
	t.Helper()
	var balance int64
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(credit_total - debit_total), 0)::bigint FROM wallet_balance_projection
			  WHERE wallet_id = $1 AND account_type = 'player_withdrawal_hold'`, f.walletID).Scan(&balance)
	})
	if err != nil {
		t.Fatalf("read player_withdrawal_hold balance: %v", err)
	}
	return balance
}

// TestStage9_ConcurrentBetAndWithdrawalOneWallet_ExactlyOneReservesTheBalance
// is the cross-vertical case no existing suite covered: every prior
// "only one of two can have the money" test races two calls into the SAME
// package (two casino bets, two sportsbook bets, two withdrawal requests),
// so each proves only that ONE code path's own FOR UPDATE is correct. A
// player emptying their wallet into a withdrawal while simultaneously
// staking it on a game crosses a package boundary: internal/casino's
// lockCashBalance and internal/withdrawal's lockCashBalanceForUpdate are
// two independently written functions, and nothing outside this test
// asserts they take the SAME row lock on the SAME
// wallet_balance_projection row - which is the only reason the two
// verticals serialize against each other at all.
//
// A wallet funded for exactly one of the two must end with exactly one
// winner and a non-negative player_cash: never both (the player spends the
// same money twice - once as a stake, once as a payout), never neither.
// This package can import internal/withdrawal without a cycle
// (internal/withdrawal depends only on audit/db/ledger).
func TestStage9_ConcurrentBetAndWithdrawalOneWallet_ExactlyOneReservesTheBalance(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 1_000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	// Both want the whole 1000. Only one can have it.
	const amount = int64(1_000)

	release, blockerPID := s9Blocker(t, pool, f.tenantID, s9CashAccountID(t, pool, f))

	var wg sync.WaitGroup
	var betResult ReceiveCallbackResult
	var betErr, withdrawalErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "s9-bet-vs-wd", "", "s9-round-bet-vs-wd", "game-1",
			amount, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
		betErr = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			betResult, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
			return err
		})
	}()
	go func() {
		defer wg.Done()
		withdrawalErr = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := withdrawal.RequestWithdrawal(ctx, tx, withdrawal.RequestParams{
				TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, PersonID: f.personID, WalletID: f.walletID,
				AssetCode: "EUR", Amount: amount, IdempotencyKey: "s9-wd-vs-bet",
			})
			return err
		})
	}()

	if !fmWaitForLockWaiters(t, pool, blockerPID, 2) {
		release()
		wg.Wait()
		t.Fatal("timed out waiting for the bet and the withdrawal request to both block on the uncommitted blocker row")
	}
	release()
	wg.Wait()

	if betErr != nil {
		t.Fatalf("the bet callback must resolve to an outcome, never an error: %v", betErr)
	}
	betAccepted := betResult.Outcome == OutcomeSucceeded
	if !betAccepted && betResult.Outcome != OutcomeDeclined {
		t.Fatalf("unexpected bet outcome %q", betResult.Outcome)
	}
	if !betAccepted && betResult.DeclineReason != "insufficient_funds" {
		t.Fatalf("expected an insufficient_funds decline, got %q", betResult.DeclineReason)
	}

	var withdrawalAccepted bool
	switch {
	case withdrawalErr == nil:
		withdrawalAccepted = true
	case errors.Is(withdrawalErr, withdrawal.ErrInsufficientFunds):
	default:
		t.Fatalf("the withdrawal must either reserve the funds or be refused with ErrInsufficientFunds; got %v", withdrawalErr)
	}

	if betAccepted == withdrawalAccepted {
		t.Fatalf("a wallet funded for exactly one of the two saw bet_accepted=%t withdrawal_accepted=%t - "+
			"both means the same money was spent twice, neither means a spurious double decline",
			betAccepted, withdrawalAccepted)
	}

	if got := cashBalance(t, pool, f); got != 0 {
		t.Fatalf("expected player_cash 0 after exactly one 1000 reservation on 1000, got %d (negative means both sides debited)", got)
	}
	wantHold := int64(0)
	if withdrawalAccepted {
		wantHold = amount
	}
	if got := s9HoldBalance(t, pool, f); got != wantHold {
		t.Fatalf("expected player_withdrawal_hold %d, got %d", wantHold, got)
	}
	if got := s9CountTxOfType(t, pool, f.tenantID,
		roundCorrelationID(f.tenantID, "mock-casino", "s9-round-bet-vs-wd"), ledger.TxCasinoBet); got != map[bool]int{true: 1, false: 0}[betAccepted] {
		t.Fatalf("casino_bet postings inconsistent with the bet outcome (accepted=%t, rows=%d)", betAccepted, got)
	}
	debits, credits := sumDebitsCredits(t, pool, f.tenantID)
	if debits != credits {
		t.Fatalf("invariant #1 violated: debits=%d credits=%d", debits, credits)
	}
}
