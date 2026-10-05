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
	errRollback := errors.New("rollback")
	wrap := map[string]func(string) string{
		"direct": func(s string) string { return s },
		"do":     func(s string) string { return "DO $do$ BEGIN EXECUTE $q$" + s + "$q$; END $do$" },
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
				if err == nil {
					t.Errorf("%s: the RUNTIME role created a temporary object", name)
				} else if pgState(err) != "42501" {
					t.Errorf("%s: want SQLSTATE 42501, got %q (%v)", name, pgState(err), err)
				}
			}
		}
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
	migDir := filepath.Join("..", "..", "migrations")
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
	if len(applied) == 0 || applied[len(applied)-1] < 116 {
		t.Fatalf("expected the chain to reach at least 116, applied %v", applied)
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

	// Down: the historical default is restored. (0116 must be the newest migration
	// for steps=1 to roll it back; a later migration makes this test's step count
	// wrong, which is the intended tripwire.)
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
