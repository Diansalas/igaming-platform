//go:build integration

// Real-PostgreSQL tests for migration 0071 and this package: the
// tenant_licence basis (B-4), Persist's write path, the RLS/append-only
// contract on jurisdiction_resolutions (B-2), the jurisdiction_resolution_
// active fact (B-6), and the jurisdictions/licences registry write
// surface (B-1). Follows internal/assetregistry/assetregistry_integration_
// test.go's fixture conventions.
package jurisdiction

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
	pgRLSViolation   = "42501"
	pgRaisedError    = "P0001"
	pgCheckViolation = "23514"
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
	jurisdiction2  uuid.UUID
	licenceID      uuid.UUID
	brandID        uuid.UUID
	playerID       uuid.UUID
	platformAdmin  uuid.UUID
}

// seedFixture creates two tenants (one with a licence pointing at a real
// jurisdiction, one without), a brand+player under the first, and a
// platform-admin staff principal - everything the resolver/registry/
// resolution-active tests below need, seeded once via direct SQL (this
// package's own admin/resolver functions are what is under test, so
// fixture seeding deliberately does not go through them).
func seedFixture(t *testing.T, pool *db.Pool) fixture {
	t.Helper()
	var f fixture
	f.tenantID = uuid.New()
	f.otherTenantID = uuid.New()
	f.jurisdictionID = uuid.New()
	f.jurisdiction2 = uuid.New()
	f.licenceID = uuid.New()
	f.platformAdmin = uuid.New()

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
				`INSERT INTO tenants (id, slug, name, licensing_model, status) VALUES ($1, $2, 'Test Tenant', 'under_platform_licence', 'active')`,
				id, "t-"+id.String()[:8])
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("expected to insert 1 tenant row, inserted %d", tag.RowsAffected())
			}
		}
		tag, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'Test Jurisdiction A')`,
			f.jurisdictionID, "TJ-"+f.jurisdictionID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 jurisdiction row, inserted %d", tag.RowsAffected())
		}
		tag, err = tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'Test Jurisdiction B')`,
			f.jurisdiction2, "TJ-"+f.jurisdiction2.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 jurisdiction row, inserted %d", tag.RowsAffected())
		}
		tag, err = tx.Exec(ctx,
			`INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, 'platform', 'LIC-1')`,
			f.licenceID, f.jurisdictionID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 licence row, inserted %d", tag.RowsAffected())
		}
		tag, err = tx.Exec(ctx, `UPDATE tenants SET licence_id = $2 WHERE id = $1`, f.tenantID, f.licenceID)
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
		t.Fatalf("seed tenants/jurisdictions/licence: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		f.brandID = uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name, status) VALUES ($1, $2, $3, 'Brand A', 'active')`,
			f.brandID, f.tenantID, "b-"+f.brandID.String()[:8]); err != nil {
			return err
		}
		f.playerID = uuid.New()
		personID := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			f.playerID, f.tenantID, f.brandID, personID, f.playerID.String()+"@example.com")
		return err
	})
	if err != nil {
		t.Fatalf("seed brand/player: %v", err)
	}
	return f
}

// --- B-4: tenant_licence basis ---

func TestResolveTenantLicence_ProducesResolvedFromTheRealRegistryRelationship(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var res Resolution
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = Resolve(ctx, tx, Params{TenantID: f.tenantID, OperationClass: OperationCatalogueAvailability, RequestedByActorType: ActorSystem})
		return err
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Outcome() != Resolved {
		t.Fatalf("expected Resolved, got %s(%s)", res.Outcome(), res.Reason())
	}
	code, err := res.Code()
	if err != nil {
		t.Fatalf("Code(): %v", err)
	}
	if want := "TJ-" + f.jurisdictionID.String()[:8]; code != want {
		t.Fatalf("expected code %q, got %q", want, code)
	}
	id, err := res.ID()
	if err != nil || id != f.jurisdictionID {
		t.Fatalf("expected id %s, got %s (err %v)", f.jurisdictionID, id, err)
	}
	if res.SelectedBasis() != BasisTenantLicence {
		t.Fatalf("expected basis tenant_licence, got %q", res.SelectedBasis())
	}
	if res.ConfidenceClass() != ConfidenceAuthoritative {
		t.Fatalf("expected confidence authoritative, got %q", res.ConfidenceClass())
	}
}

func TestResolveTenantLicence_NoLicenceConfiguredIsUnresolvedNoSignal(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var res Resolution
	err := pool.WithTenant(context.Background(), f.otherTenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = Resolve(ctx, tx, Params{TenantID: f.otherTenantID, OperationClass: OperationCatalogueAvailability, RequestedByActorType: ActorSystem})
		return err
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if res.Outcome() != Unresolved || res.Reason() != ReasonNoSignal {
		t.Fatalf("expected unresolved(no_signal), got %s(%s)", res.Outcome(), res.Reason())
	}
}

// TestResolveTenantLicence_CrossTenantRequestIsRefused is the integration-
// level proof of canonical-model §4.4 Layer 2's resolver-side scope
// assertion (`architect`, Stage 4I final cross-domain certification).
//
// This is a genuine cross-tenant isolation test, not a formality, and it
// is deliberately written against the REAL database rather than a fake
// querier: `tenants`, `licences` and `jurisdictions` all carry NO
// row-level security (verified directly: pg_class.relrowsecurity is false
// for all three), so RLS provides zero isolation on the resolver's only
// producible basis. Before the assertion existed, the call below returned
// a fully Resolved Resolution carrying tenant A's jurisdiction to a
// transaction that had only ever proven tenant B - an actual cross-tenant
// disclosure reachable by any caller that sourced Params.TenantID from
// anything other than tenant.FromContext.
func TestResolveTenantLicence_CrossTenantRequestIsRefused(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	// Connection proves otherTenantID; the caller asks about tenantID,
	// which is the tenant that actually HAS a licence and a resolvable
	// jurisdiction - so a missing assertion would visibly leak it.
	var res Resolution
	err := pool.WithTenant(context.Background(), f.otherTenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = Resolve(ctx, tx, Params{TenantID: f.tenantID, OperationClass: OperationCatalogueAvailability, RequestedByActorType: ActorSystem})
		return err
	})
	if err != nil {
		t.Fatalf("a scope mismatch is a REFUSED outcome, not a Go error: %v", err)
	}
	if res.Outcome() != Refused || res.Reason() != ReasonScopeMismatch {
		t.Fatalf("expected refused(scope_mismatch) for a cross-tenant resolve, got %s(%s) - tenant isolation on the resolver's tenant_licence basis is NOT provided by RLS", res.Outcome(), res.Reason())
	}
	if _, err := res.Code(); err == nil {
		t.Fatal("Code() must be unreachable for a Refused outcome - a cross-tenant request must not be able to read another tenant's jurisdiction code")
	}

	// Anti-inertness control: the SAME call, from the correctly-scoped
	// connection, still resolves. Without this, the assertion above would
	// also pass if Resolve had simply stopped working.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = Resolve(ctx, tx, Params{TenantID: f.tenantID, OperationClass: OperationCatalogueAvailability, RequestedByActorType: ActorSystem})
		return err
	})
	if err != nil {
		t.Fatalf("Resolve (in-scope): %v", err)
	}
	if res.Outcome() != Resolved {
		t.Fatalf("expected the in-scope resolve to still succeed, got %s(%s)", res.Outcome(), res.Reason())
	}
}

// TestResolveTenantLicence_UnscopedConnectionErrors proves canonical-model
// §6.1's "it must error, not return unresolved" requirement holds against
// the real database for a platform-scoped (WithoutTenant) connection. The
// canonical model justified that requirement by FORCE RLS returning zero
// rows; on this path there is no RLS at all, so the explicit assertion is
// the only thing that makes the required behaviour true.
func TestResolveTenantLicence_UnscopedConnectionErrors(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var res Resolution
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = Resolve(ctx, tx, Params{TenantID: f.tenantID, OperationClass: OperationCatalogueAvailability, RequestedByActorType: ActorSystem})
		return err
	})
	if err == nil {
		t.Fatal("expected a genuine error on a connection with no app.tenant_id - never a silent unresolved, and never a Resolved answer")
	}
	if !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("expected ErrScopeMismatch, got %v", err)
	}
	if res.Outcome() == Resolved {
		t.Fatal("an unscoped connection must never produce a Resolved outcome")
	}
}

// --- B-2: Persist + jurisdiction_resolutions RLS/append-only contract ---

func TestPersist_ResolvedAndUnresolvedBothWriteARow(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	for _, oc := range []OperationClass{OperationCatalogueAvailability, OperationPlay} {
		var persisted Resolution
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			p := Params{TenantID: f.tenantID, OperationClass: oc, RequestedByActorType: ActorSystem}
			if oc == OperationPlay {
				p.PlayerAccountID = &f.playerID
			}
			res, err := Resolve(ctx, tx, p)
			if err != nil {
				return err
			}
			persisted, err = Persist(ctx, tx, res)
			return err
		})
		if err != nil {
			t.Fatalf("%s: %v", oc, err)
		}
		if persisted.RecordID() == uuid.Nil {
			t.Fatalf("%s: expected a non-nil RecordID after Persist", oc)
		}
	}

	// Both a resolved (catalogue_availability, tenant_licence) and an
	// unresolved (play, no_signal) row must exist - "including failures"
	// (canonical-model §5.1).
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM jurisdiction_resolutions WHERE tenant_id = $1`, f.tenantID).Scan(&count); err != nil {
			return err
		}
		if count != 2 {
			t.Fatalf("expected 2 persisted rows, got %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
}

func TestJurisdictionResolutions_CrossTenantReadReturnsZeroRows(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		res, err := Resolve(ctx, tx, Params{TenantID: f.tenantID, OperationClass: OperationCatalogueAvailability, RequestedByActorType: ActorSystem})
		if err != nil {
			return err
		}
		_, err = Persist(ctx, tx, res)
		return err
	})
	if err != nil {
		t.Fatalf("seed row for tenant A: %v", err)
	}

	// Tenant B's own connection must see ZERO rows of tenant A's - not an
	// error, not a mismatched-but-visible row.
	err = pool.WithTenant(context.Background(), f.otherTenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM jurisdiction_resolutions`).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("tenant B must see 0 rows of tenant A's jurisdiction_resolutions, saw %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cross-tenant read check: %v", err)
	}
}

func TestJurisdictionResolutions_PlayerScopedReadReturnsZeroRows(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		res, err := Resolve(ctx, tx, Params{TenantID: f.tenantID, PlayerAccountID: &f.playerID, OperationClass: OperationPlay, RequestedByActorType: ActorSystem})
		if err != nil {
			return err
		}
		_, err = Persist(ctx, tx, res)
		return err
	})
	if err != nil {
		t.Fatalf("seed row: %v", err)
	}

	// No player-read policy exists at all (canonical-model §6.1/§6.2
	// scenario 3) - a player-scoped connection must see ZERO rows, even
	// for its OWN resolution.
	err = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM jurisdiction_resolutions`).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("a player-scoped connection must see 0 rows of jurisdiction_resolutions, saw %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("player-scoped read check: %v", err)
	}
}

func TestJurisdictionResolutions_WithoutTenantWriteIsRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO jurisdiction_resolutions (tenant_id, operation_class, requested_by_actor_type, outcome, reason, resolver_policy_version, as_of)
			VALUES ($1, 'play', 'system', 'unresolved', 'no_signal', 'test', now())`, f.tenantID)
		return err
	})
	if err == nil {
		t.Fatal("expected a platform-scoped (no app.tenant_id) connection to be rejected by RLS - there is no platform-admin bypass for this table")
	}
	assertPgCode(t, err, pgRLSViolation)
}

// TestJurisdictionResolutions_AppendOnly_NoRLSPolicyMakesUpdateAndDeleteNoOps
// proves the append-only guarantee holds through the ONLY path an
// ordinary (non-superuser, NOBYPASSRLS) connection has: migration 0071
// deliberately defines no UPDATE and no DELETE policy on this table
// (canonical-model §6.1), so under FORCE ROW LEVEL SECURITY those
// commands match zero rows and succeed as no-ops rather than raising -
// this is a DIFFERENT, but equally load-bearing, half of the same
// guarantee the BEFORE UPDATE OR DELETE trigger backstops (the trigger
// exists for the case RLS itself cannot cover: a role with BYPASSRLS or
// a future policy mistakenly added for UPDATE/DELETE - this project's
// own connection deliberately has neither, so the trigger is not
// reachable from this test, exactly as intended).
func TestJurisdictionResolutions_AppendOnly_NoRLSPolicyMakesUpdateAndDeleteNoOps(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var recordID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		res, err := Resolve(ctx, tx, Params{TenantID: f.tenantID, OperationClass: OperationCatalogueAvailability, RequestedByActorType: ActorSystem})
		if err != nil {
			return err
		}
		persisted, err := Persist(ctx, tx, res)
		recordID = persisted.RecordID()
		return err
	})
	if err != nil {
		t.Fatalf("seed row: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE jurisdiction_resolutions SET reason = 'determined' WHERE id = $1`, recordID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("expected the UPDATE to affect 0 rows (no UPDATE policy grants visibility), affected %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("UPDATE attempt: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM jurisdiction_resolutions WHERE id = $1`, recordID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("expected the DELETE to affect 0 rows (no DELETE policy grants visibility), affected %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("DELETE attempt: %v", err)
	}

	// Confirm the row is genuinely untouched.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var reason string
		if err := tx.QueryRow(ctx, `SELECT reason FROM jurisdiction_resolutions WHERE id = $1`, recordID).Scan(&reason); err != nil {
			return err
		}
		if reason != string(ReasonDetermined) {
			t.Fatalf("expected the row to be untouched (reason=%q), got %q", ReasonDetermined, reason)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify untouched: %v", err)
	}
}

func TestJurisdictionResolutions_TruncateRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE jurisdiction_resolutions`)
		return err
	})
	if err == nil {
		t.Fatal("expected TRUNCATE to be rejected on an append-only table")
	}
	assertPgCode(t, err, pgRaisedError)
}

func TestJurisdictionResolutions_CheckConstraint_PlayerScopedTenantLicenceRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO jurisdiction_resolutions (
				tenant_id, player_account_id, operation_class, requested_by_actor_type,
				outcome, reason, jurisdiction_code, selected_basis, resolver_policy_version, as_of
			) VALUES ($1, $2, 'play', 'system', 'resolved', 'determined', $3, 'tenant_licence', 'test', now())`,
			f.tenantID, f.playerID, "TJ-"+f.jurisdictionID.String()[:8])
		return err
	})
	if err == nil {
		t.Fatal("expected the database to reject a player-scoped row carrying basis=tenant_licence - this is HDR-J-1 answered 'yes' by accident")
	}
	assertPgCode(t, err, pgCheckViolation)
}

func TestJurisdictionResolutions_CheckConstraint_ResolvedRequiresCode(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO jurisdiction_resolutions (tenant_id, operation_class, requested_by_actor_type, outcome, reason, resolver_policy_version, as_of)
			VALUES ($1, 'play', 'system', 'resolved', 'determined', 'test', now())`, f.tenantID)
		return err
	})
	if err == nil {
		t.Fatal("expected the database to reject outcome=resolved with no jurisdiction_code")
	}
	assertPgCode(t, err, pgCheckViolation)
}

// --- B-6: jurisdiction_resolution_active ---

func TestSetResolutionActive_RoundTripAndIsActiveAccessor(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	staffID := uuid.New()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetResolutionActive(ctx, tx, SetResolutionActiveParams{
			TenantID: f.tenantID, OperationClass: OperationPlay, Active: true, ActorType: ActorStaff, ActorID: staffID, ReasonCode: "stage-4i-test",
		})
		return err
	})
	if err != nil {
		t.Fatalf("SetResolutionActive: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		active, err := IsActive(ctx, tx, f.tenantID, OperationPlay)
		if err != nil {
			return err
		}
		if !active {
			t.Fatal("expected IsActive to report true after SetResolutionActive(true)")
		}
		notSet, err := IsActive(ctx, tx, f.tenantID, OperationBonusIssuance)
		if err != nil {
			return err
		}
		if notSet {
			t.Fatal("expected IsActive to fail closed (false) for a pair with no row at all")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("IsActive: %v", err)
	}

	// An audit_log entry must exist for the change (canonical-model §5.1).
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'jurisdiction_resolution_active.changed'`,
			f.tenantID).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			t.Fatalf("expected exactly 1 audit_log row, got %d", count)
		}
		// CLAUDE.md's audit rule names reason code explicitly, and
		// PermAssetAuthorizationWrite's own tenant-scoped writes (the
		// precedent canonical-model §4.2 tells B-6 to follow) require and
		// record one on every write.
		var reasonCode string
		if err := tx.QueryRow(ctx,
			`SELECT metadata ->> 'reason_code' FROM audit_log WHERE tenant_id = $1 AND action = 'jurisdiction_resolution_active.changed'`,
			f.tenantID).Scan(&reasonCode); err != nil {
			return err
		}
		if reasonCode != "stage-4i-test" {
			t.Fatalf("expected the audit record to carry the caller's reason_code, got %q", reasonCode)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("audit check: %v", err)
	}

	// A write with no reason code is rejected outright - a record that
	// says what changed but never why is the gap CLAUDE.md's audit rule
	// names.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetResolutionActive(ctx, tx, SetResolutionActiveParams{
			TenantID: f.tenantID, OperationClass: OperationBonusIssuance, Active: true,
			ActorType: ActorStaff, ActorID: staffID,
		})
		if !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput for a missing reason_code, got %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("missing-reason-code check: %v", err)
	}
}

func TestJurisdictionResolutionActive_CrossTenantIsolation(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	staffID := uuid.New()

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetResolutionActive(ctx, tx, SetResolutionActiveParams{
			TenantID: f.tenantID, OperationClass: OperationPlay, Active: true, ActorType: ActorStaff, ActorID: staffID, ReasonCode: "stage-4i-test",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.otherTenantID, func(ctx context.Context, tx pgx.Tx) error {
		active, err := IsActive(ctx, tx, f.tenantID, OperationPlay)
		if err != nil {
			return err
		}
		if active {
			t.Fatal("tenant B's connection must not be able to observe tenant A's resolution-active fact as true")
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM jurisdiction_resolution_active`).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("tenant B must see 0 rows of tenant A's jurisdiction_resolution_active, saw %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cross-tenant isolation check: %v", err)
	}
}

// SEC-4I-F4 regression guard (migration 0072). The canonical model §6.1 /
// security model §S-3.1 are binding on jurisdiction_resolution_active as
// well as jurisdiction_resolutions: "Per-command policies. No `FOR ALL`
// policy. No DELETE policy." Migration 0071 shipped a single FOR ALL
// policy, which silently granted DELETE to every tenant-scoped
// transaction in the platform.
//
// This is not a style point. There is no application DELETE path for this
// table; the absence of a DELETE policy is the backstop for that fact.
// A DELETE is not equivalent to SetResolutionActive(active=false): the
// latter writes an audited before/after `audit_log` row in the same
// transaction, the former writes nothing - so a DELETE is an UNAUDITED
// change to a control's state, and leaves audit_log permanently
// disagreeing with the table.
//
// Pre-0072 this test fails with "affected 1".
func TestJurisdictionResolutionActive_DeleteIsDeniedByRLS(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	staffID := uuid.New()

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetResolutionActive(ctx, tx, SetResolutionActiveParams{
			TenantID: f.tenantID, OperationClass: OperationPlay, Active: true, ActorType: ActorStaff, ActorID: staffID, ReasonCode: "stage-4i-test",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// The tenant's OWN connection - the strongest scope any application
	// code in this platform ever holds for this table - must still not be
	// able to delete the row.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM jurisdiction_resolution_active WHERE tenant_id = $1 AND operation_class = $2`,
			f.tenantID, string(OperationPlay))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("expected the DELETE to affect 0 rows (no DELETE policy must exist on jurisdiction_resolution_active), affected %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("DELETE attempt: %v", err)
	}

	// The fact is still there and still true.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		active, err := IsActive(ctx, tx, f.tenantID, OperationPlay)
		if err != nil {
			return err
		}
		if !active {
			t.Fatal("expected the resolution-active fact to survive the DELETE attempt")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("post-delete IsActive: %v", err)
	}

	// And the policy set itself is per-command with no DELETE entry -
	// asserted directly so a future FOR ALL policy is caught even if some
	// other mechanism happened to make the DELETE above a no-op.
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT cmd FROM pg_policies WHERE tablename = 'jurisdiction_resolution_active' ORDER BY cmd`)
		if err != nil {
			return err
		}
		defer rows.Close()
		var cmds []string
		for rows.Next() {
			var cmd string
			if err := rows.Scan(&cmd); err != nil {
				return err
			}
			cmds = append(cmds, cmd)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		want := map[string]bool{"SELECT": true, "INSERT": true, "UPDATE": true}
		if len(cmds) != len(want) {
			t.Fatalf("expected exactly SELECT/INSERT/UPDATE policies on jurisdiction_resolution_active, got %v", cmds)
		}
		for _, cmd := range cmds {
			if !want[cmd] {
				t.Fatalf("unexpected policy command %q on jurisdiction_resolution_active (ALL and DELETE are both forbidden), full set %v", cmd, cmds)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("policy shape check: %v", err)
	}
}

// TestJurisdictionResolutionActive_TruncateIsDenied is SEC-4I-F8
// (`security`, Stage 4I final independent security/compliance
// certification) and the regression guard for migration 0073.
//
// It is the companion to the DELETE case immediately above, and it
// exists because PostgreSQL row-level security **does not apply to
// TRUNCATE at all**. TRUNCATE is governed only by the TRUNCATE
// privilege, which the application role holds implicitly by OWNING the
// table (deploy/init-app-role.sql makes it NOSUPERUSER/NOBYPASSRLS, but
// it still owns the database and schema). So every control migrations
// 0071 and 0072 placed on this table -- the tenant predicate, the
// player-scope exclusion, and SEC-4I-F4's deliberate absence of a DELETE
// policy -- is bypassed by one statement from an ordinary tenant-scoped
// connection.
//
// canonical-model §6.1 is binding on this table by its own opening line
// and its per-command-policy bullet ends "Plus a `BEFORE TRUNCATE ...
// FOR EACH STATEMENT` deny trigger." Migration 0071 gave that trigger to
// jurisdiction_resolutions only.
//
// Pre-0073 this test fails: the TRUNCATE succeeds and every tenant's
// resolution-active fact is gone, with audit_log still recording that
// each one was set -- an unaudited, platform-wide control-state change,
// which is exactly the hazard SEC-4I-F4 closed for DELETE.
func TestJurisdictionResolutionActive_TruncateIsDenied(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	staffID := uuid.New()

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetResolutionActive(ctx, tx, SetResolutionActiveParams{
			TenantID: f.tenantID, OperationClass: OperationBonusIssuance, Active: true,
			ActorType: ActorStaff, ActorID: staffID, ReasonCode: "stage-4i-sec-f8",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// A tenant-scoped connection -- the ordinary shape every admin handler
	// in this platform runs under.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE jurisdiction_resolution_active`)
		return err
	})
	if err == nil {
		t.Fatal("expected TRUNCATE to be rejected on jurisdiction_resolution_active - RLS does not cover TRUNCATE, so only a BEFORE TRUNCATE trigger can deny it")
	}
	assertPgCode(t, err, pgRaisedError)

	// A player-scoped connection, which the RLS predicates exclude from
	// this table entirely, must equally not be able to erase it.
	err = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE jurisdiction_resolution_active`)
		return err
	})
	if err == nil {
		t.Fatal("expected TRUNCATE to be rejected from a player-scoped connection too")
	}
	assertPgCode(t, err, pgRaisedError)

	// Anti-inertness: the fact is still there and still true, so the two
	// assertions above cannot pass merely because the row never existed.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		active, err := IsActive(ctx, tx, f.tenantID, OperationBonusIssuance)
		if err != nil {
			return err
		}
		if !active {
			t.Fatal("expected the resolution-active fact to survive both TRUNCATE attempts")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("post-truncate IsActive: %v", err)
	}
}

// --- B-1: jurisdictions/licences registry write surface ---

func TestCreateJurisdictionAndListJurisdictions(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	code := "TEST-" + uuid.NewString()[:8]
	var created Jurisdiction
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		created, err = CreateJurisdiction(ctx, tx, CreateJurisdictionParams{
			Code: code, Name: "Test Created Jurisdiction",
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "test-setup"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("CreateJurisdiction: %v", err)
	}
	if created.Code != code {
		t.Fatalf("expected code %q, got %q", code, created.Code)
	}

	var list []Jurisdiction
	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		list, err = ListJurisdictions(ctx, tx)
		return err
	})
	if err != nil {
		t.Fatalf("ListJurisdictions: %v", err)
	}
	found := false
	for _, j := range list {
		if j.Code == code {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("created jurisdiction %q not found in ListJurisdictions", code)
	}

	// An audit_log entry must exist (canonical-model §5.1's fourth event
	// class, "jurisdiction_registry.*").
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id IS NULL AND action = 'jurisdiction_registry.jurisdiction_created' AND target_id = $1`,
			created.ID.String()).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			t.Fatalf("expected exactly 1 audit_log row, got %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("audit check: %v", err)
	}
}

func TestCreateLicence_UnknownJurisdictionIsRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateLicence(ctx, tx, CreateLicenceParams{
			JurisdictionID: uuid.New(), Licensee: "platform", LicenceNumber: "LIC-UNKNOWN",
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "test"},
		})
		return err
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for an unknown jurisdiction_id, got %v", err)
	}
}

func TestCreateJurisdiction_DuplicateCodeIsRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateJurisdiction(ctx, tx, CreateJurisdictionParams{
			Code: "TJ-" + f.jurisdictionID.String()[:8], Name: "Duplicate",
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "test"},
		})
		return err
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for a duplicate code, got %v", err)
	}
}
