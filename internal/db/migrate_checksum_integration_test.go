//go:build integration

// PLAT-MIGDRIFT-1 (docs/architecture/38-deployment-architecture.md §3/§4):
// integration coverage for schema_migrations.checksum recording and the
// `migrate verify` detection path (db.VerifyMigrations). Runs against its
// own scratch database with a small, synthetic migration set - not the
// real 83-migration chain - so these tests stay fast and exercise the
// checksum mechanics in isolation, exactly the same pattern
// internal/ledger/migration_0048_integration_test.go uses for its own
// migration-mechanics coverage.
package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

func writeMigrationPair(t *testing.T, dir string, version int, name, upSQL, downSQL string) {
	t.Helper()
	base := fmt.Sprintf("%04d_%s", version, name)
	if err := os.WriteFile(filepath.Join(dir, base+".up.sql"), []byte(upSQL), 0o644); err != nil {
		t.Fatalf("write up file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, base+".down.sql"), []byte(downSQL), 0o644); err != nil {
		t.Fatalf("write down file: %v", err)
	}
}

func sha256Hex(t *testing.T, content string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// checksumScratchDatabase creates a throwaway database, dropped on
// cleanup - delegates to the shared internal/testsupport/scratchdb helper
// (Stage 10 W0), which imports nothing from this package.
func checksumScratchDatabase(t *testing.T) string {
	t.Helper()
	return scratchdb.New(t, "dbchecksum_")
}

func checksumScratchPool(t *testing.T, databaseURL string) *Pool {
	t.Helper()
	pool, err := Connect(context.Background(), databaseURL, 5, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect to scratch database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Applying a migration must record a checksum matching a fresh SHA-256 of
// its up-file content.
func TestMigrateUp_RecordsCorrectChecksum(t *testing.T) {
	dir := t.TempDir()
	upSQL := "CREATE TABLE checksum_t1 (id INT);"
	writeMigrationPair(t, dir, 1, "create_t1", upSQL, "DROP TABLE checksum_t1;")

	pool := checksumScratchPool(t, checksumScratchDatabase(t))
	ctx := context.Background()
	if _, err := pool.MigrateUp(ctx, dir); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}

	var checksum *string
	err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version = 1`).Scan(&checksum)
	})
	if err != nil {
		t.Fatalf("read checksum: %v", err)
	}
	if checksum == nil {
		t.Fatal("expected a non-NULL checksum for a migration applied by the current code")
	}
	if want := sha256Hex(t, upSQL); *checksum != want {
		t.Errorf("expected checksum %s, got %s", want, *checksum)
	}
}

// VerifyMigrations must report clean (OK, no results other than
// MigrationCheckOK, no gaps) on an untouched chain.
func TestVerifyMigrations_PassesOnUntouchedChain(t *testing.T) {
	dir := t.TempDir()
	writeMigrationPair(t, dir, 1, "create_t1", "CREATE TABLE checksum_t1 (id INT);", "DROP TABLE checksum_t1;")
	writeMigrationPair(t, dir, 2, "create_t2", "CREATE TABLE checksum_t2 (id INT);", "DROP TABLE checksum_t2;")

	pool := checksumScratchPool(t, checksumScratchDatabase(t))
	ctx := context.Background()
	if _, err := pool.MigrateUp(ctx, dir); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}

	report, err := pool.VerifyMigrations(ctx, dir)
	if err != nil {
		t.Fatalf("VerifyMigrations: %v", err)
	}
	if !report.OK() {
		t.Fatalf("expected a clean report, got: %+v", report)
	}
	if len(report.Results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(report.Results))
	}
	for _, res := range report.Results {
		if res.Status != MigrationCheckOK {
			t.Errorf("version %d: expected status ok, got %s (%s)", res.Version, res.Status, res.Detail)
		}
	}
}

// The core defect this feature exists to catch: a historical migration's
// up-file edited in place AFTER it was applied. VerifyMigrations must
// report a clear, actionable mismatch naming the affected version.
func TestVerifyMigrations_DetectsEditedMigrationAfterApply(t *testing.T) {
	dir := t.TempDir()
	originalSQL := "CREATE TABLE checksum_t1 (id INT);"
	writeMigrationPair(t, dir, 1, "create_t1", originalSQL, "DROP TABLE checksum_t1;")

	pool := checksumScratchPool(t, checksumScratchDatabase(t))
	ctx := context.Background()
	if _, err := pool.MigrateUp(ctx, dir); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}

	// Simulate the exact real-world defect: the committed migration file
	// is amended AFTER this database already applied the original
	// content (this codebase's own documented MKT-MIG76-1 practice,
	// misapplied to an already-migrated environment).
	editedSQL := "CREATE TABLE checksum_t1 (id INT, extra_column TEXT);"
	if err := os.WriteFile(filepath.Join(dir, "0001_create_t1.up.sql"), []byte(editedSQL), 0o644); err != nil {
		t.Fatalf("edit migration file: %v", err)
	}

	report, err := pool.VerifyMigrations(ctx, dir)
	if err != nil {
		t.Fatalf("VerifyMigrations: %v", err)
	}
	if report.OK() {
		t.Fatal("expected VerifyMigrations to report a problem after the file was edited post-apply, got a clean report")
	}
	if len(report.Results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(report.Results))
	}
	res := report.Results[0]
	if res.Status != MigrationCheckMismatch {
		t.Fatalf("expected status mismatch, got %s", res.Status)
	}
	if res.Version != 1 {
		t.Errorf("expected the mismatch to name version 1, got %d", res.Version)
	}
	if !strings.Contains(res.Detail, "edited after being applied") {
		t.Errorf("expected an actionable message naming the defect, got: %s", res.Detail)
	}
}

// A gap in the on-disk version sequence (e.g. version 2 missing between 1
// and 3) must be reported explicitly.
func TestVerifyMigrations_DetectsVersionGap(t *testing.T) {
	dir := t.TempDir()
	writeMigrationPair(t, dir, 1, "create_t1", "CREATE TABLE checksum_t1 (id INT);", "DROP TABLE checksum_t1;")
	writeMigrationPair(t, dir, 3, "create_t3", "CREATE TABLE checksum_t3 (id INT);", "DROP TABLE checksum_t3;")

	pool := checksumScratchPool(t, checksumScratchDatabase(t))
	ctx := context.Background()
	if _, err := pool.MigrateUp(ctx, dir); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}

	report, err := pool.VerifyMigrations(ctx, dir)
	if err != nil {
		t.Fatalf("VerifyMigrations: %v", err)
	}
	if report.OK() {
		t.Fatal("expected a version gap to fail verification")
	}
	if len(report.VersionGaps) != 1 {
		t.Fatalf("expected exactly 1 gap reported, got %v", report.VersionGaps)
	}
	if !strings.Contains(report.VersionGaps[0], "version 2") {
		t.Errorf("expected the gap to name version 2, got: %s", report.VersionGaps[0])
	}
}

// VerifyMigrations against a database whose schema_migrations predates
// checksum tracking entirely (no checksum column at all) must fail with a
// clear, actionable error rather than a raw SQL error or a false-clean
// report.
func TestVerifyMigrations_NoChecksumColumnYet_FailsClearly(t *testing.T) {
	dir := t.TempDir()
	writeMigrationPair(t, dir, 1, "create_t1", "CREATE TABLE checksum_t1 (id INT);", "DROP TABLE checksum_t1;")

	pool := checksumScratchPool(t, checksumScratchDatabase(t))
	ctx := context.Background()

	// Manually reproduce the OLD (pre-item-2) schema_migrations shape,
	// with one row recorded the old way - no checksum column at all -
	// simulating a database that has never run the new migrate.go.
	err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `CREATE TABLE schema_migrations (
			version     BIGINT PRIMARY KEY,
			description TEXT NOT NULL,
			applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, description) VALUES (1, 'create_t1')`)
		return err
	})
	if err != nil {
		t.Fatalf("seed pre-item-2 schema_migrations: %v", err)
	}

	_, err = pool.VerifyMigrations(ctx, dir)
	if err == nil {
		t.Fatal("expected an error when schema_migrations has no checksum column, got nil")
	}
	if !strings.Contains(err.Error(), "checksum column") {
		t.Errorf("expected an actionable error naming the missing checksum column, got: %v", err)
	}
}

// Applying migrations against a database that already has legacy,
// checksum-less rows (from before this feature existed) must backfill
// them automatically, as part of the ordinary `up` flow - no separate
// manual step.
func TestMigrateUp_BackfillsMissingChecksumsAutomatically(t *testing.T) {
	dir := t.TempDir()
	legacySQL := "CREATE TABLE checksum_t1 (id INT);"
	writeMigrationPair(t, dir, 1, "create_t1", legacySQL, "DROP TABLE checksum_t1;")

	pool := checksumScratchPool(t, checksumScratchDatabase(t))
	ctx := context.Background()

	// Seed a pre-item-2-style row: version 1 recorded as applied, but
	// with no checksum, AND the table it created not actually present -
	// good enough for this test, which only checks the metadata
	// bookkeeping (checksum backfill), not that MigrateUp re-runs SQL for
	// versions it thinks are already applied (it must not, and does not).
	err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `CREATE TABLE schema_migrations (
			version     BIGINT PRIMARY KEY,
			description TEXT NOT NULL,
			applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, description) VALUES (1, 'create_t1')`)
		return err
	})
	if err != nil {
		t.Fatalf("seed pre-item-2 schema_migrations: %v", err)
	}

	if _, err := pool.MigrateUp(ctx, dir); err != nil {
		t.Fatalf("MigrateUp: %v", err)
	}

	var checksum *string
	err = pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version = 1`).Scan(&checksum)
	})
	if err != nil {
		t.Fatalf("read checksum: %v", err)
	}
	if checksum == nil {
		t.Fatal("expected MigrateUp to backfill the missing checksum automatically")
	}
	if want := sha256Hex(t, legacySQL); *checksum != want {
		t.Errorf("expected backfilled checksum %s (from current on-disk content), got %s", want, *checksum)
	}

	// Now VerifyMigrations must report clean - the backfill established a
	// baseline, even though it could not retroactively validate the
	// pre-existing row against its true historical apply-time content.
	report, err := pool.VerifyMigrations(ctx, dir)
	if err != nil {
		t.Fatalf("VerifyMigrations: %v", err)
	}
	if !report.OK() {
		t.Fatalf("expected a clean report after backfill, got: %+v", report)
	}
}
