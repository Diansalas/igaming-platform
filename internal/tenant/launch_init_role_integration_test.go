//go:build integration

package tenant

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// deploy/init-app-role.sql re-asserts the launch-table grants on every run (its blanket backfill
// GRANT would otherwise re-grant DELETE / TRUNCATE on re-run against a migrated database). On a
// THROWAWAY scratch database: migrate, simulate the blanket grant, run the script's runtime-role
// section, and require the migration's own narrow grants back; a second run changes nothing.
func TestLaunchGov_InitAppRoleSQL_ReassertsLaunchTableGrants(t *testing.T) {
	if os.Getenv("TEST_RUNTIME_DATABASE_URL") == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set")
	}
	ctx := context.Background()
	ownerURL := scratchdb.New(t, "m0128g_")
	pool, err := db.Connect(ctx, ownerURL, 5, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.MigrateUp(ctx, "../../migrations"); err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	for _, stmt := range []string{
		`GRANT CONNECT ON DATABASE ` + pgx.Identifier{dbName}.Sanitize() + ` TO igaming_runtime`,
		`GRANT USAGE ON SCHEMA public TO igaming_runtime`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO igaming_runtime`,
	} {
		if _, err := pool.Raw().Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	snapshot := func() string {
		var out string
		if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT string_agg(x, E'\n' ORDER BY x) FROM (
				SELECT table_name || ':' || privilege_type AS x FROM information_schema.role_table_grants
				 WHERE grantee = 'igaming_runtime' AND table_schema = 'public' AND table_name LIKE 'launch\_%'
				UNION ALL
				SELECT table_name || ':col:' || column_name || ':' || privilege_type FROM information_schema.column_privileges
				 WHERE grantee = 'igaming_runtime' AND table_schema = 'public' AND table_name LIKE 'launch\_%' AND privilege_type = 'UPDATE') s`).Scan(&out)
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// The simulated blanket grant widened them.
	if before := snapshot(); !strings.Contains(before, "launch_status_transitions:DELETE") {
		t.Fatalf("test premise: the blanket grant should have granted DELETE, got:\n%s", before)
	}
	raw, err := os.ReadFile("../../deploy/init-app-role.sql")
	if err != nil {
		t.Fatal(err)
	}
	const marker = "-- --- PLAT-ROLESPLIT-1: non-owning runtime application role ---"
	i := strings.Index(string(raw), marker)
	if i < 0 {
		t.Fatal("init-app-role.sql marker not found")
	}
	script := strings.ReplaceAll(string(raw[i:]), "igaming_platform_dev", pgx.Identifier{dbName}.Sanitize())
	if _, err := pool.Raw().Exec(ctx, script); err != nil {
		t.Fatalf("init-app-role.sql runtime section: %v", err)
	}
	want := strings.Join([]string{
		"launch_authorisation_approvals:INSERT", "launch_authorisation_approvals:SELECT",
		"launch_authorisation_requests:INSERT", "launch_authorisation_requests:SELECT",
		"launch_authorisation_requests:col:decided_at:UPDATE", "launch_authorisation_requests:col:executing_txid:UPDATE",
		"launch_authorisation_requests:col:refusal_code:UPDATE", "launch_authorisation_requests:col:status:UPDATE",
		"launch_status_transitions:INSERT", "launch_status_transitions:SELECT",
	}, "\n")
	got := snapshot()
	if got != want {
		t.Fatalf("grants after init-app-role.sql:\n%s\nwant:\n%s", got, want)
	}
	if _, err := pool.Raw().Exec(ctx, script); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if again := snapshot(); again != got {
		t.Fatalf("a second run changed the grants:\n%s\nvs\n%s", got, again)
	}
}
