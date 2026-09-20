//go:build integration

// Real-PostgreSQL tests for migration 0076 and this package: the write
// paths (CreateLicenceCountryCeilingVersion/CreateOperatingCountryPolicyVersion),
// the resolution algorithm (resolve()/ResolveOperatingCountryPolicy), the
// explain-why diagnostic, the registration projection, RLS, audit
// content, and the four licence behavioural cases (ADR 0045). Follows
// internal/jurisdiction/jurisdiction_integration_test.go's fixture
// conventions.
package operatingmarket

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
)

const (
	pgRLSViolation    = "42501"
	pgRaisedError     = "P0001"
	pgCheckViolation  = "23514"
	pgUniqueViolation = "23505"
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

func assertPgCode(t *testing.T, err error, code string) *pgconn.PgError {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a *pgconn.PgError, got %T: %v", err, err)
	}
	if pgErr.Code != code {
		t.Fatalf("expected SQLSTATE %s, got %s: %s", code, pgErr.Code, pgErr.Message)
	}
	return pgErr
}

type fixture struct {
	tenantID       uuid.UUID
	otherTenantID  uuid.UUID
	jurisdictionID uuid.UUID
	licenceID      uuid.UUID
	otherLicenceID uuid.UUID
	brandID        uuid.UUID
	platformAdmin  uuid.UUID
	staffActorID   uuid.UUID
}

// seedFixture creates two tenants each bound to their OWN licence (so the
// composite-ownership ceiling-read test has a genuinely foreign licence
// to attempt), a brand under the first tenant, and a platform-admin staff
// principal.
func seedFixture(t *testing.T, pool *db.Pool) fixture {
	t.Helper()
	var f fixture
	f.tenantID = uuid.New()
	f.otherTenantID = uuid.New()
	f.jurisdictionID = uuid.New()
	f.licenceID = uuid.New()
	f.otherLicenceID = uuid.New()
	f.platformAdmin = uuid.New()
	f.staffActorID = uuid.New()

	// Stage 4I Phase E-SECURITY (migration 0077): `tenants`/`jurisdictions`/
	// `licences` writes now require a genuinely platform-admin-scoped
	// transaction; rows-affected is checked explicitly on every write
	// below because a denied RLS write is a silent zero-row no-op, not an
	// error. `staff_users` is unaffected (its dual_scope_isolation policy
	// only keys on app.tenant_id, which WithPlatformAdmin also leaves
	// unset, exactly like WithoutTenant).
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		for _, id := range []uuid.UUID{f.tenantID, f.otherTenantID} {
			tag, err := tx.Exec(ctx,
				`INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'Test Tenant', 'under_platform_licence')`,
				id, "t-"+id.String()[:8])
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("expected to insert 1 tenant row, inserted %d", tag.RowsAffected())
			}
		}
		tag, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'Test Jurisdiction')`,
			f.jurisdictionID, "TJ-"+f.jurisdictionID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 jurisdiction row, inserted %d", tag.RowsAffected())
		}
		for _, l := range []uuid.UUID{f.licenceID, f.otherLicenceID} {
			tag, err := tx.Exec(ctx,
				`INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, 'platform', $3)`,
				l, f.jurisdictionID, "LIC-"+l.String()[:8])
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("expected to insert 1 licence row, inserted %d", tag.RowsAffected())
			}
		}
		tag, err = tx.Exec(ctx, `UPDATE tenants SET licence_id = $2 WHERE id = $1`, f.tenantID, f.licenceID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to update 1 tenant row, updated %d", tag.RowsAffected())
		}
		tag, err = tx.Exec(ctx, `UPDATE tenants SET licence_id = $2 WHERE id = $1`, f.otherTenantID, f.otherLicenceID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to update 1 tenant row, updated %d", tag.RowsAffected())
		}
		personID := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id, status)
			 VALUES ($1, NULL, $2, 'x', 'platform_admin', $3, 'active')`,
			f.platformAdmin, f.platformAdmin.String()+"@example.com", personID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed tenants/jurisdiction/licences: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		f.brandID = uuid.New()
		_, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'Brand A')`,
			f.brandID, f.tenantID, "b-"+f.brandID.String()[:8])
		return err
	})
	if err != nil {
		t.Fatalf("seed brand: %v", err)
	}
	return f
}

func testActor(actorID uuid.UUID, reason string) ActorContext {
	return ActorContext{ActorID: actorID, ReasonCode: reason}
}

// enableCeiling is a test helper that enables country cc under licenceID
// via the sanctioned write path.
func enableCeiling(t *testing.T, pool *db.Pool, platformAdmin, licenceID uuid.UUID, cc string) CeilingRecord {
	t.Helper()
	var rec CeilingRecord
	err := pool.WithPlatformAdmin(context.Background(), platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rec, err = CreateLicenceCountryCeilingVersion(ctx, tx, CreateLicenceCountryCeilingVersionParams{
			LicenceID: licenceID, CountryCode: cc, State: StateEnabled, Status: StatusActive,
			AuthorizationReference: "legal-ref-1", Actor: testActor(platformAdmin, "enable-ceiling"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("enable ceiling for %s: %v", cc, err)
	}
	return rec
}

func disableCeiling(t *testing.T, pool *db.Pool, platformAdmin, licenceID uuid.UUID, cc string) CeilingRecord {
	t.Helper()
	var rec CeilingRecord
	err := pool.WithPlatformAdmin(context.Background(), platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rec, err = CreateLicenceCountryCeilingVersion(ctx, tx, CreateLicenceCountryCeilingVersionParams{
			LicenceID: licenceID, CountryCode: cc, State: StateDisabled, Status: StatusActive,
			Actor: testActor(platformAdmin, "disable-ceiling"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable ceiling for %s: %v", cc, err)
	}
	return rec
}

func enableTenantPolicy(t *testing.T, pool *db.Pool, tenantID, actorID uuid.UUID, cc string) PolicyRecord {
	t.Helper()
	var rec PolicyRecord
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rec, err = CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeTenant, TenantID: tenantID, CountryCode: cc, State: StateEnabled, Status: StatusActive,
			AuthorizationReference: "compliance-ref-1", Actor: testActor(actorID, "enable-tenant"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("enable tenant policy for %s: %v", cc, err)
	}
	return rec
}

func disableTenantPolicy(t *testing.T, pool *db.Pool, tenantID, actorID uuid.UUID, cc string) PolicyRecord {
	t.Helper()
	var rec PolicyRecord
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rec, err = CreateOperatingCountryPolicyVersion(ctx, tx, CreateOperatingCountryPolicyVersionParams{
			Scope: ScopeTenant, TenantID: tenantID, CountryCode: cc, State: StateDisabled, Status: StatusActive,
			Actor: testActor(actorID, "disable-tenant"),
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable tenant policy for %s: %v", cc, err)
	}
	return rec
}

func resolveNow(t *testing.T, pool *db.Pool, tenantID uuid.UUID, q Query) Result {
	t.Helper()
	var res Result
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = ResolveOperatingCountryPolicy(ctx, tx, q)
		return err
	})
	if err != nil {
		t.Fatalf("ResolveOperatingCountryPolicy: %v", err)
	}
	return res
}
