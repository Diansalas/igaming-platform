//go:build integration

// Q2 mutation-pass survivors (docs/governance/stage-10-w1-mutation-and-sql-
// branch-coverage.md): gremlins found LIVED mutants in GetOrCreateAccounts'
// own canonical creation order (ADR 0082 §3.2) that
// TestLockOrder_ConcurrentFirstAccountCreationOppositeArgumentOrder does not
// kill, because that test only pins the RETURNED order (argument order) and
// the absence of a deadlock, never the actual creation ORDER. This file
// pins the creation order directly.
package ledger

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestGetOrCreateAccounts_CreatesInCanonicalOrder_HouseBeforeWallet: a
// house-level spec (WalletID nil, canonicalKey's wallet component is "")
// always sorts before ANY wallet-scoped spec (a non-empty UUID string),
// per canonicalKey's own doc comment. Passing [wallet, house] (the REVERSE
// of canonical order) must still CREATE the house account first - killing
// both the "s.WalletID != nil" negation (line 417) and the sort
// comparator's boundary/negation mutants (line 454), which
// TestLockOrder_ConcurrentFirstAccountCreationOppositeArgumentOrder does
// not distinguish (it only asserts the RETURNED slice order, which
// GetOrCreateAccounts always keeps in argument order regardless of
// creation order).
func TestGetOrCreateAccounts_CreatesInCanonicalOrder_HouseBeforeWallet(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	walletID := loNewWallet(t, pool, f, "EUR")

	walletSpec := AccountSpec{WalletID: &walletID, AccountType: AccountPlayerCash, AssetCode: "EUR"}
	houseSpec := AccountSpec{WalletID: nil, AccountType: AccountHouseGaming, AssetCode: "EUR"}

	var walletAccountID, houseAccountID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Reverse of canonical order: wallet-scoped first, house-level
		// second. GetOrCreateAccounts must still CREATE house-level first.
		resolved, err := GetOrCreateAccounts(ctx, tx, f.tenantID, walletSpec, houseSpec)
		if err != nil {
			return err
		}
		walletAccountID, houseAccountID = resolved[0], resolved[1]
		return nil
	})
	if err != nil {
		t.Fatalf("resolve accounts: %v", err)
	}

	// ctid reflects physical insertion order in a freshly-created heap with
	// no concurrent writers - both rows were just inserted, in this same
	// transaction, by GetOrCreateAccounts' own canonical-order loop.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id FROM ledger_accounts WHERE id = ANY($1) ORDER BY ctid`,
			[]uuid.UUID{walletAccountID, houseAccountID})
		if err != nil {
			return err
		}
		defer rows.Close()
		var order []uuid.UUID
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			order = append(order, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(order) != 2 {
			t.Fatalf("expected 2 ledger_accounts rows, got %d", len(order))
		}
		if order[0] != houseAccountID {
			t.Fatalf("expected the house-level account to be CREATED first (canonical order: house key \"\" < any wallet key), "+
				"got creation order %v (house=%v, wallet=%v)", order, houseAccountID, walletAccountID)
		}
		if order[1] != walletAccountID {
			t.Fatalf("expected the wallet-scoped account to be created second, got creation order %v", order)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read creation order: %v", err)
	}
}

// TestPost_RejectsZeroAmountEntry: prepareEntries' "entry amount must be
// positive" check (shared by Post and LockProjectionsForPosting) rejects
// Amount == 0, not just negative amounts (CONDITIONALS_BOUNDARY: "<= 0" vs
// "< 0" - no existing test in this package posted a zero-amount entry).
func TestPost_RejectsZeroAmountEntry(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Post(ctx, tx, TransactionInput{
			TenantID: f.tenantID, TransactionType: TxManualAdjustment,
			IdempotencyKey: "zero-amount-" + uuid.NewString(), CorrelationID: uuid.New(),
			ReasonCode: strPtr("qa-zero-amount-test"),
			Entries: []EntryInput{
				{LedgerAccountID: f.cashAccountID, Direction: Debit, Amount: 0},
				{LedgerAccountID: f.clearingID, Direction: Credit, Amount: 0},
			},
		})
		return err
	})
	if err == nil {
		t.Fatalf("expected Post to reject a zero-amount entry")
	}
	if !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("expected ErrInvalidEntry for a zero-amount entry, got %v", err)
	}
}

// TestPost_RejectsTombstoneWithEntries: a tombstone moves no money. Post must
// refuse a tombstone that carries real, balanced entries with ErrInvalidEntry
// and write NO ledger_transactions, ledger_entries or projection change (the
// gameplay tenant-status gate's tombstone exemption relies on this).
func TestPost_RejectsTombstoneWithEntries(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	counts := func(ctx context.Context, tx pgx.Tx) (txs, entries int, cash int64) {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, f.tenantID).Scan(&txs); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE tenant_id = $1`, f.tenantID).Scan(&entries); err != nil {
			t.Fatal(err)
		}
		b, err := GetProjectedBalance(ctx, tx, f.cashAccountID)
		if err != nil {
			t.Fatal(err)
		}
		return txs, entries, b.Signed()
	}
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tx0, e0, c0 := counts(ctx, tx)
		_, perr := Post(ctx, tx, TransactionInput{
			TenantID: f.tenantID, TransactionType: TxTombstone,
			IdempotencyKey: "tombstone-entries-" + uuid.NewString(), CorrelationID: uuid.New(),
			Entries: []EntryInput{
				{LedgerAccountID: f.clearingID, Direction: Debit, Amount: 5},
				{LedgerAccountID: f.cashAccountID, Direction: Credit, Amount: 5},
			},
		})
		if !errors.Is(perr, ErrInvalidEntry) {
			t.Errorf("expected ErrInvalidEntry for a tombstone with entries, got %v", perr)
		}
		tx1, e1, c1 := counts(ctx, tx)
		if tx1 != tx0 || e1 != e0 || c1 != c0 {
			t.Errorf("a refused tombstone must write nothing: tx %d->%d entries %d->%d cash %d->%d", tx0, tx1, e0, e1, c0, c1)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
