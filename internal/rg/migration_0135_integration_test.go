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
	"github.com/Diansalas/igaming-platform/internal/wallet"
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
	// No pre-build, a writer holding ROW EXCLUSIVE: the small-table build waits at most the step's
	// lock_timeout (5 s) and fails 55P03 - it never queues indefinitely behind live traffic.
	{
		w, err := pool.Raw().Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		wt, err := w.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Exec(ctx, `LOCK TABLE ledger_entries IN ROW EXCLUSIVE MODE`); err != nil {
			t.Fatal(err)
		}
		bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
		start := time.Now()
		_, err = pool.MigrateUp(bounded, full)
		cancel()
		_ = wt.Rollback(ctx)
		w.Release()
		if codeOf(err) != "55P03" || time.Since(start) > 20*time.Second {
			t.Fatalf("a blocked index build must fail on lock_timeout (55P03) within seconds, got %v after %s", err, time.Since(start))
		}
	}
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
	// Security C3 / ledger-finance 1-2: with the indexes pre-built the migration takes no SHARE lock
	// on either table - it succeeds while another session holds ROW EXCLUSIVE (live writers) on both.
	writer, err := pool.Raw().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Release()
	wtx, err := writer.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wtx.Exec(ctx, `LOCK TABLE ledger_entries, deposit_intents IN ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	applied, err := pool.MigrateUp(bounded, full)
	cancel()
	_ = wtx.Rollback(ctx)
	if err != nil || len(applied) != 1 {
		t.Fatalf("up over valid pre-built indexes, with writers holding ROW EXCLUSIVE: %v %v", applied, err)
	}
	var oid2 uint32
	_ = pool.Raw().QueryRow(ctx, `SELECT 'idx_ledger_entries_tenant_player_asset_created'::regclass::oid`).Scan(&oid2)
	if oid1 == 0 || oid1 != oid2 {
		t.Fatalf("the migration must keep the pre-built index (no rebuild): %d -> %d", oid1, oid2)
	}
}

func mustUUID() uuid.UUID { return uuid.New() }

func mustParse(s string) uuid.UUID { return uuid.MustParse(s) }

// Security L1: the 5 s lock_timeout stays in force for the whole file. With the indexes built in the
// transaction (SHARE on ledger_entries / deposit_intents), a later ACCESS EXCLUSIVE step that is
// blocked fails 55P03 within seconds and the rollback applies nothing - it never queues indefinitely
// while holding the ledger SHARE lock.
func TestMigration0135_BlockedAccessExclusiveStepFailsFast(t *testing.T) {
	ctx := context.Background()
	pool, _ := m135Scratch(t, "m0135x_")
	if _, err := pool.MigrateUp(ctx, m135Dir(t, migration0135Version-1)); err != nil {
		t.Fatal(err)
	}
	for _, tbl := range []string{"player_restrictions", "casino_launch_sessions"} {
		holder, err := pool.Raw().Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		htx, err := holder.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := htx.Exec(ctx, `LOCK TABLE `+tbl+` IN ACCESS SHARE MODE`); err != nil {
			t.Fatal(err)
		}
		bounded, cancel := context.WithTimeout(ctx, 60*time.Second)
		start := time.Now()
		_, err = pool.MigrateUp(bounded, m135Dir(t, migration0135Version))
		elapsed := time.Since(start)
		cancel()
		_ = htx.Rollback(ctx)
		holder.Release()
		if codeOf(err) != "55P03" || elapsed > 20*time.Second {
			t.Fatalf("%s held: the blocked ACCESS EXCLUSIVE step must fail 55P03 within seconds, got %v after %s", tbl, err, elapsed)
		}
		var n int
		var head int64
		if err := pool.Raw().QueryRow(ctx, `SELECT (SELECT count(*) FROM pg_class WHERE relname IN ('rg_limit_configs', 'database_environment',
				'idx_ledger_entries_tenant_player_asset_created', 'idx_deposit_intents_tenant_player_asset_created')),
				(SELECT max(version) FROM schema_migrations)`).Scan(&n, &head); err != nil || n != 0 || head >= migration0135Version {
			t.Fatalf("%s held: a refused up must apply nothing (objects=%d head=%d err=%v)", tbl, n, head, err)
		}
	}
	if _, err := pool.MigrateUp(ctx, m135Dir(t, migration0135Version)); err != nil {
		t.Fatalf("with nothing held the migration applies: %v", err)
	}
}

// Ledger-finance 5: an INVALID index under the expected name (a failed / cancelled CONCURRENTLY
// build) is refused with RL098 even though its definition matches.
func TestMigration0135_InvalidPrebuiltIndexRefused(t *testing.T) {
	ctx := context.Background()
	pool, _ := m135Scratch(t, "m0135v_")
	if _, err := pool.MigrateUp(ctx, m135Dir(t, migration0135Version-1)); err != nil {
		t.Fatal(err)
	}
	// A session holding ROW EXCLUSIVE makes the CONCURRENTLY build wait after it has created its
	// (invalid) catalog entry; a statement timeout then cancels it and leaves the index INVALID.
	holder, err := pool.Raw().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	htx, err := holder.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := htx.Exec(ctx, `LOCK TABLE deposit_intents IN ROW EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	builder, err := pool.Raw().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := builder.Exec(ctx, `SET statement_timeout = '1s'`); err != nil {
		t.Fatal(err)
	}
	_, buildErr := builder.Exec(ctx, `CREATE INDEX CONCURRENTLY idx_deposit_intents_tenant_player_asset_created ON deposit_intents (tenant_id, player_account_id, asset_code, created_at)`)
	_, _ = builder.Exec(ctx, `RESET statement_timeout`)
	builder.Release()
	_ = htx.Rollback(ctx)
	holder.Release()
	if buildErr == nil {
		t.Fatal("the concurrent build was expected to be cancelled")
	}
	var valid bool
	if err := pool.Raw().QueryRow(ctx, `SELECT indisvalid FROM pg_index WHERE indexrelid = 'idx_deposit_intents_tenant_player_asset_created'::regclass`).Scan(&valid); err != nil || valid {
		t.Fatalf("precondition: an INVALID index must exist (valid=%v err=%v)", valid, err)
	}
	_, err = pool.MigrateUp(ctx, m135Dir(t, migration0135Version))
	if codeOf(err) != "RL098" || !strings.Contains(err.Error(), "INVALID") {
		t.Fatalf("an INVALID pre-built index must be refused with RL098, got %v", err)
	}
	if _, err := pool.Raw().Exec(ctx, `DROP INDEX CONCURRENTLY idx_deposit_intents_tenant_player_asset_created`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.MigrateUp(ctx, m135Dir(t, migration0135Version)); err != nil {
		t.Fatalf("after DROP INDEX CONCURRENTLY the small-table path builds the index: %v", err)
	}
}

// Security C3 / ledger-finance 1-2: without a pre-built index, a table above the threshold
// (reltuples > 100000) is refused with RL098 pointing at runbook 5.1; nothing is created.
func TestMigration0135_LargeTableWithoutPrebuiltIndexRefused(t *testing.T) {
	ctx := context.Background()
	pool, _ := m135Scratch(t, "m0135l_")
	if _, err := pool.MigrateUp(ctx, m135Dir(t, migration0135Version-1)); err != nil {
		t.Fatal(err)
	}
	tenantID, brandID, acct, person := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if err := pool.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model, status) VALUES ($1, 'm0135l', $2, 'under_platform_licence', 'active')`,
			tenantID, "m0135l-"+tenantID.String()[:8]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name, status) VALUES ($1, $2, $3, 'b', 'active')`, brandID, tenantID, "m0135l-b-"+brandID.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status) VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			acct, tenantID, brandID, person, acct.String()+"@m0135l.invalid"); err != nil {
			return err
		}
		w, err := wallet.GetOrCreate(ctx, tx, tenantID, brandID, acct, "EUR")
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO deposit_intents (tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, idempotency_key)
			SELECT $1, $2, $3, $4, 'EUR', 100, 'card', 'm0135l-' || g FROM generate_series(1, 120000) AS g`, tenantID, brandID, acct, w.ID)
		return err
	}); err != nil {
		t.Fatalf("seed 120k deposit intents: %v", err)
	}
	if _, err := pool.Raw().Exec(ctx, `ANALYZE deposit_intents`); err != nil {
		t.Fatal(err)
	}
	_, err := pool.MigrateUp(ctx, m135Dir(t, migration0135Version))
	if codeOf(err) != "RL098" || !strings.Contains(err.Error(), "section 5.1") || !strings.Contains(err.Error(), "deposit_intents") {
		t.Fatalf("a large table without a pre-built index must be refused with RL098 (runbook 5.1), got %v", err)
	}
	var n int
	if err := pool.Raw().QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname IN ('rg_limit_configs', 'idx_ledger_entries_tenant_player_asset_created', 'idx_deposit_intents_tenant_player_asset_created')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a refused up must create nothing: %d %v", n, err)
	}
	if _, err := pool.Raw().Exec(ctx, `CREATE INDEX CONCURRENTLY idx_deposit_intents_tenant_player_asset_created ON deposit_intents (tenant_id, player_account_id, asset_code, created_at)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.MigrateUp(ctx, m135Dir(t, migration0135Version)); err != nil {
		t.Fatalf("after the runbook pre-build the migration applies: %v", err)
	}
}
