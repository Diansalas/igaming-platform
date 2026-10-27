//go:build integration

package jurisdiction

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestResolveLicensingChain_RealDatabase(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	read := func(tenantID uuid.UUID) LicensingChain {
		var out LicensingChain
		if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			out, err = ResolveLicensingChain(ctx, tx, tenantID)
			return err
		}); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		return out
	}
	got := read(f.tenantID)
	if got.Outcome != LicensingResolved || got.LicenceID != f.licenceID || got.JurisdictionID != f.jurisdictionID || got.JurisdictionCode == "" {
		t.Fatalf("licensed tenant: %+v", got)
	}
	if none := read(f.otherTenantID); none.Outcome != LicensingNoLicence {
		t.Fatalf("unlicensed tenant must be no_licence, got %+v", none)
	}
	// A tenant that is not the session's tenant is not visible (RLS): never "resolved".
	var cross LicensingChain
	if err := pool.WithTenant(context.Background(), f.otherTenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		cross, err = ResolveLicensingChain(ctx, tx, f.tenantID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if cross.Outcome != LicensingInvalid || cross.InvalidReason == "" {
		t.Fatalf("cross-tenant read must be licence_invalid with a reason (which of the two rows RLS hides is not part of the contract), got %+v", cross)
	}
}
