//go:build integration

// Migration mechanics for 0092 (Stage 10.1 PAY-REV-1, ADR 0090,
// docs/plans/stage-10.1-planning-gate-proposal.md §F/§J tests #7-#9): the
// up-migration's refusal when duplicate deposit_reversal rows already
// exist, its success on a clean database, and its down migration's exact
// restoration. All three run against their own throwaway database built
// from the real migrations directory (mirroring
// migration_0048_integration_test.go's own pattern exactly) so the shared
// test database is never polluted with seeded duplicate reversals
// (ledger-finance review P2-4).
//
// Migration 0093 (a different, in-flight workstream - SB-T1-XMIN) is
// deliberately held back in every scratch database this file builds: it
// has no dependency on 0092 (the two "are independent and commute" per
// the planning gate's architect ruling), and excluding it keeps these
// tests about 0092 alone.
package ledger

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

const migration0092Version = int64(92)

// migration0092HeldPrefixes holds back 0092 itself (and, always, the
// unrelated in-flight 0093) so a scratch database can be brought to
// exactly "chain applied through 0091" before seeding data that a
// migration 0021-shaped ledger_transactions table permits today but 0092
// will no longer permit once applied.
var migration0092HeldPrefixes = []string{"0092_", "0093_"}

func stagedMigrations0092(t *testing.T, includeMigration0092 bool) (dir string, addMigration0092 func()) {
	t.Helper()
	src := migrationsDir(t)
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
		heldBack := strings.HasPrefix(e.Name(), "0093_")
		if !includeMigration0092 && strings.HasPrefix(e.Name(), "0092_") {
			heldBack = true
		}
		if heldBack {
			held = append(held, e.Name())
			continue
		}
		copyMigrationFile(t, src, dir, e.Name())
	}
	return dir, func() {
		for _, name := range held {
			if strings.HasPrefix(name, "0092_") {
				copyMigrationFile(t, src, dir, name)
			}
		}
	}
}

// seedDepositAndReversals inserts one bare `deposit` ledger_transactions
// row for tenantID plus n `deposit_reversal` rows all naming it via
// reverses_transaction_id - directly by SQL, bypassing ledger.Post
// entirely (Post itself would refuse a second reversal once the L2/S4
// re-check code exists; these tests are about the SCHEMA's own backstop,
// independent of the application-layer fix). Every row is inserted inside
// a tenant-scoped transaction, so FORCE ROW LEVEL SECURITY (migration
// 0021) is honoured exactly as it is for every other write in this
// codebase - these tests never bypass RLS to seed data.
func seedDepositAndReversals(t *testing.T, pool *db.Pool, tenantID uuid.UUID, n int) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		depositID := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, correlation_id)
			 VALUES ($1, $2, 'deposit', $3, $4)`,
			depositID, tenantID, "seed-dep-"+depositID.String(), uuid.New()); err != nil {
			return fmt.Errorf("seed deposit: %w", err)
		}
		for i := 0; i < n; i++ {
			revID := uuid.New()
			if _, err := tx.Exec(ctx,
				`INSERT INTO ledger_transactions
					(id, tenant_id, transaction_type, idempotency_key, correlation_id, reverses_transaction_id)
				 VALUES ($1, $2, 'deposit_reversal', $3, $4, $5)`,
				revID, tenantID, fmt.Sprintf("seed-rev-%d-%s", i, revID), uuid.New(), depositID); err != nil {
				return fmt.Errorf("seed reversal %d: %w", i, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed deposit+%d reversals for tenant %s: %v", n, tenantID, err)
	}
}

// seedBareTenant inserts the minimum tenants/persons rows a
// tenant-scoped ledger_transactions write needs to satisfy RLS - the
// same platform-admin-scoped pattern migration_0048_integration_test.go's
// seedLockedAccountRow uses, trimmed to only what ledger_transactions
// itself needs (no brand/player/wallet chain: ledger_transactions carries
// no FK to any of those).
func seedBareTenant(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	tenantID := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'Migration 0092 Test Tenant', 'under_platform_licence')`,
			tenantID, "t92-"+tenantID.String()[:8])
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	return tenantID
}

func indexExists(t *testing.T, pool *db.Pool, name string) bool {
	t.Helper()
	var exists bool
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_class WHERE relname = $1)`, name).Scan(&exists)
	})
	if err != nil {
		t.Fatalf("check index %s existence: %v", name, err)
	}
	return exists
}

// TestMigration0092_RefusesWithExistingDuplicates is test #7 (§J): seeded
// duplicates in TWO tenants, FORCE RLS active, run as the real migration
// role (scratchdb.New's owner - the same NOBYPASSRLS role every other
// integration test uses; see assertUnprivilegedOwner). The refusal must
// come from the index build itself scanning every row regardless of RLS,
// never from a SELECT that RLS could blind (ruling R-4).
func TestMigration0092_RefusesWithExistingDuplicates(t *testing.T) {
	scratchURL := scratchDatabase(t)
	pool := scratchPool(t, scratchURL)
	dir, addMigration0092 := stagedMigrations0092(t, false)

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up through 0091 (0092/0093 held back): %v", err)
	}
	if appliedVersions(t, pool)[migration0092Version] {
		t.Fatal("migration 0092 must not have been applied yet")
	}

	tenantA := seedBareTenant(t, pool)
	tenantB := seedBareTenant(t, pool)
	seedDepositAndReversals(t, pool, tenantA, 2)
	seedDepositAndReversals(t, pool, tenantB, 3)

	addMigration0092()
	_, err := pool.MigrateUp(context.Background(), dir)
	if err == nil {
		t.Fatal("migration 0092 must refuse to run while duplicate deposit_reversal rows exist")
	}
	for _, want := range []string{"migration 0092", "duplicate deposit reversals exist", "escalated"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal must mention %q, got: %v", want, err)
		}
	}
	if appliedVersions(t, pool)[migration0092Version] {
		t.Fatal("a failed migration 0092 must not be recorded in schema_migrations")
	}
	if indexExists(t, pool, "ledger_transactions_one_deposit_reversal") {
		t.Fatal("a failed migration 0092 must leave no partial index behind (the whole file ran in one rolled-back transaction)")
	}

	// Never deleted: both tenants' seeded rows are still exactly present
	// (CLAUDE.md: ledger rows are never deleted, and this migration must
	// not have tried to).
	for _, tc := range []struct {
		tenantID uuid.UUID
		want     int
	}{{tenantA, 2}, {tenantB, 3}} {
		var n int
		err := pool.WithTenant(context.Background(), tc.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'deposit_reversal'`,
				tc.tenantID).Scan(&n)
		})
		if err != nil {
			t.Fatalf("count reversals for tenant %s: %v", tc.tenantID, err)
		}
		if n != tc.want {
			t.Fatalf("tenant %s: expected %d deposit_reversal rows still present, got %d", tc.tenantID, tc.want, n)
		}
	}
}

// TestMigration0092_SucceedsOnCleanDatabase is test #8.
func TestMigration0092_SucceedsOnCleanDatabase(t *testing.T) {
	scratchURL := scratchDatabase(t)
	pool := scratchPool(t, scratchURL)
	dir, _ := stagedMigrations0092(t, true)

	applied, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("migrate up through 0092: %v", err)
	}
	found := false
	for _, v := range applied {
		if v == migration0092Version {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected migration 0092 among applied versions, got %v", applied)
	}
	if !appliedVersions(t, pool)[migration0092Version] {
		t.Fatal("migration 0092 must be recorded in schema_migrations")
	}
	if !indexExists(t, pool, "ledger_transactions_one_deposit_reversal") {
		t.Fatal("expected ledger_transactions_one_deposit_reversal to exist after migration 0092")
	}

	// The invariant actually holds now: a second reversal for one
	// original, in one tenant, via raw SQL (bypassing the application
	// lock entirely) is refused by the index itself.
	tenantID := seedBareTenant(t, pool)
	seedDepositAndReversals(t, pool, tenantID, 1)
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var origID uuid.UUID
		if err := tx.QueryRow(ctx,
			`SELECT reverses_transaction_id FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'deposit_reversal' LIMIT 1`,
			tenantID).Scan(&origID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, correlation_id, reverses_transaction_id)
			 VALUES ($1, $2, 'deposit_reversal', $3, $4, $5)`,
			uuid.New(), tenantID, "second-rev", uuid.New(), origID)
		return err
	})
	if !db.IsUniqueViolation(err) {
		t.Fatalf("expected a unique violation for the second reversal, got %v", err)
	}
	if name, ok := db.UniqueViolationConstraintName(err); !ok || name != "ledger_transactions_one_deposit_reversal" {
		t.Fatalf("expected the violation to name ledger_transactions_one_deposit_reversal, got %q (ok=%v)", name, ok)
	}
}

// TestMigration0092_DownRestoresPriorState is test #9: the down migration
// drops only the index, is safe on a clean database, and a subsequent
// up re-creates it - a full round trip.
func TestMigration0092_DownRestoresPriorState(t *testing.T) {
	scratchURL := scratchDatabase(t)
	pool := scratchPool(t, scratchURL)
	dir, _ := stagedMigrations0092(t, true)

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up through 0092: %v", err)
	}
	if !indexExists(t, pool, "ledger_transactions_one_deposit_reversal") {
		t.Fatal("expected the index to exist before rolling back")
	}

	if _, err := pool.MigrateDown(context.Background(), dir, 1); err != nil {
		t.Fatalf("migrate down 0092: %v", err)
	}
	if appliedVersions(t, pool)[migration0092Version] {
		t.Fatal("migration 0092 must no longer be recorded after rolling it back")
	}
	if indexExists(t, pool, "ledger_transactions_one_deposit_reversal") {
		t.Fatal("expected the index to be gone after rolling back migration 0092")
	}

	// Re-apply: the up migration is re-runnable after its own down.
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("re-apply migration 0092: %v", err)
	}
	if !indexExists(t, pool, "ledger_transactions_one_deposit_reversal") {
		t.Fatal("expected the index to exist again after re-applying migration 0092")
	}
}
