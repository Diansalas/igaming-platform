//go:build integration

// Stage 8 provider-integration readiness (docs/decisions/0080 Decision 2):
// proves the two additive, always-nil-today sportsbook_bets columns
// (provider_id/provider_bet_reference, migration
// 0081_sportsbook_bets_provider_reference) behave exactly as that
// migration's own comments claim - the symmetric-null CHECK constraint
// (mirroring ledger_transactions') and the tenant-scoped partial unique
// index (mirroring idx_ledger_transactions_tenant_provider_tx) - via
// direct SQL inserts, since PlaceBet itself never sets either column this
// stage (no real provider adapter exists yet). Follows this package's own
// orchestrator_integration_test.go fixture/style conventions
// (seedFixture/fundWallet/seedSelection/placeBet, and
// internal/operatingmarket's established pgconn.PgError SQLSTATE-assertion
// convention for constraint-violation tests).
package sportsbook

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
)

const (
	pgCheckViolation  = "23514"
	pgUniqueViolation = "23505"
)

func assertPgCode(t *testing.T, err error, code string) *pgconn.PgError {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a *pgconn.PgError, got %T: %v", err, err)
	}
	if pgErr.Code != code {
		t.Fatalf("expected SQLSTATE %s, got %s: %v", code, pgErr.Code, err)
	}
	return pgErr
}

// strPtr mirrors internal/casino's identical test helper (bonus_settlement_
// integration_test.go's strPtr) for constructing *string literals inline.
func strPtr(s string) *string { return &s }

// rawBetParams is the direct-SQL insert shape provider_reference tests use
// to exercise sportsbook_bets' constraints below the PlaceBet orchestrator
// (which never sets provider_id/provider_bet_reference this stage) - reusing an
// already-placed bet's own FK-satisfying identifiers (selection, wallet,
// player, brand, ledger transaction) so only provider_id/provider_bet_reference/
// idempotency_key vary between rows.
type rawBetParams struct {
	TenantID             uuid.UUID
	BrandID              uuid.UUID
	PlayerAccountID      uuid.UUID
	WalletID             uuid.UUID
	SelectionID          uuid.UUID
	LedgerTransactionID  uuid.UUID
	IdempotencyKey       string
	ProviderID           *string
	ProviderBetReference *string
}

func insertRawBet(ctx context.Context, tx pgx.Tx, p rawBetParams) error {
	_, err := tx.Exec(ctx,
		`INSERT INTO sportsbook_bets
			(id, tenant_id, brand_id, player_account_id, wallet_id, selection_id, asset_code,
			 stake_amount, odds_numerator, odds_denominator, potential_return, status, idempotency_key,
			 ledger_transaction_id, provider_id, provider_bet_reference)
		 VALUES ($1, $2, $3, $4, $5, $6, 'EUR', 1000, 200, 100, 2000, 'open', $7, $8, $9, $10)`,
		uuid.New(), p.TenantID, p.BrandID, p.PlayerAccountID, p.WalletID, p.SelectionID,
		p.IdempotencyKey, p.LedgerTransactionID, p.ProviderID, p.ProviderBetReference,
	)
	return err
}

// setupProviderRefFixture places one real bet (via the existing PlaceBet
// flow) purely to obtain a tenant/brand/player/wallet/selection/ledger-
// transaction tuple that already satisfies every sportsbook_bets FK, so
// the tests below only vary provider_id/provider_bet_reference/idempotency_key.
func setupProviderRefFixture(t *testing.T, pool *db.Pool) (sbFixture, Selection, uuid.UUID) {
	t.Helper()
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel := seedSelection(t, pool, seedSelectionParams{})
	result, err := placeBet(t, pool, f, sel, 1_000, "provider-ref-fixture-bet")
	if err != nil {
		t.Fatalf("place fixture bet: %v", err)
	}
	if !result.Accepted {
		t.Fatalf("expected fixture bet to be accepted, got %q", result.RejectionCategory)
	}
	return f, sel, result.Bet.LedgerTransactionID
}

// TestSportsbookBets_ProviderRefSymmetricNullCheckConstraint proves
// migration 0081's CHECK ((provider_id IS NULL) = (provider_bet_reference IS
// NULL)) - mirroring ledger_transactions' identical constraint - rejects
// half-set rows and accepts both-NULL and both-set rows.
func TestSportsbookBets_ProviderRefSymmetricNullCheckConstraint(t *testing.T) {
	pool := testPool(t)
	f, sel, ledgerTxID := setupProviderRefFixture(t, pool)

	cases := []struct {
		name                 string
		providerID           *string
		providerBetReference *string
		wantCheckFail        bool
	}{
		{name: "provider_id set, provider_bet_reference nil", providerID: strPtr("dummy-sportsbook"), providerBetReference: nil, wantCheckFail: true},
		{name: "provider_id nil, provider_bet_reference set", providerID: nil, providerBetReference: strPtr("ref-1"), wantCheckFail: true},
		{name: "both nil", providerID: nil, providerBetReference: nil, wantCheckFail: false},
		{name: "both set", providerID: strPtr("dummy-sportsbook"), providerBetReference: strPtr("ref-2"), wantCheckFail: false},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idempotencyKey := fmt.Sprintf("provider-ref-check-%d", i)
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return insertRawBet(ctx, tx, rawBetParams{
					TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID,
					WalletID: f.walletID, SelectionID: sel.ID, LedgerTransactionID: ledgerTxID,
					IdempotencyKey: idempotencyKey, ProviderID: tc.providerID, ProviderBetReference: tc.providerBetReference,
				})
			})
			if tc.wantCheckFail {
				if err == nil {
					t.Fatal("expected the symmetric-null CHECK constraint to reject this row, got no error")
				}
				pgErr := assertPgCode(t, err, pgCheckViolation)
				if pgErr.ConstraintName != "sportsbook_bets_provider_reference_symmetric_null" {
					t.Fatalf("expected constraint sportsbook_bets_provider_reference_symmetric_null, got %q", pgErr.ConstraintName)
				}
				return
			}
			if err != nil {
				t.Fatalf("expected this row to be accepted, got: %v", err)
			}
		})
	}
}

// TestSportsbookBets_ProviderRefPartialUniqueIndex proves migration 0081's
// idx_sportsbook_bets_tenant_provider_reference only enforces (tenant_id,
// provider_id, provider_bet_reference) uniqueness among rows where provider_id
// IS NOT NULL: two NULL/NULL rows never collide, a duplicate non-null pair
// within the same tenant does collide, and the identical non-null pair
// across two different tenants does not.
func TestSportsbookBets_ProviderRefPartialUniqueIndex(t *testing.T) {
	pool := testPool(t)
	f, sel, ledgerTxID := setupProviderRefFixture(t, pool)

	t.Run("two NULL provider rows in the same tenant never collide", func(t *testing.T) {
		for i := 0; i < 2; i++ {
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return insertRawBet(ctx, tx, rawBetParams{
					TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID,
					WalletID: f.walletID, SelectionID: sel.ID, LedgerTransactionID: ledgerTxID,
					IdempotencyKey: fmt.Sprintf("provider-ref-null-collision-%d", i),
					ProviderID:     nil, ProviderBetReference: nil,
				})
			})
			if err != nil {
				t.Fatalf("row %d: expected two NULL-provider rows to coexist, got: %v", i, err)
			}
		}
	})

	t.Run("duplicate non-null pair in the same tenant collides", func(t *testing.T) {
		providerID, providerRef := strPtr("dummy-sportsbook"), strPtr("dup-ref-same-tenant")

		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return insertRawBet(ctx, tx, rawBetParams{
				TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID,
				WalletID: f.walletID, SelectionID: sel.ID, LedgerTransactionID: ledgerTxID,
				IdempotencyKey: "provider-ref-dup-first", ProviderID: providerID, ProviderBetReference: providerRef,
			})
		})
		if err != nil {
			t.Fatalf("first insert of the pair: expected success, got: %v", err)
		}

		err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return insertRawBet(ctx, tx, rawBetParams{
				TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID,
				WalletID: f.walletID, SelectionID: sel.ID, LedgerTransactionID: ledgerTxID,
				IdempotencyKey: "provider-ref-dup-second", ProviderID: providerID, ProviderBetReference: providerRef,
			})
		})
		if err == nil {
			t.Fatal("expected the duplicate (tenant, provider_id, provider_bet_reference) pair to collide, got no error")
		}
		pgErr := assertPgCode(t, err, pgUniqueViolation)
		if pgErr.ConstraintName != "idx_sportsbook_bets_tenant_provider_reference" {
			t.Fatalf("expected constraint idx_sportsbook_bets_tenant_provider_reference, got %q", pgErr.ConstraintName)
		}
	})

	t.Run("identical non-null pair in a different tenant does not collide", func(t *testing.T) {
		g, sel2, ledgerTxID2 := setupProviderRefFixture(t, pool)
		providerID, providerRef := strPtr("dummy-sportsbook"), strPtr("dup-ref-cross-tenant")

		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return insertRawBet(ctx, tx, rawBetParams{
				TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID,
				WalletID: f.walletID, SelectionID: sel.ID, LedgerTransactionID: ledgerTxID,
				IdempotencyKey: "provider-ref-cross-tenant-a", ProviderID: providerID, ProviderBetReference: providerRef,
			})
		})
		if err != nil {
			t.Fatalf("tenant A insert: expected success, got: %v", err)
		}

		err = pool.WithTenant(context.Background(), g.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return insertRawBet(ctx, tx, rawBetParams{
				TenantID: g.tenantID, BrandID: g.brandID, PlayerAccountID: g.playerAccountID,
				WalletID: g.walletID, SelectionID: sel2.ID, LedgerTransactionID: ledgerTxID2,
				IdempotencyKey: "provider-ref-cross-tenant-b", ProviderID: providerID, ProviderBetReference: providerRef,
			})
		})
		if err != nil {
			t.Fatalf("tenant B insert: expected the SAME (provider_id, provider_bet_reference) pair to be permitted in a different tenant, got: %v", err)
		}
	})
}

// TestPlaceBet_ProviderReferenceColumnsAlwaysNil is a regression guard for
// this stage's own scope boundary (ADR 0080 Decision 2): the existing,
// unmodified PlaceBet flow never sets provider_id/provider_bet_reference, since
// no external provider round-trip exists or is added this stage.
func TestPlaceBet_ProviderReferenceColumnsAlwaysNil(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	sel := seedSelection(t, pool, seedSelectionParams{})

	result, err := placeBet(t, pool, f, sel, 1_000, "provider-ref-always-nil")
	if err != nil {
		t.Fatalf("place bet: %v", err)
	}
	if !result.Accepted {
		t.Fatalf("expected bet to be accepted, got %q", result.RejectionCategory)
	}
	if result.Bet.ProviderID != nil {
		t.Fatalf("expected ProviderID nil, got %q", *result.Bet.ProviderID)
	}
	if result.Bet.ProviderBetReference != nil {
		t.Fatalf("expected ProviderBetReference nil, got %q", *result.Bet.ProviderBetReference)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		b, err := GetBetByID(ctx, tx, result.Bet.ID)
		if err != nil {
			return err
		}
		if b.ProviderID != nil || b.ProviderBetReference != nil {
			return fmt.Errorf("expected both provider reference columns nil on re-fetch, got provider_id=%v provider_bet_reference=%v", b.ProviderID, b.ProviderBetReference)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("re-fetch bet: %v", err)
	}
}
