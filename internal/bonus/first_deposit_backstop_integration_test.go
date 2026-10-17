//go:build integration

// Migration 0173 (BONUS-FIRST-DEPOSIT-DOUBLE-GRANT-1, ledger-finance review condition C1): the database backstop for
// "one FirstDepositOnly Grant per (tenant, offer, player)". Direct SQL inserts, concurrency, and the migration's own
// refuse / up / down / up behaviour on THROWAWAY scratch databases.
package bonus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

const firstDepositIndex = "bonus_grants_one_first_deposit_per_player_offer"

func insertGrantRaw(ctx context.Context, tx pgx.Tx, f lifecycleFixture, co campaignOffer, playerID, walletID uuid.UUID, triggerRef string, explicitFDO *bool) error {
	cols, vals := "", ""
	args := []any{uuid.New(), f.tenantID, f.brandID, playerID, walletID, co.campaignID, co.campaignVersionID, co.offerID, co.offerVersionID, f.assetCode, triggerRef, f.staffID}
	if explicitFDO != nil {
		cols, vals = ", first_deposit_only", ", $13"
		args = append(args, *explicitFDO)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO bonus_grants (id, tenant_id, brand_id, player_account_id, wallet_id, campaign_id, campaign_version_id,
			offer_id, offer_version_id, asset_code, decimal_exponent, funding_source, fulfillment_destination, fulfillment_owner,
			trigger_reference, status, created_by_actor_type, created_by_actor_id`+cols+`)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,2,'operator','into_platform_wallet','internal',$11,'issued','staff',$12`+vals+`)`, args...)
	return err
}

func rawGrant(t *testing.T, pool *db.Pool, f lifecycleFixture, co campaignOffer, ref string, explicit *bool) error {
	t.Helper()
	return pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return insertGrantRaw(ctx, tx, f, co, f.playerID, f.walletID, ref, explicit)
	})
}

func assertUniqueFirstDeposit(t *testing.T, err error) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgUniqueViolation || pgErr.ConstraintName != firstDepositIndex {
		t.Fatalf("want SQLSTATE 23505 on %s, got %v", firstDepositIndex, err)
	}
}

func TestFirstDepositBackstop_DirectInsertRefusedAnyStatusAnyDerivedFlag(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	fdo := seedDepositMatchableOffer(t, pool, f, true, nil)
	if err := rawGrant(t, pool, f, fdo, "dep-1", nil); err != nil {
		t.Fatalf("first FirstDepositOnly grant: %v", err)
	}
	assertUniqueFirstDeposit(t, rawGrant(t, pool, f, fdo, "dep-2", nil))
	f2 := false // the column is DERIVED from the offer version: a caller-supplied value is overridden, not trusted
	assertUniqueFirstDeposit(t, rawGrant(t, pool, f, fdo, "dep-3", &f2))
	// Terminal status does not free the slot.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE bonus_grants SET status = 'cancelled', terminal_at = clock_timestamp(), terminal_resolution = 'cancelled' WHERE tenant_id = $1 AND offer_id = $2`, f.tenantID, fdo.offerID)
		return e
	}); err != nil {
		t.Fatalf("cancel first grant: %v", err)
	}
	assertUniqueFirstDeposit(t, rawGrant(t, pool, f, fdo, "dep-4", nil))
	// The derived flag is immutable.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE bonus_grants SET first_deposit_only = false WHERE tenant_id = $1 AND offer_id = $2`, f.tenantID, fdo.offerID)
		return e
	})
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("first_deposit_only must be immutable, got %v", err)
	}
	// A different player holds their own slot; a non-FirstDepositOnly offer is unconstrained.
	p2, w2 := seedSecondPlayerAccountSamePerson(t, pool, f)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return insertGrantRaw(ctx, tx, f, fdo, p2, w2, "dep-5", nil)
	}); err != nil {
		t.Fatalf("other player's first deposit grant: %v", err)
	}
	plain := seedDepositMatchableOffer(t, pool, f, false, nil)
	for _, ref := range []string{"a", "b", "c"} {
		if err := rawGrant(t, pool, f, plain, ref, nil); err != nil {
			t.Fatalf("non-FirstDepositOnly offer must stay unconstrained: %v", err)
		}
	}
}

func TestFirstDepositBackstop_ConcurrentInsertsExactlyOneWins(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	fdo := seedDepositMatchableOffer(t, pool, f, true, nil)
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- rawGrant2(pool, f, fdo, "race-"+strconv.Itoa(i))
		}(i)
	}
	wg.Wait()
	close(errs)
	won := 0
	for e := range errs {
		if e == nil {
			won++
			continue
		}
		assertUniqueFirstDeposit(t, e)
	}
	if won != 1 {
		t.Fatalf("exactly one concurrent insert must win, got %d", won)
	}
}

func rawGrant2(pool *db.Pool, f lifecycleFixture, co campaignOffer, ref string) error {
	return pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return insertGrantRaw(ctx, tx, f, co, f.playerID, f.walletID, ref, nil)
	})
}

func migrationsThrough(t *testing.T, through int64) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") || len(name) < 4 {
			continue
		}
		v, err := strconv.Atoi(name[:4])
		if err != nil || int64(v) > through {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func bonusGrantsSnapshot(t *testing.T, pool *db.Pool) string {
	t.Helper()
	var snap string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(string_agg(x, E'\n' ORDER BY x), '') FROM (
			SELECT 'column:' || column_name || ':' || data_type || ':' || is_nullable || ':' || coalesce(column_default, '') AS x
			  FROM information_schema.columns WHERE table_schema = 'public' AND table_name = 'bonus_grants'
			UNION ALL SELECT 'index:' || indexname || ':' || indexdef FROM pg_indexes WHERE schemaname = 'public' AND tablename = 'bonus_grants'
			UNION ALL SELECT 'trigger:' || t.tgname || ':' || pg_get_triggerdef(t.oid) FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
			   WHERE NOT t.tgisinternal AND c.relname = 'bonus_grants'
			UNION ALL SELECT 'function:' || p.proname FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace AND p.proname LIKE 'bonus_grants%'
			UNION ALL SELECT 'rls:' || relrowsecurity || ':' || relforcerowsecurity FROM pg_class WHERE relname = 'bonus_grants') s`).Scan(&snap)
	}); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return snap
}

func scratchPool(t *testing.T, prefix string) *db.Pool {
	t.Helper()
	pool, err := db.Connect(context.Background(), scratchdb.New(t, prefix), 8, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// The up REFUSES (SQLSTATE BG173) while two FirstDepositOnly grants already exist for one (tenant, offer, player),
// and changes nothing (the migration transaction rolls back).
func TestMigration0173_RefusesWhileDuplicateFirstDepositGrantsExist(t *testing.T) {
	ctx := context.Background()
	pool := scratchPool(t, "m0173r_")
	if _, err := pool.MigrateUp(ctx, migrationsThrough(t, 172)); err != nil {
		t.Fatalf("migrate to 0172: %v", err)
	}
	before := bonusGrantsSnapshot(t, pool)
	f := seedLifecycleFixture(t, pool)
	fdo := seedDepositMatchableOffer(t, pool, f, true, nil)
	for _, ref := range []string{"dup-1", "dup-2"} { // possible on 0172: no backstop yet
		if err := rawGrantNoDerived(pool, f, fdo, ref); err != nil {
			t.Fatalf("seed duplicate on 0172: %v", err)
		}
	}
	_, err := pool.MigrateUp(ctx, migrationsThrough(t, 173))
	var pgErr *pgconn.PgError
	if err == nil || !(errors.As(err, &pgErr) && pgErr.Code == "BG173") {
		t.Fatalf("0173 up must refuse with BG173 while duplicates exist, got %v", err)
	}
	if got := bonusGrantsSnapshot(t, pool); got != before {
		t.Fatalf("a refused 0173 up must change nothing:\nbefore=%s\nafter=%s", before, got)
	}
}

func rawGrantNoDerived(pool *db.Pool, f lifecycleFixture, co campaignOffer, ref string) error {
	return rawGrant2(pool, f, co, ref) // on 0172 the insert has no first_deposit_only column at all
}

// Clean data: up applies (existing FirstDepositOnly grants backfilled), down restores the 0172 shape exactly, up again
// equals the first up.
func TestMigration0173_UpBackfillDownUp(t *testing.T) {
	ctx := context.Background()
	pool := scratchPool(t, "m0173u_")
	if _, err := pool.MigrateUp(ctx, migrationsThrough(t, 172)); err != nil {
		t.Fatalf("migrate to 0172: %v", err)
	}
	before := bonusGrantsSnapshot(t, pool)
	f := seedLifecycleFixture(t, pool)
	fdo := seedDepositMatchableOffer(t, pool, f, true, nil)
	if err := rawGrant2(pool, f, fdo, "only-1"); err != nil {
		t.Fatalf("seed pre-existing first-deposit grant: %v", err)
	}
	full := migrationsThrough(t, 173)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("0173 up: %v", err)
	}
	up := bonusGrantsSnapshot(t, pool)
	if up == before || !strings.Contains(up, firstDepositIndex) {
		t.Fatal("0173 must create the partial unique index (non-vacuity)")
	}
	var flagged int
	if err := pool.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM bonus_grants WHERE first_deposit_only`).Scan(&flagged)
	}); err != nil || flagged != 1 {
		t.Fatalf("pre-existing FirstDepositOnly grant must be backfilled: flagged=%d err=%v", flagged, err)
	}
	assertUniqueFirstDeposit(t, rawGrant2(pool, f, fdo, "late-2"))
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("0173 down: %v", err)
	}
	if got := bonusGrantsSnapshot(t, pool); got != before {
		t.Fatalf("0173 down must restore the 0172 shape:\nwant=%s\ngot=%s", before, got)
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("0173 up again: %v", err)
	}
	if got := bonusGrantsSnapshot(t, pool); got != up {
		t.Fatalf("0173 up again must equal the first up:\nwant=%s\ngot=%s", up, got)
	}
}
