//go:build integration

package wallet

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
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

type fixture struct {
	tenantID        uuid.UUID
	brandID         uuid.UUID
	playerAccountID uuid.UUID
}

func seedFixture(t *testing.T, pool *db.Pool) fixture {
	t.Helper()
	f := fixture{tenantID: uuid.New(), brandID: uuid.New(), playerAccountID: uuid.New()}
	personID := uuid.New()

	// Stage 4I Phase E-SECURITY (migration 0077): `tenants` writes now
	// require a genuinely platform-admin-scoped transaction.
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'Test Tenant', 'under_platform_licence')`,
			f.tenantID, "t-"+f.tenantID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 tenant row, inserted %d", tag.RowsAffected())
		}
		_, err = tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed platform rows: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'Test Brand')`,
			f.brandID, f.tenantID, "b-"+f.brandID.String()[:8]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			f.playerAccountID, f.tenantID, f.brandID, personID, f.playerAccountID.String()+"@example.com")
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant rows: %v", err)
	}
	return f
}

func TestGetOrCreate_ConcurrentFirstUseCreatesExactlyOneWallet(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	const n = 8
	var wg sync.WaitGroup
	ids := make([]uuid.UUID, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				w, err := GetOrCreate(ctx, tx, f.tenantID, f.brandID, f.playerAccountID, "EUR")
				ids[i] = w.ID
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
			t.Fatalf("goroutine %d got a different wallet id: %s vs %s", i, ids[i], ids[0])
		}
	}

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM wallets WHERE player_account_id = $1 AND asset_code = 'EUR'`,
			f.playerAccountID,
		).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			t.Fatalf("expected exactly 1 wallet row after %d concurrent GetOrCreate calls, got %d", n, count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestGetSummary_ReflectsPostedEntries(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var w Wallet
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		w, err = GetOrCreate(ctx, tx, f.tenantID, f.brandID, f.playerAccountID, "EUR")
		if err != nil {
			return err
		}
		cashID, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &w.ID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		clearingID, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountPSPClearing, "EUR")
		if err != nil {
			return err
		}
		provider, providerTx := "mockpsp", "dep-1"
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxDeposit, IdempotencyKey: "dep-1",
			ProviderID: &provider, ProviderTxID: &providerTx, CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: clearingID, Direction: ledger.Debit, Amount: 5000},
				{LedgerAccountID: cashID, Direction: ledger.Credit, Amount: 5000},
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		summary, err := GetSummary(ctx, tx, w)
		if err != nil {
			return err
		}
		if summary.CashBalance != 5000 {
			t.Fatalf("expected cash balance 5000, got %d", summary.CashBalance)
		}
		if summary.AvailableBalance != 5000 {
			t.Fatalf("expected available balance 5000, got %d", summary.AvailableBalance)
		}
		if summary.HeldForWithdrawal != 0 || summary.LockedBalance != 0 || summary.BonusBalance != 0 {
			t.Fatalf("expected all other balances zero, got %+v", summary)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestPlayerScope_CannotSeeAnotherPlayersWallet(t *testing.T) {
	pool := testPool(t)
	f1 := seedFixture(t, pool)

	// seedFixture creates a fresh tenant every call, so a second call
	// would test cross-TENANT isolation (already covered elsewhere).
	// Instead, add a second player under f1's own tenant/brand so this
	// test exercises cross-player-within-the-SAME-tenant isolation - the
	// exact gap ADR 0016 found for `sessions` and this design exists to
	// close for financial data.
	// actually exercises cross-player-within-the-SAME-tenant isolation,
	// not cross-tenant isolation (already covered elsewhere).
	player2 := uuid.New()
	person2 := uuid.New()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person2)
		return err
	})
	if err != nil {
		t.Fatalf("seed second person: %v", err)
	}
	err = pool.WithTenant(context.Background(), f1.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			player2, f1.tenantID, f1.brandID, person2, player2.String()+"@example.com")
		if err != nil {
			return err
		}
		_, err = GetOrCreate(ctx, tx, f1.tenantID, f1.brandID, f1.playerAccountID, "EUR")
		return err
	})
	if err != nil {
		t.Fatalf("seed second player + first player's wallet: %v", err)
	}

	err = pool.WithPlayerScope(context.Background(), f1.tenantID, player2, func(ctx context.Context, tx pgx.Tx) error {
		wallets, err := List(ctx, tx, f1.playerAccountID) // deliberately asking for player 1's wallets while scoped as player 2
		if err != nil {
			return err
		}
		if len(wallets) != 0 {
			t.Fatalf("player 2's scope must not see player 1's wallets, got %d", len(wallets))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("player-scope query: %v", err)
	}
}
