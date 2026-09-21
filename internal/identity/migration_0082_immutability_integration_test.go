//go:build integration

// Stage 9 (Production Readiness), migration 0082 section 1.5. Proves
// login_attempts is append-only at the DATABASE level.
//
// Two distinct things break if this table is mutable, and the second is
// the one that is easy to miss:
//
//  1. It is the brute-force / credential-stuffing forensic record, so an
//     editable row is an erasable attack trail.
//  2. It is also LIVE SECURITY STATE. IsLockedOut counts failures since
//     the last success for an identifier, so flipping succeeded=false to
//     true on a single row - or deleting a handful of rows - resets the
//     lockout window for that identifier immediately. That is a working
//     lockout bypass, not merely a records-tampering issue.
//
// Migration 0082 gives it the strongest guard (UPDATE + DELETE + TRUNCATE
// all denied) because internal/identity is the only writer and issues
// exactly one INSERT and two SELECTs - there is no update, delete or
// retention job anywhere in the tree. A future retention policy needs its
// own migration to relax this, which is the correct place for that
// decision to be visible.
package identity

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func TestMigration0082_LoginAttemptsAppendOnly(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)
	identifier := "mig0082-" + uuid.NewString()

	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return RecordLoginAttempt(ctx, tx, &tenant.ID, "player", identifier, "127.0.0.1", false)
	}); err != nil {
		t.Fatalf("seed login attempt: %v", err)
	}

	for _, stmt := range []string{
		// The lockout bypass: one flipped boolean clears the whole
		// failure window for this identifier.
		`UPDATE login_attempts SET succeeded = true WHERE identifier = $1`,
		// Trail tampering.
		`UPDATE login_attempts SET ip_address = '10.0.0.1'::inet WHERE identifier = $1`,
		`UPDATE login_attempts SET identifier = 'someone-else' WHERE identifier = $1`,
		`UPDATE login_attempts SET attempted_at = now() - interval '1 year' WHERE identifier = $1`,
		// Deleting the failures is equivalent to flipping them.
		`DELETE FROM login_attempts WHERE identifier = $1`,
	} {
		err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, stmt, identifier)
			return err
		})
		if err == nil {
			t.Fatalf("expected login_attempts_immutable to reject %q, got nil error", stmt)
		}
		if !strings.Contains(err.Error(), "append-only") {
			t.Fatalf("expected ledger_deny_mutation's own append-only message for %q, got: %v", stmt, err)
		}
	}

	// The failed attempt is still recorded, still failed.
	var count int
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM login_attempts WHERE identifier = $1 AND succeeded = false`, identifier,
		).Scan(&count)
	}); err != nil {
		t.Fatalf("re-read login attempts: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected the seeded failed attempt to survive intact, got %d matching rows", count)
	}
}

// TestMigration0082_LoginAttemptsDenyTruncate covers the statement-level
// half. It needs its own trigger because row-level triggers never fire on
// TRUNCATE - so without it, the UPDATE/DELETE guard above would stop a
// targeted edit while leaving "erase the entire attack log in one
// statement" wide open.
func TestMigration0082_LoginAttemptsDenyTruncate(t *testing.T) {
	pool := testPool(t)
	tenant := createTestTenant(t, pool)

	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE login_attempts`)
		return err
	})
	if err == nil {
		t.Fatal("expected login_attempts_no_truncate to reject TRUNCATE, got nil error")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("expected ledger_deny_mutation's own append-only message, got: %v", err)
	}
}
