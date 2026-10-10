//go:build integration

// Package scratchdb creates throwaway PostgreSQL databases for migration
// and RLS integration tests that must start from an empty schema (every
// other integration test runs against the shared, already-migrated
// TEST_DATABASE_URL database).
//
// Test-only by construction: every file carries the `integration` build
// tag, so this package is never compiled into an application binary, and
// it is the only non-_test.go file allowed to read TEST_ADMIN_DATABASE_URL
// (a CI guard in .github/workflows/ci.yml enforces that).
//
// Role model (Stage 10 W0, docs/testing/testing-strategy.md):
//
//   - TEST_ADMIN_DATABASE_URL connects as igaming_test_admin, a CI/dev-only
//     role with CREATEDB, NOSUPERUSER, NOBYPASSRLS and membership in the
//     application owner role. It is used for CREATE DATABASE and DROP
//     DATABASE only - never for any assertion.
//   - The scratch database is created OWNER <TEST_DATABASE_URL's role>, so
//     migrations and RLS assertions run as the same non-superuser,
//     non-BYPASSRLS owner as every other integration test. The application
//     roles (igaming, igaming_runtime) are never granted CREATEDB.
//
// If either variable is unset the calling test is skipped; the helper never
// falls back to creating databases through TEST_DATABASE_URL.
package scratchdb

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// New creates an empty scratch database whose name starts with prefix,
// owned by TEST_DATABASE_URL's role, and returns a TEST_DATABASE_URL-style
// URL pointing at it. The database is dropped on test cleanup.
func New(t testing.TB, prefix string) string {
	t.Helper()
	baseURL := os.Getenv("TEST_DATABASE_URL")
	if baseURL == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	adminURL := os.Getenv("TEST_ADMIN_DATABASE_URL")
	if adminURL == "" {
		t.Skip("TEST_ADMIN_DATABASE_URL not set; skipping scratch-database test (see docs/testing/testing-strategy.md)")
	}

	base, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	owner := base.User.Username()
	if owner == "" {
		t.Fatal("TEST_DATABASE_URL has no user; cannot choose the scratch database owner")
	}

	name := prefix + strings.ReplaceAll(uuid.New().String(), "-", "")[:16]
	ident := pgx.Identifier{name}.Sanitize()

	ctx := context.Background()
	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatalf("connect as test admin: %v", err)
	}
	defer func() { _ = admin.Close(ctx) }()
	var adminSuper, adminBypass bool
	if err := admin.QueryRow(ctx,
		`SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&adminSuper, &adminBypass); err != nil {
		t.Fatalf("inspect test admin role: %v", err)
	}
	if adminSuper || adminBypass {
		t.Fatal("TEST_ADMIN_DATABASE_URL must use the CREATEDB-only igaming_test_admin role, not a superuser or BYPASSRLS role")
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+ident+" OWNER "+pgx.Identifier{owner}.Sanitize()); err != nil {
		t.Fatalf("create scratch database %s: %v", name, err)
	}
	t.Cleanup(func() {
		conn, err := pgx.Connect(context.Background(), adminURL)
		if err != nil {
			t.Logf("scratch database %s left behind (connect failed: %v)", name, err)
			return
		}
		defer func() { _ = conn.Close(context.Background()) }()
		// pool.Close returns before the server has reaped the closed backends,
		// and WITH (FORCE) cannot terminate another role's session ("permission
		// denied to terminate process"). Wait (bounded) for the sessions to
		// drain and retry instead of leaking the database. No privilege is
		// changed and no extra session-termination is attempted (WITH (FORCE) still
		// ends the scratch database's own sessions that the admin role may signal).
		var dropErr error
		for attempt := 0; attempt < 40; attempt++ {
			if _, dropErr = conn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+ident+" WITH (FORCE)"); dropErr == nil {
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
		t.Logf("scratch database %s left behind (drop failed after retries: %v)", name, dropErr)
	})

	base.Path = "/" + name
	scratchURL := base.String()
	assertUnprivilegedOwner(t, scratchURL, owner)
	return scratchURL
}

// AsAdmin returns a URL for the SAME scratch database connected as the test-admin role
// (TEST_ADMIN_DATABASE_URL's user). That role is a MEMBER of the application owner role but does
// not own the tables itself, which is exactly the case the production role check must still
// refuse (ADR 0112 security S-7). Used for that one assertion only; the helper stays in this
// file so the CI guard on reading TEST_ADMIN_DATABASE_URL keeps holding.
func AsAdmin(t testing.TB, scratchURL string) string {
	t.Helper()
	adminURL := os.Getenv("TEST_ADMIN_DATABASE_URL")
	if adminURL == "" {
		t.Skip("TEST_ADMIN_DATABASE_URL not set")
	}
	a, err := url.Parse(adminURL)
	if err != nil {
		t.Fatalf("parse TEST_ADMIN_DATABASE_URL: %v", err)
	}
	s, err := url.Parse(scratchURL)
	if err != nil {
		t.Fatalf("parse scratch URL: %v", err)
	}
	a.Path = s.Path
	return a.String()
}

// assertUnprivilegedOwner proves the scratch database is used exactly as
// every other integration test uses the shared one: connected as the
// non-superuser, non-BYPASSRLS owner. Without this, a misconfigured admin
// URL could make RLS tests pass for the wrong reason (the trap described at
// the top of .github/workflows/ci.yml).
func assertUnprivilegedOwner(t testing.TB, scratchURL, owner string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, scratchURL)
	if err != nil {
		t.Fatalf("connect to scratch database as its owner: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var currentUser, dbOwner string
	var super, bypass bool
	if err := conn.QueryRow(ctx, `
		SELECT current_user, pg_get_userbyid(d.datdba), r.rolsuper, r.rolbypassrls
		  FROM pg_database d, pg_roles r
		 WHERE d.datname = current_database() AND r.rolname = current_user`).
		Scan(&currentUser, &dbOwner, &super, &bypass); err != nil {
		t.Fatalf("inspect scratch database role: %v", err)
	}
	if currentUser != owner || dbOwner != owner {
		t.Fatalf("scratch database connected as %q and owned by %q, want both %q", currentUser, dbOwner, owner)
	}
	if super || bypass {
		t.Fatalf("scratch database role %q is superuser=%v bypassrls=%v; RLS assertions would pass for the wrong reason", currentUser, super, bypass)
	}
}
