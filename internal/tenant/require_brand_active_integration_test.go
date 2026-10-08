//go:build integration

// H(8) (ADR 0095 section 44 decisions 19-23): RequireBrandActive is the brand-only policy
// check; it never reads the tenant (decision 23) and fails closed. Runs as the runtime role.
package tenant

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestRequireBrandActive_BrandOnly_FailClosed(t *testing.T) {
	owner := brandPinOwnerPool(t)
	rt := brandPinRuntimePool(t)
	ctx := context.Background()
	a := seedBrandPinTenant(t, owner)
	b := seedBrandPinTenant(t, owner)

	check := func(tenantID, brandID uuid.UUID) error {
		return rt.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return RequireBrandActive(ctx, tx, tenantID, brandID)
		})
	}
	setBrand := func(f brandPinFixture, status string) {
		if err := owner.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE brands SET status = $2 WHERE id = $1`, f.brandID, status)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := check(a.tenantID, a.brandID); err != nil {
		t.Fatalf("active brand must pass: %v", err)
	}
	// Tenant and brand are separate: a non-active TENANT with an active brand passes the
	// brand-only check (the tenant policy is a different check), and the combined gate still refuses.
	if err := owner.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, a.tenantID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := check(a.tenantID, a.brandID); err != nil {
		t.Fatalf("RequireBrandActive must not read the tenant status: %v", err)
	}
	if err := rt.WithTenant(ctx, a.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return RequireActiveForPaymentInitiation(ctx, tx, a.tenantID, a.brandID)
	}); !errors.Is(err, ErrNotActiveForPaymentInitiation) || errors.Is(err, ErrBrandNotActive) {
		t.Fatalf("combined gate with a suspended tenant must refuse as tenant, not brand: %v", err)
	}

	for _, st := range []string{"suspended", "closed"} {
		setBrand(b, st)
		err := check(b.tenantID, b.brandID)
		if !errors.Is(err, ErrBrandNotActive) || !errors.Is(err, ErrNotActiveForPaymentInitiation) {
			t.Fatalf("brand %s: want ErrBrandNotActive+ErrNotActiveForPaymentInitiation, got %v", st, err)
		}
		if err := rt.WithTenant(ctx, b.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return RequireActiveForPaymentInitiation(ctx, tx, b.tenantID, b.brandID)
		}); !errors.Is(err, ErrNotActiveForPaymentInitiation) {
			t.Fatalf("combined gate semantics changed for brand %s: %v", st, err)
		}
	}
	setBrand(b, "active")
	if err := check(b.tenantID, b.brandID); err != nil {
		t.Fatalf("reactivated brand must pass: %v", err)
	}
	// Fail closed: nil id, missing row, another tenant's brand.
	for name, args := range map[string][2]uuid.UUID{
		"nil":      {a.tenantID, uuid.Nil},
		"missing":  {a.tenantID, uuid.New()},
		"foreign":  {a.tenantID, b.brandID},
		"foreign2": {b.tenantID, a.brandID},
	} {
		if err := check(args[0], args[1]); !errors.Is(err, ErrBrandNotActive) {
			t.Fatalf("%s: want fail-closed ErrBrandNotActive, got %v", name, err)
		}
	}
}
