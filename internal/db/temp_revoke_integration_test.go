//go:build integration

// PRH-2 R2 (migration 0116, ADR 0108, TRIGGER-SEARCH-PATH-1): the invariant
// "the runtime role cannot create temporary objects". Every probe below runs as
// the REAL runtime role (TEST_RUNTIME_DATABASE_URL), asserted first to be
// neither superuser nor BYPASSRLS. Fixtures and the migration round trip use the
// OWNER role on a private scratch database.
package db

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// tempOwnerPool is the owner role's pool, asserted neither superuser nor BYPASSRLS.
func tempOwnerPool(t *testing.T) *Pool {
	t.Helper()
	p := testPool(t)
	var super, bypass bool
	if err := p.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass)
	}); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatalf("owner pool must be neither superuser nor BYPASSRLS (super=%v bypassrls=%v)", super, bypass)
	}
	return p
}

func pgState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

type tempACL struct{ publicTemp, runtimeTemp, ownerTemp, runtimeIsSuper, runtimeBypass bool }

func readTempACL(t *testing.T, p *Pool) tempACL {
	t.Helper()
	var a tempACL
	if err := p.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM pg_database d, LATERAL aclexplode(COALESCE(d.datacl, acldefault('d', d.datdba))) x
			                WHERE d.datname = current_database() AND x.grantee = 0 AND x.privilege_type = 'TEMPORARY'),
			       has_database_privilege('igaming_runtime', current_database(), 'TEMP'),
			       has_database_privilege((SELECT pg_get_userbyid(datdba) FROM pg_database WHERE datname = current_database()), current_database(), 'TEMP'),
			       (SELECT rolsuper FROM pg_roles WHERE rolname = 'igaming_runtime'),
			       (SELECT rolbypassrls FROM pg_roles WHERE rolname = 'igaming_runtime')`).
			Scan(&a.publicTemp, &a.runtimeTemp, &a.ownerTemp, &a.runtimeIsSuper, &a.runtimeBypass)
	}); err != nil {
		t.Fatalf("read TEMP ACL: %v", err)
	}
	return a
}

// (1) The catalogue: no PUBLIC TEMP, no runtime TEMP, owner keeps TEMP.
func TestTempRevoke_Catalogue_RuntimeAndPublicHaveNoTemp_OwnerKeepsIt(t *testing.T) {
	rt := kycRuntimePool(t, 1) // asserts NOT rolsuper AND NOT rolbypassrls
	owner := tempOwnerPool(t)
	a := readTempACL(t, owner)
	if a.runtimeIsSuper || a.runtimeBypass {
		t.Fatalf("igaming_runtime must be neither superuser nor BYPASSRLS: %+v", a)
	}
	if a.publicTemp {
		t.Error("PUBLIC still holds TEMPORARY on the database (migration 0116 not in effect)")
	}
	if a.runtimeTemp {
		t.Error("igaming_runtime still holds TEMPORARY on the database")
	}
	if !a.ownerTemp {
		t.Error("the database owner must keep TEMPORARY (the owner role runs migrations and TEMP-probing tests)")
	}
	// The same answer from the runtime role's own session.
	var self bool
	if err := rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT has_database_privilege(current_user, current_database(), 'TEMP')`).Scan(&self)
	}); err != nil {
		t.Fatal(err)
	}
	if self {
		t.Error("the runtime session reports it holds TEMP")
	}
}

type tempForm struct {
	name, sql string
	// noDO: plpgsql cannot EXECUTE a SELECT ... INTO (it means a variable there).
	noDO bool
}

var tempForms = []tempForm{
	{"CREATE TEMP TABLE", `CREATE TEMP TABLE tr_x (a int)`, false},
	{"CREATE TEMP TABLE AS", `CREATE TEMP TABLE tr_x AS SELECT 1 AS a`, false},
	{"SELECT INTO TEMP", `SELECT 1 AS a INTO TEMP TABLE tr_x`, true},
	{"CREATE TEMP VIEW", `CREATE TEMP VIEW tr_x AS SELECT 1 AS a`, false},
	{"CREATE TEMP SEQUENCE", `CREATE TEMP SEQUENCE tr_x`, false},
	{"CREATE TABLE pg_temp.x", `CREATE TABLE pg_temp.tr_x (a int)`, false},
	{"CREATE FUNCTION pg_temp.f()", `CREATE FUNCTION pg_temp.tr_f() RETURNS int LANGUAGE sql AS $f$ SELECT 1 $f$`, false},
}

// (2) Every TEMP-creating form is refused to the runtime role with 42501, direct,
// inside a DO block, and inside a transaction with pg_temp first on the path.
// The same statements succeed for the owner (so a refusal is the privilege, not
// a typo).
func TestTempRevoke_RuntimeRoleCannotCreateAnyTempObject_AllForms(t *testing.T) {
	rt := kycRuntimePool(t, 1)
	owner := tempOwnerPool(t)
	// The form list is pinned: dropping or skipping a form must fail the test.
	wantForms := "CREATE TEMP TABLE|CREATE TEMP TABLE AS|SELECT INTO TEMP|CREATE TEMP VIEW|CREATE TEMP SEQUENCE|CREATE TABLE pg_temp.x|CREATE FUNCTION pg_temp.f()"
	var gotForms []string
	for _, f := range tempForms {
		gotForms = append(gotForms, f.name)
	}
	if strings.Join(gotForms, "|") != wantForms {
		t.Fatalf("TEMP form list changed or a form was skipped: %v", gotForms)
	}
	var attempts int
	errRollback := errors.New("rollback")
	wrap := map[string]func(string) string{
		"direct": func(s string) string { return s },
		"do":     func(s string) string { return "DO $do$ BEGIN EXECUTE $q$" + s + "$q$; END $do$" },
	}
	if !strings.HasPrefix(wrap["do"]("SELECT 1"), "DO ") || wrap["direct"]("SELECT 1") != "SELECT 1" {
		t.Fatal("the DO-block / direct wrappers are not what they claim to be")
	}
	run := func(p *Pool, mode, sqlText string, searchPathFirst bool) error {
		err := p.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			if searchPathFirst {
				if _, err := tx.Exec(ctx, `SET LOCAL search_path = pg_temp, public`); err != nil {
					return fmt.Errorf("set search_path: %w", err)
				}
			}
			if _, err := tx.Exec(ctx, wrap[mode](sqlText)); err != nil {
				return err
			}
			return errRollback
		})
		if errors.Is(err, errRollback) {
			return nil
		}
		return err
	}
	for _, f := range tempForms {
		for _, mode := range []string{"direct", "do"} {
			if mode == "do" && f.noDO {
				continue
			}
			for _, spFirst := range []bool{false, true} {
				name := fmt.Sprintf("%s/%s/pg_temp_first=%v", f.name, mode, spFirst)
				if err := run(owner, mode, f.sql, spFirst); err != nil {
					t.Fatalf("%s: the statement must be valid for the owner (non-vacuity): %v", name, err)
				}
				err := run(rt, mode, f.sql, spFirst)
				attempts++
				if err == nil {
					t.Errorf("%s: the RUNTIME role created a temporary object", name)
				} else if pgState(err) != "42501" {
					t.Errorf("%s: want SQLSTATE 42501, got %q (%v)", name, pgState(err), err)
				}
			}
		}
	}
	if want := 7*2*2 - 2; attempts != want { // SELECT INTO has no DO variant
		t.Fatalf("expected %d runtime attempts, ran %d", want, attempts)
	}
}

// stagedThrough0116 copies every migration numbered <= 116 into a temp dir, so
// the round-trip test (MigrateDown steps=1 rolls back exactly 0116) does not
// depend on what is newest in the live migrations directory.
func stagedThrough0116(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "..", "migrations")
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var copied int
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".sql") || len(n) < 4 {
			continue
		}
		v := 0
		if _, err := fmt.Sscanf(n[:4], "%d", &v); err != nil || v > 116 {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, n))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, n), b, 0o644); err != nil {
			t.Fatal(err)
		}
		copied++
	}
	if copied < 232 {
		t.Fatalf("staged only %d files through 0116, want at least 232 (116 up + 116 down)", copied)
	}
	return dir
}

// (2b) PRH-2 R2 security C2: the production startup check reads the same fact.
func TestTempRevoke_ConnectingRoleHoldsTemp_RuntimeFalse_OwnerTrue(t *testing.T) {
	rt := kycRuntimePool(t, 1)
	owner := tempOwnerPool(t)
	if holds, err := rt.ConnectingRoleHoldsTemp(context.Background()); err != nil || holds {
		t.Fatalf("runtime role: holds=%v err=%v, want false", holds, err)
	}
	if holds, err := owner.ConnectingRoleHoldsTemp(context.Background()); err != nil || !holds {
		t.Fatalf("owner role: holds=%v err=%v, want true (the owner keeps TEMP)", holds, err)
	}
	// And through the real production gate: the runtime pool passes in production.
	if err := VerifyRuntimeRoleInProduction(context.Background(), "production", rt); err != nil {
		t.Fatalf("production gate with the runtime role: %v", err)
	}
}

// migration 0116 file contents (the real files).
func tempRevokeUpSQL(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "migrations", "0116_revoke_temp_from_runtime.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func execOn(t *testing.T, rawURL string, fn func(ctx context.Context, conn *pgx.Conn) error) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, rawURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if err := fn(ctx, conn); err != nil {
		t.Fatal(err)
	}
}

func runtimeURLFor(t *testing.T, scratchURL string) string {
	t.Helper()
	rtURL := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if rtURL == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set")
	}
	su, err := url.Parse(scratchURL)
	if err != nil {
		t.Fatal(err)
	}
	ru, err := url.Parse(rtURL)
	if err != nil {
		t.Fatal(err)
	}
	ru.Path = su.Path
	return ru.String()
}

// (5) Migration 0116: up on a fresh database, down, up again; verify clean; the
// in-migration assertion fires when the invariant cannot be established.
func TestTempRevoke_Migration_UpDownUp_VerifyClean_AssertionFires(t *testing.T) {
	scratchURL := scratchdb.New(t, "temprev_")
	ctx := context.Background()
	migDir := stagedThrough0116(t) // stable when later migrations (0117...) land
	p := checksumScratchPool(t, scratchURL)
	rtURL := runtimeURLFor(t, scratchURL)
	su, err := url.Parse(scratchURL)
	if err != nil {
		t.Fatal(err)
	}
	dbIdent := pgx.Identifier{strings.TrimPrefix(su.Path, "/")}.Sanitize()

	// Before: the PostgreSQL default - PUBLIC (hence the runtime role) has TEMP.
	if a := readTempACL(t, p); !a.publicTemp || !a.runtimeTemp || !a.ownerTemp {
		t.Fatalf("fresh database must start with the PostgreSQL default (PUBLIC TEMP): %+v", a)
	}
	// A role-only revoke is a NO-OP while PUBLIC holds TEMP (the reason for the PUBLIC revoke).
	execOn(t, scratchURL, func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `REVOKE TEMPORARY ON DATABASE `+dbIdent+` FROM igaming_runtime`)
		return err
	})
	if a := readTempACL(t, p); !a.runtimeTemp {
		t.Fatal("a role-only REVOKE must be a no-op while PUBLIC holds TEMP (runtime inherits through PUBLIC)")
	}

	// The migration runs ASSERTING.
	applied, err := p.MigrateUp(ctx, migDir)
	if err != nil {
		t.Fatalf("migrate up (full chain): %v", err)
	}
	if len(applied) == 0 || applied[len(applied)-1] != 116 {
		t.Fatalf("expected the staged chain to end at 116, applied %v", applied)
	}
	if a := readTempACL(t, p); a.publicTemp || a.runtimeTemp || !a.ownerTemp {
		t.Fatalf("after up: want no PUBLIC/runtime TEMP and owner TEMP, got %+v", a)
	}
	rep, err := p.VerifyMigrations(ctx, migDir)
	if err != nil || !rep.OK() {
		t.Fatalf("verify after up: %v %+v", err, rep)
	}
	// Idempotent: running the up SQL again on the already-revoked database passes.
	execOn(t, scratchURL, func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, tempRevokeUpSQL(t))
		return err
	})

	// Down: the historical default is restored (0116 is the newest of the STAGED set).
	rolled, err := p.MigrateDown(ctx, migDir, 1)
	if err != nil || len(rolled) != 1 || rolled[0] != 116 {
		t.Fatalf("migrate down 1: rolled=%v err=%v", rolled, err)
	}
	if a := readTempACL(t, p); !a.publicTemp || !a.runtimeTemp || !a.ownerTemp {
		t.Fatalf("after down: PUBLIC TEMP must be granted back, got %+v", a)
	}

	// The assertion fires: run the up SQL as a NON-OWNER role (the runtime role
	// connected to the scratch database, which can revoke nothing). The REVOKEs
	// only warn; the assertion must RAISE and PUBLIC TEMP must stay untouched. No
	// role or credential is changed: the scratch database merely grants CONNECT to
	// the existing runtime role.
	execOn(t, scratchURL, func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `GRANT CONNECT ON DATABASE `+dbIdent+` TO igaming_runtime`)
		return err
	})
	execOn(t, rtURL, func(ctx context.Context, c *pgx.Conn) error {
		var super, bypass bool
		if err := c.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass); err != nil {
			return err
		}
		if super || bypass {
			return fmt.Errorf("runtime role must be neither superuser nor BYPASSRLS")
		}
		_, err := c.Exec(ctx, tempRevokeUpSQL(t))
		if err == nil {
			return errors.New("the migration silently succeeded as a role that cannot revoke (the assertion is missing)")
		}
		if pgState(err) != "42501" || !strings.Contains(err.Error(), "TEMP-REVOKE-1: PUBLIC still holds TEMPORARY") {
			return fmt.Errorf("want 42501 TEMP-REVOKE-1 (PUBLIC still holds), got %q: %v", pgState(err), err)
		}
		return nil
	})
	if a := readTempACL(t, p); !a.publicTemp {
		t.Fatal("the failed non-owner run must not have changed anything")
	}

	// The second assertion (runtime holds TEMP some other way) by simulation in a
	// rolled-back owner transaction: PUBLIC is revoked, the runtime role gets a
	// DIRECT grant, and the migration's own role-revoke statement is removed from
	// the SQL text, so only the assertion can catch it.
	revokeRole := "EXECUTE format('REVOKE TEMPORARY ON DATABASE %I FROM igaming_runtime', db);"
	up := tempRevokeUpSQL(t)
	if !strings.Contains(up, revokeRole) {
		t.Fatal("test out of date: the role revoke statement was not found in the up SQL")
	}
	simulated := strings.Replace(up, revokeRole, "NULL;", 1)
	execOn(t, scratchURL, func(ctx context.Context, c *pgx.Conn) error {
		tx, err := c.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		for _, s := range []string{`REVOKE TEMPORARY ON DATABASE ` + dbIdent + ` FROM PUBLIC`, `GRANT TEMPORARY ON DATABASE ` + dbIdent + ` TO igaming_runtime`} {
			if _, err := tx.Exec(ctx, s); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, simulated)
		if err == nil || pgState(err) != "42501" || !strings.Contains(err.Error(), "igaming_runtime still holds TEMPORARY") {
			return fmt.Errorf("the runtime-holds-TEMP assertion did not fire: %v", err)
		}
		return nil
	})

	// Up again: back to the invariant, verify clean, and the real SQL also removes a direct grant.
	if _, err := p.MigrateUp(ctx, migDir); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	if a := readTempACL(t, p); a.publicTemp || a.runtimeTemp || !a.ownerTemp {
		t.Fatalf("after re-up: %+v", a)
	}
	if rep, err := p.VerifyMigrations(ctx, migDir); err != nil || !rep.OK() {
		t.Fatalf("verify after re-up: %v %+v", err, rep)
	}
	execOn(t, scratchURL, func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `GRANT TEMPORARY ON DATABASE `+dbIdent+` TO igaming_runtime`)
		return err
	})
	if a := readTempACL(t, p); !a.runtimeTemp {
		t.Fatal("setup: direct grant did not take")
	}
	execOn(t, scratchURL, func(ctx context.Context, c *pgx.Conn) error { _, err := c.Exec(ctx, tempRevokeUpSQL(t)); return err })
	if a := readTempACL(t, p); a.runtimeTemp || a.publicTemp {
		t.Fatalf("the migration must revoke a direct runtime grant too: %+v", a)
	}

	// And on the scratch database the runtime role really cannot create TEMP objects.
	execOn(t, rtURL, func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, `CREATE TEMP TABLE tr_scratch (a int)`)
		if pgState(err) != "42501" {
			return fmt.Errorf("scratch runtime CREATE TEMP: want 42501, got %v", err)
		}
		return nil
	})
}

// The provisioning scripts carry the same revoke, so a freshly provisioned
// database is safe before migrations run. Statically pinned (the scripts create
// roles, so they are not executed here).
func TestTempRevoke_ProvisioningScriptsCarryTheRevoke(t *testing.T) {
	for _, f := range []string{
		filepath.Join("..", "..", "deploy", "init-app-role.sql"),
		filepath.Join("..", "..", "deploy", "aws", "sql", "init-runtime-role.rds.sql"),
	} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		for _, want := range []string{
			"REVOKE TEMPORARY ON DATABASE %I FROM PUBLIC', current_database()",
			"REVOKE TEMPORARY ON DATABASE %I FROM igaming_runtime', current_database()",
		} {
			if !strings.Contains(s, want) {
				t.Errorf("%s: missing %q", f, want)
			}
		}
	}
}

// QA P3 / security D2: pin the 'TEMP' privilege NAME in ConnectingRoleHoldsTemp.
// On a THROWAWAY scratch database (database-level GRANT/REVOKE only; no role is
// created or altered) the runtime role holds CREATE but not TEMP, then TEMP but
// not CREATE. A query that asked about CREATE instead of TEMP gets both wrong.
func TestTempRevoke_ConnectingRoleHoldsTemp_PinsTheTempPrivilegeName(t *testing.T) {
	scratchURL := scratchdb.New(t, "temprevp_")
	rtURL := runtimeURLFor(t, scratchURL)
	su, err := url.Parse(scratchURL)
	if err != nil {
		t.Fatal(err)
	}
	ident := pgx.Identifier{strings.TrimPrefix(su.Path, "/")}.Sanitize()
	ctx := context.Background()
	rt := checksumScratchPool(t, rtURL)
	var super, bypass bool
	if err := rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass)
	}); err != nil || super || bypass {
		t.Fatalf("runtime role must be neither superuser nor BYPASSRLS: %v super=%v bypass=%v", err, super, bypass)
	}
	setup := func(stmts ...string) {
		execOn(t, scratchURL, func(ctx context.Context, c *pgx.Conn) error {
			for _, s := range stmts {
				if _, err := c.Exec(ctx, s); err != nil {
					return err
				}
			}
			return nil
		})
	}
	holds := func() bool {
		h, err := rt.ConnectingRoleHoldsTemp(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	// Default database: PUBLIC TEMP -> true.
	if !holds() {
		t.Fatal("a fresh database grants TEMP to PUBLIC: want true")
	}
	// CREATE but not TEMP.
	setup(`REVOKE TEMPORARY ON DATABASE `+ident+` FROM PUBLIC`, `GRANT CREATE ON DATABASE `+ident+` TO igaming_runtime`)
	if holds() {
		t.Fatal("the runtime role holds CREATE but not TEMP: ConnectingRoleHoldsTemp must be false")
	}
	// TEMP but not CREATE.
	setup(`REVOKE CREATE ON DATABASE `+ident+` FROM igaming_runtime`, `GRANT TEMPORARY ON DATABASE `+ident+` TO igaming_runtime`)
	if !holds() {
		t.Fatal("the runtime role holds TEMP but not CREATE: ConnectingRoleHoldsTemp must be true")
	}
	if err := VerifyRuntimeRoleInProduction(ctx, "production", rt); err == nil || !strings.Contains(err.Error(), "TEMPORARY") {
		t.Fatalf("production gate must refuse a role holding TEMP, got %v", err)
	}
}

// ADR 0108 I2: ConnectingRoleHasMemberships reads pg_auth_members for current_user.
// The runtime role has none; the test-admin role (member of the owner role) has one.
// No role or membership is created or changed.
func TestTempRevoke_ConnectingRoleHasMemberships_RuntimeNone_AdminSome(t *testing.T) {
	rt := kycRuntimePool(t, 1)
	if has, err := rt.ConnectingRoleHasMemberships(context.Background()); err != nil || has {
		t.Fatalf("runtime role: has=%v err=%v, want no memberships", has, err)
	}
	adminURL := os.Getenv("TEST_ADMIN_DATABASE_URL")
	if adminURL == "" {
		t.Skip("TEST_ADMIN_DATABASE_URL not set")
	}
	admin, err := Connect(context.Background(), adminURL, 1, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(admin.Close)
	if has, err := admin.ConnectingRoleHasMemberships(context.Background()); err != nil || !has {
		t.Fatalf("test-admin role (member of the owner role): has=%v err=%v, want true", has, err)
	}
}
