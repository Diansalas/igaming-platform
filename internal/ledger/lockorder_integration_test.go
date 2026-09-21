//go:build integration

// ADR 0082 (canonical financial lock ordering) - the ledger-owned half of
// the test plan: §6 tests 5, 6, 7, 8 and 11. The call-site halves (tests
// 1-4) live in internal/casino, internal/payments and internal/withdrawal,
// where the production entry points they exercise live.
package ledger

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// loNewWallet adds a SECOND PLAYER (person + player_account + wallet) to
// an existing fixture's tenant/brand, so a test can construct postings
// whose CALLER entries are genuinely disjoint while their generated
// mirror legs still collide on the same house-level accounts.
//
// A second wallet, not a second player, is not an option: the multi-wallet
// model (docs/decisions/0007) gives a player exactly ONE wallet per asset,
// enforced by wallets_player_account_id_asset_code_key.
func loNewWallet(t *testing.T, pool *db.Pool, f fixture, assetCode string) uuid.UUID {
	t.Helper()
	personID := uuid.New()
	playerAccountID := uuid.New()
	walletID := uuid.New()

	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	}); err != nil {
		t.Fatalf("seed extra person: %v", err)
	}

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			playerAccountID, f.tenantID, f.brandID, personID, playerAccountID.String()+"@example.com"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, $5)`,
			walletID, f.tenantID, f.brandID, playerAccountID, assetCode)
		return err
	})
	if err != nil {
		t.Fatalf("seed extra player wallet: %v", err)
	}
	return walletID
}

func loAccount(t *testing.T, pool *db.Pool, f fixture, walletID *uuid.UUID, at AccountType, asset string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = GetOrCreateAccount(ctx, tx, f.tenantID, walletID, at, asset)
		return err
	})
	if err != nil {
		t.Fatalf("resolve %s account: %v", at, err)
	}
	return id
}

func loHasProjectionRow(t *testing.T, pool *db.Pool, tenantID, accountID uuid.UUID) bool {
	t.Helper()
	var exists bool
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM wallet_balance_projection WHERE ledger_account_id = $1)`, accountID).Scan(&exists)
	})
	if err != nil {
		t.Fatalf("check projection row: %v", err)
	}
	return exists
}

func loReason(s string) *string { return &s }

// --- §6 test 5 -------------------------------------------------------------

// TestLockOrder_ConcurrentFirstPostingsCreateSameAccounts_NoDeadlock is
// ADR 0082 R6's own justification, made executable.
//
// A ledger account with no entries yet has NO wallet_balance_projection
// row, and `SELECT ... FOR UPDATE` on an absent row locks NOTHING. Before
// this fix the row was therefore created at class L4 by migration 0023's
// AFTER INSERT trigger - one INSERT ... ON CONFLICT per entry, in the
// entry slice's order - so a transaction whose FIRST EVER posting touches
// a brand-new account creates that row out of canonical order, and an
// uncommitted index entry is a lock wait for deadlock purposes exactly
// like a row lock (ADR 0082 §Context, mechanism 3).
//
// The scenario is a first-ever posting to a NEW account (accNew, no
// projection row) crossed with an EXISTING one (accSeeded), driven from
// two call sites in opposite entry orders. Shown failing on HEAD with
// SQLSTATE 40P01 before the fix.
//
// Why the pairing is new-against-existing rather than new-against-new,
// which is worth recording because the obvious construction does NOT
// work: a waiter on an uncommitted INDEX entry waits on the inserting
// transaction's XID, and when that transaction ends Postgres wakes EVERY
// waiter at once rather than granting in queue order. Two racers both
// blocked on the same uncommitted new row therefore re-race on wake-up,
// and the cycle forms only about half the time - a test that
// intermittently passes against a deliberately un-fixed build proves
// nothing. Crossing the new row with an EXISTING row restores
// determinism, because a row lock on a live tuple IS granted in queue
// order: A is guaranteed to win accSeeded, and B is guaranteed to already
// hold accNew.
func TestLockOrder_ConcurrentFirstPostingsCreateSameAccounts_NoDeadlock(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	accSeeded := loAccount(t, pool, f, &f.walletID, AccountPlayerLockedCash, "EUR")
	accNew := loAccount(t, pool, f, &f.walletID, AccountPlayerWithdrawalHold, "EUR")

	// Give accSeeded a projection row (and only accSeeded).
	mustPost(t, pool, f, TransactionInput{
		TenantID: f.tenantID, TransactionType: TxCasinoBet,
		IdempotencyKey: "lockorder-newacct-seed", CorrelationID: uuid.New(),
		Entries: []EntryInput{
			{LedgerAccountID: f.cashAccountID, Direction: Debit, Amount: 2_000},
			{LedgerAccountID: accSeeded, Direction: Credit, Amount: 2_000},
		},
	})
	if !loHasProjectionRow(t, pool, f.tenantID, accSeeded) {
		t.Fatal("fixture precondition: the seeded account must have a projection row")
	}
	if loHasProjectionRow(t, pool, f.tenantID, accNew) {
		t.Fatal("fixture precondition: the account under test must start with NO projection row")
	}

	blockerSeeded := loHoldProjectionRow(t, pool, f.tenantID, accSeeded, "seeded-account")

	const amount = int64(1_000)
	// A: seeded account first, then the brand-new one.
	startA := func() *loRacer {
		return loStartRacer(t, pool, f.tenantID, "posting(seeded,new)", func(ctx context.Context, tx pgx.Tx) error {
			_, err := Post(ctx, tx, TransactionInput{
				TenantID: f.tenantID, TransactionType: TxManualAdjustment,
				IdempotencyKey: "lockorder-newacct-a", CorrelationID: uuid.New(),
				ReasonCode: loReason("lock_order_test"),
				Entries: []EntryInput{
					{LedgerAccountID: accSeeded, Direction: Debit, Amount: amount},
					{LedgerAccountID: accNew, Direction: Credit, Amount: amount},
				},
			})
			return err
		})
	}
	// B: the brand-new account first (which it MATERIALISES), then the
	// seeded one - the opposite order, and the half that pre-R6 created a
	// projection row at L4 with no canonical ordering at all.
	startB := func() *loRacer {
		return loStartRacer(t, pool, f.tenantID, "posting(new,seeded)", func(ctx context.Context, tx pgx.Tx) error {
			_, err := Post(ctx, tx, TransactionInput{
				TenantID: f.tenantID, TransactionType: TxManualAdjustment,
				IdempotencyKey: "lockorder-newacct-b", CorrelationID: uuid.New(),
				ReasonCode: loReason("lock_order_test"),
				Entries: []EntryInput{
					{LedgerAccountID: accNew, Direction: Debit, Amount: amount},
					{LedgerAccountID: accSeeded, Direction: Credit, Amount: amount},
				},
			})
			return err
		})
	}

	racerA, racerB, errA, errB := loRunABBA(t, pool, startA, startB, []*loBlocker{blockerSeeded})
	loAssertNoDeadlock(t, "LOCK-1/new projection rows", map[string]error{racerA.name: errA, racerB.name: errB})
	if errA != nil {
		t.Fatalf("racer A failed: %v", errA)
	}
	if errB != nil {
		t.Fatalf("racer B failed: %v", errB)
	}

	// Both postings are equal-and-opposite, so each account nets to zero
	// but both must show real activity - a silently-skipped posting would
	// also net to zero and must not be mistaken for success.
	var newBal Balance
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		newBal, err = GetProjectedBalance(ctx, tx, accNew)
		return err
	}); err != nil {
		t.Fatalf("read projection for the new account: %v", err)
	}
	if !newBal.Found {
		t.Fatal("the brand-new account still has no projection row after two postings")
	}
	if newBal.DebitTotal != amount || newBal.CreditTotal != amount {
		t.Fatalf("brand-new account: expected debit=%d credit=%d, got debit=%d credit=%d",
			amount, amount, newBal.DebitTotal, newBal.CreditTotal)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// TestLockOrder_ConcurrentFirstAccountCreationOppositeArgumentOrder is
// the mechanism-3 (ledger_accounts unique-index insertion wait) half of
// ADR 0082 §3.2, which GetOrCreateAccounts exists to close: two call
// sites resolving the SAME two not-yet-existing (wallet, type, asset)
// tuples in OPPOSITE argument orders must not be able to block on each
// other's uncommitted index entries in opposite orders.
//
// Kept as its own test rather than folded above because it cannot be
// demonstrated failing on HEAD: GetOrCreateAccounts does not exist there,
// and the HEAD-equivalent (two bare GetOrCreateAccount calls in caller
// order) is precisely what this replaces. The property it pins is
// nonetheless the one withdrawal's §1.7 finding depends on.
func TestLockOrder_ConcurrentFirstAccountCreationOppositeArgumentOrder(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	walletID := loNewWallet(t, pool, f, "EUR")

	cashSpec := AccountSpec{WalletID: &walletID, AccountType: AccountPlayerCash, AssetCode: "EUR"}
	holdSpec := AccountSpec{WalletID: &walletID, AccountType: AccountPlayerWithdrawalHold, AssetCode: "EUR"}

	const n = 8
	var wg sync.WaitGroup
	results := make([][]uuid.UUID, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Half resolve (cash, hold); half resolve (hold, cash) - the
			// exact asymmetry between RequestWithdrawal and
			// Reject/Complete/Fail/Cancel (§1.7).
			specs := []AccountSpec{cashSpec, holdSpec}
			if i%2 == 1 {
				specs = []AccountSpec{holdSpec, cashSpec}
			}
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				ids, err := GetOrCreateAccounts(ctx, tx, f.tenantID, specs...)
				results[i] = ids
				return err
			})
		}(i)
	}
	wg.Wait()

	var cash, hold uuid.UUID
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			if loIsDeadlock(errs[i]) {
				t.Fatalf("goroutine %d deadlocked resolving accounts: %s", i, loDescribeDeadlock(errs[i]))
			}
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		gotCash, gotHold := results[i][0], results[i][1]
		if i%2 == 1 {
			gotCash, gotHold = results[i][1], results[i][0]
		}
		if cash == uuid.Nil {
			cash, hold = gotCash, gotHold
			continue
		}
		// GetOrCreateAccounts must return ids IN THE CALLER'S ARGUMENT
		// ORDER even though it CREATES in canonical order - the single
		// most dangerous way to get this wrong is to silently return them
		// sorted, which would swap a debit and a credit at the call site.
		if gotCash != cash || gotHold != hold {
			t.Fatalf("goroutine %d resolved different account ids than goroutine 0 (cash %s vs %s, hold %s vs %s)",
				i, gotCash, cash, gotHold, hold)
		}
	}

	var rows int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_accounts WHERE wallet_id = $1`, walletID).Scan(&rows)
	}); err != nil {
		t.Fatalf("count ledger accounts: %v", err)
	}
	if rows != 2 {
		t.Fatalf("expected exactly 2 ledger_accounts rows for the wallet, got %d", rows)
	}
}

// --- §6 test 6 -------------------------------------------------------------

// loBonusCost is the operator-funded attribution every BONUS_SET posting
// below carries (there is no default - ledger-accounting-model.md §7.4.3).
func loBonusCost() *BonusCostAttribution {
	return &BonusCostAttribution{Funding: FundingOperator}
}

// TestLockOrder_ConcurrentBonusMirrorPostings_NoDeadlock is ADR 0082 §6
// test 6: two bonus-touching postings, both of which have Rule B2
// (extended) mirror/recognition legs GENERATED for them by ledger.Post
// itself (promo_liability + bonus_expense), racing on one tenant.
//
// This is the case internal/casino's loadEntries' `ORDER BY
// e.ledger_account_id` provably could NOT cover, and the reason that
// clause's old comment (which claimed to be the lock-ordering mechanism)
// is corrected by this dispatch: Post APPENDS the generated legs after
// the caller's entries (§7.4.2's fixed order), so "caller entries sorted
// ascending, then generated legs" is not ascending at all, and no caller
// can fix that because HR-17 forbids it from even constructing those
// legs.
//
// Sub-test "shared caller accounts in opposite order" is the genuine ABBA
// and is the one shown failing on HEAD with 40P01: a terminal-grant
// hold-capture shape (Dr house_gaming / Cr player_bonus_held) against a
// held-win rollback shape (Dr player_bonus_held / Cr house_gaming), which
// are exactly internal/casino's postWinLockedBonus-terminal and
// postRollbackHeldWin entry orders (§1.3).
//
// Sub-test "disjoint caller accounts, generated legs collide" records a
// finding this dispatch is obliged to state rather than paper over: that
// arrangement CANNOT be made to deadlock on HEAD, because the generator
// emits its legs in one globally fixed order (all step-2 promo_liability
// legs, then all step-3 recognition legs, assets sorted) identically for
// every posting, so two postings sharing only generated legs share them
// in the SAME relative order and no cycle exists. It is asserted here for
// what it does prove - that the pre-lock covers generated legs and both
// postings mirror correctly under concurrency - not as a deadlock
// reproduction it is not.
func TestLockOrder_ConcurrentBonusMirrorPostings_NoDeadlock(t *testing.T) {
	t.Run("shared caller accounts in opposite order", func(t *testing.T) {
		pool := testPool(t)
		f := seedFixture(t, pool)

		houseID := loAccount(t, pool, f, nil, AccountHouseGaming, "EUR")
		heldID := loAccount(t, pool, f, &f.walletID, AccountPlayerBonusHeld, "EUR")

		// Seed both projection rows (and the generated promo_liability /
		// bonus_expense ones) so this test isolates the ENTRY-ORDER cycle
		// rather than re-testing the absent-row case test 5 covers.
		mustPost(t, pool, f, TransactionInput{
			TenantID: f.tenantID, TransactionType: TxCasinoWin,
			IdempotencyKey: "lockorder-bonus-seed", CorrelationID: uuid.New(),
			Entries: []EntryInput{
				{LedgerAccountID: houseID, Direction: Debit, Amount: 5_000},
				{LedgerAccountID: heldID, Direction: Credit, Amount: 5_000},
			},
			BonusCost: loBonusCost(),
		})

		blockerHouse := loHoldProjectionRow(t, pool, f.tenantID, houseID, "house_gaming")
		blockerHeld := loHoldProjectionRow(t, pool, f.tenantID, heldID, "player_bonus_held")

		const amount = int64(400)
		// Hold-capture shape: house_gaming first, then player_bonus_held.
		startA := func() *loRacer {
			return loStartRacer(t, pool, f.tenantID, "hold-capture(house,held)", func(ctx context.Context, tx pgx.Tx) error {
				_, err := Post(ctx, tx, TransactionInput{
					TenantID: f.tenantID, TransactionType: TxCasinoWin,
					IdempotencyKey: "lockorder-bonus-capture", CorrelationID: uuid.New(),
					Entries: []EntryInput{
						{LedgerAccountID: houseID, Direction: Debit, Amount: amount},
						{LedgerAccountID: heldID, Direction: Credit, Amount: amount},
					},
					BonusCost: loBonusCost(),
				})
				return err
			})
		}
		// Held-win rollback shape: player_bonus_held first, then house_gaming.
		startB := func() *loRacer {
			return loStartRacer(t, pool, f.tenantID, "held-rollback(held,house)", func(ctx context.Context, tx pgx.Tx) error {
				_, err := Post(ctx, tx, TransactionInput{
					TenantID: f.tenantID, TransactionType: TxCasinoRollback,
					IdempotencyKey: "lockorder-bonus-rollback", CorrelationID: uuid.New(),
					Entries: []EntryInput{
						{LedgerAccountID: heldID, Direction: Debit, Amount: amount},
						{LedgerAccountID: houseID, Direction: Credit, Amount: amount},
					},
					BonusCost: loBonusCost(),
				})
				return err
			})
		}

		racerA, racerB, errA, errB := loRunABBA(t, pool, startA, startB, []*loBlocker{blockerHouse, blockerHeld})
		loAssertNoDeadlock(t, "bonus mirror postings", map[string]error{racerA.name: errA, racerB.name: errB})
		if errA != nil || errB != nil {
			t.Fatalf("both bonus postings must succeed: A=%v B=%v", errA, errB)
		}
		loAssertBalanced(t, pool, f.tenantID)
		loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
	})

	t.Run("disjoint caller accounts, generated legs collide", func(t *testing.T) {
		pool := testPool(t)
		f := seedFixture(t, pool)
		walletTwo := loNewWallet(t, pool, f, "EUR")

		bonusOne := loAccount(t, pool, f, &f.walletID, AccountPlayerBonus, "EUR")
		bonusTwo := loAccount(t, pool, f, &walletTwo, AccountPlayerBonus, "EUR")
		promoID := loAccount(t, pool, f, nil, AccountPromoLiability, "EUR")

		// No blocker: as the doc comment above explains, this arrangement
		// has no cycle to force. Two genuinely simultaneous postings whose
		// ONLY shared accounts are the generated legs is the strongest
		// thing there is to run here, and what it proves is that the
		// generated legs are covered by the pre-lock and applied exactly
		// once each.
		const amount = int64(250)
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, acct := range []uuid.UUID{bonusOne, bonusTwo} {
			wg.Add(1)
			go func(i int, acct uuid.UUID) {
				defer wg.Done()
				errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
					_, err := Post(ctx, tx, TransactionInput{
						TenantID: f.tenantID, TransactionType: TxBonusGrant,
						IdempotencyKey: fmt.Sprintf("lockorder-bonus-grant-%d", i), CorrelationID: uuid.New(),
						Entries:   []EntryInput{{LedgerAccountID: acct, Direction: Credit, Amount: amount}},
						BonusCost: loBonusCost(),
					})
					return err
				})
			}(i, acct)
		}
		wg.Wait()

		for i, err := range errs {
			if loIsDeadlock(err) {
				t.Fatalf("grant posting %d deadlocked on its generated legs: %s", i, loDescribeDeadlock(err))
			}
			if err != nil {
				t.Fatalf("grant posting %d: %v", i, err)
			}
		}

		// Both grants generated a promo_liability mirror leg against the
		// SAME house account; it must carry the sum of both, exactly once
		// each.
		var promo Balance
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			promo, err = GetProjectedBalance(ctx, tx, promoID)
			return err
		}); err != nil {
			t.Fatalf("read promo_liability projection: %v", err)
		}
		if promo.DebitTotal != 2*amount || promo.CreditTotal != 0 {
			t.Fatalf("promo_liability should carry both grants' mirror legs exactly once (debit=%d credit=0), got debit=%d credit=%d",
				2*amount, promo.DebitTotal, promo.CreditTotal)
		}
		loAssertBalanced(t, pool, f.tenantID)
		loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
	})
}

// --- §6 test 7 -------------------------------------------------------------

// TestLockOrder_PreLockCoversEveryEntryIncludingGeneratedLegs is the
// ordering property itself, not the absence of a symptom (ADR 0082 §6 B):
// for a bonus posting, LockedProjections.AccountIDs must be EXACTLY the
// distinct account set of Post's final entry list - caller entries plus
// the Rule B2 mirror and recognition legs Post generated - and strictly
// ascending.
//
// This is the assertion that makes R3 ("no partial pre-locking, ever")
// checkable: a caller cannot enumerate the generated legs (HR-17 forbids
// it from even constructing them), so if the pre-lock covered only the
// caller's own entries it would be a SUBSET, and a subset is safe only if
// it is a prefix of the ascending order, which no caller can know.
func TestLockOrder_PreLockCoversEveryEntryIncludingGeneratedLegs(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	houseID := loAccount(t, pool, f, nil, AccountHouseGaming, "EUR")
	bonusID := loAccount(t, pool, f, &f.walletID, AccountPlayerBonus, "EUR")
	lockedBonusID := loAccount(t, pool, f, &f.walletID, AccountPlayerLockedBonus, "EUR")

	in := TransactionInput{
		TenantID: f.tenantID, TransactionType: TxCasinoWin,
		IdempotencyKey: "lockorder-prelock-coverage", CorrelationID: uuid.New(),
		Entries: []EntryInput{
			{LedgerAccountID: houseID, Direction: Debit, Amount: 900},
			{LedgerAccountID: bonusID, Direction: Credit, Amount: 900},
			{LedgerAccountID: lockedBonusID, Direction: Debit, Amount: 300},
			{LedgerAccountID: bonusID, Direction: Credit, Amount: 300},
		},
		BonusCost: loBonusCost(),
	}

	var locked LockedProjections
	var txID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		locked, err = LockProjectionsForPosting(ctx, tx, in)
		if err != nil {
			return err
		}
		res, err := Post(ctx, tx, in)
		if err != nil {
			return err
		}
		txID = res.TransactionID
		return nil
	})
	if err != nil {
		t.Fatalf("pre-lock then post: %v", err)
	}

	// Strictly ascending in UUID byte order - identical to Postgres's own
	// uuid ordering, which is what makes the Go-side order and any
	// ORDER BY ledger_account_id agree exactly.
	for i := 1; i < len(locked.AccountIDs); i++ {
		prev, cur := locked.AccountIDs[i-1], locked.AccountIDs[i]
		if bytes.Compare(prev[:], cur[:]) >= 0 {
			t.Fatalf("LockedProjections.AccountIDs is not strictly ascending at index %d: %s then %s", i, prev, cur)
		}
	}

	var posted []uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT DISTINCT ledger_account_id FROM ledger_entries WHERE ledger_transaction_id = $1 ORDER BY 1`, txID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			posted = append(posted, id)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read posted entry accounts: %v", err)
	}

	if len(posted) != len(locked.AccountIDs) {
		t.Fatalf("pre-lock covered %d accounts but the posting touched %d (locked=%v posted=%v)",
			len(locked.AccountIDs), len(posted), locked.AccountIDs, posted)
	}
	for i := range posted {
		if posted[i] != locked.AccountIDs[i] {
			t.Fatalf("pre-locked set differs from the posted set at index %d: locked %s, posted %s",
				i, locked.AccountIDs[i], posted[i])
		}
	}

	// The whole point: the set is strictly LARGER than the caller's own
	// entries, because Post generated promo_liability and bonus_expense
	// legs the caller never named. A pre-lock that merely covered the
	// caller's entries would pass every other assertion here.
	callerAccounts := map[uuid.UUID]bool{houseID: true, bonusID: true, lockedBonusID: true}
	generated := 0
	for _, id := range locked.AccountIDs {
		if !callerAccounts[id] {
			generated++
		}
		if _, err := locked.Balance(id); err != nil {
			t.Fatalf("Balance(%s) must succeed for every locked account: %v", id, err)
		}
	}
	if generated == 0 {
		t.Fatal("expected the pre-lock to cover at least one GENERATED mirror/recognition leg the caller never named")
	}

	// Fail-closed: an account outside the locked set must error, never
	// return a zero Balance a caller could compare against a stake.
	if _, err := locked.Balance(f.clearingID); err == nil {
		t.Fatal("Balance for an account outside the locked set must return ErrAccountNotLocked, not a zero Balance")
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// --- §6 test 8 -------------------------------------------------------------

// TestLockOrder_ProjectionRowIsMaterialisedAndLockedWhenAbsent is ADR
// 0082 R6, and the direct evidence for the judgment call this dispatch
// had to make (§2.3): an account with no prior entries gets a zero-totals
// projection row created AND LOCKED by the pre-lock step, and a
// concurrent reader genuinely blocks on it.
//
// The financial assertions are the reason R6 was accepted rather than
// rejected in favour of re-ordering ledger_entries insertion: the row
// carries zeros and nothing else, its identity columns come from
// ledger_accounts (the same source migration 0023's trigger uses), and it
// reconciles as 0 == 0.
func TestLockOrder_ProjectionRowIsMaterialisedAndLockedWhenAbsent(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	newAcct := loAccount(t, pool, f, &f.walletID, AccountPlayerLockedBonus, "EUR")
	if loHasProjectionRow(t, pool, f.tenantID, newAcct) {
		t.Fatal("fixture precondition: the account under test must start with NO projection row")
	}

	in := TransactionInput{
		TenantID: f.tenantID, TransactionType: TxBonusGrant,
		IdempotencyKey: "lockorder-materialise", CorrelationID: uuid.New(),
		Entries:   []EntryInput{{LedgerAccountID: newAcct, Direction: Credit, Amount: 700}},
		BonusCost: loBonusCost(),
	}

	// Hold the pre-lock open in one transaction so a second connection can
	// prove the row is genuinely locked, not merely created. loHoldWith
	// registers its own t.Cleanup release, so a failed assertion below can
	// never strand the holding transaction (which would hang pool.Close).
	var obs struct {
		balance Balance
		rowSeen bool
	}
	holder := loHoldWith(t, pool, f.tenantID, "pre-lock holder", func(ctx context.Context, tx pgx.Tx) error {
		locked, err := LockProjectionsForPosting(ctx, tx, in)
		if err != nil {
			return err
		}
		b, err := locked.Balance(newAcct)
		if err != nil {
			return err
		}
		var seen bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM wallet_balance_projection
			  WHERE ledger_account_id = $1 AND debit_total = 0 AND credit_total = 0)`, newAcct).Scan(&seen); err != nil {
			return err
		}
		obs.balance, obs.rowSeen = b, seen
		return nil
	})

	if obs.balance.Found {
		t.Fatal("Balance.Found must be false for a row this call materialised - a caller must be able to tell " +
			"'created with zero totals' from 'a row with real history'")
	}
	if obs.balance.DebitTotal != 0 || obs.balance.CreditTotal != 0 {
		t.Fatalf("a materialised row must carry zero totals, got debit=%d credit=%d",
			obs.balance.DebitTotal, obs.balance.CreditTotal)
	}
	if obs.balance.AssetCode != "EUR" || obs.balance.AccountType != AccountPlayerLockedBonus {
		t.Fatalf("a materialised row must copy its identity columns from ledger_accounts, got asset=%q type=%q",
			obs.balance.AssetCode, obs.balance.AccountType)
	}
	if !obs.rowSeen {
		t.Fatal("the pre-lock did not materialise the projection row")
	}

	// A second transaction must genuinely be excluded from the row.
	//
	// The exclusion mechanism for a BRAND-NEW row is the unique-index
	// insertion wait, NOT the FOR UPDATE - and that distinction is the
	// whole reason R6 exists. Under READ COMMITTED an uncommitted INSERT
	// is invisible to another transaction, so a concurrent
	// `SELECT ... FOR UPDATE` on the new row finds NO ROW and locks
	// nothing; it is precisely that "locks nothing" that made the
	// pre-R6 code create the row out of canonical order at class L4.
	// What a concurrent transaction genuinely blocks on is its own
	// attempt to materialise the same row (which is what both the ensure
	// step and migration 0023's trigger do), so that is what is probed
	// here - anything else would assert a property Postgres does not
	// have and would pass for the wrong reason.
	probe := loStartRacer(t, pool, f.tenantID, "concurrent materialiser", func(ctx context.Context, tx pgx.Tx) error {
		_, err := ensureAndLockProjectionsInOrder(ctx, tx, f.tenantID, []uuid.UUID{newAcct})
		return err
	})
	if _, ok := loWaitBlocked(t, pool, probe.pid, probe.done); !ok {
		holder.release()
		_ = probe.wait()
		t.Fatal("a concurrent transaction materialising the SAME projection row did not block - the pre-lock step " +
			"is not mutually exclusive, so two first-ever postings could still create the row out of canonical order")
	}
	if got := loBlockingPIDs(t, pool, probe.pid); !loContains(got, holder.pid) {
		holder.release()
		_ = probe.wait()
		t.Fatalf("the concurrent materialiser is blocked by %v, not by the pre-lock holder (%d)", got, holder.pid)
	}

	// An UNCOMMITTED new row is invisible to another transaction, so a
	// concurrent FOR UPDATE finds nothing at all. Asserted explicitly
	// because a future reader will otherwise expect 55P03 here, and
	// because it is the exact fact that makes the ensure-then-lock
	// interleaving (rather than a batched insert pass followed by a
	// batched lock pass) load-bearing.
	var seen bool
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM wallet_balance_projection WHERE ledger_account_id = $1)`, newAcct).Scan(&seen)
	}); err != nil {
		t.Fatalf("probe visibility of the uncommitted row: %v", err)
	}
	if seen {
		t.Fatal("the uncommitted materialised row was visible to another transaction; this test's premise is wrong")
	}

	holder.release()
	if err := probe.wait(); err != nil {
		t.Fatalf("the concurrent materialiser must succeed once the holder commits: %v", err)
	}

	// The holder committed without posting anything: R6's documented,
	// accepted residue is a zero-valued row for an account that was about
	// to be used. It must reconcile as 0 == 0, not as drift.
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
	loAssertBalanced(t, pool, f.tenantID)
}

// --- §6 test 11 ------------------------------------------------------------

// TestLedger_BalancedUnderConcurrentLoad is ADR 0082 §6 test 11 - the
// "nothing else moved" proof. No pre-existing test in this repository
// covered it (searched: internal/ledger, internal/casino,
// internal/reconciliation - the closest were single-shape concurrency
// tests), so it is created here rather than claimed to already exist.
//
// N concurrent mixed postings - deposit, bet, win, withdrawal-hold and
// bonus-grant shapes, the five distinct entry shapes production writes -
// against ONE tenant, all racing for the same accounts with no blockers
// and no forced interleaving. Afterwards:
//
//   - SUM(debits) == SUM(credits) per asset;
//   - every wallet_balance_projection row equals ledger.RebuildBalance,
//     which is exactly the comparison
//     reconciliation.RunLedgerVsProjection makes (asserted here rather
//     than by calling that package, which cannot be imported from
//     internal/ledger without an import cycle);
//   - exactly N transactions posted, no duplicates, no losses.
func TestLedger_BalancedUnderConcurrentLoad(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	walletTwo := loNewWallet(t, pool, f, "EUR")

	house := loAccount(t, pool, f, nil, AccountHouseGaming, "EUR")
	hold := loAccount(t, pool, f, &f.walletID, AccountPlayerWithdrawalHold, "EUR")
	bonus := loAccount(t, pool, f, &f.walletID, AccountPlayerBonus, "EUR")
	cashTwo := loAccount(t, pool, f, &walletTwo, AccountPlayerCash, "EUR")

	const n = 40
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			in := TransactionInput{
				TenantID:       f.tenantID,
				IdempotencyKey: fmt.Sprintf("lockorder-load-%d", i),
				CorrelationID:  uuid.New(),
			}
			switch i % 5 {
			case 0: // deposit: Dr psp_clearing / Cr player_cash
				in.TransactionType = TxDeposit
				in.Entries = []EntryInput{
					{LedgerAccountID: f.clearingID, Direction: Debit, Amount: 1_000},
					{LedgerAccountID: f.cashAccountID, Direction: Credit, Amount: 1_000},
				}
			case 1: // bet: Dr player_cash / Cr house_gaming
				in.TransactionType = TxCasinoBet
				in.Entries = []EntryInput{
					{LedgerAccountID: f.cashAccountID, Direction: Debit, Amount: 100},
					{LedgerAccountID: house, Direction: Credit, Amount: 100},
				}
			case 2: // win: Dr house_gaming / Cr player_cash (opposite order)
				in.TransactionType = TxCasinoWin
				in.Entries = []EntryInput{
					{LedgerAccountID: house, Direction: Debit, Amount: 50},
					{LedgerAccountID: f.cashAccountID, Direction: Credit, Amount: 50},
				}
			case 3: // withdrawal hold: Dr player_cash / Cr player_withdrawal_hold
				in.TransactionType = TxWithdrawalRequested
				in.Entries = []EntryInput{
					{LedgerAccountID: f.cashAccountID, Direction: Debit, Amount: 20},
					{LedgerAccountID: hold, Direction: Credit, Amount: 20},
				}
			case 4: // bonus grant on wallet one, plus a cash leg on wallet two
				in.TransactionType = TxBonusGrant
				in.BonusCost = loBonusCost()
				in.Entries = []EntryInput{
					{LedgerAccountID: bonus, Direction: Credit, Amount: 30},
					{LedgerAccountID: cashTwo, Direction: Debit, Amount: 30},
				}
			}
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := Post(ctx, tx, in)
				return err
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if loIsDeadlock(err) {
			t.Fatalf("mixed-load posting %d deadlocked: %s", i, loDescribeDeadlock(err))
		}
		if err != nil {
			t.Fatalf("mixed-load posting %d: %v", i, err)
		}
	}

	var posted int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND idempotency_key LIKE 'lockorder-load-%'`,
			f.tenantID).Scan(&posted)
	}); err != nil {
		t.Fatalf("count postings: %v", err)
	}
	if posted != n {
		t.Fatalf("expected exactly %d transactions, got %d", n, posted)
	}

	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}
