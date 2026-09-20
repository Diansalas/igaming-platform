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

// Stage 4I Phase E-SECURITY (migration 0077): `tenants` gained RLS with no
// tenant-scoped/scopeless write policy, so both the INSERT and the DELETE
// cleanup below now require a genuinely platform-admin-scoped transaction.
// (The `DELETE FROM sessions` cleanup step is unrelated to migration
// 0077 - `sessions` has carried FORCE RLS with no DELETE policy at all
// since migration 0012, so it was already, and remains, a silent no-op
// regardless of connection scope; unchanged here.)
func createSessionRLSTestTenant(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, name, slug, licensing_model) VALUES ($1, $2, $3, 'under_platform_licence')`,
			id, "Session RLS Test Tenant "+id.String(), "session-rls-"+id.String(),
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("expected to insert 1 tenant row, inserted %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to create test tenant: %v", err)
	}
	t.Cleanup(func() {
		_ = pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
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

// TestSessionRLS_PrincipalCannotRevokeAnotherPrincipalsSessionDirectly
// proves the write side (not just read) of cross-principal isolation
// directly at the database level: a raw UPDATE against another
// principal's session, issued under a WithPrincipalScope transaction
// legitimately scoped to a DIFFERENT principal in the same tenant, must
// affect zero rows. This closes the gap the HTTP-level IDOR test alone
// leaves open - that test also passes if RevokeSession's own
// "AND principal_id = $2" SQL clause is doing all the work while RLS
// permits the write outright, which is exactly the "isolation via
// application code, not the database" pattern this hardening pass exists
// to eliminate.
func TestSessionRLS_PrincipalCannotRevokeAnotherPrincipalsSessionDirectly(t *testing.T) {
	pool := sessionRLSTestPool(t)
	tenant := createSessionRLSTestTenant(t, pool)
	principalA := uuid.New()
	principalB := uuid.New()
	sessionB := issueTestSession(t, pool, tenant, principalB)

	err := pool.WithPrincipalScope(context.Background(), tenant, principalA, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = now() WHERE id = $1`, sessionB.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Errorf("expected principal A's raw UPDATE against principal B's session to affect 0 rows, got %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Confirm B's session is genuinely untouched (not just that the
	// UPDATE reported 0 rows for some unrelated reason).
	err = pool.WithPrincipalScope(context.Background(), tenant, principalB, func(ctx context.Context, tx pgx.Tx) error {
		var revokedAt *time.Time
		if err := tx.QueryRow(ctx, `SELECT revoked_at FROM sessions WHERE id = $1`, sessionB.ID).Scan(&revokedAt); err != nil {
			return err
		}
		if revokedAt != nil {
			t.Error("expected principal B's session to remain unrevoked after A's denied attempt")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSessionRLS_InternalOpScopeSeesOnlyThatOneRow proves
// db.SetSessionInternalOpID grants visibility to exactly the one row it
// names - not a blanket read, and not visibility into a sibling row in
// the same tenant.
func TestSessionRLS_InternalOpScopeSeesOnlyThatOneRow(t *testing.T) {
	pool := sessionRLSTestPool(t)
	tenant := createSessionRLSTestTenant(t, pool)
	sessionA := issueTestSession(t, pool, tenant, uuid.New())
	sessionB := issueTestSession(t, pool, tenant, uuid.New())

	err := pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := db.SetSessionInternalOpID(ctx, tx, sessionA.ID); err != nil {
			return err
		}
		visibleA, err := sessionVisible(ctx, tx, sessionA.ID)
		if err != nil {
			return err
		}
		if !visibleA {
			t.Error("expected the internal-op-scoped row to be visible")
		}
		visibleB, err := sessionVisible(ctx, tx, sessionB.ID)
		if err != nil {
			return err
		}
		if visibleB {
			t.Error("expected a sibling row in the same tenant to NOT be visible when internal-op-id names a different row")
		}
		n, err := countVisibleSessions(ctx, tx)
		if err != nil {
			return err
		}
		if n != 1 {
			t.Errorf("expected exactly 1 row visible (the internal-op-scoped one), got %d", n)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSessionRLS_ScopeGUCsDoNotLeakAcrossTransactions proves that the
// per-transaction GUCs (app.session_lookup_hash, app.principal_id,
// app.session_internal_op_id, app.tenant_id) set via set_config(...,
// true) - the "is_local" form - do not survive past the transaction that
// set them, even when a later, unrelated transaction happens to reuse
// the same underlying pooled physical connection. Run several iterations
// against a small pool to make connection reuse likely, not merely
// possible.
func TestSessionRLS_ScopeGUCsDoNotLeakAcrossTransactions(t *testing.T) {
	pool := sessionRLSTestPool(t)
	tenant := createSessionRLSTestTenant(t, pool)
	session := issueTestSession(t, pool, tenant, uuid.New())
	hash := hashRefreshToken(session.RefreshToken)

	for i := 0; i < 10; i++ {
		// First: a hash-scoped transaction that legitimately sees the row.
		err := pool.WithSessionLookup(context.Background(), hash, func(ctx context.Context, tx pgx.Tx) error {
			visible, err := sessionVisible(ctx, tx, session.ID)
			if err != nil {
				return err
			}
			if !visible {
				t.Fatal("expected the session to be visible under its own token-hash scope")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("unexpected error on hash-scoped transaction: %v", err)
		}

		// Immediately after (maximizing the chance of landing on the same
		// pooled connection): a completely unscoped transaction must NOT
		// see the row, proving app.session_lookup_hash reset at commit
		// rather than leaking into this new transaction.
		err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			visible, err := sessionVisible(ctx, tx, session.ID)
			if err != nil {
				return err
			}
			if visible {
				t.Fatalf("iteration %d: session was visible in an unscoped transaction immediately after a hash-scoped one - app.session_lookup_hash leaked across transactions", i)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("unexpected error on unscoped transaction: %v", err)
		}
	}
}

// createChainedTestSession inserts a raw sessions row (bypassing
// IssueSession, since this test needs full control over
// replaced_by_session_id linkage to build a specific chain shape) and
// returns its id.
func createChainedTestSession(t *testing.T, pool *db.Pool, tenantID, principalID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO sessions (id, principal_type, principal_id, tenant_id, refresh_token_hash, expires_at)
			 VALUES ($1, 'player', $2, $3, $4, now() + interval '1 hour')`,
			id, principalID, tenantID, hashRefreshToken(uuid.NewString()),
		)
		return err
	})
	if err != nil {
		t.Fatalf("failed to create chained test session: %v", err)
	}
	t.Cleanup(func() {
		_ = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id)
			return err
		})
	})
	return id
}

// TestRevokeChainFrom_ContinuesPastAlreadyRevokedMidChainNode is the
// regression test for the pre-Stage-3 security review finding: the
// original revokeChainFrom stopped its walk the moment it hit a hop that
// was ALREADY revoked (e.g. because the session's own owner had
// logged out of that one device before the reuse was detected),
// silently leaving every session further down the chain live despite a
// confirmed token-theft signal. A→B→C→D, with B pre-revoked before the
// walk starts: all four must end up revoked, not just A and B.
func TestRevokeChainFrom_ContinuesPastAlreadyRevokedMidChainNode(t *testing.T) {
	pool := sessionRLSTestPool(t)
	tenant := createSessionRLSTestTenant(t, pool)
	principal := uuid.New()

	a := createChainedTestSession(t, pool, tenant, principal)
	b := createChainedTestSession(t, pool, tenant, principal)
	c := createChainedTestSession(t, pool, tenant, principal)
	d := createChainedTestSession(t, pool, tenant, principal)

	// All four rows share one principal, so WithPrincipalScope's SELECT
	// visibility (session_select_own_principal) covers every row in this
	// setup without needing to set the internal-op-id GUC per row - a
	// plain UPDATE still needs SOME SELECT policy to match for these
	// setup writes to actually take effect, same as the production code
	// under test.
	err := pool.WithPrincipalScope(context.Background(), tenant, principal, func(ctx context.Context, tx pgx.Tx) error {
		for _, link := range [][2]uuid.UUID{{a, b}, {b, c}, {c, d}} {
			if _, err := tx.Exec(ctx, `UPDATE sessions SET replaced_by_session_id = $1 WHERE id = $2`, link[1], link[0]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("failed to link chain: %v", err)
	}

	// Simulate the session's own owner logging out of device B before
	// the reuse of A is ever detected.
	err = pool.WithPrincipalScope(context.Background(), tenant, principal, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sessions SET revoked_at = now() WHERE id = $1`, b)
		return err
	})
	if err != nil {
		t.Fatalf("failed to pre-revoke B: %v", err)
	}

	// Now walk the chain from A, as RotateSession's reuse-detection
	// branch would.
	err = pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return revokeChainFrom(ctx, tx, a)
	})
	if err != nil {
		t.Fatalf("revokeChainFrom returned an error: %v", err)
	}

	err = pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		for name, id := range map[string]uuid.UUID{"A": a, "B": b, "C": c, "D": d} {
			if err := db.SetSessionInternalOpID(ctx, tx, id); err != nil {
				return err
			}
			var revokedAt *time.Time
			if err := tx.QueryRow(ctx, `SELECT revoked_at FROM sessions WHERE id = $1`, id).Scan(&revokedAt); err != nil {
				return err
			}
			if revokedAt == nil {
				t.Errorf("expected session %s to be revoked after the chain walk, but it wasn't", name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error verifying revocation: %v", err)
	}
}
