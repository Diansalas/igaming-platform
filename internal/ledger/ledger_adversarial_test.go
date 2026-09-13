//go:build integration

// Adversarial financial tests for the posting engine - the mandatory
// list Stage 3B may not be marked complete without (CLAUDE.md
// "Financial functionality is not done without tests for: normal
// transactions, duplicates, concurrency, retries, partial failure,
// rollback, settlement, reconciliation, provider callbacks, idempotency,
// authorization, auditability").
//
// These live in their own file rather than in ledger_integration_test.go
// purely for readability: they share that file's testPool/seedFixture/
// mustPost/depositInput helpers and its //go:build integration tag, and
// add only the few helpers below that no other test needs.

package ledger

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// TestLedgerTransactions_PlayerScopeSeesNoRows closes a Stage 3B security/
// architecture review finding: ledger_transactions' RLS policy (migration
// 0021) was a bare tenant-match check with no guard excluding a player-
// scoped connection, unlike every other Stage 3B table's two-policy
// pattern - so db.Pool.WithPlayerScope (which sets BOTH app.tenant_id and
// app.player_account_id) satisfied it completely, granting full tenant-
// wide read/write despite the table's own comment claiming "Tenant-scoped
// only, for staff/system". Migration 0028 fixed this; this test proves it
// holds for the exact scenario reviewers described - a player-scoped
// connection querying its own tenant's ledger_transactions directly (e.g.
// the natural next feature, "show me my transaction history") must see
// nothing, even though the very same rows are visible under staff/system
// (WithTenant) scope.
func TestLedgerTransactions_PlayerScopeSeesNoRows(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	mustPost(t, pool, f, depositInput(f, "player-scope-rls-test", 500))

	var staffCount int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, f.tenantID).Scan(&staffCount)
	})
	if err != nil {
		t.Fatalf("staff-scoped count: %v", err)
	}
	if staffCount == 0 {
		t.Fatal("test setup bug: expected at least one ledger_transactions row visible under staff scope")
	}

	var playerCount int
	err = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, f.tenantID).Scan(&playerCount)
	})
	if err != nil {
		t.Fatalf("player-scoped count: %v", err)
	}
	if playerCount != 0 {
		t.Fatalf("expected 0 ledger_transactions rows visible under player scope (same tenant, same player who owns them), got %d - the RLS hardening in migration 0028 has regressed", playerCount)
	}

	// A player-scoped INSERT forging a transaction must also fail, not
	// merely be invisible on read - WITH CHECK, not just USING.
	err = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key)
			 VALUES (gen_random_uuid(), $1, 'deposit', 'forged-from-player-scope')`,
			f.tenantID,
		)
		return err
	})
	if err == nil {
		t.Fatal("expected a player-scoped INSERT into ledger_transactions to be rejected by RLS's WITH CHECK")
	}
}

// seedAssetAccounts creates a second wallet for f's player in assetCode,
// plus that wallet's player_cash account and the tenant's psp_clearing
// account for the same asset - so a test can post in an asset whose
// assets.decimal_exponent differs from the fixture's EUR (2).
func seedAssetAccounts(t *testing.T, pool *db.Pool, f fixture, assetCode string) (cashAccountID, clearingID uuid.UUID) {
	t.Helper()
	walletID := uuid.New()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, $5)`,
			walletID, f.tenantID, f.brandID, f.playerAccountID, assetCode); err != nil {
			return err
		}
		var err error
		cashAccountID, err = GetOrCreateAccount(ctx, tx, f.tenantID, &walletID, AccountPlayerCash, assetCode)
		if err != nil {
			return err
		}
		clearingID, err = GetOrCreateAccount(ctx, tx, f.tenantID, nil, AccountPSPClearing, assetCode)
		return err
	})
	if err != nil {
		t.Fatalf("seed %s accounts: %v", assetCode, err)
	}
	return cashAccountID, clearingID
}

// TestPost_ConcurrentDoubleSpendSameProviderTxDifferentKeys is the double
// spend case the concurrent-duplicate test does not reach: the same
// provider transaction replayed concurrently under DIFFERENT idempotency
// keys, which the (tenant_id, provider_id, provider_tx_id) unique index
// (migration 0021) - not the idempotency key - has to catch. Exactly one
// posting may survive; the money must be credited exactly once.
func TestPost_ConcurrentDoubleSpendSameProviderTxDifferentKeys(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	const n = 8
	const providerTxID = "psp-double-spend"
	var wg sync.WaitGroup
	results := make([]PostResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// A distinct idempotency key per attempt, so the ONLY thing
			// standing between these calls and a double credit is the
			// provider-reference unique index.
			in := depositInput(f, uuid.New().String(), 500)
			providerTx := providerTxID
			in.ProviderTxID = &providerTx
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				results[i], err = Post(ctx, tx, in)
				return err
			})
		}(i)
	}
	wg.Wait()

	// Whether a loser is rejected outright or reported as an idempotent
	// no-op is a question for the caller's error handling; what this test
	// fixes is that exactly one of them may actually POST.
	var posted, rejected int
	for i := 0; i < n; i++ {
		switch {
		case errs[i] != nil:
			rejected++
		case !results[i].AlreadyPosted:
			posted++
		}
	}
	if posted != 1 {
		t.Fatalf("expected exactly 1 of %d concurrent same-provider-tx posts to post, got %d (rejected=%d)", n, posted, rejected)
	}

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND provider_tx_id = $2`,
			f.tenantID, providerTxID,
		).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			t.Fatalf("expected exactly 1 ledger_transactions row for the replayed provider tx, got %d", count)
		}
		cash, err := GetProjectedBalance(ctx, tx, f.cashAccountID)
		if err != nil {
			return err
		}
		if cash.Signed() != 500 {
			t.Fatalf("expected the provider tx to be credited exactly once (500), got %d", cash.Signed())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// assertAppendOnlyRejection asserts err is the exception raised by the
// ledger_deny_mutation trigger (migration 0021) itself - not an RLS
// denial, not a silent "0 rows matched", and not any other incidental
// failure that would let an immutability test pass for the wrong reason.
func assertAppendOnlyRejection(t *testing.T, op string, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected %s to fail with a Postgres error, got: %v", op, err)
	}
	if !strings.Contains(pgErr.Message, "append-only") {
		t.Fatalf("expected %s to be rejected by the append-only trigger, got: %s", op, pgErr.Message)
	}
}

// TestLedgerEntries_ImmutableEvenForOwningRole proves the append-only
// guarantee is enforced by Postgres itself (the ledger_entries_immutable
// trigger, migration 0022), not by the absence of an application code
// path: a raw UPDATE and a raw DELETE issued on the application's own
// connection - which owns the table and could re-GRANT itself any
// privilege - are both rejected.
func TestLedgerEntries_ImmutableEvenForOwningRole(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	posted := mustPost(t, pool, f, depositInput(f, "dep-immutable-entries", 1000))

	updateErr := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE ledger_entries SET amount = 1 WHERE ledger_transaction_id = $1`, posted.TransactionID)
		return err
	})
	assertAppendOnlyRejection(t, "UPDATE against ledger_entries", updateErr)

	deleteErr := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM ledger_entries WHERE ledger_transaction_id = $1`, posted.TransactionID)
		return err
	})
	assertAppendOnlyRejection(t, "DELETE against ledger_entries", deleteErr)

	// Confirm history is genuinely untouched, and the projection with it.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		var total int64
		if err := tx.QueryRow(ctx,
			`SELECT count(*), COALESCE(SUM(amount), 0) FROM ledger_entries WHERE ledger_transaction_id = $1`,
			posted.TransactionID,
		).Scan(&count, &total); err != nil {
			return err
		}
		if count != 2 || total != 2000 {
			t.Fatalf("expected the original 2 entries totalling 2000 to survive, got count=%d total=%d", count, total)
		}
		cash, err := GetProjectedBalance(ctx, tx, f.cashAccountID)
		if err != nil {
			return err
		}
		if cash.Signed() != 1000 {
			t.Fatalf("expected balance to remain 1000 after the rejected mutations, got %d", cash.Signed())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// TestLedgerTransactions_ImmutableEvenForOwningRole is the same proof for
// the transaction header row (the ledger_transactions_immutable trigger,
// migration 0021) - including an attempt to rewrite transaction_type,
// which is exactly how a "correction" would be smuggled in as an edit of
// history rather than as a compensating entry.
func TestLedgerTransactions_ImmutableEvenForOwningRole(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	posted := mustPost(t, pool, f, depositInput(f, "dep-immutable-tx", 750))

	updateErr := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE ledger_transactions SET transaction_type = 'manual_adjustment' WHERE id = $1`, posted.TransactionID)
		return err
	})
	assertAppendOnlyRejection(t, "UPDATE against ledger_transactions", updateErr)

	deleteErr := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM ledger_transactions WHERE id = $1`, posted.TransactionID)
		return err
	})
	assertAppendOnlyRejection(t, "DELETE against ledger_transactions", deleteErr)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var txType TransactionType
		if err := tx.QueryRow(ctx,
			`SELECT transaction_type FROM ledger_transactions WHERE id = $1`, posted.TransactionID,
		).Scan(&txType); err != nil {
			return err
		}
		if txType != TxDeposit {
			t.Fatalf("expected transaction_type to remain %q, got %q", TxDeposit, txType)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// TestCompensation_ReversalIsANewTransactionLeavingHistoryIntact proves a
// correction is a compensating entry, never an edit or deletion
// (CLAUDE.md "Corrections are compensating entries"; Flow 2 in
// financial-transaction-flows.md). The original transaction's rows must
// be bit-for-bit unchanged afterwards - same entry ids, amounts and
// created_at timestamps - and the net effect must come from the SECOND
// transaction alone.
func TestCompensation_ReversalIsANewTransactionLeavingHistoryIntact(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	original := mustPost(t, pool, f, depositInput(f, "dep-to-compensate", 1200))
	before := entriesOf(t, pool, f, original.TransactionID)
	if len(before) != 2 {
		t.Fatalf("expected the original deposit to have 2 entries, got %d", len(before))
	}

	// Flow 2: the exact inverse entries, as a NEW transaction pointing at
	// the original via reverses_transaction_id, with its own provider
	// reference (the chargeback's own).
	provider, providerTx := "mockpsp", "chargeback-1"
	reversal := mustPost(t, pool, f, TransactionInput{
		TenantID:              f.tenantID,
		TransactionType:       TxDepositReversal,
		IdempotencyKey:        "dep-to-compensate-reversal",
		ProviderID:            &provider,
		ProviderTxID:          &providerTx,
		CorrelationID:         uuid.New(),
		ReversesTransactionID: &original.TransactionID,
		Entries: []EntryInput{
			{LedgerAccountID: f.cashAccountID, Direction: Debit, Amount: 1200},
			{LedgerAccountID: f.clearingID, Direction: Credit, Amount: 1200},
		},
	})
	if reversal.TransactionID == original.TransactionID {
		t.Fatal("a compensating transaction must be a new transaction, not the original")
	}

	after := entriesOf(t, pool, f, original.TransactionID)
	if len(after) != len(before) {
		t.Fatalf("original transaction's entry count changed: %d -> %d", len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("original entry %d was mutated: %+v -> %+v", i, before[i], after[i])
		}
	}

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// Both transactions coexist, and the link points forward from the
		// compensation to the original - never the reverse, and never a
		// mutated status field on the original (ledger-accounting-model.md
		// §1.2: 'reversed' is a derived label).
		var reverses *uuid.UUID
		if err := tx.QueryRow(ctx,
			`SELECT reverses_transaction_id FROM ledger_transactions WHERE id = $1`, reversal.TransactionID,
		).Scan(&reverses); err != nil {
			return err
		}
		if reverses == nil || *reverses != original.TransactionID {
			t.Fatalf("expected the compensating transaction to reference the original, got %v", reverses)
		}

		// Net balance is zero, reached by ADDING an entry: the cash account
		// carries the original credit AND the compensating debit, not one
		// edited-to-zero entry.
		cash, err := GetProjectedBalance(ctx, tx, f.cashAccountID)
		if err != nil {
			return err
		}
		if cash.Signed() != 0 {
			t.Fatalf("expected net cash balance 0 after compensation, got %d", cash.Signed())
		}
		if cash.DebitTotal != 1200 || cash.CreditTotal != 1200 {
			t.Fatalf("expected both sides preserved (debit=1200 credit=1200), got debit=%d credit=%d", cash.DebitTotal, cash.CreditTotal)
		}
		var entryCount int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_entries WHERE ledger_account_id = $1`, f.cashAccountID,
		).Scan(&entryCount); err != nil {
			return err
		}
		if entryCount != 2 {
			t.Fatalf("expected 2 cash entries (original + compensation), got %d", entryCount)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// entrySnapshot is a comparable snapshot of one ledger_entries row, used
// to prove history did not change.
type entrySnapshot struct {
	id        uuid.UUID
	accountID uuid.UUID
	direction Direction
	amount    int64
	createdAt time.Time
}

func entriesOf(t *testing.T, pool *db.Pool, f fixture, transactionID uuid.UUID) []entrySnapshot {
	t.Helper()
	var snapshots []entrySnapshot
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, ledger_account_id, direction, amount, created_at
			 FROM ledger_entries WHERE ledger_transaction_id = $1 ORDER BY direction, id`,
			transactionID)
		if err != nil {
			return err
		}
		defer rows.Close()
		snapshots = nil
		for rows.Next() {
			var s entrySnapshot
			if err := rows.Scan(&s.id, &s.accountID, &s.direction, &s.amount, &s.createdAt); err != nil {
				return err
			}
			snapshots = append(snapshots, s)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read entries of %s: %v", transactionID, err)
	}
	return snapshots
}

// TestRebuildProjectionRow_RecreatesADeletedProjectionRow covers the
// other half of the disaster-recovery path
// TestRebuildProjectionRow_MatchesLedgerAfterCorruption exercises: total
// loss of the projection row rather than corruption of its totals. The
// rebuild must reproduce the ledger-derived balance AND the row's
// denormalized identity columns from ledger_entries/ledger_accounts
// alone, deterministically (docs/decisions/0019 "Balance serving").
func TestRebuildProjectionRow_RecreatesADeletedProjectionRow(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	mustPost(t, pool, f, depositInput(f, "dep-rebuild-a", 300))
	mustPost(t, pool, f, depositInput(f, "dep-rebuild-b", 450))

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// wallet_balance_projection is a rebuildable cache, not history -
		// deleting a row here is data loss, not a violated invariant.
		if _, err := tx.Exec(ctx,
			`DELETE FROM wallet_balance_projection WHERE ledger_account_id = $1`, f.cashAccountID); err != nil {
			return err
		}
		lost, err := GetProjectedBalance(ctx, tx, f.cashAccountID)
		if err != nil {
			return err
		}
		if lost.Signed() != 0 {
			t.Fatalf("expected the deleted projection row to read as zero, got %d", lost.Signed())
		}

		// The ledger itself is unaffected by the projection's loss.
		recomputed, err := RebuildBalance(ctx, tx, f.cashAccountID)
		if err != nil {
			return err
		}
		if recomputed.Signed() != 750 || recomputed.DebitTotal != 0 || recomputed.CreditTotal != 750 {
			t.Fatalf("expected ledger-derived balance 750 (debit=0 credit=750), got %+v", recomputed)
		}

		rebuilt, err := RebuildProjectionRow(ctx, tx, f.cashAccountID)
		if err != nil {
			return err
		}
		if rebuilt != recomputed {
			t.Fatalf("rebuild returned %+v, which differs from the recomputation %+v", rebuilt, recomputed)
		}

		// The recreated row must carry the correct denormalized identity
		// columns, not just the right totals.
		var tenantID, walletID, playerAccountID uuid.UUID
		var assetCode, accountType string
		var debitTotal, creditTotal int64
		if err := tx.QueryRow(ctx,
			`SELECT tenant_id, wallet_id, player_account_id, asset_code, account_type, debit_total, credit_total
			 FROM wallet_balance_projection WHERE ledger_account_id = $1`, f.cashAccountID,
		).Scan(&tenantID, &walletID, &playerAccountID, &assetCode, &accountType, &debitTotal, &creditTotal); err != nil {
			return err
		}
		if tenantID != f.tenantID || walletID != f.walletID || playerAccountID != f.playerAccountID {
			t.Fatalf("rebuilt row has wrong identity columns: tenant=%s wallet=%s player=%s", tenantID, walletID, playerAccountID)
		}
		if assetCode != "EUR" || accountType != string(AccountPlayerCash) {
			t.Fatalf("rebuilt row has wrong asset/type: %s/%s", assetCode, accountType)
		}
		if debitTotal != 0 || creditTotal != 750 {
			t.Fatalf("rebuilt row has wrong totals: debit=%d credit=%d", debitTotal, creditTotal)
		}

		// Deterministic: rebuilding an already-correct row changes nothing.
		second, err := RebuildProjectionRow(ctx, tx, f.cashAccountID)
		if err != nil {
			return err
		}
		if second != rebuilt {
			t.Fatalf("second rebuild produced a different balance: %+v vs %+v", second, rebuilt)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// TestPost_AssetsWithDifferentExponentsAreAccountedIndependently proves
// amounts are exact integer minor units interpreted against the owning
// asset's own assets.decimal_exponent (CLAUDE.md's no-floating-point
// rule, ADR 0021): EUR (exponent 2), USDT (6) and BTC (8) posted for the
// SAME player never mix, and a value chosen to be unrepresentable in
// float64 survives the round trip exactly.
func TestPost_AssetsWithDifferentExponentsAreAccountedIndependently(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	btcCash, btcClearing := seedAssetAccounts(t, pool, f, "BTC")
	usdtCash, usdtClearing := seedAssetAccounts(t, pool, f, "USDT")

	// The exponents are read from the Asset registry, never assumed.
	exponents := map[string]int{}
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT code, decimal_exponent FROM assets WHERE code = ANY($1)`,
			[]string{"EUR", "USDT", "BTC"})
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var code string
			var exp int
			if err := rows.Scan(&code, &exp); err != nil {
				return err
			}
			exponents[code] = exp
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read asset registry: %v", err)
	}
	if exponents["EUR"] != 2 || exponents["USDT"] != 6 || exponents["BTC"] != 8 {
		t.Fatalf("expected distinct exponents EUR=2 USDT=6 BTC=8, got %v", exponents)
	}

	// 9007199254740993 is 2^53+1: the smallest positive integer float64
	// cannot represent. Any float64 anywhere on this path would return
	// ...992 instead.
	const beyondFloat64 int64 = 9007199254740993
	const oneSatoshi int64 = 1
	const eurAmount int64 = 12345  // 123.45 EUR
	const usdtAmount int64 = 1     // 0.000001 USDT
	const usdtAmount2 int64 = 5000 // 0.005 USDT

	mustPost(t, pool, f, depositInput(f, "multiasset-eur", eurAmount))
	mustPost(t, pool, f, assetDepositInput(f, "multiasset-btc-1", btcClearing, btcCash, oneSatoshi))
	mustPost(t, pool, f, assetDepositInput(f, "multiasset-btc-2", btcClearing, btcCash, beyondFloat64))
	mustPost(t, pool, f, assetDepositInput(f, "multiasset-usdt-1", usdtClearing, usdtCash, usdtAmount))
	mustPost(t, pool, f, assetDepositInput(f, "multiasset-usdt-2", usdtClearing, usdtCash, usdtAmount2))

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for _, c := range []struct {
			name      string
			accountID uuid.UUID
			assetCode string
			want      int64
		}{
			{"EUR cash", f.cashAccountID, "EUR", eurAmount},
			{"BTC cash", btcCash, "BTC", oneSatoshi + beyondFloat64},
			{"USDT cash", usdtCash, "USDT", usdtAmount + usdtAmount2},
			{"EUR clearing", f.clearingID, "EUR", -eurAmount},
			{"BTC clearing", btcClearing, "BTC", -(oneSatoshi + beyondFloat64)},
			{"USDT clearing", usdtClearing, "USDT", -(usdtAmount + usdtAmount2)},
		} {
			projected, err := GetProjectedBalance(ctx, tx, c.accountID)
			if err != nil {
				return err
			}
			if projected.Signed() != c.want {
				t.Fatalf("%s: expected balance %d, got %d", c.name, c.want, projected.Signed())
			}
			if projected.AssetCode != c.assetCode {
				t.Fatalf("%s: expected asset %s, got %s", c.name, c.assetCode, projected.AssetCode)
			}
			// The projection is only a cache - the ledger itself must
			// carry the same exact integers.
			rebuilt, err := RebuildBalance(ctx, tx, c.accountID)
			if err != nil {
				return err
			}
			if rebuilt != projected {
				t.Fatalf("%s: ledger-derived balance %+v differs from projection %+v", c.name, rebuilt, projected)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// assetDepositInput is depositInput for an asset other than the
// fixture's EUR, whose clearing/cash accounts the caller supplies.
func assetDepositInput(f fixture, idempotencyKey string, clearingID, cashID uuid.UUID, amount int64) TransactionInput {
	provider := "mockpsp"
	providerTx := idempotencyKey
	return TransactionInput{
		TenantID:        f.tenantID,
		TransactionType: TxDeposit,
		IdempotencyKey:  idempotencyKey,
		ProviderID:      &provider,
		ProviderTxID:    &providerTx,
		CorrelationID:   uuid.New(),
		Entries: []EntryInput{
			{LedgerAccountID: clearingID, Direction: Debit, Amount: amount},
			{LedgerAccountID: cashID, Direction: Credit, Amount: amount},
		},
	}
}

// TestPost_UnbalancedVariantsRejected extends
// TestPost_UnbalancedTransactionRejected (which covers only a missing
// leg) to every other shape an unbalanced posting can take - including
// the adversarial one a per-TRANSACTION-only check would accept: entries
// that cancel out across two assets but balance within neither
// (invariant #1 is per transaction, PER ASSET - migration 0022). Each
// case must also leave nothing behind.
func TestPost_UnbalancedVariantsRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	btcCash, btcClearing := seedAssetAccounts(t, pool, f, "BTC")

	tests := []struct {
		name    string
		entries []EntryInput
	}{
		{
			name: "debit exceeds credit",
			entries: []EntryInput{
				{LedgerAccountID: f.clearingID, Direction: Debit, Amount: 500},
				{LedgerAccountID: f.cashAccountID, Direction: Credit, Amount: 400},
			},
		},
		{
			name: "credit exceeds debit",
			entries: []EntryInput{
				{LedgerAccountID: f.clearingID, Direction: Debit, Amount: 400},
				{LedgerAccountID: f.cashAccountID, Direction: Credit, Amount: 500},
			},
		},
		{
			name: "two debits and no credit",
			entries: []EntryInput{
				{LedgerAccountID: f.clearingID, Direction: Debit, Amount: 100},
				{LedgerAccountID: f.cashAccountID, Direction: Debit, Amount: 100},
			},
		},
		{
			name: "cancels across assets but balances within neither",
			entries: []EntryInput{
				{LedgerAccountID: f.clearingID, Direction: Debit, Amount: 100},
				{LedgerAccountID: btcCash, Direction: Credit, Amount: 100},
			},
		},
		{
			name: "balanced in EUR, unbalanced in BTC",
			entries: []EntryInput{
				{LedgerAccountID: f.clearingID, Direction: Debit, Amount: 100},
				{LedgerAccountID: f.cashAccountID, Direction: Credit, Amount: 100},
				{LedgerAccountID: btcClearing, Direction: Debit, Amount: 50},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key := "unbalanced-" + uuid.New().String()
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := Post(ctx, tx, TransactionInput{
					TenantID:        f.tenantID,
					TransactionType: TxDeposit,
					IdempotencyKey:  key,
					CorrelationID:   uuid.New(),
					Entries:         tc.entries,
				})
				return err
			})
			if err == nil {
				t.Fatal("expected an unbalanced transaction to be rejected, got nil error")
			}
			// Rejected by the per-asset balance trigger specifically, not
			// incidentally by some other constraint.
			if !strings.Contains(err.Error(), "unbalanced transaction") {
				t.Fatalf("expected the balance trigger to reject this posting, got: %v", err)
			}

			// The rejection must roll the whole posting back - no orphan
			// transaction header, no partial entries.
			verifyErr := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var count int
				if err := tx.QueryRow(ctx,
					`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND idempotency_key = $2`,
					f.tenantID, key,
				).Scan(&count); err != nil {
					return err
				}
				if count != 0 {
					t.Fatalf("expected no ledger_transactions row to survive the rejection, got %d", count)
				}
				return nil
			})
			if verifyErr != nil {
				t.Fatalf("verify: %v", verifyErr)
			}
		})
	}

	// Nothing at all was posted by any of the rejected attempts.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for _, accountID := range []uuid.UUID{f.cashAccountID, f.clearingID, btcCash, btcClearing} {
			balance, err := RebuildBalance(ctx, tx, accountID)
			if err != nil {
				return err
			}
			if balance.DebitTotal != 0 || balance.CreditTotal != 0 {
				t.Fatalf("account %s has entries after only-rejected postings: %+v", accountID, balance)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
}
