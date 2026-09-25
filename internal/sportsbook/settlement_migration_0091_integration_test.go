//go:build integration

// Migration 0091 mechanics (ADR 0088 §12): the down migration succeeds on
// a clean, freshly-migrated scratch database, and refuses - WITH FORCE ROW
// LEVEL SECURITY active throughout, so a naive count(*) could not have
// caught this - once ANY settlement evidence exists. Each of §12's five
// checks is exercised as close to individually as the state machine
// allows: check 2 (a sportsbook tombstone) is reachable with NO new
// transaction TYPE present at all (only pre-existing 'tombstone'), so it
// is isolated on its own; check 5 (mismatch_kind) is reachable with zero
// sportsbook lifecycle activity, so it is fully isolated. Checks 1, 3 and
// 4 cannot be triggered independently of each other by construction (any
// history row implies both a new-typed ledger transaction and a non-open
// bet), so they are proven together by the realistic "a settlement was
// posted" scenario, which is the actual state this refusal exists to
// protect.
package sportsbook

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

func migration0091Dir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolve migrations dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "0091_sportsbook_settlement.up.sql")); err != nil {
		t.Fatalf("migration 0091 not found in %s: %v", dir, err)
	}
	return dir
}

func scratchPoolMigratedUp(t *testing.T, prefix string) *db.Pool {
	t.Helper()
	url := scratchdb.New(t, prefix)
	pool, err := db.Connect(context.Background(), url, 5, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect to scratch database: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.MigrateUp(context.Background(), migration0091Dir(t)); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	return pool
}

// TestMigration0091_DownSucceedsOnCleanDatabase: the down migration must
// succeed when nothing has ever been posted.
func TestMigration0091_DownSucceedsOnCleanDatabase(t *testing.T) {
	pool := scratchPoolMigratedUp(t, "sb0091clean_")
	if _, err := pool.MigrateDown(context.Background(), migration0091Dir(t), 1); err != nil {
		t.Fatalf("expected the down migration to succeed on a clean database: %v", err)
	}
}

// assertMigration0091StillApplied is code-review finding #10: a refused
// down-migration must leave the schema EXACTLY as a successful up-migration
// left it, not merely fail with the right error text. It checks the three
// things §12's refusal path could otherwise leave half-done if any of its
// five checks were not atomic with the rest of the migration file: (a)
// schema_migrations still records version 91 as applied; (b) the 20-value
// transaction_type CHECK (added by §12 step 1's ALTER) is still the one in
// force, evidenced by both an original value and one of the three new
// values still being admitted by it; (c) sportsbook_bet_settlements still
// exists as a table. Run with pool.WithoutTenant: schema_migrations and
// information_schema are not tenant-scoped, and FORCE RLS on
// sportsbook_bet_settlements must not make its own existence unobservable.
func assertMigration0091StillApplied(t *testing.T, pool *db.Pool) {
	t.Helper()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var applied bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = 91)`).Scan(&applied); err != nil {
			return err
		}
		if !applied {
			t.Errorf("schema_migrations no longer records migration 91 as applied after a refused down-migration")
		}

		var constraintDef string
		if err := tx.QueryRow(ctx,
			`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conname = 'ledger_transactions_transaction_type_check'`,
		).Scan(&constraintDef); err != nil {
			return err
		}
		for _, want := range []string{"deposit", "sportsbook_settlement", "sportsbook_void", "sportsbook_rollback"} {
			if !strings.Contains(constraintDef, want) {
				t.Errorf("transaction_type CHECK no longer admits %q after a refused down-migration: %s", want, constraintDef)
			}
		}

		var tableExists bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'sportsbook_bet_settlements')`,
		).Scan(&tableExists); err != nil {
			return err
		}
		if !tableExists {
			t.Errorf("sportsbook_bet_settlements no longer exists after a refused down-migration")
		}

		for _, trig := range []string{
			"sportsbook_bet_settlements_validate",    // T-1
			"sportsbook_bets_status_transition",      // T-2
			"sportsbook_bet_settlements_immutable",   // deny trigger (UPDATE/DELETE)
			"sportsbook_bet_settlements_no_truncate", // deny trigger (TRUNCATE)
		} {
			var triggerExists bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = $1 AND NOT tgisinternal)`, trig,
			).Scan(&triggerExists); err != nil {
				return err
			}
			if !triggerExists {
				t.Errorf("trigger %q no longer exists after a refused down-migration", trig)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("check post-refusal schema state: %v", err)
	}
}

// TestMigration0091_DownRefuses_AfterSettlementPosted covers §12 checks 1
// (new transaction type exists), 3 (history table non-empty) and 4 (a bet
// is no longer open) together - the realistic evidence-exists case.
func TestMigration0091_DownRefuses_AfterSettlementPosted(t *testing.T) {
	pool := scratchPoolMigratedUp(t, "sb0091settle_")
	f, actor, betID := newStdBet(t, pool)
	mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))

	_, err := pool.MigrateDown(context.Background(), migration0091Dir(t), 1)
	if err == nil {
		t.Fatalf("expected the down migration to refuse once a settlement has been posted")
	}
	if !strings.Contains(err.Error(), "irreversible") {
		t.Fatalf("expected the ADR 0088 §12 refusal message, got: %v", err)
	}
	assertMigration0091StillApplied(t, pool)
}

// TestMigration0091_DownRefuses_TombstoneOnly isolates §12 check 2: a
// rollback of a never-seen settlement writes ONLY a 'tombstone'-typed
// ledger transaction (a type that existed before migration 0091), so
// check 1's new-type CHECK cannot fire; check 2's LIKE-pattern check is
// what must catch it.
func TestMigration0091_DownRefuses_TombstoneOnly(t *testing.T) {
	pool := scratchPoolMigratedUp(t, "sb0091tomb_")
	f, actor, betID := newStdBet(t, pool)
	res := mustSimulate(t, pool, f.tenantID, rollbackEvent(betID, actor, 1))
	if res.Result != SettlementResultTombstoned {
		t.Fatalf("expected a tombstone, got %q", res.Result)
	}

	_, err := pool.MigrateDown(context.Background(), migration0091Dir(t), 1)
	if err == nil {
		t.Fatalf("expected the down migration to refuse once a sportsbook tombstone has been posted")
	}
	if !strings.Contains(err.Error(), "tombstone") {
		t.Fatalf("expected the check-2 refusal message naming the tombstone, got: %v", err)
	}
	assertMigration0091StillApplied(t, pool)
}

// TestMigration0091_DownRefuses_MismatchKindOnly isolates §12 check 5: a
// reconciliation_mismatches row using one of the new sb_* mismatch_kind
// values, with ZERO sportsbook lifecycle activity (so checks 1-4 all
// pass cleanly).
func TestMigration0091_DownRefuses_MismatchKindOnly(t *testing.T) {
	pool := scratchPoolMigratedUp(t, "sb0091mismatch_")
	tenantID := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'Mismatch Test Tenant', 'under_platform_licence')`,
			tenantID, "t-"+tenantID.String()[:8])
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	var runID uuid.UUID
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO reconciliation_runs (id, tenant_id, stream, period_start, period_end, status)
			 VALUES (gen_random_uuid(), $1, 'sportsbook_settlement', now() - interval '1 hour', now(), 'mismatches_found')
			 RETURNING id`, tenantID).Scan(&runID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO reconciliation_mismatches
				(tenant_id, reconciliation_run_id, reconciliation_key, expected_value, actual_value, mismatch_kind)
			 VALUES ($1, $2, 'k', 'expected', 'actual', 'sb_status_mismatch')`,
			tenantID, runID)
		return err
	})
	if err != nil {
		t.Fatalf("seed sb_status_mismatch row: %v", err)
	}

	_, err = pool.MigrateDown(context.Background(), migration0091Dir(t), 1)
	if err == nil {
		t.Fatalf("expected the down migration to refuse with a recorded sportsbook reconciliation mismatch")
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("expected the check-5 refusal message, got: %v", err)
	}
	assertMigration0091StillApplied(t, pool)
}
