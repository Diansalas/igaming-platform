//go:build integration

// Grant pin for the runtime role's UPDATE privilege on `brands` (ADR 0095
// section 43.2 follow-up (b), PRH-2 round 14).
//
// RequireActiveForPaymentInitiation (payment_initiation_gate.go, the shared
// H-SEC-5 / H-SEC-11 gate on HTTP deposit and withdrawal initiation) reads the
// brand row with
//
//	SELECT status FROM public.brands WHERE id = $1 AND tenant_id = $2 FOR SHARE
//
// In PostgreSQL a row-locking clause (FOR UPDATE / NO KEY UPDATE / SHARE /
// KEY SHARE) requires the UPDATE privilege on the table (on at least one
// column), and under row-level security the table's UPDATE policy USING
// clause is applied to the locked rows in addition to the SELECT policy. The
// runtime role's UPDATE grant on `brands` is therefore load-bearing: a future
// least-privilege REVOKE of it would make every deposit and withdrawal
// initiation fail (closed). These tests pin that grant, pin that the grant is
// not wider than the documented intended set, prove the UPDATE privilege does
// not open a cross-tenant or tenant-less write path (RLS policy
// brand_tenant_update, migration 0008), and - on a throwaway scratch database
// only - prove the gate fails CLOSED (42501, never "active") if the grant is
// removed, so the positive pin is meaningful.
//
// The same UPDATE dependency applies to every other caller of the FOR SHARE brand
// read, tenant.RequireBrandActive (H(8), ADR 0095 sections 44-45): the SWEEPER
// deposit claim transaction and the cascade-eligible decline transactions (phase C
// and the poll path) in internal/payments, not only HTTP initiation. Their
// fail-closed behaviour under a revoked grant is exercised in
// internal/payments/h8_brand_failclosed_scratch_integration_test.go.
//
// Intended runtime-role grant set (the source of the "exactly" assertions):
// docs/security/runtime-role-separation.md section 3 ("SELECT, INSERT, UPDATE,
// DELETE on every application table ... Nothing else"; "Explicitly do not
// grant ... TRUNCATE ... REFERENCES, TRIGGER on any table") and
// deploy/init-app-role.sql (GRANT / ALTER DEFAULT PRIVILEGES ... SELECT,
// INSERT, UPDATE, DELETE ON TABLES TO igaming_runtime; `brands` has no
// narrowing block there or in any migration).
//
// Requires TEST_DATABASE_URL (owner, fixtures only) and
// TEST_RUNTIME_DATABASE_URL (the runtime role every assertion runs as); the
// scratch-database test additionally requires TEST_ADMIN_DATABASE_URL
// (CREATE/DROP DATABASE only, via internal/testsupport/scratchdb). A skip is
// NOT evidence.
package tenant

import (
	"context"
	"errors"
	"net/url"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// brandGateStmt is, byte for byte, the brand half of
// RequireActiveForPaymentInitiation.
const brandGateStmt = `SELECT status FROM public.brands WHERE id = $1 AND tenant_id = $2 FOR SHARE`

func brandPinOwnerPoolAt(t *testing.T, u string) *db.Pool {
	t.Helper()
	p, err := db.Connect(context.Background(), u, 5, 5*time.Second)
	if err != nil {
		t.Fatalf("connect owner: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func brandPinOwnerPool(t *testing.T) *db.Pool {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping brands grant-pin test")
	}
	return brandPinOwnerPoolAt(t, u)
}

// brandPinRuntimePoolAt connects as the runtime role and proves it is exactly
// igaming_runtime (the name every has_table_privilege below uses) and neither
// superuser nor BYPASSRLS (otherwise RLS assertions would pass for the wrong
// reason).
func brandPinRuntimePoolAt(t *testing.T, u string) *db.Pool {
	t.Helper()
	p, err := db.Connect(context.Background(), u, 5, 5*time.Second)
	if err != nil {
		t.Fatalf("connect runtime role: %v", err)
	}
	t.Cleanup(p.Close)
	var who string
	var super, bypass bool
	if err := p.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_user::text, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&who, &super, &bypass)
	}); err != nil {
		t.Fatalf("inspect runtime role: %v", err)
	}
	if who != "igaming_runtime" || super || bypass {
		t.Fatalf("runtime pool must be igaming_runtime, non-superuser, non-BYPASSRLS; got user=%q super=%v bypassrls=%v", who, super, bypass)
	}
	return p
}

func brandPinRuntimePool(t *testing.T) *db.Pool {
	t.Helper()
	u := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set; skipping brands grant-pin runtime-role test")
	}
	return brandPinRuntimePoolAt(t, u)
}

type brandPinFixture struct {
	tenantID, brandID uuid.UUID
	name              string
}

// seedBrandPinTenant creates an active tenant (platform-admin scope, migration
// 0077) and one active brand of it (tenant scope) through the OWNER pool.
func seedBrandPinTenant(t *testing.T, owner *db.Pool) brandPinFixture {
	t.Helper()
	ctx := context.Background()
	f := brandPinFixture{tenantID: uuid.New(), brandID: uuid.New()}
	f.name = "r14 grant pin brand " + f.brandID.String()[:8]
	if err := owner.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1,$2,'r14 grant pin tenant','under_platform_licence')`,
			f.tenantID, "r14pin-"+f.tenantID.String())
		return err
	}); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	t.Cleanup(func() {
		_ = owner.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, f.tenantID)
			return err
		})
	})
	if err := owner.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name, status) VALUES ($1,$2,$3,$4,'active')`,
			f.brandID, f.tenantID, "r14pin-"+f.brandID.String(), f.name)
		return err
	}); err != nil {
		t.Fatalf("seed brand: %v", err)
	}
	return f
}

// brandRow reads a brand's (name, status, tenant_id) through the owner pool,
// in the brand's own tenant scope (the owner is subject to FORCE RLS too; the
// SELECT policy brand_public_read is USING (true)).
func brandRow(t *testing.T, owner *db.Pool, f brandPinFixture) (name, status string, tenantID uuid.UUID) {
	t.Helper()
	if err := owner.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT name, status, tenant_id FROM brands WHERE id = $1`, f.brandID).Scan(&name, &status, &tenantID)
	}); err != nil {
		t.Fatalf("read brand %s: %v", f.brandID, err)
	}
	return name, status, tenantID
}

func requirePgCode(t *testing.T, what string, err error, code, msgSubstr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected SQLSTATE %s, got success", what, code)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: expected a *pgconn.PgError (SQLSTATE %s), got %T: %v", what, code, err, err)
	}
	if pgErr.Code != code {
		t.Fatalf("%s: expected SQLSTATE %s, got %s: %v", what, code, pgErr.Code, err)
	}
	if !strings.Contains(pgErr.Message, msgSubstr) {
		t.Fatalf("%s: expected message containing %q, got %q", what, msgSubstr, pgErr.Message)
	}
}

// runtimeBrandGrants returns the runtime role's table-level privileges on
// public.brands as recorded in the ACL (information_schema), sorted.
func runtimeBrandGrants(t *testing.T, p *db.Pool) []string {
	t.Helper()
	var got []string
	if err := p.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT privilege_type::text FROM information_schema.role_table_grants
			WHERE table_schema = 'public' AND table_name = 'brands' AND grantee = 'igaming_runtime'`)
		if err != nil {
			return err
		}
		got, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}); err != nil {
		t.Fatalf("read runtime grants on brands: %v", err)
	}
	sort.Strings(got)
	return got
}

// TestBrandsGrantPin_RuntimeHoldsUpdate_GateStatementSucceeds is the POSITIVE
// pin: the runtime role holds UPDATE on brands, holds exactly the documented
// intended set (no TRUNCATE / REFERENCES / TRIGGER), and the gate's exact
// locking read - and the gate itself - succeed for the tenant's own active
// brand.
func TestBrandsGrantPin_RuntimeHoldsUpdate_GateStatementSucceeds(t *testing.T) {
	owner := brandPinOwnerPool(t)
	rt := brandPinRuntimePool(t)
	ctx := context.Background()
	f := seedBrandPinTenant(t, owner)

	// Exactly the intended set (runtime-role-separation.md section 3,
	// init-app-role.sql), no more and no less.
	if got, want := strings.Join(runtimeBrandGrants(t, rt), ","), "DELETE,INSERT,SELECT,UPDATE"; got != want {
		t.Fatalf("igaming_runtime table grants on brands = %q, want exactly %q", got, want)
	}
	want := map[string]bool{
		"SELECT": true, "INSERT": true, "UPDATE": true, "DELETE": true,
		"TRUNCATE": false, "REFERENCES": false, "TRIGGER": false,
	}
	for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
		var has bool
		if err := rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT has_table_privilege('igaming_runtime', 'public.brands', $1)`, priv).Scan(&has)
		}); err != nil {
			t.Fatalf("has_table_privilege(%s): %v", priv, err)
		}
		if has != want[priv] {
			t.Errorf("has_table_privilege('igaming_runtime','brands',%q) = %v, want %v (intended set: SELECT, INSERT, UPDATE, DELETE only)", priv, has, want[priv])
		}
	}

	if err := rt.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var status string
		if err := tx.QueryRow(ctx, brandGateStmt, f.brandID, f.tenantID).Scan(&status); err != nil {
			t.Fatalf("gate statement as runtime role for the tenant's own brand: %v", err)
		}
		if status != "active" {
			t.Fatalf("gate statement returned status %q, want active", status)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The gate itself, end to end, as the runtime role.
	if err := rt.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return RequireActiveForPaymentInitiation(ctx, tx, f.tenantID, f.brandID)
	}); err != nil {
		t.Fatalf("RequireActiveForPaymentInitiation for an active tenant/brand as the runtime role: %v", err)
	}
}

// TestBrandsGrantPin_UpdatePrivilegeDoesNotOpenCrossTenantOrTenantlessWrites
// proves the UPDATE grant is RLS-scoped: tenant A's runtime session cannot
// update, re-tenant, or lock tenant B's brand, and a runtime session with no
// tenant context cannot update any brand. Mechanism: policy
// brand_tenant_update (migration 0008) USING / WITH CHECK
// tenant_id = NULLIF(current_setting('app.tenant_id', true), <empty string>)::uuid under
// FORCE ROW LEVEL SECURITY; rows failing USING are silently filtered (0 rows,
// no error), a new row failing WITH CHECK is refused with 42501.
func TestBrandsGrantPin_UpdatePrivilegeDoesNotOpenCrossTenantOrTenantlessWrites(t *testing.T) {
	owner := brandPinOwnerPool(t)
	rt := brandPinRuntimePool(t)
	ctx := context.Background()
	a := seedBrandPinTenant(t, owner)
	b := seedBrandPinTenant(t, owner)

	// The mechanism exists as described (not a static guard on the whole
	// schema - just that the test is attributing the behaviour correctly).
	var force bool
	var qual, check string
	if err := rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT relforcerowsecurity FROM pg_class WHERE oid = 'public.brands'::regclass`).Scan(&force); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT coalesce(qual, ''), coalesce(with_check, '') FROM pg_policies
			WHERE schemaname = 'public' AND tablename = 'brands' AND policyname = 'brand_tenant_update' AND cmd = 'UPDATE'`).Scan(&qual, &check)
	}); err != nil {
		t.Fatalf("inspect brands RLS: %v", err)
	}
	if !force || !strings.Contains(qual, "app.tenant_id") || !strings.Contains(check, "app.tenant_id") {
		t.Fatalf("brands must be FORCE RLS with brand_tenant_update keyed on app.tenant_id; force=%v qual=%q with_check=%q", force, qual, check)
	}

	bName, bStatus, _ := brandRow(t, owner, b)

	// (a) Cross-tenant: tenant A's runtime session against tenant B's brand.
	if err := rt.WithTenant(ctx, a.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for _, q := range []string{
			`UPDATE brands SET name = 'r14 cross-tenant pwned' WHERE id = $1`,
			`UPDATE brands SET status = 'suspended' WHERE id = $1`,
		} {
			tag, err := tx.Exec(ctx, q, b.brandID)
			if err != nil {
				t.Fatalf("cross-tenant %q: expected silent RLS filtering (0 rows), got error %v", q, err)
			}
			if tag.RowsAffected() != 0 {
				t.Fatalf("cross-tenant %q affected %d rows of tenant B's brand, want 0", q, tag.RowsAffected())
			}
		}
		// The row IS visible to a plain read (brand_public_read USING (true)) ...
		var plain string
		if err := tx.QueryRow(ctx, `SELECT status FROM brands WHERE id = $1`, b.brandID).Scan(&plain); err != nil {
			t.Fatalf("plain read of tenant B's brand (public read policy) failed: %v", err)
		}
		// ... but NOT to a locking read: the UPDATE policy's USING applies to
		// FOR SHARE, so the row is filtered out - whichever tenant id the
		// predicate names.
		var s string
		for _, args := range [][]any{{b.brandID, b.tenantID}, {b.brandID, a.tenantID}} {
			if err := tx.QueryRow(ctx, brandGateStmt, args...).Scan(&s); !errors.Is(err, pgx.ErrNoRows) {
				t.Fatalf("gate statement on tenant B's brand from tenant A's session (args %v): want no row, got status=%q err=%v", args, s, err)
			}
		}
		if err := tx.QueryRow(ctx, `SELECT status FROM brands WHERE id = $1 FOR SHARE`, b.brandID).Scan(&s); !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("FOR SHARE on tenant B's brand without a tenant predicate: want no row, got status=%q err=%v", s, err)
		}
		// The gate refuses it as not-active (fail closed), never "active".
		if err := RequireActiveForPaymentInitiation(ctx, tx, a.tenantID, b.brandID); !errors.Is(err, ErrNotActiveForPaymentInitiation) {
			t.Fatalf("gate for tenant A with tenant B's brand: want ErrNotActiveForPaymentInitiation, got %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n, s, tid := brandRow(t, owner, b); n != bName || s != bStatus || tid != b.tenantID {
		t.Fatalf("tenant B's brand changed after cross-tenant attempts: name=%q status=%q tenant=%s (was %q %q %s)", n, s, tid, bName, bStatus, b.tenantID)
	}

	// (b) Re-tenanting A's own brand into tenant B: WITH CHECK refuses (42501).
	err := rt.WithTenant(ctx, a.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE brands SET tenant_id = $2 WHERE id = $1`, a.brandID, b.tenantID)
		return err
	})
	requirePgCode(t, "re-tenant own brand into another tenant", err, "42501", `new row violates row-level security policy for table "brands"`)
	if _, _, tid := brandRow(t, owner, a); tid != a.tenantID {
		t.Fatalf("tenant A's brand was re-tenanted to %s", tid)
	}

	aName, aStatus, _ := brandRow(t, owner, a)

	// (c) No tenant context. Both shapes a pooled connection can present:
	// app.tenant_id never set (NULL) and set to '' (a reused connection after
	// a transaction-local set_config). NULLIF(...)::uuid is NULL in both, so
	// USING is NULL -> every row filtered: 0 rows, no error.
	for _, mode := range []string{"unset", "empty"} {
		tx, err := rt.Raw().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if mode == "empty" {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', '', true)`); err != nil {
				_ = tx.Rollback(ctx)
				t.Fatal(err)
			}
		}
		var setting *string
		if err := tx.QueryRow(ctx, `SELECT NULLIF(current_setting('app.tenant_id', true), '')`).Scan(&setting); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		if setting != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s: expected no tenant context, app.tenant_id=%q", mode, *setting)
		}
		for _, q := range []string{
			`UPDATE brands SET name = 'r14 tenantless pwned' WHERE id = ANY($1)`,
			`UPDATE brands SET status = 'closed' WHERE id = ANY($1)`,
		} {
			tag, err := tx.Exec(ctx, q, []uuid.UUID{a.brandID, b.brandID})
			if err != nil {
				_ = tx.Rollback(ctx)
				t.Fatalf("%s: tenantless %q: expected silent RLS filtering (0 rows), got %v", mode, q, err)
			}
			if tag.RowsAffected() != 0 {
				_ = tx.Rollback(ctx)
				t.Fatalf("%s: tenantless %q affected %d rows, want 0", mode, q, tag.RowsAffected())
			}
		}
		// Unbounded: no brand at all is updatable without tenant context.
		tag, err := tx.Exec(ctx, `UPDATE brands SET updated_at = updated_at`)
		if err != nil || tag.RowsAffected() != 0 {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s: tenantless unbounded UPDATE of brands: rows=%d err=%v, want 0 rows", mode, tag.RowsAffected(), err)
		}
		var s string
		if err := tx.QueryRow(ctx, brandGateStmt, a.brandID, a.tenantID).Scan(&s); !errors.Is(err, pgx.ErrNoRows) {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s: tenantless gate statement: want no row, got status=%q err=%v", mode, s, err)
		}
		// Commit (not roll back) so the row-unchanged check below is real.
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if n, s, _ := brandRow(t, owner, a); n != aName || s != aStatus {
		t.Fatalf("tenant A's brand changed after tenantless attempts: %q %q", n, s)
	}
	if n, s, _ := brandRow(t, owner, b); n != bName || s != bStatus {
		t.Fatalf("tenant B's brand changed after tenantless attempts: %q %q", n, s)
	}
}

// TestBrandsGrantPin_GateFailsClosedWithoutUpdate_ScratchOnly makes the
// positive pin meaningful: on a THROWAWAY scratch database (created and
// dropped by internal/testsupport/scratchdb; no role, password or role
// attribute is touched - only this scratch database's object ACL), the table
// owner REVOKEs UPDATE on brands from igaming_runtime, and the gate's
// locking read is then refused with 42501, the gate returns that error (it
// never reports the brand as active), while a plain read still works - so the
// dependency is exactly the FOR SHARE clause on the UPDATE privilege.
func TestBrandsGrantPin_GateFailsClosedWithoutUpdate_ScratchOnly(t *testing.T) {
	rtBase := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if rtBase == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set; skipping brands grant-pin scratch test")
	}
	scratchURL := scratchdb.New(t, "r14brandpin_")
	su, err := url.Parse(scratchURL)
	if err != nil {
		t.Fatal(err)
	}
	dbName := strings.TrimPrefix(su.Path, "/")
	ru, err := url.Parse(rtBase)
	if err != nil {
		t.Fatal(err)
	}
	ru.Path = su.Path

	ctx := context.Background()
	owner := brandPinOwnerPoolAt(t, scratchURL)
	// The runtime role's grants on the scratch database (the same pattern as
	// internal/adjustment/temp_revoke_integration_test.go; database/schema
	// object grants only, no role is created or altered).
	for _, stmt := range []string{
		`GRANT CONNECT ON DATABASE ` + pgx.Identifier{dbName}.Sanitize() + ` TO igaming_runtime`,
		`GRANT USAGE ON SCHEMA public TO igaming_runtime`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO igaming_runtime`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO igaming_runtime`,
	} {
		if _, err := owner.Raw().Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, err := owner.MigrateUp(ctx, "../../migrations"); err != nil {
		t.Fatalf("migrate scratch up: %v", err)
	}
	f := seedBrandPinTenant(t, owner)
	rt := brandPinRuntimePoolAt(t, ru.String())

	gate := func() (string, error) {
		var s string
		err := rt.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, brandGateStmt, f.brandID, f.tenantID).Scan(&s)
		})
		return s, err
	}
	hasUpdate := func() bool {
		var has bool
		if err := rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT has_table_privilege('igaming_runtime', 'public.brands', 'UPDATE')`).Scan(&has)
		}); err != nil {
			t.Fatal(err)
		}
		return has
	}

	// Control: with the intended grant, the gate statement works here too.
	if !hasUpdate() {
		t.Fatal("control: igaming_runtime lacks UPDATE on the scratch brands table before the revoke")
	}
	if s, err := gate(); err != nil || s != "active" {
		t.Fatalf("control: gate statement before the revoke: status=%q err=%v", s, err)
	}

	// Remove the grant (scratch database only; dropped on cleanup).
	if _, err := owner.Raw().Exec(ctx, `REVOKE UPDATE ON public.brands FROM igaming_runtime`); err != nil {
		t.Fatalf("revoke on scratch: %v", err)
	}
	if hasUpdate() {
		t.Fatal("REVOKE on the scratch database did not take effect")
	}

	s, err := gate()
	requirePgCode(t, "gate statement after REVOKE UPDATE", err, "42501", "permission denied for table brands")
	if s != "" {
		t.Fatalf("gate statement after REVOKE returned a status %q", s)
	}
	// The gate itself fails closed: an error carrying the 42501, never nil.
	gateErr := rt.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return RequireActiveForPaymentInitiation(ctx, tx, f.tenantID, f.brandID)
	})
	requirePgCode(t, "RequireActiveForPaymentInitiation after REVOKE UPDATE", gateErr, "42501", "permission denied for table brands")
	// The dependency is the locking clause, not SELECT: a plain read still works.
	if err := rt.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var st string
		return tx.QueryRow(ctx, `SELECT status FROM public.brands WHERE id = $1 AND tenant_id = $2`, f.brandID, f.tenantID).Scan(&st)
	}); err != nil {
		t.Fatalf("plain SELECT after REVOKE UPDATE must still work (only FOR SHARE needs UPDATE): %v", err)
	}
}
