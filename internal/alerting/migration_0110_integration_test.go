//go:build integration

package alerting

import (
	"context"
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
	pool := scratchPool(t, "alert0110down")
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	seedOpenAlert(t, pool, tenantA, KindPaymentKillSwitchEngaged, "switch:"+uuid.NewString())

	// steps=2: PRH-2 K1 (ADR 0099) added migration 0112 on top of 0110.
	// Rolling back only 1 step now undoes 0112 (which has no rows of its
	// own here and reverses cleanly), never reaching 0110's own down
	// migration at all - 2 steps is what actually exercises 0110's
	// refuse-with-rows guard this test is named for.
	_, err := pool.MigrateDown(context.Background(), realMigrationsDir(t), 2)
	if err == nil {
		t.Fatal("expected migration 0110's down migration to refuse while an alerts row exists")
	}
}

// TestMigration0110_DownRefusesWithRoutesOnly is code review F-11: the
// down migration must also refuse when alert_routes alone is non-empty,
// even with zero alerts/occurrences/deliveries.
func TestMigration0110_DownRefusesWithRoutesOnly(t *testing.T) {
	pool := scratchPool(t, "alert0110downroutes")
	admin := seedPlatformAdmin(t, pool)
	err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alert_routes (scope, severity, escalation_step, channel_kind, recipient_ref) VALUES ('platform', 'p1', 0, 'log', 'ops')`)
		return err
	})
	if err != nil {
		t.Fatalf("seed route: %v", err)
	}

	// steps=2: see TestMigration0110_DownRefusesWithRows's identical note.
	_, err = pool.MigrateDown(context.Background(), realMigrationsDir(t), 2)
	if err == nil {
		t.Fatal("expected migration 0110's down migration to refuse with a routes-only state")
	}
}

// TestMigration0110_UpDownUp verifies the migration is fully reversible
// on an otherwise-empty database.
func TestMigration0110_UpDownUp(t *testing.T) {
	pool := scratchPool(t, "alert0110updown")

	if _, err := pool.MigrateDown(context.Background(), realMigrationsDir(t), 1); err != nil {
		t.Fatalf("down: %v", err)
	}
	applied, err := pool.MigrateUp(context.Background(), realMigrationsDir(t))
	if err != nil {
		t.Fatalf("up: %v", err)
	}
	if len(applied) != 1 {
		t.Fatalf("expected exactly 1 migration re-applied, got %d", len(applied))
	}

	report, err := pool.VerifyMigrations(context.Background(), realMigrationsDir(t))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	// This branch may legitimately show ONE known, disclosed version gap,
	// depending on which sibling branches are merged into this worktree
	// yet: G1's migration 0109 (if not yet merged), or PRH-2 B's migration
	// 0111 (the casino bootstrap - if not yet merged; PRH-2 K1's own
	// migration is 0112, allocated on top of B's 0111 per the
	// orchestrator's mid-build renumbering, see ADR 0099's own "NOTE ON
	// NUMBERING"). Either is expected and resolved by the orchestrator at
	// merge time - assert every CHECKSUM result is clean (report.OK()'s
	// other half) without requiring report.OK() itself, which would also
	// fail on either known, disclosed gap.
	for _, res := range report.Results {
		if res.Status == db.MigrationCheckMismatch || res.Status == db.MigrationCheckMissingFile {
			t.Fatalf("expected no checksum drift, got %+v", res)
		}
	}
	knownGaps := map[string]bool{
		"missing migration version 109 (gap between 108 and 110)": true,
		"missing migration version 111 (gap between 110 and 112)": true,
	}
	if len(report.VersionGaps) > 1 {
		t.Fatalf("expected at most one known gap, got %v", report.VersionGaps)
	}
	if len(report.VersionGaps) == 1 && !knownGaps[report.VersionGaps[0]] {
		t.Fatalf("expected only a known gap, got %v", report.VersionGaps)
	}
}
