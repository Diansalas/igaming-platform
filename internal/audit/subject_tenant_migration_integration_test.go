//go:build integration

// PRH-2 G1 (ADR 0104 §7 "MIG"): migration 0109's up/down/up round trip and
// its two down-refusal preconditions (a subject_tenant_id row present; a
// non-NULL staff_users.display_name), on an isolated scratch database -
// migration_checksum/RLS tests elsewhere in this codebase use the same
// scratchdb pattern (see internal/payments/migration_0101_integration_test.go).
package audit

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

func m0109RealMigrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// m0109Version reads the on-disk filename rather than hard-coding 109, so
// a rename/renumbering at merge (see this migration's own up.sql numbering
// note) fails loudly here instead of silently testing the wrong file.
func m0109Version(t *testing.T) int64 {
	t.Helper()
	dir := m0109RealMigrationsDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "0109_tenant_visible_platform_audit.up.sql") {
			return 109
		}
	}
	t.Fatal("no 0109_tenant_visible_platform_audit.up.sql found")
	return 0
}

// m0109ScratchThrough0109 copies the on-disk migration files numbered up
// to and including 0109 (never a later one) into a fresh temp dir and
// migrates a new scratch database up through them, so "down one step"
// always rolls back exactly 0109 however many later migrations exist on
// disk (the migration_0099Scratch pattern in internal/casino). The
// returned dir must be used for every subsequent MigrateDown/MigrateUp.
func m0109ScratchThrough0109(t *testing.T, prefix string) (*db.Pool, string) {
	t.Helper()
	v := m0109Version(t)
	src := m0109RealMigrationsDir(t)
	dir := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || len(name) < 4 {
			continue
		}
		n, perr := strconv.ParseInt(name[:4], 10, 64)
		if perr != nil || n > v {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(src, name))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if werr := os.WriteFile(filepath.Join(dir, name), b, 0o600); werr != nil {
			t.Fatal(werr)
		}
	}
	url := scratchdb.New(t, prefix)
	pool, err := db.Connect(context.Background(), url, 10, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	t.Cleanup(pool.Close)
	applied, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("migrate scratch up through %d: %v", v, err)
	}
	if len(applied) == 0 || applied[len(applied)-1] != v {
		t.Fatalf("expected %d to be the last applied migration, got %v", v, applied)
	}
	return pool, dir
}

func m0109SeedTenant(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model) VALUES ($1, $2, $3, 'under_platform_licence')`,
			id, "m0109 tenant "+id.String(), "m0109-"+id.String())
		return err
	}); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return id
}

func m0109SeedPlatformAdmin(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, NULL, $2, 'x', 'platform_admin')`,
			id, "m0109-admin-"+id.String()+"@example.com")
		return err
	}); err != nil {
		t.Fatalf("seed platform admin: %v", err)
	}
	return id
}

func TestMigration0109_UpDownUpRoundTrip_CleanDB(t *testing.T) {
	pool, migDir := m0109ScratchThrough0109(t, "m0109rt_")

	assertColumnExists := func(want bool, label string) {
		var n int
		if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name='audit_log' AND column_name='subject_tenant_id'`).Scan(&n)
		}); err != nil {
			t.Fatalf("%s: query columns: %v", label, err)
		}
		if want && n != 1 {
			t.Fatalf("%s: expected subject_tenant_id present, found %d", label, n)
		}
		if !want && n != 0 {
			t.Fatalf("%s: expected subject_tenant_id absent, found %d", label, n)
		}
	}
	assertColumnExists(true, "after up")

	down, err := pool.MigrateDown(context.Background(), migDir, 1)
	v := m0109Version(t)
	if err != nil || len(down) != 1 || down[0] != v {
		t.Fatalf("down must roll back exactly %d: %v %v", v, down, err)
	}
	assertColumnExists(false, "after down")

	if _, err := pool.MigrateUp(context.Background(), migDir); err != nil {
		t.Fatalf("re-up after down: %v", err)
	}
	assertColumnExists(true, "after re-up")
}

func TestMigration0109_Down_RefusesWhenSubjectTenantRowExists(t *testing.T) {
	pool, migDir := m0109ScratchThrough0109(t, "m0109subj_")
	tenantID := m0109SeedTenant(t, pool)
	adminID := m0109SeedPlatformAdmin(t, pool)

	if err := pool.WithPlatformAdmin(context.Background(), adminID, func(ctx context.Context, tx pgx.Tx) error {
		return Record(ctx, tx, Entry{
			ActorType: ActorStaff, ActorID: adminID, Action: "test.subject_row", Outcome: OutcomeSuccess,
			SubjectTenantID: tenantID,
		})
	}); err != nil {
		t.Fatalf("seed subject row: %v", err)
	}

	if _, err := pool.MigrateDown(context.Background(), migDir, 1); err == nil {
		t.Fatal("expected the down migration to refuse with a subject_tenant_id row present")
	} else if !strings.Contains(err.Error(), "subject_tenant_id set") {
		t.Fatalf("expected the subject_tenant_id refusal message, got: %v", err)
	}
}

func TestMigration0109_Down_RefusesWhenDisplayNameSet(t *testing.T) {
	pool, migDir := m0109ScratchThrough0109(t, "m0109name_")
	adminID := m0109SeedPlatformAdmin(t, pool)

	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET display_name = 'Alice Admin' WHERE id = $1`, adminID)
		return err
	}); err != nil {
		t.Fatalf("seed display_name: %v", err)
	}

	if _, err := pool.MigrateDown(context.Background(), migDir, 1); err == nil {
		t.Fatal("expected the down migration to refuse with a non-NULL display_name present")
	} else if !strings.Contains(err.Error(), "display_name set") {
		t.Fatalf("expected the display_name refusal message, got: %v", err)
	}
}

// m0109SeedTenantStaff seeds a TENANT-scoped (not platform) staff_users
// row for tenantID.
func m0109SeedTenantStaff(t *testing.T, pool *db.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, $2, $3, 'x', 'tenant_admin')`,
			id, tenantID, "m0109-tenant-staff-"+id.String()+"@example.com")
		return err
	}); err != nil {
		t.Fatalf("seed tenant staff: %v", err)
	}
	return id
}

// TestMigration0109_Down_RefusesWhenTenantStaffDisplayNameSet is G1-C1
// (security review, 2026-09-28): staff_users is FORCE ROW LEVEL SECURITY,
// and a connection with no app.tenant_id set (the shape the down
// migration's own EXISTS check originally ran under) can see ONLY
// platform staff (tenant_id IS NULL), per dual_scope_isolation (migration
// 0011) - so a check that only ever looked with no GUC set would be
// blind to a TENANT staff member's display_name and let the column be
// dropped while real tenant data still existed. This test uses a tenant
// (not platform) staff row specifically to catch that gap; the fix loops
// over every tenant, setting app.tenant_id for each pass.
func TestMigration0109_Down_RefusesWhenTenantStaffDisplayNameSet(t *testing.T) {
	pool, migDir := m0109ScratchThrough0109(t, "m0109tstaff_")
	tenantID := m0109SeedTenant(t, pool)
	staffID := m0109SeedTenantStaff(t, pool, tenantID)

	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET display_name = 'Tenant Staffer' WHERE id = $1`, staffID)
		return err
	}); err != nil {
		t.Fatalf("seed tenant staff display_name: %v", err)
	}

	if _, err := pool.MigrateDown(context.Background(), migDir, 1); err == nil {
		t.Fatal("expected the down migration to refuse with a TENANT staff display_name present")
	} else if !strings.Contains(err.Error(), "display_name set") {
		t.Fatalf("expected the display_name refusal message, got: %v", err)
	}
}

func TestMigration0109_Down_SucceedsWhenNeitherPresent(t *testing.T) {
	pool, migDir := m0109ScratchThrough0109(t, "m0109clean_")
	if _, err := pool.MigrateDown(context.Background(), migDir, 1); err != nil {
		t.Fatalf("expected the down migration to succeed with no subject rows/display names, got: %v", err)
	}
}

func TestM0109Version_Sanity(t *testing.T) {
	if v := strconv.FormatInt(m0109Version(t), 10); v != "109" {
		t.Fatalf("expected version 109, got %s", v)
	}
}
