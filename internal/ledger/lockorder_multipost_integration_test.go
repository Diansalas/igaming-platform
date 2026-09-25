//go:build integration

// Ledger-finance sign-off P3-1.5 (docs/governance/stage-10-w1-ledger-
// finance-signoff.md): LockProjectionsForPostings (ADR 0088 §5.2, ADR 0082
// Amendment A4) had no test pinning its OWN contract directly - coverage
// was only indirect, through internal/sportsbook's void-after-settlement
// tests. This file pins: a tenant-mismatch rejection, an empty-input
// rejection, and "the locked set equals the union of accounts Post will
// lock, including a generated bonus-mirror leg, in strictly ascending
// canonical order".
package ledger

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestLockProjectionsForPostings_MixedTenantsRejected: inputs spanning more
// than one tenant must be rejected with ErrPreLockTenantMismatch, before
// any lock is taken.
func TestLockProjectionsForPostings_MixedTenantsRejected(t *testing.T) {
	pool := testPool(t)
	f1 := seedFixture(t, pool)
	f2 := seedFixture(t, pool)

	house1 := loAccount(t, pool, f1, nil, AccountHouseGaming, "EUR")
	house2 := loAccount(t, pool, f2, nil, AccountHouseGaming, "EUR")

	in1 := TransactionInput{
		TenantID: f1.tenantID, TransactionType: TxManualAdjustment,
		IdempotencyKey: "lop-mixed-1-" + uuid.NewString(), CorrelationID: uuid.New(), ReasonCode: loReason("lock_order_test"),
		Entries: []EntryInput{
			{LedgerAccountID: f1.cashAccountID, Direction: Debit, Amount: 10},
			{LedgerAccountID: house1, Direction: Credit, Amount: 10},
		},
	}
	// A second input from a DIFFERENT tenant - the exact shape §5.2 forbids
	// (LockProjectionsForPostings' contract is a single tenant per call).
	in2 := TransactionInput{
		TenantID: f2.tenantID, TransactionType: TxManualAdjustment,
		IdempotencyKey: "lop-mixed-2-" + uuid.NewString(), CorrelationID: uuid.New(), ReasonCode: loReason("lock_order_test"),
		Entries: []EntryInput{
			{LedgerAccountID: f2.cashAccountID, Direction: Debit, Amount: 10},
			{LedgerAccountID: house2, Direction: Credit, Amount: 10},
		},
	}

	err := pool.WithTenant(context.Background(), f1.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := LockProjectionsForPostings(ctx, tx, in1, in2)
		return err
	})
	if !errors.Is(err, ErrPreLockTenantMismatch) {
		t.Fatalf("expected ErrPreLockTenantMismatch, got %v", err)
	}
}

// TestLockProjectionsForPostings_EmptyInputRejected: calling with zero
// postings is a caller bug, not a silent no-op.
func TestLockProjectionsForPostings_EmptyInputRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := LockProjectionsForPostings(ctx, tx)
		return err
	})
	if err == nil {
		t.Fatalf("expected an error for zero postings, got nil")
	}
	if !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("expected ErrInvalidEntry, got %v", err)
	}
}

// TestLockProjectionsForPostings_LockedSetEqualsUnionOfBothPostings is the
// ordering property itself (mirroring
// TestLockOrder_PreLockCoversEveryEntryIncludingGeneratedLegs's single-
// posting version, ADR 0082 §6 B), generalised to TWO postings that will be
// Post'ed in the SAME transaction: LockedProjections.AccountIDs must equal
// EXACTLY the distinct union of both postings' final entry sets - caller
// entries plus a generated bonus-mirror leg the caller never named - and
// be strictly ascending, so subsequent Posts (in order) re-lock rows
// already held rather than acquiring anything new.
func TestLockProjectionsForPostings_LockedSetEqualsUnionOfBothPostings(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	houseID := loAccount(t, pool, f, nil, AccountHouseGaming, "EUR")
	bonusID := loAccount(t, pool, f, &f.walletID, AccountPlayerBonus, "EUR")

	// A bonus grant (generates a promo_liability mirror leg the caller
	// never names) and a disjoint plain bet-shape posting - the exact
	// void-after-settlement shape §5.2 exists for: two postings whose
	// CALLER account sets differ, pre-locked once as a union.
	grant := TransactionInput{
		TenantID: f.tenantID, TransactionType: TxBonusGrant,
		IdempotencyKey: "lop-union-grant-" + uuid.NewString(), CorrelationID: uuid.New(),
		Entries:   []EntryInput{{LedgerAccountID: bonusID, Direction: Credit, Amount: 500}},
		BonusCost: loBonusCost(),
	}
	bet := TransactionInput{
		TenantID: f.tenantID, TransactionType: TxCasinoBet,
		IdempotencyKey: "lop-union-bet-" + uuid.NewString(), CorrelationID: uuid.New(),
		Entries: []EntryInput{
			{LedgerAccountID: f.cashAccountID, Direction: Debit, Amount: 200},
			{LedgerAccountID: houseID, Direction: Credit, Amount: 200},
		},
	}

	var locked LockedProjections
	var grantTxID, betTxID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		locked, err = LockProjectionsForPostings(ctx, tx, grant, bet)
		if err != nil {
			return err
		}
		grantRes, err := Post(ctx, tx, grant)
		if err != nil {
			return err
		}
		grantTxID = grantRes.TransactionID
		// Re-locking rows this transaction already holds must be a no-op
		// (R3/R4): this Post call takes no NEW lock beyond what the
		// pre-lock above already acquired.
		betRes, err := Post(ctx, tx, bet)
		if err != nil {
			return err
		}
		betTxID = betRes.TransactionID
		return nil
	})
	if err != nil {
		t.Fatalf("pre-lock then post both postings: %v", err)
	}

	for i := 1; i < len(locked.AccountIDs); i++ {
		prev, cur := locked.AccountIDs[i-1], locked.AccountIDs[i]
		if bytes.Compare(prev[:], cur[:]) >= 0 {
			t.Fatalf("LockedProjections.AccountIDs is not strictly ascending at index %d: %s then %s", i, prev, cur)
		}
	}

	var posted []uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT DISTINCT ledger_account_id FROM ledger_entries WHERE ledger_transaction_id IN ($1, $2) ORDER BY 1`,
			grantTxID, betTxID)
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
		t.Fatalf("pre-lock covered %d accounts but the two postings together touched %d (locked=%v posted=%v)",
			len(locked.AccountIDs), len(posted), locked.AccountIDs, posted)
	}
	for i := range posted {
		if posted[i] != locked.AccountIDs[i] {
			t.Fatalf("pre-locked set differs from the posted set at index %d: locked %s, posted %s",
				i, locked.AccountIDs[i], posted[i])
		}
	}

	// The whole point (mirroring the single-posting test): the union is
	// STRICTLY LARGER than the two postings' own caller-named accounts,
	// because the bonus grant generated a promo_liability mirror leg
	// neither caller ever named.
	callerAccounts := map[uuid.UUID]bool{bonusID: true, f.cashAccountID: true, houseID: true}
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
		t.Fatal("expected the union to include at least one GENERATED bonus-mirror leg neither posting's caller entries named")
	}

	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}
