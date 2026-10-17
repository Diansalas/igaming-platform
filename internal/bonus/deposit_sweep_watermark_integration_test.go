//go:build integration

// BONUS-SWEEP-WATERMARK-SKEW-1 (NA-4): ledger_transactions.posted_at is
// DEFAULT now(), i.e. the TRANSACTION START time, not the commit time. A
// deposit transaction that began before, but commits after, a sweep that
// already advanced its (posted_at, id) watermark past it was skipped
// permanently. These tests interleave two real transactions to reproduce
// that, and prove the bounded late-commit re-scan closes it without ever
// double-granting.
package bonus

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

func runSweepOnce(t *testing.T, pool *db.Pool, f lifecycleFixture) DepositSweepOutcome {
	t.Helper()
	var out DepositSweepOutcome
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		out, e = RunDepositSweepForTenant(ctx, tx, f.tenantID, uuid.Nil)
		return e
	})
	if err != nil {
		t.Fatalf("RunDepositSweepForTenant: %v", err)
	}
	return out
}

func grantCountFor(t *testing.T, pool *db.Pool, f lifecycleFixture, depositTx uuid.UUID) int {
	t.Helper()
	var n int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM bonus_grants WHERE tenant_id = $1 AND trigger_reference = $2`, f.tenantID, depositTx.String()).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count grants: %v", err)
	}
	return n
}

var depositIDCh chan uuid.UUID

// startSlowDeposit begins a deposit transaction for (playerID, walletID)
// whose now() is pinned at its start, posts the deposit, then HOLDS the
// transaction open until release is closed. It returns the deposit's
// ledger tx id once posted (still uncommitted) and a done channel.
func startSlowDeposit(t *testing.T, pool *db.Pool, f lifecycleFixture, playerID, walletID uuid.UUID, amount int64, release <-chan struct{}) (posted <-chan uuid.UUID, done <-chan error) {
	t.Helper()
	postedCh := make(chan uuid.UUID, 1)
	depositIDCh = make(chan uuid.UUID, 1)
	doneCh := make(chan error, 1)
	go func() {
		doneCh <- pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var pin int
			if err := tx.QueryRow(ctx, `SELECT 1`).Scan(&pin); err != nil { // pins now() = tx start
				return err
			}
			intentID := uuid.New()
			if _, err := tx.Exec(ctx, `
				INSERT INTO deposit_intents (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, idempotency_key, status)
				VALUES ($1, $2, $3, $4, $5, $6, $7, 'card', $8, 'succeeded')`,
				intentID, f.tenantID, f.brandID, playerID, walletID, f.assetCode, amount, "idem-"+intentID.String()); err != nil {
				return err
			}
			clearing, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountPSPClearing, f.assetCode)
			if err != nil {
				return err
			}
			cash, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &walletID, ledger.AccountPlayerCash, f.assetCode)
			if err != nil {
				return err
			}
			// The realistic skew: the tx has already started (posted_at
			// pinned) but reaches ledger.Post (which locks the shared
			// projection rows) only after B committed and the sweep ran.
			postedCh <- uuid.Nil
			<-release
			res, err := ledger.Post(ctx, tx, ledger.TransactionInput{
				TenantID: f.tenantID, TransactionType: ledger.TxDeposit, IdempotencyKey: "mock-psp:" + intentID.String(),
				CorrelationID: intentID,
				Entries: []ledger.EntryInput{
					{LedgerAccountID: clearing, Direction: ledger.Debit, Amount: amount},
					{LedgerAccountID: cash, Direction: ledger.Credit, Amount: amount},
				},
			})
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE deposit_intents SET ledger_transaction_id = $2 WHERE id = $1`, intentID, res.TransactionID); err != nil {
				return err
			}
			depositIDCh <- res.TransactionID
			return nil
		})
	}()
	return postedCh, doneCh
}

func TestRunDepositSweepForTenant_LateCommittingDepositIsNotSkippedByWatermark(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	seedDepositMatchableOffer(t, pool, f, false, nil)
	secondPlayer, secondWallet := seedSecondPlayerAccountSamePerson(t, pool, f)

	// Warm-up deposit so shared ledger accounts exist (no uncommitted
	// account-creation contention between A and B) and a watermark exists.
	seedRawDeposit(t, pool, f, 1000, "card")
	runSweepOnce(t, pool, f)

	// A starts FIRST (earlier posted_at), posts, stays uncommitted.
	release := make(chan struct{})
	posted, done := startSlowDeposit(t, pool, f, secondPlayer, secondWallet, 20000, release)
	<-posted // A has started (now() pinned) but has not posted yet

	// B starts later (later posted_at) and commits; the sweep runs and
	// advances its watermark past A's posted_at while A is invisible.
	depositB := seedRawDeposit(t, pool, f, 30000, "card")
	runSweepOnce(t, pool, f)
	if got := grantCountFor(t, pool, f, depositB); got != 1 {
		t.Fatalf("deposit B: expected 1 grant, got %d", got)
	}

	// A commits only now.
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("slow deposit A: %v", err)
	}
	depositA := <-depositIDCh

	runSweepOnce(t, pool, f)
	if got := grantCountFor(t, pool, f, depositA); got != 1 {
		t.Fatalf("BONUS-SWEEP-WATERMARK-SKEW-1: late-committing deposit A was skipped permanently (grants=%d, want 1)", got)
	}
	// Re-scans never duplicate (B and A stay at exactly one grant).
	for i := 0; i < 3; i++ {
		runSweepOnce(t, pool, f)
	}
	if a, b := grantCountFor(t, pool, f, depositA), grantCountFor(t, pool, f, depositB); a != 1 || b != 1 {
		t.Fatalf("repeated sweeps double-granted: A=%d B=%d", a, b)
	}
}

func TestRunDepositSweepForTenant_RescanIdempotentUnderConcurrencyRetryAndPartialFailure(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	seedDepositMatchableOffer(t, pool, f, false, nil)
	var deposits []uuid.UUID
	for i := 0; i < 4; i++ {
		deposits = append(deposits, seedRawDeposit(t, pool, f, int64(1000*(i+1)), "card"))
	}
	runSweepOnce(t, pool, f)

	// Partial failure: a sweep tx that does real re-scan work and then
	// fails must roll back entirely, leaving no grants/watermark change.
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := RunDepositSweepForTenant(ctx, tx, f.tenantID, uuid.Nil); err != nil {
			t.Errorf("sweep in failing tx: %v", err)
		}
		return fmt.Errorf("injected failure after sweep")
	})

	// Concurrent overlapping sweeps (no advisory lock: worst case).
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, e := RunDepositSweepForTenant(ctx, tx, f.tenantID, uuid.Nil)
				return e
			})
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatalf("concurrent sweep: %v", e)
		}
	}
	// Retry.
	runSweepOnce(t, pool, f)
	for _, d := range deposits {
		if got := grantCountFor(t, pool, f, d); got != 1 {
			t.Fatalf("deposit %s: expected exactly 1 grant, got %d", d, got)
		}
	}
}

// A deposit already granted must not be re-evaluated at all by the
// re-scan (no duplicate audit noise, no outcome inflation).
func TestRunDepositSweepForTenant_RescanSkipsAlreadyGrantedDeposits(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	seedDepositMatchableOffer(t, pool, f, false, nil)
	seedRawDeposit(t, pool, f, 5000, "card")
	first := runSweepOnce(t, pool, f)
	if first.EventsProcessed != 1 {
		t.Fatalf("first: want 1 event, got %d", first.EventsProcessed)
	}
	second := runSweepOnce(t, pool, f)
	if second.EventsProcessed != 0 || second.GrantsDenied != 0 || second.GrantsIssued != 0 {
		t.Fatalf("re-scan re-evaluated an already-granted deposit: %+v", second)
	}
}

func grantCountForPlayerOffer(t *testing.T, pool *db.Pool, f lifecycleFixture, playerID, offerID uuid.UUID) int {
	t.Helper()
	var n int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM bonus_grants WHERE tenant_id = $1 AND player_account_id = $2 AND offer_id = $3`, f.tenantID, playerID, offerID).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count grants for player/offer: %v", err)
	}
	return n
}

// BONUS-FIRST-DEPOSIT-DOUBLE-GRANT-1 (ledger-finance review condition C1):
// after the NA-4 bounded re-scan, a late-committing EARLIER deposit also
// qualified as "first deposit" (isFirstDeposit saw nothing earlier) and
// received a second FirstDepositOnly Grant for the same player and offer.
// SEMANTICS (no retroactive swap): the first deposit to be GRANTED holds
// the one FirstDepositOnly Grant for (tenant, player, offer); a deposit
// that commits late with an earlier posted_at never replaces or duplicates
// it. Status is irrelevant (a cancelled/forfeited Grant still consumes it).
func TestRunDepositSweepForTenant_LateEarlierDepositDoesNotGetSecondFirstDepositOnlyGrant(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedDepositMatchableOffer(t, pool, f, true, nil)
	secondPlayer, secondWallet := seedSecondPlayerAccountSamePerson(t, pool, f)

	// Warm-up on the OTHER account so shared ledger accounts exist and a
	// watermark exists, without consuming secondPlayer's first deposit.
	seedRawDeposit(t, pool, f, 1000, "card")
	runSweepOnce(t, pool, f)
	// Pre-create (committed) the second wallet's cash account so A and B do not contend on its uncommitted creation.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, e := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &secondWallet, ledger.AccountPlayerCash, f.assetCode)
		return e
	}); err != nil {
		t.Fatalf("pre-create account: %v", err)
	}

	release := make(chan struct{})
	posted, done := startSlowDeposit(t, pool, f, secondPlayer, secondWallet, 20000, release)
	<-posted // A (the genuinely earliest deposit) has started but not posted

	depositB := seedRawDepositForAccount(t, pool, f, secondPlayer, secondWallet, 30000, "card")
	runSweepOnce(t, pool, f)
	if got := grantCountFor(t, pool, f, depositB); got != 1 {
		t.Fatalf("deposit B (first visible deposit): expected 1 grant, got %d", got)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("slow deposit A: %v", err)
	}
	depositA := <-depositIDCh

	for i := 0; i < 3; i++ { // late re-scan plus retries
		runSweepOnce(t, pool, f)
	}
	if got := grantCountFor(t, pool, f, depositA); got != 0 {
		t.Fatalf("BONUS-FIRST-DEPOSIT-DOUBLE-GRANT-1: late earlier deposit A received a second FirstDepositOnly grant (grants=%d, want 0)", got)
	}
	if got := grantCountForPlayerOffer(t, pool, f, secondPlayer, co.offerID); got != 1 {
		t.Fatalf("expected exactly ONE FirstDepositOnly grant for (player, offer), got %d", got)
	}
}

// Same late-commit scenario, NON-FirstDepositOnly offer: every deposit still earns its own Grant (the C1 fix must not
// leak into offers without the first-deposit rule).
func TestRunDepositSweepForTenant_LateEarlierDepositStillGrantedForNonFirstDepositOnlyOffer(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedDepositMatchableOffer(t, pool, f, false, nil)
	secondPlayer, secondWallet := seedSecondPlayerAccountSamePerson(t, pool, f)
	seedRawDeposit(t, pool, f, 1000, "card")
	runSweepOnce(t, pool, f)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, e := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &secondWallet, ledger.AccountPlayerCash, f.assetCode)
		return e
	}); err != nil {
		t.Fatalf("pre-create account: %v", err)
	}
	release := make(chan struct{})
	posted, done := startSlowDeposit(t, pool, f, secondPlayer, secondWallet, 20000, release)
	<-posted
	depositB := seedRawDepositForAccount(t, pool, f, secondPlayer, secondWallet, 30000, "card")
	runSweepOnce(t, pool, f)
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("slow deposit A: %v", err)
	}
	depositA := <-depositIDCh
	for i := 0; i < 3; i++ {
		runSweepOnce(t, pool, f)
	}
	if a, b := grantCountFor(t, pool, f, depositA), grantCountFor(t, pool, f, depositB); a != 1 || b != 1 {
		t.Fatalf("non-FirstDepositOnly offer must grant per deposit: A=%d B=%d", a, b)
	}
	if got := grantCountForPlayerOffer(t, pool, f, secondPlayer, co.offerID); got != 2 {
		t.Fatalf("want 2 grants for (player, offer), got %d", got)
	}
}

// Overlapping sweeps (no advisory lock: worst case) after a late earlier deposit: the application rule plus the 0173
// backstop leave exactly ONE FirstDepositOnly Grant. A losing racer may only fail with unique_violation (23505).
func TestRunDepositSweepForTenant_LateEarlierDepositOverlappingSweepsSingleFirstDepositGrant(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedDepositMatchableOffer(t, pool, f, true, nil)
	secondPlayer, secondWallet := seedSecondPlayerAccountSamePerson(t, pool, f)
	seedRawDeposit(t, pool, f, 1000, "card")
	runSweepOnce(t, pool, f)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, e := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &secondWallet, ledger.AccountPlayerCash, f.assetCode)
		return e
	}); err != nil {
		t.Fatalf("pre-create account: %v", err)
	}
	release := make(chan struct{})
	posted, done := startSlowDeposit(t, pool, f, secondPlayer, secondWallet, 20000, release)
	<-posted
	seedRawDepositForAccount(t, pool, f, secondPlayer, secondWallet, 30000, "card")
	runSweepOnce(t, pool, f)
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("slow deposit A: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, e := RunDepositSweepForTenant(ctx, tx, f.tenantID, uuid.Nil)
				return e
			})
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		var pgErr *pgconn.PgError
		if e != nil && !(errors.As(e, &pgErr) && pgErr.Code == pgUniqueViolation) {
			t.Fatalf("overlapping sweep failed with something other than unique_violation: %v", e)
		}
	}
	runSweepOnce(t, pool, f) // retry
	if got := grantCountForPlayerOffer(t, pool, f, secondPlayer, co.offerID); got != 1 {
		t.Fatalf("expected exactly ONE FirstDepositOnly grant under overlapping sweeps, got %d", got)
	}
}
