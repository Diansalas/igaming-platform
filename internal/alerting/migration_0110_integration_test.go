//go:build integration

package alerting

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

func scratchPool(t *testing.T, prefix string) *db.Pool {
	t.Helper()
	url := scratchdb.New(t, prefix)
	pool, err := db.Connect(context.Background(), url, 10, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.MigrateUp(context.Background(), realMigrationsDir(t)); err != nil {
		t.Fatalf("migrate scratch up: %v", err)
	}
	return pool
}

func realMigrationsDir(t *testing.T) string {
	t.Helper()
	return "../../migrations"
}

// migration0110Version is the version these down/round-trip tests target.
const migration0110Version = int64(110)

// scratchPoolThrough0110 copies only the migration files numbered up to
// and including 0110 into a temp dir and migrates a fresh scratch DB
// through them, so "down one step" always rolls back exactly 0110 however
// many later migrations exist on disk (the internal/casino
// migration0099Scratch pattern). The returned dir must be used for every
// subsequent MigrateDown/MigrateUp/VerifyMigrations call.
func scratchPoolThrough0110(t *testing.T, prefix string) (*db.Pool, string) {
	t.Helper()
	src := realMigrationsDir(t)
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
		if perr != nil || n > migration0110Version {
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
		t.Fatalf("migrate scratch up through %d: %v", migration0110Version, err)
	}
	if len(applied) == 0 || applied[len(applied)-1] != migration0110Version {
		t.Fatalf("expected %d to be the last applied migration, got %v", migration0110Version, applied)
	}
	return pool, dir
}

// TestMigration0110_NoRouteSeeded is AL-8/HD-PRH2-4: alert_routes ships
// completely empty.
func TestMigration0110_NoRouteSeeded(t *testing.T) {
	pool := scratchPool(t, "alert0110routes")
	admin := seedPlatformAdmin(t, pool)
	var count int
	err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alert_routes`).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count routes: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected zero seeded routes, got %d", count)
	}
}

// TestMigration0110_KindsSeeded checks every Go-side Kind exists in the
// database with matching flags (LF test 3, partial - the full walk of
// "both in-tx raise and RaiseDetached succeed from the real session" is
// covered by the RLS/guarded integration tests for the kinds exercised
// there).
func TestMigration0110_KindsSeeded(t *testing.T) {
	pool := scratchPool(t, "alert0110kinds")
	admin := seedPlatformAdmin(t, pool)

	for _, k := range Kinds() {
		def := MustDef(k)
		var severity, scope string
		var simulation, requiresSubject, inTxRaisable bool
		err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT severity, scope, simulation, requires_subject, in_tx_raisable_by_tenant FROM alert_kinds WHERE kind = $1`, string(k)).
				Scan(&severity, &scope, &simulation, &requiresSubject, &inTxRaisable)
		})
		if err != nil {
			t.Fatalf("kind %q: %v", k, err)
		}
		if severity != string(def.Severity) {
			t.Errorf("kind %q: severity mismatch: Go=%s DB=%s", k, def.Severity, severity)
		}
		if scope != string(def.Scope) {
			t.Errorf("kind %q: scope mismatch: Go=%s DB=%s", k, def.Scope, scope)
		}
		if simulation != def.Simulation {
			t.Errorf("kind %q: simulation mismatch: Go=%v DB=%v", k, def.Simulation, simulation)
		}
		if requiresSubject != def.RequiresSubject {
			t.Errorf("kind %q: requires_subject mismatch: Go=%v DB=%v", k, def.RequiresSubject, requiresSubject)
		}
		if inTxRaisable != def.InTxRaisableByTenant {
			t.Errorf("kind %q: in_tx_raisable_by_tenant mismatch: Go=%v DB=%v", k, def.InTxRaisableByTenant, inTxRaisable)
		}
	}
}

// TestMigration0110_RecipientRefRefusesEmailAndPhoneShapes.
func TestMigration0110_RecipientRefRefusesEmailAndPhoneShapes(t *testing.T) {
	pool := scratchPool(t, "alert0110ref")
	admin := seedPlatformAdmin(t, pool)

	for _, ref := range []string{"ops@example.com", "+15551234567", "5551234567"} {
		err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO alert_routes (scope, severity, escalation_step, channel_kind, recipient_ref) VALUES ('platform', 'p1', 0, 'mock', $1)`, ref)
			return err
		})
		if err == nil {
			t.Errorf("expected recipient_ref %q to be refused", ref)
		}
	}
}

// TestMigration0110_DownRefusesWithRows.
func TestMigration0110_DownRefusesWithRows(t *testing.T) {
	pool, migDir := scratchPoolThrough0110(t, "alert0110down")
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	seedOpenAlert(t, pool, tenantA, KindPaymentKillSwitchEngaged, "switch:"+uuid.NewString())

	_, err := pool.MigrateDown(context.Background(), migDir, 1)
	if err == nil {
		t.Fatal("expected migration 0110's down migration to refuse while an alerts row exists")
	}
}

// TestMigration0110_DownRefusesWithRoutesOnly is code review F-11: the
// down migration must also refuse when alert_routes alone is non-empty,
// even with zero alerts/occurrences/deliveries.
func TestMigration0110_DownRefusesWithRoutesOnly(t *testing.T) {
	pool, migDir := scratchPoolThrough0110(t, "alert0110downroutes")
	admin := seedPlatformAdmin(t, pool)
	err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alert_routes (scope, severity, escalation_step, channel_kind, recipient_ref) VALUES ('platform', 'p1', 0, 'log', 'ops')`)
		return err
	})
	if err != nil {
		t.Fatalf("seed route: %v", err)
	}

	_, err = pool.MigrateDown(context.Background(), migDir, 1)
	if err == nil {
		t.Fatal("expected migration 0110's down migration to refuse with a routes-only state")
	}
}

// TestMigration0110_UpDownUp verifies the migration is fully reversible
// on an otherwise-empty database.
func TestMigration0110_UpDownUp(t *testing.T) {
	pool, migDir := scratchPoolThrough0110(t, "alert0110updown")

	if _, err := pool.MigrateDown(context.Background(), migDir, 1); err != nil {
		t.Fatalf("down: %v", err)
	}
	applied, err := pool.MigrateUp(context.Background(), migDir)
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if len(applied) != 1 || applied[0] != migration0110Version {
		t.Fatalf("expected exactly [%d] re-applied, got %v", migration0110Version, applied)
	}

	report, err := pool.VerifyMigrations(context.Background(), migDir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	// Migrations 0001..0110 are contiguous on main, so the round trip must
	// leave a fully clean report: no checksum drift, no missing file and
	// no version gap.
	if !report.OK() {
		t.Fatalf("expected a clean migration report after the round trip, got %+v", report)
	}
	for _, res := range report.Results {
		if res.Status == db.MigrationCheckMismatch || res.Status == db.MigrationCheckMissingFile {
			t.Fatalf("expected no checksum drift, got %+v", res)
		}
	}
}
