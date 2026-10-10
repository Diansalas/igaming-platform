//go:build integration

// Package launchfix is the TEST-ONLY fixture path of ADR 0112 section 4.6 (LF2).
//
// Since migration 0128 a tenant or brand is created 'pending_launch' and its status
// moves only through a governed, same-transaction decision record. Existing
// integration fixtures need ACTIVE (and sometimes suspended / closed) subjects, so they
// provision them through the one path the database leaves open on purpose: the TABLE
// OWNER role inserting a row with an explicit non-pending status. The subject INSERT
// guard (LA021) refuses that for every other role, and the database itself writes the
// matching `owner_provisioned` launch_status_transitions row, so owner-provisioned
// fixtures are visible in the history exactly as production owner inserts would be.
//
// Mid-test status changes (a tenant that is active, then suspended) go through the REAL
// guards: SetTenantStatus / SetBrandStatus below run a complete governed sequence in SQL
// (a request, the required number of distinct-Person approvals, executing, the governed
// transition, the status UPDATE, executed) as fixture platform operators. Nothing is
// disabled or bypassed; a test that wants the refusal paths exercises the guards directly.
//
// This package carries the `integration` build tag and lives under internal/testsupport,
// so it is never compiled into an application binary; a static test
// (internal/tenant/launch_fixture_pin_test.go) pins that no non-test package imports it.
// It must only ever be pointed at a synthetic development / CI database (CLAUDE.md
// "Environment safety"); the operators it creates are synthetic and unreachable by login
// (the password hash is not a valid hash).
package launchfix

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

var (
	ownerPoolOnce sync.Once
	ownerPool     *db.Pool
	ownerPoolErr  error
)

// OwnerPool returns a process-wide pool on the TABLE-OWNER role (TEST_DATABASE_URL). Fixtures
// whose callers hold a RUNTIME-role pool (the runtime role may only create pending_launch rows,
// guard LA021) use it to provision the tenants and brands they need ACTIVE, then continue with
// the runtime pool for everything else. The pool is never closed (test process lifetime).
func OwnerPool(tb testing.TB) *db.Pool {
	tb.Helper()
	u := ownerURL(tb)
	ownerPoolOnce.Do(func() {
		ownerPool, ownerPoolErr = db.Connect(context.Background(), u, 4, 5*time.Second)
	})
	if ownerPoolErr != nil {
		tb.Fatalf("launchfix: connect owner pool: %v", ownerPoolErr)
	}
	return ownerPool
}

var (
	ownerForMu    sync.Mutex
	ownerForPools = map[string]*db.Pool{}
)

// OwnerFor returns a pool on the TABLE-OWNER role for the SAME database p is connected to. If p
// already connects as the table owner it is returned unchanged; if p is the runtime role (or any
// other non-owner), a cached owner pool for that database is opened from TEST_DATABASE_URL's
// credentials. This is what fixtures use when their caller may hold either a shared-database
// pool or a scratch-database pool, owner or runtime (the runtime role may only create
// pending_launch rows, guard LA021).
func OwnerFor(tb testing.TB, p *db.Pool) *db.Pool {
	tb.Helper()
	var isOwner bool
	var dbName string
	if err := p.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_user = pg_get_userbyid(c.relowner), current_database()
			FROM pg_class c WHERE c.oid = 'public.tenants'::regclass`).Scan(&isOwner, &dbName)
	}); err != nil {
		tb.Fatalf("launchfix: inspect pool role: %v", err)
	}
	if isOwner {
		return p
	}
	u, err := url.Parse(ownerURL(tb))
	if err != nil {
		tb.Fatalf("launchfix: parse TEST_DATABASE_URL: %v", err)
	}
	u.Path = "/" + dbName
	ownerForMu.Lock()
	defer ownerForMu.Unlock()
	if op, ok := ownerForPools[dbName]; ok {
		return op
	}
	op, err := db.Connect(context.Background(), u.String(), 4, 5*time.Second)
	if err != nil {
		tb.Fatalf("launchfix: connect owner pool for %s: %v", dbName, err)
	}
	ownerForPools[dbName] = op
	return op
}

// ErrNotPlatformScope mirrors identity.ErrPlatformTransactionScope for the fixture
// CreateTenant (same assertion, same sentinel).
var ErrNotPlatformScope = identity.ErrPlatformTransactionScope

// CreateTenant is the fixture twin of identity.CreateTenant: the same platform-scope
// assertion and slug conflict mapping, but the row is provisioned 'active' by the table
// owner (tx MUST be an owner-role platform-admin transaction). Production code uses
// identity.CreateTenant, which creates pending_launch.
func CreateTenant(ctx context.Context, tx pgx.Tx, name, slug, licensingModel string) (identity.Tenant, error) {
	var platformAdmin, scopedTenant, scopedPlayer *uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid,
		        NULLIF(current_setting('app.tenant_id', true), '')::uuid,
		        NULLIF(current_setting('app.player_account_id', true), '')::uuid`,
	).Scan(&platformAdmin, &scopedTenant, &scopedPlayer); err != nil {
		return identity.Tenant{}, fmt.Errorf("launchfix: read platform admin scope: %w", err)
	}
	if platformAdmin == nil || scopedTenant != nil || scopedPlayer != nil {
		return identity.Tenant{}, ErrNotPlatformScope
	}
	t := identity.Tenant{ID: uuid.New(), Name: name, Slug: slug, LicensingModel: licensingModel, Status: "active"}
	if _, err := tx.Exec(ctx,
		`INSERT INTO tenants (id, name, slug, licensing_model, status) VALUES ($1, $2, $3, $4, 'active')`,
		t.ID, t.Name, t.Slug, t.LicensingModel,
	); err != nil {
		if db.IsUniqueViolation(err) {
			return identity.Tenant{}, identity.ErrSlugTaken
		}
		return identity.Tenant{}, fmt.Errorf("launchfix: create tenant: %w", err)
	}
	return t, nil
}

// CreateBrand is the fixture twin of identity.CreateBrand ('active', owner-provisioned).
func CreateBrand(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, name, slug string) (identity.Brand, error) {
	b := identity.Brand{ID: uuid.New(), TenantID: tenantID, Name: name, Slug: slug, Status: "active"}
	if _, err := tx.Exec(ctx,
		`INSERT INTO brands (id, tenant_id, name, slug, status) VALUES ($1, $2, $3, $4, 'active')`,
		b.ID, b.TenantID, b.Name, b.Slug,
	); err != nil {
		if db.IsUniqueViolation(err) {
			return identity.Brand{}, identity.ErrSlugTaken
		}
		return identity.Brand{}, fmt.Errorf("launchfix: create brand: %w", err)
	}
	return b, nil
}

// Fixture platform operators: one requester and two approvers, three distinct Persons.
// Deterministic ids so concurrent tests converge on the same rows (ON CONFLICT DO NOTHING).
var (
	requesterStaff = uuid.MustParse("f1c70000-0000-4000-8000-000000000001")
	approverStaff  = [2]uuid.UUID{uuid.MustParse("f1c70000-0000-4000-8000-000000000002"), uuid.MustParse("f1c70000-0000-4000-8000-000000000003")}
	operatorPerson = [3]uuid.UUID{uuid.MustParse("f1c70000-0000-4000-8000-0000000000a1"), uuid.MustParse("f1c70000-0000-4000-8000-0000000000a2"), uuid.MustParse("f1c70000-0000-4000-8000-0000000000a3")}
)

// OperatorIDs are the synthetic fixture platform operators (three distinct Persons).
type OperatorIDs struct {
	Requester, Approver1, Approver2 uuid.UUID
	Persons                         [3]uuid.UUID
}

// Operators returns the fixture operators' ids (they exist after SeedOperators or any
// Set*Status call).
func Operators() OperatorIDs {
	return OperatorIDs{Requester: requesterStaff, Approver1: approverStaff[0], Approver2: approverStaff[1], Persons: operatorPerson}
}

// SeedOperators makes sure the fixture operators exist on the TEST_DATABASE_URL database.
func SeedOperators(tb testing.TB) {
	tb.Helper()
	if err := run(context.Background(), ownerURL(tb), func(tx pgx.Tx) error { return ensureOperators(context.Background(), tx) }); err != nil {
		tb.Fatalf("launchfix: seed operators: %v", err)
	}
}

// SeedOperatorsAt is SeedOperators against an explicit owner URL (scratch databases).
func SeedOperatorsAt(tb testing.TB, url string) {
	tb.Helper()
	if err := run(context.Background(), url, func(tx pgx.Tx) error { return ensureOperators(context.Background(), tx) }); err != nil {
		tb.Fatalf("launchfix: seed operators: %v", err)
	}
}

func setPlatform(ctx context.Context, tx pgx.Tx, staff uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', '', true)`); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, staff.String())
	return err
}

func ensureOperators(ctx context.Context, tx pgx.Tx) error {
	if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, requesterStaff.String()); err != nil {
		return err
	}
	for i, p := range operatorPerson {
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1) ON CONFLICT (id) DO NOTHING`, p); err != nil {
			return fmt.Errorf("launchfix: operator person %d: %w", i, err)
		}
	}
	staff := []uuid.UUID{requesterStaff, approverStaff[0], approverStaff[1]}
	for i, s := range staff {
		if _, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id)
			 VALUES ($1, NULL, $2, '!launchfix-not-a-login', 'platform_admin', $3)
			 ON CONFLICT (id) DO NOTHING`,
			s, fmt.Sprintf("launchfix-operator-%d@fixture.invalid", i+1), operatorPerson[i]); err != nil {
			return fmt.Errorf("launchfix: operator staff %d: %w", i, err)
		}
	}
	return nil
}

func actionFor(from, to string) (string, error) {
	switch {
	case from == "pending_launch" && to == "active":
		return "activate", nil
	case from == "active" && to == "suspended":
		return "suspend", nil
	case from == "suspended" && to == "active":
		return "reactivate", nil
	case to == "closed" && from != "closed":
		return "close", nil
	}
	return "", fmt.Errorf("launchfix: no governed action for %s -> %s", from, to)
}

// Hooks lets a test run SQL inside the status-changing transaction: BeforeUpdate just before
// the subject UPDATE, AfterUpdate just after it, Hold after it with the transaction still open.
// A fixture that needs a state the guards would refuse for a reason UNRELATED to launch
// governance (for example closing a tenant that has an open bet, which the GP020 closure gate
// refuses) supplies its own transaction-scoped SQL here, in the _test.go file, so this
// package itself never mentions a trigger disable (internal/tenant/no_trigger_disable_test.go).
type Hooks struct {
	BeforeUpdate func(ctx context.Context, tx pgx.Tx) error
	AfterUpdate  func(ctx context.Context, tx pgx.Tx) error
	Hold         func()
}

// TrySetTenantStatusHooked is TrySetTenantStatus with Hooks (TEST_DATABASE_URL).
func TrySetTenantStatusHooked(ctx context.Context, tenantID uuid.UUID, status string, h Hooks) error {
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		return errors.New("launchfix: TEST_DATABASE_URL is not set")
	}
	return run(ctx, u, func(tx pgx.Tx) error { return flip(ctx, tx, tenantID, nil, status, h) })
}

// flip runs one complete governed status change inside tx, which MUST be a fresh
// transaction on an owner-role connection (it overrides the session GUCs). brandID is
// nil for a tenant subject. The status UPDATE's own errors (for example the GP020
// closure refusal) are returned unwrapped so callers can inspect the *pgconn.PgError.
func flip(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, brandID *uuid.UUID, to string, h Hooks) error {
	if err := ensureOperators(ctx, tx); err != nil {
		return err
	}
	if err := setPlatform(ctx, tx, requesterStaff); err != nil {
		return err
	}
	kind := "tenant"
	var from string
	if brandID == nil {
		if err := tx.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1`, tenantID).Scan(&from); err != nil {
			return fmt.Errorf("launchfix: read tenant status: %w", err)
		}
	} else {
		kind = "brand"
		if err := tx.QueryRow(ctx, `SELECT status FROM brands WHERE id = $1 AND tenant_id = $2`, *brandID, tenantID).Scan(&from); err != nil {
			return fmt.Errorf("launchfix: read brand status: %w", err)
		}
	}
	if from == to {
		return nil
	}
	action, err := actionFor(from, to)
	if err != nil {
		return err
	}
	var reqID uuid.UUID
	var payloadHash string
	var need int
	if err := tx.QueryRow(ctx, `
		INSERT INTO launch_authorisation_requests
		    (tenant_id, subject_kind, brand_id, action, reason_code, readiness_snapshot, readiness_snapshot_hash,
		     licensing_model, licensing_status, responsible_operator_name, jurisdiction_ids)
		VALUES ($1, $2, $3, $4, 'test_fixture', '{}'::jsonb, repeat('0', 64),
		        CASE WHEN $4 IN ('activate','reactivate') THEN 'other_manually_approved' END,
		        CASE WHEN $4 IN ('activate','reactivate') THEN 'conditional' END,
		        CASE WHEN $4 IN ('activate','reactivate') THEN 'launchfix' END,
		        CASE WHEN $4 IN ('activate','reactivate') THEN ARRAY[gen_random_uuid()] ELSE '{}'::uuid[] END)
		RETURNING id, payload_hash, required_approvals`,
		tenantID, kind, brandID, action).Scan(&reqID, &payloadHash, &need); err != nil {
		return fmt.Errorf("launchfix: insert request: %w", err)
	}
	for i := 0; i < need; i++ {
		if err := setPlatform(ctx, tx, approverStaff[i]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO launch_authorisation_approvals (request_id, decision, payload_hash, readiness_snapshot_hash_at_decision, reason_code)
			VALUES ($1, 'approve', $2, repeat('0', 64), 'test_fixture')`, reqID, payloadHash); err != nil {
			return fmt.Errorf("launchfix: insert approval %d: %w", i+1, err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, reqID); err != nil {
		return fmt.Errorf("launchfix: executing: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO launch_status_transitions (tenant_id, subject_kind, brand_id, kind, from_status, to_status, request_id)
		VALUES ($1, $2, $3, 'governed', $4, $5, $6)`, tenantID, kind, brandID, from, to, reqID); err != nil {
		return fmt.Errorf("launchfix: governed transition: %w", err)
	}
	if h.BeforeUpdate != nil {
		if err := h.BeforeUpdate(ctx, tx); err != nil {
			return err
		}
	}
	if brandID == nil {
		// tenants_platform_admin_update: platform principal, app.tenant_id unset.
		if _, err := tx.Exec(ctx, `UPDATE tenants SET status = $1, updated_at = now() WHERE id = $2`, to, tenantID); err != nil {
			return err
		}
	} else {
		// S6 technique: bind app.tenant_id to the subject's own tenant for the brand UPDATE
		// (existing brand_tenant_update policy), then restore.
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE brands SET status = $1, updated_at = now() WHERE id = $2`, to, *brandID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', '', true)`); err != nil {
			return err
		}
	}
	if h.AfterUpdate != nil {
		if err := h.AfterUpdate(ctx, tx); err != nil {
			return err
		}
	}
	if h.Hold != nil {
		// The subject UPDATE has run (and holds the advisory / row locks); the transaction is
		// still open. Concurrency tests block here to stage a race, then return to commit.
		h.Hold()
	}
	if _, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executed' WHERE id = $1`, reqID); err != nil {
		return fmt.Errorf("launchfix: executed: %w", err)
	}
	return nil
}

// ownerURL is the TABLE-OWNER connection string (TEST_DATABASE_URL).
func ownerURL(tb testing.TB) string {
	tb.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		tb.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	return u
}

func run(ctx context.Context, url string, fn func(pgx.Tx) error) error {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return fmt.Errorf("launchfix: connect owner: %w", err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// TrySetTenantStatusAt moves a tenant to status through the governed sequence on the
// database at the owner URL (committed on success). On a refusal (for example GP020) the
// transaction rolls back and the database error is returned.
func TrySetTenantStatusAt(ctx context.Context, url string, tenantID uuid.UUID, status string) error {
	return run(ctx, url, func(tx pgx.Tx) error { return flip(ctx, tx, tenantID, nil, status, Hooks{}) })
}

// TrySetBrandStatusAt is the brand twin (the tenant GUC technique of ADR 0112 S6).
func TrySetBrandStatusAt(ctx context.Context, url string, tenantID, brandID uuid.UUID, status string) error {
	return run(ctx, url, func(tx pgx.Tx) error { return flip(ctx, tx, tenantID, &brandID, status, Hooks{}) })
}

// TrySetTenantStatusOn runs the governed fixture against the database p is connected to (shared
// or scratch), on that database's table-owner pool (OwnerFor).
func TrySetTenantStatusOn(ctx context.Context, tb testing.TB, p *db.Pool, tenantID uuid.UUID, status string) error {
	tb.Helper()
	return runOn(ctx, OwnerFor(tb, p), func(tx pgx.Tx) error { return flip(ctx, tx, tenantID, nil, status, Hooks{}) })
}

// TrySetBrandStatusOn is the brand twin of TrySetTenantStatusOn.
func TrySetBrandStatusOn(ctx context.Context, tb testing.TB, p *db.Pool, tenantID, brandID uuid.UUID, status string) error {
	tb.Helper()
	return runOn(ctx, OwnerFor(tb, p), func(tx pgx.Tx) error { return flip(ctx, tx, tenantID, &brandID, status, Hooks{}) })
}

// TrySetTenantStatusHoldingOn is TrySetTenantStatusHolding against the database p is connected to.
func TrySetTenantStatusHoldingOn(ctx context.Context, tb testing.TB, p *db.Pool, tenantID uuid.UUID, status string, hold func()) error {
	tb.Helper()
	return runOn(ctx, OwnerFor(tb, p), func(tx pgx.Tx) error { return flip(ctx, tx, tenantID, nil, status, Hooks{Hold: hold}) })
}

// TrySetBrandStatusHoldingOn is the brand twin of TrySetTenantStatusHoldingOn.
func TrySetBrandStatusHoldingOn(ctx context.Context, tb testing.TB, p *db.Pool, tenantID, brandID uuid.UUID, status string, hold func()) error {
	tb.Helper()
	return runOn(ctx, OwnerFor(tb, p), func(tx pgx.Tx) error { return flip(ctx, tx, tenantID, &brandID, status, Hooks{Hold: hold}) })
}

func runOn(ctx context.Context, owner *db.Pool, fn func(pgx.Tx) error) error {
	// WithoutTenant opens a plain transaction (no GUC); flip sets every setting it needs itself,
	// transaction-locally. A returned error rolls the whole governed sequence back.
	return owner.WithoutTenant(ctx, func(_ context.Context, tx pgx.Tx) error { return fn(tx) })
}

// TrySetTenantStatusHolding is like TrySetTenantStatus but calls hold() after the status
// UPDATE succeeded and before the transaction commits, with every lock the UPDATE took
// still held. Concurrency tests use it to stage "a status change is in flight" (the
// replacement for a raw UPDATE inside a long-lived transaction).
func TrySetTenantStatusHolding(ctx context.Context, tenantID uuid.UUID, status string, hold func()) error {
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		return errors.New("launchfix: TEST_DATABASE_URL is not set")
	}
	return run(ctx, u, func(tx pgx.Tx) error { return flip(ctx, tx, tenantID, nil, status, Hooks{Hold: hold}) })
}

// TrySetBrandStatusHolding is the brand twin of TrySetTenantStatusHolding.
func TrySetBrandStatusHolding(ctx context.Context, tenantID, brandID uuid.UUID, status string, hold func()) error {
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		return errors.New("launchfix: TEST_DATABASE_URL is not set")
	}
	return run(ctx, u, func(tx pgx.Tx) error { return flip(ctx, tx, tenantID, &brandID, status, Hooks{Hold: hold}) })
}

// TrySetTenantStatus is TrySetTenantStatusAt against TEST_DATABASE_URL (the table owner).
func TrySetTenantStatus(ctx context.Context, tenantID uuid.UUID, status string) error {
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		return errors.New("launchfix: TEST_DATABASE_URL is not set")
	}
	return TrySetTenantStatusAt(ctx, u, tenantID, status)
}

// TrySetBrandStatus is TrySetBrandStatusAt against TEST_DATABASE_URL.
func TrySetBrandStatus(ctx context.Context, tenantID, brandID uuid.UUID, status string) error {
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		return errors.New("launchfix: TEST_DATABASE_URL is not set")
	}
	return TrySetBrandStatusAt(ctx, u, tenantID, brandID, status)
}

// SetTenantStatus is the TEST_DATABASE_URL convenience; it fails the test on error.
func SetTenantStatus(tb testing.TB, tenantID uuid.UUID, status string) {
	tb.Helper()
	if err := TrySetTenantStatusAt(context.Background(), ownerURL(tb), tenantID, status); err != nil {
		tb.Fatalf("launchfix: set tenant %s status %s: %v", tenantID, status, err)
	}
}

// SetBrandStatus is the TEST_DATABASE_URL convenience; it fails the test on error.
func SetBrandStatus(tb testing.TB, tenantID, brandID uuid.UUID, status string) {
	tb.Helper()
	if err := TrySetBrandStatusAt(context.Background(), ownerURL(tb), tenantID, brandID, status); err != nil {
		tb.Fatalf("launchfix: set brand %s status %s: %v", brandID, status, err)
	}
}

// SetTenantStatusAt / SetBrandStatusAt fail the test on error against an explicit
// owner URL (scratch databases).
func SetTenantStatusAt(tb testing.TB, url string, tenantID uuid.UUID, status string) {
	tb.Helper()
	if err := TrySetTenantStatusAt(context.Background(), url, tenantID, status); err != nil {
		tb.Fatalf("launchfix: set tenant %s status %s: %v", tenantID, status, err)
	}
}

func SetBrandStatusAt(tb testing.TB, url string, tenantID, brandID uuid.UUID, status string) {
	tb.Helper()
	if err := TrySetBrandStatusAt(context.Background(), url, tenantID, brandID, status); err != nil {
		tb.Fatalf("launchfix: set brand %s status %s: %v", brandID, status, err)
	}
}
