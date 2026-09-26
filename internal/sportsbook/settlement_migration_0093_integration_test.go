//go:build integration

// Migration 0093 mechanics (SB-T1-XMIN, docs/plans/stage-10.1-planning-
// gate-proposal.md §J, tests #18-#19): 0093 is a BODY-ONLY change to
// sportsbook_bet_settlements_validate() (T-1). This file proves, on
// scratch databases, that (a) the down migration restores 0091's function
// body byte-for-byte (not merely "some function that behaves similarly"),
// and (b) the up migration changes ONLY that function's body - no table,
// column, index, constraint, trigger or RLS policy on
// sportsbook_bet_settlements or sportsbook_bets differs before and after.
package sportsbook

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// migration0093Dir resolves the real migrations directory (whatever else
// has landed in it - e.g. migration 0092 from a concurrent workstream -
// this file never assumes a specific total count, only that 0093 exists).
func migration0093Dir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("resolve migrations dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "0093_sportsbook_settlement_causation_xact_status.up.sql")); err != nil {
		t.Fatalf("migration 0093 not found in %s: %v", dir, err)
	}
	return dir
}

// migration0093DirExcludingSelf copies the real migrations directory into
// a fresh t.TempDir(), OMITTING 0093's own up/down files - i.e. "the
// migration chain exactly as it stood immediately before 0093 landed",
// regardless of how many other migrations (0092, or later ones) exist
// alongside it. Using a real copy (not a hand-maintained fixture) is what
// lets TestMigration0093_UpChangesOnlyFunctionBody snapshot the REAL
// pre-0093 schema, not a stale approximation of it.
func migration0093DirExcludingSelf(t *testing.T) string {
	t.Helper()
	realDir := migration0093Dir(t)
	entries, err := os.ReadDir(realDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	out := t.TempDir()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if name == "0093_sportsbook_settlement_causation_xact_status.up.sql" ||
			name == "0093_sportsbook_settlement_causation_xact_status.down.sql" {
			continue
		}
		content, err := os.ReadFile(filepath.Join(realDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(out, name), content, 0o600); err != nil {
			t.Fatalf("write %s into temp migrations dir: %v", name, err)
		}
	}
	return out
}

// migration0093DirThroughSelf copies the real migrations directory into a
// fresh t.TempDir(), INCLUDING 0093's own up/down files but EXCLUDING any
// migration numbered ABOVE 93 (e.g. Stage 10.3's 0095) - i.e. "the chain
// exactly as it stood the moment 0093 landed, and no later". This is what
// makes "roll back exactly 1 step" in
// TestMigration0093_DownRestoresExactPriorFunctionBody deterministically
// mean "roll back 0093 itself", regardless of how many unrelated
// migrations have landed on top of it since (this file's own header
// comment already disclaims assuming a specific total count - this
// helper is what keeps that disclaimer true for a DOWN-migration test,
// not just an up-only one).
func migration0093DirThroughSelf(t *testing.T) string {
	t.Helper()
	realDir := migration0093Dir(t)
	entries, err := os.ReadDir(realDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	out := t.TempDir()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) < 4 {
			continue
		}
		ver, err := strconv.Atoi(name[:4])
		if err != nil {
			continue
		}
		if ver > 93 {
			continue
		}
		content, err := os.ReadFile(filepath.Join(realDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(out, name), content, 0o600); err != nil {
			t.Fatalf("write %s into temp migrations dir: %v", name, err)
		}
	}
	return out
}

func migration0093ScratchPool(t *testing.T, prefix string) *db.Pool {
	t.Helper()
	url := scratchdb.New(t, prefix)
	pool, err := db.Connect(context.Background(), url, 5, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect to scratch database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// t1FunctionProsrc reads sportsbook_bet_settlements_validate()'s current
// prosrc (the literal text between the $$ ... $$ delimiters, independent
// of whether it was declared via CREATE FUNCTION or CREATE OR REPLACE
// FUNCTION - pg_get_functiondef normalizes that prefix away).
func t1FunctionProsrc(t *testing.T, pool *db.Pool) string {
	t.Helper()
	var src string
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT prosrc FROM pg_proc WHERE proname = 'sportsbook_bet_settlements_validate'`).Scan(&src)
	})
	if err != nil {
		t.Fatalf("read sportsbook_bet_settlements_validate prosrc: %v", err)
	}
	return src
}

// TestMigration0093_DownRestoresExactPriorFunctionBody proves 0093's down
// migration restores EXACTLY migration 0091's original function body
// (the xmin-equality composed-void causation rule), byte-for-byte - not a
// rewritten or approximated version of it. Two independent scratch
// databases are compared: one migrated only through the pre-0093 chain
// (the genuine "before" state), the other migrated through 0093 and then
// back down one step (the "after a revert" state).
func TestMigration0093_DownRestoresExactPriorFunctionBody(t *testing.T) {
	dirWithout93 := migration0093DirExcludingSelf(t)
	dirWith93 := migration0093DirThroughSelf(t)

	before := migration0093ScratchPool(t, "sb0093before_")
	if _, err := before.MigrateUp(context.Background(), dirWithout93); err != nil {
		t.Fatalf("migrate up (pre-0093 chain): %v", err)
	}
	beforeProsrc := t1FunctionProsrc(t, before)

	after := migration0093ScratchPool(t, "sb0093after_")
	if _, err := after.MigrateUp(context.Background(), dirWith93); err != nil {
		t.Fatalf("migrate up (full chain including 0093): %v", err)
	}
	if _, err := after.MigrateDown(context.Background(), dirWith93, 1); err != nil {
		t.Fatalf("migrate down 0093: %v", err)
	}
	afterDownProsrc := t1FunctionProsrc(t, after)

	if beforeProsrc != afterDownProsrc {
		t.Fatalf("0093's down migration did not restore migration 0091's exact function body\n--- pre-0093 (want) ---\n%s\n--- post-0093-down (got) ---\n%s",
			beforeProsrc, afterDownProsrc)
	}
}

// sbT1Snapshot captures every catalog fact about sportsbook_bet_settlements
// and sportsbook_bets that migration 0093 must NOT change: columns,
// constraints, indexes, triggers, RLS policies and the FORCE ROW LEVEL
// SECURITY / ROW LEVEL SECURITY flags. It deliberately excludes the
// trigger FUNCTION body itself (sportsbook_bet_settlements_validate's
// prosrc), which is compared separately.
type sbT1Snapshot struct {
	Columns     []string
	Constraints []string
	Indexes     []string
	Triggers    []string
	Policies    []string
	RLSFlags    []string
}

func captureSBT1Snapshot(t *testing.T, pool *db.Pool) sbT1Snapshot {
	t.Helper()
	var snap sbT1Snapshot
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT table_name || '.' || column_name || ':' || data_type || ':' || is_nullable || ':' || COALESCE(column_default, '')
			  FROM information_schema.columns
			 WHERE table_name IN ('sportsbook_bet_settlements', 'sportsbook_bets')
			 ORDER BY table_name, column_name`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				return err
			}
			snap.Columns = append(snap.Columns, s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		rows, err = tx.Query(ctx, `
			SELECT t.relname || ':' || c.conname || ':' || pg_get_constraintdef(c.oid)
			  FROM pg_constraint c JOIN pg_class t ON t.oid = c.conrelid
			 WHERE t.relname IN ('sportsbook_bet_settlements', 'sportsbook_bets')
			 ORDER BY 1`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				return err
			}
			snap.Constraints = append(snap.Constraints, s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		rows, err = tx.Query(ctx, `
			SELECT tablename || ':' || indexname || ':' || indexdef
			  FROM pg_indexes
			 WHERE tablename IN ('sportsbook_bet_settlements', 'sportsbook_bets')
			 ORDER BY 1`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				return err
			}
			snap.Indexes = append(snap.Indexes, s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		rows, err = tx.Query(ctx, `
			SELECT c.relname || ':' || tg.tgname || ':' || tg.tgtype || ':' || tg.tgfoid::regproc::text
			  FROM pg_trigger tg JOIN pg_class c ON c.oid = tg.tgrelid
			 WHERE c.relname IN ('sportsbook_bet_settlements', 'sportsbook_bets') AND NOT tg.tgisinternal
			 ORDER BY 1`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				return err
			}
			snap.Triggers = append(snap.Triggers, s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		rows, err = tx.Query(ctx, `
			SELECT tablename || ':' || policyname || ':' || cmd || ':' || COALESCE(qual, '') || ':' || COALESCE(with_check, '')
			  FROM pg_policies
			 WHERE tablename IN ('sportsbook_bet_settlements', 'sportsbook_bets')
			 ORDER BY 1`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				return err
			}
			snap.Policies = append(snap.Policies, s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		rows, err = tx.Query(ctx, `
			SELECT relname || ':rowsecurity=' || relrowsecurity::text || ':forcerowsecurity=' || relforcerowsecurity::text
			  FROM pg_class
			 WHERE relname IN ('sportsbook_bet_settlements', 'sportsbook_bets')
			 ORDER BY 1`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				return err
			}
			snap.RLSFlags = append(snap.RLSFlags, s)
		}
		rows.Close()
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("capture schema snapshot: %v", err)
	}
	return snap
}

func assertSnapshotsEqual(t *testing.T, label string, before, after []string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("%s: count changed by migration 0093: before=%v after=%v", label, before, after)
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("%s: entry %d changed by migration 0093:\nbefore: %s\nafter:  %s", label, i, before[i], after[i])
		}
	}
}

// TestMigration0093_UpChangesOnlyFunctionBody proves 0093's up migration
// changes ONLY sportsbook_bet_settlements_validate()'s body (pg_proc) -
// no column, constraint, index, trigger, RLS policy or RLS flag on
// sportsbook_bet_settlements or sportsbook_bets differs before and after
// applying it, on the SAME scratch database (so both snapshots are taken
// against identical unrelated state).
func TestMigration0093_UpChangesOnlyFunctionBody(t *testing.T) {
	pool := migration0093ScratchPool(t, "sb0093onlyfn_")
	dirWithout93 := migration0093DirExcludingSelf(t)
	dirWith93 := migration0093Dir(t)

	if _, err := pool.MigrateUp(context.Background(), dirWithout93); err != nil {
		t.Fatalf("migrate up (pre-0093 chain): %v", err)
	}
	before := captureSBT1Snapshot(t, pool)
	beforeProsrc := t1FunctionProsrc(t, pool)

	// Applying the real (0093-inclusive) directory against a database
	// already migrated through everything else only applies the one
	// migration still pending: 0093 itself.
	applied, err := pool.MigrateUp(context.Background(), dirWith93)
	if err != nil {
		t.Fatalf("migrate up (apply 0093): %v", err)
	}
	if len(applied) != 1 || applied[0] != 93 {
		t.Fatalf("expected exactly migration 93 to be newly applied, got %v", applied)
	}
	after := captureSBT1Snapshot(t, pool)
	afterProsrc := t1FunctionProsrc(t, pool)

	assertSnapshotsEqual(t, "columns", before.Columns, after.Columns)
	assertSnapshotsEqual(t, "constraints", before.Constraints, after.Constraints)
	assertSnapshotsEqual(t, "indexes", before.Indexes, after.Indexes)
	assertSnapshotsEqual(t, "triggers", before.Triggers, after.Triggers)
	assertSnapshotsEqual(t, "policies", before.Policies, after.Policies)
	assertSnapshotsEqual(t, "RLS flags", before.RLSFlags, after.RLSFlags)

	if beforeProsrc == afterProsrc {
		t.Fatalf("expected migration 0093 to change sportsbook_bet_settlements_validate's body, but it is byte-identical to before")
	}
}

// TestSBT1XMIN_ReconstructionGuard_NullAndErrorCasesRejectClosed isolates
// the causation branch's xid8 reconstruction + pg_xact_status guard logic
// (G1/G2, security review X-2) from the trigger machinery, so the "future"
// / unconstructible-xid8 error case (a value beyond any Postgres has
// actually assigned) can be tested directly and deterministically. It
// replicates the expression migration 0093 uses
// (docs/plans/stage-10.1-planning-gate-proposal.md §J "+" row), as a
// standalone SQL function on a scratch database - never touching the real
// trigger. See TestSBT1XMIN_GuardExpressionMatchesInstalledTrigger below,
// which pins this probe's text against the REAL installed trigger so the
// two cannot silently drift apart (code review F4/P3-3, security review
// P3-3, ledger-finance P3-4).
//
// IMPORTANT, corrected claim (code review F4/P3-3; the previous version of
// this comment overstated coverage): the "NULL-status" case below
// (raw=3) does NOT exercise pg_xact_status returning NULL. It is rejected
// earlier, by the "reconstructed < ref" branch, before pg_xact_status is
// ever called - and `xact_full IS NULL` in the real trigger is dead code,
// because `SELECT ... INTO STRICT` cannot itself yield NULL (it raises
// NO_DATA_FOUND/TOO_MANY_ROWS instead, both caught by the surrounding
// EXCEPTION WHEN OTHERS). On current PostgreSQL versions, G1's NULL-status
// path (a genuinely clog-truncated xid whose reconstruction nonetheless
// passes ">= xact_ref") is understood to be unreachable in practice: any
// xid >= the current transaction's own xid was assigned very recently and
// is never clog-truncated. G1 is retained as defensive, redundant
// belt-and-braces coverage (IS NOT DISTINCT FROM, not a bare inequality,
// so a hypothetical future NULL would still reject rather than silently
// accept) - it is not claimed as reachable or as tested end-to-end here.
// The name "NullAndErrorCases" is kept for continuity with the planning
// document's §J row naming; the case actually exercised below is the
// "reconstructs below ref" reject, run through pg_xact_status's own error
// path for the separate "future xid" probe underneath it.
func TestSBT1XMIN_ReconstructionGuard_NullAndErrorCasesRejectClosed(t *testing.T) {
	pool := migration0093ScratchPool(t, "sb0093guard_")
	ctx := context.Background()

	const guardFn = `
CREATE FUNCTION sb_t1_xmin_guard_probe(raw BIGINT, ref BIGINT) RETURNS BOOLEAN AS $$
DECLARE
    reconstructed BIGINT;
BEGIN
    reconstructed := (ref & ~4294967295) | raw;
    IF reconstructed IS NULL
        OR reconstructed < ref
        OR pg_xact_status(reconstructed::text::xid8) IS DISTINCT FROM 'in progress' THEN
        RETURN FALSE;
    END IF;
    RETURN TRUE;
EXCEPTION WHEN OTHERS THEN
    RETURN FALSE;
END;
$$ LANGUAGE plpgsql;`

	err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, guardFn)
		return err
	})
	if err != nil {
		t.Fatalf("create probe function: %v", err)
	}

	var currentTop int64
	err = pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT pg_current_xact_id()::text::bigint`).Scan(&currentTop)
	})
	if err != nil {
		t.Fatalf("read pg_current_xact_id: %v", err)
	}

	// Error case (G2): a "future" xid8 - far beyond anything Postgres has
	// actually assigned - makes pg_xact_status RAISE ("transaction ID ...
	// is in the future"), which the guard's EXCEPTION WHEN OTHERS must
	// catch and reject, not propagate or accept.
	var futureOK bool
	err = pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT sb_t1_xmin_guard_probe($1, $2)`,
			(currentTop&^4294967295)|1_000_000_000, currentTop).Scan(&futureOK)
	})
	if err != nil {
		t.Fatalf("call probe (future xid case): %v", err)
	}
	if futureOK {
		t.Fatal("SB-T1-XMIN G2: a future/unconstructible xid8 must be REJECTED (pg_xact_status errors), not accepted")
	}

	// "Old xid" case - CORRECTED (code review F4/P3-3): this does NOT
	// exercise pg_xact_status returning NULL, and does not exercise G1's
	// NULL-status branch at all. raw=3 reconstructs to
	// (ref & ~4294967295) | 3, which is < ref (the current top-level xid
	// is never that small in a live test run), so this is rejected by the
	// EARLIER "reconstructed < ref" branch, before pg_xact_status is ever
	// called. It is a (redundant, with RejectsEarlierTransactionRollback)
	// exercise of that branch, not a G1/NULL-status probe. Kept as a
	// belt-and-braces check that a tiny raw value is rejected outright; it
	// is not relied on for G1 coverage. See the doc comment above this
	// function, and TestSBT1XMIN_GuardExpressionMatchesInstalledTrigger,
	// for why G1's actual NULL-status path is treated as defensive/
	// unreachable rather than tested.
	var oldOK bool
	err = pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT sb_t1_xmin_guard_probe($1, $2)`,
			int64(3), currentTop).Scan(&oldOK)
	})
	if err != nil {
		t.Fatalf("call probe (old/below-ref xid case): %v", err)
	}
	if oldOK {
		t.Fatal("SB-T1-XMIN: an old xid that reconstructs below the reference xid must be REJECTED, never accepted")
	}

	// Positive control: the current top-level transaction's own id must be
	// accepted, proving the probe function's happy path actually works
	// (so the two rejections above are not vacuously true).
	var selfOK bool
	err = pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT sb_t1_xmin_guard_probe(pg_current_xact_id()::text::bigint & 4294967295, pg_current_xact_id()::text::bigint)`).Scan(&selfOK)
	})
	if err != nil {
		t.Fatalf("call probe (self/positive-control case): %v", err)
	}
	if !selfOK {
		t.Fatal("positive control failed: the guard rejected the current transaction's own in-progress xid - the probe does not mirror migration 0093's real logic")
	}
}

// sbT1xminGuardVerbatimText is the causation-branch guard, copied
// character-for-character from migrations/0093_sportsbook_settlement_
// causation_xact_status.up.sql (the reconstruction assignment plus the
// IF/OR/RAISE guard, using the REAL trigger's own variable names -
// xact_ref, xact_full, cause_xmin_raw - not the probe function's renamed
// ref/raw/reconstructed). Migration 0093 is checksum-immutable once
// applied, so this constant is the only thing that should ever need to
// change here, and only in lockstep with a NEW migration that replaces
// 0093's CREATE OR REPLACE body again.
const sbT1xminGuardVerbatimText = `xact_full := (xact_ref & ~4294967295) | cause_xmin_raw;
            IF xact_full IS NULL
                OR xact_full < xact_ref
                OR pg_xact_status(xact_full::text::xid8) IS DISTINCT FROM 'in progress' THEN`

// TestSBT1XMIN_GuardExpressionMatchesInstalledTrigger addresses code
// review F4/P3-3, security review P3-3 and ledger-finance P3-4: the
// NULL/error guard test above (TestSBT1XMIN_ReconstructionGuard_
// NullAndErrorCasesRejectClosed) necessarily exercises a hand-copied
// REPLICA of the guard expression, because a genuinely clog-truncated
// "too old" xid and a "future" xid cannot be deterministically produced
// through the real trigger with ordinary fixture rows. That leaves a
// drift risk: if a future migration edits 0093's guard wording without
// updating the probe, the probe test would keep passing while silently
// testing the WRONG expression.
//
// This test closes that drift risk directly, without needing to drive
// the trigger end-to-end: it reads the REAL, installed, currently-active
// sportsbook_bet_settlements_validate() definition via
// pg_get_functiondef (not a hand-transcribed copy) and asserts that the
// exact reconstruction-plus-guard text above appears in it VERBATIM. If
// a future body-only migration changes the reconstruction arithmetic or
// the guard predicate (for example weakening "IS DISTINCT FROM" back to
// "<>", or changing the reconstruction formula), this test fails even
// though the hand-copied probe in the test above would not notice.
//
// This is a textual pin, not a behavioral one - it cannot substitute for
// the probe's actual accept/reject assertions, only guarantee the two
// stay in sync. It runs against the SAME already-migrated shared test
// database every other trigger-level test in this package uses (no
// scratch database needed: this only reads pg_proc, it changes nothing).
func TestSBT1XMIN_GuardExpressionMatchesInstalledTrigger(t *testing.T) {
	pool := testPool(t)
	var def string
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT pg_get_functiondef('sportsbook_bet_settlements_validate'::regproc)`).Scan(&def)
	})
	if err != nil {
		t.Fatalf("read installed sportsbook_bet_settlements_validate definition via pg_get_functiondef: %v", err)
	}
	if !strings.Contains(def, sbT1xminGuardVerbatimText) {
		t.Fatalf("the installed trigger's causation-branch reconstruction/guard text has DRIFTED from "+
			"migration 0093's expected text - the hand-copied probe in "+
			"TestSBT1XMIN_ReconstructionGuard_NullAndErrorCasesRejectClosed may now be testing a "+
			"stale expression.\nexpected to find (verbatim):\n%s\n\nactual installed definition:\n%s",
			sbT1xminGuardVerbatimText, def)
	}
}
