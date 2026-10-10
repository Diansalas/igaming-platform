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

	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
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
		// ADR 0112: a brand status moves only through a governed decision; the fixture runs
		// the real guards as owner-provisioned platform operators (launchfix, LF2).
		launchfix.SetBrandStatus(t, f.tenantID, f.brandID, status)
	}

	if err := check(a.tenantID, a.brandID); err != nil {
		t.Fatalf("active brand must pass: %v", err)
	}
	// Tenant and brand are separate: a non-active TENANT with an active brand passes the
	// brand-only check (the tenant policy is a different check), and the combined gate still refuses.
	launchfix.SetTenantStatus(t, a.tenantID, "suspended")
	if err := check(a.tenantID, a.brandID); err != nil {
		t.Fatalf("RequireBrandActive must not read the tenant status: %v", err)
	}
	if err := rt.WithTenant(ctx, a.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return RequireActiveForPaymentInitiation(ctx, tx, a.tenantID, a.brandID)
	}); !errors.Is(err, ErrNotActiveForPaymentInitiation) || errors.Is(err, ErrBrandNotActive) {
		t.Fatalf("combined gate with a suspended tenant must refuse as tenant, not brand: %v", err)
	}

	for _, st := range []string{"suspended", "closed"} {
		target := b
		if st == "closed" {
			// ADR 0112 section 3.1: closed is terminal, so the closed case uses its own brand
			// (b is reactivated below, which the old raw-UPDATE fixture did from closed).
			target = seedBrandPinTenant(t, owner)
		}
		setBrand(target, st)
		err := check(target.tenantID, target.brandID)
		if !errors.Is(err, ErrBrandNotActive) || !errors.Is(err, ErrNotActiveForPaymentInitiation) {
			t.Fatalf("brand %s: want ErrBrandNotActive+ErrNotActiveForPaymentInitiation, got %v", st, err)
		}
		if err := rt.WithTenant(ctx, target.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return RequireActiveForPaymentInitiation(ctx, tx, target.tenantID, target.brandID)
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

	// A read error is returned, never read as "active" and never as a brand refusal.
	err := rt.WithTenant(ctx, a.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cctx, cancel := context.WithCancel(ctx)
		cancel()
		return RequireBrandActive(cctx, tx, a.tenantID, a.brandID)
	})
	if err == nil || errors.Is(err, ErrBrandNotActive) {
		t.Fatalf("a read error must be returned as a plain error (not nil, not a refusal), got %v", err)
	}
}
