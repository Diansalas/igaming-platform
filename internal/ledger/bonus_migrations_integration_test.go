//go:build integration

// Migration mechanics for 0050 (bonus_expense account type) and 0051
// (the four bonus_* transaction types + the widened reason_code
// constraint) - ledger-accounting-model.md §7.15 items 1-4. Migration
// 0052 (player_bonus_held) is the SAME mechanical shape as 0050 (an
// additive ledger_accounts_account_type_check widening, same guarded-
// DO-$$-block pattern) and is exercised together with 0048/0050 by
// TestMigration0048_DownMigrationCleanThenFailsOnDirtyDatabase in
// migration_0048_integration_test.go rather than a third time here.
//
// Coverage map (§7.15 item -> test in this file):
//
//	1  (bonus_expense insert fails before 0050, succeeds after)
//	                             -> TestMigration0050_BonusExpenseInsertGatedByMigration
//	2  (0050's down succeeds clean, fails SQLSTATE 23514 dirty)
//	                             -> TestMigration0050_DownMigrationCleanThenFailsOnDirtyDatabase
//	3  (every pre-existing account type still inserts after 0050 -
//	    superset property proved by execution)
//	                             -> TestMigration0050_PreExistingAccountTypesStillInsert
//	4  (bonus_forfeiture WITH reason_code succeeds, WITHOUT fails; a
//	    deposit WITH a reason_code still fails - the constraint was
//	    WIDENED, not loosened to an implication)
//	                             -> TestMigration0051_ReasonCodeConstraintWidenedNotLoosened
package ledger

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
)

const (
	migration0050VersionForBonusMigTest = int64(50)
	migration0051Version                = int64(51)
)

// stagedMigrationsHolding is a more general sibling of
// migration_0048_integration_test.go's stagedMigrations: it holds back
// every migration file whose name starts with one of holdPrefixes
// (rather than a fixed set), so this file can isolate 0050/0051
// individually without perturbing 0048/0052's own dedicated round-trip
// test. Copying into a temp dir (never pointing at the repo) is what
// lets a test run the chain WITHOUT the held-back migrations and then
// add them, which is the only way to observe a migration's OWN gating
// behavior against a genuinely pre-migration database.
func stagedMigrationsHolding(t *testing.T, holdPrefixes []string) (dir string, addHeld func()) {
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
		name := e.Name()
		heldBack := false
		for _, prefix := range holdPrefixes {
			if strings.HasPrefix(name, prefix) {
				heldBack = true
				break
			}
		}
		if heldBack {
			held = append(held, name)
			continue
		}
		copyMigrationFile(t, src, dir, name)
	}
	wantHeld := 2 * len(holdPrefixes)
	if len(held) != wantHeld {
		t.Fatalf("expected to hold back exactly %d files (up+down for each of %v), held %v", wantHeld, holdPrefixes, held)
	}
	return dir, func() {
		for _, name := range held {
			copyMigrationFile(t, src, dir, name)
		}
	}
}

// holdPrefixesAfterVersion scans the REAL migrations directory (never
// the staged temp copy) and returns the "NNNN_" prefix for every
// migration whose version is strictly greater than maxVersion.
//
// Why this exists (a genuine defect this dispatch found and is fixing,
// not merely working around, per CLAUDE.md's "no specialist redesigns
// shared architecture unilaterally" / "name it precisely" instruction):
// TestMigration0050_DownMigrationCleanThenFailsOnDirtyDatabase originally
// hard-coded its "isolate 0050 as the sole most-recent migration" trick
// as holdPrefixes = {"0051_", "0052_"} - correct only as long as 0050/52
// happened to be the last migrations in the repository. Stage 4H-B1 Wave
// 2 Phase 2 landed eight new migrations (0053-0060) immediately
// afterwards, exactly as migration 0052's own comment anticipated
// ("0053-0054 are deliberately left unclaimed... for bonus-engine's own
// domain-table range") - and because stagedMigrationsHolding only holds
// back the prefixes it is TOLD to, those eight files were silently
// included in the staged directory, applied in the SAME MigrateUp call
// as 0050, and became the new most-recently-applied migrations. A
// single-step MigrateDown(1) then popped 0060, not 0050 - the down-
// migration equivalent of the exact "no automated way to confirm no
// intervening migration was added" hazard doc 27 §1.1 already warns
// about, now caught for real by this file's own regression run rather
// than left to recur every time a future stage adds another migration
// after this one. This helper generalizes the fix so it doesn't need to
// be hand-updated the NEXT time a migration lands after 0052 either.
func holdPrefixesAfterVersion(t *testing.T, maxVersion int) []string {
	t.Helper()
	src := migrationsDir(t)
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	seen := map[string]bool{}
	var prefixes []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		underscore := strings.IndexByte(e.Name(), '_')
		if underscore < 0 {
			continue
		}
		version, err := strconv.Atoi(e.Name()[:underscore])
		if err != nil {
			continue
		}
		if version <= maxVersion {
			continue
		}
		prefix := e.Name()[:underscore+1]
		if !seen[prefix] {
			seen[prefix] = true
			prefixes = append(prefixes, prefix)
		}
	}
	return prefixes
}

// TestMigration0050_BonusExpenseInsertGatedByMigration is §7.15 item 1.
// 0052 is held back alongside 0050 (0052 assumes 0050's exact prior
// value list as its own baseline - see this file's own package doc
// comment), so the chain stays coherent; 0051 is unaffected (it touches
// ledger_transactions, not ledger_accounts) and is left applied normally.
func TestMigration0050_BonusExpenseInsertGatedByMigration(t *testing.T) {
	scratchURL := scratchDatabase(t)
	pool := scratchPool(t, scratchURL)
	dir, addHeld := stagedMigrationsHolding(t, []string{"0050_", "0052_"})

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up without 0050/0052: %v", err)
	}
	if appliedVersions(t, pool)[migration0050VersionForBonusMigTest] {
		t.Fatal("migration 0050 must not have been applied yet")
	}

	// Before 0050: bonus_expense is rejected by the CHECK constraint.
	if err := insertRawAccount(pool, "bonus_expense"); err == nil {
		t.Fatal("bonus_expense must be rejected before migration 0050")
	} else {
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != pgCheckViolation {
			t.Fatalf("expected a check-violation PostgreSQL error, got %T: %v", err, err)
		}
	}

	addHeld()
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up 0050/0052: %v", err)
	}
	if !appliedVersions(t, pool)[migration0050VersionForBonusMigTest] {
		t.Fatal("migration 0050 should now be applied")
	}

	// After 0050: bonus_expense is accepted.
	if err := insertRawAccount(pool, "bonus_expense"); err != nil {
		t.Fatalf("bonus_expense must be accepted after migration 0050: %v", err)
	}
}

// insertRawAccount inserts a single house-level ledger_accounts row of
// accountType against a fresh tenant/asset, returning any database error
// unwrapped. Uses raw SQL (never internal/ledger.GetOrCreateAccount) so
// it can express account types this package's Go consts may not cover at
// every point in a migration test's staged chain.
func insertRawAccount(pool *db.Pool, accountType string) error {
	tenantID := uuid.New()
	return pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'Bonus Migration Test Tenant', 'under_platform_licence')`,
			tenantID, "bmt-"+tenantID.String()[:8]); err != nil {
			return err
		}
		return pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO ledger_accounts (id, tenant_id, wallet_id, account_type, asset_code, status)
				 VALUES ($1, $2, NULL, $3, 'EUR', 'active')`,
				uuid.New(), tenantID, accountType)
			return err
		})
	})
}

// TestMigration0050_DownMigrationCleanThenFailsOnDirtyDatabase is §7.15
// item 2, isolated to 0050 alone (0052 held back throughout, so 0050's
// down migration is never blocked by 0052 depending on it).
func TestMigration0050_DownMigrationCleanThenFailsOnDirtyDatabase(t *testing.T) {
	scratchURL := scratchDatabase(t)
	pool := scratchPool(t, scratchURL)
	// 0051 is also held back here even though it does not touch
	// ledger_accounts at all: left applied, it would land AFTER 0050
	// (ascending version order) and become the most-recently-applied
	// migration, so MigrateDown(1) would target 0051 instead of 0050 -
	// unrelated to what this test is isolating. Holding it back keeps
	// 0050 as the sole most-recent migration for a clean single-step
	// MigrateDown(1).
	// EVERY migration numbered above 0050 is excluded from dir for this
	// test's ENTIRE lifetime (their addHeld callback is deliberately
	// never invoked): this test isolates 0050 alone, and re-introducing
	// any of them later would make a subsequent MigrateUp/MigrateDown(1)
	// target one of them instead of 0050, defeating that isolation.
	// holdPrefixesAfterVersion (this file's own generalized fix for the
	// regression Stage 4H-B1 Wave 2 Phase 2's migrations 0053-0060
	// exposed - see that function's own doc comment) computes this
	// dynamically rather than the original hard-coded {"0051_", "0052_"},
	// so it stays correct the next time a migration lands after 0052
	// too.
	dir, _ := stagedMigrationsHolding(t, holdPrefixesAfterVersion(t, int(migration0050VersionForBonusMigTest)))

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up through 0050 (0051/0052 excluded): %v", err)
	}
	if !appliedVersions(t, pool)[migration0050VersionForBonusMigTest] {
		t.Fatal("migration 0050 should be applied")
	}

	// Clean rollback.
	rolledBack, err := pool.MigrateDown(context.Background(), dir, 1)
	if err != nil {
		t.Fatalf("down migration must succeed on a database with no bonus_expense account: %v", err)
	}
	if len(rolledBack) != 1 || rolledBack[0] != migration0050VersionForBonusMigTest {
		t.Fatalf("expected exactly migration 50 to be rolled back, got %v", rolledBack)
	}
	if strings.Contains(accountTypeCheckDef(t, pool), "bonus_expense") {
		t.Fatal("restored constraint must not admit bonus_expense")
	}

	// Round trip: up again.
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("re-applying migration 0050: %v", err)
	}

	// Dirty rollback: a real bonus_expense account now exists.
	if err := insertRawAccount(pool, "bonus_expense"); err != nil {
		t.Fatalf("seed a bonus_expense account: %v", err)
	}
	widened := accountTypeCheckDef(t, pool)

	_, err = pool.MigrateDown(context.Background(), dir, 1)
	if err == nil {
		t.Fatal("the down migration must FAIL once a bonus_expense account exists")
	}
	// Unlike migration 0048's down (a bare ALTER, letting a raw SQLSTATE
	// 23514 propagate unmodified), migration 0050's down is DELIBERATELY
	// wrapped in a DO $$ ... EXCEPTION WHEN check_violation ... RAISE
	// EXCEPTION block ("THIS is the direction where the wrapper... earns
	// its place" - ledger-accounting-model.md §7.2's own down.sql
	// comment) so the down-narrowing failure carries a legible, remedy-
	// naming message. plpgsql's RAISE EXCEPTION with no explicit SQLSTATE
	// defaults to P0001, not the underlying check_violation's own 23514 -
	// this test asserts the ACTUAL behavior the migration was written to
	// have, not the unwrapped 0048 pattern.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a PostgreSQL error, got %T: %v", err, err)
	}
	if pgErr.Code != "P0001" {
		t.Fatalf("expected SQLSTATE P0001 (the guard's own RAISE EXCEPTION), got %s: %v", pgErr.Code, err)
	}
	for _, want := range []string{
		"migration 0050 (down)",
		"bonus_expense ledger account exists",
		"constraint validation",
		"row-level security cannot filter",
		"irreversible",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the guard's exception must mention %q, got: %v", want, err)
		}
	}
	if !appliedVersions(t, pool)[migration0050VersionForBonusMigTest] {
		t.Fatal("a failed rollback must leave migration 0050 recorded as applied")
	}
	if after := accountTypeCheckDef(t, pool); after != widened {
		t.Fatalf("a failed rollback must leave the constraint untouched:\nbefore: %s\nafter:  %s", widened, after)
	}
}

// TestMigration0050_PreExistingAccountTypesStillInsert is §7.15 item 3's
// "superset property proved by execution" half (the GetSummary-unaffected
// half is internal/wallet's, unchanged by this migration and therefore
// not re-tested here).
func TestMigration0050_PreExistingAccountTypesStillInsert(t *testing.T) {
	pool := testPool(t) // the shared, already-fully-migrated test database
	for _, accountType := range []string{
		"player_cash", "player_bonus", "player_locked_cash", "player_locked_bonus",
		"player_withdrawal_hold", "house_gaming", "provider_payable", "psp_clearing",
		"psp_reserve", "jackpot_contribution", "promo_liability", "manual_adjustment",
	} {
		if err := insertRawAccount(pool, accountType); err != nil {
			t.Fatalf("pre-existing account type %q must still insert after migration 0050: %v", accountType, err)
		}
	}
}

// TestMigration0051_ReasonCodeConstraintWidenedNotLoosened is §7.15 item
// 4, run against the shared, already-migrated test database (0051 is
// unconditionally applied there, like every other migration up to HEAD).
func TestMigration0051_ReasonCodeConstraintWidenedNotLoosened(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	a := seedBonusAccounts(t, pool, f, f.walletID, "EUR")

	// bonus_forfeiture WITH a reason code: accepted (via Post, which also
	// exercises the Go-side copy of this rule).
	forfeiture := mustPost(t, pool, f, TransactionInput{
		TenantID:        f.tenantID,
		TransactionType: TxBonusForfeiture,
		IdempotencyKey:  "reason-code-widened-forfeiture-with-reason",
		CorrelationID:   uuid.New(),
		ReasonCode:      strPtr("expired"),
		Entries:         []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Debit, Amount: 10}},
		BonusCost:       &BonusCostAttribution{Funding: FundingOperator},
	})
	if forfeiture.AlreadyPosted {
		t.Fatal("expected a new posting")
	}

	// bonus_forfeiture WITHOUT a reason code: rejected, at the Go
	// boundary (ErrInvalidEntry) - which the database constraint would
	// also reject if reached directly, but Post's own check fires first.
	var postErr error
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, postErr = Post(ctx, tx, TransactionInput{
			TenantID:        f.tenantID,
			TransactionType: TxBonusForfeiture,
			IdempotencyKey:  "reason-code-widened-forfeiture-without-reason",
			CorrelationID:   uuid.New(),
			Entries:         []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Debit, Amount: 10}},
			BonusCost:       &BonusCostAttribution{Funding: FundingOperator},
		})
		return nil
	})
	if err != nil {
		t.Fatalf("transaction wrapper failed: %v", err)
	}
	if !errors.Is(postErr, ErrInvalidEntry) {
		t.Fatalf("expected ErrInvalidEntry for a bonus_forfeiture with no reason code, got %v", postErr)
	}

	// The DATABASE constraint itself, bypassing Post's Go-side check
	// entirely (raw SQL), proving the requirement is enforced twice - at
	// the boundary AND at the database, per §7.3.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, correlation_id, reason_code)
			 VALUES ($1, $2, 'bonus_forfeiture', $3, $4, NULL)`,
			uuid.New(), f.tenantID, "reason-code-widened-raw-sql-no-reason", uuid.New())
		return err
	})
	if err == nil {
		t.Fatal("the database constraint must reject a bonus_forfeiture row with no reason_code")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgCheckViolation {
		t.Fatalf("expected a check-violation PostgreSQL error, got %T: %v", err, err)
	}

	// Control: a deposit WITH a reason code still fails - the constraint
	// was WIDENED to a larger equality set (manual_adjustment,
	// bonus_forfeiture), never loosened to a one-directional implication
	// that would let ANY type carry an optional reason code.
	provider, providerTx := "mockpsp", "reason-code-widened-deposit-control"
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, postErr = Post(ctx, tx, TransactionInput{
			TenantID:        f.tenantID,
			TransactionType: TxDeposit,
			IdempotencyKey:  "reason-code-widened-deposit-control",
			ProviderID:      &provider,
			ProviderTxID:    &providerTx,
			CorrelationID:   uuid.New(),
			ReasonCode:      strPtr("should not be allowed here"),
			Entries: []EntryInput{
				{LedgerAccountID: f.clearingID, Direction: Debit, Amount: 10},
				{LedgerAccountID: f.cashAccountID, Direction: Credit, Amount: 10},
			},
		})
		return nil
	})
	if err != nil {
		t.Fatalf("transaction wrapper failed: %v", err)
	}
	if postErr == nil {
		t.Fatal("a deposit carrying a reason_code must still be rejected by the DATABASE constraint " +
			"(Post itself has no Go-side check for this direction, so this proves the DB constraint, not Post's copy)")
	}
}
