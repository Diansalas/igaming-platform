//go:build integration

package ledger

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := db.Connect(context.Background(), url, 10, 5_000_000_000)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedTenantWalletFixture creates a tenant/brand/person/player_account/
// wallet chain and two ledger accounts (one player-owned player_cash,
// one house-level psp_clearing) ready to post balanced transactions
// between. Returns the tenant id, player_account id, and both account
// ids.
type fixture struct {
	tenantID        uuid.UUID
	brandID         uuid.UUID
	playerAccountID uuid.UUID
	walletID        uuid.UUID
	cashAccountID   uuid.UUID
	clearingID      uuid.UUID
}

func seedFixture(t *testing.T, pool *db.Pool) fixture {
	t.Helper()
	f := fixture{
		tenantID:        uuid.New(),
		brandID:         uuid.New(),
		playerAccountID: uuid.New(),
	}
	personID := uuid.New()

	// Stage 4I Phase E-SECURITY (migration 0077): `tenants` writes now
	// require a genuinely platform-admin-scoped transaction.
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model, status) VALUES ($1, $2, 'Test Tenant', 'under_platform_licence', 'active')`,
			f.tenantID, "t-"+f.tenantID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 tenant row, inserted %d", tag.RowsAffected())
		}
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed platform-level rows: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO brands (id, tenant_id, slug, name, status) VALUES ($1, $2, $3, 'Test Brand', 'active')`,
			f.brandID, f.tenantID, "b-"+f.brandID.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			f.playerAccountID, f.tenantID, f.brandID, personID, f.playerAccountID.String()+"@example.com"); err != nil {
			return err
		}
		f.walletID = uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, 'EUR')`,
			f.walletID, f.tenantID, f.brandID, f.playerAccountID); err != nil {
			return err
		}
		var err error
		f.cashAccountID, err = GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		f.clearingID, err = GetOrCreateAccount(ctx, tx, f.tenantID, nil, AccountPSPClearing, "EUR")
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant-scoped fixture: %v", err)
	}
	return f
}

func mustPost(t *testing.T, pool *db.Pool, f fixture, in TransactionInput) PostResult {
	t.Helper()
	var res PostResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = Post(ctx, tx, in)
		return err
	})
	if err != nil {
		t.Fatalf("Post failed: %v", err)
	}
	return res
}

// depositInput builds one deposit posting. CorrelationID is derived
// deterministically from the tenant and key, so two calls with the same
// key model a legitimate retry of ONE logical operation (which reuses its
// correlation id) - since the Stage 10 F-7 remediation, a differing
// correlation under one key is ErrIdempotencyPayloadMismatch, not a
// replay (ADR 0020 amendment 2026-09-25).
func depositInput(f fixture, idempotencyKey string, amount int64) TransactionInput {
	provider := "mockpsp"
	providerTx := idempotencyKey
	return TransactionInput{
		TenantID:        f.tenantID,
		TransactionType: TxDeposit,
		IdempotencyKey:  idempotencyKey,
		ProviderID:      &provider,
		ProviderTxID:    &providerTx,
		CorrelationID:   uuid.NewSHA1(f.tenantID, []byte(idempotencyKey)),
		Entries: []EntryInput{
			{LedgerAccountID: f.clearingID, Direction: Debit, Amount: amount},
			{LedgerAccountID: f.cashAccountID, Direction: Credit, Amount: amount},
		},
	}
}

func TestPost_BalancedDepositUpdatesProjection(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	mustPost(t, pool, f, depositInput(f, "dep-1", 1000))

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cash, err := GetProjectedBalance(ctx, tx, f.cashAccountID)
		if err != nil {
			return err
		}
		if cash.Signed() != 1000 {
			t.Fatalf("expected player_cash balance 1000, got %d (debit=%d credit=%d)", cash.Signed(), cash.DebitTotal, cash.CreditTotal)
		}
		clearing, err := GetProjectedBalance(ctx, tx, f.clearingID)
		if err != nil {
			return err
		}
		if clearing.Signed() != -1000 {
			t.Fatalf("expected psp_clearing signed balance -1000 (debit-normal), got %d", clearing.Signed())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestPost_UnbalancedTransactionRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	provider := "mockpsp"
	providerTx := "unbalanced-1"
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Post(ctx, tx, TransactionInput{
			TenantID:        f.tenantID,
			TransactionType: TxDeposit,
			IdempotencyKey:  "unbalanced-1",
			ProviderID:      &provider,
			ProviderTxID:    &providerTx,
			CorrelationID:   uuid.New(),
			Entries: []EntryInput{
				{LedgerAccountID: f.clearingID, Direction: Debit, Amount: 500},
				// missing matching credit - must fail synchronously inside Post
			},
		})
		return err
	})
	if err == nil {
		t.Fatal("expected an unbalanced transaction to be rejected, got nil error")
	}
}

func TestPost_ExactRetryIsIdempotent(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	first := mustPost(t, pool, f, depositInput(f, "dep-retry", 250))
	if first.AlreadyPosted {
		t.Fatal("first post should not be AlreadyPosted")
	}
	second := mustPost(t, pool, f, depositInput(f, "dep-retry", 250))
	if !second.AlreadyPosted {
		t.Fatal("retried post should be AlreadyPosted")
	}
	if second.TransactionID != first.TransactionID {
		t.Fatalf("retried post returned a different transaction id: %s vs %s", second.TransactionID, first.TransactionID)
	}

	// Balance must reflect ONE deposit, not two.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cash, err := GetProjectedBalance(ctx, tx, f.cashAccountID)
		if err != nil {
			return err
		}
		if cash.Signed() != 250 {
			t.Fatalf("expected balance 250 after exact retry (no double-post), got %d", cash.Signed())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// TestPost_SameKeyDifferentTypeRejected (renamed from
// ...DifferentPayloadRejected by the Stage 10 F-7 remediation: it only
// ever varied the TYPE; payload differences under one type are covered by
// replay_integration_test.go).
func TestPost_SameKeyDifferentTypeRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	mustPost(t, pool, f, depositInput(f, "dep-mismatch", 100))

	provider := "mockpsp"
	providerTx := "dep-mismatch-2"
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Post(ctx, tx, TransactionInput{
			TenantID:        f.tenantID,
			TransactionType: TxManualAdjustment, // different type, same idempotency key
			IdempotencyKey:  "dep-mismatch",
			ReasonCode:      strPtr("test"),
			ProviderID:      &provider,
			ProviderTxID:    &providerTx,
			CorrelationID:   uuid.New(),
			Entries: []EntryInput{
				{LedgerAccountID: f.clearingID, Direction: Debit, Amount: 100},
				{LedgerAccountID: f.cashAccountID, Direction: Credit, Amount: 100},
			},
		})
		return err
	})
	if !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("expected ErrIdempotencyKeyReused, got %v", err)
	}
}

func TestPost_ConcurrentDuplicatesOnlyOneWins(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	const n = 8
	var wg sync.WaitGroup
	results := make([]PostResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				results[i], err = Post(ctx, tx, depositInput(f, "dep-concurrent", 777))
				return err
			})
			errs[i] = err
		}(i)
	}
	wg.Wait()

	var succeeded, alreadyPosted int
	firstID := uuid.Nil
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: unexpected error: %v", i, errs[i])
		}
		if results[i].AlreadyPosted {
			alreadyPosted++
		} else {
			succeeded++
		}
		if firstID == uuid.Nil {
			firstID = results[i].TransactionID
		} else if results[i].TransactionID != firstID {
			t.Fatalf("goroutine %d returned a different transaction id: %s vs %s", i, results[i].TransactionID, firstID)
		}
	}
	if succeeded != 1 {
		t.Fatalf("expected exactly 1 goroutine to actually post, got %d (alreadyPosted=%d)", succeeded, alreadyPosted)
	}

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cash, err := GetProjectedBalance(ctx, tx, f.cashAccountID)
		if err != nil {
			return err
		}
		if cash.Signed() != 777 {
			t.Fatalf("expected balance 777 after %d concurrent duplicate posts, got %d", n, cash.Signed())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestRebuildProjectionRow_MatchesLedgerAfterCorruption(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	mustPost(t, pool, f, depositInput(f, "dep-a", 300))
	mustPost(t, pool, f, depositInput(f, "dep-b", 450))

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		before, err := GetProjectedBalance(ctx, tx, f.cashAccountID)
		if err != nil {
			return err
		}
		if before.Signed() != 750 {
			t.Fatalf("expected 750 before corruption, got %d", before.Signed())
		}

		// Corrupt the projection directly (simulating drift/data loss) -
		// this requires superuser-equivalent bypass in a real deployment,
		// but the test role owns the table, so a direct UPDATE is enough
		// to prove the rebuild path is exercised and correct.
		if _, err := tx.Exec(ctx,
			`UPDATE wallet_balance_projection SET debit_total = 999999, credit_total = 111111 WHERE ledger_account_id = $1`,
			f.cashAccountID,
		); err != nil {
			return err
		}
		corrupted, err := GetProjectedBalance(ctx, tx, f.cashAccountID)
		if err != nil {
			return err
		}
		if corrupted.Signed() == 750 {
			t.Fatal("corruption step had no effect - test is not exercising anything")
		}

		rebuilt, err := RebuildProjectionRow(ctx, tx, f.cashAccountID)
		if err != nil {
			return err
		}
		if rebuilt.Signed() != 750 {
			t.Fatalf("expected rebuild to restore 750, got %d", rebuilt.Signed())
		}

		after, err := GetProjectedBalance(ctx, tx, f.cashAccountID)
		if err != nil {
			return err
		}
		if after.Signed() != 750 {
			t.Fatalf("expected projection to read 750 after rebuild, got %d", after.Signed())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestGetOrCreateAccount_ConcurrentFirstUseCreatesExactlyOneRow(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	// A second wallet/asset combination the fixture hasn't already
	// created an account for, so this test actually exercises the
	// create path, not just the fetch path.
	secondWallet := uuid.New()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, 'USD')`,
			secondWallet, f.tenantID, f.brandID, f.playerAccountID)
		return err
	})
	if err != nil {
		t.Fatalf("seed second wallet: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	ids := make([]uuid.UUID, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				id, err := GetOrCreateAccount(ctx, tx, f.tenantID, &secondWallet, AccountPlayerCash, "USD")
				ids[i] = id
				return err
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
		if ids[i] != ids[0] {
			t.Fatalf("goroutine %d got a different account id: %s vs %s", i, ids[i], ids[0])
		}
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_accounts WHERE wallet_id = $1 AND account_type = 'player_cash' AND asset_code = 'USD'`,
			secondWallet,
		).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			t.Fatalf("expected exactly 1 ledger_accounts row after %d concurrent GetOrCreateAccount calls, got %d", n, count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func strPtr(s string) *string { return &s }
