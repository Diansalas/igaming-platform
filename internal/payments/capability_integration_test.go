//go:build integration

package payments

import (
	"context"
	"errors"
	"fmt"
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
		return nil
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

// TestProviderCapabilityAmountLimits_CrossTenantRoutingIsolation is
// Stage 3C adversarial test 8.E ("provider amount limits from Tenant A
// cannot affect Tenant B"). Both tenants configure the SAME provider_id
// with deliberately DIFFERENT amount limits; an amount that would be
// rejected under tenant A's own (narrow) limit must still route
// successfully under tenant B's (wide) limit, proving RouteProvider
// never consults another tenant's amount_limits row - if it did, this
// would either wrongly reject tenant B's routable amount or wrongly
// accept tenant A's unroutable one.
func TestProviderCapabilityAmountLimits_CrossTenantRoutingIsolation(t *testing.T) {
	pool := testPool(t)
	fA := seedCapFixture(t, pool)
	fB := seedCapFixture(t, pool)
	providerA := NewMockProvider("shared-psp", "EUR")
	providerB := NewMockProvider("shared-psp", "EUR")

	err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := WriteCapability(ctx, tx, providerA, fA.tenantID, nil, CapabilityConfig{
			SupportedFiatCurrencies: []string{"EUR"}, SupportedPaymentMethods: []string{"card"},
			SupportsDeposit: true, AmountLimits: []AmountLimit{{AssetCode: "EUR", MinAmount: 100, MaxAmount: 5000}},
			Status: CapabilityActive,
		})
		return err
	})
	if err != nil {
		t.Fatalf("write tenant A capability (narrow limit): %v", err)
	}

	err = pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := WriteCapability(ctx, tx, providerB, fB.tenantID, nil, CapabilityConfig{
			SupportedFiatCurrencies: []string{"EUR"}, SupportedPaymentMethods: []string{"card"},
			SupportsDeposit: true, AmountLimits: []AmountLimit{{AssetCode: "EUR", MinAmount: 100, MaxAmount: 100_000_000}},
			Status: CapabilityActive,
		})
		return err
	})
	if err != nil {
		t.Fatalf("write tenant B capability (wide limit): %v", err)
	}

	orch := NewOrchestrator(map[string]PaymentProvider{"shared-psp": providerB})
	const amountAboveTenantALimitButWithinTenantB = 50000

	// Under tenant B's own scope, an amount tenant A's limit would reject
	// must still route successfully - proving tenant B's routing decision
	// used tenant B's own limit row, never tenant A's.
	err = pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, capability, err := orch.RouteProvider(ctx, tx, RoutingRequest{
			TenantID: fB.tenantID, BrandID: fB.brandID, AssetCode: "EUR", PaymentMethod: "card",
			Amount: amountAboveTenantALimitButWithinTenantB, Operation: OperationDeposit,
		})
		if err != nil {
			return fmt.Errorf("expected tenant B to route this amount under its own wide limit: %w", err)
		}
		if capability.ProviderID != "shared-psp" {
			t.Fatalf("expected shared-psp to be selected, got %q", capability.ProviderID)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Under tenant A's own scope, the SAME amount must be refused -
	// proving tenant A's routing decision is still governed by its own
	// narrow limit and was never widened by tenant B's row existing.
	orchA := NewOrchestrator(map[string]PaymentProvider{"shared-psp": providerA})
	err = pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := orchA.RouteProvider(ctx, tx, RoutingRequest{
			TenantID: fA.tenantID, BrandID: fA.brandID, AssetCode: "EUR", PaymentMethod: "card",
			Amount: amountAboveTenantALimitButWithinTenantB, Operation: OperationDeposit,
		})
		return err
	})
	if !errors.Is(err, ErrNoRoutableProvider) {
		t.Fatalf("expected ErrNoRoutableProvider for tenant A (amount exceeds its own narrow limit), got %v", err)
	}
}

// TestProviderCapabilityAmountLimits_CrossTenantWriteRejected is Stage
// 3C adversarial test 8.F ("provider amount-limit writes cannot cross
// tenant boundary"). A connection scoped to tenant B attempting to
// INSERT an amount-limit row against tenant A's own provider_capability_id
// - even supplying tenant A's real, valid id - must be rejected: either
// by RLS's WITH CHECK (tenant_id must equal the connection's own
// app.tenant_id) or by the composite FK (provider_capability_id,
// tenant_id) requiring the two to actually agree, whichever fires first.
func TestProviderCapabilityAmountLimits_CrossTenantWriteRejected(t *testing.T) {
	pool := testPool(t)
	fA := seedCapFixture(t, pool)
	fB := seedCapFixture(t, pool)
	providerA := NewMockProvider("mock-psp", "EUR")

	var capabilityAID uuid.UUID
	err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		capabilityAID, err = WriteCapability(ctx, tx, providerA, fA.tenantID, nil, CapabilityConfig{
			SupportedFiatCurrencies: []string{"EUR"}, SupportedPaymentMethods: []string{"card"},
			SupportsDeposit: true, AmountLimits: nil, Status: CapabilityActive,
		})
		return err
	})
	if err != nil {
		t.Fatalf("write tenant A capability: %v", err)
	}

	// Attempt 1: forged tenant_id matching the connection's own scope
	// (fB.tenantID) but a provider_capability_id that belongs to tenant
	// A - the composite FK must reject this, since no row of
	// provider_capabilities has (id=capabilityAID, tenant_id=fB.tenantID).
	err = pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO provider_capability_amount_limits (provider_capability_id, tenant_id, asset_code, min_amount, max_amount)
			 VALUES ($1, $2, 'EUR', 100, 1000)`,
			capabilityAID, fB.tenantID,
		)
		return err
	})
	if err == nil {
		t.Fatal("expected the composite FK to reject a provider_capability_id/tenant_id pair that doesn't match any real capability row")
	}

	// Attempt 2: the honest tenant_id (fA.tenantID) alongside tenant A's
	// real capability id, but issued from a connection scoped to tenant
	// B - RLS's WITH CHECK must reject this regardless of the FK being
	// satisfiable, since app.tenant_id is fB.tenantID here.
	err = pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO provider_capability_amount_limits (provider_capability_id, tenant_id, asset_code, min_amount, max_amount)
			 VALUES ($1, $2, 'EUR', 100, 1000)`,
			capabilityAID, fA.tenantID,
		)
		return err
	})
	if err == nil {
		t.Fatal("expected RLS's WITH CHECK to reject an INSERT naming a different tenant_id than the connection's own scope")
	}
}
