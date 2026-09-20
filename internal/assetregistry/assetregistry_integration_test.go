//go:build integration

// Real-PostgreSQL tests for migrations 0044/0045 and this package:
// fail-closed defaults, the platform-admin RLS backstop (Stage 4H-B0-R5
// finding S-3), identity immutability, four-eyes dual control (S-5a), and
// CheckEligibility's per-layer reason codes including the product
// dimension. Follows internal/risk/risk_integration_test.go's fixture
// conventions.
package assetregistry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
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

const (
	pgRLSViolation     = "42501"
	pgRaisedException  = "P0001"
	pgUniqueViolation  = "23505"
	pgNotNullViolation = "23502"
)

func assertPgErrorCode(t *testing.T, err error, code string) *pgconn.PgError {
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
	brandID        uuid.UUID
	otherBrandID   uuid.UUID
	jurisdictionID uuid.UUID
	playerID       uuid.UUID
}

func seedTenantFixture(t *testing.T, pool *db.Pool) fixture {
	t.Helper()
	var f fixture
	f.tenantID = uuid.New()
	f.jurisdictionID = uuid.New()
	// Stage 4I Phase E-SECURITY (migration 0077): `tenants`/`jurisdictions`
	// writes now require a genuinely platform-admin-scoped transaction.
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
		tag, err = tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'Test Jurisdiction')`,
			f.jurisdictionID, "TJ-"+f.jurisdictionID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 jurisdiction row, inserted %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed tenant/jurisdiction: %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		f.brandID = uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'Brand A')`,
			f.brandID, f.tenantID, "b-"+f.brandID.String()[:8]); err != nil {
			return err
		}
		f.otherBrandID = uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'Brand B')`,
			f.otherBrandID, f.tenantID, "b-"+f.otherBrandID.String()[:8]); err != nil {
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
		t.Fatalf("seed brands/player: %v", err)
	}
	return f
}

// seedPlatformAdmin creates a platform-scoped staff principal that is
// ALWAYS linked to a Person: passing nil means "link a fresh, unrelated
// Person", not "leave person_id NULL".
//
// That default changed with migration 0047. Before it, the same-person
// half of the four-eyes guard only compared person ids when BOTH were
// non-NULL, so an unlinked platform admin sailed through - which is
// exactly the defect security exploited (and, because no production code
// path can set person_id on a platform_admin account, the ONLY state a
// real deployment had). Migration 0047 refuses an unlinked principal on
// either side, so a test that wants two genuinely distinct approvers must
// link both. Auto-linking here, once, keeps every existing test in this
// file exercising the legitimate path without editing each one -
// mirroring internal/httpserver's mustCreateStaff, which made the same
// change for the same reason at Stage 3D.
//
// A test that specifically needs an UNLINKED or a non-active platform
// admin - i.e. one proving migration 0047's refusals - uses
// seedPlatformAdminRaw instead, never this one.
func seedPlatformAdmin(t *testing.T, pool *db.Pool, personID *uuid.UUID) uuid.UUID {
	t.Helper()
	if personID == nil {
		p := seedPerson(t, pool)
		personID = &p
	}
	return seedPlatformAdminRaw(t, pool, personID, "active")
}

// seedPlatformAdminRaw inserts a platform-scoped staff row EXACTLY as
// asked - including person_id NULL and a non-active status. It exists so
// the migration-0047 regression tests can reproduce the pre-fix state
// faithfully rather than approximating it.
func seedPlatformAdminRaw(t *testing.T, pool *db.Pool, personID *uuid.UUID, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id, status)
			 VALUES ($1, NULL, $2, 'x', 'platform_admin', $3, $4)`,
			id, id.String()+"@platform.example.com", personID, status)
		return err
	})
	if err != nil {
		t.Fatalf("seed platform admin: %v", err)
	}
	return id
}

func seedTenantStaff(t *testing.T, pool *db.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role)
			 VALUES ($1, $2, $3, 'x', 'tenant_admin')`,
			id, tenantID, id.String()+"@tenant.example.com")
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant staff: %v", err)
	}
	return id
}

func seedPerson(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, id)
		return err
	})
	if err != nil {
		t.Fatalf("seed person: %v", err)
	}
	return id
}

// newAssetCode returns a unique code. Asset rows are never deletable (by
// design), so tests must not reuse codes across runs.
func newAssetCode() string {
	return "TT" + strings.ToUpper(uuid.New().String()[:8])
}

func actor(id uuid.UUID) ActorContext {
	return ActorContext{ActorID: id, ReasonCode: "test", RequestID: uuid.NewString()}
}

// fileAndApprove files a dual-controlled request as `requester` and
// approves it as `approver` - two DISTINCT platform principals, which is
// the whole point of the control.
func fileAndApprove(t *testing.T, pool *db.Pool, op ChangeOperation, code string,
	requester, approver uuid.UUID, assetType string, exponent int16, network string) {
	t.Helper()
	var reqID uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		req, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
			Operation: op, AssetCode: code, AssetType: assetType, DecimalExponent: exponent,
			Network: network, Actor: actor(requester),
		})
		reqID = req.ID
		return err
	})
	if err != nil {
		t.Fatalf("file %s request: %v", op, err)
	}
	err = pool.WithPlatformAdmin(context.Background(), approver, func(ctx context.Context, tx pgx.Tx) error {
		_, err := DecideChangeRequest(ctx, tx, DecideChangeRequestParams{
			RequestID: reqID, Approve: true, Actor: actor(approver),
		})
		return err
	})
	if err != nil {
		t.Fatalf("approve %s request: %v", op, err)
	}
}

// liveAsset walks the whole ADR 0037 §C.5.1 layer-1-3 path for a fresh
// asset: create (dual-controlled) -> activate (dual-controlled) ->
// platform-authorize (dual-controlled). Each step needs its OWN approved
// request; there is no shortcut, which is exactly what is being asserted
// by the fact this helper has to be this long.
func liveAsset(t *testing.T, pool *db.Pool, adminA, adminB uuid.UUID) string {
	t.Helper()
	code := newAssetCode()

	fileAndApprove(t, pool, ChangeCreate, code, adminA, adminB, AssetTypeFiat, 2, "")
	err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateAsset(ctx, tx, CreateAssetParams{
			Code: code, AssetType: AssetTypeFiat, DecimalExponent: 2, DisplayName: "Test Token",
			Actor: actor(adminA),
		})
		return err
	})
	if err != nil {
		t.Fatalf("create asset: %v", err)
	}

	fileAndApprove(t, pool, ChangeActivate, code, adminA, adminB, "", 0, "")
	err = pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetActive(ctx, tx, code, true, actor(adminA))
		return err
	})
	if err != nil {
		t.Fatalf("activate asset: %v", err)
	}

	fileAndApprove(t, pool, ChangePlatformAuthorize, code, adminA, adminB, "", 0, "")
	err = pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetPlatformAuthorized(ctx, tx, code, true, actor(adminA))
		return err
	})
	if err != nil {
		t.Fatalf("platform-authorize asset: %v", err)
	}
	return code
}

// fileAndApproveEligibility files and approves the four-eyes request
// migration 0047 requires before a PLATFORM-WIDE layer-7 default can be
// granted. product "" means every product, recorded in the approved
// payload as the explicit EveryProduct sentinel.
func fileAndApproveEligibility(t *testing.T, pool *db.Pool, code string, op Operation, product string,
	requester, approver uuid.UUID) {
	t.Helper()
	var reqID uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		req, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
			Operation: ChangePlatformOperationEligibility, AssetCode: code,
			EligibilityOperation: op, EligibilityProduct: product, Actor: actor(requester),
		})
		reqID = req.ID
		return err
	})
	if err != nil {
		t.Fatalf("file platform_operation_eligibility request: %v", err)
	}
	err = pool.WithPlatformAdmin(context.Background(), approver, func(ctx context.Context, tx pgx.Tx) error {
		_, err := DecideChangeRequest(ctx, tx, DecideChangeRequestParams{
			RequestID: reqID, Approve: true, Actor: actor(approver),
		})
		return err
	})
	if err != nil {
		t.Fatalf("approve platform_operation_eligibility request: %v", err)
	}
}

// authorizeFullChain grants layers 4-7 so CheckEligibility passes, for
// one product/operation pair.
func authorizeFullChain(t *testing.T, pool *db.Pool, f fixture, adminA uuid.UUID, code, product string, op Operation) {
	t.Helper()
	// Layer 7 platform default first: migration 0045's narrowing trigger
	// requires the platform row before a tenant override can exist.
	//
	// The GRANT is dual-controlled since migration 0047, so a second
	// platform principal is needed here. It is seeded locally rather than
	// taken as a parameter so every existing caller of this helper is
	// unchanged - the point being tested by those callers is the layer
	// chain, not the four-eyes flow (which has its own dedicated tests).
	approver := seedPlatformAdmin(t, pool, nil)
	fileAndApproveEligibility(t, pool, code, op, product, adminA, approver)

	err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ConfigureOperationEligibility(ctx, tx, ConfigureEligibilityParams{
			AssetCode: code, Product: product, Operation: op, Eligible: true, Actor: actor(adminA),
		})
		return err
	})
	if err != nil {
		t.Fatalf("configure platform eligibility: %v", err)
	}

	staffID := seedTenantStaff(t, pool, f.tenantID)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := AuthorizeScope(ctx, tx, AuthorizeScopeParams{
			TenantID: f.tenantID, ScopeKind: ScopeTenant, AssetCode: code, Product: product,
			Eligible: true, Actor: actor(staffID),
		}); err != nil {
			return err
		}
		_, err := AuthorizeScope(ctx, tx, AuthorizeScopeParams{
			TenantID: f.tenantID, ScopeKind: ScopeJurisdiction, JurisdictionID: f.jurisdictionID,
			AssetCode: code, Product: product, Eligible: true, Actor: actor(staffID),
		})
		return err
	})
	if err != nil {
		t.Fatalf("authorize tenant/jurisdiction: %v", err)
	}
}

func check(t *testing.T, pool *db.Pool, f fixture, brand uuid.UUID, jurisdiction uuid.UUID, code, product string, op Operation) (bool, ReasonCode, error) {
	t.Helper()
	var eligible bool
	var reason ReasonCode
	var checkErr error
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		eligible, reason, checkErr = AssetAuthorization{}.CheckEligibility(
			ctx, tx, f.tenantID, brand, jurisdiction, code, OperationScope{Product: product, Operation: op})
		return nil
	})
	if err != nil {
		t.Fatalf("transaction: %v", err)
	}
	return eligible, reason, checkErr
}

// --- S-4: fail-closed defaults ---

// The seven rows migration 0003 seeded are explicitly grandfathered
// active (they are referenced by live wallet/ledger schema) but
// explicitly NOT platform-authorized - the deliberate split migration
// 0044 documents.
func TestSeededAssets_ActiveGrandfatheredButNotPlatformAuthorized(t *testing.T) {
	pool := testPool(t)
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		for _, code := range []string{"EUR", "USD", "GBP", "BRL", "MXN", "BTC", "USDT"} {
			a, err := GetAsset(ctx, tx, code)
			if err != nil {
				return fmt.Errorf("%s: %w", code, err)
			}
			if !a.Active {
				return fmt.Errorf("%s: expected the seeded row to be explicitly active", code)
			}
			if a.PlatformAuthorized {
				return fmt.Errorf("%s: seeded rows must NOT be grandfathered platform-authorized", code)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The column default itself must be fail-closed, so any future insert
// path that omits the columns cannot fail open (finding S-4's exact
// wording).
func TestAssetsColumnDefaults_AreFailClosed(t *testing.T) {
	pool := testPool(t)
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var activeDefault, authorizedDefault *string
		if err := tx.QueryRow(ctx, `
			SELECT
			  (SELECT column_default FROM information_schema.columns
			    WHERE table_name = 'assets' AND column_name = 'active'),
			  (SELECT column_default FROM information_schema.columns
			    WHERE table_name = 'assets' AND column_name = 'platform_authorized')`).
			Scan(&activeDefault, &authorizedDefault); err != nil {
			return err
		}
		if activeDefault == nil || !strings.Contains(*activeDefault, "false") {
			return fmt.Errorf("assets.active default must be false, got %v", activeDefault)
		}
		if authorizedDefault == nil || !strings.Contains(*authorizedDefault, "false") {
			return fmt.Errorf("assets.platform_authorized default must be false, got %v", authorizedDefault)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// --- S-3: the database backstop for layers 1-3 ---

func TestRLS_TenantScopedConnectionCannotWriteAssets(t *testing.T) {
	pool := testPool(t)
	f := seedTenantFixture(t, pool)

	// INSERT from a tenant-scoped connection.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO assets (code, asset_type, decimal_exponent, display_name) VALUES ($1, 'fiat', 2, 'Forged')`,
			newAssetCode())
		return err
	})
	if err == nil {
		t.Fatal("a tenant-scoped connection must never be able to insert an asset row")
	}

	// UPDATE from a tenant-scoped connection: with no matching UPDATE
	// policy, RLS gives the row zero visibility, so the statement affects
	// 0 rows and the value is unchanged (it is NOT silently applied).
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE assets SET platform_authorized = true WHERE code = 'EUR'`)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			return fmt.Errorf("a tenant-scoped connection updated %d asset rows; expected 0", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		a, err := GetAsset(ctx, tx, "EUR")
		if err != nil {
			return err
		}
		if a.PlatformAuthorized {
			t.Fatal("EUR became platform-authorized from a tenant-scoped connection - the S-3 backstop failed")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A platform-admin GUC alone is not enough if the connection is also
// tenant-scoped: the policies require app.tenant_id to be UNSET, so a
// tenant-scoped code path cannot escalate by additionally setting the
// platform GUC.
func TestRLS_TenantScopedConnectionCannotEscalateByForgingThePlatformGUC(t *testing.T) {
	pool := testPool(t)
	f := seedTenantFixture(t, pool)
	adminA := seedPlatformAdmin(t, pool, nil)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, adminA.String()); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE assets SET platform_authorized = true WHERE code = 'USD'`)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			return fmt.Errorf("escalation succeeded: %d rows updated", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRLS_TenantScopedConnectionCannotSeeOrWriteChangeRequests(t *testing.T) {
	pool := testPool(t)
	f := seedTenantFixture(t, pool)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	code := newAssetCode()
	fileAndApprove(t, pool, ChangeCreate, code, adminA, adminB, AssetTypeFiat, 2, "")

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM asset_change_requests`).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("a tenant-scoped connection saw %d change requests; expected 0", count)
		}
		var approvals int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM asset_change_approvals`).Scan(&approvals); err != nil {
			return err
		}
		if approvals != 0 {
			return fmt.Errorf("a tenant-scoped connection saw %d approvals; expected 0", approvals)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// And it cannot forge one either. Two independent mechanisms refuse
	// this, and which one fires first depends on Postgres's own ordering
	// (BEFORE-INSERT triggers run before the RLS WITH CHECK): the
	// platform-principal trigger cannot resolve adminA at all from tenant
	// scope (staff_users' own dual_scope_isolation policy hides
	// platform-scoped staff rows), and the RLS policy would reject the
	// row regardless. Either refusal is correct; asserting on one
	// specific SQLSTATE would be asserting on trigger-vs-RLS ordering,
	// not on the security property.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO asset_change_requests (operation, asset_code, payload, reason_code, requested_by_principal_id)
			 VALUES ('create', $1, '{"asset_type":"fiat","decimal_exponent":2}', 'forged', $2)`,
			newAssetCode(), adminA)
		return err
	})
	if err == nil {
		t.Fatal("a tenant-scoped connection must never insert a change request")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || (pgErr.Code != pgRLSViolation && pgErr.Code != pgRaisedException) {
		t.Fatalf("expected an RLS or trigger refusal, got %v", err)
	}

	// The RLS half on its own, with no trigger involved: an UPDATE from
	// tenant scope has zero row visibility, so it affects nothing rather
	// than quietly cancelling a platform change request.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE asset_change_requests SET state = 'cancelled' WHERE asset_code = $1`, code)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			return fmt.Errorf("a tenant-scoped connection updated %d change requests; expected 0", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A tenant-scoped staff principal must not be able to appear on either
// side of a layer-1-3 mutation even from a correctly platform-scoped
// connection - the second, independent half of the S-3 backstop.
func TestDualControl_RequesterAndApproverMustBePlatformScopedStaff(t *testing.T) {
	pool := testPool(t)
	f := seedTenantFixture(t, pool)
	tenantStaff := seedTenantStaff(t, pool, f.tenantID)
	adminA := seedPlatformAdmin(t, pool, nil)
	code := newAssetCode()

	err := pool.WithPlatformAdmin(context.Background(), tenantStaff, func(ctx context.Context, tx pgx.Tx) error {
		_, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
			Operation: ChangeCreate, AssetCode: code, AssetType: AssetTypeFiat, DecimalExponent: 2,
			Actor: actor(tenantStaff),
		})
		return err
	})
	// Under staff_users' dual_scope_isolation policy a platform-scoped
	// transaction cannot see a tenant_id IS NOT NULL row at all, so the
	// tenant staff id resolves to nothing - which migration 0047
	// classifies as ErrApproverNotEligible ("cannot be resolved"),
	// alongside its person-linkage and status refusals. Migration 0044
	// reported the same condition as generic invalid input; the refusal
	// is identical, only the classification is now specific.
	if !errors.Is(err, ErrApproverNotEligible) {
		t.Fatalf("expected a tenant-scoped requester to be refused, got %v", err)
	}

	// Now a legitimate request, approved by a tenant-scoped principal.
	var reqID uuid.UUID
	err = pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		req, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
			Operation: ChangeCreate, AssetCode: code, AssetType: AssetTypeFiat, DecimalExponent: 2,
			Actor: actor(adminA),
		})
		reqID = req.ID
		return err
	})
	if err != nil {
		t.Fatalf("file request: %v", err)
	}
	err = pool.WithPlatformAdmin(context.Background(), tenantStaff, func(ctx context.Context, tx pgx.Tx) error {
		_, err := DecideChangeRequest(ctx, tx, DecideChangeRequestParams{
			RequestID: reqID, Approve: true, Actor: actor(tenantStaff),
		})
		return err
	})
	if !errors.Is(err, ErrApproverNotEligible) {
		t.Fatalf("expected a tenant-scoped approver to be refused, got %v", err)
	}
}

// --- S-5a: four-eyes is enforced, not documented ---

func TestDualControl_CreateWithNoApprovalIsRefused(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	code := newAssetCode()

	// No request at all.
	err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateAsset(ctx, tx, CreateAssetParams{
			Code: code, AssetType: AssetTypeFiat, DecimalExponent: 2, DisplayName: "X", Actor: actor(adminA),
		})
		return err
	})
	if !errors.Is(err, ErrDualControlRequired) {
		t.Fatalf("expected ErrDualControlRequired, got %v", err)
	}

	// A request with NO approval is equally insufficient - filing is not
	// approving.
	err = pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
			Operation: ChangeCreate, AssetCode: code, AssetType: AssetTypeFiat, DecimalExponent: 2,
			Actor: actor(adminA),
		}); err != nil {
			return err
		}
		_, err := CreateAsset(ctx, tx, CreateAssetParams{
			Code: code, AssetType: AssetTypeFiat, DecimalExponent: 2, DisplayName: "X", Actor: actor(adminA),
		})
		return err
	})
	if !errors.Is(err, ErrDualControlRequired) {
		t.Fatalf("expected ErrDualControlRequired for an unapproved request, got %v", err)
	}
}

func TestDualControl_SelfApprovalIsRejected(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	code := newAssetCode()

	var reqID uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		req, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
			Operation: ChangeCreate, AssetCode: code, AssetType: AssetTypeFiat, DecimalExponent: 2,
			Actor: actor(adminA),
		})
		reqID = req.ID
		return err
	})
	if err != nil {
		t.Fatalf("file request: %v", err)
	}
	err = pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := DecideChangeRequest(ctx, tx, DecideChangeRequestParams{
			RequestID: reqID, Approve: true, Actor: actor(adminA),
		})
		return err
	})
	if !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("expected ErrSelfApproval, got %v", err)
	}
}

// Two staff accounts held by ONE person are one human - migration 0029's
// withdrawal precedent, applied here.
func TestDualControl_SamePersonViaTwoStaffAccountsIsRejected(t *testing.T) {
	pool := testPool(t)
	personID := seedPerson(t, pool)
	adminA := seedPlatformAdmin(t, pool, &personID)
	adminAlt := seedPlatformAdmin(t, pool, &personID)
	code := newAssetCode()

	var reqID uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		req, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
			Operation: ChangeCreate, AssetCode: code, AssetType: AssetTypeFiat, DecimalExponent: 2,
			Actor: actor(adminA),
		})
		reqID = req.ID
		return err
	})
	if err != nil {
		t.Fatalf("file request: %v", err)
	}
	err = pool.WithPlatformAdmin(context.Background(), adminAlt, func(ctx context.Context, tx pgx.Tx) error {
		_, err := DecideChangeRequest(ctx, tx, DecideChangeRequestParams{
			RequestID: reqID, Approve: true, Actor: actor(adminAlt),
		})
		return err
	})
	if err == nil {
		t.Fatal("a second staff account belonging to the same person must not be able to approve")
	}
	if !strings.Contains(err.Error(), "same person") {
		t.Fatalf("expected the same-person guard to fire, got %v", err)
	}
}

func TestDualControl_OneApproverCannotCountTwice(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	code := newAssetCode()

	var reqID uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		req, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
			Operation: ChangeCreate, AssetCode: code, AssetType: AssetTypeFiat, DecimalExponent: 2,
			Actor: actor(adminA),
		})
		reqID = req.ID
		return err
	})
	if err != nil {
		t.Fatalf("file request: %v", err)
	}
	for i := 0; i < 2; i++ {
		err = pool.WithPlatformAdmin(context.Background(), adminB, func(ctx context.Context, tx pgx.Tx) error {
			_, err := DecideChangeRequest(ctx, tx, DecideChangeRequestParams{
				RequestID: reqID, Approve: true, Actor: actor(adminB),
			})
			return err
		})
		if i == 0 && err != nil {
			t.Fatalf("first approval failed: %v", err)
		}
		if i == 1 && err == nil {
			t.Fatal("the same approver decided the same request twice")
		}
	}
}

func TestDualControl_ApprovalsAreImmutable(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	code := newAssetCode()
	fileAndApprove(t, pool, ChangeCreate, code, adminA, adminB, AssetTypeFiat, 2, "")

	err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE asset_change_approvals SET decision = 'reject'`)
		return err
	})
	if err == nil {
		t.Fatal("approvals must be immutable")
	}
	assertPgErrorCode(t, err, pgRaisedException)

	err = pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM asset_change_approvals`)
		return err
	})
	if err == nil {
		t.Fatal("approvals must not be deletable")
	}
}

// The request's own payload must be immutable too: otherwise the control
// is bypassable by approving a harmless request and rewriting it (the
// exact bypass migration 0026's own immutability trigger describes).
func TestDualControl_RequestPayloadIsImmutable(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	code := newAssetCode()
	fileAndApprove(t, pool, ChangeCreate, code, adminA, adminB, AssetTypeFiat, 2, "")

	err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE asset_change_requests SET payload = '{"asset_type":"crypto","decimal_exponent":18}' WHERE asset_code = $1`, code)
		return err
	})
	if err == nil {
		t.Fatal("a change request's payload must be immutable after insert")
	}
	assertPgErrorCode(t, err, pgRaisedException)
}

// Approving "exponent 2" must not authorize inserting "exponent 8".
func TestDualControl_InsertMustMatchTheApprovedPayload(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	code := newAssetCode()
	fileAndApprove(t, pool, ChangeCreate, code, adminA, adminB, AssetTypeFiat, 2, "")

	err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateAsset(ctx, tx, CreateAssetParams{
			Code: code, AssetType: AssetTypeFiat, DecimalExponent: 8, DisplayName: "Mismatch", Actor: actor(adminA),
		})
		return err
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected the payload-mismatch refusal, got %v", err)
	}
}

// --- Activation bypass ---

// The full unilateral chain the dispatch names: create -> self-authorize
// -> activate -> use. Every link is refused.
func TestActivationBypass_CreateNeverAutoActivatesOrAuthorizes(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	f := seedTenantFixture(t, pool)
	code := newAssetCode()
	fileAndApprove(t, pool, ChangeCreate, code, adminA, adminB, AssetTypeFiat, 2, "")

	err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		created, err := CreateAsset(ctx, tx, CreateAssetParams{
			Code: code, AssetType: AssetTypeFiat, DecimalExponent: 2, DisplayName: "Fresh", Actor: actor(adminA),
		})
		if err != nil {
			return err
		}
		if created.Active || created.PlatformAuthorized {
			return fmt.Errorf("a created asset must be inactive and unauthorized, got active=%v authorized=%v",
				created.Active, created.PlatformAuthorized)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// And the same actor cannot immediately activate it - the second link
	// in the create -> self-authorize -> activate -> use chain. Run in its
	// own transaction because the refusal is a raised database exception,
	// which aborts the transaction it fires in.
	err = pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetActive(ctx, tx, code, true, actor(adminA))
		return err
	})
	if !errors.Is(err, ErrDualControlRequired) {
		t.Fatalf("expected activation to require its own approval, got %v", err)
	}
	err = pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetPlatformAuthorized(ctx, tx, code, true, actor(adminA))
		return err
	})
	if !errors.Is(err, ErrDualControlRequired) {
		t.Fatalf("expected platform authorization to require its own approval, got %v", err)
	}

	// Nor is it usable financially: every layer still denies.
	eligible, reason, _ := check(t, pool, f, uuid.Nil, f.jurisdictionID, code, "casino", OperationWagering)
	if eligible {
		t.Fatal("a freshly created asset must not be eligible for anything")
	}
	if reason != ReasonAssetInactive {
		t.Fatalf("expected asset_inactive, got %q", reason)
	}
}

// Suspension is deliberately single-actor (ADR 0037 §C.5.3): an incident
// kill-switch must not wait for a second approver.
func TestSuspend_IsSingleActorByDesign(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	code := liveAsset(t, pool, adminA, adminB)

	err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		a, err := SetActive(ctx, tx, code, false, actor(adminA))
		if err != nil {
			return err
		}
		if a.Active {
			return errors.New("suspension did not take effect")
		}
		b, err := SetPlatformAuthorized(ctx, tx, code, false, actor(adminA))
		if err != nil {
			return err
		}
		if b.PlatformAuthorized {
			return errors.New("revocation did not take effect")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("suspend/revoke must not require dual control: %v", err)
	}
}

// --- Identity immutability (ADR 0037 §C.5.4) ---

func TestIdentityFields_AreImmutableAtTheDatabase(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	code := liveAsset(t, pool, adminA, adminB)

	mutations := map[string]string{
		"code":             `UPDATE assets SET code = 'RENAMED' WHERE code = $1`,
		"decimal_exponent": `UPDATE assets SET decimal_exponent = 8 WHERE code = $1`,
		"asset_type":       `UPDATE assets SET asset_type = 'crypto' WHERE code = $1`,
		"network":          `UPDATE assets SET network = 'mainnet' WHERE code = $1`,
		"created_at":       `UPDATE assets SET created_at = now() - interval '1 day' WHERE code = $1`,
	}
	for field, sql := range mutations {
		err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, code)
			return err
		})
		if err == nil {
			t.Fatalf("%s must be immutable after creation", field)
		}
		pgErr := assertPgErrorCode(t, err, pgRaisedException)
		if !strings.Contains(pgErr.Message, "immutable") {
			t.Fatalf("%s: expected the immutability trigger, got %s", field, pgErr.Message)
		}
	}

	// Mutable metadata still works, and only it.
	err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		a, err := UpdateAssetMetadata(ctx, tx, code, "Renamed Display", actor(adminA))
		if err != nil {
			return err
		}
		if a.DisplayName != "Renamed Display" {
			return errors.New("display_name update did not take effect")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("display_name must remain mutable: %v", err)
	}
}

func TestAssets_CannotBeDeleted(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	code := liveAsset(t, pool, adminA, adminB)

	err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM assets WHERE code = $1`, code)
		return err
	})
	if err == nil {
		t.Fatal("an asset row must never be deletable - ledger history references it by code")
	}
	assertPgErrorCode(t, err, pgRaisedException)
}

// --- CheckEligibility: the layer chain ---

func TestCheckEligibility_FullChainAllowsAndEachLayerDeniesIndependently(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	f := seedTenantFixture(t, pool)
	code := liveAsset(t, pool, adminA, adminB)
	authorizeFullChain(t, pool, f, adminA, code, "casino", OperationWagering)

	eligible, reason, err := check(t, pool, f, f.brandID, f.jurisdictionID, code, "casino", OperationWagering)
	if err != nil || !eligible || reason != ReasonEligible {
		t.Fatalf("expected the full chain to allow, got eligible=%v reason=%q err=%v", eligible, reason, err)
	}

	staffID := seedTenantStaff(t, pool, f.tenantID)

	// Layer 6 denied on its OWN row - this is the `qa` finding: a
	// jurisdiction denial must be distinguishable from a tenant denial,
	// which is only possible because they are separate facts.
	setScope(t, pool, f, staffID, ScopeJurisdiction, f.jurisdictionID, uuid.Nil, code, "casino", false)
	if _, reason, _ := check(t, pool, f, f.brandID, f.jurisdictionID, code, "casino", OperationWagering); reason != ReasonJurisdictionNotAuthorized {
		t.Fatalf("expected jurisdiction_not_authorized, got %q", reason)
	}
	setScope(t, pool, f, staffID, ScopeJurisdiction, f.jurisdictionID, uuid.Nil, code, "casino", true)

	// Layer 5, its own row, and only when a brand is named.
	setScope(t, pool, f, staffID, ScopeBrand, uuid.Nil, f.brandID, code, "casino", false)
	if _, reason, _ := check(t, pool, f, f.brandID, f.jurisdictionID, code, "casino", OperationWagering); reason != ReasonBrandNotAuthorized {
		t.Fatalf("expected brand_not_authorized, got %q", reason)
	}
	// The SAME check with a different brand of the same tenant is
	// unaffected - a brand denial never leaks across brands.
	if eligible, reason, _ := check(t, pool, f, f.otherBrandID, f.jurisdictionID, code, "casino", OperationWagering); !eligible {
		t.Fatalf("a denial for brand A must not deny brand B, got reason=%q", reason)
	}
	// And a check with NO brand named still passes (layer 5 only narrows).
	if eligible, _, _ := check(t, pool, f, uuid.Nil, f.jurisdictionID, code, "casino", OperationWagering); !eligible {
		t.Fatal("a brand-specific denial must not apply to a non-brand-scoped check")
	}
	setScope(t, pool, f, staffID, ScopeBrand, uuid.Nil, f.brandID, code, "casino", true)

	// Layer 4, its own row.
	setScope(t, pool, f, staffID, ScopeTenant, uuid.Nil, uuid.Nil, code, "casino", false)
	if _, reason, _ := check(t, pool, f, f.brandID, f.jurisdictionID, code, "casino", OperationWagering); reason != ReasonTenantNotAuthorized {
		t.Fatalf("expected tenant_not_authorized, got %q", reason)
	}
	setScope(t, pool, f, staffID, ScopeTenant, uuid.Nil, uuid.Nil, code, "casino", true)

	// Layer 2 and 3, on the asset row itself.
	err = pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetPlatformAuthorized(ctx, tx, code, false, actor(adminA))
		return err
	})
	if err != nil {
		t.Fatalf("revoke platform authorization: %v", err)
	}
	if _, reason, _ := check(t, pool, f, f.brandID, f.jurisdictionID, code, "casino", OperationWagering); reason != ReasonAssetNotPlatformAuthorized {
		t.Fatalf("expected asset_not_platform_authorized, got %q", reason)
	}
	err = pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetActive(ctx, tx, code, false, actor(adminA))
		return err
	})
	if err != nil {
		t.Fatalf("suspend: %v", err)
	}
	// Layer 2 short-circuits before layer 3, per §A.3's order.
	if _, reason, _ := check(t, pool, f, f.brandID, f.jurisdictionID, code, "casino", OperationWagering); reason != ReasonAssetInactive {
		t.Fatalf("expected asset_inactive, got %q", reason)
	}

	// Layer 1.
	if _, reason, _ := check(t, pool, f, f.brandID, f.jurisdictionID, "NO-SUCH-ASSET", "casino", OperationWagering); reason != ReasonAssetNotFound {
		t.Fatalf("expected asset_not_found, got %q", reason)
	}
}

func setScope(t *testing.T, pool *db.Pool, f fixture, staffID uuid.UUID, kind ScopeKind,
	jurisdiction, brand uuid.UUID, code, product string, eligible bool) {
	t.Helper()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := AuthorizeScope(ctx, tx, AuthorizeScopeParams{
			TenantID: f.tenantID, ScopeKind: kind, BrandID: brand, JurisdictionID: jurisdiction,
			AssetCode: code, Product: product, Eligible: eligible, Actor: actor(staffID),
		})
		return err
	})
	if err != nil {
		t.Fatalf("set %s scope eligible=%v: %v", kind, eligible, err)
	}
}

// --- Layer 7 and the product dimension ---

// The regulatory pattern `sportsbook`'s review named: an asset
// wagering-eligible for casino must NOT be silently wagering-eligible for
// sportsbook.
func TestCheckEligibility_ProductBypassIsRefused(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	f := seedTenantFixture(t, pool)
	code := liveAsset(t, pool, adminA, adminB)
	authorizeFullChain(t, pool, f, adminA, code, "casino", OperationWagering)

	if eligible, _, _ := check(t, pool, f, uuid.Nil, f.jurisdictionID, code, "casino", OperationWagering); !eligible {
		t.Fatal("casino wagering should be eligible")
	}
	eligible, reason, _ := check(t, pool, f, uuid.Nil, f.jurisdictionID, code, "sportsbook", OperationWagering)
	if eligible {
		t.Fatal("casino wagering eligibility must never imply sportsbook wagering eligibility")
	}
	// It fails at the first layer that has no sportsbook fact - layer 4 -
	// which is itself the point: each layer is product-aware.
	if reason != ReasonTenantNotAuthorized {
		t.Fatalf("expected tenant_not_authorized for the unconfigured product, got %q", reason)
	}

	// Authorize layers 4-6 for sportsbook but NOT layer 7: the operation
	// gate is genuinely independent of the scope gates.
	staffID := seedTenantStaff(t, pool, f.tenantID)
	setScope(t, pool, f, staffID, ScopeTenant, uuid.Nil, uuid.Nil, code, "sportsbook", true)
	setScope(t, pool, f, staffID, ScopeJurisdiction, f.jurisdictionID, uuid.Nil, code, "sportsbook", true)
	if _, reason, _ := check(t, pool, f, uuid.Nil, f.jurisdictionID, code, "sportsbook", OperationWagering); reason != ReasonOperationNotEligible {
		t.Fatalf("expected operation_not_eligible for sportsbook, got %q", reason)
	}
}

// An operation gate is per-operation as well as per-product: wagering
// eligibility must not imply withdrawal eligibility (ADR 0037 §A.2's
// worked example).
func TestCheckEligibility_OperationBypassIsRefused(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	f := seedTenantFixture(t, pool)
	code := liveAsset(t, pool, adminA, adminB)
	authorizeFullChain(t, pool, f, adminA, code, "casino", OperationWagering)

	if _, reason, _ := check(t, pool, f, uuid.Nil, f.jurisdictionID, code, "casino", OperationWithdrawal); reason != ReasonOperationNotEligible {
		t.Fatalf("expected operation_not_eligible for withdrawal, got %q", reason)
	}
}

func TestCheckEligibility_TenantOverrideNarrowsLayer7(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	f := seedTenantFixture(t, pool)
	code := liveAsset(t, pool, adminA, adminB)
	authorizeFullChain(t, pool, f, adminA, code, "casino", OperationWagering)

	staffID := seedTenantStaff(t, pool, f.tenantID)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ConfigureOperationEligibility(ctx, tx, ConfigureEligibilityParams{
			TenantID: f.tenantID, AssetCode: code, Product: "casino", Operation: OperationWagering,
			Eligible: false, Actor: actor(staffID),
		})
		return err
	})
	if err != nil {
		t.Fatalf("tenant narrowing override: %v", err)
	}
	_, reason, _ := check(t, pool, f, uuid.Nil, f.jurisdictionID, code, "casino", OperationWagering)
	if reason != ReasonOperationNotEligibleForTenant {
		t.Fatalf("expected operation_not_eligible_for_tenant, got %q", reason)
	}
}

// --- Narrow-only-never-widen, at write time ---

func TestNarrowing_TenantOverrideCannotWidenPastPlatformDenial(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	f := seedTenantFixture(t, pool)
	code := liveAsset(t, pool, adminA, adminB)

	// Platform default: NOT eligible.
	err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ConfigureOperationEligibility(ctx, tx, ConfigureEligibilityParams{
			AssetCode: code, Product: "casino", Operation: OperationWithdrawal, Eligible: false, Actor: actor(adminA),
		})
		return err
	})
	if err != nil {
		t.Fatalf("platform default: %v", err)
	}

	staffID := seedTenantStaff(t, pool, f.tenantID)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ConfigureOperationEligibility(ctx, tx, ConfigureEligibilityParams{
			TenantID: f.tenantID, AssetCode: code, Product: "casino", Operation: OperationWithdrawal,
			Eligible: true, Actor: actor(staffID),
		})
		return err
	})
	if !errors.Is(err, ErrWidensPlatformAuthorization) {
		t.Fatalf("expected the widening write to be REJECTED at write time, got %v", err)
	}
}

func TestNarrowing_AuthorizationRequiresPlatformAuthorizedAsset(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	f := seedTenantFixture(t, pool)
	code := newAssetCode()
	fileAndApprove(t, pool, ChangeCreate, code, adminA, adminB, AssetTypeFiat, 2, "")
	err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateAsset(ctx, tx, CreateAssetParams{
			Code: code, AssetType: AssetTypeFiat, DecimalExponent: 2, DisplayName: "Unauthorized", Actor: actor(adminA),
		})
		return err
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	staffID := seedTenantStaff(t, pool, f.tenantID)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := AuthorizeScope(ctx, tx, AuthorizeScopeParams{
			TenantID: f.tenantID, ScopeKind: ScopeTenant, AssetCode: code, Product: "casino",
			Eligible: true, Actor: actor(staffID),
		})
		return err
	})
	if !errors.Is(err, ErrWidensPlatformAuthorization) {
		t.Fatalf("expected a tenant authorization for a non-platform-authorized asset to be rejected, got %v", err)
	}
}

func TestNarrowing_BrandGrantCannotExceedTenantGrant(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	f := seedTenantFixture(t, pool)
	code := liveAsset(t, pool, adminA, adminB)
	staffID := seedTenantStaff(t, pool, f.tenantID)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := AuthorizeScope(ctx, tx, AuthorizeScopeParams{
			TenantID: f.tenantID, ScopeKind: ScopeBrand, BrandID: f.brandID, AssetCode: code,
			Product: "casino", Eligible: true, Actor: actor(staffID),
		})
		return err
	})
	if !errors.Is(err, ErrWidensPlatformAuthorization) {
		t.Fatalf("expected a brand grant with no tenant grant behind it to be rejected, got %v", err)
	}
}

// --- Cross-tenant / cross-brand isolation ---

func TestIsolation_CrossTenantAuthorizationIsInvisibleAndUnusable(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	fA := seedTenantFixture(t, pool)
	fB := seedTenantFixture(t, pool)
	code := liveAsset(t, pool, adminA, adminB)
	authorizeFullChain(t, pool, fA, adminA, code, "casino", OperationWagering)

	// Tenant A passes.
	if eligible, _, _ := check(t, pool, fA, uuid.Nil, fA.jurisdictionID, code, "casino", OperationWagering); !eligible {
		t.Fatal("tenant A should be eligible")
	}
	// Tenant B, same asset, same product, same operation, its OWN
	// jurisdiction: denied, because tenant A's rows are invisible to it.
	eligible, reason, _ := check(t, pool, fB, uuid.Nil, fB.jurisdictionID, code, "casino", OperationWagering)
	if eligible {
		t.Fatal("tenant A's authorization must never authorize tenant B")
	}
	if reason != ReasonTenantNotAuthorized {
		t.Fatalf("expected tenant_not_authorized for tenant B, got %q", reason)
	}
	// Tenant B naming tenant A's jurisdiction is equally denied.
	if eligible, _, _ := check(t, pool, fB, uuid.Nil, fA.jurisdictionID, code, "casino", OperationWagering); eligible {
		t.Fatal("tenant B must not borrow tenant A's jurisdiction authorization")
	}
	// And tenant B cannot read tenant A's rows at all.
	err := pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := ListAuthorizations(ctx, tx, code)
		if err != nil {
			return err
		}
		if len(rows) != 0 {
			return fmt.Errorf("tenant B saw %d of tenant A's authorization rows", len(rows))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A tenant identifier that did not come from the credential can never be
// evaluated - the spoofing defence.
func TestIsolation_TenantArgumentMustMatchTheTransactionScope(t *testing.T) {
	pool := testPool(t)
	fA := seedTenantFixture(t, pool)
	fB := seedTenantFixture(t, pool)

	var reason ReasonCode
	var checkErr error
	err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		// The transaction is tenant A's; the argument claims tenant B.
		_, reason, checkErr = AssetAuthorization{}.CheckEligibility(
			ctx, tx, fB.tenantID, uuid.Nil, fB.jurisdictionID, "EUR",
			OperationScope{Product: "casino", Operation: OperationWagering})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(checkErr, ErrTenantContextMismatch) {
		t.Fatalf("expected ErrTenantContextMismatch, got %v", checkErr)
	}
	if reason != ReasonInternalError {
		t.Fatalf("expected internal_error reason alongside the mismatch, got %q", reason)
	}
}

// A brand belonging to another tenant cannot be named at write time: the
// composite (brand_id, tenant_id) FK refuses it.
func TestIsolation_CrossTenantBrandCannotBeAuthorized(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	fA := seedTenantFixture(t, pool)
	fB := seedTenantFixture(t, pool)
	code := liveAsset(t, pool, adminA, adminB)
	authorizeFullChain(t, pool, fA, adminA, code, "casino", OperationWagering)

	staffID := seedTenantStaff(t, pool, fA.tenantID)
	err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := AuthorizeScope(ctx, tx, AuthorizeScopeParams{
			TenantID: fA.tenantID, ScopeKind: ScopeBrand, BrandID: fB.brandID, AssetCode: code,
			Product: "casino", Eligible: true, Actor: actor(staffID),
		})
		return err
	})
	if err == nil {
		t.Fatal("tenant A must not be able to authorize a brand belonging to tenant B")
	}
}

func TestIsolation_TenantCannotWritePlatformWideEligibilityDefault(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	f := seedTenantFixture(t, pool)
	code := liveAsset(t, pool, adminA, adminB)
	staffID := seedTenantStaff(t, pool, f.tenantID)

	// RLS is still the FIRST refusal here, unchanged by migration 0047:
	// the WITH CHECK is evaluated before the new AFTER dual-control
	// trigger runs, so a tenant-scoped connection never even reaches the
	// four-eyes question. Both controls are real, and either alone
	// suffices - a tenant-scoped connection could not satisfy the
	// trigger anyway, because asset_change_requests is
	// platform-admin-scoped and the consume would find no approval to
	// spend no matter what was approved elsewhere.
	//
	// Asserted for BOTH directions on purpose: a platform-wide denial
	// (eligible = false) skips the dual-control path entirely by design,
	// so it is the case that isolates the RLS backstop from the new
	// trigger.
	for _, eligible := range []bool{true, false} {
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				INSERT INTO asset_operation_eligibility
					(tenant_id, asset_code, product, operation, eligible, created_by_actor_type, created_by_actor_id)
				VALUES (NULL, $1, 'casino', 'wagering', $3, 'staff', $2)`, code, staffID, eligible)
			return err
		})
		if err == nil {
			t.Fatalf("a tenant-scoped connection must never write a platform-wide eligibility row (eligible=%v)", eligible)
		}
		assertPgErrorCode(t, err, pgRLSViolation)
	}
}

// A player-initiated operation runs under WithPlayerScope, and
// CLAUDE.md requires the authoritative read to happen in the SAME
// transaction as the write it authorizes - so CheckEligibility must work
// there (read-only) and must still be unable to change any configuration.
func TestPlayerScope_CanEvaluateEligibilityButNeverConfigureIt(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	f := seedTenantFixture(t, pool)
	code := liveAsset(t, pool, adminA, adminB)
	authorizeFullChain(t, pool, f, adminA, code, "casino", OperationWagering)

	var eligible bool
	var reason ReasonCode
	var checkErr error
	err := pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
		eligible, reason, checkErr = AssetAuthorization{}.CheckEligibility(
			ctx, tx, f.tenantID, f.brandID, f.jurisdictionID, code,
			OperationScope{Product: "casino", Operation: OperationWagering})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checkErr != nil || !eligible {
		t.Fatalf("a player-scoped transaction must be able to resolve eligibility, got eligible=%v reason=%q err=%v",
			eligible, reason, checkErr)
	}

	// A denial still reaches it - the player path reads the same facts,
	// it does not get a permissive shortcut.
	staffID := seedTenantStaff(t, pool, f.tenantID)
	setScope(t, pool, f, staffID, ScopeTenant, uuid.Nil, uuid.Nil, code, "casino", false)
	err = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
		_, reason, _ = AssetAuthorization{}.CheckEligibility(
			ctx, tx, f.tenantID, f.brandID, f.jurisdictionID, code,
			OperationScope{Product: "casino", Operation: OperationWagering})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if reason != ReasonTenantNotAuthorized {
		t.Fatalf("expected the player path to see the tenant denial, got %q", reason)
	}

	// And it cannot write - neither an authorization nor an eligibility
	// row. The UPDATE has no matching policy, so RLS gives it zero row
	// visibility: it affects nothing rather than flipping the denial the
	// player is subject to.
	err = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE asset_authorizations SET eligible = true WHERE asset_code = $1`, code)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			return fmt.Errorf("a player-scoped connection updated %d authorization rows; expected 0", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO asset_operation_eligibility
				(tenant_id, asset_code, product, operation, eligible, created_by_actor_type, created_by_actor_id)
			VALUES ($1, $2, 'casino', 'wagering', true, 'staff', $3)`, f.tenantID, code, f.playerID)
		return err
	})
	if err == nil {
		t.Fatal("a player-scoped connection must not be able to insert an eligibility row")
	}
	assertPgErrorCode(t, err, pgRLSViolation)
}

// --- Audit ---

func TestAudit_EveryMutatingOperationWritesARecord(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	f := seedTenantFixture(t, pool)
	code := liveAsset(t, pool, adminA, adminB)
	authorizeFullChain(t, pool, f, adminA, code, "casino", OperationWagering)

	// Platform-scoped audit rows (tenant_id NULL). Read under
	// WithPlatformAdmin, not WithoutTenant: the assertion joins
	// asset_change_requests, which is only visible with the platform-admin
	// GUC set (that is finding S-3's backstop working as intended).
	err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		for _, action := range []string{
			"asset.change_requested", "asset.change_approved", "asset.created",
			"asset.active_granted", "asset.platform_authorized_granted",
			"asset_operation_eligibility.configured",
		} {
			var count int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM audit_log WHERE action = $1 AND tenant_id IS NULL
				   AND (target_id = $2 OR metadata->>'asset_code' = $2 OR metadata->'after'->>'asset_code' = $2
				        OR target_id IN (SELECT id::text FROM asset_change_requests WHERE asset_code = $2))`,
				action, code).Scan(&count); err != nil {
				return err
			}
			if count == 0 {
				return fmt.Errorf("no audit record for %s on asset %s", action, code)
			}
		}
		// The reason code must actually be there - an empty audit field
		// would satisfy "a record exists" while failing CLAUDE.md's rule.
		var reason *string
		if err := tx.QueryRow(ctx,
			`SELECT metadata->>'reason_code' FROM audit_log
			  WHERE action = 'asset.created' AND target_id = $1`, code).Scan(&reason); err != nil {
			return err
		}
		if reason == nil || *reason == "" {
			return errors.New("asset.created audit record has no reason_code")
		}
		// Before/after state must be present for a state change.
		var before, after *string
		if err := tx.QueryRow(ctx,
			`SELECT metadata->>'before', metadata->>'after' FROM audit_log
			  WHERE action = 'asset.active_granted' AND target_id = $1`, code).Scan(&before, &after); err != nil {
			return err
		}
		if before == nil || after == nil || *before == *after {
			return errors.New("asset.active_granted audit record lacks a distinct before/after state")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Tenant-scoped audit rows for layers 4-6.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log
			  WHERE action = 'asset_authorization.tenant_configured' AND tenant_id = $1`, f.tenantID).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			return errors.New("no tenant-scoped audit record for a layer-4 authorization")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// --- Product registry ---

func TestProductRegistry_UnknownOrInactiveProductDenies(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	f := seedTenantFixture(t, pool)
	code := liveAsset(t, pool, adminA, adminB)
	authorizeFullChain(t, pool, f, adminA, code, "casino", OperationWagering)

	if _, reason, _ := check(t, pool, f, uuid.Nil, f.jurisdictionID, code, "roulette-only", OperationWagering); reason != ReasonProductUnknown {
		t.Fatalf("expected product_unknown, got %q", reason)
	}
}

func TestProductRegistry_IsDataNotAClosedEnum(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	f := seedTenantFixture(t, pool)
	code := liveAsset(t, pool, adminA, adminB)

	// A brand-new product is a row, not a migration to a CHECK list.
	newProduct := "retail-" + uuid.New().String()[:8]
	err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO platform_products (code, display_name, active) VALUES ($1, 'Retail', true)`, newProduct)
		return err
	})
	if err != nil {
		t.Fatalf("a new product must be insertable as data: %v", err)
	}
	authorizeFullChain(t, pool, f, adminA, code, newProduct, OperationWagering)
	if eligible, reason, err := check(t, pool, f, uuid.Nil, f.jurisdictionID, code, newProduct, OperationWagering); !eligible {
		t.Fatalf("a newly registered product must be usable with no code change, got reason=%q err=%v", reason, err)
	}

	// A tenant-scoped connection cannot add one.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO platform_products (code, display_name, active) VALUES ($1, 'Forged', true)`,
			"forged-"+uuid.New().String()[:8])
		return err
	})
	if err == nil {
		t.Fatal("a tenant-scoped connection must not register a platform product")
	}
	assertPgErrorCode(t, err, pgRLSViolation)
}

// ======================================================================
// Migration 0047 regression tests - the three defects independent
// security and code review found in migrations 0044/0045.
// ======================================================================

// --- Fix 1: the person-identity half of four-eyes was inert ---

// The literal reproduction of security's live exploit, and the headline
// regression test for this fix: ONE human runs cmd/seed-admin twice.
//
// That is not a contrived fixture - it is the only platform_admin state a
// real deployment can be in, because cmd/seed-admin passes nil for
// personID, admin_routes.go's staff-creation role allowlist excludes
// platform_admin entirely, and the person-link remediation route is
// tenant-scoped (so staff_users' dual-scope RLS hides every
// tenant_id IS NULL row from it). Under migration 0044 the person
// comparison was skipped whenever either side was NULL, so those two
// accounts satisfied "two distinct principals" and one operator completed
// create -> activate -> platform_authorize alone.
//
// This test FAILS against migration 0044 (every step succeeds) and PASSES
// against 0047 (every step is refused).
func TestDualControl_TwoUnlinkedPlatformAdminsCannotCompleteAnyDualControlledOperation(t *testing.T) {
	pool := testPool(t)
	// Two separate `seed-admin` runs by one person: distinct staff UUIDs,
	// both person_id NULL.
	unlinkedA := seedPlatformAdminRaw(t, pool, nil, "active")
	unlinkedB := seedPlatformAdminRaw(t, pool, nil, "active")
	code := newAssetCode()

	// Step 1 of the exploit: file the create request. Refused outright -
	// an unlinked principal cannot even open a four-eyes request, because
	// the control it is opening cannot be evaluated for it.
	err := pool.WithPlatformAdmin(context.Background(), unlinkedA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
			Operation: ChangeCreate, AssetCode: code, AssetType: AssetTypeFiat,
			DecimalExponent: 2, Actor: actor(unlinkedA),
		})
		return err
	})
	if !errors.Is(err, ErrApproverNotEligible) {
		t.Fatalf("an unlinked platform principal must not be able to file a change request, got %v", err)
	}

	// Step 2: with no approved request, the mutation itself is refused
	// too - so the exploit cannot be completed by skipping the paperwork.
	err = pool.WithPlatformAdmin(context.Background(), unlinkedA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateAsset(ctx, tx, CreateAssetParams{
			Code: code, AssetType: AssetTypeFiat, DecimalExponent: 2,
			DisplayName: "Exploit", Actor: actor(unlinkedA),
		})
		return err
	})
	if !errors.Is(err, ErrDualControlRequired) {
		t.Fatalf("creating an asset with no approved request must be refused, got %v", err)
	}

	// Step 3: and the same for the other two layer-1-3 operations, on an
	// asset that DOES exist (created legitimately), so the refusal is
	// attributable to the principals rather than to the missing row.
	linkedA := seedPlatformAdmin(t, pool, nil)
	linkedB := seedPlatformAdmin(t, pool, nil)
	live := liveAsset(t, pool, linkedA, linkedB)

	for _, op := range []ChangeOperation{ChangeActivate, ChangePlatformAuthorize} {
		err = pool.WithPlatformAdmin(context.Background(), unlinkedA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
				Operation: op, AssetCode: live, Actor: actor(unlinkedA),
			})
			return err
		})
		if !errors.Is(err, ErrApproverNotEligible) {
			t.Fatalf("an unlinked principal must not be able to file a %s request, got %v", op, err)
		}
	}

	// And the approval side independently: a LINKED requester's genuine
	// request cannot be approved by an unlinked account either.
	freshCode := newAssetCode()
	var reqID uuid.UUID
	err = pool.WithPlatformAdmin(context.Background(), linkedA, func(ctx context.Context, tx pgx.Tx) error {
		req, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
			Operation: ChangeCreate, AssetCode: freshCode, AssetType: AssetTypeFiat,
			DecimalExponent: 2, Actor: actor(linkedA),
		})
		reqID = req.ID
		return err
	})
	if err != nil {
		t.Fatalf("a linked principal must be able to file a request: %v", err)
	}
	err = pool.WithPlatformAdmin(context.Background(), unlinkedB, func(ctx context.Context, tx pgx.Tx) error {
		_, err := DecideChangeRequest(ctx, tx, DecideChangeRequestParams{
			RequestID: reqID, Approve: true, Actor: actor(unlinkedB),
		})
		return err
	})
	if !errors.Is(err, ErrApproverNotEligible) {
		t.Fatalf("an unlinked platform principal must not be able to approve, got %v", err)
	}

	// The asset never came into existence, and nothing was activated.
	err = pool.WithPlatformAdmin(context.Background(), linkedA, func(ctx context.Context, tx pgx.Tx) error {
		for _, c := range []string{code, freshCode} {
			var count int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM assets WHERE code = $1`, c).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return fmt.Errorf("asset %s exists; the unilateral path was not fully closed", c)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The same exploit against the layer-7 platform-wide grant (fix 1 and
// fix 2 interacting): an unlinked pair cannot obtain a platform-wide
// eligibility default either.
func TestDualControl_TwoUnlinkedPlatformAdminsCannotGrantPlatformWideEligibility(t *testing.T) {
	pool := testPool(t)
	linkedA := seedPlatformAdmin(t, pool, nil)
	linkedB := seedPlatformAdmin(t, pool, nil)
	code := liveAsset(t, pool, linkedA, linkedB)

	unlinkedA := seedPlatformAdminRaw(t, pool, nil, "active")
	err := pool.WithPlatformAdmin(context.Background(), unlinkedA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
			Operation: ChangePlatformOperationEligibility, AssetCode: code,
			EligibilityOperation: OperationWagering, EligibilityProduct: "casino", Actor: actor(unlinkedA),
		})
		return err
	})
	if !errors.Is(err, ErrApproverNotEligible) {
		t.Fatalf("an unlinked principal must not be able to file a layer-7 grant request, got %v", err)
	}

	err = pool.WithPlatformAdmin(context.Background(), unlinkedA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ConfigureOperationEligibility(ctx, tx, ConfigureEligibilityParams{
			AssetCode: code, Product: "casino", Operation: OperationWagering,
			Eligible: true, Actor: actor(unlinkedA),
		})
		return err
	})
	if !errors.Is(err, ErrDualControlRequired) {
		t.Fatalf("a platform-wide grant with no approval must be refused, got %v", err)
	}
}

// A suspended staff account is not an eligible approver, even though it
// is a distinct, person-linked, platform-scoped principal. Migration
// 0034's rule for withdrawal decisions, now applied to asset changes:
// an off-boarded operator's credential must not be able to complete a
// four-eyes control.
func TestDualControl_SuspendedApproverIsRefused(t *testing.T) {
	pool := testPool(t)
	requester := seedPlatformAdmin(t, pool, nil)
	suspendedPerson := seedPerson(t, pool)
	suspended := seedPlatformAdminRaw(t, pool, &suspendedPerson, "suspended")

	code := newAssetCode()
	var reqID uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		req, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
			Operation: ChangeCreate, AssetCode: code, AssetType: AssetTypeFiat,
			DecimalExponent: 2, Actor: actor(requester),
		})
		reqID = req.ID
		return err
	})
	if err != nil {
		t.Fatalf("file request: %v", err)
	}

	err = pool.WithPlatformAdmin(context.Background(), suspended, func(ctx context.Context, tx pgx.Tx) error {
		_, err := DecideChangeRequest(ctx, tx, DecideChangeRequestParams{
			RequestID: reqID, Approve: true, Actor: actor(suspended),
		})
		return err
	})
	if !errors.Is(err, ErrApproverNotEligible) {
		t.Fatalf("a suspended approver must be refused, got %v", err)
	}

	// And the request is still unusable afterwards - the refused decision
	// left no approval behind.
	err = pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := CreateAsset(ctx, tx, CreateAssetParams{
			Code: code, AssetType: AssetTypeFiat, DecimalExponent: 2,
			DisplayName: "Suspended approval", Actor: actor(requester),
		})
		return err
	})
	if !errors.Is(err, ErrDualControlRequired) {
		t.Fatalf("a refused suspended-approver decision must not authorize the mutation, got %v", err)
	}
}

// The requester side is checked too: a suspended account cannot open a
// four-eyes request for someone else to rubber-stamp.
func TestDualControl_SuspendedRequesterIsRefused(t *testing.T) {
	pool := testPool(t)
	person := seedPerson(t, pool)
	suspended := seedPlatformAdminRaw(t, pool, &person, "suspended")

	err := pool.WithPlatformAdmin(context.Background(), suspended, func(ctx context.Context, tx pgx.Tx) error {
		_, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
			Operation: ChangeCreate, AssetCode: newAssetCode(), AssetType: AssetTypeFiat,
			DecimalExponent: 2, Actor: actor(suspended),
		})
		return err
	})
	if !errors.Is(err, ErrApproverNotEligible) {
		t.Fatalf("a suspended requester must be refused, got %v", err)
	}
}

// A principal with no staff_users row at all (e.g. a stale or forged
// subject claim) is refused rather than treated as an exempt service
// identity. Unlike withdrawals, no service identity ever files or decides
// an asset registry change (ADR 0037 §C.1).
func TestDualControl_UnresolvablePrincipalIsRefused(t *testing.T) {
	pool := testPool(t)
	ghost := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), ghost, func(ctx context.Context, tx pgx.Tx) error {
		_, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
			Operation: ChangeCreate, AssetCode: newAssetCode(), AssetType: AssetTypeFiat,
			DecimalExponent: 2, Actor: actor(ghost),
		})
		return err
	})
	if !errors.Is(err, ErrApproverNotEligible) {
		t.Fatalf("an unresolvable principal must be refused, got %v", err)
	}
}

// --- Fix 2: the layer-7 platform-wide grant is dual-controlled ---

func TestLayer7_PlatformWideGrantRequiresTwoDistinctApprovals(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	code := liveAsset(t, pool, adminA, adminB)

	grant := func(actorID uuid.UUID, op Operation, product string) error {
		return pool.WithPlatformAdmin(context.Background(), actorID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := ConfigureOperationEligibility(ctx, tx, ConfigureEligibilityParams{
				AssetCode: code, Product: product, Operation: op, Eligible: true, Actor: actor(actorID),
			})
			return err
		})
	}

	// 1. No request at all: refused. This is the defect - before
	// migration 0047 this single call succeeded and flipped a
	// platform-wide gate for every tenant on the platform.
	if err := grant(adminA, OperationWagering, "casino"); !errors.Is(err, ErrDualControlRequired) {
		t.Fatalf("a platform-wide grant with no approved request must be refused, got %v", err)
	}

	// 2. A request the filer approves itself: refused at the approval.
	var reqID uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		req, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
			Operation: ChangePlatformOperationEligibility, AssetCode: code,
			EligibilityOperation: OperationWagering, EligibilityProduct: "casino", Actor: actor(adminA),
		})
		reqID = req.ID
		return err
	})
	if err != nil {
		t.Fatalf("file layer-7 grant request: %v", err)
	}
	err = pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := DecideChangeRequest(ctx, tx, DecideChangeRequestParams{
			RequestID: reqID, Approve: true, Actor: actor(adminA),
		})
		return err
	})
	if !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("self-approval of a layer-7 grant must be refused, got %v", err)
	}
	// Still refused, because the pending request carries no valid approval.
	if err := grant(adminA, OperationWagering, "casino"); !errors.Is(err, ErrDualControlRequired) {
		t.Fatalf("an unapproved request must not authorize the grant, got %v", err)
	}

	// 3. Two staff accounts, ONE person: refused (fix 1 and fix 2
	// together - this is the shape the whole exploit relied on).
	sharedPerson := seedPerson(t, pool)
	twinA := seedPlatformAdmin(t, pool, &sharedPerson)
	twinB := seedPlatformAdmin(t, pool, &sharedPerson)
	var twinReq uuid.UUID
	err = pool.WithPlatformAdmin(context.Background(), twinA, func(ctx context.Context, tx pgx.Tx) error {
		req, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
			Operation: ChangePlatformOperationEligibility, AssetCode: code,
			EligibilityOperation: OperationDeposit, EligibilityProduct: "casino", Actor: actor(twinA),
		})
		twinReq = req.ID
		return err
	})
	if err != nil {
		t.Fatalf("file twin request: %v", err)
	}
	err = pool.WithPlatformAdmin(context.Background(), twinB, func(ctx context.Context, tx pgx.Tx) error {
		_, err := DecideChangeRequest(ctx, tx, DecideChangeRequestParams{
			RequestID: twinReq, Approve: true, Actor: actor(twinB),
		})
		return err
	})
	if !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("one person approving through a second staff account must be refused, got %v", err)
	}

	// 4. A genuine second person: the grant goes through.
	err = pool.WithPlatformAdmin(context.Background(), adminB, func(ctx context.Context, tx pgx.Tx) error {
		_, err := DecideChangeRequest(ctx, tx, DecideChangeRequestParams{
			RequestID: reqID, Approve: true, Actor: actor(adminB),
		})
		return err
	})
	if err != nil {
		t.Fatalf("a distinct person's approval must be accepted: %v", err)
	}
	if err := grant(adminA, OperationWagering, "casino"); err != nil {
		t.Fatalf("an approved platform-wide grant must be accepted: %v", err)
	}

	// 5. The approval is consumed: a second grant of the SAME fact needs
	// its own approval. (Re-granting an already-eligible row is a no-op
	// re-write and is allowed; a genuinely new grant is not.)
	if err := grant(adminA, OperationWithdrawal, "casino"); !errors.Is(err, ErrDualControlRequired) {
		t.Fatalf("a consumed approval must not authorize a second, different grant, got %v", err)
	}
}

// An approval names the EXACT (operation, product) being granted. An
// approval for casino wagering cannot be spent on sportsbook wagering, on
// casino withdrawal, or on an every-product grant - otherwise the
// approver approved something other than what was applied, which is the
// same defect ADR 0037 §C.5.4's payload matching exists to prevent for
// asset creation.
func TestLayer7_PlatformWideGrantApprovalIsBoundToItsExactOperationAndProduct(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	code := liveAsset(t, pool, adminA, adminB)

	fileAndApproveEligibility(t, pool, code, OperationWagering, "casino", adminA, adminB)

	attempt := func(op Operation, product string) error {
		return pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := ConfigureOperationEligibility(ctx, tx, ConfigureEligibilityParams{
				AssetCode: code, Product: product, Operation: op, Eligible: true, Actor: actor(adminA),
			})
			return err
		})
	}

	if err := attempt(OperationWagering, "sportsbook"); !errors.Is(err, ErrDualControlRequired) {
		t.Fatalf("a casino approval must not authorize a sportsbook grant, got %v", err)
	}
	if err := attempt(OperationWithdrawal, "casino"); !errors.Is(err, ErrDualControlRequired) {
		t.Fatalf("a wagering approval must not authorize a withdrawal grant, got %v", err)
	}
	// Every-product ("" -> the EveryProduct sentinel) is strictly broader
	// than casino and must not be reachable with a casino approval.
	if err := attempt(OperationWagering, ""); !errors.Is(err, ErrDualControlRequired) {
		t.Fatalf("a casino-specific approval must not authorize an every-product grant, got %v", err)
	}
	// The approved fact itself still works.
	if err := attempt(OperationWagering, "casino"); err != nil {
		t.Fatalf("the approved (wagering, casino) grant must be accepted: %v", err)
	}
}

// Revoking a platform-wide default is deliberately single-actor - the
// fail-closed direction must never wait for a second approver (ADR 0037
// §C.5.3's asymmetry, already applied to suspend/revoke at layers 2-3).
// And re-granting after a revocation needs a NEW approval.
func TestLayer7_PlatformWideRevocationIsSingleActorButRegrantIsNot(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	code := liveAsset(t, pool, adminA, adminB)

	fileAndApproveEligibility(t, pool, code, OperationWagering, "casino", adminA, adminB)
	set := func(eligible bool) error {
		return pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := ConfigureOperationEligibility(ctx, tx, ConfigureEligibilityParams{
				AssetCode: code, Product: "casino", Operation: OperationWagering,
				Eligible: eligible, Actor: actor(adminA),
			})
			return err
		})
	}
	if err := set(true); err != nil {
		t.Fatalf("approved grant: %v", err)
	}
	// An idempotent re-grant of an already-eligible row needs no new
	// approval (mirrors assets_enforce_dual_control's off->on rule).
	if err := set(true); err != nil {
		t.Fatalf("an idempotent re-write of an already-granted row must not require a new approval: %v", err)
	}
	if err := set(false); err != nil {
		t.Fatalf("revocation must be single-actor: %v", err)
	}
	if err := set(true); !errors.Is(err, ErrDualControlRequired) {
		t.Fatalf("re-granting after a revocation must require a new approval, got %v", err)
	}
}

// Raw SQL cannot launder one approval into a grant of a different fact by
// mutating the row's key columns afterwards.
func TestLayer7_MutatingAGrantedRowsKeyColumnsRequiresItsOwnApproval(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	code := liveAsset(t, pool, adminA, adminB)

	fileAndApproveEligibility(t, pool, code, OperationWagering, "casino", adminA, adminB)
	err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ConfigureOperationEligibility(ctx, tx, ConfigureEligibilityParams{
			AssetCode: code, Product: "casino", Operation: OperationWagering,
			Eligible: true, Actor: actor(adminA),
		})
		return err
	})
	if err != nil {
		t.Fatalf("approved grant: %v", err)
	}

	err = pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE asset_operation_eligibility SET operation = 'withdrawal'
			  WHERE tenant_id IS NULL AND asset_code = $1 AND operation = 'wagering'`, code)
		return err
	})
	if err == nil {
		t.Fatal("re-pointing an approved grant at a different operation must be refused")
	}
	pgErr := assertPgErrorCode(t, err, pgRaisedException)
	if !strings.Contains(pgErr.Message, "four-eyes") {
		t.Fatalf("expected a four-eyes refusal, got %q", pgErr.Message)
	}
}

// --- Fix 3: asset_authorizations DELETE / TRUNCATE ---

// Security's live exploit for fix 3: a brand-level `eligible = false`
// denial row is the ONLY thing denying that brand, because
// CheckEligibility treats an ABSENT layer-5 row as "inherit the tenant
// answer". Deleting it therefore promoted the brand from denied to
// allowed, with no trigger able to see it (the narrowing trigger is
// BEFORE INSERT OR UPDATE only) and nothing in the audit trail.
//
// This test FAILS against migration 0045 (the delete succeeds and the
// brand becomes eligible) and PASSES against 0047.
func TestRLS_DeletingABrandDenialCannotSilentlyWidenEligibility(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	f := seedTenantFixture(t, pool)
	code := liveAsset(t, pool, adminA, adminB)
	authorizeFullChain(t, pool, f, adminA, code, "casino", OperationWagering)

	staffID := seedTenantStaff(t, pool, f.tenantID)
	setScope(t, pool, f, staffID, ScopeBrand, uuid.Nil, f.brandID, code, "casino", false)

	if _, reason, _ := check(t, pool, f, f.brandID, f.jurisdictionID, code, "casino", OperationWagering); reason != ReasonBrandNotAuthorized {
		t.Fatalf("precondition: the brand must be denied, got %q", reason)
	}

	// The exploit: delete the denial row from the tenant's own scope.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`DELETE FROM asset_authorizations WHERE tenant_id = $1 AND scope_kind = 'brand' AND asset_code = $2`,
			f.tenantID, code)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			return fmt.Errorf("a tenant-scoped connection deleted %d authorization rows; expected 0", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// A platform-admin-scoped connection has no DELETE policy either -
	// there is no scope from which this row can be removed.
	err = pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM asset_authorizations WHERE asset_code = $1`, code)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			return fmt.Errorf("a platform-admin connection deleted %d authorization rows; expected 0", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// Nor a player-scoped one.
	err = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM asset_authorizations WHERE asset_code = $1`, code)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			return fmt.Errorf("a player-scoped connection deleted %d authorization rows; expected 0", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// The denial still stands - which is the whole point.
	if _, reason, _ := check(t, pool, f, f.brandID, f.jurisdictionID, code, "casino", OperationWagering); reason != ReasonBrandNotAuthorized {
		t.Fatalf("the brand denial must survive every delete attempt, got %q", reason)
	}
	// And the row is still there, with its reason_code and actor intact.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM asset_authorizations
			  WHERE tenant_id = $1 AND scope_kind = 'brand' AND asset_code = $2 AND eligible = false`,
			f.tenantID, code).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("expected the brand denial row to still exist, found %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The same erasure in bulk. RLS does not apply to TRUNCATE at all (it is
// an owner-level operation and the application role owns these tables),
// so the per-command policy split cannot cover it - a statement-level
// trigger does, matching the guard migration 0044 already gives `assets`,
// asset_change_requests and asset_change_approvals.
func TestRLS_AuthorizationTablesCannotBeTruncated(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)

	for _, table := range []string{"asset_authorizations", "asset_operation_eligibility"} {
		err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `TRUNCATE TABLE `+table)
			return err
		})
		if err == nil {
			t.Fatalf("%s must not be truncatable - one statement would widen every absent-row denial on the platform", table)
		}
		assertPgErrorCode(t, err, pgRaisedException)
	}
}

// The tenant_id ... ON DELETE CASCADE migration 0045 declared must keep
// working: PostgreSQL runs referential-integrity actions with RLS
// bypassed, so removing a tenant still removes its authorization rows.
// Fix 3 is not allowed to silently change that contract, which is also
// why no BEFORE DELETE deny-trigger was added (one would contradict the
// cascade, unlike `assets`, which declares no cascade).
func TestRLS_TenantDeletionStillCascadesAuthorizationRows(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	code := liveAsset(t, pool, adminA, adminB)

	// A deliberately bare tenant - no brand, no player account. Not a
	// shortcut: seedTenantFixture's player_accounts row has a
	// non-cascading FK to brands, so deleting that tenant fails for a
	// reason that has nothing to do with the property under test here.
	tenantID := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model)
			 VALUES ($1, $2, 'Cascade Tenant', 'under_platform_licence')`,
			tenantID, "cas-"+tenantID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 tenant row, inserted %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed bare tenant: %v", err)
	}
	staffID := seedTenantStaff(t, pool, tenantID)
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := AuthorizeScope(ctx, tx, AuthorizeScopeParams{
			TenantID: tenantID, ScopeKind: ScopeTenant, AssetCode: code, Product: "casino",
			Eligible: true, Actor: actor(staffID),
		})
		return err
	})
	if err != nil {
		t.Fatalf("authorize tenant scope: %v", err)
	}

	err = pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to delete 1 tenant row, deleted %d", tag.RowsAffected())
		}
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM asset_authorizations WHERE tenant_id = $1`, tenantID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("expected the cascade to remove the tenant's authorization rows, %d remain", count)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// --- Fix 5: a duplicate decision is not self-approval ---

func TestDecideChangeRequest_DuplicateDecisionIsDistinctFromSelfApproval(t *testing.T) {
	pool := testPool(t)
	adminA := seedPlatformAdmin(t, pool, nil)
	adminB := seedPlatformAdmin(t, pool, nil)
	code := newAssetCode()

	var reqID uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
		req, err := FileChangeRequest(ctx, tx, FileChangeRequestParams{
			Operation: ChangeCreate, AssetCode: code, AssetType: AssetTypeFiat,
			DecimalExponent: 2, Actor: actor(adminA),
		})
		reqID = req.ID
		return err
	})
	if err != nil {
		t.Fatalf("file request: %v", err)
	}

	decide := func(id uuid.UUID) error {
		return pool.WithPlatformAdmin(context.Background(), id, func(ctx context.Context, tx pgx.Tx) error {
			_, err := DecideChangeRequest(ctx, tx, DecideChangeRequestParams{
				RequestID: reqID, Approve: true, Actor: actor(id),
			})
			return err
		})
	}
	if err := decide(adminB); err != nil {
		t.Fatalf("first decision: %v", err)
	}

	// The SECOND decision by the same approver is a duplicate submission
	// (a retry, a double-clicked button) - not self-dealing.
	err = decide(adminB)
	if !errors.Is(err, ErrDuplicateDecision) {
		t.Fatalf("expected ErrDuplicateDecision for a repeated decision, got %v", err)
	}
	if errors.Is(err, ErrSelfApproval) {
		t.Fatal("a duplicate decision must NOT be reported as self-approval (code-reviewer F9): it puts a false integrity signal in front of an operator who merely retried")
	}

	// And the genuine self-approval case is still ErrSelfApproval, from
	// the trigger - the two conditions stay distinguishable.
	err = decide(adminA)
	if !errors.Is(err, ErrSelfApproval) {
		t.Fatalf("expected ErrSelfApproval for the requester's own decision, got %v", err)
	}
	if errors.Is(err, ErrDuplicateDecision) {
		t.Fatal("self-approval must not be reported as a duplicate decision")
	}
}

// --- GetAsset stability (internal/risk's denomination.go will call it) ---

// internal/risk is being changed (separate dispatch) to read
// assets.decimal_exponent through GetAsset instead of its own raw SQL.
// This pins the two properties that change depends on: it is readable
// from a TENANT-scoped transaction (risk evaluates inside one), and a
// missing asset is ErrNotFound rather than a zero exponent - a silent
// zero would turn a minor-unit threshold into a major-unit one.
func TestGetAsset_IsReadableFromTenantScopeAndFailsClosedOnMissing(t *testing.T) {
	pool := testPool(t)
	f := seedTenantFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		a, err := GetAsset(ctx, tx, "EUR")
		if err != nil {
			return fmt.Errorf("GetAsset from tenant scope: %w", err)
		}
		if a.Code != "EUR" || a.DecimalExponent != 2 {
			return fmt.Errorf("unexpected asset row: %+v", a)
		}
		if _, err := GetAsset(ctx, tx, "NO-SUCH-ASSET"); !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("expected ErrNotFound for an unknown asset, got %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// And from a player-scoped transaction, which is where a
	// player-initiated financial operation resolves it.
	err = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
		a, err := GetAsset(ctx, tx, "EUR")
		if err != nil {
			return err
		}
		if a.DecimalExponent != 2 {
			return fmt.Errorf("unexpected exponent %d", a.DecimalExponent)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
