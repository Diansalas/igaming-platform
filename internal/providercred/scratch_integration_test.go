//go:build integration

// Scratch-database tests for migration 0096: everything that needs DDL,
// TRUNCATE, a disabled trigger or a down migration runs on a throwaway
// database created by internal/testsupport/scratchdb, never on the shared
// test database (where TRUNCATE/DDL would contend for ACCESS EXCLUSIVE
// locks with other packages' tests).
package providercred

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// scratchWorld is a fully migrated scratch database with the runtime role
// granted exactly as CI and deploy/init-app-role.sql do (default
// privileges before migrations, so migration 0096's own narrowing
// applies).
type scratchWorld struct {
	ownerURL   string
	runtimeURL string
	owner      *db.Pool
	runtime    *db.Pool
}

func migrationsDir() string { return "../../migrations" }

// providerCredMigrationsThrough96 copies every migration up to and
// including 0096 into a temp directory, so a scratch database built from
// it treats migration 0096 as the chain's tip no matter what lands after
// it in the real migrations directory (Stage 10.3 W2b's 0097, W3a's 0098,
// and anything later) - mirroring internal/kyc/
// migration_0095_integration_test.go's own migration0095DirThroughSelf
// precedent. TestPCMigration_RoundTripOnCleanDatabase and
// TestPCMigration_DownRefusesWithRows both assert MigrateDown(dir, 1)
// targets 0096 itself; without this, MigrateDown(dir, 1) would instead
// target whichever migration is actually most recent, silently changing
// what each test proves.
func providerCredMigrationsThrough96(t *testing.T) string {
	t.Helper()
	src := migrationsDir()
	dir := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		if len(e.Name()) >= 4 {
			if v, err := strconv.Atoi(e.Name()[:4]); err == nil && v > 96 {
				continue
			}
		}
		content, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func newScratchWorld(t *testing.T, prefix string, stagedDir string) *scratchWorld {
	t.Helper()
	rtURL := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if rtURL == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set; skipping provider credential runtime-role test")
	}
	ownerURL := scratchdb.New(t, prefix)
	u, err := url.Parse(ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	dbName := strings.TrimPrefix(u.Path, "/")
	ru, err := url.Parse(rtURL)
	if err != nil {
		t.Fatal(err)
	}
	ru.Path = "/" + dbName

	conn, err := pgx.Connect(context.Background(), ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`GRANT CONNECT ON DATABASE ` + pgx.Identifier{dbName}.Sanitize() + ` TO igaming_runtime`,
		`GRANT USAGE ON SCHEMA public TO igaming_runtime`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO igaming_runtime`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO igaming_runtime`,
	} {
		if _, err := conn.Exec(context.Background(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	_ = conn.Close(context.Background())

	w := &scratchWorld{ownerURL: ownerURL, runtimeURL: ru.String()}
	w.owner = connect(t, ownerURL, 4)
	dir := stagedDir
	if dir == "" {
		dir = migrationsDir()
	}
	if _, err := w.owner.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up: %v", err)
	}
	w.runtime = connect(t, w.runtimeURL, 8)
	return w
}

// scratchFx is an fx bound to a scratch world's runtime pool.
func scratchFx(t *testing.T, w *scratchWorld) *fx {
	t.Helper()
	t.Setenv("TEST_RUNTIME_DATABASE_URL", w.runtimeURL)
	return newFx(t)
}

// ownerExec runs statements as the scratch database's owner in one
// transaction.
func (w *scratchWorld) ownerExec(t *testing.T, stmts ...string) {
	t.Helper()
	ctx := context.Background()
	tx, err := w.owner.Raw().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestPCApproval_RequesterUnlinkedRefused: a request whose requester has no
// Person linkage (only constructible by the owner with the request insert
// trigger disabled - the trigger itself refuses such a filing, and
// staff_users.person_id is append-only) cannot be approved (PC043).
func TestPCApproval_RequesterUnlinkedRefused(t *testing.T) {
	w := newScratchWorld(t, "pc_unlinked_", "")
	f := scratchFx(t, w)
	tenant := f.tenant()
	reqID := uuid.New()
	w.ownerExec(t,
		`ALTER TABLE provider_credential_change_requests DISABLE TRIGGER provider_credential_change_requests_before_insert`,
		`SELECT set_config('app.platform_admin_principal_id', '`+f.unlinked.String()+`', true)`,
		`INSERT INTO provider_credential_change_requests
			(id, target_tenant_id, domain, provider_id, purpose, key_id, secret_ref, fingerprint, not_before,
			 predecessor_disposition, reason_code, requested_by_principal_id, content_hash)
		 VALUES ('`+reqID.String()+`', '`+tenant.String()+`', 'casino', 'acme', 'webhook_verify', 'k1',
			 '`+memRef(tenant, "casino", "acme", "n")+`', '`+randomFingerprint(t)+`', now(), 'none',
			 'initial_registration', '`+f.unlinked.String()+`', '`+repeat("c", 64)+`')`,
		`ALTER TABLE provider_credential_change_requests ENABLE TRIGGER provider_credential_change_requests_before_insert`,
	)
	_, err := f.decide(Request{ID: reqID, TargetTenantID: tenant}, f.approver, "approve", repeat("c", 64), "")
	if KindOf(err) != KindApprovalRejected || ClassOf(err) != "PC043" {
		t.Fatalf("got %v (%s)", err, ClassOf(err))
	}
}

// TestPCConsume_ExpiryBoundary backdates decided_at through the owner
// connection with the approval triggers disabled: 23h59m passes, 24h00m01s
// is refused (PC006). 24 hours is a platform constant.
func TestPCConsume_ExpiryBoundary(t *testing.T) {
	w := newScratchWorld(t, "pc_expiry_", "")
	f := scratchFx(t, w)
	backdate := func(r Request, age string) {
		w.ownerExec(t,
			`ALTER TABLE provider_credential_change_approvals DISABLE TRIGGER provider_credential_change_approvals_immutable`,
			`SELECT set_config('app.platform_admin_principal_id', '`+f.requester.String()+`', true)`,
			`UPDATE provider_credential_change_approvals SET decided_at = now() - interval '`+age+`' WHERE request_id = '`+r.ID.String()+`'`,
			`ALTER TABLE provider_credential_change_approvals ENABLE TRIGGER provider_credential_change_approvals_immutable`,
		)
	}
	tenant := f.tenant()
	fresh, _ := f.file(f.spec(tenant, "acme", "k1"))
	f.approve(fresh, f.approver)
	backdate(fresh, "23 hours 59 minutes")
	if _, err := f.apply(fresh); err != nil {
		t.Fatalf("an approval 23h59m old must still be consumable: %v (%s)", err, ClassOf(err))
	}

	stale, _ := f.file(f.spec(tenant, "other", "k1"))
	f.approve(stale, f.approver)
	backdate(stale, "24 hours 1 second")
	if _, err := f.apply(stale); KindOf(err) != KindActivationRejected || ClassOf(err) != "PC006" {
		t.Fatalf("an approval 24h00m01s old must be refused: %v (%s)", err, ClassOf(err))
	}
}

// TestPCMigration_TruncateRefused: TRUNCATE is refused on all three
// tables - by privilege for the runtime role, and by the trigger (the
// binding control, which also binds the owner).
func TestPCMigration_TruncateRefused(t *testing.T) {
	w := newScratchWorld(t, "pc_trunc_", "")
	ctx := context.Background()
	for _, table := range []string{"provider_credential_handles", "provider_credential_change_requests", "provider_credential_change_approvals"} {
		t.Run(table+"/runtime", func(t *testing.T) {
			_, err := w.runtime.Raw().Exec(ctx, `TRUNCATE `+table+` CASCADE`)
			if err == nil {
				t.Fatal("the runtime role must not be able to TRUNCATE")
			}
		})
		t.Run(table+"/owner", func(t *testing.T) {
			_, err := w.owner.Raw().Exec(ctx, `TRUNCATE `+table+` CASCADE`)
			if code := pgCode(err); code != "PC027" && code != "PC035" && code != "P0001" {
				t.Fatalf("the owner's TRUNCATE must be refused by the trigger, got %v", err)
			}
		})
	}
}

// TestPCHandleTransition_DeleteRefusedAsOwner: the owner (subject to
// FORCE RLS) has no DELETE policy, so its DELETE removes nothing; and with
// a permissive DELETE policy added (DDL, scratch only) the BEFORE DELETE
// trigger still refuses (PC027) - the trigger is the binding control.
func TestPCHandleTransition_DeleteRefusedAsOwner(t *testing.T) {
	w := newScratchWorld(t, "pc_del_", "")
	f := scratchFx(t, w)
	tenant := f.tenant()
	h, _ := f.register(f.spec(tenant, "acme", "k1"))
	var deleted int64
	err := w.owner.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM provider_credential_handles WHERE id = $1`, h.ID)
		deleted = tag.RowsAffected()
		return err
	})
	if err != nil || deleted != 0 {
		t.Fatalf("owner DELETE without a policy must remove nothing: deleted=%d err=%v", deleted, err)
	}
	w.ownerExec(t, `CREATE POLICY tmp_delete_all ON provider_credential_handles FOR DELETE USING (true)`)
	err = w.owner.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM provider_credential_handles WHERE id = $1`, h.ID)
		return err
	})
	if pgCode(err) != "PC027" {
		t.Fatalf("with a DELETE policy the trigger must still refuse (PC027), got %v", err)
	}
}

// TestPCMigration_DownRefusesWithRows: with any credential history the
// down migration refuses (validated CHECK (false) guard, never count(*)
// under FORCE RLS), and the schema is left intact.
func TestPCMigration_DownRefusesWithRows(t *testing.T) {
	dir := providerCredMigrationsThrough96(t)
	w := newScratchWorld(t, "pc_down_", dir)
	f := scratchFx(t, w)
	f.register(f.spec(f.tenant(), "acme", "k1"))

	_, err := w.owner.MigrateDown(context.Background(), dir, 1)
	if err == nil || !strings.Contains(err.Error(), "roll forward") {
		t.Fatalf("down must refuse with a roll-forward message, got %v", err)
	}
	var exists bool
	if err := w.owner.Raw().QueryRow(context.Background(),
		`SELECT to_regclass('public.provider_credential_handles') IS NOT NULL`).Scan(&exists); err != nil || !exists {
		t.Fatalf("the table must survive a refused down: exists=%v err=%v", exists, err)
	}
}

// TestPCMigration_RoundTripOnCleanDatabase: up, down, up on a clean
// database; the down drops every object 0096 created. Built from a
// migrations directory held back at 0096 (providerCredMigrationsThrough96)
// so MigrateDown(dir, 1) always targets 0096 itself, regardless of what
// later migrations (0097, 0098, ...) exist in the real chain.
func TestPCMigration_RoundTripOnCleanDatabase(t *testing.T) {
	dir := providerCredMigrationsThrough96(t)
	w := newScratchWorld(t, "pc_rt_", dir)
	ctx := context.Background()
	down, err := w.owner.MigrateDown(ctx, dir, 1)
	if err != nil || len(down) != 1 || down[0] != 96 {
		t.Fatalf("down: %v %v", down, err)
	}
	var tables, funcs int
	if err := w.owner.Raw().QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_tables WHERE tablename LIKE 'provider_credential%'),
		(SELECT count(*) FROM pg_proc WHERE proname LIKE 'provider_credential%')`).Scan(&tables, &funcs); err != nil {
		t.Fatal(err)
	}
	if tables != 0 || funcs != 0 {
		t.Fatalf("down left %d tables and %d functions behind", tables, funcs)
	}
	up, err := w.owner.MigrateUp(ctx, dir)
	if err != nil || len(up) != 1 || up[0] != 96 {
		t.Fatalf("up again: %v %v", up, err)
	}
	report, err := w.owner.VerifyMigrations(ctx, dir)
	if err != nil || !report.OK() {
		t.Fatalf("verify: %v %+v", err, report)
	}
	_ = time.Second
}
