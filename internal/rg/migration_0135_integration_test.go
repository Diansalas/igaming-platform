//go:build integration

// ADR 0115 S1, migration 0135 on THROWAWAY scratch databases: a whole-schema up/down/up snapshot (the
// down restores 0130's actor_proof_require and 0037's player_restrictions CHECK / player_self_insert
// byte for byte), the down's refusal while RG data exists (RLS-independent), and the
// DEVOPS-0107-INDEX-WINDOW-1 practice: an index pre-built CONCURRENTLY is accepted only if VALID and
// identically defined. Requires TEST_DATABASE_URL and TEST_ADMIN_DATABASE_URL; a skip is NOT evidence.
package rg

import (
	"context"
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
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

const migration0135Version = 135

func m135Dir(t *testing.T, through int64) string {
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
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
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

func m135Scratch(t *testing.T, prefix string) (*db.Pool, string) {
	t.Helper()
	u := scratchdb.New(t, prefix)
	pool, err := db.Connect(context.Background(), u, 6, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, u
}

func m135Snapshot(t *testing.T, pool *db.Pool) string {
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
			SELECT 'function:' || p.oid::regprocedure::text || ':' || md5(p.prosrc) || ':' || coalesce(array_to_string(p.proconfig, ','), '') || ':' || p.prosecdef
			  FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace
			UNION ALL
			SELECT 'acl:' || c.relname || ':' || coalesce(c.relacl::text, '')
			  FROM pg_class c WHERE c.relnamespace = 'public'::regnamespace AND c.relkind IN ('r', 'S')
			UNION ALL
			SELECT 'index:' || tablename || ':' || indexname || ':' || indexdef FROM pg_indexes WHERE schemaname = 'public') s`).Scan(&snap)
	}); err != nil {
		t.Fatalf("schema snapshot: %v", err)
	}
	return snap
}

func m135Diff(a, b string) string {
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
	if len(out) > 30 {
		out = append(out[:30], fmt.Sprintf("... (%d differences)", len(out)))
	}
	return strings.Join(out, "\n")
}

func TestMigration0135_UpDownUp_WholeSchema_RestoresPriorBodiesVerbatim(t *testing.T) {
	ctx := context.Background()
	pool, _ := m135Scratch(t, "m0135s_")
	prev := m135Dir(t, migration0135Version-1)
	if _, err := pool.MigrateUp(ctx, prev); err != nil {
		t.Fatal(err)
	}
	before := m135Snapshot(t, pool)
	full := m135Dir(t, migration0135Version)
	applied, err := pool.MigrateUp(ctx, full)
	if err != nil || len(applied) != 1 || applied[0] != migration0135Version {
		t.Fatalf("up 0135: %v %v", applied, err)
	}
	after := m135Snapshot(t, pool)
	if before == after {
		t.Fatal("0135 changed nothing in the schema snapshot (vacuous)")
	}
	for _, must := range []string{
		"table:rg_limit_configs:rls=true:force=true", "table:player_rg_limit_changes:rls=true:force=true",
		"table:rg_limit_config_change_requests:rls=true:force=true", "table:database_environment:rls=true:force=false",
		"column:casino_launch_sessions:auth_session_root_started_at:timestamp with time zone:YES:",
		"constraint:player_restrictions:player_restrictions_time_out_bounded:",
		"index:ledger_entries:idx_ledger_entries_tenant_player_asset_created:CREATE INDEX idx_ledger_entries_tenant_player_asset_created ON public.ledger_entries USING btree (tenant_id, player_account_id, asset_code, created_at)",
		"index:deposit_intents:idx_deposit_intents_tenant_player_asset_created:",
	} {
		if !strings.Contains(after, must) {
			t.Errorf("0135 snapshot lacks %q", must)
		}
	}
	var src string
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT prosrc FROM pg_proc WHERE proname = 'actor_proof_require'`).Scan(&src)
	}); err != nil || !strings.Contains(src, `rg\_limit\_config:%`) {
		t.Fatalf("the verifier must carry the rg_limit_config family: err=%v", err)
	}
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("down with no RG data: %v", err)
	}
	if got := m135Snapshot(t, pool); got != before {
		t.Fatalf("down did not restore the pre-0135 schema byte for byte:\n%s", m135Diff(before, got))
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	if got := m135Snapshot(t, pool); got != after {
		t.Fatalf("up after down differs from the first up:\n%s", m135Diff(after, got))
	}
}

// The down refuses BEFORE dropping anything while RG history exists, even rows its session cannot
// see through RLS (an operator row of a tenant, a player change), and the up/down state is intact.
func TestMigration0135_Down_RefusesWhileRGDataExists(t *testing.T) {
	ctx := context.Background()
	pool, _ := m135Scratch(t, "m0135d_")
	full := m135Dir(t, migration0135Version)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	// A time_out restriction only (tenant-scoped: invisible to the migrator's GUC-less session).
	var tenantID, person string
	if err := pool.WithPlatformAdmin(ctx, mustUUID(), func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO tenants (name, slug, licensing_model) VALUES ('m0135', $1, 'under_platform_licence') RETURNING id::text`,
			"m0135-"+mustUUID().String()[:8]).Scan(&tenantID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO persons DEFAULT VALUES RETURNING id::text`).Scan(&person)
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithTenant(ctx, mustParse(tenantID), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO player_restrictions (person_id, tenant_id, restriction_type, ends_at, source, created_by_actor_type, created_by_actor_id)
			VALUES ($1, $2, 'time_out', now() + interval '1 hour', 'staff', 'staff', $1)`, person, tenantID)
		return err
	}); err != nil {
		t.Fatalf("seed a tenant-scoped time_out: %v", err)
	}
	before := m135Snapshot(t, pool)
	_, err := pool.MigrateDown(ctx, full, 1)
	if codeOf(err) != "RL099" {
		t.Fatalf("down must refuse with RL099 while a time_out exists, got %v", err)
	}
	if got := m135Snapshot(t, pool); got != before {
		t.Fatalf("a refused down changed the schema:\n%s", m135Diff(before, got))
	}
}

// DEVOPS-0107-INDEX-WINDOW-1 practice: a CONCURRENTLY pre-built index is accepted only if VALID and
// identically defined; a wrongly defined one is refused before anything is created (RL098).
func TestMigration0135_PrebuiltConcurrentIndex(t *testing.T) {
	ctx := context.Background()
	pool, _ := m135Scratch(t, "m0135i_")
	if _, err := pool.MigrateUp(ctx, m135Dir(t, migration0135Version-1)); err != nil {
		t.Fatal(err)
	}
	full := m135Dir(t, migration0135Version)
	// Wrong definition under the expected name.
	if _, err := pool.Raw().Exec(ctx, `CREATE INDEX CONCURRENTLY idx_ledger_entries_tenant_player_asset_created ON ledger_entries (tenant_id, created_at)`); err != nil {
		t.Fatal(err)
	}
	_, err := pool.MigrateUp(ctx, full)
	if codeOf(err) != "RL098" {
		t.Fatalf("a differently defined pre-built index must be refused with RL098, got %v", err)
	}
	var n int
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname IN ('rg_limit_configs', 'database_environment')`).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("a refused up must create nothing: %d %v", n, err)
	}
	if _, err := pool.Raw().Exec(ctx, `DROP INDEX CONCURRENTLY idx_ledger_entries_tenant_player_asset_created`); err != nil {
		t.Fatal(err)
	}
	// The runbook procedure: both indexes built CONCURRENTLY (outside any transaction), then migrate.
	for _, stmt := range []string{
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_ledger_entries_tenant_player_asset_created ON ledger_entries (tenant_id, player_account_id, asset_code, created_at)`,
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_deposit_intents_tenant_player_asset_created ON deposit_intents (tenant_id, player_account_id, asset_code, created_at)`,
	} {
		if _, err := pool.Raw().Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	var oid1 uint32
	_ = pool.Raw().QueryRow(ctx, `SELECT 'idx_ledger_entries_tenant_player_asset_created'::regclass::oid`).Scan(&oid1)
	if applied, err := pool.MigrateUp(ctx, full); err != nil || len(applied) != 1 {
		t.Fatalf("up over valid pre-built indexes: %v %v", applied, err)
	}
	var oid2 uint32
	_ = pool.Raw().QueryRow(ctx, `SELECT 'idx_ledger_entries_tenant_player_asset_created'::regclass::oid`).Scan(&oid2)
	if oid1 == 0 || oid1 != oid2 {
		t.Fatalf("the migration must keep the pre-built index (no rebuild): %d -> %d", oid1, oid2)
	}
}

func mustUUID() uuid.UUID { return uuid.New() }

func mustParse(s string) uuid.UUID { return uuid.MustParse(s) }
