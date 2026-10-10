//go:build integration

// ADR 0112 slice 1, migration 0128 on THROWAWAY scratch databases: S12 backfill with data
// present (count equality, (id, status) byte-identical), defaults, S13 down refusals before
// anything is dropped, and a whole-schema up/down/up snapshot. Requires TEST_DATABASE_URL and
// TEST_ADMIN_DATABASE_URL (scratchdb); a skip is NOT evidence.
package tenant

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

const migration0128Version = 128

func lgMigDir(t *testing.T, through int64) string {
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
		if err != nil {
			t.Fatalf("migration %q has no numeric prefix", name)
		}
		if int64(v) > through {
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

func lgScratch(t *testing.T, prefix string) (*db.Pool, string) {
	t.Helper()
	u := scratchdb.New(t, prefix)
	pool, err := db.Connect(context.Background(), u, 10, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, u
}

func lgSnapshot(t *testing.T, pool *db.Pool) string {
	t.Helper()
	var snap string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT string_agg(x, E'\n' ORDER BY x) FROM (
			SELECT 'policy:' || tablename || ':' || policyname || ':' || permissive || ':' || cmd || ':' || coalesce(qual, '') || ':' || coalesce(with_check, '') AS x
			  FROM pg_policies WHERE schemaname = 'public'
			UNION ALL
			SELECT 'trigger:' || c.relname || ':' || t.tgname || ':' || pg_get_triggerdef(t.oid)
			  FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
			 WHERE NOT t.tgisinternal AND c.relnamespace = 'public'::regnamespace
			UNION ALL
			SELECT 'constraint:' || conrelid::regclass::text || ':' || conname || ':' || pg_get_constraintdef(oid)
			  FROM pg_constraint WHERE connamespace = 'public'::regnamespace
			UNION ALL
			SELECT 'table:' || tablename || ':rls=' || c.relrowsecurity || ':force=' || c.relforcerowsecurity
			  FROM pg_tables pt JOIN pg_class c ON c.relname = pt.tablename AND c.relnamespace = 'public'::regnamespace
			 WHERE pt.schemaname = 'public'
			UNION ALL
			SELECT 'column:' || table_name || ':' || column_name || ':' || data_type || ':' || is_nullable || ':' || coalesce(column_default, '')
			  FROM information_schema.columns WHERE table_schema = 'public'
			UNION ALL
			SELECT 'function:' || p.oid::regprocedure::text || ':' || md5(p.prosrc) || ':' || coalesce(array_to_string(p.proconfig, ','), '')
			  FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace
			UNION ALL
			SELECT 'index:' || tablename || ':' || indexname || ':' || indexdef FROM pg_indexes WHERE schemaname = 'public') s`).Scan(&snap)
	}); err != nil {
		t.Fatalf("schema snapshot: %v", err)
	}
	return snap
}

func lgSnapDiff(a, b string) string {
	as, bs := map[string]bool{}, map[string]bool{}
	for _, l := range strings.Split(a, "\n") {
		as[l] = true
	}
	for _, l := range strings.Split(b, "\n") {
		bs[l] = true
	}
	var out []string
	for l := range as {
		if !bs[l] {
			out = append(out, "- "+l)
		}
	}
	for l := range bs {
		if !as[l] {
			out = append(out, "+ "+l)
		}
	}
	sort.Strings(out)
	if len(out) > 20 {
		out = append(out[:20], fmt.Sprintf("... (%d differences)", len(out)))
	}
	return strings.Join(out, "\n")
}

// lgIDStatus is a digest of every (id, status) of both subject tables.
func lgIDStatus(t *testing.T, pool *db.Pool) (digest string, tenants, brands int) {
	t.Helper()
	var rows []string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		r, err := tx.Query(ctx, `SELECT 't:' || id::text || ':' || status FROM tenants UNION ALL SELECT 'b:' || id::text || ':' || status FROM brands ORDER BY 1`)
		if err != nil {
			return err
		}
		rows, err = pgx.CollectRows(r, pgx.RowTo[string])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256([]byte(strings.Join(rows, "\n")))
	for _, r := range rows {
		if strings.HasPrefix(r, "t:") {
			tenants++
		} else {
			brands++
		}
	}
	return hex.EncodeToString(h[:]), tenants, brands
}

func lgSeedLegacy(t *testing.T, pool *db.Pool) {
	t.Helper()
	ctx := context.Background()
	for i, st := range []string{"active", "suspended", "closed", "active"} {
		tid := uuid.New()
		if err := pool.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model) VALUES ($1, $2, $3, 'under_platform_licence')`, tid, "legacy", fmt.Sprintf("legacy-%d-%s", i, tid)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE tenants SET status = $1 WHERE id = $2`, st, tid)
			return err
		}); err != nil {
			t.Fatalf("seed legacy tenant: %v", err)
		}
		for j, bst := range []string{"active", st} {
			bid := uuid.New()
			if err := pool.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, name, slug) VALUES ($1, $2, 'b', $3)`, bid, tid, fmt.Sprintf("lb-%d-%d-%s", i, j, bid)); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `UPDATE brands SET status = $1 WHERE id = $2`, bst, bid)
				return err
			}); err != nil {
				t.Fatalf("seed legacy brand: %v", err)
			}
		}
	}
}

func lgBaselines(t *testing.T, pool *db.Pool) (n int, mismatched int) {
	t.Helper()
	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM launch_status_transitions WHERE kind = 'legacy_baseline' AND from_status IS NULL AND actor_type = 'system'`).Scan(&n); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			SELECT count(*) FROM launch_status_transitions x
			 WHERE x.kind = 'legacy_baseline'
			   AND x.to_status IS DISTINCT FROM CASE x.subject_kind
			       WHEN 'tenant' THEN (SELECT status FROM tenants WHERE id = x.tenant_id)
			       ELSE (SELECT status FROM brands WHERE id = x.brand_id) END`).Scan(&mismatched)
	}); err != nil {
		t.Fatal(err)
	}
	return n, mismatched
}

func TestMigration0128_S12_BackfillWithData_And_DownRefusals(t *testing.T) {
	ctx := context.Background()
	pool, url := lgScratch(t, "m0128_")
	if _, err := pool.MigrateUp(ctx, lgMigDir(t, migration0128Version-1)); err != nil {
		t.Fatalf("migrate through 0127: %v", err)
	}
	lgSeedLegacy(t, pool)
	before, nt, nb := lgIDStatus(t, pool)
	if nt != 4 || nb != 8 {
		t.Fatalf("seed: %d tenants %d brands", nt, nb)
	}

	full := lgMigDir(t, migration0128Version)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("migrate 0128 over existing data: %v", err)
	}
	after, _, _ := lgIDStatus(t, pool)
	if before != after {
		t.Fatal("(id, status) of existing rows changed by 0128")
	}
	if n, bad := lgBaselines(t, pool); n != nt+nb || bad != 0 {
		t.Fatalf("legacy_baseline rows = %d (want %d), mismatching the subject status: %d", n, nt+nb, bad)
	}
	var tdef, bdef string
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT column_default FROM information_schema.columns WHERE table_name = 'tenants' AND column_name = 'status'`).Scan(&tdef); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT column_default FROM information_schema.columns WHERE table_name = 'brands' AND column_name = 'status'`).Scan(&bdef)
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tdef, "pending_launch") || !strings.Contains(bdef, "pending_launch") {
		t.Fatalf("defaults after up: tenants=%q brands=%q", tdef, bdef)
	}

	// A pending_launch row blocks the down (the narrowed CHECK could not hold it) ...
	pending := uuid.New()
	if err := pool.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model) VALUES ($1, 'p', $2, 'under_platform_licence')`, pending, "pend-"+pending.String())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// ... and a governed transition blocks it regardless; both are checked BEFORE anything is dropped.
	launchfix.SetTenantStatusAt(t, url, lgFirstActiveTenant(t, pool), "suspended")
	for _, what := range []string{"governed transition + pending_launch row"} {
		_, err := pool.MigrateDown(ctx, full, 1)
		if err == nil || pgCodeOf(err) != "LA099" {
			t.Fatalf("%s: down must refuse with LA099, got %v", what, err)
		}
	}
	assertAllLaunchObjectsPresent(t, pool)

	// Remove only the pending row's blocker (a pending tenant has no history, so it is deletable):
	// the governed transition alone still blocks.
	if err := pool.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, pending)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.MigrateDown(ctx, full, 1); pgCodeOf(err) != "LA099" {
		t.Fatalf("a governed transition alone must refuse the down with LA099, got %v", err)
	}
	assertAllLaunchObjectsPresent(t, pool)
}

func lgFirstActiveTenant(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM tenants WHERE status = 'active' ORDER BY id LIMIT 1`).Scan(&id)
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func assertAllLaunchObjectsPresent(t *testing.T, pool *db.Pool) {
	t.Helper()
	var n int
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_tables WHERE schemaname = 'public' AND tablename LIKE 'launch\_%')
			+ (SELECT count(*) FROM pg_trigger WHERE tgname LIKE 'zz\_launch\_%' AND NOT tgisinternal)
			+ (SELECT count(*) FROM pg_proc WHERE proname LIKE 'launch\_%')`).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	if n < 3+6+12 {
		t.Fatalf("a refused down dropped something: %d launch objects remain", n)
	}
	var def string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT column_default FROM information_schema.columns WHERE table_name = 'tenants' AND column_name = 'status'`).Scan(&def)
	}); err != nil || !strings.Contains(def, "pending_launch") {
		t.Fatalf("a refused down changed the default: %q err=%v", def, err)
	}
}

func TestMigration0128_UpDownUp_WholeSchema_And_CleanDown(t *testing.T) {
	ctx := context.Background()
	pool, _ := lgScratch(t, "m0128s_")
	if _, err := pool.MigrateUp(ctx, lgMigDir(t, migration0128Version-1)); err != nil {
		t.Fatal(err)
	}
	snap127 := lgSnapshot(t, pool)
	lgSeedLegacy(t, pool)
	before, nt, nb := lgIDStatus(t, pool)
	full := lgMigDir(t, migration0128Version)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	snap128 := lgSnapshot(t, pool)
	if snap127 == snap128 {
		t.Fatal("0128 changed nothing in the schema snapshot (vacuous)")
	}
	// Down with only legacy_baseline history (no decision) is allowed and restores byte-for-byte.
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("down with only legacy_baseline rows: %v", err)
	}
	if got := lgSnapshot(t, pool); got != snap127 {
		t.Fatalf("down did not restore the 0127 schema:\n%s", lgSnapDiff(snap127, got))
	}
	if after, _, _ := lgIDStatus(t, pool); after != before {
		t.Fatal("(id, status) changed across down")
	}
	var def string
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT column_default FROM information_schema.columns WHERE table_name = 'tenants' AND column_name = 'status'`).Scan(&def)
	}); err != nil || !strings.Contains(def, "active") {
		t.Fatalf("default after down: %q err=%v", def, err)
	}
	// pending_launch is no longer storable after the down.
	err := pool.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model, status) VALUES ($1, 'x', $2, 'under_platform_licence', 'pending_launch')`, uuid.New(), "x-"+uuid.NewString())
		return err
	})
	if pgCodeOf(err) != "23514" {
		t.Fatalf("pending_launch after down must violate the CHECK, got %v", err)
	}
	// Up again: identical to the first up, and the backfill again equals the source count.
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	if got := lgSnapshot(t, pool); got != snap128 {
		t.Fatalf("up after down differs from the first up:\n%s", lgSnapDiff(snap128, got))
	}
	if n, bad := lgBaselines(t, pool); n != nt+nb || bad != 0 {
		t.Fatalf("second backfill: %d rows (want %d), mismatching: %d", n, nt+nb, bad)
	}
}
