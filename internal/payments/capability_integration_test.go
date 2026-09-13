//go:build integration

package payments

import (
	"context"
	"errors"
	"os"
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

type capFixture struct {
	tenantID uuid.UUID
	brandID  uuid.UUID
}

func seedCapFixture(t *testing.T, pool *db.Pool) capFixture {
	t.Helper()
	f := capFixture{tenantID: uuid.New(), brandID: uuid.New()}

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'Test Tenant', 'under_platform_licence')`,
			f.tenantID, "t-"+f.tenantID.String()[:8])
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'Test Brand')`,
			f.brandID, f.tenantID, "b-"+f.brandID.String()[:8])
		return err
	})
	if err != nil {
		t.Fatalf("seed brand: %v", err)
	}
	return f
}

func TestWriteAndLoadCapability_TenantWideRow(t *testing.T) {
	pool := testPool(t)
	f := seedCapFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR", "USD")

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := WriteCapability(ctx, tx, provider, f.tenantID, nil, CapabilityConfig{
			SupportedFiatCurrencies: []string{"EUR"},
			SupportedPaymentMethods: []string{"card"},
			SupportsDeposit:         true,
			SupportsWithdrawal:      true,
			AmountLimits:            []AmountLimit{{AssetCode: "EUR", MinAmount: 500, MaxAmount: 100000}},
			Priority:                50,
			Status:                  CapabilityActive,
		})
		return err
	})
	if err != nil {
		t.Fatalf("WriteCapability: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		loaded, found, err := LoadCapability(ctx, tx, f.tenantID, uuid.New(), "mock-psp")
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("expected tenant-wide capability row to be found for any brand")
		}
		if loaded.ProviderKind != ProviderKindFiat {
			t.Fatalf("expected provider_kind fiat, got %v", loaded.ProviderKind)
		}
		if loaded.Priority != 50 {
			t.Fatalf("expected priority 50, got %d", loaded.Priority)
		}
		if len(loaded.AmountLimits) != 1 || loaded.AmountLimits[0].MinAmount != 500 {
			t.Fatalf("expected 1 amount limit with min 500, got %+v", loaded.AmountLimits)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("LoadCapability: %v", err)
	}
}

func TestWriteCapability_RejectsWideningBeyondAdapter(t *testing.T) {
	pool := testPool(t)
	f := seedCapFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := WriteCapability(ctx, tx, provider, f.tenantID, nil, CapabilityConfig{
			SupportedFiatCurrencies: []string{"EUR", "USD"}, // adapter only declares EUR
			SupportedPaymentMethods: []string{"card"},
			SupportsDeposit:         true,
			AmountLimits:            []AmountLimit{{AssetCode: "EUR", MinAmount: 500, MaxAmount: 100000}},
			Status:                  CapabilityActive,
		})
		return err
	})
	if !errors.Is(err, ErrCapabilityWidensAdapter) {
		t.Fatalf("expected ErrCapabilityWidensAdapter, got %v", err)
	}
}

func TestWriteCapability_RejectsAmountRangeWiderThanAdapter(t *testing.T) {
	pool := testPool(t)
	f := seedCapFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR") // declares 100-100000000 for EUR

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := WriteCapability(ctx, tx, provider, f.tenantID, nil, CapabilityConfig{
			SupportedFiatCurrencies: []string{"EUR"},
			SupportedPaymentMethods: []string{"card"},
			SupportsDeposit:         true,
			AmountLimits:            []AmountLimit{{AssetCode: "EUR", MinAmount: 0, MaxAmount: 999_999_999_999}},
			Status:                  CapabilityActive,
		})
		return err
	})
	if !errors.Is(err, ErrCapabilityWidensAdapter) {
		t.Fatalf("expected ErrCapabilityWidensAdapter for an out-of-range amount limit, got %v", err)
	}
}

func TestBrandSpecificCapability_ReplacesTenantWideRowEntirely(t *testing.T) {
	pool := testPool(t)
	f := seedCapFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR", "USD")

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := WriteCapability(ctx, tx, provider, f.tenantID, nil, CapabilityConfig{
			SupportedFiatCurrencies: []string{"EUR", "USD"},
			SupportedPaymentMethods: []string{"card"},
			SupportsDeposit:         true,
			AmountLimits: []AmountLimit{
				{AssetCode: "EUR", MinAmount: 100, MaxAmount: 100000},
				{AssetCode: "USD", MinAmount: 100, MaxAmount: 100000},
			},
			Priority: 100,
			Status:   CapabilityActive,
		}); err != nil {
			return err
		}
		// Brand-specific row narrows to EUR only - whole-row replacement,
		// not a merge (docs/decisions/0022 §3).
		_, err := WriteCapability(ctx, tx, provider, f.tenantID, &f.brandID, CapabilityConfig{
			SupportedFiatCurrencies: []string{"EUR"},
			SupportedPaymentMethods: []string{"card"},
			SupportsDeposit:         true,
			AmountLimits:            []AmountLimit{{AssetCode: "EUR", MinAmount: 100, MaxAmount: 100000}},
			Priority:                10,
			Status:                  CapabilityActive,
		})
		return err
	})
	if err != nil {
		t.Fatalf("write capabilities: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		forBrand, found, err := LoadCapability(ctx, tx, f.tenantID, f.brandID, "mock-psp")
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("expected brand-specific row to be found")
		}
		if len(forBrand.SupportedFiatCurrencies) != 1 || forBrand.SupportedFiatCurrencies[0] != "EUR" {
			t.Fatalf("expected brand row to be narrowed to [EUR], got %v", forBrand.SupportedFiatCurrencies)
		}

		forOtherBrand, found, err := LoadCapability(ctx, tx, f.tenantID, uuid.New(), "mock-psp")
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("expected a different brand to still see the tenant-wide row")
		}
		if len(forOtherBrand.SupportedFiatCurrencies) != 2 {
			t.Fatalf("expected the tenant-wide row's [EUR USD] for an unrelated brand, got %v", forOtherBrand.SupportedFiatCurrencies)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestListRoutingCandidates_TenantIsolation(t *testing.T) {
	pool := testPool(t)
	f1 := seedCapFixture(t, pool)
	f2 := seedCapFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")

	err := pool.WithTenant(context.Background(), f1.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := WriteCapability(ctx, tx, provider, f1.tenantID, nil, CapabilityConfig{
			SupportedFiatCurrencies: []string{"EUR"}, SupportedPaymentMethods: []string{"card"},
			SupportsDeposit: true, AmountLimits: []AmountLimit{{AssetCode: "EUR", MinAmount: 100, MaxAmount: 100000}},
			Status: CapabilityActive,
		})
		return err
	})
	if err != nil {
		t.Fatalf("write capability for tenant 1: %v", err)
	}

	err = pool.WithTenant(context.Background(), f2.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		candidates, err := ListRoutingCandidates(ctx, tx, f2.tenantID, f2.brandID)
		if err != nil {
			return err
		}
		if len(candidates) != 0 {
			t.Fatalf("tenant 2 must not see tenant 1's provider_capabilities row, got %d candidates", len(candidates))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify isolation: %v", err)
	}
}
