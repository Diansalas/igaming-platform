//go:build integration

package identity

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// gateBrand inserts a brand with an explicit status through the OWNER pool (the LF2 path; package
// identity cannot import launchfix, which imports it). An empty status leaves the pending_launch default.
func gateBrand(t *testing.T, tenantID uuid.UUID, status string) Brand {
	t.Helper()
	pool := testPool(t)
	b := Brand{ID: uuid.New(), TenantID: tenantID, Name: "gate", Slug: "gate-" + uuid.NewString(), Status: status}
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if status == "" {
			_, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, name, slug) VALUES ($1, $2, $3, $4)`, b.ID, b.TenantID, b.Name, b.Slug)
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, name, slug, status) VALUES ($1, $2, $3, $4, $5)`, b.ID, b.TenantID, b.Name, b.Slug, status)
		return err
	}); err != nil {
		t.Fatalf("insert brand: %v", err)
	}
	return b
}

// ADR 0112 7.3 (code review C-6): EVERY registration entry point of package identity refuses a
// brand that is not active, or whose tenant is not active, with ErrNotAcceptingRegistrations, and
// creates nothing. Removing the shared gate in insertPlayerAccount fails all of these.
func TestRegisterPlayer_AllEntryPoints_RefuseNonActiveBrandOrTenant(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	activeTenant := createTestTenant(t, pool)
	cases := map[string]Brand{
		"pending_launch_brand": gateBrand(t, activeTenant.ID, ""),
		"suspended_brand":      gateBrand(t, activeTenant.ID, "suspended"),
		"closed_brand":         gateBrand(t, activeTenant.ID, "closed"),
	}
	// A brand 'active' under a tenant that is not active.
	pendingTenant := uuid.New()
	if err := pool.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model) VALUES ($1, 'pending tenant', $2, 'under_platform_licence')`, pendingTenant, "pt-"+pendingTenant.String())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cases["active_brand_of_pending_tenant"] = gateBrand(t, pendingTenant, "active")

	for name, brand := range cases {
		t.Run(name, func(t *testing.T) {
			person := uuid.New()
			if err := pool.WithTenant(ctx, brand.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				p, err := CreatePerson(ctx, tx)
				person = p.ID
				return err
			}); err != nil {
				t.Fatal(err)
			}
			entries := map[string]func(ctx context.Context, tx pgx.Tx) error{
				"RegisterPlayer": func(ctx context.Context, tx pgx.Tx) error {
					_, err := RegisterPlayer(ctx, tx, brand, uuid.NewString()+"@example.com", "hash")
					return err
				},
				"RegisterPlayerLinkedToPerson": func(ctx context.Context, tx pgx.Tx) error {
					_, err := RegisterPlayerLinkedToPerson(ctx, tx, brand, uuid.NewString()+"@example.com", "hash", person)
					return err
				},
				"RegisterPlayerPendingReview": func(ctx context.Context, tx pgx.Tx) error {
					_, err := RegisterPlayerPendingReview(ctx, tx, brand, uuid.NewString()+"@example.com", "hash")
					return err
				},
			}
			for entry, run := range entries {
				err := pool.WithTenant(ctx, brand.TenantID, run)
				if !errors.Is(err, ErrNotAcceptingRegistrations) {
					t.Fatalf("%s on %s: want ErrNotAcceptingRegistrations, got %v", entry, name, err)
				}
			}
			var n int
			if err := pool.WithTenant(ctx, brand.TenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM player_accounts WHERE brand_id = $1`, brand.ID).Scan(&n)
			}); err != nil || n != 0 {
				t.Fatalf("a refused registration created %d accounts (err=%v)", n, err)
			}
		})
	}

	// Control: an active brand of an active tenant registers through all three.
	ok := createTestBrand(t, pool, activeTenant)
	if err := pool.WithTenant(ctx, ok.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := RegisterPlayer(ctx, tx, ok, uuid.NewString()+"@example.com", "hash")
		return err
	}); err != nil {
		t.Fatalf("control registration: %v", err)
	}
}
