//go:build integration

// Migration mechanics for 0095 (Stage 10.3, KYC-REASON-BOUND-1): the
// up-migration's pre-flight normalization of pre-existing over-length/
// control-character kyc_verifications.reason rows (run under FORCE ROW
// LEVEL SECURITY, per-tenant, via app.tenant_id - never disabling FORCE,
// mirroring the file's own header comment and the migration 0048 lesson),
// the CHECK constraint it then adds, and the down migration's clean
// removal. Runs against its own throwaway database built from the real
// migrations directory (mirrors internal/ledger/migration_0048_
// integration_test.go's own pattern exactly) so the shared test database
// is never polluted with a deliberately-oversized seeded row.
package kyc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// migration0095MigrationsDir resolves the real migrations directory
// relative to this package. Read only, never written.
func migration0095MigrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolve migrations dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "0095_kyc_verification_reason_bound.up.sql")); err != nil {
		t.Fatalf("migration 0095 not found in %s: %v", dir, err)
	}
	return dir
}

// stagedMigrations0095 copies the real migrations into a temp directory,
// holding 0095's own up/down files back, and returns the directory plus a
// function that adds them back in for a later MigrateUp call - the only
// way to observe the pre-flight's behavior against a genuinely pre-0095
// database (a bad row seeded before the CHECK exists).
func stagedMigrations0095(t *testing.T) (dir string, addMigration0095 func()) {
	t.Helper()
	src := migration0095MigrationsDir(t)
	dir = t.TempDir()

	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var held []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		if strings.HasPrefix(e.Name(), "0095_") {
			held = append(held, e.Name())
			continue
		}
		if migrationAfter0095(e.Name()) {
			// Later, unrelated migrations (0096+, Stage 10.3 W2a onward) are
			// never part of this test's scenario: keeping them out keeps
			// 0095 the chain's tip here whatever lands after it.
			continue
		}
		copyMigration0095File(t, src, dir, e.Name())
	}
	if len(held) != 2 {
		t.Fatalf("expected to hold back exactly 2 files (up+down) for migration 0095, held %v", held)
	}
	return dir, func() {
		for _, name := range held {
			copyMigration0095File(t, src, dir, name)
		}
	}
}

// migrationAfter0095 reports whether a migration file's version is above
// 0095.
func migrationAfter0095(name string) bool {
	if len(name) < 4 {
		return false
	}
	v, err := strconv.Atoi(name[:4])
	return err == nil && v > 95
}

// migrationsThrough0095 copies every migration up to and including 0095
// into a temp directory, so MigrateDown(dir, 1) targets 0095 itself.
func migrationsThrough0095(t *testing.T) string {
	t.Helper()
	src := migration0095MigrationsDir(t)
	dir := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") || migrationAfter0095(e.Name()) {
			continue
		}
		copyMigration0095File(t, src, dir, e.Name())
	}
	return dir
}

func copyMigration0095File(t *testing.T, src, dst, name string) {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(src, name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(dst, name), content, 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func migration0095ScratchPool(t *testing.T, databaseURL string) *db.Pool {
	t.Helper()
	pool, err := db.Connect(context.Background(), databaseURL, 5, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect to scratch database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// seedFixture095 mirrors kyc_integration_test.go's own seedFixture, usable
// against a scratch pool built by this file (the fixture helpers make no
// assumption about which database they run against).
func seedFixture095(t *testing.T, pool *db.Pool) fixture {
	t.Helper()
	var f fixture
	f.tenantID = uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'Migration 0095 Test Tenant', 'under_platform_licence')`,
			f.tenantID, "t95-"+f.tenantID.String()[:8]); err != nil {
			return err
		}
		p, err := identity.CreatePerson(ctx, tx)
		f.personID = p.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant/person: %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		f.brandID = uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'Migration 0095 Test Brand')`,
			f.brandID, f.tenantID, "b95-"+f.brandID.String()[:8]); err != nil {
			return err
		}
		f.playerID = uuid.New()
		_, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			f.playerID, f.tenantID, f.brandID, f.personID, f.playerID.String()+"@example.com")
		return err
	})
	if err != nil {
		t.Fatalf("seed brand/player: %v", err)
	}
	return f
}

func seedVerificationWithRawReason(t *testing.T, pool *db.Pool, f fixture, reason string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id, reason)
			 VALUES ($1, $2, $3, $4, $5, 'rejected', 'mock', $6)`,
			id, f.tenantID, f.brandID, f.playerID, f.personID, reason,
		)
		return err
	})
	if err != nil {
		t.Fatalf("seed verification with raw reason: %v", err)
	}
	return id
}

// TestMigration0095_PreflightNormalizesOversizedRowsAcrossTenantsBeforeConstraint
// is the migration pre-flight test: TWO tenants each get one verification
// row seeded with an over-length, control/bidi-character-laden reason
// (bypassing NormalizeReason entirely, via direct SQL - exactly what a
// pre-0095 database could hold), migration 0095 is then applied, and the
// up-migration must SUCCEED (not refuse) - the pre-flight normalizes both
// tenants' rows, not just one, proving the per-tenant app.tenant_id loop
// actually reaches every tenant, not merely the first.
func TestMigration0095_PreflightNormalizesOversizedRowsAcrossTenantsBeforeConstraint(t *testing.T) {
	scratchURL := scratchdb.New(t, "kyc095_")
	pool := migration0095ScratchPool(t, scratchURL)
	dir, addMigration0095 := stagedMigrations0095(t)

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up through the chain with 0095 held back: %v", err)
	}

	fA := seedFixture095(t, pool)
	fB := seedFixture095(t, pool)

	controlLaden := "\r\x1b[31mFAKE\x1b[0m\u202Eevil"
	hugeReasonA := controlLaden + strings.Repeat("A", 4096) + controlLaden
	hugeReasonB := controlLaden + strings.Repeat("B", 4096) + controlLaden

	idA := seedVerificationWithRawReason(t, pool, fA, hugeReasonA)
	idB := seedVerificationWithRawReason(t, pool, fB, hugeReasonB)

	addMigration0095()
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migration 0095 must succeed (pre-flight must normalize, not refuse) with over-length rows in TWO tenants present: %v", err)
	}

	readReason := func(tenantID, id uuid.UUID) string {
		var reason string
		err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT reason FROM kyc_verifications WHERE id = $1`, id).Scan(&reason)
		})
		if err != nil {
			t.Fatalf("read normalized reason: %v", err)
		}
		return reason
	}

	for _, tc := range []struct {
		name     string
		tenantID uuid.UUID
		id       uuid.UUID
		original string
	}{
		{"tenant A", fA.tenantID, idA, hugeReasonA},
		{"tenant B", fB.tenantID, idB, hugeReasonB},
	} {
		got := readReason(tc.tenantID, tc.id)
		if got == tc.original {
			t.Fatalf("%s: expected the pre-flight to normalize the over-length row, got the raw value verbatim", tc.name)
		}
		if len(got) > 512 {
			t.Fatalf("%s: expected the normalized reason to be at most 512 bytes, got %d", tc.name, len(got))
		}
		for _, bad := range []string{"\r", "\x1b"} {
			if strings.Contains(got, bad) {
				t.Fatalf("%s: expected control character %q stripped by the pre-flight, got %q", tc.name, bad, got)
			}
		}
	}

	// The CHECK itself is now active: a fresh oversized insert (bypassing
	// NormalizeReason on purpose, to prove the DATABASE bound, not just
	// the application one) must be refused.
	err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id, reason)
			 VALUES ($1, $2, $3, $4, $5, 'rejected', 'mock', $6)`,
			uuid.New(), fA.tenantID, fA.brandID, fA.playerID, fA.personID, strings.Repeat("z", 513),
		)
		return err
	})
	var pgErr *pgconn.PgError
	if err == nil {
		t.Fatal("expected the 512-byte CHECK constraint to refuse a fresh 513-byte reason")
	}
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
		t.Fatalf("expected SQLSTATE 23514 (check_violation), got %v", err)
	}
}

// TestMigration0095_UpDownUpRoundTrip is the required up/down/
// reversibility test: a clean database (no pre-existing rows) migrates up
// through 0095, then down (dropping the CHECK), then up again cleanly -
// symmetric, no data transformation needed on the way down, per the
// migration's own down-file comment.
func TestMigration0095_UpDownUpRoundTrip(t *testing.T) {
	scratchURL := scratchdb.New(t, "kyc095rt_")
	pool := migration0095ScratchPool(t, scratchURL)
	dir := migrationsThrough0095(t)

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up the chain through 0095: %v", err)
	}
	if !constraintExists(t, pool, "kyc_verifications_reason_bound") {
		t.Fatal("expected the kyc_verifications_reason_bound CHECK to exist after migrating up")
	}

	if _, err := pool.MigrateDown(context.Background(), dir, 1); err != nil {
		t.Fatalf("down migration 0095 on a clean database: %v", err)
	}
	if constraintExists(t, pool, "kyc_verifications_reason_bound") {
		t.Fatal("expected the kyc_verifications_reason_bound CHECK to be dropped after rolling back migration 0095")
	}

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("re-applying migration 0095 after a clean rollback: %v", err)
	}
	if !constraintExists(t, pool, "kyc_verifications_reason_bound") {
		t.Fatal("expected the kyc_verifications_reason_bound CHECK to exist again after re-applying migration 0095")
	}
}

func constraintExists(t *testing.T, pool *db.Pool, name string) bool {
	t.Helper()
	var exists bool
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = $1)`, name).Scan(&exists)
	})
	if err != nil {
		t.Fatalf("check constraint %s existence: %v", name, err)
	}
	return exists
}
