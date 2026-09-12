//go:build integration

// Direct RLS-level proofs for the sessions table hardening
// (docs/decisions/0016-sessions-rls-hardening.md, migration 0018).
// These deliberately bypass internal/auth's own helper functions where
// possible and issue raw SQL under different scopes, so a passing test
// proves the DATABASE denies access - not that ListActiveSessions/
// RevokeSession happen to filter correctly in application code (the
// property the old FOR SELECT USING (true) policy relied on and this
// hardening pass removes the reliance on).
package auth

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func sessionRLSTestPool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := db.Connect(context.Background(), url, 5, 5*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func createSessionRLSTestTenant(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, name, slug, licensing_model) VALUES ($1, $2, $3, 'under_platform_licence')`,
			id, "Session RLS Test Tenant "+id.String(), "session-rls-"+id.String(),
		)
		return err
	})
	if err != nil {
		t.Fatalf("failed to create test tenant: %v", err)
	}
	t.Cleanup(func() {
		_ = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM sessions WHERE tenant_id = $1`, id)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, id)
			return err
		})
	})
	return id
}

// issueTestSession creates a real session (via the production IssueSession
// path, so its refresh_token_hash/expiry/etc. are exactly what a real
// login would produce) for principalID under tenantID (uuid.Nil for a
// platform-scoped principal).
func issueTestSession(t *testing.T, pool *db.Pool, tenantID, principalID uuid.UUID) Session {
	t.Helper()
	return issueTestSessionAs(t, pool, PrincipalPlayer, tenantID, principalID)
}

func issueTestSessionAs(t *testing.T, pool *db.Pool, principalType PrincipalType, tenantID, principalID uuid.UUID) Session {
	t.Helper()
	scope := pool.WithoutTenant
	if tenantID != uuid.Nil {
		scope = func(ctx context.Context, fn db.TxFunc) error { return pool.WithTenant(ctx, tenantID, fn) }
	}
	var session Session
	err := scope(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		session, err = IssueSession(ctx, tx, principalType, principalID, tenantID, time.Hour, "test-agent", "127.0.0.1")
		return err
	})
	if err != nil {
		t.Fatalf("failed to issue test session: %v", err)
	}
	t.Cleanup(func() {
		_ = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, session.ID)
			return err
		})
	})
	return session
}

// countVisibleSessions runs a bare, unfiltered SELECT count(*) FROM
// sessions inside the transaction fn provides - the same query regardless
// of which scoping helper opened the transaction, so any difference in
// result is entirely down to which RLS policy applied, not to query
// shape.
func countVisibleSessions(ctx context.Context, tx pgx.Tx) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM sessions`).Scan(&n)
	return n, err
}

func sessionVisible(ctx context.Context, tx pgx.Tx, id uuid.UUID) (bool, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE id = $1`, id).Scan(&n)
	return n > 0, err
}

// TestSessionRLS_PrincipalCannotReadAnotherPrincipalsSessionsWithinSameTenant
// is the regression test for the exact vulnerability this hardening pass
// closes: before migration 0018, two principals in the SAME tenant could
// read each other's session rows via a bare SELECT, because the old
// policy was FOR SELECT USING (true).
func TestSessionRLS_PrincipalCannotReadAnotherPrincipalsSessionsWithinSameTenant(t *testing.T) {
	pool := sessionRLSTestPool(t)
	tenant := createSessionRLSTestTenant(t, pool)
	principalA := uuid.New()
	principalB := uuid.New()
	sessionA := issueTestSession(t, pool, tenant, principalA)
	sessionB := issueTestSession(t, pool, tenant, principalB)

	err := pool.WithPrincipalScope(context.Background(), tenant, principalA, func(ctx context.Context, tx pgx.Tx) error {
		visibleA, err := sessionVisible(ctx, tx, sessionA.ID)
		if err != nil {
			return err
		}
		if !visibleA {
			t.Error("expected principal A to see their own session")
		}
		visibleB, err := sessionVisible(ctx, tx, sessionB.ID)
		if err != nil {
			return err
		}
		if visibleB {
			t.Error("expected principal A to NOT see principal B's session (same tenant) - RLS should deny this, not just application filtering")
		}
		n, err := countVisibleSessions(ctx, tx)
		if err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("expected an unfiltered SELECT to see exactly 1 row (principal A's own) scoped this way, got %d", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSessionRLS_TenantCannotReadAnotherTenantsSessions proves the
// cross-tenant half of the same fix: a principal scoped to tenant A must
// never see tenant B's session rows, even for a made-up principal id that
// happens not to collide with anything in tenant A.
func TestSessionRLS_TenantCannotReadAnotherTenantsSessions(t *testing.T) {
	pool := sessionRLSTestPool(t)
	tenantA := createSessionRLSTestTenant(t, pool)
	tenantB := createSessionRLSTestTenant(t, pool)
	principalA := uuid.New()
	principalB := uuid.New()
	sessionA := issueTestSession(t, pool, tenantA, principalA)
	sessionB := issueTestSession(t, pool, tenantB, principalB)

	err := pool.WithPrincipalScope(context.Background(), tenantA, principalA, func(ctx context.Context, tx pgx.Tx) error {
		visibleA, err := sessionVisible(ctx, tx, sessionA.ID)
		if err != nil {
			return err
		}
		if !visibleA {
			t.Error("expected tenant A's principal to see their own session")
		}
		visibleB, err := sessionVisible(ctx, tx, sessionB.ID)
		if err != nil {
			return err
		}
		if visibleB {
			t.Error("expected tenant A's connection to NOT see tenant B's session - even querying by the exact id")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSessionRLS_TokenHashLookupOnlyReturnsExactMatch proves the
// phase-1 (pre-authentication) lookup path is scoped to at most the one
// row matching the exact presented token's hash - not every row in the
// table, which is what "FOR SELECT USING (true)" allowed before.
func TestSessionRLS_TokenHashLookupOnlyReturnsExactMatch(t *testing.T) {
	pool := sessionRLSTestPool(t)
	tenant := createSessionRLSTestTenant(t, pool)
	principal1 := uuid.New()
	principal2 := uuid.New()
	session1 := issueTestSession(t, pool, tenant, principal1)
	session2 := issueTestSession(t, pool, tenant, principal2)

	hash1 := hashRefreshToken(session1.RefreshToken)

	err := pool.WithSessionLookup(context.Background(), hash1, func(ctx context.Context, tx pgx.Tx) error {
		visible1, err := sessionVisible(ctx, tx, session1.ID)
		if err != nil {
			return err
		}
		if !visible1 {
			t.Error("expected the session matching the looked-up token hash to be visible")
		}
		visible2, err := sessionVisible(ctx, tx, session2.ID)
		if err != nil {
			return err
		}
		if visible2 {
			t.Error("expected an unrelated session to NOT be visible during a token-hash lookup scoped to a different token")
		}
		n, err := countVisibleSessions(ctx, tx)
		if err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("expected exactly 1 row visible during a token-hash-scoped lookup, got %d", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSessionRLS_UnscopedConnectionSeesNoSessionRows is the standing
// regression proof that RLS - not any particular query shape - is what
// enforces this: a transaction with neither app.tenant_id,
// app.principal_id, nor app.session_lookup_hash set must see zero rows
// of a table it knows for a fact has rows in it.
func TestSessionRLS_UnscopedConnectionSeesNoSessionRows(t *testing.T) {
	pool := sessionRLSTestPool(t)
	tenant := createSessionRLSTestTenant(t, pool)
	issueTestSession(t, pool, tenant, uuid.New())

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		n, err := countVisibleSessions(ctx, tx)
		if err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("expected an unscoped connection to see 0 session rows despite rows existing, got %d - RLS is not being enforced at the database level", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSessionRLS_PlatformScopedPrincipalCanAccessOwnSessions proves
// legitimate platform-level operations (e.g. a platform_admin's own
// session self-service) still work under the new policy - the dual-scope
// "tenant_id IS NULL" branch, combined with the principal match.
func TestSessionRLS_PlatformScopedPrincipalCanAccessOwnSessions(t *testing.T) {
	pool := sessionRLSTestPool(t)
	principal := uuid.New()
	session := issueTestSessionAs(t, pool, PrincipalStaff, uuid.Nil, principal)

	err := pool.WithPrincipalScope(context.Background(), uuid.Nil, principal, func(ctx context.Context, tx pgx.Tx) error {
		sessions, err := ListActiveSessions(ctx, tx, PrincipalStaff, principal)
		if err != nil {
			return err
		}
		if len(sessions) != 1 || sessions[0].ID != session.ID {
			t.Errorf("expected the platform-scoped principal's own session to be listed, got %+v", sessions)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// And a DIFFERENT platform-scoped principal must not see it.
	other := uuid.New()
	err = pool.WithPrincipalScope(context.Background(), uuid.Nil, other, func(ctx context.Context, tx pgx.Tx) error {
		visible, err := sessionVisible(ctx, tx, session.ID)
		if err != nil {
			return err
		}
		if visible {
			t.Error("expected a different platform-scoped principal to NOT see this session")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
