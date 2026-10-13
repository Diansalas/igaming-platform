//go:build integration

package httpserver

// ADR 0119 S3, migration 0138 on THROWAWAY scratch databases (owned by the table owner; no runtime grants exist on
// a scratch database, so these tests run as the owner - the grant posture is pinned on the shared database by
// staff_mfa_schema_integration_test.go):
//
//   - up / down / up whole-schema snapshot: the down restores 0137 exactly; the up adds only its objects and changes
//     exactly one existing object (alert_kinds_check1, widened by the five platform staff Kinds);
//   - pre-flight (SF041) refusals, with nothing created;
//   - the revocation of every live staff session (counted), the cutover marker, FORCE RLS restored;
//   - the review C1 cutover rule through the real refresh route: a NULL seal on a session created before the cutover
//     is a plain 401 (no audit, no alert); a NULL seal after it is a P1;
//   - the rolling deploy: pre-S3 code after 0138 fails staff session writes with SF040 (fail closed), and S3 code on
//     the 0137 schema keeps player refresh working while every staff session path fails closed;
//   - down refusals (SF042).
//
// Requires TEST_DATABASE_URL and TEST_ADMIN_DATABASE_URL (scratchdb); a skip is NOT evidence.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

const m138Version = 138

func m138MigDir(t *testing.T, through int64) string {
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

func m138Scratch(t *testing.T, prefix string, through int64) (*db.Pool, string) {
	t.Helper()
	u := scratchdb.New(t, prefix)
	pool, err := db.Connect(context.Background(), u, 10, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.MigrateUp(context.Background(), m138MigDir(t, through)); err != nil {
		t.Fatalf("migrate scratch to %d: %v", through, err)
	}
	return pool, u
}

func m138Snapshot(t *testing.T, pool *db.Pool) string {
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
			SELECT 'colacl:' || c.relname || ':' || a.attname || ':' || a.attacl::text
			  FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
			 WHERE c.relnamespace = 'public'::regnamespace AND a.attacl IS NOT NULL AND NOT a.attisdropped
			UNION ALL
			SELECT 'column:' || table_name || ':' || column_name || ':' || data_type || ':' || is_nullable || ':' || coalesce(column_default, '')
			  FROM information_schema.columns WHERE table_schema = 'public'
			UNION ALL
			SELECT 'function:' || p.oid::regprocedure::text || ':' || md5(p.prosrc) || ':' || coalesce(array_to_string(p.proconfig, ','), '') || ':' || p.prosecdef || ':' || p.provolatile::text || ':acl=' || coalesce(p.proacl::text, '')
			  FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace
			UNION ALL
			SELECT 'index:' || tablename || ':' || indexname || ':' || indexdef FROM pg_indexes WHERE schemaname = 'public'
			UNION ALL
			SELECT 'kind:' || kind || ':' || severity || ':' || requires_subject || ':' || in_tx_raisable_by_tenant || ':' || array_to_string(allowed_keys, ',') || ':' || raise_mode
			  FROM alert_kinds) s`).Scan(&snap)
	}); err != nil {
		t.Fatalf("schema snapshot: %v", err)
	}
	return snap
}

func m138Diff(a, b string) (removed, added []string) {
	as, bs := map[string]bool{}, map[string]bool{}
	for _, l := range strings.Split(a, "\n") {
		as[l] = true
	}
	for _, l := range strings.Split(b, "\n") {
		bs[l] = true
	}
	for l := range as {
		if !bs[l] {
			removed = append(removed, l)
		}
	}
	for l := range bs {
		if !as[l] {
			added = append(added, l)
		}
	}
	sort.Strings(removed)
	sort.Strings(added)
	return
}

func TestMigration0138_UpDownUp_WholeSchema(t *testing.T) {
	ctx := context.Background()
	pool, _ := m138Scratch(t, "m0138s_", m138Version-1)
	snap137 := m138Snapshot(t, pool)
	full := m138MigDir(t, m138Version)
	if applied, err := pool.MigrateUp(ctx, full); err != nil || len(applied) != 1 || applied[0] != m138Version {
		t.Fatalf("up: %v %v", applied, err)
	}
	snap138 := m138Snapshot(t, pool)
	removed, added := m138Diff(snap137, snap138)
	// Exactly one existing object changes: the alert_kinds subject CHECK, widened by the five platform staff Kinds.
	if len(removed) != 1 || !strings.HasPrefix(removed[0], "constraint:alert_kinds:alert_kinds_check1:") {
		t.Fatalf("0138 may change only alert_kinds_check1; removed: %v", removed)
	}
	joined := strings.Join(added, "\n")
	for _, must := range []string{
		"column:sessions:aal:", "column:sessions:amr:", "column:sessions:auth_time:", "column:sessions:mfa_at:",
		"column:sessions:absolute_expires_at:", "column:sessions:assurance_kid:", "column:sessions:assurance_seal:",
		"constraint:sessions:sessions_assurance_staff_only:", "constraint:sessions:sessions_assurance_shape:",
		"trigger:sessions:sessions_staff_assurance_guard:", "trigger:staff_mfa_factors:sf_factor_lockout_clock:",
		"table:staff_session_cutover:rls=true:force=true", "policy:staff_session_cutover:staff_session_cutover_read:",
		"function:sessions_staff_assurance_guard()", "function:sf_factor_lockout_clock()", "function:sf_cutover_immutable()",
		"constraint:alert_kinds:alert_kinds_check1:", "kind:staff.mfa_integrity:p1:true:true:", "kind:staff.platform_session_integrity:p1:false:false:",
	} {
		if !strings.Contains(joined, must) {
			t.Fatalf("0138 snapshot diff lacks %q:\n%s", must, joined)
		}
	}
	for _, l := range added {
		ok := strings.Contains(l, "sessions") || strings.Contains(l, "staff_session_cutover") || strings.Contains(l, "sf_factor_lockout_clock") ||
			strings.Contains(l, "sf_cutover_immutable") || strings.HasPrefix(l, "kind:staff.") || strings.HasPrefix(l, "constraint:alert_kinds:alert_kinds_check1:")
		if !ok {
			t.Fatalf("0138 added an object outside its scope: %s", l)
		}
	}
	if n := strings.Count(joined, "\nkind:staff.") + boolInt(strings.HasPrefix(joined, "kind:staff.")); n != 10 {
		t.Fatalf("want 10 staff alert Kinds, got %d", n)
	}
	var forced bool
	if err := pool.Raw().QueryRow(ctx, `SELECT relforcerowsecurity FROM pg_class WHERE oid = 'public.sessions'::regclass`).Scan(&forced); err != nil || !forced {
		t.Fatalf("FORCE ROW LEVEL SECURITY on sessions must be restored by the up: %v %v", forced, err)
	}
	if rolled, err := pool.MigrateDown(ctx, full, 1); err != nil || len(rolled) != 1 || rolled[0] != m138Version {
		t.Fatalf("down: %v %v", rolled, err)
	}
	if got := m138Snapshot(t, pool); got != snap137 {
		r, a := m138Diff(snap137, got)
		t.Fatalf("down did not restore 0137 exactly:\n- %v\n+ %v", r, a)
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	if got := m138Snapshot(t, pool); got != snap138 {
		r, a := m138Diff(snap138, got)
		t.Fatalf("up after down differs from the first up:\n- %v\n+ %v", r, a)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func readMigration(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMigration0138_PreFlight_Refusals(t *testing.T) {
	ctx := context.Background()
	pool, _ := m138Scratch(t, "m0138p_", m138Version-1)
	up := readMigration(t, "0138_staff_session_cutover.up.sql")
	for name, c := range map[string]struct {
		drift string
		want  string
	}{
		"a new sessions column":               {`ALTER TABLE sessions ADD COLUMN zz_extra TEXT`, "zz_extra"},
		"the cutover table already exists":    {`CREATE TABLE staff_session_cutover (x int)`, "staff_session_cutover_exists=t"},
		"the 0137 counter columns are absent": {`ALTER TABLE staff_mfa_factors DROP COLUMN hard_locked_at CASCADE`, "staff_mfa_factors_counter_columns=2"},
	} {
		func() {
			tx, err := pool.Raw().Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, c.drift); err != nil {
				t.Fatalf("%s: drift: %v", name, err)
			}
			_, err = tx.Exec(ctx, up)
			var pe *pgconn.PgError
			if !errors.As(err, &pe) || pe.Code != "SF041" || !strings.Contains(pe.Message, c.want) {
				t.Fatalf("%s: want SF041 carrying %q, got %v", name, c.want, err)
			}
		}()
	}
	// Nothing was created by any refused attempt.
	var cols int
	if err := pool.Raw().QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'sessions' AND column_name = 'assurance_seal'`).Scan(&cols); err != nil || cols != 0 {
		t.Fatalf("a refused up created columns: %d %v", cols, err)
	}
}

// legacyStaffSession inserts a pre-S3 staff session row (no assurance columns) as the owner and returns its raw
// refresh token and id. created_at is explicit.
func legacyStaffSession(t *testing.T, tx pgx.Tx, staffID uuid.UUID, tenantID *uuid.UUID, created, expires time.Time) (string, uuid.UUID) {
	t.Helper()
	id := uuid.New()
	tok := uuid.NewString() + uuid.NewString()
	sum := sha256.Sum256([]byte(tok))
	if _, err := tx.Exec(context.Background(), `INSERT INTO sessions (id, principal_type, principal_id, tenant_id, refresh_token_hash, expires_at, created_at)
		VALUES ($1, 'staff', $2, $3, $4, $5, $6)`, id, staffID, tenantID, hex.EncodeToString(sum[:]), expires, created); err != nil {
		t.Fatalf("legacy session: %v", err)
	}
	return tok, id
}

func m138Server(t *testing.T, pool *db.Pool) (*httptest.Server, *syncBuffer) {
	t.Helper()
	keys, err := auth.NewKeyRegistry("k1", map[string]string{"k1": testJWTSecret})
	if err != nil {
		t.Fatal(err)
	}
	logs := &syncBuffer{}
	srv := httptest.NewServer(New(Deps{
		Logger: slog.New(slog.NewJSONHandler(io.MultiWriter(logs), &slog.HandlerOptions{Level: slog.LevelDebug})), DB: pool,
		AuthIssuer: auth.NewIssuer(keys, "platform-api-test", "platform-api-test"), ServiceName: "m138", AccessTokenTTL: 5 * time.Minute,
		RefreshTokenTTL: time.Hour, StaffMFA: testStaffMFA(), PersonResolver: identityresolution.NewMockPersonResolver(), AuthRateLimitPerMinute: -1,
	}))
	t.Cleanup(srv.Close)
	return srv, logs
}

func m138Refresh(t *testing.T, srv *httptest.Server, token string) int {
	t.Helper()
	resp := postJSON(t, srv, "/v1/auth/refresh", "", map[string]string{"refresh_token": token})
	_ = resp.Body.Close()
	return resp.StatusCode
}

// The cutover: every live staff session is revoked (and counted); players are untouched; the marker exists; and the
// review C1 rule holds through the real refresh route.
func TestMigration0138_Cutover_RevocationAndC1Rule(t *testing.T) {
	ctx := context.Background()
	pool, _ := m138Scratch(t, "m0138c_", m138Version-1)
	launchfix.SeedOperatorsAt(t, scratchURLOf(t, pool))
	plat := uuid.New()
	var liveTok string
	var liveID, revokedID, playerID uuid.UUID
	var revokedAt time.Time
	if err := pool.WithPlatformAdmin(ctx, launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := launchfix.CreateTenant(ctx, tx, "M138", "m138-"+uuid.NewString()[:12], "under_platform_licence"); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, NULL, $2, 'x', 'platform_admin')`, plat, "m138-"+plat.String()+"@x.invalid"); err != nil {
			return err
		}
		now := time.Now()
		liveTok, liveID = legacyStaffSession(t, tx, plat, nil, now.Add(-time.Hour), now.Add(time.Hour))
		_, revokedID = legacyStaffSession(t, tx, plat, nil, now.Add(-2*time.Hour), now.Add(time.Hour))
		if err := db.SetSessionInternalOpID(ctx, tx, revokedID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `UPDATE sessions SET revoked_at = now() - interval '1 hour' WHERE id = $1 RETURNING revoked_at`, revokedID).Scan(&revokedAt); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A player session (the migration must not touch it). Players carry a tenant; use the seeded operator tenant-less
	// arm is not possible, so a platform-scoped player row is not legal - insert one under a fresh tenant.
	var playerTenant uuid.UUID
	if err := pool.WithPlatformAdmin(ctx, launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "M138P", "m138p-"+uuid.NewString()[:12], "under_platform_licence")
		playerTenant = tn.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithTenant(ctx, playerTenant, func(ctx context.Context, tx pgx.Tx) error {
		playerID = uuid.New()
		sum := sha256.Sum256([]byte(uuid.NewString()))
		_, err := tx.Exec(ctx, `INSERT INTO sessions (id, principal_type, principal_id, tenant_id, refresh_token_hash, expires_at) VALUES ($1, 'player', $2, $3, $4, now() + interval '1 hour')`,
			playerID, uuid.New(), playerTenant, hex.EncodeToString(sum[:]))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if applied, err := pool.MigrateUp(ctx, m138MigDir(t, m138Version)); err != nil || len(applied) != 1 {
		t.Fatalf("up: %v %v", applied, err)
	}
	var cutover time.Time
	var liveRevoked, oldRevoked, playerRevoked *time.Time
	if err := pool.Raw().QueryRow(ctx, `SELECT at FROM staff_session_cutover`).Scan(&cutover); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for _, p := range []struct {
			id  uuid.UUID
			dst **time.Time
		}{{liveID, &liveRevoked}, {revokedID, &oldRevoked}} {
			if err := db.SetSessionInternalOpID(ctx, tx, p.id); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT revoked_at FROM sessions WHERE id = $1`, p.id).Scan(p.dst); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithTenant(ctx, playerTenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := db.SetSessionInternalOpID(ctx, tx, playerID); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT revoked_at FROM sessions WHERE id = $1`, playerID).Scan(&playerRevoked)
	}); err != nil {
		t.Fatal(err)
	}
	if liveRevoked == nil || !liveRevoked.Equal(cutover) {
		t.Fatalf("the live staff session must be revoked at the cutover: %v vs %v", liveRevoked, cutover)
	}
	if oldRevoked == nil || !oldRevoked.Equal(revokedAt) {
		t.Fatal("an already-revoked staff session keeps its original revocation")
	}
	if playerRevoked != nil {
		t.Fatal("a player session must not be touched")
	}

	srv, _ := m138Server(t, pool)
	countIntegrity := func() (audits, alerts int) {
		if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'auth.session_assurance_invalid'`).Scan(&audits)
		}); err != nil {
			t.Fatal(err)
		}
		if err := pool.WithPlatformAdmin(ctx, launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind LIKE 'staff.%session_integrity'`).Scan(&alerts)
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	// The revoked legacy session: 401, no P1.
	if st := m138Refresh(t, srv, liveTok); st != http.StatusUnauthorized {
		t.Fatalf("legacy staff refresh after the cutover: %d", st)
	}
	if a, al := countIntegrity(); a != 0 || al != 0 {
		t.Fatalf("a pre-cutover session is not tampering (audits=%d alerts=%d)", a, al)
	}
	// C1 decisively: an UNREVOKED NULL-seal row created BEFORE the cutover (only possible with the guard off - the
	// owner switches it off for this one insert) refreshes to a plain 401 with no P1; one created AFTER is a P1.
	plantNull := func(created time.Time) string {
		var tok string
		if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `ALTER TABLE sessions DISABLE TRIGGER sessions_staff_assurance_guard`); err != nil {
				return err
			}
			tok, _ = legacyStaffSession(t, tx, plat, nil, created, time.Now().Add(time.Hour))
			_, err := tx.Exec(ctx, `ALTER TABLE sessions ENABLE TRIGGER sessions_staff_assurance_guard`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return tok
	}
	if st := m138Refresh(t, srv, plantNull(cutover.Add(-time.Minute))); st != http.StatusUnauthorized {
		t.Fatalf("pre-cutover NULL seal: %d", st)
	}
	if a, al := countIntegrity(); a != 0 || al != 0 {
		t.Fatalf("C1: a NULL seal on a pre-cutover session must not raise a P1 (audits=%d alerts=%d)", a, al)
	}
	if st := m138Refresh(t, srv, plantNull(time.Now())); st != http.StatusUnauthorized {
		t.Fatalf("post-cutover NULL seal: %d", st)
	}
	if a, al := countIntegrity(); a != 1 || al != 1 {
		t.Fatalf("C1: a NULL seal after the cutover is tampering: audits=%d alerts=%d, want 1/1", a, al)
	}
	// The cutover marker is immutable: under FORCE RLS no UPDATE / DELETE policy exists (nothing is reachable, even
	// for the owner), and with FORCE lifted the SF099 trigger refuses (checked inside a rolled-back transaction).
	if tag, err := pool.Raw().Exec(ctx, `UPDATE staff_session_cutover SET at = now()`); err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("the cutover marker must not be updatable under FORCE RLS: %v %v", tag, err)
	}
	func() {
		tx, err := pool.Raw().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `ALTER TABLE staff_session_cutover NO FORCE ROW LEVEL SECURITY`); err != nil {
			t.Fatal(err)
		}
		for _, sql := range []string{`SAVEPOINT a`, `UPDATE staff_session_cutover SET at = now()`} {
			_, err = tx.Exec(ctx, sql)
		}
		if pgCode(err) != "SF099" {
			t.Fatalf("UPDATE of the cutover marker: want SF099, got %v", err)
		}
		_, _ = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT a`)
		if _, err := tx.Exec(ctx, `DELETE FROM staff_session_cutover`); pgCode(err) != "SF099" {
			t.Fatalf("DELETE of the cutover marker: want SF099, got %v", err)
		}
	}()
}

// scratchURLOf recovers the owner URL of a scratch pool's database (for fixtures that open their own connection).
func scratchURLOf(t *testing.T, pool *db.Pool) string {
	t.Helper()
	var name string
	if err := pool.Raw().QueryRow(context.Background(), `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	base := os.Getenv("TEST_DATABASE_URL")
	i := strings.LastIndex(base, "/")
	q := ""
	if j := strings.Index(base[i:], "?"); j >= 0 {
		q = base[i+j:]
	}
	return base[:i+1] + name + q
}

// The rolling deploy (ADR 0119 s13.1a item 4; docs/runbooks/staff-mfa-s3-cutover.md): both orders fail closed for
// staff and leave players working.
func TestMigration0138_RollingDeploy_FailsClosed(t *testing.T) {
	ctx := context.Background()
	// (1) Pre-S3 code after 0138: the old IssueSession wrote a staff row without assurance columns: SF040.
	pool138, _ := m138Scratch(t, "m0138r_", m138Version)
	if err := pool138.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO sessions (principal_type, principal_id, tenant_id, refresh_token_hash, expires_at) VALUES ('staff', $1, NULL, $2, now() + interval '1 hour')`,
			uuid.New(), uuid.NewString())
		return err
	}); pgCode(err) != "SF040" {
		t.Fatalf("pre-S3 staff session write after 0138: want SF040, got %v", err)
	}
	// (2) S3 code on the 0137 schema: player register + refresh work; a staff refresh fails closed (5xx, nothing issued).
	pool137, _ := m138Scratch(t, "m0137r_", m138Version-1)
	launchfix.SeedOperatorsAt(t, scratchURLOf(t, pool137))
	var tenantID, brandID uuid.UUID
	var brandSlug string
	if err := pool137.WithPlatformAdmin(ctx, launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "M137R", "m137r-"+uuid.NewString()[:12], "under_platform_licence")
		tenantID = tn.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool137.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		b, err := launchfix.CreateBrand(ctx, tx, tenantID, "M137R", "m137rb-"+uuid.NewString()[:12])
		brandSlug, brandID = b.Slug, b.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	launchfix.SetTenantStatusAt(t, scratchURLOf(t, pool137), tenantID, "active")
	launchfix.SetBrandStatusAt(t, scratchURLOf(t, pool137), tenantID, brandID, "active")
	srv, _ := m138Server(t, pool137)
	resp := postJSON(t, srv, "/v1/auth/register", "", map[string]string{"brand_slug": brandSlug, "email": "p-" + uuid.NewString() + "@x.invalid", "password": "a-decent-password-1"})
	var tokens tokenPairResponse
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("player register on the 0137 schema: %d", resp.StatusCode)
	}
	decodeBody(t, resp, &tokens)
	if st := m138Refresh(t, srv, tokens.RefreshToken); st != http.StatusOK {
		t.Fatalf("player refresh with S3 code on the 0137 schema must keep working, got %d", st)
	}
	var staffTok string
	if err := pool137.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		plat := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, NULL, $2, 'x', 'platform_admin')`, plat, "m137r-"+plat.String()+"@x.invalid"); err != nil {
			return err
		}
		staffTok, _ = legacyStaffSession(t, tx, plat, nil, time.Now(), time.Now().Add(time.Hour))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if st := m138Refresh(t, srv, staffTok); st < 500 {
		t.Fatalf("a staff refresh with S3 code on the 0137 schema must fail closed (5xx), got %d", st)
	}
}

func TestMigration0138_Down_Refusals(t *testing.T) {
	ctx := context.Background()
	pool, _ := m138Scratch(t, "m0138d_", m138Version)
	full := m138MigDir(t, m138Version)
	plat := uuid.New()
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, NULL, $2, 'x', 'platform_admin')`, plat, "m138d-"+plat.String()+"@x.invalid")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A live MFA-assured staff session refuses the down.
	var sessionID uuid.UUID
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		s, err := auth.IssueStaffSession(ctx, tx, testStaffMFAKeys(), auth.StaffSessionParams{StaffID: plat, AMR: []string{"pwd", "otp"}, Now: time.Now(),
			AbsoluteTTL: time.Hour, IdleTTL: 30 * time.Minute})
		sessionID = s.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err := pool.MigrateDown(ctx, full, 1)
	if pgCode(err) != "SF042" || !strings.Contains(err.Error(), "live_sessions=1") {
		t.Fatalf("a live assured staff session must refuse the down with SF042: %v", err)
	}
	// Revoked, it no longer refuses; a staff alert row does.
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := db.SetSessionInternalOpID(ctx, tx, sessionID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = now() WHERE id = $1`, sessionID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithPlatformAdmin(ctx, plat, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alerts (kind, severity, discriminator, attributes) VALUES ('staff.platform_mfa_integrity', 'p1', $1, jsonb_build_object('staff_id', $2::text, 'cause', 'factor_missing'))`,
			"staff:"+plat.String(), plat.String())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err = pool.MigrateDown(ctx, full, 1)
	if pgCode(err) != "SF042" || !strings.Contains(err.Error(), "alerts=1") {
		t.Fatalf("a staff security alert must refuse the down with SF042: %v", err)
	}
	var cols int
	if err := pool.Raw().QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name = 'sessions' AND column_name = 'assurance_seal'`).Scan(&cols); err != nil || cols != 1 {
		t.Fatalf("a refused down must drop nothing: %d %v", cols, err)
	}
}
