//go:build integration

package reconciliation

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

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
	walletID        uuid.UUID
	cashAccountID   uuid.UUID
	clearingID      uuid.UUID
}

func seedFixture(t *testing.T, pool *db.Pool) fixture {
	t.Helper()
	f := fixture{tenantID: uuid.New(), brandID: uuid.New(), playerAccountID: uuid.New()}
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
		_, err = tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed platform rows: %v", err)
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
		f.cashAccountID, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		f.clearingID, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountPSPClearing, "EUR")
		if err != nil {
			return err
		}
		provider, providerTx := "mockpsp", "dep-1"
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxDeposit, IdempotencyKey: "dep-1",
			ProviderID: &provider, ProviderTxID: &providerTx, CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: f.clearingID, Direction: ledger.Debit, Amount: 1000},
				{LedgerAccountID: f.cashAccountID, Direction: ledger.Credit, Amount: 1000},
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant fixture: %v", err)
	}
	return f
}

func TestRunLedgerVsProjection_CleanWhenUncorrupted(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var run Run
	var mismatches []Mismatch
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		run, mismatches, err = RunLedgerVsProjection(ctx, tx, f.tenantID, time.Now().Add(-time.Hour), time.Now())
		return err
	})
	if err != nil {
		t.Fatalf("RunLedgerVsProjection: %v", err)
	}
	if run.Status != StatusClean {
		t.Fatalf("expected StatusClean, got %s (mismatches=%v)", run.Status, mismatches)
	}
	if len(mismatches) != 0 {
		t.Fatalf("expected zero mismatches, got %d", len(mismatches))
	}
}

func TestRunLedgerVsProjection_DetectsInjectedDrift(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	// Inject synthetic drift directly into the projection, simulating a
	// bug that bypassed the same-transaction trigger.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE wallet_balance_projection SET credit_total = credit_total + 500 WHERE ledger_account_id = $1`,
			f.cashAccountID,
		)
		return err
	})
	if err != nil {
		t.Fatalf("inject drift: %v", err)
	}

	var run Run
	var mismatches []Mismatch
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		run, mismatches, err = RunLedgerVsProjection(ctx, tx, f.tenantID, time.Now().Add(-time.Hour), time.Now())
		return err
	})
	if err != nil {
		t.Fatalf("RunLedgerVsProjection: %v", err)
	}
	if run.Status != StatusMismatchesFound {
		t.Fatalf("expected StatusMismatchesFound, got %s", run.Status)
	}
	if len(mismatches) != 1 {
		t.Fatalf("expected exactly 1 mismatch, got %d: %v", len(mismatches), mismatches)
	}
	if mismatches[0].ReconciliationKey != f.cashAccountID.String() {
		t.Fatalf("expected mismatch on cash account %s, got key %s", f.cashAccountID, mismatches[0].ReconciliationKey)
	}

	// The run and mismatch must themselves be persisted and readable back.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_mismatches WHERE reconciliation_run_id = $1`, run.ID).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			t.Fatalf("expected 1 persisted mismatch row, got %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify persistence: %v", err)
	}
}

func TestResolveMismatch_MarksResolved(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE wallet_balance_projection SET debit_total = debit_total + 1 WHERE ledger_account_id = $1`, f.clearingID)
		return err
	})
	if err != nil {
		t.Fatalf("inject drift: %v", err)
	}

	var mismatchID uuid.UUID
	resolver := uuid.New()
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, mismatches, err := RunLedgerVsProjection(ctx, tx, f.tenantID, time.Now().Add(-time.Hour), time.Now())
		if err != nil {
			return err
		}
		if len(mismatches) != 1 {
			t.Fatalf("expected 1 mismatch, got %d", len(mismatches))
		}
		mismatchID = mismatches[0].ID
		return ResolveMismatch(ctx, tx, mismatchID, resolver, "known test-induced drift", nil)
	})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, `SELECT investigation_status FROM reconciliation_mismatches WHERE id = $1`, mismatchID).Scan(&status); err != nil {
			return err
		}
		if status != "resolved" {
			t.Fatalf("expected status 'resolved', got %q", status)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
}
