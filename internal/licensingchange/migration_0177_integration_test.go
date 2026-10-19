//go:build integration

package licensingchange

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// migrationsThrough copies the real migration files with a version <= through into a temp dir and returns it plus the
// highest version below 177 that exists (the baseline; 0176 belongs to a parallel slice and may or may not be present).
func migrationsThrough(t *testing.T, through int) (string, int) {
	t.Helper()
	src := filepath.Join("..", "..", "migrations")
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	prev := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		v, err := strconv.Atoi(name[:4])
		if err != nil {
			t.Fatalf("migration %q has no version prefix", name)
		}
		if v < 177 && v > prev {
			prev = v
		}
		if v > through {
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
	return out, prev
}

// schemaSnapshot is the whole-schema snapshot of the migration tests (the f1Snapshot query of internal/payments).
func schemaSnapshot(t *testing.T, pool *db.Pool) string {
	t.Helper()
	var snap string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT string_agg(x, E'\n' ORDER BY x) FROM (
			SELECT 'policy:' || tablename || ':' || policyname || ':' || permissive || ':' || cmd || ':' || coalesce(qual, '') || ':' || coalesce(with_check, '') AS x
			  FROM pg_policies WHERE schemaname = 'public'
			UNION ALL
			SELECT 'trigger:' || c.relname || ':' || t.tgname || ':' || t.tgenabled::text || ':' || pg_get_triggerdef(t.oid)
			  FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
			 WHERE NOT t.tgisinternal AND c.relnamespace = 'public'::regnamespace
			UNION ALL
			SELECT 'constraint:' || conrelid::regclass::text || ':' || conname || ':' || pg_get_constraintdef(oid)
			  FROM pg_constraint WHERE connamespace = 'public'::regnamespace
			UNION ALL
			SELECT 'table:' || tablename || ':rls=' || c.relrowsecurity || ':force=' || c.relforcerowsecurity || ':acl=' || coalesce(c.relacl::text, '')
			  FROM pg_tables pt JOIN pg_class c ON c.relname = pt.tablename AND c.relnamespace = 'public'::regnamespace
			 WHERE pt.schemaname = 'public'
			UNION ALL
			SELECT 'column:' || table_name || ':' || column_name || ':' || data_type || ':' || is_nullable || ':' || coalesce(column_default, '')
			  FROM information_schema.columns WHERE table_schema = 'public'
			UNION ALL
			SELECT 'function:' || p.oid::regprocedure::text || ':' || md5(p.prosrc) || ':' || coalesce(array_to_string(p.proconfig, ','), '') || ':' || p.prosecdef || ':acl=' || coalesce(p.proacl::text, '')
			  FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace
			UNION ALL
			SELECT 'index:' || tablename || ':' || indexname || ':' || indexdef FROM pg_indexes WHERE schemaname = 'public') s`).Scan(&snap)
	}); err != nil {
		t.Fatalf("schema snapshot: %v", err)
	}
	return snap
}

// 0177 up / down / up on a throwaway database: the down restores the previous schema byte for byte (the previous verifier,
// the 0005 tenant_jurisdiction_configs policy, nothing left), the re-up equals the first up, and the down is refused (LC095)
// while a licensing change request exists, changing nothing.
func TestMigration0177_UpDownUp_SnapshotAndDownRefusal(t *testing.T) {
	ctx := context.Background()
	u := scratchdb.New(t, "m0177_")
	pool, err := db.Connect(ctx, u, 8, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	full, prev := migrationsThrough(t, 177)
	baseline, _ := migrationsThrough(t, prev)
	if _, err := pool.MigrateUp(ctx, baseline); err != nil {
		t.Fatalf("migrate to %04d: %v", prev, err)
	}
	before := schemaSnapshot(t, pool)
	if applied, err := pool.MigrateUp(ctx, full); err != nil || len(applied) != 1 || applied[0] != 177 {
		t.Fatalf("0177 up: %v %v", applied, err)
	}
	up := schemaSnapshot(t, pool)
	for _, want := range []string{"table:licensing_change_requests", "table:licensing_change_approvals", "table:licensing_change_kinds",
		"zz_licensing_change_governed", "uq_tenant_jurisdiction_configs_open", "tjc_platform_insert", "tenant_jurisdiction_configs_append_only"} {
		if !strings.Contains(up, want) {
			t.Fatalf("0177 up must create %q", want)
		}
	}
	if strings.Contains(up, "policy:tenant_jurisdiction_configs:tenant_isolation:") {
		t.Fatal("0177 up must drop the 0005 FOR ALL tenant policy")
	}
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("0177 down: %v", err)
	}
	if got := schemaSnapshot(t, pool); got != before {
		t.Fatal("0177 down must restore the previous schema byte for byte")
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("0177 up again: %v", err)
	}
	if got := schemaSnapshot(t, pool); got != up {
		t.Fatal("0177 up again must equal the first up")
	}

	// Down refusal: one executed single-actor request (a tenant unbind) on the scratch database.
	prooftest.InstallVia(t, pool.Raw(), u)
	launchfix.SeedOperatorsAt(t, u)
	ops := launchfix.Operators()
	var tenantID uuid.UUID
	if err := pool.WithPlatformAdmin(ctx, ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "m0177", "m0177-"+uuid.NewString(), "under_platform_licence")
		tenantID = tn.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	svc, err := New(Config{Pool: pool})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Request(ctx, Actor{StaffID: ops.Requester, Scope: ScopePlatform, Role: string(auth.RolePlatformAdmin)},
		RequestInput{Kind: KindTenantLicenceUnbind, Payload: payload(t, TenantLicenceUnbindPayload{TenantID: tenantID}), ReasonCode: "m0177"}); err != nil {
		t.Fatalf("seed a request: %v", err)
	}
	_, err = pool.MigrateDown(ctx, full, 1)
	if err == nil || !strings.Contains(err.Error(), "LC095") {
		t.Fatalf("0177 down must refuse LC095 while a request exists, got %v", err)
	}
	if got := schemaSnapshot(t, pool); got != up {
		t.Fatal("a refused down must change nothing")
	}
}
