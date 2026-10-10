//go:build integration

package identity

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := db.Connect(context.Background(), url, 5, 5*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// createTestTenant provisions a tenant directly (bypassing the HTTP
// layer, since these tests exercise the identity package itself).
// Stage 4I Phase E-SECURITY (migration 0077): `tenants` gained RLS with no
// tenant-scoped write policy of any kind, so both CreateTenant's own
// assertPlatformScope and the DELETE cleanup below now require a genuinely
// platform-admin-scoped transaction (db.Pool.WithPlatformAdmin), not
// WithoutTenant.
func createTestTenant(t *testing.T, pool *db.Pool) Tenant {
	t.Helper()
	suffix := uuid.New().String()
	var tenant Tenant
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		// ADR 0112 / LF2: CreateTenant now creates pending_launch; this fixture needs an
		// active tenant, which only the table-owner role may provision (the database writes
		// the owner_provisioned transition). Package identity cannot import
		// internal/testsupport/launchfix (that package imports identity), so it inserts directly.
		tenant = Tenant{ID: uuid.New(), Name: "Test Tenant " + suffix, Slug: "tenant-" + suffix, LicensingModel: "under_platform_licence", Status: "active"}
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model, status) VALUES ($1, $2, $3, $4, 'active')`,
			tenant.ID, tenant.Name, tenant.Slug, tenant.LicensingModel)
		return err
	})
	if err != nil {
		t.Fatalf("failed to create test tenant: %v", err)
	}
	t.Cleanup(func() {
		_ = pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenant.ID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				t.Errorf("cleanup: expected to delete 1 tenant row, deleted %d", tag.RowsAffected())
			}
			return nil
		})
	})
	return tenant
}

func createTestBrand(t *testing.T, pool *db.Pool, tenant Tenant) Brand {
	t.Helper()
	suffix := uuid.New().String()
	var brand Brand
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		// ADR 0112 / LF2: see createTestTenant; owner-provisioned active brand.
		brand = Brand{ID: uuid.New(), TenantID: tenant.ID, Name: "Test Brand", Slug: "brand-" + suffix, Status: "active"}
		_, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, name, slug, status) VALUES ($1, $2, $3, $4, 'active')`,
			brand.ID, brand.TenantID, brand.Name, brand.Slug)
		return err
	})
	if err != nil {
		t.Fatalf("failed to create test brand: %v", err)
	}
	return brand
}

// --- Brand RLS: public read, tenant-scoped write ---

func TestBrand_PublicallyReadableAcrossTenants(t *testing.T) {
	pool := testPool(t)
	tenantA := createTestTenant(t, pool)
	brandA := createTestBrand(t, pool, tenantA)

	// GetBrandBySlug uses WithoutTenant (public read) - must succeed
	// regardless of which tenant created the brand, since this is the
	// pre-authentication tenant-resolution path (see ADR 0012).
	got, err := GetBrandBySlug(context.Background(), pool, brandA.Slug)
	if err != nil {
		t.Fatalf("expected public brand read to succeed, got error: %v", err)
	}
	if got.ID != brandA.ID {
		t.Errorf("expected brand id %s, got %s", brandA.ID, got.ID)
	}
}

func TestBrand_CrossTenantWriteDenied(t *testing.T) {
	pool := testPool(t)
	tenantA := createTestTenant(t, pool)
	tenantB := createTestTenant(t, pool)

	// Attempt to create a brand claiming tenant B while scoped to tenant A.
	err := pool.WithTenant(context.Background(), tenantA.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO brands (id, tenant_id, name, slug, status) VALUES ($1, $2, $3, $4, 'active')`,
			uuid.New(), tenantB.ID, "Forged Brand", "forged-"+uuid.NewString(),
		)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("expected a row-level-security violation (42501), got: %v", err)
	}
}

// --- Player account: tenant isolation, registration, lookup ---

func TestRegisterPlayer_AndLookupByEmail(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)
	brand := createTestBrand(t, pool, tenant)

	var account PlayerAccount
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		account, err = RegisterPlayer(ctx, tx, brand, "Player@Example.com", "hashed-password")
		return err
	})
	if err != nil {
		t.Fatalf("unexpected error registering player: %v", err)
	}
	if account.Email != "player@example.com" {
		t.Errorf("expected email to be lowercased, got %q", account.Email)
	}

	var found PlayerAccount
	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		found, err = GetPlayerAccountByEmail(ctx, tx, brand.ID, "PLAYER@EXAMPLE.COM")
		return err
	})
	if err != nil {
		t.Fatalf("expected case-insensitive lookup to succeed, got error: %v", err)
	}
	if found.ID != account.ID {
		t.Errorf("expected to find the same account, got a different id")
	}
}

func TestRegisterPlayer_DuplicateEmailSameBrandRejected(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)
	brand := createTestBrand(t, pool, tenant)

	register := func() error {
		return pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := RegisterPlayer(ctx, tx, brand, "dup@example.com", "hash")
			return err
		})
	}
	if err := register(); err != nil {
		t.Fatalf("first registration should succeed, got: %v", err)
	}
	err := register()
	if !errors.Is(err, ErrEmailTaken) {
		t.Fatalf("expected ErrEmailTaken on duplicate registration, got: %v", err)
	}
}

func TestPlayerAccount_CrossTenantReadDenied(t *testing.T) {
	pool := testPool(t)
	tenantA := createTestTenant(t, pool)
	brandA := createTestBrand(t, pool, tenantA)
	tenantB := createTestTenant(t, pool)

	var accountA PlayerAccount
	err := pool.WithTenant(context.Background(), tenantA.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		accountA, err = RegisterPlayer(ctx, tx, brandA, "isolated@example.com", "hash")
		return err
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Tenant B must not be able to read tenant A's player account by id,
	// even with the exact id.
	err = pool.WithTenant(context.Background(), tenantB.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := GetPlayerAccountByID(ctx, tx, accountA.ID)
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound when tenant B reads tenant A's player account, got: %v", err)
	}
}

func TestSetPlayerAccountStatus_Suspend(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)
	brand := createTestBrand(t, pool, tenant)

	var account PlayerAccount
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		account, err = RegisterPlayer(ctx, tx, brand, "suspend-me@example.com", "hash")
		return err
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return SetPlayerAccountStatus(ctx, tx, account.ID, PlayerStatusSuspended)
	})
	if err != nil {
		t.Fatalf("unexpected error suspending account: %v", err)
	}

	var reloaded PlayerAccount
	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reloaded, err = GetPlayerAccountByID(ctx, tx, account.ID)
		return err
	})
	if err != nil {
		t.Fatalf("unexpected error reloading account: %v", err)
	}
	if reloaded.Status != PlayerStatusSuspended {
		t.Errorf("expected status %q, got %q", PlayerStatusSuspended, reloaded.Status)
	}
}

// --- Staff users: dual-scope RLS ---

func TestStaffUser_PlatformAdminOnlyVisibleWithoutTenant(t *testing.T) {
	pool := testPool(t)
	suffix := uuid.NewString()

	var admin StaffUser
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		admin, err = CreateStaffUser(ctx, tx, uuid.Nil, "admin-"+suffix+"@platform.test", "hash", StaffRolePlatformAdmin, nil)
		return err
	})
	if err != nil {
		t.Fatalf("unexpected error creating platform admin: %v", err)
	}
	t.Cleanup(func() {
		_ = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM staff_users WHERE id = $1`, admin.ID)
			return err
		})
	})

	// Visible via WithoutTenant.
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := GetStaffUserByEmail(ctx, tx, admin.Email)
		return err
	})
	if err != nil {
		t.Errorf("expected platform admin to be visible via WithoutTenant, got error: %v", err)
	}

	// NOT visible via any tenant-scoped connection.
	tenant := createTestTenant(t, pool)
	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := GetStaffUserByEmail(ctx, tx, admin.Email)
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected platform admin to be invisible via a tenant-scoped connection, got: %v", err)
	}
}

func TestStaffUser_TenantScopedNotVisibleFromOtherTenant(t *testing.T) {
	pool := testPool(t)
	tenantA := createTestTenant(t, pool)
	tenantB := createTestTenant(t, pool)
	suffix := uuid.NewString()

	var staffA StaffUser
	err := pool.WithTenant(context.Background(), tenantA.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		staffA, err = CreateStaffUser(ctx, tx, tenantA.ID, "ta-"+suffix+"@acme.test", "hash", StaffRoleTenantAdmin, nil)
		return err
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	err = pool.WithTenant(context.Background(), tenantB.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := GetStaffUserByEmail(ctx, tx, staffA.Email)
		return err
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("expected tenant A's staff user to be invisible from tenant B, got: %v", err)
	}
}

func TestCreateStaffUser_PlatformAdminMustBeTenantless(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)

	// The CHECK constraint on staff_users requires platform_admin rows to
	// have a NULL tenant_id - attempting to create a tenant-scoped
	// platform_admin must fail at the database level.
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, $2, $3, $4, $5)`,
			uuid.New(), tenant.ID, "bad-admin@test.com", "hash", "platform_admin",
		)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a *pgconn.PgError creating a tenant-scoped platform_admin, got %T: %v", err, err)
	}
	const pgCheckViolationCode = "23514"
	if pgErr.Code != pgCheckViolationCode {
		t.Fatalf("expected SQLSTATE %s (check violation), got %s: %v", pgCheckViolationCode, pgErr.Code, err)
	}
}

// --- Login attempt lockout ---

func TestLoginAttempts_LockoutAfterThreshold(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)
	identifier := "lockout-test-" + uuid.NewString()

	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		locked, err := IsLockedOut(ctx, tx, identifier)
		if err != nil {
			return err
		}
		if locked {
			t.Error("expected not locked out before any attempts")
		}

		for i := 0; i < lockoutThreshold; i++ {
			if err := RecordLoginAttempt(ctx, tx, &tenant.ID, "player", identifier, "127.0.0.1", false); err != nil {
				return err
			}
		}

		locked, err = IsLockedOut(ctx, tx, identifier)
		if err != nil {
			return err
		}
		if !locked {
			t.Errorf("expected locked out after %d failed attempts", lockoutThreshold)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoginAttempts_SuccessResetsLockoutWindow(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)
	identifier := "reset-test-" + uuid.NewString()

	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		for i := 0; i < lockoutThreshold-1; i++ {
			if err := RecordLoginAttempt(ctx, tx, &tenant.ID, "player", identifier, "127.0.0.1", false); err != nil {
				return err
			}
		}
		// A success before hitting the threshold means earlier failures
		// no longer count toward lockout.
		if err := RecordLoginAttempt(ctx, tx, &tenant.ID, "player", identifier, "127.0.0.1", true); err != nil {
			return err
		}
		if err := RecordLoginAttempt(ctx, tx, &tenant.ID, "player", identifier, "127.0.0.1", false); err != nil {
			return err
		}

		locked, err := IsLockedOut(ctx, tx, identifier)
		if err != nil {
			return err
		}
		if locked {
			t.Error("expected NOT locked out - only one failure since the last success")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- Licensing model / licence consistency (Stage 1 tech debt fix) ---

func TestTenant_LicensingModelMustMatchLicenceLicensee(t *testing.T) {
	pool := testPool(t)

	// Stage 4I Phase E-SECURITY (migration 0077): `licences` gained RLS
	// with writes restricted to a platform-admin-scoped transaction, so
	// seeding here - and the two UPDATE tenants SET licence_id calls below
	// - must move from WithoutTenant to WithPlatformAdmin. A silently
	// zero-row UPDATE (a denied RLS write is a no-op, not an error) is
	// exactly the failure mode this migration's own rollout discovered
	// elsewhere, so both UPDATEs below check rows-affected explicitly
	// rather than trusting a nil error alone.
	var jurisdictionID uuid.UUID
	var platformLicenceID, tenantLicenceID uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO jurisdictions (code, name) VALUES ($1, 'Test Jurisdiction') RETURNING id`,
			"TEST-"+uuid.NewString()[:8],
		).Scan(&jurisdictionID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO licences (jurisdiction_id, licensee, licence_number) VALUES ($1, 'platform', 'PLAT-1') RETURNING id`,
			jurisdictionID,
		).Scan(&platformLicenceID); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`INSERT INTO licences (jurisdiction_id, licensee, licence_number) VALUES ($1, 'tenant', 'TEN-1') RETURNING id`,
			jurisdictionID,
		).Scan(&tenantLicenceID)
	})
	if err != nil {
		t.Fatalf("unexpected error seeding jurisdiction/licences: %v", err)
	}

	tenant := createTestTenant(t, pool) // licensing_model = under_platform_licence

	// Matching case: under_platform_licence + a 'platform' licence succeeds.
	err = pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $1 WHERE id = $2`, platformLicenceID, tenant.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			t.Errorf("expected to update 1 tenant row, updated %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Errorf("expected matching licensing_model/licensee to succeed, got error: %v", err)
	}

	// Mismatched case: same tenant (still under_platform_licence) pointed
	// at a 'tenant' licensee licence must be rejected by the composite FK.
	err = pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $1 WHERE id = $2`, tenantLicenceID, tenant.ID)
		return err
	})
	if err == nil {
		t.Fatal("expected an error linking a licensing_model='under_platform_licence' tenant to a licensee='tenant' licence, got nil")
	}
}

// TestCreateTenant_NonPlatformScopedTransactionIsRejected is the new
// regression test required by Stage 4I Phase E-SECURITY (migration 0077):
// CreateTenant's assertPlatformScope must reject a tenant-scoped
// transaction, distinctly from any RLS error the INSERT itself might
// otherwise surface.
func TestCreateTenant_NonPlatformScopedTransactionIsRejected(t *testing.T) {
	pool := testPool(t)
	existing := createTestTenant(t, pool)

	err := pool.WithTenant(context.Background(), existing.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateTenant(ctx, tx, "Should Not Exist", "should-not-exist-"+uuid.NewString(), "under_platform_licence")
		return err
	})
	if !errors.Is(err, ErrPlatformTransactionScope) {
		t.Fatalf("expected ErrPlatformTransactionScope, got %v", err)
	}

	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateTenant(ctx, tx, "Should Not Exist Either", "should-not-exist-either-"+uuid.NewString(), "under_platform_licence")
		return err
	})
	if !errors.Is(err, ErrPlatformTransactionScope) {
		t.Fatalf("expected ErrPlatformTransactionScope from a scopeless transaction, got %v", err)
	}
}
