//go:build integration

// Stage 8 (ADR 0080) financial-failure-mode matrix for the SPORTSBOOK
// money path. Companion to internal/casino/failure_mode_matrix_integration_
// test.go; same rules: existing deterministic mocks only, no external API
// call, and zero changes to the financial code under test (PlaceBet, the
// ledger postings it makes) to make anything below pass.
//
// # WHICH MATRIX ITEMS APPLY HERE, AND WHICH CANNOT
//
// Sportsbook bet placement is SYNCHRONOUS and SAME-PROCESS (ADR 0080
// Decision 2: `sportsbook.Provider` has no bet-placement method at all,
// and every bet's provider_id/provider_bet_reference is NULL). There is no
// provider round-trip, no webhook, no callback and no settlement path in
// the tree today. That makes several matrix items structurally
// inapplicable rather than merely untested - inventing a scenario for
// them would be inventing a mechanism:
//
//	A  APPLIES, in its atomicity sense only. There is no provider to
//	   "accept" a bet the platform then loses, but the underlying question
//	   - can a failure between the postings and the commit leave a torn
//	   write - is exactly as real here, and is tested below both by an
//	   aborted transaction and by a genuine mid-flight cancellation.
//	B  APPLIES as "the caller retries after a crash": the same
//	   idempotency key must yield one financial effect. Tested below for
//	   the crash-BEFORE-commit case (the case no existing test covers);
//	   the crash-after-commit case is already covered by
//	   TestPlaceBet_IdempotentRetrySameKeyOneEffect.
//	C  DOES NOT APPLY. "Duplicate callback delivered twice by the
//	   provider's own webhook infra" has no analogue: nothing external
//	   delivers anything. Its platform-side analogue (a client retrying
//	   the same request) is item B, already covered as above.
//	D  DOES NOT APPLY. Out-of-order callbacks require at least two
//	   asynchronous events per bet (bet/win, bet/settlement). This stage's
//	   sportsbook writes exactly one event per bet - placement - and never
//	   settles; there is no second event that could arrive first.
//	E  APPLIES. Tested below (zero/negative stake, unregistered asset).
//	F  DOES NOT APPLY. MockSportsbookProvider is consulted for catalogue
//	   data only; no call it makes is on the money path, so a provider
//	   5xx/transport failure cannot produce a partial financial effect.
//	   (It also has no error-injection hook - see the report's mock-gap
//	   note.)
//	G  NOT COVERED - `internal/providers.ProviderConfig.Enabled` exists
//	   (ADR 0080 Decision 4) but is deliberately not wired into any money
//	   path, by that package's own doc comment; there is nothing to test
//	   at the ledger level and fabricating a wiring would be inventing a
//	   mechanism. Its own unit tests cover the loader.
//	H  ALREADY COVERED by TestPlaceBet_SelectionNotFound (the sportsbook
//	   analogue of "names an unknown session/round").
//	I  ALREADY COVERED by TestPlaceBet_SameIdempotencyKeyDifferentPlayers
//	   NeverCollide and TestPlaceBet_LedgerIdempotencyKeyIsNamespacedBy
//	   TypeAndPlayer.
//	J/K APPLY at the tenant level and are tested below (the same
//	   client-chosen idempotency key used in two different tenants must
//	   produce two independent bets and no cross-tenant visibility or
//	   financial effect).
package sportsbook

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// fmSumDebitsCredits proves invariant #1 (CLAUDE.md: SUM(DEBITS) ==
// SUM(CREDITS)) across a tenant's whole ledger.
func fmSumDebitsCredits(t *testing.T, pool *db.Pool, tenantID uuid.UUID) (debits, credits int64) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0),
			        COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)
			   FROM ledger_entries WHERE tenant_id = $1`, tenantID).Scan(&debits, &credits)
	})
	if err != nil {
		t.Fatalf("sum debits/credits: %v", err)
	}
	return debits, credits
}

// fmCountEntriesForBetCorrelation counts the ledger entries written under
// one bet's own correlation id (PlaceBet uses the bet id as the
// correlation id) - the per-bet form of the balance assertion.
func fmCountEntriesForBetCorrelation(t *testing.T, pool *db.Pool, tenantID, correlationID uuid.UUID) (entries int, debits, credits int64) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*),
			        COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'debit'), 0),
			        COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'credit'), 0)
			   FROM ledger_entries e
			   JOIN ledger_transactions lt ON lt.id = e.ledger_transaction_id
			  WHERE e.tenant_id = $1 AND lt.correlation_id = $2`, tenantID, correlationID).Scan(&entries, &debits, &credits)
	})
	if err != nil {
		t.Fatalf("read bet correlation %s: %v", correlationID, err)
	}
	return entries, debits, credits
}

// --- A/B. Atomicity: a failure before commit leaves no trace, and the
// retry afterwards produces exactly one financial effect ---

// TestFailureModeMatrix_SB_A_AbortBeforeCommitLeavesNoTraceAndRetryPostsOnce
// is the sportsbook form of matrix item A. PlaceBet writes its ledger
// postings (Dr player_cash / Cr player_locked_cash) AND its
// sportsbook_bets row inside the caller's single transaction, so a
// failure before commit must leave neither - not a locked stake with no
// bet, and not a bet row with no lock.
//
// The retry half (item B) is the operationally important one: the caller
// retries with the SAME idempotency key after the crash, and exactly one
// bet and one ledger transaction exist afterwards. Distinct from
// TestPlaceBet_IdempotentRetrySameKeyOneEffect, which retries after a
// COMMITTED first attempt; here the first attempt left nothing behind, so
// the retry must genuinely place the bet rather than replay it.
func TestFailureModeMatrix_SB_A_AbortBeforeCommitLeavesNoTraceAndRetryPostsOnce(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 5_000)
	sel := seedSelection(t, pool, seedSelectionParams{})

	const key = "fm-sb-a-abort"
	errBoom := errors.New("simulated platform failure after the stake was locked")
	var placed PlaceBetResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var callErr error
		placed, callErr = PlaceBet(ctx, tx, PlaceBetParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			SelectionID: sel.ID, AssetCode: "EUR", StakeAmount: 1_000,
			ExpectedOddsNumerator: sel.OddsNumerator, ExpectedOddsDenominator: sel.OddsDenominator,
			IdempotencyKey: key,
		})
		if callErr != nil {
			return callErr
		}
		if !placed.Accepted {
			return errors.New("fixture precondition failed: the bet was not accepted before the simulated crash")
		}
		return errBoom
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("expected the simulated platform failure to propagate, got %v", err)
	}

	if count := countBets(t, pool, f); count != 0 {
		t.Fatalf("a rolled-back placement must leave ZERO sportsbook_bets rows, got %d", count)
	}
	if count := countLedgerTransactions(t, pool, f); count != 0 {
		t.Fatalf("a rolled-back placement must leave ZERO ledger transactions, got %d", count)
	}
	if balance := cashBalance(t, pool, f); balance != 5_000 {
		t.Fatalf("expected cash untouched at 5000, got %d", balance)
	}
	if locked := lockedCashBalance(t, pool, f); locked != 0 {
		t.Fatalf("expected zero locked stake after the rollback, got %d", locked)
	}

	// The caller retries with the same idempotency key.
	retry, err := placeBet(t, pool, f, sel, 1_000, key)
	if err != nil {
		t.Fatalf("retry after the simulated crash: %v", err)
	}
	if !retry.Accepted {
		t.Fatalf("expected the retried placement to be accepted, got %+v", retry)
	}
	if count := countBets(t, pool, f); count != 1 {
		t.Fatalf("expected exactly one bet after the retry, got %d", count)
	}
	if count := countLedgerTransactions(t, pool, f); count != 1 {
		t.Fatalf("expected exactly one ledger transaction after the retry, got %d", count)
	}
	entries, debits, credits := fmCountEntriesForBetCorrelation(t, pool, f.tenantID, retry.Bet.ID)
	if entries != 2 || debits != credits || debits != 1_000 {
		t.Fatalf("expected a balanced two-entry 1000/1000 placement, got entries=%d debits=%d credits=%d", entries, debits, credits)
	}
	if balance := cashBalance(t, pool, f); balance != 4_000 {
		t.Fatalf("expected 5000-1000=4000 cash after the retry, got %d", balance)
	}
	if locked := lockedCashBalance(t, pool, f); locked != 1_000 {
		t.Fatalf("expected exactly one stake locked, got %d", locked)
	}
	if debits, credits := fmSumDebitsCredits(t, pool, f.tenantID); debits != credits {
		t.Fatalf("invariant #1 violated: debits=%d credits=%d", debits, credits)
	}
}

// TestFailureModeMatrix_SB_A_ContextCancelledMidPlacementLeavesNoPartialWrite
// cancels a placement that is GENUINELY MID-FLIGHT inside PlaceBet,
// deterministically rather than by sleeping: a blocker transaction holds
// the same `wallet_balance_projection ... FOR UPDATE` row lock PlaceBet's
// own lockCashBalance takes, so the real call blocks inside PlaceBet -
// after its validation and policy checks, before its ledger postings -
// and is cancelled exactly there (confirmed blocked via
// pg_stat_activity, the technique TestPlaceBet_ConcurrentPlacementsOnly
// OneSucceeds already established in this package).
//
// Proves there is no interleaving in which a cancelled placement leaves a
// debit without its credit, a locked stake without a bet row, or a bet row
// without a lock.
func TestFailureModeMatrix_SB_A_ContextCancelledMidPlacementLeavesNoPartialWrite(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 5_000)
	sel := seedSelection(t, pool, seedSelectionParams{})

	var cashAccountID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		cashAccountID, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
		return err
	}); err != nil {
		t.Fatalf("resolve cash account: %v", err)
	}

	blockerCtx := context.Background()
	blockerTx, err := pool.Raw().Begin(blockerCtx)
	if err != nil {
		t.Fatalf("begin blocker tx: %v", err)
	}
	defer func() { _ = blockerTx.Rollback(blockerCtx) }()
	if _, err := blockerTx.Exec(blockerCtx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenantID.String()); err != nil {
		t.Fatalf("set tenant context on blocker tx: %v", err)
	}
	var ignored int64
	if err := blockerTx.QueryRow(blockerCtx,
		`SELECT debit_total FROM wallet_balance_projection WHERE ledger_account_id = $1 FOR UPDATE`,
		cashAccountID).Scan(&ignored); err != nil {
		t.Fatalf("blocker lock on wallet_balance_projection: %v", err)
	}
	blockerPIDValue, err := backendPID(blockerCtx, blockerTx)
	if err != nil {
		t.Fatalf("read blocker pg_backend_pid: %v", err)
	}

	cancelCtx, cancel := context.WithCancel(context.Background())
	placeErr := make(chan error, 1)
	go func() {
		placeErr <- pool.WithTenant(cancelCtx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := PlaceBet(ctx, tx, PlaceBetParams{
				TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
				SelectionID: sel.ID, AssetCode: "EUR", StakeAmount: 1_500,
				ExpectedOddsNumerator: sel.OddsNumerator, ExpectedOddsDenominator: sel.OddsDenominator,
				IdempotencyKey: "fm-sb-a-cancel",
			})
			return err
		})
	}()

	if !waitForBlockedCount(t, pool, blockerPIDValue, 1) {
		cancel()
		<-placeErr
		t.Fatal("placement never blocked on the balance row lock; cannot cancel it mid-flight deterministically")
	}
	cancel()
	select {
	case err := <-placeErr:
		if err == nil {
			t.Fatal("expected the cancelled placement to fail, got nil")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("cancelled placement did not return")
	}
	if err := blockerTx.Rollback(blockerCtx); err != nil {
		t.Fatalf("release blocker: %v", err)
	}

	if count := countBets(t, pool, f); count != 0 {
		t.Fatalf("a cancelled placement must leave ZERO sportsbook_bets rows, got %d", count)
	}
	if count := countLedgerTransactions(t, pool, f); count != 0 {
		t.Fatalf("a cancelled placement must leave ZERO ledger transactions, got %d", count)
	}
	if balance := cashBalance(t, pool, f); balance != 5_000 {
		t.Fatalf("expected cash untouched at 5000, got %d", balance)
	}
	if locked := lockedCashBalance(t, pool, f); locked != 0 {
		t.Fatalf("expected zero locked stake, got %d", locked)
	}
	if debits, credits := fmSumDebitsCredits(t, pool, f.tenantID); debits != credits {
		t.Fatalf("invariant #1 violated: debits=%d credits=%d", debits, credits)
	}
}

// --- E. Malformed transaction ---

// TestFailureModeMatrix_SB_E_MalformedStakeAndAssetRejectedCleanly covers
// matrix item E for bet placement: a zero stake, a negative stake, and an
// asset code that is not in the `assets` registry. Each must be a clean
// rejection with no bet row and no ledger effect - never a panic, never a
// zero-value posting, never a partial write.
//
// The unregistered-asset case is rejected before the assets registry is
// even consulted: ledger_accounts' own trigger (migration 0020) refuses to
// create a wallet-owned account in an asset the WALLET does not hold. The
// registry foreign key behind it is the second line of defence.
func TestFailureModeMatrix_SB_E_MalformedStakeAndAssetRejectedCleanly(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 5_000)
	sel := seedSelection(t, pool, seedSelectionParams{})

	cases := []struct {
		name      string
		stake     int64
		assetCode string
		key       string
	}{
		{"zero stake", 0, "EUR", "fm-sb-e-zero"},
		{"negative stake", -1_000, "EUR", "fm-sb-e-negative"},
		{"unregistered asset", 1_000, "ZZZ", "fm-sb-e-asset"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := PlaceBet(ctx, tx, PlaceBetParams{
					TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
					SelectionID: sel.ID, AssetCode: tc.assetCode, StakeAmount: tc.stake,
					ExpectedOddsNumerator: sel.OddsNumerator, ExpectedOddsDenominator: sel.OddsDenominator,
					IdempotencyKey: tc.key,
				})
				return err
			})
			if err == nil {
				t.Fatal("expected a malformed placement to be rejected")
			}
			if count := countBets(t, pool, f); count != 0 {
				t.Fatalf("a malformed placement must write no bet row, got %d", count)
			}
			if count := countLedgerTransactions(t, pool, f); count != 0 {
				t.Fatalf("a malformed placement must post nothing, got %d ledger transactions", count)
			}
			if balance := cashBalance(t, pool, f); balance != 5_000 {
				t.Fatalf("expected cash untouched at 5000, got %d", balance)
			}
			if locked := lockedCashBalance(t, pool, f); locked != 0 {
				t.Fatalf("expected zero locked stake, got %d", locked)
			}
		})
	}

	if debits, credits := fmSumDebitsCredits(t, pool, f.tenantID); debits != credits {
		t.Fatalf("invariant #1 violated: debits=%d credits=%d", debits, credits)
	}
}

// --- J/K. Cross-tenant reference ---

// TestFailureModeMatrix_SB_JK_SameIdempotencyKeyInTwoTenantsNeverCollides
// is the tenant-level counterpart to the existing cross-PLAYER test
// (TestPlaceBet_SameIdempotencyKeyDifferentPlayersNeverCollide): the exact
// same client-chosen idempotency key, used by a player in tenant A and a
// player in tenant B against the same platform-level selection, must
// produce two independent bets with two independent ledger transactions -
// and neither tenant may see or be financially affected by the other's.
//
// The isolation is structural, not an application check: sportsbook_bets'
// and ledger_transactions' idempotency scoping both carry tenant_id, and
// every read runs inside an RLS-scoped transaction, so tenant B's lookup
// cannot see tenant A's row at all.
func TestFailureModeMatrix_SB_JK_SameIdempotencyKeyInTwoTenantsNeverCollides(t *testing.T) {
	pool := testPool(t)
	fA := seedFixture(t, pool)
	fB := seedFixture(t, pool)
	fundWallet(t, pool, fA, 5_000)
	fundWallet(t, pool, fB, 5_000)
	sel := seedSelection(t, pool, seedSelectionParams{})

	const sharedKey = "fm-sb-jk-shared-key"
	resultA, err := placeBet(t, pool, fA, sel, 1_000, sharedKey)
	if err != nil {
		t.Fatalf("tenant A placement: %v", err)
	}
	if !resultA.Accepted {
		t.Fatalf("expected tenant A's bet to be accepted, got %+v", resultA)
	}
	resultB, err := placeBet(t, pool, fB, sel, 2_000, sharedKey)
	if err != nil {
		t.Fatalf("tenant B placement with the same idempotency key: %v", err)
	}
	if !resultB.Accepted {
		t.Fatalf("expected tenant B's bet to be accepted independently, got %+v", resultB)
	}
	if resultA.Bet.ID == resultB.Bet.ID {
		t.Fatal("two tenants' bets must never be the same row")
	}
	if resultA.Bet.LedgerTransactionID == resultB.Bet.LedgerTransactionID {
		t.Fatal("two tenants' bets must never share a ledger transaction")
	}

	if count := countBets(t, pool, fA); count != 1 {
		t.Fatalf("expected exactly one bet in tenant A, got %d", count)
	}
	if count := countBets(t, pool, fB); count != 1 {
		t.Fatalf("expected exactly one bet in tenant B, got %d", count)
	}
	if balance := cashBalance(t, pool, fA); balance != 4_000 {
		t.Fatalf("tenant A: expected 5000-1000=4000 cash, got %d", balance)
	}
	if balance := cashBalance(t, pool, fB); balance != 3_000 {
		t.Fatalf("tenant B: expected 5000-2000=3000 cash, got %d", balance)
	}

	// Tenant B's scope cannot see tenant A's bet at all.
	var visible int
	if err := pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM sportsbook_bets WHERE id = $1`, resultA.Bet.ID).Scan(&visible)
	}); err != nil {
		t.Fatalf("cross-tenant sportsbook_bets read: %v", err)
	}
	if visible != 0 {
		t.Fatalf("tenant B must not see tenant A's bet, got %d rows", visible)
	}
	if err := pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE id = $1`, resultA.Bet.LedgerTransactionID).Scan(&visible)
	}); err != nil {
		t.Fatalf("cross-tenant ledger_transactions read: %v", err)
	}
	if visible != 0 {
		t.Fatalf("tenant B must not see tenant A's ledger transaction, got %d rows", visible)
	}

	for _, f := range []sbFixture{fA, fB} {
		if debits, credits := fmSumDebitsCredits(t, pool, f.tenantID); debits != credits {
			t.Fatalf("invariant #1 violated for tenant %s: debits=%d credits=%d", f.tenantID, debits, credits)
		}
	}
}
